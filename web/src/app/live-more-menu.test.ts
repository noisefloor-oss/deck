// Modeled DOM regressions for the More menu mount path. Use the real menu
// renderer and extract only the workspace methods under test: importing the
// full workspace would load terminal/browser dependencies into node --test.
// EventTarget dispatch, connected nodes, focus and replacement are modeled;
// a real browser probe still owns click actionability and layout verification.
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { stripTypeScriptTypes } from "node:module";
import test from "node:test";
import { confirmsBeforeClose, confirmsBeforeRemove, sessionMenuHtml } from "./session-menu.ts";
import { DEFAULT_CUSTOM_SLOTS } from "../terminal/themes.ts";
import { menuRows } from "../ui/dialog-focus.ts";
import { restoreModalFocus } from "../ui/modal-fields.ts";

const source = readFileSync(new URL("./live.ts", import.meta.url), "utf8");
let active: FakeElement | null = null;

class FakeElement extends EventTarget {
  isConnected = true;
  style: Record<string, string> = {};
  children: FakeElement[] = [];
  dataset: Record<string, string> = {};
  offsetWidth = 252;
  disabled = false;
  rect = { bottom: 40, right: 300 };
  textContent = "";
  readonly listeners = new Map<string, number>();

  readonly attrs: Record<string, string>;
  constructor(attrs: Record<string, string>) { super(); this.attrs = attrs; }
  override addEventListener(type: string, listener: EventListenerOrEventListenerObject | null, options?: boolean | AddEventListenerOptions): void {
    this.listeners.set(type, (this.listeners.get(type) ?? 0) + 1);
    super.addEventListener(type, listener, options);
  }
  matches(selector: string): boolean {
    const match = /^\[([^=\]]+)(?:="([^"]*)")?\]$/.exec(selector);
    return match !== null && Object.hasOwn(this.attrs, match[1]!)
      && (match[2] === undefined || this.attrs[match[1]!] === match[2]);
  }
  querySelector(selector: string): FakeElement | null { return this.children.find(node => node.matches(selector)) ?? null; }
  querySelectorAll(selector: string): FakeElement[] { return this.children.filter(node => node.matches(selector)); }
  closest(selector: string): FakeElement | null { return this.matches(selector) ? this : null; }
  focus(): void { active = this; }
  click(): void {
    if (!this.isConnected) throw new Error("Detached control");
    if (!this.disabled) this.dispatchEvent(new Event("click"));
  }
  disconnect(): void { this.isConnected = false; this.children.forEach(node => node.disconnect()); }
  getBoundingClientRect(): { bottom: number; right: number } { return this.rect; }
}

class FakePopovers extends FakeElement {
  mounts = 0;
  html = "";
  constructor() { super({ "data-region": "popovers" }); }
  replaceChildren(): void { this.children.forEach(node => node.disconnect()); this.children = []; }
  set innerHTML(html: string) {
    this.replaceChildren(); this.mounts++; this.html = html;
    const backdrop = new FakeElement({ "data-popover-backdrop": "" });
    const menu = new FakeElement({ "data-more-popover": "" });
    // Keep all data attributes on each element, not a separate node per hook.
    // Descendant lookup is flat because these methods use data-hook selectors.
    for (const tag of html.matchAll(/<(button|span|div)\b([^>]*)>/g)) {
      const attrs: Record<string, string> = {};
      for (const attr of tag[2]!.matchAll(/\b(data-[\w-]+)(?:="([^"]*)")?/g)) attrs[attr[1]!] = attr[2] ?? "";
      if (Object.hasOwn(attrs, "data-more-popover") || Object.hasOwn(attrs, "data-popover-backdrop")) continue;
      const node = new FakeElement(attrs);
      for (const [key, value] of Object.entries(attrs)) node.dataset[key.slice(5).replace(/-([a-z])/g, (_, c: string) => c.toUpperCase())] = value;
      menu.children.push(node);
    }
    this.children = [backdrop, menu];
  }
}

interface Settings { skip_permissions: boolean; skip_close_confirm: boolean; skip_remove_project_confirm: boolean }
interface Harness {
  openMore(): void;
  showMore(): void;
  refreshOpenPopover(): void;
  closePopover(): void;
  popoverOpen: string | null;
  moreMenuRender: { html: string; node: FakeElement } | null;
  moreFocus: string | null;
  themeFlyoutOpen: boolean;
  skipPermissions: boolean | null;
  skipCloseConfirm: boolean | null;
  skipRemoveProjectConfirm: boolean | null;
  sidebar: { isCollapsed: boolean; toggleCollapsed(): void };
  infoBar: { isShown: boolean; setShown(value: boolean): void };
  logoutCalls: number;
  disposed: boolean;
}

function method(name: string): string {
  const start = source.indexOf(`  private ${name}(`);
  assert.ok(start >= 0, `workspace method ${name} exists`);
  const end = source.indexOf("\n  private ", start + 1);
  assert.ok(end > start, `workspace method ${name} has a bounded end`);
  return source.slice(start, end).trim();
}

function fixture() {
  const popovers = new FakePopovers(), trigger = new FakeElement({ "data-live-more": "" });
  const root = {
    querySelector(selector: string): FakeElement | null {
      if (selector === "[data-live-more]") return trigger;
      if (selector === '[data-region="popovers"]') return popovers;
      if (selector.includes("[data-more-popover] ")) return popovers.querySelector("[data-more-popover]")?.querySelector(selector.split(" ").at(-1)!) ?? null;
      return null;
    },
    closest: () => null,
  };
  let settingsRequests = 0, fontSize = 14;
  let resolveSettings: ((value: Settings) => void) | undefined;
  const collaborators = {
    sessionMenuHtml, confirmsBeforeClose, confirmsBeforeRemove,
    region: () => popovers,
    window: { matchMedia: () => ({ matches: true }) },
    terminalFontSize: () => fontSize,
    terminalThemeName: () => "noisefloor",
    customThemeSlots: () => DEFAULT_CUSTOM_SLOTS,
    setTerminalFontSize: (value: number) => fontSize = value,
    menuRows,
    focusedModalControl: () => null,
    restoreModalFocus,
  };
  const names = ["anchorPopover", "positionMoreMenu", "focusInMore", "applyMoreFocus", "menuIsNarrow", "closePopover", "refreshOpenPopover", "showMore", "moreMenuHtml", "loginActionLabel", "openMore"];
  const code = stripTypeScriptTypes(`class MenuHarness {\n${names.map(method).join("\n")}\n}`);
  const Constructor = new Function(...Object.keys(collaborators), `${code}\nreturn MenuHarness;`)(...Object.values(collaborators)) as new () => Harness;
  const workspace = new Constructor();
  Object.assign(workspace, {
    root, popoverOpen: null, themeFlyoutOpen: false, moreFocus: null, moreMenuRender: null,
    panelsMenuCleanup: null, security: null, autoColorProjects: false, confirmingYolo: false,
    skipPermissions: false, skipCloseConfirm: false, skipRemoveProjectConfirm: false, disposed: false,
    sidebar: { isCollapsed: false, toggleCollapsed() { this.isCollapsed = !this.isCollapsed; } },
    infoBar: { isShown: true, setShown(value: boolean) { this.isShown = value; } },
    api: { getSettings() { settingsRequests++; return new Promise<Settings>(resolve => { resolveSettings = resolve; }); } },
    setSkipCloseConfirm(value: boolean) { workspace.skipCloseConfirm = value; },
    setSkipRemoveProjectConfirm(value: boolean) { workspace.skipRemoveProjectConfirm = value; },
    handleError() { assert.fail("unexpected settings error"); },
    logoutCalls: 0,
    signOut(button: FakeElement) { workspace.logoutCalls++; button.disabled = true; },
  });
  return {
    workspace, popovers, trigger,
    get menu(): FakeElement { const node = popovers.querySelector("[data-more-popover]"); assert.ok(node); return node; },
    get logout(): FakeElement { const node = this.menu.querySelector("[data-live-logout]"); assert.ok(node); return node; },
    get settingsRequests(): number { return settingsRequests; },
    async settleSettings(value: Settings): Promise<void> { assert.ok(resolveSettings); resolveSettings(value); await Promise.resolve(); await Promise.resolve(); },
  };
}

test("unchanged status refresh retains the menu, pending logout state and exactly one handler", () => {
  const f = fixture(); f.workspace.openMore(); const menu = f.menu, button = f.logout;
  for (let i = 0; i < 20; i++) f.workspace.refreshOpenPopover();
  assert.equal(f.menu, menu); assert.equal(f.logout, button); assert.equal(f.popovers.mounts, 1);
  assert.equal(button.listeners.get("click"), 1); button.click(); assert.equal(f.workspace.logoutCalls, 1);
  f.workspace.refreshOpenPopover(); assert.equal(f.logout, button); assert.equal(button.disabled, true);
  button.click(); assert.equal(f.workspace.logoutCalls, 1);
});

test("a missing retained-menu record replaces the mounted controls", () => {
  const f = fixture(); f.workspace.openMore(); const button = f.logout;
  f.workspace.moreMenuRender = null; f.workspace.refreshOpenPopover();
  assert.equal(button.isConnected, false); assert.throws(() => button.click(), /Detached control/);
});

test("fresh unchanged settings retain nodes; actual changed settings redraw", async () => {
  const f = fixture(); f.workspace.showMore(); const button = f.logout; assert.equal(f.settingsRequests, 1);
  await f.settleSettings({ skip_permissions: false, skip_close_confirm: false, skip_remove_project_confirm: false });
  assert.notEqual(f.logout, button, "resolving the unread yolo state removes its unavailable presentation");
  const settled = f.logout;
  f.workspace.refreshOpenPopover();
  assert.equal(f.logout, settled); assert.equal(f.settingsRequests, 1); assert.equal(f.workspace.skipPermissions, false);
  f.workspace.closePopover(); f.workspace.showMore(); const reopened = f.logout; assert.equal(f.settingsRequests, 2);
  await f.settleSettings({ skip_permissions: true, skip_close_confirm: true, skip_remove_project_confirm: true });
  assert.notEqual(f.logout, reopened); assert.equal(reopened.isConnected, false); assert.equal(f.workspace.skipCloseConfirm, true);
});

test("changed controls redraw once and retained handlers read current workspace state", () => {
  const f = fixture(); f.workspace.openMore(); const original = f.menu;
  f.menu.querySelector("[data-show-info-bar]")!.click(); assert.equal(f.workspace.infoBar.isShown, false); assert.notEqual(f.menu, original);
  const changed = f.menu; f.workspace.refreshOpenPopover(); assert.equal(f.menu, changed);
  f.menu.querySelector("[data-show-side-panel]")!.click(); assert.equal(f.workspace.sidebar.isCollapsed, true); assert.notEqual(f.menu, changed);
  assert.equal(f.logout.listeners.get("click"), 1);
});

test("unchanged refresh preserves moved focus and updates position", () => {
  const f = fixture(); f.workspace.openMore(); f.workspace.moreFocus = "[data-theme-flyout]"; f.logout.focus(); const menu = f.menu;
  f.trigger.rect = { bottom: 80, right: 600 }; f.workspace.refreshOpenPopover();
  assert.equal(f.menu, menu); assert.equal(active, f.logout); assert.equal(menu.style.top, "86px"); assert.equal(menu.style.left, "348px");
});

test("a font step followed by refresh retains its controls and focus", () => {
  const f = fixture();
  f.workspace.openMore();
  const menu = f.menu;
  const increment = menu.querySelector("[data-font-inc]");
  const label = menu.querySelector("[data-font-size]");
  assert.ok(increment);
  assert.ok(label);
  increment.focus();
  increment.click();
  assert.equal(label.textContent, "15");

  f.workspace.refreshOpenPopover();
  assert.equal(f.menu, menu);
  assert.equal(menu.querySelector("[data-font-inc]"), increment);
  assert.equal(active, increment);
  assert.equal(increment.isConnected, true);
  assert.equal(increment.listeners.get("click"), 1);
});

test("theme navigation still remounts with narrow focus and Escape restores the trigger", () => {
  const f = fixture(); f.workspace.openMore(); const original = f.menu; f.menu.querySelector("[data-theme-flyout]")!.click();
  assert.notEqual(f.menu, original); assert.equal(active, f.menu.querySelector("[data-theme-back]"));
  f.menu.querySelector("[data-theme-back]")!.click(); assert.equal(f.workspace.themeFlyoutOpen, false); assert.equal(active, f.menu.querySelector("[data-theme-flyout]"));
  const escape = new Event("keydown", { cancelable: true }); Object.defineProperty(escape, "key", { value: "Escape" }); f.menu.dispatchEvent(escape);
  assert.equal(f.workspace.popoverOpen, null); assert.equal(active, f.trigger); assert.equal(f.popovers.children.length, 0);
});

test("close/reopen and another shared-region menu never reuse a stale node", () => {
  const f = fixture(); f.workspace.openMore(); const button = f.logout; f.popovers.querySelector("[data-popover-backdrop]")!.click();
  assert.equal(button.isConnected, false); assert.equal(f.workspace.moreMenuRender, null); f.workspace.openMore(); assert.notEqual(f.logout, button);
  const replaced = f.logout; f.popovers.replaceChildren(); f.workspace.refreshOpenPopover();
  assert.equal(replaced.isConnected, false); assert.notEqual(f.logout, replaced); assert.equal(f.logout.listeners.get("click"), 1);
});

test("settings resolving after close or disposal do not reopen the menu", async () => {
  for (const disposed of [false, true]) {
    const f = fixture(); f.workspace.showMore(); f.workspace.closePopover(); f.workspace.disposed = disposed;
    await f.settleSettings({ skip_permissions: true, skip_close_confirm: true, skip_remove_project_confirm: true });
    assert.equal(f.workspace.popoverOpen, null); assert.equal(f.popovers.children.length, 0); assert.equal(f.popovers.mounts, 1);
  }
});
