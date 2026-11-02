package deploy

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The published distributions and the internal stage each is built from.
// deck:latest is the full image; deck:slim is the same Deck with only the
// two harnesses named below. `core` is deliberately absent: it is a build
// stage, not a distribution, because an image with no harness at all is not
// one anyone should pull.
// The stages that become a running image, named by the line that opens
// each, for assertions that read the Dockerfile rather than the flattened
// stream.
var runtimeStages = []string{"AS core\n", "FROM core AS slim", "FROM slim AS runtime", "FROM runtime AS mempalace"}

const (
	coreStage      = "core"
	slimStage      = "slim"
	fullStage      = "runtime"
	mempalaceStage = "mempalace"
)

// Every harness the full image adds on top of slim, identified by the exact
// pinned artifact its install names. A token here must appear in the full
// image and must not appear in slim.
var fullOnlyHarnessArtifacts = []string{
	"@moonshot-ai/kimi-code@",
	"opencode-ai@",
	"command-code@",
	"cline@",
	"@kilocode/cli@",
	"@qwen-code/qwen-code@",
	"@charmland/crush@",
	"@mimo-ai/cli@",
	"@qoder-ai/qodercli@",
	"downloads.cursor.com",
	"@xai-official/grok@",
	"@oh-my-pi/pi-coding-agent@",
	"@earendil-works/pi-coding-agent@",
	"static.devin.ai",
	// bun exists only to run omp; it follows omp out of the slim image.
	"bun.sh/install",
}

// The two harnesses the slim distribution carries.
var slimHarnessArtifacts = []string{
	"downloads.claude.ai/claude-code-releases",
	"@openai/codex@",
}

// imageInstructions returns the flattened instruction graph Docker executes
// to build one target, dependencies first. Sharing the flattener with
// deploy/image-instructions.py keeps the reviewer's diff and this suite's
// assertions reading the same stage graph.
//
// Equal streams mean two files describe the same instruction graph. They do
// not mean two builds produce the same image — the build context, build
// args, and every remote input a RUN fetches are outside the file. That is
// established by building and probing both variants, in release acceptance.
func imageInstructions(t *testing.T, target string) []string {
	t.Helper()
	return flattenDockerfile(t, filepath.Join("..", "Dockerfile"), target)
}

func flattenDockerfile(t *testing.T, path, target string) []string {
	t.Helper()
	cmd := exec.Command("python3", "image-instructions.py", path, target)
	cmd.Dir = "."
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("flatten %s of %s: %v", target, path, err)
	}
	return strings.Split(strings.TrimRight(string(out), "\n"), "\n")
}

// stageText returns one stage's Dockerfile text, from its FROM line to the
// next one. Assertions that can read the file directly do, so that a bug in
// the flattener cannot quietly make several tests agree on the wrong
// answer at once.
func stageText(t *testing.T, dockerfile, from string) string {
	t.Helper()
	parts := strings.SplitN(dockerfile, from, 2)
	if len(parts) != 2 {
		t.Fatalf("Dockerfile has no %q stage", from)
	}
	body := parts[1]
	if next := strings.Index(body, "\nFROM "); next >= 0 {
		body = body[:next]
	}
	return body
}

func dockerfileText(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// Codex invokes ps when its interactive client starts the managed app-server.
// Version and login-subcommand probes do not exercise that startup path.
func TestCoreIncludesCodexProcessInspectionDependency(t *testing.T) {
	core := stageText(t, dockerfileText(t), "AS core\n")
	for _, required := range []string{"\n      procps \\\n", "ps -p $$ -o pid="} {
		if !strings.Contains(core, required) {
			t.Errorf("shared runtime lacks Codex process inspection dependency/probe: %q", required)
		}
	}
}

func TestImageVariantGraphIsCorePlusSlimPlusFull(t *testing.T) {
	dockerfile := dockerfileText(t)
	for _, required := range []string{
		"AS core\n",
		"FROM core AS slim\n",
		"FROM slim AS runtime\n",
		"FROM runtime AS mempalace\n",
	} {
		if !strings.Contains(dockerfile, required) {
			t.Errorf("Dockerfile stage graph is missing %q", required)
		}
	}
	// Go is a Deck build dependency, so no shipped image descends from a
	// stage that installs it.
	for _, retired := range []string{
		"FROM runtime AS toolchains",
		"FROM toolchains AS mempalace",
		"noisedeck-toolchains.sh",
	} {
		if strings.Contains(dockerfile, retired) {
			t.Errorf("Dockerfile retains the retired toolchains target: %q", retired)
		}
	}
	for _, path := range []string{"toolchains-profile.sh", filepath.Join("..", "compose.toolchains.yaml")} {
		if _, err := os.Stat(path); err == nil {
			t.Errorf("%s outlives the toolchains target", path)
		}
	}
}

// The chain is a strict subset chain: the full image's instruction graph is
// the slim image's plus harness installs, so an instruction absent from
// slim's graph is absent from the shared part of full's.
func TestFullImageIsSlimPlusHarnessInstalls(t *testing.T) {
	slim := imageInstructions(t, slimStage)
	full := imageInstructions(t, fullStage)
	if len(full) <= len(slim) {
		t.Fatalf("full image has %d instructions, slim has %d", len(full), len(slim))
	}
	for i, instruction := range slim {
		if full[i] != instruction {
			t.Fatalf("full image diverges from slim at instruction %d:\n slim: %s\n full: %s",
				i, instruction, full[i])
		}
	}
	core := imageInstructions(t, coreStage)
	for i, instruction := range core {
		if slim[i] != instruction {
			t.Fatalf("slim image diverges from core at instruction %d:\n core: %s\n slim: %s",
				i, instruction, slim[i])
		}
	}
}

func TestSlimImageExcludesEveryOtherHarnessAndEveryToolchain(t *testing.T) {
	slim := strings.Join(imageInstructions(t, slimStage), "\n")
	for _, artifact := range fullOnlyHarnessArtifacts {
		if strings.Contains(slim, artifact) {
			t.Errorf("slim image installs %q", artifact)
		}
	}
	for _, excluded := range []string{
		// Go is covered by the COPY assertions below: the build stages
		// in this closure legitimately contain a toolchain, and what
		// matters is that no runtime stage copies one out.
		//
		// No build toolchain: native extension support is a
		// full-image capability, and slim has no Go with which to
		// build Deck in any case.
		"build-essential",
		" gcc ",
		"libc6-dev",
		// MemPalace stays an overlay. The removal helper that the
		// bootstrap runs is core content and is not the payload.
		"/opt/mempalace",
		"MEMPALACE_VERSION",
		"python3-venv",
	} {
		if strings.Contains(slim, excluded) {
			t.Errorf("slim image contains %q", excluded)
		}
	}
	for _, required := range slimHarnessArtifacts {
		if !strings.Contains(slim, required) {
			t.Errorf("slim image does not install %q", required)
		}
	}
}

func TestFullImageStillInstallsEveryHarness(t *testing.T) {
	full := strings.Join(imageInstructions(t, fullStage), "\n")
	for _, artifact := range append(append([]string{}, fullOnlyHarnessArtifacts...), slimHarnessArtifacts...) {
		if !strings.Contains(full, artifact) {
			t.Errorf("full image no longer installs %q", artifact)
		}
	}
	// Native extension support belongs to the full image, and it is
	// build-essential: gcc and libc6-dev alone could not build a native
	// extension, because make and a C++ compiler were never installed.
	if !strings.Contains(full, "build-essential") {
		t.Error("full image no longer installs build-essential")
	}
	for _, superseded := range []string{" gcc \\", " libc6-dev \\"} {
		if strings.Contains(full, superseded) {
			t.Errorf("full image installs %q beside build-essential, which subsumes it", superseded)
		}
	}
}

// Go ships in no runtime image. The build stages that compile Deck are part
// of every variant's dependency closure and legitimately contain a Go
// toolchain, so the assertion is on the only route by which one could reach
// an image — a COPY out of a build stage — rather than on a string a build
// stage may fairly use.
func TestNoRuntimeStageCopiesAToolchainIntoTheImage(t *testing.T) {
	dockerfile := dockerfileText(t)
	// What each runtime stage is allowed to take out of a build stage.
	permitted := map[string]bool{
		"/opt/npm-harnesses/":                            true,
		"/out/noise-deck":                                true,
		"/out/noisedeck-provision":                       true,
		"/out/a2a":                                       true,
		"/out/usr/local/bin/tmux":                        true,
		"/out/usr/local/bin/noise":                       true,
		"/out/usr/local/bin/noise-tasks":                 true,
		"/out/usr/local/bin/noisetask":                   true,
		"/out/usr/local/lib/noise/payload/noise-tasks":   true,
		"/out/usr/local/libexec/noise/noise-visual-diff": true,
		"/out/usr/local/libexec/noise/noise-sdlc":        true,
	}
	copyFrom := regexp.MustCompile(`^COPY\s+--from=\S+\s+(?:--\S+\s+)*(\S+)\s`)
	for _, stage := range runtimeStages {
		for _, line := range strings.Split(stageText(t, dockerfile, stage), "\n") {
			trimmed := strings.TrimSpace(line)
			if !strings.HasPrefix(trimmed, "COPY --from=") {
				continue
			}
			match := copyFrom.FindStringSubmatch(trimmed)
			if match == nil {
				t.Errorf("%s has an unreadable stage copy: %q", stage, trimmed)
				continue
			}
			if !permitted[match[1]] {
				t.Errorf("%s copies %q out of a build stage; only Deck's own built artifacts may cross", stage, match[1])
			}
		}
	}
	// The toolchain path itself appears nowhere a shipped image executes.
	for _, stage := range runtimeStages {
		if body := stageText(t, dockerfile, stage); strings.Contains(body, "/usr/local/go") {
			t.Errorf("%s references the Go toolchain root", stage)
		}
	}
}

// Both distributions are the same Deck: same runtime identity, same
// entrypoint, same command, same healthcheck.
func TestEveryVariantEndsAsTheRuntimeUserWithTheSameEntrypoint(t *testing.T) {
	final := func(target, verb string) string {
		t.Helper()
		last := ""
		for _, instruction := range imageInstructions(t, target) {
			if strings.HasPrefix(instruction, verb) {
				last = instruction
			}
		}
		if last == "" {
			t.Fatalf("%s image has no %s instruction", target, verb)
		}
		return last
	}
	for _, verb := range []string{"USER ", "ENTRYPOINT ", "CMD ", "HEALTHCHECK ", "EXPOSE ", "ENV HOME="} {
		want := final(slimStage, verb)
		for _, target := range []string{fullStage, mempalaceStage} {
			if got := final(target, verb); got != want {
				t.Errorf("%s image final %s = %q, slim has %q", target, verb, got, want)
			}
		}
	}
	if got := final(slimStage, "USER "); got != "USER ${NOISEDECK_RUNTIME_UID}:${NOISEDECK_RUNTIME_GID}" {
		t.Errorf("slim image final USER = %q", got)
	}
}

// Each stage that installs on top of a finished image must take root and
// give it back; an install that silently ran as the runtime user would
// write unusable file ownership.
func TestOverlayStagesTakeRootAndDropBack(t *testing.T) {
	dockerfile := dockerfileText(t)
	for _, stage := range []string{"FROM core AS slim", "FROM slim AS runtime", "FROM runtime AS mempalace"} {
		rest := strings.SplitN(dockerfile, stage, 2)
		if len(rest) != 2 {
			t.Fatalf("Dockerfile has no %q stage", stage)
		}
		body := rest[1]
		if next := strings.Index(body, "\nFROM "); next >= 0 {
			body = body[:next]
		}
		if !strings.Contains(body, "\nUSER root\n") {
			t.Errorf("%s does not take root before installing", stage)
		}
		// Trailing comments introduce the next stage; the last
		// instruction is what the image ends on.
		lines := strings.Split(body, "\n")
		last := ""
		for _, line := range lines {
			if line != "" && !strings.HasPrefix(line, "#") {
				last = line
			}
		}
		if last != "USER ${NOISEDECK_RUNTIME_UID}:${NOISEDECK_RUNTIME_GID}" {
			t.Errorf("%s does not drop back to the runtime identity last", stage)
		}
	}
}

func TestVariantOverlaysSelectTheirBuildTargets(t *testing.T) {
	type composeBuild struct {
		Services map[string]struct {
			Build struct {
				Target string `yaml:"target"`
			} `yaml:"build"`
		} `yaml:"services"`
	}

	read := func(name string) composeBuild {
		t.Helper()
		raw, err := os.ReadFile(filepath.Join("..", name))
		if err != nil {
			t.Fatal(err)
		}
		var doc composeBuild
		if err := yaml.Unmarshal(raw, &doc); err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		return doc
	}

	for _, tc := range []struct{ file, target string }{
		{"compose.yaml", fullStage},
		{"compose.slim.yaml", slimStage},
		{"compose.mempalace.yaml", mempalaceStage},
	} {
		if got := read(tc.file).Services["app"].Build.Target; got != tc.target {
			t.Errorf("%s app build target = %q, want %q", tc.file, got, tc.target)
		}
	}
}

func TestSlimOverlayChangesNothingButTheTarget(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "compose.slim.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	overlay := string(raw)
	for _, unwanted := range []string{"chromium", "NOISE_BROWSER_WS", "volumes:", "environment:"} {
		if strings.Contains(overlay, unwanted) {
			t.Errorf("slim overlay must only select the build target, found %q", unwanted)
		}
	}

	browser, err := os.ReadFile(filepath.Join("..", "compose.browser.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(browser), "network_mode: \"service:app\"") {
		t.Fatal("browser overlay no longer composes through the app service")
	}
}

// The flattener is relied on to show that two Dockerfiles describe the same
// instruction graph, so the failure that matters is the one where it calls
// two different graphs equal. Each case below changes exactly one input a
// naive reading would miss — an inherited base, a stage reached only by
// copy, a global ARG the FROM line interpolates, the parser directive — and
// each must separate.
func TestFlattenerSeparatesTargetsWhoseInstructionGraphsDiffer(t *testing.T) {
	write := func(content string) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "Dockerfile")
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	const digestA = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	const digestB = "sha256:2222222222222222222222222222222222222222222222222222222222222222"

	const directive = "# syntax=docker/dockerfile:1.7\n"

	for _, tc := range []struct{ name, left, right string }{
		{
			name:  "different base image",
			left:  "FROM alpine:3.20 AS runtime\nRUN echo ok\n",
			right: "FROM debian:bookworm AS runtime\nRUN echo ok\n",
		},
		{
			name:  "different digest of one base image",
			left:  "FROM alpine:3.20@" + digestA + " AS runtime\nRUN echo ok\n",
			right: "FROM alpine:3.20@" + digestB + " AS runtime\nRUN echo ok\n",
		},
		{
			// Deck's own shape: the base that changed is two stages
			// below the target being compared.
			name:  "different base beneath an inherited stage",
			left:  "FROM alpine:3.20 AS core\nRUN echo ok\nFROM core AS runtime\nRUN echo more\n",
			right: "FROM debian:bookworm AS core\nRUN echo ok\nFROM core AS runtime\nRUN echo more\n",
		},
		{
			// The case FROM ancestry alone cannot see: nothing
			// inherits from the build stage, the image only copies
			// its output, and what changed is how that output is
			// produced. This is how tmux, the Noise tools and Deck's
			// own binaries enter the real image.
			name: "different instructions in a stage reached only by COPY --from",
			left: "FROM alpine:3.20 AS build\nRUN echo one >/a\n" +
				"FROM debian:bookworm AS runtime\nCOPY --from=build /a /a\n",
			right: "FROM alpine:3.20 AS build\nRUN echo two >/a\n" +
				"FROM debian:bookworm AS runtime\nCOPY --from=build /a /a\n",
		},
		{
			name: "different base in a stage reached only by COPY --from",
			left: "FROM alpine:3.20 AS build\nRUN echo one >/a\n" +
				"FROM debian:bookworm AS runtime\nCOPY --from=build /a /a\n",
			right: "FROM alpine:3.21 AS build\nRUN echo one >/a\n" +
				"FROM debian:bookworm AS runtime\nCOPY --from=build /a /a\n",
		},
		{
			// Transitive: the changed stage is two copies away, which
			// is how web-build reaches the image through go-build.
			name: "different instructions two COPY --from hops away",
			left: "FROM alpine:3.20 AS assets\nRUN echo one >/a\n" +
				"FROM alpine:3.20 AS build\nCOPY --from=assets /a /a\n" +
				"FROM debian:bookworm AS runtime\nCOPY --from=build /a /a\n",
			right: "FROM alpine:3.20 AS assets\nRUN echo two >/a\n" +
				"FROM alpine:3.20 AS build\nCOPY --from=assets /a /a\n" +
				"FROM debian:bookworm AS runtime\nCOPY --from=build /a /a\n",
		},
		{
			name: "different stage behind a RUN mount",
			left: "FROM alpine:3.20 AS build\nRUN echo one >/a\n" +
				"FROM debian:bookworm AS runtime\nRUN --mount=type=bind,from=build,target=/b cp /b/a /a\n",
			right: "FROM alpine:3.20 AS build\nRUN echo two >/a\n" +
				"FROM debian:bookworm AS runtime\nRUN --mount=type=bind,from=build,target=/b cp /b/a /a\n",
		},
		{
			// The frontend that interprets every other line.
			name:  "different parser directive",
			left:  directive + "FROM alpine:3.20 AS runtime\nRUN echo ok\n",
			right: "# syntax=docker/dockerfile:1.9\nFROM alpine:3.20 AS runtime\nRUN echo ok\n",
		},
		{
			// A global ARG's default is what the FROM line
			// interpolates, so it names the base as surely as
			// writing the image out would.
			name:  "different global ARG default behind an interpolated base",
			left:  "ARG BASE=alpine:3.20\nFROM ${BASE} AS runtime\nRUN echo ok\n",
			right: "ARG BASE=debian:bookworm\nFROM ${BASE} AS runtime\nRUN echo ok\n",
		},
		{
			// The same for a version an install pins.
			name:  "different global ARG default used by a stage",
			left:  "ARG TOOL=1.0\nFROM alpine:3.20 AS runtime\nARG TOOL\nRUN install ${TOOL}\n",
			right: "ARG TOOL=2.0\nFROM alpine:3.20 AS runtime\nARG TOOL\nRUN install ${TOOL}\n",
		},
		{
			// Order is part of the graph. These two build different
			// images from the same set of instructions, so a
			// comparison that sorted before diffing would call them
			// equal — which is why the stream is compared as a
			// sequence and any multiset check is labelled as one.
			name:  "same instructions in a different order",
			left:  "FROM alpine:3.20 AS runtime\nRUN touch /a\nRUN rm /a\n",
			right: "FROM alpine:3.20 AS runtime\nRUN rm /a\nRUN touch /a\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			left := flattenDockerfile(t, write(tc.left), "runtime")
			right := flattenDockerfile(t, write(tc.right), "runtime")
			if strings.Join(left, "\n") == strings.Join(right, "\n") {
				t.Fatalf("targets on different bases flatten identically:\n%s", strings.Join(left, "\n"))
			}
		})
	}

	// The control: without a real difference the comparison must still
	// find none, or the assertions above would pass for the wrong reason.
	same := directive + "FROM alpine:3.20 AS core\nRUN echo ok\n" +
		"FROM core AS runtime\nCOPY --from=core /a /a\nRUN echo more\n"
	left := flattenDockerfile(t, write(same), "runtime")
	right := flattenDockerfile(t, write(same), "runtime")
	if strings.Join(left, "\n") != strings.Join(right, "\n") {
		t.Fatal("identical Dockerfiles do not flatten identically")
	}

	// A stage two others depend on is built once, and appears once.
	diamond := write("FROM alpine:3.20 AS build\nRUN echo one >/a\n" +
		"FROM build AS middle\nCOPY --from=build /a /b\n" +
		"FROM debian:bookworm AS runtime\nCOPY --from=build /a /a\nCOPY --from=middle /b /b\n")
	flat := flattenDockerfile(t, diamond, "runtime")
	if got := strings.Count(strings.Join(flat, "\n"), "AS build\n"); got != 1 {
		t.Errorf("shared build stage appears %d times, want once:\n%s", got, strings.Join(flat, "\n"))
	}
	// Every stage the target needs is present, in build order.
	for _, want := range []string{"AS build", "AS middle", "AS runtime"} {
		if !strings.Contains(strings.Join(flat, "\n"), want) {
			t.Errorf("flattened stream omits %q", want)
		}
	}
}

// The same inclusion and exclusion properties, read straight out of the
// Dockerfile rather than through the flattener. Two independent readings
// have to agree; a parser bug can only make one of them wrong.
func TestDockerfileTextKeepsEachHarnessInItsOwnStage(t *testing.T) {
	dockerfile := dockerfileText(t)
	coreBody := stageText(t, dockerfile, "AS core\n")
	slimBody := stageText(t, dockerfile, "FROM core AS slim")
	fullBody := stageText(t, dockerfile, "FROM slim AS runtime")
	fullBody += stageText(t, dockerfile, "AS npm-harnesses\n")

	for _, artifact := range slimHarnessArtifacts {
		if !strings.Contains(slimBody, artifact) {
			t.Errorf("slim stage does not install %q", artifact)
		}
		if strings.Contains(fullBody, artifact) {
			t.Errorf("full stage reinstalls %q, which slim already carries", artifact)
		}
	}
	for _, artifact := range fullOnlyHarnessArtifacts {
		if !strings.Contains(fullBody, artifact) {
			t.Errorf("full stage does not install %q", artifact)
		}
		if strings.Contains(coreBody, artifact) || strings.Contains(slimBody, artifact) {
			t.Errorf("%q is installed at or below the slim stage", artifact)
		}
	}
	// The core stage is what both distributions share, so a toolchain or a
	// compiler landing there reaches slim by construction.
	for _, excluded := range []string{"/usr/local/go", "build-essential", " gcc ", "libc6-dev", "/opt/mempalace"} {
		if strings.Contains(coreBody, excluded) {
			t.Errorf("shared core stage contains %q", excluded)
		}
	}
}

// Discovery only decides anything if nothing overrides it. The base Compose
// file used to pin twelve provider executables by environment, which beats
// PATH resolution: a slim container started that way would have registered
// every harness the environment named, whether or not the image contained
// it. Hook config paths are configuration and stay.
func TestComposePinsNoProviderExecutable(t *testing.T) {
	pinned := regexp.MustCompile(`NOISEDECK_PROVIDERS_[A-Z_]+_EXECUTABLE`)
	for _, file := range []string{"compose.yaml", "compose.slim.yaml", "compose.mempalace.yaml", "compose.go.yaml", "compose.go-mempalace.yaml", "compose.production.yaml", "compose.browser.yaml"} {
		raw, err := os.ReadFile(filepath.Join("..", file))
		if err != nil {
			t.Fatal(err)
		}
		if got := pinned.FindAllString(string(raw), -1); len(got) > 0 {
			t.Errorf("%s pins provider executables, defeating PATH discovery: %v", file, got)
		}
	}
}

// The runtime image carries the age CLI: encrypted backups are made and
// restored with the Go library, but a user generating a throwaway
// identity for the smoke round trip, or reading a recipient off a key,
// needs the executable inside the container.
func TestRuntimeImageInstallsTheAgeCLI(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	script := string(raw)
	// The runtime stage inherits its packages from the core stage.
	start := strings.Index(script, "AS core")
	if start < 0 {
		t.Fatal("core stage not found")
	}
	block := script[start:]
	if end := strings.Index(block, "rm -rf /var/lib/apt/lists/*"); end > 0 {
		block = block[:end]
	}
	if !strings.Contains(block, "\n      age \\\n") {
		t.Error("the runtime stage's apt list must install age")
	}
}
