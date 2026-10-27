import assert from "node:assert/strict";
import { createServer } from "node:http";
import { readFile } from "node:fs/promises";
import { existsSync } from "node:fs";
import { extname, join, resolve } from "node:path";
import { PNG } from "pngjs";
import { browser as getBrowser } from "./browser.mjs";
import { SETTLE_DELAYS_MS } from "../src/app/viewport.ts";

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

const releaseNoticeProbe = process.env.NOISEDECK_RELEASE_MOBILE_PROBE === "1";

const server = createServer(async (req, res) => {
  const pathname = decodeURIComponent(new URL(req.url, "http://x").pathname);
  const path = join(DIST, pathname === "/" ? "index.html" : pathname);
  if (!path.startsWith(DIST) || !existsSync(path)) {
    res.writeHead(404);
    res.end("not found");
    return;
  }
  res.writeHead(200, { "Content-Type": MIME[extname(path)] ?? "application/octet-stream" });
  let body = await readFile(path);
  if (releaseNoticeProbe && path.endsWith("index.html")) body = Buffer.from(body.toString().replaceAll("__NOISEDECK_BUILD_ID__", "v0.3.0"));
  res.end(body);
});
await new Promise((resolveServer) => server.listen(0, "127.0.0.1", resolveServer));
const address = server.address();
assert.notEqual(address, null);
assert.equal(typeof address, "object");
const origin = `http://127.0.0.1:${address.port}`;

let browser;
try {
  ({ browser } = await getBrowser());
  await runWidth(1024, false);
  for (const width of [320, 390, 430]) await runWidth(width, true);
  console.log("terminal soft keys: desktop plus 320px, 390px, 430px touch passed");
  await runWidth(390, true, { settingsDelayMs: 400 });
  console.log("terminal soft keys: live More menu keeps focus across its own remount");
} finally {
  await browser?.close();
  await new Promise((resolveClose) => server.close(resolveClose));
}

// options.settingsDelayMs delays the settings read the More menu fires on
// open. The live menu remounts itself when that response lands, and a focus
// placed by a click handler is undone by the remount — the preview has no
// such asynchronous remount, so its coverage of focus placement says nothing
// about the live surface.
async function runWidth(width, mobile, options = {}) {
  const context = await browser.newContext({
    viewport: { width, height: 720 },
    hasTouch: mobile,
    isMobile: mobile,
    permissions: ["clipboard-read", "clipboard-write"],
  });
  const page = await context.newPage();
  const errors = [];
  page.on("pageerror", (error) => errors.push(error.message));

  const project = {
    id: "project-touch",
    name: "touch-project",
    canonical_path: "/workspace/touch-project",
    last_used_at: "2026-08-08T00:00:00Z",
  };
  const session = {
    id: "session-touch",
    project_id: project.id,
    provider: "codex",
    display_name: "",
    status: "running",
    generation: 1,
    created_at: "2026-08-08T00:00:00Z",
    updated_at: "2026-08-08T00:00:00Z",
    recovery_capability: "none",
    recovery_state: "unavailable",
    presentation_state: "running",
  };
  const otherSession = { ...session, id: "session-other", display_name: "Other session" };

  await page.route(`${origin}/auth/session`, (route) => route.fulfill({
    status: 204,
    headers: { "X-Noisedeck-Auth-Mode": "builtin" },
  }));
  await page.route(`${origin}/api/**`, (route) => {
    const request = route.request();
    const path = new URL(request.url()).pathname;
    const json = (status, body) => route.fulfill({
      status,
      contentType: "application/json",
      body: JSON.stringify(body),
    });
    if (releaseNoticeProbe && path === "/api/build") return json(200, { build_id: "v0.3.0" });
    if (releaseNoticeProbe && path === "/api/release-update") return json(200, { version: "v0.4.0", release_url: "https://github.com/noisefloor-oss/deck/releases/tag/v0.4.0" });
    if (request.method() === "GET" && path === "/api/session-activity/events") {
      return route.fulfill({
        status: 200,
        contentType: "text/event-stream",
        body: ": connected\n\nretry: 3600000\n\n",
      });
    }
    if (request.method() === "GET" && path === "/api/harness-login") {
      // The login coordinator polls the listing while a controller is
      // attached; no login is waiting in this probe.
      return json(200, { pending: [], csrf: "login-csrf" });
    }
    if (request.method() === "GET" && path === "/api/providers") {
      // The probe drives a single codex session and never opens the picker.
      return json(200, { providers: ["codex"] });
    }
    if (request.method() === "GET" && path === "/api/settings/last-session") return json(404, { code: "not_found", message: "no last session remembered" });
    if (request.method() === "PUT" && path === "/api/settings/last-session") return json(200, request.postDataJSON());
    if (request.method() === "GET" && path === "/api/settings/layout") return json(404, { code: "not_found", message: "no layout saved" });
    if (request.method() === "GET" && path === "/api/settings/tutorial") return json(200, { version: 1, dismissed: true, lesson: "welcome", step: 0, done: [], scope: "user" });
    if (request.method() === "GET" && path === "/api/settings") {
      const body = {
        skip_permissions: false,
        skip_close_confirm: false,
        skip_remove_project_confirm: false,
      };
      if (options.settingsDelayMs === undefined) return json(200, body);
      return new Promise((settle) => {
        setTimeout(() => settle(json(200, body)), options.settingsDelayMs);
      });
    }
    if (request.method() === "GET" && path === "/api/projects") return json(200, [project]);
    if (request.method() === "GET" && path === "/api/git-identities") return json(200, {
      profiles: [{ id: "identity-1", name: "Deck probe", email: "deck@example.invalid", revision: 1, created_at: "t", updated_at: "t" }],
      default_profile_id: "identity-1", revision: 1,
    });
    if (request.method() === "GET" && path === `/api/projects/${project.id}/sessions`) {
      return json(200, [session, otherSession]);
    }
    if (request.method() === "GET" && [session.id, otherSession.id].some((id) => path === `/api/sessions/${id}`)) {
      return json(200, {
        ...(path.endsWith(otherSession.id) ? otherSession : session),
        pane_pid: 4242,
        tmux_pane_id: "%1",
        native_session_id: "native-touch",
        updated_at: "2026-08-08T00:00:00Z",
      });
    }
    if (/^\/api\/projects\/[^/]+\/lanes$/.test(path)) return json(200, { lanes: [] });
    if (request.method() === "GET" && path === `/api/projects/${project.id}/taskmaster`) {
      return json(404, { error: "not_taskmaster_project", message: "not a task project" });
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

  const frames = [];
  let socket;
  let refuseNextLive = false;
  let refuseNextAttach = false;
  let holdNextAttach = false;
  let heldAttach;
  let scrollActive = false;
  let socketCount = 0;
  await page.routeWebSocket(
    `${origin.replace("http", "ws")}/api/sessions/*/terminal?*`,
    (peer) => {
      socket = peer;
      socketCount += 1;
      if (refuseNextAttach) {
        refuseNextAttach = false;
        peer.send(JSON.stringify({
          type: "error",
          code: "attach_failed",
          message: "transient live-view preflight refusal",
        }));
        void peer.close();
        return;
      }
      scrollActive = false;
      peer.onMessage((message) => {
        if (typeof message !== "string") return;
        const frame = JSON.parse(message);
        frames.push(frame);
        if (frame.type === "input") {
          peer.send(JSON.stringify({ type: "status", code: "sent", message: "" }));
          return;
        }
        if (frame.type !== "scroll") return;
        if (frame.direction === "up") scrollActive = true;
        if (frame.direction === "live") {
          if (refuseNextLive) {
            refuseNextLive = false;
            return;
          }
          scrollActive = false;
        }
        peer.send(JSON.stringify({ type: "scroll_status", active: scrollActive }));
      });
      peer.send(JSON.stringify({ type: "scroll_status", active: false }));
      const attach = () => {
      peer.send(JSON.stringify({ type: "attached", token: 7, generation: 1 }));
      const history = Array.from({ length: 2300 }, (_, i) => `native-${String(i).padStart(4, "0")}\r\n`).join("");
      peer.send(JSON.stringify({
        type: "output",
        data: Buffer.from(history).toString("base64"),
      }));
      };
      if (holdNextAttach) {
        holdNextAttach = false;
        heldAttach = attach;
      } else {
        attach();
      }
    },
  );

  await page.goto(origin, { waitUntil: "networkidle" });
  if (releaseNoticeProbe) {
    await page.locator("[data-release-notice]").waitFor({ state: "visible" });
    assert.equal((await page.locator('[data-region="info-bar"]').boundingBox()).height, mobile ? 40 : 26);
  }
  const terminal = page.locator("[data-live-terminal] .xterm");
  await terminal.waitFor();
  assert.equal(
    await page.locator("[data-live-terminal]").getAttribute("data-terminal-scrollback-lines"),
    "1000",
    `${width}px xterm is mounted with the bounded scrollback default`,
  );
  await waitFor(() => socket !== undefined, "terminal WebSocket");
  await page.waitForFunction(() => document.querySelector(".xterm-rows")?.textContent?.includes("native-2299"));
  if (!mobile) {
    const softKeys = page.locator('[data-region="terminal-soft-keys"]');
    assert.equal(await softKeys.evaluate((el) => getComputedStyle(el).display), "none");
    assert.equal((await softKeys.boundingBox())?.height ?? 0, 0, "soft keys changed desktop geometry");
    // The attach URL carries the initial geometry, so a settled desktop
    // mount renegotiates nothing; any resize frame here is geometry churn
    // from mounting the hidden soft keys.
    await page.waitForTimeout(250);
    assert.equal(
      frames.filter((frame) => frame.type === "resize").length,
      0,
      "mounting hidden soft keys caused a desktop PTY resize",
    );
    assert.equal(errors.length, 0, `desktop page errors: ${errors.join("; ")}`);
    // index.html and preview.html run separate implementations of this
    // menu, so a preview assertion proves nothing about the live one.
    await page.locator("[data-live-more]").click();
    await page.locator("[data-more-popover]").waitFor();
    // Opening fires a settings read, and the menu remounts when it
    // resolves — a focus placed before that remount is discarded with the
    // old mount. Wait it out so the Escape below reaches a stable menu.
    await page.waitForTimeout(300);
    await page.locator("[data-more-popover] [data-font-inc]").focus();
    await page.keyboard.press("Escape");
    assert.equal(await page.locator("[data-more-popover]").count(), 0, "desktop Escape left the More menu open");
    await page.waitForFunction(
      () => document.activeElement !== null && document.activeElement.hasAttribute("data-live-more"),
      { timeout: 2000 },
    );
    await context.close();
    return;
  }
  assert.equal(
    await page.evaluate(() => matchMedia("(pointer: coarse) and (max-width: 48rem)").matches),
    true,
    `${width}px context is not a coarse phone viewport`,
  );

  const screen = terminal.locator(".xterm-screen");
  const dock = page.locator('[data-region="terminal-dock"]');
  const softKeys = page.locator('[data-region="terminal-soft-keys"]');
  await dock.waitFor();
  await softKeys.waitFor();
  const terminalBox = await page.locator("[data-live-terminal]").boundingBox();
  const softKeysBox = await softKeys.boundingBox();
  assert.notEqual(terminalBox, null);
  assert.notEqual(softKeysBox, null);
  assert.ok(
    softKeysBox.y >= terminalBox.y + terminalBox.height - 1,
    `${width}px soft keys overlap terminal output`,
  );
  assert.ok(softKeysBox.y + softKeysBox.height <= 720, `${width}px soft keys leave the viewport`);

  // Mobile keeps Sessions separate from the Panels and Settings controls.
  const titlebar = page.locator('[data-workspace-titlebar]');
  const sessionsButton = titlebar.getByRole("button", { name: "Sessions", exact: true });
  const panelsButton = titlebar.getByRole("button", { name: "Panels", exact: true });
  const moreButton = titlebar.getByRole("button", { name: "Settings", exact: true });
  for (const [locator, label] of [
    [sessionsButton, "Sessions"],
    [panelsButton, "Panels"],
    [moreButton, "Settings"],
  ]) {
    const box = await locator.boundingBox();
    assert.notEqual(box, null, `${width}px ${label} button is not rendered`);
    assert.ok(box.width >= 44 && box.height >= 44, `${width}px ${label} button is smaller than 44px`);
  }
  const titlebarOrder = await titlebar.evaluate((el) => {
    const labels = [];
    for (const button of el.querySelectorAll("button")) {
      if (button.getClientRects().length) labels.push(button.getAttribute("aria-label"));
    }
    return labels;
  });
  const expectedBar = ["Sessions", "Panels", "Settings"];
  const positions = expectedBar.map((label) => titlebarOrder.indexOf(label));
  assert.ok(
    positions.every((p) => p !== -1),
    `${width}px titlebar is missing one of ${expectedBar.join(" / ")}`,
  );
  assert.ok(
    positions.every((p, i) => i === 0 || p > positions[i - 1]),
    `${width}px titlebar does not read Sessions / Panels / Settings in order`,
  );

  // The persistent tutorial slot precedes the terminal rows and takes no space when dismissed.
  const dockOrder = await dock.evaluate((el) =>
    Array.from(el.children).map((child) => child.getAttribute("data-region"))
  );
  assert.deepEqual(
    dockOrder,
    ["tutorial-mobile-bar", "terminal-status", "terminal-soft-keys", "terminal-compose"],
    `${width}px dock rows are not in the expected order`,
  );

  const tutorialBar = dock.locator('[data-region="tutorial-mobile-bar"]');
  assert.equal(await tutorialBar.evaluate((el) => el.hidden), true);
  assert.equal(await tutorialBar.evaluate((el) => el.getBoundingClientRect().height), 0,
    `${width}px dismissed tutorial bar occupies dock space`);

  // The compose row's bottom padding carries the device inset: headless
  // resolves env() to 0, so the computed value shows only the 10px base —
  // the stylesheet is the observable for the env() term.
  {
    const composePad = await page
      .locator('[data-region="terminal-compose"]')
      .evaluate((el) => getComputedStyle(el).paddingBottom);
    assert.equal(composePad, "10px",
      `${width}px compose base padding is ${composePad}, not the comp's 10px`);
    const { readdir } = await import("node:fs/promises");
    const assetsDir = join(DIST, "assets");
    const cssFiles = (await readdir(assetsDir)).filter((name) => name.endsWith(".css"));
    const css = (await Promise.all(cssFiles.map((f) => readFile(join(assetsDir, f), "utf8")))).join("\n");
    const rule = css.match(/\[data-region=terminal-compose\][^{}]*\{[^}]*\}|\[data-region="terminal-compose"\]\s*\{[^}]*\}/g)
      ?.find((r) => r.includes("padding"));
    assert.ok(rule !== undefined && rule.includes("safe-area-inset-bottom"),
      `${width}px compose padding rule lost its safe-area inset: ${rule}`);
  }

  // The key strip is a single horizontally scrollable row.
  const stripButtonNames = [
    "Escape",
    "Tab",
    "Ctrl modifier",
    "Arrow pad",
    "Forward slash",
    "Pipe",
    "Tilde",
  ];
  const stripTops = new Set();
  const stripBottoms = new Set();
  for (const name of stripButtonNames) {
    const button = page.getByRole("button", { name, exact: true });
    const box = await button.boundingBox();
    assert.notEqual(box, null, `${width}px ${name} is not accessible and visible`);
    assert.ok(box.width >= 44 && box.height >= 44, `${width}px ${name} is smaller than 44px`);
    stripTops.add(Math.round(box.y));
    stripBottoms.add(Math.round(box.y + box.height));
  }
  assert.equal(stripTops.size, 1, `${width}px key strip spans more than one vertical row`);
  assert.equal(stripBottoms.size, 1, `${width}px key strip spans more than one vertical row`);

  // The arrow pad collapses to one key and pops a 4-way pad above the strip.
  const arrowPad = page.getByRole("button", { name: "Arrow pad", exact: true });
  await tapTouch(page, arrowPad);
  const pad = page.locator('[data-terminal-arrow-pad-popover]');
  await pad.waitFor();
  for (const name of ["Arrow up", "Arrow down", "Arrow left", "Arrow right"]) {
    const box = await page.getByRole("button", { name, exact: true }).boundingBox();
    assert.notEqual(box, null, `${width}px ${name} is not inside the popped arrow pad`);
    assert.ok(box.width >= 44 && box.height >= 44, `${width}px ${name} is smaller than 44px`);
  }
  await page.keyboard.press("Escape");
  await page.waitForSelector('[data-terminal-arrow-pad-popover]', { state: "detached" });

  assert.equal(
    await page.evaluate(() => document.documentElement.scrollWidth <= document.documentElement.clientWidth),
    true,
    `${width}px soft keys introduced horizontal overflow`,
  );
  const scrollbackPill = page.locator("[data-live-scrollback-pill]");
  const liveButton = page.getByRole("button", { name: "Jump to live" });
  assert.equal(await scrollbackPill.textContent(), "Jump to live");
  assert.doesNotMatch(
    `${await scrollbackPill.textContent()} ${await scrollbackPill.getAttribute("aria-label")}`,
    /esc(?:ape)?/i,
    "scrollback UI must not advertise Escape as a view control",
  );
  assert.equal(
    frames.some((frame) => frame.type === "scroll" && frame.direction === "live"),
    false,
    "browser attempted an optimistic live command after the server attach handshake",
  );
  await terminal.click();
  const beforeRetentionCheck = frames.length;
  for (let pageUp = 0; pageUp < 100; pageUp += 1) await page.keyboard.press("Shift+PageUp");
  await liveButton.waitFor();

  // The popped arrow pad sits above the key strip and covers the pill.
  await openArrowPad(page);
  const padBox = await page.locator('[data-terminal-arrow-pad-popover]').boundingBox();
  const pillBox = await liveButton.boundingBox();
  assert.notEqual(padBox, null, "arrow pad did not render");
  assert.notEqual(pillBox, null, "jump-to-live pill did not render");
  assert.ok(
    padBox.y <= pillBox.y && padBox.y + padBox.height >= pillBox.y + pillBox.height,
    `${width}px arrow pad does not cover the jump-to-live pill`,
  );
  // The pad hovers clear of the dock: an 8px seam separates the grid's
  // bottom row from the strip it popped from.
  {
    const dockBox = await page.locator('[data-region="terminal-soft-keys"]').boundingBox();
    const seam = dockBox.y - (padBox.y + padBox.height);
    assert.ok(Math.abs(seam - 8) <= 1,
      `${width}px arrow pad sits ${seam}px above the key strip, not 8`);
  }
  await closeArrowPad(page);

  const topText = await terminal.locator(".xterm-rows").textContent();
  const oldest = /native-(\d{4})/.exec(topText ?? "");
  assert.notEqual(oldest, null, `${width}px xterm top did not expose a retained numbered line`);
  // 2,299 lines were written into a 1,000-line buffer (the bounded default
  // asserted at mount), so the oldest retained line sits near
  // 2299 - (1000 + viewport rows).
  assert.ok(
    Number(oldest[1]) >= 1100 && Number(oldest[1]) <= 1400,
    `${width}px oldest retained line ${oldest[1]} does not reflect the bounded 1,000-line buffer`,
  );
  assert.equal(
    frames.slice(beforeRetentionCheck).some((frame) => frame.type === "input" || frame.type === "reply"),
    false,
    "retention measurement leaked terminal bytes",
  );
  await liveButton.click();
  await liveButton.waitFor({ state: "hidden" });
  const livePixels = await screen.screenshot();
  const beforeNormalDown = frames.length;
  await swipe(page, terminal, "down");
  await liveButton.waitFor();
  const olderPixels = await screen.screenshot();
  assert.equal(
    pixelDifference(olderPixels, livePixels) > 0.02,
    true,
    `${width}px native viewport did not reveal older history`,
  );
  assert.equal(
    frames.slice(beforeNormalDown).some((frame) => frame.type === "input"),
    false,
    "terminal scrolling emitted input bytes",
  );
  assert.equal(
    frames.some((frame) => frame.type === "scroll" && frame.direction === "up"),
    false,
    "normal-buffer touch bypassed xterm's native viewport",
  );

  socket.send(JSON.stringify({
    type: "output",
    data: Buffer.from("new-output-while-reading\r\n").toString("base64"),
  }));
  await page.waitForTimeout(100);
  const readingPixels = await screen.screenshot();
  assert.ok(
    pixelDifference(readingPixels, olderPixels) < 0.01,
    "new output materially changed the native reading position",
  );
  assert.equal(
    await liveButton.isVisible(),
    true,
    "new output stole the native reading position",
  );
  const beforeNativeUp = frames.length;
  await swipe(page, terminal, "up");
  if (await liveButton.isVisible()) {
    await swipe(page, terminal, "up");
  }
  await liveButton.waitFor({ state: "hidden" });
  assert.equal(
    frames.slice(beforeNativeUp).some((frame) => frame.type === "input" || frame.type === "reply"),
    false,
    "native upward touch drag leaked terminal bytes",
  );
  await swipe(page, terminal, "down");
  await liveButton.waitFor();
  const beforeNativeJump = frames.length;
  await liveButton.click();
  await liveButton.waitFor({ state: "hidden" });
  assert.equal(
    frames.slice(beforeNativeJump).some((frame) => frame.type === "input" || frame.type === "reply"),
    false,
    "jump-to-live leaked terminal bytes",
  );
  assert.ok(
    frames.some((frame) => frame.type === "scroll" && frame.direction === "live"),
    "jump-to-live did not use the fenced scroll path",
  );

  socket.send(JSON.stringify({
    type: "output",
    data: Buffer.from("\x1b[?1049halternate mode\r\n").toString("base64"),
  }));
  await page.waitForTimeout(100);
  const beforeAlt = frames.length;
  await swipe(page, terminal, "down");
  await waitFor(
    () => frames.slice(beforeAlt).some((frame) => frame.type === "scroll" && frame.direction === "up"),
    "alternate-buffer scroll frame",
  );
  assert.equal(
    frames.slice(beforeAlt).some((frame) => frame.type === "input" || frame.type === "reply"),
    false,
    "touch drag leaked terminal bytes in the copy-mode route",
  );
  const beforeUp = frames.length;
  await swipe(page, terminal, "up");
  await waitFor(
    () => frames.slice(beforeUp).some((frame) => frame.type === "scroll" && frame.direction === "live"),
    "alternate-buffer return-to-live frame",
  );
  await page.waitForTimeout(100);
  assert.equal(
    frames.slice(beforeUp).filter((frame) => frame.type === "scroll" && frame.direction === "live").length,
    1,
    "one four-move upward gesture emitted more than one live frame",
  );
  assert.equal(
    frames.slice(beforeUp).some((frame) => frame.type === "input" || frame.type === "reply"),
    false,
    "upward touch drag leaked terminal bytes",
  );
  await liveButton.waitFor({ state: "hidden" });
  await swipe(page, terminal, "down");
  await liveButton.waitFor();
  refuseNextLive = true;
  const beforeAltJump = frames.length;
  await liveButton.click();
  await waitFor(
    () => frames.slice(beforeAltJump).some((frame) => frame.type === "scroll" && frame.direction === "live"),
    "alternate-buffer jump-to-live frame",
  );
  await page.waitForTimeout(100);
  assert.equal(await liveButton.isVisible(), true, "a refused live request hid the scrollback pill");
  assert.equal(
    frames.slice(beforeAltJump).filter((frame) => frame.type === "scroll" && frame.direction === "live").length,
    1,
    "an independent jump emitted more than one live frame",
  );
  assert.equal(
    frames.slice(beforeAltJump).some((frame) => frame.type === "input" || frame.type === "reply"),
    false,
    "alternate jump-to-live leaked terminal bytes",
  );
  socket.send(JSON.stringify({ type: "scroll_status", active: false }));
  await liveButton.waitFor({ state: "hidden" });

  // A mouse-tracking application (Claude Code's shape: alternate screen,
  // SGR mouse, empty tmux history) owns its own transcript. The drag must
  // become the wheel reports the desktop wheel already sends - input
  // frames carrying SGR wheel events - and never a copy-mode scroll,
  // which would move through history that does not exist.
  socket.send(JSON.stringify({
    type: "output",
    data: Buffer.from("\x1b[?1000h\x1b[?1006h").toString("base64"),
  }));
  await page.waitForTimeout(100);
  const decodeInput = (frame) => Buffer.from(frame.data ?? "", "base64").toString("latin1");
  const beforeMouse = frames.length;
  await swipe(page, terminal, "down");
  await waitFor(
    () => frames.slice(beforeMouse).some((frame) =>
      frame.type === "reply" && decodeInput(frame).includes("\x1b[<64;")),
    "mouse-tracking drag did not emit SGR wheel-up reports on the reply path",
  );
  assert.equal(
    frames.slice(beforeMouse).some((frame) => frame.type === "scroll"),
    false,
    "mouse-tracking drag fell back to the copy-mode scroll path",
  );
  const beforeMouseUp = frames.length;
  await swipe(page, terminal, "up");
  await waitFor(
    () => frames.slice(beforeMouseUp).some((frame) =>
      frame.type === "reply" && decodeInput(frame).includes("\x1b[<65;")),
    "mouse-tracking upward drag did not emit SGR wheel-down reports on the reply path",
  );
  // Every report in one drag must name the same cell. A wheel does not
  // travel while it turns, so the position is the one the finger started
  // from; reporting the moving finger walks the events down the pane, and an
  // application that routes a wheel by position gives the later ones to
  // whatever occupies the bottom rows instead of the transcript.
  const reportCells = (from, to) => frames.slice(from, to)
    .filter((frame) => frame.type === "reply")
    .flatMap((frame) => [...decodeInput(frame).matchAll(/\x1b\[<6[45];(\d+);(\d+)M/g)]
      .map((match) => `${match[1]},${match[2]}`));
  // Each gesture is bounded: an unbounded slice would run past this drag into
  // the next one, whose own anchor is a different - and correct - cell.
  for (const [label, from, to] of [
    ["downward", beforeMouse, beforeMouseUp],
    ["upward", beforeMouseUp, frames.length],
  ]) {
    const cells = new Set(reportCells(from, to));
    assert.equal(cells.size > 0, true, `${label} drag emitted no wheel reports to check`);
    assert.equal(cells.size <= 1, true,
      `${label} drag reported wheel events at ${cells.size} different cells (${[...cells].join(" ")}); one gesture must report one cell`);
  }
  // An abandoned gesture must not lend its anchor to the next one. (The
  // other half of that invariant - a drag whose start was never seen - is
  // not reachable here: CDP refuses a touchMove without a touchStart, the
  // same rule the browser enforces, so the null-anchor branch is defensive
  // rather than drivable.)
  const cancelBox = await terminal.boundingBox();
  const cancelClient = await page.context().newCDPSession(page);
  await cancelClient.send("Input.dispatchTouchEvent", {
    type: "touchStart",
    touchPoints: [{ x: cancelBox.x + cancelBox.width / 2, y: cancelBox.y + cancelBox.height * 0.8 }],
  });
  await cancelClient.send("Input.dispatchTouchEvent", { type: "touchCancel", touchPoints: [] });
  await page.waitForTimeout(50);
  const beforeAfterCancel = frames.length;
  await swipe(page, terminal, "down");
  const afterCancelCells = new Set(reportCells(beforeAfterCancel, frames.length));
  // Same gesture as the first downward swipe, so the same start point and
  // therefore the same cell. Asserting only "one cell" would pass if the
  // cancelled gesture's stale anchor - taken lower down the pane - were
  // reused, which is the leak this case exists to catch.
  const downwardCell = [...new Set(reportCells(beforeMouse, beforeMouseUp))][0];
  assert.deepEqual([...afterCancelCells], [downwardCell],
    `after a cancelled gesture the live drag reported ${[...afterCancelCells].join(" ") || "nothing"}, want the downward start cell ${downwardCell}`);
  assert.equal(
    frames.slice(beforeMouseUp).some((frame) => frame.type === "scroll"),
    false,
    "mouse-tracking upward drag leaked a scroll frame",
  );
  await assertTouchSelection(page, screen, socket, frames, true);

  // Turning tracking off hands the drag back to the copy-mode route.
  socket.send(JSON.stringify({
    type: "output",
    data: Buffer.from("\x1b[?1000l").toString("base64"),
  }));
  await page.waitForTimeout(100);
  const beforeMouseOff = frames.length;
  await swipe(page, terminal, "down");
  await waitFor(
    () => frames.slice(beforeMouseOff).some((frame) => frame.type === "scroll" && frame.direction === "up"),
    "post-tracking drag did not return to the copy-mode route",
  );
  const beforeMouseOffUp = frames.length;
  await swipe(page, terminal, "up");
  await waitFor(
    () => frames.slice(beforeMouseOffUp).some((frame) => frame.type === "scroll" && frame.direction === "live"),
    "post-tracking upward drag did not return to live",
  );
  socket.send(JSON.stringify({ type: "scroll_status", active: false }));
  await liveButton.waitFor({ state: "hidden" });

  await assertTouchSelection(page, screen, socket, frames, false);

  // Exercise the production xterm reset callback and reconnect chrome: a
  // preserved alternate view remains visible across a refused handshake,
  // then clears only when the retry supplies authoritative live evidence.
  await swipe(page, terminal, "down");
  await liveButton.waitFor();
  refuseNextAttach = true;
  const reconnectCtrl = page.getByRole("button", { name: "Ctrl modifier", exact: true });
  await tapTouch(page, reconnectCtrl);
  assert.equal(await reconnectCtrl.getAttribute("aria-pressed"), "true");
  const draft = page.locator("[data-mobile-compose-input]");
  await draft.fill("saved offline draft");
  await openArrowPad(page);
  await socket.close();
  const reconnect = page.getByRole("button", { name: "Reconnect" });
  await reconnect.waitFor();
  assert.equal(await page.locator("[data-terminal-arrow-pad-popover]").count(), 0,
    "disconnected arrow pad remained tappable");
  assert.equal(await draft.isDisabled(), true, "disconnected compose remains enabled");
  assert.equal(await reconnectCtrl.isDisabled(), true, "disconnected soft keys remain enabled");
  const offlineFrames = frames.length;
  await page.locator("[data-mobile-compose-form]").dispatchEvent("submit");
  assert.equal(await draft.inputValue(), "saved offline draft", "offline submit erased the draft");
  assert.equal(frames.slice(offlineFrames).filter((frame) => frame.type === "input").length, 0,
    "offline submit sent input");
  const notice = page.locator("[data-attachment-notice]");
  const originalNotice = await notice.elementHandle();
  await page.waitForResponse((response) => new URL(response.url()).pathname === `/api/sessions/${session.id}`);
  await page.evaluate(() => new Promise((resolve) => requestAnimationFrame(() => requestAnimationFrame(resolve))));
  assert.equal(await notice.evaluate((node, original) => node === original, originalNotice), true,
    "status polling replaced the unchanged live region");
  await originalNotice.dispose();
  const noticeBox = await notice.boundingBox();
  const paneBox = await page.locator('[data-region="terminal"]').boundingBox();
  assert.ok(Math.abs(noticeBox.y - paneBox.y) < 1, "attachment notice is not at the top of the pane");
  assert.ok((await reconnect.boundingBox()).height >= 44, "phone reconnect target is too short");
  const offlinePadding = await page.locator('[data-region="terminal"]').evaluate((pane) => {
    const provider = pane.dataset.terminalProvider;
    const host = pane.querySelector("[data-live-terminal]");
    const padded = getComputedStyle(host).padding;
    pane.dataset.terminalProvider = "opencode";
    const edge = getComputedStyle(host).padding;
    pane.dataset.terminalProvider = provider;
    return { padded, edge };
  });
  assert.deepEqual(offlinePadding, { padded: "12px 16px", edge: "0px" },
    "attachment notice changed provider-specific terminal insets");
  assert.equal(
    await reconnectCtrl.getAttribute("aria-pressed"),
    "false",
    "transport close preserved a stale Ctrl chord",
  );
  assert.equal(await liveButton.isVisible(), true, "disconnect optimistically hid alternate scrollback");
  await reconnect.click();
  await waitFor(() => socketCount === 2, "refused reconnect socket");
  await reconnect.waitFor();
  assert.equal(await liveButton.isVisible(), true, "refused reconnect hid alternate scrollback");
  holdNextAttach = true;
  await reconnect.click();
  await waitFor(() => socketCount === 3 && heldAttach !== undefined, "pending reconnect socket");
  assert.equal(await reconnect.count(), 0, "connecting offered a competing reconnect button");
  assert.equal(await draft.isDisabled(), true, "pending attach enabled compose");
  assert.equal((await notice.boundingBox()).height, 32, "connecting strip is not 32px");
  heldAttach();
  await page.waitForFunction(() => !document.querySelector("[data-mobile-compose-input]").disabled);
  assert.equal(await draft.inputValue(), "saved offline draft", "reattach erased the draft");
  assert.equal(await reconnectCtrl.isEnabled(), true, "reattach did not enable soft keys");
  assert.equal(await notice.count(), 0, "successful attachment kept its notice");
  await draft.fill("");
  await liveButton.waitFor({ state: "hidden" });
  assert.equal(
    await reconnectCtrl.getAttribute("aria-pressed"),
    "false",
    "reconnect preserved a stale Ctrl chord",
  );

  await assertSoftKeyInput(page, socket, frames);

  await page.evaluate(() => {
    const viewport = window.visualViewport;
    if (!viewport) throw new Error("visual viewport is unavailable");
    Object.defineProperty(viewport, "height", { configurable: true, get: () => 480 });
    viewport.dispatchEvent(new Event("resize"));
  });
  await page.waitForFunction(() => {
    const bar = document.querySelector('[data-region="terminal-soft-keys"]');
    return bar instanceof HTMLElement && bar.getBoundingClientRect().bottom <= 480;
  });
  // The frame tracker re-reads the viewport after keyboard animation settles.
  // Wait beyond that cascade so a transient frame cannot pass this check.
  await page.evaluate((delay) => new Promise((resolve) => setTimeout(resolve, delay)),
    Math.max(...SETTLE_DELAYS_MS) + 50);
  assert.equal(await page.locator("[data-workspace]").evaluate((workspace) =>
    workspace.getBoundingClientRect().height), 480, "workspace lost its visual viewport height");
  const shrunkBar = await softKeys.boundingBox();
  assert.notEqual(shrunkBar, null);
  assert.ok(
    shrunkBar.y + shrunkBar.height <= 480,
    `${width}px soft keys are obscured after visual viewport shrink: ${JSON.stringify(shrunkBar)}`,
  );
  assert.equal(errors.length, 0, `${width}px page errors: ${errors.join("; ")}`);

  // Focus lands on the pushed theme screen's back control in the live app.
  // The preview runs a separate implementation of this menu with no async
  // settings remount, so it cannot stand in for this check.
  await page.locator("[data-live-more]").click();
  await page.locator("[data-more-popover]").waitFor();
  await page.locator("[data-theme-flyout]").click();
  await page.locator("[data-theme-list]").waitFor();
  await page.waitForFunction(
    () => document.activeElement !== null && document.activeElement.hasAttribute("data-theme-back"),
    { timeout: 2000 },
  );

  // With the settings read delayed, the More menu remounts itself while the
  // user is already inside the pushed theme screen. Focus has to survive
  // that remount, which it only does if it is reapplied by the mount rather
  // than by the click that navigated.
  if (options.settingsDelayMs !== undefined) {
    await page.waitForTimeout(options.settingsDelayMs + 300);
    await page.waitForFunction(
      () => document.activeElement !== null && document.activeElement.hasAttribute("data-theme-back"),
      { timeout: 2000 },
    );
  }

  // A detached composer retains its listeners, but changing sessions must
  // dispose its terminal capability before a late submit can write anywhere.
  await page.locator("[data-theme-back]").click();
  await page.keyboard.press("Escape");
  await page.locator("[data-more-popover]").waitFor({ state: "detached" });
  const oldForm = await page.locator("[data-mobile-compose-form]").elementHandle();
  await page.locator("[data-mobile-compose-input]").fill("stale draft");
  const socketsBeforeSwitch = socketCount;
  await sessionsButton.click();
  await page.locator(`[data-session="${otherSession.id}"]`).click();
  await waitFor(() => socketCount === socketsBeforeSwitch + 1, "second session attachment");
  await page.waitForFunction(() => document.querySelector(".xterm-rows")?.textContent?.includes("native-2299"));
  const afterSwitch = frames.length;
  await oldForm.evaluate((form) => form.dispatchEvent(new Event("submit", { bubbles: true, cancelable: true })));
  await page.waitForTimeout(100);
  assert.equal(frames.slice(afterSwitch).filter((frame) => frame.type === "input").length, 0,
    "a detached composer sent its draft after switching sessions");
  assert.equal(await page.locator("[data-mobile-compose-input]").inputValue(), "",
    "the old draft leaked into the newly selected session");
  await oldForm.dispose();
  await context.close();
}

async function assertTouchSelection(page, screen, socket, frames, tracking) {
  const text = "touch-select-0123456789";
  const nextLine = "second-touch-line";
  socket.send(JSON.stringify({ type: "output", data: Buffer.from(`\x1b[2J\x1b[H${text}\r\n${nextLine}`).toString("base64") }));
  await page.waitForFunction((text) => document.querySelector(".xterm-rows")?.textContent?.includes(text), text);
  const menu = page.locator("[data-terminal-menu-popover]");
  // Long press is the touch-only entry to the terminal actions menu.
  await holdTouch(page, screen, 800);
  await menu.waitFor();
  await tapTouch(page, menu.locator("[data-terminal-menu-select]"));
  await menu.waitFor({ state: "detached" });
  const chip = page.locator("[data-live-select-text]");
  assert.equal(await chip.getAttribute("aria-pressed"), "true");
  const firstRow = screen.locator(".xterm-rows > div").first();
  const rowBox = await firstRow.boundingBox();
  assert.notEqual(rowBox, null);
  const cellWidth = await firstRow.evaluate((row) => {
    const range = document.createRange();
    range.selectNodeContents(row.firstChild);
    return range.getBoundingClientRect().width / row.firstChild.textContent.length;
  });
  const start = { x: rowBox.x + cellWidth * 1.5, y: rowBox.y + rowBox.height / 2 };
  const end = { x: rowBox.x + cellWidth * 6.5, y: start.y };
  for (const cancel of ["cancel", "multitouch"]) {
    const before = frames.length;
    const client = await page.context().newCDPSession(page);
    await client.send("Input.dispatchTouchEvent", { type: "touchStart", touchPoints: [{ ...start, id: 0 }] });
    if (cancel === "multitouch") {
      await client.send("Input.dispatchTouchEvent", { type: "touchStart", touchPoints: [{ ...start, id: 0 }, { ...end, id: 1 }] });
      await client.send("Input.dispatchTouchEvent", { type: "touchMove", touchPoints: [{ ...end, id: 0 }, { ...start, id: 1 }] });
    }
    await client.send("Input.dispatchTouchEvent", { type: "touchCancel", touchPoints: [] });
    await client.detach();
    assert.equal(await menu.count(), 0, `${cancel} opened a selection menu`);
    assert.deepEqual(frames.slice(before).filter((frame) => ["input", "reply", "scroll"].includes(frame.type)), [],
      `${cancel} selection sent input to the application`);
  }
  const nextRow = { ...end, y: end.y + rowBox.height };
  for (const [from, to, expected] of [
    [start, end, text.slice(1, 7)],
    [end, start, text.slice(1, 7)],
    [start, nextRow, `${text.slice(1)}\n${nextLine.slice(0, 7)}`],
    [nextRow, start, `${text.slice(1)}\n${nextLine.slice(0, 7)}`],
  ]) {
    await page.evaluate(() => navigator.clipboard.writeText("before-touch-copy"));
    const before = frames.length;
    const client = await page.context().newCDPSession(page);
    await client.send("Input.dispatchTouchEvent", { type: "touchStart", touchPoints: [from] });
    await client.send("Input.dispatchTouchEvent", { type: "touchMove", touchPoints: [to] });
    await client.send("Input.dispatchTouchEvent", { type: "touchEnd", touchPoints: [] });
    await client.detach();
    await menu.waitFor();
    assert.equal(await menu.locator("[data-terminal-menu-copy]").innerText(), "Copy");
    await tapTouch(page, menu.locator("[data-terminal-menu-copy]"));
    await page.waitForFunction(() => navigator.clipboard.readText().then((value) => value !== "before-touch-copy"));
    assert.equal(await page.evaluate(() => navigator.clipboard.readText()), expected,
      `touch selection copied wrong cells (tracking=${tracking})`);
    assert.deepEqual(frames.slice(before).filter((frame) => ["input", "reply", "scroll"].includes(frame.type)), [],
      "Select text sent the touch drag to the application");
  }
  const beforeToggle = frames.length;
  const toggleClient = await page.context().newCDPSession(page);
  await toggleClient.send("Input.dispatchTouchEvent", { type: "touchStart", touchPoints: [start] });
  await chip.click();
  await toggleClient.send("Input.dispatchTouchEvent", { type: "touchMove", touchPoints: [end] });
  await toggleClient.send("Input.dispatchTouchEvent", { type: "touchEnd", touchPoints: [] });
  await toggleClient.detach();
  assert.equal(await menu.count(), 0, "disabling Select text mid-gesture opened its menu");
  assert.deepEqual(frames.slice(beforeToggle).filter((frame) => ["input", "reply", "scroll"].includes(frame.type)), [],
    "disabling Select text mid-gesture sent input to the application");
  // Preserve the existing rule that an active selection blocks scrolling.
  // Reset the terminal fixture, then check the toggle restored its route.
  const reset = "\x1bc\x1b[?1049h" + (tracking ? "\x1b[?1000h\x1b[?1006h" : "");
  socket.send(JSON.stringify({ type: "output", data: Buffer.from(`${reset}touch-scroll-restored`).toString("base64") }));
  await page.waitForFunction(() => document.querySelector(".xterm-rows")?.textContent?.includes("touch-scroll-restored"));
  const before = frames.length;
  await swipe(page, screen, "down");
  await waitFor(() => frames.slice(before).some((frame) => tracking
    ? frame.type === "reply" && Buffer.from(frame.data, "base64").toString().includes("\x1b[<64;")
    : frame.type === "scroll" && frame.direction === "up"), "scroll route after Select text is disabled");
  if (!tracking) {
    await swipe(page, screen, "up");
    await page.getByRole("button", { name: "Jump to live", exact: true }).waitFor({ state: "hidden" });
  }
}

async function assertSoftKeyInput(page, socket, frames) {
  const inputBytes = (start) => frames.slice(start)
    .filter((frame) => frame.type === "input")
    .map((frame) => Buffer.from(frame.data, "base64"));
  const physicalKey = async (chord, expected) => {
    await page.locator(".xterm-helper-textarea").focus();
    const start = frames.length;
    await page.keyboard.press(chord);
    await waitFor(() => inputBytes(start).length === 1, `${chord} physical input frame`);
    assert.deepEqual(inputBytes(start), [Buffer.from(expected)], `${chord} physical key bytes`);
    return inputBytes(start)[0];
  };
  const touchKey = async (name, expected, physicalChord) => {
    const physical = await physicalKey(physicalChord, expected);
    const start = frames.length;
    await tapTouch(page, page.getByRole("button", { name, exact: true }));
    await waitFor(() => inputBytes(start).length === 1, `${name} input frame`);
    assert.deepEqual(inputBytes(start), [physical], `${name} differs from the physical key gesture`);
    assert.equal(
      await page.evaluate(() => document.activeElement?.classList.contains("xterm-helper-textarea")),
      true,
      `${name} did not restore terminal focus`,
    );
  };

  await touchKey("Escape", "\x1b", "Escape");
  await touchKey("Tab", "\t", "Tab");

  await openArrowPad(page);
  await touchKey("Arrow up", "\x1b[A", "ArrowUp");
  await closeArrowPad(page);
  await openArrowPad(page);
  await touchKey("Arrow down", "\x1b[B", "ArrowDown");
  await closeArrowPad(page);
  await openArrowPad(page);
  await touchKey("Arrow left", "\x1b[D", "ArrowLeft");
  await closeArrowPad(page);
  await openArrowPad(page);
  await touchKey("Arrow right", "\x1b[C", "ArrowRight");
  await closeArrowPad(page);
  await openArrowPad(page);
  await touchKey("Backspace", "\x7f", "Backspace");
  await closeArrowPad(page);
  await openArrowPad(page);
  await touchKey("Clear screen", "\x0c", "Control+l");
  await closeArrowPad(page);

  socket.send(JSON.stringify({
    type: "output",
    data: Buffer.from("\x1b[?1h").toString("base64"),
  }));
  await page.waitForTimeout(50);
  await openArrowPad(page);
  await touchKey("Arrow up", "\x1bOA", "ArrowUp");
  await closeArrowPad(page);
  socket.send(JSON.stringify({
    type: "output",
    data: Buffer.from("\x1b[?1l").toString("base64"),
  }));

  const ctrl = page.getByRole("button", { name: "Ctrl modifier", exact: true });
  for (let offset = 0; offset < 26; offset += 1) {
    const letter = String.fromCharCode(97 + offset);
    const chord = `Control+${letter.toUpperCase()}`;
    const physical = await physicalKey(chord, String.fromCharCode(1 + offset));
    const start = frames.length;
    await tapTouch(page, ctrl);
    assert.equal(await ctrl.getAttribute("aria-pressed"), "true");
    assert.equal(inputBytes(start).length, 0, `arming Ctrl before ${letter} sent input`);
    await page.keyboard.press(letter);
    await waitFor(() => inputBytes(start).length === 1, `soft Ctrl+${letter} input frame`);
    assert.deepEqual(
      inputBytes(start),
      [physical],
      `soft Ctrl+${letter} differs from the physical key gesture`,
    );
    assert.equal(await ctrl.getAttribute("aria-pressed"), "false");
  }

  for (const [physicalChord, softChord] of [
    ["Control+2", "2"],
    ["Control+/", "/"],
    ["Control+Shift+C", "Shift+C"],
    ["Control+Shift+6", "Shift+6"],
    ["Control+Shift+/", "Shift+/"],
  ]) {
    let start = frames.length;
    await page.locator(".xterm-helper-textarea").focus();
    await page.keyboard.press(physicalChord);
    await page.waitForTimeout(50);
    assert.equal(inputBytes(start).length, 0, `${physicalChord} unexpectedly emitted xterm input`);

    start = frames.length;
    await tapTouch(page, ctrl);
    await page.keyboard.press(softChord);
    await page.waitForTimeout(50);
    assert.equal(inputBytes(start).length, 0, `soft ${physicalChord} emitted input`);
    assert.equal(await ctrl.getAttribute("aria-pressed"), "false");
  }

  const physicalCtrlRight = await physicalKey("Control+ArrowRight", "\x1b[1;5C");
  let start = frames.length;
  await tapTouch(page, ctrl);
  await openArrowPad(page);
  await tapTouch(page, page.getByRole("button", { name: "Arrow right", exact: true }));
  await waitFor(() => inputBytes(start).length === 1, "Ctrl+Right input frame");
  assert.deepEqual(
    inputBytes(start),
    [physicalCtrlRight],
    "soft Ctrl+Right differs from physical Ctrl+Right",
  );
  assert.equal(await ctrl.getAttribute("aria-pressed"), "false");
  await closeArrowPad(page);

  start = frames.length;
  await openArrowPad(page);
  await holdTouch(page, page.getByRole("button", { name: "Arrow left", exact: true }), 620);
  await waitFor(() => inputBytes(start).length >= 2, "Arrow left hold repeat");
  assert.ok(
    inputBytes(start).every((bytes) => bytes.equals(Buffer.from("\x1b[D"))),
    "arrow hold repeated anything except the corresponding key bytes",
  );
  const repeatedCount = inputBytes(start).length;
  await page.waitForTimeout(200);
  assert.equal(
    inputBytes(start).length,
    repeatedCount,
    "arrow repeat continued after the touch ended",
  );

  start = frames.length;
  await dragTouch(page, page.getByRole("button", { name: "Escape", exact: true }));
  await page.waitForTimeout(100);
  assert.equal(inputBytes(start).length, 0, "a scrolling movement across a soft key activated it");

  // Each literal key sends its own byte and nothing else. Presence and a 44px
  // box prove a control is drawn, not that it is wired: without this, deleting
  // the literal binding leaves every assertion green.
  const literalKey = async (name, expected) => {
    const key = page.getByRole("button", { name, exact: true });
    // The strip scrolls horizontally at the narrowest width, so a key can sit
    // outside the viewport until it is brought in.
    await key.scrollIntoViewIfNeeded();
    const from = frames.length;
    await tapTouch(page, key);
    await waitFor(() => inputBytes(from).length === 1, `${name} input frame`);
    assert.deepEqual(inputBytes(from), [Buffer.from(expected)], `${name} did not send its own byte`);
  };
  await literalKey("Forward slash", "/");
  await literalKey("Pipe", "|");
  await literalKey("Tilde", "~");

  // The input row buffers typing and dictation. Use the terminal's native
  // paste mode for the draft, then a separate Enter outside the pasted text.
  // A raw text+Enter burst lets a CLI's paste detector swallow the submit.
  socket.send(JSON.stringify({
    type: "output",
    data: Buffer.from("\x1b[?2004h\r\ncompose-paste-ready\r\n").toString("base64"),
  }));
  await page.waitForFunction(() => document.querySelector(".xterm-rows")?.textContent?.includes("compose-paste-ready"));
  const composeField = page.locator("[data-mobile-compose-input]");
  await composeField.focus();
  let composeFrom = frames.length;
  await page.keyboard.type("ls");
  await page.waitForTimeout(150);
  assert.equal(inputBytes(composeFrom).length, 0,
    "typing left for the session before the flush");
  assert.equal(await composeField.inputValue(), "ls", "the draft did not accumulate");

  await page.getByRole("button", { name: "Send", exact: true }).click();
  await waitFor(() => inputBytes(composeFrom).length > 0, "compose flush frames");
  await page.waitForTimeout(100);
  assert.deepEqual(
    inputBytes(composeFrom),
    [Buffer.from("\x1b[200~ls\x1b[201~"), Buffer.from("\r")],
    "Send must put the draft inside native paste markers and Enter after them exactly once",
  );
  assert.equal(await composeField.inputValue(), "", "the flush did not clear the field");

  // Multiline and composed Unicode text stay in the pasted draft. Normal
  // xterm paste normalizes its line breaks; none becomes a submit key.
  composeFrom = frames.length;
  await composeField.fill("first line\n第二行 café\n");
  await composeField.dispatchEvent("beforeinput", {
    inputType: "insertCompositionText", data: "é", isComposing: true,
  });
  assert.equal(inputBytes(composeFrom).length, 0, "composition editing sent the draft");
  await page.getByRole("button", { name: "Send", exact: true }).click();
  await waitFor(() => inputBytes(composeFrom).length >= 2, "multiline compose frames");
  await page.waitForTimeout(100);
  assert.deepEqual(inputBytes(composeFrom), [
    Buffer.from("\x1b[200~first line\r第二行 café\r\x1b[201~"), Buffer.from("\r"),
  ], "multiline Send lost text or included its submit inside the paste");
  assert.equal(frames.slice(composeFrom).some((frame) => frame.type === "reply"), false,
    "pasted user text was misclassified as an emulator reply");

  // An empty flush still answers a bare-Enter prompt.
  composeFrom = frames.length;
  await page.getByRole("button", { name: "Send", exact: true }).click();
  await waitFor(() => inputBytes(composeFrom).length === 1, "empty flush frame");
  assert.deepEqual(inputBytes(composeFrom), [Buffer.from("\r")],
    "an empty flush did not send the bare Return");

  // The soft keyboard's Return is the same flush.
  composeFrom = frames.length;
  await composeField.focus();
  await page.keyboard.type("pwd");
  await page.keyboard.press("Enter");
  await waitFor(() => inputBytes(composeFrom).length >= 2, "Return flush frames");
  assert.deepEqual(inputBytes(composeFrom), [Buffer.from("\x1b[200~pwd\x1b[201~"), Buffer.from("\r")],
    "Return did not flush the draft");
  assert.equal(await composeField.inputValue(), "", "Return left the draft behind");

  // Shells and other applications that do not enable bracketed paste still
  // receive their native paste bytes, followed by one ordinary Enter.
  socket.send(JSON.stringify({
    type: "output",
    data: Buffer.from("\x1b[?2004l\r\ncompose-plain-ready\r\n").toString("base64"),
  }));
  await page.waitForFunction(() => document.querySelector(".xterm-rows")?.textContent?.includes("compose-plain-ready"));
  composeFrom = frames.length;
  await composeField.fill("plain input");
  await page.getByRole("button", { name: "Send", exact: true }).click();
  await waitFor(() => inputBytes(composeFrom).length >= 2, "plain compose frames");
  await page.waitForTimeout(100);
  assert.deepEqual(inputBytes(composeFrom), [Buffer.from("plain input"), Buffer.from("\r")],
    "compose forced bracketed paste on an application that did not enable it");

  // The compose field never forwards Escape to the terminal.
  const composeInput = page.locator("[data-mobile-compose-input]");
  await composeInput.focus();
  start = frames.length;
  await page.keyboard.press("Escape");
  await page.waitForTimeout(100);
  assert.equal(inputBytes(start).length, 0, "compose field stole Escape as terminal input");
}

async function openArrowPad(page) {
  const arrowPad = page.getByRole("button", { name: "Arrow pad", exact: true });
  // A previous key may have left the strip scrolled away from this trigger.
  await arrowPad.scrollIntoViewIfNeeded();
  await tapTouch(page, arrowPad);
  const pad = page.locator('[data-terminal-arrow-pad-popover]');
  await pad.waitFor();
}

async function closeArrowPad(page) {
  await page.keyboard.press("Escape");
  await page.waitForSelector('[data-terminal-arrow-pad-popover]', { state: "detached" });
}

async function tapTouch(page, locator) {
  const box = await locator.boundingBox();
  assert.notEqual(box, null);
  const client = await page.context().newCDPSession(page);
  const point = { x: box.x + box.width / 2, y: box.y + box.height / 2 };
  await client.send("Input.dispatchTouchEvent", { type: "touchStart", touchPoints: [point] });
  await client.send("Input.dispatchTouchEvent", { type: "touchEnd", touchPoints: [] });
}

async function holdTouch(page, locator, duration) {
  const box = await locator.boundingBox();
  assert.notEqual(box, null);
  const client = await page.context().newCDPSession(page);
  const point = { x: box.x + box.width / 2, y: box.y + box.height / 2 };
  await client.send("Input.dispatchTouchEvent", { type: "touchStart", touchPoints: [point] });
  await page.waitForTimeout(duration);
  await client.send("Input.dispatchTouchEvent", { type: "touchEnd", touchPoints: [] });
}

async function dragTouch(page, locator) {
  const box = await locator.boundingBox();
  assert.notEqual(box, null);
  const client = await page.context().newCDPSession(page);
  const start = { x: box.x + box.width / 2, y: box.y + box.height / 2 };
  const end = { x: start.x + 20, y: start.y + 20 };
  await client.send("Input.dispatchTouchEvent", { type: "touchStart", touchPoints: [start] });
  await client.send("Input.dispatchTouchEvent", { type: "touchMove", touchPoints: [end] });
  await client.send("Input.dispatchTouchEvent", { type: "touchEnd", touchPoints: [] });
}

async function swipe(page, locator, direction) {
  const box = await locator.boundingBox();
  assert.notEqual(box, null);
  const x = box.x + box.width / 2;
  const startY = box.y + box.height * (direction === "up" ? 0.75 : 0.25);
  const endY = box.y + box.height * (direction === "up" ? 0.25 : 0.75);
  const client = await page.context().newCDPSession(page);
  await client.send("Input.dispatchTouchEvent", {
    type: "touchStart",
    touchPoints: [{ x, y: startY }],
  });
  for (let step = 1; step <= 4; step += 1) {
    const y = startY + (endY - startY) * step / 4;
    await page.waitForTimeout(30);
    await client.send("Input.dispatchTouchEvent", {
      type: "touchMove",
      touchPoints: [{ x, y }],
    });
  }
  await client.send("Input.dispatchTouchEvent", { type: "touchEnd", touchPoints: [] });
}

async function waitFor(predicate, label) {
  const deadline = Date.now() + 5000;
  while (Date.now() < deadline) {
    if (predicate()) return;
    await new Promise((resolveWait) => setTimeout(resolveWait, 25));
  }
  throw new Error(`timed out waiting for ${label}`);
}

function pixelDifference(leftBuffer, rightBuffer) {
  const left = PNG.sync.read(leftBuffer);
  const right = PNG.sync.read(rightBuffer);
  assert.equal(left.width, right.width);
  assert.equal(left.height, right.height);
  let changed = 0;
  for (let offset = 0; offset < left.data.length; offset += 4) {
    if (left.data[offset] !== right.data[offset] ||
        left.data[offset + 1] !== right.data[offset + 1] ||
        left.data[offset + 2] !== right.data[offset + 2] ||
        left.data[offset + 3] !== right.data[offset + 3]) changed += 1;
  }
  return changed / (left.width * left.height);
}
