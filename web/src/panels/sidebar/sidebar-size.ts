const DEFAULT = 272;
const MIN = 240;
const MAX = 560;
const STORAGE_KEY = "noisedeck.sidebar.width";

export function sidebarPreference(value: string | null): number {
  const width = value === null || value.trim() === "" ? NaN : Number(value);
  return Number.isFinite(width) && width >= MIN && width <= MAX ? width : DEFAULT;
}

export function sidebarBounds(available: number): { min: number; max: number } {
  return { min: MIN, max: Math.max(MIN, Math.min(MAX, available - 360)) };
}

export class SidebarSize {
  private preferred = DEFAULT;
  private drag: { pointer: number; x: number; width: number; saved: number } | null = null;
  private readonly observer: ResizeObserver;
  private readonly mobile = window.matchMedia("(max-width: 47.999rem)");
  private readonly cleanup: Array<() => void> = [];
  private disposed = false;

  private readonly workspace: HTMLElement;
  private readonly handle: HTMLElement;

  constructor(workspace: HTMLElement, handle: HTMLElement) {
    this.workspace = workspace;
    this.handle = handle;
    try { this.preferred = sidebarPreference(window.localStorage.getItem(STORAGE_KEY)); } catch { /* Preferences are optional. */ }
    this.listen(handle, "pointerdown", event => this.begin(event as PointerEvent));
    this.listen(window, "pointermove", event => this.move(event as PointerEvent));
    this.listen(window, "pointerup", event => { if ((event as PointerEvent).pointerId === this.drag?.pointer) this.finish(true); });
    this.listen(window, "pointercancel", event => { if ((event as PointerEvent).pointerId === this.drag?.pointer) this.finish(false); });
    this.listen(handle, "lostpointercapture", () => this.finish(false));
    this.listen(window, "blur", () => this.finish(false));
    this.listen(handle, "keydown", event => this.key(event as KeyboardEvent));
    this.listen(handle, "dblclick", () => this.reset());
    this.listen(this.mobile, "change", () => { this.finish(false); this.refresh(); });
    this.observer = new ResizeObserver(() => { this.finish(false); this.refresh(); });
    this.observer.observe(workspace);
    this.refresh();
  }

  private listen(target: EventTarget, name: string, listener: EventListener): void {
    target.addEventListener(name, listener);
    this.cleanup.push(() => target.removeEventListener(name, listener));
  }

  private enabled(): boolean {
    return !this.mobile.matches && this.workspace.dataset.viewport !== "narrow" && this.workspace.dataset.collapsed !== "true";
  }

  private bounds(): { min: number; max: number } { return sidebarBounds(this.workspace.getBoundingClientRect().width); }
  private clamp(value: number): number { const { min, max } = this.bounds(); return Math.round(Math.max(min, Math.min(max, value))); }

  refresh(): void {
    if (this.disposed) return;
    const enabled = this.enabled();
    if (!enabled && this.drag !== null) this.finish(false);
    const width = this.mobile.matches || this.workspace.dataset.viewport === "narrow" ? DEFAULT : this.clamp(this.preferred);
    this.workspace.style.setProperty("--sidebar-w", `${width}px`);
    this.handle.hidden = !enabled;
    this.handle.tabIndex = enabled ? 0 : -1;
    this.handle.setAttribute("aria-valuemin", String(this.bounds().min));
    this.handle.setAttribute("aria-valuemax", String(this.bounds().max));
    this.handle.setAttribute("aria-valuenow", String(width));
    this.handle.setAttribute("aria-valuetext", `${width} pixels`);
    if (!enabled && document.activeElement === this.handle) this.workspace.querySelector<HTMLElement>('[data-region="sidebar-expand"]')?.focus();
  }

  private begin(event: PointerEvent): void {
    if (!this.enabled() || event.button !== 0 || !event.isPrimary || this.drag !== null) return;
    event.preventDefault();
    this.drag = { pointer: event.pointerId, x: event.clientX, width: this.clamp(this.preferred), saved: this.preferred };
    this.workspace.dataset.sidebarResizing = "true";
    try { this.handle.setPointerCapture(event.pointerId); } catch { this.finish(false); }
  }

  private move(event: PointerEvent): void {
    if (this.drag === null || event.pointerId !== this.drag.pointer) return;
    if (!this.enabled()) { this.finish(false); return; }
    this.preferred = this.clamp(this.drag.width + event.clientX - this.drag.x);
    this.refresh();
  }

  private finish(commit: boolean): void {
    const drag = this.drag;
    if (drag === null) return;
    this.drag = null;
    delete this.workspace.dataset.sidebarResizing;
    if (!commit) this.preferred = drag.saved;
    try { if (this.handle.hasPointerCapture(drag.pointer)) this.handle.releasePointerCapture(drag.pointer); } catch { /* Capture may already be gone. */ }
    if (commit) this.save();
    this.refresh();
  }

  private save(): void { try { window.localStorage.setItem(STORAGE_KEY, String(this.preferred)); } catch { /* Keep the current width in memory. */ } }
  private reset(): void { this.finish(false); this.preferred = DEFAULT; this.save(); this.refresh(); }

  private key(event: KeyboardEvent): void {
    if (event.key === "Escape" && this.drag !== null) { event.preventDefault(); this.finish(false); return; }
    if (!this.enabled() || event.altKey || event.ctrlKey || event.metaKey) return;
    let width: number;
    switch (event.key) {
      case "ArrowLeft": width = this.clamp(this.preferred) - (event.shiftKey ? 48 : 16); break;
      case "ArrowRight": width = this.clamp(this.preferred) + (event.shiftKey ? 48 : 16); break;
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
    this.workspace.style.removeProperty("--sidebar-w");
  }
}
