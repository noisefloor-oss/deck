// A first-time user on an installation with no projects at all walks the course: lesson 1 from
// Choose an agent through creation to the prompt, lesson 2 through both prompts, and lesson 3's
// brief actions, against a stateful mock of the project, file and Tasks APIs. Every prompt must land
// in the session of the project its step is about, and on a phone the primary action stays in reach.
import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import { readdirSync, readFileSync } from "node:fs";
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
  const { buildTutorialCatalog } = await import("../src/app/tutorial/catalog.ts");
  const lessonsDir = new URL("../src/app/tutorial/lessons/en/", import.meta.url);
  const catalog = buildTutorialCatalog(Object.fromEntries(readdirSync(lessonsDir)
    .filter((name) => name.endsWith(".md"))
    .map((name) => [name, readFileSync(new URL(name, lessonsDir), "utf8")])));
  const lesson = (id) => catalog.find((entry) => entry.id === id);
  const stepIndex = (id, title) => {
    const index = lesson(id).steps.findIndex((step) => step.title === title);
    assert.notEqual(index, -1, `${id} has a step titled ${title}`);
    return index;
  };
  const promptText = lesson("lesson-1").steps[stepIndex("lesson-1", "Ask {agent} to build it")].prompt;
  const commandText = lesson("lesson-1").steps[stepIndex("lesson-1", "Check that it works")].command;

  const context = await browser.newContext({ viewport: { width: 1280, height: 900 } });
  await context.addCookies([{ name: "noisedeck_csrf", value: "probe", url: origin }]);
  const page = await context.newPage();
  const errors = [];
  page.on("pageerror", (error) => errors.push(error.message));
  const stamp = "2026-08-01T00:00:00Z";
  const projects = [];
  const base = { project_id: "p1", status: "running", presentation_state: "running", generation: 1, created_at: stamp, updated_at: stamp, recovery_capability: "none", recovery_state: "unavailable", tmux_pane_id: "%1", pane_pid: 4242 };
  const sessions = [];
  let outcome = "sent";
  // While true, write statuses wait here until the probe acknowledges them in order.
  let holdAcks = false;
  const heldAcks = [];
  let holdCreate = null;
  let createCount = 0;
  // A project of the user's own, with one task, and the lanes opened on it.
  const ownId = "p-own";
  const ownTasks = [{ id: "1", title: "Make the release notes shorter", description: "Cut the preamble.", status: "pending", priority: "high", revision: 3 }];
  const lanes = [];
  const seenProjectIds = new Set();
  const goneRequests = [];
  let providerList = ["claude", "codex", "shell"];
  const laneFaults = { sessionsLive: true, reviewSeen: false, failOwnReads: 0 };
  let record = { version: 1, dismissed: false, lesson: "lesson-1", step: 0, done: [], scope: "user" };
  const inputs = [];
  let inputDelay = 0;
  const sockets = new Map();
  const replies = [];
  const modifiedFiles = new Set();
  let relnotesTaskReads = 0;
  const mutations = [];
  const fakeFiles = new Map();
  const directories = new Set();
  const tasks = [];
  const prds = new Set();
  const runs = new Map();
  const faults = { hold: null, codex: false, defaultIdentity: "identity", strayFile: null, failUpload: null, runExit: 0, generated: 2, failGenerate: false };
  let runPolls = 0;
  await page.routeWebSocket(`${origin.replace("http", "ws")}/api/sessions/*/terminal?*`, (socket) => {
    const id = new URL(socket.url()).pathname.split("/")[3];
    sockets.set(id, socket);
    socket.onMessage(async (raw) => {
      const frame = JSON.parse(String(raw));
      // The server answers an emulator reply outside its allowlist with a
      // dropped status and writes nothing.
      if (frame.type === "reply") {
        replies.push(Buffer.from(frame.data, "base64").toString());
        socket.send(JSON.stringify({ type: "status", code: "dropped", message: "a terminal reply outside the allowed set was not written" }));
        return;
      }
      if (frame.type === "input") {
        if (inputDelay) await new Promise((resolve) => setTimeout(resolve, inputDelay));
        inputs.push({ session: id, text: Buffer.from(frame.data, "base64").toString(), token: frame.token, generation: frame.generation });
        const ack = (code) => {
          socket.send(JSON.stringify({ type: "status", code, message: code === "sent" ? "" : `write ${code}` }));
          if (code === "uncertain") socket.close();
        };
        if (holdAcks) heldAcks.push(ack);
        else ack(outcome);
      }
    });
    socket.send(JSON.stringify({ type: "attached", token: 7, generation: 1 }));
    socket.send(JSON.stringify({ type: "output", data: Buffer.from(`Console ${id}\r\n`).toString("base64") }));
  });
  await page.route(`${origin}/auth/session`, (route) => route.fulfill({ status: 204, headers: { "X-Noisedeck-Auth-Mode": "builtin" } }));
  await page.route(`${origin}/api/**`, async (route) => {
    const request = route.request();
    const url = new URL(request.url());
    const path = url.pathname;
    const method = request.method();
    const json = (status, body) => route.fulfill({ status, contentType: "application/json", body: JSON.stringify(body) });
    if (path === "/api/settings/tutorial") {
      if (method === "PUT") record = { ...request.postDataJSON(), scope: "user" };
      return json(200, record);
    }
    if (path === "/api/settings/layout" || path === "/api/settings/last-session") {
      return json(method === "PUT" ? 200 : 404, method === "PUT" ? request.postDataJSON() : { code: "not_found" });
    }
    const projectPath = /^\/api\/projects\/([^/]+)(\/.*)?$/.exec(path);
    const known = projectPath ? projects.find((entry) => entry.id === projectPath[1]) : undefined;
    const rest = projectPath?.[2] ?? "";
    // A project that was registered and then removed (a lane's project after
    // Done, or a practice project the scenario resets) can still be read by a
    // refresh that was already in flight. Reads answer 404, as the server does;
    // a write to a removed project is a bug and fails the probe.
    for (const entry of projects) seenProjectIds.add(entry.id);
    if (projectPath && !known && seenProjectIds.has(projectPath[1])) {
      goneRequests.push(`${method} ${path}`);
      if (method !== "GET") throw new Error(`write to a removed project: ${method} ${path}`);
      return json(404, { error: "not_found", message: "project not found" });
    }
    if (method !== "GET") {
      assert.equal(request.headers()["x-csrf-token"], "probe");
      const body = method === "POST" && request.postData() !== null && !rest.startsWith("/files") ? request.postDataJSON() : null;
      mutations.push({ method, path: path + (rest.startsWith("/files") ? url.search : ""), body, bytes: rest.startsWith("/files") ? request.postData() : null });
      if (method === "POST" && path === "/api/project-directories/mkdir") {
        const full = `${body.path}/${body.name}`;
        if (directories.has(full)) return json(409, { error: "exists", message: "directory already exists" });
        directories.add(full);
        return json(201, { path: full });
      }
      if (method === "POST" && path === "/api/projects") {
        const created = { id: `p${projects.length + 1}`, name: body.name, canonical_path: body.path, last_used_at: "t", git_identity_setup_revision: 1 };
        projects.push(created);
        fakeFiles.set(created.id, new Map([[".git", ""]]));
        if (faults.strayFile !== null) fakeFiles.get(created.id).set(faults.strayFile, "stray");
        return json(201, created);
      }
      if (known && method === "POST" && rest === "/sessions") {
        if (faults.hold !== null && body.provider === "codex" && known.name.startsWith("relnotes")) await faults.hold;
        const created = { ...base, ...(body.provider === "codex" && faults.codex ? { codex_login_helper: true } : {}), project_id: known.id, id: `${body.provider === "shell" ? "shell" : "agent"}-${sessions.length}`, provider: body.provider, display_name: body.provider === "shell" ? "Terminal" : "Agent", native_session_id: "fixture" };
        sessions.push(created);
        createCount += 1;
        if (holdCreate) await holdCreate.promise;
        return json(201, created);
      }
      if (known && method === "POST" && rest === "/files") {
        const name = url.searchParams.get("path");
        if (faults.failUpload === name) return json(500, { error: "boom", message: "upload exploded" });
        fakeFiles.get(known.id).set(name, request.postData() ?? "");
        return json(200, { path: name, size: (request.postData() ?? "").length });
      }
      if (known && method === "POST" && rest === "/starter-commit") return json(201, { commit: "abc", created: true });
      if (known && method === "POST" && rest === "/taskmaster") return json(200, { path: "/store", tag: "master", already_initialized: false });
      if (known && method === "POST" && rest === "/taskmaster/prds") {
        prds.add(body.name);
        return json(201, {});
      }
      if (known && method === "POST" && rest === "/taskmaster/tasks") {
        const store = known.id === ownId ? ownTasks : tasks;
        store.push({ id: String(store.length + 1), title: body.title, description: body.description, status: "pending", priority: "medium", revision: 1 });
        return json(201, { id: String(store.length) });
      }
      if (known && method === "POST" && rest === "/taskmaster/generate") {
        if (faults.failGenerate) return json(500, { error: "boom", message: "generation exploded" });
        runPolls = 0;
        const run = { id: "run-1", project_id: known.id, argv: [], prd: body.prd, status: "running", exit_code: null, started_at: stamp };
        runs.set(run.id, run);
        return json(202, run);
      }
      if (known && method === "POST" && rest === "/lanes") {
        const laneProject = { id: "p-lane", name: `${known.name} (task ${body.task})`, canonical_path: `${known.canonical_path}/.worktrees/main-${body.task}`, last_used_at: "t", git_identity_setup_revision: 1 };
        projects.push(laneProject);
        const opened = { tag: body.tag, task: body.task, agent: body.agent, project_id: known.id, lane_project_id: laneProject.id, branch: `tasks/${body.tag}/${body.task}`, phase: "open", review_requested: false };
        lanes.push(opened);
        return json(201, opened);
      }
      const reviewRequest = /^\/lanes\/([^/]+)\/(\d+)\/review$/.exec(rest);
      if (known && method === "POST" && reviewRequest) {
        const found = lanes.find((entry) => entry.project_id === known.id && String(entry.task) === reviewRequest[2]);
        found.review_requested = true;
        found.review_requested_at = new Date().toISOString();
        found.review_agent = body.agent;
        return json(200, found);
      }
      const markDone = /^\/taskmaster\/tasks\/(\d+)\/done$/.exec(rest);
      if (known && method === "POST" && markDone) {
        const found = (known.id === ownId ? ownTasks : tasks).find((entry) => entry.id === markDone[1]);
        const lane = lanes.find((entry) => entry.project_id === known.id && String(entry.task) === markDone[1]);
        if (lane !== undefined && laneFaults.sessionsLive && body.stop_sessions !== true) {
          return json(409, { error: "lane_sessions_live", message: "these sessions are still running in the lane: claude, codex", sessions: ["claude", "codex"] });
        }
        found.status = "done";
        if (lane === undefined) return json(200, { status: "done", lane_closed: false });
        lanes.splice(lanes.indexOf(lane), 1);
        projects.splice(projects.findIndex((entry) => entry.id === lane.lane_project_id), 1);
        for (let i = sessions.length - 1; i >= 0; i--) if (sessions[i].project_id === lane.lane_project_id) sessions.splice(i, 1);
        return json(200, { status: "done", lane_closed: true, branch: lane.branch });
      }
    if (path === "/api/providers") return json(200, { providers: providerList });
      throw new Error(`unexpected mutation ${method} ${path}`);
    }
    if (path === "/api/settings") return json(200, { skip_permissions: false, skip_close_confirm: false, skip_remove_project_confirm: false });
    if (path === "/api/projects") return json(200, projects);
    if (known && rest === "/sessions") return json(200, sessions.filter((entry) => entry.project_id === known.id));
    if (path.startsWith("/api/sessions/") && !path.endsWith("/chats")) return json(200, sessions.find((entry) => entry.id === path.split("/")[3]));
    if (path === "/api/providers") return json(200, { providers: providerList });
    if (path === "/api/harness-login") return json(200, { pending: [] });
    if (path === "/api/harness-usage") return json(200, { global: [], project: [] });
    if (path === "/api/git-identities") return json(200, { profiles: [{ id: "identity", name: "Probe", email: "probe@example.invalid", revision: 1 }], default_profile_id: faults.defaultIdentity, revision: 1 });
    if (path === "/api/project-directories") {
      const names = [...directories].map((full) => full.slice(full.lastIndexOf("/") + 1));
      return json(200, { current_path: "/workspace", parent_path: null, directories: names.map((name) => ({ name, path: `/workspace/${name}` })) });
    }
    if (path.endsWith("/events")) return route.fulfill({ status: 200, contentType: "text/event-stream", body: ": connected\n\nretry: 3600000\n\n" });
    if (known && rest === "/files") {
      const entries = [...(fakeFiles.get(known.id)?.keys() ?? [])];
      return json(200, { path: "", at_root: true, entries: entries.map((name) => ({ name, path: name, dir: false, mode: "-rw-r--r--", mtime: stamp, ...(modifiedFiles.has(`${known.id}/${name}`) ? { change: "modified" } : {}) })) });
    }
    if (known && rest === "/files/view") {
      const content = fakeFiles.get(known.id)?.get(url.searchParams.get("path"));
      return content === undefined ? json(404, { code: "not_found", message: "no such file" }) : json(200, { content });
    }
    if (projectPath && rest === "/lanes") return json(200, { lanes: lanes.filter((entry) => entry.project_id === projectPath[1] || entry.lane_project_id === projectPath[1]) });
    const reviewRead = /^\/lanes\/([^/]+)\/(\d+)\/review$/.exec(rest);
    if (known && reviewRead) {
      const found = lanes.find((entry) => entry.project_id === known.id && String(entry.task) === reviewRead[2]);
      return json(200, { requested: found?.review_requested === true, seen: laneFaults.reviewSeen, pull_request: 12 });
    }
    // A lane's worktree reads the same task store as the project it works on.
    const ownStore = known?.id === ownId || known?.id === "p-lane";
    if (known && ownStore && rest === "/taskmaster") return json(200, { path: known.canonical_path, config_path: "", tasks_path: "", docs_path: ".tasks/docs" });
    if (known && ownStore && rest === "/taskmaster/tasks" && laneFaults.failOwnReads > 0) {
      laneFaults.failOwnReads -= 1;
      return json(500, { error: "boom", message: "task read failed" });
    }
    if (known && ownStore && rest === "/taskmaster/tasks") return json(200, { main: { tasks: ownTasks } });
    if (known && rest === "/taskmaster/prds") return json(200, [...prds]);
    if (known && rest === "/taskmaster/tasks") {
      if (known.name.startsWith("relnotes")) relnotesTaskReads += 1;
      return fakeFiles.has(known.id) && fakeFiles.get(known.id).has("tutorial.json")
        ? json(200, { master: { tasks } })
        : json(404, { code: "not_taskmaster_project", message: "Not a Tasks project" });
    }
    if (known && rest.startsWith("/taskmaster/runs/")) {
      const run = runs.get(rest.split("/")[3]);
      runPolls += 1;
      if (runPolls >= 2 && run.status === "running") {
        run.status = "exited";
        run.exit_code = faults.runExit;
        if (faults.runExit === 0) for (let i = 0; i < faults.generated; i++) tasks.push({ id: String(tasks.length + 1), title: `Generated ${i + 1}`, description: "", status: "pending" });
      }
      return json(200, run);
    }
    if (path.includes("/taskmaster")) return json(404, { code: "not_taskmaster_project", message: "Not a Tasks project" });
    if (path.endsWith("/chats")) return json(200, path.startsWith("/api/sessions/") ? { agent: "probe", chats: [] } : { chats: [] });
    if (path === "/api/chats/status") return json(200, { available: true });
    if (path === "/api/github/connect/status") return json(200, { outcome: "no-account" });
    if (path.endsWith("/preview/settings")) return json(200, { configured: false, self_service: false, enabled: false });
    // The closed Assets panel reads the listing and follows its feed for its badge.
    if (/^\/api\/projects\/[^/]+\/assets$/.test(path)) return route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ locations: [], scanned: "2026-01-01T00:00:00Z", preview_max_bytes: 16777216 }) });
    if (/^\/api\/projects\/[^/]+\/assets\/events$/.test(path)) return route.fulfill({ status: 200, contentType: "text/event-stream", body: ": connected\n\nretry: 3600000\n\n" });
    throw new Error(`unhandled ${path}`);
  });
  const panel = page.locator('[data-region="tutorial"]');
  const button = (name) => panel.getByRole("button", { name, exact: true });
  const status = () => panel.locator(".tutorial-status");
  const settle = () => new Promise((resolve) => setTimeout(resolve, 150));
  // The saved position is the only way to land on a step without walking
  // there, so each scenario writes it and reloads.
  // A step that opens a panel folds the lesson into the bar; these walks
  // read the page, so they unfold it.
  const bar = page.locator('[data-region="tutorial-bar"]');
  const reveal = async () => {
    for (let attempt = 0; attempt < 20; attempt++) {
      if (!(await bar.isVisible())) return;
      const toggle = bar.getByRole("button", { name: "Show step", exact: true });
      try {
        if (await toggle.getAttribute("aria-expanded", { timeout: 1500 }) === "true") return;
        await toggle.click({ timeout: 1500 });
        await panel.waitFor({ state: "visible", timeout: 3000 });
        return;
      } catch {
        // The step moved on while the bar was being read; look again.
      }
    }
  };
  const open = async (id, title) => {
    record = { ...record, dismissed: false, lesson: id, step: stepIndex(id, title) };
    await page.reload({ waitUntil: "networkidle" });
    await page.waitForFunction(() => document.querySelector('[data-region="tutorial"]')?.dataset.open === "true" || document.querySelector('[data-region="tutorial-bar"]:not([hidden])') !== null);
    await reveal();
    await panel.waitFor({ state: "visible" });
  };
  const until = async (what, condition, ms = 12000) => {
    const started = Date.now();
    while (Date.now() - started < ms) {
      if (await condition()) return;
      await new Promise((resolve) => setTimeout(resolve, 50));
    }
    assert.fail(`timed out waiting for ${what}`);
  };
  const titlePattern = (title) => new RegExp(`^${title.replace(/[.*+?^$()|[\]\\]/g, "\\$&").replace(/\{agent\}/g, ".+")}$`);
  const heading = async () => (await panel.locator(".tutorial-body h3").textContent())?.trim() ?? "";
  const onStep = (title) => until(`the step "${title}"`, async () => titlePattern(title).test(await heading()));
  const forward = panel.locator('[data-tutorial-action="next"], [data-tutorial-action="skip"]');
  const select = async (id) => {
    if (id === "rel-agent") {
      const toggle = page.locator('[data-project-toggle="p-rel"]');
      if (await toggle.getAttribute("aria-expanded") === "false") await toggle.click();
    }
    await page.locator(`[data-session="${id}"]`).click();
    await page.waitForFunction((sessionID) => document.querySelector(`[data-session="${sessionID}"]`)?.getAttribute("aria-selected") === "true", id);
    await page.waitForFunction(() => document.querySelector("[data-live-terminal] .xterm-rows")?.textContent?.includes("Console"));
  };
  const shots = process.env.TUTORIAL_SHOTS ?? "";
  const shoot = async (name) => { if (shots !== "") await page.screenshot({ path: `${shots}/${name}.png` }); };
  const writesOnly = () => mutations.map((m) => `${m.method} ${m.path}`);
  const lessonFileCount = (name) => readdirSync(new URL(`../src/app/tutorial/starters/${name}/`, import.meta.url)).length;
  const goTo = async (id, title) => {
    const target = stepIndex(id, title);
    await reveal();
    if (await panel.locator(`nav[aria-label="Tutorial lessons"]`).count() === 0) await button("All lessons").click();
    await panel.locator(`[data-tutorial-action="lesson"][data-lesson="${id}"]`).click();
    for (let i = 0; i < 30; i++) {
      await reveal();
      const current = await heading();
      const index = lesson(id).steps.findIndex((step) => titlePattern(step.title).test(current));
      if (index === target) return;
      assert.notEqual(index, -1, `${current} is a step of ${id}`);
      if (index > target) await button("Back").click();
      else await forward.click();
      await settle();
    }
    assert.fail(`could not reach ${title}`);
  };
  const sessionOf = (name) => sessions.find((entry) => entry.project_id === projects.find((project) => project.name === name)?.id && entry.provider !== "shell");
  const selected = () => page.evaluate(() => document.querySelector('[data-session][aria-selected="true"]')?.getAttribute("data-session") ?? null);
  const asked = (title) => lesson("lesson-1").steps[stepIndex("lesson-1", title)];
  const ask = asked("Ask {agent} to build it").prompt;
  assert.ok(ask.includes("node --test relnotes.test.mjs") && !/node --test[,.]/.test(ask), "the prompt runs only the test file the Check step runs");
  const clean = () => {
    projects.length = 0;
    sessions.length = 0;
    tasks.length = 0;
    mutations.length = 0;
    inputs.length = 0;
    for (const collection of [fakeFiles, directories, prds, runs, modifiedFiles]) collection.clear();
  };
  const reset = async (step = 0) => {
    clean();
    record = { version: 1, dismissed: false, lesson: "lesson-1", step, done: [], scope: "user" };
    await page.reload({ waitUntil: "networkidle" });
    await arrived();
    if (step === 0) await button("Start the tutorial").click();
  };
  const arrived = async () => {
    await page.waitForFunction(() => document.querySelector('[data-region="tutorial"]')?.dataset.open === "true" || document.querySelector('[data-region="tutorial-bar"]:not([hidden])') !== null);
    await reveal();
    await panel.waitFor({ state: "visible" });
  };
  const pick = async (provider) => {
    await page.locator("[data-new-session-modal]").waitFor();
    await page.locator(`[data-new-session-provider="${provider}"]`).click();
    await page.locator("[data-new-session-modal]").waitFor({ state: "detached" });
  };

  // --- Lesson 1 on an installation with no projects ---
  await page.goto(origin, { waitUntil: "networkidle" });
  await panel.waitFor({ state: "visible" });
  await onStep("Welcome to Deck");
  await shoot("after-0-welcome");
  await button("Start the tutorial").click();
  await onStep("Choose a coding agent");
  await shoot("after-1-empty-installation-start");
  await button("Choose an agent…").click();
  await page.locator("[data-new-session-modal]").waitFor();
  assert.equal(await panel.locator(".tutorial-status").filter({ hasText: "Add a project first" }).count(), 0, "no dead message about a missing project");
  const offered = await page.locator("[data-new-session-provider]").evaluateAll((nodes) => nodes.map((node) => node.dataset.newSessionProvider).sort());
  assert.deepEqual(offered, ["claude", "codex"], "the picker lists the installation's agents and not the plain shell");
  assert.match(await page.locator("[data-new-session-dialog]").innerText(), /for the tutorial/);
  await shoot("after-1-choose-agent-picker");
  assert.deepEqual(mutations, []);
  await pick("claude");
  await onStep("Create the practice projects");
  assert.deepEqual(writesOnly(), [], "choosing an agent creates no session and no project");
  assert.equal(projects.length, 0);
  await shoot("after-1-agent-chosen");
  await button("Create relnotes and start claude").click();
  await onStep("Turn the PRD into tasks");
  const ids = Object.fromEntries(projects.map((entry) => [entry.name, entry.id]));
  assert.deepEqual(Object.keys(ids).sort(), ["ccparse", "relnotes"]);
  assert.equal(fakeFiles.get(ids.relnotes).size, lessonFileCount("relnotes") + 2);
  const relnotesAgent = sessionOf("relnotes");
  assert.equal(relnotesAgent?.provider, "claude", "claude started in relnotes");
  assert.equal(sessions.length, 1, "exactly one session, in relnotes");
  assert.equal(inputs.length, 0, "nothing typed by creating");
  await reveal();
  await button("Add PRD… → Generate tasks").click();
  await status().filter({ hasText: "Generated 2 tasks." }).waitFor({ state: "attached" });
  await onStep("Ask {agent} to build it");
  await reveal();
  await shoot("after-1-prompt-step-1280");
  await button("Put it in the terminal").click();
  await status().filter({ hasText: "Press Return to send it" }).waitFor();
  await until("the relnotes prompt on its terminal socket", () => inputs.length > 0);
  await settle();
  assert.deepEqual(inputs.map((entry) => [entry.session, entry.text]), [[relnotesAgent.id, ask]], "the prompt is typed once, into the relnotes agent, without Return");
  assert.equal(/[\r\n\x1b]/.test(inputs[0].text), false);

  // --- Skipping the agent step does not strand the user ---
  await reset(stepIndex("lesson-1", "Create the practice projects"));
  await onStep("Create the practice projects");
  await button("Create relnotes and start your agent").click();
  await page.locator("[data-new-session-modal]").waitFor();
  assert.deepEqual(writesOnly(), [], "the choice is offered where the user is, and nothing is created before it");
  await shoot("after-1-create-without-agent");
  await pick("codex");
  await until("the projects", () => projects.length === 2);
  await onStep("Turn the PRD into tasks");
  assert.equal(sessionOf("relnotes")?.provider, "codex", "creating continues with the agent chosen there");

  // --- Codex on an installation with no projects: the sign-in helper opens on its own ---
  faults.codex = true;
  await reset();
  await button("Choose an agent…").click();
  await pick("codex");
  await button("Create relnotes and start codex").click();
  await onStep("Sign in to codex");
  await page.locator("[data-harness-login-dialog]").waitFor({ timeout: 10000 });
  await shoot("after-codex-helper-opens-on-its-own");
  await page.locator("[data-harness-login-dialog] [data-login-close]").first().click();
  await page.locator("[data-harness-login-dialog]").waitFor({ state: "detached" });
  faults.codex = false;

  // --- Lesson 2: each prompt goes to its own project's session ---
  await reset();
  await button("Choose an agent…").click();
  await pick("claude");
  await button("Create relnotes and start claude").click();
  await onStep("Turn the PRD into tasks");
  const relnotesSession = sessionOf("relnotes");
  await goTo("lesson-2", "Start the other agent");
  await shoot("after-2-start-the-other-agent");
  assert.equal(await panel.locator('[data-tutorial-action="insert-prompt"]').count(), 0, "the second action is not offered before the session exists");
  mutations.length = 0;
  await button("Start claude in ccparse").click();
  await until("a ccparse session", () => sessionOf("ccparse") !== undefined);
  const ccparseSession = sessionOf("ccparse");
  await until("the new session to be selected", async () => await selected() === ccparseSession.id);
  await button("Put it in the terminal").waitFor();
  assert.equal(await panel.getByRole("button", { name: "Start claude in ccparse", exact: true }).count(), 0, "the button became Put it in the terminal");
  await shoot("after-2-put-it-in-the-terminal");
  // The first prompt belongs to ccparse, even with relnotes selected.
  await page.locator(`[data-session="${relnotesSession.id}"]`).click();
  await page.waitForFunction((id) => document.querySelector(`[data-session="${id}"]`)?.getAttribute("aria-selected") === "true", relnotesSession.id);
  inputs.length = 0;
  await reveal();
  await button("Put it in the terminal").click();
  await status().filter({ hasText: "Press Return to send it" }).waitFor();
  await until("the ccparse prompt on its terminal socket", () => inputs.length > 0);
  await settle();
  const ccparsePrompt = lesson("lesson-2").steps[stepIndex("lesson-2", "Start the other agent")].prompt;
  assert.deepEqual(inputs.map((entry) => [entry.session, entry.text]), [[ccparseSession.id, ccparsePrompt]], "the ccparse prompt went to the ccparse agent");
  assert.equal(await selected(), ccparseSession.id, "the ccparse agent was selected first");
  await onStep("Ask the relnotes agent to find it");
  // With ccparse still selected the relnotes prompt goes to relnotes.
  inputs.length = 0;
  await reveal();
  assert.equal(await selected(), ccparseSession.id);
  // A previous delivery status may still be visible. Delay this socket write
  // beyond the old fixed sleep: only observed input can satisfy delivery.
  inputDelay = 350;
  await button("Put it in the terminal").click();
  await until("the relnotes follow-up on its terminal socket", () => inputs.length > 0);
  // Confirmed input opens Chats and folds the tutorial on the next step.
  // Reopen that step before checking the delivery notice it carries forward.
  await onStep("Watch them talk");
  await panel.waitFor({ state: "hidden" });
  await reveal();
  await status().filter({ hasText: "Press Return to send it" }).waitFor();
  inputDelay = 0;
  await settle();
  const relnotesPrompt = lesson("lesson-2").steps[stepIndex("lesson-2", "Ask the relnotes agent to find it")].prompt;
  assert.deepEqual(inputs.map((entry) => [entry.session, entry.text]), [[relnotesSession.id, relnotesPrompt]], "the relnotes prompt went to the relnotes agent, not the selected ccparse one");
  assert.equal(await selected(), relnotesSession.id);
  // Commands follow their step's project too: Proof runs the relnotes tests with ccparse selected.
  await goTo("lesson-2", "Proof");
  await page.locator(`[data-session="${ccparseSession.id}"]`).click();
  await page.waitForFunction((id) => document.querySelector(`[data-session="${id}"]`)?.getAttribute("aria-selected") === "true", ccparseSession.id);
  inputs.length = 0;
  mutations.length = 0;
  await reveal();
  await button("Put in a terminal").click();
  await status().filter({ hasText: "Press Return to send it" }).waitFor();
  await until("the proof command on its terminal socket", () => inputs.length > 0);
  await settle();
  const relnotesID = projects.find((entry) => entry.name === "relnotes").id;
  assert.deepEqual(mutations.map((m) => [m.method, m.path, m.body.provider]), [["POST", `/api/projects/${relnotesID}/sessions`, "shell"]], "the shell is made in relnotes, not in the selected ccparse");
  const proofShell = sessions.find((entry) => entry.provider === "shell");
  assert.deepEqual(inputs.map((entry) => [entry.session, entry.text]), [[proofShell.id, "node --test"]], "the command went to the relnotes shell, without Return");
  assert.equal(proofShell.project_id, relnotesID);
  // Pressing it again from ccparse reuses that shell.
  await page.locator(`[data-session="${ccparseSession.id}"]`).click();
  await page.waitForFunction((id) => document.querySelector(`[data-session="${id}"]`)?.getAttribute("aria-selected") === "true", ccparseSession.id);
  mutations.length = 0;
  await button("Put in a terminal").click();
  await until("the command typed again", () => inputs.length === 2);
  assert.deepEqual(mutations, [], "no second shell");
  assert.equal(inputs[1].session, proofShell.id);
  // A step about the user's own project refuses when none is known, and names what to do.
  await goTo("lesson-3", "See your release notes");
  mutations.length = 0;
  inputs.length = 0;
  await button("Put in a terminal").click();
  await status().filter({ hasText: "Select a session in your own project first." }).waitFor();
  assert.deepEqual([mutations.length, inputs.length], [0, 0]);
  await goTo("lesson-2", "Ask the relnotes agent to find it");
  // A project with no agent session is named in the refusal and nothing is typed.
  sessions.splice(sessions.indexOf(relnotesSession), 1);
  await page.reload({ waitUntil: "networkidle" });
  await arrived();
  await goTo("lesson-2", "Ask the relnotes agent to find it");
  inputs.length = 0;
  await button("Put it in the terminal").click();
  await status().filter({ hasText: "no" }).filter({ hasText: "session in relnotes" }).waitFor();
  await settle();
  assert.equal(inputs.length, 0, "no session, no typing");

  // --- Lesson 3: the brief on the user's own project ---
  await reset();
  projects.push({ id: ownId, name: "mycode", canonical_path: "/workspace/mycode", last_used_at: "t", git_identity_setup_revision: 1 });
  sessions.push({ ...base, id: "own-agent", project_id: ownId, provider: "claude", display_name: "Own agent", native_session_id: "fixture-own" });
  fakeFiles.set(ownId, new Map());
  record = { version: 1, dismissed: false, lesson: "lesson-3", step: stepIndex("lesson-3", "Give it a brief"), done: [], scope: "user" };
  await page.reload({ waitUntil: "networkidle" });
  await arrived();
  await select("own-agent");
  // The user's task already exists, so the step completes on arrival; Back returns to read it.
  await goTo("lesson-3", "Give it a brief");
  await shoot("after-4-brief-step");
  assert.equal(await button("Add task…").isDisabled(), false);
  assert.equal(await button("Add PRD… → Generate tasks").isDisabled(), false);
  assert.equal(await panel.locator(".sr-only").count(), 0, "no reason is left for screen readers alone");
  await button("Add task…").click();
  const addTask = page.getByRole("dialog", { name: "Add task" });
  await addTask.waitFor();
  await shoot("after-4-add-task-form");
  await addTask.locator("[data-tm-task-title]").fill("Shorten the preamble");
  await addTask.getByRole("button", { name: "Add task", exact: true }).click();
  await addTask.waitFor({ state: "detached" });
  assert.equal(ownTasks.at(-1).title, "Shorten the preamble", "the form added a task to the user's project");
  await goTo("lesson-3", "Give it a brief");
  mutations.length = 0;
  await button("Add PRD… → Generate tasks").click();
  const addPrd = page.getByRole("dialog", { name: "Add PRD" });
  await addPrd.waitFor();
  assert.deepEqual(writesOnly(), [], "with no PRD the Tasks panel's own Add PRD flow opens");
  await shoot("after-4-add-prd-dialog");
  await addPrd.locator("[data-tm-prd-file]").setInputFiles({ name: "brief.md", mimeType: "text/markdown", buffer: Buffer.from("# Brief\nShorten the notes.\n") });
  await addPrd.locator("[data-tm-prd-upload]").click();
  await until("the PRD upload and generation", () => mutations.some((m) => m.path === `/api/projects/${ownId}/taskmaster/generate`));
  await addPrd.waitFor({ state: "detached" });
  const generated = mutations.find((m) => m.path === `/api/projects/${ownId}/taskmaster/generate`);
  assert.equal(generated.body.numTasks, undefined, "no task count is sent for the user's own project");
  // With a PRD on the board, the button generates straight away with the default count.
  await until("the first generation to finish", () => [...runs.values()].every((run) => run.status !== "running"));
  await until("the board to see it", async () => await page.getByText("generating tasks…").count() === 0, 15000);
  await goTo("lesson-3", "Give it a brief");
  mutations.length = 0;
  await reveal();
  await button("Add PRD… → Generate tasks").click();
  await until("generation", () => mutations.some((m) => m.path.endsWith("/taskmaster/generate")));
  const direct = mutations.find((m) => m.path.endsWith("/taskmaster/generate"));
  assert.equal(direct.path, `/api/projects/${ownId}/taskmaster/generate`);
  assert.equal(direct.body.numTasks, undefined);
  assert.equal(await page.getByRole("dialog", { name: "Add PRD" }).count(), 0);
  await status().filter({ hasText: "Generating tasks from the project's PRD" }).waitFor();
  // A refused request is reported with its own error, never as a job that is running.
  await until("the run to finish", () => [...runs.values()].every((run) => run.status !== "running"));
  await until("the board to see it", async () => await page.getByText("generating tasks…").count() === 0, 15000);
  faults.failGenerate = true;
  await goTo("lesson-3", "Give it a brief");
  await reveal();
  await button("Add PRD… → Generate tasks").click();
  await status().filter({ hasText: "Task generation did not start: generation exploded" }).waitFor();
  assert.equal(await status().filter({ hasText: "Generating tasks from the project's PRD" }).count(), 0, "a failed request is not called generating");
  faults.failGenerate = false;

  // --- A phone keeps the primary action in reach ---
  await page.setViewportSize({ width: 390, height: 844 });
  const reachable = async (label, id, title) => {
    clean();
    projects.push({ id: ownId, name: "mycode", canonical_path: "/workspace/mycode", last_used_at: "t", git_identity_setup_revision: 1 });
    sessions.push({ ...base, id: "own-agent", project_id: ownId, provider: "claude", display_name: "Own agent", native_session_id: "fixture-own" });
    record = { version: 1, dismissed: false, lesson: id, step: stepIndex(id, title), done: [], scope: "user" };
    await page.reload({ waitUntil: "networkidle" });
    await arrived();
    await onStep(title);
    // With a terminal open, the phone's key bar takes the room under the sheet.
    await page.waitForFunction(() => document.querySelector("[data-live-terminal] .xterm-rows")?.textContent?.includes("Console"));
    await reveal();
    await onStep(title);
    await shoot(`measure-${id}-${title.replace(/\W+/g, "-")}-390`);
    const geometry = await page.evaluate((name) => {
      const body = document.querySelector('[data-region="tutorial"] .tutorial-body');
      const footer = document.querySelector('[data-region="tutorial"] .tutorial-footer');
      const target = [...body.querySelectorAll("button")].find((candidate) => candidate.textContent.trim() === name);
      const rect = target.getBoundingClientRect();
      const frame = body.getBoundingClientRect();
      return { top: rect.top, bottom: rect.bottom, frameTop: frame.top, frameBottom: frame.bottom, footerTop: footer.getBoundingClientRect().top, scrollTop: body.scrollTop, height: window.innerHeight };
    }, label);
    assert.ok(geometry.top >= geometry.frameTop && geometry.bottom <= geometry.frameBottom + 1, `${title}: "${label}" is inside the lesson body without scrolling (${JSON.stringify(geometry)})`);
    assert.ok(geometry.bottom <= geometry.footerTop + 1 && geometry.bottom <= geometry.height, `${title}: "${label}" sits above Continue and Skip step`);
    assert.equal(geometry.scrollTop, 0);
    return geometry;
  };
  await reachable("Put it in the terminal", "lesson-1", "Ask {agent} to build it");
  await shoot("after-5-lesson-1-prompt-390");
  const more = panel.getByRole("button", { name: "Show full prompt", exact: true });
  await more.click();
  assert.equal(await panel.getByRole("button", { name: "Show less", exact: true }).getAttribute("aria-expanded"), "true", "the whole prompt can be shown");
  await shoot("after-5-lesson-1-prompt-390-expanded");
  await reachable("Start your agent in ccparse", "lesson-2", "Start the other agent");
  await shoot("after-5-lesson-2-start-390");

  assert.deepEqual(errors, []);
  console.log("ok: an installation with no projects walks lesson 1 from Choose an agent (picker without a project, no shell, records only) through creation to the prompt; skipping the agent step offers the choice at Create; lesson 2 types each prompt into its own project's agent and names a project with no agent; lesson 3's brief opens the Tasks panel's Add task form and Add PRD flow or generates with the default count; on a 390x844 phone the primary action stays above the footer");
  await context.close();
} finally {
  await browser?.close();
  vite.kill("SIGTERM");
}
