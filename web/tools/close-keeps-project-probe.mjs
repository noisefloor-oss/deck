// Browser probe for the close-keeps-project contract: closing a session
// leaves that session's project in context, so the empty pane names it and
// the pane's New session action targets it instead of defaulting to the
// first project in the rail.
import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import { existsSync } from "node:fs";
import net from "node:net";

if (process.env.PLAYWRIGHT_BROWSERS_PATH === undefined &&
    existsSync("/workspace/tools/ms-playwright")) {
  process.env.PLAYWRIGHT_BROWSERS_PATH = "/workspace/tools/ms-playwright";
}
const { browser: getBrowser } = await import("./browser.mjs");

const port = await freePort();
const origin = `http://127.0.0.1:${port}`;
let serverOutput = "";
const vite = spawn(
  process.env.PNPM_BIN ?? "pnpm",
  ["exec", "vite", "--host", "127.0.0.1", "--port", String(port), "--strictPort"],
  { cwd: new URL("..", import.meta.url), stdio: ["ignore", "pipe", "pipe"] },
);
vite.stdout.on("data", (chunk) => { serverOutput += chunk; });
vite.stderr.on("data", (chunk) => { serverOutput += chunk; });

function freePort() {
  return new Promise((resolve, reject) => {
    const server = net.createServer();
    server.unref();
    server.on("error", reject);
    server.listen(0, "127.0.0.1", () => {
      const address = server.address();
      server.close(() => resolve(address.port));
    });
  });
}

function waitForServer(url) {
  const started = Date.now();
  return new Promise((resolve, reject) => {
    const tick = async () => {
      if (Date.now() - started > 30_000) {
        reject(new Error(`vite did not start:\n${serverOutput}`));
        return;
      }
      try {
        const response = await fetch(url, { redirect: "manual" });
        if (response.status < 500) { resolve(); return; }
      } catch { /* keep polling */ }
      setTimeout(tick, 100);
    };
    tick();
  });
}

const sseBody = ": connected\n\nretry: 3600000\n\n";

let removedBeta = false;
let createProjectID = null;

let browser;
let page;
const pageErrors = [];
try {
  await waitForServer(origin);
  let via;
  ({ browser, via } = await getBrowser());
  console.log(`browser: ${via}`);
  const context = await browser.newContext({ viewport: { width: 1024, height: 768 } });
  await context.addCookies([{
    name: "noisedeck_csrf",
    value: "probe-csrf",
    url: origin,
    sameSite: "Lax",
  }]);
  page = await context.newPage();
  page.on("pageerror", (error) => pageErrors.push(error.message));

  const projects = [
    { id: "proj-1", name: "alpha", canonical_path: "/workspace/alpha", git_identity_setup_revision: 1, last_used_at: "2026-08-01T00:00:00Z" },
    { id: "proj-2", name: "beta", canonical_path: "/workspace/beta", git_identity_setup_revision: 1, last_used_at: "2026-08-01T00:00:00Z" },
  ];

  const runningFacts = {
    recovery_capability: "none",
    recovery_state: "unavailable",
    presentation_state: "running",
  };

  const sessions = {
    "proj-1": [
      { id: "sess-alpha", project_id: "proj-1", provider: "claude", display_name: "", status: "running", generation: 1, created_at: "2026-08-01T00:00:00Z", updated_at: "2026-08-01T00:00:00Z", last_output_at: "2026-08-01T00:00:00Z", ...runningFacts },
    ],
    "proj-2": [
      { id: "sess-beta", project_id: "proj-2", provider: "claude", display_name: "", status: "running", generation: 1, created_at: "2026-08-01T00:00:00Z", updated_at: "2026-08-01T00:00:00Z", last_output_at: "2026-08-01T00:00:00Z", ...runningFacts },
    ],
  };

  const sessionDetails = {
    "sess-alpha": {
      id: "sess-alpha",
      project_id: "proj-1",
      provider: "claude",
      display_name: "",
      status: "running",
      generation: 1,
      pane_pid: 123,
      created_at: "2026-08-01T00:00:00Z",
      updated_at: "2026-08-01T00:00:00Z",
      last_output_at: "2026-08-01T00:00:00Z",
      ...runningFacts,
      replayed: { generation: 1, native_id: "native-alpha" },
    },
    "sess-beta": {
      id: "sess-beta",
      project_id: "proj-2",
      provider: "claude",
      display_name: "",
      status: "running",
      generation: 1,
      pane_pid: 124,
      created_at: "2026-08-01T00:00:00Z",
      updated_at: "2026-08-01T00:00:00Z",
      last_output_at: "2026-08-01T00:00:00Z",
      ...runningFacts,
      replayed: { generation: 1, native_id: "native-beta" },
    },
  };

  await page.route(`${origin}/auth/session`, async (route) => {
    await route.fulfill({ status: 204, body: "", headers: { "X-Noisedeck-Auth-Mode": "builtin" } });
  });

  await page.route(`${origin}/api/**`, async (route) => {
    const request = route.request();
    const path = new URL(request.url()).pathname;
    const json = (status, body) => route.fulfill({
      status, contentType: "application/json", body: JSON.stringify(body),
    });

    if (request.method() === "GET" && path === "/api/session-activity/events") {
      return route.fulfill({ status: 200, contentType: "text/event-stream", body: sseBody });
    }
    // The New session picker ranks by recorded launches; this probe is not
    // about ranking, so an empty history leaves the served order.
    if (request.method() === "GET" && path === "/api/harness-usage") {
      return json(200, { global: [], project: [] });
    }
    if (request.method() === "GET" && path === "/api/providers") {
      // The probe opens the New session picker and clicks the first visible
      // provider; serving only claude keeps that selection matching the mock's
      // session creation response.
      return json(200, { providers: ["claude"] });
    }
    if (request.method() === "GET" && path === "/api/settings/last-session") return json(404, { code: "not_found", message: "no last session remembered" });
    if (request.method() === "PUT" && path === "/api/settings/last-session") return json(200, request.postDataJSON());
    if (request.method() === "GET" && path === "/api/settings/layout") return json(404, { code: "not_found", message: "no layout saved" });
    if (request.method() === "PUT" && path === "/api/settings/layout") return json(200, request.postDataJSON());
    if (request.method() === "GET" && path === "/api/settings/tutorial") return json(200, { version: 1, dismissed: true, lesson: "welcome", step: 0, done: [], scope: "user" });
    if (request.method() === "GET" && path === "/api/settings") {
      return json(200, { skip_permissions: false, skip_close_confirm: true, skip_remove_project_confirm: false });
    }
    if (request.method() === "GET" && path === "/api/git-identities") {
      return json(200, {
        profiles: [{ id: "identity-1", name: "Probe identity", email: "probe@example.com", revision: 1, created_at: "2026-08-01T00:00:00Z", updated_at: "2026-08-01T00:00:00Z" }],
        default_profile_id: "identity-1",
        revision: 1,
      });
    }
    if (request.method() === "GET" && path === "/api/projects") {
      return json(200, projects);
    }
    if (request.method() === "GET" && path.match(/^\/api\/projects\/[^/]+\/sessions$/)) {
      const projectID = decodeURIComponent(path.split("/")[3]);
      const list = sessions[projectID] ?? [];
      if (projectID === "proj-2" && removedBeta) return json(200, []);
      return json(200, list);
    }
    if (/^\/api\/projects\/[^/]+\/lanes$/.test(path)) return json(200, { lanes: [] });
    if (request.method() === "GET" && path.match(/^\/api\/projects\/[^/]+\/taskmaster$/)) {
      return json(404, { code: "not_taskmaster_project", message: "not a Tasks project" });
    }
    if (request.method() === "GET" && path.match(/^\/api\/projects\/[^/]+\/taskmaster\/events$/)) {
      return route.fulfill({ status: 200, contentType: "text/event-stream", body: sseBody });
    }
    if (request.method() === "GET" && path.match(/^\/api\/sessions\/[^/]+$/)) {
      const sessionID = path.split("/")[3];
      const detail = sessionDetails[sessionID];
      if (detail !== undefined) return json(200, detail);
      return json(404, { code: "not_found", message: "session not found" });
    }
    if (request.method() === "DELETE" && path.match(/^\/api\/sessions\/[^/]+$/)) {
      const sessionID = path.split("/")[3];
      if (sessionID === "sess-beta") removedBeta = true;
      return json(204, {});
    }
    if (request.method() === "POST" && path.match(/^\/api\/projects\/[^/]+\/sessions$/)) {
      createProjectID = decodeURIComponent(path.split("/")[3]);
      return json(200, { id: "sess-new", project_id: createProjectID, provider: "claude", display_name: "", status: "running", generation: 1, created_at: "t", updated_at: "t", ...runningFacts });
    }
    // The live shell reads the chat ledger for every selected session.
    // A probe that is not about chats answers with an empty
    // ledger, an empty index and a quiet change stream.
    if (request.method() === "GET" && /^\/api\/sessions\/[^/]+\/chats\/events$/.test(path)) {
      return route.fulfill({ status: 200, contentType: "text/event-stream", body: ": connected\n\nretry: 3600000\n\n" });
    }
    if (request.method() === "GET" && /^\/api\/sessions\/[^/]+\/chats$/.test(path)) {
      return route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ agent: "claude-sess", chats: [] }) });
    }
    if (request.method() === "GET" && path === "/api/chats/status") return json(200, { available: true });
    if (request.method() === "GET" && path === "/api/chats") {
      return route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ chats: [] }) });
    }
    // The closed Assets panel reads the listing and follows its feed for its badge.
    if (/^\/api\/projects\/[^/]+\/assets$/.test(path)) return route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ locations: [], scanned: "2026-01-01T00:00:00Z", preview_max_bytes: 16777216 }) });
    if (/^\/api\/projects\/[^/]+\/assets\/events$/.test(path)) return route.fulfill({ status: 200, contentType: "text/event-stream", body: ": connected\n\nretry: 3600000\n\n" });
    throw new Error(`unhandled ${request.method()} ${path}`);
  });

  await page.goto(origin, { waitUntil: "networkidle" });

  // The rail shows both projects. Startup auto-selects the first running
  // session (sess-alpha in proj-1), so the user must explicitly select
  // the session under the non-first project to exercise the bug.
  const betaToggle = page.locator('[data-project-toggle="proj-2"]');
  await betaToggle.waitFor();
  await betaToggle.click();
  const betaRow = page.locator('[data-session="sess-beta"]');
  await betaRow.waitFor({ state: "visible" });

  await betaRow.click();
  await page.locator('[data-session="sess-beta"][aria-selected="true"]').waitFor();

  // Open the row menu and close the session. skip_close_confirm is true,
  // so the menu action removes the session immediately.
  await page.locator('[data-session-menu="sess-beta"]').click();
  await page.locator('[data-session-menu-close]').waitFor();
  await page.locator('[data-session-menu-close]').click();

  // After close the pane must be empty and name beta, not alpha.
  await page.locator('[data-empty-pane="no-session"]').waitFor();
  const body = await page.locator("[data-empty-body]").textContent();
  assert.ok(body.includes("beta"), `empty pane body names the closed session's project (got: ${body})`);
  assert.ok(!body.includes("alpha"), `empty pane body must not name the first project (got: ${body})`);
  const titlebarTitle = await page.locator('[data-region="terminal-titlebar"] [data-titlebar-title]').textContent();
  assert.equal(titlebarTitle, "beta", "titlebar names the closed session's project");

  // Click the pane's New session action and verify it targets beta.
  await page.locator("[data-empty-action]").click();
  await page.locator("[data-new-session-modal]").waitFor();
  const provider = page.locator('[data-new-session-provider]:visible').first();
  await provider.waitFor();
  await provider.click();
  await page.waitForTimeout(100);
  assert.equal(createProjectID, "proj-2", "the pane's New session action creates in the closed session's project");

  assert.deepEqual(pageErrors, [], "no uncaught page errors");
  console.log("close-keeps-project: closing a session leaves its project in context: PASS");
} finally {
  await page?.close();
  await browser?.close();
  vite.kill("SIGTERM");
}
