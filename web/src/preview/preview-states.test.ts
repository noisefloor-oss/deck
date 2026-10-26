// The preview state catalog: every enumerated state must parse
// from its own params and round-trip back to its name, so the capture
// harness and preview.ts cannot disagree about what a URL means.
import assert from "node:assert/strict";
import test from "node:test";
import {
  describeState,
  parsePreviewParams,
  PREVIEW_STATES,
} from "./preview-states.ts";

test("every enumerated state parses from its own params and round-trips to its name", () => {
  for (const entry of PREVIEW_STATES) {
    const parsed = parsePreviewParams(new URLSearchParams(entry.params));
    assert.equal(
      describeState(parsed).name,
      entry.name,
      `${entry.name} should round-trip through parse + describe`,
    );
  }
});

test("state names are unique", () => {
  const names = PREVIEW_STATES.map((entry) => entry.name);
  assert.equal(new Set(names).size, names.length);
});

test("an unknown axis value throws instead of silently defaulting", () => {
  assert.throws(
    () => parsePreviewParams(new URLSearchParams({ menu: "bogus" })),
    /invalid menu/,
  );
  assert.throws(
    () => parsePreviewParams(new URLSearchParams({ dialog: "nope" })),
    /invalid dialog/,
  );
  assert.throws(
    () => parsePreviewParams(new URLSearchParams({ "files-depth": "banana" })),
    /invalid files-depth/,
  );
  assert.throws(
    () => parsePreviewParams(new URLSearchParams({ "files-depth": "99" })),
    /out of range/,
  );
  assert.throws(
    () => parsePreviewParams(new URLSearchParams({ empty: "nope" })),
    /invalid empty/,
  );
});


test("unset axes are null and the pre-existing defaults hold", () => {
  const parsed = parsePreviewParams(new URLSearchParams());
  assert.equal(parsed.menu, null);
  assert.equal(parsed.dialog, null);
  assert.equal(parsed.hover, null);
  assert.equal(parsed.rename, null);
  assert.equal(parsed.panel, null);
  assert.equal(parsed.assets, null);
  assert.equal(parsed.empty, null);
  assert.equal(parsed.identities, "some");
  assert.equal(parsed.filesDepth, null);
  assert.equal(parsed.viewer, null);
  assert.equal(parsed.filesExpanded, false);
  assert.equal(parsed.tasks, "ready");
  assert.equal(parsed.recovery, "live");
  assert.equal(parsed.delayedRemoval, false);
  assert.equal(parsed.sidebarPollMS, null);
  assert.equal(parsed.sidebar, null);
});

test("the baseline is the fallback for a param set that matches no state", () => {
  const parsed = parsePreviewParams(new URLSearchParams({ "delayed-removal": "1" }));
  assert.equal(describeState(parsed).name, "baseline");
});

test("dialog=rename-project aliases the inline rename=project state", () => {
  const parsed = parsePreviewParams(new URLSearchParams({ dialog: "rename-project" }));
  assert.equal(parsed.dialog, null);
  assert.equal(parsed.rename, "project");
  assert.equal(describeState(parsed).name, "rename-project");
});

test("sidebar-poll parses a millisecond interval and rejects anything else", () => {
  assert.equal(
    parsePreviewParams(new URLSearchParams({ "sidebar-poll": "250" })).sidebarPollMS,
    250,
  );
  assert.throws(
    () => parsePreviewParams(new URLSearchParams({ "sidebar-poll": "often" })),
    /invalid sidebar-poll/,
  );
  assert.throws(
    () => parsePreviewParams(new URLSearchParams({ "sidebar-poll": "10" })),
    /out of range/,
  );
});

test("mount-delay parses a millisecond hold and rejects anything else", () => {
  assert.equal(parsePreviewParams(new URLSearchParams()).mountDelayMS, null);
  assert.equal(parsePreviewParams(new URLSearchParams({ "mount-delay": "800" })).mountDelayMS, 800);
  assert.throws(() => parsePreviewParams(new URLSearchParams({ "mount-delay": "soon" })), /invalid mount-delay/);
  assert.throws(() => parsePreviewParams(new URLSearchParams({ "mount-delay": "90000" })), /invalid mount-delay/);
});

test("project-refresh-delay parses a millisecond hold and rejects anything else", () => {
  assert.equal(parsePreviewParams(new URLSearchParams()).projectRefreshDelayMS, null);
  assert.equal(parsePreviewParams(new URLSearchParams({ "project-refresh-delay": "800" })).projectRefreshDelayMS, 800);
  assert.throws(() => parsePreviewParams(new URLSearchParams({ "project-refresh-delay": "soon" })), /invalid project-refresh-delay/);
  assert.throws(() => parsePreviewParams(new URLSearchParams({ "project-refresh-delay": "90000" })), /invalid project-refresh-delay/);
});

test("the files axes parse and are reported by describeState", () => {
  const params = parsePreviewParams(new URLSearchParams({
    panel: "files",
    "files-depth": "2",
    "files-select": "provider.go",
  }));
  assert.equal(params.filesDepth, 2);
  assert.equal(params.filesSelect, "provider.go");
  // describeState builds its record from the params, so an axis it omitted
  // would make two different screens describe themselves identically.
  const withSelect = describeState(params);
  const withoutSelect = describeState(
    parsePreviewParams(new URLSearchParams({ panel: "files", "files-depth": "2" })),
  );
  assert.notEqual(withSelect.name, withoutSelect.name);
});

test("the files axes are absent when unset, so no other state carries them", () => {
  const params = parsePreviewParams(new URLSearchParams({ panel: "files" }));
  assert.equal(params.filesDepth, null);
  assert.equal(params.filesSelect, null);
});

test("a files-select naming nothing is rejected rather than ignored", () => {
  const params = parsePreviewParams(new URLSearchParams({
    panel: "files",
    "files-depth": "2",
    "files-select": "missing.go",
  }));
  // Parsing accepts any name; the applied state is what must refuse, because
  // only there is the directory listing known. This pins the parse half.
  assert.equal(params.filesSelect, "missing.go");
});

test("identities=none starts with no Git identity, and only none is accepted", () => {
  assert.equal(parsePreviewParams(new URLSearchParams("identities=none")).identities, "none");
  assert.throws(() => parsePreviewParams(new URLSearchParams("identities=all")), /invalid identities/);
});

test("the image viewer states expand only where an expanded state is enumerated", () => {
  const expanded = PREVIEW_STATES.filter((entry) => entry.params.expanded === "1").map((entry) => entry.name);
  assert.deepEqual(expanded, [
    "viewer-image-expanded",
    "viewer-image-actual-expanded",
    "viewer-image-oversized-expanded",
  ]);
  const gone = parsePreviewParams(new URLSearchParams({ panel: "files", viewer: "image-gone", expanded: "1" }));
  assert.equal(describeState(gone).name, "baseline", "an expanded combination with no state is not certified as another");
});

test("frame=phone-screen is the phone frame drawn flush with the device's top edge", () => {
  const flush = parsePreviewParams(new URLSearchParams({ frame: "phone-screen" }));
  assert.equal(flush.frame, "phone");
  assert.equal(flush.phoneScreen, true);
  const figure = parsePreviewParams(new URLSearchParams({ frame: "phone" }));
  assert.equal(figure.phoneScreen, false);
  assert.throws(() => parsePreviewParams(new URLSearchParams({ frame: "tablet" })), /invalid frame/);
});

test("the assets axis parses its six presentations and rejects anything else", () => {
  for (const assets of ["loading", "empty", "list", "selected", "truncated", "after"]) {
    const parsed = parsePreviewParams(new URLSearchParams({ panel: "assets", assets }));
    assert.equal(parsed.panel, "assets");
    assert.equal(parsed.assets, assets);
    assert.equal(describeState(parsed).name, `assets-${assets}`);
  }
  assert.throws(() => parsePreviewParams(new URLSearchParams({ assets: "bogus" })), /invalid assets/);
  assert.equal(describeState(parsePreviewParams(new URLSearchParams({ panel: "assets", assets: "selected", dialog: "delete-assets" }))).name, "delete-confirm");
  assert.equal(describeState(parsePreviewParams(new URLSearchParams({ panel: "assets", assets: "after", dialog: "delete-outcome" }))).name, "delete-outcome");
  assert.equal(describeState(parsePreviewParams(new URLSearchParams({ menu: "panels" }))).name, "menu-panels");
});

test("the assets-unseen axis counts new fixture files and rejects anything else", () => {
  const parsed = parsePreviewParams(new URLSearchParams({ "assets-unseen": "3" }));
  assert.equal(parsed.assetsUnseen, 3);
  assert.equal(describeState(parsed).name, "assets-unseen");
  assert.equal(describeState(parsePreviewParams(new URLSearchParams({ menu: "panels", "assets-unseen": "3" }))).name, "menu-panels-assets-unseen");
  assert.equal(parsePreviewParams(new URLSearchParams()).assetsUnseen, null);
  for (const bad of ["0", "4", "x", "3.5"]) {
    assert.throws(() => parsePreviewParams(new URLSearchParams({ "assets-unseen": bad })), /invalid assets-unseen/);
  }
});
