import { test } from "node:test";
import assert from "node:assert/strict";
import { AgentTerminal } from "./attach.ts";
import type { MountedTerminal } from "./term.ts";
import type { ScrollCommand } from "./scroll.ts";
import type { Transport } from "./attachment.ts";
import type { TerminalOutbound } from "./outbound.ts";
import type { Terminal } from "@xterm/xterm";

class FakeTransport implements Transport {
  sent: string[] = [];
  closed = false;
  private msgFn: ((raw: string) => void) | null = null;
  private closeFn: (() => void) | null = null;
  send(raw: string): void {
    this.sent.push(raw);
  }
  close(): void {
    this.closed = true;
  }
  onMessage(fn: (raw: string) => void): void {
    this.msgFn = fn;
  }
  onClose(fn: () => void): void {
    this.closeFn = fn;
  }
  deliver(raw: string): void {
    this.msgFn?.(raw);
  }
  closeRemote(): void {
    this.closed = true;
    this.closeFn?.();
  }
  frames(): Array<Record<string, unknown>> {
    return this.sent.map((s) => JSON.parse(s));
  }
}

// The smallest Terminal shape AgentTerminal touches (write/cols/rows);
// everything else is the MountedTerminal wrapper's own record.
function fakeMount(record: {
  fontSizes: number[];
  fits?: number;
  fitDimensions?: { cols: number; rows: number };
  scrollFns?: Array<(command: ScrollCommand) => void>;
  scrollbackFns?: Array<(active: boolean) => void>;
  bufferType?: "normal" | "alternate";
  jumps?: number;
  resets?: number;
  ctrlCancels?: number;
  inputs?: string[];
  pastes?: string[];
  outbound?: (event: TerminalOutbound) => void;
}): MountedTerminal {
  const dimensions = {
    cols: 80,
    rows: 24,
    write: () => {},
  };
  const term = dimensions as unknown as Terminal;
  const input = (data: string): void => {
    record.outbound?.({ kind: "input", data: new TextEncoder().encode(data) });
  };
  return {
    terminal: term,
    fit: () => {
      record.fits = (record.fits ?? 0) + 1;
      if (record.fitDimensions !== undefined) {
        dimensions.cols = record.fitDimensions.cols;
        dimensions.rows = record.fitDimensions.rows;
      }
    },
    setSelectText: () => false,
    setTheme: () => {},
    setFontSize: (px) => {
      record.fontSizes.push(px);
    },
    onOutbound: (fn) => {
      record.outbound = fn;
      return { dispose: () => {} };
    },
    onScroll: (fn) => {
      record.scrollFns?.push(fn);
      return { dispose: () => {} };
    },
    onScrollbackChange: (fn) => {
      record.scrollbackFns?.push(fn);
      return { dispose: () => {} };
    },
    activateSoftKey: () => {},
    toggleCtrlChord: () => false,
    cancelCtrlChord: () => {
      record.ctrlCancels = (record.ctrlCancels ?? 0) + 1;
    },
    onCtrlChordChange: (fn) => {
      fn(false);
      return { dispose: () => {} };
    },
    focus: () => {},
    sendInput: (data) => {
      record.inputs?.push(data);
      input(data);
    },
    copyable: () => ({ fromSelection: false, text: "" }),
    paste: (text) => {
      record.pastes?.push(text);
      input(text);
    },
    jumpToLive: () => {
      record.jumps = (record.jumps ?? 0) + 1;
      record.scrollFns?.at(-1)?.({ direction: "live" });
    },
    resetScrollback: () => {
      record.resets = (record.resets ?? 0) + 1;
      if (record.bufferType === "normal") record.scrollbackFns?.at(-1)?.(false);
    },
    dispose: () => {},
  };
}

function withLocation(run: () => void): void {
  const original = Object.getOwnPropertyDescriptor(globalThis, "location");
  Object.defineProperty(globalThis, "location", {
    value: { protocol: "http:", host: "noisedeck.test" },
    configurable: true,
  });
  try {
    run();
  } finally {
    if (original === undefined) {
      delete (globalThis as { location?: unknown }).location;
    } else {
      Object.defineProperty(globalThis, "location", original);
    }
  }
}

test("setFontSize refits the mount AND propagates the new geometry to the PTY", (t) => {
  withLocation(() => {
    const record = {
      fontSizes: [] as number[],
      fits: 0,
      fitDimensions: { cols: 70, rows: 22 },
    };
    const transport = new FakeTransport();
    const term = new AgentTerminal(
      {} as HTMLElement,
      {},
      {
        controllerID: "test-controller",
        transportFactory: () => transport,
        mountTerminal: () => fakeMount(record),
      },
    );
    t.after(() => term.dispose());
    term.attach("session-1");
    transport.deliver(`{"type":"attached","token":7,"generation":1}`);

    term.setFontSize(16);

    assert.deepEqual(record.fontSizes, [16], "the mount receives the new size");
    assert.equal(record.fits, 1, "AgentTerminal performs the fit after changing cell geometry");
    const resizes = transport.frames().filter((f) => f.type === "resize");
    assert.equal(resizes.length, 1, "only the font-driven geometry change reaches the PTY");
    assert.equal(resizes[0]!.token, 7);
    assert.equal(resizes[0]!.cols, 70);
    assert.equal(resizes[0]!.rows, 22);
  });
});

test("attach, session replacement, and disconnect cancel a pending soft Ctrl chord", (t) => {
  withLocation(() => {
    const record = { fontSizes: [] as number[], ctrlCancels: 0 };
    const transports: FakeTransport[] = [];
    const term = new AgentTerminal(
      {} as HTMLElement,
      {},
      {
        controllerID: "test-controller",
        transportFactory: () => {
          const transport = new FakeTransport();
          transports.push(transport);
          return transport;
        },
        mountTerminal: () => fakeMount(record),
      },
    );
    t.after(() => term.dispose());

    term.attach("session-1");
    assert.equal(record.ctrlCancels, 1, "the first attachment starts unmodified");
    term.attach("session-2");
    assert.equal(record.ctrlCancels, 2, "a session switch cancels the prior chord");
    assert.equal(transports[0]?.closed, true, "the replaced attachment is closed");
    term.disconnect();
    assert.equal(record.ctrlCancels, 3, "disconnect cancels a pending chord before reconnect");
  });
});

test("every current attachment termination signal cancels a pending soft Ctrl chord", (t) => {
  withLocation(() => {
    const endings = [
      `{"type":"error","code":"lease_held","message":"held"}`,
      `{"type":"error","code":"fenced","message":"superseded"}`,
      `{"type":"error","code":"lease_expired","message":"expired"}`,
      `{"type":"error","code":"slow_consumer","message":"dropped"}`,
      `{"type":"error","code":"internal","message":"failed"}`,
      null,
    ];

    for (const ending of endings) {
      const record = { fontSizes: [] as number[], ctrlCancels: 0 };
      const transport = new FakeTransport();
      const term = new AgentTerminal(
        {} as HTMLElement,
        {},
        {
          controllerID: "test-controller",
          transportFactory: () => transport,
          mountTerminal: () => fakeMount(record),
        },
      );
      t.after(() => term.dispose());
      term.attach("session-1");
      record.ctrlCancels = 0;

      if (ending === null) transport.closeRemote();
      else transport.deliver(ending);

      assert.equal(record.ctrlCancels, 1, `Ctrl remained armed after ${ending ?? "transport close"}`);
    }
  });
});

test("attach waits for authoritative live state and jump-to-live sends no input bytes", (t) => {
  withLocation(() => {
    const record = {
      fontSizes: [] as number[],
      jumps: 0,
      resets: 0,
      scrollFns: [] as Array<(command: ScrollCommand) => void>,
    };
    const transport = new FakeTransport();
    const term = new AgentTerminal(
      {} as HTMLElement,
      {},
      {
        controllerID: "test-controller",
        transportFactory: () => transport,
        mountTerminal: () => fakeMount(record),
      },
    );
    t.after(() => term.dispose());

    term.attach("session-1");
    assert.equal(record.resets, 1);
    transport.deliver(`{"type":"attached","token":7,"generation":1}`);
    assert.deepEqual(
      transport.frames().filter((frame) => frame.type === "scroll"),
      [],
      "the server-side attach handshake owns stale tmux view exit",
    );

    term.jumpToLive();
    assert.equal(record.jumps, 1);
    assert.equal(
      transport.frames().filter((frame) => frame.type === "scroll" && frame.direction === "live").length,
      1,
    );
    assert.equal(
      transport.frames().filter((frame) => frame.type === "input").length,
      0,
      "jump-to-live never emits terminal input",
    );
  });
});

test("alternate reconnect preserves the pill until the attach handshake reports live", (t) => {
  withLocation(() => {
    const record = {
      fontSizes: [] as number[],
      bufferType: "alternate" as const,
      scrollbackFns: [] as Array<(active: boolean) => void>,
    };
    const transport = new FakeTransport();
    const changes: boolean[] = [];
    const term = new AgentTerminal(
      {} as HTMLElement,
      { onScrollbackChange: (active) => changes.push(active) },
      {
        controllerID: "test-controller",
        transportFactory: () => transport,
        mountTerminal: () => fakeMount(record),
      },
    );
    t.after(() => term.dispose());

    record.scrollbackFns[0]?.(true);
    assert.deepEqual(changes, [true]);
    term.attach("session-1");
    assert.deepEqual(changes, [true], "alternate reset cannot publish live state locally");

    transport.deliver(`{"type":"error","code":"attach_failed","message":"retry"}`);
    assert.deepEqual(changes, [true], "a refused attach retains the visible retry affordance");

    term.attach("session-2");
    assert.deepEqual(changes, [true], "session replacement also waits for tmux evidence");
    transport.deliver(`{"type":"scroll_status","active":false}`);
    transport.deliver(`{"type":"attached","token":8,"generation":2}`);
    assert.deepEqual(changes, [true, false]);
  });
});

test("alternate scrollback state changes only after the server acknowledgment", (t) => {
  withLocation(() => {
    const record = {
      fontSizes: [] as number[],
      scrollFns: [] as Array<(command: ScrollCommand) => void>,
    };
    const transport = new FakeTransport();
    const changes: boolean[] = [];
    const term = new AgentTerminal(
      {} as HTMLElement,
      { onScrollbackChange: (active) => changes.push(active) },
      {
        controllerID: "test-controller",
        transportFactory: () => transport,
        mountTerminal: () => fakeMount(record),
      },
    );
    t.after(() => term.dispose());

    term.attach("session-1");
    transport.deliver(`{"type":"attached","token":7,"generation":1}`);
    record.scrollFns[0]?.({ direction: "up", lines: 3 });
    assert.deepEqual(changes, [], "forwarding a request does not claim copy-mode entered");

    transport.deliver(`{"type":"scroll_status","active":true}`);
    assert.deepEqual(changes, [true]);
    term.jumpToLive();
    assert.deepEqual(changes, [true], "requesting live does not hide the pill before tmux confirms it");

    transport.deliver(`{"type":"scroll_status","active":false}`);
    assert.deepEqual(changes, [true, false]);
  });
});

test("an injected terminalPath routes the socket with the encoded ID and unchanged attach semantics", (t) => {
  withLocation(() => {
    const record = {
      fontSizes: [] as number[],
      scrollFns: [] as Array<(command: ScrollCommand) => void>,
    };
    const urls: string[] = [];
    const transport = new FakeTransport();
    const term = new AgentTerminal(
      {} as HTMLElement,
      {},
      {
        controllerID: "test-controller",
        terminalPath: (id) => `/api/attempts/${encodeURIComponent(id)}/terminal`,
        transportFactory: (url) => {
          urls.push(url);
          return transport;
        },
        mountTerminal: () => fakeMount(record),
      },
    );
    t.after(() => term.dispose());

    term.attach("attempt/a b", { takeover: true });
    assert.deepEqual(urls, [
      "ws://noisedeck.test/api/attempts/attempt%2Fa%20b/terminal" +
        "?controller=test-controller&cols=80&rows=24&takeover=1",
    ]);

    // The lease handshake and generation-fenced frames ride the injected
    // route exactly as they do the session route.
    transport.deliver(`{"type":"attached","token":9,"generation":4}`);
    record.scrollFns[0]?.({ direction: "up", lines: 3 });
    assert.deepEqual(transport.frames().filter((f) => f.type === "scroll"), [
      { type: "scroll", token: 9, generation: 4, direction: "up", lines: 3 },
    ]);
  });
});

test("the default terminalPath is the encoded session terminal route", (t) => {
  withLocation(() => {
    const record = { fontSizes: [] as number[] };
    const urls: string[] = [];
    const transport = new FakeTransport();
    const term = new AgentTerminal(
      {} as HTMLElement,
      {},
      {
        controllerID: "test-controller",
        transportFactory: (url) => {
          urls.push(url);
          return transport;
        },
        mountTerminal: () => fakeMount(record),
      },
    );
    t.after(() => term.dispose());

    term.attach("session/1 2");
    assert.deepEqual(urls, [
      "ws://noisedeck.test/api/sessions/session%2F1%202/terminal" +
        "?controller=test-controller&cols=80&rows=24",
    ]);
  });
});

test("mount scroll commands become scroll frames only while an attachment is live", (t) => {
  withLocation(() => {
    const record = {
      fontSizes: [] as number[],
      scrollFns: [] as Array<(c: ScrollCommand) => void>,
    };
    const transport = new FakeTransport();
    const term = new AgentTerminal(
      {} as HTMLElement,
      {},
      {
        controllerID: "test-controller",
        transportFactory: () => transport,
        mountTerminal: () => fakeMount(record),
      },
    );
    t.after(() => term.dispose());
    assert.equal(record.scrollFns.length, 1, "the terminal subscribes once at mount");
    const emit = record.scrollFns[0]!;

    // No attachment yet: the command is dropped, never queued.
    emit({ direction: "up", lines: 3 });
    assert.equal(transport.sent.length, 0);

    term.attach("session-1");
    // Attach opened but not acknowledged: still no token, still dropped.
    emit({ direction: "up", lines: 3 });
    assert.equal(
      transport.frames().filter((f) => f.type === "scroll").length,
      0,
      "scroll must not be sent before the fencing token exists",
    );

    transport.deliver(`{"type":"attached","token":7,"generation":1}`);
    emit({ direction: "up", lines: 3 });
    emit({ direction: "down", lines: 24 });

    // Scroll frames carry the generation like writes do: the server runs
    // the same pane-identity preflight before moving a real pane's view.
    const scrolls = transport.frames().filter((f) => f.type === "scroll");
    assert.deepEqual(scrolls, [
      { type: "scroll", token: 7, generation: 1, direction: "up", lines: 3 },
      { type: "scroll", token: 7, generation: 1, direction: "down", lines: 24 },
    ]);
  });
});

test("trackWrite wraps any mount write and sendTracked delegates to it", (t) => {
  withLocation(() => {
    const record = { fontSizes: [] as number[], inputs: [] as string[], pastes: [] as string[] };
    const transport = new FakeTransport();
    const outcomes: string[] = [];
    const term = new AgentTerminal(
      {} as HTMLElement,
      {},
      {
        controllerID: "test-controller",
        transportFactory: () => transport,
        mountTerminal: () => fakeMount(record),
      },
    );
    t.after(() => term.dispose());
    term.attach("session-1");
    transport.deliver(`{"type":"attached","token":7,"generation":1}`);

    assert.equal(term.trackWrite(() => term.mounted.paste("draft"), (code) => outcomes.push(`paste:${code}`)), true);
    assert.equal(term.sendTracked("\r", (code) => outcomes.push(`input:${code}`)), true);
    assert.deepEqual(record.pastes, ["draft"]);
    assert.deepEqual(record.inputs, ["\r"]);
    assert.deepEqual(outcomes, []);
    transport.deliver(`{"type":"status","code":"sent","message":""}`);
    assert.deepEqual(outcomes, ["paste:sent"]);
    transport.deliver(`{"type":"status","code":"sent","message":""}`);
    assert.deepEqual(outcomes, ["paste:sent", "input:sent"]);
  });
});
