// One field, one dialog: what
// was typed decides the shape below it. These pin the decision table —
// which readout, which store line, whether identity applies, what the
// primary says — for every kind the inspection can answer, so no two
// readouts can describe the same directory two ways.
import assert from "node:assert/strict";
import test from "node:test";
import type { CloneJob, Inspection } from "../../../api/projects.ts";
import { addProjectModalHtml, addProjectView, plainSummary, hostForm, type AddProjectState } from "./add-project.ts";

const projects = [
  { id: "p1", name: "Deck", path: "/workspace/deck" },
  { id: "p2", name: "noisefloor-site", path: "/workspace/noisefloor-site" },
];
const profile = { id: "id-1", name: "Test Author", email: "test@example.invalid", revision: 1, created_at: "t", updated_at: "t" };

function state(input: string, inspection: Inspection | null, over: Partial<AddProjectState> = {}): AddProjectState {
  return {
    input, inspection, inspectError: "", projects, profiles: [profile], selectedProfileID: "id-1",
    busy: false, inspecting: false, serverError: "", job: null, projectsRoot: "/workspace", ...over,
  };
}

const store = (root: string) => `${root}/.git/noise-tasks/tasks.sqlite`;

test("an empty field offers nothing and the primary is dead", () => {
  const v = addProjectView(state("", null));
  assert.equal(v.kind, "empty");
  assert.equal(v.primaryLabel, "Add project");
  assert.equal(v.primaryDisabled, true);
  assert.equal(v.storeOwn, false);
  assert.equal(v.signable, false);
});

test("a path that does not exist is a new directory with its own store and an identity", () => {
  const v = addProjectView(state("brand-new", { kind: "absent", path: "/workspace/brand-new", exists: false, entries: [], store_path: store("/workspace/brand-new") }));
  assert.equal(v.kind, "new");
  assert.equal(v.storeOwn, true);
  assert.equal(v.signable, true);
  assert.equal(v.primaryLabel, "Add project");
  assert.equal(v.primaryDisabled, false);
  const html = addProjectModalHtml(state("brand-new", { kind: "absent", path: "/workspace/brand-new", exists: false, entries: [], store_path: store("/workspace/brand-new") }));
  assert.ok(html.includes("git init · no history"), "git init is stated because it is the difference between a directory and a project");
  assert.ok(!html.includes("Created if it doesn"), "no hedge goes back under that field");
});

test("the identity select appears only when there is a real choice", () => {
  const inspection: Inspection = { kind: "absent", path: "/workspace/brand-new", exists: false, entries: [], store_path: store("/workspace/brand-new") };
  const second = { ...profile, id: "id-2", email: "second@example.invalid" };
  assert.ok(!addProjectModalHtml(state("brand-new", inspection, { profiles: [] })).includes("data-project-git-profile"),
    "with none, adding asks for one on its own screen");
  assert.ok(!addProjectModalHtml(state("brand-new", inspection)).includes("data-project-git-profile"),
    "the only identity applies without a field");
  const several = addProjectModalHtml(state("brand-new", inspection, { profiles: [profile, second], selectedProfileID: "id-2" }));
  assert.ok(several.includes("data-project-git-profile"));
  assert.ok(several.includes('value="id-2" selected'));
  assert.equal(addProjectView(state("brand-new", inspection, { profiles: [] })).primaryDisabled, false,
    "no identity yet does not hold the primary");
});

test("a plain directory says what is there, owns no store, and applies no identity", () => {
  const s = state("tmp", { kind: "plain", path: "/workspace/tmp", exists: true, entries: ["vd-gate2", "vd-perf"] });
  const v = addProjectView(s);
  assert.equal(v.kind, "plain");
  assert.equal(v.storeOwn, false);
  assert.equal(v.storeParent, false);
  assert.equal(v.signable, false);
  assert.equal(v.primaryLabel, "Add as project");
  const html = addProjectModalHtml(s);
  assert.ok(html.includes("No repository here — and none in any directory above it."));
  assert.ok(html.includes("2 directories, no .git"), "derived from the same array as the collision list");
  assert.ok(html.includes("Git identity does not apply"));
  assert.ok(!html.includes("Task store"), "nothing owns a store until it is a repository");
});

test("a repository root is recognised with its own store", () => {
  const s = state("deck", { kind: "repository", path: "/workspace/audio-player", exists: true, entries: [".git", "src"], remote: "github.com/fable/audio", head: "main · 8f3c21a · 3d ago · working tree clean", store_path: store("/workspace/audio-player") });
  const v = addProjectView(s);
  assert.equal(v.kind, "repo");
  assert.equal(v.storeOwn, true);
  assert.equal(v.signable, true);
  assert.equal(v.primaryLabel, "Add project");
  const html = addProjectModalHtml(s);
  assert.ok(html.includes("origin github.com/fable/audio"));
  assert.ok(html.includes("Its own repository root."));
});

test("a worktree carries the boundary warning verbatim and takes its parent's store", () => {
  const s = state("/workspace/tmp/vd-gate2", { kind: "worktree", path: "/workspace/tmp/vd-gate2", exists: true, entries: [".git"], remote: "github.com/fable/visual-diff", head: "feat/gate2 · 41b90c7 · 2h ago · 4 files modified", parent: { name: "visual-diff", path: "/workspace/visual-diff" }, store_path: store("/workspace/visual-diff") });
  const v = addProjectView(s);
  assert.equal(v.kind, "worktree");
  assert.equal(v.nested, false);
  assert.equal(v.storeOwn, false);
  assert.equal(v.storeParent, true);
  assert.equal(v.signable, true, "a worktree is signed by this project");
  assert.equal(v.primaryLabel, "Add anyway");
  assert.equal(v.parentAction, "Add visual-diff instead", "the parent is not in the rail");
  const html = addProjectModalHtml(s);
  assert.ok(html.includes("A worktree's store lives inside the parent repository's .git, so working in this directory cannot change where a write lands. Tasks here go to <span class=\"font-medium text-text-primary\">visual-diff</span>'s board — including tasks an agent writes while working in this directory."));
  assert.ok(html.includes(`store ${store("/workspace/visual-diff")}`));
});

test("a plain path inside a registered repository binds to it and hides identity", () => {
  const s = state("/workspace/deck/src", { kind: "plain", path: "/workspace/deck/src", exists: true, entries: [], parent: { name: "Deck", path: "/workspace/deck" }, store_path: store("/workspace/deck") });
  const v = addProjectView(s);
  assert.equal(v.kind, "plain");
  assert.equal(v.nested, true);
  assert.equal(v.signable, false, "the parent's identity governs");
  assert.equal(v.storeParent, true);
  assert.equal(v.primaryLabel, "Add anyway");
  assert.equal(v.parentAction, "Open Deck", "the parent is already in the rail");
  const html = addProjectModalHtml(s);
  assert.ok(html.includes("This directory is inside <span class=\"font-medium text-text-primary\">Deck</span>, so working in this directory cannot change where a write lands."));
  assert.ok(!html.includes("What is there"), "the nested case is not board 2d");
});

test("a path already in the sidebar is an error with a way to it, matched on path never name", () => {
  const s = state("/workspace/noisefloor-site", { kind: "repository", path: "/workspace/noisefloor-site", exists: true, entries: [".git"], store_path: store("/workspace/noisefloor-site") });
  const v = addProjectView(s);
  assert.equal(v.kind, "dup");
  assert.equal(v.dup?.name, "noisefloor-site");
  assert.equal(v.primaryDisabled, true);
  assert.equal(v.storeOwn, false);
  const html = addProjectModalHtml(s);
  assert.ok(html.includes("Already in the sidebar as noisefloor-site."));
  assert.ok(html.includes("data-project-show-dup"));
});

test("a URL clones into a destination named by the repository", () => {
  const s = state("fable/tasks-engine", { kind: "clone", path: "/workspace/tasks-engine", exists: false, entries: [], clone_url: "https://github.com/fable/tasks-engine", store_path: store("/workspace/tasks-engine") });
  const v = addProjectView(s);
  assert.equal(v.kind, "url");
  assert.equal(v.primaryLabel, "Clone and add", "everything else here is instant; a clone is not");
  assert.equal(v.storeOwn, true);
  assert.equal(v.signable, true);
  const html = addProjectModalHtml(s);
  assert.ok(html.includes("Clone into"));
  assert.ok(html.includes("From the repository name."));
});

test("a destination that already holds this repository adopts it without fetching", () => {
  const s = state("fable/deck", { kind: "clone", path: "/workspace/deck", exists: true, entries: [".git", "src"], clone_url: "https://github.com/fable/deck", same_clone: true, remote: "github.com/fable/deck", store_path: store("/workspace/deck") });
  const v = addProjectView(s);
  assert.equal(v.sameClone, true);
  assert.equal(v.collide, false);
  assert.equal(v.primaryLabel, "Add existing clone");
  assert.ok(addProjectModalHtml(s).includes("Already cloned here from"));
});

test("a destination holding anything else is a collision that lists what is there", () => {
  const s = state("github.com/other/deck", { kind: "clone", path: "/workspace/deck", exists: true, entries: [".git", "assets", "src", "docs", "vendor"], clone_url: "https://github.com/other/deck" });
  const v = addProjectView(s);
  assert.equal(v.collide, true);
  assert.equal(v.primaryDisabled, true);
  assert.equal(v.storeOwn, false, "no store for a project that will not exist");
  assert.equal(v.signable, false);
  const html = addProjectModalHtml(s);
  assert.ok(html.includes("Not empty — 5 items already here."));
  assert.ok(html.includes("and 1 more"));
});

test("a clone landing inside a repository warns and moves its destination", () => {
  const s = state("fable/tasks-engine", { kind: "clone", path: "/workspace/deck/tasks-engine", exists: false, entries: [], clone_url: "https://github.com/fable/tasks-engine", parent: { name: "Deck", path: "/workspace/deck" }, store_path: store("/workspace/deck") });
  const v = addProjectView(s);
  assert.equal(v.nested, true);
  assert.equal(v.primaryLabel, "Clone anyway");
  assert.equal(v.parentAction, "Choose another directory");
  assert.ok(addProjectModalHtml(s).includes("The store sits inside <span class=\"font-medium text-text-primary\">Deck</span>'s own .git"));
});

const job = (over: Partial<CloneJob>): CloneJob => ({
  id: "c1", url: "https://github.com/fable/tasks-engine", destination: "/workspace/tasks-engine", name: "tasks-engine",
  status: "running", percent: 34, line: "Receiving objects:  34% (1/3), 4.20 MiB | 1.20 MiB/s", partial_bytes: 4_404_019,
  started_at: "t", updated_at: "t", ...over,
});

test("a running clone shows git's own progress and can only be backgrounded or cancelled", () => {
  const s = state("fable/tasks-engine", null, { job: job({}) });
  const v = addProjectView(s);
  assert.equal(v.running, true);
  assert.equal(v.editing, false);
  assert.equal(v.desc, "Cloning into tasks-engine.");
  const html = addProjectModalHtml(s);
  assert.ok(html.includes(">34% of this phase<"));
  assert.ok(html.includes("Receiving objects:  34%"));
  assert.ok(html.includes("data-clone-background") && html.includes("data-clone-cancel"));
  assert.ok(!html.includes("data-project-submit"));
});

test("a stopped clone keeps its partial directory as a state with resume and discard", () => {
  for (const j of [job({ status: "stopped", stderr: "fatal: early EOF" }), job({ status: "failed", failure: "network", stderr: "fatal: early EOF" })]) {
    const s = state("fable/tasks-engine", null, { job: j });
    const v = addProjectView(s);
    assert.equal(v.stopped, true, `${j.status}/${j.failure ?? ""} is the stopped state`);
    assert.equal(v.desc, "The clone stopped part-way. Nothing was deleted.");
    const html = addProjectModalHtml(s);
    assert.ok(html.includes("Partial clone on disk"));
    assert.ok(html.includes("4.2 MB"));
    assert.ok(html.includes("Resuming starts the fetch again."));
    assert.ok(html.includes("data-clone-resume") && html.includes("data-clone-discard"));
    assert.ok(html.includes("Git said"));
  }
});

test("bad and private are one refusal; a refused key names the fix", () => {
  const refused = addProjectModalHtml(state("fable/missing", { kind: "clone", path: "/workspace/missing", exists: false, entries: [], clone_url: "https://github.com/fable/missing" }, { job: job({ url: "https://github.com/fable/missing", status: "failed", failure: "bad_or_private", stderr: "remote: Repository not found." }) }));
  assert.ok(refused.includes("No repository there, or it is private."));
  assert.ok(refused.includes("Use the SSH URL for a private repository."));
  const auth = addProjectModalHtml(state("git@github.com:fable/private.git", { kind: "clone", path: "/workspace/private", exists: false, entries: [], clone_url: "git@github.com:fable/private.git" }, { job: job({ url: "git@github.com:fable/private.git", status: "failed", failure: "auth", stderr: "Permission denied (publickey)." }) }));
  assert.ok(auth.includes("The server refused this machine's key."));
  assert.ok(auth.includes("ssh -T git@github.com"));
  assert.equal(addProjectView(state("x", null, { job: job({ status: "failed", failure: "auth" }) })).editing, true, "a failure returns to the field");
});

test("a successful re-inspection enables retry of the same failed clone", () => {
  const failedJob = job({ status: "failed", failure: "bad_or_private", stderr: "remote: Repository not found." });
  const s = state("fable/tasks-engine", { kind: "clone", path: "/workspace/tasks-engine", exists: false, entries: [], clone_url: failedJob.url }, { job: failedJob, inspecting: true });
  assert.equal(addProjectView(s).failed, true);
  assert.equal(addProjectView(s).primaryDisabled, true);

  s.inspecting = false;
  s.input = "fable/other-project";
  s.inspection = { kind: "clone", path: "/workspace/other-project", exists: false, entries: [], clone_url: "https://github.com/fable/other-project" };
  assert.equal(addProjectView(s).failed, false);
  assert.equal(addProjectView(s).primaryDisabled, false);
  assert.ok(!addProjectModalHtml(s).includes("No repository there, or it is private."));

  s.input = "fable/tasks-engine";
  s.inspection = { kind: "clone", path: "/workspace/tasks-engine", exists: true, empty: true, entries: [], clone_url: failedJob.url };
  const retry = addProjectView(s);
  assert.equal(retry.failed, true, "the failure remains visible while retry is available");
  assert.equal(retry.collide, false);
  assert.equal(retry.primaryDisabled, false);

  s.inspection = { kind: "clone", path: "/workspace/tasks-engine", exists: true, entries: [], clone_url: failedJob.url };
  assert.equal(addProjectView(s).primaryDisabled, true, "files and hidden directories omitted from entries still block retry");

  s.input = "other/deck";
  s.inspection = { kind: "clone", path: "/workspace/deck", exists: true, entries: [".git", "src"], clone_url: "https://github.com/other/deck" };
  assert.equal(addProjectView(s).collide, true, "a different occupied destination stays blocked");
  assert.equal(addProjectView(s).primaryDisabled, true);

  s.inspection = { kind: "clone", path: "/workspace/deck", exists: true, empty: true, entries: [], clone_url: "https://github.com/other/deck" };
  assert.equal(addProjectView(s).primaryDisabled, true, "a different repository's existing empty destination stays blocked");

  s.input = "fable/other-tasks-engine";
  s.inspection = { kind: "clone", path: "/workspace/other-tasks-engine", exists: true, empty: true, entries: [], clone_url: failedJob.url };
  assert.equal(addProjectView(s).primaryDisabled, true, "the same repository at a different existing destination stays blocked");

  s.input = failedJob.url;
  s.inspection = { kind: "clone", path: "/workspace/tasks-engine", exists: true, entries: [".git", "src"], clone_url: failedJob.url };
  assert.equal(addProjectView(s).collide, true, "the failed repository's occupied destination stays blocked");
  assert.equal(addProjectView(s).primaryDisabled, true);
});

test("a finished clone offers an empty board and asks for no PRD", () => {
  const s = state("fable/tasks-engine", null, { job: job({ status: "done", percent: 100, project_id: "p9" }) });
  const v = addProjectView(s);
  assert.equal(v.done, true);
  assert.equal(v.title, "tasks-engine imported");
  const html = addProjectModalHtml(s);
  assert.ok(html.includes("Start an empty board"));
  assert.ok(html.includes("data-clone-create-board"));
  assert.ok(!/PRD/.test(html));
});

test("an inspection in flight is not actionable, whatever the last answer said", () => {
  const ins: Inspection = { kind: "absent", path: "/workspace/old", exists: false, entries: [], store_path: store("/workspace/old") };
  const settled = addProjectView(state("old", ins));
  assert.equal(settled.primaryDisabled, false);
  // The field now says something newer and the server has not answered it:
  // the old answer must not be acted on under the new text.
  const racing = addProjectView(state("newer", ins, { inspecting: true }));
  assert.equal(racing.primaryDisabled, true);
  assert.equal(racing.blocked, true);
  assert.equal(racing.storeOwn, false, "no store is claimed for text the server has not seen");
  assert.equal(racing.signable, false);
});

test("summaries and host forms are derived, never sampled", () => {
  assert.equal(plainSummary([]), "Empty, no .git");
  assert.equal(plainSummary(["one"]), "1 directory, no .git");
  assert.equal(hostForm("https://github.com/fable/deck.git"), "github.com/fable/deck");
  assert.equal(hostForm("git@github.com:fable/deck.git"), "github.com/fable/deck");
});

function head(html: string): string {
  return html.slice(html.indexOf('<header class="dialog-head">'), html.indexOf("</header>"));
}

test("every source screen leads its head with Back and keeps the body free of it", () => {
  for (const [source, target] of [["local", "choose"], ["url", "choose"], ["github", "github"]] as const) {
    const html = addProjectModalHtml(state("", null, { source }));
    const h = head(html);
    const back = h.indexOf(`data-project-source="${target}" aria-label="Back"`);
    assert.ok(back !== -1 && back < h.indexOf("Add project") && h.indexOf("Add project") < h.indexOf("data-project-cancel"), `${source}: back, title, close`);
    assert.ok(!html.slice(html.indexOf("</header>")).includes("data-project-source"), `${source}: no Back in the body`);
  }
  const running = addProjectModalHtml(state("fable/tasks-engine", null, { source: "url", job: job({}) }));
  assert.ok(!running.includes('aria-label="Back"'), "a running clone has nothing to go back to");
});

test("the folder screen asks for a folder under the projects folder, never a URL", () => {
  const html = addProjectModalHtml(state("", null, { source: "local" }));
  assert.ok(html.includes('for="add-project-input">Folder</label>'));
  assert.ok(html.includes('placeholder="/workspace/my-project"'));
  assert.ok(html.includes("data-project-browse"));
  assert.ok(!html.includes("github.com"), "no repository example on the folder screen");
  assert.ok(!html.includes("font-label"), "labels are sentence case, not small caps");
  const mac = addProjectModalHtml(state("", null, { source: "local", projectsRoot: "/Users/you/Projects/" }));
  assert.ok(mac.includes('placeholder="/Users/you/Projects/my-project"'), "the example is a path on this machine");
  const unknown = addProjectModalHtml(state("", null, { source: "local", projectsRoot: "" }));
  assert.ok(unknown.includes('placeholder=""'), "no invented path before the server names its folder");
});

test("the repository URL screen asks for a URL and offers no folder picker", () => {
  const html = addProjectModalHtml(state("", null, { source: "url" }));
  assert.ok(html.includes('for="add-project-input">Repository URL</label>'));
  assert.ok(html.includes('placeholder="https://github.com/owner/repo"'));
  assert.ok(!html.includes("data-project-browse"));
  assert.ok(!html.includes("/workspace/my-project"));
});

test("the primary is enabled exactly when the form is valid", () => {
  const disabled = /data-project-submit\s+disabled\s/;
  assert.match(addProjectModalHtml(state("", null, { source: "local" })), disabled);
  const valid = addProjectModalHtml(state("brand-new", { kind: "absent", path: "/workspace/brand-new", exists: false, entries: [], store_path: store("/workspace/brand-new") }, { source: "local" }));
  assert.doesNotMatch(valid, disabled);
});
