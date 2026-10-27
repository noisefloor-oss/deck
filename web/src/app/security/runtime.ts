import { escapeHtml as esc } from "../../lib/escape.ts";
import { lucideIcon as icon } from "../../ui/icons.ts";
import { googleMark } from "../login.ts";
import type { MethodKind } from "../../security/envelope.ts";
import type { SecurityState } from "../../security/client.ts";

export type RuntimeScreen =
  | { kind: "plain-stopped" | "plain-starting" | "plain-unknown" }
  | { kind: "signed-out" }
  | { kind: "locked"; cancelled?: boolean; confirmed?: boolean; restarted?: { at: string; stopped: number } }
  | { kind: "authorize-browser" }
  | { kind: "resume"; initializing?: boolean }
  | { kind: "password"; context: "unlock" | "authorize-browser" | "resume" | "resume-initialization" | "initialize" | "lock"; wrong?: boolean }
  | { kind: "recovery"; context: "unlock" | "authorize-browser" | "resume" | "lock" }
  | { kind: "incompatible" | "unavailable" | "unlock-failed" | "help" | "lost-methods" }
  | { kind: "progress"; operation: "unlock" | "initialize" | "lock"; storage: SecurityState["storage"]; runtime: SecurityState["runtime"] }
  | { kind: "unknown"; operation: "unlock" | "initialize" | "lock" }
  | { kind: "lock-confirm" | "lock-failed" };
export interface RuntimeView {
  name: string;
  address: string;
  screen: RuntimeScreen;
  methods: MethodKind[];
  methodChoices?: { id: string; kind: MethodKind }[];
  selectedMethod?: string;
  busy?: boolean;
  error?: string;
  notice?: string;
  email?: string;
  /** Render only observations supplied by an authoritative source. */
  lastReached?: string;
  lockReceipt?: { at: string; stopped: number };
  sessions?: { name: string; command: string; age: string; state: "running" | "stopped" | "stopping" | "unconfirmed" }[];
}
export type RuntimeAction =
  | { kind: "google-sign-in" | "passkey" | "password" | "recovery" | "copy-address" | "retry" | "sign-out" | "cancel" | "confirm-lock" | "help" | "lost-methods" | "start-workspace" }
  | { kind: "submit-password" | "submit-recovery" | "select-method"; value: string }
  | { kind: "import-kit"; file: File };

const button = (action: string, label: string, style = "primary", extra = ""): string => `<button type="button" class="enroll-button enroll-button-${style} ${extra}" data-security-action="${action}">${label}</button>`;
const link = (action: string, label: string): string => `<button type="button" class="security-text-link" data-security-action="${action}">${label}</button>`;
const title = (value: string): string => `<h1 class="enroll-title" tabindex="-1">${value}</h1>`;
const copy = (value: string): string => `<p class="enroll-copy">${value}</p>`;
const caption = (value: string): string => `<p class="enroll-caption">${value}</p>`;
const logo = (): string => '<img class="enroll-logo" src="/brand/noisefloor-lockup-inverse.svg" alt="Noisefloor">';
const brand = (right = icon("lock", 16), tone = "tertiary"): string => `<div class="enroll-brand-row">${logo()}<span class="enroll-${tone} security-brand-status">${right}</span></div>`;
const status = (value: string, tone = "warning"): string => `<div class="enroll-status enroll-${tone}">${icon("triangle-alert", 15)}<span>${value}</span></div>`;
const intro = (heading: string, body: string): string => `<div class="enroll-intro">${title(heading)}${copy(body)}</div>`;
const spinner = (): string => '<span class="enroll-spinner" aria-hidden="true"></span>';
function methods(view: RuntimeView, browser = false): string {
  return `${view.methods.includes("passkey") ? button("passkey", browser ? "Verify with passkey" : "Unlock with passkey") : ""}
    ${view.methods.includes("password") ? button("password", "Use encryption password", "secondary") : ""}
    ${view.methods.includes("recovery") ? button("recovery", "Use recovery key", "ghost") : ""}`;
}
function sessionRows(view: RuntimeView): string {
  if (!view.sessions?.length) return "";
  return `<div class="enroll-card enroll-flush security-session-list">${view.sessions.map(s => `<div class="security-session-row"><i class="security-running-dot"></i><span>${esc(s.name)}</span><span class="security-session-detail"><span class="enroll-mono">${esc(s.command)}</span> · ${esc(s.age)}</span></div>`).join("")}</div>`;
}
function progressRows(view: RuntimeView, locking: boolean): string {
  const s = view.screen;
  if (s.kind !== "progress") return "";
  const row = (symbol: string, text: string, extra = "", suffix = ""): string => `<div class="security-progress-row ${extra}">${symbol}<span>${text}</span>${suffix}</div>`;
  const storageDone = s.storage === (locking ? "closed" : "open");
  const storage = row(storageDone ? `<span class="enroll-success">${icon("circle-check", 14)}</span>` : locking && s.runtime !== "stopped" ? '<i class="security-empty-circle"></i>' : spinner(), storageDone ? locking ? "Storage closed" : "Unlocked" : locking ? "Locking storage" : "Opening storage", !storageDone && locking && s.runtime !== "stopped" ? "enroll-waiting" : "");
  const runtime = view.sessions?.length && locking ? view.sessions.map((session, i) => row(session.state === "stopped" ? `<span class="enroll-success">${icon("circle-check", 14)}</span>` : spinner(), session.state === "stopped" ? `${esc(session.name)} stopped` : `Stopping ${esc(session.name)}`, session.state === "stopped" ? "" : "security-current", session.state === "stopping" ? `<small>${i + 1} of ${view.sessions!.length}</small>` : "")).join("") : row(s.runtime === (locking ? "stopped" : "running") ? `<span class="enroll-success">${icon("circle-check", 14)}</span>` : spinner(), s.runtime === "running" ? "Workspace running" : s.runtime === "stopped" ? "Workspace stopped" : s.runtime === "starting" ? "Starting the workspace" : s.runtime === "stopping" ? "Stopping the workspace" : "Checking the workspace", "security-current");
  return `<div class="security-progress">${locking ? runtime + storage : storage + runtime}</div>`;
}

export function runtimeHtml(view: RuntimeView): string {
  const name = esc(view.name), s = view.screen;
  let content = "", modifier = "", backdrop = false;
  switch (s.kind) {
    case "plain-stopped": content = `${logo()}${intro(`Start ${name}`, "Workspace setup is complete. Start it to continue working.")}${button("start-workspace", "Start workspace")}${caption("Deck encryption is off for this workspace.")}${link("sign-out", "Sign out")}`; break;
    case "plain-starting": content = `${logo()}${title(`Starting ${name}`)}<div class="security-progress">${spinner()}${copy("Waiting for the workspace to confirm it is running.")}</div>${caption("Reconnecting checks progress without repeating the request.")}`; break;
    case "plain-unknown": content = `${status("Not known")}${intro("Could not confirm the workspace state", `The current state of ${name} is not known. Check again before starting another request.`)}${button("retry", "Reconnect and check")}${caption("Checking does not restart the workspace or change its protection.")}`; break;
    case "signed-out": content = `${logo()}${button("google-sign-in", `${googleMark()}Sign in with Google`, "secondary")}${caption(`Signing in gets you to ${name}. Unlocking it is a separate step.`)}`; break;
    case "locked": {
      if (s.confirmed) {
        modifier = "security-card-400";
        content = `${brand(icon("lock", 16), "success")}${intro(`${name} is locked`, view.lockReceipt ? `Storage closed at ${esc(view.lockReceipt.at)}. ${view.lockReceipt.stopped === 2 ? "Two" : view.lockReceipt.stopped} sessions stopped.` : "Storage is closed and the workspace has stopped.")}${button("retry", "Unlock again", "secondary")}${caption(`You are still signed in${view.email ? ` as ${esc(view.email)}` : ""}.`)}`;
      } else if (s.restarted) {
        modifier = "security-card-400 security-restarted"; backdrop = true;
        content = `${brand()}${intro(`${name} restarted and is locked`, `The machine came back up at ${esc(s.restarted.at)}. Your ${s.restarted.stopped === 2 ? "two" : s.restarted.stopped} sessions stopped when it went down; unlock to start work again.`)}${methods(view)}${caption("Every restart needs an unlock — including ones whoever runs the machine performs.")}`;
      } else content = `${brand()}${intro(`${name} is locked`, s.cancelled ? "Unlock cancelled. Nothing changed." : "Nothing is running. Unlock to start your sessions again.")}${methods(view)}${!s.cancelled ? `<div class="security-identity">${view.email ? `<i></i><span>${esc(view.email)}</span>` : ""}${link("sign-out", "Sign out")}</div>` : ""}`;
      break;
    }
    case "authorize-browser": modifier = "security-card-420"; content = `${brand('<i class="security-running-dot"></i><span>Running</span>', "success")}${intro("Your workspace is running", "Verify an unlock method to connect this browser. Your sessions keep running while you do — nothing stops and nothing restarts.")}<div class="enroll-card enroll-muted security-private-note">${icon("info", 14)}${copy("What is running, and what it is working on, stays hidden until this browser is authorized.")}</div>${methods(view, true)}${caption("A browser with current, valid authorization picks up where it was, without this step.")}`; break;
    case "resume": content = `${brand()}${intro(`Finish setting up ${name}`, s.initializing ? "Verify the saved unlock method to check the interrupted setup." : "An unlock method is saved. Verify it to continue setup and save a recovery key.")}${view.methods.includes("passkey") ? button("passkey", "Verify saved passkey") : ""}${view.methods.includes("password") ? button("password", "Use encryption password", "secondary") : ""}${button("sign-out", "Not now", "ghost")}`; break;
    case "password": {
      modifier = "security-card-400";
      const settingUp = ["resume", "initialize", "resume-initialization"].includes(s.context);
      const label = s.context === "authorize-browser" ? "Verify this browser" : s.context === "resume" ? "Continue setup" : settingUp ? "Finish setup" : s.context === "lock" ? "Confirm lock" : "Unlock";
      content = `${brand()}${title(s.context === "authorize-browser" ? "Connect this browser" : settingUp ? `Finish setting up ${name}` : s.context === "lock" ? `Lock ${name}` : `Unlock ${name}`)}<form data-security-form="password" class="enroll-form"><label class="enroll-field"><span id="security-password-label">Encryption password</span><input aria-labelledby="security-password-label" name="password" type="password" autocomplete="current-password" class="enroll-input${s.wrong ? " enroll-input-error" : ""}" required maxlength="1024"${s.wrong ? ' aria-invalid="true"' : ""}>${s.wrong ? `<span class="enroll-caption enroll-error" role="alert">That password did not unlock ${name}.</span>${caption("Deck cannot reset it or send it to you. If it is gone, your recovery key is the way back.")}` : ""}</label><button type="submit" class="enroll-button enroll-button-primary">${s.wrong ? "Try again" : label}</button></form>${s.wrong && !settingUp ? button("recovery", "Use recovery key", "secondary") : view.methods.includes("passkey") ? button("passkey", "Unlock with passkey instead", "ghost") : button("cancel", "Back", "ghost")}`;
      break;
    }
    case "recovery": {
      modifier = "security-plain security-recovery";
      const verifying = s.context === "authorize-browser";
      content = `${logo()}${intro(`Get back into ${name}`, verifying ? "Verify your key to connect this browser. Your sessions keep running." : "Use your recovery key to get back in, then set up a new unlock method. The key keeps working until you replace it and check the new recovery key.")}<form data-security-form="recovery" class="enroll-form"><label class="enroll-field"><span>Recovery key</span><textarea name="recovery" class="enroll-input enroll-recovery-input" rows="3" spellcheck="false" autocapitalize="off" autocomplete="off" maxlength="512" required placeholder="DK1-…"></textarea>${caption("Spacing, dashes and capitals do not matter.")}</label><div class="enroll-actions"><button type="submit" class="enroll-button enroll-button-primary">${verifying ? "Verify this browser" : s.context === "lock" ? "Confirm lock" : "Unlock with this key"}</button>${button("import-kit", `${icon("upload", 14)}Import kit file`, "secondary")}</div><input type="file" data-security-kit-file accept="application/json,.json" hidden></form><div class="security-recovery-desktop">${view.methods.includes("passkey") ? caption(`Still have a working passkey? ${link("passkey", "Unlock with that instead")} — it leaves your recovery key unused.`) : caption(`${link("cancel", "Back to unlock methods")}.`)}${caption(link("help", "Need help getting back in?"))}</div><div class="security-recovery-mobile">${caption(`Cannot reach ${name} at all? That is a different problem — ${link("help", "whoever runs the machine can help with it")}.`)}</div>`;
      break;
    }
    case "incompatible": modifier = "security-card-420"; content = `${status("Cannot unlock here", "error")}${title(`This browser can sign you in, but cannot unlock ${name}`)}${copy("This passkey did not provide the encryption result this browser needs. Try another browser or another unlock method.")}<div class="enroll-card enroll-muted security-browser-note">${copy(`Open <span class="enroll-mono">${esc(view.address)}</span> in another browser on this machine, or unlock from your phone.`)}${caption("If you also set an encryption password, you can use that in a supported browser.")}</div><div class="enroll-actions">${button("copy-address", "Copy the address", "secondary", "enroll-button-small")}${view.methods.includes("password") ? button("password", "Use a password instead", "ghost", "enroll-button-small") : button("recovery", "Use recovery key", "ghost", "enroll-button-small")}</div>`; break;
    case "unavailable": modifier = "security-card-420"; content = `${status("No answer")}${title(`Cannot reach ${name}`)}${copy(`The workspace did not answer at <span class="enroll-mono">${esc(view.address)}</span>. Your unlock methods have not been changed. Check the connection and try again.`)}${view.lastReached ? `<div class="security-last-reached"><span>Last reached</span><span>${esc(view.lastReached)}</span></div>` : ""}<div class="enroll-actions">${button("retry", `${icon("refresh-cw", 14)}Try again`, "primary", "enroll-button-small")}${button("help", "Get help reaching it", "ghost", "enroll-button-small")}</div>`; break;
    case "unlock-failed": modifier = "security-card-420"; content = `<div class="security-mobile-brand">${logo()}</div>${status("Still locked", "error")}${title(`That did not unlock ${name}`)}${copy("The workspace did not confirm an unlock. Check the workspace address and try another saved unlock method.")}<div class="security-failure-actions">${view.methods.includes("passkey") ? button("passkey", "Try another passkey") : button("password", "Use encryption password")}${button("recovery", "Use recovery key", "secondary")}</div>`; break;
    case "progress": {
      const locking = s.operation === "lock"; backdrop = locking; modifier = locking ? "security-lock-progress" : "";
      content = `${locking ? `<div class="security-progress-heading">${spinner()}${title(`Locking ${name}`)}</div>` : `${logo()}${title(s.operation === "initialize" ? `Preparing ${name}` : `Unlocking ${name}`)}`}${progressRows(view, locking)}${caption(locking ? "Do not close this tab until it finishes." : "Sessions do not come back by themselves — you will reopen the ones you want.")}`; break;
    }
    case "unknown": modifier = "security-card-440"; content = `${status("Not known")}${title(s.operation === "lock" ? "Could not confirm whether your workspace locked" : "Could not confirm the workspace state")}${copy(`The connection to ${name} dropped while ${s.operation === "lock" ? "it was locking" : "the request was being handled"}. It may have finished, and it may not. Reconnect to check.`)}<div class="enroll-actions">${button("retry", `${icon("refresh-cw", 14)}Reconnect and check`, "primary", "enroll-button-small")}</div>${caption("Checking the result does not repeat the request.")}`; break;
    case "lock-confirm": {
      backdrop = true; modifier = "security-lock-dialog";
      const count = view.sessions?.length, countText = count === 2 ? "Two sessions" : count ? `${count} sessions` : "Your sessions";
      content = `<div class="security-sheet-handle"></div><header class="security-dialog-head"><div>${title(`Lock ${name}?`)}${copy(`${countText} are running right now`)}</div></header><div class="security-dialog-body"><div class="security-lock-desktop">${copy("Locking stops them now and closes the storage. Anything they have not written to disk is lost, and nothing restarts by itself when you unlock.")}</div><div class="security-lock-mobile">${title(`Lock ${name}?`)}${copy(`${countText} are running. Locking stops them now and closes the storage; unsaved work is lost.`)}</div>${sessionRows(view)}<div class="security-lock-desktop">${caption(`To step away without stopping anything, sign out instead — your sessions keep running and ${name} stays unlocked.`)}</div><div class="security-lock-mobile">${caption("Signing out instead leaves them running.")}</div></div><footer class="security-dialog-foot">${button("cancel", "Keep working", "ghost")}${button("confirm-lock", "Stop sessions and lock")}</footer>`;
      break;
    }
    case "lock-failed": modifier = "security-card-460"; content = `${status("Still unlocked", "error")}${title(`${name} did not lock`)}${copy("Storage is still open. Your work may still be running and readable on the machine.")}${view.sessions?.length ? `<div class="enroll-card enroll-flush">${view.sessions.map(session => `<div class="security-session-row"><span class="enroll-${session.state === "stopped" ? "success" : "error"}">${icon(session.state === "stopped" ? "circle-check" : "x", 14)}</span><span>${esc(session.name)} ${session.state === "stopped" ? "stopped" : "did not stop"}</span></div>`).join("")}</div>` : ""}<div class="enroll-actions">${button("retry", "Check workspace state", "primary", "enroll-button-small")}</div>${caption("Checking the result does not repeat the lock request.")}`; break;
    case "help": modifier = "security-plain security-help"; content = `${logo()}${intro("Who can help with what", "Two different problems get mistaken for each other. Only one of them has someone to call.")}<div class="security-help-columns"><div class="enroll-card"><div class="security-help-heading enroll-accent">${icon("refresh-cw", 15)}<h2>Cannot reach ${name}</h2></div>${copy("The address does not answer, the machine is down, the workspace disappeared after a move.")}<div class="security-help-divider">${copy("Whoever runs the machine can help. They run the machine, its address and its backups of the encrypted storage.")}</div>${caption("Contact the person who gave you this workspace using your usual contact method.")}</div><div class="enroll-card"><div class="security-help-heading enroll-warning">${icon("lock", 15)}<h2>Cannot unlock ${name}</h2></div>${copy("The passkey is gone, the password is forgotten, the workspace answers but will not open.")}<div class="security-help-divider">${copy("Only your own unlock methods or your recovery key can open it. Deck gives whoever runs the machine no unlock override — there is nothing on their side to reset.")}</div>${button("recovery", "Use recovery key", "secondary", "enroll-button-small")}</div></div>${caption(`Whoever runs the machine can confirm the machine is running and can delete it. Neither of those opens the storage. ${link("lost-methods", "No unlock methods left?")}`)}`; break;
    case "lost-methods": modifier = "security-plain security-lost"; content = `${logo()}<div class="enroll-intro">${status("No way in", "error")}${title(`Without an unlock method, ${name} cannot be opened`)}${copy("If every unlock method for this workspace is gone and there is no recovery key, the stored data stays encrypted. Deck gives whoever runs the machine no override. There is no reset to request.")}</div><div class="enroll-card enroll-muted security-survives"><span class="enroll-eyebrow">What is not lost</span>${copy("Anything pushed to a git remote is on that remote, untouched by this.")}${copy("Your account still works. Ask whoever runs the machine about a fresh workspace, where you can clone your repositories again.")}${copy("Whoever runs the machine can delete the encrypted storage when you ask them to.")}</div><div class="enroll-actions">${button("cancel", "Back to unlock methods")}${button("help", "Get help", "ghost")}</div>`; break;
  }
  const candidates = view.methodChoices?.filter(method => s.kind === "password" ? method.kind === "password" : s.kind === "recovery" ? method.kind === "recovery" : method.kind === "passkey") ?? [];
  const selector = candidates.length > 1 && ["locked", "authorize-browser", "resume", "password", "recovery", "unlock-failed"].includes(s.kind)
    ? `<label class="enroll-field"><span>Choose a saved method</span><select class="enroll-input" data-security-method>${candidates.map(method => `<option value="${esc(method.id)}"${method.id === view.selectedMethod ? " selected" : ""}>${method.kind === "passkey" ? "Passkey" : method.kind === "password" ? "Encryption password" : "Recovery key"} · ${esc(method.id.slice(-8))}</option>`).join("")}</select></label>` : "";
  const section = `<section class="enroll-content ${modifier}" aria-label="Workspace security"${view.busy ? ' aria-busy="true" inert' : ""}>${content}${selector}${view.error ? `<p class="enroll-copy-small enroll-error" role="alert">${esc(view.error)}</p>` : ""}${view.notice ? `<p class="enroll-caption" role="status">${esc(view.notice)}</p>` : ""}</section>`;
  return `<main class="enrollment security-runtime${backdrop ? " security-with-backdrop" : ""}" data-mono="workspace" data-security-runtime="${s.kind}">${backdrop ? `<div class="security-blank-workspace" aria-hidden="true"><div></div><div><i></i><span></span></div></div><div class="security-scrim">${section}</div>` : section}</main>`;
}

export function bindRuntimeActions(root: HTMLElement, run: (action: RuntimeAction) => Promise<void>, onError: (error: unknown) => void): () => void {
  const abort = new AbortController();
  const dispatch = (action: RuntimeAction): void => { void run(action).catch(onError); };
  root.addEventListener("click", event => {
    const button = event.target instanceof Element ? event.target.closest<HTMLButtonElement>("[data-security-action]") : null;
    if (!button || button.disabled) return;
    const kind = button.dataset.securityAction;
    if (kind === "import-kit") root.querySelector<HTMLInputElement>("[data-security-kit-file]")?.click();
    else if (kind) dispatch({ kind } as RuntimeAction);
  }, { signal: abort.signal });
  root.addEventListener("submit", event => {
    if (!(event.target instanceof HTMLFormElement)) return;
    event.preventDefault();
    const name = event.target.dataset.securityForm;
    if (name !== "password" && name !== "recovery") return;
    const value = String(new FormData(event.target).get(name) ?? "");
    event.target.reset();
    dispatch({ kind: name === "password" ? "submit-password" : "submit-recovery", value });
  }, { signal: abort.signal });
  root.addEventListener("change", event => {
    if (event.target instanceof HTMLSelectElement && event.target.matches("[data-security-method]")) { dispatch({ kind: "select-method", value: event.target.value }); return; }
    if (!(event.target instanceof HTMLInputElement) || !event.target.matches("[data-security-kit-file]")) return;
    const file = event.target.files?.[0];
    event.target.value = "";
    if (file) dispatch({ kind: "import-kit", file });
  }, { signal: abort.signal });
  return () => abort.abort();
}
