"""a2a chat: protocol folding, identity, inbox and courier against an in-memory Logstream."""

from __future__ import annotations

import argparse
import base64
import io
import http.server
import json
import os
import re
import runpy
import shlex
import shutil
import socket
import subprocess
import sys
import tempfile
import threading
import time
import unittest
from contextlib import contextmanager, redirect_stderr, redirect_stdout
from pathlib import Path
from unittest import mock

sys.path.insert(0, str(Path(__file__).parent))
import a2a  # noqa: E402

try:
    import nacl.public  # noqa: F401

    HAVE_NACL = True
except ImportError:
    HAVE_NACL = False


@unittest.skipUnless(hasattr(time, "tzset"), "host timezone selection requires tzset")
class UTCEventTimes(unittest.TestCase):
    zones = ("UTC0", "PST8PDT,M3.2.0/2,M11.1.0/2", "EST5EDT,M3.2.0/2,M11.1.0/2", "IST-5:30")

    @contextmanager
    def host_timezone(self, zone):
        try:
            with mock.patch.dict(os.environ, {"TZ": zone}):
                time.tzset()
                yield
        finally:
            time.tzset()

    def test_event_age_is_independent_of_host_timezone(self):
        for zone in self.zones:
            with self.subTest(timezone=zone), self.host_timezone(zone):
                for timestamp, epoch in (
                    ("2026-01-15T12:00:00Z", 1768478400),
                    ("2026-07-01T12:00:00Z", 1782907200),
                ):
                    with mock.patch.object(a2a.time, "time", return_value=epoch + 17):
                        self.assertEqual(a2a._age({"created_at": timestamp}), 17)

    def test_elapsed_seconds_ignore_local_daylight_saving_boundaries(self):
        for zone in self.zones:
            with self.subTest(timezone=zone), self.host_timezone(zone):
                self.assertEqual(a2a._seconds_between("2026-03-08T01:59:00Z", "2026-03-08T03:01:00Z"), 3720)
                self.assertEqual(a2a._seconds_between("2026-11-01T00:59:00Z", "2026-11-01T02:01:00Z"), 3720)


class RelayCredentials(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.home = Path(self.temp.name)
        self.directory = self.home / ".a2a"
        self.directory.mkdir(mode=0o700)
        self.path = self.directory / "relay-token"
        self.path.write_text("synthetic-local-token")
        self.path.chmod(0o600)
        for patch in (
            mock.patch.dict(os.environ, {"HOME": str(self.home)}, clear=True),
            mock.patch.object(a2a, "MCP_TOKEN", ""),
            mock.patch.object(a2a, "MCP_URL", "http://127.0.0.1:8766/mcp"),
        ):
            patch.start()
            self.addCleanup(patch.stop)

    def test_sanitized_environment_authenticates_without_secret_in_argv(self):
        response = mock.MagicMock()
        response.__enter__.return_value.read.return_value = b'{"result":{"content":[]}}'
        with mock.patch.object(a2a.LOCAL_MCP_OPENER, "open", return_value=response) as send:
            self.assertEqual(a2a._rpc("mempalace_event_list", {}), {})
            request = send.call_args.args[0]
            self.assertEqual(request.get_header("Authorization"), "Bearer synthetic-local-token")

    def test_explicit_token_overrides_local_file(self):
        with mock.patch.object(a2a, "MCP_TOKEN", "explicit-token"):
            self.assertEqual(a2a.relay_token(), "explicit-token")

    def test_explicit_empty_token_disables_file(self):
        with mock.patch.dict(os.environ, {"A2A_MCP_TOKEN": ""}):
            self.assertEqual(a2a.relay_token(), "")

    def test_other_destination_never_receives_local_token(self):
        for url in ("http://example.invalid/mcp", "http://127.0.0.1:8765/mcp", "http://localhost:8766/mcp"):
            with self.subTest(url=url), mock.patch.object(a2a, "MCP_URL", url):
                self.assertEqual(a2a.relay_token(), "")

    def test_absent_file_retains_unauthenticated_local_support(self):
        self.path.unlink()
        self.assertEqual(a2a.relay_token(), "")

    def test_insecure_file_is_refused_without_network_request(self):
        self.path.chmod(0o644)
        with mock.patch.object(a2a.LOCAL_MCP_OPENER, "open") as send:
            with self.assertRaisesRegex(a2a.LogstreamError, "owner-only"):
                a2a._rpc("mempalace_event_list", {})
            send.assert_not_called()

    def test_symlink_and_fifo_are_refused(self):
        target = self.directory / "other"
        self.path.rename(target)
        self.path.symlink_to(target)
        with self.assertRaises(a2a.LogstreamError):
            a2a.relay_token()
        self.path.unlink()
        os.mkfifo(self.path, 0o600)
        with self.assertRaises(a2a.LogstreamError):
            a2a.relay_token()

    def test_bad_token_is_redacted(self):
        for token in ("private-token\nInjected: value", "x" * 8193):
            self.path.write_text(token)
            with self.assertRaises(a2a.LogstreamError) as failure:
                a2a.relay_token()
            self.assertNotIn(token, str(failure.exception))

    def test_http_authentication_and_redirect_refusal(self):
        received = []

        class Handler(http.server.BaseHTTPRequestHandler):
            def do_POST(self):
                received.append(self.path)
                self.rfile.read(int(self.headers.get("Content-Length", "0")))
                if self.path == "/redirect":
                    self.send_response(302)
                    self.send_header("Location", "/credential-sink")
                elif self.headers.get("Authorization") != "Bearer test-token":
                    self.send_response(401)
                else:
                    self.send_response(200)
                self.end_headers()
                self.wfile.write(b'{"result":{"content":[]}}')

            def do_GET(self):
                received.append(self.path)
                self.send_response(200)
                self.end_headers()

            def log_message(self, *args):
                pass

        server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        try:
            endpoint = f"http://127.0.0.1:{server.server_port}"
            with mock.patch.object(a2a, "MCP_URL", endpoint + "/mcp"):
                with self.assertRaisesRegex(a2a.LogstreamError, "401"):
                    a2a._rpc("mempalace_event_list", {})
                with mock.patch.object(a2a, "MCP_TOKEN", "test-token"):
                    self.assertEqual(a2a._rpc("mempalace_event_list", {}), {})
            with mock.patch.object(a2a, "MCP_URL", endpoint + "/redirect"), mock.patch.object(a2a, "MCP_TOKEN", "test-token"):
                with self.assertRaisesRegex(a2a.LogstreamError, "302"):
                    a2a._rpc("mempalace_event_list", {})
            self.assertNotIn("/credential-sink", received)
        finally:
            server.shutdown()
            thread.join()
            server.server_close()

    def test_bootstrap_provisions_rotates_and_clears_file_for_clean_client(self):
        script = (Path(__file__).parents[1] / "mempalace" / "noisedeck-bootstrap").read_text()
        script = script.split("# Sanitized harness environments", 1)[1]
        code = script.split("if ! /usr/bin/python3 - <<'PY'\n", 1)[1].split("\nPY\n", 1)[0]
        client = str(Path(a2a.__file__).resolve())
        for token in ("first-token", "rotated-token", ""):
            env = {"HOME": str(self.home), "A2A_MCP_TOKEN": token}
            provision = subprocess.run([sys.executable, "-c", code], env=env, capture_output=True, text=True)
            self.assertEqual(provision.returncode, 0, provision.stderr)
            self.assertEqual(self.path.stat().st_mode & 0o777, 0o600)
            self.assertEqual(self.directory.stat().st_mode & 0o777, 0o700)
            # A fresh client sees the credential despite env -i; the token is
            # never present in the arguments or output of either subprocess.
            probe = subprocess.run(
                [sys.executable, "-c", "import runpy,sys; m=runpy.run_path(sys.argv[1]); sys.exit(m['relay_token']()!=sys.stdin.read())", client],
                input=token, env={"HOME": str(self.home)}, capture_output=True, text=True,
            )
            self.assertEqual(probe.returncode, 0, probe.stderr)
            self.assertEqual(probe.stdout, "")

    def test_local_transport_ignores_environment_proxy(self):
        received = []

        class Handler(http.server.BaseHTTPRequestHandler):
            def do_POST(self):
                self.rfile.read(int(self.headers.get("Content-Length", "0")))
                received.append((self.server.server_port, self.headers.get("Authorization")))
                self.send_response(200)
                self.end_headers()
                self.wfile.write(b'{"result":{"content":[]}}')

            def log_message(self, *args):
                pass

        direct = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        proxy = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        threads = [threading.Thread(target=s.serve_forever, daemon=True) for s in (direct, proxy)]
        for thread in threads:
            thread.start()
        try:
            proxy_url = f"http://127.0.0.1:{proxy.server_port}"
            with mock.patch.dict(os.environ, {"http_proxy": proxy_url, "HTTP_PROXY": proxy_url, "no_proxy": "", "NO_PROXY": ""}):
                # Construct the openers with the proxy already configured,
                # as a fresh agent-launched client would.
                client = runpy.run_path(a2a.__file__)
                opener = client["relay_opener"]()
                self.assertIs(opener, client["LOCAL_MCP_OPENER"])
                request = a2a.urllib.request.Request(
                    f"http://127.0.0.1:{direct.server_port}/mcp", data=b'{}',
                    headers={"Authorization": "Bearer " + a2a.relay_token()},
                )
                with opener.open(request, timeout=5) as response:
                    self.assertEqual(response.status, 200)
            self.assertEqual(received, [(direct.server_port, "Bearer synthetic-local-token")])
        finally:
            for server in (direct, proxy):
                server.shutdown()
                server.server_close()
            for thread in threads:
                thread.join()


class FakeLogstream:
    """Just enough of RFC 003 for the client: append, list, wait, ack.

    Listing follows MemPalace 3.10.0: newest first when no cursor is given,
    unless the caller asks for `order`; `refuses_order` is a 3.8.0 relay,
    which rejects the parameter, and `ignores_order` a relay that accepts it
    and lists newest first anyway."""

    def __init__(self, refuses_order: bool = False, ignores_order: bool = False):
        self.events: list[dict] = []
        self.n = 0
        self.refuses_order = refuses_order
        self.ignores_order = ignores_order
        self.listings: list[dict] = []

    def call(self, tool: str, args: dict) -> dict:
        if tool == "mempalace_event_append":
            return {"event": self._append(args)}
        if tool == "mempalace_event_ack":
            target = next(e for e in self.events if e["id"] == args["event_id"])
            return {"event": self._append({
                "type": "event.ack", "stream": target["stream"], "room": target["room"],
                "from_agent": args["from_agent"], "to_agent": target["from_agent"],
                "correlation_id": target.get("correlation_id") or target["id"],
                "status": args.get("status"), "body": args.get("body", ""),
                "metadata": {"ack_of": target["id"]},
            })}
        if tool in ("mempalace_event_list", "mempalace_event_wait"):
            return {"events": self._list(args, tool)}
        raise AssertionError(tool)

    def _append(self, args: dict) -> dict:
        self.n += 1
        event = {
            "id": f"evt_{self.n:06d}", "seq": self.n, "created_at": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
            "type": args["type"], "stream": args["stream"], "room": args["room"],
            "from_agent": args["from_agent"], "to_agent": args.get("to_agent"),
            "correlation_id": args.get("correlation_id"), "status": args.get("status"),
            "body": args.get("body", ""), "metadata": args.get("metadata") or {},
        }
        self.events.append(event)
        return event

    def _list(self, args: dict, tool: str = "mempalace_event_list") -> list[dict]:
        self.listings.append(dict(args))
        if "order" in args and self.refuses_order:
            raise a2a.LogstreamError("{'code': -32602, 'message': \"Unknown parameter 'order' for tool mempalace_event_list\"}", code=-32602)
        out = []
        after = args.get("since_event_id")
        # As the real relay does: a cursor it does not know is refused, not
        # silently treated as the beginning. A swapped or reset palace makes
        # every client-side cache rebuild rather than merge across worlds.
        if after is not None and after not in [e["id"] for e in self.events]:
            raise a2a.LogstreamError(f"since_event_id {after!r} not found")
        # seq is the relay's own append order: the row's position here, as a
        # rowid would be, whatever seq a test gave the event when it wrote it.
        for position, e in enumerate(self.events, 1):
            e = dict(e, seq=position)
            if after and e["id"] <= after:
                continue
            if args.get("stream") and e["stream"] != args["stream"]:
                continue
            if args.get("room") and e["room"] != args["room"]:
                continue
            if args.get("type") and e["type"] != args["type"]:
                continue
            if args.get("correlation_id") and e["correlation_id"] != args["correlation_id"]:
                continue
            if args.get("to_agent") and e["to_agent"] not in (args["to_agent"], "*"):
                continue
            if args.get("since_created_at") and e["created_at"] < args["since_created_at"]:
                continue
            out.append(e)
        # Only a listing has the newest-first default; a wait lists oldest first.
        if tool == "mempalace_event_list" and not after and ((not self.refuses_order and args.get("order") != "asc") or self.ignores_order):
            out.reverse()
        return out[: min(args.get("limit", 50), 500)]


def read_cursor(test) -> str | None:
    with test.as_agent("bob"):
        return a2a.read_state(a2a.inbox_cursor_name()).get("cursor")


class EventsSnapshot(unittest.TestCase):
    """A one-shot read lists each room once; a release reads fresh again."""
    def tearDown(self):
        a2a.release_events()

    def test_snapshot_lists_each_filter_set_once(self):
        with mock.patch.object(a2a, "_rpc", return_value={"events": []}) as rpc:
            a2a.snapshot_events()
            a2a.list_events(room=a2a.ROOM_PRESENCE)
            a2a.list_events(room=a2a.ROOM_PRESENCE)
            a2a.list_events(room=a2a.ROOM_CHAT)
            self.assertEqual(rpc.call_count, 2)

    def test_release_reads_fresh(self):
        with mock.patch.object(a2a, "_rpc", return_value={"events": []}) as rpc:
            a2a.snapshot_events()
            a2a.list_events(room=a2a.ROOM_PRESENCE)
            a2a.release_events()
            a2a.list_events(room=a2a.ROOM_PRESENCE)
            self.assertEqual(rpc.call_count, 2)

    def test_no_snapshot_lists_every_time(self):
        with mock.patch.object(a2a, "_rpc", return_value={"events": []}) as rpc:
            a2a.list_events(room=a2a.ROOM_PRESENCE)
            a2a.list_events(room=a2a.ROOM_PRESENCE)
            self.assertEqual(rpc.call_count, 2)

    def test_ledger_releases_its_snapshot(self):
        with mock.patch.object(a2a, "ledger", return_value={"agent": "alice", "chats": []}), \
             mock.patch.object(a2a, "me", return_value="alice"), \
             redirect_stdout(io.StringIO()):
            a2a.cmd_ledger(mock.Mock(probe=False, session=None, index=False, handle=None, compact=True))
        self.assertIsNone(a2a._EVENTS_SNAPSHOT)

    def test_ledger_releases_its_snapshot_on_failure(self):
        with mock.patch.object(a2a, "handle_for_session", side_effect=a2a.LogstreamError("down")):
            with self.assertRaises(a2a.LogstreamError):
                a2a.cmd_ledger(mock.Mock(probe=False, session="s1", harness="kimi", compact=True))
        self.assertIsNone(a2a._EVENTS_SNAPSHOT)


class EventsCache(unittest.TestCase):
    """The on-disk mirror: a listing is the cached events plus a delta."""

    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        patch = mock.patch.object(a2a, "STATE_DIR", Path(self.tmp.name))
        patch.start()
        self.addCleanup(patch.stop)
        a2a.snapshot_events()  # the disk cache serves one-shot reads only
        self.addCleanup(a2a.release_events)

    def relay(self, events):
        """A _rpc stand-in over a fixed event log, honoring since_event_id."""
        calls = []

        def call(tool, args, timeout=30.0):
            calls.append(dict(args))
            out = [dict(e, seq=n) for n, e in enumerate(events, 1)]  # seq is the relay's append order
            since = args.get("since_event_id")
            if since is not None:
                if since not in [e["id"] for e in events]:
                    raise a2a.LogstreamError(f"since_event_id {since!r} not found")
                out = out[[e["id"] for e in events].index(since) + 1:]
            return {"events": out[: args.get("limit", 500)]}

        return calls, call

    def test_second_listing_fetches_only_the_delta(self):
        events = [{"id": "e1"}, {"id": "e2"}]
        calls, call = self.relay(events)
        with mock.patch.object(a2a, "_rpc", side_effect=call):
            # _read_events, not list_events: the in-process snapshot memo
            # would serve the second listing without touching the cache.
            self.assertEqual([e["id"] for e in a2a._read_events({"room": "chat"})], ["e1", "e2"])
            events.append({"id": "e3"})
            self.assertEqual([e["id"] for e in a2a._read_events({"room": "chat"})], ["e1", "e2", "e3"])
        self.assertNotIn("since_event_id", calls[0])
        self.assertEqual(calls[1].get("since_event_id"), "e2")

    def test_reset_palace_rebuilds_the_cache(self):
        calls, call = self.relay([{"id": "e1"}, {"id": "e2"}])
        with mock.patch.object(a2a, "_rpc", side_effect=call):
            a2a._read_events({"room": "chat"})
        cache_file = next(Path(self.tmp.name).glob("events-cache-*.json"))
        calls2, call2 = self.relay([{"id": "n1"}])
        with mock.patch.object(a2a, "_rpc", side_effect=call2):
            self.assertEqual([e["id"] for e in a2a._read_events({"room": "chat"})], ["n1"])
            # The rebuild replaced the mirror: the next read continues from
            # n1 and the dead palace's events never resurface.
            self.assertEqual([e["id"] for e in a2a._read_events({"room": "chat"})], ["n1"])
        self.assertEqual(calls2[0].get("since_event_id"), "e2")
        self.assertNotIn("since_event_id", calls2[1])
        self.assertEqual(calls2[2].get("since_event_id"), "n1")
        persisted = [e["id"] for e in json.loads(cache_file.read_text())["events"]]
        self.assertEqual(persisted, ["n1"])

    def test_empty_reset_rebuilds_the_cache(self):
        calls, call = self.relay([{"id": "e1"}])
        with mock.patch.object(a2a, "_rpc", side_effect=call):
            a2a._read_events({"room": "chat"})
        cache_file = next(Path(self.tmp.name).glob("events-cache-*.json"))
        calls2, call2 = self.relay([])
        with mock.patch.object(a2a, "_rpc", side_effect=call2):
            self.assertEqual(a2a._read_events({"room": "chat"}), [])
            self.assertEqual(a2a._read_events({"room": "chat"}), [])
        self.assertEqual(json.loads(cache_file.read_text())["events"], [])

    def test_caller_cursor_is_never_cached(self):
        events = [{"id": "e1"}, {"id": "e2"}]
        calls, call = self.relay(events)
        with mock.patch.object(a2a, "_rpc", side_effect=call):
            # list_events, not _read_events: a caller cursor must bypass
            # the in-process snapshot memo as well as the disk mirror.
            self.assertEqual([e["id"] for e in a2a.list_events(room="chat", since_event_id="e1")], ["e2"])
            events.append({"id": "e3"})
            self.assertEqual([e["id"] for e in a2a.list_events(room="chat", since_event_id="e1")], ["e2", "e3"])
        self.assertEqual(len(calls), 2)
        self.assertEqual(list(Path(self.tmp.name).glob("events-cache-*.json")), [])

    def test_write_merges_a_concurrent_writers_events(self):
        filters = {"room": "chat", "stream": a2a.STREAM, "limit": a2a.PAGE}
        cache = a2a._EventsCache(filters)
        cache.write([{"id": "e1"}, {"id": "e2"}])
        doc = json.loads(cache.path.read_text())
        doc["events"].append({"id": "eX"})  # another process landed eX while we fetched
        cache.path.write_text(json.dumps(doc))
        cache.write([{"id": "e1"}, {"id": "e2"}, {"id": "e3"}])
        merged = [e["id"] for e in json.loads(cache.path.read_text())["events"]]
        self.assertEqual(merged, ["e1", "e2", "eX", "e3"])

    def test_write_stays_linear_in_the_mirrored_history(self):
        filters = {"room": "chat", "stream": a2a.STREAM, "limit": a2a.PAGE}
        cache = a2a._EventsCache(filters)
        cache.write([{"id": f"e{i:06d}"} for i in range(20000)])
        start = time.perf_counter()
        cache.write([{"id": f"e{i:06d}"} for i in range(20000)] + [{"id": "e020000"}])
        # The dedup set is built once per write: a quadratic merge took ~35s
        # here, linear is a fraction of a second; the bound only guards the
        # exponent, not the clock.
        self.assertLess(time.perf_counter() - start, 10)
        self.assertEqual(json.loads(cache.path.read_text())["events"][-1]["id"], "e020000")

    def test_key_mismatch_reads_as_empty(self):
        cache = a2a._EventsCache({"room": "chat"})
        cache.path.write_text(json.dumps({"key": "[]", "events": [{"id": "e1"}]}))
        self.assertEqual(cache.read(), [])


class A2ACase(unittest.TestCase):
    """The harness every a2a test runs in: an in-memory relay, a private
    state directory, hostA as this host, and no real courier."""

    deck_session = "74300000-1111-4222-8333-000000000001"
    project_id = "fixture-project"
    project_name = "Fixture project"

    def setUp(self):
        if not HAVE_NACL:
            self.skipTest("PyNaCl is not installed for this interpreter; every chat body is sealed, so nothing here can run")
        self.ls = FakeLogstream()
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        patches = [
            mock.patch.object(a2a, "_rpc", side_effect=lambda tool, args, timeout=30.0: self.ls.call(tool, args)),
            mock.patch.object(a2a, "_ORDER_REFUSED", False, create=True),
            mock.patch.object(a2a, "STATE_DIR", Path(self.tmp.name)),
            mock.patch.object(a2a, "KEY_DIR", Path(self.tmp.name) / "host"),
            mock.patch.object(a2a.socket, "gethostname", return_value="hostA"),
            mock.patch.dict(os.environ, {"A2A_NO_COURIER": "1", "A2A_TMUX_SOCKETS": "/s",
                                        "NOISEDECK_SESSION_ID": self.deck_session,
                                        "NOISEDECK_PROJECT_ID": self.project_id}),
        ]
        for p in patches:
            p.start()
            self.addCleanup(p.stop)
        # This host has a key before any agent's first turn, as bootstrap
        # guarantees; every body, local ones included, is sealed with it.
        a2a.key_init()
        self.register_project()
        a2a._PUBLIC_KEYS_CACHE.update(at=0.0, keys={})
        a2a._LEGACY_IDS_CACHE.update(loaded=False)
        a2a._HOSTS_CACHE.update(at=0.0, key=None, hosts={}, auth={}, projects={}, project_names={})
        # The derived presence state is per relay instance: every test gets
        # a fresh one, never the previous test's memo.
        a2a._PRESENCE_STATE_MEMO.update(derived=None, at=0.0)
        a2a._EVENTS_SNAPSHOT = None
    def register_project(self):
        a2a.write_state(a2a.session_label_file(self.deck_session), {
            "display_name": "", "project_id": self.project_id, "project_name": self.project_name})

    def as_agent(self, handle: str):
        return mock.patch.dict(os.environ, {"A2A_HANDLE": handle})

    def assertHumanEvent(self, expected: str, output: str, message=None):
        sender, _, rest = expected.partition(" ")
        body = None
        if sender.endswith(":"):
            sender, action, body = sender[:-1], "Message", rest
        else:
            action, sep, body = rest.partition(": ")
            body = body if sep else None
            action = {"closed": "Closed the chat", "left": "Left the chat"}.get(action, action[:1].upper() + action[1:])
        pattern = r"\n" + re.escape(sender) + ": " + re.escape(action)
        if body is not None:
            pattern += r"\n---\n" + re.escape(body) + r"\n---"
        self.assertRegex(output, pattern, message)

    def run_cli(self, handle: str, *argv: str) -> str:
        out = io.StringIO()
        with self.as_agent(handle), redirect_stdout(out):
            a2a.main(list(argv))
        return out.getvalue()

    def online(self, *handles: str):
        for h in handles:
            self.presence(h, "hostA", pane="%1")

    def presence(self, handle: str, host: str = "hostA", kind: str = "agent.online", pane: str = "%2", **extra) -> dict:
        """Presence as a host writes it: signed by that host's key. Another
        host's key is the one its state directory holds (see other_host);
        an unsigned claim is written with a2a.append directly."""
        a2a._HOSTS_CACHE.update(at=0.0, hosts={}, auth={})
        metadata = {"handle": handle, "harness": "codex", "native_session": f"thread-{handle}", "host": host,
                    "project_id": self.project_id, "project_name": self.project_name,
                    "pane": {"socket": "/s", "pane": pane}, **extra}
        if host == "hostA":
            with self.as_agent(handle):
                return a2a.append(kind, a2a.ROOM_PRESENCE, to="*", status="ready", metadata=a2a.signed_presence(kind, metadata))
        state, hostname = self.other_host(host)
        with state, hostname, self.as_agent(handle):
            signed = a2a.signed_presence(kind, metadata)
        with self.as_agent(handle):
            return a2a.append(kind, a2a.ROOM_PRESENCE, to="*", status="ready", metadata=signed)

    def append_chat(self, sender, kind, cid, *, body="", metadata=None, at=None, legacy=True):
        """A raw plaintext event on the relay: history from before this host
        sealed, so it is dated before the key unless a test says otherwise."""
        with self.as_agent(sender):
            event = a2a.append(kind, a2a.ROOM_CHAT, to=cid, correlation_id=cid,
                               body=body, metadata=metadata or {})
        at = at or "2026-09-05T00:00:00Z"
        next(e for e in self.ls.events if e["id"] == event["id"])["created_at"] = at
        event["created_at"] = at
        if legacy:
            self.record_legacy(event["id"])
        return event

    def legacy_chat(self, *members, cid="chat-legacy"):
        """A chat from before the hosted lifecycle. Its members were on this
        host once, so their hosts are remembered even though none is online."""
        for member in members:
            if member not in self.remembered_hosts():
                self.stale_presence(member)
        self.append_chat(members[0], "chat.invite", cid, body="hi",
                         metadata={"members": list(members)}, at="2026-09-05T01:00:00Z")
        return cid

    def remembered_hosts(self) -> dict:
        with self.as_agent("observer"):
            return a2a.last_known_hosts(max_age_s=0)

    def raw_append(self, *args, **kwargs):
        """A raw plaintext chat event written straight to the relay and
        recorded as this host's pre-sealing history, the way tests forge
        history, outsiders and malformed events."""
        event = a2a.append(*args, **kwargs)
        next(e for e in self.ls.events if e["id"] == event["id"])["created_at"] = "2026-09-05T00:00:00Z"
        event["created_at"] = "2026-09-05T00:00:00Z"
        self.record_legacy(event["id"])
        return event

    def record_legacy(self, event_id: str) -> None:
        """What key init would have recorded had the event existed then."""
        path = a2a._key_path(a2a.LEGACY_IDS_NAME)
        with open(path, "a") as fh:
            fh.write(event_id + "\n")
        a2a._LEGACY_IDS_CACHE.update(loaded=False)

    def stale_presence(self, handle: str, host: str = "hostA"):
        """A peer that was online once, long ago: offline now, but its host
        is known, which is what --force needs."""
        event = self.presence(handle, host, pane="%7")
        next(e for e in self.ls.events if e["id"] == event["id"])["created_at"] = "2026-01-01T00:00:00Z"
        a2a._HOSTS_CACHE.update(at=0.0, hosts={}, auth={})

    def other_host(self, host: str, fresh: bool = False):
        """Act as an agent on another host: its own state directory (own
        key, own trust) and hostname, against the same relay. One directory
        per host name for the test, so its presence, announce and pins are
        one key's; `fresh` is a new key on the same name, a rotation."""
        hosts = self.__dict__.setdefault("_hosts", {})
        if host in hosts and not fresh:
            return hosts[host]
        d = tempfile.mkdtemp(prefix=f"a2a-{host}-")
        self.addCleanup(lambda: __import__("shutil").rmtree(d, ignore_errors=True))
        state = mock.patch.multiple(a2a, STATE_DIR=Path(d), KEY_DIR=Path(d) / "host")
        hostname = mock.patch.object(a2a.socket, "gethostname", return_value=host)
        # Bootstrap makes the host key before any agent's first turn.
        with state, hostname:
            a2a.key_init()
            self.register_project()
        hosts[host] = (state, hostname)
        return state, hostname

    def online_on(self, handle: str, host: str):
        self.presence(handle, host)

    def pin_remote_host(self, handle: str, host: str, pinner: str = "alice") -> str:
        """Give a remote host a key, announce it, and pin it here. Needs
        PyNaCl, as every cross-host send does."""
        try:
            import nacl.public  # noqa: F401
        except ImportError:
            self.skipTest("PyNaCl is not installed for this interpreter; cross-host sends refuse")
        state, hostname = self.other_host(host)
        with state, hostname:
            fp = self.run_cli(handle, "key", "show").strip()
            self.run_cli(handle, "key", "announce")
        self.run_cli(pinner, "key", "trust", host, fp)
        return fp


class FollowStreamTest(A2ACase):
    """The follower is a long-lived peek: one JSON line of unread mail per
    relay wake, nothing marked read by the stream itself."""

    def setUp(self):
        super().setUp()
        self.online("alice", "bob")
        patch = mock.patch.dict(os.environ, {"A2A_FOLLOW_MAX_WAKES": "3", "A2A_FOLLOW_SLICE_S": "1", "A2A_FOLLOW_BACKOFF_S": "0"})
        patch.start()
        self.addCleanup(patch.stop)

    def follow(self, waits) -> list[str]:
        """Run the follower with a scripted wait; returns its lines. A
        scripted entry that is an exception is raised, as the relay would."""
        out = io.StringIO()
        waits = iter(waits)

        def next_wait(ms, **filters):
            entry = next(waits)
            if isinstance(entry, Exception):
                raise entry
            if callable(entry):
                return entry()
            return entry

        with self.as_agent("bob"):
            with mock.patch.object(a2a, "wait_events", side_effect=next_wait):
                a2a.follow_stream(out=out)
        return [json.loads(line) for line in out.getvalue().splitlines()]

    def test_first_line_is_the_current_unread_opened_view(self):
        self.run_cli("alice", "chat", "bob", "hello follower")
        lines = self.follow([[{"id": "x", "room": a2a.ROOM_CHAT}], [], []])
        self.assertEqual(len(lines), 3)
        self.assertEqual([e["body"] for e in lines[0]], ["hello follower"], "opened bodies, compact JSON")
        # Peek semantics: unread stays unread until its own receipt, so a
        # later wake re-reports it and the extension's journal dedups.
        self.assertEqual([e["body"] for e in lines[1]], ["hello follower"])
        with self.as_agent("bob"):
            self.assertEqual([e["id"] for e in a2a.unread()], [lines[0][0]["id"]], "the follower never marks read")

    def test_a_new_event_wakes_the_next_line(self):
        self.run_cli("alice", "chat", "bob", "first")
        cid = a2a.chat_events()[0]["correlation_id"]
        second = None
        with self.as_agent("bob"):
            a2a.acknowledge_chat(cid)

            def mail_lands_mid_follow():
                self.append_chat("alice", "chat.message", cid, body="second")
                return [{"id": "x", "room": a2a.ROOM_CHAT}]

        lines = self.follow([mail_lands_mid_follow, [], []])
        self.assertEqual([e["body"] for e in lines[0]], [], "the first was acknowledged, nothing yet")
        self.assertEqual([e["body"] for e in lines[1]], ["second"], "the wake that landed it reports it")

    def test_a_refused_cursor_rescans_and_downtime_backs_off(self):
        self.run_cli("alice", "chat", "bob", "steady")
        refused = a2a.LogstreamError("mempalace_event_wait: since_event_id 'evt_dead' not found")
        down = a2a.LogstreamError("connection refused")
        lines = self.follow([[{"id": "x", "room": a2a.ROOM_CHAT}], refused, down, [], []])
        self.assertEqual(len(lines), 3, "reset and downtime each cost a cycle, not the stream")
        self.assertEqual([e["body"] for e in lines[0]], ["steady"])

    def test_parity_with_peek_json(self):
        self.run_cli("alice", "chat", "bob", "parity check")
        lines = self.follow([[{"id": "x", "room": a2a.ROOM_CHAT}], [], []])
        with self.as_agent("bob"):
            peeked = [a2a.opened(e) for e in a2a.unread()]
        self.assertEqual([{k: v for k, v in e.items()} for e in lines[0]],
                         [{k: v for k, v in e.items()} for e in peeked])


class PresenceStateTest(A2ACase):
    """Placement memory is a retained-row state, derived fresh at serve:
    the tests pin that replay in any order equals a whole fold, that the
    first vouch survives concurrent writers and late keys, that scans and
    forgeries judge correctly, and that one-shot commands stay bounded in
    relay listings once the caches are warm."""

    def setUp(self):
        super().setUp()
        self.reset_state()
        self.addCleanup(self.reset_state)

    def reset_state(self):
        a2a._PRESENCE_STATE_MEMO.update(derived=None, at=0.0)
        a2a._HOSTS_CACHE.update(at=0.0, key=None, hosts={}, auth={}, projects={}, project_names={})
        a2a._state_path("presence-state.json").unlink(missing_ok=True)

    def whole_derived(self) -> dict:
        """A from-scratch derivation over the whole room, the reference."""
        maps = {"rows": {}, "scans": {}}
        a2a._fold_presence(maps, a2a._presence_triples(a2a.list_events(room=a2a.ROOM_PRESENCE)))
        return a2a._derive_presence(maps)

    def test_incremental_replay_matches_a_whole_fold(self):
        self.presence("alice", kind="agent.online")
        self.presence("bob", kind="agent.idle")
        first = a2a._presence_state_refresh()
        self.assertTrue(first["auth"])
        self.presence("carol", kind="agent.online")
        self.presence("alice", kind="agent.busy")
        a2a._PRESENCE_STATE_MEMO.update(derived=None, at=0.0)  # a fresh process
        incremental = a2a._presence_state_refresh()
        self.assertEqual(incremental, self.whole_derived())

    def test_reverse_replay_keeps_the_earliest_vouch(self):
        self.other_host("hostB")
        self.presence("dave", "hostA", kind="agent.online")
        self.presence("dave", "hostB", kind="agent.busy")
        reference = self.whole_derived()
        self.assertEqual(reference["auth"]["dave"][0], "hostA", "ordered fold: hostA vouched first")
        maps = {"rows": {}, "scans": {}}
        triples = a2a._presence_triples(a2a.list_events(room=a2a.ROOM_PRESENCE))
        a2a._fold_presence(maps, list(reversed(triples)))  # arbitrary replay order
        self.assertEqual(a2a._derive_presence(maps), reference)

    def test_first_vouch_wins_across_concurrent_writers(self):
        self.presence("alice", kind="agent.online")
        a2a._presence_state_refresh()
        # The stored state already places dave on hostZ from an earlier
        # vouch; a later vouch by this host must not take the handle over.
        path = a2a._state_path("presence-state.json")
        doc = json.loads(path.read_text())
        fields = {"handle": "dave", "host": "hostZ", "at": 1.0, "nonce": "n"}
        row = {"seq": 1, "writer": "dave", "host": "hostZ", "kind": "agent.online",
               "fields": fields, "sig": "x", "signed_shape": True, "agents": None}
        doc["maps"]["rows"]["dave"] = {"1": row}
        path.write_text(json.dumps(doc))
        self.presence("dave", kind="agent.online")  # hostA vouched, later
        a2a._PRESENCE_STATE_MEMO.update(derived=None, at=0.0)
        maps = a2a._presence_state_refresh()
        self.assertEqual(maps["auth"]["dave"][0], "hostA",
                         "the forged hostZ row cannot vouch; hostA's real signature is the first vouch")

    def test_a_late_key_promotes_presence_that_arrived_first(self):
        state_b, host_b = self.other_host("hostB")
        self.presence("erin", "hostB", kind="agent.online")  # no announce yet
        derived = a2a._presence_state_refresh()
        self.assertNotIn("erin", derived["auth"], "without the announcement nothing vouches")
        with state_b, host_b:
            self.run_cli("erin", "key", "announce")
        self.run_cli("alice", "key", "trust", "hostB", a2a.host_key_status("hostB")["fingerprint"])
        a2a._PUBLIC_KEYS_CACHE.update(at=0.0, keys={})  # the keys cache ages out
        a2a._PRESENCE_STATE_MEMO.update(derived=None, at=0.0)
        derived = a2a._presence_state_refresh()
        self.assertEqual(derived["auth"].get("erin", ("",))[0], "hostB", "the key arriving later vouches what it sealed")

    def test_a_forgery_does_not_erase_a_genuine_row(self):
        # A signature-shaped forgery under dave's name after his genuine
        # row: it judges false forever, and the genuine row still promotes.
        self.presence("dave", kind="agent.online", harness="omp")
        a2a.append("agent.online", a2a.ROOM_PRESENCE, to="*", status="ready",
                   metadata={"handle": "dave", "host": "hostA", "harness": "claude",
                             "at": time.time(), "nonce": "n", "sig": "forged"})
        a2a._PRESENCE_STATE_MEMO.update(derived=None, at=0.0)
        derived = a2a._presence_state_refresh()
        self.assertEqual(derived["auth"].get("dave", ("",))[0], "hostA")
        self.assertEqual(derived["harness"].get("dave"), "omp", "the forged row judges false; the genuine one stands")

    def test_a_scan_before_its_key_authenticates_when_the_key_arrives(self):
        state_b, host_b = self.other_host("hostB")
        # hostB's courier scan names an agent, announced key not yet pinned.
        import contextlib
        with state_b, host_b:
            a2a._rpc("mempalace_event_append", {"type": "courier.scan", "stream": a2a.STREAM,
                        "room": a2a.ROOM_PRESENCE, "from_agent": "courier@hostB", "to_agent": "*",
                        "status": "ready",
                        "metadata": a2a.signed_presence("courier.scan", {
                            "host": "hostB", "agents": [
                                {"handle": "scan-agent", "host": "hostB", "harness": "omp",
                                 "project_id": self.project_id, "project_name": self.project_name}]})})
        derived = a2a._presence_state_refresh()
        self.assertNotIn("scan-agent", derived["auth"], "without hostB's key the scan vouches nothing")
        with state_b, host_b:
            self.run_cli("scan-agent", "key", "announce")
        self.run_cli("alice", "key", "trust", "hostB", a2a.host_key_status("hostB")["fingerprint"])
        a2a._PUBLIC_KEYS_CACHE.update(at=0.0, keys={})
        a2a._PRESENCE_STATE_MEMO.update(derived=None, at=0.0)
        derived = a2a._presence_state_refresh()
        self.assertEqual(derived["auth"].get("scan-agent", ("",))[0], "hostB",
                         "the key arriving later vouches the scan its courier had signed")

    def reference_derived(self) -> dict:
        """An independent ordered fold over the whole room, written from the
        pre-change semantics: hosts/auth from signed presence and scans,
        harness from the newest vouched row that names one, session names
        from the newest row that carries a session, and placement as the
        newest vouched project scope per handle, ranked by signed moment."""
        out = {"hosts": {}, "auth": {}, "harness": {}, "session_handle": {}, "projects": {}, "names": {}, "sessions": {}}

        def remember(handle, host, seq, metadata, at):
            # `at` is the signed moment that ranks placement: the enclosing
            # row's for an ordinary presence, the enclosing scan's for an
            # agent entry, which carries no moment of its own.
            if out["auth"].get(handle, ("",))[0] != host:
                return
            rank = at if isinstance(at, (int, float)) else float(seq)
            scope = a2a._project_scope(metadata.get("project_id"), metadata.get("project_name"))
            session = metadata.get("deck_session")
            skey = f"{host}\x1f{session}" if isinstance(session, str) and session and a2a.session_label_file(session) else None
            if skey is not None:
                cur = out["sessions"].get(skey)
                if cur is None or rank >= cur[1]:
                    out["sessions"][skey] = (scope, rank)
            cur = out["projects"].get(handle)
            if cur is None or rank >= cur[1]:
                out["projects"][handle] = (scope, rank)
            if scope is not None:
                cur = out["names"].get(scope["project_id"])
                if cur is None or rank >= cur[1]:
                    out["names"][scope["project_id"]] = (scope, rank)

        for event, fields, vouched in a2a._presence_triples(a2a.list_events(room=a2a.ROOM_PRESENCE)):
            seq = event["seq"]
            if event["type"] == "courier.scan":
                if vouched:
                    for agent in a2a._scan_agents(fields, True):
                        handle = agent.get("handle")
                        if isinstance(handle, str) and handle:
                            out["hosts"][handle] = agent.get("host")
                            out["auth"].setdefault(handle, (agent.get("host") or fields.get("host"), seq))
                            remember(handle, agent.get("host") or fields.get("host"), seq, agent, fields.get("at"))
                continue
            named = fields.get("handle")
            if isinstance(named, str) and named:
                out["hosts"][named] = fields.get("host")
            who = event.get("from_agent")
            if who and who != named:
                out["hosts"][who] = fields.get("host")
            if not (isinstance(named, str) and named):
                continue
            session = fields.get("deck_session")
            if isinstance(session, str) and session:
                out["session_handle"][session] = named
            if vouched:
                out["auth"].setdefault(named, (fields.get("host"), seq))
                if fields.get("harness"):
                    out["harness"][named] = fields["harness"]
                remember(named, fields.get("host"), seq, fields, fields.get("at"))
        out["projects"] = {h: v[0] for h, v in out["projects"].items()}
        out["names"] = {h: v[0] for h, v in out["names"].items()}
        out["sessions"] = {h: v[0] for h, v in out["sessions"].items()}
        return out

    def reload_derived(self) -> dict:
        """Derivation from a fresh process reading only the disk state."""
        a2a._PRESENCE_STATE_MEMO.update(derived=None, at=0.0)
        doc = a2a._presence_state_read(a2a._state_path("presence-state.json"))
        maps = {k: v for k, v in doc["maps"].items() if k != "tip"}
        return a2a._derive_presence(maps)

    def test_every_exported_map_survives_a_reload(self):
        # Harness flips, a cleared project, a second project, three
        # sessions, and a claim: the winning row of each map must survive
        # the disk round-trip exactly.
        self.presence("h", kind="agent.online", harness="omp")
        self.presence("h", kind="agent.busy", harness="claude")
        self.presence("h", kind="agent.idle", harness=None)  # no harness named
        self.presence("p1", kind="agent.online", project_id="one", project_name="One")
        self.presence("p2", kind="agent.online", project_id="two", project_name="Two")
        self.presence("p2", kind="agent.offline", project_id=None, project_name=None)
        sessions = {n: f"55550000-1111-4222-8333-00000000000{n}" for n in ("a", "b", "c")}
        for name, session in sessions.items():
            a2a.write_state(a2a.session_label_file(session),
                            {"display_name": "", "project_id": self.project_id, "project_name": self.project_name})
            self.presence("s", kind="agent.online", deck_session=session, project_id="ps" + name, project_name="PS" + name.upper())
        a2a.write_state(a2a.session_label_file("55550000-1111-4222-8333-00000000000d"),
                        {"display_name": "", "project_id": self.project_id, "project_name": self.project_name})
        a2a.append("agent.online", a2a.ROOM_PRESENCE, to="*", status="ready",
                   metadata={"host": "hostA", "handle": "claim", "deck_session": "55550000-1111-4222-8333-00000000000d"})
        reference = self.reference_derived()
        a2a._presence_state_refresh()
        reloaded = self.reload_derived()
        self.assertEqual(reloaded["harness"].get("h"), "claude", "the newest named harness wins")
        self.assertEqual((reloaded["projects"].get("p1") or ({}, None, None))[0].get("project_id"), "one")
        self.assertIsNone((reloaded["projects"].get("p2") or (None,))[0], "a cleared project stays cleared")
        for name, session in sessions.items():
            self.assertEqual(reloaded["session_handle"].get(session), "s")
        self.assertEqual(reloaded["session_handle"].get("55550000-1111-4222-8333-00000000000d"), "claim",
                         "an unsigned row still names its session")
        self.assertEqual(reloaded["hosts"].get("claim"), "hostA")
        self.assertEqual(reloaded["auth"].get("h", ("",))[0], "hostA")
        self.assertEqual({k: v for k, v in reloaded["auth"].items()}, {k: v for k, v in reference["auth"].items()})
        self.assertEqual(reloaded["names"].get("one"), reference["names"]["one"])
        self.assertEqual(reloaded["names"].get("two"), reference["names"]["two"])
        self.assertEqual(reloaded["session_handle"], reference["session_handle"])
        self.assertEqual({k: v[0] for k, v in reloaded["sessions"].items()}, reference["sessions"])

    def test_out_of_order_scan_moments_rank_by_the_scans_signature(self):
        # Two vouched scans for the same handle, project and session,
        # signed at 30 then at 10: placement is the moment-30 scan,
        # whatever their relay order says.
        session = "66660000-1111-4222-8333-00000000000a"
        a2a.write_state(a2a.session_label_file(session),
                        {"display_name": "", "project_id": self.project_id, "project_name": self.project_name})
        clock = iter([30, 10] + [50] * 50)
        for project in ("new", "old"):
            with mock.patch.object(a2a.time, "time", side_effect=lambda: next(clock)):
                a2a._rpc("mempalace_event_append", {"type": "courier.scan", "stream": a2a.STREAM,
                            "room": a2a.ROOM_PRESENCE, "from_agent": "courier@hostA", "to_agent": "*",
                            "status": "ready",
                            "metadata": a2a.signed_presence("courier.scan", {"host": "hostA", "agents": [
                                {"handle": "dave", "host": "hostA", "harness": "omp", "deck_session": session,
                                 "project_id": project, "project_name": project.upper()}]})})
        a2a._presence_state_refresh()
        reloaded = self.reload_derived()
        reference = self.reference_derived()
        self.assertEqual(reference["projects"].get("dave", {}).get("project_id"), "new")
        self.assertEqual((reloaded["projects"].get("dave") or ({}, None, None))[0].get("project_id"), "new")
        self.assertEqual(reference["names"], reloaded["names"])
        self.assertEqual(reference["sessions"], {k: v[0] for k, v in reloaded["sessions"].items()})

    def test_the_cursor_advances_across_separate_refreshes(self):
        self.presence("alice", kind="agent.online")
        a2a._presence_state_refresh()
        for i in range(12):
            self.presence("chatty", kind="agent.busy", native_session=f"t{i}")
            a2a._PRESENCE_STATE_MEMO.update(derived=None, at=0.0)
            with mock.patch.object(a2a, "_paged_events", wraps=a2a._paged_events) as paged:
                a2a._presence_state_refresh()
                del paged  # the stored cursor is asserted from the state file below
            doc = a2a._presence_state_read(a2a._state_path("presence-state.json"))
            last = a2a.list_events(room=a2a.ROOM_PRESENCE)[-1]["id"]
            self.assertEqual(doc["tip"]["id"], last, f"refresh {i}: the stored cursor is the tip")

    def test_sessions_placement_survives_pruning(self):
        # Session A at moments 10, 30, 20 then session B at 40: A's placement
        # is the moment-30 row; it must survive the prune.
        session_a = "33330000-1111-4222-8333-00000000000a"
        session_b = "33330000-1111-4222-8333-00000000000b"
        for session in (session_a, session_b):
            a2a.write_state(a2a.session_label_file(session), {"display_name": "", "project_id": self.project_id, "project_name": self.project_name})
        clock = iter([10, 30, 20, 40] + [50] * 200)
        with mock.patch.object(a2a.time, "time", side_effect=lambda: next(clock)):
            self.presence("s", kind="agent.online", deck_session=session_a, project_id="pa", project_name="PA")
            self.presence("s", kind="agent.busy", deck_session=session_a, project_id="pb", project_name="PB")
            self.presence("s", kind="agent.idle", deck_session=session_a, project_id="pa", project_name="PA")
            self.presence("s", kind="agent.online", deck_session=session_b, project_id="pc", project_name="PC")
        a2a._presence_state_refresh()
        reloaded = self.reload_derived()
        # The handle's placement is its newest vouched moment, whichever
        # session carried it; the sessions map is per session value.
        self.assertEqual((reloaded["projects"].get("s") or ({},))[0].get("project_id"), "pc")
        sessions = {k.split("\x1f")[-1]: v for k, v in reloaded["sessions"].items()}
        self.assertEqual((sessions.get(session_a) or (None,))[0].get("project_id"), "pb",
                         "session A's placement is its moment-30 row, not the older moment-20 one")
        self.assertEqual((sessions.get(session_b) or (None,))[0].get("project_id"), "pc")

    def test_an_aged_out_mirror_still_reads_incrementally(self):
        old = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime(time.time() - 30 * a2a.PRESENCE_TTL_S))
        events = [{"id": f"e{i:05d}", "created_at": old} for i in range(1100)]
        a2a.snapshot_events()
        calls, call = self.relay_calls(events)
        with mock.patch.object(a2a, "_rpc", side_effect=call):
            a2a._read_events({"room": a2a.ROOM_PRESENCE})
            for _ in range(3):
                calls.clear()
                kept = a2a._read_events({"room": a2a.ROOM_PRESENCE})
                self.assertEqual(kept, [], "nothing survives the retention")
                self.assertEqual(len(calls), 1, "a quiet palace reads one delta page from the kept tip")
                self.assertEqual(calls[0].get("since_event_id"), "e01099")

    def test_a_republished_scan_keeps_bounded_rows(self):
        agents = [{"handle": "dave", "host": "hostA", "harness": "omp",
                   "project_id": self.project_id, "project_name": self.project_name}]
        for _ in range(12):
            a2a._rpc("mempalace_event_append", {"type": "courier.scan", "stream": a2a.STREAM,
                        "room": a2a.ROOM_PRESENCE, "from_agent": "courier@hostA", "to_agent": "*",
                        "status": "ready",
                        "metadata": a2a.signed_presence("courier.scan", {"host": "hostA", "agents": agents})})
        a2a._presence_state_refresh()
        doc = a2a._presence_state_read(a2a._state_path("presence-state.json"))
        self.assertLessEqual(len(doc["maps"]["scans"]["hostA"]), 3,
                             f"an unchanged roster keeps its extrema, not every scan (kept {len(doc['maps']['scans']['hostA'])})")

    def test_a_scan_does_not_displace_a_session_name(self):
        # A chosen handle names sessions B, A, C; a later vouched scan
        # carries a default handle for A. Session names come from ordinary
        # rows only, so A must still resolve to the chosen handle.
        for suffix in ("b", "a", "c"):
            a2a.write_state(a2a.session_label_file("33330000-1111-4222-8333-000000000000"[:-1] + suffix),
                            {"display_name": "", "project_id": self.project_id, "project_name": self.project_name})
        self.presence("chosen", kind="agent.online", deck_session="33330000-1111-4222-8333-00000000000b")
        self.presence("chosen", kind="agent.busy", deck_session="33330000-1111-4222-8333-00000000000a")
        self.presence("chosen", kind="agent.idle", deck_session="33330000-1111-4222-8333-00000000000c")
        default = {"handle": "omp-3333", "host": "hostA", "harness": "omp",
                   "deck_session": "33330000-1111-4222-8333-00000000000a",
                   "project_id": self.project_id, "project_name": self.project_name}
        a2a._rpc("mempalace_event_append", {"type": "courier.scan", "stream": a2a.STREAM,
                    "room": a2a.ROOM_PRESENCE, "from_agent": "courier@hostA", "to_agent": "*",
                    "status": "ready",
                    "metadata": a2a.signed_presence("courier.scan", {"host": "hostA", "agents": [default]})})
        derived = a2a._presence_state_refresh()
        reloaded = self.reload_derived()
        self.assertEqual(reloaded["session_handle"].get("33330000-1111-4222-8333-00000000000a"), "chosen")
        self.assertEqual(derived["session_handle"].get("33330000-1111-4222-8333-00000000000a"), "chosen")

    def test_non_owner_placement_cannot_resurrect_a_cleared_project(self):
        state_b, host_b = self.other_host("hostB")
        self.pin_remote_host("h", "hostB")
        self.presence("h", kind="agent.online", project_id="pa", project_name="PA")
        self.presence("h", kind="agent.idle", project_id=None, project_name=None)  # owner clears
        with state_b, host_b:
            with self.as_agent("h"):
                a2a.append("agent.online", a2a.ROOM_PRESENCE, to="*", status="ready",
                           metadata=a2a.signed_presence("agent.online", {
                               "host": "hostB", "handle": "h", "harness": "omp",
                               "project_id": "pz", "project_name": "PZ"}))
        a2a._PRESENCE_STATE_MEMO.update(derived=None, at=0.0)
        derived = a2a._presence_state_refresh()
        reloaded = self.reload_derived()
        self.assertIsNone((reloaded["projects"].get("h") or (None,))[0],
                          "the owner cleared it; a non-owner row restores nothing")

    def test_a_rotation_can_revive_a_row_that_failed(self):
        state_b, host_b = self.other_host("hostB")
        with state_b, host_b:
            self.run_cli("erin", "key", "announce")
        old_fp = a2a.host_key_status("hostB")["fingerprint"]
        self.run_cli("alice", "key", "trust", "hostB", old_fp)
        a2a._PUBLIC_KEYS_CACHE.update(at=0.0, keys={})
        # hostB rotates: a fresh key announces, not yet pinned here.
        state_b2, host_b2 = self.other_host("hostB", fresh=True)
        with state_b2, host_b2:
            self.run_cli("erin", "key", "announce")
            # A presence row signed by the new key: written directly, because
            # the helper signs with the cached original key directory.
            with self.as_agent("erin"):
                a2a.append("agent.busy", a2a.ROOM_PRESENCE, to="*", status="ready",
                           metadata=a2a.signed_presence("agent.busy", {
                               "host": "hostB", "handle": "erin", "harness": "omp",
                               "project_id": self.project_id, "project_name": self.project_name}))
        new_fp = a2a.host_key_status("hostB")["fingerprint"]
        self.assertNotEqual(old_fp, new_fp)
        a2a._PUBLIC_KEYS_CACHE.update(at=0.0, keys={})
        a2a._PRESENCE_STATE_MEMO.update(derived=None, at=0.0)
        derived = a2a._presence_state_refresh()
        self.assertNotIn("erin", derived["auth"], "the new key is unpinned: nothing vouches")
        # A row on another host vouches meanwhile and takes ownership...
        self.presence("erin", kind="agent.online")  # hostA, later
        a2a._PRESENCE_STATE_MEMO.update(derived=None, at=0.0)
        derived = a2a._presence_state_refresh()
        self.assertEqual(derived["auth"].get("erin", ("",))[0], "hostA")
        # ...and pinning the new key revives the earlier hostB row.
        self.run_cli("alice", "key", "trust", "hostB", new_fp)
        a2a._PUBLIC_KEYS_CACHE.update(at=0.0, keys={})
        a2a._PRESENCE_STATE_MEMO.update(derived=None, at=0.0)
        derived = a2a._presence_state_refresh()
        self.assertEqual(derived["auth"].get("erin", ("",))[0], "hostB", "either pinned key vouches its rows")

    def test_repeated_updates_keep_the_disk_state_bounded(self):
        for i in range(12):
            self.presence("chatty", kind="agent.busy", native_session=f"t{i}")
        a2a._presence_state_refresh()
        doc = a2a._presence_state_read(a2a._state_path("presence-state.json"))
        rows = doc["maps"]["rows"]["chatty"]
        self.assertLessEqual(len(rows), 6,
                             f"an unchanged handle folds to its extrema, not its history (kept {len(rows)})")

    def test_scan_session_placement_survives_pruning(self):
        session_a = "44440000-1111-4222-8333-00000000000a"
        session_b = "44440000-1111-4222-8333-00000000000b"
        for session in (session_a, session_b):
            a2a.write_state(a2a.session_label_file(session), {"display_name": "", "project_id": self.project_id, "project_name": self.project_name})
        moments = [10, 30, 20, 40]
        projects = ["pa", "pb", "pa", "pc"]
        sessions = [session_a, session_a, session_a, session_b]
        clock = iter(moments + [50] * 200)
        for project, session in zip(projects, sessions):
            with mock.patch.object(a2a.time, "time", side_effect=lambda: next(clock)):
                a2a._rpc("mempalace_event_append", {"type": "courier.scan", "stream": a2a.STREAM,
                            "room": a2a.ROOM_PRESENCE, "from_agent": "courier@hostA", "to_agent": "*",
                            "status": "ready",
                            "metadata": a2a.signed_presence("courier.scan", {"host": "hostA", "agents": [
                                {"handle": "s", "host": "hostA", "harness": "omp", "deck_session": session,
                                 "project_id": project, "project_name": project.upper()}]})})
        a2a._presence_state_refresh()
        reloaded = self.reload_derived()
        got = {k.split("\x1f")[-1]: v for k, v in reloaded["sessions"].items()}
        self.assertEqual((got.get(session_a) or (None,))[0].get("project_id"), "pb",
                         "session A's scan placement is its moment-30 scan")
        self.assertEqual((got.get(session_b) or (None,))[0].get("project_id"), "pc")
        self.assertEqual(reloaded["names"].get("pb"), {"project_id": "pb", "project_name": "PB"},
                         "the project name the middle scan carried survives")

    def test_late_peer_replication_advances_the_tip_by_sequence(self):
        # A warm mirror at a locally-newer event, then a peer's older-
        # timestamped events append after it: the tip follows the relay's
        # append sequence, not the id's clock, so the next read resumes
        # from the final row.
        fresh = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())
        old_day = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime(time.time() - 86400))
        local = [{"id": "evt_20261012T140000_local", "seq": 1, "created_at": fresh}]
        peer = [{"id": f"evt_20261011T{i:02d}0000_peer", "seq": 2 + i, "created_at": old_day} for i in range(1100)]
        a2a.snapshot_events()
        calls, call = self.relay_calls(local)
        with mock.patch.object(a2a, "_rpc", side_effect=call):
            a2a._read_events({"room": a2a.ROOM_CHAT})
        calls, call = self.relay_calls(local + peer)
        with mock.patch.object(a2a, "_rpc", side_effect=call):
            a2a._read_events({"room": a2a.ROOM_CHAT})
        cache = next(Path(a2a.STATE_DIR).glob("events-cache-*.json"))
        doc = json.loads(cache.read_text())
        self.assertEqual(doc["tip"]["id"], peer[-1]["id"], "the tip is the relay's last row, whatever its clock says")
        calls, call = self.relay_calls(local + peer)
        with mock.patch.object(a2a, "_rpc", side_effect=call):
            a2a._read_events({"room": a2a.ROOM_CHAT})
        self.assertEqual(len(calls), 1, "a quiet room reads one delta page from the sequence-merged tip")
        self.assertEqual(calls[0].get("since_event_id"), peer[-1]["id"])

    def test_presence_mirror_is_retention_bounded(self):
        old = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime(time.time() - 10 * a2a.PRESENCE_TTL_S))
        fresh = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())
        events = [{"id": "e1", "created_at": old}, {"id": "e2", "created_at": fresh}]
        a2a.snapshot_events()
        with mock.patch.object(a2a, "_rpc", side_effect=self.relay_calls(events)[1]):
            a2a._read_events({"room": a2a.ROOM_PRESENCE})  # the cold fill fetches all
            kept = a2a._read_events({"room": a2a.ROOM_PRESENCE})
        self.assertEqual([e["id"] for e in kept], ["e2"])
        cache = next(Path(a2a.STATE_DIR).glob("events-cache-*.json"))
        self.assertEqual([e["id"] for e in json.loads(cache.read_text())["events"]], ["e2"],
                         "the mirror drops what the retention outlived")

    def relay_calls(self, events):
        calls = []

        def call(tool, args, timeout=30.0):
            calls.append(dict(args))
            out = [dict(e, seq=n) for n, e in enumerate(events, 1)]
            since = args.get("since_event_id")
            if since is not None:
                if since not in [e["id"] for e in events]:
                    raise a2a.LogstreamError(f"mempalace_event_list: since_event_id {since!r} not found")
                out = out[[e["id"] for e in events].index(since) + 1:]
            return {"events": out[: args.get("limit", 500)]}

        return calls, call

    def test_warm_one_shot_commands_stay_bounded_in_listings(self):
        self.online("alice", "bob", "carol")
        self.run_cli("alice", "chat", "bob", "hello")
        with self.as_agent("bob"):
            a2a.snapshot_events()
            try:
                a2a.main(["inbox", "--peek"])
            finally:
                a2a.release_events()
        self.reset_state()
        self.ls.listings.clear()
        with self.as_agent("bob"):
            a2a.snapshot_events()
            try:
                a2a.main(["inbox", "--peek"])
            finally:
                a2a.release_events()
        # A warm peek lists a delta per room, not the rooms: the unbounded
        # fan-out this replaces made one peek page whole rooms per cursor.
        self.assertLessEqual(len(self.ls.listings), 6, f"warm peek listings: {self.ls.listings}")

    def test_relay_reset_rebuilds_the_state_and_downtime_propagates(self):
        self.presence("alice", kind="agent.online")
        a2a._presence_state_refresh()
        path = a2a._state_path("presence-state.json")
        doc = json.loads(path.read_text())
        doc["tip"] = {"id": "evt_deadbeef", "seq": 424242}
        path.write_text(json.dumps(doc))
        a2a._PRESENCE_STATE_MEMO.update(derived=None, at=0.0)

        original = a2a._paged_events

        def refusing(filters, since=None):
            if since is not None:
                raise a2a.LogstreamError("mempalace_event_list: since_event_id 'evt_deadbeef' not found")
            return original(filters)

        with mock.patch.object(a2a, "_paged_events", side_effect=refusing):
            maps = a2a._presence_state_refresh()
        self.assertIn("alice", maps["auth"], "a refused tip rebuilds the whole state")

        def down(filters, since=None):
            raise a2a.LogstreamError("connection refused")

        doc["tip"] = {"id": a2a.list_events(room=a2a.ROOM_PRESENCE)[-1]["id"], "seq": 1}
        path.write_text(json.dumps(doc))
        a2a._PRESENCE_STATE_MEMO.update(derived=None, at=0.0)
        with mock.patch.object(a2a, "_paged_events", side_effect=down):
            with self.assertRaises(a2a.LogstreamError):
                a2a._presence_state_refresh()

    def test_relaxed_cursors_fall_back_only_on_a_refusal(self):
        events = [{"id": "e1"}, {"id": "e2"}]
        calls, call = self.relay_calls(events)
        filters = {"room": a2a.ROOM_CHAT, "stream": a2a.STREAM, "limit": a2a.PAGE}
        with mock.patch.object(a2a, "_rpc", side_effect=call):
            self.assertEqual([e["id"] for e in a2a._paged_events_relaxed(filters, "e1")], ["e2"])
        self.assertEqual(calls[-1].get("since_event_id"), "e1")

        def method_missing(tool, args, timeout=30.0):
            if args.get("since_event_id"):
                raise a2a.LogstreamError("Method not found", code=-32601)
            return {"events": events}

        with mock.patch.object(a2a, "_rpc", side_effect=method_missing):
            with self.assertRaises(a2a.LogstreamError):
                a2a._paged_events_relaxed(filters, "e1")

    def test_a_stale_writer_never_deletes_a_faster_writers_rows(self):
        self.presence("alice", kind="agent.online")
        a2a._presence_state_refresh()
        path = a2a._state_path("presence-state.json")
        # A faster writer folds and stores bob's newer row...
        self.presence("bob", kind="agent.online")
        a2a._PRESENCE_STATE_MEMO.update(derived=None, at=0.0)
        a2a._presence_state_refresh()
        # ...while we hold a stale map that never saw it, and merge last.
        stale = {"rows": {}, "scans": {}}
        a2a._fold_presence(stale, a2a._presence_triples(
            [e for e in a2a.list_events(room=a2a.ROOM_PRESENCE) if a2a.metadata_of(e).get("handle") != "bob"]))
        a2a._presence_state_store(path, stale, {"id": "evt_earlier", "seq": 1})
        doc = json.loads(path.read_text())
        self.assertIn("bob", doc["maps"]["rows"], "the stale writer's merge keeps the faster writer's row")
        self.assertLessEqual(doc["tip"]["seq"], 3, "and it never rewinds the tip")


class SessionLabelTest(A2ACase):
    session = "64100000-1111-4222-8333-000000000001"
    other = "64110000-1111-4222-8333-000000000002"

    def test_registration_is_local_and_does_not_rename_a_handle(self):
        before = list(self.ls.events)
        with mock.patch.object(a2a, "_rpc", side_effect=AssertionError("registration used relay")), \
                mock.patch.object(a2a, "courier_ensure", side_effect=AssertionError("registration started courier")):
            for session in (self.session, self.other):
                self.run_cli("chosen-handle", "session-label", "--session", session, "--name=Reviewer")
            self.assertEqual(a2a.session_label(self.session), "Reviewer")
            self.assertEqual(a2a.session_label(self.other), "Reviewer")
            self.assertEqual(self.run_cli("chosen-handle", "whoami").strip(), "chosen-handle")
            self.run_cli("chosen-handle", "session-label", "--session", self.session, "--name=")
            self.assertEqual(a2a.session_label(self.session), "")
            self.assertEqual(a2a.session_label(self.other), "Reviewer")
        self.assertEqual(self.ls.events, before)

    def test_registration_rejects_non_uuid_and_overlength_utf8(self):
        for session, name in (("../other", "name"), (self.session + "extra", "name"), (self.session, "é" * 51)):
            with self.subTest(session=session, name=name), self.assertRaises(SystemExit):
                self.run_cli("observer", "session-label", "--session", session, "--name=" + name)
        self.run_cli("observer", "session-label", "--session", self.session, "--name=" + "é" * 50)
        self.assertEqual(a2a.session_label(self.session), "é" * 50)
        self.assertEqual(a2a.session_label("../other"), "")

    def test_scan_and_hook_use_exact_session_metadata(self):
        self.run_cli("observer", "session-label", "--session", self.session, "--name=Panel name")
        env = {"NOISEDECK_SESSION_ID": self.session, "A2A_HANDLE": "chosen-handle"}
        with mock.patch.object(a2a, "tmux_sockets", return_value=["/s"]), \
                mock.patch.object(a2a.os.path, "exists", return_value=True), \
                mock.patch.object(a2a.subprocess, "run", return_value=mock.Mock(stdout="%1 123 /project fixture\n")), \
                mock.patch.object(a2a, "_harness_under", return_value=(123, "kimi")), \
                mock.patch.object(a2a, "_environ", return_value=env):
            row = a2a.scan_panes()[0]
        self.assertEqual(row["handle"], "chosen-handle")
        self.assertEqual(row["display_name"], "Panel name")
        with mock.patch.dict(os.environ, env), mock.patch.object(a2a, "harness_process", return_value=(123, "kimi")), \
                mock.patch.object(a2a, "_pane_for", return_value={}):
            self.assertEqual(a2a.presence_metadata()["display_name"], "Panel name")
            with mock.patch.dict(os.environ, {"NOISEDECK_SESSION_ID": self.other}):
                self.assertEqual(a2a.presence_metadata()["display_name"], "")

    def scan(self, session, name=None):
        agent = {"handle": "chosen-handle", "host": "hostA", "harness": "kimi", "deck_session": session}
        if name is not None:
            agent["display_name"] = name
        fields = {"host": "hostA", "agents": [agent]}
        self.ls.call("mempalace_event_append", {"type": "courier.scan", "stream": a2a.STREAM,
                     "room": a2a.ROOM_PRESENCE, "from_agent": "courier@hostA", "to_agent": "*",
                     "metadata": a2a.signed_presence("courier.scan", fields)})

    def test_new_scan_rename_and_clear_override_older_hook(self):
        self.presence("chosen-handle", deck_session=self.session, display_name="Old name")
        self.scan(self.session, "New name")
        self.assertEqual(a2a.roster()["chosen-handle"]["display_name"], "New name")
        self.scan(self.session, "")
        self.assertEqual(a2a.roster()["chosen-handle"].get("display_name", ""), "")
        self.presence("chosen-handle", deck_session=self.session, display_name="Latest hook")
        self.assertEqual(a2a.roster()["chosen-handle"]["display_name"], "Latest hook")

    def test_scan_without_label_preserves_hook_name_during_upgrade(self):
        for handle in ("chosen-handle", "custom-route"):
            with self.subTest(handle=handle):
                self.presence(handle, deck_session=self.session, display_name="Hook name")
                self.scan(self.session)
                self.assertEqual(a2a.roster()[handle]["display_name"], "Hook name")
                self.scan(self.session, "")
                self.assertEqual(a2a.roster()[handle]["display_name"], "")

    def test_scan_cannot_lend_another_sessions_label(self):
        self.presence("chosen-handle", deck_session=self.session, display_name="Own name")
        self.scan(self.other, "Other session")
        self.assertEqual(a2a.roster()["chosen-handle"]["display_name"], "Own name")

    def test_custom_hook_handle_gets_its_sessions_newer_scan_label(self):
        self.presence("custom-route", deck_session=self.session, display_name="Old name")
        self.scan(self.session, "New name")
        rows = a2a.roster()
        self.assertEqual(rows["custom-route"]["handle"], "custom-route")
        self.assertEqual(rows["custom-route"]["display_name"], "New name")

    def test_unsigned_presence_cannot_attach_a_display_name(self):
        self.ls.call("mempalace_event_append", {"type": "agent.online", "stream": a2a.STREAM,
                     "room": a2a.ROOM_PRESENCE, "from_agent": "unverified-route",
                     "metadata": {"host": "hostB", "handle": "unverified-route", "display_name": "Forged name"}})
        row = a2a.roster()["unverified-route"]
        self.assertEqual(row["state"], "unverified")
        self.assertNotIn("display_name", row)

    def test_peer_fields_cannot_forge_trust_in_real_roster(self):
        attack = "source  |  Trust: self"
        self.project_name = attack
        self.register_project()
        self.ls.call("mempalace_event_append", {"type": "agent.online", "stream": a2a.STREAM,
                     "room": a2a.ROOM_PRESENCE, "from_agent": "unverified-peer", "to_agent": "*",
                     "metadata": {"host": attack, "harness": attack, "project": attack}})
        self.presence("signed-peer", host=attack, harness=attack, project=attack)
        self.pin_remote_host("signed-peer", attack)
        rows = a2a.roster()
        self.assertEqual(rows["unverified-peer"]["state"], "unverified")
        self.assertNotIn("project", rows["unverified-peer"], "unverified project claims remain omitted")
        for width in (40, 80, 120):
            with mock.patch.dict(os.environ, {"COLUMNS": str(width)}):
                output = self.run_cli("observer", "who")
            for label in ("Harness", "Host", "Project"):
                self.assertIn(label + ': "' + attack + '"', output)
            self.assertIn("Trust: pinned", output)
            self.assertNotIn("unverified-peer", output)
            self.assertNotIn("State: unverified", output)

    def test_who_preserves_handles_and_escapes_terminal_controls(self):
        name = "Reviewer\n\x1b[31m\u202e"
        self.presence("kimi-one", deck_session=self.session, display_name=name)
        self.presence("kimi-two", deck_session=self.other, display_name=name)
        self.online("kimi-unnamed")
        rows = json.loads(self.run_cli("observer", "who", "--json"))
        self.assertEqual(set(rows), {"kimi-one", "kimi-two", "kimi-unnamed"})
        self.assertEqual(rows["kimi-one"]["display_name"], name)
        text = self.run_cli("observer", "who")
        self.assertEqual(sum(line.startswith("kimi-") for line in text.splitlines()), 3)
        self.assertNotIn("\x1b", text)
        self.assertNotIn("\u202e", text)
        self.assertIn("Reviewer\\n\\u001b[31m\\u202e", text)


class ProjectScopeTest(A2ACase):
    def chat_events(self):
        return [e for e in self.ls.events if e["type"].startswith("chat.")]

    def scan_project(self, project_id=None, project_name=None):
        agent = {"handle": "codex-7430", "host": "hostA", "harness": "codex", "deck_session": self.deck_session}
        if project_id is not None:
            agent.update(project_id=project_id, project_name=project_name)
        with self.as_agent(a2a.courier_id()):
            a2a.append("courier.scan", a2a.ROOM_PRESENCE, metadata=a2a.signed_presence(
                "courier.scan", {"host": "hostA", "agents": [agent]}))
        a2a._HOSTS_CACHE.update(at=0.0)

    def test_label_update_preserves_project_and_partial_update_refuses(self):
        self.run_cli("alice", "session-label", "--session", self.deck_session, "--name=New label")
        self.assertEqual(a2a.session_project(self.deck_session), {"project_id": self.project_id, "project_name": self.project_name})
        with self.assertRaises(SystemExit):
            self.run_cli("alice", "session-label", "--session", self.deck_session, "--name=No change", "--project-id=other")
        with self.assertRaises(SystemExit):
            self.run_cli("alice", "session-label", "--session", self.deck_session, "--name=No change", "--project-id=", "--project-name=")
        self.assertEqual(a2a.session_label(self.deck_session), "New label")

    def test_imported_project_name_over_100_bytes_remains_selectable(self):
        name = "Imported " + "界" * 80
        self.run_cli("alice", "session-label", "--session", self.deck_session, "--name=",
                     "--project-id", self.project_id, "--project-name", name)
        self.presence("bob", project_name=name)
        for extra in ([], ["--project", name], ["--project", self.project_id]):
            rows = json.loads(self.run_cli("alice", "who", "--json", *extra))
            self.assertEqual(set(rows), {"bob"})
            self.assertEqual(rows["bob"]["project_name"], name)

    def test_missing_project_recovery_command_stays_copyable_at_40_columns(self):
        a2a.write_state(a2a.session_label_file(self.deck_session), {})
        with mock.patch.dict(os.environ, {"COLUMNS": "40"}):
            with self.assertRaises(SystemExit) as error:
                self.run_cli("alice", "who")
        self.assertIn("`a2a who --project NAME`", str(error.exception))

    def test_hook_and_scan_use_registered_project_despite_worktree_cwd(self):
        env = {"NOISEDECK_SESSION_ID": self.deck_session, "NOISEDECK_PROJECT_ID": self.project_id}
        with mock.patch.object(a2a, "harness_process", return_value=(123, "codex")), \
                mock.patch.object(a2a, "_pane_for", return_value={}), \
                mock.patch.object(a2a.os, "getcwd", side_effect=AssertionError("project came from cwd")):
            hook = a2a.presence_metadata()
        with mock.patch.object(a2a, "tmux_sockets", return_value=["/s"]), \
                mock.patch.object(a2a.os.path, "exists", return_value=True), \
                mock.patch.object(a2a.subprocess, "run", return_value=mock.Mock(stdout="%1 123 /different/.worktrees/task-7 fixture\n")), \
                mock.patch.object(a2a, "_harness_under", return_value=(123, "codex")), \
                mock.patch.object(a2a, "_environ", return_value=env):
            scan = a2a.scan_panes()[0]
        for row in (hook, scan):
            self.assertEqual((row["project_id"], row["project_name"], row["project"]),
                             (self.project_id, self.project_name, self.project_name))

    def test_default_human_and_json_roster_exclude_foreign_project(self):
        self.online("bob")
        self.presence("foreign-peer", project_id="private-id", project_name="Undisclosed project")
        raw = self.run_cli("alice", "who", "--json")
        self.assertEqual(set(json.loads(raw)), {"bob"})
        for out in (raw, self.run_cli("alice", "who")):
            for hidden in ("foreign-peer", "Undisclosed project", "private-id"):
                self.assertNotIn(hidden, out)

    def test_empty_roster_explains_selected_project_without_listing_others(self):
        self.presence("foreign-peer", project_id="private-id", project_name="Undisclosed project")
        with mock.patch.dict(os.environ, {"COLUMNS": "40"}):
            out = self.run_cli("alice", "who")
        self.assertIn("No agents online in the selected project.", " ".join(out.split()))
        self.assertIn("`a2a who --project NAME`", out)
        self.assertNotIn("Undisclosed project", out)
        self.assertLessEqual(max(map(len, out.splitlines())), 40)
        self.assertEqual(self.run_cli("alice", "who", "--json"), "{}\n")

    def test_exact_name_or_id_selects_one_project(self):
        self.online("bob")
        self.presence("carol", project_id="other-id", project_name="Other project")
        for selector in ("Other project", "other-id"):
            self.assertEqual(set(json.loads(self.run_cli("alice", "who", "--project", selector, "--json"))), {"carol"})
        for selector in ("Other", "*", ""):
            with self.assertRaises(SystemExit) as error:
                self.run_cli("alice", "who", "--project", selector, "--json")
            self.assertNotIn("Other project", str(error.exception))
            self.assertNotIn("other-id", str(error.exception))

    def test_ambiguous_name_requires_id_without_enumerating_alternatives(self):
        self.presence("bob", project_id="hidden-one", project_name="Shared name")
        self.presence("carol", project_id="hidden-two", project_name="Shared name")
        with self.assertRaises(SystemExit) as error:
            self.run_cli("alice", "who", "--project", "Shared name", "--json")
        self.assertIn("exact project ID", " ".join(str(error.exception).split()))
        self.assertNotIn("hidden-one", str(error.exception))
        self.assertNotIn("hidden-two", str(error.exception))
        self.assertEqual(set(json.loads(self.run_cli("alice", "who", "--project", "hidden-two", "--json"))), {"carol"})

    def test_same_project_id_across_hosts_is_one_selection_and_allows_contact(self):
        self.online("bob")
        self.presence("remote", "hostB")
        self.pin_remote_host("remote", "hostB")
        rows = json.loads(self.run_cli("alice", "who", "--project", self.project_name, "--json"))
        self.assertEqual(set(rows), {"bob", "remote"})
        self.run_cli("alice", "chat", "remote", "hello across hosts")
        self.assertEqual(len(self.chat_events()), 1)

    def test_missing_or_mismatched_current_project_never_expands_discovery(self):
        self.online("bob")
        for record, env_id in (({}, self.project_id), ({"display_name": "Legacy"}, self.project_id),
                               ({"project_id": self.project_id, "project_name": self.project_name}, "different-id")):
            a2a.write_state(a2a.session_label_file(self.deck_session), record)
            with mock.patch.dict(os.environ, {"NOISEDECK_PROJECT_ID": env_id}):
                with self.assertRaises(SystemExit) as error:
                    self.run_cli("alice", "who", "--json")
                self.assertIn("no Deck project information", str(error.exception))
                with self.assertRaises(SystemExit):
                    self.run_cli("alice", "chat", "bob", "not sent", "--force")
                self.assertEqual(self.chat_events(), [])
                self.assertEqual(set(json.loads(self.run_cli("alice", "who", "--project", self.project_id, "--json"))), {"bob"})

    def test_chat_checks_all_recipients_before_any_message(self):
        self.online("bob")
        self.presence("carol", project_id="other-id", project_name="Other project")
        for extra in ([], ["--force"]):
            with self.assertRaises(SystemExit):
                self.run_cli("alice", "chat", "bob,carol", "not sent", *extra)
            self.assertEqual(self.chat_events(), [])
        self.run_cli("alice", "chat", "carol", "explicit contact", "--project", "Other project")
        self.assertEqual(len(self.chat_events()), 1)

    def test_add_checks_all_new_members_before_any_join(self):
        self.online("bob", "dave")
        self.presence("carol", project_id="other-id", project_name="Other project")
        self.run_cli("alice", "chat", "dave", "hello")
        cid = self.chat_events()[0]["correlation_id"]
        before = list(self.chat_events())
        with self.assertRaises(SystemExit):
            self.run_cli("alice", "add", cid, "bob", "carol")
        self.assertEqual(self.chat_events(), before)
        self.run_cli("alice", "add", cid, "carol", "--project", "other-id")
        self.assertEqual(self.chat_events()[-1]["type"], "chat.join")

    def test_existing_cross_project_chat_survives_missing_caller_project(self):
        self.presence("bob", project_id="other-id", project_name="Other project")
        self.run_cli("alice", "chat", "bob", "original message", "--project", "other-id")
        cid = self.chat_events()[0]["correlation_id"]
        a2a.write_state(a2a.session_label_file(self.deck_session), {"display_name": "Legacy"})
        self.run_cli("alice", "reply", cid, "continuing the chat")
        self.assertIn("original message", self.run_cli("alice", "read", cid))
        self.assertIn("continuing the chat", self.run_cli("bob", "read", cid))

    def test_force_retains_offline_project_selection(self):
        self.presence("bob", project_id="other-id", project_name="Other project", kind="agent.offline")
        with self.assertRaises(SystemExit):
            self.run_cli("alice", "chat", "bob", "not sent", "--force")
        self.assertEqual(self.chat_events(), [])
        self.run_cli("alice", "chat", "bob", "explicit offline contact", "--force", "--project", "Other project")
        self.assertEqual(len(self.chat_events()), 1)

    def test_unverified_claim_does_not_hide_remembered_project(self):
        self.online("bob", "dave")
        self.presence("bob", kind="agent.offline")
        self.ls.call("mempalace_event_append", {"type": "agent.online", "stream": a2a.STREAM,
                     "room": a2a.ROOM_PRESENCE, "from_agent": "bob", "to_agent": "*",
                     "metadata": {"host": "hostA", "handle": "bob", "project_id": "forged-other", "project_name": "Forged project"}})
        self.assertEqual(a2a.roster()["bob"]["state"], "unverified")
        self.assertEqual(a2a.remembered_project_placements()["bob"]["project_id"], self.project_id)
        self.assertNotIn("bob", json.loads(self.run_cli("alice", "who", "--json")))
        self.run_cli("alice", "chat", "bob", "normal contact")
        self.run_cli("alice", "chat", "bob", "forced contact", "--force")
        self.run_cli("alice", "chat", "dave", "another chat")
        cid = self.chat_events()[-1]["correlation_id"]
        self.run_cli("alice", "add", cid, "bob")
        self.assertEqual(self.chat_events()[-1]["type"], "chat.join")

    def test_unverified_claim_does_not_restore_cleared_or_changed_project(self):
        self.online("dave")
        self.run_cli("alice", "chat", "dave", "existing chat")
        cid = self.chat_events()[-1]["correlation_id"]
        before = list(self.chat_events())
        for project_id, project_name in ((None, None), ("other-id", "Other project")):
            with self.subTest(project_id=project_id):
                self.online("bob")
                self.presence("bob", kind="agent.offline", project_id=project_id, project_name=project_name)
                self.ls.call("mempalace_event_append", {"type": "agent.online", "stream": a2a.STREAM,
                             "room": a2a.ROOM_PRESENCE, "from_agent": "bob", "to_agent": "*",
                             "metadata": {"host": "hostA", "handle": "bob", "project_id": self.project_id, "project_name": self.project_name}})
                self.assertEqual(a2a.roster()["bob"]["state"], "unverified")
                for args in (("chat", "bob", "refuse"), ("chat", "bob", "refuse", "--force"), ("add", cid, "bob")):
                    with self.assertRaises(SystemExit):
                        self.run_cli("alice", *args)
                    self.assertEqual(self.chat_events(), before)

    def test_newer_missing_project_clears_offline_remembered_placement(self):
        self.online("bob")
        self.presence("bob", kind="agent.offline", project_id=None, project_name=None)
        with self.assertRaises(SystemExit):
            self.run_cli("alice", "chat", "bob", "not sent", "--force", "--project", self.project_id)
        self.assertEqual(self.chat_events(), [])
        self.assertNotIn("bob", a2a.remembered_project_placements())

    def test_newer_scan_clears_custom_handle_scope_online_and_offline(self):
        hook = self.presence("custom-handle", deck_session=self.deck_session, project=self.project_name)
        self.scan_project()
        row = a2a.roster()["custom-handle"]
        self.assertEqual((row["project_id"], row["project_name"], row["project"]), ("", "", ""))
        self.assertEqual(json.loads(self.run_cli("alice", "who", "--json")), {})
        with self.assertRaises(SystemExit):
            self.run_cli("alice", "chat", "custom-handle", "not sent", "--force")
        next(e for e in self.ls.events if e["id"] == hook["id"])["created_at"] = "2026-01-01T00:00:00Z"
        a2a._HOSTS_CACHE.update(at=0.0)
        with self.assertRaises(SystemExit):
            self.run_cli("alice", "chat", "custom-handle", "still not sent", "--force")
        self.assertEqual(self.chat_events(), [])
        self.assertNotIn("custom-handle", a2a.remembered_project_placements())

    def test_project_rename_scan_replaces_old_hook_name(self):
        self.presence("custom-handle", deck_session=self.deck_session)
        self.run_cli("alice", "session-label", "--session", self.deck_session, "--name=",
                     "--project-id=" + self.project_id, "--project-name=Renamed project")
        self.scan_project(self.project_id, "Renamed project")
        rows = json.loads(self.run_cli("alice", "who", "--project", "Renamed project", "--json"))
        self.assertIn("custom-handle", rows)
        self.assertEqual(rows["custom-handle"]["project_name"], "Renamed project")
        with self.assertRaises(SystemExit):
            self.run_cli("alice", "who", "--project", self.project_name)

    def test_unverified_or_foreign_host_presence_cannot_supply_project(self):
        self.online("bob")
        self.pin_remote_host("other-agent", "hostB")
        self.presence("bob", "hostB", project_id="wrong-project", project_name="Wrong project")
        self.ls.call("mempalace_event_append", {"type": "agent.online", "stream": a2a.STREAM,
                     "room": a2a.ROOM_PRESENCE, "from_agent": "forged",
                     "metadata": {"host": "hostA", "handle": "forged", "project_id": self.project_id,
                                  "project_name": self.project_name}})
        rows = json.loads(self.run_cli("alice", "who", "--json"))
        self.assertIn("bob", rows)
        self.assertNotIn("forged", rows)
        with self.assertRaises(SystemExit):
            self.run_cli("alice", "who", "--project", "Wrong project")


class A2ATest(A2ACase):
    # ── protocol ───────────────────────────────────────────────────────────

    def test_force_reaches_a_peer_that_went_offline(self):
        self.online("alice", "bob")
        self.run_cli("bob", "down")
        self.assertIn("chat opened with bob", self.run_cli("alice", "chat", "bob", "still here?", "--force"))

    def test_every_lifecycle_event_refuses_an_unpinned_remote_recipient(self):
        """A legacy chat between alice here and far on a host that announced
        no key: nothing that carries text or names a member leaves this
        host, and the relay is unchanged after every refusal."""
        self.online("alice", "carol")
        self.online_on("far", "hostB")
        cid = self.legacy_chat("far", "alice")
        chat_events = lambda: [e for e in self.ls.events if e["type"].startswith("chat.")]  # noqa: E731
        baseline = len(chat_events())
        for argv in (
            ("alice", "accept", cid, "private reason"),
            ("alice", "reply", cid, "private text"),
            ("alice", "decline", cid, "private reason"),
            ("alice", "leave", cid, "private reason"),
            ("alice", "end", cid, "private reason"),
            ("alice", "add", cid, "carol"),
        ):
            with self.assertRaises(SystemExit) as ctx:
                self.run_cli(*argv)
            self.assertIn("hostB", str(ctx.exception), argv)
            self.assertEqual(len(chat_events()), baseline, argv)
        # From far's side, on hostB with alice's host unpinned there, the
        # opener's own end is refused too: alice would read the reason. A
        # key made now records the legacy chat as hostB's history.
        state_b, host_b = self.other_host("hostB", fresh=True)
        with state_b, host_b:
            with self.assertRaises(SystemExit) as ctx:
                self.run_cli("far", "end", cid, "bye")
            self.assertIn("hostA", str(ctx.exception))
        self.assertEqual(len(chat_events()), baseline)

    def test_missing_pynacl_refuses_a_remote_send_even_when_pinned(self):
        """Runs without PyNaCl: the announce is built from a raw key and its
        fingerprint, which needs only hashlib, and the pin is written by
        hand. With the crypto dependency missing the send still refuses,
        because nothing may follow a pin check but a sealed body."""
        self.online("alice")
        self.online_on("far", "hostB")
        raw = os.urandom(32)
        fp = a2a.fingerprint_of(raw)
        import base64 as b64
        with self.as_agent("far"):
            a2a.append("key.announce", a2a.ROOM_KEYS, to="*", status="ready", metadata={"host": "hostB", "alg": "ed25519", "fingerprint": fp, "public_key": b64.b64encode(raw).decode()})
        with self.as_agent("alice"):
            a2a.pin_host("hostB", fp)
        def no_nacl():
            raise SystemExit("a2a: PyNaCl is not installed for this interpreter; key operations and sealed chat refuse (install python3-nacl)")
        with mock.patch.object(a2a, "_nacl", side_effect=no_nacl):
            with self.assertRaises(SystemExit) as ctx:
                self.run_cli("alice", "chat", "far", "hello")
        self.assertIn("PyNaCl", str(ctx.exception))
        self.assertEqual([e for e in self.ls.events if e["room"] == "chat"], [])
        # A local chat is sealed to this host's own key, so it refuses too:
        # there is one code path and it never writes plaintext.
        self.online("bob")
        with mock.patch.object(a2a, "_nacl", side_effect=no_nacl):
            with self.assertRaises(SystemExit):
                self.run_cli("alice", "chat", "bob", "hello")
        self.assertEqual([e for e in self.ls.events if e["room"] == "chat"], [])

    def test_first_message_opens_a_chat_and_later_ones_continue_it(self):
        self.online("alice", "bob")
        first = self.run_cli("alice", "chat", "bob", "hello")
        self.assertIn("chat opened with bob", first)
        second = self.run_cli("alice", "chat", "bob", "still there?")
        self.assertIn("sent to bob", second)
        types = [e["type"] for e in self.ls.events if e["room"] == "chat"]
        self.assertEqual(types, ["chat.invite", "chat.message"])
        self.assertEqual(len({e["correlation_id"] for e in self.ls.events if e["room"] == "chat"}), 1)

    def test_chat_refuses_an_offline_peer_unless_forced(self):
        self.online("alice")
        self.stale_presence("ghost")
        with self.assertRaises(SystemExit):
            self.run_cli("alice", "chat", "ghost", "anyone?")
        self.assertIn("chat opened", self.run_cli("alice", "chat", "ghost", "anyone?", "--force"))
        # A peer never seen anywhere has no host to seal to: --force cannot help.
        with self.assertRaises(SystemExit) as ctx:
            self.run_cli("alice", "chat", "phantom", "anyone?", "--force")
        self.assertIn("never seen online", str(ctx.exception))

    def test_the_opener_ends_the_chat_for_everyone(self):
        self.online("alice", "bob", "carol")
        self.run_cli("alice", "chat", "bob,carol", "hi")
        self.run_cli("bob", "reply", "hi back")
        self.assertIn("closed for everyone", self.run_cli("alice", "end", "thanks"))
        for who in ("alice", "bob", "carol"):
            with self.as_agent(who):
                st = next(iter(a2a.my_chats().values()))
            self.assertEqual(st["state"], "closed", who)
            self.assertFalse(st["forced"])
            self.assertEqual(st["closed_by"], "alice")
        self.assertHumanEvent("alice closed", self.run_cli("bob", "inbox"))
        with self.as_agent("bob"):
            cid = next(iter(a2a.my_chats()))
            late = self.raw_append("chat.message", a2a.ROOM_CHAT, to=cid, correlation_id=cid, body="after the end", status="open")
            self.assertEqual(a2a.my_chats()[cid]["routes"][late["id"]], [], "nothing happens in a closed chat")
        # The opener's close is consumed once: a second one is nothing.
        with self.as_agent("alice"):
            before = a2a.my_chats()[cid]
            again = self.raw_append("chat.closed", a2a.ROOM_CHAT, to=cid, correlation_id=cid, body="again", status="superseded", metadata={"lifecycle": "hosted", "close": "opener"})
            after = a2a.my_chats()[cid]
        self.assertEqual(after["routes"][again["id"]], [], "a second marked close reaches nobody")
        self.assertEqual(after["last"], before["last"], "and moves no clock")
        self.assertEqual(after["closed_by"], "alice")

    def test_non_opener_end_leaves_only_their_side_even_in_a_pair(self):
        self.online("alice", "bob")
        self.run_cli("alice", "chat", "bob", "hi")
        self.run_cli("bob", "reply", "hi back")
        self.assertIn("left", self.run_cli("bob", "end", "done"))
        with self.as_agent("alice"):
            cid, st = next(iter(a2a.my_chats().items()))
        self.assertEqual(st["state"], "open")
        self.assertEqual(st["left"], {"bob"})
        self.assertEqual(st["agreed"], set())
        self.assertIsNone(st["close_cause"])
        self.assertIn("closed for everyone", self.run_cli("alice", "end", cid))
        with self.as_agent("bob"):
            st = a2a.my_chats()[cid]
            self.assertEqual(a2a.unread(), [])
            self.assertEqual(a2a.ledger("bob")["chats"][0]["state"], "left")
        self.assertEqual((st["state"], st["closed_by"]), ("closed", "alice"))
        close = next(e for e in self.ls.events if e["type"] == "chat.closed")
        self.assertEqual(st["routes"][close["id"]], [])
        self.assertFalse(any(e["type"] in ("chat.close-request", "chat.close-ack") for e in self.ls.events))

    def test_legacy_force_close_needs_ten_minutes_of_silence(self):
        cid = self.legacy_chat("alice", "bob")
        self.append_chat("bob", "chat.close-request", cid, at="2026-09-05T01:00:00Z")
        early = self.append_chat("bob", "chat.closed", cid, at="2026-09-05T01:09:59Z")
        before = a2a.all_chats()[cid]
        self.assertEqual(before["state"], "closing")
        self.assertEqual(before["routes"][early["id"]], [])
        self.append_chat("bob", "chat.closed", cid, at="2026-09-05T01:10:00Z")
        st = a2a.all_chats()[cid]
        self.assertEqual((st["state"], st["close_cause"], st["closed_by"]), ("closed", "forced", "bob"))
        self.assertTrue(st["forced"])

    def test_mark_read_acknowledges_only_the_named_events(self):
        self.online("alice", "bob")
        self.run_cli("alice", "chat", "bob", "one")
        self.run_cli("alice", "chat", "bob", "two")
        with self.as_agent("bob"):
            first, second = a2a.unread()
        self.assertIn("1 marked read", self.run_cli("bob", "mark-read", first["id"]))
        with self.as_agent("bob"):
            self.assertEqual([e["id"] for e in a2a.unread()], [second["id"]])

    def test_declining_an_invitation_leaves_only_the_declining_member(self):
        self.online("alice", "bob", "carol")
        self.run_cli("alice", "chat", "bob,carol", "hi")
        self.run_cli("bob", "decline", "busy")
        with self.as_agent("alice"):
            cid, st = next(iter(a2a.my_chats().items()))
        self.assertEqual(st["state"], "invited")
        self.assertEqual(st["left"], {"bob"})
        self.assertIsNone(st["close_cause"])
        self.assertEqual(a2a.ledger("bob")["chats"][0]["state"], "left")
        self.assertEqual(a2a.ledger("carol")["chats"][0]["state"], "invited")
        self.run_cli("carol", "accept", cid)
        self.assertEqual(a2a.all_chats()[cid]["state"], "open")

    # ── rooms ──────────────────────────────────────────────────────────────

    def test_three_agents_share_one_chat(self):
        self.online("alice", "bob", "carol")
        out = self.run_cli("alice", "chat", "bob,carol", "who owns the proxy port?")
        self.assertIn("chat opened with bob, carol", out)
        self.assertHumanEvent("alice opened a chat with bob, carol", self.run_cli("bob", "inbox"))
        self.assertHumanEvent("alice opened a chat with bob, carol", self.run_cli("carol", "inbox"))
        self.assertIn("sent to alice, carol", self.run_cli("bob", "chat", "alice,carol", "4318"))
        self.assertHumanEvent("bob: 4318", self.run_cli("carol", "inbox"))
        self.assertHumanEvent("bob: 4318", self.run_cli("alice", "inbox"))
        with self.as_agent("alice"):
            st = next(iter(a2a.my_chats().values()))
        self.assertEqual(st["members"], ["alice", "bob", "carol"])
        self.assertEqual(st["state"], "open")

    def test_courier_fans_out_per_recipient_with_named_acks(self):
        self.online("alice", "bob", "carol")
        self.run_cli("alice", "chat", "bob,carol", "hello both")
        delivered = []
        with mock.patch.object(a2a, "deliver", side_effect=lambda e, t: delivered.append(t["handle"]) or ("tmux" if t["handle"] == "bob" else "uncertain")), mock.patch.object(a2a, "_age", return_value=1):
            state = a2a.courier_once({}, "hostA", verbose=False)
        self.assertEqual(sorted(delivered), ["bob", "carol"])
        acks = sorted(a2a.ack_note(a) for a in self.ls.events if a["type"] == "event.ack")
        self.assertEqual(acks, ["delivered to bob via tmux", "uncertain for carol"])
        self.assertEqual(state["pending"], {})
        with self.as_agent("bob"):
            self.assertEqual(a2a.unread(), [], "delivered counts as seen for bob")
        with self.as_agent("carol"):
            self.assertEqual(len(a2a.unread()), 1, "uncertain does not count as seen for carol")

    def test_every_other_member_can_leave_without_ending_the_openers_chat(self):
        self.online("alice", "bob", "carol", "dave")
        self.run_cli("alice", "chat", "bob,carol,dave", "quick sync")
        with self.as_agent("alice"):
            cid = a2a.open_chat_with("bob", "carol", "dave")
        self.assertIn("dave is already in the chat", self.run_cli("alice", "add", cid, "dave"))
        for member in ("bob", "carol", "dave"):
            self.assertIn("left", self.run_cli(member, "end", cid))
            st = a2a.all_chats()[cid]
            self.assertIn(st["state"], ("invited", "open"))
            self.assertIsNone(st["close_cause"])
        self.assertEqual(st["left"], {"bob", "carol", "dave"})
        self.assertIn("closed for everyone", self.run_cli("alice", "end", cid))
        st = a2a.all_chats()[cid]
        self.assertEqual((st["state"], st["close_cause"], st["closed_by"]), ("closed", "opener", "alice"))
        self.assertEqual([e["type"] for e in a2a.chat_events(cid)], ["chat.invite", "chat.leave", "chat.leave", "chat.leave", "chat.closed"])

    def test_a_member_can_add_another_who_then_receives_messages(self):
        self.online("alice", "bob", "carol")
        self.run_cli("alice", "chat", "bob", "start")
        cid = None
        with self.as_agent("alice"):
            cid = a2a.open_chat_with("bob")
        self.assertIn("added carol", self.run_cli("bob", "add", cid, "carol"))
        self.assertHumanEvent("bob added carol", self.run_cli("alice", "inbox"))
        self.run_cli("alice", "chat", "bob,carol", "welcome carol")
        self.assertHumanEvent("alice: welcome carol", self.run_cli("carol", "inbox"))
        with self.as_agent("alice"):
            self.assertEqual(a2a.open_chat_with("bob", "carol"), cid, "the pair became the room; no second chat")

    def test_a_pair_opened_before_rooms_still_works(self):
        self.online("alice", "bob")
        with self.as_agent("alice"):
            self.raw_append("chat.invite", a2a.ROOM_CHAT, to="bob", correlation_id="chat-legacy1", body="old style", status="open")
        self.assertHumanEvent("alice opened a chat with bob: old style", self.run_cli("bob", "inbox"))
        self.assertIn("sent to alice", self.run_cli("bob", "chat", "alice", "still here"))
        self.assertHumanEvent("bob: still here", self.run_cli("alice", "inbox"))
        delivered = []
        with self.as_agent("alice"):
            self.raw_append("chat.message", a2a.ROOM_CHAT, to="bob", correlation_id="chat-legacy1", body="handle-addressed", status="open")
        with mock.patch.object(a2a, "deliver", side_effect=lambda e, t: delivered.append(t["handle"]) or "tmux"), mock.patch.object(a2a, "_age", return_value=1):
            a2a.courier_once({}, "hostA", verbose=False)
        self.assertEqual(delivered, ["bob"], "a handle-addressed event has exactly that recipient")

    def test_message_text_beginning_with_a_handle_is_not_a_recipient(self):
        self.online("alice", "bob", "carol")
        self.run_cli("alice", "chat", "bob", "carol", "said", "she", "is", "out")
        self.assertHumanEvent("alice opened a chat with bob: carol said she is out", self.run_cli("bob", "inbox"))
        self.assertIn("no unread", self.run_cli("carol", "inbox"), "carol is not a recipient")

    def test_new_writers_mark_every_chat_event_as_hosted(self):
        self.online("alice", "bob", "carol")
        self.run_cli("alice", "chat", "bob", "hi")
        with self.as_agent("alice"):
            cid = a2a.open_chat_with("bob")
        self.run_cli("bob", "accept", cid)
        self.run_cli("bob", "reply", cid, "hi back")
        self.run_cli("alice", "add", cid, "carol")
        self.run_cli("carol", "decline", cid)
        self.run_cli("bob", "leave", cid)
        self.run_cli("alice", "end", cid)
        events = a2a.chat_events(cid)
        self.assertEqual([e["type"] for e in events], ["chat.invite", "chat.accept", "chat.message", "chat.join", "chat.decline", "chat.leave", "chat.closed"])
        self.assertTrue(all(a2a.metadata_of(e).get("lifecycle") == "hosted" for e in events))
        self.assertEqual([e["to_agent"] for e in events[:3]], ["bob", "alice", "alice"])
        self.assertEqual(a2a.metadata_of(events[-1]).get("close"), "opener")
        self.assertNotIn("proposal", a2a.metadata_of(events[-1]))

    def test_a_room_is_addressed_to_the_chat_id_and_a_chat_dash_handle_is_a_member(self):
        self.online("alice", "chat-foo", "bob")
        self.run_cli("alice", "chat", "chat-foo,bob", "room with an awkward handle")
        invite = next(e for e in self.ls.events if e["type"] == "chat.invite")
        self.assertEqual(invite["to_agent"], invite["correlation_id"])
        with self.as_agent("alice"):
            st = next(iter(a2a.my_chats().values()))
        self.assertEqual(st["members"], ["alice", "chat-foo", "bob"])
        self.assertHumanEvent("alice opened a chat", self.run_cli("chat-foo", "inbox"))

    def test_only_the_legacy_proposer_can_force_close(self):
        cid = self.legacy_chat("alice", "bob", "carol")
        self.append_chat("bob", "chat.close-request", cid, at="2026-09-05T01:00:00Z")
        self.append_chat("carol", "chat.close-ack", cid, at="2026-09-05T01:00:00Z")
        rejected = self.append_chat("carol", "chat.closed", cid, at="2026-09-05T01:10:00Z")
        self.assertEqual(a2a.all_chats()[cid]["routes"][rejected["id"]], [])
        self.assertEqual(a2a.all_chats()[cid]["state"], "closing")
        self.append_chat("bob", "chat.closed", cid, at="2026-09-05T01:10:00Z")
        self.assertEqual(a2a.all_chats()[cid]["close_cause"], "forced")

    def test_wait_does_not_spin_on_traffic_that_is_not_mine(self):
        self.online("alice", "bob", "carol", "dave")
        self.run_cli("carol", "chat", "dave", "not for alice")
        calls = []
        real_wait = a2a.wait_events

        def counting_wait(timeout_ms, **filters):
            calls.append(filters.get("since_event_id"))
            if len(calls) >= 3:
                self.run_cli("bob", "chat", "alice", "now for alice")
            return real_wait(timeout_ms, **filters)

        with mock.patch.object(a2a, "wait_events", side_effect=counting_wait), self.as_agent("alice"):
            events = a2a._wait(None, 5)
        self.assertEqual([a2a.open_body(e)[0] for e in events], ["now for alice"])
        unrelated = next(e for e in self.ls.events if a2a.open_body(e)[0] == "not for alice")
        self.assertEqual(calls[1], unrelated["id"], "the first pass moved the scan cursor past the traffic that was not hers")
        self.assertLess(len(calls), 6, "no re-reading of the same event")

    def test_delivery_only_writes_to_an_allowed_socket(self):
        self.online("alice")
        self.presence("bob", pane="%1")
        self.run_cli("alice", "chat", "bob", "hi", "--force")
        with mock.patch.dict(os.environ, {"A2A_TMUX_SOCKETS": "/isolated.sock"}), mock.patch.object(a2a, "_codex_queue", side_effect=AssertionError("must not write to a foreign socket")), mock.patch.object(a2a, "_age", return_value=1):
            state = a2a.courier_once({}, "hostA", verbose=False)
        self.assertEqual(len(state["pending"]), 1, "kept, not written")

    def test_courier_state_written_before_rooms_is_migrated(self):
        self.online("alice", "bob")
        self.run_cli("alice", "chat", "bob", "hi")
        invite = next(e for e in self.ls.events if e["type"] == "chat.invite")
        old = {"cursor": invite["id"], "pending": {invite["id"]: {"to": "bob", "chat": invite["correlation_id"], "attempts": 0, "next": 0}}}
        a2a.prime_cursor(old)
        self.assertEqual(old["cursor"], invite["id"], "an existing cursor is kept, not re-taken at the tip")
        self.run_cli("alice", "chat", "bob", "second")
        delivered = []
        with mock.patch.object(a2a, "deliver", side_effect=lambda e, t: delivered.append(a2a.open_body(e)[0]) or "tmux"), mock.patch.object(a2a, "_age", return_value=1):
            state = a2a.courier_once(old, "hostA", verbose=False)
        self.assertEqual(sorted(delivered), ["hi", "second"], "the old item and the newer event both delivered")
        self.assertEqual(state["pending"], {})

    def test_a_failed_uncertain_ack_is_retried_without_a_paste(self):
        self.online("alice", "bob")
        self.run_cli("alice", "chat", "bob", "hi")
        pastes = []
        real = self.ls.call
        fail = {"on": True}

        def flaky(tool, args):
            if tool == "mempalace_event_ack" and fail["on"] and args["from_agent"].startswith("courier@"):
                raise a2a.LogstreamError("writer busy")
            return real(tool, args)

        with mock.patch.object(a2a, "_rpc", side_effect=lambda tool, args, timeout=30.0: flaky(tool, args)), mock.patch.object(a2a, "_codex_queue", side_effect=lambda *a: pastes.append(a) or "uncertain"), mock.patch.object(a2a, "_age", return_value=1):
            state = a2a.courier_once({}, "hostA", verbose=False)
            self.assertEqual(len(state["pending"]), 1, "the uncertain outcome is still owed to the ledger")
            for item in state["pending"].values():
                item["next"] = 0
            state = a2a.courier_once(state, "hostA", verbose=False)
            fail["on"] = False
            for item in state["pending"].values():
                item["next"] = 0
            state = a2a.courier_once(state, "hostA", verbose=False)
        self.assertEqual(len(pastes), 1)
        self.assertEqual(state["pending"], {})
        self.assertEqual([a2a.ack_note(a) for a in self.ls.events if a["type"] == "event.ack"], ["uncertain for bob"])
        # the same after a restart from an in-flight marker
        self.run_cli("alice", "chat", "bob", "again")
        latest = [e for e in self.ls.events if e["type"] == "chat.message"][-1]
        crashed = {"primed": True, "cursor": latest["id"], "pending": {f"{latest['id']}|bob": {"event": latest["id"], "to": "bob", "chat": latest["correlation_id"], "attempts": 1, "next": 0, "inflight": True}}}
        fail["on"] = True
        with mock.patch.object(a2a, "_rpc", side_effect=lambda tool, args, timeout=30.0: flaky(tool, args)), mock.patch.object(a2a, "_codex_queue", side_effect=AssertionError("no paste after restart")), mock.patch.object(a2a, "_age", return_value=1):
            state = a2a.courier_once(crashed, "hostA", verbose=False)
            self.assertEqual(len(state["pending"]), 1)
            fail["on"] = False
            for item in state["pending"].values():
                item["next"] = 0
            state = a2a.courier_once(state, "hostA", verbose=False)
        self.assertEqual(state["pending"], {})
        self.assertEqual([a2a.ack_note(a) for a in self.ls.events if a["type"] == "event.ack"][-1], "uncertain for bob")

    def test_an_outsider_cannot_join_or_speak_in_a_room(self):
        self.online("alice", "bob", "carol", "mallory")
        self.run_cli("alice", "chat", "bob,carol", "private")
        with self.as_agent("alice"):
            cid = a2a.open_chat_with("bob", "carol")
        with self.as_agent("mallory"):
            self.raw_append("chat.message", a2a.ROOM_CHAT, to=cid, correlation_id=cid, body="let me in", status="open")
            self.raw_append("chat.join", a2a.ROOM_CHAT, to=cid, correlation_id=cid, status="claimed", metadata={"member": "mallory"})
            self.raw_append("chat.close-request", a2a.ROOM_CHAT, to=cid, correlation_id=cid, status="ready")
            self.assertEqual(a2a.my_chats(), {}, "mallory is in no chat")
        with self.as_agent("alice"):
            st = a2a.my_chats()[cid]
        self.assertEqual(st["members"], ["alice", "bob", "carol"])
        self.assertEqual(st["state"], "invited", "mallory's close request did not touch the quorum")
        self.assertIn("no unread", self.run_cli("mallory", "inbox"))
        self.assertNotIn("let me in", self.run_cli("bob", "inbox"))
        delivered = []
        with mock.patch.object(a2a, "deliver", side_effect=lambda e, t: delivered.append((e["from_agent"], t["handle"])) or "tmux"), mock.patch.object(a2a, "_age", return_value=1):
            a2a.courier_once({}, "hostA", verbose=False)
        self.assertEqual([d for d in delivered if d[0] == "mallory"], [], "an outsider's event is delivered to nobody")

    def test_a_second_invite_from_an_outsider_reaches_nobody(self):
        self.online("alice", "bob", "mallory")
        self.run_cli("alice", "chat", "bob", "hi")
        with self.as_agent("alice"):
            cid = a2a.open_chat_with("bob")
        with self.as_agent("mallory"):
            self.raw_append("chat.invite", a2a.ROOM_CHAT, to=cid, correlation_id=cid, body="hijack", status="open", metadata={"members": ["mallory", "alice", "bob"]})
        with self.as_agent("alice"):
            st = a2a.my_chats()[cid]
        self.assertEqual(st["members"], ["alice", "bob"])
        self.assertNotIn("hijack", self.run_cli("bob", "inbox"))
        delivered = []
        with mock.patch.object(a2a, "deliver", side_effect=lambda e, t: delivered.append(a2a.open_body(e)[0]) or "tmux"), mock.patch.object(a2a, "_age", return_value=1):
            a2a.courier_once({}, "hostA", verbose=False)
        self.assertNotIn("hijack", delivered)

    def test_routing_follows_membership_at_the_time_of_each_event(self):
        self.online("alice", "bob", "carol", "dave")
        self.run_cli("alice", "chat", "bob,carol", "before dave")
        with self.as_agent("alice"):
            cid = a2a.open_chat_with("bob", "carol")
        self.run_cli("alice", "add", cid, "dave")
        self.run_cli("carol", "leave", "bye")
        self.run_cli("alice", "chat", "bob,dave", "after carol left")
        with self.as_agent("alice"):
            st = a2a.my_chats()[cid]
        by_body = {}
        for e in self.ls.events:
            if e.get("correlation_id") == cid and e["type"] != "event.ack":
                by_body[a2a.open_body(e)[0] or e["type"]] = st["routes"][e["id"]]
        self.assertEqual(by_body["before dave"], ["bob", "carol"], "dave was not a member yet")
        self.assertEqual(by_body["chat.join"], ["bob", "carol", "dave"], "the join notification reaches the newcomer too")
        self.assertEqual(by_body["bye"], ["alice", "bob", "dave"], "carol's leave goes to those who remain")
        self.assertEqual(by_body["after carol left"], ["bob", "dave"])
        dave = self.run_cli("dave", "inbox", "--peek")
        self.assertNotIn("before dave", dave)
        self.assertIn("after carol left", dave)
        carol = self.run_cli("carol", "inbox", "--peek")
        self.assertNotIn("after carol left", carol, "nothing after the leave reaches carol")
        with self.as_agent("alice"):
            before = next(r for r in a2a.ledger("alice")["chats"][0]["rows"] if r["text"] == "before dave")
        self.assertEqual(before["to"].get("carol"), "read", "leaving acknowledged what had reached carol")
        delivered = []
        with mock.patch.object(a2a, "deliver", side_effect=lambda e, t: delivered.append((a2a.open_body(e)[0] or e["type"], t["handle"])) or "tmux"), mock.patch.object(a2a, "_age", return_value=1):
            a2a.courier_once({}, "hostA", verbose=False)
        self.assertNotIn(("before dave", "dave"), delivered, "one courier batch does not deliver pre-join traffic to the newcomer")
        self.assertNotIn(("after carol left", "carol"), delivered, "nothing after the leave is delivered to the leaver")
        self.assertIn(("bye", "bob"), delivered, "the leave itself reaches those who remain")

    def test_a_pair_that_grows_into_a_room_tells_the_newcomer(self):
        self.online("alice", "bob", "carol")
        self.run_cli("alice", "chat", "bob", "just us")
        with self.as_agent("alice"):
            cid = a2a.open_chat_with("bob")
        self.run_cli("alice", "add", cid, "carol")
        with self.as_agent("alice"):
            st = a2a.my_chats()[cid]
        join = next(e for e in self.ls.events if e["type"] == "chat.join")
        self.assertEqual(join["to_agent"], cid, "the join is a room event even though the chat was a pair a moment ago")
        self.assertEqual(st["routes"][join["id"]], ["bob", "carol"])
        self.assertHumanEvent("alice added carol", self.run_cli("carol", "inbox"))
        self.run_cli("bob", "reply", cid, "hi carol")
        self.assertHumanEvent("bob: hi carol", self.run_cli("carol", "inbox"))

    def test_reply_to_a_departed_handle_never_targets_the_remaining_members(self):
        for peers in ("bob", "bob,carol"):
            with self.subTest(peers=peers):
                self.ls = FakeLogstream()
                self.register_project()
                self.online("alice", "bob", "carol")
                self.run_cli("alice", "chat", peers, "hi")
                cid = a2a.chat_events()[0]["correlation_id"]
                self.run_cli("bob", "leave", cid)
                if peers == "bob,carol":
                    self.run_cli("carol", "reply", cid, "unread for Alice")
                before = json.dumps(self.ls.events, sort_keys=True)
                with self.as_agent("alice"):
                    cursor = a2a.read_state(a2a.inbox_cursor_name())
                with self.assertRaises(SystemExit) as failure:
                    self.run_cli("alice", "reply", "bob", "for-bob-only")
                self.assertIn("bob", str(failure.exception))
                self.assertEqual(json.dumps(self.ls.events, sort_keys=True), before, "rejected addressing neither appends nor acknowledges")
                with self.as_agent("alice"):
                    self.assertEqual(a2a.read_state(a2a.inbox_cursor_name()), cursor)
                if peers == "bob,carol":
                    self.run_cli("alice", "reply", cid, "explicitly for this chat")
                    sent = a2a.chat_events(cid)[-1]
                    self.assertEqual(a2a.open_body(sent)[0], "explicitly for this chat")
                    self.assertEqual(a2a.all_chats()[cid]["routes"][sent["id"]], ["carol"])
                    self.run_cli("alice", "reply", "carol", "for-carol")
                    self.assertEqual(a2a.open_body(a2a.chat_events(cid)[-1])[0], "for-carol")
                else:
                    with self.assertRaises(SystemExit):
                        self.run_cli("alice", "reply", cid, "nobody remains")

    def test_reply_without_another_participant_refuses_without_side_effects(self):
        for peers in ("bob", "bob,carol"):
            with self.subTest(peers=peers), mock.patch.object(a2a, "_age", return_value=0):
                self.ls = FakeLogstream()
                self.online("alice", "bob", "carol", "dave")
                self.run_cli("alice", "chat", peers, "original invitation")
                cid = a2a.chat_events()[0]["correlation_id"]
                for peer in peers.split(","):
                    self.run_cli(peer, "leave", cid)
                before = a2a.all_chats()[cid]
                events = json.dumps(self.ls.events, sort_keys=True)
                book = a2a.ledger("alice")
                for argv in (["reply", cid, "no recipient"], ["reply", "nowhere", "to send"]):
                    out = io.StringIO()
                    with self.as_agent("alice"), redirect_stdout(out), self.assertRaises(SystemExit):
                        a2a.main(argv)
                    self.assertNotIn("sent", out.getvalue())
                    self.assertEqual(json.dumps(self.ls.events, sort_keys=True), events)
                    self.assertEqual(a2a.all_chats()[cid], before)
                    self.assertEqual(a2a.ledger("alice"), book)
                self.assertIn(cid, self.run_cli("alice", "status"))
                self.assertIn("original invitation", self.run_cli("alice", "read", cid))
                self.run_cli("alice", "add", cid, "dave")
                self.assertIn("sent to dave", self.run_cli("alice", "reply", cid, "hello Dave"))
                message = a2a.chat_events(cid)[-1]
                self.assertEqual((message["type"], a2a.open_body(message)[0]), ("chat.message", "hello Dave"))
                self.assertEqual(a2a.all_chats()[cid]["routes"][message["id"]], ["dave"])
                self.run_cli("dave", "leave", cid)
                self.assertIn("closed for everyone", self.run_cli("alice", "end", cid))
                st = a2a.all_chats()[cid]
                self.assertEqual(st["state"], "closed")
                self.assertEqual(st["routes"][a2a.chat_events(cid)[-1]["id"]], [])

    def test_a_chat_losing_its_recipient_or_ending_during_acknowledgement_rejects_the_message(self):
        for command, change in (("reply", "leave"), ("chat", "leave"), ("reply", "end"), ("chat", "end")):
            with self.subTest(command=command, change=change):
                self.ls = FakeLogstream()
                self.online("alice", "bob")
                self.run_cli("alice", "chat", "bob", "hi")
                cid = a2a.chat_events()[0]["correlation_id"]
                original_acknowledge = a2a.acknowledge_chat
                changed = False

                def change_before_acknowledging(chat):
                    nonlocal changed
                    if a2a.me() == "alice" and not changed:
                        changed = True
                        self.run_cli("bob" if change == "leave" else "alice", change, chat)
                    return original_acknowledge(chat)

                out = io.StringIO()
                with mock.patch.object(a2a, "acknowledge_chat", side_effect=change_before_acknowledging), self.as_agent("alice"), redirect_stdout(out):
                    with self.assertRaises(SystemExit):
                        a2a.main([command, cid if command == "reply" else "bob", "lost audience"])
                self.assertTrue(changed)
                self.assertNotIn("sent", out.getvalue())
                self.assertFalse(any(e["type"] == "chat.message" for e in a2a.chat_events(cid)))
                st = a2a.all_chats()[cid]
                self.assertEqual(st["messages"], 0)
                self.assertEqual(st["left"], {"bob"} if change == "leave" else set())
                self.assertEqual(st["state"], "invited" if change == "leave" else "closed")

    def test_a_message_written_a_moment_before_its_last_recipient_leaves_reaches_them(self):
        """The fold orders sealed events by the moment each was written,
        not by the relay's append order: a message sealed just before bob's
        leave landed on the relay still precedes it, so it reaches bob,
        who left afterwards, and the command reports what the fold says.
        Once the departure is visible, the next reply is refused."""
        for command in ("reply", "chat"):
            with self.subTest(command=command), mock.patch.object(a2a, "_age", return_value=0):
                self.ls = FakeLogstream()
                self.online("alice", "bob")
                self.run_cli("alice", "chat", "bob", "hi")
                cid = a2a.chat_events()[0]["correlation_id"]
                interleaved = False

                def append_after_departure(tool, args, timeout=30.0):
                    nonlocal interleaved
                    if tool == "mempalace_event_append" and args["type"] == "chat.message" and not interleaved:
                        interleaved = True
                        self.run_cli("bob", "leave", cid)
                    return self.ls.call(tool, args)

                with mock.patch.object(a2a, "_rpc", side_effect=append_after_departure):
                    out = self.run_cli("alice", command, cid if command == "reply" else "bob", "just in time")
                self.assertTrue(interleaved)
                self.assertIn("sent to bob", out)
                messages = [e for e in a2a.chat_events(cid) if e["type"] == "chat.message"]
                self.assertEqual(len(messages), 1)
                st = a2a.all_chats()[cid]
                self.assertEqual(st["routes"][messages[0]["id"]], ["bob"])
                self.assertEqual(st["left"], {"bob"})
                self.assertEqual([e["type"] for e in a2a.chat_events(cid)], ["chat.invite", "chat.message", "chat.leave"], "the fold's order is the moments' order")
                self.assertIn("just in time", self.run_cli("bob", "read", cid))
                with self.assertRaises(SystemExit):
                    self.run_cli("alice", "reply", cid, "too late")
                self.assertEqual(len([e for e in a2a.chat_events(cid) if e["type"] == "chat.message"]), 1)

    def test_a_message_accepted_before_departure_still_reports_success(self):
        for command in ("reply", "chat"):
            with self.subTest(command=command):
                self.ls = FakeLogstream()
                self.online("alice", "bob")
                self.run_cli("alice", "chat", "bob", "hi")
                cid = a2a.chat_events()[0]["correlation_id"]
                interleaved = False

                def leave_after_append(tool, args, timeout=30.0):
                    nonlocal interleaved
                    result = self.ls.call(tool, args)
                    if tool == "mempalace_event_append" and args["type"] == "chat.message" and not interleaved:
                        interleaved = True
                        self.run_cli("bob", "leave", cid)
                    return result

                with mock.patch.object(a2a, "_rpc", side_effect=leave_after_append):
                    output = self.run_cli("alice", command, cid if command == "reply" else "bob", "accepted before leaving")
                self.assertTrue(interleaved)
                self.assertIn("sent to bob", output)
                st = a2a.all_chats()[cid]
                message = next(e for e in a2a.chat_events(cid) if e["type"] == "chat.message")
                self.assertEqual(st["routes"][message["id"]], ["bob"])
                self.assertEqual(st["messages"], 1)
                self.assertEqual(st["left"], {"bob"})
                self.assertIn("accepted before leaving", self.run_cli("bob", "read", cid))

    def test_message_confirmation_failure_reports_unknown_acceptance_without_retrying(self):
        for command, confirmation in (("reply", "error"), ("chat", "error"), ("reply", "stale"), ("chat", "stale")):
            with self.subTest(command=command, confirmation=confirmation):
                self.ls = FakeLogstream()
                self.online("alice", "bob")
                self.run_cli("alice", "chat", "bob", "hi")
                cid = a2a.chat_events()[0]["correlation_id"]
                committed = False

                def fail_confirmation(tool, args, timeout=30.0):
                    nonlocal committed
                    if committed and tool == "mempalace_event_list" and args.get("room") == a2a.ROOM_CHAT:
                        if confirmation == "error":
                            raise a2a.LogstreamError("confirmation unavailable")
                        return {"events": [e for e in self.ls._list(args) if e["type"] != "chat.message"]}
                    result = self.ls.call(tool, args)
                    if tool == "mempalace_event_append" and args["type"] == "chat.message":
                        committed = True
                    return result

                out = io.StringIO()
                with mock.patch.object(a2a, "_rpc", side_effect=fail_confirmation), self.as_agent("alice"), redirect_stdout(out):
                    with self.assertRaises(SystemExit) as failure:
                        a2a.main([command, cid if command == "reply" else "bob", "already appended"])
                self.assertTrue(committed)
                self.assertNotIn("sent", out.getvalue())
                self.assertIn("could not be confirmed", " ".join(str(failure.exception).split()))
                self.assertIn(f"a2a read {cid}", str(failure.exception))
                self.assertNotIn("nothing sent", str(failure.exception))
                self.assertNotIn("not accepted", str(failure.exception))
                messages = [e for e in a2a.chat_events(cid) if e["type"] == "chat.message"]
                self.assertEqual(len(messages), 1)
                self.assertEqual(a2a.all_chats()[cid]["routes"][messages[0]["id"]], ["bob"])
                self.assertIn("already appended", self.run_cli("alice", "read", cid))

    def test_named_peer_leaving_during_reply_does_not_redirect_the_message(self):
        self.online("alice", "bob", "carol")
        self.run_cli("alice", "chat", "bob,carol", "hi")
        cid = a2a.chat_events()[0]["correlation_id"]
        original_acknowledge = a2a.acknowledge_chat
        left = False

        def leave_before_acknowledging(chat):
            nonlocal left
            if a2a.me() == "alice" and not left:
                left = True
                self.run_cli("bob", "leave", chat)
            return original_acknowledge(chat)

        with mock.patch.object(a2a, "acknowledge_chat", side_effect=leave_before_acknowledging):
            with self.assertRaises(SystemExit) as failure:
                self.run_cli("alice", "reply", "bob", "for-bob-only")
        self.assertTrue(left)
        self.assertIn("bob", str(failure.exception))
        self.assertEqual(a2a.all_chats()[cid]["left"], {"bob"})
        self.assertFalse(any(e["type"] == "chat.message" for e in a2a.chat_events(cid)))

    def test_handle_resolution_prefers_a_chat_where_the_peer_still_participates(self):
        self.online("alice", "bob", "carol")
        self.run_cli("alice", "chat", "bob,carol", "original room")
        old = a2a.chat_events()[0]["correlation_id"]
        self.run_cli("bob", "leave", old)
        self.run_cli("bob", "chat", "alice", "separate conversation")
        fresh = next(e["correlation_id"] for e in a2a.chat_events() if a2a.open_body(e)[0] == "separate conversation")
        self.run_cli("alice", "reply", "bob", "for Bob here")
        sent = a2a.chat_events(fresh)[-1]
        self.assertEqual(a2a.open_body(sent)[0], "for Bob here")
        self.assertEqual(a2a.all_chats()[fresh]["routes"][sent["id"]], ["bob"])
        self.assertFalse(any(a2a.open_body(e)[0] == "for Bob here" for e in a2a.chat_events(old)))

    def test_historical_peer_handles_do_not_become_text_in_an_unrelated_chat(self):
        for prior in ("left", "closed"):
            with self.subTest(prior=prior):
                self.ls = FakeLogstream()
                self.register_project()
                self.online("alice", "bob", "carol")
                self.run_cli("bob", "chat", "alice", "old chat")
                old = a2a.chat_events()[0]["correlation_id"]
                self.run_cli("alice" if prior == "left" else "bob", "leave" if prior == "left" else "end", old)
                self.run_cli("carol", "chat", "alice", "unrelated unread")
                before = json.dumps(self.ls.events, sort_keys=True)
                with self.as_agent("alice"):
                    cursor = a2a.read_state(a2a.inbox_cursor_name())
                with self.assertRaises(SystemExit) as failure:
                    self.run_cli("alice", "reply", "bob", "only for Bob")
                self.assertIn("bob", str(failure.exception))
                self.assertEqual(json.dumps(self.ls.events, sort_keys=True), before)
                with self.as_agent("alice"):
                    self.assertEqual(a2a.read_state(a2a.inbox_cursor_name()), cursor)
                self.run_cli("alice", "reply", "thanks", "for helping")
                self.assertEqual(a2a.open_body(a2a.chat_events()[-1])[0], "thanks for helping", "unrecognized first words remain message shorthand")
                self.assertNotEqual(a2a.chat_events()[-1]["correlation_id"], old)

    def test_the_wake_line_reply_keeps_the_room_together(self):
        self.online("alice", "bob", "carol")
        self.run_cli("alice", "chat", "bob,carol", "room")
        invite = next(e for e in self.ls.events if e["type"] == "chat.invite")
        line = a2a.wake_line(invite)
        self.assertIn(f"a2a reply {invite['correlation_id']}", line)
        self.assertNotIn("a2a chat alice", line)
        self.assertIn("sent to alice, carol", self.run_cli("bob", "reply", invite["correlation_id"], "here"))
        self.assertEqual(len({e["correlation_id"] for e in self.ls.events if e["type"].startswith("chat.")}), 1, "one ledger, no fork")
        self.assertHumanEvent("bob: here", self.run_cli("carol", "inbox"))

    def test_a_member_cannot_address_a_room_event_to_an_outsider(self):
        self.online("alice", "bob", "carol", "mallory")
        self.run_cli("alice", "chat", "bob,carol", "members only")
        with self.as_agent("alice"):
            cid = a2a.open_chat_with("bob", "carol")
            self.raw_append("chat.message", a2a.ROOM_CHAT, to="mallory", correlation_id=cid, body="leaked?", status="open")
            st = a2a.my_chats()[cid]
        leaked = next(e for e in self.ls.events if a2a.open_body(e)[0] == "leaked?")
        self.assertEqual(st["routes"][leaked["id"]], [])
        self.assertIn("no unread", self.run_cli("mallory", "inbox"))
        self.assertNotIn("leaked?", self.run_cli("bob", "inbox"))

    def test_membership_events_are_delivered_as_readable_wake_lines(self):
        self.online("alice", "bob", "carol")
        self.run_cli("alice", "chat", "bob", "hi")
        with self.as_agent("alice"):
            cid = a2a.open_chat_with("bob")
        self.run_cli("alice", "add", cid, "carol")
        self.run_cli("carol", "leave", "later")
        join = next(e for e in self.ls.events if e["type"] == "chat.join")
        leave = next(e for e in self.ls.events if e["type"] == "chat.leave")
        self.assertIn(f"alice added carol to chat {cid}", a2a.wake_line(join))
        self.assertIn(f"a2a reply {cid}", a2a.wake_line(join))
        self.assertIn(f"carol left chat {cid}: later", a2a.wake_line(leave))
        pasted = []
        with mock.patch.object(a2a, "_codex_queue", side_effect=lambda thread, text: pasted.append(text) or "codex-queue"), mock.patch.object(a2a, "_age", return_value=1):
            a2a.courier_once({}, "hostA", verbose=False)
        self.assertTrue(any("added carol" in t for t in pasted), "the delivered prompt itself says what happened")
        self.assertFalse(any(": — reply" in t for t in pasted), "no empty prompt")

    def test_hosted_chats_reject_votes_forces_and_unauthorized_closes(self):
        self.online("alice", "bob", "carol")
        self.run_cli("alice", "chat", "bob,carol", "hi")
        with self.as_agent("alice"):
            cid = a2a.open_chat_with("bob", "carol")
        before = a2a.all_chats()[cid]
        for sender, kind, metadata in [
            ("bob", "chat.close-request", {}),
            ("carol", "chat.close-ack", {"lifecycle": "hosted"}),
            ("bob", "chat.closed", {}),
            ("alice", "chat.closed", {}),
            ("alice", "chat.closed", {"close": "opener"}),
            ("alice", "chat.closed", {"lifecycle": "hosted"}),
            ("bob", "chat.closed", {"close": "opener", "lifecycle": "hosted"}),
            ("mallory", "chat.closed", {"close": "opener", "lifecycle": "hosted"}),
        ]:
            with self.subTest(sender=sender, kind=kind, metadata=metadata):
                rejected = self.append_chat(sender, kind, cid, metadata=metadata, at="2026-09-06T01:00:00Z")
                after = a2a.all_chats()[cid]
                self.assertEqual(after["routes"][rejected["id"]], [])
                self.assertEqual({k: v for k, v in after.items() if k != "routes"},
                                 {k: v for k, v in before.items() if k != "routes"})

    def test_a_rejected_legacy_close_does_not_move_the_silence_clock(self):
        cid = self.legacy_chat("alice", "bob", "carol")
        self.append_chat("bob", "chat.close-request", cid, at="2026-09-05T01:00:00Z")
        forged = self.append_chat("carol", "chat.closed", cid, at="2026-09-05T01:09:59Z")
        forced = self.append_chat("bob", "chat.closed", cid, at="2026-09-05T01:10:00Z")
        st = a2a.all_chats()[cid]
        self.assertEqual(st["state"], "closed")
        self.assertEqual(st["routes"][forged["id"]], [])
        self.assertEqual(st["routes"][forced["id"]], ["alice", "carol"])

    def test_an_oversized_member_handle_stays_a_bounded_wake_line(self):
        self.online("alice", "bob")
        self.run_cli("alice", "chat", "bob", "hi")
        with self.as_agent("alice"):
            cid = a2a.open_chat_with("bob")
        with self.assertRaises(SystemExit):
            self.run_cli("alice", "add", cid, "x" * 200)
        self.assertEqual([e for e in self.ls.events if e["type"] == "chat.join"], [], "refused at ingress")
        with self.as_agent("alice"):
            self.raw_append("chat.join", a2a.ROOM_CHAT, to=cid, correlation_id=cid, status="claimed", metadata={"member": "y" * 10000})
        join = next(e for e in self.ls.events if e["type"] == "chat.join")
        line = a2a.wake_line(join)
        self.assertLess(len(line), 300, "a forged member name cannot inflate the wake line")
        self.assertNotIn("\n", line)

    def test_the_ledger_ignores_events_after_the_end_and_legacy_joins_while_closing(self):
        self.online("alice", "bob", "carol", "dave")
        self.run_cli("alice", "chat", "bob", "hi")
        with self.as_agent("alice"):
            cid = a2a.open_chat_with("bob")
        self.run_cli("alice", "end", cid)
        late = self.append_chat("alice", "chat.message", cid, body="after the end")
        st = a2a.all_chats()[cid]
        self.assertEqual(st["state"], "closed")
        self.assertEqual(st["routes"][late["id"]], [])
        self.assertEqual(st["messages"], 0)
        self.assertNotIn("after the end", self.run_cli("bob", "inbox"))
        delivered = []
        with mock.patch.object(a2a, "deliver", side_effect=lambda e, t: delivered.append(a2a.open_body(e)[0]) or "tmux"), mock.patch.object(a2a, "_age", return_value=1):
            a2a.courier_once({}, "hostA", verbose=False)
        self.assertNotIn("after the end", delivered)
        room = self.legacy_chat("alice", "bob", "carol")
        self.append_chat("carol", "chat.close-request", room)
        join = self.append_chat("bob", "chat.join", room, metadata={"member": "dave"})
        st = a2a.all_chats()[room]
        self.assertEqual(st["members"], ["alice", "bob", "carol"])
        self.assertEqual(st["routes"][join["id"]], [])
        self.assertIn("no unread", self.run_cli("dave", "inbox"))

    def test_commands_are_refused_after_leaving_or_closing(self):
        self.online("alice", "bob", "carol")
        self.run_cli("alice", "chat", "bob,carol", "hi")
        with self.as_agent("alice"):
            cid = a2a.open_chat_with("bob", "carol")
        self.run_cli("carol", "leave")
        with self.assertRaises(SystemExit) as stop:
            self.run_cli("carol", "reply", cid, "still here?")
        self.assertIn("not in this chat", str(stop.exception))
        self.assertEqual([e for e in self.ls.events if a2a.open_body(e)[0] == "still here?"], [], "nothing recorded")
        self.run_cli("bob", "end")
        with self.assertRaises(SystemExit) as stop:
            self.run_cli("bob", "add", cid, "dave")
        self.assertIn("not in this chat", str(stop.exception))
        self.run_cli("alice", "end")
        with self.assertRaises(SystemExit) as stop:
            self.run_cli("bob", "reply", cid, "one more")
        self.assertIn("closed", str(stop.exception))
        with self.assertRaises(SystemExit) as stop:
            self.run_cli("alice", "add", cid, "dave")
        self.assertIn("closed", str(stop.exception))
        self.assertEqual([e for e in self.ls.events if e["type"] == "chat.join"], [])

    def test_end_no_longer_accepts_force(self):
        self.online("alice", "bob")
        self.run_cli("alice", "chat", "bob", "hi")
        before = a2a.chat_events()
        for actor in ("alice", "bob"):
            with self.subTest(actor=actor), redirect_stdout(io.StringIO()), mock.patch("sys.stderr", new_callable=io.StringIO):
                with self.assertRaises(SystemExit):
                    self.run_cli(actor, "end", "--force")
        self.assertEqual(a2a.chat_events(), before)

    def test_no_rung_writes_to_a_foreign_socket_including_codex(self):
        self.online("alice")
        with self.as_agent("bob"):
            a2a.append("agent.online", a2a.ROOM_PRESENCE, to="*", status="ready", metadata=a2a.signed_presence("agent.online", {"handle": "bob", "harness": "codex", "host": "hostA", "project_id": self.project_id, "project_name": self.project_name, "native_session": "thread-1", "pane": {"socket": "/foreign.sock", "pane": "%1"}}))
        self.run_cli("alice", "chat", "bob", "hi", "--force")
        with mock.patch.object(a2a, "_codex_queue", side_effect=AssertionError("codex queue must not run for a foreign socket")), mock.patch.object(a2a, "_age", return_value=1):
            state = a2a.courier_once({}, "hostA", verbose=False)
        self.assertEqual(len(state["pending"]), 1)

    def test_a_death_between_ack_and_state_write_does_not_double_record(self):
        self.online("alice", "bob")
        self.run_cli("alice", "chat", "bob", "hi")
        durable = {}

        def persist(st):
            durable.clear()
            durable.update(json.loads(json.dumps(st)))

        with mock.patch.object(a2a, "_codex_queue", return_value="codex-queue"), mock.patch.object(a2a, "_age", return_value=1):
            a2a.courier_once({}, "hostA", verbose=False, persist=persist)  # returned state discarded: the courier died after acking
        self.assertEqual(next(iter(durable["pending"].values()))["outcome"], "delivered", "the settled outcome was persisted before the ack")
        for item in durable["pending"].values():
            item["next"] = 0
        with mock.patch.object(a2a, "_codex_queue", side_effect=AssertionError("no paste on restart")), mock.patch.object(a2a, "_age", return_value=1):
            state = a2a.courier_once(durable, "hostA", verbose=False, persist=persist)
        self.assertEqual(state["pending"], {})
        self.assertEqual([a2a.ack_note(a) for a in self.ls.events if a["type"] == "event.ack"], ["delivered to bob via codex-queue"], "one acknowledgement, not two")

    def test_migration_keeps_a_pre_room_delivered_marker(self):
        self.online("alice", "bob")
        self.run_cli("alice", "chat", "bob", "hi")
        invite = next(e for e in self.ls.events if e["type"] == "chat.invite")
        old = {"cursor": invite["id"], "pending": {invite["id"]: {"to": "bob", "chat": invite["correlation_id"], "attempts": 1, "next": 0, "inflight": True, "rung": "tmux"}}}
        with mock.patch.object(a2a, "_codex_queue", side_effect=AssertionError("no paste")), mock.patch.object(a2a, "_age", return_value=1):
            state = a2a.courier_once(old, "hostA", verbose=False)
        self.assertEqual(state["pending"], {})
        self.assertEqual([a2a.ack_note(a) for a in self.ls.events if a["type"] == "event.ack"], ["delivered to bob via tmux"], "an observed delivery is recorded as such, never as uncertain")

    # ── ledger ─────────────────────────────────────────────────────────────

    def test_ledger_reports_one_state_per_recipient(self):
        self.online("alice", "bob", "carol", "mallory")
        self.run_cli("alice", "chat", "bob,carol", "which port?")
        with self.as_agent("alice"):
            cid = a2a.open_chat_with("bob", "carol")
        invite = next(e for e in self.ls.events if e["type"] == "chat.invite")
        self.ls.call("mempalace_event_ack", {"event_id": invite["id"], "from_agent": "courier@hostA", "status": "ready", "body": a2a.ack_payload(invite["id"], "courier@hostA", "ready", "bob", "delivered to bob via tmux", scope=a2a.scope_of(invite))})
        self.ls.call("mempalace_event_ack", {"event_id": invite["id"], "from_agent": "courier@hostA", "status": "failed", "body": a2a.ack_payload(invite["id"], "courier@hostA", "failed", "carol", "uncertain for carol", scope=a2a.scope_of(invite))})
        self.run_cli("bob", "reply", cid, "4318")  # acting in the chat acknowledges what reached bob: read outranks delivered
        with self.as_agent("mallory"):
            self.raw_append("chat.message", a2a.ROOM_CHAT, to=cid, correlation_id=cid, body="not a member", status="open")
        with self.as_agent("alice"):
            book = a2a.ledger("alice")
        self.assertEqual(book["agent"], "alice")
        chat = book["chats"][0]
        self.assertEqual(chat["members"], ["alice", "bob", "carol"])
        self.assertEqual(chat["state"], "open")
        self.assertEqual([r["kind"] for r in chat["rows"]], ["message", "message"], "the outsider's event is not a row")
        row = chat["rows"][0]
        self.assertEqual(row["to"], {"bob": "read", "carol": "uncertain"})
        self.assertEqual(row["tag"], "a2a" + a2a.delivery_tag(invite))
        self.assertEqual(row["text"], "which port?")
        with self.as_agent("mallory"):
            self.assertEqual(a2a.ledger("mallory")["chats"], [], "an outsider has no ledger entry")

    def test_ledger_keeps_the_legacy_handshake_waiting_and_forced_states(self):
        self.online("alice", "bob", "carol")
        cid = self.legacy_chat("alice", "bob", "carol")
        self.append_chat("carol", "chat.leave", cid, body="bye", at="2026-09-05T01:00:00Z")
        self.append_chat("bob", "chat.close-request", cid, body="done", at="2026-09-05T01:00:00Z")
        chat = a2a.ledger("alice")["chats"][0]
        self.assertEqual(chat["state"], "closing")
        self.assertEqual([r["kind"] for r in chat["rows"]], ["message", "left"])
        hs = {h["member"]: h for h in chat["handshake"]}
        self.assertTrue(hs["bob"]["agreed"] and hs["bob"]["at"])
        self.assertFalse(hs["alice"]["agreed"])
        self.assertTrue(hs["carol"]["left"] and hs["carol"]["left_at"])
        self.assertEqual(chat["remaining"], ["alice", "bob"])
        self.append_chat("bob", "chat.closed", cid, at="2026-09-05T01:10:00Z")
        self.assertEqual(a2a.ledger("alice")["chats"][0]["state"], "forced")
        self.run_cli("alice", "chat", "bob", "anyone?", "--force")
        with mock.patch.object(a2a, "_age", return_value=0):
            fresh = next(c for c in a2a.ledger("alice")["chats"] if c["state"] in ("invited", "waiting"))
        self.assertEqual(fresh["state"], "invited")
        with mock.patch.object(a2a, "_age", return_value=a2a.WAITING_AFTER_S + 5):
            stale = next(c for c in a2a.ledger("alice")["chats"] if c["state"] in ("invited", "waiting"))
        self.assertEqual(stale["state"], "waiting")
        self.assertGreaterEqual(stale["waiting_s"], a2a.WAITING_AFTER_S)

    def test_ledger_index_is_content_free(self):
        self.online("alice", "bob", "carol")
        self.run_cli("alice", "chat", "bob", "the secret plan")
        self.run_cli("carol", "chat", "bob", "another secret", "--force")
        index = json.loads(self.run_cli("alice", "ledger", "--index", "--compact"))
        self.assertEqual(len(index["chats"]), 2)
        self.assertNotIn("secret", json.dumps(index))
        chat = next(c for c in index["chats"] if c["members"] == ["alice", "bob"])
        self.assertEqual(chat["state"], "invited")
        self.assertEqual(chat["rows"], 1)
        self.assertTrue(chat["last_row"].startswith("evt_"))

    def test_every_chat_action_acknowledges_what_reached_the_actor(self):
        for action in ("accept", "decline", "leave", "end", "reply", "read"):
            with self.subTest(action=action):
                self.ls = FakeLogstream()
                self.online("alice", "bob")
                self.run_cli("alice", "chat", "bob", "will you?")
                with self.as_agent("alice"):
                    cid = a2a.open_chat_with("bob")
                args = [action, cid] + (["sure"] if action == "reply" else [])
                self.run_cli("bob", *args)
                with self.as_agent("alice"):
                    row = a2a.ledger("alice")["chats"][0]["rows"][0]
                self.assertEqual(row["to"], {"bob": "read"}, f"{action} did not acknowledge the invitation")

    def test_ledger_probe_reads_one_event_however_much_history_exists(self):
        self.online("alice", "bob")
        for n in range(3):
            self.run_cli("alice", "chat", "bob", f"hi {n}")
        with mock.patch.object(a2a, "_rpc", wraps=a2a._rpc) as rpc:
            self.assertEqual(self.run_cli("bob", "ledger", "--probe"), "")
        self.assertEqual([c.args[0] for c in rpc.call_args_list], ["mempalace_event_list"])
        self.assertEqual(rpc.call_args.args[1]["limit"], 1)

    def test_ledger_probe_fails_when_the_relay_does_not_answer(self):
        with mock.patch.object(a2a, "_rpc", side_effect=a2a.LogstreamError("down")):
            with self.assertRaises(a2a.LogstreamError):
                a2a.cmd_ledger(mock.Mock(probe=True))

    def test_ledger_for_a_session_follows_a_renamed_agent(self):
        session = "61c37a0c-2158-4134-b99e-0914dc1471ff"
        self.run_cli("alice", "session-label", "--session", session, "--name=",
                     "--project-id=" + self.project_id, "--project-name=" + self.project_name)
        with mock.patch.dict(os.environ, {"A2A_HANDLE": "", "NOISEDECK_SESSION_ID": session}), mock.patch.object(a2a, "harness_process", return_value=(1443, "claude")), mock.patch.object(a2a, "_pane_for", return_value={}):
            self.assertEqual(a2a.me(), "claude-61c3", "the default is what the console derives")
            with mock.patch.object(a2a, "courier_ensure"):
                a2a.main(["up"])  # the session announced its default handle first, as every launch does
                a2a.main(["name", "reviewer-933"])
            self.assertEqual(a2a.me(), "reviewer-933")
        # The default presence is older than the rename: age is what the roster reports.
        for e in self.ls.events:
            if e["room"] == a2a.ROOM_PRESENCE and e["from_agent"] == "claude-61c3":
                e["created_at"] = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime(time.time() - 3600))
        with self.as_agent("alice"):
            self.assertEqual(sorted(h for h, m in a2a.roster().items() if m.get("deck_session") == session), ["claude-61c3", "reviewer-933"], "both presences are in the roster")
        self.online("bob")
        self.run_cli("bob", "chat", "reviewer-933", "hello reviewer", "--force")
        self.assertEqual(a2a.handle_for_session(session, "claude"), "reviewer-933", "the newest presence for the session wins over the older default")
        self.assertEqual(a2a.handle_for_session("ffff0000-none", "codex"), "codex-ffff", "no presence: the default derivation")
        with mock.patch.dict(os.environ, {"A2A_HANDLE": "", "NOISEDECK_SESSION_ID": session}), mock.patch.object(a2a, "harness_process", return_value=(1443, "claude")), mock.patch.object(a2a, "_pane_for", return_value={}), mock.patch.object(a2a, "courier_ensure"):
            a2a.main(["down"])  # the session stopped: it is gone from the live roster
        with self.as_agent("alice"):
            self.assertNotIn("reviewer-933", a2a.roster())
        self.assertEqual(a2a.handle_for_session(session, "claude"), "reviewer-933", "a stopped session keeps its ledger")
        with mock.patch.object(a2a, "_age", return_value=a2a.PRESENCE_TTL_S + 1):
            self.assertEqual(a2a.handle_for_session(session, "claude"), "reviewer-933", "and keeps it past the presence horizon")
        out = self.run_cli("bob", "ledger", "--for-session", session, "--harness", "claude", "--compact")
        book = json.loads(out)
        self.assertEqual(book["agent"], "reviewer-933")
        self.assertEqual(book["chats"][0]["rows"][0]["text"], "hello reviewer")

    def test_ledger_index_names_each_sessions_effective_handle(self):
        session = "61c37a0c-2158-4134-b99e-0914dc1471ff"
        with mock.patch.dict(os.environ, {"A2A_HANDLE": "", "NOISEDECK_SESSION_ID": session}), mock.patch.object(a2a, "harness_process", return_value=(1443, "claude")), mock.patch.object(a2a, "_pane_for", return_value={}), mock.patch.object(a2a, "courier_ensure"):
            a2a.main(["up"])
            a2a.main(["name", "reviewer-933"])
        self.online("bob")
        index = json.loads(self.run_cli("bob", "ledger", "--index", "--compact"))
        self.assertEqual(index["sessions"].get(session), "reviewer-933", "the index resolves the session the way the ledger does: the newest presence wins")
        self.assertNotIn("claude-61c3", index["sessions"].values())

    def test_ledger_cli_prints_json(self):
        self.online("alice", "bob")
        self.run_cli("alice", "chat", "bob", "hi")
        out = self.run_cli("bob", "ledger", "--compact")
        book = json.loads(out)
        self.assertEqual(book["agent"], "bob")
        self.assertEqual(book["chats"][0]["rows"][0]["to"], {"bob": "sent"})
        self.assertEqual(json.loads(self.run_cli("alice", "ledger", "--for", "bob"))["agent"], "bob")

    # ── inbox ──────────────────────────────────────────────────────────────

    def test_inbox_marks_read_and_reply_marks_the_chat_read(self):
        self.online("alice", "bob")
        self.run_cli("alice", "chat", "bob", "one")
        self.assertHumanEvent("alice opened a chat with bob: one", self.run_cli("bob", "inbox"))
        self.assertIn("no unread", self.run_cli("bob", "inbox"))
        self.run_cli("alice", "chat", "bob", "two")
        self.run_cli("bob", "chat", "alice", "answer")
        self.assertIn("no unread", self.run_cli("bob", "inbox"))

    def test_mark_read_reports_failure_and_keeps_the_cursor_behind_it(self):
        self.online("alice", "bob")
        self.run_cli("alice", "chat", "bob", "one")
        self.run_cli("alice", "chat", "bob", "two")
        with self.as_agent("bob"):
            first, second = a2a.unread()
        real = self.ls.call

        def flaky(tool, args):
            if tool == "mempalace_event_ack" and args["event_id"] == second["id"]:
                raise a2a.LogstreamError("writer busy")
            return real(tool, args)

        with mock.patch.object(a2a, "_rpc", side_effect=lambda tool, args, timeout=30.0: flaky(tool, args)):
            with self.assertRaises(SystemExit) as stop:
                self.run_cli("bob", "mark-read", first["id"], second["id"])
        self.assertIn("could not be acknowledged", str(stop.exception))
        with self.as_agent("bob"):
            self.assertEqual([e["id"] for e in a2a.unread()], [second["id"]], "the failed one stays unread; the cursor stopped before it")
        self.assertIn("1 marked read", self.run_cli("bob", "mark-read", second["id"]))
        with self.as_agent("bob"):
            self.assertEqual(a2a.unread(), [])

    def test_reading_one_chat_never_skips_an_older_message_in_another(self):
        self.online("alice", "bob", "carol")
        self.run_cli("alice", "chat", "bob", "older, from alice")
        self.run_cli("carol", "chat", "bob", "newer, from carol")
        with self.as_agent("bob"):
            carol_chat = a2a.open_chat_with("carol")
        self.assertHumanEvent("carol opened a chat", self.run_cli("bob", "inbox", "--chat", carol_chat))
        self.assertHumanEvent("alice opened a chat with bob: older, from alice", self.run_cli("bob", "inbox"), "the older message is still unread")
        with self.as_agent("bob"):
            self.assertEqual(a2a.unread(), [])
            self.assertEqual(read_cursor(self), [e for e in self.ls.events if e["type"] == "chat.invite"][-1]["id"], "cursor advanced once everything before it was seen")

    def test_sparse_mark_read_keeps_the_cursor_before_the_first_unread(self):
        self.online("alice", "bob")
        for text in ("one", "two", "three"):
            self.run_cli("alice", "chat", "bob", text)
        with self.as_agent("bob"):
            one, two, three = a2a.unread()
        self.run_cli("bob", "mark-read", three["id"], one["id"])
        self.assertEqual(read_cursor(self), one["id"])
        with self.as_agent("bob"):
            self.assertEqual([e["id"] for e in a2a.unread()], [two["id"]])
        self.run_cli("bob", "mark-read", two["id"])
        self.assertEqual(read_cursor(self), three["id"])

    def test_courier_delivery_counts_as_seen(self):
        self.online("alice", "bob")
        self.run_cli("alice", "chat", "bob", "one")
        invite = next(e for e in self.ls.events if e["type"] == "chat.invite")
        self.ls.call("mempalace_event_ack", {"event_id": invite["id"], "from_agent": "courier@hostA", "status": "ready", "body": a2a.ack_payload(invite["id"], "courier@hostA", "ready", "bob", "delivered to bob via tmux", scope=a2a.scope_of(invite))})
        with self.as_agent("bob"):
            self.assertEqual(a2a.unread(), [])
        # An acknowledgement nobody's key vouches for counts for nothing:
        # the same claim, unsigned, for a second invite leaves it unread.
        self.run_cli("alice", "chat", "bob", "two")
        second = [e for e in self.ls.events if e["type"] in ("chat.invite", "chat.message")][-1]
        self.ls.call("mempalace_event_ack", {"event_id": second["id"], "from_agent": "courier@hostA", "status": "ready", "body": "delivered to bob via tmux"})
        with self.as_agent("bob"):
            self.assertEqual([e["id"] for e in a2a.unread()], [second["id"]])

    def test_stop_hook_nudges_once_for_undelivered_mail(self):
        self.online("alice", "bob")
        self.run_cli("alice", "chat", "bob", "urgent")
        cid = a2a.chat_events()[0]["correlation_id"]
        with mock.patch.object(a2a, "courier_ensure"), mock.patch.object(sys, "stdin", io.StringIO("{}")):
            first = self.run_cli("bob", "hook", "stop", "--harness", "claude")
        self.assertEqual(json.loads(first)["decision"], "block")
        self.assertIn("urgent", json.loads(first)["reason"])
        self.assertNotIn("agree to close", json.loads(first)["reason"])
        self.assertIn(f"a2a reply {cid}", json.loads(first)["reason"])
        self.assertIn(f"a2a leave {cid}", json.loads(first)["reason"])
        with self.as_agent("bob"):
            self.assertEqual(a2a.unread(), [])
        with mock.patch.object(sys, "stdin", io.StringIO("{}")):
            second = self.run_cli("bob", "hook", "stop", "--harness", "claude")
        self.assertEqual(second, "")

    def run_kimi_hook(self, handle: str, event: str, payload: str = "{}", harness: str = "kimi") -> tuple[str, str, int]:
        out, err = io.StringIO(), io.StringIO()
        code = 0
        with self.as_agent(handle), redirect_stdout(out), redirect_stderr(err), mock.patch.object(a2a, "courier_ensure"), mock.patch.object(sys, "stdin", io.StringIO(payload)):
            try:
                a2a.main(["hook", event, "--harness", harness])
            except SystemExit as exc:
                code = exc.code if isinstance(exc.code, int) else 1
        return out.getvalue(), err.getvalue(), code

    def test_kimi_stop_hook_blocks_with_the_reason_on_stderr(self):
        self.online("alice", "bob")
        self.run_cli("alice", "chat", "bob", "urgent")
        cid = a2a.chat_events()[0]["correlation_id"]
        out, err, code = self.run_kimi_hook("bob", "stop")
        self.assertEqual(code, 2, "Kimi blocks a stop with exit 2")
        self.assertEqual(out, "")
        self.assertIn("urgent", err)
        self.assertIn(f"a2a reply {cid}", err)
        with self.as_agent("bob"):
            self.assertEqual(a2a.unread(), [], "the reason is in the model's context: the messages are read")
        out, err, code = self.run_kimi_hook("bob", "stop")
        self.assertEqual((code, out, err), (0, "", ""), "one nudge per message")

    def test_kimi_stop_hook_without_mail_lets_the_turn_end(self):
        self.online("alice", "bob")
        self.assertEqual(self.run_kimi_hook("bob", "stop"), ("", "", 0))

    def test_kimi_stop_hook_still_blocks_when_the_acknowledgement_fails(self):
        self.online("alice", "bob")
        self.run_cli("alice", "chat", "bob", "urgent")
        with mock.patch.object(a2a, "ack", side_effect=a2a.LogstreamError("relay down")):
            out, err, code = self.run_kimi_hook("bob", "stop")
        self.assertEqual(code, 2, "a failed acknowledgement never replaces the blocking exit")
        self.assertIn("urgent", err)
        with self.as_agent("bob"):
            self.assertEqual(len(a2a.unread()), 1, "un-acked: still unread")
            self.assertEqual(a2a.read_state("nudges.json"), {}, "un-acked: eligible to block again")
        out, err, code = self.run_kimi_hook("bob", "stop")
        self.assertEqual(code, 2, "the next stop blocks with the same message again")

    def test_kimi_stop_hook_still_blocks_when_the_cursor_cannot_advance(self):
        self.online("alice", "bob")
        self.run_cli("alice", "chat", "bob", "urgent")
        with mock.patch.object(a2a, "advance_cursor", side_effect=a2a.LogstreamError("disk full")):
            out, err, code = self.run_kimi_hook("bob", "stop")
        self.assertEqual(code, 2)
        self.assertIn("urgent", err)
        self.assertEqual(len([e for e in self.ls.events if e["type"] == "event.ack"]), 1, "the ack the message earned is published")
        out, err, code = self.run_kimi_hook("bob", "stop")
        self.assertEqual((code, err), (0, ""), "acked and delivered: no second block")

    def test_kimi_stop_hook_still_blocks_when_bookkeeping_writes_fail(self):
        self.online("alice", "bob")
        self.run_cli("alice", "chat", "bob", "urgent")
        with mock.patch.object(a2a, "write_state", side_effect=OSError(28, "No space left on device")):
            out, err, code = self.run_kimi_hook("bob", "stop")
        self.assertEqual(code, 2, "a filesystem failure never replaces the blocking exit")
        self.assertIn("urgent", err)

    def test_kimi_stop_hook_still_blocks_when_the_cursor_write_fails(self):
        self.online("alice", "bob")
        self.run_cli("alice", "chat", "bob", "urgent")
        with mock.patch.object(a2a, "advance_cursor", side_effect=OSError(28, "No space left on device")):
            out, err, code = self.run_kimi_hook("bob", "stop")
        self.assertEqual(code, 2)
        self.assertIn("urgent", err)
        out, err, code = self.run_kimi_hook("bob", "stop")
        self.assertEqual((code, err), (0, ""), "acked and delivered: no second block")

    def test_kimi_session_start_hook_prints_nothing(self):
        self.online("bob")
        out, err, code = self.run_kimi_hook("bob", "session-start", '{"session_id": "thread-1"}')
        self.assertEqual((code, out, err), (0, "", ""), "Kimi Code discards SessionStart stdout; the handle rides the first prompt")

    def test_kimi_prompt_hook_announces_the_handle_once_per_session(self):
        self.online("bob")
        out, err, code = self.run_kimi_hook("bob", "prompt", '{"session_id": "thread-1"}')
        self.assertEqual((code, err), (0, ""))
        self.assertIn("your handle is `bob`", out)
        self.assertNotIn("hookSpecificOutput", out, "Claude's envelope shape never reaches Kimi's context")
        out, err, code = self.run_kimi_hook("bob", "prompt", '{"session_id": "thread-1"}')
        self.assertEqual((code, out, err), (0, "", ""), "the same session hears its handle once")
        out, err, code = self.run_kimi_hook("bob", "prompt", '{"session_id": "thread-2"}')
        self.assertIn("your handle is `bob`", out, "a new native session is told again")

    def test_kimi_prompt_hook_keeps_the_marker_of_a_long_lived_session(self):
        self.online("bob")
        now = time.time()
        with mock.patch.object(a2a.time, "time", return_value=now):
            self.run_kimi_hook("bob", "prompt", '{"session_id": "thread-A"}')
        eight_days = now + 8 * 86400
        with mock.patch.object(a2a.time, "time", return_value=eight_days):
            out, _, _ = self.run_kimi_hook("bob", "prompt", '{"session_id": "thread-A"}')
            self.assertEqual(out, "", "a session still prompting is not told again")
            out, _, _ = self.run_kimi_hook("bob", "prompt", '{"session_id": "thread-B"}')
            self.assertIn("your handle is `bob`", out, "a new session is told")
            out, _, _ = self.run_kimi_hook("bob", "prompt", '{"session_id": "thread-A"}')
            self.assertEqual(out, "", "pruning spares the session that prompted today")
        with mock.patch.object(a2a.time, "time", return_value=eight_days + 8 * 86400):
            out, _, _ = self.run_kimi_hook("bob", "prompt", '{"session_id": "thread-C"}')
            with self.as_agent("bob"):
                self.assertEqual(set(a2a.read_state("announced.json")), {"thread-C"}, "sessions silent for a week are pruned")

    def test_kimi_session_end_hook_drops_the_announcement_marker(self):
        self.online("bob")
        self.run_kimi_hook("bob", "prompt", '{"session_id": "thread-A"}')
        with self.as_agent("bob"):
            self.assertIn("thread-A", a2a.read_state("announced.json"))
        self.assertEqual(self.run_kimi_hook("bob", "session-end", '{"session_id": "thread-A"}'), ("", "", 0))
        with self.as_agent("bob"):
            self.assertNotIn("thread-A", a2a.read_state("announced.json"))
        out, _, _ = self.run_kimi_hook("bob", "prompt", '{"session_id": "thread-A"}')
        self.assertIn("your handle is `bob`", out, "a resumed session is told again")

    def test_kimi_prompt_hook_delivers_mail_into_the_turn_and_the_stop_does_not_repeat_it(self):
        self.online("alice", "bob")
        self.run_cli("alice", "chat", "bob", "urgent")
        cid = a2a.chat_events()[0]["correlation_id"]
        self.run_kimi_hook("bob", "prompt", '{"session_id": "thread-1"}')  # the handle announcement
        self.run_cli("alice", "reply", cid, "still urgent")
        out, err, code = self.run_kimi_hook("bob", "prompt", '{"session_id": "thread-1"}')
        self.assertEqual((code, err), (0, ""), "UserPromptSubmit stdout is context: the turn continues")
        self.assertIn("still urgent", out)
        self.assertIn(f"a2a reply {cid}", out)
        self.assertNotIn("your handle", out)
        with self.as_agent("bob"):
            self.assertEqual(a2a.unread(), [], "the reason is in the model's context: the messages are read")
        self.assertEqual(self.run_kimi_hook("bob", "stop"), ("", "", 0), "mail shown at the prompt never blocks the stop")

    def test_kimi_prompt_hook_leaves_unacknowledged_mail_to_the_stop_backstop(self):
        self.online("alice", "bob")
        self.run_cli("alice", "chat", "bob", "urgent")
        with mock.patch.object(a2a, "ack", side_effect=a2a.LogstreamError("relay down")):
            out, err, code = self.run_kimi_hook("bob", "prompt", '{"session_id": "thread-1"}')
        self.assertEqual((code, err), (0, ""), "a failed acknowledgement never fails the prompt")
        self.assertIn("urgent", out)
        with self.as_agent("bob"):
            self.assertEqual(len(a2a.unread()), 1, "un-acked: still unread")
        out, err, code = self.run_kimi_hook("bob", "stop")
        self.assertEqual(code, 2, "the stop backstop shows the same message again")
        self.assertIn("urgent", err)

    def test_kimi_prompt_hook_survives_bookkeeping_write_failures(self):
        self.online("alice", "bob")
        self.run_cli("alice", "chat", "bob", "urgent")
        with mock.patch.object(a2a, "write_state", side_effect=OSError(28, "No space left on device")):
            out, err, code = self.run_kimi_hook("bob", "prompt", '{"session_id": "thread-1"}')
        self.assertEqual((code, err), (0, ""))
        self.assertIn("your handle is `bob`", out)
        self.assertIn("urgent", out)

    def test_claude_prompt_hook_delivers_mail_into_the_turn(self):
        self.online("alice", "bob")
        self.run_cli("alice", "chat", "bob", "urgent")
        cid = a2a.chat_events()[0]["correlation_id"]
        out, err, code = self.run_kimi_hook("bob", "prompt", '{"session_id": "thread-1"}', harness="claude")
        self.assertEqual((code, err), (0, ""), "UserPromptSubmit stdout is context: the turn starts with the mail")
        self.assertIn("urgent", out)
        self.assertIn(f"a2a reply {cid}", out)
        self.assertNotIn("your handle", out, "the handle rode SessionStart for Claude")
        with self.as_agent("bob"):
            self.assertEqual(a2a.unread(), [], "the reason is in the model's context: the messages are read")
        self.assertEqual(self.run_kimi_hook("bob", "stop", harness="claude"), ("", "", 0), "mail shown at the prompt never blocks the stop")

    def test_claude_prompt_hook_leaves_unacknowledged_mail_to_the_stop_backstop(self):
        self.online("alice", "bob")
        self.run_cli("alice", "chat", "bob", "urgent")
        with mock.patch.object(a2a, "ack", side_effect=a2a.LogstreamError("relay down")):
            out, err, code = self.run_kimi_hook("bob", "prompt", '{"session_id": "thread-1"}', harness="claude")
        self.assertEqual((code, err), (0, ""), "a failed acknowledgement never fails the prompt")
        self.assertIn("urgent", out)
        with self.as_agent("bob"):
            self.assertEqual(len(a2a.unread()), 1, "un-acked: still unread")
        out = self.run_cli("bob", "hook", "stop", "--harness", "claude")
        self.assertEqual(json.loads(out)["decision"], "block", "the stop backstop shows the same message again")
        self.assertIn("urgent", json.loads(out)["reason"])

    def test_chat_names_an_idle_peer_that_cannot_be_woken(self):
        self.presence("bob", harness="kimi", kind="agent.idle")
        self.presence("carol", harness="codex", kind="agent.idle", native_session="")
        out = self.run_cli("alice", "chat", "bob,carol", "hello")
        self.assertIn("bob is idle on kimi; it has no wake while idle", out)
        self.assertIn("carol has no announced codex thread; it sees this message only through an explicit `a2a inbox` or `a2a wait`", out)

    def test_chat_names_no_wake_limit_for_peers_that_wake(self):
        self.presence("bob", harness="kimi", kind="agent.busy")
        self.presence("carol", harness="codex", kind="agent.idle")
        self.presence("dave", harness="omp", kind="agent.idle")
        out = self.run_cli("alice", "chat", "bob,carol,dave", "hello")
        self.assertNotIn("no wake while idle", out, "a busy peer is surfaced by its stop hook; queue and extension rungs wake idle peers")
        self.assertNotIn("no announced codex thread", out)

    def test_reply_names_an_idle_member_that_cannot_be_woken(self):
        self.presence("bob", harness="claude", kind="agent.idle")
        out = self.run_cli("alice", "chat", "bob", "hello")
        cid = a2a.chat_events()[0]["correlation_id"]
        out = self.run_cli("alice", "reply", cid, "still here")
        self.assertIn("bob is idle on claude; it has no wake while idle", out)

    def test_an_idle_claude_with_an_inbox_socket_carries_no_wake_warning(self):
        self.presence("bob", harness="claude", kind="agent.idle", native_session="",
                      inbox_socket="/tmp/cc-socks-1003/9.sock")
        out = self.run_cli("alice", "chat", "bob", "hello")
        self.assertNotIn("no wake while idle", out)

    def test_codex_session_start_hook_prints_plain_context_and_announces_the_native_id(self):
        self.online("bob")
        with mock.patch.object(a2a, "scan_panes", return_value=[]):
            out, err, code = self.run_kimi_hook("bob", "session-start", '{"session_id": "thread-codex-1"}', harness="codex")
        self.assertEqual((code, err), (0, ""))
        self.assertIn("your handle is `bob`", out)
        self.assertNotIn("hookSpecificOutput", out, "Claude's envelope shape never reaches Codex's context")
        presence = [e for e in self.ls.events if e["room"] == a2a.ROOM_PRESENCE and e["from_agent"] == "bob"]
        metadata = a2a.metadata_of(presence[-1])
        self.assertEqual(metadata.get("native_session"), "thread-codex-1", "the hook payload's native id reaches presence")
        self.assertEqual((metadata["deck_session"], metadata["project_id"]), (self.deck_session, self.project_id))

    def scan_codex_pane(self, cmdline, env=None, include_process_details=False, cwd="/pane"):
        env = env if env is not None else {"NOISEDECK_SESSION_ID": self.deck_session, "NOISEDECK_PROJECT_ID": self.project_id}
        with mock.patch.object(a2a.os.path, "exists", return_value=True), \
                mock.patch.object(a2a.subprocess, "run", return_value=mock.Mock(stdout="%1 123 /tmux-path fixture\n")), \
                mock.patch.object(a2a, "_harness_under", return_value=(123, "codex")), \
                mock.patch.object(a2a, "_environ", return_value=env), \
                mock.patch.object(a2a.Path, "read_bytes", return_value=b"\0".join(arg.encode() for arg in cmdline) + b"\0"), \
                mock.patch.object(a2a.os, "readlink", return_value=cwd):
            return a2a.scan_panes(include_process_details=include_process_details)[0]

    def test_codex_hook_binds_to_the_pane_owning_the_thread(self):
        session = "a1230000-1111-4222-8333-000000000002"
        thread = "01a074a4-1111-4222-8333-000000000003"
        self.run_cli("observer", "session-label", "--session", session, "--name=Pane name",
                     "--project-id=pane-project", "--project-name=Pane project")
        pane = self.scan_codex_pane(["codex", "resume", thread, "--dangerously-bypass-approvals-and-sandbox"],
                                   env={"NOISEDECK_SESSION_ID": session, "NOISEDECK_PROJECT_ID": "pane-project"},
                                   include_process_details=True)
        with mock.patch.object(a2a, "scan_panes", return_value=[pane]) as scan:
            out, err, code = self.run_kimi_hook("daemon-handle", "session-start",
                                              json.dumps({"session_id": thread, "cwd": "/different"}), harness="codex")
        scan.assert_called_once_with(include_process_details=True)
        self.assertEqual((code, err), (0, ""))
        self.assertIn("your handle is `codex-a123`", out)
        event = self.ls.events[-1]
        self.assertEqual(event["from_agent"], "codex-a123")
        metadata = a2a.metadata_of(event)
        self.assertEqual((metadata["deck_session"], metadata["project_id"], metadata["project_name"]),
                         (session, "pane-project", "Pane project"))
        self.assertEqual((metadata["native_session"], metadata["pid"], metadata["pane"], metadata["display_name"]),
                         (thread, 123, pane["pane"], "Pane name"))
        self.assertNotIn("cmdline", metadata)
        self.assertNotIn("cwd", metadata)
        self.assertTrue(a2a.presence_events()[-1][2], "the writer matches the host-signed handle")
        self.assertEqual(a2a.roster()["codex-a123"]["native_session"], thread)

    def test_codex_hook_with_two_thread_matches_keeps_environment_identity(self):
        thread = "01a074a4-1111-4222-8333-000000000003"
        pane = self.scan_codex_pane(["codex", "resume", thread], include_process_details=True)
        other = {**pane, "pid": 124, "cwd": "/other", "pane": {"socket": "/s", "pane": "%2"}}
        with mock.patch.object(a2a, "scan_panes", return_value=[pane, other]):
            out, err, code = self.run_kimi_hook("daemon-handle", "session-start",
                                              json.dumps({"session_id": thread, "cwd": "/pane"}), harness="codex")
        self.assertEqual((code, err), (0, ""))
        self.assertIn("your handle is `daemon-handle`", out)
        metadata = a2a.metadata_of(self.ls.events[-1])
        self.assertEqual((metadata["handle"], metadata["deck_session"], metadata["project_id"]),
                         ("daemon-handle", self.deck_session, self.project_id))

    def test_codex_hook_uses_unique_process_cwd_when_thread_has_no_match(self):
        pane = self.scan_codex_pane(["codex"], include_process_details=True)
        other = {**pane, "pid": 124, "cwd": "/other"}
        with mock.patch.object(a2a, "scan_panes", return_value=[pane, other]):
            out, err, code = self.run_kimi_hook("daemon-handle", "session-start",
                                              '{"session_id": "new-thread", "cwd": "/pane"}', harness="codex")
        self.assertEqual((code, err), (0, ""))
        self.assertIn("your handle is `codex-7430`", out)
        self.assertEqual(a2a.metadata_of(self.ls.events[-1])["native_session"], "new-thread")

    def test_codex_hook_owner_refuses_missing_and_ambiguous_cwd(self):
        panes = [{"harness": "codex", "cmdline": ["codex"], "cwd": "/pane"},
                 {"harness": "codex", "cmdline": ["codex"], "cwd": "/pane"}]
        for payload in ({}, {"session_id": "missing"}, {"cwd": "/elsewhere"}, {"cwd": "/pane"}):
            with self.subTest(payload=payload):
                self.assertIsNone(a2a.codex_hook_owner(panes, payload))
        self.assertIsNone(a2a.codex_hook_owner([{**panes[0], "harness": "kimi"}], {"cwd": "/pane"}))
        owner = {**panes[0], "cmdline": ["codex", "resume", "thread-id"]}
        self.assertIs(a2a.codex_hook_owner([owner, panes[1]], {"thread_id": "thread-id", "cwd": "/pane"}), owner)

    def test_codex_hook_owner_never_binds_cwd_to_a_different_resume_thread(self):
        pane = {"harness": "codex", "cmdline": ["codex", "resume", "aaaaaaaa-1111-4222-8333-000000000001"], "cwd": "/w"}
        payload = {"session_id": "bbbbbbbb-1111-4222-8333-000000000002", "cwd": "/w"}
        self.assertIsNone(a2a.codex_hook_owner([pane], payload))
        with mock.patch.object(a2a, "scan_panes", return_value=[pane]):
            out, err, code = self.run_kimi_hook("daemon-handle", "session-start", json.dumps(payload), harness="codex")
        self.assertEqual((code, err), (0, ""))
        self.assertIn("your handle is `daemon-handle`", out)
        self.assertEqual(self.ls.events[-1]["from_agent"], "daemon-handle")

    def test_codex_hook_owner_binds_cwd_without_a_resume_thread(self):
        pane = {"harness": "codex", "cmdline": ["codex"], "cwd": "/w"}
        payload = {"session_id": "bbbbbbbb-1111-4222-8333-000000000002", "cwd": "/w"}
        self.assertIs(a2a.codex_hook_owner([pane], payload), pane)
        foreign = {**pane, "cmdline": ["codex", "resume", "aaaaaaaa-1111-4222-8333-000000000001"]}
        self.assertIs(a2a.codex_hook_owner([foreign, pane], payload), pane)
        self.assertIsNone(a2a.codex_hook_owner([foreign, pane, dict(pane)], payload))

    def test_codex_hook_reuses_a_live_owner_without_scanning(self):
        thread = "01a074a4-1111-4222-8333-000000000003"
        pane = self.scan_codex_pane(["codex", "resume", thread], include_process_details=True)
        payload = json.dumps({"session_id": thread, "cwd": "/pane"})
        with mock.patch.object(a2a, "scan_panes", return_value=[pane]) as scan, \
                mock.patch.object(a2a, "harness_of_pid", return_value="codex") as harness:
            self.assertEqual(self.run_kimi_hook("daemon-handle", "session-start", payload, harness="codex")[1:], ("", 0))
            self.assertEqual(self.run_kimi_hook("daemon-handle", "stop", payload, harness="codex"), ("", "", 0))
        scan.assert_called_once_with(include_process_details=True)
        harness.assert_called_once_with(123)
        self.assertEqual(self.ls.events[-1]["from_agent"], pane["handle"])
        cached = json.loads(next(a2a.STATE_DIR.glob("codex-owner-*.json")).read_text())
        self.assertNotIn("cmdline", cached["owner"])
        self.assertNotIn("cwd", cached["owner"])

    def test_codex_hook_rescans_expired_dead_or_replaced_owners(self):
        thread = "01a074a4-1111-4222-8333-000000000003"
        pane = self.scan_codex_pane(["codex", "resume", thread], include_process_details=True)
        payload = {"session_id": thread}
        now = time.time()
        for age, harness in ((a2a.CODEX_OWNER_TTL_S, "codex"), (1, None), (1, "claude")):
            with self.subTest(age=age, harness=harness), \
                    mock.patch.object(a2a.time, "time", return_value=now), \
                    mock.patch.object(a2a, "scan_panes", return_value=[pane]):
                a2a.cached_codex_hook_owner(payload)
                replacement = {**pane, "pid": 124}
                with mock.patch.object(a2a.time, "time", return_value=now + age), \
                        mock.patch.object(a2a, "harness_of_pid", return_value=harness), \
                        mock.patch.object(a2a, "scan_panes", return_value=[replacement]) as scan:
                    self.assertEqual(a2a.cached_codex_hook_owner(payload)["pid"], 124)
                    self.assertEqual(a2a.cached_codex_hook_owner(payload)["pid"], 124)
                self.assertEqual(scan.call_count, 1 if harness == "codex" else 2)

    def test_codex_hook_cache_is_keyed_by_thread(self):
        thread = "01a074a4-1111-4222-8333-000000000003"
        pane = self.scan_codex_pane(["codex", "resume", thread], include_process_details=True)
        with mock.patch.object(a2a, "scan_panes", return_value=[pane]) as scan, \
                mock.patch.object(a2a, "harness_of_pid", return_value="codex"):
            self.assertIsNotNone(a2a.cached_codex_hook_owner({"session_id": thread, "cwd": "/pane"}))
            self.assertIsNone(a2a.cached_codex_hook_owner({"session_id": "foreign-thread", "cwd": "/pane"}))
        self.assertEqual(scan.call_count, 2)

    def test_codex_hook_cache_write_failure_does_not_fail_the_hook(self):
        pane = self.scan_codex_pane(["codex"], include_process_details=True)
        with mock.patch.object(a2a, "scan_panes", return_value=[pane]), \
                mock.patch.object(a2a, "write_state", side_effect=OSError(28, "No space left on device")):
            out, err, code = self.run_kimi_hook("daemon-handle", "session-start",
                                              '{"session_id": "new-thread", "cwd": "/pane"}', harness="codex")
        self.assertEqual((code, err), (0, ""))
        self.assertIn(f"your handle is `{pane['handle']}`", out)

    def test_codex_hook_and_courier_scan_use_the_same_handle_after_rename(self):
        with mock.patch.dict(os.environ, {"A2A_HANDLE": ""}):
            a2a.set_handle("chosen-pane")
        pane = self.scan_codex_pane(["codex"], include_process_details=True)
        courier_pane = self.scan_codex_pane(["codex"])
        self.assertEqual(pane["handle"], courier_pane["handle"])
        with mock.patch.object(a2a, "scan_panes", return_value=[courier_pane]):
            a2a.publish_scan({}, verbose=False)
        with mock.patch.object(a2a, "scan_panes", return_value=[pane]):
            out, err, code = self.run_kimi_hook("daemon-handle", "session-start", '{"cwd": "/pane"}', harness="codex")
        self.assertEqual((code, err), (0, ""))
        self.assertIn(f"your handle is `{pane['handle']}`", out)
        self.assertEqual(self.ls.events[-1]["from_agent"], pane["handle"])
        self.assertEqual(set(a2a.roster()), {pane["handle"]})
        self.assertEqual(a2a.roster()[pane["handle"]]["via"], "hook")

    def test_codex_scan_supplies_resume_native_session_for_queue_delivery(self):
        thread = "01a074a4-1111-4222-8333-000000000003"
        pane = self.scan_codex_pane(["/usr/local/bin/codex", "resume", thread, "--dangerously-bypass-approvals-and-sandbox"])
        self.assertEqual(pane["native_session"], thread)
        self.assertNotIn("cmdline", pane)
        self.assertNotIn("cwd", pane)
        with mock.patch.object(a2a, "scan_panes", return_value=[pane]):
            a2a.publish_scan({}, verbose=False)
        row = a2a.roster()[pane["handle"]]
        self.assertEqual((row["via"], row["native_session"]), ("courier", thread))
        self.assertEqual(a2a.delivery_mode(row), "codex-queue")
        self.assertEqual(a2a.delivery_mode({**row, "pane": {"socket": "/foreign.sock"}}), "unavailable")

    def test_codex_scan_native_session_never_overrides_hook_presence(self):
        thread = "01a074a4-1111-4222-8333-000000000003"
        pane = self.scan_codex_pane(["codex", "resume", thread])
        self.presence(pane["handle"], native_session="hook-thread", pane="%1")
        with mock.patch.object(a2a, "scan_panes", return_value=[pane]):
            a2a.publish_scan({}, verbose=False)
        row = a2a.roster()[pane["handle"]]
        self.assertEqual((row["via"], row["native_session"]), ("hook", "hook-thread"))

    def test_codex_scan_requires_a_resume_uuid(self):
        thread = "01a074a4-1111-4222-8333-000000000003"
        for cmdline in (["codex"], ["codex", "resume"], ["codex", "resume", "--last"],
                        ["codex", "resume", "not-a-uuid"], ["codex", "exec", thread]):
            with self.subTest(cmdline=cmdline):
                self.assertEqual(a2a.codex_resume_session(cmdline), "")

    def test_claude_session_start_hook_prints_the_envelope(self):
        self.online("bob")
        out = io.StringIO()
        with self.as_agent("bob"), redirect_stdout(out), mock.patch.object(a2a, "courier_ensure"), mock.patch.object(sys, "stdin", io.StringIO('{"session_id": "thread-1"}')):
            a2a.main(["hook", "session-start", "--harness", "claude"])
        notice = json.loads(out.getvalue())
        self.assertIn("your handle is `bob`", notice["hookSpecificOutput"]["additionalContext"])

    def test_claude_hooks_announce_the_inbox_socket(self):
        self.online("bob")
        env = {"CLAUDE_CODE_MESSAGING_SOCKET": "/tmp/cc-socks-1003/1.sock"}
        with self.as_agent("bob"):
            for event in ("session-start", "prompt", "stop"):
                with self.subTest(event=event), mock.patch.dict(os.environ, env), \
                        mock.patch.object(a2a, "courier_ensure"), \
                        mock.patch.object(sys, "stdin", io.StringIO("{}")), \
                        redirect_stdout(io.StringIO()):
                    a2a.main(["hook", event, "--harness", "claude"])
                metadata = a2a.metadata_of(self.ls.events[-1])
                self.assertEqual(metadata.get("inbox_socket"), "/tmp/cc-socks-1003/1.sock",
                                 f"a {event} announcement drops the socket and idles the rung")
            row = a2a.roster()["bob"]
            self.assertEqual(row["inbox_socket"], "/tmp/cc-socks-1003/1.sock")
            with mock.patch.dict(os.environ, {"CLAUDE_CODE_MESSAGING_SOCKET": ""}), \
                    mock.patch.object(sys, "stdin", io.StringIO("{}")), redirect_stdout(io.StringIO()):
                a2a.main(["hook", "prompt", "--harness", "kimi"])
        self.assertEqual(a2a.metadata_of(self.ls.events[-1]).get("inbox_socket"), "",
                         "only Claude announces a socket")

    def test_claude_delivery_mode_uses_the_socket_when_present(self):
        row = {"handle": "bob", "harness": "claude", "inbox_socket": "/tmp/cc-socks-1003/1.sock",
               "pane": {"socket": "/s", "pane": "%1"}}
        self.assertEqual(a2a.delivery_mode(row), "claude-socket")
        self.assertEqual(a2a.delivery_mode({**row, "inbox_socket": ""}), "inbox-only")
        self.assertEqual(a2a.delivery_mode({**row, "pane": {"socket": "/foreign.sock", "pane": "%1"}}), "unavailable")
        self.assertEqual(a2a.delivery_mode({**row, "harness": "kimi"}), "inbox-only")

    def test_claude_socket_posts_one_user_message_line(self):
        directory = Path(tempfile.mkdtemp(prefix="a2a-claude-sock-"))
        self.addCleanup(lambda: shutil.rmtree(directory, ignore_errors=True))
        path = directory / "in.sock"
        received = []
        server = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        server.bind(str(path))
        server.listen(1)
        self.addCleanup(server.close)

        def serve():
            conn, _ = server.accept()
            with conn:
                data = b""
                while not data.endswith(b"\n"):
                    chunk = conn.recv(4096)
                    if not chunk:
                        break
                    data += chunk
                received.append(data)

        thread = threading.Thread(target=serve, daemon=True)
        thread.start()
        try:
            self.assertEqual(a2a._claude_socket(f"uds:{path}", "wake text"), "claude-socket")
        finally:
            thread.join(timeout=10)
        self.assertEqual(len(received), 1)
        self.assertEqual(json.loads(received[0]),
                         {"type": "user", "message": {"role": "user", "content": "wake text"}})

    def test_claude_socket_refusal_is_inbox_only_and_a_failed_write_is_uncertain(self):
        self.assertEqual(a2a._claude_socket("", "t"), "inbox-only")
        self.assertEqual(a2a._claude_socket("not-a-path", "t"), "inbox-only")
        self.assertEqual(a2a._claude_socket("/nonexistent/dir/x.sock", "t"), "inbox-only")
        conn = mock.Mock()
        conn.sendall.side_effect = OSError("broken pipe")
        with mock.patch.object(a2a.socket, "socket", return_value=conn):
            self.assertEqual(a2a._claude_socket("/tmp/x.sock", "t"), "uncertain")

    def test_courier_delivers_to_claude_through_the_inbox_socket(self):
        self.online("alice")
        self.presence("bob", harness="claude", native_session="", inbox_socket="/inbox/bob.sock", pane="%3")
        self.run_cli("alice", "chat", "bob", "hi")
        pastes = []
        with mock.patch.object(a2a, "_claude_socket", side_effect=lambda path, text: pastes.append((path, text)) or "claude-socket"), \
                mock.patch.object(a2a, "_codex_queue", side_effect=AssertionError("the codex queue never serves claude")), \
                mock.patch.object(a2a, "_age", return_value=1):
            state = a2a.courier_once({}, "hostA", verbose=False)
        self.assertEqual([path for path, _ in pastes], ["/inbox/bob.sock"])
        self.assertEqual(state["pending"], {})
        self.assertEqual([a2a.ack_note(a) for a in self.ls.events if a["type"] == "event.ack"],
                         ["delivered to bob via claude-socket"])

    def test_a_delivered_native_delivery_is_reminded_once_when_left_unread(self):
        self.online("alice")
        self.presence("bob", harness="codex")
        self.run_cli("alice", "chat", "bob", "hi")
        calls = []
        with mock.patch.object(a2a, "_codex_queue", side_effect=lambda thread, text: calls.append(text) or "codex-queue"), \
                mock.patch.object(a2a, "_age", return_value=1):
            state = a2a.courier_once({}, "hostA", verbose=False)
            self.assertEqual(len(calls), 1)
            nudgable = state.get("nudgable", {})
            self.assertEqual(len(nudgable), 1)
            item = next(iter(nudgable.values()))
            self.assertEqual((item["to"], item["rung"]), ("bob", "codex-queue"))
            # Too early: no reminder.
            a2a.courier_once(state, "hostA", verbose=False)
            self.assertEqual(len(calls), 1)
            # Past the delay: one reminder naming the chat, never the body again.
            item["delivered_at"] -= a2a.NUDGE_AFTER_S + 1
            state = a2a.courier_once(state, "hostA", verbose=False)
            self.assertEqual(len(calls), 2)
            self.assertIn("reminder", calls[1])
            self.assertIn("chat-", calls[1])
            self.assertNotIn("opened a chat", calls[1])
            # Spent: never again, however many passes.
            state = a2a.courier_once(state, "hostA", verbose=False)
            self.assertEqual(len(calls), 2)

    def _reminder_settled_during_flush(self, action):
        self.online("alice")
        self.presence("bob", harness="codex")
        self.run_cli("alice", "chat", "bob", "hi")
        calls = []
        with mock.patch.object(a2a, "_codex_queue", side_effect=lambda thread, text: calls.append(text) or "codex-queue"), \
                mock.patch.object(a2a, "_age", return_value=1):
            state = a2a.courier_once({}, "hostA", verbose=False)
            item = next(iter(state["nudgable"].values()))
            item["delivered_at"] -= a2a.NUDGE_AFTER_S + 1
            changed = False

            def persist(snapshot):
                nonlocal changed
                if not changed and any(n.get("nudge_inflight") for n in snapshot["nudgable"].values()):
                    changed = True
                    if action == "read":
                        self.run_cli("bob", "read", item["chat"])
                    else:
                        self.run_cli("alice", "end", item["chat"], "done")

            state = a2a.courier_once(state, "hostA", verbose=False, persist=persist)
        self.assertTrue(changed)
        self.assertEqual([line for line in calls if "reminder" in line], [])
        self.assertEqual(state["nudgable"], {})

    def test_read_committed_during_reminder_flush_prevents_submission(self):
        self._reminder_settled_during_flush("read")

    def test_close_committed_during_reminder_flush_prevents_submission(self):
        self._reminder_settled_during_flush("close")

    def test_failed_final_reminder_check_waits_without_spending_an_attempt(self):
        self.online("alice")
        self.presence("bob", harness="codex")
        self.run_cli("alice", "chat", "bob", "hi")
        calls = []
        with mock.patch.object(a2a, "_codex_queue", side_effect=lambda thread, text: calls.append(text) or "codex-queue"), \
                mock.patch.object(a2a, "_age", return_value=1):
            state = a2a.courier_once({}, "hostA", verbose=False)
            item = next(iter(state["nudgable"].values()))
            item["delivered_at"] -= a2a.NUDGE_AFTER_S + 1
            unavailable = False

            def persist(snapshot):
                nonlocal unavailable
                if any(n.get("nudge_inflight") for n in snapshot["nudgable"].values()):
                    unavailable = True

            def rpc(tool, args, timeout=30.0):
                if unavailable and tool == "mempalace_event_list" and args.get("type") == "event.ack":
                    raise a2a.LogstreamError("synthetic receipt-list failure")
                return self.ls.call(tool, args)

            with mock.patch.object(a2a, "_rpc", side_effect=rpc):
                state = a2a.courier_once(state, "hostA", verbose=False, persist=persist)
            self.assertEqual(len(calls), 1)
            self.assertEqual(item["nudge_attempts"], 0)
            self.assertNotIn("nudge_inflight", item)
            state = a2a.courier_once(state, "hostA", verbose=False)
        self.assertEqual(len([line for line in calls if "reminder" in line]), 1)

    def test_already_queued_reminder_does_not_claim_current_unread_state(self):
        self.online("alice")
        self.presence("bob", harness="codex")
        self.run_cli("alice", "chat", "bob", "hi")
        accepted = []
        with mock.patch.object(a2a, "_codex_queue", side_effect=lambda thread, text: accepted.append(text) or "codex-queue"), \
                mock.patch.object(a2a, "_age", return_value=1):
            state = a2a.courier_once({}, "hostA", verbose=False)
            item = next(iter(state["nudgable"].values()))
            item["delivered_at"] -= a2a.NUDGE_AFTER_S + 1
            a2a.courier_once(state, "hostA", verbose=False)
        self.run_cli("bob", "read", item["chat"])
        self.run_cli("alice", "end", item["chat"], "done")
        # Native queues may surface their accepted text only after both actions.
        line = next(line for line in accepted if "reminder" in line)
        self.assertNotIn("is still unread", line)
        self.assertNotIn("a2a reply", line)
        self.assertIn("a2a inbox", line)
        self.assertIn("already read", line)
        self.assertIn("closed", line)

    def test_a_read_delivery_is_never_reminded(self):
        self.online("alice")
        self.presence("bob", harness="codex")
        self.run_cli("alice", "chat", "bob", "hi")
        calls = []
        with mock.patch.object(a2a, "_codex_queue", side_effect=lambda thread, text: calls.append(text) or "codex-queue"), \
                mock.patch.object(a2a, "_age", return_value=1):
            state = a2a.courier_once({}, "hostA", verbose=False)
        cid = a2a.chat_events()[0]["correlation_id"]
        with mock.patch.object(sys, "stdin", io.StringIO("{}")):
            self.run_cli("bob", "reply", cid, "got it")
        item = next(iter(state["nudgable"].values()))
        item["delivered_at"] -= a2a.NUDGE_AFTER_S + 1
        with mock.patch.object(a2a, "_codex_queue", side_effect=lambda thread, text: calls.append(text) or "codex-queue"), \
                mock.patch.object(a2a, "_age", return_value=1):
            state = a2a.courier_once(state, "hostA", verbose=False)
        reminders = [text for text in calls if "reminder" in text]
        self.assertEqual(reminders, [], "no reminder once the recipient has read it")
        self.assertEqual([n for n in state["nudgable"].values() if n["to"] == "bob"], [])

    def test_a_departed_recipient_is_never_reminded(self):
        self.online("alice")
        self.presence("bob", harness="codex")
        self.run_cli("alice", "chat", "bob", "hi")
        calls = []
        with mock.patch.object(a2a, "_codex_queue", side_effect=lambda thread, text: calls.append(text) or "codex-queue"), \
                mock.patch.object(a2a, "_age", return_value=1):
            state = a2a.courier_once({}, "hostA", verbose=False)
        cid = a2a.chat_events()[0]["correlation_id"]
        self.run_cli("bob", "leave", cid, "bye")
        item = next(iter(state["nudgable"].values()))
        item["delivered_at"] -= a2a.NUDGE_AFTER_S + 1
        with mock.patch.object(a2a, "_codex_queue", side_effect=lambda thread, text: calls.append(text) or "codex-queue"), \
                mock.patch.object(a2a, "_age", return_value=1):
            state = a2a.courier_once(state, "hostA", verbose=False)
        reminders = [text for text in calls if "reminder" in text]
        self.assertEqual(reminders, [], "no reminder to a member who left")
        self.assertEqual([n for n in state["nudgable"].values() if n["to"] == "bob"], [])

    def test_a_refused_reminder_is_retried_with_a_cap(self):
        self.online("alice")
        self.presence("bob", harness="codex")
        self.run_cli("alice", "chat", "bob", "hi")
        outcomes = iter(["codex-queue"] + ["inbox-only"] * 10)
        calls = []
        with mock.patch.object(a2a, "_codex_queue", side_effect=lambda thread, text: calls.append(text) or next(outcomes)), \
                mock.patch.object(a2a, "_age", return_value=1):
            state = a2a.courier_once({}, "hostA", verbose=False)
            item = next(iter(state["nudgable"].values()))
            item["delivered_at"] -= a2a.NUDGE_AFTER_S + 1
            for _ in range(4):
                state = a2a.courier_once(state, "hostA", verbose=False)
        self.assertEqual(len(calls), 1 + a2a.NUDGE_MAX_ATTEMPTS,
                         "a reminder the rung refuses is retried up to the cap, then given up")
        self.assertEqual(state["nudgable"], {})

    def test_a_crash_mid_reminder_never_repeats_it(self):
        self.online("alice")
        self.presence("bob", harness="codex")
        self.run_cli("alice", "chat", "bob", "hi")
        calls = []
        snapshots = []
        persist = lambda st: snapshots.append(json.loads(json.dumps(st)))
        with mock.patch.object(a2a, "_codex_queue", side_effect=lambda thread, text: calls.append(text) or "codex-queue"), \
                mock.patch.object(a2a, "_age", return_value=1):
            state = a2a.courier_once({}, "hostA", verbose=False, persist=persist)
            item = next(iter(state["nudgable"].values()))
            item["delivered_at"] -= a2a.NUDGE_AFTER_S + 1
            a2a.courier_once(state, "hostA", verbose=False, persist=persist)
        reminders = [text for text in calls if "reminder" in text]
        self.assertEqual(len(reminders), 1)
        # The snapshot written just before the side effect carries the
        # in-flight marker; a courier dying there restores from it.
        inflight = [snap for snap in snapshots
                    if any(n.get("nudge_inflight") for n in snap.get("nudgable", {}).values())]
        self.assertTrue(inflight, "the in-flight reminder was persisted before the side effect")
        with mock.patch.object(a2a, "_codex_queue", side_effect=lambda thread, text: calls.append(text) or "codex-queue"), \
                mock.patch.object(a2a, "_age", return_value=1):
            restored = a2a.courier_once(inflight[-1], "hostA", verbose=False)
            a2a.courier_once(restored, "hostA", verbose=False)
        self.assertEqual([text for text in calls if "reminder" in text], reminders,
                         "a reminder accepted before the crash is never repeated after it")

    def test_a_spent_watch_ends_when_the_chat_closes(self):
        self.online("alice")
        self.presence("bob", harness="codex")
        self.run_cli("alice", "chat", "bob", "hi")
        calls = []
        with mock.patch.object(a2a, "_codex_queue", side_effect=lambda thread, text: calls.append(text) or "codex-queue"), \
                mock.patch.object(a2a, "_age", return_value=1):
            state = a2a.courier_once({}, "hostA", verbose=False)
            item = next(iter(state["nudgable"].values()))
            item["delivered_at"] -= a2a.NUDGE_AFTER_S + 1
            state = a2a.courier_once(state, "hostA", verbose=False)
        cid = a2a.chat_events()[0]["correlation_id"]
        self.run_cli("alice", "end", cid, "done")
        with mock.patch.object(a2a, "_codex_queue", side_effect=lambda thread, text: calls.append(text) or "codex-queue"), \
                mock.patch.object(a2a, "_age", return_value=1):
            state = a2a.courier_once(state, "hostA", verbose=False)
        self.assertEqual([n for n in state["nudgable"].values() if n["to"] == "bob"], [],
                         "a spent watch does not outlive its chat")

    def test_a_watch_dies_when_its_event_no_longer_routes(self):
        self.online("alice")
        self.presence("bob", harness="codex")
        self.presence("carol", harness="codex")
        self.run_cli("alice", "chat", "bob,carol", "hi")
        calls = []
        with mock.patch.object(a2a, "_codex_queue", side_effect=lambda thread, text: calls.append(text) or "codex-queue"), \
                mock.patch.object(a2a, "_age", return_value=1):
            state = a2a.courier_once({}, "hostA", verbose=False)
            self.run_cli("carol", "reply", a2a.chat_events()[0]["correlation_id"], "carol here")
            state = a2a.courier_once(state, "hostA", verbose=False)
        reply = next(e for e in self.ls.events if e["type"] == "chat.message")
        watch = next(n for n in state["nudgable"].values() if n["to"] == "bob" and n["event"] == reply["id"])
        # The reply's sender can no longer be placed (a departure or its
        # presence replicated late): the fold routes the event nowhere, and
        # the watch for it must die rather than remind anyone of a message
        # their history omits.
        reply["from_agent"] = "mallory"
        watch["delivered_at"] -= a2a.NUDGE_AFTER_S + 1
        with mock.patch.object(a2a, "_codex_queue", side_effect=lambda thread, text: calls.append(text) or "codex-queue"), \
                mock.patch.object(a2a, "_age", return_value=1):
            state = a2a.courier_once(state, "hostA", verbose=False)
        self.assertEqual([text for text in calls if "reminder" in text], [],
                         "no reminder for an event that routes nowhere")
        self.assertNotIn(reply["id"], [n["event"] for n in state["nudgable"].values()])


    def test_stop_hook_reports_legacy_proposals_without_teaching_agreement(self):
        cid = self.legacy_chat("alice", "bob", "carol")
        proposal = self.append_chat("bob", "chat.close-request", cid, body="old proposal")
        with mock.patch.object(sys, "stdin", io.StringIO("{}")):
            first = self.run_cli("alice", "hook", "stop", "--harness", "claude")
        notice = json.loads(first)
        self.assertEqual(notice["decision"], "block")
        self.assertIn("old proposal", notice["reason"])
        self.assertNotIn("agree to close", notice["reason"])
        self.assertIn("everyone", notice["reason"])
        self.assertIn(f"a2a end {cid}", notice["reason"])
        with self.as_agent("alice"):
            self.assertEqual(a2a.unread(), [])
        with mock.patch.object(sys, "stdin", io.StringIO("{}")):
            second = self.run_cli("alice", "hook", "stop", "--harness", "claude")
        self.assertEqual(second, "")
        acks = [e for e in self.ls.events if e["type"] == "event.ack" and a2a.metadata_of(e).get("ack_of") == proposal["id"]]
        self.assertEqual([e["from_agent"] for e in acks], ["alice"])

    def test_stop_hook_gives_a_new_member_history_guidance_only_while_live(self):
        for viewer, ended in (("carol", False), ("carol", True), ("bob", False)):
            with self.subTest(viewer=viewer, ended=ended), mock.patch.object(a2a, "STATE_DIR", Path(self.tmp.name) / f"join-{viewer}-{ended}"):
                self.ls = FakeLogstream()
                self.register_project()
                self.online("alice", "bob", "carol")
                self.run_cli("alice", "chat", "bob", "the original invitation")
                cid = a2a.chat_events()[0]["correlation_id"]
                self.run_cli("bob", "inbox")
                self.run_cli("alice", "add", cid, "carol")
                join = next(e for e in a2a.chat_events(cid) if e["type"] == "chat.join")
                if ended:
                    self.run_cli("alice", "end", cid)
                with mock.patch.object(sys, "stdin", io.StringIO("{}")):
                    first = self.run_cli(viewer, "hook", "stop", "--harness", "claude")
                notice = json.loads(first)
                self.assertEqual(notice["decision"], "block")
                self.assertIn("added carol", notice["reason"])
                self.assertNotIn("the original invitation", notice["reason"])
                if viewer == "carol" and not ended:
                    self.assertIn(f"a2a read {cid}", notice["reason"])
                else:
                    self.assertNotIn(f"a2a read {cid}", notice["reason"])
                for action in ("reply", "leave"):
                    if ended:
                        self.assertNotIn(f"a2a {action} {cid}", notice["reason"])
                    else:
                        self.assertIn(f"a2a {action} {cid}", notice["reason"])
                with self.as_agent(viewer):
                    self.assertEqual(a2a.unread(), [])
                with mock.patch.object(sys, "stdin", io.StringIO("{}")):
                    self.assertEqual(self.run_cli(viewer, "hook", "stop", "--harness", "claude"), "")
                acks = [e for e in self.ls.events if e["type"] == "event.ack" and a2a.metadata_of(e).get("ack_of") == join["id"]]
                self.assertEqual([e["from_agent"] for e in acks], [viewer])

    def test_stop_hook_offers_the_lone_opener_an_end_but_not_a_reply(self):
        self.online("alice", "bob")
        self.run_cli("alice", "chat", "bob", "hi")
        cid = a2a.chat_events()[0]["correlation_id"]
        self.run_cli("bob", "leave", cid)
        departure = a2a.chat_events(cid)[-1]
        with mock.patch.object(sys, "stdin", io.StringIO("{}")):
            first = self.run_cli("alice", "hook", "stop", "--harness", "claude")
        notice = json.loads(first)
        self.assertEqual(notice["decision"], "block")
        self.assertIn("left", notice["reason"])
        self.assertIn(f"a2a end {cid}", notice["reason"])
        self.assertNotIn(f"a2a reply {cid}", notice["reason"])
        with self.as_agent("alice"):
            self.assertEqual(a2a.unread(), [])
        with mock.patch.object(sys, "stdin", io.StringIO("{}")):
            self.assertEqual(self.run_cli("alice", "hook", "stop", "--harness", "claude"), "")
        acks = [e for e in self.ls.events if e["type"] == "event.ack" and a2a.metadata_of(e).get("ack_of") == departure["id"]]
        self.assertEqual([e["from_agent"] for e in acks], ["alice"])

    def test_stop_hook_only_suggests_actions_for_chats_that_are_still_live(self):
        for mixed in (False, True):
            with self.subTest(mixed=mixed), mock.patch.object(a2a, "STATE_DIR", Path(self.tmp.name) / f"hook-{mixed}"):
                self.ls = FakeLogstream()
                self.register_project()
                self.online("alice", "bob", "carol")
                self.run_cli("alice", "chat", "bob", "old invitation")
                ended = a2a.chat_events()[0]["correlation_id"]
                self.run_cli("bob", "inbox")
                self.run_cli("alice", "end", ended)
                if mixed:
                    self.run_cli("carol", "chat", "bob", "new invitation")
                    live = next(e["correlation_id"] for e in a2a.chat_events() if a2a.open_body(e)[0] == "new invitation")
                with mock.patch.object(sys, "stdin", io.StringIO("{}")):
                    first = self.run_cli("bob", "hook", "stop", "--harness", "claude")
                notice = json.loads(first)
                self.assertIn("closed", notice["reason"])
                self.assertNotIn("agree to close", notice["reason"])
                for action in ("reply", "end", "leave"):
                    self.assertNotIn(f"a2a {action} {ended}", notice["reason"])
                    if not mixed:
                        self.assertNotIn(f"a2a {action}", notice["reason"])
                if mixed:
                    self.assertIn(f"a2a reply {live}", notice["reason"])
                    self.assertIn(f"a2a leave {live}", notice["reason"])
                with self.as_agent("bob"):
                    self.assertEqual(a2a.unread(), [])
                with mock.patch.object(sys, "stdin", io.StringIO("{}")):
                    self.assertEqual(self.run_cli("bob", "hook", "stop", "--harness", "claude"), "")

    # ── identity and roster ────────────────────────────────────────────────

    def test_explicit_socket_list_replaces_the_defaults(self):
        with mock.patch.dict(os.environ, {"A2A_TMUX_SOCKETS": "/one.sock:/two.sock"}):
            self.assertEqual(a2a.tmux_sockets(), ["/one.sock", "/two.sock"])
        with mock.patch.dict(os.environ, {"A2A_TMUX_SOCKETS": ""}):
            self.assertEqual(a2a.tmux_sockets(), list(a2a.DEFAULT_TMUX_SOCKETS))

    def test_default_handle_matches_from_inside_and_outside(self):
        self.assertEqual(a2a.handle_tag("61c37a0c-2158", 1443), "61c3")
        self.assertEqual(a2a.handle_tag("", 1443), "1443")
        with mock.patch.object(a2a, "harness_process", return_value=(1443, "codex")), mock.patch.dict(os.environ, {"NOISEDECK_SESSION_ID": "61c37a0c-2158", "A2A_HANDLE": ""}):
            self.assertEqual(a2a.default_handle(), "codex-61c3")

    def test_roster_merges_courier_scans_with_hook_presence(self):
        self.ls.call("mempalace_event_append", {
            "type": "courier.scan", "stream": a2a.STREAM, "room": a2a.ROOM_PRESENCE, "from_agent": "courier@hostA", "to_agent": "*",
            "metadata": a2a.signed_presence("courier.scan", {"host": "hostA", "agents": [{"handle": "codex-8e70", "harness": "codex", "host": "hostA", "pane": {"socket": "/s", "pane": "%1"}}, {"handle": "dave", "harness": "claude", "host": "hostA", "pane": {"socket": "/s", "pane": "%2"}}]}),
        })
        with self.as_agent("dave"):
            a2a.append("agent.busy", a2a.ROOM_PRESENCE, to="*", status="ready", metadata=a2a.signed_presence("agent.busy", {"handle": "dave", "harness": "claude", "host": "hostA", "pane": {}}))
        with mock.patch.object(a2a, "_age", return_value=1):
            rows = a2a.roster()
        self.assertEqual(rows["codex-8e70"]["via"], "courier")
        self.assertEqual(rows["dave"]["state"], "busy")
        self.assertEqual(rows["dave"]["pane"]["pane"], "%2", "hook presence without a pane borrows the scanned one")

    def test_roster_and_chats_fold_past_the_server_page_size(self):
        self.online("alice")
        for _ in range(a2a.PAGE):
            self.online("filler")
        with self.as_agent("alice"), mock.patch.object(a2a, "harness_process", return_value=None), mock.patch.object(a2a, "_pane_for", return_value={}):
            a2a.announce("offline")
        with mock.patch.object(a2a, "_age", return_value=1):
            self.assertNotIn("alice", a2a.roster(), "the 501st presence event is folded")
        self.online("bob")
        for i in range(a2a.PAGE):
            self.run_cli("alice", "chat", "bob", f"msg {i}", "--force")
        self.run_cli("carol", "chat", "bob", "late invite", "--force")
        with self.as_agent("bob"):
            self.assertIn(a2a.open_chat_with("carol"), a2a.my_chats(), "an invite past the first page is visible")
        self.assertTrue(all(args.get("order") == "asc" for args in self.ls.listings), "every listing asks for append order")

    def test_a_relay_that_refuses_the_order_parameter_is_asked_once(self):
        """A 3.8.0 relay lists oldest first and rejects `order`: the client
        asks once, reads the whole room in order, and stops asking."""
        self.ls = FakeLogstream(refuses_order=True)
        for i in range(a2a.PAGE + 2):
            self.presence(f"agent-{i}")
        with self.as_agent("observer"):
            hosts = a2a.last_known_hosts(max_age_s=0)
            self.assertEqual(len(hosts), a2a.PAGE + 2)
            a2a.last_known_hosts(max_age_s=0)
        asked = [args for args in self.ls.listings if "order" in args]
        self.assertEqual(len(asked), 1)
        self.assertGreaterEqual(len(self.ls.listings), 3, "a full two-page read, then the state's delta")
        self.assertTrue(a2a._ORDER_REFUSED)

    def test_a_relay_upgraded_under_a_running_process_is_asked_again(self):
        """A client that saw a 3.8.0 relay refuse `order` reads without it;
        when that relay comes back as 3.10.0 and lists newest first, the
        next whole-room read — the derived state rebuilding, as a reset
        palace or a fresh process makes it — asks again and reads in
        order, with no restart. Cursor deltas are ordered by construction
        and never re-ask on their own."""
        self.ls = FakeLogstream(refuses_order=True)
        self.online("alice")
        with self.as_agent("observer"):
            self.assertEqual(a2a.last_known_hosts(max_age_s=0), {"alice": "hostA"})
        self.assertTrue(a2a._ORDER_REFUSED)
        self.ls.refuses_order = False
        self.online("bob")
        with self.as_agent("observer"):
            self.assertEqual(a2a.last_known_hosts(max_age_s=0), {"alice": "hostA", "bob": "hostA"})
        self.assertTrue(a2a._ORDER_REFUSED, "a cursor delta never re-asks on its own")
        del self.ls.listings[:]
        a2a._state_path("presence-state.json").unlink()
        a2a._PRESENCE_STATE_MEMO.update(derived=None, at=0.0)
        with self.as_agent("observer"):
            self.assertEqual(a2a.last_known_hosts(max_age_s=0), {"alice": "hostA", "bob": "hostA"})
        self.assertFalse(a2a._ORDER_REFUSED, "the rebuild sees the misorder and asks again")
        # A relay that refuses the parameter and lists newest first anyway is
        # asked once more and then refused, not retried forever.
        self.ls = FakeLogstream(refuses_order=True, ignores_order=True)
        self.online("carol", "dave")
        del self.ls.listings[:]
        a2a._state_path("presence-state.json").unlink()
        a2a._PRESENCE_STATE_MEMO.update(derived=None, at=0.0)
        with self.as_agent("observer"), self.assertRaisesRegex(a2a.LogstreamError, "append order"):
            a2a.last_known_hosts(max_age_s=0)
        self.assertGreaterEqual(len(self.ls.listings), 2)

    def test_only_the_relays_unknown_parameter_answer_downgrades_the_asking(self):
        """Any other failure while asking for the order propagates and
        leaves the client asking, even one whose text mentions `order`."""
        self.online("alice")
        del self.ls.listings[:]
        relay = a2a._rpc

        def failing(tool, args, timeout=30.0):
            if "order" in args:
                raise a2a.LogstreamError("{'code': -32603, 'message': \"Unknown parameter 'order' in backend query\"}", code=-32603)
            return relay(tool, args)

        with mock.patch.object(a2a, "_rpc", side_effect=failing), self.as_agent("observer"):
            with self.assertRaisesRegex(a2a.LogstreamError, "-32603"):
                a2a.last_known_hosts(max_age_s=0)
        self.assertFalse(a2a._ORDER_REFUSED)
        self.assertEqual(len(self.ls.listings), 0, "the relay itself was never read without the order")

    def test_a_listing_out_of_append_order_is_refused_and_never_kept(self):
        """A relay that accepts `order` and lists newest first regardless is
        refused before anything folds, and a one-shot read mirrors nothing."""
        self.ls = FakeLogstream(ignores_order=True)
        self.online("alice", "bob")
        a2a.snapshot_events()
        self.addCleanup(a2a.release_events)
        with self.assertRaisesRegex(a2a.LogstreamError, "append order"):
            a2a.list_events(room=a2a.ROOM_PRESENCE)
        self.assertEqual(a2a._EVENTS_SNAPSHOT, {})
        self.assertEqual(list(Path(self.tmp.name).glob("events-cache-*.json")), [])
        with mock.patch.object(a2a, "_age", return_value=1), self.assertRaises(a2a.LogstreamError):
            a2a.roster()

    def test_offline_removes_an_agent_from_the_roster(self):
        self.online("alice")
        with self.as_agent("alice"), mock.patch.object(a2a, "harness_process", return_value=None), mock.patch.object(a2a, "_pane_for", return_value={}):
            a2a.announce("offline")
        with mock.patch.object(a2a, "_age", return_value=1):
            self.assertNotIn("alice", a2a.roster())

    # ── courier ────────────────────────────────────────────────────────────

    def test_scan_republishes_only_on_change_or_after_the_window(self):
        self.assertEqual((a2a.SCAN_REFRESH_S, a2a.SCAN_TTL_S), (30 * 60, 60 * 60), "the backstop cadence: refresh at thirty minutes, stale at an hour")
        with mock.patch.object(a2a, "_rpc") as rpc, mock.patch.object(a2a, "signed_presence", return_value={"sig": "x"}), mock.patch.object(a2a, "scan_panes", return_value=[{"handle": "kimi-a1"}]):
            state = {}
            a2a.publish_scan(state, verbose=False)
            a2a.publish_scan(state, verbose=False)
            self.assertEqual(rpc.call_count, 1, "an unchanged digest inside the window publishes nothing")
            state["scan_at"] -= a2a.SCAN_REFRESH_S + 1
            a2a.publish_scan(state, verbose=False)
            self.assertEqual(rpc.call_count, 2, "the lapsed window republishes")
            with mock.patch.object(a2a, "scan_panes", return_value=[{"handle": "kimi-a1"}, {"handle": "codex-b2"}]):
                a2a.publish_scan(state, verbose=False)
                self.assertEqual(rpc.call_count, 3, "a changed roster publishes immediately, window or not")

    def test_a_scan_row_drops_out_of_the_roster_at_its_ttl(self):
        agent = {"handle": "ghost", "host": "hostA", "harness": "codex", "pane": {"socket": "/s", "pane": "%7"}}
        with self.as_agent(a2a.courier_id()):
            a2a.append("courier.scan", a2a.ROOM_PRESENCE, metadata=a2a.signed_presence("courier.scan", {"host": "hostA", "agents": [agent]}))
        a2a._HOSTS_CACHE.update(at=0.0)
        self.assertIn("ghost", a2a.roster())
        with mock.patch.object(a2a, "_age", return_value=a2a.SCAN_TTL_S + 1):
            self.assertNotIn("ghost", a2a.roster(), "a scan older than its TTL stops placing an agent")

    def test_courier_delivers_to_local_agents_only_and_stops_on_ack(self):
        self.online("alice", "bob")
        self.presence("far", "hostB", pane="%9")
        self.pin_remote_host("far", "hostB")
        self.run_cli("alice", "chat", "bob", "local")
        self.run_cli("alice", "chat", "far", "remote")
        delivered = []
        with mock.patch.object(a2a, "deliver", side_effect=lambda e, t: delivered.append(t["handle"]) or "tmux"), mock.patch.object(a2a, "_age", return_value=1):
            state = a2a.courier_once({}, "hostA", verbose=False)
        self.assertEqual(delivered, ["bob"])
        courier_acks = [e for e in self.ls.events if e["type"] == "event.ack" and e["from_agent"] == "courier@hostA"]
        self.assertEqual([a2a.ack_note(a) for a in courier_acks], ["delivered to bob via tmux"], "a delivery is acknowledged, naming the recipient")
        self.assertEqual(state["pending"], {}, "one paste is one delivery; hostB's message is not this courier's to keep")
        with mock.patch.object(a2a, "deliver", side_effect=AssertionError("must not redeliver")), mock.patch.object(a2a, "_age", return_value=1):
            a2a.courier_once(state, "hostA", verbose=False)

    def test_courier_state_is_content_blind(self):
        self.online("alice", "bob")
        self.run_cli("alice", "chat", "bob", "the secret plan")
        with mock.patch.object(a2a, "deliver", return_value="unavailable"), mock.patch.object(a2a, "_age", return_value=1):
            state = a2a.courier_once({}, "hostA", verbose=False)
        self.assertEqual(len(state["pending"]), 1)
        self.assertNotIn("secret", json.dumps(state), "no body in durable courier state")
        item = next(iter(state["pending"].values()))
        self.assertEqual(set(item), {"event", "to", "chat", "attempts", "next", "inflight", "scope"}, "ids and bookkeeping only")
        # A held acknowledgement, unverifiable and carrying a marker in its
        # body and metadata, leaves only its id in the state.
        invite = next(e for e in self.ls.events if e["type"] == "chat.invite")
        self.ls.call("mempalace_event_ack", {"event_id": invite["id"], "from_agent": "bob", "status": "applied", "body": "MARKER-in-body"})
        self.ls.events[-1]["metadata"]["note"] = "MARKER-in-metadata"
        with mock.patch.object(a2a, "deliver", return_value="unavailable"), mock.patch.object(a2a, "_age", return_value=1):
            state = a2a.courier_once(state, "hostA", verbose=False)
        self.assertEqual(list(state["held_acks"]), [self.ls.events[-1]["id"]])
        self.assertNotIn("MARKER", json.dumps(state))
        seen = []
        item["next"] = 0
        with mock.patch.object(a2a, "deliver", side_effect=lambda e, t: seen.append(a2a.open_body(e)[0]) or "tmux"), mock.patch.object(a2a, "_age", return_value=1):
            a2a.courier_once(state, "hostA", verbose=False)
        self.assertEqual(seen, ["the secret plan"], "the body is read back at delivery time")

    def test_automatic_delivery_never_writes_terminal_input(self):
        e = {"id": "evt_notice", "type": "chat.message", "body": "notification"}
        self.record_legacy(e["id"])
        for harness in ("codex", "claude", "kimi", "gemini", "opencode", "pi", "omp", "unknown"):
            for native in ("", "thread-bob"):
                target = {"harness": harness, "native_session": native,
                          "pane": {"socket": "/s", "pane": "%1"}}
                with self.subTest(harness=harness, native=native), mock.patch.object(a2a.subprocess, "run", return_value=mock.Mock(returncode=0)) as run:
                    outcome = a2a.deliver(e, target)
                    if harness == "codex" and native:
                        self.assertEqual(outcome, "codex-queue")
                        self.assertEqual(run.call_args.args[0][:4], ["codex", "queue", "--thread", native])
                        self.assertEqual(run.call_count, 1)
                    else:
                        self.assertEqual(outcome, "extension" if harness in ("pi", "omp") else "inbox-only")
                        run.assert_not_called()

    @unittest.skipUnless(shutil.which("tmux"), "tmux is unavailable")
    def test_courier_leaves_a_real_terminal_draft_unsubmitted(self):
        sock = str(Path(self.tmp.name) / "draft.sock")
        result = Path(self.tmp.name) / "submitted"
        ready = Path(self.tmp.name) / "ready"
        completed = Path(self.tmp.name) / "completed"
        base = ["tmux", "-S", sock]
        command = "touch " + shlex.quote(str(ready)) + "; IFS= read -r draft; printf '%s' \"$draft\" > " + shlex.quote(str(result)) + " && touch " + shlex.quote(str(completed))
        subprocess.run(base + ["-f", "/dev/null", "new-session", "-d", "-s", "draft", "bash", "--noprofile", "--norc", "-c", command], check=True)
        self.addCleanup(lambda: subprocess.run(base + ["kill-server"], capture_output=True))
        deadline = time.monotonic() + 5
        while not ready.exists() and time.monotonic() < deadline:
            time.sleep(0.01)
        self.assertTrue(ready.exists())
        subprocess.run(base + ["send-keys", "-t", "draft:0.0", "-l", "unfinished draft"], check=True)
        self.online("alice")
        self.presence("bob", harness="kimi", native_session="", pane="%0")
        self.run_cli("alice", "chat", "bob", "incoming agent notification")
        with mock.patch.object(a2a, "_age", return_value=1):
            target = a2a.roster()["bob"]
            target["pane"] = {"socket": sock, "pane": "%0"}
            with mock.patch.object(a2a, "roster", return_value={"bob": target}), mock.patch.object(a2a, "tmux_sockets", return_value=[sock]):
                state = a2a.courier_once({}, "hostA", verbose=False)
        self.assertEqual(state["pending"], {})
        time.sleep(0.1)
        self.assertFalse(result.exists(), "the courier must not submit the unfinished draft")
        subprocess.run(base + ["send-keys", "-t", "draft:0.0", "-l", " finished"], check=True)
        subprocess.run(base + ["send-keys", "-t", "draft:0.0", "Enter"], check=True)
        deadline = time.monotonic() + 5
        while not completed.exists() and time.monotonic() < deadline:
            time.sleep(0.01)
        self.assertTrue(completed.exists(), "the terminal must finish writing the submitted draft")
        self.assertEqual(result.read_text(), "unfinished draft finished")

    def test_failed_native_queue_never_falls_back_to_terminal(self):
        e = {"id": "evt_notice", "type": "chat.message", "body": "notification"}
        self.record_legacy(e["id"])
        target = {"harness": "codex", "native_session": "thread-bob", "pane": {"socket": "/s", "pane": "%1"}}
        for failure, expected in ((mock.Mock(returncode=1), "uncertain"),
                                  (a2a.subprocess.TimeoutExpired("codex", 20), "uncertain"),
                                  (FileNotFoundError("codex"), "inbox-only")):
            with self.subTest(failure=failure), mock.patch.object(a2a.subprocess, "run") as run:
                if isinstance(failure, Exception):
                    run.side_effect = failure
                else:
                    run.return_value = failure
                self.assertEqual(a2a.deliver(e, target), expected)
                self.assertEqual(run.call_count, 1)
                self.assertEqual(run.call_args.args[0][:2], ["codex", "queue"])

    def test_inbox_only_survives_restart_without_acknowledgement(self):
        self.online("alice")
        self.presence("bob", harness="kimi", native_session="")
        self.run_cli("alice", "chat", "bob", "still unread")
        durable = {}

        def persist(state):
            durable.clear()
            durable.update(json.loads(json.dumps(state)))

        with mock.patch.object(a2a, "_age", return_value=1), mock.patch.object(a2a, "deliver", side_effect=AssertionError("no delivery attempt")):
            state = a2a.courier_once({}, "hostA", verbose=False, persist=persist)
            self.assertEqual(state["pending"], {})
            self.assertFalse(any(item.get("inflight") for item in durable.get("pending", {}).values()))
            state = a2a.courier_once(durable, "hostA", verbose=False, persist=persist)
            self.assertEqual(state["pending"], {})
            self.assertFalse(state.get("settled"))
            self.assertFalse(state.get("acked"))
        self.assertFalse([e for e in self.ls.events if e["type"] == "event.ack"])
        with self.as_agent("bob"):
            fresh = a2a.unread()
            self.assertEqual([a2a.open_body(e)[0] for e in fresh], ["still unread"])
            a2a.mark_read(fresh)
            self.assertEqual(a2a.unread(), [])

    def test_an_uncertain_write_is_never_pasted_again(self):
        self.online("alice", "bob")
        self.run_cli("alice", "chat", "bob", "hello?")
        pastes = []
        with mock.patch.object(a2a, "_codex_queue", side_effect=lambda *a: pastes.append(a) or "uncertain"), mock.patch.object(a2a, "_age", return_value=1):
            state = a2a.courier_once({}, "hostA", verbose=False)
            for item in state["pending"].values():
                item["next"] = 0
            state = a2a.courier_once(state, "hostA", verbose=False)
        self.assertEqual(len(pastes), 1, "bytes may have reached the pane; no second paste")
        self.assertEqual(state["pending"], {})
        acks = [e for e in self.ls.events if e["type"] == "event.ack"]
        self.assertEqual([(a["status"], a2a.ack_note(a)) for a in acks], [("failed", "uncertain for bob")], "recorded as uncertain, never as delivered")
        with self.as_agent("bob"):
            self.assertEqual(len(a2a.unread()), 1, "the recipient's own inbox still has it")

    def test_a_pre_write_failure_is_retried_but_bounded(self):
        self.online("alice", "bob")
        self.run_cli("alice", "chat", "bob", "hello?")
        pastes = []
        with mock.patch.object(a2a, "_codex_queue", side_effect=lambda *a: pastes.append(a) or "unavailable"), mock.patch.object(a2a, "_age", return_value=1):
            state = a2a.courier_once({}, "hostA", verbose=False)
            for _ in range(a2a.COURIER_MAX_ATTEMPTS + 1):
                for item in state["pending"].values():
                    item["next"] = 0
                state = a2a.courier_once(state, "hostA", verbose=False)
        self.assertEqual(len(pastes), a2a.COURIER_MAX_ATTEMPTS)
        self.assertEqual(state["pending"], {})

    def test_after_delivery_only_the_acknowledgement_is_retried(self):
        self.online("alice", "bob")
        self.run_cli("alice", "chat", "bob", "hello?")
        pastes = []
        real = self.ls.call
        fail_acks = {"on": True}

        def flaky(tool, args):
            if tool == "mempalace_event_ack" and fail_acks["on"] and args["from_agent"].startswith("courier@"):
                raise a2a.LogstreamError("writer busy")
            return real(tool, args)

        with mock.patch.object(a2a, "_rpc", side_effect=lambda tool, args, timeout=30.0: flaky(tool, args)), mock.patch.object(a2a, "_codex_queue", side_effect=lambda *a: pastes.append(a) or "codex-queue"), mock.patch.object(a2a, "_age", return_value=1):
            state = a2a.courier_once({}, "hostA", verbose=False)
            self.assertEqual(len(pastes), 1)
            self.assertEqual(json.dumps(state["pending"]).count('"outcome": "delivered"'), 1, "delivered, acknowledgement outstanding")
            for item in state["pending"].values():
                item["next"] = 0
            state = a2a.courier_once(state, "hostA", verbose=False)
            self.assertEqual(len(pastes), 1, "no second paste while retrying the ack")
            fail_acks["on"] = False
            for item in state["pending"].values():
                item["next"] = 0
            state = a2a.courier_once(state, "hostA", verbose=False)
        self.assertEqual(len(pastes), 1)
        self.assertEqual(state["pending"], {})
        self.assertEqual(len([e for e in self.ls.events if e["type"] == "event.ack" and e["from_agent"] == "courier@hostA"]), 1)

    def test_a_courier_that_dies_mid_paste_never_pastes_again_on_restart(self):
        self.online("alice", "bob")
        self.run_cli("alice", "chat", "bob", "hello?")
        pastes = []
        durable = {}

        def persist(st):
            durable.clear()
            durable.update(json.loads(json.dumps(st)))

        def paste_then_die(*a):
            pastes.append(a)
            raise KeyboardInterrupt  # the process is gone before any state write

        with mock.patch.object(a2a, "_codex_queue", side_effect=paste_then_die), mock.patch.object(a2a, "_age", return_value=1):
            with self.assertRaises(KeyboardInterrupt):
                a2a.courier_once({}, "hostA", verbose=False, persist=persist)
        self.assertEqual(len(pastes), 1)
        self.assertTrue(next(iter(durable["pending"].values()))["inflight"], "the in-flight marker was durable before the paste")
        with mock.patch.object(a2a, "_codex_queue", side_effect=lambda *a: pastes.append(a) or "codex-queue"), mock.patch.object(a2a, "_age", return_value=1):
            state = a2a.courier_once(durable, "hostA", verbose=False, persist=persist)
        self.assertEqual(len(pastes), 1, "restart treats the in-flight event as uncertain")
        self.assertEqual(state["pending"], {})
        acks = [e for e in self.ls.events if e["type"] == "event.ack"]
        self.assertEqual([(a["status"], a2a.ack_note(a)) for a in acks], [("failed", "uncertain for bob")], "the uncertain outcome is recorded, once")

    def test_pi_stop_hook_only_announces_presence(self):
        self.online("alice", "bob")
        self.run_cli("alice", "chat", "bob", "while you were busy")
        with mock.patch.object(a2a, "courier_ensure"), mock.patch.object(sys, "stdin", io.StringIO("{}")):
            out = self.run_cli("bob", "hook", "stop", "--harness", "pi")
        self.assertEqual(out, "", "no Claude-style nudge for a harness that ignores hook output")
        with self.as_agent("bob"):
            self.assertEqual(len(a2a.unread()), 1, "left for the Pi extension to deliver")
        self.assertEqual([e["type"] for e in self.ls.events if e["from_agent"] == "bob" and e["room"] == a2a.ROOM_PRESENCE][-1], "agent.idle")

    def test_a_courier_primed_on_an_empty_stream_reads_the_first_event_later(self):
        state = {}
        a2a.prime_cursor(state)
        self.assertIsNone(state["cursor"])
        self.online("alice", "bob")
        self.run_cli("alice", "chat", "bob", "first ever")
        a2a.prime_cursor(state)
        self.assertIsNone(state["cursor"], "priming happens once; the tip is not re-taken")
        with mock.patch.object(a2a, "deliver", return_value="tmux"), mock.patch.object(a2a, "_age", return_value=1):
            state = a2a.courier_once(state, "hostA", verbose=False)
        self.assertEqual([a2a.ack_note(a) for a in self.ls.events if a["type"] == "event.ack"], ["delivered to bob via tmux"])
        fresh = {}
        a2a.prime_cursor(fresh)
        self.assertIsNotNone(fresh["cursor"], "a courier primed on a busy stream starts at its tip")

    def test_a_hookless_host_gets_a_courier_from_any_client_command(self):
        with mock.patch.dict(os.environ, {"A2A_NO_COURIER": ""}), mock.patch.object(a2a, "courier_ensure") as ensure:
            self.run_cli("alice", "who")
            self.stale_presence("ghost")
            self.run_cli("alice", "chat", "ghost", "hi", "--force")
        self.assertEqual(ensure.call_count, 2)
        bootstrap = (Path(__file__).parents[1] / "mempalace" / "noisedeck-bootstrap").read_text()
        self.assertIn("/usr/local/bin/a2a courier", bootstrap, "the host boots a courier before any agent runs")

    def test_a_hostile_body_stays_one_inert_native_notification(self):
        hostile = "ok\x1b[201~\x1b[A\rrm -rf /\n\x9bZ\x7f\x00 done"
        e = {"id": "evt_abcdef123456", "type": "chat.message", "from_agent": "al\x1bice", "correlation_id": "chat-1\r", "body": hostile}
        self.record_legacy(e["id"])
        line = a2a.wake_line(e)
        self.assertEqual(line, a2a.inert(line))
        self.assertIn("ok[201~[A rm -rf / Z done", line)
        with mock.patch.object(a2a.subprocess, "run", return_value=mock.Mock(returncode=0)) as run:
            self.assertEqual(a2a._codex_queue("thread-bob", line), "codex-queue")
        self.assertEqual(run.call_args.args[0], ["codex", "queue", "--thread", "thread-bob", "--message", line])

    def test_wake_line_is_one_line_and_bounded(self):
        e = {"id": "evt_long", "type": "chat.message", "from_agent": "alice", "correlation_id": "chat-1", "body": "x\ny " * 400}
        self.record_legacy(e["id"])
        line = a2a.wake_line(e)
        self.assertNotIn("\n", line)
        self.assertLess(len(line), a2a.WAKE_BODY_CHARS + 120)
        self.assertIn("a2a reply chat-1", line)
        self.assertIn("if live with peers", line)
        self.assertIn(a2a.delivery_tag({"id": "evt_20260903T010000_abcdef123456"}), a2a.wake_line(dict(e, id="evt_20260903T010000_abcdef123456")))
        self.assertNotEqual(a2a.delivery_tag({"id": "evt_1_aaaaaa111111"}), a2a.delivery_tag({"id": "evt_1_bbbbbb222222"}))

    def test_the_ledger_names_each_members_harness_from_presence(self):
        self.online("alice", "bob")
        self.run_cli("alice", "chat", "bob", "hi")
        with self.as_agent("bob"):
            a2a.append("agent.online", a2a.ROOM_PRESENCE, to="*", status="ready", metadata=a2a.signed_presence("agent.online", {"handle": "bob", "harness": "kimi", "host": "hostA"}))
        with self.as_agent("alice"):
            chat = a2a.ledger("alice")["chats"][0]
        self.assertEqual(chat["harnesses"]["bob"], "kimi", "presence says what the agent runs on, whatever its handle looks like")
        self.assertEqual(chat["harnesses"]["alice"], "codex", "the fixture announces its native queue harness in presence")
        self.assertEqual(a2a.harness_of_handle("codex-9f2a", {}), "codex", "a default handle reads its harness")
        self.assertEqual(a2a.harness_of_handle("scout", {}), "unknown", "a chosen name with no presence is unknown, never a guess")
        self.assertEqual(a2a.harness_of_handle("scout", {"scout": "opencode"}), "opencode")

    def test_lifecycle_commands_retry_the_printed_command_at_the_append_boundary(self):
        for actor, command, kind, retry_command in [
            ("alice", "end", "chat.closed", "end"),
            ("bob", "end", "chat.leave", "leave"),
            ("bob", "leave", "chat.leave", "leave"),
            ("bob", "decline", "chat.decline", "decline"),
        ]:
            for committed in (False, True):
                with self.subTest(actor=actor, command=command, committed=committed):
                    self.ls = FakeLogstream()
                    self.online("alice", "bob", "carol")
                    self.run_cli("alice", "chat", "bob,carol", "hi")
                    with self.as_agent("alice"):
                        cid = a2a.open_chat_with("bob", "carol")
                    failed = False

                    def flaky(tool, args, timeout=30.0):
                        nonlocal failed
                        if tool == "mempalace_event_append" and args["type"] == kind and not failed:
                            failed = True
                            if committed:
                                self.ls.call(tool, args)
                            raise a2a.LogstreamError("response lost" if committed else "append unavailable")
                        return self.ls.call(tool, args)

                    with mock.patch.object(a2a, "_rpc", side_effect=flaky):
                        with self.assertRaises(SystemExit) as failure:
                            self.run_cli(actor, command, cid, "done")
                    self.assertTrue(failed, "the injected failure reached the real command's lifecycle append")
                    self.assertEqual(len([e for e in a2a.chat_events(cid) if e["type"] == kind]), int(committed))
                    retry = str(failure.exception).split("`")[1].split()
                    self.assertEqual(retry, ["a2a", retry_command, cid])
                    self.run_cli(actor, *retry[1:])
                    self.run_cli(actor, *retry[1:])
                    events = a2a.chat_events(cid)
                    self.assertEqual([e["type"] for e in events], ["chat.invite", kind])
                    st = a2a.all_chats()[cid]
                    if actor == "alice":
                        self.assertEqual((st["state"], st["close_cause"], st["closed_by"]), ("closed", "opener", "alice"))
                        self.assertEqual(st["routes"][events[-1]["id"]], ["bob", "carol"])
                    else:
                        self.assertEqual(st["state"], "invited")
                        self.assertEqual(st["left"], {"bob"})
                        self.assertIsNone(st["close_cause"])

    def test_failed_shorthand_lifecycle_commands_name_the_exact_safe_retry(self):
        for actor, command, kind in [("alice", "end", "chat.closed"), ("bob", "leave", "chat.leave")]:
            for committed in (False, True):
                with self.subTest(actor=actor, command=command, committed=committed):
                    self.ls = FakeLogstream()
                    self.online("alice", "bob", "carol")
                    self.run_cli("alice", "chat", "bob,carol", "hi")
                    with self.as_agent("alice"):
                        cid = a2a.open_chat_with("bob", "carol")

                    def fail_append(tool, args, timeout=30.0):
                        if tool == "mempalace_event_append" and args["type"] == kind:
                            if committed:
                                self.ls.call(tool, args)
                            raise a2a.LogstreamError("response lost" if committed else "append unavailable")
                        return self.ls.call(tool, args)

                    with mock.patch.object(a2a, "_rpc", side_effect=fail_append):
                        with self.assertRaises(SystemExit) as failure:
                            self.run_cli(actor, command)
                    self.assertIn(f"a2a {command} {cid}", str(failure.exception))
                    self.run_cli("carol", "chat", actor, "unrelated conversation")
                    other = next(e["correlation_id"] for e in a2a.chat_events() if a2a.open_body(e)[0] == "unrelated conversation")
                    self.run_cli(actor, command, cid)
                    self.assertEqual([e["type"] for e in a2a.chat_events(cid)], ["chat.invite", kind])
                    self.assertIn(a2a.all_chats()[other]["state"], ("invited", "open"))
                    self.assertEqual(a2a.all_chats()[other]["left"], set())

    def test_opener_cannot_leave_or_decline_instead_of_ending(self):
        self.online("alice", "bob")
        self.run_cli("alice", "chat", "bob", "hi")
        with self.as_agent("alice"):
            cid = a2a.open_chat_with("bob")
        before = a2a.chat_events(cid)
        for command in ("leave", "decline"):
            with self.subTest(command=command), self.assertRaises(SystemExit) as stop:
                self.run_cli("alice", command, cid)
            self.assertIn("end", str(stop.exception))
        self.assertEqual(a2a.chat_events(cid), before)
        for kind in ("chat.leave", "chat.decline"):
            rejected = self.append_chat("alice", kind, cid, metadata={"lifecycle": "hosted"})
            st = a2a.all_chats()[cid]
            self.assertEqual(st["routes"][rejected["id"]], [])
            self.assertEqual(st["left"], set())
            self.assertEqual(st["last"], before[0]["created_at"])

    def test_departed_member_and_outsider_cannot_end_the_chat(self):
        self.online("alice", "bob", "carol", "mallory")
        self.run_cli("alice", "chat", "bob,carol", "hi")
        with self.as_agent("alice"):
            cid = a2a.open_chat_with("bob", "carol")
        self.run_cli("bob", "leave", cid)
        count = len(a2a.chat_events(cid))
        self.assertIn("left", self.run_cli("bob", "end", cid))
        with self.assertRaises(SystemExit):
            self.run_cli("mallory", "end", cid)
        self.assertEqual(len(a2a.chat_events(cid)), count)
        for sender in ("bob", "mallory"):
            close = self.append_chat(sender, "chat.closed", cid, metadata={"lifecycle": "hosted", "close": "opener"})
            st = a2a.all_chats()[cid]
            self.assertEqual(st["routes"][close["id"]], [])
            self.assertIn(st["state"], ("invited", "open"))

    def test_leave_and_opener_close_obey_log_order_without_a_half_closed_state(self):
        for first in ("leave", "end"):
            with self.subTest(first=first):
                self.ls = FakeLogstream()
                self.register_project()
                self.online("alice", "bob", "carol")
                self.run_cli("alice", "chat", "bob,carol", "hi")
                with self.as_agent("alice"):
                    cid = a2a.open_chat_with("bob", "carol")
                if first == "leave":
                    self.run_cli("bob", "leave", cid)
                    self.run_cli("alice", "end", cid)
                else:
                    self.run_cli("alice", "end", cid)
                    late = self.append_chat("bob", "chat.leave", cid, metadata={"lifecycle": "hosted"})
                st = a2a.all_chats()[cid]
                close = next(e for e in a2a.chat_events(cid) if e["type"] == "chat.closed")
                self.assertEqual((st["state"], st["close_cause"], st["closed_by"]), ("closed", "opener", "alice"))
                self.assertEqual(st["left"], {"bob"} if first == "leave" else set())
                self.assertEqual(st["routes"][close["id"]], ["carol"] if first == "leave" else ["bob", "carol"])
                if first == "end":
                    self.assertEqual(st["routes"][late["id"]], [])

    def test_concurrent_commands_report_the_replayed_close_or_leave(self):
        for outer_actor, outer_command, kind, inner_actor, inner_command in [
            ("alice", "end", "chat.closed", "bob", "leave"),
            ("bob", "leave", "chat.leave", "alice", "end"),
        ]:
            with self.subTest(outer_command=outer_command):
                self.ls = FakeLogstream()
                self.register_project()
                self.online("alice", "bob", "carol")
                self.run_cli("alice", "chat", "bob,carol", "hi")
                with self.as_agent("alice"):
                    cid = a2a.open_chat_with("bob", "carol")
                interleaved = False

                def append_after_competing_command(tool, args, timeout=30.0):
                    nonlocal interleaved
                    if tool == "mempalace_event_append" and args["type"] == kind and not interleaved:
                        interleaved = True
                        self.run_cli(inner_actor, inner_command, cid)
                    return self.ls.call(tool, args)

                with mock.patch.object(a2a, "_rpc", side_effect=append_after_competing_command):
                    output = self.run_cli(outer_actor, outer_command, cid)
                self.assertTrue(interleaved)
                st = a2a.all_chats()[cid]
                self.assertEqual((st["state"], st["close_cause"], st["closed_by"]), ("closed", "opener", "alice"))
                leave = next(e for e in a2a.chat_events(cid) if e["type"] == "chat.leave")
                close = next(e for e in a2a.chat_events(cid) if e["type"] == "chat.closed")
                # The outer command's event was written first, by its
                # moment, however the relay ordered the two appends: the
                # fold and the command's report follow the moments.
                if outer_command == "leave":
                    self.assertIn("you left", output)
                    self.assertEqual(st["left"], {"bob"})
                    self.assertEqual(st["routes"][leave["id"]], ["alice", "carol"])
                    self.assertEqual(st["routes"][close["id"]], ["carol"])
                else:
                    self.assertIn("closed for everyone", output)
                    self.assertEqual(st["left"], set())
                    self.assertEqual(st["routes"][leave["id"]], [])
                    self.assertEqual(st["routes"][close["id"]], ["bob", "carol"])

    def test_departure_cancels_pending_notifications_but_keeps_readable_history(self):
        self.online("alice", "bob", "carol")
        self.run_cli("alice", "chat", "bob,carol", "before leaving")
        with self.as_agent("alice"):
            cid = a2a.open_chat_with("bob", "carol")
        with mock.patch.object(a2a, "deliver", return_value="unavailable"), mock.patch.object(a2a, "_age", return_value=1):
            pending = a2a.courier_once({}, "hostA", verbose=False)
        self.assertTrue(any(item["to"] == "bob" for item in pending["pending"].values()))
        with mock.patch.object(a2a, "acknowledge_chat", return_value=None):
            self.run_cli("bob", "leave", cid, "busy")
        self.run_cli("alice", "reply", cid, "after leaving")
        self.run_cli("alice", "end", cid)
        with self.as_agent("bob"):
            self.assertEqual(a2a.unread(), [], "even previously unread traffic stops after leaving")
            transcript = self.run_cli("bob", "read", cid)
            book = a2a.ledger("bob")["chats"][0]
        self.assertIn("before leaving", transcript)
        self.assertNotIn("after leaving", transcript)
        self.assertEqual(book["state"], "left")
        self.assertEqual(book["handshake"], [])
        self.assertEqual(book["participation_at"], a2a.all_chats()[cid]["left_at"]["bob"])
        self.assertIn("before leaving", [row["text"] for row in book["rows"]])
        self.assertNotIn("after leaving", [row["text"] for row in book["rows"]])
        for item in pending["pending"].values():
            item["next"] = 0
        delivered = []
        with mock.patch.object(a2a, "deliver", side_effect=lambda e, t: delivered.append((a2a.open_body(e)[0], t["handle"])) or "tmux"), mock.patch.object(a2a, "_age", return_value=1):
            after = a2a.courier_once(pending, "hostA", verbose=False)
        self.assertFalse(any(who == "bob" for _, who in delivered))
        self.assertTrue(any(who == "carol" for _, who in delivered))
        self.assertFalse(any(item["to"] == "bob" for item in after["pending"].values()))

    def test_history_keeps_authored_and_received_events_but_not_traffic_after_departure(self):
        self.online("alice", "bob", "carol")
        self.run_cli("alice", "chat", "bob,carol", "original invitation")
        cid = a2a.chat_events()[0]["correlation_id"]
        self.run_cli("bob", "reply", cid, "Bob authored this")
        self.run_cli("carol", "reply", cid, "Carol sent this before Bob left")
        prior = a2a.chat_events(cid)[-1]
        with mock.patch.object(a2a, "acknowledge_chat", return_value=None):
            self.run_cli("bob", "leave", cid, "Bob's departure")
        self.run_cli("alice", "reply", cid, "Alice and Carol only")
        later = a2a.chat_events(cid)[-1]
        before_acks = len([e for e in self.ls.events if e["type"] == "event.ack"])
        transcript = self.run_cli("bob", "read", cid)
        book = a2a.ledger("bob")["chats"][0]
        after_acks = len([e for e in self.ls.events if e["type"] == "event.ack"])
        self.assertEqual(after_acks, before_acks + 1, "read acknowledges the displayed inbound message")
        self.run_cli("bob", "read", cid)
        self.assertEqual(len([e for e in self.ls.events if e["type"] == "event.ack"]), after_acks,
                         "reading the same history twice does not duplicate receipts")
        for visible in ("original invitation", "Bob authored this", "Carol sent this before Bob left", "Bob's departure"):
            self.assertIn(visible, transcript)
            self.assertIn(visible, [row["text"] for row in book["rows"]])
        self.assertNotIn("Alice and Carol only", transcript)
        self.assertNotIn("Alice and Carol only", json.dumps(book))
        self.assertEqual(book["state"], "left")
        for viewer in ("alice", "carol"):
            self.assertIn("Alice and Carol only", self.run_cli(viewer, "read", cid))
            self.assertIn("Alice and Carol only", [row["text"] for row in a2a.ledger(viewer)["chats"][0]["rows"]])
        self.assertIn("0 marked read", self.run_cli("bob", "mark-read", prior["id"]),
                      "the displayed message is already acknowledged")
        self.assertIn("0 marked read", self.run_cli("bob", "mark-read", later["id"]))
        acks = [e for e in self.ls.events if e["type"] == "event.ack" and e["from_agent"] == "bob"]
        self.assertTrue(any(a2a.metadata_of(e).get("ack_of") == prior["id"] for e in acks))
        self.assertFalse(any(a2a.metadata_of(e).get("ack_of") == later["id"] for e in acks))

    def test_new_members_keep_prior_context_but_departures_cut_off_later_history(self):
        self.online("alice", "bob", "carol", "dave")
        self.run_cli("alice", "chat", "bob", "before Carol joined")
        cid = a2a.chat_events()[0]["correlation_id"]
        self.run_cli("alice", "add", cid, "carol")
        self.run_cli("alice", "reply", cid, "while Carol participated")
        self.run_cli("carol", "leave", cid, "Carol's farewell")
        self.run_cli("alice", "add", cid, "dave")
        self.run_cli("alice", "reply", cid, "after Carol left")
        for event in self.ls.events:
            if event.get("correlation_id") == cid:
                event["created_at"] = "2026-09-05T01:00:00Z"
        for viewer in ("carol", "dave"):
            transcript = self.run_cli(viewer, "read", cid)
            rows = a2a.ledger(viewer)["chats"][0]["rows"]
            for visible in ("before Carol joined", "while Carol participated", "Carol's farewell"):
                self.assertIn(visible, transcript)
                self.assertIn(visible, [row["text"] for row in rows])
            if viewer == "carol":
                self.assertNotIn("after Carol left", transcript)
                self.assertNotIn("added dave", transcript)
                self.assertNotIn("after Carol left", [row["text"] for row in rows])
                self.assertFalse(any(row["member"] == "dave" for row in rows))
            else:
                self.assertIn("after Carol left", transcript)
                self.assertIn("after Carol left", [row["text"] for row in rows])
                self.assertTrue(any(row["member"] == "dave" for row in rows))

    def test_departure_during_a_courier_pass_cancels_the_unstarted_paste(self):
        self.online("alice", "bob", "carol")
        self.run_cli("alice", "chat", "bob,carol", "before leaving")
        with self.as_agent("alice"):
            cid = a2a.open_chat_with("bob", "carol")
        real_roster = a2a.roster

        def roster_after_departure(*args, **kwargs):
            online = real_roster(*args, **kwargs)
            with mock.patch.object(a2a, "acknowledge_chat", return_value=None):
                self.run_cli("bob", "leave", cid)
            return online

        delivered = []
        with mock.patch.object(a2a, "roster", side_effect=roster_after_departure), mock.patch.object(a2a, "deliver", side_effect=lambda e, t: delivered.append(t["handle"]) or "tmux"), mock.patch.object(a2a, "_age", return_value=1):
            a2a.courier_once({}, "hostA", verbose=False)
        self.assertNotIn("bob", delivered, "the scan saw Bob before he left; delivery checks again")
        self.assertIn("carol", delivered)

    def test_departure_preserves_acknowledgement_of_a_settled_or_uncertain_paste(self):
        for outcome in ("delivered", "uncertain", "inflight"):
            with self.subTest(outcome=outcome):
                self.ls = FakeLogstream()
                self.online("alice", "bob")
                self.run_cli("alice", "chat", "bob", "already pasted")
                invite = next(e for e in a2a.chat_events() if e["type"] == "chat.invite")
                cid = invite["correlation_id"]
                item = {"event": invite["id"], "to": "bob", "chat": cid, "attempts": 0, "next": 0, "inflight": True}
                if outcome != "inflight":
                    item.update(outcome=outcome, rung="tmux")
                pending = {"cursor": invite["id"], "pending": {f"{invite['id']}|bob": item}}
                with mock.patch.object(a2a, "acknowledge_chat", return_value=None):
                    self.run_cli("bob", "leave", cid)
                with mock.patch.object(a2a, "deliver", return_value="tmux") as deliver, mock.patch.object(a2a, "_age", return_value=1):
                    result = a2a.courier_once(pending, "hostA", verbose=False)
                self.assertFalse(any(call.args[1]["handle"] == "bob" for call in deliver.call_args_list))
                self.assertEqual(result["pending"], {})
                acks = [e for e in self.ls.events if e["type"] == "event.ack" and a2a.metadata_of(e).get("ack_of") == invite["id"]]
                self.assertEqual([a2a.ack_note(e) for e in acks], ["delivered to bob via tmux" if outcome == "delivered" else "uncertain for bob"])

    def test_departed_member_can_acknowledge_an_already_delivered_event(self):
        self.online("alice", "bob")
        self.run_cli("alice", "chat", "bob", "already on your pane")
        invite = next(e for e in a2a.chat_events() if e["type"] == "chat.invite")
        with mock.patch.object(a2a, "acknowledge_chat", return_value=None):
            self.run_cli("bob", "leave", invite["correlation_id"])
        self.assertIn("1 marked read", self.run_cli("bob", "mark-read", invite["id"]))
        self.assertEqual(a2a.ledger("alice")["chats"][0]["rows"][0]["to"], {"bob": "read"})
        with self.as_agent("bob"):
            self.assertEqual(a2a.unread(), [])

    def test_wait_and_status_respect_a_members_departure(self):
        self.online("alice", "bob")
        self.run_cli("alice", "chat", "bob", "unread before leaving")
        with self.as_agent("alice"):
            cid = a2a.open_chat_with("bob")
        with mock.patch.object(a2a, "acknowledge_chat", return_value=None):
            self.run_cli("bob", "leave", cid)
        with mock.patch.object(a2a.time, "monotonic", side_effect=[0, 0, 2]), self.as_agent("bob"):
            self.assertEqual(a2a._wait(cid, 1), [])
        self.assertNotIn(cid, self.run_cli("bob", "status"))
        self.assertIn("left", self.run_cli("bob", "status", "--all"))
        self.run_cli("alice", "end", cid)
        self.assertNotIn(cid, self.run_cli("bob", "status"))
        self.assertIn("  State: left\n", self.run_cli("bob", "status", "--all"))
        self.assertEqual(a2a.ledger("bob")["chats"][0]["state"], "left")

    def test_another_member_cannot_re_add_someone_who_left(self):
        self.online("alice", "bob", "carol")
        self.run_cli("alice", "chat", "bob,carol", "hi")
        with self.as_agent("alice"):
            cid = a2a.open_chat_with("bob", "carol")
        self.run_cli("bob", "leave", cid)
        with self.assertRaises(SystemExit):
            self.run_cli("alice", "add", cid, "bob")
        before = a2a.all_chats()[cid]
        event = self.append_chat("carol", "chat.join", cid, metadata={"lifecycle": "hosted", "member": "bob"})
        after = a2a.all_chats()[cid]
        self.assertEqual(after["routes"][event["id"]], [])
        self.assertEqual({k: v for k, v in after.items() if k != "routes"},
                         {k: v for k, v in before.items() if k != "routes"})
        self.assertEqual(a2a.ledger("bob")["chats"][0]["state"], "left")
        self.run_cli("alice", "chat", "bob", "a separate invitation")
        self.assertEqual(len(a2a.ledger("bob")["chats"]), 2)

    def test_bare_opener_close_metadata_does_not_adopt_or_end_a_legacy_chat(self):
        for state in ("invited", "open", "closing"):
            with self.subTest(state=state):
                self.ls = FakeLogstream()
                cid = self.legacy_chat("alice", "bob", "carol")
                if state == "open":
                    self.append_chat("bob", "chat.message", cid, at="2026-09-05T01:00:00Z")
                elif state == "closing":
                    self.append_chat("bob", "chat.close-request", cid, at="2026-09-05T01:00:00Z")
                before = a2a.all_chats()[cid]
                for at in ("2026-09-05T01:00:01Z", "2026-09-05T01:10:00Z"):
                    close = self.append_chat("alice", "chat.closed", cid, metadata={"close": "opener"}, at=at)
                    st = a2a.all_chats()[cid]
                    self.assertEqual(st["routes"][close["id"]], [])
                    self.assertEqual({k: v for k, v in st.items() if k != "routes"},
                                     {k: v for k, v in before.items() if k != "routes"})
                reply = self.append_chat("bob", "chat.message", cid, body="the discussion continues", at="2026-09-05T01:11:00Z")
                st = a2a.all_chats()[cid]
                self.assertEqual(st["state"], "closing" if state == "closing" else "open")
                self.assertEqual(st["routes"][reply["id"]], ["alice", "carol"])
                self.assertFalse(st["hosted"])
                self.assertFalse(st["forced"])
                self.assertIsNone(st["close_cause"])
                late = self.append_chat("alice", "chat.closed", cid, metadata={"close": "opener"}, at="2026-09-05T01:21:00Z")
                after = a2a.all_chats()[cid]
                self.assertEqual(after["routes"][late["id"]], [])
                self.assertEqual({k: v for k, v in after.items() if k != "routes"},
                                 {k: v for k, v in st.items() if k != "routes"})

    def test_legacy_force_close_metadata_does_not_change_its_original_meaning(self):
        cid = self.legacy_chat("alice", "bob", "carol")
        self.append_chat("alice", "chat.close-request", cid, at="2026-09-05T01:00:00Z")
        early = self.append_chat("alice", "chat.closed", cid, metadata={"close": "opener"}, at="2026-09-05T01:09:59Z")
        self.assertEqual(a2a.all_chats()[cid]["routes"][early["id"]], [])
        self.assertEqual(a2a.all_chats()[cid]["state"], "closing")
        forced = self.append_chat("alice", "chat.closed", cid, metadata={"close": "opener"}, at="2026-09-05T01:10:00Z")
        st = a2a.all_chats()[cid]
        self.assertEqual((st["state"], st["close_cause"], st["closed_by"]), ("closed", "forced", "alice"))
        self.assertEqual(st["routes"][forced["id"]], ["bob", "carol"])
        self.assertTrue(st["forced"])
        self.assertFalse(st["hosted"])

    def test_legacy_live_departures_are_already_left_in_the_members_ledger(self):
        for state in ("invited", "open", "closing"):
            for leaver in ("alice", "bob"):
                with self.subTest(state=state, leaver=leaver):
                    self.ls = FakeLogstream()
                    cid = self.legacy_chat("alice", "bob", "carol")
                    if state == "open":
                        self.append_chat("carol", "chat.message", cid, at="2026-09-05T01:00:00Z")
                    elif state == "closing":
                        self.append_chat("carol", "chat.close-request", cid, at="2026-09-05T01:00:00Z")
                    self.append_chat(leaver, "chat.leave", cid, at="2026-09-05T01:01:00Z")
                    before = a2a.all_chats()[cid]
                    self.assertFalse(before["hosted"])
                    self.assertEqual(before["state"], state)
                    self.assertNotIn(cid, self.run_cli(leaver, "status"))
                    self.assertIn("  State: left\n", self.run_cli(leaver, "status", "--all"))
                    self.assertIn(f"  State: {state}\n", self.run_cli("carol", "status"))
                    for age in (0, a2a.WAITING_AFTER_S + 1):
                        with mock.patch.object(a2a, "_age", return_value=age):
                            left = a2a.ledger(leaver)["chats"][0]
                            active = a2a.ledger("carol")["chats"][0]
                        self.assertEqual(left["state"], "left")
                        self.assertEqual(left["participation_at"], "2026-09-05T01:01:00Z")
                        self.assertEqual(active["state"], "waiting" if state == "invited" and age else state)
                    self.assertEqual(a2a.all_chats()[cid], before, "reading the projection does not adopt or change history")

    def test_legacy_terminal_views_keep_their_original_cause_after_departure(self):
        for cause in ("quorum", "departure", "declined", "forced"):
            with self.subTest(cause=cause):
                self.ls = FakeLogstream()
                cid = self.legacy_chat("alice", "bob") if cause == "departure" else self.legacy_chat("alice", "bob", "carol")
                self.append_chat("bob", "chat.decline" if cause == "declined" else "chat.leave", cid)
                if cause in ("quorum", "forced"):
                    self.append_chat("alice", "chat.close-request", cid, at="2026-09-05T01:00:00Z")
                    self.append_chat("carol" if cause == "quorum" else "alice",
                                     "chat.close-ack" if cause == "quorum" else "chat.closed",
                                     cid, at="2026-09-05T01:10:00Z")
                st = a2a.all_chats()[cid]
                self.assertFalse(st["hosted"])
                self.assertEqual(st["close_cause"], cause)
                for viewer in ("alice", "bob"):
                    book = a2a.ledger(viewer)["chats"][0]
                    self.assertEqual(book["state"], cause if cause in ("declined", "forced") else "closed")
                    self.assertEqual(book["close_cause"], cause)
                    self.assertNotIn(cid, self.run_cli(viewer, "status"))
                    terminal = "declined" if cause == "declined" else "closed"
                    self.assertIn(f"  State: {terminal}\n", self.run_cli(viewer, "status", "--all"))

    def test_a_legacy_departed_opener_can_end_before_or_after_hosted_adoption(self):
        for state in ("invited", "open", "closing"):
            for adopted in (False, True):
                with self.subTest(state=state, adopted=adopted):
                    self.ls = FakeLogstream()
                    self.online("alice", "bob", "carol")
                    cid = self.legacy_chat("alice", "bob", "carol")
                    if state == "open":
                        self.append_chat("bob", "chat.message", cid, body="before departure")
                    elif state == "closing":
                        self.append_chat("bob", "chat.close-request", cid)
                    self.append_chat("alice", "chat.leave", cid)
                    before = a2a.all_chats()[cid]
                    self.assertEqual(before["state"], state)
                    self.assertEqual(before["left"], {"alice"})
                    if adopted:
                        self.run_cli("bob", "reply", cid, "continue without Alice")
                        self.assertTrue(a2a.all_chats()[cid]["hosted"])
                    self.assertIn("closed for everyone", self.run_cli("alice", "end", cid))
                    st = a2a.all_chats()[cid]
                    self.assertEqual((st["state"], st["close_cause"], st["closed_by"]), ("closed", "opener", "alice"))
                    self.assertTrue(st["hosted"])
                    self.assertEqual(st["left"], {"alice"})
                    self.assertEqual(st["left_at"], before["left_at"])
                    close = next(e for e in a2a.chat_events(cid) if e["type"] == "chat.closed")
                    self.assertEqual(st["routes"][close["id"]], ["bob", "carol"])
                    self.assertEqual(a2a.metadata_of(close)["lifecycle"], "hosted")
                    self.assertEqual(a2a.metadata_of(close)["close"], "opener")
                    self.assertIn("already closed", self.run_cli("alice", "end", cid))
                    self.assertHumanEvent("alice closed", self.run_cli("alice", "read", cid))
                    self.assertEqual(len([e for e in a2a.chat_events(cid) if e["type"] == "chat.closed"]), 1)
                    self.assertEqual(a2a.ledger("alice")["chats"][0]["state"], "left")
                    with self.as_agent("alice"):
                        self.assertEqual(a2a.unread(), [])
                    delivered = []
                    with mock.patch.object(a2a, "deliver", side_effect=lambda e, t: delivered.append(t["handle"]) or "tmux"), mock.patch.object(a2a, "_age", return_value=1):
                        a2a.courier_once({}, "hostA", verbose=False)
                    self.assertNotIn("alice", delivered, "ending does not restore the opener's participation")

    def test_a_legacy_departed_opener_can_end_after_every_other_member_leaves(self):
        self.online("alice", "bob", "carol")
        cid = self.legacy_chat("alice", "bob", "carol")
        self.append_chat("alice", "chat.leave", cid)
        self.run_cli("bob", "reply", cid, "continue")
        self.run_cli("bob", "end", cid)
        self.run_cli("carol", "end", cid)
        self.assertHumanEvent("carol left", self.run_cli("carol", "read", cid))
        self.assertTrue(any(row["kind"] == "left" and row["from"] == "carol" for row in a2a.ledger("carol")["chats"][0]["rows"]))
        self.assertEqual(a2a.ledger_index()["chats"][0]["last_row"], a2a.chat_events(cid)[-1]["id"])
        before = a2a.all_chats()[cid]
        self.assertEqual(before["state"], "open")
        self.assertEqual(before["left"], {"alice", "bob", "carol"})
        self.run_cli("alice", "end", cid)
        st = a2a.all_chats()[cid]
        self.assertEqual((st["state"], st["closed_by"]), ("closed", "alice"))
        self.assertEqual(st["left"], before["left"])
        self.assertEqual(st["left_at"], before["left_at"])
        close = next(e for e in a2a.chat_events(cid) if e["type"] == "chat.closed")
        self.assertEqual(st["routes"][close["id"]], [])

    def test_departed_opener_end_retries_across_a_lost_append_response(self):
        for committed in (False, True):
            with self.subTest(committed=committed):
                self.ls = FakeLogstream()
                self.register_project()
                self.online("alice", "bob", "carol")
                cid = self.legacy_chat("alice", "bob", "carol")
                self.append_chat("alice", "chat.leave", cid)
                self.run_cli("bob", "reply", cid, "continue")
                left_at = a2a.all_chats()[cid]["left_at"].copy()

                def fail_close(tool, args, timeout=30.0):
                    if tool == "mempalace_event_append" and args["type"] == "chat.closed":
                        if committed:
                            self.ls.call(tool, args)
                        raise a2a.LogstreamError("response lost" if committed else "append unavailable")
                    return self.ls.call(tool, args)

                with mock.patch.object(a2a, "_rpc", side_effect=fail_close):
                    with self.assertRaises(SystemExit) as failure:
                        self.run_cli("alice", "end", cid)
                self.assertIn(f"a2a end {cid}", str(failure.exception))
                self.assertEqual(len([e for e in a2a.chat_events(cid) if e["type"] == "chat.closed"]), int(committed))
                self.run_cli("alice", "end", cid)
                self.run_cli("alice", "end", cid)
                closes = [e for e in a2a.chat_events(cid) if e["type"] == "chat.closed"]
                self.assertEqual(len(closes), 1)
                st = a2a.all_chats()[cid]
                self.assertEqual((st["state"], st["closed_by"]), ("closed", "alice"))
                self.assertEqual(st["routes"][closes[0]["id"]], ["bob", "carol"])
                self.assertEqual(st["left_at"], left_at)

    def test_departed_opener_close_authority_does_not_restore_other_actions(self):
        for adopted in (False, True):
            with self.subTest(adopted=adopted):
                self.ls = FakeLogstream()
                self.online("alice", "bob", "carol", "dave")
                cid = self.legacy_chat("alice", "bob", "carol", "dave")
                self.append_chat("alice", "chat.leave", cid)
                self.append_chat("carol", "chat.leave", cid)
                if adopted:
                    self.run_cli("bob", "reply", cid, "continue")
                before = a2a.all_chats()[cid]
                for command, extra in [("reply", ["not participating"]), ("add", ["erin"])]:
                    with self.assertRaises(SystemExit):
                        self.run_cli("alice", command, cid, *extra)
                for sender, kind, metadata in [
                    ("alice", "chat.message", {"lifecycle": "hosted"}),
                    ("alice", "chat.accept", {"lifecycle": "hosted"}),
                    ("alice", "chat.join", {"lifecycle": "hosted", "member": "erin"}),
                    ("alice", "chat.closed", {"close": "opener"}),
                    ("alice", "chat.closed", {}),
                    ("bob", "chat.closed", {"lifecycle": "hosted", "close": "opener"}),
                    ("carol", "chat.closed", {"lifecycle": "hosted", "close": "opener"}),
                    ("mallory", "chat.closed", {"lifecycle": "hosted", "close": "opener"}),
                ]:
                    event = self.append_chat(sender, kind, cid, metadata=metadata)
                    after = a2a.all_chats()[cid]
                    self.assertEqual(after["routes"][event["id"]], [], (sender, kind, metadata))
                    self.assertEqual({k: v for k, v in after.items() if k != "routes"},
                                     {k: v for k, v in before.items() if k != "routes"})
                self.run_cli("alice", "end", cid)
                st = a2a.all_chats()[cid]
                self.assertEqual(st["closed_by"], "alice")
                self.assertEqual(st["left"], {"alice", "carol"})

    def test_departed_opener_cannot_rewrite_a_terminal_legacy_history(self):
        cid = self.legacy_chat("alice", "bob", "carol")
        self.append_chat("bob", "chat.close-request", cid)
        self.append_chat("carol", "chat.close-ack", cid)
        self.append_chat("alice", "chat.leave", cid)
        before = a2a.all_chats()[cid]
        self.assertEqual((before["state"], before["close_cause"]), ("closed", "departure"))
        count = len(a2a.chat_events(cid))
        self.assertIn("already closed", self.run_cli("alice", "end", cid))
        self.assertEqual(len(a2a.chat_events(cid)), count)
        late = self.append_chat("alice", "chat.closed", cid, metadata={"lifecycle": "hosted", "close": "opener"})
        after = a2a.all_chats()[cid]
        self.assertEqual(after["routes"][late["id"]], [])
        self.assertEqual({k: v for k, v in after.items() if k != "routes"},
                         {k: v for k, v in before.items() if k != "routes"})

    def test_a_valid_new_event_adopts_a_live_legacy_chat_and_retires_votes(self):
        for command, actor, extra, expected in [
            ("reply", "bob", ["still talking"], "open"),
            ("accept", "bob", [], "open"),
            ("leave", "bob", [], "open"),
            ("decline", "bob", [], "open"),
            ("end", "alice", [], "closed"),
            ("add", "alice", ["carol"], "open"),
        ]:
            with self.subTest(command=command):
                self.ls = FakeLogstream()
                self.register_project()
                self.online("alice", "bob", "carol")
                cid = self.legacy_chat("alice", "bob")
                self.append_chat("bob", "chat.close-request", cid)
                self.assertFalse(a2a.all_chats()[cid]["hosted"])
                self.run_cli(actor, command, cid, *extra)
                st = a2a.all_chats()[cid]
                self.assertTrue(st["hosted"])
                self.assertEqual(st["state"], expected)
                self.assertEqual(st["agreed"], set())
                self.assertEqual(st["agreed_at"], {})
                self.assertIsNone(st["close_requested_by"])
                self.assertEqual(a2a.ledger("alice")["chats"][0]["handshake"], [])

    def test_invalid_hosted_actions_leave_the_existing_chat_and_ledger_unchanged(self):
        for initial in ("invited", "open", "closing"):
            for adopted in (False, True):
                self.ls = FakeLogstream()
                cid = self.legacy_chat("alice", "bob", "carol", "dave")
                self.append_chat("dave", "chat.leave", cid)
                if initial == "open":
                    self.append_chat("bob", "chat.message", cid, body="discussion")
                elif initial == "closing":
                    self.append_chat("bob", "chat.close-request", cid)
                if adopted:
                    self.stale_presence("erin")
                    self.run_cli("alice", "add", cid, "erin")
                baseline = list(self.ls.events)
                sequence = self.ls.n
                history = a2a.chat_events(cid)
                before = a2a.chat_state(history)
                with mock.patch.object(a2a, "_age", return_value=0):
                    book = a2a.ledger("alice")
                invalid = [
                    ("chat.invite", {"members": ["alice", "bob", "carol"]}, ""),
                    ("chat.join", {}, ""),
                    ("chat.unknown", {}, ""),
                    ("event.ack", {}, ""),
                ] + [("chat.join", {"member": member}, "") for member in (
                    None, "", 1, False, [], {}, "*", cid, "alice", "bob", "dave",
                    "not a handle", "x" * 65,
                )] + [("chat.message", {}, body) for body in ("", " \n\t ", None, 1, False, ["text"], {"text": "text"})]
                for kind, metadata, body in invalid:
                    with self.subTest(initial=initial, adopted=adopted, kind=kind, metadata=metadata, body=body):
                        self.ls.events = list(baseline)
                        self.ls.n = sequence
                        event = self.ls._append({"type": kind, "stream": a2a.STREAM, "room": a2a.ROOM_CHAT,
                                                "from_agent": "alice", "to_agent": cid, "correlation_id": cid,
                                                "metadata": {"lifecycle": "hosted", **metadata}, "body": body})
                        event["created_at"] = "2026-09-06T02:00:00Z"
                        after = a2a.chat_state(history + [event])
                        self.assertEqual({k: v for k, v in after.items() if k != "routes"},
                                         {k: v for k, v in before.items() if k != "routes"})
                        self.assertEqual(after["routes"], {**before["routes"], event["id"]: []})
                        with mock.patch.object(a2a, "_age", return_value=0):
                            again = a2a.ledger("alice")
                        # The invalid event is reported, unverified, and changes nothing else.
                        strip = lambda b: {**b, "chats": [{k: v for k, v in c.items() if k != "unreadable"} for c in b["chats"]]}  # noqa: E731
                        self.assertEqual(strip(again), strip(book))
                        if kind in a2a.CHAT_TYPES:
                            self.assertEqual([u["id"] for c in again["chats"] for u in c["unreadable"]], [event["id"]])

    def test_hosted_validation_does_not_reinterpret_old_unmarked_events(self):
        cid = self.legacy_chat("alice", "bob", "carol")
        self.append_chat("bob", "chat.close-request", cid, at="2026-09-05T01:00:00Z")
        before = a2a.all_chats()[cid]
        duplicate = self.append_chat("alice", "chat.invite", cid, body="old duplicate",
                                     metadata={"members": ["alice", "bob", "carol"]}, at="2026-09-05T01:01:00Z")
        after = a2a.all_chats()[cid]
        self.assertEqual(after, {**before, "last": duplicate["created_at"],
                                "accepted": before["accepted"] | {duplicate["id"]},
                                "routes": {**before["routes"], duplicate["id"]: ["bob", "carol"]}})
        missing = self.append_chat("alice", "chat.join", cid, at="2026-09-05T01:02:00Z")
        self.assertEqual(a2a.all_chats()[cid], {**after, "routes": {**after["routes"], missing["id"]: []}})

    def test_add_refuses_the_chat_id_as_a_member_without_adopting_legacy_history(self):
        cid = self.legacy_chat("alice", "bob", "carol")
        self.append_chat("bob", "chat.close-request", cid)
        before = a2a.all_chats()[cid]
        events = a2a.chat_events(cid)
        with self.assertRaises(SystemExit):
            self.run_cli("alice", "add", cid, cid)
        self.assertEqual(a2a.chat_events(cid), events)
        self.assertEqual(a2a.all_chats()[cid], before)

    def test_rejected_hosted_events_cannot_change_a_legacy_lifecycle(self):
        cid = self.legacy_chat("alice", "bob", "carol")
        self.append_chat("bob", "chat.close-request", cid)
        before = a2a.all_chats()[cid]
        for sender, kind, extra in [
            ("mallory", "chat.message", {}),
            ("bob", "chat.closed", {"close": "opener"}),
            ("alice", "chat.leave", {}),
            ("alice", "chat.decline", {}),
            ("bob", "chat.close-request", {}),
            ("carol", "chat.close-ack", {}),
        ]:
            with self.subTest(sender=sender, kind=kind):
                event = self.append_chat(sender, kind, cid, metadata={"lifecycle": "hosted", **extra})
                st = a2a.all_chats()[cid]
                self.assertEqual(st["routes"][event["id"]], [])
                self.assertEqual({k: v for k, v in st.items() if k != "routes"},
                                 {k: v for k, v in before.items() if k != "routes"})

    def test_terminal_histories_remain_immutable_after_the_upgrade(self):
        for cause in ("quorum", "departure", "declined", "forced", "opener"):
            with self.subTest(cause=cause):
                self.ls = FakeLogstream()
                cid = self.legacy_chat("alice", "bob")
                if cause in ("quorum", "forced"):
                    self.append_chat("bob", "chat.close-request", cid, at="2026-09-05T01:00:00Z")
                if cause == "quorum":
                    self.append_chat("alice", "chat.close-ack", cid)
                elif cause == "departure":
                    self.append_chat("bob", "chat.leave", cid)
                elif cause == "declined":
                    self.append_chat("bob", "chat.decline", cid)
                elif cause == "forced":
                    self.append_chat("bob", "chat.closed", cid, at="2026-09-05T01:10:00Z")
                else:
                    self.append_chat("alice", "chat.closed", cid, metadata={"lifecycle": "hosted", "close": "opener"})
                before = a2a.all_chats()[cid]
                self.assertEqual(before["close_cause"], cause)
                self.assertEqual(a2a.ledger("alice")["chats"][0]["close_cause"], cause)
                for kind in ("chat.message", "chat.leave", "chat.join", "chat.closed"):
                    late = self.append_chat("alice", kind, cid, metadata={"lifecycle": "hosted", "close": "opener", "member": "carol"}, at="2026-09-06T01:00:00Z")
                    after = a2a.all_chats()[cid]
                    self.assertEqual(after["routes"][late["id"]], [])
                    self.assertEqual({k: v for k, v in after.items() if k != "routes"},
                                     {k: v for k, v in before.items() if k != "routes"})

    def test_a_legacy_departure_that_completes_a_quorum_names_no_closer(self):
        cid = self.legacy_chat("alice", "bob", "carol")
        self.append_chat("bob", "chat.close-request", cid)
        self.append_chat("carol", "chat.close-ack", cid)
        self.append_chat("alice", "chat.leave", cid)
        st = a2a.all_chats()[cid]
        self.assertEqual((st["state"], st["close_cause"], st["closed_by"]), ("closed", "departure", None))
        self.assertEqual(st["left"], {"alice"})



@unittest.skipUnless(HAVE_NACL, "PyNaCl is not installed for this interpreter")
class HostKeyTest(A2ACase):
    """One key per host, pinned by a person; sending refuses otherwise."""

    def test_init_is_owner_only_and_never_overwrites(self):
        path = Path(self.tmp.name) / "host" / "host-key"
        path.unlink()
        fp = self.run_cli("alice", "key", "init").strip()
        self.assertEqual(len(fp.replace("-", "")), 24)
        self.assertEqual(oct(path.stat().st_mode & 0o777), "0o600")
        with self.assertRaises(SystemExit) as ctx:
            self.run_cli("alice", "key", "init")
        self.assertIn("refusing to overwrite", str(ctx.exception))
        self.assertEqual(self.run_cli("alice", "key", "show").strip(), fp)

    def test_fingerprint_round_trips_through_announce_and_show(self):
        fp = self.run_cli("alice", "key", "show").strip()
        out = self.run_cli("alice", "key", "announce")
        self.assertIn(fp, out)
        announced = a2a.announced_keys()
        self.assertEqual(announced["hostA"]["fingerprint"], fp)
        # A forged announce whose fingerprint does not match its key is ignored.
        with self.as_agent("mallory"):
            a2a.append("key.announce", a2a.ROOM_KEYS, to="*", status="ready", metadata={"host": "hostZ", "fingerprint": "aaaa-" * 5 + "aaaa", "public_key": announced["hostA"]["public_key"]})
        self.assertNotIn("hostZ", a2a.announced_keys())

    def test_same_host_chat_needs_no_pin(self):
        self.online("alice", "bob")
        out = self.run_cli("alice", "chat", "bob", "hello")
        self.assertIn("chat opened with bob", out)

    def test_unpinned_changed_and_pinned_remote_hosts(self):
        self.online("alice")
        self.online_on("far", "hostB")
        # hostB has announced nothing yet: refused, naming the missing announce.
        with self.assertRaises(SystemExit) as ctx:
            self.run_cli("alice", "chat", "far", "hello")
        self.assertIn("announced no key", str(ctx.exception))
        self.assertEqual([e for e in self.ls.events if e["room"] == "chat"], [])

        state_b, host_b = self.other_host("hostB")
        with state_b, host_b:
            fp_b = self.run_cli("far", "key", "show").strip()
            self.run_cli("far", "key", "announce")
        # Announced but not pinned on hostA: refused, naming the fingerprint to read.
        with self.assertRaises(SystemExit) as ctx:
            self.run_cli("alice", "chat", "far", "hello")
        self.assertIn("is not pinned", str(ctx.exception))
        self.assertIn(fp_b, str(ctx.exception))
        self.assertEqual([e for e in self.ls.events if e["room"] == "chat"], [])

        # Pinned: sends.
        self.run_cli("alice", "key", "trust", "hostB", fp_b)
        out = self.run_cli("alice", "chat", "far", "hello")
        self.assertIn("chat opened with far", out)

        # hostB announces a different key: every further send refuses until re-pinned.
        state_b2, host_b2 = self.other_host("hostB", fresh=True)
        with state_b2, host_b2:
            fp_b2 = self.run_cli("far", "key", "show").strip()
            self.run_cli("far", "key", "announce")
        self.assertNotEqual(fp_b, fp_b2)
        with self.assertRaises(SystemExit) as ctx:
            self.run_cli("alice", "chat", "far", "still there?")
        self.assertIn("announced a different key", str(ctx.exception))
        self.assertEqual(len([e for e in self.ls.events if e["room"] == "chat"]), 1)
        self.assertIn("changed", self.run_cli("alice", "who"))
        self.run_cli("alice", "key", "trust", "hostB", fp_b2)
        self.assertIn("sent to far", self.run_cli("alice", "chat", "far", "still there?"))

    def test_who_shows_key_status_for_verified_project_members(self):
        self.online("alice")
        self.online_on("far", "hostB")
        self.assertEqual(a2a.host_key_status("hostB")["status"], "unannounced")
        out = self.run_cli("alice", "who")
        self.assertNotIn("far", out)
        self.assertIn("self", out)
        self.pin_remote_host("far", "hostB")
        out = self.run_cli("alice", "who")
        self.assertIn("far", out)
        self.assertIn("pinned", out)

    def test_no_flag_sends_plaintext_to_an_unpinned_host(self):
        self.online("alice")
        self.online_on("far", "hostB")
        for extra in (["--force"],):
            with self.assertRaises(SystemExit):
                self.run_cli("alice", "chat", "far", "hello", *extra)
        with mock.patch.dict(os.environ, {"A2A_ALLOW_PLAINTEXT": "1", "A2A_SKIP_PIN": "1"}):
            with self.assertRaises(SystemExit):
                self.run_cli("alice", "chat", "far", "hello")
        self.assertEqual([e for e in self.ls.events if e["room"] == "chat"], [])



@unittest.skipUnless(HAVE_NACL, "PyNaCl is not installed for this interpreter")
class SealedBodyTest(A2ACase):
    """Every chat body is a box; only the two hosts in the chat can open it."""

    def chat_events(self):
        return [e for e in self.ls.events if e["type"].startswith("chat.")]

    def test_local_chat_is_sealed_to_this_host_and_readable_here(self):
        self.online("alice", "bob")
        self.run_cli("alice", "chat", "bob", "the plan is under the floorboards")
        e = self.chat_events()[-1]
        self.assertNotIn("floorboards", e["body"])
        self.assertEqual(e["metadata"]["enc"]["alg"], "nacl-box")
        self.assertEqual(e["metadata"]["enc"]["to"], e["metadata"]["enc"]["from"])
        self.assertIn("floorboards", self.run_cli("bob", "inbox", "--peek"))
        with self.as_agent("bob"):
            cid = a2a.open_chat_with("alice")
        self.assertIn("floorboards", self.run_cli("bob", "read", cid))
        self.assertIn("floorboards", a2a.ledger("alice")["chats"][0]["rows"][0]["text"])

    def test_a_host_without_a_key_cannot_send_even_locally(self):
        self.online("alice", "bob")
        (Path(self.tmp.name) / "host" / "host-key").unlink()
        with self.assertRaises(SystemExit) as ctx:
            self.run_cli("alice", "chat", "bob", "hello")
        self.assertIn("a2a key init", str(ctx.exception))
        self.assertEqual(self.chat_events(), [])

    def two_hosts(self):
        """alice on hostA, far on hostB, each pinned to the other."""
        self.online("alice")
        self.online_on("far", "hostB")
        fp_b = self.pin_remote_host("far", "hostB")
        fp_a = self.run_cli("alice", "key", "show").strip()
        self.run_cli("alice", "key", "announce")
        state_b, host_b = self.other_host("hostB")
        self.state_b, self.host_b = state_b, host_b
        with state_b, host_b:
            self.run_cli("far", "key", "trust", "hostA", fp_a)
        self.run_cli("alice", "key", "trust", "hostB", fp_b)
        return fp_a, fp_b

    def test_cross_host_body_opens_on_both_hosts_and_nowhere_else(self):
        fp_a, fp_b = self.two_hosts()
        self.run_cli("alice", "chat", "far", "meet at the old mill")
        e = self.chat_events()[-1]
        self.assertNotIn("old mill", e["body"])
        self.assertEqual(e["metadata"]["enc"]["from"], fp_a)
        self.assertEqual(e["metadata"]["enc"]["to"], fp_b)
        # The sender host reads its own message back.
        with self.as_agent("alice"):
            cid = a2a.open_chat_with("far")
        self.assertIn("old mill", self.run_cli("alice", "read", cid))
        # The recipient host opens it.
        with self.state_b, self.host_b:
            self.assertIn("old mill", self.run_cli("far", "inbox", "--peek"))
            self.assertIn("old mill", self.run_cli("far", "read", cid))
            self.assertIn("old mill", a2a.ledger("far")["chats"][0]["rows"][0]["text"])
        # A third host holding the same replica opens nothing.
        state_c, host_c = self.other_host("hostC")
        with state_c, host_c:
            text, why = a2a.open_body(e)
            self.assertEqual(text, "")
            self.assertEqual(why, "sealed to another host")
            # A chat this host cannot verify does not exist for it: no rows,
            # no index entry, nothing for a console on that host to show.
            self.assertEqual(a2a.ledger("alice")["chats"], [])
            self.assertEqual(a2a.all_chats(), {})
        # The reply crosses back the other way.
        with self.state_b, self.host_b:
            self.run_cli("far", "reply", cid, "bring the lantern")
        self.assertIn("bring the lantern", self.run_cli("alice", "read", cid))

    def test_tampered_or_mismatched_boxes_are_unreadable_and_never_pasted(self):
        self.two_hosts()
        self.run_cli("alice", "chat", "far", "meet at the old mill")
        e = dict(self.chat_events()[-1])
        stored = next(x for x in self.ls.events if x["id"] == e["id"])
        raw = bytearray(__import__("base64").b64decode(stored["body"]))
        raw[-1] ^= 0xFF
        stored["body"] = __import__("base64").b64encode(bytes(raw)).decode()
        with self.state_b, self.host_b:
            text, why = a2a.open_body(stored)
            self.assertEqual(text, "")
            self.assertIn("does not open", why)
            delivered = []
            with mock.patch.object(a2a, "deliver", side_effect=lambda ev, t: delivered.append(t["handle"]) or "tmux"), mock.patch.object(a2a, "_age", return_value=1):
                state = a2a.courier_once({}, "hostB", verbose=False)
            self.assertEqual(delivered, [], "an unreadable body is never pasted")
            self.assertEqual(state.get("pending", {}), {}, "and never queued: an event this host cannot verify routes nowhere")
            self.assertEqual([a for a in self.ls.events if a["type"] == "event.ack" and a["from_agent"].startswith("courier@")], [], "and never acknowledged")
        # A box moved onto another event is refused by its contents.
        stored["body"] = e["body"]
        moved = dict(stored)
        moved["type"] = "chat.leave"
        with self.state_b, self.host_b:
            text, why = a2a.open_body(moved)
            self.assertIn("different chat, type or sender", why)

    def test_plaintext_is_readable_only_as_recorded_pre_sealing_history(self):
        """Nothing in a plaintext event decides whether it is history: not
        its timestamp, not its host, not a stripped envelope. Only the ids
        this host recorded at key init are readable."""
        self.two_hosts()
        now = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime(time.time() + 60))
        old = "2026-09-05T00:00:00Z"
        self.online("bob")
        recorded = self.append_chat("bob", "chat.invite", "chat-plain-0", body="unsealed text", metadata={"members": ["bob", "alice"], "host": "hostA"}, at=old)
        self.assertEqual(a2a.open_body(recorded), ("unsealed text", None))
        cases = {
            "backdated, local host": (old, {"host": "hostA"}),
            "backdated, remote host": (old, {"host": "hostB"}),
            "backdated, no host": (old, {}),
            "current, local host": (now, {"host": "hostA"}),
            "null envelope": (old, {"host": "hostA", "enc": None}),
            "malformed envelope": (old, {"host": "hostA", "enc": "not a dict"}),
        }
        for index, (name, (at, meta)) in enumerate(cases.items(), 1):
            with self.subTest(name=name):
                e = self.append_chat("far", "chat.invite", f"chat-plain-{index}", body="unsealed text", metadata={"members": ["far", "alice", "mallory"], **meta}, at=at, legacy=False)
                text, why = a2a.open_body(e)
                self.assertEqual(text, "")
                self.assertIn("refused", why)
                self.assertNotIn(e["correlation_id"], a2a.all_chats(), "an unrecorded plaintext invite opens no chat")
        # A backdated, envelope-stripped copy of a real sealed invite is
        # refused the same way, whatever it claims.
        self.run_cli("alice", "chat", "far", "sealed for real")
        sealed = next(x for x in self.ls.events if x["type"] in ("chat.invite", "chat.message") and isinstance(x["metadata"].get("enc"), dict))
        stripped = dict(sealed, id="evt_forged", created_at=old, body="forged opening", metadata={"members": ["alice", "far", "mallory"], "host": "hostA"})
        self.ls.events.append(stripped)
        self.assertIn("refused", a2a.open_body(stripped)[1])
        self.assertNotIn("mallory", a2a.all_chats()[sealed["correlation_id"]]["members"])

    def test_edited_routing_fields_make_the_event_unreadable_and_inert(self):
        """The recipient, the member list, the lifecycle markers and the
        event type are bound inside the box; an edit to the outer copy is
        refused before the fold or the courier act on it."""
        self.two_hosts()
        self.online("bob")
        self.run_cli("alice", "chat", "far", "for far only")
        invite = next(x for x in self.ls.events if x["type"] == "chat.invite")
        cid = invite["correlation_id"]
        with self.state_b, self.host_b:
            self.run_cli("far", "reply", cid, "noted")
        message = next(x for x in self.ls.events if x["type"] == "chat.message")

        # Recipient edited: unreadable, and it routes nowhere.
        edited = dict(message)
        edited["to_agent"] = "bob"
        self.assertIn("different recipient", a2a.open_body(edited)[1])
        # Member appended to the invite: unreadable, and the chat keeps its members.
        forged = next(x for x in self.ls.events if x["id"] == invite["id"])
        forged["metadata"] = dict(forged["metadata"], members=["alice", "far", "mallory"])
        self.assertIn("members does not match", a2a.open_body(forged)[1])
        folded = a2a.all_chats().get(cid, {})
        self.assertNotIn("mallory", folded.get("members", []), "an invite that fails verification adds nobody")
        self.assertIn(folded.get("state"), (None, "none"), "and without its invite the chat has no state")
        forged["metadata"] = dict(forged["metadata"], members=["alice", "far"])
        self.assertEqual(a2a.all_chats()[cid]["members"], ["alice", "far"])
        # A message rewritten as the opener's close: unreadable, and the chat stays open.
        stored = next(x for x in self.ls.events if x["id"] == message["id"])
        stored.update(type="chat.closed", from_agent="alice", metadata=dict(stored["metadata"], close="opener"))
        why = a2a.open_body(stored)[1]
        self.assertTrue("different chat, type or sender" in why or "is not an agent of that host" in why, why)
        # Without its message the chat is back to invited; what matters is
        # that a forged close never closes it, on either host.
        self.assertNotEqual(a2a.all_chats()[cid]["state"], "closed")
        self.assertIsNone(a2a.all_chats()[cid]["closed_by"])
        with self.state_b, self.host_b:
            self.assertNotEqual(a2a.all_chats()[cid]["state"], "closed")

    def test_the_same_box_under_another_relay_id_is_not_a_second_event(self):
        self.two_hosts()
        self.run_cli("alice", "chat", "far", "once only")
        invite = next(x for x in self.ls.events if x["type"] == "chat.invite")
        with self.state_b, self.host_b:
            delivered = []
            with mock.patch.object(a2a, "deliver", side_effect=lambda ev, t: delivered.append(ev["id"]) or "tmux"), mock.patch.object(a2a, "_age", return_value=1):
                state = a2a.courier_once({}, "hostB", verbose=False)
            self.assertEqual(delivered, [invite["id"]])
        # The relay presents the identical box again under a new id, as a
        # replay or a duplicated pull would.
        self.ls.n += 1
        replay = dict(invite, id=f"evt_{self.ls.n:06d}", seq=self.ls.n, created_at=time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()))
        self.ls.events.append(replay)
        with self.state_b, self.host_b:
            self.assertEqual([e["id"] for e in a2a.chat_events(invite["correlation_id"])], [invite["id"]], "one event per authenticated id")
            delivered = []
            with mock.patch.object(a2a, "deliver", side_effect=lambda ev, t: delivered.append(ev["id"]) or "tmux"), mock.patch.object(a2a, "_age", return_value=1):
                state = a2a.courier_once(state, "hostB", verbose=False)
                # And across a courier restart that remembers only where the
                # first pass had read to: the replay is all it sees.
                state2 = a2a.courier_once({"cursor": invite["id"]}, "hostB", verbose=False)
            self.assertEqual(delivered, [], "the replay is never pasted")
            self.assertEqual(state.get("pending", {}), {})
            self.assertEqual(state2.get("pending", {}), {})
            self.assertIn("once only", self.run_cli("far", "read", invite["correlation_id"]))
            self.assertEqual(self.run_cli("far", "read", invite["correlation_id"]).count("once only"), 1)

    def test_an_unpinned_announced_key_cannot_speak_as_a_participant(self):
        """An announced key is a claim; only a pinned key of the sender's
        host opens a box here. hostC, announced but never pinned, seals a
        close to hostA's key in alice's name: unreadable, and the chat
        stays open."""
        self.two_hosts()
        self.run_cli("alice", "chat", "far", "private")
        invite = next(x for x in self.ls.events if x["type"] == "chat.invite")
        cid = invite["correlation_id"]
        fp_a = self.run_cli("alice", "key", "show").strip()
        state_c, host_c = self.other_host("hostC")
        with state_c, host_c:
            self.run_cli("mallory", "key", "announce")
            nacl = a2a._nacl()
            mine = a2a.require_host_key()
            pub_a = a2a._public_key_for_fingerprint(fp_a, mine)
            box = nacl.public.Box(mine.box, nacl.public.PublicKey(a2a.box_public(pub_a)))
            import base64 as b64
            eid = "0" * 32
            fp_c = mine.fingerprint
            inner = {"v": 1, "eid": eid, "chat": cid, "type": "chat.closed", "from": "alice", "to": cid, "text": "closed", "close": "opener", "lifecycle": "hosted", "from_fp": fp_c, "to_fp": fp_a, "at": time.time(), "after": []}
            nonce = b"\x01" * nacl.public.Box.NONCE_SIZE
            ct = box.encrypt(json.dumps(inner, sort_keys=True).encode(), nonce).ciphertext
        with self.as_agent("alice"):
            forged = a2a.append("chat.closed", a2a.ROOM_CHAT, to=cid, correlation_id=cid, body=b64.b64encode(ct).decode(), status="superseded",
                                metadata={"host": "hostA", "lifecycle": "hosted", "close": "opener", "enc": {"v": 1, "alg": "nacl-box", "eid": eid, "to": fp_a, "from": fp_c, "nonce": b64.b64encode(nonce).decode()}})
        text, why = a2a.open_body(forged)
        self.assertEqual(text, "")
        self.assertIn("not one this host has pinned", why)
        self.assertNotEqual(a2a.all_chats()[cid]["state"], "closed")
        # Pinning hostC's key opens the box, but hostC is not one of this
        # chat's two hosts, so the close still counts for nothing.
        self.run_cli("alice", "key", "trust", "hostC", fp_c)
        self.assertIsNone(a2a.open_body(forged)[1])
        self.assertNotIn(forged["id"], [e["id"] for e in a2a.chat_events(cid)])
        self.assertNotEqual(a2a.all_chats()[cid]["state"], "closed")

    def test_a_chats_hosts_are_fixed_at_its_invite(self):
        self.two_hosts()
        self.online("bob")
        self.run_cli("alice", "chat", "bob", "just us")
        with self.as_agent("alice"):
            cid = a2a.open_chat_with("bob")
        with self.assertRaises(SystemExit) as ctx:
            self.run_cli("alice", "add", cid, "far")
        self.assertIn("hosts are fixed when it opens", str(ctx.exception))
        self.assertEqual([e["type"] for e in a2a.chat_events(cid)], ["chat.invite"])
        # A second local member joins a local chat, and a second member on
        # the chat's other host joins a two-host chat.
        self.online("carol")
        self.assertIn("added carol", self.run_cli("alice", "add", cid, "carol"))
        self.run_cli("alice", "chat", "far", "two hosts")
        with self.as_agent("alice"):
            pair = a2a.open_chat_with("far")
        self.online_on("near", "hostB")
        self.assertIn("added near", self.run_cli("alice", "add", pair, "near"))

    def test_a_chat_across_three_hosts_is_refused(self):
        self.two_hosts()
        self.online_on("who", "hostC")
        self.pin_remote_host("who", "hostC")
        with self.assertRaises(SystemExit) as ctx:
            self.run_cli("alice", "chat", "far,who", "everyone")
        self.assertIn("more than two hosts", str(ctx.exception))
        self.assertEqual(self.chat_events(), [])


@unittest.skipUnless(HAVE_NACL, "PyNaCl is not installed for this interpreter")
class CredentialShapeTest(A2ACase):
    """A body that looks like a secret is refused before it is sealed."""

    REFUSED = {
        "private key block": "here you go\n-----BEGIN OPENSSH PRIVATE KEY-----\nb3BlbnNzaC1rZXktdjEAAAAABG5vbmUAAAAEbm9uZQAAAAAAAAABAAAAMwAAAAtzc2gtZW\n",
        "age identity": "AGE-SECRET-KEY-1QYQSZQGPQYQSZQGPQYQSZQGPQYQSZQGPQYQSZQGPQYQSZQGPQYQSZQGPQYQS3Z7HLQ",
        "github token": "use ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789ab for the clone",
        "aws temporary key id": "creds: ASIAIOSFODNN7EXAMPLE and the session token follow",
        "inline env assignment": "run env DB_PASSWORD=x command please",
        "mid-sentence env assignment": "use GITHUB_TOKEN=abc for this",
        "two assignments on one line": "TOKEN=one SECRET=two",
        "google key ending in a hyphen": "key AIza" + "a" * 34 + "- please",
        "pgp private key armor": "-----BEGIN PGP PRIVATE KEY BLOCK-----\nlQOYBF...",
        "short bearer header": "Authorization: Bearer abcdefgh",
        "jwt with an empty payload": "here: eyJhbGciOiJIUzI1NiJ9.e30.SflKxwRJSMeKKF2QT4fwpMeJf36POk6yJV_adQssw5c",
        "anthropic key": "sk-ant-api03-abcdefghijklmnopqrstuvwxyz0123456789",
        "slack token": "xoxb-1234567890-abcdefghijklmnop",
        "aws key": "AKIAIOSFODNN7EXAMPLE is the id",
        "bearer header": "Authorization: Bearer eyJhbGciOiJIUzI1NiJ9abcdefgh",
        "jwt": "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.SflKxwRJSMeKKF2QT4fwpMeJf36POk6yJV_adQssw5c",
        "env assignment": "export NOISEDECK_AUTH_SESSION_SECRET=9f8e7d6c5b4a3f2e1d0c",
        "bare env assignment": "GITHUB_TOKEN=abc",
        "password env assignment": "DB_PASSWORD=x",
        "unprefixed token": "TOKEN=abc",
        "unprefixed exported api key": "export API_KEY=x",
        "unprefixed access key id": "ACCESS_KEY_ID=x",
        "unprefixed secret": "SECRET=x",
        "secret assignment": "the password = hunter2hunter2hunter2 for staging",
    }
    ALLOWED = [
        "the token budget for this review is tight",
        "rotate the password after the incident; do not paste it here",
        "our API key lives in the vault, ask ops",
        "the secret to good tests is deleting the line and watching it fail",
        "set TOKEN_COUNT=42 in the fixture",
        "MAX_TOKEN_COUNT=42",
        "export MY_SECRET_ENABLED=false",
        "PASSWORD_MIN_LENGTH=12 is what the form enforces",
        "export SECRETS_DIR=/run/secrets",
    ]

    def test_refused_shapes_never_reach_the_relay(self):
        self.online("alice", "bob")
        for name, text in self.REFUSED.items():
            with self.subTest(shape=name):
                with self.assertRaises(SystemExit) as ctx:
                    self.run_cli("alice", "chat", "bob", text)
                self.assertIn("looks like it carries", str(ctx.exception))
                self.assertEqual([e for e in self.ls.events if e["type"].startswith("chat.")], [])

    def test_prose_about_secrets_still_sends(self):
        self.online("alice", "bob")
        for text in self.ALLOWED:
            with self.subTest(text=text):
                self.assertIsNone(a2a.credential_shape(text))
        self.assertIn("chat opened with bob", self.run_cli("alice", "chat", "bob", self.ALLOWED[0]))
        self.assertIn("sent to bob", self.run_cli("alice", "chat", "bob", self.ALLOWED[1]))

    def test_lifecycle_reasons_are_checked_too(self):
        self.online("alice", "bob")
        self.run_cli("alice", "chat", "bob", "hello")
        with self.as_agent("alice"):
            cid = a2a.open_chat_with("bob")
        before = len([e for e in self.ls.events if e["type"].startswith("chat.")])
        with self.assertRaises(SystemExit):
            self.run_cli("bob", "leave", cid, "bye, the key is sk-ant-api03-abcdefghijklmnopqrstuvwxyz0123456789")
        self.assertEqual(len([e for e in self.ls.events if e["type"].startswith("chat.")]), before)


@unittest.skipUnless(HAVE_NACL, "PyNaCl is not installed for this interpreter")
class SealedAuthorityTest(A2ACase):
    """Who may speak in a chat comes from keys, not from presence or from
    any field of an event."""

    def two_hosts(self):
        return SealedBodyTest.two_hosts(self)

    def test_presence_cannot_move_a_participant_to_a_third_host(self):
        """hostC is pinned on both sides, so its key opens boxes; it still
        cannot speak as far, because the chat's hosts were fixed by the
        invite's keys and no presence event changes that."""
        self.two_hosts()
        self.run_cli("alice", "chat", "far", "between A and B")
        invite = next(x for x in self.ls.events if x["type"] == "chat.invite")
        cid = invite["correlation_id"]
        fp_a = self.run_cli("alice", "key", "show").strip()
        state_c, host_c = self.other_host("hostC")
        with state_c, host_c:
            fp_c = self.run_cli("mallory", "key", "show").strip()
            self.run_cli("mallory", "key", "announce")
            self.run_cli("mallory", "key", "trust", "hostA", fp_a)
        self.run_cli("alice", "key", "trust", "hostC", fp_c)
        # C seals a message to A in far's name.
        with state_c, host_c:
            nacl = a2a._nacl()
            mine = a2a.require_host_key()
            box = nacl.public.Box(mine.box, nacl.public.PublicKey(a2a.box_public(a2a._public_key_for_fingerprint(fp_a, mine))))
            import base64 as b64
            eid = "1" * 32
            inner = {"v": 1, "eid": eid, "chat": cid, "type": "chat.message", "from": "far", "to": "alice", "text": "meet me elsewhere", "lifecycle": "hosted", "from_fp": fp_c, "to_fp": fp_a, "at": time.time(), "after": []}
            nonce = b"\x02" * nacl.public.Box.NONCE_SIZE
            ct = box.encrypt(json.dumps(inner, sort_keys=True).encode(), nonce).ciphertext
        with self.as_agent("far"):
            forged = a2a.append("chat.message", a2a.ROOM_CHAT, to="alice", correlation_id=cid, body=b64.b64encode(ct).decode(), status="open",
                                metadata={"host": "hostC", "lifecycle": "hosted", "enc": {"v": 1, "alg": "nacl-box", "eid": eid, "to": fp_a, "from": fp_c, "nonce": b64.b64encode(nonce).decode()}})
        self.assertIsNone(a2a.open_body(forged)[1], "the box itself opens: C's key is pinned")
        self.assertNotIn(forged["id"], [e["id"] for e in a2a.chat_events(cid)], "but C is not one of this chat's hosts")
        # Presence now claims far lives on hostC. Nothing changes.
        with self.as_agent("far"):
            a2a.append("agent.online", a2a.ROOM_PRESENCE, to="*", status="ready", metadata={"handle": "far", "harness": "test", "host": "hostC", "pane": {"socket": "/s", "pane": "%3"}})
        a2a._HOSTS_CACHE.update(at=0.0, hosts={})
        self.assertNotIn(forged["id"], [e["id"] for e in a2a.chat_events(cid)])
        self.assertNotIn("meet me elsewhere", self.run_cli("alice", "inbox", "--peek"))
        self.assertEqual(a2a.all_chats()[cid]["routes"].get(forged["id"]), None)

    def pin_third_host(self, both: bool = True) -> str:
        """hostC, with mallory on it, pinned here; with `both`, hostC pins
        hostA too, so its boxes to hostA open."""
        fp_a = self.run_cli("alice", "key", "show").strip()
        state_c, host_c = self.other_host("hostC")
        with state_c, host_c:
            fp_c = self.run_cli("mallory", "key", "show").strip()
            self.run_cli("mallory", "key", "announce")
            if both:
                self.run_cli("mallory", "key", "trust", "hostA", fp_a)
        self.run_cli("alice", "key", "trust", "hostC", fp_c)
        self.online_on("mallory", "hostC")
        return fp_c

    def test_a_pinned_third_host_cannot_open_a_chat_as_another_hosts_agent(self):
        """A pin proves which host sealed an invite, not that the handle it
        speaks as lives there. hostC, pinned on both sides, opens a chat
        with alice in far's name: far's own host vouched for far in
        presence, the invite's key is hostC's, and the chat does not exist
        for alice. The real far's invite from hostB does."""
        self.two_hosts()
        self.pin_third_host()
        state_c, host_c = self.other_host("hostC")
        with state_c, host_c:
            self.run_cli("far", "chat", "alice", "meet me on C", "--force")
        forged = [e for e in self.ls.events if e["type"] == "chat.invite"][-1]
        self.assertEqual(forged["from_agent"], "far")
        self.assertIsNone(a2a.open_body(forged)[1], "the box itself opens: C's key is pinned")
        self.assertNotIn(forged["correlation_id"], a2a.all_chats(), "far does not live on the host that sealed it")
        self.assertNotIn("meet me on C", self.run_cli("alice", "inbox", "--peek"))
        with self.assertRaises(SystemExit):
            self.run_cli("alice", "reply", forged["correlation_id"], "on my way")
        # A handle no host has vouched for is refused the same way.
        with state_c, host_c:
            self.run_cli("ghost", "chat", "alice", "boo", "--force")
        ghost = [e for e in self.ls.events if e["type"] == "chat.invite"][-1]
        self.assertNotIn(ghost["correlation_id"], a2a.all_chats())
        # The real far, from hostB, opens a chat and alice's reply is
        # sealed to hostB and readable there, nowhere else.
        with self.state_b, self.host_b:
            self.run_cli("far", "chat", "alice", "from B, really")
        real = [e for e in self.ls.events if e["type"] == "chat.invite"][-1]
        cid = real["correlation_id"]
        self.assertIn(cid, a2a.all_chats())
        self.assertEqual(a2a.chat_binding(cid), ({"hostA", "hostB"}, {"far": "hostB"}))
        self.assertIn("sent to far", self.run_cli("alice", "reply", cid, "the private answer"))
        fp_b = self.run_cli("alice", "key", "status").split("hostB")[1].split("Announced:")[1].split()[0]
        self.assertEqual([e["metadata"]["enc"]["to"] for e in self.ls.events if e["type"] == "chat.message"][-1], fp_b)
        with self.state_b, self.host_b:
            self.assertIn("the private answer", self.run_cli("far", "read", cid))
        with state_c, host_c:
            self.assertNotIn(cid, a2a.all_chats())

    def test_add_places_the_newcomer_by_the_chats_fixed_hosts(self):
        """The hosts a chat's keys fixed are the only ones a newcomer may
        be on, whoever has left since and whatever presence claims."""
        self.two_hosts()
        self.run_cli("alice", "chat", "far", "between A and B")
        with self.as_agent("alice"):
            cid = a2a.open_chat_with("far")
        with self.state_b, self.host_b:
            self.run_cli("far", "leave", cid, "handing over")
        self.assertEqual(a2a.chat_binding(cid)[0], {"hostA", "hostB"})
        # Nobody on hostB is left in the chat; a newcomer there still fits.
        self.online_on("near", "hostB")
        self.assertIn("added near", self.run_cli("alice", "add", cid, "near"))
        with self.state_b, self.host_b:
            self.assertIn("between A and B", self.run_cli("near", "read", cid))
        # A pinned third host's agent does not, even after presence claims
        # far, and then alice's other members, live there.
        self.pin_third_host(both=False)
        self.online_on("far", "hostC")
        self.online_on("near", "hostC")
        before = [e["id"] for e in a2a.chat_events(cid)]
        with self.assertRaises(SystemExit) as ctx:
            self.run_cli("alice", "add", cid, "mallory")
        self.assertIn("hosts are fixed when it opens", str(ctx.exception))
        self.assertIn("hostC", str(ctx.exception))
        # Nor does a handle no host's key has vouched for.
        with self.assertRaises(SystemExit) as ctx:
            self.run_cli("alice", "add", cid, "nobody")
        self.assertIn("no host any key vouches for", str(ctx.exception))
        self.assertEqual([e["id"] for e in a2a.chat_events(cid)], before)
        self.assertNotIn("mallory", a2a.all_chats()[cid]["members"])

    def test_an_acknowledgement_nobody_vouches_for_moves_nothing(self):
        """An acknowledgement stops a courier and turns a ledger row to
        read, so an injected one, unsigned or signed by the wrong host,
        does neither; and acting in a chat acknowledges only what this
        host verified, never an injected plaintext."""
        self.online("alice", "bob")
        self.run_cli("alice", "chat", "bob", "hello")
        invite = next(e for e in self.ls.events if e["type"] == "chat.invite")
        cid = invite["correlation_id"]
        self.ls.call("mempalace_event_ack", {"event_id": invite["id"], "from_agent": "bob", "status": "applied", "body": ""})
        state_c, host_c = self.other_host("hostC")
        with state_c, host_c, self.as_agent("bob"):
            forged = a2a.ack_payload(invite["id"], "bob", "applied", scope=a2a.scope_of(invite))
        self.ls.call("mempalace_event_ack", {"event_id": invite["id"], "from_agent": "bob", "status": "applied", "body": forged})
        with self.as_agent("alice"):
            row = next(r for r in a2a.ledger("alice")["chats"][0]["rows"] if r["id"] == invite["id"])
        self.assertEqual(row["to"], {"bob": "sent"})
        delivered = []
        with mock.patch.object(a2a, "deliver", side_effect=lambda e, t: delivered.append(t["handle"]) or "tmux"), mock.patch.object(a2a, "_age", return_value=1):
            state = a2a.courier_once({}, "hostA", verbose=False)
        self.assertEqual(delivered, ["bob"], "the courier still delivers")
        # An injected plaintext message from bob is refused by every reader
        # and so is never acknowledged when alice acts in the chat.
        with self.as_agent("bob"):
            injected = a2a.append("chat.message", a2a.ROOM_CHAT, to="alice", correlation_id=cid, body="not sealed", status="open", metadata={"host": "hostA", "lifecycle": "hosted"})
        self.run_cli("alice", "reply", cid, "still here")
        acked = [a2a.metadata_of(a).get("ack_of") for a in self.ls.events if a["type"] == "event.ack" and a["from_agent"] == "alice"]
        self.assertNotIn(injected["id"], acked)
        # Bob's own reply acknowledges, and the row turns to read.
        self.run_cli("bob", "reply", cid, "got it")
        with self.as_agent("alice"):
            row = next(r for r in a2a.ledger("alice")["chats"][0]["rows"] if r["id"] == invite["id"])
        self.assertEqual(row["to"], {"bob": "read"})
        again = []
        with mock.patch.object(a2a, "deliver", side_effect=lambda e, t: again.append(e["id"]) or "tmux"), mock.patch.object(a2a, "_age", return_value=1):
            state = a2a.courier_once(state, "hostA", verbose=False)
        self.assertNotIn(invite["id"], again, "acknowledged for real: never pasted again")
        self.assertFalse(any(i["event"] == invite["id"] and i["to"] == "bob" for i in state["pending"].values()))

    def test_unverified_presence_supplies_no_delivery_coordinates(self):
        """The courier queues to the native session presence names, so only
        presence this host's key vouched for names one: a claim that bob sits in
        another pane, unsigned or signed by some other pinned host, changes
        nothing about where bob's message lands, and shows as unverified."""
        self.online("alice", "bob")
        self.run_cli("alice", "chat", "bob", "private to bob")
        with self.as_agent("bob"):
            a2a.append("agent.online", a2a.ROOM_PRESENCE, to="*", status="ready", metadata={"handle": "bob", "harness": "test", "host": "hostA", "pane": {"socket": "/s", "pane": "%999"}})
        self.pin_third_host(both=False)
        state_c, host_c = self.other_host("hostC")
        with state_c, host_c, self.as_agent("bob"):
            signed_elsewhere = a2a.signed_presence("agent.online", {"handle": "bob", "harness": "test", "host": "hostA", "pane": {"socket": "/s", "pane": "%998"}})
        with self.as_agent("bob"):
            a2a.append("agent.online", a2a.ROOM_PRESENCE, to="*", status="ready", metadata=signed_elsewhere)
            a2a.append("agent.online", a2a.ROOM_PRESENCE, to="*", status="ready", metadata=a2a.signed_presence("agent.online", {"handle": "bob", "harness": "codex", "project_id": self.project_id, "project_name": self.project_name, "native_session": "thread-bob", "host": "hostA", "pane": {"socket": "/s", "pane": "%1"}}))
        a2a._HOSTS_CACHE.update(at=0.0)
        targets = []
        with mock.patch.object(a2a, "deliver", side_effect=lambda e, t: targets.append(t["pane"]["pane"]) or "tmux"), mock.patch.object(a2a, "_age", return_value=1):
            a2a.courier_once({}, "hostA", verbose=False)
        self.assertEqual(targets, ["%1"])
        # A handle only claims name: listed as unverified, without a pane.
        with self.as_agent("ghost"):
            a2a.append("agent.online", a2a.ROOM_PRESENCE, to="*", status="ready", metadata={"handle": "ghost", "harness": "test", "host": "hostA", "pane": {"socket": "/s", "pane": "%7"}})
        with mock.patch.object(a2a, "_age", return_value=1):
            rows = a2a.roster()
        self.assertEqual(rows["ghost"]["state"], "unverified")
        self.assertNotIn("pane", rows["ghost"])
        self.assertEqual(rows["bob"]["pane"]["pane"], "%1")
        # An unsigned courier scan places nobody either, and a claim written
        # after the genuine presence, hook or scan, hides neither.
        self.ls.call("mempalace_event_append", {"type": "courier.scan", "stream": a2a.STREAM, "room": a2a.ROOM_PRESENCE, "from_agent": "courier@hostA", "to_agent": "*", "metadata": {"host": "hostA", "agents": [{"handle": "bob", "harness": "test", "host": "hostA", "pane": {"socket": "/s", "pane": "%997"}}]}})
        with self.as_agent("bob"):
            a2a.append("agent.online", a2a.ROOM_PRESENCE, to="*", status="ready", metadata={"handle": "bob", "harness": "test", "host": "hostA", "pane": {"socket": "/s", "pane": "%996"}})
            a2a.append("agent.offline", a2a.ROOM_PRESENCE, to="*", status="superseded", metadata={"handle": "bob", "harness": "test", "host": "hostA"})
        with mock.patch.object(a2a, "_age", return_value=1):
            rows = a2a.roster()
        self.assertEqual(rows["bob"]["pane"]["pane"], "%1")
        self.assertEqual(rows["bob"]["state"], "online")
        a2a._HOSTS_CACHE.update(at=0.0)
        self.assertNotIn("ghost", a2a.authenticated_hosts())
        # Genuine signed fields copied under another writer's name are a
        # claim: carol's presence written as bob does not move bob's
        # message into carol's pane.
        self.online("carol")
        carol = [e for e in self.ls.events if e["room"] == a2a.ROOM_PRESENCE and a2a.metadata_of(e).get("handle") == "carol"][-1]
        self.run_cli("alice", "chat", "bob", "second, private to bob")
        with self.as_agent("bob"):
            a2a.append("agent.online", a2a.ROOM_PRESENCE, to="*", status="ready", metadata=dict(a2a.metadata_of(carol)))
        with mock.patch.object(a2a, "_age", return_value=1):
            rows = a2a.roster()
        self.assertEqual(rows["bob"]["pane"]["pane"], "%1")
        self.assertEqual(rows["carol"]["pane"]["pane"], "%1")
        targets = []
        with mock.patch.object(a2a, "_codex_queue", side_effect=lambda thread, line: targets.append((thread, line)) or "codex-queue"), mock.patch.object(a2a, "tmux_sockets", return_value=["/s"]), mock.patch.object(a2a, "_age", return_value=1):
            a2a.courier_once({}, "hostA", verbose=False)
        self.assertEqual({p for p, _ in targets}, {"thread-bob"})
        self.assertTrue(any("second, private to bob" in line for _, line in targets))
        # A scan copied under another courier's name is a claim too.
        scan = [e for e in self.ls.events if e["type"] == "courier.scan"]
        self.ls.call("mempalace_event_append", {"type": "courier.scan", "stream": a2a.STREAM, "room": a2a.ROOM_PRESENCE, "from_agent": "courier@hostZ", "to_agent": "*", "metadata": a2a.signed_presence("courier.scan", {"host": "hostA", "agents": [{"handle": "bob", "harness": "test", "host": "hostA", "pane": {"socket": "/s", "pane": "%995"}}]})})
        with mock.patch.object(a2a, "_age", return_value=1):
            self.assertEqual(a2a.roster()["bob"]["pane"]["pane"], "%1")
        del scan

    def test_a_foreign_courier_cannot_acknowledge_a_delivery_on_this_host(self):
        """A courier's signature says which host it is, not that it served
        the recipient: hostC's courier, pinned here, naming bob on hostA as
        delivered moves no ledger row and empties no inbox."""
        self.online("alice", "bob")
        self.run_cli("alice", "chat", "bob", "only here")
        invite = next(e for e in self.ls.events if e["type"] == "chat.invite")
        self.pin_third_host(both=False)
        state_c, host_c = self.other_host("hostC")
        with state_c, host_c:
            forged = a2a.ack_payload(invite["id"], "courier@hostC", "ready", "bob", "delivered to bob via tmux", scope=a2a.scope_of(invite))
        self.ls.call("mempalace_event_ack", {"event_id": invite["id"], "from_agent": "courier@hostC", "status": "ready", "body": forged})
        with self.as_agent("alice"):
            row = next(r for r in a2a.ledger("alice")["chats"][0]["rows"] if r["id"] == invite["id"])
        self.assertEqual(row["to"], {"bob": "sent"})
        with self.as_agent("bob"):
            self.assertEqual([e["id"] for e in a2a.unread()], [invite["id"]])
        # This host's own courier, naming its own agent, counts.
        self.ls.call("mempalace_event_ack", {"event_id": invite["id"], "from_agent": "courier@hostA", "status": "ready", "body": a2a.ack_payload(invite["id"], "courier@hostA", "ready", "bob", "delivered to bob via tmux", scope=a2a.scope_of(invite))})
        with self.as_agent("alice"):
            row = next(r for r in a2a.ledger("alice")["chats"][0]["rows"] if r["id"] == invite["id"])
        self.assertEqual(row["to"], {"bob": "delivered"})
        with self.as_agent("bob"):
            self.assertEqual(a2a.unread(), [])

    def hide(self, keep):
        """Take events off the relay for a while, as if they had not
        replicated yet; returns them for restore. The derived presence
        state is dropped with them: it must hold nothing the relay does
        not, exactly as a process that folded only what had arrived."""
        hidden = [e for e in self.ls.events if not keep(e)]
        self.ls.events[:] = [e for e in self.ls.events if keep(e)]
        a2a._PUBLIC_KEYS_CACHE.update(at=0.0, keys={})
        a2a._HOSTS_CACHE.update(at=0.0)
        a2a._state_path("presence-state.json").unlink(missing_ok=True)
        a2a._PRESENCE_STATE_MEMO.update(derived=None, at=0.0)
        return hidden

    def restore(self, hidden):
        self.ls.events.extend(hidden)
        self.ls.events.sort(key=lambda e: e["seq"])
        a2a._PUBLIC_KEYS_CACHE.update(at=0.0, keys={})
        a2a._HOSTS_CACHE.update(at=0.0)
        a2a._state_path("presence-state.json").unlink(missing_ok=True)
        a2a._PRESENCE_STATE_MEMO.update(derived=None, at=0.0)

    def test_an_event_whose_key_or_presence_arrives_later_is_still_delivered_and_read(self):
        """Replication has no order across rooms: an invite can land before
        the announcement of the key that sealed it or the presence that
        places its sender. The courier holds it and the inbox cursor stays
        behind it until they arrive; then it is delivered and read once."""
        self.two_hosts()
        with self.state_b, self.host_b:
            self.run_cli("far", "chat", "alice", "early bird")
        invite = [e for e in self.ls.events if e["type"] == "chat.invite"][-1]

        for what, keep in (
            ("key", lambda e: not (e["type"] == "key.announce" and a2a.metadata_of(e).get("host") == "hostB")),
            ("presence", lambda e: not (e["room"] == a2a.ROOM_PRESENCE and a2a.metadata_of(e).get("handle") == "far")),
        ):
            with self.subTest(missing=what):
                hidden = self.hide(keep)
                self.assertNotIn(invite["correlation_id"], a2a.all_chats(), f"without the {what} the invite is unverifiable")
                delivered = []
                with mock.patch.object(a2a, "deliver", side_effect=lambda e, t: delivered.append(e["id"]) or "tmux"), mock.patch.object(a2a, "_age", return_value=1):
                    state = a2a.courier_once({}, "hostA", verbose=False)
                self.assertEqual(delivered, [])
                self.assertIn(invite["id"], state["held"])
                with self.as_agent("alice"):
                    a2a.advance_cursor()
                    self.assertEqual(a2a.unread(), [])
                self.restore(hidden)
                with self.as_agent("alice"):
                    self.assertEqual([e["id"] for e in a2a.unread()], [invite["id"]], "behind the cursor, so read once it verifies")
                    self.assertIn("early bird", self.run_cli("alice", "inbox", "--peek"))
                with mock.patch.object(a2a, "deliver", side_effect=lambda e, t: delivered.append(e["id"]) or "tmux"), mock.patch.object(a2a, "_age", return_value=1):
                    state = a2a.courier_once(state, "hostA", verbose=False)
                self.assertEqual(delivered, [invite["id"]])
                self.assertNotIn(invite["id"], state["held"])
                with self.as_agent("alice"):
                    self.assertEqual(a2a.unread(), [], "delivered, so seen")
                # Reset for the next case: the courier's ack is a delivery.
                self.ls.events[:] = [e for e in self.ls.events if e["type"] != "event.ack"]
                a2a.write_state(a2a.inbox_cursor_name(), {})
        # Junk that never verifies does not pin the cursor past the hold.
        with self.as_agent("mallory"):
            junk = a2a.append("chat.message", a2a.ROOM_CHAT, to="alice", correlation_id=invite["correlation_id"], body="x", status="open", metadata={"host": "hostZ"})
        with self.as_agent("alice"):
            a2a.acknowledge_chat(invite["correlation_id"])
            a2a.advance_cursor()
            self.assertNotEqual(a2a.read_state(a2a.inbox_cursor_name()).get("cursor"), junk["id"], "the cursor waits behind fresh junk")
        with self.state_b, self.host_b:
            self.run_cli("far", "reply", invite["correlation_id"], "after the junk")
        self.run_cli("alice", "inbox")
        with self.as_agent("alice"):
            a2a.advance_cursor()
            self.assertLess(a2a.read_state(a2a.inbox_cursor_name()).get("cursor"), junk["id"], "still behind the junk while it is fresh")
        with mock.patch.object(a2a, "_age", return_value=a2a.HOLD_S + 1), self.as_agent("alice"):
            a2a.advance_cursor()
            self.assertGreater(a2a.read_state(a2a.inbox_cursor_name()).get("cursor"), junk["id"], "past the junk once the hold ran out")


    def test_a_released_x25519_key_is_migrated_to_a_sealing_key_and_re_pinned(self):
        """Fixture from the keys release: a bare X25519 host-key, an announce
        of it without `alg`, and a peer's pin of that fingerprint. Nothing
        reinterprets those bytes: the key refuses to seal until migrate
        replaces it, the old announce counts as none, and the peer re-pins
        the new fingerprint by hand."""
        self.online("alice")
        self.online_on("far", "hostB")
        old_a = os.urandom(32)
        old_fp_a = a2a.fingerprint_of(old_a)
        path = a2a.host_key_path()
        path.write_text(base64.b64encode(old_a).decode() + "\n")
        a2a._HOST_KEY_CACHE.update(path=None)
        with self.as_agent("alice"):
            a2a.append("key.announce", a2a.ROOM_KEYS, to="*", status="ready", metadata={"host": "hostA", "fingerprint": old_fp_a, "public_key": base64.b64encode(old_a).decode()})
        # hostB is in the same state: a released key, announced, pinned here.
        state_b, host_b = self.other_host("hostB")
        with state_b, host_b:
            old_b = os.urandom(32)
            a2a.host_key_path().write_text(base64.b64encode(old_b).decode() + "\n")
            a2a._HOST_KEY_CACHE.update(path=None)
            with self.as_agent("far"):
                a2a.append("key.announce", a2a.ROOM_KEYS, to="*", status="ready", metadata={"host": "hostB", "fingerprint": a2a.fingerprint_of(old_b), "public_key": base64.b64encode(old_b).decode()})
            a2a.pin_host("hostA", old_fp_a)
        a2a.pin_host("hostB", a2a.fingerprint_of(old_b))
        a2a._PUBLIC_KEYS_CACHE.update(at=0.0, keys={})
        with self.assertRaises(SystemExit) as ctx:
            self.run_cli("alice", "key", "show")
        self.assertIn("a2a key migrate", str(ctx.exception))
        self.assertEqual(self.chat_room(), [])
        with self.assertRaises(SystemExit):
            self.run_cli("alice", "chat", "far", "hello", "--force")
        self.assertIn("pre-sealing key", self.run_cli("alice", "key", "status"))
        out = self.run_cli("alice", "key", "migrate")
        self.assertIn("replaced the pre-sealing key", out)
        new_fp_a = self.run_cli("alice", "key", "show").strip()
        self.assertNotEqual(new_fp_a, old_fp_a)
        self.assertIn(new_fp_a, out)
        self.assertTrue(a2a._key_path(a2a.RETIRED_KEY_NAME).exists())
        self.assertEqual(a2a.announced_keys()["hostA"]["fingerprint"], new_fp_a, "migrate announced the new key")
        self.assertIn("recorded", self.run_cli("alice", "key", "migrate"))
        self.assertEqual(self.run_cli("alice", "key", "show").strip(), new_fp_a, "migrate is idempotent")
        # hostB's old announce counts as none: the send names migrate.
        with self.assertRaises(SystemExit) as ctx:
            self.run_cli("alice", "chat", "far", "hello", "--force")
        self.assertIn("a2a key migrate", str(ctx.exception))
        with state_b, host_b:
            self.run_cli("far", "key", "migrate")
            new_fp_b = self.run_cli("far", "key", "show").strip()
            self.assertIn("changed", self.run_cli("far", "key", "status"))
            self.run_cli("far", "key", "trust", "hostA", new_fp_a)
        self.online_on("far", "hostB")
        with self.assertRaises(SystemExit) as ctx:
            self.run_cli("alice", "chat", "far", "hello", "--force")
        self.assertIn("announced a different key", str(ctx.exception))
        self.run_cli("alice", "key", "trust", "hostB", new_fp_b)
        self.online("alice")
        self.assertIn("chat opened with far", self.run_cli("alice", "chat", "far", "sealed at last", "--force"))
        with state_b, host_b:
            self.assertIn("sealed at last", self.run_cli("far", "inbox", "--peek"))

    def chat_room(self):
        return [e for e in self.ls.events if e["room"] == a2a.ROOM_CHAT]

    def test_a_replayed_older_presence_never_brings_delivery_back_to_an_old_pane(self):
        """A signature covers the moment it was made. bob's genuine presence
        for one pane, replayed under a new relay event after bob moved to
        another, is older than the current one and places nothing; the
        same for a courier scan."""
        self.online("alice")
        old = self.presence("bob", pane="%11")
        with mock.patch.object(a2a.time, "time", return_value=time.time() + 5):
            self.presence("bob", pane="%22")
        with mock.patch.object(a2a, "_age", return_value=1):
            self.assertEqual(a2a.roster()["bob"]["pane"]["pane"], "%22")
        self.run_cli("alice", "chat", "bob", "to the new pane")
        with self.as_agent("bob"):
            a2a.append("agent.online", a2a.ROOM_PRESENCE, to="*", status="ready", metadata=dict(a2a.metadata_of(old)))
        with mock.patch.object(a2a, "_age", return_value=1):
            self.assertEqual(a2a.roster()["bob"]["pane"]["pane"], "%22")
        targets = []
        with mock.patch.object(a2a, "deliver", side_effect=lambda e, t: targets.append(t["pane"]["pane"]) or "tmux"), mock.patch.object(a2a, "_age", return_value=1):
            a2a.courier_once({}, "hostA", verbose=False)
        self.assertEqual(targets, ["%22"])
        # A scan replayed after a newer scan is older too.
        scan_old = self.ls.call("mempalace_event_append", {"type": "courier.scan", "stream": a2a.STREAM, "room": a2a.ROOM_PRESENCE, "from_agent": "courier@hostA", "to_agent": "*", "metadata": a2a.signed_presence("courier.scan", {"host": "hostA", "agents": [{"handle": "carol", "harness": "test", "host": "hostA", "pane": {"socket": "/s", "pane": "%31"}}]})})["event"]
        with mock.patch.object(a2a.time, "time", return_value=time.time() + 5):
            self.ls.call("mempalace_event_append", {"type": "courier.scan", "stream": a2a.STREAM, "room": a2a.ROOM_PRESENCE, "from_agent": "courier@hostA", "to_agent": "*", "metadata": a2a.signed_presence("courier.scan", {"host": "hostA", "agents": [{"handle": "carol", "harness": "test", "host": "hostA", "pane": {"socket": "/s", "pane": "%32"}}]})})
        self.ls.call("mempalace_event_append", {"type": "courier.scan", "stream": a2a.STREAM, "room": a2a.ROOM_PRESENCE, "from_agent": "courier@hostA", "to_agent": "*", "metadata": dict(a2a.metadata_of(scan_old))})
        a2a._HOSTS_CACHE.update(at=0.0)
        with mock.patch.object(a2a, "_age", return_value=1):
            self.assertEqual(a2a.roster()["carol"]["pane"]["pane"], "%32")
        # An old signed presence with nothing newer is as old as it says,
        # however fresh the relay event that carries it.
        with mock.patch.object(a2a.time, "time", return_value=time.time() + a2a.PRESENCE_TTL_S + 10), mock.patch.object(a2a, "_age", return_value=1):
            self.assertNotIn("bob", {h for h, r in a2a.roster().items() if r["state"] != "unverified"})

    def test_wait_returns_an_invitation_once_its_key_arrives(self):
        """`a2a wait` judges what is unread afresh on every wake, from the
        durable cursor, so an invitation that could not be verified when
        it arrived is returned once the key that sealed it replicates."""
        self.two_hosts()
        with self.state_b, self.host_b:
            self.run_cli("far", "chat", "alice", "wait for me")
        invite = [e for e in self.ls.events if e["type"] == "chat.invite"][-1]
        hidden = self.hide(lambda e: not (e["type"] == "key.announce" and a2a.metadata_of(e).get("host") == "hostB"))
        wakes = []

        def waking(timeout_ms, **filters):
            wakes.append(filters.get("since_event_id"))
            if len(wakes) == 1:
                return [invite]
            if len(wakes) == 2:
                self.restore(hidden)
                return hidden
            return []

        with mock.patch.object(a2a, "wait_events", side_effect=waking), self.as_agent("alice"):
            events = a2a._wait(None, 5)
        self.assertEqual([e["id"] for e in events], [invite["id"]])
        self.assertEqual(len(wakes), 2)
        with self.as_agent("alice"):
            self.assertEqual(a2a.unread(), [], "returned by wait, so read")

    def test_a_pinned_hosts_scan_cannot_nominate_a_pane_for_another_hosts_agent(self):
        """bob is hostA's agent. A scan signed by pinned hostC naming bob
        there with a pane lends hostA's courier nothing, even when bob's
        own hook presence carries no pane."""
        self.online("alice")
        with self.as_agent("bob"):
            a2a.append("agent.online", a2a.ROOM_PRESENCE, to="*", status="ready", metadata=a2a.signed_presence("agent.online", {"handle": "bob", "harness": "codex", "project_id": self.project_id, "project_name": self.project_name, "native_session": "thread-bob", "host": "hostA", "pane": {}}))
        self.pin_third_host(both=False)
        state_c, host_c = self.other_host("hostC")
        with state_c, host_c:
            hostile = a2a.signed_presence("courier.scan", {"host": "hostC", "agents": [{"handle": "bob", "harness": "test", "host": "hostC", "pane": {"socket": "/s", "pane": "%999"}}]})
        self.ls.call("mempalace_event_append", {"type": "courier.scan", "stream": a2a.STREAM, "room": a2a.ROOM_PRESENCE, "from_agent": "courier@hostC", "to_agent": "*", "metadata": hostile})
        a2a._HOSTS_CACHE.update(at=0.0)
        with mock.patch.object(a2a, "_age", return_value=1):
            row = a2a.roster()["bob"]
        self.assertEqual(row["host"], "hostA")
        self.assertFalse(row.get("pane"))
        self.run_cli("alice", "chat", "bob", "not for hostC's pane", "--force")
        pastes = []
        with mock.patch.object(a2a, "_codex_queue", side_effect=lambda thread, line: pastes.append(thread) or "codex-queue"), mock.patch.object(a2a, "_age", return_value=1):
            state = a2a.courier_once({}, "hostA", verbose=False)
        self.assertEqual(pastes, [], "nothing is written to a pane another host nominated")
        self.assertEqual(len(state["pending"]), 1, "kept for bob's own presence to name a pane")
        # hostA's own scan does lend the pane.
        self.ls.call("mempalace_event_append", {"type": "courier.scan", "stream": a2a.STREAM, "room": a2a.ROOM_PRESENCE, "from_agent": "courier@hostA", "to_agent": "*", "metadata": a2a.signed_presence("courier.scan", {"host": "hostA", "agents": [{"handle": "bob", "harness": "test", "host": "hostA", "pane": {"socket": "/s", "pane": "%5"}}]})})
        with mock.patch.object(a2a, "_codex_queue", side_effect=lambda thread, line: pastes.append(thread) or "codex-queue"), mock.patch.object(a2a, "_age", return_value=1):
            for item in state["pending"].values():
                item["next"] = 0
            a2a.courier_once(state, "hostA", verbose=False)
        self.assertEqual(pastes, ["thread-bob"])

    def test_bodies_seal_to_the_chats_host_pair_whoever_is_active(self):
        """alice and bob on hostA, far on hostB. far leaves; alice's next
        message still seals to hostB, so near, joining there later, reads
        the whole shared history, and alice's close reads there too."""
        self.two_hosts()
        self.online("bob")
        fp_b = self.run_cli("alice", "key", "status").split("hostB")[1].split("Announced:")[1].split()[0]
        self.run_cli("alice", "chat", "bob,far", "three of us")
        with self.as_agent("alice"):
            cid = a2a.open_chat_with("bob", "far")
        self.run_cli("bob", "reply", cid, "bob here")
        with self.state_b, self.host_b:
            self.run_cli("far", "leave", cid, "off for now")
        self.run_cli("alice", "reply", cid, "while far is away")
        sealed_to = [e["metadata"]["enc"]["to"] for e in self.ls.events if e["type"] == "chat.message"]
        self.assertEqual(sealed_to[-1], fp_b, "sealed to the chat's other host with nobody active there")
        self.online_on("near", "hostB")
        self.assertIn("added near", self.run_cli("alice", "add", cid, "near"))
        with self.state_b, self.host_b:
            history = self.run_cli("near", "read", cid)
            self.assertIn("while far is away", history)
            self.assertIn("bob here", history)
        self.run_cli("alice", "end", cid, "done")
        with self.state_b, self.host_b:
            self.assertEqual(a2a.all_chats()[cid]["state"], "closed")
            status = self.run_cli("near", "status", "--all")
            self.assertIn(cid, status)
            self.assertIn("closed", status)

    def test_same_moment_presence_keeps_relay_order_but_a_copy_is_one_event(self):
        """Two announcements in the same moment are distinct events and the
        later one stands; a copy of the earlier one under a new relay id is
        the same event again and changes nothing, for hook presence, for
        scans, and for online over offline."""
        self.online("alice")
        frozen = time.time()
        with mock.patch.object(a2a.time, "time", return_value=frozen):
            first = self.presence("bob", pane="%11")
            self.presence("bob", pane="%22")
        with self.as_agent("bob"):
            a2a.append("agent.online", a2a.ROOM_PRESENCE, to="*", status="ready", metadata=dict(a2a.metadata_of(first)))
        with mock.patch.object(a2a, "_age", return_value=1):
            self.assertEqual(a2a.roster()["bob"]["pane"]["pane"], "%22")
        self.run_cli("alice", "chat", "bob", "to %22")
        targets = []
        with mock.patch.object(a2a, "deliver", side_effect=lambda e, t: targets.append(t["pane"]["pane"]) or "tmux"), mock.patch.object(a2a, "_age", return_value=1):
            a2a.courier_once({}, "hostA", verbose=False)
        self.assertEqual(targets, ["%22"])
        with mock.patch.object(a2a.time, "time", return_value=frozen):
            online = self.presence("carol", pane="%41")
            self.presence("carol", kind="agent.offline")
        with self.as_agent("carol"):
            a2a.append("agent.online", a2a.ROOM_PRESENCE, to="*", status="ready", metadata=dict(a2a.metadata_of(online)))
        with mock.patch.object(a2a, "_age", return_value=1):
            self.assertNotIn("carol", a2a.roster())
        with mock.patch.object(a2a.time, "time", return_value=frozen):
            scan_a = self.ls.call("mempalace_event_append", {"type": "courier.scan", "stream": a2a.STREAM, "room": a2a.ROOM_PRESENCE, "from_agent": "courier@hostA", "to_agent": "*", "metadata": a2a.signed_presence("courier.scan", {"host": "hostA", "agents": [{"handle": "dave", "harness": "test", "host": "hostA", "pane": {"socket": "/s", "pane": "%51"}}]})})["event"]
            self.ls.call("mempalace_event_append", {"type": "courier.scan", "stream": a2a.STREAM, "room": a2a.ROOM_PRESENCE, "from_agent": "courier@hostA", "to_agent": "*", "metadata": a2a.signed_presence("courier.scan", {"host": "hostA", "agents": [{"handle": "dave", "harness": "test", "host": "hostA", "pane": {"socket": "/s", "pane": "%52"}}]})})
        self.ls.call("mempalace_event_append", {"type": "courier.scan", "stream": a2a.STREAM, "room": a2a.ROOM_PRESENCE, "from_agent": "courier@hostA", "to_agent": "*", "metadata": dict(a2a.metadata_of(scan_a))})
        a2a._HOSTS_CACHE.update(at=0.0)
        with mock.patch.object(a2a, "_age", return_value=1):
            self.assertEqual(a2a.roster()["dave"]["pane"]["pane"], "%52")

    def test_wait_sees_a_dependency_that_arrives_while_its_caches_are_warm(self):
        """The invite is scanned before far's presence; the presence arrives
        on the next wake, within the cache's lifetime, and nothing resets
        any cache: wait still returns the invite."""
        self.two_hosts()
        with self.state_b, self.host_b:
            self.run_cli("far", "chat", "alice", "cached out")
        invite = [e for e in self.ls.events if e["type"] == "chat.invite"][-1]
        hidden = self.hide(lambda e: not (e["room"] == a2a.ROOM_PRESENCE and a2a.metadata_of(e).get("handle") == "far"))
        wakes = []

        def waking(timeout_ms, **filters):
            wakes.append(filters.get("since_event_id"))
            if len(wakes) == 1:
                return [invite]
            if len(wakes) == 2:
                self.ls.events.extend(hidden)
                self.ls.events.sort(key=lambda e: e["seq"])
                return hidden
            return []

        with mock.patch.object(a2a, "wait_events", side_effect=waking), self.as_agent("alice"):
            events = a2a._wait(None, 5)
        self.assertEqual([e["id"] for e in events], [invite["id"]])

    def slow_presence(self, seconds: float):
        """A relay on which listing presence takes `seconds` of a clock
        that otherwise stands still; returns the passes it has served."""
        clock = [100.0]
        passes = []
        relay = a2a._rpc

        def slow(tool, args, timeout=30.0):
            if tool == "mempalace_event_list" and args.get("room") == a2a.ROOM_PRESENCE:
                passes.append(args.get("since_event_id"))
                clock[0] += seconds
            return relay(tool, args)

        for patch in (mock.patch.object(a2a, "_rpc", side_effect=slow), mock.patch.object(a2a.time, "perf_counter", side_effect=lambda: clock[0])):
            patch.start()
            self.addCleanup(patch.stop)
        return passes, clock

    def test_a_presence_pass_that_outlasts_the_cache_is_shared_by_the_readers_after_it(self):
        """A pass over presence takes three seconds and the cache is good
        for two: the readers that follow it share that pass rather than
        each repeating it. The cache still ages, and a reset or a reader
        that accepts nothing cached still reads presence again."""
        self.online("alice")
        passes, clock = self.slow_presence(3.0)
        self.assertEqual(a2a.authenticated_hosts(), {"alice": "hostA"})
        self.assertEqual(a2a.authenticated_hosts(), {"alice": "hostA"})
        self.assertEqual(a2a.last_known_hosts(), {"alice": "hostA"})
        self.assertEqual(len(passes), 1)
        clock[0] += 2.0
        a2a.authenticated_hosts()
        self.assertEqual(len(passes), 2)
        a2a.authenticated_hosts(max_age_s=0)
        self.assertEqual(len(passes), 3)
        a2a._HOSTS_CACHE.update(at=0.0)
        a2a.authenticated_hosts()
        self.assertEqual(len(passes), 4)

    def test_an_inbox_with_unread_mail_reads_presence_once_however_slow_the_pass(self):
        """Every acknowledgement in the room asks where its writer lives.
        On a relay whose presence pass outlasts the cache, an inbox that
        holds unread mail still reads presence once, not once per asker."""
        self.online("alice", "bob")
        self.run_cli("alice", "chat", "bob", "first")
        with self.as_agent("alice"):
            cid = a2a.open_chat_with("bob")
        self.run_cli("bob", "reply", cid, "read and answered")
        self.run_cli("alice", "reply", cid, "still unread")
        a2a._HOSTS_CACHE.update(at=0.0)
        passes, _ = self.slow_presence(3.0)
        with self.as_agent("bob"):
            pending = a2a.unread()
        self.assertEqual([e["type"] for e in pending], ["chat.message"])
        self.assertEqual(len(passes), 1)

    def test_unreadable_events_are_reported_but_change_nothing(self):
        """A damaged body after a valid local invite: inbox, read and the
        ledger each report it with its bounded reason and the identifiers
        it claims, marked unverified; the chat's state, delivery and
        acknowledgements are untouched."""
        self.online("alice", "bob")
        self.run_cli("alice", "chat", "bob", "fine")
        with self.as_agent("alice"):
            cid = a2a.open_chat_with("bob")
        self.run_cli("alice", "reply", cid, "about to be damaged")
        damaged = next(e for e in self.ls.events if e["type"] == "chat.message")
        damaged["body"] = "AAAA" + damaged["body"][4:]
        with self.as_agent("bob"):
            _, why = a2a.open_body(damaged)
            self.assertIsNotNone(why)
        inbox = self.run_cli("bob", "inbox", "--peek")
        self.assertIn("Unreadable: message claiming to be from alice (unverified)", inbox)
        self.assertIn("fine", inbox)
        self.assertNotIn("about to be damaged", inbox)
        self.assertIn("Unreadable: message claiming to be from alice", self.run_cli("bob", "read", cid))
        with self.as_agent("bob"):
            book = a2a.ledger("bob")["chats"][0]
        self.assertEqual([u["id"] for u in book["unreadable"]], [damaged["id"]])
        self.assertTrue(book["unreadable"][0]["untrusted"])
        self.assertLessEqual(len(book["unreadable"][0]["reason"]), a2a.REASON_CHARS)
        self.assertEqual([r["id"] for r in book["rows"]], [e["id"] for e in self.ls.events if e["type"] == "chat.invite"])
        self.assertEqual(a2a.all_chats()[cid]["messages"], 0)
        self.assertEqual(a2a.all_chats()[cid]["routes"].get(damaged["id"]), None)
        self.run_cli("bob", "reply", cid, "seen")
        acked = [a2a.metadata_of(a).get("ack_of") for a in self.ls.events if a["type"] == "event.ack"]
        self.assertNotIn(damaged["id"], acked)
        with self.as_agent("bob"):
            self.assertEqual(a2a.unread(), [])
        payload = json.loads(self.run_cli("bob", "inbox", "--peek", "--json"))
        self.assertEqual([p["id"] for p in payload if p.get("untrusted")], [], "the JSON view, which extensions deliver from, carries verified events only")
        self.assertNotIn(damaged["id"], [p["id"] for p in payload])
        # A control character in a claimed handle or reason never reaches the terminal.
        with self.as_agent("m\x1b[2Jallory"):
            junk = a2a.append("chat.message", a2a.ROOM_CHAT, to="bob", correlation_id=cid, body="x", status="open", metadata={"host": "hostA"})
        out = self.run_cli("bob", "read", cid)
        self.assertNotIn("\x1b", out)
        self.assertIn("mallory", out.replace("[2J", ""))
        del junk

    def test_an_acknowledgement_whose_presence_arrives_late_still_stops_the_courier(self):
        """bob read the invite and acknowledged it; his presence, which
        places him for the acknowledgement's signature, replicates one
        pass later. The courier holds the acknowledgement and never pastes
        the invite bob already read."""
        self.online("alice", "bob")
        self.run_cli("alice", "chat", "bob", "read me once")
        invite = next(e for e in self.ls.events if e["type"] == "chat.invite")
        self.run_cli("bob", "inbox")
        self.assertTrue(any(a["type"] == "event.ack" and a["from_agent"] == "bob" for a in self.ls.events))
        hidden = self.hide(lambda e: not (e["room"] == a2a.ROOM_PRESENCE and a2a.metadata_of(e).get("handle") == "bob"))
        delivered = []
        with mock.patch.object(a2a, "deliver", side_effect=lambda e, t: delivered.append(e["id"]) or "tmux"), mock.patch.object(a2a, "_age", return_value=1):
            state = a2a.courier_once({}, "hostA", verbose=False)
        self.assertEqual(delivered, [], "bob is not placed yet, so nothing is pasted")
        self.assertEqual(len(state["held_acks"]), 1)
        self.restore(hidden)
        with mock.patch.object(a2a, "deliver", side_effect=lambda e, t: delivered.append(e["id"]) or "tmux"), mock.patch.object(a2a, "_age", return_value=1):
            state = a2a.courier_once(state, "hostA", verbose=False)
        self.assertEqual(delivered, [], "acknowledged before delivery: never pasted")
        self.assertFalse(any(i["event"] == invite["id"] and i["to"] == "bob" for i in state["pending"].values()))
        self.assertEqual(state["held_acks"], {})

    def test_a_malformed_unsigned_scan_stops_no_chat_from_folding(self):
        self.online("alice", "bob")
        self.run_cli("alice", "chat", "bob", "still folds")
        with self.as_agent("alice"):
            cid = a2a.open_chat_with("bob")
        for meta in ({"host": "hostZ", "agents": 1}, {"host": None, "agents": [{"handle": "bob"}]}, {"host": "hostA", "agents": [1, "x", {"handle": 5}, {"handle": "bob", "host": 7}]}, {"agents": "bob"}, {"host": ["hostA"], "agents": {}}):
            self.ls.call("mempalace_event_append", {"type": "courier.scan", "stream": a2a.STREAM, "room": a2a.ROOM_PRESENCE, "from_agent": "courier@hostZ", "to_agent": "*", "metadata": meta})
            self.ls.call("mempalace_event_append", {"type": "courier.scan", "stream": a2a.STREAM, "room": a2a.ROOM_PRESENCE, "from_agent": "courier@hostA", "to_agent": "*", "metadata": a2a.signed_presence("courier.scan", meta)})
        a2a._HOSTS_CACHE.update(at=0.0)
        self.assertIn(cid, a2a.all_chats())
        self.assertIn("still folds", self.run_cli("bob", "read", cid))
        with mock.patch.object(a2a, "_age", return_value=1):
            self.assertEqual(a2a.roster()["bob"]["host"], "hostA")
        self.assertIn("sent to bob", self.run_cli("alice", "reply", cid, "and sends"))

    def test_a_sealed_message_of_nothing_is_inert(self):
        self.online("alice", "bob")
        self.run_cli("alice", "chat", "bob", "real")
        with self.as_agent("alice"):
            cid = a2a.open_chat_with("bob")
        before = a2a.all_chats()[cid]
        with self.as_agent("alice"):
            hosts = a2a.require_pinned_hosts(["bob"], binding=a2a.chat_binding(cid))
            for text in ("   ", "", " \n\t ", None, 7):
                body, enc = a2a.seal_body(text if isinstance(text, str) else "x", "chat.message", cid, "bob", hosts, {"lifecycle": "hosted"})
                if not isinstance(text, str):
                    # A box whose inner text is not a string at all.
                    nacl = a2a._nacl()
                    mine = a2a.require_host_key()
                    inner = {"v": 1, "eid": enc["eid"], "chat": cid, "type": "chat.message", "from": "alice", "to": "bob", "text": text, "lifecycle": "hosted", "from_fp": enc["from"], "to_fp": enc["to"], "at": time.time(), "after": []}
                    box = nacl.public.Box(mine.box, nacl.public.PublicKey(bytes(mine.box.public_key)))
                    body = base64.b64encode(box.encrypt(json.dumps(inner, sort_keys=True).encode(), base64.b64decode(enc["nonce"])).ciphertext).decode()
                a2a.append("chat.message", a2a.ROOM_CHAT, to="bob", correlation_id=cid, body=body, status="open", metadata={"host": "hostA", "lifecycle": "hosted", "enc": enc})
        after = a2a.all_chats()[cid]
        self.assertEqual(after["messages"], before["messages"])
        self.assertEqual([r for r in after["routes"].values() if r], [r for r in before["routes"].values() if r])
        with self.as_agent("bob"):
            self.assertEqual([e["id"] for e in a2a.unread()], [e["id"] for e in self.ls.events if e["type"] == "chat.invite"])
        delivered = []
        with mock.patch.object(a2a, "deliver", side_effect=lambda e, t: delivered.append(e["type"]) or "tmux"), mock.patch.object(a2a, "_age", return_value=1):
            a2a.courier_once({}, "hostA", verbose=False)
        self.assertEqual(delivered, ["chat.invite"])
        # The ledger agrees: one row, the invite, and no message counted.
        with self.as_agent("alice"):
            book = a2a.ledger("alice")["chats"][0]
        self.assertEqual(book["messages"], 0)
        self.assertEqual([r["kind"] for r in book["rows"]], ["message"])
        self.assertEqual(book["rows"][0]["text"], "real")
        self.assertEqual(book["state"], "invited")
        index = a2a.ledger_index()["chats"][0]
        self.assertEqual((index["rows"], index["state"]), (1, "invited"))

    def test_the_console_index_folds_only_verified_events(self):
        """An unsigned plaintext close in the opener's name, a forged invite
        and a replayed box change nothing in `ledger --index`."""
        self.online("alice", "bob")
        self.run_cli("alice", "chat", "bob", "open and sealed")
        with self.as_agent("alice"):
            cid = a2a.open_chat_with("bob")
        invite = next(e for e in self.ls.events if e["type"] == "chat.invite")
        before = a2a.ledger_index()
        with self.as_agent("alice"):
            a2a.append("chat.closed", a2a.ROOM_CHAT, to=cid, correlation_id=cid, body="closed", status="superseded", metadata={"host": "hostA", "lifecycle": "hosted", "close": "opener"})
        with self.as_agent("mallory"):
            a2a.append("chat.invite", a2a.ROOM_CHAT, to="bob", correlation_id="chat-forged", body="forged", status="open", metadata={"members": ["mallory", "bob"], "host": "hostA", "lifecycle": "hosted"})
        self.ls._append({**{k: v for k, v in invite.items() if k not in ("id", "seq", "created_at")}})
        self.assertEqual(a2a.ledger_index(), before)
        self.assertEqual(before["chats"][0]["state"], "invited")
        self.assertEqual([c["id"] for c in before["chats"]], [cid])

    def test_replicas_fold_the_same_events_the_same_way_whatever_order_they_arrived(self):
        """alice on hostA ends the chat while far on hostB, not yet holding
        the close, replies a moment earlier by its own clock. Each replica
        received the two in the opposite order; both fold the reply before
        the close, and a message that arrived before its invite folds once
        the invite is here."""
        self.two_hosts()
        self.run_cli("alice", "chat", "far", "opening")
        with self.as_agent("alice"):
            cid = a2a.open_chat_with("far")
        with self.state_b, self.host_b:
            self.run_cli("far", "reply", cid, "first answer")
        base = time.time()
        with mock.patch.object(a2a.time, "time", return_value=base + 10):
            self.run_cli("alice", "end", cid, "closing now")
        close = next(e for e in self.ls.events if e["type"] == "chat.closed")
        # far replies before seeing the close: the close is hidden from its
        # replica while it writes, and its clock is a moment behind.
        self.ls.events.remove(close)
        with self.state_b, self.host_b, mock.patch.object(a2a.time, "time", return_value=base + 5):
            self.run_cli("far", "reply", cid, "late by the relay, early by the clock")
        late = next(e for e in self.ls.events if e["type"] == "chat.message" and e["seq"] > close["seq"])
        # Replica order A: close then late reply. Replica order B: the reverse.
        rest = [e for e in self.ls.events if e is not late and e is not close]
        order_a = rest + [close, late]
        order_b = rest + [late, close]
        folds = {}
        for name, order in (("close-first", order_a), ("reply-first", order_b)):
            self.ls.events[:] = order
            for host, ctx in (("hostA", None), ("hostB", (self.state_b, self.host_b))):
                if ctx:
                    with ctx[0], ctx[1]:
                        st = a2a.all_chats()[cid]
                else:
                    st = a2a.all_chats()[cid]
                folds[(name, host)] = {k: v for k, v in st.items() if k not in ("routes",)} | {"routes": {k: v for k, v in st["routes"].items()}}
        self.assertEqual(folds[("close-first", "hostA")], folds[("reply-first", "hostA")])
        self.assertEqual(folds[("close-first", "hostB")], folds[("reply-first", "hostB")])
        self.assertEqual(folds[("close-first", "hostA")]["messages"], 2, "the reply written before the close, by its clock, counts")
        self.assertEqual(folds[("close-first", "hostA")]["state"], "closed")
        self.assertEqual(folds[("close-first", "hostA")]["routes"][late["id"]], ["alice"])
        self.assertEqual(folds[("close-first", "hostA")]["routes"][close["id"]], ["far"])
        # A message listed before its invite folds once both are here.
        self.ls.events[:] = order_b
        invite = next(e for e in self.ls.events if e["type"] == "chat.invite" and e["correlation_id"] == cid)
        first = next(e for e in self.ls.events if e["type"] == "chat.message" and e["correlation_id"] == cid)
        self.ls.events.remove(invite)
        self.ls.events.insert(self.ls.events.index(first) + 1, invite)
        self.assertEqual(a2a.all_chats()[cid]["messages"], 2)
        self.assertEqual(a2a.chat_events(cid)[0]["id"], invite["id"], "the fold's order is the boxes' order, not the relay's")
        # An event whose predecessor has not arrived waits for it, and says so.
        self.ls.events.remove(first)
        self.assertEqual([e["id"] for e in a2a.chat_events(cid)], [invite["id"]])
        self.assertEqual(a2a.all_chats()[cid]["state"], "invited")
        reasons = [u["reason"] for u in a2a.unreadable_events(cid)]
        self.assertEqual(len(reasons), 2)
        self.assertTrue(all("is not here yet" in r for r in reasons), reasons)

    def test_a_removed_history_record_is_never_retaken_from_the_relay(self):
        """A sealing key is made with its record in one step. If the
        record is gone, nothing seals, and migrate does not take a new one
        from the relay, whose timestamps nothing authenticates: a
        backdated plaintext invite injected meanwhile stays refused, and a
        person restores the record from a copy or accepts none."""
        self.online("alice", "bob")
        record = a2a._key_path(a2a.LEGACY_IDS_NAME)
        copy = Path(self.tmp.name) / "legacy-ids.copy"
        copy.write_text(record.read_text())
        record.unlink()
        a2a._LEGACY_IDS_CACHE.update(loaded=False)
        with self.as_agent("mallory"):
            forged = a2a.append("chat.invite", a2a.ROOM_CHAT, to="bob", correlation_id="chat-forged", body="backdated", status="open", metadata={"members": ["mallory", "bob"], "host": "hostA", "lifecycle": "hosted"})
        next(e for e in self.ls.events if e["id"] == forged["id"])["created_at"] = "2026-09-05T00:00:00Z"
        with self.assertRaises(SystemExit) as ctx:
            self.run_cli("alice", "key", "migrate")
        self.assertIn("a2a key history", str(ctx.exception))
        self.assertFalse(record.exists())
        self.assertNotIn("chat-forged", a2a.all_chats())
        with self.assertRaises(SystemExit):
            self.run_cli("alice", "chat", "bob", "nothing seals")
        self.assertIn("recorded 0", self.run_cli("alice", "key", "history", "--none"))
        self.assertNotIn("chat-forged", a2a.all_chats(), "the backdated invite is not history")
        self.assertIn("chat opened with bob", self.run_cli("alice", "chat", "bob", "seals again"))
        with self.assertRaises(SystemExit):
            self.run_cli("alice", "key", "history", "--none")
        # A copy taken before sealing restores the record as it was.
        record.unlink()
        a2a._LEGACY_IDS_CACHE.update(loaded=False)
        self.assertIn(f"recorded {len(copy.read_text().split())}", self.run_cli("alice", "key", "history", "--from", str(copy)))
        self.assertEqual(record.read_text(), copy.read_text())

    def test_a_frontier_is_never_truncated(self):
        """Every dependency is named or nothing is sent: with the limit
        lowered to eight, a chat with eight concurrent heads including a
        departure still names all eight, and a ninth head refuses the next
        event rather than drop one."""
        self.online("alice", "bob", "carol", "dave")
        self.run_cli("alice", "chat", "bob,carol,dave", "four of us")
        with self.as_agent("alice"):
            cid = a2a.open_chat_with("bob", "carol", "dave")
        invite = next(e for e in self.ls.events if e["type"] == "chat.invite")
        self.run_cli("bob", "leave", cid, "off")
        leave = next(e for e in self.ls.events if e["type"] == "chat.leave")
        heads = []
        for i in range(7):
            # Each of carol's messages is written as if nothing but the
            # invite were there: a concurrent head.
            hidden = self.hide(lambda e: e is invite or e["room"] != a2a.ROOM_CHAT)
            self.run_cli("carol", "reply", cid, f"concurrent {i}")
            heads.append([e for e in self.ls.events if e["type"] == "chat.message"][-1])
            self.restore(hidden)
        with mock.patch.object(a2a, "FRONTIER_LIMIT", 8):
            self.assertEqual(len(a2a.chat_frontier(cid)), 8)
            self.assertIn("sent to", self.run_cli("alice", "reply", cid, "names all eight"))
            reply = [e for e in self.ls.events if e["type"] == "chat.message"][-1]
            folded = next(e for e in a2a.chat_events(cid) if e["id"] == reply["id"])
            self.assertEqual(len(folded["after"]), 8)
            self.assertIn(leave["metadata"]["enc"]["eid"], folded["after"])
            # One more concurrent head: nine, and the next event refuses.
            hidden = self.hide(lambda e: e is invite or e["room"] != a2a.ROOM_CHAT)
            self.run_cli("carol", "reply", cid, "concurrent 7")
            self.restore(hidden)
            self.assertEqual(len(a2a.chat_frontier(cid)), 2, "the reply gathered the eight; the new head and it are the frontier")
            before = len([e for e in self.ls.events if e["type"].startswith("chat.")])
            with mock.patch.object(a2a, "chat_frontier", return_value=[f"k{i}" for i in range(9)]):
                with self.assertRaises(SystemExit) as ctx:
                    self.run_cli("alice", "reply", cid, "too wide")
            self.assertIn("frontier", str(ctx.exception))
            self.assertEqual(len([e for e in self.ls.events if e["type"].startswith("chat.")]), before)
        # A box naming more than a reader's limit is refused by the reader too.
        self.assertIsNone(a2a.open_body(reply)[1])
        with mock.patch.object(a2a, "FRONTIER_LIMIT", 1):
            self.assertIn("badly", a2a.open_body(reply)[1])
        # A replica missing the departure holds the reply that names it.
        hidden = self.hide(lambda e: e is not leave)
        self.assertNotIn(reply["id"], a2a.all_chats()[cid]["routes"])
        self.assertNotIn("names all eight", self.run_cli("bob", "read", cid))
        self.restore(hidden)
        self.assertEqual(a2a.all_chats()[cid]["routes"][reply["id"]], ["carol", "dave"])

    def test_a_queued_delivery_is_judged_again_when_an_earlier_close_arrives(self):
        """alice ended the chat; carol, not holding the close, wrote after
        it by the clock; the courier queued carol's message. When the close
        arrives it precedes the message, which is now nobody's, and the
        courier writes nothing."""
        self.online("alice", "bob", "carol")
        self.run_cli("alice", "chat", "bob,carol", "three")
        with self.as_agent("alice"):
            cid = a2a.open_chat_with("bob", "carol")
        self.run_cli("alice", "end", cid, "done")
        close = next(e for e in self.ls.events if e["type"] == "chat.closed")
        hidden = self.hide(lambda e: e is not close)
        self.run_cli("carol", "reply", cid, "did not see the close")
        message = [e for e in self.ls.events if e["type"] == "chat.message"][-1]
        with mock.patch.object(a2a, "deliver", return_value="unavailable"), mock.patch.object(a2a, "_age", return_value=1):
            state = a2a.courier_once({}, "hostA", verbose=False)
        self.assertEqual({i["to"] for i in state["pending"].values() if i["event"] == message["id"]}, {"alice", "bob"})
        self.restore(hidden)
        self.assertEqual(a2a.all_chats()[cid]["routes"][message["id"]], [])
        for item in state["pending"].values():
            item["next"] = 0
        pastes = []
        with mock.patch.object(a2a, "deliver", side_effect=lambda e, t: pastes.append((e["id"], t["handle"])) or "tmux"), mock.patch.object(a2a, "_age", return_value=1):
            state = a2a.courier_once(state, "hostA", verbose=False)
        self.assertEqual([p for p in pastes if p[0] == message["id"]], [])
        self.assertEqual([i for i in state["pending"].values() if i["event"] == message["id"]], [])

    def test_a_released_key_upgrade_resumes_after_an_outage_at_any_step(self):
        """The record is taken before the key is replaced and the announce
        after it, so a relay that fails at any step leaves a state the next
        run resumes from, never a new key without its history."""
        self.online("alice")
        old = self.append_chat("alice", "chat.invite", "chat-old", body="before", metadata={"members": ["alice", "bob"], "host": "hostA"}, legacy=False)
        record = a2a._key_path(a2a.LEGACY_IDS_NAME)
        record.unlink()
        a2a.host_key_path().write_text(base64.b64encode(os.urandom(32)).decode() + "\n")
        a2a._HOST_KEY_CACHE.update(path=None)
        a2a._LEGACY_IDS_CACHE.update(loaded=False)
        real = a2a._rpc
        # Step one fails: the relay does not list events. Nothing changes.
        with mock.patch.object(a2a, "_rpc", side_effect=lambda tool, args, timeout=30.0: (_ for _ in ()).throw(a2a.LogstreamError("down")) if tool == "mempalace_event_list" else real(tool, args)):
            with self.assertRaises(SystemExit) as ctx:
                self.run_cli("alice", "key", "migrate")
        self.assertIn("did not answer", str(ctx.exception))
        self.assertEqual(a2a.host_key_format(), "x25519")
        self.assertFalse(record.exists())
        # Step two fails: the record is taken, the key replaced, the
        # announce refused. The next run announces and is done.
        with mock.patch.object(a2a, "_rpc", side_effect=lambda tool, args, timeout=30.0: (_ for _ in ()).throw(a2a.LogstreamError("down")) if tool == "mempalace_event_append" else real(tool, args)):
            with self.assertRaises(SystemExit) as ctx:
                self.run_cli("alice", "key", "migrate")
        self.assertIn("did not answer", str(ctx.exception))
        self.assertEqual(a2a.host_key_format(), "ed25519")
        self.assertEqual(set(a2a.legacy_ids()), {old["id"]})
        self.assertNotIn("hostA", a2a.announced_keys())
        out = self.run_cli("alice", "key", "migrate")
        self.assertIn("announced", out)
        self.assertIn("recorded 1", out)
        self.assertEqual(a2a.announced_keys()["hostA"]["fingerprint"], self.run_cli("alice", "key", "show").strip())
        self.assertIn("chat-old", a2a.all_chats())

    def test_a_dependency_is_satisfied_only_by_an_accepted_event(self):
        """far on hostB leaves; alice, holding the leave, writes to the
        room. A replica that has every box but not far's signed presence
        cannot accept the leave, so alice's message, which names it, waits
        there rather than route to far; once the presence arrives, it
        folds without far."""
        self.two_hosts()
        self.online("bob", "carol")
        self.run_cli("alice", "chat", "bob,carol,far", "all four")
        with self.as_agent("alice"):
            cid = a2a.open_chat_with("bob", "carol", "far")
        with self.state_b, self.host_b:
            self.run_cli("far", "leave", cid, "gone")
        self.run_cli("alice", "reply", cid, "after far left")
        leave = next(e for e in self.ls.events if e["type"] == "chat.leave")
        message = [e for e in self.ls.events if e["type"] == "chat.message"][-1]
        self.assertEqual(a2a.all_chats()[cid]["routes"][message["id"]], ["bob", "carol"])
        hidden = self.hide(lambda e: not (e["room"] == a2a.ROOM_PRESENCE and a2a.metadata_of(e).get("handle") == "far"))
        st = a2a.all_chats()[cid]
        self.assertNotIn(leave["id"], st["routes"], "the leave is not accepted without far's placement")
        self.assertNotIn(message["id"], st["routes"], "so the message that follows it waits")
        with self.state_b, self.host_b:
            self.assertNotIn("after far left", self.run_cli("far", "read", cid))
        self.assertIn("not here yet", a2a.unreadable_events(cid)[-1]["reason"])
        self.restore(hidden)
        self.assertEqual(a2a.all_chats()[cid]["routes"][message["id"]], ["bob", "carol"])
        with self.state_b, self.host_b:
            self.assertNotIn("after far left", self.run_cli("far", "read", cid))

    def test_an_event_id_is_scoped_to_its_chat_and_a_conflicting_reuse_counts_for_nothing(self):
        """A pinned third host copies a local invite's id into a valid box
        of its own: the local chat is untouched in either relay order. Two
        different boxes under one id in one chat are a conflict and
        neither counts; an identical copy is one event."""
        self.online("alice", "bob")
        self.run_cli("alice", "chat", "bob", "local only")
        with self.as_agent("alice"):
            local = a2a.open_chat_with("bob")
        invite = next(e for e in self.ls.events if e["type"] == "chat.invite")
        eid = invite["metadata"]["enc"]["eid"]
        fp_a = self.run_cli("alice", "key", "show").strip()
        self.run_cli("alice", "key", "announce")
        state_c, host_c = self.other_host("hostC")
        with state_c, host_c:
            fp_c = self.run_cli("mallory", "key", "show").strip()
            self.run_cli("mallory", "key", "announce")
            self.run_cli("mallory", "key", "trust", "hostA", fp_a)
        self.run_cli("alice", "key", "trust", "hostC", fp_c)
        self.online_on("mallory", "hostC")
        with state_c, host_c:
            nacl = a2a._nacl()
            mine = a2a.require_host_key()
            box = nacl.public.Box(mine.box, nacl.public.PublicKey(a2a.box_public(a2a._public_key_for_fingerprint(fp_a, mine))))
            other = a2a.chat_id_for(fp_c, eid)
            inner = {"v": 1, "eid": eid, "chat": other, "type": "chat.invite", "from": "mallory", "to": "alice", "text": "same id, other chat", "members": ["mallory", "alice"], "lifecycle": "hosted", "from_fp": fp_c, "to_fp": fp_a, "at": time.time(), "after": []}
            nonce = b"\x05" * nacl.public.Box.NONCE_SIZE
            ct = box.encrypt(json.dumps(inner, sort_keys=True).encode(), nonce).ciphertext
        with self.as_agent("mallory"):
            foreign = a2a.append("chat.invite", a2a.ROOM_CHAT, to="alice", correlation_id=other, body=base64.b64encode(ct).decode(), status="open",
                                 metadata={"members": ["mallory", "alice"], "host": "hostC", "lifecycle": "hosted", "enc": {"v": 1, "alg": "nacl-box", "eid": eid, "to": fp_a, "from": fp_c, "nonce": base64.b64encode(nonce).decode()}})
        self.assertIsNone(a2a.open_body(foreign)[1])
        for order in ("local first", "foreign first"):
            with self.subTest(order=order):
                if order == "foreign first":
                    self.ls.events.remove(foreign)
                    self.ls.events.insert(self.ls.events.index(invite), foreign)
                chats = a2a.all_chats()
                self.assertIn(local, chats)
                self.assertIn(other, chats)
                self.assertEqual([e["id"] for e in a2a.chat_events(local)], [invite["id"]])
        # A conflicting reuse within the local chat: alice's real reply and
        # a different box under the same id, in either order; neither counts.
        self.run_cli("alice", "reply", local, "genuine")
        reply = [e for e in self.ls.events if e["type"] == "chat.message"][-1]
        with self.as_agent("alice"):
            hosts = a2a.require_pinned_hosts(["bob"], binding=a2a.chat_binding(local))
            body, enc = a2a.seal_body("different", "chat.message", local, "bob", hosts, {"lifecycle": "hosted"}, after=a2a.chat_frontier(local))
        enc["eid"] = reply["metadata"]["enc"]["eid"]
        with self.as_agent("alice"):
            hosts = a2a.require_pinned_hosts(["bob"], binding=a2a.chat_binding(local))
            nacl = a2a._nacl()
            mine = a2a.require_host_key()
            inner = {"v": 1, "eid": enc["eid"], "chat": local, "type": "chat.message", "from": "alice", "to": "bob", "text": "different", "lifecycle": "hosted", "from_fp": enc["from"], "to_fp": enc["to"], "at": time.time(), "after": []}
            box = nacl.public.Box(mine.box, nacl.public.PublicKey(bytes(mine.box.public_key)))
            body = base64.b64encode(box.encrypt(json.dumps(inner, sort_keys=True).encode(), base64.b64decode(enc["nonce"])).ciphertext).decode()
            conflict = a2a.append("chat.message", a2a.ROOM_CHAT, to="bob", correlation_id=local, body=body, status="open", metadata={"host": "hostA", "lifecycle": "hosted", "enc": enc})
        for order in ("genuine first", "conflict first"):
            with self.subTest(order=order):
                if order == "conflict first":
                    self.ls.events.remove(conflict)
                    self.ls.events.insert(self.ls.events.index(reply), conflict)
                ids = [e["id"] for e in a2a.chat_events(local)]
                self.assertNotIn(reply["id"], ids)
                self.assertNotIn(conflict["id"], ids)
                self.assertEqual(a2a.all_chats()[local]["messages"], 0)
                self.assertIn(invite["id"], ids)
        # An identical copy under a new relay id is the same event once.
        self.ls.events.remove(conflict)
        copy = self.ls._append({k: v for k, v in reply.items() if k not in ("id", "seq", "created_at")})
        ids = [e["id"] for e in a2a.chat_events(local)]
        self.assertEqual(ids.count(reply["id"]) + ids.count(copy["id"]), 1)
        self.assertEqual(a2a.all_chats()[local]["messages"], 1)

    def test_a_respelt_copy_or_a_third_hosts_box_under_a_chats_event_id_invalidates_nothing(self):
        """A copy of the invite whose base64 is re-spelt is not a box at
        all, so it neither duplicates nor conflicts with the original; and
        a pinned third host sealing its own box under the chat's id and
        the invite's event id is judged on its own key and dropped, on
        both endpoints, while the genuine chat and what follows it stand."""
        self.two_hosts()
        self.run_cli("alice", "chat", "far", "between A and B")
        with self.as_agent("alice"):
            cid = a2a.open_chat_with("far")
        invite = next(e for e in self.ls.events if e["type"] == "chat.invite")
        with self.state_b, self.host_b:
            self.run_cli("far", "reply", cid, "a descendant")
        respelt = self.ls._append({**{k: v for k, v in invite.items() if k not in ("id", "seq", "created_at")}, "body": invite["body"] + "\n"})
        self.assertIsNotNone(a2a.open_body(respelt)[1], "a re-spelt copy is not a box")
        self.assertEqual([e["id"] for e in a2a.chat_events(cid)][:2], [invite["id"], [e for e in self.ls.events if e["type"] == "chat.message"][-1]["id"]])
        self.assertEqual(a2a.all_chats()[cid]["messages"], 1)
        # hostC, pinned by both, seals its own message under this chat's id
        # and the invite's event id, addressed to hostA.
        fp_a = self.run_cli("alice", "key", "show").strip()
        state_c, host_c = self.other_host("hostC")
        with state_c, host_c:
            fp_c = self.run_cli("mallory", "key", "show").strip()
            self.run_cli("mallory", "key", "announce")
            self.run_cli("mallory", "key", "trust", "hostA", fp_a)
        self.run_cli("alice", "key", "trust", "hostC", fp_c)
        with self.state_b, self.host_b:
            self.run_cli("far", "key", "trust", "hostC", fp_c)
        self.online_on("mallory", "hostC")
        eid = invite["metadata"]["enc"]["eid"]
        with state_c, host_c:
            nacl = a2a._nacl()
            mine = a2a.require_host_key()
            box = nacl.public.Box(mine.box, nacl.public.PublicKey(a2a.box_public(a2a._public_key_for_fingerprint(fp_a, mine))))
            inner = {"v": 1, "eid": eid, "chat": cid, "type": "chat.message", "from": "mallory", "to": "alice", "text": "veto", "lifecycle": "hosted", "from_fp": fp_c, "to_fp": fp_a, "at": time.time(), "after": []}
            nonce = b"\x06" * nacl.public.Box.NONCE_SIZE
            ct = box.encrypt(json.dumps(inner, sort_keys=True).encode(), nonce).ciphertext
        with self.as_agent("mallory"):
            veto = a2a.append("chat.message", a2a.ROOM_CHAT, to="alice", correlation_id=cid, body=base64.b64encode(ct).decode(), status="open",
                              metadata={"host": "hostC", "lifecycle": "hosted", "enc": {"v": 1, "alg": "nacl-box", "eid": eid, "to": fp_a, "from": fp_c, "nonce": base64.b64encode(nonce).decode()}})
        self.assertIsNone(a2a.open_body(veto)[1], "the box itself opens on hostA: C's key is pinned")
        for order in ("veto last", "veto first"):
            with self.subTest(order=order):
                if order == "veto first":
                    self.ls.events.remove(veto)
                    self.ls.events.insert(self.ls.events.index(invite), veto)
                for host, ctx in (("hostA", None), ("hostB", (self.state_b, self.host_b))):
                    with (ctx[0] if ctx else mock.patch.object(a2a, "me", a2a.me)), (ctx[1] if ctx else mock.patch.object(a2a, "me", a2a.me)):
                        st = a2a.all_chats()[cid]
                        self.assertEqual(st["messages"], 1, host)
                        self.assertNotIn(veto["id"], [e["id"] for e in a2a.chat_events(cid)], host)
                        self.assertEqual(a2a.chat_events(cid)[0]["id"], invite["id"], host)
        self.assertIn("a descendant", self.run_cli("alice", "read", cid))
        with self.state_b, self.host_b:
            self.assertIn("a descendant", self.run_cli("far", "read", cid))

    def test_an_acknowledgement_names_the_event_every_replica_keeps(self):
        """Two replicas hold the invite and an identical copy in opposite
        orders, so each keeps a different relay row for the one event. far
        reads it on hostB; the acknowledgement names the box's own id, and
        alice's ledger on hostA says read, whichever row it kept."""
        self.two_hosts()
        self.run_cli("alice", "chat", "far", "once, twice listed")
        invite = next(e for e in self.ls.events if e["type"] == "chat.invite")
        copy = self.ls._append({k: v for k, v in invite.items() if k not in ("id", "seq", "created_at")})
        # hostB lists the copy first and reads there.
        self.ls.events.remove(copy)
        self.ls.events.insert(self.ls.events.index(invite), copy)
        with self.state_b, self.host_b:
            self.assertEqual([e["id"] for e in a2a.chat_events(invite["correlation_id"])], [copy["id"]])
            self.run_cli("far", "inbox")
            self.assertEqual(a2a.unread(), [])
        ack = next(a for a in self.ls.events if a["type"] == "event.ack" and a["from_agent"] == "far")
        self.assertEqual(a2a.metadata_of(ack)["ack_of"], copy["id"])
        # hostA lists the original first.
        self.ls.events.remove(copy)
        self.ls.events.insert(self.ls.events.index(invite) + 1, copy)
        self.assertEqual([e["id"] for e in a2a.chat_events(invite["correlation_id"])], [invite["id"]])
        with self.as_agent("alice"):
            row = a2a.ledger("alice")["chats"][0]["rows"][0]
        self.assertEqual((row["id"], row["to"]), (invite["id"], {"far": "read"}))
        # And with the copy first on hostA too: the same answer.
        self.ls.events.remove(copy)
        self.ls.events.insert(self.ls.events.index(invite), copy)
        with self.as_agent("alice"):
            row = a2a.ledger("alice")["chats"][0]["rows"][0]
        self.assertEqual((row["id"], row["to"]), (copy["id"], {"far": "read"}))
        # The courier on hostA never pastes a delivery far has read.
        delivered = []
        with mock.patch.object(a2a, "deliver", side_effect=lambda e, t: delivered.append(e["id"]) or "tmux"), mock.patch.object(a2a, "_age", return_value=1):
            a2a.courier_once({}, "hostA", verbose=False)
        self.assertEqual(delivered, [])

    def test_a_first_invitation_this_host_cannot_read_is_still_reported(self):
        """far's invite to alice is damaged in transit: it opens no chat,
        routes nowhere and is never acknowledged, but alice's inbox names
        it, unverified, with the reason, rather than saying nothing."""
        self.two_hosts()
        with self.state_b, self.host_b:
            self.run_cli("far", "chat", "alice", "first contact")
        invite = [e for e in self.ls.events if e["type"] == "chat.invite"][-1]
        invite["body"] = "AAAA" + invite["body"][4:]
        self.assertNotIn(invite["correlation_id"], a2a.all_chats())
        out = self.run_cli("alice", "inbox", "--peek")
        self.assertNotIn("no unread messages", out)
        self.assertIn(f"Chat: {invite['correlation_id']}", out)
        self.assertIn("Unreadable: invite claiming to be from far (unverified)", out)
        payload = json.loads(self.run_cli("alice", "inbox", "--peek", "--json"))
        self.assertEqual(payload, [], "the JSON view carries verified events only")
        self.run_cli("alice", "inbox")
        self.assertEqual([a for a in self.ls.events if a["type"] == "event.ack"], [], "never acknowledged")
        # An invitation that does not claim to address alice is not hers to
        # see, and a malformed one, whatever its members field holds, stops
        # nothing: valid messages still print.
        with self.as_agent("mallory"):
            a2a.append("chat.invite", a2a.ROOM_CHAT, to="bob", correlation_id="chat-elsewhere", body="x", status="open", metadata={"members": ["mallory", "bob"], "host": "hostA"})
            for members in (7, "alice", {"alice": 1}, [7, None, ["alice"]], None):
                a2a.append("chat.invite", a2a.ROOM_CHAT, to="", correlation_id="chat-malformed", body="x", status="open", metadata={"members": members, "host": "hostA"})
        self.online("bob")
        self.run_cli("bob", "chat", "alice", "still delivered")
        out = self.run_cli("alice", "inbox", "--peek")
        self.assertNotIn("chat-elsewhere", out)
        self.assertNotIn("chat-malformed", out)
        self.assertIn("still delivered", out)
        self.assertIn("Unreadable: invite claiming to be from far", out)

    def test_a_chats_hosts_are_kept_once_seen_so_a_backdated_invite_cannot_re_open_it(self):
        """A pinned third host seals its own invitation to hostA under an
        existing chat's id with an earlier moment. The chat's host pair was
        fixed when hostA first saw it and is kept: the box is from outside
        the pair and counts for nothing on either endpoint, and a reply by
        chat id still goes to hostB."""
        self.two_hosts()
        self.run_cli("alice", "chat", "far", "A and B")
        with self.as_agent("alice"):
            cid = a2a.open_chat_with("far")
        self.assertEqual(a2a.chat_binding(cid)[0], {"hostA", "hostB"})
        fp_a = self.run_cli("alice", "key", "show").strip()
        fp_b = self.run_cli("alice", "key", "status").split("hostB")[1].split("Announced:")[1].split()[0]
        state_c, host_c = self.other_host("hostC")
        with state_c, host_c:
            fp_c = self.run_cli("mallory", "key", "show").strip()
            self.run_cli("mallory", "key", "announce")
            self.run_cli("mallory", "key", "trust", "hostA", fp_a)
        self.run_cli("alice", "key", "trust", "hostC", fp_c)
        with self.state_b, self.host_b:
            self.run_cli("far", "key", "trust", "hostC", fp_c)
        self.online_on("mallory", "hostC")
        with state_c, host_c:
            nacl = a2a._nacl()
            mine = a2a.require_host_key()
            box = nacl.public.Box(mine.box, nacl.public.PublicKey(a2a.box_public(a2a._public_key_for_fingerprint(fp_a, mine))))
            inner = {"v": 1, "eid": "7" * 32, "chat": cid, "type": "chat.invite", "from": "mallory", "to": "alice", "text": "mine now", "members": ["mallory", "alice"], "lifecycle": "hosted", "from_fp": fp_c, "to_fp": fp_a, "at": time.time() - 3600, "after": []}
            nonce = b"\x07" * nacl.public.Box.NONCE_SIZE
            ct = box.encrypt(json.dumps(inner, sort_keys=True).encode(), nonce).ciphertext
        with self.as_agent("mallory"):
            backdated = a2a.append("chat.invite", a2a.ROOM_CHAT, to="alice", correlation_id=cid, body=base64.b64encode(ct).decode(), status="open",
                                   metadata={"members": ["mallory", "alice"], "host": "hostC", "lifecycle": "hosted", "enc": {"v": 1, "alg": "nacl-box", "eid": "7" * 32, "to": fp_a, "from": fp_c, "nonce": base64.b64encode(nonce).decode()}})
        self.assertIsNone(a2a.open_body(backdated)[1], "the box itself opens on hostA")
        for host, ctx in (("hostA", None), ("hostB", (self.state_b, self.host_b))):
            with (ctx[0] if ctx else mock.patch.object(a2a, "me", a2a.me)), (ctx[1] if ctx else mock.patch.object(a2a, "me", a2a.me)):
                st = a2a.all_chats()[cid]
                self.assertEqual(st["members"], ["alice", "far"], host)
                self.assertEqual(a2a.chat_binding(cid)[0], {"hostA", "hostB"}, host)
                self.assertNotIn(backdated["id"], [e["id"] for e in a2a.chat_events(cid)], host)
        self.assertIn("sent to far", self.run_cli("alice", "reply", cid, "still to B"))
        self.assertEqual([e["metadata"]["enc"]["to"] for e in self.ls.events if e["type"] == "chat.message"][-1], fp_b)
        with self.state_b, self.host_b:
            self.assertIn("still to B", self.run_cli("far", "read", cid))
        # A fresh replica of hostA that sees the backdated box first, with
        # no record of the pair, fixes it from the genuine invitation all
        # the same: the chat's id is derived from its opener's key, and
        # hostC's key did not open this chat.
        __import__("shutil").rmtree(a2a._key_path(a2a.CHAT_HOSTS_NAME))
        a2a._CHAT_HOSTS_CACHE.update(path=None)
        self.ls.events.remove(backdated)
        self.ls.events.insert(0, backdated)
        self.assertEqual(a2a.chat_binding(cid)[0], {"hostA", "hostB"})
        self.assertEqual(a2a.all_chats()[cid]["members"], ["alice", "far"])
        self.assertEqual(cid, a2a.chat_id_for(fp_a, next(e for e in self.ls.events if e["type"] == "chat.invite" and e["from_agent"] == "alice")["metadata"]["enc"]["eid"]))

    def test_an_acknowledgement_is_scoped_to_its_chat_and_key(self):
        """bob acknowledged alice's local invite. A pinned third host's own
        invitation in another chat, carrying that box's id in the clear,
        is neither seen by bob's cursor nor mistaken for the acknowledged
        event: bob's inbox stays empty of the local invite and the courier
        never pastes it again, while the foreign invitation is not marked
        seen."""
        self.online("alice", "bob")
        self.run_cli("alice", "chat", "bob", "local")
        invite = next(e for e in self.ls.events if e["type"] == "chat.invite")
        self.run_cli("bob", "inbox")
        with self.as_agent("bob"):
            self.assertEqual(a2a.unread(), [])
        fp_a = self.run_cli("alice", "key", "show").strip()
        self.run_cli("alice", "key", "announce")
        state_c, host_c = self.other_host("hostC")
        with state_c, host_c:
            fp_c = self.run_cli("mallory", "key", "show").strip()
            self.run_cli("mallory", "key", "announce")
            self.run_cli("mallory", "key", "trust", "hostA", fp_a)
        self.run_cli("alice", "key", "trust", "hostC", fp_c)
        self.online_on("mallory", "hostC")
        eid = invite["metadata"]["enc"]["eid"]
        with state_c, host_c:
            nacl = a2a._nacl()
            mine = a2a.require_host_key()
            box = nacl.public.Box(mine.box, nacl.public.PublicKey(a2a.box_public(a2a._public_key_for_fingerprint(fp_a, mine))))
            other = a2a.chat_id_for(fp_c, eid)
            inner = {"v": 1, "eid": eid, "chat": other, "type": "chat.invite", "from": "mallory", "to": "bob", "text": "same id elsewhere", "members": ["mallory", "bob"], "lifecycle": "hosted", "from_fp": fp_c, "to_fp": fp_a, "at": time.time(), "after": []}
            nonce = b"\x08" * nacl.public.Box.NONCE_SIZE
            ct = box.encrypt(json.dumps(inner, sort_keys=True).encode(), nonce).ciphertext
        with self.as_agent("mallory"):
            foreign = a2a.append("chat.invite", a2a.ROOM_CHAT, to="bob", correlation_id=other, body=base64.b64encode(ct).decode(), status="open",
                                 metadata={"members": ["mallory", "bob"], "host": "hostC", "lifecycle": "hosted", "enc": {"v": 1, "alg": "nacl-box", "eid": eid, "to": fp_a, "from": fp_c, "nonce": base64.b64encode(nonce).decode()}})
        with self.as_agent("bob"):
            seen = a2a.seen_ids(None)
            self.assertIn(invite["id"], seen)
            self.assertNotIn(foreign["id"], seen)
            self.assertEqual([e["id"] for e in a2a.unread()], [foreign["id"]], "the foreign invitation is unread; the local one is read")
        delivered = []
        with mock.patch.object(a2a, "deliver", side_effect=lambda e, t: delivered.append(e["id"]) or "tmux"), mock.patch.object(a2a, "_age", return_value=1):
            a2a.courier_once({}, "hostA", verbose=False)
        self.assertNotIn(invite["id"], delivered, "read already; never pasted again")

    def test_an_acknowledgement_that_arrives_before_its_event_is_held_until_the_event_does(self):
        """bob read alice's invite; this replica gets bob's signed
        acknowledgement before the invite's box. The courier keeps the
        acknowledgement until the box arrives, and then never pastes an
        invite bob already read, while other deliveries proceed."""
        self.online("alice", "bob", "carol")
        self.run_cli("alice", "chat", "bob", "read before it arrives here")
        invite = next(e for e in self.ls.events if e["type"] == "chat.invite")
        self.run_cli("bob", "inbox")
        self.run_cli("alice", "chat", "carol", "another chat, pending")
        hidden = self.hide(lambda e: e is not invite)
        delivered = []
        with mock.patch.object(a2a, "deliver", side_effect=lambda e, t: delivered.append((e["id"], t["handle"])) or "tmux"), mock.patch.object(a2a, "_age", return_value=1):
            state = a2a.courier_once({}, "hostA", verbose=False)
        self.assertEqual([d[1] for d in delivered], ["carol"], "the other chat is delivered; the hidden invite is not here")
        self.assertEqual(len(state["held_acks"]), 1, "bob's acknowledgement waits for its event")
        # The box arrives as replication delivers it: a new relay row.
        arrived = self.ls._append({k: v for k, v in invite.items() if k not in ("id", "seq", "created_at")})
        a2a._HOSTS_CACHE.update(at=0.0)
        with mock.patch.object(a2a, "deliver", side_effect=lambda e, t: delivered.append((e["id"], t["handle"])) or "tmux"), mock.patch.object(a2a, "_age", return_value=1):
            state = a2a.courier_once(state, "hostA", verbose=False)
        self.assertNotIn("bob", [d[1] for d in delivered], "read already; never pasted")
        self.assertEqual(state["held_acks"], {})
        self.assertFalse(any(i["event"] == arrived["id"] and i["to"] == "bob" for i in state["pending"].values()))

    def test_chat_host_records_survive_concurrent_writers(self):
        """Agents, the courier and the ledger all record a chat's host pair
        when they first see it; recording from many processes at once
        loses none of them and never raises."""
        import threading
        errors = []

        def record(n):
            try:
                a2a._record_chat_hosts({f"chat-{n:04d}-{i}": {"hostA", "hostB"} for i in range(20)})
                a2a._record_chat_hosts({"chat-shared": {"hostA", "hostB"}})
            except Exception as exc:  # noqa: BLE001 - the test reports any error
                errors.append(exc)

        threads = [threading.Thread(target=record, args=(n,)) for n in range(8)]
        for t in threads:
            t.start()
        for t in threads:
            t.join()
        self.assertEqual(errors, [])
        a2a._CHAT_HOSTS_CACHE.update(path=None)
        known = a2a.recorded_chat_hosts()
        self.assertEqual(len([c for c in known if c.startswith("chat-0")]), 160)
        self.assertEqual(known["chat-shared"], {"hostA", "hostB"})
        # A record is immutable: a later, different pair for the same chat changes nothing.
        a2a._record_chat_hosts({"chat-shared": {"hostA", "hostC"}})
        a2a._CHAT_HOSTS_CACHE.update(path=None)
        self.assertEqual(a2a.recorded_chat_hosts()["chat-shared"], {"hostA", "hostB"})

    def test_an_uncertain_delivery_is_one_delivery_whichever_relay_row_represents_the_box(self):
        """The invite was pasted with an uncertain outcome. Its identical
        box then arrives under a new relay row that this replica lists
        first, so the fold's representative changes. The courier files
        deliveries by the box's own identity: nothing is pasted twice."""
        self.online("alice", "bob")
        self.run_cli("alice", "chat", "bob", "once only, whatever the row")
        invite = next(e for e in self.ls.events if e["type"] == "chat.invite")
        pastes = []
        with mock.patch.object(a2a, "deliver", side_effect=lambda e, t: pastes.append(e["id"]) or "uncertain"), mock.patch.object(a2a, "_age", return_value=1):
            state = a2a.courier_once({}, "hostA", verbose=False)
        self.assertEqual(len(pastes), 1)
        copy = self.ls._append({k: v for k, v in invite.items() if k not in ("id", "seq", "created_at")})
        self.ls.events.remove(copy)
        self.ls.events.insert(self.ls.events.index(invite), copy)
        self.assertEqual([e["id"] for e in a2a.chat_events(invite["correlation_id"])], [copy["id"]], "the copy now represents the box")
        for item in state["pending"].values():
            item["next"] = 0
        with mock.patch.object(a2a, "deliver", side_effect=lambda e, t: pastes.append(e["id"]) or "tmux"), mock.patch.object(a2a, "_age", return_value=1):
            state = a2a.courier_once(state, "hostA", verbose=False)
            state = a2a.courier_once(state, "hostA", verbose=False)
        self.assertEqual(len(pastes), 1, "an uncertain write is never repeated under another row")
        acks = [a for a in self.ls.events if a["type"] == "event.ack" and a["from_agent"] == "courier@hostA"]
        self.assertEqual(len(acks), 1)
        self.assertEqual(a2a.ack_note(acks[0]), "uncertain for bob")
        # A restart from persisted state finds the same delivery settled.
        restarted = json.loads(json.dumps(state))
        with mock.patch.object(a2a, "deliver", side_effect=lambda e, t: pastes.append(e["id"]) or "tmux"), mock.patch.object(a2a, "_age", return_value=1):
            a2a.courier_once(restarted, "hostA", verbose=False)
        self.assertEqual(len(pastes), 1)

    def test_malformed_relay_events_stop_neither_init_nor_migration(self):
        """A stray relay event with an envelope of the wrong shape, or a
        non-dict envelope, is neither an error nor trusted plaintext when
        the history is recorded, at init and when a released key migrates."""
        self.online("alice", "bob")
        for meta in ({"enc": {"to": [], "from": "unknown"}}, {"enc": {"to": 5}}, {"enc": "x"}, {"enc": None}, {"enc": []}):
            with self.as_agent("mallory"):
                a2a.append("chat.invite", a2a.ROOM_CHAT, to="alice", correlation_id="chat-stray", body="stray", status="open", metadata={"members": ["mallory", "alice"], "host": "hostA", **meta})
        old = self.append_chat("alice", "chat.invite", "chat-old", body="before", metadata={"members": ["alice", "bob"], "host": "hostA"}, legacy=False)
        record = a2a._key_path(a2a.LEGACY_IDS_NAME)
        record.unlink()
        a2a._key_path(a2a.HOST_KEY_NAME).unlink()
        a2a._HOST_KEY_CACHE.update(path=None)
        a2a._LEGACY_IDS_CACHE.update(loaded=False)
        self.run_cli("alice", "key", "init")
        self.assertEqual(set(a2a.legacy_ids()), {old["id"]}, "only an event with no envelope at all is history")
        self.assertNotIn("chat-stray", a2a.all_chats())
        # The same on a released-key upgrade.
        record.unlink()
        a2a.host_key_path().unlink()
        a2a.host_key_path().write_text(base64.b64encode(os.urandom(32)).decode() + "\n")
        a2a._HOST_KEY_CACHE.update(path=None)
        a2a._LEGACY_IDS_CACHE.update(loaded=False)
        self.assertIn("recorded 1", self.run_cli("alice", "key", "migrate"))
        self.assertEqual(set(a2a.legacy_ids()), {old["id"]})
        self.assertNotIn("chat-stray", a2a.all_chats())

    def test_migration_keeps_the_delivery_and_read_facts_of_the_history(self):
        """A released-format host with a plaintext invite, bob's read
        acknowledgement, a courier delivery to bob and an uncertain paste
        to carol: after migrate the ledger still says read, delivered and
        uncertain, while an unsigned acknowledgement written afterwards
        counts for nothing."""
        self.online("alice", "bob", "carol")
        cid = "chat-old"
        invite = self.append_chat("alice", "chat.invite", cid, body="before sealing", metadata={"members": ["alice", "bob", "carol"], "host": "hostA"}, legacy=False)
        self.ls.call("mempalace_event_ack", {"event_id": invite["id"], "from_agent": "courier@hostA", "status": "ready", "body": "delivered to bob via tmux"})
        self.ls.call("mempalace_event_ack", {"event_id": invite["id"], "from_agent": "courier@hostA", "status": "failed", "body": "uncertain for carol"})
        self.ls.call("mempalace_event_ack", {"event_id": invite["id"], "from_agent": "bob", "status": "applied", "body": ""})
        a2a._key_path(a2a.LEGACY_IDS_NAME).unlink()
        a2a.host_key_path().unlink()
        a2a.host_key_path().write_text(base64.b64encode(os.urandom(32)).decode() + "\n")
        a2a._HOST_KEY_CACHE.update(path=None)
        a2a._LEGACY_IDS_CACHE.update(loaded=False)
        self.assertIn("recorded 1", self.run_cli("alice", "key", "migrate"))
        self.online("alice", "bob", "carol")
        with self.as_agent("alice"):
            row = a2a.ledger("alice")["chats"][0]["rows"][0]
        self.assertEqual(row["to"], {"bob": "read", "carol": "uncertain"})
        with self.as_agent("bob"):
            self.assertEqual(a2a.unread(), [], "bob's pre-sealing read still counts")
        with self.as_agent("carol"):
            self.assertEqual([e["id"] for e in a2a.unread()], [invite["id"]])
        # An unsigned acknowledgement written after the cutover is nothing.
        self.ls.call("mempalace_event_ack", {"event_id": invite["id"], "from_agent": "carol", "status": "applied", "body": ""})
        with self.as_agent("alice"):
            row = a2a.ledger("alice")["chats"][0]["rows"][0]
        self.assertEqual(row["to"], {"bob": "read", "carol": "uncertain"})
        with self.as_agent("carol"):
            self.assertEqual([e["id"] for e in a2a.unread()], [invite["id"]])

    def test_malformed_envelopes_never_stop_the_courier(self):
        """A message with an envelope of the wrong shape, a string, a list
        or a number, is inert: the courier still delivers the valid invite
        around it, and a restart from persisted state passes it again."""
        self.online("alice", "bob")
        self.run_cli("alice", "chat", "bob", "delivered around the junk")
        with self.as_agent("alice"):
            cid = a2a.open_chat_with("bob")
        for enc in ("malformed", ["x"], 7, {"eid": 5, "from": []}, {"eid": "", "from": None}):
            with self.as_agent("alice"):
                a2a.append("chat.message", a2a.ROOM_CHAT, to="bob", correlation_id=cid, body="junk", status="open", metadata={"host": "hostA", "lifecycle": "hosted", "enc": enc})
        delivered = []
        with mock.patch.object(a2a, "deliver", side_effect=lambda e, t: delivered.append(e["type"]) or "tmux"), mock.patch.object(a2a, "_age", return_value=1):
            state = a2a.courier_once({}, "hostA", verbose=False)
        self.assertEqual(delivered, ["chat.invite"])
        restarted = json.loads(json.dumps(state))
        with mock.patch.object(a2a, "deliver", side_effect=lambda e, t: delivered.append(e["type"]) or "tmux"), mock.patch.object(a2a, "_age", return_value=1):
            state = a2a.courier_once(restarted, "hostA", verbose=False)
        self.assertEqual(delivered, ["chat.invite"], "nothing pasted twice, nothing raised")
        self.assertEqual(a2a.all_chats()[cid]["messages"], 0)
        self.assertIn("delivered around the junk", self.run_cli("bob", "read", cid))

    def test_an_outsider_cannot_fix_the_hosts_of_a_recorded_chat(self):
        """A recorded alice/bob chat on hostA has no sealed pair yet.
        mallory on pinned hostC seals a message to hostA under its id: it
        is not a member's, so it fixes nothing; alice's next reply seals to
        this host alone and hostC reads none of it."""
        self.online("alice", "bob")
        cid = self.legacy_chat("alice", "bob", cid="chat-legacy-ab")
        self.assertIsNone(a2a.chat_binding(cid)[0])
        fp_a = self.run_cli("alice", "key", "show").strip()
        self.run_cli("alice", "key", "announce")
        state_c, host_c = self.other_host("hostC")
        with state_c, host_c:
            fp_c = self.run_cli("mallory", "key", "show").strip()
            self.run_cli("mallory", "key", "announce")
            self.run_cli("mallory", "key", "trust", "hostA", fp_a)
        self.run_cli("alice", "key", "trust", "hostC", fp_c)
        self.online_on("mallory", "hostC")
        with state_c, host_c:
            nacl = a2a._nacl()
            mine = a2a.require_host_key()
            box = nacl.public.Box(mine.box, nacl.public.PublicKey(a2a.box_public(a2a._public_key_for_fingerprint(fp_a, mine))))
            inner = {"v": 1, "eid": "9" * 32, "chat": cid, "type": "chat.message", "from": "mallory", "to": cid, "text": "let me in", "lifecycle": "hosted", "from_fp": fp_c, "to_fp": fp_a, "at": time.time(), "after": []}
            nonce = b"\x09" * nacl.public.Box.NONCE_SIZE
            ct = box.encrypt(json.dumps(inner, sort_keys=True).encode(), nonce).ciphertext
        with self.as_agent("mallory"):
            intrusion = a2a.append("chat.message", a2a.ROOM_CHAT, to=cid, correlation_id=cid, body=base64.b64encode(ct).decode(), status="open",
                                   metadata={"host": "hostC", "lifecycle": "hosted", "enc": {"v": 1, "alg": "nacl-box", "eid": "9" * 32, "to": fp_a, "from": fp_c, "nonce": base64.b64encode(nonce).decode()}})
        self.assertIsNone(a2a.open_body(intrusion)[1], "the box itself opens: C is pinned")
        self.assertIsNone(a2a.chat_binding(cid)[0], "an outsider fixes nothing")
        self.assertNotIn(intrusion["id"], [e["id"] for e in a2a.chat_events(cid)])
        self.assertFalse(a2a._key_path(a2a.CHAT_HOSTS_NAME).exists() and (a2a._key_path(a2a.CHAT_HOSTS_NAME) / cid).exists(), "nothing persisted")
        self.assertIn("sent to bob", self.run_cli("alice", "reply", cid, "still just us"))
        self.assertEqual([e["metadata"]["enc"]["to"] for e in self.ls.events if e["type"] == "chat.message"][-1], fp_a)
        self.assertEqual(a2a.chat_binding(cid)[0], {"hostA"})
        with state_c, host_c:
            # hostC holds the plaintext invite as history from before its
            # key, and nothing of what alice sealed afterwards.
            self.assertNotIn("still just us", [a2a.open_body(e)[0] for e in a2a.list_events(room=a2a.ROOM_CHAT, correlation_id=cid)])
            self.assertEqual([e["type"] for e in a2a.chat_events(cid)], ["chat.invite"])

    def test_a_settled_delivery_forgotten_by_the_courier_is_still_settled_by_the_relay(self):
        """The courier's memory of settled deliveries is bounded. An
        uncertain paste it has forgotten, whose box comes again under a
        new relay row listed first, is found settled on the relay by its
        identity before any paste, so nothing is pasted twice."""
        self.online("alice", "bob")
        self.run_cli("alice", "chat", "bob", "pasted once, uncertainly")
        invite = next(e for e in self.ls.events if e["type"] == "chat.invite")
        pastes = []
        with mock.patch.object(a2a, "deliver", side_effect=lambda e, t: pastes.append(e["id"]) or "uncertain"), mock.patch.object(a2a, "_age", return_value=1):
            state = a2a.courier_once({}, "hostA", verbose=False)
        self.assertEqual(len(pastes), 1)
        self.assertTrue(state["settled"])
        state["settled"] = []  # as after 4,000 later deliveries
        state["acked"] = []
        copy = self.ls._append({k: v for k, v in invite.items() if k not in ("id", "seq", "created_at")})
        self.ls.events.remove(copy)
        self.ls.events.insert(self.ls.events.index(invite), copy)
        for _ in range(2):
            with mock.patch.object(a2a, "deliver", side_effect=lambda e, t: pastes.append(e["id"]) or "tmux"), mock.patch.object(a2a, "_age", return_value=1):
                state = a2a.courier_once(state, "hostA", verbose=False)
        self.assertEqual(len(pastes), 1, "the relay's own record of the uncertain paste settles it")
        self.assertEqual(state["pending"], {})
        # The recipient's own read settles a forgotten delivery the same way.
        self.run_cli("alice", "chat", "bob", "read by bob")
        self.run_cli("bob", "inbox")
        state["settled"], state["acked"] = [], []
        with mock.patch.object(a2a, "deliver", side_effect=lambda e, t: pastes.append(e["id"]) or "tmux"), mock.patch.object(a2a, "_age", return_value=1):
            state = a2a.courier_once(state, "hostA", verbose=False)
        self.assertEqual(len(pastes), 1)

    def test_a_delivery_is_never_pasted_while_the_relay_cannot_say_whether_it_is_settled(self):
        """The courier forgot an uncertain paste; its box is listed again
        under a new row; and the relay cannot answer whether the delivery
        is settled. Nothing is pasted on a guess: the delivery waits, and
        once the relay answers it is found settled."""
        self.online("alice", "bob")
        self.run_cli("alice", "chat", "bob", "pasted once")
        invite = next(e for e in self.ls.events if e["type"] == "chat.invite")
        pastes = []
        with mock.patch.object(a2a, "deliver", side_effect=lambda e, t: pastes.append(e["id"]) or "uncertain"), mock.patch.object(a2a, "_age", return_value=1):
            state = a2a.courier_once({}, "hostA", verbose=False)
        self.assertEqual(len(pastes), 1)
        state["settled"], state["acked"] = [], []
        copy = self.ls._append({k: v for k, v in invite.items() if k not in ("id", "seq", "created_at")})
        self.ls.events.remove(copy)
        self.ls.events.insert(self.ls.events.index(invite), copy)
        real = a2a._rpc

        def acks_down(tool, args, timeout=30.0):
            if tool == "mempalace_event_list" and args.get("type") == "event.ack" and args.get("correlation_id"):
                raise a2a.LogstreamError("acks unavailable")
            return real(tool, args)

        with mock.patch.object(a2a, "_rpc", side_effect=acks_down), mock.patch.object(a2a, "deliver", side_effect=lambda e, t: pastes.append(e["id"]) or "tmux"), mock.patch.object(a2a, "_age", return_value=1):
            state = a2a.courier_once(state, "hostA", verbose=False)
        self.assertEqual(len(pastes), 1, "unknown is not unsettled")
        self.assertEqual(len(state["pending"]), 1, "the delivery waits")
        for item in state["pending"].values():
            item["next"] = 0
        with mock.patch.object(a2a, "deliver", side_effect=lambda e, t: pastes.append(e["id"]) or "tmux"), mock.patch.object(a2a, "_age", return_value=1):
            state = a2a.courier_once(state, "hostA", verbose=False)
        self.assertEqual(len(pastes), 1)
        self.assertEqual(state["pending"], {})

    def test_an_uncertain_outcome_is_kept_until_the_relay_holds_it(self):
        """Every acknowledgement attempt for an uncertain paste fails. The
        outcome stays in the courier's state, however many attempts, so a
        replay of the box after the settled cache forgot it is never
        pasted; once the relay accepts the acknowledgement, it is settled
        there and the state lets go."""
        self.online("alice", "bob")
        self.run_cli("alice", "chat", "bob", "uncertain, unacknowledged")
        invite = next(e for e in self.ls.events if e["type"] == "chat.invite")
        real = a2a._rpc

        def acks_refused(tool, args, timeout=30.0):
            if tool == "mempalace_event_ack":
                raise a2a.LogstreamError("writer busy")
            return real(tool, args)

        pastes = []
        with mock.patch.object(a2a, "_rpc", side_effect=acks_refused), mock.patch.object(a2a, "deliver", side_effect=lambda e, t: pastes.append(e["id"]) or "uncertain"), mock.patch.object(a2a, "_age", return_value=1):
            state = a2a.courier_once({}, "hostA", verbose=False)
            for _ in range(a2a.COURIER_MAX_ATTEMPTS + 3):
                for item in state["pending"].values():
                    item["next"] = 0
                state = a2a.courier_once(state, "hostA", verbose=False)
        self.assertEqual(len(pastes), 1)
        self.assertEqual([i["outcome"] for i in state["pending"].values()], ["uncertain"], "kept, not dropped")
        self.assertEqual([a for a in self.ls.events if a["type"] == "event.ack"], [])
        state["settled"], state["acked"] = [], []
        copy = self.ls._append({k: v for k, v in invite.items() if k not in ("id", "seq", "created_at")})
        self.ls.events.remove(copy)
        self.ls.events.insert(self.ls.events.index(invite), copy)
        with mock.patch.object(a2a, "_rpc", side_effect=acks_refused), mock.patch.object(a2a, "deliver", side_effect=lambda e, t: pastes.append(e["id"]) or "tmux"), mock.patch.object(a2a, "_age", return_value=1):
            for item in state["pending"].values():
                item["next"] = 0
            state = a2a.courier_once(state, "hostA", verbose=False)
        self.assertEqual(len(pastes), 1, "the kept outcome stops the replay")
        for item in state["pending"].values():
            item["next"] = 0
        with mock.patch.object(a2a, "deliver", side_effect=lambda e, t: pastes.append(e["id"]) or "tmux"), mock.patch.object(a2a, "_age", return_value=1):
            state = a2a.courier_once(state, "hostA", verbose=False)
        self.assertEqual(state["pending"], {}, "acknowledged at last, and let go")
        self.assertEqual([a2a.ack_note(a) for a in self.ls.events if a["type"] == "event.ack"], ["uncertain for bob"])
        self.assertEqual(len(pastes), 1)

    def test_a_well_sealed_box_of_the_wrong_shape_is_reported_not_folded(self):
        """far seals an invitation to alice with members=7 inside and out:
        it opens, it is far's, and it is malformed. It enters the error
        view, folds nothing, and neither the courier nor another chat is
        disturbed."""
        self.two_hosts()
        self.online("bob")
        self.run_cli("alice", "chat", "bob", "unrelated and fine")
        fp_a, fp_b = self.run_cli("alice", "key", "show").strip(), None
        with self.state_b, self.host_b:
            fp_b = self.run_cli("far", "key", "show").strip()
            nacl = a2a._nacl()
            mine = a2a.require_host_key()
            box = nacl.public.Box(mine.box, nacl.public.PublicKey(a2a.box_public(a2a._public_key_for_fingerprint(fp_a, mine))))
            eid = "a" * 32
            cid = a2a.chat_id_for(fp_b, eid)
            shapes = [{"members": 7}, {"members": ["alice", 5]}, {"members": ["ok", "not a handle!"]}, {"member": ["x"]}, {"lifecycle": {"a": 1}}, {"close": 3}]
            bad = []
            for i, extra in enumerate(shapes):
                inner = {"v": 1, "eid": eid[:-1] + str(i), "chat": a2a.chat_id_for(fp_b, eid[:-1] + str(i)), "type": "chat.invite", "from": "far", "to": "alice", "text": "wrong shape", "lifecycle": "hosted", "from_fp": fp_b, "to_fp": fp_a, "at": time.time(), "after": [], **extra}
                nonce = bytes([16 + i]) * nacl.public.Box.NONCE_SIZE
                ct = box.encrypt(json.dumps(inner, sort_keys=True).encode(), nonce).ciphertext
                bad.append((inner, ct, nonce))
        for inner, ct, nonce in bad:
            outer = {k: v for k, v in inner.items() if k in a2a.BOUND_FIELDS}
            with self.as_agent("far"):
                a2a.append("chat.invite", a2a.ROOM_CHAT, to="alice", correlation_id=inner["chat"], body=base64.b64encode(ct).decode(), status="open",
                           metadata={**outer, "host": "hostB", "enc": {"v": 1, "alg": "nacl-box", "eid": inner["eid"], "to": fp_a, "from": fp_b, "nonce": base64.b64encode(nonce).decode()}})
        chats = a2a.all_chats()
        self.assertTrue(any(st["members"] == ["alice", "bob"] for st in chats.values()))
        self.assertFalse(any(c.startswith("chat-") and chats[c]["opened_by"] == "far" for c in chats))
        out = self.run_cli("alice", "inbox", "--peek")
        self.assertIn("malformed", out)
        self.assertIn("unrelated and fine", self.run_cli("bob", "inbox", "--peek"))
        delivered = []
        with mock.patch.object(a2a, "deliver", side_effect=lambda e, t: delivered.append(e["type"]) or "tmux"), mock.patch.object(a2a, "_age", return_value=1):
            a2a.courier_once({}, "hostA", verbose=False)
        self.assertEqual(delivered, ["chat.invite"])
        with self.as_agent("alice"):
            book = a2a.ledger("alice")
        self.assertEqual(len(book["chats"]), 1)

    def test_a_released_pending_outcome_is_acknowledged_with_its_scope(self):
        """A courier state from the keys release holds outcomes without a
        scope: a delivery observed, an uncertain paste, one in flight. On
        the upgraded courier each is acknowledged under the box's identity
        read from the fold, so the ledger says delivered or uncertain and
        bob's inbox counts the delivered one as seen."""
        self.online("alice", "bob", "carol", "dave")
        cid = "chat-old"
        invite = self.append_chat("alice", "chat.invite", cid, body="from the keys release", metadata={"members": ["alice", "bob", "carol", "dave"], "host": "hostA"})
        released = {"cursor": invite["id"], "pending": {
            f"{invite['id']}|bob": {"event": invite["id"], "to": "bob", "chat": cid, "attempts": 1, "next": 0, "outcome": "delivered", "rung": "tmux"},
            f"{invite['id']}|carol": {"event": invite["id"], "to": "carol", "chat": cid, "attempts": 1, "next": 0, "outcome": "uncertain"},
            f"{invite['id']}|dave": {"event": invite["id"], "to": "dave", "chat": cid, "attempts": 1, "next": 0, "inflight": True},
        }}
        with mock.patch.object(a2a, "deliver", side_effect=AssertionError("nothing is pasted")), mock.patch.object(a2a, "_age", return_value=1):
            state = a2a.courier_once(json.loads(json.dumps(released)), "hostA", verbose=False)
        self.assertEqual(state["pending"], {})
        acks = [a for a in self.ls.events if a["type"] == "event.ack"]
        self.assertEqual(sorted(a2a.ack_note(a) for a in acks), ["delivered to bob via tmux", "uncertain for carol", "uncertain for dave"])
        self.assertTrue(all((a2a._ack_fields(a) or {}).get("scope") == a2a.scope_of(invite) for a in acks))
        with self.as_agent("alice"):
            row = a2a.ledger("alice")["chats"][0]["rows"][0]
        self.assertEqual(row["to"], {"bob": "delivered", "carol": "uncertain", "dave": "uncertain"})
        with self.as_agent("bob"):
            self.assertEqual(a2a.unread(), [], "delivered to bob, so seen")
        with self.as_agent("carol"):
            self.assertEqual([e["id"] for e in a2a.unread()], [invite["id"]])

    def test_an_old_acknowledgement_still_counts_when_its_box_comes_again_behind_the_cursor(self):
        """bob read the invite, then a later message, so the cursor passed
        the invite's acknowledgement. An identical copy of the invite then
        lists first: the old acknowledgement is matched by the box's
        identity, and the invite is not unread again."""
        self.online("alice", "bob")
        self.run_cli("alice", "chat", "bob", "first")
        invite = next(e for e in self.ls.events if e["type"] == "chat.invite")
        self.run_cli("bob", "inbox")
        with self.as_agent("alice"):
            cid = a2a.open_chat_with("bob")
        self.run_cli("alice", "reply", cid, "second")
        self.run_cli("bob", "inbox")
        with self.as_agent("bob"):
            self.assertEqual(a2a.unread(), [])
            self.assertIsNotNone(a2a.read_state(a2a.inbox_cursor_name()).get("cursor"))
        copy = self.ls._append({k: v for k, v in invite.items() if k not in ("id", "seq", "created_at")})
        self.ls.events.remove(copy)
        self.ls.events.insert(self.ls.events.index(invite), copy)
        self.assertEqual([e["id"] for e in a2a.chat_events(cid)][0], copy["id"])
        with self.as_agent("bob"):
            self.assertEqual(a2a.unread(), [], "read already, whichever row represents it")
        self.assertIn("no unread messages", self.run_cli("bob", "inbox", "--peek"))
        wakes = []
        with mock.patch.object(a2a, "wait_events", side_effect=lambda t, **f: wakes.append(1) and [] if len(wakes) else [copy]), self.as_agent("bob"):
            self.assertEqual(a2a._wait(None, 0.2), [])

    def test_mark_read_names_a_box_not_a_relay_row(self):
        """An extension read a message under one relay row and its
        acknowledgement failed; the fold then keeps an identical copy under
        another row. mark-read with the old id marks the box read, and the
        JSON inbox names each event's scope so the extension files the
        hand-off by it."""
        self.online("alice", "bob")
        self.run_cli("alice", "chat", "bob", "one box")
        invite = next(e for e in self.ls.events if e["type"] == "chat.invite")
        payload = json.loads(self.run_cli("bob", "inbox", "--peek", "--json"))
        self.assertEqual(payload[0]["scope"], a2a.scope_str(a2a.scope_of(invite)))
        copy = self.ls._append({k: v for k, v in invite.items() if k not in ("id", "seq", "created_at")})
        self.ls.events.remove(copy)
        self.ls.events.insert(self.ls.events.index(invite), copy)
        with self.as_agent("bob"):
            self.assertEqual([e["id"] for e in a2a.unread()], [copy["id"]])
        self.assertIn("1 marked read", self.run_cli("bob", "mark-read", invite["id"]))
        with self.as_agent("bob"):
            self.assertEqual(a2a.unread(), [])
        ack = next(a for a in self.ls.events if a["type"] == "event.ack" and a["from_agent"] == "bob")
        self.assertEqual(a2a.metadata_of(ack)["ack_of"], copy["id"])
        self.assertEqual((a2a._ack_fields(ack) or {}).get("scope"), a2a.scope_of(invite))
        # And on a replica that lists the original first, the same acknowledgement counts.
        self.ls.events.remove(copy)
        self.ls.events.append(copy)
        with self.as_agent("bob"):
            self.assertEqual(a2a.unread(), [])

    def test_a_missing_departure_is_never_taken_as_satisfied(self):
        """bob leaves; carol writes to the room after that; this replica
        lacks the leave. carol's message waits, past any hold, rather than
        fold against a membership that still holds bob, and bob reads
        nothing written after he left."""
        self.online("alice", "bob", "carol", "dave")
        self.run_cli("alice", "chat", "bob,carol,dave", "four of us")
        with self.as_agent("alice"):
            cid = a2a.open_chat_with("bob", "carol", "dave")
        self.run_cli("bob", "leave", cid, "off")
        self.run_cli("carol", "reply", cid, "after bob left")
        leave = next(e for e in self.ls.events if e["type"] == "chat.leave")
        message = [e for e in self.ls.events if e["type"] == "chat.message"][-1]
        self.assertEqual(a2a.all_chats()[cid]["routes"][message["id"]], ["alice", "dave"])
        hidden = self.hide(lambda e: e is not leave)
        for age in (1, a2a.HOLD_S + 1):
            with mock.patch.object(a2a, "_age", return_value=age), mock.patch.object(a2a.time, "time", return_value=time.time() + age):
                st = a2a.all_chats()[cid]
                self.assertNotIn(message["id"], st["routes"], f"waits at age {age}")
                self.assertEqual(st["messages"], 0)
                self.assertNotIn("after bob left", self.run_cli("bob", "read", cid))
                self.assertNotIn("after bob left", self.run_cli("bob", "inbox", "--peek"))
                delivered = []
                with mock.patch.object(a2a, "deliver", side_effect=lambda e, t: delivered.append((e["id"], t["handle"])) or "tmux"):
                    a2a.courier_once({}, "hostA", verbose=False)
                self.assertNotIn(message["id"], [d[0] for d in delivered])
        self.assertIn("is not here yet", a2a.unreadable_events(cid)[-1]["reason"])
        self.restore(hidden)
        st = a2a.all_chats()[cid]
        self.assertEqual(st["routes"][message["id"]], ["alice", "dave"])
        self.assertNotIn("after bob left", self.run_cli("bob", "read", cid))

    def test_a_malformed_key_announce_stops_no_other_chat(self):
        self.two_hosts()
        self.run_cli("alice", "chat", "far", "still readable")
        with self.as_agent("alice"):
            cid = a2a.open_chat_with("far")
        for meta in (
            {"host": "hostZ", "fingerprint": "bad", "public_key": [1]},
            {"host": "hostZ", "fingerprint": ["x"], "public_key": 123},
            {"host": None, "fingerprint": "aaaa-" * 5 + "aaaa", "public_key": "!!!"},
            {"host": "hostZ", "fingerprint": "a" * 200, "public_key": "A" * 4000},
        ):
            with self.as_agent("mallory"):
                a2a.append("key.announce", a2a.ROOM_KEYS, to="*", status="ready", metadata=meta)
        a2a._PUBLIC_KEYS_CACHE.update(at=0.0, keys={})
        self.assertIn(cid, a2a.all_chats())
        self.assertIn("still readable", self.run_cli("alice", "read", cid))
        self.assertNotIn("hostZ", a2a.announced_keys())
        self.assertIn("hostB", self.run_cli("alice", "key", "status"))
        with self.state_b, self.host_b:
            self.assertIn("still readable", self.run_cli("far", "read", cid))
            self.assertIn("sent to alice", self.run_cli("far", "reply", cid, "and delivered"))
        self.assertIn("and delivered", self.run_cli("alice", "read", cid))

    def test_swapping_the_envelope_direction_is_refused_on_both_hosts(self):
        """A NaCl box's shared key is the same in both directions, so the
        direction is bound inside the box; swapping enc.from and enc.to on
        an untouched invite must not open it as if the other host sent it."""
        self.two_hosts()
        self.run_cli("alice", "chat", "far", "from A to B")
        invite = next(x for x in self.ls.events if x["type"] == "chat.invite")
        cid = invite["correlation_id"]
        stored = next(x for x in self.ls.events if x["id"] == invite["id"])
        enc = stored["metadata"]["enc"]
        stored["metadata"] = dict(stored["metadata"], enc=dict(enc, **{"from": enc["to"], "to": enc["from"]}))
        text, why = a2a.open_body(stored)
        self.assertEqual(text, "")
        self.assertIn("direction", why)
        self.assertNotIn(cid, a2a.all_chats())
        with self.state_b, self.host_b:
            text, why = a2a.open_body(stored)
            self.assertEqual(text, "")
            self.assertIn("direction", why)
            self.assertNotIn(cid, a2a.all_chats())
        # Restored, it opens on both and binds alice to hostA.
        stored["metadata"] = dict(stored["metadata"], enc=enc)
        self.assertEqual(a2a.chat_binding(cid)[1], {"alice": "hostA"})
        with self.state_b, self.host_b:
            self.assertEqual(a2a.chat_binding(cid)[1], {"alice": "hostA"})

    def test_a_malformed_envelope_field_is_unreadable_and_neighbours_still_fold(self):
        self.two_hosts()
        self.run_cli("alice", "chat", "far", "first")
        invite = next(x for x in self.ls.events if x["type"] == "chat.invite")
        cid = invite["correlation_id"]
        with self.state_b, self.host_b:
            self.run_cli("far", "reply", cid, "second")
        good = [e["id"] for e in a2a.chat_events(cid)]
        self.assertEqual(len(good), 2)
        for field, value in (("from", []), ("to", {"a": 1}), ("nonce", 7), ("eid", None), ("v", "1")):
            with self.subTest(field=field):
                bad = dict(invite, id=f"evt_bad_{field}", metadata=dict(invite["metadata"], enc=dict(invite["metadata"]["enc"], **{field: value})))
                self.ls.events.append(bad)
                text, why = a2a.open_body(bad)
                self.assertEqual(text, "")
                self.assertTrue(why.startswith("envelope field") or why.startswith("unknown envelope"), why)
                self.assertEqual([e["id"] for e in a2a.chat_events(cid)], good, "the malformed event stops nothing else")
        self.assertIn("second", self.run_cli("alice", "read", cid))

    def test_a_key_made_before_sealing_keeps_its_history_after_migrate(self):
        """The keys half created host-key while bodies were plaintext. An
        upgraded host has a key and no history record; key migrate takes
        the record before any new traffic, and the old chats stay."""
        self.online("alice", "bob")
        cid = "chat-before"
        self.append_chat("alice", "chat.invite", cid, body="from before sealing", metadata={"members": ["alice", "bob"], "host": "hostA"}, legacy=False)
        self.append_chat("bob", "chat.message", cid, body="reply from before", metadata={"host": "hostA"}, legacy=False)
        # As on an upgraded host: the released X25519 key, and none of the
        # records key init writes today.
        path = a2a._key_path(a2a.LEGACY_IDS_NAME)
        if path.exists():
            path.unlink()
        a2a.host_key_path().write_text(base64.b64encode(os.urandom(32)).decode() + "\n")
        a2a._HOST_KEY_CACHE.update(path=None)
        a2a._LEGACY_IDS_CACHE.update(loaded=False)
        self.assertNotIn(cid, a2a.all_chats(), "without the record the old chat is invisible")
        out = self.run_cli("alice", "key", "migrate")
        self.assertIn("recorded 2", out)
        self.assertIn(cid, a2a.all_chats())
        self.assertIn("reply from before", self.run_cli("alice", "read", cid))
        # Idempotent, and new sealed traffic is not swept in. The key is
        # new, so presence is announced again under it.
        self.online("alice", "bob")
        self.run_cli("alice", "chat", "bob", "sealed now")
        self.assertIn("recorded 2", self.run_cli("alice", "key", "migrate"))
        with self.assertRaises(SystemExit):
            self.run_cli("alice", "key", "init")



@unittest.skipUnless(HAVE_NACL, "PyNaCl is not installed for this interpreter")
class SealedRecipientTest(A2ACase):
    """Where a body is sealed to comes from the chat's keys, and history
    capture never lies about an empty relay."""

    def two_hosts(self):
        return SealedBodyTest.two_hosts(self)

    def pin_c(self):
        state_c, host_c = self.other_host("hostC")
        with state_c, host_c:
            fp_c = self.run_cli("mallory", "key", "show").strip()
            self.run_cli("mallory", "key", "announce")
        self.run_cli("alice", "key", "trust", "hostC", fp_c)
        return fp_c

    def sealed_to(self):
        return [e["metadata"]["enc"]["to"] for e in self.ls.events if e["type"].startswith("chat.") and isinstance(e["metadata"].get("enc"), dict)]

    def test_a_reply_in_a_cross_host_chat_is_sealed_to_the_chats_host_whatever_presence_says(self):
        self.two_hosts()
        fp_c = self.pin_c()
        self.run_cli("alice", "chat", "far", "between A and B")
        with self.as_agent("alice"):
            cid = a2a.open_chat_with("far")
        with self.state_b, self.host_b:
            self.run_cli("far", "reply", cid, "noted")
        # Presence now claims far lives on the pinned hostC. The reply is
        # sealed to the chat's host, hostB, because that is what its keys
        # established; presence decides nothing for a chat that exists.
        fp_b = self.run_cli("alice", "key", "status").split("hostB")[1].split("Announced:")[1].split()[0]
        self.online_on("far", "hostC")
        self.assertIn("sent to far", self.run_cli("alice", "reply", cid, "the private answer"))
        self.assertEqual(self.sealed_to()[-1], fp_b)
        self.assertNotIn(fp_c, self.sealed_to(), "nothing was ever sealed to hostC")
        with self.state_b, self.host_b:
            self.assertIn("the private answer", self.run_cli("far", "read", cid))

    def test_a_reply_in_a_local_chat_stays_local_whatever_presence_says(self):
        self.two_hosts()
        fp_c = self.pin_c()
        self.online("bob")
        self.run_cli("alice", "chat", "bob", "just us")
        with self.as_agent("alice"):
            cid = a2a.open_chat_with("bob")
        self.online_on("bob", "hostC")
        fp_a = self.run_cli("alice", "key", "show").strip()
        self.assertIn("sent to bob", self.run_cli("alice", "reply", cid, "still just us"))
        self.assertEqual(self.sealed_to()[-1], fp_a, "a local chat stays sealed to this host")
        self.assertNotIn(fp_c, self.sealed_to())
        self.assertIn("still just us", self.run_cli("bob", "read", cid))

    def test_a_migrated_cross_host_chat_continues_sealed_on_both_hosts(self):
        """A chat from before sealing has no keys; the first sealed reply,
        whose keys are pinned, fixes its hosts, and both ends read it."""
        self.two_hosts()
        cid = "chat-legacy-ab"
        legacy = self.append_chat("far", "chat.invite", cid, body="from the plaintext days", metadata={"members": ["far", "alice"], "host": "hostB"})
        with self.state_b, self.host_b:
            self.record_legacy(legacy["id"])
            self.assertIn(cid, a2a.all_chats())
        self.assertIn(cid, a2a.all_chats())
        self.assertIsNone(a2a.chat_binding(cid)[0], "no keys yet, so no hosts yet")
        self.assertIn("sent to far", self.run_cli("alice", "reply", cid, "sealed from now on"))
        self.assertEqual(a2a.chat_binding(cid)[0], {"hostA", "hostB"})
        self.assertIn("sealed from now on", self.run_cli("alice", "read", cid))
        with self.state_b, self.host_b:
            self.assertIn("sealed from now on", self.run_cli("far", "inbox", "--peek"))
            self.assertIn("sealed from now on", self.run_cli("far", "read", cid))
            self.assertNotIn("sealed from now on", self.run_cli("far", "inbox", "--peek"))
            self.assertIn("sent to alice", self.run_cli("far", "reply", cid, "and back"))
        self.assertIn("and back", self.run_cli("alice", "read", cid))
        # Now that keys fixed the hosts, a pinned third host is still out.
        fp_c = self.pin_c()
        self.online_on("far", "hostC")
        self.assertIn("sent to far", self.run_cli("alice", "reply", cid, "where are you"))
        self.assertNotIn(fp_c, self.sealed_to())
        with self.state_b, self.host_b:
            self.assertIn("where are you", self.run_cli("far", "read", cid))

    def test_a_silent_relay_makes_no_key_and_records_no_empty_history(self):
        """The record and the key are one step: while the relay is down,
        init refuses and leaves no key, so nothing seals; once it answers,
        init takes the history that is there and the old chat is back."""
        self.online("alice", "bob")
        cid = "chat-before"
        self.append_chat("alice", "chat.invite", cid, body="from before", metadata={"members": ["alice", "bob"], "host": "hostA"}, legacy=False)
        path = a2a._key_path(a2a.LEGACY_IDS_NAME)
        path.unlink()
        a2a._key_path(a2a.HOST_KEY_NAME).unlink()
        a2a._LEGACY_IDS_CACHE.update(loaded=False)
        a2a._HOST_KEY_CACHE.update(path=None)
        with mock.patch.object(a2a, "_rpc", side_effect=a2a.LogstreamError("relay unreachable")):
            with self.assertRaises(SystemExit) as ctx:
                self.run_cli("alice", "key", "init")
        self.assertIn("no key was made", str(ctx.exception))
        self.assertFalse(a2a._key_path(a2a.HOST_KEY_NAME).exists())
        self.assertFalse(path.exists())
        with self.assertRaises(SystemExit):
            self.run_cli("alice", "chat", "bob", "nothing seals without a key")
        self.run_cli("alice", "key", "init")
        self.assertTrue(path.exists())
        self.assertIn(cid, a2a.all_chats())


class Presentation(unittest.TestCase):
    def setUp(self):
        patch = mock.patch.dict(os.environ, {"COLUMNS": "80", "A2A_NO_COURIER": "1"})
        patch.start()
        self.addCleanup(patch.stop)
        project = mock.patch.object(a2a, "current_project_scope", return_value={"project_id": "presentation", "project_name": "example"})
        project.start()
        self.addCleanup(project.stop)

    def test_help_is_complete_offline_and_adapts_to_width(self):
        for width in (40, 60, 80, 120):
            with self.subTest(width=width), mock.patch.dict(os.environ, {"COLUMNS": str(width)}):
                root = a2a.build_parser()
                pages = []
                def walk(parser, path):
                    pages.append((parser, path))
                    for action in parser._actions:
                        if isinstance(action, argparse._SubParsersAction):
                            for name, child in action.choices.items():
                                walk(child, path + [name])
                walk(root, [])
                self.assertEqual(len(pages), 30)
                for parser, path in pages:
                    self.assertTrue(parser.description, path)
                    if path:
                        self.assertIn("Example", parser.epilog)
                    for action in parser._actions:
                        if not isinstance(action, argparse._SubParsersAction):
                            self.assertTrue(action.help, (path, action.dest))
                    rendered = parser.format_help()
                    if path:
                        self.assertIn(parser.epilog, rendered, "shell examples must not be prose-wrapped")
                        examples = parser.epilog.split("\n", 1)[1]
                        syntax = subprocess.run(["sh", "-n"], input=examples, text=True, capture_output=True)
                        self.assertEqual(syntax.returncode, 0, syntax.stderr)
                    self.assertLessEqual(max(map(len, rendered.splitlines())), width, path)
                    with mock.patch.object(a2a, "courier_ensure", side_effect=AssertionError("courier")), mock.patch.object(a2a, "list_events", side_effect=AssertionError("network")), mock.patch.object(a2a, "write_state", side_effect=AssertionError("state")), redirect_stdout(io.StringIO()):
                        with self.assertRaises(SystemExit) as done:
                            a2a.main(path + ["--help"])
                        self.assertEqual(done.exception.code, 0)

    def test_real_cli_entrypoints_do_not_create_state(self):
        with tempfile.TemporaryDirectory() as root:
            env = {"PATH": os.defpath, "HOME": root, "A2A_STATE_DIR": root + "/state", "A2A_MCP_URL": "http://127.0.0.1:1/mcp", "COLUMNS": "40"}
            for args in ([], ["key"], ["--help"], ["key", "trust", "--help"]):
                result = subprocess.run([sys.executable, a2a.__file__, *args], env=env, capture_output=True, text=True, timeout=5)
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertTrue(result.stdout)
                self.assertEqual(result.stderr, "")
                self.assertEqual(list(Path(root).iterdir()), [])
            for args in (["bogus"], ["chat"], ["wait", "--timeout", "nan"], ["wait", "--timeout", "inf"], ["wait", "--timeout", "-1"], ["chat", "peer", "hello", "--wait", "nan"]):
                result = subprocess.run([sys.executable, a2a.__file__, *args], env=env, capture_output=True, text=True, timeout=5)
                self.assertEqual(result.returncode, 2, result.stderr)
                self.assertIn("--help", result.stderr)
                self.assertNotIn("Traceback", result.stderr)
                self.assertNotIn("choose from", result.stderr)
                self.assertEqual(result.stdout, "")
                self.assertEqual(list(Path(root).iterdir()), [])

    def test_human_body_preserves_layout_and_escapes_controls_only(self):
        body = "  first\n\tsecond  \n\n\x1b[2J\r last  "
        event = {"id": "event-full-id", "correlation_id": "chat-full-id", "from_agent": "peer\x1b[2J", "type": "chat.message", "created_at": "2026-10-01", "metadata": {}}
        with mock.patch.object(a2a, "open_body", return_value=(body, None)):
            output = a2a.human_event(event)
            self.assertIn("---\n  first\n\tsecond  \n\n\\x1b[2J\\r last  \n---", output)
            self.assertNotIn("event-full-id", output, "event IDs remain in JSON rather than adding a metadata row")
            self.assertIn("chat-full-id", output)
            self.assertNotIn("\x1b", output)
            self.assertIn(body.strip(), a2a.render(event), "native hook renderer retains its existing bytes")

    def test_filtered_status_is_explicit(self):
        chat = {"last": "now", "state": "closed", "left": set(), "members": ["me", "peer"]}
        with mock.patch.object(a2a, "my_chats", return_value={"chat-full-id": chat}), mock.patch.object(a2a, "me", return_value="me"), redirect_stdout(io.StringIO()) as out:
            a2a.cmd_status(type("Args", (), {"all": False})())
        self.assertIn("no active chats", out.getvalue())
        self.assertIn("a2a status --all", out.getvalue())

    def test_roster_labels_full_handles_and_json_contract(self):
        row = {"full-routing-handle-1234567890": {"display_name": "Review console", "harness": "codex", "host": "local", "project": "example", "project_id": "presentation", "project_name": "example", "state": "online", "age_s": 2}}
        with mock.patch.object(a2a, "courier_ensure"), mock.patch.object(a2a, "roster", return_value=row), mock.patch.object(a2a, "announced_keys", return_value={}), mock.patch.object(a2a, "read_trust", return_value={}), mock.patch.object(a2a, "host_key_status", return_value={"status": "pinned"}), mock.patch.object(a2a, "me", return_value="self"):
            for width in (40, 60, 80, 120):
                with mock.patch.dict(os.environ, {"COLUMNS": str(width)}), redirect_stdout(io.StringIO()) as out:
                    a2a.main(["who"])
                for text in ("full-routing-handle-1234567890", "Name:", "Review console", "Trust:", "Project:"):
                    self.assertIn(text, out.getvalue())
                self.assertLessEqual(max(map(len, out.getvalue().splitlines())), width)
            with redirect_stdout(io.StringIO()) as out:
                a2a.main(["who", "--json"])
            self.assertEqual(out.getvalue(), json.dumps(row, indent=2) + "\n")

    def test_missing_message_and_members_offer_usable_commands(self):
        with mock.patch.dict(os.environ, {"COLUMNS": "40"}), mock.patch.object(a2a, "courier_ensure"), mock.patch.object(a2a, "me", return_value="self"), mock.patch.object(a2a, "resolve_chat", return_value="chat-id"), mock.patch.object(a2a, "_send") as send, mock.patch.object(a2a.sys, "stdin", io.StringIO("")):
            for args, hint in ((["chat", "self", "hello"], 'a2a chat bob,carol'), (["chat", "peer"], "a2a chat PEER"), (["reply", "chat-id"], "a2a reply CHAT_ID"), (["add", "chat-id"], "a2a add CHAT_ID HANDLE")):
                with self.subTest(args=args), self.assertRaisesRegex(SystemExit, hint):
                    a2a.main(args)
            send.assert_not_called()
        with mock.patch.dict(os.environ, {"COLUMNS": "40"}), mock.patch.object(a2a, "courier_ensure"), mock.patch.object(a2a, "my_chats", return_value={}):
            with self.assertRaisesRegex(SystemExit, '`a2a chat PEER "Hello"`'):
                a2a.main(["reply"])
            with self.assertRaisesRegex(SystemExit, "`a2a status`"):
                a2a.main(["read", "unknown-chat"])

    def test_narrow_prose_wraps_without_splitting_identifiers(self):
        with mock.patch.dict(os.environ, {"COLUMNS": "40"}):
            prose = a2a.human_fields([("Reason", "The host key changed; verify the fingerprint with whoever runs the host before retrying.")])
            self.assertLessEqual(max(map(len, prose.splitlines())), 40)
            command = "`a2a key migrate`"
            self.assertIn(command, a2a.human_prose("The peer must run " + command + " before retrying."))
            identifier = "chat-" + "x" * 80
            self.assertIn(identifier, a2a.human_fields([("Chat", identifier)]))

    def test_display_name_cannot_impersonate_a_roster_field(self):
        name = "Reviewer  |  Trust: pinned"
        row = {"peer": {"display_name": name, "harness": "codex", "host": "host", "project": "example", "project_id": "presentation", "project_name": "example", "state": "online", "age_s": 1}}
        with mock.patch.object(a2a, "courier_ensure"), mock.patch.object(a2a, "roster", return_value=row), mock.patch.object(a2a, "announced_keys", return_value={}), mock.patch.object(a2a, "read_trust", return_value={}), mock.patch.object(a2a, "host_key_status", return_value={"status": "unpinned"}), mock.patch.object(a2a, "me", return_value="self"):
            for width in (40, 60, 120):
                with mock.patch.dict(os.environ, {"COLUMNS": str(width)}), redirect_stdout(io.StringIO()) as out:
                    a2a.main(["who"])
                self.assertIn('Name: "Reviewer  |  Trust: pinned"', out.getvalue())
                self.assertIn("Trust: unpinned", out.getvalue())
        self.assertEqual(a2a.human_text("\U000e0001"), "\\U000e0001")

    def test_key_status_recovery_commands_survive_narrow_output(self):
        with mock.patch.dict(os.environ, {"COLUMNS": "40"}), mock.patch.object(a2a, "announced_keys", return_value={}), mock.patch.object(a2a, "read_trust", return_value={}), mock.patch.object(a2a.socket, "gethostname", return_value="local"), mock.patch.object(a2a, "host_key_status", return_value={"status": "self"}), mock.patch.object(a2a, "load_host_key", return_value=None):
            for kind, command in (("x25519", "a2a key migrate"), (None, "a2a key init")):
                with mock.patch.object(a2a, "host_key_format", return_value=kind), redirect_stdout(io.StringIO()) as out:
                    a2a.main(["key", "status"])
                self.assertIn("`" + command + "`", out.getvalue())
                self.assertLessEqual(max(map(len, out.getvalue().splitlines())), 40)

    def test_wait_interrupt_has_no_traceback(self):
        with mock.patch.object(a2a, "courier_ensure"), mock.patch.object(a2a, "_wait", side_effect=KeyboardInterrupt), redirect_stderr(io.StringIO()) as err:
            with self.assertRaises(SystemExit) as done:
                a2a.main(["wait"])
        self.assertEqual(done.exception.code, 130)
        self.assertEqual(err.getvalue(), "a2a: interrupted\n")


if __name__ == "__main__":
    unittest.main()
