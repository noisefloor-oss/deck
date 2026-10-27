package deploy

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The private palace never syncs and the relay palace is the only server
// a2a clients reach: bootstrap starts the private server with its sync loop
// disabled and no peer configuration, starts the relay server over a
// separate palace directory on 8766, and exports A2A_MCP_URL before the
// courier and serve start, so every a2a writer on the host lands in the
// relay and nothing else does.
func TestBootstrapSplitsPrivateAndRelayPalaces(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("mempalace", "noisedeck-bootstrap"))
	if err != nil {
		t.Fatalf("read bootstrap: %v", err)
	}
	script := string(raw)

	private := strings.Index(script, "MEMPALACE_SYNC_INTERVAL=0 /opt/mempalace/venv/bin/mempalace-mcp")
	relay := strings.Index(script, "MEMPALACE_MCP_HTTP_TOKEN=\"${NOISEDECK_RELAY_TOKEN:-}\"")
	if private < 0 || relay < 0 {
		t.Fatalf("bootstrap must start both palaces; private@%d relay@%d", private, relay)
	}
	privateBlock := script[strings.LastIndex(script[:private], "(\n") : strings.Index(script[private:], ") &")+private]
	if !strings.Contains(privateBlock, "MEMPALACE_SYNC_INTERVAL=0") {
		t.Error("the private palace server must run with its sync loop disabled")
	}
	if strings.Contains(privateBlock, "peers") {
		t.Error("the private palace server must not receive peer configuration")
	}
	relayBlock := script[strings.LastIndex(script[:relay], "(\n") : strings.Index(script[relay:], ") &")+relay]
	for _, want := range []string{"--port 8766", `--host "$relay_host"`, "NOISEDECK_RELAY_SYNC_INTERVAL:-2", "MEMPALACE_MCP_HTTP_TOKEN"} {
		if !strings.Contains(relayBlock, want) {
			t.Errorf("relay server block lacks %q", want)
		}
	}
	if !strings.Contains(privateBlock, "--port 8765") || strings.Contains(privateBlock, "8766") {
		t.Error("the private palace keeps port 8765 and only the relay uses 8766")
	}

	export := strings.Index(script, "export A2A_MCP_URL=http://127.0.0.1:8766/mcp")
	courier := strings.Index(script, "a2a courier")
	serve := strings.Index(script, "deck serve")
	if export < 0 || courier < 0 || serve < 0 || export > courier || export > serve {
		t.Errorf("A2A_MCP_URL must be exported before the courier and serve start: export@%d courier@%d serve@%d", export, courier, serve)
	}
}

// Nothing in the deployment writes a peers.json into the private palace,
// and nothing but the a2a client is pointed at the relay.
func TestOnlyA2AReachesTheRelayAndNothingPeersThePrivatePalace(t *testing.T) {
	for _, path := range []string{
		filepath.Join("mempalace", "configure.py"),
		filepath.Join("mempalace", "hook_forwarder.py"),
		filepath.Join("mempalace", "noisedeck-bootstrap"),
	} {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if strings.Contains(string(raw), "palace/peers.json") {
			t.Errorf("%s writes peers into the private palace", path)
		}
	}
	for _, path := range []string{
		filepath.Join("mempalace", "configure.py"),
		filepath.Join("mempalace", "hook_forwarder.py"),
	} {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if strings.Contains(string(raw), "8766") {
			t.Errorf("%s reaches the relay palace; only a2a may", path)
		}
		if !strings.Contains(string(raw), "127.0.0.1:8765") {
			t.Errorf("%s no longer targets the private palace", path)
		}
	}
	client, err := os.ReadFile(filepath.Join("a2a", "a2a.py"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(client), `os.environ.get("A2A_MCP_URL", "http://127.0.0.1:8766/mcp")`) {
		t.Error("the a2a client's default is not the relay palace")
	}
	if !strings.Contains(string(client), `headers["Authorization"] = f"Bearer {token}"`) {
		t.Error("the a2a client does not present A2A_MCP_TOKEN")
	}
}

// The relay settings reach the container through the Compose deployment,
// so a token set in the deployment env file is the token the relay
// requires.
func TestComposeForwardsRelaySettings(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "compose.mempalace.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var file composeFile
	if err := yaml.Unmarshal(raw, &file); err != nil {
		t.Fatal(err)
	}
	env := file.Services["app"].Environment
	if env["NOISEDECK_RELAY_TOKEN"] != "${NOISEDECK_RELAY_TOKEN:-}" {
		t.Errorf("NOISEDECK_RELAY_TOKEN = %q, want it interpolated from the deployment env", env["NOISEDECK_RELAY_TOKEN"])
	}
	if env["NOISEDECK_RELAY_SYNC_INTERVAL"] != "${NOISEDECK_RELAY_SYNC_INTERVAL:-2}" {
		t.Errorf("NOISEDECK_RELAY_SYNC_INTERVAL = %q", env["NOISEDECK_RELAY_SYNC_INTERVAL"])
	}
}

// The bearer travels to the relay server in its environment and never in
// any argv: the launch line is run for real with the server binary replaced
// by a probe that records its environment and its arguments.
func TestRelayTokenNeverEntersArgv(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("mempalace", "noisedeck-bootstrap"))
	if err != nil {
		t.Fatal(err)
	}
	script := string(raw)
	start := strings.Index(script, "MEMPALACE_SYNC_INTERVAL=\"${NOISEDECK_RELAY_SYNC_INTERVAL:-2}\"")
	if start < 0 {
		t.Fatal("relay launch line not found")
	}
	end := strings.Index(script[start:], ">>") + start
	launch := script[start:end]
	if strings.Contains(launch, "env ") {
		t.Fatalf("the relay launch uses env(1), which puts the token in argv: %s", launch)
	}
	dir := t.TempDir()
	probe := filepath.Join(dir, "probe.sh")
	if err := os.WriteFile(probe, []byte("#!/bin/sh\nprintf '%s\\n' \"$MEMPALACE_MCP_HTTP_TOKEN\" > \"$PROBE_OUT.env\"\nprintf '%s\\n' \"$@\" > \"$PROBE_OUT.argv\"\ntr '\\0' '\\n' < /proc/$$/cmdline > \"$PROBE_OUT.cmdline\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	launch = strings.ReplaceAll(launch, "/opt/mempalace/venv/bin/mempalace-mcp", probe)
	launch = strings.ReplaceAll(launch, "\\\n", " ")
	out := filepath.Join(dir, "probe")
	cmd := exec.Command("sh", "-c", launch)
	cmd.Env = append(os.Environ(), "NOISEDECK_RELAY_TOKEN=synthetic-relay-token", "PROBE_OUT="+out)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("launch line failed: %v: %s", err, output)
	}
	envToken, _ := os.ReadFile(out + ".env")
	if strings.TrimSpace(string(envToken)) != "synthetic-relay-token" {
		t.Fatalf("token did not reach the server's environment: %q", envToken)
	}
	for _, f := range []string{out + ".argv", out + ".cmdline"} {
		body, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("probe did not record %s: %v", filepath.Base(f), err)
		}
		if strings.Contains(string(body), "synthetic-relay-token") {
			t.Fatalf("token appeared in %s: %q", filepath.Base(f), body)
		}
	}
}

// The relay is published on the host loopback only, for the tailnet proxy
// that forwards /sync and /healthz to it; the port is yours to
// choose so two Decks on one host do not collide.
func TestRelayIsPublishedOnLoopbackOnly(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "compose.mempalace.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var file composeFile
	if err := yaml.Unmarshal(raw, &file); err != nil {
		t.Fatal(err)
	}
	ports := file.Services["app"].Ports
	if len(ports) != 1 || ports[0] != "127.0.0.1:${NOISEDECK_RELAY_HOST_PORT:-18766}:8766" {
		t.Fatalf("relay port publication = %v, want exactly one loopback mapping of 8766 with an overridable host port", ports)
	}
}

// The relay listens on the container network only when it has a token to
// require; without one it stays on loopback. Both cases run the real
// bind selection from the bootstrap.
func TestRelayBindsTheContainerNetworkOnlyWithAToken(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("mempalace", "noisedeck-bootstrap"))
	if err != nil {
		t.Fatal(err)
	}
	script := string(raw)
	start := strings.Index(script, "relay_host=127.0.0.1")
	end := strings.Index(script[start:], "\n(\n") + start
	if start < 0 || end <= start {
		t.Fatal("relay bind selection not found in bootstrap")
	}
	selection := script[start:end]
	for token, want := range map[string]string{"": "127.0.0.1", "synthetic-relay-token": "0.0.0.0"} {
		cmd := exec.Command("sh", "-c", selection+"\nprintf '%s' \"$relay_host\"")
		cmd.Env = append(os.Environ(), "NOISEDECK_RELAY_TOKEN="+token)
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("token %q: %v", token, err)
		}
		if string(out) != want {
			t.Errorf("token %q: relay_host = %q, want %q", token, out, want)
		}
	}
}

// Bootstrap records a pre-sealing host's history before any new traffic:
// key init when there is no key, then key migrate, idempotent, before the
// courier and serve start.
func TestBootstrapMigratesAnExistingKeysHistory(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("mempalace", "noisedeck-bootstrap"))
	if err != nil {
		t.Fatal(err)
	}
	script := string(raw)
	init := strings.Index(script, "a2a key init")
	migrate := strings.Index(script, "a2a key migrate")
	courier := strings.Index(script, "a2a courier")
	if init < 0 || migrate < 0 || courier < 0 || init >= migrate || migrate >= courier {
		t.Fatalf("bootstrap must run key init, then key migrate, before the courier: init@%d migrate@%d courier@%d", init, migrate, courier)
	}
	// The relay is still starting when migrate first runs; a relay that
	// did not answer is retried, never taken as empty.
	if !strings.Contains(script, "until /usr/bin/python3 /usr/local/bin/a2a key migrate") {
		t.Error("key migrate must be retried until the relay answers")
	}
	if !strings.Contains(script, "|| /usr/bin/python3 /usr/local/bin/a2a key init") {
		t.Error("key init, which takes the history record with the key, must be retried until the relay answers")
	}
	// A relay that never answers stops the start: a Deck that ran on
	// without the record could never take it safely afterwards.
	loop := script[migrate : strings.Index(script[migrate:], "done")+migrate]
	if !strings.Contains(loop, "exit 1") || strings.Contains(loop, "break") {
		t.Error("bootstrap must fail closed when the history cannot be recorded, not continue")
	}
}
