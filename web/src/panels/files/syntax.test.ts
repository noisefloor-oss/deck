import assert from "node:assert/strict";
import test from "node:test";

import { highlightedLines, MAX_SYNTAX_HIGHLIGHT_CHARS, syntaxLanguage } from "./syntax.ts";

test("the file viewer recognizes common code languages", () => {
  for (const [path, language] of Object.entries({
    "src/main.go": "go",
    "src/app.tsx": "typescript",
    "scripts/build.mjs": "javascript",
    "web/app.css": "css",
    "api/query.sql": "sql",
    "README.md": "markdown",
    "Dockerfile": "dockerfile",
    "config.yaml": "yaml",
    "src/lib.rs": "rust",
    "src/main.c": "cpp",
  })) assert.equal(syntaxLanguage(path), language, path);
  assert.equal(syntaxLanguage("notes.txt"), null);
  assert.equal(syntaxLanguage("LICENSE"), null);
});

test("large and unknown files retain the plain-text path", () => {
  assert.equal(highlightedLines("main.go", "x".repeat(MAX_SYNTAX_HIGHLIGHT_CHARS + 1)), null);
  assert.equal(highlightedLines("notes.txt", "const x = 1"), null);
});
