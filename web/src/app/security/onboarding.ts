import { escapeHtml as esc } from "../../lib/escape.ts";
import { lucideIcon as icon, type IconName } from "../../ui/icons.ts";
import { googleMark } from "../login.ts";
import { enrollmentHtml, type EnrollmentView, type EnrollmentWorkspace } from "./enrollment.ts";

export type EncryptionPolicy = "optional" | "required" | "unavailable";
export type OnboardingEncryption = { kind: "encrypted"; methodName: string } | { kind: "unencrypted"; policy: "optional" | "unavailable" };
export interface OnboardingProgressRow { label: string; state: "done" | "active" | "pending"; detail?: string }
export type OnboardingScreen =
  | { kind: "welcome"; policy: EncryptionPolicy }
  | { kind: "google" }
  | { kind: "needlink"; canSwitchAccount?: boolean }
  | { kind: "expired" | "used" | "unreachable" | "interrupted" }
  | { kind: "notowner"; email?: string }
  | { kind: "resume"; email?: string; startedAt?: string; rows: readonly OnboardingProgressRow[]; requiresMethod: boolean }
  | { kind: "encryption"; policy: EncryptionPolicy; email?: string; boundaryOpen?: boolean }
  | { kind: "enrollment"; enrollment: EnrollmentView["screen"] }
  | { kind: "finishing"; encrypted?: boolean; rows: readonly OnboardingProgressRow[] }
  | { kind: "unconfirmed" }
  | { kind: "migration" }
  | { kind: "failed"; methodKind: "passkey" | "password" }
  | { kind: "done"; encryption: OnboardingEncryption; email?: string }
  | { kind: "signin"; encrypted: boolean };

export interface OnboardingView {
  workspace: EnrollmentWorkspace;
  screen: OnboardingScreen;
  busy?: boolean;
  error?: string;
  notice?: string;
}

const p = (body: string): string => `<p class="onboarding-copy">${body}</p>`;
const caption = (body: string): string => `<p class="enroll-caption">${body}</p>`;
const title = (body: string): string => `<h1 class="enroll-title" tabindex="-1">${body}</h1>`;
const eyebrow = (body: string): string => `<span class="enroll-eyebrow">${body}</span>`;
const mono = (value: string): string => `<span class="enroll-mono">${esc(value)}</span>`;
const brand = (step = ""): string => `<div class="enroll-brand-row"><img class="enroll-logo" src="/brand/noisefloor-lockup-inverse.svg" alt="Noisefloor">${step ? `<span class="enroll-step">${esc(step)}</span>` : ""}</div>`;
const button = (action: string, label: string, secondary = false, symbol?: IconName): string => `<button type="button" class="enroll-button enroll-button-${secondary ? "secondary" : "primary"}" data-onboarding-action="${action}">${symbol ? icon(symbol, 16) : ""}${label}</button>`;
const google = (): string => button("google-sign-in", `${googleMark()}Sign in with Google`, true);
const intro = (heading: string, body: string, label = ""): string => `<div class="onboarding-intro">${label ? eyebrow(label) : ""}${title(heading)}${p(body)}</div>`;
const status = (label: string, tone: "warning" | "error" | "success", symbol: IconName = "triangle-alert"): string => `<div class="enroll-status enroll-${tone}">${icon(symbol, 15)}<span>${label}</span></div>`;
const card = (body: string, muted = false): string => `<div class="enroll-card${muted ? " enroll-muted" : ""} onboarding-note">${body}</div>`;
function facts(rows: readonly [string, string, boolean?][]): string {
  return `<dl class="enroll-card enroll-flush onboarding-facts">${rows.map(([key, value, machine]) => `<div><dt>${esc(key)}</dt><dd${machine ? ' class="enroll-mono"' : ""}>${esc(value)}</dd></div>`).join("")}</dl>`;
}
function progress(rows: readonly OnboardingProgressRow[]): string {
  return `<div class="enroll-card enroll-flush onboarding-progress" role="status">${rows.map(row => `<div class="onboarding-progress-row" data-progress="${row.state}"><span class="enroll-symbol enroll-success">${row.state === "done" ? icon("circle-check", 15) : row.state === "active" ? '<span class="enroll-spinner" aria-hidden="true"></span>' : '<span class="enroll-empty-circle" aria-hidden="true"></span>'}</span><span>${esc(row.label)}</span>${row.detail ? `<span class="enroll-caption">${esc(row.detail)}</span>` : ""}</div>`).join("")}</div>`;
}
function protection(name: string, open: boolean): string {
  return `<button type="button" class="enroll-disclosure" data-onboarding-action="toggle-boundary" aria-expanded="${open}" aria-controls="onboarding-protection">What this protects</button>${open ? `<div id="onboarding-protection" class="enroll-card enroll-muted onboarding-protection"><p><strong>While ${name} is locked,</strong> its stored files, session history and notes are encrypted. Only your unlock methods, or your recovery key, open them.</p><p><strong>While it is running, it is open.</strong> Agents need to read your files to work. Someone controlling the machine could look at what is in use, or change the service in a way that affects later unlocks.</p><p>This does not encrypt everything in memory, and it cannot guarantee privacy from a host that means harm.</p><p>Google signs you in. Recovering your Google account does not recover ${name} — only an unlock method or your recovery key can.</p></div>` : ""}`;
}
function encryption(view: OnboardingView, screen: Extract<OnboardingScreen, { kind: "encryption" }>): string {
  const name = esc(view.workspace.name);
  const step = `Step 2${screen.policy === "required" ? " of 4" : screen.policy === "unavailable" ? " of 2" : ""} · Encryption`;
  if (screen.policy === "optional") return `${brand(step)}${intro("Workspace encryption", `Deck can encrypt what ${name} stores, so it opens only with an unlock method you choose.${screen.email ? ` Signed in as ${mono(screen.email)}.` : ""}`)}
    <section class="enroll-card enroll-selected onboarding-choice"><div class="enroll-card-heading">${icon("lock", 15)}<h2>Enable workspace encryption</h2><span class="enroll-badge enroll-badge-accent">Recommended</span></div>${p(`Next you set up an unlock method and save a recovery key. ${name} asks for one whenever it starts.`)}${button("enable-encryption", "Enable workspace encryption")}</section>
    <section class="enroll-card onboarding-choice"><h2>Continue without Deck encryption</h2>${p("You still sign in with Google. Deck does not add its protection to stored workspace data. Any protection the host's storage provides is separate.")}${button("continue-unencrypted", "Continue without Deck encryption", true)}</section>
    ${caption("Choose now: Deck cannot yet turn on encryption for a workspace that already holds data.")}${protection(name, screen.boundaryOpen === true)}`;
  const required = screen.policy === "required";
  return `${brand(step)}<div class="onboarding-intro onboarding-policy-intro"><div class="onboarding-policy-title">${title("Workspace encryption")}<span class="enroll-badge${required ? " enroll-badge-accent" : ""}">${required ? "Required" : "Not available here"}</span></div>${p(required ? `This workspace requires encryption for its stored data. Next you set up an unlock method and save a recovery key — ${name} asks for one whenever it starts.` : `This installation does not support Deck encryption. You can finish setting up ${name} without it.`)}</div>
    ${required ? `<div class="enroll-card enroll-flush onboarding-steps"><div><span>3</span><span>Choose how to unlock</span><span class="enroll-caption">Passkey or password</span></div><div><span>4</span><span>Save and check a recovery key</span></div></div>${button("enable-encryption", "Set up encryption")}${protection(name, screen.boundaryOpen === true)}` : card(`${icon("info", 14)}<div>${p("You still sign in with Google. Deck does not add its protection to stored workspace data; any protection the host's storage provides is separate.")}${p("Deck cannot yet turn on encryption for a workspace that already holds data.")}</div>`, true) + button("continue-unencrypted", "Finish setup")}`;
}

/** Renders supplied observations only; the coordinator owns policy and completion. */
export function onboardingHtml(view: OnboardingView): string {
  const { screen: s, workspace: w } = view;
  if (s.kind === "enrollment") return enrollmentHtml({ workspace: w, screen: s.enrollment, busy: view.busy, error: view.error, notice: view.notice, setupProgress: s.enrollment.kind.startsWith("recovery") ? "Step 4 of 4 · Recovery key" : "Step 3 of 4 · Unlock method", onboarding: true });
  const name = esc(w.name);
  let body: string;
  switch (s.kind) {
    case "welcome": body = `${brand(`Step 1${s.policy === "required" ? " of 4" : s.policy === "unavailable" ? " of 2" : ""} · Sign in`)}${intro("Set up your workspace", `Sign in with Google to make ${name} yours${s.policy === "optional" ? ", then choose whether Deck encrypts what it stores." : s.policy === "required" ? ", then set up its encryption." : ". It takes two steps."}`, "Setup link")}${facts([["Workspace", w.name], ["Address", w.address, true]])}${google()}${caption(`This link works once. The eligible Google account you sign in with becomes the owner of ${name}, and the link cannot be used to claim it again.`)}`; break;
    case "google": body = `${brand()}${intro("Continue with Google", "Use Google’s sign-in page to choose your account. Deck checks that account before continuing setup.")}${google()}`; break;
    case "needlink": body = `${brand()}${intro("Open your setup link to begin", `${name} does not have an owner yet. It is set up from its setup link — a private link that works once.`, "Not set up yet")}${card("<p><strong>Installed Deck yourself?</strong> The setup link was given to you when installation finished.</p><p><strong>Someone set it up for you?</strong> Ask them to send you the link.</p>", true)}${caption(`Signing in here cannot claim ${name}. Only the setup link can.`)}${s.canSwitchAccount ? button("switch-account", "Use a different account", true) : ""}`; break;
    case "expired": body = `${brand()}${status("Link expired", "warning")}${title("This setup link has expired")}${p(`It can no longer be used, and nothing in ${name} was changed. Ask whoever sent it for a new setup link.`)}<div class="onboarding-separated">${p(`Already started setting up ${name}? Sign in with the account you used, and setup continues where it stopped.`)}${google()}</div>`; break;
    case "used": body = `${brand()}${eyebrow("Link already used")}${title("This setup link has already been used")}${p(`${name} has an owner. This link cannot set it up again or change who owns it. If you are the owner, sign in.`)}${google()}`; break;
    case "notowner": body = `${brand()}${eyebrow("Different account")}${title(`This account does not own ${name}`)}${p(`${s.email ? `You signed in as ${mono(s.email)}. ` : ""}${name} belongs to another Google account, and nothing was changed.`)}${button("switch-account", "Use a different account", true)}`; break;
    case "unreachable": body = `${brand()}${status("Not reachable", "warning")}${title(`Cannot reach ${name} right now`)}${p(`This page cannot tell whether ${name} is new, part-way through setup, or already in use until it reconnects. Checking again does not start or change setup.`)}${button("check-state", "Check again", false, "refresh-cw")}`; break;
    case "interrupted": body = `${brand()}${intro(`Finish setting up ${name}`, "Setup was started and has not finished. Sign in with the Google account that started it to continue where it stopped.", "Setup not finished")}${google()}${caption("Another browser, or cleared cookies, does not start setup over. What was done is kept.")}`; break;
    case "resume": body = `${brand()}${intro("Pick up where you left off", `${s.email ? `Signed in as ${mono(s.email)}. ` : ""}Setup of ${name} has not finished${s.startedAt ? `; it started on ${esc(s.startedAt)}` : ""}.`)}${progress(s.rows)}${s.requiresMethod ? p("Verify your saved unlock method before continuing. If a new recovery key is needed, save and check its replacement.") : ""}${button("resume-setup", s.requiresMethod ? "Verify your saved unlock method" : "Continue setup")}`; break;
    case "encryption": body = encryption(view, s); break;
    case "finishing": body = `${brand()}${title("Finishing setup")}${progress(s.rows)}${caption(`If this tab closes, sign in to ${name} again to check its progress.${s.encrypted === false ? "" : " Deck may ask for your saved unlock method before continuing."}`)}`; break;
    case "migration": body = `${brand()}${intro("Waiting for your existing data", "Your encryption method is saved. The person running this workspace needs to import and verify its existing data before setup can finish.", "Import needed")}${caption("Keep your password and recovery key private. The import does not require sharing them. After the import, verify your saved unlock method to continue.")}${button("check-state", "Check import status", false, "refresh-cw")}`; break;
    case "unconfirmed": body = `${brand()}${status("Not known", "warning")}${title("Could not confirm setup finished")}${p(`The result of setup on ${name} is not known. It may have finished, and it may not. Reconnecting checks — nothing starts over.`)}${button("check-state", "Reconnect and check", false, "refresh-cw")}`; break;
    case "failed": body = `${brand()}${status("Not finished", "error")}${title("Encrypted storage could not be set up")}${p(`${name} is not ready, and it is still set to use encryption. Verify your saved ${s.methodKind} to continue safely; Deck checks the existing setup before proceeding.`)}${button("resume-setup", "Verify and continue", false, "refresh-cw")}${caption("Setup never continues without encryption on its own. If this keeps happening, ask whoever runs the machine to check its storage.")}`; break;
    case "done": body = `${brand()}${status("Setup confirmed", "success", "circle-check")}${title("Workspace ready")}${facts([...(s.email ? [["Owner", s.email] as [string, string]] : []), ["Encryption", s.encryption.kind === "encrypted" ? "On · stored data is encrypted while locked" : s.encryption.policy === "unavailable" ? "Not available on this installation" : "Off · Deck encryption not used"], ...(s.encryption.kind === "encrypted" ? [["Unlock method", s.encryption.methodName], ["Recovery key", "Checked"]] as [string, string][] : [])])}${caption(s.encryption.kind === "encrypted" ? "Google signs you in. Your unlock methods or recovery key open the workspace. Keep your saved recovery key somewhere independent." : "You sign in with Google. Deck encryption is off; any protection the host's storage provides is separate.")}${button("open-workspace", `Open ${name}`)}`; break;
    case "signin": body = `${brand()}<h1>Sign in to ${mono(w.name)}</h1>${google()}${caption(s.encrypted ? "This workspace is already set up. After sign-in, verify an unlock method to connect this browser when required." : "This workspace is already set up. Sign in with its owner account to continue.")}`; break;
  }
  return `<main class="onboarding" data-mono="workspace" data-onboarding="${s.kind}"${s.kind === "encryption" ? ` data-policy="${s.policy}"` : ""}><section class="onboarding-content" aria-label="Workspace setup"${view.busy ? ' aria-busy="true"' : ""}>${body}${view.error ? `<p class="onboarding-copy enroll-error" role="alert">${esc(view.error)}</p>` : ""}${view.notice ? `<p class="enroll-caption" role="status">${esc(view.notice)}</p>` : ""}</section></main>`;
}
