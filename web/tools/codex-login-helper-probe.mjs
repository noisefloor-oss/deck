// The Codex login helper, measured in a real browser against the real build.
//
// The unit suite can pin the dialog markup, but not the contract the user
// depends on: that attaching a Codex session opens the helper over the SAME
// terminal (reparented, never cloned, one socket), that dismissing restores
// the pane, that a live SSE pending event updates an already-open helper
// without nagging a dismissed one, and that losing the controller tears the
// dialog down without stranding the terminal inside it. All of these need a
// real EventSource and a mounted xterm with a fixture terminal transport.
import assert from "node:assert/strict";
import { createServer } from "node:http";
import { mkdir, readFile, writeFile } from "node:fs/promises";
import { existsSync } from "node:fs";
import { extname, join, resolve } from "node:path";
import { browser as getBrowser } from "./browser.mjs";

const WEB = resolve(import.meta.dirname, "..");
const DIST = join(WEB, "dist");
const captureDir = process.env.CODEX_LOGIN_CAPTURE_DIR;
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

// Held event-stream responses per session, so the probe can raise real SSE
// events on demand. Every write is fenced: a destroyed response must never
// take the dist server down with it.
const watchers = new Map();
const emitWatch = (session, event, data) => {
  const res = watchers.get(session);
  assert.ok(res && !res.destroyed, "the selected session has a live event stream");
  res.write(`event: ${event}\ndata: ${JSON.stringify(data)}\n\n`);
};

const server = createServer(async (req, res) => {
  const url = new URL(req.url, "http://x");
  const pathname = decodeURIComponent(url.pathname);
  if (pathname === "/api/harness-login" && url.searchParams.get("watch") === "1") {
    res.writeHead(200, {
      "Content-Type": "text/event-stream",
      "Cache-Control": "no-cache",
      Connection: "keep-alive",
    });
    res.write(": connected\n\n");
    const session = url.searchParams.get("session");
    watchers.set(session, res);
    res.on("close", () => { if (watchers.get(session) === res) watchers.delete(session); });
    return;
  }
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
  name: "helper-project",
  canonical_path: "/workspace/menu-project",
  last_used_at: "2026-09-06T00:00:00Z",
};
const existing = {
  id: "existing-session",
  project_id: project.id,
  provider: "claude",
  display_name: "",
  status: "running",
  generation: 1,
  created_at: "2026-09-06T00:00:00Z",
  updated_at: "2026-09-06T00:00:00Z",
  recovery_capability: "none",
  recovery_state: "unavailable",
  presentation_state: "running",
};
const codex = {
  id: "codex-new",
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
const LINES = Array.from({ length: 24 }, (_, i) => `line-${String(i).padStart(2, "0")}-abcdefghij`);
const EXPIRY = () => new Date(Date.now() + 300000).toISOString();

let browser;
try {
  ({ browser } = await getBrowser());
  await run(false);
  await run();
  await run(false, "codex");
  await run(true, "codex");
  await run(true, "shell");
  await run(true, "codex", true);
  await run(true, null, false, true);
  console.log("codex login helper probe: all assertions passed");
} finally {
  for (const res of watchers.values()) res.destroy();
  watchers.clear();
  await browser?.close();
  await new Promise((closed) => server.close(closed));
}

function waitFor(fn, label) {
  const deadline = Date.now() + 5000;
  return (async function tick() {
    while (!fn()) {
      if (Date.now() > deadline) throw new Error(`timed out waiting for ${label}`);
      await new Promise((r) => setTimeout(r, 25));
    }
  })();
}

async function run(initialHint = true, recoveryProvider = null, delayedCreation = false, signedInElsewhere = false) {
  const priorSession = recoveryProvider === null ? existing : {
    ...existing, provider: recoveryProvider, status: "exited", presentation_state: "resume_ended",
  };
  const createdSession = { ...codex, provider: recoveryProvider ?? "codex" };
  const otherSession = { ...existing, id: "other-live", display_name: "Other session" };
  const context = await browser.newContext({ viewport: { width: 1280, height: 900 } });
  await context.addInitScript(() => {
    const NativeEventSource = window.EventSource;
    window.EventSource = class extends NativeEventSource {
      constructor(...args) {
        super(...args);
        this.addEventListener("pending", () => { window.__loginPendingEvents = (window.__loginPendingEvents ?? 0) + 1; });
      }
    };
    Object.defineProperty(navigator, "userAgentData", { value: { platform: "Windows" } });
    Object.defineProperty(navigator, "platform", { value: "Win32" });
  });
  await context.addCookies([{ name: "noisedeck_csrf", value: "cookie-csrf", url: origin }]);
  const page = await context.newPage();
  await page.clock.install();
  const errors = [];
  page.on("pageerror", (error) => errors.push(error.message));
  const capture = async state => {
    if (captureDir) await mkdir(captureDir, { recursive: true });
    const defects = [];
    const viewports = [["desktop", { width: 1280, height: 900 }], ["phone", { width: 393, height: 852 }], ["phone-short", { width: 393, height: 600 }]];
    if (state === "initial") viewports.push(["phone-narrow", { width: 320, height: 600 }]);
    for (const [name, viewport] of viewports) {
      await page.setViewportSize(viewport);
      await page.evaluate(async () => { await document.fonts.ready; await new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve))); });
      const roles = await page.locator('[data-harness-login-dialog] :is([data-login-intro], [data-login-warning], [data-login-status], [data-login-field-label], [data-login-privacy]):visible').evaluateAll(elements => elements.map(element => {
        const style = getComputedStyle(element);
        return [style.fontFamily, style.fontSize, style.fontWeight, style.color, style.lineHeight];
      }));
      for (const role of roles) assert.deepEqual(role, roles[0], `${state} ${name}: instructional text shares one semantic typography role`);
      assert.equal(await page.locator('[data-harness-login-dialog] .dialog-desc').isHidden(), true);
      assert.equal(await page.locator('[data-harness-login-dialog]').innerText().then(text => /For the selected session|Waiting for Codex sign-in/.test(text)), false, 'redundant session and waiting copy stays absent');
      if (state === 'ready' && viewport.width < 640) assert.ok(Number.parseFloat(await page.locator('[data-login-callback]').evaluate(input => getComputedStyle(input).fontSize)) >= 16, 'shared mobile editable-field role prevents input zoom');
      const terminal = await page.locator("[data-harness-login-dialog] [data-live-terminal]").boundingBox();
      assert.ok(terminal && terminal.width > 0 && terminal.height > 0 && terminal.y < viewport.height && terminal.y + terminal.height > 0, "the helper keeps the native terminal visible");
      const geometry = await page.evaluate(() => Object.fromEntries(["[data-harness-login-dialog]", "[data-login-terminal]", "[data-live-terminal]", ".xterm-screen", "[data-harness-login-dialog] .dialog-body", "[data-harness-login-dialog] .dialog-foot", "[data-login-later]"].map(selector => {
        const element = document.querySelector(selector);
        const { x, y, width, height } = element.getBoundingClientRect();
        return [selector, { x, y, width, height, scrollHeight: element.scrollHeight, clientHeight: element.clientHeight }];
      })));
      if (captureDir) {
        await writeFile(join(captureDir, `${state}-${name}.json`), JSON.stringify(geometry, null, 2));
        await page.screenshot({ path: join(captureDir, `${state}-${name}.png`) });
      }
      const screen = geometry[".xterm-screen"];
      const native = geometry["[data-live-terminal]"];
      const rowHeight = await page.locator(".xterm-rows > div").first().evaluate(el => el.getBoundingClientRect().height);
      if (native.height - screen.height > rowHeight + 2) defects.push(`${state} ${name}: terminal height ${native.height}, row area ${screen.height}`);
      const clippedButtons = await page.locator("[data-harness-login-dialog] .dialog-foot button:visible").evaluateAll(buttons => buttons.filter(button => {
        const rect = button.getBoundingClientRect();
        const dialog = button.closest("[data-harness-login-dialog]").getBoundingClientRect();
        return rect.y < Math.max(0, dialog.y) || rect.bottom > Math.min(innerHeight, dialog.bottom) + 1;
      }).map(button => button.textContent.trim()));
      if (clippedButtons.length) defects.push(`${state} ${name}: clipped footer actions ${clippedButtons.join(", ")}`);
      if (state === "initial") {
        const check = page.locator("[data-harness-login-dialog] .dialog-foot [data-login-refresh]");
        assert.equal(await check.innerText(), "Check for login");
        const style = await check.evaluate(button => ({ border: Number.parseFloat(getComputedStyle(button).borderTopWidth), width: button.getBoundingClientRect().width, height: button.getBoundingClientRect().height, dialogWidth: button.closest('[role=dialog]').getBoundingClientRect().width }));
        assert.ok(style.border > 0 && style.width < style.dialogWidth - 40, `${name}: the login check is a compact outlined footer button`);
        if (name !== "desktop") assert.ok(style.height >= 44, `${name}: the login check retains a touch-sized target`);
        const checkBox = await check.boundingBox();
        const laterBox = await page.locator("[data-login-later]").boundingBox();
        if (viewport.width === 393) assert.ok(Math.abs(checkBox.y - laterBox.y) <= 1, `${name}: pending footer buttons share one compact row`);
        if (viewport.width === 320) assert.ok(laterBox.y >= checkBox.y + checkBox.height, "narrow phone footer wraps its touch targets without overlap");
      }
    }
    await page.setViewportSize({ width: 1280, height: 900 });
    assert.deepEqual(defects, [], "native terminal fits its region and every footer action stays reachable");
  };

  let codexCreated = false;
  let releaseCreation;
  let creationCount = 0;
  let pendingLogin = false;
  let claimedLogin = false;
  let listGets = 0;
  let listedHint = initialHint;
  let sessionListings = 0;
  let holdEmptyListing = false;
  let releaseEmptyListing;
  await page.route(`${origin}/auth/session`, (route) => route.fulfill({
    status: 204,
    headers: { "X-Noisedeck-Auth-Mode": "builtin" },
  }));
  await page.route(`${origin}/api/**`, (route) => {
    const request = route.request();
    const url = new URL(request.url());
    const path = url.pathname;
    const json = (status, body) => route.fulfill({ status, contentType: "application/json", body: JSON.stringify(body) });
    const stream = () => route.fulfill({
      status: 200,
      contentType: "text/event-stream",
      body: ": connected\n\nretry: 3600000\n\n",
    });
    if (path === "/api/harness-login") {
      // The watch stream is a real EventSource served by the dist server; it
      // must pass through untouched or the probe measures a fixture echo.
      if (url.searchParams.get("watch") === "1") return route.continue();
      if (request.method() === "GET") {
        if (url.searchParams.get("session") !== "codex-new") return json(200, { csrf: "login-csrf", pending: [] });
        listGets += 1;
        if (holdEmptyListing) {
          holdEmptyListing = false;
          return new Promise(resolve => {
            releaseEmptyListing = async () => {
              await json(200, { csrf: "login-csrf", pending: [] });
              resolve();
            };
          });
        }
        return json(200, {
          csrf: "login-csrf",
          pending: pendingLogin && !claimedLogin ? [{ id: "native-attempt", flow: "codex-browser", expires: EXPIRY() }] : [],
        });
      }
      assert.equal(request.method(), "POST");
      assert.equal(request.headers()["x-harness-login-csrf"], "login-csrf");
      assert.equal(request.headers()["x-csrf-token"], "cookie-csrf");
      const body = request.postDataJSON();
      if (body.action === "begin") {
        if (!pendingLogin || claimedLogin || body.registration !== "native-attempt") return json(409, {});
        claimedLogin = true;
        return json(200, { id: "one-use-handle" });
      }
      if (body.action === "authorization") return json(200, { url: "https://auth.openai.com/oauth/authorize?state=synthetic-state" });
      if (body.action === "dismiss") { claimedLogin = false; return json(200, { status: "dismissed" }); }
      if (body.action === "cancel") { pendingLogin = false; claimedLogin = false; return json(200, { status: "cancelled" }); }
      assert.equal(body.action, "submit");
      pendingLogin = false;
      claimedLogin = false;
      return json(200, { status: "completed" });
    }
    if (request.method() === "GET" && path === "/api/harness-usage") return json(200, { global: [], project: [] });
    if (request.method() === "GET" && path === "/api/session-activity/events") return stream();
    if (request.method() === "GET" && path === "/api/providers") return json(200, { providers: ["codex", "claude"] });
    if (request.method() === "GET" && path === "/api/settings/last-session") return json(404, { code: "not_found", message: "no last session remembered" });
    if (request.method() === "PUT" && path === "/api/settings/last-session") return json(200, request.postDataJSON());
    if (request.method() === "GET" && path === "/api/settings/layout") return json(404, { code: "not_found", message: "no layout saved" });
    if (request.method() === "PUT" && path === "/api/settings/layout") return json(200, { saved: true });
    if (request.method() === "GET" && path === "/api/settings/tutorial") return json(200, { version: 1, dismissed: true, lesson: "welcome", step: 0, done: [], scope: "user" });
    if (request.method() === "GET" && path === "/api/settings") return json(200, { skip_permissions: false, skip_close_confirm: false, skip_remove_project_confirm: false });
    if (request.method() === "GET" && path === "/api/projects") return json(200, [project]);
    if (request.method() === "GET" && path === "/api/git-identities") {
      return json(200, {
        profiles: [{ id: "identity-1", name: "Deck probe", email: "deck@example.invalid", revision: 1, created_at: "t", updated_at: "t" }],
        default_profile_id: "identity-1",
        revision: 1,
      });
    }
    if (request.method() === "POST" && path === `/api/projects/${project.id}/sessions`) {
      assert.equal(request.postDataJSON().provider, createdSession.provider);
      creationCount++;
      codexCreated = true;
      if (delayedCreation) return new Promise(resolve => {
        releaseCreation = async () => {
          await json(201, { ...createdSession, codex_login_helper: initialHint });
          resolve();
        };
      });
      return json(201, { ...createdSession, codex_login_helper: initialHint });
    }
    if (request.method() === "GET" && path === `/api/projects/${project.id}/sessions`) {
      // The server re-checks the hint on every read and omits it once false.
      sessionListings++;
      const listed = { ...createdSession, ...(listedHint ? { codex_login_helper: true } : {}) };
      return json(200, [priorSession, ...(delayedCreation ? [otherSession] : []), ...(codexCreated ? [listed] : [])]);
    }
    if (request.method() === "GET" && path === `/api/sessions/${existing.id}/screen`) return route.fulfill({ status: 200, contentType: "text/plain", body: "Synthetic native resume ended" });
    for (const s of [priorSession, createdSession, ...(delayedCreation ? [otherSession] : [])]) {
      if (request.method() === "GET" && path === `/api/sessions/${s.id}`) {
        return json(200, { ...s, pane_pid: 4242, tmux_pane_id: "%1", native_session_id: `native-${s.id}` });
      }
    }
    if (/^\/api\/projects\/[^/]+\/lanes$/.test(path)) return json(200, { lanes: [] });
    if (request.method() === "GET" && path === `/api/projects/${project.id}/taskmaster`) return json(404, { error: "not_taskmaster_project", message: "not a task project" });
    if (request.method() === "GET" && path === `/api/projects/${project.id}/taskmaster/events`) return stream();
    if (request.method() === "GET" && /^\/api\/sessions\/[^/]+\/chats\/events$/.test(path)) return stream();
    if (request.method() === "GET" && /^\/api\/sessions\/[^/]+\/chats$/.test(path)) return json(200, { agent: "helper-sess", chats: [] });
    if (request.method() === "GET" && path === "/api/chats/status") return json(200, { available: true });
    if (request.method() === "GET" && path === "/api/chats") return json(200, { chats: [] });
    // The closed Assets panel reads the listing and follows its feed for its badge.
    if (/^\/api\/projects\/[^/]+\/assets$/.test(path)) return route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ locations: [], scanned: "2026-01-01T00:00:00Z", preview_max_bytes: 16777216 }) });
    if (/^\/api\/projects\/[^/]+\/assets\/events$/.test(path)) return route.fulfill({ status: 200, contentType: "text/event-stream", body: ": connected\n\nretry: 3600000\n\n" });
    throw new Error(`unhandled ${request.method()} ${path}`);
  });

  const frames = [];
  const sockets = new Map();
  const openCount = new Map();
  let releaseAttach;
  await page.routeWebSocket(`${origin.replace("http", "ws")}/api/sessions/*/terminal?*`, (peer) => {
    const id = new URL(peer.url()).pathname.split("/")[3];
    sockets.set(id, peer);
    openCount.set(id, (openCount.get(id) ?? 0) + 1);
    peer.onMessage((message) => {
      if (typeof message === "string") frames.push({ session: id, frame: JSON.parse(message) });
    });
    if (id === "codex-new") {
      // Attachment is held back so the probe can observe the pre-attach
      // state: mounted terminal, live socket, no helper yet.
      releaseAttach = () => {
        peer.send(JSON.stringify({ type: "attached", token: 7, generation: 1 }));
        peer.send(JSON.stringify({ type: "output", data: Buffer.from(`\u001b[?2004h${LINES.join("\r\n")}\r\n`).toString("base64") }));
      };
      return;
    }
    peer.send(JSON.stringify({ type: "attached", token: 1, generation: 1 }));
    peer.send(JSON.stringify({ type: "output", data: Buffer.from(`\u001b[?2004h${LINES.join("\r\n")}\r\n`).toString("base64") }));
  });

  await page.goto(origin, { waitUntil: "domcontentloaded" });
  if (recoveryProvider !== null) {
    if (delayedCreation) await waitFor(() => openCount.get(otherSession.id) === 1, "initial other-session controller");
    await page.locator('[data-session="existing-session"]').click();
    await page.getByRole("button", { name: "Start fresh session", exact: true }).click();
    if (delayedCreation) {
      await waitFor(() => releaseCreation !== undefined, "delayed recovery creation response");
      await page.locator('[data-session="other-live"]').click();
      await waitFor(() => openCount.get(otherSession.id) === 2, "user-selected other session");
      const response = page.waitForResponse(response => response.request().method() === "POST" && new URL(response.url()).pathname === `/api/projects/${project.id}/sessions`);
      await releaseCreation();
      await (await response).finished();
      await page.evaluate(() => new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve))));
      assert.equal(await page.locator('[data-session="other-live"]').getAttribute("aria-selected"), "true", "a late recovery response cannot steal the user's selected session");
      assert.equal(openCount.get(codex.id) ?? 0, 0, "the stale response opens no controller for its created session");
      assert.equal(await page.locator("[data-harness-login-dialog]").count(), 0);
      assert.equal(creationCount, 1);
      assert.equal(frames.some(({ frame }) => frame.type === "input"), false);
      assert.deepEqual(errors, []);
      await context.close();
      return;
    }
  } else {
    await page.locator("[data-live-terminal] .xterm").waitFor();
    await waitFor(() => openCount.get("existing-session") === 1, "initial terminal WebSocket");
    await page.waitForFunction(() => document.querySelector(".xterm-rows")?.textContent?.includes("line-20-abcdefghij"));
    // Create the Codex session and hold the attach until the probe releases it.
    await page.locator('[data-project="project-menu"] [data-new-session]').focus();
    await page.keyboard.press("Enter");
    await page.locator('[data-new-session-provider="codex"]').click();
  }
  await waitFor(() => openCount.get("codex-new") === 1, "codex terminal WebSocket");
  assert.equal(creationCount, 1, "one launch gesture creates exactly one session");
  await page.waitForFunction(() => document.querySelector("[data-live-terminal] .xterm") !== null);
  assert.equal(await page.locator("[data-harness-login-dialog]").count(), 0);
  const host0 = await page.evaluateHandle(() => document.querySelector("[data-live-terminal]"));
  const xterm0 = await page.evaluateHandle(() => document.querySelector("[data-live-terminal] .xterm"));

  releaseAttach();
  if (createdSession.provider === "codex") await waitFor(() => watchers.has("codex-new"), "harness-login watch stream");
  if (!initialHint || createdSession.provider !== "codex") {
    await waitFor(() => listGets > 0, "normal attached session login listing");
    await page.evaluate(() => new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve))));
    assert.equal(await page.locator("[data-harness-login-dialog]").count(), 0, "a normal Codex launch remains in its native terminal");
    assert.equal(await page.locator("[data-live-login]").count(), 0, "no waiting login is invented");
    assert.equal(openCount.get(codex.id), 1);
    assert.deepEqual(errors, []);
    await context.close();
    return;
  }
  await page.locator("[data-harness-login-dialog]").waitFor();
  assert.equal(await page.locator('.xterm-helper-textarea').evaluate(input => input === document.activeElement), true, 'initial helper focuses the existing native terminal without a click');
  // Same DOM host reparented into the dialog — not a clone, not a second mount.
  assert.equal(await page.evaluate(([a, b]) => a === b, [host0, await page.evaluateHandle(() => document.querySelector("[data-live-terminal]"))]), true);
  assert.equal(await page.evaluate(([a, b]) => a === b, [xterm0, await page.evaluateHandle(() => document.querySelector("[data-live-terminal] .xterm"))]), true);
  assert.equal(await page.evaluate((h) => h.closest("[data-harness-login-dialog]") !== null, host0), true);
  assert.equal(openCount.get("codex-new"), 1);
  if (signedInElsewhere) {
    // Signing in inside the terminal never completes the helper; the next
    // listing that reports no hint must still take every login entry away.
    const helper = page.locator("[data-harness-login-dialog]");
    await helper.locator("[data-login-later]").click();
    await helper.waitFor({ state: "detached" });
    const chromeLogin = page.locator("[data-live-login]");
    await chromeLogin.waitFor();
    const terminalMenu = page.locator("[data-terminal-menu-popover]");
    const openMenus = async () => {
      await page.locator("[data-live-more]").click();
      await page.locator("[data-more-popover]").waitFor();
      const more = await page.locator("[data-more-popover] [data-harness-login]").count();
      await page.locator("[data-popover-backdrop]").click({ position: { x: 400, y: 600 } });
      await page.locator("[data-more-popover]").waitFor({ state: "detached" });
      await page.locator(".xterm-screen").click({ button: "right", position: { x: 60, y: 40 } });
      await terminalMenu.waitFor();
      const terminal = await terminalMenu.locator("[data-harness-login]").count();
      await page.keyboard.press("Escape");
      await terminalMenu.waitFor({ state: "detached" });
      return { more, terminal };
    };
    assert.deepEqual(await openMenus(), { more: 1, terminal: 1 }, "both menus offer the helper while the listing reports it");
    const listings = sessionListings;
    await page.clock.fastForward(10_000);
    await waitFor(() => sessionListings > listings, "a sidebar refresh that still reports the hint");
    await page.evaluate(() => new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve))));
    assert.equal(await chromeLogin.count(), 1, "a listing that still reports the hint keeps the helper");
    // The terminal menu stays open across the refresh that drops the hint.
    await page.locator(".xterm-screen").click({ button: "right", position: { x: 60, y: 40 } });
    await terminalMenu.waitFor();
    const staleRow = await terminalMenu.locator("[data-harness-login]").elementHandle();
    listedHint = false;
    const signedIn = sessionListings;
    await page.clock.fastForward(10_000);
    await waitFor(() => sessionListings > signedIn, "a sidebar refresh after signing in");
    await chromeLogin.waitFor({ state: "detached" });
    await terminalMenu.locator("[data-harness-login]").waitFor({ state: "detached" });
    assert.equal(await terminalMenu.count(), 1, "dropping the row leaves the open menu in place");
    // A click already aimed at the row still reaches its handler.
    await staleRow.evaluate((row) => row.click());
    await page.evaluate(() => new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve))));
    assert.equal(await helper.count(), 0, "a stale menu row never reopens the helper");
    assert.equal(await chromeLogin.count(), 0, "a stale menu row never restores the helper");
    assert.deepEqual(await openMenus(), { more: 0, terminal: 0 }, "neither menu offers the helper after signing in");
    assert.equal(await helper.count(), 0, "signing in elsewhere never reopens the helper");
    assert.equal(frames.some(({ frame }) => frame.type === "input"), false, "dropping the helper never types into Codex");
    assert.deepEqual(errors, []);
    await context.close();
    return;
  }
  if (recoveryProvider !== null) {
    const helper = page.locator("[data-harness-login-dialog]");
    assert.equal(openCount.get(existing.id) ?? 0, 0, "starting fresh never reconnects the ended session");
    await helper.locator("[data-login-later]").click();
    await helper.waitFor({ state: "detached" });
    assert.equal(await host0.evaluate(host => host.isConnected && host.closest("[data-harness-login-dialog]") === null), true);
    await page.locator("[data-live-login]").click();
    await helper.waitFor({ state: "visible" });
    assert.equal(await page.evaluate(node => node === document.querySelector("[data-live-terminal] .xterm"), xterm0), true);
    assert.equal(openCount.get(codex.id), 1, "recovery helper reopening keeps the same transport");
    sockets.get(codex.id).send(JSON.stringify({ type: "error", code: "fenced", message: "Another controller took over" }));
    await helper.waitFor({ state: "detached" });
    assert.equal(await page.locator("[data-live-login]").count(), 0);
    assert.equal(await host0.evaluate(host => host.isConnected && host.closest("[data-harness-login-dialog]") === null), true);
    assert.equal(frames.some(({ frame }) => frame.type === "input"), false, "recovery and helper navigation never type into Codex");
    assert.deepEqual(errors, []);
    await context.close();
    return;
  }
  await capture("initial");

  const dialog = page.locator("[data-harness-login-dialog]");
  const field = dialog.locator("[data-login-callback]");
  const chromeAction = page.locator("[data-live-login]");
  const inputFrames = () => frames.filter(({ frame }) => frame.type === "input");
  const codexInput = () => inputFrames().filter(({ session, frame }) => session === codex.id && frame.token === 7 && frame.generation === 1)
    .map(({ frame }) => Buffer.from(frame.data, "base64").toString()).join("");
  const sameTerminal = async () => {
    assert.equal(await page.evaluate(h => h === document.querySelector("[data-live-terminal]"), host0), true);
    assert.equal(await page.evaluate(x => x === document.querySelector("[data-live-terminal] .xterm"), xterm0), true);
    assert.equal(openCount.get(codex.id), 1, "moving the terminal never creates another controller socket");
  };
  const output = async (text, marker) => {
    sockets.get(codex.id).send(JSON.stringify({ type: "output", data: Buffer.from(text).toString("base64") }));
    await page.waitForFunction(marker => document.querySelector(".xterm-rows")?.textContent?.includes(marker), marker);
  };
  const scrollState = () => page.evaluate(() => ({
    viewport: document.querySelector(".xterm-viewport").scrollTop,
    rows: document.querySelector(".xterm-rows").textContent,
    body: document.querySelector("[data-harness-login-dialog] .dialog-body")?.scrollTop ?? null,
    pageX: scrollX,
    pageY: scrollY,
  }));
  const wheelOverTerminal = async (deltaX, deltaY) => {
    const point = await page.locator(".xterm-screen").evaluate(element => {
      const screen = element.getBoundingClientRect();
      const body = element.closest("[data-harness-login-dialog]")?.querySelector(".dialog-body").getBoundingClientRect();
      return { x: screen.x + screen.width / 2, y: (Math.max(screen.top, body?.top ?? 0) + Math.min(screen.bottom, body?.bottom ?? innerHeight)) / 2 };
    });
    await page.mouse.move(point.x, point.y);
    await page.mouse.wheel(deltaX, deltaY);
    await page.evaluate(() => new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve))));
    await page.waitForTimeout(50);
  };
  const assertWheelContained = async label => {
    await page.locator("[data-login-terminal]").scrollIntoViewIfNeeded();
    const before = await scrollState();
    const beforeFrames = frames.length;
    for (const [x, y] of [[0, -180], [0, 180], [-180, 0], [180, 0], [0, -7], [0, -7], [0, 7], [3, -4]]) {
      await wheelOverTerminal(x, y);
      assert.deepEqual(await scrollState(), before, `${label}: wheel/trackpad cannot move the terminal, dialog body or page`);
    }
    assert.deepEqual(frames.slice(beforeFrames).filter(({ frame }) => ["scroll", "input", "reply"].includes(frame.type)), [], `${label}: wheel never reaches native input or backend scroll transport`);
    await sameTerminal();
  };

  assert.equal(await dialog.locator("[data-login-later]").innerText(), "Use terminal instead");
  assert.equal(inputFrames().length, 0, "opening the helper sends no invented native keystrokes");
  assert.equal(await dialog.locator('[data-login-intro]').innerText(), 'Choose a sign-in option in Codex below.');
  assert.equal(await dialog.locator('[data-login-status]').isHidden(), true);
  const expectKeys = async (keys, expected, label) => {
    const before = inputFrames().length;
    for (const key of keys) await page.keyboard.press(key);
    await waitFor(() => inputFrames().length >= before + expected.length, label);
    await page.evaluate(() => new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve))));
    const actual = inputFrames().slice(before);
    assert.deepEqual(actual.map(({ frame }) => Buffer.from(frame.data, 'base64').toString()), expected, `${label}: each key arrives exactly once`);
    assert.ok(actual.every(({ session, frame }) => session === codex.id && frame.token === 7 && frame.generation === 1), `${label}: the existing controller owns every input`);
    await sameTerminal();
  };
  await expectKeys(['ArrowUp', 'ArrowDown', 'Enter'], ['\u001b[A', '\u001b[B', '\r'], 'initial native navigation without a terminal click');
  for (const selector of ['.dialog-title', '[data-login-intro]', '.dialog-head']) {
    await dialog.locator(selector).click(selector === '.dialog-head' ? { position: { x: 12, y: 20 } } : {});
    assert.equal(await page.locator('.xterm-helper-textarea').evaluate(input => input === document.activeElement), true, `${selector}: clicking static helper content restores native focus`);
    await expectKeys(['ArrowUp', 'ArrowDown', 'Enter'], ['\u001b[A', '\u001b[B', '\r'], `${selector} native navigation`);
  }
  // A focusable modal itself is the fallback path, using xterm's current
  // application-cursor mode instead of inventing fixed arrow sequences.
  await output('\u001b[?1happlication-cursor-enabled\r\n', 'application-cursor-enabled');
  for (const [key, bytes] of [['ArrowUp', '\u001bOA'], ['ArrowDown', '\u001bOB'], ['Enter', '\r']]) {
    await dialog.focus();
    await expectKeys([key], [bytes], 'modal background fallback');
  }
  await output('\u001b[?1lnormal-cursor-restored\r\n', 'normal-cursor-restored');
  const beforeInteractive = inputFrames().length;
  const check = dialog.locator('[data-login-refresh]');
  await check.focus();
  const checkGets = listGets;
  await page.keyboard.press('Enter');
  await waitFor(() => listGets > checkGets, 'keyboard activation of Check for login');
  await page.evaluate(() => new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve))));
  assert.equal(await check.evaluate(button => button === document.activeElement), true, 'an empty check keeps its button focus');
  await page.waitForTimeout(1100);
  assert.equal(await check.evaluate(button => button === document.activeElement), true, 'fallback polling never steals focused control');
  assert.equal(inputFrames().length, beforeInteractive, 'Enter on Check for login does not type into Codex');
  const intro = dialog.locator('[data-login-intro]');
  const introBox = await intro.boundingBox();
  await page.mouse.move(introBox.x + 2, introBox.y + introBox.height / 2);
  await page.mouse.down();
  await page.mouse.move(introBox.x + 170, introBox.y + introBox.height / 2, { steps: 8 });
  await page.mouse.up();
  assert.ok(await page.evaluate(() => (getSelection()?.toString().length ?? 0) > 0), 'helper instruction text stays selectable');
  assert.equal(await page.locator('.xterm-helper-textarea').evaluate(input => input === document.activeElement), false, 'text selection does not refocus the native terminal');
  await page.evaluate(() => getSelection()?.removeAllRanges());
  await dialog.focus();
  await page.keyboard.press('Shift+ArrowUp');
  assert.equal(inputFrames().length, beforeInteractive, 'modified background keys are not forwarded');
  await page.keyboard.press('Tab');
  assert.equal(await dialog.locator('[data-login-close]').evaluate(button => button === document.activeElement), true, 'Tab keeps the ordinary dialog focus order');
  await output(Array.from({ length: 100 }, (_, i) => `helper-history-${i}\r\n`).join("") + "helper-live-output\r\n", "helper-live-output");
  assert.equal((await scrollState()).rows.includes("helper-history-0"), false, "the wheel check starts after earlier fixture lines have left the viewport");
  await assertWheelContained("normal buffer");
  await output("\u001b[?1049h\u001b[2J\u001b[Hhelper-alternate-output\r\n", "helper-alternate-output");
  await assertWheelContained("alternate buffer");
  await output("\u001b[?1000h\u001b[?1006hhelper-mouse-output\r\n", "helper-mouse-output");
  await page.waitForFunction(() => document.querySelector(".xterm")?.classList.contains("enable-mouse-events"));
  await assertWheelContained("native mouse tracking");
  await output("\u001b[?1000l\u001b[?1006l\u001b[?1049lhelper-live-restored\r\n", "helper-live-restored");
  const firstRow = page.locator(".xterm-rows > div").first();
  const rowBox = await firstRow.boundingBox();
  await page.mouse.move(rowBox.x + 2, rowBox.y + rowBox.height / 2);
  await page.mouse.down();
  await page.mouse.move(rowBox.x + 95, rowBox.y + rowBox.height / 2, { steps: 8 });
  await page.mouse.up();
  await page.locator(".xterm-selection div").first().waitFor({ state: "visible" });
  assert.ok(await page.locator(".xterm-selection div").count() > 0, "native mouse selection still paints selected terminal cells");
  await page.locator(".xterm-helper-textarea").focus();
  await page.keyboard.type("native-probe");
  await waitFor(() => codexInput().includes("native-probe"), "native input on the existing controller");
  await page.keyboard.press("Escape");
  await waitFor(() => codexInput().includes("\u001b"), "native Escape input");
  assert.equal(await dialog.count(), 1, "native Escape belongs to Codex, not the dialog dismissal handler");
  await page.keyboard.press("ArrowUp");
  await page.keyboard.press("ArrowDown");
  await waitFor(() => codexInput().includes("\u001b[A") && codexInput().includes("\u001b[B"), "native arrow keys on the existing controller");

  // Hold an empty check across the SSE event: the dialog must queue another
  // check even when its current request has not finished. Advance less than
  // the one-second fallback, but let the queued zero-delay refresh run.
  await waitFor(() => listGets > 0, "initial empty login listing");
  holdEmptyListing = true;
  await check.dispatchEvent("click");
  await waitFor(() => releaseEmptyListing !== undefined, "in-flight empty listing");
  await page.clock.pauseAt(new Date(Date.now() + 100));
  pendingLogin = true;
  const eventStarted = performance.now();
  const eventsBefore = await page.evaluate(() => window.__loginPendingEvents ?? 0);
  emitWatch(codex.id, "pending", {});
  while (await page.evaluate(() => window.__loginPendingEvents ?? 0) === eventsBefore) {
    assert.ok(performance.now() - eventStarted < 5000, "native pending event reaches the browser");
    await new Promise(resolve => setTimeout(resolve, 25));
  }
  await releaseEmptyListing();
  let advanced = 0;
  while (!await field.isVisible() && advanced < 500 && performance.now() - eventStarted < 5000) {
    await page.clock.runFor(5);
    advanced += 5;
    await new Promise(resolve => setTimeout(resolve, 25));
  }
  assert.ok(await field.isVisible(), "native registration updates the helper before fallback polling can run");
  await page.clock.resume();
  await capture("ready");
  await page.setViewportSize({ width: 393, height: 600 });
  await page.evaluate(() => new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve))));
  assert.equal(await dialog.locator(".dialog-body").evaluate(body => body.scrollHeight > body.clientHeight), true, "reduced phone viewport exercises a scrollable outer dialog body");
  await assertWheelContained("scrollable phone dialog");
  await page.setViewportSize({ width: 1280, height: 900 });
  await sameTerminal();
  assert.equal(await page.evaluate(h => h.closest("[data-harness-login-dialog]") !== null, host0), true);

  const beforeFieldKeys = inputFrames().length;
  await dialog.locator('[data-login-field-label]').click();
  assert.equal(await field.evaluate(input => input === document.activeElement), true, 'the callback label keeps its ordinary input focus behavior');
  await page.keyboard.press('ArrowUp');
  await page.keyboard.press('ArrowDown');
  await page.keyboard.press('Enter');
  assert.equal(inputFrames().length, beforeFieldKeys, 'callback-field navigation and empty submit never reach terminal input');
  assert.equal(await field.evaluate(input => input === document.activeElement), true, 'empty form validation retains callback focus');

  const callback = "http://localhost:1455/auth/callback?code=synthetic-private-callback&state=synthetic-state";
  const beforeCallback = inputFrames().length;
  await field.fill(callback);
  assert.equal(inputFrames().length, beforeCallback, "callback text is never sent to the CLI stdin");
  const staleField = await field.elementHandle();
  const staleDialog = await dialog.elementHandle();
  await dialog.locator("[data-login-later]").focus();
  await page.keyboard.press('Enter');
  await dialog.waitFor({ state: "detached" });
  await staleDialog.evaluate(element => element.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true })));
  assert.equal(inputFrames().length, beforeCallback, 'Use terminal instead and detached helper events never send native Enter');
  assert.equal(await staleField.evaluate(el => el.value), "", "dismissal clears callback input");
  assert.equal(await page.evaluate(h => h.closest("[data-harness-login-dialog]") === null && h.isConnected, host0), true);
  await sameTerminal();
  await chromeAction.waitFor();
  assert.equal(await chromeAction.innerText(), "Open login helper");
  await output(Array.from({ length: 100 }, (_, i) => `restored-history-${i}\r\n`).join("") + "restored-live-output\r\n", "restored-live-output");
  const restoredBottom = (await scrollState()).rows;
  await wheelOverTerminal(0, -180);
  assert.notEqual((await scrollState()).rows, restoredBottom, "normal terminal wheel scrolling returns after dismissal");
  await page.getByRole("button", { name: "Jump to live", exact: true }).click();
  await output("\u001b[?1049h\u001b[2J\u001b[Hrestored-alternate-output\r\n", "restored-alternate-output");
  const beforeRestoredWheel = frames.length;
  await wheelOverTerminal(0, -180);
  await waitFor(() => frames.slice(beforeRestoredWheel).some(({ frame }) => frame.type === "scroll"), "normal alternate-buffer scroll transport restored after dismissal");
  await output("\u001b[?1049lrestored-normal-output\r\n", "restored-normal-output");

  // A delivered listing response is the barrier for the no-nag assertion.
  const dismissedResponse = page.waitForResponse(response => {
    const url = new URL(response.url());
    return url.pathname === "/api/harness-login" && url.searchParams.get("session") === codex.id && !url.searchParams.has("watch");
  });
  emitWatch(codex.id, "pending", {});
  await (await dismissedResponse).finished();
  await page.evaluate(() => new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve))));
  assert.equal(await dialog.count(), 0, "an event does not reopen a deliberately dismissed helper");
  await chromeAction.click();
  await dialog.waitFor();
  await field.waitFor({ state: "visible" });
  await sameTerminal();
  assert.equal(await page.evaluate(h => h.closest("[data-harness-login-dialog]") !== null, host0), true);
  await assertWheelContained("reopened helper");
  const switchedDialog = await dialog.elementHandle();

  // Session changes may dispose xterm; the borrowed host must first leave the
  // dialog so disposal cannot strand a live terminal under a detached modal.
  await page.evaluate(() => document.querySelector('[data-session="existing-session"]').click());
  await waitFor(() => openCount.get(existing.id) === 2, "switch to the existing session");
  await dialog.waitFor({ state: "detached" });
  const switchedInputCount = inputFrames().length;
  await switchedDialog.evaluate(element => element.dispatchEvent(new KeyboardEvent('keydown', { key: 'ArrowUp', bubbles: true })));
  assert.equal(inputFrames().length, switchedInputCount, 'stale helper cannot type after a session switch');
  assert.equal(await page.evaluate(h => h.closest("[data-harness-login-dialog]") === null, host0), true);
  assert.equal(await page.locator("[data-live-terminal] .xterm").count(), 1);
  await page.locator('[data-session="codex-new"]').click();
  await waitFor(() => openCount.get(codex.id) === 2, "switch back to Codex");
  releaseAttach();
  await chromeAction.waitFor();
  await chromeAction.click();
  await field.waitFor({ state: "visible" });
  const currentHost = await page.locator("[data-live-terminal]").elementHandle();
  const currentXterm = await page.locator("[data-live-terminal] .xterm").elementHandle();
  const fencedField = await field.elementHandle();
  const fencedDialog = await dialog.elementHandle();
  await field.fill(callback);

  sockets.get(codex.id).send(JSON.stringify({ type: "error", code: "fenced", message: "Another controller took over" }));
  await dialog.waitFor({ state: "detached" });
  const fencedInputCount = inputFrames().length;
  await fencedDialog.evaluate(element => element.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true })));
  assert.equal(inputFrames().length, fencedInputCount, 'controller loss disables helper key forwarding');
  assert.equal(await fencedField.evaluate(el => el.value), "", "controller loss clears callback input");
  assert.equal(await currentHost.evaluate(h => h.closest("[data-harness-login-dialog]") === null && h.isConnected), true);
  assert.equal(await page.locator("[data-live-login]").count(), 0, "a displaced controller cannot reopen the helper");
  assert.equal(openCount.get(codex.id), 2, "controller loss does not silently reconnect");

  // An explicit reconnect preserves xterm but establishes a fresh transport.
  await page.locator("[data-live-attach]").click();
  await waitFor(() => openCount.get(codex.id) === 3, "explicit reconnect socket");
  releaseAttach();
  await chromeAction.waitFor();
  assert.equal(await page.evaluate(x => x === document.querySelector("[data-live-terminal] .xterm"), currentXterm), true);
  await chromeAction.click();
  await dialog.waitFor();
  assert.equal(await page.evaluate(h => h === document.querySelector("[data-live-terminal]"), currentHost), true);
  assert.equal(openCount.get(codex.id), 3, "reopening after reconnect never adds a second transport");
  await field.waitFor({ state: 'visible' });
  await field.fill(callback);
  const beforeFinish = inputFrames().length;
  await dialog.locator('[data-login-submit]').focus();
  await page.keyboard.press('Enter');
  await dialog.getByText('Native login completed. Return to the terminal.', { exact: true }).waitFor();
  await dialog.locator('[data-login-close]').focus();
  await page.keyboard.press('Enter');
  await dialog.waitFor({ state: 'detached' });
  assert.equal(inputFrames().length, beforeFinish, 'Finish sign-in and Close keep their keyboard actions without sending native Enter');
  assert.equal(codexInput().includes("synthetic-private-callback"), false);
  assert.deepEqual(errors, []);
  await context.close();
}
