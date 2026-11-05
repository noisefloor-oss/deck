package provider

// VersionProbeArgs returns only probes that exit without starting a session.
// Callers must isolate HOME and XDG directories before running these probes.
func VersionProbeArgs(name Name) []string {
	switch name {
	case CommandCode:
		return []string{"--no-auto-update", "--version"}
	case Grok, Qoder, Claude, Cline, Codex, Cursor, Devin, Kimi, Kilo, Mimo, Omp, Opencode, Pi, Qwen, Shell:
		return []string{"--version"}
	default:
		return nil
	}
}
