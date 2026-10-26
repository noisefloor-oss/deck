// Thin xterm.js wrapper: construction and sizing only. Rendering fidelity
// (resize, UTF-8, colors, alternate screen, bracketed paste, control keys)
// is xterm's job; this module only configures it and must never interpret
// terminal content (the authority invariant).
import { Terminal } from "@xterm/xterm";
import type { ITheme } from "@xterm/xterm";
export type { ITheme };
import { isSafeUrl } from "../lib/url.ts";
import { isWindowsClient } from "../lib/platform.ts";
import { forceSelectionModifier, nativeClipboardKey, planClipboardKey, readClipboard, viewportText, writeClipboard, type ClipboardLike } from "./copy.ts";
import { joinWrappedSelection, WrappedUrlLinkProvider } from "./links.ts";
import {
  subscribeXtermOutbound,
  type TerminalOutbound,
} from "./outbound.ts";
import {
  TouchScrollPlanner,
  AlternateTouchGesturePlanner,
  alternateTouchRoute,
  wheelReports,
  WheelScrollPlanner,
  planScrollKey,
  type ScrollCommand,
} from "./scroll.ts";
import {
  TerminalSoftKeyInput,
  type TerminalSoftKey,
} from "./soft-keys.ts";
import { composeTerminalOptions, defaultScrollbackLines, deviceLetterSpacing, monoTrackingPx, monoWeight, watchDevicePixelRatio } from "./options.ts";

export interface MountedTerminal {
  terminal: Terminal;
  fit: () => void;
  setTheme: (theme: ITheme) => void;
  setFontSize: (px: number) => void;
  onOutbound: (fn: (event: TerminalOutbound) => void) => { dispose(): void };
  // onScroll delivers scrollback commands (wheel or Shift+PageUp/PageDown
  // over the alternate screen) for the attachment to forward as scroll
  // frames. The decisions read only xterm's own emulator state, never
  // terminal content (see scroll.ts).
  onScroll: (fn: (command: ScrollCommand) => void) => { dispose(): void };
  onScrollbackChange: (fn: (active: boolean) => void) => { dispose(): void };
  activateSoftKey: (key: TerminalSoftKey) => void;
  toggleCtrlChord: () => boolean;
  cancelCtrlChord: () => void;
  onCtrlChordChange: (fn: (armed: boolean) => void) => { dispose(): void };
  focus: () => void;
  sendInput: (data: string) => void;
  // What a copy takes right now, read as ONE value: the selection when xterm
  // holds one, and the visible screen when it does not. One rule for both
  // callers — the copy chord and the context menu's Copy row — and one
  // reading, because separate reads of "is there a selection" and "what is
  // the text" can straddle output that clears the selection, and a caller
  // that names the first while copying the second lies to the user.
  copyable: () => { fromSelection: boolean; text: string };
  // Clipboard text into the pane through xterm's own paste path, which
  // applies bracketed paste and the line-ending transforms the application
  // expects; sendInput would deliver the raw bytes instead.
  paste: (text: string) => void;
  // setSelectText routes a plain drag to the browser while the application
  // has asked for mouse reports: while on, the press that would have been
  // reported to the application starts a native selection instead — the
  // same path Shift+drag already takes. Wheel and keyboard are untouched,
  // and the normal buffer behaves as it always did. Returns the state
  // applied.
  setSelectText: (on: boolean) => boolean;
  jumpToLive: () => void;
  resetScrollback: () => void;
  dispose: () => void;
}
// The platform decides the clipboard chords (copy.ts). Apple platforms take
// ⌘; everything else the Ctrl and Insert chords.
function isMacPlatform(): boolean {
  const platform = (navigator as { userAgentData?: { platform?: string } }).userAgentData?.platform ?? navigator.platform ?? "";
  return /mac|iphone|ipad|ipod/i.test(platform);
}

// xterm draws glyphs from its own fontFamily option, not the CSS cascade,
// so the stack is resolved off the mount point's computed --font-mono: the
// emulator and every other mono surface share the one declaration in
// app.css. An unresolvable declaration degrades to the generic mono at the
// stack's tail — never a different size.
function monoFontStack(mount: HTMLElement): string {
  const stack = getComputedStyle(mount).getPropertyValue("--font-mono").trim();
  return stack === "" ? "monospace" : stack;
}

// The mono tracking beside the stack, resolved the same way (see
// monoTrackingPx): the emulator's cell must agree with every other mono
// surface's advance, or a path reads one width in a panel and another in
// the terminal.
function monoTracking(mount: HTMLElement, fontSize: number): number {
  return deviceLetterSpacing(monoTrackingCssPx(mount, fontSize), window.devicePixelRatio);
}

function monoTrackingCssPx(mount: HTMLElement, fontSize: number): number {
  const declared = getComputedStyle(mount).getPropertyValue("--type-mono-tracking");
  const plain = monoTrackingPx(declared, fontSize);
  if (plain !== 0 || /^\s*(0|0em|0px)?\s*$/.test(declared)) return plain;
  // Not a plain length (the free set states it as a calc over em, rounded
  // to the pixel): let the engine resolve it at the terminal's size.
  const probe = document.createElement("span");
  probe.style.cssText = `position:absolute;visibility:hidden;font-size:${fontSize}px;letter-spacing:${declared}`;
  mount.appendChild(probe);
  try {
    const px = parseFloat(getComputedStyle(probe).letterSpacing);
    return Number.isFinite(px) ? px : 0;
  } finally {
    probe.remove();
  }
}

// The emulator measures its cell the instant it opens, against whatever the
// mono stack resolves to at that moment. A webfont still downloading measures
// as the stack's fallback, so the connect URL carries geometry for a face the
// terminal never renders in, and the first fit after the real face lands
// re-measures the cell and renegotiates cols/rows mid-session — a panel
// opening or the soft-key row mounting then resizes the PTY a second time.
// Awaiting the face before the first measurement pins the connect URL and
// every later fit to the one cell. A face that fails to download resolves
// unloaded and the measurement degrades to the stack's tail, exactly as
// monoFontStack already promises, so a broken font never blocks the mount.
export function ensureMonoFontLoaded(mount: HTMLElement, fontSize: number): Promise<void> {
  if (typeof document === "undefined" || document.fonts === undefined) {
    return Promise.resolve();
  }
  return document.fonts.load(`${fontSize}px ${monoFontStack(mount)}`, "W").then(
    () => undefined,
    () => undefined,
  );
}

// The render metrics xterm's public typings omit. One unchecked cast in
// one place — a pinned @xterm/xterm bump that moves this field leaves the
// grid unfitted and the geometry smoke failing loudly, rather than a
// scattered private reliance rotting quietly.
type XtermRenderService = {
  dimensions?: { css: { cell: { width: number; height: number } } };
  clear(): void;
};

function xtermRenderService(terminal: Terminal): XtermRenderService | undefined {
  // The field is private in the typings but stable in the pinned build.
  const core = (terminal as unknown as { _core?: { _renderService?: XtermRenderService } })._core;
  return core?._renderService;
}

export function createTerminal(
  parent: HTMLElement,
  theme: ITheme,
  fontSize: number,
): MountedTerminal {
  const terminal = new Terminal(
    composeTerminalOptions(
      monoFontStack(parent), fontSize, theme, monoTracking(parent, fontSize),
      monoWeight(getComputedStyle(parent).getPropertyValue("--type-mono-weight")),
    ),
  );

  // Links open only for http/https, and in a new browsing context. The
  // provider reassembles URLs the harness hard-wrapped over rows (links.ts),
  // so a sign-in URL longer than the window opens whole. OSC 52 stays off
  // (xterm does not enable it by default, and nothing here may turn it on).
  const openLink = (uri: string) => {
    if (isSafeUrl(uri)) {
      window.open(uri, "_blank", "noopener,noreferrer");
    }
  };
  terminal.registerLinkProvider(new WrappedUrlLinkProvider(terminal, openLink));
  terminal.open(parent);
  // The letter spacing is device pixels for the ratio at mount; a display
  // change re-derives it and refits, or the cell drifts (options.ts).
  const ratioWatch = watchDevicePixelRatio(
    () => {
      terminal.options.letterSpacing = monoTracking(parent, terminal.options.fontSize ?? fontSize);
      fitToHost();
    },
    window,
  );
  // The same reassembly for the clipboard: a selection that is one wrapped
  // URL copies as the URL. Anything else keeps xterm's native copy, which
  // preserves exactly what the user highlighted. Capture phase, on the
  // mount, so it runs before xterm's own handler on its textarea.
  const copyWrappedUrl = (event: ClipboardEvent) => {
    if (!terminal.hasSelection()) return;
    const joined = joinWrappedSelection(terminal.getSelection(), terminal.cols);
    if (joined === null || event.clipboardData === null) return;
    event.clipboardData.setData("text/plain", joined);
    event.preventDefault();
    event.stopImmediatePropagation();
  };
  parent.addEventListener("copy", copyWrappedUrl, true);
  parent.dataset.terminalScrollbackLines = String(terminal.options.scrollback);
  fitToHost();
  // The renderer publishes its cell dimensions asynchronously after
  // open(), so the first fit can run before they exist and return
  // without resizing. The grid would then stay at xterm's 80x24 default —
  // content anchored top-left with our colour filling the rest of the
  // pane, reading as a giant right-and-bottom gutter — and nothing would
  // retry it: the ResizeObserver only fires on container changes. Retry
  // on a timer (not rAF, which pauses in hidden tabs) until the first
  // measured fit; a renderer that never publishes stops the loop at the
  // cap instead of spinning.
  let fitAttempts = 0;
  const fitUntilMeasured = (): void => {
    if (fitToHost() || ++fitAttempts > 40) return;
    setTimeout(fitUntilMeasured, 50);
  };
  fitUntilMeasured();

  // fitToHost sizes the grid to the mount's whole content box. Reserving
  // scrollbar width for it would narrow the grid for nothing: xterm 6's
  // scrollbar is an absolute overlay that occupies no layout, so a
  // reserved strip only leaves the pane's ground beside the painted grid
  // — a full gutter on the self-painting providers, whose own background
  // ends at the last column. Scrolling is untouched: tmux owns the
  // alternate screen's history, and xterm's buffer still holds the normal
  // buffer's.
  function fitToHost(): boolean {
    const renderService = xtermRenderService(terminal);
    if (renderService === undefined) return false;
    const cell = renderService.dimensions?.css.cell;
    if (cell === undefined || cell.width === 0 || cell.height === 0) return false;
    const box = window.getComputedStyle(parent);
    const cols = Math.max(2, Math.floor(
      Math.max(0, parseInt(box.getPropertyValue("width"))) / cell.width,
    ));
    const rows = Math.max(1, Math.floor(
      parseInt(box.getPropertyValue("height")) / cell.height,
    ));
    if (terminal.cols === cols && terminal.rows === rows) return true;
    renderService.clear();
    terminal.resize(cols, rows);
    return true;
  }

  // Scrollback routing. The tmux client keeps xterm in the
  // alternate screen for the whole attachment, so xterm's native scrollback
  // cannot serve wheel-scrolling there; the pane's tmux history can, via
  // scroll commands the attachment forwards. When the application requests
  // mouse reports, or xterm is in its normal buffer, xterm's own handling
  // stays untouched (return true).
  let scrollSubscriber: ((command: ScrollCommand) => void) | null = null;
  let scrollbackSubscriber: ((active: boolean) => void) | null = null;
  const softKeyInput = new TerminalSoftKeyInput(
    (data) => terminal.input(data, true),
    () => terminal.modes.applicationCursorKeysMode,
  );
  const planner = new WheelScrollPlanner();
  terminal.attachCustomWheelEventHandler((event) => {
    const decision = planner.plan(event, {
      bufferType: terminal.buffer.active.type,
      mouseTracking: terminal.modes.mouseTrackingMode !== "none",
      pageLines: terminal.rows,
    });
    if (decision.kind === "native") return true;
    event.preventDefault();
    if (decision.kind === "scroll") scrollSubscriber?.(decision.command);
    return false;
  });
  const windowsClient = isWindowsClient();
  // A selection that is one URL the harness hard-wrapped over rows copies
  // as the URL (links.ts), for the chord and the context menu's Copy row
  // alike; any other selection copies exactly what the user highlighted.
  const copyable = (): { fromSelection: boolean; text: string } => {
    if (!terminal.hasSelection()) return { fromSelection: false, text: viewportText(terminal.buffer.active, terminal.rows) };
    const selection = terminal.getSelection();
    return { fromSelection: true, text: joinWrappedSelection(selection, terminal.cols) ?? selection };
  };
  terminal.attachCustomKeyEventHandler((event) => {
    if (softKeyInput.consumeKeyboardEvent(event)) {
      event.preventDefault();
      return false;
    }
    // The clipboard chords per platform (copy.ts). A copy with no selection
    // — the mouse-tracking case where none can be made — takes the visible
    // screen so harness output is never uncopyable; a copy with one takes
    // the selection and clears it, as a terminal does; paste goes through
    // xterm's own paste so it is bracketed like a native one.
    const plan = planClipboardKey(event, terminal.hasSelection(), isMacPlatform());
    if (plan !== null) {
      event.preventDefault();
      // The chord is consumed whatever the clipboard does. A clipboard the
      // page cannot use (absent in an insecure context, a denied permission)
      // fails quietly through the helpers in copy.ts; a selection is
      // cleared only after its copy landed, and only if it is still the
      // same selection, so a refused copy leaves the highlight for a retry
      // instead of sending the next Ctrl+C to the pane as an interrupt.
      const clipboard = navigator.clipboard as ClipboardLike | undefined;
      if (plan === "paste") {
        void readClipboard(clipboard).then((text) => { if (text !== null) terminal.paste(text); });
      } else {
        // One read: the selection when xterm holds one (as the URL it may
        // be, see copyable), the visible screen when it does not.
        const copy = copyable();
        const selected = copy.fromSelection ? terminal.getSelection() : null;
        void writeClipboard(clipboard, copy.text).then((landed) => {
          if (landed && selected !== null && terminal.getSelection() === selected) terminal.clearSelection();
        });
      }
      return false;
    }
      if (nativeClipboardKey(event, terminal.hasSelection(), windowsClient)) return false;
      const command = planScrollKey(event, {
      bufferType: terminal.buffer.active.type,
      pageLines: terminal.rows,
    });
    if (command === null) return true;
    event.preventDefault();
    scrollSubscriber?.(command);
    return false;
  });

  const touchPlanner = new TouchScrollPlanner();
  const alternateTouchPlanner = new AlternateTouchGesturePlanner();
  const touchMedia = window.matchMedia("(pointer: coarse) and (max-width: 48rem)");
  let touchID: number | null = null;
  // Where this gesture's wheel reports say the pointer is. A wheel does not
  // travel while it turns, so every report in one drag carries the position
  // the finger started from. Reporting the moving finger instead walks the
  // events down the pane, and an application that routes a wheel by position
  // hands the later ones to whatever occupies the bottom rows.
  let wheelAnchor: { clientX: number; clientY: number } | null = null;
  // Every path that gives up a gesture ends it here. Clearing the anchor
  // beside the planners keeps them one fact rather than two that can drift:
  // a new exit added later cannot clear one and leave the other.
  const endGesture = () => {
    touchID = null;
    wheelAnchor = null;
    touchPlanner.reset();
    alternateTouchPlanner.reset();
  };
  const touchPoint = (event: TouchEvent): Touch | null => {
    if (touchID === null) return null;
    for (const touch of event.touches) {
      if (touch.identifier === touchID) return touch;
    }
    return null;
  };
  const touchStart = (event: TouchEvent) => {
    if (!touchMedia.matches || event.touches.length !== 1 || terminal.hasSelection()) {
      endGesture();
      return;
    }
    const touch = event.touches[0]!;
    touchID = touch.identifier;
    wheelAnchor = { clientX: touch.clientX, clientY: touch.clientY };
    alternateTouchPlanner.reset();
    touchPlanner.start({ clientX: touch.clientX, clientY: touch.clientY, timeStamp: event.timeStamp });
  };
  const touchMove = (event: TouchEvent) => {
    if (event.touches.length !== 1) {
      endGesture();
      return;
    }
    const touch = touchPoint(event);
    if (touch === null) return;
    const decision = touchPlanner.move(
      { clientX: touch.clientX, clientY: touch.clientY, timeStamp: event.timeStamp },
      { selectionActive: terminal.hasSelection() },
    );
    if (decision.kind === "native") {
      // The planner has reset itself and handed the gesture back to the page.
      endGesture();
      return;
    }
    event.preventDefault();
    if (decision.kind !== "scroll") return;
    if (terminal.buffer.active.type === "normal") {
      terminal.scrollLines(decision.command.direction === "up"
        ? -decision.command.lines
        : decision.command.lines);
      return;
    }
    // A mouse-tracking application owns its own transcript, and the tmux
    // history a copy-mode scroll would move through is empty - the drag
    // becomes the wheel reports the desktop's wheel already sends.
    if (alternateTouchRoute(terminal.modes.mouseTrackingMode !== "none") === "mouse-reports") {
      // No anchor means no gesture established this reporting position.
      // Falling back to the moving finger would restore exactly the walking
      // this fixes, so report nothing instead.
      if (wheelAnchor === null) return;
      const box = element.getBoundingClientRect();
      const at = wheelAnchor;
      const reports = wheelReports(decision.command, {
        column: ((at.clientX - box.left) / Math.max(1, box.width)) * terminal.cols + 1,
        row: ((at.clientY - box.top) / Math.max(1, box.height)) * terminal.rows + 1,
      }, { columns: terminal.cols, rows: terminal.rows });
      for (const report of reports) terminal.input(report, true);
      return;
    }
    const command = alternateTouchPlanner.plan(decision.command);
    if (command !== null) scrollSubscriber?.(command);
  };
  const touchEnd = (event: TouchEvent) => {
    if (touchID === null) return;
    const ended = [...event.changedTouches].some((touch) => touch.identifier === touchID);
    if (!ended) return;
    if (touchPlanner.end()) event.preventDefault();
    endGesture();
  };
  const touchCancel = () => {
    endGesture();
  };
  const element = terminal.element!;
  element.addEventListener("touchstart", touchStart, { passive: true });
  element.addEventListener("touchmove", touchMove, { passive: false });
  element.addEventListener("touchend", touchEnd, { passive: false });
  element.addEventListener("touchcancel", touchCancel);
  let selectText = false;
  // xterm decides between reporting a press to the application and starting
  // a native selection when the press lands, and the modifier it reads for
  // the native path while mouse reporting is on is the platform's
  // force-selection one. A DOM modifier is a read-only getter, so the press
  // is marked by shadowing that property with an own getter in the capture
  // phase — before xterm sees it — and only presses over an application
  // that asked for mouse reports are marked, so nothing changes when
  // nothing tracks the mouse.
  const selectPress = (event: MouseEvent): void => {
    if (!selectText || event.type !== "mousedown" || terminal.modes.mouseTrackingMode === "none") return;
    Object.defineProperty(event, forceSelectionModifier(isMacPlatform()), { get: () => true });
  };
  element.addEventListener("mousedown", selectPress, true);
  const viewportSubscription = terminal.onScroll(() => {
    if (terminal.buffer.active.type !== "normal") return;
    scrollbackSubscriber?.(
      terminal.buffer.active.viewportY < terminal.buffer.active.baseY,
    );
  });

  const resetScrollback = () => {
    endGesture();
    if (terminal.buffer.active.type === "normal") {
      terminal.scrollToBottom();
      scrollbackSubscriber?.(false);
    }
  };

  return {
    terminal,
    fit: fitToHost,
    setTheme: (next) => {
      terminal.options.theme = next;
    },
    setFontSize: (px) => {
      terminal.options.fontSize = px;
      terminal.options.letterSpacing = monoTracking(parent, px);
    },
    onOutbound: (fn) => subscribeXtermOutbound(terminal, fn),
    onScroll: (fn) => {
      scrollSubscriber = fn;
      return {
        dispose: () => {
          if (scrollSubscriber === fn) scrollSubscriber = null;
        },
      };
    },
    onScrollbackChange: (fn) => {
      scrollbackSubscriber = fn;
      return {
        dispose: () => {
          if (scrollbackSubscriber === fn) scrollbackSubscriber = null;
        },
      };
    },
    activateSoftKey: (key) => softKeyInput.activate(key),
    toggleCtrlChord: () => softKeyInput.toggleCtrl(),
    cancelCtrlChord: () => softKeyInput.cancelCtrl(),
    onCtrlChordChange: (fn) => softKeyInput.onCtrlChange(fn),
    focus: () => terminal.focus(),
    sendInput: (data) => terminal.input(data, true),
    copyable,
    paste: (text) => terminal.paste(text),
    setSelectText: (on) => {
      selectText = on;
      return selectText;
    },
    jumpToLive: () => {
      endGesture();
      // The normal buffer's viewport is local xterm state and can be
      // projected immediately. Alternate-buffer scrollback belongs to
      // tmux, so its pill remains until the server acknowledges the
      // post-command pane state.
      if (terminal.buffer.active.type === "normal") {
        terminal.scrollToBottom();
        scrollbackSubscriber?.(false);
      }
      scrollSubscriber?.({ direction: "live" });
    },
    resetScrollback,
    dispose: () => {
      element.removeEventListener("touchstart", touchStart);
      element.removeEventListener("touchmove", touchMove);
      element.removeEventListener("touchend", touchEnd);
      element.removeEventListener("touchcancel", touchCancel);
      viewportSubscription.dispose();
      parent.removeEventListener("copy", copyWrappedUrl, true);
      element.removeEventListener("mousedown", selectPress, true);
      ratioWatch.dispose();
      softKeyInput.dispose();
      delete parent.dataset.terminalScrollbackLines;
      terminal.dispose();
    },
  };
}
