package harnessbump

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBrokerPutsTheKeyOnForwardedRequestsAndNothingElse(t *testing.T) {
	var seenAuth, seenCookie, seenPath, seenBody, seenOrg, seenQuery string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAuth, seenCookie, seenPath = r.Header.Get("Authorization"), r.Header.Get("Cookie"), r.URL.Path
		seenOrg, seenQuery = r.Header.Get("OpenAI-Organization"), r.URL.RawQuery
		b, _ := io.ReadAll(r.Body)
		seenBody = string(b)
		w.Header().Set("Set-Cookie", "upstream=1")
		_, _ = io.WriteString(w, "ok")
	}))
	defer upstream.Close()
	u, _ := url.Parse(upstream.URL)
	policy := BrokerPolicy{Model: "gpt-review", MaxOutputTokens: 1000, MaxRequests: 3}
	broker := httptest.NewServer(NewBroker(u, "sk-real-key-000000000000000000", policy))
	defer broker.Close()

	req, _ := http.NewRequest(http.MethodPost, broker.URL+"/v1/responses?organization=other&x=1", strings.NewReader(`{"model":"gpt-review","input":"hi"}`))
	req.Header.Set("Authorization", "Bearer jail")
	req.Header.Set("Cookie", "session=stolen")
	req.Header.Set("OpenAI-Organization", "org-someone-else")
	req.Header.Set("OpenAI-Project", "proj-someone-else")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || seenPath != "/v1/responses" {
		t.Fatalf("status %d path %q", resp.StatusCode, seenPath)
	}
	if seenAuth != "Bearer sk-real-key-000000000000000000" || seenCookie != "" || seenOrg != "" || seenQuery != "" {
		t.Errorf("upstream saw auth %q cookie %q org %q query %q; the jail selects none of these", seenAuth, seenCookie, seenOrg, seenQuery)
	}
	if resp.Header.Get("Set-Cookie") != "" {
		t.Error("an upstream cookie reached the jail")
	}
	// The forwarded body is the policy's reconstruction: a minimal request
	// leaves with store false and the output bound set.
	var forwarded map[string]any
	if err := json.Unmarshal([]byte(seenBody), &forwarded); err != nil || forwarded["store"] != false || forwarded["max_output_tokens"] != float64(1000) || forwarded["input"] != "hi" {
		t.Errorf("forwarded body = %s (%v)", seenBody, err)
	}

	for _, tc := range []struct {
		method, path string
		want         int
	}{
		{http.MethodGet, "/admin", http.StatusNotFound},
		{http.MethodPost, "/v1/files", http.StatusNotFound},
		{http.MethodPost, "/v1/responses/", http.StatusNotFound},
		{http.MethodGet, "/v1/responses", http.StatusMethodNotAllowed},
		{http.MethodDelete, "/v1/responses", http.StatusMethodNotAllowed},
	} {
		seenPath = ""
		req, _ := http.NewRequest(tc.method, broker.URL+tc.path, strings.NewReader(`{"model":"gpt-review"}`))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != tc.want || seenPath != "" {
			t.Errorf("%s %s: %d (upstream saw %q), want %d and nothing upstream", tc.method, tc.path, resp.StatusCode, seenPath, tc.want)
		}
	}

	// The request shape is the policy's: any other model, persistence, a
	// persisted continuation, an oversized output bound, or a body that is
	// not a Responses request is refused before the key is touched.
	for name, body := range map[string]string{
		"image input":                `{"model":"gpt-review","input":[{"role":"user","content":[{"type":"input_image","image_url":"https://x/y.png"}]}]}`,
		"file input":                 `{"model":"gpt-review","input":[{"type":"message","role":"user","content":[{"type":"input_file","file_id":"file_1"}]}]}`,
		"stored item":                `{"model":"gpt-review","input":[{"type":"item_reference","id":"msg_1"}]}`,
		"hosted call":                `{"model":"gpt-review","input":[{"type":"web_search_call","id":"ws_1"}]}`,
		"input object":               `{"model":"gpt-review","input":{"x":1}}`,
		"image in a tool output":     `{"model":"gpt-review","input":[{"type":"function_call_output","call_id":"c1","output":[{"type":"input_image","image_url":"https://x/y.png"}]}]}`,
		"file url in a tool output":  `{"model":"gpt-review","input":[{"type":"custom_tool_call_output","call_id":"c1","output":[{"type":"input_file","file_url":"https://x/y.pdf"}]}]}`,
		"file in a shell output":     `{"model":"gpt-review","input":[{"type":"local_shell_call_output","id":"s1","output":[{"type":"input_file","file_id":"file_1"}]}]}`,
		"object tool output":         `{"model":"gpt-review","input":[{"type":"function_call_output","call_id":"c1","output":{"image_url":"https://x"}}]}`,
		"extra nested key":           `{"model":"gpt-review","input":[{"type":"function_call_output","call_id":"c1","output":"x","file_url":"https://x"}]}`,
		"extra part key":             `{"model":"gpt-review","input":[{"role":"user","content":[{"type":"input_text","text":"x","image_url":"https://x"}]}]}`,
		"extra tool key":             `{"model":"gpt-review","tools":[{"type":"function","name":"f","server_url":"https://x"}]}`,
		"hosted tool in a namespace": `{"model":"gpt-review","tools":[{"type":"namespace","name":"n","tools":[{"type":"web_search"}]}]}`,
		"codex's own web search":     `{"model":"gpt-review","tools":[{"type":"web_search","external_web_access":true}]}`,
		"extra reasoning key":        `{"model":"gpt-review","reasoning":{"effort":"low","mcp":true}}`,
		"object tool_choice":         `{"model":"gpt-review","tool_choice":{"type":"web_search"}}`,
		"file citation":              `{"model":"gpt-review","input":[{"role":"assistant","content":[{"type":"output_text","text":"x","annotations":[{"type":"file_citation","file_id":"file_1"}]}]}]}`,
		"logprobs":                   `{"model":"gpt-review","input":[{"role":"assistant","content":[{"type":"output_text","text":"x","logprobs":[{"token":"x"}]}]}]}`,
		"part without text":          `{"model":"gpt-review","input":[{"role":"user","content":[{"type":"input_text"}]}]}`,
		"image in call arguments":    `{"model":"gpt-review","input":[{"type":"function_call","call_id":"c1","name":"f","arguments":[{"type":"input_image","image_url":"https://x"}]}]}`,
		"another model":              `{"model":"gpt-5-max","input":"x"}`,
		"persistence":                `{"model":"gpt-review","store":true}`,
		"continuation":               `{"model":"gpt-review","previous_response_id":"resp_1"}`,
		"unbounded output":           `{"model":"gpt-review","max_output_tokens":100000}`,
		"null output bound":          `{"model":"gpt-review","max_output_tokens":null}`,
		"text output bound":          `{"model":"gpt-review","max_output_tokens":"1000"}`,
		"negative output bound":      `{"model":"gpt-review","max_output_tokens":-1}`,
		"zero output bound":          `{"model":"gpt-review","max_output_tokens":0}`,
		"not json":                   `model=gpt-review`,
		"no model":                   `{"input":"x"}`,
		"hosted web search":          `{"model":"gpt-review","tools":[{"type":"web_search"}]}`,
		"remote mcp":                 `{"model":"gpt-review","tools":[{"type":"function","name":"shell"},{"type":"mcp","server_url":"https://x"}]}`,
		"code interpreter":           `{"model":"gpt-review","tools":[{"type":"code_interpreter"}]}`,
		"service tier":               `{"model":"gpt-review","service_tier":"priority"}`,
		"background":                 `{"model":"gpt-review","background":true}`,
		"hosted prompt":              `{"model":"gpt-review","prompt":{"id":"pmpt_1"}}`,
		"conversation":               `{"model":"gpt-review","conversation":"conv_1"}`,
		"metadata":                   `{"model":"gpt-review","metadata":{"k":"v"}}`,
		"other include":              `{"model":"gpt-review","include":["file_search_call.results"]}`,
	} {
		seenPath = ""
		resp, err := http.Post(broker.URL+"/v1/responses", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden || seenPath != "" {
			t.Errorf("%s: %d (upstream saw %q)", name, resp.StatusCode, seenPath)
		}
	}
	// Codex's own shape, measured against the pinned CLI, is admitted.
	codex := `{"model":"gpt-review","instructions":"x","input":[{"role":"user","content":[{"type":"input_text","text":"hi"}]},{"role":"assistant","content":[{"type":"output_text","text":"ok","annotations":[],"logprobs":[]}]},{"type":"function_call","name":"shell","arguments":"{}","call_id":"c1"},{"type":"function_call_output","call_id":"c1","output":"ok"},{"type":"reasoning","encrypted_content":"e"}],"tools":[{"type":"function","name":"shell","parameters":{},"description":"d","strict":false},{"type":"namespace","name":"fs","description":"d","tools":[{"type":"function","name":"read","parameters":{},"description":"d","strict":false}]},{"type":"local_shell"}],"tool_choice":"auto","parallel_tool_calls":false,"reasoning":{"effort":"low","context":"all_turns"},"store":false,"stream":true,"include":["reasoning.encrypted_content"],"text":{"verbosity":"low"},"prompt_cache_key":"k","client_metadata":{"a":"b"},"max_output_tokens":1000}`
	// The budget: three admitted calls, then 429 with nothing upstream.
	for i := 0; i < 2; i++ {
		body := codex
		if i == 1 {
			body = `{"model":"gpt-review","max_output_tokens":1}`
		}
		resp, _ := http.Post(broker.URL+"/v1/responses", "application/json", strings.NewReader(body))
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("admitted call %d: %d", i+2, resp.StatusCode)
		}
	}
	var lowerForwarded map[string]any
	if err := json.Unmarshal([]byte(seenBody), &lowerForwarded); err != nil || lowerForwarded["max_output_tokens"] != float64(1) {
		t.Fatalf("lower bound was replaced: %s (%v)", seenBody, err)
	}

	seenPath = ""
	resp, _ = http.Post(broker.URL+"/v1/responses", "application/json", strings.NewReader(`{"model":"gpt-review"}`))
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests || seenPath != "" {
		t.Errorf("over budget: %d (upstream saw %q)", resp.StatusCode, seenPath)
	}
	big, _ := http.Post(broker.URL+"/v1/responses", "application/json", strings.NewReader(`{"model":"gpt-review","input":"`+strings.Repeat("x", MaxBrokerRequestBytes+10)+`"}`))
	_ = big.Body.Close()
	if big.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("oversized body: %d", big.StatusCode)
	}

	// An unreachable upstream yields a bare 502 that quotes nothing.
	dead, _ := url.Parse("http://127.0.0.1:1")
	deadBroker := httptest.NewServer(NewBroker(dead, "sk-real-key-000000000000000000", policy))
	defer deadBroker.Close()
	resp, err = http.Post(deadBroker.URL+"/v1/responses", "application/json", strings.NewReader(`{"model":"gpt-review"}`))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway || strings.Contains(string(body), "sk-real") {
		t.Errorf("dead upstream: %d %q", resp.StatusCode, body)
	}
}

func TestParseVerdictAcceptsOnlyAWellFormedCredentialFreeVerdict(t *testing.T) {
	v, err := ParseVerdict([]byte("APPROVE\n✅ VERDICT: APPROVE\n\n🟢 fine\n"))
	if err != nil || v.Event != "APPROVE" || !strings.HasPrefix(v.Body, "✅ VERDICT: APPROVE") {
		t.Fatalf("v = %+v, err %v", v, err)
	}
	if _, err := ParseVerdict([]byte("REQUEST_CHANGES\n❌ VERDICT: REQUEST_CHANGES\n🔴 1. no\n")); err != nil {
		t.Error(err)
	}
	if _, err := ParseVerdict([]byte("COMMENT\njust a note\n")); err != nil {
		t.Error(err)
	}
	for name, capture := range map[string]string{
		"unknown event":          "MERGE\n✅ VERDICT: APPROVE\n",
		"mismatched opening":     "APPROVE\n❌ VERDICT: REQUEST_CHANGES\n",
		"no opening":             "APPROVE\nlooks good\n",
		"comment with a verdict": "COMMENT\n✅ VERDICT: APPROVE\n",
		"empty body":             "APPROVE\n",
		"no body":                "APPROVE",
		"pem":                    "APPROVE\n✅ VERDICT: APPROVE\n-----BEGIN RSA PRIVATE KEY-----\nabc\n",
		"github token":           "APPROVE\n✅ VERDICT: APPROVE\nsee ghp_abcdefghijklmnopqrstuvwxyz0123\n",
		"api key":                "APPROVE\n✅ VERDICT: APPROVE\nsk-abcdefghijklmnopqrstuvwxyz0123\n",
		"jwt":                    "APPROVE\n✅ VERDICT: APPROVE\neyJabcdefghijklmnopqrstuvwx.eyJabcdefghijklmnopqrstuvwx.sig\n",
		"oversized":              "APPROVE\n✅ VERDICT: APPROVE\n" + strings.Repeat("x", MaxVerdictBytes+100),
	} {
		if _, err := ParseVerdict([]byte(capture)); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

func TestJailDialForwardsToTheBrokersSocket(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "ok") }))
	defer upstream.Close()
	u, _ := url.Parse(upstream.URL)
	socket := filepath.Join(shortDir(t), "b.sock")
	unixLn, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: NewBroker(u, "sk-real-key-000000000000000000", BrokerPolicy{Model: "m", MaxOutputTokens: 10, MaxRequests: 10})}
	go func() { _ = srv.Serve(unixLn) }()
	defer func() { _ = srv.Close() }()

	tcpLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = JailDial(ctx, tcpLn, socket) }()
	resp, err := http.Post("http://"+tcpLn.Addr().String()+"/v1/responses", "application/json", strings.NewReader(`{"model":"m"}`))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "ok" {
		t.Errorf("through the dialer: %d %q", resp.StatusCode, body)
	}
	resp, _ = http.Get("http://" + tcpLn.Addr().String() + "/admin")
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("the dialer reaches only the broker: %d", resp.StatusCode)
	}
}

func TestBrokerRecordsPositiveEvidence(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusUnauthorized) }))
	defer upstream.Close()
	u, _ := url.Parse(upstream.URL)
	statsPath := filepath.Join(t.TempDir(), "stats.json")
	broker := httptest.NewServer(NewBrokerWithStats(u, "sk-canary", BrokerPolicy{Model: "m", MaxOutputTokens: 10, MaxRequests: 10}, statsPath))
	defer broker.Close()
	resp, _ := http.Post(broker.URL+"/v1/responses", "application/json", strings.NewReader(`{"model":"other"}`))
	_ = resp.Body.Close()
	resp, _ = http.Post(broker.URL+"/v1/responses", "application/json", strings.NewReader(`{"model":"m","input":"hi"}`))
	_ = resp.Body.Close()
	raw, err := os.ReadFile(statsPath)
	if err != nil {
		t.Fatal(err)
	}
	var stats BrokerStats
	if err := json.Unmarshal(raw, &stats); err != nil {
		t.Fatal(err)
	}
	if stats.Admitted != 1 || stats.Refused != 1 || stats.Forwarded != 1 || stats.LastUpstreamStatus != http.StatusUnauthorized {
		t.Errorf("stats = %+v", stats)
	}
}

func TestRefusalReasonsNeverEchoTheBody(t *testing.T) {
	const marker = "HOSTILE-MARKER"
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("a refused request reached the upstream")
	}))
	defer upstream.Close()
	u, _ := url.Parse(upstream.URL)
	for name, body := range map[string]string{
		"field":                 `{"model":"m","` + marker + `":1}`,
		"include":               `{"model":"m","include":["` + marker + `"]}`,
		"reasoning key":         `{"model":"m","reasoning":{"` + marker + `":1}}`,
		"tool type":             `{"model":"m","tools":[{"type":"` + marker + `"}]}`,
		"tool key":              `{"model":"m","tools":[{"type":"function","` + marker + `":1}]}`,
		"namespace member type": `{"model":"m","tools":[{"type":"namespace","name":"n","description":"d","tools":[{"type":"` + marker + `"}]}]}`,
		"namespace member key":  `{"model":"m","tools":[{"type":"namespace","name":"n","description":"d","tools":[{"type":"function","` + marker + `":1}]}]}`,
		"input item type":       `{"model":"m","input":[{"type":"` + marker + `"}]}`,
		"input item key":        `{"model":"m","input":[{"role":"user","content":"x","` + marker + `":1}]}`,
		"part type":             `{"model":"m","input":[{"role":"user","content":[{"type":"` + marker + `"}]}]}`,
		"part key":              `{"model":"m","input":[{"role":"user","content":[{"type":"input_text","text":"x","` + marker + `":1}]}]}`,
	} {
		statsPath := filepath.Join(t.TempDir(), "stats.json")
		broker := httptest.NewServer(NewBrokerWithStats(u, "sk-canary", BrokerPolicy{Model: "m", MaxOutputTokens: 10, MaxRequests: 10}, statsPath))
		resp, err := http.Post(broker.URL+"/v1/responses", "application/json", strings.NewReader(body))
		if err != nil {
			broker.Close()
			t.Fatalf("%s: %v", name, err)
		}
		got, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		broker.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("%s: status %d, want 403", name, resp.StatusCode)
		}
		if strings.Contains(string(got), marker) {
			t.Errorf("%s: response %q echoes the body", name, got)
		}
		raw, err := os.ReadFile(statsPath)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var stats BrokerStats
		if err := json.Unmarshal(raw, &stats); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if stats.LastRefusal == "" {
			t.Errorf("%s: no refusal recorded", name)
		}
		if strings.Contains(stats.LastRefusal, marker) {
			t.Errorf("%s: LastRefusal %q echoes the body", name, stats.LastRefusal)
		}
	}
}

func TestReadCaptureRefusesWhatAJailCouldPlant(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good")
	_ = os.WriteFile(good, []byte("APPROVE\n✅ VERDICT: APPROVE\n"), 0o600)
	if raw, err := ReadCapture(good); err != nil || !strings.HasPrefix(string(raw), "APPROVE") {
		t.Errorf("good capture: %q %v", raw, err)
	}
	link := filepath.Join(dir, "link")
	_ = os.Symlink(good, link)
	if _, err := ReadCapture(link); err == nil {
		t.Error("a symlink was followed")
	}
	sparse := filepath.Join(dir, "sparse")
	f, _ := os.Create(sparse)
	_ = f.Truncate(int64(MaxVerdictBytes) * 100)
	_ = f.Close()
	if _, err := ReadCapture(sparse); err == nil {
		t.Error("a huge sparse file was read")
	}
	if _, err := ReadCapture(dir); err == nil {
		t.Error("a directory was read")
	}
}

func TestDuplicateKeysAreSeenAtAnyDepth(t *testing.T) {
	for raw, want := range map[string]bool{
		`{"a":1,"b":{"c":2,"c":3}}`:         true,
		`{"a":[{"x":1},{"x":2}]}`:           false,
		`{"a":{"x":1},"b":{"x":2}}`:         false,
		`{"a":1,"a":2}`:                     true,
		`[{"k":1,"k":2}]`:                   true,
		`{"a":[[{"k":1}],[{"k":2,"k":3}]]}`: true,
		`{"a":"x","b":[1,2,3],"c":null}`:    false,
	} {
		if got := duplicateKeys([]byte(raw)); got != want {
			t.Errorf("%s: duplicateKeys = %v, want %v", raw, got, want)
		}
	}
}

// shortDir is a scratch directory short enough for a unix socket path,
// which the kernel bounds at 108 bytes; the harness's TMPDIR is longer.
func shortDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "hb")
	if err != nil {
		return t.TempDir()
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}
