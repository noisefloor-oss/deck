#!/usr/bin/env python3
"""a2a — real-time agent-to-agent chat over the MemPalace Logstream.

One file, standard library only, so every harness on every host runs the
same thing. The Logstream is the wire; this is the phone.

    a2a who                      reachable agents in your Deck project
    a2a chat <peer> <text>       open or continue a chat in the selected project
    a2a inbox                    unread messages, acknowledged on read
    a2a wait [--chat ID]         block until something arrives for me
    a2a reply <chat> <text>      send into a chat you are in
    a2a end <chat> [why]         end it for everyone if you opened it; otherwise leave
    a2a courier                  per-host native delivery daemon

Identity comes from A2A_HANDLE, else from the harness session this process
runs under. Presence, chats and messages are immutable Logstream events.
Deck registers the project used for discovery and new contact; existing
chat membership still determines who can read and reply.
"""

from __future__ import annotations

import argparse
import base64
import calendar
import hashlib
import fcntl
import json
import math
import os
import re
import secrets
import socket
import shutil
import threading
import textwrap
import stat
import subprocess
import sys
import time
import urllib.error
import urllib.request
from pathlib import Path

MCP_URL = os.environ.get("A2A_MCP_URL", "http://127.0.0.1:8766/mcp")
# The relay palace may require a bearer token when its host exposes it to a
# tailnet; the same token is what the peer writes into its peers.json.
MCP_TOKEN = os.environ.get("A2A_MCP_TOKEN", "")
STREAM = os.environ.get("A2A_STREAM", "a2a")
ROOM_PRESENCE = "presence"
ROOM_CHAT = "chat"
ROOM_KEYS = "keys"
STATE_DIR = Path(os.environ.get("A2A_STATE_DIR", "") or Path.home() / ".a2a")
# The host key and the pins are per host, shared by every agent on it; they
# live beside the per-agent state unless you keep them elsewhere.
KEY_DIR = Path(os.environ.get("A2A_KEY_DIR", "") or STATE_DIR)
WAIT_MAX_MS = 300_000
PRESENCE_TTL_S = 6 * 3600
# The scan is a backstop: every a2a harness announces lifecycle presence
# from hooks or the extension, so the heartbeat only has to keep
# hook-less or crashed agents discoverable. Thirty minutes, with an
# hour before a scan reads stale, cuts scan volume six-fold. A courier
# that watches an agent vanish still publishes the changed digest at
# once; what the hour bounds is only the scan-only case of an agent
# whose pane died while its courier went down with it.
SCAN_TTL_S = 60 * 60
SCAN_REFRESH_S = 30 * 60
CODEX_OWNER_TTL_S = 10 * 60
CLOSE_TIMEOUT_S = 10 * 60
WAKE_BODY_CHARS = 400
COURIER_RETRY_S = 60
HOLD_S = 24 * 3600  # how long an event that cannot be verified yet is kept for its dependencies to arrive
COURIER_MAX_ATTEMPTS = 3
NUDGE_AFTER_S = 300  # a native delivery this old and still unread earns one reminder
NUDGE_TTL_S = 24 * 3600  # beyond this a reminder is stale, not helpful
NUDGE_MAX_ATTEMPTS = 3  # a reminder whose rung refuses is retried, then given up
HARNESS_COMMS = {
    "claude": "claude",
    "codex": "codex",
    "pi": "pi",
    "omp": "omp",
    "kimi-code": "kimi",
    "kimi": "kimi",
    "opencode": "opencode",
}
DEFAULT_TMUX_SOCKETS = ("/run/noisedeck/tmux.sock",)


# ── Logstream transport ─────────────────────────────────────────────────────


class LogstreamError(RuntimeError):
    def __init__(self, message: str, code: int | None = None):
        super().__init__(message)
        self.code = code  # the JSON-RPC error code, when the relay answered with one


class _NoRelayRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        # A relay credential authorizes one endpoint, never its redirect target.
        return None


MCP_OPENER = urllib.request.build_opener(_NoRelayRedirect())
LOCAL_MCP_OPENER = urllib.request.build_opener(urllib.request.ProxyHandler({}), _NoRelayRedirect())


def relay_opener():
    # A proxy environment must not move the local request or its bearer off-host.
    if MCP_URL == "http://127.0.0.1:8766/mcp":
        return LOCAL_MCP_OPENER
    return MCP_OPENER


def relay_token() -> str:
    # Deck sanitizes harness environments. Its unconfined, single-owner
    # deployment provisions this credential in the shared agent home instead
    # of passing a secret through env(1)'s command-line assignments.
    if MCP_TOKEN or "A2A_MCP_TOKEN" in os.environ:
        return MCP_TOKEN
    # Never send the local relay credential to a caller-selected destination.
    if MCP_URL != "http://127.0.0.1:8766/mcp":
        return ""
    path = Path.home() / ".a2a" / "relay-token"
    try:
        fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    except FileNotFoundError:
        return ""
    except OSError:
        raise LogstreamError("local relay credential cannot be opened safely") from None
    try:
        with os.fdopen(fd, "r", encoding="utf-8") as source:
            info = os.fstat(source.fileno())
            if not stat.S_ISREG(info.st_mode) or info.st_uid != os.geteuid() or info.st_mode & 0o077:
                raise LogstreamError("local relay credential must be an owner-only regular file")
            token = source.read(8193)
            if len(token) > 8192 or any(c in token for c in "\r\n\0"):
                raise LogstreamError("local relay credential is malformed")
            return token
    except (OSError, UnicodeError):
        raise LogstreamError("local relay credential cannot be read") from None


def _rpc(tool: str, arguments: dict, timeout: float = 30.0) -> dict:
    payload = json.dumps(
        {
            "jsonrpc": "2.0",
            "id": 1,
            "method": "tools/call",
            "params": {"name": tool, "arguments": arguments},
        }
    ).encode()
    headers = {"Content-Type": "application/json", "Accept": "application/json"}
    token = relay_token()
    if token:
        headers["Authorization"] = f"Bearer {token}"
    request = urllib.request.Request(MCP_URL, data=payload, headers=headers)
    try:
        with relay_opener().open(request, timeout=timeout) as response:
            document = json.loads(response.read())
    except (urllib.error.URLError, OSError, ValueError) as exc:
        raise LogstreamError(f"logstream unreachable at {MCP_URL}: {exc}") from exc
    if "error" in document:
        error = document["error"]
        raise LogstreamError(str(error), code=error.get("code") if isinstance(error, dict) else None)
    content = document.get("result", {}).get("content") or []
    text = "".join(part.get("text", "") for part in content if part.get("type") == "text")
    try:
        result = json.loads(text) if text else {}
    except ValueError:
        raise LogstreamError(f"{tool}: non-JSON reply: {text[:200]}")
    if isinstance(result, dict) and result.get("error"):
        raise LogstreamError(f"{tool}: {result['error']}")
    return result


def append(type_: str, room: str, to: str | None = None, **fields) -> dict:
    arguments = {"type": type_, "stream": STREAM, "room": room, "from_agent": me()}
    if to:
        arguments["to_agent"] = to
    arguments.update({k: v for k, v in fields.items() if v not in (None, "", {}, [])})
    result = _rpc("mempalace_event_append", arguments)
    return result.get("event", result)


PAGE = 500

# A one-shot read command folds the logstream once and exits: snapshot
# mode lists each filter set once per process, served through the
# on-disk mirror below. Long-running loops never enable it — the
# courier's and wait's whole job is seeing what arrives next.
_EVENTS_SNAPSHOT: dict | None = None
# The derived placement state serves one derivation per snapshot; see the
# derived presence state section below.
_PRESENCE_STATE_MEMO: dict = {"derived": None, "at": 0.0}


def snapshot_events() -> None:
    """List each room once until release_events; for one-shot reads."""
    global _EVENTS_SNAPSHOT
    _EVENTS_SNAPSHOT = {}
    _PRESENCE_STATE_MEMO.update(derived=None, at=0.0)


def release_events() -> None:
    """Read live again."""
    global _EVENTS_SNAPSHOT
    _EVENTS_SNAPSHOT = None
    _PRESENCE_STATE_MEMO.update(derived=None, at=0.0)


def list_events(**filters) -> list[dict]:
    """Every matching event, oldest first, paged through to the tip.

    The server returns at most 500 per call; a fold that stopped there
    would freeze presence and chats once a room outgrew one page.
    """
    cursor = "since_event_id" in filters or "since_created_at" in filters
    key = tuple(sorted(filters.items()))
    if not cursor and _EVENTS_SNAPSHOT is not None and key in _EVENTS_SNAPSHOT:
        return list(_EVENTS_SNAPSHOT[key])
    events = _read_events(dict(filters))
    if not cursor and _EVENTS_SNAPSHOT is not None:
        _EVENTS_SNAPSHOT[key] = list(events)
    return events


def _read_events(filters: dict) -> list[dict]:
    filters.setdefault("stream", STREAM)
    filters["limit"] = PAGE
    if _EVENTS_SNAPSHOT is None or "since_event_id" in filters or "since_created_at" in filters:
        # A live read: long-running loops and caller-driven cursors never
        # touch the disk cache; only one-shot reads (snapshot mode) do.
        return _paged_events(filters)
    cache = _EventsCache(filters)
    doc = cache.read_doc()
    events = doc["events"] if doc else []
    tip = (doc or {}).get("tip") or {}
    tip_id = tip.get("id") if isinstance(tip, dict) else tip
    if cache.keep_from is not None and any(str(e.get("created_at") or "") < cache.keep_from for e in events):
        # A retention-bounded mirror only shrinks when something is written;
        # with no delta this read is the one chance to drop what the
        # retention outlived. The relay tip is kept beside the events, not
        # derived from their last survivor, so trimming every retained row
        # still resumes from the tip instead of re-reading history.
        events = [e for e in events if str(e.get("created_at") or "") >= cache.keep_from]
        cache.write(events, tip=tip)
    cursor = tip_id or (events[-1]["id"] if events else None)
    try:
        delta = _paged_events(filters, since=cursor)
    except LogstreamError:
        if cursor is None:
            raise
        # The palace no longer knows the cached cursor: it was reset (or
        # this state dir moved palaces). Its rooms are re-listed whole.
        delta = None
    if delta is None:
        events = _paged_events(filters)
        cache.replace(events, tip={"id": events[-1]["id"], "seq": events[-1].get("seq")} if events else None)
    elif delta:
        events.extend(delta)
        cache.write(events, tip={"id": delta[-1]["id"], "seq": delta[-1].get("seq")})
    return events


# A fold reads a room in append order, oldest first. MemPalace 3.10.0 lists
# newest first when no cursor is given unless asked for the order, and a
# relay from 3.8.0 refuses the asking as an unknown parameter; every listing
# asks, and a relay that refused is not asked again in this process until
# it lists out of order, which is what its upgrade looks like from here.
_ORDER_REFUSED = False
_UNORDERED = "the relay listed events out of append order; nothing was read"


def _list_page(filters: dict) -> list[dict]:
    global _ORDER_REFUSED
    if not _ORDER_REFUSED:
        try:
            return _rpc("mempalace_event_list", {**filters, "order": "asc"}).get("events") or []
        except LogstreamError as exc:
            if exc.code != -32602 or "Unknown parameter 'order'" not in str(exc):
                raise
            _ORDER_REFUSED = True
    return _rpc("mempalace_event_list", filters).get("events") or []


def _paged_events(filters: dict, since: str | None = None) -> list[dict]:
    global _ORDER_REFUSED
    if since is not None:
        filters = {**filters, "since_event_id": since}
    try:
        return _pages_in_order(dict(filters))
    except LogstreamError as exc:
        if str(exc) != _UNORDERED or not _ORDER_REFUSED:
            raise
        _ORDER_REFUSED = False  # ask once more; a relay that still refuses and still misorders raises below
        return _pages_in_order(dict(filters))

def _paged_events_relaxed(filters: dict, since: str | None) -> list[dict]:
    """A listing from a caller-held cursor, surviving a reset palace: the
    relay refuses a cursor it no longer knows, and a reader's only honest
    fallback is the whole room, from which its cursor is re-derived. The
    ack trail lives on the relay too, so after a reset everything reads
    as unread — the true state of a reset world."""
    if since is None:
        return _paged_events(dict(filters))
    try:
        return _paged_events(dict(filters), since=since)
    except LogstreamError as exc:
        if not _cursor_refused(exc):
            raise
        return _paged_events(dict(filters))



def _pages_in_order(filters: dict) -> list[dict]:
    events: list[dict] = []
    last = None
    while True:
        page = _list_page(filters)
        for e in page:
            # seq is the relay's append order; a listing in any other order
            # would fold chats and presence wrongly and, mirrored, keep them so.
            seq = e.get("seq")
            if not isinstance(seq, int) or (last is not None and seq <= last):
                raise LogstreamError(_UNORDERED)
            last = seq
        events.extend(page)
        if len(page) < PAGE:
            return events
        filters["since_event_id"] = page[-1]["id"]


class _EventsCache:
    """This host's mirror of one filter set's matching events, on disk.

    Only one-shot read commands (snapshot mode) list through it; the
    courier and every caller-held cursor always read the relay live.
    The logstream is append-only and its events immutable, so a listing
    is the cached events plus whatever arrived since the cached tip, and
    since_event_id cannot skip an event however late a peer syncs it in.
    The relay refuses a cursor it does not know, so a reset palace fails
    the delta loudly and the mirror is rebuilt rather than read stale.
    """

    def __init__(self, filters: dict) -> None:
        self.key = json.dumps([MCP_URL, sorted(filters.items())])
        digest = hashlib.sha256(self.key.encode()).hexdigest()[:40]
        self.path = _state_path(f"events-cache-{digest}.json")
        # Presence alone is retention-bounded: the roster folds nothing
        # older than its horizon, so the mirror keeps a little past that
        # and drops the rest. Chat history is permanent and stays whole.
        self.keep_from = time.strftime(
            "%Y-%m-%dT%H:%M:%SZ", time.gmtime(time.time() - 2 * PRESENCE_TTL_S)
        ) if filters.get("room") == ROOM_PRESENCE and "type" not in filters else None
    def read(self) -> list[dict]:
        return self.read_doc()["events"] if self.read_doc() else []

    def read_doc(self) -> dict | None:
        try:
            doc = json.loads(self.path.read_text())
        except (OSError, ValueError):
            return None
        if not isinstance(doc, dict) or doc.get("key") != self.key or not isinstance(doc.get("events"), list):
            return None
        tip = doc.get("tip")
        if isinstance(tip, str):
            # Old mirrors stored a bare id: its event, if still retained,
            # names the sequence; a tip older than every retained row keeps
            # the oldest row's sequence, which only under-fetches nothing.
            seq = next((e.get("seq") for e in reversed(doc["events"]) if e.get("id") == tip), None)
            if seq is None and doc["events"]:
                seq = doc["events"][0].get("seq")
            doc["tip"] = {"id": tip, "seq": seq}
        elif not isinstance(tip, dict):
            doc["tip"] = {"id": doc["events"][-1]["id"] if doc["events"] else None,
                          "seq": doc["events"][-1].get("seq") if doc["events"] else None}
        return doc

    # Both writes go through the lock. write merges what is on disk now:
    # two processes listing at once each fetch their own delta, and the
    # merge keeps every fetched event whichever of them writes last.
    # Events are deduplicated by id and keep append order.
    def write(self, events: list[dict], tip: str | None = None) -> None:
        def fold(current: list[dict]) -> list[dict]:
            seen = {e.get("id") for e in current}
            kept = current + [e for e in events if e.get("id") not in seen]
            if self.keep_from is not None:
                kept = [e for e in kept if str(e.get("created_at") or "") >= self.keep_from]
            return kept

        def merge_tip(doc: dict | None):
            # The tip merges by the relay's append sequence, never by the
            # event id's timestamp: a peer's late replication appends older
            # timestamps after newer local events, and the relay resolves
            # since_event_id by row, not by clock.
            stored = (doc or {}).get("tip") or {}
            seq_of = lambda t: t.get("seq") if isinstance(t, dict) and isinstance(t.get("seq"), int) else -1
            return tip if seq_of(tip) >= seq_of(stored) else stored

        self._store(fold, merge_tip)

    # replace is the rebuild after the relay refused the cached cursor:
    # the palace holds nothing the mirror remembers, so merging would
    # resurrect events that palace no longer has.
    def replace(self, events: list[dict], tip: str | None = None) -> None:
        if self.keep_from is not None:
            events = [e for e in events if str(e.get("created_at") or "") >= self.keep_from]
        self._store(lambda current: list(events), lambda doc: tip)

    def _store(self, fold, tip_of=lambda doc: None) -> None:
        lock_path = self.path.with_suffix(".lock")
        try:
            with open(lock_path, "a") as lock:
                fcntl.flock(lock, fcntl.LOCK_EX)
                try:
                    doc = self.read_doc()
                    merged = fold((doc or {}).get("events") or [])
                    tip = tip_of(doc)
                    if tip is None:
                        tip = {"id": merged[-1]["id"], "seq": merged[-1].get("seq")} if merged else (doc or {}).get("tip")
                    temporary = self.path.with_suffix(".tmp")
                    temporary.write_text(json.dumps({"key": self.key, "tip": tip, "events": merged}))
                    temporary.chmod(0o600)
                    temporary.replace(self.path)
                finally:
                    fcntl.flock(lock, fcntl.LOCK_UN)
        except OSError:
            pass  # the cache is an accelerator; a listing never fails over it

# ── Derived presence state ──────────────────────────────────────────────────
# Placement memory is forever — a handle's host is remembered from the first
# host that vouched for it, however long ago — while the room itself only
# needs the roster's horizon. Rather than re-folding every presence event
# ever written on every cold command, the client retains, per handle and
# host, the earliest and the latest verifiable-shaped row (and per host the
# earliest and latest courier scan), and derives placement from them fresh
# each serve under the keys that exist then. A vouch is a fact about the
# keys room: rows are re-judged when keys arrive, never frozen at fold
# time, and a signature-shaped forgery simply never judges true. Because
# retention keeps the earliest row per host, arbitrary replay order and
# concurrent writers merge to the same state as one ordered whole fold.


def _cursor_refused(exc: LogstreamError) -> bool:
    """Only the relay's own refusal of this cursor means a reset palace;
    downtime or a broken endpoint is never that."""
    message = str(exc)
    return "since_event_id" in message and "not found" in message


def _retained_row(event: dict, fields: dict) -> dict | None:
    """One presence row with everything its re-judgment needs: the writer
    (a copied row under a foreign writer is a claim, never a vouch), the
    signature shape, and the signed envelope."""
    meta = metadata_of(event)
    at = fields.get("at")
    return {
        "seq": event.get("seq"),
        "writer": event.get("from_agent") or "",
        "host": str(fields.get("host") or ""),
        "kind": event["type"],
        "fields": {k: v for k, v in fields.items()},
        "sig": meta.get("sig") if isinstance(meta.get("sig"), str) else "",
        "signed_shape": (isinstance(fields.get("host"), str) and fields["host"]
                         and isinstance(meta.get("sig"), str)
                         and isinstance(at, (int, float)) and not isinstance(at, bool)
                         and isinstance(fields.get("nonce"), str)),
        "agents": ([a for a in fields.get("agents") if isinstance(a, dict)]
                   if event["type"] == "courier.scan" and isinstance(fields.get("agents"), list) else None),
    }


def _fold_presence(maps: dict, triples: list[tuple[dict, dict, bool]]) -> None:
    """Retain rows verbatim; judgment and pruning happen at serve. Rows are
    kept per named handle (and per writer, for claims), and courier scans
    per host, so whatever keys arrive the whole fold's outcome stays
    reachable from what is retained."""
    for event, fields, _vouched in triples:
        seq = event.get("seq")
        if not isinstance(seq, int):
            continue
        row = _retained_row(event, fields)
        if row is None:
            continue
        if event["type"] == "courier.scan":
            scans = maps["scans"].setdefault(row["host"], {})
            scans[str(seq)] = row
            continue
        named = fields.get("handle")
        if isinstance(named, str) and named:
            maps["rows"].setdefault(named, {})[str(seq)] = row
        who = event.get("from_agent")
        if isinstance(who, str) and who and who != named:
            maps["rows"].setdefault(who, {})[str(seq)] = row


def _judge_row(row: dict, keys_of: dict) -> bool | None:
    """True, false, or None when the host's key is not verifiable here and
    the verdict can still change. Eligibility is the original fold's: the
    writer must be the identity the signature covers, and the signature
    shape must hold; only then does the key judge it."""
    if not row["signed_shape"]:
        return False
    signed_for = f"courier@{row['host']}" if row["kind"] == "courier.scan" else row["fields"].get("handle")
    if not row["writer"] or row["writer"] != signed_for:
        return False
    host = row["host"]
    if host not in keys_of:
        try:
            keys_of[host] = verify_keys_of(host)
        except LogstreamError:
            keys_of[host] = []
    if not keys_of[host]:
        return None  # no verifiable key here (yet): the verdict can change
    envelope = {"kind": row["kind"], **row["fields"]}
    return host_vouches(host, row["sig"], envelope, keys_of[host])


def _derive_newer(map_, key, value, rank) -> None:
    current = map_.get(key)
    if current is None or rank >= current[1]:
        map_[key] = (value, rank)


def _judge_rows(maps: dict) -> list[tuple[int, dict]]:
    """All retained rows in relay order with their verdicts set: True, False,
    or None when no verifiable key for the host exists here yet. A False
    verdict is not permanent — pins are added over time and a host may hold
    several — so only True rows are ever pruning candidates."""
    keys_of: dict[str, list[bytes]] = {}
    ordered: list[tuple[int, dict]] = []
    for host, scans in maps["scans"].items():
        for _seq, row in scans.items():
            ordered.append((row["seq"], row))
    for _handle, rows in maps["rows"].items():
        for _seq, row in rows.items():
            ordered.append((row["seq"], row))
    ordered.sort(key=lambda item: item[0])
    for _seq, row in ordered:
        row["verdict"] = _judge_row(row, keys_of)
    return ordered


def _derive_presence(maps: dict) -> dict:
    """Placement from the retained rows under the keys that exist now, in
    relay order — the same result as folding the whole room at this
    moment."""
    out = {"hosts": {}, "auth": {}, "projects": {}, "sessions": {},
           "names": {}, "harness": {}, "session_handle": {}}
    ordered = _judge_rows(maps)

    def remember_project(handle: str, host: str, at: float, metadata: dict) -> None:
        if out["auth"].get(handle, ("", 0))[0] != host:
            return
        scope = _project_scope(metadata.get("project_id"), metadata.get("project_name"))
        session = metadata.get("deck_session")
        session_key = f"{host}\x1f{session}" if isinstance(session, str) and session_label_file(session) else None
        if session_key is not None:
            _derive_newer(out["sessions"], session_key, scope, at)
        _derive_newer(out["projects"], handle, (scope, session_key), at)
        if scope is not None:
            _derive_newer(out["names"], scope["project_id"], scope, at)

    for seq, row in ordered:
        verdict = row["verdict"]
        if row["kind"] == "courier.scan":
            if verdict is not True:
                continue
            fields = dict(row["fields"])
            fields["agents"] = row["agents"] or []
            for agent in _scan_agents(fields, True):
                handle = agent.get("handle")
                if isinstance(handle, str) and handle:
                    out["hosts"][handle] = agent.get("host")
                    current = out["auth"].get(handle)
                    if current is None or seq < current[1]:
                        out["auth"][handle] = (agent.get("host") or row["host"], seq)
                    remember_project(handle, row["host"], fields.get("at", 0), agent)
            continue
        named = row["fields"].get("handle")
        if isinstance(named, str) and named:
            out["hosts"][named] = row["host"]
        who = row["writer"]
        if who and who != named:
            out["hosts"][who] = row["host"]
        if not (isinstance(named, str) and named):
            continue
        session = row["fields"].get("deck_session")
        if isinstance(session, str) and session:
            _derive_newer(out["session_handle"], session, named, seq)
        if verdict is True:
            current = out["auth"].get(named)
            if current is None or seq < current[1]:
                out["auth"][named] = (row["host"], seq)
            if row["fields"].get("harness"):
                _derive_newer(out["harness"], named, row["fields"]["harness"], seq)
            remember_project(named, row["host"], row["fields"].get("at", 0), row["fields"])

    _prune_retention(maps, ordered)
    return {
        "hosts": out["hosts"],
        "auth": out["auth"],
        "harness": {k: v[0] for k, v in out["harness"].items()},
        "session_handle": {k: v[0] for k, v in out["session_handle"].items()},
        "names": {k: v[0] for k, v in out["names"].items()},
        "projects": {k: (v[0][0], v[0][1], v[1]) for k, v in out["projects"].items()},
        "sessions": dict(out["sessions"]),
    }


def _prune_retention(maps: dict, ordered: list[tuple[int, dict]]) -> None:
    """Keep exactly what any future fold could still read.

    A row whose verdict is not True keeps its whole body: no verifiable key
    for its host exists here yet, or it failed against the keys that do,
    and pins are added over time — a host may hold several — so its
    verdict can still change either way. A True row is kept only where it
    wins some exported map's fold, and each map ranks its own way:

    - claims (hosts) and session naming: the newest row of any verdict by
      relay sequence, per handle and per session value;
    - ownership (auth): the earliest True row per handle;
    - harness: the newest True row that names one, by sequence;
    - placement (projects, sessions, names): the newest True row by signed
      moment, per handle-and-host — the derive gates placement on the
      first-vouch owner's host, so each host's winner is kept and a late
      pin that changes ownership finds its rows whole;
    - scans: the newest per host by sequence, and per agent the earliest
      True vouch plus the newest by sequence and by signed moment.

    Winners are registered in the bucket the row actually lives in — its
    handle's, its writer's, or its scan host's — so a row kept for one
    map's sake is never deleted for living under another map's key.
    """
    rows_keep: dict[str, set[str]] = {}
    scans_keep: dict[str, set[str]] = {}

    def win_row(handle: str, row: dict) -> None:
        if isinstance(handle, str) and handle:
            rows_keep.setdefault(handle, set()).add(str(row["seq"]))

    def win_scan(host: str, row: dict) -> None:
        if isinstance(host, str) and host:
            scans_keep.setdefault(host, set()).add(str(row["seq"]))

    latest_claim: dict[str, dict] = {}
    latest_session_name: dict[str, tuple[dict, str]] = {}
    earliest_vouch: dict[str, dict] = {}
    latest_harness: dict[str, dict] = {}
    latest_placement: dict[tuple, dict] = {}  # (handle, host) -> row, by moment
    latest_session_placement: dict[tuple, dict] = {}  # (handle, host, session) -> row, by moment
    latest_name: dict[tuple, dict] = {}       # (project, handle, host) -> row
    latest_scan_host: dict[str, dict] = {}
    agent_first: dict[tuple, dict] = {}
    agent_newest_seq: dict[tuple, dict] = {}
    agent_newest_at: dict[tuple, dict] = {}

    def rank_of(row: dict) -> float:
        at = row["fields"].get("at")
        return at if isinstance(at, (int, float)) and not isinstance(at, bool) else float(row["seq"])

    for seq, row in ordered:
        if row["kind"] == "courier.scan":
            if row["host"] not in latest_scan_host or seq >= latest_scan_host[row["host"]]["seq"]:
                latest_scan_host[row["host"]] = row
            if row["verdict"] is not True:
                continue
            fields = dict(row["fields"])
            fields["agents"] = row["agents"] or []
            at = rank_of(row)
            for agent in _scan_agents(fields, True):
                handle = agent.get("handle")
                if not (isinstance(handle, str) and handle):
                    continue
                key = (row["host"], handle)
                if key not in agent_first:
                    agent_first[key] = row
                if key not in agent_newest_seq or seq >= agent_newest_seq[key]["seq"]:
                    agent_newest_seq[key] = row
                if key not in agent_newest_at or at >= rank_of(agent_newest_at[key]):
                    agent_newest_at[key] = row
                # The derive folds each vouched agent as a claim, a possible
                # first vouch, and placement metadata of its own.
                if handle not in latest_claim or seq >= latest_claim[handle]["seq"]:
                    latest_claim[handle] = row
                if handle not in earliest_vouch:
                    earliest_vouch[handle] = row
                hkey = (handle, row["host"])
                if hkey not in latest_placement or at >= rank_of(latest_placement[hkey]):
                    latest_placement[hkey] = row
                scope = _project_scope(agent.get("project_id"), agent.get("project_name"))
                if scope is not None:
                    nkey = (scope["project_id"], handle, row["host"])
                    if nkey not in latest_name or at >= rank_of(latest_name[nkey]):
                        latest_name[nkey] = row
                session = agent.get("deck_session")
                if isinstance(session, str) and session:
                    skey = (handle, row["host"], session)
                    if skey not in latest_session_placement or at >= rank_of(latest_session_placement[skey]):
                        latest_session_placement[skey] = row
            continue
        named = row["fields"].get("handle")
        for key in (named, row["writer"]):
            if isinstance(key, str) and key:
                if key not in latest_claim or seq >= latest_claim[key]["seq"]:
                    latest_claim[key] = row
        session = row["fields"].get("deck_session")
        if isinstance(named, str) and named and isinstance(session, str) and session:
            if session not in latest_session_name or seq >= latest_session_name[session][0]["seq"]:
                latest_session_name[session] = (row, named)
        if row["verdict"] is not True or not (isinstance(named, str) and named):
            continue
        if named not in earliest_vouch:
            earliest_vouch[named] = row
        if row["fields"].get("harness"):
            if named not in latest_harness or seq >= latest_harness[named]["seq"]:
                latest_harness[named] = row
        at = rank_of(row)
        key = (named, row["host"])
        if key not in latest_placement or at >= rank_of(latest_placement[key]):
            latest_placement[key] = row
        if isinstance(session, str) and session:
            skey = (named, row["host"], session)
            if skey not in latest_session_placement or at >= rank_of(latest_session_placement[skey]):
                latest_session_placement[skey] = row
        scope = _project_scope(row["fields"].get("project_id"), row["fields"].get("project_name"))
        if scope is not None:
            nkey = (scope["project_id"], named, row["host"])
            if nkey not in latest_name or at >= rank_of(latest_name[nkey]):
                latest_name[nkey] = row

    # Winner tables built; registration follows. A vouched scan agent
    # folded into a table registers in its scan bucket, an ordinary row in
    # its handle's and writer's.

    def register(row: dict, *handles: str) -> None:
        # A winner lives in exactly one bucket: its scan host's when it came
        # from a scan agent, its handle's and writer's otherwise. Registering
        # in the wrong bucket would delete the row it was kept for.
        if row["kind"] == "courier.scan":
            win_scan(row["host"], row)
            return
        for handle in handles:
            win_row(handle, row)
        win_row(row["writer"], row)

    for handle, row in latest_claim.items():
        register(row, handle)
    for (row, handle) in latest_session_name.values():
        register(row, handle)
    for handle, row in earliest_vouch.items():
        register(row, handle)
    for handle, row in latest_harness.items():
        register(row, handle)
    for (handle, _host), row in latest_placement.items():
        register(row, handle)
    for (handle, _host, _session), row in latest_session_placement.items():
        register(row, handle)
    for (_project, handle, _host), row in latest_name.items():
        register(row, handle)
    for host, row in latest_scan_host.items():
        win_scan(host, row)
    for table in (agent_first, agent_newest_seq, agent_newest_at):
        for (_host, _handle), row in table.items():
            win_scan(row["host"], row)

    for handle, rows in maps["rows"].items():
        keep = rows_keep.get(handle, set())
        maps["rows"][handle] = {s: row for s, row in rows.items() if row.get("verdict") is not True or s in keep}
    for host, scans in maps["scans"].items():
        keep = scans_keep.get(host, set())
        maps["scans"][host] = {s: row for s, row in scans.items() if row.get("verdict") is not True or s in keep}


def _presence_state_refresh() -> dict:
    """The derived placement, current to the relay's tip. One delta pass per
    one-shot command: within a snapshot the derivation is served, and the
    hosts cache's own brief TTL bounds the readers that follow."""
    if _PRESENCE_STATE_MEMO["derived"] is not None and _EVENTS_SNAPSHOT is not None:
        return _PRESENCE_STATE_MEMO["derived"]
    base = {"room": ROOM_PRESENCE, "stream": STREAM, "limit": PAGE}
    path = _state_path("presence-state.json")
    doc = _presence_state_read(path)
    maps: dict | None = None
    if doc is not None:
        maps, tip = doc["maps"], doc.get("tip") or {}
        if isinstance(tip.get("id"), str) and tip["id"]:
            try:
                delta = _paged_events(dict(base), since=tip["id"])
                if delta:
                    _fold_presence(maps, _presence_triples(delta))
                    maps["tip"] = {"id": delta[-1]["id"], "seq": delta[-1].get("seq")}
            except LogstreamError as exc:
                if not _cursor_refused(exc):
                    raise  # downtime is not a reset: propagate, keep the state
                maps = None  # the relay refused this tip: rebuild whole
        else:
            maps = None
    if maps is None:
        maps = {"rows": {}, "scans": {}}
        triples = _presence_triples(_paged_events(dict(base)))
        _fold_presence(maps, triples)
        _derive_presence(maps)  # judges and prunes what was just folded
        tip = {"id": triples[-1][0]["id"], "seq": triples[-1][0].get("seq")} if triples else {}
        reconciled = _presence_state_store(path, maps, tip, replace=True)
        derived = _derive_presence(reconciled)
        _PRESENCE_STATE_MEMO.update(derived=derived, at=time.perf_counter())
        return derived
    _derive_presence(maps)  # judge the delta, prune, then persist the union
    reconciled = _presence_state_store(path, maps, maps.get("tip") or tip)
    derived = _derive_presence(reconciled)
    _PRESENCE_STATE_MEMO.update(derived=derived, at=time.perf_counter())
    return derived


def _presence_state_read(path) -> dict | None:
    try:
        doc = json.loads(path.read_text())
    except (OSError, ValueError):
        return None
    if not isinstance(doc, dict) or doc.get("url") != MCP_URL or not isinstance(doc.get("maps"), dict):
        return None
    if set(doc["maps"]) - {"rows", "scans"} or not {"rows", "scans"} <= set(doc["maps"]):
        return None
    if doc.get("schema") != 2:
        return None  # an older retention format: rebuild whole
    return doc


def _presence_state_store(path, maps: dict, tip: dict, replace: bool = False) -> dict | None:
    """Persist the state under the lock. Concurrent writers union first —
    rows are immutable and keyed by relay sequence — and the union is then
    judged and pruned before it is written, so the disk state stays bounded
    however many writers race. Returns the reconciled maps so the caller's
    derivation includes rows another process had already stored."""
    lock_path = path.with_suffix(".lock")
    merged = None
    try:
        with open(lock_path, "a") as lock:
            fcntl.flock(lock, fcntl.LOCK_EX)
            try:
                doc = None if replace else _presence_state_read(path)
                if doc is not None:
                    merged = doc["maps"]
                    for section in ("rows", "scans"):
                        for key, entries in maps.get(section, {}).items():
                            target = merged[section].setdefault(key, {})
                            for seq, row in entries.items():
                                target.setdefault(seq, row)
                    stored = doc.get("tip") or {}
                    if isinstance(stored.get("seq"), int) and isinstance(tip.get("seq"), int) and stored["seq"] > tip["seq"]:
                        tip = stored
                else:
                    merged = {k: v for k, v in maps.items() if k != "tip"}
                _prune_retention(merged, _judge_rows(merged))
                persisted = {k: v for k, v in merged.items() if k != "tip"}
                temporary = path.with_suffix(".tmp")
                temporary.write_text(json.dumps({"url": MCP_URL, "schema": 2, "tip": tip, "maps": persisted}))
                temporary.chmod(0o600)
                temporary.replace(path)
            finally:
                fcntl.flock(lock, fcntl.LOCK_UN)
    except OSError:
        pass  # the state is an accelerator; placement never fails over it
    return merged if merged is not None else {k: v for k, v in maps.items() if k != "tip"}


def wait_events(timeout_ms: int, **filters) -> list[dict]:
    filters.setdefault("stream", STREAM)
    filters["timeout_ms"] = min(int(timeout_ms), WAIT_MAX_MS)
    return _rpc("mempalace_event_wait", filters, timeout=filters["timeout_ms"] / 1000 + 15).get("events") or []


def ack(e: dict, status: str = "applied", body: str = "") -> None:
    _rpc("mempalace_event_ack", {"event_id": e["id"], "from_agent": me(), "status": status, "body": ack_payload(e["id"], me(), status, note=body, scope=scope_of(e))})


def scope_of(e: dict) -> dict:
    """An event's authenticated identity, as every replica names it: its
    chat, the key that sealed it and its box's id; for recorded plaintext
    history, the chat and the relay id. A copy of a box under another
    relay id has the same scope, and a box under the same id in another
    chat or from another key does not."""
    enc = metadata_of(e).get("enc")
    enc = enc if isinstance(enc, dict) else {}  # a malformed envelope names nothing; the event is scoped by its relay id
    sealed_by, eid = enc.get("from"), enc.get("eid")
    return {"chat": e.get("correlation_id") or "", "sealed_by": sealed_by if isinstance(sealed_by, str) else None, "key": eid if isinstance(eid, str) and eid else e["id"]}


def scope_key(scope) -> tuple | None:
    if not isinstance(scope, dict):
        return None
    chat, sealed_by, key = scope.get("chat"), scope.get("sealed_by"), scope.get("key")
    if not (isinstance(chat, str) and isinstance(key, str) and (sealed_by is None or isinstance(sealed_by, str))):
        return None
    return (chat, sealed_by, key)


def scope_str(scope: dict | None) -> str:
    """The scope as one string, the key a courier files a delivery under,
    so a delivery is one delivery whichever relay row represents its box."""
    scope = scope or {}
    return f"{scope.get('chat') or ''}|{scope.get('sealed_by') or ''}|{scope.get('key') or ''}"


def scope_index(events) -> dict[tuple, str]:
    """scope -> the relay id this replica kept for it, over verified events."""
    return {(e.get("correlation_id") or "", e.get("sealed_by"), e["key"]): e["id"] for e in events if e.get("type") in CHAT_TYPES and "key" in e}


ACK_VERSION = 1


def ack_payload(ack_of: str, writer: str, status: str, recipient: str | None = None, note: str = "", scope: dict | None = None) -> str:
    """An acknowledgement's body: what it says, and this host's signature
    binding that to the event it acknowledges, by its authenticated key
    as well as the relay id it was read under, its writer and its status.
    An acknowledgement moves a reader's cursor and stops a courier, so one
    nobody's key vouches for moves nothing, and a replica that kept another
    copy of the same box still knows which event was acknowledged."""
    host = socket.gethostname()
    fields = {"kind": "event.ack", "ack_of": ack_of, "from": writer, "status": status, "host": host, "recipient": recipient, "note": note, "scope": scope}
    return json.dumps({"v": ACK_VERSION, "host": host, "recipient": recipient, "note": note, "scope": scope, "sig": sign_fields(fields)}, sort_keys=True)


def ack_target(a: dict, index: dict[tuple, str] | None = None) -> str:
    """The relay id, in this replica, of the event an acknowledgement is
    for: the one this replica kept for the signed scope. An
    acknowledgement whose scope this replica has not verified names
    nothing, and one that carries no scope names nothing either."""
    if a.get("id") in legacy_ack_ids():
        target = metadata_of(a).get("ack_of") or ""
        return target if index and (a.get("correlation_id") or "", None, target) in index else ""
    payload = _ack_fields(a) or {}
    scope = scope_key(payload.get("scope"))
    if scope is None or not index:
        return ""
    return index.get(scope, "")


def _ack_fields(a: dict) -> dict | None:
    body = a.get("body")
    if not isinstance(body, str) or not body.startswith("{"):
        return None
    try:
        payload = json.loads(body)
    except ValueError:
        return None
    if not isinstance(payload, dict) or payload.get("v") != ACK_VERSION:
        return None
    return payload


def ack_note(a: dict) -> str:
    """What an acknowledgement says, for a reader."""
    payload = _ack_fields(a)
    note = payload.get("note") if payload else a.get("body")
    return note if isinstance(note, str) else ""


def ack_vouched(a: dict, handle_host: dict[str, str] | None = None) -> bool:
    """Whether an acknowledgement is the word of the host its writer is on
    and within that host's authority: a courier's host is in its name and
    must be the named recipient's host; a handle's host is the one the
    chat's keys bound it to or the one its own host vouched for in
    presence; the signature must be that host's."""
    if a.get("id") in legacy_ack_ids():
        return True  # recorded with the history it acknowledges
    payload = _ack_fields(a)
    if payload is None:
        return False
    writer = a.get("from_agent") or ""
    if writer.startswith("courier@"):
        host = writer[len("courier@"):]
        # A courier speaks for deliveries on its own host only: the
        # recipient it names must live there, by the chat's binding or by
        # the recipient's own host's word. A pinned host's courier cannot
        # declare another host's agent served.
        recipient = payload.get("recipient")
        if not isinstance(recipient, str) or not recipient:
            return False
        placed = (handle_host or {}).get(recipient) or authenticated_hosts().get(recipient)
        if placed != host:
            return False
    elif writer == me():
        host = socket.gethostname()
    else:
        host = (handle_host or {}).get(writer) or authenticated_hosts().get(writer)
    if not host or payload.get("host") != host:
        return False
    fields = {"kind": "event.ack", "ack_of": metadata_of(a).get("ack_of"), "from": writer, "status": a.get("status"), "host": host, "recipient": payload.get("recipient"), "note": payload.get("note"), "scope": payload.get("scope")}
    return host_vouches(host, payload.get("sig"), fields)


def verified_acks(events: list[dict], bindings: dict[str, dict[str, str]] | None = None) -> list[dict]:
    """The acknowledgements among `events` that a host's key vouches for.
    `bindings` is chat id -> handle -> host from verified_chats; without
    it the chats are folded here."""
    acks = [a for a in events if a["type"] == "event.ack"]
    if not acks:
        return []
    if bindings is None:
        bindings = verified_chats()[2]
    return [a for a in acks if ack_vouched(a, bindings.get(a.get("correlation_id") or ""))]


def metadata_of(event: dict) -> dict:
    raw = event.get("metadata")
    if isinstance(raw, dict):
        return raw
    try:
        return json.loads(raw) if raw else {}
    except ValueError:
        return {}


# ── Local state ─────────────────────────────────────────────────────────────


def _state_path(name: str) -> Path:
    STATE_DIR.mkdir(parents=True, exist_ok=True, mode=0o700)
    return STATE_DIR / name


def read_state(name: str) -> dict:
    path = _state_path(name)
    try:
        return json.loads(path.read_text())
    except (OSError, ValueError):
        return {}


def write_state(name: str, value: dict) -> None:
    path = _state_path(name)
    temporary = path.with_suffix(".tmp")
    temporary.write_text(json.dumps(value, indent=2))
    temporary.chmod(0o600)
    temporary.replace(path)


def session_label_file(session: str) -> str | None:
    if not isinstance(session, str) or not re.fullmatch(r"[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12}", session):
        return None
    return f"session-label-{session}.json"


def session_label(session: str) -> str:
    filename = session_label_file(session)
    if filename is None:
        return ""
    value = read_state(filename)
    name = value.get("display_name", "") if isinstance(value, dict) else ""
    return name if isinstance(name, str) and len(name.encode("utf-8")) <= 100 else ""


def _valid_project_id(value) -> bool:
    return isinstance(value, str) and re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9._-]{0,127}", value) is not None


def _valid_project_name(value) -> bool:
    return isinstance(value, str) and bool(value)


def session_project(session: str) -> dict | None:
    """The committed Deck project registered beside a session's label: its id
    and the name the project carried when Deck committed the registration.
    None when the record is missing, partial, or invalid — a session without
    usable metadata has no project scope, whatever directory it runs in."""
    filename = session_label_file(session)
    if filename is None:
        return None
    value = read_state(filename)
    if not isinstance(value, dict):
        return None
    project_id, name = value.get("project_id"), value.get("project_name")
    if not _valid_project_id(project_id) or not _valid_project_name(name):
        return None
    return {"project_id": project_id, "project_name": name}


def local_session_scope(session: str, env_project_id: str = "") -> dict | None:
    """Read the session's registered project and check the launch ID agrees."""
    record = session_project(session)
    if record is None:
        return None
    if env_project_id and env_project_id != record["project_id"]:
        return None  # the environment and the committed record disagree
    return record


def current_project_scope() -> dict | None:
    """This session's committed Deck project: the default for discovery and
    new contact, independent of the cwd. None refuses rather than guesses."""
    return local_session_scope(os.environ.get("NOISEDECK_SESSION_ID", ""), os.environ.get("NOISEDECK_PROJECT_ID", ""))


def cmd_session_label(args) -> None:
    """Register Deck-owned display metadata locally, without announcing presence."""
    filename = session_label_file(args.session)
    if filename is None:
        raise SystemExit("a2a: session must be a canonical Deck UUID")
    if len(args.name.encode("utf-8")) > 100:
        raise SystemExit("a2a: display name exceeds 100 bytes")
    project_id = args.project_id
    project_name = args.project_name
    if (project_id is None) != (project_name is None):
        raise SystemExit("a2a: --project-id and --project-name arrive together")
    record = {"display_name": args.name}
    if project_id is not None:
        if not _valid_project_id(project_id):
            raise SystemExit("a2a: project id is not a Deck project id")
        if not _valid_project_name(project_name):
            raise SystemExit("a2a: project name is empty")
        record["project_id"] = project_id
        record["project_name"] = project_name
    else:
        # A caller that predates project metadata re-registers the label
        # alone; the scope an earlier registration committed stands.
        previous = read_state(filename)
        if isinstance(previous, dict) and _valid_project_id(previous.get("project_id")) and _valid_project_name(previous.get("project_name")):
            record["project_id"] = previous["project_id"]
            record["project_name"] = previous["project_name"]
    write_state(filename, record)


# ── Identity ────────────────────────────────────────────────────────────────


def _ancestry() -> list[tuple[int, str]]:
    chain = []
    pid = os.getpid()
    while pid > 1:
        try:
            comm = Path(f"/proc/{pid}/comm").read_text().strip()
            ppid = int(re.search(r"PPid:\s+(\d+)", Path(f"/proc/{pid}/status").read_text()).group(1))
        except (OSError, AttributeError, ValueError):
            break
        chain.append((pid, comm))
        pid = ppid
    return chain


def harness_of_pid(pid: int) -> str | None:
    try:
        comm = Path(f"/proc/{pid}/comm").read_text().strip()
        argv = Path(f"/proc/{pid}/cmdline").read_bytes().split(b"\0")
    except OSError:
        return None
    if comm in HARNESS_COMMS:
        return HARNESS_COMMS[comm]
    for arg in argv[:3]:
        name = os.path.basename(arg.decode(errors="replace"))
        if name in HARNESS_COMMS:
            return HARNESS_COMMS[name]
    return None


def harness_process() -> tuple[int, str] | None:
    """The nearest ancestor that is a known harness: (pid, harness name)."""
    for pid, _ in _ancestry():
        harness = harness_of_pid(pid)
        if harness:
            return pid, harness
    return None


def session_key() -> str:
    """Stable key for this harness session, used to file its identity."""
    for name in ("A2A_SESSION", "NOISEDECK_SESSION_ID", "CLAUDE_SESSION_ID"):
        if os.environ.get(name):
            return re.sub(r"[^A-Za-z0-9_-]", "", os.environ[name])
    found = harness_process()
    if found:
        pid, _ = found
        try:
            start = Path(f"/proc/{pid}/stat").read_text().split()[21]
        except (OSError, IndexError):
            start = "0"
        return f"pid{pid}-{start}"
    return "shell"


def default_handle() -> str:
    """<harness>-<tag>, computed identically here and by the courier's pane scan.

    The tag is the first four characters of the Deck session id when there is
    one, else the harness pid: both are visible from inside the session and
    from the outside, so an agent that never ran a hook still has the same
    name in `a2a who` as in `a2a whoami`.
    """
    found = harness_process()
    harness = found[1] if found else "agent"
    return f"{harness}-{handle_tag(os.environ.get('NOISEDECK_SESSION_ID', ''), found[0] if found else os.getpid())}"


def handle_tag(deck_session: str, pid: int) -> str:
    return deck_session[:4].lower() if deck_session else str(pid)


def me() -> str:
    if os.environ.get("A2A_HANDLE"):
        return os.environ["A2A_HANDLE"].strip()
    identity = read_state(f"identity-{session_key()}.json")
    if identity.get("handle"):
        return identity["handle"]
    # The default is never persisted: it is recomputed the same way by the
    # courier's pane scan, so the two can never drift apart.
    return default_handle()


def set_handle(handle: str) -> None:
    if not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9._-]{0,63}", handle):
        raise SystemExit("handle: letters, digits, '.', '_', '-' only, max 64")
    write_state(f"identity-{session_key()}.json", {"handle": handle})


# ── Presence ────────────────────────────────────────────────────────────────


def tmux_sockets() -> list[str]:
    """An explicit A2A_TMUX_SOCKETS list replaces the defaults, so a test
    or a user can point the courier at one server and nowhere else."""
    explicit = [s for s in os.environ.get("A2A_TMUX_SOCKETS", "").split(os.pathsep) if s]
    return explicit or list(DEFAULT_TMUX_SOCKETS)


def _pane_for(pid: int) -> dict:
    """Locate the tmux pane whose process tree contains pid.

    Deck strips TMUX from harness environments, so the pane is found from
    the outside: every known socket, every pane, match pane_pid against the
    ancestry of the harness process.
    """
    ancestors = {p for p, _ in _ancestry()} | {pid}
    sockets = tmux_sockets()
    tmux_env = os.environ.get("TMUX", "")
    if tmux_env and not os.environ.get("A2A_TMUX_SOCKETS"):
        sockets.insert(0, tmux_env.split(",")[0])
    for socket_path in sockets:
        if not os.path.exists(socket_path):
            continue
        try:
            output = subprocess.run(
                ["tmux", "-S", socket_path, "list-panes", "-a", "-F", "#{pane_id} #{pane_pid} #{session_name}"],
                capture_output=True,
                text=True,
                timeout=5,
                check=False,
            ).stdout
        except (OSError, subprocess.SubprocessError):
            continue
        for line in output.splitlines():
            parts = line.split(" ", 2)
            if len(parts) == 3 and parts[1].isdigit() and int(parts[1]) in ancestors:
                return {"socket": socket_path, "pane": parts[0], "session": parts[2]}
    return {}


def presence_metadata(harness: str | None = None, native_session: str | None = None, cwd: str | None = None, owner: dict | None = None, inbox_socket: str | None = None) -> dict:
    if owner is not None:
        fields = ("handle", "harness", "host", "pid", "project", "project_id", "project_name", "deck_session", "display_name", "pane")
        return {**{key: owner[key] for key in fields}, "native_session": native_session or ""}
    found = harness_process()
    pid = found[0] if found else os.getpid()
    scope = current_project_scope()
    return {
        "handle": me(),
        "harness": harness or (found[1] if found else "unknown"),
        "host": socket.gethostname(),
        "pid": pid,
        "project": scope["project_name"] if scope else "",
        "project_id": scope["project_id"] if scope else "",
        "project_name": scope["project_name"] if scope else "",
        "native_session": native_session or "",
        # Claude Code exports its inbox socket to every hook's environment;
        # without it the courier cannot reach an idle Claude session at all.
        "inbox_socket": inbox_socket or "",
        "deck_session": os.environ.get("NOISEDECK_SESSION_ID", ""),
        "display_name": session_label(os.environ.get("NOISEDECK_SESSION_ID", "")),
        "pane": _pane_for(pid),
    }


def signed_presence(kind: str, metadata: dict) -> dict:
    """The metadata with this host's signature over it and the event
    kind, so a reader can tell the host's own word from a claim."""
    fields = {k: v for k, v in metadata.items() if k not in ("sig", "at", "nonce")}
    # The moment and a nonce are signed too, so a copy of an older
    # announcement replayed under a new relay event is older, not newer.
    fields["at"] = time.time()
    fields["nonce"] = secrets.token_hex(8)
    out = dict(fields)
    out["sig"] = sign_fields({"kind": kind, **fields})
    return out


def announce(state: str, **extra) -> dict:
    """state: online | busy | idle | offline."""
    kind = f"agent.{state}"
    metadata = presence_metadata(**extra)
    return append(kind, ROOM_PRESENCE, to="*", from_agent=metadata["handle"], status={"offline": "superseded"}.get(state, "ready"), metadata=signed_presence(kind, metadata))


def _seconds_between(earlier: str | None, later: str | None) -> int:
    try:
        a = calendar.timegm(time.strptime(earlier or "", "%Y-%m-%dT%H:%M:%SZ"))
        b = calendar.timegm(time.strptime(later or "", "%Y-%m-%dT%H:%M:%SZ"))
    except ValueError:
        return 0
    return int(b - a)


def _age(event: dict) -> int:
    try:
        seen = calendar.timegm(time.strptime(event["created_at"], "%Y-%m-%dT%H:%M:%SZ"))
    except (KeyError, ValueError):
        return 10**9
    return int(time.time() - seen)


def presence_events(**filters) -> list[tuple[dict, dict, bool]]:
    """Presence, oldest first, as (event, fields without the signature,
    vouched): vouched when the host the fields name signed them with its
    own key, this host's or one pinned here, and the event's writer is
    the identity the signature covers: the signed handle for an agent's
    presence, the host's courier for a scan; and the fields carry the
    signed moment they were written. Signed fields copied under another
    writer's name are a claim like any other. Every consumer of
    presence reads through this, so a claim anyone can write never
    supplies a host, a pane, a socket or a native session to anything
    that acts."""
    return _presence_triples(list_events(room=ROOM_PRESENCE, **filters))


def _presence_triples(events: list[dict]) -> list[tuple[dict, dict, bool]]:
    out = []
    keys_of: dict[str, list[bytes]] = {}
    for event in events:
        meta = metadata_of(event)
        fields = {k: v for k, v in meta.items() if k != "sig"}
        host = fields.get("host")
        vouched = False
        writer = event.get("from_agent") or ""
        signed_for = f"courier@{host}" if event["type"] == "courier.scan" else fields.get("handle")
        if isinstance(host, str) and host and isinstance(meta.get("sig"), str) and writer and writer == signed_for and isinstance(fields.get("at"), (int, float)) and not isinstance(fields.get("at"), bool) and isinstance(fields.get("nonce"), str):
            if host not in keys_of:
                keys_of[host] = verify_keys_of(host)
            vouched = host_vouches(host, meta["sig"], {"kind": event["type"], **fields}, keys_of[host])
        out.append((event, fields, vouched))
    return out


def _scan_agents(fields: dict, vouched: bool) -> list[dict]:
    """The agents a courier scan names, each on the scan's own host."""
    host = fields.get("host")
    agents = fields.get("agents")
    if not isinstance(host, str) or not host or not isinstance(agents, list):
        return []  # a malformed scan from anyone names nobody and stops nothing
    return [a for a in agents if isinstance(a, dict) and isinstance(a.get("handle"), str) and a.get("handle") and isinstance(a.get("host"), str) and (a["host"] == host if vouched else a["host"])]


def roster(max_age_s: int = PRESENCE_TTL_S) -> dict[str, dict]:
    """Who is reachable: the latest courier scan per host, refined by each
    agent's own presence events (state, native session). Newest wins
    among the rows a host's key vouched for. A claim nobody vouched for
    is listed as `unverified`, with its host and nothing else: no pane,
    socket or native session, so no courier can act on it, and it never
    replaces a vouched row for the same handle."""
    # Vouched presence and claims are collected apart, so a claim written
    # after a host's genuine presence neither replaces nor hides it.
    latest: dict[str, tuple[dict, dict]] = {}
    scans: dict[str, tuple[dict, dict]] = {}
    claimed: dict[str, tuple[dict, dict]] = {}
    claimed_scans: list[tuple[dict, dict]] = []
    horizon = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime(time.time() - max_age_s))
    now = time.time()
    placed = authenticated_hosts()
    seen_signed: set[tuple[str, float, str]] = set()

    def first_copy(key: str, fields: dict) -> bool:
        # A signed announcement is one event: its moment and nonce name
        # it, and a copy under another relay id is not a second one.
        identity = (key, fields["at"], fields["nonce"])
        if identity in seen_signed:
            return False
        seen_signed.add(identity)
        return True

    def newer(current, fields):
        # Newest by the signed moment, never by relay order: an older
        # announcement replayed later stays older. Two distinct
        # announcements in the same moment keep their relay order.
        return current is None or fields["at"] >= current[1]["at"]

    def vouched_age(event, fields):
        return max(_age(event), int(now - fields["at"]))

    for event, fields, vouched in presence_events():
        # The horizon is a local slice, not a server-side cursor: a
        # since_created_at filter marks the listing caller-driven, which
        # bypasses the disk mirror and re-pages the whole presence room on
        # every roster fold — the single largest read cost in every
        # one-shot command. The room itself is mirrored incrementally.
        if str(event.get("created_at") or "") < horizon:
            continue
        who = event.get("from_agent") or ""
        if event["type"] == "courier.scan":
            if vouched:
                if first_copy("scan:" + fields["host"], fields) and newer(scans.get(fields["host"]), fields):
                    scans[fields["host"]] = (event, fields)
            else:
                claimed_scans.append((event, fields))
        elif vouched:
            # A handle's presence is its own host's word, the first host
            # whose key vouched for it; another pinned host saying where
            # that handle is places nothing.
            handle = fields["handle"]
            if placed.get(handle) == fields["host"] and first_copy(handle, fields) and newer(latest.get(handle), fields):
                latest[handle] = (event, fields)
        elif who:
            claimed[who] = (event, fields)
    result: dict[str, dict] = {}
    scan_labels: dict[tuple[str, str], tuple[float, str]] = {}
    scan_projects: dict[tuple[str, str], tuple[float, str, str]] = {}
    for scan, fields in scans.values():
        age = vouched_age(scan, fields)
        if age > SCAN_TTL_S:
            continue
        for agent in _scan_agents(fields, True):
            if placed.get(agent["handle"]) != fields["host"]:
                continue  # a scan names the agents on its host, not another host's
            result[agent["handle"]] = dict(agent, state="online", seen=scan["created_at"], age_s=age, via="courier")
            session = agent.get("deck_session")
            if isinstance(session, str) and session and "display_name" in agent:
                scan_labels[(fields["host"], session)] = (fields["at"], agent["display_name"])
            if isinstance(session, str) and session:
                scan_projects[(fields["host"], session)] = (fields["at"], agent.get("project_id") or "", agent.get("project_name") or "")
    for handle, (event, fields) in latest.items():
        age = vouched_age(event, fields)
        if age > max_age_s:
            continue
        if event["type"] == "agent.offline":
            result.pop(handle, None)
            continue
        meta = {k: v for k, v in fields.items() if k not in ("at", "nonce")}
        meta.update(state=event["type"].split(".", 1)[1], seen=event["created_at"], age_s=age, via="hook")
        scanned = result.get(handle, {})
        if scanned and not meta.get("pane") and scanned.get("host") == meta.get("host"):
            meta["pane"] = scanned.get("pane")
        session = meta.get("deck_session")
        label = scan_labels.get((meta.get("host"), session)) if isinstance(session, str) else None
        if label is not None and label[0] > fields["at"]:
            # Older lifecycle hooks must not undo a rename or a cleared name.
            meta["display_name"] = label[1]
        project = scan_projects.get((meta.get("host"), session)) if isinstance(session, str) else None
        if project is not None and project[0] > fields["at"]:
            # A newer scan also refreshes or clears the session's project.
            meta["project_id"], meta["project_name"] = project[1], project[2]
            meta["project"] = project[2] if project[1] and project[2] else ""
        result[handle] = meta
    for scan, fields in claimed_scans:
        if _age(scan) > SCAN_TTL_S:
            continue
        for agent in _scan_agents(fields, False):
            result.setdefault(agent["handle"], {"handle": agent["handle"], "host": agent.get("host"), "harness": agent.get("harness"), "state": "unverified", "seen": scan["created_at"], "age_s": _age(scan), "via": "courier"})
    for handle, (event, fields) in claimed.items():
        if _age(event) > max_age_s or event["type"] == "agent.offline":
            continue
        result.setdefault(handle, {"handle": handle, "host": fields.get("host"), "harness": fields.get("harness"), "state": "unverified", "seen": event["created_at"], "age_s": _age(event), "via": "hook"})
    return result


# ── Project selection ──────────────────────────────────────────────────────
# A convenience guard for new contact, not a project-isolation boundary.


def _project_scope(project_id, project_name) -> dict | None:
    if not _valid_project_id(project_id) or not _valid_project_name(project_name):
        return None
    return {"project_id": project_id, "project_name": project_name}


def remembered_project_placements() -> dict[str, dict]:
    """Each handle's latest signed project, including offline peers."""
    _refresh_hosts(2.0)
    return _HOSTS_CACHE["projects"]


def resolve_project_scope(selector: str | None = None) -> dict:
    current = current_project_scope()
    if selector is None:
        if current is None:
            raise SystemExit("This session has no Deck project information. Use `a2a who --project NAME` for a known project, or ask whoever runs this Deck to update it and restart its backend.")
        return current
    if not selector or selector == "*":
        raise SystemExit("Name one exact project with `--project NAME`, or use its exact project ID.")
    _refresh_hosts(2.0)
    projects = dict(_HOSTS_CACHE["project_names"])
    if current is not None:
        projects[current["project_id"]] = current
    if selector in projects:
        return projects[selector]
    matches = [scope for scope in projects.values() if scope["project_name"] == selector]
    if not matches:
        raise SystemExit("No known project matches that name or ID. Ask whoever runs this Deck for the exact project name or ID.")
    if len(matches) != 1:
        raise SystemExit("That name matches more than one project. Use `--project ID` with the exact project ID.")
    return matches[0]


def scoped_roster(rows: dict, scope: dict) -> dict:
    return {handle: {**row, "project": scope["project_name"], "project_name": scope["project_name"]}
            for handle, row in rows.items()
            if row.get("state") != "unverified"
            and (_project_scope(row.get("project_id"), row.get("project_name")) or {}).get("project_id") == scope["project_id"]}


def require_project_recipients(scope: dict, handles: list[str], rows: dict, force: bool = False) -> None:
    placements = None
    for handle in handles:
        row = rows.get(handle)
        other = None
        if row is not None and row.get("state") != "unverified":
            other = _project_scope(row.get("project_id"), row.get("project_name"))
        elif force or row is not None:
            # Unsigned claims cannot replace the host's remembered placement.
            if placements is None:
                placements = remembered_project_placements()
            other = placements.get(handle)
        if other is None or other["project_id"] != scope["project_id"]:
            raise SystemExit("A recipient is outside the selected project or has no Deck project information. Check `a2a who`, or use `--project NAME` for a known project. Nothing was sent.")


# ── Host keys ───────────────────────────────────────────────────────────────
#
# One Ed25519 key per host: it signs what the host says in the clear
# (presence, acknowledgements), and the X25519 key derived from it seals
# bodies. Every agent on a host shares one Unix user and one provider
# home, so a per-agent key would be bookkeeping, not identity. The public
# half is announced in the relay stream; the two operators read the
# fingerprints to each other once and pin them. Sending to a host with
# no pin, or whose announced key differs from the pin, is refused with the
# reason. There is no flag, environment value or prompt that sends anyway.

HOST_KEY_NAME = "host-key"
HOST_KEY_ALG = "ed25519"
RETIRED_KEY_NAME = "host-key.x25519"  # the pre-sealing key, kept beside the new one after migrate
TRUST_NAME = "trust.json"
CHAT_HOSTS_NAME = "chat-hosts"  # a directory: one immutable record per chat of the two hosts its keys fixed, kept once seen
LEGACY_IDS_NAME = "legacy-ids"
LEGACY_ACKS_NAME = "legacy-acks"  # the acknowledgements of that history, recorded with it: delivered, read and uncertain from before sealing


def _nacl():
    """PyNaCl, or a refusal. The client is otherwise stdlib-only; this is
    the one dependency, and without it sealed chat refuses rather than
    falling back to plaintext."""
    try:
        import nacl.public  # noqa: F401
        import nacl.signing  # noqa: F401
        return nacl
    except ImportError as exc:
        raise SystemExit("a2a: PyNaCl is not installed for this interpreter; key operations and sealed chat refuse (install python3-nacl)") from exc


_KEY_DIRS_MADE: set[str] = set()


def _key_path(name: str) -> Path:
    if str(KEY_DIR) not in _KEY_DIRS_MADE or not KEY_DIR.is_dir():
        KEY_DIR.mkdir(parents=True, exist_ok=True, mode=0o700)
        _KEY_DIRS_MADE.add(str(KEY_DIR))
    return KEY_DIR / name


def host_key_path() -> Path:
    return _key_path(HOST_KEY_NAME)


def fingerprint_of(public_key_bytes: bytes) -> str:
    """Short, readable, and meant to be read aloud: 24 base32 characters in
    groups of four, from the SHA-256 of the raw public key."""
    digest = hashlib.sha256(public_key_bytes).digest()
    text = base64.b32encode(digest[:15]).decode().lower()
    return "-".join(text[i : i + 4] for i in range(0, len(text), 4))


class HostKey:
    """The host's identity: an Ed25519 seed. Its signing key vouches for
    what this host says in the clear, presence and acknowledgements; the
    Curve25519 key derived from it seals bodies. The public key a host
    announces, and the fingerprint people read aloud, are the verify
    key's; the box key follows from it on both sides."""

    def __init__(self, seed: bytes):
        nacl = _nacl()
        self.signing = nacl.signing.SigningKey(seed)
        self.box = self.signing.to_curve25519_private_key()
        self.public = bytes(self.signing.verify_key)
        self.fingerprint = fingerprint_of(self.public)


_BOX_PUBLIC_CACHE: dict[bytes, bytes] = {}


def box_public(verify_key_bytes: bytes) -> bytes:
    """The Curve25519 key a host's announced verify key seals to."""
    found = _BOX_PUBLIC_CACHE.get(verify_key_bytes)
    if found is None:
        found = bytes(_nacl().signing.VerifyKey(verify_key_bytes).to_curve25519_public_key())
        if len(_BOX_PUBLIC_CACHE) > 256:
            _BOX_PUBLIC_CACHE.clear()
        _BOX_PUBLIC_CACHE[verify_key_bytes] = found
    return found


def _canonical(fields: dict) -> bytes:
    return json.dumps(fields, sort_keys=True, separators=(",", ":")).encode()


def sign_fields(fields: dict) -> str | None:
    """This host's signature over the fields, or None when it has no key
    or no crypto: what it then writes is a claim, and no reader binds
    anything to it."""
    try:
        key = load_host_key()
    except SystemExit:
        return None
    if key is None:
        return None
    return base64.b64encode(key.signing.sign(_canonical(fields)).signature).decode()


def signature_holds(public: bytes, sig, fields: dict) -> bool:
    if not isinstance(sig, str) or not sig or len(sig) > 256:
        return False
    try:
        nacl = _nacl()
    except SystemExit:
        return False
    try:
        nacl.signing.VerifyKey(public).verify(_canonical(fields), base64.b64decode(sig))
        return True
    except Exception:  # noqa: BLE001 - every failure is one answer: not this host's word
        return False


def verify_keys_of(host: str) -> list[bytes]:
    """The keys whose word is `host`'s: this host's own, or every key this
    host has pinned for it. An unpinned host has none, however well it
    signs."""
    if not isinstance(host, str) or not host:
        return []
    if host == socket.gethostname():
        try:
            mine = load_host_key()
        except SystemExit:
            return []
        return [mine.public] if mine else []
    fps = [fp for fp, h in trusted_hosts_by_fingerprint().items() if h == host]
    if not fps:
        return []
    keys = announced_public_keys()
    if any(fp not in keys for fp in fps):
        keys = announced_public_keys(max_age_s=0)
    return [keys[fp] for fp in fps if fp in keys]


def host_vouches(host: str, sig, fields: dict, keys: list[bytes] | None = None) -> bool:
    """Whether `sig` over `fields` is the word of `host`."""
    keys = verify_keys_of(host) if keys is None else keys
    return any(signature_holds(public, sig, fields) for public in keys)


_HOST_KEY_CACHE: dict = {"path": None, "mtime": None, "key": None}


def _read_key_file(path: Path) -> tuple[str, bytes] | None:
    """(format, raw key). The sealing key file is `ed25519 <base64 seed>`;
    a bare base64 line is the pre-sealing X25519 key the keys release
    wrote, which cannot sign and is never reinterpreted as a seed."""
    try:
        text = path.read_text().strip()
    except OSError:
        return None
    alg, _, rest = text.partition(" ")
    if rest and alg == HOST_KEY_ALG:
        return HOST_KEY_ALG, base64.b64decode(rest.strip())
    return "x25519", base64.b64decode(text)


def host_key_format() -> str | None:
    """`ed25519`, `x25519` for a pre-sealing key, or None without a key."""
    found = _read_key_file(host_key_path())
    return found[0] if found else None


def load_host_key() -> HostKey | None:
    """The host's sealing key, or None when there is none: `a2a key init`
    has not run, or the key is the pre-sealing format that `a2a key
    migrate` replaces. Read once per file version: every signature
    checked asks for it."""
    path = host_key_path()
    try:
        mtime = path.stat().st_mtime_ns
    except OSError:
        return None
    if _HOST_KEY_CACHE["path"] == str(path) and _HOST_KEY_CACHE["mtime"] == mtime:
        return _HOST_KEY_CACHE["key"]
    found = _read_key_file(path)
    key = HostKey(found[1]) if found and found[0] == HOST_KEY_ALG and len(found[1]) == 32 else None
    _HOST_KEY_CACHE.update(path=str(path), mtime=mtime, key=key)
    return key


def require_host_key() -> HostKey:
    key = load_host_key()
    if key is None:
        if host_key_format() == "x25519":
            raise SystemExit(f"a2a: the host key at {host_key_path()} is the pre-sealing X25519 format; run `a2a key migrate` to make the sealing key, then read its new fingerprint to your peers to re-pin")
        raise SystemExit(f"a2a: no host key at {host_key_path()}; run `a2a key init` on this host first")
    return key


def _write_new_host_key(path: Path) -> bytes:
    nacl = _nacl()
    seed = bytes(nacl.signing.SigningKey.generate())
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    with os.fdopen(fd, "w") as fh:
        fh.write(f"{HOST_KEY_ALG} {base64.b64encode(seed).decode()}\n")
    _HOST_KEY_CACHE.update(path=None)
    return seed


def key_init() -> str:
    nacl = _nacl()
    path = host_key_path()
    if path.exists():
        raise SystemExit(f"a2a: {path} exists; refusing to overwrite a host key (rotation is a new key on a new host name, announced and re-pinned by hand)")
    # The chat events on this host's relay at this moment are its
    # pre-sealing history; their ids are the only plaintext ever readable.
    # The record is taken first and the key written after it, as one step:
    # a relay that does not answer leaves no key, and bootstrap tries
    # again, so no key ever exists whose history was taken later, from a
    # relay whose timestamps nothing authenticates.
    legacy = _key_path(LEGACY_IDS_NAME)
    if not legacy.exists():
        try:
            _record_legacy_ids()
        except LogstreamError as exc:
            raise SystemExit(f"a2a: the relay did not answer, so this host's pre-sealing history could not be recorded and no key was made; run `a2a key init` again once it is up ({exc})")
    seed = _write_new_host_key(path)
    return HostKey(seed).fingerprint


def _record_legacy_ids() -> int:
    """Write legacy-ids: the plaintext chat events on the relay at this
    moment, which is the moment sealing begins here. Raises LogstreamError,
    and writes nothing, when the relay does not answer."""
    events = list_events(room=ROOM_CHAT)
    mine = load_host_key()
    fps = {mine.fingerprint} if mine else set()
    for ev in events:
        enc = metadata_of(ev).get("enc")
        if isinstance(enc, dict) and (isinstance(enc.get("to"), str) and enc["to"] in fps or isinstance(enc.get("from"), str) and enc["from"] in fps):
            # Sealed traffic of this host's exists: a scan now would take
            # timestamps on trust, and a relay's timestamps are not
            # authenticated. The history stays unrecorded; nothing sends
            # from this host until it is, and only a person can decide
            # what this host's pre-sealing history was.
            raise SystemExit("a2a: this host already has sealed chat traffic, so its pre-sealing history cannot be recorded from the relay now; you can record it by hand from a copy taken before sealing, or accept that pre-sealing chats stay unreadable (touch the empty record)")
    # Plaintext history is an event with no envelope at all; anything
    # carrying an envelope, well formed or not, is never recorded as
    # trusted plaintext. The acknowledgements of that history, delivered,
    # read and uncertain, are recorded with it, so the ledger keeps what
    # it knew; nothing unsigned written later is ever taken.
    ids = [ev["id"] for ev in events if ev["type"] in CHAT_TYPES and "enc" not in metadata_of(ev)]
    known = set(ids)
    ack_ids = [ev["id"] for ev in events if ev["type"] == "event.ack" and metadata_of(ev).get("ack_of") in known]
    acks = _key_path(LEGACY_ACKS_NAME)
    tmp = acks.with_suffix(".tmp")
    tmp.write_text("\n".join(ack_ids) + ("\n" if ack_ids else ""))
    os.replace(tmp, acks)
    legacy = _key_path(LEGACY_IDS_NAME)
    tmp = legacy.with_suffix(".tmp")
    tmp.write_text("\n".join(ids) + ("\n" if ids else ""))
    os.replace(tmp, legacy)
    _LEGACY_IDS_CACHE.update(loaded=False)
    return len(ids)


_LEGACY_IDS_CACHE: dict = {"path": None, "ids": frozenset()}


def legacy_ids() -> frozenset:
    path = _key_path(LEGACY_IDS_NAME)
    if _LEGACY_IDS_CACHE["path"] != str(path) or not _LEGACY_IDS_CACHE.get("loaded"):
        ids = frozenset(line.strip() for line in path.read_text().splitlines() if line.strip()) if path.exists() else frozenset()
        acks_path = _key_path(LEGACY_ACKS_NAME)
        acks = frozenset(line.strip() for line in acks_path.read_text().splitlines() if line.strip()) if acks_path.exists() else frozenset()
        _LEGACY_IDS_CACHE.update(path=str(path), ids=ids, acks=acks, loaded=True)
    return _LEGACY_IDS_CACHE["ids"]


def legacy_ack_ids() -> frozenset:
    """The acknowledgements recorded with the pre-sealing history: the only
    unsigned ones that ever count."""
    legacy_ids()
    return _LEGACY_IDS_CACHE.get("acks") or frozenset()


def key_migrate() -> dict:
    """Bring a host from the keys release to sealing, idempotently.

    That release wrote an X25519 host-key, announced it, and had peers pin
    it, before any body was sealed. Such a key cannot sign, so it is
    retired beside a new Ed25519 key, whose fingerprint is different: the
    new key is announced, and the peers who pinned the old one see
    `changed` until they re-pin by hand, which is the same call they made
    the first time. Nothing sealed is lost, because nothing was sealed to
    the old key. The pre-sealing history record is taken as key init
    would have, and an existing record is kept."""
    out = {"migrated": False, "announced": False, "fingerprint": None, "recorded": 0}
    path = host_key_path()
    found = _read_key_file(path)
    if found is None:
        raise SystemExit(f"a2a: no host key at {path}; run `a2a key init` on this host first")
    if found[0] == "x25519":
        # The history is recorded before the key is replaced, so a relay
        # that fails at any step leaves a state the next run resumes from:
        # no record and the old key, or the record and the new key not yet
        # announced. Nothing is ever left as a new key without its record.
        if not _key_path(LEGACY_IDS_NAME).exists():
            out["recorded"] = _record_legacy_ids()
        retired = _key_path(RETIRED_KEY_NAME)
        if not retired.exists():
            os.replace(path, retired)
        _write_new_host_key(path)
        out["migrated"] = True
    key = require_host_key()
    out["fingerprint"] = key.fingerprint
    if _key_path(RETIRED_KEY_NAME).exists() and (announced_keys().get(socket.gethostname()) or {}).get("fingerprint") != key.fingerprint:
        # The retired key was announced; the new one takes its place on the
        # relay so peers see a changed key, not a missing one.
        announce_key()
        out["announced"] = True
    legacy = _key_path(LEGACY_IDS_NAME)
    if legacy.exists():
        out["recorded"] = len(legacy_ids())
    else:
        # A sealing key is made with its record in one step, so a key
        # without one is a record that was removed. It is not taken again
        # from the relay, whose timestamps nothing authenticates: a person
        # supplies it from a copy made before sealing, or accepts none.
        raise SystemExit("a2a: this host has a sealing key but no pre-sealing history record; it cannot be taken from the relay now. Restore it with `a2a key history --from <copy of legacy-ids>` or accept that pre-sealing chats stay unreadable with `a2a key history --none`")
    return out


def key_history(source: Path | None) -> int:
    """Write the history record by hand: from a copy, or empty."""
    legacy = _key_path(LEGACY_IDS_NAME)
    if legacy.exists():
        raise SystemExit(f"a2a: {legacy} exists; the history record is never replaced")
    ids = [line.strip() for line in source.read_text().splitlines() if line.strip()] if source else []
    tmp = legacy.with_suffix(".tmp")
    tmp.write_text("\n".join(ids) + ("\n" if ids else ""))
    os.replace(tmp, legacy)
    _LEGACY_IDS_CACHE.update(loaded=False)
    return len(ids)


def require_history_recorded() -> None:
    """Nothing seals from this host until its pre-sealing history record
    exists: the record can only be taken before sealed traffic, so a host
    that sealed first could never record its history safely."""
    if not _key_path(LEGACY_IDS_NAME).exists():
        raise SystemExit("a2a: this host's pre-sealing chat history is not recorded yet; run `a2a key migrate` once the relay answers (bootstrap does this before any chat). Nothing was sent")


def my_fingerprint() -> str:
    return require_host_key().fingerprint


_TRUST_CACHE: dict = {"path": None, "mtime": None, "trust": {}}


def _read_trust_file() -> dict:
    """The pins, re-read only when the file changes: every box opened asks."""
    path = _key_path(TRUST_NAME)
    try:
        mtime = path.stat().st_mtime_ns if path.exists() else None
    except OSError:
        mtime = None
    if _TRUST_CACHE["path"] == str(path) and _TRUST_CACHE["mtime"] == mtime:
        return _TRUST_CACHE["trust"]
    try:
        trust = json.loads(path.read_text()) if path.exists() else {}
    except ValueError:
        trust = {}
    _TRUST_CACHE.update(path=str(path), mtime=mtime, trust=trust)
    return trust


def read_trust() -> dict[str, str]:
    """host -> pinned fingerprint."""
    hosts = _read_trust_file().get("hosts") or {}
    return {h: f for h, f in hosts.items() if isinstance(h, str) and isinstance(f, str)}


def pin_host(host: str, fingerprint: str) -> None:
    """Pin a host's current fingerprint. Earlier pins stay on record, so a
    body sealed with a key this host once trusted still opens after the
    other host rotated; a key that was never pinned never speaks."""
    trust = _read_trust_file()
    hosts = dict(trust.get("hosts") or {})
    hosts[host] = fingerprint
    trust["hosts"] = hosts
    history = dict(trust.get("history") or {})
    known = list(history.get(host) or [])
    if fingerprint not in known:
        known.append(fingerprint)
    history[host] = known
    trust["history"] = history
    path = _key_path(TRUST_NAME)
    tmp = path.with_suffix(".tmp")
    tmp.write_text(json.dumps(trust, indent=2, sort_keys=True) + "\n")
    os.chmod(tmp, 0o600)
    os.replace(tmp, path)
    _HOSTS_CACHE.update(at=0.0)


def trusted_hosts_by_fingerprint() -> dict[str, str]:
    """fingerprint -> host, for every fingerprint this host has ever pinned."""
    trust = _read_trust_file()
    out: dict[str, str] = {}
    for host, fps in (trust.get("history") or {}).items():
        for fp in fps or []:
            out[fp] = host
    for host, fp in (trust.get("hosts") or {}).items():
        out[fp] = host
    return out


def announce_key() -> dict:
    key = require_host_key()
    event = append(
        "key.announce", ROOM_KEYS, to="*", status="ready",
        metadata={"host": socket.gethostname(), "alg": HOST_KEY_ALG, "fingerprint": key.fingerprint, "public_key": base64.b64encode(key.public).decode()},
    )
    _PUBLIC_KEYS_CACHE.update(at=0.0)
    _HOSTS_CACHE.update(at=0.0)
    return event


def announced_keys() -> dict[str, dict]:
    """host -> the newest key.announce for it, as {fingerprint, public_key}.
    An announce whose fingerprint does not match its public key is ignored:
    the fingerprint is what a person pinned, and it must be the key's own."""
    latest: dict[str, dict] = {}
    for event in list_events(room=ROOM_KEYS, type="key.announce"):
        meta = metadata_of(event)
        host, fp, pub = meta.get("host"), meta.get("fingerprint"), meta.get("public_key")
        raw = _announced_key(fp, pub, meta.get("alg"))
        if not (isinstance(host, str) and host and raw):
            continue
        latest[host] = {"fingerprint": fp, "public_key": pub, "announced_at": event.get("created_at")}
    return latest


def host_key_status(host: str, announced: dict[str, dict] | None = None, trust: dict[str, str] | None = None) -> dict:
    """pinned | changed | unpinned | unannounced | self, with the facts."""
    if host == socket.gethostname():
        return {"status": "self", "fingerprint": None, "public_key": None}
    announced = announced_keys() if announced is None else announced
    trust = read_trust() if trust is None else trust
    seen = announced.get(host)
    pinned = trust.get(host)
    if not seen:
        return {"status": "unannounced", "fingerprint": None, "public_key": None, "pinned": pinned}
    if not pinned:
        return {"status": "unpinned", "fingerprint": seen["fingerprint"], "public_key": seen["public_key"]}
    if pinned != seen["fingerprint"]:
        return {"status": "changed", "fingerprint": seen["fingerprint"], "public_key": seen["public_key"], "pinned": pinned}
    return {"status": "pinned", "fingerprint": seen["fingerprint"], "public_key": seen["public_key"]}


_HOSTS_CACHE: dict = {"at": 0.0, "key": None, "hosts": {}, "auth": {}, "projects": {}, "project_names": {}}


def _refresh_hosts(max_age_s: float) -> None:
    """Host ownership and project placement read the derived presence
    state, kept incrementally by replaying only new events. Cached
    briefly, per host and key directory: a fold opens every box, and
    each box asks."""
    key = (str(KEY_DIR), socket.gethostname())
    if _HOSTS_CACHE["at"] and _HOSTS_CACHE["key"] == key and time.perf_counter() - _HOSTS_CACHE["at"] < max_age_s:
        return
    derived = _presence_state_refresh()
    hosts = dict(derived["hosts"])
    auth = {handle: host for handle, (host, _seq) in derived["auth"].items()}
    placements = {}
    for handle, (scope, session_key, at) in derived["projects"].items():
        latest = derived["sessions"].get(session_key) if session_key else None
        if latest is not None and latest[1] > at:
            # The scan's default handle and a renamed handle share a session.
            scope = latest[0]
        if scope is not None:
            placements[handle] = scope
    # Aged from the end of the pass: one that outlasts max_age_s would be
    # stale before its first reader, and every reader would repeat it.
    _HOSTS_CACHE.update(at=time.perf_counter(), key=key, hosts=hosts, auth=auth,
                        projects=placements,
                        project_names=dict(derived["names"]))


def last_known_hosts(max_age_s: float = 2.0) -> dict[str, str]:
    """handle -> the host its newest presence event claims, of any state,
    however old. A claim: anyone with relay access writes presence. It
    places nobody; it only says which host's key to ask for."""
    _refresh_hosts(max_age_s)
    return _HOSTS_CACHE["hosts"]


def authenticated_hosts(max_age_s: float = 2.0) -> dict[str, str]:
    """handle -> the host whose own key first vouched for it: presence
    signed by this host's key, or by a key pinned here for the host it
    names. The first host to vouch for a handle keeps it, so a pinned host
    cannot later speak in the name of another host's agent; the same
    handle chosen on two hosts belongs to the one that announced first,
    and the other renames. Going offline does not forget where an agent
    lives; a peer no pinned host has vouched for has no host here."""
    _refresh_hosts(max_age_s)
    return _HOSTS_CACHE["auth"]


def require_pinned_hosts(handles, known: dict[str, dict] | None = None, binding=None) -> dict[str, dict]:
    """Refuse unless every named agent's host is this host or a pinned one.
    Returns host -> key status for the callers that seal; a pass is always
    followed by a sealed body, never a plaintext one.

    For a chat that exists, `binding` is what its keys established: the two
    hosts it is sealed between and the host each handle spoke from. Those
    decide where a body is sealed to. A first contact is placed by signed
    presence, the word of the handle's own host, and a bare claim decides
    nothing but which host's key status the refusal names. A placement
    that contradicts the binding is refused, so a reply can never be
    sealed to a host the chat does not include."""
    known = known or {}
    announced = announced_keys()
    trust = read_trust()
    result: dict[str, dict] = {}
    chat_hosts, handle_hosts = binding if binding else (None, {})
    my_host = socket.gethostname()
    vouched = authenticated_hosts()
    claimed = last_known_hosts()
    looked_fresh = False
    for handle in handles:
        if handle == me():
            continue
        host = handle_hosts.get(handle)
        if host is None and chat_hosts is not None:
            # The chat's hosts are fixed; a handle that has not spoken yet is
            # on the other one, or this one for a local chat.
            others = chat_hosts - {my_host}
            host = next(iter(others)) if len(others) == 1 else my_host
        unvouched = None
        if not host:
            # A first contact: the handle's host is what its host's key
            # vouched for. A bare claim only says which host's key status
            # to report, so the refusal names the pin that is missing.
            host = vouched.get(handle)
            if not host and not looked_fresh:
                # A peer that appeared moments ago must not be refused
                # behind the brief placement cache: look once, fresh,
                # before saying a host was never seen.
                looked_fresh = True
                _refresh_hosts(0.0)
                vouched = authenticated_hosts(0.0)
                claimed = last_known_hosts(0.0)
                host = vouched.get(handle)
            if not host:
                unvouched = (known.get(handle) or {}).get("host") or claimed.get(handle)
                host = unvouched
        if chat_hosts is not None and host not in chat_hosts:
            raise SystemExit(f"a2a: {handle} is placed on {host}, which is not one of this chat's hosts ({', '.join(sorted(chat_hosts))}); nothing was sent")
        if not host:
            raise SystemExit(f"a2a: {handle} has no known host (never seen online); a message cannot be sealed to a host that is not known, so nothing was sent")
        st = require_host_pinned(host, handle, announced, trust)
        result[host] = st
        if unvouched:
            raise SystemExit(f"a2a: {handle} claims to be on {host}, but no presence signed by {host}'s key says so; nothing was sent")
    return result


# ── Credential-shaped bodies ────────────────────────────────────────────────
#
# Sealing makes the other host's replica a place where a pasted secret would
# land encrypted but retrievable by that host forever. A body that looks
# like a token, a key, or an environment assignment is refused before it is
# sealed, with the reason. Ordinary prose that mentions a token still sends.

_CREDENTIAL_SHAPES = [
    ("a private key block", re.compile(r"-----BEGIN [A-Z ]*PRIVATE KEY(?: BLOCK)?-----")),  # OpenSSH, PEM, PGP armor alike
    ("an age identity", re.compile(r"\bAGE-SECRET-KEY-1[0-9A-Z]{20,}")),
    ("a GitHub token", re.compile(r"\b(?:ghp|gho|ghu|ghs|ghr|github_pat)_[A-Za-z0-9_]{20,}")),
    ("an Anthropic key", re.compile(r"\bsk-ant-[A-Za-z0-9_-]{20,}")),
    ("an OpenAI-style key", re.compile(r"\bsk-(?!ant-)[A-Za-z0-9_-]{20,}")),
    ("a Slack token", re.compile(r"\bxox[abprs]-[A-Za-z0-9-]{10,}")),
    ("an AWS access key", re.compile(r"\b(?:AKIA|ASIA)[0-9A-Z]{16}\b")),  # long-term and temporary key ids alike
    ("a Google API key", re.compile(r"\bAIza[0-9A-Za-z_-]{35}(?![0-9A-Za-z_-])")),  # the boundary is the key's own alphabet, hyphen included
    ("a bearer header", re.compile(r"(?i)\bauthorization:\s*bearer\s+\S+")),  # any length: the header is the shape
    ("a JSON web token", re.compile(r"\beyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+")),  # the payload may be as short as {}
    # Only names that end in a secret word: MAX_TOKEN_COUNT and
    # MY_SECRET_ENABLED are configuration, GITHUB_TOKEN and DB_PASSWORD are not.
    ("an environment assignment of a secret", re.compile(r"(?i)(?<![A-Za-z0-9_])(?:export\s+)?(?:[A-Z][A-Z0-9_]*_)?(?:TOKEN|SECRET|PASSWORD|PASSWD|API_KEY|APIKEY|PRIVATE_KEY|ACCESS_KEY|ACCESS_KEY_ID|SECRET_ACCESS_KEY)\s*=\s*\S+")),  # anywhere in the text, not only at a line's start
    ("a secret assignment", re.compile(r"(?i)\b(?:token|secret|password|passwd|api[_-]?key)\s*[:=]\s*['\"]?[A-Za-z0-9_\-./+=]{16,}")),
]


def credential_shape(text: str) -> str | None:
    """The first credential shape found in text, or None."""
    for name, pattern in _CREDENTIAL_SHAPES:
        if pattern.search(text or ""):
            return name
    return None


def refuse_credential_shapes(text: str) -> None:
    shape = credential_shape(text)
    if shape:
        raise SystemExit(f"a2a: the message looks like it carries {shape}; a chat body is stored on the other host for as long as they keep it, so secrets never go in one. Nothing was sent")


def require_host_pinned(host: str, label: str, announced: dict[str, dict] | None = None, trust: dict[str, str] | None = None) -> dict:
    """The key status of a host a body is about to be sealed to, or the
    refusal that names what is missing. `label` is what the message calls
    the reason for sealing there: an agent, or the chat itself."""
    st = host_key_status(host, announced, trust)
    if st["status"] == "self":
        require_host_key()
        return st
    if st["status"] == "unannounced":
        raise SystemExit(f"a2a: host {host} ({label}) has announced no key; they run `a2a key init` (or `a2a key migrate`, for a key from before sealing) and `a2a key announce`, then you pin it. Nothing was sent")
    if st["status"] == "unpinned":
        raise SystemExit(f"a2a: host {host} ({label}) is not pinned; read its fingerprint {st['fingerprint']} with whoever runs it and run `a2a key trust {host} {st['fingerprint']}`. Nothing was sent")
    if st["status"] == "changed":
        raise SystemExit(f"a2a: host {host} ({label}) announced a different key ({st['fingerprint']}) than the one pinned ({st['pinned']}); confirm with whoever runs it and re-pin. Nothing was sent")
    # Pinned: what follows is a sealed body, so the crypto must be here.
    _nacl()
    return st


# ── Sealed bodies ───────────────────────────────────────────────────────────
#
# Every chat event's body is a NaCl box from this host's key to the one
# other host in the chat, or to this host's own key when every member is
# local, so there is one code path. The box authenticates the sender host;
# the plaintext carries the chat id, event type, sender and recipient
# handles, and the text, and a reader refuses a box whose contents do not
# match the event that carries it. What stays in the clear: event type, chat
# id, handles, timestamps, lifecycle markers, and the envelope below.

ENVELOPE_VERSION = 1
ENVELOPE_ALG = "nacl-box"
_PUBLIC_KEYS_CACHE: dict = {"at": 0.0, "keys": {}}


def _announced_key(fp, pub, alg=HOST_KEY_ALG) -> bytes | None:
    """The raw key an announce carries, when its fields have the right
    shape, it is a sealing key, and its fingerprint is the key's own; a
    malformed announce from any host is skipped, never raised, so it
    cannot stop a fold. The keys release announced X25519 keys with no
    `alg`; those cannot sign or seal under this scheme and count as no
    announcement, which `a2a key migrate` on that host replaces."""
    if alg != HOST_KEY_ALG:
        return None
    if not (isinstance(fp, str) and isinstance(pub, str) and 0 < len(fp) <= 64 and 0 < len(pub) <= 128):
        return None
    try:
        raw = base64.b64decode(pub, validate=True)
    except (ValueError, TypeError):
        return None
    if len(raw) != 32 or fingerprint_of(raw) != fp:
        return None
    return raw


def announced_public_keys(max_age_s: float = 5.0) -> dict[str, bytes]:
    """fingerprint -> public key for every valid announce ever made, so a
    body sealed to a host's earlier key still opens after that host
    rotated. Which key a host uses now is announced_keys(); which keys have
    existed is this."""
    now = time.perf_counter()
    if now - _PUBLIC_KEYS_CACHE["at"] > max_age_s:
        keys: dict[str, bytes] = {}
        for event in list_events(room=ROOM_KEYS, type="key.announce"):
            meta = metadata_of(event)
            raw = _announced_key(meta.get("fingerprint"), meta.get("public_key"), meta.get("alg"))
            if raw:
                keys[meta["fingerprint"]] = raw
        _PUBLIC_KEYS_CACHE.update(at=now, keys=keys)
    return _PUBLIC_KEYS_CACHE["keys"]


def _public_key_for_fingerprint(fingerprint: str, mine) -> bytes | None:
    if fingerprint == mine.fingerprint:
        return mine.public
    for fresh in (False, True):
        found = announced_public_keys(max_age_s=0 if fresh else 5.0).get(fingerprint)
        if found:
            return found
    return None


# The outer metadata fields the fold and the courier act on. Each is bound
# inside the box, so an edit to the outer copy makes the event unreadable
# before it can route or change a chat's state.
BOUND_FIELDS = ("members", "member", "close", "lifecycle")


FRONTIER_LIMIT = 4096  # a chat whose frontier is wider than this is refused a new event, never one with a dependency dropped


def seal_body(text: str, type_: str, chat_id: str, to_handle: str | None, hosts: dict[str, dict], meta: dict | None = None, after: list[str] | None = None, eid: str | None = None) -> tuple[str, dict]:
    """Seal one event's text for the chat's other host, or for this host.
    `hosts` is require_pinned_hosts' answer: host -> key status. The box
    carries a fresh event id, every outer field the readers act on, the
    moment it was written and `after`, the frontier of the chat as the
    writer had folded it: every event nothing folded yet follows. A
    reader folds this event only once all of them have, so the membership
    the writer saw, every departure included, is the membership it is
    routed against everywhere."""
    nacl = _nacl()
    mine = require_host_key()
    remote = [st for st in hosts.values() if st["status"] != "self"]
    if len({st["fingerprint"] for st in remote}) > 1:
        raise SystemExit("a2a: this chat spans more than two hosts; a body can be sealed to one other host in v1, so nothing was sent")
    if remote:
        peer_public = box_public(base64.b64decode(remote[0]["public_key"]))
        to_fp = remote[0]["fingerprint"]
    else:
        peer_public = bytes(mine.box.public_key)
        to_fp = mine.fingerprint
    if len(after or []) > FRONTIER_LIMIT:
        raise SystemExit(f"a2a: this chat's frontier is {len(after)} events wide, more than a box may name; start a new chat. Nothing was sent")
    box = nacl.public.Box(mine.box, nacl.public.PublicKey(peer_public))
    eid = eid or secrets.token_hex(16)
    my_fp = mine.fingerprint
    # The box's shared key is the same in both directions, so the
    # direction is bound inside it: which key sealed, which key it is for.
    inner = {"v": ENVELOPE_VERSION, "eid": eid, "chat": chat_id, "type": type_, "from": me(), "to": to_handle or chat_id, "text": text, "from_fp": my_fp, "to_fp": to_fp, "at": time.time(), "after": list(after or [])}
    for field in BOUND_FIELDS:
        if meta and field in meta:
            inner[field] = meta[field]
    plaintext = json.dumps(inner, sort_keys=True).encode()
    nonce = secrets.token_bytes(nacl.public.Box.NONCE_SIZE)
    sealed = box.encrypt(plaintext, nonce)
    envelope = {"v": ENVELOPE_VERSION, "alg": ENVELOPE_ALG, "eid": eid, "to": to_fp, "from": my_fp, "nonce": base64.b64encode(nonce).decode()}
    return base64.b64encode(sealed.ciphertext).decode(), envelope


def _envelope(meta: dict) -> tuple[dict | None, str | None]:
    """The envelope with every field checked for shape before any use, or a
    bounded reason. A malformed field in one relay event must not stop the
    fold for every other."""
    enc = meta.get("enc")
    if not isinstance(enc, dict):
        return None, None
    if enc.get("v") != ENVELOPE_VERSION or enc.get("alg") != ENVELOPE_ALG:
        return None, f"unknown envelope {enc.get('alg')!r} v{enc.get('v')!r}"
    for field in ("eid", "to", "from", "nonce"):
        value = enc.get(field)
        if not isinstance(value, str) or not value or len(value) > 256:
            return None, f"envelope field {field} is not a short string"
    return enc, None


def open_event(e: dict) -> tuple[str, str | None, str | None, str | None]:
    """(text, why unreadable, sender host, recipient host); see open_inner."""
    inner, why, sender_host, recipient_host = open_inner(e)
    if why:
        return "", why, None, None
    text = inner.get("text")
    return (text if isinstance(text, str) else ""), None, sender_host, recipient_host


def open_inner(e: dict) -> tuple[dict, str | None, str | None, str | None]:
    """(the box's contents, why unreadable, sender host, recipient host).

    A sealed body opens only with this host's key and a key this host has
    pinned, and the hosts it names come from those keys, never from the
    event: the sender host is the host whose pinned key sealed it (or this
    host, for its own key), the recipient host is the host whose key it was
    sealed to. Plaintext is readable only as recorded pre-sealing history,
    with this host as both ends."""
    meta = metadata_of(e)
    body = e.get("body") or ""
    if not isinstance(body, str):
        return {}, "body is not text", None, None
    enc, why = _envelope(meta)
    if why:
        return "", why, None, None
    if enc is None:
        if e.get("id") in legacy_ids():
            return {"text": body, "at": _epoch(e.get("created_at")), "after": []}, None, socket.gethostname(), socket.gethostname()
        return {}, "plaintext body that is not this host's pre-sealing history; refused", None, None
    try:
        nacl = _nacl()
    except SystemExit:
        return {}, "PyNaCl is not installed here", None, None
    mine = load_host_key()
    if mine is None:
        return {}, "no host key here", None, None
    my_fp = mine.fingerprint
    my_host = socket.gethostname()
    to_fp, from_fp = enc["to"], enc["from"]
    if my_fp not in (to_fp, from_fp):
        return {}, "sealed to another host", None, None
    pinned = trusted_hosts_by_fingerprint()
    sender_host = my_host if from_fp == my_fp else pinned.get(from_fp)
    recipient_host = my_host if to_fp == my_fp else pinned.get(to_fp)
    if sender_host is None:
        return {}, f"the sender's key {from_fp} is not one this host has pinned", None, None
    if recipient_host is None:
        return {}, f"the recipient's key {to_fp} is not one this host has pinned", None, None
    peer_fp = from_fp if to_fp == my_fp else to_fp
    peer_public = _public_key_for_fingerprint(peer_fp, mine)
    if peer_public is None:
        return {}, f"the other host's key {peer_fp} is not announced", None, None
    try:
        box = nacl.public.Box(mine.box, nacl.public.PublicKey(box_public(peer_public)))
        # Only the canonical spelling of the bytes is a box: a copy with
        # its base64 re-spelt is not the same event and not an event.
        ciphertext = base64.b64decode(body, validate=True)
        if base64.b64encode(ciphertext).decode() != body:
            return {}, "the body is not a canonical box", None, None
        plaintext = box.decrypt(ciphertext, base64.b64decode(enc["nonce"], validate=True))
        inner = json.loads(plaintext)
    except Exception:  # noqa: BLE001 - every failure here is one answer: unreadable
        return {}, "the box does not open: tampered, truncated, or not for this key", None, None
    if not isinstance(inner, dict) or inner.get("v") != ENVELOPE_VERSION:
        return {}, "the box holds an unknown version", None, None
    if inner.get("chat") != e.get("correlation_id") or inner.get("type") != e.get("type") or inner.get("from") != e.get("from_agent"):
        return {}, "the box names a different chat, type or sender than the event that carries it", None, None
    if inner.get("to") != (e.get("to_agent") or e.get("correlation_id")):
        return {}, "the box names a different recipient than the event that carries it", None, None
    if inner.get("eid") != enc["eid"]:
        return {}, "the box's event id does not match its envelope", None, None
    if inner.get("from_fp") != from_fp or inner.get("to_fp") != to_fp:
        return {}, "the box's direction does not match its envelope", None, None
    for field in BOUND_FIELDS:
        if inner.get(field) != meta.get(field):
            return {}, f"the box's {field} does not match the event that carries it", None, None
    if not _well_formed_fields(inner):
        return {}, "the box's fields are malformed", None, None
    if not isinstance(inner.get("at"), (int, float)) or isinstance(inner.get("at"), bool):
        return {}, "the box carries no moment", None, None
    after = inner.get("after")
    if not isinstance(after, list) or len(after) > FRONTIER_LIMIT or any(not (isinstance(k, str) and 0 < len(k) <= 128) for k in after):
        return {}, "the box names what it follows badly", None, None
    return inner, None, sender_host, recipient_host


_HANDLE_RE = re.compile(r"[A-Za-z0-9][A-Za-z0-9._-]{0,63}")


def _well_formed_fields(inner: dict) -> bool:
    """The shape a reader relies on, checked before a box enters the fold:
    a members list of handles, a member handle, short strings for the
    lifecycle markers. A box that passes its key and still carries the
    wrong shapes is reported, never folded."""
    members = inner.get("members")
    if members is not None and not (isinstance(members, list) and len(members) <= 256 and all(isinstance(m, str) and _HANDLE_RE.fullmatch(m) for m in members)):
        return False
    member = inner.get("member")
    if member is not None and not (isinstance(member, str) and _HANDLE_RE.fullmatch(member)):
        return False
    for field in ("close", "lifecycle"):
        value = inner.get(field)
        if value is not None and not (isinstance(value, str) and len(value) <= 64):
            return False
    return True


def _epoch(created_at) -> float:
    try:
        return time.mktime(time.strptime(created_at or "", "%Y-%m-%dT%H:%M:%SZ")) - time.timezone
    except ValueError:
        return 0.0


def open_body(e: dict) -> tuple[str, str | None]:
    """(text, None) when the event's body is readable here, else ("",
    reason). See open_event for the rules."""
    text, why, _, _ = open_event(e)
    return text, why


def opened(e: dict) -> dict:
    """A copy of the event with its body opened for a reader that only
    understands plaintext, and `unreadable` set when it could not be."""
    text, why = open_body(e)
    out = dict(e)
    out["body"] = text
    out["scope"] = scope_str(scope_of(e))  # the identity a reader files a delivery under, whichever relay row carries the box
    if why:
        out["unreadable"] = why
    return out


# ── Chats ───────────────────────────────────────────────────────────────────

CHAT_TYPES = ("chat.invite", "chat.accept", "chat.decline", "chat.message", "chat.join", "chat.leave", "chat.close-request", "chat.close-ack", "chat.closed")


def chat_events(chat_id: str | None = None) -> list[dict]:
    """The chat events this host can vouch for, oldest first: every sealed
    event verified against its box, plaintext only as pre-sealing history,
    and one event per authenticated id, so a replay of the same box under
    another relay id is not a second event."""
    return verified_chats(chat_id)[0]


UNREADABLE_LIMIT = 20
REASON_CHARS = 160


def unreadable_events(chat_id: str | None = None, chats: dict[str, dict] | None = None, since: str | None = None) -> list[dict]:
    """The bounded error view: chat-room events this host could not accept,
    newest last, at most UNREADABLE_LIMIT within the hold, each with its
    bounded reason and the identifiers the event claims, which nothing
    verified. They change no chat's state, route to nobody and are never
    acknowledged; they are shown so a damaged or misdirected body is a
    fact a reader sees, not a silence."""
    verified, _, _ = verified_chats(chat_id)
    accepted = {e["id"] for e in verified}
    filters = {"room": ROOM_CHAT}
    if chat_id:
        filters["correlation_id"] = chat_id
    if since:
        filters["since_event_id"] = since
    out = []
    for e in list_events(**filters):
        if e["type"] not in CHAT_TYPES or e["id"] in accepted or _age(e) > HOLD_S:
            continue
        cid = e.get("correlation_id") or ""
        if chats is not None and cid not in chats:
            # A first invitation this host could not accept names no chat
            # of mine yet; one that claims to address me is still reported,
            # so a damaged or not-yet-verifiable first contact is not a
            # silence. It folds nothing, routes nowhere and is never
            # acknowledged.
            members = metadata_of(e).get("members")
            named = [m for m in members if isinstance(m, str)] if isinstance(members, list) else []
            claims_me = e["type"] == "chat.invite" and (e.get("to_agent") == me() or me() in named)
            if not claims_me:
                continue
        inner, why, sender_host, _ = open_inner(e)
        if why is None:
            after = inner.get("after") or []
            why = (f"the box opens, but an event it follows ({', '.join(after[:4])}) is not here yet, or its sender is not placed in this chat" if after else "the box opens, but its sender is not placed in this chat") + (f" (sealed by {sender_host})" if sender_host else "")
        out.append({"id": e["id"], "chat": handle_text(cid), "from": handle_text(e.get("from_agent")), "type": e["type"], "at": e.get("created_at"), "reason": inert(why)[:REASON_CHARS], "untrusted": True})
    return out[-UNREADABLE_LIMIT:]


def render_unreadable(u: dict) -> str:
    return f"[{u['chat']}] unreadable {u['type'].split('.', 1)[1]} claiming to be from {u['from']} (unverified): {u['reason']}"


def chat_frontier(chat_id: str) -> list[str]:
    """The keys of the chat's folded events that nothing folded follows:
    what a new event must name so a reader folds it only after them."""
    history = chat_events(chat_id)
    followed = {k for e in history for k in e.get("after") or []}
    return [e["key"] for e in history if e["key"] not in followed]


def chat_binding(chat_id: str) -> tuple[set[str] | None, dict[str, str]]:
    """The two hosts a chat is sealed between, from its keys, and the host
    each handle has spoken from. None for a pre-sealing chat that no sealed
    event has fixed yet."""
    _, hosts, handles = verified_chats(chat_id)
    return hosts.get(chat_id), handles.get(chat_id, {})


_CHAT_HOSTS_CACHE: dict = {"path": None, "mtime": None, "hosts": {}}


def _chat_record_name(cid: str) -> str:
    return cid if re.fullmatch(r"[A-Za-z0-9._-]{1,128}", cid) else hashlib.sha256(cid.encode()).hexdigest()


def recorded_chat_hosts() -> dict[str, set[str]]:
    """chat id -> its host pair, as this host first saw it fixed by keys.
    A chat's hosts are a fact about its opening, not about whichever box
    claims the earliest moment: once seen, they are kept, so a pinned
    third host cannot re-open an existing chat with a backdated invite.
    One immutable file per chat, so any number of readers, agents,
    courier and ledger alike, record concurrently without losing one."""
    path = _key_path(CHAT_HOSTS_NAME)
    try:
        mtime = path.stat().st_mtime_ns if path.is_dir() else None
    except OSError:
        mtime = None
    if _CHAT_HOSTS_CACHE["path"] == str(path) and _CHAT_HOSTS_CACHE["mtime"] == mtime:
        return _CHAT_HOSTS_CACHE["hosts"]
    hosts: dict[str, set[str]] = {}
    if mtime is not None:
        for entry in path.iterdir():
            try:
                raw = json.loads(entry.read_text())
            except (OSError, ValueError):
                continue
            cid, pair = raw.get("chat") if isinstance(raw, dict) else None, raw.get("hosts") if isinstance(raw, dict) else None
            if isinstance(cid, str) and isinstance(pair, list) and pair and all(isinstance(h, str) for h in pair):
                hosts[cid] = set(pair)
    _CHAT_HOSTS_CACHE.update(path=str(path), mtime=mtime, hosts=hosts)
    return hosts


def _record_chat_hosts(fixed: dict[str, set[str]]) -> None:
    known = recorded_chat_hosts()
    new = {cid: pair for cid, pair in fixed.items() if cid not in known}
    if not new:
        return
    path = _key_path(CHAT_HOSTS_NAME)
    path.mkdir(mode=0o700, exist_ok=True)
    for cid, pair in new.items():
        # Created exclusively: the first process to see a chat records it,
        # and every later one finds the record and writes nothing.
        try:
            fd = os.open(path / _chat_record_name(cid), os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
        except FileExistsError:
            continue
        with os.fdopen(fd, "w") as fh:
            fh.write(json.dumps({"chat": cid, "hosts": sorted(pair)}, sort_keys=True) + "\n")
    _CHAT_HOSTS_CACHE.update(path=None)


def verified_chats(chat_id: str | None = None) -> tuple[list[dict], dict[str, set[str] | None], dict[str, dict[str, str]]]:
    events = list_events(room=ROOM_CHAT, correlation_id=chat_id) if chat_id else list_events(room=ROOM_CHAT)
    # Per chat: the two hosts its opening box named, and the host each
    # handle first spoke from. Both come from keys, never from presence or
    # from a field of the event, so a later announcement, however worded,
    # cannot move a participant to another host or let a third host in.
    # A pre-sealing chat has no keys to name its hosts; the first sealed
    # event, whose keys are pinned, fixes them. A missing entry means the
    # chat has not been seen; None means seen, not yet fixed.
    chat_hosts: dict[str, set[str] | None] = {cid: set(h) for cid, h in recorded_chat_hosts().items() if not chat_id or cid == chat_id}
    handle_host: dict[str, dict[str, str]] = {}
    my_host = socket.gethostname()
    vouched = authenticated_hosts()
    candidates = []
    for e in events:
        if e["type"] not in CHAT_TYPES:
            continue
        inner, unreadable, sender_host, recipient_host = open_inner(e)
        if unreadable:
            continue
        sealed = isinstance(metadata_of(e).get("enc"), dict)
        key = (metadata_of(e).get("enc") or {}).get("eid") if sealed else e["id"]
        candidates.append((key, e, inner, sealed, sender_host, recipient_host))
    # An event's id is scoped to its chat and to the key that sealed it:
    # the same id there twice is one event when the boxes are the same
    # bytes, and a copy under another relay id is not a second one; two
    # different boxes under one id from one key are that key's own
    # conflict, and neither counts. A box from any other key under that
    # id is that key's own event, judged on its own, so no third host can
    # invalidate an event by reusing its id. Every replica keeps the same
    # one.
    by_id: dict[tuple[str, str, str], list] = {}
    for c in candidates:
        if c[3]:
            by_id.setdefault((c[1].get("correlation_id") or "", c[0], c[2].get("from_fp") or ""), []).append(c)
    kept = []
    for c in candidates:
        if c[3]:
            group = by_id[(c[1].get("correlation_id") or "", c[0], c[2].get("from_fp") or "")]
            if len({x[1].get("body") for x in group}) > 1 or c is not group[0]:
                continue
        kept.append(c)
    # Recorded plaintext history is this host's own, from before sealing:
    # it keeps its recorded order and binds nothing, and a chat it opens
    # exists for the sealed events that continue it.
    # Who a recorded chat's members were, from its own plaintext: only one
    # of them may write the first sealed event that fixes its host pair.
    legacy_members: dict[str, set[str]] = {}
    for c in kept:
        if c[3]:
            continue
        e, cid = c[1], c[1].get("correlation_id") or ""
        meta = metadata_of(e)
        if e["type"] == "chat.invite":
            chat_hosts.setdefault(cid, None)
            named = [m for m in meta.get("members") or [] if isinstance(m, str)] if isinstance(meta.get("members"), list) else []
            legacy_members.setdefault(cid, set()).update([e.get("from_agent") or ""] + named + ([e["to_agent"]] if isinstance(e.get("to_agent"), str) else []))
        elif e["type"] == "chat.join" and isinstance(meta.get("member"), str):
            legacy_members.setdefault(cid, set()).add(meta["member"])

    def accept(c) -> bool:
        """Whether a sealed event belongs to its chat's verified fold: its
        key from one of the chat's two hosts, its handle placed on the
        host that sealed it, and a chat that exists."""
        _, e, inner, _, sender_host, recipient_host = c
        # A pinned key proves which host sealed the event, not that the
        # handle it speaks as lives there: a remote handle's host is the
        # one whose own key vouched for it in presence, and the sealing
        # key must be that host's. This host's key speaks for its own
        # agents by construction.
        if sender_host != my_host and vouched.get(e.get("from_agent") or "") != sender_host:
            return False
        cid = e.get("correlation_id") or ""
        if e["type"] == "chat.invite" and cid != chat_id_for(inner.get("from_fp") or "", c[0]):
            return False  # a sealed chat's id is derived from its opener's key; this key did not open this chat
        if cid not in chat_hosts:
            if e["type"] != "chat.invite":
                return False  # a chat exists from its invite; nothing precedes it
            chat_hosts[cid] = {sender_host, recipient_host}
        elif chat_hosts[cid] is None:
            # A recorded chat's pair is fixed by its first sealed event, and
            # only a member of that chat, by its own plaintext, may write it:
            # an outsider's box, however well sealed, fixes nothing.
            if (e.get("from_agent") or "") not in legacy_members.get(cid, set()):
                return False
            chat_hosts[cid] = {sender_host, recipient_host}
        elif sender_host not in chat_hosts[cid] or recipient_host not in chat_hosts[cid]:
            return False  # a key from outside the chat's two hosts
        bound = handle_host.setdefault(cid, {})
        who = e.get("from_agent") or ""
        return bound.setdefault(who, sender_host) == sender_host  # else this handle already speaks from another host

    # Every replica folds the same sealed events in the same order: by the
    # moment each box says it was written, then by its key, and each only
    # after every event it names as its frontier has been accepted into
    # the same chat's fold. An event whose predecessor is not here, or
    # not accepted yet, waits, however long: folding it without what its
    # writer saw would route it against a membership the writer did not
    # see. The sealed events take, in that order, the places sealed
    # events had in the relay's.
    accepted: set[tuple[str, str]] = {(c[1].get("correlation_id") or "", c[0]) for c in kept if not c[3]}
    accepted_sealed = []
    pending = sorted((c for c in kept if c[3]), key=lambda c: (c[2]["at"], c[0]))
    while pending:
        progressed = False
        for c in list(pending):
            cid = c[1].get("correlation_id") or ""
            if all((cid, k) in accepted for k in c[2].get("after") or []):
                pending.remove(c)
                progressed = True
                if accept(c):
                    accepted.add((cid, c[0]))
                    accepted_sealed.append(c)
        if not progressed:
            break  # what remains waits for a predecessor that is not here, or not accepted
    _record_chat_hosts({cid: h for cid, h in chat_hosts.items() if h})
    slots = iter(accepted_sealed)
    out = []
    for c in kept:
        if c[3]:
            c = next(slots, None)
            if c is None:
                continue
        elif (c[1].get("correlation_id") or "") not in chat_hosts:
            continue  # a chat exists from its invite; nothing precedes it
        key, e, inner, _, _, _ = c
        text = inner.get("text")
        # The fold judges content by what the box held, never by the
        # ciphertext: a sealed message of nothing is inert, and the
        # opened text rides with the event as `text`.
        out.append(dict(e, text=text if isinstance(text, str) else "", key=key, sealed_by=inner.get("from_fp"), after=list(inner.get("after") or [])))
    return out, chat_hosts, handle_host


def chat_state(events: list[dict]) -> dict:
    """Fold one chat's events.

    Members come from the first invitation's member list (pairs opened
    before rooms existed name the peer in `to_agent` instead), plus later
    joins by members. Only a member's events count. Every event is routed
    against the membership at that moment, recorded in `routes`, so a
    member added later never receives earlier traffic and one who left
    keeps what was sent before they went. Hosted chats end only when the
    opener ends them. Unmarked historical events retain their quorum and
    timed-force meaning.
    """
    state = {"id": None, "members": [], "left": set(), "agreed": set(), "state": "none", "opened_by": None, "close_requested_by": None, "closed_by": None, "close_cause": None, "hosted": False, "last": None, "messages": 0, "routes": {}, "accepted": set(), "forced": False, "opened_at": None, "left_at": {}, "agreed_at": {}}

    def add(handle):
        if handle and handle != "*" and handle != state["id"] and handle not in state["members"]:
            state["members"].append(handle)

    def current(sender):
        return [m for m in state["members"] if m != sender and m not in state["left"]]

    for e in events:
        state["id"] = e.get("correlation_id")
        meta = metadata_of(e)
        t = e["type"]
        sender = e.get("from_agent")
        if t == "chat.invite" and not state["members"]:
            add(sender)
            for m in (meta.get("members") if isinstance(meta.get("members"), list) else []) or []:
                add(m)
            add(e.get("to_agent"))
            state.update(state="invited", opened_by=sender, opened_at=e["created_at"], hosted=meta.get("lifecycle") == "hosted")
            state["last"] = e["created_at"]
            state["routes"][e["id"]] = current(sender)
            state["accepted"].add(e["id"])
            continue
        opener_close = (t == "chat.closed" and sender == state["opened_by"]
                        and meta.get("close") == "opener" and meta.get("lifecycle") == "hosted")
        # A legacy opener may have left without ending the chat. A new end
        # keeps their close authority without rejoining or restoring delivery.
        if sender not in state["members"] or (sender in state["left"] and not opener_close):
            state["routes"][e["id"]] = []  # an outsider's event reaches nobody
            continue
        if state["state"] in ("closed", "declined"):
            state["routes"][e["id"]] = []
            continue
        to = e.get("to_agent") or ""
        recipients = current(sender)
        if to and to not in (state["id"], "*"):
            recipients = [to] if to in recipients else []
        hosted = state["hosted"] or meta.get("lifecycle") == "hosted"
        if hosted:
            member = meta.get("member")
            valid_join = (t == "chat.join" and isinstance(member, str)
                          and re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9._-]{0,63}", member)
                          and member != state["id"] and member not in state["members"])
            content = e["text"] if "text" in e else e.get("body")
            valid_message = t == "chat.message" and isinstance(content, str) and content.strip() and recipients
            if not (t == "chat.accept" or valid_message or valid_join or opener_close
                    or (t in ("chat.leave", "chat.decline") and sender != state["opened_by"])):
                state["routes"][e["id"]] = []
                continue
        if hosted and not state["hosted"]:
            # A new action adopts the hosted rules without reinterpreting
            # earlier events or completing an old agreement on someone's leave.
            state["hosted"] = True
            if state["state"] == "closing":
                state["state"] = "open"
            state["agreed"].clear()
            state["agreed_at"].clear()
            state["close_requested_by"] = None
        if t == "chat.join" and state["state"] == "closing":
            state["routes"][e["id"]] = []
            continue
        if t == "chat.closed":
            silence = _seconds_between(state["last"], e["created_at"])
            legacy_force = not hosted and state["state"] == "closing" and sender == state["close_requested_by"] and silence >= CLOSE_TIMEOUT_S
            if opener_close or legacy_force:
                state["state"] = "closed"
                state["forced"] = legacy_force
                state["close_cause"] = "forced" if legacy_force else "opener"
                state["closed_by"] = sender
                state["last"] = e["created_at"]
                state["routes"][e["id"]] = current(sender)
                state["accepted"].add(e["id"])
            else:
                state["routes"][e["id"]] = []
            continue
        state["last"] = e["created_at"]
        state["accepted"].add(e["id"])
        if t == "chat.join":
            add(meta.get("member"))
            state["left"].discard(meta.get("member"))
        if t == "chat.join" or to == state["id"] or not to or to == "*":
            state["routes"][e["id"]] = current(sender)
        else:
            # A handle-addressed event stays inside the member set of its moment.
            state["routes"][e["id"]] = [to] if to in current(sender) else []
        if t == "chat.leave" or t == "chat.decline":
            state["left"].add(sender)
            state["left_at"][sender] = e["created_at"]
            state["agreed"].discard(sender)
            if not hosted and t == "chat.decline" and state["state"] == "invited":
                state["state"] = "declined"
                state["close_cause"] = "declined"
        elif t in ("chat.accept", "chat.message") and state["state"] in ("invited", "open"):
            state["state"] = "open"
        elif t in ("chat.close-request", "chat.close-ack"):
            state["agreed"].add(sender)
            state["agreed_at"].setdefault(sender, e["created_at"])
            state["close_requested_by"] = state["close_requested_by"] or sender
            state["state"] = "closing"
        if t == "chat.message":
            state["messages"] += 1
        remaining = [m for m in state["members"] if m not in state["left"]]
        if not hosted and state["state"] == "closing" and remaining and all(m in state["agreed"] for m in remaining):
            state["state"] = "closed"
            # A quorum close names the member whose agreement completed it;
            # a departure that leaves everyone remaining agreed names nobody.
            if t in ("chat.close-request", "chat.close-ack"):
                state["closed_by"] = state["closed_by"] or sender
                state["close_cause"] = "quorum"
            else:
                state["close_cause"] = "departure"
        if not hosted and state["state"] in ("invited", "open", "closing") and len(remaining) < 2 and state["left"]:
            state["state"] = "closed"
            state["close_cause"] = "departure"
    state["parties"] = set(state["members"])
    return state


def all_chats() -> dict[str, dict]:
    grouped: dict[str, list] = {}
    for e in chat_events():
        grouped.setdefault(e.get("correlation_id") or "", []).append(e)
    return {cid: chat_state(evs) for cid, evs in grouped.items() if cid}


def my_chats() -> dict[str, dict]:
    return {cid: st for cid, st in all_chats().items() if me() in st["members"]}


def recipients_of(e: dict, chats: dict[str, dict]) -> list[str]:
    """Who a chat event is for, as routed when it happened."""
    st = chats.get(e.get("correlation_id") or "")
    if st is None:
        return []
    return list(st["routes"].get(e["id"], []))


def visible_history(events: list[dict], st: dict, handle: str) -> list[dict]:
    """Shared context through participation, excluding rejected attempts."""
    if handle not in st["members"]:
        return []
    cutoff = None
    if handle in st["left"]:
        cutoff = next((e["id"] for e in reversed(events) if e["id"] in st["accepted"]
                       and e.get("from_agent") == handle and e["type"] in ("chat.leave", "chat.decline")), None)
    visible = []
    departed = False
    for e in events:
        if e["id"] in st["accepted"] and (not departed or e.get("from_agent") == handle):
            visible.append(e)
        if e["id"] == cutoff:
            departed = True
    return visible


def participation_state(st: dict, handle: str) -> str:
    """Project a local departure without relabeling legacy terminal facts."""
    if handle in st["left"] and (st["hosted"] or st["state"] in ("invited", "open", "closing")):
        return "left"
    return st["state"]


def open_chat_with(*peers: str) -> str | None:
    wanted = set(peers)
    for cid, st in my_chats().items():
        others = {m for m in st["members"] if m != me() and m not in st["left"]}
        if others == wanted and st["state"] in ("invited", "open", "closing") and me() not in st["left"]:
            return cid
    return None


def other_members(state: dict | None) -> list[str]:
    return [p for p in state["members"] if p != me() and p not in state["left"]] if state else []


def peer_of(state: dict) -> str:
    return ", ".join(other_members(state)) or "?"


def require_recipient(cid: str, st: dict | None) -> None:
    if not other_members(st):
        raise SystemExit(f"[{cid}] no other participants remain; nothing to send")


def new_chat_id() -> str:
    return f"chat-{secrets.token_hex(4)}"


def chat_id_for(opener_fp: str, eid: str) -> str:
    """A sealed chat's id is derived from the key that opened it and its
    invitation's box id, so the chat's identity is the opener's: no other
    key can open, re-open or claim a chat under that id, on any replica,
    however its invitation is dated."""
    return "chat-" + hashlib.sha256(f"{opener_fp}:{eid}".encode()).hexdigest()[:12]


def require_live_member(cid: str, st: dict, action: str) -> None:
    if st["state"] in ("closed", "declined"):
        raise SystemExit(f"[{cid}] is {st['state']}; nothing to {action}")
    if me() in st["left"] or me() not in st["members"]:
        raise SystemExit(f"[{cid}] you are not in this chat")


def resolve_chat(argument: str | None, text: list[str] | None = None) -> str:
    """A chat id, a participating member's handle, or the only live chat.

    A first word that names neither is the start of the message, so
    `a2a end thanks all` works when there is one chat to end.
    """
    chats = my_chats()
    if argument:
        if argument in chats:
            return argument
        with_member = {cid: st for cid, st in chats.items() if argument in st["members"]}
        if with_member:
            live_with = [cid for cid, st in with_member.items() if argument not in st["left"] and st["state"] in ("invited", "open", "closing") and me() not in st["left"]]
            if len(live_with) == 1:
                return live_with[0]
            if not live_with:
                raise SystemExit(f"no live chat with '{argument}' (try: `a2a status`)")
            raise SystemExit(f"several open chats with '{argument}'; name one: " + ", ".join(live_with))
        if text is not None:
            text.insert(0, argument)
        else:
            raise SystemExit(f"no chat '{argument}' (try: `a2a status`)")
    live = [cid for cid, st in chats.items() if st["state"] in ("invited", "open", "closing") and me() not in st["left"]]
    if len(live) == 1:
        return live[0]
    if not live:
        raise SystemExit('no open chat; use `a2a chat PEER "Hello"` to start one, or `a2a status --all` for history')
    raise SystemExit("several open chats; name one: " + ", ".join(live))


# ── Inbox ───────────────────────────────────────────────────────────────────


def inbox_cursor_name() -> str:
    return f"inbox-{re.sub(r'[^A-Za-z0-9._-]', '_', me())}.json"


_LEGACY_ACK_RECIPIENT_RE = re.compile(r"\b(?:delivered to|uncertain for) (\S+)")


def courier_ack_recipient(a: dict) -> str | None:
    payload = _ack_fields(a)
    recipient = payload.get("recipient") if payload else None
    if recipient is None and a.get("id") in legacy_ack_ids():
        m = _LEGACY_ACK_RECIPIENT_RE.search(a.get("body") or "")
        recipient = m.group(1) if m else None
    return recipient if isinstance(recipient, str) and recipient else None


def seen_ids(since: str | None) -> set[str]:
    """Events I acknowledged, or a courier already delivered to me.

    An ack routes back to the message's writer, so a courier names the
    recipient in its body. Only acknowledgements a host's key vouches for
    count: an injected one moves no cursor."""
    # Every acknowledgement counts, wherever the cursor stands: an event
    # read long ago that comes again under a new relay row is matched to
    # its old acknowledgement by the box's identity, not by row order.
    seen = set()
    verified, _, bindings = verified_chats()
    index = scope_index(verified)
    for a in verified_acks(list_events(room=ROOM_CHAT, type="event.ack"), bindings):
        writer = a.get("from_agent") or ""
        target = ack_target(a, index)
        if writer == me():
            seen.add(target)
        elif writer.startswith("courier@") and a.get("status") == "ready" and courier_ack_recipient(a) == me():
            seen.add(target)
    return seen


def addressed_to_me(events: list[dict], chats: dict[str, dict] | None = None) -> list[dict]:
    chats = chats if chats is not None else my_chats()
    return [e for e in events if e["type"] in CHAT_TYPES and e.get("from_agent") != me() and me() in recipients_of(e, chats)]


def unread(since: str | None = None, chat_id: str | None = None, include_left: bool = False) -> list[dict]:
    cursor = since or read_state(inbox_cursor_name()).get("cursor")
    filters = {"room": ROOM_CHAT, "stream": STREAM, "limit": PAGE}
    if chat_id:
        filters["correlation_id"] = chat_id
    chats = my_chats()
    events = [e for e in addressed_to_me(_paged_events_relaxed(filters, cursor), chats)
              if include_left or me() not in chats[e["correlation_id"]]["left"]]
    if not events:
        return []
    seen = seen_ids(cursor)
    return [e for e in events if e["id"] not in seen]


def acknowledge_chat(cid: str) -> list[str]:
    """Acting in a chat means I have read what reached me in it, courier
    delivery or not: acknowledge every verified event routed to me that I
    have not acknowledged myself, so the ledger can say `read`. Only what
    this host could verify is acknowledged; an injected event never is."""
    events, _, bindings = verified_chats(cid)
    acks = verified_acks(list_events(room=ROOM_CHAT, correlation_id=cid, type="event.ack"), bindings)
    index = scope_index(events)
    mine = {ack_target(a, index) for a in acks if a.get("from_agent") == me()}
    chats = {cid: chat_state(events)}
    pending = [e for e in addressed_to_me(events, chats) if e["id"] not in mine]
    return mark_read(pending)

def wake_limits(peers: list[str], known: dict[str, dict]) -> list[str]:
    """One note per idle peer whose harness has no receive path that wakes
    an idle session. A busy peer is surfaced by its stop hook at turn end;
    only a peer that is already idle, on a harness whose only native
    receive path rides a lifecycle hook, waits for its user's next
    turn. Scan-only rows (state online, via courier) cannot tell idle
    from busy and stay silent rather than guess."""
    notes = []
    for peer in peers:
        info = known.get(peer) or {}
        if info.get("state") != "idle":
            continue
        harness = info.get("harness")
        if harness == "claude" and info.get("inbox_socket"):
            continue  # the courier's socket rung wakes an idle session
        if harness in ("claude", "kimi"):
            notes.append(f"note: {peer} is idle on {harness}; it has no wake while idle and reads this at its next turn")
        elif harness == "codex" and not info.get("native_session"):
            notes.append(f"note: {peer} has no announced codex thread; it sees this message only through an explicit `a2a inbox` or `a2a wait`")
    return notes


def mark_read(events: list[dict]) -> list[str]:
    """Acknowledge in order; return the ids that failed.

    Callers pass subsets (one chat, a few ids), so the cursor is not moved
    to the last of them. It advances only across the prefix of the whole
    inbox in which every event is already seen, so an older unread message
    in another chat is never skipped.
    """
    failed = []
    for e in events:
        if failed:
            failed.append(e["id"])
            continue
        try:
            ack(e, status="applied")
        except LogstreamError:
            failed.append(e["id"])
    advance_cursor()
    return failed


def advance_cursor() -> None:
    cursor = read_state(inbox_cursor_name()).get("cursor")
    filters = {"room": ROOM_CHAT, "stream": STREAM, "limit": PAGE}
    verified, _, _ = verified_chats()
    verified_ids = {e["id"] for e in verified}
    chats = my_chats()
    seen = seen_ids(cursor)
    new_cursor = None
    # The cursor passes an event only once it is settled: verified and
    # seen, or unverifiable and older than the hold. A key or presence
    # that has not replicated yet leaves the event behind the cursor, so
    # it is read once they arrive; junk that never verifies is passed
    # after the hold, so the cursor is never pinned forever.
    for e in _paged_events_relaxed(filters, cursor):
        if e["type"] not in CHAT_TYPES:
            continue
        if e["id"] not in verified_ids:
            if _age(e) < HOLD_S:
                break
            continue
        if e.get("from_agent") != me() and me() in recipients_of(e, chats):
            if e["id"] not in seen:
                break
            new_cursor = e["id"]
    if new_cursor:
        write_state(inbox_cursor_name(), {"cursor": new_cursor, "at": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())})


def _report_unacked(failed: list[str]) -> None:
    if failed:
        raise SystemExit(f"a2a: {len(failed)} message(s) could not be acknowledged and stay unread")


def human_text(value, *, layout=True) -> str:
    """Keep text layout, but make terminal control characters visible."""
    out = []
    for char in str(value):
        code = ord(char)
        if (layout and char in "\n\t") or char.isprintable():
            out.append(char)
        elif char in "\r\n\t":
            out.append({"\r": "\\r", "\n": "\\n", "\t": "\\t"}[char])
        else:
            out.append(f"\\x{code:02x}" if code < 256 else (f"\\u{code:04x}" if code <= 0xffff else f"\\U{code:08x}"))
    return "".join(out)


class ProseWrapper(textwrap.TextWrapper):
    def _split(self, text):
        # Inline command examples remain copyable across narrow error displays.
        return re.findall(r"`[^`]*`|[^\s`]+|\s+|`", text)


def human_prose(value, *, indent="", continuation="") -> str:
    wrapper = ProseWrapper(width=terminal_width(), initial_indent=indent,
                           subsequent_indent=continuation, break_long_words=False,
                           break_on_hyphens=False)
    return wrapper.fill(human_text(value, layout=False))


def human_fields(fields: list[tuple[str, object]]) -> str:
    return "\n".join(human_prose(f"{label}: {value}", indent="  ", continuation="    > ")
                     for label, value in fields)


def human_event(e: dict) -> str:
    # Native hook rendering remains separate; do not strip or reflow bodies.
    who, cid = e.get("from_agent"), e.get("correlation_id")
    kind = e["type"].split(".", 1)[1]
    meta = metadata_of(e)
    actions = {
        "message": "Message", "accept": "Accepted the chat",
        "decline": "Declined the chat", "leave": "Left the chat",
        "close-request": "Proposed ending the chat under the legacy agreement rules",
        "close-ack": "Agreed to end the chat", "closed": "Closed the chat",
    }
    action = actions.get(kind, kind)
    if kind == "invite":
        others = [m for m in (meta.get("members") or [e.get("to_agent")]) if m and m != who]
        action = "Opened a chat with " + ", ".join(others)
    elif kind == "join":
        action = "Added " + str(meta.get("member"))
    body, unreadable = open_body(e)
    chat = f"[{human_text(cid, layout=False)}]"
    when = human_text(e.get("created_at", "?"), layout=False)
    heading = chat + "  " + when
    if len(heading) > terminal_width():
        heading = chat + "\n" + when
    header = heading + "\n" + human_prose(f"{who}: {action}", continuation="  ")
    if unreadable:
        return header + "\n  Unreadable sealed body: " + human_text(unreadable) + "\n"
    return header + ("\n---\n" + human_text(body) + "\n---\n" if body else "\n")


def human_unreadable(u: dict) -> str:
    return human_fields([("Chat", u["chat"]), ("Event", u.get("id", "?")),
                         ("When", u.get("at", "?")),
                         ("Unreadable", f"{u['type'].split('.', 1)[1]} claiming to be from {u['from']} (unverified)"),
                         ("Reason", u["reason"])]) + "\n"


def render(e: dict) -> str:
    who = e.get("from_agent")
    cid = e.get("correlation_id")
    t = e["type"].split(".", 1)[1]
    body, unreadable = open_body(e)
    body = body.strip()
    if unreadable:
        return f"[{cid}] {who} {t}: (unreadable sealed body: {unreadable})"
    meta = metadata_of(e)
    if t == "invite":
        others = [m for m in (meta.get("members") or [e.get("to_agent")]) if m and m != who]
        return f"[{cid}] {who} opened a chat with {', '.join(others)}: {body}"
    if t == "message":
        return f"[{cid}] {who}: {body}"
    if t == "accept":
        return f"[{cid}] {who} accepted the chat"
    if t == "decline":
        return f"[{cid}] {who} declined the chat" + (f": {body}" if body else "")
    if t == "join":
        return f"[{cid}] {who} added {meta.get('member')}"
    if t == "leave":
        return f"[{cid}] {who} left the chat" + (f": {body}" if body else "")
    if t == "close-request":
        return f"[{cid}] {who} proposed ending the chat under the legacy agreement rules" + (f": {body}" if body else "")
    if t == "close-ack":
        return f"[{cid}] {who} agreed to end the chat"
    return f"[{cid}] {who} {t}" + (f": {body}" if body else "")


# ── Ledger export (what the console may read) ───────────────────────────────

WAITING_AFTER_S = COURIER_MAX_ATTEMPTS * COURIER_RETRY_S


def delivery_states(events: list[dict], routes: dict, bindings: dict[str, dict[str, str]] | None = None) -> dict[str, dict[str, str]]:
    """Per event, per recipient: sent | delivered | uncertain | read. Only
    acknowledgements a host's key vouches for change a state."""
    states = {e["id"]: {r: "sent" for r in routes.get(e["id"], [])} for e in events if e["type"] in CHAT_TYPES}
    index = scope_index(events)
    for a in verified_acks(events, bindings):
        target = ack_target(a, index)
        if target not in states:
            continue
        writer = a.get("from_agent") or ""
        if writer.startswith("courier@"):
            recipient = courier_ack_recipient(a)
            if recipient in states[target] and states[target][recipient] != "read":
                states[target][recipient] = "delivered" if a.get("status") == "ready" else "uncertain"
        elif writer in states[target]:
            states[target][writer] = "read"
    return states


ROW_KINDS = {"chat.invite": "message", "chat.message": "message", "chat.join": "added", "chat.leave": "left", "chat.accept": "accepted", "chat.decline": "declined", "chat.close-request": "close-request", "chat.close-ack": "close-request", "chat.closed": "forced"}


def ledger(handle: str) -> dict:
    """Every chat `handle` is or was a member of, folded for a reader."""
    grouped: dict[str, list] = {}
    verified_events, _, bindings = verified_chats()
    # A chat event is a row only as this host verified and ordered it;
    # acknowledgements ride along for the delivery states.
    for e in verified_events:
        grouped.setdefault(e.get("correlation_id") or "", []).append(e)
    for a in list_events(room=ROOM_CHAT, type="event.ack"):
        grouped.setdefault(a.get("correlation_id") or "", []).append(a)
    known = presence_harnesses()
    chats = []
    for cid, evs in grouped.items():
        if not cid:
            continue
        st = chat_state([e for e in evs if e["type"] in CHAT_TYPES])
        if handle not in st["members"]:
            continue
        states = delivery_states(evs, st["routes"], bindings)
        rows = []
        for e in visible_history(evs, st, handle):
            if e["type"] not in ROW_KINDS or ROW_KINDS[e["type"]] in ("close-request", "forced", "accepted"):
                continue  # acks and the handshake are not rows
            meta = metadata_of(e)
            text, unreadable = open_body(e)
            row = {
                "id": e["id"], "tag": "a2a" + delivery_tag(e), "kind": ROW_KINDS[e["type"]],
                "from": e["from_agent"], "member": meta.get("member") if e["type"] == "chat.join" else None,
                "text": inert(text) if e["type"] in ("chat.invite", "chat.message", "chat.leave", "chat.decline") else "",
                "at": e["created_at"], "to": states.get(e["id"], {}),
            }
            if unreadable:
                row["unreadable"] = unreadable
            rows.append(row)
        remaining = [m for m in st["members"] if m not in st["left"]]
        handshake = [{"member": m, "agreed": m in st["agreed"], "at": st["agreed_at"].get(m), "left": m in st["left"], "left_at": st["left_at"].get(m)} for m in st["members"]] if not st["hosted"] and st["state"] in ("closing", "closed") else []
        state = participation_state(st, handle)
        if state == "closed" and st["forced"]:
            state = "forced"
        waiting_s = None
        if state == "invited":
            invite = next((e for e in evs if e["type"] == "chat.invite"), None)
            if invite and _age(invite) >= WAITING_AFTER_S and all(v == "sent" for v in states.get(invite["id"], {}).values()):
                state, waiting_s = "waiting", _age(invite)
        chats.append({
            "id": cid, "state": state, "waiting_s": waiting_s, "forced": st["forced"],
            "opened_by": st["opened_by"], "opened_at": st["opened_at"], "last": st["last"],
            "participation_at": st["left_at"].get(handle),
            "members": st["members"], "left": sorted(st["left"]), "remaining": remaining,
            "harnesses": {m: harness_of_handle(m, known) for m in st["members"]},
            "close_requested_by": st["close_requested_by"], "closed_by": st["closed_by"], "close_cause": st["close_cause"], "messages": st["messages"],
            "rows": rows, "handshake": handshake,
            "unreadable": unreadable_events(cid),
        })
    chats.sort(key=lambda c: c["last"] or "", reverse=True)
    return {"agent": handle, "chats": chats}


def ledger_index() -> dict:
    """Every chat on the palace, content-free: id, state, members, and the
    id of the last row, so a console can mark which sessions are in a
    live chat without reading any body. sessions maps each Deck session
    presence names to its agent's effective handle, so the marker and the
    per-session ledger resolve a session's identity the same way."""
    grouped: dict[str, list] = {}
    for e in chat_events():
        grouped.setdefault(e.get("correlation_id") or "", []).append(e)
    chats = []
    for cid, evs in grouped.items():
        if not cid:
            continue
        st = chat_state(evs)
        if not st["members"]:
            continue
        state = "forced" if st["state"] == "closed" and st["forced"] else st["state"]
        rows = [e["id"] for e in evs if e["type"] in ROW_KINDS and ROW_KINDS[e["type"]] not in ("close-request", "forced", "accepted") and e["id"] in st["accepted"]]
        chats.append({"id": cid, "state": state, "members": st["members"], "left": sorted(st["left"]), "last": st["last"], "last_row": rows[-1] if rows else None, "rows": len(rows)})
    chats.sort(key=lambda c: c["last"] or "", reverse=True)
    return {"chats": chats, "sessions": session_handles()}


def presence_harnesses() -> dict[str, str]:
    """Every handle vouched presence has named, with the harness it last ran on."""
    derived = _presence_state_refresh()
    return dict(derived["harness"])


def harness_of_handle(handle: str, known: dict[str, str] | None = None) -> str:
    """The harness an agent runs on, as its presence says.

    Presence events carry both the handle and the harness, so a chosen
    name resolves the same way a default one does. Without any presence
    for the handle, the default handle shape, harness then tag, is read;
    a handle that fits neither is `unknown`, never a guess."""
    if known is None:
        known = presence_harnesses()
    if handle in known:
        return known[handle]
    prefix = handle.rsplit("-", 1)[0] if "-" in handle else handle
    return prefix if prefix in set(HARNESS_COMMS.values()) else "unknown"


def session_handles() -> dict[str, str]:
    """deck_session -> the handle its agent last announced, from presence.

    The resolution handle_for_session applies, folded once for every
    session presence names: the newest presence event for a session names
    its agent, chosen name or default, whether or not the agent is still
    reachable."""
    derived = _presence_state_refresh()
    return dict(derived["session_handle"])


def handle_for_session(deck_session: str, harness: str) -> str:
    """The effective handle of the agent running under a Deck session.

    An agent may have chosen a name with `a2a name`; its presence events
    carry both the chosen handle and the session id. The newest presence
    event ever written for that session names the agent, whether or not
    the agent is still reachable: a stopped session keeps its ledger.
    Without one, the handle is the default the agent itself derives: the
    harness name and the session id's first four characters."""
    return session_handles().get(deck_session) or f"{harness}-{handle_tag(deck_session, 0)}"


def cmd_ledger(args) -> None:
    if args.probe:
        _rpc("mempalace_event_list", {"stream": STREAM, "room": ROOM_PRESENCE, "limit": 1})
        return
    snapshot_events()
    try:
        if args.session:
            if not args.harness:
                raise SystemExit("--for-session needs --harness")
            handle = handle_for_session(args.session, args.harness)
            print(json.dumps(ledger(handle), indent=None if args.compact else 2))
            return
        if args.index:
            print(json.dumps(ledger_index(), indent=None if args.compact else 2))
            return
        handle = args.handle or me()
        print(json.dumps(ledger(handle), indent=None if args.compact else 2))
    finally:
        release_events()


# ── Commands ────────────────────────────────────────────────────────────────


def cmd_whoami(args) -> None:
    print(me())


def cmd_name(args) -> None:
    courier_ensure()
    set_handle(args.handle)
    announce("online")
    print(f"you are now {args.handle}")


def cmd_up(args) -> None:
    courier_ensure()
    announce("online")
    print(f"{me()} online")


def cmd_down(args) -> None:
    announce("offline")
    print(f"{me()} offline")


def cmd_key(args) -> None:
    if args.key_command == "init":
        print(key_init())
        return
    if args.key_command == "show":
        key = require_host_key()
        print(key.fingerprint)
        if args.public:
            print(base64.b64encode(key.public).decode())
        return
    if args.key_command == "announce":
        event = announce_key()
        print(f"announced {metadata_of(event)['fingerprint']} for {socket.gethostname()}")
        return
    if args.key_command == "trust":
        fp = args.fingerprint.strip().lower()
        if len(fp.replace("-", "")) != 24:
            raise SystemExit("a2a: a fingerprint is 24 characters in groups of four, as `a2a key show` prints it")
        pin_host(args.host, fp)
        st = host_key_status(args.host)
        print(f"pinned {args.host} {fp} ({st['status']})")
        return
    if args.key_command == "migrate":
        try:
            done = key_migrate()
        except LogstreamError as exc:
            raise SystemExit(f"a2a: the relay did not answer, so the migration is incomplete; run `a2a key migrate` again once it is up ({exc})")
        if done["migrated"]:
            print(f"replaced the pre-sealing key with a sealing key, fingerprint {done['fingerprint']}; read it to your peers, who re-pin with `a2a key trust {socket.gethostname()} {done['fingerprint']}`")
        if done["announced"]:
            print(f"announced {done['fingerprint']} for {socket.gethostname()}")
        print(f"recorded {done['recorded']} pre-sealing chat event(s) as this host's readable history")
        return
    if args.key_command == "history":
        if not args.none and not args.source:
            raise SystemExit("choose `a2a key history --none` or `a2a key history --from FILE`")
        count = key_history(Path(args.source) if args.source else None)
        print(f"recorded {count} pre-sealing chat event(s) as this host's readable history")
        return
    if args.key_command == "status":
        announced, trust = announced_keys(), read_trust()
        hosts = sorted(set(announced) | set(trust) | {socket.gethostname()})
        for host in hosts:
            st = host_key_status(host, announced, trust)
            if st["status"] == "self":
                mine = load_host_key()
                fp = mine.fingerprint if mine else ("(pre-sealing key; run `a2a key migrate`)" if host_key_format() == "x25519" else "(no key; run `a2a key init`)")
                print(human_fields([("Host", json.dumps(host, ensure_ascii=False)), ("Trust", "self"), ("Fingerprint", fp)]) + "\n")
                continue
            print(human_fields([("Host", json.dumps(host, ensure_ascii=False)), ("Trust", st["status"]), ("Announced", st.get("fingerprint") or "—"), ("Pinned", st.get("pinned") or trust.get(host) or "—")]) + "\n")
        return
    raise SystemExit("choose a host-key action; use `a2a key --help`")


def cmd_who(args) -> None:
    courier_ensure()
    scope = resolve_project_scope(args.project)
    rows = scoped_roster(roster(), scope)
    if args.json:
        print(json.dumps(rows, indent=2))
        return
    if not rows:
        print(human_prose("No agents online in the selected project. For another known project, use `a2a who --project NAME`."))
        return
    announced, trust = announced_keys(), read_trust()
    print("Reachable agents\n")
    for handle, meta in sorted(rows.items(), key=lambda kv: kv[1]["age_s"]):
        key = host_key_status(meta.get("host") or "", announced, trust)["status"]
        print(human_text(handle, layout=False) + (" (you)" if handle == me() else ""))
        name = meta.get("display_name")
        fields = [("Name", json.dumps(name, ensure_ascii=False) if isinstance(name, str) and name else "—"),
                  ("Harness", json.dumps(meta.get("harness", "?"), ensure_ascii=False)),
                  ("Host", json.dumps(meta.get("host", "?"), ensure_ascii=False)),
                  ("Project", json.dumps(meta.get("project", "?"), ensure_ascii=False)), ("State", meta["state"]),
                  ("Trust", key), ("Seen", f"{meta['age_s']}s ago")]
        # Pair short fields only when both fit; never truncate routing identifiers.
        while fields:
            label, value = fields.pop(0)
            line = f"  {label}: {human_text(value, layout=False)}"
            if fields:
                next_label, next_value = fields[0]
                pair = line + f"  |  {next_label}: {human_text(next_value, layout=False)}"
                if len(pair) <= terminal_width():
                    fields.pop(0)
                    line = pair
            print(textwrap.fill(line, terminal_width(), subsequent_indent="    > ",
                                break_long_words=False, break_on_hyphens=False))
        print()


def _send(chat_id: str, text: str, type_: str = "chat.message", status: str = "open", to: str | None = None, selected_member: str | None = None, **meta) -> dict:
    """Address a pair to the peer's handle and a room to its chat id."""
    meta.setdefault("host", socket.gethostname())
    meta["lifecycle"] = "hosted"
    # The chat's current membership is what decides who reads this event,
    # so it is read for every type, not only for messages.
    st = all_chats().get(chat_id)
    if selected_member and (not st or selected_member not in st["members"] or selected_member in st["left"]):
        raise SystemExit(f"[{chat_id}] {selected_member} is no longer participating; name a chat or active member")
    if type_ == "chat.message":
        require_recipient(chat_id, st)
        require_live_member(chat_id, st, "reply to")
    # Every event that leaves this host carries text or names a member; the
    # hosts that will read it must all be pinned before anything is appended.
    if type_ == "chat.invite":
        recipients = list(meta.get("members") or [])
    elif st:
        recipients = other_members(st)
    else:
        recipients = [to] if to and to != chat_id else []
    if type_ == "chat.join" and meta.get("member"):
        recipients = recipients + [meta["member"]]
    require_history_recorded()
    binding = chat_binding(chat_id) if st else None
    hosts = require_pinned_hosts([r for r in recipients if r and r != me()], binding=binding)
    refuse_credential_shapes(text)
    # A chat's bodies are sealed between its two hosts for its whole life:
    # a message while the other host has nobody active still seals to it,
    # so a member who joins there later reads the shared history whole,
    # and a lone opener's close reads there too.
    for other in (binding[0] or set()) if binding else set():
        if other != socket.gethostname() and other not in hosts:
            hosts[other] = require_host_pinned(other, f"the other host of chat {chat_id}")
    eid = None
    if type_ == "chat.invite":
        # The chat's id is the opener's: derived from this host's key and
        # the invitation's box id, so nobody else can claim it. A pair is
        # addressed to the peer, a room to the chat.
        eid = secrets.token_hex(16)
        chat_id = chat_id_for(require_host_key().fingerprint, eid)
        if to is None:
            to = chat_id
    elif to is None:
        others = other_members(st)
        to = others[0] if len(others) == 1 else chat_id
    body, meta["enc"] = seal_body(text, type_, chat_id, to, hosts, meta, after=chat_frontier(chat_id) if st else [], eid=eid)
    event = append(type_, ROOM_CHAT, to=to, correlation_id=chat_id, body=body, status=status, metadata=meta)
    if type_ == "chat.message":
        # The membership snapshot above is not a transaction. Verify the
        # committed event's route, not the chat's possibly newer membership.
        try:
            committed = all_chats().get(chat_id)
        except LogstreamError:
            raise SystemExit(f"[{chat_id}] message appended, but its acceptance could not be confirmed; inspect `a2a read {chat_id}` before retrying")
        if not committed or event["id"] not in committed["routes"]:
            raise SystemExit(f"[{chat_id}] message appended, but its acceptance could not be confirmed; inspect `a2a read {chat_id}` before retrying")
        if not committed["routes"][event["id"]]:
            raise SystemExit(f"[{chat_id}] message was not accepted for any recipient; nothing sent")
    return event


def cmd_chat(args) -> None:
    courier_ensure()
    peers = [p for p in args.peers.split(",") if p]
    if not peers or me() in peers:
        raise SystemExit("name the peer(s), comma-separated: `a2a chat bob,carol \"...\"`")
    text = " ".join(args.text).strip() or (sys.stdin.read().strip() if not sys.stdin.isatty() else "")
    if not text:
        raise SystemExit('message text is required; use `a2a chat PEER "Hello"` or pipe text on stdin')
    scope = resolve_project_scope(args.project)
    known = roster()
    offline = [p for p in peers if p not in known]
    if offline and not args.force:
        raise SystemExit(f"{', '.join(offline)} not online (`a2a who`); use --force to leave a message anyway")
    require_pinned_hosts(peers, known)
    require_project_recipients(scope, peers, known, force=args.force)
    cid = open_chat_with(*peers)
    if cid:
        acknowledge_chat(cid)
        _send(cid, text)
        print(f"[{cid}] sent to {', '.join(peers)}")
    else:
        event = _send(new_chat_id(), text, type_="chat.invite", to=(peers[0] if len(peers) == 1 else None), members=[me()] + peers)
        cid = event["correlation_id"]
        print(f"[{cid}] chat opened with {', '.join(peers)}")
    for note in wake_limits(peers, known):
        print(note)
    if args.wait:
        _wait(cid, args.wait)
    else:
        print("(native receive hooks may notify you; use a2a wait or a2a inbox to receive replies)")


def cmd_reply(args) -> None:
    courier_ensure()
    text = list(args.text)
    cid = resolve_chat(args.chat, text)
    body = " ".join(text).strip() or (sys.stdin.read().strip() if not sys.stdin.isatty() else "")
    if not body:
        raise SystemExit('message text is required; use `a2a reply CHAT_ID "Hello"` or pipe text on stdin')
    st = my_chats()[cid]
    require_live_member(cid, st, "reply to")
    require_recipient(cid, st)
    acknowledge_chat(cid)
    _send(cid, body, selected_member=args.chat if args.chat in st["members"] else None)
    print(f"[{cid}] sent to {peer_of(st)}")
    for note in wake_limits(other_members(st), roster()):
        print(note)
    if args.wait:
        _wait(cid, args.wait)


def cmd_accept(args) -> None:
    cid = resolve_chat(args.chat, args.text)
    require_live_member(cid, my_chats()[cid], "accept")
    acknowledge_chat(cid)
    _send(cid, " ".join(args.text), type_="chat.accept", status="claimed")
    print(f"[{cid}] accepted")


def cmd_decline(args) -> None:
    _leave(args, decline=True)


def cmd_add(args) -> None:
    handles = list(args.handle)
    cid = resolve_chat(args.chat, handles)
    if not handles:
        raise SystemExit("member handles are required; use `a2a add CHAT_ID HANDLE [HANDLE ...]`")
    st = my_chats()[cid]
    require_live_member(cid, st, "add to")
    pending = []
    for handle in handles:
        if not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9._-]{0,63}", handle):
            raise SystemExit(f"'{handle[:24]}' is not a handle: letters, digits, '.', '_', '-' only, max 64")
        if handle == cid:
            raise SystemExit(f"[{cid}] cannot add the chat id as a member handle")
        if handle in st["members"] and handle not in st["left"]:
            print(f"[{cid}] {handle} is already in the chat")
            continue
        if handle in st["left"]:
            raise SystemExit(f"[{cid}] {handle} left this chat; start a new chat to invite them again")
        # Every body so far was sealed to the hosts the chat's keys fixed
        # at its first sealed event; a member on a host outside that set
        # could read none of it, and no host can re-seal history for them.
        # The set stays fixed whoever has left since, and presence, which
        # anyone writes, does not place the newcomer: their own host's
        # key does.
        chat_hosts, handle_hosts = chat_binding(cid)
        newcomer = handle_hosts.get(handle) or authenticated_hosts().get(handle)
        if chat_hosts is not None:
            if not newcomer:
                raise SystemExit(f"[{cid}] {handle} has no host any key vouches for (never seen online from a pinned host), so they cannot be placed in this chat; nothing was sent")
            if newcomer not in chat_hosts:
                raise SystemExit(f"[{cid}] {handle} is on {newcomer}, and this chat's hosts are {', '.join(sorted(chat_hosts))}; a chat's hosts are fixed when it opens, so start a new chat that includes them from the first message")
        if handle not in pending:
            pending.append(handle)
    if pending:
        scope = resolve_project_scope(args.project)
        require_project_recipients(scope, pending, roster(), force=True)
    for handle in pending:
        _send(cid, "", type_="chat.join", status="claimed", to=cid, member=handle)
        print(f"[{cid}] added {handle}")


def cmd_leave(args) -> None:
    _leave(args)


def _leave(args, decline: bool = False) -> None:
    cid = resolve_chat(args.chat, args.text)
    st = my_chats()[cid]
    if me() in st["left"]:
        print(f"[{cid}] you already left")
        return
    require_live_member(cid, st, "leave")
    if me() == st["opened_by"]:
        raise SystemExit(f"[{cid}] you opened this chat; use `a2a end {cid}` to end it for everyone")
    acknowledge_chat(cid)
    try:
        _send(cid, " ".join(args.text), type_="chat.decline" if decline else "chat.leave", status="failed" if decline else "superseded")
        after = my_chats()[cid]
    except LogstreamError as exc:
        command = "decline" if decline else "leave"
        raise LogstreamError(f"could not confirm departure; retry `a2a {command} {cid}`") from exc
    if me() in after["left"]:
        print(f"[{cid}] {'declined' if decline else 'you left'}")
    else:
        print(f"[{cid}] already {after['state']}")


def cmd_end(args) -> None:
    cid = resolve_chat(args.chat, args.text)
    st = my_chats()[cid]
    why = " ".join(args.text)
    if st["state"] in ("closed", "declined"):
        print(f"[{cid}] already {st['state']}")
        return
    if st["opened_by"] != me():
        # Resolve once: the first word may have been a reason, not a chat id.
        args.chat = cid
        _leave(args)
        return
    # Legacy openers can have left a still-live chat; end does not rejoin them.
    acknowledge_chat(cid)
    try:
        _send(cid, why or "closed", type_="chat.closed", status="superseded", close="opener")
    except LogstreamError as exc:
        raise LogstreamError(f"could not confirm close; retry `a2a end {cid}`") from exc
    print(f"[{cid}] closed for everyone")


def cmd_mark_read(args) -> None:
    wanted = set(args.event_id)
    # An id names a box, not a relay row: a row that has since been
    # replaced as the box's representative still names the box, so the
    # read receipt lands on whichever row the fold keeps now.
    scopes = {scope_str(scope_of(e)) for e in list_events(room=ROOM_CHAT) if e["id"] in wanted}
    # A hand-off that preceded a departure still needs its read receipt.
    events = [e for e in unread(include_left=True) if e["id"] in wanted or scope_str(scope_of(e)) in scopes]
    failed = mark_read(events)
    print(f"{len(events) - len(failed)} marked read")
    _report_unacked(failed)


def cmd_inbox(args) -> None:
    courier_ensure()
    if args.follow:
        # One JSON line of unread mail per relay wake, forever: the
        # streaming peek the extension rungs follow. Nothing is marked
        # read here — the native receipt is the acknowledgement.
        if not args.json:
            raise SystemExit("a2a: --follow prints JSON lines; pair it with --json")
        follow_stream(chat_id=args.chat)
        return
    events = unread(chat_id=args.chat)
    # What could not be read is reported beside what could, never marked
    # read: it is bounded to my chats since the cursor and within the hold.
    failures = unreadable_events(args.chat, my_chats(), read_state(inbox_cursor_name()).get("cursor"))
    if args.json:
        # The JSON view is what extensions deliver from: verified events
        # only. What could not be read is for a person, in the text view.
        print(json.dumps([opened(e) for e in events], indent=2))
    else:
        if not events and not failures:
            print("no unread messages")
        for e in events:
            print(human_event(e))
        for u in failures:
            print(human_unreadable(u))
    if not args.peek:
        _report_unacked(mark_read(events))


def _wait(chat_id: str | None, timeout_s: float) -> list[dict]:
    deadline = time.monotonic() + timeout_s
    durable = read_state(inbox_cursor_name()).get("cursor")
    scan = durable  # moves over traffic that is not mine; the durable cursor does not
    while True:
        remaining = deadline - time.monotonic()
        if remaining <= 0:
            print("nothing arrived (timeout)")
            return []
        # Any room can wake the wait: a key or a presence arriving is what
        # turns an event that could not be verified into one addressed to
        # me, so what is unread is judged afresh from the durable cursor
        # each time, never only from what arrived since the last look.
        filters = {}
        if scan:
            filters["since_event_id"] = scan
        arrived = wait_events(int(min(remaining, 300) * 1000), **filters)
        if arrived:
            scan = arrived[-1]["id"]
            if any(e.get("room") in (ROOM_PRESENCE, ROOM_KEYS) for e in arrived):
                # A dependency arrived: what is placed and which keys are
                # known is read again, not taken from a cache.
                _HOSTS_CACHE.update(at=0.0)
                _PUBLIC_KEYS_CACHE.update(at=0.0)
        chats = my_chats()
        events = [e for e in unread(durable, chat_id) if me() not in chats[e["correlation_id"]]["left"]]
        if events:
            for e in events:
                print(human_event(e))
            _report_unacked(mark_read(events))
            return events


def cmd_wait(args) -> None:
    courier_ensure()
    _wait(args.chat, args.timeout)

def follow_stream(stop=None, out=None, chat_id: str | None = None) -> None:
    """A long-lived peek: one JSON line of unread mail per relay wake.

    The extension rungs consume this instead of polling: one process per
    session blocks on the relay's wait (any room, since a key or presence
    arriving is what turns an unverifiable event into mail), and every
    wake prints exactly what `inbox --peek --json` would — the same
    `opened()` view, nothing marked read, because the native receipt is
    what acknowledges.
    """
    out = out or sys.stdout
    stop = stop or (lambda: False)
    # Slices are short because the relay's wait is a polling loop whose
    # polls can queue behind another client's reads: a fresh wait's first
    # poll is immediate, so re-arming every few seconds bounds detection
    # even where a long slice would stall behind traffic.
    slice_s = max(1.0, float(os.environ.get("A2A_FOLLOW_SLICE_S") or 5))
    backoff_s = max(0.0, float(os.environ.get("A2A_FOLLOW_BACKOFF_S") or 1))
    max_wakes = int(os.environ.get("A2A_FOLLOW_MAX_WAKES") or 0)
    wakes = 0
    # Parent-death binding, armed only when the spawner asks for it: the
    # harness holds this stdin pipe open and never writes, so EOF means it
    # is gone and this process must not linger. A terminal or a test runner
    # owns stdin differently and never sets the marker.
    if os.environ.get("A2A_FOLLOW_PARENT_STDIN") == "1":
        def _die_with_parent() -> None:
            try:
                sys.stdin.buffer.read()
            except Exception:
                pass
            os._exit(0)
        threading.Thread(target=_die_with_parent, daemon=True).start()
    scan = read_state(inbox_cursor_name()).get("cursor")

    def emit() -> None:
        snapshot_events()
        try:
            events = unread(chat_id=chat_id)
        finally:
            release_events()
        out.write(json.dumps([opened(e) for e in events]) + "\n")
        out.flush()

    emit()  # what arrived while nothing was following is still mail
    wakes += 1
    if max_wakes and wakes >= max_wakes:
        return
    while not stop():
        try:
            filters = {"since_event_id": scan} if scan else {}
            arrived = wait_events(int(slice_s * 1000), **filters)
        except LogstreamError as exc:
            if _cursor_refused(exc):
                # A reset palace refuses the cursor: rescan from the whole
                # stream, which the next wake folds into fresh unread.
                scan = None
                continue
            # Downtime is not a reset: pause, keep the state, try again.
            if stop():
                return
            time.sleep(backoff_s)
            continue
        if arrived:
            scan = arrived[-1]["id"]
            if any(e.get("room") in (ROOM_PRESENCE, ROOM_KEYS) for e in arrived):
                # A dependency arrived: what is placed and which keys are
                # known is read again, not taken from a cache.
                _HOSTS_CACHE.update(at=0.0)
                _PUBLIC_KEYS_CACHE.update(at=0.0)
        emit()
        wakes += 1
        if max_wakes and wakes >= max_wakes:
            return


def cmd_read(args) -> None:
    cid = resolve_chat(args.chat)
    events = chat_events(cid)
    st = chat_state(events)
    visible = list(visible_history(events, st, me()))
    for e in visible:
        print(human_event(e))
    for u in unreadable_events(cid):
        print(human_unreadable(u))
    index = scope_index(visible)
    mine = {ack_target(a, index) for a in list_events(room=ROOM_CHAT, correlation_id=cid, type="event.ack")
            if a.get("from_agent") == me() and ack_vouched(a)}
    _report_unacked(mark_read([e for e in addressed_to_me(visible, {cid: st}) if e["id"] not in mine]))


def cmd_status(args) -> None:
    chats = my_chats()
    visible = [(cid, st, participation_state(st, me())) for cid, st in
               sorted(chats.items(), key=lambda kv: kv[1]["last"] or "")]
    visible = [(cid, st, state) for cid, st, state in visible
               if args.all or state not in ("closed", "declined", "left")]
    if not visible:
        print(human_prose(f"{me()}: no chats" if args.all or not chats else f"{me()}: no active chats."))
        if chats and not args.all:
            print("History: a2a status --all")
        return
    for cid, st, state in visible:
        fields = [("Chat", cid), ("With", peer_of(st)), ("State", state),
                  ("Messages", st["messages"]), ("Last", st["last"] or "—")]
        if state == "closing":
            fields.append(("Close proposed by", st["close_requested_by"]))
        print(human_fields(fields) + "\n")


# ── Hooks (harness lifecycle → presence + nudges) ───────────────────────────


def cmd_hook(args) -> None:
    snapshot_events()  # one-shot command: the stop path lists several rooms
    try:
        _cmd_hook(args)
    finally:
        release_events()


def codex_hook_owner(panes: list[dict], payload: dict) -> dict | None:
    """Resolve one Codex pane by thread argument, then by process cwd."""
    panes = [pane for pane in panes if pane.get("harness") == "codex"]
    native = payload.get("session_id") or payload.get("thread_id")
    matches = [pane for pane in panes if native and native in pane.get("cmdline", [])]
    if not matches:
        cwd = payload.get("cwd")
        matches = [pane for pane in panes if cwd and pane.get("cwd") == cwd
                   and codex_resume_session(pane.get("cmdline", [])) in ("", native)]
    return matches[0] if len(matches) == 1 else None


def cached_codex_hook_owner(payload: dict) -> dict | None:
    native = payload.get("session_id") or payload.get("thread_id")
    filename = f"codex-owner-{hashlib.sha256(native.encode()).hexdigest()}.json" if native else None
    cached = read_state(filename) if filename else {}
    owner = cached.get("owner")
    at = cached.get("at", 0)
    if (isinstance(owner, dict) and isinstance(at, (int, float))
            and 0 <= time.time() - at < CODEX_OWNER_TTL_S
            and isinstance(owner.get("pid"), int) and owner["pid"] > 0
            and harness_of_pid(owner["pid"]) == "codex"):
        return owner
    owner = codex_hook_owner(scan_panes(include_process_details=True), payload)
    if filename:
        # Process arguments may contain prompts; cache only presence metadata.
        owner = {k: v for k, v in owner.items() if k not in ("cmdline", "cwd")} if owner else None
        try:
            write_state(filename, {"owner": owner, "at": time.time()})
        except OSError:
            pass
    return owner


def _cmd_hook(args) -> None:
    payload = {}
    if not sys.stdin.isatty():
        try:
            payload = json.loads(sys.stdin.read() or "{}")
        except ValueError:
            payload = {}
    native = payload.get("session_id") or payload.get("thread_id") or ""
    cwd = payload.get("cwd") or os.getcwd()
    owner = cached_codex_hook_owner(payload) if args.harness == "codex" else None
    # Claude Code exports its inbox socket to every hook's environment; every
    # lifecycle event re-announces it so a newer announcement never drops it.
    inbox = os.environ.get("CLAUDE_CODE_MESSAGING_SOCKET", "") if args.harness == "claude" else ""
    try:
        state = {"session-start": "online", "prompt": "busy", "stop": "idle", "session-end": "offline"}[args.event]
        announce(state, harness=args.harness, native_session=native, cwd=cwd, owner=owner, inbox_socket=inbox)
        if args.event == "session-start":
            courier_ensure()
    except LogstreamError as exc:
        print(f"a2a: {exc}", file=sys.stderr)
        return
    if args.event == "session-start":
        if args.harness == "claude":
            print(json.dumps({"hookSpecificOutput": {"hookEventName": "SessionStart", "additionalContext": handle_context()}}))
        elif args.harness != "kimi":
            # Codex lands a hook's stdout as session context. Kimi Code
            # discards SessionStart stdout, so its handle rides the first
            # prompt instead.
            print(handle_context(owner["handle"] if owner else None))
    elif args.event == "prompt" and args.harness == "kimi":
        # Kimi Code appends UserPromptSubmit stdout to the turn's context:
        # mail waiting when the user types reaches the model at the
        # start of this turn, instead of blocking its stop into a second one.
        announced = read_state("announced.json")
        lines = [] if native in announced else [handle_context()]
        fresh, reason, nudge_state = pending_nudge()
        if fresh:
            lines.append(reason)
        if lines:
            print("\n".join(lines))
        if native:
            # Every prompt refreshes its session's marker, so pruning only
            # drops sessions that have not prompted for a week; a session
            # that ends cleanly drops its own marker on session-end.
            try:
                write_state("announced.json", {**{k: v for k, v in announced.items() if time.time() - v < 7 * 86400}, native: time.time()})
            except OSError:
                pass
        if fresh:
            settle_nudges(fresh, nudge_state)
    elif args.event == "prompt" and args.harness == "claude":
        # Claude Code wraps UserPromptSubmit stdout into the turn's context
        # as a bounded reminder block: mail waiting when the user types
        # reaches the model at the start of this turn, instead of blocking
        # its stop into a second one. The handle already rode SessionStart.
        fresh, reason, nudge_state = pending_nudge()
        if fresh:
            print(reason)
            settle_nudges(fresh, nudge_state)
    elif args.event == "session-end" and args.harness == "kimi" and native:
        announced = read_state("announced.json")
        if native in announced:
            try:
                write_state("announced.json", {k: v for k, v in announced.items() if k != native})
            except OSError:
                pass
    elif args.event == "stop" and args.harness in ("claude", "kimi"):
        # Claude Code takes the JSON decision; Kimi Code blocks on exit 2
        # with the reason on stderr. On Pi the extension delivers from the
        # inbox, so marking anything read here would lose it.
        fresh, reason, nudge_state = pending_nudge()
        if fresh:
            if args.harness == "claude":
                for e in fresh:
                    nudge_state[e["id"]] = time.time()
                write_state("nudges.json", {k: v for k, v in nudge_state.items() if time.time() - v < 86400})
                print(json.dumps({"decision": "block", "reason": reason}))
                # The messages are now in the model's context; mark them read.
                mark_read(fresh)
            else:
                print(reason, file=sys.stderr)
                # Kimi continues the turn only when this hook exits 2: no
                # bookkeeping failure after the print may replace that exit.
                settle_nudges(fresh, nudge_state)
                raise SystemExit(2)


def handle_context(handle: str | None = None) -> str:
    return f"a2a chat: your handle is `{handle or me()}`. Other agents can message you; `a2a inbox` shows unread messages, `a2a who` shows who is online."


def pending_nudge() -> tuple[list[dict], str, dict]:
    """Unread events no hook has shown the model yet, rendered with the
    actions each live chat allows, plus the nudge bookkeeping to update."""
    nudge_state = read_state("nudges.json")
    try:
        pending = unread()
        chats = my_chats() if pending else {}
    except LogstreamError:
        pending, chats = [], {}
    fresh = [e for e in pending if e["id"] not in nudge_state]
    if not fresh:
        return [], "", nudge_state
    lines = "\n".join(render(e) for e in fresh)
    chat_ids = {e.get("correlation_id") for e in fresh}
    joined = {e.get("correlation_id") for e in fresh if e["type"] == "chat.join" and metadata_of(e).get("member") == me()}
    guidance = []
    for cid, st in chats.items():
        if cid not in chat_ids or st["state"] not in ("invited", "open", "closing") or me() in st["left"]:
            continue
        if cid in joined:
            guidance.append(f"Read your available history with `a2a read {cid}`.")
        end = f"You opened it; `a2a end {cid}` ends it for everyone." if st["opened_by"] == me() else f"`a2a leave {cid}` leaves only for you."
        reply = f"reply with `a2a reply {cid} \"...\"`. " if other_members(st) else ""
        guidance.append(f"Live chat {cid}: {reply}{end}")
    return fresh, "\n".join([f"a2a: {len(fresh)} unread chat update(s) for you.", lines, *guidance]), nudge_state


def settle_nudges(fresh: list[dict], nudge_state: dict) -> None:
    """The reason is already in the model's context: mark the events read
    and remember the nudge. Events the acknowledgement did not cover stay
    un-nudged, so the next hook shows them again. Never raises: on Kimi the
    caller's exit status is what continues the turn."""
    try:
        failed = mark_read(fresh)
    except (LogstreamError, OSError):
        failed = [e["id"] for e in fresh]
    try:
        for e in fresh:
            if e["id"] not in failed:
                nudge_state[e["id"]] = time.time()
        write_state("nudges.json", {k: v for k, v in nudge_state.items() if time.time() - v < 86400})
    except OSError:
        pass


# ── Courier (per-host delivery) ─────────────────────────────────────────────


def courier_id() -> str:
    return f"courier@{socket.gethostname()}"


def _children(pid: int) -> list[int]:
    try:
        return [int(x) for x in Path(f"/proc/{pid}/task/{pid}/children").read_text().split()]
    except (OSError, ValueError):
        return []


def _harness_under(pid: int, depth: int = 3) -> tuple[int, str] | None:
    harness = harness_of_pid(pid)
    if harness:
        return pid, harness
    if depth == 0:
        return None
    for child in _children(pid):
        found = _harness_under(child, depth - 1)
        if found:
            return found
    return None


def _environ(pid: int) -> dict:
    try:
        raw = Path(f"/proc/{pid}/environ").read_bytes()
    except OSError:
        return {}
    env = {}
    for item in raw.split(b"\0"):
        key, sep, value = item.partition(b"=")
        if sep:
            env[key.decode(errors="replace")] = value.decode(errors="replace")
    return env


def _cmdline(pid: int) -> list[str]:
    try:
        return [arg.decode(errors="replace") for arg in Path(f"/proc/{pid}/cmdline").read_bytes().split(b"\0") if arg]
    except OSError:
        return []


def codex_resume_session(cmdline: list[str]) -> str:
    for command, argument in zip(cmdline, cmdline[1:]):
        if command == "resume" and re.fullmatch(r"[0-9a-fA-F]{8}(?:-[0-9a-fA-F]{4}){3}-[0-9a-fA-F]{12}", argument):
            return argument
    return ""


def scan_panes(include_process_details: bool = False) -> list[dict]:
    """Every harness running in a tmux pane on this host, hook or no hook."""
    sockets = tmux_sockets()
    agents = []
    for socket_path in sockets:
        if not os.path.exists(socket_path):
            continue
        try:
            output = subprocess.run(["tmux", "-S", socket_path, "list-panes", "-a", "-F", "#{pane_id} #{pane_pid} #{pane_current_path} #{session_name}"], capture_output=True, text=True, timeout=5, check=False).stdout
        except (OSError, subprocess.SubprocessError):
            continue
        for line in output.splitlines():
            parts = line.split(" ", 3)
            if len(parts) < 4 or not parts[1].isdigit():
                continue
            found = _harness_under(int(parts[1]))
            if not found:
                continue
            pid, harness = found
            env = _environ(pid)
            # Hook ownership and courier roster rows must use the same handle.
            handle = env.get("A2A_HANDLE") or f"{harness}-{handle_tag(env.get('NOISEDECK_SESSION_ID', ''), pid)}"
            # Hooks and scans use the same Deck registration, not the pane cwd.
            scope = local_session_scope(env.get("NOISEDECK_SESSION_ID", ""), env.get("NOISEDECK_PROJECT_ID", ""))
            agent = {
                "handle": handle,
                "harness": harness,
                "host": socket.gethostname(),
                "pid": pid,
                "project": scope["project_name"] if scope else "",
                "project_id": scope["project_id"] if scope else "",
                "project_name": scope["project_name"] if scope else "",
                "deck_session": env.get("NOISEDECK_SESSION_ID", ""),
                "display_name": session_label(env.get("NOISEDECK_SESSION_ID", "")),
                "pane": {"socket": socket_path, "pane": parts[0], "session": parts[3]},
            }
            if harness == "codex":
                cmdline = _cmdline(pid)
                agent["native_session"] = codex_resume_session(cmdline)
                if include_process_details:
                    # Process arguments may contain prompts; only native IDs enter presence.
                    agent["cmdline"] = cmdline
                    try:
                        agent["cwd"] = os.readlink(f"/proc/{pid}/cwd")
                    except OSError:
                        agent["cwd"] = ""
            agents.append(agent)
    return agents


def publish_scan(state: dict, verbose: bool) -> None:
    agents = sorted(scan_panes(), key=lambda a: a["handle"])
    digest = json.dumps(agents, sort_keys=True)
    if digest == state.get("scan_digest") and time.time() - state.get("scan_at", 0) < SCAN_REFRESH_S:
        return
    metadata = signed_presence("courier.scan", {"host": socket.gethostname(), "agents": agents})
    _rpc("mempalace_event_append", {"type": "courier.scan", "stream": STREAM, "room": ROOM_PRESENCE, "from_agent": courier_id(), "to_agent": "*", "status": "ready", "metadata": metadata})
    state["scan_digest"] = digest
    state["scan_at"] = time.time()
    if verbose:
        print(f"scan: {', '.join(a['handle'] for a in agents) or 'no harness panes'}", flush=True)


def _codex_queue(native_session: str, text: str) -> str:
    if not native_session:
        return "inbox-only"
    try:
        done = subprocess.run(["codex", "queue", "--thread", native_session, "--message", text], capture_output=True, text=True, timeout=20, check=False)
        return "codex-queue" if done.returncode == 0 else "uncertain"
    except OSError:
        return "inbox-only"  # the executable did not start
    except subprocess.SubprocessError:
        # The queue may have accepted the message before the command failed.
        return "uncertain"


def _claude_socket(path: str, text: str) -> str:
    """Post the wake line to a Claude session's inbox socket. A missing or
    refused socket accepted nothing; a failed write may have."""
    path = path.removeprefix("uds:")
    if not path.startswith("/"):
        return "inbox-only"
    line = json.dumps({"type": "user", "message": {"role": "user", "content": text}}).encode() + b"\n"
    conn = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    try:
        conn.settimeout(10)
        conn.connect(path)
    except OSError:
        conn.close()
        return "inbox-only"  # the socket is gone; the inbox keeps the event
    try:
        conn.sendall(line)
        return "claude-socket"
    except OSError:
        return "uncertain"
    finally:
        conn.close()


_CONTROL_RE = re.compile(r"[\x00-\x1f\x7f\x80-\x9f]")


def inert(text) -> str:
    """One printable notification line with C0 and C1 controls removed."""
    return _CONTROL_RE.sub("", re.sub(r"\s+", " ", str(text or ""))).strip()


HANDLE_CHARS = 64


def handle_text(value) -> str:
    """A handle as the wake line may carry it: inert and no longer than a
    handle is allowed to be, whatever an event claims."""
    text = inert(value)
    return text if len(text) <= HANDLE_CHARS else text[:HANDLE_CHARS] + "…"


def delivery_tag(e: dict) -> str:
    """A short event tag for distinguishing notifications in the same chat."""
    return "#" + (e.get("id") or "").rsplit("_", 1)[-1][-6:]


def wake_line(e: dict) -> str:
    text, unreadable = open_body(e)
    if unreadable:
        return f"[a2a{inert(delivery_tag(e))}] unreadable sealed message from {handle_text(e.get('from_agent'))} in chat {handle_text(e.get('correlation_id'))}: {inert(unreadable)}"
    body = inert(text)
    if len(body) > WAKE_BODY_CHARS:
        body = body[:WAKE_BODY_CHARS] + "…"
    who, cid, tag = handle_text(e.get("from_agent")), handle_text(e.get("correlation_id")), inert(delivery_tag(e))
    kind = inert(e["type"]).split(".", 1)[1]
    if kind == "close-request":
        return f"[a2a{tag}] {who} proposed ending chat {cid} under the legacy agreement rules: {body} — history: `a2a read {cid}`"
    if kind == "closed":
        return f"[a2a{tag}] {who} closed chat {cid}" + (f": {body}" if body and body != "closed" else ".")
    if kind == "close-ack":
        return f"[a2a{tag}] {who} agreed to end chat {cid} under the legacy agreement rules."
    if kind == "decline":
        return f"[a2a{tag}] {who} declined chat {cid}: {body}"
    if kind == "join":
        return f"[a2a{tag}] {who} added {handle_text(metadata_of(e).get('member'))} to chat {cid} — if live with peers: `a2a reply {cid} \"...\"`; history: `a2a read {cid}`"
    if kind == "leave":
        return f"[a2a{tag}] {who} left chat {cid}" + (f": {body}" if body else "")
    if kind == "accept":
        return f"[a2a{tag}] {who} accepted chat {cid}" + (f": {body}" if body else "")
    verb = "opened a chat" if kind == "invite" else "says"
    return f"[a2a{tag}] {who} {verb} ({cid}): {body} — if live with peers: `a2a reply {cid} \"...\"`; history: `a2a read {cid}`"


def delivery_mode(target: dict) -> str:
    """Choose a native receive path without touching terminal input."""
    harness = target.get("harness")
    if harness in ("pi", "omp"):
        return "extension"
    pane = target.get("pane") or {}
    if pane.get("socket") not in tmux_sockets():
        # Presence is agent-declared; a foreign socket authorizes no queue.
        return "unavailable"
    if harness == "codex" and target.get("native_session"):
        return "codex-queue"
    if harness == "claude" and target.get("inbox_socket"):
        return "claude-socket"
    return "inbox-only"


def deliver(e: dict, target: dict, line: str | None = None) -> str:
    """Queue through the native API, or leave the event to its recipient.

    An uncertain queue attempt is never repeated. Unsupported harnesses
    stay unread in the inbox; no automatic delivery writes terminal bytes.
    """
    mode = delivery_mode(target)
    text = wake_line(e) if line is None else line
    if mode == "codex-queue":
        return _codex_queue(target["native_session"], text)
    if mode == "claude-socket":
        return _claude_socket(target["inbox_socket"], text)
    return mode


def reminder_line(e: dict, delivered_at: float) -> str:
    """The one re-nudge a delivered-but-unread native delivery earns: a new
    one-line message that names the chat, never a replay of the body."""
    who, cid, tag = handle_text(e.get("from_agent")), handle_text(e.get("correlation_id")), inert(delivery_tag(e))
    ago = max(1, int((time.time() - delivered_at) / 60))
    return (f"[a2a{tag}] reminder: {who}'s message in {cid} had no read acknowledgement "
            f"when checked {ago}m after delivery. This queued reminder may be stale; "
            "check `a2a inbox` and ignore it if the message is already read or the chat is closed.")


def recipient_read(item: dict, recipient: str) -> bool | None:
    """The recipient's own vouched acknowledgement of this box, nothing
    else. None when the relay did not answer: unknown, and unknown nudges
    nothing."""
    scope = item.get("scope")
    if not scope:
        return False
    try:
        acks = list_events(room=ROOM_CHAT, type="event.ack", correlation_id=item.get("chat"))
    except LogstreamError:
        return None
    for a in acks:
        if a.get("from_agent") == recipient and (_ack_fields(a) or {}).get("scope") == scope and ack_vouched(a):
            return True
    return False


def reminder_event(item: dict, recipient: str) -> dict | None:
    """Revalidate routing and read state immediately before native submission."""
    history = chat_events(item["chat"])
    current = chat_state(history)
    event = next((e for e in history if scope_str(scope_of(e)) == scope_str(item.get("scope"))), None)
    if (event is None or current.get("state") == "closed" or recipient in current["left"]
            or recipient not in current["routes"].get(event["id"], [])):
        return None
    read = recipient_read(item, recipient)
    if read is None:
        raise LogstreamError("reminder read state is unavailable")
    return None if read else event


def delivery_settled(item: dict, recipient: str) -> bool | None:
    """Whether the relay already settles this delivery: a vouched outcome
    from this courier for the box and recipient, or the recipient's own
    vouched acknowledgement of the box. Asked before a native submission,
    so forgetting a local outcome cannot repeat it. None when the relay
    did not answer: unknown, and unknown submits nothing."""
    scope = item.get("scope")
    if not scope:
        return False
    try:
        acks = list_events(room=ROOM_CHAT, type="event.ack", correlation_id=item.get("chat"))
    except LogstreamError:
        return None  # unknown: nothing is submitted on a guess
    for a in acks:
        writer = a.get("from_agent") or ""
        payload = _ack_fields(a) or {}
        if payload.get("scope") != scope:
            continue
        if writer == courier_id() and courier_ack_recipient(a) == recipient and ack_vouched(a):
            return True
        if writer == recipient and ack_vouched(a):
            return True
    return False


def courier_ack(event_id: str, recipient: str, outcome: str, rung: str = "", chat_id: str | None = None, scope: dict | None = None) -> bool:
    """Record one recipient's delivery outcome, once. The ack routes to the
    writer, so the recipient is named in the body: the Chats panel and the
    recipient's own inbox both read it from there. An outcome already in
    the ledger, from a courier that died before saving its state, is not
    written again."""
    note = f"delivered to {recipient} via {rung}" if outcome == "delivered" else f"uncertain for {recipient}"
    status = "ready" if outcome == "delivered" else "failed"
    try:
        for a in list_events(room=ROOM_CHAT, type="event.ack", correlation_id=chat_id):
            same = (_ack_fields(a) or {}).get("scope") == scope if scope else metadata_of(a).get("ack_of") == event_id
            if same and a.get("from_agent") == courier_id() and courier_ack_recipient(a) == recipient and ack_vouched(a):
                return True
    except LogstreamError:
        return False
    try:
        _rpc("mempalace_event_ack", {"event_id": event_id, "from_agent": courier_id(), "status": status, "body": ack_payload(event_id, courier_id(), status, recipient=recipient, note=note, scope=scope)})
        return True
    except LogstreamError:
        return False


def courier_once(state: dict, host: str, verbose: bool, persist=None) -> dict:
    """One pass: pick up new addressed events, deliver, retry the unacked.

    One pending item per (event, recipient). `persist` writes the state
    durably just before a native submission, with the item marked in flight.
    A courier that dies before recording the outcome treats that attempt
    as uncertain on restart and never submits it again.
    """
    persist = persist or (lambda st: None)
    migrate_pending(state)
    cursor = state.get("cursor")
    filters = {"room": ROOM_CHAT, "stream": STREAM, "limit": PAGE}
    events = _paged_events_relaxed(filters, cursor)
    pending = state.setdefault("pending", {})
    held = state.setdefault("held", {})
    nudgable = state.setdefault("nudgable", {})
    chats = all_chats() if pending or held or nudgable or any(e["type"] in CHAT_TYPES for e in events) else {}
    now = time.time()
    # A delivery is one delivery by the box's identity and recipient: one
    # already settled, delivered or read, is not queued again when the same
    # box arrives under another relay row.
    settled = state.setdefault("settled", [])
    done = set(settled) | set(state.get("acked", []))
    for e in events:
        state["cursor"] = e["id"]
        if e["type"] not in CHAT_TYPES:
            continue
        # Content-blind: routing fields only. The body is read back from the
        # Logstream at delivery time and copied nowhere but the wake line.
        # An event the fold could not place yet, because the key or the
        # presence it depends on has not replicated, is held and looked at
        # again each pass until it routes or the hold runs out.
        if e["id"] in (chats.get(e.get("correlation_id") or "") or {}).get("routes", {}):
            for recipient in recipients_of(e, chats):
                key = f"{scope_str(scope_of(e))}|{recipient}"
                if key not in done:
                    pending.setdefault(key, {"event": e["id"], "to": recipient, "chat": e.get("correlation_id"), "scope": scope_of(e), "attempts": 0, "next": 0})
        else:
            held[e["id"]] = {"chat": e.get("correlation_id"), "scope": scope_of(e), "at": e.get("created_at"), "since": now}
    for event_id in list(held):
        item = held[event_id]
        routes = (chats.get(item.get("chat") or "") or {}).get("routes", {})
        if event_id in routes:
            for recipient in routes[event_id]:
                key = f"{scope_str(item.get('scope'))}|{recipient}"
                if key not in done:
                    pending.setdefault(key, {"event": event_id, "to": recipient, "chat": item.get("chat"), "scope": item.get("scope"), "attempts": 0, "next": 0})
            del held[event_id]
        elif now - item.get("since", now) > HOLD_S:
            del held[event_id]
    if len(held) > 4000:
        for event_id in sorted(held, key=lambda k: held[k].get("since", 0))[: len(held) - 4000]:
            del held[event_id]
    if not pending and not nudgable:
        return state
    online = roster()
    # A recipient's own ack, one its host's key vouches for, means the
    # message reached a reading agent; an injected one stops nothing.
    acked = set()
    cursor_before = state.get("ack_cursor")
    fresh = _paged_events_relaxed({"room": ROOM_CHAT, "stream": STREAM, "limit": PAGE, "type": "event.ack"}, cursor_before)
    if fresh:
        state["ack_cursor"] = fresh[-1]["id"]
    # An acknowledgement whose key or presence has not arrived is held and
    # judged again each pass, like a chat event, so a delivery already
    # read is never submitted because its acknowledgement verified late.
    # The state keeps only the acknowledgement's id and when it was first
    # seen, never its body or metadata: the event is read back from the
    # Logstream each pass, from the cursor the oldest held one came in
    # under, and judged then.
    held_acks = state.setdefault("held_acks", {})
    if fresh and any(not a.get("from_agent", "").startswith("courier@") for a in fresh) and not held_acks:
        state["held_floor"] = cursor_before
    for a in fresh:
        if not a.get("from_agent", "").startswith("courier@"):
            held_acks[a["id"]] = now
    if held_acks:
        verified_events, _, bindings = verified_chats()
        index = scope_index(verified_events)
        for a in _paged_events_relaxed({"room": ROOM_CHAT, "stream": STREAM, "limit": PAGE, "type": "event.ack"}, state.get("held_floor")):
            if a["id"] not in held_acks:
                continue
            # Consumed only once both hold: the signature, and a target this
            # replica has verified. An acknowledgement that arrives before
            # the event it names stays held until the event does.
            target = ack_target(a, index) if ack_vouched(a, bindings.get(a.get("correlation_id") or "")) else ""
            if target:
                kept = next((e for e in verified_events if e["id"] == target), None)
                acked.add(f"{scope_str(scope_of(kept) if kept else None)}|{a.get('from_agent')}")
                del held_acks[a["id"]]
            elif now - held_acks[a["id"]] > HOLD_S:
                del held_acks[a["id"]]
        if len(held_acks) > 4000:
            for ack_id in sorted(held_acks, key=held_acks.get)[: len(held_acks) - 4000]:
                del held_acks[ack_id]
        if not held_acks:
            state.pop("held_floor", None)
    state.setdefault("acked", [])
    state["acked"] = (state["acked"] + sorted(acked))[-4000:]
    acked_all = set(state["acked"])
    for key in list(pending):
        item = pending[key]
        event_id, recipient = item["event"], item["to"]
        if key in acked_all:
            del pending[key]
            continue
        if item.get("inflight") and not item.get("outcome"):
            # A previous courier died during delivery, including old tmux
            # writes; the notification may have arrived.
            if verbose:
                print(f"{event_id}: in flight to {recipient} when the courier stopped; not submitted again", flush=True)
            item.update(outcome="uncertain", attempts=0, next=0)
        if item.get("outcome"):
            # The write is settled; only its acknowledgement is outstanding.
            # Never submit again, whatever happens to the ack; and the
            # outcome is kept here, unevictable, until the relay holds it,
            # because only the relay's copy outlives this courier's memory.
            if now < item["next"]:
                continue
            if not item.get("scope"):
                # An outcome recorded before scopes existed: the box's
                # identity is read from the fold before it is acknowledged,
                # so the acknowledgement names the event every replica
                # knows; until the fold has it, the outcome waits.
                found = next((x for x in chat_events(item.get("chat")) if x["id"] == event_id), None)
                if found is None:
                    item["next"] = now + COURIER_RETRY_S
                    continue
                item["scope"] = scope_of(found)
            item["attempts"] += 1
            item["next"] = now + min(COURIER_RETRY_S * item["attempts"], 3600)
            if courier_ack(event_id, recipient, item["outcome"], item.get("rung", ""), item.get("chat"), item.get("scope")):
                del pending[key]
                state["settled"] = (settled + [key])[-4000:]
                settled = state["settled"]
            continue
        chat = chats.get(item["chat"])
        if chat and recipient in chat["left"]:
            del pending[key]
            continue
        if now < item["next"]:
            continue
        target = online.get(recipient)
        if not target:
            item["attempts"] += 1
            item["next"] = now + COURIER_RETRY_S
            if item["attempts"] >= COURIER_MAX_ATTEMPTS:
                if verbose:
                    print(f"{event_id}: {recipient} never appeared; leaving it to their next turn")
                del pending[key]
            continue
        if target.get("host") != host:
            del pending[key]  # that host's courier owns it; nothing to keep here
            continue
        history = chat_events(item["chat"])
        # The box is found by its scope, whichever relay row represents it
        # now, so a delivery attempted under one row is never attempted
        # again under another.
        wanted = scope_str(item.get("scope")) if item.get("scope") else None
        e = next((x for x in history if (scope_str(scope_of(x)) == wanted if wanted else x["id"] == event_id)), None)
        if e is not None:
            item["scope"] = scope_of(e)
            item["event"] = event_id = e["id"]
        current_chat = chat_state(history)
        # The route is judged again now: an earlier close or departure
        # that replicated after this was queued makes it nobody's, and a
        # write that never started is not started.
        if e is None or recipient not in current_chat["routes"].get(event_id, []) or recipient in current_chat["left"]:
            del pending[key]
            continue
        known = delivery_settled(item, recipient)
        if known is None:
            item["next"] = now + COURIER_RETRY_S  # the relay did not answer; asked again later, never submitted on a guess
            continue
        if known:
            # The relay already holds this delivery's outcome or the
            # recipient's own acknowledgement, under the box's identity:
            # whatever this courier's bounded memory kept, it is settled.
            del pending[key]
            state["settled"] = (settled + [key])[-4000:]
            settled = state["settled"]
            continue
        _, unreadable = open_body(e)
        if unreadable:
            # Reported, never submitted, never acknowledged: a body this host
            # cannot open is not a delivery this courier can make.
            if verbose:
                print(f"{event_id}: unreadable for {recipient} ({unreadable}); not delivered", flush=True)
            state.setdefault("unreadable", {})[event_id] = unreadable
            del pending[key]
            continue
        mode = delivery_mode(target)
        if mode in ("extension", "inbox-only"):
            # Verification and routing above still apply. No hand-off was
            # attempted, so a restart must not invent an uncertain outcome.
            if verbose:
                print(f"{event_id}: -> {recipient} via {mode}; left unread", flush=True)
            del pending[key]
            continue
        item["inflight"] = True
        persist(state)
        rung = deliver(e, target)
        item["attempts"] += 1
        item["next"] = now + COURIER_RETRY_S
        if verbose:
            print(f"{event_id}: -> {recipient} via {rung} (attempt {item['attempts']})", flush=True)
        if rung == "unavailable":
            item["inflight"] = False
            if item["attempts"] >= COURIER_MAX_ATTEMPTS:
                del pending[key]
        elif rung in ("extension", "inbox-only"):
            del pending[key]
        else:
            # A native queue was attempted: never submit it twice. Keep the
            # outcome so the ledger and the inbox can show what is known.
            item.update(outcome="uncertain" if rung == "uncertain" else "delivered", rung=rung, attempts=0)
            if item["outcome"] == "delivered":
                # These rungs have no hook backstop: the model may never act
                # on a wake that landed while it was busy, and nothing else
                # resurfaces it. One reminder later is the only second chance.
                nudgable[key] = {"event": event_id, "to": recipient, "chat": item.get("chat"), "scope": item.get("scope"), "rung": rung, "delivered_at": now, "nudge_attempts": 0}
            persist(state)  # the settled outcome survives a death before the ack
            if courier_ack(event_id, recipient, item["outcome"], rung, item.get("chat"), item.get("scope")):
                del pending[key]
                state["settled"] = (settled + [key])[-4000:]
                settled = state["settled"]
    for key in list(nudgable):
        item = nudgable[key]
        recipient = item["to"]
        if item.get("nudge_inflight"):
            # A previous courier died with the reminder in flight: it may
            # have been accepted, so the one reminder is spent either way.
            item["nudged"] = now
            item.pop("nudge_inflight", None)
        if key in acked_all:
            del nudgable[key]
            continue
        age = now - item["delivered_at"]
        if age > NUDGE_TTL_S:
            del nudgable[key]
            continue
        history = chat_events(item["chat"])
        current = chat_state(history)
        e = next((x for x in history if scope_str(scope_of(x)) == scope_str(item.get("scope"))), None)
        # The route is rejudged now, as at first delivery: a departure that
        # replicated late can make the message nobody's, and its watch ends.
        if e is None or current.get("state") == "closed" or recipient in current["left"] or recipient not in current["routes"].get(e["id"], []):
            del nudgable[key]
            continue
        if item.get("nudged") or age < NUDGE_AFTER_S:
            continue
        read = recipient_read(item, recipient)
        if read is None:
            continue  # the relay did not answer: unknown, and unknown nudges nothing
        if read:
            del nudgable[key]
            continue
        target = online.get(recipient)
        if not target or target.get("host") != host:
            continue  # offline: the reminder waits rather than dies
        item["nudge_attempts"] += 1
        item["nudge_inflight"] = True
        persist(state)  # settled before the side effect, like a first delivery
        # A read or closure can commit while the in-flight record is flushed.
        # Native queues cannot retract an accepted reminder, so check again.
        try:
            current_event = reminder_event(item, recipient)
        except LogstreamError:
            item.pop("nudge_inflight", None)
            item["nudge_attempts"] -= 1
            persist(state)
            continue
        if current_event is None:
            del nudgable[key]
            persist(state)
            continue
        outcome = deliver(current_event, target, reminder_line(current_event, item["delivered_at"]))
        item.pop("nudge_inflight", None)
        if verbose:
            print(f"{item['event']}: reminder -> {recipient} via {outcome} (attempt {item['nudge_attempts']})", flush=True)
        if outcome in ("codex-queue", "claude-socket", "uncertain"):
            # Accepted, or maybe accepted: the one reminder is spent either way.
            item["nudged"] = now
        elif item["nudge_attempts"] >= NUDGE_MAX_ATTEMPTS:
            del nudgable[key]
        persist(state)
    if len(nudgable) > 4000:
        for key in sorted(nudgable, key=lambda k: nudgable[k]["delivered_at"])[: len(nudgable) - 4000]:
            del nudgable[key]
    return state


def prime_cursor(state: dict) -> None:
    """A courier's first run starts at the tip: old traffic was someone
    else's problem. That decision is made once and remembered, so a stream
    that was empty on the first run is read from its first event later. A
    state written by an earlier courier keeps its cursor."""
    if state.get("primed"):
        return
    if state.get("cursor"):
        state["primed"] = True
        return
    latest = list_events(room=ROOM_CHAT)
    state["cursor"] = latest[-1]["id"] if latest else None
    state["primed"] = True


def migrate_pending(state: dict) -> None:
    """Pending items written before rooms were keyed by event id alone and
    carried no `event` field. They become (event, recipient) items."""
    pending = state.get("pending") or {}
    for key in list(pending):
        item = pending[key]
        if "event" not in item:
            del pending[key]
            if item.get("to"):
                item["event"] = key
                if item.get("rung"):
                    item["outcome"] = "delivered"  # observed before the upgrade; only its ack was outstanding
                pending[f"{key}|{item['to']}"] = item


def courier_lock() -> object | None:
    path = _state_path(f"courier-{socket.gethostname()}.lock")
    handle = open(path, "a+")
    try:
        fcntl.flock(handle, fcntl.LOCK_EX | fcntl.LOCK_NB)
    except OSError:
        handle.close()
        return None
    handle.seek(0)
    handle.truncate()
    handle.write(str(os.getpid()))
    handle.flush()
    return handle


def courier_ensure() -> None:
    """Start a detached courier for this host if none holds the lock.

    Called from every client command as well as the hooks, so a host whose
    harnesses have no lifecycle hooks still gets discovery and delivery
    the first time any agent there runs `a2a`.
    """
    if os.environ.get("A2A_NO_COURIER"):
        return
    lock = courier_lock()
    if lock is None:
        return
    lock.close()  # release so the child can take it
    log = open(_state_path(f"courier-{socket.gethostname()}.log"), "ab")
    subprocess.Popen([sys.executable, os.path.abspath(__file__), "courier"], stdin=subprocess.DEVNULL, stdout=log, stderr=log, start_new_session=True, close_fds=True)


def cmd_courier(args) -> None:
    if args.ensure:
        courier_ensure()
        return
    lock = courier_lock()
    if lock is None:
        print("courier already running on this host")
        return
    host = socket.gethostname()
    name = f"courier-{host}.json"
    state = read_state(name)
    prime_cursor(state)
    write_state(name, state)
    print(f"{courier_id()} watching {STREAM}/{ROOM_CHAT} from {state.get('cursor') or 'the start'}", flush=True)
    while True:
        try:
            publish_scan(state, args.verbose)
            state = courier_once(state, host, args.verbose, persist=lambda st: write_state(name, st))
            write_state(name, state)
            if args.once:
                return
            timeout = 5_000 if state.get("pending") else 60_000
            try:
                wait_events(timeout, room=ROOM_CHAT, since_event_id=state.get("cursor"))
            except LogstreamError as exc:
                if not _cursor_refused(exc):
                    raise
                # A reset palace refuses the wait cursor too: wait from the
                # whole room, which the next pass re-primes from its tip.
                wait_events(timeout, room=ROOM_CHAT)
        except LogstreamError as exc:
            print(f"courier: {exc}", file=sys.stderr, flush=True)
            time.sleep(10)
        except KeyboardInterrupt:
            return


# ── CLI ─────────────────────────────────────────────────────────────────────


COMMAND_GROUPS = (
    ("Find agents", ("who", "whoami", "name", "up", "down")),
    ("Talk", ("chat", "reply", "accept", "decline", "add", "leave", "end")),
    ("Catch up", ("inbox", "wait", "read", "status", "mark-read")),
    ("Host trust", ("key",)),
    ("Integrations", ("ledger", "session-label", "hook", "courier")),
)


def terminal_width() -> int:
    return max(40, shutil.get_terminal_size((80, 24)).columns)


class HelpFormatter(argparse.HelpFormatter):
    def __init__(self, prog):
        super().__init__(prog, width=terminal_width(), max_help_position=22)

    def _split_lines(self, text, width):
        return textwrap.wrap(text, width, break_long_words=False, break_on_hyphens=False)

    def _fill_text(self, text, width, indent):
        if text.startswith(("Examples:\n", "Example:\n")):
            return "\n".join(indent + line for line in text.splitlines())
        return "\n".join(textwrap.fill(line, width, initial_indent=indent,
                                     subsequent_indent=indent, break_long_words=False,
                                     break_on_hyphens=False) if line else ""
                         for line in text.splitlines())


class CLIParser(argparse.ArgumentParser):
    def __init__(self, *args, **kwargs):
        kwargs.setdefault("formatter_class", HelpFormatter)
        super().__init__(*args, **kwargs)

    def error(self, message):
        # argparse's full choice inventory obscures the useful next step.
        message = re.sub(r" \(choose from .*\)$", "", message)
        self.exit(2, human_prose(f"{self.prog}: {message}", continuation="  ") + "\n" + human_prose(f"Try '{self.prog} --help'.") + "\n")

    def format_help(self):
        output = super().format_help()
        groups = getattr(self, "command_groups", None)
        if groups:
            for title, commands in groups:
                output += f"\n{title}:\n"
                for name in commands:
                    summary = self.command_summaries[name]
                    output += textwrap.fill(f"{name:<13} {summary}", terminal_width(),
                                            initial_indent="  ", subsequent_indent=" " * 16,
                                            break_long_words=False, break_on_hyphens=False) + "\n"
            output += "\n" + textwrap.fill("Run a2a <command> --help for arguments and examples.", terminal_width()) + "\n"
        return output


def seconds_arg(value: str) -> float:
    try:
        seconds = float(value)
    except ValueError:
        raise argparse.ArgumentTypeError("use a finite, nonnegative number of seconds") from None
    if not math.isfinite(seconds) or seconds < 0:
        raise argparse.ArgumentTypeError("use a finite, nonnegative number of seconds")
    return seconds


def build_parser() -> argparse.ArgumentParser:
    p = CLIParser(prog="a2a", usage="a2a <command> [options]",
                  description="Chat with other agents through the MemPalace relay.")
    sub = p.add_subparsers(dest="command", prog="a2a", metavar="<command>", help=argparse.SUPPRESS)
    p.command_groups = COMMAND_GROUPS
    p.command_summaries = {}

    def command(name, summary, description, example, fn):
        p.command_summaries[name] = summary
        parser = sub.add_parser(name, description=description,
                                epilog="Examples:\n" + example)
        parser.set_defaults(fn=fn)
        return parser

    def selector(parser):
        parser.add_argument("chat", nargs="?", metavar="CHAT", help="chat ID or active member handle; omit when only one live chat exists")

    def text(parser, optional=False):
        parser.add_argument("text", nargs="*", metavar="TEXT", help=("optional message or reason" if optional else "message text; read stdin if omitted"))

    command("whoami", "Show your handle", "Print your exact routing handle, without decoration.", "a2a whoami", cmd_whoami)
    q = command("name", "Choose your handle", "Choose a handle and announce it online.", "a2a name reviewer", cmd_name)
    q.add_argument("handle", help="new routing handle")
    command("up", "Announce online", "Publish your current presence as online.", "a2a up", cmd_up)
    command("down", "Announce offline", "Publish your current presence as offline.", "a2a down", cmd_down)
    q = command("who", "List agents in your project", "Show reachable agents in this Deck project. Select another known project by exact name or ID.", 'a2a who\na2a who --json\na2a who --project "Other project"', cmd_who)
    q.add_argument("--json", action="store_true", help="print this project's roster as JSON")
    q.add_argument("--project", metavar="NAME_OR_ID", help="select one exact project name or ID")
    q = command("session-label", "Register Deck session metadata", "Register a local Deck display name and project; this does not rename the routing handle.", "a2a session-label \\\n  --session SESSION_ID --name Review", cmd_session_label)
    q.usage = "a2a session-label [options]"
    q.add_argument("--session", required=True, help="Deck session ID")
    q.add_argument("--name", required=True, help="display name for the session")
    q.add_argument("--project-id", default=None, metavar="ID", help="Deck project ID; requires --project-name")
    q.add_argument("--project-name", default=None, metavar="NAME", help="Deck project name; requires --project-id")

    def key_dispatch(args):
        if args.key_command is None:
            k.print_help()
        else:
            cmd_key(args)

    k = command("key", "Manage host fingerprints", "Inspect host keys and pin fingerprints verified with whoever runs the host.", "a2a key show\na2a key status\na2a key trust --help", key_dispatch)
    ks = k.add_subparsers(dest="key_command", metavar="<action>")
    def key(name, summary, example):
        return ks.add_parser(name, help=summary, description=summary + ".", epilog="Example:\n" + example)
    key("init", "Generate a host key without overwriting an existing key", "a2a key init")
    q = key("show", "Print this host's fingerprint", "a2a key show --public")
    q.add_argument("--public", action="store_true", help="also print the public key; never the private key")
    key("announce", "Publish this host's public key to the relay", "a2a key announce")
    q = key("trust", "Pin a fingerprint only after verifying it with whoever runs the host", "a2a key trust HOST FINGERPRINT")
    q.add_argument("host", help="host name supplied by whoever runs it")
    q.add_argument("fingerprint", help="24-character fingerprint from that host's a2a key show")
    key("status", "Compare announced and pinned host fingerprints", "a2a key status")
    key("migrate", "Replace a pre-sealing key and record its history", "a2a key migrate")
    q = key("history", "Supply a missing pre-sealing history record", "a2a key history --from legacy-ids\na2a key history --none")
    q.add_argument("--none", action="store_true", help="accept that pre-sealing chats remain unreadable")
    q.add_argument("--from", dest="source", metavar="FILE", help="legacy-ids copy saved before sealing")

    q = command("chat", "Start or continue a chat", "Message peers in this Deck project. Use --project with an exact name or ID for another project, even when continuing a chat. Use reply to continue an existing chat without selecting its project again. Receiving a reply is not guaranteed.", 'a2a chat reviewer "Ready to review?"\nprintf %s Hello | a2a chat peer', cmd_chat)
    q.add_argument("peers", metavar="PEER[,PEER...]", help="one or more exact handles, comma-separated")
    text(q)
    q.add_argument("--wait", type=seconds_arg, metavar="SECONDS", help="wait this many seconds for a reply, then acknowledge received messages")
    q.add_argument("--force", action="store_true", help="attempt contact even if a peer is offline; project selection and host trust still apply")
    q.add_argument("--project", metavar="NAME_OR_ID", help="select the recipients' exact project name or ID")
    shorthand = " Select a chat by ID or active member, or omit it when only one live chat exists. An unrecognized first word becomes message text in that single chat."
    q = command("reply", "Reply in an existing chat", "Send to the chat's participating members, not privately to the selected member." + shorthand, 'a2a reply CHAT_ID "Looks good"\na2a reply reviewer "Looks good"', cmd_reply)
    selector(q); text(q)
    q.add_argument("--wait", type=seconds_arg, metavar="SECONDS", help="wait this many seconds for another message and acknowledge it")
    for name, summary, description, fn in (
        ("accept", "Accept a chat", "Explicitly accept a chat; replying also accepts it.", cmd_accept),
        ("decline", "Decline participation", "Decline your participation. The opener must use end instead.", cmd_decline),
        ("leave", "Leave a chat", "Leave only your participation. The opener must use end instead.", cmd_leave),
        ("end", "End your chat or leave", "The opener closes the chat for everyone; another member only leaves. History remains readable.", cmd_end),
    ):
        q = command(name, summary, description + shorthand, f"a2a {name} CHAT_ID", fn)
        selector(q); text(q, optional=True)
    q = command("add", "Add chat members", "Add members from this Deck project, or use --project with an exact name or ID for another project. Former members cannot rejoin it; a new host may require a new chat." + shorthand.replace("message text", "member handles"), "a2a add CHAT_ID reviewer tester", cmd_add)
    selector(q)
    q.add_argument("handle", nargs="*", metavar="HANDLE", help="handles to add; omit CHAT when there is only one live chat")
    q.add_argument("--project", metavar="NAME_OR_ID", help="select the new members' exact project name or ID")
    q = command("inbox", "Read unread messages", "Show unread messages and acknowledge readable messages by default. Unreadable events stay unread.", "a2a inbox\na2a inbox --peek\na2a inbox --chat CHAT_ID", cmd_inbox)
    q.add_argument("--chat", metavar="ID", help="restrict to this chat ID")
    q.add_argument("--json", action="store_true", help="print verified events as JSON; still acknowledges unless --peek")
    q.add_argument("--peek", action="store_true", help="show messages without marking them read")
    q.add_argument("--follow", action="store_true", help="stream one JSON line of unread mail per relay wake, forever; implies --peek semantics and needs --json")
    q = command("mark-read", "Acknowledge event IDs", "Mark the named message events read.", "a2a mark-read EVENT_ID", cmd_mark_read)
    q.add_argument("event_id", nargs="+", metavar="ID", help="one or more exact event IDs")
    q = command("wait", "Wait for messages", "Wait for incoming messages, display them and acknowledge them. Ctrl+C stops waiting.", "a2a wait --timeout 60\na2a wait --chat CHAT_ID", cmd_wait)
    q.add_argument("--chat", metavar="ID", help="restrict to this chat ID")
    q.add_argument("--timeout", type=seconds_arg, default=540, metavar="SECONDS", help="maximum wait in seconds (default: 540)")
    q = command("read", "Read chat history", "Show visible chat history and acknowledge the incoming messages displayed. Omit CHAT only when one live chat exists.", "a2a read CHAT_ID\na2a read reviewer", cmd_read)
    selector(q)
    q = command("status", "Show your active chats", "List your active chats, including invitations and pending closure.", "a2a status\na2a status --all", cmd_status)
    q.add_argument("--all", action="store_true", help="include closed, declined and left chats")
    q = command("ledger", "Export chat JSON", "Export a machine-readable chat ledger. This command always prints JSON.", "a2a ledger --compact\na2a ledger --index", cmd_ledger)
    q.add_argument("--for", dest="handle", default=None, metavar="HANDLE", help="agent handle (default: your own)")
    q.add_argument("--compact", action="store_true", help="print JSON without indentation")
    q.add_argument("--index", action="store_true", help="export content-free chat IDs, states and members")
    q.add_argument("--for-session", dest="session", default=None, metavar="ID", help="resolve this Deck session's handle; requires --harness")
    q.add_argument("--harness", default=None, help="session harness used for its default handle")
    q.add_argument("--probe", action="store_true", help="read one relay event, exit nonzero if the relay does not answer; prints nothing")
    q = command("hook", "Run a native lifecycle hook", "Integration entry point: read native hook JSON from stdin. Output is reserved for the harness.", "a2a hook prompt < hook.json", cmd_hook)
    q.add_argument("event", choices=["session-start", "prompt", "stop", "session-end"], metavar="EVENT", help="session-start, prompt, stop or session-end")
    q.add_argument("--harness", default=None, help="native harness name; otherwise use process discovery")
    q = command("courier", "Run host delivery", "Run the host's native delivery daemon. Only one courier runs per host.", "a2a courier --ensure\na2a courier --once", cmd_courier)
    q.add_argument("--once", action="store_true", help="perform one delivery pass and exit")
    q.add_argument("--ensure", action="store_true", help="start a detached courier only if none is running")
    q.add_argument("-v", "--verbose", action="store_true", help="print delivery and scan diagnostics")
    return p


def main(argv: list[str] | None = None) -> None:
    parser = build_parser()
    args = parser.parse_args(argv)
    if args.command is None:
        print("a2a — chat with other agents\n")
        print('  a2a who     Find agents in this project\n  a2a inbox   Read replies\n  a2a status  See active chats\n\nStart a chat:\n  a2a chat PEER "Hello"\n\nAll commands: a2a --help')
        return
    try:
        args.fn(args)
    except KeyboardInterrupt:
        print("a2a: interrupted", file=sys.stderr)
        raise SystemExit(130) from None
    except LogstreamError as exc:
        raise SystemExit(human_prose(f"a2a: {exc}", continuation="  ")) from None
    except SystemExit as exc:
        if isinstance(exc.code, str):
            message = human_text(exc.code, layout=False)
            raise SystemExit(human_prose(message if message.startswith("a2a:") else f"a2a: {message}", continuation="  ")) from None
        raise


if __name__ == "__main__":
    main()
