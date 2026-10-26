import type { ITerminalOptions, ITheme } from "@xterm/xterm";

export const defaultScrollbackLines = 1_000;

// Base xterm.js constructor options shared by every mounted terminal.
export const baseTerminalOptions = {
  cursorBlink: false,
  scrollback: defaultScrollbackLines,
  // macOptionClickForcesSelection: when an application has requested mouse
  // reports, xterm normally forwards all mouse events to the application and
  // the browser cannot select text. On macOS the only way to reach a native
  // selection is Option+drag, but xterm disables that by default. Enabling it
  // means Option+click is no longer reported to the application as an alt-
  // modified mouse event.
  macOptionClickForcesSelection: true,
  // xterm selects the word under a right-click on macOS by default, which
  // replaces the user's selection before the terminal menu opens and turns
  // its Copy into a one-word copy. A right-click must leave the selection,
  // or its absence, as the user left it.
  rightClickSelectsWord: false,
} satisfies ITerminalOptions;

export function composeTerminalOptions(
  fontFamily: string,
  fontSize: number,
  theme: ITheme,
  letterSpacing = 0,
  fontWeight: number = 400,
): ITerminalOptions {
  return {
    ...baseTerminalOptions,
    fontFamily,
    fontSize,
    theme,
    letterSpacing,
    fontWeight,
  };
}

// The mono weight the stylesheet declares, as the number xterm wants. The
// emulator has its own fontWeight option and never reads the cascade, so a
// face whose regular is not the weight the design sets (the token exists
// for that) would render in the terminal at 400 regardless. Anything not a
// plain weight is 400.
export function monoWeight(declared: string): number {
  const n = Number(declared.trim());
  return Number.isInteger(n) && n >= 100 && n <= 900 ? n : 400;
}

// The mono tracking the stylesheet declares, as the pixels xterm wants.
// xterm draws on a canvas and never sees CSS letter-spacing, so a mono face
// tuned with tracking would render a narrower cell in the terminal than
// everywhere else. An em value scales
// with the font size; a px value is taken as it is; anything else is zero.
export function monoTrackingPx(tracking: string, fontSize: number): number {
  const m = /^\s*(-?[\d.]+)\s*(em|px)?\s*$/.exec(tracking);
  if (!m) return 0;
  const n = Number(m[1]);
  if (!Number.isFinite(n)) return 0;
  return m[2] === "em" ? n * fontSize : m[2] === "px" ? n : 0;
}

// xterm's letterSpacing is device pixels, not CSS pixels: the DOM renderer
// rounds it and adds it to the device cell width before dividing by the
// device pixel ratio (DomRenderer, cell width), so 1 CSS px of tracking at
// a ratio of 2 needs 2. Measured: at 13px on a 2x display the free set's
// 1px tracking left 133 columns in a window the licensed face fills with
// 125; scaled to 2 it is 125 again.
export function deviceLetterSpacing(cssPx: number, devicePixelRatio: number): number {
  const dpr = Number.isFinite(devicePixelRatio) && devicePixelRatio > 0 ? devicePixelRatio : 1;
  return Math.round(cssPx * dpr);
}

// A display change moves the device pixel ratio under a mounted terminal —
// a window dragged to another monitor, or browser zoom — and xterm remeasures
// its cell on its own, but the letter spacing it was handed is device
// pixels for the old ratio. The ratio is watched two ways, because Chromium
// delivers a resolution media query's change event only with the next
// layout: the query (re-armed on every change, since it names the ratio it
// matches) and the window's resize, which every real display change also
// produces. Either way the caller hears only when the ratio actually moved.
// The window is injectable so the arming logic is testable without one.
export interface MediaQueryLike {
  addEventListener(type: "change", listener: () => void): void;
  removeEventListener(type: "change", listener: () => void): void;
}

export interface RatioWindow {
  matchMedia(query: string): MediaQueryLike;
  readonly devicePixelRatio: number;
  addEventListener(type: "resize", listener: () => void): void;
  removeEventListener(type: "resize", listener: () => void): void;
}

export function watchDevicePixelRatio(
  onChange: (devicePixelRatio: number) => void,
  win: RatioWindow,
): { dispose(): void } {
  let query: MediaQueryLike | null = null;
  let known = win.devicePixelRatio;
  let disposed = false;
  const check = () => {
    if (disposed) return;
    const now = win.devicePixelRatio;
    if (now === known) return;
    known = now;
    arm();
    onChange(now);
  };
  const arm = () => {
    query?.removeEventListener("change", check);
    query = win.matchMedia(`(resolution: ${known}dppx)`);
    query.addEventListener("change", check);
  };
  arm();
  win.addEventListener("resize", check);
  return {
    dispose: () => {
      disposed = true;
      query?.removeEventListener("change", check);
      query = null;
      win.removeEventListener("resize", check);
    },
  };
}
