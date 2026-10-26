// The first screen's typeface, measured in a real browser against the real
// build.
//
// "Checking authentication…" is painted the moment main.ts runs, before any
// webface has arrived. Deck ships its own faces in both font sets, so that
// text must wait for them rather than paint in whatever the fallback stack
// resolves to on the reader's machine. The probe holds the auth check (so the
// message stays up) and every font file, reads the pixels where the message
// sits, then releases the fonts and reads them again.
import assert from "node:assert/strict";
import { createServer } from "node:http";
import { readFile } from "node:fs/promises";
import { existsSync } from "node:fs";
import { extname, join, resolve } from "node:path";
import { PNG } from "pngjs";
import { browser as getBrowser } from "./browser.mjs";

const DIST = resolve(import.meta.dirname, "..", "dist");
if (!existsSync(join(DIST, "index.html"))) {
  throw new Error("dist/index.html is missing — run `pnpm build` first");
}
const MIME = {
  ".css": "text/css", ".html": "text/html", ".js": "text/javascript",
  ".png": "image/png", ".svg": "image/svg+xml", ".woff2": "font/woff2",
};

const server = createServer(async (req, res) => {
  const pathname = decodeURIComponent(new URL(req.url, "http://x").pathname);
  const file = join(DIST, pathname === "/" ? "index.html" : pathname);
  if (!file.startsWith(DIST) || !existsSync(file)) {
    res.writeHead(404);
    res.end("not found");
    return;
  }
  res.writeHead(200, { "Content-Type": MIME[extname(file)] ?? "application/octet-stream" });
  res.end(await readFile(file));
});
await new Promise((listening) => server.listen(0, "127.0.0.1", listening));
const origin = `http://127.0.0.1:${server.address().port}`;

// Pixels inside the box that differ from its top-left corner, which is the
// surface behind the text.
function inkPixels(png) {
  const { data, width, height } = PNG.sync.read(png);
  const [r, g, b] = data;
  let ink = 0;
  for (let i = 0; i < width * height * 4; i += 4) {
    if (Math.abs(data[i] - r) + Math.abs(data[i + 1] - g) + Math.abs(data[i + 2] - b) > 48) ink++;
  }
  return ink;
}

let browser;
try {
  ({ browser } = await getBrowser());
  const page = await browser.newPage({ viewport: { width: 1024, height: 640 } });
  const heldFonts = [];
  const requestedFonts = [];
  let releaseFonts = false;
  await page.route(`${origin}/auth/session`, () => {});
  await page.route(`${origin}/**/*.woff2`, (route) => {
    requestedFonts.push(new URL(route.request().url()).pathname);
    if (releaseFonts) return route.continue();
    heldFonts.push(route);
  });
  await page.goto(origin, { waitUntil: "commit" });
  const message = page.getByText("Checking authentication…", { exact: true });
  await message.waitFor();
  const deadline = Date.now() + 5000;
  while (heldFonts.length === 0) {
    assert.ok(Date.now() < deadline, "the boot screen requested no webface");
    await page.waitForTimeout(25);
  }
  // Within the block period: long enough for a fallback to have painted,
  // well short of the point where a browser gives up waiting.
  await page.waitForTimeout(500);
  const box = await message.boundingBox();
  assert.ok(box !== null && box.width > 0, "the boot message has no box");
  // Playwright's own screenshot waits for pending fonts, which would measure
  // the frame after the wait; the protocol call captures the frame as drawn.
  const cdp = await page.context().newCDPSession(page);
  const shot = async () => Buffer.from((await cdp.send("Page.captureScreenshot", {
    format: "png", clip: { x: box.x, y: box.y, width: box.width, height: box.height, scale: 1 },
  })).data, "base64");
  assert.equal(inkPixels(await shot()), 0,
    `the boot message painted before its face arrived (held: ${requestedFonts.join(", ")})`);

  releaseFonts = true;
  for (const route of heldFonts.splice(0)) await route.continue();
  await page.evaluate(() => document.fonts.ready);
  await page.waitForTimeout(100);
  assert.ok(inkPixels(await shot()) > 0, "the boot message never painted once its face arrived");
  const face = await message.evaluate((el) => getComputedStyle(el).fontFamily.split(",")[0].trim());
  assert.ok(await page.evaluate((family) => document.fonts.check(`16px ${family}`), face),
    `the boot message's face ${face} did not load`);
  console.log(`boot font probe: all assertions passed (${face})`);
} finally {
  await browser?.close();
  await new Promise((closed) => server.close(closed));
}
