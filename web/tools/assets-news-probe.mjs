// Deterministic browser probe for the Assets count: the badge on the
// trigger, the info bar's news line, and how opening the panel clears both.
// Drives the BUILT live page against a mocked authenticated API whose
// assets listing and change feed the probe controls, so it can land files
// with chosen mtimes while the panel is closed.
//
// Usage: pnpm build && node tools/assets-news-probe.mjs
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
const secondSession = { ...session, id: "session-2", display_name: "Second session" };
const seenKey = `noisedeck.assets.seen.${project.id}`;

// mtimes are whole seconds, as the server reports them.
const ago = (seconds) => new Date(Math.floor(Date.now() / 1000) * 1000 - seconds * 1000).toISOString().replace(".000Z", "Z");
const files = new Map();
const land = (name, secondsAgo) => {
  const mtime = ago(secondsAgo);
  files.set(name, { path: `.deck/assets/${name}`, name, kind: "file", size: 1000, mtime, version: `1-${name}-${mtime}`, locked: null });
  return mtime;
};
const listing = () => ({
  locations: [
    { id: "assets", name: "Assets folder", path: ".deck/assets/", truncated: false, entries: [...files.values()] },
    { id: "build", name: "Build scratch", path: ".deck/build/", truncated: false, entries: [] },
    { id: "visual-diff", name: "Visual-diff captures", path: ".deck/visual-diff/", truncated: false, entries: [] },
    { id: "runs", name: "Run receipts", path: ".deck/runs/", truncated: false, entries: [] },
    { id: "sessions", name: "Session scratch", path: ".deck/sessions/", truncated: false, entries: [] },
  ],
  scanned: ago(0),
  preview_max_bytes: 16 << 20,
});

let chats = [];
const liveChat = () => ({
  id: "chat-1", state: "open", waiting_s: null, forced: false, opened_by: "claude-sess", opened_at: ago(5), last: ago(5),
  members: ["claude-sess", "codex-9f2a"], left: [], remaining: [], close_requested_by: null, closed_by: null,
  messages: 0, rows: [], handshake: [],
});

// Each held stream request carries the next event of its kind and then
// ends; its retry field brings the browser straight back for the next one.
const held = { assets: [], chats: [] };
const fire = (kind) => {
  const waiting = held[kind].splice(0);
  for (const release of waiting) release();
  return waiting.length;
};
let scans = 0;
let assetStreams = 0;

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
  // Every arrival wash started on the controls that carry the count, the
  // colour it starts from, and when each one runs to its end.
  const recordArrivals = () => {
    window.__arrivals = [];
    window.__washTints = [];
    window.__washDurations = [];
    window.__washEnds = [];
    const animate = Element.prototype.animate;
    Element.prototype.animate = function (keyframes, options) {
      const animation = animate.call(this, keyframes, options);
      if (this.closest("[data-live-assets], [data-live-panels]") !== null) {
        window.__arrivals.push(this.closest("[data-live-assets]") !== null ? "assets" : "panels");
        window.__washTints.push(keyframes[0]?.backgroundColor ?? "");
        window.__washDurations.push(options?.duration);
        animation.finished.then(() => window.__washEnds.push(performance.now()), () => {});
      }
      return animation;
    };
  };
  await context.addInitScript(recordArrivals);
  const page = await context.newPage();
  const arrivals = (target = page) => target.evaluate(() => window.__arrivals);
  page.on("pageerror", (error) => pageErrors.push(error.message));

  await page.route(`${origin}/auth/session`, (route) =>
    route.fulfill({ status: 204, body: "", headers: { "X-Noisedeck-Auth-Mode": "builtin" } }));
  const apiRoute = async (route) => {
    const request = route.request();
    const path = new URL(request.url()).pathname;
    const method = request.method();
    const json = (status, body) => route.fulfill({ status, contentType: "application/json", body: JSON.stringify(body) });
    const stream = (body = quiet) => route.fulfill({ status: 200, contentType: "text/event-stream", body });
    const hold = async (kind, event) => {
      await new Promise((release) => held[kind].push(release));
      return stream(`: connected\n\nretry: 50\n\nevent: ${event}\ndata: changed\n\n`).catch(() => {});
    };

    if (method === "GET" && path === "/api/projects") return json(200, [project]);
    if (method === "GET" && path === "/api/git-identities") return json(200, {
      profiles: [{ id: "identity-1", name: "Deck probe", email: "deck@example.invalid", revision: 1, created_at: "t", updated_at: "t" }],
      default_profile_id: "identity-1", revision: 1,
    });
    if (method === "GET" && path === `/api/projects/${project.id}/sessions`) return json(200, [session, secondSession]);
    if (method === "GET" && path === `/api/sessions/${session.id}`) return json(200, session);
    if (method === "GET" && path === `/api/sessions/${secondSession.id}`) return json(200, secondSession);
    if (method === "GET" && path === "/api/settings/last-session") return json(404, { code: "not_found", message: "no last session remembered" });
    if (method === "PUT" && path === "/api/settings/last-session") return json(200, request.postDataJSON());
    if (method === "GET" && path === "/api/settings/layout") return json(404, { code: "not_found", message: "no layout saved" });
    if (method === "GET" && path === "/api/harness-login") return json(200, { pending: [], csrf: "login-csrf" });
    if (method === "GET" && path === "/api/providers") return json(200, { providers: ["claude"] });
    if (method === "GET" && path === "/api/settings/tutorial") return json(200, { version: 1, dismissed: true, lesson: "welcome", step: 0, done: [], scope: "user" });
    if (method === "GET" && path === "/api/settings") return json(200, { skip_permissions: false, skip_close_confirm: false, skip_remove_project_confirm: false });
    if (method === "GET" && path === "/api/session-activity/events") return stream();
    if (/^\/api\/projects\/[^/]+\/lanes$/.test(path)) return json(200, { lanes: [] });
    if (method === "GET" && path === `/api/projects/${project.id}/taskmaster`) return json(404, { error: "not_taskmaster_project", message: "not a Taskmaster project" });
    if (method === "GET" && path === `/api/projects/${project.id}/taskmaster/events`) return stream();
    if (method === "GET" && path === `/api/projects/${project.id}/files`) return json(200, { path: "", at_root: true, entries: [] });
    if (method === "GET" && path === `/api/projects/${project.id}/files/events`) return stream();
    if (method === "GET" && path === `/api/projects/${project.id}/files/image`) return json(404, { error: "not_found", message: "no such file in the project" });
    if (method === "GET" && /^\/api\/sessions\/[^/]+\/chats\/events$/.test(path)) return hold("chats", "chats-changed");
    if (method === "GET" && /^\/api\/sessions\/[^/]+\/chats$/.test(path)) return json(200, { agent: "claude-sess", chats });
    if (method === "GET" && path === "/api/chats/status") return json(200, { available: true });
    if (method === "GET" && path === "/api/chats") return json(200, { chats: [] });
    if (method === "GET" && path === `/api/projects/${project.id}/assets`) {
      scans++;
      return json(200, listing());
    }
    if (method === "GET" && path === `/api/projects/${project.id}/assets/events`) {
      assetStreams++;
      return hold("assets", "assets-changed");
    }
    throw new Error(`unhandled ${method} ${path}`);
  };
  await page.route(`${origin}/api/**`, apiRoute);
  await page.routeWebSocket(`${origin.replace("http", "ws")}/api/sessions/*/terminal?*`, (socket) => {
    socket.send(JSON.stringify({ type: "attached", token: 1, generation: 1 }));
  });

  const trigger = page.locator("[data-live-assets]");
  const badge = trigger.locator("[data-assets-badge]");
  const status = page.locator("[data-info-status]");
  // The bar speaks the hovered or focused control's hint over the news, so
  // the resting line is read with the pointer parked on the terminal and
  // focus taken off the clicked trigger.
  const statusText = async () => {
    await page.mouse.move(300, 500);
    await page.evaluate(() => document.activeElement instanceof HTMLElement && document.activeElement.blur());
    return status.textContent();
  };
  const landAndFire = async (name, secondsAgo) => {
    const before = scans;
    const mtime = land(name, secondsAgo);
    await eventually(() => fire("assets"), (n) => n > 0, "the closed panel's change feed is connected");
    await eventually(() => scans, (n) => n > before, "an assets-changed event re-reads the listing");
    return mtime;
  };
  const expectCount = async (count, why) => {
    await eventually(() => trigger.getAttribute("data-assets-new"), (value) => value === String(count), why);
    assert.equal(await trigger.getAttribute("aria-label"), count > 0 ? `Assets, ${count} new` : "Assets");
    assert.equal(await badge.isVisible(), count > 0, `the badge ${count > 0 ? "shows" : "hides"}`);
    if (count > 0) assert.equal(await badge.textContent(), String(count));
  };

  // ---- a first visit counts nothing: the history is never news ----
  land("old.png", 600);
  await page.goto(origin, { waitUntil: "networkidle" });
  await page.locator("[data-live-terminal]").waitFor();
  await eventuallyEqual(() => scans, 1, "the closed panel reads the listing once for its count");
  await expectCount(0, "a first visit badges nothing");
  assert.equal(await page.evaluate((key) => localStorage.getItem(key), seenKey), files.get("old.png").mtime,
    "the first visit remembers the newest listed file");
  assert.equal(await statusText(), "");
  assert.deepEqual(await arrivals(), [], "a first read is not an arrival");

  // ---- files that land while the panel is closed badge and speak ----
  await landAndFire("chart.png", 60);
  await landAndFire("home.png", 0);
  const secondRead = await page.evaluate(() => performance.now());
  await expectCount(2, "two files landed after the last visit");
  assert.equal(await statusText(), "2 new assets · newest just now");
  assert.equal(await status.getAttribute("data-mode"), "news");
  const visual = trigger.locator("[data-panel-trigger-visual]");
  await eventually(arrivals, (seen) => seen.includes("assets"), "an arrival washes the trigger");
  assert.ok(await page.evaluate(() => window.__washTints.every((tint) => tint !== "" && CSS.supports("color", tint))),
    "the wash starts from the resolved accent tint");
  assert.deepEqual([...new Set(await page.evaluate(() => window.__washDurations))], [600], "the wash lasts the notice duration");
  // The second file can land while the first wash is still playing. A
  // restart that cancels itself leaves no wash at all, so one must run to
  // its end after the second read.
  await eventually(() => page.evaluate(() => window.__washEnds), (ends) => ends.some((at) => at > secondRead),
    "a wash runs to its end after the second file");
  assert.equal(await page.locator('[data-region="assets"]').getAttribute("data-open"), "false", "files never open the panel");
  await page.emulateMedia({ reducedMotion: "reduce" });
  assert.match(await page.evaluate(() => getComputedStyle(document.documentElement).getPropertyValue("--dur-notice").trim()), /^0m?s$/,
    "reduced motion zeroes the wash, leaving only the badge");
  await page.emulateMedia({ reducedMotion: "no-preference" });
  const accent = await page.evaluate(() => getComputedStyle(document.documentElement).getPropertyValue("--color-accent").trim());
  assert.ok(accent !== "", "the accent token resolves");
  const badgeBg = await badge.evaluate((el) => getComputedStyle(el).backgroundColor);
  await eventuallyEqual(() => visual.evaluate((el) => getComputedStyle(el).color), badgeBg,
    "the trigger takes the accent its badge is drawn in");
  const badgeFont = await badge.evaluate((el) => getComputedStyle(el).fontFamily);
  assert.ok(!/mono/i.test(badgeFont), `the count is interface voice, not ${badgeFont}`);
  const tBox = await trigger.boundingBox();
  const bBox = await badge.boundingBox();
  assert.ok(Math.abs(bBox.height - 14) <= 0.5 && bBox.width >= 13.5, `a 14px pill, not ${bBox.width}x${bBox.height}`);
  assert.ok(Math.abs(bBox.y - (tBox.y - 3)) <= 0.5 && Math.abs(bBox.x + bBox.width - (tBox.x + tBox.width + 5)) <= 0.5,
    "the badge sits 3px above and 5px past the trigger's corner");

  // ---- the one news slot: the newer item speaks; clearing falls back ----
  chats = [liveChat()];
  await eventually(() => fire("chats"), (n) => n > 0, "the chats feed is connected");
  await eventually(statusText, (text) => text === "claude-sess is in a chat with codex-9f2a", "a chat that starts later takes the line");
  chats = [];
  await eventually(() => fire("chats"), (n) => n > 0, "the chats feed reconnected");
  await eventually(statusText, (text) => text === "2 new assets · newest just now", "the chat ending hands the line back to the files");

  // ---- opening the panel clears both and records what was seen ----
  await trigger.click();
  await page.locator('[data-region="assets"] [data-assets-list]').waitFor();
  await expectCount(0, "an open panel has nothing unseen");
  assert.equal(await statusText(), "", "the news line goes with the badge");
  assert.equal(await page.evaluate((key) => localStorage.getItem(key), seenKey), files.get("home.png").mtime,
    "last seen is the newest listed mtime");

  // ---- and they stay cleared across a reload ----
  await page.reload({ waitUntil: "networkidle" });
  await page.locator("[data-live-terminal]").waitFor();
  const afterReload = scans;
  await page.locator("[data-region='assets'][data-open='false']").waitFor({ state: "attached" });
  await eventually(() => scans, (n) => n >= afterReload, "the reload reads the listing");
  await expectCount(0, "seen files stay seen after a reload");
  assert.equal(await statusText(), "");

  // ---- an older file never badges; a later one does, alone ----
  await landAndFire("backfill.png", 3600);
  await expectCount(0, "a file older than the last seen is not new");
  const washed = (await arrivals()).length;
  await landAndFire("latest.png", 0);
  await expectCount(1, "a later file badges again with a count of one");
  // Resize while the wash is still playing: neither the control the layout
  // reveals nor the trigger coming back starts another.
  await eventually(arrivals, (seen) => seen.slice(washed).includes("assets"), "the later file washes the trigger");
  await page.waitForTimeout(100);
  const rewashed = (await arrivals()).length;
  await page.setViewportSize({ width: 760, height: 900 });
  await page.locator("[data-live-panels]").waitFor({ state: "visible" });
  await page.waitForTimeout(800);
  await page.setViewportSize({ width: 1400, height: 900 });
  await trigger.waitFor({ state: "visible" });
  await page.waitForTimeout(800);
  assert.equal((await arrivals()).length, rewashed, "a layout change replays no wash");
  assert.equal(await statusText(), "1 new asset · just now");

  // ---- a session switch inside the project keeps the feed and the count ----
  // The last event ended its stream; wait for the browser's reconnect and
  // the re-read it triggers, so only the switch can move the counters.
  await eventually(() => held.assets.length, (n) => n === 1, "the feed has reconnected");
  await page.waitForTimeout(500);
  const streamsBefore = assetStreams;
  const scansBefore = scans;
  await page.evaluate(() => {
    const seen = [];
    window.__assetsCounts = seen;
    new MutationObserver(() => {
      const value = document.querySelector("[data-live-assets]")?.getAttribute("data-assets-new");
      if (value !== undefined && value !== null) seen.push(value);
    }).observe(document.body, { subtree: true, attributes: true, childList: true, attributeFilter: ["data-assets-new"] });
  });
  await page.locator(`[data-session="${secondSession.id}"]`).click();
  await eventually(() => page.evaluate(() => document.querySelector("[data-live-terminal]") !== null && location.href), (href) => href !== false, "the second session mounts");
  await page.waitForTimeout(500);
  await expectCount(1, "the count survives the switch");
  assert.deepEqual([...new Set(await page.evaluate(() => window.__assetsCounts))], ["1"], "the badge never blinks to zero");
  assert.equal(assetStreams, streamsBefore, "the same project does not resubscribe");
  assert.equal(scans, scansBefore, "and does not rescan");
  assert.equal(await statusText(), "1 new asset · just now");

  // ---- the compact chrome: the Panels button and its Assets row ----
  // A narrow window that last saw the project before chart.png landed:
  // chart, home and latest are new; backfill is older than what it saw.
  const compact = await browser.newContext({ viewport: { width: 760, height: 720 } });
  await compact.addInitScript(recordArrivals);
  await compact.addCookies([{ name: "noisedeck_csrf", value: "probe-csrf", url: origin, sameSite: "Lax" }]);
  await compact.addInitScript(([key, value]) => {
    if (sessionStorage.getItem("seeded") === null) {
      localStorage.setItem(key, value);
      sessionStorage.setItem("seeded", "1");
    }
  }, [seenKey, files.get("old.png").mtime]);
  const narrow = await compact.newPage();
  narrow.on("pageerror", (error) => pageErrors.push(error.message));
  await compact.route(`${origin}/auth/session`, (route) =>
    route.fulfill({ status: 204, body: "", headers: { "X-Noisedeck-Auth-Mode": "builtin" } }));
  await compact.route(`${origin}/api/**`, apiRoute);
  await narrow.routeWebSocket(`${origin.replace("http", "ws")}/api/sessions/*/terminal?*`, (socket) => {
    socket.send(JSON.stringify({ type: "attached", token: 1, generation: 1 }));
  });
  await narrow.goto(origin, { waitUntil: "networkidle" });
  const panels = narrow.locator("[data-live-panels]");
  await panels.waitFor({ state: "visible" });
  await eventually(() => panels.getAttribute("data-assets-new"), (value) => value === "3", "the Panels button carries the count");
  assert.equal(await panels.getAttribute("aria-label"), "Panels, 3 new assets", "the name carries the count the badge hides");
  const panelsBadge = panels.locator("[data-assets-badge]");
  assert.equal(await panelsBadge.textContent(), "3");
  assert.ok(await panelsBadge.isVisible());
  const pBox = await panels.boundingBox();
  const pbBox = await panelsBadge.boundingBox();
  assert.deepEqual(await arrivals(narrow), [], "files waiting at load are not an arrival");
  assert.ok(Math.abs(pbBox.height - 18) <= 0.5, `the small badge is 18px tall, not ${pbBox.height}`);
  assert.ok(Math.abs(pbBox.y - (pBox.y + 3)) <= 0.5 && Math.abs(pBox.x + pBox.width - 2 - (pbBox.x + pbBox.width)) <= 0.5,
    "the small badge sits 3px down and 2px in from the button's corner");
  await panels.click();
  const row = narrow.locator('[data-panels-menu] [data-panel-choice="assets"]');
  await row.waitFor();
  assert.equal(await row.getAttribute("data-assets-new"), "3");
  assert.equal(await row.locator("[data-assets-caption]").textContent(), "3 new");
  await eventuallyEqual(() => row.evaluate((el) => getComputedStyle(el).color), badgeBg, "the Assets row takes the accent");
  await row.click();
  await narrow.waitForFunction(() => document.querySelector('[data-region="assets"]')?.getAttribute("data-open") === "true");
  await eventually(() => panels.getAttribute("data-assets-new"), (value) => value === "0", "opening from the menu clears the count");
  assert.equal(await panels.getAttribute("aria-label"), "Panels");
  assert.ok(!(await panelsBadge.isVisible()), "and hides the Panels badge");
  await compact.close();

  assert.ok(assetStreams >= 3, "the closed panel's feed reconnected after each event");
  assert.deepEqual(pageErrors, [], "no page threw");
  console.log("assets-news-probe: ok");
} finally {
  await browser?.close();
  server.close();
}
