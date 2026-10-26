// Package supervisor contains the authority records shared with the confined
// runtime supervisor. It does not construct or launch a sandbox inside Deck.
package supervisor

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"strings"

	"github.com/noisefloor-oss/deck/internal/provider"
)

const (
	ProviderProfileSchemaV1 = "noisedeck.provider-profile.v1"
	// ClaudeDescriptorRevisionV1 pins the measured launch, resume, and assigned
	// identity surface admitted for the profile's exact runtime artifact. The
	// broader behavioral measurement remains recorded separately.
	ClaudeDescriptorRevisionV1 = "claude-assigned-identity.v1"
)

var (
	ErrInvalidProviderProfile     = errors.New("invalid provider profile")
	ErrProviderProfileDigest      = errors.New("provider profile digest mismatch")
	ErrProviderProfileArtifact    = errors.New("provider profile artifact mismatch")
	ErrProviderProfilePolicy      = errors.New("provider profile does not match project policy")
	ErrUnsupportedProviderProfile = errors.New("unsupported provider profile")
)

// ProviderProfileV1 is the closed, content-free identity of one executable
// admitted by confined-linux-v1. RuntimeRelativeExecutable is resolved beneath
// the supervisor-owned immutable runtime root; it is never a host path supplied
// by Deck or a provider process.
type ProviderProfileV1 struct {
	Schema                    string        `json:"schema"`
	Provider                  provider.Name `json:"provider"`
	DescriptorRevision        string        `json:"descriptor_revision"`
	TestedVersion             string        `json:"tested_version"`
	RuntimeRelativeExecutable string        `json:"runtime_relative_executable"`
	ExecutableSHA256          string        `json:"executable_sha256"`
}

type ProviderProfileRevision struct {
	Digest        string
	CanonicalJSON string
	Profile       ProviderProfileV1
}

// CurrentProviderProfile returns the one profile this build can launch for a
// provider. Adding a provider is an explicit code change, not data-plane input.
func CurrentProviderProfile(name provider.Name) (*ProviderProfileRevision, error) {
	profile, err := builtInProviderProfile(name)
	if err != nil {
		return nil, err
	}
	return canonicalizeProviderProfile(profile)
}

// ResolveProviderProfile verifies that the policy digest, the requested
// profile revision, and the supervisor-observed executable digest all select
// the same compiled profile. The closed profile is selected by its digest
// alone, matching the canonical supervisor launch request; the provider name
// is a derived result, never caller input.
func ResolveProviderProfile(
	boundPolicyProfileDigest string,
	requestedProfileRevision string,
	observedExecutableSHA256 string,
) (*ProviderProfileRevision, error) {
	if !isLowerSHA256(requestedProfileRevision) {
		return nil, fmt.Errorf("%w: malformed profile digest", ErrProviderProfileDigest)
	}
	if boundPolicyProfileDigest != requestedProfileRevision {
		return nil, ErrProviderProfilePolicy
	}
	if !isLowerSHA256(observedExecutableSHA256) {
		return nil, fmt.Errorf("%w: malformed executable digest", ErrProviderProfileArtifact)
	}
	revision, err := CurrentProviderProfile(provider.Claude)
	if err != nil {
		return nil, err
	}
	if revision.Digest != requestedProfileRevision {
		return nil, ErrProviderProfileDigest
	}
	if revision.Profile.ExecutableSHA256 != observedExecutableSHA256 {
		return nil, ErrProviderProfileArtifact
	}
	return revision, nil
}

func builtInProviderProfile(name provider.Name) (ProviderProfileV1, error) {
	if name != provider.Claude {
		return ProviderProfileV1{}, ErrUnsupportedProviderProfile
	}
	if runtime.GOOS != "linux" {
		return ProviderProfileV1{}, fmt.Errorf(
			"%w: claude on %s/%s", ErrUnsupportedProviderProfile, runtime.GOOS, runtime.GOARCH)
	}

	var executableDigest string
	switch runtime.GOARCH {
	case "amd64":
		executableDigest = "b73676430af12f97316a798b8371f81d640f90a58bdcb8f3ce56a835bb47df56"
	case "arm64":
		executableDigest = "9c64d9a7442436b65fcd7cc02c1aa0051bb5a7041e7788576ed3b5f29c447d54"
	default:
		return ProviderProfileV1{}, fmt.Errorf(
			"%w: claude on linux/%s", ErrUnsupportedProviderProfile, runtime.GOARCH)
	}

	return ProviderProfileV1{
		Schema:                    ProviderProfileSchemaV1,
		Provider:                  provider.Claude,
		DescriptorRevision:        ClaudeDescriptorRevisionV1,
		TestedVersion:             "2.1.310",
		RuntimeRelativeExecutable: "usr/local/bin/claude",
		ExecutableSHA256:          executableDigest,
	}, nil
}

func canonicalizeProviderProfile(profile ProviderProfileV1) (*ProviderProfileRevision, error) {
	if profile.Schema != ProviderProfileSchemaV1 {
		return nil, fmt.Errorf("%w: unsupported schema %q", ErrInvalidProviderProfile, profile.Schema)
	}
	if !isASCIIAtom(string(profile.Provider)) ||
		!isASCIIAtom(profile.DescriptorRevision) ||
		!isASCIIAtom(profile.TestedVersion) {
		return nil, fmt.Errorf("%w: provider identity", ErrInvalidProviderProfile)
	}
	if !validRuntimeRelativeExecutable(profile.RuntimeRelativeExecutable) {
		return nil, fmt.Errorf("%w: runtime executable", ErrInvalidProviderProfile)
	}
	if !isLowerSHA256(profile.ExecutableSHA256) {
		return nil, fmt.Errorf("%w: executable digest", ErrInvalidProviderProfile)
	}
	raw, err := json.Marshal(profile)
	if err != nil {
		return nil, fmt.Errorf("%w: encode: %w", ErrInvalidProviderProfile, err)
	}
	digest := sha256.Sum256(raw)
	return &ProviderProfileRevision{
		Digest:        hex.EncodeToString(digest[:]),
		CanonicalJSON: string(raw),
		Profile:       profile,
	}, nil
}

func validRuntimeRelativeExecutable(path string) bool {
	if path == "" || strings.HasPrefix(path, "/") || strings.HasSuffix(path, "/") {
		return false
	}
	for _, component := range strings.Split(path, "/") {
		if component == "" || component == "." || component == ".." || !isASCIIAtom(component) {
			return false
		}
	}
	return true
}

func isASCIIAtom(value string) bool {
	if value == "" || strings.TrimSpace(value) != value {
		return false
	}
	for _, r := range value {
		if r < 0x21 || r > 0x7e {
			return false
		}
	}
	return true
}

func isLowerSHA256(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	for _, r := range value {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}
