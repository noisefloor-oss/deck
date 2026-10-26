// The workspace frame (the Claude Design artifact): 272px
// project sidebar left, titlebar above a terminal that fills the rest,
// and the Files and Tasks panels as DOCKED SIBLINGS in the content row.
// Neither overlays the terminal: each takes layout width, so the
// terminal keeps its own pixels and stays interactive beside it. At most
// one is open (app/docked-panels.ts). There is no composer/outbox stack
// — the terminal is the only input surface. Layout classes live
// here; the components own their internals.
// Collapsing the sidebar hides it entirely (zero width; the reference's
// railCollapsed branch defines no icon rail); the re-expand control
// ([data-region="sidebar-expand"]) lives at the left of the titlebar and
// is populated by the sidebar shell. Per the reference it is a 26px
// button carrying the noisefloor mark at 20px, inset 1px past the
// header's own padding, followed by a 1×18 divider that keeps the pane
// title out of the brand/expand group; both are hidden while the sidebar
// is expanded.
import * as hints from "../lib/hints.ts";

export function workspaceLayout(): string {
  return `
    <div data-workspace data-mono="operator" class="group/workspace flex h-screen w-screen flex-col overflow-hidden bg-sidebar-surface text-text-primary">
      <!-- The content row: [sidebar | main | right panel]. It is a row
           inside the shell's column flex, so the info bar sits below it
           as the last row. -->
      <div class="relative flex min-h-0 flex-1 items-stretch overflow-hidden">
        <aside data-region="sidebar"
          class="flex h-full w-(--sidebar-w) flex-none flex-col overflow-hidden border-r border-border bg-sidebar-surface max-md:fixed max-md:inset-y-0 max-md:left-0 max-md:z-30 max-md:w-[min(86vw,var(--sidebar-w))]">
        </aside>
        <div data-sidebar-resize role="separator" aria-orientation="vertical" aria-label="Resize sidebar"
          title="Drag to resize. Use Left or Right, Home or End. Press Enter or double-click to reset."
          tabindex="0" class="sidebar-resize"></div>
        <!-- The mobile sidebar overlays rather than compressing: as a flex
             sibling it left the terminal 118px of a 390px viewport. The
             scrim dismisses it and exists only while it is open. -->
        <div data-region="sidebar-scrim" aria-hidden="true"
          class="fixed inset-0 z-20 hidden bg-scrim group-data-[collapsed=false]/workspace:max-md:block md:hidden"></div>
        <!-- The content region holds the main column and both docked panels,
             so a panel's clamp resolves against the space right of the
             sidebar and an expanded
             file viewer can cover exactly that region. -->
        <div data-region="content" class="relative flex min-w-0 flex-1 items-stretch overflow-hidden">
          <main class="flex min-w-0 flex-1 flex-col bg-sidebar-surface">
            <div data-workspace-titlebar class="chrome w-full gap-(--space-4) px-(--space-5) max-md:px-3">
              <button data-region="sidebar-expand" aria-label="Show sidebar" data-hint="${hints.EXPAND_PROJECT_SIDEBAR}"
                class="ml-[1px] flex h-[26px] w-[26px] flex-none items-center justify-center rounded-control-sm focus-visible:bg-wash-hover pointer-coarse:h-10 pointer-coarse:w-10"></button>
              <span data-region="sidebar-expand-divider" hidden aria-hidden="true"
                class="h-[18px] w-px flex-none bg-border"></span>
              <div data-region="terminal-titlebar"
                class="flex min-w-0 flex-1 items-center gap-(--space-4)">
              </div>
              <div data-header-context-actions class="flex flex-none items-center gap-2">
              <span data-region="login-action" class="contents"></span>
              <button type="button" data-version-reload hidden data-hint="${hints.RELOAD_TO_UPDATE}"
                class="flex-none rounded-control border border-border px-2 py-1 text-caption text-text-secondary hover:bg-wash-hover hover:text-text-primary">New version available</button>
              </div>
            </div>
            <div data-region="terminal-card"
              class="flex min-h-0 flex-1 flex-col overflow-hidden bg-console-bg">
              <!-- The terminal paints edge-to-edge: the pane owns its
                   pixels with no chrome frame. -->
              <div data-region="terminal"
                class="relative min-h-0 flex-1 overflow-hidden bg-console-bg text-console-fg">
              </div>
              <div data-region="terminal-dock" aria-label="Terminal dock">
                <div data-region="tutorial-mobile-bar" hidden></div>
                <div data-region="terminal-status" aria-live="polite"></div>
                <div data-region="terminal-soft-keys" aria-label="Terminal keys"></div>
                <div data-region="terminal-compose" aria-label="Compose input"></div>
              </div>
            </div>
          </main>
          <!-- Both panels are docked splits, not overlays. Their full-width
               surfaces take their space immediately; layout margin gives or
               reclaims the same space beside the terminal. -->
          <aside data-region="files" aria-label="Files" aria-hidden="true" inert
            class="flex h-full flex-none flex-col overflow-hidden border-l border-border-strong bg-surface"
            data-open="false"><div data-panel-resize="files" role="separator" tabindex="-1" aria-orientation="vertical"
              aria-label="Resize Files panel" hidden class="absolute left-0 top-0 z-10 h-full w-2 cursor-col-resize"></div><div data-region="files-body" class="flex min-h-0 flex-1 flex-col overflow-hidden"></div></aside>
          <aside data-region="tasks" aria-label="Project tasks" aria-hidden="true" inert
            class="flex h-full flex-none flex-col overflow-hidden border-l border-border-strong bg-surface"
            data-open="false"><div data-panel-resize="tasks" role="separator" tabindex="-1" aria-orientation="vertical"
              aria-label="Resize Project tasks panel" hidden class="absolute left-0 top-0 z-10 h-full w-2 cursor-col-resize"></div><div data-region="tasks-body" class="flex min-h-0 flex-1 flex-col overflow-hidden"></div></aside>
          <aside data-region="chats" aria-label="Chats" aria-hidden="true" inert
            class="flex h-full flex-none flex-col overflow-hidden border-l border-border-strong bg-surface"
            data-open="false"><div data-panel-resize="chats" role="separator" tabindex="-1" aria-orientation="vertical"
              aria-label="Resize Chats panel" hidden class="absolute left-0 top-0 z-10 h-full w-2 cursor-col-resize"></div><div data-region="chats-body" class="flex min-h-0 flex-1 flex-col overflow-hidden"></div></aside>
          <aside data-region="assets" aria-label="Assets" aria-hidden="true" inert
            class="flex h-full flex-none flex-col overflow-hidden border-l border-border-strong bg-surface"
            data-open="false"><div data-panel-resize="assets" role="separator" tabindex="-1" aria-orientation="vertical"
              aria-label="Resize Assets panel" hidden class="absolute left-0 top-0 z-10 h-full w-2 cursor-col-resize"></div><div data-region="assets-body" class="flex min-h-0 flex-1 flex-col overflow-hidden"></div></aside>
          <aside data-region="tutorial" aria-label="Tutorial" aria-hidden="true" inert data-open="false"></aside>
          <div data-region="tutorial-bar" hidden></div>
          <div data-region="tutorial-notice" role="status" hidden class="absolute bottom-2 left-2 z-50 max-w-sm rounded-control border border-border bg-surface p-3 text-text-primary"></div>
        </div>
      </div>
      <!-- The info bar: the shell's last row, the chrome bar's --bottom
           modifier at 26px. Content is the hover/focus hint only;
           this region carries the geometry. -->
      <footer data-region="info-bar" class="chrome chrome-bottom">
        <span class="chrome-status min-w-0 overflow-hidden text-ellipsis whitespace-nowrap" data-info-status></span>
        <span data-release-notice role="status" hidden class="ml-auto flex self-stretch flex-none items-center gap-2 whitespace-nowrap text-caption text-text-secondary"></span>
      </footer>
      <!-- Dialogs own the top of the stack: the modals host sits above the
           popover host so a dialog never renders beneath a menu. -->
      <div data-region="modals" class="pointer-events-none fixed inset-0 z-60"></div>
      <div data-region="popovers" class="pointer-events-none fixed inset-0 z-50"></div>
    </div>
  `;
}

// Regions the shell mounts components into.
export function region(root: ParentNode, name: string): HTMLElement {
  const el = root.querySelector<HTMLElement>(`[data-region="${name}"]`);
  if (!el) {
    throw new Error(`missing region ${name}`);
  }
  return el;
}
