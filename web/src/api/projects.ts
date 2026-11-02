export interface Project {
  id: string;
  name: string;
  canonical_path: string;
  git_author_name?: string;
  git_author_email?: string;
  git_identity_profile_id?: string;
  git_identity_profile_revision?: number;
  git_identity_setup_revision?: number;
  last_used_at: string;
}

export interface GitIdentityProfile {
  id: string;
  name: string;
  email: string;
  revision: number;
  created_at: string;
  updated_at: string;
}

export interface GitIdentityCatalog {
  profiles: GitIdentityProfile[];
  default_profile_id: string | null;
  revision: number;
}

// ReplacedGitIdentity names the repository-local git settings a write
// overwrote. The backend sends it only when a write actually replaced
// something, and only for values it read from the repository's own config —
// never from the user's global git config.
export interface ReplacedGitIdentity {
  name?: string;
  email?: string;
  use_config_only?: string;
}

// ProjectWriteResult is a project as returned by a write that also sets the
// repository's git identity. It is a Project plus, when the write overwrote
// local git settings, what those settings were.
export interface ProjectWriteResult extends Project {
  replaced_git_identity?: ReplacedGitIdentity;
}

// Inspection is what the add-project field learns about what was typed,
// answered by the server from the filesystem inside the allowed roots and
// never from the network. kind decides the shape below the field.
export interface Inspection {
  kind: "absent" | "plain" | "repository" | "worktree" | "clone";
  path: string;
  exists: boolean;
  empty?: boolean;
  remote?: string;
  head?: string;
  parent?: ParentRepository;
  store_path?: string;
  entries: string[];
  clone_url?: string;
  same_clone?: boolean;
}

export interface ParentRepository {
  name: string;
  path: string;
}

// CloneJob is a clone in flight or at rest on the server: it outlives the
// dialog that started it, and a half-cloned directory is a state it holds.
export interface CloneJob {
  id: string;
  url: string;
  destination: string;
  name: string;
  status: "running" | "stopped" | "failed" | "done";
  failure?: "bad_or_private" | "auth" | "dest_not_empty" | "network";
  percent: number;
  line?: string;
  stderr?: string;
  partial_bytes: number;
  project_id?: string;
  started_at: string;
  updated_at: string;
}

export interface CloneStartRequest {
  githubAccount?: string;
  githubRepositoryID?: number;
  url: string;
  destination: string;
  name: string;
  profile: GitIdentityProfile | null;
}

export interface CreateProjectOptions {
  // plain registers the directory as it is, with no Git provisioning: a
  // directory that is not a repository, or one inside a repository whose
  // identity already governs it.
  plain?: boolean;
}

export interface ProjectGitIdentitySelection {
  profile_id: string;
  expected_profile_revision: number;
  expected_project_setup_revision: number;
}

export interface ProjectDirectoryEntry {
  name: string;
  path: string;
}

export interface ProjectDirectoryListing {
  current_path: string;
  parent_path: string | null;
  directories: ProjectDirectoryEntry[];
}

export interface StarterCommitResult {
  commit: string;
  created: boolean;
}
