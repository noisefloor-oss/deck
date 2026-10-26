import type { ProjectDirectoryListing } from "../../../api/projects.ts";
import { escapeHtml } from "../sidebar.ts";
import { DIALOG_BODY, DIALOG_FOOT, DIALOG_ICON_BUTTON, GHOST_MD_BUTTON, PRIMARY_MD_BUTTON, SECONDARY_MD_BUTTON } from "../../../ui/controls.ts";
import { lucideIcon } from "../../../ui/icons.ts";
import * as hints from "../../../lib/hints.ts";

export interface DirectoryPickerState {
  open: boolean;
  loading: boolean;
  listing: ProjectDirectoryListing | null;
  error: string;
  fallbackError: string | null;
  newFolderOpen: boolean;
  newFolderName: string;
  // mountRoots is how many roots the mount listing offered. The server
  // reports no parent for a root's top (its parent is outside every allowed
  // root), so the only way back to a second root is the mount listing — and
  // the Up control offers that step only when there is a choice to return to.
  mountRoots: number;
}

// The picker is a navigator, not a list with a selection.
// A row cd's into its directory; the directory being chosen is the one you
// are standing in, named in the mono path line above the list and taken by
// the primary. Nothing is ever selected, so no row wears a pressed wash.
export function directoryPickerHtml(state: DirectoryPickerState): string {
    if (!state.open) return "";
    if (state.loading) {
      return directoryFrame(`<div class="px-2 py-3 text-center text-caption text-text-tertiary">Loading directories…</div>`, "");
    }
    const listing = state.listing;
    if (listing === null) {
      return directoryFrame(`<div class="px-2 py-3 text-caption text-error-on-tint">
        <div>${escapeHtml(state.error || state.fallbackError || "Directories are unavailable")}</div>
        <button type="button" data-directory-roots class="mt-2 h-6 rounded-control-sm bg-sidebar-surface px-2 text-caption" data-hint="${hints.RETURN_TO_ROOT_FOLDER_LIST}">Retry</button>
      </div>`, "");
    }
    // The mount listing (an empty current path) is the set of allowed roots,
    // not a directory: there is nothing to stand in, so nothing to choose or
    // create in, and no path to print. A single root never shows it — the
    // opener descends straight into that root.
    const inDirectory = listing.current_path !== "";
    const canGoUp = listing.parent_path !== null || state.mountRoots > 1;
    const rows = listing.directories.length === 0
      ? `<span class="p-2.5 text-caption text-text-tertiary">No subdirectories.</span>`
      : listing.directories.map((directory) => `
          <button type="button" data-directory-path="${escapeHtml(directory.path)}" data-hint="${hints.directoryPath(escapeHtml(directory.path))}"
            class="menuitem">
            <span aria-hidden="true" class="flex-none">${lucideIcon("folder", 14)}</span>
            <span class="min-w-0 flex-1 truncate">${escapeHtml(directory.name)}</span>
            <span aria-hidden="true" class="flex-none text-text-tertiary">${lucideIcon("chevron-right", 14)}</span>
          </button>`).join("");
    // The inline create row: a pressed menuitem whose input takes the
    // folder name in place, Enter commits. The 16px floor stands until the
    // sm breakpoint so iOS never zooms the dialog to focus it.
    const newFolderRow = !state.newFolderOpen ? "" : `
        <div class="menuitem bg-wash-pressed">
          <span aria-hidden="true" class="flex-none text-accent">${lucideIcon("folder", 14)}</span>
          <input data-directory-new-name value="${escapeHtml(state.newFolderName)}" placeholder="new-directory" aria-label="New directory name" data-hint="${hints.NAME_NEW_FOLDER}"
            autocapitalize="none" autocomplete="off" spellcheck="false"
            class="font-input caret-accent min-w-0 flex-1 border-0 bg-transparent p-0 text-[length:var(--type-input-size)] text-text-primary outline-none placeholder:text-text-tertiary sm:text-body-sm">
          <span class="flex-none text-caption text-text-tertiary">Enter to create</span>
        </div>`;
    // The path line is the only statement of which directory the primary
    // will take, so it prints the path verbatim with its tail elided —
    // never reordered for a left-side truncation.
    const pathRow = !inDirectory ? "" : `
        <div class="flex items-center gap-2">
          <button type="button" data-directory-parent aria-label="Up one directory" ${canGoUp ? "" : "disabled"} data-hint="${hints.GO_UP_ONE_FOLDER}"
            class="dialog-iconbtn">${lucideIcon("arrow-up", 16)}</button>
          <span data-directory-path-line class="min-w-0 flex-1 truncate text-text-primary">${escapeHtml(listing.current_path)}</span>
        </div>`;
    return directoryFrame(`
      ${pathRow}
      <div data-directory-list class="flex h-[210px] flex-col gap-px overflow-y-auto rounded-control border border-border p-1">
        ${state.error === "" ? "" : `<div class="rounded-[6px] bg-error-tint px-2 py-1 text-caption text-error-on-tint">${escapeHtml(state.error)}</div>`}
        ${newFolderRow}
        ${rows}
      </div>
      `, directoryFoot(inDirectory));
}

  // The picker speaks directory: New directory as the ghost-plus action
  // pushed left, Cancel as the secondary every dialog footer uses, Choose
  // this directory as the primary. The h-9 recipes are the New project
  // footer's — one footer system, not a per-dialog size. Both actions need
  // a directory to act in, so only the mount listing disables them.
function directoryFoot(inDirectory: boolean): string {
    return DIALOG_FOOT(`
      <button type="button" data-directory-new-folder ${inDirectory ? "" : "disabled"} data-hint="${hints.ADD_NEW_FOLDER}"
        class="${GHOST_MD_BUTTON}"><span aria-hidden="true" class="flex-none">${lucideIcon("plus", 16)}</span>New directory</button>
      <span class="flex-1"></span>
      <button type="button" data-directory-cancel data-hint="${hints.CANCEL_CHOOSING_FOLDER}"
        class="${SECONDARY_MD_BUTTON}">Cancel</button>
      <button type="button" data-directory-select ${inDirectory ? "" : "disabled"} data-hint="${hints.SELECT_THIS_FOLDER}"
        class="${PRIMARY_MD_BUTTON}">Choose this directory</button>
    `);
}

// The picker is a screen inside Add project: Back returns to the form, the
// close X closes the whole dialog.
function directoryFrame(body: string, foot: string): string {
    return `
        <div data-directory-dialog role="dialog" aria-modal="true" aria-label="Choose a directory" class="dialog">
          <header class="dialog-head">
            <button type="button" data-directory-cancel aria-label="Back" data-hint="${hints.GO_BACK}" class="${DIALOG_ICON_BUTTON}">${lucideIcon("chevron-left", 16)}</button>
            <h2 class="dialog-title dialog-title-prompt">Choose a directory</h2>
            <button type="button" data-project-cancel data-dialog-close aria-label="Close" data-hint="${hints.CLOSE_THIS_DIALOG}" class="ml-auto ${DIALOG_ICON_BUTTON}">${lucideIcon("x", 16)}</button>
          </header>
          ${DIALOG_BODY(body)}
          ${foot}
        </div>
    `;
}
