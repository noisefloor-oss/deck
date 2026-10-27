// Real-browser probe for asking for a Git identity only when one is needed:
// none on file, one, several, and GitHub connected or not.
import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import net from "node:net";
import { browser as connectBrowser } from "./browser.mjs";
import { exactlyOnce } from "./eventually.mjs";

const port = await new Promise((resolve, reject) => {
  const server = net.createServer();
  server.once("error", reject);
  server.listen(0, "127.0.0.1", () => {
    const address = server.address();
    server.close(() => resolve(address.port));
  });
});
const origin = `http://127.0.0.1:${port}`;
let serverOutput = "";
const vite = spawn("pnpm", ["exec", "vite", "--host", "127.0.0.1", "--port", String(port), "--strictPort"], {
  cwd: new URL("..", import.meta.url), stdio: ["ignore", "pipe", "pipe"],
});
vite.stdout.on("data", (chunk) => { serverOutput += chunk; });
vite.stderr.on("data", (chunk) => { serverOutput += chunk; });

let browser;
try {
  for (let attempt = 0; attempt < 100; attempt++) {
    try {
      const response = await fetch(origin);
      if (response.ok) break;
    } catch {}
    await new Promise((resolve) => setTimeout(resolve, 50));
    if (attempt === 99) throw new Error(`Vite did not start:\n${serverOutput}`);
  }
  ({ browser } = await connectBrowser());
  const context = await browser.newContext({ viewport: { width: 1000, height: 760 } });
  const page = await context.newPage();
  const pageErrors = [];
  page.on("pageerror", (error) => pageErrors.push(error.message));

  const profile = {
    id: "identity-1", name: "Deck Author", email: "deck@example.invalid", revision: 1,
    created_at: "2026-08-13T00:00:00Z", updated_at: "2026-08-13T00:00:00Z",
  };
  const second = { ...profile, id: "identity-2", name: "Second Author", email: "second@example.invalid" };
  const octo = { name: "Octo Cat", email: "583231+octocat@users.noreply.github.com" };
  const octoProfile = { ...profile, id: "identity-3", ...octo };
  let catalog = { profiles: [], default_profile_id: null, revision: 0 };
  let githubAccount = null;
  let githubHold = null;
  // The connection status the identity form reads on open.
  let statusLogin = null;
  let statusHold = null;
  let statusFails = false;
  let providersHold = null;
  const identityWrites = [];
  const kappaSession = {
    id: "session-kappa", project_id: "kappa", provider: "cline", display_name: "",
    status: "running", generation: 1, created_at: "2026-08-13T00:00:00Z", updated_at: "2026-08-13T00:00:00Z",
    last_output_at: "2026-08-13T00:00:00Z", recovery_capability: "none", recovery_state: "unavailable", presentation_state: "running",
  };
  const expectedCreate = { current: profile };
  const setupRevisions = { alpha: 0, beta: 0, gamma: 0, delta: 0, epsilon: 0, zeta: 0, eta: 0 };
  const setupWrites = [];
  const setupHolds = {};
  const launches = { alpha: 0, beta: 0, gamma: 0, delta: 0, epsilon: 0, zeta: 0, eta: 0 };
  const created = [];
  const project = (id) => ({
    id, name: id, canonical_path: `/workspace/${id}`,
    git_identity_setup_revision: setupRevisions[id], last_used_at: "2026-08-13T00:00:00Z",
  });

  await page.route(`${origin}/auth/session`, (route) => route.fulfill({
    status: 204, body: "", headers: { "X-Noisedeck-Auth-Mode": "trusted_proxy" },
  }));
  await page.route(`${origin}/api/**`, async (route) => {
    const request = route.request();
    const url = new URL(request.url());
    const path = url.pathname;
    const json = (status, body) => route.fulfill({ status, contentType: "application/json", body: JSON.stringify(body) });
    const method = request.method();
    if (method === "GET" && path === "/api/settings/tutorial") return json(200, { version: 1, dismissed: true, lesson: "welcome", step: 0, done: [], scope: "user" });
    if (method === "GET" && path === "/api/settings") {
      return json(200, { skip_permissions: false, skip_close_confirm: false, skip_remove_project_confirm: false });
    }
    if (method === "GET" && path === "/api/session-activity/events") {
      return route.fulfill({ status: 200, contentType: "text/event-stream", body: ": connected\n\n" });
    }
    if (method === "GET" && path === "/api/github/account") {
      if (githubHold !== null) await githubHold;
      return json(200, githubAccount === null ? { outcome: "authentication-required", account: "" } : { outcome: "ok", account: "octocat", ...githubAccount });
    }
    if (method === "GET" && path === "/api/github/connect/status") {
      if (statusHold !== null) await statusHold;
      if (statusFails) return json(502, { error: "unavailable" });
      return json(200, statusLogin === null ? { outcome: "no-account", active: false } : { outcome: "authenticated", login: statusLogin, source: "stored", active: true });
    }
    if (method === "GET" && path === "/api/github/connect/attempt") return json(200, { csrf: "connect-fixture", attempt: null });
    if (method === "GET" && path === "/api/git-identities") return json(200, catalog);
    if (method === "POST" && path === "/api/git-identities") {
      const made = expectedCreate.current;
      assert.deepEqual(request.postDataJSON(), { name: made.name, email: made.email });
      catalog = { profiles: [...catalog.profiles, made], default_profile_id: catalog.default_profile_id ?? made.id, revision: catalog.revision + 1 };
      return json(201, made);
    }
    if (method === "PUT" && path.startsWith("/api/git-identities/")) identityWrites.push(path);
    if (method === "GET" && path === "/api/projects") return json(200, [...Object.keys(setupRevisions).map(project), ...created]);
    if (method === "GET" && path === "/api/projects/inspect") {
      const input = url.searchParams.get("input");
      return json(200, { kind: "absent", path: input, exists: false, entries: [], store_path: `${input}/.git/noise-tasks/tasks.sqlite` });
    }
    if (method === "POST" && path === "/api/projects") {
      const body = request.postDataJSON();
      const made = { id: "fresh", name: body.name, canonical_path: body.path, git_identity_setup_revision: 1, last_used_at: "t", git_identity_profile_id: body.git_identity_profile_id };
      created.push(made);
      return json(201, made);
    }
    if (method === "GET" && path === "/api/project-clones") return json(200, []);
    const sessions = /^\/api\/projects\/([^/]+)\/sessions$/.exec(path);
    if (method === "GET" && sessions !== null) return json(200, sessions[1] === "kappa" ? [kappaSession] : []);
    if (method === "POST" && sessions !== null) {
      const id = sessions[1];
      launches[id]++;
      assert.deepEqual(request.postDataJSON(), { provider: "cline" });
      if (setupRevisions[id] === 0) return json(409, { error: "git_identity_required", message: "Git identity setup is required" });
      return json(201, {
        id: `session-${id}`, project_id: id, provider: "cline", display_name: "",
        status: "starting", generation: 1, created_at: "2026-08-13T00:00:00Z", updated_at: "2026-08-13T00:00:00Z",
        recovery_capability: "none", recovery_state: "automatic_pending", presentation_state: "recovering",
      });
    }
    const setup = /^\/api\/projects\/([^/]+)\/git-identity$/.exec(path);
    if (method === "PUT" && setup !== null) {
      const id = setup[1];
      const body = request.postDataJSON();
      setupWrites.push({ project: id, profile: body.profile_id });
      if (setupHolds[id] !== undefined) await setupHolds[id];
      assert.equal(body.expected_project_setup_revision, 0);
      setupRevisions[id] = 1;
      return json(200, {
        ...project(id),
        ...(id === "alpha" ? { replaced_git_identity: { name: "Repository Author", email: "repository@example.invalid" } } : {}),
      });
    }
    // The probe launches cline through the provider picker, so the served
    // inventory must admit it.
    if (method === "GET" && path === "/api/providers") {
      if (providersHold !== null) await providersHold;
      return json(200, { providers: ["cline"] });
    }
    return json(404, { error: "not_found", message: path });
  });

  const modal = page.locator('[role="dialog"][aria-modal="true"]');
  const infoBar = page.locator("[data-info-status]");
  // A hover hint outranks the bar's resting sentence, so the pointer rests
  // on the bar itself while it is read.
  const restPointer = async () => {
    const box = await page.locator('[data-region="info-bar"]').boundingBox();
    await page.mouse.move(box.x + box.width / 2, box.y + box.height / 2);
  };
  const startCline = async (id) => {
    const row = page.locator(`[data-project="${id}"]`);
    await row.locator("[data-project-toggle]").hover();
    await row.locator("[data-project-actions] [data-new-session]").click();
    await page.locator('[data-new-session-provider="cline"]').click();
  };

  // No identity on file, and no dialog until something needs one.
  await page.goto(origin, { waitUntil: "networkidle" });
  await page.locator('[data-project="alpha"]').waitFor();
  await page.waitForTimeout(300);
  assert.equal(await modal.count(), 0, "the app opens without asking for a Git identity");

  // None, GitHub not connected: the session start asks with the form.
  await startCline("alpha");
  const form = page.locator("[data-git-identity-onboarding]");
  await form.waitFor();
  for (const label of ["Author name", "Author email"]) {
    assert.equal(await form.getByText(label, { exact: true }).evaluate((el) => getComputedStyle(el).fontSize), "13px",
      `${label} uses the label type token`);
  }
  await form.getByRole("button", { name: "Connect GitHub", exact: true }).waitFor();
  assert.equal(await form.locator("[data-git-identity-note]").innerText(), "Cline starts as soon as you save this.");
  assert.equal(await form.getByRole("textbox", { name: "Git author name" }).inputValue(), "");
  await form.getByRole("textbox", { name: "Git author name" }).fill(profile.name);
  await form.getByRole("textbox", { name: "Git author email" }).fill(profile.email);
  await form.getByRole("button", { name: "Save identity" }).click();
  await form.waitFor({ state: "detached" });
  // What the write replaced is stated in the rail, where you can read it
  // and put the old value back by hand.
  const notice = page.locator("[data-identity-notice]");
  await notice.waitFor();
  assert.match(await notice.innerText(), /Repository Author/);
  assert.match(await notice.innerText(), /repository@example\.invalid/);
  await exactlyOnce([
    { read: () => setupWrites.filter((w) => w.project === "alpha").length, expected: 1, message: "the new identity is applied to the project once" },
    { read: () => launches.alpha, expected: 2, message: "the waiting session starts exactly once after saving" },
  ]);

  // With an identity on file the settings row opens the manager.
  await page.locator("[data-live-more]").click();
  await page.locator("[data-more-popover]").waitFor();
  await page.getByRole("button", { name: "Git identity…" }).click();
  const manager = page.locator("[data-git-identity-settings]");
  await manager.waitFor();
  assert.equal(await form.count(), 0, "the settings row reopened the first-identity form although an identity exists");
  await page.getByRole("dialog", { name: "Git identity" }).getByRole("button", { name: "Close" }).click();
  await manager.waitFor({ state: "detached" });

  // One identity: applied without asking, and said in the info bar.
  await startCline("beta");
  await restPointer();
  await page.waitForFunction(() => document.querySelector("[data-info-status]")?.textContent === "Commits as Deck Author <deck@example.invalid>");
  assert.equal(await modal.count(), 0, "one identity is never a question");
  await exactlyOnce([
    { read: () => setupWrites.filter((w) => w.project === "beta" && w.profile === profile.id).length, expected: 1, message: "the only identity is applied once" },
    { read: () => launches.beta, expected: 2, message: "the session starts once the identity is applied" },
  ]);

  // The launch stays in progress while the identity is written and the
  // session retried, so a second project cannot start in between.
  catalog = { profiles: [profile], default_profile_id: profile.id, revision: catalog.revision + 1 };
  await page.reload({ waitUntil: "networkidle" });
  let releaseSetup;
  setupHolds.zeta = new Promise((resolve) => { releaseSetup = resolve; });
  const writing = page.waitForRequest((request) => request.method() === "PUT" && new URL(request.url()).pathname === "/api/projects/zeta/git-identity");
  await startCline("zeta");
  await writing;
  const picker = page.locator("[data-new-session-modal]");
  assert.equal(await picker.count(), 1, "the picker stays open while the identity is written");
  assert.equal(await picker.locator("[data-new-session-provider]").evaluateAll((buttons) => buttons.every((b) => b.disabled)), true,
    "every launch choice stays disabled while the identity is written");
  await page.keyboard.press("Escape");
  assert.equal(await picker.count(), 1, "Escape does not drop a launch in progress");
  await page.evaluate(() => {
    document.querySelector('[data-project="eta"] [data-new-session]')?.click();
    document.querySelector('[data-new-session-provider="cline"]')?.click();
  });
  delete setupHolds.zeta;
  releaseSetup();
  await picker.waitFor({ state: "detached" });
  await exactlyOnce([
    { read: () => setupWrites.filter((w) => w.project === "zeta").length, expected: 1, message: "the delayed identity is applied once" },
    { read: () => launches.zeta, expected: 2, message: "the delayed session starts once the identity is applied" },
    { read: () => setupWrites.filter((w) => w.project === "eta").length, expected: 0, message: "no second identity write starts during the first" },
    { read: () => launches.eta, expected: 0, message: "no second launch starts during the first" },
  ]);
  await startCline("eta");
  await exactlyOnce([
    { read: () => setupWrites.filter((w) => w.project === "eta").length, expected: 1, message: "the next project's identity is applied once" },
    { read: () => launches.eta, expected: 2, message: "the next project's session starts once the guard is released" },
  ]);

  // Several with no default: a real choice, so the project screen asks.
  catalog = { profiles: [profile, second], default_profile_id: null, revision: catalog.revision + 1 };
  await page.reload({ waitUntil: "networkidle" });
  await startCline("gamma");
  const choose = page.locator("[data-project-git-identity]");
  await choose.waitFor();
  assert.equal(await choose.getAttribute("aria-label"), "Git identity");
  assert.equal(await choose.locator(".dialog-title").innerText(), "Git identity");
  assert.equal(await choose.locator("[data-project-identity-waiting]").innerText(), "Cline starts as soon as you apply this.");
  assert.equal(await choose.locator(".bg-wash-selected").count(), 0, "no notice box");
  {
    const label = choose.locator("label > span").first();
    assert.equal(await label.innerText(), "Git identity", "the label reads in sentence case");
    assert.equal(await label.evaluate((node) => getComputedStyle(node).fontVariantCaps), "normal");
    assert.equal(await label.evaluate((node) => getComputedStyle(node).textTransform), "none");
  }
  // The DS select chrome (bp-selectwrap): the native picker glyph is
  // suppressed, the wrap paints the 15px chevron at the 14px trailing
  // inset, pointer-inert, and the select reserves 37px so the value never
  // runs under it.
  {
    const select = choose.locator("[data-project-identity-profile]");
    const field = await select.evaluate((node) => {
      const s = getComputedStyle(node);
      return { appearance: s.appearance, paddingRight: s.paddingRight };
    });
    assert.equal(field.appearance, "none", "the native select glyph is suppressed");
    assert.equal(field.paddingRight, "37px", "the value reserves the DS trailing padding");
    const chevron = await select.evaluate((node) => {
      const wrap = node.parentElement;
      const svg = wrap?.querySelector("svg");
      if (!svg) return null;
      const wrapBox = wrap.getBoundingClientRect();
      const svgBox = svg.getBoundingClientRect();
      return {
        inset: Math.round(wrapBox.right - svgBox.right),
        size: svg.getAttribute("width"),
        pointer: getComputedStyle(svg.parentElement).pointerEvents,
      };
    });
    assert.ok(chevron !== null, "the selectwrap paints a chevron");
    assert.equal(chevron.size, "15", "the chevron is the 15px DS glyph");
    assert.equal(chevron.inset, 14, "the chevron sits at the 14px field inset");
    assert.equal(chevron.pointer, "none", "the chevron is pointer-inert");
    await select.selectOption(second.id);
  }
  for (const button of await choose.locator("button").all()) {
    assert.ok(await button.getAttribute("data-hint"), `${await button.getAttribute("aria-label") ?? await button.innerText()} has a hint`);
  }
  await choose.getByRole("button", { name: "Apply identity" }).click();
  await choose.waitFor({ state: "detached" });
  await exactlyOnce([
    { read: () => setupWrites.filter((w) => w.project === "gamma" && w.profile === second.id).length, expected: 1, message: "the chosen identity is applied once" },
    { read: () => launches.gamma, expected: 2, message: "the session starts once the chosen identity is applied" },
  ]);

  // None again: adding a project asks on a screen inside Add project, and
  // connecting GitHub from there fills it from the account's noreply address.
  catalog = { profiles: [], default_profile_id: null, revision: catalog.revision + 1 };
  expectedCreate.current = octoProfile;
  await page.reload({ waitUntil: "networkidle" });
  await page.locator("[data-projects-menu]").click();
  await page.locator("[data-projects-menu-new-project]").click();
  await page.locator('[data-project-source="local"]').click();
  await page.locator("[data-project-path]").fill("/workspace/fresh");
  const addForm = page.locator("[data-project-form]");
  await page.locator("[data-project-form]:not([data-inspecting])").waitFor();
  assert.equal(await addForm.locator("[data-project-git-profile]").count(), 0, "no identity select without a choice");
  await addForm.getByRole("button", { name: "Add project" }).click();
  await form.waitFor();
  assert.equal(await page.locator(".scrim").count(), 1, "the identity screen lives in the Add project modal");
  await form.getByRole("button", { name: "Connect GitHub", exact: true }).click();
  const connect = page.locator("[data-github-connect-dialog]");
  await connect.waitFor();
  githubAccount = octo;
  await page.keyboard.press("Escape");
  await connect.waitFor({ state: "detached" });
  await form.locator("[data-git-identity-from-github]").waitFor();
  assert.equal(await form.getByRole("textbox", { name: "Git author name" }).inputValue(), octo.name);
  assert.equal(await form.getByRole("textbox", { name: "Git author email" }).inputValue(), octo.email);
  assert.equal(await form.getByRole("button", { name: "Connect GitHub" }).count(), 0, "no Connect GitHub once connected");
  assert.equal(await form.locator("[data-git-identity-note]").count(), 0);
  await form.getByRole("button", { name: "Back", exact: true }).click();
  await addForm.waitFor();
  assert.equal(await page.locator("[data-project-path]").inputValue(), "/workspace/fresh", "Back keeps what you typed");
  assert.deepEqual(created, [], "stepping back creates nothing");
  await addForm.getByRole("button", { name: "Add project" }).click();
  await form.locator("[data-git-identity-from-github]").waitFor();
  await form.getByRole("button", { name: "Save identity" }).click();
  await page.locator('[data-project="fresh"]').waitFor();
  assert.equal(await modal.count(), 0, "saving finishes the add");
  assert.deepEqual(created.map((p) => ({ path: p.canonical_path, identity: p.git_identity_profile_id })),
    [{ path: "/workspace/fresh", identity: octoProfile.id }], "the project is created with the saved identity");

  const openProjectIdentity = async (id) => {
    await page.locator(`[data-project="${id}"] [data-project-toggle]`).hover();
    await page.locator(`[data-project-menu="${id}"]`).click();
    await page.locator("[data-project-menu-identity]").click();
    await form.waitFor();
  };

  // None, from a project's menu: there is nothing to choose, so you create
  // the identity and it is applied to that project.
  catalog = { profiles: [], default_profile_id: null, revision: catalog.revision + 1 };
  await page.reload({ waitUntil: "networkidle" });
  await openProjectIdentity("delta");
  assert.equal(await page.locator("[data-project-git-identity]").count(), 0, "no chooser with nothing to choose");
  assert.equal(await form.locator("[data-git-identity-note]").count(), 0, "no session waits on this");
  assert.equal(await form.getByRole("button", { name: "Back" }).count(), 0);
  const attribution = form.locator("[data-git-identity-from-github]");
  await attribution.waitFor();
  const nameField = form.getByRole("textbox", { name: "Git author name" });
  const fieldBefore = await nameField.elementHandle();
  await nameField.fill("Octo Edited");
  await attribution.waitFor({ state: "hidden" });
  assert.equal(await fieldBefore.evaluate((node) => node.isConnected && node === document.activeElement), true, "editing keeps the focused field");
  await nameField.fill(octo.name);
  await attribution.waitFor();
  await form.getByRole("button", { name: "Save identity" }).click();
  await form.waitFor({ state: "detached" });
  await exactlyOnce([
    { read: () => setupWrites.filter((w) => w.project === "delta" && w.profile === octoProfile.id).length, expected: 1, message: "the saved identity is applied to the project once" },
  ]);
  assert.equal(launches.delta, 0, "applying from the menu starts nothing");
  assert.equal(await modal.count(), 0, "saving closes the dialog");

  // GitHub answers while you type: the field you are in keeps its focus and
  // its text, and only the empty field fills.
  catalog = { profiles: [], default_profile_id: null, revision: catalog.revision + 1 };
  await page.reload({ waitUntil: "networkidle" });
  let releaseGitHub;
  githubHold = new Promise((resolve) => { releaseGitHub = resolve; });
  const asked = page.waitForRequest((request) => new URL(request.url()).pathname === "/api/github/account");
  await openProjectIdentity("epsilon");
  await asked;
  const typed = form.getByRole("textbox", { name: "Git author name" });
  const typedField = await typed.elementHandle();
  await typed.click();
  await page.keyboard.type("Typed Author");
  githubHold = null;
  releaseGitHub();
  await form.getByRole("button", { name: "Connect GitHub" }).waitFor({ state: "detached" });
  assert.equal(await form.getByRole("textbox", { name: "Git author email" }).inputValue(), octo.email, "the empty field fills from GitHub");
  assert.equal(await typedField.evaluate((node) => node.isConnected && node === document.activeElement), true, "the field you are typing in keeps its focus");
  await page.keyboard.type(" Jr");
  assert.equal(await typed.inputValue(), "Typed Author Jr", "typing carries on in the same field");
  assert.equal(await form.locator("[data-git-identity-from-github]").isHidden(), true, "a typed name is not claimed as GitHub's");
  // The sidebar's poll lands mid-word with a new project in the rail: the
  // field keeps its focus and caret.
  await page.keyboard.press("ArrowLeft");
  await page.keyboard.press("ArrowLeft");
  await page.keyboard.press("ArrowLeft");
  setupRevisions.theta = 0;
  await page.locator('[data-project="theta"]').waitFor({ timeout: 15_000 });
  assert.equal(await typed.evaluate((node) => node === document.activeElement), true, "a sidebar refresh keeps the field you are typing in");
  assert.deepEqual(await typed.evaluate((node) => [node.selectionStart, node.selectionEnd]), [12, 12], "a sidebar refresh keeps the caret");
  await page.keyboard.type(",");
  assert.equal(await typed.inputValue(), "Typed Author, Jr", "typing carries on at the caret after a refresh");
  await form.locator("[data-git-identity-onboarding-close]").click();
  await form.waitFor({ state: "detached" });

  // Reopening starts clean: the name you typed last time does not keep
  // GitHub out of the fields now.
  await openProjectIdentity("epsilon");
  await form.locator("[data-git-identity-from-github]").waitFor();
  assert.equal(await form.getByRole("textbox", { name: "Git author name" }).inputValue(), octo.name, "a reopened form fills the name you left untouched");
  assert.equal(await form.getByRole("textbox", { name: "Git author email" }).inputValue(), octo.email, "a reopened form fills the email you left untouched");
  await form.locator("[data-git-identity-onboarding-close]").click();
  await form.waitFor({ state: "detached" });

  // GitHub answers after you focus an empty field but before you type: the
  // field stays yours, so what you type is all it holds. Another window then
  // makes a default while the form is open: saving applies the identity you
  // entered, not that default.
  const entered = { ...profile, id: "identity-4", name: "Entered Author", email: "entered@example.invalid" };
  expectedCreate.current = entered;
  githubHold = new Promise((resolve) => { releaseGitHub = resolve; });
  const askedAgain = page.waitForRequest((request) => new URL(request.url()).pathname === "/api/github/account");
  await openProjectIdentity("epsilon");
  await askedAgain;
  const enteredName = form.getByRole("textbox", { name: "Git author name" });
  await enteredName.click();
  githubHold = null;
  releaseGitHub();
  await form.getByRole("button", { name: "Connect GitHub" }).waitFor({ state: "detached" });
  assert.equal(await enteredName.inputValue(), "", "a focused field is not filled from GitHub");
  assert.equal(await form.getByRole("textbox", { name: "Git author email" }).inputValue(), octo.email, "the untouched field still fills");
  await page.keyboard.type(entered.name);
  assert.equal(await enteredName.inputValue(), entered.name, "the typed name is all the field holds");
  await form.getByRole("textbox", { name: "Git author email" }).fill(entered.email);
  catalog = { profiles: [second], default_profile_id: second.id, revision: catalog.revision + 1 };
  await form.getByRole("button", { name: "Save identity" }).click();
  await form.waitFor({ state: "detached" });
  await exactlyOnce([
    { read: () => setupWrites.filter((w) => w.project === "epsilon" && w.profile === entered.id).length, expected: 1, message: "the identity you entered is applied once" },
    { read: () => setupWrites.filter((w) => w.project === "epsilon" && w.profile !== entered.id).length, expected: 0, message: "the other window's default is not applied" },
  ]);

  // A refresh reads a new identity while the harness list is still on its
  // way, and you type in the open manager meanwhile: the new identity still
  // reaches the screen, and the name you are editing keeps its text, focus
  // and caret without being saved.
  const third = { ...profile, id: "identity-5", name: second.name, email: "third@example.invalid" };
  await page.locator("[data-live-more]").click();
  await page.locator("[data-more-popover]").waitFor();
  await page.getByRole("button", { name: "Git identity…" }).click();
  await manager.waitFor();
  const emails = manager.locator('[aria-label="Saved git identities"] li');
  assert.equal(await emails.count(), 2);
  const managerName = manager.locator("[data-git-identity-name]");
  await managerName.click();
  await page.keyboard.press("End");
  let releaseProviders;
  providersHold = new Promise((resolve) => { releaseProviders = resolve; });
  const catalogRead = page.waitForResponse((response) => new URL(response.url()).pathname === "/api/git-identities", { timeout: 15_000 });
  catalog = { ...catalog, profiles: [...catalog.profiles, third], revision: catalog.revision + 1 };
  await catalogRead;
  await page.waitForTimeout(500);
  await page.keyboard.type(" Q");
  await page.keyboard.press("ArrowLeft");
  providersHold = null;
  releaseProviders();
  await emails.nth(2).waitFor({ timeout: 3_000 });
  assert.equal(await managerName.inputValue(), `${second.name} Q`, "a refresh keeps the name you are editing");
  assert.equal(await managerName.evaluate((node) => node === document.activeElement), true, "a refresh keeps the field you are editing");
  assert.deepEqual(await managerName.evaluate((node) => [node.selectionStart, node.selectionEnd]), [second.name.length + 1, second.name.length + 1], "a refresh keeps the caret");
  await page.keyboard.press("Escape");
  await manager.waitFor({ state: "detached" });
  assert.deepEqual(identityWrites, [], "a redraw does not save a name you are still editing");

  // After a reload the sidebar has seen no sign-in, so the form reads the
  // connection itself: a connected account is named and never offered.
  const openIdentitySettings = async () => {
    await page.locator("[data-live-more]").click();
    await page.locator("[data-more-popover]").waitFor();
    await page.getByRole("button", { name: "Git identity…" }).click();
    await form.waitFor();
  };
  // Records whether the offer to connect was ever drawn, however briefly.
  const watchConnectOffer = () => page.evaluate(() => {
    window.connectOffered = false;
    new MutationObserver((records) => {
      for (const record of records) {
        for (const node of record.addedNodes) {
          const offer = "[data-git-identity-onboarding] [data-github-connect]";
          if (node instanceof Element && (node.matches(offer) || node.querySelector(offer) !== null)) window.connectOffered = true;
        }
      }
    }).observe(document.body, { childList: true, subtree: true });
  });
  const closeIdentityForm = async () => {
    await form.locator("[data-git-identity-onboarding-close]").click();
    await form.waitFor({ state: "detached" });
  };
  const connectedLine = form.locator("[data-github-connected]");
  catalog = { profiles: [], default_profile_id: null, revision: catalog.revision + 1 };
  githubAccount = octo;
  statusLogin = "octocat";
  await page.reload({ waitUntil: "networkidle" });
  await watchConnectOffer();
  await openIdentitySettings();
  await connectedLine.waitFor();
  assert.equal((await connectedLine.innerText()).trim(), "Connected to GitHub as octocat");
  await form.locator("[data-git-identity-from-github]").waitFor();
  assert.equal(await form.getByRole("button", { name: "Connect GitHub" }).count(), 0, "a connected account is not offered again");
  assert.equal(await page.evaluate(() => window.connectOffered), false, "the offer to connect never flashed");
  await closeIdentityForm();

  // A slow read shows neither line, and its answer lands without touching
  // the field you are typing in.
  await page.reload({ waitUntil: "networkidle" });
  await watchConnectOffer();
  let releaseStatus;
  statusHold = new Promise((resolve) => { releaseStatus = resolve; });
  const statusAsked = page.waitForRequest((request) => new URL(request.url()).pathname === "/api/github/connect/status");
  await openIdentitySettings();
  await statusAsked;
  const slowName = form.getByRole("textbox", { name: "Git author name" });
  const slowField = await slowName.elementHandle();
  await slowName.click();
  await slowName.press("ControlOrMeta+a");
  await page.keyboard.type("Slow Author");
  await page.keyboard.press("ArrowLeft");
  await page.waitForTimeout(300);
  assert.equal(await form.getByRole("button", { name: "Connect GitHub" }).count(), 0, "no offer while the connection is read");
  assert.equal(await connectedLine.count(), 0, "no account named before the read answers");
  statusHold = null;
  releaseStatus();
  await connectedLine.waitFor();
  assert.equal(await slowField.evaluate((node) => node.isConnected && node === document.activeElement), true, "the answer keeps the field you are typing in");
  assert.equal(await slowName.inputValue(), "Slow Author", "the answer keeps what you typed");
  assert.deepEqual(await slowName.evaluate((node) => [node.selectionStart, node.selectionEnd]), [10, 10], "the answer keeps the caret");
  assert.equal(await page.evaluate(() => window.connectOffered), false, "the offer to connect never flashed during the slow read");
  await closeIdentityForm();

  // No account: the offer to connect shows once the read says so.
  githubAccount = null;
  statusLogin = null;
  await page.reload({ waitUntil: "networkidle" });
  await openIdentitySettings();
  await form.getByRole("button", { name: "Connect GitHub", exact: true }).waitFor();
  assert.equal(await connectedLine.count(), 0, "no account is named when none is connected");
  await closeIdentityForm();

  // A failed read keeps the offer to connect.
  statusFails = true;
  await page.reload({ waitUntil: "networkidle" });
  await openIdentitySettings();
  await form.getByRole("button", { name: "Connect GitHub", exact: true }).waitFor();
  assert.equal(await connectedLine.count(), 0, "a failed read names no account");
  await closeIdentityForm();
  statusFails = false;

  // A project a refresh no longer lists takes its dialogs with it, and the
  // info bar says so: saving the form would make an identity for nothing.
  const goneNotice = "That project is no longer in your list, so its dialog closed.";
  catalog = { profiles: [], default_profile_id: null, revision: catalog.revision + 1 };
  setupRevisions.iota = 0;
  await page.reload({ waitUntil: "networkidle" });
  await openProjectIdentity("iota");
  await form.getByRole("textbox", { name: "Git author name" }).fill("Gone Author");
  delete setupRevisions.iota;
  await form.waitFor({ state: "detached", timeout: 15_000 });
  await restPointer();
  await page.waitForFunction((sentence) => document.querySelector("[data-info-status]")?.textContent === sentence, goneNotice);
  assert.equal(catalog.profiles.length, 0, "no identity is made for a project that is gone");

  catalog = { profiles: [second], default_profile_id: second.id, revision: catalog.revision + 1 };
  setupRevisions.kappa = 1;
  await page.reload({ waitUntil: "networkidle" });
  const kappaRow = page.locator('[data-session="session-kappa"]');
  if (!(await kappaRow.isVisible())) await page.locator('[data-project-toggle="kappa"]').click();
  await kappaRow.hover();
  await page.locator('[data-session-menu="session-kappa"]').click();
  await page.locator("[data-session-menu-close]").click();
  const closeConfirm = page.locator("[data-remove-confirm]");
  await closeConfirm.waitFor();
  delete setupRevisions.kappa;
  await closeConfirm.waitFor({ state: "detached", timeout: 15_000 });
  await restPointer();
  await page.waitForFunction((sentence) => document.querySelector("[data-info-status]")?.textContent === sentence, goneNotice);

  // In a frame with no modals region the sidebar hosts its dialogs itself,
  // so a rail redraw must put the dialog back with the name you are typing
  // unsaved, focused and at its caret.
  await page.evaluate(() => document.querySelector('[data-region="modals"]')?.removeAttribute("data-region"));
  await page.locator("[data-live-more]").click();
  await page.locator("[data-more-popover]").waitFor();
  await page.getByRole("button", { name: "Git identity…" }).click();
  await manager.waitFor();
  await managerName.focus();
  await managerName.press("ControlOrMeta+a");
  await page.keyboard.type("Partial Name");
  await page.keyboard.press("Home");
  for (let i = 0; i < 4; i++) await page.keyboard.press("ArrowRight");
  const writesBefore = identityWrites.length;
  setupRevisions.lambda = 1;
  await page.locator('[data-project="lambda"]').waitFor({ timeout: 15_000 });
  assert.equal(await manager.count(), 1, "a rail redraw keeps the dialog it hosts");
  assert.equal(identityWrites.length, writesBefore, "a rail redraw does not save a name you are still typing");
  assert.equal(await managerName.inputValue(), "Partial Name", "a rail redraw keeps the name you are typing");
  assert.equal(await managerName.evaluate((node) => node === document.activeElement), true, "a rail redraw keeps the field you are typing in");
  assert.deepEqual(await managerName.evaluate((node) => [node.selectionStart, node.selectionEnd]), [4, 4], "a rail redraw keeps the caret");

  assert.deepEqual(pageErrors, []);
  console.log("Git identity asked only when needed: none, one, several; GitHub prefill; one-shot launch retry: PASS");
  await context.close();
} finally {
  if (browser !== undefined) await browser.close();
  vite.kill("SIGTERM");
}
