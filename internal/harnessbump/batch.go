package harnessbump

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"slices"
)

// DroppedHarness records one update the batch gave back after its gates
// failed: the pin and its repeats returned to the pre-batch state.
type DroppedHarness struct {
	Harness string `json:"harness"`
	// Version is the pin the harness keeps.
	Version string `json:"version"`
	Reason  string `json:"reason"`
}

// Revert is one updated harness's pre-batch pin: what Drop puts back when
// the harness fails its gates. It is recorded at discovery so a drop needs
// no second upstream read.
type Revert struct {
	Version  string            `json:"version"`
	Digests  map[string]string `json:"digests,omitempty"`
	PinnedOn string            `json:"pinnedOn,omitempty"`
}

// BatchResult records the complete discovery snapshot and every applied update.
type BatchResult struct {
	ID       string   `json:"id"`
	Statuses []Status `json:"statuses"`
	Updated  []string `json:"updated"`
	// Dropped names the updates returned to their pre-batch pin after a
	// failed gate; the pull request must say who stayed behind and why.
	Dropped []DroppedHarness `json:"dropped,omitempty"`
	// Revert maps each updated harness to its pre-batch pin for Drop.
	Revert map[string]Revert `json:"revert,omitempty"`
}

// BatchID binds a batch to the complete catalogue of target versions.
func BatchID(pins map[string]string) (string, error) {
	sum := sha256.New()
	for _, h := range Catalogue {
		v := pins[h.VersionArg]
		if !ValidVersion(v) {
			return "", fmt.Errorf("invalid target version for %s", h.Name)
		}
		_, _ = fmt.Fprintf(sum, "%s=%s\n", h.Name, v)
	}
	return hex.EncodeToString(sum.Sum(nil)), nil
}

// ApplyBatch discovers once before changing pins. A failed discovery or digest
// download cannot leave a partial batch that a caller mistakes for a full update.
func ApplyBatch(ctx context.Context, root string, resolver *Resolver) (BatchResult, error) {
	statuses, err := Check(ctx, root, resolver, Catalogue)
	if err != nil {
		return BatchResult{}, err
	}
	result := BatchResult{Statuses: statuses, Updated: []string{}}
	var bumps []Bump
	for _, s := range statuses {
		h, err := Lookup(s.Harness)
		if err != nil {
			return result, err
		}
		if s.Note != "" && h.Source != SourceManual {
			return result, fmt.Errorf("%s: %s", s.Harness, s.Note)
		}
		if !s.Outdated {
			continue
		}
		digests, err := resolver.Digests(ctx, h, s.Latest)
		if err != nil {
			return result, err
		}
		bumps = append(bumps, Bump{Harness: h, From: s.Pinned, To: s.Latest, Digests: digests})
	}
	for _, b := range bumps {
		_, oldDigests, err := Pinned(root, b.Harness)
		if err != nil {
			return result, err
		}
		previousDate := ""
		readme, readErr := os.ReadFile(filepath.Clean(filepath.Join(root, "README.md")))
		if readErr != nil && !os.IsNotExist(readErr) {
			return result, readErr
		}
		if readErr == nil {
			previousDate, err = candidatePinDate(string(readme), b.Harness, b.From)
			if err != nil {
				return result, err
			}
		}
		if _, err := Apply(root, b); err != nil {
			return result, err
		}
		result.Updated = append(result.Updated, b.Harness.Name)
		if result.Revert == nil {
			result.Revert = map[string]Revert{}
		}
		result.Revert[b.Harness.Name] = Revert{Version: b.From, Digests: oldDigests, PinnedOn: previousDate}
	}
	raw, err := os.ReadFile(filepath.Clean(filepath.Join(root, "Dockerfile")))
	if err != nil {
		return result, err
	}
	result.ID, err = BatchID(ReadPins(raw))
	return result, err
}

// Drop returns one harness to its pre-batch pin after its gates failed and
// recomputes the batch identity over what remains. One broken CLI release
// must not hold every other harness's update hostage: the batch ships
// without it, the report names it, and the next discovery tries it again.
func Drop(root string, result *BatchResult, harness, reason string) error {
	idx := slices.Index(result.Updated, harness)
	if idx < 0 {
		return fmt.Errorf("harnessbump: %s is not in the batch", harness)
	}
	h, err := Lookup(harness)
	if err != nil {
		return err
	}
	revert, ok := result.Revert[harness]
	if !ok {
		return fmt.Errorf("harnessbump: no pre-batch pin recorded for %s", harness)
	}
	current, _, err := Pinned(root, h)
	if err != nil {
		return err
	}
	if _, err := Apply(root, Bump{Harness: h, From: current, To: revert.Version, Digests: revert.Digests, PinnedOn: revert.PinnedOn}); err != nil {
		return fmt.Errorf("harnessbump: drop %s: %w", harness, err)
	}
	result.Updated = slices.Delete(result.Updated, idx, idx+1)
	delete(result.Revert, harness)
	result.Dropped = append(result.Dropped, DroppedHarness{Harness: harness, Version: revert.Version, Reason: reason})
	raw, err := os.ReadFile(filepath.Clean(filepath.Join(root, "Dockerfile")))
	if err != nil {
		return err
	}
	id, err := BatchID(ReadPins(raw))
	if err != nil {
		return err
	}
	result.ID = id
	return nil
}

// batchHarness is a settlement identity, not a CLI or a catalogue entry.
func batchHarness(name string) (Harness, error) {
	if name == "all" {
		return Harness{Name: "all"}, nil
	}
	return Lookup(name)
}
