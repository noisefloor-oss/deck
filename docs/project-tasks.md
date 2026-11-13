# Project tasks

Task boards are driven by the bundled Tasks engine, not by Deck itself.
The image pins Tasks v2.11.0 at commit
`3172edd94b6ca468817e6f7d4eeada6d8671e08f` and uses `noise tasks` as its
command-line interface.

Deck discovers each project's task state through the engine's versioned,
read-only `noise tasks context --json` schema 5 contract. The only
task-state authority is the repository-family SQLite store at
`<git-common-dir>/noise-tasks/tasks.sqlite`. Linked worktrees of one
repository share current revisions and claims, and no file inside a
worktree is task state. Tasks 2.11.0 uses SQLite store schema 6; the context
JSON schema remains 5. Migration or adoption recovery blocks access until
the engine completes recovery.

Before the first mutation with Tasks 2.11.0, stop family writers, create and
verify a backup, and update every checkout and linked worktree to the same
release. The first mutation upgrades store schema 5 to 6; older binaries then
refuse that store. Read-only context inspection does not perform the upgrade.

A task family whose one-way migration from the retired JSON store is
incomplete fails closed. The engine's own migration guide owns that
procedure.

## Native macOS

Native macOS supports task initialization, manual tasks, board reads and
PRD uploads through the configured Tasks CLI. Generate tasks from a PRD on
Linux or macOS through Deck's descriptor handoff. On other platforms, PRD
task generation is not supported; add tasks manually or use the Tasks CLI.
The first-project tutorial does not need PRD generation.
