// Tests for the pure markup helpers in the sidebar's DOM shell. The class
// itself needs a real browser (there is no jsdom here), so the pieces that
// can be decided without one are kept as module functions and pinned here.
import { readdirSync, readFileSync } from "node:fs";
import assert from "node:assert/strict";
import test from "node:test";

import {
  emptyRailNewSessionHtml,
  pendingSessionRowHtml,
  cloneJobStaysListed,
  cloneRowHtml,
  projectRowHtml,
  projectChevronHtml,
  loadingBandsHtml,
  projectsHeaderHtml,
  sessionActionControlsHtml,
  sessionRailContainerHtml,
  sessionRailStyle,
  sessionTileHtml,
  lockupImg,
} from "./markup.ts";
import {
  moveProjectOneStep,
  moveProjectToTop,
  moveProjectRelative,
  moveSessionRelative,
  newSessionTargetProject,
  sortedProjectOrderByName,
  presentedProjectOrder,
  presentedRowOrder,
  presentedSessionOrder,
  reconcileCreatedProjectExpansion,
} from "./order.ts";
import { rowDropSlot } from "./drag.ts";
import type { CloneJob } from "../../api/projects.ts";
import {
  newSessionBodyHtml,
  newSessionDialogTarget,
  newSessionVisibleProviders,
} from "./dialogs/new-session.ts";
import { FIXTURE_PROVIDERS } from "../../app/fixture-providers.ts";

// assertNoNestedDataHints fails when an element carrying [data-hint] has a
// descendant that also carries [data-hint]. hintValue() resolves with the
// nearest ancestor, so nested hints shadow each other and are always a bug.
function assertNoNestedDataHints(html: string, message: string): void {
  const voidTags = new Set([
    "area", "base", "br", "col", "embed", "hr", "img", "input", "link",
    "meta", "param", "source", "track", "wbr",
  ]);
  // Matches opening tags, closing tags, and self-closing tags. Captures:
  // 1: slash for closing tags, 2: tag name, 3: attributes, 4: slash for self-closing.
  const tagRegex = /<(\/?)([a-zA-Z][a-zA-Z0-9-]*)\b([^>]*?)(\/?)>/g;
  const hintStack: boolean[] = [];
  let match: RegExpExecArray | null;
  while ((match = tagRegex.exec(html)) !== null) {
    const [, slash, tagName, attrs, selfClose] = match;
    if (slash === "/") {
      hintStack.pop();
      continue;
    }
    const hasHint = /\sdata-hint\s*=/.test(attrs ?? "");
    if (hasHint && hintStack.some((h) => h)) {
      assert.fail(`${message}: nested data-hint around \`${match[0] ?? ""}\``);
    }
    if (selfClose !== "/" && !voidTags.has(tagName ?? "")) {
      hintStack.push(hasHint);
    }
  }
}
import { lucideIcon } from "../../ui/icons.ts";
import { providerTileClass } from "../../ui/session-identity.ts";
import type { ProjectWithSessions } from "./sidebar.ts";

test("a created project expands only after the refreshed list contains it", () => {
  const group = (id: string): ProjectWithSessions => ({
    project: { id, name: id, canonical_path: `/srv/${id}`, last_used_at: "t" },
    sessions: [],
  });
  const expanded = new Set(["kept-open"]);

  assert.equal(
    reconcileCreatedProjectExpansion(expanded, "created", [group("kept-open"), group("kept-closed")]),
    "created",
  );
  assert.deepEqual([...expanded], ["kept-open"]);

  assert.equal(
    reconcileCreatedProjectExpansion(
      expanded,
      "created",
      [group("kept-open"), group("kept-closed"), group("created")],
    ),
    null,
  );
  assert.deepEqual([...expanded], ["kept-open", "created"]);
  assert.equal(expanded.has("kept-closed"), false);
});

// FULL is a full image's served inventory. The picker has no list of its
// own any more: whatever the server sends is what it draws, in that order,
// so every test has to state the inventory it is drawing.
const FULL = FIXTURE_PROVIDERS;


// The picker draws the served inventory and nothing else. A slim image is a
// console with two harnesses and a terminal, so that is what it must show —
// the other thirteen are not installed, and a chip for one of them is a
// button that fails when pressed.
test("the picker draws the served harnesses and no others", () => {
  const slim = ["claude", "codex", "shell"];
  const html = newSessionBodyHtml({
    available: slim, filter: "", launching: null, creating: false,
    selection: null,
  });
  for (const provider of slim) {
    assert.ok(html.includes(`data-new-session-provider="${provider}"`), `${provider} is offered`);
  }
  for (const absent of FULL.filter((provider) => !slim.includes(provider))) {
    assert.ok(!html.includes(`data-new-session-provider="${absent}"`),
      `${absent} is not installed and must not be a chip`);
  }
  assert.ok(html.includes('placeholder="Filter 3 harnesses"'),
    "the count names what this image has, not what the bundle can draw");
});

// The order is the server's too. Nothing here re-sorts it: a client-side
// order is a second opinion, and the picker has no basis for one.
test("the chips keep the order they were served in", () => {
  const html = newSessionBodyHtml({
    available: ["shell", "codex", "claude"], filter: "", launching: null,
    creating: false, selection: null,
  });
  const drawn = [...html.matchAll(/data-new-session-provider="([^"]+)"/g)].map((m) => m[1]);
  assert.deepEqual(drawn, ["shell", "codex", "claude"]);
});

// An inventory nobody could read is an empty picker that says so. It must not
// be reported as a search that found nothing: you typed no search.
test("an empty inventory renders an empty picker, not an empty search result", () => {
  const html = newSessionBodyHtml({
    available: [], filter: "", launching: null, creating: false,
    selection: null,
  });
  assert.ok(!html.includes("data-new-session-provider"), "no chip renders");
  assert.ok(html.includes("No harnesses are available"), "the empty state names the cause");
  assert.ok(!html.includes("No provider matches"), "nothing was searched for");
  assert.ok(!/data-new-session-empty[^>]*hidden/.test(html), "the message is visible");
  assert.ok(!html.includes("data-new-session-inventory-unread"), "a served empty list is not a fetch failure");
});

// A fetch failure with no previous good inventory cannot claim emptiness. It
// must say it could not ask and offer a retry.
test("an unknown inventory shows the unread state with a retry", () => {
  const html = newSessionBodyHtml({
    available: [], filter: "", launching: null, creating: false,
    selection: null,
    inventoryStatus: "unknown", inventoryError: "could not reach server",
  });
  assert.ok(!html.includes("No harnesses are available"), "unknown must not claim emptiness");
  assert.ok(!html.includes("data-new-session-provider"), "no chips render when nothing is known");
  assert.ok(html.includes("data-new-session-inventory-unread"), "the unread banner renders");
  assert.ok(html.includes("could not reach server"), "the error is shown");
  assert.ok(html.includes("data-new-session-inventory-retry"), "a retry button is offered");
  assert.ok(!html.includes("data-new-session-empty"), "the empty element is not rendered");
});

// A stale inventory keeps the last good list on screen while telling you
// the refresh failed and offering a retry.
test("a stale inventory keeps the last good chips and shows a retry", () => {
  const html = newSessionBodyHtml({
    available: ["claude", "codex"], filter: "", launching: null, creating: false,
    selection: null,
    inventoryStatus: "stale", inventoryError: "refresh failed",
  });
  assert.ok(html.includes('data-new-session-provider="claude"'), "stale keeps claude");
  assert.ok(html.includes('data-new-session-provider="codex"'), "stale keeps codex");
  assert.ok(html.includes("data-new-session-inventory-unread"), "the unread banner renders");
  assert.ok(html.includes("refresh failed"), "the error is shown");
  assert.ok(html.includes("data-new-session-inventory-retry"), "a retry button is offered");
  assert.ok(!html.includes("No harnesses are available"), "stale must not claim server-stated emptiness");
});

// A stale inventory with a last-good empty list still shows the retry and
// does not show the server-stated empty message.
test("a stale empty inventory shows the retry, not the empty message", () => {
  const html = newSessionBodyHtml({
    available: [], filter: "", launching: null, creating: false,
    selection: null,
    inventoryStatus: "stale", inventoryError: "refresh failed",
  });
  assert.ok(html.includes("data-new-session-inventory-unread"), "the unread banner renders");
  assert.ok(!html.includes("No harnesses are available"), "stale must not claim server-stated emptiness");
  assert.ok(!html.includes("data-new-session-empty"), "the empty element is not rendered");
});

// The retry button disables itself while a refresh is in flight so you
// cannot double-click, and it exposes a hook tests can use to prove
// the listener actually fired.
test("the retry button shows an in-flight state while refreshing", () => {
  const html = newSessionBodyHtml({
    available: ["claude", "codex"], filter: "", launching: null, creating: false,
    selection: null,
    inventoryStatus: "stale", inventoryError: "refresh failed",
    inventoryRefreshing: true,
  });
  assert.ok(html.includes("data-new-session-inventory-retrying"), "the retrying flag is rendered");
  assert.ok(/data-new-session-inventory-retry[^>]*disabled/.test(html), "the retry button is disabled while refreshing");
});


// The phone sheet draws rows instead of chips from the same inventory.
test("the phone sheet lists only the served harnesses", () => {
  const html = newSessionBodyHtml({
    available: ["claude", "codex", "shell"], filter: "", launching: null, creating: false,
    selection: null, phone: true,
  });
  assert.ok(!html.includes('data-new-session-provider="devin"'),
    "a recent harness this image no longer ships is not a row");
  assert.ok(html.includes('data-new-session-provider="codex"'), "every installed harness is offered");
});

// The New session dialog: a filter field followed by the complete inventory as wrapping chips.
// Picking commits, so there is no footer and no primary.
test("the new-session body renders chips, not a grid, and no overflow control", () => {
  const html = newSessionBodyHtml({ available: FULL, filter: "", launching: null, creating: false, selection: null });
  assert.ok(html.includes('data-new-session-filter'), "the filter field renders");
  assert.ok(html.includes("search"), "the filter carries the lead search icon");
  assert.ok(html.includes('data-new-session-chips'), "the inventory renders as chips");
  assert.ok(html.includes("rounded-pill"), "chips use the pill radius");
  assert.ok(!html.includes("data-new-session-more"), "the overflow cell is gone");
  assert.ok(!html.includes("grid-cols-1"), "no fixed grid remains");
  assert.ok(!html.includes("dialog-foot"), "picking commits — no footer, no primary");
});



test("the new-session body escapes its filter into the empty message", () => {
  const html = newSessionBodyHtml({ available: FULL, filter: '"><img src=x onerror=alert(1)>', launching: null, creating: false, selection: null });
  assert.ok(!html.includes("<img"), "no tag survives into the empty message");
  assert.ok(html.includes("&lt;img"), "the filter renders as visible text in the message");
});

test("a filtered provider chip is drawn with its own hue and colour classes", () => {
  const html = newSessionBodyHtml({ available: FULL, filter: "clau", launching: null, creating: false, selection: null });
  assert.ok(html.includes('data-new-session-provider="claude"'), "the matching provider chip renders");
  assert.ok(html.includes(providerTileClass("claude")), "the chip carries the provider's hue classes");
  assert.ok(html.includes('data-new-session-empty') && /hidden/.test(html), "the empty message is present but hidden");
});

test("an empty filter result hides every chip and shows the empty message", () => {
  const html = newSessionBodyHtml({ available: FULL, filter: "zzz", launching: null, creating: false, selection: null });
  assert.ok(!html.includes('data-new-session-provider'), "no provider chip renders");
  assert.ok(html.includes('data-new-session-empty') && !/data-new-session-empty[^>]*hidden/.test(html), "the empty message is visible");
});


test("a chip wears the border recipe at rest and when armed", () => {
  const html = newSessionBodyHtml({ available: FULL, filter: "", launching: null, creating: false, selection: 0 });
  const chipTag = /<button type="button" data-new-session-provider="[^"]+"[^>]*class="([^"]*)"/.exec(html);
  assert.ok(chipTag !== null, "a chip opening tag renders with a class attribute");
  const classes = chipTag[1]!.split(/\s+/);
  // Resting: a --border hairline on --surface (W6 2320–2322). A bare
  // `border` class defaults to currentColor and shouts.
  assert.ok(classes.includes("border-border"), "resting chips carry the hairline border color");
  assert.ok(classes.includes("bg-surface"), "resting chips sit on the surface ground");
  // Armed: --accent-border with the selected wash — never the full accent.
  assert.ok(classes.includes("data-[selected=true]:border-accent-border"),
    "armed chips use the accent-border token");
  assert.ok(!classes.includes("data-[selected=true]:border-accent"),
    "armed chips must not take the full accent border");
  assert.ok(classes.includes("data-[selected=true]:bg-wash-selected"), "armed chips keep the selected wash");
  assert.match(html, /data-selected="true"/, "the first chip is armed");
});

test("a pending launch draws a row only in its own project", () => {
  const pending = { projectID: "p1", provider: "claude" };
  assert.equal(pendingSessionRowHtml(pending, "p2"), "");
  assert.equal(pendingSessionRowHtml(null, "p1"), "");
  assert.ok(pendingSessionRowHtml(pending, "p1").includes("<li"));
});

test("the pending row shows the provider being launched, with a spinner", () => {
  const html = pendingSessionRowHtml({ projectID: "p1", provider: "opencode" }, "p1");
  assert.ok(html.includes("opencode"), "the name is the provider's");
  assert.ok(html.includes("bg-provider-opencode"), "the tile carries the provider's color");
  assert.ok(html.includes("animate-row-spinner"), "it spins while the launch is in flight");
  assert.ok(html.includes('aria-label="starting"'));
});

// The row is a placeholder for a session that does not exist yet, so every
// control that acts on a session id must be absent — a rename or remove
// bound to it would have nothing to send.
test("the pending row carries no session identity or controls", () => {
  const html = pendingSessionRowHtml({ projectID: "p1", provider: "claude" }, "p1");
  assert.ok(!html.includes("data-session="), "nothing can select it");
  assert.ok(!html.includes("data-rename-session"));
  assert.ok(!html.includes("data-remove-session"));
  assert.ok(!html.includes("<button"), "it is inert");
});

// The row's trailing edge is ONE control: the age tick,
// which hover fades out, plus the ellipsis trigger that fades in — never a
// row-level rename/close button, which collapses into the menu.
test("session actions carry one hover-revealed menu trigger beside the evidence tick", () => {
  const html = sessionActionControlsHtml("s<&", '<span aria-label="output streaming"></span>', false);
  assert.ok(html.includes("session-evidence"));
  assert.ok(html.includes('aria-label="output streaming"'));
  assert.ok(html.includes('data-session-menu="s&lt;&amp;"'));
  assert.ok(html.includes('aria-label="Session menu"'));
  assert.ok(html.includes('data-hint="Rename, recolor or close this session"'));
  assert.ok(html.includes('class="session-menu-trigger"'));
  assert.ok(html.includes("h-7 w-7"), "the trigger is --control-h-sm, not a 40px button");
  for (const gone of ["data-rename-session", "data-remove-session", "data-color-session"]) {
    assert.ok(!html.includes(gone), `${gone} has no row button anymore`);
  }
});

// The ENTIRE motion budget for row chrome is the 110ms opacity
// hover-reveal: the delayed delete controls and the evidence
// cell fade, and nothing else animates on the row. These three utilities are
// the source classes that produce `transition: opacity 110ms ease`; the real
// verification is the built CSS: `grep -o "110ms" dist/assets/*.css`.
test("the row hover-reveal is a 110ms opacity transition", () => {
  const html = sessionActionControlsHtml("s<&", '<span aria-label="output streaming"></span>', false);
  const evidence = html.match(/session-evidence[^"]*"[^"]*"/);
  assert.ok(evidence !== null, "the evidence cell carries its reveal classes");
  assert.match(evidence![0], /transition-opacity/);
  assert.match(evidence![0], /duration-\[110ms\]/);
  assert.match(evidence![0], /ease-\[ease\]/);
  assert.match(evidence![0], /group-hover:opacity-0/);
  // The tick fades for exactly what reveals the trigger over it: hover, and
  // keyboard focus on the row or on the trigger inside it. focus-within
  // would add a mouse click, which reveals nothing and would leave the
  // row's trailing edge empty.
  assert.match(evidence![0], /group-focus-visible:opacity-0/);
  assert.match(evidence![0], /group-has-focus-visible:opacity-0/);
  assert.ok(!evidence![0].includes("group-focus-within:"), "a mouse click does not fade the tick");
  assert.ok(!html.includes("transition-transform"), "no geometry motion on row controls");
});

// The tick is a fit-content cell flush
// with the row's content edge, sized with no line-height of its own (a
// normal line box), and coloured by the row's selected state: secondary on the selected row alone, tertiary everywhere
// else. Both selected states are asserted — checking one row would let
// the promotion go missing.
test("the age tick's line box, position and colour follow the row's selected state", () => {
  for (const selected of [false, true]) {
    const html = sessionActionControlsHtml("s-1", "<span>2m</span>", selected);
    const evidence = /class="session-evidence ([^"]*)"/.exec(html);
    assert.ok(evidence !== null, "the evidence cell renders with its classes");
    const classes = evidence[1]!;
    assert.ok(classes.includes("leading-[normal]"),
      `the tick needs a normal line box, not the micro 1.3 ratio: ${classes}`);
    assert.ok(!classes.includes("leading-normal"),
      `leading-normal is Tailwind's 1.5, a different line box: ${classes}`);
    assert.ok(classes.includes("flex-none"),
      `the tick is the row's last in-flow item, flush at the content edge: ${classes}`);
    // The 11px voices are all weight 500; a 400 tick sat a
    // step lighter than either sanctioned micro voice.
    assert.ok(classes.includes("font-medium"),
      `the tick carries the medium weight: ${classes}`);
    assert.ok(!classes.includes("font-mono"),
      `mono is the machine's voice; an elapsed time the app prints about itself is the app speaking: ${classes}`);
    assert.ok(classes.includes("text-micro"),
      `the tick stays at the micro size: ${classes}`);
    for (const displaced of ["-mr-[5px]", "min-w-6", "justify-center"]) {
      assert.ok(!classes.includes(displaced),
        `${displaced} walks the tick right of its x: ${classes}`);
    }
    const colour = selected ? "text-text-secondary" : "text-text-tertiary";
    const other = selected ? "text-text-tertiary" : "text-text-secondary";
    assert.ok(classes.includes(colour),
      `the ${selected ? "selected" : "unselected"} row's tick carries ${colour}: ${classes}`);
    assert.ok(!classes.includes(other),
      `only the selected row promotes its tick to secondary: ${classes}`);
  }
});

// The sidebar wordmark is the collapse control: a single
// button carrying the lockup, with an action name instead of the brand
// name so screen readers say what happens. The binding path reads
// [data-collapse], so losing that attribute breaks the handler.
test("the lockup is the collapse control", () => {
  const html = lockupImg();
  assert.ok(html.startsWith("<button"), "the lockup renders as a button");
  assert.ok(html.includes('data-collapse'), "the lockup button carries the collapse binding");
  // These strings are pinned verbatim, and four browser probes look the
  // button up by its accessible name - renaming it makes them wait on a
  // locator that can never resolve.
  assert.ok(html.includes('aria-label="Collapse sidebar"'), "the accessible name is pinned");
  assert.ok(html.includes('data-hint="Collapse the project sidebar"'), "the hint is pinned");
  assert.ok(html.includes('src="/brand/noisefloor-lockup-inverse.svg"'), "the artwork still renders");
  assert.ok(html.includes('alt=""'), "the artwork is decorative inside the named button");
});

// BOTH the Projects label and the Open control are uppercased. The authored
// copy stays sentence case in the markup, so the casing is presentational
// via text-transform; losing it changes the measured box and is a regression,
// so both are pinned alongside their size and line box classes.
// The unanswered rail: bands at row
// height, flat, with the eyebrow dimmed and no menu to open over a list
// nobody has read. A syncing header says the rows below are the last known
// list and carries no menu either: its rows act on a list nobody confirmed.
test("the loading bands are flat rows at the real row height", () => {
  const html = loadingBandsHtml();
  assert.equal((html.match(/data-loading-band/g) ?? []).length, 4, "four project bands");
  assert.equal((html.match(/h-\[31px\]/g) ?? []).length, 9, "every band is one row tall");
  assert.ok(!/animate-|shimmer|pulse/.test(html), "bands neither shimmer nor pulse");
  assert.ok(html.includes('aria-hidden="true"'), "bands say nothing to a reader");
  assert.ok(!html.includes("data-list-row"), "a band is not a row the keyboard can reach");
});

test("the dimmed header has no menu and the syncing header says so", () => {
  const dimmed = projectsHeaderHtml({ dimmed: true });
  assert.ok(dimmed.includes("Projects") && !dimmed.includes("data-projects-menu"), "no menu over an unread list");
  assert.ok(dimmed.includes("opacity-60"));
  const syncing = projectsHeaderHtml({ syncing: true });
  assert.ok(!syncing.includes("data-projects-menu"), "no menu over a stale list either: its rows act on a list nobody confirmed");
  assert.ok(syncing.includes('data-list-syncing role="status"') && syncing.includes("Syncing"));
  assert.ok(syncing.includes("animate-spinner"));
  assert.ok(!projectsHeaderHtml().includes("data-list-syncing"), "a served list does not say syncing");
});

test("the projects header is a menubar-style menu button, with no standalone Open control", () => {
  const html = projectsHeaderHtml();
  const trigger = /<button data-projects-menu aria-haspopup="menu" aria-expanded="false"[\s\S]*?class="([^"]*)"/.exec(html);
  assert.ok(trigger !== null, html);
  const classes = trigger[1]!;
  for (const cls of ["text-micro", "font-medium", "uppercase", "leading-[1.55]", "tracking-(--type-micro-ls)", "text-text-tertiary", "px-2", "py-[3px]"]) {
    assert.ok(classes.includes(cls), `${cls} missing from Projects trigger: ${classes}`);
  }
  assert.ok(!html.includes("Open…"), "the standalone Open… control must not render");
  assert.ok(!html.includes("data-new-project"), "the header carries no standalone new-project button");
});

test("the projects trigger hovers in colour only and takes the pressed wash only while its menu is open", () => {
  const html = projectsHeaderHtml();
  const trigger = /<button data-projects-menu[\s\S]*?class="([^"]*)"/.exec(html);
  assert.ok(trigger !== null, html);
  const classes = trigger[1]!;
  assert.ok(classes.includes("hover:text-text-primary"), "hover promotes the colour to primary");
  assert.ok(!classes.includes("hover:bg-wash-hover"), "hover is colour only — no wash");
  assert.ok(classes.includes("aria-expanded:bg-wash-pressed"), "the pressed wash is gated on the open menu");
  assert.ok(!/(^|\s)bg-wash-pressed(\s|")/.test(classes), "no unconditional pressed wash");
});

test("sort by name rewrites the manual order once and leaves it hand-editable", () => {
  const group = (id: string, name: string): ProjectWithSessions => ({
    project: { id, name, canonical_path: `/srv/${id}`, last_used_at: "t" },
    sessions: [],
  });
  const groups = [group("p1", "zeta"), group("p2", "Echo"), group("p3", "alpha")];
  // Your manual order is not alphabetical.
  const manual = ["p1", "p3", "p2"];
  const sorted = sortedProjectOrderByName(groups);
  assert.notDeepEqual(sorted, manual, "the rewrite changes the order");
  // Case-insensitive by displayed name: alpha, Echo, zeta.
  assert.deepEqual(sorted, ["p3", "p2", "p1"]);
  // Not a mode: the result is an ordinary manual order, and a subsequent
  // manual move still takes.
  const moved = moveProjectRelative(sorted, "p1", "p3", false);
  assert.deepEqual(moved, ["p1", "p3", "p2"]);
  assert.deepEqual(presentedProjectOrder(groups, moved), ["p1", "p3", "p2"]);
});

test("an import sorts where the project it becomes will sort", () => {
  const rows = [{ id: "p1", name: "zeta" }, { id: "p2", name: "alpha" }, { id: "p3", name: "mike" }];
  const imported = { id: "clone-1", name: "kilo" };
  const ids = (order: readonly string[] | null, row: { id: string; name: string }) =>
    presentedRowOrder([...rows, row], order).map((r) => r.id);
  assert.deepEqual(ids(null, imported), ["p2", "clone-1", "p3", "p1"], "alphabetical among the projects");
  assert.deepEqual(ids(null, { id: "p4", name: "kilo" }), ids(null, imported).map((id) => id === "clone-1" ? "p4" : id), "the project lands in the same slot");
  const custom = ["p3", "p1", "p2"];
  assert.deepEqual(ids(custom, imported), ["p3", "p1", "p2", "clone-1"], "after a custom order");
  assert.deepEqual(ids(custom, { id: "p4", name: "kilo" }), ["p3", "p1", "p2", "p4"], "and the project lands there too");
});

test("the header menu's new-session target is the selected project, else the first presented", () => {
  const group = (id: string, name: string): ProjectWithSessions => ({
    project: { id, name, canonical_path: `/srv/${id}`, last_used_at: "t" },
    sessions: [],
  });
  const groups = [group("p1", "zeta"), group("p2", "alpha")];
  const order = presentedProjectOrder(groups, null);
  assert.deepEqual(newSessionTargetProject(groups, order, "p1"), { id: "p1", name: "zeta" });
  assert.deepEqual(newSessionTargetProject(groups, order, null), { id: "p2", name: "alpha" });
  assert.deepEqual(newSessionTargetProject(groups, order, "gone"), { id: "p2", name: "alpha" });
  assert.equal(newSessionTargetProject([], [], null), null);
});

// The sessions-list rail is the same per-project inline
// pattern as the disclosure chevron: a set colour tints it, unset leaves
// the .session-rail rule's --border fallback.
test("the sessions rail tints only a set project colour", () => {
  for (const unset of [null, undefined, ""]) {
    assert.equal(sessionRailStyle(unset), "", "an unset colour leaves the border fallback");
  }
  assert.match(sessionRailStyle("#C8563C"), /style="border-left-color:#C8563C"/);
});

// The rail's left inset is hand-tuned: 11px margin
// plus the 2px border plus 8px padding lands the tile under the project name.
// Rounding the margin to the 4px ramp shifts every row 2px right and narrows
// it by the same amount, so the classes are pinned verbatim.
test("the sessions rail carries its hand-tuned left inset", () => {
  const html = sessionRailContainerHtml("p1", null, true, "<li>child</li>");
  assert.ok(html.includes('class="session-rail mb-1.5 ml-[11px] mt-0.5 flex flex-col gap-px pl-2"'),
    "rail uses 11px margin and 8px padding");
  assert.ok(html.includes('id="sessions-p1"'), "rail is keyed to the project");
  // The rail wraps exactly its children: the empty-rail row (rendered by
  // the caller for a sessionless group) is the only in-group creation
  // affordance, so the rail itself appends no phone chrome.
  assert.ok(html.includes("<li>child</li></ul>"),
    "rail wraps its children and closes with no trailing chrome row");
  assert.ok(!html.includes("data-sheet-group-new-session"),
    "the retired phone in-group row never renders");

  const collapsed = sessionRailContainerHtml("p2", null, false, "");
  assert.ok(collapsed.includes("hidden"), "a collapsed rail is hidden");

  const tinted = sessionRailContainerHtml("p3", "#C8563C", true, "");
  assert.ok(tinted.includes('style="border-left-color:#C8563C"'), "a set colour tints the rail");
});

// The point of the row is that it occupies the geometry the real row will,
// so you see no reflow when the placeholder is replaced. These
// are the classes that decide that: row padding and gap, the tile's box,
// and the trailing spinner cell matching the evidence cell's fit-content
// flush-right box. A measured 0.00px parity is only as durable as the
// classes that produced it.
test("the pending row carries the real row's geometry", () => {
  const html = pendingSessionRowHtml({ projectID: "p1", provider: "claude" }, "p1");
  // The row interior reads the rail values as literals (px-2 py-1.5
  // gap-2 = 8/6/8px), matching the merged rows.
  for (const cls of ["gap-2", "px-2", "py-1.5", "rounded-control", "h-4", "w-4", "flex-none"]) {
    assert.ok(html.includes(cls), `${cls} is load-bearing for row parity: ${html}`);
  }
  for (const displaced of ["-mr-[5px]", "min-w-6", "justify-center"]) {
    assert.ok(!html.includes(displaced),
      `${displaced} seats the spinner off the evidence cell's edge and the real row jumps sideways on replace: ${html}`);
  }
});

// The placeholder swaps for the real row in one frame, so a tile drawing
// something different is a visible jump at the moment you are
// looking straight at it. Both rows are drawn by the SHARED sessionTileHtml,
// so the relation holds by construction rather than by coincidence.
test("the pending row's tile IS the real row's tile", () => {
  for (const provider of ["claude", "shell"]) {
    const pending = pendingSessionRowHtml({ projectID: "p1", provider }, "p1");
    assert.ok(pending.includes(sessionTileHtml(provider)),
      `the pending tile must be the shared tile for ${provider}: ${pending}`);
    assert.ok(pending.includes(providerTileClass(provider)),
      `the shared tile still carries the row tile's hue for ${provider}`);
  }
});

// The tile is 16px on the --radius-mark radius and carries colour; nothing
// renders inside it (tiles carry colour, never letters). sessionColorStyle
// drives the session-colour override, so a session-tagged tile swaps the
// background for the session hue.
test("the session tile is a 16px radius-mark square with no glyph", () => {
  const plain = sessionTileHtml("codex");
  assert.ok(plain.includes("h-4 w-4"), "the tile box is the fixed 16px slot");
  assert.ok(plain.includes("rounded-mark"), "the tile uses the --radius-mark radius");
  assert.ok(!plain.includes("<svg"), "no icon renders inside the tile");
  assert.ok(!plain.includes("font-mono"), "no glyph font on a content-free tile");
  assert.ok(!/\bgrid\b/.test(plain), "no inert grid utility on a content-free tile");
  assert.match(plain, /<\/span>$/, "the tile carries no text node");

  const tagged = sessionTileHtml("codex", 'style="background:var(--color-amber-600)"');
  assert.ok(tagged.includes('background:var(--color-amber-600)'), "a session colour overrides the background");
});

// The provider reaches this from a data attribute, and the row is built by
// string concatenation, so it is escaped like every other server value.
test("the pending row escapes the provider it is given", () => {
  const html = pendingSessionRowHtml({ projectID: "p1", provider: '"><img src=x onerror=alert(1)>' }, "p1");
  assert.ok(!html.includes("<img"), "no tag survives");
  assert.ok(html.includes("&lt;img"), "it renders as visible text instead");
  // The provider also lands in an attribute value, so the quote that would
  // close it early must be encoded there too.
  const attr = /data-pending-session="([^"]*)"/.exec(html);
  assert.ok(attr !== null, html);
  assert.ok(!attr[1]!.includes("<"), "no raw angle bracket in the attribute");
  assert.ok(attr[1]!.includes("&quot;"), "the quote is encoded, not terminating");
});

// The colour strip moved into the session ⋯ menu, so the row
// helper no longer owns a picker; session-menu.test.ts pins the strip's
// swatches and selection state instead.

// The disclosure column is the project row's fixed lead slot:
// 13px mono, the reference's ▸/▾, and the project color only when one
// is set — otherwise the secondary text voice, never a guess at a hue.
test("the project disclosure chevron tints only a set project color", () => {
  const collapsed = projectChevronHtml(false, null);
  assert.ok(collapsed.includes("▸"), "collapsed projects disclose to the right");
  assert.ok(!collapsed.includes("▾"));
  assert.ok(collapsed.includes("text-text-secondary"), "no color falls back to the secondary voice");
  assert.ok(!collapsed.includes("style="), "the fallback carries no inline tint");

  const expanded = projectChevronHtml(true, "#C8563C");
  assert.ok(expanded.includes("▾"), "expanded projects disclose downward");
  assert.ok(expanded.includes("w-[13px]"), "the column is 13px wide");
  assert.ok(expanded.includes("style=\"color:#C8563C\""), "a set color tints the chevron");
});

// The menu reorder's pure core places the moved project before or after the
// named row. An unknown target appends.
test("project menu reorder places the moved row relative to the target", () => {
  assert.deepEqual(moveProjectRelative(["a", "b", "c"], "c", "a", false), ["c", "a", "b"]);
  assert.deepEqual(moveProjectRelative(["a", "b", "c", "d"], "d", "b", false), ["a", "d", "b", "c"]);
  assert.deepEqual(moveProjectRelative(["a", "b", "c"], "b", "b", false), ["a", "c", "b"]);
  assert.deepEqual(moveProjectRelative(["a", "b", "c"], "c", "a", true), ["a", "c", "b"]);
  assert.deepEqual(moveProjectRelative(["a", "b", "c"], "a", "c", true), ["b", "c", "a"]);
  assert.deepEqual(moveProjectRelative(["a", "b", "c"], "b", "missing", false), ["a", "c", "b"]);
  assert.deepEqual(moveProjectRelative(["a", "b", "c"], "b", "missing", true), ["a", "c", "b"]);
});

test("project menu moves swap one neighbour and no-op at the sidebar boundaries", () => {
  assert.deepEqual(moveProjectOneStep(["a", "b", "c"], "b", "up"), ["b", "a", "c"]);
  assert.deepEqual(moveProjectOneStep(["a", "b", "c"], "b", "down"), ["a", "c", "b"]);
  // The long move: everything above shifts down one, nothing below moves.
  assert.deepEqual(moveProjectToTop(["a", "b", "c", "d"], "c"), ["c", "a", "b", "d"]);
  assert.deepEqual(moveProjectToTop(["a", "b", "c"], "a"), ["a", "b", "c"], "first stays first");
  assert.deepEqual(moveProjectToTop(["a", "b", "c"], "zz"), ["a", "b", "c"], "an unknown id changes nothing");
  const source = ["a", "b", "c"];
  moveProjectToTop(source, "c");
  assert.deepEqual(source, ["a", "b", "c"], "the input is not reordered in place");
  assert.deepEqual(moveProjectOneStep(["a", "b", "c"], "a", "up"), ["a", "b", "c"]);
  assert.deepEqual(moveProjectOneStep(["a", "b", "c"], "c", "down"), ["a", "b", "c"]);
});

// The session drag reorder uses the same relative placement as the project
// menu move: remove the dragged id and insert it before or after the target.
test("session drag reorder relative to the target opens a gap before or after", () => {
  assert.deepEqual(moveSessionRelative(["s1", "s2", "s3"], "s3", "s1", false), ["s3", "s1", "s2"]);
  assert.deepEqual(moveSessionRelative(["s1", "s2", "s3"], "s1", "s3", true), ["s2", "s3", "s1"]);
  assert.deepEqual(moveSessionRelative(["s1", "s2", "s3"], "s2", "missing", false), ["s1", "s3", "s2"]);
});

// rowDropSlot resolves a drag pointer against the dragstart geometry, the
// only coordinates that stay still while the drop gap's own margin shifts
// the live rows. The source row's box and the space below the last row
// offer no slot; the 2px seams between boxes resolve to the row below.
test("rowDropSlot names the target row and side from the dragstart geometry", () => {
  const boxes = [
    { id: "s1", top: 0, bottom: 30 },
    { id: "s2", top: 32, bottom: 62 },
    { id: "s3", top: 64, bottom: 94 },
  ];
  assert.deepEqual(rowDropSlot(boxes, 40, "s1"), { id: "s2", after: false }, "top half of s2 opens the gap before it");
  assert.deepEqual(rowDropSlot(boxes, 50, "s1"), { id: "s2", after: true }, "bottom half of s2 opens the gap after it");
  assert.deepEqual(rowDropSlot(boxes, 68, "s1"), { id: "s3", after: false }, "top half of s3 opens the gap before it");
  assert.deepEqual(rowDropSlot(boxes, 80, "s1"), { id: "s3", after: true }, "bottom half of s3 opens the gap after it");
  assert.deepEqual(rowDropSlot(boxes, 31, "s1"), { id: "s2", after: false }, "the seam between boxes belongs to the row below");
  assert.deepEqual(rowDropSlot(boxes, 5, "s3"), { id: "s1", after: false }, "above every other row targets the first row");
  assert.equal(rowDropSlot(boxes, 15, "s1"), null, "the source row's own box offers no slot");
  assert.equal(rowDropSlot(boxes, 50, "s2"), null, "the source row's box offers no slot wherever it lays out");
  assert.equal(rowDropSlot(boxes, 120, "s1"), null, "below the last row offers no slot");
  const shrunk = [
    { id: "s2", top: 32, bottom: 62 },
    { id: "s3", top: 64, bottom: 94 },
  ];
  assert.equal(rowDropSlot(shrunk, 40, "s1"), null, "a source missing from the rows offers no slot — the drag is over");
});

// A saved session order is applied on top of the server's order. Sessions the
// saved order does not name sort after the named ones in their server order,
// so a new session lands predictably instead of jumping to the top.
test("presented session order appends unknown sessions in server order", () => {
  const sessions = [
    { id: "s1" },
    { id: "s2" },
    { id: "s3" },
    { id: "s4" },
  ] as never[];
  assert.deepEqual(presentedSessionOrder(sessions, null), ["s1", "s2", "s3", "s4"]);
  assert.deepEqual(presentedSessionOrder(sessions, ["s3", "s1"]), ["s3", "s1", "s2", "s4"]);
  assert.deepEqual(presentedSessionOrder(sessions, ["s4", "s2", "s1", "s3"]), ["s4", "s2", "s1", "s3"]);
});

// The menu move seeds from the presented list, not the model's server
// order. With a non-alphabetical server order [charlie, alpha, bravo] the
// rows render [alpha, bravo, charlie], so a move must place a project
// relative to that VISIBLE sequence. Seeding from the model order instead
// drags alpha along: moving bravo before charlie commits [bravo, charlie,
// alpha] even though you only touched one visible neighbor.
test("a menu move reorders the visible neighbors of the presented list, not the model order", () => {
  const groups = [
    { project: { id: "charlie", name: "charlie", canonical_path: "/srv/charlie", last_used_at: "t" }, sessions: [] },
    { project: { id: "alpha", name: "alpha", canonical_path: "/srv/alpha", last_used_at: "t" }, sessions: [] },
    { project: { id: "bravo", name: "bravo", canonical_path: "/srv/bravo", last_used_at: "t" }, sessions: [] },
  ];
  const seed = presentedProjectOrder(groups, null);
  assert.deepEqual(seed, ["alpha", "bravo", "charlie"]);

  const committed = moveProjectRelative(seed, "bravo", "charlie", false);
  assert.deepEqual(committed, ["alpha", "bravo", "charlie"]);
  assert.deepEqual(presentedProjectOrder(groups, committed), ["alpha", "bravo", "charlie"]);
});


test("the project row renders the permanence rules: actions visible expanded, 28px, plus always present", () => {
  const row = projectRowHtml({ id: "solo", name: "solo", expanded: true, hint: "Hide the sessions in solo", chev: "", actions: "opacity-100" });
  assert.ok(row.includes("opacity-100"), "the expanded actions span carries the visible class");
  assert.ok(row.includes("h-7 w-7"), "row action controls are 28px");
  assert.ok(row.includes("data-new-session"), "the plus control renders even with zero sessions");
  assert.ok(!row.includes("sessions.length"), "presence never depends on session count");
  const collapsed = projectRowHtml({ id: "solo", name: "solo", expanded: false, hint: "Show the sessions in solo", chev: "", actions: "pointer-events-none opacity-0" });
  assert.ok(collapsed.includes("opacity-0"), "collapsed actions reveal on hover/focus only");
  // Two 28px controls + 1px gap + 3px inset occupy 60px; the label padding
  // reservation clears it with room to breathe.
  assert.ok(row.includes("pr-[64px]"), "the desktop label reservation clears the 60px action cluster");
});

test("the project row's ⋯ declares the same menu semantics the session row's does", () => {
  const row = projectRowHtml({ id: "solo", name: "solo", expanded: true, hint: "Hide the sessions in solo", chev: "", actions: "opacity-100" });
  assert.match(row, /data-project-menu="solo"[^>]*aria-haspopup="menu"[^>]*aria-expanded="false"/,
    "the trigger states its menu role and resting state for the mount path to flip");
});

test("a selected project row holds the hover wash and states it for probes", () => {
  const args = { id: "solo", name: "solo", expanded: true, hint: "Hide the sessions in solo", chev: "", actions: "opacity-100" };
  const selected = projectRowHtml({ ...args, selected: true });
  assert.ok(selected.includes('data-selected="true"'));
  assert.ok(selected.includes("bg-wash-hover"), "selection holds the same wash hover uses");
  const resting = projectRowHtml(args);
  assert.ok(resting.includes('data-selected="false"'));
  assert.ok(resting.includes("hover:bg-wash-hover"), "an unselected row washes on hover only");
});

test("the project row uses an action-named menu hint and carries no nested data-hints", () => {
  const row = projectRowHtml({ id: "solo", name: "Solo Project", expanded: true, hint: "Hide the sessions in Solo Project", chev: "", actions: "opacity-100" });
  assert.ok(row.includes('data-hint="Rename, recolor or remove Solo Project"'),
    "the project menu trigger names the actions and the project");
  assert.ok(!row.includes('data-project-name data-hint='),
    "the project name span does not shadow the row hint");
  assertNoNestedDataHints(row, "project row");
});

const cloneJob = (over: Partial<CloneJob> = {}): CloneJob => ({
  id: "clone-1", url: "https://github.com/fable/deck", destination: "/workspace/deck", name: "deck",
  status: "running", percent: 42, partial_bytes: 0, started_at: "t", updated_at: "t", ...over,
});

test("an import row is the collapsed project row it becomes, with progress in place of the actions", () => {
  const row = cloneRowHtml(cloneJob());
  const project = projectRowHtml({ id: "deck", name: "deck", expanded: false, hint: "", chev: projectChevronHtml(false, null), actions: "" });
  const buttonClass = (html: string, attr: string) => new RegExp(`<button[^>]*${attr}[^>]*class="([^"]*)"`).exec(html)?.[1];
  assert.equal(buttonClass(row, "data-clone-row"), buttonClass(project, "data-project-toggle"), "same height, padding and wash as the project toggle");
  assert.ok(row.includes(projectChevronHtml(false, null)), "the collapsed chevron holds the disclosure column");
  assert.ok(row.includes('class="truncate text-body font-medium leading-[1.3] text-text-primary">deck</span>'), "the name reads as a project name");
  assert.match(row, /data-clone-row="clone-1" data-list-row role="option" tabindex="-1"/, "a roving list row like the project toggle");
  assert.ok(!row.includes("aria-expanded"), "nothing to expand");
  assert.ok(!row.includes("data-new-session") && !row.includes("data-project-menu"), "no actions before the project exists");
  assert.ok(row.includes('data-hint="Importing github.com/fable/deck — 42%"'));
  assert.ok(row.includes('<span class="sr-only">Importing, 42%</span>'), "the percent is readable text");
  assert.ok(row.includes('stroke-dasharray="42 100"'), "the ring is determinate");
  assert.match(row, /data-clone-progress class="[^"]*h-7 w-7[^"]*pointer-coarse:h-10 pointer-coarse:w-10"/, "the slot is the row's 28px control size");
  assert.ok(!/opacity-0|group-hover/.test(row), "progress is always visible");
  assert.ok(!row.includes("h-[3px]") && !row.includes("importing</span>"), "no bar or caption under the name");
});

test("a stopped import keeps the row and swaps the ring for a warning", () => {
  const row = cloneRowHtml(cloneJob({ status: "stopped", percent: 51 }));
  assert.ok(row.includes('data-hint="Import stopped part-way — click to resume"'));
  assert.ok(row.includes("text-warning"), "the slot carries the warning glyph");
  assert.ok(!row.includes("data-clone-ring"), "no ring once the import has stopped");
  assert.ok(cloneRowHtml(cloneJob({ status: "failed" })).includes("text-warning"), "a failed import reads the same");
});

test("a completed import awaiting its project reads complete, not stopped", () => {
  const row = cloneRowHtml(cloneJob({ status: "done", percent: 100, project_id: "proj-7" }));
  assert.ok(row.includes('data-hint="Import complete — refreshing projects"'));
  assert.ok(row.includes('<span class="sr-only">Import complete</span>'));
  assert.ok(row.includes("text-accent"), "completion uses the accent check, not a warning");
  assert.ok(!row.includes("text-warning"));
  assert.ok(!row.includes("Import stopped part-way"));
});

test("only a newly completed import waits for its project, and removal cannot resurrect it", () => {
  const job = cloneJob({ status: "done", percent: 100, project_id: "proj-7" });
  const awaiting = new Set<string>();
  const absent: { project: { id: string } }[] = [];
  const present = [{ project: { id: "proj-7" } }];
  assert.equal(cloneJobStaysListed(job, awaiting, absent), false,
    "a fresh sidebar does not resurrect a retained completed job whose project is absent");
  awaiting.add(job.id);
  assert.equal(cloneJobStaysListed(job, awaiting, absent), true,
    "a completion observed by this sidebar waits for its project");
  assert.equal(cloneJobStaysListed(job, awaiting, present), false,
    "the project replaces the import");
  assert.equal(awaiting.has(job.id), false, "the first project observation settles the completion");
  assert.equal(cloneJobStaysListed(job, awaiting, absent), false,
    "removing that project does not bring the completed import back");
});

test("the import ring clamps and rounds the server's percent", () => {
  assert.ok(cloneRowHtml(cloneJob({ percent: 41.6 })).includes('stroke-dasharray="42 100"'));
  assert.ok(cloneRowHtml(cloneJob({ percent: 140 })).includes('stroke-dasharray="100 100"'));
  assert.ok(cloneRowHtml(cloneJob({ percent: -3 })).includes('stroke-dasharray="0 100"'));
});

test("the session row uses an action-named menu hint and carries no nested data-hints", () => {
  const controls = sessionActionControlsHtml("s-1", "<span>2m</span>", false);
  assert.ok(controls.includes('data-hint="Rename, recolor or close this session"'),
    "the session menu trigger names the actions");
  assertNoNestedDataHints(controls, "session action controls");
});

// An expanded project with no sessions shows an empty rail, and the + on the
// project row is invisible until the row is hovered - so without this row
// there is no visible way to start the first session. It is scoped to empty
// projects.
test("an expanded project with no sessions offers a way to start one", () => {
  const g = { sessions: [], project: { id: "solo", name: "solo" } };
  const row = emptyRailNewSessionHtml(g, true, null);
  assert.ok(row.includes("data-new-session"), "the row is a new-session control");
  assert.ok(row.includes("New session"), "the row is labelled");
  assert.ok(row.includes("Start a new session in solo"), "the hint names the project");

  assert.equal(emptyRailNewSessionHtml(g, false, null), "", "a collapsed project shows nothing");
  assert.equal(
    emptyRailNewSessionHtml({ sessions: [{}], project: { id: "solo", name: "solo" } }, true, null),
    "",
    "a project with sessions shows its sessions, not this row",
  );
  assert.equal(
    emptyRailNewSessionHtml(g, true, { projectID: "solo", provider: "claude" } as never),
    "",
    "a pending session already occupies the rail",
  );
});

// The three-verb rule is copy law: Close ends a process,
// Remove takes a project off the sidebar, and Delete appears nowhere in
// this UI. "remove session"/"delete session"/"delete project" in shipped
// copy would reopen the confusion the rule closed — an aria-label that
// says "Remove session" once shipped and read as data loss. The phrases
// are checked mechanically across web/src because the wording has decayed
// before; review alone did not hold it. Test files are exempt: they quote
// server error fixtures and assert the absence of these very phrases.
test("no shipped copy says remove/delete session or delete project", () => {
  const banned = ["delete session", "remove session", "delete project"];
  const root = new URL("../../", import.meta.url);
  const files: URL[] = [];
  // Directory URLs must keep their trailing slash or children resolve one
  // segment up (api/client.ts would read as src/client.ts and vanish).
  const walk = (dir: URL): void => {
    for (const entry of readdirSync(dir, { withFileTypes: true })) {
      const child = new URL(`${entry.name}${entry.isDirectory() ? "/" : ""}`, dir);
      if (entry.isDirectory()) walk(child);
      else if (entry.name.endsWith(".ts") && !entry.name.endsWith(".test.ts")) files.push(child);
    }
  };
  walk(root);
  for (const file of files) {
    const text = readFileSync(file, "utf8").toLowerCase();
    for (const phrase of banned) {
      assert.ok(!text.includes(phrase), `${file.pathname}: contains "${phrase}"`);
    }
  }
});

test("the New session dialog names the stored launch project, or nothing", () => {
  const groups = [
    { project: { id: "p1", name: "field-notes" } },
    { project: { id: "p2", name: "audio-player" } },
  ];
  // The clicked row's id wins, not a context-else-first resolution: those
  // agree only when the row clicked happens to be the context project.
  assert.equal(newSessionDialogTarget(groups, "p2")?.name, "audio-player");
  assert.equal(newSessionDialogTarget(groups, "p1")?.name, "field-notes");
  // A stored id that no longer resolves names nothing. Substituting another
  // project here would tell you the session lands somewhere it does
  // not, which is worse than an undescribed dialog.
  assert.equal(newSessionDialogTarget(groups, "gone"), null);
  assert.equal(newSessionDialogTarget([], "p1"), null);
  assert.equal(newSessionDialogTarget(groups, null), null);
});

// Filtering is presentation over the list it is handed, in the order it was
// handed: the ranking (harness-ranking.ts) decides that order before this
// sees it, so a filter that sorted would be a second opinion about it.
test("filtering keeps the complete inventory in the order it was given", () => {
  assert.deepEqual(newSessionVisibleProviders(FULL, "", null), FULL);
  assert.deepEqual(newSessionVisibleProviders(FULL, "command", null), ["command-code"]);
  assert.deepEqual(newSessionVisibleProviders(FULL, "kilo", null), ["kilo"]);
  assert.deepEqual(newSessionVisibleProviders(FULL, "no such provider", null), []);
  assert.deepEqual(newSessionVisibleProviders(FULL, "kilo", "pi"), ["kilo", "pi"]);
  const ranked = ["kilo", ...FULL.filter((provider) => provider !== "kilo")];
  assert.deepEqual(newSessionVisibleProviders(ranked, "", null), ranked);
});

// A harness whose label differs from its key has to answer to both: the
// executable is command-code and the session is called "command", and a
// user who knows either name types it.
test("the filter matches the provider key as well as the display name", () => {
  assert.deepEqual(newSessionVisibleProviders(FULL, "command-code", null), ["command-code"]);
  assert.deepEqual(newSessionVisibleProviders(FULL, "shell", null), ["shell"], "the key");
  assert.deepEqual(newSessionVisibleProviders(FULL, "terminal", null), ["shell"], "the label");
});

test("desktop and phone offer every harness once without recommendations", () => {
  for (const phone of [false, true]) {
    const html = newSessionBodyHtml({available: FULL, filter: "", launching: null, creating: false, selection: null, phone});
    const providers = [...html.matchAll(/data-new-session-provider="([^"]+)"/g)].map((match) => match[1]);
    assert.deepEqual(providers, FULL);
    assert.ok(!html.includes("data-new-session-recommendation"));
    assert.ok(!html.includes("Recent in this project"));
    assert.ok(html.includes('placeholder="Filter 16 harnesses"'));
    assertNoNestedDataHints(html, "new session picker");
  }
});

test("the initial inventory wait is loading, not an error", () => {
  const html = newSessionBodyHtml({available: [], filter: "", launching: null, creating: false, selection: null, inventoryStatus: "unknown"});
  assert.ok(html.includes('role="status"'));
  assert.ok(html.includes("Loading harnesses…"));
  assert.ok(!html.includes("data-new-session-inventory-retry"));
  assert.ok(!html.includes("No harnesses are available"));
});
