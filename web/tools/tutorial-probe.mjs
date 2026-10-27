// Tutorial persistence and docking through the production workspace.
import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import net from "node:net";

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

let browser;
try {
  await waitForServer(origin);
  ({ browser } = await getBrowser());
  const context = await browser.newContext({ viewport: { width: 1280, height: 800 } });
  await context.addCookies([{ name: "noisedeck_csrf", value: "probe", url: origin }]);
  const page = await context.newPage();
  const errors = [];
  page.on("pageerror", (error) => { errors.push(error.message); console.error("page error:", error.message); });
  let record = null;
  let failWrites = false;
  let hasSession = false;
  let holdRead = false;
  let holdProjects = false;
  let holdSessionDetail = false;
  let sessionDetailReads = 0;
  let releaseSessionDetail;
  let releaseProjects;
  let lastSessionID = null;
  let releaseRead;
  let holdWrite = false;
  let releaseWrite;
  let writeStarted;
  let terminalSocket;
  let identityReady = false;
  let identityWrites = 0;
  const identity = { id: "identity-1", name: "Probe identity", email: "probe@example.invalid", revision: 1, created_at: "t", updated_at: "t" };
  const writes = [];
  const project = { id: "p1", name: "Existing project", canonical_path: "/workspace/probe", last_used_at: "t", git_identity_setup_revision: 1 };
  const session = { id: "s1", project_id: "p1", provider: "claude", display_name: "Probe session", status: "running", presentation_state: "running", generation: 1, created_at: "2026-08-01T00:00:00Z", updated_at: "2026-08-01T00:00:00Z", recovery_capability: "none", recovery_state: "unavailable", tmux_pane_id: "%1", pane_pid: 4242, native_session_id: "probe-native" };
  const secondProject = { ...project, id: "p2", name: "Another project", canonical_path: "/workspace/another" };
  const secondSession = { ...session, id: "s2", project_id: "p2", display_name: "Another session", native_session_id: "probe-native-2" };
  await page.routeWebSocket(`${origin.replace("http", "ws")}/api/sessions/*/terminal?*`, (socket) => {
    terminalSocket = socket;
    socket.send(JSON.stringify({ type: "attached", token: 7, generation: 1 }));
    socket.send(JSON.stringify({ type: "output", data: Buffer.from(`Tutorial probe ${new URL(socket.url()).pathname}\r\n`).toString("base64") }));
  });
  await page.route(`${origin}/auth/session`, (route) => route.fulfill({ status: 204, headers: { "X-Noisedeck-Auth-Mode": "builtin" } }));
  await page.route(`${origin}/auth/logout`, (route) => route.fulfill({ status: 204 }));
  await page.route(`${origin}/api/**`, async (route) => {
    const request = route.request();
    const path = new URL(request.url()).pathname;
    const json = (status, body) => route.fulfill({ status, contentType: "application/json", body: JSON.stringify(body) });
    if (path === "/api/settings/tutorial") {
      if (request.method() === "GET") {
        if (holdRead) await new Promise((resolve) => { releaseRead = resolve; });
        return json(record ? 200 : 404, record ?? { code: "not_found" });
      }
      assert.equal(request.method(), "PUT");
      assert.equal(request.headers()["x-csrf-token"], "probe");
      writes.push(request.postDataJSON());
      if (holdWrite) await new Promise((resolve) => { releaseWrite = resolve; writeStarted?.(); });
      if (failWrites) return json(503, { code: "unavailable", message: "Preference write unavailable" });
      record = { ...request.postDataJSON(), scope: "user" };
      return json(200, record);
    }
    if (path === "/api/git-identities" && request.method() === "POST") {
      identityWrites++;
      assert.deepEqual(request.postDataJSON(), { name: identity.name, email: identity.email });
      identityReady = true;
      return json(201, identity);
    }
    if (path === "/api/settings/layout" && request.method() === "PUT") return json(200, request.postDataJSON());
    // The project/session switch lesson clicks rows, and a row click is the
    // user choosing a session: that choice is remembered on the server.
    if (path === "/api/settings/last-session" && request.method() === "PUT") return json(200, request.postDataJSON());
    assert.equal(request.method(), "GET", `tutorial caused unexpected mutation ${path}`);
    if (path === "/api/settings") return json(200, { skip_permissions: false, skip_close_confirm: false, skip_remove_project_confirm: false });
    if (path === "/api/settings/layout") return json(404, { code: "not_found" });
    if (path === "/api/settings/last-session") {
      return lastSessionID ? json(200, { session_id: lastSessionID }) : json(404, { code: "not_found", message: "no last session remembered" });
    }
    if (path === "/api/projects") {
      if (holdProjects) await new Promise((resolve) => { releaseProjects = resolve; });
      return json(200, [project, secondProject]);
    }
    if (path.endsWith("/sessions")) return json(200, hasSession ? [path.includes("/p2/") ? secondSession : session] : []);
    if (path === "/api/sessions/s1") {
      if (holdSessionDetail && ++sessionDetailReads > 1) await new Promise((resolve) => { releaseSessionDetail = resolve; });
      return json(200, session);
    }
    if (path === "/api/sessions/s2") return json(200, secondSession);
    if (path.endsWith("/chats") && path.startsWith("/api/sessions/")) return json(200, { agent: "probe", chats: [] });
    if (path === "/api/chats/status") return json(200, { available: true });
    if (path === "/api/github/connect/status") return json(200, { outcome: "authenticated", login: "probe" });
    if (path.endsWith("/preview/settings")) return json(200, { configured: true, self_service: true, enabled: false });
    if (path === "/api/providers") return json(200, { providers: [] });
    if (path === "/api/harness-usage") return json(200, { global: [], project: [] });
    if (path === "/api/harness-login" && request.method() === "GET") return json(200, { pending: [] });
    if (path === "/api/github/account") return json(200, { outcome: "authentication-required", account: "" });
    if (path === "/api/git-identities") return json(200, { profiles: identityReady ? [identity] : [], default_profile_id: identityReady ? identity.id : null, revision: 1 });
    if (path.endsWith("/events")) return route.fulfill({ status: 200, contentType: "text/event-stream", body: ": connected\n\nretry: 3600000\n\n" });
    if (path.endsWith("/files")) return json(200, { path: "", at_root: true, entries: [] });
    if (/^\/api\/projects\/[^/]+\/lanes$/.test(path)) return json(200, { lanes: [] });
    if (path.includes("/taskmaster")) return json(404, { code: "not_taskmaster_project", message: "Not a Tasks project" });
    if (path === "/api/chats") return json(200, { chats: [] });
    // The closed Assets panel reads the listing and follows its feed for its badge.
    if (/^\/api\/projects\/[^/]+\/assets$/.test(path)) return route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ locations: [], scanned: "2026-01-01T00:00:00Z", preview_max_bytes: 16777216 }) });
    if (/^\/api\/projects\/[^/]+\/assets\/events$/.test(path)) return route.fulfill({ status: 200, contentType: "text/event-stream", body: ": connected\n\nretry: 3600000\n\n" });
    throw new Error(`unhandled ${path}`);
  });
  const panel = page.locator('[data-region="tutorial"]');
  const open = () => panel.getAttribute("data-open");
  const clickSave = (button, keyboard = false) => Promise.all([
    page.waitForResponse((r) => r.url().endsWith("/api/settings/tutorial") && r.request().method() === "PUT"),
    keyboard ? button.press("Enter") : button.click(),
  ]);
  const show = async () => {
    await page.locator("[data-live-more]").click();
    await clickSave(page.getByRole("button", { name: "Show tutorial", exact: true }));
    await panel.waitFor({ state: "visible" });
    const resume = panel.getByRole("button", { name: "Resume", exact: true });
    if (await resume.isVisible()) await resume.click();
  };
  const resumeHeading = panel.getByRole("heading", { name: "Pick up where you left off", exact: true });
  await page.goto(origin, { waitUntil: "networkidle" });
  assert.equal(await page.locator('[role="dialog"][aria-modal="true"]').count(), 0, "no Git identity gate on first use");
  assert.equal(writes.length, 0, "tutorial does not write before the first-user setup gesture");
  await panel.waitFor({ state: "visible" });
  if (process.env.TUTORIAL_CAPTURE) await page.screenshot({ path: `/workspace/deck/build/tutorial-${process.env.TUTORIAL_CAPTURE}-desktop.png` });
  assert.equal(await open(), "true", "first use opens even with existing projects");
  assert.equal(await resumeHeading.count(), 0, "first use shows no resume page");
  const welcomeHeading = panel.getByRole("heading", { name: "Welcome to Deck", exact: true });
  await welcomeHeading.waitFor();
  assert.equal(await panel.locator(".tutorial-footer").count(), 0, "the welcome page has no Back and Next");
  const startButton = panel.getByRole("button", { name: "Start the tutorial", exact: true });
  const exploreButton = panel.getByRole("button", { name: "Explore on my own", exact: true });
  for (const button of [startButton, exploreButton]) assert.ok(await button.getAttribute("data-hint"), "welcome buttons explain themselves in the info bar");
  assert.equal(Math.round((await panel.boundingBox()).width), 360);
  assert.equal(await panel.evaluate((el) => el.contains(document.activeElement)), false, "automatic opening leaves focus alone");
  assert.ok((await panel.ariaSnapshot()).includes('heading "Tutorial"'));
  await clickSave(exploreButton);
  await page.waitForFunction(() => document.querySelector('[data-region="tutorial"]').getAttribute("data-open") === "false");
  assert.equal(record.dismissed, true, "Explore on my own closes the tutorial");
  assert.deepEqual(record.done, [], "Explore on my own does not count as starting");
  await page.locator("[data-live-more]").click();
  await clickSave(page.getByRole("button", { name: "Show tutorial", exact: true }));
  await welcomeHeading.waitFor();
  assert.equal(await startButton.evaluate((el) => el === document.activeElement), true, "Show tutorial focuses Start the tutorial");
  await clickSave(startButton);
  assert.deepEqual(record.done, ["welcomed"], "Start the tutorial is saved");
  assert.ok((await panel.innerText()).includes("Step 1 of"), "Start the tutorial opens the first step");
  await clickSave(panel.locator('[data-tutorial-action="next"], [data-tutorial-action="skip"]'), true);
  await clickSave(panel.getByRole("button", { name: "Back", exact: true }), true);
  // A save repaint replaces the buttons; query focus within the stable panel.
  assert.equal(await panel.evaluate((el) => el.querySelector('[data-tutorial-action="next"], [data-tutorial-action="skip"]') === document.activeElement), true,
    "Back at the first step restores keyboard focus to the forward button");
  await page.keyboard.press("Enter");
  await page.waitForFunction(() => document.querySelector(".tutorial-lesson")?.textContent?.includes("Step 2"));
  // With no identity the create step offers to set one; saving it clears the offer.
  const identityFallback = panel.getByRole("button", { name: "Set a Git identity…", exact: true });
  await identityFallback.click();
  const setup = page.getByRole("dialog", { name: "Git identity", exact: true });
  await setup.waitFor();
  await setup.getByRole("textbox", { name: "Git author name", exact: true }).fill(identity.name);
  await setup.getByRole("textbox", { name: "Git author email", exact: true }).fill(identity.email);
  await setup.getByRole("button", { name: "Save identity", exact: true }).click();
  await setup.waitFor({ state: "hidden" });
  assert.equal(identityWrites, 1);
  await identityFallback.waitFor({ state: "detached" });
  assert.ok((await panel.innerText()).includes("Step 2 of"), "saving the identity keeps the lesson where it was");
  await clickSave(panel.getByRole("button", { name: "Close tutorial", exact: true }));
  await page.waitForFunction(() => document.querySelector('[data-region="tutorial"]').getAttribute("data-open") === "false");
  assert.equal(await page.locator("[data-live-more]").evaluate((el) => el === document.activeElement), true, "Close restores focus to the global menu trigger");
  await page.reload({ waitUntil: "networkidle" });
  assert.equal(await open(), "false", "Close persists across reload");
  await page.locator("[data-live-more]").click();
  await clickSave(page.getByRole("button", { name: "Show tutorial", exact: true }));
  await panel.waitFor({ state: "visible" });
  await resumeHeading.waitFor();
  const resumeLine = await panel.locator(".tutorial-body p").first().innerText();
  assert.match(resumeLine, /^.+ · step 2 of \d+ · .+$/, "the resume page names the lesson, the step and its title");
  assert.equal(await panel.locator(".tutorial-footer").count(), 0, "the resume page has no Back and Next");
  assert.equal(await panel.locator(".tutorial-progress").count(), 0, "the resume page is not the step page");
  if (process.env.TUTORIAL_CAPTURE) await page.screenshot({ path: `/workspace/deck/build/843-shots/resume-${process.env.TUTORIAL_CAPTURE}.png`, clip: { x: 0, y: 0, width: 1280, height: 800 } });
  const writesBeforeResume = writes.length;
  const resumeButton = panel.getByRole("button", { name: "Resume", exact: true });
  assert.equal(await resumeButton.evaluate((el) => el === document.activeElement), true, "Show tutorial focuses Resume");
  await resumeButton.click();
  assert.equal(await resumeHeading.count(), 0, "Resume leaves the resume page");
  assert.ok((await panel.innerText()).includes("Step 2 of"), "Resume lands on the saved step");
  assert.equal(await panel.locator(".tutorial-footer").count(), 1);
  assert.equal(writes.length, writesBeforeResume, "the resume page and Resume write nothing");
  assert.equal(record.step, 1, "Resume keeps the saved step");
  await clickSave(panel.getByRole("button", { name: "Close tutorial", exact: true }));
  await page.locator("[data-live-more]").click();
  await clickSave(page.getByRole("button", { name: "Show tutorial", exact: true }));
  await resumeHeading.waitFor();
  const writesBeforeCatalog = writes.length;
  await panel.locator(".tutorial-body").getByRole("button", { name: "All lessons", exact: true }).click();
  const catalogNav = panel.getByRole("navigation", { name: "Tutorial lessons" });
  await catalogNav.waitFor();
  assert.equal(await resumeHeading.count(), 0, "All lessons leaves the resume page");
  assert.equal(await catalogNav.locator('[aria-current="step"]').count(), 1, "the catalog marks the current lesson");
  assert.equal(writes.length, writesBeforeCatalog, "opening the catalog from the resume page writes nothing");
  await panel.locator(".tutorial-header").getByRole("button", { name: "All lessons", exact: true }).click();
  assert.ok((await panel.innerText()).includes("Step 2 of"), "closing the catalog returns to the saved step, not the resume page");
  await panel.getByRole("button", { name: "All lessons", exact: true }).click();
  await catalogNav.waitFor();
  await clickSave(panel.getByRole("button", { name: "Close tutorial", exact: true }));
  await page.locator("[data-live-more]").click();
  await clickSave(page.getByRole("button", { name: "Show tutorial", exact: true }));
  await resumeHeading.waitFor();
  assert.equal(await catalogNav.count(), 0, "a catalog left open before Close does not hide the resume page");
  await panel.getByRole("button", { name: "Resume", exact: true }).click();
  assert.equal(record.step, 1, "manual reopen restores the saved step");
  const writesBeforePanels = writes.length;
  const bar = page.locator('[data-region="tutorial-bar"]');
  const showStep = bar.getByRole("button", { name: "Show step", exact: true });
  for (const name of ["files", "tasks"]) {
    await page.locator(`[data-live-${name}]`).click();
    const host = page.locator(`[data-region="${name}"][data-open="true"]`);
    await host.waitFor();
    assert.equal(await open(), "false", `opening ${name} folds the tutorial page`);
    await bar.waitFor({ state: "visible" });
    const total = /^Tutorial, step 2 of (\d+)$/.exec(await bar.getAttribute("aria-label"))?.[1];
    assert.ok(total, "the bar is a region named for the step");
    assert.equal(await bar.getAttribute("role"), "region");
    const lessonTitle = await bar.locator(".tutorial-bar-title").innerText();
    assert.equal(await bar.locator(".tutorial-bar-count").innerText(), `2/${total}`);
    assert.ok(lessonTitle.length > 0, "the bar names the step");
    assert.equal(await showStep.getAttribute("aria-expanded"), "false");
    assert.equal(await host.locator("[data-tutorial-action]").count(), 0, `${name} itself shows no tutorial controls`);
    const geometry = await page.evaluate((region) => {
      const barBox = document.querySelector('[data-region="tutorial-bar"]').getBoundingClientRect();
      const panelBox = document.querySelector(`[data-region="${region}"]`).getBoundingClientRect();
      const titlebar = document.querySelector("[data-workspace-titlebar]").getBoundingClientRect();
      const terminal = document.querySelector('[data-region="terminal"]').getBoundingClientRect();
      return { barLeft: barBox.left, barRight: barBox.right, barBottom: barBox.bottom, panelLeft: panelBox.left, panelRight: panelBox.right, titlebarBottom: titlebar.bottom, terminalRight: terminal.right };
    }, name);
    assert.ok(Math.abs(geometry.barLeft - geometry.panelLeft) <= 1 && Math.abs(geometry.barRight - geometry.panelRight) <= 1, `${name}: the bar spans the dock, not a second column`);
    assert.ok(Math.abs(geometry.barBottom - geometry.titlebarBottom) <= 0.5, `${name}: the bar is the dock's chrome row`);
    assert.ok(geometry.terminalRight <= geometry.panelLeft + 1, `${name}: the terminal keeps off the dock`);
    const terminalWidth = (await page.locator('[data-region="terminal"]').boundingBox()).width;
    if (process.env.TUTORIAL_CAPTURE) await page.screenshot({ path: `/workspace/deck/build/840-shots/${process.env.TUTORIAL_CAPTURE}-${name}-folded.png` });
    await showStep.click();
    assert.equal(await open(), "true", "Show step unfolds the page");
    assert.equal(await showStep.getAttribute("aria-expanded"), "true");
    assert.equal(await panel.getAttribute("data-over"), "true");
    assert.equal(await page.locator('[data-region="tutorial"] .tutorial-body h3').evaluate((el) => el === document.activeElement), true, "Show step moves focus to the step");
    assert.equal(Math.round((await page.locator('[data-region="terminal"]').boundingBox()).width), Math.round(terminalWidth), "the unfolded page takes no terminal width");
    assert.ok((await panel.boundingBox()).y >= geometry.barBottom - 0.5, "the page sits under the bar");
    await page.setViewportSize({ width: 880, height: 800 });
    const overflowing = await page.evaluate(() => [...document.querySelectorAll('[data-region="tutorial"] .tutorial-header button')]
      .filter((el) => el.getBoundingClientRect().width > 0)
      .map((el) => ({ label: el.getAttribute("aria-label") ?? el.textContent.trim(), box: el.getBoundingClientRect() }))
      .filter(({ box }) => box.left < 0 || box.right > window.innerWidth)
      .map(({ label, box }) => `${label} ${Math.round(box.left)}..${Math.round(box.right)}`));
    assert.deepEqual(overflowing, [], `${name}: every header control stays inside an 880px viewport`);
    await page.setViewportSize({ width: 1280, height: 800 });
    if (process.env.TUTORIAL_CAPTURE) await page.screenshot({ path: `/workspace/deck/build/840-shots/${process.env.TUTORIAL_CAPTURE}-${name}-expanded.png` });
    await page.keyboard.press("Escape");
    assert.equal(await open(), "false", "Escape folds the page");
    assert.equal(await page.evaluate(() => document.activeElement?.getAttribute("data-tutorial-action")), "toggle-step", "folding returns focus to Show step");
    assert.equal(await host.getAttribute("data-open"), "true", "Escape folds the page without closing the panel");
    await showStep.click();
    await showStep.click();
    assert.equal(await open(), "false", "pressing Show step again folds the page");
    await showStep.click();
    await panel.getByRole("button", { name: "Hide step", exact: true }).click();
    assert.equal(await open(), "false", "Hide step folds the page");
    await page.locator(`[data-live-${name}]`).click();
    await page.waitForFunction((region) => document.querySelector(`[data-region="${region}"]`)?.getAttribute("data-open") === "false", name);
    assert.equal(await open(), "true", `closing ${name} restores the full tutorial page`);
    assert.equal(await bar.isHidden(), true, "the bar leaves with the panel");
    assert.equal(record.step, 1, "panel docking preserves the saved step");
  }
  assert.equal(/^Step 2 of \d+: /.test(await page.locator('[data-region="tutorial-announce"]').innerText()), true, "the step is announced once through the live region");
  assert.equal(writes.length, writesBeforePanels, "panel docking never writes progress");
  await panel.getByRole("button", { name: "All lessons", exact: true }).press("Enter");
  const secondLesson = panel.locator('[data-lesson="lesson-2"]');
  const secondCaption = await secondLesson.locator(".tutorial-lesson-caption").innerText();
  assert.match(secondCaption, /^6 min/, "a catalog row shows the lesson's duration");
  assert.ok(secondCaption.includes("Uses two agent sessions"), "information appears on the row");
  assert.equal(await panel.locator("nav [data-lesson]").count() > 10, true, "the catalog lists every lesson");
  await clickSave(secondLesson, true);
  // The lesson's first step says it opens Tasks: arriving opens the panel
  // and the tutorial folds into the bar.
  const tasksHost = page.locator('[data-region="tasks"]');
  await page.waitForFunction(() => document.querySelector('[data-region="tasks"]')?.getAttribute("data-open") === "true");
  await bar.waitFor({ state: "visible" });
  assert.match(await bar.getAttribute("aria-label"), /^Tutorial, step 1 of \d+$/, "choosing a lesson opens the panel its first step names");
  assert.equal(await open(), "false");
  await page.locator("[data-live-tasks]").click();
  await page.waitForFunction(() => document.querySelector('[data-region="tasks"]')?.getAttribute("data-open") === "false");
  await page.evaluate(() => new Promise((resolve) => requestAnimationFrame(() => requestAnimationFrame(resolve))));
  assert.equal(await tasksHost.getAttribute("data-open"), "false", "a panel closed on the same step stays closed");
  assert.equal(await open(), "true", "closing the panel returns the full page");
  await clickSave(panel.locator('[data-tutorial-action="next"], [data-tutorial-action="skip"]'), true);
  assert.equal(await tasksHost.getAttribute("data-open"), "false", "a step with no panel opens none");
  await clickSave(panel.getByRole("button", { name: "Back", exact: true }), true);
  await page.waitForFunction(() => document.querySelector('[data-region="tasks"]')?.getAttribute("data-open") === "true");
  assert.equal(await open(), "false", "Back arrives on the step and opens its panel again");
  await showStep.click();
  await page.keyboard.press("Escape");
  // A save that settles after Show step repaints the page; focus stays on
  // the step heading, so Escape still folds it.
  holdWrite = true;
  const nextWriteStarted = new Promise((resolve) => { writeStarted = resolve; });
  await showStep.click();
  await panel.locator('[data-tutorial-action="next"], [data-tutorial-action="skip"]').click();
  await nextWriteStarted;
  await showStep.click();
  const heading = page.locator('[data-region="tutorial"] .tutorial-body h3');
  assert.equal(await heading.evaluate((el) => el === document.activeElement), true, "Show step focuses the step");
  releaseWrite();
  holdWrite = false;
  await page.waitForFunction(() => document.querySelector('[data-region="tutorial"]').dataset.over === "true");
  await page.evaluate(() => new Promise((resolve) => requestAnimationFrame(() => requestAnimationFrame(resolve))));
  assert.equal(await heading.evaluate((el) => el === document.activeElement), true, "a repaint from a late save keeps focus on the step");
  await page.keyboard.press("Escape");
  assert.equal(await open(), "false", "Escape still folds the step after the repaint");
  // Navigating from the unfolded page folds it; focus lands on Show step.
  const forward = panel.locator('[data-tutorial-action="next"], [data-tutorial-action="skip"]');
  const foldedFocus = async (label) => {
    await page.waitForFunction(() => document.querySelector('[data-region="tutorial"]').dataset.over === "false");
    const landed = await page.evaluate(() => document.activeElement?.getAttribute("data-tutorial-action"));
    assert.equal(landed, "toggle-step", `${label} leaves focus on Show step`);
  };
  await showStep.click();
  await forward.focus();
  await page.keyboard.press("Enter");
  await foldedFocus("forward");
  await showStep.click();
  await panel.locator('[data-tutorial-action="back"]').focus();
  await page.keyboard.press("Enter");
  await foldedFocus("Back");
  await showStep.click();
  await panel.locator('[data-tutorial-action="lessons"]').focus();
  await page.keyboard.press("Enter");
  const choice = panel.locator('[data-tutorial-action="lesson"]').first();
  await choice.focus();
  await page.keyboard.press("Enter");
  await foldedFocus("choosing a lesson");
  await page.locator("[data-live-tasks]").click();
  await page.waitForFunction(() => document.querySelector('[data-region="tutorial"]').getAttribute("data-open") === "true");
  await page.getByRole("button", { name: "Collapse sidebar", exact: true }).click();
  await page.setViewportSize({ width: 390, height: 844 });
  if (process.env.TUTORIAL_CAPTURE) await page.screenshot({ path: `/workspace/deck/build/tutorial-${process.env.TUTORIAL_CAPTURE}-mobile.png` });
  const mobileGeometry = await page.waitForFunction(() => {
    const panel = document.querySelector('[data-tutorial-mobile="true"] [data-region="tutorial"]');
    const header = panel?.querySelector(".tutorial-header")?.getBoundingClientRect();
    const close = panel?.querySelector('[aria-label="Close tutorial"]')?.getBoundingClientRect();
    if (!header?.width || !header.height || !close?.width || !close.height) return null;
    return {
      header: { y: header.y, height: header.height },
      close: { y: close.y, width: close.width, height: close.height },
    };
  });
  const { header: mobileHeader, close: mobileClose } = await mobileGeometry.jsonValue();
  assert.ok(mobileClose.width >= 36 && mobileClose.height >= 36, "mobile close target retains its size");
  assert.ok(mobileClose.y >= mobileHeader.y && mobileClose.y + mobileClose.height <= mobileHeader.y + mobileHeader.height,
    "mobile close target stays inside the sheet header");
  const beforeCollapse = writes.length;
  await panel.getByRole("button", { name: "Collapse tutorial", exact: true }).click();
  await page.locator('[data-region="tutorial-mobile-bar"]').waitFor({ state: "visible" });
  assert.equal(writes.length, beforeCollapse, "Collapse does not persist dismissal");
  await page.getByRole("button", { name: "Panels", exact: true }).click();
  await page.getByRole("menuitem", { name: "Files", exact: true }).click();
  const mobileFiles = page.locator('[data-region="files"][data-open="true"]');
  await mobileFiles.waitFor();
  await page.locator('[data-region="tutorial-mobile-bar"]').waitFor({ state: "hidden" });
  await panel.waitFor({ state: "hidden" });
  await mobileFiles.getByRole("button", { name: "Close files", exact: true }).click();
  await page.locator('[data-region="tutorial-mobile-bar"]').waitFor({ state: "visible" });
  assert.equal(await open(), "false", "closing another panel preserves explicit mobile collapse");
  assert.equal(writes.length, beforeCollapse, "temporarily hiding a collapsed tutorial does not save dismissal");
  await page.reload({ waitUntil: "networkidle" });
  assert.equal(await open(), "true", "Collapse resets on reload");
  failWrites = true;
  await clickSave(panel.getByRole("button", { name: "Close tutorial", exact: true }));
  await page.locator('[data-region="tutorial-notice"]').waitFor({ state: "visible" });
  assert.equal(await open(), "false", "failed save is reported after the panel closes");
  failWrites = false;
  hasSession = true;
  // The saved step opens Tasks. The project list arrives after the
  // preferences, and selecting the remembered session tears panels down;
  // that teardown is not the user closing Tasks.
  await page.setViewportSize({ width: 1280, height: 800 });
  record = { version: 1, dismissed: false, lesson: "lesson-2", step: 0, done: ["lesson-1"], scope: "user" };
  lastSessionID = "s1";
  holdProjects = true;
  releaseProjects = undefined;
  await page.reload({ waitUntil: "domcontentloaded" });
  await bar.waitFor({ state: "visible" });
  await page.waitForFunction(() => document.querySelector('[data-region="tasks"]')?.getAttribute("data-open") === "true");
  while (releaseProjects === undefined) await new Promise((resolve) => setTimeout(resolve, 20));
  holdProjects = false;
  releaseProjects();
  await page.waitForFunction(() => document.querySelector("[data-live-terminal] .xterm-rows")?.textContent?.includes("Tutorial probe"));
  await page.evaluate(() => new Promise((resolve) => requestAnimationFrame(() => requestAnimationFrame(resolve))));
  assert.equal(await page.locator('[data-region="tasks"]').getAttribute("data-open"), "true", "initial session selection keeps the panel the saved step opened");
  assert.equal(await bar.isVisible(), true, "the bar stays with it");
  // The user closes the panel while that selection is still pending:
  // finishing the selection must not reopen it.
  holdSessionDetail = true;
  sessionDetailReads = 0;
  releaseSessionDetail = undefined;
  holdProjects = true;
  releaseProjects = undefined;
  await page.reload({ waitUntil: "domcontentloaded" });
  await bar.waitFor({ state: "visible" });
  while (releaseProjects === undefined) await new Promise((resolve) => setTimeout(resolve, 20));
  holdProjects = false;
  releaseProjects();
  while (releaseSessionDetail === undefined) await new Promise((resolve) => setTimeout(resolve, 20));
  await page.waitForFunction(() => document.querySelector('[data-region="tasks"]')?.getAttribute("data-open") === "false");
  await page.locator("[data-live-tasks]").click();
  await page.locator('[data-region="tasks"][data-open="true"]').waitFor();
  await page.locator('[data-region="tasks"]').getByRole("button", { name: "Close tasks", exact: true }).click();
  await page.waitForFunction(() => document.querySelector('[data-region="tasks"]')?.getAttribute("data-open") === "false");
  holdSessionDetail = false;
  releaseSessionDetail();
  await page.waitForFunction(() => document.querySelector("[data-live-terminal] .xterm-rows")?.textContent?.includes("Tutorial probe"));
  await page.evaluate(() => new Promise((resolve) => requestAnimationFrame(() => requestAnimationFrame(resolve))));
  assert.equal(await page.locator('[data-region="tasks"]').getAttribute("data-open"), "false", "a panel the user closed while selection was pending stays closed");
  lastSessionID = null;
  holdRead = true;
  record = { version: 1, dismissed: false, lesson: "lesson-2", step: 1, done: ["lesson-1"], scope: "user" };
  await page.setViewportSize({ width: 1280, height: 800 });
  await page.reload({ waitUntil: "domcontentloaded" });
  const expandSidebar = page.getByRole("button", { name: "Show sidebar", exact: true });
  if (await expandSidebar.isVisible()) await expandSidebar.click();
  await page.locator('[data-session="s1"]').click();
  await page.waitForFunction(() => document.querySelector("[data-live-terminal] .xterm-rows")?.textContent?.includes("Tutorial probe"));
  const writesBeforeLoss = writes.length;
  terminalSocket.send(JSON.stringify({ type: "error", code: "fenced", message: "Another controller took over" }));
  await page.getByRole("button", { name: "Reconnect", exact: true }).waitFor();
  await page.locator("[data-live-more]").click();
  await page.getByRole("button", { name: "Show tutorial", exact: true }).click();
  assert.equal(await open(), "false", "Show tutorial is ignored while control is suspended");
  holdRead = false;
  releaseRead();
  await page.evaluate(() => new Promise((resolve) => requestAnimationFrame(() => requestAnimationFrame(resolve))));
  assert.equal(await open(), "false", "controller loss fences the delayed preferences read");
  assert.equal(writes.length, writesBeforeLoss);
  await page.getByRole("button", { name: "Reconnect", exact: true }).click();
  await panel.waitFor({ state: "visible" });
  assert.equal(await open(), "true", "regaining control re-reads preferences");
  assert.equal(await panel.evaluate((el) => el.contains(document.activeElement)), false,
    "ignored Show tutorial does not steal focus after reconnect");
  const dividerMeasurements = [];
  for (const density of ["normal", "compact", "comfortable"]) {
    await page.evaluate((value) => {
      if (value === "normal") document.documentElement.removeAttribute("data-density");
      else document.documentElement.dataset.density = value;
    }, density);
    const tutorialGeometry = await panel.locator(".tutorial-header").evaluate((header) => {
      const box = header.getBoundingClientRect();
      const terminal = document.querySelector("[data-workspace-titlebar]").getBoundingClientRect();
      const controlsFit = [...header.querySelectorAll("button")].filter((button) => button.getClientRects().length)
        .every((button) => {
          const control = button.getBoundingClientRect();
          return control.top >= box.top && control.bottom <= box.bottom && control.left >= box.left && control.right <= box.right;
        });
      return { tutorial: box.bottom, terminal: terminal.bottom, height: box.height, controlsFit };
    });
    assert.ok(Math.abs(tutorialGeometry.tutorial - tutorialGeometry.terminal) <= 0.5,
      `${density}: tutorial divider ${tutorialGeometry.tutorial} aligns with terminal ${tutorialGeometry.terminal}`);
    assert.ok(tutorialGeometry.controlsFit, `${density}: tutorial header controls fit without clipping`);
    await clickSave(panel.getByRole("button", { name: "Close tutorial", exact: true }));
    const panels = {};
    for (const [name, header] of [["files", "[data-files-header]"], ["tasks", "[data-tm-header]"], ["chats", "[data-chats-header]"]]) {
      await page.locator(`[data-live-${name}]`).click();
      const dock = page.locator(`[data-region="${name}"][data-open="true"]`);
      await dock.waitFor();
      const measured = await page.waitForFunction((selector) => {
        const box = document.querySelector(selector)?.getBoundingClientRect();
        return box && box.height > 0 ? box.bottom : false;
      }, `[data-region="${name}"][data-open="true"] ${header}`);
      const bottom = await measured.jsonValue();
      await measured.dispose();
      assert.ok(Math.abs(tutorialGeometry.tutorial - bottom) <= 0.5,
        `${density}: tutorial divider ${tutorialGeometry.tutorial} aligns with ${name} ${bottom}`);
      panels[name] = bottom;
      await page.locator(`[data-live-${name}]`).click();
      await dock.waitFor({ state: "hidden" });
    }
    await show();
    dividerMeasurements.push({ density, ...tutorialGeometry, panels });
  }
  await page.evaluate(() => { delete document.documentElement.dataset.density; });
  console.log("tutorial divider geometry:", JSON.stringify(dividerMeasurements));
  const writesBeforeChats = writes.length;
  await page.locator("[data-live-chats]").click();
  await page.locator('[data-region="chats"][data-open="true"]').waitFor();
  assert.equal(await open(), "false", "opening Chats folds the tutorial page");
  await bar.waitFor({ state: "visible" });
  assert.match(await bar.getAttribute("aria-label"), /^Tutorial, step 2 of \d+$/, "the bar stays and still names the step beside Chats");
  assert.equal(await page.locator('[data-region="chats"]').locator("[data-tutorial-action]").count(), 0, "Chats shows no tutorial progress or help entry");
  await page.locator("[data-live-chats]").click();
  await page.waitForFunction(() => document.querySelector('[data-region="chats"]')?.getAttribute("data-open") === "false");
  assert.equal(await open(), "true", "closing Chats restores the full tutorial page");
  assert.equal(writes.length, writesBeforeChats, "Chats docking never writes progress");
  const writesBeforeSwitch = writes.length;
  const secondToggle = page.locator('[data-project-toggle="p2"]');
  if (await secondToggle.getAttribute("aria-expanded") !== "true") await secondToggle.click();
  await page.locator('[data-session="s2"]').click();
  await page.waitForFunction(() => document.querySelector("[data-live-terminal] .xterm-rows")?.textContent?.includes("/s2/terminal"));
  assert.equal(await open(), "true");
  assert.ok((await panel.innerText()).includes("Two agents, one bug"));
  assert.equal(record.step, 1);
  assert.equal(writes.length, writesBeforeSwitch, "switching projects and sessions preserves progress without a write");
  const cursors = await page.evaluate(() => {
    const host = document.createElement("div");
    host.innerHTML = [
      '<button id="c-on">on</button>',
      '<button id="c-off" disabled>off</button>',
      '<button id="c-role-off" role="button" disabled>off</button>',
      '<div id="c-role-on" role="button" tabindex="0">on</div>',
      '<div id="c-role-aria" role="button" aria-disabled="true">off</div>',
      '<a id="c-link-on" href="#x">on</a>',
    ].join("");
    document.body.append(host);
    const read = Object.fromEntries([...host.children].map((el) => [el.id, getComputedStyle(el).cursor]));
    host.remove();
    return read;
  });
  assert.deepEqual(cursors, {
    "c-on": "pointer", "c-off": "default", "c-role-off": "default", "c-role-on": "pointer",
    "c-role-aria": "auto", "c-link-on": "pointer",
  }, "enabled controls get the pointer; disabled and aria-disabled ones keep the default cursor");
  const collapseSidebar = page.getByRole("button", { name: "Collapse sidebar", exact: true });
  if (await collapseSidebar.isVisible()) await collapseSidebar.click();
  await page.setViewportSize({ width: 390, height: 844 });
  await page.waitForFunction(() => {
    const sheet = document.querySelector('[data-region="tutorial"]').getBoundingClientRect();
    const dock = document.querySelector('[data-region="terminal-dock"]').getBoundingClientRect();
    return dock.height > 0 && sheet.bottom <= dock.top + 1;
  });
  if (process.env.TUTORIAL_CAPTURE) await page.screenshot({ path: `/workspace/deck/build/tutorial-${process.env.TUTORIAL_CAPTURE}-mobile-session.png` });
  await panel.getByRole("button", { name: "Collapse tutorial", exact: true }).click();
  const lessonBar = page.locator('[data-region="tutorial-mobile-bar"]');
  await lessonBar.waitFor({ state: "visible" });
  assert.ok((await lessonBar.boundingBox()).y < (await page.locator('[data-region="terminal-soft-keys"]').boundingBox()).y);
  await lessonBar.getByRole("button", { name: "Show step", exact: true }).click();
  assert.equal(writes.length, writesBeforeSwitch, "mobile sheet and bar navigation do not write progress");
  holdWrite = true;
  const started = new Promise((resolve) => { writeStarted = resolve; });
  await panel.locator('[data-tutorial-action="next"], [data-tutorial-action="skip"]').click();
  await started;
  await page.locator("[data-live-more]").click();
  await page.locator("[data-live-logout]").click();
  await panel.waitFor({ state: "detached" });
  releaseWrite();
  await page.evaluate(() => new Promise((resolve) => requestAnimationFrame(() => requestAnimationFrame(resolve))));
  assert.equal(await page.locator('[data-region="tutorial"]').count(), 0, "late save cannot remount after auth teardown");
  assert.equal(writes.length, writesBeforeSwitch + 1);
  assert.equal(identityWrites, 1);
  assert.deepEqual(errors, []);
  console.log("ok: first-use, persistent close, restored progress, dock handoff, mobile collapse, failed write, controller loss, project/session switch, auth teardown");
  await context.close();
} finally {
  await browser?.close();
  vite.kill("SIGTERM");
}
