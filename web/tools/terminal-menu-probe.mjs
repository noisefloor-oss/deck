// The terminal's own context menu, measured in a real browser
// against the real build.
//
// The unit suite covers the panel's markup and where it is placed. What it
// cannot see is the part the user depends on: that a right-click over the
// pane suppresses the browser's own menu and raises ours, that Copy reaches
// the system clipboard with the pane's text, and that Paste delivers the
// clipboard to the session as an input frame rather than as typing. All four
// need a browser, a mounted xterm and a clipboard, so they live here.
import assert from "node:assert/strict";
import { createServer } from "node:http";
import { readFile } from "node:fs/promises";
import { existsSync } from "node:fs";
import { extname, join, resolve } from "node:path";
import { browser as getBrowser } from "./browser.mjs";

const WEB = resolve(import.meta.dirname, "..");
const DIST = join(WEB, "dist");
const MIME = {
  ".css": "text/css",
  ".html": "text/html",
  ".js": "text/javascript",
  ".png": "image/png",
  ".svg": "image/svg+xml",
  ".woff2": "font/woff2",
};

if (!existsSync(join(DIST, "index.html"))) {
  throw new Error("dist/index.html is missing — run `pnpm build` first");
}

const server = createServer(async (req, res) => {
  const pathname = decodeURIComponent(new URL(req.url, "http://x").pathname);
  const path = join(DIST, pathname === "/" ? "index.html" : pathname);
  if (!path.startsWith(DIST) || !existsSync(path)) {
    res.writeHead(404);
    res.end("not found");
    return;
  }
  res.writeHead(200, { "Content-Type": MIME[extname(path)] ?? "application/octet-stream" });
  res.end(await readFile(path));
});
await new Promise((listening) => server.listen(0, "127.0.0.1", listening));
const address = server.address();
assert.equal(typeof address, "object");
const origin = `http://127.0.0.1:${address.port}`;

const project = {
  id: "project-menu",
  name: "menu-project",
  canonical_path: "/workspace/menu-project",
  last_used_at: "2026-09-06T00:00:00Z",
};
const session = {
  id: "session-menu",
  project_id: project.id,
  provider: "codex",
  display_name: "",
  status: "running",
  generation: 1,
  created_at: "2026-09-06T00:00:00Z",
  updated_at: "2026-09-06T00:00:00Z",
  recovery_capability: "none",
  recovery_state: "unavailable",
  presentation_state: "running",
};
// Each line is distinct so a copied fragment can be traced back to the row it
// came from, and short enough that the whole screen is legible in a failure.
const LINES = Array.from({ length: 24 }, (_, i) => `line-${String(i).padStart(2, "0")}-abcdefghij`);

let browser;
try {
  ({ browser } = await getBrowser());
  await run("windows");
  await run("mac");
  console.log("terminal menu probe: all assertions passed");
} finally {
  await browser?.close();
  await new Promise((closed) => server.close(closed));
}

// The mac pass stops after the right-click menu: xterm reads the platform at
// load, and on macOS it would select the word under a right-click unless
// told not to. The keyboard chords that follow are Windows chords.
async function run(platform) {
  const context = await browser.newContext({
    viewport: { width: 1280, height: 800 },
    permissions: ["clipboard-read", "clipboard-write"],
  });
  await context.addInitScript((mac) => {
    Object.defineProperty(navigator, "userAgentData", { value: { platform: mac ? "macOS" : "Windows" } });
    Object.defineProperty(navigator, "platform", { value: mac ? "MacIntel" : "Win32" });
  }, platform === "mac");
  await context.addCookies([{ name: "noisedeck_csrf", value: "cookie-csrf", url: origin }]);
  const page = await context.newPage();
  const loginRequests = [];
  let completeDelivery;
  let malformed = true;
  // Nothing waits until a phase asks for it: the login coordinator raises
  // automatically while a controller is attached, so earlier phases run
  // with an empty listing.
  let pendingLogin = false;
  let claimedLogin = false;
  let attemptId = "native-attempt";
  let holdLoginListings = false;
  const heldLoginListings = [];
  const errors = [];
  page.on("pageerror", (error) => errors.push(error.message));

  await page.route(`${origin}/auth/session`, (route) => route.fulfill({
    status: 204,
    headers: { "X-Noisedeck-Auth-Mode": "builtin" },
  }));
  await page.route(`${origin}/api/**`, (route) => {
    const request = route.request();
    const url = new URL(request.url());
    const path = url.pathname;
    const json = (status, body) => route.fulfill({
      status,
      contentType: "application/json",
      body: JSON.stringify(body),
    });
    const stream = () => route.fulfill({
      status: 200,
      contentType: "text/event-stream",
      body: ": connected\n\nretry: 3600000\n\n",
    });
    if (path === "/api/harness-login") {
      if (request.method() === "GET") {
        assert.equal(new URL(request.url()).searchParams.get("session"), session.id);
        const listing = { csrf: "login-csrf", pending: pendingLogin && !claimedLogin ? [{ id: attemptId, flow: "codex-browser", expires: new Date(Date.now() + 300000).toISOString() }] : [] };
        if (holdLoginListings && !url.searchParams.has("watch")) return new Promise(resolve => {
          heldLoginListings.push(async () => { await json(200, listing); resolve(); });
        });
        return json(200, listing);
      }
      assert.equal(request.method(), "POST");
      assert.equal(request.headers().origin, origin);
      assert.equal(request.headers()["x-harness-login-csrf"], "login-csrf");
      assert.equal(request.headers()["x-csrf-token"], "cookie-csrf");
      assert.equal(request.headers().referer, undefined);
      const body = request.postDataJSON();
      loginRequests.push(body);
      if (body.action === "begin") {
        if (!pendingLogin || claimedLogin || body.registration !== attemptId) return json(409, {});
        claimedLogin = true;
        return json(200, { id: "one-use-handle" });
      }
      if (body.action === "dismiss") { claimedLogin = false; return json(200, { status: "dismissed" }); }
      if (body.action === "cancel") { pendingLogin = false; claimedLogin = false; return json(200, { status: "cancelled" }); }
      assert.equal(body.action, "submit");
      assert.equal(claimedLogin, true);
      if (malformed) { malformed = false; return json(400, { ignored: "callback-fixture-canary" }); }
      pendingLogin = false;
      claimedLogin = false;
      return new Promise((resolve) => { completeDelivery = async () => { await json(200, { status: "completed", ignored: "response-fixture-canary" }); resolve(); }; });
    }
    if (request.method() === "GET" && path === "/api/session-activity/events") return stream();
    if (request.method() === "GET" && path === "/api/providers") return json(200, { providers: ["codex"] });
    if (request.method() === "GET" && path === "/api/settings/last-session") return json(404, { code: "not_found", message: "no last session remembered" });
    if (request.method() === "PUT" && path === "/api/settings/last-session") return json(200, request.postDataJSON());
    if (request.method() === "GET" && path === "/api/settings/layout") return json(404, { code: "not_found", message: "no layout saved" });
    if (request.method() === "GET" && path === "/api/settings/tutorial") return json(200, { version: 1, dismissed: true, lesson: "welcome", step: 0, done: [], scope: "operator" });
    if (request.method() === "GET" && path === "/api/settings") {
      return json(200, { skip_permissions: false, skip_close_confirm: false, skip_remove_project_confirm: false });
    }
    if (request.method() === "GET" && path === "/api/projects") return json(200, [project]);
    if (request.method() === "GET" && path === "/api/git-identities") {
      // A configured identity keeps the workspace out of the git-identity
      // setup modal, whose scrim would sit over the pane this probe clicks.
      return json(200, {
        profiles: [{ id: "identity-1", name: "Deck probe", email: "deck@example.invalid", revision: 1, created_at: "t", updated_at: "t" }],
        default_profile_id: "identity-1",
        revision: 1,
      });
    }
    if (request.method() === "GET" && path === `/api/projects/${project.id}/sessions`) return json(200, [session]);
    if (request.method() === "GET" && path === `/api/sessions/${session.id}`) {
      return json(200, { ...session, pane_pid: 4242, tmux_pane_id: "%1", native_session_id: "native-menu" });
    }
    if (/^\/api\/projects\/[^/]+\/lanes$/.test(path)) return json(200, { lanes: [] });
    if (request.method() === "GET" && path === `/api/projects/${project.id}/taskmaster`) {
      return json(404, { error: "not_taskmaster_project", message: "not a task project" });
    }
    if (request.method() === "GET" && path === `/api/projects/${project.id}/taskmaster/events`) return stream();
    if (request.method() === "GET" && /^\/api\/sessions\/[^/]+\/chats\/events$/.test(path)) return stream();
    if (request.method() === "GET" && /^\/api\/sessions\/[^/]+\/chats$/.test(path)) {
      return json(200, { agent: "codex-sess", chats: [] });
    }
    if (request.method() === "GET" && path === "/api/chats/status") return json(200, { available: true });
    if (request.method() === "GET" && path === "/api/chats") return json(200, { chats: [] });
    // The closed Assets panel reads the listing and follows its feed for its badge.
    if (/^\/api\/projects\/[^/]+\/assets$/.test(path)) return route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ locations: [], scanned: "2026-01-01T00:00:00Z", preview_max_bytes: 16777216 }) });
    if (/^\/api\/projects\/[^/]+\/assets\/events$/.test(path)) return route.fulfill({ status: 200, contentType: "text/event-stream", body: ": connected\n\nretry: 3600000\n\n" });
    throw new Error(`unhandled ${request.method()} ${path}`);
  });

  const frames = [];
  let socket;
  let emitOutput = null;
  await page.routeWebSocket(`${origin.replace("http", "ws")}/api/sessions/*/terminal?*`, (peer) => {
    emitOutput = (data) => peer.send(JSON.stringify({ type: "output", data: Buffer.from(data).toString("base64") }));
    socket = peer;
    peer.onMessage((message) => {
      if (typeof message === "string") frames.push(JSON.parse(message));
    });
    peer.send(JSON.stringify({ type: "attached", token: 7, generation: 1 }));
    // The application turns bracketed paste on, as every modern CLI does,
    // and asks for mouse reports in SGR encoding, as a TUI harness does:
    // what arrives as a paste, what arrives as typing, and what arrives as
    // mouse bytes are then three different things to it, and the probe can
    // tell which of the three a gesture became.
    peer.send(JSON.stringify({
      type: "output",
      data: Buffer.from(`\u001b[?2004h\u001b[?1000h\u001b[?1006h${LINES.join("\r\n")}\r\n`).toString("base64"),
    }));
  });

  await page.goto(origin, { waitUntil: "networkidle" });
  await page.locator("[data-live-terminal] .xterm").waitFor();
  await waitFor(() => socket !== undefined, "terminal WebSocket");
  await page.waitForFunction(
    () => document.querySelector(".xterm-rows")?.textContent?.includes("line-20-abcdefghij"),
  );
  // Whether the browser's own menu was suppressed is not visible in the DOM:
  // it is the event's defaultPrevented, read after the app's handler on the
  // workspace root has had the event.
  await page.evaluate(() => {
    window.__contextmenus = [];
    document.addEventListener("contextmenu", (event) => {
      window.__contextmenus.push({
        prevented: event.defaultPrevented,
        inTerminal: event.target instanceof Element
          && event.target.closest('[data-region="terminal"]') !== null,
      });
    });
  });

  const screen = page.locator("[data-live-terminal] .xterm-screen");
  const box = await screen.boundingBox();
  assert.notEqual(box, null, "the terminal screen has no box");
  const menu = page.locator("[data-terminal-menu-popover]");

  // 1. A right-click over the pane raises the app's menu instead of the
  // browser's.
  await screen.click({ button: "right", position: { x: 60, y: 40 } });
  await menu.waitFor();
  assert.deepEqual(
    await page.evaluate(() => window.__contextmenus),
    [{ prevented: true, inTerminal: true }],
    "the browser's own menu was left in place over the pane",
  );
  assert.equal(
    await menu.locator("[data-terminal-menu-copy]").innerText(),
    "Copy screen",
    "with no selection the copy row must say it takes the screen",
  );
  assert.equal(await menu.locator("[data-terminal-menu-paste]").count(), 1, "the menu offers no Paste");
  // The menu opens at the pointer, not at some fixed corner.
  const menuBox = await menu.boundingBox();
  assert.ok(
    Math.abs(menuBox.x - (box.x + 60)) <= 1 && Math.abs(menuBox.y - (box.y + 40)) <= 1,
    `the menu opened at ${menuBox.x},${menuBox.y} rather than the click`,
  );

  // 2. Escape dismisses it and hands the keyboard back to the pane, which is
  // where the user's next keystroke belongs.
  await page.keyboard.press("Escape");
  await menu.waitFor({ state: "detached" });
  await page.waitForFunction(
    () => document.activeElement?.classList.contains("xterm-helper-textarea") === true,
    { timeout: 2000 },
  );

  // 3. Copy with no selection puts the visible screen on the system
  // clipboard. This is the case that matters: while a TUI tracks the mouse
  // an ordinary drag no longer selects, so a user who does not know the
  // force-selection modifier has only the screen to take.
  await page.evaluate(() => navigator.clipboard.writeText("stale-clipboard"));
  await screen.click({ button: "right", position: { x: 60, y: 40 } });
  await menu.waitFor();
  await menu.locator("[data-terminal-menu-copy]").click();
  await menu.waitFor({ state: "detached" });
  const copiedScreen = await waitForClipboard(page, (text) => text !== "stale-clipboard");
  for (const line of LINES) {
    assert.ok(copiedScreen.includes(line), `the copied screen is missing ${line}`);
  }

  // 4. The application tracks the mouse, so an ordinary drag is reported to
  // it and selects nothing: the menu still says it would copy the screen.
  // That is the defect Select text exists for, seen from both sides.
  const dragOverFirstLine = async () => {
    await page.mouse.move(box.x + 8, box.y + 8);
    await page.mouse.down();
    await page.mouse.move(box.x + 120, box.y + 8, { steps: 8 });
    await page.mouse.up();
  };
  // Mouse reports ride the terminal's reply channel, not the input channel:
  // an SGR sequence beginning \u001b[< is the application seeing the mouse.
  const mouseFrames = (from) => frames.slice(from).filter((frame) =>
    frame.type === "reply" && Buffer.from(frame.data, "base64").toString().startsWith("\u001b[<"));
  const inputFrames = (from) => frames.slice(from).filter((frame) => frame.type === "input");
  const dragStart = frames.length;
  await dragOverFirstLine();
  await waitFor(() => mouseFrames(dragStart).length > 0, "the drag was reported to the application");
  await screen.click({ button: "right", position: { x: 200, y: 300 } });
  await menu.waitFor();
  assert.equal(
    await menu.locator("[data-terminal-menu-copy]").innerText(),
    "Copy screen",
    "a drag under mouse capture made no selection the browser could copy",
  );

  // 5. Select text routes the drag to the browser: the same plain drag now
  // selects, nothing mouse-shaped reaches the application for it, the pane
  // carries the chip that says so, and clicking the chip hands the mouse
  // back to the application.
  await menu.locator("[data-terminal-menu-select]").click();
  await menu.waitFor({ state: "detached" });
  const chip = page.locator("[data-live-select-text]");
  await chip.waitFor();
  assert.equal(
    await chip.getAttribute("aria-pressed"),
    "true",
    "the pane shows the session routes drags to the browser",
  );
  const selectDragStart = frames.length;
  await dragOverFirstLine();
  assert.ok(
    inputFrames(selectDragStart).length === 0 && mouseFrames(selectDragStart).length === 0,
    `the select-text drag still reached the application: ${JSON.stringify(frames.slice(selectDragStart))}`,
  );
  await screen.click({ button: "right", position: { x: 200, y: 300 } });
  await menu.waitFor();
  assert.equal(
    await menu.locator("[data-terminal-menu-copy]").innerText(),
    "Copy",
    "with select text on, the plain drag must have made a selection",
  );
  await menu.locator("[data-terminal-menu-copy]").click();
  await menu.waitFor({ state: "detached" });
  const copiedSelection = await waitForClipboard(page, (text) => text !== copiedScreen);
  const fragment = copiedSelection.trim();
  assert.ok(
    fragment.length > 0 && LINES[0].includes(fragment) && fragment.length < LINES[0].length,
    `the selection copy took ${JSON.stringify(copiedSelection)} rather than a run of cells from ${LINES[0]}`,
  );
  assert.ok(
    !copiedSelection.includes(LINES[1]),
    "the selection copy took the screen instead of the dragged cells",
  );
  await chip.click();
  await chip.waitFor({ state: "hidden" });
  const offDragStart = frames.length;
  await dragOverFirstLine();
  await waitFor(() => mouseFrames(offDragStart).length > 0, "turning select text off restores mouse reporting");

  // 6. The chord checks need the plain buffer back: the application turns
  // mouse reporting off, and an ordinary drag selects natively again.
  emitOutput("\u001b[?1000l\u001b[?1006l");
  await new Promise((resolve) => setTimeout(resolve, 500));
  await dragOverFirstLine();
  if (platform === "mac") {
    await context.close();
    return;
  }

  // Windows native copy must not also send an interrupt to the session.
  await page.evaluate(() => navigator.clipboard.writeText("before-control-copy"));
  const copyStart = frames.length;
  await page.keyboard.press("Control+c");
  assert.equal(await waitForClipboard(page, (text) => text !== "before-control-copy"), copiedSelection);
  await page.keyboard.press("ArrowRight");
  await waitFor(() => frames.slice(copyStart).some((frame) => frame.type === "input"), "keyboard barrier");
  assert.ok(!frames.slice(copyStart).some((frame) => frame.type === "input"
    && Buffer.from(frame.data, "base64").includes(3)), "selection copy sent Ctrl+C to the CLI");

  // With no selection Ctrl+C still interrupts, and Ctrl+Insert copies the screen.
  await screen.click({ position: { x: 200, y: 300 } });
  const interruptStart = frames.length;
  await page.keyboard.press("Control+c");
  await waitFor(() => frames.slice(interruptStart).some((frame) => frame.type === "input"
    && Buffer.from(frame.data, "base64").toString() === "\u0003"), "Ctrl+C interrupt");
  await page.evaluate(() => navigator.clipboard.writeText("before-insert-copy"));
  await page.keyboard.press("Control+Insert");
  assert.equal(await waitForClipboard(page, (text) => text !== "before-insert-copy"), copiedScreen);

  for (const chord of ["Control+v", "Control+Shift+v", "Shift+Insert"]) {
    const text = `keyboard-paste-${chord}`;
    await page.evaluate((text) => navigator.clipboard.writeText(text), text);
    const start = frames.length;
    await page.keyboard.press(chord);
    await waitFor(() => frames.slice(start).some((frame) => frame.type === "input"
      && Buffer.from(frame.data, "base64").toString() === `\u001b[200~${text}\u001b[201~`), `${chord} bracketed paste`);
    assert.ok(!frames.slice(start).some((frame) => frame.type === "input"
      && Buffer.from(frame.data, "base64").toString() === "\u0016"), `${chord} also sent Ctrl+V`);
  }

  // Mouse-tracking applications retain their clicks; Shift+drag still selects.
  socket.send(JSON.stringify({ type: "output", data: Buffer.from("\u001b[?1000h\u001b[?1006h").toString("base64") }));
  await page.waitForFunction(() => document.querySelector(".xterm")?.classList.contains("enable-mouse-events"));
  const mouseStart = frames.length;
  await screen.click({ position: { x: 200, y: 300 } });
  await waitFor(() => frames.slice(mouseStart).some((frame) => frame.type === "reply"
    && Buffer.from(frame.data, "base64").toString().startsWith("\u001b[<")), "native CLI mouse report");
  await page.keyboard.down("Shift");
  await page.mouse.move(box.x + 8, box.y + 8);
  await page.mouse.down();
  await page.mouse.move(box.x + 120, box.y + 8, { steps: 8 });
  await page.mouse.up();
  await page.keyboard.up("Shift");
  await page.evaluate(() => navigator.clipboard.writeText("before-mouse-copy"));
  await page.keyboard.press("Control+c");
  assert.equal(await waitForClipboard(page, (text) => text !== "before-mouse-copy"), copiedSelection);
  socket.send(JSON.stringify({ type: "output", data: Buffer.from("\u001b[?1000l\u001b[?1006l").toString("base64") }));

  // 5. Paste delivers the clipboard to the session, bracketed. The route is
  // xterm's own paste path, and the brackets are the proof: an application
  // that asked for bracketed paste is told this text was pasted rather than
  // typed, which is what keeps a multi-line paste from being executed a line
  // at a time. Forwarding the text as raw input would arrive unbracketed.
  await page.evaluate(() => navigator.clipboard.writeText("pasted-into-the-pane"));
  await screen.click({ button: "right", position: { x: 60, y: 40 } });
  await menu.waitFor();
  await menu.locator("[data-terminal-menu-paste]").click();
  await menu.waitFor({ state: "detached" });
  await waitFor(
    () => frames.some((frame) => frame.type === "input" && frame.token === 7
      && Buffer.from(frame.data, "base64").toString() === "\u001b[200~pasted-into-the-pane\u001b[201~"),
    "the pasted text as a bracketed input frame",
  );

  // 6. Copy screen means the screen, on a pane that has scrolled. getLine
  // indexes the whole buffer, scrollback included, so a screen copy anchored
  // at line zero returns the oldest lines the user scrolled past instead
  // of the ones in front of them.
  const later = Array.from({ length: 60 }, (_, i) => `later-${String(i).padStart(2, "0")}`);
  socket.send(JSON.stringify({
    type: "output",
    data: Buffer.from(`${later.join("\r\n")}\r\n`).toString("base64"),
  }));
  await page.waitForFunction(
    () => document.querySelector(".xterm-rows")?.textContent?.includes("later-59"),
  );
  await page.evaluate(() => navigator.clipboard.writeText("stale-clipboard"));
  await screen.click({ button: "right", position: { x: 60, y: 40 } });
  await menu.waitFor();
  await menu.locator("[data-terminal-menu-copy]").click();
  await menu.waitFor({ state: "detached" });
  const copiedScrolled = await waitForClipboard(page, (text) => text !== "stale-clipboard");
  assert.ok(
    copiedScrolled.includes("later-59"),
    `the scrolled screen copy is missing the last painted line: ${JSON.stringify(copiedScrolled)}`,
  );
  assert.ok(
    !copiedScrolled.includes(LINES[0]),
    "the scrolled screen copy returned lines that had scrolled out of view",
  );

  // 7. What the row NAMES is what it takes. The pane keeps painting while
  // the menu is open, and output can clear the selection under it: a row
  // that read "Copy" must not quietly become a screen copy.
  await page.mouse.move(box.x + 8, box.y + 8);
  await page.mouse.down();
  await page.mouse.move(box.x + 120, box.y + 8, { steps: 8 });
  await page.mouse.up();
  await screen.click({ button: "right", position: { x: 200, y: 300 } });
  await menu.waitFor();
  assert.equal(await menu.locator("[data-terminal-menu-copy]").innerText(), "Copy");
  socket.send(JSON.stringify({
    type: "output",
    // The alternate screen replaces the whole viewport and drops the
    // selection with it — the same thing a TUI does when it starts.
    data: Buffer.from("\u001b[?1049h\u001b[2Jreplacement-screen\r\n").toString("base64"),
  }));
  await page.waitForFunction(
    () => document.querySelector(".xterm-rows")?.textContent?.includes("replacement-screen"),
  );
  await page.evaluate(() => navigator.clipboard.writeText("stale-clipboard"));
  await menu.locator("[data-terminal-menu-copy]").click();
  await menu.waitFor({ state: "detached" });
  const copiedNamed = await waitForClipboard(page, (text) => text !== "stale-clipboard");
  const named = copiedNamed.trim();
  assert.ok(
    named.length > 0 && later.some((line) => line.includes(named)),
    `a row labelled Copy took ${JSON.stringify(copiedNamed)} rather than the cells it was dragged over`,
  );
  assert.ok(
    !copiedNamed.includes("replacement-screen"),
    "a row labelled Copy took the replacement screen after output cleared the selection",
  );

  // 8. Off the pane the browser keeps its own menu: the sidebar is ordinary
  // HTML whose selection the browser's Copy can act on.
  await page.evaluate(() => { window.__contextmenus = []; });
  await page.locator('[data-region="sidebar"]').click({ button: "right", position: { x: 20, y: 20 } });
  await page.waitForFunction(() => window.__contextmenus.length === 1, { timeout: 2000 });
  assert.deepEqual(
    await page.evaluate(() => window.__contextmenus),
    [{ prevented: false, inTerminal: false }],
    "a right-click outside the pane was taken by the terminal menu",
  );
  assert.equal(await menu.count(), 0, "the terminal menu opened outside the pane");

  // The login form sends its manufactured callback only in an explicit POST,
  // clears the input while pending, and never types it into the mounted CLI.
  // The row exists only while a login is waiting. Armed while the menu is
  // open (an occupied screen the coordinator will not raise over), then the
  // menu reopens before the next interval poll can raise the dialog.
  await screen.click({ button: "right", position: { x: 60, y: 40 } });
  assert.equal(await menu.locator("[data-harness-login]").count(), 0, "the menu offers a login nobody started");
  pendingLogin = true;
  await page.evaluate(() => document.dispatchEvent(new Event("visibilitychange")));
  await page.locator("[data-live-login]").waitFor();
  await page.keyboard.press("Escape");
  await menu.waitFor({ state: "detached" });
  await screen.click({ button: "right", position: { x: 60, y: 40 } });
  assert.equal(await menu.locator("[data-harness-login]").innerText(), "Open login helper");
  await menu.locator("[data-harness-login]").click();
  const dialog = page.locator("[data-harness-login-dialog]");
  const field = dialog.getByLabel("2. Paste the return URL", { exact: true });
  const canary = "http://localhost:1455/auth/callback?code=callback-fixture-canary&state=fixture";
  await field.fill(canary);
  await dialog.getByRole("button", { name: "Finish sign-in", exact: true }).click();
  await dialog.getByText("That URL doesn’t match this sign-in.", { exact: false }).waitFor();
  assert.equal(await field.inputValue(), "");
  await field.fill(canary);
  await field.press("Enter");
  await waitFor(() => completeDelivery !== undefined, "pending callback delivery");
  assert.equal(await field.inputValue(), "", "callback retained while transport pending");
  assert.equal(await field.isHidden(), true);
  await completeDelivery();
  await dialog.getByText("Native login completed. Return to the terminal.", { exact: true }).waitFor();
  assert.equal(await dialog.innerText().then((text) => text.includes("response-fixture-canary")), false);
  assert.equal(frames.some((frame) => frame.type === "input" && Buffer.from(frame.data, "base64").toString().includes("callback-fixture-canary")), false);
  assert.equal(page.url(), origin + "/");
  assert.equal(await page.evaluate(() => JSON.stringify({ ...localStorage, ...sessionStorage }).includes("callback-fixture-canary")), false);
  assert.equal(loginRequests.filter((r) => r.action === "submit").length, 2);
  await dialog.locator("[data-login-later]").click();
  await page.setViewportSize({ width: 390, height: 844 });
  await page.locator('[data-region="sidebar-scrim"]').click({ position: { x: 380, y: 100 } });
  await page.locator("[data-live-more]").click();
  await page.locator("[data-more-popover]").waitFor();
  assert.equal(await page.locator("[data-more-popover] [data-harness-login]").count(), 0, "a finished login stays in the menu");
  assert.equal(await page.locator("[data-live-login]").count(), 0, "a finished login stays in the titlebar");
  await page.locator("[data-settings-sheet-close]").click();
  await page.locator("[data-more-popover]").waitFor({ state: "detached" });

  // The automatic coordinator: only a real listing answer raises the
  // dialog, once per attempt, and a dismissal keeps that attempt from
  // nagging again across polls, reloads and a session's own chrome. A
  // visibilitychange is a real poll trigger, so no phase waits on the
  // interval clock.
  await page.setViewportSize({ width: 1280, height: 800 });
  await page.reload({ waitUntil: "domcontentloaded" });
  await screen.locator(".xterm-rows").first().waitFor();
  attemptId = "reload-without-cleanup";
  pendingLogin = true;
  await page.evaluate(() => document.dispatchEvent(new Event("visibilitychange")));
  await dialog.waitFor();
  await field.waitFor();
  const beforeDrop = loginRequests.length;
  // Stop teardown listeners before navigation, as with a discarded tab.
  await page.evaluate(() => {
    for (const type of ["pagehide", "beforeunload", "unload"]) {
      window.addEventListener(type, event => event.stopImmediatePropagation(), { capture: true });
    }
  });
  await page.reload({ waitUntil: "domcontentloaded" });
  await screen.locator(".xterm-rows").first().waitFor();
  await page.evaluate(() => document.dispatchEvent(new Event("visibilitychange")));
  await page.locator("[data-live-login]").waitFor();
  assert.equal(loginRequests.length, beforeDrop, "reload sent no release or replacement Begin");
  await page.locator("[data-live-login]").click();
  await field.fill(canary);
  completeDelivery = undefined;
  await field.press("Enter");
  await waitFor(() => completeDelivery !== undefined, "same attempt delivery after page loss");
  assert.equal(loginRequests.findLast(request => request.action === "begin").registration, "reload-without-cleanup");
  await completeDelivery();
  await dialog.getByText("Native login completed. Return to the terminal.", { exact: true }).waitFor();
  await dialog.locator("[data-login-later]").click();

  attemptId = "auto-attempt";
  pendingLogin = true;
  await page.evaluate(() => document.dispatchEvent(new Event("visibilitychange")));
  await dialog.waitFor();
  await dialog.getByLabel("2. Paste the return URL", { exact: true }).waitFor();
  assert.equal(await dialog.getByRole("button", { name: "Finish sign-in", exact: true }).count(), 1,
    "the raised dialog binds the single waiting attempt directly");
  await dialog.locator("[data-login-close]").focus();
  await page.keyboard.press("Escape");
  await dialog.waitFor({ state: "detached" });

  await page.evaluate(() => document.dispatchEvent(new Event("visibilitychange")));
  await page.waitForTimeout(250);
  assert.equal(await page.locator("[data-harness-login-dialog]").count(), 0,
    "a dismissed attempt is not re-raised by the next poll");

  await page.locator("[data-live-login]").waitFor();
  assert.equal(await page.locator("[data-live-login]").innerText(), "Open login helper");
  holdLoginListings = true;
  await page.locator("[data-live-login]").click();
  await dialog.waitFor();
  await waitFor(() => heldLoginListings.length > 0, "delayed login listing on manual reopen");
  assert.equal(await field.isHidden(), true, "the listing cannot advance the helper before this phase releases it");
  assert.equal(await dialog.locator(".xterm-helper-textarea").evaluate(input => input === document.activeElement), true);
  const beforeNativeEscape = frames.length;
  await page.keyboard.press("Escape");
  await waitFor(() => frames.slice(beforeNativeEscape).some(frame => frame.type === "input"), "native Escape before the listing returns");
  assert.deepEqual(frames.slice(beforeNativeEscape).filter(frame => frame.type === "input").map(frame => Buffer.from(frame.data, "base64").toString()), ["\u001b"], "Escape reaches the native terminal exactly once while it owns focus");
  assert.equal(await dialog.isVisible(), true, "native Escape does not dismiss the login helper");
  holdLoginListings = false;
  await Promise.all(heldLoginListings.splice(0).map(release => release()));
  await field.waitFor();
  await dialog.locator("[data-login-close]").focus();
  await page.keyboard.press("Escape");
  await dialog.waitFor({ state: "detached" });

  const suppressedShape = await page.evaluate(() => {
    const raw = sessionStorage.getItem("noisedeck.harness-login");
    if (raw === null) return null;
    return JSON.parse(raw).map((entry) => Object.keys(entry).sort().join(","));
  });
  assert.ok(suppressedShape !== null && suppressedShape.every((keys) => keys === "expires,id,session"),
    "only opaque attempt facts persist");

  await page.reload({ waitUntil: "domcontentloaded" });
  await screen.locator(".xterm-rows").first().waitFor();
  await page.evaluate(() => document.dispatchEvent(new Event("visibilitychange")));
  await page.waitForTimeout(250);
  assert.equal(await page.locator("[data-harness-login-dialog]").count(), 0,
    "the suppression survives a reload");

  attemptId = "auto-attempt-2";
  await page.evaluate(() => document.dispatchEvent(new Event("visibilitychange")));
  await dialog.waitFor();
  await dialog.getByLabel("2. Paste the return URL", { exact: true }).waitFor();
  await field.fill("http://localhost:1457/auth/callback?code=synthetic-controller-canary&state=fixture");
  await page.evaluate(() => window.detachedLoginInput = document.querySelector("[data-login-callback]"));
  const requestsBeforeFence = loginRequests.length;
  socket.send(JSON.stringify({ type: "error", code: "fenced", message: "Another controller took over" }));
  await dialog.waitFor({ state: "detached" });
  assert.equal(loginRequests.length, requestsBeforeFence, "controller loss needs no release for an unclaimed attempt");
  assert.equal(await page.evaluate(() => window.detachedLoginInput.value), "", "controller loss clears callback input");
  assert.equal(await page.locator("[data-live-login]").count(), 0, "lost controller has no contextual login action");
  assert.equal(JSON.stringify(frames).includes("synthetic-controller-canary"), false, "callback never reaches terminal frames");

  assert.deepEqual(errors, [], `page errors: ${errors.join("; ")}`);
  await context.close();
}

async function waitForClipboard(page, accept) {
  let last = "";
  await waitFor(async () => {
    last = await page.evaluate(() => navigator.clipboard.readText());
    return accept(last);
  }, "the clipboard to change");
  return last;
}

async function waitFor(condition, label, timeoutMs = 5000) {
  const deadline = Date.now() + timeoutMs;
  for (;;) {
    if (await condition()) return;
    if (Date.now() > deadline) throw new Error(`timed out waiting for ${label}`);
    await new Promise((tick) => setTimeout(tick, 50));
  }
}
