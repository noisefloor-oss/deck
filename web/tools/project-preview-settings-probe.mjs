// Browser interaction checks for preview settings. The HTTPS app-preview
// probe separately exercises the real gateway, authentication and launcher.
import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import net from "node:net";
import { fileURLToPath } from "node:url";
import { browser as connectBrowser } from "./browser.mjs";

const port = await new Promise((resolve, reject) => {
  const listener = net.createServer();
  listener.once("error", reject);
  listener.listen(0, "127.0.0.1", () => {
    const port = listener.address().port;
    listener.close(() => resolve(port));
  });
});
const origin = `http://127.0.0.1:${port}`;
const vite = spawn(process.execPath, [fileURLToPath(new URL("../node_modules/vite/bin/vite.js", import.meta.url)),
  "--host", "127.0.0.1", "--port", String(port), "--strictPort"], {
  cwd: new URL("..", import.meta.url), stdio: ["ignore", "pipe", "pipe"],
});
let output = "";
vite.stdout.on("data", chunk => { output += chunk; });
vite.stderr.on("data", chunk => { output += chunk; });
let browser;

try {
  for (let attempt = 0; attempt < 100; attempt++) {
    try { if ((await fetch(origin)).ok) break; } catch { /* startup */ }
    if (attempt === 99) throw new Error(`Vite did not start: ${output}`);
    await new Promise(resolve => setTimeout(resolve, 50));
  }
  ({ browser } = await connectBrowser());
  const context = await browser.newContext({ viewport: { width: 1280, height: 900 }, serviceWorkers: "block" });
  await context.addCookies([{ name: "noisedeck_csrf", value: "preview-probe", url: origin }]);
  const page = await context.newPage();
  page.setDefaultTimeout(10000);
  const errors = [];
  page.on("pageerror", error => errors.push(error.message));
  const profile = { id: "identity-1", name: "Preview Probe", email: "preview@example.invalid", revision: 1 };
  const projects = ["first", "second"].map(id => ({
    id, name: `project-${id}`, canonical_path: `/synthetic/${id}`,
    git_identity_setup_revision: 1, git_identity_profile_id: profile.id,
    git_identity_profile_revision: 1, git_author_name: profile.name, git_author_email: profile.email,
  }));
  const empty = () => ({ configured: true, self_service: true, enabled: false });
  const assignment = id => ({ ...empty(), enabled: true, source: "managed",
    origin: `https://${id}.preview.example.net`, port: id === "first" ? 15001 : 15002,
    upstream: `127.0.0.1:${id === "first" ? 15001 : 15002}` });
  const settings = new Map(projects.map(project => [project.id, empty()]));
  const mutations = [];
  let checkCalls = 0, entryCalls = 0, nextFailure = "", heldMutation = null;
  let checkReachable = false, checkFailure = false, holdNextMutation = false;
  await page.route(`${origin}/auth/session`, route => route.fulfill({
    status: 204, body: "", headers: { "X-Noisedeck-Auth-Mode": "trusted_proxy" },
  }));
  await page.route(`${origin}/api/**`, async route => {
    const request = route.request(), path = new URL(request.url()).pathname, method = request.method();
    const json = (status, body) => route.fulfill({ status, contentType: "application/json", body: JSON.stringify(body) });
    if (path === "/api/settings/tutorial") return json(200, { version: 1, dismissed: true, lesson: "welcome", step: 0, done: [], scope: "user" });
    if (path === "/api/settings") return json(200, { skip_permissions: false, skip_close_confirm: false, skip_remove_project_confirm: false });
    if (path === "/api/git-identities") return json(200, { profiles: [profile], default_profile_id: profile.id, revision: 1 });
    if (path === "/api/session-activity/events") return route.fulfill({ status: 200, contentType: "text/event-stream", body: ": connected\n\n" });
    if (path === "/api/projects") return json(200, projects);
    if (/\/sessions$/.test(path)) return json(200, []);
    if (path === "/api/providers") return json(200, { providers: [] });
    const match = /^\/api\/projects\/(first|second)\/preview(?:\/(settings|check))?$/.exec(path);
    if (match) {
      const [, id, action] = match;
      if (action === "settings" && method === "GET") return json(200, settings.get(id));
      assert.equal(request.headers()["x-csrf-token"], "preview-probe", "preview mutations/checks need CSRF protection");
      if (action === "settings") {
        mutations.push({ id, method });
        if (holdNextMutation) {
          holdNextMutation = false;
          await new Promise(resolve => { heldMutation = resolve; });
          heldMutation = null;
        }
        if (nextFailure === "ports") {
          nextFailure = "";
          return json(409, { error: { code: "preview_ports_exhausted", message: "No port available." } });
        }
        settings.set(id, method === "PUT" ? assignment(id) : { ...settings.get(id), enabled: false });
        if (nextFailure === "enable-lost") {
          nextFailure = "";
          return route.abort("failed");
        }
        if (nextFailure === "disable-write") {
          nextFailure = "";
          return json(500, { error: { code: "internal", message: "Could not save the preview registration." } });
        }
        return json(200, settings.get(id));
      }
      if (action === "check") {
        checkCalls++;
        if (checkFailure) return json(503, { error: { code: "unavailable", message: "Check unavailable." } });
        return json(200, { reachable: checkReachable, checked_at: new Date().toISOString() });
      }
      entryCalls++;
      return json(503, { error: { code: "unavailable", message: "Entry unavailable." } });
    }
    return json(404, { error: { code: "not_found", message: path } });
  });
  await page.goto(origin, { waitUntil: "networkidle" });
  await page.locator('[data-project-menu="first"]').waitFor();
  const dialog = page.locator("[data-project-preview-dialog]");
  async function openSettings(id = "first") {
    await page.locator(`[data-project-toggle="${id}"]`).hover();
    await page.locator(`[data-project-menu="${id}"]`).click();
    await page.locator("[data-project-menu-preview-settings]").click();
    await dialog.waitFor();
    await page.waitForFunction(() => document.querySelector("[data-project-preview-dialog]")?.getAttribute("data-preview-phase") !== "loading");
  }
  await openSettings();
  await dialog.getByRole("heading", { name: "Preview project-first", exact: true }).waitFor();
  assert.equal(checkCalls, 0, "opening settings must not invent a connectivity observation");
  assert.equal(await dialog.getByText(/Copy agent instructions/).count(), 0);
  holdNextMutation = true;
  await dialog.getByRole("button", { name: "Enable preview", exact: true }).click();
  await dialog.getByRole("button", { name: "Enabling…", exact: true }).waitFor();
  assert.equal(await dialog.getByRole("button", { name: "Cancel", exact: true }).isDisabled(), true);
  await page.keyboard.press("Escape");
  assert.equal(await dialog.count(), 1, "a pending mutation must keep its outcome visible");
  while (heldMutation === null) await new Promise(resolve => setTimeout(resolve, 10));
  heldMutation();
  await dialog.getByRole("heading", { name: "Preview enabled", exact: true }).waitFor();
  await dialog.locator("[data-preview-connectivity]").getByText("Not checked yet", { exact: true }).waitFor();
  assert.deepEqual(mutations, [{ id: "first", method: "PUT" }]);
  await dialog.getByRole("button", { name: "Check connectivity", exact: true }).click();
  await dialog.locator("[data-preview-connectivity]").filter({ hasText: "Not responding" }).waitFor();
  checkReachable = true;
  await dialog.getByRole("button", { name: "Check connectivity", exact: true }).click();
  await dialog.locator("[data-preview-connectivity]").filter({ hasText: "Accepting connections" }).waitFor();
  async function expectObservedMenuStatus(status) {
    const calls = checkCalls;
    await dialog.getByRole("button", { name: "Done", exact: true }).click();
    await dialog.waitFor({ state: "detached" });
    await page.locator('[data-project-toggle="first"]').hover();
    await page.locator('[data-project-menu="first"]').click();
    const menu = page.locator("[data-project-menu-popover]");
    await menu.locator("[data-project-preview-menu-status]").filter({ hasText: status }).waitFor();
    await menu.getByRole("menuitem", { name: "Open preview", exact: true }).waitFor();
    assert.equal(checkCalls, calls, "opening the menu must not trigger a connectivity check");
    await menu.locator("[data-project-menu-preview-settings]").click();
    await dialog.locator("[data-preview-connectivity]").filter({ hasText: status }).waitFor();
    assert.equal(checkCalls, calls, "reopening settings retains the observation without checking again");
  }
  await expectObservedMenuStatus("Accepting connections");
  await dialog.getByRole("button", { name: "Details", exact: true }).click();
  await dialog.getByText("15001", { exact: true }).waitFor();
  assert.doesNotMatch(await dialog.innerText(), /healthy/i);
  checkFailure = true;
  await dialog.getByRole("button", { name: "Check connectivity", exact: true }).click();
  await dialog.locator("[data-preview-connectivity]").getByText("Check failed", { exact: true }).waitFor();
  await expectObservedMenuStatus("Check failed");
  checkFailure = false;
  await page.evaluate(() => {
    Object.defineProperty(navigator, "clipboard", { configurable: true, value: { writeText: async () => { throw new Error("clipboard denied"); } } });
  });
  await dialog.getByRole("button", { name: "Copy URL", exact: true }).click();
  await dialog.getByText("Couldn't copy the URL", { exact: true }).waitFor();
  assert.equal(await dialog.getByRole("button", { name: "Copied", exact: true }).count(), 0);
  await page.evaluate(() => { window.open = () => null; });
  await dialog.getByRole("button", { name: "Open preview", exact: true }).click();
  await dialog.getByText(/Allow pop-ups/).waitFor();
  assert.equal(entryCalls, 0, "a blocked popup must not issue an entry ticket");
  await dialog.getByRole("button", { name: "Done", exact: true }).click();
  await dialog.waitFor({ state: "detached" });
  assert.equal(await page.locator('[data-project-menu="first"]').evaluate(el => el === document.activeElement), true);

  await openSettings();
  await dialog.getByRole("button", { name: "Disable access…", exact: true }).click();
  await dialog.getByRole("heading", { name: "Disable preview access?", exact: true }).waitFor();
  assert.match(await dialog.innerText(), /URL and port stay reserved/);
  nextFailure = "disable-write";
  await dialog.getByRole("button", { name: "Disable access", exact: true }).click();
  await dialog.getByText("Disable outcome unconfirmed", { exact: true }).waitFor();
  assert.equal(await dialog.getByRole("heading", { name: "Preview access disabled", exact: true }).count(), 0,
    "a runtime-disabled read cannot prove failed persistence succeeded");
  await dialog.getByRole("button", { name: "Try again", exact: true }).click();
  await dialog.getByRole("heading", { name: "Preview access disabled", exact: true }).waitFor();
  assert.equal(settings.get("first").origin, assignment("first").origin);
  await page.keyboard.press("Escape");
  await dialog.waitFor({ state: "detached" });

  await openSettings("second");
  nextFailure = "ports";
  await dialog.getByRole("button", { name: "Enable preview", exact: true }).click();
  await dialog.getByText("No dev-server ports are free", { exact: true }).waitFor();
  assert.match(await dialog.innerText(), /Disabling a preview keeps its reserved port/);
  nextFailure = "enable-lost";
  await dialog.getByRole("button", { name: "Try again", exact: true }).click();
  await dialog.getByText("Enable outcome unconfirmed", { exact: true }).waitFor();
  assert.doesNotMatch(await dialog.innerText(), /Nothing was reserved/);
  await page.keyboard.press("Escape");
  await dialog.waitFor({ state: "detached" });

  settings.set("first", { ...assignment("first"), source: "static", self_service: false });
  await openSettings();
  await dialog.getByText(/managed by the installation/).waitFor();
  assert.equal(await dialog.getByRole("button", { name: "Disable access…", exact: true }).count(), 0);
  await page.keyboard.press("Escape");
  settings.set("first", { configured: true, self_service: false, enabled: false });
  await openSettings();
  await dialog.getByRole("heading", { name: "Preview setup is managed by the installation owner", exact: true }).waitFor();
  assert.equal(await dialog.getByRole("button", { name: "Enable preview", exact: true }).count(), 0);
  await page.keyboard.press("Escape");
  settings.set("first", { configured: false, self_service: false, enabled: false });
  await openSettings();
  await dialog.getByRole("heading", { name: "Previews aren't set up", exact: true }).waitFor();
  await page.keyboard.press("Escape");

  for (const viewport of [{ width: 393, height: 852 }, { width: 320, height: 568 }]) {
    await page.setViewportSize(viewport);
    await page.goto(`${origin}/preview.html?project-preview=up&project-preview-details=1`, { waitUntil: "networkidle" });
    await dialog.waitFor();
    const dimensions = await dialog.evaluate(el => ({ width: el.clientWidth, scrollWidth: el.scrollWidth, box: el.getBoundingClientRect().toJSON() }));
    assert.equal(dimensions.scrollWidth, dimensions.width, "preview settings must not overflow horizontally");
    assert.ok(dimensions.box.x >= 0 && dimensions.box.right <= viewport.width, "the mobile sheet stays within the viewport");
    await dialog.getByRole("button", { name: "Open preview", exact: true }).focus();
    for (let i = 0; i < 12; i++) {
      await page.keyboard.press("Tab");
      assert.equal(await dialog.evaluate(el => el.contains(document.activeElement)), true, "Tab must stay inside the dialog");
    }
    await page.goto(`${origin}/preview.html?project-preview=menu-enabled`, { waitUntil: "networkidle" });
    const menu = page.locator("[data-project-menu-popover]");
    await menu.waitFor();
    await menu.locator("[data-project-menu-sheet-title]").getByText("rewrite-scheduler", { exact: true }).waitFor();
    const box = await menu.boundingBox();
    assert.equal(Math.round(box.x), 0, "mobile project menu starts at the left edge");
    assert.equal(Math.round(box.width), viewport.width, "mobile project menu fills the viewport width");
    assert.equal(Math.round(box.y + box.height), viewport.height, "mobile project menu is bottom anchored");
    for (const action of ["move-up", "move-down", "move-top", "rename", "identity", "preview", "preview-settings", "remove"]) {
      const row = menu.locator(`[data-project-menu-${action}]`);
      assert.equal(await row.count(), 1, `mobile project menu retains ${action}`);
      assert.ok((await row.boundingBox()).height >= 44, `${action} has a 44px touch target`);
    }
    assert.ok(await menu.locator("[data-proj-swatch]").count() > 0, "mobile project menu retains color selection");
    await menu.locator("[data-project-menu-preview]").focus();
    for (const key of ["ArrowDown", "ArrowUp", "Home", "End", "Tab", "Shift+Tab"]) {
      await page.keyboard.press(key);
      assert.equal(await menu.evaluate(el => el.contains(document.activeElement)), true, `${key} stays in the menu`);
    }
    await page.keyboard.press("Escape");
    await menu.waitFor({ state: "detached" });
    assert.equal(await page.evaluate(() => document.activeElement?.hasAttribute("data-project-menu")), true,
      "closing the mobile menu restores its trigger focus");
  }
  assert.deepEqual(errors, []);
  console.log("Preview settings: mutations, uncertain outcomes, observed connectivity, static configuration, clipboard/popup refusal, focus and mobile layout PASS");
  await context.close();
} finally {
  if (browser) await browser.close();
  vite.kill("SIGTERM");
}
