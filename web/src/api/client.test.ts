import { test } from "node:test";
import assert from "node:assert/strict";
import {
  ApiError,
  HttpApiClient,
  csrfToken,
  loginWithPassword,
  type Project,
  type SessionSummary,
  type SidebarLayout,
} from "./client.ts";

function stubFetch(handler: (url: string, init?: RequestInit) => Response) {
  const calls: Array<{ url: string; init?: RequestInit }> = [];
  const original = globalThis.fetch;
  globalThis.fetch = ((url: string | URL | Request, init?: RequestInit) => {
    const u = typeof url === "string" ? url : url instanceof URL ? url.href : url.url;
    calls.push({ url: u, init });
    return Promise.resolve(handler(u, init));
  }) as typeof fetch;
  return {
    calls,
    restore() {
      globalThis.fetch = original;
    },
  };
}

function json(status: number, body: unknown): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

test("upload replacement returns only the explicitly approved server version", async () => {
  const existing = { name: "notes.md", dir: false, size: 12, mtime: "2026-09-23T00:00:00Z", version: "opaque/version+1" };
  const body = new Blob(["new bytes"]);
  const stub = stubFetch((_url, init) => {
    assert.equal(init?.body, body);
    assert.equal(init?.method, "POST");
    assert.equal(init?.credentials, "same-origin");
    assert.equal(new Headers(init?.headers).get("X-CSRF-Token"), "csrf-value");
    return json(409, { error: "conflict", message: "Name exists", reason: "exists", existing });
  });
  try {
    const api = new HttpApiClient(() => "noisedeck_csrf=csrf-value");
    for (const consent of [undefined, { version: existing.version }]) {
      await assert.rejects(api.uploadProjectFile("p/1", "docs/notes.md", body, consent), (error: unknown) => {
        assert.ok(error instanceof ApiError);
        assert.deepEqual(error.conflict?.existing, existing);
        return true;
      });
    }
    assert.equal(stub.calls.length, 2, "a conflict never triggers an automatic retry");
    const first = new URL(stub.calls[0]!.url, "http://localhost");
    const second = new URL(stub.calls[1]!.url, "http://localhost");
    assert.equal(first.pathname, "/api/projects/p%2F1/files");
    assert.equal(first.searchParams.get("path"), "docs/notes.md");
    assert.equal(first.searchParams.has("replace"), false);
    assert.equal(first.searchParams.has("expected_version"), false);
    assert.equal(second.searchParams.get("replace"), "1");
    assert.equal(second.searchParams.get("expected_version"), existing.version);
  } finally { stub.restore(); }
});

test("unknown upload conflict reasons never decode into replacement consent", async () => {
  const stub = stubFetch(() => json(409, {
    error: "conflict", message: "Additional confirmation required", reason: "future_reason",
    existing: { name: "notes.md", dir: false, size: 12, mtime: "t", version: "opaque" },
  }));
  try {
    await assert.rejects(new HttpApiClient().uploadProjectFile("p1", "notes.md", new Blob(["new"])), (error: unknown) => {
      assert.ok(error instanceof ApiError);
      assert.equal(error.conflict, null);
      return true;
    });
    assert.equal(stub.calls.length, 1);
  } finally { stub.restore(); }
});

// ---- the measured upload transport (XHR) ----

// FakeXHR stands in for the browser's XMLHttpRequest so the measured
// path's wire contract and event ordering are testable in node. It
// mirrors the one abort semantic that matters here: abort() dispatches
// its event only once the request was actually sent.
class FakeXHR {
  static instances: FakeXHR[] = [];
  readyState = 0;
  status = 0;
  statusText = "";
  responseText = "";
  withCredentials = false;
  aborted = false;
  sent = false;
  method = "";
  url = "";
  sendBody: Blob | null = null;
  headers: Record<string, string> = {};
  upload: {
    onprogress: ((event: { loaded: number; total: number }) => void) | null;
    onload: (() => void) | null;
  } = { onprogress: null, onload: null };
  onload: (() => void) | null = null;
  onerror: (() => void) | null = null;
  onabort: (() => void) | null = null;

  constructor() {
    FakeXHR.instances.push(this);
  }

  open(method: string, url: string): void {
    this.method = method;
    this.url = url;
    this.readyState = 1;
  }

  setRequestHeader(key: string, value: string): void {
    this.headers[key] = value;
  }

  send(body: Blob): void {
    this.sent = true;
    this.sendBody = body;
  }

  abort(): void {
    this.aborted = true;
    if (this.sent) this.onabort?.();
  }

  emitProgress(loaded: number, total: number): void {
    this.upload.onprogress?.({ loaded, total });
  }

  emitSent(): void {
    this.upload.onload?.();
  }

  respond(status: number, statusText: string, body: string): void {
    this.status = status;
    this.statusText = statusText;
    this.responseText = body;
    this.readyState = 4;
    this.onload?.();
  }

  networkFail(): void {
    this.onerror?.();
  }
}

function stubXHR(): { instances: FakeXHR[]; restore(): void } {
  FakeXHR.instances = [];
  const globalObject = globalThis as { XMLHttpRequest?: unknown };
  const original = globalObject.XMLHttpRequest;
  globalObject.XMLHttpRequest = FakeXHR;
  return {
    instances: FakeXHR.instances,
    restore() {
      globalObject.XMLHttpRequest = original;
    },
  };
}

test("the measured path sends the fetch path's exact request and reports progress as it happens", async () => {
  const stub = stubXHR();
  try {
    const api = new HttpApiClient(() => "noisedeck_csrf=csrf-value");
    const body = new Blob(["0123456789"]);
    const order: string[] = [];
    const pending = api.uploadProjectFile("p/1", "docs/notes.md", body, undefined, {
      onProgress: (loaded, total) => order.push(`progress ${loaded}/${total}`),
      onSent: () => order.push("sent"),
    });
    let resolved = false;
    void pending.then(() => {
      resolved = true;
    });
    const request = stub.instances[0]!;
    assert.equal(request.method, "POST");
    assert.equal(request.url, "/api/projects/p%2F1/files?path=docs%2Fnotes.md");
    // Headers iteration lowercases names, as HTTP treats them anyway.
    assert.equal(request.headers["content-type"], "application/octet-stream", "the raw-body path, not multipart");
    assert.equal(request.headers["x-csrf-token"], "csrf-value");
    assert.equal(request.sendBody, body, "the file's bytes are the request body, unframed");
    request.emitProgress(4, 10);
    request.emitProgress(8, 10);
    request.emitSent();
    await Promise.resolve();
    assert.equal(resolved, false, "everything-sent is not completion; only the response completes");
    request.respond(201, "Created", JSON.stringify({ path: "docs/notes.md", size: 10 }));
    assert.deepEqual(await pending, { path: "docs/notes.md", size: 10 });
    assert.deepEqual(order, ["progress 4/10", "progress 8/10", "sent"],
      "the observations are upload-side facts in the order the browser measured them");
    assert.equal(stub.instances.length, 1, "nothing retries on its own");
  } finally {
    stub.restore();
  }
});

test("consent carries the approved version through the measured path's query", async () => {
  const stub = stubXHR();
  try {
    const pending = new HttpApiClient().uploadProjectFile("p1", "notes.md", new Blob(["new"]), {
      version: "opaque/v+1",
    }, {});
    assert.equal(
      stub.instances[0]!.url,
      "/api/projects/p1/files?path=notes.md&replace=1&expected_version=opaque%2Fv%2B1",
    );
    stub.instances[0]!.respond(200, "OK", JSON.stringify({ path: "notes.md", size: 3 }));
    assert.deepEqual(await pending, { path: "notes.md", size: 3 });
  } finally {
    stub.restore();
  }
});

test("an already-aborted signal dispatches no request at all", async () => {
  const stub = stubXHR();
  try {
    const controller = new AbortController();
    controller.abort();
    await assert.rejects(
      new HttpApiClient().uploadProjectFile("p1", "notes.md", new Blob(["x"]), undefined, {
        signal: controller.signal,
      }),
      (error: unknown) => error instanceof Error && error.name === "AbortError",
    );
    assert.equal(stub.instances.length, 0, "no XHR was created, opened, or sent");
  } finally {
    stub.restore();
  }
});

test("aborting mid-transfer rejects as an outcome the browser cannot name, never a server answer", async () => {
  const stub = stubXHR();
  try {
    const controller = new AbortController();
    const pending = new HttpApiClient().uploadProjectFile("p1", "notes.md", new Blob(["x"]), undefined, {
      signal: controller.signal,
    });
    stub.instances[0]!.emitProgress(1, 2);
    controller.abort();
    await assert.rejects(pending, (error: unknown) =>
      error instanceof Error && error.name === "AbortError" && !(error instanceof ApiError));
    assert.equal(stub.instances[0]!.aborted, true, "the in-flight request was torn down");
    assert.equal(stub.instances.length, 1, "an abort is never replayed automatically");
  } finally {
    stub.restore();
  }
});

test("a lost response stays an unknown outcome with no automatic retry", async () => {
  const stub = stubXHR();
  try {
    const pending = new HttpApiClient().uploadProjectFile("p1", "notes.md", new Blob(["x"]), undefined, {});
    const request = stub.instances[0]!;
    request.emitSent();
    request.networkFail();
    await assert.rejects(pending, (error: unknown) => !(error instanceof ApiError),
      "no server answer arrived, so nothing may read this as the server's refusal");
    assert.equal(stub.instances.length, 1);
  } finally {
    stub.restore();
  }
});

test("a settled upload detaches its abort listener, so a late abort changes nothing", async () => {
  const stub = stubXHR();
  try {
    const controller = new AbortController();
    const pending = new HttpApiClient().uploadProjectFile("p1", "notes.md", new Blob(["x"]), undefined, {
      signal: controller.signal,
    });
    const request = stub.instances[0]!;
    request.respond(200, "OK", JSON.stringify({ path: "notes.md", size: 1 }));
    assert.deepEqual(await pending, { path: "notes.md", size: 1 });
    // AbortController fires its listeners synchronously, so a listener
    // still attached would already have called abort() by here.
    controller.abort();
    assert.equal(request.aborted, false, "the detached listener never reached the finished request");
    assert.equal(stub.instances.length, 1);
  } finally {
    stub.restore();
  }
});

test("the measured path decodes the 409 ask exactly as the fetch path does", async () => {
  const stub = stubXHR();
  try {
    const api = new HttpApiClient();
    const existing = { name: "notes.md", dir: false, size: 12, mtime: "2026-09-23T00:00:00Z", version: "opaque/version+1" };
    const measured = api.uploadProjectFile("p1", "notes.md", new Blob(["new"]), undefined, {});
    stub.instances[0]!.respond(409, "Conflict", JSON.stringify({
      error: "conflict", message: "Name exists", reason: "exists", existing,
    }));
    await assert.rejects(measured, (error: unknown) => {
      assert.ok(error instanceof ApiError);
      assert.equal(error.code, "conflict");
      assert.deepEqual(error.conflict?.existing, existing);
      return true;
    });
    const unknownReason = api.uploadProjectFile("p1", "notes.md", new Blob(["new"]), undefined, {});
    stub.instances[1]!.respond(409, "Conflict", JSON.stringify({
      error: "conflict", message: "Additional confirmation required", reason: "future_reason",
      existing: { name: "notes.md", dir: false, size: 12, mtime: "t", version: "opaque" },
    }));
    await assert.rejects(unknownReason, (error: unknown) => {
      assert.ok(error instanceof ApiError);
      assert.equal(error.conflict, null, "a reason the client does not know never becomes consent");
      return true;
    });
    assert.equal(stub.instances.length, 2, "neither refusal replays");
  } finally {
    stub.restore();
  }
});

test("the measured path reports authorization loss on 401 and the pending-ceremony 403s", async () => {
  const stub = stubXHR();
  try {
    let losses = 0;
    const api = new HttpApiClient();
    api.onAuthorizationLost = () => {
      losses++;
    };
    const body = new Blob(["x"]);
    const unauthorized = api.uploadProjectFile("p1", "notes.md", body, undefined, {});
    stub.instances[0]!.respond(401, "Unauthorized", JSON.stringify({ error: "unauthorized", message: "sign in again" }));
    await assert.rejects(unauthorized, (error: unknown) => {
      assert.ok(error instanceof ApiError);
      assert.equal(error.status, 401);
      assert.equal(error.code, "unauthorized");
      return true;
    });
    assert.equal(losses, 1);
    const locked = api.uploadProjectFile("p1", "notes.md", body, undefined, {});
    stub.instances[1]!.respond(403, "Forbidden", JSON.stringify({ code: "unlock-required", message: "unlock required" }));
    await assert.rejects(locked, (error: unknown) => error instanceof ApiError && error.status === 403 && error.code === "unlock-required");
    assert.equal(losses, 2, "a 403 naming a pending ceremony reads as lost authorization");
    const refused = api.uploadProjectFile("p1", "notes.md", body, undefined, {});
    stub.instances[2]!.respond(403, "Forbidden", JSON.stringify({ error: "forbidden", message: "not allowed" }));
    await assert.rejects(refused, (error: unknown) => error instanceof ApiError && error.status === 403);
    assert.equal(losses, 2, "an ordinary 403 refusal is not an authorization loss");
  } finally {
    stub.restore();
  }
});

test("a successful status with a malformed body stays an unknown outcome", async () => {
  const stub = stubXHR();
  try {
    const pending = new HttpApiClient().uploadProjectFile("p1", "notes.md", new Blob(["x"]), undefined, {});
    stub.instances[0]!.respond(200, "OK", "not json");
    await assert.rejects(pending, (error: unknown) => !(error instanceof ApiError),
      "the fetch path's readJSON rejects the same way, so neither path invents a stored file");
  } finally {
    stub.restore();
  }
});

test("the measured path fails loudly where no XMLHttpRequest exists rather than inventing progress", async () => {
  if (typeof XMLHttpRequest !== "undefined") return;
  await assert.rejects(
    new HttpApiClient().uploadProjectFile("p1", "notes.md", new Blob(["x"]), undefined, { onSent() {} }),
    /XMLHttpRequest/,
  );
});

test("only the matching file result confirms a measured upload", async () => {
  const stub = stubXHR();
  try {
    for (const response of ["", "null", "{}", '{"path":"other.md","size":1}', '{"path":"notes.md","size":2}']) {
      const pending = new HttpApiClient().uploadProjectFile("p1", "notes.md", new Blob(["x"]), undefined, {});
      stub.instances.at(-1)!.respond(response === "" ? 204 : 200, "OK", response);
      await assert.rejects(pending, (error: unknown) => !(error instanceof ApiError));
    }
    assert.equal(stub.instances.length, 5, "unconfirmed responses never trigger retries");
  } finally { stub.restore(); }
});

test("a synchronous send failure detaches its abort listener", async () => {
  const stub = stubXHR();
  const send = FakeXHR.prototype.send;
  const controller = new AbortController();
  try {
    FakeXHR.prototype.send = () => { throw new Error("send refused"); };
    await assert.rejects(new HttpApiClient().uploadProjectFile("p1", "notes.md", new Blob(["x"]), undefined, {
      signal: controller.signal,
    }), /send refused/);
    controller.abort();
    assert.equal(stub.instances[0]!.aborted, false, "the rejected request no longer observes cancellation");
  } finally { FakeXHR.prototype.send = send; stub.restore(); }
});

test("callers that pass no transport options keep the fetch path untouched", async () => {
  const xhrStub = stubXHR();
  const fetchStub = stubFetch(() => json(201, { path: "notes.md", size: 4 }));
  try {
    const api = new HttpApiClient(() => "noisedeck_csrf=csrf-value");
    assert.deepEqual(
      await api.uploadProjectFile("p1", "notes.md", new Blob(["x"])),
      { path: "notes.md", size: 4 },
    );
    assert.equal(xhrStub.instances.length, 0, "no XHR was constructed");
    assert.equal(fetchStub.calls.length, 1);
    assert.equal(fetchStub.calls[0]!.init?.credentials, "same-origin");
  } finally {
    fetchStub.restore();
    xhrStub.restore();
  }
});

test("listProjects bypasses caches and parses the authoritative response", async () => {
  const projects: Project[] = [
    { id: "p1", name: "demo", canonical_path: "/x", last_used_at: "t" },
  ];
  const stub = stubFetch((url, init) => {
    assert.equal(url, "/api/projects");
    assert.equal(init?.cache, "no-store");
    assert.equal(init?.credentials, "same-origin");
    return json(200, projects);
  });
  try {
    const api = new HttpApiClient();
    assert.deepEqual(await api.listProjects(), projects);
  } finally {
    stub.restore();
  }
});

// The harness inventory changes with the image the binary runs in, so a
// cached answer would draw the wrong console's chips.
test("listProviders reads the served inventory uncached", async () => {
  const stub = stubFetch((url, init) => {
    assert.equal(url, "/api/providers");
    assert.equal(init?.cache, "no-store");
    assert.equal(init?.credentials, "same-origin");
    return json(200, { providers: ["claude", "codex", "shell"] });
  });
  try {
    assert.deepEqual(await new HttpApiClient().listProviders(), ["claude", "codex", "shell"]);
  } finally {
    stub.restore();
  }
});

// Go encodes a nil slice as null, and the picker maps over what it gets.
test("listProviders reads a null inventory as an empty one", async () => {
  const stub = stubFetch(() => json(200, { providers: null }));
  try {
    assert.deepEqual(await new HttpApiClient().listProviders(), []);
  } finally {
    stub.restore();
  }
});

test("addTaskmasterTask posts the exact fields with mutation protections", async () => {
  const input = {
    title: "literal; $(payload)",
    description: "description",
    details: "details",
    priority: "high" as const,
  };
  const stub = stubFetch((url, init) => {
    assert.equal(url, "/api/projects/p%2F1/taskmaster/tasks");
    assert.equal(init?.method, "POST");
    assert.equal(init?.credentials, "same-origin");
    assert.equal(init?.cache, "no-store");
    assert.equal(init?.body, JSON.stringify(input));
    assert.equal(new Headers(init?.headers).get("Content-Type"), "application/json");
    return json(200, { id: "18" });
  });
  try {
    assert.deepEqual(await new HttpApiClient().addTaskmasterTask("p/1", input), { id: "18" });
  } finally {
    stub.restore();
  }
});

test("requireSession probes authentication without caching", async () => {
  const stub = stubFetch((url, init) => {
    assert.equal(url, "/auth/session");
    assert.equal(init?.cache, "no-store");
    assert.equal(init?.credentials, "same-origin");
    return new Response(null, {
      status: 204,
      headers: { "X-Noisedeck-Auth-Mode": "builtin" },
    });
  });
  try {
    assert.equal(await new HttpApiClient().requireSession(), "builtin");
  } finally {
    stub.restore();
  }
});

test("requireSession accepts the canonical tier vocabulary", async () => {
  for (const mode of ["none", "password", "oidc", "trusted_proxy"] as const) {
    const stub = stubFetch(() => new Response(null, {
      status: 204,
      headers: { "X-Noisedeck-Auth-Mode": mode },
    }));
    try {
      assert.equal(await new HttpApiClient().requireSession(), mode);
    } finally {
      stub.restore();
    }
  }
});

test("requireSession preserves the server auth mode on a refusal", async () => {
  const stub = stubFetch(() => new Response("unauthorized", {
    status: 401,
    headers: { "X-Noisedeck-Auth-Mode": "trusted_proxy" },
  }));
  try {
    await assert.rejects(new HttpApiClient().requireSession(), (cause: unknown) => {
      assert(cause instanceof ApiError);
      assert.equal(cause.code, "unauthorized_trusted_proxy");
      return true;
    });
  } finally {
    stub.restore();
  }
});

test("password login sends one bounded JSON credential request", async () => {
  const stub = stubFetch((url, init) => {
    assert.equal(url, "/auth/password");
    assert.equal(init?.method, "POST");
    assert.equal(init?.credentials, "same-origin");
    assert.equal(init?.cache, "no-store");
    assert.equal(new Headers(init?.headers).get("Content-Type"), "application/json");
    assert.equal(init?.body, JSON.stringify({ password: "secret" }));
    return new Response(null, { status: 204 });
  });
  try {
    await loginWithPassword("secret");
  } finally {
    stub.restore();
  }
});

test("listProjects normalizes the backend's nil-slice null on a fresh database", async () => {
  const stub = stubFetch(() => json(200, null));
  try {
    assert.deepEqual(await new HttpApiClient().listProjects(), []);
  } finally {
    stub.restore();
  }
});

test("listProjectDirectories browses container paths with encoded queries", async () => {
  const listing = {
    current_path: "/workspace/projects",
    parent_path: null,
    directories: [{ name: "audio-player", path: "/workspace/projects/audio-player" }],
  };
  const stub = stubFetch((url, init) => {
    assert.equal(url, "/api/project-directories?path=%2Fworkspace%2Fprojects");
    assert.equal(init?.cache, "no-store");
    return json(200, listing);
  });
  try {
    assert.deepEqual(
      await new HttpApiClient().listProjectDirectories("/workspace/projects"),
      listing,
    );
  } finally {
    stub.restore();
  }
});

test("createProject posts the user-selected name and path", async () => {
  const project: Project = {
    id: "p1", name: "demo", canonical_path: "/projects/demo", last_used_at: "t",
  };
  const stub = stubFetch((url, init) => {
    assert.equal(url, "/api/projects");
    assert.equal(init?.method, "POST");
    assert.deepEqual(JSON.parse(String(init?.body)), {
      name: "demo",
      path: "/projects/demo",
      plain: false,
      git_identity_profile_id: "identity-1",
      expected_git_identity_revision: 3,
    });
    return json(201, project);
  });
  try {
    assert.deepEqual(
      await new HttpApiClient().createProject("demo", "/projects/demo", {
        id: "identity-1", name: "Test Author", email: "test@example.invalid", revision: 3,
        created_at: "t", updated_at: "t",
      }),
      project,
    );
  } finally {
    stub.restore();
  }
});

test("Git identity mutations carry exact CAS revisions", async () => {
  const profile = {
    id: "identity/1", name: "Deck Author", email: "deck@example.invalid", revision: 4,
    created_at: "t", updated_at: "t",
  };
  const calls: Array<{ url: string; method?: string; body: unknown }> = [];
  const stub = stubFetch((url, init) => {
    calls.push({ url, method: init?.method, body: init?.body ? JSON.parse(String(init.body)) : null });
    if (url === "/api/git-identities" || url === "/api/git-identities/identity%2F1") {
      return json(init?.method === "POST" ? 201 : 200, profile);
    }
    if (url === "/api/git-identities/default") {
      return json(200, { profiles: [profile], default_profile_id: profile.id, revision: 8 });
    }
    if (url === "/api/projects/project%2F1/git-identity") {
      return json(200, { id: "project/1", name: "p", canonical_path: "/p", git_identity_setup_revision: 3, last_used_at: "t" });
    }
    return new Response(null, { status: 204 });
  });
  try {
    const api = new HttpApiClient();
    await api.createGitIdentity(profile.name, profile.email);
    await api.updateGitIdentity(profile.id, profile.revision, "Updated", "updated@example.invalid");
    await api.setDefaultGitIdentity(profile.id, 7, profile.revision);
    await api.deleteGitIdentity(profile.id, profile.revision, 8);
    await api.setProjectGitIdentity("project/1", {
      profile_id: profile.id,
      expected_profile_revision: profile.revision,
      expected_project_setup_revision: 2,
    });
    assert.deepEqual(calls, [
      { url: "/api/git-identities", method: "POST", body: {
        name: "Deck Author", email: "deck@example.invalid",
      } },
      { url: "/api/git-identities/identity%2F1", method: "PUT", body: {
        name: "Updated", email: "updated@example.invalid", expected_revision: 4,
      } },
      { url: "/api/git-identities/default", method: "PUT", body: {
        profile_id: "identity/1", expected_revision: 7, expected_profile_revision: 4,
      } },
      { url: "/api/git-identities/identity%2F1", method: "DELETE", body: {
        expected_revision: 4, catalog_revision: 8,
        replacement_default_id: "", expected_replacement_revision: 0,
      } },
      { url: "/api/projects/project%2F1/git-identity", method: "PUT", body: {
        profile_id: "identity/1", expected_profile_revision: 4,
        expected_project_setup_revision: 2,
      } },
    ]);
  } finally {
    stub.restore();
  }
});

test("a project write reports the repository-local values it replaced", async () => {
  const stub = stubFetch(() => json(200, {
    id: "p1", name: "demo", canonical_path: "/projects/demo", last_used_at: "t",
    replaced_git_identity: { name: "Existing", email: "existing@example.invalid" },
  }));
  try {
    const updated = await new HttpApiClient().setProjectGitIdentity("p1", {
      profile_id: "i1", expected_profile_revision: 1, expected_project_setup_revision: 0,
    });
    assert.deepEqual(updated.replaced_git_identity, {
      name: "Existing", email: "existing@example.invalid",
    });
  } finally {
    stub.restore();
  }
});

test("deleteProject unregisters via DELETE and accepts the empty 204", async () => {
  const stub = stubFetch((url, init) => {
    assert.equal(url, "/api/projects/p%2F1");
    assert.equal(init?.method, "DELETE");
    assert.equal(init?.cache, "no-store");
    return new Response(null, { status: 204 });
  });
  try {
    await new HttpApiClient().deleteProject("p/1");
  } finally {
    stub.restore();
  }
});

test("deleteProject surfaces the live-session refusal as a coded ApiError", async () => {
  const stub = stubFetch(() =>
    json(409, { error: "live_sessions", message: "stop them before removing the project" }));
  try {
    await assert.rejects(new HttpApiClient().deleteProject("p1"), (cause: unknown) => {
      assert(cause instanceof ApiError);
      assert.equal(cause.status, 409);
      assert.equal(cause.code, "live_sessions");
      return true;
    });
  } finally {
    stub.restore();
  }
});

test("renameProject posts the display name and accepts the empty 204", async () => {
  const stub = stubFetch((url, init) => {
    assert.equal(url, "/api/projects/p%2F1/rename");
    assert.equal(init?.method, "POST");
    assert.equal(init?.cache, "no-store");
    assert.deepEqual(JSON.parse(String(init?.body)), { name: "Noise Deck" });
    return new Response(null, { status: 204 });
  });
  try {
    await new HttpApiClient().renameProject("p/1", "Noise Deck");
  } finally {
    stub.restore();
  }
});

test("renameProject surfaces the server refusal as a coded ApiError", async () => {
  const stub = stubFetch(() =>
    json(404, { error: "not_found", message: "project not found" }));
  try {
    await assert.rejects(new HttpApiClient().renameProject("p1", "x"), (cause: unknown) => {
      assert(cause instanceof ApiError);
      assert.equal(cause.status, 404);
      assert.equal(cause.code, "not_found");
      return true;
    });
  } finally {
    stub.restore();
  }
});

// The project app preview entry: a CSRF-protected POST that
// carries no body and answers the exact HTTPS entry URL plus its one-use
// ticket — the only shape the open flow will submit to.
test("projectPreview posts with CSRF protection and parses the entry", async () => {
  const stub = stubFetch((url, init) => {
    assert.equal(url, "/api/projects/p%2F1/preview");
    assert.equal(init?.method, "POST");
    assert.equal(init?.credentials, "same-origin");
    assert.equal(init?.cache, "no-store");
    assert.equal(init?.body, undefined);
    assert.equal(new Headers(init?.headers).get("Content-Type"), null);
    assert.equal(new Headers(init?.headers).get("X-CSRF-Token"), "csrf-value");
    return json(200, { url: "https://app.preview.example/entry", ticket: "one-use" });
  });
  try {
    const api = new HttpApiClient(() => "noisedeck_csrf=csrf-value");
    assert.deepEqual(await api.projectPreview("p/1"), {
      url: "https://app.preview.example/entry",
      ticket: "one-use",
    });
  } finally {
    stub.restore();
  }
});

// The disabled/unconfigured answer is a 404 in the nested envelope. The
// sidebar renders this message verbatim, so the decode must keep the
// server's own sentence.
test("projectPreview surfaces the unconfigured refusal verbatim", async () => {
  const stub = stubFetch(() =>
    json(404, { error: { code: "preview_unavailable", message: "Preview is not configured for this project." } }));
  try {
    await assert.rejects(new HttpApiClient().projectPreview("p1"), (cause: unknown) => {
      assert(cause instanceof ApiError);
      assert.equal(cause.status, 404);
      assert.equal(cause.code, "preview_unavailable");
      assert.equal(cause.message, "Preview is not configured for this project.");
      return true;
    });
  } finally {
    stub.restore();
  }
});

test("listSessions encodes the project id and normalizes a nil slice", async () => {
  const stub = stubFetch((url) => {
    assert.equal(url, "/api/projects/a%2Fb/sessions");
    return json(200, null);
  });
  try {
    const api = new HttpApiClient();
    assert.deepEqual(await api.listSessions("a/b"), []);
  } finally {
    stub.restore();
  }
});

test("createSession posts the provider and parses the session", async () => {
  const session: SessionSummary = {
    id: "s1",
    project_id: "p1",
    provider: "claude",
    display_name: "",
    status: "running",
    generation: 1,
    created_at: "t",
    updated_at: "t",
    recovery_capability: "none",
    recovery_state: "unavailable",
    presentation_state: "running",
  };
  const stub = stubFetch((url, init) => {
    assert.equal(url, "/api/projects/p1/sessions");
    assert.equal(init?.method, "POST");
    assert.deepEqual(JSON.parse(String(init?.body)), { provider: "claude" });
    return json(201, session);
  });
  try {
    const api = new HttpApiClient();
    assert.deepEqual(await api.createSession("p1", "claude"), session);
  } finally {
    stub.restore();
  }
});

test("builtin OIDC mutations carry the session-bound CSRF cookie", async () => {
  const stub = stubFetch((_url, init) => {
    const headers = new Headers(init?.headers);
    assert.equal(headers.get("X-CSRF-Token"), "csrf-value");
    return json(201, {
      id: "s1", project_id: "p1", provider: "claude", display_name: "", status: "running",
      generation: 1, created_at: "t", updated_at: "t",
      recovery_capability: "none", recovery_state: "unavailable", presentation_state: "running",
    });
  });
  try {
    const api = new HttpApiClient(() => "theme=dark; noisedeck_csrf=csrf-value; other=x");
    await api.createSession("p1", "claude");
  } finally {
    stub.restore();
  }
});

test("logout is CSRF-protected and bypasses caches", async () => {
  const stub = stubFetch((url, init) => {
    assert.equal(url, "/auth/logout");
    assert.equal(init?.method, "POST");
    assert.equal(init?.cache, "no-store");
    assert.equal(init?.credentials, "same-origin");
    assert.equal(new Headers(init?.headers).get("X-CSRF-Token"), "csrf-value");
    return new Response(null, { status: 204 });
  });
  try {
    await new HttpApiClient(() => "noisedeck_csrf=csrf-value").logout();
  } finally {
    stub.restore();
  }
});

test("trusted-proxy mutations omit CSRF when no builtin cookie exists", async () => {
  const stub = stubFetch((_url, init) => {
    assert.equal(new Headers(init?.headers).has("X-CSRF-Token"), false);
    return new Response(null, { status: 204 });
  });
  try {
    await new HttpApiClient(() => "unrelated=x").deleteSession("s1");
  } finally {
    stub.restore();
  }
});

test("csrfToken accepts secure cookies and refuses malformed encoding", () => {
  assert.equal(csrfToken("x=1; __Host-noisedeck_csrf=secure%2Dtoken"), "secure-token");
  assert.equal(
    csrfToken("noisedeck_csrf=plain; __Host-noisedeck_csrf=secure"),
    "secure",
  );
  assert.equal(
    csrfToken("__Host-noisedeck_csrf=secure; noisedeck_csrf=%E0%A4%A"),
    "secure",
  );
  assert.equal(csrfToken("noisedeck_csrf=%E0%A4%A"), null);
  assert.equal(csrfToken("x=1"), null);
});

test("ApiError carries status, code, and the server's message", async () => {
  const stub = stubFetch(() =>
    json(409, { error: "provider_not_launchable", message: "identity capture pending" }),
  );
  try {
    const api = new HttpApiClient();
    await assert.rejects(api.createSession("p1", "codex"), (err: unknown) => {
      assert.ok(err instanceof ApiError);
      assert.equal(err.status, 409);
      assert.equal(err.code, "provider_not_launchable");
      assert.equal(err.message, "identity capture pending");
      return true;
    });
  } finally {
    stub.restore();
  }
});

test("non-JSON error bodies still become ApiError", async () => {
  const stub = stubFetch(() => new Response("boom", { status: 500, statusText: "Internal Server Error" }));
  try {
    const api = new HttpApiClient();
    await assert.rejects(api.listProjects(), (err: unknown) => {
      assert.ok(err instanceof ApiError);
      assert.equal(err.status, 500);
      return true;
    });
  } finally {
    stub.restore();
  }
});

test("a 204 No Content success resolves instead of throwing", async () => {
  const stub = stubFetch(() => new Response(null, { status: 204 }));
  try {
    const api = new HttpApiClient();
    await api.deleteSession("s1");
  } finally {
    stub.restore();
  }
});

test("a nested error envelope decodes like the flat one", async () => {
  // Some handlers write {"error": {"code", "message"}} and others write them
  // flat. The client must surface both identically, whichever it meets.
  const stub = stubFetch(() =>
    json(409, { error: { code: "input_line_dirty", message: "clear the line first" } }),
  );
  try {
    const api = new HttpApiClient();
    await assert.rejects(
      api.getSession("s1"),
      (err: unknown) => {
        assert.ok(err instanceof ApiError);
        assert.equal(err.status, 409);
        assert.equal(err.code, "input_line_dirty");
        assert.equal(err.message, "clear the line first");
        return true;
      },
    );
  } finally {
    stub.restore();
  }
});

test("getLayout reads the sidebar arrangement document", async () => {
  const layout: SidebarLayout = {
    expanded: ["p1"],
    projects: ["p2", "p1"],
    sessions: { p1: ["s2", "s1"] },
  };
  const stub = stubFetch((url, init) => {
    assert.equal(url, "/api/settings/layout");
    assert.equal(init?.cache, "no-store");
    assert.equal(init?.credentials, "same-origin");
    return json(200, layout);
  });
  try {
    assert.deepEqual(await new HttpApiClient().getLayout(), layout);
  } finally {
    stub.restore();
  }
});

test("getLayout returns null on a 404 so the sidebar keeps its defaults", async () => {
  const stub = stubFetch((url) => {
    assert.equal(url, "/api/settings/layout");
    return json(404, { error: "not_found", message: "no layout saved" });
  });
  try {
    assert.equal(await new HttpApiClient().getLayout(), null);
  } finally {
    stub.restore();
  }
});

test("saveLayout replaces the whole arrangement document", async () => {
  const layout: SidebarLayout = {
    expanded: ["p1"],
    projects: ["p2", "p1"],
    sessions: {},
  };
  const stub = stubFetch((url, init) => {
    assert.equal(url, "/api/settings/layout");
    assert.equal(init?.method, "PUT");
    assert.equal(init?.cache, "no-store");
    assert.equal(init?.credentials, "same-origin");
    assert.equal(new Headers(init?.headers).get("Content-Type"), "application/json");
    assert.equal(init?.body, JSON.stringify(layout));
    return json(200, layout);
  });
  try {
    assert.deepEqual(await new HttpApiClient().saveLayout(layout), layout);
  } finally {
    stub.restore();
  }
});

// A fake EventSource that records its listeners: the browser's own reconnect
// is not reachable from a unit test, so the reopen it produces is delivered
// by dispatching a second "open" here.
function stubEventSource() {
  const original = (globalThis as { EventSource?: unknown }).EventSource;
  const sources: Array<{
    url: string;
    closed: boolean;
    emit(type: string): void;
  }> = [];
  class FakeEventSource {
    private listeners = new Map<string, Array<() => void>>();
    closed = false;
    url: string;
    constructor(url: string) {
      this.url = url;
      sources.push(this);
    }
    addEventListener(type: string, handler: () => void): void {
      const bucket = this.listeners.get(type) ?? [];
      bucket.push(handler);
      this.listeners.set(type, bucket);
    }
    emit(type: string): void {
      for (const handler of this.listeners.get(type) ?? []) handler();
    }
    close(): void {
      this.closed = true;
    }
  }
  (globalThis as { EventSource?: unknown }).EventSource = FakeEventSource;
  return {
    sources,
    restore() {
      (globalThis as { EventSource?: unknown }).EventSource = original;
    },
  };
}

test("a file stream reports every reopen as a change, but not its first connection", () => {
  const stub = stubEventSource();
  try {
    let changes = 0;
    const unsubscribe = new HttpApiClient().subscribeProjectFileEvents("p 1", "a/b", () => {
      changes += 1;
    });
    const events = stub.sources[0];
    assert.ok(events !== undefined, "subscribing opens exactly one stream");
    assert.equal(events.url, "/api/projects/p%201/files/events?path=a%2Fb");

    events.emit("open");
    assert.equal(changes, 0, "the caller has just listed; the first connection is not news");

    events.emit("files-changed");
    assert.equal(changes, 1);

    // A dropped connection: whatever changed during the gap is already in the
    // server's fresh baseline, so the reopen is the only signal there is.
    events.emit("open");
    assert.equal(changes, 2);

    unsubscribe();
    assert.equal(events.closed, true);
  } finally {
    stub.restore();
  }
});

test("an asset stream reads assets-changed and reports every reopen as a change", () => {
  const stub = stubEventSource();
  try {
    let changes = 0;
    const unsubscribe = new HttpApiClient().subscribeProjectAssetEvents("p 1", () => {
      changes += 1;
    });
    const events = stub.sources[0];
    assert.ok(events !== undefined, "subscribing opens exactly one stream");
    assert.equal(events.url, "/api/projects/p%201/assets/events");
    events.emit("open");
    events.emit("files-changed");
    assert.equal(changes, 0, "neither the first connection nor the files feed's event is an asset change");
    events.emit("assets-changed");
    assert.equal(changes, 1);
    events.emit("open");
    assert.equal(changes, 2);
    unsubscribe();
    assert.equal(events.closed, true);
  } finally {
    stub.restore();
  }
});

test("listProjectAssets reads the scan uncached", async () => {
  const listing = { locations: [], scanned: "2026-10-18T17:00:00Z", preview_max_bytes: 16777216 };
  const stub = stubFetch((url, init) => {
    assert.equal(url, "/api/projects/p%2F1/assets");
    assert.equal(init?.cache, "no-store");
    return json(200, listing);
  });
  try {
    assert.deepEqual(await new HttpApiClient().listProjectAssets("p/1"), listing);
  } finally {
    stub.restore();
  }
});

test("deleteProjectAssets posts only path and version and returns the per-file results", async () => {
  const results = [
    { path: ".deck/assets/a.png", outcome: "deleted" },
    { path: ".deck/assets/b.png", outcome: "skipped", reason: "Already gone" },
  ];
  const stub = stubFetch((url, init) => {
    assert.equal(url, "/api/projects/p%2F1/assets/delete");
    assert.equal(init?.method, "POST");
    assert.equal(init?.credentials, "same-origin");
    assert.equal(init?.cache, "no-store");
    assert.equal(new Headers(init?.headers).get("Content-Type"), "application/json");
    assert.equal(init?.body, JSON.stringify({ files: [
      { path: ".deck/assets/a.png", version: "1-a" },
      { path: ".deck/assets/b.png", version: "1-b" },
    ] }));
    return json(200, { results });
  });
  try {
    const selected = [
      { path: ".deck/assets/a.png", version: "1-a", name: "a.png" },
      { path: ".deck/assets/b.png", version: "1-b" },
    ];
    assert.deepEqual(await new HttpApiClient().deleteProjectAssets("p/1", selected), results);
  } finally {
    stub.restore();
  }
});

test("a refused asset delete rejects with the server's error", async () => {
  const stub = stubFetch(() => json(400, { error: "bad_request", message: "at most 500 files per request" }));
  try {
    await assert.rejects(
      new HttpApiClient().deleteProjectAssets("p", [{ path: "x", version: "y" }]),
      (err: unknown) => err instanceof ApiError && err.status === 400 && err.code === "bad_request",
    );
  } finally {
    stub.restore();
  }
});

test("preview settings and checks encode the project and preserve auth/cache boundaries", async () => {
  const seen: string[] = [];
  const stub = stubFetch((url, init) => {
    const method = init?.method ?? "GET";
    seen.push(method);
    assert.equal(url, `/api/projects/p%2F1/preview/${method === "POST" ? "check" : "settings"}`);
    assert.equal(init?.credentials, "same-origin");
    assert.equal(init?.cache, "no-store");
    assert.equal(init?.body, undefined, "the browser cannot select an upstream or port");
    if (method !== "GET") assert.equal(new Headers(init?.headers).get("X-CSRF-Token"), "csrf-value");
    return json(200, method === "POST" ? { reachable: true, checked_at: "2026-09-21T00:00:00Z" } : { configured: true, self_service: true, enabled: method !== "DELETE", origin: "https://p.preview.example", port: 41207 });
  });
  try {
    const api = new HttpApiClient(() => "noisedeck_csrf=csrf-value");
    assert.equal((await api.getProjectPreviewSettings("p/1")).configured, true);
    assert.equal((await api.enableProjectPreview("p/1")).enabled, true);
    assert.equal((await api.disableProjectPreview("p/1")).enabled, false);
    assert.equal((await api.checkProjectPreview("p/1")).reachable, true);
    assert.deepEqual(seen, ["GET", "PUT", "DELETE", "POST"]);
  } finally { stub.restore(); }
});

test("preview actions report lost authorization before returning the refusal", async () => {
  for (const [status, code] of [[401, "unauthorized"], [403, "unlock-required"], [403, "authorization-required"], [403, "browser-grant-required"]] as const) {
    const stub = stubFetch(() => json(status, { code }));
    try {
      const api = new HttpApiClient();
      let losses = 0;
      api.onAuthorizationLost = () => { losses += 1; };
      const actions = [() => api.enableProjectPreview("p1"), () => api.disableProjectPreview("p1"), () => api.checkProjectPreview("p1")];
      for (const [index, action] of actions.entries()) {
        await assert.rejects(action, (error: unknown) => {
          assert.equal(losses, index + 1, "private UI teardown precedes the caller's error handling");
          return error instanceof ApiError && error.status === status;
        });
      }
    } finally { stub.restore(); }
  }
});

test("a preview permission refusal does not revoke the workspace grant", async () => {
  const stub = stubFetch(() => json(403, { error: "preview-management-disabled" }));
  try {
    const api = new HttpApiClient();
    let losses = 0;
    api.onAuthorizationLost = () => { losses += 1; };
    await assert.rejects(() => api.enableProjectPreview("p1"), (error: unknown) => error instanceof ApiError && error.code === "preview-management-disabled");
    assert.equal(losses, 0);
  } finally { stub.restore(); }
});

// A missing tutorial preference is the first-use signal, not an error: the
// 404 maps to null exactly like the layout read.
test("getTutorial maps a missing record to null", async () => {
  const stub = stubFetch((url, init) => {
    assert.equal(url, "/api/settings/tutorial");
    assert.equal(init?.credentials, "same-origin");
    assert.equal(init?.cache, "no-store");
    return json(404, { error: "not_found", message: "no tutorial preference saved" });
  });
  try {
    assert.equal(await new HttpApiClient().getTutorial(), null);
  } finally {
    stub.restore();
  }
});

test("getTutorial returns the stored document with its scope and the caller's signal", async () => {
  const record = { version: 1, dismissed: true, lesson: "navigation", step: 2, done: ["welcome"], scope: "installation" };
  const controller = new AbortController();
  let seen: AbortSignal | undefined;
  const stub = stubFetch((_url, init) => {
    seen = init?.signal ?? undefined;
    return json(200, record);
  });
  try {
    const api = new HttpApiClient();
    assert.deepEqual(await api.getTutorial(controller.signal), record);
    // The caller's signal no longer passes through by identity: every
    // request is composed with the logout signal, so EITHER aborting must
    // cancel the request.
    assert.ok(seen instanceof AbortSignal, "the request carries a signal");
    assert.equal(seen.aborted, false, "an open caller signal and an open logout signal compose open");
    controller.abort();
    assert.equal(seen.aborted, true, "the caller's abort cancels the composed signal");
  } finally {
    stub.restore();
  }
});

test("saveTutorial replaces the whole document under CSRF protection", async () => {
  const document = { version: 1, dismissed: false, lesson: "welcome", step: 1, done: [] };
  const stub = stubFetch((url, init) => {
    assert.equal(url, "/api/settings/tutorial");
    assert.equal(init?.method, "PUT");
    assert.equal(init?.credentials, "same-origin");
    assert.equal(init?.cache, "no-store");
    assert.equal(init?.body, JSON.stringify(document));
    assert.equal(new Headers(init?.headers).get("Content-Type"), "application/json");
    assert.equal(new Headers(init?.headers).get("X-CSRF-Token"), "csrf-value");
    return json(200, { ...document, scope: "user" });
  });
  try {
    const api = new HttpApiClient(() => "noisedeck_csrf=csrf-value");
    assert.deepEqual(await api.saveTutorial(document), { ...document, scope: "user" });
  } finally {
    stub.restore();
  }
});

// The server refuses rather than resolves a write over a stored record it
// cannot speak; the client surfaces that refusal unchanged so the caller
// can stop writing instead of misreading the record.
test("saveTutorial reports a stored-version conflict as an ApiError", async () => {
  const stub = stubFetch(() => json(409, { error: "version_conflict", message: "the stored tutorial preference belongs to a contract version this build does not speak; refusing to overwrite it" }));
  try {
    await assert.rejects(new HttpApiClient().saveTutorial({ version: 1, dismissed: true, lesson: "welcome", step: 0, done: [] }), (cause: unknown) => {
      assert(cause instanceof ApiError);
      assert.equal(cause.status, 409);
      assert.equal(cause.code, "version_conflict");
      return true;
    });
  } finally {
    stub.restore();
  }
});

test("generateTasks sends an optional harness and preserves omitted fields", async () => {
  const stub = stubFetch(() => json(202, { id: "run" }));
  try {
    const api = new HttpApiClient(() => "noisedeck_csrf=test");
    await api.generateTasks("p/1", "PRD.md", 2, "codex");
    await api.generateTasks("p/1", "PRD.md", undefined, "kimi");
    await api.generateTasks("p/1", "PRD.md");
    assert.deepEqual(stub.calls.map((call) => JSON.parse(String(call.init?.body))), [
      { prd: "PRD.md", numTasks: 2, harness: "codex" },
      { prd: "PRD.md", harness: "kimi" },
      { prd: "PRD.md" },
    ]);
    assert.ok(stub.calls.every((call) => call.url === "/api/projects/p%2F1/taskmaster/generate" && call.init?.method === "POST"));
  } finally { stub.restore(); }
});

test("logout aborts in-flight requests before its own POST leaves", async () => {
  const calls: string[] = [];
  let releaseRenewal: (value: Response) => void = () => {};
  const renewal = new Promise<Response>((resolve) => { releaseRenewal = resolve; });
  const original = globalThis.fetch;
  // A signal-honoring fetch: it resolves when the handler says, and rejects
  // the moment the request's signal aborts, which is the real fetch's
  // contract the logout race depends on.
  globalThis.fetch = ((_url: string | URL | Request, init?: RequestInit) => {
    calls.push(init?.signal?.aborted === true ? "aborted" : "open");
    const body = calls.length === 1 ? renewal
      : Promise.resolve(new Response(null, { status: 204 }));
    return new Promise<Response>((resolve, reject) => {
      const abort = (): void => reject(new DOMException("aborted", "AbortError"));
      if (init?.signal?.aborted) { abort(); return; }
      init?.signal?.addEventListener("abort", abort, { once: true });
      body.then((value: Response) => { init?.signal?.removeEventListener("abort", abort); resolve(value); }, reject);
    });
  }) as typeof fetch;
  try {
    const api = new HttpApiClient();
    const inflight = api.listProjects();
    await api.logout();
    // The straggler's response would set a cookie after logout; the abort
    // means the fetch never applies it, whatever the network did.
    releaseRenewal(new Response("[]", { status: 200, headers: { "Content-Type": "application/json" } }));
    await assert.rejects(inflight, (cause: unknown) => cause instanceof Error && cause.name === "AbortError");
    assert.deepEqual(calls, ["open", "open"], "the logout POST itself is not aborted");
  } finally {
    globalThis.fetch = original;
  }
});

test("a failed logout re-arms the client instead of bricking every request", async () => {
  const original = globalThis.fetch;
  let calls = 0;
  globalThis.fetch = ((_url: string | URL | Request, _init?: RequestInit) => {
    calls += 1;
    if (calls === 1) return Promise.reject(new Error("network down"));
    return Promise.resolve(new Response("[]", { status: 200, headers: { "Content-Type": "application/json" } }));
  }) as typeof fetch;
  try {
    const api = new HttpApiClient();
    await assert.rejects(api.logout(), /network down/);
    // The session may still be live: the next ordinary request must work,
    // not reject with the abort from the failed logout attempt.
    assert.deepEqual(await api.listProjects(), []);
  } finally {
    globalThis.fetch = original;
  }
});
