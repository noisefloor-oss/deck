import { ONBOARDING_PREVIEW_STATES, parseOnboardingPreview, type OnboardingPreviewId } from "./onboarding-states.ts";
// The enumerable catalog of URL-drivable preview states.
//
// preview.ts reads the params each entry names to reach that state from a
// bare preview.html load — no scripted interaction — and verifies the
// `ready` selector is present before marking the state reached. The capture
// harness (tools/mvp-shots.mjs) imports this list and iterates it instead of
// hard-coding names, so a state added here is captured automatically and a
// capture that fails to reach its state fails loudly rather than returning
// the default view.
//
// This module is pure (no DOM, no browser globals) so it runs under plain
// node tests and in the harness.

import { SECURITY_PREVIEW_STATES, parseSecurityPreview, type SecurityPreviewFigure } from "./security-states.ts";

export const PROJECT_PREVIEW_STATES = ["enable", "enabling", "failed", "noports", "setup", "settings", "checking", "up", "down", "checkfailed", "disableconfirm", "disablefailed", "disabled", "unconfigured", "static", "static-only", "loading", "menu-disabled", "menu-enabled", "menu-unconfigured"] as const;
export type ProjectPreviewParam = typeof PROJECT_PREVIEW_STATES[number];

export type MenuParam = "settings" | "theme" | "project" | "session" | "projects" | "panels";
export type DialogParam =
  | "new-session"
  | "open-project"
  | "choose-directory"
  | "git-identity"
  | "choose-git-identity"
  | "rename-project"
  | "add-prd"
  | "custom-theme"
  | "remove-project"
  | "close-session"
  | "delete-assets"
  | "delete-outcome";
export type HoverParam = "project" | "session" | "row-controls";
export type RenameParam = "project" | "session";
export type PanelParam = "files" | "tasks" | "assets" | "none";
// What the Assets panel shows: the scan pending or empty, the fixture
// listing at rest, with five files picked, with Build scratch
// stopped at the scan bound, or after a partly refused delete.
export type AssetsParam = "loading" | "empty" | "list" | "selected" | "truncated" | "after";
export type ViewerParam =
  | "text"
  | "binary"
  | "empty"
  | "code"
  | "image"
  | "image-actual"
  | "image-alpha"
  | "image-oversized"
  | "image-undecodable"
  | "image-gone";
export type RecoveryParam = "restarting" | "replayed" | "failed";
export type EmptyParam = "no-session" | "no-project" | "loading" | "error";
// The phone device frame around every screen. It is capture
// scaffolding, not product: it reserves the device mock's bezel, status bar
// and home band so the app's content lands at the same origin as the
// reference, and every reserved band is masked in the gate.
export type FrameParam = "phone";
// Sidebar open state for the phone sessions sheet:
// on a narrow viewport the sidebar overlays the terminal as a full-screen
// sheet, so the preview needs a URL axis that uncollapses it.
export type SidebarParam = "open";

const MENUS: readonly MenuParam[] = ["settings", "theme", "project", "session", "projects", "panels"];
const DIALOGS: readonly DialogParam[] = [
  "new-session",
  "open-project",
  "choose-directory",
  "git-identity",
  "choose-git-identity",
  "rename-project",
  "add-prd",
  "custom-theme",
  "remove-project",
  "close-session",
  "delete-assets",
  "delete-outcome",
];
const HOVERS: readonly HoverParam[] = ["project", "session", "row-controls"];
const RENAMES: readonly RenameParam[] = ["project", "session"];
const PANELS: readonly PanelParam[] = ["files", "tasks", "assets", "none"];
const ASSETS: readonly AssetsParam[] = ["loading", "empty", "list", "selected", "truncated", "after"];
const VIEWERS: readonly ViewerParam[] = [
  "text",
  "binary",
  "empty",
  "code",
  "image",
  "image-actual",
  "image-alpha",
  "image-oversized",
  "image-undecodable",
  "image-gone",
];
const RECOVERIES: readonly RecoveryParam[] = ["restarting", "replayed", "failed"];

const EMPTIES: readonly EmptyParam[] = ["no-session", "no-project", "loading", "error"];
const FRAMES: readonly FrameParam[] = ["phone"];
const SIDEBARS: readonly SidebarParam[] = ["open"];

// The typed result of parsing a preview URL's query string. Every axis is
// null when unset (except tasks/recovery/delayedRemoval, which keep their
// pre-existing defaults) so the caller can tell "not requested" from "off".
export interface PreviewParams {
  projectPreview: ProjectPreviewParam | null;
  projectPreviewDetails: boolean;
  security: SecurityPreviewFigure | null;
  onboarding: OnboardingPreviewId | null;
  menu: MenuParam | null;
  dialog: DialogParam | null;
  hover: HoverParam | null;
  rename: RenameParam | null;
  panel: PanelParam | null;
  // assets is meaningful only with panel=assets.
  assets: AssetsParam | null;
  // assetsUnseen is how many fixture files are new since the Assets panel
  // was last open: the trigger's badge and the info bar's news line.
  assetsUnseen: number | null;
  empty: EmptyParam | null;
  filesDepth: number | null;
  filesSelect: string | null;
  // filesConflict seeds the upload-collision ask: the dialog
  // the server's 409 puts on screen, reached the way a picker gesture
  // reaches it. Meaningful only with panel=files.
  filesConflict: boolean;
  filesUpload: "live" | "details" | "unconfirmed" | null;
  // task opens one task's detail from the tasks panel, by id. Meaningful only
  // with panel=tasks; resolution against the fixture happens at application
  // time, where an unknown id stops the page rather than certifying the list
  // as the detail.
  task: string | null;
  sidebarPollMS: number | null;
  viewer: ViewerParam | null;
  // filesExpanded gives the open viewer the whole window, the way the
  // viewer's expand button does. Meaningful only with a viewer.
  filesExpanded: boolean;
  tasks: "ready" | "onboarding";
  recovery: RecoveryParam | "live";
  delayedRemoval: boolean;
  frame: FrameParam | null;
  // phoneScreen draws the phone frame flush with the device's top edge
  // (frame=phone-screen), for references framed to the 393x852 screen
  // itself rather than to a figure with a 12px margin above it.
  phoneScreen: boolean;
  sidebar: SidebarParam | null;
  // roots is how many allowed roots the fixture's mount listing offers. One
  // is the deployment's shape and the gate's; two exists so a probe can
  // drive the picker's way back from a root's top to the mount.
  roots: 1 | 2;
  // mountDelayMS holds the mount listing's answer back that long, so a probe
  // can land it while you type in the Add project dialog.
  mountDelayMS: number | null;
  // projectPath pre-fills the New project dialog's path, so Browse opens the
  // picker straight at a directory the way a returning user reaches it —
  // without the mount listing ever having been read.
  projectPath: string | null;
  // identities "none" starts with no Git identity, so the Git identity
  // dialog opens the first-identity form.
  identities: "some" | "none";
}

// The fixture directory each files-depth value navigates to (the project's
// canonical_path tree, mirrored in app/fixtures.ts). Depth 0 is the root;
// deeper values exercise the breadcrumb's elision.
export const FILES_DEPTH_PATHS: readonly string[] = [
  "", // 0: root
  "docs", // 1
  "internal/provider", // 2
  "internal/db", // 3
  "internal/db/migrations", // 4
];

// parsePreviewParams validates a preview URL's query string and returns the
// typed axes. An unknown value is a hard error — the preview must fail loudly
// rather than silently render the default state.
export function parsePreviewParams(search: URLSearchParams): PreviewParams {
  const menu = parseEnum("menu", search.get("menu"), MENUS);
  let dialog = parseEnum("dialog", search.get("dialog"), DIALOGS);
  const hover = parseEnum("hover", search.get("hover"), HOVERS);
  let rename = parseEnum("rename", search.get("rename"), RENAMES);
  // rename-project appears in the dialog list but rename is
  // inline, so it aliases rename=project rather than a dialog.
  if (dialog === "rename-project") {
    dialog = null;
    rename = "project";
  }
  const panel = parseEnum("panel", search.get("panel"), PANELS);
  const viewer = parseEnum("viewer", search.get("viewer"), VIEWERS);
  const empty = parseEnum("empty", search.get("empty"), EMPTIES);
  const sidebar = parseEnum("sidebar", search.get("sidebar"), SIDEBARS);
  const frameRaw = search.get("frame");

  let assetsUnseen: number | null = null;
  const assetsUnseenRaw = search.get("assets-unseen");
  if (assetsUnseenRaw !== null) {
    if (!/^[1-3]$/.test(assetsUnseenRaw)) {
      throw new Error(`invalid assets-unseen: ${JSON.stringify(assetsUnseenRaw)}`);
    }
    assetsUnseen = Number(assetsUnseenRaw);
  }

  let filesDepth: number | null = null;
  const filesDepthRaw = search.get("files-depth");
  if (filesDepthRaw !== null) {
    if (!/^\d+$/.test(filesDepthRaw)) {
      throw new Error(`invalid files-depth: ${JSON.stringify(filesDepthRaw)}`);
    }
    const depth = Number(filesDepthRaw);
    if (depth < 0 || depth >= FILES_DEPTH_PATHS.length) {
      throw new Error(
        `files-depth ${depth} is out of range (0..${FILES_DEPTH_PATHS.length - 1})`,
      );
    }
    filesDepth = depth;
  }

  const filesSelect = search.get("files-select");
  const task = search.get("task");
  if (task !== null && !/^[\w.-]+$/.test(task)) {
    throw new Error(`invalid task: ${JSON.stringify(task)}`);
  }

  const projectPath = search.get("project-path");
  if (projectPath !== null && !projectPath.startsWith("/")) {
    throw new Error(`invalid project-path: ${JSON.stringify(projectPath)}`);
  }

  const recoveryRaw = search.get("recovery");
  const recovery: RecoveryParam | "live" =
    recoveryRaw !== null && (RECOVERIES as readonly string[]).includes(recoveryRaw)
      ? (recoveryRaw as RecoveryParam)
      : "live";

  // ?sidebar-poll=<ms> runs the live workspace's refresh-and-render loop on
  // that interval. Fixture data never changes, so the preview has no poll
  // of its own; a probe that needs to watch what a poll tick does to an
  // unchanged rail drives it from here.
  let sidebarPollMS: number | null = null;
  const sidebarPollRaw = search.get("sidebar-poll");
  if (sidebarPollRaw !== null) {
    if (!/^\d+$/.test(sidebarPollRaw)) {
      throw new Error(`invalid sidebar-poll: ${JSON.stringify(sidebarPollRaw)}`);
    }
    const ms = Number(sidebarPollRaw);
    if (ms < 50 || ms > 60_000) {
      throw new Error(`sidebar-poll ${ms} is out of range (50..60000)`);
    }
    sidebarPollMS = ms;
  }

  let mountDelayMS: number | null = null;
  const mountDelayRaw = search.get("mount-delay");
  if (mountDelayRaw !== null) {
    if (!/^\d+$/.test(mountDelayRaw) || Number(mountDelayRaw) > 60_000) {
      throw new Error(`invalid mount-delay: ${JSON.stringify(mountDelayRaw)}`);
    }
    mountDelayMS = Number(mountDelayRaw);
  }

  return {
    projectPreview: parseEnum("project-preview", search.get("project-preview"), PROJECT_PREVIEW_STATES),
    projectPreviewDetails: search.get("project-preview-details") === "1",
    security: parseSecurityPreview(search),
    onboarding: parseOnboardingPreview(search),
    menu,
    dialog,
    hover,
    rename,
    panel,
    assets: parseEnum("assets", search.get("assets"), ASSETS),
    assetsUnseen,
    empty,
    filesDepth,
    filesSelect,
    filesConflict: search.get("files-conflict") === "1",
    filesUpload: parseEnum("files-upload", search.get("files-upload"), ["live", "details", "unconfirmed"] as const),
    task,
    sidebarPollMS,
    viewer,
    filesExpanded: search.get("expanded") === "1",
    tasks: search.get("tasks") === "onboarding" ? "onboarding" : "ready",
    recovery,
    delayedRemoval: search.get("delayed-removal") === "1",
    frame: frameRaw === "phone-screen" ? "phone" : parseEnum("frame", frameRaw, FRAMES),
    phoneScreen: frameRaw === "phone-screen",
    sidebar,
    roots: parseEnum("roots", search.get("roots"), ["2"] as const) === "2" ? 2 : 1,
    mountDelayMS,
    projectPath,
    identities: parseEnum("identities", search.get("identities"), ["none"] as const) === "none" ? "none" : "some",
  };
}

function parseEnum<T extends string>(
  key: string,
  raw: string | null,
  allowed: readonly T[],
): T | null {
  if (raw === null) return null;
  if ((allowed as readonly string[]).includes(raw)) return raw as T;
  throw new Error(`invalid ${key}: ${JSON.stringify(raw)}`);
}

// One capture state: the query params that reach it and the selector that
// proves it was reached.
export interface PreviewState {
  viewport?: { width: number; height: number };
  // name is the stable capture slug (the <name>.png filename).
  name: string;
  // params are the URL query params that reach the state from a bare load.
  params: Record<string, string>;
  // ready is the selector that must be present once the state is applied.
  ready: string;
}

// The capture harness iterates this list; the order is the capture order.
export const PREVIEW_STATES: readonly PreviewState[] = [
  ...SECURITY_PREVIEW_STATES,
  ...ONBOARDING_PREVIEW_STATES,
  { name: "baseline", params: {}, ready: "[data-projects-menu]" },
  ...PROJECT_PREVIEW_STATES.map((value) => ({ name: `project-preview-${value}`, params: { "project-preview": value }, ready: value.startsWith("menu-") ? "[data-project-menu-popover]" : "[data-project-preview-dialog]" })),

  { name: "menu-settings", params: { menu: "settings" }, ready: "[data-more-popover]" },
  { name: "menu-theme", params: { menu: "theme" }, ready: "[data-more-popover] [data-theme-list]" },
  { name: "menu-project", params: { menu: "project" }, ready: "[data-project-menu-popover]" },
  { name: "menu-session", params: { menu: "session" }, ready: "[data-session-menu-popover]" },
  { name: "menu-projects", params: { menu: "projects" }, ready: "[data-projects-menu-popover]" },

  { name: "dialog-new-session", params: { dialog: "new-session" }, ready: "[data-new-session-modal]" },
  { name: "dialog-open-project", params: { dialog: "open-project" }, ready: "[data-project-form]" },
  { name: "dialog-choose-directory", params: { dialog: "choose-directory" }, ready: "[data-directory-dialog]" },
  // The gate captures the first visit; the manager behind it is not
  // captured.
  { name: "dialog-git-identity", params: { dialog: "git-identity", identities: "none" }, ready: "[data-git-identity-onboarding]" },
  { name: "dialog-choose-git-identity", params: { dialog: "choose-git-identity" }, ready: "[data-project-git-identity]" },
  { name: "dialog-add-prd", params: { panel: "tasks", dialog: "add-prd" }, ready: "[data-tm-add-prd-modal]" },
  { name: "dialog-custom-theme", params: { dialog: "custom-theme" }, ready: "[data-theme-dialog]" },
  { name: "dialog-remove-project", params: { dialog: "remove-project" }, ready: "[data-remove-project-confirm]" },
  { name: "dialog-close-session", params: { dialog: "close-session" }, ready: "[data-remove-confirm]" },

  { name: "hover-project", params: { hover: "project" }, ready: '[data-preview-hover="project"]' },
  { name: "hover-session", params: { hover: "session" }, ready: '[data-preview-hover="session"]' },
  { name: "hover-row-controls", params: { hover: "row-controls" }, ready: '[data-preview-hover="row-controls"]' },

  { name: "rename-project", params: { rename: "project" }, ready: "[data-rename-project-input]" },
  { name: "rename-session", params: { rename: "session" }, ready: "[data-rename-input]" },

  { name: "panel-files", params: { panel: "files" }, ready: '[data-region="files"][data-open="true"]' },
  { name: "panel-tasks", params: { panel: "tasks" }, ready: '[data-region="tasks"][data-open="true"]' },
  { name: "assets-loading", params: { panel: "assets", assets: "loading" }, ready: '[data-region="assets"][data-open="true"] [data-assets-loading]' },
  { name: "assets-empty", params: { panel: "assets", assets: "empty" }, ready: '[data-region="assets"][data-open="true"] [data-assets-empty]' },
  { name: "assets-list", params: { panel: "assets", assets: "list" }, ready: '[data-region="assets"][data-open="true"] [data-assets-list]' },
  { name: "assets-selected", params: { panel: "assets", assets: "selected" }, ready: '[data-region="assets"] [data-asset-select][aria-checked="true"]' },
  { name: "assets-truncated", params: { panel: "assets", assets: "truncated" }, ready: '[data-region="assets"] [data-assets-truncated]' },
  { name: "assets-after", params: { panel: "assets", assets: "after" }, ready: '[data-region="assets"] [data-asset-fact]' },
  { name: "delete-confirm", params: { panel: "assets", assets: "selected", dialog: "delete-assets" }, ready: '[data-assets-dialog="confirm"]' },
  { name: "delete-outcome", params: { panel: "assets", assets: "after", dialog: "delete-outcome" }, ready: '[data-assets-dialog="outcome"]' },
  { name: "menu-panels", params: { menu: "panels" }, ready: '[data-panels-menu] [data-panel-choice="assets"]', viewport: { width: 760, height: 720 } },
  { name: "assets-unseen", params: { "assets-unseen": "3" }, ready: '[data-titlebar-assets][data-assets-new="3"]' },
  { name: "menu-panels-assets-unseen", params: { menu: "panels", "assets-unseen": "3" }, ready: '[data-panels-menu] [data-panel-choice="assets"][data-assets-new="3"]', viewport: { width: 760, height: 720 } },
  { name: "viewer-code", params: { viewer: "code" }, ready: '[data-files-viewer-scroll][data-files-path="internal/provider/provider.go"]' },
  // The readiness pins the requested task's own dialog, so a page that opened
  // the panel but failed the click cannot certify the list as the detail.
  { name: "task-detail", params: { panel: "tasks", task: "30.2" }, ready: '[role="dialog"][aria-label="Task 30.2"]' },
  { name: "panel-none", params: { panel: "none" }, ready: '[data-region="tasks"][data-open="false"]' },

  ...FILES_DEPTH_PATHS.map((path, depth) => ({
    name: `files-depth-${depth}`,
    params: { panel: "files", "files-depth": String(depth) },
    ready: `[data-files-list-scroll][data-files-dir="${path}"]`,
  })),

  { name: "viewer-text", params: { panel: "files", viewer: "text" }, ready: "[data-files-text]" },
  { name: "viewer-binary", params: { panel: "files", viewer: "binary" }, ready: '[data-files-download][data-focus-key="download-binary"]' },
  { name: "viewer-empty", params: { panel: "files", viewer: "empty" }, ready: "[data-files-empty]" },
  // Image viewer states. Each readiness pins the decoded or refused state
  // itself, so a page still loading, or one whose image fell into another
  // state, cannot certify the requested one.
  { name: "viewer-image", params: { panel: "files", viewer: "image" }, ready: '[data-files-image-canvas="fit"] [data-files-image-frame][data-files-alpha="false"] img' },
  { name: "viewer-image-actual", params: { panel: "files", viewer: "image-actual" }, ready: '[data-files-image-canvas="actual"] [data-files-image-frame] img' },
  { name: "viewer-image-alpha", params: { panel: "files", viewer: "image-alpha" }, ready: '[data-files-image-canvas="fit"] [data-files-image-frame][data-files-alpha="true"] img' },
  { name: "viewer-image-oversized", params: { panel: "files", viewer: "image-oversized" }, ready: '[data-files-image-message="oversized"]' },
  { name: "viewer-image-undecodable", params: { panel: "files", viewer: "image-undecodable" }, ready: '[data-files-image-message="undecodable"]' },
  { name: "viewer-image-gone", params: { panel: "files", viewer: "image-gone" }, ready: '[data-files-image-message="gone"]' },
  { name: "viewer-image-expanded", params: { panel: "files", viewer: "image", expanded: "1" }, ready: '[data-region="files"][data-expanded="true"] [data-files-image-canvas="fit"] [data-files-image-frame] img' },
  { name: "viewer-image-actual-expanded", params: { panel: "files", viewer: "image-actual", expanded: "1" }, ready: '[data-region="files"][data-expanded="true"] [data-files-image-canvas="actual"] [data-files-image-frame] img' },
  { name: "viewer-image-oversized-expanded", params: { panel: "files", viewer: "image-oversized", expanded: "1" }, ready: '[data-region="files"][data-expanded="true"] [data-files-image-message="oversized"]' },
  { name: "files-conflict", params: { panel: "files", "files-conflict": "1" }, ready: "[data-files-conflict]" },

  { name: "tasks-onboarding", params: { tasks: "onboarding" }, ready: '[data-tm-add-prd="onboarding"]' },

  { name: "recovery-restarting", params: { recovery: "restarting" }, ready: '[data-region="terminal"][data-recovery-state="restarting"]' },

  { name: "recovery-replayed", params: { recovery: "replayed" }, ready: '[data-region="terminal"][data-recovery-state="replayed"]' },
  { name: "recovery-failed", params: { recovery: "failed" }, ready: '[data-region="terminal"][data-recovery-state="failed"]' },
  { name: "empty-no-session", params: { empty: "no-session" }, ready: '[data-empty-pane="no-session"]' },
  { name: "empty-no-project", params: { empty: "no-project" }, ready: '[data-empty-pane="no-project"]' },
  { name: "empty-loading", params: { empty: "loading" }, ready: '[data-empty-pane="loading"]' },
  { name: "empty-error", params: { empty: "error" }, ready: '[data-empty-pane="error"]' },

  { name: "sidebar-open", params: { sidebar: "open" }, ready: '[data-workspace][data-collapsed="false"]' },
];

// describeState maps a parsed param set back to its enumerated capture state
// (name + ready selector), so preview.ts can verify it reached the state it
// was asked for and mark the page accordingly. The baseline is the fallback.
export function describeState(params: PreviewParams): { name: string; ready: string } {
  if (params.projectPreview !== null) return { name: `project-preview-${params.projectPreview}`, ready: params.projectPreview.startsWith("menu-") ? "[data-project-menu-popover]" : "[data-project-preview-dialog]" };
  for (const entry of PREVIEW_STATES) {
    if (entry.name === "baseline") continue;
    if (paramsMatch(params, entry.params)) {
      return { name: entry.name, ready: entry.ready };
    }
  }
  return { name: "baseline", ready: "[data-projects-menu]" };
}

function paramsMatch(params: PreviewParams, expected: Record<string, string>): boolean {
  // The parsed state must set exactly the axes the entry names — no more,
  // no fewer — so a more specific state (files-depth-0) is not shadowed by
  // a less specific one (panel-files).
  const actual = paramRecord(params);
  const keys = new Set([...Object.keys(actual), ...Object.keys(expected)]);
  for (const key of keys) {
    if (actual[key] !== expected[key]) return false;
  }
  return true;
}

function paramRecord(params: PreviewParams): Record<string, string> {
  const out: Record<string, string> = {};
  if (params.projectPreview !== null) out["project-preview"] = params.projectPreview;
  if (params.security !== null) out.security = params.security;
  if (params.onboarding !== null) out.onboarding = params.onboarding;
  if (params.menu !== null) out.menu = params.menu;
  if (params.dialog !== null) out.dialog = params.dialog;
  if (params.hover !== null) out.hover = params.hover;
  if (params.rename !== null) out.rename = params.rename;
  if (params.panel !== null) out.panel = params.panel;
  if (params.assets !== null) out.assets = params.assets;
  if (params.assetsUnseen !== null) out["assets-unseen"] = String(params.assetsUnseen);
  if (params.empty !== null) out.empty = params.empty;
  if (params.filesDepth !== null) out["files-depth"] = String(params.filesDepth);
  if (params.filesSelect !== null) out["files-select"] = params.filesSelect;
  if (params.filesConflict) out["files-conflict"] = "1";
  if (params.filesUpload) out["files-upload"] = params.filesUpload;
  if (params.task !== null) out.task = params.task;
  if (params.viewer !== null) out.viewer = params.viewer;
  if (params.filesExpanded) out.expanded = "1";
  if (params.tasks === "onboarding") out.tasks = "onboarding";
  if (params.recovery !== "live") out.recovery = params.recovery;
  if (params.sidebar !== null) out.sidebar = params.sidebar;
  if (params.identities === "none") out.identities = "none";
  return out;
}
