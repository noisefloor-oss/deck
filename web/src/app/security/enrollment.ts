import { escapeHtml as esc } from "../../lib/escape.ts";
import { lucideIcon as icon, type IconName } from "../../ui/icons.ts";
import { googleMark } from "../login.ts";

export interface EnrollmentWorkspace {
  name: string;
  address: string;
  operator?: string;
}

export type EnrollmentScreen =
  | { kind: "invitation"; expires?: string; boundaryOpen?: boolean }
  | { kind: "invitation-expired" }
  | { kind: "interrupted"; email: string; interruptedAt: string; methodName: string; methodKind: "passkey" | "password" }
  | { kind: "choose-method" }
  | { kind: "passkey-pending" }
  | { kind: "checking-method"; methodKind: "passkey" | "password" }
  | { kind: "passkey-incompatible" }
  | { kind: "password" }
  | { kind: "method-ready"; methodKind: "passkey" | "password"; methodName: string }
  | { kind: "recovery-save"; key: string; example?: boolean }
  | { kind: "recovery-verify"; input?: string; failure?:
    { kind: "wrong-key" } | { kind: "format"; message: string; hint: string } }
  | { kind: "complete"; methodName: string; methodCheckedAt: string; recoveryCheckedAt: string; kitDownloadRequested: boolean };

export interface EnrollmentView {
  workspace: EnrollmentWorkspace;
  screen: EnrollmentScreen;
  busy?: boolean;
  error?: string;
  notice?: string;
  canManageMethods?: boolean;
  setupProgress?: string;
  onboarding?: boolean;
}

const mono = (value: string): string => `<span class="enroll-mono">${esc(value)}</span>`;
const responsive = (desktop: string, mobile: string): string => `${desktop ? `<span class="enroll-desktop">${desktop}</span>` : ""}${mobile ? `<span class="enroll-mobile">${mobile}</span>` : ""}`;
const paragraph = (html: string, small = false): string => `<p class="enroll-copy${small ? " enroll-copy-small" : ""}">${html}</p>`;
const caption = (html: string): string => `<p class="enroll-caption">${html}</p>`;
const heading = (html: string): string => `<h1 class="enroll-title" tabindex="-1">${html}</h1>`;
const intro = (title: string, copy = "", small = false): string => `<div class="enroll-intro">${heading(title)}${copy ? paragraph(copy, small) : ""}</div>`;
const logo = (step?: number, progress?: string): string => progress
  ? `<div class="enroll-brand-row"><img class="enroll-logo" src="/brand/noisefloor-lockup-inverse.svg" alt="Noisefloor"><span class="enroll-step">${esc(progress)}</span></div>`
  : step
  ? `<div class="enroll-brand-row"><img class="enroll-logo" src="/brand/noisefloor-lockup-inverse.svg" alt="Noisefloor"><span class="enroll-step">Step ${step} of 3</span></div>`
  : `<img class="enroll-logo" src="/brand/noisefloor-lockup-inverse.svg" alt="Noisefloor">`;
const button = (action: string, label: string, variant = "primary", iconName?: IconName, disabled = false): string =>
  `<button type="button" data-enroll-action="${action}" class="enroll-button enroll-button-${variant}"${disabled ? " disabled" : ""}>${iconName ? icon(iconName, 16) : ""}${label}</button>`;
const status = (label: string, tone: "success" | "error"): string =>
  `<div class="enroll-status enroll-${tone}">${icon(tone === "success" ? "circle-check" : "triangle-alert", 15)}<span>${label}</span></div>`;
const spinner = (): string => `<span class="enroll-spinner" aria-hidden="true"></span>`;
const row = (html: string, symbol: IconName | "pending" | "waiting", tone = "success"): string =>
  `<div class="enroll-check-row${symbol === "waiting" ? " enroll-waiting" : ""}"><span class="enroll-symbol enroll-${tone}">${symbol === "pending" ? spinner() : symbol === "waiting" ? '<span class="enroll-empty-circle"></span>' : icon(symbol, 15)}</span><span${symbol === "pending" ? ' class="enroll-primary"' : ""}>${html}</span></div>`;

function protection(workspace: EnrollmentWorkspace): string {
  const name = esc(workspace.name);
  return `<div class="enroll-card enroll-muted enroll-protection" id="enroll-protection">
    <div>${icon("lock", 14)}<span><strong>While ${name} is locked,</strong> the files, session history and notes stored on it are encrypted. Only your unlock methods, or your recovery key, can open them.</span></div>
    <div>${icon("triangle-alert", 14)}<span><strong>While it is running, it is open.</strong> Someone who controls the machine could deliberately look at what is in use, or change the service in a way that affects later unlocks. Lock ${name} when you are done with it.</span></div>
    <div>${icon("info", 14)}<span>${workspace.operator ? `Host ${esc(workspace.operator)}` : "Whoever runs this machine"} can restart, resize or delete the machine, and can tell you whether it is reachable. Deck gives them no override for unlocking it — there is no host-operated reset.</span></div>
    <div>${icon("circle-help", 14)}<span>If you lose every unlock method and your recovery key, the stored data stays encrypted and cannot be recovered by anyone.</span></div>
  </div>`;
}

function invitation(view: EnrollmentView, screen: Extract<EnrollmentScreen, { kind: "invitation" }>): string {
  const { workspace: w } = view;
  const name = esc(w.name);
  const disclosure = paragraph(`You choose how to unlock ${name}, and its stored data is encrypted whenever it is locked. Deck gives the host no override for unlocking or recovering your workspace.`, true);
  const detail = screen.boundaryOpen === true;
  return `${logo()}${detail ? heading(`Set up ${name}`) : `<div class="enroll-intro"><span class="enroll-eyebrow">Single-use invitation</span>${heading(`Set up ${name}`)}${paragraph(responsive("You have been invited to own this workspace. Setup takes three steps: sign in, choose how to unlock, save a recovery key.", "Sign in, choose how to unlock, save a recovery key."))}</div>
    <dl class="enroll-card enroll-flush enroll-identity">
      <div class="enroll-desktop"><dt>Workspace</dt><dd class="enroll-mono">${esc(w.name)}</dd></div>
      <div><dt>Address</dt><dd class="enroll-mono">${esc(w.address)}</dd></div>
      ${w.operator ? `<div><dt>Hosted by</dt><dd>${mono(w.operator)}</dd></div>` : ""}
      <div class="enroll-desktop"><dt>Invitation</dt><dd class="enroll-secondary">Opens once${screen.expires ? ` · expires ${esc(screen.expires)}` : ""}</dd></div>
    </dl>`}
    ${disclosure}<button type="button" data-enroll-action="toggle-boundary" class="enroll-disclosure" aria-expanded="${detail}" aria-controls="enroll-protection">What this protects${icon(detail ? "chevron-up" : "chevron-down", 14)}</button>
    ${detail ? protection(w) : ""}
    ${button("google-sign-in", `${googleMark()}${responsive("Sign in with Google to continue", "Sign in with Google")}`, "secondary")}
    ${detail ? "" : `<div class="enroll-desktop">${caption(`Signing in identifies you to ${name}. You will choose how to unlock it next.`)}</div>`}`;
}

function choose(w: EnrollmentWorkspace, progress?: string): string {
  const name = esc(w.name);
  return `${logo(2, progress)}${intro(`Choose how to unlock ${name}`, responsive(`You sign in with Google. Unlocking is separate: whenever ${name} starts, it asks to be unlocked before your sessions can run.`, `You sign in with Google. Unlocking is separate — ${name} asks whenever it starts.`))}
    <section class="enroll-card enroll-selected enroll-method-card"><div class="enroll-card-heading"><span class="enroll-accent">${icon("lock", 16)}</span><h2>Use a passkey</h2><span class="enroll-badge enroll-badge-accent">Recommended</span></div>
      ${paragraph(responsive(`Deck creates a passkey for ${name} and your browser or password manager saves it. Not every passkey can unlock a workspace, so it is checked before setup finishes.`, `Deck creates a passkey for ${name} and your phone saves it, then checks it can unlock.`), true)}
      ${button("create-passkey", "Choose a passkey")}</section>
    <section class="enroll-card enroll-method-card"><div class="enroll-card-heading"><span class="enroll-desktop enroll-tertiary">${icon("ellipsis", 16)}</span><h2>Unlock with an encryption password</h2></div>
      ${paragraph(responsive(`For browsers and password managers whose passkeys cannot unlock. You type this password each time ${name} is unlocked. Deck cannot reset it or send it to you.`, "You type an encryption password every time. Deck cannot reset it."), true)}
      ${button("choose-password", "Use a password instead", "password-choice")}</section>
    ${caption(responsive("You can add more unlock methods later. A recovery key comes last, and setup is not finished without it.", "A recovery key comes last. Setup is not finished without it."))}`;
}

function incompatible(w: EnrollmentWorkspace, progress?: string, onboarding = false): string {
  const name = esc(w.name);
  return `${logo(undefined, progress)}<div class="enroll-intro enroll-incompatible-intro">${status("Not enabled", "error")}${heading("This passkey was created, but cannot unlock this workspace here")}
    ${paragraph(responsive(`It was created and saved by your browser, but it could not unlock ${name} here, so Deck did not enable it as an unlock method. ${onboarding ? "Encryption is still selected, and setup is not finished." : `Nothing has changed — ${name} is still locked.`}`, `It was created and saved by your browser, but it could not unlock ${name} here. ${onboarding ? "Encryption is still selected, and setup is not finished." : `Nothing has changed — ${name} is still locked.`}`))}</div>
    <div class="enroll-actions-vertical">${button("create-passkey", "Try a different passkey")}${button("choose-password", responsive("Use an encryption password instead", "Use an encryption password"), "secondary")}</div>
    <div class="enroll-card enroll-muted enroll-desktop enroll-why"><h2>Why this happens</h2>${paragraph("Support differs between browsers and password managers. The same passkey often works in a different browser on the same machine, and a passkey stored on a phone or a security key is worth trying too.", true)}</div>
    <div class="enroll-mobile">${caption("Support differs between browsers and password managers — the same passkey often works elsewhere.")}</div>`;
}

function password(w: EnrollmentWorkspace, busy: boolean, progress?: string): string {
  return `${logo(undefined, progress)}${intro("Set an encryption password", `You sign in with Google. This password unlocks ${esc(w.name)}, and you type it each time.`, true)}
    <form data-enroll-form="password" class="enroll-form">
      <label class="enroll-field"><span id="enroll-password-label">Encryption password</span><input aria-labelledby="enroll-password-label" name="password" type="password" autocomplete="new-password" required maxlength="1024" class="enroll-input" aria-describedby="enroll-password-hint"${busy ? " disabled" : ""}>
        <span class="enroll-length-meter" aria-hidden="true"><i></i><i></i><i></i><i></i></span>
        <span class="enroll-caption" id="enroll-password-hint">At least 15 characters; a few unrelated words beat one clever one.</span></label>
      <label class="enroll-field"><span>Repeat it</span><input name="repeat" type="password" autocomplete="new-password" required maxlength="1024" class="enroll-input"${busy ? " disabled" : ""}></label>
      <div class="enroll-card enroll-muted enroll-warning-copy">${icon("triangle-alert", 14)}${paragraph("Save this in your password manager if that is where your passwords live. Keep your recovery key somewhere else — if both sit in one place, one break-in gets both. Deck cannot reset this password or send it to you.", true)}</div>
      <button class="enroll-button enroll-button-primary" type="submit"${busy ? " disabled" : ""}>${busy ? "Checking password…" : "Set password and check it"}</button>
    </form>`;
}

function recoverySave(w: EnrollmentWorkspace, screen: Extract<EnrollmentScreen, { kind: "recovery-save" }>, progress?: string): string {
  const pieces = screen.key.split("-");
  const version = pieces.shift() ?? "";
  const check = pieces.pop() ?? "";
  const name = esc(w.name);
  return `${logo(3, progress)}${intro("Save your recovery key", responsive(`This key can unlock ${name} if you lose every other way in. Keep it private. Store a copy somewhere you can reach without this workspace or your usual password manager.`, "Keep it private. Store a copy somewhere you can reach without this workspace or your usual password manager."))}
    <div class="enroll-card enroll-key-card"><div class="enroll-key-heading"><span class="enroll-eyebrow">${responsive("Recovery key · 52 characters · version", "Version")} ${mono(version)}</span>${screen.example ? `<span class="enroll-badge enroll-badge-warning">${responsive("Example key — not a real one", "Example key")}</span>` : ""}</div>
      <div class="enroll-key-groups" aria-label="Recovery key">${pieces.map(group => `<span>${esc(group)}</span>`).join("")}</div>
      <div class="enroll-key-check"><span class="enroll-caption">Check group</span><span>${esc(check)}</span></div>
    </div>
    <div class="enroll-actions enroll-save-actions">${button("copy-key", `${responsive(icon("file-text", 16), "")}Copy key`, "secondary")}${button("download-kit", responsive("Download recovery kit", "Download kit"), "secondary", "download")}<span class="enroll-desktop">${button("print-kit", "Print", "secondary")}</span></div>
    <div class="enroll-card enroll-muted enroll-kit-note enroll-desktop">${paragraph(`The kit is a single file holding this key, the address ${mono(w.address)}, and short instructions for using it.`, true)}<div class="enroll-warning-copy">${icon("triangle-alert", 14)}${paragraph(`If you lose every unlock method and this key, what is stored in ${name} cannot be decrypted — by you, by ${w.operator ? `host ${esc(w.operator)}` : "whoever runs this machine"}, or by anyone else. There is no reset.`, true)}</div></div>
    <div class="enroll-mobile">${caption(`Lose every unlock method and this key, and what is stored in ${name} cannot be decrypted by anyone. There is no reset.`)}</div>
    ${button("verify-recovery", "I have saved it — check it works")}`;
}

function recoveryVerify(w: EnrollmentWorkspace, screen: Extract<EnrollmentScreen, { kind: "recovery-verify" }>, busy: boolean, progress?: string): string {
  const failure = screen.failure;
  const name = esc(w.name);
  const message = failure?.kind === "wrong-key" ? `This key did not unlock ${name}.` : failure?.kind === "format" ? esc(failure.message) : "";
  const hint = failure?.kind === "wrong-key" ? `Check you are on ${mono(w.address)} and that the kit you opened names the same address — a key from another workspace will not work here.`
    : failure?.kind === "format" ? esc(failure.hint) : "Spacing, dashes and capitals do not matter.";
  const input = `<textarea aria-labelledby="enroll-key-label" name="recovery" class="enroll-input enroll-recovery-input${failure ? ` enroll-input-${failure.kind === "format" ? "warning" : "error"}` : ""}" rows="3" spellcheck="false" autocapitalize="off" autocomplete="off" maxlength="512" placeholder="DK1-…" aria-describedby="enroll-key-hint${failure ? " enroll-key-error" : ""}"${failure ? ' aria-invalid="true"' : ""}${busy ? " disabled" : ""}>${esc(screen.input ?? "")}</textarea>`;
  const field = failure?.kind === "format" ? `<span class="enroll-group-editor"><span class="enroll-group-preview" aria-hidden="true">${(screen.input ?? "").trim().split(/[\s-]+/).map(group => `<span${group.length !== 4 && group.toUpperCase() !== "DK1" ? ' class="enroll-malformed-group"' : ""}>${esc(group)}</span>`).join("")}</span>${input}</span>` : input;
  return `${logo(undefined, progress)}${failure ? heading("Check your recovery key") : intro("Check your recovery key", responsive(`Paste the copy you just saved, or import the kit file. Deck checks it can unlock ${name}.`, `Paste the copy you saved. Deck checks it can unlock ${name}.`))}
    <form data-enroll-form="recovery" class="enroll-form">
      <label class="enroll-field"><span id="enroll-key-label">Recovery key</span>${field}
        ${message ? `<span class="enroll-caption enroll-${failure?.kind === "format" ? "warning" : "error"}" role="alert" id="enroll-key-error">${message}</span>` : ""}<span class="enroll-caption" id="enroll-key-hint">${hint}</span></label>
      <input type="file" accept="application/json,.json" data-enroll-kit-file hidden>
      ${failure ? "" : `<div class="enroll-actions enroll-import-row">${button("import-kit", "Import kit file", "secondary", "upload")}<span class="enroll-caption enroll-desktop">or drop it anywhere on this page</span></div>`}
      <div class="enroll-actions enroll-verify-actions"><button class="enroll-button enroll-button-primary" type="submit"${!screen.input || busy ? " disabled" : ""}>${busy ? "Checking key…" : "Check key"}</button>
        ${failure?.kind === "wrong-key" ? button("show-recovery", "Show my key again", "ghost") : failure?.kind === "format" ? button("import-kit", "Import kit file instead", "secondary", "upload") : '<span class="enroll-caption enroll-desktop">Setup finishes once this passes.</span>'}</div>
    </form>`;
}

export function enrollmentHtml(view: EnrollmentView): string {
  const { screen: s, workspace: w } = view;
  const name = esc(w.name);
  let content: string;
  switch (s.kind) {
    case "invitation": content = invitation(view, s); break;
    case "invitation-expired": content = `${logo(undefined, view.setupProgress)}${intro("This invitation no longer works", "Invitations open once and expire after 72 hours. This one has been used or has run out.")}
      <div class="enroll-card enroll-expired-note">${paragraph(`Ask ${w.operator ? `host ${mono(w.operator)}` : "whoever runs this machine"} for a new invitation to ${mono(w.name)}.`, true)}${caption(`This link cannot complete setup. It does not change an existing workspace or its unlock methods. If somebody else has already finished setup on ${name}, they hold its unlock methods.`)}</div>${button("copy-address", "Copy the workspace address", "secondary")}`; break;
    case "interrupted": content = `${logo(undefined, view.setupProgress)}${intro(`Finish setting up ${name}`, `You stopped part way through on ${esc(s.interruptedAt)}. Your ${s.methodKind} is already in place.`)}
      <div class="enroll-card enroll-flush">${row(`Signed in as ${mono(s.email)}`, "circle-check")}${row(`Unlock method added and tested — <strong>${esc(s.methodName)}</strong>`, "circle-check")}${row("Recovery key not checked", "triangle-alert", "warning")}</div>
      ${paragraph(`Until you save and check a recovery key, losing that one ${s.methodKind} means losing everything stored in ${name}. It takes a minute.`, true)}<div class="enroll-actions">${button("resume-recovery", "Save a recovery key")}${button("sign-out", "Not now", "ghost")}</div>`; break;
    case "choose-method": content = choose(w, view.setupProgress); break;
    case "passkey-pending": content = `${logo(undefined, view.setupProgress)}${heading(`Choose how to unlock ${name}`)}<div class="enroll-card enroll-pending-card"><div class="enroll-card-heading" role="status">${spinner()}<span>Waiting for your browser</span></div>${paragraph(`Your browser is asking where to save the passkey for ${name}.`, true)}${button("cancel-passkey", "Cancel", "ghost enroll-button-small")}</div>`; break;
    case "checking-method": content = `${logo(undefined, view.setupProgress)}${intro(`Checking this ${s.methodKind}`, `Checking your ${s.methodKind} can unlock this workspace.`)}<div class="enroll-card enroll-flush enroll-checking">${row(s.methodKind === "passkey" ? "Passkey created and saved" : "Password entered", "circle-check")}${row(`Checking it unlocks ${name}`, "pending")}${row("Saving it as an unlock method", "waiting")}</div>${caption(s.methodKind === "passkey" ? "Your browser may ask for the passkey a second time. Deck enables this unlock method only after the check succeeds." : "Deck enables this unlock method only after the check succeeds.")}`; break;
    case "passkey-incompatible": content = incompatible(w, view.setupProgress, view.onboarding); break;
    case "password": content = password(w, view.busy === true, view.setupProgress); break;
    case "method-ready": content = `${logo(undefined, view.setupProgress)}<div class="enroll-intro enroll-confirm-intro">${status("Checked", "success")}${heading(s.methodKind === "passkey" ? "Passkey checked" : "Password checked")}${paragraph(`Your ${s.methodKind} passed its unlock check. Next, save a recovery key.`)}</div><form data-enroll-form="method-name" class="enroll-form"><label class="enroll-field"><span id="enroll-method-label">Name this method</span><input aria-labelledby="enroll-method-label" name="methodName" class="enroll-input" maxlength="80" value="${esc(s.methodName)}"><span class="enroll-caption">For your own list later. “Work laptop”, “phone”, “yubikey in the drawer”.</span></label><button type="submit" class="enroll-button enroll-button-primary"${view.busy ? " disabled" : ""}>Continue to your recovery key</button></form>${caption("One method is not enough on its own. Setup finishes when a recovery key is saved and checked.")}`; break;
    case "recovery-save": content = recoverySave(w, s, view.setupProgress); break;
    case "recovery-verify": content = recoveryVerify(w, s, view.busy === true, view.setupProgress); break;
    case "complete": content = `${logo(undefined, view.setupProgress)}<div class="enroll-intro enroll-confirm-intro">${status("Recovery key checked", "success")}${heading(`${name} is ready`)}</div><div class="enroll-card enroll-flush">${row(`<strong>${esc(s.methodName)}</strong> passed its unlock check · ${esc(s.methodCheckedAt)}`, "circle-check")}${row(`<strong>Recovery key</strong> passed its unlock check · ${esc(s.recoveryCheckedAt)}`, "circle-check")}${row(s.kitDownloadRequested ? "Your recovery kit download was requested. Deck does not keep a copy." : "Keep your saved recovery key somewhere you can reach without this workspace.", "info", "tertiary")}</div>${paragraph("Add a second passkey whenever you like — a phone or a security key means a lost laptop is an inconvenience rather than a recovery.", true)}<div class="enroll-actions">${button("open-workspace", `Open ${name}`)}${view.canManageMethods !== false ? button("add-method", "Add another unlock method", "ghost") : ""}</div>`; break;
  }
  return `<main class="enrollment" data-mono="workspace" data-enrollment="${s.kind}"${s.kind === "invitation" && s.boundaryOpen ? ' data-boundary-open="true"' : ""}><section class="enroll-content" aria-label="Workspace setup"${view.busy ? ' aria-busy="true"' : ""}>${content}${view.error ? `<p class="enroll-copy-small enroll-error" role="alert">${esc(view.error)}</p>${button("check-state", "Check workspace state", "secondary")}` : ""}${view.notice ? `<p class="enroll-caption" role="status">${esc(view.notice)}</p>` : ""}</section></main>`;
}
