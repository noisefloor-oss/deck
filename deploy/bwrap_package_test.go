package deploy

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/noisefloor-oss/deck/internal/supervisor"
	"gopkg.in/yaml.v3"
)

const bwrapRecipe = "bwrap/build-bwrap.sh"

func readBwrapRecipe(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(bwrapRecipe))
	if err != nil {
		t.Fatalf("read %s: %v", bwrapRecipe, err)
	}
	return string(raw)
}

func TestBwrapRecipePinsClosedInputs(t *testing.T) {
	recipe := readBwrapRecipe(t)
	for _, required := range []string{
		"https://github.com/containers/bubblewrap/releases/download/v${BUBBLEWRAP_VERSION}/bubblewrap-${BUBBLEWRAP_VERSION}.tar.xz",
		"BUBBLEWRAP_VERSION=0.11.2",
		"BUBBLEWRAP_SHA256=" + supervisor.PinnedBubblewrapArchiveSHA256,
		"https://kernel.org/pub/linux/libs/security/linux-privs/libcap2/libcap-${LIBCAP_VERSION}.tar.xz",
		"LIBCAP_VERSION=2.71",
		"LIBCAP_SHA256=b7006c9af5168315f35fc734bf1a8d2aa70766bd8b8c4340962e05b19c35b900",
		"https://ftp.gnu.org/gnu/make/make-${MAKE_VERSION}.tar.gz",
		"https://mirrors.ocf.berkeley.edu/gnu/make/make-${MAKE_VERSION}.tar.gz",
		"https://mirror.csclub.uwaterloo.ca/gnu/make/make-${MAKE_VERSION}.tar.gz",
		"MAKE_VERSION=4.4.1",
		"MAKE_SHA256=dd16fb1d67bfab79a72f5e8390735c49e3e8e70b4945a15ab1f81ddb78658fb3",
		"https://ziglang.org/download/${ZIG_VERSION}/zig-x86_64-linux-${ZIG_VERSION}.tar.xz",
		"ZIG_VERSION=0.14.1",
		"ZIG_SHA256=24aeeec8af16c381934a6cd7d95c807a8cb2cf7df9fa40d359aa884195c4716c",
		"sha256sum --check --strict",
		"--retry-all-errors",
		`--connect-timeout 10 --max-time "$max_time"`,
		`--retry "$retries"`,
		`fetch 300 0 "https://ziglang.org/`,
		"SOURCE_DATE_EPOCH=1735689600",
		"export SOURCE_DATE_EPOCH LC_ALL=C LANG=C TZ=UTC",
		"umask 022",
	} {
		if !strings.Contains(recipe, required) {
			t.Errorf("bwrap recipe is missing immutable pin %q", required)
		}
	}
}

// Exercise the recipe's actual fetch function: a failed source falls back,
// while a successful transfer with the wrong bytes must fail closed.
func TestBwrapFetchVerifiesFallbackBytes(t *testing.T) {
	recipe := readBwrapRecipe(t)
	start := strings.Index(recipe, "fetch() {")
	if start < 0 {
		t.Fatal("fetch function not found")
	}
	end := strings.Index(recipe[start:], "\nextract() {")
	if end < 0 {
		t.Fatal("fetch function not found")
	}
	dir := t.TempDir()
	script := recipe[start:start+end] + `
SRC=$1
curl() {
  case "$*" in
    *"--retry 0"*"--max-time 300"*) ;;
    *) echo "incorrect large archive transfer budget" >&2; return 91 ;;
  esac
  while [ "$#" -gt 0 ]; do
    case "$1" in
      https://unavailable/*) return 28 ;;
      --output) shift; printf '%s' "$FETCH_BYTES" >"$1"; return ;;
    esac
    shift
  done
  return 92
}
fetch 300 0 https://unavailable/zig.tar.xz "$2" https://fallback/zig.tar.xz
`
	run := func(bytes string) error {
		cmd := exec.Command("sh", "-eu", "-c", script, "fetch-test", dir, "2689367b205c16ce32ed4200942b8b8b1e262dfc70d9bc9fbc77c49699a4f1df")
		cmd.Env = append(os.Environ(), "FETCH_BYTES="+bytes)
		out, err := cmd.CombinedOutput()
		t.Logf("%s", out)
		return err
	}
	if err := run("ok"); err != nil {
		t.Fatalf("verified fallback failed: %v", err)
	}
	if err := os.Remove(filepath.Join(dir, "zig.tar.xz")); err != nil {
		t.Fatal(err)
	}
	if err := run("tampered"); err == nil {
		t.Fatal("unverified download accepted")
	}
	if err := run("ok"); err == nil {
		t.Fatal("cached unverified bytes accepted")
	}
}

func TestBwrapRecipeBuildsDeterministicStaticExecutable(t *testing.T) {
	recipe := readBwrapRecipe(t)
	for _, required := range []string{
		"-Wl,--no-undefined -static -O2 -g0",
		"-O2 -g0 -c",
		`"$CC" -s -o "$OBJ/bwrap"`,
		"PACKAGE_STRING \"bubblewrap $BUBBLEWRAP_VERSION\"",
		"-D_GNU_SOURCE -D_FILE_OFFSET_BITS=64",
		"install -m 0555 \"$OBJ/bwrap\" \"$OUT/$TARGET\"",
		"rootfs/usr/local/libexec/noise/bwrap",
		`[ "$(stat -c %h "$OUT/$TARGET")" = "1" ]`,
		`[ "$(stat -c %a "$OUT/$TARGET")" = "555" ]`,
		`[ "$(stat -c %F "$OUT/$TARGET")" = "regular file" ]`,
		"readelf -l \"$OUT/$TARGET\" | grep -q INTERP && fail",
		"readelf -d \"$OUT/$TARGET\" | grep -q NEEDED && fail",
		`grep -qF "$WORK"`,
		"grep -qxF \"$VERSION_STRING\"",
		"--version",
		"not a dynamic executable",
		"--sort=name --format=ustar --owner=0 --group=0 --numeric-owner",
		"--mtime=\"@$SOURCE_DATE_EPOCH\"",
		"payload = \"usr/local/libexec/noise/bwrap\"",
		"if [m.name for m in members].count(payload) != 1:",
		"if any(not m.isdir() for m in members if m.name != payload):",
		"if not member.isfile():",
		"if member.mode != 0o555 or member.uid != 0 or member.gid != 0:",
		"EXPECTED_EXECUTABLE_SHA256_AMD64=7710b19d89bedbdd3b1dd8ac46643f188f8fdc92917fe3093abd827eb783cf7f",
		"EXPECTED_ROOTFS_TAR_SHA256_AMD64=f61874a5bd6e02d89ffd0022f0bbd486a74948325923e7c8cac700b604501344",
		"packaged rootfs tar digest $TARSHA256 does not match the pin",
		"rootfs-tar-sha256",
		"build path leaked into executable",
	} {
		if !strings.Contains(recipe, required) {
			t.Errorf("bwrap recipe is missing deterministic contract %q", required)
		}
	}
	if !strings.Contains(recipe, `echo "build-bwrap: unsupported architecture: $ARCH" >&2`) {
		t.Error("bwrap recipe does not fail closed on an unsupported architecture")
	}
	if !strings.Contains(recipe, "exit 64") {
		t.Error("bwrap recipe lacks the unsupported-architecture exit status")
	}
}

func TestBwrapRecipeMatchesSupervisorExecutablePins(t *testing.T) {
	recipe := readBwrapRecipe(t)
	pattern := regexp.MustCompile(`EXPECTED_EXECUTABLE_SHA256_AMD64=([0-9a-f]{64})`)
	match := pattern.FindStringSubmatch(recipe)
	if match == nil {
		t.Fatal("bwrap recipe does not pin the linux/amd64 executable digest")
	}
	pin, err := supervisor.ResolveBubblewrapExecutableSHA256("linux", "amd64")
	if err != nil {
		t.Fatalf("resolve linux/amd64 pin: %v", err)
	}
	if match[1] != pin {
		t.Fatalf("bwrap recipe pins linux/amd64 as %s, supervisor pin is %s", match[1], pin)
	}
}

// pinnedGoBuildImage resolves the digest-pinned Go builder image from the
// runtime Dockerfile so the proof uses the exact image the release builds
// with instead of a second, drifting copy of the digest.
func pinnedGoBuildImage(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "Dockerfile"))
	if err != nil {
		t.Fatalf("read Dockerfile: %v", err)
	}
	pattern := regexp.MustCompile(`FROM (golang:[^\s]+) AS go-build`)
	match := pattern.FindStringSubmatch(string(raw))
	if match == nil {
		t.Fatal("Dockerfile has no digest-pinned golang go-build stage")
	}
	if !strings.Contains(match[1], "@sha256:") {
		t.Fatalf("go-build image %q is not pinned by digest", match[1])
	}
	return match[1]
}

// TestBwrapDoubleBuildReproducesPinnedExecutable is the packaging proof: two
// independent clean builds of the single supported target inside the pinned
// builder image must reproduce byte-for-byte the digest compiled into the
// supervisor's closed GOARCH map. It needs Docker and skips where the
// runtime is unavailable.
func TestBwrapDoubleBuildReproducesPinnedExecutable(t *testing.T) {
	if os.Getenv("NOISEDECK_CI_FAST") == "1" {
		t.Skip("NOISEDECK_CI_FAST=1: the slow acceptance workflow certifies the double-build proof on every main push and twice daily")
	}
	docker, err := exec.LookPath("docker")
	if err != nil {
		t.Skip("docker is not available; the Bubblewrap double-build proof cannot run here")
	}
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	image := pinnedGoBuildImage(t)

	pin, err := supervisor.ResolveBubblewrapExecutableSHA256("linux", "amd64")
	if err != nil {
		t.Fatalf("resolve linux/amd64 pin: %v", err)
	}
	var observed []string
	for run := 1; run <= 2; run++ {
		cmd := exec.Command(docker, "run", "--rm",
			"--volume", root+":/src", "--workdir", "/src",
			"--env", "TMPDIR=/tmp",
			image, "deploy/bwrap/build-bwrap.sh", "amd64", "/tmp/bwrap-amd64-proof")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("linux/amd64 build %d: %v\n%s", run, err, out)
		}
		digest := ""
		for _, line := range strings.Split(string(out), "\n") {
			if fields := strings.Fields(line); len(fields) == 3 && fields[0] == "executable-sha256" {
				digest = fields[2]
			}
		}
		if digest == "" {
			t.Fatalf("linux/amd64 build %d reported no executable digest:\n%s", run, out)
		}
		observed = append(observed, digest)
	}
	if observed[0] != observed[1] {
		t.Fatalf("linux/amd64 builds differ: %s != %s", observed[0], observed[1])
	}
	if observed[0] != pin {
		t.Fatalf("linux/amd64 reproduced %s, supervisor pin is %s", observed[0], pin)
	}
}

func TestBwrapProofStopsAfterFailedBuild(t *testing.T) {
	var workflow struct {
		Jobs map[string]struct{ Steps []struct{ Name, Run string } }
	}
	if err := yaml.Unmarshal(readWorkflow(t, "release-acceptance.yaml"), &workflow); err != nil {
		t.Fatal(err)
	}
	proof := ""
	for _, step := range workflow.Jobs["bwrap-proof"].Steps {
		if step.Name == "Prove the pinned Bubblewrap executable identity" {
			proof = step.Run
		}
	}
	if proof == "" {
		t.Fatal("proof step missing")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0700); err != nil {
		t.Fatal(err)
	}
	// A real failed container must escape tee and stop the loop immediately.
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte("#!/bin/sh\necho called >> \"$CALL_LOG\"\nexit 42\n"), 0700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", "-c", proof)
	cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "RUNNER_TEMP="+dir, "GITHUB_ENV="+filepath.Join(dir, "env"), "GITHUB_RUN_ID=1", "GITHUB_RUN_ATTEMPT=1", "CALL_LOG="+filepath.Join(dir, "calls"))
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 42 {
		t.Fatalf("proof did not propagate build failure: %v\n%s", err, out)
	}
	calls, err := os.ReadFile(filepath.Join(dir, "calls"))
	if err != nil {
		t.Fatal(err)
	}
	if string(calls) != "called\n" {
		t.Fatalf("proof continued after failed build: %q", calls)
	}
}
