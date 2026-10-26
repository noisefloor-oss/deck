// Package harnessbump moves the agent harness pins in the Dockerfile: it
// reads what the image ships, asks each upstream what it publishes, rewrites
// the pin and everything that repeats it, and checks a built image against
// the harness's behavioural contract. It decides nothing about shipping on
// its own; the scheduled job opens a pull request and the settle workflow
// merges it once the verdict allows.
package harnessbump

import "fmt"

// Source is where a harness publishes its releases.
type Source string

const (
	// SourceNPM resolves the registry's `latest` dist-tag.
	SourceNPM Source = "npm"
	// SourceClaudeReleases reads the release index Claude Code's installer
	// reads.
	SourceClaudeReleases Source = "claude-releases"
	// SourceCursorInstaller reads the version the Cursor installer script
	// pins today.
	SourceCursorInstaller Source = "cursor-installer"
	// SourceDevinManifest reads the promoted release manifest used by its installer.
	SourceDevinManifest Source = "devin-manifest"
	// SourceManual has no index the job can read; the pin moves by hand.
	SourceManual Source = "manual"
)

// Artifact is one digest-pinned download of a harness.
type Artifact struct {
	// Arch is the Docker TARGETARCH the artifact serves.
	Arch string
	// SHAArg is the Dockerfile ARG that carries the artifact's digest.
	SHAArg string
	// URL builds the download address for a version.
	URL func(version string) string
}

// Repeat is one place outside the Dockerfile that spells the shipped
// version: a package test's literal, the contracts README, a contract's
// launch path, a provider descriptor's constant. Format carries one %s for
// the version. A required repeat that a bump cannot find is an error: the
// catalogue is the enumeration, and a literal it does not know about is a
// pin the bump would leave behind.
type Repeat struct {
	Path     string
	Format   string
	Required bool
}

// Harness is one pinned agent CLI.
type Harness struct {
	// Name matches the contract file and the contracts README row.
	Name string
	// Executable is the command on the image's PATH.
	Executable string
	// VersionArg is the Dockerfile ARG that carries the pin.
	VersionArg string
	Source     Source
	// Package is the npm package for SourceNPM.
	Package   string
	Artifacts []Artifact
	Repeats   []Repeat
}

// Catalogue lists every agent harness the image pins, in Dockerfile order.
// Tools the image also pins (gh, tmux, bun, the Noise tools) are not
// harnesses and move on their own release cadence.
var Catalogue = []Harness{
	{
		Name: "claude", Executable: "claude", VersionArg: "CLAUDE_CODE_VERSION", Source: SourceClaudeReleases,
		Artifacts: []Artifact{
			{Arch: "amd64", SHAArg: "CLAUDE_BINARY_SHA256_AMD64", URL: claudeURL("linux-x64")},
			{Arch: "arm64", SHAArg: "CLAUDE_BINARY_SHA256_ARM64", URL: claudeURL("linux-arm64")},
		},
		// The supervisor's provider profile repeats the version and both
		// digests; Apply moves it as a unit and regolds its test.
		Repeats: []Repeat{readmeRow("claude")},
	},
	{Name: "codex", Executable: "codex", VersionArg: "CODEX_VERSION", Source: SourceNPM, Package: "@openai/codex", Repeats: []Repeat{readmeRow("codex"), loginVersion("codex")}},
	{Name: "kimi", Executable: "kimi", VersionArg: "KIMI_VERSION", Source: SourceNPM, Package: "@moonshot-ai/kimi-code", Repeats: []Repeat{readmeRow("kimi")}},
	{Name: "opencode", Executable: "opencode", VersionArg: "OPENCODE_VERSION", Source: SourceNPM, Package: "opencode-ai", Repeats: []Repeat{readmeRow("opencode")}},
	{Name: "command-code", Executable: "command-code", VersionArg: "COMMAND_CODE_VERSION", Source: SourceNPM, Package: "command-code",
		Repeats: []Repeat{deployTest("command_code_cli_package_test.go", "COMMAND_CODE_VERSION"), readmeRow("command-code")}},
	{Name: "cline", Executable: "cline", VersionArg: "CLINE_VERSION", Source: SourceNPM, Package: "cline",
		Repeats: []Repeat{deployTest("cline_cli_package_test.go", "CLINE_VERSION"), readmeRow("cline")}},
	{Name: "kilo", Executable: "kilo", VersionArg: "KILO_VERSION", Source: SourceNPM, Package: "@kilocode/cli",
		Repeats: []Repeat{deployTest("kilo_cli_package_test.go", "KILO_VERSION"), readmeRow("kilo")}},
	{Name: "qwen", Executable: "qwen", VersionArg: "QWEN_VERSION", Source: SourceNPM, Package: "@qwen-code/qwen-code",
		Repeats: []Repeat{deployTest("qwen_code_cli_package_test.go", "QWEN_VERSION"), readmeRow("qwen")}},
	{Name: "crush", Executable: "crush", VersionArg: "CRUSH_VERSION", Source: SourceNPM, Package: "@charmland/crush",
		// Crush is a CLI that people invoke directly with no Deck provider, so the
		// README names it in prose rather than a row.
		Repeats: []Repeat{deployTest("crush_cli_package_test.go", "CRUSH_VERSION"), {Path: "docs/provider-contracts/README.md", Format: "ships Crush %s as", Required: true}}},
	{Name: "mimo", Executable: "mimo", VersionArg: "MIMO_VERSION", Source: SourceNPM, Package: "@mimo-ai/cli",
		Repeats: []Repeat{deployTest("mimo_code_cli_package_test.go", "MIMO_VERSION"), readmeRow("mimo")}},
	{Name: "qoder", Executable: "qoder", VersionArg: "QODER_VERSION", Source: SourceNPM, Package: "@qoder-ai/qodercli",
		Repeats: []Repeat{deployTest("qoder_cli_package_test.go", "QODER_VERSION"), readmeRow("qoder")}},
	{
		Name: "cursor", Executable: "cursor-agent", VersionArg: "CURSOR_AGENT_VERSION", Source: SourceCursorInstaller,
		Artifacts: []Artifact{
			{Arch: "amd64", SHAArg: "CURSOR_AGENT_SHA256_AMD64", URL: cursorURL("x64")},
			{Arch: "arm64", SHAArg: "CURSOR_AGENT_SHA256_ARM64", URL: cursorURL("arm64")},
		},
		Repeats: []Repeat{
			deployTest("cursor_agent_cli_package_test.go", "CURSOR_AGENT_VERSION"), readmeRow("cursor"),
			// The install directory is the launch target the descriptor and
			// its contract name; a bump that left them behind would lose the
			// process at identity reconciliation.
			{Path: "internal/provider/freshrelaunch.go", Format: `cursorShippedVersion = "%s"`, Required: true},
			{Path: "internal/provider/freshrelaunch_test.go", Format: "/opt/cursor-agent/%s/", Required: true},
			{Path: "docs/provider-contracts/cursor.yaml", Format: "/opt/cursor-agent/%s/", Required: true},
		},
	},
	{Name: "grok", Executable: "grok", VersionArg: "GROK_VERSION", Source: SourceNPM, Package: "@xai-official/grok",
		Repeats: []Repeat{deployTest("grok_cli_package_test.go", "GROK_VERSION"), readmeRow("grok")}},
	{Name: "omp", Executable: "omp", VersionArg: "OMP_VERSION", Source: SourceNPM, Package: "@oh-my-pi/pi-coding-agent",
		Repeats: []Repeat{deployTest("omp_cli_package_test.go", "OMP_VERSION"), readmeRow("omp")}},
	{Name: "pi", Executable: "pi", VersionArg: "PI_VERSION", Source: SourceNPM, Package: "@earendil-works/pi-coding-agent",
		Repeats: []Repeat{deployTest("pi_cli_package_test.go", "PI_VERSION"), readmeRow("pi"), loginVersion("pi"), {Path: "internal/provider/assets/conversation-transport.test.mjs", Format: `assert.equal(pkg.version, "%s");`}}},
	{
		Name: "devin", Executable: "devin", VersionArg: "DEVIN_VERSION", Source: SourceDevinManifest,
		Artifacts: []Artifact{
			{Arch: "amd64", SHAArg: "DEVIN_SHA256_AMD64", URL: devinURL("x86_64")},
			{Arch: "arm64", SHAArg: "DEVIN_SHA256_ARM64", URL: devinURL("aarch64")},
		},
		Repeats: []Repeat{
			deployTest("devin_cli_package_test.go", "DEVIN_VERSION"), readmeRow("devin"),
			{Path: "internal/provider/devin.go", Format: "/opt/devin/%s/", Required: true},
			// Update deployment paths, not historical version_tested evidence.
			{Path: "docs/provider-contracts/devin.yaml", Format: "/opt/devin/%s/", Required: true},
		},
	},
}

// Lookup finds a catalogue entry by name.
func Lookup(name string) (Harness, error) {
	for _, h := range Catalogue {
		if h.Name == name {
			return h, nil
		}
	}
	return Harness{}, fmt.Errorf("harnessbump: no harness named %q", name)
}

// readmeRow is the contracts README's shipped column for a harness.
func readmeRow(name string) Repeat {
	return Repeat{Path: "docs/provider-contracts/README.md", Format: "| " + name + " | %s |", Required: true}
}

// deployTest is the package test that asserts the Dockerfile's ARG line.
func deployTest(file, arg string) Repeat {
	return Repeat{Path: "deploy/" + file, Format: "ARG " + arg + "=%s", Required: true}
}

func claudeURL(platform string) func(string) string {
	return func(version string) string {
		return "https://downloads.claude.ai/claude-code-releases/" + version + "/" + platform + "/claude"
	}
}

func cursorURL(arch string) func(string) string {
	return func(version string) string {
		return "https://downloads.cursor.com/lab/" + version + "/linux/" + arch + "/agent-cli-package.tar.gz"
	}
}

func devinURL(arch string) func(string) string {
	return func(version string) string {
		return "https://static.devin.ai/cli/" + version + "/devin-" + version + "-" + arch + "-unknown-linux.tar.gz"
	}
}

// loginVersion keeps executable admission aligned with the image, without a manual hold.
func loginVersion(name string) Repeat {
	return Repeat{Path: "internal/harnesslogin/versions.go", Format: name + `Version = "%s"`, Required: true}
}
