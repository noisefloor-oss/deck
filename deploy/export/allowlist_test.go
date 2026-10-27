package exportgate

// Each exception names one file and only the functional or immutable text it
// needs. Prose citations stay failures, including prose in these same files.
var allowedPrivateReferences = []exception{
	{"deploy/mempalace/sqlite-only.patch", `\(#1068\)`, "Pinned upstream patch context is byte-exact and required for the image's ONNX thread-cap change."},
	{"internal/project/repository_test.go", `"task` + `102"`, "Synthetic nested directory verifies that only immediate projects are listed."},
	{"internal/experiment/handler_test.go", `"task ` + `295"`, "The prompt assertion checks synthetic delegated-task input."},
	{"internal/provider/descriptor_test.go", `accept task ` + `295`, "Synthetic prompt bytes verify delegated CLI arguments."},
	{"internal/session/manager_test.go", `accept task ` + `295`, "Synthetic prompt bytes verify delegated launch forwarding."},
	{"web/src/panels/taskmaster/taskmaster-panel.test.ts", `Task ` + `999|(?:subtask|top-level task) ` + `10`, "Synthetic task references exercise unresolved links and parent-child ID collisions."},
	{"web/src/terminal/task-ref-hover.test.ts", `Task ` + `(126|300|9999)\b|task-` + `(126|200)\b`, "Synthetic terminal references exercise task hover and activation."},
	{"web/src/ui/markdown.test.ts", `Task ` + `(126|999)\b|task-` + `126\b`, "Synthetic references exercise Markdown rendering and hostile input handling."},
	{"web/src/ui/task-ref.test.ts", `Task ` + `1260?|task-` + `1260?|task ?` + `126`, "Synthetic references exercise the task-reference parser's lexical boundaries."},
	{"web/src/preview/preview-states.ts", `Task ` + `30\.2`, "The selector identifies the synthetic task detail dialog."},
	{"web/tools/phone-route-probe.mjs", `Task ` + `30\.2`, "Browser assertions identify the synthetic task detail dialog."},
	{"web/tools/tasks-panel-probe.mjs", `Task ` + `999`, "Synthetic unresolved task references exercise the browser fixture."},
	{"internal/provider/assets/opencode-hook.js", `task ` + `90`, "The embedded hook's exact bytes are checksum-pinned."},
	{"internal/db/migrations/001_initial.sql", requirement, "Released migration bytes are immutable and checksum-verified at startup."},
	{"internal/db/migrations/002_writability_probe.sql", requirement, "Released migration bytes are immutable and checksum-verified at startup."},
	{"internal/db/migrations/003_lease_epoch.sql", requirement, "Released migration bytes are immutable and checksum-verified at startup."},
	{"internal/db/migrations/004_input_dirty.sql", requirement + `|` + prCitation, "Released migration bytes are immutable and checksum-verified at startup."},
	{"internal/db/migrations/005_retire_composer.sql", requirement, "Released migration bytes are immutable and checksum-verified at startup."},
	{"internal/db/migrations/007_session_display_name.sql", taskCitation, "Released migration bytes are immutable and checksum-verified at startup."},
	{"internal/db/migrations/008_shell_provider.sql", requirement, "Released migration bytes are immutable and checksum-verified at startup."},
	{"internal/db/migrations/009_opencode_provider.sql", requirement + `|` + taskCitation, "Released migration bytes are immutable and checksum-verified at startup."},
	{"internal/db/migrations/010_resume_state.sql", requirement, "Released migration bytes are immutable and checksum-verified at startup."},
	{"internal/db/migrations/011_settings.sql", requirement, "Released migration bytes are immutable and checksum-verified at startup."},
	{"internal/db/migrations/012_last_output.sql", requirement, "Released migration bytes are immutable and checksum-verified at startup."},
	{"internal/db/migrations/013_identity_capture_lost.sql", requirement, "Released migration bytes are immutable and checksum-verified at startup."},
	{"internal/db/migrations/015_resume_probation.sql", requirement, "Released migration bytes are immutable and checksum-verified at startup."},
	{"internal/db/migrations/017_boot_epoch.sql", requirement, "Released migration bytes are immutable and checksum-verified at startup."},
	{"internal/db/migrations/019_project_git_identity.sql", requirement, "Released migration bytes are immutable and checksum-verified at startup."},
	{"internal/db/migrations/020_project_policy_authority.sql", requirement, "Released migration bytes are immutable and checksum-verified at startup."},
	{"internal/db/migrations/024_git_identity_profiles.sql", requirement, "Released migration bytes are immutable and checksum-verified at startup."},
}

// The copied skill is provisioned verbatim; its upstream documentation is not
// bundled in Deck. Keep this exception scoped to that single destination.
var allowedBrokenMarkdownLinks = []struct {
	path, finding, reason string
}{
	{
		"deploy/mempalace/skills/mempalace/SKILL.md",
		`broken relative Markdown link "../../website/guide/cursor-hooks.md" (archive path deploy/mempalace/website/guide/cursor-hooks.md)`,
		"The provisioned MemPalace skill preserves upstream bytes, including a link into the upstream documentation tree.",
	},
}
