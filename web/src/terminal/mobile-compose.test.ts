import assert from "node:assert/strict";
import test from "node:test";

import { MobileTerminalCompose, isComposeSubmitIntent } from "./mobile-compose.ts";

type Outcome = { code: string; message: string };

function composeRecorder() {
  const sent: Array<["paste" | "input", string]> = [];
  const pending: Array<(code: string, message: string) => void> = [];
  const compose = new MobileTerminalCompose({
    paste: (text) => sent.push(["paste", text]),
    sendInput: (data) => sent.push(["input", data]),
    trackWrite: (send, done) => {
      send();
      pending.push(done);
      return true;
    },
  });
  return { compose, sent, pending };
}

test("a flush tracks the complete draft before a separate submit key and finishes after both", () => {
  const { compose, sent, pending } = composeRecorder();
  const outcomes: Outcome[] = [];
  const draft = "go test ./...\n第二行 café\n";
  assert.equal(compose.flush(draft, (code, message) => outcomes.push({ code, message })), true);
  assert.deepEqual(sent, [["paste", draft], ["input", "\r"]]);
  assert.deepEqual(outcomes, []);
  pending[0]?.("sent", "");
  assert.deepEqual(outcomes, [], "paste success must not stand in for Return success");
  pending[1]?.("sent", "");
  assert.deepEqual(outcomes, [{ code: "sent", message: "" }]);
});

test("an empty flush tracks only the bare Return", () => {
  const { compose, sent, pending } = composeRecorder();
  const outcomes: Outcome[] = [];
  assert.equal(compose.flush("", (code, message) => outcomes.push({ code, message })), true);
  assert.deepEqual(sent, [["input", "\r"]]);
  pending[0]?.("sent", "");
  assert.deepEqual(outcomes, [{ code: "sent", message: "" }]);
});

test("every write must be sent and the aggregate callback runs once", () => {
  const { compose, pending } = composeRecorder();
  const outcomes: Outcome[] = [];
  compose.flush("draft", (code, message) => outcomes.push({ code, message }));
  pending[0]?.("sent", "");
  pending[1]?.("uncertain", "connection lost");
  pending[1]?.("sent", "");
  assert.deepEqual(outcomes, [{ code: "uncertain", message: "connection lost" }]);
});

test("an earlier non-sent outcome is retained when Return later fails", () => {
  const { compose, pending } = composeRecorder();
  const outcomes: Outcome[] = [];
  compose.flush("draft", (code, message) => outcomes.push({ code, message }));
  pending[0]?.("failed", "paste failed");
  pending[1]?.("interrupted", "attachment closed");
  assert.deepEqual(outcomes, [{ code: "failed", message: "paste failed" }]);
});

test("overlapping flushes are rejected without writing or completing", () => {
  const { compose, sent, pending } = composeRecorder();
  const outcomes: Outcome[] = [];
  assert.equal(compose.flush("first", (code, message) => outcomes.push({ code, message })), true);
  assert.equal(compose.flush("second", (code, message) => outcomes.push({ code, message })), false);
  assert.deepEqual(sent, [["paste", "first"], ["input", "\r"]]);
  pending[0]?.("sent", "");
  pending[1]?.("sent", "");
  assert.deepEqual(outcomes, [{ code: "sent", message: "" }]);
});

test("a disposed row sends nothing and suppresses pending callbacks", () => {
  const { compose, sent, pending } = composeRecorder();
  const outcomes: Outcome[] = [];
  compose.flush("draft", (code, message) => outcomes.push({ code, message }));
  compose.dispose();
  pending[0]?.("sent", "");
  pending[1]?.("sent", "");
  assert.deepEqual(outcomes, []);
  const lateOutcomes: Outcome[] = [];
  assert.equal(compose.flush("late", (code, message) => lateOutcomes.push({ code, message })), false);
  assert.deepEqual(lateOutcomes, []);
  assert.deepEqual(sent, [["paste", "draft"], ["input", "\r"]]);
});

test("late callbacks from a completed flush cannot complete a later flush", () => {
  const { compose, pending } = composeRecorder();
  const outcomes: string[] = [];
  compose.flush("first", (code) => outcomes.push(`first:${code}`));
  pending[0]?.("sent", "");
  pending[1]?.("sent", "");
  compose.flush("second", (code) => outcomes.push(`second:${code}`));
  pending[0]?.("failed", "stale");
  pending[1]?.("failed", "stale");
  assert.deepEqual(outcomes, ["first:sent"]);
  pending[2]?.("sent", "");
  pending[3]?.("sent", "");
  assert.deepEqual(outcomes, ["first:sent", "second:sent"]);
});

test("a write that cannot start returns false without a callback", () => {
  const outcomes: Outcome[] = [];
  const compose = new MobileTerminalCompose({
    paste: () => assert.fail("a rejected paste must not run"),
    sendInput: () => {},
    trackWrite: () => false,
  });
  assert.equal(compose.flush("draft", (code, message) => outcomes.push({ code, message })), false);
  assert.deepEqual(outcomes, []);
});

test("a rejected Return preserves the paste outcome and completes as failed", () => {
  const outcomes: Outcome[] = [];
  let calls = 0;
  const pending: Array<(code: string, message: string) => void> = [];
  const compose = new MobileTerminalCompose({
    paste: () => {},
    sendInput: () => assert.fail("a rejected Return must not run"),
    trackWrite: (send, done) => {
      calls++;
      if (calls === 2) return false;
      send();
      pending.push(done);
      return true;
    },
  });
  assert.equal(compose.flush("draft", (code, message) => outcomes.push({ code, message })), true);
  assert.deepEqual(outcomes, []);
  pending[0]?.("sent", "");
  assert.deepEqual(outcomes, [{ code: "failed", message: "The submit key was not sent." }]);
});

// Only the soft keyboard's Return flushes; every editing intent — typing,
// dictation's compositions and insertions, in-draft deletions, paste —
// belongs to the field and must be left alone, or voice apps break.
test("only the Return intents flush the buffer", () => {
  assert.equal(isComposeSubmitIntent("insertLineBreak"), true);
  assert.equal(isComposeSubmitIntent("insertParagraph"), true);
  for (const editing of [
    "insertText",
    "insertCompositionText",
    "insertFromPaste",
    "insertReplacementText",
    "deleteContentBackward",
    "deleteContentForward",
    "deleteWordBackward",
  ]) {
    assert.equal(isComposeSubmitIntent(editing), false, `${editing} must edit the field`);
  }
});
