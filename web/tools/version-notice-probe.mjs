import assert from "node:assert/strict";
import { createServer } from "node:http";
import { readFile } from "node:fs/promises";
import { existsSync } from "node:fs";
import { extname, join, resolve } from "node:path";
import { browser as getBrowser } from "./browser.mjs";

const dist = resolve(import.meta.dirname, "..", "dist");
if (!existsSync(join(dist, "index.html"))) throw new Error("dist/index.html is missing — run pnpm build first");

let runningBuild = "v0.3.0";
let buildChecks = 0;
let publicVersion = "v0.4.0";
const mime = { ".css": "text/css", ".html": "text/html", ".js": "text/javascript", ".svg": "image/svg+xml", ".png": "image/png", ".woff2": "font/woff2" };
const server = createServer(async (req, res) => {
  const url = new URL(req.url, "http://x");
  if (url.pathname === "/auth/session") {
    res.writeHead(204, { "X-Noisedeck-Auth-Mode": "builtin" });
    res.end();
    return;
  }
  if (url.pathname === "/api/release-update") {
    res.writeHead(200, { "Content-Type": "application/json" });
    res.end(JSON.stringify({ version: publicVersion, release_url: `https://github.com/noisefloor-oss/deck/releases/tag/${publicVersion}` }));
    return;
  }
  if (url.pathname === "/api/build") {
    buildChecks += 1;
    res.writeHead(200, { "Content-Type": "application/json", "Cache-Control": "no-store" });
    res.end(JSON.stringify({ build_id: runningBuild }));
    return;
  }
  if (url.pathname === "/api/settings/tutorial") {
    res.writeHead(200, { "Content-Type": "application/json" });
    res.end(JSON.stringify({ version: 1, dismissed: true, lesson: "welcome", step: 0, done: [], scope: "user" }));
    return;
  }
  if (url.pathname === "/api/settings") {
    res.writeHead(200, { "Content-Type": "application/json" });
    res.end(JSON.stringify({ skip_permissions: false, skip_close_confirm: false, skip_remove_project_confirm: false }));
    return;
  }
  if (url.pathname === "/api/projects") {
    res.writeHead(200, { "Content-Type": "application/json" });
    res.end("[]");
    return;
  }
  const name = url.pathname === "/" ? "index.html" : url.pathname.slice(1);
  const path = join(dist, name);
  if (!path.startsWith(dist) || !existsSync(path)) {
    res.writeHead(404);
    res.end("not found");
    return;
  }
  let body = await readFile(path);
  if (name === "index.html") {
    body = Buffer.from(body.toString().replaceAll("__NOISEDECK_BUILD_ID__", runningBuild));
  }
  res.writeHead(200, { "Content-Type": mime[extname(path)] ?? "application/octet-stream" });
  res.end(body);
});

await new Promise((resolveServer) => server.listen(0, "127.0.0.1", resolveServer));
const origin = `http://127.0.0.1:${server.address().port}`;
let browser;
try {
  ({ browser } = await getBrowser());
  const context = await browser.newContext({ viewport: { width: 1024, height: 768 } });
  const oldTab = await context.newPage();
  await oldTab.goto(origin, { waitUntil: "networkidle" });
  await oldTab.locator("[data-workspace]").waitFor();
  assert.equal(await oldTab.locator('meta[name="noisedeck-build-id"]').getAttribute("content"), "v0.3.0");
  assert.equal(await oldTab.locator("[data-version-reload]").isHidden(), true);
  assert.ok(buildChecks >= 1, "old tab never checked the running build");

  const notice = oldTab.locator("[data-release-notice]");
  await notice.waitFor({ state: "visible" });
  assert.equal(await oldTab.locator('[data-region="info-bar"] [data-release-notice]').count(), 1, "notice shares the existing info bar");
  assert.equal((await oldTab.locator('[data-region="info-bar"]').boundingBox()).height, 26, "desktop keeps its existing strip height");
  assert.equal(await notice.locator("a").getAttribute("href"), "https://github.com/noisefloor-oss/deck/releases/tag/v0.4.0");
  await notice.getByRole("button", { name: "Dismiss update notice" }).click();
  assert.equal(await notice.isHidden(), true);
  await oldTab.reload({ waitUntil: "networkidle" });
  assert.equal(await oldTab.locator("[data-release-notice]").isHidden(), true, "dismissed release returns after reload");
  publicVersion = "v0.5.0";
  await oldTab.setViewportSize({ width: 390, height: 844 });
  await oldTab.reload({ waitUntil: "networkidle" });
  await oldTab.locator("[data-release-notice]").waitFor({ state: "visible" });
  assert.ok(await oldTab.getByRole("link", { name: "Deck 0.5.0 available. View release notes", exact: true }).isVisible());
  assert.equal((await oldTab.locator('[data-region="info-bar"]').boundingBox()).height, 40, "phone makes room for the dismiss control");
  await oldTab.getByRole("button", { name: "Dismiss update notice" }).click();
  assert.equal(await oldTab.locator('[data-region="info-bar"]').isHidden(), true, "phone strip closes when the notice is dismissed");
  await oldTab.setViewportSize({ width: 1024, height: 768 });
  runningBuild = "v0.3.1";
  const currentTab = await context.newPage();
  await currentTab.goto(origin, { waitUntil: "networkidle" });
  await currentTab.locator("[data-workspace]").waitFor();
  assert.equal(await currentTab.locator('meta[name="noisedeck-build-id"]').getAttribute("content"), "v0.3.1");
  assert.equal(await currentTab.locator("[data-version-reload]").isHidden(), true,
    "a tab loaded from the current deployment showed a stale notice");

  await oldTab.locator("[data-version-reload]").waitFor({ state: "visible", timeout: 15_000 });
  assert.equal(await oldTab.locator('[data-region="terminal"]').count(), 1,
    "the non-blocking notice replaced the workspace");
  await Promise.all([
    oldTab.waitForNavigation(),
    oldTab.locator("[data-version-reload]").click(),
  ]);
  await oldTab.locator("[data-workspace]").waitFor();
  assert.equal(await oldTab.locator('meta[name="noisedeck-build-id"]').getAttribute("content"), "v0.3.1");
  assert.equal(await oldTab.locator("[data-version-reload]").isHidden(), true,
    "explicit reload looped the version notice");
  await oldTab.evaluate(() => localStorage.setItem("noisedeck-show-info-bar", "off"));
  publicVersion = "v0.6.0";
  await oldTab.reload({ waitUntil: "networkidle" });
  assert.equal(await oldTab.locator('[data-region="info-bar"]').isHidden(), true, "info-bar preference is respected");
  await context.close();
  console.log("version notice: old/current tabs and explicit reload PASS");
} finally {
  await browser?.close();
  server.close();
}
