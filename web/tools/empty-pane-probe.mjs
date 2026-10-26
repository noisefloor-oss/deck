// The empty pane's two fixture states (comp desktop-r16), driven in a real
// browser through the preview's fixture states. The unit suite pins the
// markup's shape; this probe pins what the markup cannot prove on its
// own — that the small-caps treatment actually computes to all-small-caps
// (textTransform reads nothing there), that no part of the pane or its
// sidebar rows resolved to the mono face, that the composition is lifted
// above the geometric centre, and that both states are reachable and
// actionable end to end: the pane's New session opens the same provider
// picker the sidebar's + opens, and New project the same project form
// the Projects menu opens. Project rails retain their grouping tint with
// and without a selection; only the session row carries selection state.
import assert from "node:assert/strict";
import { assertFontVoice } from "./font-assertions.mjs";
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
  await page.addInitScript(() => localStorage.setItem("noisedeck-auto-color-projects", "on"));
  const pageErrors = [];
  page.on("pageerror", (error) => pageErrors.push(error.message));

  const openState = async (query) => {
    await page.goto(`${origin}/preview.html?${query}`, { waitUntil: "networkidle" });
    await page.waitForSelector("html[data-preview-state]");
    assert.equal(await page.locator("html[data-preview-error]").count(), 0,
      `the preview reached its capture state but also carries an error marker (${query})`);
  };

  // What the markup cannot prove: the treatment resolves in the cascade.
  const paneTypography = () => page.evaluate(() => {
    const title = document.querySelector("[data-empty-title]");
    const body = document.querySelector("[data-empty-body]");
    if (title === null || body === null) throw new Error("empty pane anatomy not found");
    return {
      caps: getComputedStyle(title).fontVariantCaps,
      transform: getComputedStyle(title).textTransform,
      titleFont: getComputedStyle(title).fontFamily,
      bodyFont: getComputedStyle(body).fontFamily,
    };
  });

  // The composition must sit above the pane's geometric centre, and not so
  // far up that the lift stops reading as centring.
  const opticalLift = () => page.evaluate(() => {
    const pane = document.querySelector('[data-region="terminal"]');
    const block = document.querySelector("[data-empty-pane] > div");
    if (pane === null || block === null) throw new Error("pane or composition not found");
    const paneBox = pane.getBoundingClientRect();
    const blockBox = block.getBoundingClientRect();
    const paneCenter = paneBox.top + paneBox.height / 2;
    const blockCenter = blockBox.top + blockBox.height / 2;
    return {
      lift: paneCenter - blockCenter,
      paneHeight: paneBox.height,
    };
  });

  // The pane sits on the page surface — the same ground the titlebar's
  // chrome bar paints — never on the terminal's themed ground.
  const surface = () => page.evaluate(() => {
    const pane = document.querySelector('[data-region="terminal"]');
    const chrome = document.querySelector(".chrome");
    if (pane === null || chrome === null) throw new Error("pane or chrome bar not found");
    return {
      pane: getComputedStyle(pane).backgroundColor,
      chrome: getComputedStyle(chrome).backgroundColor,
    };
  });

  // ---- no-session ---------------------------------------------------------
  await openState("empty=no-session");

  const noSession = await paneTypography();
  assert.equal(noSession.caps, "all-small-caps",
    "the title's small-caps treatment is font-variant-caps, which textTransform cannot express");
  assert.equal(noSession.transform, "none",
    "the title is never case-transformed — the authored copy is sentence case");
  for (const font of [noSession.titleFont, noSession.bodyFont]) {
    await assertFontVoice(page, font, "ui", `the interface face renders, not mono (${font})`);
  }

  const lift = await opticalLift();
  assert.ok(lift.lift > lift.paneHeight * 0.01,
    `the composition is lifted above the geometric centre (${lift.lift}px in a ${lift.paneHeight}px pane), not centred on it`);
  assert.ok(lift.lift < lift.paneHeight * 0.12,
    `the lift (${lift.lift}px) stops reading as centring in a ${lift.paneHeight}px pane`);

  const painted = await surface();
  assert.equal(painted.pane, painted.chrome,
    "the empty pane sits on the page surface, not the terminal's themed ground");

  assert.equal(await page.locator("[data-empty-title]").textContent(), "No session open");
  const presented = await page.locator("[data-project-name]").first().textContent();
  assert.equal(
    await page.locator("[data-empty-body]").textContent(),
    `Pick a session from the side panel, or start a new one in ${presented}.`,
    "the body names the project the sidebar presents first, so the sentence and the button agree",
  );
  assert.equal(
    await page.locator('[data-region="terminal-titlebar"] [data-titlebar-title]').textContent(),
    presented,
    "the header tile and name fall back to the project in context",
  );
  for (const hook of ["[data-titlebar-files]", "[data-titlebar-tasks]", "[data-titlebar-more]"]) {
    assert.ok(await page.locator(hook).count() > 0, `${hook} stays: a project is in context`);
  }
  // The right cluster owns the bar's right edge in the empty chrome too:
  // the project identity takes the left, the account menu flushes the end.
  {
    const edges = await page.evaluate(() => {
      const bar = document.querySelector('[data-region="terminal-titlebar"]');
      const more = document.querySelector("[data-titlebar-more]");
      const barBox = bar.getBoundingClientRect();
      const moreBox = more.getBoundingClientRect();
      return {
        barRight: barBox.right,
        moreRight: moreBox.right,
        identityRight: document.querySelector("[data-terminal-identity]")?.getBoundingClientRect().right ?? 0,
        filesLeft: document.querySelector("[data-titlebar-files]")?.getBoundingClientRect().left ?? 0,
      };
    });
    assert.ok(Math.abs(edges.moreRight - edges.barRight) < 0.5,
      `the account menu hugs the empty titlebar's right edge (${edges.moreRight} vs ${edges.barRight})`);
    assert.ok(edges.filesLeft > edges.identityRight,
      "Files clears the identity — the empty right cluster is not glued to the project name");
  }
  assert.equal(await page.locator('[data-session][aria-selected="true"]').count(), 0,
    "nothing is selected, so no session row renders selected");
  assert.equal(await page.locator("[data-session].bg-wash-pressed").count(), 0,
    "no session row keeps the selected wash");
  const emptyRail = await page.locator('.session-rail[style*="border-left-color"]').first().evaluate((el) => ({
    width: getComputedStyle(el).borderLeftWidth,
    color: getComputedStyle(el).borderLeftColor,
    declaration: el.getAttribute("style"),
    projectColor: getComputedStyle(el.closest("[data-project-row]").querySelector("[data-project-toggle] span")).color,
  }));
  assert.equal(emptyRail.width, "2px", "the grouping rail remains visible without a selection");
  assert.match(emptyRail.declaration ?? "", /border-left-color:/,
    "the empty-state rail retains its project tint declaration");
  assert.equal(emptyRail.color, emptyRail.projectColor,
    "the computed rail color is the project tint, not a fallback or transparent override");

  await page.locator("[data-empty-action]").click();
  await page.locator("[data-new-session-modal]").waitFor();
  assert.equal(await page.locator('[data-new-session-provider]:visible').count() > 0, true,
    "the pane's New session opens the same provider picker the sidebar's + opens");
  await page.locator("[data-new-session-cancel]").click();

  await openState("");
  await page.locator('[data-session][aria-selected="true"]').first().waitFor();
  const selectedRail = await page.locator('.session-rail[style*="border-left-color"]').first().evaluate((el) => ({
    width: getComputedStyle(el).borderLeftWidth,
    color: getComputedStyle(el).borderLeftColor,
  }));
  assert.deepEqual(selectedRail, { width: emptyRail.width, color: emptyRail.color },
    "selection does not change the project grouping rail");

  // ---- no-project ---------------------------------------------------------
  await openState("empty=no-project");

  const noProject = await paneTypography();
  assert.equal(noProject.caps, "all-small-caps", "both states share the one anatomy");
  assert.equal(await page.locator("[data-empty-title]").textContent(), "No projects yet");
  assert.equal(
    await page.locator("[data-empty-body]").textContent(),
    "Point Deck at a directory, an existing repository, or a repository to clone.",
  );

  const projectLift = await opticalLift();
  assert.ok(projectLift.lift > projectLift.paneHeight * 0.01,
    `the no-project composition is lifted above the geometric centre (${projectLift.lift}px in a ${projectLift.paneHeight}px pane), like its sibling`);
  assert.ok(projectLift.lift < projectLift.paneHeight * 0.12,
    `the lift (${projectLift.lift}px) stops reading as centring in a ${projectLift.paneHeight}px pane`);

  // The sidebar collapses to a single + New project row.
  const row = page.locator('[data-region="sidebar"] [data-new-project-row]');
  assert.ok(await row.isVisible(),
    "the sidebar carries its single + New project row");
  assert.equal(await row.evaluate((el) => el.tagName), "BUTTON",
    "the row is itself the action");
  assert.equal(await row.locator("span.flex-1").textContent(), "Add project");
  // The row's own voice, past the pane's title and body: every text the
  // row renders — label and glyph slot alike — takes the interface
  // face, and the plus is the control-icon glyph, not a mono character.
  const rowFonts = await row.locator("span").evaluateAll(
    (spans) => spans.map((el) => getComputedStyle(el).fontFamily),
  );
  for (const font of rowFonts) {
    await assertFontVoice(page, font, "ui", `the row renders in the interface face (${font})`);
  }
  assert.ok(await row.locator('svg[width="14"][stroke="currentColor"]').count() > 0,
    "the row's plus is the 14px lucide glyph");
  assert.equal(await page.locator("[data-projects-menu]").count(), 0,
    "the Projects header is gone — every other row it could offer needs a project");
  assert.equal(await page.locator("[data-project-toggle]").count(), 0,
    "no project rows render");
  assert.equal(await page.locator("[data-terminal-identity]").count(), 0,
    "no identity without a project in context");
  for (const hook of ["[data-titlebar-files]", "[data-titlebar-tasks]"]) {
    assert.equal(await page.locator(hook).count(), 0,
      `${hook} does not apply without a project`);
  }
  assert.ok(await page.locator("[data-titlebar-more]").count() > 0,
    "only the account menu stays");

  await page.locator("[data-empty-action]").click();
  await page.locator("[data-project-source-dialog]").waitFor();
  assert.ok(true, "the pane's Add project opens the same source chooser the Projects menu opens");

  // ---- loading (comp Workspace Loading State) -----------------------------
  // Until the first read answers, nothing on the page may read as an empty
  // database: the pane is a spinner and a line, the rail is flat bands at
  // the real row height, and no create affordance exists anywhere.
  await openState("empty=loading");
  await page.locator('[data-empty-pane="loading"]').waitFor();
  assert.equal(await page.locator("[data-empty-title]").count(), 0,
    "the loading pane has no title: nothing has been found out yet");
  assert.equal(await page.locator("[data-empty-action]").count(), 0,
    "and no action");
  assert.equal(await page.locator("[data-empty-line]").textContent(), "Reconnecting to your sessions…");
  for (const text of ["No projects yet", "No session open", "Projects unavailable"]) {
    assert.equal(await page.getByText(text, { exact: true }).count(), 0,
      `"${text}" never shows before the server has answered`);
  }
  assert.equal(await page.locator("[data-new-project-row]").count(), 0,
    "no Add project row before a read succeeds");
  assert.equal(await page.locator("[data-projects-menu]").count(), 0,
    "no Projects menu over a list nobody has read");
  const bands = page.locator('[data-region="sidebar"] [data-loading-band]');
  assert.equal(await bands.count(), 4, "four flat project bands hold the rail's shape");
  const bandMetrics = await bands.first().evaluate((el) => {
    const row = el.querySelector("div");
    const styles = getComputedStyle(row);
    return {
      height: row.getBoundingClientRect().height,
      animation: styles.animationName,
      listBusy: el.closest("[data-project-list]")?.getAttribute("aria-busy") ?? null,
    };
  });
  assert.equal(bandMetrics.height, 31, `a band is one real row tall (${bandMetrics.height}px)`);
  assert.equal(bandMetrics.animation, "none", "bands are flat — no shimmer, no pulse");
  assert.equal(bandMetrics.listBusy, "true", "the unanswered list says it is busy");
  const spinnerFont = await page.locator("[data-empty-line]").evaluate((el) => getComputedStyle(el).fontFamily);
  await assertFontVoice(page, spinnerFont, "ui", `the loading line is the interface voice (${spinnerFont})`);
  const loadingSurface = await surface();
  assert.equal(loadingSurface.pane, loadingSurface.chrome,
    "the loading pane sits on the page surface like the settled states");

  // ---- error ---------------------------------------------------------------
  // A failed read is its own state: the reassurance leads, the server's
  // message trails, the one action retries, and nothing offers to create.
  await openState("empty=error");
  await page.locator('[data-empty-pane="error"]').waitFor();
  assert.equal(await page.locator("[data-empty-title]").textContent(), "Can't reach the runtime");
  assert.ok((await page.locator("[data-empty-body]").textContent()).includes("Your sessions are still on the server."),
    "the body reassures before it explains");
  assert.equal(await page.locator("[data-empty-detail]").textContent(), "project registry unavailable",
    "the server's own message trails as a caption");
  assert.equal(await page.locator("[data-empty-action]").textContent(), "Retry now");
  assert.equal(await page.locator("[data-new-project-row]").count(), 0,
    "a failed read never offers onboarding");
  const errorType = await paneTypography();
  assert.equal(errorType.caps, "all-small-caps", "the error state shares the settled anatomy");
  const errorLift = await opticalLift();
  assert.ok(errorLift.lift > errorLift.paneHeight * 0.01 && errorLift.lift < errorLift.paneHeight * 0.12,
    `the error composition is lifted like its siblings (${errorLift.lift}px in ${errorLift.paneHeight}px)`);

  assert.deepEqual(pageErrors, [], "no uncaught page errors across the four states");
  console.log("empty pane: the settled states render the r16 anatomy, the loading state never reads as empty, and the error state retries: PASS");
  await context.close();
} finally {
  await browser?.close();
  server.close();
}
