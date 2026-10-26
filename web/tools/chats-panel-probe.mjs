// The Chats ledger (handoff §23, desktop-r50) in the built preview: a third
// docked panel beside Files and Tasks that draws what the console knows
// about the selected agent's chats. Drives dist/preview.html on the fixture
// ledger, so every assertion is deterministic: the trigger's unseen accent
// before a first open and its absence after, one-panel-width geometry, the
// quiet rows (read rows dim without a word, the newest owed row alone
// carries the courier's word, uncertain the only hue), time as a stamp,
// the Details toggle and the hover record, the member line folding from
// four, the Earlier fold, the sidebar marks on the other members' rows,
// and the info bar's news sentence at rest.
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

const near = (a, b, label, tolerance = 1.5) => assert.ok(Math.abs(a - b) <= tolerance, `${label}: ${a} vs ${b}`);
const box = async (locator) => {
  const b = await locator.boundingBox();
  assert.ok(b, "element has a box");
  return b;
};

let browser;
try {
  ({ browser } = await getBrowser());
  const context = await browser.newContext({ viewport: { width: 1400, height: 900 } });
  const page = await context.newPage();
  const pageErrors = [];
  page.on("pageerror", (error) => pageErrors.push(error.message));
  await page.goto(`${origin}/preview.html`, { waitUntil: "networkidle" });
  await page.waitForSelector("html[data-preview-state]");
  assert.equal(await page.locator("html[data-preview-error]").count(), 0, "the preview reached its capture state cleanly");

  const trigger = page.locator("[data-titlebar-chats]");
  await trigger.waitFor();
  assert.equal(await trigger.getAttribute("aria-pressed"), "false");
  await page.waitForFunction(() => document.querySelector("[data-titlebar-chats]")?.getAttribute("data-unseen") === "true");
  const visual = trigger.locator("[data-panel-trigger-visual]");
  const accent = await page.evaluate(() => getComputedStyle(document.documentElement).getPropertyValue("--color-accent").trim());
  const unseenColor = await visual.evaluate((el) => getComputedStyle(el).color);
  const tasksColor = await page.locator("[data-titlebar-tasks] [data-panel-trigger-visual]").evaluate((el) => getComputedStyle(el).color);
  assert.notEqual(unseenColor, tasksColor, "unseen messages colour the Chats trigger and nothing else");
  assert.ok(accent !== "", "the accent token is defined");

  // The news tier speaks at rest, before any hover.
  const status = page.locator("[data-info-status]");
  await page.waitForFunction(() => document.querySelector("[data-info-status]")?.dataset.mode === "news");
  const news = await status.textContent();
  assert.match(news ?? "", /^claude-sess is in a chat with codex-sess, kimi-sess · a2a#3ff6c0 written, no echo seen$/);

  // The sidebar marks the active session's row and the other members' rows, not the bystanders'.
  assert.equal(await page.locator('[data-session="sess-1"] [data-meta-chat]').getAttribute("data-meta-chat"), "unseen");
  assert.equal(await page.locator('[data-session="sess-2"] [data-meta-chat]').count(), 1, "codex-sess is in the room");
  assert.equal(await page.locator('[data-session="sess-3"] [data-meta-chat]').count(), 1, "kimi-sess is in the room");
  assert.equal(await page.locator('[data-session="sess-9"] [data-meta-chat]').getAttribute("data-meta-chat"), "live",
    "opencode-sess holds an invitation with no rows: in a live chat, nothing unseen");
  const markBox = await box(page.locator('[data-session="sess-2"] [data-meta-chat]'));
  near(markBox.width, 11, "the mark is the 11px meta glyph", 2);
  // Rows in a chat carry different ticks (15s and 12m in the fixture); the
  // marks still share one x, because the tick sits in a fixed slot.
  const otherMark = await box(page.locator('[data-session="sess-3"] [data-meta-chat]'));
  near(markBox.x, otherMark.x, "chat marks line up across rows whatever the tick reads", 0.5);
  // A tick wider than the slot (a session last heard from years ago) is
  // clipped; the mark does not move.
  await page.evaluate(() => {
    const tick = document.querySelector('[data-session="sess-3"] [data-meta-slot] span');
    if (tick) tick.textContent = "10000d";
  });
  const wideMark = await box(page.locator('[data-session="sess-3"] [data-meta-chat]'));
  near(wideMark.x, markBox.x, "a tick wider than the slot never pushes the mark", 0.5);
  const slotBox = await box(page.locator('[data-session="sess-3"] [data-meta-slot]'));
  near(slotBox.width, 26, "the slot stays 26px", 0.5);

  // Opening: one panel width, the other two closed, the trigger pressed and
  // no longer accent.
  const panel = page.locator('[data-region="chats"]');
  const closedBox = await box(panel);
  await trigger.click();
  assert.equal(await trigger.getAttribute("aria-pressed"), "true");
  await page.waitForFunction(() => document.querySelector('[data-region="chats"]')?.getAttribute("data-open") === "true");
  const openBox = await box(panel);
  near(Math.abs(openBox.x - closedBox.x), openBox.width, "the Chats panel traverses exactly one panel width");
  near(openBox.width, 400, "the Chats panel is the file list's width family");
  assert.equal(await page.locator('[data-region="tasks"]').getAttribute("data-open"), "false");
  assert.equal(await page.locator('[data-region="files"]').getAttribute("data-open"), "false");
  assert.equal(await trigger.getAttribute("data-unseen"), "false", "opening the panel is what makes rows seen");
  assert.equal(await page.locator('[data-session="sess-1"] [data-meta-chat]').getAttribute("data-meta-chat"), "live",
    "the active session's own sidebar mark clears in the same tick, from the same seen state");

  // The Chats panel rides the shared docked-panel resizing. Its default
  // width sits at the family minimum, so the drag widens: moving the left
  // boundary left grows the panel by exactly the drag, and Enter returns
  // the stylesheet's 400px family width.
  const chatsHandle = page.locator('[data-panel-resize="chats"]');
  await chatsHandle.waitFor();
  const chatsSaved = () => page.evaluate(() => localStorage.getItem("noisedeck.preview.panel.chats.width"));
  assert.equal(await chatsSaved(), null, "no Chats width is saved before the user resizes");
  const beforeChatsDrag = await box(panel);
  await page.mouse.move(beforeChatsDrag.x + 4, beforeChatsDrag.y + 100);
  await page.mouse.down();
  await page.mouse.move(beforeChatsDrag.x + 4 - 80, beforeChatsDrag.y + 100, { steps: 6 });
  await page.mouse.up();
  const widenedChats = await box(panel);
  near(widenedChats.width, beforeChatsDrag.width + 80, "an 80px left drag widens the Chats panel 80px");
  assert.equal(await chatsSaved(), String(Math.round(beforeChatsDrag.width + 80)), "the drag commits the Chats width");
  await chatsHandle.focus();
  await page.keyboard.press("Enter");
  const resetChats = await box(panel);
  near(resetChats.width, beforeChatsDrag.width, "Enter returns the Chats panel's family width");
  assert.equal(await chatsSaved(), null, "Enter clears the saved Chats width");
  assert.equal(await page.locator('[data-session="sess-2"] [data-meta-chat]').getAttribute("data-meta-chat"), "unseen",
    "another member's mark is that session's own fact and stays");

  // The header is the shared chrome; the ledger is rows, not bubbles.
  assert.equal(await panel.locator("[data-chats-header] .chrome-title").textContent(), "Chats");
  assert.equal(await panel.locator("[data-chats-close] svg").count(), 1, "the close control is a Lucide glyph");
  const room = panel.locator('[data-chat="chat-a1b2c3d4"]');
  await room.waitFor();
  assert.equal(await room.getAttribute("data-chat-state"), "open");
  assert.equal(await room.locator("[data-chat-word]").textContent(), "open");
  assert.equal(await room.locator("[data-chats-members] > span").count(), 3, "three members on the member line");
  assert.equal(await room.locator("[data-chat-row]").count(), 3);
  const words = await room.locator("[data-chat-row-word]").allTextContents();
  assert.deepEqual(words.map((w) => w.trim()), ["", "", "uncertain"],
    "read is not a word, an older owed row carries none, and the failure case keeps its word");
  assert.deepEqual(await room.locator("[data-chat-row]").evaluateAll((els) => els.map((el) => el.getAttribute("data-chat-row-settled"))), ["true", "false", "false"]);
  const readText = room.locator('[data-chat-row][data-chat-row-settled="true"] > span:nth-child(2)');
  const owedText = room.locator('[data-chat-row][data-chat-row-settled="false"] > span:nth-child(2)').first();
  assert.notEqual(await readText.evaluate((el) => getComputedStyle(el).color), await owedText.evaluate((el) => getComputedStyle(el).color),
    "a read row dims and an owed row stays bright");
  const uncertain = room.locator('[data-chat-row][data-chat-row-state="uncertain"] [data-chat-row-word]');
  const warning = await page.evaluate(() => getComputedStyle(document.documentElement).getPropertyValue("--color-warning").trim());
  assert.ok(warning !== "", "the warning token is defined");
  assert.notEqual(await uncertain.evaluate((el) => getComputedStyle(el).color), await room.locator("[data-chat-word]").evaluate((el) => getComputedStyle(el).color),
    "uncertain is the only word that takes a hue");
  assert.equal(await room.locator('[data-chat-row][data-chat-row-state="uncertain"]').getAttribute("data-hint"), "a2a#3ff6c0 · 14:23:02 · codex-sess read · kimi-sess uncertain",
    "the hover record leads with the id, then the time, then the per-recipient breakdown in the courier's register");
  assert.equal(await panel.locator(".font-mono").count(), 0, "no mono in the pane");
  assert.deepEqual((await room.locator("[data-chat-stamp]").allTextContents()).map((t) => t.trim()), ["14:22"], "time is one stamp; the rows are under a minute apart");
  assert.equal(await room.locator("[data-chat-row-time]").count(), 0);
  assert.equal(await room.locator("[data-chat-detail]").count(), 0, "the chat id line waits for Details");

  // The stress room from the 2026-09-23 audit (chat-f0486445: 17 members,
  // 41 rows): every row renders its real text - no filler rows - the
  // member line folds from its crowd, and the newest row is the last
  // thing in the ledger.
  const stressRoom = panel.locator('[data-chat="chat-f0486445"]');
  await stressRoom.waitFor();
  assert.equal(await stressRoom.getAttribute("data-chat-state"), "open");
  assert.equal(await stressRoom.locator("[data-chat-row]").count(), 41, "all 41 stress rows render");
  // The word element is the delivery word and is quiet by design; the
  // filler-row guard reads the message text off the rows themselves.
  const stressTexts = await stressRoom.locator("[data-chat-row]").allTextContents();
  assert.equal(stressTexts.length, 41);
  assert.ok(stressTexts.every((text) => /stress message \d+:/.test(text)), "no filler or empty rows in the stress room");
  assert.match(stressTexts.at(-1) ?? "", /stress message 40/, "the newest row is the ledger's last");
  const stressLine = stressRoom.locator("button[data-chats-members]");
  await stressLine.waitFor();
  assert.equal(await stressLine.getAttribute("aria-expanded"), "false", "the 17-member line stays folded");
  assert.match((await stressLine.locator("[data-chats-members-folded] span").last().textContent()) ?? "", /^\+16$/, "the folded count names the other sixteen members");

  // Details: the time on every row, the chat id under the header, the stamps go.
  const details = panel.locator("[data-chats-details]");
  assert.equal(await details.getAttribute("aria-pressed"), "false");
  await details.focus();
  await page.keyboard.press("Enter");
  assert.equal(await details.getAttribute("aria-pressed"), "true");
  assert.equal(await page.evaluate(() => document.activeElement?.getAttribute("data-chats-details")), "", "Details keeps focus across its rerender");
  assert.deepEqual((await room.locator("[data-chat-row-time]").allTextContents()).map((t) => t.trim()), ["14:22:07", "14:22:31", "14:23:02"]);
  assert.equal(await room.locator("[data-chat-stamp]").count(), 0);
  assert.match((await room.locator("[data-chat-detail]").textContent()) ?? "", /chat-a1b2c3d4\s+opened 14:22 by claude-sess/);
  assert.equal(await room.locator('[data-chat-row][data-chat-row-state="uncertain"]').getAttribute("data-hint"), "a2a#3ff6c0 · codex-sess read · kimi-sess uncertain",
    "the record drops the time the row already shows");
  await page.keyboard.press("Enter");
  assert.equal(await details.getAttribute("aria-pressed"), "false", "a second Enter toggles it back");
  assert.equal(await room.locator("[data-chat-row-time]").count(), 0);

  // From four members the line folds to the opener, tiles and a count, and is a disclosure button.
  const invitation = panel.locator('[data-chat="chat-e5f6a7b8"]');
  assert.equal(await invitation.locator("[data-chat-word]").textContent(), "invited");
  const line = invitation.locator("button[data-chats-members]");
  assert.equal(await line.getAttribute("aria-expanded"), "false");
  assert.equal(await line.locator("> span").count(), 2, "the opener's unit and the folded run");
  assert.equal(await line.locator("[data-chats-members-folded] > span").count(), 2, "three tiles and a count");
  assert.equal((await line.locator("[data-chats-members-folded] > span:last-child").textContent())?.trim(), "+3");
  assert.equal(await line.locator("[data-chats-members-folded] [aria-hidden]").count(), 3);
  await line.focus();
  await page.keyboard.press("Enter");
  assert.equal(await invitation.locator("button[data-chats-members]").getAttribute("aria-expanded"), "true");
  assert.equal(await invitation.locator("button[data-chats-members] > span").count(), 4, "expanded, every member is named");
  assert.equal(await invitation.locator("[data-chats-members-folded]").count(), 0);
  assert.equal(await page.evaluate(() => document.activeElement?.closest("[data-chat]")?.getAttribute("data-chat")), "chat-e5f6a7b8",
    "the replacement disclosure holds focus");
  await page.keyboard.press("Enter");
  assert.equal(await invitation.locator("button[data-chats-members]").getAttribute("aria-expanded"), "false", "Enter again folds it");
  await invitation.locator("button[data-chats-members]").click();
  assert.equal(await invitation.locator("button[data-chats-members]").getAttribute("aria-expanded"), "true");
  await invitation.locator("button[data-chats-members]").click();
  assert.equal(await invitation.locator("button[data-chats-members]").getAttribute("aria-expanded"), "false");
  assert.equal(await room.locator("span[data-chats-members]").count(), 1, "a line that names everyone is not a control");

  // Earlier folds the ended chats, one line each, and opens in place.
  const earlier = panel.locator("[data-chats-earlier]");
  assert.equal(await earlier.locator("[data-chats-earlier-toggle]").getAttribute("aria-expanded"), "false");
  assert.equal(await earlier.locator("[data-chat-earlier]").count(), 0);
  const listBefore = await box(panel.locator("[data-chats-list]"));
  await earlier.locator("[data-chats-earlier-toggle]").click();
  assert.equal(await earlier.locator("[data-chat-earlier]").count(), 4);
  const states = await earlier.locator("[data-chat-earlier] [data-chat-earlier-open] > span:last-child").allTextContents();
  assert.deepEqual(states.map((s) => s.trim()), ["closed", "declined", "closed", "left"]);
  // An ended chat is a row that opens in place to its rows and folds again.
  const ended = earlier.locator('[data-chat-earlier="chat-7c0e91f2"]');
  const endedRow = ended.locator("button[data-chat-earlier-open]");
  assert.equal(await endedRow.getAttribute("aria-expanded"), "false");
  assert.equal(await ended.locator("[data-chat-row]").count(), 0, "an ended chat's rows wait to be opened");
  assert.equal((await endedRow.locator("> span:nth-last-child(2)").textContent())?.trim(), "6 messages");
  await endedRow.focus();
  await page.keyboard.press("Enter");
  assert.equal(await ended.locator("button[data-chat-earlier-open]").getAttribute("aria-expanded"), "true");
  assert.equal(await ended.locator("[data-chat-row]").count(), 6, "open, the ended chat reads whole");
  assert.equal(await page.evaluate(() => document.activeElement?.closest("[data-chat-earlier]")?.getAttribute("data-chat-earlier")), "chat-7c0e91f2",
    "the replacement row holds focus");
  assert.match((await ended.locator("[data-chat-row]").first().textContent()) ?? "", /scout, can you take the pi contract probe/);
  assert.equal(await ended.locator("[data-chat-row][data-chat-row-settled=\"true\"]").count(), 6, "every row of an ended chat was read");
  const endedRowBox = await box(ended.locator("button[data-chat-earlier-open]"));
  const endedListBox = await box(panel.locator("[data-chats-list]"));
  assert.ok(endedRowBox.x + endedRowBox.width <= endedListBox.x + endedListBox.width + 0.5, "the row stays inside the list");
  assert.equal(await panel.locator("[data-chats-list]").evaluate((el) => el.scrollWidth <= el.clientWidth), true, "the list never scrolls sideways");
  await page.keyboard.press("Enter");
  assert.equal(await ended.locator("button[data-chat-earlier-open]").getAttribute("aria-expanded"), "false", "Enter again folds it");
  assert.equal(await ended.locator("[data-chat-row]").count(), 0);
  assert.match(await earlier.locator('[data-chat-earlier="chat-7c0e91f2"] [data-chat-earlier-open]').getAttribute("data-hint"), /^claude-sess opened it and ended it for everyone at \d\d:\d\d$/, "the opener ends a current chat without an agreement handshake");
  assert.match(await earlier.locator('[data-chat-earlier="chat-legacy-quorum"] [data-chat-earlier-open]').getAttribute("data-hint"), /^Everyone agreed to end it at \d\d:\d\d$/, "a historical quorum completed by the opener retains its original meaning");
  assert.equal(await earlier.locator('[data-chat-earlier="chat-member-left"] [data-chat-earlier-open]').getAttribute("data-hint"), "claude-sess left the chat at 14:26", "departure time is not the other members' subsequent 14:28 activity");
  assert.equal(await panel.locator('[data-chat="chat-member-left"]').count(), 0, "a pre-adoption legacy departure is not a live chat for the leaver");
  assert.equal(await earlier.locator('[data-chat-earlier="chat-member-left"] [data-chat-handshake-rows]').count(), 0, "the legacy proposal's unfinished handshake is not an active obligation for the leaver");
  const continuing = panel.locator('[data-chat="chat-another-member-left"]');
  assert.equal(await continuing.locator("[data-chat-word]").textContent(), "open", "another member leaving does not end the chat");
  assert.equal(await continuing.locator("[data-chat-handshake-rows]").count(), 0, "current chats have no closing quorum");
  assert.match(await continuing.textContent(), /codex-sess left/);
  assert.doesNotMatch(await continuing.locator("[data-chats-members]").textContent(), /codex-sess/, "the leaver is absent from the current member line");
  // A member whose chosen name has no harness in it colours by what
  // presence says it runs on; no tile is ever the sunken surface.
  const scout = earlier.locator('[data-chat-earlier="chat-7c0e91f2"] [data-chat-tile="pi"]');
  assert.equal(await scout.count(), 1, "scout's tile is pi's, from the ledger's harness map");
  assert.equal(await panel.locator(".bg-surface-sunken").count(), 0, "no tile falls back to a surface");
  const sunken = await page.evaluate(() => getComputedStyle(document.documentElement).getPropertyValue("--color-surface-sunken").trim());
  const scoutColor = await scout.evaluate((el) => getComputedStyle(el).backgroundColor);
  assert.ok(scoutColor !== "" && scoutColor !== "rgba(0, 0, 0, 0)", "the tile is painted");
  assert.notEqual(scoutColor, sunken);
  const listAfter = await box(panel.locator("[data-chats-list]"));
  near(listAfter.x, listBefore.x, "unfolding Earlier navigates nowhere");
  assert.match(await earlier.locator('[data-chat-earlier="chat-2d9a40bb"] [data-chat-earlier-open]').getAttribute("data-hint"), /^claude-sess declined the invitation at \d\d:\d\d$/);

  // Hovering a row puts its record in the bar; leaving returns the news. The
  // focused disclosure's hint would otherwise hold the bar, so focus leaves first.
  await page.evaluate(() => document.activeElement?.blur());
  await room.locator('[data-chat-row][data-chat-row-state="uncertain"]').hover();
  await page.waitForFunction(() => document.querySelector("[data-info-status]")?.textContent === "a2a#3ff6c0 · 14:23:02 · codex-sess read · kimi-sess uncertain");
  assert.equal(await status.getAttribute("data-mode"), "hint");
  await page.mouse.move(5, 5);
  await page.waitForFunction(() => document.querySelector("[data-info-status]")?.dataset.mode === "news");

  // Closing from the panel's own control.
  assert.equal(await panel.locator("[data-chats-close]").getAttribute("data-hint"), "Hide the chats panel; participation is unchanged");
  await panel.locator("[data-chats-close]").click();
  await page.waitForFunction(() => document.querySelector('[data-region="chats"]')?.getAttribute("data-open") === "false");
  assert.equal(await trigger.getAttribute("aria-pressed"), "false");
  assert.equal(await trigger.getAttribute("data-unseen"), "false", "the rows stay seen once they have been in front of the user");

  // Files, Tasks and Chats are siblings: opening Tasks closes Chats.
  await trigger.click();
  assert.equal(await room.locator("[data-chat-word]").textContent(), "open", "dismissing and reopening leaves participation unchanged");
  assert.equal(await continuing.locator("[data-chat-word]").textContent(), "open");
  await page.locator("[data-titlebar-tasks]").click();
  await page.waitForFunction(() => document.querySelector('[data-region="tasks"]')?.getAttribute("data-open") === "true");
  assert.equal(await panel.getAttribute("data-open"), "false");

  // A short viewport makes the list scroll. Opening lands on the newest
  // row; a repaint (Details is one) keeps a list that was scrolled up where
  // it was and keeps a list showing the newest row at the newest row.
  const short = await context.newPage();
  short.on("pageerror", (error) => pageErrors.push(error.message));
  await short.setViewportSize({ width: 1400, height: 360 });
  await short.goto(`${origin}/preview.html`, { waitUntil: "networkidle" });
  await short.waitForSelector("html[data-preview-state]");
  await short.locator("[data-titlebar-chats]").click();
  await short.waitForSelector('[data-region="chats"] [data-chat]');
  // ChatsPanel follows the newest row through an async ResizeObserver, so
  // wait for the scroll to settle at the end before reading geometry.
  await short.waitForFunction(() => {
    const list = document.querySelector('[data-region="chats"] [data-chats-list]');
    return list ? Math.abs(list.scrollTop - (list.scrollHeight - list.clientHeight)) <= 4 : false;
  }, null, { timeout: 2000 });
  const shortList = short.locator('[data-region="chats"] [data-chats-list]');
  const placeOf = () => shortList.evaluate((el) => ({ top: el.scrollTop, end: el.scrollHeight - el.clientHeight }));
  let place = await placeOf();
  assert.ok(place.end > 40, `the short list scrolls (${place.end}px of travel)`);
  near(place.top, place.end, "opening shows the newest row, not the first", 4);
  await shortList.evaluate((el) => { el.scrollTop = 0; });
  await short.locator('[data-region="chats"] [data-chats-details]').click();
  place = await placeOf();
  assert.equal(place.top, 0, "a repaint keeps a list scrolled up to read where it was");
  await shortList.evaluate((el) => { el.scrollTop = el.scrollHeight; });
  await short.locator('[data-region="chats"] [data-chats-details]').click();
  place = await placeOf();
  near(place.top, place.end, "a repaint keeps a list at the newest row at the newest row", 4);
  await short.close();

  assert.deepEqual(pageErrors, [], "no page errors");
  console.log("chats-panel-probe: ok");
} finally {
  await browser?.close();
  server.close();
}
