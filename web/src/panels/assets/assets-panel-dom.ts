// The Assets panel's DOM shell: one section per agent location in the
// server's order, rows you can pick for deleting, the delete
// consent and its outcome. The shell draws what the model folded and owns
// only which dialog is up. A row's ownership fact is a visible line, never a
// hover: a lock replaces the checkbox, and the reason sits under the name.
// Only paths are mono; counts, sizes and ages speak in the interface voice.
import { escapeHtml } from "../../lib/escape.ts";
import { formatFileSize, formatSince } from "../../lib/format.ts";
import * as hints from "../../lib/hints.ts";
import { GHOST_SM_BUTTON, ICON_BUTTON, SECONDARY_MD_BUTTON, PRIMARY_MD_BUTTON, MD_BUTTON_TYPE, SECONDARY_SM_TYPE } from "../../ui/controls.ts";
import { activateDialog } from "../../ui/dialog-focus.ts";
import { openModalScreen, returnFocus, type ModalScreen } from "../../ui/modal-host.ts";
import { lucideIcon } from "../../ui/icons.ts";
import type { AssetEntry, AssetLocation } from "../../api/assets.ts";
import {
  AssetsPanelModel,
  OUTCOME_WORD,
  SCAN_BOUND,
  deleteAction,
  deleteTitle,
  filesNoun,
  isDeletable,
  isImageAsset,
  outcomeDescription,
  outcomeFact,
  outcomeTitle,
  totalSize,
  truncatedNote,
  type AssetOutcome,
} from "./assets-panel.ts";

export interface AssetsPanelShellCallbacks {
  onClose?: () => void;
  onOpen?: (entry: AssetEntry) => void;
  // The dialogs mount above the whole workspace: a scrim inside the panel
  // would cover only the panel.
  modals: () => HTMLElement | null;
  // imageUrl names the inline image route for a thumbnail. Without it an
  // image row shows the file glyph like any other file.
  imageUrl?: (path: string) => string;
}

const MONO_CAPTION = "font-mono text-[min(var(--type-mono-size),calc(var(--type-caption-size)*var(--type-mono-xh-ratio)))]";
const MONO_BODY_SM = "font-mono text-[min(var(--type-mono-size),calc(var(--type-body-sm-size)*var(--type-mono-xh-ratio)))]";

const DANGER_SM_BUTTON = `flex h-7 flex-none items-center gap-1 rounded-control-sm border border-transparent bg-error px-2.5 ${SECONDARY_SM_TYPE} text-text-on-error transition-colors hover:opacity-90 disabled:cursor-not-allowed disabled:opacity-45`;
const DANGER_MD_BUTTON = `h-9 rounded-control border border-transparent bg-error px-(--control-px) ${MD_BUTTON_TYPE} text-text-on-error transition-colors hover:opacity-90 disabled:opacity-45 pointer-coarse:min-h-11`;

const OUTCOME_TONE: Record<AssetOutcome["outcome"], { word: string; path: string }> = {
  deleted: { word: "text-text-secondary", path: "text-text-tertiary" },
  skipped: { word: "text-text-primary", path: "text-text-primary" },
  unconfirmed: { word: "text-warning-on-tint", path: "text-text-primary" },
};

type Dialog = "confirm" | "outcome" | null;

export class AssetsPanel {
  private container: HTMLElement;
  private model: AssetsPanelModel;
  private cb: AssetsPanelShellCallbacks;
  private dialog: Dialog = null;
  private dialogScreen: ModalScreen | null = null;
  private dismissDialog: () => void = () => {};
  private releaseDialog: (() => void) | null = null;
  private disposed = false;
  // Thumbnails the image route refused, by path and version, so a repaint
  // keeps the glyph instead of asking again for a file that will not decode.
  private refused = new Set<string>();

  constructor(container: HTMLElement, model: AssetsPanelModel, cb: AssetsPanelShellCallbacks) {
    this.container = container;
    this.model = model;
    this.cb = cb;
    this.render();
  }

  async open(): Promise<void> {
    if (this.disposed) return;
    this.render();
    await this.model.open();
  }

  close(): void {
    this.model.close();
    this.closeDialog(false);
  }

  dispose(): void {
    this.disposed = true;
    this.closeDialog(false);
    this.model.dispose();
    this.container.innerHTML = "";
  }

  render(): void {
    if (this.disposed) return;
    const list = this.container.querySelector<HTMLElement>("[data-assets-list]");
    const scrollTop = list?.scrollTop ?? 0;
    const focused = document.activeElement instanceof HTMLElement && this.container.contains(document.activeElement)
      ? focusKey(document.activeElement)
      : null;
    this.container.innerHTML = `
      <div data-assets class="flex min-h-0 flex-1 flex-col">
        ${this.headerHtml()}
        ${this.bodyHtml()}
      </div>
    `;
    const next = this.container.querySelector<HTMLElement>("[data-assets-list]");
    if (next !== null) next.scrollTop = scrollTop;
    if (focused !== null) this.container.querySelector<HTMLElement>(focused)?.focus({ preventScroll: true });
    this.bind();
    if (this.dialog === "outcome" && this.model.lastDelete === null) this.closeDialog(false);
  }

  private bind(): void {
    this.container.querySelector("[data-assets-close]")?.addEventListener("click", () => this.cb.onClose?.());
    this.container.querySelector("[data-assets-rescan]")?.addEventListener("click", () => void this.model.refresh());
    this.container.querySelector("[data-assets-clear]")?.addEventListener("click", () => this.model.clearSelection());
    this.container.querySelector("[data-assets-delete]")?.addEventListener("click", () => this.openConfirm());
    // A file that will not decode, or is gone by the time the image route
    // reads it, keeps the plain file glyph rather than a blank tile.
    for (const img of this.container.querySelectorAll<HTMLImageElement>("img[data-asset-img]")) {
      img.addEventListener("error", () => {
        this.refused.add(img.dataset.assetImg ?? "");
        img.replaceWith(glyph("file-text"));
      }, { once: true });
    }
    for (const select of this.container.querySelectorAll<HTMLElement>("[data-asset-select]")) {
      select.addEventListener("click", () => this.model.toggle(select.dataset.assetSelect ?? ""));
    }
    for (const open of this.container.querySelectorAll<HTMLElement>("[data-asset-open]")) {
      open.addEventListener("click", () => {
        const entry = this.model.entries().find((candidate) => candidate.path === open.dataset.assetOpen);
        if (entry !== undefined) this.cb.onOpen?.(entry);
      });
    }
  }

  private headerHtml(): string {
    return `
      <div data-assets-header class="chrome gap-2.5 pl-(--space-5) pr-(--space-4)">
        <h2 class="chrome-title flex-1">Assets</h2>
        <button type="button" data-assets-rescan aria-label="Rescan" data-hint="${hints.RESCAN_ASSETS}"
          class="${ICON_BUTTON}">${lucideIcon("refresh-cw", 14)}</button>
        <button type="button" data-assets-close aria-label="Close assets" data-hint="${hints.CLOSE_ASSETS}"
          class="${ICON_BUTTON}">${lucideIcon("x", 14)}</button>
      </div>
    `;
  }

  private bodyHtml(): string {
    const m = this.model;
    if (m.state === "loading") {
      return `
        <div data-assets-loading class="flex flex-1 items-center justify-center">
          <span role="status" class="inline-flex items-center gap-(--space-2) text-body-sm text-text-tertiary">
            <span aria-hidden="true" class="block h-4 w-4 flex-none animate-spinner rounded-circle border-2 border-accent-tint border-t-accent"></span>Scanning agent locations
          </span>
        </div>`;
    }
    if (m.state === "unavailable") {
      return `<div data-assets-unavailable role="alert" class="p-5 text-body-sm text-text-tertiary">The agent locations could not be scanned: ${escapeHtml(m.error ?? "")}</div>`;
    }
    const note = m.truncated.length > 0
      ? `<div data-assets-truncated role="status" class="flex flex-none items-start gap-2 border-b border-border bg-surface-sunken px-(--space-5) py-[9px] text-caption text-text-secondary">
          <span aria-hidden="true" class="mt-0.5 flex flex-none text-text-tertiary">${lucideIcon("info", 13)}</span>
          <span class="text-pretty">${escapeHtml(truncatedNote(m.truncated))}</span>
        </div>`
      : "";
    if (m.empty) {
      return `${note}
        <div data-assets-empty class="flex flex-1 items-center justify-center">
          <div class="flex flex-col items-center gap-(--space-3) px-(--space-6) py-(--space-10) text-center">
            <span aria-hidden="true" class="text-text-tertiary">${lucideIcon("eye", 24)}</span>
            <h3 class="text-title-3 font-title-caps text-text-primary">No agent files</h3>
            <p class="max-w-[44ch] text-body-sm text-text-tertiary">Files agents write to the assets folder, build scratch, visual-diff captures, run receipts and session scratch are listed here.</p>
          </div>
        </div>`;
    }
    const selected = m.selectedCount > 0;
    return `${note}
      <div data-assets-list class="flex min-h-0 flex-1 flex-col overflow-y-auto px-(--space-3) pt-(--space-1) pb-(--space-3)">
        ${m.locations.filter((location) => location.entries.length > 0).map((location) => this.locationHtml(location)).join("")}
      </div>
      <div data-assets-footer class="flex h-12 flex-none items-center gap-(--space-2) border-t border-border pr-(--space-4) pl-(--space-5)">
        <span data-assets-summary role="status" class="min-w-0 truncate text-body-sm ${selected ? "text-text-primary" : "text-text-tertiary"}">${escapeHtml(m.summary)}</span>
        <span class="ml-auto"></span>
        ${selected ? `<button type="button" data-assets-clear data-hint="${hints.CLEAR_ASSET_SELECTION}" class="${GHOST_SM_BUTTON}">Clear</button>` : ""}
        <button type="button" data-assets-delete data-hint="${hints.DELETE_ASSETS}" ${selected && !m.deleting ? "" : "disabled"}
          class="${DANGER_SM_BUTTON}">${lucideIcon("trash-2", 14)}<span>Delete…</span></button>
      </div>`;
  }

  private locationHtml(location: AssetLocation): string {
    const note = location.truncated ? `${SCAN_BOUND}+ entries` : "";
    return `
      <section data-assets-location="${escapeHtml(location.id)}" role="group" aria-label="${escapeHtml(location.name)}" class="flex flex-col gap-px">
        <div data-assets-location-head class="flex items-baseline gap-2 px-2.5 pt-1.5 pb-0.5 leading-[1.3]">
          <span class="flex-none text-body-sm leading-[1.3] font-medium text-text-secondary">${escapeHtml(location.name)}</span>
          <span class="min-w-0 truncate ${MONO_CAPTION} leading-[1.3] text-text-tertiary">${escapeHtml(location.path)}</span>
          ${note === "" ? "" : `<span data-assets-location-note class="ml-auto flex-none text-caption leading-[1.3] text-text-tertiary">${note}</span>`}
        </div>
        ${location.entries.map((entry) => this.rowHtml(entry)).join("")}
      </section>`;
  }

  private rowHtml(entry: AssetEntry): string {
    const m = this.model;
    const deletable = isDeletable(entry);
    const checked = m.isSelected(entry.path);
    const size = entry.kind === "dir" ? "Folder" : formatFileSize(entry.size);
    const age = formatSince(entry.mtime, Date.now());
    const meta = age === null ? size : `${size} · ${age}`;
    const outcome = m.outcomeFor(entry.path);
    const fact = outcome !== undefined ? outcomeFact(outcome) : entry.locked;
    const factTone = outcome === undefined
      ? "text-text-tertiary"
      : outcome.outcome === "unconfirmed" ? "text-warning-on-tint" : "text-text-secondary";
    const label = [entry.name, size, fact].filter((part) => part !== null && part !== "").join(", ");
    const check = `<span aria-hidden="true" data-asset-check="${checked}" class="asset-check">${lucideIcon("check", 12)}</span>`;
    const slot = deletable
      ? `<button type="button" data-asset-select="${escapeHtml(entry.path)}" role="checkbox" aria-checked="${checked}"
          aria-label="Select ${escapeHtml(entry.name)} for deletion" data-hint="${hints.SELECT_ASSET}"
          class="flex h-[30px] w-[18px] flex-none items-center justify-center">${check}</button>`
      : `<span data-asset-slot class="flex h-[30px] w-[18px] flex-none items-center justify-center text-text-tertiary">${lucideIcon("lock", 13)}</span>`;
    return `
      <div data-asset-path="${escapeHtml(entry.path)}" data-selected="${checked}"
        class="asset-row flex items-start gap-2.5 rounded-control px-2.5 py-1">
        ${slot}
        <button type="button" data-asset-open="${escapeHtml(entry.path)}" aria-label="${escapeHtml(label)}"
          data-hint="${entry.kind === "dir" ? hints.OPEN_THIS_FOLDER : hints.OPEN_THIS_FILE}"
          class="flex min-w-0 flex-1 cursor-pointer items-start gap-2.5 rounded-control-sm text-left">
          ${this.thumbHtml(entry)}
          <span class="flex min-w-0 flex-1 flex-col leading-[1.3]">
            <span data-asset-name class="truncate text-body-sm leading-[1.3] text-text-primary">${escapeHtml(entry.name)}</span>
            <span data-asset-meta class="whitespace-nowrap text-caption leading-[1.3] text-text-tertiary">${escapeHtml(meta)}</span>
            ${fact === null || fact === "" ? "" : `<span data-asset-fact class="truncate text-caption leading-[1.3] ${factTone}">${escapeHtml(fact)}</span>`}
          </span>
        </button>
      </div>`;
  }

  private thumbHtml(entry: AssetEntry): string {
    const maxBytes = this.model.listing?.preview_max_bytes ?? 0;
    const key = `${entry.path}@${entry.version}`;
    const url = this.cb.imageUrl !== undefined && isImageAsset(entry, maxBytes) && !this.refused.has(key) ? this.cb.imageUrl(entry.path) : null;
    const inner = url !== null
      ? `<img data-asset-img="${escapeHtml(key)}" src="${escapeHtml(url)}" alt="" loading="lazy" decoding="async" class="h-full w-full object-cover">`
      : lucideIcon(entry.kind === "dir" ? "folder" : "file-text", 14);
    return `<span data-asset-thumb class="relative flex h-[30px] w-10 flex-none items-center justify-center overflow-hidden rounded-control-sm bg-surface-sunken text-text-tertiary outline outline-1 outline-border">${inner}</span>`;
  }

  // openConfirm puts the consent up for the current selection.
  openConfirm(): void {
    if (this.model.selectedCount === 0 || this.model.deleting) return;
    this.showDialog("confirm");
  }

  // openOutcome shows the last delete's answer, if there is one.
  openOutcome(): void {
    if (this.model.lastDelete === null) return;
    this.showDialog("outcome");
  }

  private showDialog(kind: Exclude<Dialog, null>): void {
    const modals = this.cb.modals();
    if (modals === null) return;
    this.closeDialog(false);
    this.dialog = kind;
    this.dialogScreen = openModalScreen(modals, { dismiss: () => this.dismissDialog(), dismissOnScrim: false });
    const host = this.dialogScreen.element;
    host.innerHTML = kind === "confirm" ? this.confirmHtml() : this.outcomeHtml();
    const dialog = host.querySelector<HTMLElement>("[data-assets-dialog]")!;
    if (kind === "confirm") {
      host.querySelector("[data-assets-cancel]")?.addEventListener("click", () => this.closeDialog(true));
      host.querySelector("[data-assets-dialog-close]")?.addEventListener("click", () => this.closeDialog(true));
      host.querySelector("[data-assets-confirm]")?.addEventListener("click", () => void this.confirm());
      this.dismissDialog = () => this.closeDialog(true);
      this.releaseDialog = activateDialog(dialog, this.dismissDialog, "[data-assets-cancel]");
    } else {
      const done = (): void => {
        this.closeDialog(true);
        this.model.dismissOutcome();
      };
      host.querySelector("[data-assets-done]")?.addEventListener("click", done);
      host.querySelector("[data-assets-dialog-close]")?.addEventListener("click", done);
      host.querySelector("[data-assets-outcome-rescan]")?.addEventListener("click", () => {
        done();
        void this.model.refresh();
      });
      this.dismissDialog = done;
      this.releaseDialog = activateDialog(dialog, done, "[data-assets-done]");
    }
  }

  private async confirm(): Promise<void> {
    const dialog = this.dialogScreen?.element.querySelector<HTMLElement>("[data-assets-dialog]");
    if (dialog === null || dialog === undefined) return;
    // While the delete is in flight the consent's controls are disabled, so
    // focus moves to the dialog itself to keep Tab trapped inside it. Escape
    // is ignored until the answer lands: closing would not stop a request
    // the workspace may already be acting on.
    this.releaseDialog?.();
    for (const button of dialog.querySelectorAll<HTMLButtonElement>("button")) button.disabled = true;
    dialog.setAttribute("aria-busy", "true");
    this.dismissDialog = () => {};
    this.releaseDialog = activateDialog(dialog, this.dismissDialog, ":dialog");
    await this.model.delete();
    if (this.disposed) return;
    this.closeDialog(false);
    this.openOutcome();
  }

  private closeDialog(restoreFocus: boolean): void {
    this.releaseDialog?.();
    this.releaseDialog = null;
    this.dismissDialog = () => {};
    this.dialogScreen?.close();
    this.dialogScreen = null;
    this.dialog = null;
    if (restoreFocus) {
      returnFocus(this.container.querySelector<HTMLButtonElement>("[data-assets-delete]:not([disabled])")
        ?? this.container.querySelector<HTMLElement>("[data-assets-rescan]"));
    }
  }

  private phoneClose(): string {
    return `<button type="button" data-assets-dialog-close data-dialog-close aria-label="Close" data-hint="${hints.CLOSE_THIS_DIALOG}"
      class="assets-sheet-close grid h-11 w-11 flex-none place-items-center rounded-control-sm text-text-secondary transition-colors hover:bg-wash-hover">${lucideIcon("x", 16)}</button>`;
  }

  private confirmHtml(): string {
    const picked = this.model.selectedEntries();
    const n = picked.length;
    const rows = picked.map((entry) => `
      <div data-assets-target="${escapeHtml(entry.path)}" class="assets-chip-row flex items-start gap-2.5 border-b border-border px-[11px] py-[7px]">
        <span class="flex min-w-0 flex-1 flex-col gap-0.5">
          <span class="truncate ${MONO_BODY_SM} text-text-primary">${escapeHtml(entry.path)}</span>
        </span>
        <span class="flex-none pt-0.5 text-caption leading-[1.55] text-text-tertiary">${formatFileSize(entry.size)}</span>
      </div>`).join("");
    return `
        <div data-assets-dialog="confirm" role="alertdialog" aria-modal="true" aria-labelledby="assets-del-title" aria-describedby="assets-del-desc" class="dialog">
          <header class="dialog-head">
            <span aria-hidden="true" class="mt-[3px] flex-none text-error-on-tint">${lucideIcon("triangle-alert", 17)}</span>
            <div class="min-w-0 flex-1">
              <h2 id="assets-del-title" class="dialog-title dialog-title-prompt">${deleteTitle(n)}</h2>
              <p id="assets-del-desc" class="dialog-desc">They're removed from disk on the workspace. This can't be undone. A file that changes before Deck reaches it is skipped, not deleted.</p>
            </div>
            ${this.phoneClose()}
          </header>
          <div class="dialog-body">
            <div data-assets-chip class="assets-chip flex flex-col rounded-control border border-border bg-surface-sunken">
              ${rows}
              <div data-assets-totals class="flex items-center px-[11px] py-2 text-caption leading-[1.55] text-text-secondary">
                <span>${n} ${filesNoun(n)}</span><span class="ml-auto font-medium">${formatFileSize(totalSize(picked))}</span>
              </div>
            </div>
          </div>
          <footer class="dialog-foot">
            <button type="button" data-assets-cancel data-hint="${hints.CANCEL_DELETING_ASSETS}" class="${SECONDARY_MD_BUTTON}">Cancel</button>
            <button type="button" data-assets-confirm data-hint="${hints.CONFIRM_DELETING_ASSETS}" class="${DANGER_MD_BUTTON}">${deleteAction(n)}</button>
          </footer>
        </div>`;
  }

  private outcomeHtml(): string {
    const outcomes = this.model.lastDelete ?? [];
    const desc = outcomeDescription(outcomes);
    const rows = outcomes.map((o, i) => {
      const tone = OUTCOME_TONE[o.outcome];
      const rule = i < outcomes.length - 1 ? "border-b border-border" : "";
      return `
        <div role="listitem" data-assets-outcome="${o.outcome}" data-assets-outcome-path="${escapeHtml(o.path)}" class="assets-chip-row flex items-start gap-2.5 px-[11px] py-[7px] ${rule}">
          <span class="flex min-w-0 flex-1 flex-col gap-0.5">
            <span class="truncate ${MONO_BODY_SM} ${tone.path}">${escapeHtml(o.path)}</span>
            ${o.reason === "" ? "" : `<span class="text-caption text-text-tertiary">${escapeHtml(o.reason)}</span>`}
          </span>
          <span data-assets-outcome-word class="min-w-[84px] flex-none pt-0.5 text-right text-caption leading-[1.55] font-medium ${tone.word}">${OUTCOME_WORD[o.outcome]}</span>
        </div>`;
    }).join("");
    return `
        <div data-assets-dialog="outcome" role="dialog" aria-modal="true" aria-labelledby="assets-out-title" class="dialog">
          <header class="dialog-head">
            <div class="min-w-0 flex-1">
              <h2 id="assets-out-title" class="dialog-title dialog-title-prompt">${outcomeTitle(outcomes)}</h2>
              ${desc === "" ? "" : `<p class="dialog-desc">${escapeHtml(desc)}</p>`}
            </div>
            ${this.phoneClose()}
          </header>
          <div class="dialog-body">
            <div role="list" data-assets-chip class="assets-chip flex flex-col rounded-control border border-border bg-surface-sunken">${rows}</div>
          </div>
          <footer class="dialog-foot">
            <button type="button" data-assets-outcome-rescan data-hint="${hints.RESCAN_ASSETS}" class="${SECONDARY_MD_BUTTON} flex items-center gap-2">${lucideIcon("refresh-cw", 16)}<span>Rescan</span></button>
            <button type="button" data-assets-done data-hint="${hints.CLOSE_DELETE_OUTCOME}" class="${PRIMARY_MD_BUTTON}">Done</button>
          </footer>
        </div>`;
  }
}

function glyph(name: "file-text"): Element {
  const template = document.createElement("template");
  template.innerHTML = lucideIcon(name, 14);
  return template.content.firstElementChild!;
}

function focusKey(element: HTMLElement): string | null {
  for (const [key, hook] of [["assetSelect", "data-asset-select"], ["assetOpen", "data-asset-open"]] as const) {
    const path = element.dataset[key];
    if (path !== undefined) return `[${hook}="${CSS.escape(path)}"]`;
  }
  for (const hook of ["data-assets-rescan", "data-assets-close", "data-assets-clear", "data-assets-delete"]) {
    if (element.hasAttribute(hook)) return `[${hook}]`;
  }
  return null;
}
