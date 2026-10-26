// The Files panel's presentation model: one
// directory listing, one read-only view of one file, and the upload
// lifecycle. DOM-free so the logic runs under plain node tests; the DOM
// shell (files-panel-dom.ts) renders it.
//
// The server owns every fact here. The model holds what the last answers
// carried and nothing it inferred: it never decodes bytes the server
// declined to decode, never counts what the listing did not report, and
// never claims an upload landed before its request returned.
import { ApiError, AUTHORIZATION_LOSS_CODES } from "../../api/http.ts";
import {
  UPLOAD_CONFLICT_REASONS,
  type FileApiClient,
  type FileEntry,
  type FileListing,
  type UploadConflictInfo,
  type UploadConsent,
} from "../../api/files.ts";
import { formatFileSize, formatSince } from "../../lib/format.ts";

// ViewState is what the viewer pane has to show for the selected file.
// "no-preview" is the honest answer when the server refuses to decode the
// bytes: nothing is decoded here either.
export type ViewState =
  | { kind: "loading" }
  | { kind: "text"; content: string }
  | { kind: "image"; image: ImageView }
  | { kind: "no-preview"; reason: string }
  | { kind: "error"; message: string };

// ImageView is an image file's place between the listing and the pixels.
// The browser decodes the bytes itself from the image route; the model
// holds only what that decode reported. "shown" carries src because a
// background re-read swaps the source while the old dimensions stay on
// screen until the new decode reports. "gone" is a file the listing named
// that the server no longer has.
export type ImageView =
  | { state: "loading"; src: string }
  | { state: "shown"; src: string; width: number; height: number; alpha: boolean }
  | { state: "undecodable" }
  | { state: "oversized" }
  | { state: "gone" };

// Crumb is one segment of the header trail. path is the directory the
// segment navigates to, project-relative; the root crumb's is "".
export interface Crumb {
  label: string;
  path: string;
  current: boolean;
}

export interface FilesPanelCallbacks {
  onError?: (message: string, cause?: unknown) => void;
  onChange?: () => void;
  // A file, not a directory, was chosen in the listing.
  onOpenFile?: (entry: FileEntry) => void;
  // onProgress reports one upload record's measured bytes moving. It is
  // NOT a change event: a byte tick must not re-render whole panes or
  // disturb focus and scroll, so the DOM patches the one row in place.
  onProgress?: (record: UploadRecord) => void;
}

// UPLOAD_LIMIT_BYTES mirrors the server's per-file bound (32 MiB).
// Checked before anything is sent so an oversize pick never starts a
// request: the record stays visible and dismissible instead.
const UPLOAD_LIMIT_BYTES = 32 << 20;

// MAX_IMAGE_VIEW_BYTES mirrors the image route's bound (16 MiB). A listed
// file past it is never requested: the server would refuse it from its
// size alone, so the request could only cost the round trip.
export const MAX_IMAGE_VIEW_BYTES = 16 << 20;

// IMAGE_EXTENSIONS decide which viewer to try, nothing more: the image
// route sniffs the bytes and is the authority on what is an image.
const IMAGE_EXTENSIONS = new Set(["png", "jpg", "jpeg", "gif", "webp", "bmp", "avif"]);

export function isImageName(name: string): boolean {
  const dot = name.lastIndexOf(".");
  return dot > 0 && IMAGE_EXTENSIONS.has(name.slice(dot + 1).toLowerCase());
}

// formatMiB is the image cap's copy: tenths of a MiB rounded up, so a
// file one byte past the cap never reads as the cap itself, and no
// decimal when it is zero. The server words its 413 the same way.
export function formatMiB(bytes: number): string {
  const tenths = Math.ceil((bytes * 10) / (1 << 20));
  return tenths % 10 === 0 ? `${tenths / 10} MiB` : `${Math.floor(tenths / 10)}.${tenths % 10} MiB`;
}

// UploadStatus is one picked file's honest place in the upload
// lifecycle. "confirming" means every byte left the browser and the
// server's answer is still owed — it is not success. "unconfirmed" means
// no answer ever came back (lost response, or an abort after the request
// was dispatched): the file may have been written, and nothing may claim
// otherwise. "stopped" means the record was cancelled before any request
// was made, so nothing was sent.
export type UploadStatus =
  | "queued"
  | "preparing"
  | "uploading"
  | "confirming"
  | "conflict"
  | "complete"
  | "failed"
  | "unconfirmed"
  | "stopped"
  | "too-large";

// UploadRecord is one picked file the panel is placing or has finished
// trying to place. The destination facts are the ones bound at pick
// time — the project is the model's own, destDir is the directory the
// user was looking at, name is the current destination basename (the
// original one until an Upload-as answer edits it). sent/total are the
// transport's own measurements, never estimated.
export interface UploadRecord {
  id: number;
  file: File;
  name: string;
  destDir: string;
  status: UploadStatus;
  sent: number;
  total: number;
  // error is the server's own refusal sentence for a failed record.
  error: string | null;
  // position is this file's 1-based place in its pick; batchSize is how
  // many valid files that pick carried.
  position: number;
  batchSize: number;
  // replaces marks a record sent as a Replace answer, so its row can say
  // what replacing costs while the original stays on disk until the
  // server confirms.
  replaces: boolean;
}

// SortField is a column the user can order the list by. modified is a
// mtime, not a child count; sorting is an explicit user gesture, so it
// applies to what the listing reported, never to something the panel guessed.
export type SortField = "name" | "size" | "modified";

export type SortDirection = "asc" | "desc";

export interface SortState {
  field: SortField;
  direction: SortDirection;
}

// confine reduces a path to one that cannot leave the project root: a
// segment that would climb above it is dropped, so the root is the
// highest reachable directory and the breadcrumb never offers a "..".
// The server enforces the real boundary; this keeps the
// browser's own bookkeeping from ever asking it to.
export function confine(path: string): string {
  const out: string[] = [];
  for (const segment of path.split("/")) {
    if (segment === "" || segment === ".") continue;
    if (segment === "..") {
      out.pop();
      continue;
    }
    out.push(segment);
  }
  return out.join("/");
}

export function projectRootLabel(canonicalPath: string): string {
  const leaf = canonicalPath.slice(canonicalPath.lastIndexOf("/") + 1);
  return leaf === "" ? canonicalPath : leaf;
}

// projectPathPrefix is the faint part of the breadcrumb: everything above
// the project root. Visible so you can place the project on
// disk, and never a link, because nothing above the root is reachable.
export function projectPathPrefix(canonicalPath: string): string {
  const cut = canonicalPath.lastIndexOf("/");
  return cut < 0 || projectRootLabel(canonicalPath) === canonicalPath
    ? ""
    : canonicalPath.slice(0, cut + 1);
}

export function breadcrumbs(rootLabel: string, dir: string): Crumb[] {
  const segments = dir === "" ? [] : dir.split("/");
  const crumbs: Crumb[] = [
    { label: rootLabel, path: "", current: segments.length === 0 },
  ];
  segments.forEach((segment, i) => {
    crumbs.push({
      label: segment,
      path: segments.slice(0, i + 1).join("/"),
      current: i === segments.length - 1,
    });
  });
  return crumbs;
}

// uploadBasename keeps only a chosen file's own name. A picker can hand
// back a relative path, and the destination is always the directory the
// user is looking at — never one the file name implies.
export function uploadBasename(name: string): string {
  const leaf = name.split(/[\\/]/).pop() ?? "";
  return leaf === "." || leaf === ".." ? "" : leaf;
}

// splitFileName separates a name into base and extension at the last dot,
// so a collision-free suggestion keeps the extension the incoming file
// arrived with. A dot with nothing before it (".env") is the name itself,
// not an extension.
export function splitFileName(name: string): { base: string; ext: string } {
  const dot = name.lastIndexOf(".");
  if (dot <= 0) return { base: name, ext: "" };
  return { base: name.slice(0, dot), ext: name.slice(dot) };
}

// suggestUploadName is the first free "<base>-2<ext>", then -3, -4 … for
// an incoming name, against the names this flow already knows are taken.
// The server rechecks whatever is sent; this only picks a name worth
// proposing.
export function suggestUploadName(incoming: string, taken: Iterable<string>): string {
  const used = new Set(taken);
  const { base, ext } = splitFileName(incoming);
  for (let n = 2; n < 1000; n++) {
    const candidate = `${base}-${n}${ext}`;
    if (!used.has(candidate)) return candidate;
  }
  return incoming;
}

export function rowName(entry: FileEntry): string {
  return entry.dir ? `${entry.name}/` : entry.name;
}

// rowMeta is a row's trailing cell. The listing reports a size, not a
// child count, so a directory row carries no metadata rather than a
// number nothing measured. Under a modified sort the cell repeats the
// file's age, because the column you ordered by is the fact
// worth showing in the row.
export function rowMeta(entry: FileEntry, field: SortField = "name", now?: number): string {
  if (entry.dir) return "";
  if (field === "modified") {
    const since = formatSince(entry.mtime, now ?? Date.now());
    return since === null ? "" : since;
  }
  return formatFileSize(entry.size ?? 0);
}

// rowChange is the words for a row's git marker, or "" when the listing
// reported none.
export function rowChange(entry: FileEntry): string {
  if (entry.change === "modified") return "Modified";
  if (entry.change === "new") return "New";
  return "";
}

// viewerStatusParts is the footer's machine register as facts: kind, line
// count for decoded text, size, recency (its words split from its number so
// a narrow presentation can keep just the number), and read-only — stated
// because it is the whole contract of this panel. One list feeds both the
// plain string and the DOM renderer, so the two cannot drift.
export interface ViewerStatusPart {
  key: "kind" | "lines" | "dims" | "size" | "modified" | "read-only";
  text: string;
  // For "modified": the bare age, so the phone can say "38m" while the
  // desktop says "modified 38m ago". Equal to text elsewhere.
  short: string;
}
// An image's facts name its format the way the format names itself (PNG),
// and its dimensions join only once the decode has measured them.
export function viewerStatusParts(entry: FileEntry, view: ViewState | null, now: number): ViewerStatusPart[] {
  const image = view?.kind === "image";
  const kind = image ? entry.name.slice(entry.name.lastIndexOf(".") + 1).toUpperCase() : kindOfName(entry.name);
  const parts: ViewerStatusPart[] = [{ key: "kind", text: kind, short: kind }];
  if (view?.kind === "text") {
    const lines = `${view.content.split("\n").length} lines`;
    parts.push({ key: "lines", text: lines, short: lines });
  }
  if (view?.kind === "image" && view.image.state === "shown") {
    const dims = `${view.image.width} × ${view.image.height}`;
    parts.push({ key: "dims", text: dims, short: dims });
  }
  const size = formatFileSize(entry.size ?? 0);
  parts.push({ key: "size", text: size, short: size });
  const since = formatSince(entry.mtime, now);
  if (since !== null) parts.push({ key: "modified", text: `modified ${since} ago`, short: since });
  if (!image) parts.push({ key: "read-only", text: "read-only", short: "read-only" });
  return parts;
}

export function viewerStatus(entry: FileEntry, view: ViewState | null, now: number): string {
  return viewerStatusParts(entry, view, now).map((part) => part.text).join(" · ");
}

// kindOfName names what a file most plausibly holds, extension by
// extension, so the footer reads as a statement about the bytes rather
// than a bare suffix. An unknown extension falls back to its suffix
// verbatim, and an extensionless file is "text".
const KIND_OF: Record<string, string> = {
  md: "markdown", go: "go", mod: "go module", sum: "go checksums",
  ts: "typescript", tsx: "tsx", js: "javascript", jsx: "jsx",
  json: "json", html: "html", css: "css", yml: "yaml", yaml: "yaml",
  toml: "toml", sh: "shell", log: "log", cast: "asciicast",
  txt: "text", csv: "csv",
};

export function kindOfName(name: string): string {
  const dot = name.lastIndexOf(".");
  if (dot <= 0) return "text";
  const ext = name.slice(dot + 1).toLowerCase();
  return KIND_OF[ext] ?? ext;
}

// fileExtension is the viewer's "No preview for <ext> files." answer:
// the suffix with its dot, or "these" when the name has none.
export function fileExtension(name: string): string {
  const dot = name.lastIndexOf(".");
  return dot > 0 ? name.slice(dot) : "these";
}

export class FilesPanelModel {
  dir = "";
  listing: FileListing | null = null;
  listError: string | null = null;
  loading = false;
  selected: FileEntry | null = null;
  view: ViewState | null = null;
  uploading = false;
  // uploadErrors is one line per file that did NOT land. A multi-file
  // pick is many requests, so a partial failure has to name its files.
  uploadErrors: string[] = [];
  expanded = false;
  // sort is the user's chosen column and direction. Name/asc is the
  // default because it is what the server already returns for a directory.
  sort: SortState = { field: "name", direction: "asc" };
  // conflict is the file currently being asked about: the server refused
  // to place it because the name is taken, and the user has not
  // answered yet. Null when nothing is being asked. It is distinct from
  // uploading: nothing is in flight while the question stands.
  conflict: UploadConflictAsk | null = null;
  // conflictName is the editable "Or upload as" answer for the current
  // ask, prefilled with the first free suggestion; conflictNameError is
  // its refusal (taken, or not a name). The server rechecks whatever is
  // sent — this is the pick worth proposing, not a second authority.
  conflictName = "";
  conflictNameError: string | null = null;
  // sameChoice carries the user's Replace or Upload-as answer to the
  // remaining conflicts in this batch. It starts unchecked every batch,
  // never applies to Cancel, and every file is still validated at commit
  // whatever it says.
  sameChoice = false;
  // confirming is true while the current ask's answer is in flight, so a
  // double click cannot send the file twice.
  confirming = false;
  // uploadRecords is every picked file this panel is placing or has
  // finished trying to place. A complete record leaves at the refresh
  // that reads the file back as an ordinary listing row; every other
  // outcome stays until the user dismisses it — a file that failed,
  // stopped, or came back unconfirmed remains visible per file.
  uploadRecords: UploadRecord[] = [];
  // selectedUpload is the record whose details the viewer pane shows.
  // Selecting it is a user gesture, never a side effect of picking
  // files or of progress: nothing selects, focuses, or scrolls on its
  // own while uploads move.
  selectedUpload: UploadRecord | null = null;
  private client: FileApiClient;
  private projectID: string;
  private projectPath: string;
  private cb: FilesPanelCallbacks;
  // Sequence guards: an older answer must never paint over a newer one.
  private listSeq = 0;
  private foregroundListSeq = 0;
  private viewSeq = 0;
  // resetSeq invalidates work that may outlive a close or disposal. The
  // request may already have reached the server, but its late answer must
  // not restore viewer or error state that reset deliberately cleared.
  private resetSeq = 0;
  private unsubscribeEvents: (() => void) | null = null;
  private openState = false;
  // The conflict session spans one pick: the queued asks, the names this
  // flow has already claimed, and the last stored path for the finishing
  // selection. It exists only while conflicts remain unanswered.
  private conflictQueue: UploadConflictAsk[] = [];
  private conflictSession: ConflictSession | null = null;
  // conflictsAsked counts asks already put on screen in this session, so
  // the dialog's "n of m" advances through cancellations too.
  private conflictsAsked = 0;
  // controllers owns each record's in-flight request. Clearing or
  // replacing the entry is what fences a late answer: a send whose
  // controller is no longer registered (dismissed, cancelled, reset,
  // superseded) finds itself dead and mutates nothing.
  private controllers = new Map<number, AbortController>();
  private uploadIdSeq = 0;

  constructor(
    client: FileApiClient,
    projectID: string,
    projectPath: string,
    cb: FilesPanelCallbacks = {},
  ) {
    this.client = client;
    this.projectID = projectID;
    this.projectPath = projectPath;
    this.cb = cb;
  }

  rootLabel(): string {
    return projectRootLabel(this.projectPath);
  }

  pathPrefix(): string {
    return projectPathPrefix(this.projectPath);
  }

  crumbs(): Crumb[] {
    return breadcrumbs(this.rootLabel(), this.dir);
  }

  // entries preserves the SERVER's order. The listing endpoint already
  // sorts directories first and then by name, so re-sorting here would
  // only invent a second opinion about the same directory.
  entries(): FileEntry[] {
    return this.listing?.entries ?? [];
  }

  // setSort selects a column. Re-selecting the active column flips its
  // direction; another column restarts ascending. Selecting anything keeps
  // the panel's selection: sorting reorders rows, never the open file.
  setSort(field: SortField): void {
    if (this.sort.field === field) {
      this.sort = { field, direction: this.sort.direction === "asc" ? "desc" : "asc" };
      return;
    }
    this.sort = { field, direction: "asc" };
  }

  // sortedEntries applies the sort to the raw listing. Directories always
  // stay first — a directory group is a shift of the eye, not a sortable
  // file — so the direction orders within each group, never across them.
  sortedEntries(): FileEntry[] {
    const entries = this.entries();
    if (this.sort.field === "name" && this.sort.direction === "asc") {
      // The server order is already directory-first, name-asc; sorting here
      // would be redundant for the default, so skip the copy.
      return entries;
    }
    const byName = (a: FileEntry, b: FileEntry): number =>
      a.name.localeCompare(b.name, undefined, { sensitivity: "base" });
    const key = (entry: FileEntry): number => {
      if (this.sort.field === "size") return entry.size ?? 0;
      if (this.sort.field === "modified") {
        const t = Date.parse(entry.mtime);
        return Number.isNaN(t) ? -1 : t;
      }
      return 0;
    };
    const order = (a: FileEntry, b: FileEntry): number => {
      const ka = key(a);
      const kb = key(b);
      const primary = ka !== kb ? (this.sort.direction === "desc" ? kb - ka : ka - kb) : 0;
      // A name tiebreak keeps equal-key rows in a name order; for a name
      // sort itself the key is constant, so the name IS the direction.
      const named = byName(a, b);
      if (primary !== 0) return primary;
      if (named !== 0) return this.sort.direction === "desc" ? -named : named;
      return 0;
    };
    const dirs = entries.filter((e) => e.dir).sort(order);
    const files = entries.filter((e) => !e.dir).sort(order);
    return [...dirs, ...files];
  }

  // dropTargetLabel is the directory named by the upload drop overlay. The
  // root reads as the project's name; a nested directory by its own leaf.
  dropTargetLabel(): string {
    const leaf = this.dir.slice(this.dir.lastIndexOf("/") + 1);
    return leaf === "" ? this.rootLabel() : leaf;
  }

  truncated(): boolean {
    return this.listing?.truncated === true;
  }

  // hasUp is true whenever the user has left the project root; the
  // ".." row is the one-step complement to the breadcrumb's jumps.
  hasUp(): boolean {
    return this.dir !== "";
  }

  // upLabel names the directory ".." returns to: the project root when
  // one level deep, otherwise the parent directory's leaf segment.
  upLabel(): string {
    if (this.dir === "") return this.rootLabel();
    const parent = this.dir.slice(0, this.dir.lastIndexOf("/"));
    return parent === "" ? this.rootLabel() : parent.slice(parent.lastIndexOf("/") + 1);
  }

  // parentDir is the confined directory one segment above the current one.
  parentDir(): string {
    const cut = this.dir.lastIndexOf("/");
    return cut < 0 ? "" : this.dir.slice(0, cut);
  }

  downloadUrl(entry: FileEntry): string {
    return this.client.projectFileDownloadUrl(this.projectID, entry.path);
  }

  // canDownload is false only for a file the server no longer has: a
  // download link to it could only fail.
  canDownload(): boolean {
    return !(this.view?.kind === "image" && this.view.image.state === "gone");
  }

  // imageSrc is the image route for one listing generation of the file.
  // The listing's mtime and size name the generation, so a changed file is
  // a different URL: the response is never cached, but an <img> whose src
  // did not change is never asked to fetch again.
  private imageSrc(entry: FileEntry): string {
    return this.client.projectFileImageUrl(this.projectID, entry.path, `${entry.mtime}:${entry.size ?? 0}`);
  }

  // openDirectory lists a directory. The selection is dropped: the viewer
  // showed a file (or an upload's details) from the directory being left.
  async openDirectory(path: string): Promise<void> {
    const next = confine(path);
    if (next !== this.dir) {
      this.selected = null;
      this.selectedUpload = null;
      this.view = null;
      this.listing = null;
      this.listError = null;
    }
    this.dir = next;
    await this.refresh();
    this.subscribeEvents();
  }

  // openFile resolves a project-relative path through the current listing,
  // so preview uses the server's entry and its size, age, and git facts.
  async openFile(path: string, canActivate: () => boolean = () => true): Promise<void> {
    const resetSeq = this.resetSeq;
    const confined = confine(path);
    const cut = confined.lastIndexOf("/");
    const parent = cut < 0 ? "" : confined.slice(0, cut);
    await this.openDirectory(parent);
    if (resetSeq !== this.resetSeq || !canActivate()) return;
    const entry = this.entries().find((candidate) => !candidate.dir && candidate.path === confined);
    if (entry !== undefined) await this.activate(entry);
  }

  async activate(entry: FileEntry): Promise<void> {
    if (entry.dir) {
      await this.openDirectory(entry.path);
      return;
    }
    // Choosing a file takes the viewer back from an upload's details;
    // a pending upload selection never overrides the file viewer.
    this.selectedUpload = null;
    this.selected = entry;
    this.cb.onOpenFile?.(entry);
    await this.loadView(entry);
  }

  async refresh(quiet = false): Promise<void> {
    const seq = ++this.listSeq;
    const completedAtStart = new Set(this.uploadRecords.filter((record) => record.status === "complete").map((record) => record.id));
    // Only a foreground read owns the spinner, and only the newest one may
    // stop it: a slow first read finishing after a second one started must
    // not clear a spinner that belongs to the read still running.
    let foregroundSeq = 0;
    if (!quiet) {
      foregroundSeq = ++this.foregroundListSeq;
      this.loading = true;
    }
    try {
      const listing = await this.client.listProjectFiles(this.projectID, this.dir);
      if (seq !== this.listSeq) return;
      this.listing = listing;
      this.listError = null;
      if (this.selected !== null) {
        const selected = listing.entries.find((entry) =>
          !entry.dir && entry.path === this.selected?.path
        );
        if (selected === undefined) {
          ++this.viewSeq;
          this.selected = null;
          this.view = null;
        } else {
          const previous = this.selected;
          this.selected = selected;
          this.reconcileSelectedImage(previous, selected);
        }
      }
      // A completed upload becomes an ordinary row the moment a
      // successful listing of its own destination actually names it —
      // observation, not the response alone. A failed read, a different
      // directory on screen, or a listing that does not carry the file
      // keeps the record, so the user never loses sight of a stored
      // file the panel has not verified.
      this.observeCompletedRecords(listing, completedAtStart);
    } catch (err) {
      if (seq !== this.listSeq) return;
      const missing = err instanceof ApiError && (err.status === 404 || err.code === "not_found");
      if (quiet && missing && this.dir !== "") {
        this.dir = this.parentDir();
        await this.refresh(true);
        return;
      }
      // A quiet re-poll is another read of the same directory, so its last
      // good rows remain useful while a transient failure is shown.
      if (!quiet) this.listing = null;
      this.listError = messageOf(err);
      this.cb.onError?.(this.listError, err);
    } finally {
      if (!quiet && foregroundSeq === this.foregroundListSeq) this.loading = false;
    }
  }

  async open(): Promise<void> {
    if (this.openState) return;
    this.openState = true;
    await this.refresh();
    this.subscribeEvents();
  }

  private subscribeEvents(): void {
    if (!this.openState) return;
    this.unsubscribeEvents?.();
    this.unsubscribeEvents = this.client.subscribeProjectFileEvents(
      this.projectID,
      this.dir,
      () => void this.backgroundRefresh(),
    );
  }

  private async backgroundRefresh(): Promise<void> {
    if (!this.openState) return;
    const previousDir = this.dir;
    const selectedPath = this.selected?.path;
    const selectedMTime = this.selected?.mtime;
    await this.refresh(true);
    if (!this.openState) return;
    if (this.dir !== previousDir) {
      this.subscribeEvents();
    } else if (selectedPath !== undefined && selectedMTime !== this.selected?.mtime) {
      const entry = this.selected;
      if (entry !== null && !isImageName(entry.name)) await this.loadView(entry, true);
    }
    this.cb.onChange?.();
  }

  // loadView fetches the selected file's text. A refusal to decode is a
  // STATE, not an error: the file exists and can still be downloaded.
  async loadView(entry: FileEntry, quiet = false): Promise<void> {
    const seq = ++this.viewSeq;
    if (isImageName(entry.name)) {
      this.loadImage(entry, quiet);
      return;
    }
    if (!quiet) this.view = { kind: "loading" };
    try {
      const view = await this.client.viewProjectFile(this.projectID, entry.path);
      if (seq !== this.viewSeq) return;
      this.view = { kind: "text", content: view.content };
    } catch (err) {
      if (seq !== this.viewSeq) return;
      const failure = viewFailure(err);
      // A background re-read that fails must not blank text the user is
      // reading: report the failure and keep the text, as the listing does.
      // The server's deliberate refusals are not transient — the file really
      // is binary or past the size bound now — so those still replace it.
      if (quiet && failure.kind === "error" && this.view?.kind === "text") {
        this.cb.onError?.(failure.message, err);
        return;
      }
      this.view = failure;
      if (this.view.kind === "error") this.cb.onError?.(this.view.message, err);
    }
  }

  // loadImage starts an image view without reading a byte: the viewer's
  // <img> fetches the image route, and imageDecoded/imageFailed report
  // what came of it. A listed size past the cap is answered here, so
  // nothing is fetched for a file the route would refuse anyway. A
  // background re-read of a shown image keeps its dimensions on screen
  // until the new source decodes.
  private loadImage(entry: FileEntry, quiet: boolean): void {
    if ((entry.size ?? 0) > MAX_IMAGE_VIEW_BYTES) {
      this.view = { kind: "image", image: { state: "oversized" } };
      return;
    }
    const src = this.imageSrc(entry);
    const current = this.view?.kind === "image" ? this.view.image : null;
    if (quiet && current?.state === "shown") {
      this.view = { kind: "image", image: { ...current, src } };
      return;
    }
    this.view = { kind: "image", image: { state: "loading", src } };
  }

  // reconcileSelectedImage follows an open image to the listing's newer
  // generation of it, whatever read produced that listing: an upload that
  // replaced it, a foreground refresh, or a change event. The view is
  // loaded again under the new source, or refused when the file has grown
  // past the cap; a refusal or removal shown for the old generation says
  // nothing about the new one either.
  private reconcileSelectedImage(previous: FileEntry, next: FileEntry): void {
    if (this.view?.kind !== "image" && !(this.view?.kind === "error" && isImageName(next.name))) return;
    if (previous.mtime === next.mtime && previous.size === next.size) return;
    ++this.viewSeq;
    this.loadImage(next, true);
  }

  // imageDecoded records what the browser measured once src decoded. A
  // report for any other source is a late answer for a view that has
  // moved on, and changes nothing.
  imageDecoded(src: string, width: number, height: number, alpha: boolean): boolean {
    const image = this.view?.kind === "image" ? this.view.image : null;
    if (image === null || (image.state !== "loading" && image.state !== "shown") || image.src !== src) return false;
    this.view = { kind: "image", image: { state: "shown", src, width, height, alpha } };
    return true;
  }

  // imageFailed classifies an <img> that did not decode. The element
  // cannot see the response status, so the route is asked again without a
  // body: a 404 is a file removed since the listing, a 413 the size cap,
  // and a 415 or a successful answer is bytes that are not the image they
  // claim to be. Anything else is an error shown verbatim.
  async imageFailed(src: string): Promise<boolean> {
    const image = this.view?.kind === "image" ? this.view.image : null;
    const entry = this.selected;
    if (image === null || entry === null || (image.state !== "loading" && image.state !== "shown") || image.src !== src) {
      return false;
    }
    const seq = this.viewSeq;
    let next: ViewState = { kind: "image", image: { state: "undecodable" } };
    let cause: unknown = undefined;
    try {
      await this.client.checkProjectFileImage(this.projectID, entry.path);
    } catch (err) {
      if (err instanceof ApiError && err.status === 404) next = { kind: "image", image: { state: "gone" } };
      else if (err instanceof ApiError && err.status === 413) next = { kind: "image", image: { state: "oversized" } };
      else if (!(err instanceof ApiError && err.status === 415)) {
        next = { kind: "error", message: messageOf(err) };
        cause = err;
      }
    }
    if (seq !== this.viewSeq) return false;
    this.view = next;
    if (next.kind === "error") this.cb.onError?.(next.message, cause);
    return true;
  }

  // refreshGone answers "No longer on disk": the folder is read again, and
  // a file the new listing still names is opened again rather than left
  // claiming it is gone.
  async refreshGone(): Promise<void> {
    await this.refresh();
    const entry = this.selected;
    if (entry !== null) await this.loadView(entry);
  }

  // upload ingests a pick into the CURRENT directory under each file's
  // own basename, one request at a time, one record per file. A name the
  // server refuses to take is not an error: the record is held at
  // "conflict" and asked about once the clean files have been placed —
  // the ask shows what the server observed, never a possibly stale
  // listing. Files that landed appear only through the refresh at the
  // end, which reads back what the server actually holds; nothing is
  // selected on the model's behalf, so the viewer the user left open
  // stays open.
  async upload(files: readonly File[]): Promise<void> {
    if (this.uploading || this.conflict !== null || files.length === 0) return;
    const resetSeq = this.resetSeq;
    // One picker gesture has one destination. Navigation may remain live
    // while the requests run, but it cannot split the picked batch across
    // directories.
    const destinationDir = this.dir;
    this.uploadErrors = [];
    this.uploading = true;
    this.sameChoice = false;
    this.conflictsAsked = 0;
    // Names this flow must not propose: what the listing shows, every
    // basename in the pick, and everything that lands or is chosen while
    // the batch resolves. Two files of one name in one pick cannot be
    // handed the same alternative either.
    const taken = new Set<string>(this.entries().map((entry) => entry.name));
    const batch: UploadRecord[] = [];
    for (const file of files) {
      const name = uploadBasename(file.name);
      if (name === "") {
        if (resetSeq === this.resetSeq) {
          this.uploadErrors.push(`${file.name}: not a file name`);
        }
        continue;
      }
      taken.add(name);
      batch.push({
        id: ++this.uploadIdSeq,
        file,
        name,
        destDir: destinationDir,
        // The 32 MiB bound is checked before anything is sent: an oversize
        // pick never starts a request.
        status: file.size > UPLOAD_LIMIT_BYTES ? "too-large" : "queued",
        sent: 0,
        total: file.size,
        error: null,
        position: 0,
        batchSize: 0,
        replaces: false,
      });
    }
    for (const [index, record] of batch.entries()) {
      record.position = index + 1;
      record.batchSize = batch.length;
    }
    this.uploadRecords.push(...batch);
    this.cb.onChange?.();
    for (const record of batch) {
      // A closed panel stops dispatching the rest of the pick: the
      // remaining files are work the user dismissed, not work to
      // finish quietly, and reset has already reclassified their records.
      if (resetSeq !== this.resetSeq) return;
      if (record.status !== "queued") continue;
      const outcome = await this.sendRecord(record, resetSeq);
      if (resetSeq !== this.resetSeq) return;
      if (outcome.kind === "conflict") {
        this.conflictQueue.push({ record, destDir: record.destDir, info: outcome.info, reconfirmed: false });
        continue;
      }
      if (outcome.kind === "failed" || outcome.kind === "unconfirmed") {
        this.uploadErrors.push(uploadOutcomeLine(record));
      }
      if (outcome.kind === "failed" && outcome.unauthorized) {
        // Authorization is gone; the rest of the pick would only collect
        // the same refusal, so queued work stops unsent.
        for (const remaining of batch) {
          if (remaining.status === "queued") remaining.status = "stopped";
        }
        break;
      }
    }
    this.uploading = false;
    if (this.conflictQueue.length > 0) {
      this.conflictSession = { resetSeq, taken };
      this.askNextConflict();
      this.cb.onChange?.();
      return;
    }
    await this.finishUpload();
  }

  // sendRecord performs one record's upload through the measured
  // transport: the signal owns cancellation, onProgress carries the
  // browser's own byte counts, and onSent is the upload-side fact that
  // every byte left — which moves the record to "confirming", never to
  // success. Only a valid server response completes it; a request that
  // dies after dispatch leaves the outcome unknown. The outcome is
  // fenced to the controller that started it, so a stale progress event,
  // abort, or late answer cannot touch a newer batch's records.
  private async sendRecord(
    record: UploadRecord,
    resetSeq: number,
    consent?: UploadConsent,
  ): Promise<SendOutcome> {
    const dest = record.destDir === "" ? record.name : `${record.destDir}/${record.name}`;
    record.status = "preparing";
    record.sent = 0;
    record.error = null;
    const controller = new AbortController();
    this.controllers.set(record.id, controller);
    this.cb.onChange?.();
    try {
      await this.client.uploadProjectFile(this.projectID, dest, record.file, consent, {
        signal: controller.signal,
        onProgress: (loaded, total) => {
          if (!this.recordLive(record, resetSeq, controller)) return;
          record.total = total > 0 ? total : record.total;
          record.sent = Math.min(loaded, record.total);
          if (record.status === "preparing" || record.status === "queued") record.status = "uploading";
          this.cb.onProgress?.(record);
        },
        onSent: () => {
          if (!this.recordLive(record, resetSeq, controller)) return;
          record.sent = record.total;
          record.status = "confirming";
          this.cb.onChange?.();
        },
      });
      if (!this.recordLive(record, resetSeq, controller)) return { kind: "dead" };
      this.controllers.delete(record.id);
      record.sent = record.total;
      record.status = "complete";
      return { kind: "stored" };
    } catch (err) {
      if (!this.recordLive(record, resetSeq, controller)) return { kind: "dead" };
      this.controllers.delete(record.id);
      if (err instanceof ApiError && err.conflict !== null
          && UPLOAD_CONFLICT_REASONS.includes(err.conflict.reason)) {
        record.status = "conflict";
        return { kind: "conflict", info: err.conflict };
      }
      if (err instanceof ApiError) {
        record.status = "failed";
        record.error = err.message;
        const unauthorized = isAuthorizationLoss(err);
        if (unauthorized) {
          for (const queued of this.uploadRecords) {
            if (queued.status === "queued" || queued.status === "conflict") queued.status = "stopped";
          }
          this.conflictQueue = [];
        }
        this.cb.onError?.(err.message, err);
        return { kind: "failed", unauthorized };
      }
      // No server answer arrived: the request was dispatched and bytes
      // may have been written, so the outcome is unknown and the
      // application never retries it on its own.
      record.status = "unconfirmed";
      return { kind: "unconfirmed" };
    }
  }

  // recordLive is whether a callback from this exact send may still
  // mutate its record: the panel generation must match, the controller
  // must still be the registered one, and the record must still be in
  // flight. Cancel, dismiss, reset, and a newer batch each break one of
  // those, and the stale callback finds itself dead.
  private recordLive(record: UploadRecord, resetSeq: number, controller: AbortController): boolean {
    return resetSeq === this.resetSeq
      && this.controllers.get(record.id) === controller
      && (record.status === "preparing" || record.status === "uploading" || record.status === "confirming");
  }

  // activeUploadCount is how many records are between queued and
  // confirming — the count a busy badge reads.
  activeUploadCount(): number {
    return this.uploadRecords.filter((record) =>
      record.status === "queued" || record.status === "preparing"
      || record.status === "uploading" || record.status === "confirming").length;
  }

  // selectUpload points the viewer pane at one record's details. It is
  // the user's gesture only; it takes the file selection because the
  // viewer shows one thing at a time, and it never fires on its own.
  selectUpload(record: UploadRecord | null): void {
    this.selectedUpload = record;
    if (record !== null) {
      ++this.viewSeq;
      this.selected = null;
      this.view = null;
    }
    this.cb.onChange?.();
  }

  // cancelUpload stops one record. A queued record was never sent, and a
  // conflict-held record's requests were all refused — either way nothing
  // further will go out, so it stops cleanly and says so. A record whose
  // request is already dispatching cannot promise the server saw nothing:
  // it aborts and reads "unconfirmed". Stopping a confirmation wait uses
  // the same uncertain outcome: all bytes may already have been stored.
  cancelUpload(record: UploadRecord): void {
    if (this.conflict?.record === record && record.status === "conflict") return;
    if (record.status === "queued" || record.status === "conflict") {
      const queued = this.conflictQueue.findIndex((ask) => ask.record === record);
      if (queued >= 0) this.conflictQueue.splice(queued, 1);
      record.status = "stopped";
      this.cb.onChange?.();
      return;
    }
    if (record.status === "preparing" || record.status === "uploading" || record.status === "confirming") {
      record.status = "unconfirmed";
      this.controllers.get(record.id)?.abort();
      this.controllers.delete(record.id);
      this.cb.onChange?.();
    }
  }

  // dismissUpload drops a finished record from the list — the one way a
  // terminal record leaves besides completion's refresh. Nothing in
  // flight is dismissable.
  dismissUpload(record: UploadRecord): void {
    if (record.status !== "failed" && record.status !== "unconfirmed"
      && record.status !== "stopped" && record.status !== "too-large"
      && record.status !== "complete") return;
    this.uploadRecords = this.uploadRecords.filter((candidate) => candidate !== record);
    if (this.selectedUpload === record) this.selectedUpload = null;
    this.cb.onChange?.();
  }

  // conflictRemaining is how many asks stand behind the current one; the
  // same-choice offer reads it.
  conflictRemaining(): number {
    return this.conflictQueue.length;
  }

  // conflictIncomingName is the name the held file would land under —
  // the picked file's own basename, not an edited alternative.
  conflictIncomingName(): string {
    return uploadBasename(this.conflict?.record.file.name ?? "");
  }

  // conflictDirLabel names the directory the held file was bound for:
  // the project's own name at the root, the directory's leaf below it.
  conflictDirLabel(): string {
    const destDir = this.conflict?.destDir ?? "";
    if (destDir === "") return this.rootLabel();
    return destDir.slice(destDir.lastIndexOf("/") + 1);
  }

  // setConflictName records the editable answer and refuses it on the
  // field the moment it is not worth sending — taken, or not a name. It
  // deliberately fires no change event: the DOM keeps the field's own
  // focus while typing, and a full re-render would steal it. The server
  // still decides at commit.
  setConflictName(name: string): void {
    this.conflictName = name;
    this.conflictNameError = name === "" ? null : this.validateConflictName(name);
  }

  // validateConflictName returns the answer's refusal, or null when the
  // name is worth sending. The server still decides at commit.
  private validateConflictName(name: string): string | null {
    if (name === "") return "Enter a name.";
    if (name === "." || name === ".." || name.includes("\0")) return "That's not a file name.";
    if (name.includes("/")) return "Names can't contain /.";
    if (this.conflictSession?.taken.has(name)) return "That name is taken too.";
    return null;
  }

  // cancelConflict drops the current file — only this file, whatever the
  // same-choice box says. The existing file is untouched and nothing is
  // sent; the record stops as never-sent, the next ask (if any) stands,
  // and a batch whose last ask was cancelled still settles: its earlier
  // landings refresh.
  cancelConflict(): void {
    if (this.confirming || this.conflict === null) return;
    const session = this.conflictSession;
    if (this.conflict.record.status === "conflict") this.conflict.record.status = "stopped";
    this.advanceConflict(false);
    this.settleConfirms(session);
  }

  // confirmUploadAs sends the current file under the edited name — the
  // existing file is never touched. A name the server still refuses comes
  // back as a fresh ask about THAT name, with the error on the field.
  // With the same-choice box checked, each answer the flow advances to is
  // sent under its own suggestion until the batch is spent or one needs
  // the user again. The whole answer is fenced to its session: a
  // reset or a newer pick in between kills it before it can send,
  // advance, or settle anything that is no longer this batch's.
  async confirmUploadAs(): Promise<void> {
    if (this.confirming || this.conflict === null) return;
    const name = this.conflictName;
    const refusal = this.validateConflictName(name);
    if (refusal !== null) {
      this.conflictNameError = refusal;
      this.cb.onChange?.();
      return;
    }
    const session = this.conflictSession;
    if (session === null) return;
    const bulk = this.applySameChoice();
    let landed = await this.sendUploadAs(session, this.conflict, name);
    while (bulk && landed && this.sessionLive(session) && this.conflict !== null && !this.conflict.reconfirmed) {
      landed = await this.sendUploadAs(session, this.conflict, this.conflictName);
    }
    this.settleConfirms(session);
  }

  // replaceConflict carries out the user's Replace answer: the upload
  // is sent again with the version the conflict reported, and the server
  // recomputes the destination's version after staging. A target that
  // changed since the approval — or turned out to be a folder or a link —
  // comes back as the same ask with fresh facts, to be chosen over again;
  // it is never destroyed on the strength of a stale approval. With the
  // same-choice box checked, the answer follows the flow from file to
  // file and stops at any refusal that needs the user again. Like
  // every answer, it is fenced to its session — a stale continuation
  // must not send a replacement for a newer batch's file.
  async replaceConflict(): Promise<void> {
    if (this.confirming || this.conflict === null) return;
    if (this.conflict.info.reason !== "exists" && this.conflict.info.reason !== "changed") return;
    const session = this.conflictSession;
    if (session === null) return;
    const bulk = this.applySameChoice();
    let landed = await this.sendReplace(session, this.conflict);
    while (bulk && landed && this.sessionLive(session) && this.conflict !== null && !this.conflict.reconfirmed) {
      landed = await this.sendReplace(session, this.conflict);
    }
    this.settleConfirms(session);
  }

  // sessionLive is whether a captured session still owns the flow: the
  // same object must still be installed, and no reset may have happened
  // since it started.
  private sessionLive(session: ConflictSession): boolean {
    return this.conflictSession === session && session.resetSeq === this.resetSeq;
  }

  // applySameChoice is whether the current answer also covers the queued
  // asks. Cancel never reads it — cancelling is always the current file.
  private applySameChoice(): boolean {
    return this.sameChoice && this.conflictQueue.length > 0;
  }

  // sendUploadAs places one held file under the given free-candidate
  // name through the same measured send as a plain pick. Nothing
  // destructive is in flight: a refusal of the alternative keeps the
  // ORIGINAL ask — its file, its facts, and the version a later Replace
  // would approve — and refuses the name on the field. Every mutation is
  // fenced to the session: a send still in flight when its batch was
  // dismissed leaves the newer batch's flags, queue, and asks alone.
  private async sendUploadAs(session: ConflictSession, ask: UploadConflictAsk, name: string): Promise<boolean> {
    if (!this.sessionLive(session)) return false;
    ask.record.name = name;
    this.confirming = true;
    this.cb.onChange?.();
    let landed = false;
    try {
      const outcome = await this.sendRecord(ask.record, session.resetSeq);
      if (!this.sessionLive(session)) return false;
      if (outcome.kind === "stored") {
        session.taken.add(name);
        landed = true;
      } else if (outcome.kind === "conflict") {
        // Refusing the alternative does not change which original file
        // the dialog names or which version a later Replace approves,
        // and the pending row goes back to the original name so it does
        // not carry the refused alternative as if it were the
        // destination.
        session.taken.add(name);
        ask.record.name = uploadBasename(ask.record.file.name);
        if (ask === this.conflict) {
          this.conflictName = name;
          this.conflictNameError = "That name is taken too.";
        }
        return false;
      } else if (outcome.kind === "failed" || outcome.kind === "unconfirmed") {
        this.uploadErrors.push(uploadOutcomeLine(ask.record));
      }
    } finally {
      // The flag belongs to whichever session is live; a stale send must
      // not clear a newer batch's busy state.
      if (this.sessionLive(session)) this.confirming = false;
    }
    if (ask === this.conflict) this.advanceConflict(landed);
    return landed;
  }

  // sendReplace places one held file over the file its ask names, with
  // the version that ask reported — the one destructive request the panel
  // sends, and only behind the user's explicit Replace on facts the
  // server observed. It flows through the same measured send, so the
  // replacement reads truthfully as a transfer in progress while the
  // original stays on disk until the server confirms. A target that
  // changed since the approval comes back as the same ask with fresh
  // facts, never destroyed on the strength of a stale approval. It is
  // fenced to its session like every send.
  private async sendReplace(session: ConflictSession, ask: UploadConflictAsk): Promise<boolean> {
    if (!this.sessionLive(session) || ask.info.existing.dir
        || (ask.info.reason !== "exists" && ask.info.reason !== "changed")) return false;
    const name = ask.info.existing.name;
    ask.record.name = name;
    ask.record.replaces = true;
    this.confirming = true;
    this.cb.onChange?.();
    let landed = false;
    try {
      const outcome = await this.sendRecord(ask.record, session.resetSeq, {
        version: ask.info.existing.version,
      });
      if (!this.sessionLive(session)) return false;
      if (outcome.kind === "stored") {
        session.taken.add(name);
        landed = true;
      } else if (outcome.kind === "conflict") {
        this.reask(ask, outcome.info);
        return false;
      } else if (outcome.kind === "failed" || outcome.kind === "unconfirmed") {
        this.uploadErrors.push(uploadOutcomeLine(ask.record));
      }
    } finally {
      if (this.sessionLive(session)) this.confirming = false;
    }
    if (ask === this.conflict) this.advanceConflict(landed);
    return landed;
  }
  // reask swaps an ask's facts for what a refusal just reported. The
  // current ask stays on screen (marked re-confirmed when the refusal was
  // a stale approval); a queued ask is returned to the front of the queue
  // to be asked in its turn with the same fresh-facts rule. Callers are
  // session-fenced, so the queue this touches is always the ask's own.
  private reask(ask: UploadConflictAsk, info: UploadConflictInfo): void {
    ask.info = info;
    if (ask === this.conflict) {
      ask.reconfirmed = info.reason === "changed";
      this.conflictName = suggestUploadName(uploadBasename(ask.record.file.name), this.conflictSession?.taken ?? []);
      this.conflictNameError = null;
      this.cb.onChange?.();
      return;
    }
    const index = this.conflictQueue.indexOf(ask);
    if (index >= 0) this.conflictQueue.splice(index, 1);
    this.conflictQueue.unshift(ask);
  }

  // settleConfirms finishes the batch when no ask is current — the common
  // end of every answer path. A batch whose last ask was cancelled
  // settles too: its earlier landings still refresh. Stale answers pass
  // a session that is no longer installed and do nothing.
  private settleConfirms(session: ConflictSession | null): void {
    if (session === null || this.conflictSession !== session) return;
    this.cb.onChange?.();
    if (this.conflict !== null) return;
    this.conflictSession = null;
    if (session.resetSeq === this.resetSeq) {
      void this.finishUpload();
    }
  }

  // advanceConflict moves past the current ask — landed says whether
  // this answer stored the file.
  private advanceConflict(landed: boolean): void {
    this.conflict = null;
    this.conflictName = "";
    this.conflictNameError = null;
    if (this.conflictSession === null) return;
    if (!landed && this.conflictQueue.length === 0) return;
    this.askNextConflict();
  }

  // askNextConflict puts the queue's head on screen with a fresh
  // suggestion, or ends the session when the queue is empty.
  private askNextConflict(): void {
    const session = this.conflictSession;
    if (session === null) return;
    const next = this.conflictQueue.shift() ?? null;
    if (next !== null) this.conflictsAsked++;
    this.conflict = next;
    this.conflictName = next === null
      ? ""
      : suggestUploadName(uploadBasename(next.record.file.name), session.taken);
    this.conflictNameError = null;
  }

  // conflictPosition is where the current ask stands in the batch — the
  // "n of m" the dialog's description reads.
  conflictPosition(): { current: number; total: number } {
    return { current: this.conflictsAsked, total: this.conflictsAsked + this.conflictQueue.length };
  }

  // observeCompletedRecords releases the records a listing has verified:
  // a complete record leaves only when this successful listing is of its
  // own destination directory and actually names the file. Verification
  // is truth-based, so an older in-flight read can never release a newer
  // batch's record it did not observe.
  private observeCompletedRecords(listing: FileListing, completedAtStart: ReadonlySet<number>): void {
    if (this.uploadRecords.length === 0) return;
    this.uploadRecords = this.uploadRecords.filter((record) =>
      !(record.status === "complete" && completedAtStart.has(record.id) && listing.path === record.destDir
        && listing.entries.some((candidate) => !candidate.dir && candidate.name === record.name)));
    if (this.selectedUpload !== null && !this.uploadRecords.includes(this.selectedUpload)) {
      this.selectedUpload = null;
    }
  }

  // finishUpload reads back what the server holds after a batch. It is
  // deliberately selection-free: the refresh reconciles whatever the
  // user already has open, and nothing selects, focuses, or scrolls
  // on the model's behalf while or after uploads move. Which complete
  // records leave is decided by the observation above — the ones this
  // read verified — while every unresolved record stays for the user
  // to dismiss.
  private async finishUpload(): Promise<void> {
    const resetSeq = this.resetSeq;
    await this.refresh();
    if (resetSeq !== this.resetSeq) return;
    this.cb.onChange?.();
  }

  toggleExpanded(): void {
    this.expanded = !this.expanded;
  }

  closeViewer(): void {
    ++this.viewSeq;
    this.selectedUpload = null;
    this.selected = null;
    this.view = null;
    this.expanded = false;
  }

  // Closing clears viewer state but preserves the current directory for
  // the next open. Uploads stop honestly: queued work never sent reads
  // "stopped", an in-flight request is aborted and its record reads
  // "unconfirmed" (bytes may have reached the server), and the records
  // survive the close — including completed ones the panel has not yet
  // seen in a listing — so a reopen shows what actually happened rather
  // than a cleared slate. A pending conflict is dismissed without an
  // answer, and the next open starts from the server's own listing.
  reset(): void {
    this.openState = false;
    this.unsubscribeEvents?.();
    this.unsubscribeEvents = null;
    ++this.resetSeq;
    ++this.listSeq;
    ++this.viewSeq;
    for (const controller of this.controllers.values()) controller.abort();
    this.controllers.clear();
    for (const record of this.uploadRecords) {
      if (record.status === "queued" || record.status === "conflict") record.status = "stopped";
      else if (record.status === "preparing" || record.status === "uploading" || record.status === "confirming") {
        record.status = "unconfirmed";
      }
    }
    this.loading = false;
    this.selected = null;
    this.selectedUpload = null;
    this.view = null;
    this.expanded = false;
    this.uploadErrors = [];
    this.uploading = false;
    this.confirming = false;
    this.conflict = null;
    this.conflictQueue = [];
    this.conflictSession = null;
    this.conflictName = "";
    this.conflictNameError = null;
    this.sameChoice = false;
  }
}

// ConflictSession is one pick's answer flow. Every send, advance, and
// settle is fenced to the session object it started under: a reset or a
// newer pick installs a new session, and a stale loop comparing identity
// finds itself dead instead of acting on the new batch's state.
interface ConflictSession {
  // resetSeq is the generation the batch started at; any bump means the
  // panel was closed or replaced underneath it.
  resetSeq: number;
  // taken is every name this flow has claimed — the listing it read, the
  // pick's basenames, and everything that has landed or been chosen.
  taken: Set<string>;
}

// UploadConflictAsk is one record the server refused to place, held for
// the user's answer: the record itself (its file and its bound
// destination), and the facts the server observed about what holds the
// name.
export interface UploadConflictAsk {
  record: UploadRecord;
  destDir: string;
  info: UploadConflictInfo;
  // reconfirmed marks an ask whose Replace came back refused because the
  // target changed since that approval: the facts are the server's
  // current ones and the user must choose over them again.
  reconfirmed: boolean;
}

// SendOutcome is what one measured send came back with. "dead" means the
// send's controller stopped owning the record (cancelled, dismissed,
// reset, superseded) — its callers must treat the outcome as belonging to
// no live batch and mutate nothing further.
type SendOutcome =
  | { kind: "stored" }
  | { kind: "conflict"; info: UploadConflictInfo }
  | { kind: "failed"; unauthorized: boolean }
  | { kind: "unconfirmed" }
  | { kind: "dead" };

// isAuthorizationLoss names the refusals that mean the browser lost its
// authorization — a 401, or a 403 whose body carries one of the codes the
// control plane answers a pending re-authentication ceremony with. Any
// other 403 stays a per-file refusal.
function isAuthorizationLoss(err: ApiError): boolean {
  return err.status === 401
    || (err.status === 403 && AUTHORIZATION_LOSS_CODES.includes(err.code));
}

// uploadOutcomeLine is the per-record line the error block carries. A
// request that never came back is not a plain failure: the file may have
// landed, so the line says the outcome is unknown rather than claiming the
// file did not upload, and the application never retries it on its own.
function uploadOutcomeLine(record: UploadRecord): string {
  if (record.status === "unconfirmed") {
    return `${record.name}: outcome unknown — no server confirmation was received; check the folder before retrying`;
  }
  return `${record.name}: ${record.error ?? "upload failed"}`;
}

// viewFailure classifies why the viewer has no text. The server's two
// deliberate refusals — bytes that are not UTF-8, and a file past the
// viewer's size bound — are states with a download; anything else is an
// error you should see verbatim.
function viewFailure(err: unknown): ViewState {
  if (err instanceof ApiError) {
    if (err.code === "not_text" || err.status === 415) {
      return { kind: "no-preview", reason: "" };
    }
    if (err.code === "too_large" || err.status === 413) {
      return { kind: "no-preview", reason: err.message };
    }
  }
  return { kind: "error", message: messageOf(err) };
}

function messageOf(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}
