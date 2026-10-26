// Production workspace composition. The component models remain deliberately
// small and server-backed; this layer only coordinates their lifetimes and the
// browser-owned WebSocket/controller lease. It never interprets terminal
// output or invents provider/session state.
import { ConversationAcknowledgements } from "../terminal/conversation-attention.ts";
import { csrfToken } from "../api/http.ts";
import { TutorialModel } from "./tutorial/model.ts";
import { TutorialPanel } from "./tutorial/panel.ts";
import { tutorialCatalog } from "./tutorial/lesson-files.ts";
import { tutorialStarterBundle } from "./tutorial/starter-files.ts";
import { TutorialStarterModel, starterCondition, type StarterProjectName } from "./tutorial/starter.ts";
import { runTutorialAction, type TutorialActionHost } from "./tutorial/actions.ts";
import { chatsFact, githubFact, type RequirementFacts } from "./tutorial/requirements.ts";
import { FactBook, type EventContext } from "./tutorial/events.ts";
import type { LessonCall } from "./tutorial/lessons.ts";
import { emptyStepContext, stepHidden, type StepContext } from "./tutorial/step-view.ts";
import { openProjectPreview } from "./project-preview.ts";
import { TutorialLanes } from "./tutorial/lane-actions.ts";
import type { ApiClient } from "../api/contracts.ts";
import type { ChatIndex } from "../api/chats.ts";
import type { ConnectionState, SessionDetail } from "../api/sessions.ts";
import type { ConsoleSettings } from "../api/settings.ts";
import type { FileApiClient } from "../api/files.ts";
import type { LaneApiClient } from "../api/tasks.ts";
import type { Project } from "../api/projects.ts";
import { ApiError, fetchWithAuthorization } from "../api/http.ts";
import type { AuthMode } from "../api/http.ts";
import type { SessionSummary } from "../api/sessions.ts";
import { escapeHtml } from "../lib/escape.ts";
import {
  isVerifiedRunningSession,
  SessionStatusModel,
} from "../ui/session-status.ts";
import type { SidebarModel as SidebarModelType } from "../panels/sidebar/sidebar.ts";
import { SidebarModel } from "../panels/sidebar/sidebar.ts";
import { Sidebar } from "../panels/sidebar/sidebar-dom.ts";
import { TaskmasterPanelModel } from "../panels/taskmaster/taskmaster-panel.ts";
import { TaskmasterPanel } from "../panels/taskmaster/taskmaster-panel-dom.ts";
import { FilesPanelModel } from "../panels/files/files-panel.ts";
import { FilesPanel } from "../panels/files/files-panel-dom.ts";
import {
  emptyPaneHtml,
  emptyTitlebarHtml,
  titlebarRightHtml,
  type EmptyPane,
  type LoadingPhase,
} from "../ui/empty-pane.ts";
import { providerDisplayName } from "../ui/session-identity.ts";
import { flashAssetsArrived, focusPanelTrigger, mountPanelsMenu, paintAssetsNew } from "../ui/panel-trigger.ts";
import { menuRows } from "../ui/dialog-focus.ts";
import { focusedModalControl, restoreModalFocus } from "../ui/modal-fields.ts";
import { SECONDARY_SM_BUTTON } from "../ui/controls.ts";
import { terminalIdentityHtml } from "../ui/terminal-identity.ts";
import { AgentTerminal } from "../terminal/attach.ts";
import { createTerminal, ensureMonoFontLoaded } from "../terminal/term.ts";
import { placeTerminalMenu, terminalMenuHtml } from "./terminal-menu.ts";
import { inertSoftKeyTerminal, MobileTerminalSoftKeys } from "../terminal/soft-keys.ts";
import { MobileTerminalCompose, isComposeSubmitIntent } from "../terminal/mobile-compose.ts";
import { mobileComposeMarkup } from "../terminal/mobile-compose-markup.ts";
import { TaskRefHover } from "../terminal/task-ref-hover.ts";
import { ChatsPanelModel, chatHandles, chatMarks } from "../panels/chats/chats-panel.ts";
import { ChatsPanel } from "../panels/chats/chats-panel-dom.ts";
import { AssetsPanelModel } from "../panels/assets/assets-panel.ts";
import { AssetsPanel } from "../panels/assets/assets-panel-dom.ts";
import { AssetsFeed, assetsNews, browserStorage } from "../panels/assets/assets-news.ts";
import { DockedPanels } from "./docked-panels.ts";
import {
  LastSessionMemory,
  type LastSessionStorage,
  pickInitialSession,
  readLastSession,
} from "./last-session.ts";
import { region, workspaceLayout } from "./layout.ts";
import { TerminalFocusRequest } from "./terminal-focus.ts";
import { InfoBar } from "./info-bar.ts";
import { PanelSize } from "../panels/panel-size.ts";
import { setAutoColorProjects, showAutoColorProjects } from "../lib/auto-color.ts";
import { trackVisualViewport } from "./viewport.ts";
import { shortcutCommand, shortcutTarget, type ShortcutCommand } from "./shortcuts.ts";
import { confirmsBeforeClose, confirmsBeforeRemove, sessionMenuHtml } from "./session-menu.ts";
import { decideReclaim } from "./reclaim.ts";
import { HarnessLoginDialog, harnessLoginLabel, type HarnessLoginListing } from "./harness-login.ts";
import { HarnessLoginCoordinator } from "./harness-login-coordinator.ts";
import { HttpGitHubConnectClient } from "../github-connect/client.ts";
import { GitHubConnectDialog } from "../github-connect/dialog.ts";
import { openPasswordChange } from "./password-form.ts";
import { readGitHubAccountIdentity } from "../github-repositories/client.ts";
import { ThemeDialog } from "./theme-dialog.ts";
import { bindLoginActions, loginHtml } from "./login.ts";
import { OutputActivityWindows } from "./output-activity.ts";
import {
  setTerminalFontSize,
  terminalFontSize,
} from "../terminal/font-size.ts";
import * as hints from "../lib/hints.ts";
import {
  customThemeSlots,
  resolveTerminalTheme,
  setCustomThemeSlots,
  setTerminalThemeName,
  terminalThemeName,
} from "../terminal/theme.ts";
import { type CustomThemeSlots } from "../terminal/themes.ts";
import {
  terminalChromeFor,
  terminalThemeFor,
} from "../terminal/provider-theme.ts";
import {
  applyTerminalChromeVars,
  customThemeChromeSlots,
  terminalChromePalette,
} from "../terminal/palette.ts";
import {
  RECOVERY_MONO,
  recoveryFromSession,
  recoveryBannerHtml,
  recoveryRegisterHtml,
  recoveryStreamHtml,
} from "./recovery.ts";

const statusRefreshMS = 3000;
const sidebarRefreshMS = 10000;
const loadingSlowAfterMS = 2000;
const loadingFailedAfterMS = 10000;
const requirementFactTimeoutMS = 8000;

type AttachmentProblem = {
  kind: "takeover" | "fenced" | "leaseExpired" | "dropped" | "error" | "closed";
  message: string;
};

export class LiveWorkspace {
  private root: HTMLElement;
  private api: ApiClient & FileApiClient & LaneApiClient;
  private sidebarModel: SidebarModelType;
  private sidebar: Sidebar;
  private terminal: AgentTerminal | null = null;
  private loginCoordinator!: HarnessLoginCoordinator;
  private githubConnect: GitHubConnectDialog | null = null;
  private harnessLogin: HarnessLoginDialog | null = null;
  private codexLoginHelpers = new Set<string>();
  private dismissedCodexHelpers = new Set<string>();
  private offeredCodexHelpers = new Set<string>();
  private loginEvents: EventSource | null = null;
  private terminalSoftKeys: MobileTerminalSoftKeys | null = null;
  private terminalCompose: MobileTerminalCompose | null = null;
  private composeDrafts = new Map<string, string>();
  // Whether each session routes a plain drag to the browser (the terminal
  // menu's Select text row). Per session, like the compose drafts, so a
  // remount or a reselection restores the choice the operator made.
  private selectTextSessions = new Map<string, boolean>();
  private dockBoundaryCleanup: (() => void) | null = null;
  // What the pane is showing, and what the dock is currently mounted for.
  // They are compared rather than assumed: the dock outlives any one terminal,
  // and the width can cross the phone boundary at any point in between.
  private dockMode: "live" | "recovery" | "none" = "none";
  private dockMounted: "live" | "recovery" | null = null;
  private dockSource: AgentTerminal | null = null;
  private taskRefHover: TaskRefHover | null = null;
  private statusModel: SessionStatusModel | null = null;
  private taskmasterModel: TaskmasterPanelModel | null = null;
  private taskmaster: TaskmasterPanel | null = null;
  // taskmasterEvents is the SSE subscription that refreshes the task board
  // when the engine's content-free feed advances, so a CLI edit is reflected
  // without a manual reload.
  private taskmasterEvents: EventSource | null = null;
  private filesModel: FilesPanelModel | null = null;
  private files: FilesPanel | null = null;
  private filesProjectID: string | null = null;
  // The Chats ledger is scoped to the selected session's agent;
  // it mounts with a session and leaves with it.
  private chatsModel: ChatsPanelModel | null = null;
  private chats: ChatsPanel | null = null;
  private assets: AssetsPanel | null = null;
  // The Assets count follows the selected session's project while the
  // panel is closed; the panel's own feed takes over while it is open.
  private assetsFeed: AssetsFeed;
  private chatsEvents: EventSource | null = null;
  // chatSeen records, per session and chat, the last row the user had
  // in front of them, so the sidebar mark and the trigger agree on unseen.
  private chatSeen = new Map<string, Map<string, string | null>>();
  private chatIndex: ChatIndex | null = null;
  private chatIndexTimer: number | null = null;
  private selectedSessionID: string | null = null;
  private conversationAcks: ConversationAcknowledgements | null = null;
  private connection: ConnectionState = "disconnected";
  private attachmentProblem: AttachmentProblem | null = null;
  private statusTimer: number | null = null;
  private sidebarTimer: number | null = null;
  // The loading pane changes what it says with time:
  // "Reconnecting…" for two seconds, then "Still reaching
  // the runtime", then after ten seconds the error state with its retry,
  // whether or not the read has come back. These repaint it on that clock.
  private loadingTimers: number[] = [];
  private loadingSince = 0;
  // lastSession keeps this browser's memory of the last opened session in
  // step with the server's, so a phone opens what the desktop was looking
  // at and the other way round.
  private readonly lastSession: LastSessionMemory;
  private epoch = 0;
  // Files and Tasks dock into the same content row, so which one is open
  // is ONE value, not two booleans that could both be true.
  private panels: DockedPanels;
  private tutorial: TutorialModel;
  private tutorialPanel: TutorialPanel | null = null;
  private starter: TutorialStarterModel;
  private tutorialLanes: TutorialLanes;
  // The registered projects as of the last starter observation, so the
  // manifest is read again only when the set changes.
  private starterProjects: string | null = null;
  private lastStarterMessage: string | null = null;
  private tutorialFocusRequested = false;
  // The agent the user picked through the tutorial's own buttons.
  private tutorialAgent: string | null = null;
  private tutorialPickArmed = false;
  private tutorialShellBusy = false;
  private tutorialPromptBusy = false;
  private selectionEpoch = 0;
  // Facts the tutorial's steps wait on, noted as they happen.
  private readonly tutorialFacts = new FactBook();
  // The session the tutorial started on Codex, which opens with a sign-in
  // helper. Codex is the only provider whose sign-in Deck sees finish.
  private tutorialLogin: { session: string } | null = null;
  // The starter is being created: its session, and so whether a sign-in is
  // needed, is not known until the create call returns.
  private tutorialStarting = false;
  private tutorialHadPair = false;
  private tutorialTaskTimer: number | null = null;
  private taskmasterProject: string | null = null;
  // reclaimOnFocus reconnects the terminal when the window comes back.
  private reclaimOnFocus: (() => void) | null = null;
  // autoReclaims counts consecutive automatic attempts, so a server that
  // keeps refusing surfaces the button instead of spinning. Reset by any
  // successful connection.
  private autoReclaims = 0;
  // connectedAt is when the live attachment succeeded; lastConnectionMs
  // is how long the one that just ended lasted. The budget is earned by
  // LASTING, so the duration has to outlive the connection that earned it.
  private connectedAt: number | null = null;
  private lastConnectionMs: number | null = null;
  private untrackViewport: (() => void) | null = null;
  // The recovery presentation the terminal pane shows, so an unchanged poll
  // keeps its scroll position, in-flight actions and retained screen.
  private recoveryKey: string | null = null;
  private authRequired = false;
  private controllerID: string;
  private disposed = false;
  private started = false;
  private lifecycle = 0;
  private authMode: AuthMode;
  private readonly security?: { lock?(): void; authenticationLost(): void; manageMethods?(): void };
  // popoverOpen tracks which popover is anchored so a status refresh can
  // re-render its live content in place.
  private popoverOpen: "more" | "panels" | null = null;
  private panelsMenuCleanup: (() => void) | null = null;
  // Keep unchanged mounted controls connected across status/settings refreshes.
  private moreMenuRender: { html: string; node: HTMLElement } | null = null;
  // themeFlyoutOpen tracks the ⋯ menu's theme list across the
  // popover's in-place re-renders.
  private themeFlyoutOpen = false;
  // skipPermissions mirrors the console setting; null means "not read
  // yet" (or the last read/write failed), which renders the switch
  // disabled rather than showing a guessed state.
  private skipPermissions: boolean | null = null;
  // confirmingYolo gates ENABLING the permission-skipping mode behind a
  // second, deliberate click. Disabling is immediate.
  private confirmingYolo = false;
  // skipCloseConfirm mirrors the console setting that drops the
  // close-a-session confirmation; null means not read yet. The sidebar
  // owns the prompt, so this is pushed to it rather than read there.
  private skipCloseConfirm: boolean | null = null;
  private skipRemoveProjectConfirm: boolean | null = null;
  // autoColorProjects is the rainbow toggle: a local preference,
  // no backend setting, so it is read once here and toggled in place.
  private autoColorProjects = showAutoColorProjects();
  private outputActivityEvents: EventSource | null = null;
  private outputActivity: OutputActivityWindows;
  // scrollbackActive tracks the pane's copy-mode view as the CLIENT knows
  // it: set when a scrollback command was forwarded, cleared
  // when user input exits the view server-side. An approximation —
  // tmux also leaves copy-mode by scrolling to the bottom, which the
  // client cannot observe, so the pill can outlive the view until the
  // next keypress.
  private scrollbackActive = false;
  // suppressAutoSelect is set when the user deletes the selected
  // session: with nothing selected the poll would otherwise grab the
  // first session and bury the clean empty state. Cleared by
  // the next explicit selection.
  private suppressAutoSelect = false;
  // focusRequest carries a user session selection to the terminal
  // attach handshake; focus moves only once the terminal is ready and the
  // current focus owner allows it.
  private focusRequest = new TerminalFocusRequest();
  // emptyPanelsProject names the project the no-session state mounted its
  // Files/Tasks panels for, so the sidebar poll's repaints reuse them
  // instead of rebuilding a fresh EventSource per refresh.
  private emptyPanelsProject: string | null = null;
  private readonly lastSessionStore: LastSessionStorage;
  private infoBar: InfoBar | null = null;
  private panelSizes: PanelSize[] = [];

  // Right-click over the pane opens the terminal's own Copy/Paste menu.
  // It is registered on the workspace root for the workspace's
  // whole life, rather than on the pane: mountTerminal rewrites the pane's
  // contents on every selection, and a listener re-added there would stack
  // one copy per mount.
  private readonly onTerminalContextMenu = (event: MouseEvent): void => {
    if (this.disposed || this.terminal === null) return;
    const target = event.target as Element | null;
    if (target?.closest?.('[data-region="terminal"]') == null) return;
    event.preventDefault();
    this.openTerminalMenu(this.terminal, event.clientX, event.clientY);
  };

  // The four app chords ride ONE capture-phase window listener,
  // installed before any component can see the key: ⌘R must be claimed
  // ahead of both the browser's reload and the terminal's own key
  // handling, and capture is the only phase where that order is
  // guaranteed.
  private readonly onShortcutKey = (event: KeyboardEvent): void => {
    if (this.disposed) return;
    const command = shortcutCommand(event, shortcutTarget(event.target));
    if (command === null) return;
    // ⌘R is reload to every browser user; the chord is ours only because
    // the page claims it here, and the menus print it only while this
    // intercept exists.
    event.preventDefault();
    event.stopPropagation();
    this.runShortcut(command);
  };

  constructor(
    root: HTMLElement,
    api: ApiClient & FileApiClient & LaneApiClient,
    controllerID: string,
    authMode: AuthMode = "builtin",
    lastSessionStore: LastSessionStorage,
    security?: { lock?(): void; authenticationLost(): void; manageMethods?(): void },
  ) {
    this.root = root;
    this.api = api;
    this.controllerID = controllerID;
    this.authMode = authMode;
    this.security = security;
    this.lastSessionStore = lastSessionStore;
    this.lastSession = new LastSessionMemory(lastSessionStore, {
      read: () => api.getLastSession(),
      write: (id) => api.saveLastSession(id),
    });
    // The phone boundary is a live condition, not a fact about any one
    // session: a rotation or a resized window crosses it whether the pane is
    // showing a terminal, a recovery presentation or nothing. The listener
    // therefore belongs to the workspace — hung off a terminal it would leak
    // one closure per recovery cycle, and stop firing on the surfaces that
    // have no terminal at all.
    const boundary = window.matchMedia("(max-width: 600px)");
    const onBoundary = (): void => this.syncDock();
    boundary.addEventListener("change", onBoundary);
    this.dockBoundaryCleanup = () => boundary.removeEventListener("change", onBoundary);
    // Load console settings eagerly so the close-confirmation and
    // permission-skipping choices apply from the first interaction, not
    // only after the More menu is opened (otherwise closing a session
    // before opening that menu ignores a saved "skip" and still asks).
    void this.api.getSettings().then((s) => {
      if (this.disposed) return;
      this.skipPermissions = s.skip_permissions;
      this.setSkipCloseConfirm(s.skip_close_confirm);
      this.setSkipRemoveProjectConfirm(s.skip_remove_project_confirm);
    }).catch((cause) => this.handleError("settings read failed", cause));
    root.innerHTML = workspaceLayout();
    // The info bar is the single explanation channel: hover and focus hints
    // only. Nothing renders at rest, so a stale session's line cannot linger
    // after the selection that produced it has gone.
    this.infoBar = new InfoBar(root);
    this.assetsFeed = new AssetsFeed(this.api, browserStorage, (arrived) => {
      this.paintAssetFacts();
      if (arrived) flashAssetsArrived(this.root, "live");
    });
    // Opening a ready panel reads canonical state; closing Files resets
    // its viewer. Both live here so the two panels cannot disagree about
    // which one is showing.
    this.panels = new DockedPanels(
      (name) => this.root.querySelector<HTMLElement>(`[data-region="${name}"]`),
      {
        onOpen: (name) => {
          if (name === "files") void this.files?.open();
          else if (name === "chats") void this.chats?.open().then(() => this.repaintChatFacts());
          else if (name === "assets") {
            this.assetsFeed.pause();
            void this.assets?.open();
          }
          else void this.taskmaster?.open();
        },
        onClose: (name) => {
          if (name === "files") this.files?.close();
          else if (name === "chats") {
            this.chats?.close();
            this.repaintChatFacts();
          } else if (name === "assets") {
            this.assets?.close();
            this.assetsFeed.resume();
          }
          else this.taskmaster?.close();
        },
        onChange: () => { this.repaintPanelToggles(); this.repaintTutorial(); },
      },
    );

    // The docked panels carry the shared resizing controller, one per
    // panel: each owns its saved width, drag boundary and keyboard values,
    // and their lifetime is the workspace's.
    for (const [region, key] of [
      ["files", "noisedeck.panel.files.width"],
      ["tasks", "noisedeck.panel.tasks.width"],
      ["chats", "noisedeck.panel.chats.width"],
      ["assets", "noisedeck.panel.assets.width"],
    ] as const) {
      const host = this.root.querySelector<HTMLElement>(`[data-region="${region}"]`);
      const handle = this.root.querySelector<HTMLElement>(`[data-panel-resize="${region}"]`);
      if (host === null || handle === null) continue;
      this.panelSizes.push(new PanelSize(host, handle, {
        key, min: 400, max: 1000, reserve: 360,
        isEnabled: () => host.dataset.expanded !== "true",
      }));
    }
    this.starter = new TutorialStarterModel(api, api, tutorialStarterBundle, {
      onChange: () => this.starterChanged(),
    });
    this.tutorialLanes = new TutorialLanes({
      api,
      say: (message) => this.tutorialPanel?.say(message),
      tasks: () => this.taskmasterModel === null || this.taskmasterProject === null
        ? null
        : { project: this.taskmasterProject, tags: this.taskmasterModel.tags },
      agent: () => this.tutorialAgent,
      sessions: (projectID) => this.sidebarModel.groups.flatMap((group) => group.sessions)
        .filter((session) => session.project_id === projectID && session.provider !== "shell"),
      attached: () => {
        const session = this.statusModel?.session;
        return session && session.id === this.selectedSessionID && this.connection === "connected" && this.terminal !== null ? session : null;
      },
      refreshSidebar: () => this.refreshSidebar(),
      openSession: async (session) => {
        this.noteTutorialLogin(session);
        await this.tutorialOpenSession(session.id);
      },
      chooseAgentIn: (projectID) => this.sidebar.openNewSession(projectID),
      chooseAgent: (then) => this.tutorialChooseAgent(then),
      type: (text, sent) => this.tutorialType(text, false, (message) => this.tutorialPanel?.say(message), sent),
      refreshTasks: async () => {
        await this.taskmasterModel?.refresh();
        if (this.taskmasterProject !== null) this.tutorialNoteTasks(this.taskmasterProject);
      },
      changed: () => { this.repaintTutorial(); this.syncTutorial(); },
    });
    this.tutorialFacts.current = (call, context) => this.tutorialHolds(call, context);
    this.tutorial = new TutorialModel(api, tutorialCatalog, {
      onChange: () => this.repaintTutorial(),
      facts: this.tutorialFacts,
      hidden: (step) => stepHidden(step, { ...emptyStepContext, ...this.tutorialContext() }),
      openPanel: (panel) => {
        if (this.disposed || this.tutorialPanel?.isMobile() === true || this.panels.isOpen(panel)) return;
        this.panels.set(panel, false);
      },
    });
    this.tutorialPanel = new TutorialPanel(root, this.tutorial, {
      sharedPreferences: authMode === "none",
      context: () => this.tutorialContext(),
      requirementFacts: () => this.requirementFacts(),
      onLessonsOpen: () => { void this.readRequirementFacts(); },
      onAction: (action, text, arg) => { runTutorialAction(this.tutorialActions(), action, text, arg); },
      showStep: () => {
        if (this.tutorial.state.phase !== "ready") return;
        this.tutorialFocusRequested = true;
        this.panels.close();
        this.tutorial.expand();
      },
      onClose: () => this.root.querySelector<HTMLElement>("[data-live-more]")?.focus(),
    });
    // The last served list is cached under the same storage as the last
    // session, so a cold load draws the previous visit's rail dimmed
    // instead of an empty one.
    this.sidebarModel = new SidebarModel(api, {
      onCreateProject: () => {
        if (!this.disposed && this.selectedSessionID === null) this.renderEmptyPane();
      },
      // Persist only at user-gesture callbacks. selectSession also serves
      // startup and polling, which must not overwrite the preference.
      onSelectSession: (id, keyboard) => {
        this.rememberSession(id);
        // A click hands the user to the terminal; Enter on a focused
        // row keeps the keyboard in the sidebar so the next arrow still
        // moves the list.
        if (!keyboard) this.focusRequest.request();
        void this.selectSession(id);
      },
      onDeselectSession: (id) => {
        if (this.selectedSessionID !== id) return;
        this.suppressAutoSelect = true;
        this.clearSelection();
      },
      onCreateSession: (id, codexLoginHelper) => {
        if (codexLoginHelper) this.codexLoginHelpers.add(id);
        this.noteTutorialAgent(id);
        const created = this.sidebarModel.groups.flatMap((group) => group.sessions).find((session) => session.id === id);
        if (created !== undefined) this.tutorialLanes.noteCreated(created);
        this.rememberSession(id);
        // Launching a session hands the user to it: the picker is
        // closing, so focus belongs in the new terminal, not on the
        // button that launched it. The attach handshake still applies
        // the guard, so a user who focused an input mid-launch, a
        // phone's soft keyboard, and a still-open dialog all keep focus.
        this.focusRequest.request();
        void this.selectSession(id);
      },
      onRenameSession: (id) => {
        if (this.selectedSessionID === id) void this.refreshTitleAfterRename(id);
      },
      onRenameProject: (id) => {
        // Fires AFTER the model refresh, so the titlebar
        // subtitle — which names the selected session's project from the
        // sidebar's groups — repaints from the renamed truth.
        const detail = this.statusModel?.session;
        if (detail !== null && detail !== undefined && detail.project_id === id) {
          this.renderChrome(detail);
        }
      },
      onRemoveProject: (id) => {
        // Fires before the model refresh, so the group (and its session
        // ids) is still visible here. Removing the selected session's
        // project deselects it exactly like removing the session itself:
        // teardown, and no auto-select of a neighbor.
        const group = this.sidebarModel.groups.find((g) => g.project.id === id);
        if (group?.sessions.some((s) => s.id === this.selectedSessionID)) {
          this.suppressAutoSelect = true;
          this.clearSelection();
        }
      },
      onRemoveSession: (id) => {
        if (this.selectedSessionID === id) {
          // An explicit delete is a deselect: the poll must not
          // auto-select a neighbor behind the user's back.
          this.suppressAutoSelect = true;
          this.clearSelection();
        }
      },
      onProvidersChanged: () => { if (!this.disposed) this.sidebar.renderNewSessionPicker(); },
      onDefaultIdentityChanged: () => { if (!this.disposed) void this.starter.observe(); },
      onError: (message, cause) => this.handleError(message, cause),
    }, this.autoColorProjects, lastSessionStore);
    this.sidebar = new Sidebar(region(root, "sidebar"), this.sidebarModel, undefined, (onClosed) => this.openGitHubConnect(onClosed), {
      githubEnabled: this.authMode !== "none",
      onNotice: (sentence) => this.infoBar?.announce(sentence),
      githubIdentity: () => this.authMode === "none" ? Promise.resolve(null)
        : readGitHubAccountIdentity(AbortSignal.timeout(15000), () => this.requireAuthentication()),
      githubStatus: () => this.authMode === "none" ? Promise.resolve({ outcome: "authentication-required" })
        : new HttpGitHubConnectClient(() => this.requireAuthentication()).status(AbortSignal.timeout(15000)),
      onAuthorizationLost: () => this.requireAuthentication(),
      tutorialUnavailableReason: () => {
        if (this.tutorial.state.phase === "loading") return "The tutorial is still loading. Try again in a moment.";
        if (this.disposed || this.authRequired || this.tutorial.state.phase !== "ready") return "The tutorial is unavailable right now. Try again after reconnecting.";
        return null;
      },
      onTutorial: () => {
        if (this.disposed || this.authRequired || this.tutorial.state.phase !== "ready") return;
        this.tutorialFocusRequested = true;
        this.panels.close();
        this.tutorial.selectLesson("lesson-1");
        this.tutorial.reopen();
      },
      onOpenProject: (id, source) => {
        if (this.disposed || this.authRequired || !this.sidebarModel.groups.some(group => group.project.id === id)) return;
        if (source === "github") this.noteTutorialFact([{ name: "project_opened", arg: "source=github" }]);
        ++this.epoch;
        this.suppressAutoSelect = true;
        this.sidebar.setProjectContext(id);
        this.clearSelection();
        this.sidebar.render();
        if (!this.panels.isOpen("tasks")) this.panels.toggle("tasks");
      },
    });
    this.outputActivity = new OutputActivityWindows(
      () => this.sidebarModel.groups.flatMap((group) => group.sessions),
      (id, active) => this.sidebar.setStreaming(id, active),
    );
    // Registered after the layout exists and removed in dispose(), so
    // the chords are live exactly as long as the workspace is.
    window.addEventListener("keydown", this.onShortcutKey, { capture: true });
    root.addEventListener("contextmenu", this.onTerminalContextMenu);
    // A backgrounded tab stops draining its socket, so the proxy drops it
    // to keep the CLI unblocked. The user did not choose
    // that, so coming back reconnects rather than asking.
    this.conversationAcks = new ConversationAcknowledgements({
      current: () => this.sidebarModel.groups.flatMap(group => group.sessions).find(session => session.id === this.selectedSessionID),
      visible: () => {
        if (document.visibilityState !== "visible" || !document.hasFocus() || this.root.querySelector("[role=dialog]") !== null) return false;
        const displayed = this.statusModel?.session;
        const selected = this.sidebarModel.groups.flatMap(group => group.sessions).find(session => session.id === this.selectedSessionID);
        if (!displayed || displayed.id !== selected?.id || displayed.generation !== selected.generation) return false;
        const terminal = this.root.querySelector<HTMLElement>("[data-live-terminal], [data-recovery-scrollback]");
        if (this.connection !== "connected" && terminal?.matches("[data-recovery-scrollback]") !== true) return false;
        if (!terminal) return false;
        const rect = terminal.getBoundingClientRect();
        return rect.width > 0 && rect.height > 0 && terminal.contains(document.elementFromPoint(rect.x + rect.width / 2, rect.y + rect.height / 2));
      },
      schedule: callback => requestAnimationFrame(() => requestAnimationFrame(callback)),
      acknowledge: async (id, fact) => {
        const token = csrfToken(document.cookie);
        const response = await fetchWithAuthorization(`/api/sessions/${encodeURIComponent(id)}/attention/seen`, {
          method: "POST", credentials: "same-origin", cache: "no-store", redirect: "error",
          headers: { "Content-Type": "application/json", ...(token ? { "X-CSRF-Token": token } : {}) },
          body: JSON.stringify({ generation: fact.generation, context_key: fact.context_key, ended_sequence: fact.ended_sequence }),
          signal: AbortSignal.timeout(5000),
        }, () => this.requireAuthentication());
        if (!response.ok) throw new Error("acknowledgement unavailable");
      },
      changed: () => { void this.refreshSidebar(); },
    });
    this.reclaimOnFocus = () => { this.maybeReclaim(); this.conversationAcks?.observe(); };
    document.addEventListener("visibilitychange", this.reclaimOnFocus);
    window.addEventListener("focus", this.reclaimOnFocus);
    // The frame follows the VISUAL viewport so a software keyboard
    // shrinks it instead of covering it.
    const frame = root.querySelector<HTMLElement>("[data-workspace]");
    if (frame !== null) this.untrackViewport = trackVisualViewport(frame);
    this.loginCoordinator = new HarnessLoginCoordinator({
      listing: (session, signal) => fetchWithAuthorization(`/api/harness-login?session=${encodeURIComponent(session)}`, {
        credentials: "same-origin", cache: "no-store", redirect: "error", referrerPolicy: "no-referrer", signal,
      }, this.security ? () => this.security?.authenticationLost() : undefined).then(async (response) => {
        if (!response.ok) throw new Error("harness login listing unavailable");
        return await response.json() as HarnessLoginListing;
      }),
      raise: (session, listing) => !this.dismissedCodexHelpers.has(session) && this.mountHarnessLogin(session, listing),
      // A raise must never cover what is already on screen: this dialog,
      // any dialog, or an open menu.
      occupied: () => this.harnessLogin !== null
        || this.popoverOpen !== null
        || this.root.querySelector("[role=dialog], [data-terminal-menu-popover]") !== null,
      session: () => this.selectedSessionID,
      attached: () => this.connection === "connected" && this.terminal !== null,
      visible: () => typeof document === "undefined" || document.visibilityState === "visible",
      now: () => Date.now(),
      changed: () => {
        if (!this.disposed && this.statusModel?.session) this.renderChrome(this.statusModel.session);
      },
    }, {
      getItem: (key) => window.sessionStorage.getItem(key),
      setItem: (key, value) => window.sessionStorage.setItem(key, value),
      removeItem: (key) => window.sessionStorage.removeItem(key),
    });
    this.beginLoadingClock();
    this.renderEmptyPane();
  }

  async start(): Promise<void> {
    if (this.started || this.disposed) return;
    this.started = true;
    this.tutorial.load();
    const lifecycle = this.lifecycle;
    // The rail paints before the first read answers: the cached list, or
    // flat bands at row height, so the first thing on screen is a shell
    // with the right shape rather than nothing.
    this.sidebar.render();
    this.mountOutputActivityEvents();
    this.loginCoordinator.start();
    // The poll starts before the first read answers, not after: a first
    // read that hangs would otherwise hold every retry the error pane
    // promises. A later poll supersedes a read still in flight through the
    // model's generation guard.
    this.sidebarTimer = window.setInterval(() => {
      void this.refreshSidebar();
    }, sidebarRefreshMS);
    // The server's memory is asked for alongside the first read; every
    // refresh then waits on that one answer before picking a session.
    void this.lastSession.adopt();
    await this.refreshSidebar();
    if (this.disposed || lifecycle !== this.lifecycle) return;
  }

  // rememberSession records a user's choice of session in both
  // memories. The server write is best effort: a browser that cannot reach
  // it still remembers locally, and the next successful read of the server
  // is what the other device will see.
  private rememberSession(id: string | null): void {
    this.lastSession.remember(id);
  }

  dispose(): void {
    if (this.disposed) return;
    window.removeEventListener("keydown", this.onShortcutKey, { capture: true });
    this.root.removeEventListener("contextmenu", this.onTerminalContextMenu);
    this.dockBoundaryCleanup?.();
    this.dockBoundaryCleanup = null;
    this.disposed = true;
    this.conversationAcks?.dispose();
    this.stopTutorialTimers();
    this.tutorial.dispose();
    this.starter.dispose();
    this.tutorialLanes.dispose();
    this.tutorialPanel?.dispose();
    this.tutorialPanel = null;
    ++this.lifecycle;
    ++this.epoch;
    this.disposeSession();
    this.assetsFeed.stop();
    this.loginCoordinator.dispose();
    this.untrackViewport?.();
    this.untrackViewport = null;
    this.githubConnect?.dispose();
    this.githubConnect = null;
    this.sidebar.dispose();
    if (this.reclaimOnFocus !== null) {
      document.removeEventListener("visibilitychange", this.reclaimOnFocus);
      window.removeEventListener("focus", this.reclaimOnFocus);
      this.reclaimOnFocus = null;
    }
    if (this.sidebarTimer !== null) {
      window.clearInterval(this.sidebarTimer);
      this.sidebarTimer = null;
    }
    this.endLoadingClock();
    this.outputActivityEvents?.close();
    this.outputActivityEvents = null;
    this.clearOutputActivity();
    this.infoBar?.dispose();
    for (const size of this.panelSizes) size.dispose();
    this.panelSizes = [];
    this.infoBar = null;
  }

  private beginLoadingClock(): void {
    this.endLoadingClock();
    this.loadingSince = Date.now();
    for (const delay of [loadingSlowAfterMS, loadingFailedAfterMS]) {
      this.loadingTimers.push(window.setTimeout(() => {
        if (!this.disposed && this.selectedSessionID === null) this.renderEmptyPane();
      }, delay));
    }
  }

  private endLoadingClock(): void {
    for (const timer of this.loadingTimers) window.clearTimeout(timer);
    this.loadingTimers = [];
  }

  // loadingPhase reads the clock the loading pane speaks by. A read that
  // has not answered in ten seconds is presented as failed — the user
  // gets the reassurance and the retry — even though the model is still
  // waiting on it.
  private loadingPhase(): LoadingPhase | "failed" {
    const elapsed = Date.now() - this.loadingSince;
    if (elapsed >= loadingFailedAfterMS) return "failed";
    return elapsed >= loadingSlowAfterMS ? "slow" : "reconnect";
  }

  // Remembers the agent a session created from the tutorial's pick button
  // runs, so lesson text can name it. Shell sessions are not agents.
  private noteTutorialAgent(sessionID: string): void {
    if (!this.tutorialPickArmed) return;
    this.tutorialPickArmed = false;
    const provider = this.sidebarModel.groups.flatMap((group) => group.sessions).find((session) => session.id === sessionID)?.provider;
    if (provider === undefined || provider === "shell") return;
    this.tutorialAgent = provider;
    this.repaintTutorial();
    this.syncTutorial();
  }

  private tutorialContext(): Partial<StepContext> {
    return {
      agent: this.tutorialAgent,
      relnotes: this.starter.state.pair?.relnotes.canonical_path ?? null,
      task: this.tutorialLanes.taskText(),
      primaryDone: this.tutorialPrimaryDone(),
      // Deck's review button always posts a comment: the formal review app is
      // not part of this image.
      holds: (call) => starterCondition(this.starter.state, call, {
        agent: this.tutorialAgent,
        agentNeedsLogin: this.tutorialLogin !== null,
        reviewAppAvailable: false,
      }),
    };
  }

  // Whether the current step's first action, starting the picked agent in a
  // practice project, has already happened: a live session of that agent
  // exists there.
  private tutorialPrimaryDone(): boolean {
    if (!this.tutorial) return false;
    const { lesson, step } = this.tutorial.state;
    const action = this.tutorial.catalog.find((entry) => entry.id === lesson)?.steps[step]?.action;
    const project = action?.name === "start-session" ? /^project=(relnotes|ccparse)$/.exec(action.arg ?? "")?.[1] as "relnotes" | "ccparse" | undefined : undefined;
    const pair = this.starter.state.pair;
    if (project === undefined || pair === null) return false;
    return this.liveAgentSessions(pair[project].id).some((session) => this.tutorialAgent === null || session.provider === this.tutorialAgent);
  }

  // The running agent sessions of a project: never the plain shell.
  private liveAgentSessions(projectID: string): SessionSummary[] {
    return this.sidebarModel.groups.flatMap((group) => group.sessions)
      .filter((session) => session.project_id === projectID && session.provider !== "shell"
        && (session.status === "running" || session.status === "starting"));
  }

  private installFacts: { chats: boolean | null; github: boolean | null; previews: boolean | null } = { chats: null, github: null, previews: null };
  private installFactsInFlight = false;

  private requirementFacts(): RequirementFacts {
    return { ...this.installFacts, chats: chatsFact(this.chatsModel?.state, this.installFacts.chats), hostname: window.location.hostname };
  }

  // Whether agent chat works, the GitHub connection and the preview setup
  // are read when the lesson list opens. Each fact lands as soon as its own
  // read settles; one that fails or stalls past its deadline is unknown, which never blocks.
  private async readRequirementFacts(): Promise<void> {
    if (this.disposed || this.installFactsInFlight) return;
    this.installFactsInFlight = true;
    const land = (fact: Partial<typeof this.installFacts>): void => {
      if (this.disposed) return;
      this.installFacts = { ...this.installFacts, ...fact };
      this.repaintTutorial();
    };
    const projectID = this.sidebarModel.groups[0]?.project.id;
    const reads: Promise<void>[] = [
      this.authMode === "none"
        ? Promise.resolve(land({ github: false }))
        : new HttpGitHubConnectClient().status(AbortSignal.timeout(15000)).then((status) => land({ github: githubFact(status.outcome) }), () => land({ github: null })),
    ];
    if (projectID === undefined || this.api.getProjectPreviewSettings === undefined) {
      land({ previews: null });
    } else {
      reads.push(this.api.getProjectPreviewSettings(projectID, AbortSignal.timeout(requirementFactTimeoutMS)).then((settings) => land({ previews: settings.configured }), () => land({ previews: null })));
    }
    if (this.api.getChatsStatus === undefined) {
      land({ chats: null });
    } else {
      reads.push(this.api.getChatsStatus(AbortSignal.timeout(requirementFactTimeoutMS)).then((status) => land({ chats: status.available }), () => land({ chats: null })));
    }
    try {
      await Promise.all(reads);
    } finally {
      this.installFactsInFlight = false;
    }
  }

  // Asks the tutorial to look at its current step again.
  private syncTutorial(): void {
    if (this.disposed || this.authRequired) return;
    this.tutorialFacts.sample(
      [{ name: "session_attached", arg: "project=relnotes" }, { name: "session_attached", arg: "project=ccparse" }],
      { lesson: "", step: 0, restored: false },
    );
    this.tutorial.factsChanged();
  }

  private noteTutorialFact(calls: LessonCall[], scope: { lesson: string; step: number } | null = null): void {
    this.tutorialFacts.note(scope, ...calls);
    this.syncTutorial();
  }

  private tutorialProjectName(projectID: string | undefined): "relnotes" | "ccparse" | null {
    const pair = this.starter.state.pair;
    if (pair === null || projectID === undefined) return null;
    return pair.relnotes.id === projectID ? "relnotes" : pair.ccparse.id === projectID ? "ccparse" : null;
  }

  private selectedSessionSummary() {
    return this.sidebarModel.groups.flatMap((group) => group.sessions).find((session) => session.id === this.selectedSessionID);
  }

  // Whether an event holds right now. Events that are noted once as they
  // happen are answered by the fact book instead.
  private tutorialHolds(call: LessonCall, context: EventContext): boolean {
    const state = this.starter.state;
    const session = this.statusModel?.session;
    switch (call.name) {
      case "agent_picked": return this.tutorialAgent !== null;
      case "starter_created": return state.pair !== null && (!this.tutorialStarting || this.tutorialHadPair);
      case "session_attached": {
        const project = /^project=(relnotes|ccparse)$/.exec(call.arg ?? "")?.[1] as "relnotes" | "ccparse" | undefined;
        return project !== undefined && state.pair !== null && session !== null && session !== undefined
          && session.project_id === state.pair[project].id && session.id === this.selectedSessionID
          && this.connection === "connected" && this.terminal !== null;
      }
      case "lane_session_attached": case "review_seen": return this.tutorialLanes.holds(call);
      case "task_exists": case "task_done": {
        const requirement = Number(/^requirement=(\d+)$/.exec(call.arg ?? "")?.[1] ?? 1);
        if (context.lesson === "lesson-1" || context.lesson === "lesson-2") {
          if (state.pair === null) return false;
          return call.name === "task_done" ? state.doneRequirements.has(1) : state.requirements.has(requirement);
        }
        const tags = this.taskmasterModel?.tags ?? [];
        const tasks = tags.flatMap(([, list]) => list);
        return call.name === "task_done" ? (this.tutorialLanes.taskDone() ?? tasks.some((task) => task.status === "done")) : tasks.length > 0;
      }
      case "chat_message_seen":
        return this.chatsModel?.state === "ready" && this.tutorialProjectName(session?.project_id) !== null
          && this.chatsModel.chats.some((chat) => chat.rows.some((row) => row.kind === "message"));
      case "page_reloaded": {
        if (!context.restored) return false;
        const live = this.selectedSessionSummary();
        return live !== undefined && live.status === "running" && live.presentation_state === "running";
      }
      default: return false;
    }
  }

  // The Tasks panel already follows the selected project's change stream, so
  // its reads feed the tutorial. Only when the practice project is not the
  // one selected does the tutorial read the list itself, every three
  // seconds, while the tab is visible and the step waits on a task.
  private tutorialNoteTasks(projectID: string): void {
    const model = this.taskmasterModel;
    if (model !== null && this.starter.state.pair?.relnotes.id === projectID) {
      this.starter.noteTasks(model.tags.flatMap(([, tasks]) => tasks));
    }
    void this.tutorialLanes.observeClosed();
    this.syncTutorial();
  }

  private updateTutorialTaskPoll(): void {
    const state = this.tutorial.state;
    const lesson = this.tutorial.catalog.find((entry) => entry.id === state.lesson);
    const waiting = lesson?.steps[state.step]?.advanceOn?.some((call) => call.name === "task_exists" || call.name === "task_done") === true;
    const relnotes = this.starter.state.pair?.relnotes.id ?? null;
    const covered = this.taskmasterEvents !== null && this.taskmasterProject === relnotes;
    const want = waiting && state.phase === "ready" && !state.dismissed && relnotes !== null && !covered && !this.disposed && !this.authRequired;
    if (!want) {
      if (this.tutorialTaskTimer !== null) window.clearInterval(this.tutorialTaskTimer);
      this.tutorialTaskTimer = null;
    } else if (this.tutorialTaskTimer === null) {
      this.tutorialTaskTimer = window.setInterval(() => {
        if (document.visibilityState === "visible") void this.starter.refreshTasks();
      }, 3000);
    }
  }

  private updateTutorialReviewPoll(): void {
    const state = this.tutorial.state;
    const lesson = this.tutorial.catalog.find((entry) => entry.id === state.lesson);
    const waiting = lesson?.steps[state.step]?.advanceOn?.some((call) => call.name === "review_seen") === true
      && state.phase === "ready" && !state.dismissed && !this.authRequired;
    this.tutorialLanes.syncReviewPoll(waiting);
  }

  private stopTutorialTimers(): void {
    if (this.tutorialTaskTimer !== null) window.clearInterval(this.tutorialTaskTimer);
    this.tutorialTaskTimer = null;
  }

  // Shows the starter model's latest outcome under the current step, once.
  private starterChanged(): void {
    this.repaintTutorial();
    this.syncTutorial();
    const text = this.starter.state.error ?? this.starter.state.notice;
    if (text === this.lastStarterMessage) return;
    this.lastStarterMessage = text;
    if (text !== null) this.tutorialPanel?.say(text);
  }

  private tutorialProjectID(): string | null {
    return this.statusModel?.session?.project_id ?? this.sidebar.newSessionTarget()?.id ?? null;
  }

  // Whether text of the wanted kind may be typed into the selected session now.
  private canTypeIntoSelected(text: string, shell: boolean): boolean {
    const session = this.statusModel?.session;
    return !(this.disposed || this.authRequired || this.connection !== "connected" || !this.terminal
      || !session || session.id !== this.selectedSessionID || (session.provider === "shell") !== shell
      || /[\u0000-\u001f\u007f-\u009f]/u.test(text));
  }

  private tutorialActions(): TutorialActionHost {
    const say = (message: string): void => this.tutorialPanel?.say(message);
    const openPicker = (): void => {
      const target = this.sidebar.newSessionTarget();
      if (target === null) {
        say("Add a project first, then choose an agent.");
        return;
      }
      this.tutorialPickArmed = true;
      this.sidebar.openNewSession(target.id);
    };
    const withProject = (use: (projectID: string) => void): void => {
      const projectID = this.tutorialProjectID();
      if (projectID === null) say("Add a project first.");
      else use(projectID);
    };
    return {
      say,
      chooseAgent: () => this.tutorialChooseAgent(),
      newSession: openPicker,
      addProject: () => this.sidebar.openNewProject(),
      gitIdentity: () => this.sidebar.openGitIdentitySettings(),
      insertPrompt: (text, target) => { void this.tutorialInsertPrompt(text, target, say); },
      runCommand: (text, target) => { void this.tutorialRunCommand(text, target, say); },
      uploadFiles: () => withProject(() => this.panels.set("files")),
      previewSettings: () => withProject((id) => { void this.sidebar.openProjectPreviewSettings(id); }),
      openPreview: () => withProject((id) => openProjectPreview(id, this.api, say)),
      showAddress: () => say(`Deck's address is ${window.location.origin}`),
      createStarter: () => {
        if (this.tutorialAgent !== null) {
          void this.tutorialCreateStarter();
          return;
        }
        say("Choose an agent first. Deck creates the projects as soon as you do.");
        this.tutorialChooseAgent(() => { void this.tutorialCreateStarter(); });
      },
      addPrd: () => { void this.starter.addPrd(); },
      generateTasks: (count) => {
        if (count !== null) {
          void this.starter.generateTasks(count, this.tutorialAgent ?? undefined);
          return;
        }
        const panel = this.tutorialOwnTasksPanel(say);
        if (panel === null) return;
        void panel.generateFromPrd(this.tutorialAgent ?? undefined).then((result) => {
          if (result.outcome === "failed") say(`Task generation did not start: ${result.error}`);
          else if (result.outcome === "missing-prd") say("This project has no PRD yet. Add one in the Tasks panel and Deck generates the tasks from it.");
          else say("Generating tasks from the project's PRD. They appear on the board when it finishes.");
        });
      },
      addTask: (requirement) => {
        if (requirement !== null) {
          void this.starter.addTask(requirement);
          return;
        }
        const panel = this.tutorialOwnTasksPanel(say);
        if (panel === null) return;
        void panel.openAddTask().then((opened) => {
          if (!opened) say("The Tasks panel could not open the Add task form. Try again in a moment.");
        });
      },
      startSession: (project) => { void this.tutorialStartSession(project, say); },
      startLane: () => { void this.tutorialLanes.start(); },
      requestReview: () => { void this.tutorialLanes.requestReview(); },
      markTaskDone: () => {
        const lesson = this.tutorial.state.lesson;
        if (lesson === "lesson-1" || lesson === "lesson-2") void this.starter.markRequirementDone(1);
        else void this.tutorialLanes.markDone();
      },
    };
  }

  // Opens the provider picker with no project. Choosing an agent only records
  // it; the next step starts the session.
  private tutorialChooseAgent(then?: () => void): void {
    this.sidebar.openAgentChoice((provider) => {
      if (this.disposed || this.authRequired) return;
      this.tutorialAgent = provider;
      if (then === undefined) this.tutorialPanel?.say(`Chose ${providerDisplayName(provider)}.`);
      this.repaintTutorial();
      this.syncTutorial();
      then?.();
    });
  }

  // The Tasks panel for a step about the user's own project, opened. Null,
  // with the reason said, when no project's tasks are showing or they are the
  // practice project's.
  private tutorialOwnTasksPanel(say: (message: string) => void): TaskmasterPanel | null {
    const panel = this.taskmaster;
    if (panel === null || this.taskmasterProject === null) {
      say("Select a session in your project first, so Deck knows whose tasks to use.");
      return null;
    }
    const practice = this.tutorialProjectName(this.taskmasterProject);
    if (practice !== null) {
      say(`The Tasks panel is showing the ${practice} practice project. Select a session in your own project first.`);
      return null;
    }
    if (!this.panels.isOpen("tasks")) this.panels.set("tasks", false);
    return panel;
  }

  // The project a prompt step is about and the agent whose session takes the
  // prompt, or null when that project does not exist yet.
  private tutorialPromptTarget(target: string): { projectID: string; name: string; provider: string | null } | null {
    if (target === "source") {
      const projectID = this.tutorialLanes.sourceProject()
        ?? (this.taskmasterProject !== null && this.tutorialProjectName(this.taskmasterProject) === null ? this.taskmasterProject : null);
      if (projectID === null) return null;
      const name = this.sidebarModel.groups.find((group) => group.project.id === projectID)?.project.name ?? "your project";
      return { projectID, name, provider: null };
    }
    if (target === "lane") {
      const lane = this.tutorialLanes.open();
      if (lane === null) return null;
      const name = this.sidebarModel.groups.find((group) => group.project.id === lane.lane_project_id)?.project.name ?? "the lane";
      return { projectID: lane.lane_project_id, name, provider: lane.agent };
    }
    const pair = this.starter.state.pair;
    if (pair === null || (target !== "relnotes" && target !== "ccparse")) return null;
    return { projectID: pair[target].id, name: target, provider: this.tutorialAgent };
  }

  // Types a step's prompt into the agent session of the project the step is
  // about, selecting that session first, and types nothing when the project
  // has no such session. Without a target it goes to the selected session.
  private async tutorialInsertPrompt(text: string, target: string | null, say: (message: string) => void): Promise<void> {
    const { lesson, step } = this.tutorial.state;
    // The prompt counts for this step only once Deck confirms it arrived.
    const sentFor = (projectID: string | undefined) => () => {
      const project = this.tutorialProjectName(projectID);
      this.noteTutorialFact([{ name: "input_sent", arg: null }, ...(project === null ? [] : [{ name: "input_sent", arg: `project=${project}` }])], { lesson, step });
    };
    if (target === null) {
      if (!this.tutorialType(text, false, say, sentFor(this.statusModel?.session?.project_id))) say("Select an attached agent session first.");
      return;
    }
    const wanted = this.tutorialPromptTarget(target);
    if (wanted === null) {
      say(target === "lane" ? "Start a lane first." : "Create the practice projects first.");
      return;
    }
    const candidates = this.liveAgentSessions(wanted.projectID).filter((session) => wanted.provider === null || session.provider === wanted.provider);
    const session = candidates.find((candidate) => candidate.id === this.selectedSessionID) ?? candidates[0];
    if (session === undefined) {
      say(`There is no ${wanted.provider === null ? "agent" : providerDisplayName(wanted.provider)} session in ${wanted.name} yet, so start one there first.`);
      return;
    }
    if (this.tutorialPromptBusy) {
      say("A session is already being selected.");
      return;
    }
    const sent = sentFor(session.project_id);
    if (this.selectedSessionID === session.id && this.tutorialType(text, false, say, sent)) return;
    const lifecycle = this.lifecycle;
    const stale = (expected: number): boolean => this.disposed || this.authRequired || lifecycle !== this.lifecycle || this.selectionEpoch !== expected;
    this.tutorialPromptBusy = true;
    try {
      const selecting = this.selectionEpoch + 1;
      this.rememberSession(session.id);
      await this.selectSession(session.id);
      const until = Date.now() + 5000;
      for (;;) {
        if (stale(selecting) || this.selectedSessionID !== session.id) return;
        if (this.tutorialType(text, false, say, sent)) return;
        if (Date.now() > until) {
          say(`The ${wanted.name} session did not attach in time. Choose the button again.`);
          return;
        }
        await new Promise((resolve) => setTimeout(resolve, 50));
      }
    } finally {
      this.tutorialPromptBusy = false;
    }
  }

  // Types text into the selected terminal as input, never Enter, and says
  // nothing about delivery until the server answers that very write. This
  // is the one place that decides a tutorial write was sent; `sent` runs
  // only then. Never retried.
  private tutorialType(text: string, shell: boolean, say: (message: string) => void, sent?: () => void): boolean {
    if (!this.canTypeIntoSelected(text, shell)) return false;
    const terminal = this.terminal!;
    const { lesson, step, visible } = this.tutorial.state;
    const focusedAtSend = document.activeElement;
    return terminal.sendTracked(text, (code) => {
      switch (code) {
        case "sent": {
          // Focus moves only while the step is still showing and you have not gone elsewhere.
          const now = this.tutorial.state;
          const stillHere = visible && now.visible && now.lesson === lesson && now.step === step
            && (document.activeElement === focusedAtSend || this.tutorialPanel?.focusIsFree() === true);
          if (!this.disposed && this.terminal === terminal && stillHere) terminal.mounted.focus();
          say("Press Return to send it.");
          sent?.();
          break;
        }
        case "failed": say("Nothing was typed. Choose the button again."); break;
        case "uncertain": say("Check the terminal for the text before you press Return."); break;
        default: say("The connection dropped. Check the terminal for the text before you press Return.");
      }
    });
  }

  // Opens a session the tutorial just started, the way choosing it in the
  // rail would.
  private async tutorialOpenSession(sessionID: string): Promise<void> {
    await this.refreshSidebar();
    if (this.disposed || this.authRequired) return;
    this.rememberSession(sessionID);
    this.focusRequest.request();
    // Selecting a Codex session that is already selected would tear it down,
    // and with it the sign-in helper its first attach just offered.
    if (this.selectedSessionID !== sessionID || !this.codexLoginHelpers.has(sessionID)) await this.selectSession(sessionID);
    this.repaintTutorial();
  }

  // A Codex session that opens with its sign-in helper makes the sign-in
  // step apply. For any other provider nothing tells Deck the sign-in
  // finished, so the step stays out of the lesson.
  private noteTutorialLogin(session: SessionSummary): void {
    if (session.provider !== "codex" || session.codex_login_helper !== true) return;
    this.codexLoginHelpers.add(session.id);
    this.tutorialLogin = { session: session.id };
  }

  // The listing re-checks each running Codex session on every read, so a
  // signed-in session loses its helper wherever the sign-in happened. A row
  // still starting has not been checked yet.
  private dropFinishedCodexHelpers(): void {
    let dropped = false;
    for (const group of this.sidebarModel.groups) {
      for (const session of group.sessions) {
        if (session.codex_login_helper === true || session.status === "starting" || !this.codexLoginHelpers.has(session.id)) continue;
        this.codexLoginHelpers.delete(session.id);
        this.offeredCodexHelpers.delete(session.id);
        this.dismissedCodexHelpers.delete(session.id);
        dropped = true;
      }
    }
    if (dropped && this.statusModel?.session) this.renderChrome(this.statusModel.session);
  }

  private async tutorialCreateStarter(): Promise<void> {
    // Only the call that opened the gate may change or release it.
    if (this.tutorialStarting) return;
    this.tutorialStarting = true;
    this.tutorialHadPair = this.starter.state.pair !== null;
    let created: Awaited<ReturnType<TutorialStarterModel["create"]>>;
    try {
      created = await this.starter.create(this.tutorialAgent);
      if (!this.disposed && !this.authRequired && created?.session) this.noteTutorialLogin(created.session);
    } finally {
      this.tutorialStarting = false;
    }
    this.syncTutorial();
    if (this.disposed || this.authRequired) return;
    if (created?.session) await this.tutorialOpenSession(created.session.id);
    else await this.refreshSidebar();
  }

  private async tutorialStartSession(project: string, say: (message: string) => void): Promise<void> {
    if (project !== "relnotes" && project !== "ccparse") {
      say("Deck can only start a session in the practice projects.");
      return;
    }
    if (this.tutorialAgent === null) {
      say("Choose an agent first. Deck starts it in " + project + " as soon as you do.");
      this.tutorialChooseAgent(() => { void this.tutorialStartSession(project, say); });
      return;
    }
    const session = await this.starter.startAgent(project as StarterProjectName, this.tutorialAgent);
    if (this.disposed || this.authRequired) return;
    if (session) this.noteTutorialLogin(session);
    if (session) await this.tutorialOpenSession(session.id);
  }

  // Types a command into a plain shell. With a target the shell is the one
  // of that project, opened or created and selected first; the selected
  // project is never a stand-in. Without one, the selected session's project.
  private async tutorialRunCommand(text: string, target: string | null, say: (message: string) => void): Promise<void> {
    if (this.tutorialShellBusy) {
      say("A terminal is already being opened.");
      return;
    }
    let projectID: string | null;
    if (target === null) {
      if (this.tutorialType(text, true, say)) return;
      projectID = this.tutorialProjectID();
      if (projectID === null) {
        say("Add a project first.");
        return;
      }
    } else {
      const wanted = this.tutorialPromptTarget(target);
      if (wanted === null) {
        say(target === "lane" ? "Start a lane first."
          : target === "source" ? "Select a session in your own project first."
            : "Create the practice projects first.");
        return;
      }
      projectID = wanted.projectID;
      if (this.statusModel?.session?.project_id === projectID && this.tutorialType(text, true, say)) return;
    }
    const existing = target === null ? undefined : this.sidebarModel.groups.flatMap((group) => group.sessions)
      .find((session) => session.project_id === projectID && session.provider === "shell" && (session.status === "running" || session.status === "starting"));
    // Any selection change while the shell is being made wins, including a
    // return to the session that was selected when the button was pressed:
    // the shell is left created and available, never selected or typed into.
    const lifecycle = this.lifecycle;
    const epoch = this.selectionEpoch;
    const stale = (expected: number): boolean => this.disposed || this.authRequired || lifecycle !== this.lifecycle || this.selectionEpoch !== expected;
    this.tutorialShellBusy = true;
    try {
      const created = existing ?? await this.api.createSession(projectID, "shell");
      if (stale(epoch)) return;
      if (existing === undefined) {
        await this.refreshSidebar();
        if (stale(epoch)) return;
      }
      const selecting = this.selectionEpoch + 1;
      await this.selectSession(created.id);
      const until = Date.now() + 5000;
      for (;;) {
        if (stale(selecting) || this.selectedSessionID !== created.id) return;
        if (this.tutorialType(text, true, say)) return;
        if (Date.now() > until) {
          say("The new terminal did not attach in time. Select it and choose the button again.");
          return;
        }
        await new Promise((resolve) => setTimeout(resolve, 50));
      }
    } catch (cause) {
      say(cause instanceof Error ? cause.message : String(cause));
    } finally {
      this.tutorialShellBusy = false;
    }
  }

  private repaintTutorial(): void {
    if (this.disposed || !this.tutorial) return;
    const state = this.tutorial.state;
    // A lost controller must not carry a pending focus request into reconnect.
    if (state.phase === "suspended" || state.phase === "disposed") this.tutorialFocusRequested = false;
    this.tutorialPanel?.update(state, this.panels.open);
    this.updateTutorialTaskPoll();
    this.updateTutorialReviewPoll();
    if (this.tutorialFocusRequested && state.visible && this.panels.open === null) {
      (this.root.querySelector<HTMLElement>('[data-region="tutorial"] [data-tutorial-action="resume"], [data-region="tutorial"] [data-tutorial-action="start"]')
        ?? this.root.querySelector<HTMLElement>('[data-region="tutorial"] button'))?.focus();
      this.tutorialFocusRequested = false;
    }
    const notice = this.root.querySelector<HTMLElement>('[data-region="tutorial-notice"]');
    if (notice) {
      // The full panel owns its error; the outside notice survives Close or folding.
      const fullPanel = this.root.querySelector('[data-region="tutorial"]')?.getAttribute("data-open") === "true";
      notice.hidden = state.saveError === null || fullPanel;
      notice.textContent = state.saveError ? "Tutorial preferences could not be saved. Your changes may not survive a reload." : "";
    }
  }

  // initialSessionID picks the row to reopen, from a served list only: a
  // cached or stale list cannot vouch for a session, and selecting into
  // one would fetch, fail, and clear the remembered selection.
  private initialSessionID(): string | null {
    if (this.sidebarModel.listState !== "ready") return null;
    const sessions = this.sidebarModel.groups.flatMap((group) => group.sessions);
    return pickInitialSession(readLastSession(this.lastSessionStore), sessions);
  }

  // reattachingSessionName names the remembered selection as the cached
  // list knows it, for the loading pane's first line.
  private reattachingSessionName(): string | null {
    const id = readLastSession(this.lastSessionStore);
    if (id === null) return null;
    const session = this.sidebarModel.groups.flatMap((group) => group.sessions).find((s) => s.id === id);
    if (session === undefined) return null;
    return session.display_name || providerDisplayName(session.provider);
  }

  private async refreshSidebar(): Promise<void> {
    if (this.disposed) return;
    const lifecycle = this.lifecycle;
    await this.sidebarModel.refresh();
    if (this.disposed || lifecycle !== this.lifecycle) return;
    if (this.sidebarModel.listState !== "loading") this.endLoadingClock();
    if (this.sidebarModel.listState === "ready") this.dropFinishedCodexHelpers();
    const projects = this.sidebarModel.groups.map((group) => `${group.project.id}\n${group.project.canonical_path}`).join("\u0000");
    if (projects !== this.starterProjects) {
      this.starterProjects = projects;
      void this.starter.observe();
    }
    // The server's remembered session is adopted before any automatic
    // selection, on every refresh that could make one; the barrier is
    // shared and bounded, so an overlapping poll waits on the same answer
    // rather than opening this browser's old choice first.
    await this.lastSession.adopt();
    if (this.disposed || lifecycle !== this.lifecycle) return;
    this.outputActivity.reconcile();
    this.syncTutorial();
    this.sidebarModel.selectedSessionID = this.selectedSessionID;
    this.sidebar.render();
    this.conversationAcks?.observe();
    if (this.selectedSessionID === null && !this.authRequired) {
      if (this.suppressAutoSelect) {
        this.renderEmptyPane();
        return;
      }
      const initial = this.initialSessionID();
      if (initial !== null) {
        // Selecting tears the docked panel down. A panel a tutorial step
        // opened before the list arrived is not one the user closed.
        // Restored only if the user touched no panel meanwhile and no
        // newer selection took over.
        const reopen = this.panels.open;
        const gestures = this.panels.gestures;
        const selection = this.selectionEpoch + 1;
        await this.selectSession(initial);
        if (reopen !== null && this.panels.open === null && this.panels.gestures === gestures
          && this.selectionEpoch === selection && !this.disposed && lifecycle === this.lifecycle) this.panels.set(reopen, false);
      } else {
        this.renderEmptyPane();
      }
    }
  }

  private async selectSession(sessionID: string): Promise<void> {
    if (this.disposed) return;
    const epoch = ++this.epoch;
    ++this.selectionEpoch;
    this.suppressAutoSelect = false;
    const projectID = this.sidebarModel.groups
      .find((g) => g.sessions.some((s) => s.id === sessionID))?.project.id ?? null;
    this.disposeSession(projectID);
    this.selectedSessionID = sessionID;
    this.sidebarModel.selectedSessionID = sessionID;
    this.sidebar.setProjectContext(projectID);
    this.sidebar.render();
    this.renderLoading();

    let detail: SessionDetail;
    try {
      detail = await this.api.getSession(sessionID);
    } catch (cause) {
      if (this.disposed || epoch !== this.epoch) return;
      this.handleError(cause instanceof Error ? cause.message : String(cause), cause);
      if (!this.authRequired) {
        this.suppressAutoSelect = true;
        this.clearSelection();
        await this.refreshSidebar();
      }
      return;
    }
    if (this.disposed || epoch !== this.epoch) return;
    const recovery = recoveryFromSession(detail);
    if (recovery === null && !isVerifiedRunningSession(detail)) {
      this.suppressAutoSelect = true;
      this.clearSelection();
      await this.refreshSidebar();
      return;
    }

    // The server-presented recovery facts decide the pane: a session carrying
    // replay/failure evidence shows the register's recovery surface even
    // while verified-running (a replayed session IS running — the replay card,
    // then the resumed stream). A plain running session mounts the emulator;
    // both poll the server presentation so a completed recovery remounts.
    this.connection = recovery !== null ? "disconnected" : "connecting";
    this.attachmentProblem = null;
    this.statusModel = new SessionStatusModel(this.api, sessionID, {
      onError: (message, cause) => this.handleError(message, cause),
    });
    this.statusModel.session = detail;

    this.mountTaskmaster(detail.project_id, epoch);
    this.mountFiles(detail.project_id);
    this.mountAssets(detail.project_id);
    this.mountChats(sessionID, epoch);
    this.renderChrome(detail);
    if (recovery !== null) {
      this.renderRecovery(detail);
    } else {
      await this.mountTerminal(detail, epoch);
    }
    // The mount may have waited on the mono face; a selection that took over
    // during that window owns the status polling now, and arming a timer
    // here would clobber its own.
    if (this.disposed || epoch !== this.epoch) return;
    this.statusTimer = window.setInterval(() => {
      void this.refreshStatus(epoch);
    }, statusRefreshMS);
    await this.refreshStatus(epoch);
  }

  // clearSelection is the common teardown whenever the selected row leaves
  // the verified-running active surface.
  private clearSelection(): void {
    ++this.selectionEpoch;
    this.disposeSession();
    this.tutorial.resume();
    this.starter.resume();
    this.selectedSessionID = null;
    this.sidebarModel.selectedSessionID = null;
    this.rememberSession(null);
    this.renderEmptyPane();
  }

  private disposeSession(preserveFilesProjectID: string | null = null): void {
    const keepFilesOpen = preserveFilesProjectID !== null
      && this.filesProjectID === preserveFilesProjectID && this.panels.isOpen("files");
    this.root.querySelector('[data-region="login-action"]')?.replaceChildren();
    this.harnessLogin?.close("context-lost");
    this.harnessLogin = null;
    this.closePopover();
    if (this.statusTimer !== null) {
      window.clearInterval(this.statusTimer);
      this.statusTimer = null;
    }
    this.recoveryKey = null;
    this.scrollbackActive = false;
    // Null the current terminal first, so a close event dispatched by dispose
    // cannot mutate the next selection's browser-owned connection state.
    const terminal = this.terminal;
    this.terminal = null;
    // Through syncDock rather than by hand: it owns the marker the dock is
    // painted by, and a teardown that clears the tracking fields without it
    // leaves the marker standing on a dock with nothing in it — and leaves
    // the next sync at the same width believing there is nothing to do.
    this.dockMode = "none";
    this.dockSource = null;
    this.syncDock();
    const statusHost = this.root.querySelector<HTMLElement>('[data-region="terminal-status"]');
    if (statusHost !== null) statusHost.innerHTML = "";
    this.taskRefHover?.dispose();
    this.taskRefHover = null;
    terminal?.dispose();
    this.statusModel = null;
    this.taskmaster?.dispose();
    this.taskmaster = null;
    this.taskmasterModel = null;
    this.taskmasterEvents?.close();
    this.taskmasterEvents = null;
    if (this.filesProjectID !== preserveFilesProjectID) {
      this.files?.dispose();
      this.files = null;
      this.filesModel = null;
      this.filesProjectID = null;
    }
    this.assets?.dispose();
    this.assets = null;
    this.disposeChats();
    this.emptyPanelsProject = null;
    if (!keepFilesOpen) this.panels.close(false);
    this.connection = "disconnected";
    this.endConnection();
    this.attachmentProblem = null;
  }

  // applyTerminalGround paints the terminal frame with the active theme's
  // chrome roles and the emulator's background. The --t-* locals live on the
  // terminal element only; the card takes only the emulator background. A
  // provider with a presentation palette (provider-theme.ts) paints with
  // that palette instead, so its letterboxed TUI sits on its own ground
  // whatever theme the user chose for the rest.
  private applyTerminalGround(): void {
    const name = terminalThemeName();
    const theme = resolveTerminalTheme(name);
    const base = name === "custom"
      ? customThemeChromeSlots(customThemeSlots())
      : terminalChromePalette(name, theme);
    const palette = terminalChromeFor(this.mountedTerminalProvider(), base);
    for (const pane of this.root.querySelectorAll<HTMLElement>('[data-region="terminal"], [data-login-terminal]')) {
      pane.dataset.mono = "operator";
      applyTerminalChromeVars(pane, palette);
      pane.style.backgroundColor = palette.bg;
    }
    const card = this.root.querySelector<HTMLElement>('[data-region="terminal-card"]');
    if (card !== null) card.style.backgroundColor = palette.bg;
  }

  // The terminal's soft-key contract keys on the same media query at both
  // call sites; a drift between them would summon keyboards on phones or
  // suppress focus on desktops.
  private coarseMobile(): boolean {
    return window.matchMedia("(pointer: coarse) and (max-width: 48rem)").matches;
  }

  // phoneLayout is the dock's mount condition, and it must stay the same
  // boundary app.css paints the dock at. The two are not interchangeable with
  // coarseMobile(): CSS below the phone width shows the dock whatever the
  // pointer is, so gating the mount on a coarse pointer would paint an empty
  // bar in a narrowed desktop window, and gating it on the wider coarse
  // boundary would mount controls CSS keeps hidden on a tablet.
  private phoneLayout(): boolean {
    return window.matchMedia("(max-width: 600px)").matches;
  }

  // takeFocus moves focus to a freshly attached terminal when the
  // user's request allows it. An overlay refusal is usually the launch
  // picker's own closing frames — the residue of the gesture that asked —
  // so the request survives and retries on a short timer (not rAF, which
  // pauses in hidden tabs) and dies at the bound; a dialog the user
  // opened afterwards keeps focus by outliving the retries. Composing in a
  // field and the phone soft-key contract still refuse for good, and a
  // disposed or superseded terminal has nothing left to focus.
  private takeFocus(terminal: AgentTerminal, attempt: number): void {
    const outcome = this.focusRequest.consume(
      document.activeElement,
      document.body,
      this.coarseMobile(),
    );
    if (outcome === "granted") {
      if (!this.disposed && this.terminal === terminal) terminal.mounted.focus();
      return;
    }
    if (outcome === "refused") return;
    if (attempt >= 10) {
      // The retries are the request's whole lifetime: an overlay that
      // outlived them is a user gesture of its own, and a request
      // left armed would let some later attach steal focus with no fresh
      // gesture behind it.
      this.focusRequest.cancel();
      return;
    }
    setTimeout(() => {
      if (!this.disposed && this.terminal === terminal) this.takeFocus(terminal, attempt + 1);
    }, 100);
  }

  // The mounted session's provider, read from the pane the mount stamped
  // it on: the ground paint and both theme-change paths resolve the
  // presentation palette against the same value.
  private mountedTerminalProvider(): string | undefined {
    return this.root
      .querySelector<HTMLElement>('[data-region="terminal"]')
      ?.dataset.terminalProvider;
  }

  private async mountTerminal(detail: SessionDetail, epoch: number): Promise<void> {
    if (!isVerifiedRunningSession(detail)) return;
    this.recoveryKey = null;
    const pane = region(this.root, "terminal");
    // The provider lands on the pane before the ground paint: the ground
    // resolves a presentation palette against it.
    pane.dataset.terminalProvider = detail.provider;
    this.applyTerminalGround();
    pane.innerHTML = `
      <div data-live-terminal class="h-full w-full"></div>
      <button type="button" data-live-scrollback-pill hidden
        aria-label="Jump to live" data-hint="${hints.RETURN_TO_LIVE_END}"
        class="absolute right-[18px] top-[10px] z-10 ${SECONDARY_SM_BUTTON} rounded-pill px-[10px]">Jump to live</button>
      <button type="button" data-live-select-text hidden aria-pressed="false"
        aria-label="Select text is on. Select to turn it off." data-hint="${hints.SELECT_TERMINAL_TEXT}"
        class="absolute bottom-[10px] right-[18px] z-10 ${SECONDARY_SM_BUTTON} rounded-pill px-[10px]">Select text</button>
      <div data-live-terminal-overlay class="absolute inset-0"></div>
    `;
    pane.classList.add("relative", "overflow-hidden");
    const host = pane.querySelector<HTMLElement>("[data-live-terminal]")!;
    const controllerID = this.controllerID;
    // The cell must be measured against the face the emulator will render
    // in, so the wait precedes the mount. A selection that supersedes the
    // wait leaves the pane to whoever now owns the epoch, and so does
    // anything that rewrote the pane meanwhile: the status poll asks for a
    // mount every tick while no terminal exists, a cold face can outlast a
    // tick, and recovery can take the pane back. Only the latest host mounts.
    await ensureMonoFontLoaded(host, terminalFontSize());
    if (this.disposed || epoch !== this.epoch) return;
    if (pane.querySelector("[data-live-terminal]") !== host) return;
    let terminal: AgentTerminal;
    const current = () => epoch === this.epoch && this.terminal === terminal;
    terminal = new AgentTerminal(host, {
      onAttached: () => {
        if (!current()) return;
        this.connection = "connected";
        this.tutorial.resume();
        this.starter.resume();
        this.attachmentProblem = null;
        // The budget is earned by LASTING, judged when this connection
        // ends: a slow-consumer drop can only follow a successful attach,
        // so resetting here would refund the very attempt that caused it.
        this.connectedAt = Date.now();
        this.lastConnectionMs = null;
        this.renderAttachmentState();
        // A freshly attached controller is the first moment a waiting
        // login can be offered; ask for the listing now, not at the next
        // interval.
        this.tryInitialCodexHelper();
        if (detail.provider === "codex") {
          this.loginEvents?.close();
          this.loginEvents = new EventSource(`/api/harness-login?session=${encodeURIComponent(detail.id)}&watch=1`);
          this.loginEvents.addEventListener("pending", () => {
            if (!current()) return;
            void this.loginCoordinator.pollNow();
            this.harnessLogin?.notifyPending();
          });
        }
        void this.loginCoordinator.pollNow();
        this.takeFocus(terminal, 0);
      },
      onTakeoverRequired: (message) => {
        if (!current()) return;
        this.tutorial.suspend();
        this.starter.suspend();
        this.connection = "disconnected";
        this.endConnection();
        this.attachmentProblem = { kind: "takeover", message };
        this.renderAttachmentState();
      },
      onFenced: (message) => {
        if (!current()) return;
        this.tutorial.suspend();
        this.starter.suspend();
        this.connection = "disconnected";
        this.endConnection();
        this.attachmentProblem = { kind: "fenced", message };
        this.renderAttachmentState();
      },
      onLeaseExpired: (message) => {
        if (!current()) return;
        this.tutorial.suspend();
        this.starter.suspend();
        this.connection = "disconnected";
        this.endConnection();
        this.attachmentProblem = { kind: "leaseExpired", message };
        this.renderAttachmentState();
        // The lease timed out because this page's heartbeat stopped, which
        // is what a hidden tab does. Nobody took the session, so if the
        // window is in front now, take it straight back.
        this.maybeReclaim();
      },
      onDropped: (message) => {
        if (!current()) return;
        this.connection = "disconnected";
        this.endConnection();
        this.attachmentProblem = { kind: "dropped", message };
        this.renderAttachmentState();
        // Already in front: no focus event is coming, so reclaim now.
        this.maybeReclaim();
      },
      onError: (message) => {
        if (!current()) return;
        this.connection = "disconnected";
        this.endConnection();
        this.attachmentProblem = { kind: "error", message };
        this.renderAttachmentState();
      },
      onWriteStatus: (code, _message) => {
        if (!current()) return;
        // Sent is silent — nothing visible changes, so the
        // per-keystroke status frame must not churn the DOM: no
        // status-panel refresh, no chrome re-render. Outcomes that render
        // (uncertain/failed/interrupted) still repaint immediately.
        // A dropped reply wrote nothing and changes nothing visible.
        if (code === "dropped") return;
        if (code === "sent") {
          // The input path exits tmux copy-mode under the session gate
          // before reporting sent. Until that server outcome arrives, a
          // locally forwarded input must not claim the pane is live.
          this.setScrollbackPill(false);
          return;
        }
        this.renderAttachmentState();
      },
      // Selected-terminal output joins the same content-free activity path as
      // background SSE edges, while also dismissing transient task chrome.
      onOutput: () => {
        if (!current()) return;
        this.taskRefHover?.dismiss();
        if (this.selectedSessionID !== null) this.outputActivity.mark(this.selectedSessionID);
      },
      // A forwarded command only dismisses transient chrome. The scrollback
      // pill follows xterm's local normal-buffer viewport or the server's
      // acknowledged tmux pane state; a request alone proves neither.
      onScrollCommand: () => {
        if (!current()) return;
        this.taskRefHover?.dismiss();
      },
      onScrollbackChange: (active) => {
        if (!current()) return;
        this.setScrollbackPill(active);
      },
      onInput: () => {
        if (!current()) return;
        this.taskRefHover?.dismiss();
      },
      onClose: () => {
        if (!current()) return;
        this.connection = "disconnected";
        this.endConnection();
        // A terminal outcome (failed/uncertain/interrupted) was the
        // server's reason for closing and stays in the overlay message;
        // anything else is a plain connection-level close.
        const outcome = terminal.writeOutcome();
        this.attachmentProblem ??= {
          kind: "closed",
          message: outcome !== null && outcome.message !== ""
            ? outcome.message
            : outcome !== null && outcome.code === "interrupted"
              ? "connection interrupted"
              : outcome !== null
                ? `write ${outcome.code}`
                : "connection interrupted",
        };
        this.renderAttachmentState();
      },
    }, {
      controllerID,
      mountTerminal: (c) =>
        createTerminal(
          c,
          terminalThemeFor(detail.provider, resolveTerminalTheme(terminalThemeName())),
          terminalFontSize(),
        ),
    });
    this.terminal = terminal;
    // The soft-key row is a touch affordance; mounting it on a desktop
    // browser would add chrome the desktop layout does not have. The CSS
    // hides it there too, but an unmounted row also stays out of the a11y
    // tree.
    this.dockSource = terminal;
    this.dockMode = "live";
    this.syncDock();
    pane.querySelector<HTMLElement>("[data-live-scrollback-pill]")
      ?.addEventListener("click", () => terminal.jumpToLive());
    pane.querySelector<HTMLElement>("[data-live-select-text]")
      ?.addEventListener("click", () => {
        this.selectTextSessions.set(detail.id, false);
        terminal.mounted.setSelectText(false);
        this.paintSelectText(false);
      });
    this.taskRefHover = new TaskRefHover(terminal.mounted.terminal, pane, {
      getTags: () => this.taskmasterModel?.tags ?? [],
    });
    terminal.attach(detail.id);
    // The pane carries the session's select-text choice across mounts, and
    // the chip says so for as long as it is on.
    const selectText = this.selectTextSessions.get(detail.id) === true;
    terminal.mounted.setSelectText(selectText);
    this.paintSelectText(selectText);
    this.renderAttachmentState();
  }

  // syncDock brings the dock's controls in and out with the width CSS paints
  // it at, and with what the pane is showing. Sampling either once when a
  // session attaches leaves them disagreeing for the rest of the session: a
  // window narrowed past the phone boundary paints a dock with nothing in it,
  // one widened out of it leaves the key strip in the a11y tree with nothing
  // on screen, and a session that drops into recovery paints a dock whose
  // keys were disposed with the emulator.
  private syncDock(): void {
    const wanted = this.phoneLayout() ? this.dockMode : "none";
    if (wanted === this.dockMounted) return;
    this.terminalSoftKeys?.dispose();
    this.terminalSoftKeys = null;
    this.terminalCompose?.dispose();
    this.terminalCompose = null;
    const composeHost = this.root.querySelector<HTMLElement>('[data-region="terminal-compose"]');
    if (composeHost !== null) composeHost.innerHTML = "";
    this.dockMounted = wanted === "none" ? null : wanted;
    const dock = this.root.querySelector<HTMLElement>('[data-region="terminal-dock"]');
    if (wanted === "none") dock?.removeAttribute("data-dock-mounted");
    else dock?.setAttribute("data-dock-mounted", wanted);
    if (wanted === "none") return;
    if (wanted === "recovery") {
      // The key strip survives the reconnect as presentation: it is drawn
      // while the session is away. There is no emulator to receive a
      // keystroke, so the keys are bound to an inert receiver and disabled.
      // The input row does not survive — a focusable field with nowhere to
      // send would invite typing into nothing.
      this.terminalSoftKeys = new MobileTerminalSoftKeys(
        region(this.root, "terminal-soft-keys"),
        inertSoftKeyTerminal(),
      );
      this.setSoftKeysInert(true);
      return;
    }
    const source = this.dockSource;
    if (source === null) return;
    this.terminalSoftKeys = new MobileTerminalSoftKeys(
      region(this.root, "terminal-soft-keys"),
      source.mounted,
    );
    this.mountMobileCompose(source);
    this.syncAttachmentInputs();
  }

  // mountMobileCompose wires the buffered input row. It is a focus
  // target for the soft keyboard AND a real buffer — the one exception to
  // terminal-only input: text accumulates in the field so dictation apps
  // can edit it, and Send or Return pastes the draft then presses Enter.
  // Escape and the control keys stay with the key strip.
  // The draft is written and tracked through one terminal: a write counts
  // only on the attachment that forwarded it.
  private mountMobileCompose(terminal: AgentTerminal): void {
    const host = region(this.root, "terminal-compose");
    host.innerHTML = mobileComposeMarkup();
    const compose = new MobileTerminalCompose({
      paste: (text) => terminal.mounted.paste(text),
      sendInput: (data) => terminal.mounted.sendInput(data),
      trackWrite: (send, done) => terminal.trackWrite(send, done),
    });
    this.terminalCompose = compose;
    const input = host.querySelector<HTMLTextAreaElement>("[data-mobile-compose-input]")!;
    const form = host.querySelector<HTMLFormElement>("[data-mobile-compose-form]")!;
    const sessionID = this.selectedSessionID;
    if (sessionID !== null) input.value = this.composeDrafts.get(sessionID) ?? "";
    input.addEventListener("input", () => {
      if (sessionID !== null) this.composeDrafts.set(sessionID, input.value);
    });
    const send = (): void => {
      if (this.connection !== "connected" || this.selectedSessionID !== sessionID || this.terminalCompose !== compose) return;
      const snapshot = input.value;
      const started = compose.flush(snapshot, (code) => {
        if (this.disposed || this.selectedSessionID !== sessionID || this.terminalCompose !== compose) return;
        if (code === "sent") {
          if (input.value !== snapshot || sessionID === null || this.composeDrafts.get(sessionID) !== snapshot) return;
          input.value = "";
          this.composeDrafts.delete(sessionID);
          return;
        }
        this.infoBar?.announce("The draft may have been partially sent. Inspect the terminal before retrying; your draft is still in the composer.");
      });
      if (!started) this.infoBar?.announce("Terminal input is not ready. Your draft is still in the composer.");
    };
    input.addEventListener("beforeinput", (event) => {
      if (!isComposeSubmitIntent((event as InputEvent).inputType)) return;
      event.preventDefault();
      send();
    });
    form.addEventListener("submit", (event) => {
      event.preventDefault();
      send();
      input.focus();
    });
  }

  private syncAttachmentInputs(): void {
    const unavailable = this.connection !== "connected";
    if (this.dockMounted === "live") this.setSoftKeysInert(unavailable);
    const compose = this.root.querySelector<HTMLElement>('[data-region="terminal-compose"]');
    if (compose !== null) {
      compose.inert = unavailable;
      for (const control of compose.querySelectorAll<HTMLTextAreaElement | HTMLButtonElement>("textarea, button")) {
        control.disabled = unavailable;
      }
    }
    const terminalInput = this.root.querySelector<HTMLTextAreaElement>("[data-live-terminal] textarea");
    if (terminalInput !== null) terminalInput.disabled = unavailable;
    const jump = this.root.querySelector<HTMLButtonElement>("[data-live-scrollback-pill]");
    if (jump !== null) jump.disabled = unavailable;
  }

  // setSoftKeysInert disables or re-enables the key strip's controls. An
  // inert key is still drawn, so the strip keeps the height the terminal above
  // it is sized against.
  private setSoftKeysInert(inert: boolean): void {
    this.terminalSoftKeys?.setInert(inert);
  }

  // renderRecovery paints the server-presented recovery presentation over the
  // terminal pane, disposing any live emulator first — the pane
  // belongs to the register while the recovery presentation owns it. The
  // presentation derives ONLY from the server-presented session record;
  // nothing here reads or interprets the pane's captured output.
  private renderRecovery(detail: SessionDetail): void {
    if (this.disposed) return;
    const recovery = recoveryFromSession(detail);
    if (recovery === null) return;
    this.recoveryKey = this.recoveryKeyOf(detail);
    // A selection can open straight into recovery, without a terminal ever
    // having been mounted, so the dock is declared here rather than only on
    // the way out of a live one.
    this.dockSource = null;
    this.dockMode = "recovery";
    this.syncDock();
    if (this.terminal !== null) {
      const terminal = this.terminal;
      this.terminal = null;
      this.taskRefHover?.dispose();
      this.taskRefHover = null;
      terminal.dispose();
      this.scrollbackActive = false;
      this.connection = "disconnected";
    }
    // The identity lands before the ground is painted. applyTerminalGround
    // resolves the presentation palette from the pane's own provider, so
    // painting first paints the session that just left: switching away from a
    // letterboxing provider showed the next session's recovery surface on the
    // old provider's ground until an unrelated refresh repainted it.
    const pane = region(this.root, "terminal");
    pane.dataset.terminalProvider = detail.provider;
    this.applyTerminalGround();
    pane.innerHTML = `
      <div class="flex h-full w-full flex-col">
        ${recoveryBannerHtml(recovery)}
        <div data-recovery-scrollback class="min-h-0 flex-1 overflow-y-auto ${RECOVERY_MONO} text-(--t-dim)">
          ${recoveryStreamHtml(recovery)}
        </div>
        ${recoveryRegisterHtml(recovery)}
      </div>
    `;
    this.bindRecoveryActions(pane, detail);
    if (recovery.state === "resumeEnded") void this.mountRetainedScreen(pane, detail, this.epoch);
  }

  private recoveryKeyOf(detail: SessionDetail): string {
    return `${detail.id}\n${JSON.stringify(recoveryFromSession(detail))}`;
  }

  // mountRetainedScreen shows the dead pane's last screen above the
  // resume-ended card: the provider printed why it stopped, and that text is
  // the user's explanation. It is transported as bytes and escaped;
  // nothing reads it. A session that retains no screen shows the card alone.
  private async mountRetainedScreen(pane: HTMLElement, detail: SessionDetail, epoch: number): Promise<void> {
    let screen: string | null;
    try {
      screen = await this.api.sessionScreen(detail.id);
    } catch {
      return;
    }
    if (this.disposed || epoch !== this.epoch || screen === null || screen.trim() === "") return;
    const scrollback = pane.querySelector<HTMLElement>("[data-recovery-scrollback]");
    if (scrollback === null || scrollback.querySelector("[data-recovery-screen]") !== null) return;
    const pre = document.createElement("pre");
    pre.dataset.recoveryScreen = "";
    pre.className = "m-0 whitespace-pre-wrap break-words px-(--space-5) pt-[10px] font-[inherit] text-[inherit] leading-[inherit] text-(--t-dim)";
    pre.textContent = screen.replace(/\s+$/, "");
    scrollback.prepend(pre);
  }

  // The failed card's actions are live verbs: Retry runs the
  // user's explicit recover — the same row revives in place — and
  // Start fresh session launches a new one beside it in the same project
  // and provider. Both settle through the ordinary status poll; the
  // buttons only disable themselves while their request is in flight, so
  // a refused recovery leaves the card exactly as the server presents it.
  private bindRecoveryActions(pane: HTMLElement, detail: SessionDetail): void {
    // A refused action must say so on the card: the info-bar error path
    // only reacts to auth, and a silent 409 reads as a dead button.
    const sayWhy = (err: unknown): void => {
      const line = pane.querySelector<HTMLElement>("[data-recovery-error]");
      if (line === null) return;
      line.textContent = err instanceof ApiError
        ? err.message
        : "the request did not reach the server";
      line.hidden = false;
    };
    const retry = pane.querySelector<HTMLButtonElement>("[data-recovery-retry]");
    retry?.addEventListener("click", () => {
      retry.disabled = true;
      void this.api.recoverSession(detail.id)
        .then((result) => {
          // The server names what committed. A relaunch happens only for a
          // row that recorded no conversation to resume, and it must say so
          // ON THE CARD: letting the remount pass silently would imply an
          // earlier thread came back.
          if (result.outcome !== "relaunched") return;
          const line = pane.querySelector<HTMLElement>("[data-recovery-error]");
          if (line === null) return;
          line.textContent =
            "no conversation was recorded to resume — a fresh conversation was relaunched in this session";
          line.hidden = false;
        })
        .catch((err: unknown) => {
          sayWhy(err);
          this.handleError("session recovery refused", err);
        })
        .finally(() => {
          if (this.disposed) return;
          retry.disabled = false;
          void this.statusModel?.refresh(true);
          void this.sidebarModel.refresh();
        });
    });
    const fresh = pane.querySelector<HTMLButtonElement>("[data-recovery-fresh]");
    fresh?.addEventListener("click", () => {
      const epoch = this.epoch;
      fresh.disabled = true;
      void this.api.createSession(detail.project_id, detail.provider)
        .then((created) => {
          if (this.disposed || this.authRequired || epoch !== this.epoch) return;
          if (created.provider === "codex" && created.codex_login_helper === true) this.codexLoginHelpers.add(created.id);
          void this.selectSession(created.id);
        })
        .catch((err: unknown) => {
          sayWhy(err);
          this.handleError("fresh session launch failed", err);
        })
        .finally(() => {
          if (this.disposed) return;
          fresh.disabled = false;
          void this.sidebarModel.refresh();
        });
    });
  }

  // endConnection banks how long the attachment that just ended lasted,
  // which is what the reclaim budget is earned by. An attempt that never
  // connected banks ZERO rather than leaving the previous connection's
  // duration standing — otherwise a failed attach would keep refunding
  // the budget with a success that is already spent.
  private endConnection(): void {
    this.loginEvents?.close();
    this.loginEvents = null;
    this.harnessLogin?.close("context-lost");
    this.harnessLogin = null;
    this.loginCoordinator.invalidate();
    this.lastConnectionMs = this.connectedAt === null ? 0 : Date.now() - this.connectedAt;
    this.connectedAt = null;
  }

  // maybeReclaim reconnects a dropped terminal when the window is in
  // front, and does nothing otherwise. The policy is decideReclaim's; this
  // only supplies what the shell knows and carries the counter.
  private maybeReclaim(): void {
    if (this.disposed || this.terminal === null || this.selectedSessionID === null) return;
    const decision = decideReclaim({
      kind: this.attachmentProblem?.kind ?? null,
      visible: typeof document === "undefined" || document.visibilityState === "visible",
      sessionRunning: this.statusModel?.session?.status === "running",
      connected: this.connection === "connected",
      connectedMs: this.connectedAt !== null
        ? Date.now() - this.connectedAt
        : this.lastConnectionMs,
      attempts: this.autoReclaims,
    });
    this.autoReclaims = decision.attempts;
    if (decision.reconnect) this.reconnect(false);
  }

  private reconnect(takeover: boolean): void {
    if (this.terminal === null || this.selectedSessionID === null) return;
    if (this.statusModel?.session?.status !== "running") return;
    this.connection = "connecting";
    this.attachmentProblem = null;
    this.renderAttachmentState();
    this.terminal.attach(this.selectedSessionID, { takeover });
  }

  // One authenticated, content-free event stream covers every managed pane;
  // per-session silence windows live in OutputActivityWindows.
  private mountOutputActivityEvents(): void {
    this.outputActivityEvents?.close();
    const events = new EventSource("/api/session-activity/events");
    events.addEventListener("output", (event) => {
      if (this.disposed) return;
      try {
        const activity = JSON.parse((event as MessageEvent).data) as {
          session_id?: unknown;
          generation?: unknown;
        };
        if (typeof activity.session_id !== "string" || !Number.isInteger(activity.generation)) return;
        this.outputActivity.ingest({
          session_id: activity.session_id,
          generation: activity.generation as number,
        });
      } catch {
        // Malformed presentation evidence proves nothing.
      }
    });
    this.outputActivityEvents = events;
  }

  private clearOutputActivity(): void {
    this.outputActivity.dispose();
  }

  // setScrollbackPill toggles the copy-mode pill over the terminal pane.
  private setScrollbackPill(active: boolean): void {
    if (this.scrollbackActive === active) return;
    this.scrollbackActive = active;
    const pill = this.root.querySelector<HTMLElement>("[data-live-scrollback-pill]");
    if (pill !== null) pill.hidden = !active;
  }

  // paintSelectText shows the pane's select-text chip. The chip is the
  // state made visible: while it is on screen, drags select in the browser
  // instead of reaching the application, and clicking it turns that off.
  private paintSelectText(active: boolean): void {
    const chip = this.root.querySelector<HTMLElement>("[data-live-select-text]");
    if (chip === null) return;
    chip.hidden = !active;
    chip.setAttribute("aria-pressed", active ? "true" : "false");
  }

  private renderAttachmentState(): void {
    this.conversationAcks?.observe();
    if (this.disposed) return;
    this.syncAttachmentInputs();
    const detail = this.statusModel?.session;
    if (detail !== null && detail !== undefined) {
      this.renderChrome(detail);
    }
    const overlay = this.root.querySelector<HTMLElement>("[data-live-terminal-overlay]");
    if (overlay === null) return;
    if (detail === null || detail === undefined || detail.status !== "running") {
      overlay.className = "pointer-events-none absolute inset-0 hidden";
      overlay.innerHTML = "";
      return;
    }
    if (this.connection === "connected") {
      overlay.className = "pointer-events-none absolute inset-0 hidden";
      overlay.innerHTML = "";
      return;
    }
    const problem = this.attachmentProblem;
    const takeover = problem?.kind === "takeover";
    const action = takeover ? "Take control" : "Reconnect";
    const message = problem?.message ?? "Connecting to the terminal…";
    // The phone pins this notice above scrollback; desktop keeps a corner
    // overlay. It classifies only this browser attachment.
    overlay.className = "pointer-events-none absolute inset-0 text-(--t-fg)";
    const notice = overlay.querySelector("[data-attachment-notice]");
    // Polling an unchanged attachment must not re-announce its live region.
    if (notice?.querySelector("span")?.textContent === message
      && (notice.querySelector("[data-live-attach]")?.textContent?.trim() ?? "")
        === (this.connection === "connecting" ? "" : action)) return;
    overlay.innerHTML = `
      <div data-attachment-notice role="status" class="pointer-events-auto absolute bottom-3 right-3 flex max-w-sm items-center gap-3 rounded-control border border-(--t-panel-border) bg-(--t-bg)/90 px-3 py-2">
        <span class="text-caption text-(--t-dim)">${escapeHtml(message)}</span>
        ${this.connection === "connecting" ? "" : `
          <button data-live-attach data-hint="${takeover ? hints.TAKE_CONTROL_OF_SESSION : hints.RECONNECT_TO_SESSION}" class="h-8 flex-none rounded-control bg-accent px-3 text-body-sm font-medium text-text-on-accent">${action}</button>
        `}
      </div>
    `;
    overlay.querySelector("[data-live-attach]")?.addEventListener("click", () => {
      this.reconnect(takeover);
    });
  }

  private async refreshStatus(epoch: number): Promise<void> {
    const model = this.statusModel;
    if (model === null) return;
    const previousStatus = model.session?.status;
    await model.refresh(false);
    if (epoch !== this.epoch || model !== this.statusModel) return;
    if (model.session !== null) {
      if (recoveryFromSession(model.session) !== null) {
        // The recovery presentation decides over the running gate: replay and
        // failure evidence own the pane even for a verified-running session.
        // When the server stops presenting recovery facts, the next tick
        // remounts the live surface.
        this.refreshOpenPopover();
        this.renderChrome(model.session);
        if (this.recoveryKeyOf(model.session) !== this.recoveryKey) this.renderRecovery(model.session);
        return;
      }
      if (!isVerifiedRunningSession(model.session)) {
        this.suppressAutoSelect = true;
        this.clearSelection();
        await this.refreshSidebar();
        return;
      }
      this.refreshOpenPopover();
      this.renderChrome(model.session);
      this.syncAttachmentToStatus(model.session, previousStatus !== model.session.status);
    }
  }

  private async refreshTitleAfterRename(sessionID: string): Promise<void> {
    // Route through the status model's own refresh so its sequencing discards
    // stale responses rather than mutating session state around the model.
    const model = this.statusModel;
    if (model === null || model.session?.id !== sessionID) return;
    await model.refresh(true);
    // The model instance is part of the guard: an A→B→A selection cycle
    // replaces it, and the captured one's state must never paint over the
    // current chrome.
    if (this.disposed || this.selectedSessionID !== sessionID || this.statusModel !== model) return;
    const detail = model.session;
    if (detail !== null && detail !== undefined) this.renderChrome(detail);
  }

  // mountTaskmaster prepares the board WITHOUT waiting for the user:
  // the first read answers whether the project has tasks at all. The panel
  // owns its own 47px header (title, Add PRD, close), like the Files panel.
  private mountTaskmaster(projectID: string, epoch: number): void {
    this.taskmasterModel = new TaskmasterPanelModel(this.api, projectID, {
      onError: (message, cause) => this.handleError(message, cause),
    });
    this.taskmaster = new TaskmasterPanel(region(this.root, "tasks-body"), this.taskmasterModel, {
      onClose: () => this.panels.close(),
    });
    this.taskmasterProject = projectID;
    void this.taskmaster.refresh().then(() => {
      if (epoch !== this.epoch) return;
      this.tutorialNoteTasks(projectID);
      void this.tutorialLanes.refresh(projectID);
      this.panels.reapply();
      const detail = this.statusModel?.session;
      if (detail !== null && detail !== undefined) this.renderChrome(detail);
    });
    this.taskmasterEvents?.close();
    const events = new EventSource(`/api/projects/${encodeURIComponent(projectID)}/taskmaster/events`);
    events.addEventListener("tasks-changed", () => {
      if (epoch === this.epoch) void this.taskmasterModel?.refresh().then(() => { if (epoch === this.epoch) this.tutorialNoteTasks(projectID); });
    });
    this.taskmasterEvents = events;
  }

  // mountChats prepares the ledger for the selected session's agent and
  // reads it once, so the trigger's unseen accent and the info bar's news
  // sentence are true before the user opens anything. The change
  // stream carries no content; each event re-reads the ledger.
  private mountChats(sessionID: string, epoch: number): void {
    this.disposeChats();
    this.chatsModel = new ChatsPanelModel(this.api, sessionID, {
      onError: (message, cause) => this.handleError(message, cause),
      onChange: () => this.repaintChatFacts(),
    }, this.chatSeenFor(sessionID));
    this.chats = new ChatsPanel(region(this.root, "chats-body"), this.chatsModel, {
      onClose: () => {
        this.panels.close();
        focusPanelTrigger(this.root, "live", "chats");
      },
    });
    void this.chats.refresh().then(() => {
      if (epoch !== this.epoch) return;
      this.panels.reapply();
      this.repaintChatFacts();
    });
    const events = new EventSource(`/api/sessions/${encodeURIComponent(sessionID)}/chats/events`);
    events.addEventListener("chats-changed", () => {
      if (epoch === this.epoch) void this.chats?.refresh();
    });
    this.chatsEvents = events;
    this.startChatIndex(epoch);
  }

  private disposeChats(): void {
    this.chats?.dispose();
    this.chats = null;
    this.chatsModel = null;
    this.chatsEvents?.close();
    this.chatsEvents = null;
    if (this.chatIndexTimer !== null) window.clearInterval(this.chatIndexTimer);
    this.chatIndexTimer = null;
    this.infoBar?.setNews("chats", null);
    this.sidebar.setChatMarks(new Map());
  }

  // chatSeenFor is the one seen-state per session: the panel model and the
  // sidebar mark both read it, so the two accents cannot disagree.
  private chatSeenFor(sessionID: string): Map<string, string | null> {
    let seen = this.chatSeen.get(sessionID);
    if (seen === undefined) {
      seen = new Map();
      this.chatSeen.set(sessionID, seen);
    }
    return seen;
  }

  // The sidebar marks come from the content-free index, polled on the
  // same cadence as the session list: one read marks every row.
  private startChatIndex(epoch: number): void {
    const read = async (): Promise<void> => {
      let index: ChatIndex;
      try {
        index = await this.api.listChats();
      } catch {
        return; // the marks keep their last value; the next poll may succeed
      }
      if (epoch !== this.epoch || this.disposed) return;
      this.chatIndex = index;
      this.paintChatMarks();
    };
    void read();
    this.chatIndexTimer = window.setInterval(() => void read(), 10_000);
  }

  private paintChatMarks(): void {
    if (this.chatIndex === null) return;
    const sessions = this.sidebarModel.groups.flatMap((group) => group.sessions);
    this.sidebar.setChatMarks(chatMarks(this.chatIndex, chatHandles(this.chatIndex, sessions), (sessionID, chatID) => this.chatSeen.get(sessionID)?.get(chatID)));
  }

  // repaintChatFacts refreshes the two tells the panel leaves outside
  // itself: the trigger's unseen accent and the info bar's news sentence.
  private repaintChatFacts(): void {
    const model = this.chatsModel;
    const unseen = model !== null && model.unseen && !this.panels.isOpen("chats");
    for (const trigger of this.root.querySelectorAll<HTMLElement>('[data-live-chats], [data-live-panels], [data-panel-choice="chats"]')) {
      trigger.setAttribute("data-unseen", String(unseen));
    }
    this.infoBar?.setNews("chats", model?.news ?? null);
    this.syncTutorial();
    // The model already marked its rows seen in the shared map; the
    // sidebar mark is repainted from the same map, in the same tick.
    this.paintChatMarks();
  }

  // mountFiles prepares the panel WITHOUT reading anything: a project's
  // files are listed when the user opens the panel, not on every
  // session selection.
  //
  // The breadcrumb's root is the project directory, so the panel is given
  // the registered path — the one fact that says what "the project root"
  // is on disk. It is a label here; the server, not this string, decides
  // what any request may reach.
  private mountFiles(projectID: string): void {
    if (this.files !== null && this.filesModel !== null && this.filesProjectID === projectID) {
      this.files.render();
      return;
    }
    this.files?.dispose();
    const panel = region(this.root, "files-body");
    const project = this.project(projectID);
    this.filesProjectID = projectID;
    this.filesModel = new FilesPanelModel(
      this.api,
      projectID,
      project?.canonical_path ?? project?.name ?? "",
      {
        onError: (message, cause) => this.handleError(message, cause),
        onChange: () => this.files?.render(),
        // The marker is read defensively: a listing without it opens nothing.
        onOpenFile: (entry) => {
          if ((entry as { change?: unknown }).change === "modified") this.noteTutorialFact([{ name: "file_opened", arg: "modified" }]);
        },
        onProgress: (record) => this.files?.progress(record),
      },
    );
    this.files = new FilesPanel(panel, this.filesModel, {
      onExpandedChange: () => { for (const size of this.panelSizes) size.refresh(); },
      onClose: () => {
        this.panels.close();
        focusPanelTrigger(this.root, "live", "files");
      },
    });
  }

  // mountAssets prepares the panel without reading anything: the panel
  // scans the agent locations only while it is open. The count's feed
  // belongs to the project, so a session switch inside it keeps the feed.
  private mountAssets(projectID: string): void {
    this.assets?.dispose();
    this.assetsFeed.follow(projectID, this.panels.isOpen("assets"));
    const seen = this.assetsFeed.seen;
    const model = new AssetsPanelModel(this.api, projectID, {
      onError: (message, cause) => this.handleError(message, cause),
      onChange: () => {
        // Every listing the open panel shows is seen, including files that
        // land while it is open.
        if (model.isOpen && model.listing !== null) seen?.markSeen(model.listing);
        this.assets?.render();
      },
    });
    this.assets = new AssetsPanel(region(this.root, "assets-body"), model, {
      onClose: () => {
        this.panels.close();
        focusPanelTrigger(this.root, "live", "assets");
      },
      onOpen: (entry) => {
        const filesModel = this.filesModel;
        if (filesModel === null) return;
        const selection = this.selectionEpoch;
        const current = () => this.filesModel === filesModel && this.selectionEpoch === selection;
        this.panels.set("files");
        const opening = entry.kind === "dir"
          ? filesModel.openDirectory(entry.path)
          : filesModel.openFile(entry.path, current);
        void opening.then(() => {
          if (current()) this.files?.render();
        });
      },
      modals: () => this.root.querySelector<HTMLElement>('[data-region="modals"]'),
      imageUrl: (path) => this.api.projectFileImageUrl(projectID, path),
    });
  }

  // paintAssetFacts writes the feed's count onto the controls and its line
  // into the info bar. The line's age is when the count last grew, so a
  // fresh file takes the bar back from an older chat sentence and a repaint
  // never makes an old count look new.
  private paintAssetFacts(): void {
    const feed = this.assetsFeed;
    this.infoBar?.setNews("assets", assetsNews(feed.unseen, Date.now()), feed.at ?? undefined);
    paintAssetsNew(this.root, "live", feed.count);
  }

  private renderChrome(detail: SessionDetail): void {
    if (this.disposed) return;
    this.syncTutorial();
    const titlebar = region(this.root, "terminal-titlebar");
    const focusedControl = ["data-live-more", "data-live-panels", "data-live-sessions", "data-live-tasks", "data-live-files", "data-live-chats", "data-live-assets", "data-live-login"]
      .find((attribute) => region(this.root, "content").contains(document.activeElement) && document.activeElement?.hasAttribute(attribute));
    // The chip resolves its colour through the sidebar tile's rule, so it
    // needs the project's derived tint beside the session's own colour. The
    // group covering the session's project can lag a fresh selection (the
    // sidebar loads async); until one arrives the chip wears the provider
    // hue and the next status tick repaints it.
    const autoColor = this.sidebarModel.groups
      .find((group) => group.project.id === detail.project_id)
      ?.autoColor;
    region(this.root, "terminal-titlebar").innerHTML = `
      <!-- One line: the provider tile and the session name. The tile
           centres on the name by sharing a baseline row. The session name
           is unbounded (the user's own label) so it truncates rather
           than overrunning the Tasks and ⋯ controls to its right. -->
      ${terminalIdentityHtml(detail, autoColor)}
      ${titlebarRightHtml("live", {
        panels: true,
        filesPressed: this.panels.isOpen("files"),
        tasksPressed: this.panels.isOpen("tasks"),
        sessions: true,
        chats: {
          pressed: this.panels.isOpen("chats"),
          unseen: (this.chatsModel?.unseen ?? false) && !this.panels.isOpen("chats"),
          hint: this.chatsModel?.agent ? hints.watchChatsOf(this.chatsModel.agent) : undefined,
        },
        assets: { pressed: this.panels.isOpen("assets"), count: this.assetsFeed.count },
      })}
    `;
    region(this.root, "login-action").innerHTML = this.loginActionLabel(detail) !== null
        ? `<button type="button" data-live-login class="${SECONDARY_SM_BUTTON}">${detail.provider === "codex" ? "Open login helper" : "Finish login"}</button>`
        : "";
    this.root.querySelector("[data-live-more]")?.addEventListener("click", () => this.showMore());
    this.root.querySelector("[data-live-panels]")?.addEventListener("click", () => this.showPanels());
    this.root.querySelector("[data-live-sessions]")?.addEventListener("click", () => this.sidebar.open());
    this.root.querySelector("[data-live-tasks]")?.addEventListener("click", () => {
      this.panels.toggle("tasks");
    });
    this.root.querySelector("[data-live-files]")?.addEventListener("click", () => {
      this.panels.toggle("files");
    });
    this.root.querySelector("[data-live-chats]")?.addEventListener("click", () => {
      this.panels.toggle("chats");
    });
    this.root.querySelector("[data-live-assets]")?.addEventListener("click", () => {
      this.panels.toggle("assets");
    });
    this.root.querySelector("[data-live-login]")?.addEventListener("click", () => this.openHarnessLogin());
    // Polling session metadata must not drop keyboard focus from a titlebar control.
    if (focusedControl) this.root.querySelector<HTMLElement>(`[${focusedControl}]`)?.focus();
    if (this.popoverOpen === "panels") titlebar.querySelector("[data-live-panels]")?.setAttribute("aria-expanded", "true");
    this.infoBar?.refresh();
    this.repaintChatFacts();
    this.dropStaleLoginRows();
    this.tryInitialCodexHelper();
  }

  // repaintPanelToggles patches the two docked-panel buttons' pressed
  // state in place. Re-rendering the whole titlebar would rebuild the ⋯
  // trigger under an open popover.
  private repaintPanelToggles(): void {
    for (const [selector, open] of [
      ["[data-live-files]", this.panels.isOpen("files")],
      ["[data-live-tasks]", this.panels.isOpen("tasks")],
      ["[data-live-chats]", this.panels.isOpen("chats")],
      ["[data-live-assets]", this.panels.isOpen("assets")],
    ] as const) {
      this.root
        .querySelector(selector)
        ?.setAttribute("aria-pressed", open ? "true" : "false");
    }
  }

  // The terminal's context menu: the shared menu surface in the shared
  // popover region, dismissed by a backdrop click or Escape, with focus
  // handed back to the pane afterwards.
  //
  // The menu row is the user gesture Chrome requires for a clipboard read,
  // which is why Paste can only live behind one.
  private openTerminalMenu(terminal: AgentTerminal, x: number, y: number): void {
    // A More menu cannot be open under a click that reached the pane, but
    // its state must not outlive the panel this mount replaces.
    this.closePopover();
    // The row names what it will take, so it has to take what it named. The
    // pane keeps painting while the menu is open — output can clear the
    // selection under it — so the label and the clipboard both come from
    // this one reading, taken when the user asked for the menu.
    const copy = terminal.mounted.copyable();
    const popovers = region(this.root, "popovers");
    popovers.innerHTML = `<div class="absolute inset-0" data-popover-backdrop>${
      terminalMenuHtml({ hasSelection: copy.fromSelection, selectText: this.selectTextSessions.get(this.selectedSessionID ?? "") === true, loginLabel: this.loginActionLabel(this.statusModel?.session) })}</div>`;
    const backdrop = popovers.querySelector<HTMLElement>("[data-popover-backdrop]")!;
    const menu = popovers.querySelector<HTMLElement>("[data-terminal-menu-popover]");
    if (menu === null) return;
    const place = placeTerminalMenu(
      { x, y },
      { width: menu.offsetWidth, height: menu.offsetHeight },
      { width: window.innerWidth, height: window.innerHeight },
    );
    menu.style.top = `${place.top}px`;
    menu.style.left = `${place.left}px`;
    const close = (): void => {
      popovers.replaceChildren();
      if (!this.disposed && this.terminal === terminal) terminal.mounted.focus();
    };
    backdrop.addEventListener("click", (event) => {
      if (event.target === backdrop) close();
    });
    menu.addEventListener("keydown", (event) => {
      if ((event as KeyboardEvent).key !== "Escape") return;
      event.preventDefault();
      close();
    });
    menu.querySelector("[data-terminal-menu-copy]")?.addEventListener("click", () => {
      close();
      void navigator.clipboard.writeText(copy.text)
        .catch((cause) => this.handleError("clipboard write failed", cause));
    });
    menu.querySelector("[data-harness-login]")?.addEventListener("click", () => { close(); this.openHarnessLogin(); });
    menu.querySelector("[data-terminal-menu-select]")?.addEventListener("click", () => {
      close();
      const sessionID = this.selectedSessionID;
      if (sessionID === null) return;
      const next = !(this.selectTextSessions.get(sessionID) === true);
      this.selectTextSessions.set(sessionID, next);
      // The pane the menu was opened over is the pane the choice lands on;
      // a selection that moved under the menu keeps its own session's state.
      if (!this.disposed && this.terminal === terminal) {
        terminal.mounted.setSelectText(next);
        this.paintSelectText(next);
      }
    });
    menu.querySelector("[data-terminal-menu-paste]")?.addEventListener("click", () => {
      close();
      void navigator.clipboard.readText().then((text) => {
        // The read is async, so the pane it was opened over may be gone or
        // replaced by the time it resolves; pasting into its successor would
        // deliver the text to a session the user never asked.
        if (this.disposed || this.terminal !== terminal || text === "") return;
        terminal.mounted.paste(text);
      }).catch((cause) => this.handleError("clipboard read failed", cause));
    });
    menu.querySelector<HTMLElement>("[role=menuitem]")?.focus();
  }

  // Dismissal is click-away-ONLY: the close handler sits on the full-screen
  // backdrop and fires only when the backdrop itself is the event target, so
  // a click landing inside the panel never bubbles into a close. The panel
  // is positioned with explicit coordinates from the trigger's rect — the
  // shared popover region is viewport-fixed, so percentage anchoring
  // against the trigger does not hold there (the sidebar menus do the same).
  private anchorPopover(panel: string, rect: DOMRect): void {
    const popovers = region(this.root, "popovers");
    popovers.innerHTML = `<div class="absolute inset-0" data-popover-backdrop>${panel}</div>`;
    const backdrop = popovers.querySelector("[data-popover-backdrop]")!;
    backdrop.addEventListener("click", (event) => {
      if (event.target === backdrop) this.closePopover();
    });
    const menu = popovers.querySelector<HTMLElement>("[data-more-popover]");
    if (menu === null) return;
    this.positionMoreMenu(menu, rect);
  }

  private positionMoreMenu(menu: HTMLElement, rect: DOMRect): void {
    menu.style.top = `${rect.bottom + 6}px`;
    menu.style.left = `${Math.max(8, rect.right - menu.offsetWidth)}px`;
  }

  // moreFocus is a one-shot navigation intent. Later focus belongs to the
  // user, including focus restored after an asynchronous menu replacement.
  private moreFocus: string | null = null;

  private focusInMore(selector: string): void {
    this.moreFocus = selector;
    this.applyMoreFocus();
  }

  private applyMoreFocus(): void {
    const selector = this.moreFocus;
    if (selector === null) return;
    this.moreFocus = null;
    this.root
      .querySelector<HTMLElement>(`[data-region="popovers"] [data-more-popover] ${selector}`)
      ?.focus();
  }

  // menuIsNarrow is true when the More menu is rendered as a bottom sheet:
  // either the real viewport is narrow, or the preview phone frame sets
  // [data-viewport="narrow"] on an ancestor. It gates what only exists at
  // that scope — the pushed theme screen, and the focus placement that
  // navigating it needs. Closing the menu on Escape and restoring its
  // trigger is not gated: that contract holds at every width.
  private menuIsNarrow(): boolean {
    return window.matchMedia("(max-width: 600px)").matches
      || this.root.closest("[data-viewport=\"narrow\"]") !== null;
  }

  private showPanels(): void {
    this.closePopover();
    this.popoverOpen = "panels";
    this.panelsMenuCleanup = mountPanelsMenu(this.root, "live", () => this.closePopover(), (panel) => this.panels.toggle(panel));
  }

  private closePopover(): void {
    this.panelsMenuCleanup?.();
    this.panelsMenuCleanup = null;
    this.popoverOpen = null;
    this.themeFlyoutOpen = false;
    this.moreFocus = null;
    this.moreMenuRender = null;
    // After auth teardown the root no longer carries workspace regions;
    // cleanup must be a no-op there, not a throw.
    this.root.querySelector('[data-region="popovers"]')?.replaceChildren();
  }

  private refreshOpenPopover(): void {
    if (this.popoverOpen !== "more") return;
    this.openMore();
  }

  private showMore(): void {
    this.skipPermissions = null;
    this.confirmingYolo = false;
    this.moreFocus = null;
    this.openMore();
  }

  private moreMenuHtml(): string {
    return sessionMenuHtml({
      changePassword: this.authMode === "password",
      loginLabel: this.loginActionLabel(this.statusModel?.session),
      workspaceSecurity: this.security?.lock !== undefined,
      workspaceMethods: this.security?.manageMethods !== undefined,
      fontSize: terminalFontSize(),
      themeName: terminalThemeName(),
      themeFlyoutOpen: this.themeFlyoutOpen,
      customSlots: customThemeSlots(),
      showSidePanel: !this.sidebar.isCollapsed,
      showInfoBar: this.infoBar?.isShown ?? true,
      autoColorProjects: this.autoColorProjects,
      skipPermissions: this.skipPermissions,
      confirmingYolo: this.confirmingYolo,
      skipCloseConfirm: this.skipCloseConfirm,
      skipRemoveProjectConfirm: this.skipRemoveProjectConfirm,
    });
  }

  // openMore mounts the More menu: terminal presentation (font
  // size, theme), auto-color projects, the info bar / yolo / confirm
  // switches, then Git identity… and Sign out.
  // Session management moved out entirely: an authorized recovery gesture
  // renders in the selected session's titlebar, and ordinary removal stays
  // in the sidebar row's normal menu. The panel markup itself lives in
  // session-menu.ts, shared with the preview page.
  // showMore OPENS the menu, as distinct from openMore() which also
  // re-renders it in place after every font step, theme pick and toggle.
  // Only opening re-reads the console settings: the backend is the
  // authority and the menu may last have been open minutes ago, while
  // refetching on each in-place re-render would issue a request per
  // keystroke-equivalent.
  private openMore(): void {
    if (this.popoverOpen === "panels") this.closePopover();
    this.popoverOpen = "more";
    const trigger = this.root.querySelector<HTMLElement>("[data-live-more]");
    if (trigger === null) return;
    const html = this.moreMenuHtml();
    const popovers = region(this.root, "popovers");
    const current = popovers.querySelector<HTMLElement>("[data-more-popover]");
    const focus = focusedModalControl(current);
    // Polling and unchanged settings must not detach a control mid-gesture.
    // Another menu can take over the shared popover region.
    const reuse = this.moreMenuRender?.html === html
      && this.moreMenuRender.node.isConnected && current === this.moreMenuRender.node;
    if (!reuse) this.anchorPopover(html, trigger.getBoundingClientRect());
    const menu = popovers.querySelector<HTMLElement>("[data-more-popover]");
    if (menu === null) { this.moreMenuRender = null; return; }
    if (reuse) this.positionMoreMenu(menu, trigger.getBoundingClientRect());
    else this.moreMenuRender = { html, node: menu };
    // Read fresh whenever the menu opens: the backend is the authority,
    // and a cached copy could show a mode the next launch does not use.
    if (this.skipPermissions === null || this.skipCloseConfirm === null || this.skipRemoveProjectConfirm === null) {
      void this.api.getSettings().then((s) => {
        if (this.disposed || this.popoverOpen !== "more") return;
        this.skipPermissions = s.skip_permissions;
        this.setSkipCloseConfirm(s.skip_close_confirm);
        this.setSkipRemoveProjectConfirm(s.skip_remove_project_confirm);
        this.openMore();
      }).catch((cause) => this.handleError("settings read failed", cause));
    }
    if (reuse) {
      // Retain handlers and user focus, including a pending action's
      // disabled control; rewiring would duplicate the action.
      return;
    }
    // Each toggle row is a button.menuitem whose whole surface is the hit
    // target; its switch is presentation only, so there is nothing inside
    // to exclude from the click.
    const toggle = (selector: string, apply: () => void): void => {
      menu.querySelector(selector)?.addEventListener("click", apply);
    };
    toggle("[data-show-side-panel]", () => {
      this.sidebar.toggleCollapsed();
      this.openMore();
    });
    toggle("[data-show-tutorial]", () => {
      this.closePopover();
      if (this.tutorial.state.phase !== "ready" && this.tutorial.state.phase !== "loading") return;
      this.tutorialFocusRequested = true;
      this.panels.close();
      this.tutorial.reopen();
    });
    toggle("[data-show-info-bar]", () => {
      this.infoBar?.setShown(!(this.infoBar?.isShown ?? true));
      this.openMore();
    });
    toggle("[data-confirm-before-closing]", () => {
      if (this.skipCloseConfirm === null) return;
      // "Confirm before closing a session" is on when the stored skip is not
      // true;
      // flipping the row flips that, so the new skip is the old confirm.
      void this.writeSettings({ skip_close_confirm: confirmsBeforeClose(this.skipCloseConfirm) });
    });
    toggle("[data-confirm-before-removing]", () => {
      if (this.skipRemoveProjectConfirm === null) return;
      void this.writeSettings({ skip_remove_project_confirm: confirmsBeforeRemove(this.skipRemoveProjectConfirm) });
    });
    // Yolo shares the row hit target, but enabling expands an inline
    // confirmation first, and its buttons must not re-trigger the row, so
    // it is wired explicitly rather than through toggle().
    const toggleYolo = (): void => this.toggleYoloMode();
    menu.querySelector("[data-skip-permissions]")?.addEventListener("click", (event) => {
      const target = event.target as HTMLElement;
      if (
        target.closest("[data-yolo-cancel]") !== null ||
        target.closest("[data-yolo-confirm]") !== null
      ) {
        return;
      }
      toggleYolo();
    });
    menu.querySelector("[data-yolo-cancel]")?.addEventListener("click", () => {
      this.confirmingYolo = false;
      this.openMore();
    });
    menu.querySelector("[data-yolo-confirm]")?.addEventListener("click", () => {
      void this.writeSettings({ skip_permissions: true });
    });
    menu.querySelector("[data-auto-color]")?.addEventListener("click", () => {
      this.autoColorProjects = setAutoColorProjects(!this.autoColorProjects);
      this.sidebarModel.setAutoColorProjects(this.autoColorProjects);
      this.sidebar.render();
      // The tint is part of the session's identity, so the chip repaints
      // with the tiles rather than waiting for the next status tick.
      const detail = this.statusModel?.session;
      if (detail !== null && detail !== undefined) this.renderChrome(detail);
      this.openMore();
    });
    const stepFont = (delta: number): void => {
      const applied = setTerminalFontSize(terminalFontSize() + delta);
      this.terminal?.setFontSize(applied);
      const label = menu.querySelector("[data-font-size]");
      if (label !== null) label.textContent = String(applied);
      if (this.moreMenuRender?.node === menu) {
        this.moreMenuRender.html = this.moreMenuHtml();
      }
    };
    menu.querySelector("[data-font-dec]")?.addEventListener("click", () => stepFont(-1));
    menu.querySelector("[data-font-inc]")?.addEventListener("click", () => stepFont(1));
    // Pushing and popping replace the focused control with a freshly
    // mounted menu, so focus must be placed by hand or it falls to <body>
    // and the keyboard loses the surface entirely. Entering lands on the
    // pushed screen's back control — the way out is the first thing a
    // keyboard reaches — and leaving returns to the row that pushed it.
    menu.querySelector("[data-theme-flyout]")?.addEventListener("click", () => {
      this.themeFlyoutOpen = !this.themeFlyoutOpen;
      // The intent is recorded BEFORE the mount, because the mount is what
      // applies it — and the menu mounts again on its own when a pending
      // settings read resolves. Focusing after the call would place focus
      // once and lose it to the next remount.
      this.moreFocus = this.menuIsNarrow()
        ? (this.themeFlyoutOpen ? "[data-theme-back]" : "[data-theme-flyout]")
        : null;
      this.openMore();
    });
    menu.querySelector("[data-theme-back]")?.addEventListener("click", () => {
      this.themeFlyoutOpen = false;
      this.moreFocus = this.menuIsNarrow() ? "[data-theme-flyout]" : null;
      this.openMore();
    });
    // Escape closes the menu and restores the trigger at every width,
    // which is what every other popover here does. Only stepping back one
    // level from the pushed theme screen is narrow-only: the narrow sheet
    // pushes a full screen, and dismissing it outright would discard a
    // navigation the user did not undo; the desktop flyout has no
    // pushed level to step back from.
    menu.addEventListener("keydown", (event) => {
      const key = (event as KeyboardEvent).key;
      if (key === "ArrowDown" || key === "ArrowUp" || key === "Home" || key === "End") {
        const navigationRoot = this.themeFlyoutOpen && this.menuIsNarrow()
          ? menu.querySelector<HTMLElement>("[data-theme-list]") ?? menu
          : menu;
        const rows = menuRows(navigationRoot);
        if (rows.length === 0) return;
        const current = rows.indexOf(document.activeElement as HTMLElement);
        const direction = key === "ArrowDown" ? 1 : key === "ArrowUp" ? -1 : 0;
        const next = key === "Home" ? 0 : key === "End" ? rows.length - 1
          : current < 0 ? (direction > 0 ? 0 : rows.length - 1)
            : (current + direction + rows.length) % rows.length;
        event.preventDefault();
        rows[next]!.focus();
        return;
      }
      if (key !== "Escape") return;
      event.preventDefault();
      if (this.themeFlyoutOpen && this.menuIsNarrow()) {
        this.themeFlyoutOpen = false;
        this.openMore();
        this.focusInMore("[data-theme-flyout]");
        return;
      }
      this.closePopover();
      this.root.querySelector<HTMLElement>("[data-live-more]")?.focus();
    });
    menu.querySelector("[data-theme-close]")?.addEventListener("click", () => {
      this.closePopover();
    });
    menu.querySelector("[data-settings-sheet-close]")?.addEventListener("click", () => {
      this.closePopover();
      this.root.querySelector<HTMLElement>("[data-live-more]")?.focus();
    });
    menu.querySelectorAll<HTMLButtonElement>("[data-theme-pick]").forEach((button) => {
      button.addEventListener("click", () => {
        const applied = setTerminalThemeName(button.dataset.themePick!);
        this.terminal?.setTheme(
          terminalThemeFor(this.mountedTerminalProvider(), resolveTerminalTheme(applied)));
        this.applyTerminalGround();
        this.openMore();
      });
    });
    menu.querySelector("[data-theme-custom-open]")?.addEventListener("click", () => {
      this.closePopover();
      this.openThemeDialog();
    });
    menu.querySelector("[data-harness-login]")?.addEventListener("click", () => { this.closePopover(); this.openHarnessLogin(); });
    menu.querySelector("[data-github-connect]")?.addEventListener("click", () => { this.closePopover(); this.openGitHubConnect(); });
    menu.querySelector("[data-git-identities]")?.addEventListener("click", () => {
      this.closePopover();
      this.sidebar.openGitIdentitySettings();
    });
    menu.querySelector("[data-change-password]")?.addEventListener("click", () => {
      this.closePopover();
      const host = this.root.querySelector<HTMLElement>('[data-region="modals"]');
      if (host !== null) openPasswordChange(host);
    });
    menu.querySelector("[data-workspace-lock]")?.addEventListener("click", () => { this.closePopover(); this.security?.lock?.(); });
    menu.querySelector("[data-workspace-methods]")?.addEventListener("click", () => { this.closePopover(); this.security?.manageMethods?.(); });
    menu
      .querySelector<HTMLButtonElement>("[data-live-logout]")
      ?.addEventListener("click", (event) =>
        void this.signOut(event.currentTarget as HTMLButtonElement));
    // Every mount ends here, including the one a resolving settings read
    // triggers, so a focus the user navigated to survives a remount
    // they did not ask for.
    if (!reuse) restoreModalFocus(menu, focus);
    this.applyMoreFocus();
  }

  // The titlebar button and both menu rows exist only while the selected
  // session has a login to finish.
  private loginActionLabel(detail: SessionDetail | null | undefined): string | null {
    if (detail == null || this.connection !== "connected" || this.terminal === null) return null;
    const pending = this.loginCoordinator.pendingFor(detail.id);
    if (pending.length === 0 && !this.codexLoginHelpers.has(detail.id)) return null;
    return detail.provider === "codex" ? "Open login helper" : harnessLoginLabel(pending);
  }

  private openHarnessLogin(): void {
    if (this.selectedSessionID === null || this.loginActionLabel(this.statusModel?.session) === null) return;
    this.mountHarnessLogin(this.selectedSessionID, undefined);
  }

  // An open menu keeps the rows it was built with, so a login that ends
  // while it is open takes its row away in place.
  private dropStaleLoginRows(): void {
    const popovers = region(this.root, "popovers");
    if (popovers.querySelector("[data-harness-login]") === null || this.loginActionLabel(this.statusModel?.session) !== null) return;
    if (popovers.querySelector("[data-more-popover]") !== null) {
      this.refreshOpenPopover();
      return;
    }
    const row = popovers.querySelector<HTMLElement>("[data-terminal-menu-popover] [data-harness-login]");
    if (row === null) return;
    const focused = row === document.activeElement;
    const menu = row.parentElement!;
    row.remove();
    if (focused) menu.querySelector<HTMLElement>("[role=menuitem]")?.focus();
  }

  private tryInitialCodexHelper(): void {
    const session = this.selectedSessionID;
    if (session === null || this.connection !== "connected" || this.terminal === null
      || !this.codexLoginHelpers.has(session) || this.offeredCodexHelpers.has(session)
      || this.dismissedCodexHelpers.has(session) || this.popoverOpen !== null
      || this.root.querySelector("[role=dialog], [data-terminal-menu-popover]") !== null) return;
    if (this.mountHarnessLogin(session, undefined)) this.offeredCodexHelpers.add(session);
  }

  // One dialog slot serves the manual menu and the coordinator's automatic
  // raise. Whichever opens replaces what was there, closing the old one as
  // context-lost so its attempts are never marked answered.
  private mountHarnessLogin(session: string, listing: HarnessLoginListing | undefined): boolean {
    const host = this.root.querySelector<HTMLElement>('[data-region="modals"]');
    if (host === null) return false;
    this.harnessLogin?.close("context-lost");
    const codex = this.statusModel?.session?.provider === "codex" && this.selectedSessionID === session && this.connection === "connected";
    const terminal = codex ? this.terminal : null;
    const epoch = this.epoch;
    const currentTerminal = () => terminal !== null && !this.disposed && !this.authRequired
      && this.terminal === terminal && this.epoch === epoch && this.selectedSessionID === session && this.connection === "connected";
    if (codex) this.codexLoginHelpers.add(session);
    const dialog = new HarnessLoginDialog(host, session, {
      listing,
      initialCodex: codex,
      terminalHost: codex ? this.root.querySelector<HTMLElement>("[data-live-terminal]") ?? undefined : undefined,
      focusTerminal: () => { if (currentTerminal()) terminal!.mounted.focus(); },
      activateTerminalKey: (key) => {
        if (!currentTerminal()) return false;
        if (key === "Enter") terminal!.mounted.sendInput("\r");
        else terminal!.mounted.activateSoftKey(key === "ArrowUp" ? "arrow-up" : "arrow-down");
        return true;
      },
      onCompleted: () => {
        if (codex && this.tutorialLogin !== null) this.noteTutorialFact([{ name: "session_signed_in", arg: null }]);
        this.codexLoginHelpers.delete(session);
        this.dismissedCodexHelpers.delete(session);
        if (this.statusModel?.session) this.renderChrome(this.statusModel.session);
      },
      onAuthorizationLost: this.security ? () => this.security?.authenticationLost() : undefined,
      onClosed: (attempts, reason) => {
        if (this.harnessLogin === dialog) this.harnessLogin = null;
        if (codex && reason === "dismissed") this.dismissedCodexHelpers.add(session);
        this.loginCoordinator.closed(session, attempts, reason);
        // An automatic raise had no user gesture to return focus to;
        // the live terminal is where the user left off.
        if (!this.disposed && reason !== "context-lost" && document.activeElement === document.body) this.terminal?.mounted.focus();
      },
    });
    this.harnessLogin = dialog;
    this.applyTerminalGround();
    return true;
  }

  private openGitHubConnect(onClosed?: () => void): void {
    if (this.disposed || this.githubConnect !== null) return;
    const region = this.root.querySelector<HTMLElement>('[data-region="modals"]');
    if (region === null) return;
    const client = this.authMode === "none" ? {
      status: async () => ({ outcome: "authentication-required" }),
      current: async () => ({ csrf: "unavailable", attempt: null }),
      start: async (): Promise<never> => { throw new Error("Deck authentication is required"); },
      cancel: async (): Promise<never> => { throw new Error("Deck authentication is required"); },
    } : new HttpGitHubConnectClient(() => this.requireAuthentication());
    this.githubConnect = new GitHubConnectDialog(region, client,
      {
        controllerID: this.controllerID,
        onClosed: () => { this.githubConnect = null; onClosed?.(); },
        onConnected: (login) => {
          this.installFacts = { ...this.installFacts, github: true };
          this.sidebar.githubConnected(login);
          this.repaintTutorial();
        },
      });
    this.githubConnect.open();
  }

  // openThemeDialog mounts the custom theme editor. The dialog
  // owns a draft; Apply theme is the only path that writes it — through
  // the same theme storage the pane already reads — and selects the
  // custom theme, so the terminal repaints exactly what the dialog
  // previewed.
  private openThemeDialog(): void {
    const host = this.root.querySelector<HTMLElement>('[data-region="modals"]');
    if (host === null) return;
    new ThemeDialog(host, customThemeSlots(), (slots) => {
      setCustomThemeSlots(slots);
      const applied = setTerminalThemeName("custom");
      this.terminal?.setTheme(
        terminalThemeFor(this.mountedTerminalProvider(), resolveTerminalTheme(applied)));
      this.applyTerminalGround();
    });
  }

  // writeSettings persists one changed setting and paints what the
  // SERVER stored, never what was requested. A failure drops both values
  // to unknown so the switches render disabled rather than showing a
  // state that may not be real.
  //
  // PUT replaces the whole document, so the unchanged settings are sent
  // alongside — `change` carries ONLY the setting the user just
  // touched, or the others are overwritten with it. An unknown value is
  // sent as false, which is the careful direction because every setting
  // names what it SKIPS. Both mirrors are read and cleared together, so
  // in practice a write never happens with one of them unknown.
  private async writeSettings(change: Partial<ConsoleSettings>): Promise<void> {
    this.confirmingYolo = false;
    try {
      const saved = await this.api.saveSettings({
        skip_permissions: this.skipPermissions ?? false,
        skip_close_confirm: this.skipCloseConfirm ?? false,
        skip_remove_project_confirm: this.skipRemoveProjectConfirm ?? false,
        ...change,
      });
      if (this.disposed) return;
      this.skipPermissions = saved.skip_permissions;
      this.setSkipCloseConfirm(saved.skip_close_confirm);
      this.setSkipRemoveProjectConfirm(saved.skip_remove_project_confirm);
    } catch (cause) {
      if (this.disposed) return;
      this.skipPermissions = null;
      this.setSkipCloseConfirm(null);
      this.setSkipRemoveProjectConfirm(null);
      this.handleError("settings write failed", cause);
    }
    if (!this.disposed && this.popoverOpen === "more") this.openMore();
  }


  // toggleYoloMode is the one yolo gesture: the menu's row, its switch,
  // and the ⌥⌘Y chord all arrive here. Turning the mode ON expands the
  // More menu into its inline confirmation — opening it when it was
  // closed, so the consequence of granting is never hidden; turning it
  // OFF is immediate. An unread setting does nothing: the switch renders
  // disabled for exactly that state, and the chord must not be a path
  // around it.
  private toggleYoloMode(): void {
    if (this.skipPermissions === null) return;
    if (!this.skipPermissions) {
      this.confirmingYolo = true;
      this.openMore();
      return;
    }
    void this.writeSettings({ skip_permissions: false });
  }

  // runShortcut executes one app chord. The toggles re-render an open
  // More menu so its switches never disagree with what the chord did,
  // and ⌘R resolves its target through the sidebar — an open row menu
  // first, then the selection — exactly as the menu's own Rename rows
  // do.
  private runShortcut(command: ShortcutCommand): void {
    switch (command) {
      case "toggleRail":
        this.sidebar.toggleCollapsed();
        this.refreshOpenPopover();
        return;
      case "toggleInfoBar":
        this.infoBar?.setShown(!(this.infoBar?.isShown ?? true));
        this.refreshOpenPopover();
        return;
      case "toggleYolo":
        this.toggleYoloMode();
        return;
      case "renameSelection": {
        if (this.popoverOpen !== null) this.closePopover();
        const target = this.sidebar.renameTarget();
        if (target !== null) this.sidebar.beginRename(target);
        return;
      }
    }
  }

  // setSkipCloseConfirm records the setting and hands it to the sidebar,
  // which owns the confirmation. Neither side interprets it —
  // confirmsBeforeClose does.
  private setSkipCloseConfirm(skip: boolean | null): void {
    this.skipCloseConfirm = skip;
    this.sidebar.setSkipCloseConfirm(skip);
  }

  private setSkipRemoveProjectConfirm(skip: boolean | null): void {
    this.skipRemoveProjectConfirm = skip;
    this.sidebar.setSkipRemoveProjectConfirm(skip);
  }

  private project(id: string): Project | null {
    return this.sidebarModel.groups.find((group) => group.project.id === id)?.project ?? null;
  }

  // emptyPane is the pane's one empty-state enum, not
  // a boolean: `off` while a session owns the pane, `loading` until the
  // first registry read answers, `error` while the latest read has
  // failed, and only then `no-session` when a project is in context with
  // nothing open or `no-project` when a successful read found nothing —
  // a read that has not answered is not an empty database. Deriving it
  // from the selection and the sidebar's groups keeps the pane and the
  // rail from disagreeing about what is in context.
  private get emptyPane(): EmptyPane {
    if (this.selectedSessionID !== null) return "off";
    const list = this.sidebarModel.listState;
    if (list === "loading") return this.loadingPhase() === "failed" ? "error" : "loading";
    if (list === "error") return "error";
    return this.sidebarModel.groups.length > 0 ? "no-session" : "no-project";
  }

  private handleError(_message: string, cause?: unknown): void {
    if (this.disposed) return;
    if (!(cause instanceof ApiError) || cause.status !== 401) return;
    this.requireAuthentication();
  }

  private requireAuthentication(): void {
    if (this.disposed || this.authRequired) return;
    this.githubConnect?.dispose();
    this.githubConnect = null;
    if (this.security) { this.security.authenticationLost(); return; }
    this.authRequired = true;
    this.stopTutorialTimers();
    this.tutorial.dispose();
    this.starter.dispose();
    this.tutorialPanel?.dispose();
    this.tutorialPanel = null;
    ++this.lifecycle;
    ++this.epoch;
    this.disposeSession();
    this.assetsFeed.stop();
    this.selectedSessionID = null;
    this.sidebarModel.selectedSessionID = null;
    this.sidebarModel.groups = [];
    this.sidebarModel.error = null;
    if (this.sidebarTimer !== null) {
      window.clearInterval(this.sidebarTimer);
      this.sidebarTimer = null;
    }
    this.outputActivityEvents?.close();
    this.outputActivityEvents = null;
    this.clearOutputActivity();
    this.sidebar.dispose();
    this.root.innerHTML = loginHtml({
      mode: this.authMode,
      state: "signed-out",
      reason: "Your browser session has expired.",
    });
    bindLoginActions(this.root, this.authMode, () => window.location.reload());
  }

  private async signOut(button: HTMLButtonElement): Promise<void> {
    if (this.disposed || this.authRequired) return;
    button.disabled = true;
    try {
      await this.api.logout();
      // A hard navigation, not an in-app swap: the reload drops the current
      // page state and the fresh load answers from the server's session
      // check - where the logout epoch rejects any cookie a straggler
      // renewal response managed to set after the logout cleared the jar.
      window.location.replace(window.location.pathname);
    } catch (cause) {
      if (this.disposed) return;
      const message = cause instanceof Error ? cause.message : String(cause);
      this.handleError(message, cause);
      if (!this.authRequired) {
        button.disabled = false;
      }
    }
  }

  private syncAttachmentToStatus(detail: SessionDetail, mayConnect: boolean): void {
    if (!isVerifiedRunningSession(detail)) return;
    if (this.terminal === null) {
      void this.mountTerminal(detail, this.epoch);
      return;
    }
    if (mayConnect && this.connection !== "connected") {
      this.reconnect(false);
      return;
    }
    this.renderAttachmentState();
  }

  private renderLoading(): void {
    if (this.disposed) return;
    this.applyTerminalGround();
    region(this.root, "terminal-titlebar").innerHTML =
      `<span class="text-body-sm text-text-tertiary">Loading session…</span>
      <span class="ml-auto"></span>`;
    region(this.root, "terminal").innerHTML = `
      <div class="grid h-full place-items-center px-6 text-center font-mono text-[min(var(--type-mono-size),calc(var(--type-body-sm-size)*var(--type-mono-xh-ratio)))] text-(--t-faint)">
        <div>
          <div class="mx-auto mb-3 h-5 w-5 animate-spinner rounded-circle border-2 border-(--t-faint) border-t-(--t-accent)"></div>
          <p>Loading session…</p>
        </div>
      </div>
    `;
    region(this.root, "tasks-body").innerHTML = "";
    region(this.root, "files-body").innerHTML = "";
  }

  // renderEmptyPane paints the pane's empty states from the emptyPane
  // enum: `no-session` when a project is in context, `no-project` when a
  // successful read found nothing, `unread` while the registry read has
  // not succeeded. The states share one anatomy
  // (ui/empty-pane.ts) and sit on the page surface — the app
  // speaking, not a session's ground. `unread` carries no create action
  // and sheds the docked panels: an unread registry offered as a
  // known-empty database invites onboarding against data nobody read.
  private renderEmptyPane(): void {
    if (this.disposed) return;
    // No session is open, so nothing shows the count.
    this.assetsFeed.stop();
    this.paintEmptySurface();
    const kind = this.emptyPane;
    const target = kind === "no-session" ? this.sidebar.newSessionTarget() : null;
    const project = target !== null
      ? { name: target.name, tint: target.tint }
      : null;
    // Unreachable through the current callers — every one holds no
    // selection — but painting an empty state over a live session is
    // worse than skipping the call.
    if (kind === "off") return;
    region(this.root, "login-action").replaceChildren();
    region(this.root, "terminal-titlebar").innerHTML = emptyTitlebarHtml("live", {
      kind,
      project,
      filesPressed: this.panels.isOpen("files"),
      tasksPressed: this.panels.isOpen("tasks"),
      sessions: kind === "no-session",
    });
    const phase = this.loadingPhase();
    region(this.root, "terminal").innerHTML = emptyPaneHtml({
      kind,
      projectName: project?.name ?? null,
      error: this.sidebarModel.error ?? "",
      phase: phase === "failed" ? "slow" : phase,
      session: this.reattachingSessionName(),
      cached: this.sidebarModel.stale,
    });
    this.root.querySelector("[data-live-more]")?.addEventListener("click", () => this.showMore());
    this.root.querySelector("[data-live-panels]")?.addEventListener("click", () => this.showPanels());
    this.root.querySelector("[data-live-sessions]")?.addEventListener("click", () => this.sidebar.open());
    this.root.querySelector("[data-live-tasks]")?.addEventListener("click", () => {
      this.panels.toggle("tasks");
    });
    this.root.querySelector("[data-live-files]")?.addEventListener("click", () => {
      this.panels.toggle("files");
    });
    this.root.querySelector("[data-empty-action]")?.addEventListener("click", () => {
      // The pane's action reaches the same dialogs the sidebar's own rows
      // open, so there is exactly one target for each gesture.
      if (kind === "no-project") this.sidebar.openNewProject();
      else if (kind === "error") void this.refreshSidebar();
      else if (target !== null) this.sidebar.openNewSession(target.id);
    });
    if (target !== null) {
      this.mountEmptyPanels(target.id);
    } else {
      // No project is readable in context, so neither docked panel has
      // anything to read; the triggers are gone and the panes empty out
      // with them.
      this.disposeEmptyPanels();
      region(this.root, "tasks-body").innerHTML = "";
      region(this.root, "files-body").innerHTML = "";
    }
    this.infoBar?.refresh();
  }

  // paintEmptySurface repaints the terminal card on the page surface. The
  // empty pane is interface chrome; leaving the emulator's themed ground
  // under it makes the app's own instruction read as terminal output from
  // a session that is not running, and mono metrics ride the same
  // data-mono scope, so the attribute leaves with the ground.
  private paintEmptySurface(): void {
    const pane = this.root.querySelector<HTMLElement>('[data-region="terminal"]');
    if (pane !== null) {
      delete pane.dataset.mono;
      pane.style.backgroundColor = "var(--color-surface)";
    }
    const card = this.root.querySelector<HTMLElement>('[data-region="terminal-card"]');
    if (card !== null) card.style.backgroundColor = "var(--color-surface)";
  }

  // The no-session state keeps Files and Tasks, and both read the project
  // in context — the same panels a selection would mount. The guard keeps
  // the sidebar poll's repaints from rebuilding them: a fresh EventSource
  // per refresh would leak subscriptions against the same project.
  private mountEmptyPanels(projectID: string): void {
    if (this.emptyPanelsProject === projectID && this.taskmaster !== null && this.files !== null) return;
    this.disposeEmptyPanels();
    this.emptyPanelsProject = projectID;
    this.mountTaskmaster(projectID, this.epoch);
    this.mountFiles(projectID);
  }

  private disposeEmptyPanels(): void {
    this.taskmaster?.dispose();
    this.taskmaster = null;
    this.taskmasterModel = null;
    this.taskmasterEvents?.close();
    this.taskmasterEvents = null;
    this.files?.dispose();
    this.files = null;
    this.filesModel = null;
    this.filesProjectID = null;
    this.emptyPanelsProject = null;
  }
}
