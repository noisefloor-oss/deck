package deploy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func productionFixture() map[string]any {
	return map[string]any{"services": map[string]any{
		"app": map[string]any{
			"build": map[string]any{
				"target":              "runtime",
				"dockerfile":          "/checkout/deploy/derived/go.Dockerfile",
				"additional_contexts": map[string]any{"deck-base": "service:go-base"},
			},
			"image":          "deck:abc1234",
			"container_name": "noisefloor-deck",
			"healthcheck":    map[string]any{"test": []any{"CMD", "curl"}},
			"labels": map[string]any{
				"com.noisefloor.deck.capability-profile": "production-v1",
			},
			"environment": map[string]any{
				"NOISEDECK_AUTH_MODE":                               "oidc",
				"NOISEDECK_SERVER_ALLOWED_ORIGINS":                  "https://deck.example.com",
				"NOISEDECK_AUTH_GOOGLE_CLIENT_ID":                   "client",
				"NOISEDECK_AUTH_GOOGLE_CLIENT_SECRET":               "secret",
				"NOISEDECK_AUTH_GOOGLE_REDIRECT_URL":                "https://deck.example.com/auth/callback",
				"NOISEDECK_AUTH_GOOGLE_ALLOWED_EMAIL":               "operator@example.com",
				"NOISEDECK_AUTH_SESSION_SECRET":                     "session-secret",
				"NOISEDECK_DEPLOYMENT_HOST_SHA256":                  strings.Repeat("a", 64),
				"MEMPALACE_MCP_IDLE_HOURS":                          "0",
				"MEMPALACE_MCP_HTTP_ALLOW_INSECURE_NO_TOKEN":        "1",
				"NOISE_BROWSER_WS":                                  "ws://127.0.0.1:3000/",
				"NOISEDECK_EXPERIMENTS_UNCONFINED_CAPACITY_ROUTING": "false",
			},
			"ports": []any{map[string]any{"host_ip": "192.0.2.10", "target": 8837}, map[string]any{"host_ip": "127.0.0.1", "target": 8766}},
			"volumes": []any{
				map[string]any{"type": "bind", "source": "/srv/mempalace", "target": "/home/agent/.mempalace"},
				map[string]any{"type": "bind", "source": "/srv/noisedeck-tmp", "target": "/tmp"},
			},
			"tmpfs": []any{"/run/noisedeck:rw,size=16m"},
		},
		"go-base": map[string]any{
			"build": map[string]any{"target": "mempalace"},
			"scale": 0,
		},
		"chromium": map[string]any{"network_mode": "service:app", "volumes": []any{sidecarResolverFixture()}},
	}}
}

func sidecarResolverFixture() map[string]any {
	source, err := filepath.Abs("sidecar-resolv.conf")
	if err != nil {
		panic(err)
	}
	return map[string]any{"type": "bind", "source": source, "target": "/etc/resolv.conf", "read_only": true,
		"bind": map[string]any{"selinux": "z", "create_host_path": false}}
}

func verifyProductionFixture(t *testing.T, fixture map[string]any) (string, error) {
	t.Helper()
	payload, err := json.Marshal(fixture)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("python3", "verify-production-compose.py")
	cmd.Dir = "."
	cmd.Stdin = bytes.NewReader(payload)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func verifyProductionTmpHost(t *testing.T, source, runtimeUser string) (string, error) {
	t.Helper()
	fixture := map[string]any{"services": map[string]any{"app": map[string]any{
		"user":    runtimeUser,
		"volumes": []any{map[string]any{"source": source, "target": "/tmp"}},
	}}}
	payload, err := json.Marshal(fixture)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "compose.json")
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("./verify-production-tmp-host.sh", path)
	cmd.Dir = "."
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func TestProductionTmpHostVerifierUsesEffectiveFilesystem(t *testing.T) {
	diskSource, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	identity := fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid())
	if out, err := verifyProductionTmpHost(t, diskSource, identity); err != nil {
		t.Fatalf("disk-backed source failed: %v\n%s", err, out)
	}

	if _, err := os.Stat("/dev/shm"); err != nil {
		t.Skip("host has no /dev/shm RAM filesystem")
	}
	out, err := verifyProductionTmpHost(t, "/dev/shm", "0:0")
	if err == nil || !strings.Contains(out, "RAM-backed") {
		t.Fatalf("RAM-backed bind source result = %v, output %q", err, out)
	}
}

func TestProductionTmpHostVerifierChecksRuntimeAccess(t *testing.T) {
	// This verifier rejects RAM-backed binds; /tmp may itself be tmpfs.
	checkout, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	source, err := os.MkdirTemp(checkout, ".tmp-host-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(source) })
	if err := os.Chmod(source, 0o777); err != nil {
		t.Fatal(err)
	}
	otherIdentity := fmt.Sprintf("%d:%d", os.Getuid()+1, os.Getgid()+1)
	if out, err := verifyProductionTmpHost(t, source, otherIdentity); err != nil {
		t.Fatalf("writable source owned by another identity failed: %v\n%s", err, out)
	}
	if err := os.Chmod(source, 0o700); err != nil {
		t.Fatal(err)
	}
	out, err := verifyProductionTmpHost(t, source, otherIdentity)
	if err == nil || !strings.Contains(out, "rwx permissions") {
		t.Fatalf("inaccessible source result = %v, output %q", err, out)
	}
}

func TestProductionTmpHostVerifierRejectsMissingSource(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	identity := fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid())
	out, err := verifyProductionTmpHost(t, missing, identity)
	if err == nil || !strings.Contains(out, "not an existing directory") {
		t.Fatalf("missing bind source result = %v, output %q", err, out)
	}
}

func TestProductionComposeVerifierAcceptsCompleteProfile(t *testing.T) {
	out, err := verifyProductionFixture(t, productionFixture())
	if err != nil {
		t.Fatalf("complete production profile failed: %v\n%s", err, out)
	}
}

func TestProductionPreviewConnectorProfile(t *testing.T) {
	fixture := productionFixture()
	services := fixture["services"].(map[string]any)
	env := services["app"].(map[string]any)["environment"].(map[string]any)
	env["NOISEDECK_PROJECT_PREVIEWS_BIND"] = "127.0.0.1:8840"
	env["NOISEDECK_PROJECT_PREVIEWS_UNCONFINED"] = "true"
	connector := map[string]any{
		"image":        "cloudflare/cloudflared@sha256:faa8e0a1475f137c910be6025f43c8e4092fdd35461b31e19fd1390657a1ff39",
		"network_mode": "service:app", "read_only": true, "user": "65532:65532", "cap_drop": []string{"ALL"},
		"security_opt": []string{"no-new-privileges:true"}, "cpus": 0.25, "mem_limit": 268435456,
		"volumes": []any{map[string]any{"target": "/run/secrets/preview-token", "read_only": true}, sidecarResolverFixture()},
	}
	services["preview-tunnel"] = connector
	if out, err := verifyProductionFixture(t, fixture); err != nil {
		t.Fatalf("valid connector: %v\n%s", err, out)
	}
	// Compose emits byte sizes as a JSON string on the production host.
	connector["mem_limit"] = "268435456"
	if out, err := verifyProductionFixture(t, fixture); err != nil {
		t.Fatalf("rendered connector memory limit: %v\n%s", err, out)
	}
	for key, bad := range map[string]any{"network_mode": "host", "ports": []any{map[string]any{"target": 8840}}, "read_only": false, "user": "0:0", "image": "cloudflare/cloudflared:latest", "volumes": []any{}, "cap_drop": []string{}, "security_opt": []string{}, "cpus": 0, "mem_limit": 0} {
		old := connector[key]
		connector[key] = bad
		if _, err := verifyProductionFixture(t, fixture); err == nil {
			t.Fatalf("unsafe connector accepted: %s", key)
		}
		connector[key] = old
	}
}

func TestProductionPreviewConnectorLifecycle(t *testing.T) {
	raw, err := os.ReadFile("production-compose.sh")
	if err != nil {
		t.Fatal(err)
	}
	script := string(raw)
	start := strings.Index(script, "\napply_images() {")
	end := strings.Index(script, "\npreflight() {")
	if start < 0 || end <= start {
		t.Fatal("cannot locate deployment lifecycle function")
	}
	for _, tc := range []struct {
		name, connector, optIn string
		refused                bool
		want                   string
	}{
		{"stopped app with connector", "old-connector", "", true, ""},
		{"removed app with connector", "orphan-connector", "", true, ""},
		{"no previews", "", "", false, "--force-recreate app chromium"},
		{"enabled previews", "old-connector", "/private/token", false, "--force-recreate app chromium preview-tunnel"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			rendered := filepath.Join(dir, "rendered.json")
			if err := os.WriteFile(rendered, []byte(`{"name":"expected-project"}`), 0o600); err != nil {
				t.Fatal(err)
			}
			// App inspection deliberately returns nothing. Project-label discovery
			// must still find stopped and orphaned connectors before any rollout.
			stub := `
compose() {
  case "$*" in
    "up "*) printf 'ROLLOUT:%s\n' "$*" ;;
    "ps "*) ;;
    *) return 91 ;;
  esac
}
docker() {
  if test "$*" = "ps -aq --filter label=com.docker.compose.project=expected-project --filter label=com.docker.compose.service=relay-tunnel"; then return 0; fi
  test "$*" = "ps -aq --filter label=com.docker.compose.project=expected-project --filter label=com.docker.compose.service=preview-tunnel" || return 92
  printf '%s' "$CONNECTOR"
}
`
			cmd := exec.Command("sh", "-c", "set -eu\n"+stub+script[start:end]+"\napply_images\n")
			cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "rendered=" + rendered, "CONNECTOR=" + tc.connector, "NOISEDECK_PREVIEW_TUNNEL_TOKEN_FILE=" + tc.optIn}
			out, err := cmd.CombinedOutput()
			if tc.refused {
				if err == nil || strings.Contains(string(out), "ROLLOUT:") || !strings.Contains(string(out), "existing preview connector") {
					t.Fatalf("did not refuse before rollout: %v %s", err, out)
				}
			} else if err != nil || !strings.Contains(string(out), tc.want) {
				t.Fatalf("rollout command: %v %s", err, out)
			}
		})
	}
}

func TestProductionPreviewConnectorOptIn(t *testing.T) {
	directory := t.TempDir()
	token := filepath.Join(directory, "connector-token")
	envFile := filepath.Join(directory, "compose.env")
	for _, path := range []string{token, envFile} {
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(directory, "docker"), []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$ARGS\"\nexit 42\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, path      string
		docker, overlay bool
	}{
		{"default", "", true, false}, {"enabled", token, true, true},
		{"relative", "relative/token", false, false}, {"missing", filepath.Join(directory, "absent"), false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			argsFile := filepath.Join(directory, tc.name+".args")
			cmd := exec.Command("./production-compose.sh", envFile, "config")
			cmd.Env = []string{"PATH=" + directory + ":" + os.Getenv("PATH"), "ARGS=" + argsFile, "NOISEDECK_PREVIEW_TUNNEL_TOKEN_FILE=" + tc.path}
			if out, err := cmd.CombinedOutput(); err == nil {
				t.Fatalf("expected stub/refusal: %s", out)
			}
			args, err := os.ReadFile(argsFile)
			if !tc.docker {
				if !os.IsNotExist(err) {
					t.Fatalf("invalid token path reached Docker: %s", args)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(args), "compose.preview-tunnel.yaml") != tc.overlay {
				t.Fatalf("wrong overlay: %s", args)
			}
		})
	}
}

// An explicitly enabled experiment switch is still a valid production
// profile: the verifier accepts exactly true or false, nothing else.
func TestProductionComposeVerifierAcceptsEnabledCapacityExperiment(t *testing.T) {
	fixture := productionFixture()
	env := fixture["services"].(map[string]any)["app"].(map[string]any)["environment"].(map[string]any)
	env["NOISEDECK_EXPERIMENTS_UNCONFINED_CAPACITY_ROUTING"] = "true"
	if out, err := verifyProductionFixture(t, fixture); err != nil {
		t.Fatalf("enabled experiment switch failed: %v\n%s", err, out)
	}
}

func TestProductionComposeVerifierRejectsLostCapabilities(t *testing.T) {
	tests := map[string]func(map[string]any){
		"plain MemPalace target": func(f map[string]any) {
			f["services"].(map[string]any)["app"].(map[string]any)["build"].(map[string]any)["target"] = "mempalace"
		},
		"wrong derived Dockerfile": func(f map[string]any) {
			f["services"].(map[string]any)["app"].(map[string]any)["build"].(map[string]any)["dockerfile"] = "/checkout/Dockerfile"
		},
		"detached derived base": func(f map[string]any) {
			f["services"].(map[string]any)["app"].(map[string]any)["build"].(map[string]any)["additional_contexts"].(map[string]any)["deck-base"] = "docker-image://deck:other"
		},
		"plain runtime base": func(f map[string]any) {
			f["services"].(map[string]any)["go-base"].(map[string]any)["build"].(map[string]any)["target"] = "runtime"
		},
		"runnable base service": func(f map[string]any) {
			f["services"].(map[string]any)["go-base"].(map[string]any)["scale"] = 1
		},
		"password auth": func(f map[string]any) {
			f["services"].(map[string]any)["app"].(map[string]any)["environment"].(map[string]any)["NOISEDECK_AUTH_MODE"] = "password"
		},
		"missing palace mount": func(f map[string]any) {
			volumes := f["services"].(map[string]any)["app"].(map[string]any)["volumes"].([]any)
			f["services"].(map[string]any)["app"].(map[string]any)["volumes"] = volumes[1:]
		},
		"missing disk-backed tmp mount": func(f map[string]any) {
			volumes := f["services"].(map[string]any)["app"].(map[string]any)["volumes"].([]any)
			f["services"].(map[string]any)["app"].(map[string]any)["volumes"] = volumes[:1]
		},
		"ram-backed production tmp": func(f map[string]any) {
			f["services"].(map[string]any)["app"].(map[string]any)["tmpfs"] = []any{"/tmp:rw,size=32g"}
		},
		"detached browser network": func(f map[string]any) {
			f["services"].(map[string]any)["chromium"].(map[string]any)["network_mode"] = "default"
		},
		"wildcard publish address": func(f map[string]any) {
			f["services"].(map[string]any)["app"].(map[string]any)["ports"].([]any)[0].(map[string]any)["host_ip"] = "0.0.0.0"
		},
		"missing relay port": func(f map[string]any) {
			ports := f["services"].(map[string]any)["app"].(map[string]any)["ports"].([]any)
			f["services"].(map[string]any)["app"].(map[string]any)["ports"] = ports[:1]
		},
		"relay port off loopback": func(f map[string]any) {
			f["services"].(map[string]any)["app"].(map[string]any)["ports"].([]any)[1].(map[string]any)["host_ip"] = "192.0.2.10"
		},
		"a third port": func(f map[string]any) {
			ports := f["services"].(map[string]any)["app"].(map[string]any)["ports"].([]any)
			f["services"].(map[string]any)["app"].(map[string]any)["ports"] = append(ports, map[string]any{"host_ip": "127.0.0.1", "target": 8765})
		},
		"local image tag": func(f map[string]any) {
			f["services"].(map[string]any)["app"].(map[string]any)["image"] = "deck:local"
		},
		"non-boolean capacity experiment switch": func(f map[string]any) {
			f["services"].(map[string]any)["app"].(map[string]any)["environment"].(map[string]any)["NOISEDECK_EXPERIMENTS_UNCONFINED_CAPACITY_ROUTING"] = "1"
		},
		"missing capacity experiment switch": func(f map[string]any) {
			delete(f["services"].(map[string]any)["app"].(map[string]any)["environment"].(map[string]any), "NOISEDECK_EXPERIMENTS_UNCONFINED_CAPACITY_ROUTING")
		},
		"missing healthcheck": func(f map[string]any) {
			f["services"].(map[string]any)["app"].(map[string]any)["healthcheck"] = map[string]any{}
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			fixture := productionFixture()
			mutate(fixture)
			if out, err := verifyProductionFixture(t, fixture); err == nil {
				t.Fatalf("incomplete profile passed:\n%s", out)
			}
		})
	}
}

func TestProductionCommandPinsCapabilityOrderAndVerification(t *testing.T) {
	raw, err := os.ReadFile("production-compose.sh")
	if err != nil {
		t.Fatal(err)
	}
	script := string(raw)
	ordered := []string{
		"compose.yaml", "compose.mempalace.yaml",
		"compose.go.yaml", "compose.go-mempalace.yaml",
		"compose.browser.yaml", "compose.production.yaml",
	}
	position := -1
	for _, name := range ordered {
		next := strings.Index(script[position+1:], name)
		if next < 0 {
			t.Fatalf("production command omits %s", name)
		}
		position += next + 1
	}
	for _, required := range []string{
		"config --format json", "verify-production-compose.py",
		"verify-production-tmp-host.sh", `tmpfs|ramfs`,
		`stat -f -c %T /tmp`, "noisedeck-tmp-probe",
		"compose build --pull app chromium",
		"--force-recreate app chromium", "verify_runtime",
		`chromium.connect("ws://127.0.0.1:3000/")`,
		"--check-host-files", "page.goto(process.env.NOISEDECK_VERIFY_ORIGIN",
		"prune-build-cache.sh\" --after-verified-deploy",
	} {
		if !strings.Contains(script, required) {
			t.Errorf("production command does not contain %q", required)
		}
	}
	if strings.Contains(script, "connectOverCDP") {
		t.Fatal("production verification must use the Playwright protocol exposed by browser-server.mjs")
	}
	if strings.Contains(script, "browser.disconnect()") {
		t.Fatal("production verification must close its Playwright protocol connection")
	}
	verifyAt := strings.LastIndex(script, "verify_runtime")
	pruneAt := strings.LastIndex(script, "prune-build-cache.sh")
	if verifyAt < 0 || pruneAt < 0 || verifyAt > pruneAt {
		t.Fatal("cache pruning is not gated behind runtime verification")
	}
}

func TestProductionCommandDoesNotMaskComposeRenderFailure(t *testing.T) {
	payload, err := json.Marshal(productionFixture())
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	fixture := filepath.Join(directory, "fixture.json")
	if err := os.WriteFile(fixture, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	docker := filepath.Join(directory, "docker")
	if err := os.WriteFile(docker, []byte("#!/bin/sh\ncat \"$FIXTURE\"\nexit 42\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	envFile := filepath.Join(directory, "compose.env")
	if err := os.WriteFile(envFile, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	command := exec.Command("./production-compose.sh", envFile, "config")
	command.Dir = "."
	command.Env = []string{
		"PATH=" + directory + ":" + os.Getenv("PATH"),
		"FIXTURE=" + fixture,
	}
	out, err := command.CombinedOutput()
	if err == nil {
		t.Fatalf("failed Compose render was accepted:\n%s", out)
	}
	if strings.Contains(string(out), "rendered capability profile verified") {
		t.Fatalf("verifier ran after Compose failed:\n%s", out)
	}
}

func TestProductionLicensedFontOptIn(t *testing.T) {
	directory := t.TempDir()
	fonts := filepath.Join(directory, "private fonts")
	if err := os.Mkdir(fonts, 0o700); err != nil {
		t.Fatal(err)
	}
	envFile := filepath.Join(directory, "compose.env")
	if err := os.WriteFile(envFile, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	docker := filepath.Join(directory, "docker")
	if err := os.WriteFile(docker, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$ARGS\"\nexit 42\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, path              string
		overlays, invokesDocker bool
	}{
		{"default", "", false, true},
		{"licensed", fonts, true, true},
		{"relative", "relative/fonts", false, false},
		{"missing", filepath.Join(directory, "absent"), false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			argsFile := filepath.Join(directory, tc.name+".args")
			cmd := exec.Command("./production-compose.sh", envFile, "config")
			cmd.Env = []string{"PATH=" + directory + ":" + os.Getenv("PATH"), "ARGS=" + argsFile, "NOISEDECK_LICENSED_FONTS_DIR=" + tc.path}
			if out, err := cmd.CombinedOutput(); err == nil {
				t.Fatalf("expected refusal or stub failure: %s", out)
			}
			args, err := os.ReadFile(argsFile)
			if !tc.invokesDocker {
				if !os.IsNotExist(err) {
					t.Fatalf("invalid directory reached Docker: %s, %v", args, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			for _, overlay := range []string{"compose.licensed-fonts.yaml", "compose.go-licensed-fonts.yaml"} {
				if strings.Contains(string(args), overlay) != tc.overlays {
					t.Fatalf("unexpected overlays: %s", args)
				}
				if tc.overlays && strings.Index(string(args), overlay) < strings.Index(string(args), "compose.production.yaml") {
					t.Fatalf("font overlay precedes production policy: %s", args)
				}
			}
		})
	}
	for _, entry := range []struct{ file, service string }{
		{"compose.licensed-fonts.yaml", "app"},
		{"compose.go-licensed-fonts.yaml", "go-base"},
	} {
		raw, err := os.ReadFile(filepath.Join("..", entry.file))
		if err != nil {
			t.Fatal(err)
		}
		var overlay struct {
			Services map[string]struct {
				Build struct {
					Contexts map[string]string `yaml:"additional_contexts"`
				} `yaml:"build"`
			} `yaml:"services"`
		}
		if err := yaml.Unmarshal(raw, &overlay); err != nil {
			t.Fatal(err)
		}
		if got := overlay.Services[entry.service].Build.Contexts["licensed-fonts"]; got != "${NOISEDECK_LICENSED_FONTS_DIR:?Set the external licensed font directory}" {
			t.Fatalf("%s context = %q", entry.service, got)
		}
	}
}

func TestProductionOverlayIsCheckedInPolicy(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "compose.production.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	for _, required := range []string{
		"NOISEDECK_AUTH_MODE: oidc",
		"NOISEDECK_IMAGE_TAG:?",
		"NOISEDECK_PUBLISH_ADDRESS:?",
		"com.noisefloor.deck.capability-profile: production-v1",
		"ports: !override",
		"NOISEDECK_TMP_HOST_PATH:?",
		"create_host_path: false",
		"tmpfs: !override",
	} {
		if !strings.Contains(text, required) {
			t.Errorf("production overlay does not contain %q", required)
		}
	}
}

func TestProductionCapabilityBuildUsesBuildOrderedGoBase(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "compose.go-mempalace.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	for _, required := range []string{
		"go-base:",
		"target: mempalace",
		"VERSION: ${NOISEDECK_BUILD_VERSION:-${NOISEDECK_IMAGE_TAG:-local}}",
		"scale: 0",
		"deck-base: service:go-base",
	} {
		if !strings.Contains(text, required) {
			t.Errorf("production capability build does not contain %q", required)
		}
	}
}
