import { test } from "node:test";
import assert from "node:assert/strict";
import { touchSelectionCell, touchSelectionRange } from "./touch-selection.ts";

const box = { left: 10, top: 20, width: 800, height: 400 };
const grid = { columns: 80, rows: 20, viewportY: 100 };

test("touch selection uses screen cells and the scrolled buffer offset", () => {
  assert.equal(touchSelectionCell({ clientX: 35, clientY: 50 }, box, grid), 8082);
  assert.equal(touchSelectionCell({ clientX: -30, clientY: -10 }, box, grid), 8000);
  assert.equal(touchSelectionCell({ clientX: 900, clientY: 500 }, box, grid), 9599);
  assert.equal(touchSelectionCell({ clientX: 35, clientY: 50 }, { ...box, width: 0 }, grid), null);
});

test("touch selection includes both endpoints in either drag direction across rows", () => {
  assert.deepEqual(touchSelectionRange(8082, 8165, 80), { column: 2, row: 101, length: 84 });
  assert.deepEqual(touchSelectionRange(8165, 8082, 80), { column: 2, row: 101, length: 84 });
  assert.deepEqual(touchSelectionRange(8082, 8082, 80), { column: 2, row: 101, length: 1 });
});
