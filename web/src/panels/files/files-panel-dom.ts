// The Files panel's DOM shell: renders a FilesPanelModel into the docked
// panel and forwards its events. All state lives in the model (and
// ultimately on the server); this file only draws and listens.
//
// Layout is a chrome-bar header carrying
// the title, the breadcrumb, and the upload control, a 320px list column
// with its 38px sort strip, and a read-only viewer beside it. The literal
// pixel values are deliberate.
//
// The viewer is read-only in the strong sense: there is no editor, and
// syntax highlighting returns DOM text nodes from escaped highlighter output.
import type { FileEntry } from "../../api/files.ts";
import { DIALOG_ICON_BUTTON, ICON_BUTTON, SECONDARY_MD_BUTTON, SECONDARY_SM_BUTTON } from "../../ui/controls.ts";
import { escapeHtml } from "../../lib/escape.ts";
import { formatFileSize, formatSince } from "../../lib/format.ts";
import { lucideIcon } from "../../ui/icons.ts";
import { ModalSlot, modalOpen } from "../../ui/modal-host.ts";
import * as hints from "../../lib/hints.ts";
import { uploadDetailsHtml, uploadInfo, uploadRing } from "./files-upload-view.ts";
import {
  fileExtension,
  isImageName,
  formatMiB,
  MAX_IMAGE_VIEW_BYTES,
  rowChange,
  rowMeta,
  rowName,
  splitFileName,
  viewerStatusParts,
  type FilesPanelModel,
  type SortField,
  type ImageView,
  type ViewState,
  type UploadRecord,
} from "./files-panel.ts";

// Wrapping is the viewer's default; the toggle's choice is remembered per
// browser. A storage that throws leaves the default in force.
export const WRAP_STORAGE_KEY = "noisedeck.files.wrap";

export function readWrap(): boolean {
  try {
    return localStorage.getItem(WRAP_STORAGE_KEY) !== "off";
  } catch {
    return true;
  }
}

export function writeWrap(on: boolean): void {
  try {
    localStorage.setItem(WRAP_STORAGE_KEY, on ? "on" : "off");
  } catch {
    // The preference simply fails to persist.
  }
}

// viewerStatusHtml is the footer's status line in addressable parts: each
// fact in its own span, each separator tied to the part after it, and the
// age's words split from its number. The desktop reads every part; the
// phone's footer reads "0.9 KB · 38m", and CSS chooses per
// width — one source of facts, two presentations.
function viewerStatusHtml(entry: FileEntry, view: ViewState | null, now: number): string {
  const joined = viewerStatusParts(entry, view, now).map((part, index) => {
    const html = part.key === "modified"
      ? `<span data-stat-word>modified </span>${escapeHtml(part.short)}<span data-stat-word> ago</span>`
      : escapeHtml(part.text);
    return `${index === 0 ? "" : `<span data-viewer-sep="${part.key}"> · </span>`}<span data-viewer-stat="${part.key}">${html}</span>`;
  }).join("");
  return `<span data-files-viewer-status class="min-w-0 truncate whitespace-pre">${joined}</span>`;
}

export interface FilesPanelShellCallbacks {
  // onClose is the panel's own ✕: the shell does not own the docked
  // frame's visibility, the workspace does.
  onClose?: () => void;
  onExpandedChange?: () => void;
}

export class FilesPanel {
  private container: HTMLElement;
  private model: FilesPanelModel;
  private cb: FilesPanelShellCallbacks;
  private disposed = false;
  private listScroll = new Map<string, { top: number; left: number }>();
  private viewerScroll = new Map<string, { top: number; left: number }>();
  private lastFocusKey: string | null = null;
  private pendingFocusKey: string | null = null;
  private wrapLines = readWrap();
  // zoom is the image viewer's one step, Fit or 100%. It is a reading
  // preference for this panel session, not a fact about any file, so it
  // lives here and outlasts switching files.
  private zoom: "fit" | "actual" = "fit";
  // image is the <img> that loads the current view's source. It outlives
  // renders: the pane is rebuilt from markup on every render, and a fresh
  // element would fetch the image again each time.
  private image: { src: string; el: HTMLImageElement } | null = null;
  private lastDir = "";
  // conflictWasOpen is whether the previous render showed an upload
  // conflict ask, so closing one can hand focus to what the answer
  // produced instead of wherever focus last rested.
  private conflictWasOpen = false;
  // The ask is a screen of the workspace's one modal, outside the docked
  // panel: the panel is a transformed stacking context, so a scrim inside
  // it covers the panel, not the screen. With no document (node tests) or
  // no workspace layer, the panel itself draws the ask inline.
  private readonly conflictSlot = new ModalSlot(() => this.modalsRegion()!, { dismiss: () => this.dismissConflict() });
  // dragCount fences the enter/leave pairs the browser emits as it
  // traverses the column's children, so the overlay stays up for the whole
  // drag and drops only on a real leave.
  private dragCount = 0;
  // Breadcrumb elision state: tail is how many trailing directory segments
  // are currently shown (null means the whole trail), rootLabel hides the
  // project name after every intermediate segment is gone, elision hides the
  // "…" that stands for the given-up segments last, and fitAt is the header
  // width at which the last piece was given up. A ResizeObserver resets the
  // trail on every width change so a mid-transition measurement cannot strand
  // it elided, and growth past fitAt + 4 restores folders when the side panel
  // closes.
  private crumbTail: number | null = null;
  private crumbRootLabel = true;
  // crumbElision is whether the "…" standing in for the given-up segments is
  // drawn. It is the last thing surrendered, because it says only that
  // something was elided while the root and the current crumb say where.
  private crumbElision = true;
  // crumbRoot is the last piece to go. Below roughly 800px the header leaves
  // the trail 55px while the root's icon, one chevron and the current crumb
  // want 64, so where you are wins over how to get back - the list's own
  // parent row still goes up.
  private crumbRoot = true;
  private crumbFitAt = 0;
  private crumbRO: ResizeObserver | null = null;
  private conflictMedia: MediaQueryList | null = null;
  private readonly uploadSlot = new ModalSlot(() => this.modalsRegion() ?? this.container, { dismiss: () => this.closeUploadDetails() });
  private uploadMedia: MediaQueryList | null = null;
  private uploadSheetWasOpen = false;
  private resizeUpload = (): void => this.render();

  private syncConflictActionOrder = (): void => {
    const foot = this.conflictRoot().querySelector<HTMLElement>("[data-files-conflict] .dialog-foot");
    if (foot === null) return;
    const narrow = this.conflictMedia?.matches === true || this.container.closest('[data-viewport="narrow"]') !== null;
    const selectors = ["[data-files-conflict-cancel]", "[data-files-conflict-upload-as]", "[data-files-conflict-replace]"];
    if (narrow) selectors.reverse();
    const buttons = selectors.map((selector) => foot.querySelector<HTMLElement>(selector)).filter((button) => button !== null);
    if (buttons.every((button, index) => foot.children[index] === button)) return;
    const active = typeof document === "undefined" ? null : document.activeElement;
    const restore = active instanceof HTMLElement && foot.contains(active) ? active : null;
    foot.append(...buttons);
    restore?.focus({ preventScroll: true });
  };

  constructor(
    container: HTMLElement,
    model: FilesPanelModel,
    cb: FilesPanelShellCallbacks = {},
  ) {
    this.container = container;
    this.model = model;
    this.cb = cb;
    if (typeof window !== "undefined" && typeof window.matchMedia === "function") {
      this.conflictMedia = window.matchMedia("(max-width: 600px)");
      this.conflictMedia.addEventListener("change", this.syncConflictActionOrder);
      this.uploadMedia = window.matchMedia("(max-width: 800px)");
      this.uploadMedia.addEventListener("change", this.resizeUpload);
    }
    this.watchCrumbWidth();
    this.render();
  }

  async open(): Promise<void> {
    if (this.disposed) return;
    this.pendingFocusKey = this.lastFocusKey ?? "first-row";
    this.render();
    await this.model.open();
    if (this.disposed) return;
    this.render();
    if (this.pendingFocusKey !== null) {
      const key = this.pendingFocusKey;
      this.pendingFocusKey = null;
      this.restoreFocus(key);
    }
  }

  close(): void {
    if (this.disposed) return;
    this.model.reset();
    this.zoom = "fit";
    this.image = null;
    this.render();
  }

  dispose(): void {
    this.model.reset();
    this.disposed = true;
    this.conflictMedia?.removeEventListener("change", this.syncConflictActionOrder);
    this.crumbRO?.disconnect();
    this.crumbRO = null;
    this.conflictSlot.close();
    this.uploadSlot.close();
    this.uploadMedia?.removeEventListener("change", this.resizeUpload);
    this.container.innerHTML = "";
  }

  render(): void {
    if (this.disposed) return;
    const m = this.model;
    this.captureScroll();
    const focusKey = this.activeFocusKey();
    if (focusKey !== null) this.lastFocusKey = focusKey;
    const panel = this.container.closest<HTMLElement>('[data-region="files"]') ?? this.container;
    const expanded = m.expanded ? "true" : "false";
    if (panel.dataset.expanded !== expanded) {
      panel.dataset.expanded = expanded;
      this.cb.onExpandedChange?.();
    }
    // On the phone the list stays visible when a file is merely selected;
    // the viewer appears only once its content
    // has been requested, so selection without a view keeps the list on
    // screen. On desktop a selected file always has a view.
    panel.dataset.viewing =
      m.selected !== null && m.view !== null ? "true" : "false";
    // Navigation to a new directory shows the whole trail; the fit loop
    // re-elides it after paint if the header is too narrow.
    if (m.dir !== this.lastDir) {
      this.resetCrumbTrail();
      this.lastDir = m.dir;
    }
    // Expanding hands the viewer the width the list column occupied, but the
    // header stays: without the breadcrumb trail there is nothing on screen
    // saying which directory the open file came from, and no way to reach a
    // sibling file without collapsing first.
    // On the fallback mount (no modals layer) the ask is part of the
    // panel's own markup; on the portal it is rendered separately below.
    const inlineConflict = this.modalsRegion() === null ? this.conflictDialogHtml() : "";
    this.container.innerHTML = `
      ${this.headerHtml()}
      <div class="flex min-h-0 min-w-0 flex-1">
        ${m.expanded ? "" : this.listColumnHtml()}
        <div data-files-viewer class="flex min-w-0 flex-1 flex-col">${this.viewerHtml()}</div>
      </div>
      ${inlineConflict}
    `;
    this.paintViewerText();
    this.mountImage();
    this.bind();
    this.restoreScroll();
    this.renderConflict();
    this.renderUploadSheet();
    // Measure after the next paint so the loop works with real geometry,
    // not the stub container's zero rects in unit tests. Scheduled before
    // the focus guard: every render re-measures the trail, whoever owns
    // focus.
    if (typeof requestAnimationFrame !== "undefined") {
      requestAnimationFrame(() => this.measureCrumbs());
    }
    const conflictOpen = m.conflict !== null && !m.confirming;
    if (!conflictOpen && this.conflictWasOpen) {
      // The ask closed: hand focus to what the answer produced — the
      // viewer of the file that landed, or the upload control when
      // nothing did.
      this.pendingFocusKey = m.selected !== null && m.view !== null ? "back" : "upload";
    }
    this.conflictWasOpen = conflictOpen;
    if (conflictOpen) {
      // The ask owns the keyboard while it stands: the field takes focus
      // with its base selected unless focus is already inside the dialog,
      // and any focus a completed action queued gives way to it.
      this.pendingFocusKey = null;
      const dialog = this.conflictRoot().querySelector<HTMLElement>("[data-files-conflict]");
      const active = typeof document === "undefined" ? null : document.activeElement;
      if (dialog !== null && !(active !== null && dialog.contains(active))) {
        this.focusConflictField();
      }
      return;
    }
    if (this.uploadSheetWasOpen) return;
    // Focus is restored only when the panel owns it — a key queued by an
    // action, or the focus this render just destroyed. A render while the
    // you work elsewhere must not pull focus into the panel.
    if (this.pendingFocusKey === null && focusKey === null) return;
    // A screen still open in the modal keeps the keyboard.
    if (modalOpen()) {
      this.pendingFocusKey = null;
      return;
    }
    if (this.restoreFocus(this.pendingFocusKey) || this.restoreFocus(focusKey)) {
      this.pendingFocusKey = null;
    } else {
      this.pendingFocusKey = null;
      this.restoreFocus("close");
    }
  }

  // The trail IS the panel's title: no "Files" label, no rooted prefix,
  // just the house carrying the project name and the directory segments.
  // The nav uses the repo's own crumb classes so the three required DS
  // overrides (item padding, root square target, shrink floors) land once
  // in app.css.
  private headerHtml(): string {
    return `
      <div data-files-header class="chrome gap-[10px] pl-(--space-5) pr-(--space-4)">
        <nav aria-label="Project path" data-files-crumbs class="chrome-title min-w-0 flex-1">
          ${this.crumbsHtml()}
        </nav>
        ${this.uploadBadgeHtml()}${this.uploadButtonHtml()}
        <button type="button" data-files-close data-focus-key="close" aria-label="Close files" data-hint="${hints.CLOSE_FILES_PANEL}"
          class="${ICON_BUTTON}">${lucideIcon("x", 14)}</button>
      </div>
    `;
  }

  // crumbsHtml renders the whole trail or its elided form. Clicks are
  // delegated on the wrapper; each clickable crumb carries its segment
  // depth (-1 for the root) so the handler resolves it positionally.
  // Every label is wrapped in a shrinking span so the text ellipsises
  // inside the crumb rather than clipping the whole item past the nav.
  private crumbsHtml(): string {
    const m = this.model;
    const segments = m.dir === "" ? [] : m.dir.split("/");
    const tail = this.crumbTail ?? segments.length;
    const deep = tail < segments.length;
    const shown: number[] = deep
      ? [-1].concat(segments.slice(segments.length - tail).map((_, i) => segments.length - tail + i))
      : segments.map((_, i) => i);

    const rootLabelText = escapeHtml(m.rootLabel());
    const rootLabel = this.crumbRootLabel ? rootLabelText : "";
    const root = `<button type="button" data-files-crumb-depth="-1" data-focus-key="crumb:root"
      title="${rootLabelText}" aria-label="${rootLabelText}" ${m.dir === "" ? 'aria-current="location"' : ""}
      data-hint="${hints.CLICK_PROJECT_NAME_ROOT}"
      class="crumb-item ${rootLabel === "" ? "crumb-root-icon" : ""}">
      <span class="inline-flex h-[1.55em] items-center gap-[6px]">${lucideIcon("house", 14)}${rootLabel === "" ? "" : `<span class="crumb-label min-w-0 truncate">${rootLabel}</span>`}</span>
    </button>`;

    const tight = tail <= 1 && !this.crumbRootLabel;
    const items = shown.map((i, n) => {
      const isLast = n === shown.length - 1;
      const label = i < 0 ? "…" : escapeHtml(segments[i]!);
      if (isLast) {
        return `<span class="crumb-item crumb-current" ${tight ? "data-crumb-tight" : ""} aria-current="location"><span class="crumb-label min-w-0 truncate">${label}</span></span>`;
      }
      if (i < 0) {
        return this.crumbElision
          ? `<span class="crumb-item crumb-ellipsis" aria-hidden="true">${label}</span>`
          : "";
      }
      return `<button type="button" data-files-crumb-depth="${i}" data-focus-key="crumb:${escapeHtml(segments[i]!)}" data-hint="${hints.OPEN_THIS_FOLDER}"
        class="crumb-item"><span class="crumb-label min-w-0 truncate">${label}</span></button>`;
    });

    const sep = `<span class="crumb-sep" aria-hidden="true">${lucideIcon("chevron-right", 13)}</span>`;
    // A surrendered piece renders as nothing, and joining a gap would leave two
    // chevrons with no crumb between them.
    return [this.crumbRoot ? root : "", ...items].filter((part) => part !== "").join(sep);
  }

  private listColumnHtml(): string {
    return `
      <div data-files-list class="relative flex w-[320px] min-h-0 flex-none flex-col border-r border-border">
        ${this.sortStripHtml()}
        <div data-files-list-scroll data-files-dir="${escapeHtml(this.model.dir)}"
          class="flex min-h-0 flex-1 flex-col gap-px overflow-y-auto overscroll-contain p-3">${this.listBodyHtml()}</div>
        ${this.dropOverlayHtml()}
        ${this.floatingUploadButtonHtml()}
      </div>
    `;
  }

  // The sort strip is the reference's 38px ordering row: Name / Size /
  // Modified, with the active column carrying a wash and a direction arrow.
  // Each heading is a button so the reorder is reachable from the keyboard.
  private sortStripHtml(): string {
    const m = this.model;
    return `
      <div data-files-sort role="group" aria-label="Sort files"
        class="flex h-[38px] flex-none items-center gap-[2px] border-b border-border px-(--space-3)">
        ${(["name", "size", "modified"] as SortField[]).map((field) => {
          const active = m.sort.field === field;
          const label = field === "name" ? "Name" : field === "size" ? "Size" : "Modified";
          const arrow = active ? (m.sort.direction === "asc" ? " ↑" : " ↓") : "";
          const tone = active ? "bg-wash-selected text-text-primary" : "bg-transparent text-text-tertiary";
          return `<button type="button" data-files-sort data-sort-field="${field}" data-focus-key="sort:${field}"
            aria-pressed="${active ? "true" : "false"}" data-hint="${hints.sortFiles(field)}"
            class="flex-none cursor-pointer rounded-control-sm border-0 px-[8px] py-[3px] text-caption font-medium transition-colors hover:bg-wash-hover hover:text-text-primary focus-visible:bg-wash-hover ${tone}">${label}${arrow}</button>`;
        }).join("")}
      </div>
    `;
  }

  private uploadBadgeHtml(): string {
    const records = this.model.uploadRecords;
    if (records.length === 0) return "";
    const count = this.model.activeUploadCount();
    const label = count > 0
      ? `${count} active ${count === 1 ? "upload" : "uploads"}`
      : `${records.length} ${records.length === 1 ? "upload" : "uploads"} to inspect`;
    return `<button type="button" data-files-upload-summary data-focus-key="upload-summary" class="files-upload-badge" aria-label="Show upload details: ${label}">${count || records.length}</button>`;
  }

  // uploadButtonHtml is the header's icon-only upload action over the
  // hidden multiple file input. The trail needs the width; the word
  // "Upload" was given up for the house and its project name.
  private uploadButtonHtml(): string {
    const m = this.model;
    return `
      <span data-files-upload-area class="relative flex-none">
        <input data-files-input type="file" multiple class="hidden">
        <span role="status" aria-live="polite" class="sr-only">${m.uploading || m.confirming ? "Uploading files…" : ""}</span>
        <button type="button" data-files-upload-header data-focus-key="upload" aria-label="Upload files" data-hint="${hints.UPLOAD_FILES_INTO_FOLDER}"
          ${m.uploading || m.confirming ? "disabled" : ""}
          class="${ICON_BUTTON} disabled:opacity-45 max-md:h-11">${lucideIcon("upload", 14)}</button>
      </span>
    `;
  }

  // floatingUploadButtonHtml is the phone's circular upload affordance at
  // the bottom right of the list. It shares the
  // same hidden input as the header button, which is hidden on narrow.
  private floatingUploadButtonHtml(): string {
    const m = this.model;
    return `
      <button type="button" data-files-upload data-files-upload-floating data-focus-key="upload-floating"
        aria-label="Upload files" data-hint="${hints.UPLOAD_FILES_INTO_FOLDER}"
        ${m.uploading || m.confirming ? "disabled" : ""}
        class="absolute right-5 bottom-[104px] z-10 hidden h-14 w-14 items-center justify-center rounded-full bg-accent text-text-on-accent transition-colors hover:bg-accent-hover disabled:opacity-45 focus-visible:bg-wash-hover">
        ${lucideIcon("upload", 20)}
      </button>
    `;
  }

  // dropOverlayHtml is the whole-column drop target's drag state: an inset
  // dashed accent box on the accent tint naming the directory being filled.
  // It is drawn always and shown by bind() while a file drag is inside, so
  // the drag never has to wait on a re-render.
  private dropOverlayHtml(): string {
    return `
      <div data-files-drop-overlay hidden aria-hidden="true"
        class="pointer-events-none absolute inset-2 z-10 flex items-center justify-center rounded-control border-2 border-dashed border-accent bg-accent-tint text-body-sm font-medium text-accent-on-tint">
        <span>Drop to upload into <span>${escapeHtml(this.model.dropTargetLabel())}</span></span>
      </div>
    `;
  }

  // Upload errors render at the top of the list body: the upload lives in
  // the header now, so its per-file failures sit with the rows they belong
  // to rather than in a footer the sort strip separated from the action.
  private uploadErrorsHtml(): string {
    if (this.model.uploadErrors.length === 0) return "";
    return `<ul data-files-upload-errors class="mb-2 flex flex-col gap-1 rounded-control bg-error-tint px-3 py-2">${
      this.model.uploadErrors.map((line) =>
        `<li class="break-words font-mono text-micro text-error-on-tint">${escapeHtml(line)}</li>`
      ).join("")
    }</ul>`;
  }

  private listBodyHtml(): string {
    const m = this.model;
    const ready = m.listError === null && m.listing !== null
      ? this.uploadErrorsHtml()
      : "";
    if (m.listError !== null) {
      return `<p class="rounded-control bg-error-tint px-3 py-2 text-caption text-error-on-tint">${escapeHtml(m.listError)}</p>`;
    }
    if (m.listing === null) {
      return `<p class="px-2 py-1.5 text-body-sm text-text-tertiary">Loading files…</p>`;
    }
    const up = this.upRowHtml();
    const entries = m.sortedEntries();
    // An empty directory says so even when the ".." row is present: the
    // row is the way out, the message is why there is nothing else.
    const empty = entries.length === 0
      ? `<p class="px-2 py-1.5 text-body-sm text-text-tertiary">empty directory</p>`
      : "";
    const records = m.uploadRecords.filter((record) => record.destDir === m.dir);
    const occupied = new Set(records.filter((record) => !["failed", "stopped", "too-large"].includes(record.status)).map((record) => record.name));
    const rows = entries.filter((entry) => !occupied.has(entry.name)).map((entry) => this.rowHtml(entry)).join("")
      + records.map((record) => this.uploadRowHtml(record)).join("");
    // A truncated listing says so where the rows end: a short list
    // rendered as a complete one is a lie about the directory.
    const truncated = m.truncated()
      ? `<p class="px-2 pt-2 text-caption text-text-tertiary">listing truncated · showing ${entries.length} entries</p>`
      : "";
    return `${ready}${up}${rows}${records.length === 0 ? empty : ""}${truncated}`;
  }

  private uploadRowHtml(record: UploadRecord): string {
    const info = uploadInfo(record);
    const selected = this.model.selectedUpload === record;
    return `<div data-files-upload-row="${record.id}" class="files-upload-row ${selected ? "bg-wash-selected" : ""}">
      <button type="button" data-files-upload-select="${record.id}" data-focus-key="upload:${record.id}" aria-current="${selected}" aria-label="${escapeHtml(`${record.name}, ${info.state}`)}" data-hint="${escapeHtml(`${record.name} · ${info.amount} · ${info.state}`)}">
        <span data-files-upload-icon class="flex-none ${info.error ? "text-error" : "text-text-tertiary"}">${uploadRing(record)}</span>
        <span class="min-w-0 flex-1 truncate text-body-sm font-medium text-text-secondary">${escapeHtml(record.name)}</span>
        <span data-files-upload-meta class="flex-none text-caption ${info.error ? "text-error" : "text-text-tertiary"}">${escapeHtml(info.meta)}</span>
      </button>
      ${!info.cancel ? "" : `<button type="button" data-files-upload-action="${record.id}" data-action="cancel" data-focus-key="upload-cancel:${record.id}" aria-label="${info.cancelLabel} ${record.status === "confirming" ? "for" : "of"} ${escapeHtml(record.name)}" class="files-upload-row-action">${lucideIcon("x", 12)}</button>`}
    </div>`;
  }

  // The ".." row steps one level up — the bigger target for the common
  // step, where crumbs are for jumping several. It names where it goes.
  private upRowHtml(): string {
    const m = this.model;
    if (!m.hasUp()) return "";
    const label = escapeHtml(m.upLabel());
    return `
      <div role="button" tabindex="0" data-files-up data-focus-key="up" data-hint="${hints.upTo(label)}"
        class="flex w-full flex-none cursor-pointer items-center gap-[9px] rounded-control border-0 bg-transparent px-2 py-1.5 text-left transition-colors hover:bg-wash-hover focus-visible:bg-wash-hover max-md:min-h-11">
        <span aria-hidden="true" class="flex-none text-text-secondary">${lucideIcon("folder", 14)}</span>
        <span class="min-w-0 flex-1 truncate text-body-sm font-medium text-text-primary">..</span>
      </div>
    `;
  }

  private rowHtml(entry: FileEntry): string {
    const m = this.model;
    const selected = m.selected?.path === entry.path;
    const meta = rowMeta(entry, m.sort.field, Date.now());
    const change = rowChange(entry);
    const tone = change === "New" ? "text-git-added" : change === "Modified" ? "text-git-modified" : entry.dir ? "text-text-secondary" : "text-text-tertiary";
    return `
      <div role="button" tabindex="0" data-files-row="${escapeHtml(entry.path)}" data-focus-key="row:${escapeHtml(entry.path)}"
        aria-current="${selected ? "true" : "false"}" data-hint="${change === "" ? (entry.dir ? hints.OPEN_THIS_FOLDER : hints.OPEN_THIS_FILE) : `${change} · ${entry.dir ? hints.OPEN_THIS_FOLDER : hints.OPEN_THIS_FILE}`}"
        class="flex w-full flex-none cursor-pointer items-center gap-[9px] rounded-control border-0 px-2 py-1.5 text-left transition-colors max-md:min-h-11 ${selected ? "bg-wash-selected" : "bg-transparent"} hover:bg-wash-hover focus-visible:bg-wash-hover">
        <span aria-hidden="true" ${change === "" ? "" : `data-files-change="${entry.change}"`} class="flex-none ${tone}">${lucideIcon(entry.dir ? "folder" : "file-text", 14)}</span>
        ${change === "" ? "" : `<span class="sr-only">${change}</span>`}
        <span class="min-w-0 flex-1 truncate text-body-sm font-medium ${entry.dir || selected ? "text-text-primary" : "text-text-secondary"}">${escapeHtml(rowName(entry))}</span>
        ${meta === "" ? "" : `<span class="flex-none text-caption text-text-tertiary">${escapeHtml(meta)}</span>`}
      </div>
    `;
  }

  private viewerHtml(): string {
    const m = this.model;
    if (m.selectedUpload !== null) return uploadDetailsHtml(m.selectedUpload, m.rootLabel(), false);
    const entry = m.selected;
    if (entry === null) {
      return `<div class="flex flex-1 flex-col items-center justify-center gap-[5px] bg-surface-sunken p-6 text-center">
        <span class="text-body font-medium text-text-secondary">Nothing open</span>
        <span class="max-w-[34ch] text-body-sm leading-[1.5] text-text-tertiary [text-wrap:pretty]">Pick a file to read it.</span>
      </div>`;
    }
    return `${this.viewerHeaderHtml(entry)}${this.viewerBodyHtml(entry)}${this.viewerFooterHtml(entry)}`;
  }

  private viewerHeaderHtml(entry: FileEntry): string {
    const m = this.model;
    const expandLabel = m.expanded ? "Collapse" : "Expand";
    const back = `<button type="button" data-files-back data-focus-key="back" aria-label="Back to files" data-hint="${hints.RETURN_TO_FILE_LIST}"
      class="${ICON_BUTTON} hidden max-md:grid">${lucideIcon("chevron-left", 14)}</button>`;
    const download = m.canDownload()
      ? `<a data-files-download data-focus-key="download" download
      href="${escapeHtml(m.downloadUrl(entry))}" aria-label="Download file" data-hint="${hints.DOWNLOAD_THIS_FILE}"
      class="${ICON_BUTTON}">${lucideIcon("download", 14)}</a>`
      : "";
    const expand = `<button type="button" data-files-expand data-focus-key="expand" aria-label="${expandLabel}"
      aria-pressed="${m.expanded ? "true" : "false"}" data-hint="${m.expanded ? hints.VIEWER_COLLAPSE : hints.VIEWER_EXPAND}"
      class="${ICON_BUTTON} max-md:hidden">${lucideIcon(m.expanded ? "minimize-2" : "maximize-2", 14)}</button>`;
    return `
      <div data-files-viewer-header class="flex h-[38px] flex-none items-center gap-2.5 border-b border-border px-4">
        ${back}
        <span class="flex-none text-text-tertiary max-md:hidden">${lucideIcon("file-text", 14)}</span>
        <span data-files-viewer-name data-hint="${hints.viewerFileName(escapeHtml(entry.name))}" class="min-w-0 shrink truncate text-body-sm font-medium text-text-primary">${escapeHtml(entry.name)}</span>
        <span data-files-viewer-meta class="min-w-0 shrink-[999] truncate text-caption text-text-tertiary max-md:hidden">${escapeHtml(rowMeta(entry, m.sort.field, Date.now()))}</span>
        <span class="ml-auto"></span>
        <span data-files-viewer-actions class="flex flex-none items-center gap-2.5">${download}${expand}</span>
      </div>
    `;
  }

  // viewerBodyHtml draws the pane between header and footer. It never
  // decodes bytes the server declined to decode: no-preview is a state
  // with a download, and the reason is only shown when the server gave
  // one (the size bound).
  private viewerBodyHtml(entry: FileEntry): string {
    const view = this.model.view;
    if (view === null || view.kind === "loading") {
      return `<div class="flex flex-1 items-center justify-center bg-surface-sunken">
        <span class="text-micro text-text-tertiary">loading…</span>
      </div>`;
    }
    if (view.kind === "error") {
      return `<div class="flex flex-1 items-center justify-center bg-surface-sunken p-6">
        <p class="max-w-full break-words rounded-control bg-error-tint px-3 py-2 text-caption text-error-on-tint">${escapeHtml(view.message)}</p>
      </div>`;
    }
    if (view.kind === "image") return this.imageBodyHtml(entry, view.image);
    if (view.kind === "no-preview") {
      return `<div class="flex flex-1 flex-col items-center justify-center gap-[11px] bg-surface-sunken p-6 text-center">
        <span class="text-body-sm text-text-tertiary">No preview for ${escapeHtml(fileExtension(entry.name))} files.</span>
        ${view.reason === "" ? "" : `<span class="max-w-full break-words text-micro text-text-tertiary">${escapeHtml(view.reason)}</span>`}
        <a data-files-download data-focus-key="download-binary" download href="${escapeHtml(this.model.downloadUrl(entry))}" data-hint="${hints.DOWNLOAD_THIS_FILE}"
          class="flex h-7 items-center gap-1.5 rounded-control border border-field-border bg-sidebar-surface px-3 text-body-sm text-text-primary transition-colors max-md:min-h-11 hover:bg-field-hover focus-visible:bg-wash-hover">${lucideIcon("download", 13)}Download</a>
      </div>`;
    }
    if (view.kind === "diff" && view.content === "") {
      return `<div data-files-empty class="flex flex-1 items-center justify-center bg-surface-sunken">
        <span class="text-micro text-text-tertiary">No changes since the last commit</span>
      </div>`;
    }
    // An empty file has no lines to number. Saying so is the faithful
    // rendering; a lone blank gutter row would read as a failed load.
    if (view.content === "") {
      return `<div data-files-empty class="flex flex-1 items-center justify-center bg-surface-sunken">
        <span class="text-micro text-text-tertiary">empty file</span>
      </div>`;
    }
    return `
      <div data-files-viewer-scroll data-files-path="${escapeHtml(entry.path)}"
        class="min-h-0 min-w-0 flex-1 overflow-auto overscroll-contain bg-surface-sunken">
        <pre data-files-text
          data-files-wrapped="${this.wrapLines ? "true" : "false"}"
          class="m-0 pb-4 pr-4 pt-3 font-mono text-[min(var(--type-mono-size),calc(var(--type-caption-size)*var(--type-mono-xh-ratio)))] leading-[1.62] text-text-secondary ${this.wrapLines ? "w-full min-w-0 whitespace-pre-wrap [overflow-wrap:anywhere]" : "min-w-max whitespace-pre"}"></pre>
      </div>
    `;
  }

  // viewerFooterHtml is the mono status line and the wrap toggle. The
  // toggle is a REAL control on every screen (the "wrap" label with a
  // 6px dot) — not a phone-only second action.
  private viewerFooterHtml(entry: FileEntry): string {
    const m = this.model;
    const image = m.view?.kind === "image" ? m.view.image : null;
    // An image's facts are a format, dimensions, a size and an age: all
    // interface voice, so this footer is set in the interface face.
    if (image !== null) {
      return `
      <div data-files-viewer-footer data-files-image-footer class="flex flex-none items-center gap-2 border-t border-border bg-surface px-4 py-[5px] font-ui text-caption leading-5 text-text-tertiary">
        ${viewerStatusHtml(entry, m.view, Date.now())}
        <span class="ml-auto"></span>
        ${image.state === "shown" ? this.zoomHtml() : ""}
      </div>
    `;
    }
    const changes = (entry.change === undefined && m.viewerMode !== "changes") || isImageName(entry.name) ? "" : `<button type="button" data-files-changes data-focus-key="changes"
      aria-label="${m.viewerMode === "changes" ? "Show file" : "Show changes"}" aria-pressed="${m.viewerMode === "changes" ? "true" : "false"}"
      data-hint="${m.viewerMode === "changes" ? hints.SHOW_FILE_CONTENT : hints.SHOW_FILE_CHANGES}" class="${SECONDARY_SM_BUTTON} font-ui max-md:min-h-11 max-md:px-3">${m.viewerMode === "changes" ? "Show file" : "Show changes"}</button>`;
    const wrap = m.view?.kind === "text" || m.view?.kind === "diff"
      ? `<button type="button" data-files-wrap data-focus-key="wrap" aria-label="Wrap lines"
          aria-pressed="${this.wrapLines ? "true" : "false"}" data-hint="${hints.WRAP_LONG_LINES}"
          class="flex flex-none items-center gap-[6px] rounded-control-sm px-[7px] py-0.5 text-text-tertiary transition-colors hover:bg-wash-hover hover:text-text-primary focus-visible:bg-wash-hover max-md:min-h-11 max-md:px-3">${this.wrapDot()}wrap${this.wrapSwitch()}</button>`
      : "";
    return `
      <div data-files-viewer-footer class="flex flex-none items-center gap-2 border-t border-border bg-surface px-4 py-[6px] font-mono text-micro text-text-tertiary">
        ${viewerStatusHtml(entry, m.view, Date.now())}
        <span class="ml-auto"></span>
        ${changes}${wrap}
      </div>
    `;
  }

  // imageBodyHtml draws an image view. While the decode runs the pane is
  // the spinner, because there is nothing measured to lay out yet; the
  // <img> itself is mounted after the markup (mountImage). Fit is the
  // whole image inside the pane, never scaled past its natural size; 100%
  // is natural size and scrolls. The checkerboard is drawn only behind an
  // image whose decode found a transparent pixel.
  private imageBodyHtml(entry: FileEntry, image: ImageView): string {
    if (image.state === "loading") {
      return `<div data-files-image-loading class="flex flex-1 items-center justify-center bg-surface-sunken">
        <span role="status" aria-label="Loading preview"
          class="block h-4 w-4 flex-none animate-spinner rounded-circle border-[1.5px] border-border-strong border-t-text-secondary"></span>
      </div>`;
    }
    if (image.state === "shown") {
      const ground = image.alpha ? "checkerboard" : "bg-surface-sunken";
      const frameClass = `relative overflow-hidden [outline:1px_solid_var(--color-border)] ${ground}`;
      const label = `aria-label="${escapeHtml(entry.name)}"`;
      if (this.zoom === "actual") {
        return `<div data-files-viewer-scroll data-files-image-canvas="actual" data-files-path="${escapeHtml(entry.path)}"
          class="min-h-0 min-w-0 flex-1 overflow-auto overscroll-contain bg-surface-sunken">
          <div class="w-max p-5 max-md:p-4">
            <div data-files-image-frame role="img" ${label} data-files-alpha="${image.alpha ? "true" : "false"}"
              class="${frameClass}" style="width:${image.width}px;height:${image.height}px"></div>
          </div>
        </div>`;
      }
      return `<div data-files-image-canvas="fit" class="flex min-h-0 min-w-0 flex-1 bg-surface-sunken p-5 max-md:p-4">
        <div class="flex min-h-0 min-w-0 flex-1 items-center justify-center [container-type:size]">
          <div data-files-image-frame role="img" ${label} data-files-alpha="${image.alpha ? "true" : "false"}"
            class="${frameClass} flex-none"
            style="width:min(100cqw, calc(100cqh * ${image.width} / ${image.height}), ${image.width}px);aspect-ratio:${image.width} / ${image.height}"></div>
        </div>
      </div>`;
    }
    const download = `<a data-files-download data-focus-key="download-binary" download href="${escapeHtml(this.model.downloadUrl(entry))}" data-hint="${hints.DOWNLOAD_THIS_FILE}"
      class="${SECONDARY_SM_BUTTON} gap-1.5 text-text-primary">${lucideIcon("download", 13)}Download</a>`;
    if (image.state === "gone") {
      const refresh = `<button type="button" data-files-refresh-list data-focus-key="refresh-list" data-hint="${hints.REFRESH_FILE_LIST}"
        class="${SECONDARY_SM_BUTTON} gap-1.5 text-text-primary">${lucideIcon("refresh-cw", 13)}Refresh list</button>`;
      return this.imageMessageHtml("gone", "No longer on disk",
        `${entry.name} was in the list, then removed before Deck could read it.`, refresh);
    }
    if (image.state === "oversized") {
      return this.imageMessageHtml("oversized", "Too large to preview",
        `Previews stop at ${formatMiB(MAX_IMAGE_VIEW_BYTES)}. This file is ${formatMiB(entry.size ?? 0)}.`, download);
    }
    return this.imageMessageHtml("undecodable", "This file didn’t decode as an image.",
      "It may still be being written. Download it to check.", download);
  }

  private imageMessageHtml(state: string, title: string, body: string, action: string): string {
    return `<div role="status" data-files-image-message="${state}"
      class="flex flex-1 flex-col items-center justify-center gap-[6px] bg-surface-sunken px-6 text-center">
      <span class="text-body-sm font-medium text-text-secondary">${escapeHtml(title)}</span>
      <span class="max-w-[34ch] text-caption text-text-tertiary [text-wrap:pretty]">${escapeHtml(body)}</span>
      <span data-files-image-action class="mt-[5px] flex">${action}</span>
    </div>`;
  }

  // zoomHtml is the image's Fit/100% pair, in the footer slot text files
  // give the wrap toggle.
  private zoomHtml(): string {
    const segment = (value: "fit" | "actual", label: string, hint: string): string => {
      const on = this.zoom === value;
      return `<button type="button" data-files-zoom="${value}" data-focus-key="zoom-${value}" aria-pressed="${on ? "true" : "false"}" data-hint="${hint}"
        class="rounded-control-sm px-[7px] py-0.5 font-medium transition-colors ${on ? "bg-wash-selected text-text-primary" : "text-text-tertiary hover:bg-wash-hover hover:text-text-primary"} focus-visible:bg-wash-hover">${label}</button>`;
    };
    return `<span data-files-zoom-group role="group" aria-label="Zoom" class="-my-0.5 -mr-[5px] flex flex-none gap-0.5">
      ${segment("fit", "Fit", hints.ZOOM_FIT)}${segment("actual", "100%", hints.ZOOM_ACTUAL)}
    </span>`;
  }

  // mountImage puts the view's <img> into the pane. The element is kept
  // across renders for as long as the source is the same, so a render
  // never refetches; a new source (another file, or the same file at a
  // new mtime) gets a new element, and only its decode may report.
  private mountImage(): void {
    const view = this.model.view;
    const image = view?.kind === "image" ? view.image : null;
    if (image === null || (image.state !== "loading" && image.state !== "shown") || typeof document === "undefined") {
      this.image = null;
      return;
    }
    if (this.image?.src !== image.src) this.image = { src: image.src, el: this.loadImage(image.src) };
    const frame = this.container.querySelector<HTMLElement>("[data-files-image-frame]");
    if (frame !== null) frame.append(this.image.el);
  }

  private loadImage(src: string): HTMLImageElement {
    const el = document.createElement("img");
    el.alt = "";
    el.draggable = false;
    el.dataset.filesImage = "";
    el.className = "block h-full w-full";
    el.addEventListener("load", () => {
      if (this.disposed || this.image?.el !== el) return;
      if (this.model.imageDecoded(src, el.naturalWidth, el.naturalHeight, hasAlpha(el))) this.render();
    });
    el.addEventListener("error", () => {
      if (this.disposed || this.image?.el !== el) return;
      void this.model.imageFailed(src).then((changed) => {
        if (changed && !this.disposed) this.render();
      });
    });
    el.src = src;
    return el;
  }

  // wrapDot is the toggle's 6px state mark: accent when wrapping is on,
  // the strong border when it is off.
  private wrapDot(): string {
    return `<span aria-hidden="true" data-files-wrap-dot class="h-[6px] w-[6px] flex-none rounded-full ${this.wrapLines ? "bg-accent" : "bg-border-strong"}"></span>`;
  }

  // wrapSwitch is the same state as a 26×16 track and knob: the phone draws
  // the toggle as a switch where the desktop draws the dot.
  // Both are in the markup and CSS shows one per width, so the control's
  // state cannot fork between them.
  private wrapSwitch(): string {
    return `<span aria-hidden="true" data-files-wrap-switch
      class="hidden h-4 w-[26px] flex-none items-center rounded-pill p-0.5 ${this.wrapLines ? "justify-end bg-accent" : "justify-start bg-border-strong"}">
      <span class="h-3 w-3 flex-none rounded-circle bg-surface-card"></span>
    </span>`;
  }

  // paintViewerText writes the file's bytes as TEXT. Server-returned file
  // contents never reach an innerHTML: the panel renders a file, it does
  // not run one. Each logical line is a row carrying a sticky number
  // gutter and the line's own text node, so the pane's textContent is
  // exactly the file's bytes and the newline separators.
  private paintViewerText(): void {
    const view = this.model.view;
    if (view === null || (view.kind !== "text" && view.kind !== "diff")) return;
    const body = this.container.querySelector<HTMLElement>("[data-files-text]");
    if (body === null) return;
    const path = this.model.selected?.path ?? "";
    this.paintViewerRows(body, view.content.split("\n"), view.kind === "diff");
    if (view.kind === "text") void this.highlightViewerText(body, path, view.content);
  }

  private async highlightViewerText(body: HTMLElement, path: string, content: string): Promise<void> {
    try {
      const { highlightedLines } = await import("./syntax.ts");
      const lines = highlightedLines(path, content);
      if (lines === null || this.disposed || !body.isConnected ||
          this.model.selected?.path !== path || this.model.view?.kind !== "text" ||
          this.model.view.content !== content) return;
      this.paintViewerRows(body, lines);
    } catch {
      // A module-load failure leaves the already painted plain text intact.
    }
  }

  private paintViewerRows(body: HTMLElement, lines: (string | DocumentFragment)[], diff = false): void {
    body.replaceChildren();
    let inHunk = false;
    lines.forEach((line, index) => {
      const row = document.createElement("span");
      row.dataset.filesLine = "";
      if (diff && typeof line === "string") {
        if (line.startsWith("@@")) inHunk = true;
        row.dataset.filesDiff = !inHunk || line.startsWith("@@") ? "meta"
          : line.startsWith("+") ? "added"
          : line.startsWith("-") ? "removed"
          : "context";
      }
      const number = document.createElement("span");
      number.dataset.filesLineNumber = "";
      number.dataset.lineNumber = String(index + 1);
      const text = document.createElement("span");
      text.dataset.filesLineText = "";
      if (typeof line === "string") text.textContent = line;
      else text.append(line);
      row.append(number, text);
      body.append(row);
      if (index < lines.length - 1) {
        const newline = document.createElement("span");
        newline.dataset.filesNewline = "";
        newline.textContent = "\n";
        body.append(newline);
      }
    });
  }

  private bind(): void {
    this.bindUploadControls(this.container);
    const q = <T extends Element>(selector: string): T | null =>
      this.container.querySelector<T>(selector);

    // Breadcrumb clicks are delegated on the wrapper because Blueprint's
    // Breadcrumbs renders from label + href only and drops per-item click
    // props. The depth table works whether the trail is whole or elided.
    q<HTMLElement>("[data-files-crumbs]")?.addEventListener("click", (event) => {
      const item = (event.target as HTMLElement).closest<HTMLElement>("[data-files-crumb-depth]");
      if (item === null) return;
      event.preventDefault();
      event.stopPropagation();
      const raw = item.dataset.filesCrumbDepth;
      if (raw === undefined || raw === "") return;
      const depth = Number.parseInt(raw, 10);
      const target = depth === -1
        ? ""
        : this.model.dir.split("/").slice(0, depth + 1).join("/");
      void this.run(() => this.model.openDirectory(target), "close", "first-row");
    });
    for (const row of this.container.querySelectorAll<HTMLElement>("[data-files-row]")) {
      const activate = (event: Event): void => {
        event.stopPropagation();
        const entry = this.model.entries().find((e) => e.path === row.dataset.filesRow);
        if (entry === undefined) return;
        void this.run(
          () => this.model.activate(entry),
          entry.dir ? "close" : "back",
          entry.dir ? "first-row" : "back",
        );
      };
      row.addEventListener("click", activate);
      row.addEventListener("keydown", (event) => {
        if (event.key !== "Enter" && event.key !== " ") return;
        event.preventDefault();
        activate(event);
      });
    }
    const up = q<HTMLElement>("[data-files-up]");
    if (up !== null) {
      const goUp = (event: Event): void => {
        event.stopPropagation();
        void this.run(() => this.model.openDirectory(this.model.parentDir()), "close", "first-row");
      };
      up.addEventListener("click", goUp);
      up.addEventListener("keydown", (event) => {
        if (event.key !== "Enter" && event.key !== " ") return;
        event.preventDefault();
        goUp(event);
      });
    }
    for (const sort of this.container.querySelectorAll<HTMLElement>("[data-files-sort]")) {
      sort.addEventListener("click", (event) => {
        event.stopPropagation();
        const field = sort.dataset.sortField as SortField;
        if (field === undefined) return;
        this.model.setSort(field);
        this.render();
      });
    }
    this.bindDropTarget();
    const input = q<HTMLInputElement>("[data-files-input]");
    q<HTMLElement>("[data-files-changes]")?.addEventListener("click", (event) => {
      event.stopPropagation();
      void this.run(() => this.model.toggleViewerMode(), "back", "back");
    });
    q<HTMLElement>("[data-files-wrap]")?.addEventListener("click", (event) => {
      event.stopPropagation();
      this.wrapLines = !this.wrapLines;
      writeWrap(this.wrapLines);
      this.render();
    });
    for (const button of this.container.querySelectorAll<HTMLElement>("[data-files-zoom]")) {
      button.addEventListener("click", (event) => {
        event.stopPropagation();
        const next = button.dataset.filesZoom === "actual" ? "actual" : "fit";
        if (next === this.zoom) return;
        this.zoom = next;
        this.render();
      });
    }
    q<HTMLElement>("[data-files-refresh-list]")?.addEventListener("click", (event) => {
      event.stopPropagation();
      void this.run(() => this.model.refreshGone(), "refresh-list", "first-row");
    });
    for (const uploadBtn of this.container.querySelectorAll<HTMLElement>("[data-files-upload], [data-files-upload-header]")) {
      uploadBtn.addEventListener("click", (event) => {
        event.stopPropagation();
        input?.click();
      });
    }
    input?.addEventListener("change", () => {
      const files = Array.from(input.files ?? []);
      // Clearing the input immediately lets the same file be re-picked,
      // and leaves nothing selected to re-submit.
      input.value = "";
      if (files.length === 0) return;
      void this.run(() => this.model.upload(files), null, null);
    });
    q<HTMLElement>("[data-files-back]")?.addEventListener("click", (event) => {
      event.stopPropagation();
      const selectedPath = this.model.selected?.path ?? "";
      this.pendingFocusKey = `row:${selectedPath}`;
      this.model.closeViewer();
      this.render();
    });
    q<HTMLElement>("[data-files-expand]")?.addEventListener("click", (event) => {
      event.stopPropagation();
      this.model.toggleExpanded();
      this.render();
    });
    for (const close of this.container.querySelectorAll<HTMLElement>("[data-files-close]")) {
      close.addEventListener("click", (event) => {
        event.stopPropagation();
        this.cb.onClose?.();
      });
    }
  }

  // modalsRegion is the workspace-level mount for the panel's dialogs.
  // The docked panel cannot host them: its transform makes it a stacking
  // context, so a scrim inside it dims the panel and the dialog centers
  // inside the panel instead of the screen.
  private modalsRegion(): HTMLElement | null {
    if (this.disposed || typeof document === "undefined") return null;
    return this.container.closest("[data-workspace]")
      ?.querySelector<HTMLElement>('[data-region="modals"]') ?? null;
  }

  /** Byte events patch only the affected row and open details. */
  progress(record: UploadRecord): void {
    if (this.disposed) return;
    const info = uploadInfo(record);
    const row = this.container.querySelector<HTMLElement>(`[data-files-upload-row="${record.id}"]`);
    const select = row?.querySelector<HTMLElement>("[data-files-upload-select]");
    select?.setAttribute("aria-label", `${record.name}, ${info.state}`);
    select?.setAttribute("data-hint", `${record.name} · ${info.amount} · ${info.state}`);
    const meta = row?.querySelector<HTMLElement>("[data-files-upload-meta]");
    if (meta) meta.textContent = info.meta;
    const icon = row?.querySelector<HTMLElement>("[data-files-upload-icon]");
    if (icon) icon.innerHTML = uploadRing(record);
    for (const root of [this.container, this.uploadSlot.element]) {
      const detail = root?.querySelector<HTMLElement>(`[data-files-upload-details="${record.id}"]`);
      if (!detail) continue;
      const state = detail.querySelector<HTMLElement>("[data-files-upload-state]");
      if (state) state.textContent = info.state;
      const amount = detail.querySelector<HTMLElement>("[data-files-upload-amount]");
      if (amount) amount.textContent = info.amount;
      const note = detail.querySelector<HTMLElement>("[data-files-upload-note]");
      if (note) note.textContent = info.note;
      const bar = detail.querySelector<HTMLElement>("[data-files-upload-progress]");
      if (bar) {
        bar.hidden = record.total === 0;
        bar.setAttribute("aria-valuenow", String(record.sent));
        bar.setAttribute("aria-valuemax", String(Math.max(1, record.total)));
        const fill = bar.firstElementChild as HTMLElement | null;
        if (fill) fill.style.width = `${info.percent}%`;
      }
      if (detail.dataset.status !== record.status) {
        detail.dataset.status = record.status;
        const announcement = detail.querySelector<HTMLElement>("[data-files-upload-announcement]");
        if (announcement) announcement.textContent = info.state.split(" · ")[0]!;
      }
    }
  }

  private closeUploadDetails(): void {
    this.pendingFocusKey = `upload:${this.model.selectedUpload?.id}`;
    this.model.selectUpload(null);
    this.render();
  }

  private bindUploadControls(root: HTMLElement): void {
    root.querySelector<HTMLElement>("[data-files-upload-summary]")?.addEventListener("click", () => {
      const record = this.model.uploadRecords.find((record) => ["uploading", "confirming", "unconfirmed", "failed"].includes(record.status)) ?? this.model.uploadRecords[0];
      if (record) {
        this.model.selectUpload(record);
        this.render();
      }
    });
    for (const button of root.querySelectorAll<HTMLElement>("[data-files-upload-select]")) {
      button.addEventListener("click", () => {
        const record = this.model.uploadRecords.find((record) => String(record.id) === button.dataset.filesUploadSelect);
        if (!record) return;
        this.model.selectUpload(record);
        this.render();
      });
    }
    for (const button of root.querySelectorAll<HTMLElement>("[data-files-upload-action]")) {
      button.addEventListener("click", (event) => {
        event.stopPropagation();
        const record = this.model.uploadRecords.find((record) => String(record.id) === button.dataset.filesUploadAction);
        if (!record) return;
        if (button.dataset.action === "dismiss") {
          this.pendingFocusKey = "upload";
          this.model.dismissUpload(record);
        } else {
          this.pendingFocusKey = this.model.selectedUpload === record
            ? `upload-state:${record.id}` : `upload:${record.id}`;
          this.model.cancelUpload(record);
        }
        this.render();
      });
    }
    for (const button of root.querySelectorAll<HTMLElement>("[data-files-upload-close]")) {
      button.addEventListener("click", () => this.closeUploadDetails());
    }
  }

  private renderUploadSheet(): void {
    if (typeof document === "undefined") return;
    const narrow = this.uploadMedia?.matches === true || this.container.closest('[data-viewport="narrow"]') !== null;
    const record = this.model.selectedUpload;
    const visible = narrow && record !== null && (this.model.conflict === null || this.model.confirming);
    if (!visible) {
      this.uploadSlot.close();
      this.uploadSheetWasOpen = false;
      return;
    }
    const open = this.uploadSlot.element;
    const active = open !== null && document.activeElement instanceof HTMLElement && open.contains(document.activeElement)
      ? document.activeElement.dataset.focusKey : undefined;
    const host = this.uploadSlot.render(uploadDetailsHtml(record, this.model.rootLabel(), true))!;
    this.bindUploadControls(host);
    host.onkeydown = (event) => {
      if (event.key === "Escape") {
        event.preventDefault();
        event.stopPropagation();
        this.closeUploadDetails();
      } else if (event.key === "Tab") {
        const buttons = [...host.querySelectorAll<HTMLButtonElement>("button:not([disabled])")];
        const index = buttons.indexOf(document.activeElement as HTMLButtonElement);
        if (event.shiftKey && index <= 0) {
          event.preventDefault();
          buttons.at(-1)?.focus();
        } else if (!event.shiftKey && (index === -1 || index === buttons.length - 1)) {
          event.preventDefault();
          buttons[0]?.focus();
        }
      }
    };
    const focus = [...host.querySelectorAll<HTMLElement>("[data-focus-key]")].find((element) => element.dataset.focusKey === (this.pendingFocusKey ?? active))
      ?? host.querySelector<HTMLElement>("[data-files-upload-close]");
    focus?.focus({ preventScroll: true });
    this.pendingFocusKey = null;
    this.uploadSheetWasOpen = true;
  }

  // conflictRoot is where the ask's markup lives: its modal screen when
  // one is open, the panel itself otherwise.
  private conflictRoot(): HTMLElement {
    return this.conflictSlot.element ?? this.container;
  }

  // renderConflict draws the ask on its mount. In the workspace it owns a
  // modal screen and closes it when no ask stands; on the fallback mount
  // the panel render above already drew (or dropped) it.
  private renderConflict(): void {
    if (this.disposed) return;
    if (this.modalsRegion() === null) {
      // Fallback mount: the panel render above drew the ask inline; it
      // still needs its bindings.
      if (this.model.conflict !== null) this.bindConflictDialog(this.container);
      return;
    }
    const markup = this.conflictDialogHtml();
    if (markup === "") {
      this.conflictSlot.close();
      return;
    }
    const open = this.conflictSlot.element;
    const active = open !== null && document.activeElement instanceof HTMLElement && open.contains(document.activeElement)
      ? document.activeElement : null;
    const focusAttribute = ["data-files-conflict-name", "data-files-conflict-same", "data-files-conflict-close",
      "data-files-conflict-cancel", "data-files-conflict-upload-as", "data-files-conflict-replace"]
      .find((attribute) => active?.hasAttribute(attribute));
    const selection = active instanceof HTMLInputElement && active.type === "text"
      ? [active.selectionStart, active.selectionEnd] as const : null;
    const root = this.conflictSlot.render(markup)!;
    this.bindConflictDialog(root);
    if (focusAttribute !== undefined) {
      const control = root.querySelector<HTMLElement>(`[${focusAttribute}]`);
      control?.focus({ preventScroll: true });
      if (selection !== null && control instanceof HTMLInputElement) {
        control.setSelectionRange(selection[0], selection[1]);
      }
    }
  }

  // conflictDialogHtml is the ask for the file the server refused to
  // place: a warning head with an explanatory sentence, the
  // folder's copy and the uploading copy in two aligned columns, an
  // editable alternative name, and the answers — Cancel, the alternative
  // as a secondary outline, Replace as the destructive one, last. For a
  // folder or a non-regular file Replace is absent entirely; on the phone
  // the sheet orders Replace, the alternative, then a ghost Cancel.
  private conflictDialogHtml(): string {
    const m = this.model;
    const ask = m.conflict;
    if (ask === null || m.confirming) return "";
    const incoming = m.conflictIncomingName();
    const existing = ask.info.existing;
    const position = m.conflictPosition();
    const since = formatSince(existing.mtime, Date.now());
    const existingMeta = existing.dir
      ? "a folder"
      : ask.info.reason === "not_a_file"
        ? "not a regular file"
        : `${formatFileSize(existing.size)} · modified ${since ?? "recently"} ago`;
    const reconfirmed = ask.reconfirmed
      ? " The folder's copy changed since you approved replacing it — choose again with these facts."
      : "";
    const remaining = m.conflictRemaining();
    const sameChoice = remaining === 0
      ? ""
      : `
        <label class="flex cursor-pointer items-center gap-2 text-body-sm text-text-secondary">
          <input type="checkbox" data-files-conflict-same ${m.sameChoice ? "checked" : ""} ${m.confirming ? "disabled" : ""}
            class="flex-none" data-hint="${hints.SAME_CHOICE_FOR_REMAINING_UPLOADS(remaining)}">
          <span>Same choice for the other ${remaining} file${remaining === 1 ? "" : "s"}</span>
        </label>`;
    const replaceable = !existing.dir && ask.info.reason !== "not_a_file";
    const busy = m.confirming ? "disabled" : "";
    const dim = m.confirming ? " opacity-45" : "";
    const replace = replaceable
      ? `
        <button type="button" data-files-conflict-replace data-focus-key="conflict-replace" data-hint="${hints.REPLACE_THE_EXISTING_FILE}"
          ${busy} class="h-9 rounded-control bg-error px-(--control-px) text-(length:--type-button-size) font-medium leading-none tracking-(--type-button-ls) text-text-on-error transition-colors hover:opacity-90 pointer-coarse:min-h-11${dim}">Replace</button>`
      : "";
    return `
        <div data-files-conflict role="alertdialog" aria-modal="true"
          aria-labelledby="files-conflict-title" aria-describedby="files-conflict-desc files-conflict-explanation" class="dialog">
          <header class="dialog-head">
            <span aria-hidden="true" data-files-conflict-icon class="mt-1 flex-none text-warning-on-tint">${lucideIcon("triangle-alert", 17)}</span>
            <div class="min-w-0 flex-1">
              <h2 id="files-conflict-title" class="dialog-title dialog-title-prompt">${escapeHtml(incoming)} already exists</h2>
              <p id="files-conflict-desc" class="dialog-desc">in ${escapeHtml(m.conflictDirLabel())}/ · ${position.current} of ${position.total}${reconfirmed}</p>
            </div>
            <button type="button" data-files-conflict-close data-dialog-close aria-label="Close" data-hint="${hints.CLOSE_THIS_DIALOG}" ${busy}
              class="ml-auto ${DIALOG_ICON_BUTTON}${dim}">${lucideIcon("x", 16)}</button>
          </header>
          <div class="dialog-body">
            <p id="files-conflict-explanation" class="text-body-sm text-text-secondary">Replace it with the file you're uploading, or upload yours under another name.</p>
            <div class="grid grid-cols-[auto_minmax(0,1fr)] items-baseline gap-x-3 gap-y-1 rounded-control border border-field-border bg-surface-sunken px-3 py-2.5 text-body-sm">
              <span class="text-text-tertiary">In the folder</span>
              <span class="min-w-0 truncate text-text-primary">${escapeHtml(existingMeta)}</span>
              <span class="text-text-tertiary">Uploading</span>
              <span class="min-w-0 truncate text-text-primary">${formatFileSize(ask.record.file.size)} · from your computer</span>
            </div>
            <div class="flex flex-col gap-1.5">
              <label for="files-conflict-name" class="eyebrow text-text-secondary">Or upload as</label>
              <input id="files-conflict-name" data-files-conflict-name type="text" value="${escapeHtml(m.conflictName)}"
                ${busy} autocomplete="off" spellcheck="false"
                class="font-input h-9 w-full rounded-control border border-border-strong bg-surface-card px-3 text-[length:var(--type-input-size)] text-text-primary sm:text-body-sm">
              <p data-files-conflict-name-error class="text-caption text-error-on-tint">${escapeHtml(m.conflictNameError ?? "")}</p>
            </div>
            ${sameChoice}
          </div>
          <div class="dialog-foot">
            <button type="button" data-files-conflict-cancel data-focus-key="conflict-cancel" data-hint="${hints.CANCEL_THIS_UPLOAD}"
              ${busy} class="${SECONDARY_MD_BUTTON}${dim}">Cancel</button>
            <button type="button" data-files-conflict-upload-as data-focus-key="conflict-upload-as" data-hint="${hints.UPLOAD_UNDER_EDITED_NAME}"
              ${busy} class="${SECONDARY_MD_BUTTON}${dim}">Upload as <span data-files-conflict-upload-as-name>${escapeHtml(m.conflictName)}</span></button>
            ${replace}
          </div>
        </div>
    `;
  }

  // focusConflictField puts the keyboard in the alternative-name field
  // with its base selected, so typing a new base keeps the extension the
  // incoming file arrived with.
  private focusConflictField(): void {
    if (typeof document === "undefined") return;
    const field = this.conflictRoot().querySelector<HTMLInputElement>("[data-files-conflict-name]");
    if (field === null) return;
    field.focus();
    const { base } = splitFileName(field.value);
    field.setSelectionRange(0, base.length);
  }

  // syncConflictField mirrors the field's own edits into the error line
  // and the confirm button's label without a re-render, so typing never
  // loses the caret.
  private syncConflictField(): void {
    const root = this.conflictRoot();
    const error = root.querySelector<HTMLElement>("[data-files-conflict-name-error]");
    if (error !== null) error.textContent = this.model.conflictNameError ?? "";
    const label = root.querySelector<HTMLElement>("[data-files-conflict-upload-as-name]");
    if (label !== null) label.textContent = this.model.conflictName;
    const confirm = root.querySelector<HTMLButtonElement>("[data-files-conflict-upload-as]");
    if (confirm !== null) {
      confirm.disabled = this.model.conflictName.trim() === "" || this.model.conflictNameError !== null;
    }
  }

  // bindConflictDialog wires the ask on its mount. Escape, the scrim, and
  // the close control cancel the CURRENT file only — never the rest,
  // never a replacement — Enter in the field confirms the alternative
  // name, and Tab stays inside the dialog because it is modal.
  private bindConflictDialog(root: HTMLElement): void {
    this.syncConflictActionOrder();
    const q = <T extends Element>(selector: string): T | null => root.querySelector<T>(selector);
    const dialog = q<HTMLElement>("[data-files-conflict]");
    if (dialog === null) return;
    const m = this.model;
    const field = q<HTMLInputElement>("[data-files-conflict-name]");
    const dismiss = (): void => this.dismissConflict();
    dialog.addEventListener("keydown", (event) => {
      if (event.key === "Escape") {
        event.stopPropagation();
        dismiss();
        return;
      }
      if (event.key === "Enter" && event.target === field) {
        event.preventDefault();
        void this.runConflictAnswer(() => m.confirmUploadAs());
        return;
      }
      if (event.key === "Tab") {
        const focusable = [...dialog.querySelectorAll<HTMLElement>("button:not([disabled]), input:not([disabled])")];
        if (focusable.length === 0) return;
        const first = focusable[0]!;
        const last = focusable[focusable.length - 1]!;
        const active = document.activeElement;
        if (event.shiftKey && (active === first || active === dialog)) {
          event.preventDefault();
          last.focus();
        } else if (!event.shiftKey && (active === last || active === dialog)) {
          event.preventDefault();
          first.focus();
        }
      }
    });
    q<HTMLElement>("[data-files-conflict-close]")?.addEventListener("click", (event) => {
      event.stopPropagation();
      dismiss();
    });
    field?.addEventListener("input", () => {
      m.setConflictName(field.value);
      this.syncConflictField();
    });
    q<HTMLInputElement>("[data-files-conflict-same]")?.addEventListener("change", (event) => {
      m.sameChoice = event.target instanceof HTMLInputElement ? event.target.checked : false;
    });
    q<HTMLElement>("[data-files-conflict-cancel]")?.addEventListener("click", dismiss);
    q<HTMLElement>("[data-files-conflict-upload-as]")?.addEventListener("click", () => {
      void this.runConflictAnswer(() => m.confirmUploadAs());
    });
    q<HTMLElement>("[data-files-conflict-replace]")?.addEventListener("click", () => {
      void this.runConflictAnswer(() => m.replaceConflict());
    });
    this.syncConflictField();
  }

  // dismissConflict cancels the current file only, never the rest and
  // never a replacement.
  private dismissConflict(): void {
    if (this.model.confirming) return;
    this.model.cancelConflict();
    this.render();
  }

  // runConflictAnswer carries one answer and re-renders the panel when it
  // settles. The model's own guards make a second click while one is in
  // flight a no-op.
  private async runConflictAnswer(answer: () => Promise<void>): Promise<void> {
    if (this.disposed) return;
    await answer();
    if (!this.disposed) this.render();
  }

  // bindDropTarget makes the whole list column an upload drop target. The
  // overlay is toggled by data-offset-less enter and leave pairs counted on
  // the column itself, so a drag across its children never flickers it.
  // preventDefault on dragover is what tells the browser this drop is
  // accepted; without it the file would navigate the tab instead.
  private bindDropTarget(): void {
    const column = this.container.querySelector<HTMLElement>("[data-files-list]");
    if (column === null) return;
    const overlay = this.container.querySelector<HTMLElement>("[data-files-drop-overlay]");
    const setDriving = (active: boolean): void => {
      if (overlay === null) return;
      if (active) overlay.hidden = false;
      else { overlay.hidden = true; this.dragCount = 0; }
    };
    const isFileDrag = (event: DragEvent): boolean =>
      Array.from(event.dataTransfer?.types ?? []).includes("Files");
    column.addEventListener("dragenter", (event) => {
      if (!isFileDrag(event)) return;
      event.preventDefault();
      this.dragCount += 1;
      setDriving(true);
    });
    column.addEventListener("dragover", (event) => {
      if (!isFileDrag(event)) return;
      event.preventDefault();
    });
    column.addEventListener("dragleave", () => {
      if (this.dragCount > 0) this.dragCount -= 1;
      if (this.dragCount === 0) setDriving(false);
    });
    column.addEventListener("drop", (event) => {
      if (!isFileDrag(event)) return;
      event.preventDefault();
      setDriving(false);
      const files = Array.from(event.dataTransfer?.files ?? []);
      if (files.length === 0 || this.model.uploading) return;
      void this.run(() => this.model.upload(files), null, null);
    });
  }

  // watchCrumbWidth resets the trail on a real width change of the PANEL,
  // which is what restores dropped segments when the side panel closes.
  //
  // The observer is created once, on the container — the one node that
  // survives every render. Observing the nav instead means re-observing a
  // fresh node after every render, and every fresh observe() fires an
  // initial callback; with an unconditional reset in the callback that is
  // a render for every render, forever — a re-render storm that makes
  // browser probes read styles off nodes that are being replaced. Hence
  // also the guard: a callback on an un-elided trail does nothing.
  private watchCrumbWidth(): void {
    if (typeof ResizeObserver === "undefined") return;
    this.crumbRO = new ResizeObserver(() => {
      // Every width change re-renders and re-measures, including one that
      // arrives while the whole trail is showing: that is the case where the
      // header has just become too narrow and the fit loop has to start. An
      // early return for "nothing is elided yet" left the trail overflowing
      // for the rest of the session, and only a fresh page load recovered it.
      this.resetCrumbTrail();
      this.render();
    });
    this.crumbRO.observe(this.container);
    if (typeof document === "undefined" || document.fonts == null) return;
    // A late font swap changes every crumb's width without moving the
    // panel, so no observer ever fires; reset once when the real face is
    // in and let the fit loop re-measure with true metrics.
    void document.fonts.ready.then(() => {
      if (this.disposed) return;
      this.resetCrumbTrail();
      this.render();
    });
  }

  // resetCrumbTrail returns the trail to "show everything": whole segments,
  // the root carrying its project name, the ellipsis if segments were elided,
  // and no remembered give-up width.
  private resetCrumbTrail(): void {
    this.crumbTail = null;
    this.crumbRootLabel = true;
    this.crumbElision = true;
    this.crumbRoot = true;
    this.crumbFitAt = 0;
  }

  // measureCrumbs is one pass of the elision loop. It runs after paint;
  // if the nav overflows its header it gives up one piece and re-renders,
  // so the next pass re-measures. Growth only happens on a real width
  // increase (handled by the ResizeObserver), so this cannot oscillate.
  // The give-way order is: intermediate segments first, then the root's
  // text label (its icon always stays), and finally the current crumb
  // ellipsises inside the remaining space rather than overflowing the nav.
  // measureCrumbs fits the trail to the header in one pass. Each piece it
  // gives up changes what the next measurement sees, so it re-renders and
  // re-measures in place rather than once per animation frame: a frame-by-frame
  // loop leaves the trail overflowing for however many frames it needs, and if
  // anything interrupts the chain it stays that way.
  //
  // Give-way order, tightest last: intermediate segments, then the root's
  // label, then the ellipsis standing in for them, then the root itself. The
  // current crumb ellipsises inside its own box and is never given up - it is
  // the one thing the trail has to say.
  private measureCrumbs(): void {
    if (typeof document === "undefined") return;
    let nav = this.container.querySelector<HTMLElement>("[data-files-crumbs]");
    if (nav === null || nav.getClientRects().length === 0) return;

    // Growing back: the trail was elided at a narrower header than this one.
    if ((this.crumbTail !== null || !this.crumbRootLabel || !this.crumbElision || !this.crumbRoot)
        && nav.clientWidth > this.crumbFitAt + 4) {
      this.resetCrumbTrail();
      this.render();
      nav = this.container.querySelector<HTMLElement>("[data-files-crumbs]");
      if (nav === null || nav.getClientRects().length === 0) return;
    }

    const doesNotFit = (el: HTMLElement): boolean => {
      if (el.scrollWidth > el.clientWidth) return true;
      return [...el.querySelectorAll<HTMLElement>(".crumb-item")].some((item) => {
        if (item.classList.contains("crumb-current") || item.classList.contains("crumb-ellipsis")) {
          return false;
        }
        const label = item.querySelector<HTMLElement>(".crumb-label");
        return label !== null && label.scrollWidth > label.clientWidth + 1;
      });
    };

    // One step per turn of the loop, bounded by the number of pieces there are
    // to give up, so it terminates whatever the widths are.
    for (let guard = 0; guard < 64 && doesNotFit(nav); guard++) {
      const segments = this.model.dir === "" ? 0 : this.model.dir.split("/").length;
      const tail = this.crumbTail ?? segments;
      this.crumbFitAt = nav.clientWidth;
      if (tail > 1) this.crumbTail = tail - 1;
      else if (this.crumbRootLabel) this.crumbRootLabel = false;
      else if (this.crumbElision) this.crumbElision = false;
      else if (this.crumbRoot && segments > 0) this.crumbRoot = false;
      else break;
      this.render();
      nav = this.container.querySelector<HTMLElement>("[data-files-crumbs]");
      if (nav === null || nav.getClientRects().length === 0) return;
    }
  }

  private async run(
    start: () => Promise<void>,
    pendingFocus: string | null,
    completionFocus: string | null,
  ): Promise<void> {
    this.pendingFocusKey = pendingFocus;
    const work = start();
    this.render();
    await Promise.resolve();
    if (this.disposed) return;
    this.restoreFocus(pendingFocus);
    await work;
    if (this.disposed) return;
    this.pendingFocusKey = completionFocus;
    this.render();
    if (this.pendingFocusKey !== null) {
      const key = this.pendingFocusKey;
      this.pendingFocusKey = null;
      if (!this.restoreFocus(key)) {
        this.restoreFocus("close");
      }
    }
  }

  private captureScroll(): void {
    const list = this.container.querySelector<HTMLElement>("[data-files-list-scroll]");
    if (list !== null && list.getClientRects().length > 0) {
      this.listScroll.set(list.dataset.filesDir ?? "", {
        top: list.scrollTop,
        left: list.scrollLeft,
      });
    }
    const viewer = this.container.querySelector<HTMLElement>("[data-files-viewer-scroll]");
    if (viewer !== null && viewer.getClientRects().length > 0) {
      this.viewerScroll.set(viewer.dataset.filesPath ?? "", {
        top: viewer.scrollTop,
        left: viewer.scrollLeft,
      });
    }
  }

  private restoreScroll(): void {
    const list = this.container.querySelector<HTMLElement>("[data-files-list-scroll]");
    const listPosition = list === null ? undefined : this.listScroll.get(list.dataset.filesDir ?? "");
    if (list !== null && listPosition !== undefined) {
      list.scrollTop = listPosition.top;
      list.scrollLeft = listPosition.left;
    }
    const viewer = this.container.querySelector<HTMLElement>("[data-files-viewer-scroll]");
    const viewerPosition = viewer === null
      ? undefined
      : this.viewerScroll.get(viewer.dataset.filesPath ?? "");
    if (viewer !== null && viewerPosition !== undefined) {
      viewer.scrollTop = viewerPosition.top;
      viewer.scrollLeft = viewerPosition.left;
    }
  }

  // Stable keys restore focus after a render replaces the panel's nodes.
  private activeFocusKey(): string | null {
    if (typeof document === "undefined") return null;
    const active = document.activeElement;
    if (active === null || !(active instanceof HTMLElement)) return null;
    if (!this.container.contains(active) && this.uploadSlot.element?.contains(active) !== true) return null;
    return active.dataset.focusKey ?? null;
  }

  private restoreFocus(key: string | null): boolean {
    if (key === null || typeof document === "undefined") return false;
    const candidate = key === "first-row"
      ? this.container.querySelector<HTMLElement>("[data-files-row]")
      : [...this.container.querySelectorAll<HTMLElement>("[data-focus-key]")]
        .find((el) => el.dataset.focusKey === key) ?? null;
    if (candidate === null || candidate.getClientRects().length === 0) return false;
    candidate.focus({ preventScroll: true });
    return document.activeElement === candidate;
  }
}

// hasAlpha reports whether a decoded image has any pixel that is not fully
// opaque, read from a copy at most 64px on its long side. A canvas that
// cannot be read back counts as opaque: the checkerboard is a claim about
// the file, and an unread file makes no claim.
function hasAlpha(img: HTMLImageElement): boolean {
  const scale = Math.min(1, 64 / Math.max(img.naturalWidth, img.naturalHeight, 1));
  const width = Math.max(1, Math.round(img.naturalWidth * scale));
  const height = Math.max(1, Math.round(img.naturalHeight * scale));
  try {
    const canvas = document.createElement("canvas");
    canvas.width = width;
    canvas.height = height;
    const context = canvas.getContext("2d", { willReadFrequently: true });
    if (context === null) return false;
    context.drawImage(img, 0, 0, width, height);
    const pixels = context.getImageData(0, 0, width, height).data;
    for (let i = 3; i < pixels.length; i += 4) {
      if (pixels[i]! < 255) return true;
    }
    return false;
  } catch {
    return false;
  }
}
