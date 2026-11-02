package harnessbump

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const fixtureDockerfile = `FROM scratch AS slim
ARG CLAUDE_CODE_VERSION=2.1.258
ARG CLAUDE_BINARY_SHA256_AMD64=704f1334ac65d3e89e1c6c1d7663293ad786a6166afdb71b5075337df630f976
ARG CLAUDE_BINARY_SHA256_ARM64=43dc490af55262edcb3e9b1cb315de22cc09ccb08bd52a4c39bc5eabaa63100f
ARG CODEX_VERSION=0.152.1
RUN npm install --global "@openai/codex@${CODEX_VERSION}"
FROM slim AS runtime
ARG KIMI_VERSION=0.39.1
ARG OMP_VERSION=18.1.2
ARG CRUSH_VERSION=0.92.0
ARG CURSOR_AGENT_VERSION=2026.08.31-4057e58
ARG CURSOR_AGENT_SHA256_AMD64=7e306db5750219a99c00ed517fe8b235d3c54e4ca5f77e2ff160cc97ce707798
ARG CURSOR_AGENT_SHA256_ARM64=cf5db6b5047b3280d8a49471cfd41beb1d5e475774177df5df2851857ab6514a
`

func fixtureRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	write := func(rel, body string) {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("Dockerfile", fixtureDockerfile)
	write("deploy/omp_cli_package_test.go", "package deploy\n// \"ARG OMP_VERSION=18.1.2\"\n")
	write("deploy/other_test.go", "package deploy\n// nothing pinned here\n")
	write("deploy/crush_cli_package_test.go", "package deploy\n// \"ARG CRUSH_VERSION=0.92.0\"\n")
	write("deploy/cursor_agent_cli_package_test.go", "package deploy\n// \"ARG CURSOR_AGENT_VERSION=2026.08.31-4057e58\",\n// \"ARG CURSOR_AGENT_SHA256_AMD64=7e306db5750219a99c00ed517fe8b235d3c54e4ca5f77e2ff160cc97ce707798\",\n// \"ARG CURSOR_AGENT_SHA256_ARM64=cf5db6b5047b3280d8a49471cfd41beb1d5e475774177df5df2851857ab6514a\",\n")
	write("internal/provider/freshrelaunch.go", "package provider\nconst cursorShippedVersion = \"2026.08.31-4057e58\"\n")
	write("internal/provider/freshrelaunch_test.go", "package provider\n// /opt/cursor-agent/2026.08.31-4057e58/dist-package/node\n")
	write("docs/provider-contracts/cursor.yaml", "executable:\n  launch_target: \"/opt/cursor-agent/2026.08.31-4057e58/dist-package/index.js\"\n  proc_exe: \"/opt/cursor-agent/2026.08.31-4057e58/dist-package/node\"\n")
	write("internal/supervisor/provider_profile.go", `package supervisor
	case "amd64":
		executableDigest = "704f1334ac65d3e89e1c6c1d7663293ad786a6166afdb71b5075337df630f976"
	case "arm64":
		executableDigest = "43dc490af55262edcb3e9b1cb315de22cc09ccb08bd52a4c39bc5eabaa63100f"
		TestedVersion:             "2.1.258",
`)
	write("internal/supervisor/provider_profile_test.go", `package supervisor
		wantJSON = `+"`"+`{"tested_version":"2.1.258","executable_sha256":"704f1334ac65d3e89e1c6c1d7663293ad786a6166afdb71b5075337df630f976"}`+"`"+`
		wantDigest = "0000000000000000000000000000000000000000000000000000000000000000"
		wantJSON = `+"`"+`{"tested_version":"2.1.258","executable_sha256":"43dc490af55262edcb3e9b1cb315de22cc09ccb08bd52a4c39bc5eabaa63100f"}`+"`"+`
		wantDigest = "1111111111111111111111111111111111111111111111111111111111111111"
`)
	write("docs/provider-contracts/README.md", "| claude | 2.1.258 | 2.1.220 | x |\n| omp | 18.1.2 | 17.2.12 | x |\n| cursor | 2026.08.31-4057e58 | 2026.08.11-e8db854 | x |\n\nThe image also ships Crush 0.92.0 as a CLI that people invoke directly.\n")
	return root
}

func TestReadPinsTakesTheFirstDeclaration(t *testing.T) {
	pins := ReadPins([]byte("ARG A=1\nARG B=two\nARG A=3\n"))
	if pins["A"] != "1" || pins["B"] != "two" {
		t.Fatalf("pins = %v", pins)
	}
}

func TestApplyMovesThePinAndEveryLiteralThatRepeatsIt(t *testing.T) {
	root := fixtureRepo(t)
	omp, _ := Lookup("omp")
	changed, err := Apply(root, Bump{Harness: omp, From: "18.1.2", To: "18.2.0"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"Dockerfile", "deploy/omp_cli_package_test.go", "docs/provider-contracts/README.md"}
	if strings.Join(changed, ",") != strings.Join(want, ",") {
		t.Fatalf("changed = %v, want %v", changed, want)
	}
	assertContains(t, filepath.Join(root, "Dockerfile"), "ARG OMP_VERSION=18.2.0")
	assertContains(t, filepath.Join(root, "deploy", "omp_cli_package_test.go"), `"ARG OMP_VERSION=18.2.0"`)
	assertContains(t, filepath.Join(root, "docs", "provider-contracts", "README.md"), "| omp | 18.2.0 | 17.2.12 |")
	assertContains(t, filepath.Join(root, "docs", "provider-contracts", "README.md"), "| claude | 2.1.258 |")
}

func TestApplyMovesClaudeProfileDigestsWithTheBinary(t *testing.T) {
	root := fixtureRepo(t)
	claude, _ := Lookup("claude")
	amd := strings.Repeat("a", 64)
	arm := strings.Repeat("b", 64)
	changed, err := Apply(root, Bump{Harness: claude, From: "2.1.258", To: "2.1.260",
		Digests: map[string]string{"CLAUDE_BINARY_SHA256_AMD64": amd, "CLAUDE_BINARY_SHA256_ARM64": arm}})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(changed, ",")
	for _, f := range []string{"Dockerfile", "internal/supervisor/provider_profile.go", "internal/supervisor/provider_profile_test.go", "docs/provider-contracts/README.md"} {
		if !strings.Contains(joined, f) {
			t.Errorf("%s was not changed (changed = %v)", f, changed)
		}
	}
	assertContains(t, filepath.Join(root, "Dockerfile"), "ARG CLAUDE_BINARY_SHA256_AMD64="+amd)
	assertContains(t, filepath.Join(root, "Dockerfile"), "ARG CLAUDE_BINARY_SHA256_ARM64="+arm)
	profile := filepath.Join(root, "internal", "supervisor", "provider_profile.go")
	assertContains(t, profile, `TestedVersion:             "2.1.260"`)
	assertContains(t, profile, `"`+amd+`"`)
	assertContains(t, profile, `"`+arm+`"`)
	golden := filepath.Join(root, "internal", "supervisor", "provider_profile_test.go")
	assertContains(t, golden, `"tested_version":"2.1.260","executable_sha256":"`+amd+`"`)
	assertContains(t, golden, `"tested_version":"2.1.260","executable_sha256":"`+arm+`"`)
	amdSum := sha256.Sum256([]byte(`{"tested_version":"2.1.260","executable_sha256":"` + amd + `"}`))
	armSum := sha256.Sum256([]byte(`{"tested_version":"2.1.260","executable_sha256":"` + arm + `"}`))
	assertContains(t, golden, `wantDigest = "`+hex.EncodeToString(amdSum[:])+`"`)
	assertContains(t, golden, `wantDigest = "`+hex.EncodeToString(armSum[:])+`"`)
	raw, _ := os.ReadFile(golden)
	if strings.Contains(string(raw), "0000000000000000") || strings.Contains(string(raw), "1111111111111111") {
		t.Error("a golden digest was left behind")
	}
}

func TestApplyMovesEveryRepeatOfCursorAndCrush(t *testing.T) {
	root := fixtureRepo(t)
	cursor, _ := Lookup("cursor")
	amd, arm := strings.Repeat("c", 64), strings.Repeat("d", 64)
	changed, err := Apply(root, Bump{Harness: cursor, From: "2026.08.31-4057e58", To: "2026.09.02-c22c1a3",
		Digests: map[string]string{"CURSOR_AGENT_SHA256_AMD64": amd, "CURSOR_AGENT_SHA256_ARM64": arm}})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"Dockerfile", "deploy/cursor_agent_cli_package_test.go", "docs/provider-contracts/README.md",
		"internal/provider/freshrelaunch.go", "internal/provider/freshrelaunch_test.go", "docs/provider-contracts/cursor.yaml"} {
		if !strings.Contains(strings.Join(changed, ","), f) {
			t.Errorf("%s was not changed (changed = %v)", f, changed)
		}
	}
	assertContains(t, filepath.Join(root, "internal", "provider", "freshrelaunch.go"), `cursorShippedVersion = "2026.09.02-c22c1a3"`)
	assertContains(t, filepath.Join(root, "internal", "provider", "freshrelaunch_test.go"), "/opt/cursor-agent/2026.09.02-c22c1a3/dist-package/node")
	assertContains(t, filepath.Join(root, "docs", "provider-contracts", "cursor.yaml"), `proc_exe: "/opt/cursor-agent/2026.09.02-c22c1a3/dist-package/node"`)
	assertContains(t, filepath.Join(root, "deploy", "cursor_agent_cli_package_test.go"), "ARG CURSOR_AGENT_SHA256_AMD64="+amd)
	assertContains(t, filepath.Join(root, "deploy", "cursor_agent_cli_package_test.go"), "ARG CURSOR_AGENT_SHA256_ARM64="+arm)
	assertContains(t, filepath.Join(root, "docs", "provider-contracts", "README.md"), "| cursor | 2026.09.02-c22c1a3 |")
	for _, f := range []string{"internal/provider/freshrelaunch.go", "docs/provider-contracts/cursor.yaml", "deploy/cursor_agent_cli_package_test.go"} {
		raw, _ := os.ReadFile(filepath.Join(root, filepath.FromSlash(f)))
		if strings.Contains(string(raw), "2026.08.31-4057e58") {
			t.Errorf("%s still spells the old version", f)
		}
	}

	crush, _ := Lookup("crush")
	changed, err = Apply(root, Bump{Harness: crush, From: "0.92.0", To: "0.93.0"})
	if err != nil {
		t.Fatal(err)
	}
	assertContains(t, filepath.Join(root, "docs", "provider-contracts", "README.md"), "ships Crush 0.93.0 as")
	if !strings.Contains(strings.Join(changed, ","), "deploy/crush_cli_package_test.go") {
		t.Errorf("crush's package test was not changed: %v", changed)
	}

	// A repeat the tree does not spell is a pin left behind, not a warning,
	// and the refusal leaves every file as it was: the Dockerfile and the
	// repeats before it are not half-moved.
	_ = os.WriteFile(filepath.Join(root, "internal", "provider", "freshrelaunch.go"), []byte("package provider\n"), 0o644)
	before, _ := os.ReadFile(filepath.Join(root, "Dockerfile"))
	if _, err := Apply(root, Bump{Harness: cursor, From: "2026.09.02-c22c1a3", To: "2026.09.03-abcdef0",
		Digests: map[string]string{"CURSOR_AGENT_SHA256_AMD64": amd, "CURSOR_AGENT_SHA256_ARM64": arm}}); err == nil || !strings.Contains(err.Error(), "freshrelaunch.go") {
		t.Errorf("a missing required repeat did not fail closed: %v", err)
	}
	after, _ := os.ReadFile(filepath.Join(root, "Dockerfile"))
	if string(before) != string(after) {
		t.Error("a refused bump wrote the Dockerfile anyway")
	}
	assertContains(t, filepath.Join(root, "deploy", "cursor_agent_cli_package_test.go"), "ARG CURSOR_AGENT_VERSION=2026.09.02-c22c1a3")
}

func TestApplyRefusesWhatItCannotMoveOnce(t *testing.T) {
	root := fixtureRepo(t)
	claude, _ := Lookup("claude")
	if _, err := Apply(root, Bump{Harness: claude, From: "2.1.258", To: "2.1.260"}); err == nil {
		t.Error("a digest-pinned harness moved without digests")
	}
	if _, err := Apply(root, Bump{Harness: claude, From: "2.1.258", To: "2.1.260",
		Digests: map[string]string{"CLAUDE_BINARY_SHA256_AMD64": "nothex", "CLAUDE_BINARY_SHA256_ARM64": strings.Repeat("b", 64)}}); err == nil {
		t.Error("a malformed digest was written")
	}
	omp, _ := Lookup("omp")
	if _, err := Apply(root, Bump{Harness: omp, From: "18.1.2", To: "18.2.0; rm -rf /"}); err == nil {
		t.Error("a version with shell metacharacters was written")
	}
	if _, err := Apply(root, Bump{Harness: omp, From: "18.0.0", To: "18.2.0"}); err == nil {
		t.Error("a From the Dockerfile does not declare was accepted")
	}
	dup := filepath.Join(root, "Dockerfile")
	raw, _ := os.ReadFile(dup)
	_ = os.WriteFile(dup, append(raw, []byte("ARG OMP_VERSION=18.1.2\n")...), 0o644)
	if _, err := Apply(root, Bump{Harness: omp, From: "18.1.2", To: "18.2.0"}); err == nil {
		t.Error("a pin declared twice was moved")
	}
	_ = os.WriteFile(dup, append(raw, []byte("ARG OMP_VERSION=18.1.9\n")...), 0o644)
	if _, err := Apply(root, Bump{Harness: omp, From: "18.1.2", To: "18.2.0"}); err == nil {
		t.Error("a pin declared twice with another value was moved")
	}
	_ = os.WriteFile(dup, raw, 0o644)
	changed, err := Apply(root, Bump{Harness: omp, From: "18.1.2", To: "18.1.2"})
	if err != nil || changed != nil {
		t.Errorf("a no-op bump changed %v, err %v", changed, err)
	}
}

func TestResolverReadsEachUpstreamShape(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/npm/@openai/codex/latest":
			_, _ = w.Write([]byte(`{"name":"@openai/codex","version":"0.153.2"}`))
		case "/claude/latest":
			_, _ = w.Write([]byte("2.1.260\n"))
		case "/cursor":
			_, _ = w.Write([]byte("DOWNLOAD_URL=\"https://downloads.cursor.com/lab/2026.09.02-c22c1a3/${OS}/${ARCH}/agent-cli-package.tar.gz\"\n"))
		case "/cursor-lookalike":
			_, _ = w.Write([]byte("DOWNLOAD_URL=\"https://evil.example/downloads.cursor.com/lab/2026.09.02-c22c1a3/${OS}/${ARCH}/agent-cli-package.tar.gz\"\n"))
		case "/cursor-embedded":
			_, _ = w.Write([]byte("DOWNLOAD_URL=\"https://evil.example/https://downloads.cursor.com/lab/2026.09.02-c22c1a3/${OS}/${ARCH}/agent-cli-package.tar.gz\"\n"))
		case "/cursor-mixed-quote":
			_, _ = w.Write([]byte("DOWNLOAD_URL=\"https://evil.example/'https://downloads.cursor.com/lab/2026.09.02-c22c1a3/${OS}/${ARCH}/agent-cli-package.tar.gz\"\n"))
		case "/cursor-missing":
			_, _ = w.Write([]byte("echo \"https://downloads.cursor.com/lab/2026.09.02-c22c1a3/linux/amd64/agent-cli-package.tar.gz\"\n"))
		case "/cursor-disagree":
			_, _ = w.Write([]byte("DOWNLOAD_URL=\"https://downloads.cursor.com/lab/2026.09.02-c22c1a3/a\"\nexport DOWNLOAD_URL='https://downloads.cursor.com/lab/2026.09.03-abcdef0/a'\n"))
		case "/cursor-agree":
			_, _ = w.Write([]byte("  DOWNLOAD_URL=https://downloads.cursor.com/lab/2026.09.02-c22c1a3/a\nexport DOWNLOAD_URL='https://downloads.cursor.com/lab/2026.09.02-c22c1a3/b'\n"))
		case "/blob":
			_, _ = w.Write([]byte("hello"))
		case "/devin":
			_, _ = w.Write([]byte(`{"version":"3000.6.14"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	r := &Resolver{HTTP: srv.Client(), NPMRegistry: srv.URL + "/npm", ClaudeReleases: srv.URL + "/claude", CursorInstaller: srv.URL + "/cursor"}
	ctx := context.Background()
	for name, want := range map[string]string{"codex": "0.153.2", "claude": "2.1.260", "cursor": "2026.09.02-c22c1a3"} {
		h, _ := Lookup(name)
		got, err := r.Latest(ctx, h)
		if err != nil || got != want {
			t.Errorf("%s latest = %q, %v; want %q", name, got, err, want)
		}
	}
	cursor, _ := Lookup("cursor")
	r.CursorInstaller = srv.URL + "/cursor-lookalike"
	if got, err := r.Latest(ctx, cursor); err == nil {
		t.Errorf("cursor latest from a lookalike host = %q; want an error", got)
	}
	r.CursorInstaller = srv.URL + "/cursor-embedded"
	if got, err := r.Latest(ctx, cursor); err == nil {
		t.Errorf("cursor latest from a URL embedded in another URL = %q; want an error", got)
	}
	for _, path := range []string{"/cursor-mixed-quote", "/cursor-missing", "/cursor-disagree"} {
		r.CursorInstaller = srv.URL + path
		if got, err := r.Latest(ctx, cursor); err == nil {
			t.Errorf("cursor latest from %s = %q; want an error", path, got)
		}
	}
	r.CursorInstaller = srv.URL + "/cursor-agree"
	if got, err := r.Latest(ctx, cursor); err != nil || got != "2026.09.02-c22c1a3" {
		t.Errorf("cursor latest from agreeing assignments = %q, %v", got, err)
	}
	r.DevinManifest = srv.URL + "/devin"
	devin, _ := Lookup("devin")
	if got, err := r.Latest(ctx, devin); err != nil || got != "3000.6.14" {
		t.Errorf("devin latest = %q, %v", got, err)
	}
	digest, err := r.Digest(ctx, srv.URL+"/blob")
	if err != nil || digest != "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824" {
		t.Errorf("digest = %q, %v", digest, err)
	}
	if _, err := r.Digest(ctx, srv.URL+"/missing"); err == nil {
		t.Error("a missing artifact produced a digest")
	}
}

func TestCheckReportsWhatItCannotReadWithoutCallingItOutdated(t *testing.T) {
	root := fixtureRepo(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/npm/@openai/codex/latest" {
			_, _ = w.Write([]byte(`{"version":"0.153.2"}`))
			return
		}
		http.Error(w, "nope", http.StatusForbidden)
	}))
	defer srv.Close()
	r := &Resolver{HTTP: srv.Client(), NPMRegistry: srv.URL + "/npm", ClaudeReleases: srv.URL + "/claude", CursorInstaller: srv.URL + "/cursor"}
	codex, _ := Lookup("codex")
	claude, _ := Lookup("claude")
	kimi, _ := Lookup("kimi")
	statuses, err := Check(context.Background(), root, r, []Harness{codex, claude, kimi})
	if err != nil {
		t.Fatal(err)
	}
	if !statuses[0].Outdated || statuses[0].Latest != "0.153.2" {
		t.Errorf("codex = %+v", statuses[0])
	}
	if statuses[1].Outdated || !strings.Contains(statuses[1].Note, "unreachable") {
		t.Errorf("claude = %+v", statuses[1])
	}
	if statuses[2].Outdated || !strings.Contains(statuses[2].Note, "unreachable") {
		t.Errorf("kimi = %+v", statuses[2])
	}
}

func TestCatalogueMatchesTheDockerfile(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	pins := ReadPins(raw)
	body := string(raw)
	for _, h := range Catalogue {
		if _, ok := pins[h.VersionArg]; !ok {
			t.Errorf("%s: Dockerfile declares no ARG %s", h.Name, h.VersionArg)
		}
		if n := strings.Count(body, "\nARG "+h.VersionArg+"="); n != 1 {
			t.Errorf("%s: Dockerfile declares ARG %s %d times", h.Name, h.VersionArg, n)
		}
		if h.Source == SourceNPM && !strings.Contains(body, `"`+h.Package+`@${`+h.VersionArg+`}"`) {
			t.Errorf("%s: Dockerfile does not install %s@${%s}", h.Name, h.Package, h.VersionArg)
		}
		for _, a := range h.Artifacts {
			if _, ok := pins[a.SHAArg]; !ok {
				t.Errorf("%s: Dockerfile declares no ARG %s", h.Name, a.SHAArg)
			}
			// The catalogue's URL must be the Dockerfile's, modulo the
			// variables the Dockerfile substitutes.
			u := a.URL("V")
			host := u[:strings.Index(u[8:], "/")+8]
			if !strings.Contains(body, host) {
				t.Errorf("%s: Dockerfile does not download from %s", h.Name, host)
			}
		}
		for _, r := range h.Repeats {
			// The private documentation has its own consistency test.
			if r.Path == "docs/provider-contracts/README.md" {
				continue
			}
			raw, err := os.ReadFile(filepath.Join("..", "..", filepath.FromSlash(r.Path)))
			if err != nil {
				t.Errorf("%s: %v", h.Name, err)
				continue
			}
			if !strings.Contains(string(raw), fmt.Sprintf(r.Format, pins[h.VersionArg])) {
				t.Errorf("%s: %s does not spell the shipped version as %q", h.Name, r.Path, fmt.Sprintf(r.Format, pins[h.VersionArg]))
			}
		}
		// A contract is optional: LoadProbe reports no probe for a harness
		// without one and the install layer's checks remain its gate.
		if _, err := LoadProbe(filepath.Join("..", ".."), h.Name); err != nil {
			t.Errorf("%s: %v", h.Name, err)
		}
	}
}

func TestProbeReadsBothContractShapesAndFailsClosed(t *testing.T) {
	root := filepath.Join("..", "..")
	for _, name := range []string{"codex", "opencode", "kimi", "devin"} {
		if _, err := LoadProbe(root, name); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	spec, err := LoadProbe(root, "codex")
	if err != nil || spec == nil || len(spec.commands()) != 2 {
		t.Fatalf("codex probe = %+v, %v", spec, err)
	}
	if spec, _ := LoadProbe(root, "opencode"); spec == nil || len(spec.commands()) != 1 || spec.commands()[0].RequiredFlags[0] != "-s" {
		t.Fatalf("opencode probe = %+v", spec)
	}
	// The older runtime_compatibility block, in each of its shapes, is
	// enforced too: a command and named capability probes.
	enforced := map[string]int{"claude": 1, "command-code": 1, "pi": 1, "qwen": 1, "cursor": 1, "grok": 1, "qoder": 1}
	for name, commands := range enforced {
		spec, err := LoadProbe(root, name)
		if err != nil || spec == nil || len(spec.commands()) != commands || len(spec.commands()[0].RequiredFlags) == 0 {
			t.Errorf("%s: spec = %+v, err %v; want %d enforced command(s)", name, spec, err, commands)
		}
	}
	mimo, err := LoadProbe(root, "mimo")
	if err != nil || mimo == nil || mimo.ExactVersionRequired != "" || len(mimo.commands()) != 1 {
		t.Fatalf("mimo: spec = %+v, err %v; want a syntax check", mimo, err)
	}
	if _, err := Probe(context.Background(), mimo, "0.1.14", func(context.Context, []string, string) (string, error) {
		return "  --trust\n  --yolo\n", nil
	}); err != nil {
		t.Errorf("mimo syntax at a newer version: %v", err)
	}
	if _, err := LoadProbe(root, "kilo"); err != nil {
		t.Errorf("kilo: %v", err)
	}
	for _, h := range Catalogue {
		if _, err := LoadProbe(root, h.Name); err != nil {
			t.Errorf("%s: %v", h.Name, err)
		}
	}

	calls := 0
	good := func(_ context.Context, args []string, stdin string) (string, error) {
		calls++
		if strings.Join(args, " ") == "resume --help" {
			return "Usage: codex resume [OPTIONS] [SESSION_ID]\n  --no-daemon\n  --no-alt-screen\n  --dangerously-bypass-approvals-and-sandbox  yes\n", nil
		}
		return "  -a, --ask-for-approval\n  --no-daemon\n  --no-alt-screen\n  --dangerously-bypass-approvals-and-sandbox\n", nil
	}
	report, err := Probe(context.Background(), spec, "0.162.1", good)
	if err != nil || calls != 2 || len(report) != 7 {
		t.Fatalf("report = %v, err %v, calls %d", report, err, calls)
	}
	prefix := func(out string) Runner {
		return func(context.Context, []string, string) (string, error) { return out, nil }
	}
	if _, err := Probe(context.Background(), spec, "0.153.2", prefix("--dangerously-bypass-approvals-and-sandboxes only\n")); err == nil {
		t.Error("a flag matched as a prefix of another")
	}
	if _, err := Probe(context.Background(), spec, "0.153.2", func(context.Context, []string, string) (string, error) {
		return "", errors.New("exit status 2")
	}); err == nil {
		t.Error("a failing --help passed")
	}
	if !flagPresent("  -s, --auto  run\n", "-s") || flagPresent("  --sandbox\n", "-s") {
		t.Error("flagPresent matches tokens, not substrings")
	}

	// A usage token is found wherever the CLI documents it. These are the two
	// real shapes: codex names [SESSION_ID] on its Usage line and again under
	// Arguments, omp names MESSAGES under an ARGUMENTS header and never on the
	// USAGE line. A withdrawn positional is what the probe exists to catch,
	// and a longer identifier must not stand in for the token.
	for _, tc := range []struct {
		name  string
		out   string
		token string
		want  bool
	}{
		{"codex usage line", "Usage: codex resume [OPTIONS] [SESSION_ID] [PROMPT]\n", "[SESSION_ID]", true},
		{"codex arguments block", "Arguments:\n  [SESSION_ID]\n          Session id (UUID)\n", "[SESSION_ID]", true},
		{"omp arguments block", "ARGUMENTS\n  MESSAGES   Messages to send (prefix files with @)\n", "MESSAGES", true},
		{"withdrawn positional", "FLAGS\n      --model=<value>  Model to use\n", "MESSAGES", false},
		{"longer identifier", "  MESSAGES_FILE  path to a file\n", "MESSAGES", false},
	} {
		if got := usageTokenPresent(tc.out, tc.token); got != tc.want {
			t.Errorf("usageTokenPresent(%s) = %v, want %v", tc.name, got, tc.want)
		}
	}

	exact := &ProbeSpec{ExactVersionRequired: "3000.6.11", Reason: "version-pinned"}
	if _, err := Probe(context.Background(), exact, "3000.7.0", prefix("")); err == nil {
		t.Error("an exact-version contract passed at another version")
	}
	if _, err := Probe(context.Background(), exact, "3000.6.11", prefix("")); err != nil {
		t.Errorf("the measured version failed: %v", err)
	}
	report, err = Probe(context.Background(), nil, "1", prefix(""))
	if err != nil || len(report) != 1 {
		t.Errorf("no contract probe = %v, %v", report, err)
	}
	stdin := &ProbeSpec{Commands: []ProbeCommand{{Args: []string{"doctor"}, AcceptedStdin: "[[hooks]]\n"}}}
	seen := ""
	if _, err := Probe(context.Background(), stdin, "1", func(_ context.Context, _ []string, in string) (string, error) { seen = in; return "", nil }); err != nil || seen != "[[hooks]]\n" {
		t.Errorf("stdin probe: seen %q, err %v", seen, err)
	}
}

func TestLoadProbeFailsClosedOnAMethodItCannotEnforce(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "docs", "provider-contracts")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"novel":   "version_tested: \"1\"\nruntime_compatibility:\n  method: vibes\n",
		"empty":   "runtime_compatibility:\n  method: capability_probes\n  capability_probes: none\n",
		"noexact": "runtime_compatibility:\n  method: exact_tested_version\n",
		"nocmd":   "runtime_compatibility:\n  required_flags: [\"--x\"]\n",
		// The newer block, held to the same rule.
		"emptyprobe": "runtime_compatibility_probe:\n  scope: command-line-syntax-only\n",
		"misspelled": "runtime_compatibility_probe:\n  comands:\n    - args: [\"--help\"]\n      required_flags: [\"-a\"]\n",
		"noargs":     "runtime_compatibility_probe:\n  commands:\n    - required_flags: [\"-a\"]\n",
		"checksnone": "runtime_compatibility_probe:\n  commands:\n    - args: [\"--help\"]\n",
		"nullprobe":  "runtime_compatibility_probe: {}\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name+".yaml"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadProbe(root, name); err == nil {
			t.Errorf("%s loaded as a pass", name)
		}
	}
	both := "version_tested: \"2\"\nruntime_compatibility_probe:\n  commands:\n    - args: [\"--help\"]\n      required_flags: [\"-a\"]\nruntime_compatibility:\n  command: [\"x\", \"--help\"]\n  required_flags: [\"--b\"]\n"
	if err := os.WriteFile(filepath.Join(dir, "both.yaml"), []byte(both), 0o644); err != nil {
		t.Fatal(err)
	}
	spec, err := LoadProbe(root, "both")
	if err != nil || spec == nil || len(spec.commands()) != 2 {
		t.Errorf("both blocks are enforced together: %+v, %v", spec, err)
	}
}

func TestLoadReceiveProbeReadsTheFleetAndFailsClosed(t *testing.T) {
	root := filepath.Join("..", "..")
	receive := map[string]string{
		"claude":   "probe_a2a_claude.py",
		"codex":    "probe_a2a_codex.py",
		"kimi":     "probe_a2a_kimi.py",
		"omp":      "probe_a2a_receive.py --harness omp",
		"pi":       "probe_a2a_receive.py --harness pi",
		"opencode": "probe_a2a_receive.py --harness opencode",
	}
	for name, script := range receive {
		spec, err := LoadReceiveProbe(root, name)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if spec == nil || spec.Script != script {
			t.Errorf("%s: spec = %+v, want script %q", name, spec, script)
		}
	}
	// Only opencode needs outbound network: its bootstrap installs the
	// plugin SDK. Everything else runs beside the syntax probes with none.
	for _, name := range []string{"claude", "codex", "kimi", "omp", "pi"} {
		spec, err := LoadReceiveProbe(root, name)
		if err != nil {
			t.Fatal(err)
		}
		if spec.Network != "none" {
			t.Errorf("%s network = %q, want none", name, spec.Network)
		}
	}
	if spec, _ := LoadReceiveProbe(root, "opencode"); spec.Network != "outbound" {
		t.Errorf("opencode network = %q, want outbound", spec.Network)
	}
	if spec, err := LoadReceiveProbe(root, "devin"); err != nil || spec != nil {
		t.Errorf("a contract without the block loads as none: %+v, %v", spec, err)
	}
	for _, h := range Catalogue {
		if _, err := LoadReceiveProbe(root, h.Name); err != nil {
			t.Errorf("%s: %v", h.Name, err)
		}
	}

	tmp := t.TempDir()
	dir := filepath.Join(tmp, "docs", "provider-contracts")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"noscript":   "native_receive_probe:\n  network: none\n",
		"nullprobe":  "native_receive_probe:\n",
		"nullvalue":  "native_receive_probe: null\n",
		"tilde":      "native_receive_probe: ~\n",
		"emptymap":   "native_receive_probe: {}\n",
		"misspelled": "native_receive_probe:\n  script: probe_a2a_claude.py\n  netwrk: none\n",
		"path":       "native_receive_probe:\n  script: ../../etc/passwd\n",
		"badnet":     "native_receive_probe:\n  script: probe_a2a_claude.py\n  network: host\n",
		"shorttime":  "native_receive_probe:\n  script: probe_a2a_claude.py\n  timeout_s: 5\n",
		"longtime":   "native_receive_probe:\n  script: probe_a2a_claude.py\n  timeout_s: 9999\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name+".yaml"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadReceiveProbe(tmp, name); err == nil {
			t.Errorf("%s loaded as a pass", name)
		}
	}
}

func TestProbeReceiveReportsTheProbeVerdict(t *testing.T) {
	spec := &ReceiveProbeSpec{Script: "probe_a2a_claude.py", TimeoutS: 60}
	report, err := ProbeReceive(context.Background(), spec, func(_ context.Context, args []string, stdin string) (string, error) {
		if strings.Join(args, " ") != "/usr/local/lib/noisedeck/a2a-probe/probe_a2a_claude.py" {
			t.Errorf("probe argv = %v", args)
		}
		if stdin != "" {
			t.Errorf("receive probes take no stdin, got %q", stdin)
		}
		return "PASS [claude]: yes\n", nil
	})
	withArgs := &ReceiveProbeSpec{Script: "probe_a2a_receive.py --harness pi", TimeoutS: 60}
	if _, err := ProbeReceive(context.Background(), withArgs, func(_ context.Context, args []string, _ string) (string, error) {
		if strings.Join(args, " ") != "/usr/local/lib/noisedeck/a2a-probe/probe_a2a_receive.py --harness pi" {
			t.Errorf("probe argv with args = %v", args)
		}
		return "", nil
	}); err != nil {
		t.Fatal(err)
	}
	if err != nil || len(report) != 1 || !strings.HasPrefix(report[0], "ok   native receive: probe_a2a_claude.py") {
		t.Fatalf("report = %v, err = %v", report, err)
	}
	report, err = ProbeReceive(context.Background(), spec, func(_ context.Context, _ []string, _ string) (string, error) {
		return "FAIL [claude]: nope\n", errors.New("exit 1")
	})
	if err == nil || len(report) != 1 || !strings.HasPrefix(report[0], "FAIL native receive: probe_a2a_claude.py") {
		t.Fatalf("failing report = %v, err = %v", report, err)
	}
	if report, err := ProbeReceive(context.Background(), nil, func(_ context.Context, _ []string, _ string) (string, error) {
		t.Fatal("a nil spec runs nothing")
		return "", nil
	}); report != nil || err != nil {
		t.Fatalf("nil spec = %v, %v", report, err)
	}
}

func TestDockerRunnerArgv(t *testing.T) {
	// The runner is docker; this pins the shape the workflow relies on:
	// the image read-only, no network, a tmpfs home.
	src, err := os.ReadFile("probe.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"--network", "none"`, `"--read-only"`, `"--tmpfs", tmpfsHome`, `"HOME=/tmp"`, `"XDG_CONFIG_HOME=/tmp/config"`, `"XDG_DATA_HOME=/tmp/data"`, `"XDG_STATE_HOME=/tmp/state"`, `"XDG_CACHE_HOME=/tmp/cache"`, `"--rm"`} {
		if !regexp.MustCompile(regexp.QuoteMeta(want)).Match(src) {
			t.Errorf("DockerRunner lacks %s", want)
		}
	}
	// Without the explicit exec token moby mounts the probe's /tmp noexec
	// and every hook-based receive probe fails with "Permission denied".
	if !strings.Contains(string(src), `"/tmp:rw,exec,mode=1777"`) {
		t.Error("tmpfsHome lost its exec token; hook-based receive probes cannot run their wrapper")
	}
}

func TestDailyCatalogueProbesAreCredentialFreeSyntaxChecks(t *testing.T) {
	for _, h := range Catalogue {
		t.Run(h.Name, func(t *testing.T) {
			spec, err := LoadProbe(filepath.Join("..", ".."), h.Name)
			if err != nil {
				t.Fatal(err)
			}
			if spec == nil {
				return // The install layer gates harnesses without a contract probe.
			}
			if spec.ExactVersionRequired != "" {
				t.Fatal("daily updates must not require historical behavioral certification")
			}
			for _, c := range spec.commands() {
				// Kimi's offline config parser is the only non-help probe.
				args := strings.Join(c.Args, " ")
				if h.Name == "kimi" && args == "doctor config /dev/stdin" {
					continue
				}
				if len(c.Args) == 0 || c.Args[len(c.Args)-1] != "--help" || c.AcceptedStdin != "" {
					t.Errorf("daily probe must be credential-free help/config syntax: %q", args)
				}
			}
		})
	}
}

func assertContains(t *testing.T, path, want string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), want) {
		t.Errorf("%s lacks %q", filepath.Base(path), want)
	}
}

// A MiMo update must carry both native artifacts through reconstruction;
// moving only the npm version would leave a stale binary in the image.
func TestApplyMimoMovesBothNativeDigests(t *testing.T) {
	root := fixtureRepo(t)
	h, err := Lookup("mimo")
	if err != nil {
		t.Fatal(err)
	}
	if len(h.Artifacts) != 2 {
		t.Fatalf("artifacts = %v", h.Artifacts)
	}
	appendFile := func(rel, text string) {
		t.Helper()
		p := filepath.Join(root, rel)
		old, err := os.ReadFile(p)
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, append(old, []byte(text)...), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	pins := "ARG MIMO_VERSION=0.1.16\nARG MIMO_SHA256_AMD64=" + strings.Repeat("1", 64) + "\nARG MIMO_SHA256_ARM64=" + strings.Repeat("2", 64) + "\n"
	appendFile("Dockerfile", pins)
	appendFile("deploy/mimo_code_cli_package_test.go", pins)
	appendFile("docs/provider-contracts/README.md", "| mimo | 0.1.16 | 0.1.13 | x |\n")
	digests := map[string]string{"MIMO_SHA256_AMD64": strings.Repeat("a", 64), "MIMO_SHA256_ARM64": strings.Repeat("b", 64)}
	for _, a := range h.Artifacts {
		want := "https://github.com/XiaomiMiMo/MiMo-Code/releases/download/v0.1.17/mimocode-linux-arm64.tar.gz"
		if a.Arch == "amd64" {
			want = "https://github.com/XiaomiMiMo/MiMo-Code/releases/download/v0.1.17/mimocode-linux-x64-baseline.tar.gz"
		}
		if got := a.URL("0.1.17"); got != want {
			t.Fatalf("URL = %s, want %s", got, want)
		}
	}
	if _, err := Apply(root, Bump{Harness: h, From: "0.1.16", To: "0.1.17", Digests: digests}); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"Dockerfile", "deploy/mimo_code_cli_package_test.go"} {
		assertContains(t, filepath.Join(root, rel), "ARG MIMO_VERSION=0.1.17")
		for arg, digest := range digests {
			assertContains(t, filepath.Join(root, rel), "ARG "+arg+"="+digest)
		}
	}
}
