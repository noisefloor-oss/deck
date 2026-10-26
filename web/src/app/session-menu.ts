import { previewConnectionLabel, type ProjectPreviewObservation } from "./project-preview-settings.ts";
import type { ProjectPreviewSettings } from "../api/project-preview.ts";
// The menu surfaces: this module owns the More menu
// (session header ⋯) and the shared surface beneath the
// sidebar's project and session row menus. Every popover is
// ONE source of markup, rendered by the live workspace (app/live.ts), the
// preview page (app/preview.ts) and the sidebar shell
// (panels/sidebar/sidebar-dom.ts), so screenshot comparisons against the
// reference exercise the real menus instead of stand-ins. The templates are
// pure — state in, HTML out; the callers own the event wiring and where
// the values persist.
//
// More menu contents, in order: terminal font stepper, terminal theme
// flyout, auto-color projects, then the toggle band (Show side panel,
// Show info bar, Enable yolo mode, Confirm before closing a session,
// Confirm before removing a project), then Git identity… and Sign out. Toggle rows are a
// button.menuitem whose label colour alone reports state; the whole row is
// the hit target and nothing in these menus is ever disabled, because
// tertiary text is reserved for the "off" state.
import {
  TERMINAL_THEMES,
  TERMINAL_THEME_LABELS,
  TERMINAL_THEME_NAMES,
  type CustomThemeSlots,
  type TerminalThemeName,
} from "../terminal/themes.ts";
import { isAppleClient } from "../lib/platform.ts";
import { SHORTCUTS, type ShortcutCommand } from "./shortcuts.ts";
import { SESSION_COLOR_TAGS, SESSION_COLOR_VARS } from "../lib/session-colors.ts";
import { operatorProjectColorKey, resolveTagKey } from "../lib/session-colors.ts";
import { escapeHtml } from "../lib/escape.ts";
import { lucideIcon } from "../ui/icons.ts";
import * as hints from "../lib/hints.ts";

// The shortcut glyph column, one span per glyph
// on a 0.34em flex gap (letter-spacing would also track the trailing
// edge and break the right alignment), modifiers at 0.98em nudged down
// and the key nudged up because ⌘ and a cap letter do not share an
// optical centre, and the row's own type at row weight — Euclid, never
// mono, which at menu size reads as terminal output that wandered into
// the chrome. A modifier that follows another modifier pulls in by
// 0.13em: two symbols read as one cluster and the full gap belongs
// before the key. Colour is always the tertiary text voice — the
// shortcut is reference, not content.
export function menuShortcutHtml(command: ShortcutCommand, apple = isAppleClient()): string {
  const { modifiers, key } = SHORTCUTS[command];
  if (!apple) {
    const labels = modifiers.map((glyph) => glyph === "⌘" ? "Ctrl" : "Alt").reverse();
    return `<span data-menu-shortcut class="menu-shortcut" style="flex:none;color:var(--color-text-tertiary);white-space:nowrap">${[...labels, escapeHtml(key)].join("+")}</span>`;
  }
  const mod = (glyph: string, first: boolean): string =>
    `<span style="font-size:0.98em;line-height:1;display:inline-flex;align-items:center;${first ? "" : "margin-left:-0.13em;"}transform:translateY(0.055em)">${glyph}</span>`;
  return `<span data-menu-shortcut class="menu-shortcut" style="flex:none;align-items:center;gap:0.34em;color:var(--color-text-tertiary);white-space:nowrap">${modifiers.map((glyph, i) => mod(glyph, i === 0)).join("")}<span style="line-height:1;display:inline-flex;align-items:center;transform:translateY(-0.055em)">${escapeHtml(key)}</span></span>`;
}

// The Auto-color projects row's strip: six families sampled from the mark
// palette the walk uses. Gold (the kilo chip) is the yellow; lime reads
// chartreuse beside it and stays out of the auto-color palette entirely.
const RAINBOW = [
  "var(--color-mark-crimson)", "var(--color-mark-gold)", "var(--color-mark-emerald)",
  "var(--color-mark-sapphire)", "var(--color-mark-indigo)", "var(--color-mark-orchid)",
];


export interface SessionMenuState {
  changePassword?: boolean;
  // Present only while the selected session has a login waiting to be finished.
  loginLabel?: string | null;
  workspaceSecurity?: boolean;
  workspaceMethods?: boolean;
  fontSize: number;
  themeName: TerminalThemeName;
  themeFlyoutOpen: boolean;
  customSlots: CustomThemeSlots;
  // showSidePanel mirrors the sidebar rail: on means the rail is showing.
  // The row toggles it through the sidebar shell, which owns the collapse;
  // ⌘B is the same gesture.
  showSidePanel: boolean;
  // showInfoBar drives the info-bar preference: on means the strip
  // shows. The row writes it through InfoBar.setShown.
  showInfoBar: boolean;
  // autoColorProjects is the rainbow toggle: on means the strip renders at
  // full opacity, off at 0.3 (the DC's resting state for an unset toggle),
  // and every project without a colour of its own takes one from the mark
  // palette. A local preference, with no backend setting behind it.
  autoColorProjects: boolean;
  // skipPermissions is the console setting (yolo mode). null while it is
  // being read (or after a failed read): the row renders as "off" rather
  // than guessing for a control that removes safety prompts.
  skipPermissions: boolean | null;
  // confirmingYolo expands the row into an inline confirmation. Turning
  // the mode ON asks first; turning it OFF never does — a control that
  // removes safety prompts should be deliberate to grant and trivial to
  // revoke.
  confirmingYolo: boolean;
  // skipCloseConfirm is the stored console setting. The "Confirm before
  // closing" row is ON when this is not true (see confirmsBeforeClose).
  skipCloseConfirm: boolean | null;
  // skipRemoveProjectConfirm is the stored console setting. The "Confirm
  // before removing a project" row is ON when this is not true.
  skipRemoveProjectConfirm: boolean | null;
}

// confirmsBeforeClose turns the stored setting into the behaviour, and is
// the ONLY place that mapping exists.
//
// The stored setting names what is skipped; the user's question is
// whether to be asked. Only an explicit true removes the confirmation —
// unread, unreadable and false all keep it — so the safe answer does not
// depend on remembering to write `!== true` rather than `=== false` at
// each of the places that need it.
export function confirmsBeforeClose(skipCloseConfirm: boolean | null): boolean {
  return skipCloseConfirm !== true;
}

// confirmsBeforeRemove is the same safe mapping for the project-removal
// confirmation: only an explicit true skips the prompt.
export function confirmsBeforeRemove(skipRemoveProjectConfirm: boolean | null): boolean {
  return skipRemoveProjectConfirm !== true;
}

// The sheet's theme row previews the current theme:
// three 11px swatches — background, foreground, accent — before the name.
// Phone-only chrome; the desktop popover's flyout rows carry their own
// strips, so the wide scope keeps this hidden.
function currentThemeSwatchesHtml(state: SessionMenuState): string {
  const t = state.themeName === "custom"
    ? { background: state.customSlots.background, foreground: state.customSlots.text, cursor: state.customSlots.accent }
    : TERMINAL_THEMES[state.themeName];
  const swatch = (c: string): string =>
    `<span class="h-[11px] w-[11px] rounded-[2px] border border-border" style="background:${c}"></span>`;
  return `<span data-theme-swatches aria-hidden="true" class="hidden flex-none items-center gap-[3px]">${
    swatch(t.background ?? "#000000")}${swatch(t.foreground ?? "#FFFFFF")}${swatch(t.cursor ?? t.foreground ?? "#FFFFFF")}</span>`;
}

// The switch is presentation only: the row button is the one hit target
// and carries the checked state.
function rowSwitchHtml(on: boolean): string {
  return `<span data-row-switch aria-hidden="true" class="inline-flex flex-none">
      <span class="row-switch-track${on ? " row-switch-on" : ""}"><span class="row-switch-thumb"></span></span>
    </span>`;
}

// sessionMenuHtml is the menu surface: the DC's popover recipe — a 252px
// overlay-surface menu with a card radius and an overlay ring, 1px row
// gaps, and .menuitem rows that wash on hover. The caller positions it
// (the live app and the preview both render it into the shared popover
// region, so it takes explicit coordinates computed from the ⋯ trigger's
// rect rather than anchoring against the trigger).
export function sessionMenuHtml(state: SessionMenuState): string {
  return `
      <div data-more-popover class="menu absolute max-w-[calc(100vw-24px)]" style="min-width:252px">
        <div data-settings-sheet-chrome class="hidden items-center gap-2 pb-1 pl-2.5 pr-1 pt-1">
          <span class="flex-1 text-title-3 font-medium text-text-primary">Session settings</span>
          <button type="button" data-settings-sheet-close aria-label="Close" data-hint="${hints.CLOSE_SESSION_SETTINGS}"
            class="grid h-11 w-11 flex-none place-items-center rounded-control text-text-secondary transition-colors hover:text-text-primary">${lucideIcon("x", 18)}</button>
        </div>
        <!-- Everything in this menu sizes from --type-body-sm-size, which steps
             13px -> 15px on a phone. The text-body-sm utility reads a different
             token that never steps, so using it here left these spans at 13px
             beside 15px rows. Rows that are .menuitem inherit the size; this
             row is not one, so it names the token. -->
        <div data-font-row class="flex items-center gap-1.5 px-(--row-px) py-(--row-py)">
          <span data-font-label class="text-[length:var(--type-body-sm-size)] font-medium text-text-secondary">Terminal font</span>
          <span class="ml-auto flex items-center gap-1.5">
            <button data-font-dec aria-label="Decrease terminal font size" data-hint="${hints.DECREASE_TERMINAL_FONT_SIZE}"
              class="grid h-[22px] w-[22px] place-items-center rounded-control-sm border border-field-border font-ui text-caption text-text-secondary transition-colors hover:bg-wash-hover hover:text-text-primary"><span data-copy-wide>−</span><span data-copy-phone class="hidden">${lucideIcon("chevron-down", 18)}</span></button>
            <span data-font-size class="min-w-[18px] text-center text-[length:var(--type-body-sm-size)] text-text-primary">${state.fontSize}</span>
            <button data-font-inc aria-label="Increase terminal font size" data-hint="${hints.INCREASE_TERMINAL_FONT_SIZE}"
              class="grid h-[22px] w-[22px] place-items-center rounded-control-sm border border-field-border font-ui text-caption text-text-secondary transition-colors hover:bg-wash-hover hover:text-text-primary"><span data-copy-wide>+</span><span data-copy-phone class="hidden">${lucideIcon("chevron-up", 18)}</span></button>
          </span>
        </div>
        <div class="relative">
          <button data-theme-flyout aria-expanded="${state.themeFlyoutOpen}" data-hint="${hints.CHANGE_TERMINAL_COLOR_SCHEME}"
            class="menuitem w-full justify-between">
            <span class="flex-1 text-left">Terminal theme</span>
            ${currentThemeSwatchesHtml(state)}
            <span data-theme-name class="leading-[normal] text-text-tertiary">${TERMINAL_THEME_LABELS[state.themeName]}</span>
            <span data-theme-disclosure aria-hidden="true" class="hidden text-text-tertiary">${lucideIcon("chevron-right", 16)}</span>
          </button>
          ${themeFlyoutHtml(state)}
        </div>
        <button type="button" data-auto-color role="switch" aria-checked="${state.autoColorProjects}" data-hint="${hints.GIVE_PROJECT_DISTINCT_COLOR}"
          class="menuitem justify-between">
          <span class="flex-1">Auto-color projects</span>
          ${rainbowStripHtml(state.autoColorProjects)}
        </button>
        <hr class="menu-sep">
        ${toggleRowHtml("show-side-panel", "Show side panel", hints.SHOW_PROJECT_SIDEBAR, state.showSidePanel, "toggleRail")}
        ${toggleRowHtml("show-info-bar", "Show info bar", hints.SHOW_HOVER_FOCUS_HINTS, state.showInfoBar, "toggleInfoBar")}
        ${yoloRowHtml(state.skipPermissions, state.confirmingYolo)}
        ${toggleRowHtml("confirm-before-closing", phoneCopy("Confirm before closing a session", "Confirm before closing"), hints.ASK_BEFORE_CLOSING_SESSION, confirmsBeforeClose(state.skipCloseConfirm))}
        ${toggleRowHtml("confirm-before-removing", "Confirm before removing a project", hints.ASK_BEFORE_REMOVING_PROJECT, confirmsBeforeRemove(state.skipRemoveProjectConfirm))}
        <hr class="menu-sep">
        <button type="button" data-show-tutorial data-hint="${hints.SHOW_TUTORIAL_HINT}" class="menuitem">Show tutorial</button>
        ${state.loginLabel ? `<button type="button" data-harness-login data-hint="${hints.FINISH_AGENT_LOGIN}" class="menuitem">${escapeHtml(state.loginLabel)}</button>` : ""}
        <button type="button" data-github-connect class="menuitem">GitHub connection…</button>
        <button data-git-identities data-hint="${hints.SET_NAME_EMAILS_COMMITS_SIGNED}"
          class="menuitem">Git identity…</button>
        ${state.workspaceSecurity ? `<hr class="menu-sep"><button type="button" data-workspace-lock class="menuitem security-menu-action"><span>Lock workspace</span><span>Stops your sessions and locks storage</span></button>${state.workspaceMethods ? '<button type="button" data-workspace-methods class="menuitem">Unlock methods…</button>' : ""}` : ""}
        ${state.changePassword ? `<button type="button" data-change-password data-hint="${hints.CHANGE_PASSWORD}" class="menuitem">Change password</button>` : ""}
        <button data-live-logout data-hint="${hints.SIGN_OUT_BROWSER_SESSION}"
          class="menuitem${state.workspaceSecurity ? " security-menu-action" : ""}">${state.workspaceSecurity ? "<span>Sign out</span><span>Leaves your sessions running</span>" : "Sign out"}</button>
      </div>`;
}

// The phone sheet is narrower than the desktop popover and trims
// two labels to fit it; the pair renders both and the narrow scope picks.
function phoneCopy(wide: string, phone: string): string {
  return `<span data-copy-wide>${wide}</span><span data-copy-phone class="hidden">${phone}</span>`;
}

function toggleRowHtml(
  key: string,
  label: string,
  hint: string,
  on: boolean,
  shortcut?: ShortcutCommand,
): string {
  return `
        <button type="button" data-${key} role="switch" aria-checked="${on}" data-hint="${hint}"
          class="menuitem menuitem-toggle ${shortcut === undefined ? "" : "gap-3"}">
          <span class="flex-1">${label}</span>
          ${shortcut === undefined ? "" : menuShortcutHtml(shortcut)}
          ${rowSwitchHtml(on)}
        </button>`;
}

// The yolo toggle, inline: this menu IS the settings surface, so the
// setting does not hide behind a dialog. Turning it ON expands the row
// into an inline confirmation first; turning it OFF is immediate.
// The confirmation is contained inline so its sentence wraps to the
// menu's width rather than widening the menu.
function yoloRowHtml(value: boolean | null, confirming: boolean): string {
  const on = value === true;
  return `
        <button type="button" data-skip-permissions role="switch" aria-checked="${on}" aria-expanded="${confirming}"
          ${value === null ? 'aria-disabled="true"' : ""} ${confirming ? 'aria-controls="yolo-confirmation"' : ""}
          data-hint="${hints.LET_NEW_SESSIONS_SKIP_PERMISSIONS}" class="menuitem menuitem-toggle flex-col items-stretch gap-0.5">
          <span class="flex w-full items-center gap-3">
            <span class="flex-1">${phoneCopy("Enable yolo mode", "Yolo mode")}</span>
            ${menuShortcutHtml("toggleYolo")}
            ${rowSwitchHtml(on)}
          </span>
        </button>${confirming
          ? `<div id="yolo-confirmation" class="mt-1 flex flex-col gap-1.5 rounded-control bg-warning-tint px-2 py-1.5 [contain:inline-size]">
              <span class="text-caption leading-[1.35] text-warning-on-tint">Agents will edit files and run commands without asking, in every new session until you turn this off.</span>
              <span class="flex justify-end gap-1.5">
                <button type="button" data-yolo-cancel data-hint="${hints.CANCEL_ENABLING_YOLO}" class="h-6 rounded-control-sm px-2 text-caption text-text-secondary transition-colors hover:bg-wash-hover">Cancel</button>
                <button type="button" data-yolo-confirm data-hint="${hints.ENABLE_YOLO_MODE}" class="h-6 rounded-control-sm bg-error px-2 text-caption font-medium text-text-on-error transition-colors hover:opacity-90">Turn it on</button>
              </span>
            </div>`
          : ""}`;
}

// rainbowStripHtml renders the six-bar rainbow strip on the Auto-color
// projects row: full opacity when the preference is on, 0.3 when off (the
// DC's resting state for an unset toggle).
function rainbowStripHtml(on: boolean): string {
  return `<span aria-hidden="true" class="flex flex-none gap-[2px]">${RAINBOW
    .map((hex) => `<span class="h-[12px] w-[6px] rounded-[1.5px]" style="background:${hex};opacity:${on ? "1" : "0.3"}"></span>`)
    .join("")}</span>`;
}

// themeSwatches renders the DC's five identity chips for a theme row:
// ground, text, accent, warning, cyan.
function themeSwatches(colors: readonly string[]): string {
  return `<span class="flex flex-none gap-[3px]">${colors
    .map((c) => `<span class="h-[9px] w-[9px] rounded-[2px] border border-field-border" style="background:${c}"></span>`)
    .join("")}</span>`;
}

// The theme flyout: a side popover to the menu's left (DC), one row per
// theme with its swatch strip, then Custom… behind a divider. Custom…
// opens the six-slot editor dialog rather than picking a
// theme; its check mark still shows when the custom theme is current.
//
// At the narrow scope the same surface becomes the pushed Terminal theme
// screen: a chrome with back/close, a 16px leading
// check column, and a chevron on Custom… that pushes to the custom theme
// editor. The desktop popover hides the chrome and chevron via CSS.
function themeFlyoutHtml(state: SessionMenuState): string {
  if (!state.themeFlyoutOpen) return "";
  const active = state.themeName;
  const row = (name: TerminalThemeName, swatches: string): string => `
      <button data-theme-pick="${name}" data-hint="${hints.themeApply(TERMINAL_THEME_LABELS[name])}"
        class="menuitem theme-row ${name === active ? "theme-row-active" : ""}">
        <span aria-hidden="true" class="theme-check flex w-3 flex-none justify-center text-caption text-accent leading-none">${name === active ? "✓" : ""}</span>
        <span class="flex-1 leading-[normal] ${name === active ? "text-text-primary" : "text-text-secondary"}">${TERMINAL_THEME_LABELS[name]}</span>
        <span class="theme-swatch-strip">${swatches}</span>
      </button>`;
  const rows = TERMINAL_THEME_NAMES.filter((n) => n !== "custom").map((name) => {
    const t = TERMINAL_THEMES[name];
    return row(name, themeSwatches([
      t.background ?? "#000000",
      t.foreground ?? "#FFFFFF",
      t.cursor ?? t.foreground ?? "#FFFFFF",
      t.yellow ?? t.foreground ?? "#FFFFFF",
      t.cyan ?? t.foreground ?? "#FFFFFF",
    ]));
  }).join("");
  const s = state.customSlots;
  const customRow = `
      <button data-theme-custom-open data-hint="${hints.OPEN_CUSTOM_COLOR_EDITOR}"
        class="menuitem theme-row ${active === "custom" ? "theme-row-active" : ""}">
        <span aria-hidden="true" class="theme-check flex w-3 flex-none justify-center text-caption text-accent leading-none">${active === "custom" ? "✓" : ""}</span>
        <span class="flex-1 leading-[normal] ${active === "custom" ? "text-text-primary" : "text-text-secondary"}">Custom…</span>
        <span class="theme-swatch-strip">${themeSwatches([s.background, s.text, s.accent, s.warning, s.cyan])}</span>
        <span class="theme-chevron" aria-hidden="true">›</span>
      </button>`;
  return `<div data-theme-list class="menu absolute">
      <div data-theme-chrome class="theme-chrome">
        <button type="button" data-theme-back aria-label="Back" class="theme-back">${lucideIcon("chevron-left", 20)}</button>
        <span class="theme-title">Terminal theme</span>
        <button type="button" data-theme-close aria-label="Close" class="theme-close">${lucideIcon("x", 16)}</button>
      </div>
      <div data-theme-rows class="theme-rows">
        ${rows}
        <hr class="menu-sep">
        ${customRow}
      </div>
    </div>`;
}

// The sidebar row menus: the project and session ⋯ popovers.
// They sit on the same anatomy as the More menu — one .menu surface
// (overlay surface, card radius, overlay ring), .menuitem rows that wash
// on hover, .menu-sep hairlines between groups (styles/app.css restates
// the design system's bp-menu family). The content lives here — labels,
// verb-first hints, order, swatch strip, and the per-menu min-width the
// design pins (196px project, 190px session) — so the two menus cannot
// drift apart; sidebar-dom.ts mounts the surface and wires each action key
// to the flow it already owns (inline rename, the color picker, the dialogs).
interface RowMenuAction {
  // action names the row's data attribute (data-<action>); it is the
  // stable contract the sidebar's click wiring binds to.
  action: string;
  label: string;
  // hint is the verb-first info-bar explanation: no subject, no period.
  hint: string;
  // shortcut names the binding the row prints, sourced from the
  // same table the key listener claims — undefined prints nothing, which
  // is the honest state for a row with no binding.
  shortcut?: ShortcutCommand;
}

interface RowMenu {
  label: string;
  minWidth: number;
  actions: readonly RowMenuAction[];
}


const ROW_MENUS: { project: RowMenu; session: RowMenu } = {
  project: {
    label: "Project actions",
    minWidth: 196,
    actions: [
      { action: "project-menu-rename", label: "Rename", hint: hints.RENAME_THIS_PROJECT, shortcut: "renameSelection" },
      { action: "project-menu-identity", label: "Project git identity…", hint: hints.CHANGE_PROJECT_GIT_IDENTITY },
      { action: "project-menu-remove", label: "Remove from sidebar", hint: hints.REMOVE_PROJECT_FROM_SIDEBAR },
    ],
  },
  session: {
    label: "Session actions",
    minWidth: 190,
    actions: [
      // The swatch strip above the action rows is the color control; there
      // is no color row among the actions. Labels are short
      // forms: "Rename" edits inline so it takes no ellipsis, and "Close"
      // keeps its plain verb even when a confirm dialog follows.
      { action: "session-menu-rename", label: "Rename", hint: hints.RENAME_THIS_SESSION, shortcut: "renameSelection" },
      { action: "session-menu-close", label: "Close", hint: hints.END_SESSION_PROCESS_KEEP_RECORDING },
    ],
  },
};

export function projectPreviewMenuHtml(settings?: ProjectPreviewSettings, observation: ProjectPreviewObservation | null = null): string {
  if (settings?.enabled) {
    const status = previewConnectionLabel({ busy: null, check: observation?.check ?? null, checkFailed: observation?.checkFailed ?? false });
    return `<button type="button" role="menuitem" aria-label="Open preview" aria-describedby="project-preview-menu-status" data-project-menu-preview data-hint="${escapeHtml(hints.OPEN_PROJECT_PREVIEW)}" class="menuitem"><span class="flex-1">Open preview</span><span id="project-preview-menu-status" data-project-preview-menu-status class="text-caption text-text-tertiary">${escapeHtml(status)}</span><span aria-hidden="true">↗</span></button><button type="button" role="menuitem" data-project-menu-preview-settings data-hint="${escapeHtml(hints.VIEW_PROJECT_PREVIEW_SETTINGS)}" class="menuitem">Preview settings…</button>`;
  }
  return `<button type="button" role="menuitem" data-project-menu-preview-settings data-hint="${escapeHtml(hints.SET_UP_PROJECT_PREVIEW)}" class="menuitem"><span class="flex-1">Preview…</span>${settings?.configured === false ? '<span class="text-caption text-text-tertiary">Not set up</span>' : ""}</button>`;
}

export type RowMenuKind = keyof typeof ROW_MENUS;
export interface ProjectRowMenuState {
  name?: string;
  // The user's own colour, never the auto-color tint the row happens
  // to paint: this row writes durable session tags.
  operatorColor?: string | null;
  canMoveUp?: boolean;
  canMoveDown?: boolean;
  preview?: ProjectPreviewSettings;
}
// swatchStripHtml renders the row menu's colour strip: sixteen 15px
// circles in two rows of eight, the first being "no color". The grid
// spreads its columns space-between — a start-packed row leaves the last
// chip short of the menu's right edge, which reads as a broken strip
// rather than a full one. The current tag reads as a ring — a
// text-primary border holding the surface card off the fill. An
// unselected "none" swatch keeps a border-strong outline so the control
// is still visible against the menu surface. Sessions use the mark
// radius; projects use a full circle, each with its own swatch shape.
function swatchStripHtml(kind: RowMenuKind, current: string | null | undefined): string {
  const attr = kind === "session" ? "data-sess-swatch" : "data-proj-swatch";
  const radius = kind === "session" ? "rounded-mark" : "rounded-circle";
  // A stored legacy key (red, amber, ...) marks its canonical swatch
  // current, so an old tag reads as the hue it always was.
  const currentKey = resolveTagKey(current);
  const swatches = SESSION_COLOR_TAGS.map((tag) => {
    const isCurrent = tag.value === null
      ? currentKey === null
      : currentKey === tag.key;
    const ring = isCurrent
      ? "var(--color-text-primary)"
      : tag.value === null ? "var(--color-border-strong)" : "transparent";
    const mark = isCurrent ? `inset 0 0 0 ${kind === "project" ? 1 : 1.5}px var(--color-surface-card)` : "none";
    const hint = kind === "session" ? hints.sessionColor(escapeHtml(tag.name)) : hints.projectColor(escapeHtml(tag.name));
    return `<button type="button" ${attr}="${escapeHtml(tag.key)}" aria-label="${escapeHtml(tag.name)}" data-hint="${hint}"
      class="h-[15px] w-[15px] flex-none cursor-pointer ${radius} border p-0"
      style="background:${tag.value ?? "transparent"};border-color:${ring};box-shadow:${mark}"></button>`;
  }).join("");
  return `<div class="grid grid-cols-[repeat(8,15px)] justify-between gap-y-1.5 px-2 pt-1 pb-1.5">${swatches}</div>`;
}

// applyColorRowHtml renders the project-only row that propagates the
// project's colour to its sessions. The row always offers the project's
// OWN colour, and "none" is a colour: with no user colour the action
// is the clearing one, and its dot is the strip's empty swatch — an
// outline, not a hue — so the user sees what will reach the sessions.
function applyColorRowHtml(color: string | null): string {
  const hex = color === null ? null : SESSION_COLOR_VARS[color] ?? color;
  const dot = hex === null
    ? "background:transparent;border:1px solid var(--color-border-strong)"
    : `background:${escapeHtml(hex)}`;
  return `
      <button type="button" data-project-menu-apply-color role="menuitem" data-hint="${
        color === null ? hints.CLEAR_PROJECT_COLOR_FROM_SESSIONS : hints.APPLY_PROJECT_COLOR_TO_SESSIONS}"
        class="menuitem justify-between">
        <span class="flex-1">${color === null ? "Clear color from sessions" : "Apply color to sessions"}</span>
        <span aria-hidden="true" class="h-[7px] w-[7px] flex-none rounded-circle" style="${dot}"></span>
      </button>`;
}

// The three position rows. Up and down swap with a neighbour; top is the
// long move, there because walking a menu one row at a time is O(n) and
// the long move is the one people want. Top shares Move up's boundary: a
// project already first has nowhere to go either way.
export type ProjectMove = "up" | "down" | "top";

const PROJECT_MOVE_ROWS: Record<ProjectMove, { label: string; hint: string; blocked: string }> = {
  up: { label: "Move up", hint: hints.MOVE_PROJECT_UP, blocked: hints.PROJECT_ALREADY_FIRST },
  down: { label: "Move down", hint: hints.MOVE_PROJECT_DOWN, blocked: hints.PROJECT_ALREADY_LAST },
  top: { label: "Move to top", hint: hints.MOVE_PROJECT_TO_TOP, blocked: hints.PROJECT_ALREADY_FIRST },
};

export function projectMoveHints(move: ProjectMove): { hint: string; blocked: string } {
  return PROJECT_MOVE_ROWS[move];
}

function projectMoveRowHtml(move: ProjectMove, disabled: boolean): string {
  const row = PROJECT_MOVE_ROWS[move];
  return `
      <button type="button" data-project-menu-move-${move} role="menuitem"${disabled ? ' aria-disabled="true"' : ""}
        data-hint="${disabled ? row.blocked : row.hint}" class="menuitem"><span class="flex-1">${row.label}</span></button>`;
}

// rowMenuHtml renders one row menu's surface. Like sessionMenuHtml it is
// positioned by the caller: the shared popover region is viewport-fixed,
// so the surface takes explicit coordinates computed from the ⋯ trigger's
// colour strip; the project menu keeps its colour, position, and action
// groups separate — Open preview leads the actions — and adds the
// apply-color row only for a resolvable colour.
export function rowMenuHtml(
  kind: RowMenuKind,
  currentColor?: string | null,
  projectState?: ProjectRowMenuState,
): string {
  const menu = ROW_MENUS[kind];
  const strip = swatchStripHtml(kind, currentColor);
  // One resolver on both paths: a bare resolveTagKey here would let the
  // stored "none" key through as a colour and paint the dot with it.
  const operatorColor = projectState?.operatorColor === undefined
    ? operatorProjectColorKey(currentColor)
    : projectState.operatorColor;
  const applyColor = kind === "project" ? applyColorRowHtml(operatorColor) : "";
  const positionRows = kind === "project"
    ? `${projectMoveRowHtml("up", projectState?.canMoveUp === false)}${projectMoveRowHtml("down", projectState?.canMoveDown === false)}${projectMoveRowHtml("top", projectState?.canMoveUp === false)}`
    : "";
  const sheetHead = kind === "project" ? `<div data-project-menu-sheet-head class="project-menu-sheet-head"><span aria-hidden="true" class="project-menu-sheet-grabber"></span><span data-project-menu-sheet-title class="project-menu-sheet-title">${escapeHtml(projectState?.name ?? "Project")}</span></div>` : "";
  const rows = menu.actions.map((action) => `${kind === "project" && action.action === "project-menu-remove" ? `<hr class="menu-sep"><div data-project-preview-actions>${projectPreviewMenuHtml(projectState?.preview)}</div><hr class="menu-sep">` : ""}` + `
      <button type="button" role="menuitem" data-${action.action} data-hint="${escapeHtml(action.hint)}"
        class="menuitem"><span class="flex-1">${escapeHtml(action.label)}</span>${action.shortcut === undefined ? "" : menuShortcutHtml(action.shortcut)}</button>`).join("");
  return `<div data-${kind}-menu-popover role="menu" aria-label="${escapeHtml(menu.label)}" style="min-width:${menu.minWidth}px"
      class="menu absolute max-w-[calc(100vw-24px)]">${sheetHead}${strip}${applyColor}<hr class="menu-sep">${positionRows}${kind === "project" ? '<hr class="menu-sep">' : ""}${rows}
    </div>`;
}

// The PROJECTS header menu: the sidebar's eyebrow is a
// menubar-style menu button and this is its panel. Rows group by kind —
// commands, order, disclosure — with a hairline between groups.
//
// The New session row carries no trailing caption naming its target. The
// target is disclosed in the dialog the row opens, where it sits beside the
// harness being chosen; a caption here would be a preview of a label one
// click away, and it was the only row in this menu with a trailing column.
//
// The panel takes the overlay's density like every other menu, rather than
// shrinking its own inset to line row text up with the trigger's label. That
// alignment was worth one axis at 4px against the other at 10px, which is the
// split spacing.css names as a bug: the row family is density-scoped and both
// axes move together.
export function projectsMenuHtml(newSessionTarget: string | null): string {
  const targetName = newSessionTarget === null ? "" : escapeHtml(newSessionTarget);
  const newSessionHint = newSessionTarget === null
    ? hints.START_NEW_SESSION
    : hints.projectNewSession(targetName);
  return `<div data-projects-menu-popover role="menu" aria-label="Projects menu"
      style="min-width:196px"
      class="menu absolute max-w-[calc(100vw-24px)]">
      <button type="button" role="menuitem" data-projects-menu-new-project data-hint="${hints.OPEN_DIRECTORY_AS_PROJECT}"
        class="menuitem"><span class="flex-1">Add project…</span></button>
      <button type="button" role="menuitem" data-projects-menu-new-session data-hint="${newSessionHint}"
        class="menuitem"><span class="flex-1">New session</span></button>
      <hr class="menu-sep">
      <button type="button" role="menuitem" data-projects-menu-sort data-hint="${hints.SORT_PROJECTS_BY_NAME}"
        class="menuitem"><span class="flex-1">Sort by name</span></button>
      <hr class="menu-sep">
      <button type="button" role="menuitem" data-projects-menu-expand-all data-hint="${hints.EXPAND_ALL_PROJECTS}"
        class="menuitem"><span class="flex-1">Expand all</span></button>
      <button type="button" role="menuitem" data-projects-menu-collapse-all data-hint="${hints.COLLAPSE_ALL_PROJECTS}"
        class="menuitem"><span class="flex-1">Collapse all</span></button>
    </div>`;
}
