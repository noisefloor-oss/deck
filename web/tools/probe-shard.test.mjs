import { readFileSync } from "node:fs";
import { test } from "node:test";
import assert from "node:assert/strict";
import { selectShard } from "./probe-shard.mjs";

test("shards cover every probe exactly once, including separate modes", () => {
  const entries = Array.from({ length: 26 }, (_, i) => ({ probe: i }));
  entries[2] = ["activity", "tasks-panel", { mode: "activity" }];
  const shards = [1, 2, 3].map((index) => selectShard(entries, `${index}/3`));
  assert.deepEqual(shards.map((shard) => shard.length), [9, 9, 8]);
  const flattened = shards.flat();
  assert.equal(new Set(flattened).size, entries.length);
  for (const entry of entries) assert.ok(flattened.includes(entry));
  assert.deepEqual(shards[2][0], entries[2]);
});

test("local runs retain the full inventory", () => {
  const entries = ["one", "two"];
  assert.equal(selectShard(entries), entries);
  assert.deepEqual(selectShard(entries, "1/1"), entries);
});

test("invalid or empty shards fail instead of passing with no tests", () => {
  for (const shard of ["", "0/3", "4/3", "1/0", "1/4", "1.5/3", "1/3junk", "9007199254740992/9007199254740992"]) {
    assert.throws(() => selectShard(["a", "b", "c"], shard));
  }
});


test("measured weights balance heavy probes while preserving exact coverage", () => {
  const entries = ["heavy-a", "heavy-b", "light-a", "light-b", ["mode", "script", {}], "new"];
  const weights = { "heavy-a": 80, "heavy-b": 70, "light-a": 10, "light-b": 20, mode: 5 };
  const shards = [1, 2, 3].map((i) => selectShard(entries, `${i}/3`, weights));
  assert.equal(new Set(shards.flat()).size, entries.length);
  for (const entry of entries) assert.ok(shards.flat().includes(entry));
  assert.ok(!shards.some((s) => s.includes("heavy-a") && s.includes("heavy-b")));
  assert.deepEqual(shards, [1, 2, 3].map((i) => selectShard(entries, `${i}/3`, weights)));
});

test("invalid weights and duplicate names cannot silently omit probes", () => {
  for (const seconds of [-1, NaN, Infinity, "1"]) {
    assert.throws(() => selectShard(["a"], "1/1", { a: seconds }));
  }
  assert.throws(() => selectShard(["a", ["a", "other", {}]], "1/2", {}));
});


test("actual measured baseline covers every shard even with zero-duration observations", () => {
  const weights = JSON.parse(readFileSync(new URL("./probe-durations.json", import.meta.url), "utf8")).seconds;
  const entries = Object.keys(weights);
  const shards = [1, 2, 3].map((i) => selectShard(entries, `${i}/3`, weights));
  assert.ok(shards.every((shard) => shard.length > 0));
  assert.equal(new Set(shards.flat()).size, entries.length);
  const zero = { a: 0, b: 0, c: 0 };
  assert.deepEqual([1, 2, 3].map((i) => selectShard(Object.keys(zero), `${i}/3`, zero).length), [1, 1, 1]);
});
