import type { ConversationAttention } from "../terminal/conversation-attention.ts";
export interface SessionSummary {
  id: string;
  project_id: string;
  provider: string;
  conversation_attention?: ConversationAttention;
  codex_login_helper?: boolean;
  // display_name is the user's own label; empty renders the
  // provider. Metadata, never identity.
  display_name: string;
  status: string;
  generation: number;
  created_at: string;
  // updated_at is the reconciled record's last-write time. The server
  // stamps it on state and input transitions; it is NOT output freshness
  // and must not be rendered as a last-output fact.
  updated_at: string;
  // last_output_at is the session's last pty-output time. It remains optional
  // API metadata; reconciliation chrome does not reinterpret it as state.
  last_output_at?: string;
  color?: string | null;
  // These content-free backend facts describe the durable automatic successor
  // path. The browser never derives them from provider, status, native
  // identity, or terminal output.
  recovery_capability: "resume" | "relaunch" | "none";
  recovery_state: "unavailable" | "automatic_pending" | "recovering";
  // resume_ended: the last generation resumed the recorded conversation and
  // ended on its own with a failure; the record is kept for the user's
  // explicit choice between resuming again and a fresh session.
  presentation_state: "running" | "recovering" | "attachment_unavailable" | "ended" | "inactive" | "resume_ended";
}

// SessionDetail is the reconciled session record as served: the status
// is reconciliation-derived, never inferred client-side. Only
// fields used by the active terminal surface are typed.
export interface SessionDetail {
  id: string;
  project_id: string;
  provider: string;
  display_name: string;
  color?: string | null;
  status: string;
  generation: number;
  tmux_pane_id?: string;
  pane_pid?: number;
  exit_code?: number;
  exit_signal?: string;
  exited_at?: string;
  native_session_id?: string;
  // resume_attempts is the consumed automatic-recovery opportunity:
  // zero is open, one is durably spent. Server-presented, like the recovery
  // facts below; never inferred.
  resume_attempts?: number;
  // Seam for the replayed and failed register presentations: a
  // presentation derives from them ONLY when the server presents them
  // (the authority invariant); absent today.
  replayed?: { generation: number; native_id: string };
  failure?: {
    attempt_budget: number;
    recording_bytes: number;
    queued: number;
    resent: number;
    panic: string;
  };
  created_at: string;
  updated_at: string;
  recovery_capability: SessionSummary["recovery_capability"];
  recovery_state: SessionSummary["recovery_state"];
  presentation_state: SessionSummary["presentation_state"];
  // recovery_refusal accompanies resume_ended: "provider" when the resumed
  // process ended without continuing the conversation, "store_gone" when
  // the store it was recorded in no longer exists so nothing was launched.
  recovery_refusal?: "provider" | "store_gone";
}

// RecoveryOutcome names what a user recover actually did: "resumed"
// continued the recorded conversation; "relaunched" started a fresh one in a
// session record that had no conversation to resume — no earlier thread
// came back, and the UI must never imply one did.
export type RecoveryOutcome = "resumed" | "relaunched";

// RecoverResult is the recover response: the revived session plus the
// outcome field.
export interface RecoverResult extends SessionDetail {
  outcome: RecoveryOutcome;
}

// ConnectionState is the browser's own attachment state for the session
// — the one state the browser DOES own (its WebSocket), supplied by the
// terminal attachment.
export type ConnectionState = "connected" | "connecting" | "disconnected";

// One harness's recorded launch history, as GET /api/harness-usage reports
// it: how many sessions were ever started on it and when the last one was.
// Removed rows are excluded server-side; ended and interrupted ones count,
// because they were launches the user made.
export interface HarnessUsage {
  provider: string;
  launches: number;
  last_used_at: string;
}

// Both scopes in one answer: the whole installation, and the project the
// picker was opened in. The project scope is empty when no project was
// named or when nothing has been launched there.
export interface HarnessUsageReport {
  global: readonly HarnessUsage[];
  project: readonly HarnessUsage[];
}
