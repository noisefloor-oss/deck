package deploy

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func relayBootstrap(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("mempalace/noisedeck-bootstrap")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestExternalRelayModeValidation(t *testing.T) {
	s := relayBootstrap(t)
	s = s[:strings.Index(s, "if ! /usr/local/bin/noisedeck-deployment-host-guard")]
	for _, tc := range []struct {
		mode, token string
		ok          bool
	}{
		{"", "", true}, {"local", "", true}, {"external", "synthetic", true},
		{"external", "", false}, {"typo", "synthetic", false},
	} {
		cmd := exec.Command("sh", "-c", s)
		cmd.Env = append(os.Environ(), "NOISEDECK_RELAY_MODE="+tc.mode, "NOISEDECK_RELAY_TOKEN="+tc.token)
		out, err := cmd.CombinedOutput()
		if (err == nil) != tc.ok {
			t.Fatalf("mode %q: %v %s", tc.mode, err, out)
		}
	}
}

func TestExternalRelayNeverStartsLocalWriter(t *testing.T) {
	s := relayBootstrap(t)
	start := strings.Index(s, "export MEMPALACE_CONFIG_DIR=")
	end := strings.Index(s, "export A2A_MCP_URL=")
	for _, mode := range []string{"local", "external"} {
		dir := t.TempDir()
		probe := filepath.Join(dir, "probe")
		if err := os.WriteFile(probe, []byte("#!/bin/sh\nfor argument do\n  if [ \"$argument\" = \"$MARKER_PALACE\" ]; then touch \"$MARKER\"; fi\ndone\n"), 0700); err != nil {
			t.Fatal(err)
		}
		migrationProbe := filepath.Join(dir, "migration-probe")
		if err := os.WriteFile(migrationProbe, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$MIGRATION_OUT\"\n"), 0700); err != nil {
			t.Fatal(err)
		}
		block := strings.ReplaceAll(s[start:end], "/home/agent/.mempalace", dir)
		block = strings.ReplaceAll(block, "/opt/mempalace/venv/bin/python /usr/local/lib/noisedeck/migrate-mempalace-to-sqlite.py", migrationProbe)
		block = strings.ReplaceAll(block, "/opt/mempalace/venv/bin/mempalace-mcp", probe)
		block = strings.ReplaceAll(block, "while true; do", "for once in 1; do")
		block = strings.ReplaceAll(block, "sleep 1", ":")
		migrationOutput := filepath.Join(dir, "migration")
		cmd := exec.Command("sh", "-c", "set -eu\n"+block+"\nwait")
		cmd.Env = append(os.Environ(), "NOISEDECK_RELAY_MODE="+mode, "MARKER="+filepath.Join(dir, "started"), "MARKER_PALACE="+filepath.Join(dir, "relay"), "MIGRATION_OUT="+migrationOutput)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s: %v %s", mode, err, out)
		}
		_, err := os.Stat(filepath.Join(dir, "started"))
		if (err == nil) != (mode == "local") {
			t.Fatalf("local writer in mode %s: %v", mode, err)
		}
		migrated, err := os.ReadFile(migrationOutput)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(migrated), filepath.Join(dir, "relay")) != (mode == "local") {
			t.Fatalf("relay migration in mode %s: %s", mode, migrated)
		}
		if mode == "external" {
			if _, err := os.Stat(filepath.Join(dir, "relay")); !os.IsNotExist(err) {
				t.Fatal("external mode touched relay storage")
			}
		}
	}
}

func TestExternalRelayAuthenticatedReadiness(t *testing.T) {
	s := relayBootstrap(t)
	start := strings.Index(s, "import json\n")
	end := strings.Index(s[start:], "\nPY") + start
	probe := strings.ReplaceAll(strings.ReplaceAll(s[start:end], "while time.monotonic() < deadline:", "for attempt in range(1):"), "time.sleep(1)", "time.sleep(0)")
	for _, status := range []int{200, 201, 302, 401, 503} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/sink" {
				t.Error("followed credential redirect")
				w.WriteHeader(500)
				return
			}
			if r.Header.Get("Authorization") == "" && status != 201 {
				w.WriteHeader(401)
				return
			}
			if status == 302 {
				w.Header().Set("Location", "/sink")
			}
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"result":{"serverInfo":{"version":"3.11.0"}}}`))
		}))
		cmd := exec.Command("python3", "-c", probe)
		cmd.Env = append(os.Environ(), "A2A_MCP_URL="+server.URL, "A2A_MCP_TOKEN=synthetic")
		out, err := cmd.CombinedOutput()
		server.Close()
		if (err == nil) != (status == 200) {
			t.Fatalf("status %d: %v %s", status, err, out)
		}
		if strings.Contains(string(out), "synthetic") {
			t.Fatal("bearer leaked")
		}
	}
}

func TestExternalRelayProductionContract(t *testing.T) {
	for _, damage := range []string{"", "missing tunnel", "missing bearer", "local writer", "healthy dependency", "wrong namespace", "wrong host key policy", "agent key access", "agent key file", "host valid", "host mode", "host symlink", "host nonroot"} {
		t.Run(damage, func(t *testing.T) {
			f := productionFixture()
			services := f["services"].(map[string]any)
			app := services["app"].(map[string]any)
			env := app["environment"].(map[string]any)
			env["NOISEDECK_RELAY_MODE"] = "external"
			env["NOISEDECK_RELAY_TOKEN"] = "synthetic"
			b, err := os.ReadFile("../compose.relay-tunnel.yaml")
			if err != nil {
				t.Fatal(err)
			}
			var overlay map[string]any
			if err := yaml.Unmarshal(b, &overlay); err != nil {
				t.Fatal(err)
			}
			relay := overlay["services"].(map[string]any)["relay-tunnel"].(map[string]any)
			if relay["image"] != "deck:${NOISEDECK_IMAGE_TAG:?set the explicit production image tag}" {
				t.Fatal("tunnel does not use app tag")
			}
			relay["image"] = app["image"]
			relay["volumes"] = []any{map[string]any{"type": "bind", "source": "/private/relay-ssh", "target": "/run/relay", "read_only": true}, sidecarResolverFixture()}
			services["relay-tunnel"] = relay
			switch damage {
			case "missing tunnel":
				delete(services, "relay-tunnel")
			case "missing bearer":
				delete(env, "NOISEDECK_RELAY_TOKEN")
			case "local writer":
				env["NOISEDECK_RELAY_MODE"] = "local"
			case "healthy dependency":
				relay["depends_on"] = map[string]any{"app": map[string]any{"condition": "service_healthy"}}
			case "wrong namespace":
				relay["network_mode"] = "host"
			case "wrong host key policy":
				relay["command"] = []any{"-NT", "-oStrictHostKeyChecking=no", "relay"}
			case "agent key access":
				app["volumes"] = append(app["volumes"].([]any), map[string]any{"type": "bind", "source": "/private", "target": "/keys"})
			case "agent key file":
				app["volumes"] = append(app["volumes"].([]any), map[string]any{"type": "bind", "source": "/private/relay-ssh/id_ed25519", "target": "/key"})
			}
			var out string
			if strings.HasPrefix(damage, "host ") {
				resolver := filepath.Join(t.TempDir(), "resolv.conf")
				if err := os.WriteFile(resolver, []byte("nameserver 127.0.0.11\n"), 0644); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(resolver, 0644); err != nil {
					t.Fatal(err)
				}
				services["chromium"].(map[string]any)["volumes"].([]any)[0].(map[string]any)["source"] = resolver
				relay["volumes"].([]any)[1].(map[string]any)["source"] = resolver
				dir := t.TempDir()
				if err := os.Chmod(dir, 0700); err != nil {
					t.Fatal(err)
				}
				for _, name := range []string{"config", "known_hosts", "id_ed25519"} {
					if err := os.WriteFile(filepath.Join(dir, name), []byte("synthetic"), 0600); err != nil {
						t.Fatal(err)
					}
				}
				relay["volumes"].([]any)[0].(map[string]any)["source"] = dir
				if damage == "host mode" {
					if err := os.Chmod(filepath.Join(dir, "id_ed25519"), 0644); err != nil {
						t.Fatal(err)
					}
				}
				if damage == "host symlink" {
					if err := os.Remove(filepath.Join(dir, "config")); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink("known_hosts", filepath.Join(dir, "config")); err != nil {
						t.Fatal(err)
					}
				}
				payload, e := json.Marshal(f)
				if e != nil {
					t.Fatal(e)
				}
				code := `import os,pathlib,runpy,sys
from unittest.mock import patch
original_stat, original_exists = pathlib.Path.stat, pathlib.Path.exists
root = pathlib.Path(sys.argv[1])
def stat(path,*a,**kw):
    result = original_stat(path,*a,**kw)
    if path == root or root in path.parents:
        values = list(result); values[4] = 12345 if sys.argv[2] == 'host nonroot' else 0
        return os.stat_result(values)
    return result
def exists(path):
    return False if str(path) == '/sys/fs/selinux/enforce' else original_exists(path)
with patch.object(pathlib.Path,'stat',stat), patch.object(pathlib.Path,'exists',exists):
    sys.argv.append('--check-host-files')
    runpy.run_path('verify-production-compose.py',run_name='__main__')
`
				cmd := exec.Command("python3", "-c", code, dir, damage)
				cmd.Stdin = bytes.NewReader(payload)
				var raw []byte
				raw, err = cmd.CombinedOutput()
				out = string(raw)
			} else {
				out, err = verifyProductionFixture(t, f)
			}
			if (err == nil) != (damage == "" || damage == "host valid") {
				t.Fatalf("%s: %v %s", damage, err, out)
			}
		})
	}
}

func TestExternalRelayPinsMatchDeck(t *testing.T) {
	deck, err := os.ReadFile("../Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	relay, err := os.ReadFile("mempalace/relay.Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"MEMPALACE_VERSION", "MEMPALACE_COMMIT", "UV_VERSION"} {
		re := regexp.MustCompile(`(?m)^ARG ` + key + `=([^\n]+)$`)
		want, got := re.FindSubmatch(deck), re.FindSubmatch(relay)
		if len(want) != 2 || len(got) != 2 || string(want[1]) != string(got[1]) {
			t.Fatalf("standalone relay %s differs from Deck", key)
		}
		if key == "MEMPALACE_VERSION" {
			for _, path := range []string{"mempalace/noisedeck-bootstrap", "production-compose.sh"} {
				b, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(string(b), `== "`+string(want[1])+`"`) {
					t.Fatalf("%s readiness version differs from Deck", path)
				}
			}
		}
	}
}

func TestExternalRelayRolloutLifecycle(t *testing.T) {
	b, err := os.ReadFile("production-compose.sh")
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	block := s[strings.Index(s, "\napply_images() {"):strings.Index(s, "\npreflight() {")]
	for _, tc := range []struct {
		opt, existing string
		ok            bool
	}{{"/private/ssh", "old", true}, {"", "old", false}, {"", "", true}} {
		dir := t.TempDir()
		rendered := filepath.Join(dir, "rendered.json")
		if err := os.WriteFile(rendered, []byte(`{"name":"test"}`), 0600); err != nil {
			t.Fatal(err)
		}
		stub := `
compose() { printf 'ROLLOUT:%s\n' "$*"; }
docker() {
  case "$*" in
    *service=relay-tunnel) printf '%s' "$EXISTING" ;;
    *service=preview-tunnel) ;;
    *) return 91 ;;
  esac
}
`
		cmd := exec.Command("sh", "-c", "set -eu\n"+stub+block+"\napply_images")
		cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "rendered=" + rendered, "EXISTING=" + tc.existing, "NOISEDECK_RELAY_SSH_DIR=" + tc.opt}
		out, err := cmd.CombinedOutput()
		if (err == nil) != tc.ok {
			t.Fatalf("opt=%q existing=%q: %v %s", tc.opt, tc.existing, err, out)
		}
		if !tc.ok && strings.Contains(string(out), "ROLLOUT:") {
			t.Fatal("mutated before refusal")
		}
		if tc.opt != "" && !strings.Contains(string(out), "app chromium relay-tunnel") {
			t.Fatalf("tunnel not recreated: %s", out)
		}
	}
}

func TestStandaloneRelayMigratesSelectedPalaceAndPreservesArgs(t *testing.T) {
	raw, err := os.ReadFile("mempalace/relay-entrypoint.sh")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	probe := filepath.Join(dir, "probe")
	if err := os.WriteFile(probe, []byte("#!/bin/sh\nprintf 'call' >> \"$PROBE_OUT\"\nprintf ' <%s>' \"$@\" >> \"$PROBE_OUT\"\nprintf '\\n' >> \"$PROBE_OUT\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	script := strings.ReplaceAll(string(raw), "/opt/mempalace/venv/bin/python /usr/local/lib/migrate-mempalace-to-sqlite.py", probe)
	script = strings.ReplaceAll(script, "/opt/mempalace/venv/bin/mempalace-mcp", probe)
	output := filepath.Join(dir, "calls")
	cmd := exec.Command("sh", "-c", script, "relay", "--palace", filepath.Join(dir, "custom palace"), "--transport", "http", "--port", "8766")
	cmd.Env = append(os.Environ(), "MEMPALACE_MCP_HTTP_TOKEN=synthetic", "MEMPALACE_CONFIG_DIR="+dir, "PROBE_OUT="+output)
	if body, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("relay entrypoint: %v: %s", err, body)
	}
	calls, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	palace := filepath.Join(dir, "custom palace")
	for _, want := range []string{
		"call <--config> <" + filepath.Join(dir, "config.json") + "> <--palace> <" + palace + ">",
		"call <--palace> <" + palace + "> <--transport> <http> <--port> <8766>",
	} {
		if !strings.Contains(string(calls), want) {
			t.Errorf("relay calls lack %q:\n%s", want, calls)
		}
	}
}
