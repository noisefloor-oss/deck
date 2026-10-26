import assert from "node:assert/strict";
import test from "node:test";

import { placeTerminalMenu, terminalMenuHtml } from "./terminal-menu.ts";

// Terminal actions are rows of the shared surface:
// a role=menu panel of button.menuitem items, which is what makes the
// backdrop dismissal, the hint line and the menu's paint the same as every
// other popover in the app.
test("the menu offers clipboard actions on the shared surface", () => {
  const html = terminalMenuHtml({ hasSelection: true });
  assert.match(html, /role="menu"/);
  assert.match(html, /data-terminal-menu-copy[^>]*\n?[^>]*class="menuitem"/);
  assert.match(html, /data-terminal-menu-paste/);
  assert.match(html, /Paste/);
  assert.doesNotMatch(html, /data-harness-login/);
});

test("the login row appears only while a login is waiting", () => {
  const html = terminalMenuHtml({ hasSelection: true, loginLabel: "Open login helper" });
  assert.match(html, /data-harness-login data-hint="Finish the sign-in this session is waiting for"\n?[^>]*class="menuitem"/);
  assert.match(html, /<span class="flex-1">Open login helper<\/span>/);
});

// Copy takes the selection when there is one and the screen when there is
// not, and the label has to say which: the two differ by a whole screen, and
// the absence of a selection is not something the user can read off the
// pane — an ordinary drag makes none at all while a TUI tracks the mouse.
test("the copy label names what it will take", () => {
  assert.match(terminalMenuHtml({ hasSelection: true }), /<span class="flex-1">Copy<\/span>/);
  assert.match(terminalMenuHtml({ hasSelection: false }), /<span class="flex-1">Copy screen<\/span>/);
  assert.match(terminalMenuHtml({ hasSelection: true }), /data-hint="Copy the selected text"/);
  assert.match(terminalMenuHtml({ hasSelection: false }), /data-hint="Copy the text on the screen"/);
});

// Select text is a toggle, not an action: the row's pressed state is the
// session's current choice, and the menu never hides it — the row is how a
// browser user discovers that a plain drag can select at all while a TUI
// tracks the mouse.
test("the select-text row mirrors and defaults off", () => {
  assert.match(terminalMenuHtml({ hasSelection: false }), /data-terminal-menu-select aria-pressed="false"/);
  assert.match(terminalMenuHtml({ hasSelection: false, selectText: true }), /data-terminal-menu-select aria-pressed="true"/);
  assert.match(terminalMenuHtml({ hasSelection: false }), /data-hint="Let a plain drag select text while the app tracks the mouse"/);
  assert.match(terminalMenuHtml({ hasSelection: false }), /<span class="flex-1">Select text<\/span>/);
});

const box = { width: 168, height: 72 };
const viewport = { width: 1200, height: 800 };

// A context menu belongs at the pointer: the click is the anchor, and there
// is no trigger element to hang it from.
test("the menu opens down and right from the click", () => {
  assert.deepEqual(placeTerminalMenu({ x: 400, y: 300 }, box, viewport), { top: 300, left: 400 });
});

// A click in the last column or the last row would otherwise put the rows
// off screen, where they cannot be clicked at all.
test("a menu that would overflow opens back across the pointer", () => {
  assert.deepEqual(
    placeTerminalMenu({ x: 1150, y: 300 }, box, viewport),
    { top: 300, left: 1150 - box.width },
    "the right edge flips the menu leftward",
  );
  assert.deepEqual(
    placeTerminalMenu({ x: 400, y: 780 }, box, viewport),
    { top: 780 - box.height, left: 400 },
    "the bottom edge flips the menu upward",
  );
  assert.deepEqual(
    placeTerminalMenu({ x: 1150, y: 780 }, box, viewport),
    { top: 780 - box.height, left: 1150 - box.width },
    "a corner flips both axes",
  );
});

// Flipping is not enough in a viewport narrower or shorter than the panel:
// the flipped edge lands off the top or the left instead, so the clamp has
// the last word and the menu always starts on screen.
test("a menu larger than the space either side is clamped on screen", () => {
  assert.deepEqual(
    placeTerminalMenu({ x: 100, y: 40 }, box, { width: 180, height: 90 }),
    { top: 8, left: 8 },
  );
});

// The DOM-free unit fakes and the first measurement of an unmounted panel
// both report zero. A zero-sized menu overflows nothing, and treating it as
// an overflow would move every menu to the clamp corner.
test("an unmeasured menu is placed at the pointer", () => {
  assert.deepEqual(
    placeTerminalMenu({ x: 1150, y: 780 }, { width: 0, height: 0 }, viewport),
    { top: 780, left: 1150 },
  );
});
