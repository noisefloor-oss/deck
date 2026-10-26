import assert from "node:assert/strict"
import { createServer } from "node:http"
import { mkdtemp, readFile, rm } from "node:fs/promises"
import { join } from "node:path"
import { performance } from "node:perf_hooks"
import test from "node:test"

// Public synthetic fixtures only. The production hooks are imported byte for
// byte, including omp's currently JavaScript-compatible TypeScript asset.
const token = "synthetic-hook-token-not-a-credential"
const sessionID = "synthetic-native-session"
const directory = "/synthetic/project with spaces/雪"
// omp's hook names its reporting process so the capture endpoint can reject
// subagent session_starts; here that process is this test runner.
const expectedBody = (name) => name === "omp" || name === "pi"
  ? { hook_event_name: "SessionStart", session_id: sessionID, cwd: directory, source: "startup", pid: String(process.pid) }
  : { hook_event_name: "SessionStart", session_id: sessionID, cwd: directory, source: "startup" }

async function loadHook(name, filename) {
  const raw = await readFile(new URL(filename, import.meta.url))
  const module = await import(`data:text/javascript;base64,${raw.toString("base64")}`)
  if (name === "opencode" || name === "kilo") {
    const hook = name === "opencode" ? await module.AcSessionHook({ directory }) : await module.default.server({ directory })
    return { fire: () => hook.event({ event: { type: "session.created", properties: { info: { id: sessionID } } } }) }
  }
  const handlers = new Map()
  module.default({ on(event, handler) { handlers.set(event, handler) } })
  const fire = () => handlers.get("session_start")({}, { sessionManager: { getSessionId: () => sessionID, getCwd: () => directory } })
  // omp also reports the conversation an in-pane /resume lands on.
  const fireSwitch = () => handlers.get("session_switch")({}, { sessionManager: { getSessionId: () => switchedID, getCwd: () => directory } })
  return handlers.has("session_switch") ? { fire, fireSwitch } : { fire }
}
const switchedID = "synthetic-switched-session"

async function listen(server, address) {
  await new Promise((resolve, reject) => { server.once("error", reject); server.listen(address, resolve) })
}

async function close(server) {
  server.closeAllConnections()
  await new Promise((resolve, reject) => server.close((error) => error ? reject(error) : resolve()))
}

for (const [name, filename] of [["opencode", "opencode-hook.js"], ["kilo", "kilo-hook.js"], ["omp", "omp-hook.ts"], ["pi", "pi-hook.js"]]) {
  test(`${name} bounded private hook delivery`, { timeout: 30000 }, async (t) => {
    const { fire, fireSwitch } = await loadHook(name, filename)
    // Keep the Unix path short even under a deeply nested Go test TMPDIR.
    const root = await mkdtemp("/tmp/deck-hook-")
    const socket = join(root, "capture.sock")
    const tcpRequests = []
    const unixRequests = []
    let responder = (_request, response) => { response.writeHead(204); response.end() }
    const record = (collection, request, response) => {
      const chunks = []
      request.on("data", (chunk) => chunks.push(chunk))
      request.on("end", () => {
        collection.push({ method: request.method, path: request.url, headers: request.headers, body: Buffer.concat(chunks).toString("utf8") })
        responder(request, response)
      })
    }
    const tcp = createServer((request, response) => record(tcpRequests, request, response))
    const unix = createServer((request, response) => record(unixRequests, request, response))
    await listen(tcp, { host: "127.0.0.1", port: 0 })
    await listen(unix, socket)
    const url = `http://127.0.0.1:${tcp.address().port}/api/hooks/capture?synthetic=1`
    const reset = () => {
      tcpRequests.length = 0
      unixRequests.length = 0
      process.env.NOISEDECK_SESSION_HOOK_URL = url
      process.env.NOISEDECK_SESSION_HOOK_TOKEN = token
      process.env.NOISEDECK_SESSION_HOOK_SOCKET = socket
      responder = (_request, response) => { response.writeHead(204); response.end() }
    }
    const assertRequest = (request) => {
      assert.equal(request.method, "POST")
      assert.equal(request.path, "/api/hooks/capture?synthetic=1")
      assert.equal(request.headers["x-noisedeck-hook-token"], token)
      assert.equal(request.headers["content-type"], "application/json")
      assert.deepEqual(JSON.parse(request.body), expectedBody(name))
    }
    try {
      await t.test("configured socket carries exact payload without TCP", async () => {
        reset()
        await fire()
        assert.equal(unixRequests.length, 1)
        assert.equal(tcpRequests.length, 0)
        assertRequest(unixRequests[0])
      })
      await t.test("in-pane switch reports the conversation it lands on", { skip: fireSwitch ? undefined : "no switch event" }, async () => {
        reset()
        await fireSwitch()
        assert.equal(unixRequests.length, 1)
        assert.equal(tcpRequests.length, 0)
        const body = JSON.parse(unixRequests[0].body)
        assert.equal(body.hook_event_name, "SessionSwitch")
        assert.equal(body.session_id, switchedID)
        assert.equal(body.pid, String(process.pid))
      })
      await t.test("legacy launch without socket retains HTTP", async () => {
        reset()
        delete process.env.NOISEDECK_SESSION_HOOK_SOCKET
        await fire()
        assert.equal(unixRequests.length, 0)
        assert.equal(tcpRequests.length, 1)
        assertRequest(tcpRequests[0])
      })
      await t.test("retry keeps exact body and the original socket", async () => {
        reset()
        responder = (_request, response) => {
          delete process.env.NOISEDECK_SESSION_HOOK_SOCKET
          response.writeHead(unixRequests.length === 1 ? 503 : 204)
          response.end()
        }
        await fire()
        assert.equal(unixRequests.length, 2)
        assert.equal(unixRequests[0].body, unixRequests[1].body)
        unixRequests.forEach(assertRequest)
        assert.equal(tcpRequests.length, 0)
      })
      await t.test("4xx stops delivery without retry", async () => {
        reset()
        responder = (_request, response) => { response.writeHead(401); response.end() }
        await fire()
        assert.equal(unixRequests.length, 1)
        assert.equal(tcpRequests.length, 0)
      })
      await t.test("redirect never reaches its TCP destination", async () => {
        reset()
        responder = (_request, response) => { response.writeHead(307, { Location: url }); response.end() }
        await fire()
        assert.equal(unixRequests.length, 1)
        assert.equal(tcpRequests.length, 0)
      })
      await t.test("5xx attempts are finite", async () => {
        reset()
        responder = (_request, response) => { response.writeHead(503); response.end() }
        await fire()
        assert.equal(unixRequests.length, 3)
        assert.equal(new Set(unixRequests.map((request) => request.body)).size, 1)
        assert.equal(tcpRequests.length, 0)
      })
      await t.test("missing socket never falls back to HTTP", async () => {
        reset()
        process.env.NOISEDECK_SESSION_HOOK_SOCKET = join(root, "absent.sock")
        await fire()
        assert.equal(unixRequests.length, 0)
        assert.equal(tcpRequests.length, 0)
      })
      await t.test("stalled socket is aborted within delivery deadline", async () => {
        reset()
        responder = () => {}
        const started = performance.now()
        await fire()
        const elapsed = performance.now() - started
        assert.ok(elapsed >= 6500 && elapsed < 10000, `delivery took ${Math.round(elapsed)}ms`)
        assert.ok(unixRequests.length >= 2 && unixRequests.length <= 3)
        assert.equal(tcpRequests.length, 0)
      })
      await t.test("no launch token remains a quiet no-op", async () => {
        reset()
        delete process.env.NOISEDECK_SESSION_HOOK_TOKEN
        await fire()
        assert.equal(unixRequests.length, 0)
        assert.equal(tcpRequests.length, 0)
      })
    } finally {
      delete process.env.NOISEDECK_SESSION_HOOK_URL
      delete process.env.NOISEDECK_SESSION_HOOK_TOKEN
      delete process.env.NOISEDECK_SESSION_HOOK_SOCKET
      await Promise.all([close(unix), close(tcp)])
      await rm(root, { recursive: true, force: true })
    }
  })
}
