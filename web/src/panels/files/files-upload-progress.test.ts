import assert from "node:assert/strict";
import test from "node:test";
import { ApiError } from "../../api/http.ts";
import type { FileApiClient, FileEntry, FileUploadResult, UploadTransportOptions } from "../../api/files.ts";
import { FilesPanelModel } from "./files-panel.ts";

function harness() {
  const requests: {
    path: string;
    options: UploadTransportOptions;
    resolve: (result: FileUploadResult) => void;
    reject: (error: Error) => void;
  }[] = [];
  let entries: FileEntry[] = [];
  let listingFails = false;
  let nextListing: Promise<void> | null = null;
  const api: FileApiClient = {
    listProjectFiles: async (_project, path) => {
      if (listingFails) throw new Error("listing unavailable");
      const snapshot = { path, at_root: path === "", entries: entries.filter((entry) => entry.path.slice(0, Math.max(0, entry.path.lastIndexOf("/"))) === path) };
      const held = nextListing;
      nextListing = null;
      if (held !== null) await held;
      return snapshot;
    },
    viewProjectFile: async () => ({ content: "existing viewer" }),
    diffProjectFile: async () => ({ content: "@@ -1 +1 @@\n-old\n+new\n" }),
    projectFileDownloadUrl: () => "",
    projectFileImageUrl: () => "",
    checkProjectFileImage: async () => {},
    subscribeProjectFileEvents: () => () => {},
    listProjectAssets: async () => ({ locations: [], scanned: "", preview_max_bytes: 0 }),
    deleteProjectAssets: async () => [],
    subscribeProjectAssetEvents: () => () => {},
    uploadProjectFile: (_project, path, _body, _consent, options) => {
      assert.ok(options, "every Files upload uses measured transport");
      return new Promise((resolve, reject) => requests.push({ path, options, resolve, reject }));
    },
  };
  const model = new FilesPanelModel(api, "project", "/workspace/project");
  return {
    model, requests,
    failListing: (value: boolean) => { listingFails = value; },
    holdNextListing: () => {
      let release!: () => void;
      nextListing = new Promise<void>((resolve) => { release = resolve; });
      return release;
    },
    list: (path: string, size = 4) => {
      entries.push({ path, name: path.split("/").at(-1)!, size, dir: false, mode: "rw-r--r--", mtime: "2026-09-23T00:00:00Z" });
    },
  };
}

const file = (name = "upload.txt", contents = "data") => new File([contents], name);
const turn = () => new Promise<void>((resolve) => setImmediate(resolve));

test("measured bytes and upload completion never substitute for the server response", async () => {
  const h = harness();
  const pending = h.model.upload([file()]);
  const record = h.model.uploadRecords[0]!;
  h.requests[0]!.options.onProgress?.(2, 4);
  assert.equal(record.status, "uploading");
  assert.equal(record.sent, 2);
  h.requests[0]!.options.onSent?.();
  assert.equal(record.status, "confirming");
  assert.equal(h.model.uploading, true);
  assert.equal(h.model.entries().length, 0);
  h.list("upload.txt");
  h.requests[0]!.resolve({ path: "upload.txt", size: 4 });
  await pending;
  assert.equal(h.model.uploadRecords.length, 0);
  assert.equal(h.model.selected, null);
});

test("a confirmed upload survives failed and missing listings until its destination is observed", async () => {
  const h = harness();
  h.failListing(true);
  const pending = h.model.upload([file()]);
  h.requests[0]!.resolve({ path: "upload.txt", size: 4 });
  await pending;
  assert.equal(h.model.uploadRecords[0]?.status, "complete");
  h.model.reset();
  assert.equal(h.model.uploadRecords[0]?.status, "complete");
  h.failListing(false);
  await h.model.open();
  assert.equal(h.model.uploadRecords.length, 1);
  h.list("upload.txt");
  await h.model.refresh();
  assert.equal(h.model.uploadRecords.length, 0);
});

test("navigation cannot retarget an upload or reconcile it against another directory", async () => {
  const h = harness();
  await h.model.openDirectory("docs");
  const pending = h.model.upload([file()]);
  const record = h.model.uploadRecords[0]!;
  h.model.selectUpload(record);
  await h.model.openDirectory("");
  assert.equal(h.model.selectedUpload, null);
  assert.equal(h.requests[0]?.path, "docs/upload.txt");
  h.list("upload.txt");
  h.requests[0]!.resolve({ path: "docs/upload.txt", size: 4 });
  await pending;
  assert.equal(record.status, "complete");
  assert.equal(h.model.uploadRecords.length, 1);
  h.list("docs/upload.txt");
  await h.model.openDirectory("docs");
  assert.equal(h.model.uploadRecords.length, 0);
});

test("a listing started before confirmation cannot retire the newly confirmed record", async () => {
  const h = harness();
  h.list("a");
  const pending = h.model.upload([file("a"), file("b")]);
  const record = h.model.uploadRecords[0]!;
  const release = h.holdNextListing();
  const oldRead = h.model.refresh();
  h.requests[0]!.resolve({ path: "a", size: 4 });
  await turn();
  release();
  await oldRead;
  assert.equal(record.status, "complete");
  assert.ok(h.model.uploadRecords.includes(record));
  h.requests[1]!.resolve({ path: "b", size: 4 });
  await pending;
  assert.ok(!h.model.uploadRecords.includes(record), "the later read observes the confirmed file");
});

test("zero-byte uploads wait for confirmation; over-limit picks never dispatch", async () => {
  const h = harness();
  const large = new File([new Uint8Array((32 << 20) + 1)], "large.bin");
  const pending = h.model.upload([file("empty.txt", ""), large]);
  h.requests[0]!.options.onSent?.();
  assert.equal(h.model.uploadRecords[0]?.status, "confirming");
  assert.equal(h.model.uploadRecords[0]?.total, 0);
  assert.equal(h.model.uploadRecords[1]?.status, "too-large");
  h.requests[0]!.resolve({ path: "empty.txt", size: 0 });
  await pending;
  assert.equal(h.requests.length, 1);
});

test("cancelling one queued file leaves other files in the batch eligible", async () => {
  const h = harness();
  const pending = h.model.upload([file("a"), file("b"), file("c")]);
  const stopped = h.model.uploadRecords[1]!;
  h.model.cancelUpload(stopped);
  h.requests[0]!.resolve({ path: "a", size: 4 });
  await turn();
  assert.deepEqual(h.requests.map((request) => request.path), ["a", "c"]);
  h.requests[1]!.resolve({ path: "c", size: 4 });
  await pending;
  assert.equal(stopped.status, "stopped");
});

test("active cancellation is unconfirmed and stale callbacks cannot revive it", async () => {
  const h = harness();
  const pending = h.model.upload([file()]);
  const record = h.model.uploadRecords[0]!;
  h.model.cancelUpload(record);
  assert.equal(record.status, "unconfirmed");
  assert.equal(h.requests[0]!.options.signal?.aborted, true);
  h.requests[0]!.options.onProgress?.(4, 4);
  h.requests[0]!.options.onSent?.();
  h.requests[0]!.resolve({ path: "upload.txt", size: 4 });
  await pending;
  assert.equal(record.status, "unconfirmed");
  assert.equal(h.requests.length, 1);
});

test("reset fences old progress and finally from a newer pick", async () => {
  const h = harness();
  const old = h.model.upload([file("old"), file("never")]);
  const oldRecord = h.model.uploadRecords[0]!;
  h.model.reset();
  const current = h.model.upload([file("new")]);
  const newRecord = h.model.uploadRecords.at(-1)!;
  h.requests[0]!.options.onSent?.();
  h.requests[0]!.resolve({ path: "old", size: 4 });
  await old;
  assert.equal(oldRecord.status, "unconfirmed");
  assert.equal(h.model.uploading, true);
  assert.equal(newRecord.status, "preparing");
  assert.deepEqual(h.requests.map((request) => request.path), ["old", "new"]);
  h.requests[1]!.resolve({ path: "new", size: 4 });
  await current;
});

test("a lost response remains per-file uncertainty while unrelated queued work continues", async () => {
  const h = harness();
  const pending = h.model.upload([file("unknown"), file("next")]);
  const unknown = h.model.uploadRecords[0]!;
  h.requests[0]!.options.onSent?.();
  h.requests[0]!.reject(new Error("response truncated"));
  await turn();
  assert.equal(unknown.status, "unconfirmed");
  assert.deepEqual(h.requests.map((request) => request.path), ["unknown", "next"]);
  h.list("next");
  h.requests[1]!.resolve({ path: "next", size: 4 });
  await pending;
  assert.deepEqual(h.model.uploadRecords.map((record) => record.name), ["unknown"]);
});

test("authorization loss stops unsent work, but an ordinary forbidden file does not", async () => {
  for (const [status, code, stops] of [[401, "unauthorized", true], [403, "unlock-required", true], [403, "forbidden", false]] as const) {
    const h = harness();
    const pending = h.model.upload([file("first"), file("next")]);
    h.requests[0]!.reject(new ApiError(status, code, "refused"));
    await turn();
    if (!stops) h.requests[1]!.resolve({ path: "next", size: 4 });
    await pending;
    assert.equal(h.requests.length, stops ? 1 : 2, code);
    assert.equal(h.model.uploadRecords[1]?.status, stops ? "stopped" : "complete", code);
  }
});

test("authorization loss during Replace also stops the held conflict queue", async () => {
  const h = harness();
  const conflict = (name: string) => new ApiError(409, "conflict", "exists", {
    reason: "exists", message: "exists",
    existing: { name, dir: false, size: 4, mtime: "2026-09-23T00:00:00Z", version: name },
  });
  const pending = h.model.upload([file("a"), file("b")]);
  h.requests[0]!.reject(conflict("a"));
  await turn();
  h.requests[1]!.reject(conflict("b"));
  await pending;
  const answer = h.model.replaceConflict();
  h.requests[2]!.reject(new ApiError(403, "unlock-required", "authorization expired"));
  await answer;
  assert.equal(h.model.uploadRecords[1]?.status, "stopped");
  assert.equal(h.model.conflict, null);
  assert.equal(h.model.conflictRemaining(), 0);
  assert.equal(h.requests.length, 3);
});

test("an explicitly consented replacement can be cancelled only as an unconfirmed outcome", async () => {
  const h = harness();
  const pending = h.model.upload([file("a")]);
  h.requests[0]!.reject(new ApiError(409, "conflict", "exists", {
    reason: "exists", message: "exists",
    existing: { name: "a", dir: false, size: 4, mtime: "2026-09-23T00:00:00Z", version: "v1" },
  }));
  await pending;
  const record = h.model.uploadRecords[0]!;
  const answer = h.model.replaceConflict();
  h.model.cancelUpload(record);
  assert.equal(h.requests[1]!.options.signal?.aborted, true);
  assert.equal(record.status, "unconfirmed");
  h.requests[1]!.reject(new Error("aborted"));
  await answer;
  assert.equal(h.model.conflict, null);
  assert.equal(record.status, "unconfirmed");
  assert.equal(h.requests.length, 2);
});


test("stopping a confirmation wait retains uncertainty and releases the next queued file", async () => {
  const h = harness();
  const work = h.model.upload([file("first.txt"), file("second.txt")]);
  const record = h.model.uploadRecords[0]!;
  const first = h.requests[0]!;
  first.options.onSent?.();
  assert.equal(record.status, "confirming");
  h.model.cancelUpload(record);
  assert.equal(first.options.signal?.aborted, true);
  assert.equal(record.status, "unconfirmed");
  first.options.onProgress?.(4, 4);
  first.options.onSent?.();
  first.resolve({ path: "first.txt", size: 4 });
  await turn();
  assert.equal(record.status, "unconfirmed", "a late response cannot replace the stopped wait's outcome");
  assert.equal(h.requests[1]?.path, "second.txt", "stopping one wait does not stop the rest of the queue");
  h.list("second.txt");
  h.requests[1]!.resolve({ path: "second.txt", size: 4 });
  await work;
  assert.equal(h.model.uploading, false);
  assert.deepEqual(h.model.uploadRecords.map((item) => [item.name, item.status]), [["first.txt", "unconfirmed"]]);
});
