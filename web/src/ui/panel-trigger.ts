import { activateMenu } from "./dialog-focus.ts";
import { escapeHtml } from "../lib/escape.ts";
import { SECONDARY_SM_TYPE } from "./controls.ts";
import { lucideIcon } from "./icons.ts";
import * as hints from "../lib/hints.ts";

export type PanelTriggerSurface = "live" | "titlebar";
export type PanelTrigger = "files" | "tasks" | "chats" | "assets";

// The docked-panel triggers are Blueprint secondary sm buttons:
// a 28px pill on the card surface, --radius-control-sm, a --border-strong
// hairline, 10px side padding, 13px medium text, and a --space-1 icon gap.
// The hint is verb-first and is read by the info bar,
// not a tooltip.
const PANEL_HINT: Record<PanelTrigger, string> = {
  files: hints.BROWSE_FILES,
  tasks: hints.OPEN_TASK_BOARD,
  chats: hints.WATCH_CHATS,
  assets: hints.OPEN_ASSETS,
};

const PANEL_LABEL: Record<PanelTrigger, string> = { files: "Files", tasks: "Project tasks", chats: "Chats", assets: "Assets" };
const PANEL_TEXT: Record<PanelTrigger, string> = { files: "Files", tasks: "Tasks", chats: "Chats", assets: "Assets" };
const PANEL_ICON: Record<PanelTrigger, "folder" | "columns-2" | "message-square" | "eye"> = {
  files: "folder",
  tasks: "columns-2",
  chats: "message-square",
  assets: "eye",
};

// assetsLabel is the Assets trigger's accessible name: the badge is
// hidden from assistive technology, so the name carries the count.
export function assetsLabel(count: number): string {
  return count > 0 ? `Assets, ${count} new` : "Assets";
}

// Panel contents own their counts. Assets is the one trigger with a badge,
// because its news is a number of files agents wrote since the panel was
// last open; the Chats trigger's news is a presence and stays a colour.
export function panelTriggerHtml(
  surface: PanelTriggerSurface,
  trigger: PanelTrigger,
  options: { pressed?: boolean; unseen?: boolean; hint?: string; count?: number } = {},
): string {
  const hook = `data-${surface}-${trigger}`;
  // Only Assets counts; a count passed for another trigger is ignored.
  const counted = trigger === "assets" && options.count !== undefined;
  const count = counted ? options.count ?? 0 : 0;
  const label = counted ? assetsLabel(count) : PANEL_LABEL[trigger];
  const icon = lucideIcon(PANEL_ICON[trigger], 14);
  const text = PANEL_TEXT[trigger];
  const pressed = options.pressed === undefined ? "" : ` aria-pressed="${options.pressed}"`;
  // unseen is the Chats trigger's one extra fact: accent while
  // a live chat holds messages the user has not seen. No count.
  const unseen = options.unseen === undefined ? "" : ` data-unseen="${options.unseen}"`;
  // A catalogue hint is authored text; a caller-supplied one carries agent
  // data (the handle in "Watch the chats codex-9f2a is in") and is escaped
  // at this boundary.
  const hint = options.hint === undefined ? PANEL_HINT[trigger] : escapeHtml(options.hint);
  const news = counted ? ` data-assets-new="${count}"` : "";
  const badge = counted ? assetsBadgeHtml(count) : "";
  return `<button ${hook} aria-label="${label}" data-hint="${hint}"${pressed}${unseen}${news}
    class="group/panel-trigger ${counted ? "relative " : ""}flex h-7 flex-none items-center justify-center text-text-primary ${SECONDARY_SM_TYPE}">
    <span data-panel-trigger-visual class="relative flex h-7 items-center gap-1 rounded-control-sm border border-border-strong bg-surface-card px-2.5 transition-colors group-hover/panel-trigger:bg-wash-hover">
      ${icon}<span>${text}</span>
    </span>${badge}
  </button>`;
}

// The desktop trigger's badge: a 14px accent pill over the trigger's top
// right corner, in the interface face. Rendered hidden at zero so a count
// can arrive without rebuilding the trigger.
function assetsBadgeHtml(count: number): string {
  return `<span data-assets-badge aria-hidden="true"${count > 0 ? "" : " hidden"}
      class="pointer-events-none absolute -top-[3px] -right-[5px] box-border h-[14px] min-w-[14px] rounded-[7px] bg-accent px-[3px] text-center font-ui text-(length:--type-badge-size) leading-[14px] font-normal tracking-normal text-text-on-accent">${count}</span>`;
}

// panelsLabel is the compact Panels button's accessible name: its badge is
// hidden from assistive technology too, so the name carries the count.
export function panelsLabel(count: number): string {
  if (count <= 0) return "Panels";
  return `Panels, ${count} new ${count === 1 ? "asset" : "assets"}`;
}

// The Panels button's badge, the compact chrome's one place for the count:
// the small solid badge over the 44px button's top right corner.
export function panelsBadgeHtml(count: number): string {
  return `<span data-assets-badge aria-hidden="true"${count > 0 ? "" : " hidden"} class="assets-panels-badge">${count}</span>`;
}

// paintAssetsNew writes the count onto every control that shows it, in
// place: the Assets trigger and its badge, the Panels button and its
// badge, and the Panels menu's Assets row while the menu is up.
export function paintAssetsNew(root: HTMLElement, surface: PanelTriggerSurface, count: number): void {
  const trigger = root.querySelector<HTMLElement>(`[data-${surface}-assets]`);
  if (trigger) {
    trigger.dataset.assetsNew = String(count);
    trigger.setAttribute("aria-label", assetsLabel(count));
  }
  const panels = root.querySelector<HTMLElement>(`[data-${surface}-panels]`);
  if (panels) {
    panels.dataset.assetsNew = String(count);
    panels.setAttribute("aria-label", panelsLabel(count));
  }
  for (const button of [trigger, panels]) {
    const badge = button?.querySelector<HTMLElement>("[data-assets-badge]");
    if (!badge) continue;
    badge.textContent = String(count);
    badge.hidden = count === 0;
  }
  const row = root.querySelector<HTMLElement>('[data-panel-choice="assets"]');
  if (row) {
    row.dataset.assetsNew = String(count);
    row.querySelector("[data-assets-caption]")?.remove();
    if (count > 0) row.querySelector(".flex-1")?.insertAdjacentHTML("afterend", assetsCaptionHtml(count));
  }
}

// The arrival wash running on each control; a later arrival replaces it.
const washes = new WeakMap<Element, Animation>();

// flashAssetsArrived washes the controls that carry the count once, fading
// from the accent tint over --dur-notice. Deck never opens the panel for new
// files: a layout change the user did not make moves the terminal under
// them, so the news stays on the control. Reduced motion zeroes the
// duration, which leaves only the badge.
export function flashAssetsArrived(root: HTMLElement, surface: PanelTriggerSurface): void {
  const styles = root.ownerDocument.defaultView?.getComputedStyle(root);
  if (!styles) return;
  const duration = milliseconds(styles.getPropertyValue("--dur-notice"));
  const easing = styles.getPropertyValue("--ease-out").trim() || "ease-out";
  const tint = styles.getPropertyValue("--color-accent-tint").trim();
  for (const control of [root.querySelector<HTMLElement>(`[data-${surface}-assets]`), root.querySelector<HTMLElement>(`[data-${surface}-panels]`)]) {
    if (!control) continue;
    const target = control.querySelector<HTMLElement>("[data-panel-trigger-visual]") ?? control;
    washes.get(target)?.cancel();
    washes.set(target, target.animate([{ backgroundColor: tint }, {}], { duration, easing }));
  }
}

// milliseconds reads a CSS time token, which the build may rewrite from
// "600ms" to ".6s".
function milliseconds(time: string): number {
  const value = parseFloat(time);
  if (!Number.isFinite(value)) return 0;
  return time.trim().endsWith("ms") ? value : value * 1000;
}

// The Panels menu's Assets row caption: "3 new", right-aligned in accent.
function assetsCaptionHtml(count: number): string {
  return `<span data-assets-caption class="flex-none text-caption">${count} new</span>`;
}

export function focusPanelTrigger(root: HTMLElement, surface: PanelTriggerSurface, panel: PanelTrigger): void {
  const mobile = root.querySelector<HTMLElement>(`[data-${surface}-panels]`);
  (mobile && mobile.getClientRects().length ? mobile : root.querySelector<HTMLElement>(`[data-${surface}-${panel}]`))?.focus();
}

export function mountPanelsMenu(
  root: HTMLElement,
  surface: PanelTriggerSurface,
  close: () => void,
  select: (panel: PanelTrigger) => void,
): () => void {
  const trigger = root.querySelector<HTMLElement>(`[data-${surface}-panels]`);
  const host = root.querySelector<HTMLElement>('[data-region="popovers"]');
  if (!trigger || !host) return () => {};
  const items = (["files", "tasks", "chats", "assets"] as const).filter((panel) => root.querySelector(`[data-${surface}-${panel}]`));
  const rows = items.map((panel) => {
    const current = root.querySelector(`[data-${surface}-${panel}]`)?.getAttribute("aria-pressed") === "true";
    const unseen = panel === "chats" && trigger.dataset.unseen === "true";
    const count = panel === "assets" ? Number(trigger.dataset.assetsNew ?? "0") || 0 : 0;
    const news = panel === "assets" ? ` data-assets-new="${count}"` : "";
    const caption = panel === "assets" && count > 0 ? assetsCaptionHtml(count) : "";
    return `<button type="button" role="menuitem" data-panel-choice="${panel}"
      aria-current="${current}" data-unseen="${unseen}"${news}
      class="flex min-h-11 w-full items-center gap-2 rounded-control-sm px-3 text-body-sm text-text-primary hover:bg-wash-hover focus-visible:bg-wash-hover">
      ${lucideIcon(PANEL_ICON[panel], 16)}<span class="flex-1 text-left">${PANEL_TEXT[panel]}</span>
      ${caption}${current ? lucideIcon("check", 16) : ""}
    </button>`;
  }).join("");
  host.innerHTML = `<div class="absolute inset-0 pointer-events-auto" data-panels-backdrop>
    <div role="menu" aria-label="Panels" data-panels-menu
      class="absolute min-w-44 rounded-control border border-border bg-surface-overlay p-1 shadow-lg">${rows}</div>
  </div>`;
  const menu = host.querySelector<HTMLElement>('[data-panels-menu]')!;
  const rect = trigger.getBoundingClientRect();
  const bounds = host.getBoundingClientRect();
  menu.style.top = `${rect.bottom - bounds.top + 4}px`;
  menu.style.left = `${Math.max(8, Math.min(rect.right - bounds.left - menu.offsetWidth, bounds.width - menu.offsetWidth - 8))}px`;
  trigger.setAttribute("aria-expanded", "true");
  const dismiss = (restore: boolean): void => {
    close();
    if (restore) focusPanelTrigger(root, surface, "files");
  };
  host.querySelector('[data-panels-backdrop]')?.addEventListener("click", (event) => {
    if (event.target === event.currentTarget) dismiss(true);
  });
  for (const item of menu.querySelectorAll<HTMLElement>('[data-panel-choice]')) {
    item.addEventListener("click", () => {
      const panel = item.dataset.panelChoice as PanelTrigger;
      dismiss(true);
      select(panel);
    });
  }
  const releaseFocus = activateMenu(menu, dismiss);
  const onResize = (): void => dismiss(true);
  window.addEventListener("resize", onResize);
  return () => {
    releaseFocus();
    window.removeEventListener("resize", onResize);
    root.querySelector(`[data-${surface}-panels]`)?.setAttribute("aria-expanded", "false");
  };
}
