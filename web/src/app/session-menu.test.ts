// Tests for the More menu's markup. The menu IS the settings surface, so
// the toggles here are where the info-bar preference and three console
// settings are set.
import assert from "node:assert/strict";
import test from "node:test";
import { confirmsBeforeClose, confirmsBeforeRemove, menuShortcutHtml, projectsMenuHtml, rowMenuHtml, sessionMenuHtml, type SessionMenuState } from "./session-menu.ts";
import { DEFAULT_CUSTOM_SLOTS } from "../terminal/themes.ts";

function menu(over: Partial<SessionMenuState> = {}): string {
  return sessionMenuHtml({
    fontSize: 13,
    themeName: "noisefloor",
    themeFlyoutOpen: false,
    customSlots: DEFAULT_CUSTOM_SLOTS,
    showSidePanel: true,
    showInfoBar: true,
    autoColorProjects: false,
    skipPermissions: false,
    confirmingYolo: false,
    skipCloseConfirm: false,
    skipRemoveProjectConfirm: false,
    ...over,
  });
}

// toggleState reads a toggle row's checked state, and checks that its switch
// agrees. Returns "on", "off", or null if the row is missing.
function toggleState(html: string, key: string): "on" | "off" | null {
  const start = html.indexOf(`data-${key}`);
  if (start === -1) return null;
  const open = html.lastIndexOf("<button", start);
  const close = html.indexOf("</button>", start);
  if (open === -1 || close === -1) return null;
  const row = html.slice(open, close + 9);
  const m = /role="switch" aria-checked="(true|false)"/.exec(row);
  if (m === null) return null;
  assert.ok(row.includes("data-row-switch"), `${key} row has no switch`);
  assert.equal(row.includes("row-switch-on"), m[1] === "true", `${key} switch disagrees with aria-checked`);
  return m[1] === "true" ? "on" : "off";
}

// The single mapping from the stored setting to the behaviour. Only an
// explicit true may remove the confirmation — this is the whole fail-safe,
// and the reason it is one function rather than a `!== true` repeated at
// each of the places that need it.
test("only an explicit skip removes the close confirmation", () => {
  assert.equal(confirmsBeforeClose(null), true, "unread or unreadable keeps the prompt");
  assert.equal(confirmsBeforeClose(false), true, "not skipping keeps the prompt");
  assert.equal(confirmsBeforeClose(true), false, "the user's explicit opt-out is honoured");
});

// The same safe mapping holds for the project-removal confirmation.
test("only an explicit skip removes the remove-project confirmation", () => {
  assert.equal(confirmsBeforeRemove(null), true, "unread or unreadable keeps the prompt");
  assert.equal(confirmsBeforeRemove(false), true, "not skipping keeps the prompt");
  assert.equal(confirmsBeforeRemove(true), false, "the user's explicit opt-out is honoured");
});

test("Confirm before closing is on unless the stored skip is explicitly true", () => {
  assert.equal(toggleState(menu({ skipCloseConfirm: false }), "confirm-before-closing"), "on");
  assert.equal(toggleState(menu({ skipCloseConfirm: true }), "confirm-before-closing"), "off");
  assert.equal(toggleState(menu({ skipCloseConfirm: null }), "confirm-before-closing"), "on");
});

test("Confirm before removing a project is on unless the stored skip is explicitly true", () => {
  assert.equal(toggleState(menu({ skipRemoveProjectConfirm: false }), "confirm-before-removing"), "on");
  assert.equal(toggleState(menu({ skipRemoveProjectConfirm: true }), "confirm-before-removing"), "off");
  assert.equal(toggleState(menu({ skipRemoveProjectConfirm: null }), "confirm-before-removing"), "on");
});

test("an unread close-confirm setting still reads as on, not hidden or disabled", () => {
  const html = menu({ skipCloseConfirm: null });
  assert.ok(html.includes("Confirm before closing a session"), "the row names what it closes");
  assert.equal(toggleState(html, "confirm-before-closing"), "on");
});

test("the toggle rows and the other rows are present and independent", () => {
  const html = menu({ skipPermissions: true, skipCloseConfirm: true, showInfoBar: false });
  assert.equal(toggleState(html, "skip-permissions"), "on");
  assert.equal(toggleState(html, "confirm-before-closing"), "off", "yolo being on says nothing about the prompt");
  assert.equal(toggleState(html, "show-info-bar"), "off");
  assert.ok(html.includes("Show side panel"));
  assert.ok(html.includes("Show info bar"));
  assert.ok(html.includes("Enable yolo mode"));
  assert.ok(html.includes("Confirm before closing a session"), "the row names what it closes");
  assert.ok(html.includes("Confirm before removing a project"));
  assert.ok(html.includes("Auto-color projects"));
  assert.ok(html.includes("Git identity…"));
  assert.ok(html.includes("Sign out"));
});

test("the info-bar toggle mirrors the shown preference", () => {
  assert.equal(toggleState(menu({ showInfoBar: true }), "show-info-bar"), "on");
  assert.equal(toggleState(menu({ showInfoBar: false }), "show-info-bar"), "off");
});

test("the rainbow strip dims when auto-color is off", () => {
  assert.ok(menu({ autoColorProjects: true }).includes("opacity:1"));
  assert.ok(menu({ autoColorProjects: false }).includes("opacity:0.3"));
});

test("toggle rows are switch rows with data keys", () => {
  const html = menu();
  assert.ok(html.includes('data-auto-color role="switch"'));
  assert.ok(html.includes('data-show-info-bar role="switch"'));
  assert.ok(html.includes('data-confirm-before-closing role="switch"'));
  assert.ok(html.includes('data-skip-permissions role="switch"'));
  assert.ok(html.includes('data-confirm-before-removing role="switch"'));
});

test("no settings row sets its own label color", () => {
  const html = menu({ showInfoBar: false, skipPermissions: true });
  assert.doesNotMatch(html, /style="color:/);
  assert.doesNotMatch(html, /data-toggle-label/);
});

// Turning the prompt off removes a confirmation but destroys nothing the
// user has not already picked, so it is a plain toggle — unlike yolo
// mode, which expands into an inline confirmation before it can be granted.
test("only yolo mode asks before it is granted", () => {
  const confirmation = menu({ confirmingYolo: true });
  assert.ok(confirmation.includes("Turn it on"));
  assert.ok(confirmation.includes('data-skip-permissions role="switch" aria-checked="false" aria-expanded="true"'));
  assert.ok(confirmation.includes('aria-controls="yolo-confirmation"'));
  assert.ok(confirmation.includes('id="yolo-confirmation"'));
  assert.ok(!menu({ skipCloseConfirm: false }).includes("Turn it on"));
});

test("an unread yolo setting is unavailable, not falsely enabled", () => {
  const html = menu({ skipPermissions: null });
  assert.ok(html.includes('data-skip-permissions role="switch" aria-checked="false" aria-expanded="false"'));
  assert.ok(html.includes('aria-disabled="true"'));
});

test("auto-color exposes its checked state and hides the decorative rainbow", () => {
  assert.ok(menu({ autoColorProjects: true }).includes('data-auto-color role="switch" aria-checked="true"'));
  assert.ok(menu({ autoColorProjects: false }).includes('data-auto-color role="switch" aria-checked="false"'));
  assert.ok(menu().includes('<span aria-hidden="true" class="flex flex-none gap-[2px]">'));
});



// The sidebar row menus are the project and session ⋯
// popovers. They share rowMenuHtml — the same surface anatomy as the More
// menu, with branch-specific copy.
test("the project menu lists colour, position, and action groups in order", () => {
  const html = rowMenuHtml("project");
  assert.ok(html.includes('role="menu"'));
  assert.ok(html.includes('aria-label="Project actions"'));
  assert.ok(html.includes('data-proj-swatch'), "the project strip renders its swatches");
  assert.ok(html.includes('data-project-menu-move-up'));
  assert.ok(html.includes("Move up"));
  assert.ok(html.includes('data-project-menu-move-down'));
  assert.ok(html.includes("Move down"));
  assert.ok(html.includes('data-project-menu-move-top'));
  assert.ok(html.includes("Move to top"));
  assert.ok(html.includes('data-project-menu-preview'));
  assert.ok(html.includes("Preview…"));
  assert.ok(html.includes('data-project-menu-rename'));
  assert.ok(html.includes("Rename"));
  assert.ok(html.includes('data-project-menu-identity'));
  assert.ok(html.includes("Project git identity…"));
  assert.ok(html.includes('data-project-menu-remove'));
  assert.ok(html.includes("Remove from sidebar"));
  assert.ok(!html.includes("Rename project…"), "the menu takes the short label");
  assert.ok(!html.includes("Change git identity…"), "the menu takes the project-specific label");
  const strip = html.indexOf("data-proj-swatch");
  const moveUp = html.indexOf("data-project-menu-move-up");
  const moveDown = html.indexOf("data-project-menu-move-down");
  const moveTop = html.indexOf("data-project-menu-move-top");
  const preview = html.indexOf("data-project-menu-preview");
  const rename = html.indexOf("data-project-menu-rename");
  const identity = html.indexOf("data-project-menu-identity");
  const remove = html.indexOf("data-project-menu-remove");
  // The long move closes the position group: the two swaps first, then the
  // row that skips the walk. Preview has its own group after identity.
  assert.ok(strip < moveUp && moveUp < moveDown && moveDown < moveTop && moveTop < rename && rename < identity && identity < preview && preview < remove,
    "rows follow the design's order");
  assert.equal((html.match(/<hr class="menu-sep">/g) ?? []).length, 4, "five groups have four separators");
  assert.ok(html.includes("data-hint=\"Set up this project&#39;s app preview\""), "verb-first preview hint");
  assert.ok(html.includes('data-hint="Rename this project"'), "verb-first hint");
  assert.ok(html.includes('style="min-width:196px"'), "the design pins the project width");
});

// The session menu is a collapsed row model: a colour
// strip picks the tag, then a separator, then the two short verbs — no
// "Tag session color" row, because the strip IS the color control.
test("the session menu lists the colour strip, rename, and close in order", () => {
  const html = rowMenuHtml("session", "violet");
  assert.ok(html.includes('role="menu"'));
  assert.ok(html.includes('aria-label="Session actions"'));
  assert.ok(html.includes('data-sess-swatch'), "the strip renders its swatches");
  assert.ok(html.includes('data-session-menu-rename'));
  assert.ok(html.includes("Rename"));
  assert.ok(html.includes('data-session-menu-close'));
  assert.ok(html.includes("Close"));
  assert.ok(!html.includes("Rename session…"), "the menu takes the short label");
  assert.ok(!html.includes("Close session…"), "the menu takes the short label");
  assert.ok(!html.includes("data-session-menu-color"), "no separate color row — the strip owns it");
  const strip = html.indexOf("data-sess-swatch");
  const rename = html.indexOf("data-session-menu-rename");
  const close = html.indexOf("data-session-menu-close");
  assert.ok(strip < rename && rename < close, "strip, then Rename, then Close");
  assert.ok(html.includes('data-hint="End this session&#39;s process and keep the recording"'),
    "the close hint states the recording is kept, not destroyed");
  assert.ok(html.includes('style="min-width:190px"'), "the design pins the session width");
});

// The strip's selection state is the ring: the current tag takes a
// text-primary border plus the inset surface-card mark; everything else
// is transparent, except "none", whose empty swatch keeps a border-strong
// outline so it stays visible unselected.
test("the session menu's strip marks the current tag and an unset one", () => {
  const tagged = rowMenuHtml("session", "violet");
  assert.match(tagged, /data-sess-swatch="violet"[^>]*aria-label="Violet"[^>]*style="[^"]*border-color:var\(--color-text-primary\)[^"]*box-shadow:inset 0 0 0 1\.5px var\(--color-surface-card\)/);
  assert.match(tagged, /data-sess-swatch="lime"[^>]*style="[^"]*border-color:transparent;box-shadow:none/);
  assert.match(tagged, /data-sess-swatch="none"[^>]*style="[^"]*border-color:var\(--color-border-strong\);box-shadow:none/);

  const cleared = rowMenuHtml("session", null);
  assert.match(cleared, /data-sess-swatch="none"[^>]*style="[^"]*border-color:var\(--color-text-primary\)[^"]*box-shadow:inset 0 0 0 1\.5px var\(--color-surface-card\)/);
  assert.match(cleared, /data-sess-swatch="violet"[^>]*style="[^"]*border-color:transparent;box-shadow:none/,
    "an unset session marks no color swatch");
});

// Sixteen 15px circles at the session mark radius — "No color" first,
// the eleven mark hues in circle order, the four neutral identities
// last — in a two-row eight-column grid, because the swatches are a set
// the user scans, not a submenu to arrow through: they carry no
// menuitem role, so keyboard focus enters the menu at Rename.
test("the session menu's strip is the mark palette plus neutrals in a two-row grid", () => {
  const html = rowMenuHtml("session", null);
  const keys = [...html.matchAll(/data-sess-swatch="([^"]*)"/g)].map((m) => m[1]);
  assert.deepEqual(keys, [
    "none", "crimson", "vermilion", "gold", "lime", "emerald", "teal",
    "cerulean", "sapphire", "indigo", "violet", "orchid", "pearl",
    "silver", "graphite", "ink",
  ]);
  assert.ok(/data-sess-swatch="none"[^>]*class="[^"]*h-\[15px\] w-\[15px\][^"]*rounded-mark/.test(html));
  // Two rows of eight fixed columns spread across the menu: a start-packed
  // row would leave the last chip short of the right edge.
  assert.ok(html.includes('class="grid grid-cols-[repeat(8,15px)] justify-between gap-y-1.5 px-2 pt-1 pb-1.5"'));
});

// A tag stored under a legacy family key still marks its canonical swatch
// current — an old "red" reads as the crimson it always was, not as no
// choice.
test("a legacy tag key marks its canonical swatch current", () => {
  const html = rowMenuHtml("session", "red");
  assert.match(html, /data-sess-swatch="crimson"[^>]*style="[^"]*border-color:var\(--color-text-primary\)/);
  assert.match(html, /data-sess-swatch="lime"[^>]*style="[^"]*border-color:transparent/);
});

test("row menu actions are menuitem buttons, separated by a hairline", () => {
  for (const kind of ["project", "session"] as const) {
    const html = rowMenuHtml(kind);
    // The project menu's colour row is unconditional — it propagates the
    // project's colour, and "none" is one — so it counts even here, where
    // project has none. Its position group is three rows (up, down, top)
    // and its action group four (preview, rename, identity, remove).
    const menuitems = kind === "session" ? 2 : 8;
    assert.equal((html.match(/role="menuitem"/g) ?? []).length, menuitems, kind);
    // Both menus' sixteen swatches are buttons without a menuitem role, so
    // they add to the button count without adding menu rows.
    assert.equal((html.match(/<button type="button"/g) ?? []).length, menuitems + 16, kind);
    assert.equal((html.match(/<hr class="menu-sep">/g) ?? []).length, kind === "project" ? 4 : 1, kind);
    assert.ok(html.includes('class="menu absolute max-w-[calc(100vw-24px)]"'), kind);
  }
});

// The row always propagates the project's own colour, and "none" is a
// colour: with no user colour it is the clearing action, dotted with
// the strip's empty swatch. The auto-color tint never appears here, so a
// project that merely looks coloured offers nothing to apply.
test("the project menu's colour row propagates the project's own colour", () => {
  const withoutColor = rowMenuHtml("project");
  assert.ok(withoutColor.includes("Clear color from sessions"),
    "no user colour makes the row the clearing one");
  assert.ok(!withoutColor.includes("Apply color to sessions"));
  assert.ok(withoutColor.includes("data-project-menu-apply-color"), "the row is still offered");
  assert.match(withoutColor,
    /data-project-menu-apply-color[^>]*data-hint="Clear the color from every session in this project"/);
  assert.match(withoutColor, /style="background:transparent;border:1px solid var\(--color-border-strong\)"/,
    "the dot is the strip's empty swatch, not a hue");

  const withColor = rowMenuHtml("project", "violet");
  assert.ok(withColor.includes("Apply color to sessions"));
  assert.ok(!withColor.includes("Clear color from sessions"));
  assert.ok(withColor.includes('role="menuitem"'), "the apply-color row is a menuitem");
  assert.match(withColor, /data-project-menu-apply-color[^>]*data-hint="Apply this project color to every session in it"/);
  assert.ok(withColor.includes("background:var(--color-mark-violet)"), "the row dots the project's violet mark var");

  // The stored "none" key is a colour the strip marks current, not a hue to
  // propagate: a bare resolveTagKey would return it and paint the dot with it.
  const storedNone = rowMenuHtml("project", "none");
  assert.ok(storedNone.includes("Clear color from sessions"));
  assert.ok(!storedNone.includes("background:none"), "no CSS keyword reaches the dot");

  assert.equal((withoutColor.match(/<hr class="menu-sep">/g) ?? []).length, 4);
  assert.equal((withColor.match(/<hr class="menu-sep">/g) ?? []).length, 4);
});

test("project move rows render disabled at the matching sidebar boundary", () => {
  const first = rowMenuHtml("project", null, { canMoveUp: false, canMoveDown: true });
  assert.match(first, /data-project-menu-move-up[^>]*aria-disabled="true"[^>]*data-hint="Already first in the sidebar"/);
  assert.match(first, /data-project-menu-move-down(?![^>]*aria-disabled)[^>]*data-hint="Swap places with the project below"/);
  assert.ok(!/<button[^>]*data-project-menu-move-up[^>]*\sdisabled(?:\s|=|>)/.test(first), "the row stays event-capable");
  // Top shares Move up's boundary: first is first.
  assert.match(first, /data-project-menu-move-top[^>]*aria-disabled="true"[^>]*data-hint="Already first in the sidebar"/);

  const last = rowMenuHtml("project", null, { canMoveUp: true, canMoveDown: false });
  assert.match(last, /data-project-menu-move-down[^>]*aria-disabled="true"[^>]*data-hint="Already last in the sidebar"/);
  assert.match(last, /data-project-menu-move-up(?![^>]*aria-disabled)[^>]*data-hint="Swap places with the project above"/);
  assert.ok(!/<button[^>]*data-project-menu-move-down[^>]*\sdisabled(?:\s|=|>)/.test(last), "the row stays event-capable");
  assert.match(last, /data-project-menu-move-top(?![^>]*aria-disabled)[^>]*data-hint="Move to the top of the sidebar"/);
});



// A printed shortcut is a promise: the glyphs appear only on the
// rows whose chords the app claims, and the browser-reserved ⌘N/⌘T/⌘W
// never appear anywhere — the browser takes them before the page sees
// the event, so printing them would advertise keys that do nothing.
test("each wired row prints its glyph, and unwired rows print none", () => {
  const html = menu();
  assert.ok(html.includes("Show side panel"));
  assert.ok(html.includes("Show info bar"));
  assert.ok(html.includes("Enable yolo mode"));
  for (const forbidden of ["⌘N", "⌘T", "⌘W"]) {
    assert.ok(!html.includes(forbidden), `${forbidden} is the browser's, not ours`);
  }
  // The shortcut column sits beside the label on each wired row.
  for (const [label, command, control] of [
    ["Show side panel", "toggleRail", "show-side-panel"],
    ["Show info bar", "toggleInfoBar", "show-info-bar"],
    ["Enable yolo mode", "toggleYolo", "skip-permissions"],
  ] as const) {
    const labelAt = html.indexOf(`>${label}</span>`);
    const row = html.slice(labelAt, html.indexOf(`data-${control}`, labelAt));
    assert.ok(row.includes(menuShortcutHtml(command)), `${label} prints its key`);
  }
  // The close-confirm row has no binding, so no glyph.
  const confirmRow = html.slice(
    html.indexOf("Confirm before closing a session"),
    html.indexOf("data-confirm-before-closing"),
  );
  assert.ok(!confirmRow.includes("translateY"), "an unbound row prints no glyph");
});

test("the side-panel toggle mirrors the rail, first in the toggle band", () => {
  assert.equal(toggleState(menu({ showSidePanel: true }), "show-side-panel"), "on");
  assert.equal(toggleState(menu({ showSidePanel: false }), "show-side-panel"), "off");
  const html = menu();
  assert.ok(html.indexOf("Show side panel") < html.indexOf("Show info bar"),
    "chrome visibility rows come first");
});

test("the glyph markup follows the design's optical corrections", () => {
  // ⌥⌘Y: a modifier following a modifier pulls in by 0.13em; the single
  // modifiers on ⌘B do not.
  const yolo = menuShortcutHtml("toggleYolo", true);
  assert.equal((yolo.match(/margin-left:-0.13em/g) ?? []).length, 1);
  assert.ok(yolo.includes("gap:0.34em"));
  assert.ok(yolo.includes("transform:translateY(0.055em)"));
  assert.ok(yolo.includes("transform:translateY(-0.055em)"));
  const plain = menuShortcutHtml("toggleRail", true);
  assert.ok(!plain.includes("margin-left"), "one modifier keeps the full gap");
  assert.ok(plain.includes("color:var(--color-text-tertiary)"));
});

test("the theme flyout marks the current row with an accent check and primary name", () => {
  const html = menu({ themeFlyoutOpen: true, themeName: "noisefloor" });
  const activeAt = html.indexOf('data-theme-pick="noisefloor"');
  const activeEnd = html.indexOf("</button>", activeAt);
  const activeRow = html.slice(activeAt, activeEnd);
  assert.ok(activeRow.includes("text-accent"), "current row check is accent");
  assert.ok(activeRow.includes("text-text-primary"), "current row name is primary");

  const otherAt = html.indexOf('data-theme-pick="gruvbox"');
  const otherEnd = html.indexOf("</button>", otherAt);
  const otherRow = html.slice(otherAt, otherEnd);
  assert.ok(otherRow.includes("text-text-secondary"), "other row name is secondary");
});

test("both Rename rows print the platform shortcut, and no other row does", () => {
  for (const kind of ["project", "session"] as const) {
    const html = rowMenuHtml(kind);
    // The session menu's swatch strip precedes Rename, so the row must be
    // sliced from its own start; a bare </button> search would stop at a
    // swatch and slice backwards to nothing.
    const renameAt = html.indexOf("menu-rename");
    const rename = html.slice(renameAt, html.indexOf("</button>", renameAt));
    assert.ok(rename.includes(menuShortcutHtml("renameSelection")), `${kind} rename prints its shortcut`);
    const otherAction = kind === "project" ? "data-project-menu-identity" : "data-session-menu-close";
    const otherStart = html.indexOf(otherAction);
    const otherEnd = html.indexOf("</button>", otherStart);
    const other = html.slice(otherStart, otherEnd);
    assert.ok(!other.includes("data-menu-shortcut"), `${kind} non-rename row prints no glyph`);
    for (const forbidden of ["⌘N", "⌘T", "⌘W"]) {
      assert.ok(!html.includes(forbidden), kind);
    }
  }
});

// Pin the settings-menu surface details that are fixed: the
// 252px min-width, the overlay menu class, the singular Git identity
// label, and the absence of description sub-lines beneath toggles.
test("the settings menu surface matches its fixed values", () => {
  const html = menu();
  assert.ok(html.includes('class="menu absolute max-w-[calc(100vw-24px)]" style="min-width:252px"'),
    "popover uses the overlay menu class at the fixed min-width");
  assert.ok(html.includes(">Git identity…</button>"), "Git identity label is singular");
  assert.ok(!html.includes("Git identities…"), "old plural label is gone");
});

// The PROJECTS header menu: five rows in a fixed order,
// grouped by kind — commands, order, disclosure — with a hairline between
// groups.
test("the projects header menu holds exactly five rows in order, with two separators", () => {
  const html = projectsMenuHtml("field-notes");
  const labels = [...html.matchAll(/role="menuitem"[\s\S]*?<span class="flex-1">([^<]*)<\/span>/g)]
    .map((m) => m[1]);
  assert.deepEqual(labels, ["Add project…", "New session", "Sort by name", "Expand all", "Collapse all"]);
  assert.equal(html.match(/menu-sep/g)?.length, 2, "exactly two separators");
  // Grouping: one separator between New session and Sort by name, one
  // between Sort by name and Expand all.
  const at = (needle: string): number => html.indexOf(needle);
  const seps = [...html.matchAll(/<hr class="menu-sep">/g)].map((m) => m.index!);
  assert.ok(at("New session") < seps[0]! && seps[0]! < at("Sort by name"), "commands | order");
  assert.ok(at("Sort by name") < seps[1]! && seps[1]! < at("Expand all"), "order | disclosure");
});

test("the header menu's New session names no target, so every row is plain text", () => {
  const html = projectsMenuHtml("field-notes");
  const row = /data-projects-menu-new-session[\s\S]*?<\/button>/.exec(html)![0];
  // The target is disclosed in the dialog this row opens, not here. A caption
  // would be the only trailing column in the menu, so its rows would stop
  // being uniform.
  const body = row.slice(row.indexOf(">") + 1);
  assert.equal(body.trim(), '<span class="flex-1">New session</span></button>', body);
  assert.ok(!row.includes("text-caption"), `the row still carries caption type: ${row}`);
  // The hint still names the target: a hover disclosure is not a column.
  assert.ok(/data-hint="[^"]*field-notes/.test(row), `the hint stopped naming the target: ${row}`);
});

test("the header menu takes the overlay's density, never its own inset", () => {
  const html = projectsMenuHtml("field-notes");
  // Density belongs to .menu. A per-menu override moved one axis and left the
  // other, which is the split spacing.css calls a bug, so the panel must carry
  // none of the row family and no padding of its own — pin the whole inline
  // style rather than the spellings that happened to be there.
  const style = /<div data-projects-menu-popover[\s\S]*?style="([^"]*)"/.exec(html)![1];
  assert.equal(style, "min-width:196px", `the panel carries inline style beyond its width: ${style}`);
  for (const axis of ["--row-py", "--row-px", "--row-icon-gap"]) {
    assert.ok(!html.includes(axis), `the panel still sets ${axis}`);
  }
  assert.ok(!/data-projects-menu-popover[\s\S]*?padding:/.test(html), `the panel still sets padding: ${html}`);
  assert.ok(!html.includes("left:-"), "no negative left anywhere in the panel");
  assert.ok(html.includes('class="menu absolute max-w-[calc(100vw-24px)]"'),
    "same overlay menu anatomy as every other popover");
});

test("the header menu escapes the target project name", () => {
  const html = projectsMenuHtml('<img src=x onerror=alert(1)>');
  assert.ok(!html.includes("<img"), html);
});


test("Windows menu shortcuts use Ctrl and Alt text without Mac glyphs", () => {
  for (const [command, label] of [
    ["toggleRail", "Ctrl+B"], ["toggleInfoBar", "Ctrl+/"],
    ["toggleYolo", "Ctrl+Alt+Y"], ["renameSelection", "Ctrl+R"],
  ] as const) {
    const html = menuShortcutHtml(command, false);
    assert.ok(html.includes(`>${label}</span>`));
    assert.doesNotMatch(html, /[⌘⌥]/);
    assert.doesNotMatch(html, /translateY|margin-left/);
  }
});

test("the mobile project sheet names its project without losing existing actions", () => {
  const html = rowMenuHtml("project", null, { name: "project <one>" });
  assert.match(html, /data-project-menu-sheet-head/);
  assert.match(html, /data-project-menu-sheet-title[^>]*>project &lt;one&gt;<\/span>/);
  assert.match(html, /data-proj-swatch/);
  assert.match(html, /data-project-menu-apply-color/);
  assert.match(html, /data-project-menu-move-top/);
  assert.match(html, /data-project-menu-preview-settings/);
  assert.match(html, /data-project-menu-remove/);
  assert.doesNotMatch(rowMenuHtml("session"), /data-project-menu-sheet-head/);
});

test("Show tutorial is an action that leads the account actions", () => {
  const html = menu();
  assert.match(html, /data-show-tutorial data-hint="[^"]+" class="menuitem">Show tutorial<\/button>/);
  const seps = [...html.matchAll(/<hr class="menu-sep">/g)].map((m) => m.index!);
  assert.ok(html.indexOf("Confirm before removing a project") < seps[1]!, "toggles end before the second separator");
  assert.ok(seps[1]! < html.indexOf("Show tutorial"), "Show tutorial sits after the toggles");
  assert.ok(html.indexOf("Show tutorial") < html.indexOf("GitHub connection…"));
  assert.ok(html.indexOf("GitHub connection…") < html.indexOf("Git identity…"));
});

test("change password is offered only in password mode", () => {
  assert.match(menu({ changePassword: true }), /data-change-password/);
  assert.match(menu({ changePassword: true }), />Change password<\/button>/);
  assert.ok(!menu().includes("data-change-password"));
});

test("the login row is offered only while a login is waiting, named after the agent", () => {
  assert.ok(!menu().includes("data-harness-login"));
  const waiting = menu({ loginLabel: "Finish Pi login" });
  assert.match(waiting, /data-harness-login data-hint="Finish the sign-in this session is waiting for" class="menuitem">Finish Pi login<\/button>/);
  assert.doesNotMatch(waiting, /harness login/i);
});
