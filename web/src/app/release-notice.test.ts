import assert from "node:assert/strict";
import test from "node:test";
import { isStableReleaseBuild, validReleaseUpdate, watchReleaseNotice } from "./release-notice.ts";

test("release notices accept only stable public Deck release links", () => {
  const value = { version: "v0.4.0", release_url: "https://github.com/noisefloor-oss/deck/releases/tag/v0.4.0" };
  assert.equal(validReleaseUpdate(value), true);
  for (const bad of [
    null,
    {},
    { ...value, version: "v00.4.0" },
    { ...value, version: "v0.4.0-rc.1" },
    { ...value, release_url: "javascript:alert(1)" },
    { ...value, release_url: "https://github.com.example/deck" },
    { ...value, release_url: value.release_url + "?extra" },
  ]) assert.equal(validReleaseUpdate(bad), false);
});

test("an authorization response after disposal cannot affect a new workspace", async () => {
  const original = globalThis.fetch;
  let answer!: (value: Response) => void;
  let lost = 0;
  const region = { hidden: true, replaceChildren() {} };
  const root = { querySelector: () => region } as unknown as HTMLElement;
  globalThis.fetch = () => new Promise<Response>((resolve) => { answer = resolve; });
  try {
    const dispose = watchReleaseNotice(root, () => { lost += 1; });
    dispose();
    answer(new Response(null, { status: 401 }));
    await Promise.resolve();
    await Promise.resolve();
    assert.equal(lost, 0);
  } finally {
    globalThis.fetch = original;
  }
});

test("private and prerelease document builds make no update check", () => {
  for (const id of [null, "dev", "dev+abc123", "v0.3.0-12-gabc", "0.4.0-rc.1", "build-a", "00.3.0"]) {
    assert.equal(isStableReleaseBuild(id), false);
  }
  for (const id of ["0.3.0", "v0.3.0"]) assert.equal(isStableReleaseBuild(id), true);
});

test("advisory denials preserve authorization while real loss is reported", async () => {
  const original = globalThis.fetch;
  const region = { hidden: true, replaceChildren() {} };
  const root = { querySelector: () => region } as unknown as HTMLElement;
  try {
    for (const [status, body, expected] of [
      [403, "origin not allowed", 0],
      [403, JSON.stringify({ code: "forbidden" }), 0],
      [403, JSON.stringify({ code: "unlock-required" }), 1],
      [401, "unauthorized", 1],
    ] as const) {
      let lost = 0;
      globalThis.fetch = async () => new Response(body, { status });
      const dispose = watchReleaseNotice(root, () => { lost += 1; });
      try {
        // The shared helper clones and reads a 403 body asynchronously.
        await new Promise(resolve => setTimeout(resolve, 30));
        assert.equal(lost, expected, `${status} ${body}`);
      } finally {
        dispose();
      }
    }
  } finally {
    globalThis.fetch = original;
  }
});
