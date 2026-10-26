// The Chats ledger the preview renders: the
// live room with an uncertain recipient, a four-member invitation
// whose member line folds, and modern and historical chats under Earlier.
// Handles follow the fixture sessions' ids the way the
// server derives them: harness name plus the id's first four characters.
import type { ChatDeliveryState, ChatIndex, ChatLedger, Chat, ChatRow } from "../api/chats.ts";

const CLAUDE = "claude-sess";
const CODEX = "codex-sess";
const KIMI = "kimi-sess";
const OPENCODE = "opencode-sess";

function at(hhmmss: string): string {
  const now = new Date();
  const [h = 0, m = 0, s = 0] = hhmmss.split(":").map(Number);
  now.setHours(h, m, s, 0);
  return now.toISOString().replace(/\.\d{3}Z$/, "Z");
}

// A message row from one member to the rest, with each recipient's delivery state.
function message(id: string, tag: string, from: string, text: string, sent: string, to: Record<string, ChatDeliveryState>): ChatRow {
  return { id, tag, kind: "message", from, member: null, text, at: sent, to };
}

// The stress room from the 2026-09-23 audit's reference (chat-f0486445:
// 17 members, 41 rows), regenerated deterministically: every row carries
// real text, so a rendering that produces filler or empty rows fails the
// probe that counts words.
const STRESS_MEMBERS = [CLAUDE, ...Array.from({ length: 16 }, (_, i) => `agent-${String(i + 1).padStart(4, "0")}`)];
const stressRows: ChatRow[] = Array.from({ length: 41 }, (unused, i) => message(
  `evt_s${i}`, `a2a#s${String(i).padStart(2, "0")}`, STRESS_MEMBERS[i % STRESS_MEMBERS.length]!,
  `stress message ${i}: the pin holds on all three probes`, at(`15:${String(Math.floor(i / 2)).padStart(2, "0")}:${String((i * 7) % 60).padStart(2, "0")}`),
  { [CODEX]: "read", [KIMI]: i % 3 === 0 ? "uncertain" : "delivered" },
));

const stress: Chat = {
  id: "chat-f0486445",
  state: "open",
  waiting_s: null,
  forced: false,
  opened_by: CLAUDE,
  opened_at: at("15:00:00"),
  last: stressRows.at(-1)!.at,
  members: STRESS_MEMBERS,
  left: [],
  remaining: STRESS_MEMBERS,
  harnesses: { [CLAUDE]: "claude" },
  close_requested_by: null,
  closed_by: null,
  messages: 41,
  rows: stressRows,
  handshake: [],
};

const room: Chat = {
  id: "chat-a1b2c3d4",
  state: "open",
  waiting_s: null,
  forced: false,
  opened_by: CLAUDE,
  opened_at: at("14:22:00"),
  last: at("14:23:02"),
  members: [CLAUDE, CODEX, KIMI],
  left: [],
  remaining: [CLAUDE, CODEX, KIMI],
  harnesses: { [CLAUDE]: "claude", [CODEX]: "codex", [KIMI]: "kimi" },
  close_requested_by: null,
  closed_by: null,
  messages: 3,
  rows: [
    { id: "evt_1_3ff6ae", tag: "a2a#3ff6ae", kind: "message", from: CLAUDE, member: null, text: "Which port does the provider proxy bind in dev?", at: at("14:22:07"), to: { [CODEX]: "read", [KIMI]: "read" } },
    { id: "evt_2_3ff6b1", tag: "a2a#3ff6b1", kind: "message", from: CODEX, member: null, text: "4318, from vite.config.ts:41.", at: at("14:22:31"), to: { [CLAUDE]: "read", [KIMI]: "delivered" } },
    { id: "evt_3_3ff6c0", tag: "a2a#3ff6c0", kind: "message", from: CLAUDE, member: null, text: "And the health path — is it /healthz or /health?", at: at("14:23:02"), to: { [CODEX]: "read", [KIMI]: "uncertain" } },
  ],
  handshake: [],
};

const invitation: Chat = {
  id: "chat-e5f6a7b8",
  state: "invited",
  waiting_s: null,
  forced: false,
  opened_by: CLAUDE,
  opened_at: at("14:24:00"),
  last: at("14:24:00"),
  members: [CLAUDE, CODEX, KIMI, OPENCODE],
  left: [],
  remaining: [CLAUDE, CODEX, KIMI, OPENCODE],
  close_requested_by: null,
  closed_by: null,
  messages: 0,
  rows: [],
  handshake: [],
};

const closed: Chat = {
  id: "chat-7c0e91f2",
  state: "closed",
  waiting_s: null,
  forced: false,
  opened_by: CLAUDE,
  opened_at: at("13:41:00"),
  last: at("13:58:00"),
  members: [CLAUDE, OPENCODE, "scout"],
  left: [],
  remaining: [CLAUDE, OPENCODE, "scout"],
  // scout chose its name with `a2a name`; presence says it runs on pi.
  harnesses: { [CLAUDE]: "claude", [OPENCODE]: "opencode", scout: "pi" },
  close_requested_by: null,
  closed_by: CLAUDE,
  close_cause: "opener",
  messages: 6,
  // The whole exchange, so opening the ended chat has something to show.
  rows: [
    message("m1", "a2a#7c0e01", CLAUDE, "scout, can you take the pi contract probe while I finish the pin?", at("13:41:00"), { [OPENCODE]: "read", scout: "read" }),
    message("m2", "a2a#7c0e02", "scout", "on it; two failing cases so far, both in resume", at("13:44:12"), { [CLAUDE]: "read", [OPENCODE]: "read" }),
    message("m3", "a2a#7c0e03", OPENCODE, "the resume path changed in 0.9; the probe wants the new marker", at("13:47:40"), { [CLAUDE]: "read", scout: "read" }),
    message("m4", "a2a#7c0e04", "scout", "confirmed, marker updated, probe green", at("13:52:05"), { [CLAUDE]: "read", [OPENCODE]: "read" }),
    message("m5", "a2a#7c0e05", CLAUDE, "pushed the pin; both probes green in CI", at("13:57:30"), { [OPENCODE]: "read", scout: "read" }),
    message("m6", "a2a#7c0e06", CLAUDE, "thanks both, closing this one", at("13:58:00"), { [OPENCODE]: "read", scout: "read" }),
  ],
  handshake: [],
};

const declined: Chat = {
  id: "chat-2d9a40bb",
  state: "declined",
  waiting_s: null,
  forced: false,
  opened_by: KIMI,
  opened_at: at("09:12:00"),
  last: at("09:12:20"),
  members: [KIMI, CLAUDE],
  left: [CLAUDE],
  remaining: [KIMI],
  close_requested_by: null,
  closed_by: null,
  messages: 1,
  rows: [],
  handshake: [],
};

const legacyQuorum: Chat = {
  ...closed,
  id: "chat-legacy-quorum",
  close_requested_by: OPENCODE,
  closed_by: CLAUDE,
  close_cause: "quorum",
  handshake: [
    { member: CLAUDE, agreed: true, at: at("13:58:00"), left: false, left_at: null },
    { member: OPENCODE, agreed: true, at: at("13:57:40"), left: false, left_at: null },
    { member: "scout", agreed: true, at: at("13:58:00"), left: false, left_at: null },
  ],
};

const departed: Chat = {
  ...room,
  id: "chat-member-left",
  // No hosted event has been written: the other members still see a legacy proposal.
  state: "closing",
  opened_by: CODEX,
  last: at("14:28:00"),
  left: [CLAUDE],
  remaining: [CODEX, KIMI],
  close_requested_by: CODEX,
  handshake: [
    { member: CLAUDE, agreed: false, at: null, left: true, left_at: at("14:26:00") },
    { member: CODEX, agreed: true, at: at("14:25:00"), left: false, left_at: null },
    { member: KIMI, agreed: false, at: null, left: false, left_at: null },
  ],
  messages: 1,
  rows: [
    { id: "evt_departed", tag: "a2a#000011", kind: "left", from: CLAUDE, member: null, text: "", at: at("14:26:00"), to: { [CODEX]: "read", [KIMI]: "read" } },
    { id: "evt_after_departure", tag: "a2a#000012", kind: "message", from: CODEX, member: null, text: "We can keep discussing it here.", at: at("14:28:00"), to: { [KIMI]: "read" } },
  ],
};

const continuing: Chat = {
  ...departed,
  id: "chat-another-member-left",
  state: "open",
  opened_by: CLAUDE,
  left: [CODEX],
  remaining: [CLAUDE, KIMI],
  close_requested_by: null,
  handshake: [],
  rows: [
    { id: "evt_other_departed", tag: "a2a#000021", kind: "left", from: CODEX, member: null, text: "", at: at("14:26:00"), to: { [CLAUDE]: "read", [KIMI]: "read" } },
    { id: "evt_other_continues", tag: "a2a#000022", kind: "message", from: CLAUDE, member: null, text: "We can finish this together.", at: at("14:28:00"), to: { [KIMI]: "read" } },
  ],
};

const ALL: Chat[] = [room, stress, invitation, closed, declined, legacyQuorum, departed, continuing];

const HANDLES: Record<string, string> = {
  "sess-1": CLAUDE,
  "sess-2": CODEX,
  "sess-3": KIMI,
  "sess-9": OPENCODE,
};

export function fixtureChatLedger(sessionID: string): ChatLedger {
  const agent = HANDLES[sessionID] ?? "";
  return {
    agent,
    chats: ALL.filter((c) => c.members.includes(agent)).map((chat) =>
      (chat === departed || chat === continuing) && chat.left.includes(agent)
        ? { ...chat, state: "left", participation_at: at("14:26:00") }
        : chat),
  };
}

export function fixtureChatIndex(): ChatIndex {
  return {
    chats: ALL.map((c) => ({
      id: c.id,
      state: c.state,
      members: c.members,
      left: c.left,
      last: c.last,
      last_row: c.rows.at(-1)?.id ?? null,
      rows: c.rows.length,
    })),
  };
}
