// The Assets panel model: the files agents wrote into the five .deck/
// locations, your selection among them, and what happened to the
// last delete. DOM-free so the node test runner can drive every fold. The
// server owns the listing and every outcome; the one word this side adds is
// "unconfirmed", for a file whose delete was sent and never answered.
import { formatFileSize } from "../../lib/format.ts";
import { ApiError } from "../../api/http.ts";
import type { AssetDeleteFile, AssetDeleteResult, AssetEntry, AssetListing, AssetLocation } from "../../api/assets.ts";
import type { FileApiClient } from "../../api/files.ts";
import { WATCH_INTERVAL_MS } from "./assets-news.ts";

export type AssetsPanelState = "loading" | "ready" | "unavailable";

export type AssetOutcomeKind = "deleted" | "skipped" | "unconfirmed";

export interface AssetOutcome {
  path: string;
  size: number;
  outcome: AssetOutcomeKind;
  reason: string;
}

// The server refuses a delete of more than this many files whole, so a
// larger selection goes out in requests of at most this size.
export const DELETE_BATCH = 500;

// The scan bound the truncated note names. The server stops each location
// at this many entries.
export const SCAN_BOUND = 500;

export const UNCONFIRMED_REASON = "Delete sent; no answer came back from the workspace";

const IMAGE_EXTENSIONS = new Set(["png", "jpg", "jpeg", "gif", "webp", "bmp", "avif"]);

type AssetClient = Pick<FileApiClient, "listProjectAssets" | "deleteProjectAssets" | "subscribeProjectAssetEvents">;

export interface AssetsPanelCallbacks {
  onChange?: () => void;
  onError?: (message: string, cause: unknown) => void;
}

export class AssetsPanelModel {
  state: AssetsPanelState = "loading";
  listing: AssetListing | null = null;
  error: string | null = null;
  deleting = false;
  // The answer to the last delete, in the order the files were sent. The
  // shell shows it as the outcome dialog until you dismiss it.
  lastDelete: AssetOutcome[] | null = null;
  private client: AssetClient;
  private projectID: string;
  private cb: AssetsPanelCallbacks;
  // Selection is keyed by path and holds the version the listing reported
  // when you picked the row, so a file rewritten since is skipped by
  // the server rather than deleted under a newer version.
  private selection = new Map<string, string>();
  // Skipped and unconfirmed files keep their outcome on the row until the
  // next scan; a deleted file leaves the list.
  private outcomes = new Map<string, AssetOutcome>();
  private refreshSeq = 0;
  private refreshing = false;
  private refreshAgain = false;
  private openState = false;
  private unsubscribe: (() => void) | null = null;
  private poll: ReturnType<typeof setInterval> | null = null;

  constructor(client: AssetClient, projectID: string, cb: AssetsPanelCallbacks = {}) {
    this.client = client;
    this.projectID = projectID;
    this.cb = cb;
  }

  get isOpen(): boolean {
    return this.openState;
  }

  // open reads the locations and follows the change feed while the panel is
  // open. A permanently closed feed falls back to polling; close stops both.
  async open(): Promise<void> {
    if (this.openState) return;
    this.openState = true;
    this.unsubscribe?.();
    this.unsubscribe = this.client.subscribeProjectAssetEvents(
      this.projectID,
      () => void this.refresh(),
      () => {
        if (!this.openState) return;
        this.startPolling();
        void this.refresh();
      },
    );
    await this.refresh();
  }

  private startPolling(): void {
    if (!this.openState || this.poll !== null) return;
    this.poll = setInterval(() => void this.refresh(), WATCH_INTERVAL_MS);
  }

  close(): void {
    this.openState = false;
    ++this.refreshSeq;
    this.unsubscribe?.();
    this.unsubscribe = null;
    if (this.poll !== null) clearInterval(this.poll);
    this.poll = null;
  }

  dispose(): void {
    this.close();
    ++this.refreshSeq;
  }

  // One scan owns the listing at a time. Events during it fold into one
  // follow-up instead of superseding each other and discarding every answer.
  async refresh(): Promise<void> {
    if (this.refreshing) {
      this.refreshAgain = true;
      return;
    }
    this.refreshing = true;
    try {
      do {
        this.refreshAgain = false;
        await this.scan();
      } while (this.refreshAgain && this.openState);
    } finally {
      this.refreshing = false;
    }
  }

  private async scan(): Promise<void> {
    const seq = ++this.refreshSeq;
    let listing: AssetListing;
    try {
      listing = await this.client.listProjectAssets(this.projectID);
    } catch (cause) {
      if (seq !== this.refreshSeq) return;
      this.error = cause instanceof Error ? cause.message : String(cause);
      // A failed rescan keeps the rows you were looking at.
      if (this.listing === null) this.state = "unavailable";
      else this.cb.onError?.("The agent locations could not be scanned", cause);
      this.cb.onChange?.();
      return;
    }
    if (seq !== this.refreshSeq) return;
    this.listing = listing;
    this.state = "ready";
    this.error = null;
    this.outcomes.clear();
    const present = new Set(this.entries().filter(isDeletable).map((entry) => entry.path));
    for (const path of [...this.selection.keys()]) {
      if (!present.has(path)) this.selection.delete(path);
    }
    this.cb.onChange?.();
  }

  get locations(): AssetLocation[] {
    return this.listing?.locations ?? [];
  }

  entries(): AssetEntry[] {
    return this.locations.flatMap((location) => location.entries);
  }

  get empty(): boolean {
    return this.state === "ready" && this.entries().length === 0;
  }

  get truncated(): AssetLocation[] {
    return this.locations.filter((location) => location.truncated);
  }

  isSelected(path: string): boolean {
    return this.selection.has(path);
  }

  outcomeFor(path: string): AssetOutcome | undefined {
    return this.outcomes.get(path);
  }

  // toggle flips one deletable row. A locked row or a folder is view-only
  // and never joins the selection, whatever gesture reached it.
  toggle(path: string): void {
    const entry = this.entries().find((candidate) => candidate.path === path);
    if (entry === undefined || !isDeletable(entry)) return;
    if (this.selection.has(path)) this.selection.delete(path);
    else this.selection.set(path, entry.version);
    this.cb.onChange?.();
  }

  clearSelection(): void {
    if (this.selection.size === 0) return;
    this.selection.clear();
    this.cb.onChange?.();
  }

  get selectedCount(): number {
    return this.selection.size;
  }

  selectedEntries(): AssetEntry[] {
    return this.entries().filter((entry) => this.selection.has(entry.path));
  }

  // The footer's line: what is listed at rest, what is picked otherwise.
  // Totals count files only; a truncated scan says its count is a listing.
  get summary(): string {
    if (this.selection.size > 0) {
      const picked = this.selectedEntries();
      return `${picked.length} selected · ${formatFileSize(totalSize(picked))}`;
    }
    const files = this.entries().filter((entry) => entry.kind === "file");
    const listed = this.truncated.length > 0 ? " listed" : "";
    return `${files.length} ${files.length === 1 ? "file" : "files"}${listed} · ${formatFileSize(totalSize(files))}`;
  }

  // delete sends the selection, each file with the version it was picked
  // at, and folds the answers. A file the server answered keeps the
  // server's word; a file in a request that failed after it was sent, or
  // one the answer left out, is unconfirmed. A refusal of the whole request
  // is an answer: those files are skipped with the server's reason.
  async delete(): Promise<AssetOutcome[]> {
    const targets = this.selectedEntries().map((entry) => ({
      path: entry.path,
      version: this.selection.get(entry.path) ?? entry.version,
      size: entry.size,
    }));
    if (targets.length === 0 || this.deleting) return [];
    this.deleting = true;
    this.cb.onChange?.();
    const outcomes: AssetOutcome[] = [];
    for (let start = 0; start < targets.length; start += DELETE_BATCH) {
      const batch = targets.slice(start, start + DELETE_BATCH);
      const files: AssetDeleteFile[] = batch.map(({ path, version }) => ({ path, version }));
      let results: AssetDeleteResult[] | null = null;
      let refused: string | null = null;
      try {
        results = await this.client.deleteProjectAssets(this.projectID, files);
      } catch (cause) {
        if (cause instanceof ApiError && cause.status >= 400 && cause.status < 500) {
          refused = `The workspace refused the request: ${cause.message}`;
        }
      }
      const answered = new Map((results ?? []).map((result) => [result.path, result]));
      for (const target of batch) {
        const result = answered.get(target.path);
        if (refused !== null) {
          outcomes.push({ path: target.path, size: target.size, outcome: "skipped", reason: refused });
        } else if (result === undefined) {
          outcomes.push({ path: target.path, size: target.size, outcome: "unconfirmed", reason: UNCONFIRMED_REASON });
        } else {
          outcomes.push({ path: target.path, size: target.size, outcome: result.outcome, reason: result.reason ?? "" });
        }
      }
    }
    this.deleting = false;
    this.selection.clear();
    const deleted = new Set(outcomes.filter((o) => o.outcome === "deleted").map((o) => o.path));
    for (const location of this.locations) {
      location.entries = location.entries.filter((entry) => !deleted.has(entry.path));
    }
    for (const outcome of outcomes) {
      if (outcome.outcome !== "deleted") this.outcomes.set(outcome.path, outcome);
    }
    this.lastDelete = outcomes;
    this.cb.onChange?.();
    return outcomes;
  }

  dismissOutcome(): void {
    this.lastDelete = null;
    this.cb.onChange?.();
  }
}

export function isDeletable(entry: AssetEntry): boolean {
  return entry.kind === "file" && entry.locked === null;
}

export function isImageAsset(entry: AssetEntry, maxBytes: number): boolean {
  if (entry.kind !== "file" || entry.size > maxBytes) return false;
  const dot = entry.name.lastIndexOf(".");
  return dot >= 0 && IMAGE_EXTENSIONS.has(entry.name.slice(dot + 1).toLowerCase());
}

export function totalSize(entries: { size: number }[]): number {
  return entries.reduce((sum, entry) => sum + entry.size, 0);
}

const WORDS = ["zero", "one", "two", "three", "four", "five", "six", "seven", "eight", "nine"];

// countWord spells a count the way the dialogs read it: words up to nine,
// digits from ten.
export function countWord(n: number, capital = false): string {
  const word = n < WORDS.length ? WORDS[n]! : String(n);
  return capital ? word.charAt(0).toUpperCase() + word.slice(1) : word;
}

export function filesNoun(n: number): string {
  return n === 1 ? "file" : "files";
}

// The consent's title and its confirming button.
export function deleteTitle(n: number): string {
  return `Delete ${n} ${filesNoun(n)}?`;
}

export function deleteAction(n: number): string {
  return `Delete ${n} ${filesNoun(n)}`;
}

// The outcome's title: "2 of 5 files deleted", or "5 files deleted" when
// every file went.
export function outcomeTitle(outcomes: AssetOutcome[]): string {
  const deleted = outcomes.filter((o) => o.outcome === "deleted").length;
  const total = outcomes.length;
  return deleted === total
    ? `${total} ${filesNoun(total)} deleted`
    : `${deleted} of ${total} ${filesNoun(total)} deleted`;
}

// The outcome's description, built from the counts: what was left alone,
// then what nobody can vouch for. Empty when every file was deleted.
export function outcomeDescription(outcomes: AssetOutcome[]): string {
  const skipped = outcomes.filter((o) => o.outcome === "skipped").length;
  const unconfirmed = outcomes.filter((o) => o.outcome === "unconfirmed").length;
  const parts: string[] = [];
  if (skipped === 1) parts.push("One was skipped and nothing was done to it.");
  else if (skipped > 1) parts.push(`${countWord(skipped, true)} were skipped and nothing was done to them.`);
  if (unconfirmed === 1) parts.push("One delete is unconfirmed — rescan to see whether it's still there.");
  else if (unconfirmed > 1) parts.push(`${countWord(unconfirmed, true)} deletes are unconfirmed — rescan to see whether they're still there.`);
  return parts.join(" ");
}

export const OUTCOME_WORD: Record<AssetOutcomeKind, string> = {
  deleted: "Deleted",
  skipped: "Skipped",
  unconfirmed: "Unconfirmed",
};

// The row's fact line after a delete: the outcome word and the reason in
// the row's short form.
export function outcomeFact(outcome: AssetOutcome): string {
  if (outcome.outcome === "unconfirmed") return "Unconfirmed — delete sent, no answer";
  const reason = outcome.reason === "" ? "" : ` — ${outcome.reason.charAt(0).toLowerCase()}${outcome.reason.slice(1)}`;
  return `${OUTCOME_WORD[outcome.outcome]}${reason}`;
}

// The truncated note names every location the scan stopped in.
export function truncatedNote(locations: AssetLocation[]): string {
  const names = locations.map((location) => location.name);
  const joined = names.length <= 1
    ? (names[0] ?? "")
    : `${names.slice(0, -1).join(", ")} and ${names.at(-1)}`;
  return `The scan stopped at ${SCAN_BOUND} entries in ${joined}, so some files there aren't listed. Totals count listed files only.`;
}
