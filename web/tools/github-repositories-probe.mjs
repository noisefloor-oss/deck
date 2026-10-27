// Exercise the built chooser and GitHub picker with fixture API responses:
// search, owner filtering, stable-id selection, clone identity, and cancellation.
import assert from "node:assert/strict";
import { createServer } from "node:http";
import { readFile, mkdir } from "node:fs/promises";
import { extname, resolve, sep } from "node:path";
import { browser as connectBrowser } from "./browser.mjs";
const dist = resolve(import.meta.dirname, "../dist");
const mime = {
  ".html": "text/html",
  ".js": "text/javascript",
  ".css": "text/css",
  ".woff2": "font/woff2",
  ".svg": "image/svg+xml"
};
const server = createServer(async (req, res) => {
  const name = new URL(req.url, "http://fixture").pathname;
  const path = resolve(dist, "." + (name === "/" ? "/index.html" : name));
  if (!path.startsWith(dist + sep)) {
    res.writeHead(404).end();
    return;
  }
  try {
    const bytes = await readFile(path);
    res.writeHead(200, { "Content-Type": mime[extname(path)] ?? "application/octet-stream" }).end(bytes);
  } catch {
    res.writeHead(404).end();
  }
});
await new Promise(resolve => server.listen(0, "127.0.0.1", resolve));
const origin = `http://127.0.0.1:${server.address().port}`;
let browser;
try {
  ({ browser } = await connectBrowser());
  for (const viewport of [{ width: 1280, height: 800 }, { width: 390, height: 844 }]) {
    const context = await browser.newContext({ viewport });
    await context.addCookies([{
        name: "noisedeck_csrf",
        value: "fixture",
        url: origin
      }]);
    const page = await context.newPage();
    const errors = [];
    const mutations = [];
    page.on("pageerror", e => errors.push(e.message));
    const account = "fixture-user";
    const profile = {
      id: "identity",
      name: "Fixture Author",
      email: "author@example.invalid",
      revision: 1,
      created_at: "t",
      updated_at: "t"
    };
    const imported = {
      id: "existing",
      name: "Existing",
      canonical_path: "/workspace/existing",
      last_used_at: "t",
      git_identity_setup_revision: 1
    };
    const repos = [
      {
        id: 1,
        name: "existing",
        full_name: "fixture-user/existing",
        owner: "fixture-user",
        owner_type: "User",
        private: false,
        description: "Already registered",
        clone_url: "https://github.com/fixture-user/existing.git"
      },
      {
        id: 2,
        name: "collaborator",
        full_name: "another-owner/collaborator",
        owner: "another-owner",
        owner_type: "User",
        private: true,
        description: "Shared repository",
        clone_url: "https://github.com/another-owner/collaborator.git"
      },
      {
        id: 3,
        name: "library",
        full_name: "sample-org/library",
        owner: "sample-org",
        owner_type: "Organization",
        private: true,
        description: "Organization repository",
        clone_url: "https://github.com/sample-org/library.git"
      }
    ];
    let outcome = "ok", ssoPartial = true, selectedReads = 0, listReads = 0, job = null;
    let releaseTutorial;
    const tutorialResponse = new Promise(resolve => {
      releaseTutorial = resolve;
    });
    await page.route(`${origin }/auth/session`, r => r.fulfill({
      status: 204,
      headers: { "X-Noisedeck-Auth-Mode": "trusted_proxy" }
    }));
    await page.route(`${origin }/api/**`, async route => {
      const req = route.request(), url = new URL(req.url()), path = url.pathname;
      const json = (body, status = 200) => route.fulfill({
        status,
        contentType: "application/json",
        body: JSON.stringify(body)
      });
      if (req.method() !== "GET")
        mutations.push({
          path,
          body: req.postDataJSON()
        });
      if (path === "/api/github/connect/status")
        return json({
          outcome: "authenticated",
          active: true,
          login: account,
          state: "success",
          source: "stored"
        });
      if (path === "/api/github/repositories") {
        listReads++;
        assert.equal(url.searchParams.get("account"), account);
        return json({
          outcome,
          account,
          complete: outcome === "ok",
          ssoPartial,
          repositories: outcome === "ok" ? repos : []
        });
      }
      if (path.startsWith("/api/github/repositories/")) {
        selectedReads++;
        return json({
          outcome: "ok",
          account,
          repository: repos.find(r => String(r.id) === path.split("/").at(-1))
        });
      }
      if (path === "/api/projects/inspect") {
        const input = url.searchParams.get("input");
        if (input === imported.canonical_path)
          return json({
            kind: "repository",
            path: input,
            exists: true,
            remote: "github.com/fixture-user/existing",
            entries: []
          });
        const name = input.split("/").at(-1).replace(/\.git$/, ""), base = url.searchParams.get("base") || "/workspace";
        return json({
          kind: "clone",
          path: `${base }/${name}`,
          exists: false,
          clone_url: input,
          entries: []
        });
      }
      if (path === "/api/projects/clone") {
        const body = req.postDataJSON();
        assert.equal(body.github_account, account);
        assert.equal(body.github_repository_id, 3);
        assert.equal(body.url, repos[2].clone_url);
        assert.equal(body.git_identity_profile_id, profile.id);
        job = {
          id: "clone-fixture",
          url: body.url,
          destination: body.destination,
          name: body.name,
          status: "running",
          percent: 40,
          line: "Receiving objects: 40% (4/10)",
          partial_bytes: 100,
          started_at: "t",
          updated_at: "t"
        };
        return json(job, 202);
      }
      if (path === "/api/project-clones/clone-fixture/cancel") {
        job = {
          ...job,
          status: "stopped",
          percent: 40
        };
        return json(job);
      }
      if (path === "/api/project-clones")
        return json(job ? [job] : []);
      if (path === "/api/projects")
        return json([imported]);
      if (path.endsWith("/sessions"))
        return json([]);
      if (path === "/api/git-identities")
        return json({
          profiles: [profile],
          default_profile_id: profile.id,
          revision: 1
        });
      if (path === "/api/providers")
        return json({ providers: [] });
      if (path === "/api/settings")
        return json({
          skip_permissions: false,
          skip_close_confirm: false,
          skip_remove_project_confirm: false
        });
      if (path === "/api/settings/tutorial") {
        await tutorialResponse;
        return json({
          version: 1,
          dismissed: true,
          lesson: "welcome",
          step: 0,
          done: [],
          scope: "user"
        });
      }
      if (path === "/api/harness-login")
        return json({
          pending: [],
          csrf: "fixture"
        });
      if (path === "/api/settings/sidebar-layout")
        return json({
          expanded: [],
          projects: [],
          sessions: {}
        });
      if (path.endsWith("/events"))
        return route.fulfill({
          contentType: "text/event-stream",
          body: ": fixture\n\n"
        });
      if (path === "/api/settings/last-session" && req.method() === "PUT")
        return json(req.postDataJSON(), 200);
      if (req.method() !== "GET")
        throw new Error(`Unexpected mutation ${path}`);
      return json({
        error: "not_found",
        message: path
      }, 404);
    });
    await page.goto(origin, { waitUntil: "domcontentloaded" });
    const openChooser = async () => {
      if (viewport.width < 600) {
        await page.locator("[data-live-sessions]").click();
        await page.locator("[data-sheet-open-project]").click();
      } else {
        await page.locator("[data-projects-menu]").click();
        await page.getByRole("menuitem", {
          name: "Add project\u2026",
          exact: true
        }).click();
      }
    };
    await openChooser();
    const chooser = page.locator("[data-project-source-dialog]");
    await chooser.waitFor();
    for (const name of [
        "GitHub",
        "Local folder",
        "Repository URL",
        "Start with the tutorial project"
      ])
      assert.ok(await chooser.getByRole("button", {
        name,
        exact: false
      }).count());
    await chooser.locator('[data-project-source="tutorial"]').click();
    await chooser.getByRole("status").filter({ hasText: "The tutorial is still loading" }).waitFor();
    assert.equal(await chooser.isVisible(), true, "loading tutorial keeps the chooser open");
    assert.equal(mutations.length, 0, "unavailable tutorial launches and creates nothing");
    const tutorialLoaded = page.waitForResponse(response => new URL(response.url()).pathname === "/api/settings/tutorial");
    releaseTutorial();
    await (await tutorialLoaded).finished();
    const starterSaved = page.waitForResponse(response => {
      const request = response.request();
      return new URL(response.url()).pathname === "/api/settings/tutorial" && request.method() === "PUT"
        && request.postDataJSON().lesson === "lesson-1" && request.postDataJSON().dismissed === false;
    });
    await chooser.getByRole("button", { name: "Start with the tutorial project", exact: true }).click();
    await (await starterSaved).finished();
    const tutorial = page.locator('[data-region="tutorial"][data-open="true"]');
    await tutorial.waitFor();
    await tutorial.getByRole("heading", { name: "Choose a coding agent", exact: true }).waitFor();
    assert.equal(await chooser.count(), 0, "ready tutorial closes the chooser and opens the guided starter");
    assert.equal(mutations.filter(m => !m.path.startsWith("/api/settings/")).length, 0, "opening the guided starter only changes preferences");
    await tutorial.getByRole("button", { name: "Choose an agent…", exact: true }).waitFor();
    assert.equal(mutations.filter(m => m.path === "/api/settings/tutorial").at(-1)?.body.lesson, "lesson-1");
    await tutorial.getByRole("button", { name: "Close tutorial", exact: true }).click();
    await openChooser();
    await chooser.locator('[data-project-source="github"]').click();
    const picker = page.locator("[data-github-picker]");
    await picker.waitFor();
    await picker.getByRole("button", {
      name: "Open fixture-user/existing",
      exact: true
    }).waitFor();
    const ssoNotice = picker.getByText("Some organizations need single sign-on before their repositories appear here.", { exact: false });
    await ssoNotice.waitFor();
    const authorize = picker.getByRole("link", { name: "Authorize on GitHub", exact: true });
    assert.equal(await authorize.getAttribute("href"), "https://github.com/settings/applications");
    assert.equal(await authorize.getAttribute("target"), "_blank");
    assert.equal(await picker.getByRole("listitem").count(), repos.length);
    ssoPartial = false;
    await picker.getByRole("button", { name: "Refresh", exact: true }).click();
    await ssoNotice.waitFor({ state: "hidden" });
    await picker.getByRole("button", { name: "Open fixture-user/existing", exact: true }).waitFor();
    assert.equal(selectedReads, 0);
    assert.equal(mutations.filter(m => m.path === "/api/projects/clone").length, 0);
    await picker.getByRole("searchbox", { name: "Search repositories" }).fill("collaborator");
    await picker.getByRole("button", {
      name: "Import another-owner/collaborator",
      exact: true
    }).waitFor();
    assert.equal(await picker.getByRole("button", {
      name: "Import sample-org/library",
      exact: true
    }).count(), 0);
    assert.equal(await picker.getByRole("searchbox", { name: "Search repositories" }).evaluate(el => document.activeElement === el), true, "typing preserves focus");
    await picker.getByRole("searchbox", { name: "Search repositories" }).fill("");
    await picker.getByRole("combobox", { name: "Repository owner" }).selectOption("sample-org");
    await picker.getByRole("button", {
      name: "Import sample-org/library",
      exact: true
    }).waitFor();
    if (process.env.NOISE_GITHUB_PICKER_CAPTURE_DIR) {
      await mkdir(process.env.NOISE_GITHUB_PICKER_CAPTURE_DIR, { recursive: true });
      await page.evaluate(() => document.fonts.ready);
      await page.screenshot({ path: resolve(process.env.NOISE_GITHUB_PICKER_CAPTURE_DIR, `${process.env.NOISE_GITHUB_PICKER_CAPTURE_LABEL ?? "build"}-${viewport.width > 600 ? "desktop" : "mobile"}.png`) });
    }
    const box = await picker.boundingBox();
    assert.ok(box.x >= 0 && box.width <= viewport.width, "picker fits viewport");
    await picker.getByRole("button", {
      name: "Import sample-org/library",
      exact: true
    }).click();
    const destination = page.locator("[data-add-project]");
    await destination.locator("[data-project-submit]").waitFor();
    assert.equal(selectedReads, 1, "selection revalidates stable repository id");
    await destination.locator("[data-project-submit]").click();
    await destination.locator("[data-clone-cancel]").waitFor();
    assert.equal(mutations.filter(m => m.path === "/api/projects/clone").length, 1);
    await destination.locator("[data-clone-cancel]").click();
    await destination.locator("[data-clone-resume]").waitFor();
    assert.equal(mutations.filter(m => m.path.includes("sessions")).length, 0, "import never launches a model");
    assert.ok(listReads >= 1);
    const mutationsBeforePreview = mutations.length;
    await page.goto(`${origin}/preview.html?empty=no-project`, { waitUntil: "networkidle" });
    await page.locator("[data-empty-action]").click();
    await chooser.getByRole("button", { name: "Start with the tutorial project", exact: true }).click();
    await tutorial.getByRole("heading", { name: "Choose a coding agent", exact: true }).waitFor();
    await tutorial.getByRole("button", { name: "Choose an agent…", exact: true }).waitFor();
    assert.equal(await chooser.count(), 0, "preview shares the starter chooser handoff");
    await tutorial.getByRole("button", { name: "Close tutorial", exact: true }).click();
    assert.equal(await page.locator("[data-titlebar-more]").evaluate(el => document.activeElement === el), true, "preview close returns focus to the current menu button");
    await page.locator("[data-titlebar-more]").click();
    await page.getByRole("button", { name: "Show tutorial", exact: true }).click();
    await tutorial.getByRole("heading", { name: "Choose a coding agent", exact: true }).waitFor();
    assert.equal(mutations.length, mutationsBeforePreview, "preview tutorial navigation sends no API mutations");
    assert.deepEqual(errors, []);
    await context.close();
  }
  console.log("GitHub picker built desktop/mobile: chooser, search, owners, imported Open, selection revalidation, clone identity and cancellation passed");
} finally {
  await browser?.close();
  await new Promise(resolve => server.close(resolve));
}
