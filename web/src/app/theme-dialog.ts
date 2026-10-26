// The custom terminal theme dialog: the
// six-slot editor opened from the More menu's Terminal theme ▸ Custom…
// entry. This module owns the markup, the draft state and the
// pointer-driven color wheel; live.ts and preview.ts both mount it and
// differ only in what Apply does — the live workspace writes the draft
// through terminal/theme.ts's storage and selects the custom theme, the
// preview paints its fixture locally and persists nothing.
//
// The live preview renders the draft through the same slot pipeline the
// terminal pane reads (terminal/palette.ts customThemeChromeSlots), so
// what the preview shows is exactly what Apply paints.
import { activateDialog } from "../ui/dialog-focus.ts";
import { openModalScreen, type ModalScreen } from "../ui/modal-host.ts";
import { DIALOG_FOOT, GHOST_MD_BUTTON, PRIMARY_MD_BUTTON, SECONDARY_MD_BUTTON, dialogHead } from "../ui/controls.ts";
import * as hints from "../lib/hints.ts";
import { normalizeHex } from "../terminal/theme.ts";
import { customThemeChromeSlots } from "../terminal/palette.ts";
import {
  CUSTOM_SLOT_LABELS,
  DEFAULT_CUSTOM_SLOTS,
  TERMINAL_THEMES,
  TERMINAL_THEME_LABELS,
  TERMINAL_THEME_NAMES,
  type CustomThemeSlots,
  type TerminalThemeName,
} from "../terminal/themes.ts";
import type { ITheme } from "@xterm/xterm";

type SlotKey = keyof CustomThemeSlots;

// The six editable slots in swatch order: the short label under the
// swatch, and the ITheme field each named theme contributes as a "from"
// candidate (dim text maps to the palettes' white slot).
interface SlotDef {
  readonly key: SlotKey;
  readonly short: string;
  readonly candidate: (theme: ITheme) => string;
}

const SLOT_DEFS: readonly SlotDef[] = [
  { key: "background", short: "bg", candidate: (t) => t.background ?? DEFAULT_CUSTOM_SLOTS.background },
  { key: "text", short: "text", candidate: (t) => t.foreground ?? DEFAULT_CUSTOM_SLOTS.text },
  { key: "dimText", short: "dim", candidate: (t) => t.white ?? DEFAULT_CUSTOM_SLOTS.dimText },
  { key: "accent", short: "accent", candidate: (t) => t.cursor ?? DEFAULT_CUSTOM_SLOTS.accent },
  { key: "warning", short: "warn", candidate: (t) => t.yellow ?? DEFAULT_CUSTOM_SLOTS.warning },
  { key: "cyan", short: "cyan", candidate: (t) => t.cyan ?? DEFAULT_CUSTOM_SLOTS.cyan },
];

const NAMED_THEMES = TERMINAL_THEME_NAMES.filter((name) => name !== "custom");

// The selected slot's ring and every other well's resting hairline.
const RING_SELECTED = "0 0 0 1px var(--color-surface-card),0 0 0 3px var(--color-accent)";
const RING_RESTING = "inset 0 0 0 1px var(--color-border-strong)";

function slotDef(key: SlotKey): SlotDef {
  return SLOT_DEFS.find((d) => d.key === key) ?? SLOT_DEFS[0]!;
}

// bareHex is the hex field's model: six hex characters, uppercase, no #.
export function bareHex(value: string): string {
  return value.replace(/[^0-9a-fA-F]/g, "").slice(0, 6).toUpperCase();
}

export interface WheelHsv {
  h: number;
  s: number;
  v: number;
}

// hexToHsv/hsvToHex are the wheel's only color math (the reference
// dialog's formulas verbatim).
export function hexToHsv(hex: string): WheelHsv {
  const n = parseInt(hex.slice(1), 16);
  const r = (n >> 16) / 255;
  const g = ((n >> 8) & 255) / 255;
  const b = (n & 255) / 255;
  const mx = Math.max(r, g, b);
  const mn = Math.min(r, g, b);
  const d = mx - mn;
  let h = 0;
  if (d !== 0) {
    h = mx === r ? 60 * (((g - b) / d) % 6) : mx === g ? 60 * ((b - r) / d + 2) : 60 * ((r - g) / d + 4);
  }
  return { h: (h + 360) % 360, s: mx !== 0 ? d / mx : 0, v: mx };
}

export function hsvToHex(h: number, s: number, v: number): string {
  const c = v * s;
  const x = c * (1 - Math.abs(((h / 60) % 2) - 1));
  const m = v - c;
  const t = h < 60 ? [c, x, 0] : h < 120 ? [x, c, 0] : h < 180 ? [0, c, x]
    : h < 240 ? [0, x, c] : h < 300 ? [x, 0, c] : [c, 0, x];
  return t.map((u) => Math.round((u + m) * 255).toString(16).padStart(2, "0")).join("").toUpperCase();
}

export interface ThemeDialogState {
  // slots is the draft — always valid six-hex colors, so the preview can
  // never render a value Apply would refuse to persist.
  slots: CustomThemeSlots;
  selected: SlotKey;
  hexText: string;
  pickerOpen: boolean;
  // hsv is the wheel's own state; it stays authoritative only while it
  // still resolves to the selected slot's hex, so a typed color replaces
  // it on the next read.
  hsv: (WheelHsv & { slot: SlotKey }) | null;
}

// liveHsv derives the wheel position from state: the stored wheel state
// when it still matches the slot, else the slot's own hue.
function liveHsv(state: ThemeDialogState): WheelHsv {
  const hex = state.slots[state.selected];
  const st = state.hsv;
  if (st !== null && st.slot === state.selected && `#${hsvToHex(st.h, st.s, st.v)}` === hex) {
    return st;
  }
  return hexToHsv(hex);
}

function knobLeft(hsv: WheelHsv): string {
  return (50 + hsv.s * 50 * Math.sin(hsv.h * Math.PI / 180)).toFixed(2);
}

function knobTop(hsv: WheelHsv): string {
  return (50 - hsv.s * 50 * Math.cos(hsv.h * Math.PI / 180)).toFixed(2);
}

// themeDialogHtml is the whole dialog on the shared anatomy: head with the
// close X, the preview and editor sections, and the
// Reset / Cancel / Apply theme foot. Dynamic pieces carry data-* hooks so
// the mounted dialog can paint them in place — a wheel drag must not
// re-render the element that owns the pointer capture.
export function themeDialogHtml(state: ThemeDialogState): string {
  const def = slotDef(state.selected);
  const hex = state.slots[def.key];
  const hsv = liveHsv(state);
  const chrome = customThemeChromeSlots(state.slots);
  const monoMicro = "font-mono text-[min(var(--type-mono-size),calc(var(--type-micro-size)*var(--type-mono-xh-ratio)))]";
  const swatches = SLOT_DEFS.map((d) => {
    const selected = d.key === state.selected;
    return `
        <button type="button" data-theme-slot="${d.key}" aria-label="${CUSTOM_SLOT_LABELS[d.key]}"
          data-hint="${hints.themeSlot(CUSTOM_SLOT_LABELS[d.key].toLowerCase())}"
          class="flex min-w-0 flex-1 cursor-pointer flex-col items-stretch gap-[5px] border-0 bg-transparent p-0">
          <span data-slot-well="${d.key}" class="theme-slot-well"
            style="background:${state.slots[d.key]};box-shadow:${selected ? RING_SELECTED : RING_RESTING}"></span>
          <span data-slot-short="${d.key}" class="overflow-hidden text-ellipsis whitespace-nowrap text-left text-caption"
            style="color:${selected ? "var(--color-text-primary)" : "var(--color-text-tertiary)"}">${d.short}</span>
        </button>`;
  }).join("");
  const candidates = NAMED_THEMES.map((name) => `
          <button type="button" data-theme-candidate="${name}"
            aria-label="${TERMINAL_THEME_LABELS[name]} ${def.short}" data-hint="${hints.themeSetFrom(def.short, TERMINAL_THEME_LABELS[name])}"
            class="h-[19px] w-[19px] flex-none cursor-pointer rounded-mark border border-border p-0"
            style="background:${def.candidate(TERMINAL_THEMES[name])}"></button>`).join("");
  return `
        <div data-theme-dialog data-phone-fullscreen role="dialog" aria-modal="true" aria-label="Custom terminal theme" class="dialog">
          ${dialogHead("Custom terminal theme", { label: "Close", attr: "data-theme-dialog-cancel" })}
          <div class="mx-6 overflow-hidden rounded-card border border-border">
            <div data-theme-preview
              class="theme-dialog-preview font-mono text-[min(var(--type-mono-size),calc(var(--type-caption-size)*var(--type-mono-xh-ratio)))] leading-[1.62]"
              style="background:${chrome.bg}">
              <div data-preview="faint" style="color:${chrome.faint}">· · · scrollback continues above · · ·</div>
              <div><span data-preview="accent" style="color:${chrome.accent}">●</span><span data-preview="fg" style="color:${chrome.fg}"> Running: go test ./internal/provider/…</span></div>
              <div data-preview="dim" style="color:${chrome.dim}">  ⎿ ok · 214 tests · 2m 14s</div>
              <div><span data-preview="warn" style="color:${chrome.warn}">▲</span><span data-preview="fg" style="color:${chrome.fg}"> 2 descriptors missing a timeout</span></div>
              <div><span data-preview="cyan" style="color:${chrome.cyan}">~/field-notes</span><span data-preview="dim" style="color:${chrome.dim}"> · </span><span data-preview="cyan" style="color:${chrome.cyan}">feat/task7</span></div>
              <div class="mt-[5px]"><span data-preview="accent" style="color:${chrome.accent}">❯</span><span data-preview="fg" style="color:${chrome.fg}"> </span><span data-preview="cursor" class="theme-dialog-cursor" style="background:${chrome.accent}"></span></div>
            </div>
          </div>
          <div class="flex gap-1.5 px-6 pt-4">${swatches}
          </div>
          <div class="flex items-center gap-2.5 px-6 pt-4">
            <span data-slot-label class="flex-none text-body-sm text-text-primary">${CUSTOM_SLOT_LABELS[def.key]}</span>
            <span class="ml-auto"></span>
            <span class="flex-none ${monoMicro} text-text-tertiary">#</span>
            <span class="relative block flex-none">
              <input data-theme-hex type="text" value="${state.hexText}" maxlength="6" spellcheck="false"
                autocomplete="off" autocapitalize="none" aria-label="${CUSTOM_SLOT_LABELS[def.key]} hex"
                data-hint="${hints.TYPE_HEX_COLOR}"
                class="block h-[30px] w-[82px] rounded-control-sm border bg-surface px-[9px] ${monoMicro} uppercase text-text-primary outline-none"
                style="border-color:${state.pickerOpen ? "var(--color-accent)" : "var(--color-border)"}">
            </span>
          </div>
          <div class="px-6">
          <span data-theme-picker ${state.pickerOpen ? "" : "hidden"}>
                                <span class="mt-[11px] block rounded-card border border-border bg-surface p-3">
                  <span data-theme-wheel data-hint="${hints.PICK_HUE_SATURATION}" class="theme-wheel">
                    <span data-wheel-dim class="theme-wheel-dim" style="opacity:${(1 - hsv.v).toFixed(3)}"></span>
                    <span data-wheel-knob class="theme-wheel-knob"
                      style="left:${knobLeft(hsv)}%;top:${knobTop(hsv)}%;background:${hex}"></span>
                  </span>
                  <span data-theme-value data-hint="${hints.SET_HOW_LIGHT}" class="theme-value-track"
                    style="background:linear-gradient(to right,#000000,#${hsvToHex(hsv.h, hsv.s, 1)})">
                    <span data-value-thumb class="theme-value-thumb" style="left:${(hsv.v * 100).toFixed(2)}%;background:${hex}"></span>
                  </span>
                </span>
              </span>
          </div>
          <div class="flex items-center gap-[9px] px-6 pb-5 pt-[11px]">
            <span class="flex-none text-caption text-text-tertiary">from</span>${candidates}
          </div>
          ${DIALOG_FOOT(`
            <button type="button" data-theme-reset data-hint="${hints.RETURN_SLOTS_TO_SHIPPED}"
              class="${GHOST_MD_BUTTON}">Reset</button>
            <span class="ml-auto"></span>
            <button type="button" data-theme-dialog-cancel data-hint="${hints.DISCARD_DRAFT_CLOSE}"
              class="${SECONDARY_MD_BUTTON}">Cancel</button>
            <button type="button" data-theme-apply data-hint="${hints.SAVE_SLOTS_USE_THEME}"
              class="${PRIMARY_MD_BUTTON}">Apply theme</button>`)}
        </div>`;
}

function clamp(n: number): number {
  return Math.max(0, Math.min(1, n));
}

// ThemeDialog mounts the editor into the modals region and owns it until
// dismissal. onApply receives the draft exactly once, from Apply theme.
export class ThemeDialog {
  private readonly root: HTMLElement;
  private readonly screen: ModalScreen;
  private readonly onApply: (slots: CustomThemeSlots) => void;
  private readonly releaseFocus: () => void;
  private slots: CustomThemeSlots;
  private selected: SlotKey;
  private hexText: string;
  private pickerOpen = false;
  private hsv: (WheelHsv & { slot: SlotKey }) | null = null;

  constructor(
    host: HTMLElement,
    initial: CustomThemeSlots,
    onApply: (slots: CustomThemeSlots) => void,
  ) {
    this.slots = { ...initial };
    this.selected = "background";
    this.hexText = bareHex(initial.background);
    this.onApply = onApply;
    this.screen = openModalScreen(host, { dismiss: () => this.close() });
    this.root = this.screen.element;
    this.root.innerHTML = themeDialogHtml(this.state());
    const dialog = this.root.querySelector<HTMLElement>("[data-theme-dialog]")!;
    // Container focus at open: the dialog draws no focus mark, and an
    // auto-focused control would wear the keyboard wash immediately.
    this.releaseFocus = activateDialog(dialog, () => this.close(), ":dialog");
    this.root.querySelectorAll("[data-theme-dialog-cancel]").forEach((button) => {
      button.addEventListener("click", () => this.close());
    });
    this.root.querySelectorAll<HTMLButtonElement>("[data-theme-slot]").forEach((button) => {
      button.addEventListener("click", () => {
        this.selected = button.dataset.themeSlot as SlotKey;
        this.hexText = bareHex(this.slots[this.selected]);
        this.paint();
      });
    });
    this.root.querySelectorAll<HTMLButtonElement>("[data-theme-candidate]").forEach((button) => {
      button.addEventListener("click", () => {
        const theme = TERMINAL_THEMES[button.dataset.themeCandidate as TerminalThemeName];
        this.writeSlot(slotDef(this.selected).candidate(theme));
      });
    });
    this.root.querySelector("[data-theme-reset]")?.addEventListener("click", () => {
      this.slots = { ...DEFAULT_CUSTOM_SLOTS };
      this.selected = "background";
      this.hsv = null;
      this.hexText = bareHex(this.slots.background);
      this.paint();
    });
    this.root.querySelector("[data-theme-apply]")?.addEventListener("click", () => {
      const applied = { ...this.slots };
      this.onApply(applied);
      this.close();
    });
    const hexInput = this.root.querySelector<HTMLInputElement>("[data-theme-hex]")!;
    // The picker is a block in the dialog body, not a popover: a popover
    // cannot escape .dialog, which carries max-height and overflow:auto, and
    // an ancestor that scrolls clips an absolutely positioned descendant
    // whatever its z-index. Clipped, the disc lost its lower half and both
    // knobs with it. Being inline, it also needs no outside-click catcher —
    // it opens with the field and closes with the dialog.
    hexInput.addEventListener("focus", () => {
      this.pickerOpen = true;
      this.paint();
    });
    hexInput.addEventListener("input", () => {
      this.hexText = bareHex(hexInput.value);
      hexInput.value = this.hexText;
      this.commitText();
      this.paint(true);
    });
    hexInput.addEventListener("change", () => {
      this.hexText = bareHex(hexInput.value);
      // An entry that never became valid snaps back to the slot's value.
      if (!this.commitText()) this.hexText = bareHex(this.slots[this.selected]);
      this.paint();
    });
    const wheel = this.root.querySelector<HTMLElement>("[data-theme-wheel]")!;
    this.bindWheel(wheel, (event) => {
      const r = wheel.getBoundingClientRect();
      const dx = event.clientX - r.left - r.width / 2;
      const dy = event.clientY - r.top - r.height / 2;
      const h = (Math.atan2(dx, -dy) * 180 / Math.PI + 360) % 360;
      const s = clamp(Math.hypot(dx, dy) / (r.width / 2));
      return { h, s, v: liveHsv(this.state()).v };
    });
    const track = this.root.querySelector<HTMLElement>("[data-theme-value]")!;
    this.bindWheel(track, (event) => {
      const r = track.getBoundingClientRect();
      const current = liveHsv(this.state());
      return { h: current.h, s: current.s, v: clamp((event.clientX - r.left) / r.width) };
    });
  }

  private state(): ThemeDialogState {
    return {
      slots: this.slots,
      selected: this.selected,
      hexText: this.hexText,
      pickerOpen: this.pickerOpen,
      hsv: this.hsv,
    };
  }

  // commitText applies the field's text when it names a color. It returns
  // whether the text is a usable six-character hex.
  private commitText(): boolean {
    const hex = normalizeHex(`#${this.hexText}`);
    if (hex === null) return false;
    this.slots[this.selected] = hex;
    this.hsv = null;
    return true;
  }

  // writeSlot commits a known-valid color (wheel, candidate) and keeps the
  // field in step.
  private writeSlot(hex: string): void {
    this.slots[this.selected] = hex;
    this.hexText = bareHex(hex);
    this.hsv = null;
    this.paint();
  }

  // bindWheel drives one pointer surface: pointerdown captures and applies
  // at once, pointermove applies while the primary button is held.
  private bindWheel(
    surface: HTMLElement,
    at: (event: PointerEvent) => WheelHsv,
  ): void {
    const apply = (event: PointerEvent): void => {
      if (event.buttons !== 1) return;
      const hsv = at(event);
      this.hsv = { slot: this.selected, ...hsv };
      const hex = `#${hsvToHex(hsv.h, hsv.s, hsv.v)}`;
      this.slots[this.selected] = hex;
      this.hexText = hex.slice(1);
      this.paint();
    };
    surface.addEventListener("pointerdown", (event) => {
      surface.setPointerCapture(event.pointerId);
      apply(event);
    });
    surface.addEventListener("pointermove", apply);
  }

  private close(): void {
    this.releaseFocus();
    this.screen.close();
  }

  // paint syncs every dynamic attribute in place. skipHexField leaves the
  // field's text alone on the typing path, where the DOM already holds it.
  private paint(skipHexField = false): void {
    const def = slotDef(this.selected);
    const hex = this.slots[def.key];
    const chrome = customThemeChromeSlots(this.slots);
    const preview = this.root.querySelector<HTMLElement>("[data-theme-preview]");
    if (preview !== null) preview.style.background = chrome.bg;
    const roles: readonly [string, string][] = [
      ["fg", chrome.fg],
      ["dim", chrome.dim],
      ["faint", chrome.faint],
      ["warn", chrome.warn],
      ["cyan", chrome.cyan],
      ["accent", chrome.accent],
    ];
    for (const [role, color] of roles) {
      this.root.querySelectorAll<HTMLElement>(`[data-preview="${role}"]`).forEach((el) => {
        el.style.color = color;
      });
    }
    const cursor = this.root.querySelector<HTMLElement>('[data-preview="cursor"]');
    if (cursor !== null) cursor.style.background = chrome.accent;
    for (const d of SLOT_DEFS) {
      const selected = d.key === this.selected;
      const well = this.root.querySelector<HTMLElement>(`[data-slot-well="${d.key}"]`);
      if (well !== null) {
        well.style.background = this.slots[d.key];
        well.style.boxShadow = selected ? RING_SELECTED : RING_RESTING;
      }
      const label = this.root.querySelector<HTMLElement>(`[data-slot-short="${d.key}"]`);
      if (label !== null) {
        label.style.color = selected ? "var(--color-text-primary)" : "var(--color-text-tertiary)";
      }
    }
    this.root.querySelector<HTMLElement>("[data-slot-label]")!.textContent = CUSTOM_SLOT_LABELS[def.key];
    const hexInput = this.root.querySelector<HTMLInputElement>("[data-theme-hex]")!;
    if (!skipHexField) hexInput.value = this.hexText;
    hexInput.setAttribute("aria-label", `${CUSTOM_SLOT_LABELS[def.key]} hex`);
    hexInput.style.borderColor = this.pickerOpen ? "var(--color-accent)" : "var(--color-border)";
    const picker = this.root.querySelector<HTMLElement>("[data-theme-picker]");
    if (picker !== null) picker.hidden = !this.pickerOpen;
    const hsv = liveHsv(this.state());
    const dim = this.root.querySelector<HTMLElement>("[data-wheel-dim]");
    if (dim !== null) dim.style.opacity = (1 - hsv.v).toFixed(3);
    const knob = this.root.querySelector<HTMLElement>("[data-wheel-knob]");
    if (knob !== null) {
      knob.style.left = `${knobLeft(hsv)}%`;
      knob.style.top = `${knobTop(hsv)}%`;
      knob.style.background = hex;
    }
    const track = this.root.querySelector<HTMLElement>("[data-theme-value]");
    if (track !== null) {
      track.style.background = `linear-gradient(to right,#000000,#${hsvToHex(hsv.h, hsv.s, 1)})`;
    }
    const thumb = this.root.querySelector<HTMLElement>("[data-value-thumb]");
    if (thumb !== null) {
      thumb.style.left = `${(hsv.v * 100).toFixed(2)}%`;
      thumb.style.background = hex;
    }
    for (const name of NAMED_THEMES) {
      const button = this.root.querySelector<HTMLButtonElement>(`[data-theme-candidate="${name}"]`);
      if (button === null) continue;
      button.style.background = def.candidate(TERMINAL_THEMES[name]);
      button.setAttribute("aria-label", `${TERMINAL_THEME_LABELS[name]} ${def.short}`);
      button.dataset.hint = `Set ${def.short} from ${TERMINAL_THEME_LABELS[name]}`;
    }
  }
}
