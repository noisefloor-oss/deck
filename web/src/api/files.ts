import type { AssetDeleteFile, AssetDeleteResult, AssetListing } from "./assets.ts";

// FileEntry is one item in a project directory listing. Path is always
// project-relative and slash-separated: the server never discloses the
// absolute layout above the project root.
export interface FileEntry {
  name: string;
  path: string;
  dir: boolean;
  // size is omitted for a zero-byte entry (Go omitempty), so a reader
  // must treat its absence as zero rather than as unknown.
  size?: number;
  mode: string;
  mtime: string;
  // change is set when git reports the entry differs from the last commit
  // (a directory: when something inside it does). Absent means unchanged,
  // or that git could not say.
  change?: "modified" | "new";
}

// FileListing is one directory inside a project. parent is absent at the
// root; truncated reports that the directory held more entries than the
// server will list — a short list must never be rendered as a complete
// one.
export interface FileListing {
  path: string;
  parent?: string;
  at_root: boolean;
  head?: string;
  entries: FileEntry[];
  truncated?: boolean;
}

// FileView is the text viewer's payload. The server decodes and validates
// UTF-8; a file it refuses (415 not_text) has no text answer at all.
export interface FileView {
  content: string;
}

// FileUploadResult is what POST /files stored: the project-relative path
// after the server's own normalization, and the byte count it wrote.
export interface FileUploadResult {
  path: string;
  size: number;
}

// UploadConflictReason is why the server refused to place an upload: the
// name is taken ("exists"), what took it changed since a replacement was
// approved ("changed"), or it is held by something no upload may ever
// replace — a folder ("directory") or a non-regular file such as a link
// ("not_a_file").
export const UPLOAD_CONFLICT_REASONS = ["exists", "changed", "directory", "not_a_file"] as const;

export type UploadConflictReason = typeof UPLOAD_CONFLICT_REASONS[number];

// UploadConflictInfo is the ask inside the upload endpoint's 409: what
// holds the destination name, as the server observed it at the moment of
// refusal. The version is opaque; returning it as consent is what
// approves replacing the file it names.
export interface UploadConflictInfo {
  reason: UploadConflictReason;
  message: string;
  existing: {
    name: string;
    dir: boolean;
    size: number;
    mtime: string;
    version: string;
  };
}

// UploadConsent is the user's Replace answer made specific: the
// version carried by the conflict that asked. The server recomputes the
// destination's version after staging and re-asks when it differs, so a
// target that changed after the approval is never silently overwritten.
export interface UploadConsent {
  version: string;
}

// UploadTransportOptions is the measured-upload surface: an AbortSignal
// for user cancellation, and the two upload-side observations a
// progress UI needs. onProgress counts bytes the browser itself measured
// handing to the socket; onSent says the last byte left the browser.
// Neither is completion — only the server's response says the file was
// placed, so a caller must keep treating the upload as unconfirmed
// between onSent and the promise settling.
export interface UploadTransportOptions {
  signal?: AbortSignal;
  onProgress?: (loaded: number, total: number) => void;
  onSent?: () => void;
}

// The per-project file surface is separate so session-only fixture clients do
// not need to implement file operations. HttpApiClient implements both.
export interface FileApiClient {
  listProjectFiles(projectID: string, path: string): Promise<FileListing>;
  viewProjectFile(projectID: string, path: string): Promise<FileView>;
  diffProjectFile(projectID: string, path: string): Promise<FileView>;
  subscribeProjectFileEvents(projectID: string, path: string, onChange: () => void): () => void;
  projectFileDownloadUrl(projectID: string, path: string): string;
  // version names the listing generation the URL is for, so a changed
  // file gets a different URL and an <img> fetches it again.
  projectFileImageUrl(projectID: string, path: string, version?: string): string;
  checkProjectFileImage(projectID: string, path: string): Promise<void>;
  uploadProjectFile(projectID: string, destPath: string, body: Blob, consent?: UploadConsent, options?: UploadTransportOptions): Promise<FileUploadResult>;
  listProjectAssets(projectID: string): Promise<AssetListing>;
  deleteProjectAssets(projectID: string, files: AssetDeleteFile[]): Promise<AssetDeleteResult[]>;
  // onClosed fires when the feed has stopped for good and will not reconnect.
  subscribeProjectAssetEvents(projectID: string, onChange: () => void, onClosed?: () => void): () => void;
}
