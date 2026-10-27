// Real-browser probe for the app chords (§8.5): ⌘B, ⌘/, ⌥⌘Y and the
// contextual ⌘R, driven against the production composition with the same
// deterministic HTTP/WS mocks the live smoke uses. It also drives the
// project menu's Open preview entry flow: tab, severed opener,
// and the one-use ticket POSTed as a hidden form body. The unit suite pins
// the matcher and the markup; this proves the wiring — the listener, the
// target resolution, and the glyph/handler pairing — in the real DOM.
import assert from "node:assert/strict";
import { PNG } from "pngjs";
import { spawn } from "node:child_process";
import { existsSync } from "node:fs";
import net from "node:net";

if (process.env.PLAYWRIGHT_BROWSERS_PATH === undefined &&
    existsSync("/workspace/tools/ms-playwright")) {
  process.env.PLAYWRIGHT_BROWSERS_PATH = "/workspace/tools/ms-playwright";
}
const { browser: getBrowser } = await import("./browser.mjs");
const { exactlyOnce } = await import("./eventually.mjs");

const apple = process.env.PROBE_PLATFORM !== "Windows";
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

let browser;
let page;
const pageErrors = [];
try {
  await waitForServer(origin);
  let via;
  ({ browser, via } = await getBrowser());
  console.log(`browser: ${via}`);
  const context = await browser.newContext();
  await context.addInitScript((apple) => {
    Object.defineProperty(navigator, "userAgentData", { value: { platform: apple ? "macOS" : "Windows" } });
    Object.defineProperty(navigator, "platform", { value: apple ? "MacIntel" : "Win32" });
  }, apple);
  await context.addCookies([{
    name: "noisedeck_csrf",
    value: "probe-csrf",
    url: origin,
    sameSite: "Lax",
  }]);
  page = await context.newPage();
  page.on("pageerror", (error) => pageErrors.push(error.message));
  page.on("console", (message) => {
    // Mocked 404 answers (the "not a Tasks project" probe) surface as
    // resource-load noise; script errors are what must stay at zero.
    if (message.type() === "error" && !/^Failed to load resource/.test(message.text())) {
      pageErrors.push(message.text());
    }
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
    id: "identity-1", name: "Probe identity", email: "probe@example.com", revision: 1,
    created_at: "2026-08-01T00:00:00Z", updated_at: "2026-08-01T00:00:00Z",
  };
  let consoleSettings = {
    skip_permissions: false,
    skip_close_confirm: false,
    skip_remove_project_confirm: false,
  };
  const sessionRenames = [];
  const projectRenames = [];
  // The Open preview answers: the entry POST starts in the
  // unconfigured 404 shape and the preview section below flips it to a
  // served entry and back, so one route drives both outcomes.
  let previewAnswer = {
    status: 404,
    body: { error: { code: "preview_unavailable", message: "Preview is not configured for this project." } },
  };
  const previewEntryCalls = [];

  const summaries = [
    {
      id: "session-1",
      project_id: project.id,
      provider: "claude",
      display_name: "alpha session",
      status: "running",
      presentation_state: "running",
      generation: 1,
      created_at: "2026-08-01T00:00:00Z",
    },
    {
      id: "session-2",
      project_id: project.id,
      provider: "codex",
      display_name: "beta session",
      status: "running",
      presentation_state: "running",
      generation: 1,
      created_at: "2026-08-01T00:00:00Z",
    },
  ];
  const detail = (summary) => ({
    ...summary,
    pane_pid: 4242,
    tmux_pane_id: "%1",
    native_session_id: `native-${summary.id}`,
    updated_at: "2026-08-01T00:00:00Z",
    recovery_capability: "none",
    recovery_state: "unavailable",
  });

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
    if (request.method() === "GET" && path === "/api/harness-login") {
      // The login coordinator polls the listing while a controller is
      // attached; no login is waiting in this probe.
      return json(200, { pending: [], csrf: "login-csrf" });
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
    if (request.method() === "GET" && path === "/api/settings") return json(200, consoleSettings);
    if (request.method() === "PUT" && path === "/api/settings") {
      consoleSettings = request.postDataJSON();
      return json(200, consoleSettings);
    }
    if (request.method() === "GET" && path === "/api/git-identities") {
      return json(200, { profiles: [gitIdentity], default_profile_id: gitIdentity.id, revision: 1 });
    }
    if (request.method() === "GET" && path === `/api/projects/${project.id}/preview/settings`) {
      return json(200, { configured: true, self_service: false, enabled: true, source: "static", origin: "https://app.preview.example", port: 41207, upstream: "127.0.0.1:41207" });
    }
    if (request.method() === "GET" && path === "/api/projects") return json(200, [project]);
    if (request.method() === "GET" && path === `/api/projects/${project.id}/sessions`) {
      return json(200, summaries);
    }
    if (/^\/api\/projects\/[^/]+\/lanes$/.test(path)) return json(200, { lanes: [] });
    if (request.method() === "GET" && path === `/api/projects/${project.id}/taskmaster`) {
      return json(404, { error: "not_taskmaster_project", message: "not a Tasks project" });
    }
    if (request.method() === "GET" && path === `/api/projects/${project.id}/taskmaster/events`) {
      return route.fulfill({ status: 200, contentType: "text/event-stream", body: ": connected\n\nretry: 3600000\n\n" });
    }
    if (request.method() === "GET" && path.startsWith("/api/sessions/")) {
      const id = path.split("/").at(-1);
      const summary = summaries.find((s) => s.id === id);
      return summary === undefined ? json(404, { error: "not_found", message: "no such session" }) : json(200, detail(summary));
    }
    if (request.method() === "POST" && path === `/api/projects/${project.id}/preview`) {
      previewEntryCalls.push({ csrf: request.headers()["x-csrf-token"] ?? null });
      return json(previewAnswer.status, previewAnswer.body);
    }
    if (request.method() === "POST" && path === "/api/sessions/session-1/rename") {
      sessionRenames.push(request.postDataJSON());
      summaries[0].display_name = request.postDataJSON().name;
      return route.fulfill({ status: 204, body: "" });
    }
    if (request.method() === "POST" && path === `/api/projects/${project.id}/rename`) {
      projectRenames.push(request.postDataJSON());
      project.name = request.postDataJSON().name;
      return route.fulfill({ status: 204, body: "" });
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
  await page.routeWebSocket(`${origin.replace("http", "ws")}/api/sessions/*/terminal?*`, (socket) => {
    const url = new URL(socket.url());
    const session = url.pathname.split("/").at(-2);
    socket.send(JSON.stringify({ type: "attached", token: 7, generation: 3 }));
    socket.send(JSON.stringify({
      type: "output",
      data: Buffer.from(`OUTPUT ${session}\r\n`).toString("base64"),
    }));
  });

  // The preview gateway fixture: a context-level route so the
  // POPUP's form POST is intercepted too, not just the opener page's
  // requests. It records exactly what the open flow submitted and serves
  // a marker body the probe can wait for in the new tab.
  const gatewayPosts = [];
  await context.route("https://app.preview.example/**", async (route) => {
    const request = route.request();
    gatewayPosts.push({
      url: request.url(),
      method: request.method(),
      contentType: request.headers()["content-type"] ?? null,
      body: request.postData() ?? "",
    });
    await route.fulfill({
      status: 200,
      contentType: "text/html",
      body: '<!doctype html><html><body data-gateway-entry>app preview home</body></html>',
    });
  });

  await page.goto(origin, { waitUntil: "networkidle" });
  await page.locator('[data-session="session-1"]').waitFor();
  await page.locator('[data-session="session-1"]').click();
  await page.waitForFunction(
    () => document.querySelector("[data-live-terminal] .xterm-rows")?.textContent?.includes("OUTPUT session-1") === true,
  );
  // A page marker plus a late capture listener: the workspace installs its
  // chord listener first, so a later capture listener observes exactly
  // whether ours claimed the event (defaultPrevented) and the marker
  // survives any reload ⌘R would have caused.
  await page.evaluate(() => {
    window.__probeAlive = true;
    window.addEventListener("keydown", (event) => {
      window.__probeLast = { key: event.key, code: event.code, prevented: event.defaultPrevented };
    }, { capture: true });
  });

  // ⌘B — the rail.
  await page.keyboard.press("Meta+b");
  await page.locator('[data-workspace][data-collapsed="true"]').waitFor();
  await page.locator('[data-region="sidebar"]').waitFor({ state: "hidden" });
  await page.keyboard.press("Meta+b");
  await page.locator('[data-workspace][data-collapsed="false"]').waitFor();
  console.log("ok: ⌘B collapses and restores the side panel");

  // ⌘/ — the info bar.
  await page.keyboard.press("Meta+/");
  // The hidden state is display:none (the strip leaves the layout), so
  // the assertion is on the attribute, not visibility.
  await page.locator('[data-region="info-bar"][data-shown="false"]').waitFor({ state: "attached" });
  await page.keyboard.press("Meta+/");
  await page.locator('[data-region="info-bar"][data-shown="true"]').waitFor();
  console.log("ok: ⌘/ toggles the info bar both ways");

  // ⌥⌘Y — the menu opens into its inline confirmation, exactly like the
  // row does; granting writes the setting, and the chord turns it off.
  await page.keyboard.press("Alt+Meta+y");
  await page.locator("[data-more-popover]").waitFor();
  await page.getByRole("button", { name: "Turn it on" }).waitFor();
  const yoloRow = page.locator("[data-skip-permissions]");
  assert.ok((await toggleChecked(yoloRow)) === "false",
    "yolo mode starts off");
  await page.getByRole("button", { name: "Turn it on" }).click();
  await page.waitForFunction(() => {
    const row = document.querySelector("[data-more-popover] [data-skip-permissions]");
    return row?.getAttribute("aria-checked") === "true";
  });
  assert.ok(
    (await toggleChecked(page.locator("[data-more-popover] [data-skip-permissions]")))
      === "true",
    "granting the confirmation turns the toggle on",
  );
  await page.locator("[data-popover-backdrop]").click({ position: { x: 8, y: 8 } });
  await page.locator("[data-more-popover]").waitFor({ state: "detached" });
  // Revoking is immediate and needs no menu: the chord writes the
  // setting, and the next menu open reads the stored truth back.
  await page.keyboard.press("Alt+Meta+y");
  await page.locator("[data-live-more]").click();
  await page.locator("[data-more-popover]").waitFor();
  assert.ok(
    (await toggleChecked(page.locator("[data-skip-permissions]"))) === "false",
    "⌥⌘Y on an enabled mode turns it off immediately",
  );
  assert.equal(consoleSettings.skip_permissions, false);
  await page.locator("[data-popover-backdrop]").click({ position: { x: 8, y: 8 } });
  await page.locator("[data-more-popover]").waitFor({ state: "detached" });
  console.log("ok: ⌥⌘Y asks before granting, and revokes immediately");
  assert.equal(
    (await page.evaluate(() => window.__probeLast)).prevented, true,
    "the chord's event was claimed (the browser reload intercept)",
  );
  // ⌘R with a session selected: the selected session's row swaps for the
  // inline field, no dialog, and the page did not reload.
  const sessionSpanBox = await measureNameSpan(page, '[data-session="session-1"]', ":scope [data-session-swipe-surface] > .truncate");
  await page.keyboard.press("Meta+r");
  const sessionInput = page.locator('[data-session="session-1"] [data-rename-input]');
  await sessionInput.waitFor();
  assert.equal(await sessionInput.inputValue(), "alpha session");
  assert.equal(await page.getByRole("dialog").count(), 0, "rename is inline, never a dialog");
  assert.equal(await page.evaluate(() => window.__probeAlive === true), true, "⌘R did not reload");
  const sessionPlace = await assertRenameTextHeldPlace(sessionInput, sessionSpanBox, "session");
  await assertRenameChromeless(sessionInput);
  const sessionAccent = await assertAccentSelection(sessionInput, "session");
  // Commit here: with the sheet's row menu retired, this desktop chord is
  // the rename path, so the end-of-run duplicate tally rides this commit.
  await sessionInput.fill("renamed session");
  await sessionInput.press("Enter");
  await page.waitForFunction(() => document.querySelectorAll("[data-rename-input]").length === 0);
  await exactlyOnce([{ read: () => sessionRenames.length, expected: 1, message: "the session rename was sent once" }]);
  assert.deepEqual(sessionRenames, [{ name: "renamed session" }]);
  console.log(`ok: ⌘R renames the selected session inline (${sessionPlace}; ${sessionAccent})`);

  // Clicking a project row selects it; ⌘R then renames the project.
  await page.locator('[data-project-toggle="project-1"]').click();
  await page.locator('[data-project="project-1"] [data-selected="true"]').waitFor();
  const projectSpanBox = await measureNameSpan(page, "[data-project-name]", null);
  await page.keyboard.press("Meta+r");
  const projectInput = page.locator("[data-rename-project-input]");
  await projectInput.waitFor();
  assert.equal(await projectInput.inputValue(), "tester-project");
  const projectPlace = await assertRenameTextHeldPlace(projectInput, projectSpanBox, "project");
  await assertRenameChromeless(projectInput);
  const projectAccent = await assertAccentSelection(projectInput, "project");
  await projectInput.fill("renamed-project");
  await projectInput.press("Enter");
  await page.waitForFunction(() => document.querySelector("[data-project-name]")?.textContent === "renamed-project");
  // The row repaints from local state, so the rename request is still in
  // flight when the new name is on screen, and a duplicate would arrive after
  // it — the count is read again once the page has nothing outstanding.
  await exactlyOnce([{ read: () => projectRenames.length, expected: 1, message: "the project rename was sent once" }]);
  assert.deepEqual(projectRenames, [{ name: "renamed-project" }]);
  console.log(`ok: ⌘R after a project click renames the project inline (${projectPlace}; ${projectAccent})`);

  // Leaving a rename by clicking elsewhere must end it. If the field survives
  // the blur, the header keeps rendering an input where the chevron belongs
  // and the next click on the row expands nothing.
  await page.locator('[data-project-toggle="project-1"]').click();      // collapse
  const collapsedFirst = await page.locator('[data-project="project-1"] [data-project-toggle]')
    .getAttribute("aria-expanded");
  await page.keyboard.press("Meta+r");
  await page.locator("[data-rename-project-input]").waitFor();
  await page.locator('[data-region="terminal"]').click({ position: { x: 40, y: 40 } });
  await page.locator("[data-rename-project-input]").waitFor({ state: "detached" });
  await page.locator('[data-project-toggle="project-1"]').click();
  await page.waitForFunction(
    (was) => document.querySelector('[data-project="project-1"] [data-project-toggle]')
      ?.getAttribute("aria-expanded") !== was,
    collapsedFirst,
  );
  console.log("ok: clicking away ends the rename, and the chevron works again");



  // An open row menu is the explicit target: the project ⋯ wins even
  // though the session selection still stands, and the menu closes.
  // The project click above collapsed the (default-expanded) project;
  // re-expand so the session row is reachable again.
  await page.locator('[data-project-toggle="project-1"]').click();
  await page.locator('[data-session="session-1"]').click();
  // The row menu rests hidden until the row is hovered; the hit test alone
  // never moves the pointer, so the probe performs the user's hover.
  await page.locator('[data-project-menu="project-1"]')
    .locator("xpath=ancestor::li[1]").locator("[data-project-toggle]").hover();
  await page.locator('[data-project-menu="project-1"]').click();
  await page.locator('[data-project-menu-popover]').waitFor();
  const glyph = await page.locator('[data-project-menu-popover] [data-project-menu-rename]').innerText();
  assert.ok(glyph.includes(apple ? "⌘" : "Ctrl+") && glyph.trim().endsWith("R"), `the Rename row prints ⌘R (got ${JSON.stringify(glyph)})`);
  await page.keyboard.press("Meta+r");
  await page.locator("[data-rename-project-input]").waitFor();
  await page.locator('[data-project-menu-popover]').waitFor({ state: "detached" });
  await page.keyboard.press("Escape");
  console.log("ok: ⌘R renames the open menu's project and closes the menu");

  // The project menu's Open preview row runs the real entry
  // flow in the real browser: the blank tab must open synchronously
  // inside the click, its opener severed, the ticket POSTed as a hidden
  // form body to the exact HTTPS URL the backend returned — and nothing
  // in any URL. The gateway origin is the fixture route above.
  await page.locator('[data-project-menu="project-1"]')
    .locator("xpath=ancestor::li[1]").locator("[data-project-toggle]").hover();
  await page.locator('[data-project-menu="project-1"]').click();
  await page.locator("[data-project-menu-popover]").waitFor();
  const previewRow = page.locator("[data-project-menu-popover] [data-project-menu-preview]");
  await previewRow.waitFor();
  assert.equal(await previewRow.getAttribute("aria-label"), "Open preview");
  assert.equal((await previewRow.locator("[data-project-preview-menu-status]").textContent()).trim(), "Not checked yet");
  assert.equal(await previewRow.getAttribute("data-hint"), "Open this project's app preview in a new tab");
  previewAnswer = { status: 200, body: { url: "https://app.preview.example/entry", ticket: "one-use-ticket" } };
  const previewTabPromise = page.waitForEvent("popup");
  await previewRow.click();
  const previewTab = await previewTabPromise;
  await previewTab.waitForSelector("[data-gateway-entry]");
  assert.equal(previewEntryCalls.length, 1, "one entry request");
  assert.equal(previewEntryCalls[0].csrf, "probe-csrf", "the entry POST carries the CSRF cookie's token");
  assert.equal(gatewayPosts.length, 1, "one gateway submission");
  assert.equal(gatewayPosts[0].method, "POST");
  assert.equal(gatewayPosts[0].contentType, "application/x-www-form-urlencoded");
  assert.equal(gatewayPosts[0].body, "ticket=one-use-ticket", "the ticket rides the form body");
  assert.equal(new URL(gatewayPosts[0].url).search, "", "the ticket never rides a query string");
  assert.equal(new URL(gatewayPosts[0].url).hash, "");
  assert.equal(await previewTab.evaluate(() => window.opener), null, "the preview tab holds no opener authority");
  await previewTab.close();
  console.log("ok: Open preview submits the ticket as a hidden POST body, opener severed");

  // A registration revoked after the menu read answers 404: the blank tab
  // closes and the sidebar reports the server's current refusal.
  previewAnswer = {
    status: 404,
    body: { error: { code: "preview_unavailable", message: "Preview is not configured for this project." } },
  };
  const blankTabPromise = page.waitForEvent("popup");
  await page.locator('[data-project-menu="project-1"]')
    .locator("xpath=ancestor::li[1]").locator("[data-project-toggle]").hover();
  await page.locator('[data-project-menu="project-1"]').click();
  await page.locator("[data-project-menu-preview]").click();
  const blankTab = await blankTabPromise;
  await page.getByText("Preview is not configured for this project.", { exact: true }).waitFor();
  assert.equal(await blankTab.isClosed(), true, "the refused entry closes its blank tab");
  assert.equal(gatewayPosts.length, 1, "a refused entry never reaches the gateway");
  console.log("ok: a revoked preview closes the blank tab and names the refusal");

  // The sheet has no per-row session menus (comp mobile-r8 screen 02):
  // swipe is the row's one action surface, so the trigger must stay
  // hidden at the narrow layout instead of opening a menu.
  await page.setViewportSize({ width: 600, height: 800 });
  await expandSidebar(page);
  assert.equal(await page.locator('[data-session-menu="session-1"]').isVisible(), false,
    "the narrow layout still exposes a per-row session menu");
  await page.setViewportSize({ width: 1280, height: 800 });
  await expandSidebar(page);
  console.log("ok: the narrow layout retires the session row menu for the swipe");

  if (!apple) {
    await page.locator("[data-live-more]").focus();
    await page.keyboard.press("Control+b");
    await page.locator('[data-workspace][data-collapsed="true"]').waitFor();
    await page.keyboard.press("Control+b");
    await page.locator('[data-workspace][data-collapsed="false"]').waitFor();
    await page.locator("[data-live-terminal] .xterm-helper-textarea").focus();
    await page.keyboard.press("Control+b");
    assert.equal(await page.locator('[data-workspace]').getAttribute("data-collapsed"), "false",
      "Ctrl+B remains the CLI's when the terminal has focus");
  }

  // The chords work while the terminal owns the keyboard, where the
  // user actually is.
  await page.locator("[data-live-terminal] .xterm-helper-textarea").focus();
  await page.keyboard.press("Meta+b");
  await page.locator('[data-workspace][data-collapsed="true"]').waitFor();
  await page.keyboard.press("Meta+b");
  await page.locator('[data-workspace][data-collapsed="false"]').waitFor();
  console.log("ok: the chords fire with the terminal focused");

  // The More menu prints what it wires: three glyphs, and never the
  // browser-reserved ones.
  await page.locator("[data-live-more]").click();
  await page.locator("[data-more-popover]").waitFor();
  // textContent, not innerText: the glyph cluster is one span per glyph,
  // and rendered line-box spacing would corrupt the pairing under test.
  const menuText = await page.locator("[data-more-popover]").textContent();
  for (const forbidden of ["⌘N", "⌘T", "⌘W"]) {
    assert.ok(!menuText.includes(forbidden), `${forbidden} must not print`);
  }
  for (const wired of (apple ? ["⌘B", "⌘/", "⌥⌘Y"] : ["Ctrl+B", "Ctrl+/", "Ctrl+Alt+Y"])) {
    assert.ok(menuText.includes(wired), `the menu prints ${wired}`);
  }
  if (!apple) assert.doesNotMatch(menuText, /[⌘⌥]/);
  const sidePanel = page.locator("[data-show-side-panel]");
  assert.ok((await toggleChecked(sidePanel)) === "true",
    "Show side panel starts on");
  await sidePanel.click();
  await page.locator('[data-workspace][data-collapsed="true"]').waitFor();
  assert.ok(
    (await toggleChecked(page.locator("[data-more-popover] [data-show-side-panel]")))
      === "false",
    "the toggle re-renders after its own chord's target moved",
  );
  await page.keyboard.press("Meta+b");
  await page.locator('[data-workspace][data-collapsed="false"]').waitFor();
  console.log("ok: the side-panel row is wired and honest in both directions");

  // A duplicate rename scheduled long after its commit lands during a later
  // step, past the window its own assertion held open, so both counts are
  // watched once more at the end of the run.
  await exactlyOnce([
    { read: () => projectRenames.length, expected: 1, message: "the project rename was sent once" },
    { read: () => sessionRenames.length, expected: 1, message: "the session rename was sent once" },
  ]);

  assert.deepEqual(pageErrors, [], `no page errors (got ${JSON.stringify(pageErrors)})`);
  console.log("shortcuts probe: PASS");
} catch (error) {
  console.error(`shortcuts probe: FAIL\n${error?.stack ?? error}`);
  if (serverOutput !== "") console.error(`vite output:\n${serverOutput}`);
  process.exitCode = 1;
} finally {
  await browser?.close().catch(() => {});
  vite.kill("SIGTERM");
}

function freePort() {
  return new Promise((resolve, reject) => {
    const server = net.createServer();
    server.unref();
    server.on("error", reject);
    server.listen(0, "127.0.0.1", () => {
      const { port: bound } = server.address();
      server.close(() => resolve(bound));
    });
  });
}

function waitForServer(url) {
  const started = Date.now();
  const attempt = (resolve, reject) => {
    net.connect({ host: "127.0.0.1", port: new URL(url).port }, () => {
      // The listener answers before vite finishes wiring; one beat of
      // slack avoids a connection racing the dev server's accept loop.
      setTimeout(resolve, 300);
    }).on("error", () => {
      if (Date.now() - started > 20_000) reject(new Error("vite dev server never came up"));
      else setTimeout(() => attempt(resolve, reject), 200);
    });
  };

  return new Promise(attempt);
}

async function toggleChecked(row) {
  assert.equal(await row.getAttribute("role"), "switch");
  return await row.getAttribute("aria-checked");
}

async function expandSidebar(page) {
  if (await page.locator("[data-workspace]").getAttribute("data-collapsed") !== "true") return;
  const expand = page.getByRole("button", { name: "Show sidebar" });
  await expand.waitFor();
  await expand.click();
  await page.locator('[data-workspace][data-collapsed="false"]').waitFor();
}

// A chromeless rename field owns no box (comp desktop-r15): the row wash
// plus the accent selection is the whole affordance, so a border, a fill
// or an inset on the field is chrome the design does not carry.
async function assertRenameChromeless(input) {
  const chrome = await input.evaluate((el) => {
    const cs = getComputedStyle(el);
    return {
      borderTop: cs.borderTopWidth,
      borderLeft: cs.borderLeftWidth,
      background: cs.backgroundColor,
      paddingLeft: cs.paddingLeft,
      paddingRight: cs.paddingRight,
      outlineStyle: cs.outlineStyle,
      color: cs.color,
      // The token is resolved in the live document so the expectation is
      // whatever the theme currently paints, not a hard-coded channel triple.
      primary: (() => {
        const probe = document.createElement("span");
        probe.style.color = "var(--color-text-primary)";
        document.body.appendChild(probe);
        const resolved = getComputedStyle(probe).color;
        probe.remove();
        return resolved;
      })(),
      boxShadow: cs.boxShadow,
      borderRadius: cs.borderTopLeftRadius,
    };
  });
  assert.equal(chrome.borderLeft, "0px", "the rename field has no left border");
  assert.equal(chrome.borderTop, "0px", "the rename field has no top border");
  assert.equal(chrome.background, "rgba(0, 0, 0, 0)", "the rename field has no background fill");
  assert.equal(chrome.paddingLeft, "0px", "the rename field has no left inset");
  assert.equal(chrome.paddingRight, "0px", "the rename field has no right inset");
  // The field is measured while focused, which is the only state it exists in
  // and the state a focus ring would paint.
  // Style, not width: a suppressed outline keeps a computed width, so width
  // alone would pass on a field that paints a ring.
  assert.equal(chrome.outlineStyle, "none", "the focused rename field paints no outline");
  assert.equal(chrome.color, chrome.primary, "the rename field paints primary text");
  assert.equal(chrome.boxShadow, "none", "the rename field carries no shadow");
  assert.equal(chrome.borderRadius, "0px", "the rename field carries no corner radius");
}

// The text must not move when the field replaces the span. Measured, not
// eyeballed: the span's Range rect is the rendered font box (its top sits
// one font ascent above the baseline), and the input centers that same
// font box in its content box, so the ascent cancels out of the comparison
// and both sides reduce to rect arithmetic.
async function assertRenameTextHeldPlace(input, spanBox, kind) {
  const fieldBox = await input.evaluate((el) => {
    const cs = getComputedStyle(el);
    const box = el.getBoundingClientRect();
    return {
      left: box.left + parseFloat(cs.paddingLeft) + parseFloat(cs.borderLeftWidth),
      top: box.top + parseFloat(cs.paddingTop) + parseFloat(cs.borderTopWidth),
      height: box.height - parseFloat(cs.paddingTop) - parseFloat(cs.paddingBottom)
        - parseFloat(cs.borderTopWidth) - parseFloat(cs.borderBottomWidth),
      font: `${cs.fontStyle} ${cs.fontWeight} ${cs.fontSize} ${cs.letterSpacing} ${cs.fontFamily}`,
    };
  });
  assert.equal(fieldBox.font, spanBox.font, `the ${kind} field renders the span's own font`);
  const dx = fieldBox.left - spanBox.left;
  const dy = (fieldBox.top - spanBox.top) + (fieldBox.height - spanBox.height) / 2;
  assert.ok(Math.abs(dx) <= 1, `the ${kind} text's left edge did not move (moved ${dx.toFixed(2)}px)`);
  assert.ok(Math.abs(dy) <= 1, `the ${kind} text's baseline did not move (moved ${dy.toFixed(2)}px)`);
  return `text held: left Δ${dx.toFixed(2)}px, baseline Δ${dy.toFixed(2)}px`;
}

// measureNameSpan returns the resting name's rendered font box — a Range
// over its text node, which is the box the selection highlight would
// paint — plus the computed font, so the field that replaces it can be
// held to the same face.
async function measureNameSpan(page, rootSelector, spanSelector) {
  const box = await page.evaluate((args) => {
    const root = document.querySelector(args.root);
    const el = args.span === null ? root : root?.querySelector(args.span);
    if (el === null || el === undefined) return null;
    const range = document.createRange();
    range.selectNodeContents(el);
    const rect = range.getClientRects()[0];
    if (rect === undefined) return null;
    const cs = getComputedStyle(el);
    return {
      left: rect.left,
      top: rect.top,
      height: rect.height,
      font: `${cs.fontStyle} ${cs.fontWeight} ${cs.fontSize} ${cs.letterSpacing} ${cs.fontFamily}`,
    };
  }, { root: rootSelector, span: spanSelector });
  assert.ok(box !== null, `the resting name is present for ${rootSelector}`);
  return box;
}

// The auto-selected text is the editing cue: with the field chromeless,
// the accent ::selection is what says "editing". Pixel truth, not
// stylesheet presence — the field's own screenshot must carry saturated
// accent selection. The bands are strict because the fallback is blue
// too: the page-wide tint is a 30% accent wash, and blended over the ink
// row it lands in the midtones (b-r around 70) where full accent sits
// at b-r above 140.
async function assertAccentSelection(input, kind) {
  const png = PNG.sync.read(await input.screenshot());
  let accent = 0;
  for (let i = 0; i < png.data.length; i += 4) {
    const r = png.data[i];
    const g = png.data[i + 1];
    const b = png.data[i + 2];
    if (b >= 150 && b - r >= 120 && b - g >= 80) accent += 1;
  }
  assert.ok(accent >= 300, `the ${kind} field paints an accent selection (${accent} accent px)`);
  return `${accent} accent px`;
}
