// The keyboard-dismissal settle: the frame height self-heals.
//
// iOS animates the software keyboard away and does not reliably fire a
// final visualViewport resize at the settled geometry: the last event can
// arrive mid-animation, or — when the focused field blurs — not at all,
// leaving the workspace frame pinned at keyboard height with a dead band
// where the keyboard was. The frame must re-read the viewport on a short
// cascade after every signal and treat focusout as a signal. The unit
// suite pins frameStyle's arithmetic; only a browser proves the listeners
// and timers actually heal a stale reading, so this drives the live app
// against a scripted visualViewport.
import assert from "node:assert/strict";
import { createServer } from "node:http";
import { readFile } from "node:fs/promises";
import { existsSync } from "node:fs";
import { extname, join, resolve } from "node:path";
import { browser as getBrowser } from "./browser.mjs";
import { eventuallyEqual } from "./eventually.mjs";

const dist = resolve(import.meta.dirname, "..", "dist");
if (!existsSync(join(dist, "index.html"))) {
  throw new Error("dist/index.html is missing — run pnpm build first");
}

const mime = { ".css": "text/css", ".html": "text/html", ".js": "text/javascript", ".svg": "image/svg+xml", ".png": "image/png", ".woff2": "font/woff2" };
const server = createServer(async (req, res) => {
  const name = new URL(req.url, "http://x").pathname.slice(1);
  const path = join(dist, name === "" ? "index.html" : name);
  if (!path.startsWith(dist) || !existsSync(path)) {
    res.writeHead(404);
    res.end("not found");
    return;
  }
  res.writeHead(200, { "Content-Type": mime[extname(path)] ?? "application/octet-stream" });
  res.end(await readFile(path));
});
await new Promise((resolveServer) => server.listen(0, "127.0.0.1", resolveServer));
const origin = `http://127.0.0.1:${server.address().port}`;

let browser;
try {
  ({ browser } = await getBrowser());
  const context = await browser.newContext({
    viewport: { width: 390, height: 720 },
    hasTouch: true,
    isMobile: true,
  });
  const page = await context.newPage();
  const errors = [];
  page.on("pageerror", (error) => errors.push(error.message));

  // The tracker captures window.visualViewport at bind time, so the fake
  // must exist before any app script runs. It is a plain EventTarget with
  // mutable metrics: the probe moves the numbers and chooses which events
  // fire, which is exactly the gap between iOS's animation and its events.
  await page.addInitScript(() => {
    const fake = new EventTarget();
    fake.height = 720;
    fake.offsetTop = 0;
    Object.defineProperty(window, "visualViewport", { value: fake, configurable: true });
    window.__probeViewport = fake;
  });

  const project = {
    id: "project-kb",
    name: "kb-project",
    canonical_path: "/workspace/kb-project",
    last_used_at: "2026-08-08T00:00:00Z",
  };
  await page.route(`${origin}/auth/session`, (route) => route.fulfill({
    status: 204,
    headers: { "X-Noisedeck-Auth-Mode": "builtin" },
  }));
  await page.route(`${origin}/api/**`, (route) => {
    const request = route.request();
    const path = new URL(request.url()).pathname;
    const json = (status, body) => route.fulfill({
      status,
      contentType: "application/json",
      body: JSON.stringify(body),
    });
    if (request.method() === "GET" && path === "/api/session-activity/events") {
      return route.fulfill({
        status: 200,
        contentType: "text/event-stream",
        body: ": connected\n\nretry: 3600000\n\n",
      });
    }
    if (request.method() === "GET" && path === "/api/providers") {
      // The probe has no open sessions and never opens the picker; a single
      // provider is enough to satisfy the startup read.
      return json(200, { providers: ["claude"] });
    }
    if (request.method() === "GET" && path === "/api/settings/last-session") return json(404, { code: "not_found", message: "no last session remembered" });
    if (request.method() === "PUT" && path === "/api/settings/last-session") return json(200, request.postDataJSON());
    if (request.method() === "GET" && path === "/api/settings/layout") return json(404, { code: "not_found", message: "no layout saved" });
    if (request.method() === "GET" && path === "/api/settings/tutorial") return json(200, { version: 1, dismissed: true, lesson: "welcome", step: 0, done: [], scope: "user" });
    if (request.method() === "GET" && path === "/api/settings") {
      return json(200, {
        skip_permissions: false,
        skip_close_confirm: false,
        skip_remove_project_confirm: false,
      });
    }
    if (request.method() === "GET" && path === "/api/projects") return json(200, [project]);
    if (request.method() === "GET" && path === "/api/git-identities") return json(200, {
      profiles: [{ id: "identity-1", name: "Deck probe", email: "deck@example.invalid", revision: 1, created_at: "t", updated_at: "t" }],
      default_profile_id: "identity-1", revision: 1,
    });
    if (request.method() === "GET" && path === `/api/projects/${project.id}/sessions`) return json(200, []);
    if (/^\/api\/projects\/[^/]+\/lanes$/.test(path)) return json(200, { lanes: [] });
    if (request.method() === "GET" && path === `/api/projects/${project.id}/taskmaster`) {
      return json(404, { error: "not_taskmaster_project", message: "not a task project" });
    }
    if (request.method() === "GET" && path === `/api/projects/${project.id}/taskmaster/events`) {
      return route.fulfill({
        status: 200,
        contentType: "text/event-stream",
        body: ": connected\n\nretry: 3600000\n\n",
      });
    }
    // The live shell reads the chat ledger for every selected session.
    // A probe that is not about chats answers with an empty
    // ledger, an empty index and a quiet change stream.
    if (request.method() === "GET" && /^\/api\/sessions\/[^/]+\/chats\/events$/.test(path)) {
      return route.fulfill({ status: 200, contentType: "text/event-stream", body: ": connected\n\nretry: 3600000\n\n" });
    }
    if (request.method() === "GET" && /^\/api\/sessions\/[^/]+\/chats$/.test(path)) {
      return route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ agent: "claude-sess", chats: [] }) });
    }
    if (request.method() === "GET" && path === "/api/chats/status") return json(200, { available: true });
    if (request.method() === "GET" && path === "/api/chats") {
      return route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ chats: [] }) });
    }
    throw new Error(`unhandled ${request.method()} ${path}`);
  });

  await page.goto(origin, { waitUntil: "networkidle" });
  await page.waitForSelector("[data-workspace]");

  const frameHeight = () => page.evaluate(() => document.querySelector("[data-workspace]").style.height);
  const setViewport = (height, event) => page.evaluate(([h, e]) => {
    const vv = window.__probeViewport;
    vv.height = h;
    if (e !== null) (e === "focusout" ? window : vv).dispatchEvent(new Event(e));
  }, [height, event]);

  // The bound tracker applied the full viewport on boot.
  await eventuallyEqual(frameHeight, "720px",
    "the tracker never applied the boot geometry");

  // The keyboard opens: the resize fires with the short height and the
  // frame pins to the visible region.
  await setViewport(400, "resize");
  await eventuallyEqual(frameHeight, "400px",
    "an open keyboard never shrank the frame");

  // Mid-animation dismissal: the geometry recovers but NO further event
  // fires. The settle cascade from the last resize must re-read it.
  await setViewport(720, null);
  await eventuallyEqual(frameHeight, "720px",
    "a silent dismissal after a stale resize left the frame pinned short");

  // Blur-only dismissal: the keyboard leaves long after the cascade is
  // exhausted, and the only signal is the field losing focus.
  await setViewport(400, "resize");
  await eventuallyEqual(frameHeight, "400px",
    "the keyboard never re-opened for the blur leg");
  await new Promise((wake) => setTimeout(wake, 900));
  assert.equal(await frameHeight(), "400px",
    "the cascade must re-read the truth, not invent a taller one");
  await setViewport(720, "focusout");
  await eventuallyEqual(frameHeight, "720px",
    "a blur-only dismissal left the frame pinned short");

  assert.deepEqual(errors, [], `page errors: ${errors.join("; ")}`);
  console.log("kb-settle probe: boot, pin, silent-dismiss heal, blur heal — all good");
} finally {
  await browser?.close();
  server.close();
}
