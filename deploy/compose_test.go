// Package deploy carries no production code. The tests here pin the Compose
// deployment's public-origin wiring: the browser-facing origin is deployment
// configuration (never derived from forwarded headers), the zero-config
// default is loopback-only, and the Google callback is exactly origin +
// "/auth/callback".
package deploy

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// originInterp is the exact interpolation every public-origin consumer must
// share. Its default matches the loopback-only published port, including an
// explicit host-port override.
const originInterp = "${NOISEDECK_PUBLIC_ORIGIN:-http://127.0.0.1:${NOISEDECK_HOST_PORT:-18082}}"

// localOriginsInterp is the allowlist's default. It differs from
// originInterp on purpose: a callback is ONE origin, but the allowlist must
// name every origin a browser can present for this machine, and a person
// types "localhost" as readily as "127.0.0.1". Setting the public origin
// collapses it to that single value, so no loopback name survives onto a
// public deployment.
const localOriginsInterp = "${NOISEDECK_PUBLIC_ORIGIN:-http://127.0.0.1:${NOISEDECK_HOST_PORT:-18082},http://localhost:${NOISEDECK_HOST_PORT:-18082}}"

type composeFile struct {
	Name     string                 `yaml:"name"`
	Volumes  map[string]interface{} `yaml:"volumes"`
	Services map[string]struct {
		Image       string            `yaml:"image"`
		Hostname    string            `yaml:"hostname"`
		Environment map[string]string `yaml:"environment"`
		Ports       []string          `yaml:"ports"`
		User        string            `yaml:"user"`
		Tmpfs       []string          `yaml:"tmpfs"`
		Volumes     []yaml.Node       `yaml:"volumes"`
		Build       struct {
			Args map[string]string `yaml:"args"`
		} `yaml:"build"`
	} `yaml:"services"`
}

func tmpfsHasOption(mount, want string) bool {
	_, options, ok := strings.Cut(mount, ":")
	if !ok {
		return false
	}
	for _, option := range strings.Split(options, ",") {
		if option == want {
			return true
		}
	}
	return false
}

func tmpfsSizeBytes(mount string) (uint64, bool) {
	_, options, ok := strings.Cut(mount, ":")
	if !ok {
		return 0, false
	}
	for _, option := range strings.Split(options, ",") {
		value, found := strings.CutPrefix(option, "size=")
		if !found {
			continue
		}
		multiplier := uint64(1)
		if value, found = strings.CutSuffix(value, "g"); found {
			multiplier = 1024 * 1024 * 1024
		} else if value, found = strings.CutSuffix(value, "m"); found {
			multiplier = 1024 * 1024
		}
		size, err := strconv.ParseUint(value, 10, 64)
		return size * multiplier, err == nil
	}
	return 0, false
}

func TestTmpfsSizeBytesAcceptsComposeRepresentations(t *testing.T) {
	for _, mount := range []string{
		"/tmp:rw,exec,size=512m",
		"/tmp:rw,exec,size=536870912",
		"/tmp:rw,exec,size=2g",
	} {
		want := uint64(512 * 1024 * 1024)
		if strings.HasSuffix(mount, "2g") {
			want = 2 * 1024 * 1024 * 1024
		}
		if size, ok := tmpfsSizeBytes(mount); !ok || size != want {
			t.Errorf("tmpfsSizeBytes(%q) = %d, %t; want %d, true", mount, size, ok, want)
		}
	}
}

func TestCanonicalDeckResourceNames(t *testing.T) {
	cf := readCompose(t)
	if cf.Name != "deck" {
		t.Fatalf("Compose project name = %q, want deck", cf.Name)
	}
	if got := cf.Services["app"].Image; got != "deck:${NOISEDECK_IMAGE_TAG:-local}" {
		t.Fatalf("app image = %q, want canonical Deck image", got)
	}
	for _, name := range []string{"deck-data", "deck-home"} {
		if _, ok := cf.Volumes[name]; !ok {
			t.Errorf("Compose volume %q is missing", name)
		}
	}
	for _, legacy := range []string{"noisedeck-data", "noisedeck-home"} {
		if _, ok := cf.Volumes[legacy]; ok {
			t.Errorf("legacy Compose volume %q remains declared", legacy)
		}
	}
}

func TestBuildVersionCanRemainStableAcrossImageTags(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "compose.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	want := "VERSION: ${NOISEDECK_BUILD_VERSION:-${NOISEDECK_IMAGE_TAG:-local}}"
	if !strings.Contains(string(raw), want) {
		t.Fatalf("Compose build args do not contain %q", want)
	}
}

func TestCanonicalDeckExecutableKeepsLegacyAdapter(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	dockerfile := string(raw)
	for _, want := range []string{
		"-o /out/noise-deck ./cmd/noisedeck",
		"COPY --from=go-build --chown=root:root /out/noise-deck /usr/local/libexec/noise/noise-deck",
		"ln -s ../libexec/noise/noise-deck /usr/local/bin/noise-deck",
		"ln -s noise-deck /usr/local/bin/noisedeck",
		"noise deck version",
	} {
		if !strings.Contains(dockerfile, want) {
			t.Errorf("Dockerfile does not contain %q", want)
		}
	}
	bootstrap, err := os.ReadFile(filepath.Join("provider-hook", "noisedeck-bootstrap"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(bootstrap), `exec /usr/local/bin/noise deck serve "$@"`) {
		t.Error("bootstrap does not exec the console through the noise plugin host")
	}
}

func TestBootstrapFinalizesProviderTrustAfterOptionalHookComposition(t *testing.T) {
	for _, tc := range []struct {
		path        string
		composition string
	}{
		{filepath.Join("provider-hook", "noisedeck-bootstrap"), "configure-mempalace.py --remove"},
		{filepath.Join("mempalace", "noisedeck-bootstrap"), "configure-mempalace.py"},
	} {
		raw, err := os.ReadFile(tc.path)
		if err != nil {
			t.Fatal(err)
		}
		body := string(raw)
		const provision = "/usr/local/bin/noisedeck-provision"
		if got := strings.Count(body, provision); got != 2 {
			t.Errorf("%s runs provider provisioning %d times, want before and after composition", tc.path, got)
			continue
		}
		first := strings.Index(body, provision)
		compose := strings.Index(body, tc.composition)
		final := strings.LastIndex(body, provision)
		serve := strings.Index(body, `exec /usr/local/bin/noise deck serve`)
		if first >= compose || compose >= final || final >= serve {
			t.Errorf("%s order = first provision %d, composition %d, final provision %d, serve %d",
				tc.path, first, compose, final, serve)
		}
	}
}

func readCompose(t *testing.T) composeFile {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "compose.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var cf composeFile
	if err := yaml.Unmarshal(raw, &cf); err != nil {
		t.Fatalf("compose.yaml does not parse: %v", err)
	}
	if _, ok := cf.Services["app"]; !ok {
		t.Fatal("compose.yaml carries no app service")
	}
	return cf
}

// The static wiring: allowed origins and the Google callback interpolate the
// same required public origin, the allowlist alone adds the second loopback
// name for a zero-config launch, and the callback appends exactly
// "/auth/callback". Cookie security is derived by the application.
func TestComposePublicOriginWiring(t *testing.T) {
	env := readCompose(t).Services["app"].Environment

	if got := env["NOISEDECK_SERVER_ALLOWED_ORIGINS"]; got != localOriginsInterp {
		t.Errorf("allowed origins = %q, want exactly %q", got, localOriginsInterp)
	}
	if got, want := env["NOISEDECK_AUTH_GOOGLE_REDIRECT_URL"], originInterp+"/auth/callback"; got != want {
		t.Errorf("redirect url = %q, want exactly %q", got, want)
	}
	if _, exists := env["NOISEDECK_AUTH_SECURE_COOKIES"]; exists {
		t.Error("compose must not expose an independent secure-cookie setting")
	}
	if got := env["NOISEDECK_AUTH_MODE"]; got != "${NOISEDECK_AUTH_MODE:-password}" {
		t.Errorf("auth mode = %q, want password default with an explicit override", got)
	}
	for _, key := range []string{
		"NOISEDECK_AUTH_GOOGLE_CLIENT_ID",
		"NOISEDECK_AUTH_GOOGLE_CLIENT_SECRET",
		"NOISEDECK_AUTH_GOOGLE_ALLOWED_EMAIL",
		"NOISEDECK_AUTH_SESSION_SECRET",
	} {
		if got := env[key]; !strings.HasSuffix(got, ":-}") {
			t.Errorf("%s = %q, want optional interpolation", key, got)
		}
	}
}

func TestComposeUsesCanonicalTaskHost(t *testing.T) {
	env := readCompose(t).Services["app"].Environment
	if got := env["NOISEDECK_TASKMASTER_EXECUTABLE"]; got != "/usr/local/bin/noise" {
		t.Fatalf("task executable = %q, want /usr/local/bin/noise", got)
	}
}

func TestComposeProvidesZeroConfigHostAuthority(t *testing.T) {
	app := readCompose(t).Services["app"]
	if got := app.Environment["NOISEDECK_DEPLOYMENT_HOST_SHA256"]; got != "${NOISEDECK_DEPLOYMENT_HOST_SHA256:-}" {
		t.Errorf("deployment host fingerprint = %q, want optional explicit pin", got)
	}
	var projectsSource string
	hasHostIdentity := false
	for _, node := range app.Volumes {
		if node.Kind != yaml.MappingNode {
			continue
		}
		var mount struct {
			Type     string `yaml:"type"`
			Source   string `yaml:"source"`
			Target   string `yaml:"target"`
			ReadOnly bool   `yaml:"read_only"`
		}
		if err := node.Decode(&mount); err != nil {
			t.Fatalf("decode deployment host identity mount: %v", err)
		}
		if mount.Target == "/workspace" {
			projectsSource = mount.Source
			continue
		}
		if mount.Target != "/run/noisedeck-host-machine-id" {
			continue
		}
		if mount.Type != "bind" || mount.Source != "${NOISEDECK_HOST_ID_PATH:-/etc/machine-id}" || !mount.ReadOnly {
			t.Fatalf("deployment host identity mount = %+v, want read-only host machine-id bind", mount)
		}
		hasHostIdentity = true
	}
	if !hasHostIdentity {
		t.Fatal("deployment host identity mount is missing")
	}
	if projectsSource != "${NOISEDECK_PROJECTS_HOST_PATH:-.}" {
		t.Fatalf("zero-config projects source = %q, want ${NOISEDECK_PROJECTS_HOST_PATH:-.}", projectsSource)
	}
}

func TestRuntimeIdentityIsSharedByImageAndCompose(t *testing.T) {
	const (
		uid = "${NOISEDECK_RUNTIME_UID:-1026}"
		gid = "${NOISEDECK_RUNTIME_GID:-100}"
	)
	app := readCompose(t).Services["app"]
	if got := app.Build.Args["NOISEDECK_RUNTIME_UID"]; got != uid {
		t.Errorf("runtime UID build arg = %q, want %q", got, uid)
	}
	if got := app.Build.Args["NOISEDECK_RUNTIME_GID"]; got != gid {
		t.Errorf("runtime GID build arg = %q, want %q", got, gid)
	}
	if got, want := app.User, uid+":"+gid; got != want {
		t.Errorf("Compose user = %q, want %q", got, want)
	}
	if len(app.Tmpfs) != 2 {
		t.Fatalf("Compose tmpfs count = %d, want 2", len(app.Tmpfs))
	}
	foundAppTmp := false
	for _, mount := range app.Tmpfs {
		if !strings.Contains(mount, "uid="+uid) || !strings.Contains(mount, "gid="+gid) {
			t.Errorf("tmpfs %q does not use the configured runtime identity", mount)
		}
		if strings.HasPrefix(mount, "/tmp:") {
			foundAppTmp = true
			if !tmpfsHasOption(mount, "size=${NOISEDECK_TMPFS_SIZE:-2g}") {
				t.Errorf("app /tmp mount = %q, want configurable 2 GiB default capacity", mount)
			}
			for _, option := range []string{"rw", "exec", "nosuid", "nodev"} {
				if !tmpfsHasOption(mount, option) {
					t.Errorf("app /tmp mount = %q, want %s option", mount, option)
				}
			}
		}
	}
	if !foundAppTmp {
		t.Fatal("app /tmp tmpfs is missing")
	}
}

// Font selection reaches the frontend build, with auto as the installation default.
func TestImageBuildsTheTypeSetItIsAsked(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	dockerfile := string(raw)
	argAt := strings.Index(dockerfile, "ARG NOISEDECK_FONTS=auto\n")
	buildAt := strings.Index(dockerfile, `NOISEDECK_EMBED_BUILD=1 NOISEDECK_FONTS="${NOISEDECK_FONTS}" pnpm build`)
	webBuildAt := strings.Index(dockerfile, "AS web-build")
	goBuildAt := strings.Index(dockerfile, "AS go-build")
	if argAt < 0 || buildAt < 0 {
		t.Fatalf("web-build does not declare NOISEDECK_FONTS and pass it to pnpm build (arg %d, build %d)", argAt, buildAt)
	}
	if webBuildAt >= argAt || argAt >= buildAt || buildAt >= goBuildAt {
		t.Errorf("NOISEDECK_FONTS must be declared in web-build before the frontend build (web-build %d, arg %d, build %d, go-build %d)", webBuildAt, argAt, buildAt, goBuildAt)
	}
	for _, file := range []string{"compose.yaml", "compose.go-mempalace.yaml"} {
		compose, readErr := os.ReadFile(filepath.Join("..", file))
		if readErr != nil {
			t.Fatal(readErr)
		}
		if !strings.Contains(string(compose), "NOISEDECK_FONTS: ${NOISEDECK_FONTS:-auto}") {
			t.Errorf("%s does not pass NOISEDECK_FONTS through to the image build", file)
		}
	}
}

// Docker initializes an empty named volume from the image's mountpoint. The
// image must therefore create the provider-config parent under the same
// configurable identity Compose uses, before dropping root. An existing
// Debian group (including the default GID 100 `users` group) is reused.
func TestRuntimeImageSupportsConfigurableIdentityAndExistingGroups(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	dockerfile := string(raw)
	for _, required := range []string{
		"ARG NOISEDECK_RUNTIME_UID=1026",
		"ARG NOISEDECK_RUNTIME_GID=100",
		`getent group "${NOISEDECK_RUNTIME_GID}"`,
		`groupadd --gid "${NOISEDECK_RUNTIME_GID}" noisedeck-agent`,
		`useradd --uid "${NOISEDECK_RUNTIME_UID}" --gid "${NOISEDECK_RUNTIME_GID}"`,
		`[ "${existing_user}" = node ]`,
		`usermod --login agent --home /home/agent --gid "${NOISEDECK_RUNTIME_GID}"`,
		`Deck refuses a root runtime UID or GID`,
		`is already owned by ${existing_user}`,
		`USER ${NOISEDECK_RUNTIME_UID}:${NOISEDECK_RUNTIME_GID}`,
		"FROM core AS slim\n\nARG TARGETARCH\nARG NOISEDECK_RUNTIME_UID=1026\nARG NOISEDECK_RUNTIME_GID=100",
	} {
		if !strings.Contains(dockerfile, required) {
			t.Errorf("runtime image is missing configurable-identity contract %q", required)
		}
	}
	seed := `install -d -m 0700 -o "${NOISEDECK_RUNTIME_UID}" -g "${NOISEDECK_RUNTIME_GID}" /home/agent/.config`
	seedAt := strings.Index(dockerfile, seed)
	userAt := strings.Index(dockerfile, "USER ${NOISEDECK_RUNTIME_UID}:${NOISEDECK_RUNTIME_GID}")
	if seedAt < 0 {
		t.Fatalf("runtime image does not seed %q", seed)
	}
	if userAt < 0 || seedAt > userAt {
		t.Fatal("provider config directory must be seeded before the runtime drops root")
	}
	cacheSeed := `install -d -m 0700 -o "${NOISEDECK_RUNTIME_UID}" -g "${NOISEDECK_RUNTIME_GID}" /home/agent/.cache`
	runtimeEnd := strings.Index(dockerfile, "FROM core AS slim")
	if runtimeEnd < 0 {
		t.Fatal("Dockerfile has no core stage boundary")
	}
	cacheAt := strings.LastIndex(dockerfile[:runtimeEnd], cacheSeed)
	finalRootCommand := strings.Index(dockerfile, "&& noise deck version")
	if cacheAt < 0 || finalRootCommand < 0 || cacheAt < finalRootCommand || cacheAt > userAt {
		t.Fatal("runtime cache directory must be reconciled after the final root command and before the runtime drops root")
	}
	if strings.Contains(dockerfile, "USER 1026:100") {
		t.Fatal("runtime image still has a fixed process identity")
	}
}

// The published port stays loopback-only regardless of the public origin:
// the reverse proxy reaches the service on 127.0.0.1, the browser never
// does.
func TestComposePortStaysLoopbackOnly(t *testing.T) {
	ports := readCompose(t).Services["app"].Ports
	if len(ports) != 1 {
		t.Fatalf("app publishes %d ports, want exactly one: %v", len(ports), ports)
	}
	p := ports[0]
	if !strings.HasPrefix(p, "127.0.0.1:") || !strings.HasSuffix(p, ":8837") {
		t.Errorf("published port %q: must bind 127.0.0.1 on the host and target container port 8837", p)
	}
}

// renderEnv is the explicit production-style override set. The empty map is
// the Tier-0 proof; this fixture verifies that every default remains
// independently overrideable.
func renderEnv(t *testing.T) map[string]string {
	t.Helper()
	return map[string]string{
		"NOISEDECK_PUBLIC_ORIGIN":          "https://deck.example.com",
		"NOISEDECK_DEPLOYMENT_HOST_SHA256": strings.Repeat("a", 64),
		"NOISEDECK_PROJECTS_HOST_PATH":     t.TempDir(),
	}
}

func TestMemPalaceIsAnOptInOverlay(t *testing.T) {
	base, err := os.ReadFile(filepath.Join("..", "compose.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	overlay, err := os.ReadFile(filepath.Join("..", "compose.mempalace.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.ToLower(string(base)), "mempalace") {
		t.Fatal("base Compose file contains MemPalace configuration")
	}
	for _, want := range []string{"target: mempalace", "NOISEDECK_MEMPALACE_HOST_PATH", "/home/agent/.mempalace"} {
		if !strings.Contains(string(overlay), want) {
			t.Errorf("MemPalace overlay does not contain %q", want)
		}
	}

	dockerfile, err := os.ReadFile(filepath.Join("..", "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.SplitN(string(dockerfile), "FROM runtime AS mempalace", 2)
	if len(parts) != 2 {
		t.Fatal("Dockerfile has no MemPalace overlay target")
	}
	for _, forbidden := range []string{"python3-venv", "/opt/mempalace", "MEMPALACE_VERSION"} {
		if strings.Contains(parts[0], forbidden) {
			t.Errorf("base runtime contains opt-in dependency %q", forbidden)
		}
	}
}

func renderCompose(t *testing.T, env map[string]string, overlays ...string) (string, error) {
	t.Helper()
	docker, err := exec.LookPath("docker")
	args := []string{"compose"}
	if err != nil {
		docker, err = exec.LookPath("docker-compose")
		if err != nil {
			t.Skip("Docker Compose is not available; the rendered-Compose proof cannot run here")
		}
		args = nil
	}
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for k, v := range env {
		lines = append(lines, k+"="+v)
	}
	envFile := filepath.Join(t.TempDir(), "compose.env")
	if err := os.WriteFile(envFile, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	args = append(args, "-f", "compose.yaml")
	for _, overlay := range overlays {
		args = append(args, "-f", overlay)
	}
	args = append(args, "--env-file", envFile, "config")
	cmd := exec.Command(docker, args...)
	cmd.Dir = root
	// A process environment variable beats --env-file in Compose
	// interpolation, so an inherited NOISEDECK_* would silently contaminate
	// the fail-closed cases. Restrict the child to what docker itself needs.
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME")}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func TestRenderedComposeBuildsNeedNoGitHubSecret(t *testing.T) {
	for _, tc := range []struct {
		name     string
		overlays []string
	}{
		{"full", nil},
		{"slim", []string{"compose.slim.yaml"}},
		{"mempalace", []string{"compose.mempalace.yaml"}},
		{"go", []string{"compose.go.yaml"}},
		{"go-mempalace", []string{"compose.mempalace.yaml", "compose.go.yaml", "compose.go-mempalace.yaml"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := renderCompose(t, map[string]string{
				"NOISEDECK_MEMPALACE_HOST_PATH": t.TempDir(),
			}, tc.overlays...)
			if err != nil {
				t.Fatalf("token-free Compose config failed: %v\n%s", err, out)
			}
			var rendered struct {
				Secrets  map[string]any `yaml:"secrets"`
				Services map[string]struct {
					Build struct {
						Secrets []any `yaml:"secrets"`
					} `yaml:"build"`
				} `yaml:"services"`
			}
			if err := yaml.Unmarshal([]byte(out), &rendered); err != nil {
				t.Fatalf("rendered Compose does not parse: %v", err)
			}
			if len(rendered.Secrets) != 0 {
				t.Errorf("public Compose declares secrets: %v", rendered.Secrets)
			}
			for name, service := range rendered.Services {
				if len(service.Build.Secrets) != 0 {
					t.Errorf("service %s requires build secrets: %v", name, service.Build.Secrets)
				}
			}
		})
	}
}

// The rendered deployment: the public origin passes through exactly, the
// callback is exactly origin + "/auth/callback", secure cookies can be
// enabled, and the published port remains loopback-only.
func TestRenderedComposePassesPublicOriginExactly(t *testing.T) {
	out, err := renderCompose(t, renderEnv(t))
	if err != nil {
		t.Fatalf("docker compose config failed: %v\n%s", err, out)
	}
	var rendered struct {
		Services map[string]struct {
			Environment map[string]any `yaml:"environment"`
			Ports       []struct {
				HostIP    string `yaml:"host_ip"`
				Target    int    `yaml:"target"`
				Published any    `yaml:"published"`
			} `yaml:"ports"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal([]byte(out), &rendered); err != nil {
		t.Fatalf("rendered Compose does not parse: %v\n%s", err, out)
	}
	app := rendered.Services["app"]

	if got := fmt.Sprint(app.Environment["NOISEDECK_SERVER_ALLOWED_ORIGINS"]); got != "https://deck.example.com" {
		t.Errorf("rendered allowed origins = %q, want exactly https://deck.example.com", got)
	}
	if got := fmt.Sprint(app.Environment["NOISEDECK_AUTH_GOOGLE_REDIRECT_URL"]); got != "https://deck.example.com/auth/callback" {
		t.Errorf("rendered redirect url = %q, want exactly https://deck.example.com/auth/callback", got)
	}
	if got := fmt.Sprint(app.Environment["NOISEDECK_AUTH_MODE"]); got != "password" {
		t.Errorf("rendered auth mode = %q, want password", got)
	}
	for _, key := range []string{
		"NOISEDECK_AUTH_GOOGLE_CLIENT_ID",
		"NOISEDECK_AUTH_GOOGLE_CLIENT_SECRET",
		"NOISEDECK_AUTH_GOOGLE_ALLOWED_EMAIL",
		"NOISEDECK_AUTH_SESSION_SECRET",
	} {
		if got := fmt.Sprint(app.Environment[key]); got != "" {
			t.Errorf("rendered %s = %q, want empty in default password mode", key, got)
		}
	}
	if _, exists := app.Environment["NOISEDECK_AUTH_SECURE_COOKIES"]; exists {
		t.Error("rendered Compose exposes an independent secure-cookie setting")
	}
	if len(app.Ports) != 1 || app.Ports[0].HostIP != "127.0.0.1" ||
		app.Ports[0].Target != 8837 || fmt.Sprint(app.Ports[0].Published) != "18082" {
		t.Errorf("rendered ports = %+v, want one loopback-only 127.0.0.1:18082->8837", app.Ports)
	}
}

func TestRenderedComposePreservesExplicitOIDCMode(t *testing.T) {
	env := renderEnv(t)
	env["NOISEDECK_AUTH_MODE"] = "oidc"
	env["GOOGLE_OIDC_CLIENT_ID"] = "test-client-id.apps.googleusercontent.com"
	env["GOOGLE_OIDC_CLIENT_SECRET"] = "test-google-secret"
	env["GOOGLE_OIDC_ALLOWED_EMAIL"] = "operator@example.com"
	env["NOISEDECK_AUTH_SESSION_SECRET"] = "test-session-secret"

	out, err := renderCompose(t, env)
	if err != nil {
		t.Fatalf("docker compose config failed: %v\n%s", err, out)
	}
	var rendered struct {
		Services map[string]struct {
			Environment map[string]any `yaml:"environment"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal([]byte(out), &rendered); err != nil {
		t.Fatalf("rendered Compose does not parse: %v\n%s", err, out)
	}
	got := rendered.Services["app"].Environment
	for key, want := range map[string]string{
		"NOISEDECK_AUTH_MODE":                 "oidc",
		"NOISEDECK_AUTH_GOOGLE_CLIENT_ID":     "test-client-id.apps.googleusercontent.com",
		"NOISEDECK_AUTH_GOOGLE_CLIENT_SECRET": "test-google-secret",
		"NOISEDECK_AUTH_GOOGLE_ALLOWED_EMAIL": "operator@example.com",
		"NOISEDECK_AUTH_SESSION_SECRET":       "test-session-secret",
	} {
		if value := fmt.Sprint(got[key]); value != want {
			t.Errorf("rendered %s = %q, want %q", key, value, want)
		}
	}
}

func TestDeploymentPreflightAndHealthcheckAssertRenderedAuthMode(t *testing.T) {
	bootstrap, err := os.ReadFile(filepath.Join("provider-hook", "noisedeck-bootstrap"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(bootstrap), "NOISEDECK_AUTH_MODE must be rendered explicitly") {
		t.Fatal("deployment bootstrap does not refuse an absent rendered auth mode")
	}
	healthcheck, err := os.ReadFile(filepath.Join("provider-hook", "noisedeck-auth-mode-healthcheck"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Xx]-[Nn]oisedeck-[Aa]uth-[Mm]ode", `"$actual" != "$expected"`, "/readyz"} {
		if !strings.Contains(string(healthcheck), want) {
			t.Errorf("auth-mode healthcheck does not contain %q", want)
		}
	}
	raw, err := os.ReadFile(filepath.Join("..", "compose.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `["CMD", "/usr/local/bin/noisedeck-auth-mode-healthcheck"]`) {
		t.Fatal("Compose does not run the post-start auth-mode assertion")
	}
}

func TestRenderedComposeUsesNonDefaultRuntimeIdentityEverywhere(t *testing.T) {
	env := renderEnv(t)
	env["NOISEDECK_RUNTIME_UID"] = "1000"
	env["NOISEDECK_RUNTIME_GID"] = "1000"
	env["NOISEDECK_TMPFS_SIZE"] = "4g"
	out, err := renderCompose(t, env)
	if err != nil {
		t.Fatalf("docker compose config failed: %v\n%s", err, out)
	}
	var rendered struct {
		Services map[string]struct {
			User  string   `yaml:"user"`
			Tmpfs []string `yaml:"tmpfs"`
			Build struct {
				Args map[string]any `yaml:"args"`
			} `yaml:"build"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal([]byte(out), &rendered); err != nil {
		t.Fatalf("rendered Compose does not parse: %v\n%s", err, out)
	}
	app := rendered.Services["app"]
	if app.User != "1000:1000" {
		t.Errorf("rendered user = %q, want 1000:1000", app.User)
	}
	if got := fmt.Sprint(app.Build.Args["NOISEDECK_RUNTIME_UID"]); got != "1000" {
		t.Errorf("rendered UID build arg = %q, want 1000", got)
	}
	if got := fmt.Sprint(app.Build.Args["NOISEDECK_RUNTIME_GID"]); got != "1000" {
		t.Errorf("rendered GID build arg = %q, want 1000", got)
	}
	if len(app.Tmpfs) != 2 {
		t.Fatalf("rendered tmpfs count = %d, want 2", len(app.Tmpfs))
	}
	foundAppTmp := false
	for _, mount := range app.Tmpfs {
		if !strings.Contains(mount, "uid=1000") || !strings.Contains(mount, "gid=1000") {
			t.Errorf("rendered tmpfs %q does not use 1000:1000", mount)
		}
		if strings.HasPrefix(mount, "/tmp:") {
			foundAppTmp = true
			if size, ok := tmpfsSizeBytes(mount); !ok || size != 4*1024*1024*1024 {
				t.Errorf("rendered app /tmp mount = %q, want 4 GiB override", mount)
			}
			for _, option := range []string{"rw", "exec", "nosuid", "nodev"} {
				if !tmpfsHasOption(mount, option) {
					t.Errorf("rendered app /tmp mount = %q, want %s option", mount, option)
				}
			}
		}
	}
	if !foundAppTmp {
		t.Fatal("rendered app /tmp tmpfs is missing")
	}
}

func TestRenderedComposeNeedsNoConfiguration(t *testing.T) {
	out, err := renderCompose(t, nil)
	if err != nil {
		t.Fatalf("zero-config docker compose config failed: %v\n%s", err, out)
	}
	var rendered struct {
		Services map[string]struct {
			Environment map[string]any `yaml:"environment"`
			Volumes     []struct {
				Source string `yaml:"source"`
				Target string `yaml:"target"`
			} `yaml:"volumes"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal([]byte(out), &rendered); err != nil {
		t.Fatalf("zero-config rendered Compose does not parse: %v\n%s", err, out)
	}
	app := rendered.Services["app"]
	for key, want := range map[string]string{
		"NOISEDECK_SERVER_ALLOWED_ORIGINS":                  "http://127.0.0.1:18082,http://localhost:18082",
		"NOISEDECK_AUTH_GOOGLE_REDIRECT_URL":                "http://127.0.0.1:18082/auth/callback",
		"NOISEDECK_DEPLOYMENT_HOST_SHA256":                  "",
		"NOISEDECK_EXPERIMENTS_UNCONFINED_CAPACITY_ROUTING": "false",
	} {
		if got := fmt.Sprint(app.Environment[key]); got != want {
			t.Errorf("zero-config %s = %q, want %q", key, got, want)
		}
	}
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	for _, mount := range app.Volumes {
		if mount.Target == "/workspace" {
			if mount.Source != root {
				t.Errorf("zero-config projects source = %q, want checkout %q", mount.Source, root)
			}
			return
		}
	}
	t.Fatal("zero-config rendered Compose has no projects mount")
}

// The capacity experiment switch renders as the exact
// string you set, never Compose's boolean coercion, and the default is false.
func TestRenderedComposePassesCapacityExperimentSwitchExactly(t *testing.T) {
	out, err := renderCompose(t, map[string]string{
		"NOISEDECK_EXPERIMENTS_UNCONFINED_CAPACITY_ROUTING": "true",
	})
	if err != nil {
		t.Fatalf("docker compose config with experiment switch failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, `NOISEDECK_EXPERIMENTS_UNCONFINED_CAPACITY_ROUTING: "true"`) {
		t.Errorf("rendered Compose does not carry the exact experiment switch:\n%s", out)
	}
}

func TestRenderedComposeLoopbackOriginTracksHostPort(t *testing.T) {
	out, err := renderCompose(t, map[string]string{"NOISEDECK_HOST_PORT": "19090"})
	if err != nil {
		t.Fatalf("docker compose config with host-port override failed: %v\n%s", err, out)
	}
	for _, want := range []string{
		// The whole value: both loopback names follow the port, and nothing
		// else is admitted.
		"NOISEDECK_SERVER_ALLOWED_ORIGINS: http://127.0.0.1:19090,http://localhost:19090",
		"NOISEDECK_AUTH_GOOGLE_REDIRECT_URL: http://127.0.0.1:19090/auth/callback",
		"published: \"19090\"",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("host-port render does not contain %q:\n%s", want, out)
		}
	}
}

// The agent-browser overlay builds a Playwright server image. Playwright
// refuses to connect across a client/server version mismatch, so the image
// tag and the client the harnesses import have to agree exactly. The
// Chromium security patch is pinned separately from the protocol client.
func TestBrowserOverlayTagMatchesPlaywrightClient(t *testing.T) {
	overlay, err := os.ReadFile(filepath.Join("..", "compose.browser.yaml"))
	if err != nil {
		t.Fatalf("read compose.browser.yaml: %v", err)
	}
	var doc struct {
		Services struct {
			Chromium struct {
				Image   string   `yaml:"image"`
				Command []string `yaml:"command"`
				Build   struct {
					Target string `yaml:"target"`
				} `yaml:"build"`
			} `yaml:"chromium"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal(overlay, &doc); err != nil {
		t.Fatalf("parse compose.browser.yaml: %v", err)
	}

	lock, err := os.ReadFile(filepath.Join("..", "web", "pnpm-lock.yaml"))
	if err != nil {
		t.Fatalf("read pnpm-lock.yaml: %v", err)
	}
	// The lockfile is the resolved truth; package.json carries a caret range
	// that does not say which version is actually installed.
	m := regexp.MustCompile(`(?m)^  playwright@([0-9]+\.[0-9]+\.[0-9]+):`).FindSubmatch(lock)
	if m == nil {
		t.Fatal("no resolved playwright version in web/pnpm-lock.yaml")
	}
	want := string(m[1])
	versionsRaw, err := os.ReadFile("browser-versions.json")
	if err != nil {
		t.Fatal(err)
	}
	var versions struct {
		Playwright string `json:"playwright"`
		Chromium   string `json:"chromium"`
	}
	if err := json.Unmarshal(versionsRaw, &versions); err != nil {
		t.Fatal(err)
	}
	if versions.Playwright != want || versions.Chromium != "156.0.8078.47" {
		t.Fatalf("browser qualification pin disagrees with lockfile or security baseline: %+v", versions)
	}

	if got := doc.Services.Chromium.Image; got != "noise-browser:"+want+"-chromium-"+versions.Chromium {
		t.Errorf("chromium image %q does not identify the resolved playwright %s build", got, want)
	}
	if got := doc.Services.Chromium.Build.Target; got != "browser" {
		t.Errorf("chromium build target = %q, want browser", got)
	}

	dockerfile, err := os.ReadFile(filepath.Join("..", "Dockerfile"))
	if err != nil {
		t.Fatalf("read Dockerfile: %v", err)
	}
	base := "FROM mcr.microsoft.com/playwright:v" + want + "-noble@sha256:"
	if !strings.Contains(string(dockerfile), base) {
		t.Errorf("browser target does not pin the upstream playwright %s image by digest", want)
	}
	for _, archive := range []struct{ arch, platform, digest string }{
		{"amd64", "linux64", "e6c94490f0b6b337225aeb2d2a5034858516b7017356980ba94f5176d8774a50"},
		{"arm64", "linux-arm64", "9e37000e3ee2731e45e475a6a2eac587d3c0ee5f8aef7cab5211ecd52cf3fb7b"},
	} {
		pin := "ADD --checksum=sha256:" + archive.digest + " https://storage.googleapis.com/chrome-for-testing-public/" + versions.Chromium + "/" + archive.platform + "/chrome-" + archive.platform + ".zip /chrome.zip"
		if !strings.Contains(string(dockerfile), "FROM scratch AS chrome-"+archive.arch) || !strings.Contains(string(dockerfile), pin) {
			t.Errorf("missing checksum-pinned fixed Chromium for %s", archive.arch)
		}
	}
	for _, contract := range []string{"FROM chrome-${TARGETARCH} AS chrome-archive", "/ms-playwright", "apt-get install -y --no-install-recommends unzip", "COPY deploy/browser-versions.json /opt/noise-browser/browser-versions.json", "apt-get purge -y gstreamer1.0-plugins-bad libgstreamer-plugins-bad1.0-0", "/usr/lib/node_modules/npm /usr/bin/npm /usr/bin/npx", "ldd /opt/noise-browser/chrome/chrome"} {
		if !strings.Contains(string(dockerfile), contract) {
			t.Errorf("browser security build is missing %q", contract)
		}
	}
	_, browserStage, found := strings.Cut(string(dockerfile), " AS browser\n")
	if !found {
		t.Fatal("Dockerfile has no browser stage")
	}
	browserStage, _, _ = strings.Cut(browserStage, "FROM golang:")
	if strings.Contains(browserStage, "COPY --from=web-build /src/web/node_modules") || !strings.Contains(browserStage, `cp -RL "${client}" "${client}/../playwright-core"`) {
		t.Error("browser must copy only the frozen-lockfile Playwright runtime closure")
	}
	// The command runs the repo-owned public-API server copied into the image. A
	// command that fetched one instead — npx, npm exec, a bare `playwright`
	// off PATH — would
	// silently decide the version at container start, where the tag can no
	// longer be trusted to describe what is running.
	cmd := strings.Join(doc.Services.Chromium.Command, " ")
	const serverEntrypoint = "/opt/noise-browser/server.mjs"
	if cmd != "node "+serverEntrypoint {
		t.Errorf("chromium command = %q, want repo-owned server %q", cmd, serverEntrypoint)
	}
	for _, fetcher := range []string{"npx", "npm ", "yarn", "pnpm"} {
		if strings.Contains(cmd, fetcher) {
			t.Errorf("chromium command %q reaches for %q; the image tag must be the only version pin",
				cmd, strings.TrimSpace(fetcher))
		}
	}
	entrypoint, err := os.ReadFile(filepath.Join("..", "deploy", "browser-server.mjs"))
	if err != nil {
		t.Fatalf("read browser server entrypoint: %v", err)
	}
	server := string(entrypoint)
	for _, contract := range []string{
		`chromium.launchServer({`,
		`executablePath: "/opt/noise-browser/chrome/chrome"`,
		`host: "127.0.0.1"`,
		`port: 3000`,
		`wsPath: "/"`,
		`ws://127.0.0.1:3000/`,
		`["SIGINT", "SIGTERM"]`,
		`process.once(signal`,
		`await server.close()`,
	} {
		if !strings.Contains(server, contract) {
			t.Errorf("browser server is missing %q", contract)
		}
	}
	if !strings.Contains(string(dockerfile), "COPY deploy/browser-server.mjs "+serverEntrypoint) {
		t.Error("browser image does not package the repo-owned server entrypoint")
	}
	acceptance, err := os.ReadFile(filepath.Join("..", "deploy", "browser_sidecar_acceptance.sh"))
	if err != nil {
		t.Fatalf("read browser acceptance: %v", err)
	}
	if !strings.Contains(string(acceptance), "node "+serverEntrypoint) {
		t.Error("browser acceptance does not launch the packaged server entrypoint")
	}
	for _, contract := range []string{"NOISE_BROWSER_SKIP_BUILD", "docker image inspect", "linux/amd64", "https.createServer", "certutil -A", "subjectAltName=DNS:localhost,IP:127.0.0.1", "https://localhost:8443", "response?.status()"} {
		if !strings.Contains(string(acceptance), contract) {
			t.Errorf("browser acceptance is missing prebuilt-image contract %q", contract)
		}
	}
	if !strings.Contains(string(acceptance), "browser.version() !== versions.chromium") {
		t.Error("browser acceptance must verify the running Chromium version")
	}
	if strings.Contains(string(acceptance), "https://example.com") {
		t.Error("browser acceptance must not depend on external content for its positive TLS control")
	}
}

// The chat identity is the container's host name: a2a announces its key
// under it and peers pin that name. Production fixes it, so a redeploy,
// which gives a container a new id, does not lapse every pin of this host.
func TestProductionFixesTheHostName(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "compose.production.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var file composeFile
	if err := yaml.Unmarshal(raw, &file); err != nil {
		t.Fatal(err)
	}
	if hostname := file.Services["app"].Hostname; hostname != "${NOISEDECK_HOSTNAME:-noisefloor-deck}" {
		t.Errorf("production hostname = %q, want ${NOISEDECK_HOSTNAME:-noisefloor-deck}: a fixed default and an override", hostname)
	}
}
