// The terminal's own context menu: Copy and Paste on the app's
// anchored-popover idiom, mounted by the live workspace.
//
// The browser's own menu carries only what a document selection gives it.
// xterm keeps its selection in its own model instead; on a right-click it
// moves its hidden textarea under the pointer and fills it with the
// selection text, so the browser's Copy does have something to act on when
// there IS a selection. Two things it has no verb for: the screen, and the
// case with no selection. That case is common rather than exotic — while an
// application tracks the mouse, an ordinary drag no longer selects at all,
// and only the force-selection modifier does (Shift off macOS, Option on
// macOS behind macOptionClickForcesSelection), which is the folklore this
// menu exists for. The screen is already in the client, so the menu that
// offers it has to be ours.
//
// The surface is the shared menu markup — a role=menu panel of button.menuitem
// rows — so this menu looks and behaves like every other popover here; the
// caller mounts it in the workspace popover region behind the click-catching
// backdrop and wires the two actions.
import { escapeHtml } from "../lib/escape.ts";
import * as hints from "../lib/hints.ts";

export interface TerminalMenuState {
  // Whether the reading the caller took holds a selection. A selection
  // copies exactly what is highlighted; with none the row copies the visible
  // screen, which is the fallback ⌘C and Ctrl+Insert already take.
  hasSelection: boolean;
  // Whether this session routes a plain drag to the browser. On, a drag
  // selects text instead of reaching an application that asked for mouse
  // reports; the pane says so while it lasts.
  selectText?: boolean;
  // Present only while the session has a login waiting to be finished.
  loginLabel?: string | null;
}

// The label says which of the two the row will take. Both are called "copy"
// and they differ by a whole screen, so a bare "Copy" would leave the
// user to infer the difference from a selection that may not exist.
export function terminalMenuHtml(state: TerminalMenuState): string {
  const copyLabel = state.hasSelection ? "Copy" : "Copy screen";
  const copyHint = state.hasSelection ? hints.COPY_TERMINAL_SELECTION : hints.COPY_TERMINAL_SCREEN;
  return `<div data-terminal-menu-popover role="menu" aria-label="Terminal actions" style="min-width:168px"
      class="menu absolute max-w-[calc(100vw-24px)]">
      <button type="button" role="menuitem" data-terminal-menu-copy data-hint="${escapeHtml(copyHint)}"
        class="menuitem"><span class="flex-1">${escapeHtml(copyLabel)}</span></button>
      <button type="button" role="menuitem" data-terminal-menu-paste data-hint="${escapeHtml(hints.PASTE_INTO_TERMINAL)}"
        class="menuitem"><span class="flex-1">Paste</span></button>
      <button type="button" role="menuitem" data-terminal-menu-select aria-pressed="${state.selectText === true ? "true" : "false"}" data-hint="${escapeHtml(hints.SELECT_TERMINAL_TEXT)}"
        class="menuitem"><span class="flex-1">Select text</span></button>
      ${state.loginLabel ? `<button type="button" role="menuitem" data-harness-login data-hint="${escapeHtml(hints.FINISH_AGENT_LOGIN)}"
        class="menuitem"><span class="flex-1">${escapeHtml(state.loginLabel)}</span></button>` : ""}
    </div>`;
}

export interface MenuPoint {
  x: number;
  y: number;
}

export interface MenuBox {
  width: number;
  height: number;
}

// The gap the menu keeps from the viewport edges, matching the row menus'
// left clamp.
const EDGE = 8;

// placeTerminalMenu puts the panel at the pointer, in the popover region's
// viewport-fixed coordinates. A context menu hangs down and to the right of
// the click, and flips across the axis that would take it off screen —
// off the bottom it opens upward, off the right it opens leftward — which is
// what a menu opened in the last row or the last column needs to stay
// clickable at all. Flipping only happens when the panel HAS a measured size:
// the DOM-free unit fakes report zero, and a zero-sized menu overflows
// nothing.
export function placeTerminalMenu(
  point: MenuPoint,
  menu: MenuBox,
  viewport: MenuBox,
): { top: number; left: number } {
  const overflowsRight = menu.width > 0 && point.x + menu.width > viewport.width - EDGE;
  const overflowsBottom = menu.height > 0 && point.y + menu.height > viewport.height - EDGE;
  const left = overflowsRight ? point.x - menu.width : point.x;
  const top = overflowsBottom ? point.y - menu.height : point.y;
  // A menu taller or wider than the space on either side of the pointer
  // still has to start on screen; the clamp is the last word.
  return { top: Math.max(EDGE, top), left: Math.max(EDGE, left) };
}
