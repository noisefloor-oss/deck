const FOCUSABLE = [
  "button:not([disabled])",
  "input:not([disabled])",
  "select:not([disabled])",
  "textarea:not([disabled])",
  "a[href]",
  '[tabindex]:not([tabindex="-1"])',
].join(",");

export function activateDialog(
  dialog: HTMLElement,
  dismiss: () => void,
  initialFocusSelector?: string | null,
): () => void {
  const focusable = (): HTMLElement[] =>
    Array.from(dialog.querySelectorAll<HTMLElement>(FOCUSABLE))
      .filter((el) => !el.hidden && el.getClientRects().length > 0);
  const onKeyDown = (event: KeyboardEvent): void => {
    if (event.key === "Escape") {
      event.preventDefault();
      dismiss();
      return;
    }
    if (event.key !== "Tab") return;
    const items = focusable();
    if (items.length === 0) {
      event.preventDefault();
      dialog.focus();
      return;
    }
    const first = items[0]!;
    const last = items.at(-1)!;
    // With the container itself focused (the :dialog initial state), a
    // native Tab in either direction leaves the dialog for the page behind
    // the scrim; both directions enter the ring explicitly instead.
    if (document.activeElement === dialog) {
      event.preventDefault();
      (event.shiftKey ? last : first).focus();
    } else if (event.shiftKey && document.activeElement === first) {
      event.preventDefault();
      last.focus();
    } else if (!event.shiftKey && document.activeElement === last) {
      event.preventDefault();
      first.focus();
    }
  };
  dialog.addEventListener("keydown", onKeyDown);
  // null leaves focus where it is: a re-render that already put it back.
  if (initialFocusSelector === null) return () => dialog.removeEventListener("keydown", onKeyDown);
  const items = focusable();
  // ":dialog" puts the initial focus on the container itself: the
  // design draws no ring at open, and only an auto-focused control paints one —
  // a mouse-opened dialog never does.
  const preferred = initialFocusSelector === undefined || initialFocusSelector === ":dialog"
    ? null
    : dialog.querySelector<HTMLElement>(initialFocusSelector);
  const target = initialFocusSelector === ":dialog"
    ? dialog
    : preferred !== null && items.includes(preferred)
      ? preferred
      : items[0] ?? dialog;
  if (target === dialog && dialog.tabIndex < 0) dialog.tabIndex = -1;
  target.focus({ preventScroll: true });
  return () => dialog.removeEventListener("keydown", onKeyDown);
}

// activateMenu owns an open menu widget's keyboard contract. A menu here
// rides a full-viewport backdrop, so nothing behind it is reachable by
// pointer — the keyboard must not reach it either. Tab cycles the rows,
// ArrowUp/ArrowDown/Home/End move between them, and Escape dismisses from
// wherever focus sits: the keydown listener lives on the document in
// capture phase so a stray focus on <body> cannot strand an open menu.
// Focus nets put focus back on the row that held it if it escapes the
// menu by any other route. The caller runs the returned teardown on every
// close path, before moving focus to whatever comes next.
export function menuRows(menu: HTMLElement): HTMLElement[] {
  const rows = [
    ...menu.querySelectorAll<HTMLElement>(".menuitem"),
    ...menu.querySelectorAll<HTMLElement>('[role="menuitem"]'),
    ...menu.querySelectorAll<HTMLElement>('[role="switch"]'),
    ...menu.querySelectorAll<HTMLElement>("[data-theme-back]"),
    ...menu.querySelectorAll<HTMLElement>("[data-theme-close]"),
  ];
  return [...new Set(rows)]
    .sort((a, b) => typeof a.compareDocumentPosition !== "function" ? 0 : (a.compareDocumentPosition(b) & 4) === 4 ? -1 : 1)
    .filter((el) => !el.hidden && (typeof el.getClientRects !== "function" || el.getClientRects().length > 0));
}

export function activateMenu(
  menu: HTMLElement,
  dismiss: (restoreFocus: boolean) => void,
): () => void {
  const rows = (): HTMLElement[] => menuRows(menu);
  let index = 0;
  const focusRow = (next: number): void => {
    const list = rows();
    if (list.length === 0) return;
    index = ((next % list.length) + list.length) % list.length;
    list[index]!.focus();
  };
  const onKeyDown = (event: KeyboardEvent): void => {
    if (event.defaultPrevented) return;
    switch (event.key) {
      case "Escape":
        event.preventDefault();
        dismiss(true);
        return;
      case "Tab":
        event.preventDefault();
        focusRow(index + (event.shiftKey ? -1 : 1));
        return;
      case "ArrowDown":
        event.preventDefault();
        focusRow(index + 1);
        return;
      case "ArrowUp":
        event.preventDefault();
        focusRow(index - 1);
        return;
      case "Home":
        event.preventDefault();
        focusRow(0);
        return;
      case "End":
        event.preventDefault();
        focusRow(rows().length - 1);
        return;
    }
  };
  const onFocusIn = (event: FocusEvent): void => {
    const target = event.target;
    if (target instanceof Node && menu.contains(target)) {
      const at = rows().indexOf(target as HTMLElement);
      if (at !== -1) index = at;
      return;
    }
    // Focus landed outside the open menu; the page behind the backdrop is
    // unreachable, so bring it back to the row that held it.
    if (menu.isConnected) focusRow(index);
  };
  const onFocusOut = (event: FocusEvent): void => {
    const related = event.relatedTarget;
    if (related instanceof Node && menu.contains(related)) return;
    // Focus left the menu — often for <body>, which fires no focusin —
    // so bring it back to the row that held it.
    if (menu.isConnected) focusRow(index);
  };
  document.addEventListener("keydown", onKeyDown, true);
  document.addEventListener("focusin", onFocusIn);
  menu.addEventListener("focusout", onFocusOut);
  focusRow(0);
  return () => {
    document.removeEventListener("keydown", onKeyDown, true);
    document.removeEventListener("focusin", onFocusIn);
    menu.removeEventListener("focusout", onFocusOut);
  };
}
