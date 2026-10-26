import assert from "node:assert/strict";
import test from "node:test";
import { sidebarBounds, sidebarPreference } from "./sidebar-size.ts";

test("invalid stored widths use the default instead of breaking layout", () => {
  for (const value of [null, "", " ", "NaN", "Infinity", "-20", "0", "239", "561", "bad"]) assert.equal(sidebarPreference(value), 272);
  assert.equal(sidebarPreference("400"), 400);
});

test("viewport bounds leave space for the main column without changing the saved preference", () => {
  assert.deepEqual(sidebarBounds(1200), { min: 240, max: 560 });
  assert.deepEqual(sidebarBounds(800), { min: 240, max: 440 });
  assert.deepEqual(sidebarBounds(600), { min: 240, max: 240 });
  assert.equal(sidebarPreference("560"), 560);
});
