import assert from "node:assert/strict";
import test from "node:test";
import { HttpApiClient } from "../../api/client.ts";
import { SecurityClient, type OnboardingState, type SecurityState } from "../../security/client.ts";
import type { OnboardingAction } from "./onboarding-actions.ts";
import type { RuntimeAction } from "./runtime.ts";
import { WorkspaceSecurity } from "./coordinator.ts";

const response = (value: unknown, status = 200) => new Response(JSON.stringify(value), { status, headers: { "X-Deck-Workspace-Security": "v1" } });
const fresh: OnboardingState = { policy: "optional", protection: "undecided", started: false, setupComplete: false };
const operationID = "a".repeat(43), nextOperationID = "b".repeat(43);
function plainState(complete = false): SecurityState {
  return { instanceId: "fixture", phase: complete ? "ready" : "awaiting-start", ownerBound: true,
    onboarding: { policy: "optional", protection: "none", started: true, setupComplete: complete },
    plainOperationId: operationID, storage: "unencrypted", runtime: complete ? "running" : "stopped", locked: false,
    browserGranted: false, workspaceAuthorized: complete };
}

function fixture(request: typeof fetch, hash = "") {
  const originals = new Map<string, PropertyDescriptor | undefined>();
  const location = { hostname: "fixture", host: "fixture", hash, pathname: "/", search: "", origin: "https://fixture", assign: (_path: string) => {} };
  for (const [name, value] of Object.entries({
    window: new EventTarget(), document: new EventTarget(), location,
    history: { replaceState: (_state: unknown, _unused: string, url: string) => { location.hash = new URL(url, location.origin).hash; } },
    sessionStorage: { getItem: () => null, setItem: () => {}, removeItem: () => {} },
  })) {
    originals.set(name, Object.getOwnPropertyDescriptor(globalThis, name));
    Object.defineProperty(globalThis, name, { configurable: true, value });
  }
  class Root extends EventTarget {
    innerHTML = ""; inert = false;
    replaceChildren() { this.innerHTML = ""; }
  }
  const root = new Root();
  let admitted = 0, detached = 0, managed = 0;
  const api = new SecurityClient(request, () => "noisedeck_csrf=fixture");
  const session = new HttpApiClient();
  const controller = new WorkspaceSecurity(root as unknown as HTMLElement, session, {
    admit: async () => { admitted++; return "admitted" as const; }, detach: () => { detached++; },
    manageMethods: async () => { managed++; return () => {}; },
  }, api);
  const actions = controller as unknown as {
    actOnboarding(action: OnboardingAction): Promise<void>;
    actRuntime(action: RuntimeAction): Promise<void>;
  };
  return { root, controller, api, session, actions, location,
    admitted: () => admitted, detached: () => detached, managed: () => managed,
    close() {
      controller.dispose();
      for (const [name, original] of originals) {
        if (original) Object.defineProperty(globalThis, name, original);
        else Reflect.deleteProperty(globalThis, name);
      }
    },
  };
}

for (const [setup, hash, expected] of [
  [fresh, "", "needlink"], [fresh, `#setup=${operationID}`, "welcome"],
  [{ ...fresh, started: true }, "", "interrupted"],
  [{ ...fresh, started: true }, `#invitation=${operationID}`, "interrupted"],
  [{ ...fresh, started: true, protection: "none", setupComplete: true }, "", "signin"],
] as const) test(`public setup state routes to ${expected} without ownership mutations`, async () => {
  const calls: string[] = [];
  const f = fixture(async input => { calls.push(String(input)); return response(setup); }, hash);
  try {
    await f.controller.start(false);
    assert.match(f.root.innerHTML, new RegExp(`data-onboarding="${expected}"`));
    assert.deepEqual(calls, ["/api/security/v1/onboarding"]);
    assert.equal(f.location.hash, ""); assert.equal(f.admitted(), 0);
    assert.ok(!f.root.innerHTML.includes(operationID), "setup capability never renders");
  } finally { f.close(); }
});

test("unreachable or malformed public metadata never becomes first-run setup", async () => {
  for (const value of [{}, { ...fresh, started: undefined }, { ...fresh, policy: "unknown" }]) {
    const f = fixture(async () => response(value), `#invite=${operationID}`);
    try {
      await f.controller.start(false);
      assert.match(f.root.innerHTML, /data-onboarding="unreachable"/);
      assert.equal(f.admitted(), 0);
    } finally { f.close(); }
  }
});

test("a different signed-in owner cannot restart setup", async () => {
  const f = fixture(async input => String(input).endsWith("/onboarding")
    ? response({ ...fresh, started: true }) : response({ code: "authorization-required" }, 403));
  try {
    await f.controller.start(true);
    assert.match(f.root.innerHTML, /data-onboarding="notowner"/);
    assert.equal(f.admitted(), 0);
    const order: string[] = [];
    f.session.logout = async () => { order.push("logout"); };
    f.location.assign = path => { order.push(path); };
    await f.actions.actOnboarding({ kind: "switch-account" });
    assert.deepEqual(order, ["logout", "/auth/login?choose_account=1"]);
  } finally { f.close(); }
});

for (const rejection of ["sign-in", "redemption"] as const) test(`fresh setup ${rejection} rejection allows account switching without inventing an owner`, async () => {
  const state: SecurityState = { instanceId: "fixture", phase: "invitation-required", ownerBound: false,
    onboarding: fresh, storage: "unknown", runtime: "stopped", locked: false, browserGranted: false, workspaceAuthorized: false };
  const f = fixture(async input => {
    const path = String(input);
    if (path.endsWith("/onboarding")) return response(fresh);
    if (path.endsWith("/state")) return rejection === "sign-in" ? response({ code: "sign-in-required" }, 401) : response(state);
    if (path.endsWith("/invitation/redeem")) return response({ code: "invitation-unavailable" }, 403);
    throw new Error("unexpected request");
  });
  try {
    await f.controller.start(true);
    assert.match(f.root.innerHTML, /data-onboarding="needlink"/);
    assert.match(f.root.innerHTML, /data-onboarding-action="switch-account"/);
    assert.doesNotMatch(f.root.innerHTML, /Setup was started|belongs to another|Link expired|already been used/);
    assert.equal(f.admitted(), 0);
    const order: string[] = [];
    f.session.logout = async () => { order.push("logout"); };
    f.location.assign = path => { order.push(path); };
    await f.actions.actOnboarding({ kind: "switch-account" });
    assert.deepEqual(order, ["/auth/login?choose_account=1"], "the private setup handoff survives an ownerless account switch");
  } finally { f.close(); }
});

test("an already-signed-in setup link survives expiry before the private state read", async () => {
  const calls: string[] = [];
  const f = fixture(async (input, init) => {
    const path = String(input); calls.push(path);
    if (path.endsWith("/onboarding")) return response(fresh);
    if (path.endsWith("/invitation/handoff")) {
      assert.deepEqual(JSON.parse(String(init?.body)), { token: operationID });
      return response({});
    }
    if (path.endsWith("/state")) return response({ code: "sign-in-required" }, 401);
    throw new Error("unexpected setup request");
  }, `#setup=${operationID}`);
  try {
    await f.controller.start(true);
    assert.deepEqual(calls, ["/api/security/v1/onboarding", "/api/security/v1/invitation/handoff", "/api/security/v1/state"],
      "the private handoff must precede the request that can expire Google authorization");
    assert.match(f.root.innerHTML, /data-onboarding="needlink"/);
    assert.equal(f.location.hash, ""); assert.ok(!f.root.innerHTML.includes(operationID));
    assert.equal(f.admitted(), 0);
    const navigation: string[] = [];
    f.session.logout = async () => { throw new Error("logout would discard the unconsumed handoff"); };
    f.location.assign = path => { navigation.push(path); };
    await f.actions.actOnboarding({ kind: "switch-account" });
    assert.deepEqual(navigation, ["/auth/login?choose_account=1"]);
    assert.equal(calls.filter(path => path.endsWith("/invitation/handoff")).length, 1);
  } finally { f.close(); }
});

test("a returning owner without the setup receipt cannot turn sign-in into protection authority", async () => {
  const state: SecurityState = { instanceId: "fixture", phase: "awaiting-protection", ownerBound: true,
    onboarding: { ...fresh, started: true }, storage: "unknown", runtime: "stopped", locked: false,
    browserGranted: false, workspaceAuthorized: false };
  let choices = 0;
  const f = fixture(async input => {
    const path = String(input);
    if (path.endsWith("/onboarding")) return response(state.onboarding);
    if (path.endsWith("/state")) return response(state);
    if (path.endsWith("/unlock-options")) return response([]);
    if (path.endsWith("/onboarding/protection")) { choices++; return response({ code: "authorization-required" }, 403); }
    throw new Error("unexpected setup mutation");
  });
  try {
    await f.controller.start(true);
    await f.actions.actOnboarding({ kind: "resume-setup" });
    await f.actions.actOnboarding({ kind: "continue-unencrypted" });
    assert.equal(choices, 1); assert.equal(f.admitted(), 0);
    assert.match(f.root.innerHTML, /data-onboarding="resume"/);
    assert.match(f.root.innerHTML, /Continue in the browser where setup started, or contact whoever runs this machine/);
    assert.ok(!f.root.innerHTML.includes('data-onboarding="done"'));
  } finally { f.close(); }
});

test("a persisted encrypted selection cannot return to optional protection choice", async () => {
  const state: SecurityState = { instanceId: "fixture", phase: "awaiting-method", ownerBound: true,
    onboarding: { ...fresh, started: true, protection: "encrypted" }, storage: "closed", runtime: "stopped", locked: true,
    browserGranted: false, workspaceAuthorized: false };
  const f = fixture(async input => {
    const path = String(input);
    if (path.endsWith("/onboarding")) return response(state.onboarding);
    if (path.endsWith("/state")) return response(state);
    if (path.endsWith("/unlock-options")) return response([]);
    throw new Error("unexpected setup mutation");
  });
  try {
    await f.controller.start(true);
    await f.actions.actOnboarding({ kind: "resume-setup" });
    assert.match(f.root.innerHTML, /data-enrollment="choose-method"/);
    assert.ok(!f.root.innerHTML.includes("Continue without Deck encryption"));
    await assert.rejects(f.actions.actOnboarding({ kind: "continue-unencrypted" }), /protection choice/);
    assert.equal(f.admitted(), 0);
  } finally { f.close(); }
});

test("lost selection and start responses reconcile without another mutation or fabricated readiness", async () => {
  let state: SecurityState = { instanceId: "fixture", phase: "awaiting-protection", ownerBound: true, onboarding: { ...fresh, started: true },
    storage: "unknown", runtime: "stopped", locked: false, browserGranted: false, workspaceAuthorized: false };
  let selected = 0, starts = 0, methods = 0, observation = "pending";
  const f = fixture(async (input, init) => {
    const path = String(input);
    if (path.endsWith("/onboarding/protection")) { selected++; state = plainState(); throw new Error("lost response"); }
    if (path.endsWith("/onboarding/start")) {
      starts++; assert.deepEqual(JSON.parse(String(init?.body)), { operationId: operationID });
      state.runtime = "unknown"; return response({ id: operationID, status: "unconfirmed" }, 202);
    }
    if (path.includes("/onboarding/operations/")) return response({ id: operationID, status: observation, storage: state.storage, runtime: state.runtime, locked: false });
    if (path.endsWith("/onboarding")) return response(state.onboarding);
    if (path.endsWith("/state")) return response(state);
    methods++; return response([]);
  });
  try {
    await f.controller.start(true);
    await f.actions.actOnboarding({ kind: "resume-setup" });
    assert.match(f.root.innerHTML, /data-onboarding="encryption"/);
    await f.actions.actOnboarding({ kind: "continue-unencrypted" });
    assert.match(f.root.innerHTML, /data-onboarding="unconfirmed"/);
    await f.actions.actOnboarding({ kind: "check-state" });
    assert.match(f.root.innerHTML, /data-onboarding="resume"/);
    assert.equal(selected, 1); assert.equal(starts, 0);
    const previousMethods = methods;
    await f.actions.actOnboarding({ kind: "resume-setup" });
    assert.match(f.root.innerHTML, /data-onboarding="unconfirmed"/);
    await f.actions.actOnboarding({ kind: "check-state" });
    assert.equal(starts, 1); assert.equal(selected, 1);
    assert.equal(methods, previousMethods, "plain path must not query or prepare encryption methods");
    state.runtime = "running";
    await f.controller.refresh();
    assert.equal(f.admitted(), 0, "running alone cannot complete setup");
    state = { ...plainState(true), workspaceAuthorized: false }; observation = "succeeded";
    await f.controller.refresh();
    assert.equal(f.admitted(), 0, "completion alone cannot admit this browser");
    state.workspaceAuthorized = true;
    await f.controller.refresh();
    assert.match(f.root.innerHTML, /data-onboarding="done"/);
    assert.equal(f.admitted(), 0, "initial completion presents the confirmed summary before arrival");
    await f.actions.actOnboarding({ kind: "open-workspace" });
    assert.equal(f.admitted(), 1); assert.equal(f.controller.encrypted, false);
    f.controller.lock(); await f.controller.manageMethods();
    assert.equal(f.managed(), 0);
  } finally { f.close(); }
});

test("completed plain workspace uses a new start request after reboot without reopening onboarding", async () => {
  let state = { ...plainState(true), runtime: "stopped" as SecurityState["runtime"], workspaceAuthorized: false };
  let starts = 0, observation = "succeeded";
  const f = fixture(async (input, init) => {
    const path = String(input);
    if (path.endsWith("/onboarding")) return response(state.onboarding);
    if (path.endsWith("/state")) return response(state);
    if (path.endsWith("/onboarding/start")) {
      starts++; assert.deepEqual(JSON.parse(String(init?.body)), {});
      state.plainOperationId = nextOperationID; state.runtime = "unknown"; observation = "uncertain";
      throw new Error("response lost after reservation");
    }
    if (path.includes("/onboarding/operations/")) return response({ id: state.plainOperationId, status: observation, storage: "unencrypted", runtime: state.runtime, locked: false });
    throw new Error("unexpected encryption or mutation endpoint");
  });
  try {
    await f.controller.start(true);
    assert.match(f.root.innerHTML, /data-security-runtime="plain-stopped"/);
    assert.equal(starts, 0);
    await f.actions.actRuntime({ kind: "start-workspace" });
    assert.match(f.root.innerHTML, /data-security-runtime="plain-unknown"/);
    await f.actions.actRuntime({ kind: "retry" });
    assert.equal(starts, 1); assert.equal(f.admitted(), 0);
    assert.match(f.root.innerHTML, /data-security-runtime="plain-unknown"/);
    state.runtime = "running"; state.workspaceAuthorized = true; observation = "succeeded";
    await f.actions.actRuntime({ kind: "retry" });
    assert.equal(f.admitted(), 1); assert.ok(!f.root.innerHTML.includes("data-onboarding=\"done\""));
  } finally { f.close(); }
});

test("failed setup start offers a fresh start request instead of rechecking the same receipt", async () => {
  const state = plainState();
  const bodies: unknown[] = [];
  let observation = "uncertain";
  const f = fixture(async (input, init) => {
    const path = String(input);
    if (path.endsWith("/onboarding")) return response(state.onboarding);
    if (path.endsWith("/state")) return response(state);
    if (path.endsWith("/onboarding/start")) {
      bodies.push(JSON.parse(String(init?.body)));
      if (bodies.length === 1) { state.runtime = "unknown"; return response({ id: operationID, status: "unconfirmed" }, 202); }
      Object.assign(state, plainState(true), { plainOperationId: nextOperationID }); observation = "succeeded";
      return response({ id: nextOperationID, status: "accepted" });
    }
    if (path.includes("/onboarding/operations/")) return response({ id: state.plainOperationId, status: observation, storage: state.storage, runtime: state.runtime, locked: false });
    throw new Error("unexpected encryption or mutation endpoint");
  });
  try {
    await f.controller.start(true);
    await f.actions.actOnboarding({ kind: "resume-setup" });
    assert.match(f.root.innerHTML, /data-onboarding="unconfirmed"/);
    state.runtime = "stopped"; observation = "failed";
    await f.actions.actOnboarding({ kind: "check-state" });
    assert.match(f.root.innerHTML, /data-onboarding="resume"/);
    assert.match(f.root.innerHTML, /did not start/);
    await f.actions.actOnboarding({ kind: "resume-setup" });
    assert.deepEqual(bodies, [{ operationId: operationID }, {}]);
    assert.match(f.root.innerHTML, /data-onboarding="done"/);
  } finally { f.close(); }
});

for (const interruption of ["dispose", "expiry"] as const) test(`plain start ${interruption} aborts waiting and rejects a late success`, async () => {
  let finish!: (response: Response) => void, entered!: () => void;
  const waiting = new Promise<Response>(resolve => { finish = resolve; });
  const started = new Promise<void>(resolve => { entered = resolve; });
  let submittedSignal: AbortSignal | null | undefined, expired = false, observed = 0;
  const state = plainState();
  const f = fixture(async (input, init) => {
    const path = String(input);
    if (path.endsWith("/onboarding")) return response(state.onboarding);
    if (path.endsWith("/state")) return response(expired ? { code: "authorization-required" } : state, expired ? 401 : 200);
    if (path.endsWith("/onboarding/start")) { submittedSignal = init?.signal; entered(); return waiting; }
    if (path.includes("/onboarding/operations/")) { observed++; return response({ id: operationID, status: "pending", storage: "unknown", runtime: "stopped", locked: false }); }
    throw new Error("unexpected request");
  });
  try {
    await f.controller.start(true);
    const observations = observed;
    const start = f.actions.actOnboarding({ kind: "resume-setup" });
    await started;
    assert.match(f.root.innerHTML, /data-onboarding="finishing"/);
    assert.ok(!f.root.innerHTML.includes("saved unlock method"));
    if (interruption === "dispose") f.controller.dispose();
    else { expired = true; await assert.rejects(f.api.state()); }
    const stoppedView = f.root.innerHTML;
    assert.equal(submittedSignal?.aborted, true);
    finish(response({ id: operationID, status: "accepted" }));
    await start;
    assert.equal(observed, observations, "late start reply cannot begin new observation work");
    assert.equal(f.root.innerHTML, stoppedView); assert.equal(f.admitted(), 0);
    if (interruption === "expiry") assert.match(stoppedView, /data-onboarding="interrupted"/);
  } finally { f.close(); }
});
