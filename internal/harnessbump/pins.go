package harnessbump

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Bump is one harness moving from one version to another, with the digest
// each of its artifacts carries at the new version.
type Bump struct {
	Harness Harness
	From    string
	To      string
	// Digests maps an artifact's SHAArg to the new hex digest.
	Digests map[string]string
	// PinnedOn is candidate metadata reused during deterministic reconstruction.
	// Empty records the UTC date of this application.
	PinnedOn string
}

var (
	argLine = regexp.MustCompile(`(?m)^ARG ([A-Z0-9_]+)=(\S+)$`)
	// A version is a bare token: no shell metacharacters, no whitespace,
	// nothing an ARG line or a test literal could misread.
	versionToken = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z.+\-]*$`)
	hexDigest    = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// ValidVersion reports whether a version string is safe to write into a
// pin.
func ValidVersion(v string) bool {
	return versionToken.MatchString(v)
}

// ReadPins returns every `ARG NAME=value` the Dockerfile declares.
func ReadPins(dockerfile []byte) map[string]string {
	pins := map[string]string{}
	for _, m := range argLine.FindAllSubmatch(dockerfile, -1) {
		name, value := string(m[1]), string(m[2])
		if _, seen := pins[name]; !seen {
			pins[name] = value
		}
	}
	return pins
}

// Pinned reads the harness's current version and artifact digests from the
// repository's Dockerfile.
func Pinned(root string, h Harness) (version string, digests map[string]string, err error) {
	raw, err := os.ReadFile(filepath.Clean(filepath.Join(root, "Dockerfile")))
	if err != nil {
		return "", nil, err
	}
	pins := ReadPins(raw)
	version, ok := pins[h.VersionArg]
	if !ok {
		return "", nil, fmt.Errorf("harnessbump: Dockerfile declares no ARG %s", h.VersionArg)
	}
	digests = map[string]string{}
	for _, a := range h.Artifacts {
		d, ok := pins[a.SHAArg]
		if !ok {
			return "", nil, fmt.Errorf("harnessbump: Dockerfile declares no ARG %s", a.SHAArg)
		}
		digests[a.SHAArg] = d
	}
	return version, digests, nil
}

// Apply rewrites the pin and every literal that repeats it. It returns the
// files it changed, relative to root. Every replacement is planned before
// any file is written, so an error leaves the tree untouched; the
// Dockerfile must carry the old pin exactly once, and a required repeat
// the tree does not spell is an error, never a guess.
func Apply(root string, b Bump) ([]string, error) {
	if !ValidVersion(b.To) {
		return nil, fmt.Errorf("harnessbump: %q is not a version", b.To)
	}
	if b.From == b.To {
		return nil, nil
	}
	for _, a := range b.Harness.Artifacts {
		d, ok := b.Digests[a.SHAArg]
		if !ok {
			return nil, fmt.Errorf("harnessbump: no digest for %s", a.SHAArg)
		}
		if !hexDigest.MatchString(d) {
			return nil, fmt.Errorf("harnessbump: %s digest %q is not sha256 hex", a.SHAArg, d)
		}
	}
	_, oldDigests, err := Pinned(root, b.Harness)
	if err != nil {
		return nil, err
	}
	// A pin declared twice, whatever the values, cannot be moved once: the
	// later declaration would be the one the build used.
	dockerfileRaw, err := os.ReadFile(filepath.Clean(filepath.Join(root, "Dockerfile")))
	if err != nil {
		return nil, err
	}
	names := []string{b.Harness.VersionArg}
	for _, a := range b.Harness.Artifacts {
		names = append(names, a.SHAArg)
	}
	for _, name := range names {
		if n := len(regexp.MustCompile(`(?m)^ARG `+regexp.QuoteMeta(name)+`=`).FindAll(dockerfileRaw, -1)); n != 1 {
			return nil, fmt.Errorf("harnessbump: Dockerfile declares ARG %s %d times, expected once", name, n)
		}
	}
	var shas [][2]string
	for _, a := range b.Harness.Artifacts {
		shas = append(shas, [2]string{"ARG " + a.SHAArg + "=" + oldDigests[a.SHAArg], "ARG " + a.SHAArg + "=" + b.Digests[a.SHAArg]})
	}

	var plan []*edit
	dockerfile, err := planEdit(root, "Dockerfile", append([][2]string{{"ARG " + b.Harness.VersionArg + "=" + b.From, "ARG " + b.Harness.VersionArg + "=" + b.To}}, shas...), 1)
	if err != nil {
		return nil, err
	}
	if dockerfile.matched == 0 {
		return nil, fmt.Errorf("harnessbump: Dockerfile does not declare ARG %s=%s", b.Harness.VersionArg, b.From)
	}
	plan = append(plan, dockerfile)

	for _, r := range b.Harness.Repeats {
		pairs := [][2]string{{fmt.Sprintf(r.Format, b.From), fmt.Sprintf(r.Format, b.To)}}
		// A package test that asserts the ARG line asserts the digests too.
		if strings.HasPrefix(r.Path, "deploy/") {
			pairs = append(pairs, shas...)
		}
		e, err := planEdit(root, r.Path, pairs, 0)
		if err != nil && (r.Required || !errors.Is(err, os.ErrNotExist)) {
			return nil, err
		}
		if e == nil || e.matched == 0 {
			if r.Required {
				return nil, fmt.Errorf("harnessbump: %s does not spell %s %s as %q", r.Path, b.Harness.Name, b.From, fmt.Sprintf(r.Format, b.From))
			}
			continue
		}
		plan = append(plan, e)
	}

	if b.Harness.Name == "claude" {
		more, err := planClaudeProfile(root, b, oldDigests)
		if err != nil {
			return nil, err
		}
		plan = append(plan, more...)
	}

	when := time.Now().UTC()
	if b.PinnedOn != "" {
		when, err = time.Parse(time.DateOnly, b.PinnedOn)
		if err != nil {
			return nil, fmt.Errorf("harnessbump: invalid pin date: %w", err)
		}
	}
	readme, err := planReadmePin(root, b, when)
	if err != nil {
		return nil, err
	}
	if readme != nil {
		plan = append(plan, readme)
	}

	var changed []string
	for _, e := range plan {
		if err := e.commit(); err != nil {
			return changed, err
		}
		changed = append(changed, e.path)
	}
	return changed, nil
}

// planClaudeProfile moves the supervisor's built-in provider profile, which
// binds Claude's tested version and executable digests: the runtime refuses
// an executable whose digest is not the profile's, so a Dockerfile bump
// without this is a launch that fails closed. The golden test pairs each
// canonical profile JSON with the digest the supervisor derives from it,
// sha256 over the canonical bytes, and is regolded from the moved JSON.
func planClaudeProfile(root string, b Bump, oldDigests map[string]string) ([]*edit, error) {
	amd64Old, amd64New := oldDigests["CLAUDE_BINARY_SHA256_AMD64"], b.Digests["CLAUDE_BINARY_SHA256_AMD64"]
	arm64Old, arm64New := oldDigests["CLAUDE_BINARY_SHA256_ARM64"], b.Digests["CLAUDE_BINARY_SHA256_ARM64"]
	profile, err := planEdit(root, "internal/supervisor/provider_profile.go", [][2]string{
		{`TestedVersion:             "` + b.From + `"`, `TestedVersion:             "` + b.To + `"`},
		{`"` + amd64Old + `"`, `"` + amd64New + `"`},
		{`"` + arm64Old + `"`, `"` + arm64New + `"`},
	}, 0)
	if err != nil {
		return nil, err
	}
	if profile.matched < 3 {
		return nil, fmt.Errorf("harnessbump: %s does not carry the pinned Claude version and both digests", profile.path)
	}
	test, err := planEdit(root, "internal/supervisor/provider_profile_test.go", [][2]string{
		{`"tested_version":"` + b.From + `"`, `"tested_version":"` + b.To + `"`},
		{`"executable_sha256":"` + amd64Old + `"`, `"executable_sha256":"` + amd64New + `"`},
		{`"executable_sha256":"` + arm64Old + `"`, `"executable_sha256":"` + arm64New + `"`},
	}, 0)
	if err != nil {
		return nil, err
	}
	if test.matched == 0 {
		return nil, fmt.Errorf("harnessbump: %s does not carry the pinned Claude golden", test.path)
	}
	regolded := goldenPair.ReplaceAllFunc(test.content, func(m []byte) []byte {
		sub := goldenPair.FindSubmatch(m)
		sum := sha256.Sum256(sub[2])
		return append(append(append(append(append([]byte{}, sub[1]...), sub[2]...), sub[3]...), []byte(hex.EncodeToString(sum[:]))...), sub[5]...)
	})
	if bytes.Equal(regolded, test.content) {
		return nil, fmt.Errorf("harnessbump: %s carries no wantJSON/wantDigest golden pair", test.path)
	}
	test.content = regolded
	return []*edit{profile, test}, nil
}

var goldenPair = regexp.MustCompile("(wantJSON = `)(\\{[^`]*\\})(`\\s*\\n\\s*wantDigest = \")([0-9a-f]{64})(\")")

// edit is one planned file rewrite: the content to write, and how many of
// the requested pairs it matched.
type edit struct {
	path    string
	abs     string
	mode    os.FileMode
	content []byte
	matched int
}

// planEdit reads the file and applies each old→new pair in memory. With
// exactly > 0, a pair that occurs more than that many times is an error: a
// pin declared twice cannot be moved once.
func planEdit(root, rel string, pairs [][2]string, exactly int) (*edit, error) {
	abs := filepath.Join(root, filepath.FromSlash(rel))
	raw, err := os.ReadFile(filepath.Clean(abs))
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return nil, err
	}
	body := string(raw)
	matched := 0
	for _, p := range pairs {
		count := strings.Count(body, p[0])
		if count == 0 {
			continue
		}
		if exactly > 0 && count != exactly {
			return nil, fmt.Errorf("harnessbump: %s declares %q %d times, expected %d", rel, p[0], count, exactly)
		}
		body = strings.ReplaceAll(body, p[0], p[1])
		matched++
	}
	return &edit{path: rel, abs: abs, mode: info.Mode().Perm(), content: []byte(body), matched: matched}, nil
}

func (e *edit) commit() error {
	return os.WriteFile(e.abs, e.content, e.mode)
}
