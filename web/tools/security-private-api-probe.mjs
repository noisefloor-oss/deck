// Production workspace composition against synthetic HTTP responses. Native
// harness login and deployment authorization are outside this browser probe.
import assert from "node:assert/strict";
import { createServer } from "node:http";
import { readFile, stat } from "node:fs/promises";
import { extname, resolve, sep } from "node:path";
import { browser as connectBrowser } from "./browser.mjs";

const dist = resolve(import.meta.dirname, "../dist");
const mime = { ".html": "text/html", ".js": "text/javascript", ".css": "text/css", ".svg": "image/svg+xml", ".woff2": "font/woff2", ".png": "image/png" };
const profile = { id: "fixture-identity", name: "Fixture", email: "fixture@example.invalid", revision: 1 };
const project = { id: "private-project", name: "Private project canary", canonical_path: "/synthetic/private", last_used_at: "2026-01-01T00:00:00Z",
  git_identity_profile_id: profile.id, git_identity_profile_revision: 1, git_identity_setup_revision: 1, git_author_name: profile.name, git_author_email: profile.email };
const session = { id: "private-session", project_id: project.id, provider: "codex", display_name: "Private session canary", status: "running", generation: 1,
  created_at: "2026-01-01T00:00:00Z", updated_at: "2026-01-01T00:00:00Z", recovery_capability: "none", recovery_state: "unavailable", presentation_state: "running" };
let authenticated = true, grant = true, denial;
const received = [], errors = [], serverErrors = [];
const onboarding = { policy: "required", protection: "encrypted", started: true, setupComplete: true };
const json = (response, status, body) => {
  response.writeHead(status, { "Content-Type": "application/json", "Cache-Control": "no-store" });
  response.end(body === undefined ? "" : JSON.stringify(body));
};
const server = createServer(async (request, response) => {
  try {
    const path = new URL(request.url, "http://fixture").pathname;
    response.setHeader("X-Deck-Workspace-Security", "v1");
    if (path === "/auth/session") {
      response.setHeader("X-Noisedeck-Auth-Mode", "oidc");
      json(response, authenticated ? 204 : 401); return;
    }
    if (path === "/api/security/v1/onboarding") { json(response, 200, onboarding); return; }
    if (path.startsWith("/api/security/v1/")) {
      if (!authenticated) { json(response, 401, { code: "authorization-required" }); return; }
      if (path.endsWith("/state")) {
        json(response, 200, { instanceId: "private-api-fixture", phase: "ready", storage: "open", runtime: "running", locked: false, browserGranted: grant,
          onboarding, ownerBound: true, workspaceAuthorized: grant }); return;
      }
      json(response, 200, []); return;
    }
    if (path.startsWith("/api/")) {
      let body;
      if (request.method === "POST") {
        let data = ""; for await (const chunk of request) data += chunk;
        body = data ? JSON.parse(data) : undefined;
      }
      // Record only routing facts, never the callback URL.
      received.push({ path, method: request.method, action: body?.action });
      if (denial && path === denial.path && (denial.action === undefined || body?.action === denial.action)) {
        const status = denial.status; denial = undefined;
        grant = false; if (status === 401) authenticated = false;
        json(response, status, { code: status === 401 ? "authorization-required" : "unlock-required" }); return;
      }
      if (!grant) { json(response, 403, { code: "unlock-required" }); return; }
      if (path === "/api/harness-login") {
        if (request.method === "GET") json(response, 200, { pending: [{ id: "registration", flow: "codex-browser", expires: "2030-01-01T00:00:00Z" }], csrf: "harness-csrf" });
        else {
          assert.equal(request.headers["x-csrf-token"], "private-api-csrf");
          assert.equal(request.headers["x-harness-login-csrf"], "harness-csrf");
          json(response, 200, { id: "handle", status: "completed" });
        }
        return;
      }
      if (path === "/api/build") { json(response, 200, { build_id: "private-api-build" }); return; }
      if (path === "/api/settings/tutorial") { json(response, 200, { version: 1, dismissed: true, lesson: "welcome", step: 0, done: [], scope: "user" }); return; }
      if (path === "/api/settings") { json(response, 200, { skip_permissions: false, skip_close_confirm: false, skip_remove_project_confirm: false }); return; }
      if (path === "/api/settings/layout") { json(response, 404, { error: "not_found" }); return; }
      if (path === "/api/projects") { json(response, 200, [project]); return; }
      if (path === `/api/projects/${project.id}/sessions`) { json(response, 200, [session]); return; }
      if (path === `/api/sessions/${session.id}`) { json(response, 200, session); return; }
      if (path.endsWith("/screen")) { json(response, 404, { error: "not_found" }); return; }
      if (path === "/api/providers") { json(response, 200, { providers: ["codex"] }); return; }
      if (path === "/api/git-identities") { json(response, 200, { profiles: [profile], default_profile_id: profile.id, revision: 1 }); return; }
      if (path.endsWith("/taskmaster")) { json(response, 404, { error: "not_taskmaster_project" }); return; }
      if (path.endsWith("/files")) { json(response, 200, { path: "", at_root: true, entries: [] }); return; }
      if (path.endsWith("/chats")) { json(response, 200, { agent: "synthetic-agent", chats: [] }); return; }
      if (path.endsWith("/events")) { response.writeHead(200, { "Content-Type": "text/event-stream" }); response.end(": fixture\n\n"); return; }
      json(response, 404, { error: "not_found" }); return;
    }
    const file = resolve(dist, `.${path === "/" ? "/index.html" : path}`);
    if (!file.startsWith(`${dist}${sep}`) || !(await stat(file)).isFile()) { response.writeHead(404); response.end(); return; }
    let body = await readFile(file);
    if (path === "/") body = Buffer.from(body.toString().replaceAll("__NOISEDECK_BUILD_ID__", "private-api-build"));
    response.writeHead(200, { "Content-Type": mime[extname(file)] ?? "application/octet-stream" }); response.end(body);
  } catch (error) {
    serverErrors.push(error.message);
    if (!response.headersSent) json(response, 500, { error: "fixture_error" }); else response.end();
  }
});
await new Promise(resolveListen => server.listen(0, "127.0.0.1", resolveListen));
const origin = `http://127.0.0.1:${server.address().port}`;
let browser;
try {
  ({ browser } = await connectBrowser());
  for (const status of [401, 403]) {
    for (const action of ["list", "begin", "submit", "build"]) {
      authenticated = true; grant = true; denial = undefined; received.length = 0;
      const context = await browser.newContext({ reducedMotion: "reduce", viewport: { width: 1120, height: 720 } });
      try {
        await context.addCookies([{ name: "noisedeck_csrf", value: "private-api-csrf", url: origin }]);
        await context.route("**/*", route => new URL(route.request().url()).origin === origin ? route.continue() : route.abort());
        // The login row exists only for an attached session with a waiting login.
        await context.routeWebSocket(/\/api\/sessions\/[^/]+\/terminal/, peer => peer.send(JSON.stringify({ type: "attached", token: 1, generation: 1 })));
        const page = await context.newPage();
        page.setDefaultTimeout(10000);
        page.on("pageerror", error => errors.push(error.message));
        await page.clock.install();
        await page.clock.pauseAt(new Date());
        const refusal = { status, path: action === "build" ? "/api/build" : "/api/harness-login", ...(action === "begin" || action === "submit" ? { action } : {}) };
        if (action === "build") denial = refusal;
        await page.goto(origin, { waitUntil: "domcontentloaded" });
        if (action !== "build") {
          // The only session opens on load and raises its waiting login. Dismiss
          // it so the menu row opens a dialog that makes its own listing request.
          await page.locator("[data-harness-login-dialog] [data-login-close]").click();
          await page.locator("[data-harness-login-dialog]").waitFor({ state: "detached" });
          await page.locator("[data-live-more]").click();
          if (action === "list") denial = refusal;
          await page.locator("[data-more-popover] [data-harness-login]").click();
          if (action === "begin" || action === "submit") {
            await page.locator("[data-login-callback]").waitFor();
            await page.locator("[data-login-callback]").fill("http://localhost:1455/auth/callback?code=synthetic-callback-canary&state=synthetic-state");
            denial = refusal;
            await page.getByRole("button", { name: "Finish sign-in", exact: true }).click();
          }
        }
        // Timers are paused: the three-second security poll cannot rescue a
        // request that forgets to detach the already-rendered private UI.
        await page.locator(status === 401 ? '[data-onboarding="signin"]' : '[data-security-runtime="authorize-browser"]').waitFor();
        assert.equal(denial, undefined, `${action} must actually receive the refusal`);
        assert.equal(await page.locator("[data-workspace], [data-harness-login-dialog], [data-login-callback]").count(), 0);
        assert.doesNotMatch(await page.locator("#app").innerText(), /Private project canary|Private session canary|synthetic-callback-canary/);
        if (action === "submit") assert.equal(received.filter(call => call.action === "submit").length, 1, "a refused callback is never replayed");
      } catch (error) {
        console.error({ action, status, receivedActions: received.map(call => call.action), pageErrors: errors, serverErrors });
        throw error;
      } finally { await context.close(); }
    }
  }
  assert.deepEqual(errors, []);
  assert.deepEqual(serverErrors, []);
  console.log("private API authorization: harness list/begin/submit and build 401/403 teardown PASS with polling paused");
} finally {
  await browser?.close();
  server.closeAllConnections();
  await new Promise(resolveClose => server.close(resolveClose));
}
