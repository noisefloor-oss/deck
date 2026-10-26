// Deterministic browser probe for the Files panel (tasks 117 and 156).
//
// This is NOT the live smoke. live-smoke.mjs drives the whole production
// composition end to end and is known to be flaky on constrained hosts;
// this harness answers a narrow set of questions that only a real engine
// can answer — measured geometry, computed type, the requests the panel
// issues, and hover timing — against the BUILT page with a mocked
// authenticated API. Every assertion here is deterministic: no wall-clock
// content, no network, and no reliance on the terminal's own output.
//
// Usage: pnpm build && node tools/files-panel-probe.mjs
// Requires: dist/ built, and a browser — the agent-browser sidecar via
// NOISE_BROWSER_WS, or a local one (see tools/browser.mjs).
import assert from "node:assert/strict";
import { assertFontVoice } from "./font-assertions.mjs";
import { createServer } from "node:http";
import { readFile } from "node:fs/promises";
import { existsSync } from "node:fs";
import { extname, join, resolve } from "node:path";

if (process.env.PLAYWRIGHT_BROWSERS_PATH === undefined &&
    existsSync("/workspace/tools/ms-playwright")) {
  process.env.PLAYWRIGHT_BROWSERS_PATH = "/workspace/tools/ms-playwright";
}
const { browser: getBrowser } = await import("./browser.mjs");
const { eventually, settled } = await import("./eventually.mjs");

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
const LIST_W = 320;
const TERMINAL_MIN_W = 360;

const project = {
  id: "project-1",
  name: "noisedeck",
  canonical_path: "/workspace/noisedeck",
  last_used_at: "2026-08-01T00:00:00Z",
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

const README = "# noisedeck\n\nA terminal-native console.\nline four\n";
const LONG_FILE_NAME = `${"long-file-name-".repeat(12)}notes.md`;
const LONG_DIR_NAME = `${"deeply-nested-directory-".repeat(6)}leaf`;
const MOBILE_TEXT = Array.from(
  { length: 120 },
  (_, index) => `${String(index + 1).padStart(3, "0")} ${"mobile viewer content ".repeat(8)}`,
).join("\n");

const entry = (path, dir, size, change) => ({
  name: path.slice(path.lastIndexOf("/") + 1),
  path,
  dir,
  size,
  mode: dir ? "rwxr-xr-x" : "rw-r--r--",
  mtime: "2026-08-01T00:00:00Z",
  ...(change === undefined ? {} : { change }),
});

// The listing the mock serves, in the server's own order (directories
// first, then by name). Uploads append to the root listing so a refresh
// shows what the server "stored".
const listings = {
  "": [
    entry("docs", true, 4096, "modified"),
    entry("x", true, 4096),
    entry("broken", true, 4096),
    entry("deep", true, 4096),
    entry(LONG_DIR_NAME, true, 4096),
    entry("README.md", false, README.length, "modified"),
    entry(LONG_FILE_NAME, false, README.length),
    entry("logo.tiff", false, 18422, "new"),
  ],
  "docs": [entry("docs/architecture.md", false, 331)],
  "x": [entry("x/y", true, 4096)],
  "x/y": [],
  "deep": [entry("deep/field-notes", true, 4096)],
  "deep/field-notes": [entry("deep/field-notes/archive6", true, 4096)],
  "deep/field-notes/archive6": [entry("deep/field-notes/archive6/sessions", true, 4096)],
  "deep/field-notes/archive6/sessions": [],
  [LONG_DIR_NAME]: [],
};

let filesEventsSeenResolve;
const filesEventsSeen = new Promise((resolve) => { filesEventsSeenResolve = resolve; });
let releaseFilesChanged;
const filesChanged = new Promise((resolve) => { releaseFilesChanged = resolve; });
let filesChangedDelivered = false;

const uploads = [];
const heldUploads = new Map();
let githubAttempt = null;
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
let context;
const pageErrors = [];
const pathOnlyComplete = Symbol("path-only-complete");
try {
  const { browser: got, via } = await getBrowser();
  browser = got;
  console.log(`browser: ${via}`);
  context = await browser.newContext({ viewport: { width: 1400, height: 900 } });
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
    const query = url.searchParams.get("path") ?? "";
    const json = (status, body) =>
      route.fulfill({ status, contentType: "application/json", body: JSON.stringify(body) });

    if (request.method() === "GET" && path === "/api/projects") return json(200, [project]);
    if (request.method() === "GET" && path === "/api/git-identities") return json(200, {
      profiles: [{ id: "identity-1", name: "Deck probe", email: "deck@example.invalid", revision: 1, created_at: "t", updated_at: "t" }],
      default_profile_id: "identity-1", revision: 1,
    });
    if (request.method() === "GET" && path === `/api/projects/${project.id}/sessions`) {
      return json(200, [session]);
    }
    if (request.method() === "GET" && path === `/api/sessions/${session.id}`) {
      return json(200, session);
    }
    if (request.method() === "GET" && path === "/api/settings/last-session") return json(404, { code: "not_found", message: "no last session remembered" });
    if (request.method() === "PUT" && path === "/api/settings/last-session") return json(200, request.postDataJSON());
    if (request.method() === "GET" && path === "/api/settings/layout") {
      return json(404, { code: "not_found", message: "no layout saved" });
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
    if (request.method() === "GET" && path === "/api/session-activity/events") {
      return route.fulfill({
        status: 200,
        contentType: "text/event-stream",
        body: ": connected\n\nretry: 3600000\n\n",
      });
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
    if (request.method() === "GET" && path === `/api/projects/${project.id}/files/events`) {
      filesEventsSeenResolve();
      // Exactly one files-changed ever flows: the first connection holds
      // until the probe releases it, then delivers the event. Every later
      // connection — a per-directory resubscribe or a reconnect — is a
      // silent stream, and the pinned retry keeps the client from redialing
      // under the geometry assertions further down.
      if (filesChangedDelivered) {
        return route.fulfill({
          status: 200,
          contentType: "text/event-stream",
          body: ": connected\n\nretry: 3600000\n\n",
        });
      }
      filesChangedDelivered = true;
      await filesChanged;
      return route.fulfill({
        status: 200,
        contentType: "text/event-stream",
        body: ": connected\n\nretry: 3600000\n\nevent: files-changed\ndata: changed\n\n",
      });
    }
    if (request.method() === "GET" && path === `/api/projects/${project.id}/files`) {
      if (["docs", "x", "x/y", "broken"].includes(query)) {
        await new Promise((resolve) => setTimeout(resolve, 150));
      }
      const entries = listings[query];
      if (entries === undefined) {
        return json(404, { error: "not_found", message: "no such file in the project" });
      }
      return json(200, { path: query, at_root: query === "", entries });
    }
    if (request.method() === "GET" && path === `/api/projects/${project.id}/files/view`) {
      if (query === "README.md" || query === LONG_FILE_NAME) return json(200, { content: README });
      if (query === "docs/architecture.md") return json(200, { content: "# Architecture\n" });
      if (query === "notes.txt") return json(200, { content: "hello\n" });
      if (query === "mobile-scroll.txt") {
        await new Promise((resolve) => setTimeout(resolve, 150));
        return json(200, { content: MOBILE_TEXT });
      }
      return json(415, { error: "not_text", message: "file is not text" });
    }
    if (request.method() === "POST" && path === `/api/projects/${project.id}/files`) {
      assert.equal(request.headers()["x-csrf-token"], "probe-csrf");
      assert.equal(
        request.headers()["content-type"],
        "application/octet-stream",
        "the raw-body path, not multipart",
      );
      const body = request.postData();
      if (query?.startsWith("progress-")) {
        const expected = url.searchParams.get("expected_version");
        if (query.startsWith("progress-replace-") && expected === null) {
          return json(409, { error: "conflict", reason: "exists", message: "destination exists",
            existing: { name: query, dir: false, size: 4, mtime: "2026-09-23T03:00:00Z", version: "progress-original" } });
        }
        await new Promise((resolve) => heldUploads.set(query, resolve));
        return route.fulfill({ status: 201, contentType: "application/json", body: JSON.stringify({ path: query, size: body?.length ?? 0 }) }).catch(() => {});
      }

      // A collision whose answer waits until the probe releases it, so
      // another screen can open before the ask arrives.
      if (query.startsWith("held-conflict-") && url.searchParams.get("expected_version") === null) {
        await new Promise((resolve) => heldUploads.set(query, resolve));
        return json(409, { error: "conflict", reason: "exists", message: "destination exists",
          existing: { name: query, dir: false, size: 4, mtime: "2026-09-23T03:00:00Z", version: "held-original" } });
      }
      if (query === "slow-mobile.txt") await new Promise((resolve) => setTimeout(resolve, 150));
      const expected = url.searchParams.get("expected_version");
      uploads.push({ path: query, body, expected });
      if (query === "reject.bin") {
        return json(413, { error: "too_large", message: "upload is too large" });
      }
      if (query === "intro.md") {
        return json(409, {
          error: "conflict", message: "intro.md: destination already exists", reason: "exists",
          existing: { name: "intro.md", dir: false, size: 700, mtime: "2026-09-23T03:00:00Z", version: "intro-owner" },
        });
      }
      // The collision the server reports for README.md: a plain pick meets
      // the taken name; Replace on the version the ask carried meets a
      // target that changed underneath it; Replace on THAT version lands.
      if (query === "README.md") {
        if (expected === null) {
          return json(409, {
            error: "conflict",
            message: "README.md: destination already exists",
            reason: "exists",
            existing: {
              name: "README.md", dir: false, size: README.length,
              mtime: "2026-09-23T03:00:00Z", version: "1-probe-a",
            },
          });
        }
        if (expected === "1-probe-a") {
          return json(409, {
            error: "conflict",
            message: "README.md: destination changed since replacement was approved",
            reason: "changed",
            existing: {
              name: "README.md", dir: false, size: 4100,
              mtime: "2026-09-23T04:00:00Z", version: "1-probe-b",
            },
          });
        }
      }
      // A folder holding the name: never replaceable, ask or not.
      if (query === "docs") {
        return json(409, {
          error: "conflict",
          message: "docs: a folder holds this name",
          reason: "directory",
          existing: {
            name: "docs", dir: true, size: 4096,
            mtime: "2026-09-23T03:00:00Z", version: "1-probe-dir",
          },
        });
      }
      // A stored name replaces its row, the way the server's listing does,
      // so a replaced file does not appear twice.
      listings[""] = [
        ...listings[""].filter((existing) => existing.path !== query),
        entry(query, false, body?.length ?? 0),
      ];
      return json(201, { path: query, size: body?.length ?? 0 });
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
    if (request.method() === "GET" && path === "/api/github/connect/status") return json(200, { outcome: "no-account", active: false });
    if (request.method() === "GET" && path === "/api/github/connect/attempt") return json(200, { csrf: "connect-fixture", attempt: githubAttempt });
    // Opening Connect GitHub with no account starts a sign-in, and opening it
    // again follows that same sign-in.
    if (request.method() === "POST" && path === "/api/github/connect/start") {
      githubAttempt = { id: "connect-attempt", status: "running", generation: 1, started_at: "2026-01-01T00:00:00Z" };
      return json(200, githubAttempt);
    }
    // The closed Assets panel reads the listing and follows its feed for its badge.
    if (/^\/api\/projects\/[^/]+\/assets$/.test(path)) return route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ locations: [], scanned: "2026-01-01T00:00:00Z", preview_max_bytes: 16777216 }) });
    if (/^\/api\/projects\/[^/]+\/assets\/events$/.test(path)) return route.fulfill({ status: 200, contentType: "text/event-stream", body: ": connected\n\nretry: 3600000\n\n" });
    throw new Error(`unhandled ${request.method()} ${path}`);
  });

  const socketFrames = [];
  await page.routeWebSocket(`${origin.replace("http", "ws")}/api/sessions/*/terminal?*`, (socket) => {
    socket.onMessage((message) => {
      if (typeof message === "string") socketFrames.push(JSON.parse(message));
    });
    socket.send(JSON.stringify({ type: "attached", token: 1, generation: 1 }));
    socket.send(JSON.stringify({
      type: "output",
      data: Buffer.from("READY\r\n").toString("base64"),
    }));
  });

  await page.goto(origin, { waitUntil: "networkidle" });
  await page.locator("[data-live-terminal]").waitFor();

  // ---- the trigger ----
  const filesButton = page.getByRole("button", { name: "Files", exact: true });
  const tasksButton = page.locator("[data-live-tasks]");
  await filesButton.waitFor();
  const order = await page.evaluate(() => {
    const bar = document.querySelector('[data-region="terminal-titlebar"]');
    const buttons = [...bar.querySelectorAll("button")].map((b) => b.dataset.liveFiles !== undefined
      ? "files"
      : b.dataset.liveTasks !== undefined
      ? "tasks"
      : "other");
    return buttons.filter((b) => b !== "other");
  });
  assert.deepEqual(order, ["files", "tasks"], "Files sits before Tasks in the top bar");
  assert.equal(await filesButton.getAttribute("aria-pressed"), "false");

  const panel = page.locator('[data-region="files"]');
  const waitForPanelState = async (locator, open, label) => {
    await locator.evaluate(async (element) => {
      await new Promise((resolve) => requestAnimationFrame(() => requestAnimationFrame(resolve)));
      await Promise.allSettled(element.getAnimations().map((animation) => animation.finished));
    });
    await eventually(
      () => locator.evaluate((element, expectedOpen) => {
        if (element.getAttribute("data-open") !== String(expectedOpen)) return false;
        const style = getComputedStyle(element);
        if (!expectedOpen) return style.visibility === "hidden";
        const offset = style.transform === "none"
          ? 0
          : new DOMMatrixReadOnly(style.transform).m41;
        return style.visibility === "visible" && Math.abs(offset) < 0.5;
      }, open),
      Boolean,
      label,
      3000,
    );
  };
  assert.equal(await panel.isVisible(), false, "the panel starts closed");

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
  // A viewport change is answered asynchronously: the app re-lays out and the
  // panel takes its new size a frame or more later. This page reports nothing
  // when it is done, so the measurement is the only signal there is — wait for
  // the content row, which the panel sizes itself from, to stop moving, and
  // read after that. A fixed delay instead measures the clock, and under load
  // reads a layout mid-flight.
  const setViewport = async (size, label) => {
    await page.setViewportSize(size);
    await page.waitForFunction(
      ([w, h]) => window.innerWidth === w && window.innerHeight === h,
      [size.width, size.height],
    );
    await settled(
      async () => JSON.stringify(await page.locator('[data-region="content"]').boundingBox()),
      `${label} layout`,
    );
  };
  const near = (actual, expected, what, tolerance = 0.5) =>
    assert.ok(
      Math.abs(actual - expected) < tolerance,
      `${what}: ${actual} is not ${expected}`,
    );

  // ---- docked geometry ----
  const closedFilesBox = await box(panel);
  await filesButton.click();
  // A docked panel takes its space immediately - it is a split, not an
  // overlay - so there is no mid-flight position to sample. The comp declares
  // three transitions and all of them are opacity on hover-revealed controls;
  // neither it nor the panel animates its margin. This used to sample the
  // panel 60ms in and require the reading to fall between the endpoints, which
  // only held if a slide were in progress, so it failed and masked every
  // assertion after it.
  await waitForPanelState(panel, true, "the Files panel opens"); 
  const openFilesBox = await box(panel);
  near(Math.abs(closedFilesBox.x - openFilesBox.x), openFilesBox.width,
    "the Files panel gives up exactly one panel width beside the terminal");
  await page.getByRole("button", { name: "README.md" }).and(page.locator("[data-files-row]")).waitFor();
  assert.equal(await filesButton.getAttribute("aria-pressed"), "true");

  // ---- shared panel resizing ----
  const handle = page.locator('[data-panel-resize="files"]');
  await handle.waitFor();
  const savedWidth = () => page.evaluate(() => localStorage.getItem("noisedeck.panel.files.width"));
  assert.equal(await savedWidth(), null, "no width is saved before the user resizes");
  assert.equal(
    await panel.evaluate((element) => element.style.getPropertyValue("--docked-panel-w")),
    "",
    "an untouched panel keeps the stylesheet's responsive width",
  );
  // Dragging the boundary right narrows the panel by exactly the drag; the
  // default already sits at the row's cap, so widening first would clamp.
  const beforeDrag = await box(panel);
  const startX = beforeDrag.x + 4;
  await page.mouse.move(startX, beforeDrag.y + 100);
  await page.mouse.down();
  await page.mouse.move(startX + 80, beforeDrag.y + 100, { steps: 6 });
  await page.mouse.up();
  const narrowed = await box(panel);
  near(narrowed.width, beforeDrag.width - 80, "an 80px right drag narrows the panel 80px");
  assert.equal(await savedWidth(), String(Math.round(beforeDrag.width - 80)), "the drag commits the width");
  assert.equal(
    await panel.evaluate((element) => element.style.getPropertyValue("--docked-panel-w")),
    `${Math.round(beforeDrag.width - 80)}px`,
    "the user width is the panel's inline override",
  );
  // Keyboard: ArrowLeft widens by one step, Enter returns the stylesheet
  // default and clears the saved preference.
  await handle.focus();
  await page.keyboard.press("ArrowLeft");
  const stepped = await box(panel);
  near(stepped.width, narrowed.width + 16, "ArrowLeft widens by one keyboard step");
  await page.keyboard.press("Enter");
  const reset = await box(panel);
  near(reset.width, beforeDrag.width, "Enter returns the stylesheet's responsive width");
  assert.equal(await savedWidth(), null, "Enter clears the saved width");
  const ariaNow = () => handle.evaluate((element) => element.getAttribute("aria-valuenow"));
  near(Number(await ariaNow()), reset.width, "the separator reports the rendered width, not zero", 1.5);
  // A saved width survives a phone crossing without leaking into it: the
  // phone stylesheet owns the full-bleed panel, and the desktop override
  // comes back when the window widens again.
  await page.mouse.move(reset.x + 4, reset.y + 100);
  await page.mouse.down();
  await page.mouse.move(reset.x + 84, reset.y + 100, { steps: 6 });
  await page.mouse.up();
  assert.equal(await savedWidth() !== null, true, "a second drag saves again");
  await setViewport({ width: 430, height: 900 }, "phone with a saved width");
  assert.equal(
    await panel.evaluate((element) => element.style.getPropertyValue("--docked-panel-w")),
    "",
    "the saved desktop width never overrides the phone panel",
  );
  await setViewport({ width: 1400, height: 900 }, "back to desktop");
  assert.equal(
    (await panel.evaluate((element) => element.style.getPropertyValue("--docked-panel-w"))).endsWith("px"),
    true,
    "the saved width is restored on the desktop",
  );
  await handle.focus();
  await page.keyboard.press("Enter");
  assert.equal(await savedWidth(), null, "reset clears the saved width for the legs that follow");
  // Escape cancels a drag even when focus never reached the separator:
  // the drag's own pointerdown prevents that focus change, so the cancel
  // path cannot depend on the handle holding focus.
  await filesButton.focus();
  const beforeEscape = await box(panel);
  await page.mouse.move(beforeEscape.x + 4, beforeEscape.y + 100);
  await page.mouse.down();
  await page.mouse.move(beforeEscape.x + 84, beforeEscape.y + 100, { steps: 6 });
  await page.keyboard.press("Escape");
  await page.mouse.up();
  const afterEscape = await box(panel);
  near(afterEscape.width, beforeEscape.width, "Escape restores the pre-drag width");
  assert.equal(await savedWidth(), null, "an escaped drag saves nothing");
  // A width a narrow row forced below the preferred minimum survives a
  // reload at a wide row, clamped to what the wide row allows.
  await setViewport({ width: 900, height: 900 }, "narrow desktop row");
  await handle.focus();
  await page.keyboard.press("End");
  const narrowSaved = await savedWidth();
  assert.equal(narrowSaved !== null && Number(narrowSaved) < 400, true, "End on a narrow row saves below the preferred minimum");
  await setViewport({ width: 1400, height: 900 }, "widened again");
  await page.reload({ waitUntil: "networkidle" });
  await filesButton.waitFor();
  await filesButton.click();
  await waitForPanelState(panel, true, "the Files panel reopens after reload");
  assert.equal(await savedWidth(), narrowSaved, "the forced narrow width survives the reload");
  const afterReload = await box(panel);
  near(afterReload.width, 400, "the wide row clamps the forced narrow width to its minimum", 1.5);
  await handle.focus();
  await page.keyboard.press("Enter");
  assert.equal(await savedWidth(), null, "reset clears the width for the legs that follow");

  const content = page.locator('[data-region="content"]');
  const terminalCard = page.locator('[data-region="terminal-card"]');
  const contentBox = await box(content);
  const panelBox = await box(panel);
  const terminalBox = await box(terminalCard);
  near(contentBox.x, SIDEBAR_W, "the content row starts right of the sidebar");
  // clamp(620px, 56%, 880px) of the content row, capped at 100% - 360px.
  const expectedWidth = Math.min(
    Math.min(Math.max(620, contentBox.width * 0.56), 880),
    contentBox.width - TERMINAL_MIN_W,
  );
  near(panelBox.width, expectedWidth, "the docked panel's width");
  near(panelBox.x + panelBox.width, contentBox.x + contentBox.width, "the panel's right edge");
  near(panelBox.height, contentBox.height, "the panel fills the content height");
  near(terminalBox.width, contentBox.width - panelBox.width, "the terminal keeps the rest");
  assert.ok(
    terminalBox.width >= TERMINAL_MIN_W,
    `the terminal column is ${terminalBox.width}, below its ${TERMINAL_MIN_W}px floor`,
  );
  assert.equal(
    ["absolute", "fixed"].includes(await panel.evaluate((el) => getComputedStyle(el).position)),
    false,
    "the docked panel takes layout space rather than covering the terminal",
  );
  near(
    await panel.evaluate((el) => Number.parseFloat(getComputedStyle(el).borderLeftWidth)),
    1,
    "the left seam",
  );
  assert.equal(
    await page.evaluate(() =>
      document.documentElement.scrollWidth <= document.documentElement.clientWidth),
    true,
    "the docked panel must not push the page into horizontal overflow",
  );

  // The header sits on the top bar's row and shares its hairline.
  const topBar = page.locator('[data-region="terminal-titlebar"]').locator("xpath=..");
  const panelHeader = panel.locator("[data-files-header]");
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

  // ---- the header's copy, in the handoff's voices ----
  assert.equal(await panel.locator("[data-files-header] h2").count(), 0, "the trail is the title, no 'Files' label");
  const crumbs = panel.getByRole("navigation", { name: "Project path" });
  assert.equal((await crumbs.innerText()).replace(/\s+/g, " ").trim(), "noisedeck");
  const crumbFont = await crumbs.evaluate((el) => {
    const s = getComputedStyle(el);
    return { family: s.fontFamily, size: s.fontSize };
  });
  await assertFontVoice(page, crumbFont.family, "ui", `breadcrumb font: ${crumbFont.family}`);
  assert.equal(
    await panel.getByRole("button", { name: ".." }).count(),
    0,
    "no control walks above the project root",
  );

  // ---- the list ----
  near((await box(panel.locator("[data-files-list]"))).width, LIST_W, "the list column is exactly 320px");
  const rowNames = await panel.locator("[data-files-row]").evaluateAll((rows) =>
    rows.map((r) => r.querySelector(".min-w-0.flex-1").textContent.trim()));
  assert.deepEqual(
    rowNames,
    ["docs/", "x/", "broken/", "deep/", `${LONG_DIR_NAME}/`, "README.md", LONG_FILE_NAME, "logo.tiff"],
    "directories first, server order",
  );
  const markers = await panel.locator("[data-files-row]").evaluateAll((rows) =>
    rows.map((r) => {
      const icon = r.querySelector("[data-files-change]");
      return icon === null ? null : [r.getAttribute("data-files-row"), icon.getAttribute("data-files-change"), getComputedStyle(icon).color, r.getAttribute("data-hint")];
    }).filter((marker) => marker !== null));
  assert.deepEqual(
    markers,
    [["docs", "modified", "rgb(217, 164, 65)", "Modified · Open this folder"], ["README.md", "modified", "rgb(217, 164, 65)", "Modified · Open this file"], ["logo.tiff", "new", "rgb(79, 178, 124)", "New · Open this file"]],
    "modified rows are orange and new rows are green, with status words in the hint",
  );
  const rowFont = await panel.locator("[data-files-row]").first()
    .evaluate((el) => {
      const s = getComputedStyle(el.querySelector(".min-w-0.flex-1"));
      return { family: s.fontFamily, size: s.fontSize };
    });
  await assertFontVoice(page, rowFont.family, "ui", `row name font: ${rowFont.family}`);
  assert.equal(rowFont.size, "13px", "row names are the interface face");
  const rowIcon = await panel.locator("[data-files-row] svg").first()
    .evaluate((el) => el.getBoundingClientRect().width);
  near(rowIcon, 14, "list icons are 14px");

  // ---- the viewer ----
  await page.getByRole("button", { name: "README.md" }).and(page.locator("[data-files-row]")).click();
  const text = panel.locator("[data-files-text]");
  await text.waitFor();
  assert.equal(
    await text.evaluate((el) => el.textContent),
    README,
    "the file's own text, verbatim",
  );
  assert.equal(
    await text.locator("[data-files-line-number]").evaluateAll((gutters) =>
      gutters.map((gutter) => gutter.getAttribute("data-line-number")).join("\n")
    ),
    "1\n2\n3\n4\n5",
    "the trailing newline is a line, and the gutter says so",
  );
  const bodyType = await text.evaluate((el) => {
    const s = getComputedStyle(el);
    return { family: s.fontFamily, size: s.fontSize, line: s.lineHeight };
  });
  await assertFontVoice(page, bodyType.family, "mono", `viewer font: ${bodyType.family}`);
  assert.equal(bodyType.size, "12px");
  near(Number.parseFloat(bodyType.line), 12 * 1.62, "viewer line-height 1.62", 0.2);
  const gutter = await panel.locator("[data-files-line-number]").first().evaluate((el) => {
    const s = getComputedStyle(el);
    return {
      sticky: s.position,
      left: s.left,
      opacity: s.opacity,
      selectable: s.userSelect,
      rule: s.borderRightWidth,
      bg: s.backgroundColor,
    };
  });
  assert.equal(gutter.sticky, "sticky", "the number gutter sticks to the viewer's left edge");
  assert.equal(gutter.left, "0px");
  assert.equal(gutter.opacity, "0.5", "the gutter reads at half opacity");
  assert.equal(gutter.selectable, "none", "the numbers never enter a selection");
  assert.equal(gutter.rule, "1px", "the gutter ends in a hairline");
  assert.notEqual(gutter.bg, "rgba(0, 0, 0, 0)", "the gutter shares the sunken body behind it");
  assert.equal(
    await panel.locator("[data-files-line-number]").first().evaluate((el) => el.textContent),
    "",
    "the label is a pseudo-element: the numbers are not in the DOM text",
  );
  assert.equal(
    await text.evaluate((el) => el.querySelector("[data-files-line]")?.textContent),
    README.split("\n")[0],
    "the row's own text is exactly the line's bytes",
  );

  // The footer is the machine register: kind · lines · size · recency ·
  // read-only, with the real wrap toggle beside it.
  const footerStatus = panel.locator("[data-files-viewer-status]");
  assert.match(await footerStatus.innerText(), /^markdown · 5 lines · .* · modified .* ago · read-only$/,
    "the footer reads as the status line");
  const wrapToggle = panel.getByRole("button", { name: "Wrap lines" });
  assert.equal(await wrapToggle.getAttribute("aria-pressed"), "true", "wrapping defaults on");
  const wrapDot = await wrapToggle.locator("span").first().evaluate((el) => ({
    size: el.getBoundingClientRect().width,
    color: getComputedStyle(el).backgroundColor,
  }));
  near(wrapDot.size, 6, "the wrap state dot is 6px", 0.5);
  assert.notEqual(wrapDot.color, "rgba(0, 0, 0, 0)", "the wrap dot is painted");
  const footerType = await panel.locator("[data-files-viewer-footer]").evaluate((el) => {
    const s = getComputedStyle(el);
    return { family: s.fontFamily, size: s.fontSize };
  });
  await assertFontVoice(page, footerType.family, "mono", `footer font: ${footerType.family}`);
  assert.equal(footerType.size, "11px", "the footer status is the mono micro register");
  assert.equal(
    await panel.locator("textarea, input:not([type=file]), [contenteditable=true]").count(),
    0,
    "the viewer is read-only: nothing in the panel edits a file",
  );

  const selectedRow = panel.locator('[data-files-row="README.md"]');
  assert.equal(await selectedRow.getAttribute("aria-current"), "true");
  const [selectedWash, plainWash] = await Promise.all([
    selectedRow.evaluate((el) => getComputedStyle(el).backgroundColor),
    panel.locator('[data-files-row="logo.tiff"]')
      .evaluate((el) => getComputedStyle(el).backgroundColor),
  ]);
  assert.notEqual(selectedWash, plainWash, "the selected row carries its own wash");
  assert.notEqual(selectedWash, "rgba(0, 0, 0, 0)");

  const download = panel.getByRole("link", { name: "Download file" });
  assert.equal(
    await download.getAttribute("href"),
    `/api/projects/${project.id}/files/download?path=README.md`,
    "the download is a real GET at the download endpoint",
  );
  const desktopShortName = await panel.locator("[data-files-viewer-name]").evaluate((el) => {
    const header = el.closest("[data-files-viewer-header]");
    const actions = header?.querySelector("[data-files-viewer-actions]");
    if (!(actions instanceof HTMLElement)) return null;
    const nameBox = el.getBoundingClientRect();
    const headerBox = header.getBoundingClientRect();
    const actionBox = actions.getBoundingClientRect();
    return {
      clipped: el.scrollWidth - el.clientWidth > 1,
      nameWidth: nameBox.width,
      nameScrollWidth: el.scrollWidth,
      nameToActionsGap: actionBox.left - nameBox.right,
      actionsContained: actionBox.right <= headerBox.right,
      actionsRightAligned: Math.abs((headerBox.right - actionBox.right) - Number.parseFloat(getComputedStyle(header).paddingRight)) < 1,
    };
  });
  assert.equal(
    desktopShortName?.clipped,
    false,
    `a short desktop filename keeps its intrinsic width: ${JSON.stringify(desktopShortName)}`,
  );
  assert.ok(
    Math.abs((desktopShortName?.nameWidth ?? 0) - (desktopShortName?.nameScrollWidth ?? -2)) <= 1,
    "the short filename hit box stays at its intrinsic text width",
  );
  // The comp keeps a margin-left:auto spacer, so the gap is flexible; what
  // the reference fixes is the actions' right alignment inside the header.
  assert.ok((desktopShortName?.nameToActionsGap ?? -1) > 0, "the spacer keeps filename and actions apart");
  assert.equal(desktopShortName?.actionsContained, true, "the actions stay inside the header");
  assert.ok(desktopShortName?.actionsRightAligned !== false, "the actions sit at the header's right edge");

  await page.getByRole("button", { name: LONG_FILE_NAME }).click();
  await panel.locator(`[data-files-viewer-scroll][data-files-path="${LONG_FILE_NAME}"]`).waitFor();
  const desktopLongName = await panel.locator("[data-files-viewer-name]").evaluate((el) => {
    const header = el.closest("[data-files-viewer-header]");
    const actions = header?.querySelector("[data-files-viewer-actions]");
    if (!(header instanceof HTMLElement) || !(actions instanceof HTMLElement)) return null;
    const nameBox = el.getBoundingClientRect();
    const headerBox = header.getBoundingClientRect();
    const actionBox = actions.getBoundingClientRect();
    return {
      clipped: el.scrollWidth > el.clientWidth,
      separated: nameBox.right <= actionBox.left,
      actionsContained: actionBox.right <= headerBox.right,
      headerContained: header.scrollWidth <= header.clientWidth,
    };
  });
  assert.deepEqual(desktopLongName, {
    clipped: true,
    separated: true,
    actionsContained: true,
    headerContained: true,
  }, "a long desktop filename truncates before the fixed viewer actions");
  const desktopViewerName = panel.locator("[data-files-viewer-name]");
  assert.equal((await desktopViewerName.innerText()).trim(), LONG_FILE_NAME);
  assert.equal(
    await desktopViewerName.getAttribute("data-hint"),
    LONG_FILE_NAME,
    "the info bar hint preserves the full long filename",
  );
  assert.equal(await panel.getByRole("link", { name: "Download file" }).isVisible(), true);
  assert.equal(await panel.getByRole("button", { name: "Expand" }).isVisible(), true);
  await page.getByRole("button", { name: "README.md" }).and(page.locator("[data-files-row]")).click();
  await panel.locator('[data-files-viewer-scroll][data-files-path="README.md"]').waitFor();

  // ---- the terminal stays live beside the panel ----
  const beforeInput = socketFrames.length;
  await page.locator("[data-live-terminal] .xterm-helper-textarea").focus();
  await page.keyboard.type("z");
  await eventually(
    () => socketFrames.slice(beforeInput).some((f) =>
      f.type === "input" && Buffer.from(f.data, "base64").toString() === "z"),
    Boolean,
    "the terminal accepts input while the Files panel is open",
    3000,
  );
  await filesEventsSeen;
  await page.evaluate(() => {
    document.querySelector("[data-live-terminal] .xterm-helper-textarea")?.focus();
  });
  const focusBeforeFilesEvent = await page.evaluate(() => ({
    tag: document.activeElement?.tagName,
    inPanel: document.querySelector("[data-region='files']")?.contains(document.activeElement),
  }));
  releaseFilesChanged();
  await page.waitForTimeout(50);
  const focusAfterFilesEvent = await page.evaluate(() => ({
    tag: document.activeElement?.tagName,
    inPanel: document.querySelector("[data-region='files']")?.contains(document.activeElement),
  }));
  assert.equal(focusBeforeFilesEvent.tag, "TEXTAREA", "terminal owns focus before the background event");
  assert.equal(focusBeforeFilesEvent.inPanel, false, "terminal focus starts outside Files");
  assert.equal(focusAfterFilesEvent.tag, "TEXTAREA", "a background refresh preserves terminal focus");
  assert.equal(focusAfterFilesEvent.inPanel, false, "a background refresh does not move focus into Files");

  // ---- hints (the info bar is the only explanation channel) ----
  assert.equal(
    await panel.getByRole("link", { name: "Download file" }).getAttribute("data-hint"),
    "Download this file",
    "the download control exposes its hint",
  );
  assert.equal(
    await panel.getByRole("button", { name: "Expand" }).getAttribute("data-hint"),
    "Expand the panel to fill the window",
    "the expand control exposes its hint",
  );

  const uploadHint = await panel
    .getByRole("button", { name: "Upload files" })
    .getAttribute("data-hint");
  assert.equal(uploadHint, "Upload files into this folder",
    "the upload control exposes its hint");

  // name is the icon's aria-label; the word went icon-only when the trail
  // became the title, and its hint is verb-first.
  assert.equal(
    (await panel.getByRole("button", { name: "Upload files" }).innerText()).trim(),
    "",
    "the upload action is icon-only; the trail took its width",
  );

  // Pending transfers are inspectable and cancellable without choosing their
  // destination again. The separate HTTP probe measures real byte events.
  for (const [width, gesture, surface] of [1400, 390].flatMap((width) => ["double-click", "keyboard"].flatMap((gesture) => ["row", "details"].map((surface) => [width, gesture, surface])))) {
    await page.setViewportSize({ width, height: 900 });
    if (width === 390 && gesture === "double-click" && surface === "row" && await page.locator("[data-sidebar-sheet-close]").isVisible()) await page.locator("[data-sidebar-sheet-close]").click();
    console.log(`upload progress probe: width ${width}, ${gesture}, ${surface}`);
    const name = width === 1400 ? `progress-replace-${gesture}-${surface}.txt` : `progress-mobile-${gesture}-${surface}.txt`;
    await panel.locator("[data-files-input]").setInputFiles({ name, mimeType: "text/plain", buffer: Buffer.from("pending transfer") });
    if (width === 1400) {
      await page.getByRole("alertdialog").waitFor();
      await page.getByRole("button", { name: "Replace", exact: true }).click();
      await page.getByRole("alertdialog").waitFor({ state: "hidden" });
    }
    const row = panel.locator("[data-files-upload-row]").filter({ hasText: name });
    await row.waitFor({ timeout: 10000 });
    if (surface === "details") await row.locator("[data-files-upload-select]").click({ timeout: 10000 });
    const detail = width === 390 ? page.getByRole("dialog", { name }) : panel.locator("[data-files-upload-details]");
    if (surface === "details") {
      await detail.waitFor({ timeout: 10000 });
      assert.match(await detail.innerText(), /noisedeck\/ · 1 of 1/);
    }
    if (width === 390 && surface === "details") {
      const rect = await box(detail);
      near(rect.x, 0, "upload sheet spans the mobile viewport", 1);
      near(rect.width, width, "upload sheet fits the mobile viewport", 1);
      await detail.getByRole("button", { name: "Cancel upload", exact: true }).focus();
      await page.keyboard.press("Tab");
      assert.equal(await page.evaluate(() => document.activeElement?.getAttribute("aria-label")), "Close upload details", "Tab stays in the upload sheet");
    }
    const cancel = surface === "row" ? row.locator('[data-action="cancel"]')
      : detail.getByRole("button", { name: "Cancel upload", exact: true });
    if (gesture === "double-click") {
      const rect = await box(cancel);
      await page.mouse.dblclick(rect.x + rect.width / 2, rect.y + rect.height / 2);
    } else {
      await cancel.focus();
      await page.keyboard.press("Enter");
      await page.keyboard.press("Enter");
      await page.keyboard.press("Space");
    }
    assert.equal(await row.count(), 1, "repeated cancellation input cannot dismiss the uncertain record");
    assert.notEqual(await page.evaluate(() => document.activeElement?.getAttribute("data-action")), "dismiss",
      "cancellation never focuses a new Dismiss control");
    if (surface === "row" && !(await detail.isVisible())) await row.locator("[data-files-upload-select]").click();
    await detail.locator("[data-files-upload-state]").filter({ hasText: "Outcome unknown" }).waitFor();
    if (surface === "details" && gesture === "keyboard") {
      assert.equal(await page.evaluate(() => document.activeElement?.hasAttribute("data-files-upload-state")), true,
        "repeated keys leave focus on the outcome");
    }
    assert.match(await detail.innerText(), /may have been written/);
    heldUploads.get(name)?.();
    heldUploads.delete(name);
    await detail.getByRole("button", { name: "Close upload details" }).click();
    assert.equal(await row.isVisible(), true, "uncertain upload survives closing details");
    assert.equal(await row.locator('[data-action="dismiss"]').count(), 0,
      "the row never replaces Cancel with Dismiss under the pointer");
    await row.locator("[data-files-upload-select]").click();
    await detail.getByRole("button", { name: "Dismiss", exact: true }).click();
    await row.waitFor({ state: "hidden" });
  }
  await page.setViewportSize({ width: 1400, height: 900 });
  if (await page.getByRole("button", { name: "Show sidebar" }).isVisible()) await page.getByRole("button", { name: "Show sidebar" }).click();

  const focusUpload = "progress-replace-focus.txt";
  await panel.locator("[data-files-input]").setInputFiles({ name: focusUpload, mimeType: "text/plain", buffer: Buffer.from("pending replacement") });
  await page.getByRole("alertdialog").waitFor();
  await page.getByRole("button", { name: "Replace", exact: true }).click();
  await page.getByRole("alertdialog").waitFor({ state: "hidden" });
  await eventually(() => heldUploads.has(focusUpload), Boolean, "the replacement is awaiting its response");
  const terminalInput = page.locator("[data-live-terminal] .xterm-helper-textarea");
  await terminalInput.focus();
  const terminalHandle = await terminalInput.elementHandle();
  heldUploads.get(focusUpload)();
  heldUploads.delete(focusUpload);
  await eventually(() => panel.locator("[data-files-upload-header]").isDisabled(), (disabled) => !disabled, "the replacement settles");
  assert.equal(await terminalHandle.evaluate((element) => document.activeElement === element), true,
    "a later upload render cannot revive the consumed fallback focus request");
  await terminalHandle.dispose();
  const retained = panel.locator("[data-files-upload-row]").filter({ hasText: focusUpload });
  await retained.locator("[data-files-upload-select]").click();
  await panel.locator("[data-files-upload-details]").getByRole("button", { name: "Dismiss", exact: true }).click();

  // Upload details selected on the desktop become a sheet on the phone. A
  // resize under Connect GitHub pushes that sheet onto GitHub: Back returns
  // to GitHub, and the sheet's close control closes the whole modal.
  {
    const name = "progress-stacked.txt";
    const github = page.locator("[data-github-connect-dialog]");
    const sheet = page.getByRole("dialog", { name });
    const row = panel.locator("[data-files-upload-row]").filter({ hasText: name });
    await panel.locator("[data-files-input]").setInputFiles({ name, mimeType: "text/plain", buffer: Buffer.from("stacked\n") });
    await eventually(() => heldUploads.has(name), Boolean, "the stacked upload awaits its response");
    const sheetOverGithub = async () => {
      await page.setViewportSize({ width: 1400, height: 900 });
      await row.locator("[data-files-upload-select]").click();
      await panel.locator("[data-files-upload-details]").waitFor();
      await page.locator("[data-live-more]").click();
      await page.getByRole("button", { name: "GitHub connection…", exact: true }).click();
      await github.waitFor();
      await page.setViewportSize({ width: 390, height: 900 });
      await sheet.waitFor();
      assert.equal(await github.isVisible(), false, "the sheet steps in front of Connect GitHub");
      assert.equal(await page.locator("[data-modal-scrim]").count(), 1, "one scrim holds both screens");
      assert.equal(await sheet.locator(".dialog-head [data-modal-back]").count(), 1, "the late-pushed sheet has a Back control");
    };

    await sheetOverGithub();
    await sheet.locator("[data-modal-back]").click();
    await sheet.waitFor({ state: "detached" });
    await github.waitFor({ state: "visible" });
    assert.equal(await page.locator("[data-modal-scrim]").count(), 1, "Back leaves Connect GitHub open");
    await github.locator(".dialog-head [data-dialog-close]").click();
    await github.waitFor({ state: "detached" });

    await sheetOverGithub();
    await sheet.getByRole("button", { name: "Close upload details" }).click();
    await sheet.waitFor({ state: "detached" });
    await github.waitFor({ state: "detached" });
    assert.equal(await page.locator("[data-modal-scrim]").count(), 0, "the sheet's close control closes the whole modal");

    await page.setViewportSize({ width: 1400, height: 900 });
    heldUploads.get(name)();
    heldUploads.delete(name);
    await eventually(() => panel.locator("[data-files-upload-header]").isDisabled(), (disabled) => !disabled, "the stacked upload settles");
    if (await row.count() > 0) {
      await row.locator("[data-files-upload-select]").click();
      await panel.locator("[data-files-upload-details]").getByRole("button", { name: "Dismiss", exact: true }).click();
      await row.waitFor({ state: "hidden" });
    }
    if (await page.getByRole("button", { name: "Show sidebar" }).isVisible()) await page.getByRole("button", { name: "Show sidebar" }).click();
  }

  // ---- upload ----
  const before = uploads.length;
  await panel.locator("[data-files-input]").setInputFiles([
    { name: "notes.txt", mimeType: "text/plain", buffer: Buffer.from("hello\n") },
    { name: "reject.bin", mimeType: "application/octet-stream", buffer: Buffer.from("xx") },
  ]);
  await panel.getByText("reject.bin: upload is too large").waitFor();
  assert.deepEqual(
    uploads.slice(before).map((u) => u.path),
    ["notes.txt", "reject.bin"],
    "one raw-body request per picked file, named by its own basename",
  );
  assert.equal(uploads[before].body, "hello\n", "the file's bytes are the request body");
  await page.getByRole("button", { name: "notes.txt" }).and(page.locator("[data-files-row]")).waitFor();
  assert.equal(
    await panel.locator('[data-files-row="reject.bin"]').count(),
    0,
    "a file that failed to upload is never listed",
  );
  assert.equal(
    await panel.locator("[data-files-input]").inputValue(),
    "",
    "the picker is cleared so the same file can be chosen again",
  );

  // ---- upload collisions ----
  const dialog = page.getByRole("alertdialog");
  const nameField = page.locator("[data-files-conflict-name]");
  const conflictError = page.locator("[data-files-conflict-name-error]");
  const conflictEls = (selector) => page.locator(`[data-files-conflict] ${selector}`);

  // One clean file and one collision in the same pick: the clean file
  // lands, the collision becomes the ask, and nothing else is disturbed.
  let batch = uploads.length;
  await panel.locator("[data-files-input]").setInputFiles([
    { name: "README.md", mimeType: "text/plain", buffer: Buffer.from("replacement readme\n") },
    { name: "fresh.txt", mimeType: "text/plain", buffer: Buffer.from("clean file\n") },
  ]);
  await dialog.waitFor();
  assert.deepEqual(
    uploads.slice(batch).map((u) => u.path),
    ["README.md", "fresh.txt"],
    "the colliding file is held for the ask while the clean file lands",
  );
  assert.equal(
    await page.locator("#files-conflict-title").innerText(),
    "README.md already exists",
    "the ask's title names the incoming file",
  );
  // The ask mounts in the workspace's modals layer, not inside the docked
  // panel: the panel is a transformed stacking context, so a dialog drawn
  // in it would dim and center within the panel instead of the screen.
  const scrimBox = await box(page.locator("[data-modal-scrim]"));
  const dialogBox = await box(page.locator("[data-files-conflict]"));
  near(scrimBox.x, 0, "the scrim starts at the viewport's left edge", 1);
  near(scrimBox.y, 0, "the scrim starts at the viewport's top edge", 1);
  near(scrimBox.width, 1400, "the scrim spans the viewport's width", 1);
  near(scrimBox.height, 900, "the scrim spans the viewport's height", 1);
  near(dialogBox.x + dialogBox.width / 2, 700, "the dialog centers on the viewport", 1.5);
  const scrimPaint = await page.locator("[data-modal-scrim]")
    .evaluate((el) => getComputedStyle(el).backgroundColor);
  assert.notEqual(scrimPaint, "rgba(0, 0, 0, 0)", "the scrim actually dims what it covers");
  assert.ok(await page.locator("[data-files-conflict-icon]").isVisible(), "the head carries the warning icon");
  assert.equal(await page.locator("[data-files-conflict-close]").getAttribute("aria-label"), "Close",
    "the head carries an accessible close control");
  assert.match(
    await page.locator("#files-conflict-desc").innerText(),
    /^in noisedeck\/ · 1 of 1$/,
    "the ask names the folder, the position, and the choice it wants",
  );
  // The comparison reads in two aligned columns: the label column carries
  // the sides, the value column the facts.
  const chipGrid = await conflictEls(".dialog-body .grid").evaluate((el) => getComputedStyle(el).gridTemplateColumns);
  assert.ok(chipGrid.split(" ").length === 2, `the comparison lays out in two columns, not ${chipGrid}`);
  assert.equal((await conflictEls(".dialog-body .grid > span >> nth=0").innerText()).trim(), "In the folder",
    "the comparison names the folder's copy");
  assert.match(
    await page.locator("[data-files-conflict]").innerText(),
    /In the folder[\s\S]*modified [^·\n]* ago/,
    "the folder's copy carries the server-observed facts",
  );
  assert.equal(await page.getByText("from your computer").count(), 1, "the comparison names the uploading copy");
  assert.deepEqual(
    await page.evaluate(() => ({
      tag: document.activeElement?.tagName,
      start: document.activeElement?.selectionStart,
      end: document.activeElement?.selectionEnd,
    })),
    { tag: "INPUT", start: 0, end: "README-2".length },
    "the ask takes focus with its base selected and the extension kept",
  );
  assert.equal(await page.getByRole("button", { name: "Cancel", exact: true }).count(), 1);
  assert.equal(await page.getByRole("button", { name: /^Upload as README-2\.md$/ }).count(), 1);
  assert.equal(await page.getByRole("button", { name: "Replace", exact: true }).count(), 1);
  assert.equal(await page.getByText("Same choice").count(), 0, "no same-choice offer with one conflict");
  // The alternative is a secondary outline and Replace the destructive
  // fill — the quiet answer must not wear the primary accent.
  const buttonPaint = await page.locator("[data-files-conflict] .dialog-foot").evaluate((foot) => {
    const read = (el) => {
      const cs = getComputedStyle(el);
      return { bg: cs.backgroundColor, borderWidth: cs.borderWidth, borderStyle: cs.borderStyle };
    };
    return {
      cancel: read(foot.querySelector("[data-files-conflict-cancel]")),
      uploadAs: read(foot.querySelector("[data-files-conflict-upload-as]")),
      replace: read(foot.querySelector("[data-files-conflict-replace]")),
    };
  });
  assert.ok(buttonPaint.uploadAs.borderWidth !== "0px" && buttonPaint.uploadAs.borderStyle === "solid",
    "the alternative is an outline button");
  assert.equal(buttonPaint.uploadAs.bg, buttonPaint.cancel.bg, "the alternative shares the secondary surface");
  assert.notEqual(buttonPaint.replace.bg, buttonPaint.uploadAs.bg, "Replace carries the destructive fill");

  // A taken alternative is refused on the field without sending anything.
  await nameField.fill("logo.tiff");
  await eventually(async () => (await conflictError.innerText()).trim() !== "", Boolean, "the taken-name error");
  assert.equal(await conflictError.innerText(), "That name is taken too.");
  assert.equal(
    await page.getByRole("button", { name: /^Upload as logo\.tiff$/ }).isDisabled(),
    true,
    "the confirm follows the field",
  );
  batch = uploads.length;
  await nameField.fill("README-2.md");
  assert.equal((await conflictError.innerText()).trim(), "", "a valid name clears the refusal");

  // Enter in the field confirms the alternative: it lands without consent.
  await nameField.press("Enter");
  await eventually(async () => (await dialog.count()) === 0 && !(await panel.locator("[data-files-upload-header]").isDisabled()), Boolean, "the ask closes on the landed alternative");
  assert.deepEqual(
    uploads.slice(batch),
    [{ path: "README-2.md", body: "replacement readme\n", expected: null }],
    "the alternative is a plain no-consent request",
  );
  await page.getByRole("button", { name: "README-2.md" }).and(page.locator("[data-files-row]")).waitFor();

  // Replace is consent that must survive the target changing: the first
  // approval meets a changed file and comes back as the same ask with
  // fresh facts, and nothing is destroyed until the user chooses
  // again over them.
  batch = uploads.length;
  await panel.locator("[data-files-input]").setInputFiles([
    { name: "README.md", mimeType: "text/plain", buffer: Buffer.from("second attempt\n") },
  ]);
  await dialog.waitFor();
  await page.getByRole("button", { name: "Replace", exact: true }).click();
  await dialog.waitFor();
  assert.deepEqual(
    uploads.slice(batch).map((u) => u.expected),
    [null, "1-probe-a"],
    "Replace carries the version the ask reported",
  );
  await eventually(
    async () => /changed since you approved replacing it — choose again with these facts\.$/.test(
      await page.locator("#files-conflict-desc").innerText()),
    Boolean,
    "the changed refusal re-asks over the fresh facts",
  );
  await eventually(
    async () => /4\.1 KB/.test(await page.locator("[data-files-conflict]").innerText()),
    Boolean,
    "the fresh facts replace the old ones",
  );
  await page.getByRole("button", { name: "Replace", exact: true }).click();
  await eventually(async () => (await dialog.count()) === 0 && !(await panel.locator("[data-files-upload-header]").isDisabled()), Boolean, "the re-approved replace lands");
  assert.deepEqual(
    uploads.slice(batch).map((u) => u.expected),
    [null, "1-probe-a", "1-probe-b"],
    "reconfirmation sends the fresh version, and only when asked",
  );

  // Escape cancels the one file being asked about: no request, no
  // authorization, and the existing file is untouched.
  batch = uploads.length;
  await panel.locator("[data-files-input]").setInputFiles([
    { name: "README.md", mimeType: "text/plain", buffer: Buffer.from("escaper\n") },
  ]);
  await dialog.waitFor();
  await page.keyboard.press("Escape");
  await eventually(async () => (await dialog.count()) === 0 && !(await panel.locator("[data-files-upload-header]").isDisabled()), Boolean, "Escape dismisses the ask");
  assert.deepEqual(uploads.slice(batch).map((u) => u.path), ["README.md"],
    "the dismissed ask sent nothing beyond the refused pick");

  batch = uploads.length;
  await panel.locator("[data-files-input]").setInputFiles([
    { name: "README.md", mimeType: "text/plain", buffer: Buffer.from("rename refusal\n") },
  ]);
  await dialog.waitFor();
  await nameField.fill("intro.md");
  await nameField.press("Enter");
  await eventually(async () => await conflictError.innerText(), (value) => value === "That name is taken too.", "the server-refused alternative");
  assert.equal(await page.locator("#files-conflict-title").innerText(), "README.md already exists");
  assert.doesNotMatch(await page.locator("#files-conflict-desc").innerText(), /changed since/);
  await page.getByRole("button", { name: "Replace", exact: true }).click();
  await eventually(async () => uploads.length, (length) => length === batch + 3, "Replace after refused rename");
  assert.deepEqual(uploads.slice(batch).map(({ path, expected }) => ({ path, expected })), [
    { path: "README.md", expected: null }, { path: "intro.md", expected: null },
    { path: "README.md", expected: "1-probe-a" },
  ], "Replace remains bound to the file named in the dialog");
  await page.locator("[data-files-conflict-cancel]").click();
  await eventually(async () => await dialog.count(), (count) => count === 0, "the refused rename ask settles");

  batch = uploads.length;
  await panel.locator("[data-files-input]").setInputFiles([
    { name: "README.md", mimeType: "text/plain", buffer: Buffer.from("mixed batch\n") },
    { name: "docs", mimeType: "text/plain", buffer: Buffer.from("folder\n") },
  ]);
  await dialog.waitFor();
  await page.locator("[data-files-conflict-same]").check();
  await page.locator("[data-files-conflict-replace]").click();
  await eventually(async () => await page.locator("#files-conflict-desc").innerText(), (text) => text.includes("changed since"), "the mixed batch rechecks its regular file");
  await page.locator("[data-files-conflict-replace]").click();
  await eventually(async () => await page.locator("#files-conflict-title").innerText(), (text) => text === "docs already exists", "the mixed batch stops at its folder");
  assert.equal(uploads.slice(batch).filter((call) => call.path === "docs").length, 1, "batch consent never sends a replacement for the folder");
  assert.doesNotMatch(await page.locator("#files-conflict-desc").innerText(), /changed since/);
  await page.locator("[data-files-conflict-cancel]").click();
  await eventually(async () => await dialog.count(), (count) => count === 0, "the mixed batch settles");

  // A folder holding the name is asked about without a Replace answer,
  // and the head's close control dismisses it like any other ask.
  await panel.locator("[data-files-input]").setInputFiles([
    { name: "docs", mimeType: "text/plain", buffer: Buffer.from("not a folder\n") },
  ]);
  await dialog.waitFor();
  assert.equal(await page.getByText("a folder").count(), 1, "the ask says a folder holds the name");
  assert.equal(await page.getByRole("button", { name: "Replace", exact: true }).count(), 0,
    "a folder is never replaceable");
  await page.locator("[data-files-conflict-close]").click();
  await eventually(async () => (await dialog.count()) === 0 && !(await panel.locator("[data-files-upload-header]").isDisabled()), Boolean, "the close control dismisses the ask");

  // An ask that arrives while Connect GitHub is open pushes onto it. Cancel
  // and Back leave GitHub open with the keyboard still in it; the head's
  // close control closes the whole modal.
  const github = page.locator("[data-github-connect-dialog]");
  const focusInGithub = () => page.evaluate(() => document.activeElement?.closest("[data-github-connect-dialog]") !== null);
  const conflictOverGithub = async (name) => {
    await panel.locator("[data-files-input]").setInputFiles([
      { name, mimeType: "text/plain", buffer: Buffer.from("held\n") },
    ]);
    await eventually(() => heldUploads.has(name), Boolean, `${name} awaits its answer`);
    await page.locator("[data-live-more]").click();
    await page.getByRole("button", { name: "GitHub connection…", exact: true }).click();
    await github.waitFor();
    heldUploads.get(name)();
    heldUploads.delete(name);
    await dialog.waitFor();
    assert.equal(await github.isVisible(), false, "the ask steps in front of Connect GitHub");
    assert.equal(await page.locator("[data-modal-scrim]").count(), 1, "one scrim holds both screens");
  };
  const askSettled = () => eventually(async () => (await dialog.count()) === 0
    && !(await panel.locator("[data-files-upload-header]").isDisabled()), Boolean, "the stacked ask settles");
  const closeGithub = async () => {
    await github.locator(".dialog-head [data-dialog-close]").click();
    await github.waitFor({ state: "detached" });
  };

  await conflictOverGithub("held-conflict-cancel.txt");
  await page.locator("[data-files-conflict-cancel]").click();
  await askSettled();
  await github.waitFor({ state: "visible" });
  assert.ok(await settled(focusInGithub, "focus after the ask closes"), "Cancel leaves the keyboard in Connect GitHub, not behind it");
  await closeGithub();

  await conflictOverGithub("held-conflict-back.txt");
  await page.locator("[data-files-conflict] [data-modal-back]").click();
  await askSettled();
  await github.waitFor({ state: "visible" });
  assert.ok(await settled(focusInGithub, "focus after the ask closes"), "Back leaves the keyboard in Connect GitHub");
  await closeGithub();

  await conflictOverGithub("held-conflict-close.txt");
  await page.locator("[data-files-conflict-close]").click();
  await askSettled();
  await github.waitFor({ state: "detached" });
  assert.equal(await page.locator("[data-modal-scrim]").count(), 0, "the ask's head close control closes the whole modal");

  // The ask rises from the bottom edge on the phone: grabber, close
  // control, and the comp's stacked order — Replace first, the
  // alternative second, Cancel last as the ghost way out.
  await setViewport({ width: 390, height: 844 }, "the conflict sheet's viewport");
  await panel.locator("[data-files-input]").setInputFiles([
    { name: "README.md", mimeType: "text/plain", buffer: Buffer.from("phone\n") },
  ]);
  await dialog.waitFor();
  const sheetBox = await box(page.locator("[data-files-conflict]"));
  const sheetButtons = await page.locator("[data-files-conflict] .dialog-foot button").evaluateAll((buttons) =>
    buttons.map((button) => {
      const rect = button.getBoundingClientRect();
      const cs = getComputedStyle(button);
      return {
        text: button.textContent.trim(), x: rect.x, y: rect.y, w: rect.width, h: rect.height,
        bg: cs.backgroundColor, borderColor: cs.borderTopColor,
      };
    }));
  assert.deepEqual(sheetButtons.map((button) => button.text), ["Replace", "Upload as README-3.md", "Cancel"],
    "the sheet's Tab order follows its visible actions");
  await page.locator("[data-files-conflict-replace]").focus();
  await page.keyboard.press("Tab");
  assert.ok(await page.locator("[data-files-conflict-upload-as]").evaluate((el) => el === document.activeElement));
  await page.keyboard.press("Tab");
  assert.ok(await page.locator("[data-files-conflict-cancel]").evaluate((el) => el === document.activeElement));
  const byRow = [...sheetButtons].sort((a, b) => a.y - b.y).map((button) => button.text);
  assert.deepEqual(byRow, ["Replace", "Upload as README-3.md", "Cancel"],
    "the sheet draws Replace first and Cancel last");
  const footPadding = await page.locator("[data-files-conflict] .dialog-foot")
    .evaluate((el) => Number.parseFloat(getComputedStyle(el).paddingLeft)
      + Number.parseFloat(getComputedStyle(el).paddingRight));
  for (const button of sheetButtons) {
    near(button.h, 48, `the ${button.text} action's height`);
    assert.ok(
      Math.abs(button.w - (sheetBox.width - footPadding)) < 2
        && button.x >= sheetBox.x - 1
        && button.x + button.w <= sheetBox.x + sheetBox.width + 1,
      `the ${button.text} action spans the sheet inside its padding`,
    );
  }
  const ghost = sheetButtons.find((button) => button.text === "Cancel");
  assert.ok(ghost !== undefined
    && (ghost.bg === "rgba(0, 0, 0, 0)" || ghost.borderColor === "rgba(0, 0, 0, 0)"),
    "Cancel reads as a ghost action on the sheet");
  const grabber = await page.locator("[data-files-conflict]")
    .evaluate((el) => getComputedStyle(el, "::before").content);
  assert.notEqual(grabber, "none", "the sheet carries its grabber");
  assert.ok(await page.locator("[data-files-conflict-close]").isVisible(),
    "the sheet keeps the accessible close control");
  await page.locator("[data-files-conflict-upload-as]").focus();
  await setViewport({ width: 1400, height: 900 }, "the open ask becomes a desktop dialog");
  assert.ok(await page.locator("[data-files-conflict-upload-as]").evaluate((el) => el === document.activeElement),
    "responsive action ordering preserves the focused control");
  assert.equal(await page.locator("[data-files-conflict] .dialog-foot button").first().innerText(), "Cancel");
  await setViewport({ width: 390, height: 844 }, "the open ask becomes a sheet again");
  assert.ok(await page.locator("[data-files-conflict-upload-as]").evaluate((el) => el === document.activeElement));
  await page.locator("[data-files-conflict-close]").click();
  await eventually(async () => (await dialog.count()) === 0 && !(await panel.locator("[data-files-upload-header]").isDisabled()), Boolean, "the sheet's close dismisses the ask");
  await setViewport({ width: 1400, height: 900 }, "the restored desktop viewport");

  // The preview reaches the same ask through the fixture's own upload
  // answers — a real two-file pick, not a hand-built state: 1 of 2, the
  // same-choice offer unchecked, a live suggestion, and the portal.
  {
    const previewPage = await context.newPage();
    await previewPage.setViewportSize({ width: 1400, height: 900 });
    await previewPage.goto(`${origin}/preview.html?panel=files&files-conflict=1`, { waitUntil: "networkidle" });
    await previewPage.waitForSelector("[data-files-conflict]");
    assert.equal(
      await previewPage.locator("#files-conflict-title").innerText(),
      "prd.md already exists",
      "the seeded pick asks about its first file",
    );
    assert.match(
      await previewPage.locator("#files-conflict-desc").innerText(),
      /in field-notes\/ · 1 of 2$/,
      "the preview ask is a real 1 of 2 from the model's own session",
    );
    assert.equal(
      await previewPage.locator("[data-files-conflict-name]").inputValue(),
      "prd-2.md",
      "the suggestion comes from the fixture listing, not a seeded string",
    );
    assert.equal(
      await previewPage.locator("[data-files-conflict-same]").isChecked(),
      false,
      "the same-choice offer is present and unchecked",
    );
    assert.ok(
      await previewPage.getByText("Same choice for the other 1 file").isVisible(),
      "the offer names what it covers",
    );
    const previewScrim = await box(previewPage.locator("[data-modal-scrim]"));
    near(previewScrim.width, 1400, "the preview ask also scrims the whole viewport", 1);
    await previewPage.close();
  }

  // ---- the no-preview state ----
  await page.getByRole("button", { name: "logo.tiff" }).and(page.locator("[data-files-row]")).click();
  await panel.getByText("No preview for .tiff files.").waitFor();
  assert.equal(await panel.locator("[data-files-text]").count(), 0, "nothing is decoded");
  await panel.getByRole("link", { name: "Download" }).first().waitFor();
  assert.match(await footerStatus.innerText(), /^tiff · 18\.4 KB · .* · read-only$/,
    "the binary footer keeps the machine register");

  // ---- expand and minimize ----
  await page.getByRole("button", { name: "README.md" }).and(page.locator("[data-files-row]")).click();
  await text.waitFor();
  await panel.getByRole("button", { name: "Expand" }).click();
  const expandedBox = await box(panel);
  near(expandedBox.x, contentBox.x, "expanded, the viewer starts at the sidebar's edge");
  near(expandedBox.width, contentBox.width, "expanded, it fills the content row");
  assert.equal(await panel.evaluate((el) => getComputedStyle(el).position), "absolute");
  // Expanding hides the list column only. The header stays, because without
  // the breadcrumb trail there is nothing saying which directory the open
  // file came from. Counting h2 proved nothing here - the panel has had no
  // h2 since the trail became its title.
  assert.equal(
    await panel.locator("[data-files-header]").isVisible(), true,
    "expanded, the header and its trail stay on screen",
  );
  assert.equal(await panel.locator("[data-files-crumbs]").count(), 1, "expanded, the trail is present");
  assert.equal(await panel.locator("[data-files-row]").count(), 0, "the list is hidden");
  assert.equal(
    await panel.locator("[data-files-close]").count(), 1,
    "expanded, exactly one close - the header owns it",
  );
  await panel.getByRole("button", { name: "Close files" }).waitFor();
  await panel.getByRole("button", { name: "Collapse" }).click();
  near((await box(panel)).width, expectedWidth, "minimizing restores the docked width");
  await page.getByRole("button", { name: "README.md" }).and(page.locator("[data-files-row]")).waitFor();

  // ---- closing resets ----
  await panel.getByRole("button", { name: "Expand" }).click();
  await panel.getByRole("button", { name: "Close files" }).click();
  await waitForPanelState(panel, false, "the expanded Files panel finishes closing");
  assert.equal(await panel.isVisible(), false);
  await filesButton.click();
  await waitForPanelState(panel, true, "Files finishes reopening");
  await panel.getByText("Nothing open").waitFor();
  await panel.getByText("Pick a file to read it.").waitFor();
  assert.equal(await panel.locator("[data-files-header]").isVisible(), true, "the docked header is back");

  // ---- mutual exclusion ----
  await tasksButton.click();
  await waitForPanelState(panel, false, "Files finishes sliding out for Tasks");
  await waitForPanelState(
    page.locator('[data-region="tasks"]'),
    true,
    "Tasks finishes replacing Files",
  );
  assert.equal(await panel.isVisible(), false, "opening Tasks closes Files");
  assert.equal(await filesButton.getAttribute("aria-pressed"), "false");
  await filesButton.click();
  await waitForPanelState(panel, true, "Files finishes replacing Tasks");
  assert.equal(
    await page.locator('[data-region="tasks"]').getAttribute("data-open"),
    "false",
    "opening Files closes Tasks",
  );

  // Current breadcrumbs share the actual desktop panel-title typography.
  const titleType = async (locator) => locator.evaluate((el) => {
    const s = getComputedStyle(el);
    return { size: s.fontSize, weight: s.fontWeight, color: s.color };
  });
  const typography = [];
  for (const density of ["normal", "compact", "comfortable"]) {
    await page.evaluate((value) => {
      if (value === "normal") document.documentElement.removeAttribute("data-density");
      else document.documentElement.dataset.density = value;
    }, density);
    await tasksButton.click();
    await waitForPanelState(page.locator('[data-region="tasks"]'), true, "Tasks typography");
    const tasks = await titleType(page.locator('[data-tm-header] .chrome-title'));
    await page.locator('[data-live-chats]').click();
    await waitForPanelState(page.locator('[data-region="chats"]'), true, "Chats typography");
    const chats = await titleType(page.locator('[data-chats-header] .chrome-title'));
    assert.deepEqual(chats, tasks, `${density}: Tasks and Chats title typography`);
    await filesButton.click();
    await waitForPanelState(panel, true, "Files typography");
    const root = crumbs.locator('[data-files-crumb-depth="-1"]');
    assert.deepEqual(await titleType(root), tasks, `${density}: root title typography`);
    assert.equal(await root.getAttribute("aria-current"), "location", "the project root is the current location");
    await panel.locator('[data-files-row]').getByText("docs/", { exact: true }).click();
    await panel.locator('[data-files-row]').getByText("architecture.md", { exact: true }).waitFor();
    const current = await titleType(crumbs.locator('[aria-current="location"]'));
    assert.deepEqual(current, tasks, `${density}: nested title typography`);
    assert.notEqual((await titleType(root)).color, tasks.color, "ancestor breadcrumb stays secondary");
    assert.equal(await root.getAttribute("aria-current"), null, "ancestor root is not current");
    typography.push({ density, tasks, chats, current });
    await root.click();
    await panel.locator('[data-files-row]').getByText("README.md", { exact: true }).waitFor();
  }
  await page.evaluate(() => { delete document.documentElement.dataset.density; });
  console.log("Files title typography", JSON.stringify(typography));

  // ---- navigation stays inside the project ----
  await page.getByRole("button", { name: "docs/" }).click();
  await page.getByRole("button", { name: "architecture.md" }).and(page.locator("[data-files-row]")).waitFor();
  assert.equal(
    (await crumbs.innerText()).replace(/\s+/g, " ").trim(),
    "noisedeck docs",
  );
  await crumbs.getByRole("button", { name: "noisedeck" }).click();
  await page.getByRole("button", { name: "README.md" }).and(page.locator("[data-files-row]")).waitFor();

  // ---- breadcrumb keeps the current crumb inside the nav at depth 4 ----
  await page.getByRole("button", { name: "deep/" }).click();
  await page.getByRole("button", { name: "field-notes/" }).waitFor();
  await page.getByRole("button", { name: "field-notes/" }).click();
  await page.getByRole("button", { name: "archive6/" }).waitFor();
  await page.getByRole("button", { name: "archive6/" }).click();
  await page.getByRole("button", { name: "sessions/" }).waitFor();
  await page.getByRole("button", { name: "sessions/" }).click();
  await panel.getByRole("button", { name: ".." }).waitFor();

  const assertCrumbFits = async (width) => {
    // Enter the path at this width rather than resizing into it: the trail is
    // fitted when the panel renders, and asserting on the settled result keeps
    // this about the geometry rather than about how many frames a resize takes
    // to propagate.
    await page.setViewportSize({ width, height: 900 });
    await page.reload({ waitUntil: "networkidle" });
    await page.getByRole("button", { name: "Files" }).click();
    await page.getByRole("button", { name: "deep/" }).click();
    await page.getByRole("button", { name: "field-notes/" }).click();
    await page.getByRole("button", { name: "archive6/" }).click();
    await page.getByRole("button", { name: "sessions/" }).click();
    await crumbs.locator(".crumb-current").waitFor();
    await page.waitForTimeout(120);
    const geometry = await crumbs.evaluate((nav) => {
      const current = nav.querySelector(".crumb-current");
      if (!(current instanceof HTMLElement)) return null;
      const navRect = nav.getBoundingClientRect();
      const currentRect = current.getBoundingClientRect();
      return {
        currentRight: currentRect.right,
        navRight: navRect.right,
        currentText: current.textContent?.trim() ?? "",
        parts: [...nav.querySelectorAll(".crumb-item, .crumb-sep")]
          .map((el) => `${el.className.includes("sep") ? "›" : (el.textContent?.trim() || "icon")}:${Math.round(el.getBoundingClientRect().width)}`)
          .join(" "),
      };
    });
    assert.ok(geometry !== null, `${width}px current crumb exists`);
    assert.ok(
      geometry.currentRight <= geometry.navRight + 0.5,
      `${width}px current crumb stays inside the nav ` +
        `(current "${geometry.currentText}" right ${Math.round(geometry.currentRight)}, ` +
        `nav right ${Math.round(geometry.navRight)}, parts ${geometry.parts})`,
    );
    assert.ok(
      geometry.currentText.length > 0,
      `${width}px current directory name is visible`,
    );
  };

  for (const width of [1502, 1100, 900, 860, 820, 800]) {
    await assertCrumbFits(width);
  }

  // The sweep leaves the page at its narrowest width, where the root crumb has
  // deliberately been given up, so put the context back the way the rest of
  // this probe expects to find it.
  await page.setViewportSize({ width: 1400, height: 900 });
  await page.reload({ waitUntil: "networkidle" });
  await page.getByRole("button", { name: "Files" }).click();
  await page.getByRole("button", { name: "README.md" }).and(page.locator("[data-files-row]")).waitFor();

  // ---- the cap, at a width where it binds ----
  await setViewport({ width: 1172, height: 900 }, "the narrowed content row");
  const cappedContent = await box(content);
  const cappedPanel = await box(panel);
  near(cappedContent.width, 900, "the narrowed content row");
  near(cappedPanel.width, 900 - TERMINAL_MIN_W, "the cap binds before the clamp's minimum");
  near((await box(terminalCard)).width, TERMINAL_MIN_W, "the terminal column holds its floor");

  // ---- phone layout, focus, and scroll continuity ----
  listings[""] = [
    listings[""][0],
    entry("mobile-scroll.txt", false, MOBILE_TEXT.length),
    ...Array.from({ length: 30 }, (_, index) =>
      entry(`phone-${String(index + 1).padStart(2, "0")}.txt`, false, 100 + index)),
    ...listings[""].slice(1),
  ];
  await page.setViewportSize({ width: 430, height: 760 });
  // The sheet closes by its own X at this width; the desktop by collapse.
  if (await page.locator("[data-sidebar-sheet-close]").isVisible()) {
    await page.locator("[data-sidebar-sheet-close]").click();
  } else {
    await page.getByRole("button", { name: "Collapse sidebar" }).click();
  }
  const phoneFilesOpenBox = await box(panel);
  // As on the desktop, the panel is a split that takes and gives up its space
  // at once: neither the comp nor the app animates its margin, so the distance it
  // covers is the assertion and there is no mid-flight position to sample.
  await panel.getByRole("button", { name: "Close files" }).click();
  await waitForPanelState(panel, false, "the phone Files panel closes");
  const phoneFilesClosedBox = await box(panel);
  near(Math.abs(phoneFilesClosedBox.x - phoneFilesOpenBox.x), phoneFilesOpenBox.width,
    "the phone Files close gives up exactly one width");
  await page.getByRole("button", { name: "Panels", exact: true }).click();
  await page.getByRole("menuitem", { name: "Files", exact: true }).click();
  await waitForPanelState(panel, true, "the phone Files panel reopens with the long listing");
  const phoneFilesReopenedBox = await box(panel);
  near(Math.abs(phoneFilesClosedBox.x - phoneFilesReopenedBox.x), phoneFilesReopenedBox.width,
    "the phone Files open takes exactly one width");
  await panel.getByRole("button", { name: "mobile-scroll.txt" }).and(page.locator("[data-files-row]")).waitFor();

  const assertNoPageOverflow = async (width, state) => {
    const sizes = await page.evaluate(() => ({
      viewport: innerWidth,
      root: document.documentElement.scrollWidth,
      body: document.body.scrollWidth,
    }));
    assert.equal(sizes.viewport, width);
    assert.equal(sizes.root, width, `${width}px ${state}: root does not overflow`);
    assert.equal(sizes.body, width, `${width}px ${state}: body does not overflow`);
  };
  const assertTouchTarget = async (target, width, name) => {
    const targetBox = await box(target);
    assert.ok(
      targetBox.width >= 44 && targetBox.height >= 44,
      `${width}px ${name} is ${targetBox.width}×${targetBox.height}, below 44×44`,
    );
  };

  for (const width of [430, 390, 320]) {
    await setViewport({ width, height: 760 }, `the ${width}px phone list`);
    assert.equal(await filesButton.isVisible(), false, `${width}px direct Files is collapsed`);
    assert.equal(await panel.getAttribute("data-open"), "true", `${width}px Files remains selected`);
    near((await box(panel)).width, width, `${width}px panel fills the visual viewport`);
    await assertNoPageOverflow(width, "list");
    await assertTouchTarget(page.getByRole("button", { name: "Panels", exact: true }), width, "Panels trigger");
    await assertTouchTarget(panel.getByRole("button", { name: "Close files" }), width, "close action");
    await assertTouchTarget(panel.getByRole("button", { name: "Upload files" }), width, "upload action");
    for (const row of await panel.locator("[data-files-row]").all()) {
      await assertTouchTarget(row, width, "file row");
    }

    await panel.getByRole("button", { name: LONG_FILE_NAME }).click();
    await panel.locator(`[data-files-viewer-scroll][data-files-path="${LONG_FILE_NAME}"]`).waitFor();
    const mobileLongName = await panel.locator("[data-files-viewer-name]").evaluate((el) => {
      const header = el.closest("[data-files-viewer-header]");
      const actions = header?.querySelector("[data-files-viewer-actions]");
      if (!(header instanceof HTMLElement) || !(actions instanceof HTMLElement)) return null;
      const nameBox = el.getBoundingClientRect();
      const headerBox = header.getBoundingClientRect();
      const actionBox = actions.getBoundingClientRect();
      return {
        clipped: el.scrollWidth > el.clientWidth,
        separated: nameBox.right <= actionBox.left,
        actionsContained: actionBox.right <= headerBox.right,
        headerContained: header.scrollWidth <= header.clientWidth,
      };
    });
    assert.deepEqual(mobileLongName, {
      clipped: true,
      separated: true,
      actionsContained: true,
      headerContained: true,
    }, `${width}px long filename truncates before the download action`);
    assert.equal((await panel.locator("[data-files-viewer-name]").innerText()).trim(), LONG_FILE_NAME);
    await assertTouchTarget(panel.getByRole("link", { name: "Download file" }), width, "long-name download action");
    await panel.getByRole("button", { name: "Back to files" }).click();
    assert.equal(
      await page.evaluate(() => document.activeElement?.getAttribute("data-files-row")),
      LONG_FILE_NAME,
      `${width}px long-name back restores focus to its row`,
    );

    await panel.getByRole("button", { name: "mobile-scroll.txt" }).and(page.locator("[data-files-row]")).click();
    assert.equal(
      await page.evaluate(() => document.activeElement?.getAttribute("aria-label")),
      "Back to files",
      `${width}px slow file open keeps focus in the pending viewer`,
    );
    await panel.locator("[data-files-text]").waitFor();
    assert.equal(await panel.locator("[data-files-list]").isVisible(), false, `${width}px list yields to viewer`);
    assert.equal(await panel.locator("[data-files-viewer]").isVisible(), true);
    assert.equal(
      await page.evaluate(() => document.activeElement?.getAttribute("aria-label")),
      "Back to files",
      `${width}px file open moves focus into the viewer`,
    );
    await assertTouchTarget(panel.getByRole("button", { name: "Back to files" }), width, "back action");
    await assertTouchTarget(panel.getByRole("link", { name: "Download file" }), width, "download action");
    const wrap = panel.getByRole("button", { name: "Wrap lines" });
    await assertTouchTarget(wrap, width, "wrap action");
    assert.equal(await wrap.getAttribute("aria-pressed"), "true", `${width}px wrapping defaults on`);
    const viewerText = panel.locator("[data-files-text]");
    assert.equal(await viewerText.textContent(), MOBILE_TEXT, `${width}px rendered text preserves API bytes`);
    assert.equal(await viewerText.getAttribute("data-files-wrapped"), "true");
    const viewerSurface = panel.locator("[data-files-viewer-scroll]");
    const wrappedGeometry = await viewerText.evaluate((el) => ({
      clientWidth: el.clientWidth,
      scrollWidth: el.scrollWidth,
      overflowWrap: getComputedStyle(el).overflowWrap,
      numberedLines: el.querySelectorAll("[data-files-line]").length,
      firstLineHeight: el.querySelector("[data-files-line]")?.getBoundingClientRect().height,
      lineHeight: Number.parseFloat(getComputedStyle(el).lineHeight),
    }));
    assert.ok(
      wrappedGeometry.scrollWidth <= wrappedGeometry.clientWidth + 1,
      `${width}px wrapped text stays within the viewer`,
    );
    assert.equal(wrappedGeometry.overflowWrap, "anywhere", `${width}px unbroken tokens may break as needed`);
    assert.equal(wrappedGeometry.numberedLines, 120, `${width}px logical line labels remain associated`);
    assert.ok(
      (wrappedGeometry.firstLineHeight ?? 0) > wrappedGeometry.lineHeight,
      `${width}px long logical lines soft-wrap to multiple visual rows`,
    );
    await wrap.click();
    assert.equal(await wrap.getAttribute("aria-pressed"), "false", `${width}px wrap can be disabled`);
    assert.equal(await viewerText.getAttribute("data-files-wrapped"), "false");
    assert.equal(await viewerText.textContent(), MOBILE_TEXT, `${width}px unwrapping does not change text bytes`);
    const unwrappedGeometry = await viewerSurface.evaluate((el) => ({
      clientWidth: el.clientWidth,
      scrollWidth: el.scrollWidth,
      firstLineStep: (() => {
        const lines = el.querySelectorAll("[data-files-line]");
        return lines.length < 2
          ? 0
          : lines[1].getBoundingClientRect().top - lines[0].getBoundingClientRect().top;
      })(),
      lineHeight: Number.parseFloat(getComputedStyle(el.querySelector("[data-files-text]")).lineHeight),
    }));
    assert.ok(
      unwrappedGeometry.scrollWidth > unwrappedGeometry.clientWidth,
      `${width}px unwrapped long lines scroll horizontally`,
    );
    near(
      unwrappedGeometry.firstLineStep,
      unwrappedGeometry.lineHeight,
      `${width}px unwrapped logical lines have no inserted blank rows`,
      0.5,
    );
    await wrap.click();
    assert.equal(await wrap.getAttribute("aria-pressed"), "true", `${width}px wrap state is exposed`);
    assert.equal(
      await panel.getByRole("button", { name: "Expand" }).isVisible(),
      false,
      `${width}px omits the desktop-only expand action`,
    );
    const viewerHeader = panel.locator("[data-files-viewer-header]");
    assert.ok(
      await viewerHeader.evaluate((el) => el.scrollWidth <= el.clientWidth),
      `${width}px viewer action row contains its filename and controls`,
    );
    await assertNoPageOverflow(width, "viewer");
    await panel.getByRole("button", { name: "Back to files" }).click();
    assert.equal(
      await page.evaluate(() => document.activeElement?.getAttribute("data-files-row")),
      "mobile-scroll.txt",
      `${width}px back restores focus to the opened row`,
    );
  }

  await page.setViewportSize({ width: 390, height: 760 });
  await panel.getByRole("button", { name: `${LONG_DIR_NAME}/` }).click();
  await panel.getByRole("button", { name: ".." }).waitFor();
  const crumbGeometry = await crumbs.evaluate((nav) => {
    const current = nav.querySelector(".crumb-current");
    if (!(current instanceof HTMLElement)) return null;
    return {
      prefixCount: nav.querySelectorAll("[data-files-prefix]").length,
      currentVisible: current.getBoundingClientRect().width > 0,
      navFits: nav.scrollWidth <= nav.clientWidth + 1,
      currentText: current.textContent,
    };
  });
  assert.deepEqual(crumbGeometry, {
    prefixCount: 0,
    currentVisible: true,
    navFits: true,
    currentText: LONG_DIR_NAME,
  }, "390px long path elides the middle and keeps the current crumb visible without a prefix");
  await crumbs.getByRole("button", { name: "noisedeck" }).click();
  await panel.getByRole("button", { name: "mobile-scroll.txt" }).and(page.locator("[data-files-row]")).waitFor();
  if (process.env.NOISEDECK_FILES_PATH_ONLY === "1") throw pathOnlyComplete;

  const listScroller = panel.locator("[data-files-list-scroll]");
  await listScroller.evaluate((el) => {
    el.scrollTop = 420;
  });
  const listScrollBeforeViewer = await listScroller.evaluate((el) => el.scrollTop);
  const scrollFile = panel.getByRole("button", { name: "phone-10.txt" }).and(page.locator("[data-files-row]"));
  await scrollFile.focus();
  await scrollFile.click();
  await panel.getByText("No preview for .txt files.").waitFor();
  await panel.getByRole("button", { name: "Back to files" }).click();
  near(
    await listScroller.evaluate((el) => el.scrollTop),
    listScrollBeforeViewer,
    "back restores the directory scroll position",
    1,
  );

  await panel.getByRole("button", { name: "mobile-scroll.txt" }).and(page.locator("[data-files-row]")).click();
  const viewerScroller = panel.locator("[data-files-viewer-scroll]");
  await viewerScroller.waitFor();
  // Wrapped text (the default) has no horizontal overflow to restore.
  await panel.getByRole("button", { name: "Wrap lines" }).click();
  await viewerScroller.evaluate((el) => {
    el.scrollTop = 360;
    el.scrollLeft = 180;
  });
  const viewerScroll = await viewerScroller.evaluate((el) => ({
    top: el.scrollTop,
    left: el.scrollLeft,
  }));
  assert.ok(viewerScroll.top > 0 && viewerScroll.left > 0, "the unwrapped phone viewer scrolls on both axes");
  await panel.getByRole("button", { name: "Back to files" }).click();
  await panel.getByRole("button", { name: "mobile-scroll.txt" }).and(page.locator("[data-files-row]")).click();
  await panel.locator("[data-files-text]").waitFor();
  const restoredViewerScroll = await viewerScroller.evaluate((el) => ({
    top: el.scrollTop,
    left: el.scrollLeft,
  }));
  near(restoredViewerScroll.top, viewerScroll.top, "reopening restores viewer vertical scroll", 1);
  near(restoredViewerScroll.left, viewerScroll.left, "reopening restores viewer horizontal scroll", 1);
  await panel.getByRole("button", { name: "Wrap lines" }).click();
  await panel.getByRole("button", { name: "Back to files" }).click();

  await panel.locator("[data-files-input]").setInputFiles([
    { name: "slow-mobile.txt", mimeType: "text/plain", buffer: Buffer.from("phone\n") },
    { name: "reject.bin", mimeType: "application/octet-stream", buffer: Buffer.from("xx") },
  ]);
  const uploadProgress = panel.locator('[data-files-upload][aria-label="Upload files"]');
  await uploadProgress.waitFor();
  assert.equal(await page.evaluate(() => {
    const active = document.activeElement;
    return document.querySelector('[data-region="files"]')?.contains(active) && !active?.disabled;
  }), true, "a pending upload keeps focus on an enabled in-panel control");
  assert.equal(await uploadProgress.isDisabled(), true, "the upload button disables while a pick is in flight");
  assert.equal(
    await page.locator('[role="status"][aria-live="polite"]').filter({ hasText: "Uploading files…" }).count(),
    1,
    "the busy pick is announced via the live region",
  );
  await assertTouchTarget(panel.getByRole("button", { name: "Upload files" }), 390, "busy upload action");
  await panel.locator('[data-files-row="slow-mobile.txt"]').waitFor();
  assert.equal(await panel.locator("[data-files-list]").isVisible(), true,
    "upload completion keeps the folder visible rather than choosing a viewer");
  assert.equal(await panel.locator("[data-files-viewer]").isVisible(), false,
    "upload completion never opens a viewer automatically");
  const uploadError = panel.getByText("reject.bin: upload is too large");
  await uploadError.waitFor();
  assert.ok(
    await panel.locator("[data-files-upload-errors]").evaluate((el) => el.scrollWidth <= el.clientWidth),
    "mobile upload errors wrap inside their contained status area",
  );

  await panel.getByRole("button", { name: "docs/" }).click();
  assert.equal(
    await page.evaluate(() => document.activeElement?.getAttribute("aria-label")),
    "Close files",
    "slow directory navigation keeps focus in the panel",
  );
  await panel.getByRole("button", { name: "architecture.md" }).and(page.locator("[data-files-row]")).waitFor();
  assert.equal(
    await page.evaluate(() => document.activeElement?.getAttribute("data-files-row")),
    "docs/architecture.md",
    "directory navigation focuses the first confirmed row",
  );
  const rootCrumb = crumbs.getByRole("button", { name: "noisedeck" });
  await assertTouchTarget(rootCrumb, 390, "breadcrumb action");
  await rootCrumb.click();
  await panel.getByRole("button", { name: "mobile-scroll.txt" }).and(page.locator("[data-files-row]")).waitFor();
  assert.equal(
    await page.evaluate(() => document.activeElement?.getAttribute("data-files-row")),
    "docs",
    "breadcrumb navigation focuses the first confirmed root row",
  );

  await panel.getByRole("button", { name: "x/" }).click();
  await panel.getByRole("button", { name: "y/" }).waitFor();
  await panel.getByRole("button", { name: "y/" }).click();
  await panel.getByText("empty directory").waitFor();
  assert.equal(
    await page.evaluate(() => document.activeElement?.getAttribute("aria-label")),
    "Close files",
    "an empty directory completion keeps focus in the panel",
  );
  await assertTouchTarget(crumbs.getByRole("button", { name: "x" }), 390, "one-character breadcrumb");
  await crumbs.getByRole("button", { name: "noisedeck" }).click();
  await panel.getByRole("button", { name: "broken/" }).waitFor();
  await panel.getByRole("button", { name: "broken/" }).click();
  assert.equal(
    await page.evaluate(() => document.activeElement?.getAttribute("aria-label")),
    "Close files",
    "a failing directory request keeps focus in the panel",
  );
  await panel.getByText("no such file in the project").waitFor();
  assert.equal(
    await page.evaluate(() => document.activeElement?.getAttribute("aria-label")),
    "Close files",
    "a directory error completion keeps focus in the panel",
  );
  await crumbs.getByRole("button", { name: "noisedeck" }).click();
  await panel.getByRole("button", { name: "mobile-scroll.txt" }).and(page.locator("[data-files-row]")).waitFor();

  await page.evaluate(() => {
    document.querySelector("[data-workspace]")?.style.setProperty("--workspace-safe-area-top", "24px");
  });
  await page.waitForTimeout(50);
  assert.ok(
    (await box(page.getByRole("button", { name: "Panels", exact: true }))).y >= 24,
    "the mobile titlebar keeps the Panels trigger below the top safe area",
  );
  await page.evaluate(() => {
    document.querySelector("[data-workspace]")?.style.removeProperty("--workspace-safe-area-top");
  });

  await panel.getByRole("button", { name: "Close files" }).click();
  await waitForPanelState(panel, false, "the phone close action closes the panel");
  assert.equal(
    await page.evaluate(() => document.activeElement?.getAttribute("aria-label")),
    "Panels",
    "panel close returns focus to the visible Panels trigger",
  );
  // A status refresh replaces the titlebar even when the session is unchanged.
  const focusedPanelsTrigger = await page.getByRole("button", { name: "Panels", exact: true }).elementHandle();
  await page.waitForFunction((trigger) => !trigger.isConnected, focusedPanelsTrigger);
  assert.equal(
    await page.evaluate(() => document.activeElement?.getAttribute("aria-label")),
    "Panels",
    "a session status refresh preserves focus on the Panels trigger",
  );
  await focusedPanelsTrigger.dispose();
  await page.getByRole("button", { name: "Panels", exact: true }).click();
  await page.getByRole("menuitem", { name: "Files", exact: true }).click();
  await waitForPanelState(panel, true, "the phone panel reopens");
  await panel.getByRole("button", { name: "slow-mobile.txt" }).and(page.locator("[data-files-row]")).waitFor();
  await setViewport({ width: 390, height: 360 }, "the shortened visual viewport");
  const uploadBottom = await panel.getByRole("button", { name: "Upload files" })
    .evaluate((el) => el.getBoundingClientRect().bottom);
  assert.ok(uploadBottom <= 360, `the shortened visual viewport keeps upload reachable at ${uploadBottom}px`);
  await assertNoPageOverflow(390, "short visual viewport");

  assert.deepEqual(pageErrors, [], "no uncaught page errors");
  // A files-select naming nothing must stop the page, including with no
// files-depth beside it: that combination once skipped the whole files setup,
// so an unresolved name left the root list unselected and the page marked
// itself reached. A state that certifies the wrong screen is worse than one
// that fails.
{
  const bad = await context.newPage();
  await bad.goto(`${origin}/preview.html?panel=files&files-select=missing.go`, { waitUntil: "networkidle" });
  const marks = await bad.evaluate(() => ({
    error: document.documentElement.dataset.previewError ?? null,
    state: document.documentElement.dataset.previewState ?? null,
  }));
  assert.match(marks.error ?? "", /files-select/,
    `an unresolved files-select did not stop the page: ${JSON.stringify(marks)}`);
  assert.equal(marks.state, null,
    `the page marked itself reached despite an unresolved files-select: ${JSON.stringify(marks)}`);
  await bad.close();

  const good = await context.newPage();
  await good.goto(`${origin}/preview.html?panel=files&files-select=README.md`, { waitUntil: "networkidle" });
  const okMarks = await good.evaluate(() => ({
    error: document.documentElement.dataset.previewError ?? null,
    selected: document.querySelector('[data-files-row][aria-current="true"]')?.getAttribute("data-files-row") ?? null,
  }));
  assert.equal(okMarks.error, null, `a root selection was rejected: ${JSON.stringify(okMarks)}`);
  assert.equal(okMarks.selected, "README.md",
    `a root files-select did not select its row: ${JSON.stringify(okMarks)}`);
  await good.close();
  console.log("files-panel probe: an unresolved files-select stops the page, a root one selects");
}

// The phone's viewer is its own screen (comp mobile-r8 screen 05), and each
// of its truths is asserted in a real browser because each was hand-verified
// once already — a claim a probe does not carry has to be re-verified by a
// person every time it might have moved.
{
  const phone = await context.newPage();
  await phone.setViewportSize({ width: 393, height: 852 });
  // The list first: browsing keeps the list chrome.
  await phone.goto(`${origin}/preview.html?panel=files&files-depth=2&files-select=provider.go`, { waitUntil: "networkidle" });
  await phone.waitForSelector('[data-files-row][aria-current="true"]');
  const listSeen = await phone.evaluate(() => ({
    viewing: document.querySelector('[data-region="files"]')?.dataset.viewing ?? null,
    header: (() => { const el = document.querySelector("[data-files-header]"); return el === null ? "absent" : getComputedStyle(el).display; })(),
  }));
  assert.equal(listSeen.viewing, "false", "browsing marks itself as viewing");
  assert.notEqual(listSeen.header, "none", "the phone list lost its chrome while browsing");

  // The viewer: the comp's file, the list chrome gone, the footer short.
  await phone.goto(`${origin}/preview.html?viewer=code`, { waitUntil: "networkidle" });
  await phone.waitForSelector('[data-files-viewer-scroll][data-files-path="internal/provider/provider.go"]');
  const seen = await phone.evaluate(() => {
    const cs = (sel, prop) => { const el = document.querySelector(sel); return el === null ? null : getComputedStyle(el)[prop]; };
    // innerText, not textContent: the age's words are display:none children
    // of a visible span, and textContent reads through the hiding.
    const visibleText = (sel) => [...document.querySelectorAll(sel)]
      .filter((el) => getComputedStyle(el).display !== "none")
      .map((el) => el.innerText).join("");
    return {
      viewing: document.querySelector('[data-region="files"]')?.dataset.viewing ?? null,
      header: cs("[data-files-header]", "display"),
      footer: visibleText("[data-files-viewer-status] > span").replace(/\s+/g, " ").trim(),
      wrapButtons: document.querySelectorAll("[data-files-wrap]").length,
      dot: cs("[data-files-wrap-dot]", "display"),
      swtch: cs("[data-files-wrap-switch]", "display"),
    };
  });
  assert.equal(seen.viewing, "true", "an open file does not mark viewing");
  assert.equal(seen.header, "none", "the list chrome stays up behind the phone viewer");
  assert.match(seen.footer, /^0\.9 KB · 38m$/,
    `the phone footer says more than the comp does: ${JSON.stringify(seen.footer)}`);
  assert.equal(seen.wrapButtons, 1, "the wrap state has more than one control");
  assert.equal(seen.dot, "none", "the phone shows the desktop's wrap dot");
  assert.notEqual(seen.swtch, "none", "the phone hides its wrap switch");

  // Fail-loud: a code viewer whose file is missing stops the page.
  const bad = await context.newPage();
  await bad.setViewportSize({ width: 393, height: 852 });
  await bad.goto(`${origin}/preview.html?viewer=nope`, { waitUntil: "networkidle" });
  const marks = await bad.evaluate(() => ({
    error: document.documentElement.dataset.previewError ?? null,
    state: document.documentElement.dataset.previewState ?? null,
  }));
  assert.match(marks.error ?? "", /viewer/, `an unknown viewer did not stop the page: ${JSON.stringify(marks)}`);
  assert.equal(marks.state, null, "the page marked itself reached despite an unknown viewer");
  await bad.close();

  // Desktop: the same state is the wide presentation — one status line, the
  // dot not the switch, the list header back.
  await phone.setViewportSize({ width: 1400, height: 900 });
  await phone.goto(`${origin}/preview.html?panel=files&viewer=code`, { waitUntil: "networkidle" });
  await phone.waitForSelector("[data-files-viewer-status]");
  const wide = await phone.evaluate(() => {
    const cs = (sel, prop) => { const el = document.querySelector(sel); return el === null ? null : getComputedStyle(el)[prop]; };
    return {
      status: document.querySelector("[data-files-viewer-status]").innerText,
      dot: cs("[data-files-wrap-dot]", "display"),
      swtch: cs("[data-files-wrap-switch]", "display"),
      header: cs("[data-files-header]", "display"),
    };
  });
  assert.match(wide.status, /^go · 18 lines · 0\.9 KB · modified .* ago · read-only$/,
    `the desktop status changed shape: ${JSON.stringify(wide.status)}`);
  assert.ok(!wide.status.includes("\n"), "the desktop status broke into lines");
  assert.notEqual(wide.dot, "none", "the desktop lost its wrap dot");
  assert.equal(wide.swtch, "none", "the desktop shows the phone's wrap switch");
  assert.notEqual(wide.header, "none", "the desktop lost the list chrome");
  await phone.close();
  console.log("files-panel probe: the phone viewer holds its screen contract");
}

console.log("files-panel probe: all assertions passed");
  await context.close();
} catch (error) {
  if (error === pathOnlyComplete) {
    console.log("files-path probe: all assertions passed");
    await context?.close();
  } else {
    if (page !== undefined && !page.isClosed()) {
      process.stderr.write(`panel html:\n${await page.locator('[data-region="files"]').innerHTML().catch(() => "<none>")}\n`);
    }
    if (pageErrors.length > 0) process.stderr.write(`page errors:\n${pageErrors.join("\n")}\n`);
    throw error;
  }
} finally {
  for (const release of heldUploads.values()) release();
  await browser?.close();
  await new Promise((r) => server.close(r));
}
