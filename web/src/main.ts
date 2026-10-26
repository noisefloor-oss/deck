// Production entry point: compose the server-backed workspace. The browser
// owns only selection, view lifetimes, and its live attachment/lease; every
// project, session, process, and outbox fact comes from the backend.
import "./styles/app.css";
import "@fonts";
import "@xterm/xterm/css/xterm.css";
import { ApiError, type AuthMode } from "./api/http.ts";
import { HttpApiClient } from "./api/client.ts";
import {
  claimTabController,
  type ControllerClaim,
  type ControllerLockOptions,
} from "./app/controller.ts";
import { LiveWorkspace } from "./app/live.ts";
import { bindLoginActions, loginHtml, mountSetup } from "./app/login.ts";
import { isStableReleaseBuild, watchReleaseNotice } from "./app/release-notice.ts";
import { documentBuildID, fetchBuildID, VersionWatcher } from "./app/version-watch.ts";
import { mountMethodManagement } from "./app/security/management-controller.ts";
import { WorkspaceSecurity } from "./app/security/coordinator.ts";
import { currentTheme } from "./app/theme.ts";
import { applyFace, currentFace } from "./app/face.ts";

// data-theme rides on <html>, never on a container: Blueprint's composite
// tokens (--hairline, --ring, --ring-overlay, --ring-accent) are substituted
// where they are defined, so on an inner element they freeze against the
// light ramp. It is set before the workspace mounts so no frame renders
// against the wrong ramp.
document.documentElement.dataset.theme = currentTheme();

// The face rides on <html> for the same reason and is set in the same frame,
// so the first paint uses the selected typeface's trims rather than swapping
// the scale under the reader a moment later.
applyFace(currentFace());

const root = document.querySelector<HTMLDivElement>("#app");
if (!root) {
  throw new Error("missing #app mount point");
}

let workspace: LiveWorkspace | null = null;
let security: WorkspaceSecurity | null = null;
let controller: ControllerClaim | null = null;
let controllerRelease: Promise<void> = Promise.resolve();
let mountAbort: AbortController | null = null;
let mountGeneration = 0;
let versionWatcher: VersionWatcher | null = null;
let releaseNoticeCleanup: (() => void) | null = null;

const renderMountState = (message: string, error = false) => {
  root.replaceChildren();
  const shell = document.createElement("div");
  shell.className = "grid h-screen w-screen place-items-center bg-surface px-6 text-center text-body-sm";
  const text = document.createElement("p");
  text.className = error ? "max-w-lg text-error" : "text-text-tertiary";
  text.textContent = message;
  shell.append(text);
  root.append(shell);
};

const renderLogin = (mode: AuthMode) => {
  root.replaceChildren();
  const shell = document.createElement("div");
  shell.innerHTML = loginHtml({ mode, state: "signed-out" });
  // The ceremony leaves the page, so the click removes the Google action
  // immediately rather than waiting for a response: there is no response
  // to wait for, and an action still standing invites a second navigation
  // that abandons the first transaction.
  bindLoginActions(shell, mode, mount);
  root.append(shell);
};

const detachWorkspace = (): void => {
  releaseNoticeCleanup?.(); releaseNoticeCleanup = null;
  workspace?.dispose(); workspace = null;
  versionWatcher?.dispose(); versionWatcher = null;
  const releasing = controller; controller = null;
  controllerRelease = releasing?.close().catch(() => undefined) ?? Promise.resolve();
  root.replaceChildren();
};

const mount = async () => {
  if (window.location.pathname === "/setup") { root.inert = false; await mountSetup(root); return; }
  const generation = ++mountGeneration;
  security?.dispose(); security = null; detachWorkspace();
  mountAbort?.abort(new Error("controller claim superseded"));
  const abort = new AbortController(); mountAbort = abort;
  root.inert = true;
  renderMountState("Checking authentication…");
  const api = new HttpApiClient();
  let authMode: AuthMode = "oidc";
  let authenticated = true;
  try { authMode = await api.requireSession(); }
  catch (cause) {
    if (generation !== mountGeneration) return;
    root.inert = false;
    if (cause instanceof ApiError && cause.code.startsWith("unauthorized_")) {
      authenticated = false;
      authMode = cause.code.slice("unauthorized_".length) as AuthMode;
    } else {
      renderMountState("Deck cannot verify authentication. Reload to check again.", true);
      return;
    }
  }
  if (generation !== mountGeneration) return;
  if (api.workspaceSecurity === "v1") {
    if (authMode !== "oidc") { renderMountState("The workspace did not identify its Google sign-in service.", true); return; }
    security = new WorkspaceSecurity(root, api, {
      admit: () => admitWorkspace(api, authMode, generation, abort),
      detach: detachWorkspace,
      manageMethods: context => mountMethodManagement(context.root, context),
    });
    api.onAuthorizationLost = () => security?.authorizationLost();
    await security.start(authenticated);
    return;
  }
  // Only the recognized legacy auth response with no security marker takes
  // this path. Errors and unknown markers never fall back to private APIs.
  if (!authenticated) { renderLogin(authMode); return; }
  await admitWorkspace(api, authMode, generation, abort);
};

const admitWorkspace = async (api: HttpApiClient, authMode: AuthMode, generation: number, abort: AbortController): Promise<void> => {
  renderMountState("Restoring the browser controller…");
  let claim: ControllerClaim | null = null;
  try {
    await controllerRelease;
    if (!("locks" in navigator) || navigator.locks === undefined) {
      throw new Error("This browser does not support the Web Locks API required for safe controller isolation.");
    }
    claim = await claimTabController(
      {
        getItem: (key) => window.sessionStorage.getItem(key),
        setItem: (key, value) => window.sessionStorage.setItem(key, value),
      },
      (name, options, callback) => navigator.locks.request(
        name,
        options as LockOptions,
        (lock) => callback(lock),
      ),
      () => crypto.randomUUID(),
      abort.signal,
    );
  } catch (cause) {
    if (generation !== mountGeneration) return;
    const message = cause instanceof Error ? cause.message : String(cause);
    root.inert = false;
    renderMountState(`Deck cannot safely attach: ${message}`, true);
    return;
  }
  if (generation !== mountGeneration) {
    await claim.close();
    return;
  }
  controller = claim;
  if (security && !await security.confirmAdmission()) return;
  root.inert = false;
  workspace = new LiveWorkspace(root, api, claim.id, authMode, {
    getItem: (key) => window.localStorage.getItem(key),
    setItem: (key, value) => window.localStorage.setItem(key, value),
    removeItem: (key) => window.localStorage.removeItem(key),
  }, security ? {
    ...(security.encrypted ? { lock: () => security?.lock(), manageMethods: () => { void security?.manageMethods(); } } : {}),
    authenticationLost: () => security?.authorizationLost(),
  } : undefined);
  const loadedBuildID = documentBuildID();
  const reload = root.querySelector<HTMLButtonElement>("[data-version-reload]");
  if (loadedBuildID !== null && reload !== null) {
    versionWatcher?.dispose();
    versionWatcher = new VersionWatcher(loadedBuildID, () => fetchBuildID(api.onAuthorizationLost), () => {
      reload.hidden = false;
    });
    reload.addEventListener("click", () => window.location.reload());
    versionWatcher.start();
  }
  if (isStableReleaseBuild(loadedBuildID)) {
    releaseNoticeCleanup = watchReleaseNotice(root, api.onAuthorizationLost);
  }
  void workspace.start();
};

window.addEventListener("pagehide", () => {
  ++mountGeneration;
  mountAbort?.abort(new Error("page hidden"));
  mountAbort = null;
  security?.dispose();
  security = null;
  detachWorkspace();
  root.inert = true;
  renderMountState("Deck is paused while this page is hidden.");
});
window.addEventListener("pageshow", (event) => {
  if (event.persisted) void mount();
});
window.addEventListener("hashchange", () => {
  const fragment = new URLSearchParams(location.hash.slice(1));
  if (fragment.has("invite") || fragment.has("invitation") || fragment.has("setup")) void mount();
});
await mount();
