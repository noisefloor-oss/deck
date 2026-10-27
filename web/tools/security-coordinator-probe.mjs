// Production entry + real browser cryptography against a local HTTP protocol
// fixture. Native storage, Google and WebAuthn are not qualified by this probe.
import assert from "node:assert/strict";
import { createServer } from "node:http";
import { readFile, stat } from "node:fs/promises";
import { extname, resolve, sep } from "node:path";
import { randomBytes, createHash, webcrypto } from "node:crypto";
import { browser as connectBrowser } from "./browser.mjs";
import { eventually } from "./eventually.mjs";

const dist = resolve(import.meta.dirname, "../dist");
const mime = { ".html": "text/html", ".js": "text/javascript", ".css": "text/css", ".svg": "image/svg+xml", ".woff2": "font/woff2", ".png": "image/png" };
const token = () => randomBytes(32).toString("base64url");
let authenticated = true, grant = false, marker = "v1", phase = "awaiting-method", storage = "closed", runtime = "stopped", commitment;
let loseLockResponse = false, locked = false;
let grantingMethod;
let policy = "required", protection = "encrypted", plainOperationId;
let losePlainResponse = false, plainStarts = 0, wrongOwner = false, onboardingAvailable = true;
const plainStartBodies = [];
let handedOffInvitation;
let heldManagementResponse;
const methods = new Map(), challenges = new Map(), operations = new Map();
const mutations = [], privateRequests = [], errors = [], serverErrors = [];
let csrfFailures = 0;
const onboarding = () => ({ policy, protection, started: phase !== "invitation-required", setupComplete: phase === "ready" });
const state = () => ({ instanceId: "test-workspace", phase, commitment, storage, runtime, locked, browserGranted: grant,
  onboarding: onboarding(), ownerBound: phase !== "invitation-required", workspaceAuthorized: phase === "ready" && (protection === "none" ? authenticated && storage === "unencrypted" && runtime === "running" : grant),
  ...(plainOperationId ? { plainOperationId } : {}) });
function json(response, status, value) {
  response.writeHead(status, { "Content-Type": "application/json" });
  response.end(value === undefined ? "" : JSON.stringify(value));
}
function holdManagementResponse(stage) {
  let entered, release;
  const ready = new Promise(resolve => { entered = resolve; });
  const waiting = new Promise(resolve => { release = resolve; });
  heldManagementResponse = { stage, entered, waiting };
  return { ready, release };
}
async function waitForManagementResponse(stage, id) {
  if (heldManagementResponse?.stage !== stage) return;
  const held = heldManagementResponse;
  heldManagementResponse = undefined;
  held.entered(id);
  await held.waiting;
}
const server = createServer(async (request, response) => {
  try {
    const path = new URL(request.url, "http://local").pathname;
    if (marker) response.setHeader("X-Deck-Workspace-Security", marker);
    response.setHeader("Cache-Control", "no-store");
    if (path === "/auth/session") {
      response.setHeader("X-Noisedeck-Auth-Mode", "oidc");
      json(response, authenticated ? 204 : 401); return;
    }
    if (path === "/auth/login") { authenticated = true; wrongOwner = false; response.writeHead(302, { Location: "/" }); response.end(); return; }
    if (path === "/auth/logout") {
      if (request.headers["x-csrf-token"] !== "public-csrf-fixture") { csrfFailures++; json(response, 403, { code: "csrf-required" }); return; }
      authenticated = false; grant = false; json(response, 204); return;
    }
    if (path.startsWith("/api/security/v1/")) {
      if (path.endsWith("/onboarding") && request.method === "GET") { json(response, onboardingAvailable ? 200 : 503, onboardingAvailable ? onboarding() : { code: "security-unavailable" }); return; }
      if (path.endsWith("/invitation/handoff")) {
        let data = ""; for await (const chunk of request) data += chunk;
        handedOffInvitation = JSON.parse(data).token;
        response.setHeader("Set-Cookie", "fixture_invitation=present; HttpOnly; SameSite=Lax; Path=/");
        json(response, 204); return;
      }
      if (!authenticated) { json(response, 401, { code: "authorization-required" }); return; }
      if (request.method !== "GET" && request.headers["x-csrf-token"] !== "public-csrf-fixture") { csrfFailures++; json(response, 403, { code: "csrf-required" }); return; }
      let body;
      if (request.method !== "GET") {
        let data = ""; for await (const chunk of request) data += chunk;
        body = JSON.parse(data);
        assert.ok(!data.includes("fixture encryption password"), "password must never cross the network");
        assert.ok(!data.includes("recoverySecret") && !data.includes("prfResult"), "wrapping secrets must never cross the network");
      }
      const route = path.slice("/api/security/v1".length);
      if (route === "/onboarding/protection") {
        if (protection !== "undecided" || policy === "required" && body.protection === "none" || policy === "unavailable" && body.protection === "encrypted") { json(response, 409, { code: "state-changed" }); return; }
        protection = body.protection;
        if (protection === "none") { plainOperationId = token(); phase = "awaiting-start"; storage = "unknown"; }
        json(response, 200, onboarding()); return;
      }
      if (route === "/onboarding/start") {
        assert.equal(protection, "none");
        if (phase === "ready" && runtime === "stopped" && body.operationId === undefined) plainOperationId = token();
        if (body.operationId) assert.equal(body.operationId, plainOperationId);
        if (operations.has(plainOperationId)) { json(response, 202, { id: plainOperationId, status: "unconfirmed" }); return; }
        plainStarts++; plainStartBodies.push(body);
        if (losePlainResponse) {
          runtime = "unknown"; storage = "unknown";
          operations.set(plainOperationId, { id: plainOperationId, status: "uncertain", storage, runtime, locked: false });
          request.socket.destroy(); return;
        }
        phase = "ready"; runtime = "running"; storage = "unencrypted";
        operations.set(plainOperationId, { id: plainOperationId, status: "succeeded", storage, runtime, locked: false });
        json(response, 200, { id: plainOperationId, status: "accepted" }); return;
      }
      const plainObservation = /^\/onboarding\/operations\/(.+)$/.exec(route);
      if (plainObservation) { json(response, 200, operations.get(plainObservation[1]) ?? { id: plainObservation[1], status: "pending", storage, runtime, locked: false }); return; }
      if (route === "/invitation/redeem") {
        assert.deepEqual(body, {}, "OAuth return redeems the HttpOnly handoff without repeating its token");
        assert.match(request.headers.cookie ?? "", /fixture_invitation=present/);
        assert.ok(handedOffInvitation); handedOffInvitation = undefined;
        phase = "awaiting-method"; protection = "undecided"; response.setHeader("Set-Cookie", "fixture_invitation=; Max-Age=0; HttpOnly; SameSite=Lax; Path=/");
        json(response, 200, { revision: 1 }); return;
      }
      if (route === "/state") { json(response, wrongOwner ? 403 : 200, wrongOwner ? { code: "authorization-required" } : state()); return; }
      if (route === "/unlock-options" || route === "/methods") { json(response, 200, [...methods.values()].map(method => ({ ...method, ...(route === "/unlock-options" ? { label: "" } : {}) }))); return; }
      if (route === "/methods/prepare") {
        assert.notEqual(protection, "none"); protection = "encrypted";
        const method = { id: token(), revision: 1, kind: body.kind, label: body.label, status: "pending", salt: token() };
        methods.set(method.id, method); json(response, 201, { method, ...(body.kind === "passkey" ? { registration: { publicKey: {
          challenge: token(), rp: { name: "Fixture", id: "127.0.0.1" }, user: { id: token(), name: "fixture@example.invalid", displayName: "Fixture" },
          pubKeyCredParams: [{ type: "public-key", alg: -7 }], authenticatorSelection: { residentKey: "required", userVerification: "required" }, extensions: { prf: {} },
        } } } : {}) }); return;
      }
      const methodRoute = /^\/methods\/([^/]+)\/(envelope|label)$/.exec(route);
      if (methodRoute) {
        const method = methods.get(methodRoute[1]); assert.ok(method);
        if (methodRoute[2] === "label") { method.label = body.label; json(response, 204); return; }
        if (request.method === "GET") { json(response, 200, method.envelope); return; }
        assert.equal(body.envelope.methodId, method.id);
        method.envelope = body.envelope; commitment ??= body.commitment;
        assert.equal(commitment, body.commitment); json(response, 204); return;
      }
      if (route === "/challenges") {
        const id = token(); challenges.set(id, body);
        json(response, 201, { id, operationId: id, challenge: id, expiresAt: "2030-01-01T00:00:00Z" }); return;
      }
      if (route === "/operations") {
        const action = challenges.get(body.challengeId);
        // Chromium can retry a transport failure before surfacing it to fetch.
        // A consumed challenge never executes a second effect.
        if (!action) { json(response, 409, { code: "state-changed" }); return; }
        challenges.delete(body.challengeId);
        const method = methods.get(action.method.id); assert.ok(method?.envelope);
        const publicKey = await webcrypto.subtle.importKey("spki", Buffer.from(method.envelope.proofPublicKey, "base64url"), { name: "ECDSA", namedCurve: "P-256" }, false, ["verify"]);
        assert.equal(await webcrypto.subtle.verify({ name: "ECDSA", hash: "SHA-256" }, publicKey, Buffer.from(body.signature, "base64url"), Buffer.from(body.challengeId, "base64url")), true, "real browser proof verifies against the saved public key");
        mutations.push({ operation: action.operation, hasVolumeSecret: Boolean(body.volumeSecret), id: body.challengeId });
        if (["initialize", "unlock"].includes(action.operation)) {
          const secret = Buffer.from(body.volumeSecret, "base64url");
          assert.equal(secret.length, 32);
          assert.equal(createHash("sha256").update("deck.volume.v1\0").update(secret).digest("base64url"), commitment);
          secret.fill(0); phase = "ready"; storage = "open"; runtime = "running"; locked = false; grant = true; grantingMethod = method.id;
          if (action.operation === "initialize") {
            for (const item of methods.values()) {
              if (item.id === action.method.id || item.id === action.recovery?.id) item.status = "active";
            }
          }
        } else assert.equal(body.volumeSecret, undefined, "browser authorization and method checks do not send the volume secret");
        if (action.operation === "verify-method") { method.verifiedAt = new Date().toISOString(); if (method.kind !== "recovery") phase = phase === "ready" ? phase : "awaiting-recovery"; }
        if (action.operation === "authorize-browser") { grant = true; grantingMethod = method.id; }
        if (["add-method", "replace-method", "remove-method"].includes(action.operation)) {
          assert.equal(method.status, "active", "a current method authorizes changes");
          const target = methods.get(action.target.id); assert.ok(target);
          if (action.operation === "remove-method") {
            assert.ok([...methods.values()].filter(item => item.status === "active" && item.kind !== "recovery").length > 1);
            target.status = "removed";
            if (grantingMethod === target.id) grant = false;
          } else {
            assert.equal(target.status, "pending"); assert.ok(target.verifiedAt, "replacement requires a saved-method test");
            target.status = "active";
            if (action.replacement) {
              const old = methods.get(action.replacement.id); assert.equal(old.status, "active"); old.status = "removed";
              if (grantingMethod === old.id) grant = false;
            }
          }
        }
        if (action.operation === "lock") {
          grant = false;
          if (loseLockResponse) {
            operations.set(body.challengeId, { id: body.challengeId, status: "unconfirmed", storage: "unknown", runtime: "unknown", locked: false });
            request.socket.destroy(); return;
          }
          storage = "closed"; runtime = "stopped"; locked = true;
        }
        operations.set(body.challengeId, { id: body.challengeId, status: "complete", storage, runtime, locked });
        await waitForManagementResponse("operate", body.challengeId);
        json(response, 200, { id: body.challengeId, status: "accepted" }); return;
      }
      const observation = /^\/operations\/(.+)$/.exec(route);
      if (observation) {
        const result = operations.get(observation[1]);
        await waitForManagementResponse("observe", observation[1]);
        json(response, result ? 200 : 404, result ?? { code: "operation-unavailable" }); return;
      }
      json(response, 404, { code: "unavailable-fixture-route" }); return;
    }
    if (path.startsWith("/api/")) {
      privateRequests.push(path);
      if (marker && !(protection === "none" ? state().workspaceAuthorized : grant)) { json(response, 403, { code: "unlock-required" }); return; }
      if (path.endsWith("/events")) { response.writeHead(200, { "Content-Type": "text/event-stream" }); response.end(": fixture\n\n"); return; }
      if (path === "/api/settings/tutorial") { json(response, 200, { version: 1, dismissed: true, lesson: "welcome", step: 0, done: [], scope: "user" }); return; }
      if (path === "/api/settings") { json(response, 200, { skip_permissions: false, skip_close_confirm: false, skip_remove_project_confirm: false }); return; }
      if (path === "/api/settings/layout") { json(response, 404, { code: "layout-unavailable" }); return; }
      if (path === "/api/git-identities") { json(response, 200, { profiles: [{ id: "fixture", revision: 1, name: "Fixture", email: "fixture@example.invalid" }], default_profile_id: "fixture", revision: 1 }); return; }
      json(response, 200, []); return;
    }
    const file = resolve(dist, `.${path === "/" ? "/index.html" : path}`);
    if (!file.startsWith(`${dist}${sep}`) || !(await stat(file)).isFile()) throw new Error("not found");
    response.writeHead(200, { "Content-Type": mime[extname(file)] ?? "application/octet-stream" });
    response.end(await readFile(file));
  } catch (error) { serverErrors.push(error.message); if (!response.headersSent) json(response, 500, { code: "fixture-error" }); else response.end(); }
});
await new Promise(resolveListen => server.listen(0, "127.0.0.1", resolveListen));
const origin = `http://127.0.0.1:${server.address().port}`;
let browser;
try {
  ({ browser } = await connectBrowser());
  const context = await browser.newContext({ reducedMotion: "reduce", colorScheme: "dark", viewport: { width: 1120, height: 720 } });
  await context.addCookies([{ name: "noisedeck_csrf", value: "public-csrf-fixture", url: origin }]);
  await context.route("**/*", route => new URL(route.request().url()).origin === origin ? route.continue() : route.abort());
  const page = await context.newPage();
  page.on("pageerror", error => errors.push(error.stack ?? error.message));
  await page.goto(origin, { waitUntil: "domcontentloaded" });
  await page.getByRole("button", { name: "Continue setup", exact: true }).click();
  await page.getByRole("button", { name: "Use a password instead" }).click();
  let password = "fixture encryption password";
  await page.getByLabel("Encryption password", { exact: true }).fill("short");
  await page.getByLabel("Repeat it").fill("short");
  await page.getByRole("button", { name: "Set password and check it" }).click();
  await page.getByRole("alert").filter({ hasText: "Use at least 15 characters" }).waitFor();
  await page.getByLabel("Encryption password", { exact: true }).fill(password);
  await page.getByLabel("Repeat it").fill("a different long password");
  await page.getByRole("button", { name: "Set password and check it" }).click();
  await page.getByRole("alert").filter({ hasText: "passwords do not match" }).waitFor();
  assert.equal(methods.size, 0, "invalid or mismatched passwords cannot prepare a method");
  await page.getByLabel("Encryption password", { exact: true }).fill(password);
  await page.getByLabel("Repeat it").fill(password);
  await page.getByRole("button", { name: "Set password and check it" }).click();
  await page.getByRole("heading", { name: "Password checked", exact: true }).waitFor();
  assert.equal(privateRequests.length, 0);
  // Reload loses volatile keys. Resuming requires the actual saved method again.
  await page.reload({ waitUntil: "domcontentloaded" });
  await page.getByRole("button", { name: "Verify your saved unlock method", exact: true }).click();
  await page.getByLabel("Encryption password", { exact: true }).fill(password);
  await page.getByRole("button", { name: "Continue setup", exact: true }).click();
  await page.getByRole("heading", { name: "Password checked", exact: true }).waitFor();
  assert.ok(mutations.some(action => action.operation === "resume-enrollment"));
  await page.getByLabel("Name this method").fill("Browser fixture password");
  await page.getByRole("button", { name: "Continue to your recovery key" }).click();
  await page.locator('[data-enrollment="recovery-save"]').waitFor();
  await page.evaluate(() => { Object.defineProperty(navigator, "clipboard", { configurable: true, value: { writeText: async () => { throw new Error("Clipboard unavailable in this fixture."); } } }); });
  await page.getByRole("button", { name: "Copy key", exact: true }).click();
  await page.getByRole("alert").filter({ hasText: "Clipboard unavailable" }).waitFor();
  await page.evaluate(() => { Object.defineProperty(navigator, "clipboard", { configurable: true, value: { writeText: async value => { window.__fixtureCopied = value; } } }); });
  await page.getByRole("button", { name: "Copy key", exact: true }).click();
  await page.getByRole("status").filter({ hasText: "Recovery key copied." }).waitFor();
  const [download] = await Promise.all([page.waitForEvent("download"), page.getByRole("button", { name: "Download recovery kit", exact: true }).click()]);
  const stream = await download.createReadStream(); const chunks = []; for await (const chunk of stream) chunks.push(chunk);
  let kitBytes = Buffer.concat(chunks), kit = JSON.parse(kitBytes.toString("utf8"));
  assert.equal(kit.instanceId, "test-workspace");
  assert.equal(kit.recoveryKey, await page.evaluate(() => window.__fixtureCopied));
  await page.getByRole("button", { name: "I have saved it — check it works" }).click();
  await page.locator("[data-enroll-kit-file]").setInputFiles({ name: "invalid.json", mimeType: "application/json", buffer: Buffer.from("{}") });
  await page.getByRole("alert").filter({ hasText: "does not belong" }).waitFor();
  authenticated = false;
  const beforeExpiry = mutations.length;
  await page.locator("[data-enroll-kit-file]").setInputFiles({ name: "kit.json", mimeType: "application/json", buffer: kitBytes });
  await page.locator('[data-onboarding="interrupted"]').waitFor();
  assert.equal(await page.locator('[name="recovery"], [data-enroll-kit-file]').count(), 0, "expired authorization removes recovery inputs");
  assert.ok(!(await page.locator("main").innerHTML()).includes(kit.recoveryKey), "the submitted recovery key must not return in an error view");
  assert.equal(mutations.length, beforeExpiry, "expired setup cannot verify or initialize");
  assert.equal(privateRequests.length, 0);
  await page.getByRole("button", { name: "Sign in with Google", exact: true }).click();
  await page.getByRole("button", { name: "Verify your saved unlock method", exact: true }).click();
  await page.getByLabel("Encryption password", { exact: true }).fill(password);
  await page.getByRole("button", { name: "Continue setup", exact: true }).click();
  await page.getByRole("heading", { name: "Password checked", exact: true }).waitFor();
  await page.getByLabel("Name this method").fill("Browser fixture password");
  await page.getByRole("button", { name: "Continue to your recovery key", exact: true }).click();
  await page.locator('[data-enrollment="recovery-save"]').waitFor();
  const [replacementDownload] = await Promise.all([page.waitForEvent("download"), page.getByRole("button", { name: "Download recovery kit", exact: true }).click()]);
  const replacementStream = await replacementDownload.createReadStream(); const replacementChunks = [];
  for await (const chunk of replacementStream) replacementChunks.push(chunk);
  kitBytes = Buffer.concat(replacementChunks); kit = JSON.parse(kitBytes.toString("utf8"));
  await page.getByRole("button", { name: "I have saved it — check it works" }).click();
  await page.locator("[data-enroll-kit-file]").setInputFiles({ name: "kit.json", mimeType: "application/json", buffer: kitBytes });
  await page.getByRole("button", { name: "Finish setup", exact: true }).waitFor();
  assert.equal(privateRequests.length, 0);
  await page.getByLabel("Encryption password", { exact: true }).fill(password);
  await page.getByRole("button", { name: "Finish setup", exact: true }).click();
  await page.getByRole("heading", { name: "Workspace ready", exact: true }).waitFor();
  assert.equal(privateRequests.length, 0);
  await page.getByRole("button", { name: "Open test-workspace", exact: true }).click();
  await page.locator("[data-live-more]").waitFor();
  await eventually(() => privateRequests.length, count => count > 0, "authorized workspace requests reach the fixture");
  assert.ok(privateRequests.length > 0);
  grant = false;
  await page.getByRole("heading", { name: "Your workspace is running" }).waitFor({ timeout: 10_000 });
  assert.equal(await page.locator("[data-live-more]").count(), 0, "revocation disposes the live workspace");
  assert.doesNotMatch(await page.locator("main").innerText(), /owner@|op-4412|rewrite-scheduler|flaky-test-hunt/, "the unauthorized browser receives no invented private or administrator metadata");
  const before = privateRequests.length, nativeBefore = mutations.filter(action => ["initialize", "unlock"].includes(action.operation)).length;
  await page.getByRole("button", { name: "Use encryption password", exact: true }).click();
  await page.getByLabel("Encryption password", { exact: true }).fill("incorrect fixture password");
  await page.getByRole("button", { name: "Verify this browser", exact: true }).click();
  await page.getByRole("alert").filter({ hasText: "That password did not unlock" }).waitFor();
  assert.equal(privateRequests.length, before);
  await page.getByLabel("Encryption password", { exact: true }).fill(password);
  await page.getByRole("button", { name: "Try again", exact: true }).click();
  await page.locator("[data-live-more]").waitFor();
  assert.equal(mutations.filter(action => ["initialize", "unlock"].includes(action.operation)).length, nativeBefore, "joining a running workspace never initializes/unlocks storage");
  assert.equal(mutations.at(-1).operation, "authorize-browser");
  await page.locator("[data-live-more]").click();
  await page.getByRole("button", { name: /Unlock methods/ }).click();
  await page.getByRole("heading", { name: "How test-workspace is unlocked" }).waitFor();
  await page.getByRole("button", { name: "Manage Browser fixture password", exact: true }).click();
  await page.getByRole("button", { name: "Remove this method", exact: true }).click();
  await page.getByRole("heading", { name: "This is the only everyday way to unlock test-workspace" }).waitFor();
  assert.equal(await page.locator('[data-method-action="confirm-remove"]').count(), 0);
  await page.getByRole("button", { name: "Close", exact: true }).click();
  for (const stage of ["operate", "observe"]) {
    const beforeMethods = methods.size, beforeProofs = mutations.length;
    const held = holdManagementResponse(stage);
    try {
      await page.getByRole("button", { name: "Manage Browser fixture password", exact: true }).click();
      await page.getByRole("button", { name: "Change encryption password", exact: true }).click();
      await page.getByLabel("Current password", { exact: true }).fill(password);
      await page.getByLabel("New password", { exact: true }).fill("fixture abandoned replacement password");
      await page.getByRole("button", { name: "Change and check it", exact: true }).click();
      const operationID = await held.ready;
      const aborted = page.waitForEvent("requestfailed", { predicate: request => {
        const url = new URL(request.url());
        return stage === "operate" ? request.method() === "POST" && url.pathname === "/api/security/v1/operations"
          : url.pathname === `/api/security/v1/operations/${operationID}`;
      }, timeout: 5000 });
      await page.getByRole("dialog").getByRole("button", { name: "Close", exact: true }).click();
      await aborted;
      await page.getByRole("heading", { name: "Check whether the request completed" }).waitFor();
      held.release();
      await page.getByRole("button", { name: "Check result", exact: true }).click();
      await page.getByRole("heading", { name: "How test-workspace is unlocked" }).waitFor();
      assert.equal(methods.size, beforeMethods, `cancel during ${stage} cannot prepare or store a candidate`);
      assert.equal(mutations.length, beforeProofs + 1, "status observation never replays the proof");
      assert.equal(mutations.at(-1).id, operationID, "observation follows the original public operation ID");
    } finally { held.release(); heldManagementResponse = undefined; }
  }
  await page.getByRole("button", { name: "Manage Browser fixture password", exact: true }).click();
  await page.getByRole("button", { name: "Change encryption password", exact: true }).click();
  const oldPasswordMethod = [...methods.values()].find(method => method.kind === "password" && method.status === "active");
  const replacementPassword = "fixture replacement encryption password";
  await page.getByLabel("Current password", { exact: true }).fill(password);
  await page.getByLabel("New password", { exact: true }).fill(replacementPassword);
  await page.getByRole("button", { name: "Change and check it", exact: true }).click();
  await page.getByRole("heading", { name: "Check the new password" }).waitFor();
  assert.equal(oldPasswordMethod.status, "active");
  await page.getByLabel("New password", { exact: true }).fill("wrong saved method fixture");
  await page.getByRole("button", { name: "Check saved method", exact: true }).click();
  await page.getByRole("alert").waitFor();
  assert.equal(oldPasswordMethod.status, "active");
  await page.getByLabel("New password", { exact: true }).fill(replacementPassword);
  await page.getByRole("button", { name: "Check saved method", exact: true }).click();
  await page.getByRole("button", { name: "Replace “Browser fixture password”", exact: true }).click();
  await page.getByRole("button", { name: "Browser fixture password", exact: true }).click();
  await page.getByLabel("Encryption password", { exact: true }).fill(password);
  await page.getByRole("button", { name: "Verify and continue", exact: true }).click();
  await page.getByRole("heading", { name: "Your workspace is running" }).waitFor();
  assert.equal(oldPasswordMethod.status, "removed");
  assert.equal(grant, false, "replacement of the granting method disconnects this browser");
  password = replacementPassword;
  await page.getByRole("button", { name: "Use recovery key", exact: true }).click();
  await page.getByRole("textbox", { name: /^Recovery key\b/ }).fill(kit.recoveryKey);
  await page.getByRole("button", { name: "Verify this browser", exact: true }).click();
  await page.getByRole("heading", { name: "Set up a way to unlock from now on" }).waitFor();
  assert.equal(await page.locator("[data-live-more]").count(), 0, "recovery follow-up precedes the private workspace");
  assert.match(await page.getByRole("region", { name: "Recovery follow-up" }).innerText(), /1 everyday method can unlock test-workspace/);
  const oldRecovery = [...methods.values()].find(method => method.kind === "recovery" && method.status === "active");
  await page.getByRole("button", { name: "Get a new one", exact: true }).click();
  await page.getByRole("button", { name: "Encryption password", exact: true }).click();
  await page.getByLabel("Encryption password", { exact: true }).fill(password);
  await page.getByRole("button", { name: "Verify and continue", exact: true }).click();
  await page.getByRole("heading", { name: "Save your new recovery key" }).waitFor();
  const newKey = await page.getByLabel("New recovery key", { exact: true }).innerText();
  assert.notEqual(newKey, kit.recoveryKey);
  assert.equal(oldRecovery.status, "active");
  await page.getByRole("button", { name: "I have saved it — check it works", exact: true }).click();
  await page.getByLabel("Recovery key", { exact: true }).fill(newKey);
  await page.getByRole("button", { name: "Check saved method", exact: true }).click();
  await page.getByRole("button", { name: "Replace “Recovery key”", exact: true }).click();
  await page.getByRole("button", { name: "Encryption password", exact: true }).click();
  await page.getByLabel("Encryption password", { exact: true }).fill(password);
  await page.getByRole("button", { name: "Verify and continue", exact: true }).click();
  await page.getByRole("heading", { name: "Your workspace is running" }).waitFor();
  assert.equal(oldRecovery.status, "removed");
  await page.getByRole("button", { name: "Use encryption password", exact: true }).click();
  await page.getByLabel("Encryption password", { exact: true }).fill(password);
  await page.getByRole("button", { name: "Verify this browser", exact: true }).click();
  await page.locator("[data-live-more]").waitFor();
  assert.equal(mutations.filter(action => ["initialize", "unlock"].includes(action.operation)).length, nativeBefore, "method management does not remount storage");
  loseLockResponse = true;
  await page.locator("[data-live-more]").click();
  await page.getByRole("button", { name: "Lock workspace Stops your sessions and locks storage" }).click();
  assert.doesNotMatch(await page.locator("main").innerText(), /Two sessions|rewrite-scheduler|flaky-test-hunt|40s|11m/, "lock confirmation uses aggregate facts when no session observations exist");
  await page.getByRole("button", { name: "Stop sessions and lock", exact: true }).click();
  await page.getByLabel("Encryption password", { exact: true }).fill(password);
  await page.getByRole("button", { name: "Confirm lock", exact: true }).click();
  await page.getByRole("heading", { name: "Could not confirm whether your workspace locked" }).waitFor();
  const locks = mutations.filter(action => action.operation === "lock").length;
  await page.reload({ waitUntil: "domcontentloaded" });
  await page.getByRole("heading", { name: "Could not confirm whether your workspace locked" }).waitFor();
  assert.equal(mutations.filter(action => action.operation === "lock").length, locks, "reload observes the operation without replay");
  const id = mutations.at(-1).id; storage = "closed"; runtime = "stopped";
  const pendingKey = "deck.security.pending-operation.v1";
  for (const result of [
    { status: "unconfirmed", observedLocked: true, stateLocked: true },
    { status: "complete", observedLocked: false, stateLocked: true },
    { status: "complete", observedLocked: true, stateLocked: false },
  ]) {
    locked = result.stateLocked;
    operations.set(id, { id, status: result.status, storage, runtime, locked: result.observedLocked });
    const previous = await page.locator("[data-security-runtime]").elementHandle();
    await page.getByRole("button", { name: "Reconnect and check", exact: true }).click();
    await page.waitForFunction(element => !element.isConnected, previous);
    await previous.dispose();
    await page.getByRole("heading", { name: "Could not confirm whether your workspace locked" }).waitFor();
    assert.equal(await page.evaluate(key => JSON.parse(sessionStorage.getItem(key)).id, pendingKey), id, "closed/stopped observations do not clear an unqualified lock operation");
    await page.reload({ waitUntil: "domcontentloaded" });
    await page.getByRole("heading", { name: "Could not confirm whether your workspace locked" }).waitFor();
    assert.equal(mutations.filter(action => action.operation === "lock").length, locks, "unconfirmed lock reconciliation never replays the operation");
    assert.equal(await page.locator("[data-live-more]").count(), 0, "uncertain lock state never admits the private workspace");
  }
  locked = true;
  operations.set(id, { id, status: "complete", storage, runtime, locked });
  await page.getByRole("button", { name: "Reconnect and check", exact: true }).click();
  await page.getByText("Storage is closed and the workspace has stopped.", { exact: true }).waitFor();
  assert.doesNotMatch(await page.locator("main").innerText(), /Storage closed at|Two sessions|owner@/, "lock confirmation does not invent receipt metadata");
  assert.equal(mutations.filter(action => action.operation === "lock").length, locks);
  assert.equal(await page.evaluate(key => sessionStorage.getItem(key), pendingKey), null, "only a qualified complete lock clears the pending reference");
  locked = false;
  const beforeUnqualifiedReload = privateRequests.length;
  await page.reload({ waitUntil: "domcontentloaded" });
  await page.locator('[data-security-runtime="unknown"]').waitFor();
  assert.equal(await page.getByRole("heading", { name: "test-workspace is locked", exact: true }).count(), 0, "closed/stopped without backend lock qualification is not a locked screen");
  assert.equal(privateRequests.length, beforeUnqualifiedReload);
  locked = true;
  await page.reload({ waitUntil: "domcontentloaded" });
  await page.getByRole("heading", { name: "test-workspace is locked", exact: true }).waitFor();
  const prior = privateRequests.length;
  marker = "unsupported";
  await page.reload({ waitUntil: "domcontentloaded" });
  await page.getByText("Deck cannot verify authentication. Reload to check again.", { exact: true }).waitFor();
  assert.equal(privateRequests.length, prior, "unknown security protocol cannot take the legacy path");
  marker = null;
  const legacyProjects = page.waitForResponse(response => new URL(response.url()).pathname === "/api/projects" && response.status() === 200);
  await page.reload({ waitUntil: "domcontentloaded" });
  await page.locator("[data-live-more]").waitFor();
  await legacyProjects;
  assert.ok(privateRequests.length > prior, "a recognized legacy auth service with no security marker retains its existing behavior");
  marker = "v1"; grant = true; storage = "open"; runtime = "running"; locked = false;
  await page.reload({ waitUntil: "domcontentloaded" });
  await page.locator("[data-live-more]").click();
  await page.getByRole("button", { name: "Sign out Leaves your sessions running", exact: true }).click();
  await page.getByRole("button", { name: "Sign in with Google", exact: true }).waitFor();
  assert.equal(storage, "open", "sign-out leaves native storage open");
  assert.equal(runtime, "running", "sign-out leaves sessions running");
  assert.equal(grant, false, "sign-out revokes this browser's access");
  phase = "invitation-required"; policy = "optional"; protection = "undecided"; storage = "closed"; runtime = "stopped"; locked = false; commitment = undefined; methods.clear();
  const invitation = token();
  await page.goto(`${origin}/#invite=${invitation}`, { waitUntil: "domcontentloaded" });
  await page.locator('[data-onboarding="welcome"]').waitFor();
  assert.equal(new URL(page.url()).hash, "", "the invitation fragment is removed before Google navigation");
  assert.equal(await page.evaluate(value => [...Object.values(sessionStorage), ...Object.values(localStorage)].some(entry => entry.includes(value)), invitation), false);
  await page.getByRole("button", { name: "Sign in with Google", exact: true }).click();
  await page.getByRole("button", { name: "Enable workspace encryption", exact: true }).click();
  await page.getByRole("heading", { name: "Choose how to unlock test-workspace" }).waitFor();
  assert.equal(handedOffInvitation, undefined);
  assert.equal(await page.evaluate(() => document.cookie.includes("fixture_invitation")), false);
  await page.evaluate(() => {
    Object.defineProperty(navigator.credentials, "create", { configurable: true, value: async () => {
      const id = new Uint8Array([1, 2, 3]);
      return { type: "public-key", id: "AQID", rawId: id.buffer, authenticatorAttachment: "platform",
        response: { clientDataJSON: new Uint8Array([1]).buffer, attestationObject: new Uint8Array([2]).buffer, getTransports: () => ["internal"] },
        getClientExtensionResults: () => ({ prf: { enabled: false } }) };
    } });
  });
  const operationsBeforeUnsupported = mutations.length;
  await page.getByRole("button", { name: "Choose a passkey", exact: true }).click();
  await page.locator('[data-enrollment="passkey-incompatible"]').waitFor();
  assert.equal(mutations.length, operationsBeforeUnsupported, "an incompatible passkey cannot prove or initialize anything");
  assert.equal([...methods.values()].some(method => method.envelope), false);
  assert.equal(protection, "encrypted", "unsupported PRF cannot undo the committed protection choice");
  assert.equal(await page.getByRole("button", { name: "Continue without Deck encryption", exact: true }).count(), 0);

  // A fresh optional installation uses the same production entry, but never
  // prepares encryption methods or pretends a method-backed grant exists.
  authenticated = false; grant = false; phase = "invitation-required"; policy = "optional"; protection = "undecided";
  storage = "unknown"; runtime = "stopped"; plainOperationId = undefined; methods.clear(); operations.clear();
  const beforePlain = privateRequests.length;
  await page.goto(origin, { waitUntil: "domcontentloaded" });
  await page.locator('[data-onboarding="needlink"]').waitFor();
  assert.equal(await page.getByRole("button", { name: "Sign in with Google", exact: true }).count(), 0);
  onboardingAvailable = false;
  await page.reload({ waitUntil: "domcontentloaded" });
  await page.locator('[data-onboarding="unreachable"]').waitFor();
  onboardingAvailable = true;
  await page.goto(`${origin}/#setup=${token()}`, { waitUntil: "domcontentloaded" });
  await page.locator('[data-onboarding="welcome"]').waitFor();
  await page.getByRole("button", { name: "Sign in with Google", exact: true }).click();
  await page.locator('[data-onboarding="encryption"][data-policy="optional"]').waitFor();
  losePlainResponse = true;
  await page.getByRole("button", { name: "Continue without Deck encryption", exact: true }).click();
  await page.locator('[data-onboarding="unconfirmed"]').waitFor();
  const startsAfterLoss = plainStarts;
  const unknownBeforeCheck = await page.locator('[data-onboarding="unconfirmed"]').elementHandle();
  await page.getByRole("button", { name: "Reconnect and check", exact: true }).click();
  await page.waitForFunction(element => !element.isConnected, unknownBeforeCheck);
  await page.locator('[data-onboarding="unconfirmed"]').waitFor();
  assert.equal(plainStarts, startsAfterLoss, "read-only observation cannot replay a lost plain start");
  assert.equal(privateRequests.length, beforePlain, "uncertain setup never mounts private data");
  phase = "ready"; storage = "unencrypted"; runtime = "running"; losePlainResponse = false;
  operations.set(plainOperationId, { id: plainOperationId, status: "succeeded", storage, runtime, locked: false });
  await page.getByRole("button", { name: "Reconnect and check", exact: true }).click();
  await page.getByRole("heading", { name: "Workspace ready", exact: true }).waitFor();
  await page.getByRole("button", { name: "Open test-workspace", exact: true }).click();
  await page.locator("[data-live-more]").click();
  assert.equal(await page.locator("[data-workspace-lock], [data-workspace-methods]").count(), 0, "plain workspace exposes neither lock nor method management");
  assert.equal(methods.size, 0); assert.equal(grant, false);
  await page.getByRole("button", { name: "Sign out", exact: true }).click();
  await page.locator('[data-onboarding="signin"]').waitFor();
  assert.equal(runtime, "running", "plain sign-out keeps sessions running");
  await page.getByRole("button", { name: "Sign in with Google", exact: true }).click();
  await page.locator("[data-live-more]").waitFor();
  assert.equal(plainStarts, startsAfterLoss, "returning owner does not restart setup or the runtime");
  runtime = "stopped";
  await page.reload({ waitUntil: "domcontentloaded" });
  await page.locator('[data-security-runtime="plain-stopped"]').waitFor();
  await page.getByRole("button", { name: "Start workspace", exact: true }).click();
  await page.locator("[data-live-more]").waitFor();
  assert.deepEqual(plainStartBodies.at(-1), {}, "post-setup restart asks the service to reserve a new operation");
  assert.equal(plainStarts, startsAfterLoss + 1);
  wrongOwner = true;
  await page.reload({ waitUntil: "domcontentloaded" });
  await page.locator('[data-onboarding="notowner"]').waitFor();
  await page.getByRole("button", { name: "Use a different account", exact: true }).click();
  await page.locator("[data-live-more]").waitFor();

  authenticated = false; phase = "invitation-required"; protection = "undecided"; policy = "unavailable";
  plainOperationId = undefined; runtime = "stopped"; storage = "unknown"; operations.clear();
  await page.goto(`${origin}/#invitation=${token()}`, { waitUntil: "domcontentloaded" });
  await page.getByRole("button", { name: "Sign in with Google", exact: true }).click();
  await page.locator('[data-onboarding="encryption"][data-policy="unavailable"]').waitFor();
  assert.equal(await page.getByRole("button", { name: "Enable workspace encryption", exact: true }).count(), 0);
  await page.getByRole("button", { name: "Finish setup", exact: true }).click();
  await page.getByRole("heading", { name: "Workspace ready", exact: true }).waitFor();
  await page.getByRole("button", { name: "Open test-workspace", exact: true }).click();
  await page.locator("[data-live-more]").waitFor();
  assert.equal(methods.size, 0); assert.equal(grant, false);
  assert.equal(csrfFailures, 0);
  assert.deepEqual(serverErrors, []);
  assert.deepEqual(errors, []);
  await context.close();
  console.log("PASS: production generic onboarding; required/optional/unavailable policy; setup-link and owner gates; password validation; real encrypted enrollment/resume/proofs, recovery and rotations; plain lost-response observation, sign-out/return and reboot startup; no plain lock/method controls; qualified lock uncertainty and unchanged legacy entry. Native runtime/Google/WebAuthn not qualified.");
} finally {
  await browser?.close();
  server.closeAllConnections();
  await new Promise(resolveClose => server.close(resolveClose));
}
