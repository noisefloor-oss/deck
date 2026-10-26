// The Taskmaster panel's DOM shell: renders a TaskmasterPanelModel into
// the docked Tasks panel and forwards its events. All state lives in the
// model (and ultimately on the server); this file only draws and listens.
//
// Layout is the tasks board: a chrome-bar header, a
// toolbar (search · sort segments · filters), three unboxed desktop
// columns or one selected mobile status lane, and fixed 132px cards
// (id + priority marker, clamped title, spacer, deps, progress). A
// task's description lives in the detail dialog, never on the card. Add
// PRD and task creation are centered modals launched from the panel
// header; there is no footer bar.
// The literal pixel values are deliberate.
//
import { priorityDotClass, priorityLabel, taskID, taskStatusFacts } from "../../lib/task-facts.ts";
import type { AddTaskInput, TmTask } from "../../api/tasks.ts";
import { DIALOG_BODY, DIALOG_FOOT, ICON_BUTTON, SECONDARY_SM_BUTTON, SELECT_FIELD, dialogHead, selectShellHtml } from "../../ui/controls.ts";
import { activateDialog } from "../../ui/dialog-focus.ts";
import { ModalSlot, returnFocus } from "../../ui/modal-host.ts";
import { editedModalFields, focusedModalControl, restoreModalEdits, restoreModalFocus, type ModalFocus } from "../../ui/modal-fields.ts";
import { escapeHtml } from "../../lib/escape.ts";
import { formatFileSize } from "../../lib/format.ts";
import { lucideIcon } from "../../ui/icons.ts";
import { renderMarkdown } from "../../ui/markdown.ts";
import {
  TASK_POPOVER_CLASS,
  taskCardPopoverHtml,
  taskDependenciesHtml,
  TASK_REF_ATTR,
} from "../../ui/task-card.ts";
import { collectTaskIDs, resolveTask } from "../../ui/task-ref.ts";
import * as hints from "../../lib/hints.ts";
import {
  boardTasks,
  kanbanColumns,
  PRD_CANONICAL_NAME,
  type BoardSortKey,
  type TaskmasterPanelModel,
} from "./taskmaster-panel.ts";

// One scheduler owns both live canonical refreshes and generation polls.
// Active generation uses a 2s cadence; an idle board uses a quieter bound
// while it remains open.
const runPollMS = 2000;
const taskRefreshMS = 5000;
const taskRefHoverMS = 350;

export interface TaskmasterPanelTimer {
  set(callback: () => void | Promise<void>, delayMS: number): number;
  clear(id: number): void;
}

const browserTimer: TaskmasterPanelTimer = {
  set: (callback, delayMS) => window.setTimeout(callback, delayMS),
  clear: (id) => window.clearTimeout(id),
};

// PickedFile is a chosen PRD read into memory, ready to upload under the
// canonical name regardless of its original filename.
interface PickedFile {
  name: string;
  size: number;
  content: string;
}

// PRIORITY_FILTERS is the Filters popover's row set, in rank order.
const PRIORITY_FILTERS = [
  ["high", "High"],
  ["medium", "Medium"],
  ["low", "Low"],
  ["none", "No priority"],
] as const;

export interface TaskmasterPanelShellCallbacks {
  // onClose is the panel's own ✕: the shell does not own the docked
  // frame's visibility, the workspace does.
  onClose?: () => void;
}

export class TaskmasterPanel {
  private container: HTMLElement;
  private model: TaskmasterPanelModel;
  private cb: TaskmasterPanelShellCallbacks;
  private timer: TaskmasterPanelTimer;
  private pollTimer: number | null = null;
  private disposed = false;
  private visible = false;
  // addPrdOpen shows the centered Add-PRD modal over the board.
  private addPrdOpen = false;
  private addTaskOpen = false;
  // detailTaskID is the card whose detail dialog is open, if any.
  private detailTaskID: string | null = null;
  private detailTask: TmTask | null = null;
  private detailError: string | null = null;
  private detailGeneration = 0;
  private taskRefHoverTimer: number | null = null;
  private taskPopover: HTMLElement | null = null;
  private taskPopoverAnchor: HTMLElement | null = null;
  private knownTaskTags: [string, TmTask[]][] | null = null;
  private knownTaskIDs: Set<string> = new Set();
  private visualViewport: VisualViewport | null = null;
  private onViewportResize = (): void => {
    this.cancelTaskHover();
    this.hideTaskPopover();
  };
  // Toolbar state (display-only; the canonical order never mutates).
  private searchQuery = "";
  private sortKey: BoardSortKey = "id";
  private sortDesc = false;
  private filtersOpen = false;
  private hiddenPriorities = new Set<string>();
  // pickedFile holds the user's selection inside the add-PRD flow; the
  // confirm step gates a replace when prd.md already exists.
  private pickedFile: PickedFile | null = null;
  private confirmReplace = false;
  private uploading = false;
  private generation = 0;
  private dialogReturnSelector: string | null = null;
  private dialogFocusCleanup: (() => void) | null = null;
  // Dialogs are a screen of the workspace's one modal, outside the docked
  // panel so its stacking context cannot cap them below the popover host.
  private readonly modalSlot = new ModalSlot(() => this.container.closest("[data-workspace]")
    ?.querySelector<HTMLElement>('[data-region="modals"]') ?? this.container, { dismiss: () => this.dismissModal() });
  private mobileStatus = "To do";
  private columnScrollTops = new Map<string, number>();

  constructor(
    container: HTMLElement,
    model: TaskmasterPanelModel,
    cb: TaskmasterPanelShellCallbacks = {},
    timer: TaskmasterPanelTimer = browserTimer,
  ) {
    this.container = container;
    this.model = model;
    this.cb = cb;
    this.timer = timer;
    if (typeof window !== "undefined") {
      window.addEventListener("resize", this.onViewportResize);
      this.visualViewport = window.visualViewport;
      this.visualViewport?.addEventListener("resize", this.onViewportResize);
    }
    this.render();
  }

  async open(): Promise<void> {
    if (this.disposed) return;
    this.visible = true;
    ++this.generation;
    this.stopPolling();
    await this.refresh();
  }

  close(): void {
    if (!this.visible) return;
    this.visible = false;
    ++this.generation;
    this.stopPolling();
    this.cancelTaskHover();
    this.hideTaskPopover();
  }

  // refresh re-reads the engine task response and PRD documents, then
  // redraws. The workspace uses it once while mounting to populate the
  // title-bar count, and open() uses it again before starting live observation.
  async refresh(): Promise<void> {
    const generation = this.generation;
    await this.model.refresh();
    if (this.disposed || generation !== this.generation) return;
    this.render();
    this.syncPolling();
  }

  dispose(): void {
    this.disposed = true;
    this.visible = false;
    ++this.generation;
    this.stopPolling();
    if (typeof window !== "undefined") window.removeEventListener("resize", this.onViewportResize);
    this.visualViewport?.removeEventListener("resize", this.onViewportResize);
    this.visualViewport = null;
    this.hideTaskPopover();
    this.cancelTaskHover();
    this.dialogFocusCleanup?.();
    this.dialogFocusCleanup = null;
    this.container.innerHTML = "";
    this.modalSlot.close();
  }

  render(): void {
    if (this.disposed) return;
    this.captureBoardScroll();
    // A dialog hosted in the container loses focus to the rewrite below.
    const modalFocus = focusedModalControl(this.modalSlot.element);
    const m = this.model;
    if (m.state !== "ready") this.stopPolling();
    this.hideTaskPopover();
    this.cancelTaskHover();
    this.container.innerHTML = `
      <div class="relative flex min-h-0 flex-1 flex-col">
        ${this.headerHtml()}
        ${m.state === "ready" ? this.renderBody() : this.renderPlaceholder()}
      </div>
    `;
    this.bindHeader();
    if (m.state === "ready") {
      this.bindToolbar();
      this.bindStatusSelector();
      this.bindCards();
      this.bindTaskRefs();
    }
    this.bindAddPrdEntry();
    this.bindAddTaskEntry();
    this.restoreBoardScroll();
    this.renderModals(modalFocus);
  }

  // A poll re-renders an open dialog, so what you typed, the caret and
  // focus carry over to the rebuilt fields.
  private renderModals(focus: ModalFocus | null): void {
    const edited = editedModalFields(this.modalSlot.element);
    const host = this.modalSlot.render([
      this.addTaskOpen ? this.renderAddTaskModal() : "",
      this.addPrdOpen ? this.renderAddPrdModal() : "",
      this.renderDetailModal(),
    ].join(""));
    restoreModalEdits(host, edited);
    this.bindAddTaskModal();
    this.bindAddPrdModal();
    this.bindDetailModal();
    this.bindDialogFocus(restoreModalFocus(host, focus));
  }

  // The panel header is the shared chrome bar (48px, bottom hairline, no
  // explicit fill) so it reads as one bar with the session header: the
  // title, the two secondary sm actions, and the close icon button.
  private headerHtml(): string {
    return `
      <div data-tm-header class="chrome gap-2.5 pl-(--space-5) pr-(--space-4)">
        <h2 class="chrome-title flex-none">Tasks</h2>
        <span class="ml-auto"></span>
        <button type="button" data-tm-add-prd="header" data-hint="${hints.ADD_PRD_TO_BOARD}"
          class="${SECONDARY_SM_BUTTON} max-md:h-11 max-md:px-3">Add PRD…</button>
        ${this.model.state === "ready" ? `<button type="button" data-tm-add-task="header" aria-label="Add task" data-hint="${hints.ADD_TASK_TO_BOARD}"
          class="${SECONDARY_SM_BUTTON} max-md:h-11 max-md:px-3">${lucideIcon("plus", 13)}<span data-tm-add-task-label>Add task</span></button>` : ""}
        <button type="button" data-tm-close aria-label="Close tasks" data-hint="${hints.CLOSE_TASK_BOARD}"
          class="${ICON_BUTTON} max-md:h-11 max-md:w-11">${lucideIcon("x", 14)}</button>
      </div>
    `;
  }

  // A project with no .taskmaster is the ONBOARDING case: the
  // add-a-PRD affordance must exist here most of all, and it is the
  // header's — the body only explains why the board is empty.
  private renderPlaceholder(): string {
    const m = this.model;
    if (m.state === "loading") {
      return `<div class="p-5 text-body-sm text-text-tertiary">Loading tasks…</div>`;
    }
    return `
      <div class="flex flex-1 flex-col items-center justify-center gap-3 p-5 text-center">
        <p class="text-body-sm text-text-tertiary">No project tasks yet.</p>
        <button type="button" data-tm-add-prd="onboarding" data-hint="${hints.ADD_PRD_TO_BOARD}"
          class="h-8 rounded-control border border-border-strong bg-surface-card px-3 text-body-sm text-text-primary transition-colors hover:bg-wash-hover focus-visible:bg-wash-hover max-md:h-11">Add PRD…</button>
        ${m.generateError === null ? "" : `<div class="rounded-control bg-error-tint px-3 py-2 text-caption text-error-on-tint">${escapeHtml(m.generateError)}</div>`}
      </div>
    `;
  }

  private renderBody(): string {
    const m = this.model;
    const columns = this.resolvedColumns();
    return `
      ${m.tasksError
        ? `<div class="mx-5 mt-3 rounded-control bg-error-tint px-3 py-2 text-caption text-error-on-tint">${escapeHtml(m.tasksError)}</div>`
        : ""}
      ${this.renderToolbar()}
      ${this.renderStatusSelector(columns)}
      <div data-tm-board class="grid min-h-0 flex-1 grid-cols-1 gap-3 overflow-hidden px-3 pb-0 pt-[10px] md:grid-cols-3 md:gap-5 md:px-5">
        ${columns.map((col) => this.renderColumn(col)).join("")}
      </div>
      ${m.generateError
        ? `<div class="mx-5 mb-3 rounded-control bg-error-tint px-3 py-2 text-caption text-error-on-tint">${escapeHtml(m.generateError)}</div>`
        : ""}
    `;
  }

  private resolvedColumns(): { title: string; total: number; shown: TmTask[] }[] {
    const priorities = this.hiddenPriorities.size === 0 ? null : this.visiblePriorities();
    return kanbanColumns(this.model.tags).map(([title, tasks]) => ({
      title,
      total: tasks.length,
      shown: boardTasks(tasks, this.searchQuery, priorities, this.sortKey, this.sortDesc),
    }));
  }

  private countLabel(total: number, shown: number): string {
    return shown === total ? String(total) : `${shown}/${total}`;
  }

  private renderStatusSelector(columns: { title: string; total: number; shown: TmTask[] }[]): string {
    return `
      <div data-tm-status-selector role="group" aria-label="Task status"
        class="flex flex-none gap-2 px-3 pt-[10px] md:hidden">
        ${this.statusSelectorButtonsHtml(columns)}
      </div>
    `;
  }

  // The selector is phone-only chrome (md:hidden), so it wears the
  // segment anatomy directly: sentence-case name, a
  // dimmed interpunct, and a tabular count, on the selected wash — the
  // uppercase eyebrow voice belongs to the desktop columns this replaces.
  private statusSelectorButtonsHtml(columns: { title: string; total: number; shown: TmTask[] }[]): string {
    return columns.map((col) => {
      const pressed = col.title === this.mobileStatus;
      const count = this.countLabel(col.total, col.shown.length);
      // One line, no indentation whitespace: probes anchor ^-regexes on the
      // button's text, and template padding would put a newline before it.
      const label = `<span class="flex items-baseline gap-[5px]">`
        + `<span class="text-body-sm ${pressed ? "font-medium text-text-primary" : "text-text-tertiary"}">${escapeHtml(col.title)}</span>`
        + `<span aria-hidden="true" class="text-body-sm text-text-tertiary opacity-55">·</span>`
        + `<span class="text-body-sm font-medium tabular-nums ${pressed ? "text-text-secondary" : "text-text-tertiary"}">${count}</span>`
        + `</span>`;
      return `<button type="button" data-tm-status="${escapeHtml(col.title)}" aria-pressed="${pressed}" data-hint="${hints.SHOW_TASKS_WITH_STATUS}"
            class="flex min-h-11 flex-1 items-center justify-center rounded-control px-3 ${pressed ? "bg-wash-selected" : "transition-colors hover:bg-wash-hover"} focus-visible:bg-wash-hover">${label}</button>`;
    }).join("");
  }

  // The filter row: search as the field family's sm
  // lead input capped at 420px, then the Sort caption, the sort pills,
  // and the Filter button. The pills carry the files panel's sort-strip
  // contract — the active key takes the selected wash and a direction
  // arrow, re-selecting it flips the direction, and selecting another
  // key restarts ascending — so the two panels' ordering controls read
  // as one system. The row wraps at narrow widths.
  private renderToolbar(): string {
    const seg = (key: BoardSortKey, label: string, field: string): string => {
      const active = this.sortKey === key;
      const arrow = active ? (this.sortDesc ? " ↓" : " ↑") : "";
      return `<button type="button" data-tm-sort="${key}" aria-pressed="${active}" data-hint="${hints.sortTasks(field)}"
          class="tab${active ? " tab-selected" : ""}">${label}${arrow}</button>`;
    };
    const filtering = this.hiddenPriorities.size > 0;
    const runLabel = this.model.activeRun?.status === "running"
      ? `<span data-tm-run class="font-ui text-caption text-text-tertiary">generating tasks…</span>`
      : "";
    return `
      <div data-tm-toolbar class="flex flex-none flex-wrap items-center gap-x-2.5 gap-y-2 px-5 py-3">
        <label class="inputwrap max-w-[420px] flex-1">
          <span aria-hidden="true" class="inputwrap-lead">${lucideIcon("search", 13)}</span>
          <!-- The field text keeps a normal line box. It must not inherit
               the 1.55 body ratio (20.15px on 13px type), and leading-normal
               is no substitute: that is Tailwind's 1.5, a third value. -->
          <input data-tm-search value="${escapeHtml(this.searchQuery)}"
            placeholder="Search tasks…" aria-label="Search tasks" data-hint="${hints.SEARCH_TASKS}"
            autocapitalize="none" autocomplete="off" spellcheck="false"
            class="input input-sm input-lead font-input leading-[normal] max-md:min-h-11">
        </label>
        <span class="ml-auto"></span>
        ${runLabel}
        <!-- 12px on the inherited 1.55 body ratio - 18.6px, not the caption
             token's 1.4 - the same line box the column counts carry. -->
        <span data-tm-sort-caption class="text-caption leading-[1.55] text-text-tertiary">Sort</span>
        <div class="tabs-pill">
          ${seg("id", "ID", "id")}
          ${seg("status", "Status", "status")}
          ${seg("priority", "Priority", "priority")}
        </div>
        <div class="relative">
          <button type="button" data-tm-filters aria-haspopup="true" aria-expanded="${this.filtersOpen}" data-hint="${hints.FILTER_TASKS_BY_PRIORITY}"
            class="${SECONDARY_SM_BUTTON} ${filtering ? "text-accent" : "text-text-primary"} max-md:h-11 max-md:px-3">${lucideIcon("chevron-down", 14)}Filter</button>
          ${this.filtersOpen ? this.renderFiltersPopover() : ""}
        </div>
      </div>
    `;
  }

  // The Filters popover: priority toggles, checked = shown. The DC draws
  // the button; the panel follows the menu recipe (popover surface,
  // wash-hovered rows).
  private renderFiltersPopover(): string {
    const rows = PRIORITY_FILTERS.map(([key, label]) => `
        <label class="flex cursor-pointer items-center gap-2 rounded-control px-2 py-1.5 text-body-sm text-text-primary transition-colors hover:bg-wash-hover max-md:min-h-11" data-hint="${hints.SHOW_TASKS_WITH_PRIORITY}">
          <input data-tm-filter-priority="${key}" type="checkbox" ${this.hiddenPriorities.has(key) ? "" : "checked"}
            class="accent-accent">
          <span>${label}</span>
        </label>`).join("");
    return `<div data-tm-filters-popover class="absolute right-0 top-[calc(100%+6px)] z-20 flex min-w-[180px] flex-col gap-px rounded-panel border border-line-300 bg-surface-card p-2">
        <span class="px-2 pb-1 pt-0.5 font-label text-micro text-text-tertiary">Priority</span>
        ${rows}
      </div>`;
  }

  // A column is UNBOXED — no background, no border, no radius. The
  // eyebrow and the mono count are the only chrome; the cards below
  // scroll on their own.
  private renderColumn(col: { title: string; total: number; shown: TmTask[] }): string {
    const display = col.title === this.mobileStatus ? " flex" : " hidden md:flex";
    return `
      <section data-tm-status="${escapeHtml(col.title)}" class="min-h-0 min-w-0 flex-col gap-3${display}">
        <!-- The column header sets font sizes but no line-height, so
             both spans inherit the body ratio against their own size: the
             count's 12px x 1.55 makes the header 18.6px, which is what puts
             the first card at the intended y. Our size utilities carry tighter
             per-token line-heights, so the ratio is restated here. -->
        <header class="flex flex-none items-baseline gap-2 px-0.5">
          <span data-tm-column-title class="text-micro font-medium uppercase leading-[1.55] tracking-[0.07em] text-text-secondary">${escapeHtml(col.title)}</span>
          <span class="ml-auto font-ui text-caption leading-[1.55] text-text-tertiary">${this.countLabel(col.total, col.shown.length)}</span>
        </header>
        <div data-tm-column-scroll="${escapeHtml(col.title)}" class="flex min-h-0 flex-1 flex-col gap-3 overflow-y-auto overscroll-y-contain pb-4">
          ${col.shown.map((t) => this.renderCard(t)).join("")}
        </div>
      </section>
    `;
  }

  private visiblePriorities(): ReadonlySet<string> {
    const all = new Set<string>(PRIORITY_FILTERS.map(([key]) => key));
    for (const hidden of this.hiddenPriorities) all.delete(hidden);
    return all;
  }

  // The card is the BUTTON: a fixed 132px desktop
  // height with flex:none children so the browser can never shrink the
  // text boxes and slice glyphs. Its anatomy is id + priority marker row,
  // a 2-line clamped title, a dedicated margin-top:auto spacer (zero
  // content height, its own element), the deps line, and the progress
  // row — all at natural height so no fixed-height box clips a line box.
  // The uniform height is desktop-only; mobile cards size to content.
  // Rows render only when present — the full title, description, and
  // state live in the detail dialog. Dependencies render as inert text
  // because the card is a <button>: a nested focusable reference would
  // both be invalid markup and add a keyboard stop; the detail dialog
  // keeps the interactive form.
  private renderCard(t: TmTask): string {
    const deps = taskDependenciesHtml(t.dependencies, this.resolvableTaskIDs(), false);
    const doneCount = t.subtasks_done ?? (t.subtasks ?? []).filter((x) => x.status === "done").length;
    const totalCount = t.subtasks_total ?? t.subtasks?.length ?? 0;
    const progress = t.status === "in-progress" && totalCount > 0
      ? `<span class="h-[3px] flex-1 overflow-hidden rounded-pill bg-wash-hover"><span class="block h-full rounded-pill bg-accent" style="width:${Math.round((100 * doneCount) / totalCount)}%"></span></span>
          <span class="flex-none font-ui text-caption leading-[normal] text-text-tertiary">${doneCount}/${totalCount}</span>`
      : "";
    const id = escapeHtml(taskID(t.id));
    const hasBottomRows = deps !== "" || progress !== "";
    return `
      <button type="button" data-tm-card data-tm-task="${id}" aria-label="Task ${id}: ${escapeHtml(t.title)}" data-hint="${hints.OPEN_TASK_DETAILS}"
        class="flex w-full flex-none flex-col gap-2 rounded-card border border-border bg-surface-card px-[14px] py-3 text-left transition-colors hover:border-border-strong focus-visible:bg-wash-hover md:h-[132px]">
        <!-- The card is a fixed 132px. The rows size to content, and
             a card dense enough to exceed that height spills rather than
             reflowing the column. -->
        <!-- The card is a <button>, so its rows inherit a normal line
             box rather than a token ratio: its id row measures 15px, not the
             16.8px a caption line-height gives. -->
        <span class="flex w-full flex-none items-center leading-[normal] gap-2">
          <span class="flex-none font-mono text-caption leading-[normal] text-text-tertiary">${id}</span>
          <span class="ml-auto"></span>
          ${t.priority
            ? `<span class="flex flex-none items-center gap-[5px]"><span aria-hidden="true" class="h-1.5 w-1.5 flex-none rounded-full ${priorityDotClass(t.priority)}"></span><span class="priority-label text-text-tertiary">${escapeHtml(priorityLabel(t.priority))}</span></span>`
            : ""}
        </span>
        <span data-tm-card-title class="line-clamp-2 w-full flex-none break-words text-body font-medium leading-[1.4] text-text-primary [text-wrap:pretty]">${escapeHtml(t.title)}</span>
        ${hasBottomRows ? `<span aria-hidden="true" data-tm-card-spacer class="mt-auto w-full flex-none"></span>` : ""}
        ${deps === "" ? "" : `<span data-tm-card-dependencies class="w-full flex-none truncate font-ui text-caption text-text-tertiary">depends on ${deps}</span>`}
        ${progress === "" ? "" : `<span data-tm-card-progress class="flex w-full flex-none items-center gap-2">${progress}</span>`}
      </button>
    `;
  }

  // The task detail dialog: where the description (and everything else
  // the card summarizes) actually lives.
  private renderDetailModal(): string {
    if (this.detailTaskID === null) return "";
    const t = this.detailTask;
    if (t === null) {
      return `
          <div data-phone-fullscreen role="dialog" aria-modal="true" aria-label="Task ${escapeHtml(this.detailTaskID)}" class="dialog">
            <header class="dialog-head">
              <h2 class="dialog-title dialog-title-prompt font-mono">${escapeHtml(this.detailTaskID)}</h2>
              <button type="button" data-tm-detail-close data-dialog-close aria-label="Close task" data-hint="${hints.CLOSE_TASK_DETAILS}" class="ml-auto ${ICON_BUTTON}">${lucideIcon("x", 14)}</button>
            </header>
            ${DIALOG_BODY(`
              <p data-tm-detail-state class="text-body-sm ${this.detailError === null ? "text-text-tertiary" : "text-danger"}">${escapeHtml(this.detailError ?? "Loading task…")}</p>
            `)}
          </div>`;
    }
    const deps = taskDependenciesHtml(t.dependencies, this.resolvableTaskIDs());
    const { label, dot } = taskStatusFacts(t);
    const subtasks = (t.subtasks ?? []).map((s) => {
      const facts = taskStatusFacts(s);
      return `<li class="flex items-center gap-2 border-t border-border px-1 py-1.5 first:border-t-0">
          <span aria-hidden="true" class="h-1.5 w-1.5 flex-none rounded-full ${facts.dot}"></span>
          <span class="flex-none font-mono text-micro text-text-tertiary">${escapeHtml(taskID(s.id))}</span>
          <span class="min-w-0 flex-1 truncate text-body-sm text-text-primary">${escapeHtml(s.title)}</span>
          <span class="flex-none text-micro text-text-tertiary">${escapeHtml(facts.label)}</span>
        </li>`;
    }).join("");
    return `
        <div data-phone-fullscreen role="dialog" aria-modal="true" aria-label="Task ${escapeHtml(taskID(t.id))}" class="dialog">
          <header class="dialog-head">
            <div class="min-w-0 flex-1">
              <h2 class="dialog-title dialog-title-prompt">${escapeHtml(t.title)}</h2>
              <p class="dialog-desc font-mono text-micro tracking-normal text-text-tertiary">${escapeHtml(taskID(t.id))}</p>
            </div>
            <button type="button" data-tm-detail-close data-dialog-close aria-label="Close task" data-hint="${hints.CLOSE_TASK_DETAILS}"
              class="ml-auto ${ICON_BUTTON} pointer-coarse:h-11 pointer-coarse:w-11">${lucideIcon("x", 14)}</button>
          </header>
          ${DIALOG_BODY(`
            <div class="flex flex-wrap items-center gap-1.5">
              <span aria-hidden="true" class="h-1.5 w-1.5 flex-none rounded-full ${dot}"></span>
              <span class="text-micro text-text-tertiary">${escapeHtml(label)}</span>
              ${t.priority
                ? `<span class="flex flex-none items-center gap-[5px]"><span aria-hidden="true" class="h-1.5 w-1.5 flex-none rounded-full ${priorityDotClass(t.priority)}"></span><span class="priority-label text-text-tertiary">${escapeHtml(priorityLabel(t.priority))}</span></span>`
                : ""}
              ${deps ? `<span data-task-dependencies class="font-ui text-caption text-text-tertiary">· depends on ${deps}</span>` : ""}
            </div>
            ${t.description
              ? `<div data-task-description class="flex flex-col gap-2">${renderMarkdown(t.description, this.resolvableTaskIDs())}</div>`
              : ""}
            ${t.details
              ? `<div data-task-details class="flex flex-col gap-2">${renderMarkdown(t.details, this.resolvableTaskIDs())}</div>`
              : ""}
            ${subtasks ? `<ul class="rounded-control border border-border px-2 py-1">${subtasks}</ul>` : ""}
          `)}
        </div>
    `;
  }

  private bindHeader(): void {
    this.container.querySelector("[data-tm-close]")?.addEventListener("click", (ev) => {
      ev.stopPropagation();
      this.cb.onClose?.();
    });
  }

  private bindToolbar(): void {
    const search = this.container.querySelector<HTMLInputElement>("[data-tm-search]");
    search?.addEventListener("input", () => {
      this.searchQuery = search.value;
      // Patch the columns in place: replacing the whole panel would drop
      // focus from the field on every keystroke.
      this.repaintColumns();
    });
    for (const btn of this.container.querySelectorAll<HTMLButtonElement>("[data-tm-sort]")) {
      btn.addEventListener("click", () => {
        const key = btn.dataset.tmSort as BoardSortKey;
        if (key === this.sortKey) this.sortDesc = !this.sortDesc;
        else {
          this.sortKey = key;
          this.sortDesc = false;
        }
        this.render();
      });
    }
    this.container.querySelector("[data-tm-filters]")?.addEventListener("click", () => {
      this.filtersOpen = !this.filtersOpen;
      this.render();
    });
    for (const input of this.container.querySelectorAll<HTMLInputElement>("[data-tm-filter-priority]")) {
      input.addEventListener("change", () => {
        const key = input.dataset.tmFilterPriority!;
        if (input.checked) this.hiddenPriorities.delete(key);
        else this.hiddenPriorities.add(key);
        this.repaintColumns();
      });
    }
  }

  private bindStatusSelector(): void {
    for (const btn of this.container.querySelectorAll<HTMLButtonElement>("[data-tm-status-selector] [data-tm-status]")) {
      btn.addEventListener("click", () => {
        const next = btn.dataset.tmStatus;
        if (next === undefined || next === this.mobileStatus) return;
        this.mobileStatus = next;
        this.repaintColumns();
        this.container.querySelector<HTMLElement>(`[data-tm-status-selector] [data-tm-status="${CSS.escape(next)}"]`)
          ?.focus({ preventScroll: true });
      });
    }
  }

  // repaintColumns re-renders only the board grid so toolbar focus (the
  // search field, a filter checkbox) survives the update. The Filters
  // button's active-filter tint is patched alongside — it is the one
  // toolbar cell whose state this path changes.
  private repaintColumns(): void {
    const grid = this.container.querySelector<HTMLElement>("[data-tm-board]");
    if (grid === null) return;
    this.captureBoardScroll();
    const columns = this.resolvedColumns();
    const selector = this.container.querySelector<HTMLElement>("[data-tm-status-selector]");
    if (selector !== null) selector.innerHTML = this.statusSelectorButtonsHtml(columns);
    grid.innerHTML = columns.map((col) => this.renderColumn(col)).join("");
    this.bindStatusSelector();
    this.bindCards();
    this.restoreBoardScroll();
    const filters = this.container.querySelector<HTMLElement>("[data-tm-filters]");
    if (filters !== null) {
      filters.classList.toggle("text-accent", this.hiddenPriorities.size > 0);
      filters.classList.toggle("text-text-primary", this.hiddenPriorities.size === 0);
    }
  }

  private captureBoardScroll(): void {
    for (const column of this.container.querySelectorAll<HTMLElement>("[data-tm-column-scroll]")) {
      const key = column.dataset.tmColumnScroll;
      if (key === undefined) continue;
      if (!Number.isFinite(column.scrollTop)) continue;
      if (typeof column.getClientRects === "function" && column.getClientRects().length === 0) continue;
      const saved = this.columnScrollTops.get(key);
      const maxScrollTop = Math.max(0, column.scrollHeight - column.clientHeight);
      if (saved !== undefined && saved > maxScrollTop && column.scrollTop === maxScrollTop) continue;
      this.columnScrollTops.set(key, column.scrollTop);
    }
  }

  private restoreBoardScroll(): void {
    for (const column of this.container.querySelectorAll<HTMLElement>("[data-tm-column-scroll]")) {
      const key = column.dataset.tmColumnScroll;
      if (key === undefined) continue;
      const scrollTop = this.columnScrollTops.get(key);
      if (scrollTop === undefined) continue;
      column.scrollTop = scrollTop;
      if (column.scrollTop === scrollTop) continue;
      // The assignment did not take: a column the browser cannot measure
      // yet — a freshly written grid, or a phone lane off screen this frame
      // — has no scrollable range, and the position clamps to nothing.
      // captureBoardScroll keeps the saved value through that, so without a
      // re-apply the map remembers the place while the pane shows the top.
      this.reapplyColumnScroll(column, scrollTop, 0);
    }
  }

  // reapplyColumnScroll re-applies a clamped position once the browser can
  // measure the column. It gives up as soon as the assignment sticks, the
  // column leaves the document, the column reports a real scrollable range
  // and still refuses the position — that column has genuinely grown
  // shorter, and the clamp is the right answer rather than a race — or a
  // bounded number of frames have passed, because a lane that is simply not
  // on screen (the phone board shows one at a time) is answered by the
  // repaint that brings it back, not by waiting here.
  private reapplyColumnScroll(column: HTMLElement, scrollTop: number, attempt: number): void {
    if (attempt >= 10 || typeof requestAnimationFrame !== "function") return;
    requestAnimationFrame(() => {
      if (this.disposed || !column.isConnected) return;
      column.scrollTop = scrollTop;
      if (column.scrollTop === scrollTop || column.scrollHeight > column.clientHeight) return;
      this.reapplyColumnScroll(column, scrollTop, attempt + 1);
    });
  }

  private bindCards(): void {
    for (const card of this.container.querySelectorAll<HTMLButtonElement>("[data-tm-task]")) {
      card.addEventListener("click", () => {
        this.detailTaskID = card.dataset.tmTask ?? null;
        this.detailTask = null;
        this.detailError = null;
        this.dialogReturnSelector = this.detailTaskID === null
          ? null
          : `[data-tm-task="${this.detailTaskID}"]`;
        this.render();
        void this.loadTaskDetail();
      });
    }
  }

  private bindDetailModal(): void {
    const host = this.modalSlot.element;
    if (host === null) return;
    host.querySelector("[data-tm-detail-close]")?.addEventListener("click", () => this.closeDetail());
    for (const span of host.querySelectorAll<HTMLElement>(`[${TASK_REF_ATTR}]`)) {
      this.bindTaskRef(span);
    }
    host.querySelector("[role=dialog][data-phone-fullscreen]")
      ?.addEventListener("scroll", () => {
        this.cancelTaskHover();
        this.hideTaskPopover();
      });
  }

  private async loadTaskDetail(): Promise<void> {
    const taskID = this.detailTaskID;
    if (taskID === null) return;
    const generation = ++this.detailGeneration;
    try {
      const task = await this.model.getTask(taskID);
      if (this.disposed || generation !== this.detailGeneration || this.detailTaskID !== taskID) return;
      this.detailTask = task;
      this.detailError = null;
    } catch (err) {
      if (this.disposed || generation !== this.detailGeneration || this.detailTaskID !== taskID) return;
      this.detailTask = null;
      this.detailError = err instanceof Error ? err.message : String(err);
    }
    this.render();
  }

  private resolvableTaskIDs(): ReadonlySet<string> {
    if (this.model.tags !== this.knownTaskTags) {
      this.knownTaskTags = this.model.tags;
      this.knownTaskIDs = collectTaskIDs(this.model.tags);
    }
    return this.knownTaskIDs;
  }

  private bindTaskRefs(): void {
    for (const span of this.container.querySelectorAll<HTMLElement>(`[${TASK_REF_ATTR}]`)) {
      this.bindTaskRef(span);
    }
  }

  private bindTaskRef(span: HTMLElement): void {
    const id = span.dataset.taskRef ?? "";
    span.addEventListener("click", (event) => {
      event.preventDefault();
      event.stopPropagation();
    });
    span.addEventListener("mouseenter", () => this.scheduleTaskHover(span, id));
    span.addEventListener("focus", () => {
      const replacingFocusedPreview = span.closest("[data-task-ref-popover]") !== null;
      this.cancelTaskHover();
      this.showTaskPopover(span, id);
      if (replacingFocusedPreview) this.taskPopover?.focus({ preventScroll: true });
    });
    span.addEventListener("mouseleave", () => this.scheduleTaskHide());
    span.addEventListener("blur", () => this.scheduleTaskHide());
  }

  private scheduleTaskHover(span: HTMLElement, id: string): void {
    if (this.disposed) return;
    this.cancelTaskHover();
    this.taskRefHoverTimer = this.timer.set(() => {
      this.taskRefHoverTimer = null;
      this.showTaskPopover(span, id);
    }, taskRefHoverMS);
  }

  private scheduleTaskHide(): void {
    this.cancelTaskHover();
    this.taskRefHoverTimer = this.timer.set(() => {
      this.taskRefHoverTimer = null;
      this.hideTaskPopover();
    }, 100);
  }

  private showTaskPopover(anchor: HTMLElement, id: string): void {
    if (this.disposed) return;
    const task = resolveTask(id, this.model.tags);
    if (task === null) {
      this.hideTaskPopover();
      return;
    }
    const rect = anchor.getBoundingClientRect();
    this.hideTaskPopover();
    const popover = document.createElement("div");
    popover.id = `task-ref-popover-${id}`;
    popover.setAttribute(TASK_REF_ATTR + "-popover", "");
    popover.setAttribute("role", "region");
    popover.setAttribute("aria-label", `Task ${id} preview`);
    popover.setAttribute("tabindex", "-1");
    popover.className = TASK_POPOVER_CLASS;
    popover.innerHTML = taskCardPopoverHtml(task, this.resolvableTaskIDs());
    // Fixed to the viewport so the detail dialog's overflow-y-auto cannot
    // clip the card; positioned off the anchor's own rect.
    popover.style.position = "fixed";
    document.body.appendChild(popover);
    popover.addEventListener("mouseenter", () => this.cancelTaskHover());
    popover.addEventListener("mouseleave", () => this.scheduleTaskHide());
    popover.addEventListener("focusin", () => this.cancelTaskHover());
    popover.addEventListener("focusout", () => this.scheduleTaskHide());
    for (const dependency of popover.querySelectorAll<HTMLElement>(`[${TASK_REF_ATTR}]`)) {
      this.bindTaskRef(dependency);
    }
    this.taskPopover = popover;
    this.taskPopoverAnchor = anchor;
    anchor.setAttribute("aria-describedby", popover.id);
    const pw = popover.offsetWidth;
    const ph = popover.offsetHeight;
    const vw = window.innerWidth;
    const vh = window.innerHeight;
    let left = rect.left;
    if (left + pw > vw) left = Math.max(0, vw - pw);
    let top = rect.bottom + 6;
    if (top + ph > vh) top = Math.max(0, rect.top - ph - 6);
    popover.style.left = `${Math.round(left)}px`;
    popover.style.top = `${Math.round(top)}px`;
  }

  private hideTaskPopover(): void {
    this.taskPopoverAnchor?.removeAttribute("aria-describedby");
    this.taskPopoverAnchor = null;
    if (this.taskPopover !== null) {
      this.taskPopover.remove();
      this.taskPopover = null;
    }
  }

  private cancelTaskHover(): void {
    if (this.taskRefHoverTimer !== null) {
      this.timer.clear(this.taskRefHoverTimer);
      this.taskRefHoverTimer = null;
    }
  }

  // syncPolling keeps exactly one timeout while the panel is visible. A
  // timeout schedules its successor only after the prior request settles,
  // so a slow canonical read cannot accumulate overlapping work.
  private syncPolling(reset = false): void {
    if (this.disposed || !this.visible || this.model.state === "loading") {
      this.stopPolling();
      return;
    }
    if (reset) this.stopPolling();
    if (this.pollTimer !== null) return;
    const generation = this.generation;
    const delay = this.model.pollActive() ? runPollMS : taskRefreshMS;
    this.pollTimer = this.timer.set(async () => {
      this.pollTimer = null;
      await this.pollOnce(generation);
      if (this.disposed || !this.visible || generation !== this.generation) return;
      this.syncPolling();
    }, delay);
  }

  private async pollOnce(generation: number): Promise<void> {
    if (this.disposed || !this.visible || generation !== this.generation) return;
    const before = this.presentationKey();
    const runWasActive = this.model.pollActive();
    if (runWasActive) await this.model.pollRun();
    if (this.disposed || !this.visible || generation !== this.generation) return;
    // pollRun reloads authoritative tasks when a run exits. A still-active run
    // needs only a task read; an idle/onboarding panel repeats detection too,
    // so an external initialization or removal is observed.
    if (runWasActive && this.model.pollActive()) await this.model.refreshTasks();
    if (!runWasActive) await this.model.refresh();
    if (this.disposed || !this.visible || generation !== this.generation) return;
    if (before === this.presentationKey()) return;
    this.render();
  }

  // Stable API responses are the common case. Rebuilding the panel for an
  // unchanged answer would drop focus and reset independently scrolled
  // columns, so compare only the server/model state render() consumes.
  private presentationKey(): string {
    return JSON.stringify([
      this.model.state,
      this.model.tags,
      this.model.prds,
      this.model.selectedPrd,
      this.model.activeRun,
      this.model.tasksError,
      this.model.generateError,
      this.model.generating,
      this.model.addError,
      this.model.adding,
      this.sortKey,
      this.sortDesc,
    ]);
  }

  private stopPolling(): void {
    if (this.pollTimer !== null) {
      this.timer.clear(this.pollTimer);
      this.pollTimer = null;
    }
  }

  private bindAddTaskEntry(): void {
    for (const btn of this.container.querySelectorAll<HTMLElement>("[data-tm-add-task]")) {
      btn.addEventListener("click", () => {
        const source = btn.dataset.tmAddTask;
        this.dialogReturnSelector = source === undefined
          ? null
          : `[data-tm-add-task="${CSS.escape(source)}"]`;
        this.addPrdOpen = false;
        this.addTaskOpen = true;
        this.model.addError = null;
        this.render();
      });
    }
  }

  private closeDetail(): void {
    this.detailTaskID = null;
    this.detailTask = null;
    ++this.detailGeneration;
    this.render();
  }

  private dismissModal(): void {
    if (this.detailTaskID !== null) this.closeDetail();
    else if (this.addTaskOpen) this.exitAddTask();
    else this.exitAddPrd();
  }

  private exitAddTask(): void {
    if (this.model.adding) return;
    this.addTaskOpen = false;
    this.model.addError = null;
    this.render();
    this.syncPolling();
  }

  private renderAddTaskModal(): string {
    const busy = this.model.adding;
    const fieldClass = "font-input mt-1 h-9 w-full rounded-[6px] border border-field-border bg-sidebar-surface px-3 text-[length:var(--type-input-size)] text-text-primary sm:text-body-sm";
    const areaClass = "font-input mt-1 w-full rounded-[6px] border border-field-border bg-sidebar-surface px-3 py-2 text-[length:var(--type-input-size)] text-text-primary sm:text-body-sm resize-y";
    return `
        <form data-tm-task-add-form role="dialog" aria-modal="true" aria-label="Add task" class="dialog">
          ${dialogHead("Add task", { label: "Close", attr: "data-tm-task-add-cancel", disabled: busy })}
          ${DIALOG_BODY(`
            <label class="block">
              <span class="font-label text-(length:--type-label-size) text-text-secondary">Title</span>
              <input data-tm-task-title required maxlength="65536" ${busy ? "disabled" : ""}
                autocomplete="off" data-hint="${hints.ENTER_TASK_TITLE}" class="${fieldClass}">
            </label>
            <label class="block">
              <span class="font-label text-(length:--type-label-size) text-text-secondary">Description</span>
              <textarea data-tm-task-description maxlength="65536" rows="3" ${busy ? "disabled" : ""}
                data-hint="${hints.ENTER_TASK_DESCRIPTION}" class="${areaClass}"></textarea>
            </label>
            <label class="block">
              <span class="font-label text-(length:--type-label-size) text-text-secondary">Details</span>
              <textarea data-tm-task-details maxlength="65536" rows="4" ${busy ? "disabled" : ""}
                data-hint="${hints.ENTER_TASK_DETAILS}" class="${areaClass}"></textarea>
            </label>
            <label class="block">
              <span class="font-label text-(length:--type-label-size) text-text-secondary">Priority</span>
              ${selectShellHtml(`<select data-tm-task-priority ${busy ? "disabled" : ""} data-hint="${hints.CHOOSE_TASK_PRIORITY}" class="${SELECT_FIELD} ${fieldClass.replace("mt-1 ", "").replace("px-3", "pl-3")}">
                <option value="high">High</option><option value="medium" selected>Medium</option><option value="low">Low</option>
              </select>`, "mt-1")}
            </label>
            ${this.model.addError === null ? "" : `<div role="alert" class="rounded-control bg-error-tint px-3 py-2 text-caption text-error-on-tint">${escapeHtml(this.model.addError)}</div>`}
          `)}
          ${DIALOG_FOOT(`
            <button type="button" data-tm-task-add-cancel ${busy ? "disabled" : ""} data-hint="${hints.CANCEL_ADDING_TASK}"
              class="h-9 rounded-control border border-border bg-surface-card px-3 text-body-sm text-text-primary hover:bg-wash-hover disabled:opacity-45 pointer-coarse:min-h-11">Cancel</button>
            <button type="submit" data-tm-task-add-submit ${busy ? "disabled" : ""} data-hint="${hints.ADD_TASK_TO_BOARD}"
              class="h-9 rounded-control bg-accent px-4 text-body-sm font-medium text-text-on-accent hover:bg-accent-hover disabled:opacity-45 pointer-coarse:min-h-11">${busy ? "Adding…" : "Add task"}</button>
          `)}
        </form>`;
  }

  private bindAddTaskModal(): void {
    const host = this.modalSlot.element;
    if (!this.addTaskOpen || host === null) return;
    for (const btn of host.querySelectorAll("[data-tm-task-add-cancel]")) {
      btn.addEventListener("click", () => this.exitAddTask());
    }
    host.querySelector<HTMLFormElement>("[data-tm-task-add-form]")?.addEventListener("submit", (event) => {
      event.preventDefault();
      if (this.model.adding) return;
      const title = host.querySelector<HTMLInputElement>("[data-tm-task-title]")?.value.trim() ?? "";
      if (title === "") {
        this.model.addError = "title is required";
        this.render();
        return;
      }
      const priority = host.querySelector<HTMLSelectElement>("[data-tm-task-priority]")?.value as AddTaskInput["priority"];
      void this.performAddTask({
        title,
        description: host.querySelector<HTMLTextAreaElement>("[data-tm-task-description]")?.value ?? "",
        details: host.querySelector<HTMLTextAreaElement>("[data-tm-task-details]")?.value ?? "",
        priority,
      });
    });
  }

  private async performAddTask(input: AddTaskInput): Promise<void> {
    const pending = this.model.addTask(input);
    this.render();
    const ok = await pending;
    if (this.disposed) return;
    if (ok) {
      this.addTaskOpen = false;
      this.model.addError = null;
    }
    this.render();
    this.syncPolling(true);
  }

  // bindAddPrdEntry wires every Add PRD affordance — the panel header's
  // and the onboarding screen's — to the same modal.
  private bindAddPrdEntry(): void {
    for (const btn of this.container.querySelectorAll<HTMLElement>("[data-tm-add-prd]")) {
      btn.addEventListener("click", (ev) => {
        ev.stopPropagation();
        const source = btn.dataset.tmAddPrd;
        this.dialogReturnSelector = source === undefined
          ? null
          : `[data-tm-add-prd="${CSS.escape(source)}"]`;
        this.openAddPrd();
      });
    }
  }

  // openAddPrd is public so the preview harness can reach the Add-PRD dialog
  // from a bare URL rather than scripting the header button.
  openAddPrd(): void {
    if (this.disposed) return;
    this.addTaskOpen = false;
    this.addPrdOpen = true;
    this.resetAddPrd();
    this.render();
  }

  // openAddTask opens the Add task form for a caller outside the panel. A
  // project with no task state gets it set up first, as the form needs it.
  async openAddTask(): Promise<boolean> {
    if (this.disposed) return false;
    if (this.model.state === "uninitialized") {
      await this.model.initialize();
      if (this.disposed) return false;
    }
    if (this.model.state !== "ready") {
      this.render();
      return false;
    }
    this.dialogReturnSelector = '[data-tm-add-task="header"]';
    this.addPrdOpen = false;
    this.addTaskOpen = true;
    this.model.addError = null;
    this.render();
    return true;
  }

  // generateFromPrd starts task generation from the PRD the board already
  // holds. The model resolves even when the request failed, so the answer
  // comes from its error: "failed" carries the message the board shows. With
  // no PRD it opens the Add PRD flow, which generates on upload.
  async generateFromPrd(harness?: string): Promise<{ outcome: "started" | "missing-prd" } | { outcome: "failed"; error: string }> {
    if (this.disposed) return { outcome: "started" };
    if (this.model.prds.length === 0) {
      this.openAddPrd();
      return { outcome: "missing-prd" };
    }
    this.model.generateError = null;
    const pending = this.model.generate(harness);
    this.render();
    await pending;
    if (this.disposed) return { outcome: "started" };
    this.render();
    this.syncPolling(true);
    const error = this.model.generateError;
    return error === null ? { outcome: "started" } : { outcome: "failed", error };
  }

  private resetAddPrd(): void {
    this.pickedFile = null;
    this.confirmReplace = false;
    this.uploading = false;
    this.model.generateError = null;
  }

  private exitAddPrd(): void {
    if (this.uploading) return;
    this.addPrdOpen = false;
    this.resetAddPrd();
    this.render();
    this.syncPolling();
  }

  private bindDialogFocus(focusRestored: boolean): void {
    this.dialogFocusCleanup?.();
    this.dialogFocusCleanup = null;
    const dialog = this.modalSlot.element?.querySelector<HTMLElement>('[role="dialog"]') ?? null;
    if (dialog !== null) {
      if (typeof HTMLElement !== "undefined" && dialog instanceof HTMLElement) {
        this.dialogFocusCleanup = activateDialog(dialog, () => {
          if (this.detailTaskID !== null) {
            this.detailTaskID = null;
            this.render();
          } else if (this.addTaskOpen) {
            this.exitAddTask();
          } else {
            this.exitAddPrd();
          }
        // Container focus unless a field is the surface's point: no focus
        // mark is drawn at open, and an auto-focused close control
        // would wear the keyboard wash immediately.
        }, focusRestored ? null : this.addTaskOpen ? "[data-tm-task-title]" : ":dialog");
      }
      return;
    }
    const selector = this.dialogReturnSelector;
    this.dialogReturnSelector = null;
    if (selector !== null) {
      const target = this.container.querySelector<HTMLElement>(selector);
      if (target !== null && typeof HTMLElement !== "undefined" && target instanceof HTMLElement) returnFocus(target);
    }
  }

  // renderAddPrdModal is the centered Add-PRD dialog on the shared
  // anatomy: the dashed drop zone over the
  // destination note, then Cancel and Add PRD in the foot. No filename
  // field — the file is stored under the canonical prd.md regardless of
  // its name, then parsed into tasks. The destination is the
  // server-presented docs directory, never an invented path.
  private renderAddPrdModal(): string {
    const m = this.model;
    const exists = m.hasCanonicalPrd();
    const picked = this.pickedFile;
    const busy = this.uploading;
    const destination = m.docsPath === null ? null : `${m.docsPath}/${PRD_CANONICAL_NAME}`;
    return `
        <div data-tm-add-prd-modal role="dialog" aria-modal="true" aria-label="Add PRD" class="dialog">
          ${dialogHead("Add PRD", { label: "Close", attr: "data-tm-prd-cancel", disabled: busy })}
          ${DIALOG_BODY(`
            <label data-tm-prd-dropzone class="flex cursor-pointer flex-col items-center gap-1.5 rounded-card border border-dashed border-border-strong bg-surface px-5 py-[26px] text-center transition-colors hover:border-accent hover:bg-accent-tint" data-hint="${hints.CHOOSE_PRD_FILE}">
              ${picked === null
                ? `<span class="text-body-sm font-medium text-text-primary">Drop a markdown file</span>
                   <span class="text-caption text-text-tertiary">or click to browse</span>`
                : `<span class="text-body-sm font-medium text-text-primary">${escapeHtml(picked.name)}</span>
                   <span class="font-mono text-micro text-text-tertiary">${escapeHtml(formatFileSize(picked.size))}</span>
                   <span class="text-caption text-accent">Choose a different file</span>`}
              <input data-tm-prd-file type="file" accept=".md" class="hidden" ${busy ? "disabled" : ""}>
            </label>
            <div class="flex flex-col gap-1">
              <span class="text-caption leading-[1.5] text-text-secondary">The file is saved as <span class="font-mono text-[min(var(--type-mono-size),calc(1em*var(--type-mono-xh-ratio)))] text-text-primary">${escapeHtml(PRD_CANONICAL_NAME)}</span>. Tasks are parsed from it into the board.</span>
              ${destination === null ? "" : `<span class="font-mono text-micro leading-[1.5] text-text-tertiary">${escapeHtml(destination)}</span>`}
            </div>
            ${m.generateError !== null
              ? `<div class="rounded-control bg-error-tint px-3 py-2 text-caption text-error-on-tint">${escapeHtml(m.generateError)}</div>`
              : ""}
            ${this.confirmReplace && exists
              ? `<p class="rounded-control bg-warning-tint px-3 py-2 text-body-sm text-warning-on-tint"><span class="font-mono">${escapeHtml(PRD_CANONICAL_NAME)}</span> already exists. Replacing overwrites the current PRD.</p>`
              : ""}
          `)}
          ${DIALOG_FOOT(this.renderAddPrdActions(picked, exists, busy))}
        </div>
    `;
  }

  private renderAddPrdActions(picked: PickedFile | null, exists: boolean, busy: boolean): string {
    if (this.confirmReplace && exists) {
      return `
        <button type="button" data-tm-prd-confirm-cancel ${busy ? "disabled" : ""} data-hint="${hints.GO_BACK_AND_CHOOSE_FILE}"
          class="h-9 rounded-control px-3 text-body-sm text-text-secondary transition-colors hover:bg-wash-hover disabled:opacity-45 pointer-coarse:min-h-11">Back</button>
        <button type="button" data-tm-prd-confirm-replace ${busy ? "disabled" : ""} data-hint="${hints.REPLACE_EXISTING_PRD}"
          class="h-9 rounded-control bg-accent px-3 text-body-sm font-medium text-text-on-accent transition-colors hover:bg-accent-hover disabled:opacity-45 pointer-coarse:min-h-11">Replace prd.md</button>`;
    }
    return `
      <button type="button" data-tm-prd-cancel ${busy ? "disabled" : ""} data-hint="${hints.CANCEL_ADDING_PRD}"
        class="h-9 rounded-control border border-border bg-surface-card px-3 text-body-sm text-text-primary transition-colors hover:bg-wash-hover disabled:opacity-45 pointer-coarse:min-h-11">Cancel</button>
      <button type="button" data-tm-prd-upload ${picked === null || busy ? "disabled" : ""} data-hint="${hints.UPLOAD_PRD_AND_GENERATE}"
        class="h-9 rounded-control bg-accent px-3 text-body-sm font-medium text-text-on-accent transition-colors hover:bg-accent-hover disabled:opacity-45 pointer-coarse:min-h-11">${busy ? "Uploading…" : "Add PRD"}</button>`;
  }

  // bindAddPrdModal wires the upload flow: cancel closes the modal, the
  // picker (or a drop on the zone) reads the file into memory, and the
  // upload either asks to confirm a collision or proceeds.
  private bindAddPrdModal(): void {
    const host = this.modalSlot.element;
    if (!this.addPrdOpen || host === null) return;
    for (const btn of host.querySelectorAll("[data-tm-prd-cancel]")) {
      btn.addEventListener("click", (ev) => {
        ev.stopPropagation();
        this.exitAddPrd();
      });
    }
    host.querySelector<HTMLInputElement>("[data-tm-prd-file]")?.addEventListener("change", (ev) => {
      const file = (ev.target as HTMLInputElement).files?.[0];
      if (file) this.pickFile(file);
    });
    const dropzone = host.querySelector<HTMLElement>("[data-tm-prd-dropzone]");
    dropzone?.addEventListener("dragover", (ev) => {
      ev.preventDefault();
    });
    dropzone?.addEventListener("drop", (ev) => {
      ev.preventDefault();
      if (this.uploading) return;
      const file = ev.dataTransfer?.files?.[0];
      if (!file) return;
      if (!file.name.endsWith(".md")) {
        this.model.generateError = "a PRD must be a .md file";
        this.render();
        return;
      }
      this.pickFile(file);
    });
    host.querySelector("[data-tm-prd-upload]")?.addEventListener("click", () => {
      if (this.pickedFile === null) return;
      if (this.model.hasCanonicalPrd()) {
        this.confirmReplace = true;
        this.render();
        return;
      }
      void this.performUpload(false);
    });
    host.querySelector("[data-tm-prd-confirm-cancel]")?.addEventListener("click", () => {
      this.confirmReplace = false;
      this.render();
    });
    host.querySelector("[data-tm-prd-confirm-replace]")?.addEventListener("click", () => {
      void this.performUpload(true);
    });
  }

  private pickFile(file: File): void {
    void file.text().then((content) => {
      if (this.disposed) return;
      this.pickedFile = { name: file.name, size: file.size, content };
      this.confirmReplace = false;
      this.model.generateError = null;
      this.render();
    });
  }

  // performUpload stores the file as prd.md, then starts generation —
  // an added PRD is parsed into tasks, no
  // separate Generate control.
  private async performUpload(replace: boolean): Promise<void> {
    if (this.pickedFile === null || this.uploading) return;
    if (this.pickedFile.content.trim() === "") {
      this.model.generateError = "the chosen file is empty";
      this.render();
      return;
    }
    this.uploading = true;
    this.render();
    const ok = await this.model.uploadPrd(this.pickedFile.content, replace);
    if (this.disposed) return;
    if (ok) {
      await this.model.generate();
      if (this.disposed) return;
      this.uploading = false;
      // Close (and clear) only when the parse actually started: this is
      // the ONLY generation path, so a failed parse must keep the modal
      // open with the model's error — resetAddPrd() would erase it.
      if (this.model.generateError === null) {
        this.addPrdOpen = false;
        this.resetAddPrd();
      }
    } else {
      this.uploading = false;
    }
    this.render();
    this.syncPolling(true);
  }
}
