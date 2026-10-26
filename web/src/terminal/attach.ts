// AgentTerminal wires the browser pieces — xterm.js, ResizeObserver, and
// a real WebSocket — to the protocol logic in attachment.ts. It holds no
// protocol state of its own (that lives in AgentAttachment) and no
// application state of any kind: the browser owns nothing authoritative.
// The terminal mount itself is injected: the caller owns the app choices
// (theme, font size), and this module never imports xterm.js — tests run
// DOM-free against a fake mount.
import { recordTerminalGeometry } from "./last-geometry.ts";
import type { ITheme, MountedTerminal } from "./term.ts";
import type { ScrollCommand } from "./scroll.ts";
import {
  AgentAttachment,
  type AttachmentCallbacks,
  type Transport,
} from "./attachment.ts";

// AgentTerminalCallbacks extends the protocol callbacks with terminal-
// level signals the app chrome consumes. None of them carry
// or interpret content:
// - onOutput fires when an output frame's bytes reach xterm — the only
//   place the client ever sees output flow, and only for the attached
//   (selected) session.
// - onScrollCommand fires when a scrollback command was forwarded to the
//   live attachment (including an explicit return to the live view).
// - onScrollbackChange projects xterm's native viewport position or tmux's
//   acknowledged post-command copy-mode state.
// - onInput fires on user input bytes (keyboard/paste) — the server
//   cancels copy-mode before writing input, so this is the client-side
//   echo of that exit.
export interface AgentTerminalCallbacks extends AttachmentCallbacks {
  onOutput?: () => void;
  onScrollCommand?: (command: ScrollCommand) => void;
  onScrollbackChange?: (active: boolean) => void;
  onInput?: () => void;
}

// WebSocketTransport adapts the browser socket to the Transport shape.
class WebSocketTransport implements Transport {
  private ws: WebSocket;

  constructor(url: string) {
    this.ws = new WebSocket(url);
  }

  send(raw: string): void {
    if (this.ws.readyState === WebSocket.OPEN) {
      this.ws.send(raw);
    }
  }

  close(): void {
    this.ws.close();
  }

  onMessage(fn: (raw: string) => void): void {
    this.ws.onmessage = (event) => {
      if (typeof event.data === "string") {
        fn(event.data);
      }
    };
  }

  onClose(fn: () => void): void {
    this.ws.onclose = () => fn();
  }
}

export interface AgentTerminalOptions {
  // controllerID identifies this browser attachment to the lease. One is
  // generated per AgentTerminal; a reconnect reuses it, which is what
  // makes the server renew the lease and keep the token.
  controllerID?: string;
  // terminalPath maps a session ID to the websocket path. The default is
  // the session terminal route /api/sessions/<id>/terminal; an attempt
  // attachment injects its own route. The query (controller, geometry,
  // takeover) is always appended by AgentTerminal.
  terminalPath?: (sessionID: string) => string;
  // transportFactory exists for tests; production uses WebSocket.
  transportFactory?: (url: string) => Transport;
  // mountTerminal mounts the real terminal into the container. The caller
  // owns the app choices (theme, font size); tests pass a fake.
  mountTerminal: (container: HTMLElement) => MountedTerminal;
}

type ContainerSize = { width: number; height: number };
type PendingAttach = { sessionID: string; takeover: boolean; generation: number };

export class AgentTerminal {
  readonly mounted: MountedTerminal;
  private attachment: AgentAttachment | null = null;
  private observer: ResizeObserver | null = null;
  private container: HTMLElement;
  private callbacks: AgentTerminalCallbacks;
  private controllerID: string;
  private terminalPath: (sessionID: string) => string;
  private makeTransport: (url: string) => Transport;
  private attachmentGeneration = 0;
  private pendingAttach: PendingAttach | null = null;
  private scrollSubscription: { dispose(): void } | null = null;
  private scrollbackSubscription: { dispose(): void } | null = null;
  private lastFitSize: ContainerSize | null = null;
  private lastReportedGeometry: string | null = null;
  private disposed = false;

  constructor(
    container: HTMLElement,
    callbacks: AgentTerminalCallbacks = {},
    options: AgentTerminalOptions,
  ) {
    this.container = container;
    this.callbacks = callbacks;
    this.mounted = options.mountTerminal(container);
    this.lastFitSize = this.containerSize();
    this.controllerID = options.controllerID ?? crypto.randomUUID();
    this.terminalPath = options.terminalPath ??
      ((id) => `/api/sessions/${encodeURIComponent(id)}/terminal`);
    this.makeTransport = options.transportFactory ?? ((url) => new WebSocketTransport(url));

    // Scrollback commands (wheel/keys over the alternate screen) go to
    // whichever attachment is live; with none, they are dropped — there is
    // no view to move (and no signal fires: nothing entered copy-mode).
    this.scrollSubscription = this.mounted.onScroll((command) => {
      if (this.attachment === null) return;
      this.attachment.scroll(command);
      this.callbacks.onScrollCommand?.(command);
    });
    this.scrollbackSubscription = this.mounted.onScrollbackChange((active) => {
      this.callbacks.onScrollbackChange?.(active);
    });

    // Tests run DOM-free unless they install a ResizeObserver fake.
    if (typeof ResizeObserver !== "undefined") {
      this.observer = new ResizeObserver(() => {
        this.resizeFromObserver();
      });
      this.observer.observe(container);
    }
  }

  // attach selects one session for attachment. takeover=1 asks for control
  // even if another controller holds the lease.
  attach(sessionID: string, opts: { takeover?: boolean } = {}): void {
    this.mounted.cancelCtrlChord();
    this.mounted.resetScrollback();
    const generation = ++this.attachmentGeneration;
    this.attachment?.dispose();
    this.attachment = null;
    this.pendingAttach = {
      sessionID,
      takeover: opts.takeover === true,
      generation,
    };
    this.lastReportedGeometry = null;
    this.startPendingAttach();
  }

  // A new font size changes the cell geometry: refit, then propagate the
  // new rows/cols to the provider PTY exactly as a container resize would.
  setFontSize(px: number): void {
    this.mounted.setFontSize(px);
    this.fitAndResize();
  }

  setTheme(theme: ITheme): void {
    this.mounted.setTheme(theme);
  }

  // End only the live socket/lease while preserving the terminal pixels.
  // Reconciliation can declare a session inactive before the socket's close
  // event arrives; the UI must then remove controller actions immediately.
  disconnect(): void {
    this.mounted.cancelCtrlChord();
    ++this.attachmentGeneration;
    this.pendingAttach = null;
    this.attachment?.dispose();
    this.attachment = null;
    this.lastReportedGeometry = null;
    this.mounted.resetScrollback();
  }

  jumpToLive(): void {
    if (this.disposed) return;
    this.mounted.jumpToLive();
  }

  // Runs one synchronous terminal write and reports that write's own outcome
  // (sent, failed, uncertain, interrupted) to done. False when nothing is
  // attached, in which case nothing was written. A reattach ends the old
  // attachment, so it can only settle as interrupted.
  trackWrite(send: () => void, done: (code: string, message: string) => void): boolean {
    const attachment = this.attachment;
    if (this.disposed || attachment === null) return false;
    return attachment.trackWrite(send, done);
  }

  sendTracked(text: string, done: (code: string, message: string) => void): boolean {
    return this.trackWrite(() => this.mounted.sendInput(text), done);
  }

  // writeOutcome is the live attachment's last terminal write outcome
  // (failed/uncertain/interrupted), so the close path can render the
  // server's specific reason.
  writeOutcome(): { code: string; message: string } | null {
    return this.attachment?.writeOutcome() ?? null;
  }

  dispose(): void {
    if (this.disposed) return;
    this.disposed = true;
    this.observer?.disconnect();
    this.scrollSubscription?.dispose();
    this.scrollSubscription = null;
    this.scrollbackSubscription?.dispose();
    this.scrollbackSubscription = null;
    this.disconnect();
    this.mounted.dispose();
  }

  // Closing the socket replaced by reconnect/takeover is asynchronous. Its
  // late onClose must not make the new attachment look disconnected, so every
  // callback is fenced to the generation that created it.
  private scopedCallbacks(generation: number): AttachmentCallbacks {
    const current = () => generation === this.attachmentGeneration;
    const attachmentEnded = () => {
      if (!current()) return false;
      this.mounted.cancelCtrlChord();
      return true;
    };
    return {
      onTakeoverRequired: (message) => {
        if (attachmentEnded()) this.callbacks.onTakeoverRequired?.(message);
      },
      onLeaseExpired: (message) => {
        if (attachmentEnded()) this.callbacks.onLeaseExpired?.(message);
      },
      onFenced: (message) => {
        if (attachmentEnded()) this.callbacks.onFenced?.(message);
      },
      onDropped: (message) => {
        if (attachmentEnded()) this.callbacks.onDropped?.(message);
      },
      onError: (message) => {
        if (attachmentEnded()) this.callbacks.onError?.(message);
      },
      onAttached: () => {
        if (current()) {
          // The handshake already established this geometry. Remember it so
          // a same-size observer delivery cannot duplicate the PTY resize.
          this.lastReportedGeometry = this.geometry();
          this.callbacks.onAttached?.();
        }
      },
      onWriteStatus: (code, message) => {
        if (current()) this.callbacks.onWriteStatus?.(code, message);
      },
      onScrollStatus: (active) => {
        if (current()) this.callbacks.onScrollbackChange?.(active);
      },
      onClose: () => {
        if (attachmentEnded()) this.callbacks.onClose?.();
      },
    };
  }

  private startPendingAttach(): void {
    const pending = this.pendingAttach;
    if (pending === null || this.disposed) return;
    this.pendingAttach = null;

    const protocol = location.protocol === "https:" ? "wss:" : "ws:";
    const term = this.mounted.terminal;
    const params = new URLSearchParams({
      controller: this.controllerID,
      cols: String(term.cols),
      rows: String(term.rows),
    });
    if (pending.takeover) params.set("takeover", "1");
    const url = `${protocol}//${location.host}${this.terminalPath(pending.sessionID)}?${params}`;

    const initialSize = { cols: term.cols, rows: term.rows };
    const transport = this.makeTransport(url);
    const callbacks = this.scopedCallbacks(pending.generation);
    try {
      this.attachment = new AgentAttachment(
        transport,
        {
          write: (data) => {
            term.write(data);
            // The output signal: bytes reached the emulator.
            // Fenced to the generation like every other callback.
            if (pending.generation === this.attachmentGeneration) this.callbacks.onOutput?.();
          },
          onOutbound: (fn) =>
            this.mounted.onOutbound((event) => {
              // User input exits the copy-mode view server-side; the
              // chrome hears about it here. Replies (mouse
              // reports, emulator answers) are not user input.
              if (event.kind === "input" &&
                pending.generation === this.attachmentGeneration) {
                this.callbacks.onInput?.();
              }
              fn(event);
            }),
          get cols() {
            return term.cols;
          },
          get rows() {
            return term.rows;
          },
        },
        callbacks,
        initialSize,
      );
    } catch (cause) {
      this.attachment = null;
      transport.close();
      const reason = cause instanceof Error ? cause.message : String(cause);
      callbacks.onError?.(`secure terminal input routing unavailable: ${reason}`);
    }
  }

  private resizeFromObserver(): void {
    if (this.disposed) return;
    const size = this.containerSize();
    if (
      size !== null &&
      this.lastFitSize !== null &&
      size.width === this.lastFitSize.width &&
      size.height === this.lastFitSize.height
    ) return;
    this.fitAndResize();
  }

  private fitAndResize(): void {
    this.mounted.fit();
    this.lastFitSize = this.containerSize();
    const geometry = this.geometry();
    if (geometry === this.lastReportedGeometry) return;
    this.attachment?.resize();
    this.lastReportedGeometry = geometry;
  }

  private geometry(): string {
    recordTerminalGeometry(this.mounted.terminal.cols, this.mounted.terminal.rows);
    return `${this.mounted.terminal.cols}x${this.mounted.terminal.rows}`;
  }

  private containerSize(): ContainerSize | null {
    if (typeof this.container.getBoundingClientRect !== "function") return null;
    const { width, height } = this.container.getBoundingClientRect();
    if (!Number.isFinite(width) || !Number.isFinite(height)) return null;
    return { width, height };
  }
}
