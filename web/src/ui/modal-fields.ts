// A modal screen re-renders from markup that does not carry what you typed,
// so an owner snapshots its edited fields and focus before the rewrite and
// puts them back on the fields the new markup draws again.

export type FormField = HTMLInputElement | HTMLTextAreaElement | HTMLSelectElement;

export interface ModalFocus {
  selector: string;
  selection: [number | null, number | null] | null;
  fallbackToDialog?: boolean;
}

// Field-only snapshots are also used by sidebar screens during navigation.
export function focusedModalField(host: HTMLElement | null): ModalFocus | null {
  const focused = document.activeElement;
  if (focused == null || host?.contains(focused) !== true) return null;
  if (!(focused instanceof HTMLInputElement || focused instanceof HTMLTextAreaElement || focused instanceof HTMLSelectElement)) return null;
  const selector = fieldSelector(focused);
  if (selector === null) return null;
  return { selector, selection: focused instanceof HTMLSelectElement ? null : [focused.selectionStart, focused.selectionEnd] };
}

// A refresh within the same dialog must also preserve buttons and links.
export function focusedModalControl(host: HTMLElement | null): ModalFocus | null {
  const focused = document.activeElement;
  if (focused == null || host?.contains(focused) !== true) return null;
  const field = focusedModalField(host);
  if (field !== null) return { ...field, fallbackToDialog: true };
  if (!(focused instanceof HTMLElement)) return null;
  return { selector: controlSelector(host, focused), selection: null, fallbackToDialog: true };
}

function controlSelector(host: HTMLElement, focused: HTMLElement): string {
  let identity = fieldSelector(focused);
  if (focused.id) identity = `#${CSS.escape(focused.id)}`;
  if (identity === null && focused.localName === "a") identity = `a[href="${CSS.escape(focused.getAttribute("href") ?? "")}"]`;
  if (identity === null) return '[role="dialog"]';
  if (host.querySelectorAll(identity).length === 1) return identity;
  // Repeated task references keep their location as well as their target.
  const path: string[] = [];
  for (let element: HTMLElement | null = focused; element != null && element !== host; element = element.parentElement) {
    const peers = [...element.parentElement?.children ?? []].filter(peer => peer.localName === element.localName);
    path.unshift(`${element === focused ? identity : element.localName}:nth-of-type(${peers.indexOf(element) + 1})`);
  }
  return `:scope > ${path.join(" > ")}`;
}

// editedModalFields lists the fields inside host whose value differs from
// the one their markup drew.
export function editedModalFields(host: HTMLElement | null): Array<readonly [string, string]> {
  return [...host?.querySelectorAll<FormField>("input, textarea, select") ?? []]
    .filter(fieldEdited)
    .flatMap((field) => {
      const selector = fieldSelector(field);
      return selector === null ? [] : [[selector, field.value] as const];
    });
}

export function restoreModalEdits(host: HTMLElement | null, edited: ReadonlyArray<readonly [string, string]>): void {
  for (const [selector, value] of edited) {
    const field = host?.querySelector<FormField>(selector);
    if (field != null && field.value !== value) field.value = value;
  }
}

// restoreModalFocus reports whether the field took focus again, so an owner
// can skip the screen's initial focus.
export function restoreModalFocus(host: HTMLElement | null, focus: ModalFocus | null): boolean {
  if (focus === null) return false;
  let restored = host?.querySelector<HTMLElement>(focus.selector) ?? null;
  if (focus.fallbackToDialog && (restored === null || focus.selector === '[role="dialog"]')) {
    restored = host?.querySelector<HTMLElement>('[role="dialog"]') ?? null;
    if (restored !== null) restored.tabIndex = -1;
  }
  restored?.focus({ preventScroll: true });
  const selection = focus.selection;
  // An email field reports no selection and refuses one.
  if ((restored instanceof HTMLInputElement || restored instanceof HTMLTextAreaElement) && selection?.[0] != null) restored.setSelectionRange(selection[0], selection[1] ?? selection[0]);
  return restored !== null && document.activeElement === restored;
}

// fieldSelector names a modal field by its first data attribute, which a
// re-render of the same screen draws again.
function fieldSelector(field: HTMLElement): string | null {
  const attribute = [...field.attributes ?? []].find((attr) => attr.name.startsWith("data-") && attr.name !== "data-hint");
  return attribute === undefined ? null : `${field.localName}[${attribute.name}="${CSS.escape(attribute.value)}"]`;
}

function fieldEdited(field: FormField): boolean {
  if (field instanceof HTMLSelectElement) return [...field.options].some((option) => option.selected !== option.defaultSelected);
  // A file input accepts only an empty value from script.
  if (field instanceof HTMLInputElement && (field.type === "checkbox" || field.type === "radio" || field.type === "file")) return false;
  return field.value !== field.defaultValue;
}
