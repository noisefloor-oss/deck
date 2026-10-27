// Tutorial buttons run the existing product actions, one gesture each, and never submit input.
// The practice-project flow runs against a stateful mock of the project, file and Tasks APIs.
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
  const projects = [{ id: "p1", name: "Practice", canonical_path: "/workspace/practice", last_used_at: "t", git_identity_setup_revision: 1 }];
  const base = { project_id: "p1", status: "running", presentation_state: "running", generation: 1, created_at: stamp, updated_at: stamp, recovery_capability: "none", recovery_state: "unavailable", tmux_pane_id: "%1", pane_pid: 4242 };
  const sessions = [
    { ...base, id: "agent", provider: "claude", display_name: "Agent console", native_session_id: "fixture" },
    { ...base, id: "agent2", provider: "claude", display_name: "Second console", native_session_id: "fixture-2" },
  ];
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
  const faults = { hold: null, codex: false, defaultIdentity: "identity", strayFile: null, failUpload: null, runExit: 0, generated: 2 };
  let runPolls = 0;
  await page.routeWebSocket(`${origin.replace("http", "ws")}/api/sessions/*/terminal?*`, (socket) => {
    const id = new URL(socket.url()).pathname.split("/")[3];
    sockets.set(id, socket);
    socket.onMessage((raw) => {
      const frame = JSON.parse(String(raw));
      // The server answers an emulator reply outside its allowlist with a
      // dropped status and writes nothing.
      if (frame.type === "reply") {
        replies.push(Buffer.from(frame.data, "base64").toString());
        socket.send(JSON.stringify({ type: "status", code: "dropped", message: "a terminal reply outside the allowed set was not written" }));
        return;
      }
      if (frame.type === "input") {
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
        tasks.push({ id: String(tasks.length + 1), title: body.title, description: body.description, status: "pending" });
        return json(201, { id: String(tasks.length) });
      }
      if (known && method === "POST" && rest === "/taskmaster/generate") {
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
    if (!(await bar.isVisible())) return;
    const toggle = bar.getByRole("button", { name: "Show step", exact: true });
    if (await toggle.getAttribute("aria-expanded") === "true") return;
    await toggle.click();
    await panel.waitFor({ state: "visible" });
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
    const toggle = page.locator('[data-project-toggle="p-rel"]');
    if (await toggle.count() > 0 && await toggle.getAttribute("aria-expanded") === "false") await toggle.click();
    await page.locator(`[data-session="${id}"]`).click();
    await page.waitForFunction((sessionID) => document.querySelector(`[data-session="${sessionID}"]`)?.getAttribute("aria-selected") === "true", id);
    await page.waitForFunction(() => document.querySelector("[data-live-terminal] .xterm-rows")?.textContent?.includes("Console"));
  };

  // A verified practice pair with an agent in relnotes, for the prompt mechanics
  // below. The practice-project walk starts from a clean installation again.
  const seedPair = () => {
    projects.push(
      { id: "p-rel", name: "relnotes", canonical_path: "/workspace/relnotes", last_used_at: "t", git_identity_setup_revision: 1 },
      { id: "p-cc", name: "ccparse", canonical_path: "/workspace/ccparse", last_used_at: "t", git_identity_setup_revision: 1 },
    );
    fakeFiles.set("p-rel", new Map([["tutorial.json", JSON.stringify({
      version: 1,
      relnotes: { projectId: "p-rel", canonicalPath: "/workspace/relnotes" },
      ccparse: { projectId: "p-cc", canonicalPath: "/workspace/ccparse" },
    })]]));
    fakeFiles.set("p-cc", new Map());
    sessions.push({ ...base, id: "rel-agent", project_id: "p-rel", provider: "claude", display_name: "Relnotes agent", native_session_id: "fixture-rel" });
  };
  const unseedPair = () => {
    projects.length = 1;
    sessions.length = 2;
    fakeFiles.clear();
  };
  await page.goto(origin, { waitUntil: "networkidle" });
  await panel.waitFor({ state: "visible" });
  await button("Start the tutorial").click();
  assert.equal(inputs.length, 0, "opening the tutorial sends no terminal input");

  // choose-agent opens the New session dialog and starts nothing.
  await button("Choose an agent…").click();
  await page.locator("[data-new-session-modal]").waitFor();
  assert.deepEqual(mutations, [], "choosing an agent only opens the dialog");
  await page.keyboard.press("Escape");
  await page.locator("[data-new-session-modal]").waitFor({ state: "detached" });

  // Without an agent the label says so, and without a project nothing happens until pressed.
  await open("lesson-1", "Create the practice projects");
  await button("Create relnotes and start your agent").waitFor();
  assert.equal(await panel.locator('[data-tutorial-action="create-starter"]').count(), 1, "create-starter is wired");
  assert.deepEqual(mutations, [], "showing the step sends nothing");
  assert.equal(await panel.getByText("Set a Git identity…").count(), 0, "the identity fallback stays hidden while a default identity exists");
  // The brief step's buttons work on the user's own project.
  await open("lesson-3", "Give it a brief");
  assert.equal(await button("Add PRD… → Generate tasks").isDisabled(), false, "generate-tasks works without a count");
  assert.equal(await button("Add task…").isDisabled(), false, "add-task works without a requirement");

  // insert-prompt types into the agent session of the project the step is about and never presses Enter.
  seedPair();
  await open("lesson-1", "Ask {agent} to build it");
  assert.equal((await panel.locator(".tutorial-prompt").innerText()).trim(), promptText, "the prompt is shown in full");
  await panel.locator(".tutorial-body").evaluate((el) => el.style.setProperty("--type-mono-size", "8px"));
  assert.equal(await panel.locator(".tutorial-prompt").evaluate((el) => getComputedStyle(el).fontSize), "8px",
    "tutorial machine text follows the mono x-height ceiling");
  assert.equal(inputs.length, 0, "showing the prompt types nothing");
  await select("rel-agent");
  await button("Put it in the terminal").click();
  await status().filter({ hasText: "Press Return to send it" }).waitFor();
  await settle();
  assert.ok(await page.evaluate(() => document.activeElement?.closest("[data-live-terminal]") !== null), "keyboard focus is in the terminal");
  assert.equal(inputs.length, 1);
  assert.equal(inputs[0].text, promptText, "the exact prompt is typed");
  assert.equal(/[\r\n\x1b]/.test(inputs[0].text), false, "no Enter and no control bytes");
  assert.equal(inputs[0].token, 7);
  assert.equal(inputs[0].generation, 1);
  assert.deepEqual(mutations, [], "inserting a prompt changes no project, session or credential");

  // The prompt goes to the relnotes agent even while another project's session is selected.
  await open("lesson-1", "Ask {agent} to build it");
  await select("agent");
  const beforeRouted = inputs.length;
  await button("Put it in the terminal").click();
  await status().filter({ hasText: "Press Return to send it" }).waitFor();
  await settle();
  assert.equal(inputs.length, beforeRouted + 1);
  assert.equal(inputs.at(-1).session, "rel-agent", "the relnotes step types into the relnotes agent, not the selected Practice agent");
  assert.equal(inputs.at(-1).text, promptText);
  assert.equal(await page.locator('[data-session="rel-agent"]').getAttribute("aria-selected"), "true", "the relnotes agent was selected first");
  inputs.length = 1;

  // run-command creates a shell session through the normal API when none is selected.
  await open("lesson-1", "Check that it works");
  await select("agent");
  await button("Put in a terminal").click();
  await status().filter({ hasText: "Press Return to send it" }).waitFor();
  await settle();
  assert.deepEqual(mutations.map((m) => [m.method, m.path, m.body.provider]), [["POST", "/api/projects/p-rel/sessions", "shell"]], "exactly one shell session was created, in the relnotes project the step is about");
  const typed = inputs.filter((entry) => entry.text === commandText);
  assert.equal(typed.length, 1, "the command is typed once");
  assert.ok(typed[0].session.startsWith("shell-"), "into the new shell, not the agent");
  assert.equal(inputs.length, 2, "nothing else was typed");
  // With the shell selected, a second press reuses it.
  await button("Put in a terminal").click();
  await settle();
  assert.equal(mutations.length, 1, "the selected shell is reused");
  assert.equal(inputs.filter((entry) => entry.text === commandText).length, 2);
  // An agent prompt is never typed into a shell.
  const beforeShellPrompt = inputs.length;
  const shellID = sessions.find((entry) => entry.provider === "shell").id;
  await open("lesson-1", "Ask {agent} to build it");
  await select(shellID);
  await button("Put it in the terminal").click();
  await status().filter({ hasText: "Press Return to send it" }).waitFor();
  await settle();
  assert.equal(inputs.length, beforeShellPrompt + 1);
  assert.equal(inputs.at(-1).session, "rel-agent", "a shell session does not receive the agent prompt; the relnotes agent does");
  // A project with no agent session refuses and names it.
  sessions.splice(sessions.findIndex((entry) => entry.id === "rel-agent"), 1);
  await open("lesson-1", "Ask {agent} to build it");
  await select(shellID);
  const beforeNoAgent = inputs.length;
  await button("Put it in the terminal").click();
  await status().filter({ hasText: "no agent session in relnotes" }).waitFor();
  await settle();
  assert.equal(inputs.length, beforeNoAgent, "nothing is typed when the project has no agent session");
  sessions.push({ ...base, id: "rel-agent", project_id: "p-rel", provider: "claude", display_name: "Relnotes agent", native_session_id: "fixture-rel" });

  // Overlapping presses make one shell, and a newer selection wins over a slow creation.
  for (let i = sessions.length - 1; i >= 0; i--) if (sessions[i].provider === "shell") sessions.splice(i, 1);
  await open("lesson-1", "Check that it works");
  await select("agent");
  const shellsBefore = sessions.filter((entry) => entry.provider === "shell").length;
  const inputsHeld = inputs.length;
  let release;
  holdCreate = { promise: new Promise((resolve) => { release = resolve; }) };
  createCount = 0;
  const press = button("Put in a terminal");
  await press.click();
  await press.click();
  await status().filter({ hasText: "already being opened" }).waitFor();
  assert.equal(createCount, 1, "a double press with a delayed response creates one shell");
  await select("agent2");
  await select("agent");
  release();
  holdCreate = null;
  await settle();
  await settle();
  assert.equal(sessions.filter((entry) => entry.provider === "shell").length, shellsBefore + 1, "the stale shell is still created");
  assert.equal(await page.locator('[data-session="agent"]').getAttribute("aria-selected"), "true", "returning to the original session stays selected");
  assert.equal(inputs.length, inputsHeld, "nothing is typed into the stale shell or the newer console");

  // An unconfirmed write is reported as such, and never retried.
  await open("lesson-1", "Ask {agent} to build it");
  await select("rel-agent");
  outcome = "uncertain";
  const uncertainBefore = inputs.length;
  await button("Put it in the terminal").click();
  await status().filter({ hasText: "Check the terminal for the text" }).waitFor();
  assert.equal(await status().filter({ hasText: "Press Return to send it" }).count(), 0, "an uncertain write is not called delivered");
  await settle();
  assert.equal(inputs.length, uncertainBefore + 1, "the uncertain write is not retried");
  outcome = "sent";

  // An earlier write awaiting acknowledgement must not stand in for the tutorial's own.
  await open("lesson-1", "Ask {agent} to build it");
  await select("rel-agent");
  holdAcks = true;
  await page.locator("[data-live-terminal]").click();
  await page.keyboard.type("z");
  await settle();
  const keystrokes = inputs.length;
  await button("Put it in the terminal").click();
  await until("the tutorial write", () => inputs.length === keystrokes + 1);
  await settle();
  assert.equal(heldAcks.length, 2, "the keystroke and the tutorial write both await acknowledgement");
  heldAcks.shift()("sent");
  await settle();
  assert.equal(await status().filter({ hasText: "Press Return to send it" }).count(), 0, "the keystroke's acknowledgement is not the tutorial's");
  heldAcks.shift()("uncertain");
  await status().filter({ hasText: "Check the terminal for the text" }).waitFor();
  assert.equal(await status().filter({ hasText: "Press Return to send it" }).count(), 0, "an uncertain write is never called delivered");
  holdAcks = false;

  // An acknowledgement that arrives after you have closed the tutorial and moved on leaves focus where you put it.
  await open("lesson-1", "Ask {agent} to build it");
  await select("rel-agent");
  holdAcks = true;
  const heldBefore = heldAcks.length;
  const writesBefore = inputs.length;
  await button("Put it in the terminal").click();
  await until("the tutorial write", () => inputs.length === writesBefore + 1);
  await button("Close tutorial").click();
  await page.locator('[data-session="rel-agent"]').focus();
  assert.equal(heldAcks.length, heldBefore + 1, "the tutorial write awaits acknowledgement");
  heldAcks.shift()("sent");
  await settle();
  assert.equal(await page.evaluate(() => document.activeElement?.getAttribute("data-session")), "rel-agent", "an acknowledgement after you moved on does not take focus");
  holdAcks = false;

  // A real emulator reply the server rejects must not shift the write count.
  await open("lesson-1", "Ask {agent} to build it");
  await select("rel-agent");
  sockets.get("rel-agent").send(JSON.stringify({ type: "output", data: Buffer.from("\x1b[?25$p").toString("base64") }));
  const untilReply = Date.now() + 5000;
  while (replies.length === 0 && Date.now() < untilReply) await settle();
  assert.ok(replies.some((reply) => reply.includes("$y")), "the emulator answered the mode query");
  await button("Put it in the terminal").click();
  await status().filter({ hasText: "Press Return to send it" }).waitFor();

  const beforeDialogs = inputs.length;
  // Dialog and panel actions.
  await open("lesson-3", "Bring your own code");
  await button("Add project…").click();
  await page.getByRole("dialog", { name: "Add project" }).waitFor();
  await page.keyboard.press("Escape");
  await open("logins", lesson("logins").steps.find((step) => step.action?.name === "git-identity").title);
  await panel.getByRole("button", { name: "Git identity…", exact: true }).click();
  await page.getByRole("dialog", { name: "Git identity" }).waitFor();
  await page.keyboard.press("Escape");
  await open("files", lesson("files").steps.find((step) => step.action?.name === "upload-files").title);
  await button("Upload files").click();
  await page.locator('[data-region="files"][data-open="true"]').waitFor();
  assert.equal(await panel.getAttribute("data-open"), "false", "Files hides the tutorial");
  await page.locator("[data-live-files]").click();
  await open("away", lesson("away").steps.find((step) => step.action?.name === "show-address").title);
  await button("Show the address").click();
  await status().filter({ hasText: origin }).waitFor();

  assert.equal(mutations.length, 2, "only the two shell session creations mutated anything");
  assert.equal(inputs.length, beforeDialogs, "no gesture, no input");

  // --- The practice projects ---
  unseedPair();
  await page.reload({ waitUntil: "networkidle" });
  mutations.length = 0;
  const writesOnly = () => mutations.map((m) => `${m.method} ${m.path}`);
  const lessonFileCount = (name) => readdirSync(new URL(`../src/app/tutorial/starters/${name}/`, import.meta.url)).length;
  const starterFile = (name, file) => readFileSync(new URL(`../src/app/tutorial/starters/${name}/${file}`, import.meta.url), "utf8");
  // Walks to a step with Back, Skip step and Continue, the way a user would.
  // A step whose event already happened completes on arrival, so the walk
  // may land past the target; Back returns to it.
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

  // Pick an agent through the New session dialog, as lesson 1 asks.
  await goTo("lesson-1", "Choose a coding agent");
  await button("Choose an agent…").click();
  assert.equal(await page.locator('[data-new-session-provider="shell"]').count(), 0, "the shell is not an agent");
  await page.locator('[data-new-session-provider="claude"]').click();
  await page.locator("[data-new-session-modal]").waitFor({ state: "detached" });
  await onStep("Create the practice projects");
  assert.deepEqual(writesOnly(), [], "picking an agent only records it and moves the lesson on");
  mutations.length = 0;
  const inputsBefore = inputs.length;

  // Create: both projects, one name, files, commit, tasks, manifest last, then the agent in relnotes.
  await goTo("lesson-1", "Create the practice projects");
  await button("Create relnotes and start claude").click();
  await status().filter({ hasText: "started claude in relnotes" }).waitFor({ state: "attached" });
  const dirs = mutations.filter((m) => m.path === "/api/project-directories/mkdir").map((m) => m.body.name);
  assert.deepEqual(dirs, ["ccparse", "relnotes"], "side by side, ccparse first");
  const created = mutations.filter((m) => m.path === "/api/projects").map((m) => ({ name: m.body.name, path: m.body.path, identity: m.body.git_identity_profile_id }));
  assert.deepEqual(created, [
    { name: "ccparse", path: "/workspace/ccparse", identity: "identity" },
    { name: "relnotes", path: "/workspace/relnotes", identity: "identity" },
  ], "registered with the default Git identity");
  const ids = Object.fromEntries(projects.map((entry) => [entry.name, entry.id]));
  assert.equal(fakeFiles.get(ids.ccparse).size, lessonFileCount("ccparse") + 1, "ccparse holds its starter files");
  assert.equal(fakeFiles.get(ids.relnotes).size, lessonFileCount("relnotes") + 2, "relnotes holds its starter files and the manifest");
  assert.equal(fakeFiles.get(ids.relnotes).get("relnotes.mjs"), starterFile("relnotes", "relnotes.mjs"), "plain names leave relnotes.mjs as it ships");
  const order = writesOnly();
  const lastUpload = order.map((entry, index) => [entry, index]).filter(([entry]) => entry.includes("/files?path=")).pop();
  assert.ok(lastUpload[0].includes("tutorial.json") && lastUpload[0].startsWith(`POST /api/projects/${ids.relnotes}/files`), "the manifest is the last file written");
  assert.equal(order.at(-1), `POST /api/projects/${ids.relnotes}/sessions`, "the session is the last mutation");
  assert.ok(order.indexOf(`POST /api/projects/${ids.relnotes}/starter-commit`) < order.indexOf(`POST /api/projects/${ids.relnotes}/taskmaster`), "committed before tasks are set up");
  assert.equal(order.filter((entry) => entry.endsWith("/starter-commit")).length, 2, "each project gets its starter commit");
  assert.deepEqual(mutations.find((m) => m.path.endsWith("/starter-commit") && m.path.includes(ids.ccparse)).body.paths.slice().sort(), readdirSync(new URL("../src/app/tutorial/starters/ccparse/", import.meta.url)).sort());
  assert.ok(prds.has("PRD.md"), "the PRD is uploaded as the project's PRD");
  const manifest = JSON.parse(fakeFiles.get(ids.relnotes).get("tutorial.json"));
  assert.deepEqual(manifest, {
    version: 1,
    relnotes: { projectId: ids.relnotes, canonicalPath: "/workspace/relnotes" },
    ccparse: { projectId: ids.ccparse, canonicalPath: "/workspace/ccparse" },
  });
  const startedSession = sessions.at(-1);
  assert.deepEqual([startedSession.project_id, startedSession.provider], [ids.relnotes, "claude"], "the picked agent runs in relnotes");
  await page.waitForFunction((id) => document.querySelector(`[data-session="${id}"]`)?.getAttribute("aria-selected") === "true", startedSession.id);
  await settle();
  assert.equal(inputs.length, inputsBefore, "creating the projects types nothing into any terminal");
  assert.equal(mutations.some((m) => m.method === "DELETE"), false, "nothing is ever deleted");

  // Pressing it again takes new names, so the first pair is never reseeded; relnotes.mjs names its sibling.
  mutations.length = 0;
  await goTo("lesson-1", "Create the practice projects");
  await button("Create relnotes and start claude").click();
  await status().filter({ hasText: "/workspace/relnotes-2 and /workspace/ccparse-2, and started claude" }).waitFor({ state: "attached" });
  assert.deepEqual(mutations.filter((m) => m.path === "/api/project-directories/mkdir").map((m) => m.body.name), ["ccparse-2", "relnotes-2"]);
  const second = projects.find((entry) => entry.name === "relnotes-2");
  const rewritten = fakeFiles.get(second.id).get("relnotes.mjs");
  assert.ok(rewritten.includes('"../ccparse-2/index.mjs"') && !rewritten.includes("../ccparse/"), "relnotes-2 imports ccparse-2");
  assert.equal(mutations.some((m) => m.path.includes(`/api/projects/${ids.relnotes}/`)), false, "the first pair is untouched");

  // A directory that is not empty is never written to.
  mutations.length = 0;
  faults.strayFile = "stray.txt";
  await button("Create relnotes and start claude").click();
  await status().filter({ hasText: "is not empty" }).waitFor({ state: "attached" });
  assert.deepEqual(writesOnly(), ["POST /api/project-directories/mkdir", "POST /api/projects"], "refused before any file was written");
  faults.strayFile = null;

  // A failure part way names the step, says nothing was deleted, and leaves no manifest.
  mutations.length = 0;
  faults.failUpload = "relnotes.mjs";
  await button("Create relnotes and start claude").click();
  await status().filter({ hasText: "uploading the starter files (relnotes-4)" }).waitFor({ state: "attached" });
  const failure = await status().textContent();
  assert.ok(failure.includes("nothing was deleted"), failure);
  assert.equal(mutations.some((m) => m.method === "DELETE" || m.path.endsWith("/sessions")), false, "no removal and no session after a failure");
  const incomplete = projects.find((entry) => entry.name === "relnotes-4");
  assert.equal(fakeFiles.get(incomplete.id).has("tutorial.json"), false, "an incomplete project has no manifest");
  faults.failUpload = null;

  // Generation asks for two tasks, follows the run record, and reports the result.
  await goTo("lesson-1", "Turn the PRD into tasks");
  const tasksHost = page.locator('[data-region="tasks"]');
  assert.equal(await tasksHost.getAttribute("data-open"), "true", "arriving on the step opens Tasks");
  await page.locator("[data-live-tasks]").click();
  await page.waitForFunction(() => document.querySelector('[data-region="tasks"]')?.getAttribute("data-open") === "false");
  await page.evaluate(() => new Promise((resolve) => requestAnimationFrame(() => requestAnimationFrame(resolve))));
  assert.equal(await tasksHost.getAttribute("data-open"), "false", "Tasks opened once: closing it on the same step keeps it closed");
  assert.equal(await panel.getAttribute("data-open"), "true", "closing Tasks returns the full lesson page");
  await panel.getByText("Uses your claude plan.", { exact: true }).waitFor();
  mutations.length = 0;
  await button("Add PRD… → Generate tasks").click();
  await status().filter({ hasText: "Generated 2 tasks." }).waitFor({ state: "attached" });
  const generate = mutations.find((m) => m.path.endsWith("/taskmaster/generate"));
  assert.equal(generate.path, `/api/projects/${second.id}/taskmaster/generate`, "generation runs in the newest complete relnotes, not the incomplete ones");
  assert.deepEqual(generate.body, { prd: "PRD.md", numTasks: 2, harness: "claude" }, "generation uses the picked agent and task count");
  assert.equal(tasks.length, 2);
  assert.equal(inputs.length, inputsBefore, "no terminal input from generation");

  // The by-hand path adds the fixed task for one requirement, and the second needs the first.
  tasks.length = 0;
  // Generating the tasks moved the lesson on by itself, so the saved step is the one after. Save the
  // Tasks step again: with no tasks its event does not hold, and the reload resumes on it.
  record = { ...record, lesson: "lesson-1", step: stepIndex("lesson-1", "Turn the PRD into tasks") };
  await page.reload({ waitUntil: "networkidle" });
  // The saved step opens Tasks, so the reload resumes folded into the bar.
  await bar.waitFor({ state: "visible" });
  await open("lesson-2", "The second task");
  const second2 = button("Add the second task by hand");
  await second2.waitFor();
  mutations.length = 0;
  await second2.click();
  await status().filter({ hasText: "requirement 1 first" }).waitFor({ state: "attached" });
  assert.equal(mutations.some((m) => m.path.endsWith("/taskmaster/tasks")), false, "requirement 2 waits for requirement 1");
  await open("lesson-1", "Turn the PRD into tasks");
  await button("Add the first task by hand").click();
  await status().filter({ hasText: "requirement 1" }).waitFor({ state: "attached" });
  const byHand = mutations.filter((m) => m.path.endsWith("/taskmaster/tasks"));
  assert.equal(byHand.length, 1);
  assert.equal(byHand[0].body.title, "Print the Breaking changes section");
  await open("lesson-2", "The second task");
  await button("Add the second task by hand").click();
  await status().filter({ hasText: "requirement 2" }).waitFor({ state: "attached" });
  const both = mutations.filter((m) => m.path.endsWith("/taskmaster/tasks"));
  assert.equal(both.length, 2);
  assert.ok(both[1].body.details.includes("Depends on the task for requirement 1"));
  await panel.getByRole("button", { name: "Add the second task by hand", exact: true }).waitFor({ state: "detached" });

  // start-session starts the picked agent in ccparse and selects it. The agent is lost on reload, so pick again.
  await goTo("lesson-1", "Choose a coding agent");
  await button("Choose an agent…").click();
  await page.locator('[data-new-session-provider="claude"]').click();
  await page.locator("[data-new-session-modal]").waitFor({ state: "detached" });
  await goTo("lesson-2", "Start the other agent");
  mutations.length = 0;
  const target = projects.find((entry) => entry.name === "ccparse-2");
  await button("Start claude in ccparse").click();
  await status().filter({ hasText: "Started claude in ccparse" }).waitFor({ state: "attached" });
  assert.deepEqual(writesOnly(), [`POST /api/projects/${target.id}/sessions`]);
  assert.equal(mutations[0].body.provider, "claude");
  const ccSession = sessions.at(-1);
  await page.waitForFunction((id) => document.querySelector(`[data-session="${id}"]`)?.getAttribute("aria-selected") === "true", ccSession.id);
  await settle();
  assert.equal(inputs.length, inputsBefore, "starting the session types nothing");

  // --- Lesson 1 by events, from a clean installation ---
  projects.length = 1;
  sessions.length = 1;
  tasks.length = 0;
  for (const collection of [fakeFiles, directories, prds, runs, modifiedFiles]) collection.clear();
  mutations.length = 0;
  inputs.length = 0;
  faults.defaultIdentity = null;
  record = { version: 1, dismissed: false, lesson: "lesson-1", step: 0, done: [], scope: "user" };
  await page.reload({ waitUntil: "networkidle" });
  await panel.waitFor({ state: "visible" });
  await button("Start the tutorial").click();
  const noNext = async (why) => assert.equal(await panel.locator('[data-tutorial-action="next"]').count(), 0, `no Next button ${why}`);
  await onStep("Choose a coding agent");
  await noNext("on a step that asks for an action");
  assert.equal(await button("Back").isDisabled(), true, "Back stops at the first step");

  // Skip step is always there and moves on without the event.
  await button("Skip step").click();
  await onStep("Create the practice projects");
  await panel.getByRole("button", { name: "Set a Git identity…", exact: true }).waitFor();
  mutations.length = 0;
  await button("Create relnotes and start your agent").click();
  // Skipping the agent step does not strand the user: creating offers the choice right here.
  await page.locator("[data-new-session-modal]").waitFor();
  assert.deepEqual(mutations, [], "offering the choice creates nothing");
  await page.locator('[data-new-session-provider="claude"]').click();
  await status().filter({ hasText: "No default Git identity is set" }).waitFor({ state: "attached" });
  assert.deepEqual(mutations, [], "nothing is created without an identity");

  // Back to a step that was skipped does not complete it by itself; doing the thing does.
  faults.defaultIdentity = "identity";
  await page.reload({ waitUntil: "networkidle" });
  await panel.waitFor({ state: "visible" });
  await onStep("Create the practice projects");
  await button("Back").click();
  await onStep("Choose a coding agent");
  await settle();
  await onStep("Choose a coding agent");
  await button("Choose an agent…").click();
  await page.locator('[data-new-session-provider="claude"]').click();
  await page.locator("[data-new-session-modal]").waitFor({ state: "detached" });
  await onStep("Create the practice projects");
  await noNext("on the create step");

  // Creating the practice projects moves the lesson on to the Tasks step.
  mutations.length = 0;
  await button("Create relnotes and start claude").click();
  await onStep("Turn the PRD into tasks");
  await noNext("on the tasks step");
  const relnotes = projects.find((entry) => entry.name === "relnotes");
  assert.ok(relnotes && mutations.some((m) => m.path === `/api/projects/${relnotes.id}/sessions`), "the agent was started in relnotes");
  await status().filter({ hasText: "started claude in relnotes" }).waitFor({ state: "attached" });

  // With another project selected, the tutorial reads the practice tasks itself
  // every few seconds while the step waits for one.
  await select("agent");
  fakeFiles.get(relnotes.id).set("notes.txt", "n");
  modifiedFiles.add(`${relnotes.id}/relnotes.mjs`);
  const polls = relnotesTaskReads;
  await until("a task poll", () => relnotesTaskReads > polls, 8000);
  tasks.push({ id: "1", title: "Generated 1", description: "", status: "pending" }, { id: "2", title: "Generated 2", description: "", status: "pending" });
  await onStep("Ask {agent} to build it");
  const quiet = relnotesTaskReads;
  await new Promise((resolve) => setTimeout(resolve, 4500));
  assert.equal(relnotesTaskReads, quiet, "no task reads while the step waits for something else");

  // Whether the agent is working is not observable, so this step waits on Continue
  // however much the terminal prints after the insert.
  assert.equal(await panel.locator('[data-tutorial-action="skip"]').count(), 0, "the prompt step offers Continue, not Skip step");
  await button("Put it in the terminal").click();
  await status().filter({ hasText: "press Return to send it" }).waitFor({ state: "attached" });
  const relnotesAgent = sessions.find((entry) => entry.project_id === relnotes.id).id;
  assert.equal(inputs.at(-1).session, relnotesAgent, "the prompt went to the relnotes agent while Practice was selected");
  const sendOutput = (text) => sockets.get(relnotesAgent).send(JSON.stringify({ type: "output", data: Buffer.from(text).toString("base64") }));
  for (let i = 0; i < 10; i++) {
    sendOutput(`working ${i}\r\n`);
    await new Promise((resolve) => setTimeout(resolve, 400));
  }
  await onStep("Ask {agent} to build it");
  await button("Continue").click();
  await onStep("See what changed");
  await noNext("on the files step");

  // Only a file the listing marks as modified counts.
  await page.locator('[data-region="files"][data-open="true"]').waitFor();
  await page.locator('[data-files-row="notes.txt"]').waitFor();
  await page.locator('[data-files-row="notes.txt"]').click();
  await settle();
  await onStep("See what changed");
  await page.locator('[data-files-row="relnotes.mjs"]').click();
  await onStep("Check that it works");

  // Deck cannot read a command's exit status, so this step stays on Continue and shows neither result.
  await reveal();
  assert.equal(await button("Continue").count(), 1, "the check step offers Continue");
  assert.equal(await panel.locator('[data-tutorial-action="skip"]').count(), 0, "and not Skip step");
  assert.equal(await panel.getByText("2 tests passed").count() + await panel.getByText("1 test failed").count(), 0, "neither result text is shown");
  await button("Continue").click();
  await onStep("Read the diff and close the task");
  await noNext("on the last action step");

  // Mark task done writes the status of the requirement's task with no lane, and the real status moves the lesson on.
  mutations.length = 0;
  await reveal();
  await button("Mark task done").click();
  await onStep("Done");
  const closeTask = mutations.filter((m) => m.path.endsWith("/done"));
  assert.deepEqual(closeTask.map((m) => [m.path, m.body]), [[`/api/projects/${relnotes.id}/taskmaster/tasks/1/done`, { tag: "master" }]], "lesson 1 marks the first task done in relnotes, with no lane");
  assert.equal(tasks[0].status, "done");
  assert.equal(tasks[1].status, "pending", "the other task is untouched");
  assert.equal(mutations.some((m) => m.path.includes("/lanes")), false, "no lane is involved");
  await until("the lesson to be recorded", () => record.done.includes("lesson-1"));
  assert.equal(await panel.locator('[data-tutorial-action="skip"]').count(), 0, "the done page has no Skip step");

  // --- Lesson 3: a lane, a review and a close, through the lane endpoints ---
  projects.push({ id: ownId, name: "mycode", canonical_path: "/workspace/mycode", last_used_at: "t", git_identity_setup_revision: 1 });
  sessions.push({ ...base, id: "own-agent", project_id: ownId, provider: "claude", display_name: "Own agent", native_session_id: "fixture" });
  record = { version: 1, dismissed: false, lesson: "lesson-1", step: 0, done: [], scope: "user" };
  await page.reload({ waitUntil: "networkidle" });
  await panel.waitFor({ state: "visible" });
  await button("Start the tutorial").click();
  await goTo("lesson-1", "Choose a coding agent");
  await button("Choose an agent…").click();
  await page.locator('[data-new-session-provider="claude"]').click();
  await page.locator("[data-new-session-modal]").waitFor({ state: "detached" });
  await goTo("lesson-3", "Give it a branch");
  const ownToggle = page.locator(`[data-project-toggle="${ownId}"]`);
  if (await ownToggle.getAttribute("aria-expanded") === "false") await ownToggle.click();
  await select("own-agent");
  await reveal();
  mutations.length = 0;
  inputs.length = 0;
  await button("Start a lane").click();
  await onStep("Ask for the change");
  assert.deepEqual(writesOnly(), [`POST /api/projects/${ownId}/lanes`, "POST /api/projects/p-lane/sessions"], "a lane, then the picked agent in it");
  assert.deepEqual(mutations[0].body, { tag: "main", task: 1, agent: "claude" }, "the lane is for the next open task");
  assert.equal(mutations[1].body.provider, "claude");
  assert.equal(JSON.stringify(mutations).toLowerCase().includes("token"), false, "no capability crosses the wire");
  assert.equal(inputs.length, 0, "starting a lane types nothing");
  const authorSession = sessions.at(-1);
  await page.waitForFunction((id) => document.querySelector(`[data-session="${id}"]`)?.getAttribute("aria-selected") === "true", authorSession.id);

  // The prompt names the lane's task and is typed into the lane's session without Return.
  await button("Put it in the terminal").click();
  await status().filter({ hasText: "press Return to send it" }).waitFor({ state: "attached" });
  await settle();
  const changePrompt = inputs.at(-1);
  assert.equal(changePrompt.session, authorSession.id, "typed into the lane's agent");
  assert.ok(changePrompt.text.includes('"Make the release notes shorter"') && changePrompt.text.includes("Cut the preamble."), changePrompt.text);
  assert.equal(/[\r\n\x1b]/.test(changePrompt.text), false, "no Return and no control bytes");
  await forward.click();
  await onStep("A reviewer that isn't the author");

  providerList = ["claude", "shell"];
  // With one agent available there is nothing to ask, and the step says where to go.
  mutations.length = 0;
  await button("Request review").click();
  await status().filter({ hasText: "only agent available" }).waitFor({ state: "attached" });
  assert.match(await status().textContent(), /Skip step/);
  assert.deepEqual(writesOnly(), [], "no reviewer can be started");

  // With another agent, the first press starts it; the second types the review prompt.
  providerList = ["claude", "codex", "shell"];
  const typedBefore = inputs.length;
  await button("Request review").click();
  await status().filter({ hasText: "choose Request review again" }).waitFor({ state: "attached" });
  assert.deepEqual(writesOnly(), ["POST /api/projects/p-lane/sessions"]);
  assert.equal(mutations[0].body.provider, "codex", "a different agent than the one that wrote the change");
  assert.match(await status().textContent(), /not a formal approval/);
  const reviewSession = sessions.at(-1);
  await page.waitForFunction((id) => document.querySelector(`[data-session="${id}"]`)?.getAttribute("aria-selected") === "true", reviewSession.id);
  await settle();
  assert.equal(inputs.length, typedBefore, "starting the reviewer types nothing");
  await button("Request review").click();
  await status().filter({ hasText: "review prompt" }).filter({ hasText: "press Return to send it" }).waitFor({ state: "attached" });
  await settle();
  assert.deepEqual(writesOnly(), ["POST /api/projects/p-lane/sessions", `POST /api/projects/${ownId}/lanes/main/1/review`]);
  assert.deepEqual(mutations[1].body, { agent: "codex" });
  const reviewPromptTyped = inputs.at(-1);
  assert.equal(reviewPromptTyped.session, reviewSession.id, "typed into the reviewer, not the author");
  for (const part of ["gh pr view", "gh pr diff", "gh pr comment", "Make the release notes shorter"]) assert.ok(reviewPromptTyped.text.includes(part), part);
  assert.equal(/[\r\n\x1b]/.test(reviewPromptTyped.text), false, "no Return and no control bytes");
  assert.match(await status().textContent(), /not a formal approval/);

  // The step finishes when GitHub shows a comment after the request.
  await new Promise((resolve) => setTimeout(resolve, 600));
  await onStep("A reviewer that isn't the author");
  laneFaults.reviewSeen = true;
  await until("the review to be seen", async () => /^Finish$/.test(await heading()), 20000);

  // Closing asks before it stops sessions, then removes the lane and marks the task done.
  mutations.length = 0;
  await button("Mark task done").click();
  await status().filter({ hasText: "Choose Mark task done again to stop them" }).waitFor({ state: "attached" });
  assert.equal(ownTasks[0].status, "pending", "a refused close changes nothing");
  // The first task read after the close fails; the step must still finish from the next one.
  laneFaults.failOwnReads = 1;
  await button("Mark task done").click();
  await until("the lane to close", async () => ownTasks[0].status === "done" && lanes.length === 0);
  const closes = mutations.filter((m) => m.path.endsWith("/done"));
  assert.deepEqual(closes.map((m) => [m.path, m.body]), [
    [`/api/projects/${ownId}/taskmaster/tasks/1/done`, { tag: "main" }],
    [`/api/projects/${ownId}/taskmaster/tasks/1/done`, { tag: "main", stop_sessions: true }],
  ], "the second press carries the consent the first one asked for");
  await until("the finish step to complete", async () => !/^Finish$/.test(await heading()));
  // The heading moves before its save reaches the mock; wait so that save
  // cannot overwrite the next scenario's reset to lesson 1.
  await until("lesson 3 progress to be saved", () => record.lesson === "lesson-3" && record.step > stepIndex("lesson-3", "Finish"));

  console.log("ok: lesson 3 starts a lane for the next task and opens the agent in it, asks a different agent for the review (second press types it, never Return, never into the author), follows the review to Finish and closes the lane after asking before it stops sessions; lesson 1 marks its task done with no lane; lesson 1 advances by events (agent, starter, task poll, insert and sustained output, modified file, task done) with Skip step, Continue for the unobserved check, and no Next on action steps; practice projects created side by side with a suffix pair, files uploaded, starter commit, manifest last, session in relnotes; no reseed; failure recovery; generate numTasks 2; by-hand tasks; start-session in ccparse; no terminal input without a gesture; existing dialog, prompt and command actions");
  providerList = ["claude", "codex", "shell"];

  // Codex opens with a sign-in helper, so creating the practice projects stops at the sign-in step.
  projects.length = 1;
  sessions.length = 1;
  tasks.length = 0;
  for (const collection of [fakeFiles, directories, prds, runs, modifiedFiles]) collection.clear();
  faults.codex = true;
  record = { version: 1, dismissed: false, lesson: "lesson-1", step: 0, done: [], scope: "user" };
  await page.reload({ waitUntil: "networkidle" });
  await panel.waitFor({ state: "visible" });
  await button("Start the tutorial").click();
  await button("Choose an agent…").click();
  await page.locator('[data-new-session-provider="codex"]').click();
  await page.locator("[data-new-session-modal]").waitFor({ state: "detached" });
  await onStep("Create the practice projects");
  // A second press while the first create waits for its session changes nothing.
  let releaseSession;
  faults.hold = new Promise((resolve) => { releaseSession = resolve; });
  await button("Create relnotes and start codex").click();
  await until("the held session request", () => mutations.some((m) => m.path.endsWith("/sessions") && m.body?.provider === "codex" && projects.some((p) => p.name === "relnotes" && m.path.includes(p.id))));
  await button("Create relnotes and start codex").click();
  await settle();
  await onStep("Create the practice projects");
  faults.hold = null;
  releaseSession();
  await onStep("Sign in to codex");
  await settle();
  await onStep("Sign in to codex");
  await page.locator("[data-harness-login-dialog]").waitFor();
  await page.locator("[data-harness-login-dialog] [data-login-close]").first().click();
  await page.locator("[data-harness-login-dialog]").waitFor({ state: "detached" });
  await noNext("on the sign-in step");
  await button("Skip step").click();
  await onStep("Turn the PRD into tasks");
  faults.codex = false;

  assert.deepEqual(goneRequests.filter((entry) => !entry.startsWith("GET ")), [], "nothing is written to a removed project");
  assert.deepEqual(errors, []);
  console.log("ok: lesson 3 lane, review and close through the lane endpoints; lesson 1 marks its task done with no lane; lesson 1 advances by events (agent, starter, task poll, insert then Continue, modified file, task done) with Skip step, Continue for the unobserved check, and no Next on action steps; practice projects created side by side with a suffix pair, files uploaded, starter commit, manifest last, session in relnotes; no reseed; failure recovery; generate numTasks 2; by-hand tasks; start-session in ccparse; no terminal input without a gesture; existing dialog, prompt and command actions");
  await context.close();
} finally {
  await browser?.close();
  vite.kill("SIGTERM");
}
