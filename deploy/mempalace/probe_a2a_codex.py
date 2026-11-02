#!/usr/bin/env python3
"""Prove (or disprove) Codex's a2a native-queue receive path.

Isolation: temp HOME (with its own hooks.json/config.toml and a loopback dummy
model provider speaking the OpenAI Responses wire API), a private tmux socket
inside the probe root, and the synthetic mailbox from probe_a2a_receive.
Nothing touches /home/agent/.codex, the real palace, the default tmux socket,
or any real credential.

Codex 0.162.1 realities this probe encodes:
  * wire_api = "chat" is rejected; only "responses" is supported.
  * hooks.json entries do not run until Codex's "Hooks need review" modal is
    answered ("Trust all and continue"), which seeds [hooks.state] blocks in
    config.toml. --dangerously-bypass-hook-trust is NOT an alternative: it
    forces embedded mode (no shared app-server daemon).
  * The SessionStart hook fires when the session's FIRST turn starts
    (payload source: "startup"), not when the TUI process launches. So the
    probe bootstraps the session with one user prompt before going
    hands-off.

PASS needs all three:
  (a) the SessionStart hook fired and its payload carried a native thread id;
  (b) `codex queue --thread <id> --message ...` exited 0 from outside;
  (c) the dummy model server saw a streaming request whose body contains the
      queued text — the queued message started a turn automatically, with no
      terminal input after the bootstrap prompt.
"""

from __future__ import annotations

import json
import os
import subprocess
import sys
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from probe_a2a_receive import INVITE_BODY, Probe, free_port  # noqa: E402

QUEUE_TEXT = f"[a2a#synthetic] fixture says (chat-fixture): {INVITE_BODY}"


# ── Dummy model provider: loopback OpenAI Responses API, always streams ──────


def start_model_server(probe: Probe) -> tuple[ThreadingHTTPServer, Path]:
    logdir = probe.root / "modellog"
    logdir.mkdir(exist_ok=True)
    lock = threading.Lock()
    seq = [0]

    class Handler(BaseHTTPRequestHandler):
        def log_message(self, *args):  # silence stderr noise
            pass

        def do_POST(self):
            length = int(self.headers.get("Content-Length", 0))
            body = self.rfile.read(length)
            with lock:
                seq[0] += 1
                (logdir / f"{seq[0]:03d}.json").write_bytes(body)
            message_item = {
                "type": "message", "id": "msg_probe", "role": "assistant",
                "status": "completed",
                "content": [{"type": "output_text", "text": "ok",
                             "annotations": []}],
            }
            response = {
                "id": "resp_probe", "object": "response",
                "status": "completed", "model": "probe-model",
                "output": [message_item],
                "usage": {"input_tokens": 1, "input_tokens_details": [],
                          "output_tokens": 1, "output_tokens_details": [],
                          "total_tokens": 2},
                "created_at": 0,
            }
            stream = b'"stream":true' in body.replace(b" ", b"")
            if stream:
                self.send_response(200)
                self.send_header("Content-Type", "text/event-stream")
                self.end_headers()
                seq_n = 0

                def send(obj: dict) -> None:
                    nonlocal seq_n
                    obj["sequence_number"] = seq_n
                    seq_n += 1
                    self.wfile.write(f"data: {json.dumps(obj)}\n\n".encode())

                send({"type": "response.created", "response": response})
                send({"type": "response.in_progress", "response": response})
                send({"type": "response.output_item.added",
                      "output_index": 0, "item": message_item})
                send({"type": "response.output_text.delta",
                      "item_id": "msg_probe", "output_index": 0,
                      "content_index": 0, "delta": "ok"})
                send({"type": "response.output_item.done",
                      "output_index": 0, "item": message_item})
                send({"type": "response.completed", "response": response})
                self.wfile.write(b"data: [DONE]\n\n")
            else:
                blob = json.dumps(response).encode()
                self.send_response(200)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(blob)))
                self.end_headers()
                self.wfile.write(blob)

    server = ThreadingHTTPServer(("127.0.0.1", free_port()), Handler)
    threading.Thread(target=server.serve_forever, daemon=True).start()
    return server, logdir


# ── tmux / codex helpers (PRIVATE socket inside the probe root) ───────────────


def tmux(probe: Probe, socket: Path, *args: str,
         capture: Path | None = None, timeout: float = 30) -> subprocess.CompletedProcess:
    # -f /dev/null keeps the server from reading the caller's tmux
    # configuration; env= keeps it from inheriting ambient HOME, XDG or
    # CODEX_* values that could redirect it off the probe root.
    argv = ["tmux", "-f", "/dev/null", "-S", str(socket), *args]
    stdout = capture.open("w") if capture else subprocess.PIPE
    return subprocess.run(argv, stdout=stdout, stderr=subprocess.DEVNULL,
                          text=not capture, timeout=timeout,
                          env={"PATH": os.environ.get("PATH", "/usr/bin:/bin"), "HOME": str(probe.root / "home"),
                               "TMPDIR": str(probe.root)})


def pane_text(probe: Probe, socket: Path) -> str:
    out = probe.root / "tmux-capture.txt"
    tmux(probe, socket, "capture-pane", "-p", capture=out)
    return out.read_text()


def launch_tui(probe: Probe, socket: Path, env: dict[str, str], cwd: Path) -> None:
    tmux(probe, socket,
         "new-session", "-d",
         "-x", "200", "-y", "50",
         "-e", f"HOME={env['HOME']}",
         "-e", f"PATH={env['PATH']}",
         "-e", "TERM=xterm-256color",
         "-e", "PROBE_API_KEY=probe-dummy-key",
         "-e", f"TMPDIR={env.get('TMPDIR', '/tmp')}",
         "-c", str(cwd),
         "codex", "--dangerously-bypass-approvals-and-sandbox",
         timeout=30)


def kill_daemons(probe: Probe) -> None:
    """Stop only this synthetic HOME's daemon through Codex's native command."""
    try:
        result = subprocess.run(
            ["codex", "app-server", "daemon", "stop"],
            env=probe.env({"PROBE_API_KEY": "probe-dummy-key"}),
            stdout=subprocess.DEVNULL, stderr=subprocess.PIPE, text=True, timeout=30,
        )
    except (OSError, subprocess.TimeoutExpired) as error:
        probe.fail(f"setup: stopping the synthetic Codex daemon failed: {error}")
        return
    if result.returncode:
        probe.fail("setup: stopping the synthetic Codex daemon failed: "
                   + result.stderr.strip())


# ── Probe ────────────────────────────────────────────────────────────────────


def probe_codex(keep: bool = False) -> None:
    probe = Probe("codex", keep=keep)
    server = None
    socket = probe.root / "tmux.sock"
    try:
        server, logdir = start_model_server(probe)
        port = server.server_address[1]
        home = probe.root / "home"
        cwd = probe.root / "cwd"

        # hooks.json — same shape configure_codex writes, but the command
        # points at a local wrapper execing the synthetic mailbox.
        wrapper = probe.root / "a2a-hook.sh"
        wrapper.write_text(
            "#!/bin/sh\n"
            "set -eu\n"
            f'exec /usr/bin/python3 "{probe.root}/mail.py" hook "${{1:?}}" '
            "--harness codex\n"
        )
        wrapper.chmod(0o755)
        hooks: dict = {"hooks": {}}
        for event, argument in (("SessionStart", "session-start"), ("Stop", "stop")):
            hooks["hooks"].setdefault(event, []).append(
                {
                    "matcher": "*",
                    "hooks": [{"type": "command",
                               "command": f"{wrapper} {argument}",
                               "timeout": 30}],
                }
            )
        codex_dir = home / ".codex"
        codex_dir.mkdir(parents=True, exist_ok=True)
        (codex_dir / "hooks.json").write_text(json.dumps(hooks, indent=2))

        # config.toml — dummy loopback provider (Responses wire API; codex
        # 0.162.1 dropped wire_api = "chat"), dummy key, pre-trusted cwd.
        (codex_dir / "config.toml").write_text(
            f'model = "probe-model"\n'
            f'model_provider = "probe"\n'
            f'sandbox_mode = "workspace-write"\n'
            f'\n'
            f'[model_providers.probe]\n'
            f'name = "probe"\n'
            f'base_url = "http://127.0.0.1:{port}/v1"\n'
            f'wire_api = "responses"\n'
            f'env_key = "PROBE_API_KEY"\n'
            f'\n'
            f'[projects."{cwd}"]\n'
            f'trust_level = "trusted"\n'
        )
        env = probe.env({"PROBE_API_KEY": "probe-dummy-key"})

        # Phase 1 — trust the hooks: launch, answer the "Hooks need review"
        # modal ("Trust all and continue"), wait for [hooks.state] in
        # config.toml, then tear the TUI and daemon down again.
        launch_tui(probe, socket, env, cwd)
        deadline = time.monotonic() + 60
        while time.monotonic() < deadline:
            if "Hooks need review" in pane_text(probe, socket):
                break
            time.sleep(0.5)
        else:
            probe.fail("setup: hook trust modal never appeared")
        # Option 2 "Trust all and continue" is one Down from the default.
        tmux(probe, socket, "send-keys", "Down")
        time.sleep(0.3)
        tmux(probe, socket, "send-keys", "Enter")
        config_toml = codex_dir / "config.toml"
        deadline = time.monotonic() + 30
        while time.monotonic() < deadline:
            if "hooks.state" in config_toml.read_text():
                break
            time.sleep(0.5)
        else:
            probe.fail("setup: trusting the modal never seeded hooks.state")
        tmux(probe, socket, "kill-server")
        kill_daemons(probe)
        time.sleep(1)

        # Phase 2 — the real probe. Fresh TUI (hooks now trusted), one
        # bootstrap prompt so the session starts its first turn, then the
        # a2a queue rung from outside with zero further terminal input.
        for stale in ("calls", "hookstdin-session-start.json",
                      "hookstdin-stop.json"):
            path = probe.root / stale
            path.unlink(missing_ok=True)
        launch_tui(probe, socket, env, cwd)

        hook_file = probe.root / "hookstdin-session-start.json"

        def hook_fired() -> bool:
            return hook_file.exists()

        # Bootstrap: a user's first message starts the session's first
        # turn — that is when Codex fires SessionStart (source: "startup").
        deadline = time.monotonic() + 60
        while time.monotonic() < deadline and "Ask Codex" not in pane_text(
                probe, socket):
            time.sleep(0.5)
        tmux(probe, socket, "send-keys", "say ok")
        time.sleep(0.5)
        tmux(probe, socket, "send-keys", "Enter")

        deadline = time.monotonic() + 120
        while time.monotonic() < deadline and not hook_fired():
            time.sleep(0.5)
        if not hook_fired():
            probe.fail("criterion (a): SessionStart hook never fired after "
                       "the session's first turn")
        payload = json.loads(hook_file.read_text())
        thread_id = payload.get("session_id") or payload.get("thread_id")
        if not thread_id:
            probe.fail(f"criterion (a): hook payload has no session_id/"
                       f"thread_id: {json.dumps(payload)[:400]}")
        # Wait for the bootstrap turn to finish: model-log quiescence.
        # (The Stop hook usually fires ~0.5s after turn completion, but in
        # 3 of 13 observed runs it never fired within ~25s, so quiescence
        # is the reliable turn-end signal here.)
        deadline = time.monotonic() + 120
        while time.monotonic() < deadline:
            files = list(logdir.glob("*.json"))
            if files:
                time.sleep(6)
                if len(list(logdir.glob("*.json"))) == len(files):
                    break
            time.sleep(0.5)
        else:
            probe.fail("setup: bootstrap turn never reached quiescence")

        # (b) queue a message from a separate process with the same HOME.
        queue = subprocess.run(
            ["codex", "queue", "--thread", str(thread_id),
             "--message", QUEUE_TEXT],
            env=env, cwd=str(cwd), capture_output=True, text=True, timeout=60,
        )
        if queue.returncode != 0:
            probe.fail(
                f"criterion (b): codex queue exit {queue.returncode}: "
                f"{queue.stdout[-400:]} {queue.stderr[-400:]}")

        # (c) the queued message started a turn with no terminal input: the
        # dummy model server saw a request body containing the queued text.
        def delivered() -> bool:
            return any(QUEUE_TEXT.encode() in p.read_bytes()
                       for p in sorted(logdir.glob("*.json")))

        deadline = time.monotonic() + 180
        while time.monotonic() < deadline and not delivered():
            time.sleep(0.5)
        if not delivered():
            probe.fail("criterion (c): queued message never reached the "
                       "model — the receive path is BROKEN; see "
                       "tmux-capture.txt and modellog/ under the root")

        print(f"PASS [codex]: SessionStart hook fired (session_id="
              f"{thread_id}, source={payload.get('source')}), codex queue "
              f"exit 0, and the dummy model received the queued message "
              f"automatically — no terminal input after the bootstrap prompt")
    finally:
        subprocess.run(["tmux", "-S", str(socket), "kill-server"],
                       check=False, timeout=15,
                       stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        primary_error = sys.exc_info()[1]
        try:
            kill_daemons(probe)
        except BaseException as error:
            if primary_error is None:
                raise
            print(f"Additional daemon cleanup failure: {error}", file=sys.stderr)
        finally:
            try:
                if server:
                    server.shutdown()
            finally:
                try:
                    probe.stop()
                finally:
                    if not keep:
                        probe.cleanup()


def main() -> None:
    keep = "--keep" in sys.argv
    probe_codex(keep=keep)


if __name__ == "__main__":
    main()
