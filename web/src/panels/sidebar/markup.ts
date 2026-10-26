import type { CloneJob } from "../../api/projects.ts";
import { providerDisplayName, providerTileClass } from "../../ui/session-identity.ts";
import { lucideIcon } from "../../ui/icons.ts";
import * as hints from "../../lib/hints.ts";
import { escapeHtml } from "../../lib/escape.ts";

// sessionActionControlsHtml is the session row's whole trailing edge:
// the age tick, which fades out on hover, and the ONE
// ellipsis control that fades in over it — .session-menu-trigger in
// app.css owns that reveal. The button is 28px (--control-h-sm) with a
// colour-only hover: row icon buttons get no wash, so the
// glyph just steps to primary. Absolute placement is what keeps the
// row's height fixed whether the control is hidden or revealed.
//
// The evidence cell is fit-content flush with the row's content edge:
// a min-width or negative margin
// here shifts the tick off the content edge and breaks the pending row's
// alignment. The tick's line box is normal — it has a font size
// but no line-height, so text-micro's 1.3 ratio (a 14.3px line
// box) must not apply. Its colour follows the row's selected state:
// secondary on the selected row, tertiary on every
// other — the same promotion the row's name gets.
//
// The trigger overlays the tick, so the two move together: the tick is
// hidden exactly while the trigger is shown, and visible again the moment
// it hides. Both answer hover and keyboard focus — on the row, which
// carries the list's roving tabindex, or on the trigger inside it. A mouse
// click on the row is neither, and leaves the tick where it is.
//
// The tick is the interface face, not mono. Mono is the machine's voice —
// the terminal's own output — and an elapsed time the app computes and
// prints about itself is the app speaking, however evidence-like it reads.
// The size stays --type-micro-size: the mono register's x-height
// correction exists to shrink mono into a micro line box, and an
// interface face at micro needs no such correction.
export function sessionActionControlsHtml(sessionID: string, metaHtml: string, selected: boolean): string {
  const id = escapeHtml(sessionID);
  return `
    <span data-session-meta="${id}" class="session-evidence flex flex-none items-center text-micro font-medium leading-[normal] ${selected ? "text-text-secondary" : "text-text-tertiary"} transition-opacity duration-[110ms] ease-[ease] group-focus-visible:opacity-0 group-has-focus-visible:opacity-0 group-hover:opacity-0">${metaHtml}</span>
    <span class="session-menu-trigger">
      <button type="button" data-session-menu="${id}" aria-label="Session menu" data-hint="${hints.RENAME_RECOLOR_CLOSE_SESSION}" aria-haspopup="menu" aria-expanded="false"
        class="grid h-7 w-7 place-items-center rounded-control-sm text-text-tertiary transition-colors hover:text-text-primary pointer-coarse:h-10 pointer-coarse:w-10">${ellipsisGlyph()}</button>
    </span>`;
}

// The project row's disclosure column: a 13px mono
// column carrying ▾/▸, tinted with the project color when one is set and
// falling back to the secondary text voice. The tint is inline because it
// is per-project data, not a token decision.
export function projectChevronHtml(
  expanded: boolean,
  color: string | null | undefined,
): string {
  const tint = color === null || color === undefined || color === ""
    ? ""
    : ` style="color:${escapeHtml(color)}"`;
  return `<span aria-hidden="true"${tint} class="w-[13px] flex-none font-mono text-[length:min(var(--type-mono-size),calc(var(--type-chevron-size)*var(--type-mono-xh-ratio)))] leading-none text-text-secondary">${expanded ? "▾" : "▸"}</span>`;
}

// The sessions-list rail: a 2px border-left indenting
// every session under an open project, in the project colour when one is
// set and the border voice when not. The tint is inline because it is
// per-project data, not a token decision; the width and fallback live in
// the .session-rail rule.
export function sessionRailStyle(color: string | null | undefined): string {
  const tint = color === null || color === undefined || color === ""
    ? ""
    : ` style="border-left-color:${escapeHtml(color)}"`;
  return tint;
}

// The leading status cell's content. The
// spinner keeps the streaming label: at the narrow scope this cell is the
// only visible streaming signal, and the trailing spinner is display:none
// there — hidden from readers too — so an unlabeled leading cell would
// leave a phone reader without the state a desktop reader hears.
export function sessionStatusCellHtml(streaming: boolean, attention = false): string {
  if (streaming) {
    return `<span role="img" aria-label="output streaming" class="block h-[11px] w-[11px] animate-row-spinner rounded-circle border-[1.5px] border-border-strong border-t-text-secondary"></span>`;
  }
  // The 7px warning dot: a listed session the
  // server cannot yet verify as running. Content-free by design. The edge
  // patch never erases it: setStreaming returns before this cell for any
  // presentation other than running, so only renders write attention rows.
  if (attention) {
    return `<span role="img" aria-label="needs attention" class="block h-[7px] w-[7px] rounded-full bg-warning"></span>`;
  }
  return "";
}

// The sessions-list rail container: a 2px border-left
// indents every session under an open project. The 11px left margin and
// 8px left padding are hand-tuned; rounding them to the 4px
// ramp shifts every row 2px right and narrows it by the same amount.
// The grouping rail remains visible without a selection; the selected
// session row's own wash and aria-selected state carry selection instead.
// The rail carries no in-group + New session chrome of its own: the
// empty-rail row (emptyRailNewSessionHtml, rendered by the caller inside
// childrenHtml) is the single creation affordance a group without
// sessions shows, on every form factor. A populated group offers none in
// its rail — the project row's own + (desktop) and the sheet's bottom
// bar (phone) are the ways in.
export function sessionRailContainerHtml(
  projectID: string,
  color: string | null | undefined,
  expanded: boolean,
  childrenHtml: string,
): string {
  return `<ul id="sessions-${escapeHtml(projectID)}" draggable="false" class="session-rail mb-1.5 ml-[11px] mt-0.5 flex flex-col gap-px pl-2"${sessionRailStyle(color)} ${expanded ? "" : "hidden"}>${childrenHtml}</ul>`;
}

// PendingSession is a launch you have asked for that the server
// has not answered yet.
export interface PendingSession {
  projectID: string;
  provider: string;
}

// The project row's toggle and name, shared with the import row so an
// import and the project it becomes draw the same row.
const PROJECT_ROW_BUTTON = "flex w-full min-w-0 cursor-pointer items-center gap-(--row-icon-gap) rounded-control py-(--row-py) pl-(--row-px) pr-[64px] text-left text-text-primary transition-colors pointer-coarse:pr-[86px]";
const PROJECT_ROW_NAME = "truncate text-body font-medium leading-[1.3] text-text-primary";

// The project row template, extracted so the rendered shape — permanence
// classes, control sizes, the unconditional plus — is regression-tested.
export function projectRowHtml(args: {
  id: string;
  name: string;
  expanded: boolean;
  hint: string;
  chev: string;
  actions: string;
  selected?: boolean;
}): string {
  // The reference's selected project row holds the same wash its hover
  // uses — sessions take the stronger selected wash — so a selected
  // project reads as position, not emphasis.
  const selectedWash = args.selected === true ? "bg-wash-hover" : "hover:bg-wash-hover";
  return `<div class="group/project relative flex items-center" data-selected="${args.selected === true}">
          <button data-project-toggle="${escapeHtml(args.id)}" data-list-row role="option" tabindex="-1" aria-expanded="${args.expanded}" aria-controls="sessions-${escapeHtml(args.id)}" data-hint="${args.hint}"
            class="${PROJECT_ROW_BUTTON} ${selectedWash} active:bg-wash-pressed focus-visible:bg-wash-hover">
            ${args.chev}
            <span class="flex min-w-0 flex-1 flex-col gap-px">
              <span data-project-name class="${PROJECT_ROW_NAME}">${escapeHtml(args.name)}</span>
            </span>
          </button>
          <span data-project-actions class="absolute right-[3px] top-1/2 flex -translate-y-1/2 items-center gap-px ${args.actions}">
            <button type="button" data-new-session aria-label="New session" data-hint="${hints.projectNewSession(escapeHtml(args.name))}"
              class="grid h-7 w-7 flex-none place-items-center rounded-control-sm pointer-coarse:h-10 pointer-coarse:w-10 text-text-tertiary transition-colors hover:bg-wash-hover hover:text-text-primary">${plusGlyph()}</button>
            <button type="button" data-project-menu="${escapeHtml(args.id)}" aria-label="Project menu" data-hint="${hints.projectMenu(escapeHtml(args.name))}" aria-haspopup="menu" aria-expanded="false"
              class="grid h-7 w-7 flex-none place-items-center rounded-control-sm pointer-coarse:h-10 pointer-coarse:w-10 text-text-tertiary transition-colors hover:bg-wash-hover hover:text-text-primary">${ellipsisGlyph()}</button>
          </span>
        </div>`;
}

// A project with no sessions would otherwise show an empty rail and no way
// in: the + on the project row is invisible until the row is hovered. This
// row fills that gap and then gets out of the way - once the project has a
// session, including a pending one, the sessions themselves are the content
// and the row's + is the way to add another.
//
// This row is not rendered once the project has sessions, because it would be a
// second plus affordance a few pixels
// from the project row's own.
export function emptyRailNewSessionHtml(
  g: { sessions: readonly unknown[]; project: { id: string; name: string } },
  expanded: boolean,
  pending: PendingSession | null,
): string {
  if (!expanded) return "";
  if (g.sessions.length > 0) return "";
  if (pending !== null && pending.projectID === g.project.id) return "";
  return `<li><button type="button" data-new-session data-hint="${hints.projectNewSession(escapeHtml(g.project.name))}"
      class="flex w-full cursor-pointer items-center gap-2 rounded-control px-2 py-1.5 text-left text-body leading-5 text-text-tertiary transition-colors hover:bg-wash-hover hover:text-text-primary focus-visible:bg-wash-hover max-md:min-h-11"><span aria-hidden="true" class="flex w-4 flex-none justify-center font-mono text-[min(var(--type-mono-size),calc(var(--type-body-size)*var(--type-mono-xh-ratio)))]">+</span><span class="min-w-0 flex-1">New session</span></button></li>`;
}

export function cloneJobStaysListed(
  job: CloneJob,
  awaitingProject: Set<string>,
  groups: readonly { project: { id: string } }[],
): boolean {
  if (job.status !== "done") return true;
  if (!awaitingProject.has(job.id)) return false;
  if (job.project_id !== undefined && groups.some((group) => group.project.id === job.project_id)) {
    awaitingProject.delete(job.id);
    return false;
  }
  return true;
}

// An import in flight is a project that exists but cannot be opened yet.
// It sits in the rail from the moment the clone starts, drawn as the
// collapsed project row it will become, with progress where the row's
// actions go. There is nothing to expand or start a session in yet.
// Clicking reopens the dialog.
export function cloneRowHtml(job: CloneJob): string {
  const running = job.status === "running";
  const done = job.status === "done";
  const percent = Math.max(0, Math.min(100, Math.round(job.percent)));
  const hint = running
    ? `Importing ${escapeHtml(job.url.replace(/^https?:\/\//, ""))} — ${percent}%`
    : done ? "Import complete — refreshing projects" : "Import stopped part-way — click to resume";
  const progress = running
    ? `<span class="sr-only">Importing, ${percent}%</span>
          <svg aria-hidden="true" width="16" height="16" viewBox="0 0 16 16" fill="none" stroke-width="2">
            <circle cx="8" cy="8" r="6" class="stroke-wash-pressed"/>
            <circle data-clone-ring cx="8" cy="8" r="6" pathLength="100" stroke-dasharray="${percent} 100" transform="rotate(-90 8 8)" class="stroke-accent"/>
          </svg>`
    : done
      ? `<span class="sr-only">Import complete</span><span aria-hidden="true" class="inline-flex text-accent">${lucideIcon("check", 14)}</span>`
      : `<span class="sr-only">Import stopped part-way</span><span aria-hidden="true" class="inline-flex text-warning">${lucideIcon("triangle-alert", 14)}</span>`;
  return `<li class="rounded-control"><div class="relative flex items-center">
      <button type="button" data-clone-row="${escapeHtml(job.id)}" data-list-row role="option" tabindex="-1" data-hint="${hint}"
        class="${PROJECT_ROW_BUTTON} hover:bg-wash-hover active:bg-wash-pressed focus-visible:bg-wash-hover">
        ${projectChevronHtml(false, null)}
        <span class="flex min-w-0 flex-1 flex-col gap-px">
          <span class="${PROJECT_ROW_NAME}">${escapeHtml(job.name)}</span>
        </span>
        <span data-clone-progress class="absolute right-[3px] top-1/2 grid h-7 w-7 -translate-y-1/2 place-items-center pointer-coarse:h-10 pointer-coarse:w-10">
          ${progress}
        </span>
      </button>
    </div></li>`;
}

// The no-projects rail's single row: a control-icon
// plus and a sentence-case label, in the interface voice — mono is the
// machine voice and never marks a creation affordance. It opens the
// same New project dialog the Projects menu's row does.
export function newProjectRowHtml(): string {
  return `<li><button type="button" data-new-project-row data-hint="${hints.ADD_WORKSPACE_DIRECTORY_AS_PROJECT}"
      class="flex w-full cursor-pointer items-center gap-2 rounded-control px-2 py-1.5 text-left text-body leading-5 text-text-tertiary transition-colors hover:bg-wash-hover hover:text-text-primary focus-visible:bg-wash-hover"><span aria-hidden="true" class="flex w-4 flex-none justify-center">${plusGlyph()}</span><span class="min-w-0 flex-1">Add project</span></button></li>`;
}

// pendingSessionRowHtml is the row for a pending launch, or "" when the
// group is not the one being launched into. It repeats the real session
// row's geometry — 16px tile, name, 11px meta cell — with the meta cell
// holding the streaming spinner. It carries no data-session, so the click
// handlers that bind by that attribute cannot reach it, and no
// rename or remove control, which would have nothing to act on.
export function pendingSessionRowHtml(
  pending: PendingSession | null,
  projectID: string,
): string {
  if (pending === null || pending.projectID !== projectID) return "";
  return `<li data-pending-session="${escapeHtml(pending.provider)}"
      class="flex items-center gap-2 rounded-control px-2 py-1.5">
      ${sessionTileHtml(pending.provider)}
      <span class="min-w-0 flex-1 truncate text-body text-text-tertiary">${escapeHtml(providerDisplayName(pending.provider))}</span>
      <span role="img" aria-label="starting"
        class="flex flex-none items-center"><span aria-hidden="true"
        class="block h-[11px] w-[11px] animate-row-spinner rounded-circle border-[1.5px] border-border-strong border-t-text-secondary"></span></span>
    </li>`;
}

// The wordmark for the expanded sidebar header is itself the collapse
// control: the whole lockup is a button so the action
// is keyboard-reachable, and its accessible name says what happens rather
// than naming the brand. The artwork is decorative inside the button.
//
// The mark is flat. It takes no hover wash and no hover tint, because the
// brand is not a control surface — collapsing swaps it for the evidence-
// coloured mark, and that swap is the only state it reports. The
// focus-visible ring stays: it is how a keyboard reaches the control.
export function lockupImg(): string {
  return `<button type="button" data-collapse aria-label="Collapse sidebar" data-hint="${hints.COLLAPSE_PROJECT_SIDEBAR}"
    class="ml-(--space-2) grid h-6 place-items-center rounded-control-sm text-text-tertiary focus-visible:bg-wash-hover pointer-coarse:h-10">
    <img src="/brand/noisefloor-lockup-inverse.svg" alt="" width="99" height="20" class="block h-[20px] w-[99px] flex-none" draggable="false">
  </button>`;
}

// The phone opens the sidebar as a sheet: a
// grabber, "Sessions" at title-3, and a close cell. Desktop hides all of it
// and keeps the lockup chrome; the phone hides the lockup and shows this.
// Both live in the markup so the two widths cannot render different bytes
// from different code paths.
export function sheetChromeHtml(search = ""): string {
  return `<div data-sidebar-sheet-chrome class="hidden">
    <div class="flex justify-center pb-0.5 pt-2">
      <span aria-hidden="true" class="h-1 w-[38px] rounded-pill bg-border-strong"></span>
    </div>
    <div class="flex items-center gap-2 pb-2 pl-5 pr-2 pt-1">
      <span class="flex-1 text-title-3 font-medium text-text-primary">Sessions</span>
      <button type="button" data-sidebar-sheet-menu aria-haspopup="menu" aria-expanded="false" aria-label="Projects menu" data-hint="${hints.OPEN_PROJECTS_MENU}"
        class="grid h-11 w-11 flex-none place-items-center rounded-control text-text-secondary transition-colors hover:text-text-primary">${lucideIcon("ellipsis", 18)}</button>
      <button type="button" data-sidebar-sheet-close aria-label="Close" data-hint="${hints.CLOSE_SESSION_LIST}"
        class="grid h-11 w-11 flex-none place-items-center rounded-control text-text-secondary transition-colors hover:text-text-primary">${lucideIcon("x", 18)}</button>
    </div>
    <!-- The field carries no placeholder: the magnifier says it, and
         the aria-label carries it for a reader. -->
    <div class="px-4 pb-3">
      <label class="inputwrap">
        <span aria-hidden="true" class="inputwrap-lead">${lucideIcon("search", 16)}</span>
        <input data-sheet-filter value="${escapeHtml(search)}" aria-label="Filter sessions" autocapitalize="none" autocomplete="off" spellcheck="false"
          class="input input-lead font-input leading-[normal]">
      </label>
    </div>
  </div>`;
}

// The sheet's bottom bar: New session across the
// width, the folder cell beside it for Open project. These are the PROJECTS
// menu's affordances in the shape the phone draws them — the eyebrow leaves
// with the desktop drawer, so the bar is where its actions live at this
// width, not an addition to them.
export function sheetActionsHtml(noProjects = false): string {
  return `<div data-sidebar-sheet-actions class="hidden">
    <div class="flex items-center gap-2.5 border-t border-border px-4 pb-[max(16px,env(safe-area-inset-bottom))] pt-3">
      <button type="button" data-sheet-new-session ${noProjects ? "disabled" : ""} data-hint="${hints.START_NEW_SESSION}"
        class="flex h-12 min-w-0 flex-1 items-center justify-center gap-2 rounded-control bg-accent text-body font-medium text-text-on-accent transition-colors hover:bg-accent-hover disabled:opacity-45">${lucideIcon("plus", 18)}New session</button>
      <button type="button" data-sheet-open-project aria-label="Open project" data-hint="${hints.OPEN_DIRECTORY_AS_PROJECT}"
        class="grid h-12 w-12 flex-none place-items-center rounded-control border border-border text-text-secondary transition-colors hover:text-text-primary">${lucideIcon("folder", 18)}</button>
    </div>
  </div>`;
}

// The PROJECTS header: the micro, tracked, uppercase
// eyebrow is itself a menubar-style menu button. The authored copy stays
// sentence case in the markup; CSS applies the casing with
// text-transform so screen readers still read "Projects". The trigger is
// tertiary at rest and primary on hover — colour only, the label voice —
// and takes the pressed wash only while its menu is held open, like a
// pressed menubar title.
// Neither the unanswered rail nor a stale one carries the Projects menu:
// every row it offers acts on the list, and there is no served list to act
// on. dimmed is the unanswered header, the eyebrow at the bands' weight;
// syncing is the stale header, the eyebrow trailed by a spinner and the
// word, over rows that are the last known list rather than the served one.
export function projectsHeaderHtml(options: { dimmed?: boolean; syncing?: boolean } = {}): string {
  const eyebrowClass = "px-2 py-[3px] text-micro font-medium uppercase leading-[1.55] tracking-(--type-micro-ls) text-text-tertiary";
  if (options.dimmed === true) {
    return `<div class="mb-1 flex min-h-[26px] items-center pr-[3px]">
    <span data-projects-eyebrow class="${eyebrowClass} opacity-60">Projects</span>
  </div>`;
  }
  if (options.syncing === true) {
    return `<div class="mb-1 flex min-h-[26px] items-center pr-[3px]">
    <span data-projects-eyebrow class="${eyebrowClass}">Projects</span>
    <span data-list-syncing role="status" class="ml-auto inline-flex items-center gap-1.5 text-micro text-text-tertiary"><span aria-hidden="true" class="block h-[10px] w-[10px] flex-none animate-spinner rounded-circle border-[1.5px] border-border-strong border-t-text-secondary"></span>Syncing</span>
  </div>`;
  }
  return `<div class="mb-1 flex min-h-[26px] items-center pr-[3px]">
    <button data-projects-menu aria-haspopup="menu" aria-expanded="false" data-hint="${hints.OPEN_PROJECTS_MENU}"
      class="rounded-control-sm ${eyebrowClass} transition-colors hover:text-text-primary aria-expanded:bg-wash-pressed">Projects</button>
  </div>`;
}

// The unanswered rail's bands: four
// project rows fading down the list, each with its session rows, flat and
// unanimated — Blueprint has no skeleton by design, so these neither
// shimmer nor pulse. Every band is the real row's height (a 19px line box
// inside the row's vertical padding) so the served rows land exactly where
// the bands were.
export function loadingBandsHtml(): string {
  const projects: Array<{ opacity: string; width: string; sessions: string[] }> = [
    { opacity: "1", width: "118px", sessions: ["132px", "96px"] },
    { opacity: ".7", width: "86px", sessions: ["110px"] },
    { opacity: ".45", width: "132px", sessions: ["146px", "88px"] },
    { opacity: ".25", width: "74px", sessions: [] },
  ];
  return projects.map((project) => {
    const sessions = project.sessions.map((width) =>
      `<li aria-hidden="true" class="flex h-[31px] items-center gap-2 px-2">
          <span class="h-4 w-4 flex-none rounded-mark bg-border"></span>
          <span class="h-2 rounded-pill bg-border" style="width:${width}"></span>
        </li>`).join("");
    return `<li data-loading-band aria-hidden="true" class="flex flex-col" style="opacity:${project.opacity}">
        <div class="flex h-[31px] items-center gap-(--row-icon-gap) pl-(--row-px)">
          <span class="inline-flex w-[13px] flex-none justify-center"><span class="h-[7px] w-[7px] rounded-[2px] bg-border-strong"></span></span>
          <span class="h-[9px] rounded-pill bg-border-strong" style="width:${project.width}"></span>
        </div>
        ${sessions === "" ? "" : `<ul class="mb-1.5 ml-[11px] mt-0.5 flex flex-col gap-px border-l-2 border-border pl-2">${sessions}</ul>`}
      </li>`;
  }).join("");
}

// The PROJECTS header's plain plus (an IconButton, not a
// "+ New" pill).
function plusGlyph(): string {
  return lucideIcon("plus", 14);
}

// The collapsed rail's expand mark (reference railCollapsed branch): 20px,
// masked from the mark artwork and filled with --evidence rather than the
// artwork's own ink, so the mark carries the product's evidence colour on
// the chrome surface. The button's accessible name carries the identity;
// the mark itself is decorative.
export function markImg(): string {
  const mask = "url(/brand/noisefloor-mark-inverse.svg) center/contain no-repeat";
  return `<span aria-hidden="true" class="block h-5 w-5 flex-none bg-evidence" style="-webkit-mask:${mask};mask:${mask}"></span>`;
}

// sessionTileHtml renders the 16px --radius-mark tile shared by the real
// session row and the pending-launch placeholder. Extracting it makes
// their parity structural instead of textual. Tiles carry
// colour, never letters: the background is the provider hue or the
// session-colour override, and nothing renders inside.
export function sessionTileHtml(provider: string, colorStyle = ""): string {
  return `<span class="h-4 w-4 flex-none rounded-mark ${providerTileClass(provider)}" ${colorStyle}></span>`;
}

// The project-row ⋯: three dots at the row-icon
// size, echoing the titlebar's session-menu glyph.
function ellipsisGlyph(): string {
  return lucideIcon("ellipsis", 13);
}
