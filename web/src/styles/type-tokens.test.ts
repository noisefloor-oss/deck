import assert from "node:assert/strict";
import { readdirSync, readFileSync } from "node:fs";
import { join } from "node:path";
import test from "node:test";

const SRC = new URL("..", import.meta.url).pathname;
const appCss = readFileSync(new URL("./app.css", import.meta.url), "utf8");

function productionFiles(dir: string): string[] {
  return readdirSync(dir, { withFileTypes: true }).flatMap((entry) => {
    if (entry.name === "bench" || entry.name === "node_modules") return [];
    const path = join(dir, entry.name);
    if (entry.isDirectory()) return productionFiles(path);
    return /\.(ts|css)$/.test(entry.name) && !entry.name.endsWith(".test.ts") ? [path] : [];
  });
}

function hasRawFontSize(source: string): boolean {
  const raw = /text-\[[^\]]*\d+(?:\.\d+)?px|font-size:\s*[^;]*\d+(?:\.\d+)?px/;
  return raw.test(source);
}

test("the raw-size guard catches direct, decimal and local-formula literals", () => {
  assert.equal(hasRawFontSize('class="text-[13.5px]"'), true);
  assert.equal(hasRawFontSize("font-size: 13.5px"), true);
  assert.equal(hasRawFontSize("font-size: min(13px)"), true);
  assert.equal(hasRawFontSize("font-size: min(var(--type-mono-size), calc(13px * var(--type-mono-xh-ratio)))"), true);
  assert.equal(hasRawFontSize('class="text-[min(var(--type-mono-size),calc(11px*var(--type-mono-xh-ratio)))]"'), true);
  assert.equal(hasRawFontSize("font-size: min(var(--type-mono-size), calc(var(--type-caption-size) * var(--type-mono-xh-ratio)))"), false);
});

test("production type roles use tokens instead of raw pixel font sizes", () => {
  const violations = productionFiles(SRC).filter((path) => hasRawFontSize(readFileSync(path, "utf8")));
  assert.deepEqual(violations, [], `raw font sizes remain: ${violations.join(", ")}`);
});

test("the scale names input and badge roles and the mono voice rule", () => {
  assert.match(appCss, /--type-input-size:\s*16px/);
  assert.match(appCss, /--type-badge-size:\s*10px/);
  assert.match(appCss, /Interface roles use the scale[\s\S]*?Mono is machine voice/);
  assert.match(appCss, /min\(var\(--type-mono-size\), calc\(<local size> \* var\(--type-mono-xh-ratio\)\)\)/);
});
