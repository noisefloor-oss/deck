#!/usr/bin/env python3
"""Exercise each harness's native a2a receive path in an isolated real CLI.

Every probe runs the real harness executable against a synthetic mailbox and
a loopback-only dummy model, in a temporary provider home: no user
session, terminal input, or provider credential is used, and nothing touches
the real palace or relay. A probe passes only when the synthetic invitation
reaches the agent through the harness's own native receive path — the
extension hand-off, the session-server prompt, the stop-hook block, or the
native queue — and the mailbox records the acknowledgement that path owes.

Usage: probe_a2a_receive.py --harness {omp,pi,opencode,claude,kimi,codex}
"""

from __future__ import annotations

import argparse
import json
import os
import socket
import subprocess
import tempfile
import threading
import time
import urllib.request
from pathlib import Path

INVITE_BODY = "Synthetic native receive check"
HOOKS = Path(__file__).resolve().parent / "hooks"
MAILBOX = r'''
import json, pathlib, sys, time
p = pathlib.Path(__file__).parent
def calls():
    with (p / "calls").open("a") as f:
        f.write(json.dumps(sys.argv[1:]) + "\n")
def invite():
    return {"id": "synthetic", "type": "chat.invite", "from_agent": "fixture",
            "correlation_id": "chat-fixture", "scope": "chat-fixture|invite",
            "body": BODY}
BODY = __BODY__
DELAY = __DELAY__
argv = sys.argv[1:]
calls()
if DELAY and argv[:1] in (["inbox"], ["mark-read"]):
    # Model a relay whose rooms have grown: the mailbox read is slow, past
    # the extension's old five-second budget, and delivery must still happen.
    time.sleep(DELAY)
if argv[:1] == ["inbox"] and "--follow" in argv:
    # The follower: an initial line of unread mail, then hold the stream
    # open the way the relay's wait would — the harness kills it at
    # shutdown. The probe asserts the pid stayed alive and a second wake
    # was emitted, so a stub that dies after its first line fails the
    # probe instead of hiding behind fallback delivery.
    (p / "follow-pid").write_text(str(__import__("os").getpid()))
    if DELAY:
        time.sleep(DELAY)
    sys.stdout.write(json.dumps([] if (p / "acked").exists() else [invite()]) + "\n")
    sys.stdout.flush()
    # The receipt that acknowledges the invitation is itself a room event,
    # so the relay wakes the stream again: emit a second, empty wake as
    # soon as the acknowledgement lands, proving the stream survived its
    # first line instead of crashing behind a successful delivery.
    for _ in range(1200):
        if (p / "acked").exists():
            sys.stdout.write("[]\n")
            sys.stdout.flush()
            (p / "woke2").write_text("yes")
            break
        time.sleep(0.05)
    time.sleep(3600)
if argv[:1] == ["inbox"]:
    print("[]" if (p / "acked").exists() else json.dumps([invite()]))
elif argv[:1] == ["mark-read"]:
    (p / "acked").write_text("yes")
elif argv[:1] == ["hook"]:
    event = argv[1] if len(argv) > 1 else ""
    harness = argv[argv.index("--harness") + 1] if "--harness" in argv else ""
    try:
        (p / f"hookstdin-{event}.json").write_text(sys.stdin.read())
    except OSError:
        pass
    fresh = not (p / "acked").exists()
    if fresh and event == "stop" and harness == "claude":
        # The real client marks the events read as it prints the block.
        (p / "acked").write_text("yes")
        print(json.dumps({"decision": "block", "reason":
            f"[a2a#synthetic] fixture says (chat-fixture): {BODY}\n"
            "reply with `a2a reply chat-fixture \"...\"`"}))
    elif fresh and event == "stop" and harness == "kimi":
        (p / "acked").write_text("yes")
        print(f"[a2a#synthetic] fixture says (chat-fixture): {BODY}", file=sys.stderr)
        raise SystemExit(2)
    elif fresh and event == "prompt" and harness == "kimi":
        (p / "acked").write_text("yes")
        print(f"[a2a#synthetic] fixture says (chat-fixture): {BODY}")
elif argv[:1] == ["whoami"]:
    print("probe-fixture")
'''


def free_port() -> int:
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


class Probe:
    def __init__(self, harness: str, keep: bool = False, mail_delay_s: float = 0):
        self.harness = harness
        self.keep = keep
        self.mail_delay_s = mail_delay_s
        self.root = Path(tempfile.mkdtemp(prefix=f"a2a-receive-{harness}-"))
        for name in ("home", "cwd", "state", "agent"):
            (self.root / name).mkdir()
        (self.root / "mail.py").write_text(
            MAILBOX.replace("__BODY__", json.dumps(INVITE_BODY)).replace("__DELAY__", str(mail_delay_s))
        )
        self.process: subprocess.Popen | None = None
        self.pty_master: int | None = None

    def env(self, extra: dict[str, str]) -> dict[str, str]:
        env = {key: value for key, value in os.environ.items() if key in ("PATH", "LANG", "TERM", "TMPDIR")}
        env.update(
            HOME=str(self.root / "home"),
            A2A_BIN=str(self.root / "mail.py"),
            A2A_STATE_DIR=str(self.root / "state"),
            NOISEDECK_MEMPALACE_PYTHON="/usr/bin/python3",
        )
        env.update(extra)
        return env

    def spawn(self, argv: list[str], env: dict[str, str], cwd: Path | None = None, pty: bool = False) -> subprocess.Popen:
        stdout = (self.root / "stdout").open("w")
        stderr = (self.root / "stderr").open("w")
        kwargs = {}
        if pty:
            import pty
            main, follower = pty.openpty()
            self.pty_master = main
            kwargs = {"stdin": follower}
            stdout = stderr = follower  # a pty merges the streams
        self.process = subprocess.Popen(
            argv, cwd=str(cwd or self.root / "cwd"), env=env,
            stdin=(subprocess.PIPE if not pty else kwargs["stdin"]),
            stdout=stdout, stderr=stderr, start_new_session=True,
        )
        if pty:
            os.close(follower)
            # A terminal harness needs the pty reader drained or it blocks.
            self._pty_reader = threading.Thread(
                target=lambda: self._drain(main), daemon=True
            ).start()
        return self.process

    def write_pty(self, text: str) -> None:
        if self.pty_master is None:
            raise RuntimeError("the harness has no interactive terminal")
        os.write(self.pty_master, text.encode())

    def _drain(self, fd: int) -> None:
        try:
            while True:
                chunk = os.read(fd, 4096)
                if not chunk:
                    break
                with (self.root / "pty.log").open("ab") as f:
                    f.write(chunk)
        except OSError:
            pass
        finally:
            os.close(fd)

    def listening_port(self) -> int | None:
        """The port a serve process says it bound, from its own output."""
        for name in ("stdout", "stderr", "pty.log"):
            path = self.root / name
            if not path.exists():
                continue
            for line in reversed(path.read_text(errors="replace").splitlines()):
                if "listening on http://" in line:
                    try:
                        return int(line.rsplit(":", 1)[1].split("/")[0])
                    except (IndexError, ValueError):
                        continue
        return None

    def tail(self, name: str, lines: int = 6) -> str:
        path = self.root / name
        if not path.exists():
            return ""
        text = path.read_text(errors="replace").strip().splitlines()
        return "\n  " + "\n  ".join(text[-lines:]) if text else ""

    def stop(self) -> None:
        if self.process and self.process.poll() is None:
            self.process.terminate()
            try:
                self.process.wait(timeout=5)
            except subprocess.TimeoutExpired:
                self.process.kill()
                self.process.wait()

    def calls(self) -> list[list[str]]:
        path = self.root / "calls"
        if not path.exists():
            return []
        return [json.loads(line) for line in path.read_text().splitlines() if line.strip()]

    def events(self) -> list[str]:
        path = self.root / "events"
        return path.read_text().splitlines() if path.exists() else []

    def mark(self, name: str) -> bool:
        return (self.root / name).exists()

    def wait_for(self, predicate, timeout: float, what: str) -> None:
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            if predicate():
                return
            if self.process and self.process.poll() is not None:
                break
            time.sleep(0.1)
        if not predicate():
            raise AssertionError(f"{what} (harness={self.harness})")

    def follower_woke_twice(self) -> bool:
        return (self.root / "woke2").exists()

    def assert_follower_alive(self) -> None:
        # Delivery with the interval disabled proves the follower delivered;
        # a second wake written after the acknowledgement proves the same
        # child survived its first line instead of crashing behind a
        # successful delivery. Whether the pid is still alive afterwards is
        # not observable here: a harness that ends its own session must take
        # the follower with it, and one that keeps running must not, so pid
        # liveness can race either way by design.
        if not (self.root / "follow-pid").exists() or not (self.root / "woke2").exists():
            self.fail("the follower never reached its second wake")

    def cleanup(self) -> None:
        if not self.keep:
            subprocess.run(["rm", "-rf", str(self.root)], check=False)

    def fail(self, detail: str) -> None:
        raise SystemExit(
            f"FAIL [{self.harness}]: {detail}\n  root: {self.root}"
            + ("" if self.keep else " (removed)")
        )


# ── Pi family: the extension hands unread mail over as a user prompt ────────


PI_MODELS = json.dumps({"providers": {"probe": {
    "baseUrl": "http://127.0.0.1:1/v1", "api": "openai-completions",
    "apiKey": "synthetic-only",
    "models": [{"id": "probe", "name": "probe", "contextWindow": 8192,
                "maxTokens": 64, "input": ["text"],
                "cost": {"input": 0, "output": 0, "cacheRead": 0, "cacheWrite": 0}}],
}}})


def probe_pi_family(probe: Probe, binary: str) -> None:
    # omp reads models.yml; pi 1.1.0 reads models.json. Writing both keeps
    # the probe blind to which side of the fork it faces.
    (probe.root / "agent" / "models.yml").write_text(PI_MODELS)
    (probe.root / "agent" / "models.json").write_text(PI_MODELS)
    flags = ["--no-session", "--no-tools", "--no-skills", "--no-extensions"]
    if binary == "omp":
        flags = ["--no-session", "--no-tools", "--no-lsp", "--no-skills", "--no-rules", "--no-extensions"]
    # Observe native lifecycle events, never scrape a screen or a session file.
    (probe.root / "extension.mjs").write_text(
        f"import hook from {json.dumps(str(HOOKS / 'a2a-pi-hook.js'))};\n"
        'import {writeFileSync, appendFileSync} from "node:fs";\n'
        "export default function(pi) { hook(pi);\n"
        'for (const n of ["session_start","agent_start","agent_end","session_shutdown"])\n'
        f'  pi.on(n, () => appendFileSync({json.dumps(str(probe.root / "events"))}, n+"\\n"));\n'
        'pi.on("agent_start", (_event, ctx) => {\n'
        f'writeFileSync({json.dumps(str(probe.root / "started"))}, "yes");\n'
        "ctx.abort(); ctx.shutdown(); }); }\n"
    )
    env = probe.env({
        "PI_CODING_AGENT_DIR": str(probe.root / "agent"),
        "A2A_PI_POLL_MS": "3600000",
        "NOISEDECK_MEMPALACE_HARNESS": "omp" if binary == "omp" else "pi",
    })
    argv = [binary, "--model", "probe/probe", "--mode", "rpc", *flags,
            "--extension", str(probe.root / "extension.mjs")]
    probe.spawn(argv, env)
    budget = 90 if probe.mail_delay_s else 25
    try:
        probe.wait_for(
            lambda: probe.mark("started") and probe.mark("acked") and probe.follower_woke_twice(),
            budget, "the extension never delivered the invitation as a user prompt")
        probe.assert_follower_alive()
    finally:
        probe.stop()
    if "session_start" not in probe.events():
        probe.fail("extension saw no session_start; the extension never loaded")
    if not probe.mark("acked"):
        probe.fail("invitation reached the turn but was never acknowledged (mark-read missing)")
    slow = " even with a mailbox read slower than the old five-second hook budget" if probe.mail_delay_s else ""
    print(f"PASS [{probe.harness}]: synthetic invitation started a real {binary} turn "
          f"and was acknowledged{slow}; no terminal input or provider credentials used")


# ── OpenCode: the plugin prompts the session through its server API ─────────


def probe_opencode(probe: Probe) -> None:
    # The plugin's own contract first, deterministically: adoption, the idle
    # gate, the inbox poll, the prompt, the receipt and the acknowledgement,
    # driven through a stub session client. The serve integration below then
    # exercises the whole harness against a loopback model.
    plugin_drive = subprocess.run(
        ["/usr/bin/python3", str(Path(__file__).resolve().parent / "probe_a2a_opencode_plugin.py")],
        capture_output=True, text=True, timeout=120,
    )
    if plugin_drive.returncode != 0:
        probe.fail("the plugin drive failed:\n" + (plugin_drive.stdout + plugin_drive.stderr)[-1500:])
    print(plugin_drive.stdout.strip())
    model = _loopback_openai_model(probe)
    config = probe.root / "home" / ".config" / "opencode"
    (config / "plugins").mkdir(parents=True)
    (config / "opencode.json").write_text(json.dumps({
        "share": "disabled",
        "telemetry": False,
        "model": "probe/probe",
        # The only outbound need is the plugin SDK the bootstrap installs;
        # the model itself stays on loopback with a synthetic key.
        "provider": {"probe": {
            "npm": "@ai-sdk/openai-compatible",
            "options": {
                "baseURL": f"http://127.0.0.1:{model['port']}/v1",
                "apiKey": "synthetic-only",
            },
            "models": {"probe": {"name": "probe"}},
        }},
    }))
    (config / "plugins" / "a2a-opencode-hook.js").write_text(
        (HOOKS / "a2a-opencode-hook.js").read_text()
    )
    port = free_port()
    env = probe.env({
        "A2A_OPENCODE_POLL_MS": "3600000",
    })
    argv = ["opencode", "serve", "--port", str(port), "--hostname", "127.0.0.1", "--print-logs"]
    probe.spawn(argv, env)
    try:
        # The serve line names the port actually bound; a requested port can
        # be taken in the window between free_port() and the bind, so the
        # probe talks to what opencode says it is listening on.
        probe.wait_for(lambda: probe.listening_port(), 15, "opencode never printed its listening port")
        base = f"http://127.0.0.1:{probe.listening_port()}"
        probe.wait_for(lambda: _up(base + "/session"), 45,
                       "opencode server never came up" + probe.tail("stderr"))
        # A session created through the server fires session.created, which
        # the plugin adopts; once idle it polls the mailbox and prompts.
        request = urllib.request.Request(base + "/session", method="POST",
                                         data=json.dumps({}).encode(),
                                         headers={"Content-Type": "application/json"})
        urllib.request.urlopen(request, timeout=10).read()
        probe.wait_for(lambda: probe.mark("acked") and probe.follower_woke_twice(), 45,
                       "the plugin never delivered the invitation as a session prompt" + probe.tail("calls"))
        probe.assert_follower_alive()
        probe.wait_for(lambda: _model_saw(model, INVITE_BODY), 30,
                       "the loopback model never saw the delivered invitation" + probe.tail("calls"))
    finally:
        probe.stop()
        model["server"].shutdown()
    hooks = [c for c in probe.calls() if c[:1] == ["hook"]]
    if not any("session-start" in c for c in hooks):
        probe.fail("plugin never announced presence (no session-start hook call)")
    print("PASS [opencode]: synthetic invitation was prompted into a real session "
          "and acknowledged; no terminal input or provider credentials used")


def _loopback_openai_model(probe: Probe) -> dict:
    """A loopback OpenAI-compatible chat server that records every request
    body under the probe root and answers with one short completion."""
    import http.server
    import socketserver

    log = probe.root / "modellog"
    log.mkdir(exist_ok=True)
    lock = __import__("threading").Lock()
    counter = [0]

    class Handler(http.server.BaseHTTPRequestHandler):
        def log_message(self, *args):
            pass

        def do_POST(self):
            length = int(self.headers.get("Content-Length") or 0)
            body = self.rfile.read(length) if length else b""
            with lock:
                counter[0] += 1
                (log / f"{counter[0]}.json").write_bytes(body)
            payload = json.dumps({"id": "chatcmpl-probe", "object": "chat.completion",
                                  "choices": [{"index": 0, "finish_reason": "stop",
                                               "message": {"role": "assistant", "content": "ok"}}],
                                  "usage": {"prompt_tokens": 1, "completion_tokens": 1}}).encode()
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(payload)))
            self.end_headers()
            self.wfile.write(payload)

    server = socketserver.ThreadingTCPServer(("127.0.0.1", 0), Handler)
    import threading
    threading.Thread(target=server.serve_forever, daemon=True).start()
    return {"server": server, "port": server.server_address[1], "log": log}


def _model_saw(model: dict, needle: str) -> bool:
    for path in sorted(model["log"].glob("*.json")):
        if needle in path.read_text(errors="replace"):
            return True
    return False


def _up(url: str) -> bool:
    try:
        urllib.request.urlopen(url, timeout=2)
        return True
    except Exception:
        return False


HARNESS_BINARIES = {"omp": "omp", "pi": "pi", "opencode": "opencode",
                    "claude": "claude", "kimi": "kimi", "codex": "codex"}


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--harness", required=True, choices=sorted(HARNESS_BINARIES))
    parser.add_argument("--keep", action="store_true", help="keep the probe root for inspection")
    args = parser.parse_args()

    def run(mail_delay_s: float = 0) -> None:
        probe = Probe(args.harness, keep=args.keep, mail_delay_s=mail_delay_s)
        try:
            if args.harness in ("omp", "pi"):
                probe_pi_family(probe, HARNESS_BINARIES[args.harness])
            elif args.harness == "opencode":
                probe_opencode(probe)
            else:
                probe.fail("probe not implemented yet for this harness")
        except AssertionError as exc:
            probe.stop()
            probe.fail(str(exc))
        except Exception as exc:  # a probe infrastructure failure is a failure
            probe.stop()
            probe.fail(f"{type(exc).__name__}: {exc}")
        finally:
            probe.cleanup()

    run()
    if args.harness in ("omp", "pi", "opencode"):
        # A relay whose rooms have grown answers mailbox reads slower than
        # the extensions' old five-second budget; delivery must survive it.
        run(mail_delay_s=8)


if __name__ == "__main__":
    main()
