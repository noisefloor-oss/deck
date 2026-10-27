import { test } from "node:test";
import assert from "node:assert/strict";
import { TutorialModel, WELCOMED, type TutorialLesson } from "./model.ts";
import { readdirSync, readFileSync } from "node:fs";
import { FactBook } from "./events.ts";
import { buildTutorialCatalog } from "./catalog.ts";
import { ApiError } from "../../api/http.ts";
import type { ApiClient } from "../../api/contracts.ts";
import type { Project } from "../../api/projects.ts";
import type { TutorialPreference, TutorialRecord } from "../../api/settings.ts";

// Model fixtures: the model walks any catalog, so these carry only ids and
// step counts. The shipped lessons are checked in lessons.test.ts.
function fixtureLesson(id: string, steps: number, optional = false): TutorialLesson {
  return {
    id,
    title: id,
    ...(optional ? { optional: true } : {}),
    steps: Array.from({ length: steps }, (_, index) => ({ title: `${id} ${index + 1}`, body: "Body." })),
  };
}
const defaultTutorialCatalog: readonly TutorialLesson[] = [fixtureLesson("welcome", 3), fixtureLesson("navigation", 3)];
const firstProjectCatalog: readonly TutorialLesson[] = ["create", "connect", "implement", "inspect", "preview", "finish"]
  .map((name) => fixtureLesson(`first-project-${name}`, 1));
const exploreTutorialCatalog: readonly TutorialLesson[] = ["projects", "terminal", "recovery", "workflow"]
  .map((name) => fixtureLesson(`explore-${name}`, 3, true));

// A settings call the test settles by hand: the promise is part of the
// handle so a test can await exactly the continuation it released. Built
// with the executor form because the project's lib is ES2022, which has
// no Promise.withResolvers.
function held<T>(): { promise: Promise<T>; resolve: (value: T) => void; reject: (cause: unknown) => void } {
  let resolve!: (value: T) => void;
  let reject!: (cause: unknown) => void;
  const promise = new Promise<T>((res, rej) => {
    resolve = res;
    reject = rej;
  });
  return { promise, resolve, reject };
}

// Stands in for the settings endpoints with exactly their contract: a read
// answers null when nothing was written, a write replaces the whole
// document. manualLoad/manualSave hold calls until the test settles them,
// which is how the deferred-answer races are driven.
class FakeTutorialApi {
  stored: TutorialRecord | null = null;
  saved: TutorialPreference[] = [];
  loadSignals: (AbortSignal | undefined)[] = [];
  saveSignals: (AbortSignal | undefined)[] = [];
  loads = 0;
  saves = 0;
  manualLoad = false;
  manualSave = false;
  failLoads: unknown[] = [];
  failSaves: unknown[] = [];
  listProjectsCalls = 0;
  projects: Project[] = [];
  pendingLoad: { promise: Promise<TutorialRecord | null>; resolve: (value: TutorialRecord | null) => void; reject: (cause: unknown) => void } | null = null;
  pendingSaves: Array<{ promise: Promise<TutorialRecord>; resolve: (value: TutorialRecord) => void; reject: (cause: unknown) => void; doc: TutorialPreference }> = [];
  private outstanding: Promise<unknown>[] = [];

  getTutorial(signal?: AbortSignal): Promise<TutorialRecord | null> {
    this.loads += 1;
    this.loadSignals.push(signal);
    if (this.manualLoad) {
      const call = held<TutorialRecord | null>();
      this.pendingLoad = call;
      return call.promise;
    }
    const failure = this.failLoads.shift();
    const promise = failure === undefined
      ? Promise.resolve(this.stored)
      : Promise.reject(failure);
    this.outstanding.push(promise);
    return promise;
  }

  saveTutorial(next: TutorialPreference, signal?: AbortSignal): Promise<TutorialRecord> {
    this.saves += 1;
    this.saved.push(next);
    this.saveSignals.push(signal);
    if (this.manualSave) {
      const call = held<TutorialRecord>();
      this.pendingSaves.push({ ...call, doc: next });
      return call.promise;
    }
    const failure = this.failSaves.shift();
    const record: TutorialRecord = { ...next, done: [...next.done], scope: "user" };
    const promise = failure === undefined
      ? Promise.resolve(record).then((kept) => {
        this.stored = kept;
        return kept;
      })
      : Promise.reject(failure);
    this.outstanding.push(promise);
    return promise;
  }

  // Drains every answer the model is waiting on — including answers its own
  // continuations go on to request — without inventing clock time.
  async settle(): Promise<void> {
    while (this.outstanding.length > 0) {
      const batch = this.outstanding.splice(0);
      await Promise.allSettled(batch);
    }
  }

  async listProjects(): Promise<Project[]> {
    this.listProjectsCalls += 1;
    return this.projects;
  }
}

const asClient = (fake: FakeTutorialApi): ApiClient => fake as unknown as ApiClient;

const manyProjects: Project[] = [
  { id: "p1", name: "one", canonical_path: "/p1", last_used_at: "t" },
  { id: "p2", name: "two", canonical_path: "/p2", last_used_at: "t" },
  { id: "p3", name: "three", canonical_path: "/p3", last_used_at: "t" },
  { id: "p4", name: "four", canonical_path: "/p4", last_used_at: "t" },
  { id: "p5", name: "five", canonical_path: "/p5", last_used_at: "t" },
];

test("first use opens the walkthrough regardless of how many projects exist", async () => {
  for (const projects of [[], manyProjects]) {
    const fake = new FakeTutorialApi();
    fake.projects = projects;
    const model = new TutorialModel(asClient(fake), defaultTutorialCatalog);
    model.load();
    await fake.settle();
    const state = model.state;
    assert.equal(state.phase, "ready");
    assert.equal(state.visible, true);
    assert.equal(state.dismissed, false);
    assert.equal(state.lesson, "welcome");
    assert.equal(state.step, 0);
    assert.deepEqual(state.done, []);
    assert.equal(state.scope, null);
    assert.equal(fake.loads, 1);
    // The record decides first use, never the workspace around it.
    assert.equal(fake.listProjectsCalls, 0);
  }
});

test("a stored record restores dismissal and position, and reopen resumes it", async () => {
  const fake = new FakeTutorialApi();
  fake.stored = { version: 1, dismissed: true, lesson: "navigation", step: 2, done: ["welcome"], scope: "user" };
  const changes: TutorialModel["state"][] = [];
  const model = new TutorialModel(asClient(fake), defaultTutorialCatalog, {
    onChange: (state) => changes.push(state),
  });
  model.load();
  await fake.settle();
  let state = model.state;
  assert.equal(state.visible, false);
  assert.equal(state.dismissed, true);
  assert.equal(state.scope, "user");
  model.reopen();
  await fake.settle();
  state = model.state;
  assert.equal(state.visible, true);
  assert.equal(state.lesson, "navigation");
  assert.equal(state.step, 2);
  assert.deepEqual(state.done, ["welcome"]);
  // The reopen replaced the whole document, position included.
  assert.equal(fake.saves, 1);
  assert.deepEqual(fake.saved[0], { version: 1, dismissed: false, lesson: "navigation", step: 2, done: ["welcome"] });
  // A fresh page reads that document back and resumes open at the same place.
  const reloaded = new TutorialModel(asClient(fake), defaultTutorialCatalog);
  reloaded.load();
  await fake.settle();
  assert.equal(reloaded.state.visible, true);
  assert.equal(reloaded.state.lesson, "navigation");
  assert.equal(reloaded.state.step, 2);
  assert.ok(changes.length >= 3);
  assert.equal(changes.at(-1)?.visible, true);
});

test("the welcome page stands before the first lesson until the user starts", async () => {
  const fake = new FakeTutorialApi();
  const model = new TutorialModel(asClient(fake), defaultTutorialCatalog);
  model.load();
  await fake.settle();
  assert.equal(model.state.welcome, true);
  // Explore on my own closes the tutorial; reopening brings the welcome back.
  model.close();
  model.reopen();
  await fake.settle();
  assert.equal(model.state.welcome, true);
  assert.equal(model.state.resumePage, false);
  model.start();
  await fake.settle();
  assert.equal(model.state.welcome, false);
  assert.equal(model.state.lesson, "welcome");
  assert.equal(model.state.step, 0);
  assert.deepEqual(fake.saved.at(-1), { version: 1, dismissed: false, lesson: "welcome", step: 0, done: [WELCOMED] });
  const reloaded = new TutorialModel(asClient(fake), defaultTutorialCatalog);
  reloaded.load();
  await fake.settle();
  assert.equal(reloaded.state.welcome, false);
});

test("a record with progress skips the welcome page", async () => {
  const records: TutorialRecord[] = [
    { version: 1, dismissed: false, lesson: "welcome", step: 1, done: [], scope: "user" },
    { version: 1, dismissed: false, lesson: "navigation", step: 0, done: [], scope: "user" },
    { version: 1, dismissed: false, lesson: "welcome", step: 0, done: ["navigation"], scope: "user" },
  ];
  for (const record of records) {
    const fake = new FakeTutorialApi();
    fake.stored = record;
    const model = new TutorialModel(asClient(fake), defaultTutorialCatalog);
    model.load();
    await fake.settle();
    assert.equal(model.state.welcome, false, JSON.stringify(record));
  }
  const fresh = new FakeTutorialApi();
  fresh.stored = { version: 1, dismissed: true, lesson: "welcome", step: 0, done: [], scope: "user" };
  const model = new TutorialModel(asClient(fresh), defaultTutorialCatalog);
  model.load();
  await fresh.settle();
  assert.equal(model.state.welcome, true);
  // Choosing the first lesson from the list opens it instead of the welcome page.
  model.selectLesson("welcome");
  assert.equal(model.state.welcome, false);
  assert.equal(model.state.lesson, "welcome");
  await fresh.settle();
  assert.deepEqual(fresh.saved.at(-1)?.done, [WELCOMED]);
});

test("an installation-wide record reports its shared scope", async () => {
  const fake = new FakeTutorialApi();
  fake.stored = { version: 1, dismissed: false, lesson: "welcome", step: 0, done: [], scope: "installation" };
  const model = new TutorialModel(asClient(fake), defaultTutorialCatalog);
  model.load();
  await fake.settle();
  assert.equal(model.state.scope, "installation");
  assert.equal(model.state.visible, true);
});

test("a slow initial read cannot undo a close made before it returns", async () => {
  const fake = new FakeTutorialApi();
  fake.manualLoad = true;
  fake.manualSave = true;
  const model = new TutorialModel(asClient(fake), defaultTutorialCatalog);
  model.load();
  model.close();
  assert.equal(model.state.phase, "loading");
  assert.equal(fake.saves, 0);
  const load = fake.pendingLoad;
  assert(load !== null);
  load.resolve({ version: 1, dismissed: false, lesson: "navigation", step: 1, done: ["welcome"], scope: "user" });
  await load.promise;
  const state = model.state;
  assert.equal(state.phase, "ready");
  // The user's close stands; the read cannot reopen the panel.
  assert.equal(state.visible, false);
  assert.equal(state.dismissed, true);
  // The close spoke only for dismissal: the stored place is still adopted,
  // so a reopen resumes where the record says the user stood.
  assert.equal(state.lesson, "navigation");
  assert.equal(state.step, 1);
  assert.deepEqual(state.done, ["welcome"]);
  const save = fake.pendingSaves[0];
  assert(save !== undefined);
  assert.deepEqual(save.doc, { version: 1, dismissed: true, lesson: "navigation", step: 1, done: ["welcome"] });
  save.resolve({ ...save.doc, scope: "user" });
  await save.promise;
  model.reopen();
  const resumed = model.state;
  assert.equal(resumed.visible, true);
  assert.equal(resumed.lesson, "navigation");
  assert.equal(resumed.step, 1);
});

test("a slow initial read cannot undo a manual reopen", async () => {
  const fake = new FakeTutorialApi();
  fake.manualLoad = true;
  const model = new TutorialModel(asClient(fake), defaultTutorialCatalog);
  model.load();
  model.reopen();
  const load = fake.pendingLoad;
  assert(load !== null);
  load.resolve({ version: 1, dismissed: true, lesson: "welcome", step: 0, done: [], scope: "user" });
  await load.promise;
  const state = model.state;
  assert.equal(state.phase, "ready");
  assert.equal(state.dismissed, false);
  assert.equal(state.visible, true);
});

test("navigation during a slow read keeps the user's position", async () => {
  const fake = new FakeTutorialApi();
  fake.manualLoad = true;
  const model = new TutorialModel(asClient(fake), defaultTutorialCatalog);
  model.load();
  model.next();
  const load = fake.pendingLoad;
  assert(load !== null);
  load.resolve({ version: 1, dismissed: false, lesson: "navigation", step: 2, done: ["welcome"], scope: "user" });
  await load.promise;
  const state = model.state;
  assert.equal(state.phase, "ready");
  assert.equal(state.visible, true);
  assert.equal(state.lesson, "welcome");
  assert.equal(state.step, 1);
  assert.deepEqual(state.done, []);
});

test("saves serialize and coalesce to the latest pending state", async () => {
  const fake = new FakeTutorialApi();
  fake.manualSave = true;
  const model = new TutorialModel(asClient(fake), defaultTutorialCatalog);
  model.load();
  await fake.settle();
  assert.equal(fake.saves, 0);
  model.next();
  assert.equal(fake.saves, 1);
  assert.deepEqual(fake.saved[0], { version: 1, dismissed: false, lesson: "welcome", step: 1, done: [] });
  // Two more gestures while the first write is still out: neither gets its
  // own request; the latest state alone follows the in-flight one.
  model.next();
  model.next();
  assert.equal(fake.saves, 1);
  const first = fake.pendingSaves[0];
  assert(first !== undefined);
  first.resolve({ ...first.doc, scope: "user" });
  await first.promise;
  assert.equal(fake.saves, 2);
  assert.deepEqual(fake.saved[1], { version: 1, dismissed: false, lesson: "navigation", step: 0, done: ["welcome"] });
  const second = fake.pendingSaves[1];
  assert(second !== undefined);
  second.resolve({ ...second.doc, scope: "user" });
  await second.promise;
  assert.equal(fake.saves, 2);
  assert.equal(model.state.saveError, null);
});

test("a failed write is reported once and not retried", async () => {
  const fake = new FakeTutorialApi();
  fake.failSaves.push(new ApiError(500, "internal", "could not save tutorial preference"));
  const errors: string[] = [];
  const model = new TutorialModel(asClient(fake), defaultTutorialCatalog, {
    onSaveError: (message) => errors.push(message),
  });
  model.load();
  await fake.settle();
  model.close();
  await fake.settle();
  // The panel is closed, yet the failure is still reported to the host.
  assert.equal(model.state.visible, false);
  assert.equal(model.state.saveError, "could not save tutorial preference");
  assert.deepEqual(errors, ["could not save tutorial preference"]);
  assert.equal(fake.saves, 1);
  // A newer gesture writes again and its success clears the error.
  model.reopen();
  await fake.settle();
  assert.equal(fake.saves, 2);
  assert.equal(model.state.saveError, null);
  assert.deepEqual(errors, ["could not save tutorial preference"]);
});

test("controller loss aborts in-flight work and resume adopts what the server kept", async () => {
  const fake = new FakeTutorialApi();
  fake.manualSave = true;
  const model = new TutorialModel(asClient(fake), defaultTutorialCatalog);
  model.load();
  await fake.settle();
  model.close();
  model.next();
  model.suspend();
  assert.equal(model.state.phase, "suspended");
  assert.equal(model.state.visible, false);
  assert.equal(fake.saveSignals[0]?.aborted, true);
  // Gestures while the controller is lost are ignored.
  model.reopen();
  assert.equal(model.state.phase, "suspended");
  // The server had already accepted the in-flight write when the abort
  // arrived; the abort cannot undo that, and the model does not pretend
  // otherwise — resume re-reads the record instead of replaying the drop.
  const doc = fake.saved[0];
  assert(doc !== undefined);
  fake.stored = { ...doc, done: [...doc.done], scope: "user" };
  const save = fake.pendingSaves[0];
  assert(save !== undefined);
  save.resolve(fake.stored);
  await save.promise;
  // The queued follow-up was dropped, and nothing retried it.
  assert.equal(fake.saves, 1);
  model.resume();
  await fake.settle();
  const state = model.state;
  assert.equal(fake.loads, 2);
  assert.equal(state.phase, "ready");
  assert.equal(state.dismissed, true);
  assert.equal(state.visible, false);
});

test("dispose ignores late answers and later gestures", async () => {
  const fake = new FakeTutorialApi();
  fake.manualLoad = true;
  fake.manualSave = true;
  const errors: string[] = [];
  const model = new TutorialModel(asClient(fake), defaultTutorialCatalog, {
    onSaveError: (message) => errors.push(message),
  });
  model.load();
  model.next();
  model.dispose();
  assert.equal(model.state.phase, "disposed");
  assert.equal(fake.loadSignals[0]?.aborted, true);
  assert.equal(fake.saves, 0);
  // The withdrawn requests still answer; nothing they say is applied and
  // no failure is reported for work that was deliberately abandoned.
  const load = fake.pendingLoad;
  assert(load !== null);
  load.resolve({ version: 1, dismissed: true, lesson: "navigation", step: 2, done: ["welcome"], scope: "user" });
  await load.promise;

  const state = model.state;
  assert.equal(state.phase, "disposed");
  assert.equal(state.visible, false);
  assert.equal(state.lesson, "welcome");
  assert.equal(state.scope, null);
  assert.deepEqual(errors, []);
  model.reopen();
  model.next();
  assert.equal(fake.saves, 0);
  assert.equal(model.state.phase, "disposed");
});

test("an unknown stored version is read-only and preserved", async () => {
  const fake = new FakeTutorialApi();
  fake.stored = { version: 2, dismissed: false, lesson: "welcome", step: 0, done: [], scope: "user" };
  const model = new TutorialModel(asClient(fake), defaultTutorialCatalog);
  model.load();
  await fake.settle();
  const state = model.state;
  assert.equal(state.unsupportedVersion, true);
  assert.equal(state.visible, false);
  assert.equal(state.scope, "user");
  // Presentation still moves locally, but nothing is ever written over
  // the record the newer build owns.
  model.reopen();
  assert.equal(model.state.visible, true);
  model.next();
  model.close();
  await fake.settle();
  assert.equal(fake.saves, 0);
  assert.deepEqual(fake.stored, { version: 2, dismissed: false, lesson: "welcome", step: 0, done: [], scope: "user" });
});

test("a record naming lessons the catalog no longer ships falls back without losing dismissal", async () => {
  const fake = new FakeTutorialApi();
  fake.stored = { version: 1, dismissed: true, lesson: "first-project", step: 99, done: ["first-project"], scope: "user" };
  const singleLesson: readonly TutorialLesson[] = [
    { id: "welcome", title: "Welcome", steps: [{ title: "Only step", body: "Body." }] },
  ];
  const model = new TutorialModel(asClient(fake), singleLesson);
  model.load();
  await fake.settle();
  let state = model.state;
  assert.equal(state.dismissed, true);
  assert.equal(state.visible, false);
  assert.equal(state.lesson, "welcome");
  assert.equal(state.step, 0);
  assert.deepEqual(state.done, ["first-project"]);
  model.reopen();
  await fake.settle();
  state = model.state;
  assert.equal(state.visible, true);
  assert.equal(state.lesson, "welcome");
  assert.equal(state.step, 0);
  assert.deepEqual(fake.saved[0], { version: 1, dismissed: false, lesson: "welcome", step: 0, done: ["first-project"] });

  // The same fallback against the shipped catalog: a stored step past the
  // lesson's end clamps to its last step.
  const fake2 = new FakeTutorialApi();
  fake2.stored = { version: 1, dismissed: true, lesson: "welcome", step: 99, done: [], scope: "user" };
  const model2 = new TutorialModel(asClient(fake2), defaultTutorialCatalog);
  model2.load();
  await fake2.settle();
  assert.equal(model2.state.dismissed, true);
  assert.equal(model2.state.lesson, "welcome");
  assert.equal(model2.state.step, 2);
});

test("navigation walks lessons, records progress, and finishes at the end", async () => {
  const fake = new FakeTutorialApi();
  const model = new TutorialModel(asClient(fake), defaultTutorialCatalog);
  model.load();
  await fake.settle();
  model.next();
  model.next();
  let state = model.state;
  assert.equal(state.lesson, "welcome");
  assert.equal(state.step, 2);
  assert.deepEqual(state.done, []);
  model.next();
  state = model.state;
  assert.equal(state.lesson, "navigation");
  assert.equal(state.step, 0);
  assert.deepEqual(state.done, ["welcome"]);
  model.back();
  state = model.state;
  assert.equal(state.lesson, "welcome");
  assert.equal(state.step, 2);
  // Going back does not unfinish a lesson.
  assert.deepEqual(state.done, ["welcome"]);
  model.back();
  model.back();
  assert.equal(model.state.step, 0);
  const savesBefore = fake.saves;
  model.back();
  assert.equal(fake.saves, savesBefore);
  // Walking off the catalog's last step finishes the walkthrough.
  for (let i = 0; i < 10; i++) model.next();
  await fake.settle();
  state = model.state;
  assert.equal(state.dismissed, true);
  assert.equal(state.visible, false);
  assert.deepEqual(state.done, ["welcome", "navigation"]);
  assert.deepEqual(fake.saved.at(-1), { version: 1, dismissed: true, lesson: "navigation", step: 2, done: ["welcome", "navigation"] });
  // A fresh page reads that back finished.
  const reloaded = new TutorialModel(asClient(fake), defaultTutorialCatalog);
  reloaded.load();
  await fake.settle();
  assert.equal(reloaded.state.visible, false);
  assert.deepEqual([...reloaded.state.done], ["welcome", "navigation"]);
});

test("collapse hides the panel for the session only and never writes", async () => {
  const fake = new FakeTutorialApi();
  const model = new TutorialModel(asClient(fake), defaultTutorialCatalog);
  model.load();
  await fake.settle();
  const savesBefore = fake.saves;
  model.collapse();
  let state = model.state;
  assert.equal(state.visible, false);
  assert.equal(state.dismissed, false);
  assert.equal(state.collapsed, true);
  assert.equal(fake.saves, savesBefore);
  // Collapse is not persisted: a fresh page shows the panel again.
  const reloaded = new TutorialModel(asClient(fake), defaultTutorialCatalog);
  reloaded.load();
  await fake.settle();
  assert.equal(reloaded.state.visible, true);
  // Within the session, expanding restores it without a write.
  model.expand();
  state = model.state;
  assert.equal(state.visible, true);
  assert.equal(state.collapsed, false);
  assert.equal(fake.saves, savesBefore);
  // A controller-loss round trip in the same page must not pop a
  // collapsed panel back open.
  model.collapse();
  model.suspend();
  model.resume();
  await fake.settle();
  assert.equal(model.state.collapsed, true);
  assert.equal(model.state.visible, false);
});

test("a failed read neither establishes first use nor overwrites unknown progress", async () => {
  const fake = new FakeTutorialApi();
  fake.failLoads.push(new ApiError(500, "internal", "could not read tutorial preference"));
  const model = new TutorialModel(asClient(fake), defaultTutorialCatalog);
  model.load();
  await fake.settle();
  assert.equal(model.state.phase, "ready");
  assert.equal(model.state.visible, false);
  assert.equal(model.state.loadError, "could not read tutorial preference");
  model.reopen();
  assert.equal(model.state.visible, true);
  model.close();
  await fake.settle();
  assert.equal(model.state.visible, false);
  assert.equal(fake.saves, 0);
});

test("a gesture before an unknown-version read never sends a replacement", async () => {
  const fake = new FakeTutorialApi();
  fake.manualLoad = true;
  const model = new TutorialModel(asClient(fake), defaultTutorialCatalog);
  model.load();
  model.close();
  model.reopen();
  assert.equal(fake.saves, 0);
  fake.pendingLoad!.resolve({ version: 2, dismissed: true, lesson: "future", step: 8, done: [], scope: "user" });
  await fake.settle();
  assert.equal(model.state.unsupportedVersion, true);
  assert.equal(fake.saves, 0);
  assert.equal(model.state.visible, true, "the pending manual reopen still takes effect without a write");
});

test("manual reopening expands a previously collapsed tutorial", async () => {
  const fake = new FakeTutorialApi();
  const model = new TutorialModel(asClient(fake), defaultTutorialCatalog);
  model.load();
  await fake.settle();
  model.collapse();
  model.close();
  model.reopen();
  assert.equal(model.state.collapsed, false);
  assert.equal(model.state.visible, true);
});

test("dispose aborts a write and drops queued gestures without a late error", async () => {
  const fake = new FakeTutorialApi();
  fake.manualSave = true;
  const errors: string[] = [];
  const model = new TutorialModel(asClient(fake), defaultTutorialCatalog, {
    onSaveError: (message) => errors.push(message),
  });
  model.load();
  await fake.settle();
  model.next();
  model.close();
  assert.equal(fake.saves, 1);
  model.dispose();
  assert.equal(fake.saveSignals[0]?.aborted, true);
  fake.pendingSaves[0]!.reject(new Error("late failure"));
  await fake.pendingSaves[0]!.promise.catch(() => {});
  assert.equal(fake.saves, 1);
  assert.deepEqual(errors, []);
});

test("a superseded write cannot clear a new generation's queued save", async () => {
  const fake = new FakeTutorialApi();
  fake.manualSave = true;
  const model = new TutorialModel(asClient(fake), defaultTutorialCatalog);
  model.load();
  await fake.settle();
  model.next();
  const old = fake.pendingSaves[0]!;
  model.suspend();
  model.resume();
  await fake.settle();
  model.selectLesson("navigation");
  model.next();
  old.resolve({ ...old.doc, scope: "user" });
  await old.promise;
  assert.equal(fake.saves, 2);
  const current = fake.pendingSaves[1]!;
  current.resolve({ ...current.doc, scope: "user" });
  await current.promise;
  assert.equal(fake.saves, 3);
  assert.equal(fake.saved[2]!.lesson, "navigation");
  assert.equal(fake.saved[2]!.step, 1);
});

// The composed catalog live and preview both mount: the guided course
// (default lessons plus the first-project curriculum) followed by the
// optional Explore Deck chapters.
const liveCatalog: readonly TutorialLesson[] = [
  ...defaultTutorialCatalog,
  ...firstProjectCatalog,
  ...exploreTutorialCatalog,
];

const stepsOf = (lessons: readonly TutorialLesson[]): number =>
  lessons.reduce((total, lesson) => total + lesson.steps.length, 0);

test("explore chapter ids are unique and disjoint from the guided course", () => {
  const ids = liveCatalog.map((lesson) => lesson.id);
  assert.equal(new Set(ids).size, ids.length);
  assert.ok(exploreTutorialCatalog.every((lesson) => lesson.optional === true));
  assert.ok(liveCatalog.slice(0, defaultTutorialCatalog.length + firstProjectCatalog.length)
    .every((lesson) => lesson.optional !== true));
});

test("Finish at the last guided step dismisses without entering optional chapters", async () => {
  const fake = new FakeTutorialApi();
  const model = new TutorialModel(asClient(fake), liveCatalog);
  model.load();
  await fake.settle();
  const guidedSteps = stepsOf(liveCatalog) - stepsOf(exploreTutorialCatalog);
  for (let taken = 1; taken < guidedSteps; taken++) {
    // Only the final guided step may offer Finish on the way there.
    if (taken < guidedSteps - 1) assert.equal(model.finishing, false);
    model.next();
  }
  await fake.settle();
  assert.equal(model.state.lesson, "first-project-finish");
  assert.equal(model.finishing, true);
  assert.equal(model.state.dismissed, false);
  model.next();
  await fake.settle();
  assert.equal(model.state.dismissed, true);
  assert.equal(model.state.visible, false);
  // The dismissal happened on the guided course's last step, not past it.
  assert.equal(model.state.lesson, "first-project-finish");
  assert.ok(fake.saved.every((doc) => !doc.lesson.startsWith("explore-")));
  assert.deepEqual(fake.saved.at(-1)?.done.slice(-1), ["first-project-finish"]);
});

test("an explicit choice opens, walks and persists an optional chapter", async () => {
  const fake = new FakeTutorialApi();
  const model = new TutorialModel(asClient(fake), liveCatalog);
  model.load();
  await fake.settle();
  model.selectLesson("explore-recovery");
  await fake.settle();
  let state = model.state;
  assert.equal(state.visible, true);
  assert.equal(state.dismissed, false);
  assert.equal(state.lesson, "explore-recovery");
  assert.equal(state.step, 0);
  assert.equal(fake.saved.at(-1)?.lesson, "explore-recovery");
  // A fresh page resumes the optional chapter from the saved record.
  const reloaded = new TutorialModel(asClient(fake), liveCatalog);
  reloaded.load();
  await fake.settle();
  assert.equal(reloaded.state.visible, true);
  assert.equal(reloaded.state.lesson, "explore-recovery");
  // Within the chosen chapters, Next walks steps and neighbors, and the
  // last optional step is the only other place the walkthrough finishes.
  while (!reloaded.finishing) reloaded.next();
  await fake.settle();
  state = reloaded.state;
  assert.equal(state.lesson, "explore-workflow");
  assert.equal(state.step, exploreTutorialCatalog.at(-1)!.steps.length - 1);
  reloaded.next();
  await fake.settle();
  assert.equal(reloaded.state.dismissed, true);
  assert.equal(reloaded.state.lesson, "explore-workflow");
});

test("the default catalog still finishes only at its own last step", async () => {
  const fake = new FakeTutorialApi();
  const model = new TutorialModel(asClient(fake), defaultTutorialCatalog);
  model.load();
  await fake.settle();
  let seen = 0;
  while (!model.finishing) {
    model.next();
    seen += 1;
  }
  await fake.settle();
  assert.equal(seen, stepsOf(defaultTutorialCatalog) - 1);
  assert.equal(model.state.lesson, "navigation");
  model.next();
  await fake.settle();
  assert.equal(model.state.dismissed, true);
  assert.deepEqual([...model.state.done], ["welcome", "navigation"]);
});

test("Back stops at the optional course boundary without another preference write", async () => {
  const fake = new FakeTutorialApi();
  const model = new TutorialModel(asClient(fake), liveCatalog);
  model.load();
  await fake.settle();
  model.selectLesson("explore-projects");
  await fake.settle();
  const writes = fake.saved.length;
  assert.equal(model.starting, true);
  model.back();
  await fake.settle();
  assert.equal(model.state.lesson, "explore-projects");
  assert.equal(model.state.step, 0);
  assert.equal(fake.saved.length, writes);
  model.next();
  assert.equal(model.starting, false);
  model.back();
  assert.equal(model.starting, true);
  model.selectLesson("explore-terminal");
  model.back();
  assert.equal(model.state.lesson, "explore-projects");
  assert.equal(model.state.step, exploreTutorialCatalog[0]!.steps.length - 1);
});

test("Next and Back skip hidden steps and the count leaves them out", async () => {
  const fake = new FakeTutorialApi();
  const lesson: TutorialLesson = {
    id: "hide",
    title: "Hide",
    steps: [
      { title: "One", body: "" },
      { title: "Two", body: "", when: { name: "agent_needs_login", arg: null } },
      { title: "Three", body: "" },
    ],
  };
  const model = new TutorialModel(asClient(fake), [lesson], { hidden: (step) => step.when !== undefined });
  model.load();
  await fake.settle();
  assert.deepEqual(model.visibleSteps(lesson), [0, 2]);
  model.next();
  assert.equal(model.state.step, 2);
  assert.equal(model.finishing, true);
  model.back();
  assert.equal(model.state.step, 0);
  assert.equal(model.starting, true);
});

test("a lesson that opens on a hidden step starts at its first visible step", async () => {
  const fake = new FakeTutorialApi();
  const lesson: TutorialLesson = {
    id: "late",
    title: "Late",
    steps: [{ title: "Hidden", body: "", when: { name: "starter_exists", arg: null } }, { title: "Shown", body: "" }],
  };
  const model = new TutorialModel(asClient(fake), [fixtureLesson("welcome", 1), lesson], { hidden: (step) => step.when !== undefined });
  model.load();
  await fake.settle();
  model.selectLesson("late");
  assert.equal(model.state.step, 1);
  assert.equal(model.starting, false);
  model.back();
  assert.equal(model.state.lesson, "welcome");
});

// Steps 0 and 2 open a panel; step 1 does not.
function panelLesson(): TutorialLesson {
  return {
    id: "panels",
    title: "Panels",
    steps: [
      { title: "Tasks", body: "", opens: "tasks" },
      { title: "Plain", body: "" },
      { title: "Files", body: "", opens: "files" },
    ],
  };
}

test("arriving on a step that opens a panel opens it once", async () => {
  const fake = new FakeTutorialApi();
  const opened: string[] = [];
  const model = new TutorialModel(asClient(fake), [panelLesson()], { openPanel: (panel) => opened.push(panel) });
  model.load();
  await fake.settle();
  assert.deepEqual(opened, ["tasks"], "resuming on the step opens its panel");
  model.next();
  assert.deepEqual(opened, ["tasks"], "a step with no panel opens nothing");
  model.next();
  assert.deepEqual(opened, ["tasks", "files"]);
  model.back();
  model.back();
  assert.deepEqual(opened, ["tasks", "files", "tasks"], "Back arrives too");
});

test("a panel the user closed stays closed while they stay on the step", async () => {
  const fake = new FakeTutorialApi();
  const opened: string[] = [];
  const model = new TutorialModel(asClient(fake), [panelLesson()], { openPanel: (panel) => opened.push(panel) });
  model.load();
  await fake.settle();
  model.setDocked("tasks");
  model.setDocked(null);
  model.collapse();
  model.expand();
  model.reopen();
  model.suspend();
  model.resume();
  await fake.settle();
  assert.deepEqual(opened, ["tasks"], "repaints, reopening and re-reads do not reopen the panel");
});

test("choosing a lesson arrives on its first step", async () => {
  const fake = new FakeTutorialApi();
  const opened: string[] = [];
  const model = new TutorialModel(asClient(fake), [fixtureLesson("welcome", 1), panelLesson()], { openPanel: (panel) => opened.push(panel) });
  model.load();
  await fake.settle();
  assert.deepEqual(opened, []);
  model.selectLesson("panels");
  assert.deepEqual(opened, ["tasks"]);
});

test("a dismissed tutorial opens no panel, and arrival waits for the first read", async () => {
  const fake = new FakeTutorialApi();
  fake.stored = { version: 1, dismissed: true, lesson: "panels", step: 0, done: [], scope: "user" };
  const opened: string[] = [];
  const model = new TutorialModel(asClient(fake), [panelLesson()], { openPanel: (panel) => opened.push(panel) });
  model.next();
  assert.deepEqual(opened, [], "no arrival while the stored record is unread");
  model.load();
  await fake.settle();
  assert.deepEqual(opened, []);
});

test("the step page folds on arrival and whenever the docked panel changes", async () => {
  const fake = new FakeTutorialApi();
  const model = new TutorialModel(asClient(fake), [panelLesson()]);
  model.load();
  await fake.settle();
  model.showStep();
  assert.equal(model.state.stepShown, false, "nothing to unfold while no panel is open");
  model.setDocked("tasks");
  model.showStep();
  assert.equal(model.state.stepShown, true);
  model.hideStep();
  assert.equal(model.state.stepShown, false);
  model.showStep();
  model.next();
  assert.equal(model.state.stepShown, false, "arriving on another step folds the page");
  model.showStep();
  model.setDocked(null);
  assert.equal(model.state.stepShown, false, "closing the panel returns to the full page");
  model.setDocked("tasks");
  assert.equal(model.state.stepShown, false, "a new panel starts folded");
  model.showStep();
  model.setDocked("files");
  assert.equal(model.state.stepShown, false, "switching directly to another panel folds the page");
});

test("reopening partway through a lesson offers the resume page and writes nothing for it", async () => {
  const fake = new FakeTutorialApi();
  fake.stored = { version: 1, dismissed: true, lesson: "navigation", step: 1, done: ["welcome"], scope: "user" };
  const model = new TutorialModel(asClient(fake), defaultTutorialCatalog);
  model.load();
  await fake.settle();
  assert.equal(model.state.resumePage, false);
  model.reopen();
  await fake.settle();
  assert.equal(model.state.resumePage, true);
  const saves = fake.saves;
  model.leaveResumePage();
  await fake.settle();
  assert.equal(model.state.resumePage, false);
  assert.equal(model.state.lesson, "navigation");
  assert.equal(model.state.step, 1);
  assert.equal(fake.saves, saves);
});

test("no resume page on first load, at a lesson's first step, on a closing page, or for a finished lesson", async () => {
  const closing: TutorialLesson = { id: "wrap", title: "wrap", steps: [{ title: "a", body: "." }, { title: "Done", body: ".", done: { next: "all-lessons" } }] };
  const cases: { record: TutorialRecord | null; catalog: readonly TutorialLesson[] }[] = [
    { record: null, catalog: defaultTutorialCatalog },
    { record: { version: 1, dismissed: true, lesson: "welcome", step: 0, done: [], scope: "user" }, catalog: defaultTutorialCatalog },
    { record: { version: 1, dismissed: true, lesson: "navigation", step: 0, done: ["welcome"], scope: "user" }, catalog: defaultTutorialCatalog },
    { record: { version: 1, dismissed: true, lesson: "wrap", step: 1, done: [], scope: "user" }, catalog: [closing] },
    { record: { version: 1, dismissed: true, lesson: "navigation", step: 2, done: ["welcome", "navigation"], scope: "user" }, catalog: defaultTutorialCatalog },
  ];
  for (const { record, catalog } of cases) {
    const fake = new FakeTutorialApi();
    fake.stored = record;
    const model = new TutorialModel(asClient(fake), catalog);
    model.load();
    await fake.settle();
    model.reopen();
    await fake.settle();
    assert.equal(model.state.resumePage, false, JSON.stringify(record));
  }
});

test("an open panel is not interrupted by a reopen, and choosing a lesson ends the resume page", async () => {
  const fake = new FakeTutorialApi();
  fake.stored = { version: 1, dismissed: false, lesson: "navigation", step: 1, done: [], scope: "user" };
  const model = new TutorialModel(asClient(fake), defaultTutorialCatalog);
  model.load();
  await fake.settle();
  model.reopen();
  assert.equal(model.state.resumePage, false);
  model.close();
  model.reopen();
  assert.equal(model.state.resumePage, true);
  model.selectLesson("welcome");
  assert.equal(model.state.resumePage, false);
});

// Event-driven steps. The fact book stands in for what live.ts observes.
function eventLesson(): TutorialLesson {
  const call = (name: string, arg: string | null = null) => ({ name, arg });
  return {
    id: "events",
    title: "events",
    steps: [
      { title: "pick", body: "", advanceOn: [call("agent_picked")] },
      { title: "send", body: "", advanceOn: [call("input_sent"), call("file_opened", "modified")] },
      { title: "explain", body: "", advanceOn: [call("continue")] },
      { title: "last", body: "", advanceOn: [call("starter_created")] },
      { title: "Done", body: "", done: { next: "all-lessons" } },
    ],
  };
}

function eventModel(fake: FakeTutorialApi, facts: FactBook) {
  return new TutorialModel(asClient(fake), [eventLesson()], { facts });
}

test("a step completes when its event is observed, one step per event", async () => {
  const fake = new FakeTutorialApi();
  const facts = new FactBook();
  let picked = false;
  facts.current = (call) => call.name === "agent_picked" && picked;
  const model = eventModel(fake, facts);
  model.load();
  await fake.settle();
  assert.equal(model.state.step, 0);
  model.factsChanged();
  assert.equal(model.state.step, 0, "nothing happened yet");
  picked = true;
  model.factsChanged();
  assert.equal(model.state.step, 1);
  model.factsChanged();
  assert.equal(model.state.step, 1, "the same event does not complete the next step");
});

test("an ordered pair needs the second event after the first", async () => {
  const fake = new FakeTutorialApi();
  const facts = new FactBook();
  const model = eventModel(fake, facts);
  facts.current = (call) => call.name === "agent_picked";
  model.load();
  await fake.settle();
  model.factsChanged();
  assert.equal(model.state.step, 1);
  facts.note(null, { name: "file_opened", arg: "modified" });
  model.factsChanged();
  assert.equal(model.state.step, 1, "the file before the input does not count");
  facts.note({ lesson: "events", step: 1 }, { name: "input_sent", arg: null });
  model.factsChanged();
  assert.equal(model.state.step, 1, "the input alone is half");
  facts.note(null, { name: "file_opened", arg: "modified" });
  model.factsChanged();
  assert.equal(model.state.step, 2);
});

test("steps whose events already happened complete on arrival, and a Continue step stops the run", async () => {
  const fake = new FakeTutorialApi();
  const facts = new FactBook();
  facts.current = (call) => call.name === "agent_picked" || call.name === "starter_created";
  facts.note({ lesson: "events", step: 1 }, { name: "input_sent", arg: null });
  facts.note(null, { name: "file_opened", arg: "modified" });
  const model = eventModel(fake, facts);
  model.load();
  await fake.settle();
  assert.equal(model.state.step, 2, "pick and send were already true; explain waits for Continue");
  model.next();
  assert.equal(model.state.step, 4, "the last step was already true too, so the done page shows");
  assert.deepEqual(model.state.done, ["events"], "finishing the steps records the lesson");
});

test("Skip step moves on without recording the lesson as done", async () => {
  const fake = new FakeTutorialApi();
  const model = eventModel(fake, new FactBook());
  model.load();
  await fake.settle();
  model.skip();
  model.skip();
  model.skip();
  model.skip();
  assert.equal(model.state.step, 4);
  assert.deepEqual(model.state.done, []);
});

test("a step the user went Back to does not complete again from facts that were true", async () => {
  const fake = new FakeTutorialApi();
  const facts = new FactBook();
  let picked = true;
  facts.current = (call) => call.name === "agent_picked" && picked;
  const model = eventModel(fake, facts);
  model.load();
  await fake.settle();
  assert.equal(model.state.step, 1);
  model.back();
  assert.equal(model.state.step, 0);
  model.factsChanged();
  assert.equal(model.state.step, 0, "held while nothing new happens");
  picked = false;
  model.factsChanged();
  picked = true;
  model.factsChanged();
  assert.equal(model.state.step, 1, "it happens again");
});

test("an event never completes the last step", async () => {
  const fake = new FakeTutorialApi();
  const facts = new FactBook();
  facts.current = () => true;
  const lesson: TutorialLesson = { id: "solo", title: "solo", steps: [{ title: "only", body: "", advanceOn: [{ name: "agent_picked", arg: null }] }] };
  const model = new TutorialModel(asClient(fake), [lesson], { facts });
  model.load();
  await fake.settle();
  model.factsChanged();
  assert.equal(model.state.step, 0);
  assert.equal(model.state.dismissed, false);
});

test("a step waiting on a reload completes only on the step the page loaded onto", async () => {
  const fake = new FakeTutorialApi();
  fake.stored = { version: 1, dismissed: false, lesson: "reload", step: 1, done: [], scope: "user" };
  const facts = new FactBook();
  let live = false;
  facts.current = (call, context) => call.name === "page_reloaded" && context.restored && live;
  const call = { name: "page_reloaded", arg: "session_live" };
  const lesson: TutorialLesson = { id: "reload", title: "reload", steps: [
    { title: "a", body: "", advanceOn: [{ name: "continue", arg: null }] },
    { title: "b", body: "", advanceOn: [call] },
    { title: "Done", body: "", done: { next: "all-lessons" } },
  ] };
  const model = new TutorialModel(asClient(fake), [lesson], { facts });
  model.load();
  await fake.settle();
  assert.equal(model.state.step, 1, "no live session yet");
  live = true;
  model.factsChanged();
  assert.equal(model.state.step, 2);
});

test("prompts typed in lesson 1 do not complete lesson 2's prompt steps", async () => {
  const dir = new URL("./lessons/en/", import.meta.url);
  const catalog = buildTutorialCatalog(Object.fromEntries(readdirSync(dir)
    .filter((name) => name.endsWith(".md"))
    .map((name) => [name, readFileSync(new URL(name, dir), "utf8")])));
  const index = (id: string, title: string) => catalog.find((l) => l.id === id)!.steps.findIndex((step) => step.title === title);
  const fake = new FakeTutorialApi();
  fake.stored = { version: 1, dismissed: false, lesson: "lesson-1", step: index("lesson-1", "Ask {agent} to build it"), done: [], scope: "user" };
  const facts = new FactBook();
  let ccparseAttached = false;
  facts.current = (call) => call.name === "task_exists" || (call.name === "session_attached" && call.arg === "project=ccparse" && ccparseAttached);
  const model = new TutorialModel(asClient(fake), catalog, { facts });
  model.load();
  await fake.settle();
  const typed = { lesson: "lesson-1", step: model.state.step };
  facts.note(typed, { name: "input_sent", arg: null }, { name: "input_sent", arg: "project=relnotes" });
  model.selectLesson("lesson-2");
  const lesson2 = catalog.find((l) => l.id === "lesson-2")!;
  const start = index("lesson-2", "Start the other agent");
  assert.equal(model.state.step, start, "the second task already exists, so the lesson opens on starting the other agent");
  ccparseAttached = true;
  model.factsChanged();
  assert.equal(model.state.step, start, "attaching ccparse alone does not finish it");
  facts.note({ lesson: "lesson-2", step: start }, { name: "input_sent", arg: null }, { name: "input_sent", arg: "project=ccparse" });
  model.factsChanged();
  const ask = index("lesson-2", "Ask the relnotes agent to find it");
  assert.equal(model.state.step, ask);
  model.factsChanged();
  assert.equal(model.state.step, ask, "lesson 1's relnotes prompt does not answer this one");
  assert.ok(lesson2.steps[ask]);
});
