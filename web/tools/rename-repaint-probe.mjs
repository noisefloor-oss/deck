// A background poll that repaints the rail mid-rename must not read as an
// acceptance. The poll's render replaces the container's markup, which
// destroys the focused inline rename input; Chrome fires that input's blur
// synchronously inside the replacement, and an unconditional blur handler
// commits the half-typed draft — a silent write of a name the user never
// confirmed — and closes the edit. The user's own blur (clicking away,
// tabbing out) must keep committing.
//
// The poll tick this probe drives has to genuinely change the model: the
// unchanged-markup guard skips ticks whose rebuilt bytes match the cached
// ones, so a still rail proves nothing about the painting path. Each leg
// renames a DIFFERENT row in the same rail from the mock side — a change
// only the poll discovers — and holds node references across the tick, so
// a rebuilt rail is proven by node identity, not by identical-looking
// markup. Requests are counted at the route layer, where a committed draft
// would surface as a POST nobody asked for.
//
// Live-mock harness (shortcuts-probe's): vite dev server plus deterministic
// HTTP/WS mocks, real chords, real poll interval.
import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import { existsSync } from "node:fs";
import net from "node:net";

if (process.env.PLAYWRIGHT_BROWSERS_PATH === undefined &&
    existsSync("/workspace/tools/ms-playwright")) {
  process.env.PLAYWRIGHT_BROWSERS_PATH = "/workspace/tools/ms-playwright";
}
const { browser: getBrowser } = await import("./browser.mjs");
const { eventually, exactlyOnce } = await import("./eventually.mjs");

// The live workspace's sidebar poll interval; the probe waits for the real
// timer, so a leg's wait is bounded by one interval plus slack.
const POLL_MS = 10_000;
const WAIT_MS = POLL_MS + 5_000;

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

let browser;
let page;
const pageErrors = [];
try {
  await waitForServer(origin);
  let via;
  ({ browser, via } = await getBrowser());
  console.log(`browser: ${via}`);
  const context = await browser.newContext();
  await context.addCookies([{
    name: "noisedeck_csrf",
    value: "probe-csrf",
    url: origin,
    sameSite: "Lax",
  }]);
  page = await context.newPage();
  page.on("pageerror", (error) => pageErrors.push(error.message));
  page.on("console", (message) => {
    // Mocked 404 answers surface as resource-load noise; script errors are
    // what must stay at zero.
    if (message.type() === "error" && !/^Failed to load resource/.test(message.text())) {
      pageErrors.push(message.text());
    }
  });

  const project = {
    id: "project-1",
    name: "tester-project",
    canonical_path: "/workspace/project",
    git_identity_profile_id: "identity-1",
    git_identity_profile_revision: 1,
    git_identity_setup_revision: 1,
    last_used_at: "2026-08-01T00:00:00Z",
  };
  const gitIdentity = {
    id: "identity-1", name: "Probe identity", email: "probe@example.com", revision: 1,
    created_at: "2026-08-01T00:00:00Z", updated_at: "2026-08-01T00:00:00Z",
  };
  let consoleSettings = {
    skip_permissions: false,
    skip_close_confirm: false,
    skip_remove_project_confirm: false,
  };
  const sessionRenames = [];
  const projectRenames = [];

  const summaries = [
    {
      id: "session-1",
      project_id: project.id,
      provider: "claude",
      display_name: "alpha session",
      status: "running",
      presentation_state: "running",
      generation: 1,
      created_at: "2026-08-01T00:00:00Z",
    },
    {
      id: "session-2",
      project_id: project.id,
      provider: "codex",
      display_name: "beta session",
      status: "running",
      presentation_state: "running",
      generation: 1,
      created_at: "2026-08-01T00:00:00Z",
    },
  ];
  const detail = (summary) => ({
    ...summary,
    pane_pid: 4242,
    tmux_pane_id: "%1",
    native_session_id: `native-${summary.id}`,
    updated_at: "2026-08-01T00:00:00Z",
    recovery_capability: "none",
    recovery_state: "unavailable",
  });

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
      return route.fulfill({ status: 200, contentType: "text/event-stream", body: ": connected\n\nretry: 3600000\n\n" });
    }
    if (request.method() === "GET" && path === "/api/harness-login") {
      // The login coordinator polls the listing while a controller is
      // attached; no login is waiting in this probe.
      return json(200, { pending: [], csrf: "login-csrf" });
    }
    if (request.method() === "GET" && path === "/api/providers") {
      // The probe's rail only drives claude and codex sessions; the picker is
      // never opened, so the served inventory need only match what is present.
      return json(200, { providers: ["claude", "codex"] });
    }
    if (request.method() === "GET" && path === "/api/settings/last-session") return json(404, { code: "not_found", message: "no last session remembered" });
    if (request.method() === "PUT" && path === "/api/settings/last-session") return json(200, request.postDataJSON());
    if (request.method() === "GET" && path === "/api/settings/layout") return json(404, { code: "not_found", message: "no layout saved" });
    if (request.method() === "PUT" && path === "/api/settings/layout") return json(200, request.postDataJSON());
    if (request.method() === "GET" && path === "/api/settings/tutorial") return json(200, { version: 1, dismissed: true, lesson: "welcome", step: 0, done: [], scope: "user" });
    if (request.method() === "GET" && path === "/api/settings") return json(200, consoleSettings);
    if (request.method() === "PUT" && path === "/api/settings") {
      consoleSettings = request.postDataJSON();
      return json(200, consoleSettings);
    }
    if (request.method() === "GET" && path === "/api/git-identities") {
      return json(200, { profiles: [gitIdentity], default_profile_id: gitIdentity.id, revision: 1 });
    }
    if (request.method() === "GET" && path === "/api/projects") return json(200, [project]);
    if (request.method() === "GET" && path === `/api/projects/${project.id}/sessions`) {
      return json(200, summaries);
    }
    if (/^\/api\/projects\/[^/]+\/lanes$/.test(path)) return json(200, { lanes: [] });
    if (request.method() === "GET" && path === `/api/projects/${project.id}/taskmaster`) {
      return json(404, { error: "not_taskmaster_project", message: "not a Tasks project" });
    }
    if (request.method() === "GET" && path === `/api/projects/${project.id}/taskmaster/events`) {
      return route.fulfill({ status: 200, contentType: "text/event-stream", body: ": connected\n\nretry: 3600000\n\n" });
    }
    if (request.method() === "GET" && path.startsWith("/api/sessions/")) {
      const id = path.split("/").at(-1);
      const summary = summaries.find((s) => s.id === id);
      return summary === undefined ? json(404, { error: "not_found", message: "no such session" }) : json(200, detail(summary));
    }
    if (request.method() === "POST" && path === `/api/sessions/${summaries[0].id}/rename`) {
      sessionRenames.push(request.postDataJSON());
      summaries[0].display_name = request.postDataJSON().name;
      return route.fulfill({ status: 204, body: "" });
    }
    if (request.method() === "POST" && path === `/api/projects/${project.id}/rename`) {
      projectRenames.push(request.postDataJSON());
      project.name = request.postDataJSON().name;
      return route.fulfill({ status: 204, body: "" });
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
  await page.routeWebSocket(`${origin.replace("http", "ws")}/api/sessions/*/terminal?*`, (socket) => {
    const url = new URL(socket.url());
    const session = url.pathname.split("/").at(-2);
    socket.send(JSON.stringify({ type: "attached", token: 7, generation: 3 }));
    socket.send(JSON.stringify({
      type: "output",
      data: Buffer.from(`OUTPUT ${session}\r\n`).toString("base64"),
    }));
  });

  await page.goto(origin, { waitUntil: "networkidle" });
  await page.locator('[data-session="session-1"]').waitFor();
  await page.locator('[data-session="session-1"]').click();
  await page.waitForFunction(
    () => document.querySelector("[data-live-terminal] .xterm-rows")?.textContent?.includes("OUTPUT session-1") === true,
  );
  // A reload would restart every timer and lose the edit; the marker proves
  // the page the probe drove is the page it asserts on.
  await page.evaluate(() => { window.__probeAlive = true; });

  // ---- session rename: a poll tick with a changed model, mid-edit ----
  await page.keyboard.press("Meta+r");
  const sessionInput = page.locator('[data-session="session-1"] [data-rename-input]');
  await sessionInput.waitFor();
  assert.equal(await sessionInput.inputValue(), "alpha session");
  assert.equal(
    await sessionInput.evaluate((el) => document.activeElement === el),
    true, "the session rename did not open focused — the later legs would not be mid-edit",
  );
  const SESSION_TYPED = "drafted na";
  await sessionInput.pressSequentially(SESSION_TYPED);
  for (let i = 0; i < 3; i += 1) await sessionInput.press("ArrowLeft");
  assert.equal(await sessionInput.inputValue(), SESSION_TYPED, "the typed draft is what the field holds");
  // Hold the nodes the tick must replace: session-2's row proves the whole
  // rail repainted, the input proves the edit's field itself was torn down
  // and rebuilt rather than left alone.
  await page.evaluate(() => {
    window.__probeRailRow = document.querySelector('[data-session="session-2"]');
    window.__probeEditInput = document.querySelector("[data-rename-input]");
  });
  assert.notEqual(
    await page.evaluate(() => window.__probeRailRow), null,
    "session-2's row is missing — the leg has no bystander to watch",
  );
  // The model change only the poll discovers: another session's row is
  // renamed from the server side while the user types.
  summaries[1].display_name = "beta session retitled";
  await eventually(
    () => page.evaluate(() => ({
      renamed: document.querySelector('[data-session="session-2"]')?.textContent ?? "",
      rowReplaced: document.querySelector('[data-session="session-2"]') !== window.__probeRailRow,
      inputReplaced: document.querySelector("[data-rename-input]") !== window.__probeEditInput,
    })),
    (v) => v.renamed.includes("beta session retitled") && v.rowReplaced && v.inputReplaced,
    "the poll never delivered the concurrent rename — the later assertions would not be about a repaint",
    WAIT_MS,
  );
  console.log("ok: the poll repainted the whole rail, edit included, mid-edit");

  assert.equal(await sessionInput.count(), 1,
    "the poll's repaint closed the session rename that was mid-edit");
  assert.equal(
    await sessionInput.evaluate((el) => document.activeElement === el),
    true, "the rebuilt session rename field did not take focus back");
  assert.equal(await sessionInput.inputValue(), SESSION_TYPED,
    "the repaint discarded the uncommitted session draft");
  const sessionCaret = await sessionInput.evaluate((el) => ({ start: el.selectionStart, end: el.selectionEnd }));
  assert.deepEqual(
    sessionCaret,
    { start: SESSION_TYPED.length - 3, end: SESSION_TYPED.length - 3 },
    `the session caret did not survive the repaint (got ${JSON.stringify(sessionCaret)})`,
  );
  await exactlyOnce([
    { read: () => sessionRenames.length, expected: 0, message: "no session rename request was sent for the unconfirmed draft" },
    { read: () => projectRenames.length, expected: 0, message: "no project rename request was sent while a session edit was open" },
  ]);
  console.log("ok: the session draft survived the repaint open, focused, intact, and uncommitted");
  await sessionInput.press("Escape");
  await sessionInput.waitFor({ state: "detached" });
  console.log("ok: Escape still abandons the edit the repaint spared");

  // ---- project rename: the same sequence against the project header ----
  await page.locator('[data-project-toggle="project-1"]').click();
  await page.locator('[data-project="project-1"] [data-selected="true"]').waitFor();
  await page.keyboard.press("Meta+r");
  const projectInput = page.locator("[data-rename-project-input]");
  await projectInput.waitFor();
  assert.equal(await projectInput.inputValue(), "tester-project");
  assert.equal(
    await projectInput.evaluate((el) => document.activeElement === el),
    true, "the project rename did not open focused — the later legs would not be mid-edit",
  );
  const PROJECT_TYPED = "draft proj";
  await projectInput.pressSequentially(PROJECT_TYPED);
  for (let i = 0; i < 2; i += 1) await projectInput.press("ArrowLeft");
  assert.equal(await projectInput.inputValue(), PROJECT_TYPED, "the typed draft is what the field holds");
  await page.evaluate(() => {
    window.__probeRailRow = document.querySelector('[data-session="session-1"]');
    window.__probeEditInput = document.querySelector("[data-rename-project-input]");
  });
  assert.notEqual(
    await page.evaluate(() => window.__probeRailRow), null,
    "session-1's row is missing — the leg has no bystander to watch",
  );
  summaries[0].display_name = "alpha session retitled";
  await eventually(
    () => page.evaluate(() => ({
      renamed: document.querySelector('[data-session="session-1"]')?.textContent ?? "",
      rowReplaced: document.querySelector('[data-session="session-1"]') !== window.__probeRailRow,
      inputReplaced: document.querySelector("[data-rename-project-input]") !== window.__probeEditInput,
    })),
    (v) => v.renamed.includes("alpha session retitled") && v.rowReplaced && v.inputReplaced,
    "the poll never delivered the concurrent rename — the later assertions would not be about a repaint",
    WAIT_MS,
  );
  console.log("ok: the poll repainted the whole rail, project edit included, mid-edit");

  assert.equal(await projectInput.count(), 1,
    "the poll's repaint closed the project rename that was mid-edit");
  assert.equal(
    await projectInput.evaluate((el) => document.activeElement === el),
    true, "the rebuilt project rename field did not take focus back");
  assert.equal(await projectInput.inputValue(), PROJECT_TYPED,
    "the repaint discarded the uncommitted project draft");
  const projectCaret = await projectInput.evaluate((el) => ({ start: el.selectionStart, end: el.selectionEnd }));
  assert.deepEqual(
    projectCaret,
    { start: PROJECT_TYPED.length - 2, end: PROJECT_TYPED.length - 2 },
    `the project caret did not survive the repaint (got ${JSON.stringify(projectCaret)})`,
  );
  await exactlyOnce([
    { read: () => projectRenames.length, expected: 0, message: "no project rename request was sent for the unconfirmed draft" },
    { read: () => sessionRenames.length, expected: 0, message: "no session rename request was sent while a project edit was open" },
  ]);
  console.log("ok: the project draft survived the repaint open, focused, intact, and uncommitted");
  await projectInput.press("Escape");
  await projectInput.waitFor({ state: "detached" });
  console.log("ok: Escape still abandons the spared project edit");

  assert.equal(await page.evaluate(() => window.__probeAlive === true), true, "the probe's page reloaded mid-run");
  assert.deepEqual(pageErrors, [], `no page errors (got ${JSON.stringify(pageErrors)})`);
  console.log("a poll repaint mid-rename neither commits, closes, nor discards the edit: PASS");
  await context.close();
} catch (error) {
  console.error(`rename-repaint probe: FAIL\n${error?.stack ?? error}`);
  if (serverOutput !== "") console.error(`vite output:\n${serverOutput}`);
  process.exitCode = 1;
} finally {
  await browser?.close().catch(() => {});
  vite.kill("SIGTERM");
}

function freePort() {
  return new Promise((resolve, reject) => {
    const server = net.createServer();
    server.unref();
    server.on("error", reject);
    server.listen(0, "127.0.0.1", () => {
      const { port: bound } = server.address();
      server.close(() => resolve(bound));
    });
  });
}

function waitForServer(url) {
  const started = Date.now();
  const attempt = (resolve, reject) => {
    net.connect({ host: "127.0.0.1", port: new URL(url).port }, () => {
      // The listener answers before vite finishes wiring; one beat of
      // slack avoids a connection racing the dev server's accept loop.
      setTimeout(resolve, 300);
    }).on("error", () => {
      if (Date.now() - started > 20_000) reject(new Error("vite dev server never came up"));
      else setTimeout(() => attempt(resolve, reject), 200);
    });
  };

  return new Promise(attempt);
}
