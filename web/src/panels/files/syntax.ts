import hljs from "highlight.js/lib/core";
import bash from "highlight.js/lib/languages/bash";
import cpp from "highlight.js/lib/languages/cpp";
import css from "highlight.js/lib/languages/css";
import dockerfile from "highlight.js/lib/languages/dockerfile";
import go from "highlight.js/lib/languages/go";
import java from "highlight.js/lib/languages/java";
import javascript from "highlight.js/lib/languages/javascript";
import json from "highlight.js/lib/languages/json";
import markdown from "highlight.js/lib/languages/markdown";
import python from "highlight.js/lib/languages/python";
import rust from "highlight.js/lib/languages/rust";
import sql from "highlight.js/lib/languages/sql";
import typescript from "highlight.js/lib/languages/typescript";
import xml from "highlight.js/lib/languages/xml";
import yaml from "highlight.js/lib/languages/yaml";

for (const [name, language] of Object.entries({
  bash, cpp, css, dockerfile, go, java, javascript, json, markdown,
  python, rust, sql, typescript, xml, yaml,
})) hljs.registerLanguage(name, language);

export const MAX_SYNTAX_HIGHLIGHT_CHARS = 64 * 1024;

interface HighlightCache {
  path: string;
  content: string;
  lines: DocumentFragment[];
}

let highlightCache: HighlightCache | null = null;

function clonedLines(lines: DocumentFragment[]): DocumentFragment[] {
  return lines.map((line) => line.cloneNode(true) as DocumentFragment);
}

const extensionLanguages: Readonly<Record<string, string>> = {
  bash: "bash",
  c: "cpp",
  cc: "cpp",
  cjs: "javascript",
  cpp: "cpp",
  css: "css",
  cxx: "cpp",
  go: "go",
  h: "cpp",
  hpp: "cpp",
  htm: "xml",
  html: "xml",
  java: "java",
  js: "javascript",
  json: "json",
  jsx: "javascript",
  md: "markdown",
  mjs: "javascript",
  py: "python",
  rs: "rust",
  sh: "bash",
  sql: "sql",
  svg: "xml",
  ts: "typescript",
  tsx: "typescript",
  xml: "xml",
  yaml: "yaml",
  yml: "yaml",
  zsh: "bash",
};

export function syntaxLanguage(path: string): string | null {
  const name = path.slice(path.lastIndexOf("/") + 1).toLowerCase();
  if (name === "dockerfile") return "dockerfile";
  const dot = name.lastIndexOf(".");
  if (dot <= 0) return null;
  return extensionLanguages[name.slice(dot + 1)] ?? null;
}

export function highlightedLines(path: string, content: string): DocumentFragment[] | null {
  const language = syntaxLanguage(path);
  if (language === null || content.length > MAX_SYNTAX_HIGHLIGHT_CHARS) return null;
  if (highlightCache?.path === path && highlightCache.content === content) {
    return clonedLines(highlightCache.lines);
  }
  let source: string;
  try {
    source = hljs.highlight(content, { language }).value;
  } catch {
    return null;
  }
  const root = document.createElement("code");
  // The highlighter escapes file bytes before adding its own spans. Only that
  // generated result enters innerHTML; the file itself is never parsed.
  root.innerHTML = source;
  if (root.textContent !== content) return null;
  const lines = [document.createDocumentFragment()];
  const walker = document.createTreeWalker(root, NodeFilter.SHOW_TEXT);
  for (let node = walker.nextNode(); node !== null; node = walker.nextNode()) {
    const ancestors: Node[] = [];
    for (let parent = node.parentNode; parent !== null && parent !== root; parent = parent.parentNode) {
      ancestors.unshift(parent);
    }
    const parts = (node.textContent ?? "").split("\n");
    parts.forEach((part, index) => {
      if (index > 0) lines.push(document.createDocumentFragment());
      let target: Node = lines[lines.length - 1]!;
      for (const ancestor of ancestors) {
        const clone = ancestor.cloneNode(false);
        target.appendChild(clone);
        target = clone;
      }
      target.appendChild(document.createTextNode(part));
    });
  }
  highlightCache = { path, content, lines };
  return clonedLines(lines);
}
