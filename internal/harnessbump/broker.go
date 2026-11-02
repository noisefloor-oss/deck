package harnessbump

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// BrokerPath is the one operation the broker forwards: the Responses call
// Codex makes for a turn. Every other method or path is refused, so the
// jail cannot use the broker as a deputy for any other credential-backed
// operation.
const BrokerPath = "/v1/responses"

// MaxBrokerRequestBytes bounds one forwarded request body.
const MaxBrokerRequestBytes = 8 << 20

// BrokerPolicy is the request shape the broker admits: the one model the
// reviewer was dispatched with, no server-side persistence, a bounded
// output, and a bounded number of calls for the broker's lifetime. A jail
// child that reads the broker's address gains exactly this and nothing
// wider.
type BrokerPolicy struct {
	Model           string
	MaxOutputTokens int
	MaxRequests     int64
}

// DefaultBrokerPolicy bounds a review: Codex makes one call per turn
// and a long review is a few hundred turns.
func DefaultBrokerPolicy(model string) BrokerPolicy {
	return BrokerPolicy{Model: model, MaxOutputTokens: 32768, MaxRequests: 1500}
}

// The Responses request Codex sends for a turn, measured against the
// pinned CLI: these top-level fields and no other. A field outside this
// set is a capability the reviewer does not need, so the body is refused.
var admittedFields = map[string]bool{
	"model": true, "input": true, "instructions": true, "tools": true, "tool_choice": true,
	"parallel_tool_calls": true, "reasoning": true, "store": true, "stream": true, "include": true,
	"text": true, "prompt_cache_key": true, "client_metadata": true, "max_output_tokens": true,
	"truncation": true, "safety_identifier": true, "user": true,
}

// admittedToolTypes are tools the client executes; the API executes none
// of them. A hosted tool (web search, file search, code interpreter, MCP,
// image generation, computer use) would be egress and cost under the real
// key, chosen by whatever runs in the jail. Each type's keys are the ones
// Codex writes.
var admittedToolTypes = map[string]map[string]bool{
	"function":    {"type": true, "name": true, "description": true, "parameters": true, "strict": true},
	"custom":      {"type": true, "name": true, "description": true, "format": true},
	"local_shell": {"type": true},
	"apply_patch": {"type": true},
	// A namespace groups function tools; its members are held to the
	// same rule.
	"namespace": {"type": true, "name": true, "description": true, "tools": true},
}

// admittedIncludes are the response parts Codex asks back for.
var admittedIncludes = map[string]bool{"reasoning.encrypted_content": true}

// admittedItemTypes are the input items Codex sends across turns, each
// with the keys Codex writes: its messages, its own tool calls and their
// outputs, and its reasoning. A reference to a stored item is not among
// them, and a key outside the set is refused, so the shape cannot widen
// as the upstream schema grows.
var admittedItemTypes = map[string]map[string]bool{
	"message":                 {"type": true, "role": true, "content": true, "id": true, "status": true},
	"function_call":           {"type": true, "name": true, "arguments": true, "call_id": true, "id": true, "status": true},
	"function_call_output":    {"type": true, "call_id": true, "output": true, "id": true, "status": true},
	"custom_tool_call":        {"type": true, "name": true, "input": true, "call_id": true, "id": true, "status": true},
	"custom_tool_call_output": {"type": true, "call_id": true, "output": true, "id": true, "status": true},
	"local_shell_call":        {"type": true, "id": true, "call_id": true, "action": true, "status": true},
	"local_shell_call_output": {"type": true, "id": true, "output": true, "status": true},
	"reasoning":               {"type": true, "id": true, "summary": true, "encrypted_content": true, "content": true, "status": true},
}

// admittedPartTypes are the content parts a message may carry: text, with
// its text. A file, an image or a URL would make the API fetch under the
// real key.
var admittedPartTypes = map[string]map[string]bool{
	"input_text":     {"type": true, "text": true},
	"output_text":    {"type": true, "text": true, "annotations": true, "logprobs": true},
	"summary_text":   {"type": true, "text": true},
	"reasoning_text": {"type": true, "text": true},
}

// admittedShellAction are the keys of a local shell call's action.
var admittedShellAction = map[string]bool{"type": true, "command": true, "timeout_ms": true, "working_directory": true, "env": true, "user": true}

// BrokerStats is what the broker records about its lifetime, written to
// a file on the host after every request so a gate can prove the jail's
// model reached it with an admitted request and the broker reached the
// API: positive evidence, not the absence of an error.
type BrokerStats struct {
	Admitted           int64 `json:"admitted"`
	Refused            int64 `json:"refused"`
	Forwarded          int64 `json:"forwarded"`
	UpstreamUnreached  int64 `json:"upstream_unreached"`
	LastUpstreamStatus int   `json:"last_upstream_status"`
	// LastRefusal is why the last refused body was refused: the reason
	// names a field or a shape, never content.
	LastRefusal string `json:"last_refusal,omitempty"`
}

type statsFile struct {
	mu    sync.Mutex
	path  string
	stats BrokerStats
}

func (f *statsFile) update(fn func(*BrokerStats)) {
	if f == nil {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(&f.stats)
	if f.path == "" {
		return
	}
	raw, err := json.Marshal(f.stats)
	if err != nil {
		return
	}
	_ = os.WriteFile(f.path, raw, 0o600)
}

// NewBroker returns the credential broker the reviewer's jail talks to. It
// is not a proxy: every outbound request is built here from the admitted
// body alone, with the key and a content type and nothing the jail sent —
// no header, no query, no cookie — so the jail selects nothing about the
// account the call is billed to or the shape the API sees. The model
// process inside the jail holds a dummy key and reaches only this; the
// real key lives in the host process and is never written, logged or
// echoed.
func NewBroker(upstream *url.URL, key string, policy BrokerPolicy) http.Handler {
	return NewBrokerWithStats(upstream, key, policy, "")
}

// NewBrokerWithStats is NewBroker recording its stats to statsPath.
func NewBrokerWithStats(upstream *url.URL, key string, policy BrokerPolicy, statsPath string) http.Handler {
	target := upstream.Scheme + "://" + upstream.Host + BrokerPath
	client := &http.Client{Timeout: 10 * time.Minute}
	stats := &statsFile{path: statsPath}
	var calls atomic.Int64
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != BrokerPath {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "broker: POST only", http.StatusMethodNotAllowed)
			return
		}
		raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxBrokerRequestBytes))
		if err != nil {
			http.Error(w, "broker: request too large", http.StatusRequestEntityTooLarge)
			return
		}
		body, reason := admit(raw, policy)
		if reason != "" {
			stats.update(func(s *BrokerStats) { s.Refused++; s.LastRefusal = reason })
			http.Error(w, "broker: "+reason, http.StatusForbidden)
			return
		}
		stats.update(func(s *BrokerStats) { s.Admitted++ })
		if calls.Add(1) > policy.MaxRequests {
			http.Error(w, "broker: the round's request budget is spent", http.StatusTooManyRequests)
			return
		}
		out, err := http.NewRequestWithContext(r.Context(), http.MethodPost, target, bytes.NewReader(body))
		if err != nil {
			http.Error(w, "broker: upstream unavailable", http.StatusBadGateway)
			return
		}
		out.Header.Set("Authorization", "Bearer "+key)
		out.Header.Set("Content-Type", "application/json")
		out.Header.Set("Accept", "text/event-stream, application/json")
		resp, err := client.Do(out)
		if err != nil {
			// The upstream error is not relayed: it could quote the request.
			stats.update(func(s *BrokerStats) { s.UpstreamUnreached++ })
			http.Error(w, "broker: upstream unavailable", http.StatusBadGateway)
			return
		}
		defer func() { _ = resp.Body.Close() }()
		stats.update(func(s *BrokerStats) { s.Forwarded++; s.LastUpstreamStatus = resp.StatusCode })
		if ct := resp.Header.Get("Content-Type"); ct != "" {
			w.Header().Set("Content-Type", ct)
		}
		w.WriteHeader(resp.StatusCode)
		relay(w, resp.Body)
	})
}

// relay streams the upstream body through, flushing each chunk so a
// streamed response arrives as it is produced.
func relay(w http.ResponseWriter, body io.Reader) {
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 32<<10)
	for {
		n, err := body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err != nil {
			return
		}
	}
}

// admit returns the body to forward and "", or nil and why the body is
// refused. The forwarded body is reconstructed, not relayed: the
// security-critical fields are set by the policy (store false, the
// output bound) whatever the request said, and every other field must be
// one Codex sends, in the form Codex sends it, down to the keys of every
// nested object.
func admit(raw []byte, policy BrokerPolicy) ([]byte, string) {
	// A duplicate key anywhere would let one value pass the check while
	// another reaches the API: the checks read the last value, a parser
	// upstream may not. No object in the body may repeat a key.
	if duplicateKeys(raw) {
		return nil, "body repeats a key"
	}
	var req map[string]json.RawMessage
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, "body is not a Responses request"
	}
	for field := range req {
		if !admittedFields[field] {
			return nil, "a field is not part of the reviewer's request shape"
		}
	}
	var model string
	if err := json.Unmarshal(req["model"], &model); err != nil || model != policy.Model {
		return nil, "model is not the one this round was dispatched with"
	}
	if raw, ok := req["store"]; ok {
		var store bool
		if err := json.Unmarshal(raw, &store); err != nil || store {
			return nil, "server-side persistence is not admitted"
		}
	}
	if raw, ok := req["max_output_tokens"]; ok {
		var n int
		if err := json.Unmarshal(raw, &n); err != nil || n <= 0 || n > policy.MaxOutputTokens {
			return nil, "max_output_tokens exceeds the round's bound"
		}
	}
	if raw, ok := req["include"]; ok {
		var includes []string
		if err := json.Unmarshal(raw, &includes); err != nil {
			return nil, "include is malformed"
		}
		for _, inc := range includes {
			if !admittedIncludes[inc] {
				return nil, "an include value is not admitted"
			}
		}
	}
	for _, field := range []string{"instructions", "prompt_cache_key", "safety_identifier", "user", "truncation", "tool_choice"} {
		if raw, ok := req[field]; ok {
			var text string
			if err := json.Unmarshal(raw, &text); err != nil {
				return nil, field + " is not text"
			}
		}
	}
	if raw, ok := req["tools"]; ok {
		if reason := admitTools(raw); reason != "" {
			return nil, reason
		}
	}
	if raw, ok := req["input"]; ok {
		if reason := admitInput(raw); reason != "" {
			return nil, reason
		}
	}
	if raw, ok := req["reasoning"]; ok {
		if reason := admitKeys("reasoning", raw, map[string]bool{"effort": true, "summary": true, "context": true, "generate_summary": true}); reason != "" {
			return nil, reason
		}
	}
	if raw, ok := req["text"]; ok {
		if reason := admitKeys("text", raw, map[string]bool{"verbosity": true, "format": true}); reason != "" {
			return nil, reason
		}
	}
	if raw, ok := req["client_metadata"]; ok {
		var meta map[string]string
		if err := json.Unmarshal(raw, &meta); err != nil {
			return nil, "client_metadata is not a map of text"
		}
	}
	// The reconstruction: the policy's values for the fields that decide
	// what the API keeps and how much it may say.
	req["store"] = json.RawMessage("false")
	if _, ok := req["max_output_tokens"]; !ok {
		req["max_output_tokens"] = json.RawMessage(strconv.Itoa(policy.MaxOutputTokens))
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, "body could not be rebuilt"
	}
	return body, ""
}

// admitKeys checks an object carries only the keys named.
func admitKeys(field string, raw json.RawMessage, allowed map[string]bool) string {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return field + " is not an object"
	}
	for key := range obj {
		if !allowed[key] {
			return field + " has a key Codex does not write"
		}
	}
	return ""
}

func admitTools(raw json.RawMessage) string {
	var tools []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &tools); err != nil {
		return "tools is malformed"
	}
	for _, tool := range tools {
		var kind string
		_ = json.Unmarshal(tool["type"], &kind)
		keys, ok := admittedToolTypes[kind]
		if !ok {
			return "a tool type is executed by the API, not the client, and is not admitted"
		}
		for key := range tool {
			if !keys[key] {
				return "a tool key is not one Codex writes"
			}
		}
		if kind == "namespace" {
			members, ok := tool["tools"]
			if !ok {
				return "namespace without tools"
			}
			var inner []map[string]json.RawMessage
			if err := json.Unmarshal(members, &inner); err != nil {
				return "namespace tools is malformed"
			}
			for _, member := range inner {
				var memberKind string
				_ = json.Unmarshal(member["type"], &memberKind)
				if memberKind != "function" {
					return "a namespace member type is not a function"
				}
				for key := range member {
					if !admittedToolTypes["function"][key] {
						return "a namespace member key is not one Codex writes"
					}
				}
			}
		}
	}
	return ""
}

// admitInput checks the input is text, or the items Codex sends, each
// with only the keys Codex writes and text-only content wherever content
// can be carried.
func admitInput(raw []byte) string {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return ""
	}
	var items []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return "input is neither text nor a list of items"
	}
	for _, item := range items {
		var kind string
		_ = json.Unmarshal(item["type"], &kind)
		if kind == "" {
			// A bare message: role and content, no type.
			if _, ok := item["role"]; ok {
				kind = "message"
			}
		}
		keys, ok := admittedItemTypes[kind]
		if !ok {
			return "an input item type is not one Codex sends"
		}
		for key := range item {
			if !keys[key] {
				return "an input item key is not one Codex writes"
			}
		}
		// Every field that can carry content is text or text parts: a
		// message's content, a tool call's arguments, a tool output. The
		// API accepts a file or an image URL in a tool output too, and would
		// fetch it under the key.
		for _, field := range []string{"content", "output", "arguments", "input", "encrypted_content", "summary"} {
			raw, ok := item[field]
			if !ok {
				continue
			}
			if reason := admitText(field, raw); reason != "" {
				return reason
			}
		}
		if raw, ok := item["action"]; ok {
			if reason := admitKeys("action", raw, admittedShellAction); reason != "" {
				return reason
			}
		}
		if kind == "message" {
			if _, ok := item["content"]; !ok {
				return "message without content"
			}
		}
	}
	return ""
}

// admitText accepts a string or a list of text parts carrying only the
// keys Codex writes.
func admitText(field string, raw json.RawMessage) string {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return ""
	}
	var parts []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &parts); err != nil {
		return field + " is neither text nor text parts"
	}
	for _, part := range parts {
		var kind string
		_ = json.Unmarshal(part["type"], &kind)
		keys, ok := admittedPartTypes[kind]
		if !ok {
			return field + " has a part type that is not text"
		}
		for key, raw := range part {
			if !keys[key] {
				return field + " has a part key Codex does not write"
			}
			// Codex echoes an assistant part back with empty annotations
			// and logprobs; a non-empty one could cite a file. Only the
			// empty list is text.
			if (key == "annotations" || key == "logprobs") && strings.TrimSpace(string(raw)) != "[]" {
				return field + " part " + key + " is not empty"
			}
		}
		var text string
		if raw, ok := part["text"]; !ok || json.Unmarshal(raw, &text) != nil {
			return field + " part has no text"
		}
	}
	return ""
}

// BrokerContext is the context a serving broker runs under; exported for
// the command's lifetime handling.
type BrokerContext = context.Context

// duplicateKeys reports whether any object in the JSON document repeats
// a key. It walks the token stream, so nesting depth does not matter.
func duplicateKeys(raw []byte) bool {
	dec := json.NewDecoder(bytes.NewReader(raw))
	type frame struct {
		object bool
		keys   map[string]bool
		expect bool // in an object: the next token is a key
	}
	var stack []*frame
	for {
		tok, err := dec.Token()
		if err != nil {
			return err != io.EOF
		}
		switch v := tok.(type) {
		case json.Delim:
			switch v {
			case '{':
				stack = append(stack, &frame{object: true, keys: map[string]bool{}, expect: true})
			case '[':
				stack = append(stack, &frame{})
			case '}', ']':
				stack = stack[:len(stack)-1]
				if len(stack) > 0 && stack[len(stack)-1].object {
					stack[len(stack)-1].expect = true
				}
			}
			continue
		}
		if len(stack) == 0 {
			continue
		}
		top := stack[len(stack)-1]
		if !top.object {
			continue
		}
		if top.expect {
			key, _ := tok.(string)
			if top.keys[key] {
				return true
			}
			top.keys[key] = true
			top.expect = false
		} else {
			top.expect = true
		}
	}
}
