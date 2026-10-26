// Runs every self-contained probe and keeps going past a failure.
//
// The probes are sequential assertion scripts, so inside one probe the first
// failure hides everything after it. What this runner guarantees is the next
// level up: one rotten probe cannot hide the others, and the summary says
// which probes ran, which failed, and which never got a verdict - a silent
// stretch must look different from a healthy run.
//
// live-smoke.mjs is deliberately absent: it is a long local smoke, not run in CI.
// mvp-shots.mjs produces screenshots, not verdicts.
import { readFileSync, writeFileSync } from "node:fs";
import { spawn } from "node:child_process";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";
import { selectShard } from "./probe-shard.mjs";

const here = dirname(fileURLToPath(import.meta.url));

// An entry is a script name, or [display name, script, extra env] for a
// probe mode selected by environment.
const PROBES = [
  "tutorial-probe.mjs",
  "tutorial-project-probe.mjs",
  "tutorial-empty-probe.mjs",
  "tutorial-explore-probe.mjs",
  "files-panel-probe.mjs",
  "image-viewer-probe.mjs",
  "tasks-panel-probe.mjs",
  ["tasks-panel-probe.mjs (activity)", "tasks-panel-probe.mjs", { NOISEDECK_ACTIVITY_PROBE: "1" }],
  ["tasks-panel-probe.mjs (composer)", "tasks-panel-probe.mjs", { NOISEDECK_COMPOSER_PROBE: "1" }],
  ["tasks-panel-probe.mjs (sidebar size)", "tasks-panel-probe.mjs", { NOISEDECK_SIDEBAR_SIZE_PROBE: "1" }],
  ["tasks-panel-probe.mjs (attention)", "tasks-panel-probe.mjs", { NOISEDECK_ATTENTION_PROBE: "1" }],
  "mobile-dialogs-probe.mjs",
  "mobile-header-probe.mjs",
  "directory-picker-probe.mjs",
  "add-project-probe.mjs",
  "projects-menu-probe.mjs",
  "row-actions-hover-probe.mjs",
  "row-menu-placement-probe.mjs",
  "mobile-terminal-scroll-probe.mjs",
  ["mobile-terminal-scroll-probe.mjs (release notice)", "mobile-terminal-scroll-probe.mjs", { NOISEDECK_RELEASE_MOBILE_PROBE: "1" }],
  "version-notice-probe.mjs",
  "shortcuts-probe.mjs",
  ["shortcuts-probe.mjs (Windows)", "shortcuts-probe.mjs", { PROBE_PLATFORM: "Windows" }],
  "git-identity-probe.mjs",
  "modal-host-probe.mjs",
  "github-connect-probe.mjs",
  "github-repositories-probe.mjs",
  "project-preview-settings-probe.mjs",
  "login-probe.mjs",
  "boot-font-probe.mjs",
  "security-enrollment-probe.mjs",
  "onboarding-probe.mjs",
  "security-runtime-probe.mjs",
  "security-coordinator-probe.mjs",
  "security-private-api-probe.mjs",
  "security-management-probe.mjs",
  "titlebar-chip-probe.mjs",
  "brand-probe.mjs",
  "caret-probe.mjs",
  "phone-route-probe.mjs",
  "new-session-probe.mjs",
  "rename-blur-probe.mjs",
  "rename-repaint-probe.mjs",
  "empty-pane-probe.mjs",
  "sidebar-idle-probe.mjs",
  "chats-panel-probe.mjs",
  "assets-panel-probe.mjs",
  "assets-news-probe.mjs",
  "close-keeps-project-probe.mjs",
  "session-row-context-probe.mjs",
  "terminal-menu-probe.mjs",
  "harness-login-probe.mjs",
  "codex-login-helper-probe.mjs",
  "harness-ranking-probe.mjs",
  "kb-settle-probe.mjs",
  "bench-probe.mjs",
  "upload-transport-probe.mjs",
];

// A probe that hangs must not hold the whole run hostage.
const PROBE_TIMEOUT_MS = 5 * 60 * 1000;

function run(entry) {
  const [probe, script, env] = Array.isArray(entry) ? entry : [entry, entry, {}];
  const started = performance.now();
  return new Promise((resolve) => {
    const child = spawn(process.execPath, [join(here, script)], {
      stdio: ["ignore", "pipe", "pipe"],
      env: { ...process.env, ...env },
    });
    let output = "";
    child.stdout.on("data", (chunk) => { output += chunk; });
    child.stderr.on("data", (chunk) => { output += chunk; });
    const timer = setTimeout(() => {
      child.kill("SIGKILL");
      resolve({ probe, verdict: "hung", output, seconds: (performance.now() - started) / 1000 });
    }, PROBE_TIMEOUT_MS);
    child.on("close", (code) => {
      clearTimeout(timer);
      resolve({ probe, verdict: code === 0 ? "pass" : "fail", output, seconds: (performance.now() - started) / 1000 });
    });
  });
}

const results = [];
const durations = JSON.parse(readFileSync(new URL("./probe-durations.json", import.meta.url), "utf8"));
const selected = selectShard(PROBES, process.env.NOISEDECK_PROBE_SHARD, durations.seconds);
for (const entry of selected) {
  const probe = Array.isArray(entry) ? entry[0] : entry;
  process.stdout.write(`==> ${probe}\n`);
  const result = await run(entry);
  results.push(result);
  if (result.verdict !== "pass") {
    // The probe's own output names the assertion that failed; everything
    // after that line never executed.
    process.stdout.write(result.output);
  }
  process.stdout.write(`<== ${probe}: ${result.verdict} (${result.seconds.toFixed(3)}s)\n`);
}

if (process.env.NOISEDECK_PROBE_METRICS) {
  writeFileSync(process.env.NOISEDECK_PROBE_METRICS, JSON.stringify({
    version: 1,
    sha: process.env.GITHUB_SHA ?? null,
    run: process.env.GITHUB_RUN_ID ?? null,
    attempt: process.env.GITHUB_RUN_ATTEMPT ?? null,
    shard: process.env.NOISEDECK_PROBE_SHARD ?? "all",
    platform: `${process.platform}/${process.arch}`,
    results: results.map(({ probe, verdict, seconds }) => ({ probe, verdict, seconds })),
  }) + "\n");
}

const failed = results.filter((r) => r.verdict !== "pass");
process.stdout.write("\n== probe summary ==\n");
for (const r of results) process.stdout.write(`  ${r.verdict.padEnd(4)}  ${r.probe}\n`);
process.stdout.write(`${results.length - failed.length}/${results.length} probes passed`
  + (failed.length > 0 ? `; ${failed.length} failed - each failure's later assertions never ran\n` : "\n"));
process.exit(failed.length === 0 ? 0 : 1);
