import { escapeHtml as esc } from "../../lib/escape.ts";
import { lucideIcon as icon, type IconName } from "../../ui/icons.ts";
import type { MethodKind } from "../../security/envelope.ts";

export interface ManagedMethod {
  id: string;
  revision: number;
  kind: MethodKind;
  label: string;
  status: "pending" | "active" | "removed";
  checked?: string;
}
export type ManagementScreen =
  | { kind: "list" }
  | { kind: "recovery-checklist"; usedRecovery: { id: string; revision: number } }
  | { kind: "detail"; method: ManagedMethod }
  | { kind: "choose"; replacement?: ManagedMethod }
  | { kind: "password"; replacement?: ManagedMethod }
  | { kind: "authorize"; purpose: "source" | "activate" | "remove" | "test"; method?: ManagedMethod }
  | { kind: "check"; candidate: ManagedMethod }
  | { kind: "ready"; candidate: ManagedMethod; replacement?: ManagedMethod }
  | { kind: "last-method"; method: ManagedMethod }
  | { kind: "remove"; method: ManagedMethod }
  | { kind: "recovery-save"; key: string; example?: boolean }
  | { kind: "incompatible" }
  | { kind: "unknown" }
  | { kind: "loading" };
export interface ManagementView {
  name: string;
  methods: ManagedMethod[];
  screen: ManagementScreen;
  busy?: boolean;
  error?: string;
  notice?: string;
}

export const METHOD_CHANGE_NOTICE = "Removing or replacing a method disconnects browsers authorized by it. Other browsers stay connected. This does not change the storage key or make previously copied data and a working key unreadable.";
const button = (action: string, label: string, variant = "secondary", id?: string, symbol?: IconName): string => `<button type="button" class="enroll-button enroll-button-${variant}" data-method-action="${action}"${id ? ` data-method-id="${esc(id)}"` : ""}>${symbol ? icon(symbol, 15) : ""}${label}</button>`;
const close = (action = "cancel"): string => `<button type="button" class="method-icon-button" data-method-action="${action}" aria-label="Close">${icon("x", 16)}</button>`;
const copy = (value: string): string => `<p class="enroll-copy-small">${value}</p>`;
const hint = (value: string): string => `<p class="enroll-caption">${value}</p>`;
const warning = (value = METHOD_CHANGE_NOTICE): string => `<div class="enroll-card enroll-muted method-warning">${icon("triangle-alert", 14)}${copy(value)}</div>`;
const field = (label: string, name: string, type = "text", value = "", extra = ""): string => `<label class="enroll-field"><span>${label}</span><input name="${name}" type="${type}" class="enroll-input" value="${esc(value)}" maxlength="${type === "password" ? "1024" : "80"}" autocomplete="${name === "current" ? "current-password" : type === "password" ? "new-password" : "off"}" required>${extra}</label>`;
const formButton = (label: string): string => `<button type="submit" class="enroll-button enroll-button-primary">${label}</button>`;
const note = (method: ManagedMethod): string => method.checked ? `Checked ${esc(method.checked)}` : "No unlock check recorded";
const labelKind = (kind: MethodKind): string => kind === "passkey" ? "passkey" : kind === "password" ? "password" : "recovery key";
const checkRow = (label: string): string => `<div class="method-check-row"><span class="enroll-success">${icon("circle-check", 14)}</span><span>${label}</span></div>`;

function methodRow(method: ManagedMethod): string {
  const recovery = method.kind === "recovery";
  return `<div class="method-row" data-method-row="${esc(method.id)}"><span class="method-row-symbol">${icon(recovery ? "circle-help" : method.kind === "passkey" ? "lock" : "file-text", 15)}</span><button type="button" class="method-row-name" data-method-action="detail" data-method-id="${esc(method.id)}"><span>${esc(method.label)}</span><span class="enroll-caption">${note(method)}</span></button><div class="method-row-actions">${button(recovery ? "replace" : "test", recovery ? "Replace" : "Test", "ghost enroll-button-small", method.id)}${recovery ? "" : `<button type="button" class="method-icon-button" data-method-action="detail" data-method-id="${esc(method.id)}" aria-label="Manage ${esc(method.label)}">${icon("ellipsis", 15)}</button>`}</div><span class="method-row-chevron" aria-hidden="true">${icon("chevron-right", 15)}</span></div>`;
}

function list(view: ManagementView): string {
  const active = view.methods.filter(method => method.status === "active");
  const everyday = active.filter(method => method.kind !== "recovery");
  const recovery = active.filter(method => method.kind === "recovery");
  return `<div class="method-list-heading"><div><h1>How ${esc(view.name)} is unlocked</h1>${copy(`<span class="enroll-desktop">Any one of these opens the workspace. Keep at least two.</span><span class="enroll-mobile">Any one of these opens ${esc(view.name)}. Keep at least two.</span>`)}</div><span class="enroll-desktop">${button("add-passkey", "Add passkey", "secondary enroll-button-small", undefined, "plus")}</span></div>
    <section class="enroll-card enroll-flush" aria-label="Everyday unlock methods">${everyday.map(methodRow).join("")}</section>
    <section class="enroll-card enroll-flush" aria-label="Recovery key">${recovery.map(methodRow).join("") || `<div class="method-row">${copy("No active recovery key.")}${button("add-recovery", "Add recovery key")}</div>`}</section>
    <div class="enroll-mobile method-mobile-add">${button("add-passkey", "Add passkey", "secondary", undefined, "plus")}</div>
    <div class="enroll-card enroll-muted method-warning method-list-notice">${icon("info", 14)}${copy(METHOD_CHANGE_NOTICE)}</div>
    ${!everyday.some(method => method.kind === "password") ? button("add-password", "Add encryption password", "ghost") : ""}`;
}

function dialog(view: ManagementView): { title: string; description?: string; body: string; footer?: string; wide?: boolean } {
  const s = view.screen;
  const name = esc(view.name);
  switch (s.kind) {
    case "detail": return { title: s.method.label, description: note(s.method), body: `${button("test", "Test this method", "secondary", s.method.id)}${button("replace", s.method.kind === "password" ? "Change encryption password" : s.method.kind === "recovery" ? "Replace recovery key" : "Replace this method", "secondary", s.method.id)}${s.method.kind !== "recovery" ? button("remove", "Remove this method", "ghost", s.method.id) : hint("Keep this key until a replacement recovery key has been saved and checked.")}`, footer: button("cancel", "Close", "ghost") };
    case "choose": return { title: s.replacement ? `Replace “${s.replacement.label}”` : "Add an unlock method", body: `${copy("The new method is saved and checked before it is enabled.")}${button("add-passkey", "Use a passkey", "primary")}${button("add-password", "Use an encryption password")}${s.replacement ? hint("The existing method keeps working until replacement is confirmed.") : ""}`, footer: button("cancel", "Cancel", "ghost") };
    case "password": return { title: s.replacement ? "Change encryption password" : "Add encryption password", body: `<form data-method-form="password" id="method-password" class="enroll-form">${s.replacement?.kind === "password" ? field("Current password", "current", "password") : ""}${field("New password", "password", "password", "", '<span class="enroll-length-meter" aria-hidden="true"><i></i><i></i><i></i><i></i></span>')}${hint("At least 15 characters. You will enter it again to check the saved method.")}${hint(`The new password is checked against its saved envelope. ${s.replacement ? "The old one keeps working until replacement is confirmed." : "It is enabled only after the check and your authorization."}`)}${warning()}</form>`, footer: `${button("cancel", "Cancel", "ghost")}<button type="submit" form="method-password" class="enroll-button enroll-button-primary">${s.replacement ? "Change and check it" : "Save and check it"}</button>` };
    case "authorize": {
      const purpose = s.purpose === "source" ? "Verify an existing unlock method" : s.purpose === "activate" ? "Authorize this change" : s.purpose === "remove" ? "Authorize removal" : "Test this unlock method";
      const credential = s.method?.kind === "password" ? field("Encryption password", "current", "password") : s.method?.kind === "recovery" ? `<label class="enroll-field"><span>Recovery key</span><textarea name="recovery" class="enroll-input enroll-recovery-input" maxlength="512" spellcheck="false" autocapitalize="off" autocomplete="off" required placeholder="DK1-…"></textarea></label>` : "";
      return { title: purpose, body: `${copy(s.purpose === "source" ? "This unlocks the saved workspace key in this browser so the new method can protect it." : "Verify an active method for this exact request. Your sessions keep running.")}${s.method ? `<p>${esc(s.method.label)}</p>${credential ? `<form id="method-authorize" data-method-form="authorize" class="enroll-form">${credential}${formButton(s.purpose === "test" ? "Test method" : "Verify and continue")}</form>${s.method.kind === "recovery" ? button("import-kit", "Import kit file", "secondary") : ""}` : button("use-method", "Verify with passkey", "primary", s.method.id)}` : view.methods.filter(method => method.status === "active").map(method => button("use-method", esc(method.label), "secondary", method.id)).join("")}`, footer: button("cancel", "Cancel", "ghost") };
    }
    case "check": return { title: `Check the new ${labelKind(s.candidate.kind)}`, body: `${copy("Use the copy you saved. Deck reads the stored envelope again and checks it opens the same workspace key.")}${s.candidate.kind === "passkey" ? button("check-passkey", "Check passkey", "primary") : `<form data-method-form="candidate" class="enroll-form">${s.candidate.kind === "password" ? field("New password", "password", "password") : `<label class="enroll-field"><span>Recovery key</span><textarea name="recovery" class="enroll-input enroll-recovery-input" maxlength="512" spellcheck="false" autocapitalize="off" autocomplete="off" required placeholder="DK1-…"></textarea></label>`}${formButton("Check saved method")}</form>${s.candidate.kind === "recovery" ? button("import-kit", "Import kit file") : ""}`}`, footer: button("cancel", "Cancel", "ghost") };
    case "ready": {
      if (s.replacement) return { title: `Replace “${s.replacement.label}”`, description: "The old one keeps working until replacement is confirmed", wide: true, body: `<ol class="method-replace-steps"><li><span class="method-step method-step-done">1</span><div><p>New ${labelKind(s.candidate.kind)} saved</p>${hint(esc(s.candidate.label))}</div></li><li><span class="method-step method-step-done">2</span><div><p>It passed its unlock check</p>${hint("Checked a moment ago, in this browser")}</div></li><li><span class="method-step">3</span><div><p>Replace the old ${labelKind(s.replacement.kind)}</p>${hint(`After this, “${esc(s.replacement.label)}” can no longer unlock ${name}. ${s.replacement.kind === "passkey" ? "Delete it from your password manager too." : s.replacement.kind === "recovery" ? "Keep the new kit; the old key stops working on this workspace." : "Keep the new password in your password manager."}`)}${button("activate", `Replace “${esc(s.replacement.label)}”`, "secondary enroll-button-small", undefined, "trash-2")}</div></li></ol>${warning()}` };
      return { title: "Add an unlock method", description: `${s.candidate.kind === "passkey" ? "Passkey" : s.candidate.kind === "password" ? "Password" : "Recovery key"} · just saved`, body: `<div class="method-checks">${checkRow(`${s.candidate.kind === "passkey" ? "Passkey" : "Method"} created and saved`)}${checkRow("It passed its unlock check")}</div><form data-method-form="name" id="method-name" class="enroll-form">${field("Name this method", "name", "text", s.candidate.label, hint("Name it after the object, not the date."))}</form>`, footer: `${button("cancel", "Cancel", "ghost")}<button form="method-name" type="submit" class="enroll-button enroll-button-primary">Save method</button>` };
    }
    case "last-method": return { title: `This is the only everyday way to unlock ${view.name}`, body: `${copy(`“${esc(s.method.label)}” cannot be removed. Without it, the only thing left that opens this workspace is your recovery key — and if that were ever lost, the data would stay encrypted for good.`)}<div class="enroll-card enroll-muted method-last-note">${hint("To remove it")}${copy("Add another passkey or an encryption password, test and enable it, then come back.")}</div>`, footer: `${button("cancel", "Close", "ghost")}${button("add", "Add another method", "primary", undefined, "plus")}` };
    case "remove": return { title: `Remove “${s.method.label}”?`, body: `${copy("This method will stop unlocking the workspace. Browsers authorized by it will be disconnected.")}${warning()}`, footer: `${button("cancel", "Keep method", "ghost")}${button("confirm-remove", "Remove method", "primary")}` };
    case "recovery-save": return { title: "Save your new recovery key", wide: true, body: `${copy("Keep this private and outside the workspace. The old recovery key works until the new one has been checked and replacement is confirmed.")}<div class="enroll-card enroll-key-card">${s.example ? '<span class="enroll-badge enroll-badge-warning">Example key</span>' : ""}<p class="method-recovery-key" aria-label="New recovery key">${esc(s.key)}</p></div><div class="enroll-actions">${button("copy-key", "Copy key")}${button("download-kit", "Download kit")}${button("print-kit", "Print", "ghost")}</div>${hint("The kit holds the key and recovery instructions. It is not a backup of your data.")}`, footer: `${button("cancel", "Cancel", "ghost")}${button("saved-key", "I have saved it — check it works", "primary")}` };
    case "incompatible": return { title: "This passkey cannot unlock here", body: `${copy("Your browser may have saved the passkey, but it did not provide the encryption result needed here. Deck did not enable it. Your existing methods still work.")}${button("add", "Try another method", "primary")}`, footer: button("cancel", "Close", "ghost") };
    case "unknown": return { title: "Check whether the request completed", body: copy("The response did not arrive. The method may have changed. Check the recorded result before making another request."), footer: `${button("open-workspace", "Back to workspace", "ghost")}${button("observe", "Check result", "primary")}` };
    case "loading": return { title: "Loading unlock methods", body: '<span class="enroll-spinner" role="status" aria-label="Loading"></span>' };
    case "recovery-checklist": case "list": throw new Error("The method list is not a dialog.");
  }
}

function recoveryChecklist(view: ManagementView, used: { id: string; revision: number }): string {
  const everyday = view.methods.filter(method => method.status === "active" && method.kind !== "recovery");
  const checked = everyday.find(method => method.checked);
  const usedStillActive = view.methods.some(method => method.id === used.id && method.revision === used.revision && method.status === "active");
  const recovery = view.methods.find(method => method.kind === "recovery" && method.status === "active");
  const row = (symbol: IconName, text: string, action = "", success = false): string => `<div class="method-recovery-row"><span class="${success ? "enroll-success" : "enroll-warning"}">${icon(symbol, 15)}</span><span>${text}</span>${action}</div>`;
  return `<main class="enrollment security-management method-recovery" data-method-management="recovery-checklist"><div class="enroll-content"><img class="enroll-logo" src="/brand/noisefloor-lockup-inverse.svg" alt="Noisefloor"><div class="method-recovery-intro"><p class="enroll-eyebrow enroll-success">${icon("circle-check", 15)}Unlocked with your recovery key</p><h1 class="enroll-title">Set up a way to unlock from now on</h1>${copy(`${esc(view.name)} is open. Add a passkey or an encryption password and let Deck check it, or you will be back here next time.`)}</div><section class="enroll-card enroll-flush" aria-label="Recovery follow-up">${checked ? row("circle-check", `<strong>${esc(checked.label)}</strong> active · checked ${esc(checked.checked!)}`, "", true) : row("triangle-alert", "Add and check an everyday unlock method", button("add", "Add method", "secondary enroll-button-small"))}${row("triangle-alert", `${everyday.length} everyday ${everyday.length === 1 ? "method can" : "methods can"} unlock ${esc(view.name)}`, button("review-methods", "Review them", "secondary enroll-button-small"))}${row(usedStillActive ? "triangle-alert" : "circle-check", usedStillActive ? "Your recovery key has been used and still works" : "The used recovery key no longer unlocks this workspace", usedStillActive && recovery ? button("replace", "Get a new one", "secondary enroll-button-small", recovery.id) : "", !usedStillActive)}</section>${hint("If you typed the key on a machine you do not trust, treat it as spent and replace it. The old key keeps working until the new one is checked and replacement is confirmed.")}<div class="enroll-actions">${button("open-workspace", `Open ${esc(view.name)}`, "primary")}${button("add", "Add another method", "ghost")}</div>${view.error ? `<p role="alert" class="enroll-error">${esc(view.error)}</p>` : ""}</div></main>`;
}

export function managementHtml(view: ManagementView): string {
  if (view.screen.kind === "recovery-checklist") return recoveryChecklist(view, view.screen.usedRecovery);
  const modal = view.screen.kind !== "list";
  const d = modal ? dialog(view) : undefined;
  const messages = `${view.error ? `<p class="enroll-copy-small enroll-error" role="alert">${esc(view.error)}</p>` : ""}${view.notice ? `<p class="enroll-caption" role="status">${esc(view.notice)}</p>` : ""}`;
  return `<main class="security-management" data-method-management="${view.screen.kind}" data-mono="workspace"><header class="method-page-header"${modal ? ' aria-hidden="true"' : ""}>${modal ? "" : `<button class="method-icon-button enroll-mobile" type="button" data-method-action="close" aria-label="Back to workspace">${icon("chevron-left", 18)}</button><span class="enroll-desktop">Settings</span><span class="enroll-desktop method-divider">/</span><span>Unlock methods</span><span class="method-header-close enroll-desktop">${close("close")}</span>`}</header>${d ? `<div class="method-modal"><section class="method-dialog${d.wide ? " method-dialog-wide" : ""}" role="dialog" aria-modal="true" aria-labelledby="method-title"${view.busy ? ' aria-busy="true"' : ""}><header>${view.screen.kind === "last-method" ? `<span class="method-header-warning">${icon("triangle-alert", 16)}</span>` : ""}<div><h1 id="method-title" tabindex="-1">${esc(d.title)}</h1>${d.description ? `<p class="method-description">${d.description}</p>` : ""}</div>${view.screen.kind === "last-method" || view.screen.kind === "password" ? "" : close()}</header><div class="method-dialog-body"${view.busy ? " inert" : ""}>${d.body}${messages}</div>${d.footer ? `<footer${view.busy ? " inert" : ""}>${d.footer}</footer>` : ""}${view.busy ? '<div class="method-busy" role="status"><span class="enroll-spinner"></span><span>Checking the saved method…</span></div>' : ""}</section></div>` : `<div class="method-page-body"><section class="method-list" aria-label="Unlock methods">${list(view)}${messages}</section></div>`}<input type="file" data-method-kit-file accept="application/json,.json" hidden></main>`;
}
