// Real-browser probe for the Add project dialog (comp desktop-r38 to r45):
// explicit local/URL choices. It drives the built preview through the
// fixture's sample workspace and scripted clone: recognition for each kind
// of input, Browse writing back into the field, a duplicate's Show it, the
// clone's run / background / rail row / cancel / resume / done arc, a
// refused key, and typing on every screen through a late answer. Markup
// tests pin the decision table; this proves the wiring.
import assert from "node:assert/strict";
import { createServer } from "node:http";
import { readFile } from "node:fs/promises";
import { existsSync } from "node:fs";
import { extname, join, resolve } from "node:path";
import { browser as getBrowser } from "./browser.mjs";

const WEB = resolve(import.meta.dirname, "..");
const DIST = join(WEB, "dist");
const MIME = { ".css": "text/css", ".html": "text/html", ".js": "text/javascript", ".mjs": "text/javascript", ".png": "image/png", ".svg": "image/svg+xml", ".woff2": "font/woff2" };
if (!existsSync(join(DIST, "preview.html"))) throw new Error("dist/preview.html is missing — run `pnpm build` first");

function serve(dir) {
  const server = createServer(async (req, res) => {
    const path = join(dir, decodeURIComponent(new URL(req.url, "http://x").pathname));
    if (!path.startsWith(dir) || !existsSync(path)) { res.writeHead(404); res.end("not found"); return; }
    res.writeHead(200, { "Content-Type": MIME[extname(path)] ?? "application/octet-stream" });
    res.end(await readFile(path));
  });
  return new Promise((resolveServer) => server.listen(0, "127.0.0.1", () => resolveServer(server)));
}

const field = (page) => page.locator("[data-project-path]");
const primary = (page) => page.locator("[data-project-submit]");
const body = (page) => page.locator("[data-add-project]");

async function type(page, text) {
  await field(page).fill(text);
  // The readout states an inspection of the current input; wait for it.
  await page.waitForFunction(() => {
    const p = document.querySelector("[data-project-submit]");
    return p !== null;
  });
  await page.waitForTimeout(350);
}

async function expectText(page, text, label) {
  await page.locator(`[data-add-project]:has-text(${JSON.stringify(text)})`).waitFor({ timeout: 5000 }).catch(() => {});
  assert.ok((await body(page).textContent()).includes(text), `${label}: shows "${text}"`);
}

const server = await serve(DIST);
const origin = `http://127.0.0.1:${server.address().port}`;
const { browser, via } = await getBrowser();
console.log(`browser: ${via}`);
try {
  const page = await browser.newPage({ viewport: { width: 1502, height: 818 } });
  await page.goto(`${origin}/preview.html?dialog=open-project`, { waitUntil: "networkidle" });
  await page.locator("[data-project-form]").waitFor();

  assert.equal(await primary(page).isDisabled(), true, "empty: nothing to add");
  assert.equal(await primary(page).innerText(), "Add project");

  // The Import-from-GitHub head on every screen: Back, title, close, and
  // no Back button in the body.
  assert.deepEqual(await page.locator("[data-project-form] .dialog-head").evaluate((head) =>
    [...head.querySelectorAll("button, h2")].filter((n) => n.offsetParent !== null).map((n) => n.getAttribute("aria-label") ?? n.innerText.trim())),
  ["Back", "Add project", "Close"], "folder screen: back, title, close");
  assert.equal(await page.locator("[data-project-form] .dialog-body button:text-is('Back')").count(), 0, "no Back button in the body");
  const label = page.locator('label[for="add-project-input"]');
  assert.equal(await label.innerText(), "Folder");
  assert.equal(await label.evaluate((n) => getComputedStyle(n).fontVariantCaps), "normal", "the label is sentence case, not small caps");
  assert.equal(await field(page).getAttribute("placeholder"), "/workspace/my-project", "the example is under the projects folder");
  const opacity = () => primary(page).evaluate((n) => getComputedStyle(n).opacity);
  assert.notEqual(await opacity(), "1", "an empty form's primary looks disabled");

  await type(page, "brand-new-thing");
  await expectText(page, "New directory", "absent");
  await expectText(page, "git init · no history", "absent");
  await expectText(page, "/workspace/brand-new-thing/.git/noise-tasks/tasks.sqlite", "absent store");
  assert.equal(await page.locator("[data-project-git-profile]").count(), 0, "absent: the only identity applies without a field");
  assert.equal(await primary(page).isDisabled(), false);
  assert.equal(await opacity(), "1", "a valid form's primary looks enabled");

  await type(page, "tmp");
  await expectText(page, "What is there", "plain");
  await expectText(page, "2 directories, no .git", "plain summary");
  assert.equal(await primary(page).innerText(), "Add as project");
  assert.equal(await page.locator("[data-project-git-profile]").count(), 0, "plain: identity does not apply");
  assert.ok(!(await body(page).textContent()).includes("Task store"), "plain: no store");

  await type(page, "deck");
  await expectText(page, "origin github.com/fable/deck", "repository");
  await expectText(page, "Its own repository root.", "repository");
  assert.equal(await primary(page).innerText(), "Add project");

  await type(page, "/workspace/tmp/vd-gate2");
  await expectText(page, "A linked worktree of /workspace/visual-diff.", "worktree");
  await expectText(page, "A worktree's store lives inside the parent repository's .git", "worktree warning");
  assert.equal(await primary(page).innerText(), "Add anyway");
  assert.equal(await page.locator("[data-project-take-parent]").textContent(), "Add visual-diff instead");

  await type(page, "field-notes/docs");
  await expectText(page, "This directory is inside", "inside");
  assert.equal(await page.locator("[data-project-take-parent]").textContent(), "Open field-notes");

  await type(page, "/workspace/field-notes");
  await expectText(page, "Already in the sidebar as field-notes.", "dup");
  assert.equal(await primary(page).isDisabled(), true);
  await page.locator("[data-project-show-dup]").click();
  await page.locator("[data-project-form]").waitFor({ state: "detached" });
  assert.equal(await page.locator('[data-project-toggle="proj-1"]').getAttribute("aria-expanded"), "true", "Show it reveals the project");

  // Browse writes back into the field.
  await page.locator("[data-projects-menu]").click();
  await page.locator("[data-projects-menu-new-project]").click();
  await page.locator('[data-project-source="local"]').click();
  await page.locator("[data-project-form]").waitFor();
  await page.locator("[data-project-browse]").click();
  await page.locator('[data-directory-path="/workspace/audio-player"]').click();
  await page.locator("[data-directory-select]").click();
  await page.locator("[data-directory-dialog]").waitFor({ state: "detached" });
  assert.equal(await field(page).inputValue(), "/workspace/audio-player", "Browse fills the field");

  // A URL: clone into, then the job's arc.
  await type(page, "fable/genuinely-new-repo");
  await expectText(page, "Clone into", "url");
  assert.equal(await page.locator("[data-project-clone-dest]").inputValue(), "/workspace/genuinely-new-repo");
  assert.equal(await primary(page).innerText(), "Clone and add");
  // Browse on Clone into picks the container, leaving the repository name
  // to name the folder — and writes back to that control, not the field.
  await page.locator("[data-project-browse-dest]").click();
  await page.locator('[data-directory-path="/workspace/tmp"]').click();
  await page.locator("[data-directory-select]").click();
  await page.locator("[data-directory-dialog]").waitFor({ state: "detached" });
  await page.locator("[data-project-form]:not([data-inspecting])").waitFor();
  assert.equal(await field(page).inputValue(), "fable/genuinely-new-repo", "the field keeps the URL");
  assert.equal(await page.locator("[data-project-clone-dest]").inputValue(), "/workspace/tmp/genuinely-new-repo", "the destination moved to the chosen container");

  // The debounce race: newer text, older answer. Nothing may act on the
  // old path under the new text, so Enter inside the debounce is inert.
  await type(page, "brand-new-thing");
  assert.equal(await primary(page).isDisabled(), false);
  await field(page).fill("/workspace/field-notes");
  assert.equal(await page.locator("[data-project-form][data-inspecting]").count(), 1, "an inspection is in flight");
  assert.equal(await primary(page).isDisabled(), true, "the primary is dead while the answer is pending");
  await field(page).press("Enter");
  await page.locator("[data-project-form]:not([data-inspecting])").waitFor();
  assert.equal(await page.locator("[data-project-form]").count(), 1, "Enter during the debounce created nothing and closed nothing");
  await expectText(page, "Already in the sidebar as field-notes.", "the settled answer is about the new text");

  await page.locator('.dialog-head [data-project-source="choose"]').click();
  await page.locator('[data-project-source="url"]').click();
  assert.equal(await label.innerText(), "Repository URL");
  assert.equal(await field(page).getAttribute("placeholder"), "https://github.com/owner/repo");
  assert.equal(await page.locator("[data-project-browse]").count(), 0, "no folder picker for a URL");
  await type(page, "fable/tasks-engine");
  await expectText(page, "Already cloned here from", "same clone on disk (comp knownDirs)");
  assert.equal(await primary(page).innerText(), "Add existing clone");
  await type(page, "fable/deck");
  await expectText(page, "Already cloned here from", "same clone");
  assert.equal(await primary(page).innerText(), "Add existing clone");
  await type(page, "github.com/other/deck");
  await expectText(page, "Not empty —", "collision");
  assert.equal(await primary(page).isDisabled(), true);

  await type(page, "fable/genuinely-new-repo");
  await primary(page).click();
  await page.locator("[data-clone-percent]").waitFor();
  await expectText(page, "Cloning into genuinely-new-repo.", "running");
  await page.locator("[data-clone-background]").click();
  await page.locator("[data-project-form]").waitFor({ state: "detached" });
  await page.locator('[data-clone-row]').waitFor();
  assert.match(await page.locator('[data-clone-row]').getAttribute("data-hint"), /^Importing github\.com\/fable\/genuinely-new-repo — \d+%$/, "the rail carries the running import");
  assert.equal(await page.locator("[data-clone-row] [data-clone-ring]").count(), 1, "the ring sits where the row's actions go");
  const rowHeight = (selector) => page.locator(selector).evaluate((el) => el.getBoundingClientRect().height);
  const rowTop = (selector) => page.locator(selector).evaluate((el) => el.getBoundingClientRect().top);
  assert.equal(await rowHeight("[data-clone-row]"), await rowHeight('[data-project-toggle="proj-1"]'), "the import row is a project row's height");
  const nextProject = (selector) => page.locator(selector).evaluate((el) => el.closest("li")?.nextElementSibling?.querySelector("[data-project-toggle]")?.getAttribute("data-project-toggle"));
  assert.equal(await nextProject("[data-clone-row]"), "proj-4", "the import row sits where its name sorts, above noisefloor-site");
  // The import row is in the list's arrow-key order, and Enter reopens it.
  await page.locator('[data-project-toggle="proj-4"]').focus();
  await page.keyboard.press("ArrowUp");
  assert.equal(await page.evaluate(() => document.activeElement?.hasAttribute("data-clone-row")), true, "ArrowUp reaches the import row");
  await page.keyboard.press("Enter");
  await page.locator("[data-clone-cancel]").waitFor();
  await page.locator("[data-clone-cancel]").click();
  await page.locator("[data-clone-resume]").waitFor();
  await expectText(page, "Partial clone on disk", "stopped");
  await page.locator(".dialog-foot [data-project-cancel]").click();
  await page.locator("[data-project-form]").waitFor({ state: "detached" });
  assert.equal(await page.locator('[data-clone-row]').getAttribute("data-hint"), "Import stopped part-way — click to resume", "the rail carries the stopped import");
  assert.equal(await page.locator("[data-clone-row] [data-clone-ring]").count(), 0, "a stopped import shows the warning, not the ring");
  // A partial redraw rebuilds the import row; the rebuilt row still opens.
  await page.locator('[data-project-toggle="proj-4"]').click();
  await page.locator('[data-project-toggle="proj-4"]').click();
  const importTop = await rowTop("[data-clone-row]");
  await page.locator('[data-clone-row]').click();
  await page.locator("[data-clone-resume]").click();
  await page.locator("[data-clone-create-board]").waitFor({ timeout: 15000 });
  assert.equal(await rowTop('[data-project-toggle="proj-7"]'), importTop, "the finished project takes the import row's place");
  await expectText(page, "genuinely-new-repo imported", "done");
  await expectText(page, "Start an empty board", "done");
  await page.locator("[data-clone-create-board]").click();
  await page.locator("[data-project-form]").waitFor({ state: "detached" });
  await page.locator('[data-project-toggle="proj-7"]').waitFor();
  assert.equal(await page.locator('[data-clone-row]').count(), 0, "a finished import leaves the rail as a project");

  // An import that finishes in the background, while its row holds focus,
  // becomes the project in place and keeps focus.
  await page.locator("[data-projects-menu]").click();
  await page.locator("[data-projects-menu-new-project]").click();
  await page.locator('[data-project-source="url"]').click();
  await page.locator("[data-project-form]").waitFor();
  await type(page, "fable/hello-world");
  await primary(page).click();
  await page.locator("[data-clone-percent]").waitFor();
  await page.locator("[data-clone-background]").click();
  await page.locator("[data-project-form]").waitFor({ state: "detached" });
  await page.locator('[data-project-toggle="proj-4"]').focus();
  await page.keyboard.press("ArrowUp");
  assert.equal(await page.evaluate(() => document.activeElement?.hasAttribute("data-clone-row")), true, "the import row holds focus");
  const backgroundTop = await rowTop("[data-clone-row]");
  await page.locator('[data-project-toggle="proj-8"]').waitFor({ timeout: 15000 });
  assert.equal(await page.locator('[data-clone-row]').count(), 0, "the import row became the project");
  assert.equal(await rowTop('[data-project-toggle="proj-8"]'), backgroundTop, "a background completion does not move the row");
  assert.equal(await page.evaluate(() => document.activeElement?.getAttribute("data-project-toggle")), "proj-8", "focus stays on the row, now the project");

  // A refused key returns to the field with git's words.
  await page.locator("[data-projects-menu]").click();
  await page.locator("[data-projects-menu-new-project]").click();
  await page.locator('[data-project-source="url"]').click();
  await page.locator("[data-project-form]").waitFor();
  await type(page, "git@github.com:fable/private.git");
  await primary(page).click();
  await page.locator("[data-project-submit]").waitFor({ timeout: 15000 });
  await expectText(page, "The server refused this machine's key.", "auth");
  await expectText(page, "ssh -T git@github.com", "auth");

  // Typing is a re-render per keystroke, and the projects folder can answer
  // mid-word: every key lands in the field and the caret stays put.
  await page.goto(`${origin}/preview.html?dialog=open-project&mount-delay=2000`, { waitUntil: "domcontentloaded" });
  await page.locator("[data-project-form]").waitFor();
  assert.equal(await field(page).getAttribute("placeholder"), "", "the folder example waits for the projects folder");
  await field(page).click();
  const word = "brand-new-thing";
  await page.keyboard.type("brand-", { delay: 40 });
  assert.equal(await field(page).getAttribute("placeholder"), "", "the answer is still out");
  await page.waitForFunction(() => document.querySelector("[data-project-path]")?.placeholder === "/workspace/my-project");
  await page.keyboard.type("new-thing", { delay: 40 });
  assert.equal(await field(page).inputValue(), word, "every keystroke landed");
  const caret = () => page.evaluate(() => {
    const active = document.activeElement;
    return active?.hasAttribute("data-project-path") ? [active.selectionStart, active.selectionEnd] : null;
  });
  assert.deepEqual(await caret(), [word.length, word.length], "focus and caret stay in the field");
  await field(page).press("Home");
  await page.locator("[data-project-form]:not([data-inspecting])").waitFor();
  await page.keyboard.type("x");
  assert.equal(await field(page).inputValue(), `x${word}`, "a key typed mid-text lands at the caret");
  assert.deepEqual(await caret(), [1, 1], "the caret follows the inserted key");

  // The late answer must not redraw the other screens either: New directory
  // in a Browse opened at a nested path, and Clone into.
  const answered = () => page.evaluate(() => document.documentElement.hasAttribute("data-preview-mount-answered"));
  const answer = () => page.waitForFunction(() => document.documentElement.hasAttribute("data-preview-mount-answered"));
  const focusedOn = (attr) => page.evaluate((name) => document.activeElement?.hasAttribute(name) ?? false, attr);

  await page.goto(`${origin}/preview.html?dialog=open-project&mount-delay=3000&project-path=/workspace/field-notes`, { waitUntil: "domcontentloaded" });
  await page.locator("[data-project-form]").waitFor();
  await page.locator("[data-project-browse]").click();
  await page.locator('[data-directory-path-line]:text-is("/workspace/field-notes")').waitFor();
  await page.locator("[data-directory-new-folder]").click();
  const newName = page.locator("[data-directory-new-name]");
  await page.keyboard.type("late-", { delay: 40 });
  assert.equal(await answered(), false, "new directory: the answer is still out");
  await answer();
  await page.keyboard.type("folder", { delay: 40 });
  assert.equal(await newName.inputValue(), "late-folder", "new directory: every keystroke landed");
  assert.equal(await focusedOn("data-directory-new-name"), true, "new directory: focus stays in the name");
  await page.keyboard.press("Enter");
  await page.locator("[data-directory-dialog]").waitFor({ state: "detached" });
  assert.equal(await field(page).inputValue(), "/workspace/new-folder", "Enter creates the folder and the fixture's answer fills the field");
  assert.equal(await field(page).getAttribute("placeholder"), "/workspace/my-project", "the field shows the folder example the answer named");

  await page.goto(`${origin}/preview.html?dialog=open-project&mount-delay=3000`, { waitUntil: "domcontentloaded" });
  await page.locator("[data-project-form]").waitFor();
  await field(page).fill("fable/genuinely-new-repo");
  const dest = page.locator("[data-project-clone-dest]");
  await dest.waitFor();
  await page.locator("[data-project-form]:not([data-inspecting])").waitFor();
  assert.equal(await answered(), false, "clone into: the answer is still out");
  await dest.click();
  await dest.press("End");
  await page.keyboard.type("-tw", { delay: 40 });
  await answer();
  await page.keyboard.type("o", { delay: 40 });
  assert.equal(await dest.inputValue(), "/workspace/genuinely-new-repo-two", "clone into: every keystroke landed");
  assert.equal(await focusedOn("data-project-clone-dest"), true, "clone into: focus stays in the destination");
  assert.equal(await field(page).getAttribute("placeholder"), "/workspace/my-project", "the folder example still arrives");

  await page.goto(`${origin}/preview.html?dialog=open-project&project-refresh-delay=4000`, { waitUntil: "networkidle" });
  await page.locator("[data-project-form]").waitFor();
  await page.locator('.dialog-head [data-project-source="choose"]').click();
  await page.locator('[data-project-source="url"]').click();
  await type(page, "fable/delayed-refresh");
  await primary(page).click();
  await page.locator("[data-clone-percent]").waitFor();
  await page.locator("[data-clone-background]").click();
  await page.locator("[data-project-form]").waitFor({ state: "detached" });
  await page.locator("html[data-preview-project-refresh-pending]").waitFor({ timeout: 15000 });
  await page.locator('[data-project-toggle="proj-4"]').click();
  const delayedRow = page.locator("[data-clone-row]");
  await delayedRow.waitFor();
  assert.equal(await delayedRow.getAttribute("data-hint"), "Import complete — refreshing projects");
  assert.equal(await delayedRow.locator(".text-warning").count(), 0, "completion does not draw a stopped warning");
  assert.equal(await delayedRow.locator(".text-accent").count(), 1, "completion draws the accent check");
  assert.equal(await page.locator('[data-project-toggle="proj-7"]').count(), 0,
    "the project is still absent while its refreshed list is delayed");
  await page.setViewportSize({ width: 390, height: 844 });
  const projectFilter = page.locator("[data-sheet-filter]");
  await delayedRow.focus();
  await projectFilter.evaluate((input) => {
    input.value = "delayed";
    input.dispatchEvent(new Event("input", { bubbles: true }));
  });
  assert.match(await page.evaluate(() => document.activeElement?.outerHTML ?? ""), /data-clone-row="clone-1"/,
    "a redraw keeps focus on the import until its replacement is rendered");
  await page.locator('[data-project-toggle="proj-7"]').waitFor({ timeout: 10000 });
  await delayedRow.waitFor({ state: "detached" });
  assert.equal(await page.evaluate(() => document.activeElement?.getAttribute("data-project-toggle")), "proj-7",
    "focus moves to the project only when its row replaces the import");
  await projectFilter.fill("no matching project");
  assert.equal(await page.locator("[data-clone-row]").count(), 0,
    "a search that excludes the project cannot resurrect its completed import");
  await projectFilter.fill("");

  console.log("add-project probe: all assertions passed");
} finally {
  await browser.close();
  server.close();
}
