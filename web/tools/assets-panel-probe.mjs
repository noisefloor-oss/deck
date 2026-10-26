// Deterministic browser probe for the Assets panel: the fourth
// docked panel, its selection, the delete consent and the outcome. Drives the
// BUILT live page against a mocked authenticated API, so it can assert on the
// exact requests the panel sends: the scan, the change feed, and the delete
// body. The phone sheet is checked on the built preview.
//
// Usage: pnpm build && node tools/assets-panel-probe.mjs
import assert from "node:assert/strict";
import { createServer } from "node:http";
import { readFile } from "node:fs/promises";
import { existsSync } from "node:fs";
import { extname, join, resolve } from "node:path";

if (process.env.PLAYWRIGHT_BROWSERS_PATH === undefined &&
    existsSync("/workspace/tools/ms-playwright")) {
  process.env.PLAYWRIGHT_BROWSERS_PATH = "/workspace/tools/ms-playwright";
}
const { browser: getBrowser } = await import("./browser.mjs");
const { eventually, eventuallyEqual } = await import("./eventually.mjs");

const DIST = join(resolve(import.meta.dirname, ".."), "dist");
const MIME = { ".html": "text/html", ".js": "text/javascript", ".css": "text/css", ".svg": "image/svg+xml", ".png": "image/png", ".woff2": "font/woff2", ".webmanifest": "application/manifest+json" };
if (!existsSync(join(DIST, "index.html"))) throw new Error("dist/index.html is missing — run `pnpm build` first");

const project = { id: "project-1", name: "noisedeck", canonical_path: "/workspace/noisedeck", last_used_at: "2026-08-01T00:00:00Z" };
const session = {
  id: "session-1", project_id: project.id, provider: "claude", display_name: "", status: "running", generation: 1,
  recovery_capability: "none", recovery_state: "unavailable", presentation_state: "running",
  created_at: "2026-08-01T00:00:00Z", updated_at: "2026-08-01T00:00:00Z",
};
const secondSession = { ...session, id: "session-2" };
const otherProject = { ...project, id: "project-2", name: "other", canonical_path: "/workspace/other" };
const otherSession = { ...session, id: "session-3", project_id: otherProject.id };

const now = Date.now();
const file = (location, name, size, minutes) => ({
  path: `${location}${name}`, name, kind: "file", size,
  mtime: new Date(now - minutes * 60_000).toISOString(), version: `1-${name}`, locked: null,
});
const folder = (location, name, minutes) => ({
  path: `${location}${name}`, name, kind: "dir", size: 0,
  mtime: new Date(now - minutes * 60_000).toISOString(), version: `1-${name}`, locked: "Folder — view only",
});
const listedEntry = (path, dir, size = 0) => ({
  name: path.slice(path.lastIndexOf("/") + 1), path, dir, size,
  mode: dir ? "rwxr-xr-x" : "rw-r--r--", mtime: new Date(now).toISOString(),
});
const listing = () => ({
  locations: [
    { id: "assets", name: "Assets folder", path: ".deck/assets/", truncated: false, entries: [
      file(".deck/assets/", "home.png", 412_000, 2),
      file(".deck/assets/", "home-mobile.png", 188_000, 2),
      file(".deck/assets/", "icon-512.png", 31_000, 60),
    ].filter((entry) => !gone.has(entry.path)) },
    { id: "build", name: "Build scratch", path: ".deck/build/", truncated: false, entries: [folder(".deck/build/", "worktrees/task-7", 180)] },
    { id: "visual-diff", name: "Visual-diff captures", path: ".deck/visual-diff/", truncated: false, entries: [] },
    { id: "runs", name: "Run receipts", path: ".deck/runs/", truncated: false, entries: [file(".deck/runs/", "run-14.json", 3_000, 8)] },
    { id: "sessions", name: "Session scratch", path: ".deck/sessions/", truncated: false, entries: [] },
  ],
  scanned: new Date(now).toISOString(),
  preview_max_bytes: 16 << 20,
});

const gone = new Set();
let scans = 0;
const imageRequests = new Set();
let imageRequestCount = 0;
let eventStreams = 0;
const deletes = [];
let deleteMode = "answer";
let releaseDelete;
const deleteReleased = new Promise((resolveDelete) => { releaseDelete = resolveDelete; });
let releaseChange;
let changeDelivered = false;
let feedFailure = false;
const changeReleased = new Promise((resolveChange) => { releaseChange = resolveChange; });

const server = createServer(async (req, res) => {
  const url = new URL(req.url, "http://x");
  const path = join(DIST, decodeURIComponent(url.pathname));
  const target = url.pathname === "/" ? join(DIST, "index.html") : path;
  if (!target.startsWith(DIST) || !existsSync(target)) {
    res.writeHead(404);
    res.end("not found");
    return;
  }
  res.writeHead(200, { "Content-Type": MIME[extname(target)] ?? "application/octet-stream" });
  res.end(await readFile(target));
});
await new Promise((r) => server.listen(0, "127.0.0.1", r));
const origin = `http://127.0.0.1:${server.address().port}`;
const quiet = ": connected\n\nretry: 3600000\n\n";

let browser;
const pageErrors = [];
try {
  const { browser: got, via } = await getBrowser();
  browser = got;
  console.log(`browser: ${via}`);
  const context = await browser.newContext({ viewport: { width: 1400, height: 900 } });
  await context.addCookies([{ name: "noisedeck_csrf", value: "probe-csrf", url: origin, sameSite: "Lax" }]);
  const page = await context.newPage();
  page.on("pageerror", (error) => pageErrors.push(error.message));

  await page.route(`${origin}/auth/session`, (route) =>
    route.fulfill({ status: 204, body: "", headers: { "X-Noisedeck-Auth-Mode": "builtin" } }));
  await page.route(`${origin}/api/**`, async (route) => {
    const request = route.request();
    const url = new URL(request.url());
    const path = url.pathname;
    const method = request.method();
    const json = (status, body) => route.fulfill({ status, contentType: "application/json", body: JSON.stringify(body) });
    const stream = (body = quiet) => route.fulfill({ status: 200, contentType: "text/event-stream", body });

    if (method === "GET" && path === "/api/projects") return json(200, [project, otherProject]);
    if (method === "GET" && path === "/api/git-identities") return json(200, {
      profiles: [{ id: "identity-1", name: "Deck probe", email: "deck@example.invalid", revision: 1, created_at: "t", updated_at: "t" }],
      default_profile_id: "identity-1", revision: 1,
    });
    if (method === "GET" && path === `/api/projects/${project.id}/sessions`) return json(200, [session, secondSession]);
    if (method === "GET" && path === `/api/projects/${otherProject.id}/sessions`) return json(200, [otherSession]);
    if (method === "GET" && path === `/api/sessions/${session.id}`) return json(200, session);
    if (method === "GET" && path === `/api/sessions/${secondSession.id}`) return json(200, secondSession);
    if (method === "GET" && path === `/api/sessions/${otherSession.id}`) return json(200, otherSession);
    if (method === "GET" && path === "/api/settings/last-session") return json(404, { code: "not_found", message: "no last session remembered" });
    if (method === "PUT" && path === "/api/settings/last-session") return json(200, request.postDataJSON());
    if (method === "GET" && path === "/api/settings/layout") return json(404, { code: "not_found", message: "no layout saved" });
    if (method === "GET" && path === "/api/harness-login") return json(200, { pending: [], csrf: "login-csrf" });
    if (method === "GET" && path === "/api/providers") return json(200, { providers: ["claude"] });
    if (method === "GET" && path === "/api/settings/tutorial") return json(200, { version: 1, dismissed: true, lesson: "welcome", step: 0, done: [], scope: "operator" });
    if (method === "GET" && path === "/api/settings") return json(200, { skip_permissions: false, skip_close_confirm: false, skip_remove_project_confirm: false });
    if (method === "GET" && path === "/api/session-activity/events") return stream();
    if (/^\/api\/projects\/[^/]+\/lanes$/.test(path)) return json(200, { lanes: [] });
    if (method === "GET" && /^\/api\/projects\/[^/]+\/taskmaster$/.test(path)) return json(404, { error: "not_taskmaster_project", message: "not a Taskmaster project" });
    if (method === "GET" && /^\/api\/projects\/[^/]+\/taskmaster\/events$/.test(path)) return stream();
    if (method === "GET" && path === `/api/projects/${otherProject.id}/files`) return json(200, { path: "", at_root: true, entries: [] });
    if (method === "GET" && path === `/api/projects/${otherProject.id}/files/events`) return stream();
    if (method === "GET" && path === `/api/projects/${otherProject.id}/assets`) return json(200, listing());
    if (method === "GET" && path === `/api/projects/${otherProject.id}/assets/events`) return stream();
    if (method === "GET" && path === `/api/projects/${project.id}/files`) {
      const dir = url.searchParams.get("path") ?? "";
      const entries = {
        "": [listedEntry(".deck", true, 4096)],
        ".deck": [listedEntry(".deck/assets", true, 4096), listedEntry(".deck/build", true, 4096), listedEntry(".deck/runs", true, 4096)],
        ".deck/assets": [listedEntry(".deck/assets/home.png", false, 412_000)],
        ".deck/runs": [listedEntry(".deck/runs/run-14.json", false, 3_000)],
      }[dir];
      if (entries === undefined) return json(404, { code: "not_found", message: "no such directory" });
      return json(200, { path: dir, at_root: dir === "", entries });
    }
    if (method === "GET" && path === `/api/projects/${project.id}/files/view`) {
      const filePath = url.searchParams.get("path");
      if (filePath === ".deck/runs/run-14.json") return json(200, { content: '{"run":14,"status":"passed"}\n' });
      return json(415, { code: "not_text", message: "file is not text" });
    }
    if (method === "GET" && path === `/api/projects/${project.id}/files/events`) return stream();
    if (method === "GET" && /^\/api\/sessions\/[^/]+\/chats\/events$/.test(path)) return stream();
    if (method === "GET" && /^\/api\/sessions\/[^/]+\/chats$/.test(path)) return json(200, { agent: "claude-sess", chats: [] });
    if (method === "GET" && path === "/api/chats/status") return json(200, { available: true });
    if (method === "GET" && path === "/api/chats") return json(200, { chats: [] });

    if (method === "GET" && path === `/api/projects/${project.id}/files/image`) {
      imageRequests.add(new URL(request.url()).searchParams.get("path"));
      imageRequestCount++;
      return json(404, { error: "not_found", message: "no such file in the project" });
    }
    if (method === "GET" && path === `/api/projects/${project.id}/assets`) {
      scans++;
      return json(200, listing());
    }
    if (method === "GET" && path === `/api/projects/${project.id}/assets/events`) {
      eventStreams++;
      if (feedFailure) return json(200, { error: "stream_unavailable" });
      // Every connection made before the probe releases one change holds
      // until then and carries it; a closed panel's connection is already
      // gone and drops it. Connections after the release are quiet.
      if (changeDelivered) return stream();
      await changeReleased;
      changeDelivered = true;
      return stream(`${quiet}event: assets-changed\ndata: changed\n\n`).catch(() => {});
    }
    if (method === "POST" && path === `/api/projects/${project.id}/assets/delete`) {
      assert.equal(request.headers()["x-csrf-token"], "probe-csrf");
      const body = request.postDataJSON();
      deletes.push(body);
      if (deleteMode === "abort") return route.abort("connectionreset");
      if (deleteMode === "hold") await deleteReleased;
      return json(200, { results: body.files.map((f) => {
        if (f.path.endsWith("home.png")) {
          gone.add(f.path);
          return { path: f.path, outcome: "deleted" };
        }
        return { path: f.path, outcome: "skipped", reason: "The workspace refused: permission denied" };
      }) });
    }
    throw new Error(`unhandled ${method} ${path}`);
  });
  await page.routeWebSocket(`${origin.replace("http", "ws")}/api/sessions/*/terminal?*`, (socket) => {
    socket.send(JSON.stringify({ type: "attached", token: 1, generation: 1 }));
  });

  await page.goto(origin, { waitUntil: "networkidle" });
  await page.locator("[data-live-terminal]").waitFor();

  // ---- the trigger, after Chats; closed, it reads only for its count ----
  const trigger = page.locator("[data-live-assets]");
  await trigger.waitFor();
  const order = await page.evaluate(() => [...document.querySelectorAll("[data-desktop-panels] button")]
    .map((button) => button.getAttribute("aria-label")));
  assert.deepEqual(order, ["Files", "Project tasks", "Chats", "Assets"], "Assets is the fourth trigger");
  assert.equal(await trigger.getAttribute("data-hint"), "Review files agents saved for you");
  // The closed panel follows the feed for the badge (assets-news-probe);
  // its one read renders no rows.
  await eventuallyEqual(() => scans, 1, "the closed panel reads the listing once for its count");
  await eventuallyEqual(() => eventStreams, 1, "the closed panel follows the change feed for its count");
  assert.equal(await page.locator('[data-region="assets"] [data-assets-list]').count(), 0, "a closed panel renders no rows");

  const region = page.locator('[data-region="assets"]');
  await trigger.click();
  await page.waitForFunction(() => document.querySelector('[data-region="assets"]')?.getAttribute("data-open") === "true");
  assert.equal(await trigger.getAttribute("aria-pressed"), "true");
  await region.locator("[data-assets-list]").waitFor();
  const width = (await region.boundingBox()).width;
  assert.ok(Math.abs(width - 400) <= 1.5, `the panel is 400px wide, not ${width}`);
  await eventuallyEqual(() => eventStreams, 2, "the open panel follows its own change feed");
  assert.equal(scans, 2);

  // The Assets panel rides the shared docked-panel resizing. Its 400px
  // default sits at the family minimum, so the drag widens; Enter returns
  // the family width and clears the saved preference.
  const assetsHandle = page.locator('[data-panel-resize="assets"]');
  await assetsHandle.waitFor();
  const assetsSaved = () => page.evaluate(() => localStorage.getItem("noisedeck.panel.assets.width"));
  assert.equal(await assetsSaved(), null, "no Assets width is saved before the user resizes");
  const beforeAssetsDrag = await region.boundingBox();
  await page.mouse.move(beforeAssetsDrag.x + 4, beforeAssetsDrag.y + 100);
  await page.mouse.down();
  await page.mouse.move(beforeAssetsDrag.x + 4 - 80, beforeAssetsDrag.y + 100, { steps: 6 });
  await page.mouse.up();
  const widenedAssets = await region.boundingBox();
  assert.ok(Math.abs(widenedAssets.width - (beforeAssetsDrag.width + 80)) <= 1.5,
    `an 80px left drag widens the panel 80px, not ${widenedAssets.width - beforeAssetsDrag.width}`);
  assert.equal(await assetsSaved(), String(Math.round(beforeAssetsDrag.width + 80)), "the drag commits the width");
  await assetsHandle.focus();
  await page.keyboard.press("Enter");
  const resetAssets = await region.boundingBox();
  assert.ok(Math.abs(resetAssets.width - beforeAssetsDrag.width) <= 1.5, "Enter returns the family width");
  assert.equal(await assetsSaved(), null, "Enter clears the saved width");

  // Only image files ask the image route for a thumbnail; one the route
  // cannot serve keeps the file glyph.
  await eventually(() => [...imageRequests].sort(), (paths) => paths.length === 3, "every listed image asks for its thumbnail");
  assert.deepEqual([...imageRequests].sort(), [".deck/assets/home-mobile.png", ".deck/assets/home.png", ".deck/assets/icon-512.png"]);
  await eventually(() => region.locator("img[data-asset-img]").count(), (n) => n === 0, "a thumbnail the route refuses falls back to the glyph");

  // ---- at most one panel open ----
  await page.locator("[data-live-files]").click();
  await page.waitForFunction(() => document.querySelector('[data-region="files"]')?.getAttribute("data-open") === "true");
  assert.equal(await region.getAttribute("data-open"), "false", "opening Files closes Assets");
  await trigger.click();
  await page.waitForFunction(() => document.querySelector('[data-region="assets"]')?.getAttribute("data-open") === "true");
  const open = await page.evaluate(() => [...document.querySelectorAll('aside[data-open="true"]')].map((aside) => aside.dataset.region));
  assert.deepEqual(open, ["assets"], "exactly one docked panel is open");
  // Closing Assets for Files restarts the count's read; reopening rescans.
  await eventuallyEqual(() => scans, 4, "reopening rescans");

  // ---- selection ----
  const row = (path) => region.locator(`[data-asset-path="${path}"]`);
  const select = (path) => region.locator(`[data-asset-select="${path}"]`);
  const summary = region.locator("[data-assets-summary]");
  assert.equal(await summary.textContent(), "4 files · 634.0 KB");
  assert.ok(await region.locator("[data-assets-delete]").isDisabled(), "Delete… is disabled with nothing selected");
  assert.equal(await region.locator("[data-assets-clear]").count(), 0, "Clear shows only with a selection");
  await select(".deck/assets/home.png").click();
  await select(".deck/runs/run-14.json").click();
  assert.equal(await select(".deck/assets/home.png").getAttribute("aria-checked"), "true");
  assert.equal(await region.getAttribute("data-open"), "true", "selecting never opens the Files panel");
  assert.equal(await summary.textContent(), "2 selected · 415.0 KB");
  assert.equal(await region.locator("[data-assets-clear]").count(), 1);

  assert.equal(imageRequestCount, 3, "a repaint does not ask again for a thumbnail the route refused");

  // A locked row still opens, but exposes no delete-selection control.
  const locked = row(".deck/build/worktrees/task-7");
  assert.equal(await locked.locator("[data-asset-open]").getAttribute("aria-label"), "worktrees/task-7, Folder, Folder — view only");
  assert.equal(await locked.locator("[data-asset-select]").count(), 0);
  assert.equal(await summary.textContent(), "2 selected · 415.0 KB");

  // Space toggles a focused checkbox and keeps focus on it across the repaint.
  await select(".deck/assets/home-mobile.png").focus();
  await page.keyboard.press(" ");
  assert.equal(await summary.textContent(), "3 selected · 603.0 KB");
  assert.equal(await page.evaluate(() => document.activeElement?.getAttribute("data-asset-select")), ".deck/assets/home-mobile.png");
  await page.keyboard.press(" ");
  assert.equal(await summary.textContent(), "2 selected · 415.0 KB");

  // ---- the consent: Escape cancels with nothing sent ----
  await region.locator("[data-assets-delete]").click();
  const confirm = page.locator('[data-region="modals"] [role="alertdialog"][data-assets-dialog="confirm"]');
  await confirm.waitFor();
  assert.equal(await confirm.locator("#assets-del-title").textContent(), "Delete 2 files?");
  assert.ok(await confirm.evaluate((dialog) => dialog.contains(document.activeElement)), "focus moves into the consent");
  assert.deepEqual(await confirm.locator("[data-assets-target]").evaluateAll((rows) => rows.map((r) => r.dataset.assetsTarget)),
    [".deck/assets/home.png", ".deck/runs/run-14.json"]);
  await page.keyboard.press("Escape");
  await confirm.waitFor({ state: "detached" });
  assert.equal(deletes.length, 0, "Escape sends nothing");
  assert.equal(await summary.textContent(), "2 selected · 415.0 KB", "and keeps the selection");

  // ---- confirm: exactly the selected path and version pairs ----
  // While the answer is pending, focus stays trapped in the consent and
  // Escape does not walk away from a request the workspace may be acting on.
  deleteMode = "hold";
  await region.locator("[data-assets-delete]").click();
  await confirm.waitFor();
  await confirm.locator("[data-assets-confirm]").click();
  await eventuallyEqual(() => deletes.length, 1, "the confirm sends the delete");
  const focusInConfirm = () => confirm.evaluate((dialog) => dialog.contains(document.activeElement));
  assert.ok(await focusInConfirm(), "a pending delete keeps focus in the consent");
  await page.keyboard.press("Tab");
  assert.ok(await focusInConfirm(), "Tab stays in the consent while the delete is pending");
  await page.keyboard.press("Shift+Tab");
  assert.ok(await focusInConfirm(), "Shift+Tab stays in the consent while the delete is pending");
  await page.keyboard.press("Escape");
  assert.equal(await confirm.count(), 1, "Escape does not close the consent while the delete is pending");
  assert.ok(await focusInConfirm());
  releaseDelete();
  deleteMode = "answer";
  const outcome = page.locator('[data-region="modals"] [role="dialog"][data-assets-dialog="outcome"]');
  await outcome.waitFor();
  assert.ok(await outcome.evaluate((dialog) => dialog.contains(document.activeElement)), "the outcome takes focus when the answer lands");
  assert.deepEqual(deletes, [{ files: [
    { path: ".deck/assets/home.png", version: "1-home.png" },
    { path: ".deck/runs/run-14.json", version: "1-run-14.json" },
  ] }]);
  assert.equal(await outcome.locator("#assets-out-title").textContent(), "1 of 2 files deleted");
  assert.equal(await outcome.locator(".dialog-desc").textContent(), "One was skipped and nothing was done to it.");
  assert.deepEqual(await outcome.locator("[data-assets-outcome]").evaluateAll((rows) => rows.map((r) => r.dataset.assetsOutcome)), ["deleted", "skipped"]);
  await outcome.locator("[data-assets-done]").click();
  await outcome.waitFor({ state: "detached" });
  assert.equal(await row(".deck/assets/home.png").count(), 0, "a deleted file leaves the list");
  assert.equal(await row(".deck/runs/run-14.json").locator("[data-asset-fact]").textContent(),
    "Skipped — the workspace refused: permission denied", "a skipped file keeps its outcome after Done");
  assert.equal(await summary.textContent(), "3 files · 222.0 KB");

  // ---- a request that never answers leaves every file unconfirmed ----
  deleteMode = "abort";
  await select(".deck/assets/home-mobile.png").click();
  await select(".deck/assets/icon-512.png").click();
  await region.locator("[data-assets-delete]").click();
  await confirm.waitFor();
  await confirm.locator("[data-assets-confirm]").click();
  await outcome.waitFor();
  assert.deepEqual(await outcome.locator("[data-assets-outcome]").evaluateAll((rows) => rows.map((r) => r.dataset.assetsOutcome)), ["unconfirmed", "unconfirmed"]);
  assert.equal(await outcome.locator(".dialog-desc").textContent(), "Two deletes are unconfirmed — rescan to see whether they're still there.");
  await page.keyboard.press("Escape");
  await outcome.waitFor({ state: "detached" });
  assert.equal(await row(".deck/assets/icon-512.png").locator("[data-asset-fact]").textContent(), "Unconfirmed — delete sent, no answer");

  // ---- assets-changed rescans and clears the outcomes ----
  const before = scans;
  releaseChange();
  await eventually(() => scans, (n) => n > before, "an assets-changed event rescans");
  await eventually(() => region.locator("[data-asset-fact]").evaluateAll((facts) => facts.map((f) => f.textContent)),
    (facts) => facts.length === 1 && facts[0] === "Folder — view only", "the rescan clears every outcome; only the lock fact remains");

  // ---- a permanently closed feed polls instead of leaving stale rows ----
  await page.clock.install();
  feedFailure = true;
  await page.reload({ waitUntil: "networkidle" });
  await page.locator("[data-live-terminal]").waitFor();
  await trigger.click();
  await region.locator('[data-asset-path=".deck/assets/icon-512.png"]').waitFor();
  const beforePoll = scans;
  gone.add(".deck/assets/icon-512.png");
  await page.clock.runFor(30_000);
  await eventually(() => scans, (n) => n > beforePoll, "the polling interval triggers another scan");
  await region.locator('[data-asset-path=".deck/assets/icon-512.png"]').waitFor({ state: "detached" });

  // ---- opening an asset uses the Files viewer ----
  await row(".deck/runs/run-14.json").locator("[data-asset-open]").click();
  const filesRegion = page.locator('[data-region="files"]');
  await filesRegion.locator("[data-files-text]").waitFor();
  assert.equal(await filesRegion.getAttribute("data-open"), "true");
  assert.equal(await region.getAttribute("data-open"), "false");
  assert.equal(await filesRegion.locator("[data-files-list-scroll]").getAttribute("data-files-dir"), ".deck/runs");
  assert.equal(await filesRegion.locator("[data-files-viewer-name]").textContent(), "run-14.json");
  assert.equal(await filesRegion.locator("[data-files-text]").textContent(), '{"run":14,"status":"passed"}\n');

  // ---- a same-project session switch keeps Files and its viewer open ----
  const secondSelected = page.waitForResponse((response) =>
    response.request().method() === "GET" && new URL(response.url()).pathname === `/api/sessions/${secondSession.id}`);
  await page.locator(`[data-session="${secondSession.id}"]`).click();
  await secondSelected;
  await filesRegion.locator('[data-files-list-scroll][data-files-dir=".deck/runs"]').waitFor();
  assert.equal(await filesRegion.getAttribute("data-open"), "true");
  assert.equal(await filesRegion.locator(".crumb-current").textContent(), "runs");
  assert.equal(await filesRegion.locator("[data-files-viewer-name]").textContent(), "run-14.json");
  assert.equal(await filesRegion.locator("[data-files-text]").textContent(), '{"run":14,"status":"passed"}\n');

  // ---- a session in another project closes Files ----
  if (await page.locator(`[data-session="${otherSession.id}"]`).count() === 0) {
    await page.locator(`[data-project-toggle="${otherProject.id}"]`).click();
  }
  const otherSelected = page.waitForResponse((response) =>
    response.request().method() === "GET" && new URL(response.url()).pathname === `/api/sessions/${otherSession.id}`);
  await page.locator(`[data-session="${otherSession.id}"]`).evaluate((row) => row.click());
  await otherSelected;
  await page.waitForFunction(() => document.querySelector('[data-region="files"]')?.getAttribute("data-open") === "false");

  assert.deepEqual(pageErrors, [], "the live page threw nothing");

  // ---- in compact chrome the Panels menu carries Assets as its fourth row ----
  const compact = await browser.newContext({ viewport: { width: 760, height: 720 } });
  const menuPage = await compact.newPage();
  menuPage.on("pageerror", (error) => pageErrors.push(error.message));
  await menuPage.goto(`${origin}/preview.html?menu=panels`, { waitUntil: "networkidle" });
  await menuPage.waitForSelector("html[data-preview-state]");
  assert.equal(await menuPage.locator("html[data-preview-error]").count(), 0);
  const menu = menuPage.locator('[data-region="popovers"] [data-panels-menu]');
  assert.deepEqual(await menu.locator("[data-panel-choice]").evaluateAll((rows) => rows.map((r) => r.dataset.panelChoice)), ["files", "tasks", "chats", "assets"]);
  await menu.locator('[data-panel-choice="assets"]').click();
  await menuPage.waitForFunction(() => document.querySelector('[data-region="assets"]')?.getAttribute("data-open") === "true");
  assert.deepEqual(await menuPage.evaluate(() => [...document.querySelectorAll('aside[data-open="true"]')].map((aside) => aside.dataset.region)), ["assets"]);
  await compact.close();

  // ---- the phone close stays below a notch's safe area and taps shut ----
  const phone = await browser.newContext({ viewport: { width: 393, height: 852 }, hasTouch: true });
  const closePage = await phone.newPage();
  closePage.on("pageerror", (error) => pageErrors.push(error.message));
  await closePage.goto(`${origin}/preview.html?panel=assets&assets=list&frame=phone`, { waitUntil: "networkidle" });
  await closePage.waitForSelector("html[data-preview-state]");
  await closePage.locator("[data-workspace]").evaluate((workspace) => workspace.style.setProperty("--workspace-safe-area-top", "47px"));
  const closeHeader = closePage.locator("[data-assets-header]");
  assert.equal(await closeHeader.evaluate((header) => getComputedStyle(header).paddingTop), "51px");
  const closeBox = await closePage.locator("[data-assets-close]").boundingBox();
  const closeAppBox = await closePage.locator("#app").boundingBox();
  assert.ok(closeBox.y >= closeAppBox.y + 47, "the close control starts below the top safe area");
  await closePage.locator("[data-assets-close]").tap();
  await closePage.waitForFunction(() => document.querySelector('[data-region="assets"]')?.getAttribute("data-open") === "false");
  await closePage.close();

  // ---- the phone consent is a bottom sheet with stacked actions ----
  const sheetPage = await phone.newPage();
  sheetPage.on("pageerror", (error) => pageErrors.push(error.message));
  await sheetPage.goto(`${origin}/preview.html?panel=assets&assets=selected&dialog=delete-assets&frame=phone`, { waitUntil: "networkidle" });
  await sheetPage.waitForSelector("html[data-preview-state]");
  assert.equal(await sheetPage.locator("html[data-preview-error]").count(), 0);
  const sheet = sheetPage.locator('[data-assets-dialog="confirm"]');
  const sheetBox = await sheet.boundingBox();
  const appBox = await sheetPage.locator("#app").boundingBox();
  assert.ok(Math.abs(sheetBox.y + sheetBox.height - (appBox.y + appBox.height)) <= 1.5, "the sheet sits on the bottom edge");
  assert.ok(Math.abs(sheetBox.width - appBox.width) <= 1.5, "the sheet fills the width");
  const danger = await sheet.locator("[data-assets-confirm]").boundingBox();
  const cancel = await sheet.locator("[data-assets-cancel]").boundingBox();
  assert.ok(danger.y < cancel.y, "Delete stacks above Cancel");
  assert.ok(Math.abs(danger.width - cancel.width) <= 0.5 && danger.height >= 48 - 0.5, "full-width 48px actions");
  await phone.close();

  assert.deepEqual(pageErrors, [], "no page threw");
  console.log("assets-panel-probe: ok");
} finally {
  await browser?.close();
  server.close();
}
