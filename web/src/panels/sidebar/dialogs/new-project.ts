import type { GitIdentityProfile } from "../../../api/projects.ts";
import { escapeHtml } from "../sidebar.ts";
import { DIALOG_BODY, DIALOG_FOOT, dialogHead, SELECT_FIELD, selectShellHtml } from "../../../ui/controls.ts";
import { lucideIcon } from "../../../ui/icons.ts";
import * as hints from "../../../lib/hints.ts";

export interface NewProjectModalState {
  path: string;
  profiles: readonly GitIdentityProfile[];
  selected: GitIdentityProfile | null;
  directoryPickerHtml: string;
}

export function newProjectModalHtml(state: NewProjectModalState): string {
    const { profiles, selected } = state;
    return `
        <form data-project-form data-phone-fullscreen tabindex="-1" role="dialog" aria-modal="true" aria-label="Open project" class="dialog outline-none">
          ${dialogHead(
            `<span data-copy-wide>New project</span><span data-copy-phone class="hidden">Open project</span>`,
            { label: "Close", attr: "data-project-cancel" },
          )}
          <p data-project-caption class="hidden truncate border-b border-border px-5 pb-2.5 pt-0.5 text-caption text-text-tertiary">Open project adds this folder to your sidebar</p>
          ${DIALOG_BODY(`
            <label class="block">
              <span class="font-label text-(length:--type-label-size) text-text-secondary">Workspace path</span>
              <div class="relative mt-1">
                <input data-project-path required readonly value="${escapeHtml(state.path)}"
                  placeholder="/workspace/my-project" aria-label="Workspace path" data-hint="${hints.SHOW_WORKSPACE_PATH}"
                  class="font-input h-9 w-full rounded-control border border-border-strong bg-surface-card pl-3 pr-10 text-[length:var(--type-input-size)] text-text-primary placeholder:text-text-tertiary sm:text-body-sm">
                <button type="button" data-project-browse aria-label="Choose folder" data-hint="${hints.CHOOSE_WORKSPACE_FOLDER}"
                  class="absolute right-[5px] top-1/2 grid h-7 w-7 -translate-y-1/2 place-items-center rounded-control-sm text-text-secondary transition-colors hover:bg-wash-hover hover:text-text-primary">${lucideIcon("folder", 16)}</button>
              </div>
              <p data-project-path-hint class="mt-1.5 text-caption text-text-tertiary">Created if it doesn't exist yet.</p>
            </label>
            <label class="block">
              <span class="font-label text-(length:--type-label-size) text-text-secondary"><span data-copy-wide>Git email</span><span data-copy-phone class="hidden">Git identity</span></span>
              ${selectShellHtml(`<select data-project-git-profile aria-label="Git identity" data-hint="${hints.CHOOSE_GIT_IDENTITY_FOR_PROJECT}"
                class="${SELECT_FIELD} font-input h-9 w-full rounded-control border border-border-strong bg-surface-card pl-2 text-body-sm text-text-primary">
                ${profiles.map((profile) => `<option value="${escapeHtml(profile.id)}" ${profile.id === selected?.id ? "selected" : ""}>${escapeHtml(profile.name)} — ${escapeHtml(profile.email)}</option>`).join("")}
              </select>`, "mt-1")}
              <p data-project-identity-hint class="mt-1.5 text-caption text-text-tertiary">Applies to this project only.</p>
            </label>
          `)}
          ${DIALOG_FOOT(`
            <button type="button" data-project-cancel data-hint="${hints.CANCEL_OPENING_PROJECT}"
              class="h-9 rounded-control border border-border bg-surface-card px-3 text-body-sm text-text-primary transition-colors hover:bg-wash-hover pointer-coarse:min-h-11">Cancel</button>
            <button type="submit" data-project-submit ${state.path === "" || selected === null ? "disabled" : ""} data-hint="${hints.OPEN_THIS_PROJECT}"
              class="h-9 rounded-control bg-accent px-4 text-body-sm font-medium text-text-on-accent transition-colors hover:bg-accent-hover disabled:opacity-45 pointer-coarse:min-h-11"><span data-copy-wide>Add project</span><span data-copy-phone class="hidden">Open project</span></button>
          `)}
        </form>
        ${state.directoryPickerHtml}
    `;
}
