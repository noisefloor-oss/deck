package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/noisefloor-oss/deck/internal/harnessbump"
)

func TestListNamesEveryHarness(t *testing.T) {
	var out bytes.Buffer
	if err := run([]string{"list"}, &out, &out); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"claude\t", "codex\t", "devin\tDEVIN_VERSION\tdevin-manifest"} {
		if !strings.Contains(out.String(), name) {
			t.Errorf("list lacks %q:\n%s", name, out.String())
		}
	}
}

func TestSettleJSONPreservesErrorAndDecisions(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "Dockerfile"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "gh"), []byte("#!/bin/sh\necho 'API unavailable' >&2\nexit 7\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", root+":"+os.Getenv("PATH"))
	var out bytes.Buffer
	err := settle(t.Context(), []string{"-repo", "o/r", "-root", root, "-json"}, &out)
	if err == nil || !strings.Contains(err.Error(), "API unavailable") || out.String() != "[]\n" {
		t.Fatalf("output %q error %v", out.String(), err)
	}
}

func TestReviewRefsJSONKeys(t *testing.T) {
	root := t.TempDir()
	head, base := strings.Repeat("a", 40), strings.Repeat("b", 40)
	response := `{"data":{"repository":{"ref":{"target":{"oid":"` + base + `"}},"pullRequest":{"id":"PR1","state":"OPEN","headRefOid":"` + head + `","baseRefOid":"stale"}}}}`
	if err := os.WriteFile(filepath.Join(root, "gh"), []byte("#!/bin/sh\nprintf '%s\\n' '"+response+"'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", root+":"+os.Getenv("PATH"))
	var out bytes.Buffer
	if err := runContext(t.Context(), []string{"review-refs", "-repo", "o/r", "-pr", "1"}, &out, &out); err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 || got["head"] != head || got["base"] != base || got["repository"] != "o/r" || got["pull_request"] != float64(1) {
		t.Fatalf("unexpected wire keys: %s", out.Bytes())
	}
}

func TestUsageErrors(t *testing.T) {
	var out bytes.Buffer
	if err := run(nil, &out, &out); err == nil {
		t.Error("no command ran")
	}
	if err := run([]string{"merge"}, &out, &out); err == nil {
		t.Error("an unknown command ran")
	}
	if err := run([]string{"probe", "-harness", "codex"}, &out, &out); err == nil || !strings.Contains(err.Error(), "-image") {
		t.Errorf("probe without an image: %v", err)
	}
	if err := run([]string{"apply", "-harness", "nope", "-root", t.TempDir()}, &out, &out); err == nil {
		t.Error("an unknown harness applied")
	}
	if err := run([]string{"settle"}, &out, &out); err == nil || !strings.Contains(err.Error(), "-repo") {
		t.Errorf("settle without a repo: %v", err)
	}
	if err := run([]string{"verdict"}, &out, &out); err == nil || !strings.Contains(err.Error(), "-capture") {
		t.Errorf("verdict without a capture: %v", err)
	}
	t.Setenv("OPENAI_API_KEY", "")
	if err := run([]string{"broker", "-model", "m"}, &out, &out); err == nil || !strings.Contains(err.Error(), "OPENAI_API_KEY") {
		t.Errorf("broker without a key: %v", err)
	}
	t.Setenv("OPENAI_API_KEY", "sk-test")
	if err := run([]string{"broker", "-model", "m", "-upstream", "http://plain.example"}, &out, &out); err == nil || !strings.Contains(err.Error(), "https") {
		t.Errorf("broker with a plaintext upstream: %v", err)
	}
	if err := run([]string{"broker"}, &out, &out); err == nil || !strings.Contains(err.Error(), "-model") {
		t.Errorf("broker without a model: %v", err)
	}
	if err := run([]string{"jail-dial"}, &out, &out); err == nil || !strings.Contains(err.Error(), "-socket") {
		t.Errorf("jail-dial without a socket: %v", err)
	}
	if err := run([]string{"drop"}, &out, &out); err == nil || !strings.Contains(err.Error(), "-harness") {
		t.Errorf("drop without its flags: %v", err)
	}
}

func TestDropRewritesThePinAndTheReport(t *testing.T) {
	saved := harnessbump.Catalogue
	omp, err := harnessbump.Lookup("omp")
	if err != nil {
		t.Fatal(err)
	}
	harnessbump.Catalogue = []harnessbump.Harness{omp}
	t.Cleanup(func() { harnessbump.Catalogue = saved })
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
	write("Dockerfile", "FROM scratch\nARG OMP_VERSION=19.0.0\n")
	write("deploy/omp_cli_package_test.go", "package deploy\n// \"ARG OMP_VERSION=19.0.0\"\n")
	write("docs/provider-contracts/README.md", "| omp | 19.0.0 |\n")
	batchFile := filepath.Join(root, "batch.json")
	write("batch.json", `{"id":"`+strings.Repeat("c", 64)+`","statuses":[],"updated":["omp"],"revert":{"omp":{"version":"18.1.2"}}}`+"\n")
	var out bytes.Buffer
	if err := run([]string{"drop", "-harness", "omp", "-reason", "contract probe failed", "-batch", batchFile, "-root", root}, &out, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "dropped omp") || !strings.Contains(out.String(), "ships 0 update(s)") {
		t.Fatalf("drop report: %s", out.String())
	}
	dockerfile, err := os.ReadFile(filepath.Join(root, "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(dockerfile), "ARG OMP_VERSION=18.1.2") {
		t.Fatalf("pin not reverted: %s", dockerfile)
	}
	raw, err := os.ReadFile(batchFile)
	if err != nil {
		t.Fatal(err)
	}
	var result harnessbump.BatchResult
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Updated) != 0 || len(result.Dropped) != 1 || result.Dropped[0].Harness != "omp" {
		t.Fatalf("report after drop: %+v", result)
	}
	wantID, err := harnessbump.BatchID(harnessbump.ReadPins(dockerfile))
	if err != nil {
		t.Fatal(err)
	}
	if result.ID != wantID {
		t.Fatalf("report id %s, recomputed %s", result.ID, wantID)
	}
}

func TestBrokerWritesItsPortAndStopsOnCancel(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "sk-test")
	portFile := filepath.Join(t.TempDir(), "port")
	ctx, cancel := context.WithCancel(context.Background())
	var out bytes.Buffer
	done := make(chan error, 1)
	go func() {
		done <- runContext(ctx, []string{"broker", "-model", "m", "-listen", "127.0.0.1:0", "-port-file", portFile}, &out, &out)
	}()
	var port []byte
	for i := 0; i < 100; i++ {
		port, _ = os.ReadFile(portFile)
		if len(port) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if n, err := strconv.Atoi(strings.TrimSpace(string(port))); err != nil || n <= 0 {
		t.Fatalf("port file = %q", port)
	}
	resp, err := http.Get("http://127.0.0.1:" + strings.TrimSpace(string(port)) + "/admin")
	if err != nil || resp.StatusCode != http.StatusNotFound {
		t.Errorf("broker answered %v %v", resp, err)
	}
	if resp != nil {
		_ = resp.Body.Close()
	}
	cancel()
	if err := <-done; err != nil {
		t.Errorf("broker exit: %v", err)
	}
}

func TestBrokerListensOnAUnixSocket(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "sk-test")
	dir, err := os.MkdirTemp("/tmp", "hb")
	if err != nil {
		dir = t.TempDir()
	} else {
		t.Cleanup(func() { _ = os.RemoveAll(dir) })
	}
	socket := filepath.Join(dir, "b.sock")
	ctx, cancel := context.WithCancel(context.Background())
	var out bytes.Buffer
	done := make(chan error, 1)
	go func() {
		done <- runContext(ctx, []string{"broker", "-model", "m", "-listen", "unix:" + socket}, &out, &out)
	}()
	var info os.FileInfo
	for i := 0; i < 100; i++ {
		var err error
		if info, err = os.Stat(socket); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if info == nil || info.Mode()&os.ModeSocket == 0 || info.Mode().Perm() != 0o600 {
		t.Fatalf("socket = %v", info)
	}
	client := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}}
	resp, err := client.Get("http://broker/admin")
	if err != nil || resp.StatusCode != http.StatusNotFound {
		t.Errorf("over the socket: %v %v", resp, err)
	}
	if resp != nil {
		_ = resp.Body.Close()
	}
	cancel()
	if err := <-done; err != nil {
		t.Errorf("broker exit: %v", err)
	}
}
