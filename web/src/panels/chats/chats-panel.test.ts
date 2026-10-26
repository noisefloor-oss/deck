// The Chats ledger's folds, DOM-free: the row word, the breakdown, the
// news sentence, unseen, and the state transitions a flapping read must
// not blank.
import assert from "node:assert/strict";
import test from "node:test";

import { ApiError } from "../../api/http.ts";
import type { ApiClient } from "../../api/contracts.ts";
import type { Chat, ChatLedger, ChatRow } from "../../api/chats.ts";
import { agentHandle } from "../../lib/agent-handle.ts";
import { ChatsPanelModel, breakdown, chatHandles, chatMarks, endedHint, formatWait, memberLine, rowRecord, rowSettled, rowWord, stampBefore, stateWord, worst } from "./chats-panel.ts";
import { fixtureChatIndex, fixtureChatLedger } from "../../app/fixture-chats.ts";

function row(to: ChatRow["to"], kind: ChatRow["kind"] = "message"): ChatRow {
  return { id: "evt_1", tag: "a2a#3ff6ae", kind, from: "claude-61c3", member: null, text: "hi", at: "2026-09-03T14:22:07Z", to };
}

function client(ledger: () => Promise<ChatLedger>): ApiClient {
  return { getSessionChats: () => ledger() } as unknown as ApiClient;
}

test("the row word is the worst state across recipients, uncertain outranking all", () => {
  assert.equal(worst(row({ a: "read", b: "read" })), "read");
  assert.equal(worst(row({ a: "read", b: "delivered" })), "delivered");
  assert.equal(worst(row({ a: "sent", b: "read" })), "sent");
  assert.equal(worst(row({ a: "read", b: "uncertain", c: "sent" })), "uncertain");
  assert.equal(worst(row({})), null, "an event seen by nobody has no word");
  assert.equal(breakdown(row({ "codex-9f2a": "read", "kimi-2b77": "uncertain" })), "codex-9f2a read · kimi-2b77 uncertain");
});

test("chat words and folded hints follow the courier's vocabulary", () => {
  const chat = fixtureChatLedger("sess-1").chats[0] as Chat;
  assert.equal(stateWord(chat), "open");
  assert.equal(stateWord({ ...chat, state: "waiting", waiting_s: 190 }), "waiting · 3m");
  assert.equal(formatWait(30), "< 1m");
  assert.equal(formatWait(7200), "2h");
  const declined = fixtureChatLedger("sess-1").chats.find((c) => c.state === "declined") as Chat;
  assert.match(endedHint(declined), /^claude-sess declined the invitation at \d\d:\d\d$/);
  assert.match(endedHint({ ...declined, state: "forced", close_requested_by: "kimi-sess" }), /^kimi-sess forced the end at/);
  const closed = fixtureChatLedger("sess-1").chats.find((c) => c.state === "closed") as Chat;
  assert.match(endedHint(closed), /^claude-sess opened it and ended it for everyone at \d\d:\d\d$/, "the current close is the opener's decision");
  assert.match(endedHint({ ...closed, closed_by: closed.opened_by, close_cause: "opener" }), /^claude-sess opened it and ended it for everyone at \d\d:\d\d$/, "the opener's close is not a quorum");
  assert.match(endedHint({ ...closed, closed_by: closed.opened_by, close_cause: "quorum" }), /^Everyone agreed to end it at \d\d:\d\d$/, "a legacy quorum completed by the opener remains a quorum");
  assert.match(endedHint({ ...closed, close_cause: undefined }), /^It ended at \d\d:\d\d$/, "an older server supplies no close cause");
  for (const departure of [
    { members: ["claude-sess", "codex-sess"], left: ["claude-sess"], remaining: ["codex-sess"] },
    { members: ["claude-sess", "codex-sess", "kimi-sess", "opencode-sess"], left: ["claude-sess"], remaining: ["codex-sess", "kimi-sess", "opencode-sess"] },
  ]) {
    assert.match(endedHint({ ...closed, ...departure, closed_by: null, close_cause: "departure" }), /^It ended at \d\d:\d\d after a member left$/, "departure copy does not invent agreement or depend on who is viewing it");
  }
});

test("the ledger folds into live and earlier, the news sentence, and unseen", async () => {
  const model = new ChatsPanelModel(client(async () => fixtureChatLedger("sess-1")), "sess-1");
  await model.refresh();
  assert.equal(model.state, "ready");
  assert.equal(model.agent, "claude-sess");
  assert.deepEqual(model.live.map((c) => c.id), ["chat-a1b2c3d4", "chat-f0486445", "chat-e5f6a7b8", "chat-another-member-left"]);
  assert.deepEqual(model.earlier.map((c) => c.state), ["closed", "declined", "closed", "left"]);
  assert.equal(model.news, "claude-sess is in a chat with codex-sess, kimi-sess · a2a#3ff6c0 written, no echo seen");
  assert.equal(model.unseen, true, "nothing has been in front of the user yet");
  model.setPanelOpen(true);
  assert.equal(model.unseen, false, "opening the panel is what makes rows seen");
  model.setPanelOpen(false);
  await model.refresh();
  assert.equal(model.unseen, false, "the same rows stay seen while the panel is closed");
});

test("a new row arriving while the panel is closed is unseen; open, it is seen as it lands", async () => {
  let ledger = fixtureChatLedger("sess-1");
  const model = new ChatsPanelModel(client(async () => ledger), "sess-1");
  await model.refresh();
  model.setPanelOpen(true);
  model.setPanelOpen(false);
  const chat = ledger.chats[0] as Chat;
  const first = chat.rows[0] as ChatRow;
  ledger = { ...ledger, chats: [{ ...chat, rows: [...chat.rows, { ...first, id: "evt_9", tag: "a2a#9", text: "new" }] }, ...ledger.chats.slice(1)] };
  await model.refresh();
  assert.equal(model.unseen, true);
  model.setPanelOpen(true);
  const grown = ledger.chats[0] as Chat;
  ledger = { ...ledger, chats: [{ ...grown, rows: [...grown.rows, { ...first, id: "evt_10", tag: "a2a#10", text: "newer" }] }, ...ledger.chats.slice(1)] };
  await model.refresh();
  assert.equal(model.unseen, false, "arrived while open: already in front of the user");
});

test("a failed read after data keeps the data; before data it is unavailable; no handle is its own state", async () => {
  let fail: unknown = null;
  const model = new ChatsPanelModel(client(async () => {
    if (fail !== null) throw fail;
    return fixtureChatLedger("sess-1");
  }), "sess-1", { onError: () => {} });
  fail = new Error("boom");
  await model.refresh();
  assert.equal(model.state, "unavailable");
  fail = null;
  await model.refresh();
  assert.equal(model.state, "ready");
  fail = new Error("flap");
  await model.refresh();
  assert.equal(model.state, "ready", "a flapping read never blanks the ledger");
  assert.equal(model.chats.length, 8);
  assert.equal(model.error, "flap");
  fail = new ApiError(404, "no_agent_handle", "no handle");
  await model.refresh();
  assert.equal(model.state, "no-agent");
  assert.equal(model.news, null);
});

test("a failed first read is off when the status says agent chat is not available, unavailable otherwise", async () => {
  const failing = (status: (() => Promise<{ available: boolean }>) | undefined) => new ChatsPanelModel({
    getSessionChats: async () => { throw new Error("boom"); },
    getChatsStatus: status,
  } as unknown as ApiClient, "sess-1", { onError: () => {} });
  for (const [name, status, state] of [
    ["off", async () => ({ available: false }), "off"],
    ["available", async () => ({ available: true }), "unavailable"],
    ["status failed", async () => { throw new Error("down"); }, "unavailable"],
    ["status not offered", undefined, "unavailable"],
  ] as const) {
    const model = failing(status);
    await model.refresh();
    assert.equal(model.state, state, name);
  }
});

test("a stalled status read falls back to unavailable and the next refresh recovers", async () => {
  let fail = true;
  let received: AbortSignal | undefined;
  const model = new ChatsPanelModel({
    getSessionChats: async () => {
      if (fail) throw new Error("boom");
      return fixtureChatLedger("sess-1");
    },
    getChatsStatus: (signal?: AbortSignal) => new Promise((_, reject) => {
      received = signal;
      signal?.addEventListener("abort", () => reject(signal.reason));
    }),
  } as unknown as ApiClient, "sess-1", { onError: () => {} });
  model.statusTimeoutMS = 20;
  await model.refresh();
  assert.ok(received, "the status read carries a deadline signal");
  assert.equal(model.state, "unavailable");
  fail = false;
  await model.refresh();
  assert.equal(model.state, "ready");
});

test("sidebar marks come from the content-free index and each session's handle", () => {
  const handles = new Map([["sess-1", "claude-sess"], ["sess-2", "codex-sess"], ["sess-9", "opencode-sess"], ["sess-10", "shell-sess"]]);
  const seen = new Map<string, string | null>();
  const marks = chatMarks(fixtureChatIndex(), handles, (sid, cid) => seen.get(`${sid}|${cid}`));
  assert.equal(marks.get("sess-1"), "unseen");
  assert.equal(marks.get("sess-2"), "unseen");
  assert.equal(marks.get("sess-9"), "live", "an invitation with no rows is live and holds nothing unseen");
  assert.equal(marks.get("sess-10"), null);
  seen.set("sess-2|chat-a1b2c3d4", "evt_3_3ff6c0");
  seen.set("sess-2|chat-member-left", "evt_after_departure");
  assert.equal(chatMarks(fixtureChatIndex(), handles, (sid, cid) => seen.get(`${sid}|${cid}`)).get("sess-2"), "live");
});

test("chatHandles resolves the session's effective handle, presence first, default as fallback", () => {
  const renamed = { id: "61c37a0c-2158-4134-b99e-0914dc1471ff", provider: "claude" };
  const plain = { id: "8e70bbbb-0000-0000-0000-000000000000", provider: "codex" };
  const shell = { id: "she11111-0000-0000-0000-000000000000", provider: "shell" };
  const sessions = [renamed, plain, shell];
  const index = { chats: [], sessions: { [renamed.id]: "reviewer-933" } };
  const handles = chatHandles(index, sessions);
  assert.equal(handles.get(renamed.id), "reviewer-933", "the renamed agent's handle wins over the default derivation");
  assert.equal(handles.get(plain.id), "codex-8e70", "no presence word: the default derivation");
  assert.equal(handles.has(shell.id), false, "a harness a2a does not know has no handle");
  const withoutMap = chatHandles({ chats: [] }, sessions);
  assert.equal(withoutMap.get(renamed.id), "claude-61c3", "an index without the map falls back everywhere");
});

test("a renamed agent's session is marked by its effective handle, never its stale default", () => {
  const session = { id: "61c37a0c-2158-4134-b99e-0914dc1471ff", provider: "claude" };
  const live = (members: string[]) => ({ id: "chat-x", state: "open" as const, members, left: [], last: null, last_row: "evt_9", rows: 1 });
  const renamed = { "61c37a0c-2158-4134-b99e-0914dc1471ff": "reviewer-933" };
  const stale = { chats: [live(["claude-61c3"])], sessions: renamed };
  assert.equal(chatMarks(stale, chatHandles(stale, [session]), () => undefined).get(session.id), null,
    "the stale default-handle membership marks nothing: the panel's ledger does not hold that chat either");
  const current = { chats: [live(["reviewer-933"])], sessions: renamed };
  assert.equal(chatMarks(current, chatHandles(current, [session]), () => undefined).get(session.id), "unseen",
    "membership under the chosen name marks the session");
});

test("a departure is past only for the departing agent, with no news or unseen mark", async () => {
  const departed = fixtureChatLedger("sess-1").chats.find((c) => c.id === "chat-member-left") as Chat;
  assert.equal(stateWord(departed), "left");
  assert.notEqual(departed.participation_at, departed.last, "the remaining members have since continued");
  assert.match(endedHint(departed, "claude-sess"), /^claude-sess left the chat at 14:26$/);
  assert.equal(endedHint({ ...departed, participation_at: undefined }, "claude-sess"), "claude-sess left the chat", "a missing departure time never falls back to the later global activity");
  const model = new ChatsPanelModel(client(async () => ({ agent: "claude-sess", chats: [departed] })), "sess-1");
  await model.refresh();
  assert.deepEqual(model.live, []);
  assert.deepEqual(model.earlier, [departed]);
  assert.equal(model.news, null);
  assert.equal(model.unseen, false);
  const continuing = fixtureChatLedger("sess-2").chats.find((c) => c.id === departed.id) as Chat;
  assert.equal(continuing.state, "closing", "remaining members still see the live legacy proposal");
  const index = { chats: fixtureChatIndex().chats.filter((c) => c.id === departed.id) };
  const marks = chatMarks(index, new Map([["departed", "claude-sess"], ["remaining", "codex-sess"]]), () => undefined);
  assert.equal(marks.get("departed"), null);
  assert.equal(marks.get("remaining"), "unseen");
});

test("a pre-adoption legacy departure stays past despite a proposal and unfinished handshake", async () => {
  const departed = fixtureChatLedger("sess-1").chats.find((c) => c.id === "chat-member-left") as Chat;
  assert.equal(departed.state, "left", "the backend projects participation before any hosted event");
  assert.equal(departed.close_requested_by, "codex-sess");
  assert.ok(departed.handshake.some((member) => !member.left && !member.agreed));
  const model = new ChatsPanelModel(client(async () => ({ agent: "claude-sess", chats: [departed] })), "sess-1");
  await model.refresh();
  assert.deepEqual(model.live, [], "historical closing facts never override the backend's left state");
  assert.deepEqual(model.earlier, [departed]);
  assert.equal(model.news, null);
  assert.equal(model.unseen, false);
  assert.equal(endedHint(departed, model.agent), "claude-sess left the chat at 14:26");
  const entry = fixtureChatIndex().chats.find((c) => c.id === departed.id)!;
  assert.equal(entry.state, "closing", "the shared index still reports the legacy chat live");
  assert.equal(chatMarks({ chats: [entry] }, new Map([["sess-1", model.agent]]), () => undefined).get("sess-1"), null);
});

test("the lone opener remains in a live chat without an empty member sentence", async () => {
  const chat = fixtureChatLedger("sess-1").chats[0] as Chat;
  const alone = { ...chat, left: ["codex-sess", "kimi-sess"], remaining: ["claude-sess"], rows: [] };
  const model = new ChatsPanelModel(client(async () => ({ agent: "claude-sess", chats: [alone] })), "sess-1");
  await model.refresh();
  assert.equal(model.live.length, 1);
  assert.equal(model.news, "claude-sess is in a chat with no other participants");
});

test("agentHandle derives the handle the server derives", () => {
  assert.equal(agentHandle({ id: "61C37A0C-2158", provider: "claude" }), "claude-61c3");
  assert.equal(agentHandle({ id: "8e70bbbb", provider: "codex" }), "codex-8e70");
  assert.equal(agentHandle({ id: "aaaa", provider: "shell" }), null);
});

test("the messages-app rule: a word only while owed and only on the newest row, uncertain anywhere", () => {
  assert.equal(rowWord(row({ a: "read", b: "read" }), true), null, "read is not a word");
  assert.equal(rowWord(row({ a: "read", b: "delivered" }), true), "delivered");
  assert.equal(rowWord(row({ a: "read", b: "delivered" }), false), null, "an older owed row carries no word");
  assert.equal(rowWord(row({ a: "uncertain", b: "read" }), false), "uncertain", "the failure case keeps its word on any row");
  assert.equal(rowWord(row({ a: "delivered" }, "added"), true), null, "an add is quiet unless uncertain");
  assert.equal(rowWord(row({}), true), null);
  assert.equal(rowSettled(row({ a: "read", b: "read" })), true);
  assert.equal(rowSettled(row({ a: "read", b: "sent" })), false);
  assert.equal(rowSettled(row({ a: "sent" }, "left")), true, "a membership fact reads quiet from the start");
  assert.equal(rowSettled(row({})), true);
});

test("time is a stamp: before the first row and at any gap of five minutes", () => {
  const a = { ...row({ a: "read" }), at: "2026-09-03T14:22:07Z" };
  const b = { ...row({ a: "read" }), at: "2026-09-03T14:27:06Z" };
  const c = { ...row({ a: "read" }), at: "2026-09-03T14:27:07Z" };
  assert.equal(stampBefore(undefined, a), true);
  assert.equal(stampBefore(a, b), false, "4:59 is under the gap");
  assert.equal(stampBefore(a, c), true, "5:00 is the gap");
  assert.equal(stampBefore(a, { ...b, at: "not a time" }), false, "an unreadable time gets no stamp");
  assert.equal(stampBefore(undefined, { ...a, at: "not a time" }), false, "nor does an unreadable first row");
  assert.equal(stampBefore({ ...a, at: "not a time" }, b), false, "a gap from an unreadable time cannot be measured");
});

test("the hover record leads with the id, carries the time unless every row shows it, then the breakdown", () => {
  const r = row({ "codex-9f2a": "read", "kimi-2b77": "uncertain" });
  assert.match(rowRecord(r, false), /^a2a#3ff6ae · \d\d:\d\d:\d\d · codex-9f2a read · kimi-2b77 uncertain$/);
  assert.equal(rowRecord(r, true), "a2a#3ff6ae · codex-9f2a read · kimi-2b77 uncertain");
  assert.match(rowRecord(row({}, "added"), false), /^a2a#3ff6ae · \d\d:\d\d:\d\d · Seen by nobody yet$/);
});

test("the member line names up to three and folds from four unless expanded", () => {
  const chat = fixtureChatLedger("sess-1").chats[0] as Chat;
  assert.deepEqual(memberLine(chat, false), { named: chat.members, folded: [], more: "", foldable: false });
  const four = { ...chat, members: [...chat.members, "opencode-sess"], left: [] };
  assert.deepEqual(memberLine(four, false), { named: ["claude-sess"], folded: ["codex-sess", "kimi-sess", "opencode-sess"], more: "+3", foldable: true });
  assert.deepEqual(memberLine(four, true), { named: four.members, folded: [], more: "", foldable: true });
  const oneLeft = { ...four, left: ["opencode-sess"] };
  assert.equal(memberLine(oneLeft, false).foldable, false, "a member who left is off the line");
});
