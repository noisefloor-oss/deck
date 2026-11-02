#!/usr/bin/env python3
"""Isolated probe: do Claude Code's a2a receive paths still work?

Runs the real `claude` CLI against a loopback Anthropic Messages server and a
synthetic a2a mailbox. Two print-mode phases cover the hook paths: the Stop
hook blocks the first stop with a wake reason; the probe proves the reason
reached the model automatically when a later model request body contains the
wake text `[a2a#synthetic]`. A third phase drives the real TUI in a private
tmux socket and posts to the session's cross-session inbox socket the way the
courier's claude-socket rung does, proving an idle session starts a turn with
no terminal input.
"""

from __future__ import annotations

import importlib.machinery
import importlib.util
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


def make_model_server(probe: Probe) -> ThreadingHTTPServer:
    modellog = probe.root / "modellog"
    modellog.mkdir(exist_ok=True)
    counter = [0]
    lock = threading.Lock()

    class Handler(BaseHTTPRequestHandler):

        def log_message(self, *a):  # silence
            pass

        def do_POST(self):
            length = int(self.headers.get("Content-Length") or 0)
            body = self.rfile.read(length) if length else b""
            with lock:
                counter[0] += 1
                n = counter[0]
                (modellog / f"{n}.json").write_bytes(body)
            wants_stream = False
            try:
                wants_stream = json.loads(body or b"{}").get("stream", False)
            except json.JSONDecodeError:
                pass
            if "/v1/messages" in self.path:
                events = [
                    ("message_start", {"type": "message", "role": "assistant",
                                       "model": "probe-loopback", "content": [],
                                       "stop_reason": None, "stop_sequence": None,
                                       "usage": {"input_tokens": 1, "output_tokens": 0}}),
                    ("content_block_start", {"type": "thinking", "thinking": "", "signature": "probe-sig"}),
                    ("content_block_delta", {"type": "thinking_delta", "thinking": "."}),
                    ("content_block_delta", {"type": "signature_delta", "signature": "probe-sig"}),
                    ("content_block_stop", {}),
                    ("content_block_start", {"type": "text", "text": ""}),
                    ("content_block_delta", {"type": "text_delta", "text": "ok"}),
                    ("content_block_stop", {}),
                    ("message_delta", {"stop_reason": "end_turn", "stop_sequence": None,
                                       "usage": {"output_tokens": 1}}),
                    ("message_stop", {}),
                ]
                if not wants_stream:
                    reply = {
                        "id": "msg_probe", "type": "message", "role": "assistant",
                        "model": "probe-loopback",
                        "content": [
                            {"type": "thinking", "thinking": ".", "signature": "probe-sig"},
                            {"type": "text", "text": "ok"},
                        ],
                        "stop_reason": "end_turn", "stop_sequence": None,
                        "usage": {"input_tokens": 1, "output_tokens": 1,
                                  "cache_creation_input_tokens": 0,
                                  "cache_read_input_tokens": 0},
                    }
                    payload = json.dumps(reply).encode()
                    self.send_response(200)
                    self.send_header("Content-Type", "application/json")
                    self.send_header("Content-Length", str(len(payload)))
                    self.end_headers()
                    self.wfile.write(payload)
                    return
                chunks = [
                    f"event: {name}\ndata: {json.dumps({'type': name, **data})}\n\n".encode()
                    for name, data in events
                ]
                self.send_response(200)
                self.send_header("Content-Type", "text/event-stream")
                self.send_header("Cache-Control", "no-cache")
                self.end_headers()
                for chunk in chunks:
                    self.wfile.write(chunk)
            else:
                self.send_response(404)
                self.send_header("Content-Length", "0")
                self.end_headers()

        def do_GET(self):
            self.send_response(404)
            self.send_header("Content-Length", "0")
            self.end_headers()

    class QuietServer(ThreadingHTTPServer):
        def handle_error(self, request, client_address):
            pass  # connection resets from the CLI are expected

    server = QuietServer(("127.0.0.1", free_port()), Handler)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    return server


def write_claude_home(probe: Probe, extra_hooks: list[tuple[str, str, int]] | None = None) -> None:
    claude_dir = probe.root / "home" / ".claude"
    claude_dir.mkdir(parents=True, exist_ok=True)

    wrapper = probe.root / "a2a-hook.sh"
    wrapper.write_text(
        "#!/bin/sh\n"
        f'exec /usr/bin/python3 "{probe.root}/mail.py" hook "$1" --harness claude\n'
    )
    wrapper.chmod(0o755)

    def add_hook(document: dict, event: str, command: str, timeout: int) -> None:
        hooks = document.setdefault("hooks", {}).setdefault(event, [])
        hooks.append(
            {
                "matcher": "*",
                "hooks": [{"type": "command", "command": command, "timeout": timeout}],
            }
        )

    settings = {
        "env": {},
        "includeCoAuthoredBy": False,
        "autoUpdates": False,
    }
    hook = str(wrapper)
    add_hook(settings, "SessionStart", f"{hook} session-start claude", 15)
    add_hook(settings, "UserPromptSubmit", f"{hook} prompt claude", 10)
    add_hook(settings, "Stop", f"{hook} stop claude", 15)
    for event, command, timeout in extra_hooks or []:
        add_hook(settings, event, command, timeout)
    add_hook(settings, "SessionEnd", f"{hook} session-end claude", 10)
    (claude_dir / "settings.json").write_text(json.dumps(settings, indent=2))

    # Trust the working directory headlessly; skip onboarding nags.
    (probe.root / "home" / ".claude.json").write_text(
        json.dumps(
            {
                "hasCompletedOnboarding": True,
                "bypassPermissionsModeAccepted": True,
                "projects": {str(probe.root / "cwd"): {"allowedTools": [], "hasTrustDialogAccepted": True}},
            }
        )
    )


def run(probe: Probe, argv: list[str], env: dict[str, str], timeout: float) -> int:
    import subprocess

    proc = probe.spawn(argv, env, cwd=probe.root / "cwd")
    try:
        return proc.wait(timeout=timeout)
    except subprocess.TimeoutExpired:
        probe.stop()
        return -1
    finally:
        probe.stop()


def probe_claude() -> None:
    probe = Probe("claude")
    try:
        server = make_model_server(probe)
        write_claude_home(probe)

        env = probe.env(
            {
                "ANTHROPIC_BASE_URL": f"http://127.0.0.1:{server.server_address[1]}",
                "ANTHROPIC_API_KEY": "dummy",
                "ANTHROPIC_AUTH_TOKEN": "dummy",
                "ANTHROPIC_MODEL": "claude-sonnet-4-5",
                "CLAUDE_CODE_DISABLE_UNKNOWN_MODEL_WINDOW_ENFORCEMENT": "1",
                "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1",
                "DISABLE_TELEMETRY": "1",
                "DISABLE_AUTOUPDATER": "1",
                "DISABLE_BUG_COMMAND": "1",
                "DISABLE_COST_WARNINGS": "1",
                "DISABLE_ERROR_REPORTING": "1",
                "CLAUDE_CONFIG_DIR": str(probe.root / "home" / ".claude"),
            }
        )
        argv = [
            "claude", "-p", "reply with the single word ok",
            "--output-format", "text",
            "--dangerously-skip-permissions",
        ]
        rc = run(probe, argv, env, timeout=180)

        calls = probe.calls()
        hook_events = [c[1] for c in calls if c[:1] == ["hook"]]
        modellog = probe.root / "modellog"
        nreq = len(list(modellog.glob("*.json"))) if modellog.exists() else 0
        wake_seen = False
        if modellog.exists():
            for f in sorted(modellog.glob("*.json")):
                try:
                    body = json.loads(f.read_text())
                except (json.JSONDecodeError, OSError):
                    continue
                text = json.dumps(body.get("messages", []))
                if "[a2a#synthetic]" in text:
                    wake_seen = True
                    break

        problems = []
        if "stop" not in hook_events:
            problems.append(f"criterion (a) failed: no `hook stop` call recorded; hook events={hook_events}")
        if nreq < 2:
            problems.append(f"criterion (b) failed: model server saw only {nreq} request(s)")
        elif not wake_seen:
            problems.append(
                f"criterion (b) failed: wake text [a2a#synthetic] never reached the model "
                f"({nreq} requests)"
            )
        if rc == -1 or probe.process.poll() is None:
            problems.append("criterion (c) failed: claude process never exited (timeout)")
        if problems:
            stderr = (probe.root / "stderr").read_text(errors="replace") if (probe.root / "stderr").exists() else ""
            probe.fail("; ".join(problems) + f"\n  claude rc={rc}\n  stderr tail: {stderr[-2000:]}")
        print(
            f"PASS [claude]: stop hook fired, wake reason reached the model "
            f"({nreq} model requests), claude exited rc={rc}"
        )
    finally:
        try:
            server.shutdown()
        except Exception:
            pass
        probe.cleanup()

# ── Follow-up: does UserPromptSubmit STDOUT reach the model context? ────────

UPS_MARKER = "[a2a#probe] user-prompt-submit stdout marker"


def probe_ups_stdout() -> None:
    """Register a UserPromptSubmit hook echoing a marker; find it in request 1."""
    probe = Probe("claude-ups")
    try:
        server = make_model_server(probe)
        write_claude_home(
            probe,
            extra_hooks=[
                ("UserPromptSubmit", f"printf '%s\\n' '{UPS_MARKER}'", 10),
            ],
        )
        env = probe.env(
            {
                "ANTHROPIC_BASE_URL": f"http://127.0.0.1:{server.server_address[1]}",
                "ANTHROPIC_API_KEY": "dummy",
                "ANTHROPIC_AUTH_TOKEN": "dummy",
                "ANTHROPIC_MODEL": "claude-sonnet-4-5",
                "CLAUDE_CODE_DISABLE_UNKNOWN_MODEL_WINDOW_ENFORCEMENT": "1",
                "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1",
                "DISABLE_TELEMETRY": "1",
                "DISABLE_AUTOUPDATER": "1",
                "DISABLE_ERROR_REPORTING": "1",
                "CLAUDE_CONFIG_DIR": str(probe.root / "home" / ".claude"),
            }
        )
        rc = run(probe, ["claude", "-p", "reply ok", "--output-format", "text"], env, timeout=180)
        first = probe.root / "modellog" / "1.json"
        if not first.exists():
            probe.fail(f"no model request recorded (claude rc={rc})")
        request = json.loads(first.read_text())

        def locate(obj, path="$"):
            hits = []
            if isinstance(obj, dict):
                for key, value in obj.items():
                    hits += locate(value, f"{path}.{key}")
            elif isinstance(obj, list):
                for i, value in enumerate(obj):
                    hits += locate(value, f"{path}[{i}]")
            elif isinstance(obj, str) and UPS_MARKER in obj:
                hits.append((path, obj))
            return hits

        hits = locate(request)
        in_messages = [h for h in hits if h[0].startswith("$.messages")]
        if not in_messages:
            probe.fail(
                "UserPromptSubmit stdout never reached the first model request's user turn; "
                "a2a's claude prompt-hook delivery depends on it"
            )
        path, text = hits[0]
        idx = text.find(UPS_MARKER)
        snippet = text[max(0, idx - 120): idx + len(UPS_MARKER) + 120]
        print(
            f"PASS [claude-ups]: UserPromptSubmit stdout is context in the FIRST model "
            f"request (path={path})\n  snippet: ...{json.dumps(snippet)}..."
        )
    finally:
        try:
            server.shutdown()
        except Exception:
            pass
        probe.cleanup()


# ── Native receive: the courier's claude-socket rung wakes an idle TUI ──────

SOCKET_TEXT = f"[a2a#synthetic] fixture says (chat-fixture): {INVITE_BODY} over the inbox socket"


def load_real_client():
    """The standalone a2a client under test: A2A_PROBE_CLIENT when set,
    then the image's installed /usr/local/bin/a2a. The installed client has
    no .py extension, so the loader is named explicitly: a bare
    spec_from_file_location returns None for it and the import dies before
    the socket check."""
    candidates = [Path("/usr/local/bin/a2a")]
    override = os.environ.get("A2A_PROBE_CLIENT")
    if override:
        candidates.insert(0, Path(override))
    for candidate in candidates:
        if candidate.exists():
            loader = importlib.machinery.SourceFileLoader("a2a_cli", str(candidate))
            spec = importlib.util.spec_from_file_location("a2a_cli", candidate, loader=loader)
            module = importlib.util.module_from_spec(spec)
            spec.loader.exec_module(module)
            return module
    raise SystemExit("FAIL [claude-socket]: no a2a client found at " + ", ".join(str(c) for c in candidates))


def probe_socket(keep: bool = False) -> None:
    """Drive the real TUI in a private tmux socket: announce the inbox socket
    at SessionStart, go idle after a bootstrap turn, then deliver through the
    real rung with zero further terminal input."""
    probe = Probe("claude-socket", keep=keep)
    server = make_model_server(probe)
    # A Unix socket path is bounded (sun_path is 108 bytes): under a deep
    # TMPDIR the probe root is already too long, so the tmux socket lives at
    # a short fixed location while everything else stays under the root.
    tsock = Path("/tmp") / f"a2a-claude-probe-{os.getpid()}.sock"

    def tmux(*args: str, capture: Path | None = None) -> subprocess.CompletedProcess:
        out = capture.open("w") if capture else subprocess.DEVNULL
        return subprocess.run(["tmux", "-f", "/dev/null", "-S", str(tsock), *args],
                              stdout=out, stderr=subprocess.DEVNULL, timeout=30, check=False)

    def pane_tail() -> str:
        capture = probe.root / "pane.txt"
        tmux("capture-pane", "-p", capture=capture)
        return "\n  " + capture.read_text(errors="replace")[-1200:]

    try:
        write_claude_home(probe)
        home = probe.root / "home"
        # With CLAUDE_CONFIG_DIR set, Claude keeps its state file inside it:
        # the trust record must be seeded there, not at $HOME/.claude.json.
        (home / ".claude" / ".claude.json").write_text((home / ".claude.json").read_text())
        settings_path = home / ".claude" / "settings.json"
        settings = json.loads(settings_path.read_text())
        settings["crossSessionInbound"] = "accept"
        settings_path.write_text(json.dumps(settings, indent=2))
        # The hook wrapper records the socket the session exports, the same
        # announcement the real hook makes into presence.
        wrapper = probe.root / "a2a-hook.sh"
        wrapper.write_text(
            "#!/bin/sh\n"
            'if [ "${1:-}" = "session-start" ]; then\n'
            f'  printf \'%s\' "${{CLAUDE_CODE_MESSAGING_SOCKET:-}}" > "{probe.root}/socket-path"\n'
            "fi\n"
            f'exec /usr/bin/python3 "{probe.root}/mail.py" hook "$1" --harness claude\n'
        )
        wrapper.chmod(0o755)

        port = server.server_address[1]
        pane_env = {
            "HOME": str(home),
            "PATH": os.environ.get("PATH", "/usr/bin:/bin"),
            "TERM": "xterm-256color",
            "TMPDIR": str(probe.root),
            "ANTHROPIC_BASE_URL": f"http://127.0.0.1:{port}",
            # ANTHROPIC_API_KEY triggers a key-approval modal in the TUI; the
            # token form does not, and the loopback model ignores both.
            "ANTHROPIC_AUTH_TOKEN": "dummy",
            "ANTHROPIC_MODEL": "claude-sonnet-4-5",
            "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1",
            "DISABLE_TELEMETRY": "1",
            "DISABLE_AUTOUPDATER": "1",
            "CLAUDE_CONFIG_DIR": str(home / ".claude"),
        }
        argv = ["new-session", "-d", "-x", "200", "-y", "50"]
        for key, value in pane_env.items():
            argv += ["-e", f"{key}={value}"]
        argv += ["-c", str(probe.root / "cwd"), "claude", "--dangerously-skip-permissions"]
        tmux(*argv)

        sock_file = probe.root / "socket-path"
        deadline = time.monotonic() + 90
        while time.monotonic() < deadline and not sock_file.exists():
            time.sleep(0.5)
        if not sock_file.exists() or not sock_file.read_text().strip():
            probe.fail("criterion (a): SessionStart hook never announced an inbox socket" + pane_tail())
        inbox = sock_file.read_text().strip()

        # The bootstrap turn stands in for a user opening the session;
        # everything after it must happen with no terminal input at all.
        time.sleep(3)
        tmux("send-keys", "say ok")
        time.sleep(0.5)
        tmux("send-keys", "Enter")
        logdir = probe.root / "modellog"
        deadline = time.monotonic() + 150
        while time.monotonic() < deadline:
            files = list(logdir.glob("*.json"))
            if files:
                time.sleep(6)
                if len(list(logdir.glob("*.json"))) == len(files):
                    break
            time.sleep(0.5)
        bootstrap = len(list(logdir.glob("*.json")))
        if bootstrap == 0:
            probe.fail("setup: the bootstrap turn never reached the model" + pane_tail())

        a2a = load_real_client()
        outcome = a2a._claude_socket(inbox, SOCKET_TEXT)
        if outcome != "claude-socket":
            probe.fail(f"criterion (b): the real rung answered {outcome!r} against a live socket")

        deadline = time.monotonic() + 90
        delivered = False
        while time.monotonic() < deadline and not delivered:
            later = sorted(logdir.glob("*.json"))[bootstrap:]
            delivered = any(SOCKET_TEXT.encode() in p.read_bytes() for p in later)
            time.sleep(0.5)
        if not delivered:
            probe.fail("criterion (c): the posted message never reached the model — "
                       "the idle session never woke" + pane_tail())
        print(f"PASS [claude-socket]: SessionStart announced {inbox}, the real rung posted, "
              "and the idle TUI started a turn with no terminal input")
    finally:
        tmux("kill-server")
        try:
            server.shutdown()
        except Exception:
            pass
        probe.cleanup()


if __name__ == "__main__":
    probe_claude()
    probe_ups_stdout()
    probe_socket()
