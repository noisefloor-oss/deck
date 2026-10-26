// Typed HTTP wrappers over the real endpoints. The client does not cache or
// reinterpret responses; the server owns recorded state.
import { lastTerminalGeometry } from "../terminal/last-geometry.ts";
import type { ConsoleSettings, SidebarLayout, TutorialPreference, TutorialRecord } from "./settings.ts";
import type {
  Project,
  GitIdentityProfile,
  GitIdentityCatalog,
  ProjectWriteResult,
  Inspection,
  CloneJob,
  CloneStartRequest,
  CreateProjectOptions,
  ProjectGitIdentitySelection,
  ProjectDirectoryListing,
  StarterCommitResult,
} from "./projects.ts";
import type {
  SessionSummary,
  SessionDetail,
  RecoverResult,
  HarnessUsage,
  HarnessUsageReport,
} from "./sessions.ts";
import type {
  TaskmasterInitResult,
  TaskmasterInfo,
  TmTask,
  TaskmasterTasks,
  TmRun,
  AddTaskInput,
  AddTaskResult,
  Lane,
  StartLaneInput,
  TaskDoneInput,
  TaskDoneResult,
  LaneReview,
  LaneApiClient,
} from "./tasks.ts";
import type { ChatLedger, ChatIndex, ChatsStatus } from "./chats.ts";
import {
  type FileListing,
  type FileView,
  type FileUploadResult,
  UPLOAD_CONFLICT_REASONS,
  type UploadConflictReason,
  type UploadConflictInfo,
  type UploadConsent,
  type UploadTransportOptions,
  type FileApiClient,
} from "./files.ts";
import type { AssetDeleteFile, AssetDeleteResult, AssetListing } from "./assets.ts";
import type { ProjectPreviewEntry, ProjectPreviewSettings, ProjectPreviewCheck } from "./project-preview.ts";
import type { ApiClient } from "./contracts.ts";
import {
  ApiError,
  type AuthMode,
  csrfToken,
  readJSON,
  AUTHORIZATION_LOSS_CODES,
  fetchWithAuthorization,
} from "./http.ts";

export * from "./settings.ts";
export * from "./projects.ts";
export * from "./sessions.ts";
export * from "./tasks.ts";
export * from "./chats.ts";
export * from "./files.ts";
export * from "./assets.ts";
export * from "./project-preview.ts";
export * from "./contracts.ts";
export { ApiError, csrfToken, loginWithPassword, AUTHORIZATION_LOSS_CODES, fetchWithAuthorization } from "./http.ts";
export type { AuthMode } from "./http.ts";

// decodeUploadFailure turns an upload endpoint failure into the ApiError
// both transports reject with. A 409 is the collision question: the
// conflict envelope becomes the ask any Replace consent must quote,
// while a 409 answering with anything else stays the opaque error it
// arrived as — never an inferred ask. Every other status decodes through
// the two envelopes readJSON knows, so an error reads identically
// whichever transport carried it. body is the already-parsed JSON value
// (null when the body was not JSON).
function decodeUploadFailure(status: number, statusText: string, body: unknown): ApiError {
  const envelope = (body ?? {}) as {
    error?: unknown;
    // A flat top-level code is how the control-plane refusals answer
    // ({"code":"unlock-required"}); surfacing it keeps the upload's
    // ApiError saying what the authorization-loss read keyed on.
    code?: unknown;
    message?: unknown;
    reason?: unknown;
    existing?: { name?: unknown; dir?: unknown; size?: unknown; mtime?: unknown; version?: unknown };
  };
  if (status === 409) {
    let code = "unknown";
    let message = statusText;
    let conflict: UploadConflictInfo | null = null;
    if (envelope.error === "conflict") {
      code = "conflict";
      message = typeof envelope.message === "string" ? envelope.message : message;
      const e = envelope.existing;
      const reason = envelope.reason;
      // A reason the client does not know is not an ask: guessing
      // "exists" would hand the dialog a Replace button for a refusal it
      // did not understand, so the error stays plain and fails closed.
      if (e !== undefined && e !== null && typeof e.name === "string" && typeof e.version === "string" && e.version !== ""
          && typeof reason === "string" && (UPLOAD_CONFLICT_REASONS as readonly string[]).includes(reason)) {
        conflict = {
          reason: reason as UploadConflictReason,
          message,
          existing: {
            name: e.name,
            dir: e.dir === true,
            size: typeof e.size === "number" ? e.size : 0,
            mtime: typeof e.mtime === "string" ? e.mtime : "",
            version: e.version,
          },
        };
      }
    }
    return new ApiError(status, code, message, conflict);
  }
  let code = "unknown";
  let message = statusText;
  if (typeof envelope.error === "string") {
    code = envelope.error;
    message = typeof envelope.message === "string" ? envelope.message : message;
  } else if (envelope.error !== null && typeof envelope.error === "object") {
    const nested = envelope.error as { code?: unknown; message?: unknown };
    code = typeof nested.code === "string" ? nested.code : code;
    message = typeof nested.message === "string" ? nested.message : message;
  } else if (typeof envelope.code === "string") {
    code = envelope.code;
  }
  return new ApiError(status, code, message);
}

// uploadConflictError adapts the fetch path's Response to the shared
// failure decoder. A body that is not the conflict envelope falls back to
// the plain error shape rather than inventing an ask the server never
// made.
async function uploadConflictError(resp: Response): Promise<ApiError> {
  let body: unknown = null;
  try {
    body = JSON.parse(await resp.text());
  } catch {
    // a non-JSON conflict body is still an error, just an opaque one
  }
  return decodeUploadFailure(resp.status, resp.statusText, body);
}

// uploadRequestPath builds the upload endpoint's request path: the
// project id and the project-relative destination encoded separately,
// the consent as the query the server's collision check reads. One
// builder serves both transports so their requests cannot drift apart.
function uploadRequestPath(projectID: string, destPath: string, consent?: UploadConsent): string {
  const consentQuery = consent === undefined
    ? ""
    : `&replace=1&expected_version=${encodeURIComponent(consent.version)}`;
  return `/api/projects/${encodeURIComponent(projectID)}/files?path=${encodeURIComponent(destPath)}${consentQuery}`;
}

// abortError is the rejection a user cancellation produces: named
// like the DOMException fetch rejects with when its signal fires, and
// deliberately not an ApiError — an aborted request has no server answer,
// so nothing downstream may read the outcome as one.
function abortError(): Error {
  const error = new Error("upload aborted before the server answered");
  error.name = "AbortError";
  return error;
}

// subscribeChanges opens one server change feed. The stream only reports
// what it observes while connected, and the server baselines a fresh
// connection from the tree as it is now. Anything that changed during a
// dropped connection is therefore already folded into that baseline and
// will never arrive as an event, so a reopen is itself a change: re-read
// once. The browser's own reconnect fires this without any gesture from
// the caller.
function subscribeChanges(url: string, event: string, onChange: () => void, onClosed?: () => void): () => void {
  const events = new EventSource(url);
  events.addEventListener(event, onChange);
  let connected = false;
  events.addEventListener("open", () => {
    if (connected) onChange();
    connected = true;
  });
  // A refusal the browser will not retry (a non-stream answer, an HTTP
  // error) leaves the stream closed for good; the caller polls instead.
  events.addEventListener("error", () => {
    if (events.readyState === EventSource.CLOSED) onClosed?.();
  });
  return () => events.close();
}

export class HttpApiClient implements ApiClient, FileApiClient, LaneApiClient {
  private cookie: () => string;
  workspaceSecurity: "v1" | null = null;
  onAuthorizationLost?: () => void;
  // Aborted the moment logout begins, so no in-flight request - above all a
  // session renewal whose response would set a fresh cookie - can outlive
  // the logout. A failed logout re-arms it: the session may still be live
  // and ordinary requests must go on working.
  private logoutAbort = new AbortController();

  private request(input: RequestInfo | URL, init?: RequestInit): Promise<Response> {
    // Every request rides the logout signal; a caller's own signal is
    // composed with it so EITHER one cancels the request.
    const caller = init?.signal;
    const signal = caller === undefined || caller === null ? this.logoutAbort.signal
      : AbortSignal.any([caller, this.logoutAbort.signal]);
    return fetchWithAuthorization(input, { ...init, signal }, this.onAuthorizationLost);
  }

  constructor(cookie: () => string = () =>
    typeof document === "undefined" ? "" : document.cookie) {
    this.cookie = cookie;
  }

  private mutationHeaders(json = false): Headers {
    const headers = new Headers();
    if (json) {
      headers.set("Content-Type", "application/json");
    }
    const token = csrfToken(this.cookie());
    if (token !== null) {
      headers.set("X-CSRF-Token", token);
    }
    return headers;
  }

  private get(path: string, signal?: AbortSignal): Promise<Response> {
    // Session/process/outbox reads are live control-plane facts. Never let an
    // HTTP cache replay an earlier reconciliation result or authenticated
    // response after logout.
    return this.request(path, { credentials: "same-origin", cache: "no-store", signal });
  }

  subscribeProjectFileEvents(projectID: string, path: string, onChange: () => void): () => void {
    return subscribeChanges(`/api/projects/${encodeURIComponent(projectID)}/files/events?path=${encodeURIComponent(path)}`, "files-changed", onChange);
  }

  subscribeProjectAssetEvents(projectID: string, onChange: () => void, onClosed?: () => void): () => void {
    return subscribeChanges(`/api/projects/${encodeURIComponent(projectID)}/assets/events`, "assets-changed", onChange, onClosed);
  }

  async requireSession(): Promise<AuthMode> {
    const response = await this.get("/auth/session");
    const mode = response.headers.get("X-Noisedeck-Auth-Mode");
    const security = response.headers.get("X-Deck-Workspace-Security");
    if (security !== null && security !== "v1") throw new ApiError(response.status, "unknown_security_protocol", "server identified an unsupported workspace security protocol");
    this.workspaceSecurity = security;
    if (mode !== "none" && mode !== "password" && mode !== "oidc" &&
        mode !== "builtin" && mode !== "trusted_proxy") {
      throw new ApiError(response.status, "unknown_auth_mode", "server did not identify its authentication mode");
    }
    try {
      await readJSON<void>(response);
    } catch (cause) {
      if (cause instanceof ApiError && cause.status === 401) {
        throw new ApiError(cause.status, `unauthorized_${mode}`, cause.message);
      }
      throw cause;
    }
    return mode;
  }

  // Console settings are read fresh whenever the menu opens: the
  // backend is the authority, and a cached copy would let the UI show a
  // permission-skipping state the next launch does not actually use.
  async getSettings(): Promise<ConsoleSettings> {
    return await readJSON<ConsoleSettings>(await this.get("/api/settings"));
  }

  async saveSettings(next: ConsoleSettings): Promise<ConsoleSettings> {
    return await readJSON<ConsoleSettings>(
      await this.request("/api/settings", {
        method: "PUT",
        headers: this.mutationHeaders(true),
        credentials: "same-origin",
        cache: "no-store",
        body: JSON.stringify(next),
      }),
    );
  }

  async getLayout(): Promise<SidebarLayout | null> {
    const resp = await this.get("/api/settings/layout");
    // A 404 means the user has never saved a layout: the sidebar keeps
    // its default expansion rather than treating a missing row as an explicit
    // all-collapsed state.
    if (resp.status === 404) {
      return null;
    }
    return await readJSON<SidebarLayout>(resp);
  }

  async saveLayout(next: SidebarLayout): Promise<SidebarLayout> {
    return await readJSON<SidebarLayout>(
      await this.request("/api/settings/layout", {
        method: "PUT",
        headers: this.mutationHeaders(true),
        credentials: "same-origin",
        cache: "no-store",
        body: JSON.stringify(next),
      }),
    );
  }

  async getLastSession(): Promise<string | null> {
    const resp = await this.get("/api/settings/last-session");
    if (resp.status === 404) return null;
    const doc = await readJSON<{ session_id: string }>(resp);
    return doc.session_id === "" ? null : doc.session_id;
  }

  async saveLastSession(id: string | null): Promise<void> {
    await this.request("/api/settings/last-session", {
      method: "PUT",
      headers: this.mutationHeaders(true),
      credentials: "same-origin",
      cache: "no-store",
      body: JSON.stringify({ session_id: id ?? "" }),
    });
  }

  async getTutorial(signal?: AbortSignal): Promise<TutorialRecord | null> {
    const resp = await this.get("/api/settings/tutorial", signal);
    // A 404 means no record was ever written: the tutorial's first-use
    // behavior, not an error.
    if (resp.status === 404) {
      return null;
    }
    return await readJSON<TutorialRecord>(resp);
  }

  async saveTutorial(next: TutorialPreference, signal?: AbortSignal): Promise<TutorialRecord> {
    return await readJSON<TutorialRecord>(
      await this.request("/api/settings/tutorial", {
        method: "PUT",
        headers: this.mutationHeaders(true),
        credentials: "same-origin",
        cache: "no-store",
        body: JSON.stringify(next),
        signal,
      }),
    );
  }

  async logout(): Promise<void> {
    // Abort every other in-flight request BEFORE the logout POST leaves: a
    // session renewal answering after this would set a still-valid cookie
    // over the cleared jar - the server's logout epoch is the authority
    // that rejects it, and this makes the browser stop racing it. The POST
    // itself is exempt (it carries the CSRF token that makes logout real).
    this.logoutAbort.abort();
    try {
      await fetchWithAuthorization("/auth/logout", {
        method: "POST",
        headers: this.mutationHeaders(),
        credentials: "same-origin",
        cache: "no-store",
      }, this.onAuthorizationLost).then(readJSON<void>);
    } catch (cause) {
      // The session may still be live: re-arm so ordinary requests work.
      this.logoutAbort = new AbortController();
      throw cause;
    }
  }

  async listProjects(): Promise<Project[]> {
    // Go's encoding/json represents a nil slice as null. Treat that one
    // server-produced empty-list shape as empty so a fresh database can reach
    // onboarding; do not let null leak into the sidebar's array contract.
    return (await readJSON<Project[] | null>(await this.get("/api/projects"))) ?? [];
  }

  // The harnesses this image can launch. Deck ships in more than one image
  // from one bundle, so the picker cannot know them from the code it was
  // served — only the process that resolved the executables knows.
  async listProviders(): Promise<readonly string[]> {
    const body = await readJSON<{ providers?: readonly string[] | null }>(
      await this.get("/api/providers"));
    return body.providers ?? [];
  }

  // Recorded launches per harness, for ranking the picker. Both scopes come
  // back in one response; a project with no history is an empty list, not an
  // error. Go's encoding/json can render an empty slice as null, so each
  // scope is defaulted rather than trusted to be an array.
  async getHarnessUsage(projectID?: string): Promise<HarnessUsageReport> {
    const query = projectID === undefined || projectID === ""
      ? ""
      : `?project=${encodeURIComponent(projectID)}`;
    const body = await readJSON<{
      global?: readonly HarnessUsage[] | null;
      project?: readonly HarnessUsage[] | null;
    }>(await this.get(`/api/harness-usage${query}`));
    return { global: body.global ?? [], project: body.project ?? [] };
  }

  async listGitIdentities(): Promise<GitIdentityCatalog> {
    return readJSON<GitIdentityCatalog>(await this.get("/api/git-identities"));
  }

  async createGitIdentity(name: string, email: string): Promise<GitIdentityProfile> {
    return readJSON<GitIdentityProfile>(await this.request("/api/git-identities", {
      method: "POST", headers: this.mutationHeaders(true), credentials: "same-origin",
      cache: "no-store", body: JSON.stringify({ name, email }),
    }));
  }

  async updateGitIdentity(profileID: string, expectedRevision: number, name: string, email: string): Promise<GitIdentityProfile> {
    return readJSON<GitIdentityProfile>(await this.request(`/api/git-identities/${encodeURIComponent(profileID)}`, {
      method: "PUT", headers: this.mutationHeaders(true), credentials: "same-origin",
      cache: "no-store", body: JSON.stringify({ name, email, expected_revision: expectedRevision }),
    }));
  }

  async setDefaultGitIdentity(profileID: string, expectedCatalogRevision: number, expectedProfileRevision: number): Promise<GitIdentityCatalog> {
    return readJSON<GitIdentityCatalog>(await this.request("/api/git-identities/default", {
      method: "PUT", headers: this.mutationHeaders(true), credentials: "same-origin",
      cache: "no-store", body: JSON.stringify({
        profile_id: profileID,
        expected_revision: expectedCatalogRevision,
        expected_profile_revision: expectedProfileRevision,
      }),
    }));
  }

  async deleteGitIdentity(profileID: string, expectedRevision: number, catalogRevision: number, replacement?: GitIdentityProfile): Promise<void> {
    await readJSON<void>(await this.request(`/api/git-identities/${encodeURIComponent(profileID)}`, {
      method: "DELETE", headers: this.mutationHeaders(true), credentials: "same-origin",
      cache: "no-store", body: JSON.stringify({
        expected_revision: expectedRevision,
        catalog_revision: catalogRevision,
        replacement_default_id: replacement?.id ?? "",
        expected_replacement_revision: replacement?.revision ?? 0,
      }),
    }));
  }

  async listProjectDirectories(path?: string): Promise<ProjectDirectoryListing> {
    const query = path === undefined ? "" : `?path=${encodeURIComponent(path)}`;
    return readJSON<ProjectDirectoryListing>(
      await this.get(`/api/project-directories${query}`),
    );
  }

  async createProjectDirectory(path: string, name: string): Promise<{ path: string }> {
    return readJSON<{ path: string }>(
      await this.request("/api/project-directories/mkdir", {
        method: "POST",
        headers: this.mutationHeaders(true),
        credentials: "same-origin",
        cache: "no-store",
        body: JSON.stringify({ path, name }),
      }),
    );
  }

  async createProject(name: string, path: string, profile: GitIdentityProfile | null, options: CreateProjectOptions = {}): Promise<ProjectWriteResult> {
    return readJSON<ProjectWriteResult>(
      await this.request("/api/projects", {
        method: "POST",
        headers: this.mutationHeaders(true),
        body: JSON.stringify({
          name,
          path,
          plain: options.plain === true,
          ...(profile === null ? {} : {
            git_identity_profile_id: profile.id,
            expected_git_identity_revision: profile.revision,
          }),
        }),
      }),
    );
  }

  async inspectProject(input: string, base = ""): Promise<Inspection> {
    const query = new URLSearchParams({ input });
    if (base !== "") query.set("base", base);
    return readJSON<Inspection>(await this.get(`/api/projects/inspect?${query.toString()}`));
  }

  async startClone(request: CloneStartRequest): Promise<CloneJob> {
    return readJSON<CloneJob>(
      await this.request("/api/projects/clone", {
        method: "POST",
        headers: this.mutationHeaders(true),
        credentials: "same-origin",
        body: JSON.stringify({
          url: request.url,
          ...(request.githubAccount === undefined ? {} : { github_account: request.githubAccount, github_repository_id: request.githubRepositoryID }),
          destination: request.destination,
          name: request.name,
          ...(request.profile === null ? {} : {
            git_identity_profile_id: request.profile.id,
            expected_git_identity_revision: request.profile.revision,
          }),
        }),
      }),
    );
  }

  async listClones(): Promise<CloneJob[]> {
    return readJSON<CloneJob[]>(await this.get("/api/project-clones"));
  }

  async getClone(id: string): Promise<CloneJob> {
    return readJSON<CloneJob>(await this.get(`/api/project-clones/${encodeURIComponent(id)}`));
  }

  async cancelClone(id: string): Promise<CloneJob> {
    return readJSON<CloneJob>(await this.request(`/api/project-clones/${encodeURIComponent(id)}/cancel`, {
      method: "POST", headers: this.mutationHeaders(), credentials: "same-origin",
    }));
  }

  async resumeClone(id: string): Promise<CloneJob> {
    return readJSON<CloneJob>(await this.request(`/api/project-clones/${encodeURIComponent(id)}/resume`, {
      method: "POST", headers: this.mutationHeaders(), credentials: "same-origin",
    }));
  }

  async discardClone(id: string): Promise<void> {
    const resp = await this.request(`/api/project-clones/${encodeURIComponent(id)}`, {
      method: "DELETE", headers: this.mutationHeaders(), credentials: "same-origin",
    });
    if (!resp.ok) await readJSON<never>(resp);
  }

  async setProjectGitIdentity(projectID: string, selection: ProjectGitIdentitySelection): Promise<ProjectWriteResult> {
    return readJSON<ProjectWriteResult>(await this.request(`/api/projects/${encodeURIComponent(projectID)}/git-identity`, {
      method: "PUT", headers: this.mutationHeaders(true), credentials: "same-origin",
      cache: "no-store", body: JSON.stringify({
        profile_id: selection.profile_id,
        expected_profile_revision: selection.expected_profile_revision,
        expected_project_setup_revision: selection.expected_project_setup_revision,
      }),
    }));
  }

  // deleteProject unregisters a project from the console. The backend
  // is the authority on what that means: a soft removal that never
  // touches the directory on disk and stops the project's live
  // sessions first; 409 when a session cannot be stopped
  // (session_stop_failed) or launches concurrently (live_sessions).
  async deleteProject(projectID: string): Promise<void> {
    await readJSON<void>(
      await this.request(`/api/projects/${encodeURIComponent(projectID)}`, {
        method: "DELETE",
        headers: this.mutationHeaders(),
        credentials: "same-origin",
        cache: "no-store",
      }),
    );
  }

  // renameProject records the user's display name for a project,
  // mirroring renameSession. Display metadata only: the registered
  // path and the directory on disk never change. An empty name resets to
  // the directory basename.
  async renameProject(projectID: string, name: string): Promise<void> {
    await readJSON<void>(
      await this.request(`/api/projects/${encodeURIComponent(projectID)}/rename`, {
        method: "POST",
        headers: this.mutationHeaders(true),
        credentials: "same-origin",
        cache: "no-store",
        body: JSON.stringify({ name }),
      }),
    );
  }

  // projectPreview requests entry to a project's registered app preview.
  // A CSRF-protected POST with no body: the backend verifies the project
  // and answers the entry URL plus its one-use ticket, or a 404 whose
  // message says previews are not configured for the project — the same
  // sentence the UI shows when it cannot ask at all.
  async projectPreview(projectID: string): Promise<ProjectPreviewEntry> {
    return readJSON<ProjectPreviewEntry>(
      await this.request(`/api/projects/${encodeURIComponent(projectID)}/preview`, {
        method: "POST",
        headers: this.mutationHeaders(),
        credentials: "same-origin",
        cache: "no-store",
      }),
    );
  }

  async getProjectPreviewSettings(projectID: string, signal?: AbortSignal): Promise<ProjectPreviewSettings> {
    return readJSON<ProjectPreviewSettings>(await this.get(`/api/projects/${encodeURIComponent(projectID)}/preview/settings`, signal));
  }

  async enableProjectPreview(projectID: string): Promise<ProjectPreviewSettings> {
    return this.changeProjectPreview(projectID, "PUT");
  }

  async disableProjectPreview(projectID: string): Promise<ProjectPreviewSettings> {
    return this.changeProjectPreview(projectID, "DELETE");
  }

  private async changeProjectPreview(projectID: string, method: "PUT" | "DELETE"): Promise<ProjectPreviewSettings> {
    return readJSON<ProjectPreviewSettings>(await this.request(`/api/projects/${encodeURIComponent(projectID)}/preview/settings`, {
      method, headers: this.mutationHeaders(), credentials: "same-origin", cache: "no-store",
    }));
  }

  async checkProjectPreview(projectID: string): Promise<ProjectPreviewCheck> {
    return readJSON<ProjectPreviewCheck>(await this.request(`/api/projects/${encodeURIComponent(projectID)}/preview/check`, {
      method: "POST", headers: this.mutationHeaders(), credentials: "same-origin", cache: "no-store",
    }));
  }

  async listSessions(projectID: string): Promise<SessionSummary[]> {
    return (await readJSON<SessionSummary[] | null>(
      await this.get(`/api/projects/${encodeURIComponent(projectID)}/sessions`),
    )) ?? [];
  }

  async recoverSession(sessionID: string): Promise<RecoverResult> {
    return readJSON<RecoverResult>(
      await this.request(`/api/sessions/${encodeURIComponent(sessionID)}/recover`, {
        method: "POST",
        headers: this.mutationHeaders(),
      }),
    );
  }

  async sessionScreen(sessionID: string): Promise<string | null> {
    const res = await this.request(`/api/sessions/${encodeURIComponent(sessionID)}/screen`);
    if (res.status === 404) return null;
    if (!res.ok) {
      // readJSON decodes the error envelope and throws the ApiError; a
      // success would be text, which is read below instead.
      await readJSON<never>(res);
    }
    return res.text();
  }

  async createSession(projectID: string, provider: string): Promise<SessionSummary> {
    // The last fitted geometry rides along so the session's window is born
    // at this screen's size rather than tmux's 80x24; the
    // server treats it as an optional bounded hint.
    const geometry = lastTerminalGeometry();
    return readJSON<SessionSummary>(
      await this.request(`/api/projects/${encodeURIComponent(projectID)}/sessions`, {
        method: "POST",
        headers: this.mutationHeaders(true),
        body: JSON.stringify(geometry === null
          ? { provider }
          : { provider, cols: geometry.cols, rows: geometry.rows }),
      }),
    );
  }

  async getTaskmasterInfo(projectID: string): Promise<TaskmasterInfo> {
    return readJSON<TaskmasterInfo>(
      await this.get(`/api/projects/${encodeURIComponent(projectID)}/taskmaster`),
    );
  }

  async initializeTaskmaster(projectID: string): Promise<TaskmasterInitResult> {
    return readJSON<TaskmasterInitResult>(
      await this.request(`/api/projects/${encodeURIComponent(projectID)}/taskmaster`, {
        method: "POST",
        headers: this.mutationHeaders(false),
      }),
    );
  }

  async listTaskmasterTasks(projectID: string): Promise<TaskmasterTasks> {
    return readJSON<TaskmasterTasks>(
      await this.get(`/api/projects/${encodeURIComponent(projectID)}/taskmaster/tasks`),
    );
  }

  async getTaskmasterTask(projectID: string, taskID: string): Promise<TmTask> {
    return readJSON<TmTask>(
      await this.get(`/api/projects/${encodeURIComponent(projectID)}/taskmaster/tasks/${encodeURIComponent(taskID)}`),
    );
  }

  async addTaskmasterTask(projectID: string, input: AddTaskInput): Promise<AddTaskResult> {
    return readJSON<AddTaskResult>(
      await this.request(`/api/projects/${encodeURIComponent(projectID)}/taskmaster/tasks`, {
        method: "POST",
        headers: this.mutationHeaders(true),
        credentials: "same-origin",
        cache: "no-store",
        body: JSON.stringify(input),
      }),
    );
  }

  async listLanes(projectID: string): Promise<Lane[]> {
    const body = await readJSON<{ lanes: Lane[] }>(await this.get(`/api/projects/${encodeURIComponent(projectID)}/lanes`));
    return body.lanes;
  }

  async startLane(projectID: string, input: StartLaneInput): Promise<Lane> {
    return readJSON<Lane>(
      await this.request(`/api/projects/${encodeURIComponent(projectID)}/lanes`, {
        method: "POST",
        headers: this.mutationHeaders(true),
        credentials: "same-origin",
        cache: "no-store",
        body: JSON.stringify(input),
      }),
    );
  }

  async markTaskDone(projectID: string, taskID: string, input: TaskDoneInput): Promise<TaskDoneResult> {
    const body: { tag: string; revision?: number; stop_sessions?: boolean } = { tag: input.tag };
    if (input.revision !== undefined) body.revision = input.revision;
    if (input.stopSessions === true) body.stop_sessions = true;
    return readJSON<TaskDoneResult>(
      await this.request(`/api/projects/${encodeURIComponent(projectID)}/taskmaster/tasks/${encodeURIComponent(taskID)}/done`, {
        method: "POST",
        headers: this.mutationHeaders(true),
        credentials: "same-origin",
        cache: "no-store",
        body: JSON.stringify(body),
      }),
    );
  }

  async requestLaneReview(projectID: string, tag: string, task: number, agent: string): Promise<Lane> {
    return readJSON<Lane>(
      await this.request(`/api/projects/${encodeURIComponent(projectID)}/lanes/${encodeURIComponent(tag)}/${task}/review`, {
        method: "POST",
        headers: this.mutationHeaders(true),
        credentials: "same-origin",
        cache: "no-store",
        body: JSON.stringify({ agent }),
      }),
    );
  }

  async getLaneReview(projectID: string, tag: string, task: number): Promise<LaneReview> {
    return readJSON<LaneReview>(
      await this.get(`/api/projects/${encodeURIComponent(projectID)}/lanes/${encodeURIComponent(tag)}/${task}/review`),
    );
  }

  async listTaskmasterPRDs(projectID: string): Promise<string[]> {
    return readJSON<string[]>(
      await this.get(`/api/projects/${encodeURIComponent(projectID)}/taskmaster/prds`),
    );
  }

  async uploadPrd(projectID: string, name: string, content: string, replace: boolean): Promise<void> {
    await readJSON<unknown>(
      await this.request(`/api/projects/${encodeURIComponent(projectID)}/taskmaster/prds`, {
        method: "POST",
        headers: this.mutationHeaders(true),
        credentials: "same-origin",
        cache: "no-store",
        body: JSON.stringify({ name, content, replace }),
      }),
    );
  }

  async generateTasks(projectID: string, prd: string, numTasks?: number, harness?: string): Promise<TmRun> {
    return readJSON<TmRun>(
      await this.request(`/api/projects/${encodeURIComponent(projectID)}/taskmaster/generate`, {
        method: "POST",
        headers: this.mutationHeaders(true),
        body: JSON.stringify({ prd, numTasks, harness }),
      }),
    );
  }

  async commitStarter(projectID: string, paths: readonly string[]): Promise<StarterCommitResult> {
    return readJSON<StarterCommitResult>(
      await this.request(`/api/projects/${encodeURIComponent(projectID)}/starter-commit`, {
        method: "POST",
        headers: this.mutationHeaders(true),
        credentials: "same-origin",
        cache: "no-store",
        body: JSON.stringify({ paths }),
      }),
    );
  }

  async getSessionChats(sessionID: string): Promise<ChatLedger> {
    return readJSON<ChatLedger>(
      await this.get(`/api/sessions/${encodeURIComponent(sessionID)}/chats`),
    );
  }

  async listChats(): Promise<ChatIndex> {
    return readJSON<ChatIndex>(await this.get("/api/chats"));
  }

  async getChatsStatus(signal?: AbortSignal): Promise<ChatsStatus> {
    return readJSON<ChatsStatus>(await this.get("/api/chats/status", signal));
  }

  async listTaskmasterRuns(projectID: string): Promise<TmRun[]> {
    return readJSON<TmRun[]>(
      await this.get(`/api/projects/${encodeURIComponent(projectID)}/taskmaster/runs`),
    );
  }

  async getTaskmasterRun(projectID: string, runID: string): Promise<TmRun> {
    return readJSON<TmRun>(
      await this.get(
        `/api/projects/${encodeURIComponent(projectID)}/taskmaster/runs/${encodeURIComponent(runID)}`,
      ),
    );
  }

  async listProjectFiles(projectID: string, path: string): Promise<FileListing> {
    return readJSON<FileListing>(
      await this.get(
        `/api/projects/${encodeURIComponent(projectID)}/files?path=${encodeURIComponent(path)}`,
      ),
    );
  }

  async viewProjectFile(projectID: string, path: string): Promise<FileView> {
    return readJSON<FileView>(
      await this.get(
        `/api/projects/${encodeURIComponent(projectID)}/files/view?path=${encodeURIComponent(path)}`,
      ),
    );
  }

  async listProjectAssets(projectID: string): Promise<AssetListing> {
    return readJSON<AssetListing>(
      await this.get(`/api/projects/${encodeURIComponent(projectID)}/assets`),
    );
  }

  async deleteProjectAssets(projectID: string, files: AssetDeleteFile[]): Promise<AssetDeleteResult[]> {
    const body = await readJSON<{ results: AssetDeleteResult[] }>(
      await this.request(`/api/projects/${encodeURIComponent(projectID)}/assets/delete`, {
        method: "POST",
        headers: this.mutationHeaders(true),
        credentials: "same-origin",
        cache: "no-store",
        body: JSON.stringify({ files: files.map(({ path, version }) => ({ path, version })) }),
      }),
    );
    return body.results;
  }

  // projectFileDownloadUrl is a URL, not a request: the download is a GET
  // the browser performs itself, so the response's Content-Disposition
  // names and saves the file. Reading the body into JavaScript to hand
  // back a blob would buffer the whole file in the tab for nothing.
  projectFileDownloadUrl(projectID: string, path: string): string {
    return `/api/projects/${encodeURIComponent(projectID)}/files/download?path=${encodeURIComponent(path)}`;
  }

  // projectFileImageUrl is a URL for an <img> to load: the browser fetches
  // and decodes it itself, and the server refuses anything that is not an
  // allowlisted image format.
  projectFileImageUrl(projectID: string, path: string, version?: string): string {
    const url = `/api/projects/${encodeURIComponent(projectID)}/files/image?path=${encodeURIComponent(path)}`;
    return version === undefined ? url : `${url}&v=${encodeURIComponent(version)}`;
  }

  // checkProjectFileImage asks the image route for its answer without the
  // body. An <img> that failed to load cannot see the status it got, and
  // the viewer words a removed file, the size cap and bytes that are not
  // an image differently.
  async checkProjectFileImage(projectID: string, path: string): Promise<void> {
    const resp = await this.request(this.projectFileImageUrl(projectID, path), {
      method: "HEAD", credentials: "same-origin", cache: "no-store",
    });
    if (!resp.ok) throw new ApiError(resp.status, "unknown", resp.statusText);
  }

  // uploadProjectFile POSTs one file's raw bytes. The destination is the
  // `path` query parameter — the name the user's directory and the
  // chosen file's basename decide — never a multipart filename, which the
  // handler deliberately ignores. A raw body also keeps the request one
  // stream per file with no framing to bound.
  //
  // The content type is stated rather than inherited from the Blob: the
  // handler switches to multipart parsing on a multipart content type,
  // and the picker reports whatever type the operating system attaches to
  // the chosen file.
  //
  // A 409 is the ask, not a failure: it decodes into an ApiError carrying
  // the conflict so the caller can put the question to the user.
  // Consent, when given, is the version that conflict reported — never a
  // bare "yes", so a request cannot destroy a file the user never saw.
  //
  // A caller that passes no options keeps the fetch request it always
  // had; a caller that wants to watch the bytes flows through the XHR
  // below, whose request is byte-identical on the wire.
  async uploadProjectFile(
    projectID: string,
    destPath: string,
    body: Blob,
    consent?: UploadConsent,
    options?: UploadTransportOptions,
  ): Promise<FileUploadResult> {
    if (options !== undefined) {
      return this.uploadProjectFileViaXHR(projectID, destPath, body, consent, options);
    }
    const headers = this.mutationHeaders();
    headers.set("Content-Type", "application/octet-stream");
    const resp = await this.request(
      uploadRequestPath(projectID, destPath, consent),
      {
        method: "POST",
        headers,
        credentials: "same-origin",
        cache: "no-store",
        body,
      },
    );
    if (resp.status === 409) {
      throw await uploadConflictError(resp);
    }
    return readJSON<FileUploadResult>(resp);
  }

  // uploadProjectFileViaXHR is the measured-upload path. XHR, because it
  // is the one browser transport that reports upload-side progress the
  // browser itself measured — routing through fetch would leave the
  // progress UI inventing bytes. The wire contract is identical to the
  // fetch path: same path and consent query, same CSRF header, same raw
  // octet-stream Blob, cookies on the same-origin request.
  //
  // Its two observations are upload-side facts only: onProgress counts
  // bytes handed to the socket, onSent says the last byte left the
  // browser. Neither is completion — the response alone decides whether
  // the file was placed — so a request that dies after onSent rejects
  // without an ApiError: the outcome is unknown (the server may have
  // committed), and nothing here retries it on its own.
  private uploadProjectFileViaXHR(
    projectID: string,
    destPath: string,
    body: Blob,
    consent: UploadConsent | undefined,
    options: UploadTransportOptions,
  ): Promise<FileUploadResult> {
    if (typeof XMLHttpRequest === "undefined") {
      // Failing loudly beats silently reporting no progress through a
      // transport that cannot measure any.
      return Promise.reject(new Error("upload progress needs XMLHttpRequest, which this environment does not provide"));
    }
    // A signal already aborted never dispatches: there is no request to
    // cancel, so none is made — no headers, no body, nothing sent.
    if (options.signal?.aborted) {
      return Promise.reject(abortError());
    }
    const headers = this.mutationHeaders();
    headers.set("Content-Type", "application/octet-stream");
    const url = uploadRequestPath(projectID, destPath, consent);
    const signal = options.signal;
    return new Promise<FileUploadResult>((resolve, reject) => {
      const xhr = new XMLHttpRequest();
      let settled = false;
      const onAbort = (): void => {
        xhr.abort();
        // An XHR aborted before send() fires no abort event of its own
        // (it never left the unsent/opened state), so the rejection comes
        // from here either way.
        fail(abortError());
      };
      const cleanup = (): void => {
        settled = true;
        xhr.upload.onprogress = null;
        xhr.upload.onload = null;
        xhr.onload = null;
        xhr.onerror = null;
        xhr.onabort = null;
        signal?.removeEventListener("abort", onAbort);
      };
      const fail = (error: unknown): void => {
        if (settled) return;
        cleanup();
        reject(error);
      };
      xhr.open("POST", url);
      // withCredentials states the fetch path's credentials:"same-origin":
      // cookies ride this request because it is same-origin by construction.
      xhr.withCredentials = true;
      headers.forEach((value, key) => xhr.setRequestHeader(key, value));
      xhr.upload.onprogress = (event: ProgressEvent) => {
        if (settled || options.onProgress === undefined) return;
        // total is the browser's own count for the body it was handed;
        // a zero means "not computable", and the Blob's size is exact.
        options.onProgress(event.loaded, event.total > 0 ? event.total : body.size);
      };
      xhr.upload.onload = () => {
        if (settled || options.onSent === undefined) return;
        options.onSent();
      };
      xhr.onload = () => {
        if (settled) return;
        let parsed: unknown = null;
        let parseError: unknown = null;
        try {
          parsed = JSON.parse(xhr.responseText);
        } catch (cause) {
          parseError = cause;
        }
        if (xhr.status < 200 || xhr.status >= 300) {
          const error = decodeUploadFailure(xhr.status, xhr.statusText, parsed);
          // The same authorization-loss read the fetch wrapper performs:
          // a 401, or a 403 whose body names a pending ceremony.
          if (this.onAuthorizationLost !== undefined) {
            if (xhr.status === 401) {
              this.onAuthorizationLost();
            } else if (xhr.status === 403) {
              const code = (parsed as { code?: unknown } | null)?.code;
              if (typeof code === "string" && AUTHORIZATION_LOSS_CODES.includes(code)) {
                this.onAuthorizationLost();
              }
            }
          }
          fail(error);
          return;
        }
        if (parseError !== null) {
          fail(parseError instanceof Error ? parseError : new Error("upload response was not JSON"));
          return;
        }
        if (parsed === null || typeof parsed !== "object" || !("path" in parsed) || !("size" in parsed)
            || parsed.path !== destPath || parsed.size !== body.size) {
          fail(new Error("upload response did not confirm the requested file"));
          return;
        }
        cleanup();
        resolve({ path: destPath, size: body.size });
      };
      xhr.onerror = () => fail(new Error("upload failed without a server answer"));
      xhr.onabort = () => fail(abortError());
      signal?.addEventListener("abort", onAbort, { once: true });
      try {
        if (signal?.aborted) onAbort();
        else xhr.send(body);
      } catch (error) {
        fail(error);
      }
    });
  }

  async getSession(sessionID: string): Promise<SessionDetail> {
    return readJSON<SessionDetail>(
      await this.get(`/api/sessions/${encodeURIComponent(sessionID)}`),
    );
  }

  async deleteSession(sessionID: string): Promise<void> {
    await readJSON<void>(
      await this.request(`/api/sessions/${encodeURIComponent(sessionID)}`, {
        method: "DELETE",
        headers: this.mutationHeaders(),
        credentials: "same-origin",
        cache: "no-store",
      }),
    );
  }

  async renameSession(sessionID: string, name: string): Promise<void> {
    await readJSON<void>(
      await this.request(`/api/sessions/${encodeURIComponent(sessionID)}/rename`, {
        method: "POST",
        headers: this.mutationHeaders(true),
        credentials: "same-origin",
        cache: "no-store",
        body: JSON.stringify({ name }),
      }),
    );
  }

  async setSessionColor(sessionID: string, color: string | null): Promise<void> {
    await readJSON<void>(
      await this.request(`/api/sessions/${encodeURIComponent(sessionID)}/color`, {
        method: "POST",
        headers: this.mutationHeaders(true),
        credentials: "same-origin",
        cache: "no-store",
        body: JSON.stringify({ color }),
      }),
    );
  }

}
