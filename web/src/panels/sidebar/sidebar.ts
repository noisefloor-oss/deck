// The sidebar's presentation model: projects with their sessions, status
// styling, and the selection/creation callbacks. DOM-free so the logic
// runs under plain node tests; the DOM shell (sidebar-dom.ts) renders it.
import type {
  CreateProjectOptions,
  GitIdentityCatalog,
  GitIdentityProfile,
  Project,
  ProjectDirectoryListing,
  ReplacedGitIdentity,
} from "../../api/projects.ts";
import type { ApiClient } from "../../api/contracts.ts";
import type { HarnessUsageReport, SessionSummary } from "../../api/sessions.ts";

// sessionColorStyle returns an inline style override for the session tile
// when the user has assigned a color tag. Null/undefined = no override
// (the provider hue is used as-is). Tiles carry colour, never letters, so
// the override sets only the background.
//

import { ApiError } from "../../api/http.ts";
import { lucideIcon } from "../../ui/icons.ts";
import { escapeHtml } from "../../lib/escape.ts";
import { isAppleClient } from "../../lib/platform.ts";
import { formatSince } from "../../lib/format.ts";
import { isVerifiedRunningSession, sessionNeedsAttention, sessionStaysListed } from "../../ui/session-status.ts";
import { applyAutoColors } from "../../lib/auto-color.ts";
import { operatorProjectColorKey } from "../../lib/session-colors.ts";
import { readListCache, writeListCache, type ListCacheStorage } from "./list-cache.ts";

// ProjectWithSessions is one sidebar row group.
export interface ProjectWithSessions {
  project: Project;
  sessions: SessionSummary[];
  // loadError reports a sessions fetch that failed for THIS project —
  // one bad project must not blank the whole sidebar.
  loadError?: string;
  // color is the user-assigned project color tag, used to
  // tint the row's disclosure chevron and session rail. Absent until the
  // project menu can set one.
  color?: string | null;
  // autoColor is the derived tint from Auto-color projects. It is render-time
  // only and never written to the backend; it is kept separate from color so
  // a user-assigned project tag still wins and the preference can be
  // toggled off without residue.
  autoColor?: string | null;
}

// HarnessInventory is the sidebar's view of GET /api/providers. It keeps
// three states distinct so the picker can tell the user the truth:
//   served  - the server answered; providers may be empty.
//   stale   - the last fetch failed, but a previous served answer is kept.
//   unknown - no answer has ever succeeded, so there is nothing to keep.
export type HarnessInventory =
  | { status: "served"; providers: readonly string[] }
  | { status: "stale"; providers: readonly string[]; error: string }
  | { status: "unknown"; error: string | null };

// replacedIdentityNotice states what a git identity write overwrote in the
// repository's local config. Deck writes first and reports; the user
// undoes it with one git config command if that was not what they wanted.
export function replacedIdentityNotice(replaced: ReplacedGitIdentity | undefined): string | null {
  if (replaced === undefined || replaced === null) return null;
  const keys: [keyof ReplacedGitIdentity, string][] = [
    ["name", "user.name"],
    ["email", "user.email"],
    ["use_config_only", "user.useConfigOnly"],
  ];
  const parts = keys
    .filter(([field]) => (replaced[field] ?? "") !== "")
    .map(([field, key]) => `${key} was ${replaced[field]}`);
  if (parts.length === 0) return null;
  return `Replaced this repository's local git config: ${parts.join(", ")}. Change it back with git config --local in the project.`;
}

// Last-output age is passive activity metadata, never a session-state or
// recovery inference. A missing timestamp renders no evidence.
export function sessionRowMetaHtml(
  session: SessionSummary,
  now = Date.now(),
): string {
  const since = formatSince(session.last_output_at, now);
  if (since === null) return "";
  return `<span class="max-w-32 truncate">${escapeHtml(since)}</span>`;
}

// The session row's after-the-name cell: a spinner while
// the server presents the session as running AND output is actively
// streaming, otherwise the age tick. The spinner's gate is the
// server-presented presentation state only — never a browser-side status
// inference — and `streaming` is the content-free output-edge window the
// DOM shell supplies; a stale edge cannot spin a row the server no longer
// presents as running.
// ChatMark is the row's chat fact: the agent is in a live
// chat, and whether that chat holds messages the user has not seen.
export type ChatMark = "live" | "unseen" | null;

export function sessionMetaHtml(session: SessionSummary, streaming: boolean, chat: ChatMark = null): string {
  const spinning = streaming && isVerifiedRunningSession(session);
  // A listed-but-unverified session carries the content-free warning dot:
  // the row says "needs attention"
  // without the list ever speaking a status word.
  const attention = sessionNeedsAttention(session)
    ? `<span data-meta-attention role="img" aria-label="needs attention" class="mr-1 block h-1.5 w-1.5 flex-none rounded-full bg-warning"></span>`
    : "";
  // Both cells render and the scope picks: the desktop swaps the tick for
  // the spinner while output streams; the phone keeps the time trailing on
  // every row and moves the spinner to the row's leading status column.
  // Content CSS cannot regenerate, so the markup carries both and display
  // does the trading.
  const spinner = spinning
    ? `<span data-meta-spinner role="img" aria-label="output streaming"
      class="block h-[11px] w-[11px] flex-none animate-row-spinner rounded-circle border-[1.5px] border-border-strong border-t-text-secondary"></span>`
    : "";
  // The chat mark sits ahead of the tick or spinner: an 11px
  // message-square, tertiary while the chat is live, accent while it holds
  // messages the user has not seen. No count.
  const mark = chat === null
    ? ""
    : `<span data-meta-chat="${chat}" role="img" aria-label="${chat === "unseen" ? "in a chat with unseen messages" : "in a chat"}"
      class="mr-1.5 flex h-[11px] w-[11px] flex-none items-center justify-center ${chat === "unseen" ? "text-accent" : "text-text-tertiary"}">${lucideIcon("message-square", 11)}</span>`;
  const tick = sessionRowMetaHtml(session);
  const trailing = `${spinner}${spinning && tick !== "" ? `<span data-meta-tick class="hidden">${tick}</span>` : tick}`;
  // With a mark in the row, the tick and the spinner share one fixed
  // right-aligned slot, 26px, the width of the widest tick at micro size
  // ("< 1m"), so every mark lands on the same x whatever its row's time
  // reads. A tick that would not fit is clipped, never allowed
  // to push the mark.
  const slot = chat === null
    ? trailing
    : `<span data-meta-slot class="inline-flex w-[26px] flex-none items-center justify-end overflow-hidden whitespace-nowrap">${trailing}</span>`;
  const fact = session.conversation_attention;
  const currentConversation = fact?.session_id === session.id && fact.generation === session.generation;
  const needsInput = currentConversation && fact.state === "needs_input" && isVerifiedRunningSession(session);
  const unseen = currentConversation && fact.ended_sequence > fact.seen_sequence;
  const conversation = needsInput || unseen
    ? `<span data-conversation-attention="${needsInput ? "needs_input" : "ended"}" role="img" aria-label="${needsInput ? "Agent needs input" : "Unseen ended turn"}" class="mr-1.5 flex h-4 w-4 flex-none items-center justify-center rounded-full ${needsInput ? "bg-warning text-text-on-accent" : "border border-accent text-accent"}">${needsInput ? "?" : "✓"}</span>`
    : "";
  return `${conversation}${mark}${attention}${slot}`;
}

// modKeyLabel maps the platform to the shortcut chip's label:
// ⌘ on Apple platforms, Ctrl elsewhere. The userAgentData platform wins
// when present AND non-empty — an empty string must fall through to the
// userAgent.
export function modKeyLabel(uaDataPlatform: string | undefined, userAgent: string): string {
  return isAppleClient({ userAgentData: { platform: uaDataPlatform }, userAgent }) ? "⌘K" : "Ctrl K";
}
// There is deliberately no hardcoded provider list here. Which harnesses
// exist is a property of the image Deck is running in, not of the bundle the
// browser was served, and only the process that resolved the executables can
// answer it. The server does, over GET /api/providers, in the order the
// picker draws them; a constant here could only ever be a second answer to a
// question this code cannot see.

export interface SidebarCallbacks {
  onProvidersChanged?: () => void;
  // The default Git identity changed after the first read.
  onDefaultIdentityChanged?: () => void;
  onCreateProject?: (projectID: string) => void;
  onRenameProject?: (projectID: string) => void;
  onRemoveProject?: (projectID: string) => void;
  onSelectSession?: (sessionID: string, keyboard: boolean) => void;
  onDeselectSession?: (sessionID: string) => void;
  onCreateSession?: (sessionID: string, codexLoginHelper?: boolean) => void;
  onRenameSession?: (sessionID: string) => void;
  onRemoveSession?: (sessionID: string) => void;
  onError?: (message: string, cause?: unknown) => void;
}

// ListState is the rail's three-valued answer to "what does the server
// say?": `loading` until the first read settles, `ready` after a read
// succeeded, `error` after the latest read failed. Empty states render only
// from `ready`; the other two draw the shell and say what is happening.
export type ListState = "loading" | "ready" | "error";

export class SidebarModel {
  groups: ProjectWithSessions[] = [];
  gitIdentities: GitIdentityCatalog | null = null;
  selectedSessionID: string | null = null;
  loading = false;
  listState: ListState = "loading";
  // stale marks groups the server has not confirmed: the previous visit's
  // cached list on a cold load, or the last served list outliving a failed
  // read. Stale rows draw dimmed and inert — the user can see what was
  // there, and nothing on the page claims it still is.
  stale = false;
  private createSessionInFlight = false;
  private refreshGeneration = 0;
  private inventoryGeneration = 0;
  private pendingSessionRemovals = new Map<string, string>();
  // error is the last user-visible failure (a rejected create, a
  // failed projects fetch) — the DOM shell renders it until the next
  // success clears it.
  error: string | null = null;
  // registryReadOk reports whether groups reflect a successful registry
  // read. It stays false until one succeeds and flips back on a failed
  // refresh, so a registry nobody has read is never presented as a
  // known-empty database: the no-project rail and the pane's create
  // actions stay suppressed until a read succeeds.
  registryReadOk = false;
  // identityNotice reports what the last git identity write overwrote in a
  // repository. It is not an error; it stands until the next identity or
  // project write, the same way error does.
  identityNotice: string | null = null;
  // harnessInventory is what the server says this image can launch. It
  // starts unknown and is only ever set from a served answer: the console
  // cannot tell from its own bundle which harnesses the image contains, so
  // an unanswered question offers nothing rather than offering all sixteen.
  // A failed fetch keeps the last served answer as stale; an unknown state
  // means there has never been a served answer to keep.
  harnessInventory: HarnessInventory = { status: "unknown", error: null };
  // harnessUsage is the recorded launch history the picker ranks by, for the
  // project it was last read for. Null means no answer — a read that has not
  // happened and one that failed are the same thing to the picker, which
  // then draws the served order rather than a guess.
  harnessUsage: HarnessUsageReport | null = null;
  private harnessUsageProject: string | null = null;
  private harnessUsageGeneration = 0;
  readonly api: ApiClient;
  private cb: SidebarCallbacks;
  private autoColorProjects: boolean;
  private listCache: ListCacheStorage | null;

  constructor(
    api: ApiClient,
    cb: SidebarCallbacks = {},
    autoColorProjects = false,
    listCache: ListCacheStorage | null = null,
  ) {
    this.api = api;
    this.cb = cb;
    this.autoColorProjects = autoColorProjects;
    this.listCache = listCache;
    const cached = listCache === null ? null : readListCache(listCache);
    if (cached !== null && cached.length > 0) {
      this.groups = cached;
      applyAutoColors(this.groups, this.autoColorProjects);
      this.stale = true;
    }
  }

  get creatingSession(): boolean {
    return this.createSessionInFlight;
  }

  // availableProviders exposes the last known list for code that only needs
  // the inventory contents. Unknown state reads as empty; stale still returns
  // the kept providers so the picker can draw them while it offers a retry.
  get availableProviders(): readonly string[] {
    return this.harnessInventory.status === "unknown" ? [] : this.harnessInventory.providers;
  }

  // setAutoColorProjects stores the preference and applies it to the current
  // groups. The model owns when colours are applied, so callers update the
  // preference here rather than calling applyAutoColors themselves.
  setAutoColorProjects(value: boolean): void {
    this.autoColorProjects = value;
    applyAutoColors(this.groups, this.autoColorProjects);
  }

  // refresh reloads projects and each project's sessions. A project that
  // fails its sessions fetch keeps its place with an error marker; a
  // failed projects fetch surfaces through onError and keeps the last
  // served list as stale: the server being unreachable is not the server
  // saying the sessions are gone, so nothing is dropped and nothing is
  // deselected until a read succeeds. Harnesses become visible as soon as
  // their independent request finishes; session reconciliation must not
  // hold up the picker.
  async refresh(): Promise<void> {
    const generation = ++this.refreshGeneration;
    this.loading = true;
    const inventoryPromise = this.refreshProviders();
    try {
      const [catalog, projects] = await Promise.all([
        this.api.listGitIdentities(),
        this.api.listProjects(),
      ]);
      const groups = await Promise.all(
        projects.map(async (project) => {
          try {
            const listed = await this.api.listSessions(project.id);
            return { project, sessions: listed.filter(sessionStaysListed) };
          } catch (err) {
            return {
              project,
              // Without a fresh reconciled response, no saved row belongs on
              // the active terminal surface.
              sessions: [],
              loadError: err instanceof Error ? err.message : String(err),
            };
          }
        }),
      );
      if (generation !== this.refreshGeneration) return;
      this.groups = groups;
      applyAutoColors(this.groups, this.autoColorProjects);
      this.stale = false;
      this.listState = "ready";
      this.deselectUnavailableSession();
      const previousDefault = this.gitIdentities?.default_profile_id;
      this.gitIdentities = catalog;
      this.error = null;
      this.registryReadOk = true;
      this.confirmPendingSessionRemovals();
      if (this.listCache !== null) writeListCache(this.listCache, this.groups);
      if (previousDefault !== undefined && previousDefault !== catalog.default_profile_id) this.cb.onDefaultIdentityChanged?.();
    } catch (err) {
      if (generation !== this.refreshGeneration) return;
      const message = err instanceof Error ? err.message : String(err);
      this.stale = this.groups.length > 0;
      this.listState = "error";
      this.error = message;
      this.registryReadOk = false;
      this.cb.onError?.(message, err);
    } finally {
      if (generation === this.refreshGeneration) this.loading = false;
      await inventoryPromise;
    }
  }

  // refreshHarnessUsage reads the launch history for one project, for the
  // picker that is opening. A failure is not surfaced: the ranking is a
  // presentation preference over a list the user already has, so its
  // absence costs the served order and nothing else.
  //
  // It is read per opening rather than cached for the session: a launch made
  // since the last read is exactly the history the next opening should
  // reflect.
  async refreshHarnessUsage(projectID: string): Promise<void> {
    const generation = ++this.harnessUsageGeneration;
    if (this.harnessUsageProject !== projectID) {
      // Another project's ranking must not paint this one's picker while
      // the read is in flight.
      this.harnessUsage = null;
      this.harnessUsageProject = projectID;
    }
    let usage: HarnessUsageReport | null = null;
    try {
      usage = await this.api.getHarnessUsage(projectID);
    } catch {
      usage = null;
    }
    if (generation !== this.harnessUsageGeneration) return;
    this.harnessUsage = usage;
    this.harnessUsageProject = projectID;
  }

  // refreshProviders re-reads only the harness inventory. It is used by the
  // picker's retry affordance so a transient failure does not rebuild the
  // whole project list underneath an open dialog.
  async refreshProviders(): Promise<void> {
    const generation = ++this.inventoryGeneration;
    try {
      const providers = await this.api.listProviders();
      if (generation !== this.inventoryGeneration) return;
      this.applyHarnessInventory(providers);
    } catch (err) {
      if (generation !== this.inventoryGeneration) return;
      this.applyHarnessInventory(err instanceof Error ? err : new Error(String(err)));
    }
    this.cb.onProvidersChanged?.();
  }

  // applyHarnessInventory updates harnessInventory from a fresh answer. A
  // successful answer always becomes served, even when it is empty. A failed
  // answer downgrades from the previous status: served becomes stale (keeping
  // its providers), stale stays stale, and unknown stays unknown. This means
  // a server that honestly answered "none" and then became unreachable reads
  // as stale, not as unknown.
  applyHarnessInventory(providersOrError: readonly string[] | { error: string } | Error): void {
    if (Array.isArray(providersOrError)) {
      this.harnessInventory = { status: "served", providers: providersOrError };
      return;
    }
    const error = "message" in providersOrError
      ? providersOrError.message
      : (providersOrError as { error: string }).error;
    const current = this.harnessInventory;
    if (current.status === "served") {
      this.harnessInventory = { status: "stale", providers: current.providers, error };
    } else if (current.status === "stale") {
      this.harnessInventory = { status: "stale", providers: current.providers, error };
    } else {
      this.harnessInventory = { status: "unknown", error };
    }
  }

  private deselectUnavailableSession(): void {
    const selected = this.selectedSessionID;
    if (selected === null) return;
    const available = this.groups.some((group) =>
      group.sessions.some((session) => session.id === selected && sessionStaysListed(session)));
    if (available) return;
    this.selectedSessionID = null;
    this.cb.onDeselectSession?.(selected);
  }

  private confirmPendingSessionRemovals(): void {
    for (const [sessionID, projectID] of this.pendingSessionRemovals) {
      if (projectID === "") {
        if (this.groups.some((group) => group.loadError !== undefined)) continue;
        if (this.groups.some((group) => group.sessions.some((session) => session.id === sessionID))) {
          continue;
        }
        this.pendingSessionRemovals.delete(sessionID);
        this.cb.onRemoveSession?.(sessionID);
        continue;
      }
      const group = this.groups.find((candidate) => candidate.project.id === projectID);
      if (group?.loadError !== undefined) continue;
      if (group?.sessions.some((session) => session.id === sessionID) === true) continue;
      this.pendingSessionRemovals.delete(sessionID);
      this.cb.onRemoveSession?.(sessionID);
    }
  }

  private projectIDForSession(sessionID: string): string {
    return this.groups.find((group) => group.sessions.some((session) => session.id === sessionID))
      ?.project.id ?? "";
  }

  private async awaitRemovalConfirmation(sessionID: string): Promise<boolean> {
    this.pendingSessionRemovals.set(sessionID, this.projectIDForSession(sessionID));
    this.error = null;
    await this.refresh();
    return !this.pendingSessionRemovals.has(sessionID);
  }

  private async confirmRejectedRemoval(sessionID: string, projectID: string): Promise<boolean> {
    let collections: Array<{ projectID: string; sessions: SessionSummary[] }>;
    try {
      const projectIDs = projectID === ""
        ? (await this.api.listProjects()).map((project) => project.id)
        : [projectID];
      collections = await Promise.all(projectIDs.map(async (id) => ({
        projectID: id,
        sessions: await this.api.listSessions(id),
      })));
    } catch {
      return false;
    }
    for (const collection of collections) {
      const group = this.groups.find((candidate) => candidate.project.id === collection.projectID);
      if (group !== undefined) {
        group.sessions = collection.sessions.filter(sessionStaysListed);
        delete group.loadError;
        const selected = this.selectedSessionID;
        if (selected !== null && !group.sessions.some((session) => session.id === selected)) {
          this.selectedSessionID = null;
          this.cb.onDeselectSession?.(selected);
        }
      }
    }
    if (collections.some((collection) =>
      collection.sessions.some((session) => session.id === sessionID))) return false;
    this.pendingSessionRemovals.delete(sessionID);
    this.error = null;
    this.cb.onRemoveSession?.(sessionID);
    return true;
  }

  // select marks the session active and reports it. Selection is local
  // presentation state; the server learns nothing until the user
  // acts on the session itself. keyboard distinguishes an Enter or Space
  // on a focused row from a click: a keyboard selection must leave focus
  // where the keyboard is, so downstream focus moves key off it.
  select(sessionID: string, keyboard = false): boolean {
    const session = this.groups
      .flatMap((group) => group.sessions)
      .find((candidate) => candidate.id === sessionID);
    // A listed session is a selectable session: limbo
    // rows select into the recovery surface; only rows off the list refuse.
    // A stale row is not listed by the server, so it refuses too: the rail
    // draws it inert, and the keyboard must not reach what the pointer
    // cannot.
    if (this.stale || session === undefined || !sessionStaysListed(session)) return false;
    this.selectedSessionID = sessionID;
    this.cb.onSelectSession?.(sessionID, keyboard);
    return true;
  }

  // rename records the user's own label for a session row, then
  // refreshes so the list shows the server's truth. An empty name clears
  // the label. Failures surface like create's: the server's message,
  // in-band, never swallowed.
  async rename(sessionID: string, name: string): Promise<boolean> {
    try {
      await this.api.renameSession(sessionID, name);
      this.cb.onRenameSession?.(sessionID);
      this.error = null;
      await this.refresh();
      return true;
    } catch (err) {
      const message = err instanceof Error ? err.message : String(err);
      this.error = message;
      this.cb.onError?.(message, err);
      return false;
    }
  }

  async setSessionColor(sessionID: string, color: string | null): Promise<boolean> {
    try {
      await this.api.setSessionColor(sessionID, color);
      this.error = null;
      await this.refresh();
      return true;
    } catch (err) {
      const message = err instanceof Error ? err.message : String(err);
      this.error = message;
      this.cb.onError?.(message, err);
      return false;
    }
  }

  // setProjectColor records the user's chosen tag for a project row in
  // the live groups. Durable storage is the sidebar layout document, which
  // the panel owns: it feeds the saved map back through applyProjectColors
  // after every refresh, so this in-memory write and the persisted map move
  // together.
  async setProjectColor(projectID: string, color: string | null): Promise<boolean> {
    const group = this.groups.find((g) => g.project.id === projectID);
    if (group === undefined) return false;
    group.color = color;
    // The auto-color tints are derived from which projects carry a colour, so
    // they are re-derived here: setting a tag drops the tint under it, and
    // clearing one back to no colour lets the tint return.
    applyAutoColors(this.groups, this.autoColorProjects);
    return true;
  }

  // applyProjectColors re-applies the user's saved project tags onto the
  // current groups. A model refresh rebuilds groups from the server without
  // colours, so the panel calls this at its render choke point with the saved
  // document's map; an id missing from the map is an explicit clear, not a
  // keep, so a cleared project does not flash its old colour back. The auto
  // tints re-derive around whatever this leaves behind. presentedOrder is
  // the displayed project id sequence, handed in by the same call: the shell
  // owns the drag-built row order and re-derives the tints against it here,
  // so a drag re-colours the rail without the model ever storing row order.
  // Omitted, the tints fall back to the alphabetical default until the next
  // render supplies the order.
  applyProjectColors(
    colors: Record<string, string>,
    presentedOrder?: readonly string[] | null,
  ): void {
    for (const g of this.groups) {
      g.color = colors[g.project.id] ?? null;
    }
    applyAutoColors(this.groups, this.autoColorProjects, presentedOrder);
  }
  // applyProjectColorToSessions persists the project's OWN colour to every
  // session in it, through the same path the inline swatch picker uses. A
  // project the user never coloured propagates no colour: it clears,
  // because "none" is the colour it has. The auto-color tint is never
  // written — it is derived from row position and would tag sessions with a
  // hue that outlives the preference and moves when a project is dragged.
  // User tags are restored after each write because refresh rebuilds
  // groups from the server.
  async applyProjectColorToSessions(projectID: string): Promise<boolean> {
    const group = this.groups.find((g) => g.project.id === projectID);
    if (group === undefined) return false;
    const projectColor = group.color ?? null;
    const color = operatorProjectColorKey(projectColor);
    let ok = true;
    for (const session of group.sessions) {
      if (!(await this.setSessionColor(session.id, color))) ok = false;
      const restored = this.groups.find((g) => g.project.id === projectID);
      if (restored !== undefined) {
        restored.color = projectColor;
        // The refresh inside setSessionColor rebuilt the groups, so the project
        // came back uncoloured and took an auto tint. Restoring the colour has
        // to re-derive too, or the tint stays under it and paints any session
        // this loop has not reached.
        applyAutoColors(this.groups, this.autoColorProjects);
      }
    }
    return ok;
  }

  // remove deletes a session through the real endpoint, notifies the app
  // shell (the selected session's teardown lives behind onRemoveSession),
  // then refreshes so the list shows the server's truth. A not_found
  // response (session already gone) is best-effort: no transient error.
  // Other failures surface in-band. Runtime classification and record-only
  // deletion stay entirely behind the ordinary backend endpoint.
  async remove(sessionID: string): Promise<boolean> {
    // Preserve the owning project before the request. A concurrent refresh may
    // hide the row while DELETE is in flight, but confirmation still has to
    // inspect that project's unfiltered authoritative collection.
    const projectID = this.projectIDForSession(sessionID);
    try {
      await this.api.deleteSession(sessionID);
      return await this.awaitRemovalConfirmation(sessionID);
    } catch (err) {
      // A transport/handler rejection can race a durable retirement. Confirm
      // the authoritative collection before showing an error so a row that is
      // already gone never produces a false failure flash.
      const confirmed = await this.confirmRejectedRemoval(sessionID, projectID);
      if (confirmed) return true;
      const message = err instanceof Error ? err.message : String(err);
      this.error = message;
      this.cb.onError?.(message, err);
      return false;
    }
  }

  // create launches a session through the real endpoint, then refreshes
  // so the active list shows the server's verified-running truth. A rejection (a 409
  // provider_not_launchable, a dead backend) is NOT swallowed: the
  // user gets the server's message, in-band.
  async create(projectID: string, provider: string): Promise<"created" | "identity_required" | "failed"> {
    // Session creation is non-idempotent: a double-click must not launch two
    // native CLI processes or race which response becomes selected.
    if (this.createSessionInFlight) return "failed";
    this.createSessionInFlight = true;
    try {
      const s = await this.api.createSession(projectID, provider);
      this.error = null;
      // Signal creation only after the refresh resolves. The launching
      // dialog closes and restores focus to its trigger on the render
      // that follows this promise; if selection started before that, the
      // terminal could attach and take focus first, and the restore would
      // steal it straight back off the input the user was promised.
      // Ordering, not timers, keeps the restore first and the terminal
      // last.
      const created = isVerifiedRunningSession(s);
      try {
        await this.refresh();
      } finally {
        if (created) this.cb.onCreateSession?.(s.id, s.provider === "codex" && s.codex_login_helper === true);
      }
      return "created";
    } catch (err) {
      if (err instanceof ApiError && err.code === "git_identity_required") {
        this.error = null;
        return "identity_required";
      }
      const message = err instanceof Error ? err.message : String(err);
      this.error = message;
      this.cb.onError?.(message, err);
      return "failed";
    } finally {
      this.createSessionInFlight = false;
    }
  }

  // renameProject records the user's display name for a project row,
  // then refreshes so the list shows the server's truth. The
  // name is display metadata only — the directory on disk never changes;
  // an empty name resets to the directory basename. Unlike removeProject,
  // the callback fires AFTER the refresh: its consumer repaints chrome
  // from the refreshed groups, not the stale ones. Failures surface like
  // rename's: the server's message, in-band, never swallowed.
  async renameProject(projectID: string, name: string): Promise<boolean> {
    try {
      await this.api.renameProject(projectID, name);
      this.error = null;
      await this.refresh();
      this.cb.onRenameProject?.(projectID);
      return true;
    } catch (err) {
      const message = err instanceof Error ? err.message : String(err);
      this.error = message;
      this.cb.onError?.(message, err);
      return false;
    }
  }

  // removeProject unregisters a project through the real endpoint — a
  // soft removal; the server never touches the directory on disk, and
  // stops the project's live sessions as part of removal. The callback
  // fires BEFORE the refresh so the app shell
  // can still see which sessions the group held; a failure (a session
  // the server could not stop, or the concurrent-launch backstop) is
  // NOT swallowed — the user gets the server's message, in-band,
  // and the callback never fires.
  async removeProject(projectID: string): Promise<boolean> {
    try {
      await this.api.deleteProject(projectID);
      this.cb.onRemoveProject?.(projectID);
      this.error = null;
      await this.refresh();
      return true;
    } catch (err) {
      const message = err instanceof Error ? err.message : String(err);
      this.error = message;
      this.cb.onError?.(message, err);
      return false;
    }
  }

  // Project registration is an explicit user action. The backend
  // canonicalizes and enforces the configured root allowlist; the browser
  // neither guesses nor rewrites the path.
  async createProject(
    name: string,
    path: string,
    profile: GitIdentityProfile | null,
    options: CreateProjectOptions = {},
  ): Promise<Project | null> {
    this.identityNotice = null;
    try {
      const project = await this.api.createProject(name, path, profile, options);
      this.error = null;
      this.identityNotice = replacedIdentityNotice(project.replaced_git_identity);
      await this.refresh();
      this.cb.onCreateProject?.(project.id);
      return project;
    } catch (err) {
      const message = err instanceof Error ? err.message : String(err);
      this.error = message;
      this.cb.onError?.(message, err);
      return null;
    }
  }

  // announceCreatedProject tells the workspace a project exists that this
  // model did not create through createProject — a finished clone
  // registered by the server — so it is selected the way a new one is.
  announceCreatedProject(projectID: string): void {
    this.cb.onCreateProject?.(projectID);
  }

  async createGitIdentity(name: string, email: string): Promise<GitIdentityProfile | null> {
    let created: GitIdentityProfile | null = null;
    const saved = await this.identityMutation(async () => { created = await this.api.createGitIdentity(name, email); });
    return saved ? created : null;
  }

  async updateGitIdentity(profile: GitIdentityProfile, name: string, email: string): Promise<boolean> {
    return this.identityMutation(async () => {
      await this.api.updateGitIdentity(profile.id, profile.revision, name, email);
    });
  }

  async setDefaultGitIdentity(profile: GitIdentityProfile): Promise<boolean> {
    const catalog = this.gitIdentities;
    if (catalog === null) return false;
    return this.identityMutation(async () => {
      await this.api.setDefaultGitIdentity(profile.id, catalog.revision, profile.revision);
    });
  }

  async deleteGitIdentity(profile: GitIdentityProfile, replacement?: GitIdentityProfile): Promise<boolean> {
    const catalog = this.gitIdentities;
    if (catalog === null) return false;
    return this.identityMutation(async () => {
      await this.api.deleteGitIdentity(profile.id, profile.revision, catalog.revision, replacement);
    });
  }

  async setupProjectGitIdentity(
    project: Project,
    profile: GitIdentityProfile,
  ): Promise<Project | null> {
    this.identityNotice = null;
    try {
      const updated = await this.api.setProjectGitIdentity(project.id, {
        profile_id: profile.id,
        expected_profile_revision: profile.revision,
        expected_project_setup_revision: project.git_identity_setup_revision ?? 0,
      });
      this.error = null;
      this.identityNotice = replacedIdentityNotice(updated.replaced_git_identity);
      await this.refresh();
      return updated;
    } catch (err) {
      const message = err instanceof ApiError && err.code === "revision_conflict"
        ? "Git identity changed in another window. Review the current values and try again."
        : err instanceof Error ? err.message : String(err);
      await this.refresh();
      this.error = message;
      this.cb.onError?.(message, err);
      return null;
    }
  }

  private async identityMutation(action: () => Promise<void>): Promise<boolean> {
    try {
      await action();
      this.error = null;
      await this.refresh();
      return true;
    } catch (err) {
      const message = err instanceof ApiError && err.code === "revision_conflict"
        ? "Git identities changed in another window. Review the current values and try again."
        : err instanceof Error ? err.message : String(err);
      await this.refresh();
      this.error = message;
      this.cb.onError?.(message, err);
      return false;
    }
  }

  async browseProjectDirectories(path?: string): Promise<ProjectDirectoryListing | null> {
    try {
      const listing = await this.api.listProjectDirectories(path);
      this.error = null;
      return listing;
    } catch (err) {
      const message = err instanceof Error ? err.message : String(err);
      this.error = message;
      this.cb.onError?.(message, err);
      return null;
    }
  }

  // previewFailed lands the Open preview flow's refusal in the same error
  // band every rejected action uses. The DOM shell owns the popup (a
  // gesture cannot survive being awaited into the model), so it hands the
  // sentence here; no refresh follows because the preview entry touches
  // no sidebar state.
  previewFailed(message: string, cause?: unknown): void {
    this.error = message;
    this.cb.onError?.(message, cause);
  }
}

// escapeHtml lives in ./escape.ts now; re-exported here so existing
// importers (the DOM shell, tests) keep working.
export { escapeHtml } from "../../lib/escape.ts";

// Below Tailwind's md breakpoint the sidebar overlays the workspace, so it
// starts closed rather than covering the terminal.
export const MOBILE_QUERY = "(max-width: 767px)";

// startsCollapsed decides the sidebar's initial state. Pure so the rule
// is testable without a viewport: narrow means closed, wide means open.
export function startsCollapsed(isNarrow: boolean): boolean {
  return isNarrow;
}

// dismissOnSelect reports whether picking a session should close the
// sidebar: on the overlay it covers the terminal the selection just
// switched to, so leaving it open would hide the result of the tap.
export function dismissOnSelect(isNarrow: boolean): boolean {
  return isNarrow;
}

// autoDescendPath answers: should the folder picker skip this listing?
// The mount listing (current_path "") exists to choose between allowed
// roots. With exactly one root there is nothing to choose, and making
// the user click through it is a step that carries no information.
// Returns the path to descend into, or null to
// show the listing as-is — so a multi-root deployment still gets the
// chooser.
export function autoDescendPath(listing: ProjectDirectoryListing): string | null {
  if (listing.current_path !== "") return null;
  if (listing.directories.length !== 1) return null;
  return listing.directories[0]!.path;
}
