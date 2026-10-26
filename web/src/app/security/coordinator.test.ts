import assert from "node:assert/strict";
import test from "node:test";
import { HttpApiClient } from "../../api/client.ts";
import { SecurityClient, type SecurityMethod, type SecurityState } from "../../security/client.ts";
import { createMethodEnvelope, openMethodEnvelope, workspaceCommitment, type OpenedMethod } from "../../security/envelope.ts";
import { encodeBase64url } from "../../security/encoding.ts";
import { formatRecoveryKey } from "../../security/recovery.ts";
import { WorkspaceSecurity } from "./coordinator.ts";

const response = (value: unknown, status = 200) => new Response(JSON.stringify(value), {
  status, headers: { "X-Deck-Workspace-Security": "v1" },
});
function deferred<T>() {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>(done => { resolve = done; });
  return { promise, resolve };
}
function fixture(request: typeof fetch) {
  const originals = new Map<string, PropertyDescriptor | undefined>();
  for (const [name, value] of Object.entries({
    window: new EventTarget(), document: new EventTarget(),
    location: { hostname: "fixture", host: "fixture", hash: "", pathname: "/", search: "" },
  })) {
    originals.set(name, Object.getOwnPropertyDescriptor(globalThis, name));
    Object.defineProperty(globalThis, name, { configurable: true, value });
  }
  class Root extends EventTarget {
    innerHTML = "";
    inert = false;
    replaceChildren() { this.innerHTML = ""; }
  }
  const root = new Root();
  let detached = 0, admitted = 0;
  const api = new SecurityClient(request, () => "noisedeck_csrf=fixture");
  const controller = new WorkspaceSecurity(root as unknown as HTMLElement, new HttpApiClient(), {
    detach: () => { detached++; }, admit: async () => { admitted++; },
  }, api);
  // The assertions inspect lifetimes that cannot be observed from rendered HTML.
  const owned = controller as unknown as {
    source?: OpenedMethod; key?: string; ceremony: AbortController; state?: SecurityState;
    everyday?: SecurityMethod; recovery?: SecurityMethod; checkRecovery(value: string): Promise<void>;
  };
  return { root, api, controller, owned, detached: () => detached, admitted: () => admitted,
    close() {
      controller.dispose();
      for (const [name, original] of originals) {
        if (original) Object.defineProperty(globalThis, name, original);
        else Reflect.deleteProperty(globalThis, name);
      }
    },
  };
}

const secret = new Uint8Array(32).fill(18), recoverySecret = new Uint8Array(32).fill(29);
const commitment = await workspaceCommitment(secret);
const envelope = await createMethodEnvelope({ instanceId: "fixture", methodId: "recovery", revision: 1, kind: "recovery" },
  secret, { kind: "recovery", recoverySecret }, new Uint8Array(32).fill(7));
const recovery: SecurityMethod = { id: "recovery", revision: 1, kind: "recovery", label: "Recovery key", status: "pending", salt: envelope.salt, envelope };
const everyday: SecurityMethod = { ...recovery, id: "password", kind: "password" };
const state: SecurityState = { instanceId: "fixture", phase: "awaiting-recovery", commitment, storage: "closed", runtime: "stopped", locked: false, browserGranted: false,
  ownerBound: true, workspaceAuthorized: false, onboarding: { policy: "required", protection: "encrypted", started: true, setupComplete: false } };
const key = await formatRecoveryKey(recoverySecret);

test("setup state 401 disposes the opened source and removes recovery data immediately", async () => {
  const f = fixture(async () => response({ code: "authorization-required" }, 401));
  try {
    const opened = await openMethodEnvelope(envelope, { kind: "recovery", recoverySecret }, envelope, commitment);
    f.owned.source = opened; f.owned.key = key;
    const signal = f.owned.ceremony.signal;
    f.root.innerHTML = key;
    await f.controller.refresh();
    assert.equal(signal.aborted, true);
    assert.throws(() => opened.copyUnlockSecret(), /disposed/);
    assert.equal(f.owned.source, undefined); assert.equal(f.owned.key, undefined);
    assert.match(f.root.innerHTML, /data-security-runtime="signed-out"/);
    assert.ok(!f.root.innerHTML.includes(key));
    assert.equal(f.detached(), 1); assert.equal(f.admitted(), 0);
  } finally { f.close(); }
});

test("a successful state response arriving after 401 cannot restore setup", async () => {
  const delayed = deferred<Response>();
  const f = fixture(async input => String(input).endsWith("/onboarding") ? response(state.onboarding) : String(input).endsWith("/state") ? delayed.promise : response({ code: "authorization-required" }, 401));
  try {
    const refresh = f.controller.refresh();
    await assert.rejects(f.api.methods());
    const signedOut = f.root.innerHTML;
    delayed.resolve(response({ ...state, phase: "awaiting-method" }));
    await refresh;
    assert.equal(f.root.innerHTML, signedOut);
    assert.equal(f.owned.state, undefined); assert.equal(f.admitted(), 0);
  } finally { f.close(); }
});

for (const status of [200, 409]) test(`recovery proof response ${status} after expiry cannot repaint the key or advance setup`, async () => {
  const submitted = deferred<void>(), delayed = deferred<Response>();
  const challenge = encodeBase64url(new Uint8Array(32).fill(5));
  const f = fixture(async input => {
    const path = String(input);
    if (path.endsWith("/challenges")) return response({ id: challenge, operationId: challenge, challenge });
    if (path.endsWith("/envelope")) return response(envelope);
    if (path.endsWith("/operations")) { submitted.resolve(); return delayed.promise; }
    return response({ code: "authorization-required" }, 401);
  });
  try {
    Object.assign(f.owned, { state, everyday, recovery, key });
    const signal = f.owned.ceremony.signal;
    const checking = f.owned.checkRecovery(key);
    await submitted.promise;
    await f.controller.refresh();
    assert.equal(signal.aborted, true);
    const signedOut = f.root.innerHTML;
    delayed.resolve(response(status === 200 ? { id: challenge, status: "accepted" } : { code: "state-changed" }, status));
    await checking;
    assert.equal(f.root.innerHTML, signedOut);
    assert.ok(!f.root.innerHTML.includes(key));
    assert.equal(f.owned.key, undefined); assert.equal(f.owned.recovery, undefined);
    assert.equal(f.owned.everyday, undefined); assert.equal(f.admitted(), 0);
  } finally { f.close(); }
});

for (const status of ["refused", "unconfirmed"]) test(`lock outcome ${status} only permits retry after a definite clean refusal`, async () => {
 const live = { ...state, phase: "ready", storage: "open", runtime: "running", onboarding: { ...state.onboarding, setupComplete: true } };
 const f = fixture(async input => String(input).endsWith("/state") ? response(live) : response({ id: "lock-attempt", status, storage: "open", runtime: "running", locked: false }));
 try {
  const owned = f.controller as unknown as { pending?: { instanceId: string; id: string; operation: string }; observe(): Promise<void> };
  owned.pending = { instanceId: "fixture", id: "lock-attempt", operation: "lock" };
  await owned.observe();
  if (status === "refused") {
   assert.equal(owned.pending, undefined);
   assert.match(f.root.innerHTML, /data-security-runtime="authorize-browser"/);
   assert.match(f.root.innerHTML, /Locking did not start/);
  } else {
   assert.equal(owned.pending?.id, "lock-attempt");
   assert.match(f.root.innerHTML, /data-security-runtime="lock-failed"/);
  }
  assert.equal(f.admitted(), 0);
 } finally { f.close(); }
});
