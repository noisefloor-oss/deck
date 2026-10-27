// Stable project, session, task-board, and files-panel data for the visual
// preview's deterministic screenshots and interaction probes.
import type { AssetDeleteFile, AssetDeleteResult, AssetListing } from "../api/assets.ts";
import type { ChatIndex, ChatLedger } from "../api/chats.ts";
import type { ApiClient } from "../api/contracts.ts";
import type {
  FileApiClient,
  FileEntry,
  FileListing,
  FileUploadResult,
  FileView,
  UploadConsent,
} from "../api/files.ts";
import type {
  Lane,
  LaneApiClient,
  LaneReview,
  TaskDoneResult,
  TaskmasterTasks,
  TmTask,
  TmRun,
} from "../api/tasks.ts";
import type {
  GitIdentityCatalog,
  GitIdentityProfile,
  Project,
  StarterCommitResult,
  Inspection,
  ParentRepository,
  CloneJob,
  CloneStartRequest,
} from "../api/projects.ts";
import type { HarnessUsageReport, SessionDetail, RecoverResult, SessionSummary } from "../api/sessions.ts";
import type { SidebarLayout, TutorialPreference, TutorialRecord } from "../api/settings.ts";
import { ApiError } from "../api/http.ts";
import type { ConsoleSettings } from "../api/settings.ts";
import { fixtureUploadAnswer } from "./fixture-files.ts";
// Never inlined: the viewer appends a version query to the image URL,
// which a data: URL would take as part of its bytes.
import homeImage from "./fixture-images/home.png?url&no-inline";
import homeMobileImage from "./fixture-images/home-mobile.png?url&no-inline";
import iconImage from "./fixture-images/icon-512.png?url&no-inline";
import partialImage from "./fixture-images/capture-partial.png?url&no-inline";
import { fixtureAssetDeleteAnswer, fixtureAssetListing, type FixtureAssetVariant } from "./fixture-assets.ts";
import { FIXTURE_PROVIDERS } from "./fixture-providers.ts";
import { fixtureChatIndex, fixtureChatLedger } from "./fixture-chats.ts";

export const fixtureProject: Project = {
  id: "proj-1",
  name: "field-notes",
  canonical_path: "/workspace/field-notes",
  git_author_name: "Deck Author",
  git_author_email: "deck@example.invalid",
  git_identity_profile_id: "identity-1",
  git_identity_profile_revision: 1,
  git_identity_setup_revision: 1,
  last_used_at: "2026-08-01T09:13:00.000Z",
};

export const fixtureProjects: Project[] = [
  fixtureProject,
  { id: "proj-2", name: "audio-player", canonical_path: "/workspace/audio-player", git_identity_setup_revision: 1, last_used_at: "t" },
  { id: "proj-3", name: "council-chat", canonical_path: "/workspace/model-groupchat", git_identity_setup_revision: 1, last_used_at: "t" },
  { id: "proj-4", name: "noisefloor-site", canonical_path: "/workspace/noisefloor-site", git_identity_setup_revision: 1, last_used_at: "t" },
  { id: "proj-5", name: "payments-api", canonical_path: "/workspace/payments-api", git_identity_setup_revision: 1, last_used_at: "t" },
  { id: "proj-6", name: "slop", canonical_path: "/workspace/slop", git_identity_setup_revision: 1, last_used_at: "t" },
];

export const fixtureGitIdentity: GitIdentityProfile = {
  id: "identity-1", name: "Deck Author", email: "deck@example.invalid", revision: 1,
  created_at: "2026-08-01T00:00:00.000Z", updated_at: "2026-08-01T00:00:00.000Z",
};

// The workspace mirrors the reference design:
// field-notes open with five sessions, the five sibling projects behind
// it. Ages are calibrated to the capture loop's frozen clock
// (2026-08-02T12:00:00Z) so every tick renders exactly as designed.
// Only verified-running sessions may render as rows, so the
// busy codex row is carried as the closest passive fact — output
// seconds ago — and the sibling projects' sessions stay
// presentation-honest (ended, unavailable, inactive) rather than idle
// rows.
// The proj-1 providers span five representative tile identities. The
// complete provider set is visible in the new-session picker.
const sinceMs = (ms: number): string => new Date(Date.now() - ms).toISOString();
const runningFacts = {
  recovery_capability: "none",
  recovery_state: "unavailable",
  presentation_state: "running",
} as const;
const endedFacts = {
  recovery_capability: "none",
  recovery_state: "unavailable",
  presentation_state: "ended",
} as const;
const unavailableFacts = {
  recovery_capability: "none",
  recovery_state: "unavailable",
  presentation_state: "attachment_unavailable",
} as const;

export const fixtureSessions: Record<string, SessionSummary[]> = {
  "proj-1": [
    { id: "sess-1", project_id: "proj-1", provider: "claude", display_name: "", status: "running", generation: 5, created_at: "2026-08-02T06:00:00.000Z", updated_at: "2026-08-02T06:00:00.000Z", last_output_at: sinceMs(42_000), ...runningFacts },
    { id: "sess-2", project_id: "proj-1", provider: "codex", display_name: "", status: "running", generation: 3, created_at: "t", updated_at: "t", last_output_at: sinceMs(15_000), ...runningFacts },
    { id: "sess-3", project_id: "proj-1", provider: "kimi", display_name: "", status: "running", generation: 1, created_at: "2026-08-02T11:59:30.000Z", updated_at: "2026-08-02T11:59:30.000Z", last_output_at: sinceMs(12 * 60_000), ...runningFacts },
    { id: "sess-9", project_id: "proj-1", provider: "opencode", display_name: "", status: "running", generation: 1, created_at: "t", updated_at: "t", last_output_at: sinceMs(3 * 3_600_000), ...runningFacts },
    { id: "sess-10", project_id: "proj-1", provider: "shell", display_name: "", status: "running", generation: 1, created_at: "t", updated_at: "t", last_output_at: sinceMs(2 * 3_600_000), ...runningFacts },
  ],
  "proj-2": [
    { id: "sess-4", project_id: "proj-2", provider: "claude", display_name: "", status: "exited", generation: 1, created_at: "t", updated_at: "t", last_output_at: sinceMs(2 * 86_400_000), ...endedFacts },
  ],
  "proj-3": [
    { id: "sess-5", project_id: "proj-3", provider: "codex", display_name: "", status: "exited", generation: 1, created_at: "t", updated_at: "t", last_output_at: sinceMs(4 * 86_400_000), ...endedFacts },
  ],
  "proj-4": [
    { id: "sess-6", project_id: "proj-4", provider: "claude", display_name: "", status: "interrupted", generation: 1, created_at: "t", updated_at: "t", last_output_at: sinceMs(86_400_000), ...unavailableFacts },
  ],
  "proj-5": [
    { id: "sess-7", project_id: "proj-5", provider: "claude", display_name: "web-relay", status: "interrupted", generation: 6, created_at: "t", updated_at: "t", last_output_at: sinceMs(6 * 3_600_000), recovery_capability: "resume", recovery_state: "unavailable", presentation_state: "inactive" },
  ],
  "proj-6": [
    { id: "sess-8", project_id: "proj-6", provider: "codex", display_name: "", status: "interrupted", generation: 1, created_at: "t", updated_at: "t", last_output_at: sinceMs(9 * 86_400_000), recovery_capability: "relaunch", recovery_state: "unavailable", presentation_state: "inactive" },
  ],
};

export const fixtureSessionDetail: SessionDetail = {
  id: "sess-1",
  project_id: "proj-1",
  provider: "claude",
  display_name: "",
  status: "running",
  generation: 5,
  pane_pid: 848,
  created_at: "2026-08-02T11:57:00.000Z",
  updated_at: "2026-08-02T11:57:00.000Z",
  ...runningFacts,
};

// The reference board: TO DO 44, IN PROGRESS 2, DONE 106. The visible
// card set is transcribed literally; the
// counts count the board's notional backlog, so filler rows pad
// each column to it. Filler ids keep the named cards first under the
// board's default ID sort.
export const fixtureTasksTotal = 152;

// Cards the reference board does not contain, kept because they exercise
// presentation its data never reaches: markdown in the detail
// dialog (a fence, inline code, a list and a link), and the worst-case
// card whose two-line title carries a deps row and a progress row at
// once, so the card grows past the 132px floor rather than slicing a row.
// They stay out of the default board, which matches the reference card for
// card; a preview state selects them when that coverage is wanted.
export const fixtureBoardEdgeCases = [
  {
    id: "31",
    title: "Implement body retention cleanup",
    status: "pending",
    priority: "low",
    dependencies: ["4", "16", "28"],
    details: [
      "Retention runs on a schedule and removes **bodies only** — the record and its hash outlive it.",
      "",
      "Run it by hand with:",
      "",
      "```sh",
      "noisetask list --status pending --json | jq '.tasks[].id'",
      "```",
      "",
      "Steps:",
      "",
      "- read the retention window from `config.yaml`",
      "- delete bodies older than it",
      "- leave `created_at` untouched",
      "",
      "See [the PRD](https://example.com/prd) for the exact window.",
    ].join("\n"),
  },
  {
    id: "42",
    title: "Reconcile provider identity capture across restarts and validate the resume matrix end to end",
    status: "in-progress",
    priority: "med",
    dependencies: ["7"],
    subtasks_done: 1,
    subtasks_total: 3,
  },
] as const;

export const fixtureTasks: TaskmasterTasks = {
  master: {
    tasks: [
      { id: "30.2", title: "Rewrite vertical-slice tests for direct-terminal MVP", status: "pending", priority: "high", dependencies: ["1"] },
      { id: "30.4", title: "Rewrite failure-matrix tests for direct-terminal MVP", status: "pending", priority: "high", dependencies: ["2", "3"] },
      { id: "30.5", title: "First-release all-providers gate (PRD 13.2) — direct terminal conformance", status: "pending", priority: "high", dependencies: ["2", "4", "41"] },
      { id: "30.6", title: "Complete first-release system gate (PRD 13.2) — infrastructure and auth", status: "pending", priority: "high", dependencies: ["4", "5", "10", "24", "27"] },
      { id: "32", title: "Implement health and diagnostics endpoints", status: "pending", priority: "med", dependencies: ["8", "13"] },
      { id: "36", title: "Implement database backup and restore", status: "pending", priority: "med", dependencies: ["3", "4"] },
      ...Array.from({ length: 38 }, (_, i) => ({
        id: String(1000 + i),
        title: `Backlog item ${1000 + i}`,
        status: "pending",
        priority: "low",
        dependencies: [],
      })),
      { id: "7", title: "Execute phase 0 provider behavior spikes", status: "in-progress", priority: "high", dependencies: ["5", "6"], subtasks_done: 1, subtasks_total: 2 },
      { id: "30", title: "Implement end-to-end acceptance test suite", status: "in-progress", priority: "high", dependencies: ["7", "12", "13", "14", "16", "19", "26", "29"], subtasks_done: 2, subtasks_total: 6 },
      { id: "1", title: "Initialize Go project structure", status: "done", priority: "high", dependencies: [] },
      { id: "2", title: "Initialize TypeScript frontend project", status: "done", priority: "high", dependencies: [] },
      { id: "3", title: "Design and implement SQLite schema", status: "done", priority: "high", dependencies: ["1"] },
      { id: "4", title: "Implement database access layer", status: "done", priority: "high", dependencies: ["3"] },
      { id: "5", title: "Implement tmux client wrapper", status: "done", priority: "high", dependencies: ["1"] },
      { id: "6", title: "Implement provider descriptors", status: "done", priority: "high", dependencies: ["5"] },
      ...Array.from({ length: 100 }, (_, i) => ({
        id: String(2000 + i),
        title: `Shipped item ${2000 + i}`,
        status: "done",
        priority: "low",
        dependencies: [],
      })),
    ],
  },
};

export const fixtureRun: TmRun = {
  id: "run-1",
  project_id: "proj-1",
  argv: ["/usr/local/bin/noise", "tasks", "generate", "/workspace/field-notes/.tasks/docs/PRD.md"],
  prd: "PRD.md",
  status: "exited",
  exit_code: 0,
  started_at: "2026-08-01T09:12:00.000Z",
  finished_at: "2026-08-01T09:12:40.000Z",
  duration_ms: 40000,
};

// The project's files, keyed by the directory the listing endpoint would
// be asked for ("" is the project root). Entries carry the same shape the
// server serves, already in its order: directories first, then by name.
const fixtureFileContents: Record<string, string> = {
  "README.md": [
    "# noisedeck",
    "",
    "A terminal-native console for CLI coding agents.",
    "",
    "- one real interactive terminal per session",
    "- automatic restart and replay (see docs/architecture.md)",
    "",
    "Dev: go run . and pnpm dev in web/.",
  ].join("\n"),
  "docs/architecture.md": [
    "# Architecture",
    "",
    "Three layers, strictly ordered:",
    "",
    "1. provider — descriptors: how each CLI spawns and resumes",
    "2. session  — tmux panes, recording, crash recovery",
    "3. server   — HTTP + WS, serves web/ and the REST API",
    "",
    "The session layer never knows provider names.",
    "The web layer never touches tmux.",
  ].join("\n"),
  // The empty file exercises the viewer's "empty file" presentation:
  // a file the server decodes to zero bytes, distinct from a
  // file it refuses to decode (noisefloor-mark.png).
  "empty.txt": "",
  "internal/provider/provider.go": [
    "package provider",
    "",
    "// Descriptor declares how a provider is spawned, resumed,",
    "// and what its terminal quirks are. All provider-specific",
    "// behavior lives here — the session layer stays generic.",
    "type Descriptor struct {",
    "    ID         string",
    "    Cmd        []string",
    "    ResumeArgs func(sessionID string) []string",
    "    Env        map[string]string",
    "    ReadyRE    string // regexp: provider is accepting input",
    "    QuirkWrap  int    // forced wrap width, 0 = none",
    "}",
    "",
    "func Lookup(id string) (Descriptor, bool) {",
    "    d, ok := registry[id]",
    "    return d, ok",
    "}"
  ].join("\n"),
  "internal/provider/provider_test.go": [
    "package provider",
    "",
    "import \"testing\"",
    "",
    "func TestProvider(t *testing.T) {}",
  ].join("\n"),
  "internal/db/schema.sql": [
    "CREATE TABLE IF NOT EXISTS schema_migrations (",
    "  version INTEGER PRIMARY KEY",
    ");",
  ].join("\n"),
  "internal/db/migrations/20260801_initial.sql": [
    "CREATE TABLE IF NOT EXISTS sessions (",
    "  id TEXT PRIMARY KEY",
    ");",
  ].join("\n"),
};

function fixtureEntry(
  path: string,
  dir: boolean,
  size: number,
  mtime: string,
): FileEntry {
  return {
    name: path.slice(path.lastIndexOf("/") + 1),
    path,
    dir,
    size,
    mode: dir ? "rwxr-xr-x" : "rw-r--r--",
    mtime,
  };
}

const fixtureListings: Record<string, FileEntry[]> = {
  "": [
    fixtureEntry("docs", true, 4096, "2026-08-02T09:00:00Z"),
    fixtureEntry("internal", true, 4096, "2026-08-02T11:22:00Z"),
    fixtureEntry("empty.txt", false, 0, "2026-08-02T11:00:00Z"),
    fixtureEntry("README.md", false, 268, "2026-08-01T18:40:00Z"),
    fixtureEntry("go.mod", false, 412, "2026-07-31T08:05:00Z"),
    fixtureEntry("noisefloor-mark.png", false, 18422, "2026-08-02T10:15:00Z"),
  ],
  "docs": [
    fixtureEntry("docs/architecture.md", false, 331, "2026-07-31T12:00:00Z"),
  ],
  "internal": [
    fixtureEntry("internal/provider", true, 4096, "2026-08-02T11:22:00Z"),
    fixtureEntry("internal/db", true, 4096, "2026-08-02T11:22:00Z"),
  ],
  "internal/provider": [
    fixtureEntry("internal/provider/descriptor.go", false, 640, "2026-08-02T11:22:00Z"),
    // 38 minutes before the fixture clock ("38m").
    fixtureEntry("internal/provider/provider.go", false, 900, sinceMs(38 * 60_000)),
    fixtureEntry("internal/provider/provider_test.go", false, 400, "2026-08-02T11:22:00Z"),
  ],
  // The deeper directories exercise the breadcrumb's elision:
  // depth 3 and 4 so a files-depth capture can show the trail eliding.
  "internal/db": [
    fixtureEntry("internal/db/migrations", true, 4096, "2026-08-02T11:22:00Z"),
    fixtureEntry("internal/db/schema.sql", false, 256, "2026-08-02T11:22:00Z"),
  ],
  "internal/db/migrations": [
    fixtureEntry("internal/db/migrations/20260801_initial.sql", false, 144, "2026-08-02T11:22:00Z"),
  ],
  // The image viewer's folder: one file per viewer state. render-8k.png is
  // listed past the 16 MiB preview cap, so it is never fetched;
  // chart-latency.png has no image behind it, so it reads as removed
  // after the listing; capture-partial.png is cut off mid-stream.
  ".deck": [
    fixtureEntry(".deck/assets", true, 4096, sinceMs(2 * 60_000)),
  ],
  ".deck/assets": [
    fixtureEntry(".deck/assets/chart-latency.png", false, 96_000, sinceMs(6 * 60_000)),
    fixtureEntry(".deck/assets/capture-partial.png", false, 12_000, sinceMs(20_000)),
    fixtureEntry(".deck/assets/home.png", false, 412_000, sinceMs(2 * 60_000)),
    fixtureEntry(".deck/assets/home-mobile.png", false, 188_000, sinceMs(2 * 60_000)),
    fixtureEntry(".deck/assets/icon-512.png", false, 31_000, sinceMs(60 * 60_000)),
    fixtureEntry(".deck/assets/render-8k.png", false, 48_200_000, sinceMs(3 * 60_000)),
    fixtureEntry(".deck/assets/scan-raw.tiff", false, 8_100_000, sinceMs(60 * 60_000)),
  ],
};

// The images the fixture's image route serves, bundled with the preview
// only. A listed image missing here answers 404 from the page's own
// origin, the way the server answers for a file removed since listing.
const FIXTURE_IMAGES: Record<string, string> = {
  ".deck/assets/home.png": homeImage,
  ".deck/assets/home-mobile.png": homeMobileImage,
  ".deck/assets/icon-512.png": iconImage,
  ".deck/assets/capture-partial.png": partialImage,
  "noisefloor-mark.png": iconImage,
};

// FixtureApiClient serves the fixtures exactly as the server would.
// The sample workspace, shared with the
// directory listing so what the picker can navigate to is exactly what
// the field recognises as extant.
const FIXTURE_TREE: Record<string, string[]> = {
  "/workspace": ["field-notes", "audio-player", "deck", "model-groupchat", "payments-api", "noisefloor-site", "slop", "tmp", "visual-diff"],
  "/workspace/field-notes": ["docs", "src", "vendor"],
  "/workspace/field-notes/vendor": [],
  "/workspace/deck": ["assets", "src"],
  "/workspace/tmp": ["vd-gate2", "vd-perf"],
  "/workspace/visual-diff": ["cases", "src"],
  "/srv/projects": ["archive", "staging"],
};

// Stands in for a directory read plus git -C.
const FIXTURE_REPOS: Record<string, { remote: string; head: string; parent: string | null; parentPath?: string }> = {
  "/workspace/deck": { remote: "github.com/fable/deck", head: "main · 8f3c21a · 3d ago · working tree clean", parent: null },
  "/workspace/tasks-engine": { remote: "github.com/fable/tasks-engine", head: "main · 2b7d004 · 6h ago · working tree clean", parent: null },
  "/workspace/visual-diff": { remote: "github.com/fable/visual-diff", head: "main · 3ce1188 · 2d ago · working tree clean", parent: null },
  "/workspace/tmp/vd-gate2": { remote: "github.com/fable/visual-diff", head: "feat/gate2 · 41b90c7 · 2h ago · 4 files modified", parent: "visual-diff", parentPath: "/workspace/visual-diff" },
  "/workspace/tmp/vd-perf": { remote: "github.com/fable/visual-diff", head: "perf/no-copy · 9a02f31 · 4d ago · working tree clean", parent: "visual-diff", parentPath: "/workspace/visual-diff" },
};

function fixtureDirExists(path: string): boolean {
  if (FIXTURE_TREE[path] !== undefined || FIXTURE_REPOS[path] !== undefined) return true;
  const cut = path.lastIndexOf("/");
  return (FIXTURE_TREE[path.slice(0, cut) || "/workspace"] ?? []).includes(path.slice(cut + 1));
}

function fixtureEntries(path: string): string[] {
  const dirs = FIXTURE_TREE[path] ?? [];
  return FIXTURE_REPOS[path] !== undefined ? [".git", ...dirs] : [...dirs];
}

// The enclosing repository of a path that is not itself one: the nearest
// repository root strictly above it. A registered project is a repository
// root too (registration provisions one), named as the sidebar names it.
function fixtureEnclosing(path: string, projects: readonly Project[]): ParentRepository | null {
  const roots = Object.keys(FIXTURE_REPOS).filter((k) => FIXTURE_REPOS[k]!.parent === null)
    .map((r) => ({ name: r.split("/").pop() ?? r, path: r }));
  for (const p of projects) {
    if (!roots.some((r) => r.path === p.canonical_path)) roots.push({ name: p.name, path: p.canonical_path });
  }
  const hit = roots.find((r) => path.startsWith(`${r.path}/`));
  return hit ?? null;
}

function stripTimer<T extends { timer: unknown }>(job: T): Omit<T, "timer"> {
  const { timer: _timer, ...rest } = job;
  return rest;
}

export class FixtureApiClient implements ApiClient, FileApiClient, LaneApiClient {
  private removedSessions = new Set<string>();
  // uploadedImages holds the bytes of every file uploaded in this page, by
  // path, so the image viewer shows what was stored there.
  private uploadedImages = new Map<string, string>();
  private fileListings = Object.fromEntries(Object.entries(fixtureListings)
    .map(([path, entries]) => [path, entries.map((entry) => ({ ...entry }))]));

  constructor(
    private taskState: "ready" | "onboarding" = "ready",
    private readonly delayedRemoval = false,
    private detail: SessionDetail = fixtureSessionDetail,
    private readonly projects: Project[] = fixtureProjects,
    private readonly mountRoots: 1 | 2 = 1,
    // identities "none" starts with an empty catalogue, so the Git identity
    // row opens the first-identity form.
    private readonly identities: "some" | "none" = "some",
    uploadConflicts = false,
    // "truncated" reports Build scratch stopped at the scan bound.
    assets: FixtureAssetVariant = "list",
  ) {
    if (uploadConflicts) {
      this.fileListings[""]!.push(fixtureEntry("prd.md", false, 4800, "2026-09-17T03:24:00Z"));
    }
    this.assetListing = fixtureAssetListing(Date.now(), assets);
  }

  private assetListing: AssetListing;

  async logout(): Promise<void> {}
  // The fixture console has the setting OFF and accepts writes without
  // persisting: the preview must never touch the live console state.
  async getSettings(): Promise<ConsoleSettings> {
    return { skip_permissions: false, skip_close_confirm: false, skip_remove_project_confirm: false };
  }
  async saveSettings(next: ConsoleSettings): Promise<ConsoleSettings> {
    return next;
  }
  // The preview is a console nobody has rearranged yet, so it has no saved
  // layout and the sidebar keeps its default expansion.
  async getLayout(): Promise<SidebarLayout | null> {
    return null;
  }
  async saveLayout(next: SidebarLayout): Promise<SidebarLayout> {
    return next;
  }
  // The preview remembers nothing across loads: it opens on the fixture
  // session whatever was last looked at.
  async getLastSession(): Promise<string | null> {
    return null;
  }
  async saveLastSession(): Promise<void> {}
  // The preview stands in for a first-use user: no stored tutorial
  // record. Writes persist only inside the preview session, so the panel
  // can be exercised without touching anything that outlives it.
  private tutorial: TutorialRecord | null = null;
  async getTutorial(): Promise<TutorialRecord | null> {
    return this.tutorial;
  }
  async saveTutorial(next: TutorialPreference): Promise<TutorialRecord> {
    this.tutorial = { ...next, done: [...next.done], scope: "user" };
    return this.tutorial;
  }
  async listProjects(): Promise<Project[]> {
    return this.projects;
  }
  async listProviders(): Promise<readonly string[]> {
    return FIXTURE_PROVIDERS;
  }
  // The preview has no session history to rank by, and inventing one would
  // put the fixture's opinion into a surface the design comps are compared
  // against. An empty report leaves the picker in the served order.
  async getHarnessUsage(): Promise<HarnessUsageReport> {
    return { global: [], project: [] };
  }
  async listGitIdentities(): Promise<GitIdentityCatalog> {
    if (this.identities === "none") return { profiles: [], default_profile_id: null, revision: 1 };
    return { profiles: [fixtureGitIdentity], default_profile_id: fixtureGitIdentity.id, revision: 1 };
  }
  async createGitIdentity(name: string, email: string): Promise<GitIdentityProfile> {
    return { ...fixtureGitIdentity, name, email };
  }
  async updateGitIdentity(_profileID: string, _expectedRevision: number, name: string, email: string): Promise<GitIdentityProfile> {
    return { ...fixtureGitIdentity, name, email, revision: 2 };
  }
  async setDefaultGitIdentity(): Promise<GitIdentityCatalog> {
    return { profiles: [fixtureGitIdentity], default_profile_id: fixtureGitIdentity.id, revision: 1 };
  }
  async deleteGitIdentity(): Promise<void> {}
  async setProjectGitIdentity(): Promise<Project> { return fixtureProject; }
  async createProjectDirectory(): Promise<{ path: string }> {
    return { path: "/workspace/new-folder" };
  }
  // The listing has the server's shape: the mount lists the allowed roots
  // (one by default, so the picker descends straight into it), and a root's
  // top reports no parent because its parent lies outside every allowed
  // root. The tree is the sample workspace;
  // the second root exists only for the probe that drives the way back.
  async listProjectDirectories(path?: string) {
    if (path === undefined || path === "") {
      const roots = [{ name: "workspace", path: "/workspace" }];
      if (this.mountRoots === 2) roots.push({ name: "projects", path: "/srv/projects" });
      return { current_path: "", parent_path: null, directories: roots };
    }

    return {
      current_path: path,
      parent_path: path === "/workspace" || path === "/srv/projects" ? null : path.split("/").slice(0, -1).join("/"),
      directories: (FIXTURE_TREE[path] ?? []).map((name) => ({ name, path: `${path}/${name}` })),
    };
  }
  async createProject(name: string, path: string): Promise<Project> {
    if (this.projects.some((p) => p.canonical_path === path)) {
      throw new ApiError(409, "duplicate_path", `duplicate project path: ${path}`);
    }
    const project: Project = { id: `proj-${this.projects.length + 1}`, name, canonical_path: path, git_identity_setup_revision: 1, last_used_at: "t" };
    this.projects.push(project);
    return project;
  }

  // The inspection answers from the sample workspace:
  // repositories with a remote and a head line,
  // two linked worktrees under tmp, and plain directories. Absent paths are
  // created, a bare word resolves under /workspace, and the owner/repo
  // shorthand yields to a relative directory that exists.
  async inspectProject(input: string, base = ""): Promise<Inspection> {
    const v = input.trim();
    if (v === "") throw new ApiError(400, "invalid_directory", "nothing to inspect");
    const shorthand = /^[\w.-]+\/[\w.-]+$/.test(v) && !v.startsWith("/");
    const isURL = /^(https?:\/\/|git@|ssh:\/\/)/.test(v) || /^[\w.-]+\.[a-z]{2,}\//i.test(v)
      || (shorthand && !fixtureDirExists(`/workspace/${v.replace(/\/+$/, "")}`));
    if (isURL) {
      const slug = (v.replace(/\.git$/, "").split("/").filter(Boolean).at(-1) ?? "").replace(/[^A-Za-z0-9._-]/g, "-");
      const container = base === "" ? "/workspace" : base;
      const dest = `${container}/${slug}`;
      const known = FIXTURE_REPOS[dest];
      const wants = v.replace(/^(https?:\/\/|git@|ssh:\/\/)/, "").replace(/\.git$/, "").replace(":", "/");
      const parent = fixtureEnclosing(container, this.projects);
      const exists = fixtureDirExists(dest);
      const sameClone = known !== undefined && known.parent === null && known.remote.includes(wants);
      return {
        kind: "clone", path: dest, exists,
        entries: exists ? fixtureEntries(dest) : [],
        clone_url: /^(https?:\/\/|git@|ssh:\/\/)/.test(v) ? v : /^[\w.-]+\.[a-z]{2,}\//i.test(v) ? `https://${v}` : `https://github.com/${v}`,
        same_clone: sameClone,
        ...(sameClone ? { remote: known.remote, store_path: `${dest}/.git/noise-tasks/tasks.sqlite` } : {}),
        ...(parent !== null && !sameClone ? { parent, store_path: `${parent.path}/.git/noise-tasks/tasks.sqlite` } : {}),
        ...(parent === null && !sameClone ? { store_path: `${dest}/.git/noise-tasks/tasks.sqlite` } : {}),
      };
    }
    const path = (v.startsWith("/") ? v : `/workspace/${v}`).replace(/\/+$/, "");
    const roots = this.mountRoots === 2 ? ["/workspace", "/srv/projects"] : ["/workspace"];
    if (!roots.some((r) => path === r || path.startsWith(`${r}/`))) {
      throw new ApiError(403, "outside_allowed_roots", `outside allowed roots: ${path}`);
    }
    const known = FIXTURE_REPOS[path];
    if (known !== undefined) {
      const worktree = known.parent !== null;
      return {
        kind: worktree ? "worktree" : "repository", path, exists: true,
        remote: known.remote, head: known.head, entries: fixtureEntries(path),
        ...(worktree ? { parent: { name: known.parent!, path: known.parentPath! }, store_path: `${known.parentPath}/.git/noise-tasks/tasks.sqlite` }
          : { store_path: `${path}/.git/noise-tasks/tasks.sqlite` }),
      };
    }
    const exists = fixtureDirExists(path);
    const parent = fixtureEnclosing(path, this.projects);
    return {
      kind: exists ? "plain" : "absent", path, exists,
      entries: exists ? fixtureEntries(path) : [],
      ...(parent !== null ? { parent, store_path: `${parent.path}/.git/noise-tasks/tasks.sqlite` }
        : exists ? {} : { store_path: `${path}/.git/noise-tasks/tasks.sqlite` }),
    };
  }

  // The clone fixture is a job that advances on a timer: a URL naming
  // "private" is refused like a bad key, one naming "missing" like a
  // repository that is not there or not visible, and one naming
  // "unplugged" dies on the network with a partial clone held. Every other
  // URL clones in about two seconds and registers its project.
  private clones = new Map<string, CloneJob & { timer: ReturnType<typeof setInterval> | null }>();
  async startClone(request: CloneStartRequest): Promise<CloneJob> {
    if (fixtureDirExists(request.destination)) {
      throw new ApiError(409, "destination_not_empty", `destination is not empty: ${request.destination}`);
    }
    const now = new Date().toISOString();
    const job: CloneJob & { timer: ReturnType<typeof setInterval> | null } = {
      id: `clone-${this.clones.size + 1}`, url: request.url, destination: request.destination, name: request.name,
      status: "running", percent: 0, line: "Cloning into " + request.name + "...", partial_bytes: 0,
      started_at: now, updated_at: now, timer: null,
    };
    this.clones.set(job.id, job);
    this.runClone(job);
    return stripTimer(job);
  }
  private runClone(job: CloneJob & { timer: ReturnType<typeof setInterval> | null }): void {
    job.status = "running";
    job.failure = undefined;
    job.percent = 0;
    job.stderr = undefined;
    job.timer = setInterval(() => {
      job.percent = Math.min(100, job.percent + 17);
      job.line = `Receiving objects: ${String(job.percent).padStart(3)}% (${job.percent}/100), ${(job.percent * 0.12).toFixed(2)} MiB | 1.20 MiB/s`;
      job.partial_bytes = job.percent * 125_829;
      job.updated_at = new Date().toISOString();
      if (job.percent >= 34 && /private/.test(job.url)) {
        this.endClone(job, "failed", "auth", "git@github.com: Permission denied (publickey).\nfatal: Could not read from remote repository.");
      } else if (job.percent >= 34 && /missing/.test(job.url)) {
        this.endClone(job, "failed", "bad_or_private", `remote: Repository not found.\nfatal: repository '${job.url}/' not found`);
      } else if (job.percent >= 51 && /unplugged/.test(job.url)) {
        this.endClone(job, "failed", "network", "fetch-pack: unexpected disconnect while reading sideband packet\nfatal: early EOF");
      } else if (job.percent >= 100) {
        this.endClone(job, "done");
        const project: Project = { id: `proj-${this.projects.length + 1}`, name: job.name, canonical_path: job.destination, git_identity_setup_revision: 1, last_used_at: "t" };
        this.projects.push(project);
        job.project_id = project.id;
        FIXTURE_TREE[job.destination] = [];
      }
    }, 300);
  }
  private endClone(job: CloneJob & { timer: ReturnType<typeof setInterval> | null }, status: CloneJob["status"], failure?: CloneJob["failure"], stderr?: string): void {
    if (job.timer !== null) clearInterval(job.timer);
    job.timer = null;
    job.status = status;
    job.failure = failure;
    job.stderr = stderr;
    job.updated_at = new Date().toISOString();
  }
  async listClones(): Promise<CloneJob[]> {
    return [...this.clones.values()].map(stripTimer);
  }
  async getClone(id: string): Promise<CloneJob> {
    const job = this.clones.get(id);
    if (job === undefined) throw new ApiError(404, "not_found", "clone job not found");
    return stripTimer(job);
  }
  async cancelClone(id: string): Promise<CloneJob> {
    const job = await this.cloneOrThrow(id);
    if (job.status === "running") this.endClone(job, "stopped", undefined, "fetch-pack: unexpected disconnect while reading sideband packet\nfatal: early EOF");
    return stripTimer(job);
  }
  async resumeClone(id: string): Promise<CloneJob> {
    const job = await this.cloneOrThrow(id);
    this.runClone(job);
    return stripTimer(job);
  }
  async discardClone(id: string): Promise<void> {
    const job = await this.cloneOrThrow(id);
    if (job.timer !== null) clearInterval(job.timer);
    this.clones.delete(id);
  }
  private async cloneOrThrow(id: string) {
    const job = this.clones.get(id);
    if (job === undefined) throw new ApiError(404, "not_found", "clone job not found");
    return job;
  }
  async listSessions(projectID: string): Promise<SessionSummary[]> {
    return (fixtureSessions[projectID] ?? [])
      .filter((session) => !this.removedSessions.has(session.id));
  }
  async recoverSession(): Promise<RecoverResult> {
    // The fixture's recover always presents an exact resume; the ladder's
    // relaunch outcome is exercised by the live smoke's mock server.
    return { ...fixtureSessionDetail, outcome: "resumed" };
  }

  async sessionScreen(): Promise<string | null> {
    return null;
  }

  async createSession(projectID: string, provider: string): Promise<SessionSummary> {
    return { id: "sess-fresh", project_id: projectID, provider, display_name: "", status: "running", generation: 1, created_at: "t", updated_at: "t", ...runningFacts };
  }
  async getTaskmasterInfo() {
    if (this.taskState === "onboarding") {
      throw new ApiError(404, "not_taskmaster_project", "preview fixture");
    }
    return {
      path: fixtureProject.canonical_path,
      config_path: "c",
      tasks_path: "t",
      docs_path: `${fixtureProject.canonical_path}/.taskmaster/docs`,
    };
  }
  // The preview's onboarding state becomes a usable board, so the
  // uninitialized affordance can be previewed end to end.
  async initializeTaskmaster() {
    this.taskState = "ready";
    return { path: `${fixtureProject.canonical_path}/.git/noise-tasks/tasks.sqlite`, tag: "main", already_initialized: false };
  }

  async listTaskmasterTasks(): Promise<TaskmasterTasks> {
    return fixtureTasks;
  }
  async addTaskmasterTask(): Promise<{ id: string }> {
    return { id: "10" };
  }
  async getTaskmasterTask(_projectID: string, taskID: string): Promise<TmTask> {
    for (const file of Object.values(fixtureTasks)) {
      const task = file.tasks.find((candidate) => String(candidate.id) === taskID);
      if (task !== undefined) return task;
    }
    throw new ApiError(404, "not_found", "task not found");
  }
  async listLanes(): Promise<Lane[]> {
    return [];
  }
  async startLane(): Promise<Lane> {
    throw new ApiError(501, "preview", "Lanes are not available in the preview.");
  }
  async markTaskDone(): Promise<TaskDoneResult> {
    throw new ApiError(501, "preview", "Tasks cannot be marked done in the preview.");
  }
  async requestLaneReview(): Promise<Lane> {
    throw new ApiError(501, "preview", "Reviews are not available in the preview.");
  }
  async getLaneReview(): Promise<LaneReview> {
    return { requested: false, seen: false };
  }
  async listTaskmasterPRDs(): Promise<string[]> {
    return ["PRD.md", "PHASE0.md"];
  }
  async uploadPrd(): Promise<void> {}
  async commitStarter(): Promise<StarterCommitResult> {
    return { commit: "0000000000000000000000000000000000000000", created: true };
  }
  async generateTasks(): Promise<TmRun> {
    return fixtureRun;
  }
  async getSessionChats(sessionID: string): Promise<ChatLedger> {
    return fixtureChatLedger(sessionID);
  }

  async listChats(): Promise<ChatIndex> {
    return fixtureChatIndex();
  }

  async listTaskmasterRuns(): Promise<TmRun[]> {
    return [fixtureRun];
  }
  async getTaskmasterRun(): Promise<TmRun> {
    return fixtureRun;
  }
  async listProjectFiles(_projectID: string, path: string): Promise<FileListing> {
    const entries = this.fileListings[path];
    if (entries === undefined) {
      throw new ApiError(404, "not_found", "no such file in the project");
    }
    return {
      path,
      parent: path === "" ? undefined : path.slice(0, Math.max(0, path.lastIndexOf("/"))),
      at_root: path === "",
      entries,
    };
  }
  // The fixture refuses exactly what the server refuses: bytes it will
  // not decode answer 415, and the panel shows its no-preview state.
  async viewProjectFile(_projectID: string, path: string): Promise<FileView> {
    const content = fixtureFileContents[path];
    if (content === undefined) {
      throw new ApiError(415, "not_text", "file is not text");
    }
    return { content };
  }

  async diffProjectFile(_projectID: string, _path: string): Promise<FileView> {
    return { content: "@@ -1 +1 @@\n-old\n+new\n" };
  }

  subscribeProjectFileEvents(_projectID: string, _path: string, _onChange: () => void): () => void {
    return () => {};
  }
  projectFileDownloadUrl(projectID: string, path: string): string {
    return `/api/projects/${encodeURIComponent(projectID)}/files/download?path=${encodeURIComponent(path)}`;
  }
  // The preview page has no image route, so the fixture stands in for it:
  // the bytes of a file uploaded in this page, else a bundled image, else
  // nothing at all. A listed image with nothing behind it is the route's
  // 404 — answered here rather than left to whatever the page's own server
  // says about a path it does not have (Vite's dev server answers 200).
  projectFileImageUrl(_projectID: string, path: string, version?: string): string {
    const uploaded = this.uploadedImages.get(path);
    if (uploaded !== undefined) return uploaded;
    const bundled = FIXTURE_IMAGES[path];
    if (bundled === undefined) return "data:,";
    return version === undefined ? bundled : `${bundled}?v=${encodeURIComponent(version)}`;
  }
  async checkProjectFileImage(projectID: string, path: string): Promise<void> {
    if (this.uploadedImages.has(path)) return;
    if (FIXTURE_IMAGES[path] === undefined) throw new ApiError(404, "not_found", "no such file in the project");
    const resp = await fetch(this.projectFileImageUrl(projectID, path), { method: "HEAD", cache: "no-store" });
    if (!resp.ok) throw new ApiError(resp.status, "unknown", resp.statusText);
  }
  // The upload answer the server gives, applied to the fixture listing:
  // a taken name asks, consent is the asked version, a free name stores.
  async uploadProjectFile(_projectID: string, destPath: string, body: Blob, consent?: UploadConsent): Promise<FileUploadResult> {
    const cut = destPath.lastIndexOf("/");
    const entries = this.fileListings[cut < 0 ? "" : destPath.slice(0, cut)];
    if (entries === undefined) {
      throw new ApiError(404, "not_found", "no such file in the project");
    }
    const stored = fixtureUploadAnswer(entries, destPath, body.size, consent, new Date().toISOString());
    const previous = this.uploadedImages.get(destPath);
    if (previous !== undefined) URL.revokeObjectURL(previous);
    this.uploadedImages.set(destPath, URL.createObjectURL(body));
    return stored;
  }
  async listProjectAssets(): Promise<AssetListing> {
    return structuredClone(this.assetListing);
  }
  async deleteProjectAssets(_projectID: string, files: AssetDeleteFile[]): Promise<AssetDeleteResult[]> {
    return fixtureAssetDeleteAnswer(this.assetListing, files);
  }
  subscribeProjectAssetEvents(_projectID: string, _onChange: () => void): () => void {
    return () => {};
  }
  async getSession(): Promise<SessionDetail> {
    return this.detail;
  }
  async deleteSession(sessionID: string): Promise<void> {
    if (this.delayedRemoval) {
      await new Promise((resolve) => setTimeout(resolve, 500));
    }
    this.removedSessions.add(sessionID);
  }
  async deleteProject(): Promise<void> {}
  async renameProject(): Promise<void> {}
  async renameSession(): Promise<void> {}
  async setSessionColor(): Promise<void> {}
}
