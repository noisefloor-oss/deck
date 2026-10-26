import assert from "node:assert/strict";
import test from "node:test";
import { ConversationAcknowledgements, type ConversationAttention } from "./conversation-attention.ts";
import type { SessionSummary } from "../api/sessions.ts";

const fact: ConversationAttention = { session_id: "s1", generation: 1, context_key: "context-1", state: "ended", sequence: 3, ended_sequence: 3, seen_sequence: 0 };
function fixture() {
  let current = { id: "s1", generation: 1, conversation_attention: { ...fact } } as SessionSummary;
  let visible = true;
  const frames: Array<() => void> = [];
  const receipts: ConversationAttention[] = [];
  let changes = 0;
  const acks = new ConversationAcknowledgements({ current: () => current, visible: () => visible, schedule: callback => frames.push(callback), acknowledge: async (_id, served) => { receipts.push(served); }, changed: () => { changes++; } });
  return { acks, receipts, frames, get current() { return current; }, set current(value) { current = value; }, set visible(value: boolean) { visible = value; }, get changes() { return changes; } };
}

test("background, covered, switched and disposed conversations are not acknowledged", () => {
  const f = fixture();
  f.visible = false; f.acks.observe(); assert.equal(f.frames.length, 0);
  f.visible = true; f.acks.observe(); f.visible = false; f.frames.shift()!(); assert.equal(f.receipts.length, 0);
  f.visible = true; f.acks.observe(); f.current = { ...f.current, id: "s2" }; f.frames.shift()!(); assert.equal(f.receipts.length, 0);
  f.current = { ...f.current, id: "s1" }; f.acks.observe(); f.current = { ...f.current, generation: 2 }; f.frames.shift()!(); assert.equal(f.receipts.length, 0);
  f.current = { ...f.current, generation: 1 }; f.acks.observe(); f.acks.dispose(); f.frames.shift()!(); assert.equal(f.receipts.length, 0);
});

test("a queued acknowledgement names its served boundary and never clears pending input", async () => {
  const f = fixture();
  f.current.conversation_attention = { ...fact, state: "needs_input" };
  f.acks.observe(); f.acks.observe(); assert.equal(f.frames.length, 1, "one receipt in flight per boundary");
  f.current = { ...f.current, conversation_attention: { ...fact, state: "needs_input", sequence: 6, ended_sequence: 6 } };
  f.frames.shift()!();
  await new Promise(resolve => setImmediate(resolve));
  assert.equal(f.receipts[0]?.ended_sequence, 3, "a newer completion cannot be swept into the old receipt");
  assert.equal(f.current.conversation_attention?.state, "needs_input");
  assert.equal(f.current.conversation_attention?.seen_sequence, 0, "the browser does not invent durable acknowledgement state");
  assert.equal(f.changes, 1, "the server receipt requests a fresh snapshot");
});

test("native identity changes invalidate a scheduled receipt within one process generation", () => {
  const f = fixture(); f.acks.observe();
  f.current = { ...f.current, conversation_attention: { ...fact, context_key: "another-native-session" } };
  f.frames.shift()!(); assert.equal(f.receipts.length, 0);
});
