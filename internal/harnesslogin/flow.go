// Package harnesslogin transports an explicitly submitted browser callback to
// a native CLI's registered login listener. The CLI owns OAuth and credentials.
package harnesslogin

import (
	"crypto/sha256"
	"errors"
	"net/url"
	"strconv"
	"strings"
)

const MaxURLBytes = 8192

// ChatGPT appends this to the state for some accounts; Codex strips it itself.
const lifeSciencesStateSuffix = ".onboarding_entrypoint=life_sciences"

var ErrUnavailable = errors.New("harness login is unavailable")
var ErrInvalid = errors.New("callback does not match this login attempt")
var ErrConsumed = errors.New("login handoff expired or was already used")

// Flow identifies a reviewed native callback/completion contract, not a model.
type Flow string

const (
	CodexBrowser Flow = "codex-browser"
	PiOpenAI     Flow = "pi-openai"
	PiAnthropic  Flow = "pi-anthropic"
)

type callback struct {
	flow      Flow
	authority string
	path      string
	state     [32]byte
}

// authorization accepts only native browser launches for declared flows. The
// Codex authorization URLs may be held in memory for its authenticated helper.
func authorization(provider, raw string) (Flow, callback, error) {
	u, err := strictURL(raw)
	if err != nil || u.Scheme != "https" {
		return "", callback{}, ErrInvalid
	}
	q, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return "", callback{}, ErrInvalid
	}
	for _, values := range q {
		if len(values) != 1 {
			return "", callback{}, ErrInvalid
		}
	}
	if q.Get("response_type") != "code" || q.Get("state") == "" || q.Get("code_challenge") == "" || q.Get("code_challenge_method") != "S256" || q.Has("access_token") {
		return "", callback{}, ErrInvalid
	}
	redirect, err := strictURL(q.Get("redirect_uri"))
	if err != nil || redirect.Scheme != "http" || redirect.RawQuery != "" {
		return "", callback{}, ErrInvalid
	}
	var flow Flow
	switch {
	case u.Host == "auth.openai.com" && u.Path == "/oauth/authorize" && redirect.Path == "/auth/callback":
		switch provider {
		case "codex":
			switch redirect.Host {
			case "localhost:1455", "localhost:1457", "127.0.0.1:1455", "127.0.0.1:1457":
			default:
				return "", callback{}, ErrUnavailable
			}
			flow = CodexBrowser
		case "pi":
			if redirect.Host != "localhost:1455" {
				return "", callback{}, ErrUnavailable
			}
			flow = PiOpenAI
		default:
			return "", callback{}, ErrUnavailable
		}
	case provider == "pi" && u.Host == "claude.ai" && u.Path == "/oauth/authorize" && redirect.Host == "localhost:53692" && redirect.Path == "/callback":
		flow = PiAnthropic
	default:
		return "", callback{}, ErrUnavailable
	}
	if q.Has("code") && (flow != PiAnthropic || q.Get("code") != "true") {
		return "", callback{}, ErrInvalid
	}
	return flow, callback{flow: flow, authority: redirect.Host, path: redirect.Path, state: sha256.Sum256([]byte(q.Get("state")))}, nil
}

func strictURL(raw string) (*url.URL, error) {
	if len(raw) == 0 || len(raw) > MaxURLBytes || strings.ContainsAny(raw, "\\\r\n\t ") {
		return nil, ErrInvalid
	}
	for _, c := range raw {
		if c < 32 || c > 126 {
			return nil, ErrInvalid
		}
	}
	u, err := url.Parse(raw)
	if err != nil || u.Opaque != "" || u.User != nil || u.Fragment != "" || strings.Contains(raw, "#") || u.Host == "" || u.RawPath != "" || u.ForceQuery {
		return nil, ErrInvalid
	}
	return u, nil
}

func (c callback) requestTarget(raw string) (string, error) {
	u, err := strictURL(raw)
	if err != nil || u.Scheme != "http" || u.Host != c.authority || u.Path != c.path {
		return "", ErrInvalid
	}
	q, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return "", ErrInvalid
	}
	for key, values := range q {
		if key != "code" && key != "state" && key != "scope" {
			return "", ErrInvalid
		}
		if len(values) != 1 || values[0] == "" || strings.ContainsFunc(values[0], func(r rune) bool { return r < 32 || r == 127 }) {
			return "", ErrInvalid
		}
	}
	state := q.Get("state")
	if sha256.Sum256([]byte(state)) != c.state {
		if c.flow != CodexBrowser && c.flow != PiOpenAI {
			return "", ErrInvalid
		}
		state = strings.TrimSuffix(state, lifeSciencesStateSuffix)
		if sha256.Sum256([]byte(state)) != c.state {
			return "", ErrInvalid
		}
		// Pi requires the exact state it issued. Codex handles this provider-added
		// suffix itself, so its native callback remains unchanged.
		if c.flow == PiOpenAI {
			q.Set("state", state)
		}
	}
	if q.Get("code") == "" {
		return "", ErrInvalid
	}
	return c.path + "?" + q.Encode(), nil
}

func (c callback) port() int {
	u, _ := url.Parse("http://" + c.authority)
	p, _ := strconv.Atoi(u.Port())
	return p
}
