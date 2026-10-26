// Terminal clipboard. A selection copies natively: xterm's textarea owns
// the clipboard once something is selected. Two things break that for real
// people. When the CLI captures the mouse (TUI harnesses enable mouse
// tracking), xterm suppresses selection entirely and on macOS offers no
// escape hatch — upstream's force-selection path is Shift+drag off-mac and
// Alt+click on mac behind macOptionClickForcesSelection. And outside macOS
// the chords people actually press are not ⌘C: Ctrl+C is SIGINT in every
// terminal, Ctrl+Shift+C is Chrome's inspect-element on Windows, and the
// Insert-key pair is what Windows Terminal and PuTTY taught.
//
// So the planner names the chords per platform and leaves the rest to the
// engine. Copy with no selection copies the visible screen out of the
// client-side buffer — already in the browser, painted by the tmux pane's
// bytes, so no server round trip and no new authority. Copy with a selection
// copies the selection (Ctrl+C then never reaches the pane as SIGINT while
// text is selected, which is Windows Terminal's rule and what the user
// meant). Paste reads the clipboard into the pane through xterm's own paste
// path, bracketed like a native paste.

export interface CopyKeyInput {
  type: string;
  key: string;
  metaKey: boolean;
  ctrlKey: boolean;
  altKey: boolean;
  shiftKey: boolean;
}

export type ClipboardPlan = "copy-selection" | "copy-viewport" | "paste" | null;

// planClipboardKey: which clipboard action this keydown is, or null when the
// key is the pane's (or the browser's). isMac selects the chord set.
//   mac      ⌘C with no selection → viewport; with one it is native (xterm's
//            copy handler fills the clipboard from its own selection on the
//            copy event). ⌘V is native.
//   others   Ctrl+C with a selection → selection; without one it is SIGINT
//            and must reach the pane untouched. Ctrl+Shift+C → selection, or
//            the viewport without one (Chrome would otherwise take it for
//            the element inspector). Ctrl+Shift+V and Shift+Insert → paste.
//            Ctrl+V is native.
//   both     Ctrl+Insert, the classic terminal copy chord for a keyboard
//            without ⌘: the viewport with no selection; with one, native on
//            a Mac and the selection elsewhere.
export function planClipboardKey(event: CopyKeyInput, hasSelection: boolean, isMac: boolean): ClipboardPlan {
  if (event.type !== "keydown") return null;
  if (event.altKey) return null;
  const key = event.key.length === 1 ? event.key.toLowerCase() : event.key;
  const insert = key === "Insert" && event.ctrlKey && !event.metaKey && !event.shiftKey;
  if (isMac) {
    const command = key === "c" && event.metaKey && !event.ctrlKey && !event.shiftKey;
    if ((command || insert) && !hasSelection) return "copy-viewport";
    return null;
  }
  if (event.metaKey) return null;
  if (event.ctrlKey && key === "c") {
    if (event.shiftKey) return hasSelection ? "copy-selection" : "copy-viewport";
    return hasSelection ? "copy-selection" : null;
  }
  if (insert) return hasSelection ? "copy-selection" : "copy-viewport";
  if (event.ctrlKey && event.shiftKey && key === "v") return "paste";
  if (event.shiftKey && !event.ctrlKey && key === "Insert") return "paste";
  return null;
}


// Returning from xterm's key handler without preventDefault lets the browser
// raise copy/paste instead of xterm sending Ctrl+C or Ctrl+V to the CLI.
export function nativeClipboardKey(event: CopyKeyInput, hasSelection: boolean, windows: boolean): boolean {
  if (!windows || event.type !== "keydown" || !event.ctrlKey || event.metaKey || event.altKey) return false;
  const key = event.key.toLowerCase();
  return key === "v" || (key === "c" && !event.shiftKey && hasSelection);
}

export interface BufferLine {
  translateToString(trimRight?: boolean): string;
}

export interface ScreenBuffer {
  // Where the top of the visible screen sits in the buffer. getLine indexes
  // the WHOLE buffer, scrollback included, so row 0 of the screen is line
  // viewportY — not line 0, which is the oldest line ever scrolled off.
  viewportY: number;
  getLine(y: number): BufferLine | undefined;
}

// viewportText assembles the visible screen; rows is the terminal's own
// viewport count, because the buffer API carries no row dimension.
// trimRight keeps the trailing spaces of the cell grid out of the clipboard;
// a fully empty trailing line still lands as a newline so vertical layout
// survives the copy.
export function viewportText(buffer: ScreenBuffer, rows: number): string {
  const lines: string[] = [];
  for (let y = 0; y < rows; y++) {
    lines.push(buffer.getLine(buffer.viewportY + y)?.translateToString(true) ?? "");
  }
  return lines.join("\n");
}

// The clipboard as the page may or may not have it. navigator.clipboard is
// undefined in an insecure context, and where it exists a call can throw
// synchronously or reject (a denied permission, a document without focus).
// A chord that reaches the clipboard must fail quietly in every one of
// those shapes: it is consumed either way, and the caller learns only
// whether the write landed — so a selection is cleared after a copy that
// happened and kept after one that did not, where retrying the chord still
// finds it rather than sending an interrupt to the pane.
export interface ClipboardLike {
  writeText?: (text: string) => Promise<void>;
  readText?: () => Promise<string>;
}

export async function writeClipboard(clipboard: ClipboardLike | undefined, text: string): Promise<boolean> {
  if (clipboard === undefined || typeof clipboard.writeText !== "function") return false;
  try {
    await clipboard.writeText(text);
    return true;
  } catch {
    return false;
  }
}

export async function readClipboard(clipboard: ClipboardLike | undefined): Promise<string | null> {
  if (clipboard === undefined || typeof clipboard.readText !== "function") return null;
  try {
    const text = await clipboard.readText();
    return typeof text === "string" && text.length > 0 ? text : null;
  } catch {
    return null;
  }
}

// The modifier xterm's SelectionService reads on its force-selection path
// while an application tracks the mouse: Option on macOS, and only behind
// the macOptionClickForcesSelection option Deck enables; Shift everywhere
// else. A press marked with this modifier selects instead of being reported.
export function forceSelectionModifier(platform: string): "altKey" | "shiftKey" {
  // xterm reads navigator.platform, not the broader Apple clipboard rule.
  // iPhone and mobile-UA iPad use Shift; marking Alt would reach the app.
  return ["Macintosh", "MacIntel", "MacPPC", "Mac68K"].includes(platform) ? "altKey" : "shiftKey";
}
