// Modeled regressions for the terminal mount and the mobile compose row it
// feeds. The workspace methods under test are extracted from live.ts and run
// against fakes, as in live-recovery.test.ts: importing the full workspace
// would load xterm and browser globals into node --test.
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { stripTypeScriptTypes } from "node:module";
import test from "node:test";
import type { SessionDetail } from "../api/sessions.ts";
import { MobileTerminalCompose } from "../terminal/mobile-compose.ts";

const source = readFileSync(new URL("./live.ts", import.meta.url), "utf8");

function method(name: string): string {
  const start = Math.max(source.indexOf(`  private ${name}(`), source.indexOf(`  private async ${name}(`));
  assert.ok(start >= 0, `workspace method ${name} exists`);
  const end = source.indexOf("\n  private ", start + 1);
  assert.ok(end > start, `workspace method ${name} has a bounded end`);
  return source.slice(start, end).trim();
}

// Every query answers with a stable child until the next innerHTML write,
// which drops them all as a real rewrite detaches the old nodes. Markup is
// never parsed.
class FakeElement {
  dataset: Record<string, string> = {};
  classList = { add() {} };
  value = "";
  inert = false;
  private children = new Map<string, FakeElement>();
  set innerHTML(_html: string) { this.children = new Map(); }
  querySelector(selector: string): FakeElement {
    let child = this.children.get(selector);
    if (child === undefined) {
      child = new FakeElement();
      this.children.set(selector, child);
    }
    return child;
  }
  querySelectorAll(): FakeElement[] { return []; }
  addEventListener() {}
  setAttribute() {}
  removeAttribute() {}
  focus() {}
}

// One attachment per terminal, counting forwarded writes the way
// AgentAttachment.trackWrite does: a write tracks only on the instance that
// forwarded it.
class FakeAgentTerminal {
  static created: FakeAgentTerminal[] = [];
  forwarded: string[] = [];
  tracked = 0;
  attached: string | null = null;
  disposed = false;
  readonly host: unknown;
  readonly mounted = {
    terminal: {},
    paste: (text: string) => { this.forwarded.push(text); },
    sendInput: (data: string) => { this.forwarded.push(data); },
    setSelectText: () => false,
  };
  constructor(host?: unknown) {
    this.host = host;
    FakeAgentTerminal.created.push(this);
  }
  attach(sessionID: string) { this.attached = sessionID; }
  dispose() { this.disposed = true; }
  trackWrite(send: () => void, done: (code: string, message: string) => void): boolean {
    const before = this.forwarded.length;
    send();
    if (this.forwarded.length !== before + 1) return false;
    this.tracked++;
    queueMicrotask(() => done("sent", ""));
    return true;
  }
}

interface Harness {
  epoch: number;
  terminal: FakeAgentTerminal | null;
  terminalCompose: MobileTerminalCompose | null;
  mountTerminal(detail: SessionDetail, epoch: number): Promise<void>;
}

function fixture() {
  FakeAgentTerminal.created = [];
  const fonts: Array<() => void> = [];
  const regions = new Map<string, FakeElement>();
  const region = (_root: unknown, name: string): FakeElement => {
    let node = regions.get(name);
    if (node === undefined) {
      node = new FakeElement();
      regions.set(name, node);
    }
    return node;
  };
  const collaborators = {
    AgentTerminal: FakeAgentTerminal,
    MobileTerminalCompose,
    MobileTerminalSoftKeys: class { dispose() {} setInert() {} },
    TaskRefHover: class { dispose() {} },
    ensureMonoFontLoaded: () => new Promise<void>(resolve => { fonts.push(resolve); }),
    isVerifiedRunningSession: () => true,
    region,
    mobileComposeMarkup: () => "",
    isComposeSubmitIntent: () => false,
    inertSoftKeyTerminal: () => ({}),
    createTerminal: () => ({ setSelectText: () => false }),
    terminalThemeFor: () => ({}),
    resolveTerminalTheme: () => ({}),
    terminalThemeName: () => "default",
    terminalFontSize: () => 14,
    hints: {},
    SECONDARY_SM_BUTTON: "",
  };
  const names = ["mountTerminal", "syncDock", "mountMobileCompose"];
  const code = stripTypeScriptTypes(`class MountHarness {\n${names.map(method).join("\n")}\n}`);
  const Constructor = new Function(...Object.keys(collaborators), `${code}\nreturn MountHarness;`)(...Object.values(collaborators)) as new () => Harness;
  const workspace = new Constructor();
  Object.assign(workspace, {
    root: new FakeElement(), disposed: false, epoch: 1, terminal: null, controllerID: "c1",
    recoveryKey: null, dockMode: "none", dockMounted: null, dockSource: null,
    terminalSoftKeys: null, terminalCompose: null, composeDrafts: new Map(),
    selectTextSessions: new Map(),
    selectedSessionID: "s1", connection: "connected", taskRefHover: null,
    applyTerminalGround() {}, renderAttachmentState() {}, syncAttachmentInputs() {}, setSoftKeysInert() {},
    paintSelectText() {},
    phoneLayout: () => true,
  });
  const pane = region(null, "terminal");
  return { workspace, fonts, pane, liveHost: () => pane.querySelector("[data-live-terminal]") };
}

const detail = { id: "s1", project_id: "p1", provider: "claude", status: "running" } as SessionDetail;
const settle = async (): Promise<void> => { for (let i = 0; i < 4; i++) await Promise.resolve(); };

test("a status poll during the font wait mounts one terminal, into the pane's host", async () => {
  const { workspace, fonts, liveHost } = fixture();
  const first = workspace.mountTerminal(detail, 1);
  const second = workspace.mountTerminal(detail, 1);
  for (const resolve of fonts) resolve();
  await Promise.all([first, second]);
  assert.equal(FakeAgentTerminal.created.length, 1);
  assert.equal(workspace.terminal, FakeAgentTerminal.created[0]);
  assert.equal(workspace.terminal?.host, liveHost());
});

test("a pane rewritten during the font wait is left to whoever rewrote it", async () => {
  const { workspace, fonts, pane, liveHost } = fixture();
  const live = workspace.mountTerminal(detail, 1);
  pane.innerHTML = "<div data-recovery-register></div>";
  const again = workspace.mountTerminal(detail, 1);
  for (const resolve of fonts) resolve();
  await Promise.all([live, again]);
  assert.equal(FakeAgentTerminal.created.length, 1);
  assert.equal(workspace.terminal?.host, liveHost(), "the terminal is in the host the pane shows");
  const recovered = fixture();
  const stranded = recovered.workspace.mountTerminal(detail, 1);
  recovered.pane.innerHTML = "<div data-recovery-register></div>";
  recovered.fonts[0]!();
  await stranded;
  assert.equal(recovered.workspace.terminal, null, "no terminal mounts into a detached host");
});

test("the compose row sends and tracks its draft through the mounted terminal", async () => {
  const { workspace, fonts } = fixture();
  const first = workspace.mountTerminal(detail, 1);
  const second = workspace.mountTerminal(detail, 1);
  for (const resolve of fonts) resolve();
  await Promise.all([first, second]);
  const compose = workspace.terminalCompose;
  assert.ok(compose !== null);
  const outcomes: string[] = [];
  assert.equal(compose.flush("ls -la", (code) => { outcomes.push(code); }), true, "the flush started");
  await settle();
  assert.deepEqual(outcomes, ["sent"]);
  const live = workspace.terminal;
  assert.ok(live !== null);
  assert.deepEqual(live.forwarded, ["ls -la", "\r"], "the Return followed the draft on the same terminal");
  assert.equal(live.tracked, 2);
  for (const other of FakeAgentTerminal.created) {
    if (other !== live) assert.deepEqual(other.forwarded, [], "nothing went through another terminal");
  }
});

test("the compose row tracks each write on the terminal that forwarded it", async () => {
  const { workspace, fonts } = fixture();
  const mount = workspace.mountTerminal(detail, 1);
  fonts[0]!();
  await mount;
  const docked = workspace.terminal;
  assert.ok(docked !== null);
  workspace.terminal = new FakeAgentTerminal();
  const compose = workspace.terminalCompose;
  assert.ok(compose !== null);
  assert.equal(compose.flush("ls -la", () => {}), true, "the flush started");
  assert.deepEqual(docked.forwarded, ["ls -la", "\r"]);
  assert.equal(docked.tracked, 2);
  assert.equal(workspace.terminal.tracked, 0);
});

test("a new selection still mounts while an older mount waits on the face", async () => {
  const { workspace, fonts, liveHost } = fixture();
  const stale = workspace.mountTerminal(detail, 1);
  workspace.epoch = 2;
  const current = workspace.mountTerminal(detail, 2);
  assert.equal(fonts.length, 2);
  for (const resolve of fonts) resolve();
  await Promise.all([stale, current]);
  assert.equal(FakeAgentTerminal.created.length, 1);
  assert.equal(workspace.terminal, FakeAgentTerminal.created[0]);
  assert.equal(workspace.terminal?.host, liveHost());
});
