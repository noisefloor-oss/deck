package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const publicReleases = "https://api.github.com/repos/noisefloor-oss/deck/releases/latest"

var stableVersion = regexp.MustCompile(`^v?(0|[1-9][0-9]{0,8})\.(0|[1-9][0-9]{0,8})\.(0|[1-9][0-9]{0,8})$`)

func releaseVersion(raw string) ([3]int, bool) {
	var v [3]int
	m := stableVersion.FindStringSubmatch(raw)
	if m == nil {
		return v, false
	}
	for i := range v {
		v[i], _ = strconv.Atoi(m[i+1])
	}
	return v, true
}

// ReleaseNotice checks only public release metadata. No operator credential,
// installation identifier or workspace data is sent to GitHub. Development,
// prerelease and commit builds never make this request.
type ReleaseNotice struct {
	installed string
	client    *http.Client
	now       func() time.Time
	mu        sync.Mutex
	next      time.Time
	available string
}

func NewReleaseNotice(installed string) *ReleaseNotice {
	return &ReleaseNotice{installed: installed, now: time.Now, client: &http.Client{
		Timeout:       5 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

func (n *ReleaseNotice) latest() string {
	current, ok := releaseVersion(n.installed)
	if !ok {
		return ""
	}
	// Keep one bounded request in flight across concurrent browser tabs.
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.now().Before(n.next) {
		return n.available
	}
	// Fail quietly and retry later, including rate limits and offline installs.
	n.next = n.now().Add(15 * time.Minute)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, publicReleases, nil)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "Noisefloor-Deck-release-check")
	resp, err := n.client.Do(req)
	if err != nil {
		return n.available
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return n.available
	}
	var release struct {
		Tag        string `json:"tag_name"`
		Draft      bool   `json:"draft"`
		Prerelease bool   `json:"prerelease"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&release) != nil || release.Draft || release.Prerelease {
		return n.available
	}
	target, valid := releaseVersion(release.Tag)
	if !valid {
		return n.available
	}
	n.available = ""
	for i := range current {
		if target[i] > current[i] {
			n.available = "v" + strings.TrimPrefix(release.Tag, "v")
			break
		}
		if target[i] < current[i] {
			break
		}
	}
	n.next = n.now().Add(6 * time.Hour)
	return n.available
}

func (n *ReleaseNotice) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	tag := n.latest()
	url := ""
	if tag != "" {
		url = "https://github.com/noisefloor-oss/deck/releases/tag/" + tag
	}
	_ = json.NewEncoder(w).Encode(struct {
		Version string `json:"version"`
		URL     string `json:"release_url"`
	}{tag, url})
}
