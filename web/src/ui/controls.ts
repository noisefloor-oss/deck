// Control recipes the docked panels share, so their headers read as one
// system: the same 28px icon footprint keeps the two panels' right edges
// on a single column, and the same focus ring appears wherever a control
// is reached by keyboard.
import { lucideIcon } from "./icons.ts";
import * as hints from "../lib/hints.ts";

export const ICON_BUTTON =
  "grid h-7 w-7 flex-none place-items-center rounded-control-sm text-text-tertiary transition-colors hover:bg-wash-hover hover:text-text-primary focus-visible:bg-wash-hover";

// The dialog head's icon button: the 28px footprint, 40px under a coarse pointer.
export const DIALOG_ICON_BUTTON = `${ICON_BUTTON} pointer-coarse:h-10 pointer-coarse:w-10`;

// The secondary sm label: 13px on a 13px line box with 0.004em tracking at
// weight 500. Split out of the button recipe so a control that paints its
// own pill (the docked-panel triggers) can carry the same type without
// duplicating the border, padding and fill that live on its inner span.
export const SECONDARY_SM_TYPE =
  "text-(length:--type-control-sm-size) font-medium leading-[13px] tracking-[0.004em]";

// The secondary sm button: a 28px hairlined pill on the card surface.
// It carries no text colour — a caller that tints the label (an active
// filter) would otherwise be competing with a second colour utility, and
// which one wins is stylesheet order, not class order.
export const SECONDARY_SM_BUTTON =
  `flex h-7 flex-none items-center gap-1 rounded-control-sm border border-border-strong bg-surface-card px-2.5 ${SECONDARY_SM_TYPE} transition-colors hover:bg-wash-hover focus-visible:bg-wash-hover `;

// The shared dialog anatomy: the modal host's one .scrim wraps each
// .dialog, which wraps the head / body / foot parts. The close X is the
// head's trailing control; on a pushed screen it closes the whole modal.
// A screen that steps back within its own dialog leads with a Back chevron.
export function dialogHead(
  title: string,
  close: { label: string; attr: string; disabled?: boolean } | null,
  desc?: string,
  back?: { attr: string },
): string {
  const backButton = back === undefined
    ? ""
    : `<button type="button" ${back.attr} aria-label="Back" data-hint="${hints.GO_BACK}"
         class="${DIALOG_ICON_BUTTON}">${lucideIcon("chevron-left", 16)}</button>`;
  const closeButton = close === null
    ? ""
    : `<button type="button" ${close.attr} data-dialog-close aria-label="${close.label}" data-hint="${hints.CLOSE_THIS_DIALOG}" ${close.disabled === true ? "disabled" : ""}
         class="ml-auto ${DIALOG_ICON_BUTTON}">${lucideIcon("x", 16)}</button>`;
  // Title and description stack in one column so the close control stays on
  // the head's trailing edge rather than being pushed by the longer of the two.
  const heading = desc === undefined
    ? `<h2 class="dialog-title dialog-title-prompt">${title}</h2>`
    : `<span class="flex min-w-0 flex-col">
         <h2 class="dialog-title dialog-title-prompt">${title}</h2>
         <span class="dialog-desc">${desc}</span>
       </span>`;
  return `
  <header class="dialog-head">
    ${backButton}
    ${heading}
    ${closeButton}
  </header>`;
}
// The dialog footer's md buttons (bp-btn/--md): a 36px control at the
// control inset, speaking the 14px button voice on a single line.
export const MD_BUTTON_TYPE =
  "text-(length:--type-button-size) font-medium leading-none tracking-(--type-button-ls)";
export const PRIMARY_MD_BUTTON =
  `h-9 rounded-control border border-transparent bg-accent px-(--control-px) ${MD_BUTTON_TYPE} text-text-on-accent transition-colors hover:bg-accent-hover disabled:opacity-45 pointer-coarse:min-h-11`;
export const SECONDARY_MD_BUTTON =
  `h-9 rounded-control border border-border-strong bg-surface-card px-(--control-px) ${MD_BUTTON_TYPE} text-text-primary transition-colors hover:bg-wash-hover pointer-coarse:min-h-11`;
export const GHOST_MD_BUTTON =
  `flex h-9 items-center gap-2 rounded-control border border-transparent px-(--control-px) ${MD_BUTTON_TYPE} text-text-secondary transition-colors hover:bg-wash-hover hover:text-text-primary disabled:opacity-45 pointer-coarse:min-h-11`;

// The ghost sm button: the secondary sm footprint without border or fill,
// for header actions that must fit a narrow dock.
export const GHOST_SM_BUTTON =
  `flex h-7 flex-none items-center gap-1 rounded-control-sm border border-transparent px-2.5 ${SECONDARY_SM_TYPE} text-text-secondary transition-colors hover:bg-wash-hover hover:text-text-primary focus-visible:bg-wash-hover disabled:opacity-45`;

// An action rendered as running text: a button that reads like a link.
export const INLINE_LINK_BUTTON =
  "text-accent underline underline-offset-2 hover:text-accent-hover";

// The primary sm button: the same 28px footprint as its secondary sibling
// with the accent fill, for one action beside a secondary one.
export const PRIMARY_SM_BUTTON =
  `flex h-7 flex-none items-center gap-1 rounded-control-sm border border-transparent bg-accent px-2.5 ${SECONDARY_SM_TYPE} text-text-on-accent transition-colors hover:opacity-90 focus-visible:bg-wash-hover`;

// A destructive/warning prompt head: the triangle-alert icon leads a title
// and description, so a confirm dialog reads as a warning before its body.
export function dialogPromptHead(title: string, desc: string, tint: string): string {
  return `
  <header class="dialog-head">
    <span aria-hidden="true" class="mt-1 flex-none ${tint}">${lucideIcon("triangle-alert", 17)}</span>
    <div class="min-w-0 flex-1">
      <h2 class="dialog-title dialog-title-prompt">${title}</h2>
      <p class="dialog-desc">${desc}</p>
    </div>
  </header>`;
}

export function DIALOG_BODY(body: string): string {
  return `<div class="dialog-body">${body}</div>`;
}

// The DS select chrome (bp-selectwrap): the native picker is suppressed
// and the wrap paints its own 15px chevron-down at the field's 14px
// trailing inset in text-tertiary, with the select's own padding
// reserving field-px + 23px so the value never runs under the glyph.
// The chevron span is pointer-inert; the select underneath owns the hit.
export const SELECT_FIELD = "appearance-none cursor-pointer pr-[37px]";

export function selectShellHtml(select: string, marginClass = ""): string {
  return `<span class="relative ${marginClass} flex w-full items-center">${select}
    <span aria-hidden="true" class="pointer-events-none absolute right-[14px] flex text-text-tertiary">${lucideIcon("chevron-down", 15)}</span></span>`;
}

export function DIALOG_FOOT(foot: string): string {
  return `<footer class="dialog-foot">${foot}</footer>`;
}
