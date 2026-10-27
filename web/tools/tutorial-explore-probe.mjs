// Optional feature help stays navigable without invoking project or provider actions.
import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import net from "node:net";
import { mkdir } from "node:fs/promises";
import { readdirSync, readFileSync } from "node:fs";
import { join } from "node:path";

const captureDir = process.env.TUTORIAL_EXPLORE_CAPTURE_DIR;
if (captureDir) await mkdir(captureDir, { recursive: true });

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
  const { buildTutorialCatalog } = await import("../src/app/tutorial/catalog.ts");
  const { emptyStepContext, stepHidden, substitute } = await import("../src/app/tutorial/step-view.ts");
  const lessonsDir = new URL("../src/app/tutorial/lessons/en/", import.meta.url);
  const catalog = buildTutorialCatalog(Object.fromEntries(readdirSync(lessonsDir)
    .filter((name) => name.endsWith(".md"))
    .map((name) => [name, readFileSync(new URL(name, lessonsDir), "utf8")])));
  const exploreTutorialCatalog = catalog.filter((lesson) => lesson.optional);
  const lastCourse = catalog.filter((lesson) => !lesson.optional).at(-1);
  const expected = ["away", "reviewer", "sessions", "files", "tasks", "previews", "memory", "reconnect", "logins", "appearance", "what-an-agent-can-reach", "core-principles"];
  assert.deepEqual(exploreTutorialCatalog.map((lesson) => lesson.id), expected, "every Explore chapter is present, in order");
  // The words a reader sees for a Markdown body: emphasis, code marks and link syntax are gone.
  const plain = (markdown) => substitute(markdown, emptyStepContext).text
    .replace(/\[([^\]]+)\]\([^)]+\)/g, "$1").replace(/[*`]/g, "").replace(/\s+/g, " ").trim();
  const visibleSteps = (lesson) => lesson.steps.map((step, index) => ({ step, index })).filter(({ step }) => !stepHidden(step, emptyStepContext));
  const context = await browser.newContext({ viewport: { width: 1280, height: 900 } });
  await context.addCookies([{ name: "noisedeck_csrf", value: "probe", url: origin }]);
  const page = await context.newPage();
  const errors = [];
  const input = [];
  page.on("pageerror", (error) => errors.push(error.message));
  page.on("request", (request) => { if (new URL(request.url()).pathname.endsWith("/preview/settings") && previewAskedAt === 0) previewAskedAt = Date.now(); });
  page.on("requestfailed", (request) => { if (new URL(request.url()).pathname.endsWith("/preview/settings") && previewFailedAt === 0) previewFailedAt = Date.now(); });
  const project = { id: "p1", name: "Explore project", canonical_path: "/workspace/explore", last_used_at: "t", git_identity_setup_revision: 1 };
  const session = { id: "s1", project_id: "p1", provider: "claude", display_name: "Explore console", status: "running", presentation_state: "running", generation: 1, created_at: "2026-08-01T00:00:00Z", updated_at: "2026-08-01T00:00:00Z", recovery_capability: "none", recovery_state: "unavailable", tmux_pane_id: "%1", pane_pid: 4242, native_session_id: "fixture" };
  let holdFacts = false;
  let factsHeld = false;
  let releaseFacts = () => {};
  let holdPreviews = true;
  let previewAskedAt = 0;
  let previewFailedAt = 0;
  let releasePreviews = () => {};
  let record = { version: 1, dismissed: false, lesson: lastCourse.id, step: lastCourse.steps.length - 1, done: [], scope: "user" };
  const writes = [];
  await page.routeWebSocket(`${origin.replace("http", "ws")}/api/sessions/*/terminal?*`, (socket) => {
    socket.onMessage((raw) => { if (JSON.parse(String(raw)).type === "input") input.push(raw); });
    socket.send(JSON.stringify({ type: "attached", token: 7, generation: 1 }));
    socket.send(JSON.stringify({ type: "output", data: Buffer.from("Explore console\r\n").toString("base64") }));
  });
  await page.route(`${origin}/auth/session`, (route) => route.fulfill({ status: 204, headers: { "X-Noisedeck-Auth-Mode": "builtin" } }));
  await page.route(`${origin}/api/**`, async (route) => {
    const request = route.request();
    const path = new URL(request.url()).pathname;
    const json = (status, body) => route.fulfill({ status, contentType: "application/json", body: JSON.stringify(body) });
    if (path === "/api/settings/tutorial") {
      if (request.method() === "PUT") {
        assert.equal(request.headers()["x-csrf-token"], "probe");
        record = { ...request.postDataJSON(), scope: "user" };
        writes.push(record);
      }
      return json(200, record);
    }
    if (path === "/api/settings/layout") return json(request.method() === "PUT" ? 200 : 404, request.method() === "PUT" ? request.postDataJSON() : { code: "not_found" });
    assert.equal(request.method(), "GET", `static help caused a mutation: ${path}`);
    if (path === "/api/settings") return json(200, { skip_permissions: false, skip_close_confirm: false, skip_remove_project_confirm: false });
    if (path === "/api/projects") return json(200, [project]);
    if (path.endsWith("/sessions")) return json(200, [session]);
    if (path === "/api/sessions/s1") return json(200, session);
    if (path === "/api/providers") return json(200, { providers: ["claude", "shell"] });
    if (path === "/api/harness-login") return json(200, { pending: [] });
    if (path === "/api/harness-usage") return json(200, { global: [], project: [] });
    if (path === "/api/git-identities") return json(200, { profiles: [{ id: "identity", name: "Probe", email: "probe@example.invalid", revision: 1 }], default_profile_id: "identity", revision: 1 });
    if (path === "/api/project-directories") return json(200, { current_path: "", parent_path: null, directories: [] });
    if (path.endsWith("/events")) return route.fulfill({ status: 200, contentType: "text/event-stream", body: ": connected\n\nretry: 3600000\n\n" });
    if (path.endsWith("/files")) return json(200, { path: "", at_root: true, entries: [] });
    if (/^\/api\/projects\/[^/]+\/lanes$/.test(path)) return json(200, { lanes: [] });
    if (path.includes("/taskmaster")) return json(404, { code: "not_taskmaster_project", message: "Not a Tasks project" });
    if (path.endsWith("/chats")) return json(502, { error: "ledger_unavailable", message: "the chat ledger could not be read" });
    if (path === "/api/chats/status") return json(200, { available: false });
    if (path === "/api/github/connect/status") {
      if (holdFacts) await new Promise((resolve) => { releaseFacts = resolve; factsHeld = true; });
      return json(200, { outcome: "no-account" });
    }
    if (path.endsWith("/preview/settings")) {
      if (holdPreviews) {
        await new Promise((resolve) => { releasePreviews = resolve; });
        return route.abort().catch(() => {});
      }
      return json(200, { configured: false, self_service: false, enabled: false });
    }
    if (path === "/api/settings/last-session") {
      if (route.request().method() === "PUT") return json(200, route.request().postDataJSON());
      return json(404, { code: "not_found", message: "no last session remembered" });
    }
    // The closed Assets panel reads the listing and follows its feed for its badge.
    if (/^\/api\/projects\/[^/]+\/assets$/.test(path)) return route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ locations: [], scanned: "2026-01-01T00:00:00Z", preview_max_bytes: 16777216 }) });
    if (/^\/api\/projects\/[^/]+\/assets\/events$/.test(path)) return route.fulfill({ status: 200, contentType: "text/event-stream", body: ": connected\n\nretry: 3600000\n\n" });
    throw new Error(`unhandled ${path}`);
  });
  const panel = page.locator('[data-region="tutorial"]');
  const button = (name) => panel.getByRole("button", { name, exact: true });
  let expectedWrites = 0;
  const save = async (action) => {
    expectedWrites += 1;
    const [response] = await Promise.all([
      page.waitForResponse((response) => response.url().endsWith("/api/settings/tutorial") && response.request().method() === "PUT"), action(),
    ]);
    await response.finished();
  };
  const show = async () => {
    await page.locator("[data-live-more]").click();
    await save(() => page.getByRole("button", { name: "Show tutorial", exact: true }).click());
    await panel.waitFor({ state: "visible" });
    const resume = button("Resume");
    if (await resume.isVisible()) await resume.click();
  };
  // A step that opens a panel folds the page into the bar on desktop;
  // the walk reads the page, so it unfolds it.
  const reveal = async () => {
    const bar = page.locator('[data-region="tutorial-bar"]');
    if (!(await bar.isVisible())) return;
    const toggle = bar.getByRole("button", { name: "Show step", exact: true });
    if (await toggle.getAttribute("aria-expanded") === "true") return;
    await toggle.click();
    await panel.waitFor({ state: "visible" });
  };
  const choose = async (lesson) => {
    await reveal();
    await button("All lessons").click();
    const option = panel.locator(`[data-lesson="${lesson.id}"]`);
    await option.focus();
    // The facts read repaints the list; the focused row must survive it.
    await page.waitForFunction((id) => document.activeElement?.getAttribute("data-lesson") === id, lesson.id);
    await save(() => option.press("Enter"));
    await reveal();
    await panel.getByRole("heading", { name: substitute(visibleSteps(lesson)[0].step.title, emptyStepContext).text, exact: true }).waitFor();
  };
  await page.goto(origin, { waitUntil: "networkidle" });
  await panel.waitFor({ state: "visible" });
  await save(() => button("Finish").click());
  assert.equal(record.dismissed, true);
  assert.equal(record.lesson, lastCourse.id, "Finish does not enter optional help");
  assert.equal(record.done.some((id) => exploreTutorialCatalog.some((lesson) => lesson.id === id)), false);
  await show();
  holdFacts = true;
  await panel.locator(".tutorial-body").getByRole("button", { name: "All lessons", exact: true }).click();
  assert.equal(await panel.locator("nav").isVisible(), true, "the closing page opens the lesson list");
  // The facts response arrives after a row has keyboard focus; the repaint must not take the focus away.
  const early = panel.locator(`nav [data-lesson="${exploreTutorialCatalog[0].id}"]`);
  await early.focus();
  assert.equal(await panel.locator(`nav [data-lesson="reviewer"]`).getAttribute("data-dimmed"), "false", "the GitHub fact is still unknown before its response");
  while (!factsHeld) await page.waitForTimeout(20);
  await page.waitForFunction(() => document.querySelector('nav [data-lesson="lesson-2"]')?.getAttribute("data-dimmed") === "true");
  assert.equal(await panel.locator(`nav [data-lesson="reviewer"]`).getAttribute("data-dimmed"), "false", "the chat fact lands while the GitHub and preview reads are still pending");
  holdFacts = false;
  releaseFacts();
  await page.waitForFunction(() => document.querySelector('nav [data-lesson="away"]')?.getAttribute("data-dimmed") === "true"
    && document.querySelector('nav [data-lesson="reviewer"]')?.getAttribute("data-dimmed") === "true");
  assert.equal(await panel.locator(`nav [data-lesson="previews"]`).getAttribute("data-dimmed"), "false", "a preview read that has not answered leaves its row unknown");
  assert.equal(await early.evaluate((el) => el === document.activeElement), true, "a late facts response keeps focus on the focused lesson row");
  const row = (id) => panel.locator(`nav [data-lesson="${id}"]`);
  const durationOf = (id) => catalog.find((lesson) => lesson.id === id).duration;
  const caption = (id) => row(id).locator(".tutorial-lesson-caption").innerText();
  for (const lesson of catalog) assert.equal((await caption(lesson.id)).split(" · ")[0], lesson.duration, `${lesson.id}: the row shows its duration`);
  assert.equal(await panel.locator("nav p.tutorial-lesson-group").allInnerTexts().then((texts) => texts.map((text) => text.toLowerCase())).then((texts) => texts.join("|")), "get started|explore deck", "the group headings stay");
  assert.equal(await panel.locator("nav [data-lesson]").count(), catalog.length, "the catalog never hides a lesson");
  const previewDeadlineMS = 12000;
  while (previewFailedAt === 0 && Date.now() - previewAskedAt < previewDeadlineMS) await page.waitForTimeout(100);
  assert.notEqual(previewFailedAt, 0, "the browser cancels a preview read the server never answers");
  assert.ok(previewFailedAt - previewAskedAt < previewDeadlineMS, "the preview read is cancelled within its deadline");
  holdPreviews = false;
  releasePreviews();
  const lessonsToggle = panel.locator('button[aria-expanded][data-tutorial-action="lessons"]');
  await lessonsToggle.click();
  await lessonsToggle.click();
  await page.waitForFunction(() => document.querySelector('nav [data-lesson="previews"]')?.getAttribute("data-dimmed") === "true");
  assert.equal(await caption("away"), `${durationOf("away")} · Needs Deck reachable from your phone`);
  assert.equal(await caption("reviewer"), `${durationOf("reviewer")} · Needs a GitHub connection`);
  assert.equal(await caption("previews"), `${durationOf("previews")} · Needs project previews`);
  assert.equal(await row("lesson-2").getAttribute("data-dimmed"), "true", "an install without agent chat dims lesson 2");
  assert.match(await caption("lesson-2"), /Needs agent chat \(MemPalace\)/);
  const setup = panel.locator('nav [data-lesson="lesson-2"] + .tutorial-lesson-enable');
  assert.equal(await setup.count(), 1, "one line under the row says how to turn agent chat on");
  assert.match(await setup.innerText(), /^Docker installs add the agent chat overlay to turn it on\. Agent chat setup$/);
  assert.match(await setup.locator("a").getAttribute("href"), /#agent-chat-and-shared-memory$/);
  assert.equal(await row("tasks").getAttribute("data-dimmed"), "false", "information does not dim a row");
  assert.ok((await caption("tasks")).endsWith("Uses your Claude plan or API key"));
  assert.equal(await row("files").getAttribute("data-dimmed"), "false");
  assert.equal(await row("reviewer").isEnabled(), true, "a dimmed row can still be opened for reading");
  assert.equal(await row(lastCourse.id).getAttribute("aria-current"), "step", "the current lesson is marked");
  assert.equal(await panel.locator('nav [aria-label="Done"]').count(), record.done.length, "a check mark for each finished lesson");
  assert.equal(await row(lastCourse.id).locator('[aria-label="Done"]').count(), 1, "the finished course lesson shows its check mark");
  if (captureDir) await page.screenshot({ path: join(captureDir, "catalog.png"), clip: { x: 0, y: 0, width: 1280, height: 800 } });
  const writesBeforeExplore = writes.length;
  await save(() => early.press("Enter"));
  await reveal();
  await panel.getByRole("heading", { name: exploreTutorialCatalog[0].steps[0].title, exact: true }).waitFor();
  assert.equal(record.lesson, exploreTutorialCatalog[0].id);
  assert.equal(record.step, 0);
  assert.equal(writes.length, writesBeforeExplore + 1, "choosing a chapter saves its destination exactly once");
  assert.equal(await button("Back").isDisabled(), true, "Back stops at the optional course boundary");
  for (const width of [1280, 390]) {
    if (width === 390) {
      const collapse = page.getByRole("button", { name: "Collapse sidebar", exact: true });
      if (await collapse.isVisible()) await collapse.click();
    }
    for (const name of ["files", "tasks", "chats"]) {
      if (await page.locator(`[data-region="${name}"][data-open="true"]`).count() > 0) await page.locator(`[data-live-${name}]`).click();
    }
    await page.setViewportSize({ width, height: 900 });
    for (const lesson of exploreTutorialCatalog) {
      await choose(lesson);
      if (captureDir && lesson.id === "files") await page.screenshot({ path: join(captureDir, `files-${width}.png`) });
      const shown = visibleSteps(lesson);
      for (const [position, { step, index }] of shown.entries()) {
        await panel.getByRole("heading", { name: substitute(step.title, emptyStepContext).text, exact: true }).waitFor();
        const text = (await panel.innerText()).replace(/\s+/g, " ");
        assert.ok(text.includes(plain(step.body).slice(0, 60)), `${lesson.id}: the body of "${step.title}" is on screen`);
        assert.equal(record.lesson, lesson.id);
        assert.equal(record.step, index);
        if (position < shown.length - 1) {
          await save(() => panel.locator('[data-tutorial-action="next"], [data-tutorial-action="skip"]').press("Enter"));
          await reveal();
        }
      }
    }
  }
  const first = exploreTutorialCatalog[0];
  await choose(first);
  for (let step = 1; step <= visibleSteps(first).length; step += 1) {
    await save(() => panel.locator('[data-tutorial-action="next"], [data-tutorial-action="skip"]').press("Enter"));
    await reveal();
  }
  assert.ok(record.done.includes(first.id), "leaving a chapter's last step records it as done");
  await button("All lessons").click();
  const doneRows = await panel.locator("nav [data-lesson]").evaluateAll((rows) => rows.filter((row) => row.querySelector('[aria-label="Done"]')).map((row) => row.getAttribute("data-lesson")));
  assert.deepEqual(doneRows.sort(), record.done.filter((id) => id !== "welcomed").sort(), "check marks follow the saved done list");
  assert.ok(doneRows.includes(first.id) && doneRows.includes(lastCourse.id));
  await button("All lessons").click();
  const saved = { lesson: record.lesson, step: record.step };
  await save(() => button("Close tutorial").click());
  await page.reload({ waitUntil: "networkidle" });
  assert.equal(await panel.getAttribute("data-open"), "false");
  await show();
  assert.deepEqual({ lesson: record.lesson, step: record.step }, saved);
  for (const width of [1280, 390]) {
    await page.setViewportSize({ width, height: 900 });
    for (const name of ["files", "tasks", "chats"]) {
      const writesBeforePanel = writes.length;
      const preferencesBeforePanel = structuredClone(record);
      if (width < 768) {
        await page.getByRole("button", { name: "Panels", exact: true }).click();
        await page.getByRole("menuitem", { name: name[0].toUpperCase() + name.slice(1), exact: true }).click();
      } else {
        await page.locator(`[data-live-${name}]`).click();
      }
      const host = page.locator(`[data-region="${name}"][data-open="true"]`);
      await host.waitFor();
      assert.equal(await panel.getAttribute("data-open"), "false", `opening ${name} folds the tutorial page`);
      assert.equal(await panel.evaluate((el) => el.inert), true);
      const mobileBar = page.locator('[data-region="tutorial-mobile-bar"]');
      const deskBar = page.locator('[data-region="tutorial-bar"]');
      await mobileBar.waitFor({ state: "hidden" });
      assert.equal(await mobileBar.boundingBox(), null, "hidden progress takes no layout space");
      if (width < 768) {
        await panel.waitFor({ state: "hidden" });
        assert.equal(await deskBar.boundingBox(), null, "the phone shows no desktop bar");
      } else {
        await deskBar.waitFor({ state: "visible" });
        assert.match(await deskBar.getAttribute("aria-label"), /^Tutorial, step \d+ of \d+$/, `${name}: the bar names the step`);
      }
      assert.equal(await host.locator("[data-tutorial-action]").count(), 0, `${name} shows no tutorial progress or help entry`);
      if (captureDir && width === 390 && name === "files") await page.screenshot({ path: join(captureDir, "explore-files-context-phone.png") });
      await host.getByRole("button", { name: `Close ${name}`, exact: true }).click();
      await page.waitForFunction((region) => document.querySelector(`[data-region="${region}"]`)?.getAttribute("data-open") === "false", name);
      assert.equal(await panel.getAttribute("data-open"), "true", `closing ${name} restores the tutorial`);
      assert.deepEqual(record, preferencesBeforePanel, "panel docking preserves every saved preference");
      assert.equal(writes.length, writesBeforePanel, "panel docking never writes progress");
      if (width < 768) {
        await page.getByRole("button", { name: "Panels", exact: true }).click();
        await page.getByRole("menuitem", { name: name[0].toUpperCase() + name.slice(1), exact: true }).click();
      } else {
        await page.locator(`[data-live-${name}]`).click();
      }
      await host.waitFor();
      if (width === 390) {
        await host.getByRole("button", { name: `Close ${name}`, exact: true }).click();
        await save(() => button("Close tutorial").click());
      }
      await show();
      assert.equal(await page.locator(`[data-region="${name}"]`).getAttribute("data-open"), "false", "Show tutorial closes the other panel");
      assert.deepEqual({ lesson: record.lesson, step: record.step }, saved, "explicit Show tutorial restores saved progress");
    }
  }
  assert.equal(input.length, 0, "reading help never sends native terminal input");
  assert.deepEqual(errors, []);
  assert.equal(writes.length, expectedWrites, "each explicit navigation saves once, including no delayed Explore Deck duplicate");
  console.log("ok: every Explore chapter on desktop/phone/keyboard, course Finish boundary, persistence, panel handoff, zero project/provider actions");
  await context.close();
} finally {
  await browser?.close();
  vite.kill("SIGTERM");
}
