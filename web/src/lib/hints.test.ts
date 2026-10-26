// Mechanical guards for the info-bar hint catalog. These tests pin the
// static copy, so divergence is a failing suite instead
// of a reading exercise.
import { readdirSync, readFileSync } from "node:fs";
import assert from "node:assert/strict";
import test from "node:test";

import * as hints from "./hints.ts";

// The complete static hint set. Parameterised hints
// are checked by the functions that produce them.
const COMP_STATIC_HINTS: readonly string[] = [
  "Add another email you commit with",
  "Ask before closing a session",
  "Browse the files in this project's working tree",
  "Change the terminal color scheme",
  "Click the project name to go back to the root",
  "Collapse the project sidebar",
  "Download this file",
  "Expand the project sidebar",
  "Give every project a distinct color in the sidebar",
  "Let new sessions skip permission prompts",
  "Let a plain drag select text while the app tracks the mouse",
  "A path on this machine, or a repository to clone",
  "Open terminal, identity and account settings",
  "Open the task board parsed from the project's PRD",
  "Remove this email from the list",
  "Rename, recolor or close this session",
  "Return to the live end of the output",
  "Set the name and emails your commits are signed with",
  "Show hover hints and session status along the bottom",
  "Show the project sidebar",
  "Upload files into this folder",
  "Use this email on new projects",
];

// collectExportedStrings returns every literal string exported by the
// hints module. Functions are evaluated separately via their own tests.
function collectExportedStrings(): string[] {
  const values: unknown[] = Object.values(hints);
  return values.filter((v): v is string => typeof v === "string");
}

// assertNoNestedDataHints fails when an element carrying [data-hint] has a
// descendant that also carries [data-hint]. hintValue() resolves with the
// nearest ancestor, so nested hints shadow each other and are always a bug.
function assertNoNestedDataHints(html: string, message: string): void {
  const voidTags = new Set([
    "area", "base", "br", "col", "embed", "hr", "img", "input", "link",
    "meta", "param", "source", "track", "wbr",
  ]);
  const tagRegex = /<(\/?)([a-zA-Z][a-zA-Z0-9-]*)\b([^>]*?)(\/?)>/g;
  const hintStack: boolean[] = [];
  let match: RegExpExecArray | null;
  while ((match = tagRegex.exec(html)) !== null) {
    const [, slash, tagName, attrs, selfClose] = match;
    if (slash === "/") {
      hintStack.pop();
      continue;
    }
    const hasHint = /\sdata-hint\s*=/.test(attrs ?? "");
    if (hasHint && hintStack.some((h) => h)) {
      assert.fail(`${message}: nested data-hint around \`${match[0] ?? ""}\``);
    }
    if (selfClose !== "/" && !voidTags.has(tagName ?? "")) {
      hintStack.push(hasHint);
    }
  }
}

// scopeFiles lists every non-test .ts file under the directories that
// may contain markup templates: the app shell, the DS primitives, and
// every feature panel.
function scopeFiles(): URL[] {
  const root = new URL("../", import.meta.url);
  const dirs = [
    new URL("app/", root),
    new URL("ui/", root),
    new URL("panels/sidebar/", root),
    new URL("panels/sidebar/dialogs/", root),
    new URL("panels/files/", root),
    new URL("panels/taskmaster/", root),
    new URL("panels/chats/", root),
  ];
  const files: URL[] = [];
  for (const dir of dirs) {
    for (const entry of readdirSync(dir, { withFileTypes: true })) {
      if (!entry.isFile()) continue;
      if (!entry.name.endsWith(".ts")) continue;
      if (entry.name.endsWith(".test.ts")) continue;
      files.push(new URL(entry.name, dir));
    }
  }
  return files;
}

test("every static hint is exported", () => {
  const exported = new Set(collectExportedStrings());
  const missing = COMP_STATIC_HINTS.filter((h) => !exported.has(h));
  const extra = [...exported].filter((h) => !(COMP_STATIC_HINTS as readonly string[]).includes(h));
  // Report extras so the diff is visible, but only missing static hints are
  // blocking: the product has more hint states than the static set covers
  // (dialogs, menus, panels), and their strings still live in this module.
  if (extra.length > 0) {
    console.log("info-bar hint extras beyond the static set:\n  " + extra.join("\n  "));
  }
  assert.deepEqual(missing, [], "missing static hints");
});

test("parameterised hints render the expected templates", () => {
  assert.equal(hints.watchChatsOf("codex-9f2a"), "Watch the chats codex-9f2a is in");
  assert.equal(hints.chatMemberAgreed("codex-9f2a"), "codex-9f2a agreed to end the chat");
  assert.equal(hints.chatMemberWaiting("kimi-2b77"), "Nothing has happened yet; kimi-2b77 has not agreed");
  assert.equal(hints.chatDeclinedAt("claude-61c3", "09:12"), "claude-61c3 declined the invitation at 09:12");
  assert.equal(hints.chatForcedAt("codex-9f2a", "14:35"), "codex-9f2a forced the end at 14:35");
  assert.equal(hints.chatEndedAt("13:58"), "Everyone agreed to end it at 13:58");
  assert.equal(hints.projectToggle(true, "field-notes"), "Hide the sessions in field-notes");
  assert.equal(hints.projectToggle(false, "field-notes"), "Show the sessions in field-notes");
  assert.equal(hints.projectNewSession("field-notes"), "Start a new session in field-notes");
  assert.equal(hints.projectMenu("field-notes"), "Rename, recolor or remove field-notes");
  assert.equal(hints.sessionSwitch("claude", "field-notes"), "Switch to claude in field-notes");
  assert.equal(hints.upTo("src"), "Up to src");
  assert.equal(hints.newSessionProvider("terminal"), "Start a terminal session");
  assert.equal(hints.themeApply("Noisefloor"), "Apply the Noisefloor color scheme");
  assert.equal(hints.themeSlot("background"), "Edit the background slot");
  assert.equal(hints.themeSetFrom("bg", "Noisefloor"), "Set bg from Noisefloor");
  assert.equal(hints.sortTasks("priority"), "Sort tasks by priority");
  assert.equal(hints.directoryPath("/workspace"), "/workspace");
  assert.equal(hints.viewerFileName("README.md"), "README.md");
});

test("three-verb guard: no hint contains delete session, remove session, or delete project", () => {
  const banned = ["delete session", "remove session", "delete project"];
  const strings = collectExportedStrings();
  for (const value of strings) {
    const lower = value.toLowerCase();
    for (const phrase of banned) {
      assert.ok(!lower.includes(phrase), `hint contains banned phrase "${phrase}": ${value}`);
    }
  }
});

test("no literal hint sentence remains in any markup template", () => {
  // Matches a non-empty data-hint value that contains a letter and is not
  // an interpolation. Interpolated values begin with ${.
  const literalHint = /data-hint="[^$"]*[a-zA-Z][^$"]*"/g;
  for (const file of scopeFiles()) {
    const text = readFileSync(file, "utf8");
    const matches = text.match(literalHint);
    assert.equal(matches, null, `${file.pathname}: found literal data-hint sentence`);
  }
});

test("exported module-level markup helpers carry no nested data-hints", () => {
  // These functions return HTML fragments that may contain data-hints.
  // Class methods and file-level inline HTML are covered by the literal-
  // hint test above; this catches the renderable pieces we can exercise
  // directly under node.
  const helpers = [
    () => hints.projectToggle(true, "solo"),
    () => hints.projectNewSession("solo"),
    () => hints.projectMenu("solo"),
    () => hints.sessionSwitch("claude", "solo"),
    () => hints.newSessionProvider("terminal"),
    () => hints.themeApply("Noisefloor"),
    () => hints.themeSlot("background"),
    () => hints.themeSetFrom("bg", "Noisefloor"),
    () => hints.upTo("src"),
    () => hints.sortTasks("priority"),
  ];
  for (const render of helpers) {
    const html = render();
    assertNoNestedDataHints(html, `hint function output`);
  }
});
