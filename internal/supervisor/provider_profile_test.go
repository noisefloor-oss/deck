package supervisor

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/noisefloor-oss/deck/internal/provider"
)

func TestClaudeProviderProfileGolden(t *testing.T) {
	revision, err := CurrentProviderProfile(provider.Claude)
	if err != nil {
		t.Fatalf("CurrentProviderProfile: %v", err)
	}
	var wantJSON, wantDigest string
	switch runtime.GOARCH {
	case "amd64":
		wantJSON = `{"schema":"noisedeck.provider-profile.v1","provider":"claude","descriptor_revision":"claude-assigned-identity.v1","tested_version":"2.1.311","runtime_relative_executable":"usr/local/bin/claude","executable_sha256":"86dc1fcc69498159eaf15f446613e847010f9036bfbbdffd4c49350be8489ada"}`
		wantDigest = "1126771cfa3c468ede8b1929bd30c2742a8fc302faeb11df1b93b30e5b10f5b7"
	case "arm64":
		wantJSON = `{"schema":"noisedeck.provider-profile.v1","provider":"claude","descriptor_revision":"claude-assigned-identity.v1","tested_version":"2.1.311","runtime_relative_executable":"usr/local/bin/claude","executable_sha256":"2e2496c9a8065df93acb0fca2d341f59a6b8777649b2e025ab8c9bcd5eef61f3"}`
		wantDigest = "5ac6b479e6306bf38ce3909b0c490a33d10001b24a6ac566887c99812685c32b"
	default:
		t.Fatalf("unsupported test architecture %s", runtime.GOARCH)
	}
	if revision.CanonicalJSON != wantJSON {
		t.Fatalf("canonical JSON = %s, want %s", revision.CanonicalJSON, wantJSON)
	}
	if revision.Digest != wantDigest {
		t.Fatalf("digest = %s, want %s", revision.Digest, wantDigest)
	}
}

func TestResolveProviderProfileRequiresExactAuthority(t *testing.T) {
	revision, err := CurrentProviderProfile(provider.Claude)
	if err != nil {
		t.Fatalf("CurrentProviderProfile: %v", err)
	}

	resolved, err := ResolveProviderProfile(
		revision.Digest, revision.Digest,
		revision.Profile.ExecutableSHA256)
	if err != nil {
		t.Fatalf("ResolveProviderProfile: %v", err)
	}
	if resolved.Digest != revision.Digest {
		t.Fatalf("resolved digest = %s, want %s", resolved.Digest, revision.Digest)
	}
	if resolved.Profile.Provider != provider.Claude {
		t.Fatalf("resolved provider = %s, want %s", resolved.Profile.Provider, provider.Claude)
	}

	tests := []struct {
		name     string
		bound    string
		profile  string
		artifact string
		want     error
	}{
		{"policy revision disagreement", revision.Digest, strings.Repeat("0", 64), revision.Profile.ExecutableSHA256, ErrProviderProfilePolicy},
		{"unknown profile", strings.Repeat("0", 64), strings.Repeat("0", 64), revision.Profile.ExecutableSHA256, ErrProviderProfileDigest},
		{"uppercase profile", revision.Digest, strings.ToUpper(revision.Digest), revision.Profile.ExecutableSHA256, ErrProviderProfileDigest},
		{"malformed profile", revision.Digest, "sha256:wrong", revision.Profile.ExecutableSHA256, ErrProviderProfileDigest},
		{"artifact mismatch", revision.Digest, revision.Digest, strings.Repeat("0", 64), ErrProviderProfileArtifact},
		{"malformed artifact", revision.Digest, revision.Digest, "/host/claude", ErrProviderProfileArtifact},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := ResolveProviderProfile(
				test.bound, test.profile, test.artifact)
			if !errors.Is(err, test.want) {
				t.Fatalf("error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestCurrentProviderProfileRejectsUnsupportedProvider(t *testing.T) {
	_, err := CurrentProviderProfile(provider.Codex)
	if !errors.Is(err, ErrUnsupportedProviderProfile) {
		t.Fatalf("error = %v, want ErrUnsupportedProviderProfile", err)
	}
	if strings.Contains(err.Error(), string(provider.Codex)) {
		t.Fatalf("error echoes caller input: %v", err)
	}
}

func TestProviderProfileRejectsCallerControlledLaunchFields(t *testing.T) {
	base, err := builtInProviderProfile(provider.Claude)
	if err != nil {
		t.Fatalf("builtInProviderProfile: %v", err)
	}
	tests := []struct {
		name   string
		mutate func(*ProviderProfileV1)
	}{
		{"absolute host path", func(p *ProviderProfileV1) { p.RuntimeRelativeExecutable = "/usr/local/bin/claude" }},
		{"parent traversal", func(p *ProviderProfileV1) { p.RuntimeRelativeExecutable = "../bin/claude" }},
		{"empty component", func(p *ProviderProfileV1) { p.RuntimeRelativeExecutable = "usr//claude" }},
		{"non-ascii executable", func(p *ProviderProfileV1) { p.RuntimeRelativeExecutable = "usr/local/bin/clаude" }},
		{"unsupported schema", func(p *ProviderProfileV1) { p.Schema = "noisedeck.provider-profile.v2" }},
		{"non-ascii descriptor", func(p *ProviderProfileV1) { p.DescriptorRevision = "claude-vа" }},
		{"uppercase digest", func(p *ProviderProfileV1) { p.ExecutableSHA256 = strings.ToUpper(p.ExecutableSHA256) }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := base
			test.mutate(&candidate)
			if _, err := canonicalizeProviderProfile(candidate); !errors.Is(err, ErrInvalidProviderProfile) {
				t.Fatalf("error = %v, want ErrInvalidProviderProfile", err)
			}
		})
	}
}

func TestProviderProfileRevisionCoversEveryAuthorityField(t *testing.T) {
	base, err := builtInProviderProfile(provider.Claude)
	if err != nil {
		t.Fatalf("builtInProviderProfile: %v", err)
	}
	revision, err := canonicalizeProviderProfile(base)
	if err != nil {
		t.Fatalf("canonicalizeProviderProfile: %v", err)
	}
	mutations := []func(*ProviderProfileV1){
		func(p *ProviderProfileV1) { p.Provider = provider.Kimi },
		func(p *ProviderProfileV1) { p.DescriptorRevision = "claude-assigned-identity.v1.1" },
		func(p *ProviderProfileV1) { p.TestedVersion = "2.1.221" },
		func(p *ProviderProfileV1) { p.RuntimeRelativeExecutable = "usr/local/bin/claude-next" },
		func(p *ProviderProfileV1) { p.ExecutableSHA256 = strings.Repeat("0", 64) },
	}
	for i, mutate := range mutations {
		candidate := base
		mutate(&candidate)
		changed, err := canonicalizeProviderProfile(candidate)
		if err != nil {
			t.Fatalf("mutation %d: %v", i, err)
		}
		if changed.Digest == revision.Digest {
			t.Errorf("mutation %d did not change digest", i)
		}
	}
}

func TestClaudeProviderProfileMatchesPinnedRuntimeImage(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "..", "Dockerfile"))
	if err != nil {
		t.Fatalf("read Dockerfile: %v", err)
	}
	revision, err := CurrentProviderProfile(provider.Claude)
	if err != nil {
		t.Fatalf("CurrentProviderProfile: %v", err)
	}
	wants := []string{
		"ARG CLAUDE_CODE_VERSION=" + revision.Profile.TestedVersion,
		"-o /usr/local/bin/claude",
	}
	switch runtime.GOARCH {
	case "amd64":
		wants = append(wants, "ARG CLAUDE_BINARY_SHA256_AMD64="+revision.Profile.ExecutableSHA256)
	case "arm64":
		wants = append(wants, "ARG CLAUDE_BINARY_SHA256_ARM64="+revision.Profile.ExecutableSHA256)
	}
	for _, want := range wants {
		if !strings.Contains(string(body), want) {
			t.Errorf("Dockerfile does not contain %q", want)
		}
	}
}
