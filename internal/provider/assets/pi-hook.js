// noisedeck-provider-hook.js — the provisioned pi identity capture hook.
// Provisioning installs this file and Noisedeck injects it every launch via
// `pi --extension <path>` (Deck controls argv). pi's extension discovery
// scans only <cwd>/.pi/extensions/ and ~/.pi/agent/extensions/, and this
// file is installed at neither, so it loads only when Deck passes the flag.
// The launch gate (internal/provider/launchgate.go) requires the installed
// file to be byte-identical to provider.PiHookPlugin, so a differing file is
// backed up and replaced — leaving it would fail every launch at the gate.
//
// pi mints its session id eagerly at construction and fires session_start
// before the first user submission (the same timing omp's hook measured).
// The event payload carries no id; it is read from the hook context, then
// POSTed directly to the per-launch durable capture endpoint.
//
// NOISEDECK_SESSION_HOOK_URL/TOKEN come from the inherited launch environment.
// Without them, a launch has no capture grant and is a quiet no-op.
// Failures never throw or change provider runtime state.

import { request as httpRequest } from "node:http"
import { randomUUID } from "node:crypto"

// A configured private socket is the only transport for this launch. Its
// failure must not disclose a capture token or payload to a TCP listener.
const post = (url, options, socketPath) => {
  if (!socketPath) return fetch(url, options)
  return new Promise((resolve, reject) => {
    const target = new URL(url)
    const request = httpRequest({
      socketPath,
      path: target.pathname + target.search,
      method: options.method,
      headers: options.headers,
      signal: options.signal,
      agent: false,
    }, (response) => {
      const status = response.statusCode ?? 0
      response.destroy()
      resolve({ status })
    })
    request.on("error", reject)
    request.end(options.body)
  })
}

const DELIVERY_DEADLINE_MS = 8000
const ATTEMPT_TIMEOUT_MS = 2500
const MAX_DELIVERY_ATTEMPTS = 3

async function deliver(url, token, body) {
  const socketPath = process.env.NOISEDECK_SESSION_HOOK_SOCKET
  const deadline = Date.now() + DELIVERY_DEADLINE_MS
  for (let attempt = 0; attempt < MAX_DELIVERY_ATTEMPTS; attempt += 1) {
    const remaining = deadline - Date.now()
    if (remaining <= 0) return
    try {
      const response = await post(url, {
        method: "POST",
        headers: {
          "Content-Type": "application/json",
          "X-Noisedeck-Hook-Token": token,
        },
        body,
        redirect: "manual",
        signal: AbortSignal.timeout(Math.min(ATTEMPT_TIMEOUT_MS, remaining)),
      }, socketPath)
      await response.body?.cancel().catch(() => {})
      if (response.status >= 200 && response.status < 300) return
      if (response.status >= 400 && response.status < 500) return
      if (response.status < 500 || response.status >= 600) return
    } catch {
      // Network failures retry within the same bounded delivery window.
    }
    const pause = Math.min(500 * (attempt + 1), deadline - Date.now())
    if (pause > 0) await new Promise((resolve) => setTimeout(resolve, pause))
  }
}

export default function (pi) {
  let sequence = 0
  let turn = randomUUID()
  let phase = "unknown"
  let prompts = 0
  let endedSequence = 0
  let endedTurn = ""
  let pending = null
  let delivering = false

  const publish = (ctx) => {
    const base = process.env.NOISEDECK_SESSION_HOOK_URL
    const token = process.env.NOISEDECK_SESSION_HOOK_TOKEN
    const nativeId = ctx?.sessionManager?.getSessionId?.()
    if (!base || !token || !nativeId) return
    let url
    try { url = new URL("/api/hooks/conversation", base).href } catch { return }
    // The process-monotonic clock survives extension reloads. A counter
    // initialized at zero would leave every new event behind the old stream.
    sequence = Math.max(sequence + 1, Number(process.hrtime.bigint() / 1000n))
    if (phase === "ended" && turn !== endedTurn) { endedSequence = sequence; endedTurn = turn }
    pending = { url, token, body: JSON.stringify({
      native_id: nativeId, pid: String(process.pid), sequence,
      turn_id: turn, state: prompts > 0 ? "needs_input" : phase,
      ended_sequence: endedSequence, ended_turn: endedTurn,
    }) }
    if (delivering) return
    delivering = true
    void (async () => {
      try {
        while (pending) {
          const item = pending
          pending = null
          await deliver(item.url, item.token, item.body)
        }
      } finally { delivering = false }
    })().catch(() => {})
  }
  pi.on("agent_start", (_event, ctx) => {
    turn = randomUUID()
    phase = "working"
    publish(ctx)
  })
  // agent_end can precede queued continuations. Only agent_settled promises
  // that no automatic retry, compaction or queued continuation will run.
  pi.on("agent_settled", (event, ctx) => {
    phase = event.aborted ? "unknown" : "ended"
    publish(ctx)
  })
  pi.on("ui_prompt_start", (_event, ctx) => { prompts += 1; publish(ctx) })
  pi.on("ui_prompt_end", (_event, ctx) => { prompts = Math.max(0, prompts - 1); publish(ctx) })

  pi.on("session_start", async (event, ctx) => {
    try {
      // session_start carries no id in its payload; the id is reached
      // through the hook context. It is already populated when the event
      // fires at launch, before first submission.
      const sessionId = ctx?.sessionManager?.getSessionId?.()
      if (!sessionId) return
      const url = process.env.NOISEDECK_SESSION_HOOK_URL
      const token = process.env.NOISEDECK_SESSION_HOOK_TOKEN
      if (!url || !token) return
      const cwd = ctx?.sessionManager?.getCwd?.() ?? ""
      const body = JSON.stringify({
        hook_event_name: "SessionStart",
        session_id: sessionId,
        cwd,
        source: event.reason ?? "startup",
        pid: String(process.pid),
      })
      await deliver(url, token, body)
      if (event.reason && event.reason !== "startup") publish(ctx)
    } catch {
      // A failed capture must not break the CLI. Its honest signal is the
      // pending identity expiring for this exact launch generation.
    }
  })
}
