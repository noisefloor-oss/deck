// Idle-render tests: render() rebuilds a markup string that totally
// describes the container, and must paint only when that string differs
// from the last one it wrote. These tests pin the skip from the outside —
// by counting container writes and observing bound listeners — because the
// point is what reaches the DOM, not that a comparison happened. The full
// Sidebar class needs a real browser for layout, but the skip decision
// needs only a container whose innerHTML writes are observable, so these
// run under `node --test` with the same kind of minimal fake DOM the
// mount-geometry tests use.
import assert from "node:assert/strict";
import test from "node:test";

import { Sidebar } from "./sidebar-dom.ts";
import { sessionMetaHtml, sessionRowMetaHtml } from "./sidebar.ts";
import type { ProjectWithSessions, SidebarModel } from "./sidebar.ts";
import type { ApiClient } from "../../api/contracts.ts";
import type { SessionSummary } from "../../api/sessions.ts";
import type { SidebarLayout } from "../../api/settings.ts";

interface FakeElement {
  tag: string;
  className: string;
  dataset: Record<string, string>;
  hidden: boolean;
  isConnected: boolean;
  innerHTML: string;
  getAttribute(name: string): string | null;
  setAttribute(name: string, value: string): void;
  removeAttribute(name: string): void;
  closest(selector: string): FakeElement | null;
  querySelector(selector: string): FakeElement | null;
  querySelectorAll(selector: string): FakeElement[];
  addEventListener(type: string, fn: (ev: unknown) => void): void;
  removeEventListener(type: string, fn: (ev: unknown) => void): void;
  dispatchEvent(ev: { type: string; [key: string]: unknown }): void;
  focus(): void;
  append(child: FakeElement): void;
  remove(): void;
}

function fakeElement(overrides: Partial<FakeElement> = {}): FakeElement {
  const listeners = new Map<string, ((ev: unknown) => void)[]>();
  const attrs: Record<string, string> = {};
  const self: FakeElement = {
    tag: "div",
    className: "",
    dataset: {},
    hidden: false,
    isConnected: true,
    innerHTML: "",
    getAttribute(name) {
      return attrs[name] ?? null;
    },
    setAttribute(name, value) {
      attrs[name] = value;
    },
    removeAttribute(name) {
      delete attrs[name];
    },
    closest() {
      return null;
    },
    querySelector() {
      return null;
    },
    querySelectorAll() {
      return [];
    },
    addEventListener(type, fn) {
      const list = listeners.get(type) ?? [];
      list.push(fn);
      listeners.set(type, list);
    },
    removeEventListener(type, fn) {
      const list = listeners.get(type);
      if (list === undefined) return;
      const index = list.indexOf(fn);
      if (index !== -1) list.splice(index, 1);
    },
    dispatchEvent(ev) {
      // The DOM snapshots a node's listener list when dispatch begins, so
      // a handler that re-binds mid-dispatch never runs its own additions
      // in that dispatch; iterating the live array would amplify each
      // re-bind into an infinite loop on this shared fake node.
      for (const fn of [...(listeners.get(ev.type) ?? [])]) fn(ev);
    },
    focus() {},
    append() {},
    remove() {},
    ...overrides,
  };
  return self;
}

interface Harness {
  sidebar: Sidebar;
  container: FakeElement & { writes: number; html: string };
  list: FakeElement & { html: string };
  metaSpan: FakeElement;
  toggle: FakeElement;
  group: ProjectWithSessions;
}

// A session with no last_output_at renders a deterministically empty meta
// cell, so string comparisons never straddle an age-bucket rollover.
function runningSession(): SessionSummary {
  return {
    id: "s1",
    project_id: "p1",
    provider: "claude",
    display_name: "",
    status: "running",
    generation: 1,
    created_at: "t",
    updated_at: "t",
    recovery_capability: "none",
    recovery_state: "unavailable",
    presentation_state: "running",
  };
}

function makeHarness(notified?: () => void): Harness {
  const group: ProjectWithSessions = {
    project: { id: "p1", name: "alpha", canonical_path: "/srv/alpha", last_used_at: "t" },
    sessions: [runningSession()],
  };

  const metaSpan = fakeElement({ tag: "span" });
  const toggle = fakeElement({ tag: "button" });
  toggle.setAttribute("data-project-toggle", "p1");
  const list = fakeElement({ tag: "ul" });
  const listWithHtml = list as FakeElement & { html: string };
  Object.defineProperty(list, "innerHTML", {
    get: () => listWithHtml.html,
    set: (value: string) => {
      listWithHtml.html = value;
    },
  });

  const projectLi = fakeElement({ tag: "li" });
  const modalsRegion = fakeElement({ tag: "div" });
  const workspace = fakeElement({
    tag: "div",
    querySelector(selector: string) {
      return selector === '[data-region="modals"]' ? modalsRegion : null;
    },
  });

  const container = fakeElement({
    tag: "aside",
    closest(selector: string) {
      return selector === "[data-workspace]" ? workspace : null;
    },
    querySelector(selector: string) {
      if (selector === "[data-collapse]") return fakeElement({ tag: "button" });
      if (selector === "[data-project-list]") return list;
      if (selector === "[data-project=\"p1\"]") return projectLi;
      if (selector === "[data-project-toggle=\"p1\"]") return toggle;
      if (selector === "[data-session-meta=\"s1\"]") return metaSpan;
      return null;
    },
    querySelectorAll(selector: string) {
      return selector === "[data-project-toggle]" ? [toggle] : [];
    },
  }) as FakeElement & { writes: number; html: string };
  let writes = 0;
  Object.defineProperty(container, "innerHTML", {
    get: () => container.html,
    set: (value: string) => {
      writes += 1;
      container.html = value;
    },
  });
  Object.defineProperty(container, "writes", { get: () => writes });

  const sidebarModel = {
    groups: [group],
    selectedSessionID: null,
    error: null,
    registryReadOk: true,
    listState: "ready",
    stale: false,
    gitIdentities: null,
    creatingSession: false,
    api: {
      getLayout: () => Promise.resolve(null),
      saveLayout: (next: SidebarLayout) => Promise.resolve(next),
    } satisfies Partial<ApiClient>,
    applyProjectColors() {},
    select() {
      return true;
    },
    rename() {
      return Promise.resolve(true);
    },
    renameProject() {
      return Promise.resolve(true);
    },
  };

  const sidebar = new Sidebar(
    container as unknown as HTMLElement,
    sidebarModel as unknown as SidebarModel,
    notified,
  );

  return { sidebar, container, list: listWithHtml, metaSpan, toggle, group };
}

function withEnv(run: (harness: Harness) => void, notified?: () => void): void {
  const originalWindow = (globalThis as unknown as { window?: unknown }).window;
  const originalDocument = (globalThis as unknown as { document?: unknown }).document;
  const originalCSS = (globalThis as unknown as { CSS?: unknown }).CSS;

  (globalThis as unknown as { window: unknown }).window = {
    matchMedia: () => ({ matches: false }),
    setInterval: () => 0,
    addEventListener: () => {},
    removeEventListener: () => {},
    clearInterval: () => {},
  };
  (globalThis as unknown as { document: unknown }).document = {
    createElement: () => fakeElement({ tag: "div" }),
  };
  (globalThis as unknown as { CSS: unknown }).CSS = { escape: (s: string) => s };

  const harness = makeHarness(notified);

  try {
    run(harness);
  } finally {
    harness.sidebar.dispose();
    (globalThis as unknown as { window?: unknown }).window = originalWindow;
    (globalThis as unknown as { document?: unknown }).document = originalDocument;
    (globalThis as unknown as { CSS?: unknown }).CSS = originalCSS;
  }
}

test("a render whose markup matches the last paint leaves the container alone", () => {
  withEnv(({ sidebar, container }) => {
    assert.equal(container.writes, 1, "construction paints once");
    sidebar.render();
    sidebar.render();
    assert.equal(container.writes, 1,
      "a render that rebuilds the identical string rewrote the container — the skip is not firing");
  });
});

test("a stale list paints dimmed and inert, and an unanswered one paints bands", () => {
  withEnv(({ sidebar, container }) => {
    const model = (sidebar as unknown as { model: { stale: boolean; listState: string; registryReadOk: boolean; groups: unknown[] } }).model;
    assert.ok(!container.html.includes("data-list-stale"), "a served list is live");
    model.stale = true;
    model.listState = "error";
    sidebar.render();
    assert.match(container.html, /data-list-stale aria-busy="true"/, "stale rows are marked busy");
    assert.match(container.html, /data-list-stale aria-busy="true" inert /, "and inert: no row control is focusable or reachable");
    assert.match(container.html, /pointer-events-none opacity-55/, "and drawn dimmed");
    assert.match(container.html, /data-list-syncing/, "the header says the rail is syncing");
    assert.ok(!container.html.includes("data-projects-menu"), "and offers no menu over it");
    assert.match(container.html, /data-sheet-new-session disabled/, "the sheet's New session is disabled over a stale list");
    assert.match(container.html, /alpha/, "the last served rows are still drawn");
    model.stale = false;
    model.listState = "loading";
    model.registryReadOk = false;
    model.groups.length = 0;
    sidebar.render();
    assert.match(container.html, /data-loading-band/, "no rows and no answer draws bands");
    assert.ok(!container.html.includes("data-new-project-row"), "never a create affordance before an answer");
    assert.ok(!container.html.includes("data-projects-menu"), "and no menu over an unread list");
  });
});

test("a model change still repaints through the same render path", () => {
  withEnv(({ sidebar, container, group }) => {
    group.project.name = "beta";
    sidebar.render();
    assert.equal(container.writes, 2, "a changed rail must paint");
    assert.match(container.html, /beta/, "the painted markup carries the new state");
    group.project.name = "alpha";
    sidebar.render();
    assert.equal(container.writes, 3, "returning to a prior string is still a change against the last paint");
  });
});

test("an unchanged render does not re-bind the row controls", () => {
  withEnv(({ sidebar, toggle, list }) => {
    sidebar.render();
    // One bound listener toggles the project once and its rail hides; a
    // re-bound listener set would toggle twice and leave it expanded.
    toggle.dispatchEvent({ type: "click" });
    assert.match(list.html, /id="sessions-p1"[^>]*hidden/,
      "one click must collapse the rail exactly once — bind() ran again on an unchanged render");
  });
});

test("streaming edges patch the span, reach the next paint, and converge without stranding", () => {
  withEnv(({ sidebar, container, metaSpan }) => {
    sidebar.setStreaming("s1", true);
    assert.match(metaSpan.innerHTML, /output streaming/, "the edge patched the span in place");
    assert.equal(container.writes, 1, "the edge itself renders nothing");
    sidebar.render();
    assert.equal(container.writes, 2);
    assert.match(container.html, /output streaming/,
      "the streaming flag is a string input, so the next render paints it");

    sidebar.setStreaming("s1", false);
    assert.equal(metaSpan.innerHTML, "", "silence restored the span's passive bytes");
    sidebar.render();
    assert.equal(container.writes, 3);
    assert.doesNotMatch(container.html, /output streaming/);

    sidebar.render();
    assert.equal(container.writes, 3,
      "a string that has converged back to the cache skips again");
    assert.equal(metaSpan.innerHTML, "",
      "the skipped render left the span holding exactly the bytes the cache says it holds");
  });
});

test("renderProjectList drops the cache, so the next full render paints", () => {
  withEnv(({ sidebar, container }) => {
    sidebar.render();
    assert.equal(container.writes, 1);
    const internals = sidebar as unknown as { renderProjectList(): void };
    internals.renderProjectList();
    sidebar.render();
    assert.equal(container.writes, 2,
      "a render after a partial list redraw must paint — the cache no longer describes the container");
  });
});

test("the project-list observer hears paints and skips alike", () => {
  let fires = 0;
  withEnv(({ sidebar, group }) => {
    assert.equal(fires, 1, "the constructor's paint notifies");
    sidebar.render();
    assert.equal(fires, 1, "a skipped render redraws no rows, so it notifies nothing");
    group.project.name = "beta";
    sidebar.render();
    assert.equal(fires, 2, "a painting render notifies");
  }, () => {
    fires += 1;
  });
});

// spyInnerHtmlWrites counts assignments to a fake element's innerHTML so a
// test can tell a meta tick that wrote nothing from one that patched.
function spyInnerHtmlWrites(el: FakeElement): { value: number } {
  const writes = { value: 0 };
  let html = el.innerHTML;
  Object.defineProperty(el, "innerHTML", {
    get: () => html,
    set: (value: string) => {
      writes.value += 1;
      html = value;
    },
    configurable: true,
  });
  return writes;
}

// The 30-second meta tick paints single spans out of band. When the age
// bucket it recomputes differs from what the span holds, that write must
// drop the rendered-markup cache: a session that just produced output has
// last_output_at moved forward, so the next render can recompute an age
// whose bytes equal the cache's while the span still shows the crossed
// bucket this tick painted. The test stages exactly that race — the tick
// paints 2m, the refreshed model makes the next render compute 1m again —
// and requires the repaint anyway.
test("a meta tick that paints a new bucket forces the next render to repaint", () => {
  withEnv(({ sidebar, container, metaSpan, group }) => {
    const session = group.sessions[0]!;
    const t0 = 1_700_000_000_000;
    const now = { value: t0 };
    const realNow = Date.now;
    Date.now = () => now.value;
    try {
      // The fixture carries no last_output_at; give it one 119s old and
      // repaint so the cache embeds 1m.
      session.last_output_at = new Date(t0 - 119_000).toISOString();
      sidebar.render();
      metaSpan.innerHTML = sessionRowMetaHtml(session, t0);
      const painted = container.writes;

      // The tick, five seconds on, crosses the minute boundary.
      now.value = t0 + 5_000;
      const tick = sidebar as unknown as { refreshSessionMeta(): void };
      tick.refreshSessionMeta();
      assert.equal(
        metaSpan.innerHTML,
        sessionMetaHtml(session, false),
        "the meta tick did not paint the crossed bucket into the span",
      );

      // The refreshed model carries output that postdates the cached
      // render, so the age recomputes to 1m — byte-identical to the cache.
      session.last_output_at = new Date(t0 - 100_000).toISOString();
      now.value = t0 + 10_000;
      sidebar.render();
      assert.equal(container.writes - painted, 1,
        "a render whose bytes match the cache skipped over the span the meta tick patched");
    } finally {
      Date.now = realNow;
    }
  });
});

// When the recomputed bucket has not moved, the tick must not touch the
// span at all: assigning innerHTML re-parses and replaces children even
// for an identical string, restarting the spinner the span may hold. The
// cache survives the no-op tick, so the next render still skips.
test("a meta tick that changes nothing writes nothing and keeps the skip armed", () => {
  withEnv(({ sidebar, container, metaSpan, group }) => {
    const session = group.sessions[0]!;
    const t0 = 1_700_000_000_000;
    const now = { value: t0 };
    const realNow = Date.now;
    Date.now = () => now.value;
    try {
      session.last_output_at = new Date(t0 - 60_000).toISOString();
      sidebar.render();
      metaSpan.innerHTML = sessionRowMetaHtml(session, t0);
      const spanWrites = spyInnerHtmlWrites(metaSpan);
      const painted = container.writes;

      now.value = t0 + 30_000;
      const tick = sidebar as unknown as { refreshSessionMeta(): void };
      tick.refreshSessionMeta();
      assert.equal(spanWrites.value, 0,
        "the meta tick rewrote a span whose bucket had not moved");
      sidebar.render();
      assert.equal(container.writes - painted, 0,
        "a no-op meta tick left the render skip disarmed");
    } finally {
      Date.now = realNow;
    }
  });
});
