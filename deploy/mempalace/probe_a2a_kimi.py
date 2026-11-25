#!/usr/bin/env python3
"""Isolated probe: does Kimi Code's a2a Stop-hook receive path still work?

Runs the real interactive `kimi` CLI in a PTY against a throwaway HOME whose
config.toml carries the a2a lifecycle hooks (same shape configure.py
installs, pointed at a local wrapper that execs the synthetic mailbox), and
against a loopback dummy model server that records every request body.

The receive path under test: at turn end the Stop hook exits 2 with the wake
reason on stderr; Kimi Code must turn that into a block, append the reason to
the context and continue the turn, so the wake text reaches the model in a
later request with no user or terminal input.

PASS requires all of:
  (a) the mailbox recorded a `hook stop` invocation (the Stop hook ran);
  (b) the dummy model saw the wake text in a request AFTER the one that
      carried the original prompt (proof the exit-2 block reason reached the
      model automatically);
  (c) the kimi process exited cleanly afterwards.

The UserPromptSubmit hook is deliberately NOT installed: the synthetic
mailbox serves exactly one invitation and the `prompt` hook consumes (acks)
it at turn start, which would make the Stop path unobservable.  Kimi fires
UserPromptSubmit for interactive prompts too (origin kind "user"), so the
probe isolates the Stop-hook backstop, which is the suspected regression.

Usage: probe_a2a_kimi.py [--keep]     (run from any directory)
"""

from __future__ import annotations

import hashlib
import json
import os
import sys
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

from probe_a2a_receive import INVITE_BODY, Probe, free_port

WAKE = "[a2a#synthetic]"
PROMPT_MARKER = "PROBE-TURN-ONE"
PROMPT = f"Reply with exactly: {PROMPT_MARKER}"
REPLY = "READY"

# Same event/argument/timeout triples as configure.py:KIMI_A2A_HOOKS, minus
# UserPromptSubmit (see module docstring).
HOOK_BLOCKS = (("SessionStart", "session-start", 15), ("Stop", "stop", 15))

CONFIG = """\
# a2a receive probe: dummy loopback provider + lifecycle hooks
default_model = "probe/kimi-probe"

[providers.probe]
type = "anthropic"
api_key = "probe-dummy-key"
base_url = "http://127.0.0.1:{port}"

[models."probe/kimi-probe"]
provider = "probe"
model = "kimi-probe"
max_context_size = 131072
capabilities = ["tool_use"]

{hooks}
"""


class ModelLog:
    """Records every request the dummy model server receives."""

    def __init__(self, root: Path):
        self.dir = root / "modellog"
        self.dir.mkdir()
        self.lock = threading.Lock()
        self.requests: list[str] = []  # raw body text, in arrival order

    def record(self, path: str, body: bytes) -> None:
        text = body.decode("utf-8", "replace")
        with self.lock:
            self.requests.append(text)
            index = len(self.requests) - 1
        (self.dir / f"{index:03d}.json").write_text(
            json.dumps({"path": path, "body": text}, indent=1))

    def snapshot(self) -> list[str]:
        with self.lock:
            return list(self.requests)


def _sse(events: list[tuple[str, dict]]) -> bytes:
    out = []
    for name, data in events:
        out.append(f"event: {name}\n")
        out.append(f"data: {json.dumps(data)}\n\n")
    return "".join(out).encode()


class DummyModelHandler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *args) -> None:  # keep the probe output clean
        pass

    def _send(self, status: int, ctype: str, payload: bytes) -> None:
        self.send_response(status)
        self.send_header("Content-Type", ctype)
        self.send_header("Content-Length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)

    def _body(self) -> bytes:
        length = int(self.headers.get("Content-Length") or 0)
        return self.rfile.read(length) if length else b""

    def _parsed(self, body: bytes):
        try:
            return json.loads(body)
        except Exception:
            return {}

    def do_POST(self) -> None:  # noqa: N802 (http.server naming)
        body = self._body()
        self.server.log.record(self.path, body)
        data = self._parsed(body)
        stream = data.get("stream") is True
        if self.path.endswith("/v1/messages"):
            self._anthropic(stream)
        elif "chat/completions" in self.path:
            self._openai_chat(stream)
        elif "responses" in self.path:
            self._openai_responses(stream)
        else:  # unknown endpoint: acknowledge so the client surfaces its error
            self._send(200, "application/json", b'{"ok": true}')

    def do_GET(self) -> None:  # noqa: N802
        self.server.log.record(self.path, b"")
        self._send(200, "application/json",
                   json.dumps({"data": [], "object": "list"}).encode())

    def _anthropic(self, stream: bool) -> None:
        message = {"id": "msg_probe", "type": "message", "role": "assistant",
                   "model": "kimi-probe",
                   "content": [{"type": "text", "text": REPLY}],
                   "stop_reason": "end_turn", "stop_sequence": None,
                   "usage": {"input_tokens": 1, "output_tokens": 1}}
        if not stream:
            self._send(200, "application/json", json.dumps(message).encode())
            return
        self._send(200, "text/event-stream", _sse([
            ("message_start", {"type": "message_start", "message": message}),
            ("content_block_start", {"type": "content_block_start", "index": 0,
                                     "content_block": {"type": "text", "text": ""}}),
            ("content_block_delta", {"type": "content_block_delta", "index": 0,
                                     "delta": {"type": "text_delta", "text": REPLY}}),
            ("content_block_stop", {"type": "content_block_stop", "index": 0}),
            ("message_delta", {"type": "message_delta",
                               "delta": {"stop_reason": "end_turn", "stop_sequence": None},
                               "usage": {"output_tokens": 1}}),
            ("message_stop", {"type": "message_stop"}),
        ]))

    def _openai_chat(self, stream: bool) -> None:
        if not stream:
            completion = {"id": "chatcmpl-probe", "object": "chat.completion",
                          "created": int(time.time()), "model": "kimi-probe",
                          "choices": [{"index": 0,
                                       "message": {"role": "assistant", "content": REPLY},
                                       "finish_reason": "stop"}],
                          "usage": {"prompt_tokens": 1, "completion_tokens": 1,
                                    "total_tokens": 2}}
            self._send(200, "application/json", json.dumps(completion).encode())
            return
        chunk = {"id": "chatcmpl-probe", "object": "chat.completion.chunk",
                 "created": int(time.time()), "model": "kimi-probe"}
        events = [
            {**chunk, "choices": [{"index": 0, "delta": {"role": "assistant"},
                                   "finish_reason": None}]},
            {**chunk, "choices": [{"index": 0, "delta": {"content": REPLY},
                                   "finish_reason": None}]},
            {**chunk, "choices": [{"index": 0, "delta": {},
                                   "finish_reason": "stop"}]},
        ]
        payload = _sse([(str(i), e) for i, e in enumerate(events)])
        payload += b"data: [DONE]\n\n"
        self._send(200, "text/event-stream", payload)

    def _openai_responses(self, stream: bool) -> None:
        response = {"id": "resp_probe", "object": "response", "status": "completed",
                    "model": "kimi-probe",
                    "output": [{"type": "message", "id": "msg_probe",
                                "role": "assistant", "status": "completed",
                                "content": [{"type": "output_text", "text": REPLY}]}],
                    "usage": {"input_tokens": 1, "output_tokens": 1,
                              "total_tokens": 2}}
        if not stream:
            self._send(200, "application/json", json.dumps(response).encode())
            return
        events = [
            ("response.output_text.delta", {"type": "response.output_text.delta",
                                            "delta": REPLY}),
            ("response.completed", {"type": "response.completed", "response": response}),
        ]
        self._send(200, "text/event-stream", _sse(events))


class DummyModelServer(ThreadingHTTPServer):
    daemon_threads = True

    def __init__(self, log: ModelLog):
        super().__init__(("127.0.0.1", 0), DummyModelHandler)
        self.log = log


def _tail(path: Path, limit: int = 800) -> str:
    try:
        return path.read_text()[-limit:]
    except OSError:
        return "<missing>"


def _diagnostics(probe: Probe, log: ModelLog) -> str:
    calls = json.dumps(probe.calls(), indent=1)
    hookstdin = sorted(p.name for p in probe.root.glob("hookstdin-*.json"))
    requests = [f"{i:03d}: {body[:200]!r}" for i, body in enumerate(log.snapshot())]
    output = probe.root / "pty.log" if (probe.root / "pty.log").exists() else probe.root / "stdout"
    return (f"kimi output tail: {_tail(output)}\n"
            f"kimi stderr tail: {_tail(probe.root / 'stderr')}\n"
            f"mailbox calls: {calls}\n"
            f"hook stdin payloads: {hookstdin}\n"
            f"model requests ({len(requests)}): " + " | ".join(requests))


def run(probe: Probe) -> None:
    # 1. Loopback dummy model that records every request body.
    log = ModelLog(probe.root)
    server = DummyModelServer(log)
    threading.Thread(target=server.serve_forever, daemon=True).start()

    # 2. Hook wrapper: same argv contract as /opt/mempalace/hooks/a2a-hook.sh,
    #    but execs the synthetic mailbox. stdin passes through via exec.
    wrapper = probe.root / "a2a-hook.sh"
    wrapper.write_text(
        "#!/bin/sh\n"
        "set -eu\n"
        f'exec /usr/bin/python3 {probe.root}/mail.py hook "$1" --harness kimi\n'
    )
    wrapper.chmod(0o755)

    # 3. Temp HOME config: a2a hook blocks exactly like configure.py
    #    (event/command/timeout), plus a dummy anthropic-protocol provider.
    hooks = "\n\n".join(
        f'[[hooks]]\nevent = "{event}"\n'
        f'command = "{wrapper} {argument} kimi"\ntimeout = {timeout}'
        for event, argument, timeout in HOOK_BLOCKS
    )
    kimi_home = probe.root / "home" / ".kimi-code"
    kimi_home.mkdir(parents=True, exist_ok=True)
    (kimi_home / "config.toml").write_text(
        CONFIG.format(port=server.server_address[1], hooks=hooks))

    # 4. Seed the exact per-directory trust record the interactive CLI reads.
    #    This is provisioning, not an answer injected into its trust modal.
    cwd = probe.root / "cwd"
    trust = kimi_home / "workspace-trust" / (
        f"wd_{cwd.name}_{hashlib.sha256(str(cwd).encode()).hexdigest()[:12]}")
    trust.parent.mkdir(parents=True, exist_ok=True)
    trust.write_text(json.dumps({"root": str(cwd), "trustedAt": int(time.time() * 1000)}))

    # 5. Run the real interactive CLI. The proxy points at a closed loopback
    #    port so any external call (telemetry, update check) fails instantly;
    #    NO_PROXY keeps the dummy model server direct. Dummy key only.
    closed_port = free_port()
    env = probe.env({
        "HTTP_PROXY": f"http://127.0.0.1:{closed_port}",
        "HTTPS_PROXY": f"http://127.0.0.1:{closed_port}",
        "NO_PROXY": "127.0.0.1,localhost",
        "no_proxy": "127.0.0.1,localhost",
    })
    probe.spawn(["kimi"], env, cwd=cwd, pty=True)
    time.sleep(3)
    probe.write_pty(PROMPT)
    time.sleep(0.2)
    probe.write_pty("\r")

    try:
        deadline = time.monotonic() + 180
        while time.monotonic() < deadline:
            if probe.mark("hookstdin-stop.json") and any(
                    WAKE in body for body in log.snapshot()):
                break
            if probe.process.poll() is not None:
                raise AssertionError(
                    "kimi exited before the Stop-hook continuation reached the model; "
                    + _diagnostics(probe, log))
            time.sleep(0.2)
        else:
            raise AssertionError(
                "kimi never delivered the Stop-hook continuation; "
                + _diagnostics(probe, log))

        # The synthetic mailbox is now acknowledged, so the second turn can
        # finish without another block. Exit through Kimi's own TUI command.
        time.sleep(1)
        probe.write_pty("/exit")
        time.sleep(0.2)
        probe.write_pty("\r")
        deadline = time.monotonic() + 30
        while probe.process.poll() is None and time.monotonic() < deadline:
            time.sleep(0.2)
        if probe.process.poll() is None:
            raise AssertionError("kimi did not exit through its native /exit command")
        if probe.process.returncode != 0:
            raise AssertionError(
                f"kimi exited {probe.process.returncode}, not cleanly; "
                + _diagnostics(probe, log))
    finally:
        probe.stop()
        server.shutdown()

    # (a) the Stop hook must have run and carried Kimi's hook payload.
    if not any(call[:2] == ["hook", "stop"] for call in probe.calls()):
        raise AssertionError(
            "mailbox never recorded a `hook stop` call — the Stop hook did not "
            "run; " + _diagnostics(probe, log))

    # (b) the exit-2 block reason must reach the model in a LATER request.
    requests = log.snapshot()
    prompt_at = next((i for i, body in enumerate(requests) if PROMPT_MARKER in body), None)
    if prompt_at is None:
        raise AssertionError(
            f"dummy model never saw the prompt ({PROMPT_MARKER!r}); "
            + _diagnostics(probe, log))
    wake_at = next((i for i, body in enumerate(requests)
                    if WAKE in body and i > prompt_at), None)
    if wake_at is None:
        raise AssertionError(
            f"wake text {WAKE!r} never reached the model after request "
            f"{prompt_at} — the Stop-hook block reason was not written back "
            "into the context; " + _diagnostics(probe, log))

    print(f"PASS [kimi]: Stop hook blocked turn end, the wake line reached the "
          f"model automatically (request {prompt_at} -> {wake_at}), and the "
          f"CLI exited cleanly; invitation body {INVITE_BODY!r} delivered with "
          "no follow-up terminal input or provider credentials")


def main() -> None:
    keep = "--keep" in sys.argv[1:]
    probe = Probe("kimi", keep=keep)
    try:
        run(probe)
    except AssertionError as exc:
        probe.stop()
        probe.fail(str(exc))
    except Exception as exc:  # probe infrastructure failure is a failure
        probe.stop()
        probe.fail(f"{type(exc).__name__}: {exc}")
    finally:
        probe.cleanup()


if __name__ == "__main__":
    main()
