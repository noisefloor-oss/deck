import assert from "node:assert/strict";
import test from "node:test";

import { forceSelectionModifier, nativeClipboardKey, planClipboardKey, readClipboard, viewportText, writeClipboard, type CopyKeyInput, type ScreenBuffer } from "./copy.ts";

const key = (overrides: Partial<CopyKeyInput> = {}): CopyKeyInput => ({
  type: "keydown",
  key: "c",
  metaKey: true,
  ctrlKey: false,
  altKey: false,
  shiftKey: false,
  ...overrides,
});

// The chords are ⌘C and Ctrl+Insert and nothing else. Ctrl+C is SIGINT in
// every terminal — it must fall through this handler untouched — and the
// shift/alt variants stay the browser's.
test("only plain ⌘C plans the copy fallback", () => {
  assert.equal(planClipboardKey(key(), false, true), "copy-viewport");
  assert.equal(planClipboardKey(key({ ctrlKey: true }), false, true), null, "⌃⌘C is not ours");
  assert.equal(planClipboardKey(key({ shiftKey: true }), false, true), null, "⌘⇧C is not ours");
  assert.equal(planClipboardKey(key({ altKey: true }), false, true), null, "⌥⌘C is not ours");
  assert.equal(planClipboardKey(key({ metaKey: false, ctrlKey: true }), false, true), null, "⌃C stays SIGINT");
  assert.equal(planClipboardKey(key({ key: "v" }), false, true), null, "paste is not ours");
  assert.equal(planClipboardKey(key({ type: "keyup" }), false, true), null, "only keydown");
  assert.equal(planClipboardKey(key({ key: "C", shiftKey: false }), false, true), "copy-viewport", "a capital label from an IME still matches");
});

// A selection means the native path already works and preserves exactly what
// the user highlighted — the fallback must not replace it with the whole
// screen. That holds for both chords: Ctrl+Insert raises the same copy event
// ⌘C does, which xterm's own handler answers from its selection.
test("a selection keeps the native copy path", () => {
  assert.equal(planClipboardKey(key(), true, true), null);
  assert.equal(planClipboardKey(insertKey(), true, true), null);
});

// Off macOS there is no ⌘, Ctrl+C is SIGINT, and Chrome eats Ctrl+Shift+C, so
// without this chord a user on Linux or Windows has no way to copy a
// screen the application is holding the mouse over.
test("Ctrl+Insert plans the same fallback for a keyboard without ⌘", () => {
  assert.equal(planClipboardKey(insertKey(), false, true), "copy-viewport");
  assert.equal(planClipboardKey(insertKey({ shiftKey: true }), false, true), null, "⇧Insert is paste by convention");
  assert.equal(planClipboardKey(insertKey({ altKey: true }), false, true), null, "⌥⌃Insert is not ours");
  assert.equal(planClipboardKey(insertKey({ metaKey: true }), false, true), null, "⌘⌃Insert is not ours");
  assert.equal(planClipboardKey(insertKey({ ctrlKey: false }), false, true), null, "a bare Insert is the pane's");
  assert.equal(planClipboardKey(insertKey({ type: "keyup" }), false, true), null, "only keydown");
});

const insertKey = (overrides: Partial<CopyKeyInput> = {}): CopyKeyInput => ({
  type: "keydown",
  key: "Insert",
  metaKey: false,
  ctrlKey: true,
  altKey: false,
  shiftKey: false,
  ...overrides,
});

const buffer = (lines: string[], viewportY = 0): ScreenBuffer => {
  const line = (text: string) => ({ translateToString: (trim?: boolean) => trim ? text.trimEnd() : text });
  return {
    viewportY,
    getLine: (y: number) => (y >= 0 && y < lines.length ? line(lines[y]!) : undefined),
  };
};

test("viewportText assembles the visible screen without cell padding", () => {
  assert.equal(viewportText(buffer(["$ cargo test   ", "ok. 3 passed", "", ""]), 4), "$ cargo test\nok. 3 passed\n\n");
});

test("a missing line copies as empty rather than undefined", () => {
  assert.equal(viewportText(buffer(["only one"]), 3), "only one\n\n");
});

// getLine indexes the whole buffer, so a screen that has scrolled starts at
// viewportY. Reading from zero instead copies the oldest scrollback — text
// the user cannot see — under a label that says "screen".
test("a scrolled screen is copied from the viewport, not the top of the buffer", () => {
  const lines = ["scrolled-off-0", "scrolled-off-1", "visible-0", "visible-1"];
  assert.equal(viewportText(buffer(lines, 2), 2), "visible-0\nvisible-1");
});

// The viewport can sit at the very end of the buffer while the emulator
// still reports its full row count; the rows past the last line are blank
// screen, not missing content.
test("rows past the end of the buffer stay blank", () => {
  assert.equal(viewportText(buffer(["a", "b"], 1), 3), "b\n\n");
});


test("Windows clipboard chords preserve selection copy and native paste", () => {
  const ctrl = key({ metaKey: false, ctrlKey: true });
  assert.equal(nativeClipboardKey(ctrl, true, true), true);
  assert.equal(nativeClipboardKey(ctrl, false, true), false, "no selection keeps interrupt");
  for (const selected of [true, false]) {
    assert.equal(nativeClipboardKey({ ...ctrl, key: "v" }, selected, true), true);
    assert.equal(nativeClipboardKey({ ...ctrl, key: "V", shiftKey: true }, selected, true), true);
  }
  for (const change of [{ altKey: true }, { metaKey: true }, { shiftKey: true }, { type: "keyup" }, { ctrlKey: false }]) {
    assert.equal(nativeClipboardKey({ ...ctrl, ...change }, true, true), false);
  }
  assert.equal(nativeClipboardKey(ctrl, true, false), false, "non-Windows Ctrl+C remains interrupt");
  assert.equal(nativeClipboardKey({ ...ctrl, key: "v" }, true, false), false, "non-Windows Ctrl+V remains a CLI byte");
});
// Off macOS the chords are the ones Windows and Linux users press. Ctrl+C
// copies only while text is selected, so an interrupt still reaches the pane
// the rest of the time; the Insert pair and the shifted pair never reach the
// browser (Chrome's Ctrl+Shift+C is inspect-element).
test("Windows and Linux chords plan copy and paste", () => {
  const k = (overrides: Partial<CopyKeyInput>) => key({ metaKey: false, ...overrides });
  assert.equal(planClipboardKey(k({ ctrlKey: true }), true, false), "copy-selection", "Ctrl+C with a selection copies it");
  assert.equal(planClipboardKey(k({ ctrlKey: true }), false, false), null, "Ctrl+C without one is SIGINT");
  assert.equal(planClipboardKey(k({ ctrlKey: true, shiftKey: true }), true, false), "copy-selection");
  assert.equal(planClipboardKey(k({ ctrlKey: true, shiftKey: true }), false, false), "copy-viewport");
  assert.equal(planClipboardKey(k({ key: "Insert", ctrlKey: true }), false, false), "copy-viewport");
  assert.equal(planClipboardKey(k({ key: "Insert", ctrlKey: true }), true, false), "copy-selection");
  assert.equal(planClipboardKey(k({ key: "Insert", shiftKey: true }), false, false), "paste");
  assert.equal(planClipboardKey(k({ key: "v", ctrlKey: true, shiftKey: true }), false, false), "paste");
  assert.equal(planClipboardKey(k({ key: "v", ctrlKey: true }), false, false), null, "Ctrl+V is the native paste");
  assert.equal(planClipboardKey(k({ ctrlKey: true, altKey: true }), true, false), null, "Alt chords are the pane's");
  assert.equal(planClipboardKey(k({ key: "C", ctrlKey: true, shiftKey: true }), true, false), "copy-selection", "shifted label");
});

test("macOS keeps ⌘C and Ctrl+Insert as the screen-copy chords and leaves ⌘V native", () => {
  assert.equal(planClipboardKey(key(), false, true), "copy-viewport");
  assert.equal(planClipboardKey(key({ key: "Insert", metaKey: false, ctrlKey: true }), false, true), "copy-viewport");
  assert.equal(planClipboardKey(key({ key: "Insert", metaKey: false, ctrlKey: true }), true, true), null, "with a selection Ctrl+Insert is the native copy on a Mac");
  assert.equal(planClipboardKey(key(), true, true), null);
  assert.equal(planClipboardKey(key({ key: "v" }), false, true), null);
  assert.equal(planClipboardKey(key({ metaKey: false, ctrlKey: true }), true, true), null, "Ctrl+C stays SIGINT on a Mac even with a selection");
});

// Every shape the clipboard can refuse in — absent (insecure context), a
// method that throws synchronously, a rejected promise — reports false or
// null and never throws; a write that lands reports true.
test("the clipboard helpers fail quietly in every shape and report what happened", async () => {
  assert.equal(await writeClipboard(undefined, "x"), false, "no clipboard API");
  assert.equal(await writeClipboard({}, "x"), false, "no writeText");
  assert.equal(await writeClipboard({ writeText: () => { throw new TypeError("no"); } }, "x"), false, "synchronous throw");
  assert.equal(await writeClipboard({ writeText: () => Promise.reject(new DOMException("denied", "NotAllowedError")) }, "x"), false, "rejected");
  const written: string[] = [];
  assert.equal(await writeClipboard({ writeText: async (t) => { written.push(t); } }, "hello"), true);
  assert.deepEqual(written, ["hello"]);
  assert.equal(await readClipboard(undefined), null);
  assert.equal(await readClipboard({}), null);
  assert.equal(await readClipboard({ readText: () => { throw new TypeError("no"); } }), null);
  assert.equal(await readClipboard({ readText: () => Promise.reject(new Error("denied")) }), null);
  assert.equal(await readClipboard({ readText: async () => "" }), null, "an empty clipboard pastes nothing");
  assert.equal(await readClipboard({ readText: async () => "paste me" }), "paste me");
});

// The platform split of the force-selection modifier, pinned so the
// Select-text toggle cannot silently break one platform: xterm reads
// Option on macOS (behind the macOptionClickForcesSelection option Deck
// enables) and Shift everywhere else.
test("the force-selection modifier is Option on macOS and Shift elsewhere", () => {
  assert.equal(forceSelectionModifier(true), "altKey");
  assert.equal(forceSelectionModifier(false), "shiftKey");
});
