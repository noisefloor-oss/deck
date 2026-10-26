import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { readFileSync } from "node:fs";
import { test } from "node:test";
import { licensedFiles } from "./select.ts";

const read = (name: string): string => readFileSync(new URL(name, import.meta.url), "utf8");
const licensed = read("../../../private/fonts/licensed.css");

test("the licensed sheet declares the three licensed families and nothing free", () => {
  for (const family of ["Euclid Flex", "Euclid Flex XL", "Operator Mono SSm", "Baskerville 10 Pro"]) {
    assert.ok(licensed.includes(`font-family:"${family}"`), `${family} must be declared`);
  }
  assert.doesNotMatch(licensed, /\/fonts\/free\//);
});

test("every licensed @font-face blocks rather than swaps", () => {
  const faces = licensed.match(/@font-face\s*\{[^}]*\}/g) ?? [];
  assert.ok(faces.length > 0);
  for (const face of faces) assert.match(face, /font-display:\s*block/, face.slice(0, 80));
});

test("the licensed sheet re-states the stacks with its families first", () => {
  const root = /:root\s*\{([^}]*)\}/.exec(licensed)?.[1] ?? "";
  assert.match(root, /--font-ui:\s*"Euclid Flex"/);
  assert.match(root, /--font-display:\s*"Euclid Flex XL"/);
  assert.match(root, /--font-mono:\s*"Operator Mono SSm"/);
  assert.match(root, /--font-prose:\s*"Baskerville 10 Pro"/);
  assert.match(root, /--font-features:\s*"ss12" 1/);
});

// Exported trees omit the licensed font binaries but keep the stylesheet.
const attributesPresent = ((): boolean => {
  try {
    readFileSync(new URL("../../../../.gitattributes", import.meta.url));
    return true;
  } catch {
    return false;
  }
})();
test("licensed binaries are export-ignored; the stylesheets and free assets ship", { skip: attributesPresent ? false : "no .gitattributes: an exported tree" }, () => {
  let out: string;
  try {
    out = execFileSync("git", ["check-attr", "export-ignore", "--",
      "web/public/fonts/EuclidFlex-Regular-WebS.woff2", "web/public/fonts/OperatorMonoSSmLig-Book.woff2",
      "web/public/fonts/Baskerville10Pro.woff2", "web/private/", "web/private/fonts/licensed.css",
      "web/public/fonts/free/jost-latin-wght-normal.woff2", "web/public/fonts/free/OFL-jost.txt",
      "web/src/styles/fonts/free.css", "web/src/styles/fonts/select.ts",
    ], { cwd: new URL("../../../..", import.meta.url), encoding: "utf8" });
  } catch {
    return; // git unavailable: nothing to ask
  }
  const attr = new Map(out.trim().split("\n").map((l) => { const [p, , v] = l.split(": "); return [p, v]; }));
  for (const p of ["web/public/fonts/EuclidFlex-Regular-WebS.woff2", "web/public/fonts/OperatorMonoSSmLig-Book.woff2",
    "web/public/fonts/Baskerville10Pro.woff2"]) {
    assert.equal(attr.get(p), "set", `${p} must be export-ignored`);
  }
  for (const p of ["web/public/fonts/free/jost-latin-wght-normal.woff2", "web/public/fonts/free/OFL-jost.txt",
    "web/src/styles/fonts/free.css", "web/src/styles/fonts/select.ts",
    "web/private/", "web/private/fonts/licensed.css"]) {
    assert.equal(attr.get(p), "unspecified", `${p} must ship`);
  }
});

test("the licensed stylesheet lists the complete licensed set", () => {
  assert.equal(licensedFiles(licensed).length, 42);
});
