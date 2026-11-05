package deploy

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestPlainWrapperUsesOnlyFixedGatewayProfile(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("plain profile refuses root")
	}
	root := t.TempDir()
	state := filepath.Join(root, "state")
	for _, name := range []string{"", "data", "home", "projects", "tmp", "gh", "data/gateway"} {
		if err := os.MkdirAll(filepath.Join(state, name), 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(state, ".deck-plain-installation.json"), []byte(strings.Repeat("a", 64)+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(root, "bin")
	if err := os.Mkdir(bin, 0700); err != nil {
		t.Fatal(err)
	}
	trace := filepath.Join(root, "calls")
	// The fake engine records arguments only. The env file contains shell
	// syntax to prove the wrapper passes it as data rather than sourcing it.
	engine := "#!/bin/sh\nif test \"$1\" = info; then echo true; exit 0; fi\nprintf '%s\\n' \"$@\" > \"$TRACE\"\n"
	if err := os.WriteFile(filepath.Join(bin, "podman"), []byte(engine), 0700); err != nil {
		t.Fatal(err)
	}
	envFile := filepath.Join(root, "fixture.env")
	tripwire := filepath.Join(root, "sourced")
	if err := os.WriteFile(envFile, []byte("BAD=$(touch "+tripwire+")\n"), 0600); err != nil {
		t.Fatal(err)
	}
	env := []string{"PATH=" + bin + ":" + os.Getenv("PATH"), "HOME=" + root, "TRACE=" + trace, "COMPOSE_PROJECT_NAME=plain-test", "NOISEDECK_PLAIN_INSTANCE=plain-test", "NOISEDECK_PLAIN_CONTAINER=plain-test-app", "NOISEDECK_PLAIN_STATE_ROOT=" + state, "NOISEDECK_WORKSPACE_GATEWAY=true", "NOISEDECK_PUBLIC_ORIGIN=https://plain.example.test"}
	for _, action := range []string{"down", "restart", "report"} {
		cmd := exec.Command("sh", "plain-compose.sh", envFile, action)
		cmd.Env = env
		if cmd.Run() == nil {
			t.Fatalf("accepted action %s", action)
		}
	}
	cmd := exec.Command("sh", "plain-compose.sh", envFile, "up")
	cmd.Env = env
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("wrapper: %v %s", err, out)
	}
	b, err := os.ReadFile(trace)
	if err != nil {
		t.Fatal(err)
	}
	args := string(b)
	for _, want := range []string{"compose.workspace-plain.yaml", "compose.workspace-gateway.yaml", "--no-recreate\n--no-build\napp\n"} {
		if !strings.Contains(args, want) {
			t.Errorf("missing fixed option %s", want)
		}
	}
	if strings.Contains(args, "compose.sealed") {
		t.Fatal("plain start invoked encrypted profile")
	}
	if _, err = os.Stat(tripwire); !os.IsNotExist(err) {
		t.Fatal("Compose env file was sourced")
	}
	if err = os.WriteFile(filepath.Join(state, "gocryptfs.conf"), []byte("foreign metadata"), 0600); err != nil {
		t.Fatal(err)
	}
	cmd = exec.Command("sh", "plain-compose.sh", envFile, "up")
	cmd.Env = env
	if cmd.Run() == nil {
		t.Fatal("wrapper accepted encrypted metadata")
	}
}

func TestPlainOverlayHasNoEncryptedClaimOrPublishedPort(t *testing.T) {
	b, err := os.ReadFile("../compose.workspace-plain.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Volumes  yaml.Node
		Services map[string]struct {
			Ports       yaml.Node
			Volumes     yaml.Node
			Environment map[string]string
			UsernsMode  string `yaml:"userns_mode"`
		}
	}
	if err = yaml.Unmarshal(b, &cfg); err != nil {
		t.Fatal(err)
	}
	app := cfg.Services["app"]
	if app.Ports.Tag != "!reset" || len(app.Ports.Content) != 0 || app.Volumes.Tag != "!override" || cfg.Volumes.Tag != "!reset" {
		t.Fatal("plain overlay retained inherited volumes or ports")
	}
	if app.Environment["NOISEDECK_SEALED_REQUIRE_ENCRYPTED_STATE"] != "false" || app.Environment["NOISEDECK_SEALED_REPORT_PATH"] != "" {
		t.Fatal("plain overlay claims encrypted state")
	}
	if !strings.HasPrefix(app.UsernsMode, "keep-id:") {
		t.Fatal("plain profile lost owner UID mapping")
	}
	for _, node := range app.Volumes.Content {
		var mount struct {
			Source, Target, Type string
			ReadOnly             bool `yaml:"read_only"`
			Bind                 struct {
				CreateHostPath bool `yaml:"create_host_path"`
			}
		}
		if err = node.Decode(&mount); err != nil {
			t.Fatal(err)
		}
		if mount.Type != "bind" || mount.Bind.CreateHostPath {
			t.Fatal("unmanaged automatic volume creation")
		}
		if !mount.ReadOnly && !strings.HasPrefix(mount.Source, "${NOISEDECK_PLAIN_STATE_ROOT:") {
			t.Fatal("writable store outside plain root")
		}
	}
}

func TestRenderedPlainGatewayProfile(t *testing.T) {
	docker, err := exec.LookPath("docker")
	if err != nil {
		t.Skip("Compose renderer unavailable; real host qualification is separate")
	}
	env := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME"), "COMPOSE_PROJECT_NAME=plain-test", "NOISEDECK_PLAIN_INSTANCE=plain-test", "NOISEDECK_PLAIN_CONTAINER=plain-test-app", "NOISEDECK_PLAIN_STATE_ROOT=/fixture/plain", "NOISEDECK_MEMPALACE_HOST_PATH=/fixture/plain/home/.mempalace", "NOISEDECK_PUBLIC_ORIGIN=https://plain.example.test"}
	cmd := exec.Command(docker, "compose", "--file", "compose.yaml", "--file", "compose.mempalace.yaml", "--file", "compose.workspace-plain.yaml", "--file", "compose.workspace-gateway.yaml", "config")
	cmd.Dir = ".."
	cmd.Env = env
	b, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("render: %v %s", err, b)
	}
	var cfg struct {
		Services map[string]struct {
			Ports       []any
			Environment map[string]string
		}
	}
	if err = yaml.Unmarshal(b, &cfg); err != nil {
		t.Fatal(err)
	}
	for _, service := range cfg.Services {
		if len(service.Ports) != 0 {
			t.Fatal("published bypass port")
		}
	}
	app := cfg.Services["app"]
	for k, want := range map[string]string{"NOISEDECK_SEALED_REQUIRE_ENCRYPTED_STATE": "false", "NOISEDECK_SERVER_BIND": "", "NOISEDECK_SERVER_UNIX_SOCKET": "/data/gateway/backend.sock", "NOISEDECK_AUTH_MODE": "oidc", "NOISEDECK_PROJECT_PREVIEWS_BIND": ""} {
		if app.Environment[k] != want {
			t.Errorf("unexpected %s", k)
		}
	}
}
