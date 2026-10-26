// The DOM-free half of shared panel resizing: the saved-width contract and
// the row bounds. The drag, keyboard and persistence lifecycle lives in the
// browser, where the panel probe exercises it.
import assert from "node:assert/strict";
import test from "node:test";

import { panelBounds, panelPreference } from "./panel-size.ts";

test("a saved width is any width the controller can persist", () => {
  // A narrow row legitimately saves below the preferred minimum, down to
  // the emergency floor, so the reader accepts that whole range and lets
  // rendering clamp it to the current row.
  for (const value of [null, "", " ", "NaN", "Infinity", "-20", "63", "1001", "bad"]) {
    assert.equal(panelPreference(value, 64, 1000), null, String(value));
  }
  assert.equal(panelPreference("268", 64, 1000), 268);
  assert.equal(panelPreference("399", 64, 1000), 399);
  assert.equal(panelPreference("620", 64, 1000), 620);
});

test("the terminal's floor outranks the panel's preferred minimum on a narrow row", () => {
  assert.deepEqual(panelBounds(1600, 400, 1000, 360), { min: 400, max: 1000 });
  assert.deepEqual(panelBounds(1000, 400, 1000, 360), { min: 400, max: 640 });
  assert.deepEqual(panelBounds(700, 400, 1000, 360), { min: 340, max: 340 });
  assert.deepEqual(panelBounds(628, 400, 1000, 360), { min: 268, max: 268 });
  assert.deepEqual(panelBounds(300, 400, 1000, 360), { min: 64, max: 64 });
  assert.equal(panelPreference("1000", 400, 1000), 1000);
});
