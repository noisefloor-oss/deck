import { csrfToken, fetchWithAuthorization } from "../api/http.ts";
import { activateDialog } from "../ui/dialog-focus.ts";
import { openModalScreen, type ModalScreen } from "../ui/modal-host.ts";
import { dialogHead, PRIMARY_MD_BUTTON, SECONDARY_MD_BUTTON, GHOST_MD_BUTTON } from "../ui/controls.ts";

export interface HarnessLoginAttempt { id: string; flow: string; expires: string }
export interface HarnessLoginListing { pending: HarnessLoginAttempt[]; csrf?: string; unavailable?: boolean }

export interface HarnessLoginOptions {
  initialCodex?: boolean;
  terminalHost?: HTMLElement;
  focusTerminal?: () => void;
  activateTerminalKey?: (key: "ArrowUp" | "ArrowDown" | "Enter") => boolean;
  onCompleted?: () => void;
  // Reuse the coordinator’s current listing without another request.
  listing?: HarnessLoginListing;
  onAuthorizationLost?: () => void;
  // Fired once after the dialog has left the DOM, whatever closed it.
  onClosed?: (attempts: string[], reason: "dismissed" | "terminal" | "context-lost") => void;
}

const SHARED_INTRO = "Start the harness’s normal login in this session and authenticate on the provider’s website. If your browser cannot open its localhost callback, finish here.";

interface FlowCopy {
  agent: string;
  name: string;
  title: string;
  intro: string;
  fieldLabel: string;
  submitLabel: string;
}

// The native attempt chooses its callback port; the UI must not prescribe one.
const FLOWS: Record<string, FlowCopy> = {
  "codex-browser": {
    agent: "Codex",
    name: "Codex · ChatGPT",
    title: "Codex login helper",
    intro: "1. Sign in with OpenAI",
    fieldLabel: "2. Paste the return URL",
    submitLabel: "Finish sign-in",
  },
  "pi-openai": {
    agent: "Pi",
    name: "Pi · ChatGPT",
    title: "Finish Pi login",
    intro: SHARED_INTRO,
    fieldLabel: "Localhost callback URL",
    submitLabel: "Deliver callback",
  },
  "pi-anthropic": {
    agent: "Pi",
    name: "Pi · Anthropic",
    title: "Finish Pi login",
    intro: SHARED_INTRO,
    fieldLabel: "Localhost callback URL",
    submitLabel: "Deliver callback",
  },
};

// A listing may carry flows this build cannot hand off; the coordinator
// and the dialog agree on which ones count through this one check.
export function isSupportedHarnessLoginFlow(flow: string): boolean {
  return Object.hasOwn(FLOWS, flow);
}

// Names the agent when every waiting attempt belongs to the same one.
export function harnessLoginLabel(attempts: readonly HarnessLoginAttempt[]): string {
  const agents = new Set(attempts.map(attempt => FLOWS[attempt.flow]?.agent));
  const [agent] = agents;
  return agents.size === 1 && agent !== undefined ? `Finish ${agent} login` : "Finish agent login";
}

export const harnessLoginMessages = {
  uncertain: "Delivery could not be confirmed. Check the native CLI before starting another login. This handoff will not be retried.",
  expired: "This login attempt expired. Start the sign-in again in the terminal.",
  invalid: "That URL doesn’t match this sign-in. Copy the full localhost URL from your browser and try again.",
  unavailable: "Callback handoff is unavailable in this Deck instance. Continue with the harness’s native login options.",
  submitting: "Delivering callback…",
  completed: "Native login completed. Return to the terminal.",
  delivered: "Callback delivered. Check the native CLI for the authentication result.",
};

export function harnessLoginHtml(): string {
  return `<div data-harness-login-dialog role="dialog" aria-modal="true" aria-label="Finish agent login" class="dialog">
      <span class="login-sheet-handle" aria-hidden="true"></span>
      ${dialogHead("Finish agent login", { label: "Close", attr: "data-login-close" }, "For the selected session")}
      <div class="dialog-body">
        <p data-login-intro class="text-body-sm text-text-secondary">${SHARED_INTRO}</p>
        <button type="button" data-login-open hidden class="${SECONDARY_MD_BUTTON}">Open OpenAI sign-in ↗</button>
        <p data-login-warning hidden class="text-body-sm text-text-secondary">A localhost error after sign-in is expected: Codex runs on Deck. Copy that tab’s full address below.</p>
        <div data-login-attempts class="flex flex-col gap-2"></div>
        <button type="button" data-login-refresh class="menuitem">Check for a waiting login</button>
        <form id="harness-login-form" data-login-form hidden autocomplete="off">
          <label class="block text-body-sm"><span data-login-field-label>Localhost callback URL</span>
            <input data-login-callback aria-describedby="harness-login-status" type="text" inputmode="url" autocomplete="off" autocapitalize="none" spellcheck="false" maxlength="8192" required
              class="block w-full rounded-control border border-border bg-surface px-3 py-2 text-text-primary">
          </label>
          <p data-login-privacy>Paste only here, never into the terminal.</p>
        </form>
        <p role="status" id="harness-login-status" data-login-status class="text-body-sm text-text-secondary">Checking for a waiting login…</p>
        <div data-login-terminal hidden></div>
      </div>
      <footer class="dialog-foot">
        <button type="button" data-login-cancel hidden class="${GHOST_MD_BUTTON}">Cancel login</button>
        <button type="button" data-login-later class="${SECONDARY_MD_BUTTON}">Not now</button>
        <button type="submit" form="harness-login-form" data-login-submit class="${PRIMARY_MD_BUTTON}">Deliver callback</button>
      </footer>
    </div>`;
}

export function applyHarnessLoginFlow(root: HTMLElement, flowName: string): void {
  const flow = FLOWS[flowName];
  if (flow === undefined) return;
  const dialog = root.querySelector<HTMLElement>("[data-harness-login-dialog]")!;
  dialog.setAttribute("aria-label", flow.title);
  dialog.dataset.loginFlow = flowName;
  root.querySelector(".dialog-title")!.textContent = flow.title;
  const description = root.querySelector<HTMLElement>(".dialog-desc")!;
  description.hidden = flowName === "codex-browser";
  description.textContent = flowName === "codex-browser" ? "" : "For the selected session";
  root.querySelector("[data-login-intro]")!.textContent = flow.intro;
  root.querySelector("[data-login-field-label]")!.textContent = flow.fieldLabel;
  root.querySelector("[data-login-submit]")!.textContent = flow.submitLabel;
  root.querySelector<HTMLElement>("[data-login-warning]")!.hidden = flowName !== "codex-browser";
  root.querySelector<HTMLButtonElement>("[data-login-open]")!.hidden = flowName !== "codex-browser";
  root.querySelector("[data-login-later]")!.textContent = flowName === "codex-browser" ? "Use terminal instead" : "Not now";
  const refresh = root.querySelector<HTMLButtonElement>("[data-login-refresh]")!;
  if (flowName === "codex-browser") {
    refresh.className = SECONDARY_MD_BUTTON;
    refresh.textContent = "Check for login";
    root.querySelector("[data-login-later]")!.before(refresh);
  } else {
    refresh.className = "menuitem";
    refresh.textContent = "Check for a waiting login";
    root.querySelector("form")!.before(refresh);
  }
  root.querySelector("[data-login-privacy]")!.textContent = flowName === "codex-browser"
    ? "Paste here, not in the terminal."
    : "Paste only here, never into the terminal.";
  root.querySelector<HTMLInputElement>("[data-login-callback]")!.placeholder = flowName === "codex-browser" ? "http://localhost:…" : "";
}

export function applyHarnessLoginTerminal(root: HTMLElement, completedCodex = false): void {
  root.querySelector<HTMLElement>("[data-login-intro]")!.hidden = true;
  root.querySelector<HTMLFormElement>("form")!.hidden = true;
  root.querySelector("[data-login-attempts]")!.replaceChildren();
  for (const name of ["refresh", "cancel", "submit", "open"]) {
    root.querySelector<HTMLButtonElement>(`[data-login-${name}]`)!.hidden = true;
  }
  root.querySelector("[data-login-later]")!.textContent = completedCodex ? "Continue to Codex" : "Close";
}

// Callback bytes stay in memory and a single POST. In particular,
// failures never go through the application's general error/logging path.
export class HarnessLoginDialog {
  private root: HTMLElement;
  private screen: ModalScreen;
  private releaseFocus: () => void;
  private opener: HTMLElement | null;
  private session: string;
  private onClosed?: HarnessLoginOptions["onClosed"];
  private onAuthorizationLost?: () => void;
  private csrf = "";
  private handle = "";
  private selected: HarnessLoginAttempt | null = null;
  private expires = 0;
  private closed = false;
  private busy = false;
  private submitting = false;
  private clearCallback?: () => void;
  private attempts: string[] = [];
  private terminal = false;
  private expiryTimer: ReturnType<typeof setTimeout> | undefined;
  private waitingTimer: ReturnType<typeof setTimeout> | undefined;
  private refreshRequest: AbortController | null = null;
  private refreshAgain = false;
  private initialCodex: boolean;
  private restoreTerminal?: () => void;
  private openingTab: Window | null = null;
  private opening = false;
  private onCompleted?: () => void;

  constructor(host: HTMLElement, session: string, options: HarnessLoginOptions = {}) {
    this.session = session;
    this.initialCodex = options.initialCodex === true;
    this.onCompleted = options.onCompleted;
    this.onAuthorizationLost = options.onAuthorizationLost;
    this.onClosed = options.onClosed;
    this.opener = document.activeElement instanceof HTMLElement ? document.activeElement : null;
    this.screen = openModalScreen(host, { dismiss: () => this.close() });
    this.root = this.screen.element;
    this.root.innerHTML = harnessLoginHtml();
    const dialog = this.root.querySelector<HTMLElement>("[role=dialog]")!;
    if (this.initialCodex) {
      applyHarnessLoginFlow(this.root, "codex-browser");
      this.root.querySelector<HTMLElement>("[data-login-open]")!.hidden = true;
      this.root.querySelector<HTMLElement>("[data-login-warning]")!.hidden = true;
      this.root.querySelector("[data-login-intro]")!.textContent = options.terminalHost
        ? "Choose a sign-in option in Codex below."
        : "Choose a sign-in option in Codex.";
      this.status("");
    }
    const terminalHost = options.terminalHost;
    if (terminalHost?.parentNode) {
      const placeholder = document.createComment("Codex terminal position");
      terminalHost.before(placeholder);
      const slot = this.root.querySelector<HTMLElement>("[data-login-terminal]")!;
      slot.hidden = false;
      slot.dataset.mono = "workspace";
      slot.appendChild(terminalHost);
      dialog.dataset.codexHelper = "true";
      const nativeKeys = (event: KeyboardEvent): void => event.stopPropagation();
      terminalHost.addEventListener("keydown", nativeKeys);
      // Stop wheel gestures before xterm scrolls or sends native mouse reports.
      const blockWheel = (event: WheelEvent): void => {
        event.preventDefault();
        event.stopImmediatePropagation();
      };
      terminalHost.addEventListener("wheel", blockWheel, { capture: true, passive: false });
      this.restoreTerminal = () => {
        terminalHost.removeEventListener("keydown", nativeKeys);
        terminalHost.removeEventListener("wheel", blockWheel, { capture: true });
        placeholder.replaceWith(terminalHost);
      };
    }
    this.releaseFocus = activateDialog(dialog, () => this.close(), ":dialog");
    if (terminalHost) options.focusTerminal?.();
    const staticTarget = (target: EventTarget | null): boolean => target instanceof Element
      && dialog.contains(target) && !terminalHost?.contains(target)
      && target.closest('button, input, textarea, select, label, a[href], [contenteditable]:not([contenteditable="false"]), [role="button"], [role="link"], [role="textbox"], [tabindex]:not([tabindex="-1"])') === null;
    dialog.addEventListener("click", (event) => {
      if (!this.closed && terminalHost && staticTarget(event.target) && window.getSelection()?.isCollapsed !== false) options.focusTerminal?.();
    });
    dialog.addEventListener("keydown", (event) => {
      if (this.closed || !terminalHost || event.defaultPrevented || event.isComposing
        || event.altKey || event.ctrlKey || event.metaKey || event.shiftKey || !staticTarget(event.target)) return;
      if (event.key !== "ArrowUp" && event.key !== "ArrowDown" && event.key !== "Enter") return;
      if (options.activateTerminalKey?.(event.key)) {
        event.preventDefault();
        event.stopPropagation();
        options.focusTerminal?.();
      }
    });
    this.root.querySelector("[data-login-close]")!.addEventListener("click", () => this.close());
    this.root.querySelector("[data-login-later]")!.addEventListener("click", () => this.close());
    this.root.querySelector("[data-login-cancel]")!.addEventListener("click", () => void this.cancelLogin());
    this.root.querySelector("[data-login-refresh]")!.addEventListener("click", () => void this.refresh());
    this.root.querySelector("[data-login-open]")!.addEventListener("click", () => void this.openAuthorization());
    this.root.querySelector("form")!.addEventListener("submit", (event) => {
      event.preventDefault();
      void this.submit();
    });
    this.root.querySelector("input")!.addEventListener("input", () => {
      this.root.querySelector("input")!.removeAttribute("aria-invalid");
      this.updateSubmit();
    });
    this.updateSubmit();
    if (options.listing === undefined) void this.refresh();
    else this.renderListing(options.listing);
  }

  private status(message: string): void {
    if (!this.closed) this.root.querySelector("[data-login-status]")!.textContent = message;
  }

  private updateSubmit(): void {
    this.root.querySelector<HTMLButtonElement>("[data-login-submit]")!.disabled = this.busy || this.root.querySelector<HTMLInputElement>("input")!.value.trim() === "";
    this.root.querySelector<HTMLButtonElement>("[data-login-open]")!.disabled = this.busy || this.opening;
  }

  notifyPending(): void {
    if (this.closed || this.terminal || this.selected !== null) return;
    if (this.busy) this.refreshAgain = true;
    else void this.refresh();
  }

  private async openAuthorization(): Promise<void> {
    const attempt = this.selected;
    if (this.closed || this.busy || this.opening || attempt?.flow !== "codex-browser") return;
    const popup = window.open("about:blank", "_blank");
    if (popup === null) {
      this.status("Allow pop-ups for Deck, then try Open OpenAI sign-in again.");
      return;
    }
    popup.opener = null;
    const policy = popup.document.createElement("meta");
    policy.name = "referrer";
    policy.content = "no-referrer";
    popup.document.head.appendChild(policy);
    popup.document.title = "Opening OpenAI sign-in";
    this.openingTab = popup;
    this.opening = true;
    this.updateSubmit();
    try {
      const response = await this.post({ action: "authorization", session: this.session, registration: attempt.id, handle: this.handle }, AbortSignal.timeout(8000));
      if (!response.ok) throw new Error("unavailable");
      const result = await response.json() as { url?: unknown };
      if (typeof result.url !== "string") throw new Error("invalid");
      const url = new URL(result.url);
      if (url.protocol !== "https:" || url.host !== "auth.openai.com" || url.pathname !== "/oauth/authorize" || url.username !== "" || url.password !== "" || url.hash !== "") throw new Error("invalid");
      if (this.closed || this.terminal || this.selected !== attempt || popup.closed) {
        popup.close();
        return;
      }
      const link = popup.document.createElement("a");
      link.href = result.url;
      link.referrerPolicy = "no-referrer";
      link.rel = "noreferrer noopener";
      popup.document.body.appendChild(link);
      link.click();
      link.remove();
      this.openingTab = null;
      this.status("After signing in, return here with the full localhost address from your browser.");
    } catch {
      popup.close();
      this.status("Could not open this sign-in. Check the Codex terminal and try again.");
    } finally {
      this.openingTab = null;
      this.opening = false;
      if (!this.closed) this.updateSubmit();
    }
  }

  private expire(): void {
    if (this.closed || this.submitting || this.selected === null) return;
    this.finish(harnessLoginMessages.expired);
  }

  private finish(message: string): void {
    const completedCodex = this.selected?.flow === "codex-browser" && message === harnessLoginMessages.completed;
    this.handle = "";
    this.selected = null;
    this.terminal = true;
    clearTimeout(this.expiryTimer);
    clearTimeout(this.waitingTimer);
    this.refreshRequest?.abort();
    this.openingTab?.close();
    if (this.closed) return;
    this.root.querySelector<HTMLInputElement>("input")!.value = "";
    applyHarnessLoginTerminal(this.root, completedCodex);
    this.status(message);
  }

  private headers(): Headers {
    const headers = new Headers({ "Content-Type": "application/json", "X-Harness-Login-CSRF": this.csrf });
    const token = csrfToken(document.cookie);
    if (token !== null) headers.set("X-CSRF-Token", token);
    return headers;
  }

  private post(body: Record<string, string>, signal?: AbortSignal): Promise<Response> {
    return fetchWithAuthorization("/api/harness-login", {
      method: "POST", credentials: "same-origin", cache: "no-store", redirect: "error", referrerPolicy: "no-referrer",
      headers: this.headers(), body: JSON.stringify(body), signal,
    }, this.onAuthorizationLost);
  }

  // Dismissal releases a claimed attempt back to the same user without
  // ending it, so "not now" leaves the login pickable again. A handle whose
  // declared expiry has passed is already dead server-side; the POST would
  // only be refused.
  private dismiss(handle: string, expires: number): void {
    if (handle === "" || (expires !== 0 && Date.now() >= expires)) return;
    void this.post({ action: "dismiss", handle }, AbortSignal.timeout(2000)).catch(() => {});
  }

  private renderListing(result: HarnessLoginListing): void {
    if (this.closed) return;
    this.csrf = result.csrf ?? "";
    const list = this.root.querySelector<HTMLElement>("[data-login-attempts]")!;
    list.replaceChildren();
    const attempts = result.pending.filter((attempt) => FLOWS[attempt.flow] !== undefined);
    this.attempts = attempts.map(attempt => attempt.id);
    if (result.unavailable) {
      this.finish(harnessLoginMessages.unavailable);
      return;
    }
    if (attempts.length === 0) {
      this.status(this.initialCodex ? "" : "No supported callback is waiting in this session. Handoff supports reviewed Codex browser login and Pi ChatGPT/Anthropic login. Other flows use the harness’s native login options. A new session may be needed after upgrading Deck.");
      return;
    }
    // Showing a form must not claim the attempt while the user is away
    // authenticating: a discarded page cannot release its handle.
    if (attempts.length === 1) {
      this.select(attempts[0]!);
      return;
    }
    const label = harnessLoginLabel(attempts);
    this.root.querySelector("[data-harness-login-dialog]")!.setAttribute("aria-label", label);
    this.root.querySelector(".dialog-title")!.textContent = label;
    this.status("Choose the login you started. Each handoff expires after five minutes and can be submitted once.");
    for (const attempt of attempts) {
      const button = document.createElement("button");
      button.type = "button";
      button.className = "menuitem";
      button.textContent = `Finish ${FLOWS[attempt.flow]!.name} login`;
      button.addEventListener("click", () => this.select(attempt));
      list.appendChild(button);
    }
  }

  private async refresh(): Promise<void> {
    if (this.closed || this.busy || this.handle !== "" || this.terminal || this.selected !== null) return;
    clearTimeout(this.waitingTimer);
    this.busy = true;
    const request = new AbortController();
    this.refreshRequest = request;
    const timeout = setTimeout(() => request.abort(), 8000);
    let result: HarnessLoginListing | null = null;
    try {
      const response = await fetchWithAuthorization(`/api/harness-login?session=${encodeURIComponent(this.session)}`, {
        credentials: "same-origin", cache: "no-store", redirect: "error", referrerPolicy: "no-referrer", signal: request.signal,
      }, this.onAuthorizationLost);
      if (!response.ok) throw new Error("unavailable");
      result = await response.json() as HarnessLoginListing;
    } catch {
      this.status("Could not check for a login. Sign in to Deck and try again.");
      return;
    } finally {
      clearTimeout(timeout);
      this.refreshRequest = null;
      this.busy = false;
      if (!this.closed && !this.terminal && this.selected === null && this.initialCodex) {
        this.waitingTimer = setTimeout(() => this.notifyPending(), this.refreshAgain ? 0 : 1000);
      }
      this.refreshAgain = false;
    }
    if (this.closed || result === null) return;
    this.renderListing(result);
  }

  private select(attempt: HarnessLoginAttempt): void {
    if (this.closed || this.busy || this.terminal) return;
    this.selected = attempt;
    clearTimeout(this.waitingTimer);
    this.attempts = [attempt.id];
    const expires = Date.parse(attempt.expires);
    this.expires = Number.isNaN(expires) ? 0 : expires;
    clearTimeout(this.expiryTimer);
    if (this.expires !== 0) this.expiryTimer = setTimeout(() => this.expire(), Math.max(0, this.expires - Date.now()));
    this.root.querySelector("[data-login-attempts]")!.replaceChildren();
    this.root.querySelector<HTMLButtonElement>("[data-login-refresh]")!.hidden = true;
    applyHarnessLoginFlow(this.root, attempt.flow);
    this.root.querySelector<HTMLButtonElement>("[data-login-cancel]")!.hidden = attempt.flow === "codex-browser";
    this.root.querySelector<HTMLFormElement>("form")!.hidden = false;
    this.status("");
    this.root.querySelector<HTMLInputElement>("input")!.focus();
  }

  private async begin(): Promise<boolean> {
    if (this.handle !== "") return true;
    const attempt = this.selected;
    if (this.closed || this.terminal || attempt === null) return false;
    try {
      const response = await this.post({ action: "begin", session: this.session, registration: attempt.id });
      if (!response.ok) throw new Error("unavailable");
      const result = await response.json() as { id: string; expires?: string };
      const declared = Date.parse(result.expires ?? attempt.expires);
      // A begin that lands after the dialog closed (or after its context
      // was torn down) must not strand the claim it created: release the
      // attempt back to the user and forget it.
      if (this.closed || this.terminal || (!Number.isNaN(declared) && Date.now() >= declared)) {
        this.dismiss(result.id, Number.isNaN(declared) ? 0 : declared);
        if (!this.closed) this.finish(harnessLoginMessages.expired);
        return false;
      }
      this.handle = result.id;
      this.expires = Number.isNaN(declared) ? 0 : declared;
      clearTimeout(this.expiryTimer);
      if (this.expires !== 0) this.expiryTimer = setTimeout(() => this.expire(), Math.max(0, this.expires - Date.now()));
      return true;
    } catch {
      this.finish("This attempt is no longer available. Check the native CLI before starting another login.");
      return false;
    }
  }

  // Cancel login is the only path that ends the attempt outright. Every
  // other close gesture dismisses, leaving it reclaimable.
  private async cancelLogin(): Promise<void> {
    if (this.closed || this.busy || this.submitting || this.selected === null) return;
    this.busy = true;
    this.root.querySelector<HTMLInputElement>("input")!.value = "";
    this.root.querySelector<HTMLFormElement>("form")!.hidden = true;
    try {
      if (!await this.begin()) return;
      const handle = this.handle;
      this.handle = "";
      this.terminal = true;
      const response = await this.post({ action: "cancel", handle }, AbortSignal.timeout(2000));
      if (!response.ok) throw new Error("unconfirmed");
      this.close();
    } catch {
      this.finish("Cancellation could not be confirmed. Check the native CLI before starting another login.");
    } finally { this.busy = false; }
  }

  private async submit(): Promise<void> {
    if (this.closed || this.busy || this.selected === null) return;
    this.busy = true;
    this.submitting = true;
    const input = this.root.querySelector<HTMLInputElement>("input")!;
    const form = this.root.querySelector<HTMLFormElement>("form")!;
    let callback = input.value;
    this.clearCallback = () => { callback = ""; };
    input.value = "";
    form.hidden = true;
    this.status(harnessLoginMessages.submitting);
    try {
      if (!await this.begin()) return;
      const handle = this.handle;
      const pending = this.post({ action: "submit", handle, callback });
      callback = "";
      const response = await pending;
      if (this.closed) {
        // The dialog is gone and the delivery is never replayed. The
        // attempt was only left untouched when the URL was rejected.
        if (response.status === 400) this.dismiss(handle, this.expires);
        this.handle = "";
        return;
      }
      if (response.status === 400) {
        if (this.expires !== 0 && Date.now() >= this.expires) {
          this.finish(harnessLoginMessages.expired);
          return;
        }
        form.hidden = false;
        input.setAttribute("aria-invalid", "true");
        this.status(harnessLoginMessages.invalid);
        input.focus();
        return;
      }
      if (!response.ok) {
        this.finish(this.expires !== 0 && Date.now() >= this.expires ? harnessLoginMessages.expired : harnessLoginMessages.uncertain);
        return;
      }
      const result = await response.json() as { status: string };
      if (result.status !== "completed" && result.status !== "delivered") {
        this.finish(harnessLoginMessages.uncertain);
        return;
      }
      this.finish(result.status === "completed"
        ? harnessLoginMessages.completed
        : harnessLoginMessages.delivered);
      if (result.status === "completed") this.onCompleted?.();
    } catch {
      this.finish(harnessLoginMessages.uncertain);
    } finally {
      callback = "";
      this.clearCallback = undefined;
      this.submitting = false;
      this.busy = false;
      if (!this.closed) this.updateSubmit();
    }
  }

  close(reason: "dismissed" | "context-lost" = "dismissed"): void {
    if (this.closed) return;
    this.closed = true;
    clearTimeout(this.expiryTimer);
    clearTimeout(this.waitingTimer);
    this.refreshRequest?.abort();
    this.openingTab?.close();
    this.clearCallback?.();
    this.root.querySelector<HTMLInputElement>("input")!.value = "";
    // Keep late Begin responses observable so their handles can be released.
    const handle = this.handle;
    this.handle = "";
    if (!this.submitting) this.dismiss(handle, this.expires);
    this.releaseFocus();
    this.restoreTerminal?.();
    this.restoreTerminal = undefined;
    this.screen.close();
    if (reason !== "context-lost" && this.opener?.isConnected) this.opener.focus({ preventScroll: true });
    this.onClosed?.(this.attempts, this.terminal || this.submitting ? "terminal" : reason);
  }
}
