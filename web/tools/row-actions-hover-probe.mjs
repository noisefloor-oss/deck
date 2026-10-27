// Real-browser probe for the sidebar row action clusters: a mouse click must
// not leave a project's + / ⋯ or a session's ⋯ visible when the pointer
// leaves the row, while keyboard focus must still reveal them (the Fastmail
// model). Driven against the production composition with deterministic
// HTTP mocks.
import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import { existsSync } from "node:fs";
import net from "node:net";
import { eventually } from "./eventually.mjs";

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

const settle = (page) => page.evaluate(
  () => new Promise((resolve) => requestAnimationFrame(() => requestAnimationFrame(resolve))),
);

function clusterState(el) {
  if (!(el instanceof HTMLElement)) return { mounted: false };
  const style = getComputedStyle(el);
  return {
    mounted: true,
    opacity: style.opacity,
    pointerEvents: style.pointerEvents,
    visible: style.opacity !== "0" && style.pointerEvents !== "none",
  };
}

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

  const gitIdentity = {
    id: "identity-1", name: "Probe identity", email: "probe@example.com", revision: 1,
    created_at: "2026-08-01T00:00:00Z", updated_at: "2026-08-01T00:00:00Z",
  };
  const projects = [
    { id: "project-1", name: "alpha", canonical_path: "/workspace/alpha", last_used_at: "2026-08-01T00:00:00Z" },
    { id: "project-2", name: "beta", canonical_path: "/workspace/beta", last_used_at: "2026-08-01T00:00:00Z" },
  ];
  const sessions = {
    "project-1": [
      { id: "session-1", project_id: "project-1", display_name: "alpha-one", provider: "claude", status: "ready", presentation_state: "running", generation: 1, created_at: "2026-08-01T00:00:00Z", updated_at: "2026-08-01T00:00:00Z" },
    ],
    "project-2": [
      { id: "session-2", project_id: "project-2", display_name: "beta-one", provider: "codex", status: "ready", presentation_state: "running", generation: 1, created_at: "2026-08-01T00:00:00Z", updated_at: "2026-08-01T00:00:00Z" },
    ],
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
      return route.fulfill({ status: 200, contentType: "text/event-stream", body: ": connected\n\nretry: 3600000\n\n" });
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
    if (request.method() === "GET" && path === "/api/settings") {
      return json(200, { skip_permissions: false, skip_close_confirm: false, skip_remove_project_confirm: false });
    }
    if (request.method() === "GET" && path === "/api/git-identities") {
      return json(200, { profiles: [gitIdentity], default_profile_id: gitIdentity.id, revision: 1 });
    }
    if (request.method() === "GET" && path === "/api/projects") return json(200, projects);
    if (request.method() === "GET" && path.startsWith("/api/projects/") && path.endsWith("/sessions")) {
      const projectId = path.split("/")[3];
      return json(200, sessions[projectId] ?? []);
    }
    if (/^\/api\/projects\/[^/]+\/lanes$/.test(path)) return json(200, { lanes: [] });
    if (request.method() === "GET" && path.includes("/taskmaster")) {
      return json(404, { error: "not_taskmaster_project", message: "not a Tasks project" });
    }
    const sessionDetailMatch = path.match(/^\/api\/sessions\/([^/]+)$/);
    if (request.method() === "GET" && sessionDetailMatch) {
      const id = sessionDetailMatch[1];
      return json(200, { id, project_id: "project-1", provider: "claude", display_name: "session", status: "running", generation: 1 });
    }
    if (request.method() === "GET" && path.match(/^\/api\/sessions\/[^/]+\/messages$/)) {
      return json(200, { messages: [], input_dirty: false });
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

  await page.goto(origin, { waitUntil: "networkidle" });
  await page.locator('[data-project="project-1"]').waitFor();

  // ---- diagnosis helpers ----
  const projectCluster = () => page.locator('[data-project="project-2"] [data-project-actions]');
  const sessionCluster = () => page.locator('[data-session="session-2"] .session-menu-trigger');
  // The tick the trigger overlays: hidden exactly while the trigger shows.
  const sessionTick = () => page.locator('[data-session-meta="session-2"]');
  const tickOpacity = () => sessionTick().evaluate((el) => getComputedStyle(el).opacity);
  const activeElement = () => page.evaluate(() => {
    const el = document.activeElement;
    if (!(el instanceof HTMLElement)) return { tag: el?.tagName ?? "null", dataset: {}, focusVisible: false };
    return { tag: el.tagName, dataset: { ...el.dataset }, ariaExpanded: el.getAttribute("aria-expanded"), focusVisible: el.matches(":focus-visible") };
  });
  const clusterInfo = (locatorFn) => locatorFn().evaluate(clusterState);

  // ---- project row: mouse click must not pin the cluster ----
  const projRow = page.locator('[data-project="project-2"]');
  await projRow.hover();
  await settle(page);
  let info = await clusterInfo(projectCluster);
  assert.ok(info.visible, "hover reveals the project action cluster before the click");
  console.log("ok: hover reveals the project action cluster");

  await page.locator('[data-project-toggle="project-2"]').click();
  await settle(page);
  const afterClick = await activeElement();
  info = await clusterInfo(projectCluster);
  console.log(`after click: activeElement=${afterClick.tag} dataset=${JSON.stringify(afterClick.dataset)} focusVisible=${afterClick.focusVisible} expanded=${afterClick.ariaExpanded} cluster visible=${info.visible} opacity=${info.opacity} pointerEvents=${info.pointerEvents}`);

  // Move the pointer off the row entirely.
  await page.mouse.move(800, 600);
  info = await eventually(
    () => clusterInfo(projectCluster),
    (i) => !i.visible,
    "project action cluster hides after clicking and moving the pointer off the row",
    2000,
  );
  const afterMove = await activeElement();
  console.log(`after move:  activeElement=${afterMove.tag} dataset=${JSON.stringify(afterMove.dataset)} focusVisible=${afterMove.focusVisible} cluster visible=${info.visible} opacity=${info.opacity} pointerEvents=${info.pointerEvents}`);
  console.log("ok: project action cluster hides after click + pointer leave");

  // ---- keyboard focus must reveal the cluster ----
  // After the mouse click the toggle still has focus. An ArrowDown walks the
  // roving tabindex to the session row below; that move is keyboard-initiated,
  // so :focus-visible matches and the session's menu trigger reveals.
  await page.keyboard.press("ArrowDown");
  const sessRow = page.locator('[data-session="session-2"]');
  await sessRow.waitFor({ state: "visible" });
  info = await eventually(
    () => clusterInfo(sessionCluster),
    (i) => i.visible,
    "keyboard-focused session row reveals its menu trigger",
    2000,
  );
  await eventually(
    tickOpacity,
    (opacity) => opacity === "0",
    "keyboard focus on the row fades the tick under the trigger",
    2000,
  );
  const afterArrowDown = await activeElement();
  console.log(`after arrowDown: activeElement=${afterArrowDown.tag} dataset=${JSON.stringify(afterArrowDown.dataset)} focusVisible=${afterArrowDown.focusVisible} cluster visible=${info.visible} opacity=${info.opacity} pointerEvents=${info.pointerEvents}`);
  console.log("ok: keyboard focus reveals the session menu trigger");

  // ArrowUp walks back to the project toggle; the project cluster should now
  // reveal because focus arrived by keyboard, even though the earlier mouse
  // click did not pin it.
  await page.keyboard.press("ArrowUp");
  info = await eventually(
    () => clusterInfo(projectCluster),
    (i) => i.visible,
    "keyboard-focused project row reveals its action cluster",
    2000,
  );
  const afterArrowUp = await activeElement();
  console.log(`after arrowUp:   activeElement=${afterArrowUp.tag} dataset=${JSON.stringify(afterArrowUp.dataset)} focusVisible=${afterArrowUp.focusVisible} cluster visible=${info.visible} opacity=${info.opacity} pointerEvents=${info.pointerEvents}`);
  console.log("ok: keyboard focus reveals the project action cluster");

  // ---- session row: mouse click must not pin the trigger ----
  // Click the session row (which selects it and focuses it by mouse), then
  // move the pointer off; the trigger should hide.
  await sessRow.click();
  await settle(page);
  info = await clusterInfo(sessionCluster);
  const sessAfterClick = await activeElement();
  console.log(`session after click: activeElement=${sessAfterClick.tag} dataset=${JSON.stringify(sessAfterClick.dataset)} focusVisible=${sessAfterClick.focusVisible} cluster visible=${info.visible} opacity=${info.opacity} pointerEvents=${info.pointerEvents}`);

  await page.mouse.move(800, 600);
  info = await eventually(
    () => clusterInfo(sessionCluster),
    (i) => !i.visible,
    "session menu trigger hides after clicking and moving the pointer off the row",
    2000,
  );
  const sessAfterMove = await activeElement();
  console.log(`session after move:  activeElement=${sessAfterMove.tag} dataset=${JSON.stringify(sessAfterMove.dataset)} focusVisible=${sessAfterMove.focusVisible} cluster visible=${info.visible} opacity=${info.opacity} pointerEvents=${info.pointerEvents}`);
  console.log("ok: session menu trigger hides after click + pointer leave");

  // ---- the age tick is the other half of the trade ----
  // The tick is hidden only while the trigger overlays it. A mouse click
  // reveals no trigger, so the tick has to be back once the pointer leaves.
  const tickInfo = await eventually(
    tickOpacity,
    (opacity) => opacity === "1",
    "the age tick returns after clicking the session row and leaving it",
    2000,
  );
  console.log(`session tick after click + leave: opacity=${tickInfo}`);
  console.log("ok: the age tick is visible on a clicked, unhovered row");

  // ---- Tab onto a control inside the row ----
  // The row's own controls are in the tab order, and pointer-events: none
  // does not take them out of it. A Tab that lands on the ⋯ has to reveal
  // it, or the user is focused on something invisible.
  await page.keyboard.press("Tab");
  info = await eventually(
    () => clusterInfo(sessionCluster),
    (i) => i.visible,
    "a Tab onto the session ⋯ reveals it",
    2000,
  );
  const onSessionMenu = await activeElement();
  console.log(`tab into session row: activeElement=${onSessionMenu.tag} dataset=${JSON.stringify(onSessionMenu.dataset)} focusVisible=${onSessionMenu.focusVisible} cluster visible=${info.visible} opacity=${info.opacity}`);
  assert.equal(onSessionMenu.dataset.sessionMenu, "session-2", "Tab lands on the session menu trigger");
  await eventually(
    tickOpacity,
    (opacity) => opacity === "0",
    "keyboard focus on the ⋯ fades the tick under it",
    2000,
  );
  console.log("ok: a Tab onto the session ⋯ reveals it");

  // The project row's cluster answers the same gesture: click the toggle,
  // leave the row, then Tab onto the +.
  await page.locator('[data-project-toggle="project-2"]').click();
  await page.mouse.move(800, 600);
  await eventually(
    () => clusterInfo(projectCluster),
    (i) => !i.visible,
    "project cluster hides again before the Tab",
    2000,
  );
  await page.keyboard.press("Tab");
  info = await eventually(
    () => clusterInfo(projectCluster),
    (i) => i.visible,
    "a Tab onto the project + reveals the cluster",
    2000,
  );
  const onProjectPlus = await activeElement();
  console.log(`tab into project row: activeElement=${onProjectPlus.tag} dataset=${JSON.stringify(onProjectPlus.dataset)} focusVisible=${onProjectPlus.focusVisible} cluster visible=${info.visible} opacity=${info.opacity}`);
  assert.ok("newSession" in onProjectPlus.dataset, "Tab lands on the project new-session button");
  console.log("ok: a Tab onto the project + reveals the cluster");

  assert.deepEqual(pageErrors, [], "page errors");
  console.log("row-actions-hover: PASS");
  await context.close();
} catch (error) {
  console.error(error);
  process.exitCode = 1;
} finally {
  await browser?.close().catch(() => {});
  vite.kill("SIGTERM");
  vite.stdout.destroy();
  vite.stderr.destroy();
}
