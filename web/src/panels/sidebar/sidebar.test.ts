import { MARKED_PROVIDERS, providerDisplayName, providerTileClass, sessionColorStyle } from "../../ui/session-identity.ts";
import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import type { ApiClient } from "../../api/contracts.ts";
import type { GitIdentityProfile, Project } from "../../api/projects.ts";
import type { HarnessUsageReport, SessionSummary } from "../../api/sessions.ts";
import { FIXTURE_PROVIDERS } from "../../app/fixture-providers.ts";
import type { ProjectWithSessions } from "./sidebar.ts";
import { ApiError } from "../../api/http.ts";
import type { ConsoleSettings, SidebarLayout, TutorialPreference } from "../../api/settings.ts";
import {
  SidebarModel,
  dismissOnSelect,
  startsCollapsed,
  escapeHtml,
  modKeyLabel,
  sessionMetaHtml,
  sessionRowMetaHtml,
} from "./sidebar.ts";
import { projectChevronHtml, sessionRailStyle } from "./markup.ts";

// The picker holds no provider list of its own to pin: which harnesses
// exist is served, and the order it draws them in is the order they were
// served in. What the bundle still owns is the palette, so that is what the
// mark tests below pin.



const projects: Project[] = [
  { id: "p1", name: "alpha", canonical_path: "/srv/alpha", last_used_at: "t1" },
  { id: "p2", name: "beta", canonical_path: "/srv/beta", last_used_at: "t2" },
];
const identity: GitIdentityProfile = {
  id: "identity-1", name: "Test Author", email: "test@example.invalid", revision: 1,
  created_at: "t", updated_at: "t",
};
const runningFacts = { recovery_capability: "none", recovery_state: "unavailable", presentation_state: "running" } as const;
const endedFacts = { recovery_capability: "none", recovery_state: "unavailable", presentation_state: "ended" } as const;

const sessions: Record<string, SessionSummary[]> = {
  p1: [
    { id: "s1", project_id: "p1", provider: "claude", display_name: "", status: "running", generation: 1, created_at: "t", updated_at: "t", ...runningFacts },
    { id: "s2", project_id: "p1", provider: "codex", display_name: "", status: "exited", generation: 2, created_at: "t", updated_at: "t", ...endedFacts },
  ],
  p2: [],
};

function fakeApi(overrides: Partial<ApiClient> = {}): ApiClient {
  return {
    logout: () => Promise.resolve(),
    getHarnessUsage: () => Promise.resolve({ global: [], project: [] }),
    getSessionChats: () => Promise.reject(new Error("unused")),
    listChats: () => Promise.reject(new Error("unused")),
    recoverSession: () => Promise.reject(new Error("unused")),
    sessionScreen: () => Promise.resolve(null),
    setSessionColor: () => Promise.resolve(),
    getSettings: () => Promise.resolve({ skip_permissions: false, skip_close_confirm: false, skip_remove_project_confirm: false }),
    saveSettings: (next: ConsoleSettings) => Promise.resolve(next),
    getLayout: () => Promise.resolve({ expanded: [], projects: [], sessions: {} }),
    saveLayout: (next: SidebarLayout) => Promise.resolve(next),
    getLastSession: () => Promise.resolve(null),
    saveLastSession: () => Promise.resolve(),
    getTutorial: () => Promise.resolve(null),
    saveTutorial: (next: TutorialPreference) => Promise.resolve({ ...next, scope: "user" }),
    deleteSession: () => Promise.resolve(),
    deleteProject: () => Promise.resolve(),
    renameProject: () => Promise.resolve(),
    renameSession: () => Promise.resolve(),
    listProjects: () => Promise.resolve(projects),
    listProviders: () => Promise.resolve(["claude", "codex", "shell"]),
    listGitIdentities: () => Promise.resolve({ profiles: [identity], default_profile_id: identity.id, revision: 1 }),
    createGitIdentity: (name, email) => Promise.resolve({ ...identity, name, email }),
    updateGitIdentity: (_id, _revision, name, email) => Promise.resolve({ ...identity, name, email, revision: 2 }),
    setDefaultGitIdentity: () => Promise.resolve({ profiles: [identity], default_profile_id: identity.id, revision: 1 }),
    deleteGitIdentity: () => Promise.resolve(),
    setProjectGitIdentity: () => Promise.resolve(projects[0]!),
    listProjectDirectories: () => Promise.resolve({ current_path: "", parent_path: null, directories: [] }),
    createProjectDirectory: () => Promise.resolve({ path: "/workspace/new" }),
    inspectProject: () => Promise.reject(new ApiError(400, "bad_request", "n/a")),
    startClone: () => Promise.reject(new ApiError(400, "bad_request", "n/a")),
    listClones: () => Promise.resolve([]),
    getClone: () => Promise.reject(new ApiError(404, "not_found", "n/a")),
    cancelClone: () => Promise.reject(new ApiError(404, "not_found", "n/a")),
    resumeClone: () => Promise.reject(new ApiError(404, "not_found", "n/a")),
    discardClone: () => Promise.resolve(),
    createProject: (name: string, path: string, profile: GitIdentityProfile) => Promise.resolve({
      id: "p-new", name, canonical_path: path,
      git_author_name: profile.name, git_author_email: profile.email,
      last_used_at: "t",
    }),
    listSessions: (id: string) => Promise.resolve(sessions[id] ?? []),
    createSession: (projectID: string, provider: string) =>
      Promise.resolve({
        id: "new-1",
        project_id: projectID,
        provider,
        display_name: "",
        status: "running",
        generation: 1,
        created_at: "t",
        updated_at: "t",
        ...runningFacts,
      }),
    // The sidebar never calls these; the interface requires them.
    initializeTaskmaster: () => Promise.resolve({ path: "/x/.git/noise-tasks/tasks.sqlite", tag: "main", already_initialized: false }),
    getTaskmasterInfo: () =>
      Promise.reject(new ApiError(404, "not_taskmaster_project", "n/a")),
    listTaskmasterTasks: () => Promise.resolve({}),
    addTaskmasterTask: () => Promise.resolve({ id: "1" }),
    getTaskmasterTask: () => Promise.reject(new ApiError(404, "not_found", "n/a")),
    listTaskmasterPRDs: () => Promise.resolve([]),
    commitStarter: () => Promise.reject(new ApiError(400, "n/a", "n/a")),
    generateTasks: () => Promise.reject(new ApiError(400, "invalid_prd", "n/a")),
    uploadPrd: () => Promise.resolve(),
    listTaskmasterRuns: () => Promise.resolve([]),
    getTaskmasterRun: () => Promise.reject(new ApiError(404, "not_found", "n/a")),
    getSession: () => Promise.reject(new ApiError(404, "not_found", "n/a")),
    ...overrides,
  };
}

test("refresh projects only with verified running sessions", async () => {
  const m = new SidebarModel(fakeApi());
  await m.refresh();
  assert.equal(m.groups.length, 2);
  assert.deepEqual(
    m.groups[0]!.sessions.map((s) => s.id),
    ["s1"],
  );
  assert.equal(m.groups[1]!.sessions.length, 0);
  assert.equal(m.loading, false);
});

// Which harnesses exist is the server's answer. The model asks for it on
// every refresh and keeps whatever came back, in the order it came back.
test("refresh keeps the served harness inventory verbatim", async () => {
  const m = new SidebarModel(fakeApi({
    listProviders: () => Promise.resolve(["claude", "codex", "shell"]),
  }));
  await m.refresh();
  assert.deepEqual(m.availableProviders, ["claude", "codex", "shell"]);
});

// A console that has not been told which harnesses it has offers none. The
// alternative — falling back to a list the bundle carries — is the failure
// that produced this endpoint: fourteen chips on a two-harness image, each
// one failing when clicked. And the failure must stay local: the sidebar's
// projects are a different question with a different answer.
test("a failed inventory empties the picker and leaves the projects alone", async () => {
  const m = new SidebarModel(fakeApi({
    listProviders: () => Promise.reject(new Error("network")),
  }));
  await m.refresh();
  assert.deepEqual(m.availableProviders, [], "nothing is offered on no answer");
  assert.equal(m.error, null, "an inventory failure is not a projects failure");
  assert.equal(m.groups.length, 2, "the projects still loaded");
});

// Before the first answer arrives there is nothing to offer either — the
// bundle cannot tell which image it is running in.
test("the picker offers nothing until the server has answered", () => {
  const m = new SidebarModel(fakeApi());
  assert.deepEqual(m.availableProviders, []);
  assert.equal(m.harnessInventory.status, "unknown");
});

// The inventory has three distinct states. A served empty answer is a true
// statement that no harnesses are available; it must not be conflated with a
// fetch failure.
test("a served empty inventory is reported as empty, not as an error", async () => {
  const m = new SidebarModel(fakeApi({
    listProviders: () => Promise.resolve([]),
  }));
  await m.refresh();
  assert.equal(m.harnessInventory.status, "served");
  assert.deepEqual(m.availableProviders, []);
});

// A failed inventory fetch with no previous good answer leaves the picker in
// the unknown state: there is nothing truthful to show.
test("an initial inventory failure leaves the picker unknown", async () => {
  const m = new SidebarModel(fakeApi({
    listProviders: () => Promise.reject(new Error("network down")),
  }));
  await m.refresh();
  assert.equal(m.harnessInventory.status, "unknown");
  assert.equal(m.availableProviders.length, 0);
  assert.equal(m.error, null, "an inventory failure is not a projects failure");
  assert.equal(m.groups.length, 2, "the projects still loaded");
});

// A failed inventory fetch after a successful one keeps the last good list
// as stale, so the user can still launch from it while a retry is offered.
test("an inventory failure after success keeps the last list as stale", async () => {
  let fail = false;
  const m = new SidebarModel(fakeApi({
    listProviders: () => fail
      ? Promise.reject(new Error("network down"))
      : Promise.resolve(["claude", "codex", "shell"]),
  }));
  await m.refresh();
  assert.equal(m.harnessInventory.status, "served");
  fail = true;
  await m.refresh();
  assert.equal(m.harnessInventory.status, "stale");
  assert.deepEqual(m.availableProviders, ["claude", "codex", "shell"]);
  assert.equal((m.harnessInventory as { status: "stale"; error: string }).error, "network down");
});

// A server that answered "none" and then became unreachable is stale, not
// unknown: the honest empty answer is the last good state.
test("a failure after a served empty inventory is stale, not unknown", async () => {
  let fail = false;
  const m = new SidebarModel(fakeApi({
    listProviders: () => fail
      ? Promise.reject(new Error("network down"))
      : Promise.resolve([]),
  }));
  await m.refresh();
  assert.equal(m.harnessInventory.status, "served");
  fail = true;
  await m.refresh();
  assert.equal(m.harnessInventory.status, "stale");
  assert.deepEqual(m.availableProviders, []);
});

// A retry that succeeds recovers from both unknown and stale, replacing the
// inventory with the fresh answer.
test("refreshProviders recovers from an inventory failure", async () => {
  let fail = true;
  const m = new SidebarModel(fakeApi({
    listProviders: () => fail
      ? Promise.reject(new Error("network down"))
      : Promise.resolve(["claude", "codex", "shell"]),
  }));
  await m.refresh();
  assert.equal(m.harnessInventory.status, "unknown");
  fail = false;
  await m.refreshProviders();
  assert.equal(m.harnessInventory.status, "served");
  assert.deepEqual(m.availableProviders, ["claude", "codex", "shell"]);
});

// Repeated inventory failures without an intervening success stay in the
// state they started from; unknown does not become stale.
test("repeated inventory failures stay unknown until a success arrives", async () => {
  const m = new SidebarModel(fakeApi({
    listProviders: () => Promise.reject(new Error("still down")),
  }));
  await m.refresh();
  assert.equal(m.harnessInventory.status, "unknown");
  await m.refreshProviders();
  assert.equal(m.harnessInventory.status, "unknown");
});

test("ended sessions stay outside the active rows", async () => {
  const ended = (id: string, provider: string, minutesAgo: number): SessionSummary => ({
    id, project_id: "p9", provider, display_name: "", status: "exited", generation: 1,
    created_at: "t", updated_at: "t",
    last_output_at: new Date(Date.now() - minutesAgo * 60000).toISOString(),
    recovery_capability: "none", recovery_state: "unavailable", presentation_state: "ended",
  });
  const m = new SidebarModel(fakeApi({
    listProjects: () => Promise.resolve([
      { id: "p9", name: "finished", canonical_path: "/srv/finished", last_used_at: "t" },
    ]),
    listSessions: () => Promise.resolve([ended("e1", "codex", 30), ended("e2", "kimi", 5)]),
  }));
  await m.refresh();
  assert.deepEqual(m.groups[0]!.sessions, [], "no ended session belongs on the active surface");
});

// Auto-color projects: the tint must survive model refresh. The
// row helpers already carry a set color; the regression was refresh rebuilding
// groups without re-applying the auto tints.
test("auto-color survives a model refresh", async () => {
  const m = new SidebarModel(fakeApi(), {}, true);
  await m.refresh();
  assert.ok(m.groups.length >= 2, "fixture provides multiple projects");
  for (const group of m.groups) {
    assert.ok(
      group.autoColor?.startsWith("var(--color-mark-"),
      `${group.project.name} keeps a mark token after refresh`,
    );
    const color = group.autoColor!;
    const chevron = projectChevronHtml(false, color);
    const rail = sessionRailStyle(color);
    assert.ok(chevron.includes(color), "the chevron renders the token");
    assert.ok(rail.includes(color), "the rail renders the token");
  }
});

// The session tiles read the derived tint directly, so a tint left standing
// under a colour the user picks later paints the tiles one hue while the
// chevron and rail carry another.
test("picking a project colour drops the auto tint under it", async () => {
  const m = new SidebarModel(fakeApi(), {}, true);
  await m.refresh();
  const target = m.groups[0]!;
  const tint = (): string | null | undefined =>
    m.groups.find((g) => g.project.id === target.project.id)?.autoColor;
  assert.ok(tint()?.startsWith("var(--color-mark-"), "the project starts tinted");

  await m.setProjectColor(target.project.id, "blue");
  assert.equal(tint(), undefined, "the tint gives way to the user's tag");

  await m.setProjectColor(target.project.id, null);
  assert.ok(tint()?.startsWith("var(--color-mark-"), "clearing the tag lets the tint return");
});

// Each setSessionColor inside the loop refreshes, which rebuilds the groups
// uncoloured and hands the project a fresh auto tint. Restoring the project's
// colour on top of that has to re-derive, or the tint sits under the colour and
// paints whichever sessions the loop has not reached yet.
test("applying a project colour to its sessions leaves no auto tint behind", async () => {
  const m = new SidebarModel(fakeApi(), {}, true);
  await m.refresh();
  const target = m.groups.find((g) => g.sessions.length > 0) ?? m.groups[0]!;
  await m.setProjectColor(target.project.id, "blue");
  assert.equal(await m.applyProjectColorToSessions(target.project.id), true);

  const after = m.groups.find((g) => g.project.id === target.project.id)!;
  assert.equal(after.color, "blue", "the project keeps the user's tag");
  assert.equal(after.autoColor, undefined, "no tint survives under the tag");
});

// An automatic tint is a render-time preference whose hue follows row
// position, so it must never reach a session record. On a project the
// user never coloured the row is the clearing one, and it clears
// whether or not the tint is currently painted.
for (const autoColor of [true, false]) {
  test(`an uncoloured project clears its sessions rather than writing a tint, auto-color ${autoColor ? "on" : "off"}`, async () => {
    const stored = sessions.p1!.map((session) => ({ ...session, color: "violet" as string | null }));
    const writes: { id: string; color: string | null }[] = [];
    const m = new SidebarModel(fakeApi({
      listSessions: (id) => Promise.resolve(id === "p1" ? stored : []),
      setSessionColor: (id, color) => {
        writes.push({ id, color });
        const session = stored.find((candidate) => candidate.id === id);
        if (session !== undefined) session.color = color;
        return Promise.resolve();
      },
    }), {}, autoColor);
    await m.refresh();
    const target = m.groups.find((group) => group.project.id === "p1")!;
    assert.equal(target.color ?? null, null, "the fixture project carries no user colour");
    assert.equal(target.autoColor?.startsWith("var(--color-mark-") ?? false, autoColor,
      "the tint is painted exactly when the preference is on");

    assert.equal(await m.applyProjectColorToSessions(target.project.id), true);
    assert.equal(writes.length, 1, "only the verified running session is updated");
    assert.deepEqual(writes[0], { id: "s1", color: null },
      "an uncoloured project propagates no colour");
    assert.equal(m.groups.find((group) => group.project.id === "p1")!.sessions[0]!.color, null);
  });
}

// The user's own colour is what propagates, and it reaches the session
// records as the durable key the renderer paints from.
test("a coloured project writes its own key to every session", async () => {
  const stored = sessions.p1!.map((session) => ({ ...session }));
  const writes: { id: string; color: string | null }[] = [];
  const m = new SidebarModel(fakeApi({
    listSessions: (id) => Promise.resolve(id === "p1" ? stored : []),
    setSessionColor: (id, color) => {
      writes.push({ id, color });
      const session = stored.find((candidate) => candidate.id === id);
      if (session !== undefined) session.color = color;
      return Promise.resolve();
    },
  }), {}, true);
  await m.refresh();
  const target = m.groups.find((group) => group.project.id === "p1")!;
  await m.setProjectColor(target.project.id, "violet");

  assert.equal(await m.applyProjectColorToSessions(target.project.id), true);
  assert.equal(writes.at(-1)!.color, "violet", "the user's key, not the tint");
  assert.equal(sessionColorStyle(writes.at(-1)!.color), 'style="background:var(--color-mark-violet)"',
    "the persisted key paints the project's mark through the session renderer");
});

// Auto-color projects: turning the preference on after the initial
// refresh must survive the next refresh. The regression was the model storing
// only the constructor value, so a later toggle coloured the current groups
// but the next refresh re-applied the stale OFF value and cleared them.
test("auto-color turned on after refresh survives the next refresh", async () => {
  const m = new SidebarModel(fakeApi(), {}, false);
  await m.refresh();
  m.setAutoColorProjects(true);
  for (const group of m.groups) {
    assert.ok(
      group.autoColor?.startsWith("var(--color-mark-"),
      `${group.project.name} gets a mark token immediately when turned on`,
    );
  }
  await m.refresh();
  for (const group of m.groups) {
    assert.ok(
      group.autoColor?.startsWith("var(--color-mark-"),
      `${group.project.name} keeps a mark token after refresh`,
    );
  }
});


// applyProjectColors is the render choke point: the shell hands it the
// saved colour map and the displayed row order in one call, and the tints
// must come back over that order. The fixture's names sort p1 before p2;
// the presented order here is reversed, so p2 wears the ramp's first hue.
// A forwarding that dropped the order would leave every project on its
// alphabetical hue while the rows render dragged, and the rail would stop
// reading in sequence.
test("applyProjectColors re-derives the tints over the presented row order", async () => {
  const m = new SidebarModel(fakeApi(), {}, true);
  await m.refresh();
  m.applyProjectColors({}, ["p2", "p1"]);
  const tintOf = (id: string): string | null | undefined =>
    m.groups.find((g) => g.project.id === id)?.autoColor;
  assert.equal(tintOf("p2"), "var(--color-mark-crimson)", "the row displayed first takes the ramp's first hue");
  assert.equal(tintOf("p1"), "var(--color-mark-cerulean)", "the row displayed second takes the second hue");
});

test("a failing session refresh keeps the project but drops unverified rows", async () => {
  const m = new SidebarModel(fakeApi({
    listSessions: (id: string) =>
      id === "p1" ? Promise.reject(new ApiError(500, "internal", "db gone")) : Promise.resolve([]),
  }));
  const errors: string[] = [];
  await m.refresh();
  assert.equal(m.groups[0]!.loadError, "db gone");
  assert.deepEqual(m.groups[0]!.sessions, []);
  assert.equal(m.groups[1]!.loadError, undefined);
  assert.deepEqual(errors, []);
});

// A server that cannot be reached has not said the sessions are gone. The
// last served list stays, marked stale, and the selection stays with it:
// a redeploy's ten seconds of refused connections must not tear the
// terminal down and forget which session the user was in.
test("a failed projects fetch surfaces through onError and keeps the last served rows as stale", async () => {
  const good = new SidebarModel(fakeApi());
  await good.refresh();
  const before = good.groups;

  const errors: string[] = [];
  const deselected: string[] = [];
  const bad = new SidebarModel(
    fakeApi({ listProjects: () => Promise.reject(new ApiError(500, "internal", "db gone")) }),
    {
      onDeselectSession: (id) => deselected.push(id),
      onError: (msg) => errors.push(msg),
    },
  );
  bad.groups = before;
  bad.selectedSessionID = "s1";
  await bad.refresh();
  assert.deepEqual(errors, ["db gone"]);
  assert.equal(bad.listState, "error");
  assert.equal(bad.stale, true, "the kept rows are marked unconfirmed");
  assert.equal(bad.registryReadOk, false);
  assert.deepEqual(bad.groups, before, "nothing the server last said is dropped");
  assert.equal(bad.selectedSessionID, "s1", "the selection survives an unreachable server");
  assert.deepEqual(deselected, []);
  assert.equal(bad.select("s1"), false, "a stale row refuses selection until confirmed");
});

test("a failed first read with nothing to keep is error, not stale", async () => {
  const m = new SidebarModel(
    fakeApi({ listProjects: () => Promise.reject(new ApiError(500, "internal", "db gone")) }),
  );
  assert.equal(m.listState, "loading", "nothing has answered yet");
  await m.refresh();
  assert.equal(m.listState, "error");
  assert.equal(m.stale, false);
  assert.deepEqual(m.groups, []);
});

test("a successful read confirms the list and writes the cache", async () => {
  const store = new Map<string, string>();
  const storage = {
    getItem: (key: string) => store.get(key) ?? null,
    setItem: (key: string, value: string) => { store.set(key, value); },
    removeItem: (key: string) => { store.delete(key); },
  };
  const m = new SidebarModel(fakeApi(), {}, false, storage);
  assert.equal(m.stale, false, "no cache, nothing to draw");
  await m.refresh();
  assert.equal(m.listState, "ready");
  assert.equal(m.stale, false);
  assert.equal(store.size, 1, "the served list is remembered for the next cold load");

  // The next cold load draws the remembered list, stale, until the server
  // answers; a served answer then replaces it wholesale, dropping what the
  // server no longer has.
  const next = new SidebarModel(fakeApi({ listProjects: () => Promise.resolve([projects[0]!]) }), {}, false, storage);
  assert.equal(next.listState, "loading");
  assert.equal(next.stale, true);
  assert.deepEqual(next.groups.map((group) => group.project.id), ["p1", "p2"]);
  assert.equal(next.select("s1"), false, "a cached row is inert");
  await next.refresh();
  assert.equal(next.stale, false);
  assert.deepEqual(next.groups.map((group) => group.project.id), ["p1"]);
  assert.equal(next.select("s1"), true);
});

// A cache the model cannot render from is dropped at read time, not
// carried into construction: the first render would throw on a malformed
// row, the loading pane would never mount, and no server answer could
// replace an entry nothing reads past.
test("a cache that does not parse or does not hold the rail's shape is dropped", () => {
  const validProject = { id: "p1", name: "alpha", canonical_path: "/srv/alpha", last_used_at: "t1" };
  const validSession = sessions.p1![0]!;
  const malformed: string[] = [
    "{not json",
    JSON.stringify({ groups: "nope" }),
    JSON.stringify({ groups: [null] }),
    JSON.stringify({ groups: [{ project: { id: "p1" }, sessions: [] }] }),
    JSON.stringify({ groups: [{ project: validProject, sessions: [null] }] }),
    JSON.stringify({ groups: [{ project: validProject, sessions: [{ id: "s1" }] }] }),
    JSON.stringify({ groups: [{ project: validProject, sessions: [{ ...validSession, presentation_state: 7 }] }] }),
    JSON.stringify({ groups: [{ project: validProject, sessions: [{ ...validSession, presentation_state: "nope" }] }] }),
    // Optional fields are typed too: a tile renderer that reads color as a
    // string must never meet an object there.
    JSON.stringify({ groups: [{ project: validProject, sessions: [{ ...validSession, color: { toString: null } }] }] }),
    JSON.stringify({ groups: [{ project: validProject, sessions: [{ ...validSession, last_output_at: 12 }] }] }),
    // The rail keys plain objects by id, so an id that names an inherited
    // property would read a function where a saved order was expected.
    ...["__proto__", "constructor", "toString", "hasOwnProperty", ""].flatMap((id) => [
      JSON.stringify({ groups: [{ project: { ...validProject, id }, sessions: [] }] }),
      JSON.stringify({ groups: [{ project: validProject, sessions: [{ ...validSession, id }] }] }),
      JSON.stringify({ groups: [{ project: validProject, sessions: [{ ...validSession, project_id: id }] }] }),
    ]),
  ];
  for (const raw of malformed) {
    const store = new Map<string, string>([["noisedeck.session-list", raw]]);
    const storage = {
      getItem: (key: string) => store.get(key) ?? null,
      setItem: (key: string, value: string) => { store.set(key, value); },
      removeItem: (key: string) => { store.delete(key); },
    };
    const m = new SidebarModel(fakeApi(), {}, false, storage);
    assert.deepEqual(m.groups, [], `rejected: ${raw}`);
    assert.equal(m.stale, false);
    assert.equal(store.size, 0, `a rejected cache is cleared: ${raw}`);
  }
  const store = new Map<string, string>([["noisedeck.session-list", JSON.stringify({ groups: [{ project: validProject, sessions: [validSession] }] })]]);
  const storage = {
    getItem: (key: string) => store.get(key) ?? null,
    setItem: () => {},
    removeItem: (key: string) => { store.delete(key); },
  };
  const m = new SidebarModel(fakeApi(), {}, false, storage);
  assert.equal(m.groups.length, 1, "a well-formed cache is drawn");
  assert.equal(m.stale, true);

  // A cached record is rebuilt from the fields the rail renders, so a key
  // the server never served does not ride along, and a null colour is
  // kept as the explicit "no override" the tile renderer understands.
  const extra = new Map<string, string>([["noisedeck.session-list", JSON.stringify({
    groups: [{ project: { ...validProject, surprise: 1 }, sessions: [{ ...validSession, color: null, surprise: 1 }] }],
  })]]);
  const kept = new SidebarModel(fakeApi(), {}, false, {
    getItem: (key: string) => extra.get(key) ?? null,
    setItem: () => {},
    removeItem: () => {},
  });
  assert.deepEqual(kept.groups[0]!.project, validProject);
  assert.deepEqual(kept.groups[0]!.sessions[0], { ...validSession, color: null });
});

test("select marks and reports only a verified running session", async () => {
  const selected: string[] = [];
  const m = new SidebarModel(fakeApi(), { onSelectSession: (id) => selected.push(id) });
  await m.refresh();
  assert.equal(m.select("s2"), false);
  assert.equal(m.select("missing"), false);
  assert.equal(m.select("s1"), true);
  assert.equal(m.selectedSessionID, "s1");
  assert.deepEqual(selected, ["s1"]);
});

test("select reports whether the keyboard made the selection", async () => {
  const reported: boolean[] = [];
  const m = new SidebarModel(fakeApi(), { onSelectSession: (_, keyboard) => reported.push(keyboard) });
  await m.refresh();
  m.select("s1");
  m.select("s1", true);
  assert.deepEqual(reported, [false, true]);
});

test("a session in limbo stays listed and selected; an ended one leaves", async () => {
  let presentation: SessionSummary["presentation_state"] = "running";
  const deselected: string[] = [];
  const m = new SidebarModel(fakeApi({
    listSessions: (id) => Promise.resolve(id === "p1" ? [{
      ...sessions.p1![0]!,
      presentation_state: presentation,
    }] : []),
  }), { onDeselectSession: (id) => deselected.push(id) });
  await m.refresh();
  assert.equal(m.select("s1"), true);

  // A redeploy's limbo (unverified but extant) must not make the session
  // unreachable: the row stays, selection stays, and the recovery surface
  // is what the selection shows.
  presentation = "attachment_unavailable";
  await m.refresh();
  assert.equal(m.selectedSessionID, "s1");
  assert.equal(m.groups[0]!.sessions.length, 1);
  assert.deepEqual(deselected, []);

  presentation = "ended";
  await m.refresh();
  assert.equal(m.selectedSessionID, null);
  assert.deepEqual(m.groups[0]!.sessions, []);
  assert.deepEqual(deselected, ["s1"]);
});

test("create reports and refreshes from the server", async () => {
  let created = false;
  const m = new SidebarModel(fakeApi(), { onCreateSession: () => { created = true; } });
  await m.create("p1", "claude");
  assert.equal(created, true);
  assert.equal(m.groups.length, 2); // refreshed
});

test("create finishes session reconciliation without waiting for provider inventory", async () => {
  let releaseProviders!: (providers: readonly string[]) => void;
  const providers = new Promise<readonly string[]>((resolve) => { releaseProviders = resolve; });
  const m = new SidebarModel(fakeApi({ listProviders: () => providers }));
  let settled = false;
  const creation = m.create("p1", "claude").then((result) => {
    settled = true;
    return result;
  });
  await new Promise((resolve) => setImmediate(resolve));
  assert.equal(settled, true, "the created row is ready while the provider request is still pending");
  releaseProviders(["claude"]);
  assert.equal(await creation, "created");
});

test("create does not select a session before the backend verifies it running", async () => {
  const selected: string[] = [];
  const m = new SidebarModel(fakeApi({
    createSession: (projectID, provider) => Promise.resolve({
      id: "starting-1",
      project_id: projectID,
      provider,
      display_name: "",
      status: "starting",
      generation: 1,
      created_at: "t",
      updated_at: "t",
      recovery_capability: "none",
      recovery_state: "automatic_pending",
      presentation_state: "recovering",
    }),
  }), { onCreateSession: (id) => selected.push(id) });

  assert.equal(await m.create("p1", "codex"), "created");
  assert.deepEqual(selected, []);
});

test("create classifies missing project identity without showing a dead-end error", async () => {
  const errors: string[] = [];
  const m = new SidebarModel(fakeApi({
    createSession: () => Promise.reject(
      new ApiError(409, "git_identity_required", "set up a Git identity"),
    ),
  }), { onError: (message) => errors.push(message) });

  assert.equal(await m.create("p1", "cline"), "identity_required");
  assert.equal(m.error, null);
  assert.deepEqual(errors, []);
});

test("concurrent create calls launch exactly one native session", async () => {
  let createCalls = 0;
  let complete!: (session: SessionSummary) => void;
  const pending = new Promise<SessionSummary>((resolve) => { complete = resolve; });
  const selected: string[] = [];
  const m = new SidebarModel(fakeApi({
    createSession: () => {
      createCalls++;
      return pending;
    },
  }), { onCreateSession: (id) => selected.push(id) });

  const first = m.create("p1", "claude");
  const duplicate = m.create("p1", "claude");
  assert.equal(m.creatingSession, true);
  assert.equal(createCalls, 1);
  complete({
    id: "only-session",
    project_id: "p1",
    provider: "claude",
    display_name: "",
    status: "running",
    generation: 1,
    created_at: "t",
    updated_at: "t",
    ...runningFacts,
  });
  await Promise.all([first, duplicate]);

  assert.equal(createCalls, 1);
  assert.deepEqual(selected, ["only-session"]);
  assert.equal(m.creatingSession, false);
});

test("project registration reports, refreshes, and preserves backend paths", async () => {
  const created: string[] = [];
  const m = new SidebarModel(fakeApi(), {
    onCreateProject: (id) => created.push(id),
  });
  const project = await m.createProject("gamma", "/projects/gamma", identity);
  assert.equal(project?.canonical_path, "/projects/gamma");
  assert.equal(project?.git_author_email, "test@example.invalid");
  assert.deepEqual(created, ["p-new"]);
  assert.equal(m.groups.length, 2);
});

test("project registration failure remains visible and resolves to null", async () => {
  const m = new SidebarModel(fakeApi({
    createProject: () => Promise.reject(
      new ApiError(403, "outside_allowed_roots", "path is outside allowed roots"),
    ),
  }));
  assert.equal(await m.createProject("bad", "/etc", identity), null);
  assert.equal(m.error, "path is outside allowed roots");
});

test("project registration reports the repository-local values it replaced", async () => {
  const m = new SidebarModel(fakeApi({
    createProject: () => Promise.resolve({
      id: "gamma", name: "gamma", canonical_path: "/projects/gamma", last_used_at: "t",
      replaced_git_identity: { name: "Existing Author", email: "existing@example.invalid" },
    }),
  }));

  assert.notEqual(await m.createProject("gamma", "/projects/gamma", identity), null);
  assert.equal(m.error, null);
  assert.match(m.identityNotice ?? "", /user\.name was Existing Author/);
  assert.match(m.identityNotice ?? "", /user\.email was existing@example\.invalid/);
});

test("a project write that replaced nothing leaves no notice", async () => {
  const m = new SidebarModel(fakeApi({
    createProject: () => Promise.resolve({
      id: "delta", name: "delta", canonical_path: "/projects/delta", last_used_at: "t",
    }),
  }));

  assert.notEqual(await m.createProject("delta", "/projects/delta", identity), null);
  assert.equal(m.identityNotice, null);
});

test("Git identity revision conflicts reload current values and remain actionable", async () => {
  let catalogCalls = 0;
  const current = { ...identity, revision: 2, name: "Current Author" };
  const errors: string[] = [];
  const m = new SidebarModel(fakeApi({
    listGitIdentities: () => {
      catalogCalls += 1;
      return Promise.resolve({ profiles: [current], default_profile_id: current.id, revision: 3 });
    },
    updateGitIdentity: () => Promise.reject(
      new ApiError(409, "revision_conflict", "stale profile"),
    ),
  }), { onError: (message) => errors.push(message) });
  await m.refresh();

  assert.equal(await m.updateGitIdentity(identity, "Changed", "changed@example.invalid"), false);
  assert.equal(catalogCalls, 2);
  assert.equal(m.gitIdentities?.profiles[0]?.name, "Current Author");
  assert.equal(m.error, "Git identities changed in another window. Review the current values and try again.");
  assert.deepEqual(errors, [m.error]);
});

test("a changed default Git identity is reported after the first read, once", async () => {
  let catalog: { profiles: GitIdentityProfile[]; default_profile_id: string | null; revision: number } = { profiles: [], default_profile_id: null, revision: 1 };
  let changes = 0;
  const m = new SidebarModel(fakeApi({
    listGitIdentities: () => Promise.resolve(catalog),
    createGitIdentity: () => {
      catalog = { profiles: [identity], default_profile_id: identity.id, revision: 2 };
      return Promise.resolve(identity);
    },
  }), { onDefaultIdentityChanged: () => changes++ });
  await m.refresh();
  assert.equal(changes, 0, "the first read is not a change");
  assert.deepEqual(await m.createGitIdentity(identity.name, identity.email), identity);
  assert.equal(changes, 1);
  await m.refresh();
  assert.equal(changes, 1, "an unchanged default stays quiet");
});

test("container directory browsing preserves the backend's canonical choices", async () => {
  const listing = {
    current_path: "/workspace/projects",
    parent_path: null,
    directories: [{ name: "Audio Player", path: "/workspace/projects/Audio Player" }],
  };
  const m = new SidebarModel(fakeApi({
    listProjectDirectories: (path?: string) => {
      assert.equal(path, "/workspace/projects");
      return Promise.resolve(listing);
    },
  }));
  assert.deepEqual(await m.browseProjectDirectories("/workspace/projects"), listing);
  assert.equal(m.error, null);
});

test("container directory browsing reports backend refusal in-band", async () => {
  const errors: string[] = [];
  const m = new SidebarModel(fakeApi({
    listProjectDirectories: () => Promise.reject(
      new ApiError(403, "outside_allowed_roots", "outside mounted workspace"),
    ),
  }), { onError: (message) => errors.push(message) });
  assert.equal(await m.browseProjectDirectories("/etc"), null);
  assert.equal(m.error, "outside mounted workspace");
  assert.deepEqual(errors, ["outside mounted workspace"]);
});

test("create surfaces a 409 in-band, never as an unhandled rejection", async () => {
  const errors: string[] = [];
  const m = new SidebarModel(
    fakeApi({
      createSession: () =>
        Promise.reject(new ApiError(409, "provider_not_launchable", "identity capture pending")),
    }),
    { onError: (msg) => errors.push(msg) },
  );
  // Resolves (does not reject) with the error recorded for the UI.
  await m.create("p1", "codex");
  assert.equal(m.error, "identity capture pending");
  assert.deepEqual(errors, ["identity capture pending"]);

  // A later success clears it.
  const m2 = new SidebarModel(fakeApi());
  m2.error = "stale";
  await m2.create("p1", "claude");
  assert.equal(m2.error, null);
});

const appCss = readFileSync(new URL("../../styles/app.css", import.meta.url), "utf8");

// Both chrome bars are one --chrome-h and both centre their contents. Under
// border-box a bottom hairline shrinks the bar's content box, so the sidebar
// header can only agree with the main header's centreline if it reserves the
// same border. Asserted against the source because the offset is half a pixel
// — below what a rendering assertion or the pixel gate resolves, and the size
// of mistake that survives review by looking right.
test("the sidebar header reserves the hairline box it does not paint", () => {
  const rule = /\.chrome-flush\s*\{([^}]*)\}/.exec(appCss);
  assert.ok(rule, ".chrome-flush must exist");
  const body = rule[1]!;
  const declared = /border-bottom:\s*([^;]+);/.exec(body);
  assert.ok(declared, ".chrome-flush must declare border-bottom");
  const value = declared[1]!.trim();
  assert.match(value, /^1px\s+solid\s+transparent$/,
    `border-bottom is ${value}; a zeroed border makes the two chrome bars centre on different boxes`);
});

// providerHueDeclarations returns every --color-provider-<name> value in
// app.css; the light and dark blocks each contribute one.
function providerHueDeclarations(provider: string): string[] {
  return [...appCss.matchAll(new RegExp(`--color-provider-${provider}:\\s*([^;]+);`, "g"))]
    .map((m) => m[1]!.trim());
}

// resolveHue maps a declared value to the colour it paints: a mark token
// resolves through its --color-mark-* definition, a raw value such as
// shell's neutral is its own hue.
function resolveHue(value: string): string {
  const mark = /^var\(--color-mark-([a-z-]+)\)$/.exec(value);
  if (!mark) return value;
  return [...appCss.matchAll(new RegExp(`--color-mark-${mark[1]}:\\s*([^;]+);`, "g"))][0]![1]!.trim();
}

// providerHue is the one colour a provider paints on both grounds; a
// provider whose blocks disagree, or that is missing a block, fails here.
function providerHue(provider: string): string {
  const hues = providerHueDeclarations(provider).map(resolveHue);
  assert.deepEqual(hues, [hues[0], hues[0]], `${provider} paints one hue on both grounds`);
  return hues[0]!;
}

// oklabDistance is the perceptual distance the mark system separates with:
// sRGB → linear → LMS → OKLab, Euclidean. The design already rejected one
// pair at 0.055 (sapphire beside kimi's indigo) as too close for two
// harnesses a user switches between.
function oklabDistance(hexA: string, hexB: string): number {
  const toOklab = (hex: string): [number, number, number] => {
    const lin = (c: number) => (c <= 0.04045 ? c / 12.92 : ((c + 0.055) / 1.055) ** 2.4);
    const [r, g, b] = [0, 1, 2].map((i) => lin(parseInt(hex.slice(1 + 2 * i, 3 + 2 * i), 16) / 255)) as [number, number, number];
    const l = Math.cbrt(0.4122214708 * r + 0.5363325363 * g + 0.0514459929 * b);
    const m = Math.cbrt(0.2119034982 * r + 0.6806995451 * g + 0.1073969566 * b);
    const s = Math.cbrt(0.0883024619 * r + 0.2817188376 * g + 0.6299787005 * b);
    return [
      0.2104542553 * l + 0.7936177850 * m - 0.0040720468 * s,
      1.9779984951 * l - 2.4285922050 * m + 0.4505937099 * s,
      0.0259040371 * l + 0.7827717662 * m - 0.8086757660 * s,
    ];
  };
  const a = toOklab(hexA);
  const b = toOklab(hexB);
  return Math.hypot(a[0] - b[0], a[1] - b[1], a[2] - b[2]);
}

// Every harness owns a hue and none borrows a status token: bg-run and
// bg-alive mean running and healthy elsewhere, so a mark wearing one would
// answer "what state" instead of "which one". codex and command-code shared
// bg-alive and were indistinguishable.
test("providerTileClass maps each known provider to its own mark and falls back for the rest", () => {
  assert.equal(providerTileClass("claude"), "bg-provider-claude");
  assert.equal(providerTileClass("cline"), "bg-provider-cline");
  assert.equal(providerTileClass("command-code"), "bg-provider-command-code");
  assert.equal(providerTileClass("codex"), "bg-provider-codex");
  assert.equal(providerTileClass("devin"), "bg-provider-devin");
  assert.equal(providerTileClass("kimi"), "bg-provider-kimi");
  assert.equal(providerTileClass("omp"), "bg-provider-omp");
  assert.equal(providerTileClass("opencode"), "bg-provider-opencode");
  assert.equal(providerTileClass("pi"), "bg-provider-pi");
  assert.equal(providerTileClass("qwen"), "bg-provider-qwen");
  assert.equal(providerTileClass("shell"), "bg-provider-shell");
  assert.equal(providerTileClass("kilo"), "bg-provider-kilo");
  assert.equal(providerTileClass("mimo"), "bg-provider-mimo");
  assert.equal(providerTileClass("qoder"), "bg-provider-qoder");
  assert.equal(providerTileClass("cursor"), "bg-provider-cursor");
  assert.equal(providerTileClass("grok"), "bg-provider-grok");
  assert.equal(providerTileClass("unknown"), "bg-text-tertiary", "the neutral reads in both themes; a surface would be black in dark mode");
  assert.equal(providerTileClass(""), "bg-text-tertiary");
  assert.notEqual(providerTileClass("unknown"), "bg-surface-sunken");

  // No two harnesses may share a mark - a duplicate makes both tiles
  // useless. The class names above stay distinct whatever hue sits behind
  // them, so the invariant is asserted on the hues they resolve to.
  const marks = MARKED_PROVIDERS.map(providerTileClass);
  assert.equal(new Set(marks).size, marks.length, "every harness has a distinct mark");
  // Coverage is asserted against a full image's harness set rather than
  // against the table's own keys, which could only agree with themselves.
  // A harness with no mark still renders — on the neutral above — so this is
  // the only place that would notice a new one arriving without a hue.
  for (const provider of FIXTURE_PROVIDERS) {
    assert.notEqual(providerTileClass(provider), "bg-surface-sunken",
      `${provider} ships in a full image and has no mark`);
  }
  const hues = MARKED_PROVIDERS.map(providerHue);
  // Exact RGB equality is not the only collision: sapphire and kimi's
  // indigo are 0.055 apart in OKLab and the design keeps sapphire
  // unassigned for exactly that reason. No two provider hues may come
  // that close, and kimi's indigo keeps clear separation from every
  // other harness.
  for (let i = 0; i < MARKED_PROVIDERS.length; i++) {
    for (let j = i + 1; j < MARKED_PROVIDERS.length; j++) {
      const distance = oklabDistance(providerHue(MARKED_PROVIDERS[i]!), providerHue(MARKED_PROVIDERS[j]!));
      assert.ok(distance > 0.055,
        `${MARKED_PROVIDERS[i]} and ${MARKED_PROVIDERS[j]} are ${distance.toFixed(3)} apart; the design rejects pairs this close`);
      if (MARKED_PROVIDERS[i] === "kimi" || MARKED_PROVIDERS[j] === "kimi") {
        assert.ok(distance >= 0.07, `kimi sits only ${distance.toFixed(3)} from a neighbouring harness`);
      }
    }
  }
  assert.equal(new Set(hues).size, hues.length, "every harness paints a distinct hue");
});

// The pinned providers carry a fixed hue mapping. shell is
// deliberately outside the mark family - the terminal is not a harness
// identity, it is the user's own surface - so it takes a raw neutral,
// never a --color-mark-* token.
test("provider hues match the mark mapping on both grounds", () => {
  const comp: Record<string, string> = {
    claude: "var(--color-mark-vermilion)",
    codex: "var(--color-mark-emerald)",
    kimi: "var(--color-mark-indigo)",
    opencode: "var(--color-mark-cerulean)",
    shell: "#5A6472",
  };
  for (const [provider, value] of Object.entries(comp)) {
    assert.deepEqual(
      providerHueDeclarations(provider),
      [value, value],
      `${provider} must carry its mark hue in the light and dark blocks only`,
    );
  }
});

test("escapeHtml neutralizes markup in user/server data", () => {
  assert.equal(
    escapeHtml(`<img src=x onerror="alert('x')"> & "friends"`),
    "&lt;img src=x onerror=&quot;alert(&#39;x&#39;)&quot;&gt; &amp; &quot;friends&quot;",
  );
});

// --- Session row meta ---
// The right side of a session row is a spinner while the server presents
// the session as running AND output is actively streaming, otherwise the
// age tick. The spinner's gate is the server-presented presentation state
// only — never a browser-side status inference.

function summaryForMeta(presentationState: SessionSummary["presentation_state"]): SessionSummary {
  return {
    id: "s1",
    project_id: "p1",
    provider: "claude",
    display_name: "",
    status: "recorded-status-is-not-presentation",
    generation: 1,
    created_at: "t",
    updated_at: "t",
    recovery_capability: "resume",
    recovery_state: "unavailable",
    presentation_state: presentationState,
  };
}

test("session row meta renders no status when output age is unavailable", () => {
  for (const state of ["running", "recovering", "attachment_unavailable", "ended", "inactive"] as const) {
    assert.equal(sessionRowMetaHtml(summaryForMeta(state)), "");
  }
});

test("session row meta renders only last-output age", () => {
  const now = Date.parse("2026-08-14T07:02:00.000Z");
  const html = sessionRowMetaHtml({
    ...summaryForMeta("inactive"),
    last_output_at: "2026-08-14T07:00:00.000Z",
  }, now);
  assert.match(html, />2m<\/span>/);
  assert.ok(!/data-hint/.test(html), "the age tick carries no hint of its own");
  assert.ok(!/data-session-recover|text-error|text-warning|bg-error/.test(html), html);
});

test("the spinner appears only for a running session with an output edge", () => {
  assert.ok(sessionMetaHtml(summaryForMeta("running"), true).includes('aria-label="output streaming"'));
  assert.ok(!sessionMetaHtml(summaryForMeta("running"), false).includes("output streaming"),
    "a running row rests on its age tick until an output edge");
  assert.ok(!sessionMetaHtml(summaryForMeta("inactive"), true).includes("output streaming"),
    "a stale output edge never spins a row the server does not present as running");
  assert.ok(!sessionMetaHtml(summaryForMeta("ended"), true).includes("output streaming"));
  assert.ok(!sessionMetaHtml(summaryForMeta("recovering"), true).includes("output streaming"));
  assert.equal(sessionMetaHtml(summaryForMeta("ended"), false), "");
});

test("a session-coloured tile overrides only the background (tiles carry colour, never letters)", () => {
  assert.equal(sessionColorStyle(null), "", "no colour leaves the provider tile as-is");
  assert.equal(sessionColorStyle(""), "", "an empty colour is not a colour");
  assert.equal(sessionColorStyle("retired-key"), "", "a retired tag key falls back to the provider hue");
  const tagged = sessionColorStyle("amber");
  assert.match(tagged, /background:var\(--color-mark-gold\)/, "the tag palette wears the mark vars the chips wear");
  assert.match(sessionColorStyle("silver"), /background:#7B8799/, "the neutral identities are pinned hex, not theme vars");
  assert.match(sessionColorStyle("gray"), /background:#7B8799/, "a legacy gray tag reads as silver, the nearest neutral");
  assert.ok(!tagged.includes("color:"), "the tile carries no text, so no text colour is set");
});

test("rename posts the label and refreshes; failures surface in-band", async () => {
  const renamed: Array<[string, string]> = [];
  const m = new SidebarModel(fakeApi({
    renameSession: (id: string, name: string) => {
      renamed.push([id, name]);
      return Promise.resolve();
    },
  }));
  assert.equal(await m.rename("s1", "deploy watcher"), true);
  assert.deepEqual(renamed, [["s1", "deploy watcher"]]);
  assert.equal(m.error, null);

  const errors: string[] = [];
  const bad = new SidebarModel(
    fakeApi({ renameSession: () => Promise.reject(new ApiError(404, "not_found", "session not found")) }),
    { onError: (msg) => errors.push(msg) },
  );
  assert.equal(await bad.rename("gone", "x"), false);
  assert.deepEqual(errors, ["session not found"]);
});

test("setSessionColor posts selected and cleared values; failures stay visible", async () => {
  const calls: Array<[string, string | null]> = [];
  const m = new SidebarModel(fakeApi({
    setSessionColor: (id, color) => {
      calls.push([id, color]);
      return Promise.resolve();
    },
  }));
  assert.equal(await m.setSessionColor("s1", "violet"), true);
  assert.equal(await m.setSessionColor("s1", null), true);
  assert.deepEqual(calls, [["s1", "violet"], ["s1", null]]);
  assert.equal(m.error, null);

  const errors: string[] = [];
  const bad = new SidebarModel(fakeApi({
    setSessionColor: () => Promise.reject(new ApiError(500, "internal", "color write failed")),
  }), { onError: (message) => errors.push(message) });
  assert.equal(await bad.setSessionColor("s1", "amber"), false);
  assert.equal(bad.error, "color write failed");
  assert.deepEqual(errors, ["color write failed"]);
});

test("remove deletes, refreshes, and fires onRemoveSession callback", async () => {
  const calls: string[] = [];
  let deleted = false;
  const m = new SidebarModel(
    fakeApi({
      deleteSession: (id: string) => {
        calls.push(`api:${id}`);
        deleted = true;
        return Promise.resolve();
      },
      listSessions: (id) => Promise.resolve(
        (sessions[id] ?? []).filter((session) => !deleted || session.id !== "s1"),
      ),
    }),
    { onRemoveSession: (id) => calls.push(`cb:${id}`) },
  );
  await m.refresh();
  assert.equal(await m.remove("s1"), true);
  assert.equal(m.error, null);
  assert.deepEqual(calls, ["api:s1", "cb:s1"]);
});

test("an already-gone session clears only after the list confirms absence", async () => {
  const removed: string[] = [];
  const m = new SidebarModel(
    fakeApi({
      deleteSession: () =>
        Promise.reject(new ApiError(404, "not_found", "no such session")),
    }),
    { onRemoveSession: (id) => removed.push(id) },
  );
  assert.equal(await m.remove("gone"), true);
  assert.equal(m.error, null);
  assert.deepEqual(removed, ["gone"]);
});

test("a rejected delete is successful when the refreshed list proves the row is gone", async () => {
  let retired = false;
  const removed: string[] = [];
  const errors: string[] = [];
  const m = new SidebarModel(fakeApi({
    deleteSession: () => {
      retired = true;
      return Promise.reject(new ApiError(500, "cleanup_pending", "could not remove session"));
    },
    listSessions: (id) => Promise.resolve(
      (sessions[id] ?? []).filter((session) => !retired || session.id !== "s1"),
    ),
  }), {
    onRemoveSession: (id) => removed.push(id),
    onError: (message) => errors.push(message),
  });
  await m.refresh();

  assert.equal(await m.remove("s1"), true);
  assert.equal(m.error, null);
  assert.deepEqual(removed, ["s1"]);
  assert.deepEqual(errors, []);
});

test("a rejected delete keeps its original error when the raw row becomes non-running", async () => {
  const removed: string[] = [];
  const errors: string[] = [];
  let presentation: SessionSummary["presentation_state"] = "running";
  const m = new SidebarModel(fakeApi({
    deleteSession: () => {
      presentation = "inactive";
      return Promise.reject(new ApiError(500, "cleanup_failed", "could not remove session"));
    },
    listSessions: (id) => Promise.resolve((sessions[id] ?? []).map((session) => ({
      ...session,
      presentation_state: presentation,
    }))),
  }), {
    onRemoveSession: (id) => removed.push(id),
    onError: (message) => errors.push(message),
  });
  await m.refresh();

  assert.equal(await m.remove("s1"), false);
  assert.equal(m.error, "could not remove session");
  assert.deepEqual(m.groups[0]!.sessions, []);
  assert.deepEqual(removed, []);
  assert.deepEqual(errors, ["could not remove session"]);
});

test("a rejected delete keeps the delete error when confirmation cannot load", async () => {
  let failConfirmation = false;
  const errors: string[] = [];
  const m = new SidebarModel(fakeApi({
    deleteSession: () => {
      failConfirmation = true;
      return Promise.reject(new ApiError(500, "cleanup_failed", "could not remove session"));
    },
    listSessions: (id) => failConfirmation && id === "p1"
      ? Promise.reject(new ApiError(503, "temporarily_unavailable", "list unavailable"))
      : Promise.resolve(sessions[id] ?? []),
  }), { onError: (message) => errors.push(message) });
  await m.refresh();

  assert.equal(await m.remove("s1"), false);
  assert.equal(m.error, "could not remove session");
  assert.deepEqual(errors, ["could not remove session"]);
});

test("remove retains the authoritative row until delete and refresh both settle", async () => {
  let finishDelete!: () => void;
  const deletion = new Promise<void>((resolve) => { finishDelete = resolve; });
  let deleted = false;
  const removed: string[] = [];
  const m = new SidebarModel(fakeApi({
    deleteSession: async () => {
      await deletion;
      deleted = true;
    },
    listSessions: (id) => Promise.resolve(
      (sessions[id] ?? []).filter((session) => !deleted || session.id !== "s1"),
    ),
  }), { onRemoveSession: (id) => removed.push(id) });
  await m.refresh();

  const pending = m.remove("s1");
  await Promise.resolve();
  assert.equal(m.groups[0]!.sessions.some((session) => session.id === "s1"), true);
  assert.deepEqual(removed, []);

  finishDelete();
  assert.equal(await pending, true);
  assert.equal(m.groups[0]!.sessions.some((session) => session.id === "s1"), false);
  assert.deepEqual(removed, ["s1"]);
});

test("a refresh failure after accepted delete hides the row without reporting success", async () => {
  let deleted = false;
  let failRefresh = false;
  const removed: string[] = [];
  const m = new SidebarModel(fakeApi({
    deleteSession: () => {
      deleted = true;
      failRefresh = true;
      return Promise.resolve();
    },
    listSessions: (id) => failRefresh && id === "p1"
      ? Promise.reject(new ApiError(500, "internal", "refresh failed"))
      : Promise.resolve((sessions[id] ?? []).filter((session) => !deleted || session.id !== "s1")),
  }), { onRemoveSession: (id) => removed.push(id) });
  await m.refresh();

  assert.equal(await m.remove("s1"), false);
  assert.equal(m.groups[0]!.loadError, "refresh failed");
  assert.equal(m.groups[0]!.sessions.some((session) => session.id === "s1"), false);
  assert.deepEqual(removed, []);

  failRefresh = false;
  await m.refresh();
  assert.equal(m.groups[0]!.sessions.some((session) => session.id === "s1"), false);
  assert.deepEqual(removed, ["s1"]);
});

test("a late pre-delete refresh cannot repopulate a confirmed removal", async () => {
  let releaseStale!: (value: SessionSummary[]) => void;
  let listCalls = 0;
  let deleted = false;
  const removed: string[] = [];
  const m = new SidebarModel(fakeApi({
    deleteSession: () => {
      deleted = true;
      return Promise.resolve();
    },
    listSessions: (id) => {
      if (id !== "p1") return Promise.resolve([]);
      listCalls += 1;
      if (listCalls === 2) {
        return new Promise<SessionSummary[]>((resolve) => { releaseStale = resolve; });
      }
      return Promise.resolve((sessions[id] ?? []).filter((session) => !deleted || session.id !== "s1"));
    },
  }), { onRemoveSession: (id) => removed.push(id) });
  await m.refresh();

  const staleRefresh = m.refresh();
  const removal = m.remove("s1");
  assert.equal(await removal, true);
  releaseStale(sessions.p1!);
  await staleRefresh;

  assert.equal(m.groups[0]!.sessions.some((session) => session.id === "s1"), false);
  assert.deepEqual(removed, ["s1"]);
});

test("removeProject unregisters, refreshes, and fires onRemoveProject callback", async () => {
  const calls: string[] = [];
  const m = new SidebarModel(
    fakeApi({
      deleteProject: (id: string) => {
        calls.push(`api:${id}`);
        return Promise.resolve();
      },
    }),
    { onRemoveProject: (id) => calls.push(`cb:${id}`) },
  );
  assert.equal(await m.removeProject("p1"), true);
  assert.equal(m.error, null);
  assert.deepEqual(calls, ["api:p1", "cb:p1"]);
});

test("removeProject refusal stays in-band and does not fire onRemoveProject", async () => {
  // The server's live_sessions refusal: live sessions block removal. The
  // message reaches the user; the callback (and any teardown behind
  // it) never fires.
  const removed: string[] = [];
  const m = new SidebarModel(
    fakeApi({
      deleteProject: () =>
        Promise.reject(new ApiError(409, "live_sessions",
          "1 session(s) still starting or running (claude) — stop them before removing the project")),
    }),
    { onRemoveProject: (id) => removed.push(id) },
  );
  assert.equal(await m.removeProject("p1"), false);
  assert.match(m.error ?? "", /stop them before removing/);
  assert.deepEqual(removed, []);
});

test("renameProject renames, refreshes, then fires onRenameProject", async () => {
  const calls: string[] = [];
  const m = new SidebarModel(
    fakeApi({
      renameProject: (id: string, name: string) => {
        calls.push(`api:${id}:${name}`);
        return Promise.resolve();
      },
      listProjects: () => {
        calls.push("refresh");
        return Promise.resolve(projects);
      },
    }),
    { onRenameProject: (id) => calls.push(`cb:${id}`) },
  );
  assert.equal(await m.renameProject("p1", "Noise Deck"), true);
  assert.equal(m.error, null);
  // The callback fires AFTER the refresh — its consumer repaints chrome
  // from the refreshed groups, not the stale ones.
  assert.deepEqual(calls, ["api:p1:Noise Deck", "refresh", "cb:p1"]);
});

test("renameProject failure stays in-band and does not fire onRenameProject", async () => {
  const renamed: string[] = [];
  const m = new SidebarModel(
    fakeApi({
      renameProject: () =>
        Promise.reject(new ApiError(404, "not_found", "project not found")),
    }),
    { onRenameProject: (id) => renamed.push(id) },
  );
  assert.equal(await m.renameProject("p1", "x"), false);
  assert.match(m.error ?? "", /project not found/);
  assert.deepEqual(renamed, []);
});

test("modKeyLabel follows the platform, with the empty-string fallback", () => {
  assert.equal(modKeyLabel("macOS", ""), "⌘K");
  // An empty userAgentData platform must not hide a Mac
  // userAgent (was ??, which only falls back on undefined).
  assert.equal(modKeyLabel("", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7)"), "⌘K");
  assert.equal(modKeyLabel(undefined, "Mozilla/5.0 (Macintosh; Intel Mac OS X)"), "⌘K");
  assert.equal(modKeyLabel("Windows", ""), "Ctrl K");
  assert.equal(modKeyLabel("Linux", "X11; Linux x86_64"), "Ctrl K");
  assert.equal(modKeyLabel(undefined, "X11; Linux x86_64"), "Ctrl K");
});

test("providerDisplayName renders every provider lowercase", () => {
  assert.equal(providerDisplayName("shell"), "terminal");
  assert.equal(providerDisplayName("pi"), "pi");
  assert.equal(providerDisplayName("omp"), "omp");
  assert.equal(providerDisplayName("claude"), "claude");
  assert.equal(providerDisplayName("cline"), "cline");
  assert.equal(providerDisplayName("command-code"), "command");
  assert.equal(providerDisplayName("codex"), "codex");
  assert.equal(providerDisplayName("kimi"), "kimi");
  assert.equal(providerDisplayName("opencode"), "opencode");
  assert.equal(providerDisplayName("unknown"), "unknown");
  // Regression guard: no provider display name may contain an
  // uppercase letter, so a future provider cannot reintroduce title-casing.
  for (const p of MARKED_PROVIDERS) {
    assert.ok(!/[A-Z]/.test(providerDisplayName(p)),
      `provider ${p} display name must be lowercase`);
  }
});

test("the picker names command-code as the session does", () => {
  assert.equal(providerDisplayName("command-code"), "command");
});

// --- Mobile layout rules ---
// Below md the sidebar OVERLAYS the workspace rather than sitting beside
// it — as a flex sibling it left the terminal 118px of a 390px viewport
// (measured). An overlay must therefore start closed and get out of the
// way once it has been used.

test("the sidebar starts collapsed on narrow viewports only", () => {
  assert.equal(startsCollapsed(true), true, "narrow: an overlay must not cover the terminal on first paint");
  assert.equal(startsCollapsed(false), false, "wide: the sidebar is a normal column");
});

test("selecting a session dismisses the sidebar only on narrow viewports", () => {
  assert.equal(dismissOnSelect(true), true, "narrow: it covers the terminal the tap just switched to");
  assert.equal(dismissOnSelect(false), false, "wide: nothing is covered, so nothing closes");
});

test("a chat mark puts the tick in a fixed slot so marks line up across rows", () => {
  const withMark = sessionMetaHtml(sessions.p1![0]!, false, "live");
  assert.match(withMark, /data-meta-chat="live"/);
  assert.match(withMark, /data-meta-slot[^>]*\bw-\[26px\][^>]*justify-end[^>]*overflow-hidden/, "a fixed width, not a minimum: a wide tick is clipped rather than moving the mark");
  const withoutMark = sessionMetaHtml(sessions.p1![0]!, false, null);
  assert.doesNotMatch(withoutMark, /data-meta-slot/, "a row without a mark keeps its natural trailing edge");
  assert.doesNotMatch(withoutMark, /data-meta-chat/);
});

test("the picker receives inventory before session reconciliation finishes", async () => {
  let releaseSessions!: (sessions: SessionSummary[]) => void;
  const sessions = new Promise<SessionSummary[]>((resolve) => { releaseSessions = resolve; });
  let notify!: () => void;
  const notified = new Promise<void>((resolve) => { notify = resolve; });
  const m = new SidebarModel(fakeApi({listSessions: () => sessions}), {
    onProvidersChanged: () => notify(),
  });
  const refresh = m.refresh();
  await notified;
  assert.deepEqual(m.availableProviders, ["claude", "codex", "shell"]);
  assert.equal(m.loading, true, "the session request is still pending");
  releaseSessions([]);
  await refresh;
});

test("an older inventory response cannot replace a newer answer", async () => {
  let releaseFirst!: (providers: readonly string[]) => void;
  const first = new Promise<readonly string[]>((resolve) => { releaseFirst = resolve; });
  let calls = 0;
  const m = new SidebarModel(fakeApi({listProviders: () => ++calls === 1 ? first : Promise.resolve(["codex"])}));
  const older = m.refreshProviders();
  await m.refreshProviders();
  releaseFirst(["claude"]);
  await older;
  assert.deepEqual(m.availableProviders, ["codex"]);
});

// The picker is opened per project, so two openings race in exactly the way
// the inventory read does: an older answer landing last would rank the
// dialog by a project the user has left.
test("an older harness-usage response cannot replace a newer answer", async () => {
  let releaseFirst!: (report: HarnessUsageReport) => void;
  const first = new Promise<HarnessUsageReport>((resolve) => { releaseFirst = resolve; });
  let calls = 0;
  const m = new SidebarModel(fakeApi({
    getHarnessUsage: () => ++calls === 1
      ? first
      : Promise.resolve({ global: [], project: [{ provider: "codex", launches: 2, last_used_at: "2026-09-06T00:00:00Z" }] }),
  }));
  const older = m.refreshHarnessUsage("p1");
  await m.refreshHarnessUsage("p2");
  releaseFirst({ global: [], project: [{ provider: "claude", launches: 9, last_used_at: "2026-09-06T00:00:00Z" }] });
  await older;
  assert.deepEqual(m.harnessUsage?.project.map((one) => one.provider), ["codex"]);
});

// A read that fails leaves no ranking rather than a wrong one: the picker
// then draws the order the server sent.
test("a failed harness-usage read leaves the picker unranked", async () => {
  const m = new SidebarModel(fakeApi({
    getHarnessUsage: () => Promise.reject(new Error("offline")),
  }));
  await m.refreshHarnessUsage("p1");
  assert.equal(m.harnessUsage, null);
});

// Opening the picker in another project must not rank it by the project
// before it, not even for the moment the read is in flight.
test("opening another project's picker drops the previous ranking at once", async () => {
  let releaseSecond!: (report: HarnessUsageReport) => void;
  const second = new Promise<HarnessUsageReport>((resolve) => { releaseSecond = resolve; });
  let calls = 0;
  const m = new SidebarModel(fakeApi({
    getHarnessUsage: () => ++calls === 1
      ? Promise.resolve({ global: [], project: [{ provider: "claude", launches: 9, last_used_at: "2026-09-06T00:00:00Z" }] })
      : second,
  }));
  await m.refreshHarnessUsage("p1");
  assert.deepEqual(m.harnessUsage?.project.map((one) => one.provider), ["claude"]);
  const pending = m.refreshHarnessUsage("p2");
  assert.equal(m.harnessUsage, null, "the previous project's ranking outlived its picker");
  releaseSecond({ global: [], project: [] });
  await pending;
});
