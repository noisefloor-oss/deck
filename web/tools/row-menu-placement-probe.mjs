// Real-browser probe for where a sidebar row menu opens and how
// it holds that placement afterward. The popover region is fixed
// to the viewport, so a menu that hangs below a row near the bottom of the
// screen is clipped by that edge and its actions cannot be clicked at all.
// A row with room below still opens downward; a row without it opens upward,
// with the menu's bottom edge on the row's top edge; a menu taller than a
// short viewport pins inside it and scrolls. Placement is re-derived when
// the list scrolls or the window resizes under an open menu, so the menu
// never stays behind at stale coordinates. The project list and the session
// list share one placement path, so the probe drives both kinds. This is a
// desktop contract. On phones, project actions use a bottom sheet and
// session actions use swipe; the mobile project sheet is checked separately.
import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import { existsSync } from "node:fs";
import net from "node:net";

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


// The 2px the menu overlaps its row, mirrored when the menu flips. Real
// layout lands on fractional pixels, so compare within a pixel.
const OVERLAP = 2;
const CLOSE_ENOUGH = 1;

// A row menu's trigger is revealed by hover, so a click without one lands on
// a pointer-events:none button and silently does nothing.
async function openRowMenu(page, rowSelector, triggerSelector) {
  await page.locator(rowSelector).hover();
  await page.locator(triggerSelector).click();
  await settle(page);
  const menu = page.locator("[data-region=popovers] [role=menu]");
  await menu.waitFor({ state: "visible" });
  return menu;
}

async function closeRowMenu(page) {
  await page.keyboard.press("Escape");
  await page.locator("[data-region=popovers] [role=menu]").waitFor({ state: "detached" });
}

const boxOf = async (locator) => {
  const box = await locator.boundingBox();
  assert.notEqual(box, null, "element has no layout box");
  return { ...box, bottom: box.y + box.height };
};

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
  // Enough projects that the list scrolls in a short viewport, so the last
  // row can be brought to the bottom edge where the bug lives. The last
  // project carries the sessions, which puts session rows at the bottom
  // too — one fixture drives both row kinds.
  const projects = Array.from({ length: 40 }, (_, index) => ({
    id: `project-${index + 1}`,
    // Zero-padded so the sidebar's name order matches the array order
    // and the last entry is the last row.
    name: `project ${String(index + 1).padStart(2, "0")}`,
    canonical_path: `/workspace/project-${index + 1}`,
    last_used_at: "2026-08-01T00:00:00Z",
  }));
  const lastProject = projects.at(-1).id;
  const sessions = {
    [lastProject]: [
      { id: "session-1", project_id: lastProject, display_name: "one", provider: "claude", status: "ready", presentation_state: "running", generation: 1, created_at: "2026-08-01T00:00:00Z", updated_at: "2026-08-01T00:00:00Z" },
      { id: "session-2", project_id: lastProject, display_name: "two", provider: "codex", status: "ready", presentation_state: "running", generation: 1, created_at: "2026-08-01T00:00:00Z", updated_at: "2026-08-01T00:00:00Z" },
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
    if (request.method() === "GET" && projects.some((project) => path === `/api/projects/${project.id}/preview/settings`)) {
      return json(200, { configured: true, self_service: true, enabled: false });
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
      return json(200, { id, project_id: lastProject, provider: "claude", display_name: "session", status: "running", generation: 1 });
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
  const viewportHeight = () => page.evaluate(() => window.innerHeight);

  // ---- a row with room below still opens downward ----
  const topRow = '[data-project="project-1"] [class~="group/project"]';
  let menu = await openRowMenu(page, topRow, '[data-project-menu="project-1"]');
  let menuBox = await boxOf(menu);
  let rowBox = await boxOf(page.locator(topRow));
  const menuHeight = menuBox.height;
  assert.ok(
    rowBox.bottom + menuHeight < await viewportHeight(),
    "fixture: the top project row has room for its whole menu below it",
  );
  assert.ok(
    Math.abs(menuBox.y - (rowBox.bottom - OVERLAP)) <= CLOSE_ENOUGH,
    `a menu with room below should hang from the row bottom: menu top ${menuBox.y}, row bottom ${rowBox.bottom}`,
  );
  console.log(`ok: a project row menu with room below opens downward (height ${menuHeight})`);
  await closeRowMenu(page);

  // ---- a row against the bottom edge opens upward ----
  // A list longer than its scroll container is what puts a row against the
  // bottom edge: scroll to the end so the last row sits on the container's
  // bottom edge, a few pixels from the viewport's.
  const scrollListToEnd = () => page.locator("[data-project-list]").evaluate((list) => {
    const scroller = list.parentElement;
    scroller.scrollTop = scroller.scrollHeight;
  });

  for (const [label, rowSelector, triggerSelector] of [
    ["project", `[data-project="${lastProject}"] [class~="group/project"]`, `[data-project-menu="${lastProject}"]`],
    ["session", '[data-session="session-2"]', '[data-session-menu="session-2"]'],
  ]) {
    if (label === "session") {
      // The sessions hang under the last project; expand it if the sidebar
      // did not restore it open.
      await scrollListToEnd();
      const toggle = page.locator(`[data-project-toggle="${lastProject}"]`);
      if (await toggle.getAttribute("aria-expanded") !== "true") await toggle.click();
      await page.locator('[data-session="session-2"]').waitFor({ state: "visible" });
    }
    await scrollListToEnd();
    await settle(page);
    rowBox = await boxOf(page.locator(rowSelector));

    menu = await openRowMenu(page, rowSelector, triggerSelector);
    menuBox = await boxOf(menu);
    const height = await viewportHeight();
    assert.ok(
      rowBox.bottom - OVERLAP + menuBox.height > height,
      `fixture: a downward ${label} menu must overflow the ${height}px viewport (row bottom ${rowBox.bottom}, menu ${menuBox.height})`,
    );
    assert.ok(
      rowBox.y >= menuBox.height,
      `fixture: the space above the ${label} row must hold the menu (row top ${rowBox.y}, menu ${menuBox.height})`,
    );
    assert.ok(
      Math.abs(menuBox.bottom - (rowBox.y + OVERLAP)) <= CLOSE_ENOUGH,
      `an upward ${label} menu should put its bottom edge on the row top: menu bottom ${menuBox.bottom}, row top ${rowBox.y}`,
    );
    assert.ok(
      menuBox.y >= 0 && menuBox.bottom <= height + CLOSE_ENOUGH,
      `the ${label} menu must stay inside the viewport: ${menuBox.y}..${menuBox.bottom} of ${height}`,
    );
    console.log(`ok: a ${label} row menu at the bottom edge opens upward and stays on screen`);
    await closeRowMenu(page);
  }

  // ---- a menu left open tracks a viewport that shrinks under it ----
  // The menu is placed in viewport coordinates once per open; a resize after
  // that would leave it running off the bottom edge with its actions
  // unreachable. Re-placement on resize keeps it inside.
  {
    await scrollListToEnd();
    await settle(page);
    menu = await openRowMenu(page, `[data-project="${lastProject}"] [class~="group/project"]`, `[data-project-menu="${lastProject}"]`);
    menuBox = await boxOf(menu);
    const height = await viewportHeight();
    assert.ok(menuBox.bottom <= height + CLOSE_ENOUGH, "fixture: the open menu starts inside the viewport");
    await page.setViewportSize({ width: 1024, height: Math.floor(menuBox.y) - 40 });
    await settle(page);
    menuBox = await boxOf(menu);
    const shrunk = await viewportHeight();
    assert.ok(
      menuBox.y >= 0 && menuBox.bottom <= shrunk + CLOSE_ENOUGH,
      `a menu left open must re-place inside a shrunken viewport: menu ${menuBox.y}..${menuBox.bottom} of ${shrunk}`,
    );
    console.log(`ok: an open project row menu re-places inside a shrunken viewport (${menuBox.y}..${menuBox.bottom} of ${shrunk})`);
    await closeRowMenu(page);
    await page.setViewportSize({ width: 1024, height: 768 });
    await settle(page);
  }

  // ---- a menu left open follows its row when the list scrolls ----
  // A scroll that carries the row moves the row's viewport rect; a menu
  // left at the old coordinates points at nothing and can end up clipped.
  // Containment alone does not prove re-placement — the stale coordinates
  // can still be on screen — so the menu must hang from the row's current
  // rect and must have moved from where it opened.
  {
    // A row high enough that the menu opens downward at this height (the
    // project menu grew a row when Open preview joined it): a row that
    // flips upward at open lands both placements on the viewport's top
    // edge, where "moved" has no witness.
    const midRow = '[data-project="project-5"] [class~="group/project"]';
    await page.locator("[data-project-list]").evaluate((list) => {
      list.parentElement.scrollTop = 0;
    });
    await settle(page);
    menu = await openRowMenu(page, midRow, '[data-project-menu="project-5"]');
    const openedBox = await boxOf(menu);
    // A bounded scroll, not to the end: the project menu grew a row
    // (Open preview joined it), and a row scrolled fully above the
    // viewport rightly takes the pinned-inside clamp, which no longer
    // hangs from the row. Scrolling by less keeps the row's bottom edge
    // on screen, so the re-placement still has to hang from the row's
    // current rect — and a row this high opens downward, so the move
    // itself stays provable too.
    await page.locator("[data-project-list]").evaluate((list) => {
      list.parentElement.scrollTop = 270;
    });
    await settle(page);
    menuBox = await boxOf(menu);
    rowBox = await boxOf(page.locator(midRow));
    const height = await viewportHeight();
    assert.ok(
      menuBox.y >= 0 && menuBox.bottom <= height + CLOSE_ENOUGH,
      `a menu whose row scrolled must stay inside the viewport: menu ${menuBox.y}..${menuBox.bottom} of ${height}`,
    );
    assert.ok(
      Math.abs(menuBox.y - openedBox.y) > 1,
      `a menu whose row scrolled must re-place, not hold its open coordinates: opened ${openedBox.y}, now ${menuBox.y}`,
    );
    assert.ok(
      Math.abs(menuBox.y - (rowBox.bottom - OVERLAP)) <= CLOSE_ENOUGH
        || Math.abs(menuBox.bottom - (rowBox.y + OVERLAP)) <= CLOSE_ENOUGH,
      `a re-placed menu must hang from its row's current rect: menu ${menuBox.y}..${menuBox.bottom}, row ${rowBox.y}..${rowBox.bottom}`,
    );
    console.log(`ok: an open project row menu re-places when its row scrolls (menu ${openedBox.y} -> ${menuBox.y}..${menuBox.bottom}, row ${rowBox.y}..${rowBox.bottom})`);
    await closeRowMenu(page);
  }

  // ---- a menu taller than a short viewport clamps, scrolls, and stays
  // keyboard-reachable ----
  // Neither side of a bottom row holds the menu in a viewport shorter than
  // the menu itself: it pins inside the viewport, scrolls within itself,
  // and every row stays reachable — Tab walks the items into view and
  // Escape closes back to the trigger.
  {
    await page.setViewportSize({ width: 1024, height: 300 });
    await scrollListToEnd();
    await settle(page);
    menu = await openRowMenu(page, `[data-project="${lastProject}"] [class~="group/project"]`, `[data-project-menu="${lastProject}"]`);
    menuBox = await boxOf(menu);
    const short = await viewportHeight();
    assert.ok(
      menuBox.y >= 0 && menuBox.bottom <= short + CLOSE_ENOUGH,
      `a menu taller than the ${short}px viewport must stay inside it: ${menuBox.y}..${menuBox.bottom}`,
    );
    const scrollable = await menu.evaluate((el) => el.scrollHeight > el.clientHeight);
    assert.ok(scrollable, "a clamped menu scrolls within itself rather than clipping rows");
    const items = menu.locator("[role=menuitem]");
    const count = await items.count();
    assert.ok(
      await items.first().evaluate((el) => el === document.activeElement),
      "an opened row menu focuses its first item",
    );
    const last = items.last();
    for (let i = 0; i < count - 1; i++) await page.keyboard.press("Tab");
    assert.ok(
      await last.evaluate((el) => el === document.activeElement),
      "Tab walks a clamped menu's rows to the last one",
    );
    const lastInView = await last.evaluate((el) => {
      const r = el.getBoundingClientRect();
      return r.top >= 0 && r.bottom <= window.innerHeight;
    });
    assert.ok(lastInView, "the last row of a clamped menu tabs into view");
    console.log(`ok: a menu taller than a short viewport clamps, scrolls, and every row tabs into view`);
    await page.keyboard.press("Escape");
    await menu.waitFor({ state: "detached" });
    assert.ok(
      await page.locator(`[data-project-menu="${lastProject}"]`).evaluate((el) => el === document.activeElement),
      "Escape returns focus to the menu's trigger",
    );
    console.log("ok: Escape dismisses a clamped menu back to its trigger");
    await page.setViewportSize({ width: 1024, height: 768 });
    await settle(page);
  }

  assert.deepEqual(pageErrors, [], "page errors");
  console.log("row-menu-placement: PASS");
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
