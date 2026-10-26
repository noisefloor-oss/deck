import type { CloneJob, GitIdentityProfile, Inspection } from "../../../api/projects.ts";
import { escapeHtml } from "../sidebar.ts";
import { DIALOG_BODY, DIALOG_FOOT, GHOST_MD_BUTTON, PRIMARY_MD_BUTTON, SECONDARY_MD_BUTTON, SECONDARY_SM_BUTTON, SELECT_FIELD, dialogHead, selectShellHtml } from "../../../ui/controls.ts";
import { lucideIcon } from "../../../ui/icons.ts";
import * as hints from "../../../lib/hints.ts";

export interface SidebarProjectRef {
  id: string;
  name: string;
  path: string;
}

export interface AddProjectState {
  source?: "local" | "url" | "github";
  githubAccount?: string;
  input: string;
  inspection: Inspection | null;
  // inspectError is the server's refusal of the input itself — outside the
  // roots, a file where a directory was expected — stated in the field.
  inspectError: string;
  projects: readonly SidebarProjectRef[];
  profiles: readonly GitIdentityProfile[];
  selectedProfileID: string;
  busy: boolean;
  // inspecting is true while the server has not yet answered the current
  // input; the form says so, and nothing that depends on the answer is
  // offered until it lands.
  inspecting: boolean;
  // serverError is a write's refusal, shown where the field's errors are.
  serverError: string;
  job: CloneJob | null;
  // projectsRoot is the server's projects folder, or "" until it is known;
  // the folder example is a path under it, so it is real on this machine.
  projectsRoot: string;
}

export type AddProjectKind = "empty" | "dup" | "new" | "plain" | "repo" | "worktree" | "url";

// AddProjectView is everything the markup states, derived once from the
// state so no two readouts can disagree about the same directory.
export interface AddProjectView {
  kind: AddProjectKind;
  path: string;
  nested: boolean;
  parentName: string;
  parentPath: string;
  parentKnown: SidebarProjectRef | null;
  dup: SidebarProjectRef | null;
  sameClone: boolean;
  collide: boolean;
  failed: boolean;
  editing: boolean;
  running: boolean;
  stopped: boolean;
  done: boolean;
  blocked: boolean;
  signable: boolean;
  storeOwn: boolean;
  storeParent: boolean;
  storePath: string;
  entries: string[];
  primaryLabel: string;
  primaryDisabled: boolean;
  parentAction: string;
  title: string;
  desc: string;
}

export function addProjectView(state: AddProjectState): AddProjectView {
  const raw = state.input.trim();
  const ins = state.inspection;
  const job = state.job;
  const running = job?.status === "running";
  // A network that died mid-fetch leaves the partial clone held, which is
  // the stopped state's fact, not a refusal's.
  const stopped = job?.status === "stopped" || (job?.status === "failed" && job.failure === "network");
  const done = job?.status === "done";
  const failed = job?.status === "failed" && !stopped;
  const editing = !running && !stopped && !done;

  let kind: AddProjectKind = "empty";
  if (raw !== "" && ins !== null) {
    switch (ins.kind) {
      case "absent": kind = "new"; break;
      case "plain": kind = "plain"; break;
      case "repository": kind = "repo"; break;
      case "worktree": kind = "worktree"; break;
      case "clone": kind = "url"; break;
    }
  }
  const path = ins?.path ?? "";
  // A path already in the sidebar is reachable only by typing it — which is
  // when an error is the right answer. Match on path, never name: a
  // project's name is renameable and often is not its directory.
  const dup = kind !== "empty" && kind !== "url"
    ? state.projects.find((p) => p.path === path) ?? null
    : null;
  if (dup !== null) kind = "dup";
  const parent = ins?.parent ?? null;
  const nested = parent !== null && kind !== "worktree" && kind !== "dup";
  const parentKnown = parent === null ? null : state.projects.find((p) => p.path === parent.path) ?? null;
  const sameClone = kind === "url" && ins?.same_clone === true;
  const collide = kind === "url" && !sameClone && (ins?.exists === true || (failed && job?.failure === "dest_not_empty"));
  const blocked = kind === "empty" || kind === "dup" || collide || failed || state.inspectError !== "" || state.inspecting;
  const hasParent = parent !== null;
  const storePath = ins?.store_path ?? "";
  // A job knows its own name; before one exists the name is the path's.
  const name = job?.name ?? (path.split("/").filter(Boolean).at(-1) ?? "");

  const parentAction = nested && kind === "url"
    ? "Choose another directory"
    : parentKnown !== null ? `Open ${parent?.name ?? ""}` : `Add ${parent?.name ?? ""} instead`;

  // Two label changes, both earned: "Add anyway" makes a committed choice
  // read as one, and "Clone and add" because everything else here is
  // instant — a button saying "Add project" that then shows a progress bar
  // has lied about its cost. Nothing is fetched when the clone is already
  // on disk, so that button must not say Clone.
  const primaryLabel = sameClone ? "Add existing clone"
    : nested && kind === "url" ? "Clone anyway"
      : hasParent && kind !== "dup" ? "Add anyway"
        : kind === "url" ? "Clone and add"
          : kind === "plain" ? "Add as project" : "Add project";

  return {
    kind, path, nested,
    parentName: parent?.name ?? "",
    parentPath: parent?.path ?? "",
    parentKnown, dup, sameClone, collide, failed, editing, running, stopped, done, blocked,
    // Identity applies where a repository will exist and be signed by this
    // project: not a plain directory (nothing to sign until it becomes one),
    // and not a path inside an existing repository, whose identity the
    // parent already governs.
    signable: editing && !blocked && !nested && (kind === "new" || kind === "repo" || kind === "worktree" || kind === "url"),
    // No store readout unless a project will exist here. Nothing owns a
    // store until it is a repository, and stating one under a .git that
    // does not exist — or naming another repository's store as this
    // project's — is the defect this readout exists to prevent.
    storeOwn: editing && !blocked && path !== "" && !hasParent && kind !== "plain" && storePath !== "",
    storeParent: editing && hasParent && kind !== "dup",
    storePath,
    entries: ins?.entries ?? [],
    primaryLabel,
    // Nothing is actionable until the server has answered the text as it
    // is now; an inspection in flight means the readout is not yet about
    // this input.
    primaryDisabled: blocked || state.busy || state.inspecting || state.inspection === null,
    parentAction,
    title: done ? `${name} imported` : "Add project",
    desc: running ? `Cloning into ${name}.`
      : stopped ? "The clone stopped part-way. Nothing was deleted."
        : done ? "Code and history are in. There is no task state yet." : "",
  };
}

const CARD = "flex gap-2.5 rounded-card border border-border bg-surface px-[13px] py-[11px]";
const CARD_COL = "flex min-w-0 flex-1 flex-col gap-[3px]";
const LABEL = "text-body-sm font-medium text-text-secondary";
const URL_EXAMPLE = "https://github.com/owner/repo";
const MONO = "data-mono";

function card(icon: string, iconClass: string, body: string, extra = ""): string {
  return `<div class="${CARD} ${extra}">
    <span aria-hidden="true" class="mt-px flex-none ${iconClass}">${lucideIcon(icon as Parameters<typeof lucideIcon>[0], 15)}</span>
    <div class="${CARD_COL}">${body}</div>
  </div>`;
}

export function addProjectPlaceholder(state: Pick<AddProjectState, "source" | "projectsRoot">): string {
  const folderExample = state.projectsRoot === "" ? "" : `${state.projectsRoot.replace(/\/+$/, "")}/my-project`;
  return state.source === "url" ? URL_EXAMPLE
    : state.source === "local" || folderExample === "" ? folderExample : `${folderExample} or ${URL_EXAMPLE}`;
}

export function addProjectModalHtml(state: AddProjectState): string {
  const v = addProjectView(state);
  const ins = state.inspection;
  const job = state.job;
  const invalid = v.failed || v.kind === "dup" || state.inspectError !== "";
  const browsable = state.source !== "url";
  const fieldClass = `font-input h-9 w-full rounded-control border bg-surface-card pl-(--field-px) ${browsable ? "pr-10" : "pr-(--field-px)"} text-[length:var(--type-input-size)] text-text-primary placeholder:text-text-tertiary sm:text-body-sm ${invalid ? "border-error" : "border-border-strong"}`;

  const fieldErrors = [
    v.failed && job?.failure === "auth" ? `<span class="text-caption text-error-on-tint">The server refused this machine's key.</span>` : "",
    v.failed && job?.failure === "bad_or_private" ? `<span class="text-caption text-error-on-tint">No repository there, or it is private.</span>
      <span class="text-caption text-text-tertiary">Use the SSH URL for a private repository.</span>` : "",
    state.inspectError !== "" ? `<span class="text-caption text-error-on-tint">${escapeHtml(state.inspectError)}</span>` : "",
    state.serverError !== "" ? `<span class="text-caption text-error-on-tint">${escapeHtml(state.serverError)}</span>` : "",
    v.dup !== null ? `<span class="text-caption text-error-on-tint">Already in the sidebar as ${escapeHtml(v.dup.name)}.</span>
      <span class="mt-0.5 inline-flex self-start"><button type="button" data-project-show-dup data-hint="${hints.SHOW_PROJECT_ALREADY_IN_SIDEBAR}" class="${SECONDARY_SM_BUTTON} border-transparent bg-transparent text-text-secondary hover:text-text-primary">Show it</button></span>` : "",
    v.kind === "plain" && !v.nested ? `<span class="text-caption text-error-on-tint">No repository here — and none in any directory above it.</span>` : "",
  ].join("");

  const fieldLabel = state.source === "local" ? "Folder" : state.source === "url" ? "Repository URL" : "Path or repository";
  const placeholder = addProjectPlaceholder(state);
  const fieldHint = state.source === "local" ? hints.TYPE_FOLDER_PATH : state.source === "url" ? hints.TYPE_REPOSITORY_URL : hints.TYPE_PATH_OR_REPOSITORY;
  const field = state.source === "github" ? card("download", "text-text-secondary", `<span class="${LABEL}">GitHub repository</span><span data-mono="caption" class="break-anywhere text-text-primary">${escapeHtml(hostForm(state.input))}</span><span class="text-caption text-text-secondary">GitHub access: ${escapeHtml(state.githubAccount ?? "")}.${state.profiles.length > 1 ? " Commit author is chosen below." : ""}</span>`) : `
    <div class="flex flex-col gap-1.5">
      <label class="${LABEL}" for="add-project-input">${escapeHtml(fieldLabel)}</label>
      <div class="relative flex w-full items-center">
        <input id="add-project-input" data-project-path type="text" value="${escapeHtml(state.input)}" ${v.editing ? "" : "readonly"}
          placeholder="${escapeHtml(placeholder)}" aria-label="${escapeHtml(fieldLabel)}" data-hint="${fieldHint}"
          autocapitalize="none" autocomplete="off" spellcheck="false" ${MONO}="control" class="${fieldClass}">
        ${browsable ? `<button type="button" data-project-browse aria-label="Browse" data-hint="${hints.CHOOSE_WORKSPACE_FOLDER}"
          class="dialog-iconbtn absolute right-[5px] top-1/2 -translate-y-1/2">${lucideIcon("folder", 16)}</button>` : ""}
      </div>
      ${fieldErrors}
    </div>`;

  // Board 2d. An existing directory that is not a repository is not the
  // same answer as a path that does not exist, and must not inherit "New
  // directory · git init". No store readout: nothing owns a store until it
  // is a repository.
  const plain = v.editing && v.kind === "plain" && !v.nested ? card("folder", "text-text-tertiary", `
      <span class="${LABEL}">What is there</span>
      <span ${MONO}="caption" class="break-anywhere text-text-secondary">${escapeHtml(plainSummary(v.entries))}</span>
      <span class="text-caption text-text-secondary"><span class="font-medium text-text-primary">Git identity does not apply</span> until it becomes a repository.</span>`) : "";

  // New directory: what typing a path that does not exist means. git init
  // is stated because it is the difference between a directory and a
  // project, and it gives the store a home.
  const fresh = v.editing && v.kind === "new" && !v.nested ? card("plus", "text-text-tertiary", `
      <span class="${LABEL}">New directory</span>
      <span ${MONO}="caption" class="break-anywhere leading-[1.55] text-text-primary">${escapeHtml(v.path)}<br>git init · no history</span>`) : "";

  // Recognition. Every value is one git -C away; the commit age is what
  // tells two checkouts of one repository apart.
  const recognised = v.editing && (v.kind === "repo" || v.kind === "worktree") ? card("circle-check", "text-success", `
      <span class="${LABEL}">Repository</span>
      <span ${MONO}="caption" class="break-anywhere leading-[1.55] text-text-primary">origin ${escapeHtml(ins?.remote ?? "")}<br>${escapeHtml(ins?.head ?? "")}</span>
      <span class="text-caption text-text-tertiary">${v.kind === "worktree" ? `A linked worktree of ${escapeHtml(v.parentPath)}.` : "Its own repository root."}</span>`) : "";

  // A URL. The destination is a consequence of the URL, so it lives inside
  // the readout rather than as a peer field — but stays editable, since the
  // container is the one thing a clone genuinely needs from you.
  const url = v.editing && v.kind === "url" ? card("download", "text-text-tertiary", `
      <span class="${LABEL}">Clone into</span>
      <div class="relative mt-0.5 flex w-full items-center">
        <input data-project-clone-dest type="text" value="${escapeHtml(v.path)}" aria-label="Clone into" data-hint="${hints.EDIT_CLONE_DESTINATION}"
          autocapitalize="none" autocomplete="off" spellcheck="false" ${MONO}="caption"
          class="font-input h-7 w-full rounded-control-sm border bg-surface-card pl-2 pr-8 text-text-primary ${v.collide ? "border-error" : "border-border-strong"}">
        <button type="button" data-project-browse-dest aria-label="Browse" data-hint="${hints.CHOOSE_CLONE_CONTAINER}"
          class="dialog-iconbtn absolute right-[2px] top-1/2 h-6 w-6 -translate-y-1/2">${lucideIcon("folder", 14)}</button>
      </div>
      ${v.collide ? `<span class="text-caption text-error-on-tint">Not empty — ${v.entries.length} items already here.</span>` : ""}
      ${v.sameClone ? `<span class="text-caption text-text-tertiary">Already cloned here from <span ${MONO}="inherit">${escapeHtml(ins?.remote ?? "")}</span>. Nothing will be fetched.</span>` : ""}
      ${!v.collide && !v.sameClone ? `<span class="text-caption text-text-tertiary">From the repository name.</span>` : ""}`) : "";

  const gitSaid = (v.failed || v.stopped) && (job?.stderr ?? "") !== "" ? `
    <div class="flex flex-col gap-[5px] rounded-card border border-border bg-surface px-3 py-2.5">
      <span class="${LABEL} text-text-tertiary">Git said</span>
      <span ${MONO}="caption" class="break-anywhere whitespace-pre-line leading-[1.55] text-text-secondary">${escapeHtml(job?.stderr ?? "")}</span>
    </div>` : "";

  const auth = v.failed && job?.failure === "auth" ? card("lock", "text-text-tertiary", `
      <span class="text-body-sm leading-[1.5] text-text-secondary">Add a key GitHub accepts to this machine's agent.</span>
      <span ${MONO}="caption" class="text-text-tertiary">ssh -T git@github.com</span>`) : "";

  const inThisDirectory = v.collide && v.entries.length > 0 ? `
    <div class="flex flex-col gap-1.5 rounded-card border border-border bg-surface px-3 py-2.5">
      <span class="${LABEL} text-text-tertiary">In this directory</span>
      <div class="flex flex-wrap gap-x-3.5 gap-y-1">
        ${v.entries.slice(0, 4).map((e) => `<span ${MONO}="caption" class="text-text-secondary">${escapeHtml(e === ".git" ? e : `${e}/`)}</span>`).join("")}
        ${v.entries.length > 4 ? `<span class="text-caption text-text-tertiary">and ${v.entries.length - 4} more</span>` : ""}
      </div>
    </div>` : "";

  const progress = (v.running || v.stopped) && job !== null ? `
    <div class="flex flex-col gap-[9px]">
      <div class="flex items-baseline gap-3">
        <span ${MONO}="body-sm" class="min-w-0 flex-1 truncate text-text-primary">${escapeHtml(hostForm(job.url))}</span>
        <span data-clone-percent class="flex-none text-body-sm font-medium tabular-nums ${v.stopped ? "text-text-secondary" : "text-text-primary"}">${job.line ? `${job.percent}% of this phase` : "Starting…"}</span>
      </div>
      <span role="progressbar" aria-label="Git phase progress" aria-valuemin="0" aria-valuemax="100" ${job.line ? `aria-valuenow="${job.percent}"` : ""} class="block h-1 overflow-hidden rounded-pill bg-wash-hover"><span class="block h-full rounded-pill ${v.stopped ? "bg-warning" : "bg-accent"}" style="width:${job.percent}%"></span></span>
      <span ${MONO}="caption" class="text-text-tertiary">${escapeHtml(progressLine(job, v.stopped))}</span>
    </div>
    ${v.running ? `<div class="flex flex-col gap-[3px] pt-1">
      <span class="${LABEL}">Workspace path</span>
      <span ${MONO}="caption" class="break-anywhere text-text-secondary">${escapeHtml(job.destination)}</span>
    </div>` : ""}
    <div class="flex flex-col gap-[3px]">
      <span class="${LABEL}">Task store</span>
      <span ${MONO}="caption" class="break-anywhere ${v.storeParent ? "text-warning" : "text-text-secondary"}">${escapeHtml(v.storePath || `${job.destination}/.git/noise-tasks/tasks.sqlite`)}</span>
    </div>` : "";

  // A half-cloned directory is a state the app holds, not a mess it hides.
  const partial = v.stopped && job !== null ? card("folder", "text-warning", `
      <span class="${LABEL}">Partial clone on disk</span>
      <span ${MONO}="caption" class="break-anywhere text-text-primary">${escapeHtml(job.destination)} · ${formatBytes(job.partial_bytes)} · no working tree</span>
      <span class="text-caption text-text-secondary">Resuming starts the fetch again.</span>`, "bg-warning-tint") : "";

  // Adopting is not populating: the project has code and history and no
  // task state. A PRD is not required and must not be asked for.
  const finished = v.done && job !== null ? `
    <div class="flex flex-col gap-[3px]">
      <span class="${LABEL}">Workspace path</span>
      <span ${MONO}="caption" class="break-anywhere text-text-secondary">${escapeHtml(job.destination)}</span>
    </div>
    <div class="flex flex-col gap-1.5">
      <span class="${LABEL}">Task board</span>
      <div class="flex gap-[11px] rounded-card border border-border bg-surface-card p-[13px]">
        <span aria-hidden="true" class="mt-px flex-none text-text-tertiary">${lucideIcon("layers", 17)}</span>
        <div class="flex min-w-0 flex-col gap-1">
          <span class="text-body font-medium text-text-primary">Start an empty board</span>
          <span class="text-caption text-text-tertiary">Three lanes, no tasks.</span>
          <span ${MONO}="caption" class="break-anywhere text-text-tertiary">${escapeHtml(`${job.destination}/.git/noise-tasks/tasks.sqlite`)}</span>
        </div>
      </div>
    </div>` : "";

  const storeOwn = v.storeOwn ? card("layers", "text-text-tertiary", `
      <span class="${LABEL}">Task store</span>
      <span ${MONO}="caption" class="break-anywhere text-text-primary">${escapeHtml(v.storePath)}</span>`) : "";

  // Two warnings, one shape. Both sentences are approved copy and neither
  // may be tightened: the opening clause is the structural rule that
  // justifies there being no per-worktree store, and the closing clause
  // names the failure we have already shipped.
  const warning = v.kind === "worktree"
    ? `A worktree's store lives inside the parent repository's .git, so working in this directory cannot change where a write lands. Tasks here go to <span class="font-medium text-text-primary">${escapeHtml(v.parentName)}</span>'s board — including tasks an agent writes while working in this directory.`
    : v.kind === "url"
      ? `The store sits inside <span class="font-medium text-text-primary">${escapeHtml(v.parentName)}</span>'s own .git, so working in this directory cannot change where a write lands. Tasks here go to that project's board, not this one — including tasks an agent writes while working in this directory.`
      : `This directory is inside <span class="font-medium text-text-primary">${escapeHtml(v.parentName)}</span>, so working in this directory cannot change where a write lands. Tasks here go to that project's board, not this one — including tasks an agent writes while working in this directory.`;
  const storeParent = v.storeParent ? `
    <div class="flex flex-col gap-2.5 rounded-card border border-border bg-warning-tint px-[13px] py-[11px]">
      <div class="flex gap-2.5">
        <span aria-hidden="true" class="mt-px flex-none text-warning">${lucideIcon("triangle-alert", 15)}</span>
        <div class="${CARD_COL}">
          <span class="${LABEL}">Task store</span>
          <span ${MONO}="caption" class="break-anywhere leading-[1.55] text-text-primary">boundary ${escapeHtml(v.path)}<br>store ${escapeHtml(v.storePath)}</span>
          <span class="text-caption text-text-secondary">${warning}</span>
        </div>
      </div>
      <span class="ml-[25px] inline-flex self-start"><button type="button" data-project-take-parent data-hint="${hints.TAKE_PARENT_REPOSITORY}" class="${SECONDARY_SM_BUTTON}">${escapeHtml(v.parentAction)}</button></span>
    </div>` : "";

  // Identity is last and appears only once there is something to sign and
  // more than one identity to choose from.
  const identity = v.signable && state.profiles.length > 1 ? `
    <label class="block">
      <span class="${LABEL}">Git identity</span>
      ${selectShellHtml(`<select data-project-git-profile aria-label="Git identity" data-hint="${hints.CHOOSE_GIT_IDENTITY_FOR_PROJECT}"
        class="${SELECT_FIELD} font-input h-9 w-full rounded-control border border-border-strong bg-surface-card pl-2 text-body-sm text-text-primary">
        ${state.profiles.map((profile) => `<option value="${escapeHtml(profile.id)}" ${profile.id === state.selectedProfileID ? "selected" : ""}>${escapeHtml(profile.name)} — ${escapeHtml(profile.email)}</option>`).join("")}
      </select>`, "mt-1")}
      <span class="mt-1.5 block text-caption text-text-tertiary">Applies to this project only.</span>
    </label>` : "";

  const foot = DIALOG_FOOT(`
    ${v.stopped ? `<span class="mr-auto inline-flex"><button type="button" data-clone-discard ${state.busy ? "disabled" : ""} data-hint="${hints.DISCARD_PARTIAL_CLONE}" class="${GHOST_MD_BUTTON}"><span aria-hidden="true" class="flex-none">${lucideIcon("trash-2", 16)}</span>Discard</button></span>` : ""}
    ${v.running ? `<span class="mr-auto inline-flex"><button type="button" data-clone-cancel ${state.busy ? "disabled" : ""} data-hint="${hints.CANCEL_CLONE}" class="${GHOST_MD_BUTTON}">Cancel import</button></span>` : ""}
    ${v.running ? "" : `<button type="button" data-project-cancel data-hint="${hints.CANCEL_OPENING_PROJECT}" class="${SECONDARY_MD_BUTTON}">${v.stopped ? "Close" : "Cancel"}</button>`}
    ${v.editing ? `<button type="submit" data-project-submit ${v.primaryDisabled ? "disabled" : ""} data-hint="${hints.OPEN_THIS_PROJECT}" class="${PRIMARY_MD_BUTTON}">${v.primaryLabel === "Add project" ? `<span data-copy-wide>Add project</span><span data-copy-phone class="hidden">Open project</span>` : escapeHtml(v.primaryLabel)}</button>` : ""}
    ${v.running ? `<button type="button" data-clone-background data-hint="${hints.RUN_CLONE_IN_BACKGROUND}" class="${PRIMARY_MD_BUTTON}">Run in background</button>` : ""}
    ${v.stopped ? `<button type="button" data-clone-resume ${state.busy ? "disabled" : ""} data-hint="${hints.RESUME_CLONE}" class="${PRIMARY_MD_BUTTON} flex items-center gap-2"><span aria-hidden="true" class="flex-none">${lucideIcon("refresh-cw", 16)}</span>Resume</button>` : ""}
    ${v.done ? `<button type="button" data-clone-create-board ${state.busy ? "disabled" : ""} data-hint="${hints.CREATE_EMPTY_BOARD}" class="${PRIMARY_MD_BUTTON}">Create board</button>` : ""}
  `);

  return `
      <form data-project-form data-add-project data-phone-fullscreen ${state.inspecting ? "data-inspecting" : ""} tabindex="-1" role="dialog" aria-modal="true" aria-label="Add project" class="dialog outline-none">
        ${dialogHead(
          v.title === "Add project"
            ? `<span data-copy-wide>Add project</span><span data-copy-phone class="hidden">Open project</span>`
            : escapeHtml(v.title),
          { label: "Close", attr: "data-project-cancel" },
          v.desc === "" ? undefined : escapeHtml(v.desc),
          state.source !== undefined && v.editing ? { attr: `data-project-source="${state.source === "github" ? "github" : "choose"}"` } : undefined,
        )}
        <p data-project-caption class="hidden truncate border-b border-border px-5 pb-2.5 pt-0.5 text-caption text-text-tertiary">Open project adds this folder to your sidebar</p>
        ${DIALOG_BODY(`${!v.editing && state.serverError !== "" ? `<p role="alert" class="text-body-sm text-error-on-tint">${escapeHtml(state.serverError)}</p>` : ""}` + (v.editing ? `${field}${plain}${fresh}${recognised}${url}${gitSaid}${auth}${inThisDirectory}${storeOwn}${storeParent}${identity}` : `${progress}${gitSaid}${partial}${finished}`))}
        ${foot}
      </form>`;
}

// plainSummary derives from the same array as the collision list, so one
// directory cannot describe itself two ways.
export function plainSummary(entries: readonly string[]): string {
  const dirs = entries.filter((e) => e !== ".git");
  const count = dirs.length === 0 ? "Empty" : `${dirs.length} ${dirs.length === 1 ? "directory" : "directories"}`;
  return `${count}, no .git`;
}

export function hostForm(url: string): string {
  return url.replace(/^(https?:\/\/|ssh:\/\/|git@)/, "").replace(/\.git$/, "").replace(/\/+$/, "").replace(/^([^/]+):/, "$1/");
}

export function formatBytes(bytes: number): string {
  if (bytes >= 1024 * 1024) return `${(bytes / (1024 * 1024)).toFixed(1)} MB`;
  if (bytes >= 1024) return `${Math.round(bytes / 1024)} KB`;
  return `${bytes} B`;
}

function progressLine(job: CloneJob, stopped: boolean): string {
  if (stopped) return `stopped · ${formatBytes(job.partial_bytes)} on disk`;
  return job.line ?? "starting";
}
