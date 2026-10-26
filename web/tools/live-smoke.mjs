// Real-browser smoke for the production composition. HTTP and WebSocket
// boundaries are deterministic here; the Docker acceptance test supplies the
// real backend and Claude CLI. This exercises the actual DOM/xterm wiring,
// controller reuse, the verified-running active surface, OIDC logout,
// and onboarding path.
import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import { existsSync } from "node:fs";
import net from "node:net";
import { assertMobileSheetMotion, assertSheetResize } from "./mobile-sheet-motion.mjs";

// This host is redeployed often, so its durable browser install lives outside
// the checkout. CI and normal developer machines keep Playwright's default.
if (process.env.PLAYWRIGHT_BROWSERS_PATH === undefined &&
    existsSync("/workspace/tools/ms-playwright")) {
  process.env.PLAYWRIGHT_BROWSERS_PATH = "/workspace/tools/ms-playwright";
}
const { browser: getBrowser } = await import("./browser.mjs");

// These CLIs paint their own background instead of using the terminal's, so we
// give them no padding — ours would show as a stripe next to theirs. The
// geometry loop below carries a session for every provider on this list: a
// provider dropped from the edge rules gains padding and a centered grid, and
// the unguarded rule change fails the smoke instead of shipping quietly.
const EDGE_TO_EDGE_PROVIDERS = new Set(["opencode", "kilo", "mimo", "grok"]);
// Providers whose terminal surface is pinned to a measured palette
// (web/src/terminal/provider-theme.ts): computed pane background per
// provider. Absent providers keep the theme's ground.
const PRESENTATION_GROUNDS = { mimo: "rgb(30, 30, 30)" };
// The harness inventory this mock serves over GET /api/providers. The probe
// derives every expected chip and count from this list, and it preconditions
// on the server actually returning it. Pointing this probe at a real backend
// with a different inventory will fail here with a named reason instead of
// mid-assertion.
const HARNESS_INVENTORY = Object.freeze([
  "claude", "cline", "codex", "command-code", "cursor", "devin", "grok",
  "kilo", "kimi", "mimo", "omp", "opencode", "pi", "qoder", "shell",
]);

// The rail lists every ten seconds, so a wait on a row has to outlast one period.
const sidebarWaitMS = 30_000;

const port = await freePort();
const origin = `http://127.0.0.1:${port}`;
let serverOutput = "";
const vite = spawn(
  "pnpm",
  ["exec", "vite", "--host", "127.0.0.1", "--port", String(port), "--strictPort"],
  { cwd: new URL("..", import.meta.url), stdio: ["ignore", "pipe", "pipe"] },
);
vite.stdout.on("data", (chunk) => { serverOutput += chunk; });
vite.stderr.on("data", (chunk) => { serverOutput += chunk; });

let browser;
let page;
const browserDiagnostics = [];
const pageErrors = [];
try {
  await waitForServer(origin);
  // Which browser answered is part of the result: a smoke run against a
  // different browser than the reader assumes is a wrong pass, not a
  // missing one.
  let via;
  ({ browser, via } = await getBrowser());
  console.log(`browser: ${via}`);
  const context = await browser.newContext();
  await context.addCookies([{
    name: "noisedeck_csrf",
    value: "browser-smoke-csrf",
    url: origin,
    sameSite: "Lax",
  }]);
  page = await context.newPage();
  page.on("pageerror", (error) => {
    browserDiagnostics.push(`pageerror: ${error.message}`);
    pageErrors.push(error.message);
  });
  page.on("console", (message) => {
    if (message.type() === "error") browserDiagnostics.push(`console: ${message.text()}`);
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
    id: "identity-1", name: "Deck smoke", email: "deck-smoke@example.com", revision: 1,
    created_at: "2026-08-01T00:00:00Z", updated_at: "2026-08-01T00:00:00Z",
  };
  const longSessionName = `${"long-session-name-".repeat(8)}claude`;
  const summaries = [
    {
      id: "session-1",
      project_id: project.id,
      provider: "claude",
      display_name: longSessionName,
      status: "running",
      generation: 1,
      created_at: "2026-08-01T00:00:00Z",
      last_output_at: "2026-08-22T00:00:00Z",
    },
    {
      id: "session-gutter",
      project_id: project.id,
      provider: "mimo",
      display_name: "",
      status: "running",
      generation: 1,
      created_at: "2026-08-01T00:00:00Z",
    },
    {
      id: "session-kilo",
      project_id: project.id,
      provider: "kilo",
      display_name: "",
      status: "running",
      generation: 1,
      created_at: "2026-08-01T00:00:00Z",
    },
    {
      id: "session-grok-edge",
      project_id: project.id,
      provider: "grok",
      display_name: "",
      status: "running",
      generation: 1,
      created_at: "2026-08-01T00:00:00Z",
    },
    {
      id: "session-2",
      project_id: project.id,
      provider: "codex",
      status: "running",
      generation: 1,
      created_at: "2026-08-01T00:00:00Z",
    },
    ...["cline", "kimi", "omp", "opencode", "pi", "shell"].map((provider, index) => ({
      id: `session-${index + 3}`,
      project_id: project.id,
      provider,
      status: "running",
      generation: 1,
      created_at: "2026-07-31T00:00:00Z",
    })),
  ];
  const createdSummary = {
    id: "session-created",
    project_id: project.id,
    provider: "shell",
    display_name: "",
    status: "running",
    generation: 1,
    created_at: "2026-08-02T00:00:00Z",
  };
  const mimoSummary = {
    id: "session-mimo",
    project_id: project.id,
    provider: "mimo",
    display_name: "",
    status: "running",
    generation: 1,
    created_at: "2026-08-02T00:00:00Z",
  };
  const qoderSummary = {
    id: "session-qoder",
    project_id: project.id,
    provider: "qoder",
    display_name: "",
    status: "running",
    generation: 1,
    created_at: "2026-08-02T00:00:00Z",
  };
  let qoderCreate = null;
  const cursorSummary = {
    id: "session-cursor",
    project_id: project.id,
    provider: "cursor",
    display_name: "",
    status: "running",
    generation: 1,
    created_at: "2026-08-02T00:00:00Z",
  };
  let cursorCreate = null;
  const grokSummary = {
    id: "session-grok",
    project_id: project.id,
    provider: "grok",
    display_name: "",
    status: "running",
    generation: 1,
    created_at: "2026-08-02T00:00:00Z",
  };
  let grokCreate = null;
  let mimoCreate = null;
  let resolveCreate;
  const createReleased = new Promise((resolve) => { resolveCreate = resolve; });
  let createStarted = false;
  let consoleSettings = {
    skip_permissions: false,
    skip_close_confirm: false,
    skip_remove_project_confirm: false,
  };
  let registered = false;
  let projectRemoved = false;
  let projectListFailing = true;
  // While set, the project-list request the Retry gesture sends is held here
  // until released; a poll's request never matches it.
  let retryHold = null;
  // The server's memory of the last opened session, and every write the
  // console makes to it.
  let serverLastSession = null;
  const lastSessionPuts = [];
  // projectListHangs holds that many project-list requests open forever:
  // the stalled-first-read path, where the poll must run anyway.
  let projectListHangs = 0;
  let providersFail = false;
  // Gates are held until the probe releases them, so a state under test lasts
  // as long as the assertions need, however slow the runner.
  let providersSuccessGate = null;
  let settingsReadGate = null;
  let sessionOneStatus = "running";
  let sessionOneRecoveryCapability = "none";
  let sessionOneRecoveryState = "unavailable";
  let sessionOnePresentationState = "running";
  let recoverCalls = 0;
  let refuseNextRecover = false;
  let ladderNextRecover = false;
  // The retained screen of session-1's dead pane, served as bytes to the
  // resume-ended card. Fixture text, never interpreted by the app.
  let sessionOneScreen = null;
  // A title marker proves the ordinary status response reached renderChrome,
  // which runs after refreshOpenPopover; response arrival alone is too early.
  // Every status response carries the current cycle's marker until the probe
  // advances it, so no poll can outrun the assertions.
  let moreMenuRefreshCycle = null;
  let sessionTwoStatus = "running";
  let sessionTwoPresentationState = "running";
  let sessionTwoDetailFailures = 0;
  let sessionTwoDetailGate = null;
  let registration = null;
  let logoutRequest = null;
  let rejectedRemoval = null;
  let authExpired = true;

  const detail = (summary) => ({
    ...summary,
    status: summary.id === "session-1"
      ? sessionOneStatus
      : summary.id === "session-2" ? sessionTwoStatus : "running",
    pane_pid: summary.id === "session-1" ? 4242 : 4343,
    tmux_pane_id: summary.id === "session-1" ? "%1" : "%2",
    native_session_id: `native-${summary.id}`,
    // A resume-ended row carries the refused attempt's exit evidence.
    ...(summary.id === "session-1" && sessionOnePresentationState === "resume_ended"
      ? { exit_code: 1, exited_at: "2026-08-01T00:00:00Z", recovery_refusal: "provider" } : {}),
    updated_at: "2026-08-01T00:00:00Z",
    recovery_capability: summary.id === "session-1" ? sessionOneRecoveryCapability : "none",
    recovery_state: summary.id === "session-1" ? sessionOneRecoveryState : "unavailable",
    presentation_state: summary.id === "session-1"
      ? sessionOnePresentationState
      : summary.id === "session-2" ? sessionTwoPresentationState : "running",
  });
  const assertMutation = (request) => {
    assert.equal(request.headers()["x-csrf-token"], "browser-smoke-csrf");
  };

  await page.route(`${origin}/auth/logout`, async (route) => {
    const request = route.request();
    assert.equal(request.method(), "POST");
    assertMutation(request);
    logoutRequest = { method: request.method() };
    // Logout is real for the reloaded page too: the session check after
    // the post-logout navigation must answer expired, or the smoke would
    // pass with a still-authenticated reload.
    authExpired = true;
    await route.fulfill({ status: 204, body: "" });
  });
  await page.route(`${origin}/auth/session`, async (route) => {
    if (authExpired) {
      await route.fulfill({
        status: 401,
        contentType: "application/json",
        headers: { "X-Noisedeck-Auth-Mode": "builtin" },
        body: JSON.stringify({ error: "unauthorized", message: "session expired" }),
      });
      return;
    }
    await route.fulfill({
      status: 204,
      body: "",
      headers: { "X-Noisedeck-Auth-Mode": "builtin" },
    });
  });
  await page.route(`${origin}/api/**`, async (route) => {
    const request = route.request();
    const url = new URL(request.url());
    const path = url.pathname;
    const json = (status, body) => route.fulfill({
      status,
      contentType: "application/json",
      body: JSON.stringify(body),
    });

    if (authExpired) {
      return json(401, { error: "unauthorized", message: "session expired" });
    }
    if (request.method() === "GET" && path === "/api/session-activity/events") {
      return route.fulfill({ status: 200, contentType: "text/event-stream", body: ": connected\n\nretry: 3600000\n\n" });
    }
    if (request.method() === "GET" && path === "/api/harness-login") {
      // The login coordinator polls the listing while a controller is
      // attached; the smoke has no login waiting.
      return json(200, { pending: [], csrf: "login-csrf" });
    }
    if (request.method() === "GET" && path === "/api/providers") {
      if (providersFail) {
        return json(503, { error: "temporarily_unavailable", message: "provider registry unavailable" });
      }
      if (providersSuccessGate) await providersSuccessGate.promise;
      return json(200, { providers: HARNESS_INVENTORY });
    }

    if (request.method() === "GET" && path.startsWith("/api/harness-usage")) {
      // The usage strip reads launch history on every mount; the smoke has
      // none to report.
      return json(200, { global: [], project: [] });
    }
    if (request.method() === "GET" && path === "/api/settings/tutorial") return json(200, { version: 1, dismissed: true, lesson: "welcome", step: 0, done: [], scope: "operator" });
    if (request.method() === "GET" && path === "/api/settings") {
      // The ⋯ menu IS the settings surface and reads the console settings
      // when it opens (the backend is the authority), so the smoke must
      // answer it for the menu to settle.
      if (settingsReadGate) await settingsReadGate.promise;
      return json(200, consoleSettings);
    }
    if (request.method() === "GET" && path === "/api/settings/last-session") {
      if (serverLastSession === null) return json(404, { error: "not_found", message: "no last session remembered" });
      return json(200, { session_id: serverLastSession });
    }
    if (request.method() === "PUT" && path === "/api/settings/last-session") {
      lastSessionPuts.push(request.postDataJSON().session_id);
      return json(200, request.postDataJSON());
    }
    if (request.method() === "GET" && path === "/api/settings/layout") {
      // 404 is the real backend's "no layout saved yet" answer: the sidebar
      // keeps its default expansion rather than reading a missing row as an
      // explicit all-collapsed state.
      return json(404, { error: "not_found", message: "no layout saved" });
    }
    if (request.method() === "PUT" && path === "/api/settings/layout") {
      assertMutation(request);
      return json(200, request.postDataJSON());
    }
    if (request.method() === "GET" && path === "/api/git-identities") {
      return json(200, { profiles: [gitIdentity], default_profile_id: gitIdentity.id, revision: 1 });
    }
    if (request.method() === "PUT" && path === "/api/settings") {
      assertMutation(request);
      consoleSettings = { ...request.postDataJSON() };
      return json(200, consoleSettings);
    }
    if (request.method() === "GET" && path === `/api/projects/${project.id}/preview/settings`) {
      return json(200, { configured: false, self_service: false, enabled: false });
    }
    if (request.method() === "GET" && path === "/api/projects") {
      if (projectListHangs > 0) {
        projectListHangs--;
        return new Promise(() => {});
      }
      if (retryHold !== null && request.headers()["x-smoke-gesture"] === "retry") {
        retryHold.seen = true;
        await retryHold.gate.promise;
      }
      if (projectListFailing) {
        return json(503, {
          error: "temporarily_unavailable",
          message: "project registry unavailable",
        });
      }
      // The current Go repository serializes its nil slice as null on a fresh
      // database. The production client must still render onboarding.
      return json(200, registered ? [project] : null);
    }
    if (request.method() === "DELETE" && path === `/api/projects/${project.id}`) {
      assertMutation(request);
      projectRemoved = true;
      registered = false;
      return route.fulfill({ status: 204, body: "" });
    }
    if (request.method() === "GET" && path === "/api/projects/inspect") {
      const input = url.searchParams.get("input");
      return json(200, {
        kind: "repository", path: input, exists: true, entries: [],
        store_path: `${input}/.git/noise-tasks/tasks.sqlite`,
        remote: "", head: "main",
      });
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
          directories: [{ name: "project", path: "/workspace/project" }],
        });
      }
      if (selected === "/workspace/project") {
        return json(200, {
          current_path: "/workspace/project",
          parent_path: "/workspace",
          directories: [],
        });
      }
      throw new Error(`unexpected directory browse path ${selected}`);
    }
    if (request.method() === "POST" && path === "/api/projects") {
      assertMutation(request);
      registration = request.postDataJSON();
      project.name = registration.name;
      project.canonical_path = registration.path;
      registered = true;
      return json(201, project);
    }
    if (request.method() === "GET" && path === `/api/projects/${project.id}/sessions`) {
      return json(200, summaries.map((summary) => ({
        ...summary,
        status: summary.id === "session-1"
          ? sessionOneStatus
          : summary.id === "session-2" ? sessionTwoStatus : "running",
        recovery_capability: summary.id === "session-1" ? sessionOneRecoveryCapability : "none",
        recovery_state: summary.id === "session-1" ? sessionOneRecoveryState : "unavailable",
        presentation_state: summary.id === "session-1"
          ? sessionOnePresentationState
          : summary.id === "session-2" ? sessionTwoPresentationState : "running",
      })));
    }
    if (request.method() === "GET" && path === "/api/sessions/session-1/screen") {
      if (sessionOneScreen === null) {
        return json(404, { error: "no_screen", message: "this session's last screen is not retained" });
      }
      return route.fulfill({ status: 200, contentType: "text/plain; charset=utf-8", body: sessionOneScreen });
    }
    if (request.method() === "POST" && path === "/api/sessions/session-1/recover") {
      assertMutation(request);
      recoverCalls += 1;
      if (refuseNextRecover) {
        refuseNextRecover = false;
        return json(409, { error: "not_resumable", message: "this session could not be revived; its provider refused or left nothing to resume" });
      }
      if (ladderNextRecover) {
        // The row recorded no conversation to resume, so the server
        // relaunched a fresh one in the same record. The presentation flip
        // is left to the test so the card's honest sentence can be asserted
        // before the remount.
        ladderNextRecover = false;
        sessionOneRecoveryState = "unavailable";
        return json(200, {
          ...summaries.find((x) => x.id === "session-1"),
          status: "running",
          presentation_state: "running",
          native_session_id: "b6de1fc2-a651-47a8-bd7d-2f28e93e2a10",
          outcome: "relaunched",
        });
      }
      sessionOnePresentationState = "running";
      sessionOneRecoveryState = "unavailable";
      return json(200, { ...summaries.find((x) => x.id === "session-1"), status: "running", presentation_state: "running", outcome: "resumed" });
    }
    if (request.method() === "POST" && path === `/api/projects/${project.id}/sessions`) {
      assertMutation(request);
      const body = request.postDataJSON();
      if (body.provider === "mimo") {
        // The MiMo admission is exercised end to end: the picker's row must
        // submit this exact create request, not merely render.
        mimoCreate = body;
        summaries.unshift(mimoSummary);
        return json(201, {
          ...mimoSummary,
          recovery_capability: "none",
          recovery_state: "unavailable",
          presentation_state: "running",
        });
      }
      if (body.provider === "qoder") {
        // The Qoder admission is exercised end to end: the picker's row must
        // submit this exact create request, not merely render.
        qoderCreate = body;
        summaries.unshift(qoderSummary);
        return json(201, {
          ...qoderSummary,
          recovery_capability: "none",
          recovery_state: "unavailable",
          presentation_state: "running",
        });
      }
      if (body.provider === "cursor") {
        // The Cursor admission is exercised end to end: the picker's row
        // must submit this exact create request, not merely render.
        cursorCreate = body;
        summaries.unshift(cursorSummary);
        return json(201, {
          ...cursorSummary,
          recovery_capability: "none",
          recovery_state: "unavailable",
          presentation_state: "running",
        });
      }
      if (body.provider === "grok") {
        // The Grok admission is exercised end to end: the picker's row must
        // submit this exact create request, not merely render. The unshift
        // serves the created row's detail GET; the geometry loop measures
        // the seeded session-grok-edge row, which exists before it runs.
        grokCreate = body;
        summaries.unshift(grokSummary);
        return json(201, {
          ...grokSummary,
          recovery_capability: "none",
          recovery_state: "unavailable",
          presentation_state: "running",
        });
      }
      assert.equal(body.provider, "shell");
      assert.deepEqual(Object.keys(body).sort(), ["cols", "provider", "rows"]);
      createStarted = true;
      await createReleased;
      summaries.unshift(createdSummary);
      return json(201, {
        ...createdSummary,
        recovery_capability: "none",
        recovery_state: "unavailable",
        presentation_state: "running",
      });
    }
    if (request.method() === "DELETE" && path === "/api/sessions/session-8") {
      assertMutation(request);
      rejectedRemoval = { id: "session-8" };
      const index = summaries.findIndex((summary) => summary.id === "session-8");
      if (index >= 0) summaries.splice(index, 1);
      return json(500, { error: "cleanup_pending", message: "could not remove session" });
    }
    for (const summary of summaries) {
      if (request.method() === "GET" && path === `/api/sessions/${summary.id}`) {
        if (summary.id === "session-2" && sessionTwoDetailGate) {
          // Held, not failed: the browser sits in its loading state until
          // released, which is the only way to observe it.
          await sessionTwoDetailGate.promise;
        }
        if (summary.id === "session-2" && sessionTwoDetailFailures > 0) {
          sessionTwoDetailFailures--;
          return json(503, { error: "temporarily_unavailable", message: "session detail unavailable" });
        }
        const session = detail(summary);
        if (summary.id === "session-1" && moreMenuRefreshCycle !== null) {
          session.display_name = `More menu refresh ${moreMenuRefreshCycle}`;
        }
        return json(200, session);
      }
      if (request.method() === "GET" && path === `/api/sessions/${summary.id}/messages`) {
        // Guard: the direct-terminal live app must never query the retired
        // composer/outbox. Any request here is a regression.
        throw new Error(`retired composer endpoint queried: GET ${path}`);
      }
    }
    if (/^\/api\/projects\/[^/]+\/lanes$/.test(path)) return json(200, { lanes: [] });
    if (request.method() === "GET" && path === `/api/projects/${project.id}/taskmaster`) {
      return json(404, { error: "not_taskmaster_project", message: "not a Taskmaster project" });
    }
    if (request.method() === "GET" && path === `/api/projects/${project.id}/taskmaster/events`) {
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
    if (request.method() === "GET" && path === `/api/projects/${project.id}/assets`) {
      return json(200, { locations: [], scanned: "2026-01-01T00:00:00Z", preview_max_bytes: 1048576 });
    }
    if (request.method() === "GET" && path === `/api/projects/${project.id}/assets/events`) {
      return route.fulfill({ status: 200, contentType: "text/event-stream", body: ": connected\n\nretry: 3600000\n\n" });
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
    throw new Error(`unhandled ${request.method()} ${path}`);
  });

  const sockets = [];
  const clientFrames = [];
  // The session's latest reported column count: every attach URL carries
  // the settled geometry, and a resize frame supersedes it when the
  // geometry changes after attach. Keyed per session so a re-attach can
  // never read the previous mount's columns.
  const columnsBySession = new Map();
  let sessionOneDSRPending = true;
  let sessionOneInputs = 0;
  // Write statuses this mock has sent, each tagged with the input it
  // answered: the DSR reply earns a sent as well, so "a sent was sent"
  // cannot identify one exact write's outcome.
  const writeStatuses = [];
  let firstLeaseRefused = false;
  let closedSockets = 0;
  await page.routeWebSocket(`${origin.replace("http", "ws")}/api/sessions/*/terminal?*`, (socket) => {
    const url = new URL(socket.url());
    const session = url.pathname.split("/").at(-2);
    const controller = url.searchParams.get("controller");
    const takeover = url.searchParams.get("takeover") === "1";
    const record = { session, controller, takeover, closed: false };
    sockets.push(record);
    columnsBySession.set(session, Number(url.searchParams.get("cols")));
    socket.onClose(() => {
      if (record.closed) return;
      record.closed = true;
      closedSockets++;
    });
    if (session === "session-1" && !takeover && !firstLeaseRefused) {
      firstLeaseRefused = true;
      socket.send(JSON.stringify({
        type: "error",
        code: "lease_held",
        message: "another controller holds this session",
      }));
      void socket.close();
      return;
    }
    const drivesOutcomes = session === "session-1";
    const sendsDSR = drivesOutcomes && sessionOneDSRPending;
    if (sendsDSR) sessionOneDSRPending = false;
    socket.onMessage((message) => {
      if (typeof message === "string") {
        const frame = JSON.parse(message);
        clientFrames.push({ session, frame });
        if (frame.type === "resize") columnsBySession.set(session, frame.cols);
        // sent is reported only in response to an actual write — a status
        // with no preceding input/reply would be invented evidence. The
        // SECOND write fails so the smoke can prove an abnormal outcome
        // renders — and does not survive reattachment.
        if (drivesOutcomes && frame.type === "reply") {
          socket.send(JSON.stringify({ type: "status", code: "sent" }));
        }
        if (drivesOutcomes && frame.type === "input") {
          sessionOneInputs += 1;
          const outcome = sessionOneInputs === 1
            ? { code: "sent" }
            : { code: "failed", message: "stale generation" };
          writeStatuses.push({ input: sessionOneInputs, code: outcome.code });
          socket.send(JSON.stringify({ type: "status", ...outcome }));
          if (sessionOneInputs === 1) {
            // Frames are handled in order: once this line renders, the
            // status before it has been handled.
            socket.send(JSON.stringify({
              type: "output",
              data: Buffer.from("INPUT 1 ANSWERED\r\n").toString("base64"),
            }));
          }
          if (sessionOneInputs > 1) {
            // The real server closes the attachment after a terminal
            // outcome; the specific reason reaches the browser through
            // that close, so the mock must close too.
            void socket.close();
          }
        }
      }
    });
    const token = session === "session-1" ? 7 : 8;
    socket.send(JSON.stringify({ type: "attached", token, generation: 3 }));
    socket.send(JSON.stringify({
      type: "output",
      data: Buffer.from(`OUTPUT ${session}\r\n`).toString("base64"),
    }));
    if (sendsDSR) {
      // A provider can ask the emulator for terminal state. xterm's automatic
      // DSR response must use the allowlisted reply channel, never raw input.
      socket.send(JSON.stringify({
        type: "output",
        data: Buffer.from("\x1b[5n").toString("base64"),
      }));
    }
  });

  await page.goto(origin, { waitUntil: "networkidle" });
  await page.getByRole("link", { name: "Sign in with Google" }).waitFor();
  assert.equal(
    await page.locator("[data-region]").count(),
    0,
    "an unauthenticated browser must not receive application chrome",
  );
  authExpired = false;
  await page.reload({ waitUntil: "networkidle" });
  await page.locator('[data-region="terminal"]')
    .getByText("project registry unavailable", { exact: true }).waitFor();
  await page.locator('[data-empty-pane="error"]').waitFor();
  assert.equal(
    await page.getByText("No projects yet", { exact: true }).count(),
    0,
    "a failed authoritative read is not rendered as an empty database",
  );
  assert.equal(
    await page.locator("[data-empty-action]").textContent(),
    "Retry now",
    "the only action while the authoritative read is failing is a retry, never a create",
  );
  assert.equal(
    await page.locator("[data-new-project-row]").count(),
    0,
    "the sidebar offers no New project row while the authoritative read is failing",
  );
  // Retry now runs the read at once. The registry keeps failing until the
  // request that gesture sent has arrived, so a poll cannot recover it first.
  await page.evaluate(() => {
    let pressing = false;
    const send = window.fetch.bind(window);
    window.fetch = (input, init) => {
      if (!pressing) return send(input, init);
      const headers = new Headers(init?.headers);
      headers.set("x-smoke-gesture", "retry");
      return send(input, { ...init, headers });
    };
    document.addEventListener("click", (event) => {
      if (!(event.target instanceof Element) || event.target.closest("[data-empty-action]") === null) return;
      pressing = true;
      setTimeout(() => { pressing = false; }, 0);
    }, true);
  });
  retryHold = { seen: false, gate: createGate() };
  await page.getByRole("button", { name: "Retry now", exact: true }).click();
  await waitForCondition(() => retryHold.seen, "Retry now sending its project-list request", 5_000);
  projectListFailing = false;
  retryHold.gate.release();
  retryHold = null;
  await page.getByText("No projects yet", { exact: true })
    .waitFor({ timeout: 5_000 });
  console.log("ok: Retry now reaches the registry without waiting for the poll");

  // The stalled first read: the request never answers. The pane says it is
  // reconnecting, then after two seconds that it is still reaching the
  // runtime, and the poll — started before the first read, not after it —
  // reaches the registry on its own at ten seconds and settles the page.
  // The loading pane is gone by ten seconds, so the page records every
  // loading paint and the probe reads the record once the page has settled.
  // The old document's poll must be gone before the hang is armed, or it
  // would take the hang meant for the first read.
  const reloadUrl = page.url();
  await page.goto("about:blank");
  projectListHangs = 1;
  await page.addInitScript(() => {
    window.__loadingPaints = [];
    new MutationObserver(() => {
      const pane = document.querySelector('[data-empty-pane="loading"]');
      if (pane === null) return;
      const paint = JSON.stringify({
        phase: pane.getAttribute("data-empty-phase"),
        actions: document.querySelectorAll("[data-empty-action]").length,
        newProjectRows: document.querySelectorAll("[data-new-project-row]").length,
        bands: document.querySelectorAll('[data-region="sidebar"] [data-loading-band]').length,
      });
      if (window.__loadingPaints.at(-1) !== paint) window.__loadingPaints.push(paint);
    }).observe(document, { subtree: true, childList: true, attributes: true });
  });
  await page.goto(reloadUrl, { waitUntil: "load" });
  await page.getByText("No projects yet", { exact: true })
    .waitFor({ timeout: 30_000 });
  const paints = (await page.evaluate(() => window.__loadingPaints)).map((paint) => JSON.parse(paint));
  assert.deepEqual([...new Set(paints.map(({ phase }) => phase))], ["reconnect", "slow"],
    "the pane says reconnecting, then still reaching the runtime");
  assert.ok(paints.every(({ actions }) => actions === 0), "no action while the read is in flight");
  assert.ok(paints.every(({ newProjectRows }) => newProjectRows === 0), "no create affordance while the read is in flight");
  assert.ok(paints.some(({ bands }) => bands > 0), "the rail draws bands while the read is in flight");
  await waitForCondition(async () => await page.locator("[data-loading-band]").count() === 0,
    "the bands leaving with the answer");
  console.log("ok: a first read that hangs is superseded by the poll, not waited on");
  // The empty-state guidance names the current action and never uses
  // container or runtime vocabulary.
  const guidance = page.locator('[data-region="terminal"]').getByText("Point Deck at a directory, an existing repository, or a repository to clone.", { exact: true });
  await guidance.waitFor();
  await page.locator('[data-region="terminal"] [data-empty-action]').getByText("Add project", { exact: true }).waitFor();
  assert.equal(await page.getByText(/container/i).count(), 0, "no container wording anywhere in the app");
  // The display identity: the document carries the product name,
  // and the sidebar carries the noisefloor lockup as decorative artwork
  // inside the named collapse control (comp desktop-r1), so the probe binds
  // to the same [data-collapse] path the handler does.
  assert.equal(await page.title(), "Noisefloor Deck");
  const lockup = page.locator('[data-region="sidebar"] [data-collapse] img');
  await lockup.waitFor();
  // The reference pins the wordmark at exactly 99×20 (the noisefloor
  // lockup, not the wider deck lockup).
  const box = await lockup.boundingBox();
  assert.ok(box !== null, "the sidebar lockup renders");
  assert.ok(Math.abs(box.height - 20) < 0.5, `lockup height ${box.height} is not 20px`);
  assert.ok(Math.abs(box.width - 99) < 1, `lockup width ${box.width} is not 99px`);
  // Left edge: the chrome bar's 16px padding-left plus the collapse
  // control's 8px margin-left — the reference's own placement values.
  assert.ok(Math.abs(box.x - 24) < 0.5, `lockup left edge ${box.x} is not 24px`);
  // Toggle parity: the collapsed rail's mark holds the same 24px left
  // edge (26px button + 1px margin + 3px centering inset).
  await page.locator("[data-collapse]").click();
  const mark = page.locator('[data-region="sidebar-expand"] span');
  await mark.waitFor();
  // Measure only once the rail has actually left the layout, so a
  // mid-transition frame cannot read as the resting geometry.
  await page.locator('[data-region="sidebar"]').waitFor({ state: "hidden" });
  const mbox = await mark.boundingBox();
  assert.ok(mbox !== null && Math.abs(mbox.x - 24) < 0.5, `collapsed mark left edge: ${mbox?.x}`);
  await page.locator('[data-region="sidebar-expand"]').click();
  // Hold until the workspace is back on its expanded attribute AND the
  // rail has fully re-entered the layout, so the assertions that follow
  // measure resting geometry.
  await page.locator('[data-workspace][data-collapsed="false"]').waitFor();
  await page.waitForFunction(() => {
    const rail = document.querySelector('[data-region="sidebar"]');
    return rail !== null && rail.getBoundingClientRect().x > -0.5;
  });
  const sidebarBox = await page.locator('[data-region="sidebar"]').boundingBox();
  assert.ok(
    sidebarBox !== null && box.x + box.width <= sidebarBox.x + sidebarBox.width,
    "the lockup fits inside the sidebar",
  );
  // Project data casing: the title-bar eyebrow must render the real
  // name/path untransformed (no small-caps, no uppercase). desktop-r16:
  // with no projects the Projects header does not render, so the first
  // registration enters through the pane's New project action — the
  // same form the menu's row opens.
  await page.locator('[data-region="terminal"] [data-empty-action]').click();
  await page.getByRole("button", { name: /^Local folder/ }).click();
  await page.locator("[data-project-form]").waitFor();
  await page.locator("[data-project-browse]").click();
  await page.locator('[data-directory-path="/workspace/project"]').click();
  await page.getByRole("button", { name: "Choose this directory" }).click();
  assert.equal(await page.getByRole("combobox", { name: "Git identity" }).count(), 0, "the only identity applies without a field");
  const projectPath = page.locator("[data-project-path]");
  assert.equal(await projectPath.inputValue(), "/workspace/project");
  assert.equal(await projectPath.isEditable(), true);
  assert.equal(await projectPath.evaluate((input) => getComputedStyle(input).fontVariantCaps), "normal");
  assert.equal(await projectPath.evaluate((input) => getComputedStyle(input).textTransform), "none");
  await page.locator("[data-project-browse]").click();
  await page.getByRole("button", { name: "Up" }).click();
  await page.locator('[data-directory-path="/workspace/project"]').click();
  await page.getByRole("button", { name: "Choose this directory" }).click();
  // Scoped to the form so the submit's own label is what's asserted; the
  // desktop card's primary says Add project (comp desktop-r32, r12 rename).
  await page.locator("[data-project-form]").getByRole("button", { name: "Add project" }).click();
  await page.locator('[data-session="session-1"]').waitFor();
  assert.deepEqual(registration, {
    name: "project",
    path: "/workspace/project",
    plain: false,
    git_identity_profile_id: gitIdentity.id,
    expected_git_identity_revision: 1,
  });

  // The sidebar header's type: the PROJECTS eyebrow is a menubar-style
  // menu trigger — a micro label with tracked spacing, the body ratio's
  // 17.05px line box, and text-transform: uppercase over sentence-case
  // copy. The header renders nothing while the registry is empty
  // (desktop-r16), so it is asserted from the registered state.
  const projectsTrigger = page.locator("[data-projects-menu]");
  await projectsTrigger.waitFor();
  const triggerStyle = await projectsTrigger.evaluate((el) => {
    const s = getComputedStyle(el);
    return { fontSize: s.fontSize, letterSpacing: s.letterSpacing, lineHeight: s.lineHeight, textTransform: s.textTransform };
  });
  assert.equal(triggerStyle.fontSize, "11px", "Projects trigger uses the micro size");
  assert.equal(triggerStyle.letterSpacing, "0.77px", "Projects trigger uses the micro tracking");
  assert.equal(
    triggerStyle.lineHeight,
    "17.05px",
    "Projects trigger line box uses the inherited 1.55 ratio",
  );
  assert.equal(triggerStyle.textTransform, "uppercase", "Projects trigger is uppercased");

  await page.locator('[data-session="session-1"]').click();
  await page.getByRole("button", { name: "Take control" }).click();
  await waitForTerminal(page, "OUTPUT session-1");

  const title = page.locator('[data-region="terminal-titlebar"] [data-titlebar-title]');
  assert.equal(await title.evaluate((el) => getComputedStyle(el).fontVariantCaps), "normal");
  assert.equal(await title.evaluate((el) => getComputedStyle(el).textTransform), "none");
  await waitForTerminal(page, "OUTPUT session-1");
  // The attach handshake carries the fitted initial size in the socket URL,
  // so a clean attach sends no resize frame; a redundant one would resize
  // the PTY twice. The DSR reply below is the sync point after which the
  // absence of any resize frame is stable evidence, not a race.
  await waitForCondition(
    () => clientFrames.some(({ session, frame }) =>
      session === "session-1" && frame.type === "reply" && frame.token === 7 &&
      Buffer.from(frame.data, "base64").toString() === "\x1b[0n"),
    "xterm DSR reply frame",
  );
  const desktopSoftKeys = page.locator('[data-region="terminal-soft-keys"]');
  assert.equal(await desktopSoftKeys.evaluate((el) => getComputedStyle(el).display), "none");
  assert.equal((await desktopSoftKeys.boundingBox())?.height ?? 0, 0, "soft keys changed desktop geometry");
  assert.equal(
    clientFrames.filter(({ session, frame }) => session === "session-1" && frame.type === "resize").length,
    0,
    "the attach resized the PTY outside the handshake",
  );
  assert.equal(
    clientFrames.some(({ frame }) =>
      frame.type === "input" && Buffer.from(frame.data, "base64").toString() === "\x1b[0n"),
    false,
    "an emulator-generated reply never impersonates user input",
  );
  // The info bar is the app's only explanation channel (handoff §6):
  // the nearest [data-hint] under keyboard focus or the pointer, and empty
  // when nothing is focused or hovered.
  const infoStatus = page.locator("[data-info-status]");
  const infoBar = page.locator('[data-region="info-bar"]');
  const moreButton = page.getByRole("button", { name: "Settings", exact: true });
  const moreHint = "Open terminal, identity and account settings";
  const filesButton = page.locator("[data-live-files]");
  const filesHint = "Browse the files in this project's working tree";
  const settleNeutral = async () => {
    await page.locator('[data-region="terminal"]').hover();
    await page.locator("[data-live-terminal] .xterm-helper-textarea").focus();
  };
  const readStatus = () => infoStatus.innerText();
  const assertEmpty = async (label) => {
    await waitForCondition(async () => (await readStatus()) === "",
      `${label} clears the strip`, 5000);
    assert.equal(await infoStatus.getAttribute("data-mode"), "hint",
      `${label} leaves the bar in hint mode`);
  };
  const assertHint = async (label, hint) => {
    await waitForCondition(async () => (await readStatus()) === hint,
      `${label} shows its hint`, 5000);
    assert.equal(await infoStatus.getAttribute("data-mode"), "hint",
      `${label} keeps the bar in hint mode`);
  };
  // Resolve a text token to its computed color so the mode assertions check
  // the actual painted color, whichever theme the run renders.
  const tokenColor = (name) => page.evaluate((token) => {
    const probe = document.createElement("span");
    probe.style.color = `var(${token})`;
    document.body.appendChild(probe);
    const resolved = getComputedStyle(probe).color;
    probe.remove();
    return resolved;
  }, name);
  await settleNeutral();
  // Registering the project announced the commit identity; the notice rests
  // on the bar for its full lifetime before the bar can be neutral.
  await waitForCondition(async () => !(await readStatus()).startsWith("Commits as"),
    "the identity notice expiring", 10_000);
  await assertEmpty("neutral pointer and focus");
  assert.notEqual(await infoBar.evaluate((el) => getComputedStyle(el).display), "none",
    "the info bar is the explanation channel and is on by default");
  assert.equal(await infoBar.evaluate((el) => getComputedStyle(el).height), "26px",
    "the info bar strip is the fixed 26px chrome-bottom height");
  assert.equal((await infoBar.boundingBox())?.height ?? 0, 26,
    "the info bar occupies exactly 26px of layout height");
  const statusGeometry = await infoStatus.evaluate((el) => {
    const style = getComputedStyle(el);
    return { overflowX: style.overflowX, textOverflow: style.textOverflow, whiteSpace: style.whiteSpace };
  });
  assert.deepEqual(statusGeometry, {
    overflowX: "hidden",
    textOverflow: "ellipsis",
    whiteSpace: "nowrap",
  }, "the status text stays one line and truncates with an ellipsis");
  await moreButton.hover();
  await assertHint("hover over More", moreHint);
  assert.equal(await infoStatus.evaluate((el) => getComputedStyle(el).color),
    await tokenColor("--color-text-secondary"),
    "a hover hint paints the secondary text color");
  await settleNeutral();
  await assertEmpty("pointer leaving the More button");
  // Only a keyboard-focused control speaks, so the probe must actually be
  // in keyboard modality: Chromium gives a programmatic focus() the
  // modality of the last real input, and this run has been clicking. A
  // bare modifier sets it and types nothing — Tab here would land in the
  // focused terminal and send an input frame the later assertions count.
  const keyboardFocus = async (locator) => {
    await page.keyboard.press("Shift");
    await locator.focus();
  };
  await keyboardFocus(moreButton);
  await assertHint("keyboard focus on More", moreHint);
  assert.equal(await infoStatus.evaluate((el) => getComputedStyle(el).color),
    await tokenColor("--color-text-secondary"),
    "a focused hint paints the secondary text color, at parity with hover");
  await settleNeutral();
  await assertEmpty("focus leaving the More button");
  // The two hint sources are ordered, not a race: a hover that lands on a
  // DIFFERENT control while one is KEYBOARD-focused must not take the
  // strip, and once the focused hint clears without replacement the same
  // hover speaks again; with nothing hovered the strip returns to empty.
  await keyboardFocus(moreButton);
  await waitForCondition(async () => (await readStatus()) === moreHint,
    "info bar focused hint before the priority check", 5000);
  await filesButton.hover();
  await waitForCondition(async () => (await readStatus()) === moreHint,
    "info bar priority keeps the focused hint over a different hover", 5000);
  assert.equal(await infoStatus.getAttribute("data-mode"), "hint");
  assert.equal(await infoStatus.evaluate((el) => getComputedStyle(el).color),
    await tokenColor("--color-text-secondary"),
    "the focused hint keeps the hint color while the pointer hovers elsewhere");
  await page.locator("[data-live-terminal] .xterm-helper-textarea").focus();
  await waitForCondition(async () => (await readStatus()) === filesHint,
    "info bar hover hint regains the strip once the focused hint clears", 5000);
  assert.equal(await infoStatus.getAttribute("data-mode"), "hint");
  await page.locator('[data-region="terminal"]').hover();
  await assertEmpty("pointer leaving the Files button");
  // A control focused by CLICK stays quiet: the pointer is already saying
  // where it is, and a project row IS its toggle button, so every click in
  // the list leaves a focused control behind. Clicking twice collapses and
  // re-expands, leaving the list as found.
  const clickedRow = page.locator("[data-project-toggle]").first();
  const hoveredRow = page.locator('[data-session="session-1"]');
  const hoveredHint = await hoveredRow.getAttribute("data-hint");
  await clickedRow.click();
  await clickedRow.click();
  await hoveredRow.hover();
  await waitForCondition(async () => (await readStatus()) === hoveredHint,
    "a session hovered after its project row was clicked names the session", 5000);
  assert.equal(
    await clickedRow.evaluate((el) => el.matches(":focus-visible")),
    false,
    "the clicked row holds focus without :focus-visible — the condition the strip reads",
  );
  // Visibility is the platform's live answer, not a fact captured at focus
  // time: the clicked row becomes focus-visible the moment the user
  // reaches for the keyboard, and speaks from then on. A bare modifier is
  // that reach, and types nothing.
  const clickedHint = await clickedRow.getAttribute("data-hint");
  await page.keyboard.press("Shift");
  await waitForCondition(async () => (await readStatus()) === clickedHint,
    "the clicked row speaks on the key press itself, with the pointer parked", 5000);
  await settleNeutral();
  await assertEmpty("pointer leaving every hinted control after a click");

  const desktopViewport = page.viewportSize();
  for (const width of [320, 390, 430]) {
    await page.setViewportSize({ width, height: 720 });
    const identity = page.locator("[data-terminal-identity]");
    assert.equal(await identity.locator("[data-terminal-provider]").count(), 1,
      `${width}px keeps the provider tile`);
    assert.equal((await title.innerText()).trim(), longSessionName, `${width}px keeps the session label`);
    assert.equal(await identity.getByText(project.name, { exact: true }).count(), 0,
      `${width}px titlebar repeats the project name`);
    const identityBox = await identity.boundingBox();
    assert.ok(identityBox !== null && identityBox.x >= 0 && identityBox.x + identityBox.width <= width,
      `${width}px terminal identity escaped the viewport`);
    assert.ok(await title.evaluate((el) => el.clientWidth >= 48 && el.clientWidth < el.scrollWidth),
      `${width}px long session label must retain readable space and truncate`);
    // Mobile has no info bar (handoff §12): the strip leaves the layout
    // entirely so the content row owns every pixel of the phone.
    assert.equal(await infoBar.evaluate((el) => getComputedStyle(el).display), "none",
      `${width}px leaves the info bar out of the layout`);
    assert.equal((await infoBar.boundingBox())?.height ?? 0, 0,
      `${width}px info bar consumed vertical space`);
  }
  if (desktopViewport !== null) await page.setViewportSize(desktopViewport);

  const terminalInput = page.locator("[data-live-terminal] .xterm-helper-textarea");
  await terminalInput.focus();
  await page.keyboard.type("x");
  await waitForCondition(
    () => clientFrames.some(({ session, frame }) =>
      session === "session-1" && frame.type === "input" && frame.token === 7 &&
      Buffer.from(frame.data, "base64").toString() === "x"),
    "raw xterm input frame",
  );
  // Every input and reply frame carries the attached generation.
  const stamped = clientFrames.filter(({ frame }) => frame.type === "input" || frame.type === "reply");
  assert.ok(stamped.length > 0, "expected input/reply frames");
  assert.ok(
    stamped.every(({ frame }) => frame.generation === 3),
    "every input/reply frame stamps the attached generation",
  );

  // Sent is silent: the write just delivered earns a sent
  // outcome, and once that outcome has been handled the strip must
  // carry no outcome text — only abnormal outcomes render. The wait is
  // bound to the status answering THIS input: the DSR reply already
  // earned an earlier sent, so waiting for "a sent was sent" would pass
  // on a frame that predates this write.
  await waitForCondition(
    () => writeStatuses.some(({ input, code }) => input === 1 && code === "sent"),
    "status answering the delivered input",
  );
  await waitForTerminal(page, "INPUT 1 ANSWERED");
  const outcomeText = /write |interrupted|uncertain|failed/;
  assert.doesNotMatch(
    await readStatus(),
    outcomeText,
    "a sent write outcome must not render in the info bar",
  );

  // The measured box is the pane to the PAINTED CELL GRID (.xterm-screen),
  // not to [data-live-terminal]: the host distance only sees the pane's own
  // padding, which is 0 for the self-painting providers, so a gutter inside
  // the host is invisible to it. Two geometries, one per provider kind:
  //
  // — A provider that draws on the terminal's background (everything not in
  //   EDGE_TO_EDGE_PROVIDERS) gets a MATCHED frame: left and right agree
  //   within half a pixel, and each is 20px plus at most half a cell — the
  //   pane's own padding plus half the sub-cell remainder an integer column
  //   count cannot avoid, centered by the host. A grid pinned to the pane's
  //   left padding drops the whole remainder on the right, and the frame
  //   reads crooked (20px against 26px at an 8px cell). A width that is an
  //   exact cell multiple hides that — both sides read 20px — so the loop
  //   must run at a width that is not one; 390 is, 1280 is not.
  //
  // — A self-painting provider's grid starts at 0: its background ends at
  //   the first column, so any inset reads as a gutter of our colour. Past
  //   the last column at most the sub-cell remainder may remain.
  const gridCellWidth = async (summary) => {
    const cols = columnsBySession.get(summary.id);
    assert.ok(cols > 0, `${summary.id} never reported its columns`);
    const gridWidth = await page.locator("[data-live-terminal] .xterm-screen")
      .evaluate((grid) => grid.getBoundingClientRect().width);
    return gridWidth / cols;
  };
  // The pane's inset is the frame's base, and the phone takes a narrower one
  // than the desktop (app.css, the 600px phone scope): a narrow screen cannot
  // spare 40px of ground. Measuring against one number at every width asserts
  // the desktop value on a phone.
  const paneInset = (width) => (width <= 600 ? 16 : 20);
  const frameSettled = (gaps, cell, edgeToEdge, base) => edgeToEdge
    ? Math.abs(gaps.left) < 0.51 && gaps.right < cell + 0.51
    : Math.abs(gaps.left - gaps.right) < 0.51 &&
      gaps.left >= base - 0.51 &&
      gaps.left < base + cell / 2 + 0.51;
  const assertTerminalFrame = (label, gaps, cell, edgeToEdge, base) => {
    const px = (value) => `${Math.round(value * 100) / 100}px`;
    if (edgeToEdge) {
      assert.ok(Math.abs(gaps.left) < 0.51,
        `${label}: the grid starts ${px(gaps.left)} from the pane edge, want 0`);
      assert.ok(gaps.right < cell + 0.51,
        `${label}: ${px(gaps.right)} beside the grid is wider than one ${px(cell)} cell`);
    } else {
      assert.ok(Math.abs(gaps.left - gaps.right) < 0.51,
        `${label}: the frame is asymmetric, ${px(gaps.left)} left and ${px(gaps.right)} right; a padded provider's frame must match on both sides`);
      assert.ok(gaps.left >= base - 0.51,
        `${label}: the frame is ${px(gaps.left)}, want at least ${px(base)}`);
      assert.ok(gaps.left < base + cell / 2 + 0.51,
        `${label}: the frame is ${px(gaps.left)}, want less than ${px(base)} plus half a ${px(cell)} cell of centered remainder`);
    }
  };
  for (const width of [1280, 390]) {
    await page.setViewportSize({ width, height: 720 });
    for (const summary of summaries) {
      if (width < 640) await expandSidebar(page);
      await page.locator(`[data-session="${summary.id}"]`).click();
      await page.locator(`[data-region="terminal"][data-terminal-provider="${summary.provider}"]`).waitFor();
      const edgeToEdge = EDGE_TO_EDGE_PROVIDERS.has(summary.provider);
      let gaps = null;
      let cell = 0;
      try {
        await waitForCondition(async () => {
          if (!(columnsBySession.get(summary.id) > 0)) return false;
          gaps = await terminalGaps(page);
          if (gaps === null) return false;
          cell = await gridCellWidth(summary);
          return frameSettled(gaps, cell, edgeToEdge, paneInset(width));
        }, `${width}px ${summary.provider} terminal geometry`, 5000);
      } catch (error) {
        error.message += `; last observed ${JSON.stringify(gaps)} at cell ${cell}px`;
        throw error;
      }
      assertTerminalFrame(`${width}px ${summary.provider} terminal`, gaps, cell, edgeToEdge, paneInset(width));
      // Presentation palettes (provider-theme.ts) pin a letterboxing
      // provider's ground to its measured colour; every other provider
      // keeps the theme's ground — the run renders the default
      // Noisefloor theme, #0A1522.
      const ground = await page
        .locator(`[data-region="terminal"][data-terminal-provider="${summary.provider}"]`)
        .evaluate((el) => getComputedStyle(el).backgroundColor);
      const wantGround = PRESENTATION_GROUNDS[summary.provider] ?? "rgb(10, 21, 34)";
      assert.equal(ground, wantGround,
        `${width}px ${summary.provider} terminal ground`);
    }
    // The phone task panel intentionally occupies the workspace; there is no
    // visible terminal frame beside it to measure. Desktop keeps both panes.
    if (width >= 640) {
      const tasksToggle = page.getByRole("button", { name: "Project tasks" });
      await tasksToggle.click();
      await page.locator('[data-region="tasks"][data-open="true"]').waitFor();
      const provider = await page.locator('[data-region="terminal"]')
        .getAttribute("data-terminal-provider");
      const edgeToEdge = provider !== null && EDGE_TO_EDGE_PROVIDERS.has(provider);
      const selected = summaries.at(-1);
      let panelGaps = null;
      let cell = 0;
      try {
        await waitForCondition(async () => {
          if (!(columnsBySession.get(selected.id) > 0)) return false;
          panelGaps = await terminalGaps(page);
          if (panelGaps === null) return false;
          cell = await gridCellWidth(selected);
          return frameSettled(panelGaps, cell, edgeToEdge, paneInset(width));
        }, `${width}px panel-open terminal geometry`, 5000);
      } catch (error) {
        error.message += `; last observed ${JSON.stringify(panelGaps)} at cell ${cell}px`;
        throw error;
      }
      assertTerminalFrame(`${width}px panel-open terminal`, panelGaps, cell, edgeToEdge, paneInset(width));
      await page.getByRole("button", { name: "Close tasks" }).click();
      // A closed tasks panel is inert and hidden in one step; there is no
      // visible intermediate state to observe, only the settled attribute.
      await page.locator('[data-region="tasks"][data-open="false"]')
        .waitFor({ state: "hidden" });
    }
  }
  // The dock is painted by CSS at the phone width and mounted by JS at the
  // same one. They have to agree at the boundary in both directions: a width
  // where CSS shows the dock but nothing mounted it paints an empty bar, and a
  // width where controls mount under display:none puts them in the a11y tree
  // with nothing on screen.
  for (const [width, docked] of [[390, true], [601, false], [900, false]]) {
    await page.setViewportSize({ width, height: 720 });
    const dock = page.locator('[data-region="terminal-dock"]');
    let shown = false;
    let mounted = 0;
    await waitForCondition(async () => {
      shown = await dock.evaluate((el) => getComputedStyle(el).display !== "none");
      mounted = await page.locator('[data-region="terminal-soft-keys"] button').count();
      return shown === docked && (mounted > 0) === docked;
    }, `${width}px dock`, 3000).catch(() => {});
    assert.equal(shown, docked, `${width}px paints the dock: want ${docked}, got ${shown}`);
    assert.equal(mounted > 0, docked,
      `${width}px mounts the key strip: want ${docked}, got ${mounted} keys`);
  }
  await page.setViewportSize({ width: 1280, height: 720 });
  await expandSidebar(page);
  await page.locator('[data-session="session-1"]').click();
  await waitForTerminal(page, "OUTPUT session-1");

  // The dock outlives the emulator. A session that drops into recovery keeps
  // the key strip as presentation — the comp draws it while the session is
  // away — with every key disabled and no input row, because there is nothing
  // to receive a keystroke. Disposing the strip with the terminal would leave
  // CSS painting a dock with nothing in it.
  {
    await page.setViewportSize({ width: 390, height: 720 });
    const keys = page.locator('[data-region="terminal-soft-keys"] button');
    const composeInput = page.locator("[data-mobile-compose-input]");
    await waitForCondition(async () => await keys.count() > 0,
      "the phone width mounts the key strip", 5000);
    assert.equal(await keys.first().isDisabled(), false,
      "a live session's keys are inert");
    assert.equal(await composeInput.count(), 1, "a live session carries the input row");

    sessionOnePresentationState = "recovering";
    await page.locator("[data-recovery-scrollback]").waitFor({ timeout: 12_000 });
    let mounted = 0;
    await waitForCondition(async () => {
      mounted = await keys.count();
      return mounted > 0 && await keys.first().isDisabled();
    }, "recovery keeps a disabled key strip", 5000).catch(() => {});
    assert.ok(mounted > 0,
      `recovery left ${mounted} keys under a dock CSS still paints`);
    for (const key of await keys.all()) {
      assert.equal(await key.isDisabled(), true, "a recovery key still takes input");
    }
    assert.equal(await composeInput.count(), 0,
      "recovery kept an input row with nowhere to send");

    // The phone carries the reconnect facts in a band at the top of the pane
    // (comp mobile-r8 screen 12). It is in flow: it reserves its height above
    // the transcript rather than floating over it, and it spans the pane, so
    // the pane's own inset moves to the text under it.
    {
      await page.locator("[data-recovery-banner]").waitFor({ timeout: 5000 });
      const seen = await page.evaluate(() => {
        const b = document.querySelector("[data-recovery-banner]");
        const t = document.querySelector("[data-recovery-scrollback]");
        const pane = document.querySelector('[data-region="terminal"]');
        const r = (el) => { const x = el.getBoundingClientRect(); return { top: Math.round(x.top), bottom: Math.round(x.bottom), width: Math.round(x.width) }; };
        return { band: r(b), text: r(t), pane: r(pane), shown: getComputedStyle(b).display };
      });
      assert.notEqual(seen.shown, "none", "the phone draws no reconnect band");
      assert.equal(seen.band.top, seen.pane.top,
        `the band floats below the pane's top edge: ${JSON.stringify(seen)}`);
      assert.equal(seen.band.width, seen.pane.width,
        `the band does not span the pane: ${JSON.stringify(seen)}`);
      assert.ok(seen.text.top >= seen.band.bottom,
        `the transcript runs under the band instead of below it: ${JSON.stringify(seen)}`);
      // The band REPLACES the register at this width. Both visible says
      // "reconnecting" twice; the register visible alone is the desktop
      // presentation on a phone.
      const registers = await page.evaluate(() =>
        [...document.querySelectorAll("[data-recovery-register]")]
          .filter((el) => getComputedStyle(el).display !== "none").length);
      assert.equal(registers, 0,
        `the phone shows ${registers} register row(s) beside the band`);
    }

    sessionOnePresentationState = "running";
    await page.locator("[data-live-terminal]").waitFor({ timeout: 12_000 });
    await waitForCondition(async () => await keys.count() > 0 && !(await keys.first().isDisabled()),
      "a live session re-arms the strip recovery left inert", 5000);
    assert.equal(await composeInput.count(), 1, "the input row did not come back");

    // Switching sessions passes through a loading state with nothing mounted.
    // The dock has to leave the layout there too: a teardown that clears the
    // tracking fields but leaves the marker standing paints a bordered bar
    // across the pane for as long as the next session takes to arrive.
    sessionTwoDetailGate = createGate();
    await expandSidebar(page);
    await page.locator('[data-session="session-2"]').click();
    await page.locator('[data-region="terminal"]').getByText("Loading session…", { exact: true })
      .waitFor({ timeout: 5000 });
    const loadingDock = await page.locator('[data-region="terminal-dock"]').boundingBox();
    assert.ok(loadingDock === null || loadingDock.height === 0,
      `the phone loading state draws a dock with nothing in it: ${JSON.stringify(loadingDock)}`);
    sessionTwoDetailGate.release();
    sessionTwoDetailGate = null;
    await page.locator("[data-live-terminal]").waitFor({ timeout: 12_000 });

    // Hand the selection back: the sections below run against session 1.
    await page.setViewportSize({ width: 1280, height: 720 });
    await expandSidebar(page);
    await page.locator('[data-session="session-1"]').click();
    await waitForTerminal(page, "OUTPUT session-1");

    // The last opened session follows the user across devices: every
    // click above was written to the server, and on a cold load the
    // server's memory outranks this browser's own — a phone opens what the
    // desktop was looking at. The phone-width clicks wrote it too.
    await waitForCondition(() => lastSessionPuts.slice(-2).join() === "session-2,session-1",
      "the last two selections written to the server");
    assert.deepEqual(lastSessionPuts.slice(-2), ["session-2", "session-1"],
      `each selection is remembered on the server, in order: ${JSON.stringify(lastSessionPuts)}`);
    assert.equal(await page.evaluate(() => localStorage.getItem("noisedeck.last-session-id")), "session-1",
      "and in this browser");
    serverLastSession = "session-2";
    // A navigation destroys the old document's socket without a close the
    // mock can observe, so the socket open across this reload is counted
    // closed by hand; the logout check below is the app's own accounting.
    const openAcrossReload = sockets.filter((record) => !record.closed);
    await page.reload({ waitUntil: "networkidle" });
    await waitForTerminal(page, "OUTPUT session-2");
    for (const record of openAcrossReload) {
      if (record.closed) continue;
      record.closed = true;
      closedSockets++;
    }
    assert.equal(await page.locator('[data-session="session-2"]').getAttribute("aria-selected"), "true",
      "a cold load opens the session the server remembers, not the one this browser last had");
    serverLastSession = null;
    await expandSidebar(page);
    await page.locator('[data-session="session-1"]').click();
    await waitForTerminal(page, "OUTPUT session-1");
    console.log("ok: the last opened session is remembered on the server and wins on a cold load");
  }

  // A presentation palette belongs to the session being shown, from its first
  // painted frame. The ground is resolved from the pane's own provider, so a
  // switch that paints before it re-stamps that identity paints the session
  // that just left — visible until some unrelated refresh repaints it, which
  // is why a settled assertion cannot see it.
  {
    await page.locator('[data-session="session-gutter"]').click();
    await page.locator('[data-region="terminal"][data-terminal-provider="mimo"]').waitFor();
    await page.evaluate(() => {
      window.__firstRecoveryPaint = null;
      new MutationObserver(() => {
        if (window.__firstRecoveryPaint !== null) return;
        if (document.querySelector("[data-recovery-scrollback]") === null) return;
        const pane = document.querySelector('[data-region="terminal"]');
        window.__firstRecoveryPaint = {
          ground: getComputedStyle(pane).backgroundColor,
          provider: pane.dataset.terminalProvider ?? null,
        };
      }).observe(document.body, { subtree: true, childList: true });
    });
    sessionOnePresentationState = "recovering";
    await page.locator('[data-session="session-1"]').click();
    await page.locator("[data-recovery-scrollback]").waitFor({ timeout: 12_000 });
    const first = await page.evaluate(() => window.__firstRecoveryPaint);
    assert.ok(first !== null, "the recovery surface never appeared");
    assert.equal(first.provider, "claude",
      `the recovery surface's first paint carried ${first.provider}`);
    assert.equal(first.ground, "rgb(10, 21, 34)",
      `a claude recovery surface first painted on ${first.ground}, not the theme ground`);
    sessionOnePresentationState = "running";
    await page.locator('[data-session="session-1"]').click();
    await waitForTerminal(page, "OUTPUT session-1");
  }

  // The desktop has room for the register under the stream, so the band is the
  // phone's alternative to it rather than an addition both widths carry.
  {
    sessionOnePresentationState = "recovering";
    await page.locator("[data-recovery-scrollback]").waitFor({ timeout: 12_000 });
    const shown = await page.evaluate(() => {
      const b = document.querySelector("[data-recovery-banner]");
      return b === null ? "absent" : getComputedStyle(b).display;
    });
    assert.ok(shown === "none" || shown === "absent",
      `the desktop draws the phone's reconnect band: ${shown}`);
    // And the register is the desktop's presentation of the same facts, so it
    // must be visible here.
    const desktopRegisters = await page.evaluate(() =>
      [...document.querySelectorAll("[data-recovery-register]")]
        .filter((el) => getComputedStyle(el).display !== "none").length);
    assert.ok(desktopRegisters > 0, "the desktop recovery surface lost its register");
    sessionOnePresentationState = "running";
    await page.locator("[data-live-terminal]").waitFor({ timeout: 12_000 });
  }

  // A session the server cannot attach stays reachable:
  // its row stays listed with the content-free attention dot, clicking it
  // shows the unavailable register — never a terminal mount — and the
  // selection holds instead of clearing. This is the exact vanished-session
  // path: only presentation flips; the row must survive the flip.
  {
    sessionOnePresentationState = "attachment_unavailable";
    await page.locator('[data-session="session-1"] [data-meta-attention]').waitFor({ timeout: sidebarWaitMS });
    await page.locator('[data-session="session-1"]').click();
    await page.locator("[data-recovery-scrollback]").waitFor({ timeout: 12_000 });
    const seen = await page.evaluate(() => ({
      register: [...document.querySelectorAll("[data-recovery-register]")]
        .some((el) => el.textContent.includes("no attachment")),
      terminalMounted: document.querySelector("[data-live-terminal]") !== null,
      rowListed: document.querySelector('[data-session="session-1"]') !== null,
      selected: document.querySelector('[data-session="session-1"]')?.getAttribute("aria-selected"),
    }));
    assert.equal(seen.register, true, "the unavailable register is missing");
    assert.equal(seen.terminalMounted, false, "an unverified session mounted a terminal");
    assert.equal(seen.rowListed, true, "the unattachable row left the list");
    assert.equal(seen.selected, "true", "selecting the unattachable row did not hold");
    // A refused recover must say so ON THE CARD: the info-bar error path
    // reacts only to auth, so without the card line a 409 reads as a dead
    // button.
    refuseNextRecover = true;
    const refusal = await armRecoveryError(page);
    await page.locator("[data-recovery-retry]").click();
    assert.match(await refusal(), /could not be revived/,
      "the card does not carry the server's refusal sentence");
    assert.equal(recoverCalls, 1, "the refused click did not post the recover");
    // The card's Resume is the user's explicit recover:
    // one click posts the recover, the server re-presents running, and
    // the terminal remounts - the exact way back from a deploy's limbo.
    await page.locator("[data-recovery-retry]").click();
    await waitForTerminal(page, "OUTPUT session-1");
    assert.equal(recoverCalls, 2, "Resume did not post the recover exactly once more");
  }

  // A recover that relaunched (a row with no identity to resume) must say
  // relaunch honestly: the recorded conversation did not survive, and
  // nothing on the card may imply it did.
  {
    sessionOnePresentationState = "attachment_unavailable";
    await page.locator("[data-recovery-scrollback]").waitFor({ timeout: 12_000 });
    ladderNextRecover = true;
    const relaunch = await armRecoveryError(page);
    await page.locator("[data-recovery-retry]").click();
    const said = await relaunch();
    assert.match(said, /no conversation was recorded to resume .* fresh conversation was relaunched/,
      "the card does not report the relaunch of a row with nothing to resume");
    assert.doesNotMatch(said, /\bresumed\b/,
      "a relaunch presented itself as a resume");
    assert.equal(recoverCalls, 3, "the ladder click did not post exactly one recover");
    sessionOnePresentationState = "running";
    await waitForTerminal(page, "OUTPUT session-1");
  }

  // A resume the provider did not continue is a listed, marked row whose
  // card shows the provider's own words from the retained screen and offers
  // both explicit choices. Reopening the row shows the same; Resume posts
  // the recover and the row stays until the server presents otherwise.
  {
    sessionOneScreen = "claude --resume\nNo conversation found with session ID: 41f26fd3-5a17-41d0-8820-87534dc87be7\n";
    sessionOnePresentationState = "resume_ended";
    await page.locator('[data-session="session-1"] [data-meta-attention]').waitFor({ timeout: sidebarWaitMS });
    const seeCard = async () => {
      await page.locator("[data-recovery-screen]").waitFor({ timeout: 12_000 });
      return page.evaluate(() => ({
        screen: document.querySelector("[data-recovery-screen]")?.textContent ?? "",
        head: document.querySelector("[data-recovery-scrollback]")?.textContent ?? "",
        register: [...document.querySelectorAll("[data-recovery-register]")]
          .some((el) => el.textContent.includes("no process")),
        retry: document.querySelector("[data-recovery-retry]")?.textContent?.trim(),
        fresh: document.querySelector("[data-recovery-fresh]")?.textContent?.trim(),
        terminalMounted: document.querySelector("[data-live-terminal]") !== null,
        rowListed: document.querySelector('[data-session="session-1"]') !== null,
      }));
    };
    const first = await seeCard();
    assert.match(first.screen, /No conversation found/, "the retained screen is not shown above the card");
    assert.match(first.head, /claude - exit 1 - resume ended/, "the resume-ended head line is missing");
    assert.equal(first.register, true, "the resume-ended register is missing");
    assert.equal(first.retry, "Resume");
    assert.equal(first.fresh, "Start fresh session");
    assert.equal(first.terminalMounted, false, "a resume-ended row mounted a terminal");
    assert.equal(first.rowListed, true, "the resume-ended row left the list");
    // Reopen the row: select a sibling, then come back.
    await page.locator('[data-session="session-2"]').click();
    await waitForTerminal(page, "OUTPUT session-2");
    await page.locator('[data-session="session-1"]').click();
    const again = await seeCard();
    assert.match(again.screen, /No conversation found/, "reopening the row lost the retained screen");
    // A refused Resume stays on the card with the server's sentence; the
    // row and its screen are still there.
    refuseNextRecover = true;
    const refusedLine = await armRecoveryError(page);
    await page.locator("[data-recovery-retry]").click();
    await refusedLine();
    assert.equal(recoverCalls, 4, "Resume on the resume-ended card did not post exactly one recover");
    assert.equal(await page.locator("[data-recovery-screen]").count(), 1, "the refusal removed the retained screen");
    sessionOneScreen = null;
    sessionOnePresentationState = "running";
    await waitForTerminal(page, "OUTPUT session-1");
  }

  // No composer/outbox UI exists: the terminal is the only input
  // surface. The ⋯ menu is presentation plus sign out; passive
  // session state and any authorized recovery gesture live in the titlebar.
  settingsReadGate = createGate();
  await page.locator("[data-live-more]").click();
  await page.locator("[data-more-popover]").waitFor();
  await page.locator("[data-live-logout]").focus();
  settingsReadGate.release();
  settingsReadGate = null;
  await page.locator('[data-skip-permissions][aria-disabled="true"]').waitFor({ state: "detached" });
  await waitForCondition(async () =>
    (await page.locator("[data-live-logout]").evaluate((el) => document.activeElement === el)),
  "unchanged settings resolving without moving focus");
  await page.keyboard.press("Escape");
  await page.locator("[data-more-popover]").waitFor({ state: "detached" });
  await page.locator("[data-live-more]").click();
  await page.locator("[data-more-popover]").waitFor();
  assert.equal(await page.locator("[data-more-popover] [data-harness-login]").count(), 0, "the menu offers a login nobody started");
  // The menu reads the console settings when it opens (the backend is the
  // authority), so wait for a toggle to reach the state that read produces.
  // Waiting on ":not([disabled])" would pass the moment the popover opened -
  // nothing in these rows carries a disabled state any more, so it observed
  // nothing.
  await waitForCondition(async () =>
    (await toggleChecked(page.locator("[data-confirm-before-closing]")))
      === "true", "settings read reaching the menu");
  // Keep real action targets through two ordinary polling/render cycles.
  // Unconditional menu replacement detaches both handles and loses focus.
  const retainedMoreMenu = await page.locator("[data-more-popover]").elementHandle();
  const retainedLogout = await page.locator("[data-more-popover] [data-live-logout]").elementHandle();
  const retainedFontIncrease = await page.locator("[data-more-popover] [data-font-inc]").elementHandle();
  assert.ok(retainedMoreMenu); assert.ok(retainedLogout); assert.ok(retainedFontIncrease);
  await retainedLogout.focus();
  try {
    for (let cycle = 1; cycle <= 2; cycle++) {
      if (cycle === 2) {
        const fontSize = Number(await page.locator("[data-font-size]").innerText());
        assert.ok(fontSize < 20, "font increase probe must change the font size");
        await retainedFontIncrease.click();
        await waitForCondition(async () =>
          Number(await page.locator("[data-font-size]").innerText()) === fontSize + 1,
        "font increase reaching the menu");
      }
      moreMenuRefreshCycle = cycle;
      const marker = `More menu refresh ${cycle}`;
      await page.waitForFunction(title =>
        document.querySelector("[data-titlebar-title]")?.textContent === title,
      marker, { timeout: 12_000 });
      assert.deepEqual(await page.evaluate(({ menu, logout, fontIncrease, focused }) => ({
        menuConnected: menu.isConnected,
        logoutConnected: logout.isConnected,
        fontIncreaseConnected: fontIncrease.isConnected,
        sameMenu: document.querySelector("[data-more-popover]") === menu,
        sameLogout: document.querySelector("[data-more-popover] [data-live-logout]") === logout,
        sameFontIncrease: document.querySelector("[data-more-popover] [data-font-inc]") === fontIncrease,
        movedFocusRetained: document.activeElement === focused,
      }), {
        menu: retainedMoreMenu, logout: retainedLogout, fontIncrease: retainedFontIncrease,
        focused: cycle === 1 ? retainedLogout : retainedFontIncrease,
      }), {
        menuConnected: true, logoutConnected: true, fontIncreaseConnected: true,
        sameMenu: true, sameFontIncrease: true,
        sameLogout: true, movedFocusRetained: true,
      }, "unchanged status render detached the More menu or stole moved focus");
    }
  } finally {
    moreMenuRefreshCycle = null;
    await retainedMoreMenu.dispose(); await retainedLogout.dispose(); await retainedFontIncrease.dispose();
  }
  const autoColor = page.locator("[data-auto-color]");
  assert.ok((await toggleChecked(autoColor)) === "false", "Auto-color projects starts off");
  await autoColor.focus();
  await page.keyboard.press("Space");
  await waitForCondition(async () => (await toggleChecked(autoColor)) === "true", "auto-color keyboard toggle");
  await autoColor.focus();
  await page.keyboard.press("ArrowDown");
  assert.match(await page.evaluate(() => document.activeElement?.outerHTML ?? ""), /data-show-side-panel/,
    "ArrowDown reaches the switch after Auto-color projects");
  await page.keyboard.press("End");
  assert.match(await page.evaluate(() => document.activeElement?.outerHTML ?? ""), /data-live-logout/,
    "End reaches the last menu row");
  await page.keyboard.press("Home");
  assert.equal(await page.evaluate(() => document.activeElement?.hasAttribute("data-theme-flyout")), true,
    "Home reaches the first menu row");

  const confirmBeforeClosing = page.locator("[data-confirm-before-closing]");
  const showInfoBar = page.locator("[data-show-info-bar]");
  assert.ok((await toggleChecked(confirmBeforeClosing)) === "true",
    "Confirm before closing starts on");
  assert.ok((await toggleChecked(showInfoBar)) === "true",
    "Show info bar starts on");
  await confirmBeforeClosing.focus();
  await page.keyboard.press("Enter");
  await waitForCondition(async () =>
    (await toggleChecked(page.locator("[data-confirm-before-closing]")))
      === "false", "confirm-before-closing setting write");
  assert.deepEqual(consoleSettings, {
    skip_permissions: false,
    skip_close_confirm: true,
    skip_remove_project_confirm: false,
  });

  await page.getByRole("button", { name: "Git identity…" }).click();
  // The comp's one-author anatomy: the Name field carries the default
  // identity's author name, the email rows carry the addresses.
  const identitiesDialog = page.getByRole("dialog", { name: "Git identity" });
  await identitiesDialog.waitFor();
  assert.equal(
    await identitiesDialog.locator("[data-git-identity-name]").inputValue(),
    "Deck smoke",
    "the Name field does not carry the default identity's author name",
  );
  const identitiesText = await identitiesDialog.innerText();
  assert.match(identitiesText, /Default/);
  assert.match(identitiesText, /Applies to projects you open next\./);
  await identitiesDialog.getByRole("button", { name: "Close" }).click();

  await page.locator("[data-live-more]").click();
  await page.locator("[data-more-popover]").waitFor();

  // Popover dismissal is click-away-ONLY: a click inside the panel
  // stays, a click on the backdrop closes it.
  await page.locator("[data-more-popover]").getByText("Terminal font", { exact: true }).click();
  await page.locator("[data-more-popover]").waitFor();
  await page.locator("[data-popover-backdrop]").click({ position: { x: 8, y: 8 } });
  await page.locator("[data-more-popover]").waitFor({ state: "detached" });


  // An abnormal outcome DOES render: the server closes the
  // attachment after a terminal outcome, and the overlay carries the
  // specific reason it gave rather than a generic disconnect. The info bar
  // is hints-only and never carries write outcomes.
  const terminalInput2 = page.locator("[data-live-terminal] .xterm-helper-textarea");
  await terminalInput2.focus();
  await page.keyboard.type("y");
  const overlay = page.locator("[data-live-terminal-overlay]");
  await waitForCondition(async () =>
    (await overlay.textContent())?.includes("stale generation") === true,
    "attachment overlay write outcome",
    5000,
  );
  const beforeB = sockets.length;
  await page.locator('[data-session="session-2"]').click();
  await waitForCondition(() => sockets.length > beforeB, "session B attachment");
  await waitForTerminal(page, "OUTPUT session-2");
  const beforeReturn = sockets.length;
  await page.locator('[data-session="session-1"]').click();
  await waitForCondition(() => sockets.length > beforeReturn, "session A reattachment");
  await waitForTerminal(page, "OUTPUT session-1");
  // …and the failed outcome does NOT survive the reattachment: a fresh
  // attachment renders no overlay at all.
  await waitForCondition(async () =>
    ((await overlay.textContent()) ?? "").trim() === "",
    "fresh attachment clearing the outcome overlay",
    5000,
  );
  await settleNeutral();
  assert.doesNotMatch(
    await readStatus(),
    outcomeText,
    "the failed outcome must not render for a fresh attachment",
  );
  assert.equal(
    new Set(sockets.map(({ controller }) => controller)).size,
    1,
    "A → B → A reuses one tab controller",
  );

  if (process.env.NOISEDECK_LIVE_SMOKE_SCREENSHOT) {
    await page.screenshot({
      path: process.env.NOISEDECK_LIVE_SMOKE_SCREENSHOT,
      fullPage: true,
    });
  }

  await page.setViewportSize({ width: 390, height: 720 });
  await assertMobileSheetMotion(page, "live composition");
  await assertMobileSheetMotion(page, "live composition", true);
  await assertSheetResize(page, "live composition");
  await page.setViewportSize({ width: 1280, height: 720 });
  assert.equal(await page.locator('[data-region="sidebar"]').evaluate((node) =>
    getComputedStyle(node).transitionDuration), "0s", "desktop sidebar must remain immediate");

  // A row leaves the active surface as soon as the backend no longer verifies
  // its attachment. The browser shows the normal landing and never offers or
  // sends a manual recovery request.
  sessionOneStatus = "interrupted";
  sessionOneRecoveryCapability = "resume";
  sessionOneRecoveryState = "unavailable";
  sessionOnePresentationState = "inactive";
  await page.getByText("No session open", { exact: true })
    .waitFor({ timeout: 12_000 });
  // With no session there is nothing to mount, and a dock with nothing in it
  // is a bordered bar drawn across the bottom of the pane for no reason. At
  // the phone width, where CSS paints the dock, it has to leave the layout
  // rather than merely empty out.
  await page.setViewportSize({ width: 390, height: 720 });
  const emptyDock = await page.locator('[data-region="terminal-dock"]').boundingBox();
  assert.ok(emptyDock === null || emptyDock.height === 0,
    `the phone landing draws a dock with nothing in it: ${JSON.stringify(emptyDock)}`);
  // The landing opens the session sheet on a phone; leave it closed so a
  // later step's click is not caught by its scrim.
  await page.setViewportSize({ width: 1280, height: 720 });
  await page.waitForFunction(() => {
    const scrim = document.querySelector('[data-region="sidebar-scrim"]');
    return scrim === null || getComputedStyle(scrim).display === "none";
  });
  // The phone settings sheet in the LIVE composition (comp mobile-r8 screen
  // 07). The preview shares the markup, but the live close handler is its
  // own wiring, and the live menu REMOUNTS when the deferred settings read
  // resolves — the X clicked here must be the remounted one. The sheet
  // drops the info-bar and side-panel rows (the phone renders neither
  // surface) and trims two labels to the phone copy.
  await page.setViewportSize({ width: 390, height: 720 });
  {
    const sheetScrim = page.locator('[data-region="sidebar-scrim"]');
    if (await sheetScrim.isVisible().catch(() => false)) {
      await sheetScrim.dispatchEvent("click");
    }
  }
  settingsReadGate = createGate();
  await page.locator("[data-live-more]").click();
  await page.locator("[data-more-popover]").waitFor();
  await waitForCondition(async () =>
    (await toggleChecked(page.locator("[data-confirm-before-closing]"))) !== null,
  "settings read remounting the phone sheet");
  const sheetTitle = page.locator("[data-settings-sheet-chrome] > span");
  assert.equal(await sheetTitle.isVisible(), true, "the live phone sheet has no header");
  assert.equal((await sheetTitle.innerText()).trim(), "Session settings", "live sheet header copy");
  for (const row of ["[data-show-info-bar]", "[data-show-side-panel]"]) {
    assert.equal(await page.locator(row).isVisible(), false,
      `the live phone sheet still renders ${row}`);
  }
  assert.equal(await page.locator("[data-skip-permissions] [data-copy-phone]").isVisible(), true,
    "the live phone sheet hides the trimmed yolo copy");
  assert.equal(await page.locator("[data-skip-permissions] [data-copy-wide]").isVisible(), false,
    "the live phone sheet leaks the wide yolo copy");
  await page.locator("[data-theme-flyout]").click();
  await page.locator("[data-theme-list]").waitFor();
  await page.keyboard.press("End");
  assert.equal(await page.evaluate(() => document.activeElement?.hasAttribute("data-theme-custom-open")), true,
    "End stays on the pushed theme screen");
  await page.keyboard.press("ArrowDown");
  assert.equal(await page.evaluate(() => document.activeElement?.hasAttribute("data-theme-back")), true,
    "ArrowDown wraps to the theme screen's Back control");
  await page.keyboard.press("ArrowUp");
  assert.equal(await page.evaluate(() => document.activeElement?.hasAttribute("data-theme-custom-open")), true,
    "ArrowUp wraps back to Custom");
  await page.keyboard.press("Home");
  assert.equal(await page.evaluate(() => document.activeElement?.hasAttribute("data-theme-back")), true,
    "Home reaches the theme screen's Back control");
  await page.keyboard.press("ArrowDown");
  assert.equal(await page.evaluate(() => document.activeElement?.hasAttribute("data-theme-close")), true,
    "ArrowDown reaches the theme screen's Close control");
  await page.keyboard.press("ArrowDown");
  assert.equal(await page.evaluate(() => document.activeElement?.hasAttribute("data-theme-pick")), true,
    "ArrowDown reaches a visible theme choice");
  await page.keyboard.press("End");
  settingsReadGate.release();
  settingsReadGate = null;
  await page.locator('[data-skip-permissions][aria-disabled="true"]').waitFor({ state: "detached" });
  assert.equal(await page.evaluate(() => document.activeElement?.hasAttribute("data-theme-custom-open")), true,
    "the settings response does not replay the old Back navigation intent");
  await page.keyboard.press("Escape");
  await page.locator("[data-theme-list]").waitFor({ state: "detached" });
  const phoneYolo = page.locator("[data-skip-permissions]");
  assert.ok((await toggleChecked(phoneYolo)) === "false", "phone yolo starts off");
  await phoneYolo.focus();
  await page.keyboard.press("Space");
  await page.locator("[data-yolo-confirm]").waitFor();
  assert.ok((await toggleChecked(phoneYolo)) === "false", "confirmation does not announce an uncommitted yolo change");
  assert.equal(await phoneYolo.getAttribute("aria-expanded"), "true");
  await page.locator("[data-yolo-cancel]").focus();
  await page.keyboard.press("Enter");
  assert.equal(await phoneYolo.getAttribute("aria-expanded"), "false");
  await confirmBeforeClosing.focus();
  await page.keyboard.press("Space");
  await waitForCondition(async () => (await toggleChecked(confirmBeforeClosing)) === "true",
    "phone confirmation toggle keyboard write");
  await confirmBeforeClosing.focus();
  await page.keyboard.press("Space");
  await waitForCondition(async () => (await toggleChecked(confirmBeforeClosing)) === "false",
    "phone confirmation toggle restores the original setting");
  await page.locator("[data-settings-sheet-close]").click();
  await page.locator("[data-more-popover]").waitFor({ state: "detached" });
  assert.equal(
    await page.locator("[data-live-more]").evaluate((el) => document.activeElement === el),
    true, "the live sheet X did not return focus to More");
  // The focused trigger and the pointer resting where the X was both paint
  // hints into the info bar at the desktop width this run returns to; the
  // next step asserts that bar empty.
  await page.locator("[data-live-more]").evaluate((el) => el.blur());
  await page.mouse.move(5, 400);
  await page.setViewportSize({ width: 1280, height: 720 });
  await page.waitForFunction(() => {
    const scrim = document.querySelector('[data-region="sidebar-scrim"]');
    return scrim === null || getComputedStyle(scrim).display === "none";
  });
  // Dismissing the phone sheet collapsed the sidebar; the desktop steps
  // below expect the rail they left open.
  await expandSidebar(page);
  await page.locator('[data-workspace][data-collapsed="false"]').waitFor();
  await page.evaluate(() => { document.activeElement?.blur?.(); });
  await page.mouse.move(640, 680);

  // The info bar mirrors the switch: the session hint it was showing is
  // cleared, not left stale on the session that just left the active surface.
  await waitForCondition(async () => (await readStatus()) === "",
    "info bar clears after the selection drops", 5000);
  assert.equal(await infoStatus.getAttribute("data-mode"), "hint");
  await waitForCondition(async () => await page.locator('[data-session="session-1"]').count() === 0,
    "a non-running row leaving the active sidebar");
  assert.equal(await page.locator("[data-live-terminal]").count(), 0,
    "a non-running selection is disposed instead of becoming a dead terminal");
  assert.equal(await page.locator('[data-region="terminal"] [data-live-session-metadata]').count(), 0,
    "the landing contains no reconciliation metadata");

  // Automatic provider recovery happens behind the browser. Its successful
  // verified generation reappears as an ordinary row and opens only after the
  // user selects it; no special recovery endpoint is involved.
  sessionOneStatus = "running";
  sessionOneRecoveryCapability = "none";
  sessionOneRecoveryState = "unavailable";
  sessionOnePresentationState = "running";
  await page.locator('[data-session="session-1"]').waitFor({ timeout: sidebarWaitMS });
  const beforeRecovery = sockets.length;
  await page.locator('[data-session="session-1"]').click();
  await waitForCondition(() => sockets.length > beforeRecovery, "automatic recovery result attachment", 12_000);
  await waitForTerminal(page, "OUTPUT session-1");

  // Any other selected row follows the same rule: once its verified attachment
  // is gone, both the row and terminal are removed with no dead-end state.
  await page.locator('[data-session="session-2"]').click();
  await waitForTerminal(page, "OUTPUT session-2");
  sessionTwoStatus = "exited";
  sessionTwoPresentationState = "ended";
  await page.getByText("No session open", { exact: true })
    .waitFor({ timeout: 12_000 });
  await waitForCondition(async () => await page.locator('[data-session="session-2"]').count() === 0,
    "an ended row leaving the active sidebar");
  assert.equal(await page.locator("[data-live-terminal]").count(), 0,
    "an ended selection leaves no empty terminal shell");
  sessionTwoStatus = "running";
  sessionTwoPresentationState = "running";
  await page.locator('[data-session="session-2"]').waitFor({ timeout: sidebarWaitMS });

  // A row whose detail cannot be verified is also cleared back to the normal
  // landing. The failed read must not leave a selected row over an error pane.
  sessionTwoDetailFailures = 1;
  await page.locator('[data-session="session-2"]').click();
  await page.getByText("No session open", { exact: true })
    .waitFor({ timeout: 12_000 });
  assert.equal(await page.getByText("session detail unavailable", { exact: true }).count(), 0,
    "a detail read failure does not mount a selected error state");
  // An active row is a neutral lift AND a promoted name, so clearing the
  // selection has to clear both. Checking only one of them would still pass
  // with the other left behind. The class repaint can lag the landing text
  // by a render, so the truth is awaited, not sampled.
  const clearedRowState = () => page.locator('[data-session="session-2"]').evaluate((row) => ({
    lifted: row.classList.contains("bg-wash-pressed"),
    named: (row.querySelector("span.flex-1")?.className ?? "").includes("text-text-primary"),
  }));
  await waitForCondition(async () => {
    const state = await clearedRowState();
    return state.lifted === false && state.named === false;
  }, "a detail read failure clearing the selected row", 5000);
  assert.deepEqual(await clearedRowState(), { lifted: false, named: false },
    "a detail read failure clears the selected row and its promoted name");

  await page.locator('[data-session="session-1"]').click();
  await waitForTerminal(page, "OUTPUT session-1");

  // Provider labels are lowercase display copy; command-code names itself
  // "command" everywhere, the picker included. The inventory now renders as
  // wrapping chips, not a fixed grid, so every admitted provider is visible
  // from the start in the same order for every project.
  // The row's + is hover-revealed (pointer-events-none until its header is
  // hovered or focused). The pointer must rest on the header itself — the
  // list item's centre is a session row below it, and Playwright's pre-click
  // hit test never moves the pointer when the target cannot receive it.
  await page.locator("[data-project-toggle]").first().hover();
  await page.locator("[data-new-session]").first().click();
  await page.locator("[data-new-session-modal]").waitFor();
  const servedProviders = await page.evaluate(async (origin) => {
    const resp = await fetch(`${origin}/api/providers`, { credentials: "same-origin" });
    if (!resp.ok) throw new Error(`GET /api/providers ${resp.status}`);
    return resp.json();
  }, origin);
  assert.deepEqual(servedProviders.providers, HARNESS_INVENTORY,
    "this run's mock serves the harness inventory this probe preconditions");
  const expectedProviderSet = new Set(servedProviders.providers);
  const pickerText = await page
    .locator("[data-new-session-provider]")
    .evaluateAll((buttons) => buttons.map((b) => b.textContent ?? ""));
  assert.ok(pickerText.length > 0, "new-session picker exposed providers");
  for (const text of pickerText) {
    assert.ok(!/[A-Z]/.test(text), `provider picker text "${text}" must be lowercase`);
  }
  const renderedProviders = await page
    .locator("[data-new-session-provider]")
    .evaluateAll((buttons) => buttons.map((b) => b.dataset.newSessionProvider ?? ""));
  if (expectedProviderSet.has("command-code")) {
    assert.equal(await page.locator('[data-new-session-provider="command-code"]').getByText("command", { exact: true }).count(), 1,
      "command-code keeps its API key while rendering as command in the picker");
  }
  if (expectedProviderSet.has("devin")) {
    assert.equal(await page.locator('[data-new-session-provider="devin"]').getByText("devin", { exact: true }).count(), 1,
      "the picker exposes the admitted Devin provider");
  }
  assert.equal(await page.locator("[data-new-session-more]").count(), 0,
    "no overflow cell remains in the chip layout");
  assert.equal(await page.locator("[data-new-session-recommendation]").count(), 0,
    "history does not move a harness out of the complete list");
  assert.deepEqual(renderedProviders, servedProviders.providers,
    "the picker renders every served provider once in served order");
  const expectedChipSet = expectedProviderSet;
  // Force the next provider fetch to fail so the post-create refresh leaves
  // the inventory stale. The browser walk below then retries back to served.
  providersFail = true;
  const providerFilter = page.locator("[data-new-session-filter]");
  if (expectedProviderSet.has("mimo")) {
    await providerFilter.fill("mimo");
    assert.equal(await page.locator('[data-new-session-provider="mimo"]:visible').count(), 1,
      "the picker searches the admitted MiMo Code provider");
    // Searching only proves the row renders. Submitting it must reach the
    // backend as the exact create request the admission gate owns; a dropped
    // admission would leave the row visible while every launch failed.
    await page.locator('[data-new-session-provider="mimo"]').click();
    await waitForCondition(() => mimoCreate !== null, "MiMo session create request");
    // The create carries the last fitted terminal geometry so the session's
    // window is born at this screen's size, not tmux's 80x24.
    // The exact numbers are the viewport's; the contract is the shape and
    // the server's admitted bounds.
    assert.equal(mimoCreate.provider, "mimo");
    assert.ok(Number.isInteger(mimoCreate.cols) && mimoCreate.cols >= 20 && mimoCreate.cols <= 500,
      `create cols out of bounds: ${mimoCreate.cols}`);
    assert.ok(Number.isInteger(mimoCreate.rows) && mimoCreate.rows >= 5 && mimoCreate.rows <= 300,
      `create rows out of bounds: ${mimoCreate.rows}`);
    assert.deepEqual(Object.keys(mimoCreate).sort(), ["cols", "provider", "rows"]);
    // Creating a session is the strongest give-me-the-terminal gesture:
    // once the new session attaches, keyboard focus must be in its input
    // field, not left on the picker that launched it.
    await page.locator('[data-region="terminal"][data-terminal-provider="mimo"] .xterm-helper-textarea')
      .waitFor();
    await waitForCondition(() => page.evaluate(() =>
      document.activeElement?.classList.contains("xterm-helper-textarea") ?? false),
    "created session moves focus to its terminal input", 5000);
    await page.locator("[data-project-toggle]").first().hover();
    // The dialog closed on submit; reopen it for the pending-launch flow.
    await page.locator("[data-new-session]").first().click();
    await page.locator("[data-new-session-modal]").waitFor();
    // The MiMo create's post-create refresh hit the forced provider failure,
    // so the picker is now stale: it keeps the last good chips and shows a
    // retry banner. Clicking retry while still failing stays stale; retrying
    // once the server recovers returns to served.
    await page.locator("[data-new-session-inventory-unread]").waitFor();
    assert.equal(await page.locator("[data-new-session-provider]").count(), expectedChipSet.size,
      "stale inventory keeps every last-good chip on screen");
    // The retry wiring must be prompt and attributable to the click. The
    // sidebar's own 10-second refresh timer can also recover the inventory, so
    // this probe asserts the signal only the listener produces: the button
    // enters its loading state synchronously on click. Holding the mock
    // response keeps that state observable without relying on the timer.
    // The gate and the recovery flip together, so a request already on its
    // way cannot succeed before the click.
    providersSuccessGate = createGate();
    providersFail = false;
    await page.locator("[data-new-session-inventory-retry]").click();
    await page.locator("[data-new-session-inventory-retrying]").waitFor({ timeout: 5000 });
    providersSuccessGate.release();
    providersSuccessGate = null;
    await page.locator("[data-new-session-inventory-unread]").waitFor({ state: "detached" });
    assert.equal(await page.locator("[data-new-session-inventory-unread]").count(), 0,
      "retry recovers the inventory to served");
  } else {
    console.log("skipping MiMo launch: not in served inventory");
  }
  await providerFilter.fill("qoder");
  assert.equal(await page.locator('[data-new-session-provider="qoder"]:visible').count(), 1,
    "the picker searches the admitted Qoder provider");
  await page.locator('[data-new-session-provider="qoder"]').click();
  await waitForCondition(() => qoderCreate !== null, "Qoder session create request");
  assert.equal(qoderCreate.provider, "qoder");
  assert.deepEqual(Object.keys(qoderCreate).sort(), ["cols", "provider", "rows"]);
  await page.locator("[data-project-toggle]").first().hover();
  // The dialog closed on submit; reopen it for the next probe.
  await page.locator("[data-new-session]").first().click();
  await page.locator("[data-new-session-modal]").waitFor();
  await providerFilter.fill("cursor");
  assert.equal(await page.locator('[data-new-session-provider="cursor"]:visible').count(), 1,
    "the picker searches the admitted Cursor Agent provider");
  await page.locator('[data-new-session-provider="cursor"]').click();
  await waitForCondition(() => cursorCreate !== null, "Cursor session create request");
  assert.equal(cursorCreate.provider, "cursor");
  assert.deepEqual(Object.keys(cursorCreate).sort(), ["cols", "provider", "rows"]);
  await page.locator("[data-project-toggle]").first().hover();
  // The dialog closed on submit; reopen it for the next probe.
  await page.locator("[data-new-session]").first().click();
  await page.locator("[data-new-session-modal]").waitFor();
  await providerFilter.fill("grok");
  assert.equal(await page.locator('[data-new-session-provider="grok"]:visible').count(), 1,
    "the picker searches the admitted Grok Build provider");
  await page.locator('[data-new-session-provider="grok"]').click();
  await waitForCondition(() => grokCreate !== null, "Grok session create request");
  assert.equal(grokCreate.provider, "grok");
  assert.deepEqual(Object.keys(grokCreate).sort(), ["cols", "provider", "rows"]);
  await page.locator("[data-project-toggle]").first().hover();
  // The dialog closed on submit; reopen it for the next probe.
  await page.locator("[data-new-session]").first().click();
  await page.locator("[data-new-session-modal]").waitFor();
  await providerFilter.fill("terminal");
  assert.equal(await page.locator("[data-new-session-provider]:visible").count(), 1,
    "the picker filters its full provider inventory");
  await providerFilter.press("Enter");
  await waitForCondition(() => createStarted, "session create request");
  assert.equal(await page.locator('[data-new-session-provider="shell"]:visible').count(), 1,
    "the searched provider stays visible while its launch is pending");
  assert.equal(await page.locator('[data-new-session-spinner="shell"]:visible').count(), 1,
    "the searched provider shows its own launch spinner");
  // The rail holds only session rows — a populated group renders no
  // in-group New session chrome.
  const sessionRows = page.locator(`#sessions-${project.id} > li`);
  const pendingRow = page.locator('[data-pending-session="shell"]');
  await pendingRow.waitFor();
  assert.equal(await pendingRow.evaluate((row) => Array.from(row.parentElement.children).indexOf(row)), 0,
    "the pending launch occupies the server's newest-first slot");
  providersSuccessGate = createGate();
  resolveCreate();
  const createdRow = page.locator('[data-session="session-created"]');
  await createdRow.waitFor();
  await pendingRow.waitFor({ state: "detached", timeout: 5000 });
  providersSuccessGate.release();
  providersSuccessGate = null;
  assert.equal(await createdRow.evaluate((row) => Array.from(row.parentElement.children).indexOf(row)), 0,
    "the authoritative row replaces its placeholder without moving");
  assert.equal(await sessionRows.count(), summaries.length, "the refresh retains every existing sibling");

  // A handler can reject after the record is already retired. The browser
  // confirms the authoritative list before surfacing that rejection, so the
  // removed row disappears without a false error flash.
  const rejectedRow = page.locator('[data-session="session-8"]');
  await rejectedRow.hover();
  await rejectedRow.locator('[data-session-menu="session-8"]').click();
  await page.locator("[data-session-menu-close]").click();
  await waitForCondition(() => rejectedRemoval !== null, "rejected removal request");
  await rejectedRow.waitFor({ state: "detached" });
  assert.equal(await page.getByText("could not remove session", { exact: true }).count(), 0,
    "a durably retired row surfaced the handler's stale failure");

  // skip_remove_project_confirm is no longer exposed in the More menu (task
  // 308); set it directly on the mocked backend and open the menu once so
  // the settings re-read picks it up. The sidebar then bypasses only its
  // dialog — the same authoritative DELETE still runs and drives refresh.
  consoleSettings.skip_remove_project_confirm = true;
  await page.locator("[data-live-more]").click();
  // Wait for the re-read to reach the menu: skip_remove_project_confirm was
  // just set, so the confirm-before-removing row must settle to off before
  // dismissing.
  await waitForCondition(async () =>
    (await toggleChecked(page.locator("[data-confirm-before-removing]")))
      === "false", "remove-confirm setting reaching the menu");
  await page.locator("[data-popover-backdrop]").click({ position: { x: 8, y: 8 } });
  await page.locator("[data-more-popover]").waitFor({ state: "detached" });
  await page.locator("[data-project-toggle]").first().hover();
  await page.locator("[data-project-menu]").first().click();
  const unconfiguredPreview = page.locator("[data-project-menu-popover] [data-project-menu-preview-settings]");
  await unconfiguredPreview.getByText("Not set up", { exact: true }).waitFor();
  assert.equal(await unconfiguredPreview.getByText("Preview…", { exact: true }).count(), 1,
    "an unconfigured installation offers preview setup");
  assert.equal(await page.locator("[data-project-menu-preview]").count(), 0,
    "an unconfigured installation must not offer Open preview");
  await page.locator("[data-project-menu-remove]").click();
  await waitForCondition(() => projectRemoved, "project removal request");
  assert.equal(await page.locator("[data-remove-project-confirm]").count(), 0,
    "skip_remove_project_confirm must bypass the dialog");

  // Sign out stays reachable after project removal: the account menu
  // owns the row, and each open re-reads console settings from the
  // backend (the authority), never cache. The removal's deselect fires
  // one more refresh that closes any open popover (disposeSession), so
  // wait for the settled empty registry before opening the menu — the
  // row cannot survive a click that lands before that redraw.
  await page.locator('[data-empty-pane="no-project"]').waitFor();
  await page.locator("[data-new-project-row]").waitFor();
  const logout = page.locator("[data-more-popover] [data-live-logout]");
  await page.locator("[data-live-more]").click();
  await logout.waitFor();
  // Sign-out hard-navigates: wait for the reload, then the reloaded page
  // answers its session check expired and shows the sign-in surface.
  await Promise.all([
    page.waitForNavigation({ waitUntil: "domcontentloaded" }),
    logout.click(),
  ]);
  await page.getByRole("link", { name: "Sign in with Google" }).waitFor();
  assert.equal(await page.locator("[data-region]").count(), 0, "logout removes all application chrome");
  await waitForCondition(() => logoutRequest !== null, "logout request");
  await waitForCondition(() => closedSockets === sockets.length, "no live socket after logout");

  // Reload stands in for completing a fresh Google login. The same browser
  // tab must reclaim its controller, and a later API 401 must tear down the
  // already-authenticated WebSocket before showing the login action.
  projectRemoved = false;
  registered = true;
  // The logout mock above expired the session for the post-logout reload;
  // the fresh login this reload stands in for is authenticated again.
  authExpired = false;
  // The "Show info bar" preference persists across boots: with it off, a
  // fresh boot reads data-shown=false and the strip leaves the layout.
  await page.evaluate(() => localStorage.setItem("noisedeck-show-info-bar", "off"));
  await page.reload({ waitUntil: "networkidle" });
  await waitForTerminal(page, "OUTPUT session-created");
  const infoBarOff = page.locator('[data-region="info-bar"]');
  assert.equal(await infoBarOff.getAttribute("data-shown"), "false",
    "a persisted off preference boots the strip hidden");
  assert.equal(await infoBarOff.evaluate((el) => getComputedStyle(el).display), "none",
    "a hidden info bar leaves the layout entirely");
  assert.equal((await infoBarOff.boundingBox())?.height ?? 0, 0,
    "a hidden info bar consumes no vertical space");
  await page.evaluate(() => localStorage.removeItem("noisedeck-show-info-bar"));
  assert.equal(new Set(sockets.map(({ controller }) => controller)).size, 1);
  const closedBeforeExpiry = closedSockets;
  authExpired = true;
  await page.getByRole("link", { name: "Sign in with Google" }).waitFor({ timeout: 12_000 });
  assert.equal(await page.locator("[data-region]").count(), 0, "expiry removes all application chrome");
  await waitForCondition(
    () => closedSockets > closedBeforeExpiry,
    "socket close after authentication expiry",
  );
  // Runtime failures in the page (uncaught exceptions on any lifecycle
  // path, including dispose after auth teardown) fail the smoke.
  assert.deepEqual(pageErrors, [], "no uncaught page errors");
  await context.close();
} catch (error) {
  if (serverOutput !== "") process.stderr.write(serverOutput);
  if (browserDiagnostics.length > 0) {
    process.stderr.write(`${browserDiagnostics.join("\n")}\n`);
  }
  if (page !== undefined && !page.isClosed()) {
    process.stderr.write(`browser body:\n${await page.locator("body").innerText()}\n`);
  }
  throw error;
} finally {
  await browser?.close();
  if (vite.exitCode === null && vite.signalCode === null) {
    vite.kill("SIGTERM");
    await new Promise((resolve) => vite.once("exit", resolve));
  }
}

async function freePort() {
  const server = net.createServer();
  await new Promise((resolve, reject) => {
    server.once("error", reject);
    server.listen(0, "127.0.0.1", resolve);
  });
  const address = server.address();
  assert.notEqual(address, null);
  assert.equal(typeof address, "object");
  const selected = address.port;
  await new Promise((resolve) => server.close(resolve));
  return selected;
}

async function waitForServer(url) {
  const deadline = Date.now() + 15_000;
  while (Date.now() < deadline) {
    try {
      const response = await fetch(url);
      if (response.ok) return;
    } catch {
      // Vite is still starting.
    }
    await new Promise((resolve) => setTimeout(resolve, 100));
  }
  throw new Error(`Vite did not become ready at ${url}`);
}

async function waitForTerminal(page, expected) {
  await page.waitForFunction(
    (text) => document.querySelector("[data-live-terminal] .xterm-rows")
      ?.textContent?.includes(text) === true,
    expected,
  );
}

// The offset from the pane to the painted cell grid. The host element
// ([data-live-terminal]) fills the pane's padded box, so measuring to it
// only re-reads the pane's own padding and is blind to anything inside the
// host — exactly where a grid narrower than its host leaves ground beside
// a provider's painted output.
async function terminalGaps(page) {
  // Null while a selection hands over between mounts: the grid from the
  // previous session is gone and the next has not opened yet. A caller in
  // a wait loop treats null as not settled rather than an error.
  return page.locator('[data-region="terminal"]').evaluate((pane) => {
    const grid = pane.querySelector('[data-live-terminal] .xterm-screen');
    if (grid === null) return null;
    const outer = pane.getBoundingClientRect();
    const inner = grid.getBoundingClientRect();
    return { left: inner.left - outer.left, right: outer.right - inner.right };
  });
}

async function expandSidebar(page) {
  const workspace = page.locator("[data-workspace]");
  if (await workspace.getAttribute("data-collapsed") !== "true") return;
  // The phone has no rail to re-expand: the session list is a sheet opened
  // from the titlebar, and the desktop's Show sidebar control is out of the
  // layout below the phone boundary. Reaching for it there waits forever.
  const sessions = page.locator("[data-live-sessions]");
  const expand = await sessions.isVisible()
    ? sessions
    : page.getByRole("button", { name: "Show sidebar" });
  await expand.waitFor();
  await expand.click();
  await page.waitForFunction(() => {
    const sidebar = document.querySelector('[data-region="sidebar"]');
    return sidebar instanceof HTMLElement && sidebar.getBoundingClientRect().left >= -1;
  });
}

async function toggleChecked(row) {
  assert.equal(await row.getAttribute("role"), "switch");
  return await row.getAttribute("aria-checked");
}

// The card's error line lasts until the next status poll repaints it, so a
// recorder installed before the click keeps the text the line showed. Returns
// a function that resolves with that text.
async function armRecoveryError(page) {
  await page.evaluate(() => {
    window.__recoveryErrorObserver?.disconnect();
    window.__recoveryError = null;
    const record = () => {
      const line = document.querySelector("[data-recovery-error]");
      const shown = line !== null
        && line.checkVisibility({ visibilityProperty: true })
        && line.getClientRects().length > 0;
      if (shown && line.innerText !== "") {
        window.__recoveryError = line.innerText;
      }
    };
    window.__recoveryErrorObserver = new MutationObserver(record);
    window.__recoveryErrorObserver.observe(document.body, {
      subtree: true, childList: true, characterData: true, attributes: true,
    });
  });
  return async () => {
    const seen = await page.waitForFunction(() => window.__recoveryError);
    return seen.jsonValue();
  };
}

function createGate() {
  let release;
  const promise = new Promise((resolve) => { release = resolve; });
  return { promise, release };
}

async function waitForCondition(condition, label, timeout = 10_000) {
  const deadline = Date.now() + timeout;
  while (Date.now() < deadline) {
    if (await condition()) return;
    await new Promise((resolve) => setTimeout(resolve, 20));
  }
  throw new Error(`timed out waiting for ${label}`);
}
