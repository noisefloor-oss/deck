// Tests for the Taskmaster panel model. DOM-free: a fake ApiClient drives
// the model exactly as the server would.
import { normalizedPriority, priorityLabel, taskID } from "../../lib/task-facts.ts";
import assert from "node:assert/strict";
import test from "node:test";
import { readFileSync } from "node:fs";

import type { ConsoleSettings, SidebarLayout, TutorialPreference } from "../../api/settings.ts";
import { ApiError } from "../../api/http.ts";
import type { ApiClient } from "../../api/contracts.ts";
import type { Project } from "../../api/projects.ts";
import type { TaskmasterInitResult, TaskmasterTasks, TmTask, TmRun } from "../../api/tasks.ts";
import {
  TaskmasterPanel,
  type TaskmasterPanelShellCallbacks,
  type TaskmasterPanelTimer,
} from "./taskmaster-panel-dom.ts";
import {
  boardTasks,
  compareTaskIDs,
  depsDisplay,
  kanbanColumns,
  qualifySubtaskIDs,
  PRD_CANONICAL_NAME,
  priorityRank,
  TaskmasterPanelModel,
} from "./taskmaster-panel.ts";

const projects: Project[] = [
  { id: "p1", name: "demo", canonical_path: "/x", last_used_at: "t" },
];

const sampleTasks: TaskmasterTasks = {
  other: { tasks: [{ id: 9, title: "other-tag task", status: "pending" }] },
  master: {
    tasks: [
      { id: "1", title: "first", status: "done", priority: "high", dependencies: [] },
      {
        id: "2",
        title: "second",
        status: "in-progress",
        dependencies: ["1"],
        subtasks: [{ id: 1, parentId: "2", title: "sub", status: "pending" }],
      },
    ],
  },
};

function tmRun(partial: Partial<TmRun> = {}): TmRun {
  return {
    id: "r1",
    project_id: "p1",
    argv: ["/usr/local/bin/task-master", "parse-prd", "/x/.taskmaster/docs/PRD.md"],
    prd: "PRD.md",
    status: "running",
    exit_code: null,
    started_at: "2026-01-01T00:00:00.000Z",
    ...partial,
  };
}

// storedTask resolves an id the way the server does: it matches each node's
// parent-qualified id at any depth and returns the node as stored.
function storedTask(tasks: TmTask[], wanted: string): Promise<TmTask> {
  const find = (nodes: TmTask[], parentID: string): TmTask | undefined => {
    for (const node of nodes) {
      const own = String(node.id);
      const id = parentID !== "" && !own.startsWith(`${parentID}.`) ? `${parentID}.${own}` : own;
      if (id === wanted) return node;
      const found = find(node.subtasks ?? [], id);
      if (found !== undefined) return found;
    }
    return undefined;
  };
  const task = find(tasks, "");
  return task === undefined
    ? Promise.reject(new ApiError(404, "not_found", "task not found"))
    : Promise.resolve(task);
}

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
    saveTutorial: (next: TutorialPreference) => Promise.resolve({ ...next, scope: "operator" }),
    deleteSession: () => Promise.resolve(),
    deleteProject: () => Promise.resolve(),
    renameProject: () => Promise.resolve(),
    renameSession: () => Promise.resolve(),
    uploadPrd: () => Promise.resolve(),
    listProjects: () => Promise.resolve(projects),
    listProviders: () => Promise.resolve(["claude", "codex", "shell"]),
    listGitIdentities: () => Promise.resolve({ profiles: [], default_profile_id: null, revision: 0 }),
    createGitIdentity: () => Promise.reject(new ApiError(400, "bad_request", "n/a")),
    updateGitIdentity: () => Promise.reject(new ApiError(400, "bad_request", "n/a")),
    setDefaultGitIdentity: () => Promise.reject(new ApiError(400, "bad_request", "n/a")),
    deleteGitIdentity: () => Promise.resolve(),
    setProjectGitIdentity: () => Promise.reject(new ApiError(400, "bad_request", "n/a")),
    listProjectDirectories: () => Promise.resolve({ current_path: "", parent_path: null, directories: [] }),
    createProjectDirectory: () => Promise.resolve({ path: "/workspace/new" }),
    inspectProject: () => Promise.reject(new ApiError(400, "bad_request", "n/a")),
    startClone: () => Promise.reject(new ApiError(400, "bad_request", "n/a")),
    listClones: () => Promise.resolve([]),
    getClone: () => Promise.reject(new ApiError(404, "not_found", "n/a")),
    cancelClone: () => Promise.reject(new ApiError(404, "not_found", "n/a")),
    resumeClone: () => Promise.reject(new ApiError(404, "not_found", "n/a")),
    discardClone: () => Promise.resolve(),
    createProject: () => Promise.reject(new ApiError(400, "bad_request", "n/a")),
    listSessions: () => Promise.resolve([]),
    createSession: () => Promise.reject(new ApiError(400, "bad_request", "n/a")),
    initializeTaskmaster: () => Promise.resolve({ path: "/x/.git/noise-tasks/tasks.sqlite", tag: "main", already_initialized: false }),
    getTaskmasterInfo: () =>
      Promise.resolve({ path: "/x", config_path: "c", tasks_path: "t", docs_path: "d" }),
    listTaskmasterTasks: () => Promise.resolve(sampleTasks),
    addTaskmasterTask: () => Promise.resolve({ id: "1" }),
    getTaskmasterTask: (_projectID, taskID) => storedTask(sampleTasks.master!.tasks, taskID),
    listTaskmasterPRDs: () => Promise.resolve(["PRD.md", "OTHER.md"]),
    commitStarter: () => Promise.reject(new Error("unused")),
    generateTasks: () => Promise.resolve(tmRun()),
    listTaskmasterRuns: () => Promise.resolve([tmRun()]),
    getTaskmasterRun: (_p: string, id: string) => Promise.resolve(tmRun({ id })),
    getSession: () => Promise.reject(new ApiError(404, "not_found", "n/a")),
    ...overrides,
  };
}

test("a project with no task state is offered a board, not hidden", async () => {
  const m = new TaskmasterPanelModel(fakeApi({
    getTaskmasterInfo: () =>
      Promise.reject(new ApiError(404, "not_taskmaster_project", "no .taskmaster")),
  }), "p1");
  await m.refresh();
  assert.equal(m.state, "uninitialized");
  assert.equal(m.tags.length, 0);
});

test("a later not-project answer clears the last authoritative task view", async () => {
  let exists = true;
  const model = new TaskmasterPanelModel(fakeApi({
    getTaskmasterInfo: () => exists
      ? Promise.resolve({ path: "/x", config_path: "c", tasks_path: "t", docs_path: "d" })
      : Promise.reject(new ApiError(404, "not_taskmaster_project", "removed")),
  }), "p1");
  await model.refresh();
  assert.equal(model.tags.length, 2);
  exists = false;
  await model.refresh();
  assert.equal(model.state, "uninitialized");
  assert.equal(model.tags.length, 0, "a removed canonical backlog cannot leave a stale count");
});

test("a ready panel sorts master first and preselects the first PRD", async () => {
  const m = new TaskmasterPanelModel(fakeApi(), "p1");
  await m.refresh();
  assert.equal(m.state, "ready");
  assert.deepEqual(m.tags.map(([tag]) => tag), ["master", "other"]);
  assert.equal(m.tags[0]![1].length, 2);
  assert.equal(m.tags[0]![1][1]!.subtasks?.[0]!.title, "sub");
  assert.equal(m.selectedPrd, "PRD.md");
});

test("totalTasks counts every task and subtask across every tag", async () => {
  const m = new TaskmasterPanelModel(fakeApi(), "p1");
  await m.refresh();
  assert.equal(m.totalTasks(), 4);
});

test("totalTasks returns 0 when no tags are loaded", () => {
  const m = new TaskmasterPanelModel(fakeApi(), "p1");
  assert.equal(m.totalTasks(), 0);
});

// The engine response numbers subtasks RELATIVE to their parent, so the
// fixture's task "1" and task 2's subtask "1" are both literally id 1 —
// the collision the board and findTask both have to survive.
test("subtask ids are parent-qualified on read", async () => {
  const m = new TaskmasterPanelModel(fakeApi(), "p1");
  await m.refresh();
  const master = m.tags[0]![1];
  assert.equal(taskID(master[0]!.id), "1", "a top-level id is untouched");
  assert.equal(taskID(master[1]!.subtasks![0]!.id), "2.1");
});

test("no two board rows can share a rendered id", async () => {
  const m = new TaskmasterPanelModel(fakeApi(), "p1");
  await m.refresh();
  const ids = kanbanColumns(m.tags).flatMap(([, tasks]) => tasks.map((t) => taskID(t.id)));
  assert.equal(ids.length, 4, "every row is on the board");
  assert.equal(new Set(ids).size, ids.length, `duplicate ids: ${ids.join(", ")}`);
});

// Qualifying must be idempotent: a file that already stores the dotted
// form must not turn "1.1" into "1.1.1".
test("qualifying an already-dotted id is a no-op", () => {
  const once = qualifySubtaskIDs([
    { id: "1", title: "p", status: "pending", subtasks: [{ id: "1.1", title: "s", status: "pending" }] },
  ]);
  assert.equal(taskID(once[0]!.subtasks![0]!.id), "1.1");
  const twice = qualifySubtaskIDs(once);
  assert.equal(taskID(twice[0]!.subtasks![0]!.id), "1.1");
});

// The guard tests for the parent id FOLLOWED BY A DOT, not just the
// parent id: subtask 10 of parent 1 starts with "1", so a prefix-only
// test would leave it as "10" and collide with top-level task 10.
test("a subtask whose id merely starts with its parent's is still qualified", () => {
  const out = qualifySubtaskIDs([
    { id: "1", title: "p", status: "pending", subtasks: [{ id: "10", title: "s", status: "pending" }] },
    { id: "10", title: "other top-level", status: "pending" },
  ]);
  assert.equal(taskID(out[0]!.subtasks![0]!.id), "1.10");
  assert.notEqual(taskID(out[0]!.subtasks![0]!.id), taskID(out[1]!.id));
});

test("qualifying reaches sub-subtasks and leaves the input untouched", () => {
  const input = [{
    id: 1, title: "p", status: "pending",
    subtasks: [{ id: 2, title: "s", status: "pending", subtasks: [{ id: 3, title: "ss", status: "pending" }] }],
  }];
  const out = qualifySubtaskIDs(input);
  assert.equal(taskID(out[0]!.subtasks![0]!.subtasks![0]!.id), "1.2.3");
  assert.equal(input[0]!.subtasks![0]!.id, 2, "the caller's own tasks are not mutated");
});

test("subtask dependencies are qualified with the parent prefix", () => {
  const out = qualifySubtaskIDs([
    { id: "30", title: "p", status: "pending", subtasks: [{ id: "1", title: "s", status: "pending", dependencies: ["2"] }] },
  ]);
  assert.deepEqual(out[0]!.subtasks![0]!.dependencies, ["30.2"]);
});

test("subtask deps are qualified even when they match a top-level id", () => {
  const out = qualifySubtaskIDs([
    { id: "30", title: "p", status: "pending", subtasks: [{ id: "1", title: "s", status: "pending", dependencies: ["7"] }] },
    { id: "7", title: "top-level ref", status: "pending" },
  ]);
  assert.deepEqual(out[0]!.subtasks![0]!.dependencies, ["30.7"]);
});

test("subtask deps are qualified even when colliding with the parent id", () => {
  const out = qualifySubtaskIDs([
    { id: "1", title: "p", status: "pending", subtasks: [{ id: "2", title: "s", status: "pending", dependencies: ["1"] }] },
  ]);
  assert.deepEqual(out[0]!.subtasks![0]!.dependencies, ["1.1"]);
});

test("a fetched subtask is qualified under the id it was requested by", async () => {
  const m = new TaskmasterPanelModel(fakeApi(), "p1");
  assert.equal(taskID((await m.getTask("2.1")).id), "2.1");
  const raw = new TaskmasterPanelModel(fakeApi({
    getTaskmasterTask: () => Promise.resolve({
      id: 1, parentId: "2", title: "s", status: "pending", dependencies: [3],
      subtasks: [{ id: 1, title: "ss", status: "pending" }],
    }),
  }), "p1");
  const sub = await raw.getTask("2.1");
  assert.equal(taskID(sub.id), "2.1");
  assert.deepEqual(sub.dependencies, ["2.3"]);
  assert.equal(taskID(sub.subtasks![0]!.id), "2.1.1");
});

test("a fetched top-level task with a dotted id keeps its id and deps", async () => {
  const tasks: TmTask[] = [{ id: "30.4", title: "dotted", status: "pending", dependencies: ["2", "3"] }];
  const m = new TaskmasterPanelModel(fakeApi({ getTaskmasterTask: (_p, id) => storedTask(tasks, id) }), "p1");
  const task = await m.getTask("30.4");
  assert.equal(taskID(task.id), "30.4");
  assert.deepEqual(task.dependencies, ["2", "3"]);
});

test("a fetched nested subtask is qualified under its full parent path", async () => {
  const tasks: TmTask[] = [{
    id: "2", title: "t", status: "pending",
    subtasks: [{
      id: 1, title: "s", status: "pending",
      subtasks: [{ id: 3, title: "ss", status: "pending", dependencies: [2] }],
    }],
  }];
  const m = new TaskmasterPanelModel(fakeApi({ getTaskmasterTask: (_p, id) => storedTask(tasks, id) }), "p1");
  const task = await m.getTask("2.1.3");
  assert.equal(taskID(task.id), "2.1.3");
  assert.deepEqual(task.dependencies, ["2.1.2"]);
});

test("a detection failure other than not-taskmaster stays ready with the error inline", async () => {
  const m = new TaskmasterPanelModel(fakeApi({
    getTaskmasterInfo: () => Promise.reject(new ApiError(500, "internal", "db gone")),
  }), "p1");
  await m.refresh();
  assert.equal(m.state, "ready");
  assert.equal(m.tasksError, "db gone");
});

test("generate tracks the run and reloads tasks when it exits", async () => {
  let taskReads = 0;
  const edited: TaskmasterTasks = {
    master: { tasks: [{ id: "1", title: "regenerated", status: "pending" }] },
  };
  const api = fakeApi({
    listTaskmasterTasks: () => {
      taskReads++;
      return Promise.resolve(taskReads < 2 ? sampleTasks : edited);
    },
    generateTasks: () => Promise.resolve(tmRun({ id: "job-1" })),
    getTaskmasterRun: () =>
      Promise.resolve(tmRun({
        id: "job-1",
        status: "exited",
        exit_code: 0,
        finished_at: "2026-01-01T00:00:01.500Z",
        duration_ms: 1500,
      })),
  });
  const m = new TaskmasterPanelModel(api, "p1");
  await m.refresh();
  await m.generate();
  assert.equal(m.activeRun?.id, "job-1");
  assert.equal(m.activeRun?.status, "running");
  // No second start while one is in flight.
  await m.generate();
  await m.pollRun();
  assert.equal(m.activeRun?.status, "exited");
  // The authoritative tasks were reloaded after completion.
  assert.equal(m.tags[0]![1][0]!.title, "regenerated");
  assert.equal(m.runSummary(m.activeRun!), "exited 0 · 1.5s");
});

test("a 409 attaches to the in-flight run instead of erroring", async () => {
  const m = new TaskmasterPanelModel(fakeApi({
    generateTasks: () =>
      Promise.reject(new ApiError(409, "generation_running", "one already")),
    listTaskmasterRuns: () => Promise.resolve([tmRun({ id: "the-running-one" })]),
  }), "p1");
  await m.refresh();
  await m.generate();
  assert.equal(m.generateError, null);
  assert.equal(m.activeRun?.id, "the-running-one");
});

test("generation failures surface inline", async () => {
  const m = new TaskmasterPanelModel(fakeApi({
    generateTasks: () => Promise.reject(new ApiError(400, "invalid_prd", "pick another")),
  }), "p1");
  await m.refresh();
  await m.generate();
  assert.equal(m.generateError, "pick another");
  assert.equal(m.activeRun, null);
});

test("concurrent generate clicks issue a single POST", async () => {
  let posts = 0;
  let release!: (run: TmRun) => void;
  const gate = new Promise<TmRun>((resolve) => { release = resolve; });
  const m = new TaskmasterPanelModel(fakeApi({
    generateTasks: () => {
      posts++;
      return gate;
    },
  }), "p1");
  await m.refresh();
  const first = m.generate();
  const second = m.generate();
  const third = m.generate();
  release(tmRun());
  await Promise.all([first, second, third]);
  assert.equal(posts, 1);
  assert.equal(m.generating, false);
});

test("pollActive is the single polling decision", async () => {
  const m = new TaskmasterPanelModel(fakeApi(), "p1");
  // hidden: never
  assert.equal(m.pollActive(), false);
  await m.refresh();
  // ready but no run: no
  assert.equal(m.pollActive(), false);
  await m.generate();
  // ready, run in flight: yes
  assert.equal(m.pollActive(), true);
  // run over: no
  m.activeRun = tmRun({ status: "exited", exit_code: 0, finished_at: "f" });
  assert.equal(m.pollActive(), false);
});

test("an instantly-exited run reloads tasks without polling", async () => {
  let taskReads = 0;
  const m = new TaskmasterPanelModel(fakeApi({
    listTaskmasterTasks: () => {
      taskReads++;
      return Promise.resolve(sampleTasks);
    },
    generateTasks: () =>
      Promise.resolve(tmRun({ status: "exited", exit_code: 3, finished_at: "f", duration_ms: 12 })),
  }), "p1");
  await m.refresh();
  const before = taskReads;
  await m.generate();
  assert.ok(taskReads > before, "tasks reloaded after the instant exit");
  assert.equal(m.runSummary(m.activeRun!), "exited 3 · 0.0s");
});

test("runSummary is honest about unobserved exits", () => {
  const m = new TaskmasterPanelModel(fakeApi(), "p1");
  assert.equal(m.runSummary(tmRun()), "running");
  assert.equal(
    m.runSummary(tmRun({ status: "exited", finished_at: "f" })),
    "exit unobserved",
  );
});

test("presentation helpers", () => {
  assert.equal(taskID(12), "12");
  assert.equal(taskID("24.1"), "24.1");
  assert.equal(taskID({ odd: true }), "?");
  assert.equal(depsDisplay(["1", 2]), "1, 2");
  assert.equal(depsDisplay([]), "");
  assert.equal(depsDisplay(undefined), "");
});


test("overlapping polls are skipped and stale responses cannot regress state", async () => {
  let fetches = 0;
  let resolveFirst!: (run: TmRun) => void;
  const first = new Promise<TmRun>((resolve) => { resolveFirst = resolve; });
  const api = fakeApi({
    generateTasks: () => Promise.resolve(tmRun({ id: "job-1" })),
    getTaskmasterRun: () => {
      fetches++;
      return fetches === 1 ? first : Promise.resolve(tmRun({ id: "job-1", status: "exited", exit_code: 0, finished_at: "f" }));
    },
  });
  const m = new TaskmasterPanelModel(api, "p1");
  await m.refresh();
  await m.generate();

  const p1 = m.pollRun();
  const p2 = m.pollRun(); // in flight: must not issue a second fetch
  await p2;
  assert.equal(fetches, 1);
  resolveFirst(tmRun({ id: "job-1" })); // still running
  await p1;
  assert.equal(m.activeRun?.status, "running");

  // The next poll completes normally.
  await m.pollRun();
  assert.equal(fetches, 2);
  assert.equal(m.activeRun?.status, "exited");
});

test("a newer task read suppresses an older response", async () => {
  let firstResolve!: (tasks: TaskmasterTasks) => void;
  let secondResolve!: (tasks: TaskmasterTasks) => void;
  let reads = 0;
  const first = new Promise<TaskmasterTasks>((resolve) => { firstResolve = resolve; });
  const second = new Promise<TaskmasterTasks>((resolve) => { secondResolve = resolve; });
  const model = new TaskmasterPanelModel(fakeApi({
    listTaskmasterTasks: () => ++reads === 1 ? first : second,
  }), "p1");

  const older = model.refreshTasks();
  const newer = model.refreshTasks();
  secondResolve({ master: { tasks: [{ id: "2", title: "new", status: "pending" }] } });
  await newer;
  firstResolve({ master: { tasks: [{ id: "1", title: "old", status: "pending" }] } });
  await older;

  assert.equal(model.tags[0]![1][0]!.title, "new");
});

test("uploadPrd stores under the canonical name, refreshes, and selects; failures surface in-band", async () => {
  const calls: Array<[string, string, boolean]> = [];
  const m = new TaskmasterPanelModel(fakeApi({
    listTaskmasterPRDs: () => Promise.resolve([PRD_CANONICAL_NAME]),
    uploadPrd: (_id: string, name: string, content: string, replace: boolean) => {
      calls.push([name, content, replace]);
      return Promise.resolve();
    },
  }), "p1");
  await m.refresh();
  assert.equal(await m.uploadPrd("# PRD", false), true);
  assert.deepEqual(calls, [[PRD_CANONICAL_NAME, "# PRD", false]]);
  assert.equal(m.selectedPrd, PRD_CANONICAL_NAME);
  assert.equal(m.hasCanonicalPrd(), true);

  const bad = new TaskmasterPanelModel(fakeApi({
    uploadPrd: () => Promise.reject(new ApiError(409, "prd_exists", "already there")),
  }), "p1");
  await bad.refresh();
  assert.equal(await bad.uploadPrd("y", false), false);
  assert.equal(bad.generateError, "already there");
});

for (const staleOutcome of ["empty", "error"] as const) {
  test(`a pre-upload ${staleOutcome} listing cannot clear the confirmed PRD`, { timeout: 2000 }, async () => {
    let finishOld!: (names: string[]) => void;
    let failOld!: (error: Error) => void;
    const oldListing = new Promise<string[]>((resolve, reject) => {
      finishOld = resolve;
      failOld = reject;
    });
    let finishFresh!: (names: string[]) => void;
    const freshListing = new Promise<string[]>((resolve) => { finishFresh = resolve; });
    let signalFresh!: () => void;
    const freshStarted = new Promise<void>((resolve) => { signalFresh = resolve; });
    let reads = 0;
    const errors: string[] = [];
    const generated: string[] = [];
    const model = new TaskmasterPanelModel(fakeApi({
      listTaskmasterPRDs: () => {
        if (++reads === 1) return oldListing;
        signalFresh();
        return freshListing;
      },
      generateTasks: (_project, prd) => {
        generated.push(prd);
        return Promise.resolve(tmRun());
      },
    }), "p1", { onError: (message) => errors.push(message) });
    const stale = model.refreshPrds();
    const upload = model.uploadPrd("# New PRD", false);
    await freshStarted;
    const poll = model.refreshPrds();
    finishFresh([PRD_CANONICAL_NAME]);
    assert.equal(await upload, true);
    await poll;
    assert.equal(reads, 2, "polling must share the post-upload listing");
    if (staleOutcome === "empty") finishOld([]);
    else failOld(new Error("old listing failed"));
    await stale;
    assert.deepEqual(model.prds, [PRD_CANONICAL_NAME]);
    assert.equal(model.selectedPrd, PRD_CANONICAL_NAME);
    assert.deepEqual(errors, []);
    await model.generate();
    assert.deepEqual(generated, [PRD_CANONICAL_NAME]);
  });
}

test("upload waits for its PRD read when polling supersedes project detection", { timeout: 2000 }, async () => {
  const info = { path: "/x", config_path: "c", tasks_path: "t", docs_path: "d" };
  let finishUploadInfo!: (value: typeof info) => void;
  const uploadInfo = new Promise<typeof info>((resolve) => { finishUploadInfo = resolve; });
  let finishPollInfo!: (value: typeof info) => void;
  const pollInfo = new Promise<typeof info>((resolve) => { finishPollInfo = resolve; });
  let signalInfo!: () => void;
  const infoStarted = new Promise<void>((resolve) => { signalInfo = resolve; });
  let finishListing!: (names: string[]) => void;
  const listing = new Promise<string[]>((resolve) => { finishListing = resolve; });
  let detections = 0;
  const model = new TaskmasterPanelModel(fakeApi({
    getTaskmasterInfo: () => {
      signalInfo();
      return ++detections === 1 ? uploadInfo : pollInfo;
    },
    listTaskmasterPRDs: () => listing,
  }), "p1");
  let uploadFinished = false;
  const upload = model.uploadPrd("# New PRD", false).then((ok) => {
    uploadFinished = true;
    return ok;
  });
  await infoStarted;
  const poll = model.refresh();
  finishUploadInfo(info);
  await new Promise<void>((resolve) => setImmediate(resolve));
  assert.equal(uploadFinished, false, "generation must wait for the post-upload listing");
  finishListing([PRD_CANONICAL_NAME]);
  assert.equal(await upload, true);
  assert.equal(model.selectedPrd, PRD_CANONICAL_NAME);
  finishPollInfo(info);
  await poll;
  assert.equal(model.selectedPrd, PRD_CANONICAL_NAME);
});

test("hasCanonicalPrd is the add-PRD flow's collision check", async () => {
  const withPrd = new TaskmasterPanelModel(fakeApi({
    listTaskmasterPRDs: () => Promise.resolve([PRD_CANONICAL_NAME, "other.md"]),
  }), "p1");
  await withPrd.refresh();
  assert.equal(withPrd.hasCanonicalPrd(), true);

  const without = new TaskmasterPanelModel(fakeApi({
    listTaskmasterPRDs: () => Promise.resolve(["other.md"]),
  }), "p1");
  await without.refresh();
  assert.equal(without.hasCanonicalPrd(), false);
});

test("uploadPrd forwards the replace flag for a confirmed collision", async () => {
  const calls: Array<boolean> = [];
  const m = new TaskmasterPanelModel(fakeApi({
    listTaskmasterPRDs: () => Promise.resolve([PRD_CANONICAL_NAME]),
    uploadPrd: (_id: string, _name: string, _content: string, replace: boolean) => {
      calls.push(replace);
      return Promise.resolve();
    },
  }), "p1");
  await m.refresh();
  assert.equal(await m.uploadPrd("# new", true), true);
  assert.deepEqual(calls, [true]);
});

// --- Board toolbar logic (design pass three) ---

test("normalizedPriority folds med/medium and maps absence to none", () => {
  assert.equal(normalizedPriority("high"), "high");
  assert.equal(normalizedPriority("med"), "medium");
  assert.equal(normalizedPriority("medium"), "medium");
  assert.equal(normalizedPriority("low"), "low");
  assert.equal(normalizedPriority(undefined), "none");
  assert.equal(normalizedPriority("weird"), "none");
});

test("priorityRank orders high, medium, low, none", () => {
  assert.ok(priorityRank("high") < priorityRank("med"));
  assert.ok(priorityRank("medium") < priorityRank("low"));
  assert.ok(priorityRank("low") < priorityRank(undefined));
});

test("compareTaskIDs: integers numerically, dotted ids by segment", () => {
  assert.ok(compareTaskIDs("9", "10") < 0, "9 before 10");
  assert.ok(compareTaskIDs("1.9", "1.10") < 0, "subtask 1.9 before 1.10");
  assert.ok(compareTaskIDs(2, "2") === 0);
  assert.ok(compareTaskIDs("1.2", "1") > 0, "subtask after its parent prefix");
});

// Note the fixture's shape: the blocked task's id (9) is HIGHER than
// pending ids, so the status sort's answer differs from the id sort's —
// a fixture where they coincide cannot pin the grouping.
const boardFixture = [
  { id: "10", title: "Wire the relay", status: "pending", priority: "low" },
  { id: "2", title: "Fix auth token", status: "pending", priority: "high" },
  { id: "9", title: "Polish sidebar", status: "blocked", priority: "med" },
  { id: "3", title: "Ship the board", status: "pending" },
];

test("boardTasks searches title and id, case-insensitive", () => {
  const byTitle = boardTasks(boardFixture, "AUTH", null, "id");
  assert.deepEqual(byTitle.map((t) => t.id), ["2"]);
  const byID = boardTasks(boardFixture, "10", null, "id");
  assert.deepEqual(byID.map((t) => t.id), ["10"]);
  assert.equal(boardTasks(boardFixture, "  ", null, "id").length, 4, "blank query filters nothing");
});

test("boardTasks priority filter uses normalized keys", () => {
  const shown = boardTasks(boardFixture, "", new Set(["medium", "none"]), "id");
  assert.deepEqual(shown.map((t) => t.id), ["3", "9"], "med matches medium; missing priority matches none");
});

test("boardTasks sorts: id numerically, priority by rank, status by label", () => {
  assert.deepEqual(boardTasks(boardFixture, "", null, "id").map((t) => t.id), ["2", "3", "9", "10"]);
  assert.deepEqual(
    boardTasks(boardFixture, "", null, "priority").map((t) => t.id),
    ["2", "9", "10", "3"],
    "high, med, low, none — id breaks ties",
  );
  assert.deepEqual(
    boardTasks(boardFixture, "", null, "status").map((t) => t.id),
    ["9", "2", "3", "10"],
    "blocked groups before pending despite its higher id; id breaks ties within a status",
  );
});

test("boardTasks desc flips the primary direction and keeps the id tiebreak ascending", () => {
  assert.deepEqual(boardTasks(boardFixture, "", null, "id", true).map((t) => t.id), ["10", "9", "3", "2"]);
  assert.deepEqual(
    boardTasks(boardFixture, "", null, "priority", true).map((t) => t.id),
    ["3", "10", "9", "2"],
    "none, low, med, high — descending rank",
  );
  assert.deepEqual(
    boardTasks(boardFixture, "", null, "status", true).map((t) => t.id),
    ["2", "3", "10", "9"],
    "pending before blocked, id ascending within the group",
  );
});

// --- The upload→parse failure path ---
// The Generate pill is gone, so upload→parse is the ONLY generation
// path: a failed parse must keep the Add-PRD modal open with the
// model's error — not silently close over a blank board.

async function panelAfterUpload(
  generateTasks: () => Promise<TmRun>,
  cb: TaskmasterPanelShellCallbacks = {},
) {
  const api = fakeApi({
    listTaskmasterPRDs: () => Promise.resolve([PRD_CANONICAL_NAME]),
    generateTasks,
  });
  const model = new TaskmasterPanelModel(api, "p1");
  await model.refresh();
  const box = fakeContainer();
  const panel = new TaskmasterPanel(box.el, model, cb);
  const p = panel as unknown as {
    addPrdOpen: boolean;
    pickedFile: { name: string; size: number; content: string } | null;
    performUpload(replace: boolean): Promise<void>;
  };
  p.addPrdOpen = true;
  p.pickedFile = { name: "spec.md", size: 4, content: "# p\n" };
  await p.performUpload(true);
  return { model, box, p };
}

test("a failed parse keeps the modal open and the error visible", async () => {
  const { model, box, p } = await panelAfterUpload(() =>
    Promise.reject(new ApiError(500, "parse_failed", "task-master exited 1")),
  );
  assert.equal(model.generateError, "task-master exited 1", "the error must survive");
  assert.equal(p.addPrdOpen, true, "the modal must stay open");
  assert.ok(box.html().includes("data-tm-add-prd-modal"), "the modal markup renders");
  assert.ok(box.html().includes("task-master exited 1"), "the error renders inside it");
});

test("a successful parse closes the modal and clears the flow", async () => {
  const { model, box, p } = await panelAfterUpload(() =>
    Promise.resolve(tmRun({ status: "exited", exit_code: 0 })),
  );
  assert.equal(model.generateError, null);
  assert.equal(p.addPrdOpen, false, "the modal closes");
  assert.equal(p.pickedFile, null, "the picked file resets");
  assert.ok(!box.html().includes("data-tm-add-prd-modal"));
});

test("priorityLabel: normalized names for the known scale, raw out-of-schema values verbatim", () => {
  assert.equal(priorityLabel("med"), "medium");
  assert.equal(priorityLabel("high"), "high");
  assert.equal(priorityLabel("critical"), "critical");
});

test("generate() with no PRD sets an error instead of silently returning", async () => {
  const model = new TaskmasterPanelModel(fakeApi({
    listTaskmasterPRDs: () => Promise.resolve([]),
  }), "p1");
  await model.refresh();
  assert.equal(model.selectedPrd, null);
  await model.generate();
  assert.ok(model.generateError !== null, "the silent path must surface an error");
});

test("an upload whose PRD never lists keeps the modal open with an error", async () => {
  // The residual case: uploadPrd succeeds but the post-upload listing
  // misses prd.md, so generate() has nothing to parse. The old silent
  // return left generateError null and the shell closed the modal over
  // an empty board.
  const api = fakeApi({
    listTaskmasterPRDs: () => Promise.resolve([]),
    generateTasks: () => {
      throw new Error("generateTasks must not be reached without a PRD");
    },
  });
  const model = new TaskmasterPanelModel(api, "p1");
  await model.refresh();
  const box = fakeContainer();
  const panel = new TaskmasterPanel(box.el, model);
  const p = panel as unknown as {
    addPrdOpen: boolean;
    pickedFile: { name: string; size: number; content: string } | null;
    performUpload(replace: boolean): Promise<void>;
  };
  p.addPrdOpen = true;
  p.pickedFile = { name: "spec.md", size: 4, content: "# p\n" };
  await p.performUpload(true);
  assert.equal(p.addPrdOpen, true, "the modal must stay open");
  assert.ok(box.html().includes("data-tm-add-prd-modal"));
  assert.ok(box.html().includes("the upload did not register"));
});

// --- The docked board's markup ---
//
// There is no jsdom here, so the shell's markup is read back off
// innerHTML and its RESOLVED geometry is pinned by the browser probe
// (tools/tasks-panel-probe.mjs). What these tests own is the set of
// declarations the probe measures: a class rewrite that quietly reboxes a
// column or reinstates a card footer fails here, in milliseconds, rather
// than in a browser run.

interface FakeContainer {
  el: HTMLElement;
  html: () => string;
  writes: () => number;
  appends: () => string[];
  click: (selector: string) => void;
}

// A stub with just enough DOM to bind and fire a click. Selectors in this
// shell are all single attribute selectors, so "does the current markup
// carry this attribute" is a faithful stand-in for matching, and handlers
// are keyed by selector rather than by node — a re-render replaces the
// nodes but not the question being asked. Dialogs render into a separate
// host element (created with document.createElement) so they escape the
// docked panel's stacking context; the stub tracks both surfaces and lets
// html() read them together. The fake default mounts the container inside
// a workspace that owns the app-level [data-region="modals"] host, so the
// dialogs exercise the real mount path; pass false to model a workspace
// without that host and force the container fallback.
function fakeContainer(withModalsHost = true): FakeContainer {
  const handlers = new Map<string, Array<(ev: unknown) => void>>();
  const surfaces: string[] = [""];
  const appends: string[] = [];
  let writes = 0;
  const attribute = (selector: string): string =>
    selector.replace(/^\[|\]$/g, "").split("=")[0]!;
  const combined = (): string => surfaces.join("");
  const node = (selector: string): unknown => {
    const attr = attribute(selector);
    const sortKeyMatch = /^\[data-tm-sort="([^"]+)"\]$/.exec(selector);
    if (sortKeyMatch !== null) {
      // A per-key sort stub: the selector value is the only differentiator,
      // and the same stub must be re-queryable after a rerender.
      const key = sortKeyMatch[1]!;
      if (!combined().includes(`data-tm-sort="${key}"`)) return null;
      return {
        dataset: { tmSort: key } as Record<string, string>,
        classList: { toggle: () => {} },
        addEventListener(type: string, fn: (ev: unknown) => void) {
          if (type !== "click") return;
          const list = handlers.get(selector) ?? [];
          list.push(fn);
          handlers.set(selector, list);
        },
        click() {
          const list = handlers.get(selector) ?? [];
          assert.ok(list.length > 0, `nothing is bound to ${selector}`);
          for (const fn of list) fn({ target: null, stopPropagation: () => {} });
        },
      };
    }
    if (!combined().includes(attr)) return null;
    // A value selector like [data-tm-add-task="header"] seeds dataset, so
    // binders that read the flag off the node see the rendered value.
    const valueMatch = /^\[([\w-]+)="([^"]*)"\]$/.exec(selector);
    const camel = (name: string): string => name.replace(/-(\w)/g, (_, c: string) => c.toUpperCase());
    const seeded: Record<string, string> = valueMatch === null ? {} : { [camel(valueMatch[1]!)]: valueMatch[2]! };
    return {
      dataset: seeded as Record<string, string>,
      classList: { toggle: () => {} },
      addEventListener(type: string, fn: (ev: unknown) => void) {
        if (type !== "click") return;
        const list = handlers.get(selector) ?? [];
        list.push(fn);
        handlers.set(selector, list);
      },
      click() {
        // A bare-attribute click fires every handler registered for that
        // attribute, whatever value each rendered node carried.
        const isBare = /^\[[\w-]+\]$/.test(selector);
        const list = isBare
          ? [...handlers.entries()].filter(([k]) => attribute(k) === selector.slice(1, -1)).flatMap(([, fns]) => fns)
          : handlers.get(selector) ?? [];
        assert.ok(list.length > 0, `nothing is bound to ${selector}`);
        for (const fn of list) fn({ target: null, stopPropagation: () => {} });
      },
    };
  };
  const surface = (index: number): HTMLElement => {
    let connected = true;
    const el: Record<string, unknown> = {
      hidden: false,
      get isConnected(): boolean {
        return connected;
      },
      className: "",
      dataset: {} as Record<string, string>,
      get innerHTML(): string {
        return surfaces[index]!;
      },
      set innerHTML(next: string) {
        writes++;
        surfaces[index] = next;
        if (index === 0) handlers.clear();
      },
      querySelector: (selector: string) => node(selector),
      querySelectorAll: (selector: string) => {
        // A bare attribute selector enumerates every rendered value, so a
        // binder like the sort pills' "[data-tm-sort]" reaches each key.
        if (/^\[data-[\w-]+\]$/.test(selector)) {
          const attr = selector.slice(1, -1);
          const values = new Set<string>();
          for (const m of combined().matchAll(new RegExp(`${attr}="([^"]*)"`, "g"))) values.add(m[1]!);
          return [...values].map((value) => node(`[${attr}="${value}"]`)).filter((n) => n !== null);
        }
        const hit = node(selector);
        return hit === null ? [] : [hit];
      },
      append: () => {
        appends.push(index === 0 ? "container" : "surface");
      },
      addEventListener: () => {},
      contains: () => false,
      remove() {
        connected = false;
        surfaces[index] = "";
        writes++;
      },
    };
    return el as unknown as HTMLElement;
  };
  const box = surface(0);
  const modalsRegion = {
    append: () => {
      appends.push("modals");
    },
  };
  (box as unknown as { closest: (selector: string) => unknown }).closest = (selector: string) => {
    if (withModalsHost && selector === "[data-workspace]") {
      return {
        querySelector: (nested: string): unknown =>
          nested === '[data-region="modals"]' ? modalsRegion : null,
      };
    }
    return null;
  };
  (globalThis as unknown as { document: unknown }).document = {
    addEventListener: () => {},
    removeEventListener: () => {},
    createElement: () => {
      surfaces.push("");
      return surface(surfaces.length - 1);
    },
  };
  return {
    el: box,
    html: () => combined(),
    writes: () => writes,
    appends: () => [...appends],
    click: (selector: string) => {
      // A bare-attribute click fans out to every handler registered under
      // that attribute, whatever value each rendered node carried.
      const isBare = /^\[[\w-]+\]$/.test(selector);
      const list = isBare
        ? [...handlers.entries()].filter(([k]) => attribute(k) === selector.slice(1, -1)).flatMap(([, fns]) => fns)
        : handlers.get(selector) ?? [];
      assert.ok(list.length > 0, `nothing is bound to ${selector}`);
      for (const fn of list) fn({ target: null, stopPropagation: () => {} });
    },
  };
}

interface FakeTimer extends TaskmasterPanelTimer {
  pending(): number;
  nextDelay(): number | null;
  fire(): Promise<boolean>;
}

function fakeTimer(): FakeTimer {
  let nextID = 1;
  const callbacks = new Map<number, {
    callback: () => void | Promise<void>;
    delayMS: number;
  }>();
  return {
    set(callback, delayMS) {
      const id = nextID++;
      callbacks.set(id, { callback, delayMS });
      return id;
    },
    clear(id) {
      callbacks.delete(id);
    },
    pending: () => callbacks.size,
    nextDelay: () => callbacks.values().next().value?.delayMS ?? null,
    async fire() {
      const next = callbacks.entries().next();
      if (next.done) return false;
      const [id, scheduled] = next.value;
      callbacks.delete(id);
      await scheduled.callback();
      return true;
    },
  };
}

async function readyPanel(
  overrides: Partial<ApiClient> = {},
  cb: TaskmasterPanelShellCallbacks = {},
) {
  const model = new TaskmasterPanelModel(fakeApi(overrides), "p1");
  const box = fakeContainer();
  const panel = new TaskmasterPanel(box.el, model, cb);
  await panel.refresh();
  return { model, box, panel };
}

test("the panel header shares the chrome bar and carries no fill", async () => {
  const { box } = await readyPanel();
  const header = /<div data-tm-header class="([^"]*chrome[^"]*)"/.exec(box.html());
  assert.ok(header !== null, box.html());
  const classes = header[1]!;
  assert.equal(/bg-surface-muted|bg-field\b/.test(classes), false, "no muted fill: " + classes);
  assert.match(box.html(), /<h2 class="[^"]*chrome-title[^"]*">Tasks<\/h2>/);
});

test("the header exposes working task and PRD entry points plus a named close", async () => {
  const { box } = await readyPanel();
  const html = box.html();
  assert.ok(html.includes('aria-label="Close tasks"'), "the close control is named");
  assert.match(html, /data-tm-close[^>]*class="[^"]*h-7 w-7/, "28px icon button");
  assert.match(html, /data-tm-close[^>]*class="[^"]*max-md:h-11 max-md:w-11/, "44px mobile target");
  assert.match(html, /data-tm-add-prd="header"[^>]*class="[^"]*max-md:h-11/, "44px mobile target");
  assert.match(html, /data-tm-add-prd="header"[^>]*class="[^"]*h-7/, "28px desktop pill");
  assert.match(html, /data-tm-add-task="header"[^>]*class="[^"]*max-md:h-11/, "44px mobile target");
  assert.ok(html.includes(">Add PRD…<"));
  assert.ok(html.includes(">Add task<"));
  assert.equal(/disabled/.test(html), false, "no dead affordances on the board");
  assert.ok(html.includes("focus-visible:bg-wash-hover"), "keyboard focus takes the hover wash, never a ring");
});

test("addTask reports success only after the returned id appears in a canonical refresh", async () => {
  let reads = 0;
  const model = new TaskmasterPanelModel(fakeApi({
    addTaskmasterTask: (_projectID, input) => {
      assert.deepEqual(input, { title: "hostile; $(text)", description: "d", details: "x", priority: "high" });
      return Promise.resolve({ id: "10" });
    },
    listTaskmasterTasks: () => Promise.resolve(reads++ === 0 ? sampleTasks : {
      master: { tasks: [...sampleTasks.master!.tasks, { id: "10", title: "hostile; $(text)", status: "pending" }] },
    }),
  }), "p1");
  await model.refresh();
  assert.equal(await model.addTask({ title: "hostile; $(text)", description: "d", details: "x", priority: "high" }), true);
  assert.equal(model.addError, null);
});

test("addTask keeps an honest error when refresh does not contain the returned id", async () => {
  const model = new TaskmasterPanelModel(fakeApi({
    addTaskmasterTask: () => Promise.resolve({ id: "404" }),
  }), "p1");
  await model.refresh();
  assert.equal(await model.addTask({ title: "missing", description: "", details: "", priority: "low" }), false);
  assert.match(model.addError ?? "", /404 was not present after refresh/);
});

test("concurrent addTask calls issue one mutation", async () => {
  let calls = 0;
  let release!: (value: { id: string }) => void;
  const pending = new Promise<{ id: string }>((resolve) => { release = resolve; });
  const model = new TaskmasterPanelModel(fakeApi({
    addTaskmasterTask: () => { calls++; return pending; },
  }), "p1");
  const input = { title: "one", description: "", details: "", priority: "medium" as const };
  const first = model.addTask(input);
  assert.equal(await model.addTask(input), false);
  assert.equal(calls, 1);
  release({ id: "1" });
  assert.equal(await first, true);
});

test("the Add task dialog is labelled, focuses its required title, and has mobile-safe controls", async () => {
  const { box } = await readyPanel();
  box.click("[data-tm-add-task]");
  const html = box.html();
  assert.match(html, /role="dialog" aria-modal="true" aria-label="Add task"/);
  assert.match(html, /data-tm-task-title required/);
  assert.match(html, /data-tm-task-priority/);
  assert.match(html, /<button type="submit" data-tm-task-add-submit\s+[^>]*data-hint="Add a task to this board"/);
  assert.match(html, /pointer-coarse:min-h-11/);
});

test("the close control reports to the workspace rather than hiding itself", async () => {
  let closes = 0;
  const { box } = await readyPanel({}, { onClose: () => closes++ });
  box.click("[data-tm-close]");
  assert.equal(closes, 1);
  // The panel does not touch its own frame: the coordinator owns which
  // panel is laid out, and nothing here listens for a click elsewhere.
  assert.ok(box.html().includes("Tasks"), "closing is the workspace's move to make");
});

test("the toolbar wraps and the search field is capped at 420px", async () => {
  const { box } = await readyPanel();
  assert.match(box.html(), /<div data-tm-toolbar class=/);
  const toolbar = /<div data-tm-toolbar class="(flex flex-none flex-wrap[^"]*)"/.exec(box.html());
  assert.ok(toolbar !== null, box.html());
  const wrap = /<label class="([^"]*max-w-\[420px\][^"]*)"/.exec(box.html());
  assert.ok(wrap !== null, "the search field caps at 420px");
  const wrapClasses = wrap[1]!;
  for (const recipe of ["inputwrap", "flex-1", "max-w-[420px]"]) {
    assert.ok(wrapClasses.includes(recipe), `${recipe} missing from ${wrapClasses}`);
  }
  const input = /<input data-tm-search[^>]*class="([^"]*)"/.exec(box.html());
  assert.ok(input !== null, box.html());
  const inputClasses = input[1]!;
  for (const recipe of ["input", "input-sm", "input-lead", "leading-[normal]"]) {
    assert.ok(inputClasses.includes(recipe), `${recipe} missing from ${inputClasses}`);
  }
  assert.ok(box.html().includes('aria-label="Search tasks"'));
  assert.ok(box.html().includes('data-hint="Search tasks by title or id"'), "the field leads with its verb");
  assert.match(box.html(), /<span data-tm-sort-caption class="text-caption leading-\[1\.55\] text-text-tertiary">Sort<\/span>/,
    "the Sort label rides the 1.55 body ratio, not the caption token's 1.4");
  for (const label of ["Status", "Priority"]) {
    assert.ok(box.html().includes(`>${label}</button>`), `the ${label} segment`);
  }
  const idPill = /data-tm-sort="id"[^>]*aria-pressed="true"[^>]*tab-selected/.exec(box.html());
  assert.ok(idPill !== null, "ID is the active sort by default and takes the selected wash");
  assert.match(box.html(), /data-tm-sort="id"[^>]*aria-pressed="true"[^>]*tab-selected[^>]*>ID ↑<\/button>/, "the active pill leads with the ascending text arrow");
  assert.doesNotMatch(box.html(), /<button[^>]*data-tm-sort[^>]*>[^<]*<svg/, "sort pills use text arrows, not icons");
  assert.match(box.html(), /data-tm-filters [^>]*aria-haspopup="true"/);
  assert.match(box.html(), /data-tm-filters[\s\S]*?<svg aria-hidden="true" width="14" height="14"/,
    "the Filter chevron is 14px, the size the panel triggers carry");
});

test("re-selecting the active sort pill flips its direction and arrow", async () => {
  const { box } = await readyPanel();
  assert.match(box.html(), /data-tm-sort="id"[^>]*aria-pressed="true"[^>]*tab-selected[^>]*>ID ↑<\/button>/, "the default sort is ID with the ascending text arrow");
  assert.doesNotMatch(box.html(), /data-tm-sort="(status|priority)"[^>]*aria-pressed="true"/, "only ID starts active");

  const active = (key: string): { label: string; arrow: string } | null => {
    const match = new RegExp(`data-tm-sort="${key}"[^>]*aria-pressed="true"[^>]*tab-selected[^>]*>([A-Za-z]+) ([↑↓])<\\/button>`).exec(box.html());
    return match === null ? null : { label: match[1]!, arrow: match[2]! };
  };
  box.el.querySelector<HTMLButtonElement>('[data-tm-sort="id"]')!.click();
  assert.deepEqual(active("id"), { label: "ID", arrow: "↓" }, "re-clicking keeps ID active and flips to descending");

  box.el.querySelector<HTMLButtonElement>('[data-tm-sort="id"]')!.click();
  assert.deepEqual(active("id"), { label: "ID", arrow: "↑" }, "a third click flips ID back to ascending");

  box.el.querySelector<HTMLButtonElement>('[data-tm-sort="priority"]')!.click();
  assert.deepEqual(active("priority"), { label: "Priority", arrow: "↑" }, "selecting another key moves the wash and restarts ascending");
  assert.doesNotMatch(box.html(), /data-tm-sort="id"[^>]*aria-pressed="true"/, "the old key loses the active wash");
  box.el.querySelector<HTMLButtonElement>('[data-tm-sort="priority"]')!.click();
  assert.deepEqual(active("priority"), { label: "Priority", arrow: "↓" }, "a second click on the fresh key flips it to descending");
});

test("the board keeps equal desktop columns and a single mobile lane", async () => {
  const { box } = await readyPanel();
  const grid = /<div data-tm-board class="([^"]*)"/.exec(box.html());
  assert.ok(grid !== null, box.html());
  const classes = grid[1]!;
  for (const declaration of [
    "grid",
    "grid-cols-1",
    "gap-3",
    "overflow-hidden",
    "px-3",
    "pt-[10px]",
    "md:grid-cols-3",
    "md:gap-5",
    "md:px-5",
  ]) {
    assert.ok(classes.includes(declaration), `${declaration} missing from ${classes}`);
  }
  assert.equal(classes.includes("overflow-x-auto"), false, "the horizontal rail is gone: " + classes);
  assert.equal(box.html().split("<section").length - 1, 3, "exactly three lanes render at every width");
});

test("the mobile status selector is an accessible group over one lane", async () => {
  const { box } = await readyPanel();
  const html = box.html();
  assert.match(html, /<div data-tm-status-selector role="group" aria-label="Task status"/);
  assert.match(html, /data-tm-status-selector[\s\S]*class="[^"]*md:hidden[^"]*"/);
  const buttons = [...html.matchAll(/<button[^>]*data-tm-status="([^"]*)"[^>]*>/g)];
  assert.deepEqual(buttons.map((m) => m[1]), ["To do", "In progress", "Done"], "the three statuses in order");
  assert.deepEqual(
    buttons.map((m) => m[0].includes('aria-pressed="true"')),
    [true, false, false],
    "To do is the initial lane",
  );
  for (const match of buttons) {
    assert.ok(match[0].includes("min-h-11"), `${match[1]} target reaches 44px`);
  }
  const sectionClasses = new Map(
    [...html.matchAll(/<section[^>]*data-tm-status="([^"]*)"[^>]*class="([^"]*)"/g)]
      .map((m) => [m[1]!, m[2]!]),
  );
  assert.equal(sectionClasses.get("To do")!.includes(" flex"), true, "the active lane shows on mobile");
  assert.equal(sectionClasses.get("In progress")!.includes("hidden md:flex"), true, "In progress is hidden on mobile");
  assert.equal(sectionClasses.get("Done")!.includes("hidden md:flex"), true, "Done is hidden on mobile");
});

test("selecting a mobile status re-presses its button and re-hides the others", async () => {
  const { box, panel } = await readyPanel();
  (panel as unknown as { mobileStatus: string }).mobileStatus = "Done";
  panel.render();
  const html = box.html();
  const buttons = [...html.matchAll(/<button[^>]*data-tm-status="([^"]*)"[^>]*>/g)];
  assert.deepEqual(
    buttons.map((m) => m[0].includes('aria-pressed="true"')),
    [false, false, true],
    "Done is now the pressed lane",
  );
  const sectionClasses = new Map(
    [...html.matchAll(/<section[^>]*data-tm-status="([^"]*)"[^>]*class="([^"]*)"/g)]
      .map((m) => [m[1]!, m[2]!]),
  );
  assert.equal(sectionClasses.get("To do")!.includes("hidden md:flex"), true, "To do is hidden once Done is active");
  assert.equal(sectionClasses.get("Done")!.includes(" flex"), true, "Done is the visible lane");
});

test("columns are unboxed and scroll independently", async () => {
  const { box } = await readyPanel();
  const sections = [...box.html().matchAll(/<section[^>]*class="([^"]*)"/g)].map((m) => m[1]!);
  assert.equal(sections.length, 3);
  for (const classes of sections) {
    assert.equal(
      /bg-|border|rounded/.test(classes),
      false,
      `a column must carry no background, border or radius: ${classes}`,
    );
    assert.ok(classes.includes("min-h-0"), classes);
  }
  // One scroller per column, not one for the board.
  assert.equal(box.html().split("overflow-y-auto").length - 1, 3);
  assert.match(box.html(), /class="flex min-h-0 flex-1 flex-col gap-3 overflow-y-auto overscroll-y-contain pb-4"/);
});

// Nothing tappable may sit under the touch floor on a phone. The sort tabs
// carried max-md:h-11 as utilities before they became a DS pill group; the
// floor now lives in app.css, so this asserts the rule is there rather than
// the utility, which is what stopped guarding it.
test("the sort tabs keep the touch floor on a phone", () => {
  const css = readFileSync(new URL("../../styles/app.css", import.meta.url), "utf8");
  // there is more than one narrow-viewport block, so check them all
  const blocks = [...css.matchAll(/@media \(max-width: 600px\) \{([\s\S]*?)\n\}/g)].map((m) => m[1]!);
  assert.ok(blocks.length > 0, "a narrow-viewport block exists");
  const floored = blocks.find((b) => /\.tab\b/.test(b));
  assert.ok(floored !== undefined, "the sort tab is in a narrow-viewport rule");
  assert.match(floored, /min-height:\s*var\(--touch-min\)/, "the floor is --touch-min");
});

// The pill tabs are 13px sentence-case labels, not the 11px uppercase
// micro style the DS token defaults to. This guards the rule so the label
// cannot silently return to micro caps.
test("the sort tabs use the sentence-case label style", () => {
  const css = readFileSync(new URL("../../styles/app.css", import.meta.url), "utf8");
  const tabBlock = /\.tab\s*\{[^}]*\}/.exec(css)?.[0] ?? "";
  assert.ok(/\.tab\s*\{/.test(css), ".tab rule exists");
  assert.match(tabBlock, /font-size:\s*var\(--type-label-size\)/, "pill tab label is 13px");
  assert.match(tabBlock, /line-height:\s*normal/, "pill tab line box is normal");
  assert.match(tabBlock, /letter-spacing:\s*var\(--type-label-ls\)/, "pill tab tracking matches the label token");
  assert.match(
    tabBlock,
    /font-variant-caps:\s*all-small-caps/,
    "pill tabs use the label voice's all-small-caps treatment",
  );
  assert.equal(
    /text-transform:\s*uppercase/.test(tabBlock),
    false,
    "pill tabs must not uppercase their labels",
  );
});

// Body text inherits letter-spacing, so a rule on the body reaches every
// text node in the app. The body carries no tracking; any declaration
// here would re-introduce the diffuse frame mismatch the desktop polish
// removed.
test("the body rule sets no letter-spacing", () => {
  const css = readFileSync(new URL("../../styles/app.css", import.meta.url), "utf8");
  const bodyBlock = /\bbody\s*\{[^}]*\}/.exec(css)?.[0] ?? "";
  assert.match(bodyBlock, /\bbody\s*\{/);
  assert.equal(
    /letter-spacing\s*:/.test(bodyBlock),
    false,
    "body rule must not declare letter-spacing; component-level tracking tokens are applied where they are used",
  );
});

test("a column header uses the sidebar Projects label tokens", async () => {
  const { box } = await readyPanel();
  const header = /<span data-tm-column-title class="([^"]*)">To do<\/span>/.exec(box.html());
  assert.ok(header !== null, box.html());
  const headingClasses = header[1]!;
  for (const declaration of [
    "text-micro",
    "font-medium",
    "uppercase",
    "tracking-[0.07em]",
    "text-text-secondary",
  ]) {
    assert.ok(headingClasses.includes(declaration), `${declaration} missing from ${headingClasses}`);
  }
  assert.equal(headingClasses.includes("font-label"), false, headingClasses);
  assert.equal(headingClasses.includes("max-md:"), false, headingClasses);
  // Neither header span sets a line-height, so both inherit the
  // body ratio against their own size - that is what makes the header 18.6px
  // and lands the first card at its intended y.
  assert.match(box.html(), /<span class="ml-auto font-ui text-caption leading-\[1\.55\] text-text-tertiary">/);
  assert.ok(headingClasses.includes("leading-[1.55]"), `inherited line ratio missing from ${headingClasses}`);
});

test("a card is a title-only summary: plain 12px id, 15px title, no footer", async () => {
  const { box } = await readyPanel();
  const card = [...box.html().matchAll(/<button type="button" data-tm-card[^>]*class="([^"]*)">([\s\S]*?)<\/button>/g)]
    .find((match) => match[2]!.includes(">first</span>")) ?? null;
  assert.ok(card !== null, box.html());
  const [, classes, body] = card as unknown as [string, string, string];
  for (const declaration of [
    "rounded-card",
    "border border-border",
    "bg-surface-card",
    "md:h-[132px]",
    "flex-none",
    "px-[14px]",
    "py-3",
    "gap-2",
    "hover:border-border-strong",
  ]) {
    assert.ok(classes.includes(declaration), `${declaration} missing from ${classes}`);
  }
  assert.equal(/\boverflow-/.test(classes), false, "the card is overflow: visible, so no overflow utility belongs here");
  // The card is the button: the task's own element carries the clickable
  // identity, and every child is flex-none so text boxes never shrink.
  assert.ok(classes.includes("flex-col"), "the card is a column flex");
  assert.ok(body.includes("flex-none"), "the card's children are flex:none");
  // The id is PLAIN mono at 12px — no chip, no background.
  assert.match(body, /<span class="flex-none font-mono text-caption leading-\[normal\] text-text-tertiary">1<\/span>/);
  assert.equal(/rounded-mark|bg-surface-muted/.test(body), false, "no id chip: " + body);
  // The title is the only UI-voice text on the card, clamped to two lines.
  assert.match(body, /<span data-tm-card-title class="[^"]*line-clamp-2[^"]*break-words[^"]*">first<\/span>/);
  assert.equal(/data-tm-card-dependencies/.test(body), false, "a task with no dependencies has no deps row");
  assert.equal(/data-tm-card-progress/.test(body), false, "a done task has no progress row");
  // The column already says the state, so the card carries no footer.
  assert.equal(/border-t|Pending|Done|In progress/.test(body), false, "no state footer: " + body);
});

test("a card with dependencies and progress pins them to the bottom after a spacer", async () => {
  const { box } = await readyPanel();
  const card = [...box.html().matchAll(/<button type="button" data-tm-card[^>]*class="([^"]*)">([\s\S]*?)<\/button>/g)]
    .find((match) => match[2]!.includes(">second</span>")) ?? null;
  assert.ok(card !== null, box.html());
  const [, , body] = card as unknown as [string, string, string];
  // The margin-top:auto spacer is its own zero-content element (decorative,
  // hidden from the a11y tree) between the title and the bottom rows; deps
  // and progress carry natural height.
  const spacer = /<span [^>]*data-tm-card-spacer class="([^"]*)"/.exec(body);
  assert.ok(spacer !== null, "the dedicated spacer renders: " + body);
  assert.ok(spacer[1]!.includes("mt-auto"), `the spacer pins the bottom rows: ${spacer[1]}`);
  assert.equal(/h-\[/.test(spacer[1]!), false, `the spacer has zero content height: ${spacer[1]}`);
  // Deps are inert text: the card is a button, so no nested focusable ref.
  assert.equal(/data-tm-card-dependencies[^>]*data-task-ref/.test(body), false, `no focusable dep ref inside the card button: ${body}`);
  const deps = /<span data-tm-card-dependencies class="([^"]*)">depends on <span data-task-dependency-inert>(.*?)<\/span>/.exec(body);
  assert.ok(deps !== null, "deps render when present: " + body);
  assert.equal(deps[2], "1", `the resolvable dependency renders as inert text: ${deps[2]}`);
  assert.ok(!deps[1]!.includes("mt-auto"), `the deps row is not the spacer: ${deps[1]}`);
  assert.equal(/h-\[/.test(deps[1]!), false, `the deps row is a natural-height line: ${deps[1]}`);
  const progress = /<span data-tm-card-progress class="([^"]*)"/.exec(body);
  assert.ok(progress !== null, "an in-progress task with subtasks renders progress");
  assert.ok(!progress[1]!.includes("mt-auto"), `the progress row is not the spacer: ${progress[1]}`);
  assert.equal(/h-\[/.test(progress[1]!), false, `the progress row is natural height: ${progress[1]}`);
});

test("the priority marker is a 6px dot plus an 11px caps label", async () => {
  const { box } = await readyPanel({
    listTaskmasterTasks: () => Promise.resolve({
      master: { tasks: [{ id: "1", title: "hot", status: "pending", priority: "high" }] },
    }),
  });
  const html = box.html();
  const dot = /<span aria-hidden="true" class="h-1\.5 w-1\.5 flex-none rounded-full (bg-[a-z-]+)"><\/span><span class="([^"]*)">high<\/span>/.exec(html);
  assert.ok(dot !== null, html);
  assert.equal(dot[1]!, "bg-error", "high is the error hue");
  assert.ok(dot[2]!.includes("priority-label"), `the label carries the uppercase normal-line-box treatment: ${dot[2]}`);
  assert.equal(dot[2]!.includes("font-label"), false, `the label no longer uses the system's small-caps voice: ${dot[2]}`);
});

test("priority dots use the priority colors, and out-of-schema values show verbatim", async () => {
  const { box } = await readyPanel({
    listTaskmasterTasks: () => Promise.resolve({
      master: {
        tasks: [
          { id: "1", title: "hot", status: "pending", priority: "high" },
          { id: "2", title: "warm", status: "pending", priority: "med" },
          { id: "3", title: "cool", status: "pending", priority: "low" },
          { id: "4", title: "odd", status: "pending", priority: "critical" },
          { id: "5", title: "none", status: "pending" },
        ],
      },
    }),
  });
  const html = box.html();
  // One chunk per card: a regex spanning the whole board would read the
  // first card's dot for every title.
  const cards = html.split('data-tm-task="').slice(1);
  const dotFor = (title: string): string => {
    const card = cards.find((c) => c.includes(`>${title}</span>`));
    assert.ok(card !== undefined, `no card for ${title}`);
    const dot = /rounded-full (bg-[a-z-]+)"/.exec(card);
    return dot === null ? "" : dot[1]!;
  };
  assert.equal(dotFor("hot"), "bg-error", "high is the error hue");
  assert.equal(dotFor("warm"), "bg-warning", "medium is amber, not red");
  assert.equal(dotFor("cool"), "bg-text-tertiary");
  assert.ok(html.includes(">critical</span>"), "an out-of-schema priority reads verbatim");
  assert.match(html, />none</, "a card without a priority still renders");
});

test("dependencies are one ellipsized interface-face line of inert text on the card", async () => {
  const { box } = await readyPanel();
  const deps = /<span data-tm-card-dependencies class="([^"]*)">depends on <span data-task-dependency-inert>1<\/span><\/span>/.exec(box.html());
  assert.ok(deps !== null, box.html());
  for (const declaration of ["truncate", "font-ui", "text-caption"]) {
    assert.ok(deps[1]!.includes(declaration), `${declaration} missing from ${deps[1]}`);
  }
  assert.ok(
    !box.html().includes("data-tm-card-dependencies data-task-ref"),
    "the card button holds no tabbable dependency reference",
  );
});

test("an in-progress task with subtasks shows the 3px track and its interface-face count", async () => {
  const { box } = await readyPanel({
    listTaskmasterTasks: () => Promise.resolve({
      master: {
        tasks: [
          { id: "1", title: "running", status: "in-progress", subtasks_done: 2, subtasks_total: 6 },
          { id: "2", title: "waiting", status: "pending", subtasks_done: 1, subtasks_total: 4 },
        ],
      },
    }),
  });
  const html = box.html();
  assert.match(html, /h-\[3px\][^"]*rounded-pill bg-wash-hover/);
  assert.match(html, /rounded-pill bg-accent" style="width:33%"/);
  assert.match(html, /<span class="flex-none font-ui text-caption leading-\[normal\] text-text-tertiary">2\/6<\/span>/);
  assert.equal(
    html.includes("1/4"),
    false,
    "a task that is not in progress carries no progress track",
  );
});

test("card text is escaped, not interpreted", async () => {
  const { box } = await readyPanel({
    listTaskmasterTasks: () => Promise.resolve({
      master: {
        tasks: [{
          id: "<img src=x>",
          title: "<script>alert(1)</script>",
          status: "pending",
          dependencies: ["<b>"],
        }],
      },
    }),
  });
  const html = box.html();
  assert.equal(html.includes("<script>"), false, html);
  assert.ok(html.includes("&lt;script&gt;alert(1)&lt;/script&gt;"));
  assert.ok(html.includes("&lt;img src=x&gt;"));
  assert.match(html, /depends on <span data-task-dependency-inert>&lt;b&gt;<\/span>/);
});

test("the detail dialog is where a task's description and state live", async () => {
  const detail = {
    id: "1",
    title: "explained",
    status: "in-progress",
    description: "the summary",
    details: "the long form",
    subtasks: [{ id: "1.1", title: "step", status: "done" }],
  };
  const { box, panel } = await readyPanel({
    listTaskmasterTasks: () => Promise.resolve({
      master: { tasks: [{ id: "1", title: "explained", status: "in-progress" }] },
    }),
    getTaskmasterTask: () => Promise.resolve(detail),
  });
  (panel as unknown as { detailTaskID: string | null }).detailTaskID = "1";
  (panel as unknown as { detailTask: TmTask | null }).detailTask = detail;
  panel.render();
  const html = box.html();
  assert.match(html, /role="dialog" aria-modal="true" aria-label="Task 1"/);
  assert.ok(html.includes("the summary"), "the canonical description lives here");
  assert.ok(html.includes("the long form"), "details render alongside the description");
  assert.ok(html.includes("In progress"), "so does the task's state");
  assert.ok(html.includes('aria-label="Close task"'));
  assert.ok(html.includes(">1.1</span>"), "subtasks are listed by their qualified id");
  assert.deepEqual(box.appends(), ["modals", "surface"],
    "the scrim mounts into the app-level modals region, not the panel, and the screen into the scrim");
});

test("without an app-level modals host the dialog host falls back to the panel", async () => {
  const box = fakeContainer(false);
  const model = new TaskmasterPanelModel(fakeApi(), "p1");
  const panel = new TaskmasterPanel(box.el, model);
  await panel.refresh();
  (panel as unknown as { detailTaskID: string | null }).detailTaskID = "1";
  panel.render();
  assert.ok(box.html().includes('role="dialog" aria-modal="true" aria-label="Task 1"'),
    "the dialog still renders through the fallback host");
  assert.deepEqual(box.appends(), ["container", "surface"],
    "a workspace without a modals region mounts the scrim inside the panel and the screen inside the scrim");
  panel.dispose();
});

test("opening a card loads its long-form detail and surfaces a read failure", async () => {
  let reject = false;
  const loaded = await readyPanel({
    getTaskmasterTask: (_projectID, taskID) => reject
      ? Promise.reject(new ApiError(500, "internal", "detail unavailable"))
      : Promise.resolve({ id: taskID, title: "loaded", status: "pending", details: "long form" }),
  });
  (loaded.panel as unknown as { detailTaskID: string | null }).detailTaskID = "1";
  loaded.panel.render();
  void (loaded.panel as unknown as { loadTaskDetail(): Promise<void> }).loadTaskDetail();
  const loadedBox = loaded.box;
  assert.ok(loadedBox.html().includes("Loading task…"));
  // The loading phase is the dialog's first paint, and the phone renders the
  // dialog as a route: a phase without the attribute flashes the desktop card
  // before snapping to full screen, and a failed load leaves the card up.
  assert.ok(/data-phone-fullscreen[^>]*aria-label="Task 1"/.test(loadedBox.html()),
    "the loading phase is not the phone route");
  await Promise.resolve();
  await Promise.resolve();
  assert.ok(loadedBox.html().includes("long form"));

  loadedBox.click("[data-tm-detail-close]");
  reject = true;
  (loaded.panel as unknown as { detailTaskID: string | null }).detailTaskID = "1";
  loaded.panel.render();
  void (loaded.panel as unknown as { loadTaskDetail(): Promise<void> }).loadTaskDetail();
  await Promise.resolve();
  await Promise.resolve();
  assert.ok(loadedBox.html().includes("detail unavailable"));
  // The error phase is the same branch: it must hold the route too, because
  // it is what a failed load leaves on screen.
  assert.ok(/data-phone-fullscreen[^>]*aria-label="Task 1"/.test(loadedBox.html()),
    "the error phase is not the phone route");
});

test("the onboarding state keeps the header, so the panel can always be closed", async () => {
  const { box } = await readyPanel({
    getTaskmasterInfo: () =>
      Promise.reject(new ApiError(404, "not_taskmaster_project", "no .taskmaster")),
  });
  const html = box.html();
  assert.ok(html.includes("No project tasks yet."));
  assert.ok(html.includes('aria-label="Close tasks"'), "the close control survives");
  assert.equal(html.includes("data-tm-board"), false, "there is no board to draw");
  assert.ok(html.includes(">Add PRD…<"), "the add-a-PRD affordance matters most here");
});

test("the Add PRD modal names the canonical file without claiming a path", async () => {
  const { box } = await readyPanel();
  box.click("[data-tm-add-prd]");
  const html = box.html();
  assert.match(html, /role="dialog" aria-modal="true" aria-label="Add PRD"/);
  assert.ok(html.includes("The file is saved as"), "the drop zone names the canonical file");
  assert.ok(html.includes(`>${PRD_CANONICAL_NAME}<`), `${PRD_CANONICAL_NAME} is the one store`);
  // The destination is the server-presented docs directory in mono —
  // signal, not an invented project-root claim.
  assert.ok(html.includes(">d/prd.md<"), "the destination path renders in mono");
  assert.equal(/name="filename"|Filename/.test(html), false, "no filename field");
  assert.ok(html.includes('type="file" accept=".md"'), "browse, beside the drop zone");
});

test("an open panel observes a later canonical backlog response", async () => {
  let tasks = sampleTasks;
  let reads = 0;
  const timer = fakeTimer();
  const model = new TaskmasterPanelModel(fakeApi({
    listTaskmasterTasks: () => {
      reads++;
      return Promise.resolve(tasks);
    },
  }), "p1");
  const box = fakeContainer();
  const panel = new TaskmasterPanel(box.el, model, {}, timer);

  await panel.refresh();
  assert.equal(timer.pending(), 0, "a mounted but closed board does not poll");
  await panel.open();
  assert.equal(timer.pending(), 1, "one owner observes the open board");
  assert.equal(timer.nextDelay(), 5000, "an idle board uses the bounded quiet cadence");
  tasks = { master: { tasks: [{ id: "8", title: "external edit", status: "done" }] } };
  assert.equal(await timer.fire(), true);

  assert.equal(model.tags[0]![1][0]!.title, "external edit");
  assert.ok(box.html().includes("external edit"), "the board repaints from the canonical answer");
  assert.equal(timer.pending(), 1, "the next bounded observation is scheduled after completion");

  panel.close();
  assert.equal(timer.pending(), 0, "closing stops observation");
  const readsAtClose = reads;
  assert.equal(await timer.fire(), false);
  assert.equal(reads, readsAtClose, "a closed board performs no canonical read");
});

test("an unchanged observation preserves the board DOM and focus", async () => {
  const timer = fakeTimer();
  const box = fakeContainer();
  const panel = new TaskmasterPanel(
    box.el,
    new TaskmasterPanelModel(fakeApi(), "p1"),
    {},
    timer,
  );
  await panel.open();
  const writes = box.writes();

  await timer.fire();
  assert.equal(box.writes(), writes, "stable data must not replace container.innerHTML");
  panel.dispose();
});

test("a changed poll keeps what the dialogs hold and the focus within them", async (t) => {
  // Each markup write draws fresh field and dialog nodes, as a browser does.
  const focusCalls: unknown[] = [];
  class FakeElement {
    hidden = false;
    tabIndex = 0;
    readonly listeners = new Map<string, (ev: unknown) => void>();
    addEventListener(type: string, fn: (ev: unknown) => void): void {
      this.listeners.set(type, fn);
    }
    removeEventListener(): void {}
    getClientRects(): unknown[] {
      return [{}];
    }
    focus(): void {
      doc.activeElement = this;
      focusCalls.push(this);
    }
  }
  class FakeField extends FakeElement {
    value = "";
    defaultValue = "";
    selectionStart = 0;
    selectionEnd = 0;
    readonly localName: string;
    readonly type: string;
    readonly attributes: Array<{ name: string; value: string }>;
    constructor(localName: string, type: string, attributes: Array<{ name: string; value: string }>) {
      super();
      this.localName = localName;
      this.type = type;
      this.attributes = attributes;
    }
    setSelectionRange(start: number, end: number): void {
      this.selectionStart = start;
      this.selectionEnd = end;
    }
  }
  class FakeInput extends FakeField {}
  class FakeTextArea extends FakeField {}
  // The priority select draws Medium selected.
  class FakeSelect extends FakeField {
    override value = "medium";
    override defaultValue = "medium";
    get options(): Array<{ selected: boolean; defaultSelected: boolean }> {
      return ["high", "medium", "low"].map((v) => ({ selected: v === this.value, defaultSelected: v === this.defaultValue }));
    }
  }
  class FakeDialog extends FakeElement {
    readonly surface: HTMLElement;
    readonly fields: () => FakeField[];
    constructor(surface: HTMLElement, fields: () => FakeField[]) {
      super();
      this.surface = surface;
      this.fields = fields;
    }
    querySelectorAll(): FakeField[] {
      return this.fields();
    }
    querySelector(selector: string): unknown {
      return this.surface.querySelector(selector);
    }
  }
  const saved = { element: globalThis.HTMLElement, input: globalThis.HTMLInputElement, area: globalThis.HTMLTextAreaElement, select: globalThis.HTMLSelectElement, css: globalThis.CSS };
  Object.assign(globalThis, { HTMLElement: FakeElement, HTMLInputElement: FakeInput, HTMLTextAreaElement: FakeTextArea, HTMLSelectElement: FakeSelect, CSS: { escape: (s: string) => s } });
  t.after(() => Object.assign(globalThis, { HTMLElement: saved.element, HTMLInputElement: saved.input, HTMLTextAreaElement: saved.area, HTMLSelectElement: saved.select, CSS: saved.css }));

  let reads = 0;
  const timer = fakeTimer();
  const box = fakeContainer();
  const doc = globalThis.document as unknown as { activeElement: unknown; createElement: () => HTMLElement };
  doc.activeElement = null;
  const create = doc.createElement;
  doc.createElement = () => {
    const surface = create();
    const markup = Object.getOwnPropertyDescriptor(surface, "innerHTML")!;
    const query = surface.querySelector.bind(surface);
    const queryAll = surface.querySelectorAll.bind(surface);
    let fields: FakeField[] = [];
    let dialog: FakeDialog | null = null;
    Object.defineProperty(surface, "innerHTML", {
      get: markup.get,
      set(next: string) {
        markup.set!.call(surface, next);
        fields = [...next.matchAll(/<(input|textarea|select)\b([^>]*)>/g)].map(([, tag, attrs]) => {
          const attributes = [...attrs!.matchAll(/\s(data-[\w-]+)(?:="([^"]*)")?/g)].map(([, name, value]) => ({ name: name!, value: value ?? "" }));
          const type = /\stype="(\w+)"/.exec(attrs!)?.[1] ?? "text";
          return new (tag === "input" ? FakeInput : tag === "textarea" ? FakeTextArea : FakeSelect)(tag!, type, attributes);
        });
        dialog = next.includes('role="dialog"') ? new FakeDialog(surface, () => fields) : null;
      },
    });
    surface.querySelectorAll = ((selector: string) =>
      selector === "input, textarea, select" ? fields : queryAll(selector)) as unknown as typeof surface.querySelectorAll;
    surface.querySelector = ((selector: string) => {
      if (selector === '[role="dialog"]') return dialog;
      const field = fields.find((f) => selector === `${f.localName}[${f.attributes[0]?.name}=""]` || selector === `[${f.attributes[0]?.name}]`);
      return field ?? query(selector);
    }) as unknown as typeof surface.querySelector;
    surface.contains = ((node: unknown) => node === dialog || fields.includes(node as FakeField)) as unknown as typeof surface.contains;
    return surface;
  };
  const panel = new TaskmasterPanel(
    box.el,
    new TaskmasterPanelModel(fakeApi({
      listTaskmasterTasks: () => Promise.resolve(reads++ === 0 ? sampleTasks : {
        master: { tasks: [...sampleTasks.master!.tasks, { id: String(2 + reads), title: `added elsewhere ${reads}`, status: "pending" }] },
      }),
    }), "p1"),
    {},
    timer,
  );
  await panel.open();
  const host = (): HTMLElement => (panel as unknown as { modalSlot: { element: HTMLElement } }).modalSlot.element;
  const field = (selector: string): FakeField => host().querySelector(selector) as unknown as FakeField;

  box.click("[data-tm-add-task]");
  assert.equal(doc.activeElement, field("[data-tm-task-title]"), "the dialog opens on the title");
  const typed = {
    "[data-tm-task-title]": "half a title",
    "[data-tm-task-description]": "why it matters",
    "[data-tm-task-details]": "how to do it",
    "[data-tm-task-priority]": "high",
  };
  for (const [selector, value] of Object.entries(typed)) field(selector).value = value;
  const description = field("[data-tm-task-description]");
  description.focus();
  description.setSelectionRange(4, 6);
  focusCalls.length = 0;

  await timer.fire();
  assert.ok(box.html().includes("added elsewhere 2"), "the poll observed a change and re-rendered");
  const rebuilt = field("[data-tm-task-description]");
  assert.notEqual(rebuilt, description, "the dialog's fields were drawn again");
  for (const [selector, value] of Object.entries(typed)) assert.equal(field(selector).value, value, selector);
  assert.deepEqual(focusCalls, [rebuilt], "focus returns to the field without passing through the title");
  assert.deepEqual([rebuilt.selectionStart, rebuilt.selectionEnd], [4, 6]);

  (host().querySelector('[role="dialog"]') as unknown as FakeDialog).listeners.get("keydown")!({ key: "Escape", preventDefault: () => {} });
  assert.equal(box.html().includes("data-tm-task-add-form"), false, "the focus trap is bound to the redrawn dialog");
  box.click("[data-tm-add-prd]");
  const opened = host().querySelector('[role="dialog"]');
  assert.equal(doc.activeElement, opened, "the PRD dialog opens on its container");
  field("[data-tm-prd-file]").listeners.get("change")!({
    target: { files: [{ name: "roadmap.md", size: 5, text: () => Promise.resolve("# PRD") }] },
  });
  await new Promise((resolve) => setImmediate(resolve));
  assert.ok(box.html().includes(">roadmap.md<"), "the picked file is drawn");
  focusCalls.length = 0;

  await timer.fire();
  assert.ok(box.html().includes("added elsewhere 3"), "the poll observed a change and re-rendered");
  assert.ok(box.html().includes(">roadmap.md<"), "the picked file survives the poll");
  assert.match(box.html(), /data-tm-prd-upload\s+data-hint/, "upload stays enabled");
  const redrawn = host().querySelector('[role="dialog"]');
  assert.notEqual(redrawn, opened, "the dialog was drawn again");
  assert.deepEqual(focusCalls, [redrawn], "with no field to restore, the dialog takes its initial focus");
  panel.dispose();
});

test("initializing an unstarted project reaches a board without a PRD", async () => {
  let initialized = false;
  let prdAsked = false;
  const model = new TaskmasterPanelModel(fakeApi({
    getTaskmasterInfo: () => initialized
      ? Promise.resolve({ path: "/x", config_path: "c", tasks_path: "t", docs_path: "d" })
      : Promise.reject(new ApiError(404, "not_taskmaster_project", "no task state")),
    initializeTaskmaster: () => {
      initialized = true;
      return Promise.resolve({ path: "/x/.git/noise-tasks/tasks.sqlite", tag: "main", already_initialized: false });
    },
    listTaskmasterPRDs: () => {
      prdAsked = true;
      return Promise.resolve([]);
    },
  }), "p1");
  await model.refresh();
  assert.equal(model.state, "uninitialized");

  await model.initialize();
  assert.equal(model.state, "ready");
  assert.equal(model.initializeError, null);
  // The board exists with no document supplied: a PRD fills a backlog, it is
  // not a condition of having one.
  assert.ok(!prdAsked || model.prds.length === 0, "no PRD was required to reach a board");
});

test("a failed initialization keeps the offer and reports why", async () => {
  const model = new TaskmasterPanelModel(fakeApi({
    getTaskmasterInfo: () => Promise.reject(new ApiError(404, "not_taskmaster_project", "no task state")),
    initializeTaskmaster: () => Promise.reject(new Error("engine refused")),
  }), "p1");
  await model.refresh();
  await model.initialize();
  assert.equal(model.state, "uninitialized", "a failure must not strand the panel");
  assert.match(model.initializeError ?? "", /engine refused/);
});

test("a second click cannot start a second initialization", async () => {
  let calls = 0;
  // The resolvers are collected rather than held in one binding: an
  // assignment inside the executor is invisible to narrowing, so a single
  // `let` reads as null at the call site.
  const release: Array<() => void> = [];
  const model = new TaskmasterPanelModel(fakeApi({
    getTaskmasterInfo: () => Promise.reject(new ApiError(404, "not_taskmaster_project", "no task state")),
    initializeTaskmaster: () => {
      calls += 1;
      return new Promise<TaskmasterInitResult>((resolve) => {
        release.push(() => resolve({ path: "/x", tag: "main", already_initialized: false }));
      });
    },
  }), "p1");
  await model.refresh();
  const first = model.initialize();
  const second = model.initialize();
  for (const r of release) r();
  await Promise.all([first, second]);
  assert.equal(calls, 1, "the in-flight guard must hold");
  assert.equal(release.length, 1, "only one engine call was started");
});

test("an open onboarding panel observes external project initialization", async () => {
  let initialized = false;
  const timer = fakeTimer();
  const model = new TaskmasterPanelModel(fakeApi({
    getTaskmasterInfo: () => initialized
      ? Promise.resolve({ path: "/x", config_path: "c", tasks_path: "t", docs_path: "d" })
      : Promise.reject(new ApiError(404, "not_taskmaster_project", "not yet")),
  }), "p1");
  const box = fakeContainer();
  const panel = new TaskmasterPanel(box.el, model, {}, timer);
  await panel.open();
  assert.equal(model.state, "uninitialized");
  assert.equal(timer.pending(), 1, "onboarding stays observable while open");

  initialized = true;
  await timer.fire();
  assert.equal(model.state, "ready");
  assert.equal(model.tags[0]![1].length, 2);
  assert.ok(box.html().includes("first"), "the externally initialized board replaces onboarding");
  panel.dispose();
});

test("closing during a canonical refresh suppresses its repaint and successor", async () => {
  let delayed = false;
  let release!: (tasks: TaskmasterTasks) => void;
  const gate = new Promise<TaskmasterTasks>((resolve) => { release = resolve; });
  const timer = fakeTimer();
  const model = new TaskmasterPanelModel(fakeApi({
    listTaskmasterTasks: () => delayed ? gate : Promise.resolve(sampleTasks),
  }), "p1");
  const box = fakeContainer();
  const panel = new TaskmasterPanel(box.el, model, {}, timer);
  await panel.open();
  const beforeHTML = box.html();
  delayed = true;
  const late = timer.fire();
  panel.close();
  release({ master: { tasks: [{ id: "9", title: "too late", status: "pending" }] } });
  await late;

  assert.equal(box.html(), beforeHTML, "a response from the closed lifecycle does not repaint");
  assert.equal(timer.pending(), 0, "the late completion cannot restart observation");
});

test("the same scheduler follows an active generation and returns to the idle cadence", async () => {
  let taskReads = 0;
  let runPolls = 0;
  const timer = fakeTimer();
  const model = new TaskmasterPanelModel(fakeApi({
    listTaskmasterTasks: () => {
      taskReads++;
      return Promise.resolve(sampleTasks);
    },
    getTaskmasterRun: () => Promise.resolve(++runPolls === 1
      ? tmRun()
      : tmRun({ status: "exited", exit_code: 0, finished_at: "f" })),
  }), "p1");
  await model.refresh();
  await model.generate();
  const panel = new TaskmasterPanel(fakeContainer().el, model, {}, timer);
  await panel.open();

  assert.equal(timer.nextDelay(), 2000, "active generation keeps its tighter cadence");
  const before = taskReads;
  await timer.fire();
  assert.equal(model.activeRun?.status, "running");
  assert.equal(taskReads, before + 1, "a still-active run also observes canonical edits once");
  assert.equal(timer.nextDelay(), 2000);
  const beforeExit = taskReads;
  await timer.fire();
  assert.equal(model.activeRun?.status, "exited");
  assert.equal(taskReads, beforeExit + 1, "the observed exit reloads authoritative tasks once");
  assert.equal(timer.nextDelay(), 5000, "the single scheduler returns to idle observation");
  panel.dispose();
  assert.equal(timer.pending(), 0, "project/session teardown stops the scheduler");
});

// --- Disposal ---

test("a disposed panel neither repaints nor polls on a late answer", async () => {
  let released!: () => void;
  const gate = new Promise<void>((resolve) => { released = () => resolve(); });
  const model = new TaskmasterPanelModel(fakeApi({
    listTaskmasterTasks: async () => {
      await gate;
      return sampleTasks;
    },
  }), "p1");
  const box = fakeContainer();
  const panel = new TaskmasterPanel(box.el, model);
  const pending = panel.refresh();
  panel.dispose();
  assert.equal(box.html(), "", "disposal clears the panel");
  released();
  await pending;
  assert.equal(box.html(), "", "the late answer must not repaint a disposed panel");
});

test("a resolvable task reference in detail text linkifies; an unknown id does not", async () => {
  const { box, panel } = await readyPanel({
    listTaskmasterTasks: () => Promise.resolve({
      master: {
        tasks: [
          {
            id: "1",
            title: "owner",
            status: "done",
            description: "description points to Task 2 and Task 999",
            details: "details point to task-3",
          },
          { id: "2", title: "referenced", status: "pending" },
          { id: "3", title: "also referenced", status: "pending" },
        ],
      },
    }),
  });
  (panel as unknown as { detailTaskID: string | null }).detailTaskID = "1";
  (panel as unknown as { detailTask: TmTask | null }).detailTask = {
    id: "1", title: "owner", status: "done",
    description: "description points to Task 2 and Task 999",
    details: "details point to task-3",
  };
  panel.render();
  const html = box.html();
  assert.ok(html.includes('data-task-ref="2"'), "a reference to a known task linkifies");
  assert.ok(html.includes('data-task-ref="3"'), "details are linkified too");
  assert.ok(!html.includes('data-task-ref="999"'), "an unknown id stays plain text");
  assert.ok(html.includes("Task 999"), "the unknown reference is still readable as text");
});

test("task detail markup that tries to inject through a reference stays escaped", async () => {
  const { box, panel } = await readyPanel({
    listTaskmasterTasks: () => Promise.resolve({
      master: {
        tasks: [
          {
            id: "1",
            title: "owner",
            status: "done",
            details: 'Task 2" onload="alert(1)<img src=x onerror=alert(2)>',
          },
          { id: "2", title: "referenced", status: "pending" },
        ],
      },
    }),
  });
  (panel as unknown as { detailTaskID: string | null }).detailTaskID = "1";
  (panel as unknown as { detailTask: TmTask | null }).detailTask = {
    id: "1", title: "owner", status: "done",
    details: 'Task 2" onload="alert(1)<img src=x onerror=alert(2)>',
  };
  panel.render();
  const html = box.html();
  assert.ok(!/<[a-z]+[^>]*\son\w+=/i.test(html), "no element may carry an event handler");
  assert.ok(!html.includes("<img"), "no img tag survives");
  const idAttr = /data-task-ref="([^"]*)"/.exec(html);
  assert.ok(idAttr !== null && idAttr[1] === "2", "the id attribute is digits only");
});

test("generation uses the most recently active session's harness or omits it", async () => {
  const sessions = [
    { provider: "claude", updated_at: "2026-10-01T00:00:00Z" },
    { provider: "codex", updated_at: "2026-10-02T00:00:00Z" },
    { provider: "kimi", updated_at: "2026-09-01T00:00:00Z", last_output_at: "2026-10-03T00:00:00Z" },
  ] as Awaited<ReturnType<ApiClient["listSessions"]>>;
  for (const [available, explicit, expected] of [
    [sessions, undefined, "kimi"],
    [sessions.slice(0, 2), undefined, "codex"],
    [[], undefined, undefined],
    [sessions, "omp", "omp"],
    [[{ ...sessions[0]!, provider: "shell" }], undefined, "shell"],
  ] as const) {
    const calls: unknown[][] = [];
    const model = new TaskmasterPanelModel(fakeApi({
      listSessions: async (id) => { assert.equal(id, "p1"); return [...available]; },
      generateTasks: async (...args) => { calls.push(args); return tmRun(); },
    }), "p1");
    await model.refresh();
    await model.generate(explicit);
    assert.deepEqual(calls, [["p1", "PRD.md", undefined, expected]]);
  }
});

test("generation proceeds without a harness when the session list fails", async () => {
  const calls: unknown[][] = [];
  const model = new TaskmasterPanelModel(fakeApi({
    listSessions: async () => { throw new Error("sessions unavailable"); },
    generateTasks: async (...args) => { calls.push(args); return tmRun(); },
  }), "p1");
  await model.refresh();
  await model.generate();
  assert.deepEqual(calls, [["p1", "PRD.md", undefined, undefined]]);
  assert.equal(model.generateError, null);
});
