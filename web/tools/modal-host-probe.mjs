// Real-browser probe for the one-modal rule: opening Connect GitHub from
// inside the Git identity dialog and from inside Add project
// pushes a screen onto the open dialog, and so does an upload collision
// that arrives while Change password is open. A page-wide observer fails
// the run if two scrims or two visible modal dialogs ever exist at once.
import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import net from "node:net";
import { browser as connectBrowser } from "./browser.mjs";
import { eventually } from "./eventually.mjs";

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

// Records the most scrims and visible modal dialogs ever on the page at once.
const watchOverlays = () => {
  const seen = { scrims: 0, dialogs: 0, at: "" };
  window.__overlays = seen;
  const check = () => {
    const scrims = document.querySelectorAll(".scrim").length;
    const dialogs = [...document.querySelectorAll('[aria-modal="true"]')]
      .filter((el) => el.getClientRects().length > 0).length;
    if (scrims > seen.scrims || dialogs > seen.dialogs) {
      seen.at = [...document.querySelectorAll('[aria-modal="true"]')]
        .map((el) => `${el.getAttribute("aria-label") ?? el.getAttribute("aria-labelledby")}:${el.getClientRects().length > 0}`).join(", ");
    }
    seen.scrims = Math.max(seen.scrims, scrims);
    seen.dialogs = Math.max(seen.dialogs, dialogs);
  };
  new MutationObserver(check).observe(document, {
    subtree: true, childList: true, attributes: true, attributeFilter: ["hidden", "class", "style"],
  });
};

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
  const context = await browser.newContext({ viewport: { width: 1100, height: 780 } });
  await context.addCookies([{ name: "noisedeck_csrf", value: "cookie-fixture", url: origin }]);
  const page = await context.newPage();
  const pageErrors = [];
  page.on("pageerror", (error) => pageErrors.push(error.message));

  await page.addInitScript(watchOverlays);

  await page.route(`${origin}/auth/session`, (route) => route.fulfill({
    status: 204, body: "", headers: { "X-Noisedeck-Auth-Mode": "trusted_proxy" },
  }));
  await page.route(`${origin}/api/**`, async (route) => {
    const request = route.request();
    const path = new URL(request.url()).pathname;
    const json = (body, status = 200) => route.fulfill({ status, contentType: "application/json", body: JSON.stringify(body) });
    if (path === "/api/github/connect/status") return json({ outcome: "no-account", active: false });
    if (path === "/api/github/connect/attempt") return json({ csrf: "connect-fixture", attempt: null });
    if (path === "/api/settings") return json({ skip_permissions: false, skip_close_confirm: false, skip_remove_project_confirm: false });
    if (path === "/api/settings/tutorial") return json({ version: 1, dismissed: true, lesson: "welcome", step: 0, done: [], scope: "user" });
    if (path === "/api/settings/sidebar-layout") return json({ expanded: [], projects: [], sessions: {} });
    if (path === "/api/harness-login") return json({ pending: [], csrf: "fixture" });
    if (path === "/api/projects") return json([]);
    if (path === "/api/project-directories") return json({ current_path: "/workspace", parent_path: null, directories: [] });
    if (path === "/api/git-identities") return json({ profiles: [], default_profile_id: null, revision: 0 });
    if (path === "/api/providers") return json({ providers: [] });
    if (path === "/api/session-activity/events") return route.fulfill({ contentType: "text/event-stream", body: ": fixture\n\n" });
    return json({ error: "not_found", message: path }, 404);
  });

  const scrims = () => page.locator(".scrim").count();
  const focusInside = (selector) => page.evaluate((s) => document.activeElement?.closest(s) !== null, selector);
  const github = page.locator("[data-github-connect-dialog]");

  await page.goto(origin, { waitUntil: "networkidle" });

  // From the Git identity dialog, which opens only when you ask for it.
  await page.locator("[data-new-project-row]").waitFor();
  assert.equal(await scrims(), 0, "no dialog opens on load");
  await page.locator("[data-live-more]").click();
  await page.getByRole("button", { name: "Git identity…" }).click();
  const identity = page.locator("[data-git-identity-onboarding]");
  await identity.waitFor();
  assert.equal(await identity.locator(".dialog-head").getByRole("button", { name: "Close" }).count(), 1,
    "the identity dialog has a close button");
  const connectFromIdentity = () => identity.getByRole("button", { name: "Connect GitHub", exact: true }).click();

  await connectFromIdentity();
  await github.waitFor();
  assert.equal(await identity.isVisible(), false, "the identity screen steps aside for the pushed screen");
  assert.equal(await scrims(), 1, "one scrim holds both screens");
  assert.equal(await github.locator(".dialog-head [data-modal-back]").count(), 1, "the pushed screen has a Back control");
  assert.ok(await focusInside("[data-github-connect-dialog]"), "focus moves into the pushed screen");
  for (let i = 0; i < 8; i++) await page.keyboard.press("Tab");
  assert.ok(await focusInside("[data-github-connect-dialog]"), "Tab stays inside the dialog");

  await page.keyboard.press("Escape");
  await github.waitFor({ state: "detached" });
  await identity.waitFor({ state: "visible" });
  assert.ok(await focusInside("[data-git-identity-onboarding]"), "Escape returns focus to the screen beneath");

  await connectFromIdentity();
  await github.locator("[data-modal-back]").click();
  await github.waitFor({ state: "detached" });
  await identity.waitFor({ state: "visible" });

  await connectFromIdentity();
  await github.locator(".dialog-head [data-dialog-close]").click();
  await github.waitFor({ state: "detached" });
  await identity.waitFor({ state: "detached" });
  assert.equal(await scrims(), 0, "the head close control closes the whole modal");

  // From inside Add project.
  await page.locator("[data-new-project-row]").click();
  const chooser = page.locator("[data-project-source-dialog]");
  await chooser.waitFor();
  assert.equal(await chooser.locator("[data-modal-back]").count(), 0, "a first screen has no Back control");
  await chooser.locator('[data-project-source="github"]').click();
  const picker = page.locator("[data-github-picker]");
  await picker.locator("[data-github-repo-connect]").click();
  await github.waitFor();
  assert.equal(await picker.isVisible(), false, "Import from GitHub steps aside for the pushed screen");
  assert.equal(await scrims(), 1, "one scrim holds both screens");
  await page.keyboard.press("Escape");
  await github.waitFor({ state: "detached" });
  await picker.waitFor({ state: "visible" });
  await page.keyboard.press("Escape");
  await picker.waitFor({ state: "detached" });
  assert.equal(await scrims(), 0, "Escape on the last screen closes the modal");

  // A lower screen showing one of its owner's inner steps: Back on the
  // pushed screen returns to that step, and the head close control closes
  // the whole modal even though the owner leaves its step first.
  const directory = page.locator("[data-directory-dialog]");
  const githubOverDirectory = async () => {
    await page.locator("[data-new-project-row]").click();
    await chooser.locator('[data-project-source="local"]').click();
    await page.locator("[data-project-browse]").click();
    await directory.waitFor();
    await page.locator("[data-live-more]").dispatchEvent("click");
    await page.getByRole("button", { name: "GitHub connection…", exact: true }).dispatchEvent("click");
    await github.waitFor();
    assert.equal(await directory.isVisible(), false, "the directory picker steps aside for the pushed screen");
    assert.equal(await scrims(), 1, "one scrim holds both screens");
  };
  await githubOverDirectory();
  await github.locator("[data-modal-back]").click();
  await github.waitFor({ state: "detached" });
  await directory.waitFor({ state: "visible" });
  await page.keyboard.press("Escape");
  await directory.waitFor({ state: "detached" });
  await page.keyboard.press("Escape");
  await eventually(scrims, (count) => count === 0, "Escape steps out of the picker, then out of Add project");

  await githubOverDirectory();
  await github.locator(".dialog-head [data-dialog-close]").click();
  await github.waitFor({ state: "detached" });
  await eventually(scrims, (count) => count === 0, "the head close control closes the whole modal past the picker");
  assert.equal(await page.locator("[data-modal-screen]").count(), 0, "no screen is left behind the closed modal");

  const seen = await page.evaluate(() => window.__overlays);
  assert.equal(seen.scrims, 1, `at most one scrim at a time (saw ${seen.scrims})`);
  assert.equal(seen.dialogs, 1, `at most one visible modal dialog at a time (saw ${seen.dialogs}: ${seen.at})`);
  assert.deepEqual(pageErrors, []);
  console.log("One modal: Connect GitHub pushes a screen from Git identity and from Add project: PASS");

  // A panel mounted without a workspace modals region hosts its modal in
  // its own container, which its next render replaces wholesale.
  await page.route(`${origin}/fallback-fixture`, (route) => route.fulfill({ contentType: "text/html", body: `<!doctype html>
    <div id="panel" style="position:relative;height:600px"></div>
    <script type="module">
      import "/src/styles/app.css";
      import { TaskmasterPanel } from "/src/panels/taskmaster/taskmaster-panel-dom.ts";
      import { TaskmasterPanelModel } from "/src/panels/taskmaster/taskmaster-panel.ts";
      window.panel = new TaskmasterPanel(document.querySelector("#panel"), new TaskmasterPanelModel({}, "fixture"));
      window.ready = true;
    </script>` }));
  await page.goto(`${origin}/fallback-fixture`);
  await page.waitForFunction(() => window.ready === true);
  const addPrd = page.locator('[role="dialog"][aria-modal="true"]');
  const visibleDialogs = () => addPrd.evaluateAll((els) => els.filter((el) => el.isConnected && el.getClientRects().length > 0).length);
  await page.evaluate(() => window.panel.openAddPrd());
  assert.equal(await visibleDialogs(), 1, "Add PRD opens in the fallback mount");
  await page.evaluate(() => window.panel.render());
  assert.equal(await visibleDialogs(), 1, "a re-render keeps the open dialog on screen");
  assert.equal(await scrims(), 1, "the re-render keeps one scrim in the container");
  await page.locator(".dialog-head [data-tm-prd-cancel]").click();
  assert.equal(await scrims(), 0, "closing the dialog removes the scrim");
  assert.deepEqual(pageErrors, []);
  console.log("One modal: a fallback mount survives its container re-rendering: PASS");
  await context.close();

  // With password sign-in, an upload collision that arrives while Change
  // password is open pushes onto it instead of raising a second overlay.
  const passwordContext = await browser.newContext({ viewport: { width: 1400, height: 900 } });
  await passwordContext.addCookies([{ name: "noisedeck_csrf", value: "cookie-fixture", url: origin }]);
  const passwordPage = await passwordContext.newPage();
  const passwordErrors = [];
  passwordPage.on("pageerror", (error) => passwordErrors.push(error.message));
  await passwordPage.addInitScript(watchOverlays);
  const project = { id: "project-1", name: "noisedeck", canonical_path: "/workspace/noisedeck", last_used_at: "2026-08-01T00:00:00Z" };
  const session = {
    id: "session-1", project_id: project.id, provider: "claude", display_name: "", status: "running", generation: 1,
    recovery_capability: "none", recovery_state: "unavailable", presentation_state: "running",
    created_at: "2026-08-01T00:00:00Z", updated_at: "2026-08-01T00:00:00Z",
  };
  const heldUploads = new Map();
  const quiet = { contentType: "text/event-stream", body: ": connected\n\nretry: 3600000\n\n" };
  await passwordPage.route(`${origin}/auth/session`, (route) => route.fulfill({
    status: 204, body: "", headers: { "X-Noisedeck-Auth-Mode": "password" },
  }));
  await passwordPage.route(`${origin}/api/**`, async (route) => {
    const request = route.request();
    const url = new URL(request.url());
    const path = url.pathname;
    const json = (body, status = 200) => route.fulfill({ status, contentType: "application/json", body: JSON.stringify(body) });
    if (path === "/api/projects") return json([project]);
    if (path === `/api/projects/${project.id}/sessions`) return json([session]);
    if (path === `/api/sessions/${session.id}`) return json(session);
    if (path === "/api/git-identities") return json({
      profiles: [{ id: "identity-1", name: "Deck probe", email: "deck@example.invalid", revision: 1, created_at: "t", updated_at: "t" }],
      default_profile_id: "identity-1", revision: 1,
    });
    if (path === "/api/settings") return json({ skip_permissions: false, skip_close_confirm: false, skip_remove_project_confirm: false });
    if (path === "/api/settings/tutorial") return json({ version: 1, dismissed: true, lesson: "welcome", step: 0, done: [], scope: "user" });
    if (path === "/api/settings/last-session" && request.method() === "PUT") return json(request.postDataJSON());
    if (path === "/api/settings/sidebar-layout") return json({ expanded: [], projects: [], sessions: {} });
    if (path === "/api/harness-login") return json({ pending: [], csrf: "fixture" });
    if (path === "/api/providers") return json({ providers: ["claude"] });
    if (path === "/api/chats") return json({ chats: [] });
    if (/^\/api\/sessions\/[^/]+\/chats$/.test(path)) return json({ agent: "claude-sess", chats: [] });
    if (/^\/api\/projects\/[^/]+\/lanes$/.test(path)) return json({ lanes: [] });
    if (/^\/api\/projects\/[^/]+\/assets$/.test(path)) return json({ locations: [], scanned: "2026-01-01T00:00:00Z", preview_max_bytes: 16777216 });
    if (path.endsWith("/events")) return route.fulfill(quiet);
    if (path === `/api/projects/${project.id}/files` && request.method() === "GET") return json({ path: "", at_root: true, entries: [] });
    if (path === `/api/projects/${project.id}/files` && request.method() === "POST") {
      const name = url.searchParams.get("path") ?? "";
      await new Promise((resolve) => heldUploads.set(name, resolve));
      return json({ error: "conflict", reason: "exists", message: "destination exists",
        existing: { name, dir: false, size: 4, mtime: "2026-09-23T03:00:00Z", version: "held-original" } }, 409);
    }
    return json({ error: "not_found", message: path }, 404);
  });
  await passwordPage.routeWebSocket(`${origin.replace("http", "ws")}/api/sessions/*/terminal?*`, (socket) => {
    socket.send(JSON.stringify({ type: "attached", token: 1, generation: 1 }));
  });

  await passwordPage.goto(origin, { waitUntil: "networkidle" });
  await passwordPage.locator("[data-live-terminal]").waitFor();
  await passwordPage.getByRole("button", { name: "Files", exact: true }).click();
  const files = passwordPage.locator('[data-region="files"]');
  await files.locator("[data-files-input]").setInputFiles([
    { name: "held-conflict-password.txt", mimeType: "text/plain", buffer: Buffer.from("held\n") },
  ]);
  await eventually(() => heldUploads.has("held-conflict-password.txt"), Boolean, "the upload awaits its answer");
  await passwordPage.locator("[data-live-more]").click();
  await passwordPage.getByRole("button", { name: "Change password", exact: true }).click();
  const change = passwordPage.getByRole("dialog", { name: "Change password" });
  await change.waitFor();
  assert.equal(await passwordPage.locator(".scrim").count(), 1, "Change password opens in the one modal");
  heldUploads.get("held-conflict-password.txt")();
  const conflict = passwordPage.getByRole("alertdialog");
  await conflict.waitFor();
  assert.equal(await change.isVisible(), false, "Change password steps aside for the ask");
  assert.equal(await passwordPage.locator(".scrim").count(), 1, "one scrim holds both screens");
  await passwordPage.locator("[data-files-conflict-cancel]").click();
  await conflict.waitFor({ state: "detached" });
  await change.waitFor({ state: "visible" });
  await eventually(() => passwordPage.evaluate(() => document.activeElement?.closest('[aria-label="Change password"]') !== null),
    Boolean, "Cancel leaves the keyboard in Change password");
  await passwordPage.keyboard.press("Escape");
  await change.waitFor({ state: "detached" });
  assert.equal(await passwordPage.locator(".scrim").count(), 0, "Escape closes Change password and the modal");
  const passwordSeen = await passwordPage.evaluate(() => window.__overlays);
  assert.equal(passwordSeen.scrims, 1, `at most one scrim at a time (saw ${passwordSeen.scrims})`);
  assert.equal(passwordSeen.dialogs, 1, `at most one visible modal dialog at a time (saw ${passwordSeen.dialogs}: ${passwordSeen.at})`);
  assert.deepEqual(passwordErrors, []);
  console.log("One modal: an upload collision pushes onto Change password: PASS");
  await passwordContext.close();
} finally {
  if (browser !== undefined) await browser.close();
  vite.kill("SIGTERM");
}
