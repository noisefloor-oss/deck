import type { HttpApiClient } from "../../api/client.ts";
import { SecurityClient, SecurityError, mayOpenWorkspace, type MethodRef, type OnboardingState, type SecurityAction, type SecurityMethod, type SecurityState } from "../../security/client.ts";
import { clearCredential, IncompatiblePasskey, MethodUnlockError, prepareProof, registerPasskey, saveCandidate, type PreparedProof } from "../../security/ceremony.ts";
import { generateWorkspaceSecret, workspaceCommitment, type OpenedMethod, type UnlockCredential } from "../../security/envelope.ts";
import { encodeBase64url } from "../../security/encoding.ts";
import { formatRecoveryKey, generateRecoverySecret, parseRecoveryKey } from "../../security/recovery.ts";
import { downloadRecoveryKit, importRecoveryKit, printRecoveryKit, recoveryKit } from "../../security/kit.ts";
import { bindEnrollmentActions, type EnrollmentAction } from "./enrollment-actions.ts";
import { enrollmentHtml, type EnrollmentScreen, type EnrollmentView, type EnrollmentWorkspace } from "./enrollment.ts";
import { bindRuntimeActions, runtimeHtml, type RuntimeAction, type RuntimeScreen, type RuntimeView } from "./runtime.ts";
import { onboardingHtml, type OnboardingScreen, type OnboardingView, type OnboardingProgressRow } from "./onboarding.ts";
import { bindOnboardingActions, type OnboardingAction } from "./onboarding-actions.ts";

export interface SecurityWorkspaceHost {
  admit(): Promise<void>;
  detach(): void;
  manageMethods?(context: MethodManagementContext): Promise<() => void>;
}
export interface MethodManagementContext {
  root: HTMLElement;
  api: SecurityClient;
  state: SecurityState;
  recoveryMethod?: MethodRef;
  onClose(): Promise<void>;
  authorizationLost(): void;
}
type Page = { kind: "enrollment"; view: EnrollmentView } | { kind: "runtime"; view: RuntimeView } | { kind: "onboarding"; view: OnboardingView };
type NativeOperation = "initialize" | "unlock" | "lock";
interface PendingOperation { instanceId: string; id: string; operation: NativeOperation }
const operationStorage = "deck.security.pending-operation.v1";
const ref = (method: SecurityMethod) => ({ id: method.id, revision: method.revision });

// Owns only the browser's views and volatile key lifetimes. Gateway state and
// native observations decide whether any private workspace may be constructed.
export class WorkspaceSecurity {
  private readonly lifetime = new AbortController();
  private ceremony = new AbortController();
  private unbind?: () => void;
  private page?: Page;
  private state?: SecurityState;
  private onboarding?: OnboardingState;
  private authenticated = true;
  private setupContinued = false;
  private setupWasIncomplete = false;
  private pendingPlain?: string;
  private failedPlain?: string;
  private methods: SecurityMethod[] = [];
  private selectedMethod?: string;
  private recoveryAdmission?: MethodRef;
  private everyday?: SecurityMethod;
  private recovery?: SecurityMethod;
  private source?: OpenedMethod;
  private temporary = new Set<() => void>();
  private key?: string;
  private clearPrint?: () => void;
  private pending?: PendingOperation;
  private busy = false;
  private admitted = false;
  private disposed = false;
  // Google sign-in loads a new document; late responses cannot revive this one.
  private authorizationExpired = false;
  private poll?: ReturnType<typeof setTimeout>;
  private invitation?: string;
  private downloadRequested = false;
  private disposeManagement?: () => void;
  private refreshGeneration = 0;
  private refreshingAuthorization = false;
  private workspace: EnrollmentWorkspace;

  private readonly root: HTMLElement;
  private readonly session: HttpApiClient;
  private readonly host: SecurityWorkspaceHost;
  readonly api: SecurityClient;
  constructor(root: HTMLElement, session: HttpApiClient, host: SecurityWorkspaceHost, api = new SecurityClient()) {
    this.root = root; this.session = session; this.host = host; this.api = api;
    this.api.onAuthorizationLost = () => this.signedOut();
    this.workspace = { name: location.hostname, address: location.host };
    const fragment = new URLSearchParams(location.hash.slice(1));
    const token = fragment.get("invite") ?? fragment.get("invitation") ?? fragment.get("setup");
    if (token !== null) {
      fragment.delete("invite"); fragment.delete("invitation"); fragment.delete("setup");
      history.replaceState(null, "", `${location.pathname}${location.search}${fragment.size ? `#${fragment}` : ""}`);
      if (/^[A-Za-z0-9_-]{43}$/.test(token)) this.invitation = token;
    }
    window.addEventListener("focus", () => { if (this.admitted) void this.checkAdmission(); }, { signal: this.lifetime.signal });
    document.addEventListener("visibilitychange", () => { if (this.admitted && !document.hidden) void this.checkAdmission(); }, { signal: this.lifetime.signal });
  }

  async start(authenticated: boolean): Promise<void> {
    this.authenticated = authenticated;
    await this.refresh();
  }

  private enrollmentOrLogin(): void {
    const setup = this.onboarding;
    if (!setup) { this.showOnboarding({ kind: "unreachable" }); return; }
    if (setup.setupComplete) this.showOnboarding({ kind: "signin", encrypted: setup.protection === "encrypted" });
    else if (setup.started) this.showOnboarding({ kind: "interrupted" });
    else if (this.invitation) this.showOnboarding({ kind: "welcome", policy: setup.policy });
    else this.showOnboarding({ kind: "needlink" });
  }

  private show(page: Page): void {
    const signIn = page.kind === "runtime" && page.view.screen.kind === "signed-out" || page.kind === "onboarding" && ["signin", "interrupted", "needlink"].includes(page.view.screen.kind);
    if (this.disposed || (this.authorizationExpired && !signIn)) return;
    this.closeManagement();
    if (this.admitted) { this.admitted = false; this.host.detach(); }
    this.unbind?.(); this.page = page;
    this.root.inert = false;
    if (page.kind === "enrollment") {
      this.root.innerHTML = enrollmentHtml(page.view);
      this.unbind = bindEnrollmentActions(this.root, { run: action => this.actEnrollment(action), error: message => this.error(message) });
    } else if (page.kind === "onboarding") {
      this.root.innerHTML = onboardingHtml(page.view);
      this.unbind = bindOnboardingActions(this.root, { run: action => this.actOnboarding(action), error: message => this.error(message) });
    } else {
      this.root.innerHTML = runtimeHtml(page.view);
      this.unbind = bindRuntimeActions(this.root, action => this.actRuntime(action), error => this.error(error instanceof Error ? error.message : "The request did not finish."));
    }
  }
  private showEnrollment(screen: EnrollmentScreen, error?: string, notice?: string): void {
    if (this.onboarding) {
      if (screen.kind === "complete") this.showOnboarding({ kind: "done", encryption: { kind: "encrypted", methodName: screen.methodName } }, error, notice);
      else this.showOnboarding({ kind: "enrollment", enrollment: screen }, error, notice);
      return;
    }
    this.show({ kind: "enrollment", view: { workspace: this.workspace, screen, busy: this.busy, error, notice, canManageMethods: this.host.manageMethods !== undefined } });
  }
  private showOnboarding(screen: OnboardingScreen, error?: string, notice?: string): void {
    this.show({ kind: "onboarding", view: { workspace: this.workspace, screen, busy: this.busy, error, notice } });
  }
  private showRuntime(screen: RuntimeScreen, error?: string, notice?: string): void {
    if (this.onboarding && screen.kind === "progress" && screen.operation === "initialize") {
      this.showOnboarding({ kind: "finishing", rows: [
        { label: "Unlock method saved", state: "done" }, { label: "Recovery key checked", state: "done" },
        { label: "Preparing workspace", state: "active" },
      ] }, error, notice); return;
    }
    if (this.onboarding && screen.kind === "unknown" && screen.operation === "initialize") {
      this.showOnboarding({ kind: "unconfirmed" }, error, notice); return;
    }
    this.show({ kind: "runtime", view: { name: this.workspace.name, address: this.workspace.address,
      screen, methods: [...new Set(this.methods.map(method => method.kind))], methodChoices: this.methods.map(({ id, kind }) => ({ id, kind })), selectedMethod: this.selectedMethod, busy: this.busy, error, notice } });
  }

  private showSetupResume(error?: string): void {
    const state = this.requiredState();
    const rows: OnboardingProgressRow[] = [{ label: "Google account connected", state: "done" }];
    if (state.onboarding.protection === "undecided") rows.push({ label: "Choose workspace protection", state: "active" });
    else if (state.onboarding.protection === "none") {
      rows.push({ label: "Deck encryption off", state: "done" }, { label: "Start workspace", state: "pending" });
    } else {
      rows.push({ label: "Encryption selected", state: "done" },
        { label: this.everyday ? "Unlock method saved" : "Choose an unlock method", state: this.everyday ? "done" : "active" });
      if (this.everyday) rows.push({ label: "Verify your saved method to continue", state: "active" });
    }
    this.showOnboarding({ kind: "resume", rows, requiresMethod: state.onboarding.protection === "encrypted" && this.everyday !== undefined }, error);
  }

  private async actOnboarding(action: OnboardingAction): Promise<void> {
    if (this.disposed || this.busy && action.kind !== "cancel-passkey") return;
    switch (action.kind) {
      case "google-sign-in": await this.googleSignIn(); return;
      case "switch-account": {
        // Before ownership, logout would discard the private setup handoff.
        // A new Google ceremony can replace the ineligible login directly.
        if (this.onboarding?.started !== false) await this.session.logout();
        if (this.disposed) return;
        this.authenticated = false; this.resetAuthorization(); await this.googleSignIn(true); return;
      }
      case "toggle-boundary": {
        if (this.page?.kind === "onboarding" && this.page.view.screen.kind === "encryption") {
          this.showOnboarding({ ...this.page.view.screen, boundaryOpen: !this.page.view.screen.boundaryOpen });
        }
        return;
      }
      case "enable-encryption": await this.chooseProtection("encrypted"); return;
      case "continue-unencrypted": await this.chooseProtection("none"); return;
      case "resume-setup":
        this.setupContinued = true;
        if (this.state?.onboarding.protection === "none") await this.startPlain();
        else await this.refresh();
        return;
      case "check-state": await this.refresh(); return;
      case "open-workspace": this.setupWasIncomplete = false; await this.refresh(); return;
      default: await this.actEnrollment(action);
    }
  }

  private async chooseProtection(protection: "encrypted" | "none"): Promise<void> {
    const state = this.requiredState();
    if (state.onboarding.protection !== "undecided" ||
        (protection === "none" && state.onboarding.policy === "required") ||
        (protection === "encrypted" && state.onboarding.policy === "unavailable")) throw new Error("Check the workspace's protection choice before continuing.");
    const signal = this.ceremony.signal;
    this.setBusy(true);
    try {
      const selected = await this.api.selectProtection(protection, signal);
      if (this.disposed || signal.aborted) return;
      this.onboarding = selected;
      const state = await this.api.state(signal);
      if (this.disposed || signal.aborted) return;
      this.state = state; this.onboarding = state.onboarding; this.setupContinued = true; this.busy = false;
      if (state.onboarding.protection === "none") await this.startPlain();
      else await this.refresh();
    } catch (error) {
      if (this.disposed || signal.aborted || this.authorizationExpired) return;
      this.busy = false;
      if (error instanceof SecurityError && error.status === 403) {
        this.showSetupResume("This browser no longer has setup authorization. Continue in the browser where setup started, or contact whoever runs this machine.");
      } else this.showOnboarding({ kind: "unconfirmed" });
    }
  }

  private showPlainProgress(): void {
    if (this.state?.onboarding.setupComplete) this.showRuntime({ kind: "plain-starting" });
    else this.showOnboarding({ kind: "finishing", encrypted: false, rows: [
      { label: "Google account connected", state: "done" }, { label: "Deck encryption off", state: "done" },
      { label: "Starting workspace", state: "active" },
    ] });
  }

  private showPlainUnknown(): void {
    if (this.state?.onboarding.setupComplete) this.showRuntime({ kind: "plain-unknown" });
    else this.showOnboarding({ kind: "unconfirmed" });
  }

  private async plainReady(): Promise<void> {
    if (!this.state || !mayOpenWorkspace(this.state) || this.state.onboarding.protection !== "none") return;
    this.pendingPlain = undefined;
    clearTimeout(this.poll);
    if (this.setupWasIncomplete) {
      const policy = this.state.onboarding.policy;
      if (policy === "required") throw new Error("Workspace protection state is inconsistent.");
      this.showOnboarding({ kind: "done", encryption: { kind: "unencrypted", policy } });
    } else await this.admit();
  }

  private async startPlain(): Promise<void> {
    const state = this.requiredState();
    if (state.onboarding.protection !== "none") return;
    const signal = this.ceremony.signal;
    this.setBusy(true); this.showPlainProgress();
    this.pendingPlain = state.plainOperationId;
    try {
      // After completed setup or a failed start, the service must observe
      // stopped and reserve a fresh ID. Otherwise setup keeps its reserved ID.
      const fresh = state.onboarding.setupComplete || this.failedPlain === state.plainOperationId;
      const result = await this.api.startPlain(fresh ? undefined : state.plainOperationId, signal);
      if (this.disposed || signal.aborted) return;
      this.pendingPlain = result.id; this.busy = false;
      await this.observePlain(result.id);
    } catch {
      if (this.disposed || signal.aborted || this.authorizationExpired) return;
      this.busy = false; this.showPlainUnknown();
    }
  }

  private async observePlain(id: string): Promise<void> {
    if (this.disposed || this.authorizationExpired) return;
    const generation = this.refreshGeneration;
    this.pendingPlain = id; this.busy = false;
    try {
      const observed = await this.api.observePlain(id, this.lifetime.signal);
      const state = await this.api.state(this.lifetime.signal);
      if (this.disposed || generation !== this.refreshGeneration || this.pendingPlain !== id) return;
      this.state = state; this.onboarding = state.onboarding;
      if (state.onboarding.protection !== "none" || state.plainOperationId !== id) { this.showPlainUnknown(); return; }
      if (mayOpenWorkspace(state)) { await this.plainReady(); return; }
      if (observed.status === "running" || state.runtime === "starting") {
        this.showPlainProgress();
        clearTimeout(this.poll); this.poll = setTimeout(() => { if (this.pendingPlain === id) void this.observePlain(id); }, 1000);
      } else if (state.runtime === "stopped" && observed.status === "pending" && !state.onboarding.setupComplete) this.showSetupResume();
      else if (state.runtime === "stopped" && observed.status === "failed" && !state.onboarding.setupComplete) {
        this.failedPlain = id; this.showSetupResume("The workspace did not start. Continue setup to start it again.");
      }
      else if (state.onboarding.setupComplete && state.runtime === "stopped" && ["succeeded", "failed"].includes(observed.status)) this.showRuntime({ kind: "plain-stopped" });
      else this.showPlainUnknown();
    } catch (error) {
      if (this.disposed || generation !== this.refreshGeneration || this.authorizationExpired) return;
      if (error instanceof SecurityError && error.status === 401) this.signedOut();
      else this.showPlainUnknown();
    }
  }
  private error(message: string): void {
    if (!this.page || this.disposed || this.authorizationExpired) return;
    this.busy = false;
    this.show({ ...this.page, view: { ...this.page.view, busy: false, error: message } } as Page);
  }
  private notice(message: string): void {
    if (!this.authorizationExpired && this.page) this.show({ ...this.page, view: { ...this.page.view, notice: message } } as Page);
  }
  private setBusy(value: boolean): void {
    if (this.authorizationExpired) return;
    this.busy = value;
    if (this.page) this.show({ ...this.page, view: { ...this.page.view, busy: value, error: undefined, notice: undefined } } as Page);
  }

  async refresh(): Promise<void> {
    if (this.disposed || this.authorizationExpired) return;
    const generation = ++this.refreshGeneration;
    this.closeManagement();
    try {
      const setup = await this.api.onboarding(this.lifetime.signal);
      if (this.disposed || generation !== this.refreshGeneration) return;
      this.onboarding = setup;
      if (!this.authenticated) { this.enrollmentOrLogin(); return; }
      if (this.invitation && !setup.started) {
        // An expired Google session clears the link from memory. Save its
        // private handoff before reading authenticated setup state.
        await this.api.handoff(this.invitation, this.lifetime.signal);
        if (this.disposed || generation !== this.refreshGeneration) return;
        this.invitation = undefined;
      }
      const state = await this.api.state(this.lifetime.signal);
      if (this.disposed || generation !== this.refreshGeneration) return;
      this.state = state;
      this.onboarding = state.onboarding;
      if (!state.onboarding.setupComplete) this.setupWasIncomplete = true;
      this.workspace = { ...this.workspace, name: state.instanceId };
      if (state.phase === "invitation-required") {
        try {
          await this.api.redeem(this.invitation, this.lifetime.signal);
          if (this.disposed || generation !== this.refreshGeneration) return;
          this.invitation = undefined; this.setupContinued = true; await this.refresh();
        } catch (error) {
          if (this.disposed || generation !== this.refreshGeneration || this.authorizationExpired) return;
          if (error instanceof SecurityError && error.code === "invitation-expired") this.showOnboarding({ kind: "expired" });
          else if (error instanceof SecurityError && error.code === "invitation-used") this.showOnboarding({ kind: "used" });
          else if (error instanceof SecurityError && error.status === 403) this.showOnboarding({ kind: "needlink", canSwitchAccount: true }, "This setup link or Google account could not be used. Use the eligible account and a current setup link to continue.");
          else this.showOnboarding({ kind: "unreachable" });
        }
        return;
      }
      this.invitation = undefined;
      if (state.onboarding.protection === "none") {
        this.methods = []; this.everyday = undefined; this.recovery = undefined; this.clearSecrets(); this.clearPending();
        if (mayOpenWorkspace(state)) { await this.plainReady(); return; }
        await this.observePlain(state.plainOperationId!); return;
      }
      const methods = await this.api.methods();
      if (this.disposed || generation !== this.refreshGeneration) return;
      this.methods = methods.filter(method => method.status === (state.phase === "ready" ? "active" : "pending") && method.envelope);
      if (!this.pending) this.readPending();
      if (state.initializingOperation) this.pending = { instanceId: state.instanceId, id: state.initializingOperation, operation: "initialize" };
      if (this.pending) { await this.observe(); return; }
      if (mayOpenWorkspace(state)) { await this.admit(); return; }
      if (state.phase !== "ready") {
        this.everyday = this.methods.find(method => method.kind !== "recovery");
        if (!this.setupContinued) { this.showSetupResume(); return; }
        if (state.onboarding.protection === "undecided") { this.showOnboarding({ kind: "encryption", policy: state.onboarding.policy }); return; }
        if (!this.everyday) { this.showEnrollment({ kind: "choose-method" }); return; }
        this.showRuntime(this.everyday.kind === "password" ? { kind: "password", context: "resume" } : { kind: "resume" });
        return;
      }
      if (state.locked === true && state.storage === "closed" && state.runtime === "stopped") this.showRuntime({ kind: "locked" });
      else if (state.storage === "open" && state.runtime === "running") this.showRuntime({ kind: "authorize-browser" });
      else this.showRuntime({ kind: "unknown", operation: "unlock" });
    } catch (error) {
      if (this.disposed || generation !== this.refreshGeneration) return;
      if (error instanceof SecurityError && error.status === 401) this.signedOut();
      else if (error instanceof SecurityError && error.status === 403) { this.resetAuthorization(); this.clearSecrets(); this.showOnboarding({ kind: "notowner" }); }
      else this.showOnboarding({ kind: "unreachable" });
    }
  }

  private async admit(): Promise<void> {
    if (this.disposed || this.authorizationExpired || !this.state || !mayOpenWorkspace(this.state)) return;
    if (this.recoveryAdmission && this.host.manageMethods) {
      const method = this.recoveryAdmission; this.recoveryAdmission = undefined;
      this.clearSecrets(); await this.manageMethods(method); return;
    }
    this.clearSecrets(); this.unbind?.(); this.unbind = undefined;
    if (!this.admitted) { this.admitted = true; await this.host.admit(); }
    if (!this.disposed && this.admitted) this.scheduleCheck();
  }
  private scheduleCheck(): void {
    clearTimeout(this.poll);
    this.poll = setTimeout(() => { void this.checkAdmission(); }, 3000);
  }
  private async checkAdmission(): Promise<void> {
    if (this.disposed || !this.admitted) return;
    try {
      const state = await this.api.state(this.lifetime.signal);
      if (!this.admitted || this.disposed) return;
      this.state = state;
      if (!mayOpenWorkspace(state)) { this.host.detach(); this.admitted = false; await this.refresh(); }
      else this.scheduleCheck();
    } catch { if (!this.disposed && !this.authorizationExpired) { this.host.detach(); this.admitted = false; this.showOnboarding({ kind: "unreachable" }); } }
  }
  private resetAuthorization(): void {
    ++this.refreshGeneration;
    clearTimeout(this.poll);
    this.closeManagement(); this.host.detach(); this.admitted = false;
    this.ceremony.abort(); this.ceremony = new AbortController(); this.clearSecrets();
    this.busy = false; this.unbind?.(); this.unbind = undefined;
    this.page = undefined; this.root.replaceChildren();
  }
  private signedOut(): void {
    if (this.disposed || this.authorizationExpired) return;
    this.authorizationExpired = true;
    this.authenticated = false;
    this.resetAuthorization();
    this.state = undefined; this.methods = []; this.everyday = undefined; this.recovery = undefined;
    this.selectedMethod = undefined; this.recoveryAdmission = undefined; this.invitation = undefined;
    if (this.onboarding) this.showOnboarding(this.onboarding.setupComplete ? { kind: "signin", encrypted: this.onboarding.protection === "encrypted" }
      : this.onboarding.started ? { kind: "interrupted" } : { kind: "needlink", canSwitchAccount: true });
    else this.showRuntime({ kind: "signed-out" });
  }
  authorizationLost(): void {
    if (this.disposed || this.authorizationExpired || this.refreshingAuthorization) return;
    this.refreshingAuthorization = true;
    this.resetAuthorization();
    void this.refresh().finally(() => { this.refreshingAuthorization = false; });
  }
  get encrypted(): boolean { return this.state?.onboarding.protection === "encrypted"; }
  lock(): void { if (this.admitted && this.encrypted) this.showRuntime({ kind: "lock-confirm" }); }
  async confirmAdmission(): Promise<boolean> {
    const state = await this.api.state(this.lifetime.signal);
    if (this.disposed || this.authorizationExpired) return false;
    this.state = state;
    if (mayOpenWorkspace(state)) return true;
    await this.refresh(); return false;
  }
  async manageMethods(recoveryMethod?: MethodRef): Promise<void> {
    if (!this.host.manageMethods || this.disposed || this.authorizationExpired) return;
    const requestedAt = this.refreshGeneration;
    const state = await this.api.state(this.lifetime.signal);
    if (this.disposed || this.authorizationExpired || requestedAt !== this.refreshGeneration) return;
    if (!mayOpenWorkspace(state) || state.onboarding.protection !== "encrypted") { await this.refresh(); return; }
    this.state = state; this.admitted = false; this.host.detach(); this.closeManagement();
    const generation = ++this.refreshGeneration;
    const dispose = await this.host.manageMethods({ root: this.root, api: this.api, state, recoveryMethod,
      onClose: () => this.refresh(), authorizationLost: () => this.authorizationLost() });
    if (this.disposed || generation !== this.refreshGeneration) dispose();
    else this.disposeManagement = dispose;
  }
  private closeManagement(): void { this.disposeManagement?.(); this.disposeManagement = undefined; }

  private context(): "unlock" | "authorize-browser" | "resume" | "resume-initialization" | "initialize" | "lock" {
    const screen = this.page?.kind === "runtime" ? this.page.view.screen : undefined;
    if (screen?.kind === "password" || screen?.kind === "recovery") return screen.context;
    if (screen?.kind === "lock-confirm") return "lock";
    if (screen?.kind === "resume" && screen.initializing) return "resume-initialization";
    if (this.state?.phase !== "ready") return "resume";
    return this.state.storage === "open" && this.state.runtime === "running" ? "authorize-browser" : "unlock";
  }

  private async actRuntime(action: RuntimeAction): Promise<void> {
    if (this.busy || this.disposed) return;
    const context = this.context();
    const signal = this.ceremony.signal;
    switch (action.kind) {
      case "help": this.showRuntime({ kind: "help" }); return;
      case "lost-methods": this.showRuntime({ kind: "lost-methods" }); return;
      case "select-method": if (this.methods.some(method => method.id === action.value)) this.selectedMethod = action.value; return;
      case "google-sign-in": await this.googleSignIn(); return;
      case "password": this.showRuntime({ kind: "password", context }); return;
      case "recovery": this.showRuntime({ kind: "recovery", context: context === "initialize" || context === "resume-initialization" ? "resume" : context }); return;
      case "copy-address": await navigator.clipboard.writeText(location.origin); this.notice("Address copied."); return;
      case "retry": await (this.state?.onboarding.protection === "none" ? this.refresh() : this.pending ? this.observe() : this.refresh()); return;
      case "start-workspace": await this.startPlain(); return;
      case "sign-out": await this.signOut(); return;
      case "cancel": await this.refresh(); return;
      case "confirm-lock": {
        const method = this.selectMethod("passkey") ?? this.selectMethod("password") ?? this.methods[0];
        if (!method) throw new Error("No unlock method is available to authorize locking.");
        if (method.kind === "password") { this.showRuntime({ kind: "password", context: "lock" }); return; }
        if (method.kind === "recovery") { this.showRuntime({ kind: "recovery", context: "lock" }); return; }
        await this.useMethod(method, "lock"); return;
      }
      case "passkey": {
        const method = this.selectMethod("passkey");
        if (!method) throw new Error("No passkey is available for this workspace.");
        await this.useMethod(method, context); return;
      }
      case "submit-password": {
        const method = this.selectMethod("password");
        if (!method) throw new Error("No encryption password is available for this workspace.");
        await this.useMethod(method, context, { kind: "password", password: action.value }); return;
      }
      case "import-kit": {
        const value = await importRecoveryKit(action.file, this.requiredState().instanceId);
        if (!signal.aborted) await this.actRuntime({ kind: "submit-recovery", value });
        return;
      }
      case "submit-recovery": {
        const method = this.selectMethod("recovery");
        if (!method) throw new Error("No recovery method is available for this workspace.");
        const credential: UnlockCredential = { kind: "recovery", recoverySecret: await parseRecoveryKey(action.value) };
        try { if (!signal.aborted) await this.useMethod(method, context, credential); } finally { clearCredential(credential); }
      }
    }
  }

  private selectMethod(kind: SecurityMethod["kind"]): SecurityMethod | undefined {
    return this.methods.find(method => method.kind === kind && method.id === this.selectedMethod) ?? this.methods.find(method => method.kind === kind);
  }

  private requiredState(): SecurityState {
    if (!this.state) throw new Error("Check the workspace state before continuing.");
    return this.state;
  }
  private temporarySecrets(clear: () => void): () => void {
    const signal = this.ceremony.signal;
    this.temporary.add(clear);
    signal.addEventListener("abort", clear, { once: true });
    return () => { signal.removeEventListener("abort", clear); this.temporary.delete(clear); clear(); };
  }

  private async useMethod(method: SecurityMethod, context: ReturnType<WorkspaceSecurity["context"]>, credential?: UnlockCredential): Promise<void> {
    const state = this.requiredState();
    if (!state.commitment) throw new Error("The workspace key is unavailable.");
    this.setBusy(true);
    let proof: PreparedProof | undefined;
    let submitted = false;
    const signal = this.ceremony.signal;
    const release = this.temporarySecrets(() => { proof?.opened.dispose(); clearCredential(credential); });
    try {
      const action: SecurityAction = { operation: context === "resume" ? "resume-enrollment" : context, method: ref(method),
        ...(context === "initialize" && this.recovery ? { recovery: ref(this.recovery) } : {}) };
      proof = await prepareProof(this.api, state.instanceId, state.commitment, method, action, credential, signal);
      if (this.disposed || signal.aborted) return;
      submitted = true;
      if (context === "resume") {
        await this.api.operate({ challengeId: proof.challenge.id, signature: proof.signature, assertion: proof.assertion }, signal);
        proof.opened.dispose(); proof = undefined;
        if (this.disposed || signal.aborted) return;
        proof = await prepareProof(this.api, state.instanceId, state.commitment, method,
          { operation: "verify-method", method: ref(method) }, credential, signal);
        if (this.disposed || signal.aborted) return;
        await this.api.operate({ challengeId: proof.challenge.id, signature: proof.signature, assertion: proof.assertion }, signal);
        if (this.disposed || signal.aborted) return;
        this.source?.dispose(); this.source = proof.opened; proof = undefined;
        this.everyday = method; this.busy = false;
        this.showEnrollment({ kind: "method-ready", methodKind: method.kind === "password" ? "password" : "passkey", methodName: method.label || "Unlock method" });
      } else if (context === "authorize-browser") {
        await this.api.operate({ challengeId: proof.challenge.id, signature: proof.signature, assertion: proof.assertion }, signal);
        if (this.disposed || signal.aborted) return;
        if (method.kind === "recovery") this.recoveryAdmission = ref(method);
        this.busy = false; await this.refresh();
      } else {
        if (method.kind === "recovery" && context === "unlock") this.recoveryAdmission = ref(method);
        await this.executeNative(context === "resume-initialization" ? "initialize" : context, proof);
      }
    } catch (error) {
      this.busy = false;
      if (this.disposed || signal.aborted) return;
      if (error instanceof IncompatiblePasskey) this.showRuntime({ kind: "incompatible" });
      else if (error instanceof DOMException && error.name === "NotAllowedError") {
        await this.refresh();
        if (this.page?.kind === "runtime" && this.page.view.screen.kind === "locked") this.showRuntime({ kind: "locked", cancelled: true });
        else this.notice("The passkey prompt was closed. No request was submitted.");
      }
      else if (credential?.kind === "password" && error instanceof MethodUnlockError) this.showRuntime({ kind: "password", context, wrong: true });
      else if (error instanceof MethodUnlockError && method.kind === "passkey" && context === "unlock") {
        await this.refresh();
        if (this.page?.kind === "runtime" && this.page.view.screen.kind === "locked") this.showRuntime({ kind: "unlock-failed" });
        else this.error("The saved passkey could not open its unlock method. Try another method.");
      }
      else if (this.pending && submitted) this.showRuntime({ kind: "unknown", operation: this.pending.operation });
      else if (error instanceof SecurityError && error.status === 401) this.signedOut();
      else this.error(submitted ? "The response was lost. Check the workspace state before trying again." : "This unlock method could not be checked. Try another method.");
    } finally { release(); if (this.ceremony.signal === signal) this.busy = false; }
  }

  private async executeNative(operation: NativeOperation, proof: PreparedProof): Promise<void> {
    const state = this.requiredState();
    const signal = this.ceremony.signal;
    this.pending = { instanceId: state.instanceId, id: proof.challenge.operationId, operation };
    if (!/^[A-Za-z0-9_-]{43}$/.test(this.pending.id)) { this.pending = undefined; throw new Error("The workspace did not reserve an operation to observe."); }
    try { sessionStorage.setItem(operationStorage, JSON.stringify(this.pending)); }
    catch { this.pending = undefined; throw new Error("This browser cannot keep the operation reference needed after reconnecting. Enable session storage before continuing."); }
    this.showRuntime({ kind: "progress", operation, storage: state.storage, runtime: state.runtime });
    let secret: Uint8Array | undefined;
    let volumeSecret: string | undefined;
    try {
      if (operation !== "lock") { secret = proof.opened.copyUnlockSecret(); volumeSecret = encodeBase64url(secret); }
    } finally { secret?.fill(0); proof.opened.dispose(); }
    const request = this.api.operate({ challengeId: proof.challenge.id, signature: proof.signature, assertion: proof.assertion,
      ...(volumeSecret ? { volumeSecret } : {}) }, signal);
    volumeSecret = undefined;
    await request;
    if (this.disposed || signal.aborted) return;
    this.busy = false;
    await this.observe();
  }
  private readPending(): void {
    try {
      const pending = JSON.parse(sessionStorage.getItem(operationStorage) ?? "null") as PendingOperation | null;
      if (pending && pending.instanceId === this.state?.instanceId && /^[A-Za-z0-9_-]{43}$/.test(pending.id) && ["initialize", "unlock", "lock"].includes(pending.operation)) this.pending = pending;
    } catch { /* A damaged local hint conveys no authority and is ignored. */ }
  }
  private clearPending(): void { this.pending = undefined; try { sessionStorage.removeItem(operationStorage); } catch { /* The server rechecks every public operation hint. */ } }
  private async observe(): Promise<void> {
    const pending = this.pending;
    if (!pending || this.disposed || this.authorizationExpired) return;
    const generation = this.refreshGeneration;
    this.busy = false;
    try {
      const observed = await this.api.observe(pending.id, this.lifetime.signal);
      const state = await this.api.state(this.lifetime.signal);
      if (this.disposed || generation !== this.refreshGeneration || this.pending !== pending) return;
      this.state = state;
      if (pending.operation === "lock" && observed.status === "complete" && observed.locked === true && state.locked === true &&
          observed.storage === "closed" && observed.runtime === "stopped" && state.storage === "closed" && state.runtime === "stopped") {
        this.clearPending(); this.clearSecrets(); this.showRuntime({ kind: "locked", confirmed: true }); return;
      }
      if (pending.operation === "lock" && observed.status === "refused" && state.storage === "open" && state.runtime === "running") {
        this.clearPending(); this.clearSecrets(); this.showRuntime({ kind: "authorize-browser" });
        this.notice("Locking did not start. Verify this browser to keep working, then try locking again."); return;
      }
      if (pending.operation !== "lock" && mayOpenWorkspace(state)) {
        this.clearPending();
        if (pending.operation === "initialize") {
          const label = this.everyday?.label || "Unlock method";
          this.clearSecrets(); this.showEnrollment({ kind: "complete", methodName: label, methodCheckedAt: "checked during setup", recoveryCheckedAt: "checked during setup", kitDownloadRequested: this.downloadRequested });
        } else await this.admit();
        return;
      }
      if (pending.operation !== "lock" && state.phase === "ready" && observed.storage === "open" && observed.runtime === "running" && state.storage === "open" && state.runtime === "running") {
        this.clearPending(); this.clearSecrets(); this.showRuntime({ kind: "authorize-browser" }); return;
      }
      if (pending.operation === "initialize" && state.phase === "initializing" && state.runtime === "stopped") {
        if (state.migration === "pending" && state.storage === "open") {
          this.clearSecrets(); this.showOnboarding({ kind: "migration" }); return;
        }
        if (state.migration === "complete") { this.showInitializationResume(); return; }
      }
      if (observed.status === "running") {
        this.showRuntime({ kind: "progress", operation: pending.operation, storage: observed.storage, runtime: observed.runtime });
        clearTimeout(this.poll); this.poll = setTimeout(() => { if (this.pending === pending) void this.observe(); }, 1000);
      } else if (pending.operation === "lock" && observed.storage === "open" && state.storage === "open") this.showRuntime({ kind: "lock-failed" });
      else this.showRuntime({ kind: "unknown", operation: pending.operation });
    } catch (error) {
      if (this.disposed || generation !== this.refreshGeneration) return;
      if (error instanceof SecurityError && error.status === 401) { this.signedOut(); return; }
      if (pending.operation === "initialize" && this.state?.phase === "initializing" && error instanceof SecurityError && [403, 404].includes(error.status)) {
        if (this.state.migration === "pending" && this.state.storage === "open" && this.state.runtime === "stopped") {
          this.clearSecrets(); this.showOnboarding({ kind: "migration" }); return;
        }
        this.showInitializationResume();
      } else this.showRuntime({ kind: "unknown", operation: pending.operation });
    }
  }

  private showInitializationResume(): void {
    const original = this.state?.initializingMethod;
    this.everyday = original ? this.methods.find(method => method.id === original.id && method.revision === original.revision) : this.methods.find(method => method.kind !== "recovery");
    this.selectedMethod = this.everyday?.id;
    this.showRuntime(this.everyday?.kind === "password" ? { kind: "password", context: "resume-initialization" } : { kind: "resume", initializing: true });
  }

  private async actEnrollment(action: EnrollmentAction): Promise<void> {
    if (this.disposed || (this.busy && action.kind !== "cancel-passkey")) return;
    const signal = this.ceremony.signal;
    switch (action.kind) {
      case "toggle-boundary": if (this.page?.kind === "enrollment" && this.page.view.screen.kind === "invitation") this.showEnrollment({ ...this.page.view.screen, boundaryOpen: !this.page.view.screen.boundaryOpen }); return;
      case "google-sign-in": await this.googleSignIn(); return;
      case "choose-password": this.showEnrollment({ kind: "password" }); return;
      case "create-passkey": await this.createEveryday("passkey"); return;
      case "set-password": await this.createEveryday("password", action.password); return;
      case "cancel-passkey": this.ceremony.abort(); this.ceremony = new AbortController(); this.busy = false; this.showEnrollment({ kind: "choose-method" }); return;
      case "name-method":
        if (!this.everyday || !action.name || action.name.length > 80) throw new Error("Give this unlock method a name.");
        await this.api.label(this.everyday, action.name);
        if (signal.aborted) return;
        this.everyday.label = action.name; await this.createRecovery(); return;
      case "resume-recovery": await this.refresh(); return;
      case "show-recovery": if (this.key) this.showEnrollment({ kind: "recovery-save", key: this.key }); else await this.refresh(); return;
      case "verify-recovery": this.showEnrollment({ kind: "recovery-verify" }); return;
      case "copy-key": if (!this.key) throw new Error("The recovery key is no longer available on this page."); await navigator.clipboard.writeText(this.key); this.notice("Recovery key copied."); return;
      case "copy-address": await navigator.clipboard.writeText(location.origin); this.notice("Address copied."); return;
      case "download-kit": case "print-kit": {
        if (!this.key) throw new Error("The recovery key is no longer available on this page.");
        const kit = await recoveryKit(this.requiredState().instanceId, location.origin, this.key);
        if (this.disposed || signal.aborted) return;
        if (action.kind === "download-kit") { downloadRecoveryKit(kit); this.downloadRequested = true; this.notice("Recovery kit download requested. Check that the file was saved."); }
        else { this.clearPrint?.(); this.clearPrint = printRecoveryKit(kit); this.notice("Print dialog requested. Check that your copy was printed."); }
        return;
      }
      case "import-kit": {
        const key = await importRecoveryKit(action.file, this.requiredState().instanceId);
        if (!signal.aborted) await this.checkRecovery(key);
        return;
      }
      case "check-recovery": await this.checkRecovery(action.key); return;
      case "open-workspace": case "check-state": await this.refresh(); return;
      case "add-method": await this.manageMethods(); return;
      case "sign-out": await this.signOut(); return;
    }
  }

  private async createEveryday(kind: "passkey" | "password", password?: string): Promise<void> {
    this.setBusy(true);
    let credential: UnlockCredential | undefined;
    let secret: Uint8Array | undefined;
    let proof: PreparedProof | undefined;
    const signal = this.ceremony.signal;
    const release = this.temporarySecrets(() => { secret?.fill(0); clearCredential(credential); proof?.opened.dispose(); });
    try {
      const state = this.requiredState();
      if (state.commitment) throw new Error("An unlock method is already saved. Verify it to continue setup.");
      const prepared = await this.api.prepare(kind, kind === "passkey" ? "Passkey" : "Encryption password", signal);
      signal.throwIfAborted();
      let registration;
      if (kind === "passkey") {
        this.showEnrollment({ kind: "passkey-pending" });
        ({ credential, registration } = await registerPasskey(prepared, signal));
      } else credential = { kind: "password", password: password! };
      this.showEnrollment({ kind: "checking-method", methodKind: kind });
      secret = generateWorkspaceSecret();
      const commitment = await workspaceCommitment(secret);
      signal.throwIfAborted();
      const method = await saveCandidate(this.api, state.instanceId, prepared, secret, commitment, credential!, registration, signal);
      signal.throwIfAborted();
      secret.fill(0); secret = undefined;
      this.state = { ...state, commitment };
      clearCredential(kind === "passkey" ? credential : undefined);
      proof = await prepareProof(this.api, state.instanceId, commitment, method,
        { operation: "verify-method", method: ref(method) }, kind === "password" ? credential : undefined, signal);
      if (this.disposed || signal.aborted) return;
      await this.api.operate({ challengeId: proof.challenge.id, signature: proof.signature, assertion: proof.assertion }, signal);
      if (this.disposed || signal.aborted) return;
      this.everyday = method; this.methods = [method];
      this.source?.dispose(); this.source = proof.opened; proof = undefined;
      this.busy = false; this.showEnrollment({ kind: "method-ready", methodKind: kind, methodName: method.label || (kind === "passkey" ? "Passkey" : "Encryption password") });
    } catch (error) {
      this.busy = false;
      if (this.disposed || signal.aborted) return;
      if (error instanceof IncompatiblePasskey) this.showEnrollment({ kind: "passkey-incompatible" });
      else if (error instanceof DOMException && error.name === "NotAllowedError") this.showEnrollment({ kind: "choose-method" }, undefined, "The passkey prompt was closed or timed out. Choose a method to continue.");
      else this.error("The unlock method was not confirmed. Check the workspace state before continuing.");
    } finally { release(); if (this.ceremony.signal === signal) this.busy = false; }
  }

  private async createRecovery(): Promise<void> {
    if (!this.source || !this.everyday) throw new Error("Verify your saved unlock method to continue.");
    const state = this.requiredState();
    this.setBusy(true);
    const secret = this.source.copyUnlockSecret();
    const recoverySecret = generateRecoverySecret();
    const signal = this.ceremony.signal;
    const release = this.temporarySecrets(() => { secret.fill(0); recoverySecret.fill(0); });
    try {
      const prepared = await this.api.prepare("recovery", "Recovery key", signal);
      signal.throwIfAborted();
      const recovery = await saveCandidate(this.api, state.instanceId, prepared, secret, state.commitment!, { kind: "recovery", recoverySecret }, undefined, signal);
      if (this.disposed || signal.aborted) return;
      this.recovery = recovery;
      const key = await formatRecoveryKey(recoverySecret);
      if (this.disposed || signal.aborted) return;
      this.key = key;
      this.source?.dispose(); this.source = undefined;
      this.busy = false; this.showEnrollment({ kind: "recovery-save", key: this.key });
    } finally { release(); if (this.ceremony.signal === signal) this.busy = false; }
  }
  private async checkRecovery(value: string): Promise<void> {
    if (!this.recovery || !this.everyday) throw new Error("Resume setup with your saved unlock method first.");
    const signal = this.ceremony.signal;
    let credential: UnlockCredential;
    try { credential = { kind: "recovery", recoverySecret: await parseRecoveryKey(value) }; }
    catch (error) {
      if (this.disposed || signal.aborted) return;
      this.showEnrollment({ kind: "recovery-verify", input: value, failure: { kind: "format", message: error instanceof Error ? error.message : "The key's format or check group does not match.", hint: "Compare the complete key with your saved copy, or import the kit file." } }); return;
    }
    if (this.disposed || signal.aborted) { clearCredential(credential); return; }
    this.setBusy(true);
    let proof: PreparedProof | undefined;
    const release = this.temporarySecrets(() => { value = ""; clearCredential(credential); proof?.opened.dispose(); });
    try {
      const state = this.requiredState();
      proof = await prepareProof(this.api, state.instanceId, state.commitment!, this.recovery,
        { operation: "verify-method", method: ref(this.recovery) }, credential, signal);
      if (this.disposed || signal.aborted) return;
      await this.api.operate({ challengeId: proof.challenge.id, signature: proof.signature }, signal);
      proof.opened.dispose(); proof = undefined;
      if (this.disposed || signal.aborted) return;
      this.busy = false; this.key = undefined;
      if (this.everyday.kind === "password") this.showRuntime({ kind: "password", context: "initialize" });
      else await this.useMethod(this.everyday, "initialize");
    } catch (error) {
      if (this.disposed || signal.aborted) return;
      this.busy = false;
      if (error instanceof SecurityError && error.status === 401) this.signedOut();
      else if (error instanceof MethodUnlockError) this.showEnrollment({ kind: "recovery-verify", input: value, failure: { kind: "wrong-key" } });
      else this.showEnrollment({ kind: "recovery-verify", input: value }, "The recovery key could not be checked. Check the connection and try again.");
    }
    finally { release(); if (this.ceremony.signal === signal) this.busy = false; }
  }

  private async googleSignIn(chooseAccount = false): Promise<void> {
    this.setBusy(true);
    if (this.invitation && !this.onboarding?.started) { await this.api.handoff(this.invitation, this.lifetime.signal); this.invitation = undefined; }
    if (!this.disposed) location.assign(chooseAccount ? "/auth/login?choose_account=1" : "/auth/login");
  }
  private async signOut(): Promise<void> {
    await this.session.logout(); this.clearPending(); this.signedOut();
  }
  private clearSecrets(): void {
    for (const clear of this.temporary) clear();
    this.clearPrint?.(); this.clearPrint = undefined;
    this.source?.dispose(); this.source = undefined; this.key = undefined;
  }
  dispose(): void {
    this.disposed = true; this.api.onAuthorizationLost = undefined; this.closeManagement(); this.lifetime.abort(); this.ceremony.abort(); this.unbind?.(); clearTimeout(this.poll); this.clearSecrets(); this.invitation = undefined;
  }
}
