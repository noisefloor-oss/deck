import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import { createServer } from "node:http";
import { mkdtemp, readFile, realpath, rm, writeFile } from "node:fs/promises";
import { dirname, join } from "node:path";
import test from "node:test";

const sleep = ms => new Promise(resolve => setTimeout(resolve, ms));
async function until(predicate) { const deadline = Date.now() + 5000; while (!predicate()) { assert.ok(Date.now() < deadline, "native event delivery timed out"); await sleep(10); } }
const asset = new URL("pi-hook.js", import.meta.url);
async function load() { return (await import(`data:text/javascript;base64,${(await readFile(asset)).toString("base64")}`)).default; }
async function listen(server, address) { await new Promise((resolve, reject) => { server.once("error", reject); server.listen(address, resolve); }); }
async function close(server) { server.closeAllConnections(); await new Promise(resolve => server.close(resolve)); }
const nativeID = "01988000-1234-7abc-8abc-123456789abc";
const ctx = { sessionManager: { getSessionId: () => nativeID } };

async function captureServer(root, { holdFirstLifecycle = false } = {}) {
  let release = null;
  const records = [];
  const server = createServer((request, response) => {
    const chunks = [];
    request.on("data", value => chunks.push(value));
    request.on("end", () => { records.push({ path: request.url, body: JSON.parse(Buffer.concat(chunks).toString()) });
      if (holdFirstLifecycle && request.url === "/api/hooks/conversation") { holdFirstLifecycle = false; release = () => { response.writeHead(204); response.end(); }; }
      else { response.writeHead(204); response.end(); } });
  });
  const socket = join(root, "capture.sock"); await listen(server, socket);
  return { server, socket, records, releaseFirst: () => release?.() };
}

test("Pi sends only ordered native lifecycle metadata over its private socket", async () => {
  const root = await mkdtemp("/tmp/deck-attention-");
  const { server, socket, records, releaseFirst } = await captureServer(root, { holdFirstLifecycle: true });
  const keys = ["NOISEDECK_SESSION_HOOK_URL", "NOISEDECK_SESSION_HOOK_TOKEN", "NOISEDECK_SESSION_HOOK_SOCKET"];
  const saved = keys.map(key => process.env[key]);
  Object.assign(process.env, { NOISEDECK_SESSION_HOOK_URL: "http://127.0.0.1:1/api/hooks/capture", NOISEDECK_SESSION_HOOK_TOKEN: "synthetic-lifecycle-grant", NOISEDECK_SESSION_HOOK_SOCKET: socket });
  try {
    const register = await load(); const handlers = new Map(); register({ on: (event, handler) => handlers.set(event, handler) });
    assert.equal(handlers.has("agent_end"), false, "an intermediate loop boundary cannot claim settlement");
    handlers.get("agent_start")({}, ctx); await until(() => records.length === 1);
    assert.equal(records.at(-1).body.state, "working");
    handlers.get("agent_settled")({ aborted: false }, ctx);
    handlers.get("agent_start")({}, ctx);
    releaseFirst();
    await until(() => records.length >= 2);
    assert.equal(records.at(-1).body.state, "working");
    assert.ok(records.at(-1).body.ended_sequence > 0, "the latest snapshot retains an ended turn even if its own delivery was coalesced");
    assert.notEqual(records.at(-1).body.ended_turn, records.at(-1).body.turn_id);

    handlers.get("ui_prompt_start")({ title: "synthetic private dialog title" }, ctx); await until(() => records.at(-1).body.state === "needs_input");
    handlers.get("ui_prompt_start")({}, ctx); handlers.get("ui_prompt_end")({}, ctx);
    await sleep(50); assert.equal(records.at(-1).body.state, "needs_input", "one remaining native dialog still needs input");
    handlers.get("ui_prompt_end")({}, ctx); await until(() => records.at(-1).body.state === "working");
    handlers.get("agent_settled")({ aborted: false, messages: ["synthetic forbidden payload"] }, ctx); await until(() => records.at(-1).body.state === "ended");
    const previous = records.at(-1).body.sequence;
    const reloaded = new Map(); register({ on: (event, handler) => reloaded.set(event, handler) });
    reloaded.get("agent_start")({}, ctx); await until(() => records.at(-1).body.sequence > previous);
    reloaded.get("agent_settled")({ aborted: true }, ctx); await until(() => records.at(-1).body.state === "unknown");
    for (const record of records) {
      assert.equal(record.path, "/api/hooks/conversation");
      assert.deepEqual(Object.keys(record.body).sort(), ["ended_sequence", "ended_turn", "native_id", "pid", "sequence", "state", "turn_id"]);
      assert.equal(record.body.native_id, nativeID);
      assert.ok(Number.isSafeInteger(record.body.sequence));
      assert.ok(!JSON.stringify(record).includes("private dialog") && !JSON.stringify(record).includes("forbidden payload"));
    }
    for (let i = 1; i < records.length; i++) assert.ok(records[i].body.sequence > records[i - 1].body.sequence);
  } finally {
    keys.forEach((key, index) => saved[index] === undefined ? delete process.env[key] : process.env[key] = saved[index]);
    await close(server); await rm(root, { recursive: true, force: true });
  }
});

test("shipped Pi settles only after a native queued continuation", { skip: !process.env.NOISEDECK_TEST_PI_EXECUTABLE, timeout: 60000 }, async () => {
  const executable = process.env.NOISEDECK_TEST_PI_EXECUTABLE;
  let directory = dirname(await realpath(executable)); let qualified = false;
  for (let i = 0; i < 6; i++, directory = dirname(directory)) {
    const pkg = await readFile(join(directory, "package.json"), "utf8").then(JSON.parse, () => null);
    if (pkg?.name === "@earendil-works/pi-coding-agent") { assert.equal(pkg.version, "1.4.0"); qualified = true; break; }
  }
  assert.ok(qualified, "native executable must be the pinned Pi package");
  const root = await mkdtemp("/tmp/deck-native-attention-");
  const { server, socket, records } = await captureServer(root);
  let requests = 0; let secondFinished = false;
  const model = createServer(async (_request, response) => {
    requests++;
    if (requests > 1) await sleep(500);
    response.writeHead(200, { "Content-Type": "text/event-stream" });
    const chunk = (delta, finish_reason = null) => response.write(`data: ${JSON.stringify({ id: "synthetic", object: "chat.completion.chunk", created: 1, model: "fixture", choices: [{ index: 0, delta, finish_reason }] })}\n\n`);
    chunk({ role: "assistant", content: "synthetic response" }); chunk({}, "stop");
    if (requests > 1) secondFinished = true;
    response.end("data: [DONE]\n\n");
  });
  await listen(model, { host: "127.0.0.1", port: 0 });
  const fixture = join(root, "fixture.mjs");
  await writeFile(fixture, `export default function(pi) {
    pi.registerProvider("deck-fixture", { baseUrl: "http://127.0.0.1:${model.address().port}/v1", apiKey: "synthetic-not-an-account-key", api: "openai-completions", models: [{ id: "fixture", name: "Fixture", reasoning: false, input: ["text"], cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 }, contextWindow: 16384, maxTokens: 32 }] });
    let continued = false;
    pi.on("agent_before_settle", () => { if (!continued) { continued = true; return { entries: [{ type: "custom_message", customType: "synthetic-followup", content: "synthetic followup", display: false }], continue: true }; } });
    pi.on("agent_settled", async () => { await new Promise(resolve => setTimeout(resolve, 500)); });
  }`);
  let premature = false;
  const watch = setInterval(() => { if (!secondFinished && records.some(record => record.body.ended_sequence > 0)) premature = true; }, 5);
  try {
    const child = spawn(executable, ["--provider", "deck-fixture", "--model", "fixture", "--thinking", "off", "--extension", asset.pathname, "--extension", fixture, "--no-session", "--mode", "json", "-p", "synthetic lifecycle check"], { cwd: root, env: { PATH: process.env.PATH, HOME: root, PI_CODING_AGENT_DIR: join(root, "agent"), NOISEDECK_SESSION_HOOK_URL: "http://127.0.0.1:1/api/hooks/capture", NOISEDECK_SESSION_HOOK_TOKEN: "synthetic-native-lifecycle-grant", NOISEDECK_SESSION_HOOK_SOCKET: socket }, stdio: ["ignore", "pipe", "pipe"] });
    let error = ""; child.stdout.resume(); child.stderr.on("data", value => { error = (error + value).slice(-2000); });
    const timeout = setTimeout(() => child.kill("SIGKILL"), 45000);
    const code = await new Promise((resolve, reject) => { child.once("error", reject); child.once("close", resolve); }); clearTimeout(timeout);
    assert.equal(code, 0, error); assert.equal(requests, 2, "native continuation really made another model request");
    await until(() => records.some(record => record.body.state === "ended"));
    assert.equal(premature, false, "intermediate agent_end did not report completed work");
    assert.equal(records.filter(record => record.body.state === "ended").length, 1);
  } finally { clearInterval(watch); await close(model); await close(server); await rm(root, { recursive: true, force: true }); }
});

test("shipped interactive Pi reports a blocking UI prompt until native confirmation", { skip: !process.env.NOISEDECK_TEST_PI_EXECUTABLE, timeout: 30000 }, async () => {
  const root = await mkdtemp("/tmp/deck-native-prompt-");
  const { server, socket, records } = await captureServer(root);
  const fixture = join(root, "ui.mjs");
  await writeFile(fixture, `export default function(pi) {
    pi.registerCommand("attention-ui", { description: "Synthetic native UI contract", handler: async (_args, ctx) => { await ctx.ui.confirm("Synthetic confirmation", "Synthetic test dialog"); } });
  }`);
  const quote = value => "'" + value.replaceAll("'", "'\\''") + "'";
  const args = [process.env.NOISEDECK_TEST_PI_EXECUTABLE, "--no-session", "--extension", asset.pathname, "--extension", fixture];
  const command = "stty cols 100 rows 30\nexec " + args.map(quote).join(" ");
  const child = spawn("script", ["-qefc", command, "/dev/null"], { detached: true, cwd: root, env: { PATH: process.env.PATH, HOME: root, TERM: "xterm-256color", PI_CODING_AGENT_DIR: join(root, "agent"), NOISEDECK_SESSION_HOOK_URL: "http://127.0.0.1:1/api/hooks/capture", NOISEDECK_SESSION_HOOK_TOKEN: "synthetic-ui-lifecycle-grant", NOISEDECK_SESSION_HOOK_SOCKET: socket }, stdio: ["pipe", "pipe", "pipe"] });
  child.stdout.resume(); child.stderr.resume(); child.stdin.on("error", () => {});
  try {
    await until(() => records.some(record => record.path === "/api/hooks/capture"));
    await sleep(150);
    child.stdin.write("/attention-ui\r");
    await until(() => records.some(record => record.body.state === "needs_input"));
    const waiting = records.find(record => record.body.state === "needs_input").body;
    child.stdin.write("\r");
    await until(() => records.some(record => record.body.sequence > waiting.sequence && record.body.state === "unknown"));
    assert.equal(records.filter(record => record.body.state === "ended").length, 0, "a UI-only command does not invent a completed agent run");
    await sleep(100);
    child.stdin.write("/new\r");
    await until(() => records.some(record => record.body.hook_event_name === "SessionStart" && record.body.session_id !== waiting.native_id));
    const switched = records.find(record => record.body.hook_event_name === "SessionStart" && record.body.session_id !== waiting.native_id).body;
    assert.equal(switched.pid, waiting.pid, "the same native process owns the new conversation");
    await until(() => records.some(record => record.body.native_id === switched.session_id && record.body.state === "unknown"));
    for (const record of records.filter(record => record.path === "/api/hooks/conversation")) assert.deepEqual(Object.keys(record.body).sort(), ["ended_sequence", "ended_turn", "native_id", "pid", "sequence", "state", "turn_id"]);
  } finally {
    if (child.exitCode === null && child.signalCode === null) {
      try { process.kill(-child.pid, "SIGKILL"); } catch {}
      await new Promise(resolve => child.once("close", resolve));
    }
    await close(server); await rm(root, { recursive: true, force: true });
  }
});
