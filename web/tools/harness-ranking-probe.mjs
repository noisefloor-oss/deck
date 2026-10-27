// The new-session picker ranks by recorded launches, measured
// against the real build.
//
// The ranking is a pure function with its own unit tests. What needs a
// browser is the wiring around it: that opening the picker in a project reads
// THAT project's history, that a second project's opening re-reads rather
// than reusing the first's answer, and that a failed read leaves the served
// order rather than an empty or reshuffled picker.
import assert from "node:assert/strict";
import { createServer } from "node:http";
import { readFile } from "node:fs/promises";
import { existsSync } from "node:fs";
import { extname, join, resolve } from "node:path";
import { browser as getBrowser } from "./browser.mjs";

const WEB = resolve(import.meta.dirname, "..");
const DIST = join(WEB, "dist");
const MIME = {
  ".css": "text/css",
  ".html": "text/html",
  ".js": "text/javascript",
  ".png": "image/png",
  ".svg": "image/svg+xml",
  ".woff2": "font/woff2",
};

if (!existsSync(join(DIST, "index.html"))) {
  throw new Error("dist/index.html is missing — run `pnpm build` first");
}

const server = createServer(async (req, res) => {
  const pathname = decodeURIComponent(new URL(req.url, "http://x").pathname);
  const path = join(DIST, pathname === "/" ? "index.html" : pathname);
  if (!path.startsWith(DIST) || !existsSync(path)) {
    res.writeHead(404);
    res.end("not found");
    return;
  }
  res.writeHead(200, { "Content-Type": MIME[extname(path)] ?? "application/octet-stream" });
  res.end(await readFile(path));
});
await new Promise((listening) => server.listen(0, "127.0.0.1", listening));
const origin = `http://127.0.0.1:${server.address().port}`;

// The served order is deliberately not the ranked order for either project:
// an assertion that passed on both would be measuring the inventory.
const SERVED = ["claude", "codex", "kimi", "opencode", "shell"];
const PROJECTS = [
  { id: "proj-alpha", name: "alpha", canonical_path: "/workspace/alpha", git_identity_setup_revision: 1, last_used_at: "2026-09-01T00:00:00Z" },
  { id: "proj-beta", name: "beta", canonical_path: "/workspace/beta", git_identity_setup_revision: 1, last_used_at: "2026-09-01T00:00:00Z" },
];
// alpha runs shell, then opencode. beta runs only kimi. The installation as a
// whole is dominated by claude, which neither project has launched — so a
// picker showing claude first is showing the global totals, and one showing
// the served order is showing no history at all.
const USAGE = {
  "proj-alpha": [
    { provider: "shell", launches: 12, last_used_at: "2026-09-05T10:00:00Z" },
    { provider: "opencode", launches: 3, last_used_at: "2026-09-04T10:00:00Z" },
  ],
  "proj-beta": [
    { provider: "kimi", launches: 2, last_used_at: "2026-09-05T09:00:00Z" },
  ],
};
const GLOBAL = [
  { provider: "claude", launches: 40, last_used_at: "2026-09-06T10:00:00Z" },
  { provider: "shell", launches: 12, last_used_at: "2026-09-05T10:00:00Z" },
  { provider: "opencode", launches: 3, last_used_at: "2026-09-04T10:00:00Z" },
  { provider: "kimi", launches: 2, last_used_at: "2026-09-05T09:00:00Z" },
];

let usageRequests = [];
let failUsage = false;
// When set, the usage route waits on this before answering, so the probe can
// look at the picker while its ranking is still in flight.
let holdUsage = null;

let browser;
try {
  ({ browser } = await getBrowser());
  await run();
  console.log("harness ranking probe: all assertions passed");
} finally {
  await browser?.close();
  await new Promise((closed) => server.close(closed));
}

async function run() {
  const context = await browser.newContext({ viewport: { width: 1280, height: 900 } });
  const page = await context.newPage();
  const errors = [];
  page.on("pageerror", (error) => errors.push(error.message));

  await page.route(`${origin}/auth/session`, (route) => route.fulfill({
    status: 204,
    headers: { "X-Noisedeck-Auth-Mode": "builtin" },
  }));
  await page.route(`${origin}/api/**`, (route) => {
    const request = route.request();
    const url = new URL(request.url());
    const path = url.pathname;
    const json = (status, body) => route.fulfill({
      status, contentType: "application/json", body: JSON.stringify(body),
    });
    const stream = () => route.fulfill({
      status: 200, contentType: "text/event-stream", body: ": connected\n\nretry: 3600000\n\n",
    });
    if (request.method() === "GET" && path === "/api/session-activity/events") return stream();
    if (request.method() === "GET" && path === "/api/providers") return json(200, { providers: SERVED });
    if (request.method() === "GET" && path === "/api/harness-usage") {
      const project = url.searchParams.get("project") ?? "";
      usageRequests.push(project);
      if (failUsage) return json(500, { code: "internal", message: "could not read harness usage" });
      const answer = () => json(200, { global: GLOBAL, project: USAGE[project] ?? [] });
      return holdUsage === null ? answer() : holdUsage.then(answer);
    }
    if (request.method() === "GET" && path === "/api/settings/last-session") return json(404, { code: "not_found", message: "no last session remembered" });
    if (request.method() === "PUT" && path === "/api/settings/last-session") return json(200, request.postDataJSON());
    if (request.method() === "GET" && path === "/api/settings/layout") return json(404, { code: "not_found", message: "no layout saved" });
    if (request.method() === "PUT" && path === "/api/settings/layout") return json(200, request.postDataJSON());
    if (request.method() === "GET" && path === "/api/settings/tutorial") return json(200, { version: 1, dismissed: true, lesson: "welcome", step: 0, done: [], scope: "user" });
    if (request.method() === "GET" && path === "/api/settings") {
      return json(200, { skip_permissions: false, skip_close_confirm: false, skip_remove_project_confirm: false });
    }
    if (request.method() === "GET" && path === "/api/git-identities") {
      return json(200, {
        profiles: [{ id: "identity-1", name: "Probe identity", email: "probe@example.invalid", revision: 1, created_at: "t", updated_at: "t" }],
        default_profile_id: "identity-1",
        revision: 1,
      });
    }
    if (request.method() === "GET" && path === "/api/projects") return json(200, PROJECTS);
    if (request.method() === "GET" && /^\/api\/projects\/[^/]+\/sessions$/.test(path)) return json(200, []);
    if (/^\/api\/projects\/[^/]+\/lanes$/.test(path)) return json(200, { lanes: [] });
    if (request.method() === "GET" && /^\/api\/projects\/[^/]+\/taskmaster$/.test(path)) {
      return json(404, { code: "not_taskmaster_project", message: "not a task project" });
    }
    if (request.method() === "GET" && /^\/api\/projects\/[^/]+\/taskmaster\/events$/.test(path)) return stream();
    if (request.method() === "GET" && /^\/api\/sessions\/[^/]+\/chats\/events$/.test(path)) return stream();
    if (request.method() === "GET" && /^\/api\/sessions\/[^/]+\/chats$/.test(path)) {
      return json(200, { agent: "probe", chats: [] });
    }
    if (request.method() === "GET" && path === "/api/chats/status") return json(200, { available: true });
    if (request.method() === "GET" && path === "/api/chats") return json(200, { chats: [] });
    throw new Error(`unhandled ${request.method()} ${path}`);
  });

  await page.goto(origin, { waitUntil: "networkidle" });
  await page.locator('[data-project="proj-alpha"]').waitFor();

  // alpha: its own two harnesses first, most-launched first; then the ones
  // launched elsewhere but not here, by their global totals — claude leads
  // the installation and still comes after alpha's own; then codex, which
  // nobody has ever launched, keeping its served place at the end.
  assert.deepEqual(await pickerOrder(page, "proj-alpha"),
    ["shell", "opencode", "claude", "kimi", "codex"],
    "the picker did not rank alpha's own history first");
  assert.deepEqual(usageRequests, ["proj-alpha"],
    `the picker read ${JSON.stringify(usageRequests)} rather than alpha's history`);

  // beta: a different project, a different answer. This is what a cached
  // first read would get wrong — beta has launched only kimi.
  assert.deepEqual(await pickerOrder(page, "proj-beta"),
    ["kimi", "claude", "shell", "opencode", "codex"],
    "the picker did not re-read the history for the second project");
  assert.deepEqual(usageRequests, ["proj-alpha", "proj-beta"],
    `the second opening read ${JSON.stringify(usageRequests)}`);

  // While a project's own read is in flight, the picker must not be showing
  // the ranking of the project before it. The dialog is up and clickable in
  // that window, so a user can launch out of it.
  {
    let release;
    holdUsage = new Promise((resolve) => { release = resolve; });
    await openPicker(page, "proj-alpha");
    assert.deepEqual(await chipOrder(page), SERVED,
      "a pending read left the previous project's ranking on screen");
    release();
    holdUsage = null;
    await settledOrder(page);
    await closePicker(page);
  }

  // And it must not reorder under a user who has started reading it: an
  // armed chip is an index into the list on screen, so a list that reorders
  // arms a different harness than the one the user walked to.
  {
    let release;
    holdUsage = new Promise((resolve) => { release = resolve; });
    await openPicker(page, "proj-alpha");
    await page.focus("[data-new-session-filter]");
    await page.keyboard.press("ArrowDown");
    const armed = await page.evaluate(() => document.querySelector(
      "[data-new-session-provider][data-selected=\"true\"]")?.dataset.newSessionProvider ?? null);
    assert.equal(armed, SERVED[0], `the first arrow armed ${armed}`);
    release();
    holdUsage = null;
    await page.waitForTimeout(300);
    const after = await page.evaluate(() => ({
      armed: document.querySelector("[data-new-session-provider][data-selected=\"true\"]")?.dataset.newSessionProvider ?? null,
      focused: document.activeElement?.dataset?.newSessionProvider
        ?? (document.activeElement?.hasAttribute?.("data-new-session-filter") === true ? "filter" : null),
      order: [...document.querySelectorAll("[data-new-session-chips] [data-new-session-provider]")]
        .map((el) => el.dataset.newSessionProvider),
    }));
    assert.equal(after.armed, armed, "the ranking re-armed a different harness under the user");
    assert.equal(after.focused, armed, "the ranking took focus off the armed chip");
    assert.deepEqual(after.order, SERVED, "the list reordered under an armed chip");
    await closePicker(page);
  }

  // The pointer is an interaction too. A chip under the cursor is a target
  // the user is already reaching for, and the ranking must not move it.
  {
    let release;
    holdUsage = new Promise((resolve) => { release = resolve; });
    await openPicker(page, "proj-alpha");
    await page.locator(`[data-new-session-provider="${SERVED[0]}"]`).hover();
    release();
    holdUsage = null;
    await page.waitForTimeout(300);
    assert.deepEqual(await chipOrder(page), SERVED,
      "the list reordered under the pointer");
    await closePicker(page);
  }

  // ...and so is a filter that has been typed and cleared: the picker was
  // being read either way, and reading the CURRENT filter would call that
  // untouched.
  {
    let release;
    holdUsage = new Promise((resolve) => { release = resolve; });
    await openPicker(page, "proj-alpha");
    await page.focus("[data-new-session-filter]");
    await page.keyboard.type("co");
    await page.keyboard.press("Backspace");
    await page.keyboard.press("Backspace");
    release();
    holdUsage = null;
    await page.waitForTimeout(300);
    assert.deepEqual(await chipOrder(page), SERVED,
      "the list reordered after a filter was typed and cleared");
    await closePicker(page);
  }

  // A key that neither types nor moves the selection is still someone at the
  // keyboard on this dialog: End in the filter produces no input event and
  // leaves focus where it is.
  {
    let release;
    holdUsage = new Promise((resolve) => { release = resolve; });
    await openPicker(page, "proj-alpha");
    await page.focus("[data-new-session-filter]");
    await page.keyboard.press("End");
    release();
    holdUsage = null;
    await page.waitForTimeout(300);
    assert.deepEqual(await chipOrder(page), SERVED,
      "the list reordered after a key that neither typed nor moved the selection");
    await closePicker(page);
  }

  // Focus can reach a chip without a key or a pointer, which is how an
  // assistive navigation moves through the list. A chip with focus is a
  // chip being read.
  {
    let release;
    holdUsage = new Promise((resolve) => { release = resolve; });
    await openPicker(page, "proj-alpha");
    await page.locator(`[data-new-session-provider="${SERVED[0]}"]`).focus();
    release();
    holdUsage = null;
    await page.waitForTimeout(300);
    const after = await page.evaluate(() => ({
      armed: document.querySelector("[data-new-session-provider][data-selected=\"true\"]")?.dataset.newSessionProvider ?? null,
      focused: document.activeElement?.dataset?.newSessionProvider ?? null,
      order: [...document.querySelectorAll("[data-new-session-chips] [data-new-session-provider]")]
        .map((el) => el.dataset.newSessionProvider),
    }));
    assert.equal(after.armed, SERVED[0], "the ranking re-armed a different harness under a focused chip");
    assert.equal(after.focused, SERVED[0], "the ranking took focus off the chip");
    assert.deepEqual(after.order, SERVED, "the list reordered under a focused chip");
    await closePicker(page);
  }

  // A filter can arrive without a keystroke — a paste, or a browser filling
  // the field — and that is still a user reading this list.
  {
    let release;
    holdUsage = new Promise((resolve) => { release = resolve; });
    await openPicker(page, "proj-alpha");
    // A value set without any key event at all — what a paste or an
    // autofill looks like to the page.
    const setFilter = (value) => page.evaluate((next) => {
      const field = document.querySelector("[data-new-session-filter]");
      field.value = next;
      field.dispatchEvent(new Event("input", { bubbles: true }));
    }, value);
    await setFilter("co");
    await setFilter("");
    release();
    holdUsage = null;
    await page.waitForTimeout(300);
    assert.deepEqual(await chipOrder(page), SERVED,
      "the list reordered after a filter arrived without a keystroke");
    await closePicker(page);
  }

  // A read belongs to the opening that issued it. Alpha's picker is closed
  // with its read still pending and beta's is opened; the answer that lands
  // first must not pin the dialog that replaced it.
  {
    let release;
    holdUsage = new Promise((resolve) => { release = resolve; });
    await openPicker(page, "proj-alpha");
    await closePicker(page);
    await openPicker(page, "proj-beta");
    release();
    holdUsage = null;
    const order = await settledOrder(page);
    assert.deepEqual(order, ["kimi", "claude", "shell", "opencode", "codex"],
      "a previous opening's read pinned the dialog that replaced it");
    await closePicker(page);
  }

  // A failed read is not an empty picker and not a reshuffled one: the
  // ranking is a preference over a list the user already has.
  failUsage = true;
  assert.deepEqual(await pickerOrder(page, "proj-alpha"), SERVED,
    "a failed usage read did not fall back to the served order");

  assert.deepEqual(errors, [], `page errors: ${errors.join("; ")}`);
  await context.close();
}

// Opens the project's New session dialog and reads the chip order once it
// has settled, then closes it.
async function pickerOrder(page, projectID) {
  await openPicker(page, projectID);
  const order = await settledOrder(page);
  await closePicker(page);
  return order;
}

async function openPicker(page, projectID) {
  const before = usageRequests.length;
  // A project row carries two of these: the header's + (revealed on hover)
  // and the expanded list's "New session" row. Both open the same dialog for
  // the same project; the row is the one that does not need a hover.
  const row = page.locator(`[data-project="${projectID}"]`);
  if (await row.locator("[data-project-toggle][aria-expanded=\"false\"]").count() > 0) {
    await row.locator("[data-project-toggle]").click();
  }
  await row.locator("[data-new-session]").last().click();
  await page.locator("[data-new-session-modal]").waitFor();
  // The read is issued by the opening; every assertion here is about what
  // the picker shows before or after its answer, so wait for the request to
  // have left rather than racing it.
  const deadline = Date.now() + 5000;
  while (usageRequests.length === before && Date.now() < deadline) {
    await page.waitForTimeout(25);
  }
}

function chipOrder(page) {
  return page.evaluate(() =>
    [...document.querySelectorAll("[data-new-session-chips] [data-new-session-provider]")]
      .map((el) => el.dataset.newSessionProvider));
}

async function closePicker(page) {
  await page.keyboard.press("Escape");
  await page.locator("[data-new-session-modal]").waitFor({ state: "detached" });
}

// The ranking arrives on a re-render, so the order is read once it stops
// changing rather than on the first look.
async function settledOrder(page) {
  const deadline = Date.now() + 5000;
  let order = [];
  let stable = 0;
  while (Date.now() < deadline && stable < 3) {
    const next = await chipOrder(page);
    stable = JSON.stringify(next) === JSON.stringify(order) ? stable + 1 : 0;
    order = next;
    await page.waitForTimeout(25);
  }
  return order;
}
