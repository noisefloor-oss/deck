// Tests for the Files panel. The model is DOM-free, so a fake
// FileApiClient drives it exactly as the server would. The DOM shell is
// exercised through the same stub container the Taskmaster panel's tests
// use — there is no jsdom here, so the shell's rendered markup is read
// back off innerHTML and its measured geometry is pinned by the browser
// probe (tools/files-panel-probe.mjs).
import assert from "node:assert/strict";
import test from "node:test";

import { ApiError } from "../../api/http.ts";
import type {
  FileApiClient,
  FileEntry,
  FileListing,
  FileUploadResult,
  FileView,
  UploadConflictInfo,
  UploadConflictReason,
} from "../../api/files.ts";
import { FilesPanel, readWrap, writeWrap } from "./files-panel-dom.ts";
import {
  breadcrumbs,
  confine,
  fileExtension,
  FilesPanelModel,
  formatMiB,
  isImageName,
  kindOfName,
  projectPathPrefix,
  projectRootLabel,
  rowChange,
  rowMeta,
  rowName,
  splitFileName,
  suggestUploadName,
  uploadBasename,
  viewerStatus,
} from "./files-panel.ts";

// assertNoNestedDataHints fails when an element carrying [data-hint] has a
// descendant that also carries [data-hint]. hintValue() resolves with the
// nearest ancestor, so nested hints shadow each other and are always a bug.
function assertNoNestedDataHints(html: string, message: string): void {
  const voidTags = new Set([
    "area", "base", "br", "col", "embed", "hr", "img", "input", "link",
    "meta", "param", "source", "track", "wbr",
  ]);
  const tagRegex = /<(\/?)([a-zA-Z][a-zA-Z0-9-]*)\b([^>]*?)(\/?)>/g;
  const hintStack: boolean[] = [];
  let match: RegExpExecArray | null;
  while ((match = tagRegex.exec(html)) !== null) {
    const [, slash, tagName, attrs, selfClose] = match;
    if (slash === "/") {
      hintStack.pop();
      continue;
    }
    const hasHint = /\sdata-hint\s*=/.test(attrs ?? "");
    if (hasHint && hintStack.some((h) => h)) {
      assert.fail(`${message}: nested data-hint around \`${match[0] ?? ""}\``);
    }
    if (selfClose !== "/" && !voidTags.has(tagName ?? "")) {
      hintStack.push(hasHint);
    }
  }
}

const PROJECT = "proj-1";
const PROJECT_PATH = "/workspace/noisedeck";

function entry(path: string, dir = false, size = 100, mtime = "2026-08-02T11:00:00Z"): FileEntry {
  return {
    name: path.slice(path.lastIndexOf("/") + 1),
    path,
    dir,
    size,
    mode: dir ? "rwxr-xr-x" : "rw-r--r--",
    mtime,
  };
}

const ROOT_ENTRIES = [
  entry("docs", true, 4096),
  entry("README.md", false, 268),
  entry("logo.png", false, 18422),
];

interface Recorder {
  listed: string[];
  viewed: string[];
  checked: string[];
  uploaded: { path: string; size: number }[];
  subscribed: string[];
  unsubscribed: string[];
  notify: (() => void) | null;
}

function fakeFileApi(
  overrides: Partial<FileApiClient> = {},
): { api: FileApiClient; calls: Recorder } {
  const calls: Recorder = { listed: [], viewed: [], checked: [], uploaded: [], subscribed: [], unsubscribed: [], notify: null };
  const api: FileApiClient = {
    listProjectFiles: async (_id: string, path: string): Promise<FileListing> => {
      calls.listed.push(path);
      const entries = path === ""
        ? ROOT_ENTRIES
        : path === "docs"
        ? [entry("docs/architecture.md", false, 331)]
        : [];
      return { path, at_root: path === "", entries };
    },
    viewProjectFile: async (_id: string, path: string): Promise<FileView> => {
      calls.viewed.push(path);
      return { content: "alpha\nbeta\n" };
    },
    diffProjectFile: async (): Promise<FileView> => ({ content: "@@ -1 +1 @@\n-alpha\n+beta\n" }),
    subscribeProjectFileEvents: (_id: string, path: string, onChange: () => void): (() => void) => {
      calls.subscribed.push(path);
      calls.notify = onChange;
      return () => calls.unsubscribed.push(path);
    },
    projectFileDownloadUrl: (id: string, path: string): string =>
      `/api/projects/${encodeURIComponent(id)}/files/download?path=${encodeURIComponent(path)}`,
    projectFileImageUrl: (id: string, path: string, version?: string): string =>
      `/api/projects/${encodeURIComponent(id)}/files/image?path=${encodeURIComponent(path)}` +
      (version === undefined ? "" : `&v=${encodeURIComponent(version)}`),
    checkProjectFileImage: async (_id: string, path: string): Promise<void> => {
      calls.checked.push(path);
    },
    uploadProjectFile: async (
      _id: string,
      destPath: string,
      body: Blob,
    ): Promise<FileUploadResult> => {
      calls.uploaded.push({ path: destPath, size: body.size });
      return { path: destPath, size: body.size };
    },
    listProjectAssets: async () => ({ locations: [], scanned: "", preview_max_bytes: 0 }),
    deleteProjectAssets: async () => [],
    subscribeProjectAssetEvents: () => () => {},
    ...overrides,
  };
  return { api, calls };
}

function model(api: FileApiClient): FilesPanelModel {
  return new FilesPanelModel(api, PROJECT, PROJECT_PATH);
}

// A File the model can upload without a browser: the client only reads
// name and passes the body straight through as a Blob.
function pickedFile(name: string, body: string): File {
  return new File([body], name, { type: "text/plain" });
}

function deferred<T>(): {
  promise: Promise<T>;
  resolve: (value: T) => void;
} {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((done) => {
    resolve = done;
  });
  return { promise, resolve };
}

// ---- paths and the root boundary ----

test("confine drops any segment that would climb above the project root", () => {
  assert.equal(confine("docs/api"), "docs/api");
  assert.equal(confine("/docs/api"), "docs/api");
  assert.equal(confine("./docs//api/"), "docs/api");
  assert.equal(confine(".."), "");
  assert.equal(confine("../../etc"), "etc");
  assert.equal(confine("docs/../.."), "");
  assert.equal(confine(""), "");
});

test("the breadcrumb roots at the project directory and offers nothing above it", () => {
  assert.equal(projectRootLabel(PROJECT_PATH), "noisedeck");
  assert.equal(projectPathPrefix(PROJECT_PATH), "/workspace/");
  // A path with no separator is the root itself; there is nothing faint
  // to show above it.
  assert.equal(projectRootLabel("noisedeck"), "noisedeck");
  assert.equal(projectPathPrefix("noisedeck"), "");

  const atRoot = breadcrumbs("noisedeck", "");
  assert.deepEqual(atRoot.map((c) => c.label), ["noisedeck"]);
  assert.equal(atRoot[0]!.current, true);

  const nested = breadcrumbs("noisedeck", "internal/provider");
  assert.deepEqual(nested.map((c) => c.label), ["noisedeck", "internal", "provider"]);
  assert.deepEqual(nested.map((c) => c.path), ["", "internal", "internal/provider"]);
  assert.deepEqual(nested.map((c) => c.current), [false, false, true]);
  assert.equal(nested.some((c) => c.label === ".."), false, "no crumb walks upward");
});

test("navigating never asks the server for a path outside the project", async () => {
  const { api, calls } = fakeFileApi();
  const m = model(api);
  await m.openDirectory("../../etc");
  assert.equal(m.dir, "etc");
  await m.openDirectory("docs/../..");
  assert.equal(m.dir, "");
  assert.deepEqual(calls.listed, ["etc", ""]);
});

test("the drop target names the directory being filled", async () => {
  const { api } = fakeFileApi();
  const m = model(api);
  assert.equal(m.dropTargetLabel(), "noisedeck", "the root reads as the project name");
  await m.openDirectory("internal/provider");
  assert.equal(m.dropTargetLabel(), "provider", "a nested directory reads by its leaf");
});

// ---- activation ----

test("activating a directory descends and drops the open file", async () => {
  const { api, calls } = fakeFileApi();
  const m = model(api);
  await m.refresh();
  await m.activate(entry("README.md"));
  assert.equal(m.selected?.name, "README.md");
  await m.activate(entry("docs", true));
  assert.equal(m.dir, "docs");
  assert.equal(m.selected, null, "the viewer showed a file from the directory we left");
  assert.equal(m.view, null);
  assert.deepEqual(calls.listed, ["", "docs"]);
});

test("activating a file opens it read-only in the viewer", async () => {
  const { api, calls } = fakeFileApi();
  const m = model(api);
  await m.refresh();
  await m.activate(entry("README.md"));
  assert.deepEqual(calls.viewed, ["README.md"]);
  assert.deepEqual(m.view, { kind: "text", content: "alpha\nbeta\n" });
});

test("openFile previews the server's listed entry and honors the activation guard", async () => {
  const { api, calls } = fakeFileApi();
  const m = model(api);
  await m.openFile("docs/architecture.md");
  assert.equal(m.dir, "docs");
  assert.equal(m.selected?.path, "docs/architecture.md");
  assert.deepEqual(m.view, { kind: "text", content: "alpha\nbeta\n" });

  await m.openFile("README.md", () => false);
  assert.equal(m.dir, "", "the requested directory still opens");
  assert.equal(m.selected, null, "a stale handoff cannot select a file");
  assert.deepEqual(calls.viewed, ["docs/architecture.md"]);
});

test("openFile cannot restore a viewer after the panel resets during its listing", async () => {
  const base = fakeFileApi();
  const pending = deferred<FileListing>();
  let delayed = false;
  const api = {
    ...base.api,
    listProjectFiles: (id: string, path: string): Promise<FileListing> =>
      delayed ? pending.promise : base.api.listProjectFiles(id, path),
  };
  const m = model(api);
  await m.openFile("docs/architecture.md");
  assert.equal(m.selected?.path, "docs/architecture.md");

  delayed = true;
  const work = m.openFile("docs/architecture.md");
  m.reset();
  pending.resolve({ path: "docs", at_root: false, entries: [entry("docs/architecture.md")] });
  await work;

  assert.equal(m.selected, null);
  assert.equal(m.view, null);
  assert.deepEqual(base.calls.viewed, ["docs/architecture.md"], "the stale open never reads the file again");
});

test("viewer mode switches between the file and its last-commit changes", async () => {
  const { api } = fakeFileApi();
  const m = model(api);
  await m.activate({ ...entry("README.md"), change: "modified" });
  assert.deepEqual(m.view, { kind: "text", content: "alpha\nbeta\n" });
  await m.toggleViewerMode();
  assert.equal(m.viewerMode, "changes");
  assert.deepEqual(m.view, { kind: "diff", content: "@@ -1 +1 @@\n-alpha\n+beta\n" });
  await m.toggleViewerMode();
  assert.equal(m.viewerMode, "file");
  assert.deepEqual(m.view, { kind: "text", content: "alpha\nbeta\n" });
});

test("changes mode refreshes after the file is committed and retains an exit", async () => {
  let reads = 0;
  const { api, calls } = fakeFileApi({
    diffProjectFile: async () => ({ content: reads++ === 0 ? "@@ -1 +1 @@\n-alpha\n+beta\n" : "" }),
  });
  const m = model(api);
  await m.open();
  await m.activate({ ...entry("README.md"), change: "modified" });
  await m.toggleViewerMode();
  calls.notify?.();
  await new Promise((resolve) => setTimeout(resolve, 0));
  assert.equal(m.selected?.change, undefined);
  assert.equal(reads, 2, "a new commit baseline revalidates the diff");
  assert.deepEqual(m.view, { kind: "diff", content: "" });
  await m.toggleViewerMode();
  assert.equal(m.viewerMode, "file");
  assert.deepEqual(m.view, { kind: "text", content: "alpha\nbeta\n" });
});

test("a partial commit revalidates the diff even when the file stays modified", async () => {
  let head = "one";
  let reads = 0;
  const { api, calls } = fakeFileApi({
    listProjectFiles: async (_id, path) => ({
      path, at_root: path === "", entries: [{ ...entry("README.md"), change: "modified" }], head,
    }),
    diffProjectFile: async () => ({ content: reads++ === 0 ? "before partial" : "after partial" }),
  });
  const m = model(api);
  await m.open();
  await m.activate({ ...entry("README.md"), change: "modified" });
  await m.toggleViewerMode();
  head = "two";
  calls.notify?.();
  await new Promise((resolve) => setTimeout(resolve, 0));
  assert.equal(m.selected?.change, "modified");
  assert.equal(reads, 2, "a changed HEAD revalidates the diff");
  assert.deepEqual(m.view, { kind: "diff", content: "after partial" });
});

test("closing the viewer fences a late view response", async () => {
  const pending = deferred<FileView>();
  const { api } = fakeFileApi({ viewProjectFile: async () => pending.promise });
  const m = model(api);
  const work = m.activate(entry("README.md"));
  assert.equal(m.view?.kind, "loading");

  m.closeViewer();
  pending.resolve({ content: "late" });
  await work;

  assert.equal(m.selected, null);
  assert.equal(m.view, null);
});

// ---- listing states ----

test("a failed listing shows the error and no rows", async () => {
  const { api } = fakeFileApi({
    listProjectFiles: () => Promise.reject(new ApiError(403, "outside_project", "path is outside the project")),
  });
  const m = model(api);
  await m.refresh();
  assert.equal(m.listError, "path is outside the project");
  assert.deepEqual(m.entries(), [], "stale rows must not stand under a failure");
});

test("opening subscribes and reset unsubscribes from directory events", async () => {
  const { api, calls } = fakeFileApi();
  const m = model(api);
  await m.open();
  assert.deepEqual(calls.subscribed, [""]);
  m.reset();
  assert.deepEqual(calls.unsubscribed, [""]);
});

test("a background event re-lists quietly and keeps the selection", async () => {
  let entries = ROOT_ENTRIES;
  const { api, calls } = fakeFileApi({
    listProjectFiles: async (_id, path) => {
      calls.listed.push(path);
      return { path, at_root: path === "", entries };
    },
  });
  const m = model(api);
  await m.open();
  await m.activate(entry("README.md"));
  const listedBeforeEvent = calls.listed.length;
  entries = [entry("docs", true), entry("README.md", false, 999, "2026-08-03T11:00:00Z")];
  calls.notify?.();
  await Promise.resolve();
  await Promise.resolve();
  assert.equal(m.loading, false);
  assert.equal(m.selected?.path, "README.md");
  assert.equal(calls.listed.length, listedBeforeEvent + 1, "the event triggers one new listing");
  assert.equal(m.entries().find((e) => e.path === "README.md")?.size, 999);
  assert.equal(m.selected?.path, "README.md", "the selected path survives the re-list");
});

test("a missing directory during a quiet refresh walks up to its parent", async () => {
  let nestedReads = 0;
  let renders = 0;
  const { api, calls } = fakeFileApi({
    listProjectFiles: async (_id, path) => {
      if (path === "a/b") {
        nestedReads += 1;
        if (nestedReads > 1) throw new ApiError(404, "not_found", "directory disappeared");
      }
      return { path, at_root: path === "", entries: [] };
    },
  });
  const m = new FilesPanelModel(api, PROJECT, PROJECT_PATH, { onChange: () => ++renders });
  await m.open();
  await m.openDirectory("a/b");
  calls.notify?.();
  await new Promise((resolve) => setTimeout(resolve, 0));
  assert.equal(m.dir, "a");
  assert.equal(m.listError, null);
  assert.equal(renders, 1, "walking up repaints the parent directory");
});

test("navigating drops the previous directory's rows and error before the new listing lands", async () => {
  const pending = deferred<FileListing>();
  let lists = 0;
  const { api } = fakeFileApi({
    listProjectFiles: async (_id, path) => {
      lists += 1;
      if (lists === 1) return { path, at_root: false, entries: [entry("a/kept.md")] };
      if (lists === 2) throw new ApiError(500, "internal", "server restarting");
      return pending.promise;
    },
  });
  const m = model(api);
  await m.openDirectory("a");
  // A quiet failure is what leaves rows and an error live at the same time:
  // the listing path keeps its last good rows and reports the failure.
  await m.refresh(true);
  assert.deepEqual(m.entries().map((item) => item.path), ["a/kept.md"]);
  assert.equal(m.listError, "server restarting");

  // The third listing never settles, so this is the window the user
  // actually sees while a directory opens. Neither the rows nor the error
  // from the directory being left may still be on screen in it.
  const opening = m.openDirectory("docs");
  assert.equal(m.listing, null, "the previous directory's rows are gone");
  assert.equal(m.listError, null, "so is its error");
  pending.resolve({ path: "docs", at_root: false, entries: [] });
  await opening;
});

test("opening a directory replaces the previous file event subscription", async () => {
  const { api, calls } = fakeFileApi();
  const m = model(api);
  await m.open();
  await m.openDirectory("docs");
  assert.deepEqual(calls.unsubscribed, [""]);
  assert.deepEqual(calls.subscribed, ["", "docs"]);
});

test("a quiet refresh landing during a foreground refresh does not stop its spinner", async () => {
  const foreground = deferred<FileListing>();
  let lists = 0;
  const { api } = fakeFileApi({
    listProjectFiles: async (_id, path) => {
      lists += 1;
      if (lists === 1) return foreground.promise;
      return { path, at_root: path === "", entries: [] };
    },
  });
  const m = model(api);
  const foregroundRefresh = m.refresh();
  assert.equal(m.loading, true);
  await m.refresh(true);
  assert.equal(m.loading, true);
  foreground.resolve({ path: "", at_root: true, entries: [] });
  await foregroundRefresh;
  assert.equal(m.loading, false);
});

test("a server failure during a quiet refresh stays put and surfaces the error", async () => {
  let nestedReads = 0;
  const { api } = fakeFileApi({
    listProjectFiles: async (_id, path) => {
      if (path === "a/b") {
        nestedReads += 1;
        if (nestedReads > 1) throw new ApiError(500, "internal", "server restarting");
      }
      return { path, at_root: path === "", entries: [entry(`${path}/kept.md`)] };
    },
  });
  const m = model(api);
  await m.openDirectory("a/b");
  await m.refresh(true);
  assert.equal(m.dir, "a/b");
  assert.equal(m.listError, "server restarting");
  assert.deepEqual(m.entries().map((item) => item.path), ["a/b/kept.md"]);
});

test("a network failure during a quiet refresh stays put and surfaces the error", async () => {
  let nestedReads = 0;
  const { api } = fakeFileApi({
    listProjectFiles: async (_id, path) => {
      if (path === "a/b") {
        nestedReads += 1;
        if (nestedReads > 1) throw new Error("connection lost");
      }
      return { path, at_root: path === "", entries: [] };
    },
  });
  const m = model(api);
  await m.openDirectory("a/b");
  await m.refresh(true);
  assert.equal(m.dir, "a/b");
  assert.equal(m.listError, "connection lost");
});

test("an empty directory is a state, not a failure", async () => {
  const { api } = fakeFileApi();
  const m = model(api);
  await m.openDirectory("internal");
  assert.equal(m.listError, null);
  assert.deepEqual(m.entries(), []);
});

test("a truncated listing is reported as truncated", async () => {
  const { api } = fakeFileApi({
    listProjectFiles: async () => ({
      path: "",
      at_root: true,
      entries: [entry("a.txt")],
      truncated: true,
    }),
  });
  const m = model(api);
  await m.refresh();
  assert.equal(m.truncated(), true);
});

test("the rows keep the server's order, which already puts directories first", async () => {
  const { api } = fakeFileApi();
  const m = model(api);
  await m.refresh();
  assert.deepEqual(m.entries().map((e) => e.path), ["docs", "README.md", "logo.png"]);
});

test("sorting reorders by the chosen column, keeping directories grouped first", async () => {
  const { api } = fakeFileApi({
    listProjectFiles: async () => ({
      path: "",
      at_root: true,
      entries: [
        entry("docs", true, 4096),
        entry("a.txt", false, 10),
        entry("README.md", false, 268),
        entry("logo.png", false, 18422),
      ],
    }),
  });
  const m = model(api);
  await m.refresh();
  assert.deepEqual(m.sortedEntries().map((e) => e.path), ["docs", "a.txt", "README.md", "logo.png"],
    "name-asc is the default and matches directories-first, name order");

  m.setSort("size");
  assert.deepEqual(m.sortedEntries().map((e) => e.path), ["docs", "a.txt", "README.md", "logo.png"],
    "directories stay grouped first within a size sort");

  m.setSort("size");
  assert.equal(m.sort.direction, "desc", "re-selecting the active column flips direction");
  assert.deepEqual(m.sortedEntries().map((e) => e.path), ["docs", "logo.png", "README.md", "a.txt"],
    "directories stay grouped first even descending");

  m.setSort("modified");
  assert.deepEqual(
    m.sortedEntries().map((e) => e.path),
    ["docs", "a.txt", "logo.png", "README.md"],
    "a new column restarts ascending; equal mtimes are tie-broken by name order",
  );
  assert.equal(m.sort.field, "modified");
  assert.equal(m.sort.direction, "asc");

  m.setSort("name");
  m.setSort("name");
  assert.equal(m.sort.direction, "desc", "a second click on the active column flips");
  assert.deepEqual(m.sortedEntries().map((e) => e.path), ["docs", "README.md", "logo.png", "a.txt"],
    "name descending orders within each group, directories still first");
});

test("refresh clears a selection that no longer exists in the directory", async () => {
  let entries = ROOT_ENTRIES;
  const { api } = fakeFileApi({
    listProjectFiles: async () => ({ path: "", at_root: true, entries }),
  });
  const m = model(api);
  await m.refresh();
  await m.activate(entry("README.md"));

  entries = [entry("docs", true)];
  await m.refresh();

  assert.equal(m.selected, null);
  assert.equal(m.view, null);
});

// ---- the viewer's refusals ----

test("bytes the server will not decode become the no-preview state, never garbage", async () => {
  const { api } = fakeFileApi({
    viewProjectFile: () => Promise.reject(new ApiError(415, "not_text", "file is not text")),
  });
  const m = model(api);
  await m.activate(entry("scan.tiff", false, 18422));
  assert.deepEqual(m.view, { kind: "no-preview", reason: "" });
});

test("a file past the viewer's bound says why and still offers the download", async () => {
  const { api } = fakeFileApi({
    viewProjectFile: () =>
      Promise.reject(new ApiError(413, "too_large", "file is too large to view: 4194304 bytes, limit 1048576")),
  });
  const m = model(api);
  await m.activate(entry("dump.log", false, 4194304));
  assert.equal(m.view?.kind, "no-preview");
  assert.match((m.view as { reason: string }).reason, /too large/);
});

test("a transient failure on a quiet re-read keeps the text the user is reading", async () => {
  let reads = 0;
  let lists = 0;
  const errors: string[] = [];
  const { api, calls } = fakeFileApi({
    listProjectFiles: async (_id, path) => {
      calls.listed.push(path);
      lists += 1;
      // The file is rewritten between the two listings, which is what sends
      // the panel back for its quiet re-read.
      return { path, at_root: path === "", entries: [entry("README.md", false, 12, `2026-08-0${lists}T11:00:00Z`)] };
    },
    viewProjectFile: async (_id, path) => {
      calls.viewed.push(path);
      reads += 1;
      if (reads > 1) throw new ApiError(500, "internal", "server restarting");
      return { content: "alpha\nbeta\n" };
    },
  });
  const m = new FilesPanelModel(api, PROJECT, PROJECT_PATH, { onError: (message) => errors.push(message) });
  await m.open();
  const readme = m.entries()[0];
  assert.ok(readme !== undefined);
  await m.activate(readme);
  assert.deepEqual(m.view, { kind: "text", content: "alpha\nbeta\n" });

  // The file changed, so the background refresh re-reads it — and the read
  // fails. The user asked for none of this and is still reading.
  calls.notify?.();
  await new Promise((resolve) => setTimeout(resolve, 0));
  assert.deepEqual(m.view, { kind: "text", content: "alpha\nbeta\n" }, "the last good text stays on screen");
  assert.deepEqual(errors, ["server restarting"], "the failure is still reported");
});

test("a quiet re-read that the server refuses replaces the text, because that is a real state", async () => {
  let reads = 0;
  const { api } = fakeFileApi({
    viewProjectFile: async () => {
      reads += 1;
      if (reads > 1) throw new ApiError(415, "not_text", "file is not text");
      return { content: "alpha\nbeta\n" };
    },
  });
  const m = model(api);
  const readme = entry("README.md");
  await m.activate(readme);
  await m.loadView(readme, true);
  assert.deepEqual(m.view, { kind: "no-preview", reason: "" }, "the file really is binary now");
});

test("any other view failure surfaces the server's message", async () => {
  const { api } = fakeFileApi({
    viewProjectFile: () => Promise.reject(new ApiError(500, "internal", "read failed")),
  });
  const m = model(api);
  await m.activate(entry("README.md"));
  assert.deepEqual(m.view, { kind: "error", message: "read failed" });
});

// ---- metadata voices ----

test("a row's git marker is the listing's words, and nothing when unreported", () => {
  assert.equal(rowChange({ ...entry("a.go"), change: "modified" }), "Modified");
  assert.equal(rowChange({ ...entry("src", true), change: "modified" }), "Modified");
  assert.equal(rowChange({ ...entry("b.go"), change: "new" }), "New");
  assert.equal(rowChange(entry("c.go")), "");
});

test("row and viewer metadata state only what the listing reported", () => {
  assert.equal(rowName(entry("docs", true)), "docs/");
  assert.equal(rowName(entry("README.md")), "README.md");
  assert.equal(rowMeta(entry("README.md", false, 268)), "0.3 KB");
  assert.equal(rowMeta(entry("docs", true, 4096)), "", "the listing reports no child count");
  const now = Date.parse("2026-08-02T12:00:00Z");
  assert.equal(
    viewerStatus(entry("README.md", false, 2048, "2026-07-31T12:00:00Z"), null, now),
    "markdown · 2.0 KB · modified 2d ago · read-only",
  );
  assert.equal(
    viewerStatus(
      entry("internal/provider/descriptor.go", false, 640, "2026-07-31T12:00:00Z"),
      { kind: "text", content: "one\ntwo\n" },
      now,
    ),
    "go · 3 lines · 0.6 KB · modified 2d ago · read-only",
    "decoded text adds the line count; the kind comes from the extension",
  );
  assert.equal(
    viewerStatus({ ...entry("README.md", false, 2048), mtime: "" }, null, now),
    "markdown · 2.0 KB · read-only",
    "an unreadable timestamp shows nothing rather than a guess",
  );
});

test("the footer names the file's kind and the binary state's extension", () => {
  assert.equal(kindOfName("README.md"), "markdown");
  assert.equal(kindOfName("descriptor.go"), "go");
  assert.equal(kindOfName("go.sum"), "go checksums");
  assert.equal(kindOfName("notes.txt"), "text");
  assert.equal(kindOfName("archive.tar.gz"), "gz", "an unknown suffix is named verbatim");
  assert.equal(kindOfName("Makefile"), "text", "an extensionless name is plain text");
  assert.equal(fileExtension("logo.png"), ".png");
  assert.equal(fileExtension("LICENSE"), "these", "no extension reads as 'these files'");
});

// ---- upload ----

test("a multi-file pick uploads each file to the current directory under its own name", async () => {
  const { api, calls } = fakeFileApi();
  const m = model(api);
  await m.openDirectory("docs");
  await m.upload([pickedFile("one.md", "a"), pickedFile("two.md", "bb")]);
  assert.deepEqual(calls.uploaded, [
    { path: "docs/one.md", size: 1 },
    { path: "docs/two.md", size: 2 },
  ]);
  assert.deepEqual(m.uploadErrors, []);
  assert.equal(calls.listed.at(-1), "docs", "the listing is re-read from the server");
});

test("an upload keeps only the chosen file's basename", async () => {
  assert.equal(uploadBasename("dir/sub/logo.png"), "logo.png");
  assert.equal(uploadBasename("..\\..\\evil.sh"), "evil.sh");
  assert.equal(uploadBasename(".."), "");
  const { api, calls } = fakeFileApi();
  const m = model(api);
  await m.upload([pickedFile("nested/dir/logo.png", "x")]);
  assert.deepEqual(calls.uploaded.map((u) => u.path), ["logo.png"]);
});

test("a failed file is named and never reported as stored", async () => {
  const attempted: string[] = [];
  const { api, calls } = fakeFileApi({
    uploadProjectFile: async (_id: string, destPath: string): Promise<FileUploadResult> => {
      attempted.push(destPath);
      if (destPath === "big.bin") {
        throw new ApiError(413, "too_large", "upload is too large");
      }
      return { path: destPath, size: 1 };
    },
  });
  const m = model(api);
  await m.upload([pickedFile("big.bin", "x"), pickedFile("ok.md", "y")]);
  assert.deepEqual(m.uploadErrors, ["big.bin: upload is too large"]);
  assert.equal(m.uploading, false);
  // One failure does not abandon the rest of the pick, and the listing is
  // re-read either way: what the panel shows next is the server's answer.
  assert.deepEqual(attempted, ["big.bin", "ok.md"]);
  assert.equal(calls.listed.at(-1), "");
});

test("an upload that lands moves no selection: the viewer the user left open stays open", async () => {
  const stored: string[] = [];
  const { api } = fakeFileApi({
    listProjectFiles: async (_id, path): Promise<FileListing> => ({
      path,
      at_root: path === "",
      entries: [...ROOT_ENTRIES, ...stored.map((p) => entry(p))],
    }),
    uploadProjectFile: async (_id, destPath): Promise<FileUploadResult> => {
      stored.push(destPath);
      return { path: destPath, size: 4 };
    },
  });
  const m = model(api);
  await m.refresh();
  await m.activate(entry("README.md"));
  const openPath = m.selected?.path;
  const openView = m.view;
  await m.upload([pickedFile("logo.png", "png!")]);
  assert.equal(m.selected?.path, openPath, "completion refreshes the listing without selecting the landed file");
  assert.equal(m.view, openView, "the viewer content the user left open stays open");
  assert.ok(
    m.uploadRecords.every((record) => record.status !== "complete"),
    "the completed record leaves once the refreshed listing names it",
  );
  assert.ok(m.entries().some((e) => e.path === "logo.png"), "the landed file is an ordinary row");
});

test("an upload the listing does not confirm selects nothing", async () => {
  const { api } = fakeFileApi({
    uploadProjectFile: async (): Promise<FileUploadResult> => ({ path: "ghost.md", size: 4 }),
  });
  const m = model(api);
  await m.upload([pickedFile("ghost.md", "text")]);
  assert.equal(m.selected, null);
});

test("closing while an upload is pending cannot restore the cleared viewer", async () => {
  const pending = deferred<FileUploadResult>();
  const { api, calls } = fakeFileApi({
    uploadProjectFile: async () => pending.promise,
  });
  const m = model(api);
  const work = m.upload([pickedFile("late.txt", "late")]);

  m.reset();
  pending.resolve({ path: "late.txt", size: 4 });
  await work;

  assert.equal(m.selected, null);
  assert.equal(m.view, null);
  assert.deepEqual(m.uploadErrors, []);
  assert.deepEqual(calls.listed, [], "a closed panel does not start a late refresh");
});

test("a multi-file pick keeps its original directory while navigation stays live", async () => {
  const first = deferred<FileUploadResult>();
  const uploaded: string[] = [];
  const { api } = fakeFileApi({
    uploadProjectFile: async (_id, path, body) => {
      uploaded.push(path);
      if (uploaded.length === 1) return first.promise;
      return { path, size: body.size };
    },
  });
  const m = model(api);
  const work = m.upload([
    pickedFile("a.txt", "a"),
    pickedFile("b.txt", "b"),
  ]);

  assert.deepEqual(uploaded, ["a.txt"], "the first request is pending");
  await m.openDirectory("docs");
  first.resolve({ path: "a.txt", size: 1 });
  await work;

  assert.deepEqual(uploaded, ["a.txt", "b.txt"]);
  assert.equal(m.dir, "docs");
  assert.equal(m.selected, null, "the root upload is not selected in another directory");
});

// ---- upload conflicts ----

// refusal builds the ApiError the client throws for the endpoint's 409:
// the ask exactly as the server's envelope decodes it.
function refusal(
  version: string,
  overrides: Partial<UploadConflictInfo["existing"]> = {},
  reason: UploadConflictReason = "exists",
): ApiError {
  const info: UploadConflictInfo = {
    reason,
    message: "README.md: destination already exists",
    existing: {
      name: "README.md",
      dir: false,
      size: 268,
      mtime: "2026-08-02T11:00:00Z",
      version,
      ...overrides,
    },
  };
  return new ApiError(409, "conflict", info.message, info);
}

test("a suggested alternative keeps the extension and skips every taken number", () => {
  assert.deepEqual(splitFileName("prd.md"), { base: "prd", ext: ".md" });
  assert.deepEqual(splitFileName(".env"), { base: ".env", ext: "" });
  assert.deepEqual(splitFileName("Makefile"), { base: "Makefile", ext: "" });
  assert.equal(suggestUploadName("prd.md", ["prd.md"]), "prd-2.md");
  assert.equal(suggestUploadName("prd.md", ["prd.md", "prd-2.md"]), "prd-3.md");
  assert.equal(suggestUploadName("logo.tar.gz", ["logo.tar.gz"]), "logo.tar-2.gz");
  assert.equal(suggestUploadName(".env", [".env"]), ".env-2");
});

test("a colliding upload is held and asked about with the server's facts, not an error", async () => {
  const { api } = fakeFileApi({
    uploadProjectFile: async (_id, destPath): Promise<FileUploadResult> => {
      if (destPath === "README.md") throw refusal("v1");
      return { path: destPath, size: 1 };
    },
  });
  const m = model(api);
  await m.open();
  await m.upload([pickedFile("README.md", "new"), pickedFile("clean.md", "ok")]);
  assert.equal(m.uploading, false, "nothing is in flight while the ask stands");
  assert.deepEqual(m.uploadErrors, [], "the refusal is a question, not a failure");
  assert.ok(m.conflict !== null);
  assert.equal(m.conflict.info.existing.version, "v1");
  assert.equal(m.conflictIncomingName(), "README.md");
  assert.equal(m.conflictName, "README-2.md", "the ask opens on the first free suggestion");
  assert.deepEqual(m.conflictPosition(), { current: 1, total: 1 });
  assert.equal(m.conflictDirLabel(), "noisedeck");
});

test("Replace sends the version the conflict reported and lands", async () => {
  const consents: (string | undefined)[] = [];
  const { api, calls } = fakeFileApi({
    uploadProjectFile: async (_id, destPath, _body, consent): Promise<FileUploadResult> => {
      consents.push(consent?.version);
      if (consent === undefined) throw refusal("v1");
      return { path: destPath, size: 3 };
    },
  });
  const m = model(api);
  await m.open();
  await m.upload([pickedFile("README.md", "new")]);
  await m.replaceConflict();
  await new Promise((resolve) => setTimeout(resolve, 0));
  assert.deepEqual(consents, [undefined, "v1"], "the second request carries the approved version");
  assert.equal(m.conflict, null);
  assert.equal(m.selected, null, "landing a replacement moves no selection");
  assert.ok(calls.listed.length > 1, "the batch ends by reading the server back");
  assert.ok(
    m.uploadRecords.every((record) => record.status !== "complete"),
    "the completed replacement record leaves with the refresh",
  );
});

test("a changed target is re-asked with fresh facts and never destroyed on the stale approval", async () => {
  const consents: (string | undefined)[] = [];
  const { api } = fakeFileApi({
    uploadProjectFile: async (_id, destPath, _body, consent): Promise<FileUploadResult> => {
      consents.push(consent?.version);
      if (consent === undefined) throw refusal("v1");
      if (consent?.version === "v1") throw refusal("v2", { size: 999, mtime: "2026-09-23T01:00:00Z" }, "changed");
      return { path: destPath, size: 3 };
    },
  });
  const m = model(api);
  await m.open();
  await m.upload([pickedFile("README.md", "new")]);
  await m.replaceConflict();
  assert.ok(m.conflict !== null, "the changed refusal puts the question back");
  assert.equal(m.conflict.reconfirmed, true);
  assert.equal(m.conflict.info.existing.version, "v2");
  assert.equal(m.conflict.info.existing.size, 999);
  await m.replaceConflict();
  await new Promise((resolve) => setTimeout(resolve, 0));
  assert.deepEqual(consents, [undefined, "v1", "v2"], "reconfirmation is a re-ask, not a dead end");
  assert.equal(m.conflict, null);
});

test("a folder or a link holding the name is never replaceable, consent or not", async () => {
  const sent: string[] = [];
  const { api } = fakeFileApi({
    uploadProjectFile: async (_id, destPath): Promise<FileUploadResult> => {
      sent.push(destPath);
      throw refusal("v1", { dir: true }, "directory");
    },
  });
  const m = model(api);
  await m.open();
  await m.upload([pickedFile("docs", "x")]);
  assert.ok(m.conflict !== null);
  assert.equal(m.conflict.info.reason, "directory");
  await m.replaceConflict();
  assert.deepEqual(sent, ["docs"], "no consented request is sent for a folder");
  assert.ok(m.conflict !== null, "the ask stands");
});

test("Cancel drops only the current file; the next ask still stands", async () => {
  const { api } = fakeFileApi({
    uploadProjectFile: async (_id, destPath): Promise<FileUploadResult> => {
      throw refusal(`${destPath}-v1`, { name: destPath });
    },
  });
  const m = model(api);
  await m.open();
  await m.upload([pickedFile("README.md", "new"), pickedFile("logo.png", "new")]);
  assert.deepEqual(m.conflictPosition(), { current: 1, total: 2 });
  assert.equal(m.conflictRemaining(), 1);
  m.cancelConflict();
  assert.ok(m.conflict !== null);
  assert.equal(m.conflictIncomingName(), "logo.png", "the second file is asked after the first is cancelled");
  assert.deepEqual(m.conflictPosition(), { current: 2, total: 2 });
  assert.equal(m.conflictRemaining(), 0);
  m.cancelConflict();
  assert.equal(m.conflict, null);
  await new Promise((resolve) => setTimeout(resolve, 0));
  assert.equal(m.selected, null, "nothing landed, nothing is selected");
});

test("same-choice Replace applies to the remaining conflicts, each validated at commit", async () => {
  const consents: (string | undefined)[] = [];
  const { api } = fakeFileApi({
    uploadProjectFile: async (_id, destPath, _body, consent): Promise<FileUploadResult> => {
      consents.push(consent?.version);
      if (consent === undefined) throw refusal(`${destPath}-v1`, { name: destPath });
      return { path: destPath, size: 3 };
    },
  });
  const m = model(api);
  await m.open();
  await m.upload([pickedFile("README.md", "new"), pickedFile("logo.png", "new")]);
  m.sameChoice = true;
  await m.replaceConflict();
  await new Promise((resolve) => setTimeout(resolve, 0));
  assert.deepEqual(
    consents,
    [undefined, undefined, "README.md-v1", "logo.png-v1"],
    "each file carries its own version to commit",
  );
  assert.equal(m.conflict, null);
});

test("same-choice Replace stops at a changed target and re-asks it, never destroys it", async () => {
  const consents: (string | undefined)[] = [];
  const { api } = fakeFileApi({
    uploadProjectFile: async (_id, destPath, _body, consent): Promise<FileUploadResult> => {
      consents.push(consent?.version);
      if (consent === undefined) throw refusal(`${destPath}-v1`, { name: destPath });
      if (destPath === "logo.png") throw refusal("logo-v2", { name: "logo.png" }, "changed");
      return { path: destPath, size: 3 };
    },
  });
  const m = model(api);
  await m.open();
  await m.upload([pickedFile("README.md", "new"), pickedFile("logo.png", "new")]);
  m.sameChoice = true;
  await m.replaceConflict();
  assert.ok(m.conflict !== null, "the changed file comes back as an ask");
  assert.equal(m.conflict.info.reason, "changed");
  assert.deepEqual(consents, [undefined, undefined, "README.md-v1", "logo.png-v1"]);
  assert.equal(m.confirming, false);
});

test("Cancel never reads the same-choice box, so Escape cannot skip the queue unasked", async () => {
  const { api } = fakeFileApi({
    uploadProjectFile: async (_id, destPath): Promise<FileUploadResult> => {
      throw refusal(`${destPath}-v1`, { name: destPath });
    },
  });
  const m = model(api);
  await m.open();
  await m.upload([pickedFile("README.md", "new"), pickedFile("logo.png", "new")]);
  m.sameChoice = true;
  m.cancelConflict();
  assert.ok(m.conflict !== null, "the remaining file is still asked after a cancelled one");
  assert.equal(m.conflictIncomingName(), "logo.png");
});

test("two files of one name in one pick collide on disk and get different alternatives", async () => {
  const { api } = fakeFileApi({
    uploadProjectFile: async (_id, destPath): Promise<FileUploadResult> => {
      if (destPath === "notes.txt") throw refusal("disk-v1");
      return { path: destPath, size: 3 };
    },
  });
  const m = model(api);
  await m.open();
  await m.upload([pickedFile("notes.txt", "first"), pickedFile("notes.txt", "second")]);
  assert.ok(m.conflict !== null);
  assert.equal(m.conflict.info.existing.version, "disk-v1", "the collision is with what the server holds");
  assert.equal(m.conflictName, "notes-2.txt", "the alternative skips the name the first file took");
});

test("Upload as sends under the edited name with no consent and records it taken", async () => {
  const sent: { path: string; version?: string }[] = [];
  const stored: string[] = [];
  const { api } = fakeFileApi({
    listProjectFiles: async (_id, path): Promise<FileListing> => ({
      path,
      at_root: path === "",
      // The listing reads back what this fake actually stored, the way
      // the server's does.
      entries: [...ROOT_ENTRIES, ...stored.map((p) => entry(p))],
    }),
    uploadProjectFile: async (_id, destPath, _body, consent): Promise<FileUploadResult> => {
      sent.push({ path: destPath, version: consent?.version });
      if (consent === undefined && destPath === "README.md") throw refusal("v1");
      stored.push(destPath);
      return { path: destPath, size: 3 };
    },
  });
  const m = model(api);
  await m.open();
  await m.upload([pickedFile("README.md", "new"), pickedFile("README-2.md", "other")]);
  assert.equal(m.conflictName, "README-3.md", "the suggestion also skips other names in the pick");
  m.setConflictName("intro.md");
  await m.confirmUploadAs();
  await new Promise((resolve) => setTimeout(resolve, 0));
  assert.deepEqual(sent, [
    { path: "README.md", version: undefined },
    { path: "README-2.md", version: undefined },
    { path: "intro.md", version: undefined },
  ], "the alternative lands without consent");
  assert.equal(m.conflict, null);
  assert.equal(m.selected, null, "landing the alternative moves no selection");
  assert.ok(
    m.entries().some((e) => e.path === "intro.md"),
    "the landed alternative is an ordinary row in the refreshed listing",
  );
});

test("Upload as refuses a taken or malformed name without sending anything", async () => {
  const sent: string[] = [];
  const { api } = fakeFileApi({
    uploadProjectFile: async (_id, destPath): Promise<FileUploadResult> => {
      sent.push(destPath);
      throw refusal("v1");
    },
  });
  const m = model(api);
  await m.open();
  await m.upload([pickedFile("README.md", "new")]);
  m.setConflictName("README.md");
  await m.confirmUploadAs();
  assert.equal(m.conflictNameError, "That name is taken too.");
  m.setConflictName("sub/dir.md");
  await m.confirmUploadAs();
  assert.equal(m.conflictNameError, "Names can't contain /.");
  m.setConflictName("");
  await m.confirmUploadAs();
  assert.equal(m.conflictNameError, "Enter a name.");
  assert.deepEqual(sent, ["README.md"], "none of the refusals sent a request");
});

test("a refused alternative keeps Replace bound to the original named file", async () => {
  const sent: { path: string; version?: string }[] = [];
  const { api } = fakeFileApi({
    uploadProjectFile: async (_id, destPath, _body, consent): Promise<FileUploadResult> => {
      sent.push({ path: destPath, version: consent?.version });
      if (destPath === "intro.md") throw refusal("taker-v1", { name: "intro.md" });
      if (destPath === "README.md" && consent === undefined) throw refusal("v1");
      return { path: destPath, size: 3 };
    },
  });
  const m = model(api);
  await m.open();
  await m.upload([pickedFile("README.md", "new")]);
  m.setConflictName("intro.md");
  await m.confirmUploadAs();
  assert.ok(m.conflict !== null);
  assert.equal(m.conflictNameError, "That name is taken too.");
  assert.equal(m.conflictName, "intro.md");
  assert.equal(m.conflictIncomingName(), "README.md");
  assert.equal(m.conflict.info.existing.name, "README.md");
  assert.equal(m.conflict.info.existing.version, "v1");
  assert.equal(m.conflict.reconfirmed, false, "rename refusal is not a changed replacement");
  await m.replaceConflict();
  assert.deepEqual(sent, [
    { path: "README.md", version: undefined },
    { path: "intro.md", version: undefined },
    { path: "README.md", version: "v1" },
  ]);
});

test("same-choice Replace stops before a folder or non-regular target", async () => {
  for (const reason of ["directory", "not_a_file"] as const) {
    const sent: { path: string; version?: string }[] = [];
    const { api } = fakeFileApi({
      uploadProjectFile: async (_id, path, _body, consent) => {
        sent.push({ path, version: consent?.version });
        if (consent === undefined) {
          throw refusal(`${path}-v1`, { name: path, dir: path === "docs" && reason === "directory" }, path === "docs" ? reason : "exists");
        }
        return { path, size: 3 };
      },
    });
    const m = model(api);
    await m.open();
    await m.upload([pickedFile("README.md", "new"), pickedFile("docs", "new"), pickedFile("later.txt", "new")]);
    m.sameChoice = true;
    await m.replaceConflict();
    assert.equal(m.conflict?.info.existing.name, "docs");
    assert.equal(m.conflict?.reconfirmed, false);
    assert.deepEqual(sent.filter((call) => call.version !== undefined), [{ path: "README.md", version: "README.md-v1" }]);
  }
});

test("a new pick clears the preceding pick's errors", async () => {
  const { api } = fakeFileApi({
    uploadProjectFile: async (_id, path) => {
      if (path === "bad.md") throw new Error("connection unavailable");
      return { path, size: 3 };
    },
  });
  const m = model(api);
  await m.open();
  await m.upload([pickedFile("bad.md", "new")]);
  assert.equal(m.uploadErrors.length, 1);
  assert.match(m.uploadErrors[0]!, /outcome unknown/);
  assert.doesNotMatch(m.uploadErrors[0]!, /file was sent/);
  await m.upload([pickedFile("good.md", "new")]);
  assert.deepEqual(m.uploadErrors, []);
});

test("same-choice Upload as stops on a refused alternative without repeating it", async () => {
  const sent: string[] = [];
  const { api } = fakeFileApi({
    uploadProjectFile: async (_id, path) => {
      sent.push(path);
      if (sent.length > 3) throw new Error("unexpected extra request");
      throw refusal(`${path}-v1`, { name: path });
    },
  });
  const m = model(api);
  await m.open();
  await m.upload([pickedFile("README.md", "new"), pickedFile("later.txt", "new")]);
  m.sameChoice = true;
  m.setConflictName("intro.md");
  await m.confirmUploadAs();
  assert.deepEqual(sent, ["README.md", "later.txt", "intro.md"]);
  assert.equal(m.conflictIncomingName(), "README.md");
  assert.equal(m.conflict?.reconfirmed, false);
  assert.equal(m.conflictNameError, "That name is taken too.");
});

test("a lost response is an unknown outcome, never a claim of failure or a retry", async () => {
  let attempts = 0;
  const { api } = fakeFileApi({
    uploadProjectFile: async (_id, _destPath, _body, consent): Promise<FileUploadResult> => {
      attempts++;
      if (consent === undefined) throw refusal("v1");
      throw new TypeError("fetch failed");
    },
  });
  const m = model(api);
  await m.open();
  await m.upload([pickedFile("README.md", "new")]);
  await m.replaceConflict();
  await new Promise((resolve) => setTimeout(resolve, 0));
  assert.equal(attempts, 2, "the lost replace is not retried");
  assert.deepEqual(m.uploadErrors, [
    "README.md: outcome unknown — no server confirmation was received; check the folder before retrying",
  ]);
  assert.equal(m.conflict, null, "the batch settles; nothing retries on its own");
});


// The exact consent race this flow must survive: a same-choice Replace
// is in flight when the panel is reset and a NEW pick produces a new
// conflict. Resolving the old request must not let the old continuation
// send a replacement for the new batch's file — you never
// approved destroying anything in it.
test("a stale same-choice continuation cannot replace a newer batch's file", async () => {
  const sent: { path: string; version?: string }[] = [];
  const pending = new Map<string, { promise: Promise<FileUploadResult>; resolve: (value: FileUploadResult) => void }>();
  const { api } = fakeFileApi({
    uploadProjectFile: async (_id, destPath, _body, consent): Promise<FileUploadResult> => {
      sent.push({ path: destPath, version: consent?.version });
      if (destPath === "new.txt" && consent === undefined) throw refusal("new-v1", { name: "new.txt" });
      if (consent === undefined) throw refusal(`${destPath}-v1`, { name: destPath });
      const gate = deferred<FileUploadResult>();
      pending.set(destPath, gate);
      return gate.promise;
    },
  });
  const m = model(api);
  await m.open();
  await m.upload([pickedFile("old-a.txt", "a"), pickedFile("old-b.txt", "b")]);
  m.sameChoice = true;
  const staleAnswer = m.replaceConflict();
  assert.deepEqual(sent.map((u) => u.path), ["old-a.txt", "old-b.txt", "old-a.txt"],
    "the first Replace of the same-choice answer is in flight");

  m.reset();
  await m.upload([pickedFile("new.txt", "n")]);
  assert.ok(m.conflict !== null, "the new pick has its own ask on screen");
  assert.equal(m.conflictIncomingName(), "new.txt");

  // The old replacement lands. Its continuation must die at the fence.
  pending.get("old-a.txt")?.resolve({ path: "old-a.txt", size: 1 });
  await staleAnswer;
  await new Promise((resolve) => setTimeout(resolve, 0));
  assert.deepEqual(
    sent.map((u) => ({ path: u.path, version: u.version })),
    [
      { path: "old-a.txt", version: undefined },
      { path: "old-b.txt", version: undefined },
      { path: "old-a.txt", version: "old-a.txt-v1" },
      { path: "new.txt", version: undefined },
    ],
    "no consented request ever names the new batch's file",
  );
  assert.ok(m.conflict !== null, "the new ask still stands after the stale answer settles");
  assert.equal(m.conflictIncomingName(), "new.txt");
  assert.equal(m.confirming, false);
});

// The busy flag belongs to whichever answer is live: a stale send's
// cleanup must not clear a newer answer's in-flight state and reopen the
// double-send window.
test("a stale answer's cleanup leaves a newer answer's busy flag alone", async () => {
  const pending = new Map<string, { promise: Promise<FileUploadResult>; resolve: (value: FileUploadResult) => void }>();
  const { api } = fakeFileApi({
    uploadProjectFile: async (_id, destPath, _body, consent): Promise<FileUploadResult> => {
      if (consent === undefined) throw refusal(`${destPath}-v1`, { name: destPath });
      const gate = deferred<FileUploadResult>();
      pending.set(destPath, gate);
      return gate.promise;
    },
  });
  const m = model(api);
  await m.open();
  await m.upload([pickedFile("old-a.txt", "a")]);
  const staleAnswer = m.replaceConflict();
  assert.equal(m.confirming, true, "the old Replace owns the busy flag while in flight");

  m.reset();
  await m.upload([pickedFile("new.txt", "n")]);
  const liveAnswer = m.replaceConflict();
  assert.equal(m.confirming, true, "the new Replace owns the busy flag");

  pending.get("old-a.txt")?.resolve({ path: "old-a.txt", size: 1 });
  await staleAnswer;
  assert.equal(m.confirming, true, "the stale cleanup did not clear the live answer's flag");

  pending.get("new.txt")?.resolve({ path: "new.txt", size: 1 });
  await liveAnswer;
  await new Promise((resolve) => setTimeout(resolve, 0));
  assert.equal(m.confirming, false);
  assert.equal(m.conflict, null);
});

// A batch that landed files before its last ask was cancelled still
// settles: the landings refresh and select, and no session is left
// hanging behind a dismissed question.
test("cancelling the last conflict settles the batch's earlier landings", async () => {
  const stored: string[] = [];
  const { api, calls } = fakeFileApi({
    listProjectFiles: async (_id, path): Promise<FileListing> => {
      calls.listed.push(path);
      return {
        path,
        at_root: path === "",
        entries: [...ROOT_ENTRIES, ...stored.map((p) => entry(p))],
      };
    },
    uploadProjectFile: async (_id, destPath): Promise<FileUploadResult> => {
      if (destPath === "final.txt") throw refusal("final-v1", { name: "final.txt" });
      stored.push(destPath);
      return { path: destPath, size: 3 };
    },
  });
  const m = model(api);
  await m.open();
  const listedBefore = calls.listed.length;
  await m.upload([pickedFile("ok.txt", "ok"), pickedFile("final.txt", "no")]);
  assert.ok(m.conflict !== null);
  m.cancelConflict();
  assert.equal(m.conflict, null);
  await new Promise((resolve) => setTimeout(resolve, 0));
  assert.ok(calls.listed.length > listedBefore, "the cancelled batch still refreshes");
  assert.equal(m.selected, null, "settling moves no selection");
  assert.ok(
    m.entries().some((e) => e.path === "ok.txt"),
    "the file that landed before the ask is an ordinary row in the refreshed listing",
  );
  assert.ok(
    m.uploadRecords.some((record) => record.status === "stopped" && record.name === "final.txt"),
    "the cancelled ask's record stays visible as stopped",
  );
});

// The bulk answer stops at a changed target and goes no further: the
// changed file stays on screen with fresh facts, and nothing behind it
// in the queue is sent without your approval.
test("same-choice Replace stops at a changed second target and never reaches the third", async () => {
  const sent: { path: string; version?: string }[] = [];
  const { api } = fakeFileApi({
    uploadProjectFile: async (_id, destPath, _body, consent): Promise<FileUploadResult> => {
      sent.push({ path: destPath, version: consent?.version });
      if (consent === undefined) throw refusal(`${destPath}-v1`, { name: destPath });
      if (destPath === "b.txt") throw refusal("b-v2", { name: "b.txt" }, "changed");
      return { path: destPath, size: 3 };
    },
  });
  const m = model(api);
  await m.open();
  await m.upload([
    pickedFile("a.txt", "a"),
    pickedFile("b.txt", "b"),
    pickedFile("c.txt", "c"),
  ]);
  m.sameChoice = true;
  await m.replaceConflict();
  assert.ok(m.conflict !== null, "the changed file is still on screen");
  assert.equal(m.conflictIncomingName(), "b.txt");
  assert.equal(m.conflict.info.existing.version, "b-v2", "the ask carries the fresh facts");
  assert.equal(m.conflictRemaining(), 1, "the third file is still queued, unasked and unsent");
  assert.deepEqual(
    sent.map((u) => u.path),
    ["a.txt", "b.txt", "c.txt", "a.txt", "b.txt"],
    "nothing was sent for the third file",
  );
  assert.equal(m.confirming, false, "no automatic retry of the changed target");
});

// The Upload-as continuation is fenced like Replace: a stale bulk loop
// must not file the new batch's conflict under an old suggestion.
test("a stale Upload-as continuation cannot send a newer batch's file", async () => {
  const sent: string[] = [];
  const pending = new Map<string, { promise: Promise<FileUploadResult>; resolve: (value: FileUploadResult) => void }>();
  const { api } = fakeFileApi({
    uploadProjectFile: async (_id, destPath): Promise<FileUploadResult> => {
      sent.push(destPath);
      if (destPath === "old-a.txt" || destPath === "old-b.txt" || destPath === "new.txt") {
        throw refusal(`${destPath}-v1`, { name: destPath });
      }
      const gate = deferred<FileUploadResult>();
      pending.set(destPath, gate);
      return gate.promise;
    },
  });
  const m = model(api);
  await m.open();
  await m.upload([pickedFile("old-a.txt", "a"), pickedFile("old-b.txt", "b")]);
  m.sameChoice = true;
  const staleAnswer = m.confirmUploadAs();

  m.reset();
  await m.upload([pickedFile("new.txt", "n")]);
  assert.ok(m.conflict !== null);
  assert.equal(m.conflictIncomingName(), "new.txt");

  pending.get("old-a-2.txt")?.resolve({ path: "old-a-2.txt", size: 1 });
  await staleAnswer;
  await new Promise((resolve) => setTimeout(resolve, 0));
  assert.equal(sent.filter((path) => path === "new.txt").length, 1,
    "the new batch's only new.txt request is its own refused pick");
  assert.ok(!sent.includes("new-2.txt"),
    "the stale continuation sent nothing for the new batch's conflict");
  assert.ok(m.conflict !== null, "the new ask still stands");
  assert.equal(m.conflictIncomingName(), "new.txt");
});

test("dot, dotdot and NUL are refused as alternative names without sending", async () => {
  const sent: string[] = [];
  const { api } = fakeFileApi({
    uploadProjectFile: async (_id, destPath): Promise<FileUploadResult> => {
      sent.push(destPath);
      throw refusal("v1");
    },
  });
  const m = model(api);
  await m.open();
  await m.upload([pickedFile("README.md", "new")]);
  for (const bad of [".", "..", "a\0b"]) {
    m.setConflictName(bad);
    await m.confirmUploadAs();
    assert.equal(m.conflictNameError, "That's not a file name.", `${JSON.stringify(bad)} is refused`);
  }
  assert.deepEqual(sent, ["README.md"]);
});

// A 409 whose reason the client does not know is a plain failure, not a
// question: guessing it replaceable would put a Replace button behind a
// refusal nobody understood.
test("an unknown conflict reason fails closed as a plain error", async () => {
  const weird = new ApiError(409, "conflict", "README.md: quota exceeded", {
    reason: "quota" as UploadConflictReason,
    message: "README.md: quota exceeded",
    existing: { name: "README.md", dir: false, size: 1, mtime: "t", version: "v" },
  });
  const { api } = fakeFileApi({
    uploadProjectFile: async (): Promise<FileUploadResult> => {
      throw weird;
    },
  });
  const m = model(api);
  await m.open();
  await m.upload([pickedFile("README.md", "new")]);
  assert.equal(m.conflict, null, "no ask is built for an unknown reason");
  assert.deepEqual(m.uploadErrors, ["README.md: README.md: quota exceeded"]);
});

// A closed panel stops dispatching the rest of the pick.
test("a reset stops dispatching the rest of the pick", async () => {
  const sent: string[] = [];
  const gate = deferred<FileUploadResult>();
  const { api } = fakeFileApi({
    uploadProjectFile: async (_id, destPath): Promise<FileUploadResult> => {
      sent.push(destPath);
      if (destPath === "a.txt") return gate.promise;
      return { path: destPath, size: 1 };
    },
  });
  const m = model(api);
  await m.open();
  const work = m.upload([
    pickedFile("a.txt", "a"),
    pickedFile("b.txt", "b"),
    pickedFile("c.txt", "c"),
  ]);
  m.reset();
  gate.resolve({ path: "a.txt", size: 1 });
  await work;
  assert.deepEqual(sent, ["a.txt"],
    "the file in flight when the panel closed is the last one dispatched");
});

test("a stale upload completion cannot clear a newer upload's busy flag", async () => {
  const oldGate = deferred<FileUploadResult>();
  const newGate = deferred<FileUploadResult>();
  const sent: string[] = [];
  const { api } = fakeFileApi({
    uploadProjectFile: async (_id, path): Promise<FileUploadResult> => {
      sent.push(path);
      return path === "old.txt" ? oldGate.promise : newGate.promise;
    },
  });
  const m = model(api);
  const oldUpload = m.upload([pickedFile("old.txt", "old")]);
  m.reset();
  const newUpload = m.upload([pickedFile("new.txt", "new")]);
  try {
    oldGate.resolve({ path: "old.txt", size: 3 });
    await oldUpload;
    assert.equal(m.uploading, true, "the newer request is still pending");
    await m.upload([pickedFile("third.txt", "third")]);
    assert.deepEqual(sent, ["old.txt", "new.txt"], "a third pick cannot enter the active batch");
  } finally {
    newGate.resolve({ path: "new.txt", size: 3 });
    await newUpload;
  }
  assert.equal(m.uploading, false);
});

test("reset during an ask dismisses it without authorizing anything", async () => {
  const sent: string[] = [];
  const { api, calls } = fakeFileApi({
    uploadProjectFile: async (_id, destPath): Promise<FileUploadResult> => {
      sent.push(destPath);
      throw refusal("v1");
    },
  });
  const m = model(api);
  await m.open();
  await m.upload([pickedFile("README.md", "new"), pickedFile("logo.png", "new")]);
  m.reset();
  assert.equal(m.conflict, null);
  assert.equal(m.conflictRemaining(), 0);
  m.cancelConflict();
  assert.deepEqual(sent, ["README.md", "logo.png"], "no further request leaves a dismissed ask");
  assert.deepEqual(calls.listed, [""], "a dismissed ask starts no refresh");
});

test("the ask renders as an alertdialog with the three answers and no nested hints", async () => {
  const { api } = fakeFileApi({
    uploadProjectFile: async (): Promise<FileUploadResult> => {
      throw refusal("v1");
    },
  });
  const m = model(api);
  await m.open();
  await m.upload([pickedFile("README.md", "new")]);
  const box = stubContainer();
  new FilesPanel(box.el, m).render();
  const html = box.html();
  assert.match(html, /role="alertdialog"/, "the ask is a modal dialog");
  assert.ok(html.includes("README.md already exists"), "the title names the incoming file");
  assert.match(html, /in noisedeck\/ · 1 of 1/,
    "the description names the folder and the position");
  assert.ok(html.includes("Replace it with the file you're uploading, or upload yours under another name."));
  assert.ok(html.includes("data-files-conflict-icon"), "the head carries the warning icon");
  assert.ok(html.includes('data-files-conflict-close data-dialog-close aria-label="Close"'), "the head carries an accessible close control on the modal's close contract");
  assert.ok(html.includes("In the folder"), "the comparison chip names the existing side");
  assert.ok(html.includes("from your computer"), "the comparison chip names the uploading side");
  assert.ok(html.includes('value="README-2.md"'), "the alternative field opens on the suggestion");
  assert.ok(html.includes(">Replace</button>"), "Replace is offered for a regular file");
  assert.ok(!html.includes("Same choice"), "no same-choice offer with nothing queued behind");
  assertNoNestedDataHints(html, "the ask's hints");
});

test("a folder collision renders no Replace and offers the alternative only", async () => {
  const { api } = fakeFileApi({
    uploadProjectFile: async (): Promise<FileUploadResult> => {
      throw refusal("v1", { dir: true }, "directory");
    },
  });
  const m = model(api);
  await m.open();
  await m.upload([pickedFile("docs", "x")]);
  const box = stubContainer();
  new FilesPanel(box.el, m).render();
  const html = box.html();
  assert.ok(!html.includes(">Replace</button>"), "a folder is never replaceable");
  assert.ok(html.includes("a folder"), "the existing side says what holds the name");
  assert.ok(html.includes("Upload as"), "the alternative is still offered");
});

test("a queued second conflict renders the same-choice offer unchecked", async () => {
  const { api } = fakeFileApi({
    uploadProjectFile: async (_id, destPath): Promise<FileUploadResult> => {
      throw refusal(`${destPath}-v1`, { name: destPath });
    },
  });
  const m = model(api);
  await m.open();
  await m.upload([pickedFile("README.md", "new"), pickedFile("logo.png", "new")]);
  const box = stubContainer();
  new FilesPanel(box.el, m).render();
  const html = box.html();
  assert.ok(html.includes("Same choice for the other 1 file"), "the offer names what it covers");
  assert.ok(!html.includes("data-files-conflict-same checked"), "the offer starts unchecked");
  assert.match(html, /1 of 2/, "the position names where the batch stands");
});

// ---- download ----

test("the download is a real URL against the download endpoint", () => {
  const { api } = fakeFileApi();
  const m = model(api);
  assert.equal(
    m.downloadUrl(entry("docs/architecture.md")),
    "/api/projects/proj-1/files/download?path=docs%2Farchitecture.md",
  );
});

// ---- expand / close ----

test("closing resets the viewer and the expanded takeover but keeps the directory", async () => {
  const { api } = fakeFileApi();
  const m = model(api);
  await m.openDirectory("docs");
  await m.activate(entry("docs/architecture.md"));
  m.toggleExpanded();
  assert.equal(m.expanded, true);
  m.reset();
  assert.equal(m.expanded, false);
  assert.equal(m.selected, null);
  assert.equal(m.view, null);
  assert.equal(m.dir, "docs", "the user's place in the tree is not a view");
});

// ---- the shell's markup ----

interface StubContainer {
  el: HTMLElement;
  html: () => string;
  expanded: () => string | undefined;
}

function stubContainer(): StubContainer {
  const box = {
    innerHTML: "",
    dataset: {} as Record<string, string>,
    closest: () => null,
    querySelector: () => null,
    querySelectorAll: () => [],
  };
  return {
    el: box as unknown as HTMLElement,
    html: () => box.innerHTML,
    expanded: () => box.dataset.expanded,
  };
}

async function shellShowing(file: FileEntry, api?: FileApiClient) {
  const built = api ?? fakeFileApi().api;
  const m = model(built);
  await m.refresh();
  await m.activate(file);
  const box = stubContainer();
  const panel = new FilesPanel(box.el, m);
  panel.render();
  return { m, box, panel };
}

test("the panel's controls carry their accessible names and are real buttons", async () => {
  const { box } = await shellShowing(entry("README.md"));
  const html = box.html();
  assert.ok(html.includes('aria-label="Close files"'));
  assert.ok(html.includes('aria-label="Upload files"'));
  assert.match(html, /data-files-upload[^>]*aria-label="Upload files"/, "the upload is an icon-only action");
  assert.ok(html.includes('aria-label="Download file"'));
  assert.ok(html.includes('aria-label="Expand"'));
  assert.ok(html.includes('aria-label="Back to files"'));
  assert.ok(html.includes('aria-label="Wrap lines"'));
  assert.ok(html.includes('data-files-wrap data-focus-key="wrap"'));
  assert.match(html, /data-files-wrap[^>]*aria-pressed="true"/, "wrapping is on by default");
  assert.ok(html.includes("data-files-viewer-footer"), "the footer carries the status line");
  assert.ok(html.includes(">wrap<"), "the wrap toggle is labelled by its visible text");
  assert.ok(/data-files-wrap-switch[^>]*aria-hidden/.test(html) || html.includes('aria-hidden="true" data-files-wrap-switch'),
    "the phone's switch is presentation, not a second control");
  assert.match(html, /data-files-wrap-dot[^>]*bg-accent/, "the wrap dot reads as on");
  assert.ok(html.includes('aria-label="Project path"'), "the breadcrumb is a landmark");
  assert.match(html, /data-files-crumb-depth="-1"[^>]*aria-label="noisedeck"/,
    "the root crumb carries the project name");
  assert.ok(!html.includes("<h2"), "there is no panel title beside the trail");
  assert.ok(html.includes('type="file" multiple'), "the picker takes more than one file");
  // Rows are divs with a button role so they stay reachable from the
  // keyboard without UA button baseline behaviour shifting text.
  assert.match(html, /<div role="button" tabindex="0" data-files-row="README\.md"/);
  assert.ok(html.includes("focus-visible:bg-wash-hover"), "keyboard focus takes the hover wash, never a ring");
  assert.ok(html.includes('data-files-sort role="group" aria-label="Sort files"'), "the sort strip is a group");
  assert.match(html, /data-sort-field="name"[^>]*aria-pressed="true"/, "name is the default active sort");
  assert.match(html, /data-sort-field="(size|modified)"[^>]*aria-pressed="false"/, "the other sorts are idle");
  assert.ok(html.includes("data-files-drop-overlay"), "the whole column is the drop target");
  assert.ok(html.includes("Drop to upload into"), "the drag overlay names the upload");
});

test("the breadcrumb root is the project name and re-sorting flips the active wash", async () => {
  const { m, box, panel } = await shellShowing(entry("README.md"));
  assert.match(
    box.html(),
    /data-files-crumb-depth="-1"[^>]*aria-label="noisedeck"/,
    "the root crumb carries the project name at the top of the trail",
  );
  m.setSort("name");
  assert.equal(m.sort.direction, "desc", "re-selecting the active column flips direction");
  panel.render();
  assert.match(
    box.html(),
    /data-sort-field="name"[^>]*aria-pressed="true"[^>]*bg-wash-selected[^>]*>Name ↓/,
    "the wash flips with direction: the active column carries the downward arrow",
  );
});

test("a long viewer filename yields to fixed-size actions", async () => {
  const name = `${"long-file-name-".repeat(12)}notes.md`;
  const { box } = await shellShowing(entry(name));
  const html = box.html();
  assert.ok(
    html.includes(`data-files-viewer-name data-hint="${name}" class="min-w-0 shrink truncate`),
    "the full name stays available while its visible box may truncate",
  );
  assert.ok(
    html.includes('<span class="ml-auto"></span>') &&
      html.includes('data-files-viewer-actions class="flex flex-none'),
    "viewer actions keep their width while the filename shrinks",
  );
});

test("the shell marks the phone list and viewer as separate scroll surfaces", async () => {
  const { box } = await shellShowing(entry("README.md"));
  const html = box.html();
  assert.ok(html.includes("data-files-list-scroll"));
  assert.ok(html.includes('data-files-viewer-scroll data-files-path="README.md"'));
  assert.ok(html.includes('role="status" aria-live="polite"'));
  assert.ok(html.includes("max-md:min-h-11"), "rows and the download action grow for touch");
  assert.ok(html.includes("crumb-item"), "breadcrumbs render with the override class");
  assert.ok(html.includes("hidden max-md:grid"), "the back action exists only in the phone viewer");
  assert.ok(html.includes('data-files-wrapped="true"'));
  assert.ok(html.includes("whitespace-pre-wrap"), "long lines wrap inside the viewer by default");
  assert.equal(html.includes("min-w-max whitespace-pre"), false, "the unwrapped scroller class is gone");
  assert.ok(html.includes("data-files-text"), "the text view mounts its body; the gutter rows fill it imperatively so the bytes never pass through innerHTML (browser probe asserts the gutter geometry)");
  // The imperative fill (files-panel-dom.ts) creates data-files-line rows
  // with number/text nodes; the browser probe asserts their geometry. The
  // unit template contract is the mount point above.
});

test("viewer actions expose verb-first hints", async () => {
  const { box } = await shellShowing(entry("README.md"));
  const html = box.html();
  assert.ok(html.includes('data-hint="Download this file"'), "the download control exposes its hint");
  // The copy names what moves - the panel - rather than calling it a
  // preview, which is the one thing that does not change size.
  assert.ok(html.includes('data-hint="Expand the panel to fill the window"'), "the expand control exposes its hint");
});

test("the header exposes the upload and breadcrumb hints and carries no nested data-hints", async () => {
  const { box } = await shellShowing(entry("README.md"));
  const html = box.html();
  assert.ok(html.includes('data-hint="Upload files into this folder"'), "the upload button exposes its hint");
  assert.ok(html.includes('data-hint="Click the project name to go back to the root"'), "the breadcrumb exposes its hint");
  assertNoNestedDataHints(html, "files panel");
});

test("the docked panel draws its fixed geometry", async () => {
  const { box } = await shellShowing(entry("README.md"));
  const html = box.html();
  assert.ok(html.includes("chrome"), "the header is a chrome bar");
  assert.ok(html.includes("w-[320px]"), "the list column is exactly 320px");
  assert.ok(html.includes("h-[38px]"), "the sort strip is 38px tall");
  assert.match(html, /data-files-viewer-header class="[^"]*h-\[38px\]/,
    "the viewer file header is the same 38px strip");
  assert.ok(html.includes("leading-[1.62]"), "the viewer's line height is 1.62");
  assert.ok(html.includes("text-[min(var(--type-mono-size),calc(var(--type-caption-size)*var(--type-mono-xh-ratio)))]"),
    "the viewer's mono x-height is capped by the caption token");
  assert.ok(html.includes("bg-surface-sunken"), "the viewer body is sunken");
  assert.ok(html.includes("font-mono"), "metadata keeps the mono voice");
  assert.ok(html.includes("data-files-viewer-status"), "the footer status line is drawn");
  assert.ok(html.includes("read-only"), "the footer states the read-only contract");
});

test("the drop overlay is an inset dashed accent box read under file drags", async () => {
  const { box } = await shellShowing(entry("README.md"));
  const html = box.html();
  assert.ok(html.includes("border-2 border-dashed border-accent"), "the overlay is the dashed accent rule");
  assert.ok(html.includes("bg-accent-tint"), "the overlay rests on the accent tint");
  assert.ok(html.includes("data-files-drop-overlay hidden"), "the overlay is inert until a drag");
  assert.match(html, /data-files-drop-overlay hidden[^>]*>\s*<span>Drop to upload into\s*<span>noisedeck<\/span>/,
    "the overlay's own line names the destination directory");
});

test("the upload errors render at the top of the rows, not a footer", async () => {
  const { api } = fakeFileApi({
    uploadProjectFile: (): Promise<FileUploadResult> =>
      Promise.reject(new ApiError(413, "too_large", "upload is too large")),
  });
  const m = model(api);
  await m.refresh();
  m.uploadErrors = ["big.bin: upload is too large"];
  const box = stubContainer();
  new FilesPanel(box.el, m);
  assert.match(box.html(), /data-files-upload-errors[^>]*>.*big\.bin: upload is too large/, "per-file failures sit with the rows");
});

test("every render schedules a breadcrumb measurement, whoever owns focus", async () => {
  const original = (globalThis as { requestAnimationFrame?: unknown }).requestAnimationFrame;
  let scheduled = 0;
  (globalThis as { requestAnimationFrame?: unknown }).requestAnimationFrame = (fn: () => void) => {
    scheduled += 1;
    // Not run: measureCrumbs wants real geometry the stub container has not got.
    void fn;
    return 0;
  };
  try {
    // Focus lives outside the panel here — the normal case — so the render
    // returns at the focus guard. The measurement must already be booked by
    // then, or the trail keeps whatever width it was last elided to.
    const { panel } = await shellShowing(entry("README.md"));
    const bookedByFirstRenders = scheduled;
    assert.ok(bookedByFirstRenders > 0, "the first render books a measurement");
    panel.render();
    assert.equal(scheduled, bookedByFirstRenders + 1, "so does every render after it");
  } finally {
    (globalThis as { requestAnimationFrame?: unknown }).requestAnimationFrame = original;
  }
});

test("expanding hides the panel header and the list; minimizing restores them", async () => {
  const { m, box, panel } = await shellShowing(entry("README.md"));
  assert.ok(box.html().includes("w-[320px]"));
  m.toggleExpanded();
  panel.render();
  assert.equal(box.expanded(), "true");
  assert.equal(box.html().includes("w-[320px]"), false, "the list is gone");
  assert.equal(box.html().includes("<h2"), false, "the panel header is gone");
  assert.ok(box.html().includes('aria-label="Collapse"'));
  assert.ok(box.html().includes('aria-label="Close files"'), "close moves into the viewer header");
  m.toggleExpanded();
  panel.render();
  assert.equal(box.expanded(), "false");
  assert.ok(box.html().includes("w-[320px]"));
});

test("closing the shell resets the view and redraws the empty viewer", async () => {
  const { m, box, panel } = await shellShowing(entry("README.md"));
  m.toggleExpanded();
  panel.render();
  panel.close();
  assert.equal(m.expanded, false);
  assert.equal(box.expanded(), "false");
  assert.ok(box.html().includes("Nothing open"));
  assert.ok(box.html().includes("Pick a file to read it."));
});

test("the wrap choice is remembered per browser and survives close", async () => {
  const g = globalThis as { localStorage?: unknown };
  const store = new Map<string, string>();
  g.localStorage = {
    getItem: (k: string) => store.get(k) ?? null,
    setItem: (k: string, v: string) => void store.set(k, v),
  };
  try {
    writeWrap(false);
    assert.equal(readWrap(), false);
    const { m, box, panel } = await shellShowing(entry("README.md"));
    assert.ok(box.html().includes('data-files-wrapped="false"'), "the stored choice wins over the default");
    panel.close();
    await m.activate(entry("README.md"));
    panel.render();
    assert.ok(box.html().includes('data-files-wrapped="false"'), "close leaves the choice standing");
  } finally {
    delete g.localStorage;
  }
});

test("a storage that throws leaves wrapping on", async () => {
  const g = globalThis as { localStorage?: unknown };
  g.localStorage = {
    getItem: () => { throw new Error("blocked"); },
    setItem: () => { throw new Error("blocked"); },
  };
  try {
    const { box } = await shellShowing(entry("README.md"));
    assert.ok(box.html().includes('data-files-wrapped="true"'), "the default stands");
  } finally {
    delete g.localStorage;
  }
});

test("an un-previewable file renders the fallback copy and a download, not decoded bytes", async () => {
  const { api } = fakeFileApi({
    viewProjectFile: () => Promise.reject(new ApiError(415, "not_text", "file is not text")),
  });
  const { box } = await shellShowing(entry("scan.tiff", false, 18422), api);
  const html = box.html();
  assert.ok(html.includes("No preview for .tiff files."));
  assert.ok(html.includes(">Download</a>"));
  assert.equal(html.includes("data-files-text"), false, "no text pane at all");
});

test("an empty text file is drawn as empty, not as a failed load", async () => {
  const { api } = fakeFileApi({ viewProjectFile: async () => ({ content: "" }) });
  const { box } = await shellShowing(entry("TODO.md", false, 0), api);
  assert.ok(box.html().includes("empty file"));
  assert.equal(box.html().includes("data-files-text"), false, "no numbered text pane for an empty file");
});

test("the list's states each draw their own copy", async () => {
  const { api } = fakeFileApi({
    listProjectFiles: async () => ({
      path: "",
      at_root: true,
      entries: [entry("a.txt")],
      truncated: true,
    }),
  });
  const m = model(api);
  const box = stubContainer();
  const panel = new FilesPanel(box.el, m);
  assert.ok(box.html().includes("Loading files…"), "before the first answer");
  await m.refresh();
  panel.render();
  assert.ok(box.html().includes("listing truncated · showing 1 entries"));

  const failing = model(fakeFileApi({
    listProjectFiles: () => Promise.reject(new ApiError(404, "not_found", "no such file in the project")),
  }).api);
  await failing.refresh();
  const errBox = stubContainer();
  new FilesPanel(errBox.el, failing);
  assert.ok(errBox.html().includes("no such file in the project"));

  const empty = model(fakeFileApi().api);
  await empty.openDirectory("internal");
  const emptyBox = stubContainer();
  new FilesPanel(emptyBox.el, empty);
  assert.ok(emptyBox.html().includes('data-files-up'), "a nested empty directory still offers the .. row");
  assert.ok(!emptyBox.html().includes('data-files-row'), "there are no file rows in an empty directory");
});

test("the shell escapes the names and paths the server returned", async () => {
  const nasty = entry('"><img src=x onerror=alert(1)>.md');
  const { api } = fakeFileApi({
    listProjectFiles: async () => ({ path: "", at_root: true, entries: [nasty] }),
  });
  const { box } = await shellShowing(nasty, api);
  assert.equal(box.html().includes("<img"), false, "no tag survives");
  assert.ok(box.html().includes("&lt;img"), "it renders as visible text instead");
});

test("marked rows color their icons and keep status words accessible", async () => {
  const marked: FileListing = {
    path: "",
    at_root: true,
    entries: [
      { ...entry("docs", true), change: "modified" },
      { ...entry("README.md"), change: "modified" },
      { ...entry("fresh.txt"), change: "new" },
    ],
  };
  const { api } = fakeFileApi({ listProjectFiles: async () => marked });
  const { box } = await shellShowing(entry("README.md"), api);
  const html = box.html();
  assert.match(html, /data-files-change="modified" class="flex-none text-git-modified"/);
  assert.match(html, /data-files-change="new" class="flex-none text-git-added"/);
  assert.match(html, /<span class="sr-only">Modified<\/span>/);
  assert.match(html, /<span class="sr-only">New<\/span>/);
  assert.ok(html.includes('data-hint="Modified · Open this folder"'));
  assert.ok(html.includes('data-hint="Modified · Open this file"'));
  assert.ok(html.includes('data-hint="New · Open this file"'));
  assert.ok(!html.includes('data-files-change="modified" class="flex-none text-caption'), "the status no longer takes a visible metadata column");
  assertNoNestedDataHints(html, "marked rows");
});

// ---- the image viewer ----

const IMAGE_MTIME = "2026-08-02T11:00:00Z";

function imageSrcOf(m: FilesPanelModel): string {
  const view = m.view;
  assert.ok(view?.kind === "image" && (view.image.state === "loading" || view.image.state === "shown"));
  return view.image.src;
}

test("the image viewer is chosen by extension, and the cap reads in MiB rounded up", () => {
  for (const name of ["a.png", "a.JPG", "a.jpeg", "a.gif", "a.webp", "a.bmp", "a.avif"]) assert.ok(isImageName(name), name);
  for (const name of ["a.tiff", "a.svg", "png", ".png", "a.png.txt"]) assert.equal(isImageName(name), false, name);
  assert.equal(formatMiB(16 << 20), "16 MiB");
  assert.equal(formatMiB(48_200_000), "46 MiB");
  assert.equal(formatMiB((16 << 20) + 1), "16.1 MiB");
});

test("opening an image reads no text and loads the image route for this listing's mtime", async () => {
  const { api, calls } = fakeFileApi();
  const m = model(api);
  await m.activate(entry("shots/home.png", false, 412_000, IMAGE_MTIME));
  assert.deepEqual(calls.viewed, [], "no text read for an image");
  assert.equal(imageSrcOf(m), `/api/projects/${PROJECT}/files/image?path=shots%2Fhome.png&v=${encodeURIComponent(`${IMAGE_MTIME}:412000`)}`);
  const status = viewerStatus(m.selected!, m.view, Date.parse(IMAGE_MTIME) + 120_000);
  assert.equal(status, "PNG · 412.0 KB · modified 2m ago", "the listing's facts, no dimensions before the decode");
});

test("a decode adds the measured dimensions; a decode for another source changes nothing", async () => {
  const { api } = fakeFileApi();
  const m = model(api);
  await m.activate(entry("home.png", false, 412_000, IMAGE_MTIME));
  const src = imageSrcOf(m);
  assert.equal(m.imageDecoded(`${src}-stale`, 10, 10, true), false);
  assert.equal(m.view?.kind === "image" && m.view.image.state, "loading");
  assert.equal(m.imageDecoded(src, 1440, 900, false), true);
  assert.deepEqual(m.view, { kind: "image", image: { state: "shown", src, width: 1440, height: 900, alpha: false } });
  assert.equal(viewerStatus(m.selected!, m.view, Date.parse(IMAGE_MTIME) + 120_000), "PNG · 1440 × 900 · 412.0 KB · modified 2m ago");
});

test("an image listed past the cap is refused without any request", async () => {
  const { api, calls } = fakeFileApi();
  const m = model(api);
  await m.activate(entry("render-8k.png", false, 48_200_000));
  assert.deepEqual(m.view, { kind: "image", image: { state: "oversized" } });
  assert.deepEqual(calls.viewed, []);
  assert.deepEqual(calls.checked, []);
  const box = stubContainer();
  new FilesPanel(box.el, m).render();
  const html = box.html();
  assert.ok(html.includes("Too large to preview"));
  assert.ok(html.includes("Previews stop at 16 MiB. This file is 46 MiB."));
  assert.ok(html.includes('data-focus-key="download-binary"'), "the refusal keeps its download");
  assert.equal(html.includes("<img"), false);
  assert.equal(html.includes("data-files-zoom"), false, "nothing to zoom");
});

test("a failed image load is classified by the route's status, not guessed", async () => {
  const cases: [unknown, unknown][] = [
    [new ApiError(404, "unknown", "Not Found"), { kind: "image", image: { state: "gone" } }],
    [new ApiError(413, "unknown", "Payload Too Large"), { kind: "image", image: { state: "oversized" } }],
    [new ApiError(415, "unknown", "Unsupported Media Type"), { kind: "image", image: { state: "undecodable" } }],
    [null, { kind: "image", image: { state: "undecodable" } }],
    [new ApiError(500, "unknown", "Internal Server Error"), { kind: "error", message: "Internal Server Error" }],
  ];
  for (const [failure, expected] of cases) {
    const { api, calls } = fakeFileApi({
      checkProjectFileImage: async () => {
        if (failure !== null) throw failure;
      },
    });
    const m = model(api);
    await m.activate(entry("chart.png"));
    assert.equal(await m.imageFailed(imageSrcOf(m)), true);
    assert.deepEqual(m.view, expected);
    assert.deepEqual(calls.checked, [], "the override answered, not the recorder");
  }
});

test("a late failure for a file the user left does not paint over the new one", async () => {
  const gate = deferred<void>();
  const { api } = fakeFileApi({
    checkProjectFileImage: () => gate.promise.then(() => { throw new ApiError(404, "unknown", "Not Found"); }),
  });
  const m = model(api);
  await m.activate(entry("chart.png"));
  const failing = m.imageFailed(imageSrcOf(m));
  await m.activate(entry("README.md"));
  gate.resolve();
  assert.equal(await failing, false);
  assert.equal(m.view?.kind, "text");
});

test("a background change retries an image whose status check failed", async () => {
  let current = entry("home.png", false, 412_000, IMAGE_MTIME);
  const { api, calls } = fakeFileApi({
    listProjectFiles: async (_id: string, path: string): Promise<FileListing> => ({
      path, at_root: true, entries: [current],
    }),
    checkProjectFileImage: async () => { throw new ApiError(500, "unknown", "Internal Server Error"); },
  });
  const m = model(api);
  await m.open();
  await m.activate(m.entries()[0]!);
  await m.imageFailed(imageSrcOf(m));
  assert.deepEqual(m.view, { kind: "error", message: "Internal Server Error" });
  current = entry("home.png", false, 412_000, "2026-08-02T11:05:00Z");
  calls.notify?.();
  await eventuallyTrue(() => m.view?.kind === "image" && m.view.image.state === "loading");
  assert.ok(imageSrcOf(m).endsWith(`v=${encodeURIComponent(`${current.mtime}:412000`)}`));
});

test("a removed image hides every download and offers Refresh list", async () => {
  const { api } = fakeFileApi({
    checkProjectFileImage: async () => { throw new ApiError(404, "unknown", "Not Found"); },
  });
  const m = model(api);
  await m.refresh();
  await m.activate(entry("chart-latency.png"));
  await m.imageFailed(imageSrcOf(m));
  assert.equal(m.canDownload(), false);
  const box = stubContainer();
  new FilesPanel(box.el, m).render();
  const html = box.html();
  assert.ok(html.includes("No longer on disk"));
  assert.ok(html.includes("chart-latency.png was in the list, then removed before Deck could read it."));
  assert.ok(html.includes("data-files-refresh-list"));
  assert.ok(html.includes(">Refresh list</button>"));
  assert.equal(html.includes("data-files-download"), false, "neither the header nor the body offers a download");
});

test("Refresh list reads the folder again and reopens a file it still names", async () => {
  const { api, calls } = fakeFileApi();
  const m = model(api);
  await m.refresh();
  await m.activate(ROOT_ENTRIES[2]!);
  m.view = { kind: "image", image: { state: "gone" } };
  await m.refreshGone();
  assert.equal(calls.listed.length, 2);
  assert.equal(m.view?.kind === "image" && m.view.image.state, "loading", "the listed file is opened again");
});

test("a background change constructs one new image source and keeps its size until it decodes", async () => {
  let mtime = IMAGE_MTIME;
  let sources = 0;
  const { api, calls } = fakeFileApi({
    listProjectFiles: async (_id: string, path: string): Promise<FileListing> => ({
      path, at_root: true, entries: [entry("home.png", false, 412_000, mtime)],
    }),
    projectFileImageUrl: (id: string, path: string, version?: string): string => {
      ++sources;
      return `/api/projects/${encodeURIComponent(id)}/files/image?path=${encodeURIComponent(path)}` +
        (version === undefined ? "" : `&v=${encodeURIComponent(version)}`);
    },
  });
  const m = model(api);
  await m.open();
  await m.activate(m.entries()[0]!);
  const first = imageSrcOf(m);
  m.imageDecoded(first, 1440, 900, false);
  mtime = "2026-08-02T11:05:00Z";
  calls.notify?.();
  await eventuallyTrue(() => imageSrcOf(m) !== first);
  const view = m.view;
  assert.ok(view?.kind === "image" && view.image.state === "shown");
  assert.ok(view.image.src.endsWith(`v=${encodeURIComponent(`${mtime}:412000`)}`));
  assert.equal(view.image.width, 1440);
  assert.equal(sources, 2, "the change event builds only the initial and replacement sources");
});

test("a shown image draws Fit by default, its edge, and a checkerboard only behind alpha", async () => {
  for (const alpha of [false, true]) {
    const { api } = fakeFileApi();
    const m = model(api);
    await m.activate(entry("icon.png", false, 31_000));
    m.imageDecoded(imageSrcOf(m), 512, 512, alpha);
    const box = stubContainer();
    new FilesPanel(box.el, m).render();
    const html = box.html();
    assert.ok(html.includes('data-files-image-canvas="fit"'));
    assert.ok(html.includes("width:min(100cqw, calc(100cqh * 512 / 512), 512px)"), "fit never upscales past natural size");
    assert.equal(html.includes("checkerboard"), alpha);
    assert.match(html, /data-files-zoom="fit"[^>]*aria-pressed="true"/);
    assert.match(html, /data-files-zoom="actual"[^>]*aria-pressed="false"/);
    assert.equal(html.includes("data-files-wrap"), false, "the zoom pair takes the wrap toggle's slot");
    assert.equal(html.includes("read-only"), false);
  }
});

async function eventuallyTrue(check: () => boolean): Promise<void> {
  for (let i = 0; i < 50; i++) {
    if (check()) return;
    await new Promise((resolve) => setTimeout(resolve, 0));
  }
  assert.fail("condition never held");
}

test("a foreground listing that carries a newer generation of the open image reloads it", async () => {
  let current = entry("home.png", false, 412_000, IMAGE_MTIME);
  const { api } = fakeFileApi({
    listProjectFiles: async (_id: string, path: string): Promise<FileListing> => ({ path, at_root: true, entries: [current] }),
  });
  const m = model(api);
  await m.refresh();
  await m.activate(m.entries()[0]!);
  const first = imageSrcOf(m);
  m.imageDecoded(first, 1440, 900, false);
  // A replace that lands within the same mtime still changes the size.
  current = entry("home.png", false, 500_000, IMAGE_MTIME);
  await m.refresh();
  const second = imageSrcOf(m);
  assert.notEqual(second, first, "a new size is a new source");
  assert.equal(m.view?.kind === "image" && m.view.image.state, "shown", "the old size stays until the new decode");
  assert.equal(m.imageDecoded(first, 10, 10, false), false, "a late decode of the old source is ignored");
  // The next generation is past the cap: refused from the listing.
  current = entry("home.png", false, 48_200_000, "2026-08-02T11:05:00Z");
  await m.refresh();
  assert.deepEqual(m.view, { kind: "image", image: { state: "oversized" } });
  // And back under it: a refusal shown for the old generation does not stick.
  current = entry("home.png", false, 412_000, "2026-08-02T11:06:00Z");
  await m.refresh();
  assert.equal(m.view?.kind === "image" && m.view.image.state, "loading");
});

test("an unchanged listing leaves the open image alone", async () => {
  const { api } = fakeFileApi({
    listProjectFiles: async (_id: string, path: string): Promise<FileListing> => ({
      path, at_root: true, entries: [entry("home.png", false, 412_000, IMAGE_MTIME)],
    }),
  });
  const m = model(api);
  await m.refresh();
  await m.activate(m.entries()[0]!);
  const src = imageSrcOf(m);
  m.imageDecoded(src, 1440, 900, false);
  await m.refresh();
  assert.deepEqual(m.view, { kind: "image", image: { state: "shown", src, width: 1440, height: 900, alpha: false } });
});
