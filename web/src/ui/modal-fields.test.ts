import assert from "node:assert/strict";
import test from "node:test";
import { focusedModalControl, focusedModalField, restoreModalFocus } from "./modal-fields.ts";

test("an empty or outside focus needs no DOM constructors", () => {
  const previous = globalThis.document;
  Object.assign(globalThis, { document: { activeElement: null } });
  try {
    assert.equal(focusedModalControl(null), null);
    Object.assign(globalThis.document, { activeElement: {} });
    assert.equal(focusedModalControl({ contains: () => false } as unknown as HTMLElement), null);
  } finally { Object.assign(globalThis, { document: previous }); }
});

test("dialog refresh preserves button identity and falls back without focusing a field", (t) => {
  const saved = Object.fromEntries(["document", "HTMLElement", "HTMLInputElement", "HTMLTextAreaElement", "HTMLSelectElement", "CSS"].map(key => [key, (globalThis as Record<string, unknown>)[key]]));
  const document = { activeElement: null as Element | null };
  class Element {
    localName = "button";
    id = "";
    tabIndex = 0;
    attributes = [{ name: "data-cancel", value: "" }];
    focus(): void { document.activeElement = this; }
  }
  class Field extends Element {}
  Object.assign(globalThis, { document, HTMLElement: Element, HTMLInputElement: Field, HTMLTextAreaElement: Field, HTMLSelectElement: Field, CSS: { escape: (value: string) => value } });
  t.after(() => Object.assign(globalThis, saved));
  const oldButton = new Element();
  const newButton = new Element();
  const dialog = new Element();
  dialog.localName = "section";
  let target: Element | null = newButton;
  const host = {
    contains: (node: unknown) => node === oldButton,
    querySelectorAll: () => [oldButton],
    querySelector: (selector: string) => selector === '[role="dialog"]' ? dialog : target,
  } as unknown as HTMLElement;
  document.activeElement = oldButton;
  const focus = focusedModalControl(host);
  assert.equal(focus?.selector, 'button[data-cancel=""]');
  assert.equal(focusedModalField(host), null, "sidebar navigation keeps field-only behavior");
  assert.equal(restoreModalFocus(host, focus), true);
  assert.equal(document.activeElement, newButton);
  target = null;
  assert.equal(restoreModalFocus(host, focus), true);
  assert.equal(document.activeElement, dialog);
  assert.equal(dialog.tabIndex, -1, "the fallback dialog is programmatically focusable");
  document.activeElement = oldButton;
  assert.equal(restoreModalFocus(host, { selector: "input[data-old]", selection: null }), false);
  assert.equal(document.activeElement, oldButton, "field-only navigation does not steal focus");
});
