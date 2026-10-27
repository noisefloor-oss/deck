// Real-browser probe for the PROJECTS header menu's two contracts, both
// of which need real geometry and a real keyboard: the open menu owns
// focus (Tab cannot walk out to the page behind the full-viewport
// backdrop, arrows walk the rows, Escape closes and restores the trigger
// from wherever focus sits) and the panel tracks its trigger (across a
// sidebar scroll and a viewport resize, and a full render that replaces
// the trigger dismisses the menu instead of leaving it pinned to a
// detached node). Driven against the production composition with the
// same deterministic HTTP/WS mocks the shortcuts probe uses; the sidebar
// carries fourteen projects so its list actually scrolls.
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

// The row the keyboard cursor sits on, as the data-projects-menu-* key
// with the trigger's own key excluded, so null means focus left the menu.
const activeRow = (page) => page.evaluate(() => {
  const el = document.activeElement;
  if (!(el instanceof HTMLElement)) return null;
  const key = Object.keys(el.dataset).find((k) =>
    k.startsWith("projectsMenu") && k !== "projectsMenu");
  return key ?? null;
});

const geometry = (page) => page.evaluate(() => {
  const trigger = document.querySelector("[data-projects-menu]");
  const menu = document.querySelector("[data-projects-menu-popover]");
  if (!(trigger instanceof HTMLElement) || !(menu instanceof HTMLElement)) return null;
  const t = trigger.getBoundingClientRect();
  const m = menu.getBoundingClientRect();
  return { tb: t.bottom, tl: t.left, mt: m.top, ml: m.left };
}).then((value) => {
  assert.notEqual(value, null, "trigger and menu are both mounted");
  return value;
});

function near(actual, expected, tolerance, label) {
  assert.ok(
    Math.abs(actual - expected) <= tolerance,
    `${label}: ${actual} is not within ${tolerance} of ${expected}`,
  );
}

const settle = (page) => page.evaluate(
  () => new Promise((resolve) => requestAnimationFrame(() => requestAnimationFrame(resolve))),
);

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
  let emptyList = false;
  let registryFails = false;
  const gitIdentity = {
    id: "identity-1", name: "Probe identity", email: "probe@example.com", revision: 1,
    created_at: "2026-08-01T00:00:00Z", updated_at: "2026-08-01T00:00:00Z",
  };
  const projects = Array.from({ length: 40 }, (_, index) => ({
    id: `project-${index + 1}`,
    name: `probe-project-${String(index + 1).padStart(2, "0")}`,
    canonical_path: `/workspace/probe-${index + 1}`,
    last_used_at: "2026-08-01T00:00:00Z",
  }));
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
    // The New session picker ranks by recorded launches; this probe is not
    // about ranking, so an empty history leaves the served order.
    if (request.method() === "GET" && path === "/api/harness-usage") {
      return json(200, { global: [], project: [] });
    }
    if (request.method() === "GET" && path === "/api/providers") {
      // The probe opens the New session picker and cancels; the inventory
      // must render but no provider is selected, so the project's sessions
      // define the sensible fixture.
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
    if (request.method() === "GET" && path === "/api/projects") {
      if (registryFails) {
        return json(503, { error: "temporarily_unavailable", message: "project registry unavailable" });
      }
      return json(200, emptyList ? [] : projects);
    }
    // Add project reads the mount listing for its folder example.
    if (request.method() === "GET" && path === "/api/project-directories" && !new URL(request.url()).searchParams.has("path")) {
      return json(200, { current_path: "", parent_path: null, directories: [{ name: "workspace", path: "/workspace" }] });
    }
    if (request.method() === "GET" && path.startsWith("/api/projects/") && path.endsWith("/sessions")) return json(200, []);
    if (/^\/api\/projects\/[^/]+\/lanes$/.test(path)) return json(200, { lanes: [] });
    if (request.method() === "GET" && path.includes("/taskmaster")) {
      return json(404, { error: "not_taskmaster_project", message: "not a Tasks project" });
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
  const trigger = page.locator("[data-projects-menu]");
  await trigger.waitFor();
  const popover = page.locator("[data-projects-menu-popover]");

  // ---- the live no-session pane (comp desktop-r16) ----
  // Projects exist but no session is open, so the pane renders the
  // no-session state: the project in context named in the header and the
  // body, Files/Tasks still present, and the pane's one action opening
  // the same provider picker the sidebar's + opens.
  await page.locator('[data-empty-pane="no-session"]').waitFor();
  assert.equal(await page.locator("[data-empty-title]").textContent(), "No session open");
  assert.equal(
    await page.locator("[data-empty-body]").textContent(),
    "Pick a session from the side panel, or start a new one in probe-project-01.",
    "the body names the first project the sidebar presents",
  );
  assert.equal(
    await page.locator('[data-region="terminal-titlebar"] [data-titlebar-title]').textContent(),
    "probe-project-01",
    "the header falls back to the project in context",
  );
  for (const hook of ["[data-live-files]", "[data-live-tasks]", "[data-live-more]"]) {
    assert.ok(await page.locator(hook).count() > 0, `${hook} stays while a project is in context`);
  }
  await page.locator("[data-empty-action]").click();
  await page.locator("[data-new-session-modal]").waitFor();
  await page.locator("[data-new-session-cancel]").click();
  await page.locator("[data-new-session-modal]").waitFor({ state: "detached" });
  console.log("ok: the no-session pane names the project and its action opens the picker");

  // ---- the keyboard contract ----
  await trigger.click();
  await popover.waitFor();
  assert.equal(await trigger.getAttribute("aria-expanded"), "true", "opening presses the trigger");
  assert.equal(await activeRow(page), "projectsMenuNewProject", "opening focuses the first row");

  const order = [
    "projectsMenuNewSession",
    "projectsMenuSort",
    "projectsMenuExpandAll",
    "projectsMenuCollapseAll",
  ];
  for (const row of order) {
    await page.keyboard.press("ArrowDown");
    assert.equal(await activeRow(page), row, `ArrowDown reaches ${row}`);
  }
  await page.keyboard.press("ArrowDown");
  assert.equal(await activeRow(page), "projectsMenuNewProject", "ArrowDown wraps to the first row");
  await page.keyboard.press("ArrowUp");
  assert.equal(await activeRow(page), "projectsMenuCollapseAll", "ArrowUp wraps to the last row");
  await page.keyboard.press("Home");
  assert.equal(await activeRow(page), "projectsMenuNewProject", "Home lands on the first row");
  await page.keyboard.press("End");
  assert.equal(await activeRow(page), "projectsMenuCollapseAll", "End lands on the last row");
  console.log("ok: arrows walk the menu rows and wrap at both ends");

  for (let tab = 0; tab < 8; tab += 1) {
    await page.keyboard.press(tab % 2 === 0 ? "Tab" : "Shift+Tab");
    assert.ok((await activeRow(page)) !== null, `Tab ${tab} keeps focus on a menu row`);
  }
  console.log("ok: Tab cycles inside the open menu");

  // Focus blurred to <body> is pulled back into the open menu, and Escape
  // closes and restores the trigger from wherever focus ended up.
  await page.evaluate(() => { (document.activeElement instanceof HTMLElement) && document.activeElement.blur(); });
  assert.ok((await activeRow(page)) !== null, "an escaped focus returns to the menu");
  await page.keyboard.press("Escape");
  await popover.waitFor({ state: "detached" });
  assert.equal(await trigger.getAttribute("aria-expanded"), "false");
  assert.equal(
    await trigger.evaluate((el) => document.activeElement === el),
    true, "Escape restores focus to the trigger",
  );
  console.log("ok: Escape closes from anywhere and restores the trigger");

  // ---- the anchoring contract ----
  const scrollable = await page.evaluate(() => {
    const scroller = document.querySelector('[data-region="sidebar"] .overflow-y-auto');
    return scroller instanceof HTMLElement && scroller.scrollHeight > scroller.clientHeight + 40;
  });
  assert.ok(scrollable, "fourteen projects make the sidebar list scroll");

  await trigger.click();
  await popover.waitFor();
  await settle(page);
  const atRest = await geometry(page);
  near(atRest.mt, atRest.tb + 2, 1, "the panel opens just below the trigger");

  await page.evaluate(() => {
    const scroller = document.querySelector('[data-region="sidebar"] .overflow-y-auto');
    scroller.scrollTop = 160;
  });
  await settle(page);
  const scrolled = await geometry(page);
  assert.ok(scrolled.tb < atRest.tb - 100, "the scroll moved the trigger upward");
  near(scrolled.mt, scrolled.tb + 2, 1, "the panel follows the trigger under scroll");
  near(scrolled.ml, scrolled.tl, 1, "the panel keeps the trigger's left edge under scroll");
  console.log("ok: the panel tracks its trigger across a sidebar scroll");

  await page.setViewportSize({ width: 360, height: 430 });
  await settle(page);
  const narrow = await geometry(page);
  assert.ok(
    Math.abs(narrow.tb - atRest.tb) > 4,
    "the narrow viewport moved the trigger (the case is not vacuous)",
  );
  near(narrow.mt, narrow.tb + 2, 1, "the panel follows the trigger across a resize");
  near(narrow.ml, narrow.tl, 1, "the panel keeps the trigger's left edge across a resize");
  await page.keyboard.press("Escape");
  await popover.waitFor({ state: "detached" });
  console.log("ok: the panel tracks its trigger across a resize, and Escape still closes");

  // ---- a render that replaces the trigger dismisses the menu ----
  await page.setViewportSize({ width: 1024, height: 768 });
  await page.locator('[data-project-toggle="project-1"]').click();
  await page.locator('[data-project="project-1"] [data-selected="true"]').waitFor();
  await trigger.click();
  await popover.waitFor();
  assert.equal(await activeRow(page), "projectsMenuNewProject");
  // ⌘R routes through the app chord into an inline rename, whose render
  // rebuilds the header the trigger lives in.
  await page.keyboard.press("Meta+r");
  const renameInput = page.locator("[data-rename-project-input]");
  await renameInput.waitFor();
  assert.equal(await popover.count(), 0, "no menu stays mounted against the detached trigger");
  assert.equal(
    await page.locator('[aria-haspopup="menu"][aria-expanded="true"]').count(),
    0, "no trigger is left claiming an open menu",
  );
  assert.equal(
    await renameInput.evaluate((el) => document.activeElement === el),
    true, "focus lands on the rename field, not <body>",
  );
  await renameInput.press("Escape");
  await renameInput.waitFor({ state: "detached" });
  console.log("ok: a full render dismisses the menu safely and hands focus to the rename field");

  // ---- the failed registry read, with the last visit's list cached ----
  // The successful reads above wrote the list to localStorage, so this
  // reload draws it: stale, dimmed, inert (comp Workspace Loading State,
  // 1b). The pane renders its own error state, says the rows on the left
  // are from the last visit, and offers a retry. Nothing on the stale rail
  // is reachable by pointer or keyboard, and no create affordance — the
  // pane's button, the sidebar's row, the Projects menu — renders until a
  // read succeeds.
  registryFails = true;
  await page.reload({ waitUntil: "networkidle" });
  await page.locator('[data-empty-pane="error"]').waitFor();
  assert.equal(await page.locator("[data-empty-title]").textContent(), "Can't reach the runtime");
  assert.ok((await page.locator("[data-empty-body]").textContent()).startsWith("The list on the left is from your last visit."),
    "the pane says whose rows those are");
  assert.equal(
    await page.locator("[data-empty-detail]").textContent(),
    "project registry unavailable",
    "the failure itself trails the reassurance",
  );
  assert.equal(await page.locator("[data-empty-action]").textContent(), "Retry now",
    "the pane offers a retry, never a create action, while the read fails");
  const staleList = page.locator("[data-project-list][data-list-stale]");
  await staleList.waitFor();
  assert.equal(await staleList.getAttribute("aria-busy"), "true", "the cached list says it is busy");
  assert.equal(await staleList.evaluate((el) => el.inert), true, "and is inert");
  assert.ok(await page.locator("[data-project-toggle]").count() > 0, "the last visit's project rows are still drawn");
  assert.ok(await page.locator("[data-list-syncing]").isVisible(), "the header says the rail is syncing");
  assert.equal(await page.locator("[data-projects-menu]").count(), 0,
    "no Projects menu over a list nobody confirmed");
  assert.equal(await page.locator("[data-new-project-row]").count(), 0,
    "the sidebar offers no New project row while the read fails");
  // Keyboard: every row control is out of the focus order, so the + on a
  // cached project cannot open a picker over an unconfirmed project.
  const staleControls = await page.locator("[data-project-list] button").count();
  assert.ok(staleControls > 0, "the cached rows still carry their controls, inert");
  const focusedInList = await page.locator("[data-project-list] button").first().evaluate((el) => {
    el.focus();
    return el.closest("[data-project-list]").contains(document.activeElement);
  });
  assert.equal(focusedInList, false, "a stale row control refuses focus");
  for (let i = 0; i < staleControls + 2; i++) await page.keyboard.press("Tab");
  assert.equal(await page.evaluate(() => document.activeElement?.closest("[data-project-list]") !== null && document.activeElement?.closest("[data-project-list]") !== undefined), false,
    "Tab never lands inside the stale list");
  await page.keyboard.press("Enter");
  assert.equal(await page.locator("[data-new-session-modal]").count(), 0,
    "Enter over the stale rail opens no picker");
  console.log("ok: a failed read with a cached list draws it stale and inert, with a retry and no create affordances");

  // ---- the failed registry read, nothing cached ----
  // Without a cache there is nothing to keep: no rows, no bands (bands are
  // for a read still in flight, not a failed one), and the same error
  // pane, now reassuring without naming a list.
  await page.evaluate(() => localStorage.removeItem("noisedeck.session-list"));
  await page.reload({ waitUntil: "networkidle" });
  await page.locator('[data-empty-pane="error"]').waitFor();
  assert.ok((await page.locator("[data-empty-body]").textContent()).startsWith("Your sessions are still on the server."),
    "the pane reassures without a list to name");
  assert.equal(await page.locator("[data-project-toggle]").count(), 0, "no rows without a cache");
  assert.equal(await page.locator("[data-loading-band]").count(), 0, "no bands over a failed read");
  assert.equal(await page.locator("[data-new-project-row]").count(), 0,
    "the sidebar offers no New project row while the read fails");
  assert.equal(await page.locator("[data-projects-menu]").count(), 0,
    "and no Projects menu over a list nobody has read");
  console.log("ok: a failed read with nothing cached renders the error state alone");

  // ---- the no-project rail (comp desktop-r16) ----
  // With no projects the Projects header and its menu are gone: the
  // sidebar collapses to a single + New project row, which opens the same
  // project form the menu's first row does. Focus never drops to
  // <body> — closing the form returns to the row.
  registryFails = false;
  emptyList = true;
  await page.reload({ waitUntil: "networkidle" });
  const newRow = page.locator("[data-new-project-row]");
  await newRow.waitFor();
  await page.locator('[data-empty-pane="no-project"]').waitFor();
  assert.equal(await page.locator("[data-empty-title]").textContent(), "No projects yet");
  assert.equal(await page.locator("[data-live-files]").count(), 0,
    "the live header sheds Files without a project");
  assert.equal(await page.locator("[data-live-tasks]").count(), 0,
    "the live header sheds Tasks without a project");
  assert.ok(await page.locator("[data-live-more]").count() > 0,
    "the account menu stays");
  assert.equal(await page.locator("[data-projects-menu]").count(), 0,
    "no Projects header renders without projects");
  await newRow.click();
  const projectForm = page.locator("[data-project-source-dialog]");
  await projectForm.waitFor();
  await projectForm.getByRole("button", { name: "Close" }).click();
  await projectForm.waitFor({ state: "detached" });
  assert.equal(
    await newRow.evaluate((el) => document.activeElement === el),
    true, "closing the form returns focus to the row, not <body>",
  );
  console.log("ok: the no-project rail is one New project row that returns focus");

  assert.deepEqual(pageErrors, [], "page errors");
  // Keyboard focus is the hover wash, never a ring. The
// capture path shows neither; a real keyboard open is owed the wash on
// the focused row — and never any outline or shadow.
{
  const forced = await context.newPage();
  await forced.goto(`${origin}/preview.html?menu=projects`, { waitUntil: "networkidle" });
  await forced.locator("[data-projects-menu-popover]").waitFor();
  const forcedShadow = await forced.evaluate(() =>
    getComputedStyle(document.querySelector("[data-projects-menu-popover] .menuitem")).boxShadow);
  assert.equal(forcedShadow, "none",
    `the captured Projects menu paints a focus ring the comp has no pixels for: ${forcedShadow}`);
  await forced.close();

  const keys = await context.newPage();
  await keys.goto(`${origin}/preview.html`, { waitUntil: "networkidle" });
  await keys.locator("[data-projects-menu]").focus();
  await keys.keyboard.press("Enter");
  await keys.locator("[data-projects-menu-popover]").waitFor();
  const keyFocus = await keys.evaluate(() => {
    const row = document.querySelector("[data-projects-menu-popover] .menuitem");
    const cs = getComputedStyle(row);
    return {
      shadow: cs.boxShadow,
      outline: cs.outlineStyle,
      matched: row.matches(":focus-visible"),
    };
  });
  assert.equal(keyFocus.shadow, "none", "a keyboard-opened menu paints a ring");
  assert.equal(keyFocus.outline, "none", "a keyboard-opened menu paints an outline");
  if (keyFocus.matched) {
    // The wash rides the row's transition, so frame zero still reads
    // transparent — wait for the settled value.
    await keys.waitForFunction(() => {
      const row = document.querySelector("[data-projects-menu-popover] .menuitem");
      return row !== null && getComputedStyle(row).backgroundColor !== "rgba(0, 0, 0, 0)";
    }, undefined, { timeout: 2000 }).catch(() => {
      throw new Error("a keyboard-opened Projects menu row lost its focus wash");
    });
  }
  await keys.close();
  console.log("projects menu: no ring anywhere, a keyboard open wears the wash");
}

console.log("projects menu: PASS");
  await context.close();
} catch (error) {
  console.error(error);
  process.exitCode = 1;
} finally {
  await browser?.close().catch(() => {});
  // SIGTERM, not SIGKILL: the child is a pnpm wrapper, and a signal it cannot
  // trap never reaches the vite process it started. The orphan keeps the
  // inherited stdout and stderr pipes open, so the probe passes every
  // assertion and then never exits — which a runner reports as a hang, the one
  // failure shape that names nothing.
  vite.kill("SIGTERM");
  vite.stdout.destroy();
  vite.stderr.destroy();
}
