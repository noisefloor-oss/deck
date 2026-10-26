// Package modelbump keeps the reviewer model pins honest: it reads the
// pinned reviewer models, asks each CLI which models exist now, maps every
// pin to its like-for-like successor (same vendor and tier family, newer
// version), and produces a plan that rewrites the pin store and every
// literal that repeats it. It decides nothing about shipping: the offer is
// a pull request a human merges, and a pin whose successor is ambiguous or
// undiscoverable produces no offer at all rather than a guess.
package modelbump

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// TruthPath is the pin store: the one file a model refresh rewrites whose
// values every other literal must repeat. It lives beside the review
// identity files because it is review configuration, not runtime state.
const TruthPath = ".noise/reviewer-models.json"

// Roles are the reviewer seats whose models the lane watches. The primary,
// secondary, and tertiary are the SDLC's interactive cross-model reviewers;
// the settle fallback is what the harness-bump settlement reviewer runs when
// the NOISE_REVIEW_MODEL repository variable is unset.
const (
	RolePrimary        = "primary"
	RoleSecondary      = "secondary"
	RoleTertiary       = "tertiary"
	RoleSettleFallback = "settle-fallback"
)

// Pin is one reviewer seat's model, by the harness that runs it.
type Pin struct {
	Harness string `json:"harness"`
	Model   string `json:"model"`
}

// Truth is the parsed pin store.
type Truth struct {
	Schema int            `json:"schema"`
	Pins   map[string]Pin `json:"pins"`
}

// Roles lists the watched seats in a stable order.
func Roles() []string {
	return []string{RolePrimary, RoleSecondary, RoleTertiary, RoleSettleFallback}
}

// ReadTruth loads the pin store from the repository root.
func ReadTruth(root string) (Truth, error) {
	raw, err := os.ReadFile(filepath.Clean(filepath.Join(root, TruthPath)))
	if err != nil {
		return Truth{}, fmt.Errorf("modelbump: reading %s: %w", TruthPath, err)
	}
	return ParseTruth(raw)
}

// ParseTruth validates captured pin-store bytes. Files parses the same
// bytes it hashes, so the rendered plan can never describe a pin store
// the publication hash did not see.
func ParseTruth(raw []byte) (Truth, error) {
	var t Truth
	if err := json.Unmarshal(raw, &t); err != nil {
		return Truth{}, fmt.Errorf("modelbump: parsing %s: %w", TruthPath, err)
	}
	if t.Schema != 1 {
		return Truth{}, fmt.Errorf("modelbump: %s schema %d is not 1", TruthPath, t.Schema)
	}
	for _, role := range Roles() {
		p, ok := t.Pins[role]
		if !ok {
			return Truth{}, fmt.Errorf("modelbump: %s declares no %s pin", TruthPath, role)
		}
		if p.Harness == "" || p.Model == "" {
			return Truth{}, fmt.Errorf("modelbump: %s pin %s is incomplete: %+v", TruthPath, role, p)
		}
	}
	return t, nil
}

// Marshal renders the pin store with a stable key order so successive
// offers produce reviewable diffs.
func (t Truth) Marshal() ([]byte, error) {
	ordered := map[string]Pin{}
	for _, role := range Roles() {
		p, ok := t.Pins[role]
		if !ok {
			return nil, fmt.Errorf("modelbump: truth is missing the %s pin", role)
		}
		ordered[role] = p
	}
	out := struct {
		Schema int            `json:"schema"`
		Pins   map[string]Pin `json:"pins"`
	}{Schema: 1, Pins: ordered}
	raw, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(raw, '\n'), nil
}

// modelSlug is one parsed model identifier.
type modelSlug struct {
	vendor  string
	tier    string
	version []int
}

var slugToken = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z._-]*$`)

// parseSlug splits a model slug into vendor, tier family, and numeric
// version. Kimi managed aliases put the version before the tier. Other
// supported slugs put the vendor first, followed by the first run of numeric
// tokens; tokens around that run name the tier. A slug with no numeric token
// is not a versioned family member and does not parse.
func parseSlug(slug string) (modelSlug, bool) {
	if strings.HasPrefix(slug, "kimi-code/") {
		return parseKimiSlug(strings.TrimPrefix(slug, "kimi-code/"))
	}
	if !slugToken.MatchString(slug) {
		return modelSlug{}, false
	}
	parts := strings.Split(slug, "-")
	if len(parts) < 2 {
		return modelSlug{}, false
	}
	vendor := parts[0]
	rest := parts[1:]
	start := 0
	for start < len(rest) && !numericToken(rest[start]) {
		start++
	}
	if start == len(rest) {
		return modelSlug{}, false
	}
	end := start
	for end < len(rest) && numericToken(rest[end]) {
		end++
	}
	var version []int
	for _, tok := range rest[start:end] {
		for _, piece := range strings.Split(tok, ".") {
			n, err := strconv.Atoi(piece)
			if err != nil {
				return modelSlug{}, false
			}
			version = append(version, n)
		}
	}
	tier := strings.Join(append(append([]string{}, rest[:start]...), rest[end:]...), "-")
	return modelSlug{vendor: vendor, tier: tier, version: version}, true
}

func parseKimiSlug(slug string) (modelSlug, bool) {
	parts := strings.Split(slug, "-")
	if len(parts) < 2 || !strings.HasPrefix(parts[0], "k") {
		return modelSlug{}, false
	}
	rawVersion := strings.TrimPrefix(parts[0], "k")
	tier := strings.Join(parts[1:], "-")
	if !numericToken(rawVersion) || tier == "" {
		return modelSlug{}, false
	}
	var version []int
	for _, piece := range strings.Split(rawVersion, ".") {
		n, err := strconv.Atoi(piece)
		if err != nil {
			return modelSlug{}, false
		}
		version = append(version, n)
	}
	return modelSlug{vendor: "kimi", tier: tier, version: version}, true
}

// numericToken reports whether a slug token is a version component.
func numericToken(tok string) bool {
	if tok == "" || tok[0] < '0' || tok[0] > '9' {
		return false
	}
	for _, r := range tok {
		if (r < '0' || r > '9') && r != '.' {
			return false
		}
	}
	return true
}

// compareVersion orders two numeric versions lexicographically by
// component, shorter-prefix first (6.1 < 6.1.1).
func compareVersion(a, b []int) int {
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] != b[i] {
			if a[i] < b[i] {
				return -1
			}
			return 1
		}
	}
	switch {
	case len(a) < len(b):
		return -1
	case len(a) > len(b):
		return 1
	}
	return 0
}

// sameFamily reports whether two slugs share vendor and tier.
func sameFamily(a, b modelSlug) bool {
	return a.vendor == b.vendor && a.tier == b.tier
}

// Successor picks the like-for-like successor of pinned within catalog:
// the newest member of pinned's own vendor and tier family, offered only
// when it is numerically STRICTLY newer than the pin. Equality by version
// (a differently spelled same version) offers nothing, and a catalog whose
// family maximum is older than the pin — a workspace CLI behind an
// already-accepted refresh — offers nothing rather than a downgrade. It
// returns an error when the slug does not name a versioned family member
// at all. A pinned model missing from the catalog still yields the family
// maximum when that maximum is newer: retirement is exactly when a
// refresh matters most.
func Successor(pinned string, catalog []string) (string, error) {
	pin, ok := parseSlug(pinned)
	if !ok {
		return "", fmt.Errorf("modelbump: %q is not a versioned model slug", pinned)
	}
	newest := ""
	var newestVersion []int
	for _, slug := range catalog {
		other, ok := parseSlug(slug)
		if !ok || !sameFamily(pin, other) {
			continue
		}
		if newest == "" || compareVersion(other.version, newestVersion) > 0 {
			newest, newestVersion = slug, other.version
		}
	}
	if newest == "" || compareVersion(newestVersion, pin.version) <= 0 {
		return "", nil
	}
	return newest, nil
}

// ClaudeCandidates lists the like-for-like successor candidates for a
// pinned claude-<tier>-<maj>-<min> slug, nearest first: the next minors,
// then the next major's earliest minor. The list is small on purpose: the
// discovery probe costs a real CLI invocation per candidate, so the lane
// probes the nearest two and stops.
func ClaudeCandidates(pinned string) ([]string, error) {
	pin, ok := parseSlug(pinned)
	if !ok || pin.vendor != "claude" || len(pin.version) == 0 {
		return nil, fmt.Errorf("modelbump: %q is not a versioned claude model slug", pinned)
	}
	tier := pin.tier
	if tier == "" {
		return nil, fmt.Errorf("modelbump: %q names no claude tier", pinned)
	}
	major, minor := pin.version[0], 0
	if len(pin.version) > 1 {
		minor = pin.version[1]
	}
	steps := [][2]int{{major, minor + 1}, {major + 1, 0}}
	out := make([]string, 0, len(steps))
	for _, step := range steps {
		out = append(out, fmt.Sprintf("claude-%s-%d-%d", tier, step[0], step[1]))
	}
	return out, nil
}
