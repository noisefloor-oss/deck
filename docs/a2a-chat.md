# Agent-to-agent chat

Running agents, on any harness and any host that share a MemPalace
palace, can hold a live conversation: two of them, or a room of several. One command, `a2a`, is the whole
client. The Logstream is the wire. A per-host courier uses supported native receive paths. Other harnesses
keep messages unread for their next hook, `a2a inbox`, or `a2a wait`; an
idle recipient is not guaranteed to wake. Automatic chat never writes into
the terminal editor.

This document is the reference for the design. The skill that agents read
is `deploy/mempalace/skills/a2a-chat/SKILL.md`; the standalone client and
its suite live in https://github.com/noisefloor-oss/a2a.

## CLI guidance and output

Run `a2a` for a compact starting point, `a2a --help` for grouped commands,
and `a2a <command> --help` for arguments and examples. `a2a key` shows its
host-trust actions. These guidance commands work offline and do not start a
courier or write local state.

Human roster and chat output labels handles, identity, trust and status.
Full handles and chat IDs remain copyable at narrow widths; event IDs for
`mark-read` are available through `inbox --json --peek`. Long IDs
and message lines are never truncated. Message bodies keep their spaces,
tabs and line breaks between visual separators. These separators aid reading;
they do not authenticate message boundaries inside arbitrary message text. Other terminal
control characters are shown visibly rather than executed by the terminal.
This display treatment does not change stored or sent text, JSON exports,
raw handle/fingerprint commands, or native hook output.

`inbox` and `wait` acknowledge readable messages; `inbox --peek` and `read`
do not. Unreadable events remain unread. `status` lists active chats;
`status --all` includes closed and left chats. Wait durations are finite,
nonnegative seconds (the default for `wait` is 540); Ctrl+C exits with 130.
Invalid arguments print a concise error and a command-specific help hint.

## What it has to do

- **Easy.** `a2a who`, `a2a chat <peer> "..."`, `a2a end`. No ids to
  remember, no room or stream to choose, no configuration.
- **It just works.** An agent that never ran a hook, on a harness with no
  plugin system, is still on the roster and still receives messages.
- **Every harness, every host.** Claude Code, Codex, Kimi, OpenCode, Pi and
  omp are all in the fleet today. Nothing in the design depends on a
  harness-specific inbox, and cross-host traffic rides the palace mesh
  that already exists.

## The chat lifecycle

| Primitive | Event | Who | Effect |
|---|---|---|---|
| Start | `chat.invite` | opener | Opens the chat and names its members. The first message is the invitation; no separate handshake. A member accepts by replying (`chat.message`) or explicitly (`chat.accept`). |
| Notify | courier delivery, one `event.ack` from `courier@host` per recipient | courier | Puts the message in front of each recipient and records what happened, delivered or uncertain, naming the recipient. |
| Leave | `chat.leave` or `chat.decline` | any member except the opener | Ends only that member's participation. The chat stays live, even if only the opener remains. |
| End | one `chat.closed` marked `close: opener` | opener | Ends the chat for everyone. There is no agreement round or force timeout. |

Everything is an immutable Logstream event in stream `a2a`. Chats live in
room `chat` and are keyed by `correlation_id`, which is the chat id.
Presence lives in room `presence`. The message text is the event body.
Metadata carries membership, lifecycle and routing facts, such as host,
harness, pid and pane. No chain of thought, no transcripts, no credentials.

A fold reads a room in append order, oldest first, through to the tip, and
the client asks the relay for that order on every listing: MemPalace 3.10.0
lists newest first when no cursor is given, and a relay from 3.8.0 refuses
the request as an unknown parameter, so the client asks once and then stops
asking that relay. A listing that is not in append order is refused before
anything folds or reaches the ledger's mirror.

Read cost must not grow with the palace. One-shot commands list through an
on-disk mirror per filter set: the cached events plus whatever arrived since
the cached tip, because the stream is append-only and its events immutable.
The presence room's mirror is retention-bounded to the roster's horizon —
the roster folds nothing older — and placement memory, which is forever, is
kept instead as one small derived state (hosts, first-vouch ownership,
project placement, harness and session names) replayed only with the events
that arrived since its tip; every update is newest-wins by the relay's
append sequence, or earliest-wins for a first vouch, so concurrent writers
merge under the mirror's lock without ordering. A relay that no longer
knows a cached tip is a reset: the mirror and the state rebuild whole.
Long-running loops — the courier, `wait`, and every caller-held cursor —
always read the relay live.

The chat's state is folded from its events, never stored:

```
invited --(accept | message)--> open
invited | open --(chat.closed from the opener)--> closed
participant active --(leave | decline)--> left; shared chat stays live
```

`a2a chat` continues an existing live chat with those members that the caller
still belongs to, or opens one, so an agent never has to hold a chat id.
It requires project selection even when continuing a chat. Use `a2a reply`
to continue an existing chat without selecting its members' project again.
The opener uses `a2a end` to close globally; anyone else's `a2a end` is an
alias for `a2a leave`. The opener cannot leave or decline their own chat.
History remains readable after leaving or closure. A member who left cannot
be added back to the same chat; start a new chat to invite them again.
If end or leave cannot confirm its result, retry the exact command with the
chat id printed in the error, not shorthand that could select another chat.
Naming a member selects a live chat where both of you still participate;
it does not send a private message within a room. A known member who no
longer shares a live chat is an error, not the first word of a message to
some other chat. Use the chat id to address the remaining members explicitly.
If nobody else remains, replies are refused; they are not stored as hidden
notes. The chat stays live so its opener can end it or add a new member.
If a departure wins while the message is being appended, the raw attempt
remains in the event log but is not accepted, counted, displayed, or delivered.
The command confirms the committed event's route before reporting it sent.
If confirmation is unavailable, inspect the history before retrying.

## Identity

An agent's handle is, in order: `A2A_HANDLE` from the environment, a name it
chose with `a2a name`, or `<harness>-<tag>`. The tag is the first four
characters of the Deck session id when there is one, else the harness pid.
The default is never persisted, because the courier computes the same
handle from the outside by reading the pane's process environment. An
agent that has done nothing at all is therefore on the roster under the
same name it will print with `a2a whoami`.

## Discovery

`a2a who` and `a2a who --json` show agents in the current Deck project.
Deck's saved session metadata supplies the stable project ID and committed
project name; the shell's current directory does not select the project.
Changing directories or working in a linked worktree leaves the scope intact.

Use `--project "Project name"` on `who`, `chat`, or `add` to select another
project explicitly. Matching is exact. An exact project ID also works when
names are ambiguous; the error asks for that ID without listing alternative
projects. Equal names never combine distinct project IDs. A stable project
ID can select participants across hosts, subject to the existing host-key
checks. There is no project-list command, wildcard, or global roster option.

`chat` and `add` check every new recipient against the selected project
before sending anything. A remembered handle does not bypass this check.
`chat --force` only waives the online check: an offline recipient still needs
host-vouched project metadata and the usual verified host placement. Missing,
conflicting, or legacy metadata refuses new contact rather than choosing a
project from a directory name. Existing chats remain readable and replyable
under their membership rules, including chats across projects. The console's
content-free ledger index retains its existing contract.

This selection guard reduces accidental contact in Deck's shared, unconfined
profile. It does not isolate project data or make relay metadata private.

`a2a who` merges two sources and the newest wins:

1. **Courier scans.** Every courier lists every pane on the tmux sockets it
   knows (Deck's `/run/noisedeck/tmux.sock`, or exactly the list in
   `A2A_TMUX_SOCKETS` when that is set), walks
   each pane's process tree three levels for a harness binary, and
   publishes one `courier.scan` event per host with the whole roster. It
   republishes on change and every thirty minutes — a backstop now that
   every harness's own lifecycle hooks report presence, not the liveness
   channel. A courier that watches an agent vanish still publishes the
   change at once; an hour bounds only the scan-only case, where the
   agent's pane and its courier died together.
   This is why Codex, Kimi
   and OpenCode appear without any hook: their pane is enough.
2. **Hook presence.** Where a harness offers lifecycle hooks, the agent
   announces `agent.online`, `agent.busy`, `agent.idle`, `agent.offline`
   itself. This adds state and the harness's native session id, and it is
   how an agent outside tmux shows up at all.

Deck strips `TMUX` from harness environments, so an agent cannot report its
own pane. The scan finds it from the outside by matching pane pids against
the process ancestry.

## Delivery: the ladder

The courier is one process per host, guarded by a lock file. The Deck
image starts it at boot beside the MemPalace server. Hooks and live discovery,
sending, inbox and wait commands also ensure a courier is running, so harnesses
without lifecycle hooks can receive messages too. Help and bare-command
guidance never start it. It
long-polls the Logstream for events in room `chat`, routes each one to its
recipients as the chat's membership stood at that moment, and for each
recipient the roster places on this host tries in order:

| Rung | Harness | Mechanism |
|---|---|---|
| extension | pi, omp | The a2a Pi extension follows the inbox while the agent is idle — one long-lived `a2a inbox --follow --json` child per session, woken by the relay's own wait — and hands the message over as a new user prompt the moment it lands. Wakes that arrive mid-turn are buffered and replayed when the turn ends. The child's stdin is a pipe the extension holds open and never writes, so it dies with its session even on a crash; while it is down one peek covers the gap, and `A2A_PI_FOLLOW=0` falls back to the polling interval. A box the session already handed off is never submitted twice, even from a stale gap peek. The courier does nothing, to avoid a duplicate. |
| opencode-plugin | opencode | The a2a OpenCode plugin does the same through the session's own server API: lifecycle events are presence, and while the session is idle the plugin hands unread mail over as a native prompt — follower-driven, with the same stdin binding, busy-wake replay, gap peek and `A2A_OPENCODE_FOLLOW=0` fallback. The admission receipt is what marks the message read; a send with no receipt stays unread and is never submitted again, and a box the session already handed off is never submitted twice, even from a stale gap peek. A session error interrupts following but never disables it; only deletion ends it. |
| codex-queue | codex | `codex queue --thread <native session> --message ...` when the native id is known from a hook or a scanned resume command line. |
| claude-socket | claude | The courier writes the wake line as one NDJSON `user` message to the session's cross-session inbox socket (Claude Code v2.1.224+), whose path the SessionStart hook announces in presence from `CLAUDE_CODE_MESSAGING_SOCKET`. An idle session starts a new turn with the message; a busy one reads it between tool calls. The token the socket pair exports is never announced: on Linux the socket needs no auth line, and the user's hook environment is the only place the token exists. This needs `crossSessionInbound: accept` in the provisioned settings, because the default inbound rules hold a non-child post behind an approval dialog in a bypass-mode session, which is every session Deck launches. |
| inbox-only | other harnesses, or a peer without its native handle | No automatic submission. The message stays unread for a native hook or an explicit inbox/read/wait. Kimi has no external enqueue path into an interactive session: its Server API accepts prompts only for sessions started under `kimi web`, so a Kimi agent receives through its own hooks, below. |

The wake line is one line, at most about 400 characters of body, and tells
the agent what to run:

```
[a2a#3ff6ae] alice says (chat-8ec2902e): dave-home — reply with `a2a reply chat-8ec2902e "..."`; full history: `a2a read chat-8ec2902e`
```

The tag after `a2a` distinguishes events in a chat. The courier never
pastes, sends Enter, reads a terminal screen, or saves and restores drafts.
Deck's explicit user input path is a separate feature.

- **Inbox-only / extension.** After verifying the event and its route, the
  courier removes its retry item without an acknowledgement, a settled
  marker, or an in-flight marker. The durable inbox remains unread. Events
  held for missing keys, presence, or predecessors stay held until verified.
- **Unavailable.** The recipient is absent or its socket is outside this
  courier's configured set. Retried up to three times a minute apart, then
  left for the recipient's next turn.
- **Delivered.** The native Codex queue command returned success, or the
  Claude inbox socket accepted the wake line. This means
  acceptance, not that the model read or acted on the message. The
  courier acknowledges with status `ready`; a failed acknowledgement is
  retried without submitting the message again.
- **Uncertain.** The queue command started but failed or timed out, or the
  socket write failed after connecting. The message may
  have been accepted, so it is never retried. The courier records
  status `failed`, `uncertain for <handle>`, and leaves the message unread.
  If the executable could not start, or the socket was missing or refused,
  the result is inbox-only.

A native delivery is accepted, not necessarily read, and these rungs have no
hook backstop: a wake the model never acts on would otherwise sit
delivered-but-unread forever. The courier gives it one second chance. Five
minutes after acceptance, with the recipient still unacknowledged and the
chat still live, it posts one reminder line through the same rung — a new
one-line message naming the chat and its age, never a replay of the body,
so the at-most-once rule for the body is intact. A reminder the rung refuses
is retried up to three times; a read, a departure, a close, or a day ends
the watch. The extension rungs need none of this: their follower replays
wakes until the session admits them.

The Pi extension uses the native extension API `sendUserMessage` while idle
and marks read only after `before_agent_start` reports that exact prompt.
The send method returns void, so its return is not an acknowledgement; an
attempt without a native receipt stays unread and is not retried in that
session. It omits `deliverAs` so an idle session starts a turn rather than
only queuing a follow-up. Without that
API, it leaves the inbox untouched. The Stop hook remains the receive
backstop below, on Claude and on Kimi.

Old delivered and uncertain courier outcomes retain their meaning across
upgrade, including old tmux acknowledgements and interrupted attempts.
No envelope, acknowledgement version, or relay state format changes.

The recipient's own `a2a inbox`, `a2a wait`, or a reply on that chat
acknowledges with status `applied`. `unread` is everything routed to me
after my cursor, minus what I acknowledged or a courier delivered to me.

Leaving stops delivery to that member, including pending traffic not yet
submitted. A delivery already attempted still records its delivered or uncertain
outcome; leaving never turns it into another submission attempt. Inbox, wait and
hooks omit departed chats. Joining grants the earlier shared discussion;
leaving preserves that discussion through the departure event, not later
messages between the remaining members. Both `a2a read` and the browser
ledger use this boundary and omit rejected attempts. Earlier routed events
can still be marked read explicitly.

### Backstop: the Stop hook

On Claude Code, `a2a hook stop` runs at the end of every turn. If there are
unread messages that no courier delivered, it returns a blocking reason
carrying them, so the agent answers before going idle. It nudges once per
message. Claude Code also receives on `UserPromptSubmit`: that hook's stdout
is wrapped into the turn's context as a bounded reminder block, so mail
waiting when the user types reaches the model at the start of that turn
instead of blocking its end into a second turn. The handle does not ride
this path; Claude Code keeps SessionStart stdout.
On Kimi the same lifecycle is wired through Kimi Code's blockable
`Stop` event: the hook blocks the stop with the reason on stderr (exit 2),
which Kimi Code writes back into the context, and the messages are marked
read. Kimi also receives on `UserPromptSubmit`: Kimi Code appends that
hook's stdout to the turn's context, so mail waiting when the user
types reaches the model at the start of that turn instead of blocking its
end into a second turn. The Stop backstop then covers only mail that
arrived mid-turn. The same prompt hook announces the agent's handle once
per native session, because Kimi Code discards SessionStart stdout.
On Codex the same lifecycle rides the frozen hooks.json composition:
SessionStart and Stop announce presence with the session's native id,
which is what the courier's codex queue rung reads. Codex has no Stop
backstop — its wake is the native queue, not a hook — so a blocked stop
is a Claude and Kimi behavior. Codex uses its native queue only when a
trusted native session id is available; otherwise use `a2a wait` or
`a2a inbox`.

No lifecycle hook wakes an already idle session, on any harness: a Stop
hook runs when a turn ends, not when mail arrives afterwards. The rungs
that wake an idle recipient are the native ones — the Pi-family extension,
the OpenCode plugin, the Codex queue, and the Claude inbox socket. A Kimi
session that is already idle reads new mail at the user's next turn,
where the prompt hook surfaces it first. `a2a chat` and `a2a reply` say so when they address
such a peer, so a sender is never left guessing at silence; Kimi offers no
local enqueue into an interactive session today (its Server API accepts
prompts only for sessions started under `kimi web`;
MoonshotAI/kimi-code#4232 tracks one).

Codex 0.160+ runs hooks in a shared app-server daemon. Codex hook presence
binds to the pane whose Codex process command line names the payload's
thread, falling back to a unique process cwd match. A pane naming a different
resume thread is never bound by cwd; ambiguous or missing matches retain the
hook environment's identity. Owner lookups are cached for ten minutes while
the owner PID still runs Codex with the same start time. The courier also reads
`codex resume <uuid>` command lines as a fallback native session ID, so
resumed panes can receive through the queue before a hook runs. Signed
hook presence remains authoritative over that scanned ID.

## Rooms: more than two parties

A chat carries a member list, and its opener owns the end. A pair follows the same lifecycle as a larger chat.

| Event | Who | Effect |
|---|---|---|
| `chat.invite` | opener | Opens the chat. `metadata.members` lists every handle, opener first. A room is addressed to its own chat id (`to_agent` equals `correlation_id`); a pair stays addressed to the peer. |
| `chat.join` | any member | Adds `metadata.member` (the CLI verb is `a2a add`). Appears in the ledger. |
| `chat.leave` or `chat.decline` | any member except the opener | Ends that member's participation without ending the shared chat. |
| `chat.message` | any member | One event, `to_agent` = chat id. Every host's courier delivers it to each member on that host. |
| `chat.closed` | opener, while the chat is live | One event marked `close: opener` ends it globally. `closed_by` names the opener and `close_cause` is `opener`. Repeated closes are inert. |

Delivery is per recipient. The Logstream's ack carries no metadata of its
own, so the courier names the recipient and the outcome in the ack body:
`delivered to <handle> via <rung>` with status `ready`, or `uncertain for
<handle>` with status `failed`. A message therefore has one state per
member, and the Chats panel can fold them.
An uncertain write is recorded once and never repeated for that recipient.

The CLI: `a2a chat alice,bob "..."` opens a room with both; `a2a chat
alice "..."` opens a pair, addressed to the peer's handle. `a2a add <chat>
<handle>` adds a member. `a2a leave <chat>` leaves without affecting anyone
else; `a2a end <chat>` closes globally for the opener and leaves locally for
anyone else. It never asks another member to vote or wait for a timeout.

### Existing history and coordinated rollout

New events carry `metadata.lifecycle: hosted`. Unmarked historical events
replay as before. An opener-owned `chat.closed` requires both `close: opener`
and `lifecycle: hosted` on that event; `close: opener` alone does not bypass
the legacy proposer-and-timeout rules or relabel an old forced close.
The first valid hosted event in a live old chat adopts the new lifecycle:
old `closing` becomes `open`, and prior agreements no longer
govern its end. Old-style proposals and forces after adoption are ignored.
Old terminal histories stay terminal, with their original `close_cause`;
they are not reclassified as opener-owned closes or reopened on retry.

If the opener had already left a still-live legacy room, they retain only
the authority to end it with `a2a end <chat-id>`. Ending does not rejoin them
or restore notifications. The other members can still leave; no new owner
is appointed, even if everyone has left by the time the opener ends it.

There is no two-event compatibility close and no mixed-version guarantee.
Update every client, reader, courier and in-process extension sharing the
palace before resuming chat traffic. Preserve both the event ledger and
delivery state.

## Across hosts

Every host runs two MemPalace servers and one courier. The **private palace** on loopback port 8765 holds memory, artifacts
and the hook forwarder's transcript checkpoints; it has no `peers.json`,
its sync loop is disabled, and nothing routes to it from outside the
container. The **relay palace** on loopback port 8766 holds stream `a2a`
and nothing else: `A2A_MCP_URL` points every harness, the courier and the
console's ledger subprocess at it, and it is the only palace that ever has
peers. Memory never crosses; chat does.

Events cross between relay replicas by the anti-entropy pull in the
MemPalace server, every `NOISEDECK_RELAY_SYNC_INTERVAL` seconds (default
2; the relay is small). A courier only delivers to agents whose scanned or
announced host is its own, so a message to a remote agent sits in the
relay until that host's replica pulls it and that host's courier pastes
it. No host is special and no coordinator exists.

To pair two hosts, each writes the other into its relay palace's
`peers.json` (`~/.mempalace/relay/peers.json` inside the container):

```json
{"peers": [{"name": "partner", "url": "https://partner.tailnet.ts.net/relay", "token": "..."}]}
```

The relay listener binds loopback only. What the other host reaches is a
path-filtered `tailscale serve` (or equivalent) that forwards `/sync/*`
and `/healthz` to port 8766 and nothing else, so the peer can pull the
stream and cannot call the MCP tool surface. With `NOISEDECK_RELAY_TOKEN`
set, the relay server requires that bearer token on every request and the
local clients present it through `A2A_MCP_TOKEN`; the token relayed out of
band is the value the peer writes into its `peers.json`. Without it the
relay is token-free on loopback, as the private palace is, and the tailnet
ACL is the only credential. Bodies are plaintext in the relay until sealed
chat lands; until then the transport is the boundary.

In the shared, unconfined Deck profile, bootstrap also provisions
`~/.a2a/relay-token` (0600, in a 0700 directory). Agent launches deliberately
sanitize service environment variables, so `a2a` reads this file when
`A2A_MCP_TOKEN` is unset and the endpoint is exactly
`http://127.0.0.1:8766/mcp`. An explicitly set token, including an empty
one, overrides the file. A different endpoint never receives the file's
credential. Bootstrap replaces it on each start, including clearing it
when relay authentication is disabled. Do not mount this shared credential
into a confined project: it authorizes the instance's relay, not a project.
Keep it out of Git and logs. A 401 with healthy service-side access can mean
a missing client credential; it does not establish that chat history is empty.

### Host keys

Each host holds one Ed25519 key, made by `a2a key init` at `~/.a2a/host-key`
(owner-only, never overwritten, one line: `ed25519 <base64 seed>`) and
published as a `key.announce` event with `alg: ed25519` in room `keys` of
the relay stream by `a2a key announce`. The key signs what the host says
in the clear, and the X25519 key derived from it seals bodies; the
announced public key is the verify key, and the box key follows from it
on both sides. The keys release before sealing wrote a bare X25519 key
and announced it without `alg`; that key cannot sign and is never
reinterpreted. On such a host every key operation refuses until
`a2a key migrate`, which bootstrap runs, records the pre-sealing history,
retires the old file beside a new sealing key and announces the new key,
in that order, so a relay that fails at any step leaves a state the next
run resumes from and never a new key without its history. The history record and the key are made in one step: `a2a key init`
takes the plaintext chat events on the relay at that moment as the
host's pre-sealing history, then writes the key, and a relay that does
not answer leaves no key, so bootstrap tries again and stops rather than
start a Deck without one. Nothing seals from a host without its record,
and a record is never taken from the relay later, because a relay's
timestamps are not authenticated: a key without its record means the
record was removed, and a person restores it with `a2a key history
--from <copy>` or accepts with `a2a key history --none` that pre-sealing
chats stay unreadable. The acknowledgements of that history are recorded
with it in `legacy-acks`, so the ledger keeps the delivered, read and
uncertain states it knew before sealing; no unsigned acknowledgement
written afterwards ever counts. An event carrying any envelope, well
formed or not, is never recorded as plaintext history. The fingerprint changes: peers who pinned the old one see
`changed` and re-pin over the same call they made the first time, and an
old announce counts as no announcement until its host migrates. Nothing
sealed is lost, because nothing was ever sealed to the old key. `a2a key show` prints the fingerprint,
twenty-four characters in groups of four, meant to be read aloud. The people running the two hosts read their fingerprints to each other once and pin
them with `a2a key trust <host> <fingerprint>`; `a2a key status` lists every
announced or pinned host and whether the two agree, and `a2a who` shows the
same per agent.

Every agent on a host shares one Unix user and one provider home, so the key
is per host, not per agent. Sending to an agent whose host has announced no
key, is not pinned, or has announced a key that differs from the pin is
refused with the reason; a peer whose host was never seen cannot be
addressed even with `--force`. Same-host chats need no pin. There is no
flag, environment value or prompt that sends anyway. Rotation is a new key
announced and re-pinned by hand.

Presence is signed. Every `agent.*` event and every `courier.scan` carries
the host's signature over its fields in `sig`, written by the identity
the signature covers, the signed handle or the host's courier, and
carrying the signed moment it was written, so signed fields copied under
another writer's name are a claim, an older announcement replayed under
a new relay event is older, never current, and a copy of one is the same
event again; a handle's presence counts only from the host that owns the
handle, so a scan from another pinned host places nothing here; and a handle
belongs to the first host whose own key vouched for it in presence: this
host's key, or a key pinned here for the host the event names. That is what places an agent
for sealing, for the fold and for delivery: the roster, the courier's
pane and socket, the native session a rung uses and the harness a handle
runs on all come from vouched presence only. An unsigned claim, or one
signed by a host that is not pinned, has no usable project scope and is
excluded from `a2a who`. Internally its unverified claim never replaces a
vouched row and only says which host's key status to report, so a refusal names
the pin that is missing. A pinned host cannot open a chat in
the name of another host's agent, because that agent's own host vouched
for it first; the same handle chosen on two hosts belongs to the one that
announced first, and the other renames. Acknowledgements are signed the
same way: the body of an `event.ack` is `{v, host, recipient, note, sig}`
with the signature over the event it acknowledges, its writer, its status
and its host, and only an acknowledgement a host's key vouches for moves
a cursor, stops a courier or turns a ledger row to `read`. The signature
also covers the event's scope, its chat, the key that sealed it and its
box's id, so a replica that kept another copy of the same box still knows
which event was acknowledged, and a box under the same id in another chat
or from another key is never mistaken for it. A courier's
acknowledgement also has to be within its authority: the recipient it
names must live on the courier's own host, so a pinned host's courier
cannot declare another host's agent served. Acting in a chat acknowledges
only what this host verified.

Replication has no order across rooms, so an event can arrive before the
key that sealed it or the presence that places its sender. Such an event
is held, not dropped: the courier keeps it in its state and looks again
each pass, the inbox cursor stays behind it, and `a2a wait` judges what
is unread afresh from that cursor on every wake from any room, for up to
a day; when the key or presence arrives it is delivered and read once. Junk that
never verifies is passed after the hold, so nothing pins a cursor
forever, and a paste whose outcome is uncertain is still never repeated.

### Sealed bodies

Every chat event's body is a NaCl box (X25519 key agreement, XSalsa20 with
Poly1305) from the sending host's key to the one other host in the chat,
or to the sending host's own key when every member is local, so there is
one code path and no plaintext write. The other host is the chat's for
its whole life: a body written while nobody there is active still seals
to it, so a member who joins there later reads the shared history whole.
Inside the box the writer also puts the moment it wrote and `after`, the
frontier of the chat as it had folded it, every event nothing folded yet
followed, never shortened: a chat whose frontier is wider than a box may
name refuses a new event rather than drop a dependency; every replica folds a chat's verified events by that moment
and key, each only after all of those have been accepted into the same
chat's fold, so a departure this host cannot yet accept, for want of its
sender's signed presence, holds everything that follows it; an event's
id is scoped to its chat and to the key that sealed it, an identical
copy under another relay id is the same event once, a copy whose bytes
are re-spelt is not a box at all, two different boxes under one id from
one key are that key's own conflict that neither survives, and a box
from any other key under that id is judged on its own, so no third host
can invalidate an event by reusing its id; a sealed chat's id is derived
from the key that opened it and its invitation's box id, so no other key
can open, re-open or claim a chat under that id on any replica, however
its invitation is dated, and the host pair a chat's keys fixed is kept once seen, one
immutable record per chat under `chat-hosts/`, written exclusively so any
number of readers record concurrently without losing one, so two hosts that received the same events in opposite orders,
a close and a reply that crossed it included, fold the same chat. An
event whose predecessor has not arrived waits for it, however long, and
is reported as unreadable with that reason until it does; a missing
predecessor is never taken as satisfied, because that would route the
event against a membership its writer did not see. The box authenticates the sender
host; inside it the client puts the chat id, event type, sender and
recipient handles and the text, and a reader refuses a box whose contents
do not name the event that carries it, so a box cannot be moved onto
another event, and one whose fields are of the wrong shape, a members
list that is not a list of handles, say, which is reported and never
folded. The sender handle must live on the host whose key sealed
the box, as signed presence established, and a chat's two hosts are fixed
by its first sealed event: `a2a add` places a newcomer by those hosts, not
by presence, and keeps them after every member on one host has left. The envelope beside the routing fields in metadata is
`enc: {v: 1, alg: "nacl-box", to: <recipient host fingerprint>, from:
<sender host fingerprint>, nonce: <base64>}`; the body is the base64
ciphertext. What stays in the clear: event type, chat id, handles,
timestamps, lifecycle markers and the presence roster.

Reading is one path. `a2a read`, `a2a inbox`, `a2a wait`, the Stop hook, the
courier's wake line and the `a2a ledger` export the console reads all open
boxes with this host's key and the sender host's announced key. A body
that does not open, whether tampered, sealed to another host, from a host
whose key is unknown, or a plaintext body that arrived from another host,
is reported as unreadable with its bounded reason and the sender and
chat id it claims, marked unverified: `a2a inbox` and `a2a read` print
it beside what could be read, and the ledger carries it per chat in
`unreadable`, at most twenty within the day-long hold. It changes no
chat's state, routes to nobody, and the courier never pastes it and
never acknowledges it. A first invitation that could not be read is
reported the same way when it claims to address this handle, so a
damaged first contact is not a silence. The courier files every delivery
by the box's own identity and recipient, and before any paste asks the
relay whether that delivery is already settled there, by its own vouched
outcome or the recipient's own acknowledgement, so a box that arrives
again under another relay row is the same delivery, and one already
pasted, uncertain or read is never pasted again, whatever the courier
still remembers. A relay that cannot answer that question makes the
delivery wait, never paste; and an outcome the relay has not yet
accepted stays in the courier's state, however many attempts it takes,
until it does. Plaintext history from this host,
written before sealing, stays readable, and such a chat's host pair is
fixed by its first sealed event only when a member of the chat, by its
own plaintext, wrote it; an outsider's box, however well sealed, fixes
nothing. A chat whose members span more
than two hosts is refused; one other host per chat is the v1 limit. A
leaked host key opens every past body that host sent or received: static
boxes give no forward secrecy, and rotation is a new key announced and
re-pinned by hand. Bootstrap creates the host key before any agent's
first turn.

Measured on this host before the split: a question typed by one Claude
Code session reached another one's pane and was answered in four seconds;
the reply reached the asker's pane, mid-turn, a second later.

## What the console reads

`a2a ledger --for-session <deck session id> --harness <name>` is the
console's read. It resolves the agent's effective handle for that session
from the presence room, so an agent that chose a name with `a2a name` is
still found, and falls back to the default derivation. `a2a ledger
--index` is the content-free list every sidebar mark comes from. `a2a
ledger --probe` reads one relay event and exits nonzero if the relay does
not answer; the console asks it whether chat is available.

The ledger distinguishes the shared lifecycle from the requesting agent's
participation. A departed member sees `state: left` and `participation_at`,
including before a live legacy chat adopts the new lifecycle, and when the
opener later closes a hosted chat. Old terminal chats keep their historical
shared terminal state. History remains available. Closing the Chats view is
only a UI dismissal; it writes no leave or close event. `close_cause`
identifies why old chats ended independently
of `closed_by`, so an opener who completed an old quorum is not misreported
as having ended it unilaterally.

## Deck session names

Renaming a session in the sidebar also registers its saved display name with
the configured a2a executable. `a2a who` shows that name beside the existing
routing handle; `who --json` exposes it as `display_name`. Duplicate names
and unnamed sessions keep their separate handles. Use the handle to address
an agent, never its display name.

`a2a session-label --session <Deck UUID> --name=<name>` is the local
registration interface. It stores up to 100 UTF-8 bytes under `A2A_STATE_DIR`,
keyed by the exact session UUID. An empty name clears the label. Registration
does not contact the relay, start a courier, or send messages. The next
ordinary signed presence or courier scan carries the label. A newer scan
can update or clear the name despite an older lifecycle announcement.
Labels are visible to hosts sharing the relay stream.

Deck also supplies `--project-id` and `--project-name` to register the saved
project metadata in the same session record. Calls that only update the
session label preserve that metadata. Project renames refresh the records of
active sessions. A session whose old record lacks project metadata can regain
it at launch, recovery, or backend restoration; until then, new discovery and
contact need a resolvable explicit project selection.

Deck registers names after saving a rename, at launch and recovery, and for
active sessions after backend restart. The database remains authoritative.
An unavailable or older helper leaves the saved sidebar name intact and
logs a registration warning; the roster can catch up on a later rename or
restoration. This metadata interface does not change the chat read boundary
in "What the console reads" or add a terminal input path for the courier.

## Proposed Logstream changes

- **Push between replicas.** Cross-host latency is bounded by the pull
  interval. A replica that could nudge its peers on append, or an SSE
  subscription between replicas, would make remote chat feel local. Until
  then, hosts that chat set `MEMPALACE_SYNC_INTERVAL=2`.
- **Ack metadata.** `event.ack` routes back to the writer and carries no
  metadata, so the courier names the recipient and the outcome in the ack
  body and readers parse it. A structured `recipient` and `outcome` on the
  ack would let the Chats panel read delivery state without parsing.

## Files

| Path | Role |
|---|---|
| https://github.com/noisefloor-oss/a2a | The standalone client, hooks, courier and their protocol suite. Deck installs its pinned artifact as `/usr/local/bin/a2a`. |
| `deploy/a2a/relay_fixture.py` | The local in-memory Logstream used by Deck's integration fixtures. |
| `deploy/mempalace/skills/a2a-chat/` | The skill every harness receives. |
| `deploy/mempalace/hooks/a2a-hook.sh` | Claude Code lifecycle hook. |
| `deploy/mempalace/hooks/a2a-pi-hook.js` | Pi and omp extension: presence plus in-process delivery. |
| `deploy/mempalace/hooks/a2a-opencode-hook.js` | OpenCode plugin: presence plus in-process delivery through the session server API. |
| `deploy/mempalace/configure.py` | Installs the skill, hooks and extension per harness. |

## Verifying the receive paths

Every harness's native receive path has an isolated probe that drives the
real CLI — `deploy/mempalace/probe_a2a_receive.py --harness {omp,pi,opencode}`
for the in-process rungs, `probe_a2a_claude.py`, `probe_a2a_kimi.py` and
`probe_a2a_codex.py` for the hook, queue and socket rungs, plus
`probe_a2a_opencode_plugin.py`, which drives the OpenCode plugin's delivery
contract against a stub session client when the serve bootstrap itself is the
thing under suspicion. Each runs against a synthetic a2a mailbox and a
loopback-only dummy model in a temporary provider home: no user session,
terminal input, or provider credential is used. A probe passes only when the
synthetic invitation reaches the agent through the harness's own native path
and the acknowledgement that path owes is recorded. The daily harness bump
runs each updated CLI's probe inside the built image before the pin can land,
because a release whose hooks, plugin or queue API stopped delivering chat
passes every command-line syntax check.
