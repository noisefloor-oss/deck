// The titlebar chip wears the selected session's resolved colour, so it must
// move with the sidebar tiles through every control that changes it: the
// More menu's Auto-color projects toggle in both directions, a project tag
// picked from the project row's own swatch strip, and a selection change to
// a different provider's session. This drives the real preview build in a
// real browser and compares the chip's computed background against the
// selected session's tile. A change-of-colour assertion on each leg keeps
// the agreement checks from passing vacuously through a no-op control.
import assert from "node:assert/strict";
import { createServer } from "node:http";
import { readFile } from "node:fs/promises";
import { existsSync } from "node:fs";
import { extname, join, resolve } from "node:path";
import { browser as getBrowser } from "./browser.mjs";

const dist = resolve(import.meta.dirname, "..", "dist");
if (!existsSync(join(dist, "preview.html"))) {
  throw new Error("dist/preview.html is missing — run pnpm build first (embed builds drop the preview)");
}

const mime = { ".css": "text/css", ".html": "text/html", ".js": "text/javascript", ".svg": "image/svg+xml", ".png": "image/png", ".woff2": "font/woff2" };
const server = createServer(async (req, res) => {
  const name = new URL(req.url, "http://x").pathname.slice(1);
  const path = join(dist, name);
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
  const context = await browser.newContext({ viewport: { width: 1400, height: 900 } });
  const page = await context.newPage();
  const pageErrors = [];
  page.on("pageerror", (error) => pageErrors.push(error.message));
  await page.goto(`${origin}/preview.html`, { waitUntil: "networkidle" });
  await page.waitForSelector("html[data-preview-state]");
  assert.equal(await page.locator("html[data-preview-error]").count(), 0,
    "the preview reached its capture state but also carries an error marker");

  // The tile locator follows the sidebar's selected row rather than a fixed
  // session id, so every leg compares the chip against whichever session
  // the sidebar actually presents. The row's first span is the phone's
  // leading status column now; the tile is the rounded-mark span.
  const chipAndTile = () => page.evaluate(() => {
    const chip = document.querySelector('[data-region="terminal-titlebar"] [data-terminal-provider]');
    const row = document.querySelector('[data-session][aria-selected="true"]');
    const tile = row?.querySelector('span[class*="rounded-mark"]') ?? null;
    if (chip === null || tile === null) throw new Error("chip or selected tile not found");
    return {
      id: row.getAttribute("data-session"),
      chip: getComputedStyle(chip).backgroundColor,
      tile: getComputedStyle(tile).backgroundColor,
    };
  });

  const autoColor = () =>
    page.locator("[data-more-popover]").getByRole("switch", { name: "Auto-color projects" });

  const providerHue = await chipAndTile();
  assert.notEqual(providerHue.chip, "rgba(0, 0, 0, 0)",
    "the chip rendered transparent — the preview's styles did not load");
  assert.equal(providerHue.chip, providerHue.tile,
    "at rest, before any toggle, the chip and the selected session's tile already disagree");

  await page.locator("[data-titlebar-more]").click();
  await autoColor().click();
  const tinted = await chipAndTile();
  assert.notEqual(tinted.tile, providerHue.tile,
    "the toggle left the tile on its provider hue — the agreement checks would pass vacuously");
  assert.equal(tinted.chip, tinted.tile,
    "after turning Auto-color projects on, the chip kept a colour the tiles no longer wear");

  await autoColor().click();
  const restored = await chipAndTile();
  assert.equal(restored.chip, restored.tile,
    "after turning Auto-color projects off, the chip kept the tint the tiles dropped");
  assert.equal(restored.tile, providerHue.tile,
    "turning the toggle off did not restore the tile's provider hue");

  await autoColor().click();
  const tintedAgain = await chipAndTile();
  assert.notEqual(tintedAgain.tile, restored.tile,
    "re-enabling auto-color left the tile on its provider hue — the tag leg would pass vacuously");
  assert.equal(tintedAgain.chip, tintedAgain.tile,
    "after re-enabling Auto-color projects, the chip kept a colour the tiles no longer wear");

  // The project swatch path: a tag on the selected session's project clears
  // the derived tint under it, so the tile returns to its provider hue and
  // the chip must make the same move.
  await page.mouse.click(700, 450);
  await page.locator('[data-project-toggle="proj-1"]').hover();
  await page.locator('[data-project-menu="proj-1"]').click();
  await page.locator('[data-proj-swatch="crimson"]').click();
  const tagged = await chipAndTile();
  assert.equal(tagged.chip, tagged.tile,
    "after tagging the selected session's project, the chip kept the tint the tile dropped");
  assert.equal(tagged.tile, providerHue.tile,
    "a project tag replaces the auto tint, so the selected tile returns to its provider hue");

  // The selection path: clicking another provider's session row must carry
  // the chip to that session's colour. Codex is a different provider than
  // the claude session the fixture boots on, so a chip that stayed put is a
  // real divergence, not a hue that happens to match.
  await page.locator('[data-session="sess-2"]').click();
  const codex = await chipAndTile();
  assert.equal(codex.id, "sess-2",
    "clicking the codex row left the sidebar presenting a different session — the later agreement would not be about the click");
  assert.notEqual(codex.chip, tagged.chip,
    "selecting the codex session left the chip on the claude hue — the agreement check would pass vacuously");
  assert.equal(codex.chip, codex.tile,
    "after selecting the codex session, the chip kept the colour of the session the sidebar no longer presents");
  console.log("titlebar chip keeps the selected session's colour across the auto-color toggles, a project tag, and a selection change: PASS");
  await context.close();
} finally {
  await browser?.close();
  server.close();
}
