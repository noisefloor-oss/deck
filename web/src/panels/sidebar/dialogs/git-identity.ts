import type { GitIdentityCatalog, GitIdentityProfile } from "../../../api/projects.ts";
import { escapeHtml } from "../sidebar.ts";
import { DIALOG_BODY, DIALOG_FOOT, dialogHead, PRIMARY_MD_BUTTON, SECONDARY_MD_BUTTON, SELECT_FIELD, selectShellHtml } from "../../../ui/controls.ts";
import { githubMark, lucideIcon } from "../../../ui/icons.ts";
import * as hints from "../../../lib/hints.ts";

export interface GitIdentitySettingsState {
  catalog: GitIdentityCatalog;
  defaultProfile: GitIdentityProfile | null;
  editor: "new" | string | null;
  deleteID: string | null;
  busy: boolean;
  error: string | null;
}

export interface GitIdentityFormState {
  busy: boolean;
  error: string | null;
  name: string;
  email: string;
  // The connected GitHub account's identity, or null when none is known.
  github: { name: string; email: string } | null;
  // The connected account's login; the offer to connect gives way to it.
  githubLogin?: string;
  // While the connection is read the line offers nothing, so a connected
  // account is never offered again.
  githubChecking?: boolean;
  desc: string;
  // A sentence about what waits on this identity, or null.
  note: string | null;
  // Within a flow the footer steps back to the screen that asked.
  back: boolean;
}

// filledFromGitHub says whether both fields still hold the GitHub account's
// values, so the form may say where they came from.
export function filledFromGitHub(draft: { name: string; email: string }, github: { name: string; email: string } | null): boolean {
  return github !== null && draft.name === github.name && draft.email === github.email;
}

// The account-level Git identity dialog: one form that
// creates your first identity, from the settings row or from a flow that
// needs one.
export function gitIdentityOnboardingHtml(state: GitIdentityFormState): string {
    return `
        <form data-git-identity-onboarding role="dialog" aria-modal="true" aria-label="Git identity" class="dialog">
          ${dialogHead("Git identity", { label: "Close", attr: "data-git-identity-onboarding-close", disabled: state.busy }, escapeHtml(state.desc))}
          ${DIALOG_BODY(`
            ${state.note === null ? "" : `<p data-git-identity-note class="text-body-sm text-text-primary">${escapeHtml(state.note)}</p>`}
            <div data-github-line class="contents">${githubLineHtml(state)}</div>
            ${gitIdentityFields("onboarding", { name: state.name, email: state.email })}
            <p data-git-identity-from-github ${filledFromGitHub(state, state.github) ? "" : "hidden"} class="text-caption text-text-secondary">Filled in from your GitHub account. You can edit it before you save.</p>
            ${state.error ? `<div class="flex gap-2.5 rounded-card border border-border bg-error-tint px-[13px] py-[11px]">
              <span aria-hidden="true" class="mt-px flex-none text-error-on-tint">${lucideIcon("triangle-alert", 15)}</span>
              <span class="text-body-sm leading-[1.5] text-text-primary">${escapeHtml(state.error)}</span>
            </div>` : ""}
          `)}
          ${DIALOG_FOOT(`
            ${state.back ? `<button type="button" data-git-identity-onboarding-back ${state.busy ? "disabled" : ""} data-hint="${hints.GO_BACK}" class="${SECONDARY_MD_BUTTON}">Back</button>` : ""}
            <button type="submit" data-git-identity-save ${state.busy ? "disabled" : ""} data-hint="${hints.SAVE_GIT_IDENTITY}"
              class="${PRIMARY_MD_BUTTON}">Save identity</button>
          `)}
        </form>`;
}

// githubLineHtml is the form's GitHub line: the connected account, or the
// offer to connect one.
export function githubLineHtml(state: GitIdentityFormState): string {
    if (state.githubLogin !== undefined) return githubConnectedHtml(state.githubLogin);
    if (state.github !== null || state.githubChecking === true) return "";
    return `<p data-github-connect-prompt class="text-body-sm text-text-secondary">Connect GitHub to fill these in from your account.</p>
            <button type="button" data-github-connect ${state.busy ? "disabled" : ""} data-hint="${hints.CONNECT_GITHUB_FOR_IDENTITY}" class="${SECONDARY_MD_BUTTON} inline-flex items-center justify-center gap-2">${githubMark(16)}Connect GitHub</button>`;
}

export function githubConnectedHtml(login: string): string {
    return `<p data-github-connected role="status" class="flex items-center gap-2 text-body-sm text-text-primary"><span aria-hidden="true" class="flex-none">${githubMark(16)}</span><span>${login === "" ? "Connected to GitHub"
      : `Connected to GitHub as <span data-github-login class="font-medium">${escapeHtml(login)}</span>`}</span></p>`;
}

// The two identity fields on the DS input recipe, in a grid that collapses
// by container width: the dialog is a fixed width, so a viewport breakpoint
// would answer for the wrong box. The grid carries no outer margin — each
// caller owns the space above it, because the onboarding body sits flush
// under its head while the manager's editor stands off its heading.
export function gitIdentityFields(prefix: string, profile?: { name: string; email: string }): string {
    return `<div class="grid gap-4 [grid-template-columns:repeat(auto-fit,minmax(184px,1fr))]">
      <label class="flex min-w-0 flex-col gap-1.5">
        <span class="font-label text-(length:--type-label-size) text-text-secondary">Author name</span>
        <input data-${prefix}-git-name required value="${escapeHtml(profile?.name ?? "")}" aria-label="Git author name" autocomplete="name" data-hint="${hints.ENTER_AUTHOR_NAME}"
          class="font-input caret-accent h-9 w-full rounded-control border border-border-strong bg-surface-card px-(--field-px) text-[length:var(--type-input-size)] text-text-primary sm:text-body-sm">
      </label>
      <label class="flex min-w-0 flex-col gap-1.5">
        <span class="font-label text-(length:--type-label-size) text-text-secondary">Author email</span>
        <input data-${prefix}-git-email required type="email" value="${escapeHtml(profile?.email ?? "")}" aria-label="Git author email" autocomplete="email" data-hint="${hints.ENTER_AUTHOR_EMAIL}"
          class="font-input caret-accent h-9 w-full rounded-control border border-border-strong bg-surface-card px-(--field-px) text-[length:var(--type-input-size)] text-text-primary sm:text-body-sm">
      </label>
    </div>`;
}

export function gitIdentitySettingsHtml(state: GitIdentitySettingsState): string {
    const catalog = state.catalog;
    const editing = state.editor === "new"
      ? undefined
      : catalog.profiles.find((profile) => profile.id === state.editor);
    const deleting = catalog.profiles.find((profile) => profile.id === state.deleteID);
    const replacements = deleting === undefined
      ? []
      : catalog.profiles.filter((profile) => profile.id !== deleting.id);
    return `
        <div data-git-identity-settings role="dialog" aria-modal="true" aria-label="Git identity" class="dialog">
          <header class="dialog-head">
            <div class="min-w-0 flex-1">
              <h2 class="dialog-title dialog-title-prompt">Git identity</h2>
              <p class="dialog-desc">Applies to projects you open next.</p>
            </div>
            <button type="button" data-git-identities-close data-dialog-close aria-label="Close" ${state.busy ? "disabled" : ""} data-hint="${hints.CLOSE_GIT_IDENTITY_SETTINGS}"
              class="ml-auto grid h-7 w-7 flex-none place-items-center rounded-control-sm text-text-tertiary hover:bg-wash-hover hover:text-text-primary pointer-coarse:h-10 pointer-coarse:w-10">${lucideIcon("x", 16)}</button>
          </header>
          ${DIALOG_BODY(`
            ${state.error ? `<div class="rounded-control bg-error-tint px-3 py-2 text-caption text-error-on-tint">${escapeHtml(state.error)}</div>` : ""}
            <label class="block">
              <span class="font-label text-(length:--type-label-size) text-text-secondary">Name</span>
              <input data-git-identity-name value="${escapeHtml(state.defaultProfile?.name ?? "")}" aria-label="Git author name"
                autocapitalize="none" autocomplete="off" spellcheck="false" data-hint="${hints.SAVE_GIT_IDENTITY}"
                class="input mt-1 h-10 w-full rounded-control border border-border-strong bg-surface-card px-3.5 text-body-sm text-text-primary">
            </label>
            <div>
              <span class="font-label text-(length:--type-label-size) text-text-secondary">Emails</span>
              <ul class="mt-1 flex flex-col gap-2" aria-label="Saved git identities">
                ${catalog.profiles.map((profile) => `<li class="flex h-[52px] items-center gap-3 rounded-control border border-field-border bg-sidebar-surface px-3.5">
                  <span class="min-w-0 flex items-center gap-2.5">
                    <span class="truncate font-mono text-body-sm text-text-primary">${escapeHtml(profile.email)}</span>
                    ${catalog.default_profile_id === profile.id ? `<span class="flex-none rounded-pill bg-wash-selected px-2 py-0.5 font-label text-(length:--type-label-sm-size) text-accent">Default</span>` : ""}
                    ${profile.name === (state.defaultProfile?.name ?? profile.name) ? "" : `<span class="flex-none truncate text-caption text-text-tertiary">${escapeHtml(profile.name)}</span>`}
                  </span>
                  <span class="ml-auto flex flex-none items-center gap-1.5">
                    ${catalog.default_profile_id === profile.id ? "" : `<button type="button" data-git-identity-default="${escapeHtml(profile.id)}" data-hint="${hints.USE_EMAIL_ON_NEW_PROJECTS}" class="h-7 rounded-control-sm px-2 text-body-sm text-text-secondary hover:bg-field">Make default</button>`}
                    <button type="button" data-git-identity-delete="${escapeHtml(profile.id)}" data-hint="${hints.REMOVE_EMAIL_FROM_LIST}" class="h-7 rounded-control-sm px-2 text-body-sm text-text-secondary hover:bg-field hover:text-text-primary">Remove</button>
                  </span>
                </li>`).join("")}
              </ul>
            </div>
            ${state.editor === null ? `<button type="button" data-git-identity-add data-hint="${hints.ADD_ANOTHER_EMAIL}" class="h-9 self-start rounded-control border border-border-strong bg-surface-card px-3 text-body-sm font-medium text-text-primary hover:bg-wash-hover">Add email</button>` : `
              <form data-git-identity-editor class="rounded-control border border-field-border bg-sidebar-surface p-3">
                <h3 class="text-body-sm font-medium text-text-primary">${editing === undefined ? "Add email" : "Edit identity"}</h3>
                <div class="mt-4">${gitIdentityFields("editor", editing)}</div>
                <div class="mt-3 flex justify-end gap-2">
                  <button type="button" data-git-identity-editor-cancel data-hint="${hints.CANCEL_EDITING_IDENTITY}" class="h-8 rounded-[6px] px-3 text-caption text-text-secondary hover:bg-field">Cancel</button>
                  <button type="submit" data-hint="${hints.SAVE_EDITED_IDENTITY}" class="h-8 rounded-[6px] bg-accent px-3 text-caption font-medium text-text-on-accent hover:bg-accent-hover">Save</button>
                </div>
              </form>`}
            ${deleting === undefined ? "" : `<div class="mt-3 rounded-control border border-error bg-error-tint p-3">
              <h3 class="text-body-sm font-medium text-error-on-tint">Remove ${escapeHtml(deleting.name)}?</h3>
              <p class="mt-1 text-caption text-error-on-tint">Existing projects keep their saved snapshot. New projects choose from the remaining identities.</p>
              ${catalog.default_profile_id === deleting.id && replacements.length > 0 ? `<label class="mt-3 block text-caption text-error-on-tint">New default
                ${selectShellHtml(`<select data-git-identity-replacement data-hint="${hints.CHOOSE_NEW_DEFAULT_IDENTITY}" class="${SELECT_FIELD} font-input h-9 w-full rounded-[6px] border border-field-border bg-sidebar-surface pl-2 text-body-sm text-text-primary">
                  ${replacements.map((profile) => `<option value="${escapeHtml(profile.id)}">${escapeHtml(profile.name)} — ${escapeHtml(profile.email)}</option>`).join("")}
                </select>`, "mt-1")}</label>` : ""}
              <div class="mt-3 flex justify-end gap-2">
                <button type="button" data-git-identity-delete-cancel data-hint="${hints.KEEP_THIS_IDENTITY}" class="h-8 rounded-[6px] px-3 text-caption text-text-secondary hover:bg-field">Cancel</button>
                <button type="button" data-git-identity-delete-confirm data-hint="${hints.REMOVE_THIS_IDENTITY}" class="h-8 rounded-[6px] bg-error px-3 text-caption font-medium text-text-on-error hover:opacity-90">Remove identity</button>
              </div>
            </div>`}
          `)}
        </div>`;
}
