package deploy

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// sealedOverlay is compose.sealed.yaml reduced to the fields the sealed
// storage rules pin. The !override and !reset tags parse as plain nodes.
type sealedOverlay struct {
	Volumes  yaml.Node `yaml:"volumes"`
	Services map[string]struct {
		UsernsMode  string            `yaml:"userns_mode"`
		Hostname    string            `yaml:"hostname"`
		Environment map[string]string `yaml:"environment"`
		Tmpfs       []string          `yaml:"tmpfs"`
		Volumes     []struct {
			Type     string `yaml:"type"`
			Source   string `yaml:"source"`
			Target   string `yaml:"target"`
			ReadOnly bool   `yaml:"read_only"`
			Bind     struct {
				CreateHostPath *bool `yaml:"create_host_path"`
			} `yaml:"bind"`
		} `yaml:"volumes"`
	} `yaml:"services"`
}

func loadSealedOverlay(t *testing.T) sealedOverlay {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "compose.sealed.yaml"))
	if err != nil {
		t.Fatalf("read compose.sealed.yaml: %v", err)
	}
	var overlay sealedOverlay
	if err := yaml.Unmarshal(raw, &overlay); err != nil {
		t.Fatalf("parse compose.sealed.yaml: %v", err)
	}
	return overlay
}

// Every store Deck writes to is a bind under the one state root, the named
// volumes are gone, and the report is bound read-only from the host.
func TestSealedOverlayBindsEveryStoreUnderTheStateRoot(t *testing.T) {
	overlay := loadSealedOverlay(t)
	app, ok := overlay.Services["app"]
	if !ok {
		t.Fatal("compose.sealed.yaml does not define the app service")
	}

	const root = "${NOISEDECK_SEALED_STATE_ROOT:?set the unlocked sealed state root}"
	want := map[string]string{
		"/data":                  root + "/data",
		"/home/agent":            root + "/home",
		"/workspace":             root + "/projects",
		"/tmp":                   root + "/tmp",
		"/home/agent/.config/gh": root + "/gh",
	}
	seen := map[string]bool{}
	for _, v := range app.Volumes {
		if source, pinned := want[v.Target]; pinned {
			seen[v.Target] = true
			if v.Type != "bind" || v.Source != source {
				t.Errorf("%s: got %s %q, want bind %q", v.Target, v.Type, v.Source, source)
			}
			if v.Bind.CreateHostPath == nil || *v.Bind.CreateHostPath {
				t.Errorf("%s: create_host_path must be false so a missing directory under the root is a refusal, not a plaintext directory Docker creates as root", v.Target)
			}
			if v.ReadOnly {
				t.Errorf("%s: a store must be writable", v.Target)
			}
		}
		if v.Target == "/run/noisedeck/sealed-state" {
			seen[v.Target] = true
			if !v.ReadOnly || v.Type != "bind" || !strings.Contains(v.Source, "NOISEDECK_SEALED_REPORT_DIR") {
				t.Errorf("report mount: got %+v, want a read-only bind from NOISEDECK_SEALED_REPORT_DIR", v)
			}
		}
	}
	for target := range want {
		if !seen[target] {
			t.Errorf("%s is not bound under the state root", target)
		}
	}
	if !seen["/run/noisedeck/sealed-state"] {
		t.Error("the host report is not bound at /run/noisedeck/sealed-state")
	}

	// The named volumes must be reset, not merely unused: a leftover
	// deck-home volume under /var/lib/docker is exactly the plaintext store
	// the overlay exists to remove.
	if overlay.Volumes.Tag != "!reset" {
		t.Errorf("top-level volumes must carry !reset so the base file's named volumes disappear; got tag %q", overlay.Volumes.Tag)
	}
}

// /tmp leaves RAM: host root reads a container tmpfs through
// /proc/<pid>/root, so on the shared-host profile the temporary files must
// sit under the state root like everything else.
func TestSealedOverlayMovesTmpOffTmpfs(t *testing.T) {
	overlay := loadSealedOverlay(t)
	app := overlay.Services["app"]
	for _, mount := range app.Tmpfs {
		if strings.HasPrefix(mount, "/tmp:") || mount == "/tmp" {
			t.Fatalf("compose.sealed.yaml keeps /tmp on tmpfs: %q", mount)
		}
	}
	if len(app.Tmpfs) != 1 || !strings.HasPrefix(app.Tmpfs[0], "/run/noisedeck:") {
		t.Errorf("tmpfs list should be exactly the /run/noisedeck scratch mount, got %v", app.Tmpfs)
	}
}

// The container's processes must be the mount owner on the host: a keep-id
// user namespace maps the invoking user's uid to the runtime uid.
func TestSealedOverlayKeepsTheInvokingUsersIdentity(t *testing.T) {
	overlay := loadSealedOverlay(t)
	if overlay.Services["app"].UsernsMode != "" {
		t.Fatalf("compose.sealed.yaml sets userns_mode %q; Docker on the own-host profile accepts none, so keep-id belongs to the Podman overlay", overlay.Services["app"].UsernsMode)
	}
	raw, err := os.ReadFile(filepath.Join("..", "compose.sealed-podman.yaml"))
	if err != nil {
		t.Fatalf("read compose.sealed-podman.yaml: %v", err)
	}
	var podman sealedOverlay
	if err := yaml.Unmarshal(raw, &podman); err != nil {
		t.Fatal(err)
	}
	userns := podman.Services["app"].UsernsMode
	if !strings.HasPrefix(userns, "keep-id:uid=${NOISEDECK_RUNTIME_UID:-1026}") {
		t.Fatalf("podman overlay userns_mode = %q, want a keep-id mapping to the runtime uid", userns)
	}
	if len(podman.Services["app"].Volumes) != 0 || len(podman.Services["app"].Environment) != 0 {
		t.Error("the Podman overlay must carry only the user namespace; every mount and setting stays in compose.sealed.yaml")
	}
}

func TestSealedOverlayRequiresEncryptedState(t *testing.T) {
	overlay := loadSealedOverlay(t)
	env := overlay.Services["app"].Environment
	if env["NOISEDECK_SEALED_REQUIRE_ENCRYPTED_STATE"] != "true" {
		t.Errorf("NOISEDECK_SEALED_REQUIRE_ENCRYPTED_STATE = %q, want \"true\"", env["NOISEDECK_SEALED_REQUIRE_ENCRYPTED_STATE"])
	}
	if env["NOISEDECK_SEALED_REPORT_PATH"] != "/run/noisedeck/sealed-state/report.json" {
		t.Errorf("NOISEDECK_SEALED_REPORT_PATH = %q", env["NOISEDECK_SEALED_REPORT_PATH"])
	}
	if !strings.Contains(env["NOISEDECK_SEALED_PROFILE"], "shared-host") {
		t.Errorf("the default profile must be shared-host, got %q", env["NOISEDECK_SEALED_PROFILE"])
	}
}

func TestSealedOverlaySwapPolicyFallsBackToLegacy(t *testing.T) {
	env := loadSealedOverlay(t).Services["app"].Environment
	const want = "${NOISEDECK_SEALED_SWAP_POLICY:-${NOISEDECK_SWAP_POLICY:-personal}}"
	if env["NOISEDECK_SEALED_SWAP_POLICY"] != want {
		t.Errorf("swap policy interpolation = %q, want canonical then legacy then personal", env["NOISEDECK_SEALED_SWAP_POLICY"])
	}
}

// The chat identity is the container's host name: a2a announces its key
// under it and peers pin that name, so a down and up must not change it.
// The overlay fixes it from a required value, never the container id.
func TestSealedOverlayFixesTheHostName(t *testing.T) {
	overlay := loadSealedOverlay(t)
	hostname := overlay.Services["app"].Hostname
	if !strings.HasPrefix(hostname, "${NOISEDECK_SEALED_HOSTNAME:?") {
		t.Errorf("hostname = %q, want a required NOISEDECK_SEALED_HOSTNAME", hostname)
	}
}

// The wrapper qualifies the host before compose starts anything, writes the
// report only after the container exists, never runs as root or under a
// rootless daemon, reads the env file through the preflight's dotenv parser
// instead of sourcing it, and addresses its own Compose project.
func TestSealedComposeWrapperOrdersPreflightAroundComposeUp(t *testing.T) {
	raw, err := os.ReadFile("sealed-compose.sh")
	if err != nil {
		t.Fatalf("read sealed-compose.sh: %v", err)
	}
	script := string(raw)
	upCase := strings.Index(script, "\n  up)")
	if upCase < 0 {
		t.Fatal("no up) case in sealed-compose.sh")
	}
	upBody := script[upCase:]
	if end := strings.Index(upBody, "\n  report)"); end > 0 {
		upBody = upBody[:end]
	}
	qualifyAt := strings.Index(upBody, "qualify")
	composeUpAt := strings.Index(upBody, "compose up -d")
	reportAt := strings.Index(upBody, "write_report")
	checkAt := strings.Index(upBody, "container_check")
	if qualifyAt < 0 || composeUpAt < 0 || reportAt < 0 || checkAt < 0 {
		t.Fatalf("up) must qualify, compose up, write_report and container_check; body:\n%s", upBody)
	}
	if qualifyAt >= composeUpAt || composeUpAt >= reportAt || reportAt >= checkAt {
		t.Errorf("up) order is wrong: qualify@%d compose up@%d report@%d check@%d", qualifyAt, composeUpAt, reportAt, checkAt)
	}
	for _, want := range []string{
		`"$preflight" qualify --env-file "$env_file"`,
		`"$preflight" report --env-file "$env_file" --engine "$engine" --container "$app_id"`,
		`engine=${compose_cmd%% *}`,
		"compose.sealed-podman.yaml",
		`"$preflight" env --env-file "$env_file" "$1"`,
		"compose.sealed.yaml",
		`--project-name "$project"`,
		`--env-file "$env_file"`,
		`test "$(id -u)" = 0`,
		"podman info --format '{{.Host.Security.Rootless}}'",
		`test "$rootless" = true`,
		"stat -f -c %T",
		"noise deck sealed-check",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("sealed-compose.sh lacks %q", want)
		}
	}
	for _, forbidden := range []string{`. "$env_file"`, "xargs", "set -a"} {
		if strings.Contains(script, forbidden) {
			t.Errorf("sealed-compose.sh must not use %q: the env file is dotenv, not shell, and arguments must not be re-split", forbidden)
		}
	}
	if !strings.Contains(script, `""|deck)`) {
		t.Error("sealed-compose.sh must refuse an empty or default (deck) COMPOSE_PROJECT_NAME")
	}
}

// On the own-host profile the container runs as the image's runtime uid,
// another host uid, so the report directory must be traversable and
// readable by everyone; the wrapper makes it so before qualifying and
// refuses a parent that other users cannot traverse.
func TestSealedComposeWrapperMakesTheOwnHostReportReadable(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 is unavailable")
	}
	if out, err := exec.Command("stat", "-f", "-c", "%T", "/dev/shm").Output(); err != nil || strings.TrimSpace(string(out)) != "tmpfs" {
		t.Skip("/dev/shm is not a tmpfs here")
	}
	dir := t.TempDir()
	for _, sub := range []string{"data", "home", "projects", "tmp", "gh"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	parent, err := os.MkdirTemp("/dev/shm", "sealed-test-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(parent) })
	if err := os.Chmod(parent, 0o755); err != nil {
		t.Fatal(err)
	}
	reportDir := filepath.Join(parent, "report")
	envFile := filepath.Join(dir, "compose.env")
	body := "COMPOSE_PROJECT_NAME=partner\nNOISEDECK_SEALED_PROFILE=own-host\nNOISEDECK_SEALED_STATE_ROOT=" + dir + "\nNOISEDECK_SEALED_REPORT_DIR=" + reportDir + "\n"
	if err := os.WriteFile(envFile, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	out, _ := exec.Command("sh", "sealed-compose.sh", envFile, "check").CombinedOutput()
	info, err := os.Stat(reportDir)
	if err != nil {
		t.Fatalf("report directory was not created: %v (out: %s)", err, out)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("own-host report directory mode = %04o, want 0755 so the runtime uid can read the report", info.Mode().Perm())
	}
	// The qualification that follows refuses this plaintext temp dir by its
	// backing, naming the store; the directory contract was applied first.
	if !strings.Contains(string(out), "sealed-preflight: refusing: data ("+filepath.Join(dir, "data")+"): plaintext") &&
		!strings.Contains(string(out), "sealed-preflight: refusing: data ("+filepath.Join(dir, "data")+"): unresolved") {
		t.Fatalf("expected qualification to refuse the plaintext state root by its backing; out=%s", out)
	}

	// A parent other users cannot traverse is refused before qualification.
	private, err := os.MkdirTemp("/dev/shm", "sealed-private-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(private) })
	info, err = os.Stat(private)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("private parent directory mode = %04o, want 0700", info.Mode().Perm())
	}
	body = "COMPOSE_PROJECT_NAME=partner\nNOISEDECK_SEALED_PROFILE=own-host\nNOISEDECK_SEALED_STATE_ROOT=" + dir + "\nNOISEDECK_SEALED_REPORT_DIR=" + filepath.Join(private, "report") + "\n"
	if err := os.WriteFile(envFile, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err = exec.Command("sh", "sealed-compose.sh", envFile, "check").CombinedOutput()
	if err == nil || !strings.Contains(string(out), private+" is not traversable by other users") {
		t.Fatalf("expected the private parent %s to be refused by name; err=%v out=%s", private, err, out)
	}
}

// The committed wrapper is executable so you can invoke it directly.
func TestSealedComposeWrapperIsExecutable(t *testing.T) {
	out, err := exec.Command("git", "ls-files", "-s", "sealed-compose.sh").Output()
	if err != nil {
		t.Skipf("git unavailable: %v", err)
	}
	if !strings.HasPrefix(string(out), "100755") {
		t.Fatalf("sealed-compose.sh is committed with mode %s, want 100755", strings.Fields(string(out))[0])
	}
}

// The wrapper refuses before anything runs when the env file names the
// default project, has no project name, or has no state root.
func TestSealedComposeWrapperRefusesBeforeUp(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 is unavailable")
	}
	cases := map[string]string{
		"default project":                 "COMPOSE_PROJECT_NAME=deck\nNOISEDECK_SEALED_STATE_ROOT=/x\nNOISEDECK_SEALED_REPORT_DIR=/y\n",
		"no project":                      "NOISEDECK_SEALED_STATE_ROOT=/x\nNOISEDECK_SEALED_REPORT_DIR=/y\n",
		"no state root":                   "COMPOSE_PROJECT_NAME=partner\nNOISEDECK_SEALED_REPORT_DIR=/y\n",
		"no report dir":                   "COMPOSE_PROJECT_NAME=partner\nNOISEDECK_SEALED_STATE_ROOT=/x\n",
		"space in root ok":                "COMPOSE_PROJECT_NAME=partner\nNOISEDECK_SEALED_STATE_ROOT=/srv/partner state\nNOISEDECK_SEALED_REPORT_DIR=/y\nNOISEDECK_SEALED_PROFILE=own-host\n",
		"shared host on a rootful podman": "COMPOSE_PROJECT_NAME=partner\nNOISEDECK_SEALED_STATE_ROOT=/x\nNOISEDECK_SEALED_REPORT_DIR=/y\nNOISEDECK_SEALED_PROFILE=shared-host\n",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			envFile := filepath.Join(dir, "compose.env")
			if err := os.WriteFile(envFile, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			// `check` runs only the qualification; with a fake docker on
			// PATH that records every call, nothing may reach compose. The
			// fake podman answers as a root daemon would, whatever the
			// machine running this test has installed.
			fake := filepath.Join(dir, "docker")
			if err := os.WriteFile(fake, []byte("#!/bin/sh\necho \"docker $*\" >> \"$FAKE_LOG\"\necho rootful\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "podman"), []byte("#!/bin/sh\necho \"podman $*\" >> \"$FAKE_LOG\"\necho false\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			logPath := filepath.Join(dir, "calls.log")
			cmd := exec.Command("sh", "sealed-compose.sh", envFile, "check")
			// The fakes come first on PATH, so the wrapper sees them and not
			// whatever engines this machine has.
			cmd.Env = append(os.Environ(), "PATH="+dir+":/usr/bin:/bin", "FAKE_LOG="+logPath)
			out, err := cmd.CombinedOutput()
			calls, _ := os.ReadFile(logPath)
			if strings.Contains(string(calls), "compose") {
				t.Fatalf("%s: compose was invoked before the refusal: %s", name, calls)
			}
			if name == "shared host on a rootful podman" {
				if err == nil || !strings.Contains(string(out), "not running rootless") {
					t.Fatalf("expected the shared-host profile to refuse a rootful podman; got err=%v out=%s", err, out)
				}
				return
			}
			if name == "space in root ok" {
				// The parser kept the space; the refusal that follows is the
				// missing directory, named with the space intact.
				if err == nil || !strings.Contains(string(out), "/srv/partner state is not a directory") {
					t.Fatalf("expected the state root with its space to be named; got err=%v out=%s", err, out)
				}
				return
			}
			if err == nil {
				t.Fatalf("%s: wrapper did not refuse; out=%s", name, out)
			}
		})
	}
}
