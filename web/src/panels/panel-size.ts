// Shared docked-panel resizing. The panels overlay the content row from
// the right; their width is --docked-panel-w, which each region's CSS sets
// as a responsive clamp. A user width overrides that clamp with an inline
// value on the panel element, and Enter or a double-click returns the
// panel to the stylesheet's default by removing the override entirely, so
// an untouched panel keeps today's behavior exactly.
//
// The drag boundary is the panel's left edge: moving the pointer left
// widens the panel. The terminal keeps its floor - the bounds never let
// the panel take the row past the available width minus the reserve.

export interface PanelSizeOptions {
  // localStorage key holding the saved width; absent or invalid means the
  // stylesheet default.
  key: string;
  min: number;
  max: number;
  // Space the rest of the row keeps: bounds cap at available - reserve.
  reserve: number;
}

// The smallest width the controller can persist: a narrow row can force a
// panel below its preferred minimum all the way to the emergency floor,
// so a saved preference is valid anywhere in [floor, max] and is clamped
// to the current row when it renders.
export const PANEL_FLOOR = 64;

export function panelPreference(value: string | null, floor: number, max: number): number | null {
  const width = value === null || value.trim() === "" ? NaN : Number(value);
  return Number.isFinite(width) && width >= floor && width <= max ? width : null;
}

export function panelBounds(available: number, min: number, max: number, reserve: number): { min: number; max: number } {
  // The terminal's reserve outranks the panel's preferred minimum: a
  // narrow desktop row must keep the terminal its floor even when that
  // means a panel below the minimum, exactly as the stylesheet's own
  // clamp already allows. A 64px emergency floor keeps a degenerate
  // window from collapsing the panel to nothing.
  const ceiling = Math.max(64, Math.min(max, available - reserve));
  return { min: Math.min(min, ceiling), max: ceiling };
}

export class PanelSize {
  private readonly panel: HTMLElement;
  private readonly handle: HTMLElement;
  private readonly opts: PanelSizeOptions;
  private preferred: number | null = null;
  private drag: { pointer: number; x: number; width: number; saved: number | null } | null = null;
  private readonly observer: ResizeObserver;
  private readonly desktop = window.matchMedia("(min-width: 48rem)");
  private readonly cleanup: Array<() => void> = [];
  private disposed = false;

  constructor(panel: HTMLElement, handle: HTMLElement, opts: PanelSizeOptions) {
    this.panel = panel;
    this.handle = handle;
    this.opts = opts;
    try { this.preferred = panelPreference(window.localStorage.getItem(opts.key), PANEL_FLOOR, opts.max); } catch { /* Preferences are optional. */ }
    this.listen(handle, "pointerdown", (event) => this.begin(event as PointerEvent));
    this.listen(window, "pointermove", (event) => this.move(event as PointerEvent));
    this.listen(window, "pointerup", (event) => { if ((event as PointerEvent).pointerId === this.drag?.pointer) this.finish(true); });
    this.listen(window, "pointercancel", (event) => { if ((event as PointerEvent).pointerId === this.drag?.pointer) this.finish(false); });
    this.listen(handle, "lostpointercapture", () => this.finish(false));
    this.listen(window, "blur", () => this.finish(false));
    // Escape cancels an in-flight drag no matter which control holds
    // focus: the drag's pointerdown prevented the focus change that would
    // have brought it to the separator, so the separator's own key
    // listener cannot be the only path. Outside a drag this listener
    // touches nothing.
    this.listen(window, "keydown", (event) => {
      if ((event as KeyboardEvent).key === "Escape" && this.drag !== null) {
        event.preventDefault();
        this.finish(false);
      }
    });
    this.listen(handle, "keydown", (event) => this.key(event as KeyboardEvent));
    this.listen(handle, "dblclick", () => this.reset());
    this.listen(this.desktop, "change", () => { this.finish(false); this.refresh(); });
    this.observer = new ResizeObserver(() => { this.finish(false); this.refresh(); });
    // The row, never the panel: observing the panel would fire this observer
    // mid-drag and rewind the drag. The row changes only when the window or
    // the layout does, which is exactly when the clamp must be re-read.
    this.observer.observe(panel.parentElement ?? panel);
    this.refresh();
  }

  private listen(target: EventTarget, name: string, listener: EventListener): void {
    target.addEventListener(name, listener);
    this.cleanup.push(() => target.removeEventListener(name, listener));
  }

  private enabled(): boolean {
    return this.desktop.matches && !this.disposed;
  }

  private bounds(): { min: number; max: number } {
    const row = this.panel.parentElement?.getBoundingClientRect();
    const room = row === undefined || row.width === 0 ? window.innerWidth : row.width;
    return panelBounds(room, this.opts.min, this.opts.max, this.opts.reserve);
  }

  private clamp(value: number): number {
    const { min, max } = this.bounds();
    return Math.round(Math.max(min, Math.min(max, value)));
  }

  refresh(): void {
    if (this.disposed) return;
    const enabled = this.enabled();
    if (!enabled && this.drag !== null) this.finish(false);
    // A panel the user has never sized keeps the stylesheet's clamp, and so
    // does every phone layout: the inline override exists only on the
    // desktop, where it is clamped to whatever the row can currently
    // afford. Writing it under the phone width would clobber the sheet's
    // full-bleed panel with the saved desktop pixels.
    if (this.preferred !== null && this.desktop.matches) {
      this.panel.style.setProperty("--docked-panel-w", `${this.clamp(this.preferred)}px`);
    } else {
      this.panel.style.removeProperty("--docked-panel-w");
    }
    this.handle.hidden = !enabled;
    this.handle.tabIndex = enabled ? 0 : -1;
    // The separator always reports the width the panel actually renders -
    // the saved one, or the stylesheet's clamp when no preference exists -
    // so the range stays valid on the default path and after a reset.
    const now = this.preferred !== null && this.desktop.matches
      ? this.clamp(this.preferred)
      : Math.round(this.panel.getBoundingClientRect().width);
    this.handle.setAttribute("aria-valuemin", String(this.bounds().min));
    this.handle.setAttribute("aria-valuemax", String(this.bounds().max));
    this.handle.setAttribute("aria-valuenow", String(now));
    this.handle.setAttribute("aria-valuetext", `${now} pixels`);
  }

  private begin(event: PointerEvent): void {
    if (!this.enabled() || event.button !== 0 || !event.isPrimary || this.drag !== null) return;
    event.preventDefault();
    this.drag = { pointer: event.pointerId, x: event.clientX, width: this.preferred === null ? this.panel.getBoundingClientRect().width : this.clamp(this.preferred), saved: this.preferred };
    this.panel.dataset.panelResizing = "true";
    try { this.handle.setPointerCapture(event.pointerId); } catch { this.finish(false); }
  }

  private move(event: PointerEvent): void {
    if (this.drag === null || event.pointerId !== this.drag.pointer) return;
    if (!this.enabled()) { this.finish(false); return; }
    this.preferred = this.clamp(this.drag.width + this.drag.x - event.clientX);
    this.refresh();
  }

  private finish(commit: boolean): void {
    const drag = this.drag;
    if (drag === null) return;
    this.drag = null;
    delete this.panel.dataset.panelResizing;
    if (!commit) this.preferred = drag.saved;
    try { if (this.handle.hasPointerCapture(drag.pointer)) this.handle.releasePointerCapture(drag.pointer); } catch { /* Capture may already be gone. */ }
    if (commit) this.save();
    this.refresh();
  }

  private save(): void {
    if (this.preferred === null) return;
    try { window.localStorage.setItem(this.opts.key, String(this.preferred)); } catch { /* Keep the width in memory. */ }
  }

  private reset(): void {
    this.finish(false);
    this.preferred = null;
    try { window.localStorage.removeItem(this.opts.key); } catch { /* The inline removal below is the reset. */ }
    this.refresh();
  }

  private key(event: KeyboardEvent): void {
    if (event.key === "Escape" && this.drag !== null) { event.preventDefault(); this.finish(false); return; }
    if (!this.enabled() || event.altKey || event.ctrlKey || event.metaKey) return;
    const current = this.preferred === null ? this.panel.getBoundingClientRect().width : this.clamp(this.preferred);
    let width: number;
    switch (event.key) {
      case "ArrowLeft": width = current + (event.shiftKey ? 48 : 16); break;
      case "ArrowRight": width = current - (event.shiftKey ? 48 : 16); break;
      case "Home": width = this.bounds().min; break;
      case "End": width = this.bounds().max; break;
      case "Enter": event.preventDefault(); this.reset(); return;
      default: return;
    }
    event.preventDefault();
    this.finish(false);
    this.preferred = this.clamp(width);
    this.save();
    this.refresh();
  }

  dispose(): void {
    this.finish(false);
    this.disposed = true;
    this.observer.disconnect();
    for (const remove of this.cleanup) remove();
    this.panel.style.removeProperty("--docked-panel-w");
  }
}
