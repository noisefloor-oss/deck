// Exercise the built live header and its preview with the same narrow-layout
// contracts. API/terminal fixtures are synthetic; the workspace and xterm are real.
import assert from "node:assert/strict";
import { createServer } from "node:http";
import { existsSync } from "node:fs";
import { mkdir, readFile } from "node:fs/promises";
import { extname, join, resolve } from "node:path";
import { browser as getBrowser } from "./browser.mjs";

const dist = resolve(import.meta.dirname, "..", "dist");
const captureDir = process.env.MOBILE_HEADER_CAPTURE_DIR;
const widths = [320, 375, 390, 600, 601, 767, 768];
const longName = "Investigate project changes with a deliberately long session label";
const mime = { ".html": "text/html", ".js": "text/javascript", ".css": "text/css", ".woff2": "font/woff2", ".png": "image/png", ".svg": "image/svg+xml" };
assert.ok(existsSync(join(dist, "preview.html")), "run pnpm build before the mobile header probe");
const streams = new Set();
const loginWatchers = new Set();
let runningBuild = "header-build-a";
const server = createServer(async (req, res) => {
  const url = new URL(req.url, "http://fixture");
  if (url.pathname.endsWith("/events") || url.searchParams.get("watch") === "1") {
    res.writeHead(200, { "Content-Type": "text/event-stream", "Cache-Control": "no-cache" });
    res.write(": connected\n\n");
    streams.add(res);
    if (url.searchParams.get("watch") === "1") loginWatchers.add(res);
    res.on("close", () => { streams.delete(res); loginWatchers.delete(res); });
    return;
  }
  const path = resolve(dist, `.${decodeURIComponent(url.pathname === "/" ? "/index.html" : url.pathname)}`);
  if (!path.startsWith(`${dist}/`) || !existsSync(path)) { res.writeHead(404); res.end(); return; }
  res.writeHead(200, { "Content-Type": mime[extname(path)] ?? "application/octet-stream" });
  const body = await readFile(path);
  res.end(url.pathname === "/" ? body.toString().replaceAll("__NOISEDECK_BUILD_ID__", runningBuild) : body);
});
await new Promise(resolveServer => server.listen(0, "127.0.0.1", resolveServer));
const origin = `http://127.0.0.1:${server.address().port}`;
if (captureDir) await mkdir(captureDir, { recursive: true });

const button = (page, name) => page.getByRole("button", { name, exact: true });
const menu = page => page.locator('[data-region="popovers"] [role="menu"]');
const panel = (page, name) => page.locator(`[data-region="${name}"]`);
const closePanel = (page, name) => page.locator({ files: "[data-files-close]", tasks: "[data-tm-close]", chats: "[data-chats-close]" }[name]);
const settled = page => page.evaluate(async () => {
  await document.fonts.ready;
  await new Promise(resolveFrame => requestAnimationFrame(() => requestAnimationFrame(resolveFrame)));
});
async function waitFor(predicate, label) {
  const deadline = Date.now() + 15000;
  while (!await predicate()) {
    assert.ok(Date.now() < deadline, `timed out: ${label}`);
    await new Promise(resolveTick => setTimeout(resolveTick, 30));
  }
}
async function capture(page, name) {
  if (captureDir) await page.screenshot({ path: join(captureDir, `${name}.png`) });
}

async function geometry(page, label, narrow, framed = false) {
  await settled(page);
  const title = page.locator("[data-titlebar-title]");
  const metrics = await page.locator("[data-workspace-titlebar]").evaluate(header => {
    const bounds = header.getBoundingClientRect();
    const rect = selector => {
      const element = header.querySelector(selector);
      if (!element || !element.getClientRects().length) return null;
      const r = element.getBoundingClientRect();
      return { x: r.x, right: r.right, width: r.width, height: r.height };
    };
    return { x: bounds.x, right: bounds.right, title: rect("[data-titlebar-title]"), settings: rect('[aria-label="Settings"]'), panels: rect('[aria-label="Panels"]'), sessions: rect('[aria-label="Sessions"]') };
  });
  assert.ok(metrics.title?.width >= 48, `${label}: session title has ${metrics.title?.width}px, needs a readable 48px floor`);
  assert.equal(await title.textContent(), longName, `${label}: label text is retained`);
  assert.ok(metrics.settings && metrics.settings.x >= metrics.x && metrics.settings.right <= metrics.right + 1, `${label}: Settings remains inside the header`);
  if (!framed) assert.ok(metrics.right <= page.viewportSize().width + 1, `${label}: header stays in viewport`);
  if (narrow) {
    assert.ok(metrics.panels && metrics.sessions, `${label}: Panels and Sessions remain separate controls`);
    assert.ok(Math.abs(metrics.panels.x - metrics.sessions.right - 4) <= 1, `${label}: Sessions and Panels retain the compact 4px gap`);
    assert.ok(Math.abs(metrics.settings.x - metrics.panels.right - 4) <= 1, `${label}: Panels sits 4px left of Settings`);
    for (const name of ["settings", "panels", "sessions"]) {
      assert.ok(metrics[name].width >= 44 && metrics[name].height >= 44, `${label}: ${name} retains a 44px touch target`);
    }
    assert.equal(await button(page, "Files").isVisible(), false, `${label}: Files is grouped`);
    assert.equal(await button(page, "Project tasks").isVisible(), false, `${label}: Tasks is grouped`);
    assert.equal(await button(page, "Chats").isVisible(), false, `${label}: Chats is grouped`);
    assert.equal(await button(page, "Assets").isVisible(), false, `${label}: Assets is grouped`);
  } else {
    assert.equal(await button(page, "Panels").isVisible(), false, `${label}: desktop retains direct panel controls`);
    for (const name of ["Files", "Project tasks", "Chats", "Assets"]) assert.equal(await button(page, name).isVisible(), true, `${label}: ${name} remains directly reachable`);
  }
}

async function openPanels(page) {
  await button(page, "Panels").click();
  await menu(page).waitFor();
  assert.equal(await button(page, "Panels").getAttribute("aria-expanded"), "true");
  assert.deepEqual(await menu(page).getByRole("menuitem").allTextContents().then(names => names.map(name => name.trim())), ["Files", "Tasks", "Chats", "Assets"]);
  for (const row of await menu(page).getByRole("menuitem").all()) {
    const box = await row.boundingBox();
    assert.ok(box && box.height >= 44, "panel menu rows remain touch sized");
  }
  const box = await menu(page).boundingBox();
  assert.ok(box && box.x >= 0 && box.x + box.width <= page.viewportSize().width + 1
    && box.y >= 0 && box.y + box.height <= page.viewportSize().height + 1,
  "the whole panel menu fits the viewport");
}

async function keyboardMenu(page) {
  await settled(page);
  await button(page, "Panels").focus();
  await page.keyboard.press("Enter");
  await menu(page).waitFor();
  const focused = () => page.evaluate(() => document.activeElement?.textContent?.trim());
  assert.equal(await focused(), "Files", "opening Panels focuses its first action");
  await page.keyboard.press("ArrowDown");
  assert.equal(await focused(), "Tasks");
  await page.keyboard.press("End");
  assert.equal(await focused(), "Assets");
  await page.keyboard.press("Home");
  assert.equal(await focused(), "Files");
  await page.keyboard.press("ArrowUp");
  assert.equal(await focused(), "Assets", "arrow navigation wraps");
  await page.keyboard.press("Escape");
  await menu(page).waitFor({ state: "detached" });
  assert.equal(await button(page, "Panels").evaluate(el => el === document.activeElement), true, "Escape restores the visible trigger");
  assert.equal(await button(page, "Panels").getAttribute("aria-expanded"), "false");
}

async function choose(page, name) {
  await openPanels(page);
  await menu(page).getByRole("menuitem", { name, exact: true }).click();
  await menu(page).waitFor({ state: "detached" });
  const selected = name.toLowerCase();
  await waitFor(() => panel(page, selected).getAttribute("data-open").then(value => value === "true"), `${name} opens`);
  for (const other of ["files", "tasks", "chats", "assets"]) {
    assert.equal(await panel(page, other).getAttribute("aria-hidden"), String(other !== selected), `${name}: only its panel is accessible`);
    assert.equal(await panel(page, other).evaluate(el => el.inert), other !== selected);
  }
}

async function panelInteractions(page, label) {
  await waitFor(() => button(page, "Panels").getAttribute("data-unseen").then(value => value === "true"), `${label}: unread Chats reaches collapsed Panels`);
  await keyboardMenu(page);
  assert.equal(await button(page, "Panels").getAttribute("data-unseen"), "true", "opening and dismissing the menu does not read Chats");
  await openPanels(page);
  await page.mouse.click(1, page.viewportSize().height - 1);
  await menu(page).waitFor({ state: "detached" });
  assert.equal(await button(page, "Panels").evaluate(el => el === document.activeElement), true, "outside dismissal restores the visible trigger");
  if (label === "live") {
    await openPanels(page);
    // The production shortcut opens only the confirmation, never enables
    // the setting. The preview does not register app-level shortcuts.
    await page.keyboard.press("Control+Alt+y");
    await page.locator("[data-more-popover]").waitFor();
    await settled(page);
    assert.equal(await page.locator("[data-more-popover]").isVisible(), true, "explicit Panels cleanup leaves the successor settings overlay usable");
    assert.equal(await menu(page).count(), 0);
    await page.locator("[data-settings-sheet-close]").click();
  }
  await choose(page, "Files");
  await closePanel(page, "files").click();
  assert.equal(await button(page, "Panels").evaluate(el => el === document.activeElement), true, "Files returns focus to visible Panels");
  assert.equal(await button(page, "Panels").getAttribute("data-unseen"), "true", "Files does not read Chats");
  await choose(page, "Tasks");
  await closePanel(page, "tasks").click();
  assert.equal(await button(page, "Panels").getAttribute("data-unseen"), "true", "Tasks does not read Chats");
  await choose(page, "Chats");
  await closePanel(page, "chats").click();
  assert.equal(await button(page, "Panels").getAttribute("data-unseen"), "false", "actually opening Chats clears its cue");
  await openPanels(page);
  await capture(page, `${label}-menu`);
  await page.setViewportSize({ width: 1000, height: 850 });
  await menu(page).waitFor({ state: "detached" });
  assert.equal(await button(page, "Panels").isVisible(), false, "widening removes the mobile menu and trigger together");
  assert.equal(await button(page, "Settings").isVisible(), true);
}

const project = { id: "header-project", name: "Header project", canonical_path: "/workspace/header", last_used_at: "2026-10-01T00:00:00Z", git_identity_setup_revision: 1 };
const session = { id: "header-session", project_id: project.id, provider: "codex", display_name: longName, status: "running", generation: 4, created_at: "2026-10-01T00:00:00Z", updated_at: "2026-10-01T00:00:00Z", recovery_capability: "none", recovery_state: "unavailable", presentation_state: "running", pane_pid: 4242, tmux_pane_id: "%1", native_session_id: "header-native" };
const secondSession = { ...session, id: "header-second", provider: "claude", display_name: "Second session", generation: 5 };
const chat = { id: "header-chat", state: "open", waiting_s: null, forced: false, opened_by: "header-agent", opened_at: session.created_at, last: session.created_at, members: ["header-agent", "peer"], left: [], remaining: [], close_requested_by: null, closed_by: null, messages: 1, handshake: [], rows: [{ id: "header-row", tag: "a2a#abc123", kind: "message", from: "peer", member: null, text: "Synthetic unread message", at: session.created_at, to: { "header-agent": "sent" } }] };

async function live(browser) {
  const context = await browser.newContext({ viewport: { width: 375, height: 850 }, hasTouch: true });
  await context.addCookies([{ name: "noisedeck_csrf", value: "header-probe", url: origin }]);
  const page = await context.newPage();
  const errors = [];
  page.on("pageerror", error => errors.push(error.message));
  let tutorial = { version: 1, dismissed: true, lesson: "welcome", step: 0, done: [], scope: "user" };
  let detailReads = 0;
  let pendingLogin = false;
  let releaseSecondDetail;
  let holdSecondDetail = true;
  let authLost = false;
  const frames = [];
  let socketCount = 0;
  let peer;
  await page.routeWebSocket(`${origin.replace("http", "ws")}/api/sessions/*/terminal?*`, socket => {
    peer = socket;
    socketCount++;
    socket.onMessage(raw => frames.push(JSON.parse(String(raw))));
    const second = new URL(socket.url()).pathname.includes(secondSession.id);
    socket.send(JSON.stringify({ type: "attached", token: second ? 18 : 17, generation: second ? 5 : 4 }));
    socket.send(JSON.stringify({ type: "output", data: Buffer.from(`${second ? "SECOND SESSION READY" : "HEADER TRANSPORT READY"}\r\n`).toString("base64") }));
  });
  await page.route(`${origin}/auth/session`, route => route.fulfill({ status: 204, headers: { "X-Noisedeck-Auth-Mode": "builtin" } }));
  await page.route(`${origin}/api/**`, async route => {
    const request = route.request();
    const url = new URL(request.url());
    const path = url.pathname;
    const json = (status, body) => route.fulfill({ status, contentType: "application/json", body: JSON.stringify(body) });
    if (path.endsWith("/events") || url.searchParams.get("watch") === "1") return route.continue();
    if (path === "/api/settings/tutorial") {
      if (request.method() === "PUT") tutorial = { ...request.postDataJSON(), scope: "user" };
      return json(200, tutorial);
    }
    if (path === "/api/settings/layout") return json(request.method() === "PUT" ? 200 : 404, request.method() === "PUT" ? request.postDataJSON() : { code: "not_found" });
    if (path === "/api/harness-login") {
      if (request.method() === "POST") {
        const { action } = request.postDataJSON();
        assert.ok(["begin", "dismiss", "cancel"].includes(action), "the fixture never opens a browser or submits credentials");
        return json(200, action === "begin" ? { id: "header-handle" } : { status: "dismissed" });
      }
      return json(200, { csrf: "header-login", pending: pendingLogin && url.searchParams.get("session") === session.id ? [{ id: "header-login", flow: "codex-browser", expires: new Date(Date.now() + 300000).toISOString() }] : [] });
    }
    // Selecting the second session is the user choosing it, and that
    // choice is remembered on the server; every other write is a defect.
    if (path === "/api/settings/last-session") {
      if (request.method() === "PUT") return json(200, request.postDataJSON());
      return json(404, { code: "not_found", message: "no last session remembered" });
    }
    assert.equal(request.method(), "GET", `header navigation must not mutate ${path}`);
    if (path === "/api/build") return json(200, { build_id: runningBuild });
    if (path === "/api/settings") return json(200, { skip_permissions: false, skip_close_confirm: false, skip_remove_project_confirm: false });
    if (path === "/api/providers") return json(200, { providers: ["codex", "shell"] });
    if (path === "/api/projects") return json(200, [project]);
    if (path === `/api/projects/${project.id}/sessions`) return json(200, [session, secondSession]);
    if (path === `/api/sessions/${session.id}`) { detailReads++; return json(200, session); }
    if (path === `/api/sessions/${secondSession.id}`) {
      if (authLost) return json(401, { code: "unauthorized", message: "Session expired" });
      if (holdSecondDetail) return new Promise(resolveResponse => {
        releaseSecondDetail = async () => {
          holdSecondDetail = false;
          await json(200, secondSession);
          resolveResponse();
        };
      });
      return json(200, secondSession);
    }
    if (path === "/api/harness-usage") return json(200, { global: [], project: [] });
    if (path === "/api/git-identities") return json(200, { profiles: [{ id: "header-identity", name: "Header probe", email: "header@example.invalid", revision: 1 }], default_profile_id: "header-identity", revision: 1 });
    if (path.endsWith("/files")) return json(200, { path: "", at_root: true, entries: [] });
    if (/^\/api\/projects\/[^/]+\/lanes$/.test(path)) return json(200, { lanes: [] });
    if (path.includes("/taskmaster")) return json(404, { code: "not_taskmaster_project", message: "Not a Tasks project" });
    if (path === `/api/sessions/${session.id}/chats`) return json(200, { agent: "header-agent", chats: [chat] });
    if (path === `/api/sessions/${secondSession.id}/chats`) return json(200, { agent: "header-second-agent", chats: [] });
    if (path === "/api/chats/status") return json(200, { available: true });
    if (path === "/api/chats") return json(200, { chats: [], sessions: { [session.id]: "header-agent" } });
    // The closed Assets panel reads the listing and follows its feed for its badge.
    if (/^\/api\/projects\/[^/]+\/assets$/.test(path)) return route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ locations: [], scanned: "2026-01-01T00:00:00Z", preview_max_bytes: 16777216 }) });
    if (/^\/api\/projects\/[^/]+\/assets\/events$/.test(path)) return route.fulfill({ status: 200, contentType: "text/event-stream", body: ": connected\n\nretry: 3600000\n\n" });
    throw new Error(`unhandled ${path}`);
  });
  try {
    await page.goto(origin, { waitUntil: "domcontentloaded" });
    await page.locator(".xterm-rows").getByText("HEADER TRANSPORT READY", { exact: false }).waitFor();
    await page.evaluate(() => { window.headerProbeTerminal = document.querySelector("[data-live-terminal]"); window.headerProbeInput = document.querySelector(".xterm-helper-textarea"); });
    for (const width of widths) {
      await page.setViewportSize({ width, height: 850 });
      await geometry(page, `live ${width}`, width < 768);
      if ([320, 375, 768].includes(width)) await capture(page, `live-${width}`);
      if (width < 768) {
        await openPanels(page);
        if ([320, 375].includes(width)) await capture(page, `live-${width}-menu`);
        await page.keyboard.press("Escape");
      }
    }
    await page.setViewportSize({ width: 375, height: 850 });
    await openPanels(page);
    await menu(page).getByRole("menuitem", { name: "Tasks", exact: true }).focus();
    const beforePoll = detailReads;
    await waitFor(() => detailReads > beforePoll, "a real session status poll while the menu is open");
    await settled(page);
    assert.equal(await menu(page).getByRole("menuitem", { name: "Tasks", exact: true }).evaluate(el => el === document.activeElement), true, "metadata repaint preserves menu focus");
    await page.keyboard.press("Escape");
    await panelInteractions(page, "live");
    assert.equal(frames.filter(frame => frame.type === "input").length, 0, "menu keys never enter the native terminal");

    // A real tutorial action must still yield completely to a selected panel.
    await page.setViewportSize({ width: 375, height: 850 });
    await button(page, "Settings").click();
    await button(page, "Show tutorial").click();
    await waitFor(() => panel(page, "tutorial").getAttribute("data-open").then(value => value === "true"), "tutorial reopens from Settings");
    await choose(page, "Files");
    assert.equal(await panel(page, "tutorial").getAttribute("aria-hidden"), "true");
    assert.equal(await panel(page, "tutorial").evaluate(el => el.inert), true);
    assert.equal(await page.locator('[data-region="tutorial-mobile-bar"]').isVisible(), false, "the tutorial bar also yields to Files");
    await closePanel(page, "files").click();
    await button(page, "Close tutorial").click();
    pendingLogin = true;
    await waitFor(() => loginWatchers.size > 0, "native login event listener");
    for (const response of loginWatchers) response.write("event: pending\ndata: {}\n\n");
    await page.locator("[data-harness-login-dialog]").waitFor();
    await button(page, "Use terminal instead").click();
    await page.locator("[data-harness-login-dialog]").waitFor({ state: "detached" });
    runningBuild = "header-build-b";
    await page.locator("[data-version-reload]").waitFor({ state: "visible", timeout: 15000 });
    for (const width of [320, 375]) {
      await page.setViewportSize({ width, height: 850 });
      await geometry(page, `contextual live ${width}`, true);
      for (const name of ["Open login helper", "New version available"]) {
        const action = button(page, name);
        const bounds = await action.boundingBox();
        const title = await page.locator("[data-titlebar-title]").boundingBox();
        assert.equal((await action.innerText()).trim(), name, `${name} remains visible text`);
        assert.ok(bounds && bounds.x >= 0 && bounds.x + bounds.width <= width + 1 && bounds.y >= title.y + title.height, `${width}px ${name} fits a separate contextual row`);
      }
      await capture(page, `live-${width}-contextual`);
    }
    assert.equal(socketCount, 1, "header navigation never reattaches the terminal");
    assert.equal(await page.evaluate(() => window.headerProbeTerminal === document.querySelector("[data-live-terminal]") && window.headerProbeInput === document.querySelector(".xterm-helper-textarea")), true, "the exact terminal DOM and input remain mounted");
    await page.locator(".xterm-helper-textarea").focus();
    await page.keyboard.type("x");
    await waitFor(() => frames.filter(frame => frame.type === "input").length === 1, "one explicit terminal keystroke");
    const input = frames.find(frame => frame.type === "input");
    assert.equal(Buffer.from(input.data, "base64").toString(), "x");
    assert.equal(input.token, 17);
    assert.equal(input.generation, 4);
    peer.send(JSON.stringify({ type: "output", data: Buffer.from("STILL SAME TERMINAL\r\n").toString("base64") }));
    await page.locator(".xterm-rows").getByText("STILL SAME TERMINAL", { exact: false }).waitFor();
    await button(page, "Sessions").click();
    await page.locator(`[data-session="${secondSession.id}"]`).click();
    await waitFor(() => typeof releaseSecondDetail === "function", "second session detail request is held");
    await settled(page);
    await capture(page, "live-switch-loading");
    assert.equal(await page.locator("[data-live-login]").count(), 0, "the prior login action disappears before the next session detail arrives");
    assert.equal(await page.locator("[data-harness-login-dialog]").count(), 0, "loading a different session cannot open an unattached login helper");
    assert.equal(socketCount, 1, "the delayed session has no terminal attachment yet");
    assert.equal(await button(page, "New version available").isVisible(), true, "the global update notice survives a session change");
    await releaseSecondDetail();
    await page.locator(".xterm-rows").getByText("SECOND SESSION READY", { exact: false }).waitFor();
    assert.equal(await page.locator("[data-live-login]").count(), 0, "a session with no login attempt has no contextual login action");
    assert.equal(socketCount, 2, "one explicit session switch attaches exactly one successor");
    await openPanels(page);
    authLost = true;
    await page.locator("[data-workspace]").waitFor({ state: "detached", timeout: 15000 });
    assert.equal(await menu(page).count(), 0, "authorization loss tears down the open menu");
    assert.equal(await page.locator("[data-live-login], [data-harness-login-dialog]").count(), 0, "authorization loss leaves no session login controls");
    assert.equal(frames.filter(frame => frame.type === "input").length, 1, "session and authorization transitions send no terminal input");
    assert.deepEqual(errors, []);
  } finally { await context.close(); }
}

async function preview(browser) {
  const context = await browser.newContext({ viewport: { width: 375, height: 850 }, hasTouch: true });
  const page = await context.newPage();
  const errors = [];
  page.on("pageerror", error => errors.push(error.message));
  try {
    await page.goto(`${origin}/preview.html`, { waitUntil: "networkidle" });
    await page.locator("html[data-preview-state]").waitFor();
    // The preview's short fixture label is extended only to measure truncation.
    await page.locator("[data-titlebar-title]").evaluate((el, name) => { el.textContent = name; }, longName);
    for (const width of widths) {
      await page.setViewportSize({ width, height: 850 });
      await geometry(page, `preview ${width}`, width < 768);
      if (width < 768) {
        await openPanels(page);
        await page.keyboard.press("Escape");
      }
    }
    await page.setViewportSize({ width: 375, height: 850 });
    await panelInteractions(page, "preview");
    await page.goto(`${origin}/preview.html?frame=phone`, { waitUntil: "networkidle" });
    await page.locator("html[data-preview-state]").waitFor();
    await page.locator("[data-titlebar-title]").evaluate((el, name) => { el.textContent = name; }, longName);
    await geometry(page, "framed preview", true, true);
    await keyboardMenu(page);
    await capture(page, "preview-framed");
    await page.setViewportSize({ width: 375, height: 850 });
    await page.goto(`${origin}/preview.html?empty=no-session`, { waitUntil: "networkidle" });
    await page.locator("html[data-preview-state]").waitFor();
    await button(page, "Panels").click();
    assert.deepEqual(await menu(page).getByRole("menuitem").allTextContents().then(names => names.map(name => name.trim())), ["Files", "Tasks"], "a project without a selected session has no Chats action");
    await page.keyboard.press("Escape");
    await capture(page, "preview-no-session");
    await page.goto(`${origin}/preview.html?empty=no-project`, { waitUntil: "networkidle" });
    await page.locator("html[data-preview-state]").waitFor();
    assert.equal(await button(page, "Panels").count(), 0, "an empty installation has no unreadable panel actions");
    assert.equal(await button(page, "Sessions").count(), 0);
    await capture(page, "preview-no-project");
    await button(page, "Settings").click();
    await page.locator("[data-more-popover]").waitFor();
    await page.locator("[data-settings-sheet-close]").click();
    assert.equal(await button(page, "Settings").evaluate(el => el === document.activeElement), true, "global settings remains usable without any project");
    assert.deepEqual(errors, []);
  } finally { await context.close(); }
}

let browser;
try {
  ({ browser } = await getBrowser());
  await live(browser);
  await preview(browser);
  console.log("mobile header probe: live and preview assertions passed");
} finally {
  await browser?.close();
  for (const response of streams) response.destroy();
  await new Promise(resolveClosed => server.close(resolveClosed));
}
