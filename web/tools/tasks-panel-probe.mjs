// Deterministic browser probe for the Project tasks panel.
//
// This is NOT the live smoke. live-smoke.mjs drives the whole production
// composition end to end, is known to be flaky on constrained hosts, and
// serves no board — its tasks read answers 404. This harness
// answers the narrow set of questions only a real engine can answer —
// measured geometry, computed type, and the requests the panel issues —
// against the BUILT page with a mocked authenticated API. Every assertion
// is deterministic: no wall-clock content, no network, and no reliance on
// the terminal's own output.
//
// Usage: pnpm build && node tools/tasks-panel-probe.mjs
// Requires: dist/ built, and a browser — the agent-browser sidecar via
// NOISE_BROWSER_WS, or a local one (see tools/browser.mjs).
import assert from "node:assert/strict";
import { assertFontVoice } from "./font-assertions.mjs";
import { createServer } from "node:http";
import { readFile } from "node:fs/promises";
import { existsSync } from "node:fs";
import { extname, join, resolve } from "node:path";
import { exactlyOnce, settled } from "./eventually.mjs";

if (process.env.PLAYWRIGHT_BROWSERS_PATH === undefined &&
    existsSync("/workspace/tools/ms-playwright")) {
  process.env.PLAYWRIGHT_BROWSERS_PATH = "/workspace/tools/ms-playwright";
}
const { browser: getBrowser } = await import("./browser.mjs");
const { eventually } = await import("./eventually.mjs");
const activityOnly = process.env.NOISEDECK_ACTIVITY_PROBE === "1";

const WEB = resolve(import.meta.dirname, "..");
const DIST = join(WEB, "dist");
const MIME = {
  ".html": "text/html",
  ".js": "text/javascript",
  ".css": "text/css",
  ".map": "application/json",
  ".svg": "image/svg+xml",
  ".png": "image/png",
  ".ico": "image/x-icon",
  ".webmanifest": "application/manifest+json",
  ".woff2": "font/woff2",
};

if (!existsSync(join(DIST, "index.html"))) {
  throw new Error("dist/index.html is missing — run `pnpm build` first");
}

const SIDEBAR_W = 272;
const TITLEBAR_H = 48;
const MOBILE_TITLEBAR_H = 56;
const TERMINAL_MIN_W = 360;
const PROJECTS_TYPOGRAPHY = {
  size: "11px",
  weight: "500",
  caps: "normal",
  transform: "uppercase",
  tracking: "0.77px",
  // The comp's header label keeps the inherited body line box (11px * 1.55);
  // the old 1.3 ratio was ours, retired by the conformance pass.
  lineHeight: "17.05px",
};
const LONG_PROJECT_NAME = `${"long-project-folder-name-".repeat(8)}deck`;

const project = {
  id: "project-1",
  name: LONG_PROJECT_NAME,
  canonical_path: "/workspace/noisedeck",
  git_identity_profile_id: "identity-1",
  git_identity_profile_revision: 1,
  git_identity_setup_revision: 1,
  last_used_at: "2026-08-01T00:00:00Z",
};
const createdProject = {
  id: "project-2",
  name: "fresh",
  canonical_path: "/workspace/fresh",
  git_identity_profile_id: "identity-1",
  git_identity_profile_revision: 1,
  git_identity_setup_revision: 1,
  last_used_at: "2026-08-01T00:00:00Z",
};
const gitIdentity = {
  id: "identity-1", name: "Deck probe", email: "deck-probe@example.com", revision: 1,
  created_at: "2026-08-01T00:00:00Z", updated_at: "2026-08-01T00:00:00Z",
};
const session = {
  id: "session-1",
  project_id: project.id,
  provider: "claude",
  display_name: "",
  status: "running",
  generation: 1,
  recovery_capability: "none",
  recovery_state: "unavailable",
  presentation_state: "running",
  created_at: "2026-08-01T00:00:00Z",
  updated_at: "2026-08-01T00:00:00Z",
};
const replacementSession = {
  ...session,
  id: "session-2",
  display_name: "Replacement terminal",
  recovery_state: activityOnly ? "recovering" : "unavailable",
  presentation_state: activityOnly ? "recovering" : "running",
};

const attentionOnly = process.env.NOISEDECK_ATTENTION_PROBE === "1";
const attentionReceipts = [];
if (attentionOnly) {
  session.provider = "pi";
  replacementSession.provider = "pi";
  session.conversation_attention = { session_id: session.id, generation: 1, context_key: "native-context-1", state: "needs_input", sequence: 2, ended_sequence: 0, seen_sequence: 0 };
  replacementSession.conversation_attention = { session_id: replacementSession.id, generation: 1, context_key: "native-context-2", state: "ended", sequence: 3, ended_sequence: 3, seen_sequence: 0 };
}

const LONG_TASK_TITLE = "Unprioritized backlog item with a deliberately long title that must stay inside two board lines";
const LONG_PROGRESS_TITLE = "In-progress work with a long title that verifies the middle column keeps the same card anatomy";
const UNBROKEN_DONE_TITLE = "CompletedCardTitleWithOneDeliberatelyUnbrokenTokenThatMustWrapWithoutOverflowingItsCard";

// The board fixture: one card per state the card anatomy has a rule for
// (each priority, dependencies, an in-progress parent with subtasks), and
// a second tag so the columns prove they flatten across tags.
const baseTasks = {
  master: {
    tasks: [
      {
        id: 1,
        title: "Initialize the workspace frame",
        status: "done",
        priority: "high",
        dependencies: [2],
      },
      {
        id: 2,
        title: "Wire the provider descriptor",
        status: "in-progress",
        priority: "high",
        dependencies: [1, 999],
        description: "See Task 1 and Task 999. [Focus documentation](https://example.invalid/focus).",
        subtasks: [
          { id: 1, title: "Read the descriptor", status: "done" },
          { id: 2, title: "Wire the matrix", status: "pending" },
          { id: 3, title: "Re-run the suite", status: "pending" },
          { id: 4, title: "Record the results", status: "pending" },
        ],
      },
      { id: 7, title: LONG_PROGRESS_TITLE, status: "in-progress", priority: "medium" },
      {
        id: 3,
        title: "Serialize generation per project",
        status: "pending",
        priority: "med",
        dependencies: [1, 2],
      },
      { id: 4, title: "Polish the sidebar", status: "pending", priority: "low" },
      { id: 5, title: LONG_TASK_TITLE, status: "blocked" },
      { id: 8, title: UNBROKEN_DONE_TITLE, status: "done", priority: "low" },
      // Enough shipped work to overflow the Done column, which is what
      // makes independent scrolling measurable.
      ...Array.from({ length: 12 }, (_, i) => ({
        id: 20 + i,
        title: `Shipped item ${20 + i}`,
        status: "done",
        priority: "low",
      })),
      // The worst-case board card: a two-line title carrying both bottom
      // rows (deps + progress) at once. The reference never shows this
      // combination, so the card grows past the 132px floor rather than
      // slicing a row (a probe-local mirror of the shared fixture's card 42, so it
      // cannot disturb the search fixtures).
      { id: 42, title: "Reconcile identity capture fidelity across many restarts and validate the full dense resume matrix end to end", status: "in-progress", priority: "med", dependencies: [7], subtasks_done: 1, subtasks_total: 3 },
    ],
  },
  release: {
    tasks: [{ id: 9, title: "Cut the first release", status: "pending", priority: "high" }],
  },
};
// master: 20 rows + 4 subtasks; release: 1 row. Subtasks are cards too.
const BASE_COUNT = 25;
// To do holds pending and blocked from every tag, subtasks included:
// 2.2, 2.3, 2.4, 3, 4, 5, 9.
const TODO_IDS = ["2.2", "2.3", "2.4", "3", "4", "5", "9"];

// What the mocked task generation produces: one extra task, so the board
// has to move after generation without the terminal being rebuilt.
const generatedTasks = {
  master: {
    tasks: [...baseTasks.master.tasks, { id: 6, title: "Parsed from the PRD", status: "pending" }],
  },
  release: baseTasks.release,
};
const GENERATED_COUNT = BASE_COUNT + 1;

let tasks = baseTasks;
let prds = [];
let projectHasSessions = true;
let createdProjectRegistered = false;
const uploads = [];
const generations = [];
const addedTasks = [];
let activityRequests = 0;

const server = createServer(async (req, res) => {
  const url = new URL(req.url, "http://x");
  const path = join(DIST, decodeURIComponent(url.pathname));
  const file = url.pathname === "/" ? join(DIST, "index.html") : path;
  if (!file.startsWith(DIST) || !existsSync(file)) {
    res.writeHead(404);
    res.end("not found");
    return;
  }
  res.writeHead(200, { "Content-Type": MIME[extname(file)] ?? "application/octet-stream" });
  res.end(await readFile(file));
});
await new Promise((r) => server.listen(0, "127.0.0.1", r));
const origin = `http://127.0.0.1:${server.address().port}`;

let browser;
let page;
const pageErrors = [];
try {
  const { browser: got, via } = await getBrowser();
  browser = got;
  console.log(`browser: ${via}`);
  const context = await browser.newContext({ viewport: { width: 1400, height: 900 } });
  await context.addCookies([
    { name: "noisedeck_csrf", value: "probe-csrf", url: origin, sameSite: "Lax" },
  ]);
  page = await context.newPage();
  page.on("pageerror", (error) => pageErrors.push(error.message));

  await page.route(`${origin}/auth/session`, (route) =>
    route.fulfill({ status: 204, body: "", headers: { "X-Noisedeck-Auth-Mode": "builtin" } }));
  await page.route(`${origin}/api/**`, async (route) => {
    const request = route.request();
    const url = new URL(request.url());
    const path = url.pathname;
    if (request.method() === "GET" && path === "/api/session-activity/events") {
      activityRequests++;
      return route.fulfill({
        status: 200,
        contentType: "text/event-stream",
        body: activityOnly
          ? ': connected\n\nevent: output\ndata: {"session_id":"session-2","generation":1}\n\nretry: 3600000\n\n'
          : ": connected\n\nretry: 3600000\n\n",
      });
    }
    const json = (status, body) =>
      route.fulfill({ status, contentType: "application/json", body: JSON.stringify(body) });
    const base = `/api/projects/${project.id}`;
    if (attentionOnly && request.method() === "POST" && /^\/api\/sessions\/[^/]+\/attention\/seen$/.test(path)) {
      assert.equal(request.headers()["x-csrf-token"], "probe-csrf");
      const id = path.split("/")[3];
      const row = [session, replacementSession].find(row => row.id === id);
      const body = request.postDataJSON();
      assert.equal(body.generation, row.generation);
      assert.equal(body.context_key, row.conversation_attention.context_key);
      assert.ok(body.ended_sequence <= row.conversation_attention.ended_sequence);
      row.conversation_attention.seen_sequence = Math.max(row.conversation_attention.seen_sequence, body.ended_sequence);
      attentionReceipts.push(id);
      return route.fulfill({ status: 204 });
    }


    if (request.method() === "GET" && [project.id, createdProject.id].some((id) => path === `/api/projects/${id}/preview/settings`)) {
      return json(200, { configured: true, self_service: true, enabled: false });
    }
    if (request.method() === "GET" && path === "/api/projects") {
      return json(200, [project, ...(createdProjectRegistered ? [createdProject] : [])]);
    }
    if (request.method() === "POST" && path === "/api/projects") {
      assert.equal(request.headers()["x-csrf-token"], "probe-csrf");
      assert.deepEqual(request.postDataJSON(), {
        name: "fresh",
        path: "/workspace/fresh",
        plain: false,
        git_identity_profile_id: gitIdentity.id,
        expected_git_identity_revision: 1,
      });
      createdProjectRegistered = true;
      return json(201, createdProject);
    }
    if (request.method() === "GET" && path === "/api/projects/inspect") {
      // The chosen directory exists and is not a repository: a plain
      // directory the field registers as it is.
      const input = url.searchParams.get("input");
      return json(200, { kind: "absent", path: input, exists: false, entries: [], store_path: `${input}/.git/noise-tasks/tasks.sqlite` });
    }
    if (request.method() === "GET" && path === "/api/project-clones") {
      return json(200, []);
    }
    if (request.method() === "GET" && path === "/api/project-directories") {
      const selected = url.searchParams.get("path");
      if (selected === null) {
        return json(200, {
          current_path: "",
          parent_path: null,
          directories: [{ name: "workspace", path: "/workspace" }],
        });
      }
      if (selected === "/workspace") {
        return json(200, {
          current_path: "/workspace",
          parent_path: null,
          directories: [{ name: "fresh", path: "/workspace/fresh" }],
        });
      }
      if (selected === "/workspace/fresh") {
        return json(200, {
          current_path: "/workspace/fresh",
          parent_path: "/workspace",
          directories: [],
        });
      }
      throw new Error(`unexpected directory path ${selected}`);
    }
    if (request.method() === "GET" && path === `${base}/sessions`) {
      return json(200, projectHasSessions ? [session, replacementSession] : []);
    }
    if (request.method() === "GET" && path === `/api/projects/${createdProject.id}/sessions`) {
      return json(200, []);
    }
    if (request.method() === "GET" && path === `/api/sessions/${session.id}`) {
      return json(200, session);
    }
    if (request.method() === "GET" && path === `/api/sessions/${replacementSession.id}`) {
      return json(200, replacementSession);
    }
    if (request.method() === "GET" && path === "/api/settings/last-session") return json(404, { code: "not_found", message: "no last session remembered" });
    if (request.method() === "PUT" && path === "/api/settings/last-session") return json(200, request.postDataJSON());
    if (request.method() === "GET" && path === "/api/settings/layout") {
      return json(404, { code: "not_found", message: "no layout saved" });
    }
    if (request.method() === "PUT" && path === "/api/settings/layout") {
      return json(200, request.postDataJSON());
    }
    if (request.method() === "GET" && path === "/api/harness-login") {
      // The login coordinator polls the listing while a controller is
      // attached; no login is waiting in this probe.
      return json(200, { pending: [], csrf: "login-csrf" });
    }
    if (request.method() === "GET" && path === "/api/providers") {
      // The probe drives a single claude session and never opens the picker.
      return json(200, { providers: ["claude"] });
    }
    if (request.method() === "GET" && path === "/api/settings/tutorial") return json(200, { version: 1, dismissed: true, lesson: "welcome", step: 0, done: [], scope: "operator" });
    if (request.method() === "GET" && path === "/api/settings") {
      return json(200, { skip_permissions: false, skip_close_confirm: false, skip_remove_project_confirm: false });
    }
    if (request.method() === "GET" && path === "/api/git-identities") {
      return json(200, { profiles: [gitIdentity], default_profile_id: gitIdentity.id, revision: 1 });
    }
    if (request.method() === "GET" && path === `${base}/lanes`) return json(200, { lanes: [] });
    if (request.method() === "GET" && path === `${base}/taskmaster`) {
      return json(200, {
        path: project.canonical_path,
        config_path: ".taskmaster/config.json",
        tasks_path: ".taskmaster/tasks/tasks.json",
        docs_path: ".taskmaster/docs",
      });
    }
    if (request.method() === "GET" && path === `${base}/taskmaster/events`) {
      // The real handler (internal/tasks/handler.go Events) streams SSE for
      // any registered project: a connected comment, then tasks-changed only
      // when the tasks file changes. Nothing changes under this mock, so the
      // body ends after the comment and pins the client reconnect far out in
      // place of the stream the server would hold open.
      return route.fulfill({
        status: 200,
        contentType: "text/event-stream",
        body: ": connected\n\nretry: 3600000\n\n",
      });
    }
    if (request.method() === "GET" && path === `${base}/files/events`) {
      return route.fulfill({
        status: 200,
        contentType: "text/event-stream",
        body: ": connected\n\nretry: 3600000\n\n",
      });
    }
    if (request.method() === "GET" && path.startsWith(`${base}/taskmaster/tasks/`)) {
      const id = decodeURIComponent(path.slice(`${base}/taskmaster/tasks/`.length));
      const found = Object.values(tasks).flatMap((tag) => tag.tasks).find((task) => String(task.id) === id);
      return found === undefined ? json(404, { error: "not_found" }) : json(200, found);
    }
    if (request.method() === "POST" && path === `${base}/taskmaster/tasks`) {
      assert.equal(request.headers()["x-csrf-token"], "probe-csrf");
      const input = request.postDataJSON();
      addedTasks.push(input);
      const id = String(39 + addedTasks.length);
      tasks = {
        ...tasks,
        master: { ...tasks.master, tasks: [...tasks.master.tasks, { id, title: input.title, status: "pending", priority: input.priority, description: input.description, details: input.details }] },
      };
      return json(201, { id });
    }
    if (request.method() === "GET" && path === `${base}/taskmaster/tasks`) return json(200, tasks);
    if (request.method() === "GET" && path === `${base}/taskmaster/prds`) return json(200, prds);
    if (request.method() === "POST" && path === `${base}/taskmaster/prds`) {
      assert.equal(request.headers()["x-csrf-token"], "probe-csrf");
      uploads.push(request.postDataJSON());
      prds = ["prd.md"];
      return json(200, {});
    }
    if (request.method() === "POST" && path === `${base}/taskmaster/generate`) {
      generations.push(request.postDataJSON());
      // An instantly-exited run: the panel reloads the canonical tasks
      // without a timer, so the probe never waits on wall-clock.
      tasks = generatedTasks;
      return json(200, {
        id: "run-1",
        project_id: project.id,
        argv: ["noise", "tasks", "generate"],
        prd: "prd.md",
        status: "exited",
        exit_code: 0,
        started_at: "2026-08-01T00:00:00Z",
        finished_at: "2026-08-01T00:00:02Z",
        duration_ms: 2000,
      });
    }
    if (request.method() === "GET" && path === `${base}/files`) {
      return json(200, { path: "", at_root: true, entries: [] });
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

  const socketFrames = [];
  const socketEvents = [];
  const heldComposerAcks = [];
  let holdComposerAcks = false;
  let socketOpens = 0;
  let terminalConnectUrl = null;
  let terminalSocket = null;
  const connectUrls = [];
  await page.routeWebSocket(`${origin.replace("http", "ws")}/api/sessions/*/terminal?*`, (socket) => {
    terminalSocket = socket;
    terminalConnectUrl = socket.url();
    connectUrls.push(socket.url());
    const connection = ++socketOpens;
    socket.onMessage((message) => {
      if (typeof message !== "string") return;
      const frame = JSON.parse(message);
      socketFrames.push(frame);
      socketEvents.push({ connection, frame });
      if (frame.type === "input") {
        const ack = (code) => socket.send(JSON.stringify({
          type: "status", code, message: code === "sent" ? "" : `write ${code}`,
        }));
        if (holdComposerAcks) heldComposerAcks.push(ack);
        else ack("sent");
      }
    });
    socket.send(JSON.stringify({ type: "attached", token: 1, generation: 1 }));
    socket.send(JSON.stringify({
      type: "output",
      data: Buffer.from("READY\r\n").toString("base64"),
    }));
  });

  await page.goto(origin, { waitUntil: "networkidle" });
  const liveTerminal = page.locator("[data-live-terminal]");
  await liveTerminal.waitFor();
  if (process.env.NOISEDECK_COMPOSER_PROBE === "1") {
    page.setDefaultTimeout(15000);
    const field = page.locator('[data-region="terminal-compose"]').getByRole("textbox", { name: "Terminal input", exact: true });
    assert.equal(await field.isVisible(), false, "desktop input goes straight to the terminal");
    await page.locator("[data-live-more]").click();
    await page.locator("[data-more-popover]").waitFor();
    assert.equal(await page.locator("[data-more-popover] [data-draft-composer]").count(), 0, "desktop offers no draft composer");
    await page.locator("[data-popover-backdrop]").click({ position: { x: 8, y: 8 } });
    await page.locator("[data-more-popover]").waitFor({ state: "detached" });
    await page.setViewportSize({ width: 390, height: 844 });
    await page.reload({ waitUntil: "networkidle" });
    await field.waitFor();
    assert.equal(await field.getAttribute("rows"), "1", "the phone compose row is one line");
    const from = socketFrames.length;
    await field.fill("First line café\nSecond line 日本語");
    await field.click({ position: { x: 10, y: 10 } });
    assert.ok(await field.evaluate(element => element.selectionStart < element.value.length), "a tap places the draft caret before the end");
    await page.keyboard.insertText("Edited ");
    await page.keyboard.press("End");
    await page.keyboard.insertText(" again");
    assert.equal(socketFrames.slice(from).filter(f => f.type === "input").length, 0, "editing sends no terminal input");
    const draft = await field.inputValue();
    assert.ok(draft.includes("Edited ") && draft.includes(" again") && draft.includes("日本語"));
    const sendButton = page.getByRole("button", { name: "Send", exact: true });
    const info = page.locator("[data-info-status]");
    const ack = (code) => {
      assert.ok(heldComposerAcks.length > 0, `missing held acknowledgement for ${code}`);
      heldComposerAcks.shift()(code);
    };
    holdComposerAcks = true;
    await page.keyboard.press("Enter");
    await waitFor(() => socketFrames.slice(from).filter(f => f.type === "input").length >= 2, "Return sends the draft");
    assert.deepEqual(socketFrames.slice(from).filter(f => f.type === "input").map(f => Buffer.from(f.data, "base64").toString()), [draft.replace(/\n/g, "\r"), "\r"], "paste precedes submit");
    assert.equal(await field.inputValue(), draft, "the draft clears before any write is acknowledged");
    ack("sent");
    await page.waitForTimeout(50);
    assert.equal(await field.inputValue(), draft, "paste success clears the draft before Return succeeds");
    ack("sent");
    await waitFor(async () => await field.inputValue() === "", "final write success clears the submitted snapshot");
    holdComposerAcks = false;
    await field.fill("Unsent draft");
    await page.setViewportSize({ width: 1024, height: 768 });
    assert.equal(await field.isVisible(), false);
    await page.setViewportSize({ width: 390, height: 844 });
    await field.waitFor();
    assert.equal(await field.inputValue(), "Unsent draft", "widening past the phone layout preserves the session draft");
    await page.reload({ waitUntil: "networkidle" });
    await field.waitFor();
    assert.equal(await field.inputValue(), "", "draft text is never persisted");

    // Empty input is one tracked Return, not a paste with an empty payload.
    holdComposerAcks = true;
    let before = socketFrames.length;
    await sendButton.click();
    await waitFor(() => socketFrames.slice(before).filter(f => f.type === "input").length === 1, "empty Return");
    assert.equal(Buffer.from(socketFrames.at(-1).data, "base64").toString(), "\r");
    ack("sent");
    await waitFor(() => heldComposerAcks.length === 0, "empty Return acknowledgement");
    holdComposerAcks = false;

    const revealOutcome = async () => {
      await page.mouse.move(0, 0);
      await field.evaluate(element => element.blur());
    };

    // A second submit while either write is pending is rejected, not duplicated.
    await field.fill("Overlapping draft");
    holdComposerAcks = true;
    await sendButton.click();
    await waitFor(() => heldComposerAcks.length === 2, "first overlapping flush");
    before = socketFrames.length;
    await page.locator("[data-mobile-compose-form]").dispatchEvent("submit");
    await page.waitForTimeout(50);
    assert.equal(socketFrames.length, before, "an overlapping flush emitted more terminal input");
    await revealOutcome();
    await info.filter({ hasText: "Terminal input is not ready" }).waitFor({ state: "attached" });
    assert.equal(await field.inputValue(), "Overlapping draft");
    ack("sent");
    ack("sent");
    await waitFor(async () => await field.inputValue() === "", "overlap draft success");
    holdComposerAcks = false;

    // Success applies only to the submitted snapshot, never to later edits.
    await field.fill("Submitted snapshot");
    holdComposerAcks = true;
    await sendButton.click();
    await waitFor(() => heldComposerAcks.length === 2, "snapshot writes");
    await field.fill("Later edit");
    ack("sent");
    ack("sent");
    await page.waitForTimeout(50);
    assert.equal(await field.inputValue(), "Later edit", "final success erased an edit made while sending");
    holdComposerAcks = false;
    await sendButton.click();
    await waitFor(async () => await field.inputValue() === "", "later edit sent successfully");

    for (const code of ["failed", "uncertain"]) {
      const value = `${code} draft`;
      await field.fill(value);
      holdComposerAcks = true;
      await sendButton.click();
      await waitFor(() => heldComposerAcks.length === 2, `${code} writes`);
      ack("sent");
      ack(code);
      await revealOutcome();
      await info.filter({ hasText: "may have been partially sent. Inspect the terminal before retrying" }).waitFor({ state: "attached" });
      assert.equal(await field.inputValue(), value, `${code} outcome erased the draft`);
      holdComposerAcks = false;
    }

    // Disposal fences callbacks from the old session and preserves its draft.
    await field.fill("Stale session draft");
    holdComposerAcks = true;
    await sendButton.click();
    await waitFor(() => heldComposerAcks.length === 2, "stale session writes");
    await page.locator('[data-session="session-2"]').evaluate(element => element.click());
    await waitFor(() => connectUrls.at(-1)?.includes("/session-2/"), "second session attachment");
    ack("sent");
    ack("sent");
    assert.equal(await field.inputValue(), "", "a stale callback populated the newly selected session");
    await page.locator('[data-session="session-1"]').evaluate(element => element.click());
    await waitFor(() => connectUrls.at(-1)?.includes("/session-1/"), "first session reattachment");
    assert.equal(await field.inputValue(), "Stale session draft", "the stale session lost its unsubmitted draft");

    // Closing before either acknowledgement settles both writes as interrupted.
    await sendButton.click();
    await waitFor(() => heldComposerAcks.length === 2, "interrupted writes");
    await terminalSocket.close();
    await revealOutcome();
    await info.filter({ hasText: "Inspect the terminal before retrying" }).waitFor({ state: "attached" });
    assert.equal(await field.inputValue(), "Stale session draft", "interrupted writes erased the draft");
    console.log("phone composer browser probe: all assertions passed");
    await context.close(); await browser.close(); await new Promise(done => server.close(done)); process.exit(0);
  }
  if (attentionOnly) {
    page.setDefaultTimeout(15000);
    await page.bringToFront();
    const first = page.locator('[data-session="session-1"]');
    const second = page.locator('[data-session="session-2"]');
    const visibleMark = (row, state) => row.locator(`[data-conversation-attention="${state}"]`).locator("visible=true");
    assert.equal(await visibleMark(first, "needs_input").count(), 1, "native input need is distinct from activity");
    assert.equal(await visibleMark(second, "ended").count(), 1, "background completion is unseen");
    assert.deepEqual(attentionReceipts, [], "viewing an input request does not dismiss it or another conversation");
    await page.getByRole("button", { name: "Project tasks", exact: true }).click();
    await page.locator("[data-tm-add-task]").click();
    await page.getByRole("dialog").waitFor();
    session.conversation_attention = { ...session.conversation_attention, sequence: 3, ended_sequence: 3, state: "ended" };
    await waitFor(async () => await visibleMark(first, "ended").count() === 1, "new boundary arrives behind the dialog", 15000);
    assert.deepEqual(attentionReceipts, [], "a covering dialog prevents automatic acknowledgement");
    session.conversation_attention.state = "needs_input";
    session.conversation_attention.sequence = 4;
    await page.getByRole("button", { name: "Cancel", exact: true }).click();
    await waitFor(() => attentionReceipts.includes("session-1"), "visible conversation acknowledges its completed boundary", 15000);
    await waitFor(async () => await visibleMark(first, "needs_input").count() === 1, "pending input remains after view", 15000);
    assert.equal(replacementSession.conversation_attention.seen_sequence, 0, "another conversation stays unseen");
    await page.reload({ waitUntil: "networkidle" });
    await liveTerminal.waitFor();
    assert.equal(await visibleMark(second, "ended").count(), 1, "unseen backend state survives reload");
    await second.click();
    await waitFor(() => attentionReceipts.includes("session-2"), "selecting the other conversation acknowledges it", 15000);
    await waitFor(async () => await visibleMark(second, "ended").count() === 0, "receipt is reflected in fresh state", 15000);
    await page.setViewportSize({ width: 390, height: 844 });
    if (await visibleMark(first, "needs_input").count() === 0) await page.getByRole("button", { name: "Sessions", exact: true }).click();
    assert.equal(await visibleMark(first, "needs_input").count(), 1, "phone rows retain pending native input");
    console.log("conversation-attention browser probe: all assertions passed");
    await context.close(); await browser.close(); await new Promise(done => server.close(done)); process.exit(0);
  }
  // Every row renders the spinner markup twice — the desktop meta cell and
  // the phone's leading status cell — and only the viewport's scope is
  // visible, so the assertions count visible spinners, never DOM matches.
  const visibleSpinners = page
    .locator('[data-session] [aria-label="output streaming"]')
    .locator("visible=true");
  if (activityOnly) {
    await visibleSpinners.first().waitFor();
    assert.equal(activityRequests, 1, "one content-free activity stream covers all session rows");
    assert.equal(await visibleSpinners.count(), 1,
      "only the selected verified-running row shows output activity");
    // The recovering row stays listed: its whole row chrome is the
    // wordless attention dot, and the output event naming it never spins
    // it — the spinner is gated on the verified running presentation.
    const recoveringRow = page.locator('[data-session="session-2"]');
    assert.equal(await recoveringRow.count(), 1, "a recovering row stays in the sidebar");
    await recoveringRow.locator("[data-meta-attention]").locator("visible=true").waitFor();
    assert.equal(await recoveringRow.locator('[aria-label="output streaming"]').locator("visible=true").count(), 0,
      "a recovering row never reports output activity");
    await visibleSpinners.first().waitFor({ state: "hidden", timeout: 5000 });
    assert.equal(await visibleSpinners.count(), 0, "the row returns to passive metadata after silence");
    const rowStates = await page.locator("[data-session-meta]").allTextContents();
    assert.deepEqual(rowStates, ["", ""], "silence restores status-free passive metadata on both rows");
    console.log("multi-session output-activity browser probe: all assertions passed");
    await context.close();
    await browser.close();
    await new Promise((done) => server.close(done));
    process.exit(0);
  }
  assert.equal(
    await liveTerminal.getAttribute("data-terminal-scrollback-lines"),
    "1000",
    "desktop xterm is mounted with the bounded scrollback default",
  );

  const panel = page.locator('[data-region="tasks"]');
  const modals = page.locator('[data-region="modals"]');
  const filesPanel = page.locator('[data-region="files"]');
  const tasksButton = page.getByRole("button", { name: "Project tasks", exact: true });
  const filesButton = page.getByRole("button", { name: "Files", exact: true });
  const content = page.locator('[data-region="content"]');
  const terminalCard = page.locator('[data-region="terminal-card"]');
  const resizeCount = () => socketFrames.filter((frame) => frame.type === "resize").length;
  // A viewport change that alters the terminal's cols or rows is answered by
  // a resize frame carrying the new geometry; one that changes neither is
  // answered by nothing. Wait for that frame rather than for a stretch of
  // quiet: a quiet window opened before the frame arrives expires just as
  // happily as one opened after it, which leaves the next baseline short by
  // one and blames the following action for the resize.
  const setViewport = async (size, label, { reports = true } = {}) => {
    const before = resizeCount();
    await page.setViewportSize(size);
    await page.waitForFunction(
      ([w, h]) => window.innerWidth === w && window.innerHeight === h,
      [size.width, size.height],
    );
    if (reports) {
      await waitFor(() => resizeCount() > before, `${label} reports its new terminal geometry`);
    }
    await settled(resizeCount, `${label} resize traffic`);
  };
  const waitForPanelState = async (locator, open, label) => {
    await locator.evaluate(async (element) => {
      await new Promise((resolve) => requestAnimationFrame(() => requestAnimationFrame(resolve)));
      await Promise.allSettled(element.getAnimations().map((animation) => animation.finished));
    });
    await waitFor(
      () => locator.evaluate((element, expectedOpen) => {
        if (element.getAttribute("data-open") !== String(expectedOpen)) return false;
        const style = getComputedStyle(element);
        if (!expectedOpen) return style.visibility === "hidden";
        const offset = style.transform === "none"
          ? 0
          : new DOMMatrixReadOnly(style.transform).m41;
        return style.visibility === "visible" && Math.abs(offset) < 0.5;
      }, open),
      label,
    );
  };
  const waitForSidebarState = async (collapsed, label) => {
    await page.locator('[data-region="sidebar"]').evaluate(async (element) => {
      await new Promise((resolve) => requestAnimationFrame(() => requestAnimationFrame(resolve)));
      await Promise.allSettled(element.getAnimations().map((animation) => animation.finished));
    });
    await waitFor(
      () => page.locator('[data-region="sidebar"]').evaluate((element, expectedCollapsed) => {
        const workspace = element.closest("[data-workspace]");
        if (workspace?.getAttribute("data-collapsed") !== String(expectedCollapsed)) return false;
        const style = getComputedStyle(element);
        const offset = style.transform === "none"
          ? 0
          : new DOMMatrixReadOnly(style.transform).m41;
        return expectedCollapsed
          ? style.visibility === "hidden"
          : style.visibility === "visible" && Math.abs(offset) < 0.5;
      }, collapsed),
      label,
    );
  };
  // boundingBox reads null while an element is still unrendered or mid
  // transition, so a measurement taken the instant after a click describes a
  // moment the user never sees. Waiting for layout keeps the measurement
  // about geometry rather than about scheduling.
  const box = async (locator) => {
    return eventually(
      () => locator.boundingBox(),
      (b) => b !== null,
      "expected a laid-out element",
    );
  };
  const assertLongProjectName = async (label) => {
    const row = page.locator('[data-project="project-1"]');
    const name = row.locator("[data-project-name]");
    await row.hover();
    const geometry = await name.evaluate((element) => {
      const projectRow = element.closest("[data-project]");
      const menu = projectRow?.querySelector("[data-project-menu]");
      if (!(projectRow instanceof HTMLElement) || !(menu instanceof HTMLElement)) return null;
      const nameBox = element.getBoundingClientRect();
      const menuBox = menu.getBoundingClientRect();
      return {
        clipped: element.scrollWidth > element.clientWidth,
        separated: nameBox.right <= menuBox.left,
        rowContained: projectRow.scrollWidth <= projectRow.clientWidth,
      };
    });
    assert.deepEqual(geometry, {
      clipped: true,
      separated: true,
      rowContained: true,
    }, `${label} long project name truncates before the fixed menu action`);
    // The name span truncates and carries no hint of its own (one hint per
    // row); the full name stays reachable through the row toggle's hint.
    const toggleHint = await row.locator("[data-project-toggle]").getAttribute("data-hint");
    assert.ok(toggleHint?.includes(LONG_PROJECT_NAME), `${label} preserves the full name`);
  };
  const near = (actual, expected, what, tolerance = 0.5) =>
    assert.ok(Math.abs(actual - expected) < tolerance, `${what}: ${actual} is not ${expected}`);

  // ---- the trigger ----
  await tasksButton.waitFor();
  const projectToggle = page.locator('[data-project-toggle="project-1"]');
  const projectName = projectToggle.locator('[data-project-name]');
  await projectName.waitFor();
  if (process.env.NOISEDECK_SIDEBAR_SIZE_PROBE === "1") {
    const handle = page.getByRole("separator", { name: "Resize sidebar" });
    const sidebar = page.locator('[data-region="sidebar"]');
    const width = async () => Math.round((await sidebar.boundingBox()).width);
    await handle.waitFor();
    assert.equal(await width(), 272);
    const before = resizeCount();
    const start = await handle.boundingBox();
    await page.mouse.move(start.x + 4, start.y + 100);
    await page.mouse.down();
    await page.mouse.move(start.x + 132, start.y + 100);
    await page.mouse.up();
    assert.equal(await width(), 400, "drag changes sidebar layout width");
    await waitFor(() => resizeCount() > before, "sidebar drag refits the real terminal");
    assert.equal(await page.evaluate(() => localStorage.getItem("noisedeck.sidebar.width")), "400");
    await page.reload({ waitUntil: "networkidle" });
    await handle.waitFor();
    assert.equal(await width(), 400, "width survives reload");
    await handle.focus();
    await page.keyboard.press("ArrowRight");
    assert.equal(await width(), 416, "keyboard adjustment");
    await page.keyboard.press("Home");
    assert.equal(await width(), 240, "minimum width");
    await page.keyboard.press("End");
    assert.equal(await width(), 560, "maximum width");
    await page.setViewportSize({ width: 800, height: 900 });
    await waitFor(async () => await width() === 440, "temporary viewport clamp");
    assert.equal(await page.evaluate(() => localStorage.getItem("noisedeck.sidebar.width")), "560", "clamp does not overwrite preference");
    await page.setViewportSize({ width: 1280, height: 900 });
    await waitFor(async () => await width() === 560, "desktop restores preferred width");
    await handle.focus();
    await page.keyboard.press("Enter");
    assert.equal(await width(), 272, "keyboard reset");
    const cancelStart = await handle.boundingBox();
    await page.mouse.move(cancelStart.x + 4, cancelStart.y + 100);
    await page.mouse.down();
    await page.mouse.move(cancelStart.x + 100, cancelStart.y + 100);
    await handle.dispatchEvent("pointercancel", { pointerId: 1 });
    await page.mouse.up();
    assert.equal(await width(), 272, "cancelled drag restores width");
    await page.getByRole("button", { name: "Collapse sidebar" }).click();
    await waitForSidebarState(true, "sidebar closes");
    assert.equal(await handle.isVisible(), false, "collapsed handle is hidden");
    await page.getByRole("button", { name: "Show sidebar" }).click();
    await waitForSidebarState(false, "sidebar reopens");
    await handle.waitFor();
    await page.setViewportSize({ width: 390, height: 844 });
    assert.equal(await handle.isVisible(), false, "phone has no resize handle");
    await page.setViewportSize({ width: 1280, height: 900 });
    await handle.waitFor();
    await page.evaluate(() => localStorage.setItem("noisedeck.sidebar.width", "Infinity"));
    await page.reload({ waitUntil: "networkidle" });
    await handle.waitFor();
    assert.equal(await width(), 272, "invalid stored preference uses default");
    await page.evaluate(() => { Storage.prototype.setItem = () => { throw new Error("blocked"); }; });
    await handle.focus();
    await page.keyboard.press("ArrowRight");
    assert.equal(await width(), 288, "blocked storage does not prevent resizing");
    console.log("sidebar-size browser probe: all assertions passed");
    await context.close();
    await browser.close();
    await new Promise(done => server.close(done));
    process.exit(0);
  }
  await assertLongProjectName("desktop sidebar");
  assert.equal(await projectToggle.getByText(/^\d+ · /).count(), 0, "expanded project omits its session count");
  await projectToggle.click();
  assert.equal(await projectToggle.getAttribute("aria-expanded"), "false");
  assert.equal(await projectName.isVisible(), true, "collapsed project keeps its name");
  assert.equal(await projectToggle.getByText(/^\d+ · /).count(), 0, "collapsed project omits its session count");

  await page.locator("[data-projects-menu]").click();
  await page.locator("[data-projects-menu-new-project]").click();
  await page.locator('[data-project-source="local"]').click();
  await page.locator("[data-project-browse]").click();
  await page.locator('[data-directory-path="/workspace/fresh"]').click();
  await page.getByRole("button", { name: "Choose this directory" }).click();
  assert.equal(await page.getByRole("combobox", { name: "Git identity" }).count(), 0, "the only identity applies without a field");
  // The desktop card's primary says Add project (comp desktop-r32, r12
  // rename); the phone route keeps Open project via the copy pair.
  await page.locator("[data-project-form]").getByRole("button", { name: "Add project" }).click();
  const createdProjectToggle = page.locator('[data-project-toggle="project-2"]');
  await createdProjectToggle.waitFor();
  assert.equal(await projectToggle.getAttribute("aria-expanded"), "false");
  assert.equal(await createdProjectToggle.getAttribute("aria-expanded"), "true");
  await page.locator('[data-project="project-2"] [data-project-actions] [data-new-session]')
    .waitFor();

  // Project position rows repeat in place: the sidebar moves behind the
  // popover, the boundary state updates, and the panel keeps the geometry
  // from the opening gesture until a closing action is chosen.
  const projectRowOrder = () => page.locator("[data-project-row]").evaluateAll(
    (rows) => rows.map((row) => row.getAttribute("data-project")),
  );
  assert.deepEqual(await projectRowOrder(), ["project-2", "project-1"]);
  const createdProjectMenu = page.locator('[data-project-menu="project-2"]');
  await createdProjectToggle.hover();
  await createdProjectMenu.click();
  const positionMenu = page.locator("[data-project-menu-popover]");
  await positionMenu.waitFor();
  // The menu mounts unpositioned and takes its coordinates in the next
  // animation frame; a baseline captured before that frame reads x=0, so
  // every frozen-geometry comparison after a move reports the 8px clamp
  // as drift.
  await page.waitForFunction(() => {
    const mounted = document.querySelector("[data-project-menu-popover]");
    return mounted !== null && mounted.style.left !== "";
  });
  // The info bar must agree with the row's data-hint: a press that changes
  // which row sits at a boundary re-announces the new hint on the bar's
  // focusin path, and a repeated boundary press leaves it standing.
  const infoStatus = page.locator("[data-info-status]");
  const positionBefore = await box(positionMenu);
  const moveUp = positionMenu.locator("[data-project-menu-move-up]");
  const moveDown = positionMenu.locator("[data-project-menu-move-down]");
  assert.equal(await moveUp.getAttribute("aria-disabled"), "true");
  assert.equal(await moveUp.getAttribute("data-hint"), "Already first in the sidebar");
  assert.equal(await moveDown.getAttribute("aria-disabled"), null);
  assert.equal(await moveDown.getAttribute("data-hint"), "Swap places with the project below");

  await moveUp.click({ force: true });
  assert.deepEqual(await projectRowOrder(), ["project-2", "project-1"], "disabled Move up is a no-op");
  assert.equal(await positionMenu.count(), 1, "a boundary press keeps the project menu open");
  assert.equal(await infoStatus.textContent(), "Already first in the sidebar",
    "a boundary press shows the blocked hint in the info bar");

  await moveDown.click();
  await page.waitForFunction(() => [...document.querySelectorAll("[data-project-row]")]
    .map((row) => row.getAttribute("data-project")).join(",") === "project-1,project-2");
  assert.equal(await positionMenu.count(), 1, "a Move press keeps the project menu open");
  assert.equal(await moveDown.getAttribute("aria-disabled"), "true");
  assert.equal(await moveDown.getAttribute("data-hint"), "Already last in the sidebar");
  assert.equal(await moveUp.getAttribute("aria-disabled"), null);
  assert.equal(await moveUp.getAttribute("data-hint"), "Swap places with the project above");
  assert.equal(await infoStatus.textContent(), "Already last in the sidebar",
    "the move that lands the focused row on a boundary re-announces its new hint");
  await moveDown.click({ force: true });
  assert.deepEqual(await projectRowOrder(), ["project-1", "project-2"], "repeating the boundary press is a no-op");
  assert.equal(await infoStatus.textContent(), "Already last in the sidebar",
    "a repeated boundary press leaves the info bar agreeing with the row");
  const positionAfterDown = await box(positionMenu);
  near(positionAfterDown.x, positionBefore.x, "project menu x stays frozen after Move down");
  near(positionAfterDown.y, positionBefore.y, "project menu y stays frozen after Move down");

  // Move to top is the long move and shares Move up's boundary: from last
  // it is enabled, it lands the row first in one press, the menu stays open
  // like the other position rows, and at the top it reads as blocked.
  const moveTop = positionMenu.locator("[data-project-menu-move-top]");
  assert.equal(await moveTop.getAttribute("aria-disabled"), null);
  assert.equal(await moveTop.getAttribute("data-hint"), "Move to the top of the sidebar");
  await moveTop.click();
  await page.waitForFunction(() => [...document.querySelectorAll("[data-project-row]")]
    .map((row) => row.getAttribute("data-project")).join(",") === "project-2,project-1");
  assert.equal(await positionMenu.count(), 1, "Move to top keeps the project menu open");
  assert.equal(await moveTop.getAttribute("aria-disabled"), "true");
  assert.equal(await moveTop.getAttribute("data-hint"), "Already first in the sidebar");
  assert.equal(await infoStatus.textContent(), "Already first in the sidebar",
    "landing first through Move to top re-announces the boundary in the info bar");
  await moveTop.click({ force: true });
  assert.deepEqual(await projectRowOrder(), ["project-2", "project-1"], "Move to top at the top is a no-op");
  const positionAfterTop = await box(positionMenu);
  near(positionAfterTop.x, positionBefore.x, "project menu x stays frozen after Move to top");
  near(positionAfterTop.y, positionBefore.y, "project menu y stays frozen after Move to top");
  // Back down, then Move up still works as before.
  await moveDown.click();
  await page.waitForFunction(() => [...document.querySelectorAll("[data-project-row]")]
    .map((row) => row.getAttribute("data-project")).join(",") === "project-1,project-2");
  await moveUp.click();
  await page.waitForFunction(() => [...document.querySelectorAll("[data-project-row]")]
    .map((row) => row.getAttribute("data-project")).join(",") === "project-2,project-1");
  assert.equal(await infoStatus.textContent(), "Already first in the sidebar",
    "moving back to first re-announces the other boundary in the info bar");
  const positionAfterUp = await box(positionMenu);
  near(positionAfterUp.x, positionBefore.x, "project menu x stays frozen after Move up");
  near(positionAfterUp.y, positionBefore.y, "project menu y stays frozen after Move up");
  await page.locator("[data-popover-backdrop]").evaluate((backdrop) => backdrop.click());
  await positionMenu.waitFor({ state: "detached" });

  await projectToggle.click();
  const order = await page.evaluate(() => {
    const bar = document.querySelector('[data-region="terminal-titlebar"]');
    return [...bar.querySelectorAll("button")]
      .map((b) => b.dataset.liveFiles !== undefined
        ? "files"
        : b.dataset.liveTasks !== undefined
        ? "tasks"
        : "other")
      .filter((b) => b !== "other");
  });
  assert.deepEqual(order, ["files", "tasks"], "Files sits before Tasks in the top bar");
  assert.ok((await tasksButton.innerText()).trim().includes("Tasks"), "Tasks button keeps its label alongside the count badge");
  assert.equal(await tasksButton.getAttribute("aria-label"), "Project tasks");
  assert.equal(await filesButton.getAttribute("aria-label"), "Files");
  assert.equal(await tasksButton.getAttribute("aria-pressed"), "false");
  assert.equal(await panel.isVisible(), false, "the panel starts closed");

  // The right cluster owns the bar's right edge: the identity takes the
  // left and Files/Tasks/More push to the far end through the cluster's
  // own spacer, so the controls never glue themselves to the session name.
  {
    const barBox = await box(page.locator('[data-region="terminal-titlebar"]'));
    const moreBox = await box(page.locator("[data-live-more]"));
    const identityBox = await box(page.locator("[data-terminal-identity]"));
    const filesBox = await box(filesButton);
    near(moreBox.x + moreBox.width, barBox.x + barBox.width,
      "the account menu hugs the titlebar's right edge");
    assert.ok(filesBox.x > identityBox.x + identityBox.width,
      "Files clears the identity — the right cluster is not glued to the session name");
  }

  // ---- docked geometry, at three desktop widths ----
  // The attachment carries its initial geometry in the connect URL, so it
  // attaches at the browser's viewport geometry; a resize frame only follows
  // a change. A settled first layout therefore sends none.
  await waitFor(() => terminalConnectUrl !== null, "the attachment opened its socket");
  {
    const params = new URL(terminalConnectUrl).searchParams;
    assert.ok(Number(params.get("cols")) > 0 && Number(params.get("rows")) > 0,
      "the connect URL reports the initial geometry");
  }
  await page.waitForTimeout(30);
  const closedTasksBox = await box(panel);
  const beforeOpenResizes = resizeCount();
  await tasksButton.click();
  assert.equal(await tasksButton.getAttribute("aria-pressed"), "true");
  await page.waitForTimeout(60);
  const midTasksBox = await box(panel);
  await waitForPanelState(panel, true, "the Tasks panel finishes sliding open");
  const openTasksBox = await box(panel);
  near(Math.abs(openTasksBox.x - closedTasksBox.x), openTasksBox.width,
    "the Tasks panel traverses exactly one panel width");
  near(midTasksBox.x, openTasksBox.x,
    "the Tasks panel is already open at the first measured frame");
  await waitFor(
    () => resizeCount() === beforeOpenResizes + 1,
    "the completed Tasks toggle reports one terminal resize",
  );
  await page.waitForTimeout(120);
  assert.equal(resizeCount(), beforeOpenResizes + 1,
    "the Tasks toggle reports exactly one terminal resize");
  await panel.getByRole("button", { name: /Wire the provider descriptor/ }).waitFor();

  // The Tasks panel rides the shared docked-panel resizing: a drag on its
  // left boundary narrows it by exactly the drag, Enter returns the
  // stylesheet's responsive width, and nothing is saved before either.
  const tasksHandle = page.locator('[data-panel-resize="tasks"]');
  await tasksHandle.waitFor();
  const tasksSaved = () => page.evaluate(() => localStorage.getItem("noisedeck.panel.tasks.width"));
  assert.equal(await tasksSaved(), null, "no Tasks width is saved before the user resizes");
  const beforeTasksDrag = await box(panel);
  await page.mouse.move(beforeTasksDrag.x + 4, beforeTasksDrag.y + 100);
  await page.mouse.down();
  await page.mouse.move(beforeTasksDrag.x + 84, beforeTasksDrag.y + 100, { steps: 6 });
  await page.mouse.up();
  const narrowedTasks = await box(panel);
  near(narrowedTasks.width, beforeTasksDrag.width - 80, "an 80px right drag narrows the Tasks panel 80px");
  assert.equal(await tasksSaved(), String(Math.round(beforeTasksDrag.width - 80)), "the drag commits the Tasks width");
  await tasksHandle.focus();
  await page.keyboard.press("ArrowLeft");
  const steppedTasks = await box(panel);
  near(steppedTasks.width, narrowedTasks.width + 16, "ArrowLeft widens the Tasks panel by one keyboard step");
  await page.keyboard.press("Enter");
  const resetTasks = await box(panel);
  near(resetTasks.width, beforeTasksDrag.width, "Enter returns the Tasks panel's responsive width");
  assert.equal(await tasksSaved(), null, "Enter clears the saved Tasks width");

  // The left dock uses the same lifecycle. Its intermediate flex widths
  // remain browser presentation only; xterm and the PTY settle once.
  const openSidebarBox = await box(page.locator('[data-region="sidebar"]'));
  const beforeCollapseResizes = resizeCount();
  await page.getByRole("button", { name: "Collapse sidebar" }).click();
  await page.waitForTimeout(60);
  const midSidebarBox = await box(page.locator('[data-region="sidebar"]'));
  await waitForSidebarState(true, "the sidebar finishes sliding closed");
  const closedSidebarBox = await box(page.locator('[data-region="sidebar"]'));
  near(Math.abs(closedSidebarBox.x - openSidebarBox.x), openSidebarBox.width,
    "the sidebar traverses exactly one panel width");
  near(midSidebarBox.x, closedSidebarBox.x,
    "the sidebar is already collapsed at the first measured frame");
  await waitFor(
    () => resizeCount() === beforeCollapseResizes + 1,
    "sidebar collapse reports one final terminal resize",
  );
  const beforeExpandResizes = resizeCount();
  await page.getByRole("button", { name: "Show sidebar" }).click();
  await waitForSidebarState(false, "the sidebar finishes opening");
  await waitFor(
    () => resizeCount() === beforeExpandResizes + 1,
    "sidebar expansion reports one final terminal resize",
  );

  const dockedWidth = (rowWidth) =>
    Math.min(Math.min(Math.max(640, rowWidth * 0.58), 1000), rowWidth - TERMINAL_MIN_W);

  for (const width of [1800, 1400, 1160]) {
    await setViewport({ width, height: 900 }, "the width sweep");
    const contentBox = await box(content);
    const panelBox = await box(panel);
    const terminalBox = await box(terminalCard);
    near(contentBox.x, SIDEBAR_W, `content row starts right of the sidebar at ${width}`);
    near(panelBox.width, dockedWidth(contentBox.width), `docked width at ${width}`);
    near(
      panelBox.x + panelBox.width,
      contentBox.x + contentBox.width,
      `the panel's right edge at ${width}`,
    );
    near(panelBox.height, contentBox.height, `full content height at ${width}`);
    near(terminalBox.width, contentBox.width - panelBox.width, `the terminal keeps the rest at ${width}`);
    assert.ok(
      terminalBox.width >= TERMINAL_MIN_W - 0.5,
      `the terminal column is ${terminalBox.width} at ${width}, below its ${TERMINAL_MIN_W}px floor`,
    );
    assert.equal(
      await page.evaluate(() =>
        document.documentElement.scrollWidth <= document.documentElement.clientWidth),
      true,
      `no horizontal overflow at ${width}`,
    );
  }
  // 1160 is where the cap binds before the clamp's minimum.
  near((await box(terminalCard)).width, TERMINAL_MIN_W, "the terminal holds its floor at the cap");
  await setViewport({ width: 1400, height: 900 }, "the cap release");

  assert.equal(
    ["absolute", "fixed"].includes(await panel.evaluate((el) => getComputedStyle(el).position)),
    false,
    "the docked panel takes layout space rather than covering the terminal",
  );
  const seam = await panel.evaluate((el) => {
    const s = getComputedStyle(el);
    return {
      width: Number.parseFloat(s.borderLeftWidth),
      color: s.borderLeftColor,
      duration: s.transitionDuration,
      animation: s.animationName,
    };
  });
  assert.equal(seam.duration.split(", ").slice(0, 2).every((value) => value === "0s"), true,
    `Tasks margin/transform durations are not both 0s: ${seam.duration}`);
  near(seam.width, 1, "the 1px left seam");
  // Resolved through an element rather than read off the custom property,
  // so the comparison is in the same colour space the border reports.
  const borderStrong = await page.evaluate(() => {
    const probe = document.createElement("span");
    probe.style.color = "var(--color-border-strong)";
    // Inside the workspace: the dark ramp is scoped to [data-theme].
    document.querySelector("[data-workspace]").append(probe);
    const resolved = getComputedStyle(probe).color;
    probe.remove();
    return resolved;
  });
  assert.equal(seam.color, borderStrong, "the seam is --border-strong");
  assert.match(seam.duration, /0s/, "the docked panel has no slide transition");
  assert.equal(seam.animation, "none", "and must not animate in");

  // The header sits on the top bar's row and shares its hairline.
  const topBar = page.locator('[data-region="terminal-titlebar"]').locator("xpath=..");
  const panelHeader = panel.locator("h2", { hasText: "Tasks" }).locator("xpath=..");
  const topBarBox = await box(topBar);
  const headerBox = await box(panelHeader);
  near(topBarBox.height, TITLEBAR_H, "the workspace top bar's height");
  near(headerBox.height, TITLEBAR_H, "the panel header's height");
  near(headerBox.y, topBarBox.y, "the two headers share a row");
  near(
    headerBox.y + headerBox.height,
    topBarBox.y + topBarBox.height,
    "the hairline runs unbroken across the seam",
  );
  assert.equal(
    await panelHeader.evaluate((el) => getComputedStyle(el).backgroundColor),
    await topBar.evaluate((el) => getComputedStyle(el).backgroundColor),
    "the panel header carries the shared chrome fill (handoff §5)",
  );
  const title = await panel.locator("h2").evaluate((el) => {
    const s = getComputedStyle(el);
    return { text: el.textContent, size: s.fontSize, weight: s.fontWeight, family: s.fontFamily };
  });
  assert.equal(title.text, "Tasks");
  // chrome-title takes the 13px medium panel voice from the shared class
  // (handoff §5) so Tasks and Files can never disagree.
  assert.equal(title.size, "13px");
  assert.equal(title.weight, "500");
  await assertFontVoice(page, title.family, "ui", `the title is UI voice: ${title.family}`);

  // ---- the header's controls ----
  const closeButton = panel.getByRole("button", { name: "Close tasks" });
  await closeButton.waitFor();
  const closeBox = await box(closeButton);
  near(closeBox.width, 28, "the close control is a 28px icon button");
  near(closeBox.height, 28, "the close control is a 28px icon button");
  assert.equal(await panel.locator("[data-tm-close] svg").count(), 1, "a Lucide glyph, not a text ✕");
  await panel.getByRole("button", { name: "Add PRD" }).waitFor();
  for (const [name, target] of [["Files", filesButton], ["Project tasks", tasksButton]]) {
    const targetBox = await box(target);
    const visualBox = await box(target.locator("[data-panel-trigger-visual]"));
    near(targetBox.height, 28, `desktop ${name} target height`);
    near(visualBox.height, 28, `desktop ${name} visible pill height`);
    near(visualBox.width, targetBox.width, `desktop ${name} visual and target widths match`);
    // The trigger is the same secondary sm control as Add PRD and Filter:
    // the label's computed line box, tracking and weight match
    // theirs, and the glyph is 14px.
    const labelStyle = await target.evaluate((el) => {
      const s = getComputedStyle(el);
      return { lineHeight: s.lineHeight, letterSpacing: s.letterSpacing, fontWeight: s.fontWeight };
    });
    assert.equal(labelStyle.lineHeight, "13px", `desktop ${name} trigger line box`);
    assert.equal(labelStyle.letterSpacing, "0.052px", `desktop ${name} trigger tracking`);
    assert.equal(labelStyle.fontWeight, "500", `desktop ${name} trigger weight`);
    const iconBox = await box(target.locator("svg"));
    near(iconBox.width, 14, `desktop ${name} trigger icon width`);
    near(iconBox.height, 14, `desktop ${name} trigger icon height`);
  }
  const desktopAddPrd = panel.getByRole("button", { name: "Add PRD" });
  near((await box(desktopAddPrd)).height, 28, "desktop Add PRD target height");
  const desktopAddTask = panel.getByRole("button", { name: "Add task" });
  await desktopAddTask.waitFor();
  near((await box(desktopAddTask)).height, 28, "desktop Add task target height");
  const buttonStyle = (locator) => locator.evaluate((el) => {
    const s = getComputedStyle(el);
    return { fontSize: s.fontSize, lineHeight: s.lineHeight, letterSpacing: s.letterSpacing, borderColor: s.borderColor };
  });
  for (const [name, target] of [
    ["Add PRD", desktopAddPrd],
    ["Add task", desktopAddTask],
    ["Filter", panel.getByRole("button", { name: "Filter" })],
  ]) {
    const style = await buttonStyle(target);
    assert.equal(style.fontSize, "13px", `desktop ${name} label font size`);
    assert.equal(style.lineHeight, "13px", `desktop ${name} label line box`);
    assert.equal(style.letterSpacing, "0.052px", `desktop ${name} label tracking`);
    assert.equal(style.borderColor, "rgb(51, 71, 95)", `desktop ${name} border color`);
  }
  assert.equal(await panel.locator("button[disabled]").count(), 0, "no dead affordances");

  // ---- the toolbar ----
  const search = panel.getByRole("textbox", { name: "Search tasks" });
  const searchField = search.locator("xpath=..");
  // The field grows into the row and stops at 420px, so the cap is only
  // observable where the row is wide enough to reach it.
  await setViewport({ width: 1800, height: 900 }, "the search-field cap");
  near((await box(searchField)).width, 420, "the search field caps at 420px");
  await page.setViewportSize({ width: 1400, height: 900 });
  await page.waitForTimeout(50);
  const narrower = (await box(searchField)).width;
  assert.ok(narrower < 420 && narrower > 200, `the field yields to the row: ${narrower}`);
  assert.equal(
    await panel.evaluate(() => {
      const el = document.querySelector('[data-region="tasks"] [data-tm-search]')
        .closest("div");
      return getComputedStyle(el).flexWrap;
    }),
    "wrap",
    "the toolbar row wraps",
  );
  const sortLabel = panel.getByText("Sort", { exact: true });
  const sortType = await sortLabel.evaluate((el) => {
    const s = getComputedStyle(el);
    return { size: s.fontSize, family: s.fontFamily };
  });
  assert.equal(sortType.size, "12px");
  await assertFontVoice(page, sortType.family, "ui", `the Sort label is UI voice: ${sortType.family}`);

  const sortPills = panel.locator(".tabs-pill button");
  assert.deepEqual(await sortPills.allInnerTexts(), ["ID ↑", "Status", "Priority"],
    "the active sort pill carries a text arrow and inactive pills do not");
  assert.equal(await panel.locator(".tabs-pill button svg").count(), 0,
    "sort direction is a text arrow, not a Lucide icon");
  const pillWidths = await sortPills.evaluateAll((els) =>
    els.map((el) => el.getBoundingClientRect().width));
  // Existing licensed/OFL baselines differ in glyph advance, not padding.
  const expectedPillWidths = title.family.includes("Jost") ? [47, 64, 72] : [47, 63, 75];
  for (const [index, label] of ["ID", "Status", "Priority"].entries()) {
    near(pillWidths[index], expectedPillWidths[index], `${label} pill width`);
  }

  // ---- the board: three unboxed columns that scroll independently ----
  const boardGrid = panel.locator("[data-tm-board]");
  const grid = await boardGrid.evaluate((el) => {
    const s = getComputedStyle(el);
    return {
      columns: s.gridTemplateColumns.split(" ").filter(Boolean),
      gap: s.columnGap,
      padding: [s.paddingTop, s.paddingRight, s.paddingBottom, s.paddingLeft],
      overflow: s.overflow,
    };
  });
  assert.equal(grid.columns.length, 3, `three columns, got ${grid.columns.join(" ")}`);
  assert.equal(new Set(grid.columns).size, 1, `equal columns, got ${grid.columns.join(" ")}`);
  assert.equal(grid.gap, "20px");
  assert.deepEqual(grid.padding, ["10px", "20px", "0px", "20px"]);
  assert.equal(grid.overflow, "hidden");

  const columnHeadings = await panel.locator("[data-tm-board] section header span:first-child")
    .allInnerTexts();
  assert.deepEqual(
    columnHeadings.map((t) => t.trim().toLowerCase()),
    ["to do", "in progress", "done"],
    "the three status columns, in the design's order",
  );
  const columnBoxes = await panel.locator("[data-tm-board] section").evaluateAll((sections) =>
    sections.map((el) => {
      const s = getComputedStyle(el);
      const stack = el.querySelector("div");
      return {
        background: s.backgroundColor,
        border: s.borderTopWidth + s.borderLeftWidth,
        radius: s.borderTopLeftRadius,
        stackOverflow: getComputedStyle(stack).overflowY,
        stackGap: getComputedStyle(stack).rowGap,
        stackPadBottom: getComputedStyle(stack).paddingBottom,
        scrollable: stack.scrollHeight > stack.clientHeight,
      };
    }));
  assert.equal(columnBoxes.length, 3);
  for (const [i, col] of columnBoxes.entries()) {
    assert.equal(col.background, "rgba(0, 0, 0, 0)", `column ${i} must be unboxed (background)`);
    assert.equal(col.border, "0px0px", `column ${i} must be unboxed (border)`);
    assert.equal(col.radius, "0px", `column ${i} must be unboxed (radius)`);
    assert.equal(col.stackOverflow, "auto", `column ${i} scrolls on its own`);
    assert.equal(col.stackGap, "12px", `column ${i} stacks cards 12px apart`);
    assert.equal(col.stackPadBottom, "16px", `column ${i} ends with 16px`);
  }

  // Independent scrolling, measured: scrolling one column leaves the
  // others where they were.
  const scrollers = panel.locator("[data-tm-board] section > div");
  const before = await scrollers.evaluateAll((els) => els.map((e) => e.scrollTop));
  await scrollers.nth(2).evaluate((el) => { el.scrollTop = 40; });
  const after = await scrollers.evaluateAll((els) => els.map((e) => e.scrollTop));
  assert.ok(after[2] > before[2], "the Done column scrolled");
  assert.deepEqual(after.slice(0, 2), before.slice(0, 2), "the other columns did not move");
  await scrollers.nth(2).evaluate((el) => { el.scrollTop = 0; });

  const typography = (locator) => locator.evaluate((el) => {
    const style = getComputedStyle(el);
    return {
      family: style.fontFamily,
      size: style.fontSize,
      weight: style.fontWeight,
      caps: style.fontVariantCaps,
      transform: style.textTransform,
      tracking: style.letterSpacing,
      lineHeight: style.lineHeight,
    };
  });
  const withoutFamily = ({ family: _family, ...values }) => values;
  const desktopProjectsLabel = page.locator("[data-projects-menu]");
  assert.equal(await desktopProjectsLabel.isVisible(), true, "desktop Projects reference is rendered");
  const projectsTypography = await typography(desktopProjectsLabel);
  await assertFontVoice(page, projectsTypography.family, "ui", `Projects is UI voice: ${projectsTypography.family}`);
  assert.deepEqual(
    withoutFamily(projectsTypography),
    PROJECTS_TYPOGRAPHY,
    "Projects retains its established typography",
  );
  const desktopHeadings = await panel.locator("[data-tm-column-title]").all();
  assert.equal(desktopHeadings.length, 3, "desktop renders all three column headings");
  for (const heading of desktopHeadings) {
    assert.deepEqual(await typography(heading), projectsTypography, "desktop column typography matches Projects");
  }
  const columnCount = await panel.locator("[data-tm-board] section header span").nth(1)
    .evaluate((el) => {
      const s = getComputedStyle(el);
      return { text: el.textContent, size: s.fontSize, family: s.fontFamily };
    });
  assert.equal(
    columnCount.text,
    String(TODO_IDS.length),
    "To do holds pending + blocked, across both tags and including subtasks",
  );
  assert.equal(columnCount.size, "12px");
  // The count is interface chrome, not machine output, so it speaks in the
  // UI face - mono is reserved for the machine's own voice.
  await assertFontVoice(page, columnCount.family, "ui", `the count is UI voice: ${columnCount.family}`);

  // ---- card anatomy ----
  const card = panel.locator("[data-tm-task='2']");
  const anatomy = await card.evaluate((el) => {
    const s = getComputedStyle(el);
    const id = el.querySelector("span span");
    const title = el.querySelectorAll(":scope > span")[1];
    const idStyle = getComputedStyle(id);
    const titleStyle = getComputedStyle(title);
    return {
      radius: s.borderTopLeftRadius,
      border: s.borderTopWidth,
      background: s.backgroundColor,
      padding: [s.paddingTop, s.paddingRight, s.paddingBottom, s.paddingLeft],
      gap: s.rowGap,
      id: { text: id.textContent, size: idStyle.fontSize, family: idStyle.fontFamily, background: idStyle.backgroundColor },
      title: { text: title.textContent, size: titleStyle.fontSize, weight: titleStyle.fontWeight, family: titleStyle.fontFamily, line: titleStyle.lineHeight },
    };
  });
  assert.equal(anatomy.radius, "14px");
  // A min-height floor, not a fixed height: this probe card
  // carries its deps and progress rows, so it grows past 132 to fit them
  // rather than slice (handoff §11, §15.11).
  assert.ok(
    Number.parseFloat(await card.evaluate((el) => getComputedStyle(el).height)) >= 131.5,
    "the card clears the 132px floor",
  );
  assert.equal(anatomy.border, "1px");
  assert.deepEqual(anatomy.padding, ["12px", "14px", "12px", "14px"]);
  assert.equal(anatomy.gap, "8px");
  assert.equal(anatomy.id.text, "2");
  assert.equal(anatomy.id.size, "12px", "card ids are the one 12px metadata");
  await assertFontVoice(page, anatomy.id.family, "mono", `the id is mono: ${anatomy.id.family}`);
  assert.equal(anatomy.id.background, "rgba(0, 0, 0, 0)", "the id is plain — no chip");
  assert.equal(anatomy.title.text, "Wire the provider descriptor");
  assert.equal(anatomy.title.size, "15px");
  assert.equal(anatomy.title.weight, "500");
  await assertFontVoice(page, anatomy.title.family, "ui", `the title is UI voice: ${anatomy.title.family}`);
  near(Number.parseFloat(anatomy.title.line), 15 * 1.4, "the title's line-height", 0.2);
  const cardMetrics = await panel.locator("[data-tm-card]").evaluateAll((cards) =>
    cards.map((element) => {
      const rect = element.getBoundingClientRect();
      const last = element.children[element.children.length - 1];
      return {
        height: rect.height,
        // The card is the comp's fixed 132px, and a card dense enough to
        // exceed it spills rather than reflowing the column - the comp's
        // own behaviour. The spill is bounded: a fully loaded card runs a
        // couple of pixels past, never a full row.
        lastRowOverhang: last === null ? 0 : last.getBoundingClientRect().bottom - rect.bottom,
      };
    }));
  assert.ok(cardMetrics.every((m) => m.height >= 131.5), `a card fell under the 132px floor: ${cardMetrics.map((m) => m.height).join(", ")}`);
  assert.ok(cardMetrics.every((m) => m.lastRowOverhang < 3), `a card spills more than the bounded couple of pixels: ${cardMetrics.map((m) => Math.round(m.lastRowOverhang * 10) / 10).join(", ")}`);
  // The worst-case card (this fixture's card 42: two-line title + deps + progress
  // together) keeps the comp's fixed 132px and spills within the bounded
  // couple of pixels - the growth model this replaced reflowed the column.
  const worstCase = await panel.locator("[data-tm-task='42']").evaluate((el) => {
    const rect = el.getBoundingClientRect();
    const last = el.children[el.children.length - 1];
    return { height: rect.height, overhang: last.getBoundingClientRect().bottom - rect.bottom };
  });
  assert.ok(Math.abs(worstCase.height - 132) < 0.5, `the worst-case card left the fixed height: ${worstCase.height}`);
  assert.ok(worstCase.overhang < 3, `the worst-case card spills more than the bounded couple of pixels: ${worstCase.overhang}px`);
  // The card mounts with the task id as its title fallback; under load the
  // titled payload lands after the card. Waiting for the card alone raced
  // that swap and read the fallback - the flake this wait guards against - so the
  // probe waits for the real title before measuring anything.
  await panel.locator("[data-tm-task='5']").locator("[data-tm-card-title]")
    .filter({ hasText: LONG_TASK_TITLE }).waitFor();
  const clampedTitle = await panel.locator("[data-tm-task='5']").locator("[data-tm-card-title]").evaluate((el) => {
    const style = getComputedStyle(el);
    return {
      text: el.textContent,
      lines: style.webkitLineClamp,
      height: el.getBoundingClientRect().height,
      clipped: el.scrollHeight > el.clientHeight,
    };
  });
  assert.equal(clampedTitle.text, LONG_TASK_TITLE);
  assert.equal(clampedTitle.lines, "2");
  near(clampedTitle.height, 42, "the board title's two-line slot");
  assert.equal(clampedTitle.clipped, true, "the long title is visually clamped on the board");
  assert.equal(
    await card.getByText(/pending|done|in progress/i).count(),
    0,
    "a card carries no state footer — the column already says it",
  );

  // Priority dots resolve to the handoff's hues, medium amber and not red.
  const hues = await panel.evaluate(() => {
    const named = (id) => {
      const el = document.querySelector(`[data-tm-task="${id}"]`);
      const dot = el.querySelector('span[aria-hidden="true"]');
      const label = dot.nextElementSibling;
      const labelStyle = getComputedStyle(label);
      return {
        color: getComputedStyle(dot).backgroundColor,
        size: [getComputedStyle(dot).width, getComputedStyle(dot).height],
        label: label.textContent,
        labelSize: labelStyle.fontSize,
        labelFamily: labelStyle.fontFamily,
        labelTransform: labelStyle.textTransform,
        labelLineHeight: labelStyle.lineHeight,
        labelWeight: labelStyle.fontWeight,
      };
    };
    return { high: named("2"), medium: named("3"), low: named("4") };
  });
  assert.equal(hues.high.color, "rgb(228, 115, 111)", "high is the error hue");
  assert.equal(hues.medium.color, "rgb(217, 164, 65)", "medium is amber, not red");
  assert.notEqual(hues.low.color, hues.medium.color);
  assert.deepEqual(hues.high.size, ["6px", "6px"]);
  assert.deepEqual(
    [hues.high.label, hues.medium.label, hues.low.label],
    ["high", "medium", "low"],
    "med normalizes to medium",
  );
  assert.equal(hues.high.labelSize, "11px");
  await assertFontVoice(page, hues.high.labelFamily, "ui", `the priority label is UI voice: ${hues.high.labelFamily}`);
  assert.equal(hues.high.labelTransform, "uppercase", "the priority label is uppercased in CSS, not in the copy");
  assert.equal(hues.high.labelLineHeight, "normal", "the priority label sits on a normal line box");
  assert.equal(hues.high.labelWeight, "500");

  // Dependencies: one ellipsized UI line; the ids stay mono.
  const deps = await panel.locator("[data-tm-task='3']").getByText(/^depends on /).evaluate((el) => {
    const s = getComputedStyle(el);
    return { text: el.textContent, size: s.fontSize, family: s.fontFamily, wrap: s.whiteSpace, overflow: s.textOverflow };
  });
  assert.equal(deps.text, "depends on 1, 2");
  assert.equal(deps.size, "12px");
  await assertFontVoice(page, deps.family, "ui", `the dependency sentence is UI voice: ${deps.family}`);
  assert.equal(deps.wrap, "nowrap");
  assert.equal(deps.overflow, "ellipsis");

  // Progress: the 3px track, its accent fill, and the mono count.
  const progress = await card.evaluate((el) => {
    const track = el.querySelector('[class*="h-[3px]"]');
    const fill = track.firstElementChild;
    const label = track.nextElementSibling;
    return {
      height: getComputedStyle(track).height,
      fillRatio: fill.getBoundingClientRect().width / track.getBoundingClientRect().width,
      fillColor: getComputedStyle(fill).backgroundColor,
      label: label.textContent,
      labelFamily: getComputedStyle(label).fontFamily,
    };
  });
  assert.equal(progress.height, "3px");
  near(progress.fillRatio, 0.25, "one of four subtasks done", 0.02);
  assert.equal(progress.label, "1/4");
  // Interface chrome speaks in the UI face; mono is the machine's voice.
  await assertFontVoice(page, progress.labelFamily, "ui");
  assert.equal(
    await panel.locator("[data-tm-task='4']").locator("[class*='h-[3px]']").count(),
    0,
    "a task that is not in progress carries no track",
  );

  // ---- the terminal stays live beside the board ----
  const beforeInput = socketFrames.length;
  await page.locator("[data-live-terminal] .xterm-helper-textarea").focus();
  await page.keyboard.type("z");
  await waitFor(
    () => socketFrames.slice(beforeInput).some((f) =>
      f.type === "input" && Buffer.from(f.data, "base64").toString() === "z"),
    "the terminal accepts input while the Tasks panel is open",
  );
  // A click on the terminal is NOT a dismissal: this is a docked working
  // surface, not a board summoned over the user's work.
  await page.locator('[data-region="terminal"]').click();
  await page.waitForTimeout(50);
  assert.equal(await panel.isVisible(), true, "clicking the terminal must not close the board");

  // ---- search, sort, filters, detail ----
  await search.fill("provider");
  await waitFor(async () => (await panel.locator("[data-tm-task]").count()) === 1,
    "search narrows the board");
  assert.equal(await panel.locator("[data-tm-task='2']").count(), 1);
  assert.equal(
    await panel.locator("[data-tm-board] section header span").nth(1).innerText(),
    `0/${TODO_IDS.length}`,
    "a filtered column says how much it is hiding",
  );
  assert.equal(await search.evaluate((el) => el === document.activeElement), true,
    "typing never costs the field its focus");
  await search.fill("");
  await waitFor(async () => (await panel.locator("[data-tm-task]").count()) === BASE_COUNT,
    "clearing search restores the board");

  const idsInColumn = async (n) =>
    panel.locator("[data-tm-board] section").nth(n).locator("[data-tm-task]")
      .evaluateAll((els) => els.map((e) => e.dataset.tmTask));
  assert.deepEqual(await idsInColumn(0), TODO_IDS, "To do sorted by id");
  await panel.getByRole("button", { name: "Priority" }).click();
  assert.deepEqual(
    await idsInColumn(0),
    ["9", "3", "4", "2.2", "2.3", "2.4", "5"],
    "priority: high, medium, low, then the unprioritized by id",
  );
  await panel.getByRole("button", { name: "ID", exact: true }).click();
  assert.deepEqual(await idsInColumn(0), TODO_IDS, "and back to id order");

  await panel.getByRole("button", { name: "Filter" }).click();
  await panel.getByRole("checkbox", { name: "Low" }).uncheck();
  await waitFor(async () => (await panel.locator("[data-tm-task='4']").count()) === 0,
    "the low-priority card is filtered out");
  await panel.getByRole("checkbox", { name: "Low" }).check();
  await panel.getByRole("button", { name: "Filter" }).click();

  // The card is a <button>, so its dependency ids are inert text — no
  // nested focusable reference. Interaction lives in the detail dialog.
  const cardDeps = panel.locator("[data-tm-card-dependencies]");
  assert.ok(await cardDeps.count() >= 2, "cards render their dependency line");
  assert.equal(
    await panel.locator("[data-tm-card-dependencies] [data-task-ref]").count(),
    0,
    "card dependency ids are inert text, never tabbable references",
  );
  assert.equal(
    await panel.locator("[data-tm-card] [tabindex]").count(),
    0,
    "the card button holds no nested focus stop",
  );
  await panel.locator("[data-tm-task='2']").click();
  const dialog = modals.getByRole("dialog", { name: "Task 2" });
  await dialog.waitFor();
  assert.equal(
    await panel.locator("[role=dialog]").count(),
    0,
    "the detail dialog lives in the global modals host, never inside the Tasks panel",
  );
  // Queried fresh from the document rather than through the dialog handle:
  // a status-poll re-render can replace the dialog node between waitFor and
  // an evaluate on the handle, and the detached node answers closest() with
  // null - a false failure on a slow runner.
  assert.ok(await page.evaluate(() =>
    document.querySelector('[data-region="modals"] [role="dialog"]') !== null),
    "the dialog is a child of the modals host");
  await dialog.getByText("In progress").waitFor();
  await dialog.getByText("Wire the matrix").waitFor();
  const taskReference = dialog.locator('[data-task-dependencies] [data-task-ref="1"]');
  await taskReference.hover();
  await taskReference.focus();
  tasks = { ...tasks, master: { ...tasks.master, tasks: tasks.master.tasks.map(task => task.id === 4 ? { ...task, title: "Refresh while task reference has focus" } : task) } };
  await waitFor(async () => (await panel.locator('[data-tm-task="4"] [data-tm-card-title]').textContent()) === "Refresh while task reference has focus", "poll updates a detail dialog with a focused task reference", 15000);
  assert.equal(await taskReference.evaluate(element => element === document.activeElement), true, "poll preserves focus on a task reference");
  const focusLink = dialog.getByRole("link", { name: "Focus documentation" });
  await focusLink.focus();
  tasks = { ...tasks, master: { ...tasks.master, tasks: tasks.master.tasks.map(task => task.id === 4 ? { ...task, title: "Refresh while markdown link has focus" } : task) } };
  await waitFor(async () => (await panel.locator('[data-tm-task="4"] [data-tm-card-title]').textContent()) === "Refresh while markdown link has focus", "poll updates a detail dialog with a focused markdown link", 15000);
  assert.equal(await focusLink.evaluate(element => element === document.activeElement), true, "poll preserves focus on a markdown link");
  await dialog.getByRole("button", { name: "Close task" }).focus();
  await page.mouse.move(0, 0);
  await page.waitForTimeout(400);
  assert.equal(await page.locator("[data-task-ref-popover]").count(), 0);
  assert.equal(await taskReference.getAttribute("aria-describedby"), null);
  await taskReference.hover();
  const taskTooltip = page.locator("[data-task-ref-popover]");
  await taskTooltip.waitFor();
  await taskTooltip.getByText("Initialize the workspace frame").waitFor();
  assert.equal(await page.locator("[data-task-ref-popover]").count(), 1, "one bounded preview is open");
  const dependencyTwo = taskTooltip.locator('[data-task-ref="2"]');
  assert.equal(await dependencyTwo.getAttribute("aria-label"), "Task 2");
  await dependencyTwo.focus();
  await taskTooltip.getByText("Wire the provider descriptor").waitFor();
  assert.equal(await page.locator("[data-task-ref-popover]").count(), 1, "dependency focus replaces rather than nests");
  assert.equal(await taskTooltip.locator('[data-task-ref="1"]').count(), 1, "the cycle remains one bounded next hop");
  assert.equal(await taskTooltip.locator('[data-task-ref="999"]').count(), 0, "unknown dependencies stay inert");
  assert.match(await taskTooltip.innerText(), /999/, "the unknown dependency remains visible");
  await taskReference.focus();
  await taskTooltip.getByText("Initialize the workspace frame").waitFor();
  const previewColors = (locator) => locator.evaluate((el) => {
    const card = getComputedStyle(el);
    const primary = getComputedStyle(el.querySelector(".text-text-primary"));
    const metadata = getComputedStyle(el.querySelector(".text-text-tertiary"));
    return {
      // data-theme rides on the workspace/`<html>` scope (main.ts), never on
      // the popover, so the resolved ramp is read from the nearest scope.
      theme: el.closest("[data-theme]")?.getAttribute("data-theme") ?? null,
      surface: card.backgroundColor,
      border: card.borderColor,
      primary: primary.color,
      metadata: metadata.color,
    };
  });
  const semanticPreviewColors = (theme) => page.evaluate((activeTheme) => {
    const probe = document.createElement("div");
    probe.setAttribute("data-theme", activeTheme);
    probe.className = "border border-border-strong bg-surface-card";
    probe.innerHTML = '<span class="text-text-primary">primary</span><span class="text-text-tertiary">metadata</span>';
    document.body.append(probe);
    const card = getComputedStyle(probe);
    const primary = getComputedStyle(probe.children[0]);
    const metadata = getComputedStyle(probe.children[1]);
    const colors = {
      theme: activeTheme,
      surface: card.backgroundColor,
      border: card.borderColor,
      primary: primary.color,
      metadata: metadata.color,
    };
    probe.remove();
    return colors;
  }, theme);
  const darkPreview = await previewColors(taskTooltip);
  assert.deepEqual(darkPreview, await semanticPreviewColors("dark"), "dark preview resolves semantic card tokens");
  assert.equal(await dialog.locator('[data-task-ref="999"]').count(), 0);
  assert.match(await dialog.innerText(), /Task 999/);
  await taskReference.focus();
  assert.equal(
    await taskReference.getAttribute("aria-describedby"),
    await taskTooltip.getAttribute("id"),
  );
  await page.setViewportSize({ width: 1390, height: 890 });
  await taskTooltip.waitFor({ state: "detached" });
  assert.equal(await taskReference.getAttribute("aria-describedby"), null);
  await page.setViewportSize({ width: 1400, height: 900 });
  // data-theme rides on <html> (main.ts), so a ramp flip belongs there,
  // not on the workspace container (handoff §2).
  await page.evaluate(() => document.documentElement.setAttribute("data-theme", "light"));
  await page.mouse.move(0, 0);
  await page.waitForTimeout(400);
  await taskReference.hover();
  await taskTooltip.waitFor();
  const lightPreview = await previewColors(taskTooltip);
  assert.deepEqual(lightPreview, await semanticPreviewColors("light"), "light fixture resolves semantic card tokens");
  assert.notDeepEqual(lightPreview, darkPreview, "light and dark previews resolve to distinct palettes");
  await page.evaluate(() => document.documentElement.setAttribute("data-theme", "dark"));
  await page.setViewportSize({ width: 1390, height: 890 });
  await taskTooltip.waitFor({ state: "detached" });
  await page.setViewportSize({ width: 1400, height: 900 });
  await dialog.getByRole("button", { name: "Close task" }).click();
  assert.equal(await dialog.count(), 0);

  // ---- Add PRD: upload and generation ----
  await panel.getByRole("button", { name: "Add PRD" }).click();
  const prdDialog = modals.getByRole("dialog", { name: "Add PRD" });
  await prdDialog.waitFor();
  const prdBody = await prdDialog.innerText();
  assert.match(prdBody, /saved as prd\.md/);
  assert.equal(/\/workspace\//.test(prdBody), false, "the modal claims no root path");
  assert.equal(await prdDialog.locator("input[type=text]").count(), 0, "no filename field");
  await prdDialog.locator("[data-tm-prd-file]").setInputFiles({
    name: "spec.md",
    mimeType: "text/markdown",
    buffer: Buffer.from("# Spec\n\nOne requirement.\n"),
  });
  await prdDialog.getByRole("button", { name: "Add PRD" }).click();
  await waitFor(async () => (await prdDialog.count()) === 0, "the modal closes once the parse starts");
  assert.equal(uploads.length, 1, "one upload, under the canonical name");
  assert.equal(uploads[0].name, "prd.md", "the picked file's own name is discarded");
  assert.equal(uploads[0].content, "# Spec\n\nOne requirement.\n");
  assert.deepEqual(generations, [{ prd: "prd.md", harness: "claude" }]);
  await panel.getByRole("button", { name: /Parsed from the PRD/ }).waitFor();
  await waitFor(async () => (await panel.locator("[data-tm-task]").count()) === GENERATED_COUNT,
    "the board follows the generated tasks");

  // With no session to name an agent, the request leaves harness out.
  projectHasSessions = false;
  await panel.getByRole("button", { name: "Add PRD" }).click();
  await prdDialog.waitFor();
  await prdDialog.locator("[data-tm-prd-file]").setInputFiles({
    name: "spec.md",
    mimeType: "text/markdown",
    buffer: Buffer.from("# Spec\n\nOne requirement.\n"),
  });
  await prdDialog.getByRole("button", { name: "Add PRD" }).click();
  await prdDialog.getByRole("button", { name: "Replace prd.md" }).click();
  await waitFor(async () => generations.length === 2, "the second generation starts");
  assert.deepEqual(generations[1], { prd: "prd.md" });
  projectHasSessions = true;
  await waitFor(async () => (await prdDialog.count()) === 0, "the modal closes once the second parse starts");
  assert.equal(socketOpens, 1, "the terminal was never rebuilt or reconnected");

  // Session replacement closes the open dock and attaches the new session
  // at the completed layout. The dock repaints in place (no motion), and
  // the geometry rides the replacement's connect URL, so a settled
  // replacement sends no resize frame at all.
  const geometryOf = (url) => {
    const params = new URL(url).searchParams;
    return { cols: Number(params.get("cols")), rows: Number(params.get("rows")) };
  };
  const beforeReplacementResizes = resizeCount();
  await page.locator(`[data-session="${replacementSession.id}"]`).click();
  await waitForPanelState(panel, false, "the Tasks dock closes for session replacement");
  await waitFor(() => socketOpens === 2, "the replacement attachment opens its socket");
  assert.deepEqual(
    geometryOf(connectUrls[1]),
    geometryOf(connectUrls[0]),
    "the replacement attaches at the completed full-width layout",
  );
  await page.waitForTimeout(120);
  assert.equal(
    socketEvents.filter(({ connection, frame }) => connection === 2 && frame.type === "resize").length,
    0,
    "the replacement renegotiates no geometry - the connect URL carried it",
  );
  assert.equal(resizeCount(), beforeReplacementResizes);
  const beforeReplacementReopenResizes = resizeCount();
  await tasksButton.click();
  await waitForPanelState(panel, true, "Tasks reopens after session replacement");
  await waitFor(
    () => resizeCount() === beforeReplacementReopenResizes + 1,
    "the Tasks reopen after session replacement reports its final geometry",
  );

  // ---- mutual exclusion, both directions ----
  const beforeFilesResizes = resizeCount();
  await filesButton.click();
  await waitForPanelState(filesPanel, true, "Files finishes replacing Tasks");
  await waitForPanelState(panel, false, "Tasks finishes sliding out for Files");
  await waitFor(
    () => resizeCount() === beforeFilesResizes + 1,
    "the two-surface switch reports one final terminal resize",
  );
  assert.equal(await panel.isVisible(), false, "opening Files closes Tasks");
  assert.equal(await tasksButton.getAttribute("aria-pressed"), "false");
  await tasksButton.click();
  await waitForPanelState(panel, true, "Tasks finishes replacing Files");
  await waitForPanelState(filesPanel, false, "Files finishes sliding out for Tasks");
  assert.equal(await filesPanel.isVisible(), false, "opening Tasks closes Files");
  assert.equal(await filesButton.getAttribute("aria-pressed"), "false");
  assert.equal(
    await page.evaluate(() =>
      [...document.querySelectorAll('[data-region="files"],[data-region="tasks"]')]
        .filter((el) => el.getAttribute("data-open") === "true").length),
    1,
    "exactly one panel is ever laid out",
  );

  // Reopening re-reads the canonical files rather than trusting the last
  // answer.
  await closeButton.click();
  await waitForPanelState(panel, false, "the Tasks close motion finishes");
  assert.equal(await panel.isVisible(), false, "the panel's own ✕ closes it");
  assert.equal(await tasksButton.getAttribute("aria-pressed"), "false");
  tasks = baseTasks;
  const beforeReopenResizes = resizeCount();
  await tasksButton.click();
  await waitForPanelState(panel, true, "Tasks finishes reopening");
  await waitFor(
    () => resizeCount() === beforeReopenResizes + 1,
    "the completed Tasks reopen reports one final terminal resize",
  );
  await waitFor(async () => (await panel.locator("[data-tm-task]").count()) === BASE_COUNT,
    "opening a ready panel re-reads canonical disk state");

  // Explicit task creation preserves hostile-looking text as data, posts
  // once, and closes only after the canonical refresh contains its id.
  await panel.getByRole("button", { name: "Add task" }).click();
  const addTaskDialog = modals.getByRole("dialog", { name: "Add task" });
  await addTaskDialog.waitFor();
  assert.equal(await addTaskDialog.getByLabel("Title").evaluate((el) => el === document.activeElement), true,
    "task title receives initial focus");
  await addTaskDialog.getByLabel("Title").fill("Unsaved focus regression");
  await addTaskDialog.getByRole("button", { name: "Cancel", exact: true }).focus();
  tasks = { ...baseTasks, master: { ...baseTasks.master, tasks: baseTasks.master.tasks.map(task => task.id === 1 ? { ...task, title: "Refresh while Cancel has focus" } : task) } };
  await waitFor(async () => (await panel.locator('[data-tm-task="1"] [data-tm-card-title]').textContent()) === "Refresh while Cancel has focus", "canonical poll redraws the board with an open dialog", 15000);
  assert.equal(await addTaskDialog.getByRole("button", { name: "Cancel", exact: true }).evaluate(element => document.activeElement === element), true, "poll preserves Cancel focus");
  assert.equal(await addTaskDialog.getByLabel("Title").inputValue(), "Unsaved focus regression", "poll preserves edited fields alongside button focus");
  await page.keyboard.press("Enter");
  await addTaskDialog.waitFor({ state: "detached" });
  assert.equal(addedTasks.length, 0, "Enter on Cancel never creates a task");
  tasks = baseTasks;
  await panel.getByRole("button", { name: "Add task" }).click();
  await addTaskDialog.waitFor();
  await addTaskDialog.getByLabel("Title").fill("Submit focus regression");
  const addTaskSubmit = addTaskDialog.locator("[data-tm-task-add-submit]");
  await addTaskSubmit.focus();
  tasks = { ...baseTasks, master: { ...baseTasks.master, tasks: baseTasks.master.tasks.map(task => task.id === 1 ? { ...task, title: "Refresh while submit has focus" } : task) } };
  await waitFor(async () => (await panel.locator('[data-tm-task="1"] [data-tm-card-title]').textContent()) === "Refresh while submit has focus", "canonical poll redraws the board with Add task submit focused", 15000);
  assert.equal(await addTaskSubmit.evaluate(element => document.activeElement === element), true, "poll preserves Add task submit focus");
  assert.equal(await addTaskDialog.getByLabel("Title").inputValue(), "Submit focus regression", "poll preserves the title alongside submit focus");
  await page.keyboard.press("Enter");
  await addTaskDialog.waitFor({ state: "detached" });
  assert.equal(addedTasks.length, 1, "Enter after redraw issues one task mutation");
  assert.deepEqual(addedTasks[0], {
    title: "Submit focus regression",
    description: "",
    details: "",
    priority: "medium",
  });

  await panel.getByRole("button", { name: "Add task" }).click();
  await addTaskDialog.waitFor();
  await addTaskDialog.getByLabel("Title").fill("Literal; $(do-not-run)");
  await addTaskDialog.getByLabel("Description").fill("Description <b>stays text</b>");
  await addTaskDialog.getByLabel("Details").fill("Details && untouched");
  await addTaskDialog.getByLabel("Priority").selectOption("high");
  await addTaskDialog.getByRole("button", { name: "Add task" }).dblclick();
  await addTaskDialog.waitFor({ state: "detached" });
  assert.equal(addedTasks.length, 2, "rapid activation adds only one task mutation");
  assert.deepEqual(addedTasks[1], {
    title: "Literal; $(do-not-run)",
    description: "Description <b>stays text</b>",
    details: "Details && untouched",
    priority: "high",
  });
  await panel.getByRole("button", { name: /Literal; \$\(do-not-run\)/ }).waitFor();
  await waitFor(async () => (await panel.locator("[data-tm-task]").count()) === BASE_COUNT + 2,
    "the board follows the canonical task refresh");

  // With no dock motion, a close reversed 40ms later is two real repaints,
  // and each reports the geometry it produced - there is no hold to hide
  // inside. What must still be true: the reversal settles open, ends at the
  // pre-gesture geometry, and the churn is exactly the two gestures' frames,
  // with nothing further once settled (attach.ts dedupes an unchanged fit
  // against lastReportedGeometry).
  const beforeReversalResizes = resizeCount();
  const reversalGeometry = `${await page.evaluate(() => {
    const rect = document.querySelector("[data-live-terminal]").getBoundingClientRect();
    return Math.round(rect.width);
  })}`;
  await tasksButton.evaluate((button) => {
    button.click();
    window.setTimeout(() => button.click(), 40);
  });
  await waitForPanelState(panel, true, "the reversed Tasks toggle settles open");
  await page.waitForTimeout(250);
  assert.ok(
    resizeCount() - beforeReversalResizes <= 2,
    `rapid reversal churned more than its two gestures: ${resizeCount() - beforeReversalResizes}`,
  );
  const settledReversalGeometry = `${await page.evaluate(() => {
    const rect = document.querySelector("[data-live-terminal]").getBoundingClientRect();
    return Math.round(rect.width);
  })}`;
  assert.equal(settledReversalGeometry, reversalGeometry,
    "rapid reversal returns the terminal to its pre-gesture geometry");
  const settledCount = resizeCount();
  await page.waitForTimeout(150);
  assert.equal(resizeCount(), settledCount, "a settled reversal emits nothing further");

  await page.emulateMedia({ reducedMotion: "reduce" });
  // The docked panels have no transition, so reduced-motion cannot be
  // distinguished by duration; the remaining assertions verify the geometry is
  // reported exactly once in each direction.
  const beforeReducedResizes = resizeCount();
  await tasksButton.click();
  await waitForPanelState(panel, false, "reduced-motion close settles without animation");
  await waitFor(
    () => resizeCount() === beforeReducedResizes + 1,
    "reduced-motion close still reports final geometry",
  );
  await tasksButton.click();
  await waitForPanelState(panel, true, "reduced-motion open settles without animation");
  await waitFor(
    () => resizeCount() === beforeReducedResizes + 2,
    "reduced-motion open reports final geometry once",
  );
  await page.emulateMedia({ reducedMotion: "no-preference" });

  await setViewport({ width: 700, height: 900 }, "the overlay sidebar width");
  const narrowSidebarOpenBox = await box(page.locator('[data-region="sidebar"]'));
  const beforeNarrowSidebarResizes = resizeCount();
  await page.getByRole("button", { name: "Collapse sidebar" }).click();
  await page.waitForTimeout(60);
  const narrowSidebarMidBox = await box(page.locator('[data-region="sidebar"]'));
  await waitForSidebarState(true, "the narrow sidebar finishes sliding closed");
  const narrowSidebarClosedBox = await box(page.locator('[data-region="sidebar"]'));
  near(Math.abs(narrowSidebarClosedBox.x - narrowSidebarOpenBox.x), narrowSidebarOpenBox.width,
    "the narrow sidebar traverses exactly one width");
  near(narrowSidebarMidBox.x, narrowSidebarClosedBox.x,
    "the narrow sidebar is already collapsed at the first measured frame");
  // "No resize happened" reaches exactly as far as the window that watches
  // for one; a frame arriving after it is invisible here.
  await exactlyOnce([{
    read: resizeCount,
    expected: beforeNarrowSidebarResizes,
    message: "the overlay sidebar does not resize the terminal",
  }]);
  const mobileContentBox = await box(content);
  const mobilePanelBox = await box(panel);
  near(mobilePanelBox.width, mobileContentBox.width, "the narrow panel keeps its full-width behavior");
  assert.equal(
    (await panel.evaluate((element) => getComputedStyle(element).transitionDuration))
      .split(", ").slice(0, 2).every((value) => value === "0s"),
    true,
    "the narrow panel has no slide transition",
  );
  const beforeNarrowResizes = resizeCount();
  const narrowTasksOpenBox = await box(panel);
  await closeButton.click();
  await page.waitForTimeout(60);
  const narrowTasksClosingBox = await box(panel);
  await waitForPanelState(panel, false, "the narrow Tasks panel closes over one width");
  const narrowTasksClosedBox = await box(panel);
  near(Math.abs(narrowTasksClosedBox.x - narrowTasksOpenBox.x), narrowTasksOpenBox.width,
    "the narrow Tasks close traverses exactly one width");
  near(narrowTasksClosingBox.x, narrowTasksClosedBox.x,
    "the narrow Tasks panel is already closed at the first measured frame");
  await waitFor(
    () => resizeCount() === beforeNarrowResizes + 1,
    "the narrow close reports final geometry once",
  );
  await page.getByRole("button", { name: "Panels", exact: true }).click();
  await page.getByRole("menuitem", { name: "Tasks", exact: true }).click();
  await page.waitForTimeout(60);
  const narrowTasksOpeningBox = await box(panel);
  await waitForPanelState(panel, true, "the narrow Tasks panel opens over one width");
  const narrowTasksReopenedBox = await box(panel);
  near(Math.abs(narrowTasksClosedBox.x - narrowTasksReopenedBox.x), narrowTasksReopenedBox.width,
    "the narrow Tasks open traverses exactly one width");
  near(narrowTasksOpeningBox.x, narrowTasksReopenedBox.x,
    "the narrow Tasks panel is already open at the first measured frame");
  await waitFor(
    () => resizeCount() === beforeNarrowResizes + 2,
    "the narrow open reports final geometry once",
  );

  const cdp = await context.newCDPSession(page);
  await cdp.send("Emulation.setTouchEmulationEnabled", { enabled: true, maxTouchPoints: 1 });
  await waitFor(
    () => page.evaluate(() => matchMedia("(pointer: coarse)").matches),
    "mobile probe enables a coarse primary pointer",
  );
  for (const width of [430, 390, 320]) {
    await page.setViewportSize({ width, height: 900 });
    await page.waitForTimeout(50);
    assert.equal(
      await page.evaluate(() => matchMedia("(pointer: coarse)").matches),
      true,
      `${width}px uses the coarse-pointer project menu geometry`,
    );
    const panelsButton = page.getByRole("button", { name: "Panels", exact: true });
    await panelsButton.waitFor({ state: "visible" });
    assert.equal(await tasksButton.isVisible(), false, `${width}px direct Tasks is collapsed`);
    assert.equal(await filesButton.isVisible(), false, `${width}px direct Files is collapsed`);
    const targetBox = await box(panelsButton);
    assert.ok(targetBox.width >= 44 && targetBox.height >= 44,
      `${width}px Panels keeps a 44px target`);
    const mobileTopBarBox = await box(topBar);
    const mobileTopBarBorder = Number.parseFloat(
      await topBar.evaluate((element) => getComputedStyle(element).borderBottomWidth),
    );
    near(mobileTopBarBox.height, MOBILE_TITLEBAR_H, `${width}px workspace titlebar height`);
    near(targetBox.y - mobileTopBarBox.y, 5.5, `${width}px Panels top breathing room`);
    near(
      mobileTopBarBox.y + mobileTopBarBox.height - mobileTopBarBorder - targetBox.y - targetBox.height,
      5.5,
      `${width}px Panels bottom breathing room`,
    );
    // The phone trigger is a bare icon: no count badge on any surface.
    assert.equal(await tasksButton.locator("[data-live-tasks-count]").count(), 0,
      `${width}px Tasks renders a count badge`);
    const viewport = await page.evaluate(() => ({
      width: innerWidth,
      root: document.documentElement.scrollWidth,
      body: document.body.scrollWidth,
    }));
    assert.equal(viewport.root, viewport.width, `${width}px root does not overflow`);
    assert.equal(viewport.body, viewport.width, `${width}px body does not overflow`);
    const mobileGrid = await boardGrid.evaluate((el) => {
      const style = getComputedStyle(el);
      return {
        overflowX: style.overflowX,
        scrollWidth: el.scrollWidth,
        clientWidth: el.clientWidth,
        columns: style.gridTemplateColumns.split(" ").filter(Boolean),
      };
    });
    assert.equal(mobileGrid.overflowX, "hidden", `${width}px board never scrolls horizontally`);
    assert.equal(mobileGrid.columns.length, 1, `${width}px lays out one full-width lane`);
    assert.ok(
      mobileGrid.scrollWidth <= mobileGrid.clientWidth + 1,
      `${width}px board overflows horizontally: ${mobileGrid.scrollWidth} > ${mobileGrid.clientWidth}`,
    );
    const mobileSections = panel.locator("[data-tm-board] section");
    const sectionVisible = await mobileSections.evaluateAll((els) =>
      els.map((el) => getComputedStyle(el).display !== "none"));
    assert.equal(sectionVisible.filter(Boolean).length, 1, `${width}px shows exactly one lane`);
    const statusGroup = panel.locator("[data-tm-status-selector]");
    assert.equal(await statusGroup.isVisible(), true, `${width}px renders the status selector`);
    assert.equal(await statusGroup.getAttribute("role"), "group", `${width}px selector is a group`);
    assert.equal(await statusGroup.getAttribute("aria-label"), "Task status", `${width}px selector is named`);
    const statusButtons = panel.locator("[data-tm-status-selector] [data-tm-status]");
    assert.equal(await statusButtons.count(), 3, `${width}px selector offers the three statuses`);
    const mobileStatusTypography = await Promise.all((await statusButtons.all()).map(typography));
    assert.deepEqual(
      await statusButtons.evaluateAll((els) => els.map((el) => el.getAttribute("aria-pressed"))),
      ["true", "false", "false"],
      `${width}px To do is the initial lane`,
    );
    for (const btn of await statusButtons.all()) {
      const targetBox = await box(btn);
      assert.ok(
        targetBox.width >= 44 && targetBox.height >= 44,
        `${width}px status target is ${targetBox.width}×${targetBox.height}, below 44×44`,
      );
    }

    for (const [name, target] of [
      ["Add task", panel.getByRole("button", { name: "Add task" })],
      ["Add PRD", panel.getByRole("button", { name: "Add PRD" })],
      ["Close tasks", closeButton],
      ["Search tasks", searchField],
      ["Filter", panel.getByRole("button", { name: "Filter" })],
    ]) {
      const targetBox = await box(target);
      assert.ok(
        targetBox.width >= 44 && targetBox.height >= 44,
        `${width}px ${name} target is ${targetBox.width}×${targetBox.height}, below 44×44`,
      );
    }
    const mobilePanelHeaderBox = await box(panel.locator("[data-tm-header]"));
    const mobilePanelHeaderBorder = Number.parseFloat(
      await panel.locator("[data-tm-header]").evaluate((element) => getComputedStyle(element).borderBottomWidth),
    );
    const addPrdBox = await box(panel.locator('[data-tm-add-prd="header"]'));
    // The phone header is the comp's (mobile-r8 screen 06): 8px above and
    // 10px below its 44px controls, and no hairline — the tab strip below
    // is the seam.
    near(mobilePanelHeaderBox.height, 62, `${width}px Tasks header height`);
    assert.equal(mobilePanelHeaderBorder, 0, `${width}px Tasks header keeps a hairline the comp dropped`);
    near(addPrdBox.y - mobilePanelHeaderBox.y, 8, `${width}px Add PRD top breathing room`);
    near(
      mobilePanelHeaderBox.y + mobilePanelHeaderBox.height - addPrdBox.y - addPrdBox.height,
      10,
      `${width}px Add PRD bottom breathing room`,
    );
    // Add task collapses to its icon: the label is gone, the name is not.
    const addTaskHeader = panel.locator('[data-tm-add-task="header"]');
    assert.equal(await addTaskHeader.locator("[data-tm-add-task-label]").isVisible(), false,
      `${width}px Add task keeps its desktop label`);
    // Sorting by status is desktop chrome: the segments hold one status
    // apiece, so the pill would reorder nothing the phone shows.
    assert.equal(await panel.locator('[data-tm-sort="status"]').isVisible(), false,
      `${width}px still offers the status sort`);
    for (const sortButton of await panel.locator("[data-tm-sort]").all()) {
      if (!(await sortButton.isVisible())) continue;
      const targetBox = await box(sortButton);
      assert.ok(
        targetBox.width >= 44 && targetBox.height >= 44,
        `${width}px sort target is ${targetBox.width}×${targetBox.height}, below 44×44`,
      );
    }

    const mobileHeadings = await panel.locator("[data-tm-column-title]").all();
    assert.equal(mobileHeadings.length, 3, `${width}px renders all three column headings`);
    const mobileHeadingTypography = await Promise.all(mobileHeadings.map(typography));
    await closeButton.click();
    await waitForPanelState(panel, false, `${width}px Tasks closes for the reference measurement`);
    // On a phone the session list opens from the titlebar's Sessions button,
    // not the desktop sidebar expand affordance.
    await page.getByRole("button", { name: "Sessions", exact: true }).click();
    await waitForSidebarState(false, `${width}px reference sidebar opens`);
    // The phone sheet titles itself "Sessions" (comp mobile-r8 screen 02);
    // the PROJECTS eyebrow leaves with the desktop drawer. Its voice is
    // title-3 medium in the UI face.
    const sheetTitle = page.locator("[data-sidebar-sheet-chrome] span.flex-1");
    assert.equal(await sheetTitle.isVisible(), true, `${width}px Sessions sheet title is rendered`);
    assert.equal((await sheetTitle.innerText()).trim(), "Sessions", `${width}px sheet title copy`);
    assert.equal(await page.locator("[data-projects-menu]").isVisible(), false,
      `${width}px still renders the desktop PROJECTS eyebrow`);
    await assertLongProjectName(`${width}px sidebar`);
    const mobileTitleTypography = await typography(sheetTitle);
    await assertFontVoice(page, mobileTitleTypography.family, "ui", `${width}px sheet title is UI voice`);
    // title-3 is the FIXED 17px token: the scope-following var reads the
    // laptop scope's 16px when the phone frame sits inside a wide page.
    assert.equal(mobileTitleTypography.size, "17px", `${width}px sheet title is title-3`);
    assert.equal(mobileTitleTypography.weight, "500", `${width}px sheet title is medium`);
    // The eyebrow voice the columns share is pinned by the constant now that
    // the phone sheet no longer renders the PROJECTS label to compare with.
    for (const headingTypography of mobileHeadingTypography) {
      await assertFontVoice(page, headingTypography.family, "ui", `${width}px column heading is UI voice`);
      assert.deepEqual(
        withoutFamily(headingTypography),
        PROJECTS_TYPOGRAPHY,
        `${width}px column typography matches the eyebrow voice`,
      );
    }
    // The segments use the phone body-small scale: a
    // sentence-case 15px name, a dimmed interpunct, and a tabular count —
    // the uppercase eyebrow voice left with the desktop columns.
    for (const statusTypography of mobileStatusTypography) {
      await assertFontVoice(page, statusTypography.family, "ui", `${width}px status selector is UI voice`);
      assert.equal(statusTypography.transform, "none",
        `${width}px status selector still shouts uppercase`);
    }
    const segmentNames = panel.locator("[data-tm-status-selector] button > span > span:first-child");
    for (const nameSpan of await segmentNames.all()) {
      const nameTypography = await typography(nameSpan);
      assert.equal(nameTypography.size, "15px", `${width}px segment name follows the phone body-small scale`);
      assert.equal(nameTypography.transform, "none", `${width}px segment name is sentence case`);
    }
    const selectedSegment = panel.locator('[data-tm-status-selector] button[aria-pressed="true"]');
    assert.notEqual(
      await selectedSegment.evaluate((el) => getComputedStyle(el).backgroundColor),
      "rgba(0, 0, 0, 0)",
      `${width}px the selected segment lost its wash`,
    );
    // The column eyebrows leave on the phone — the segments already say it.
    for (const heading of mobileHeadings) {
      assert.equal(await heading.isVisible(), false, `${width}px still renders a column eyebrow`);
    }
    // On a phone the sidebar overlay is dismissed by its scrim; there is no
    // desktop Collapse sidebar control. The scrim sits under the sidebar, so
    // dispatch the click directly instead of relying on hit-testing.
    await page.locator('[data-region="sidebar-scrim"]').dispatchEvent("click");
    await waitForSidebarState(true, `${width}px reference sidebar closes`);
    await panelsButton.click();
    await page.getByRole("menuitem", { name: "Tasks", exact: true }).click();
    await waitForPanelState(panel, true, `${width}px Tasks reopens after the reference measurement`);

    // Mobile cards size to content (the uniform height is desktop-only),
    // so each lane's first card height is recorded and the sparse To do
    // card — no deps, no progress — is proven shorter than the dense
    // In-progress card rather than pinned to a fixed number.
    const mobileFirstCardHeights = new Map();
    for (const status of ["To do", "In progress", "Done"]) {
      await statusButtons.filter({ hasText: new RegExp(`^${status}`) }).click();
      await waitFor(
        () => mobileSections.evaluateAll((els) =>
          els.filter((el) => getComputedStyle(el).display !== "none").length === 1),
        `${width}px ${status} becomes the only visible lane`,
      );
      assert.deepEqual(
        await statusButtons.evaluateAll((els) => els.map((el) => el.getAttribute("aria-pressed"))),
        ["To do", "In progress", "Done"].map((s) => String(s === status)),
        `${width}px ${status} is pressed`,
      );
      assert.equal(
        await statusButtons.filter({ hasText: new RegExp(`^${status}`) })
          .evaluate((el) => document.activeElement === el),
        true,
        `${width}px ${status} keeps focus after the lane redraw`,
      );
      const visibleScroller = panel.locator("[data-tm-column-scroll]:visible").first();
      const scrollerBox = await box(visibleScroller);
      const firstCard = visibleScroller.locator("[data-tm-card]").first();
      if (await firstCard.count() > 0) {
        const cardBox = await box(firstCard);
        mobileFirstCardHeights.set(status, cardBox.height);
        assert.ok(
          cardBox.width >= scrollerBox.width - 1,
          `${width}px ${status} card is full-width: ${cardBox.width} of ${scrollerBox.width}`,
        );
        const titleClamp = await firstCard.locator("[data-tm-card-title]").evaluate((el) => {
          const style = getComputedStyle(el);
          return {
            clamp: style.webkitLineClamp,
            scrollWidth: el.scrollWidth,
            clientWidth: el.clientWidth,
          };
        });
        assert.equal(titleClamp.clamp, "2", `${width}px ${status} title clamps to two lines`);
        assert.ok(
          titleClamp.scrollWidth <= titleClamp.clientWidth + 1,
          `${width}px ${status} title overflows horizontally`,
        );
        await visibleScroller.evaluate((el) => { el.scrollTop = el.scrollHeight; });
        const lastCard = visibleScroller.locator("[data-tm-task]").last();
        const [sBounds, cBounds] = await Promise.all([box(visibleScroller), box(lastCard)]);
        assert.ok(
          cBounds.y + cBounds.height <= sBounds.y + sBounds.height + 1,
          `${width}px ${status} reaches its final card`,
        );
      }
    }
    assert.equal(mobileFirstCardHeights.size, 3, `${width}px measured every lane's first card`);
    const sparseTodo = mobileFirstCardHeights.get("To do");
    const denseInProgress = mobileFirstCardHeights.get("In progress");
    assert.ok(
      sparseTodo < denseInProgress,
      `${width}px mobile cards size to content: the deps+progress card is ${denseInProgress}px, the bare card ${sparseTodo}px`,
    );
    const todoScroller = panel.locator('[data-tm-column-scroll="To do"]');
    await statusButtons.filter({ hasText: /^To do/ }).click();
    await waitFor(
      () => todoScroller.evaluate((el) => el.getClientRects().length > 0),
      `${width}px To do lane is visible again`,
    );
    await todoScroller.evaluate((el) => { el.scrollTop = 60; });
    const todoDepth = await todoScroller.evaluate((el) => el.scrollTop);
    await statusButtons.filter({ hasText: /^Done/ }).click();
    assert.equal(
      await todoScroller.evaluate((el) => el.getClientRects().length),
      0,
      `${width}px To do is hidden while Done is active`,
    );
    await statusButtons.filter({ hasText: /^To do/ }).click();
    assert.equal(
      await todoScroller.evaluate((el) => el.scrollTop),
      todoDepth,
      `${width}px To do restores its scroll depth after a lane switch`,
    );
  }

  // The docked panel takes the whole width here, so the terminal is already
  // zero-width, and this step changes neither its cols nor its rows: no
  // resize frame follows, and waiting for one would hang.
  await setViewport({ width: 390, height: 900 }, "the phone board", { reports: false });
  const mobileStatusButtons = panel.locator("[data-tm-status-selector] [data-tm-status]");
  const laneScroller = (status) => panel.locator(`[data-tm-column-scroll="${status}"]`);

  await mobileStatusButtons.filter({ hasText: /^Done/ }).click();
  const doneScroller = laneScroller("Done");
  await waitFor(
    () => doneScroller.evaluate((el) => el.getClientRects().length > 0),
    "390px Done lane is the visible one",
  );
  await doneScroller.evaluate((el) => { el.scrollTop = 80; });
  const doneDepth = await doneScroller.evaluate((el) => el.scrollTop);
  await search.fill("Shipped item 20");
  await waitFor(
    () => doneScroller.evaluate((el) => el.scrollTop === 0),
    "390px a restrictive search clamps the visible Done lane",
  );
  await search.fill("");
  await waitFor(
    () => panel.locator("[data-tm-task]").count().then((n) => n > 0),
    "390px clearing search restores the cards",
  );
  assert.equal(
    await doneScroller.evaluate((el) => el.scrollTop),
    doneDepth,
    "390px clearing search preserves the Done lane scroll",
  );
  await panel.getByRole("button", { name: "Filter" }).evaluate((el) => el.click());
  assert.equal(
    await doneScroller.evaluate((el) => el.scrollTop),
    doneDepth,
    "390px a full toolbar rerender preserves the Done lane scroll",
  );
  await panel.getByRole("button", { name: "Filter" }).evaluate((el) => el.click());

  await setViewport({ width: 390, height: 480 }, "the keyboard-sized viewport");
  const shortViewport = await page.evaluate(() => ({
    height: window.innerHeight,
    rootHeight: document.documentElement.getBoundingClientRect().height,
    bodyHeight: document.body.getBoundingClientRect().height,
  }));
  assert.equal(shortViewport.rootHeight, shortViewport.height, "keyboard-sized viewport keeps the root bounded");
  assert.equal(shortViewport.bodyHeight, shortViewport.height, "keyboard-sized viewport keeps the body bounded");
  assert.equal(await panel.locator("[data-tm-status-selector]").isVisible(), true, "status selector survives a short viewport");
  const shortScroller = panel.locator('[data-tm-column-scroll="Done"]');
  await shortScroller.evaluate((el) => { el.scrollTop = el.scrollHeight; });
  const [shortScrollerBox, shortLastCardBox] = await Promise.all([
    box(shortScroller),
    box(shortScroller.locator("[data-tm-task]").last()),
  ]);
  assert.ok(
    shortLastCardBox.y + shortLastCardBox.height <= shortScrollerBox.y + shortScrollerBox.height + 1,
    "the final active-lane card remains reachable with a short visual viewport",
  );
  await setViewport({ width: 390, height: 900 }, "the restored phone viewport");

  const doneCard = doneScroller.locator("[data-tm-task]").first();
  const beforeDetail = await doneScroller.evaluate((el) => el.scrollTop);
  await doneCard.evaluate((el) => el.click());
  const mobileDetail = modals.getByRole("dialog", { name: /^Task / });
  await mobileDetail.waitFor();
  // The click repaints the board, and a lane's position can take more than
  // one frame to come back (taskmaster-panel-dom.ts restoreBoardScroll), so
  // the comparison waits for a stable value rather than the first look. A
  // wrong position settles just as well as a right one, so the assertion
  // still fails a lane that ends up anywhere but where it started.
  assert.equal(
    await settled(() => doneScroller.evaluate((el) => el.scrollTop), "390px Done lane after opening detail"),
    beforeDetail,
    "390px opening detail does not move the lane",
  );
  await mobileDetail.getByRole("button", { name: "Close task" }).click();
  assert.equal(
    await doneCard.evaluate((el) => document.activeElement === el),
    true,
    "390px closing detail returns focus to the originating card",
  );
  assert.equal(
    await doneScroller.evaluate((el) => el.scrollTop),
    beforeDetail,
    "390px closing detail does not reset the lane scroll",
  );

  await mobileStatusButtons.filter({ hasText: /^To do/ }).click();
  await panel.locator("[data-tm-task='5']").evaluate((el) => el.click());
  const longTitleDetail = modals.getByRole("dialog", { name: "Task 5" });
  await longTitleDetail.waitFor();
  // The loading dialog carries the same aria-label as the loaded one and puts
  // the task id in .dialog-title, so waiting on the dialog alone can read the
  // id where the title belongs. The id moves to .dialog-desc once the task
  // arrives, which is the shape that means loaded.
  await longTitleDetail.locator(".dialog-desc").waitFor();
  assert.equal(await longTitleDetail.locator(".dialog-title").innerText(), LONG_TASK_TITLE);
  await longTitleDetail.getByRole("button", { name: "Close task" }).click();

  assert.equal(socketOpens, 2, "only the initial and explicit replacement sockets opened");
  assert.deepEqual(pageErrors, [], "no uncaught page errors");
  console.log("tasks-panel probe: all assertions passed");
  await context.close();
} catch (error) {
  if (page !== undefined && !page.isClosed()) {
    process.stderr.write(`panel html:\n${await page.locator('[data-region="tasks"]').innerHTML().catch(() => "<none>")}\n`);
  }
  if (pageErrors.length > 0) process.stderr.write(`page errors:\n${pageErrors.join("\n")}\n`);
  throw error;
} finally {
  await browser?.close();
  await new Promise((r) => server.close(r));
}

async function waitFor(condition, label, timeout = 5000) {
  const deadline = Date.now() + timeout;
  while (Date.now() < deadline) {
    if (await condition()) return;
    await new Promise((r) => setTimeout(r, 25));
  }
  throw new Error(`timed out waiting for ${label}`);
}
