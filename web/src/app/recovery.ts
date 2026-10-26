// Session recovery in the machine register: the terminal pane
// mirrors four presentations at the end of the output
// stream — live keeps the CLI's pixels, restarting drops an exit line and
// swaps the status line for a spinner, replayed draws a centred rule plus
// the resumed lines, failed owns the only bordered card and hides the
// composer. The presentation derives ONLY from the server-presented record.
import type { SessionDetail } from "../api/sessions.ts";
import { escapeHtml } from "../lib/escape.ts";
import { PRIMARY_SM_BUTTON, SECONDARY_SM_BUTTON } from "../ui/controls.ts";
import * as hints from "../lib/hints.ts";

// resumeEnded is the server's resume_ended presentation: the provider was
// asked to continue the recorded conversation and did not. The card shows the
// exit and the identity that was asked for, and offers the two explicit ways
// forward; nothing was replaced on the user's behalf.
export type RecoveryState = "live" | "restarting" | "replayed" | "failed" | "unavailable" | "resumeEnded";

// The register and the stream share the pane's mono voice and x-height ceiling.
export const RECOVERY_MONO =
  "font-mono text-[min(var(--type-mono-size),calc(var(--type-body-sm-size)*var(--type-mono-xh-ratio)))] leading-[1.62]";

// RecoveryPresentation is the fact set the machine register renders. A null
// field means the server presented no value; the renderers emit only the
// facts they were given.
export interface RecoveryPresentation {
  state: RecoveryState;
  provider: string;
  exitLabel: string | null; // observed exit, e.g. "exit 137" or "exit SIGKILL"
  exitClock: string | null; // exit time as HH:MM:SS register text
  attempt: number | null; // current restart attempt; attemptBudget its cap
  attemptBudget: number | null;
  generation: number | null; // successor generation after a restart
  nativeID: string | null; // resumed session's native id
  resumeSub: string | null; // sub-line under the resumed line
  heldTail: string | null; // "recording held - input queued"
  failedBudget: string | null; // failed card machine lines
  failedPanic: string | null;
  failedCounts: string | null;
  recording: string | null; // failed register's size fact, e.g. "recording 214 KB"
  refusal: "provider" | "store_gone" | null; // resumeEnded: what stands in the way
}

// recoveryFromSession derives the register presentation from the server
// record. Replayed and failed derive from the seam fields
// (SessionDetail.replayed / SessionDetail.failure), RESTARTING from a
// "recovering" presentation; nothing is inferred from status or timers.
export function recoveryFromSession(session: SessionDetail): RecoveryPresentation | null {
  if (session.replayed !== undefined) {
    return {
      state: "replayed", provider: session.provider,
      exitLabel: exitEvidence(session), exitClock: clockText(session.exited_at),
      attempt: null, attemptBudget: null, generation: session.replayed.generation,
      nativeID: session.replayed.native_id, resumeSub: null, heldTail: null,
      failedBudget: null, failedPanic: null, failedCounts: null, recording: null, refusal: null,
    };
  }
  if (session.failure !== undefined) {
    const failure = session.failure;
    const recording = recordingText(failure.recording_bytes);
    return {
      state: "failed", provider: session.provider,
      exitLabel: exitEvidence(session), exitClock: clockText(session.exited_at),
      attempt: null, attemptBudget: failure.attempt_budget, generation: null,
      nativeID: null, resumeSub: null, heldTail: null,
      // The "5 exits in 90s" count needs facts the seam lacks.
      failedBudget: `limit ${failure.attempt_budget}`, failedPanic: failure.panic,
      failedCounts: `${recording} - ${failure.queued} queued - ${failure.resent} resent`, recording,
      refusal: null,
    };
  }
  // A session the server cannot attach is presented, not vanished:
  // the register says where it stands, and resume is the ordinary way back
  // once the manual path exists.
  if (session.presentation_state === "attachment_unavailable") {
    return {
      state: "unavailable", provider: session.provider,
      exitLabel: exitEvidence(session), exitClock: clockText(session.exited_at),
      attempt: null, attemptBudget: null, generation: null,
      nativeID: null, resumeSub: null, heldTail: null,
      failedBudget: null, failedPanic: null, failedCounts: null, recording: null, refusal: null,
    };
  }
  if (session.presentation_state === "resume_ended") {
    return {
      state: "resumeEnded", provider: session.provider,
      exitLabel: exitEvidence(session), exitClock: clockText(session.exited_at),
      attempt: null, attemptBudget: null, generation: null,
      nativeID: shortNativeID(session.native_session_id), resumeSub: null, heldTail: null,
      failedBudget: null, failedPanic: null, failedCounts: null, recording: null,
      refusal: session.recovery_refusal ?? null,
    };
  }
  if (session.presentation_state !== "recovering") return null;
  const attempts = session.resume_attempts;
  return {
    state: "restarting", provider: session.provider,
    exitLabel: exitEvidence(session), exitClock: clockText(session.exited_at),
    attempt: attempts !== undefined && attempts >= 0 ? attempts : null,
    attemptBudget: null, generation: null, nativeID: null, resumeSub: null,
    heldTail: null, failedBudget: null, failedPanic: null, failedCounts: null, recording: null,
    refusal: null,
  };
}

// referenceRecovery is the design's canonical machine values for each state
// (the reference fixture data), shared by the preview and the tests.
// Replayed and failed are FIXTURE-ONLY: the live mapper never serves them.
export function referenceRecovery(state: RecoveryState): RecoveryPresentation {
  // Failed shows the death that exhausted recovery (exit 1), not the crash
  // the restart was answering (exit 137).
  return {
    state, provider: "claude", exitClock: "08:12:04", attempt: 2, attemptBudget: 5,
    exitLabel: state === "failed" ? "exit 1" : "exit 137",
    generation: 2, nativeID: "46f913b4", resumeSub: "claude --resume - 0 queued lost",
    heldTail: "recording held - input queued", failedBudget: "5 exits in 90s - limit 5",
    failedPanic: "panic: runtime error in provider.go:214",
    failedCounts: "recording 214 KB - 2 queued - 0 resent", recording: "recording 214 KB",
    refusal: null,
  };
}

// recoveryStreamHtml is the end-of-stream fragment: the live output line, the
// restarting exit line, the replayed rule plus resumed lines, or the failed
// state's bordered card.
export function recoveryStreamHtml(p: RecoveryPresentation): string {
  switch (p.state) {
    case "live":
      return `<div class="mt-[8px] text-(--t-fg)">✻ Thinking…</div>`;
    case "restarting": {
      const exit = machineLine(p.exitLabel, p.exitClock);
      if (exit === "") return "";
      return `<div class="mt-[8px] text-(--t-faint)">${escapeHtml(exit)}</div>`;
    }
    case "replayed": {
      const rule = machineLine(p.exitClock, p.exitLabel, "restarted", genLabel(p.generation), "scrollback replayed");
      const resumed = p.nativeID === null
        ? ""
        : `
        <div class="mt-[8px]"><span class="text-(--t-accent)">●</span><span class="text-(--t-fg)">${escapeHtml(machineLine(`resumed ${p.nativeID}`, genLabel(p.generation), "context restored"))}</span></div>
        ${p.resumeSub === null ? "" : `<div class="text-(--t-faint)">  ⎿ ${escapeHtml(p.resumeSub)}</div>`}`;
      if (rule === "") return resumed;
      return `
        <div class="mt-[12px] mb-[2px] flex items-center gap-[10px] text-(--t-faint)">
          <span class="h-px flex-1 bg-(--t-rule)"></span><span>${escapeHtml(rule)}</span><span class="h-px flex-1 bg-(--t-rule)"></span>
        </div>${resumed}`;
    }
    case "resumeEnded": {
      // The exit and the identity are the server's facts in the machine
      // voice; the explanation and the choices are interface copy. The
      // provider's own words about why sit in the retained screen the app
      // mounts above this card (data-recovery-screen).
      const head = machineLine(p.provider, p.exitLabel, "resume ended");
      const asked = p.nativeID === null ? "" : machineLine(`resumed ${p.nativeID}`, p.exitClock);
      const why = p.refusal === "store_gone"
        ? "The store this conversation was recorded in is gone, so nothing was launched. The session record is kept."
        : "The provider did not continue the recorded conversation. The session record and its output are kept.";
      return `
        <div class="mx-auto mt-[18px] mb-[6px] flex max-w-[560px] flex-col gap-[3px] rounded-[6px] border border-(--t-rule) pt-[14px] pr-[16px] pb-[14px] pl-[16px]">
          <span class="text-(--t-warn)">${escapeHtml(head)}</span>
          ${asked === "" ? "" : `<span class="text-(--t-faint)">${escapeHtml(asked)}</span>`}
          <span class="mt-[6px] font-ui text-body-sm text-(--t-dim)">${escapeHtml(why)}</span>
          <span class="font-ui text-body-sm text-(--t-faint)">Resume asks the provider again. Start fresh session opens a new conversation beside this one.</span>
          <div class="mt-[9px] flex items-center gap-[8px] font-ui">
            <button type="button" data-recovery-retry class="${PRIMARY_SM_BUTTON}" data-hint="${hints.RESUME_THIS_SESSION}">Resume</button>
            <button type="button" data-recovery-fresh class="${SECONDARY_SM_BUTTON}" data-hint="${hints.START_FRESH_SESSION}">Start fresh session</button>
          </div>
          <span data-recovery-error hidden class="mt-[6px] text-error"></span>
        </div>`;
    }
    case "unavailable": {
      const head = machineLine(p.provider, "deck attachment unavailable");
      const exit = machineLine(p.exitLabel, p.exitClock);
      return `
        <div class="mx-auto mt-[18px] mb-[6px] flex max-w-[560px] flex-col gap-[3px] rounded-[6px] border border-(--t-rule) pt-[14px] pr-[16px] pb-[14px] pl-[16px]">
          <span class="text-(--t-warn)">${escapeHtml(head)}</span>
          ${exit === "" ? "" : `<span class="text-(--t-faint)">${escapeHtml(exit)}</span>`}
          <span class="text-(--t-faint)">the session record is kept; resume asks the provider to continue it</span>
          <div class="mt-[9px] flex items-center gap-[8px]">
            <button type="button" data-recovery-retry class="${PRIMARY_SM_BUTTON}" data-hint="${hints.RESUME_THIS_SESSION}">Resume</button>
            <button type="button" data-recovery-fresh class="${SECONDARY_SM_BUTTON}" data-hint="${hints.START_FRESH_SESSION}">Start fresh session</button>
          </div>
          <span data-recovery-error hidden class="mt-[6px] text-error"></span>
        </div>`;
    }
    case "failed": {
      const head = machineLine(p.provider, p.exitLabel, "restart stopped");
      return `
        <div class="mx-auto mt-[18px] mb-[6px] flex max-w-[560px] flex-col gap-[3px] rounded-[6px] border border-error pt-[14px] pr-[16px] pb-[14px] pl-[16px]">
          <span class="text-error">${escapeHtml(head)}</span>
          ${p.failedBudget === null ? "" : `<span class="text-(--t-dim)">${escapeHtml(p.failedBudget)}</span>`}
          ${p.failedPanic === null ? "" : `<span class="text-(--t-dim)">${escapeHtml(p.failedPanic)}</span>`}
          ${p.failedCounts === null ? "" : `<span class="text-(--t-faint)">${escapeHtml(p.failedCounts)}</span>`}
          <div class="mt-[9px] flex items-center gap-[8px]">
            <button type="button" data-recovery-fresh class="${PRIMARY_SM_BUTTON}" data-hint="${hints.START_FRESH_SESSION}">Start fresh session</button>
            <button type="button" data-recovery-retry class="${SECONDARY_SM_BUTTON}" data-hint="${hints.RETRY_THIS_SESSION}">Retry</button>
          </div>
          <span data-recovery-error hidden class="mt-[6px] text-error"></span>
        </div>`;
    }
  }
}

// recoveryComposerHtml is the register's composer line; the states with no
// process behind them have none.
export function recoveryComposerHtml(p: RecoveryPresentation): string {
  if (p.state === "failed" || p.state === "resumeEnded") return "";
  return `
  <div class="flex flex-none items-start gap-[9px] border-t border-b border-(--t-rule) px-[20px] pt-[7px] pb-[7px] ${RECOVERY_MONO} text-(--t-fg)">
    <span class="flex-none text-(--t-dim)">›</span>
    <div class="min-w-0 flex-1"><span class="inline-block h-[14px] w-[7px] bg-(--t-fg) align-[-3px]"></span></div>
  </div>`;
}

// recoveryRegisterHtml is the register's status line: accept-edits for live
// and replayed, the spinner while restarting, and no-process for failed. The
// restarting row renders only the attempt number it was given — the budget
// and held-recording facts are facts the seam does not carry.
// recoveryBannerHtml is the phone's reconnecting band: a strip inside the terminal pane, above the
// transcript, that reserves its own height rather than floating over it. The desktop carries the same
// facts in the register below the stream; a phone has no room for a register,
// so the band takes the top edge instead. It lives here, beside the register,
// because the live app renders it — a banner written into the preview fixture
// would depict a screen the app does not have.
export function recoveryBannerHtml(p: RecoveryPresentation): string {
  if (p.state !== "restarting") return "";
  const attempt = p.attempt === null ? "" : `attempt ${p.attempt}`;
  return `
    <div data-recovery-banner
      class="flex h-8 flex-none items-center gap-[9px] border-b border-(--t-rule) bg-(--t-input-bg) px-4 font-mono text-[min(var(--type-mono-size),calc(var(--type-micro-size)*var(--type-mono-xh-ratio)))]">
      <span aria-hidden="true" class="h-[10px] w-[10px] flex-none rounded-circle border-[1.5px] border-(--t-panel-border) border-t-(--t-accent) animate-spinner"></span>
      <span class="text-(--t-fg)">reconnecting</span>
      ${attempt === "" ? "" : `<span class="text-(--t-faint)">·</span><span class="text-(--t-faint)">${escapeHtml(attempt)}</span>`}
      ${p.exitClock === null ? "" : `<span class="ml-auto text-(--t-faint)">held ${escapeHtml(p.exitClock)}</span>`}
    </div>`;
}

export function recoveryRegisterHtml(p: RecoveryPresentation): string {
  if (p.state === "failed") {
    return registerRow(`<span>no process</span>${p.recording === null ? "" : `<span>-</span><span>${escapeHtml(p.recording)}</span>`}`);
  }
  if (p.state === "unavailable") {
    return registerRow(`<span>no attachment</span>`);
  }
  if (p.state === "resumeEnded") {
    return registerRow(`<span>no process</span>`);
  }
  if (p.state === "restarting") {
    const attempt = p.attempt === null ? null : `attempt ${p.attempt}${p.attemptBudget === null ? "" : `/${p.attemptBudget}`}`;
    return registerRow(`
      <span aria-hidden="true" class="h-[11px] w-[11px] flex-none rounded-circle border-[1.5px] border-(--t-rule) border-t-(--t-warn) animate-spinner"></span>
      <span>${escapeHtml(machineLine(`restarting ${p.provider}`, attempt))}</span>
      ${p.heldTail === null ? "" : `<span class="text-(--t-faint)">- ${escapeHtml(p.heldTail)}</span>`}
    `, "warn");
  }
  return registerRow(`
    <span aria-hidden="true" class="text-(--t-accent)">▶▶</span>
    <span class="text-(--t-accent)">accept edits on</span>
    <span>(shift+tab to cycle)</span>
    <span>·</span>
    <span>← 1 agent</span>
  `);
}

// registerRow is the register's one-line row; "warn" adds the restarting
// spinner's amber tone and wider gap.
function registerRow(inner: string, tone: "faint" | "warn" = "faint"): string {
  // Literal classes per branch: a computed gap-[...] is invisible to the
  // Tailwind scanner and silently drops out of the build.
  const cls = tone === "warn"
    ? "gap-[9px] text-(--t-warn)"
    : "gap-[7px] text-(--t-faint)";
  return `<div data-recovery-register class="flex flex-none items-center px-(--space-5) pt-[6px] pb-[9px] ${RECOVERY_MONO} ${cls}">${inner}</div>`;
}

function machineLine(...parts: (string | number | null)[]): string {
  return parts.filter((part) => part !== null && part !== "").join(" - ");
}

function genLabel(generation: number | null): string | null {
  return generation === null ? null : `gen ${generation}`;
}

// recordingText is the failed register's size fact (e.g. "recording 214 KB").
function recordingText(bytes: number): string {
  const kb = Math.round(bytes / 1024);
  return `recording ${kb} KB`;
}

// shortNativeID is the identity's first eight characters, the register's
// form for a provider id (the replayed rule shows the same).
function shortNativeID(id: string | undefined): string | null {
  if (id === undefined || id === "") return null;
  return id.replaceAll("-", "").slice(0, 8);
}

function exitEvidence(session: Pick<SessionDetail, "exit_code" | "exit_signal">): string | null {
  if (session.exit_code !== undefined && session.exit_code !== null) return `exit ${session.exit_code}`;
  if (session.exit_signal) return `exit ${session.exit_signal}`;
  return null;
}

function clockText(iso: string | undefined): string | null {
  if (iso === undefined || iso === "") return null;
  const t = new Date(iso);
  if (Number.isNaN(t.getTime())) return null;
  return [t.getHours(), t.getMinutes(), t.getSeconds()]
    .map((n) => String(n).padStart(2, "0"))
    .join(":");
}
