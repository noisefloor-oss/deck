# Project tasks

Task boards are driven by the bundled Tasks engine, not by Deck itself.
The image pins Tasks v2.9.1 at commit
`5961c31a5854670f96a64c83a5d8f917bb0889b7` and uses `noise tasks` as its
command-line interface.

Deck discovers each project's task state through the engine's versioned,
read-only `noise tasks context --json` schema 5 contract. The only
task-state authority is the repository-family SQLite store at
`<git-common-dir>/noise-tasks/tasks.sqlite`. Linked worktrees of one
repository share current revisions and claims, and no file inside a
worktree is task state.

A task family whose one-way migration from the retired JSON store is
incomplete fails closed. The engine's own migration guide owns that
procedure.

## Native macOS

Native macOS supports task initialization, manual tasks, board reads and
PRD uploads through the configured Tasks CLI. Generate tasks from a PRD on
Linux or macOS through Deck's descriptor handoff. On other platforms, PRD
task generation is not supported; add tasks manually or use the Tasks CLI.
The first-project tutorial does not need PRD generation.
