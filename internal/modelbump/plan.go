package modelbump

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Repeat is one place outside the pin store that spells a pinned model:
// the SDLC's recipe blocks and prose, the settle workflow's fallback
// literal. Format carries one %s for the model. Count is how many times
// the tree must spell it; a required repeat the tree does not spell the
// expected number of times is an error, never a guess — the catalogue is
// the enumeration, and a literal it does not know about is a pin a
// refresh would leave behind.
type Repeat struct {
	Path   string
	Format string
	Count  int
}

func (r Repeat) format(model string) string {
	return fmt.Sprintf(r.Format, model)
}

// repeatsFor lists every literal that must move with a role's pin.
func repeatsFor(role string) ([]Repeat, error) {
	const sdlc = "SDLC.md"
	switch role {
	case RolePrimary:
		return []Repeat{
			// The dominance-rule example names the primary model; leaving
			// it behind after a refresh would teach readers a stale model.
			{Path: sdlc, Format: "when the implementer already ran %s, use", Count: 1},
			// The dispatch recipe appears twice: once in the full command
			// block, once in the standing-focus variant.
			{Path: sdlc, Format: "codex exec --model %s \\", Count: 2},
			{Path: sdlc, Format: "--reviewer-model %s \\", Count: 2},
			{Path: sdlc, Format: "Codex on `%s` at high reasoning effort", Count: 1},
		}, nil
	case RoleSecondary:
		return []Repeat{
			{Path: sdlc, Format: "Claude on Sonnet (`%s`)", Count: 1},
		}, nil
	case RoleTertiary:
		return []Repeat{
			{Path: sdlc, Format: "Kimi (`%s`)", Count: 1},
			{Path: sdlc, Format: "kimi --model %s --prompt", Count: 1},
			{Path: sdlc, Format: "--reviewer-model    %s \\", Count: 1},
		}, nil
	case RoleSettleFallback:
		return []Repeat{
			// The in-repo half of the settle reviewer's model; the
			// NOISE_REVIEW_MODEL repository variable, when set, overrides
			// it and the offer body must say so.
			{Path: ".github/workflows/harness-bump-merge.yaml", Format: "vars.NOISE_REVIEW_MODEL || '%s' }}", Count: 1},
			// The heal workflow's reviewer fallback shares the same seat
			// and the same override; an accepted offer must refresh both.
			{Path: ".github/workflows/ci-heal.yaml", Format: "vars.NOISE_REVIEW_MODEL || '%s' }}", Count: 1},
		}, nil
	}
	return nil, fmt.Errorf("modelbump: no repeats declared for role %q", role)
}

// Offer is one role's proposed like-for-like refresh.
type Offer struct {
	Role    string `json:"role"`
	Harness string `json:"harness"`
	From    string `json:"from"`
	To      string `json:"to"`
}

// Status is one role's discovery outcome, offered or not.
type Status struct {
	Role    string `json:"role"`
	Harness string `json:"harness"`
	Pinned  string `json:"pinned"`
	// Latest is the offered successor, empty when there is no offer.
	Latest string `json:"latest,omitempty"`
	// Note says why an offer was withheld; empty when one was made.
	Note string `json:"note,omitempty"`
}

// Plan is a complete, all-or-nothing refresh plan.
type Plan struct {
	// ID identifies the resulting pin set, so one offer PR maps to one
	// set of target models and a retried discovery with the same result
	// produces the same branch name.
	ID       string   `json:"id"`
	Statuses []Status `json:"statuses"`
	Offers   []Offer  `json:"offers"`
}

// CatalogSource lists the models one vendor publishes now.
type CatalogSource func(ctx context.Context, exec string) ([]string, error)

// MembershipProbe reports whether one candidate model exists in a CLI's
// local catalog. Ambiguous means the probe could not tell; the lane then
// withholds the offer rather than guess.
type MembershipProbe func(ctx context.Context, exec, model string) (Presence, error)

// Presence is a probe outcome.
type Presence int

const (
	Present Presence = iota
	Absent
	Ambiguous
)

func Discover(ctx context.Context, truth Truth, codexExec, claudeExec, kimiExec string, codex CatalogSource, claude MembershipProbe, kimi CatalogSource) (Plan, error) {
	var codexCatalog []string
	codexLoaded := false
	plan := Plan{Statuses: []Status{}, Offers: []Offer{}}
	for _, role := range Roles() {
		pin := truth.Pins[role]
		switch pin.Harness {
		case "codex":
			if !codexLoaded {
				catalog, err := codex(ctx, codexExec)
				if err != nil {
					return plan, fmt.Errorf("modelbump: %s: codex catalog: %w", role, err)
				}
				codexCatalog, codexLoaded = catalog, true
			}
			successor, err := Successor(pin.Model, codexCatalog)
			if err != nil {
				return plan, fmt.Errorf("modelbump: %s: %w", role, err)
			}
			plan.Statuses = append(plan.Statuses, statusFor(role, pin, successor, ""))
		case "claude":
			candidates, err := ClaudeCandidates(pin.Model)
			if err != nil {
				return plan, fmt.Errorf("modelbump: %s: %w", role, err)
			}
			successor, note := "", "no nearer successor found; the pinned model stays"
		probe:
			for _, candidate := range candidates {
				presence, err := claude(ctx, claudeExec, candidate)
				if err != nil {
					return plan, fmt.Errorf("modelbump: %s: probing %s: %w", role, candidate, err)
				}
				switch presence {
				case Present:
					successor, note = candidate, ""
					break probe
				case Ambiguous:
					// An unreadable probe cannot rank candidates: offering a
					// farther one while a nearer one exists would not be
					// like-for-like. Withhold the whole role this run.
					note = fmt.Sprintf("probe for %s was ambiguous; no offer this run", candidate)
					break probe
				}
			}
			plan.Statuses = append(plan.Statuses, statusFor(role, pin, successor, note))
		case "kimi":
			catalog, err := kimi(ctx, kimiExec)
			if err != nil {
				return plan, fmt.Errorf("modelbump: %s: kimi catalog: %w", role, err)
			}
			successor, err := Successor(pin.Model, catalog)
			if err != nil {
				return plan, fmt.Errorf("modelbump: %s: %w", role, err)
			}
			plan.Statuses = append(plan.Statuses, statusFor(role, pin, successor, ""))
		default:
			return plan, fmt.Errorf("modelbump: %s: harness %q has no discovery source", role, pin.Harness)
		}
	}
	for _, s := range plan.Statuses {
		if s.Latest == "" {
			continue
		}
		plan.Offers = append(plan.Offers, Offer{Role: s.Role, Harness: s.Harness, From: s.Pinned, To: s.Latest})
	}
	var err error
	plan.ID, err = planID(truth, plan.Offers)
	if err != nil {
		return Plan{}, err
	}
	return plan, nil
}

// planID binds an offer to the complete resulting pin set. Two runs that
// discover the same successors compute the same id, so the offer branch
// and pull request are stable across retries within a day.
func planID(truth Truth, offers []Offer) (string, error) {
	next := Truth{Schema: 1, Pins: map[string]Pin{}}
	for role, pin := range truth.Pins {
		next.Pins[role] = pin
	}
	for _, offer := range offers {
		next.Pins[offer.Role] = Pin{Harness: offer.Harness, Model: offer.To}
	}
	raw, err := next.Marshal()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])[:16], nil
}

func statusFor(role string, pin Pin, latest, note string) Status {
	return Status{Role: role, Harness: pin.Harness, Pinned: pin.Model, Latest: latest, Note: note}
}

// repeatPattern turns a Repeat's Format into a matcher for the repeat
// with ANY model slotted in, so a leftover copy spelling an older model
// is as visible as a missing copy. The Format has exactly one %s.
func repeatPattern(r Repeat) (*regexp.Regexp, error) {
	if strings.Count(r.Format, "%s") != 1 {
		return nil, fmt.Errorf("modelbump: repeat format %q does not carry exactly one %%s", r.Format)
	}
	parts := strings.SplitN(r.Format, "%s", 2)
	pattern, err := regexp.Compile(regexp.QuoteMeta(parts[0]) + "([^'\\s]+)" + regexp.QuoteMeta(parts[1]))
	if err != nil {
		return nil, err
	}
	return pattern, nil
}

// validateRepeats enforces, for every role, that each repeat appears
// exactly Count times and every occurrence spells the pinned model. A
// drifted copy (one recipe of two reworded), a duplicated recipe, or a
// leftover copy from an earlier model all fail here — the catalogue is
// the enumeration, and the tree must match it exactly. It reads the
// captured inputs, never the tree, so validation and rendering cannot
// observe different bytes.
func validateRepeats(inputs map[string][]byte, truth Truth) error {
	for _, role := range Roles() {
		pin := truth.Pins[role]
		repeats, err := repeatsFor(role)
		if err != nil {
			return err
		}
		for _, repeat := range repeats {
			raw, ok := inputs[repeat.Path]
			if !ok {
				return fmt.Errorf("modelbump: %s: capture is missing %s", role, repeat.Path)
			}
			pattern, err := repeatPattern(repeat)
			if err != nil {
				return err
			}
			shaped := len(pattern.FindAllSubmatch(raw, -1))
			pinned := strings.Count(string(raw), repeat.format(pin.Model))
			if shaped != repeat.Count || pinned != repeat.Count {
				return fmt.Errorf(
					"modelbump: %s repeat in %s: found %d occurrences (%d spelling the pinned %s), want exactly %d spelling %s — reconcile SDLC.md, the settle workflow, and %s",
					role, repeat.Path, shaped, pinned, pin.Model, repeat.Count, pin.Model, TruthPath)
			}
		}
	}
	return nil
}

// captureInputs reads the pin store and every repeat file — every input
// a plan can touch — exactly once. Files parses, validates, renders, and
// hashes these captured bytes and nothing else, so a checkout update
// landing anywhere during planning cannot split the plan's source hashes
// from its replacement bodies or validate bytes it does not publish
// against.
func captureInputs(root string) (map[string][]byte, error) {
	paths := map[string]bool{TruthPath: true}
	for _, role := range Roles() {
		repeats, err := repeatsFor(role)
		if err != nil {
			return nil, err
		}
		for _, repeat := range repeats {
			paths[repeat.Path] = true
		}
	}
	inputs := map[string][]byte{}
	for path := range paths {
		raw, err := os.ReadFile(filepath.Clean(filepath.Join(root, path)))
		if err != nil {
			return nil, fmt.Errorf("modelbump: capturing %s: %w", path, err)
		}
		inputs[path] = raw
	}
	return inputs, nil
}

// PlannedFile is one file's complete publication plan: the replacement
// body AND the hash of the exact source bytes that body was rendered
// from, captured in the same read. Publication validates the base blobs
// against SourceSHA and writes Body, so the two can never disagree about
// which input produced which output.
type PlannedFile struct {
	Body      []byte
	SourceSHA string
}

func sha256Hex(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// Files renders every file an offer set rewrites, without touching disk:
// the pin store plus each repeat. All inputs are captured in one pass;
// the pin store the plan renders is parsed from the same captured bytes
// its hash records, offers are reconciled against that captured store,
// and validation, rendering, and hashing all read the captures — never
// the tree. A checkout update during planning therefore either changes
// nothing the plan saw or fails the offer's staleness check; it can
// never publish a pin store and recipes that disagree.
func Files(root string, offers []Offer) (map[string]PlannedFile, error) {
	inputs, err := captureInputs(root)
	if err != nil {
		return nil, err
	}
	truth, err := ParseTruth(inputs[TruthPath])
	if err != nil {
		return nil, err
	}
	if err := validateRepeats(inputs, truth); err != nil {
		return nil, err
	}
	next := Truth{Schema: 1, Pins: map[string]Pin{}}
	for role, pin := range truth.Pins {
		next.Pins[role] = pin
	}
	for _, offer := range offers {
		pin, ok := truth.Pins[offer.Role]
		if !ok {
			return nil, fmt.Errorf("modelbump: offer for unknown role %q", offer.Role)
		}
		if pin.Model != offer.From {
			return nil, fmt.Errorf("modelbump: %s pin moved from %q to %q since discovery", offer.Role, offer.From, pin.Model)
		}
		next.Pins[offer.Role] = Pin{Harness: pin.Harness, Model: offer.To}
	}
	nextRaw, err := next.Marshal()
	if err != nil {
		return nil, err
	}
	files := map[string]PlannedFile{
		TruthPath: {Body: nextRaw, SourceSHA: sha256Hex(inputs[TruthPath])},
	}

	// Plan every repeat for every offer before returning anything.
	type edit struct{ from, to string }
	perFile := map[string][]edit{}
	for _, offer := range offers {
		repeats, err := repeatsFor(offer.Role)
		if err != nil {
			return nil, err
		}
		for _, repeat := range repeats {
			perFile[repeat.Path] = append(perFile[repeat.Path], edit{repeat.format(offer.From), repeat.format(offer.To)})
		}
	}
	for path, edits := range perFile {
		raw, ok := inputs[path]
		if !ok {
			return nil, fmt.Errorf("modelbump: capture is missing %s", path)
		}
		body := string(raw)
		for _, e := range edits {
			if toCount := strings.Count(body, e.to); toCount != 0 && e.from != e.to {
				return nil, fmt.Errorf("modelbump: %s already spells %q %d times before the rewrite", path, e.to, toCount)
			}
			body = strings.ReplaceAll(body, e.from, e.to)
		}
		files[path] = PlannedFile{Body: []byte(body), SourceSHA: sha256Hex(raw)}
	}
	return files, nil
}

// Verify checks the live tree against the pin store: for every role,
// every repeat must appear exactly its declared number of times and
// every occurrence must spell the pinned model. CI runs it so a
// hand-edited recipe, a duplicated recipe, or a leftover copy from an
// earlier model fails a build instead of drifting from the store.
func Verify(root string) error {
	inputs, err := captureInputs(root)
	if err != nil {
		return err
	}
	truth, err := ParseTruth(inputs[TruthPath])
	if err != nil {
		return err
	}
	return validateRepeats(inputs, truth)
}

// ErrLaneNotShipped reports a tree that carries neither the pin store
// nor the SDLC — the public source export, where the private review
// lane's files are outside the boundary. A tree that ships SDLC.md but
// not the pin store is NOT this error: it is a broken private checkout.
var ErrLaneNotShipped = errors.New("modelbump: neither the pin store nor SDLC.md ships in this tree")

// VerifyLiveTree binds the live tree to the pin store when the private
// review lane ships in it. In the public source export the lane's files
// (SDLC.md, .noise/, the settle workflow) are deliberately absent and
// the guard does not apply.
func VerifyLiveTree(root string) error {
	_, truthErr := os.Stat(filepath.Join(root, TruthPath))
	_, sdlcErr := os.Stat(filepath.Join(root, "SDLC.md"))
	if truthErr != nil && sdlcErr != nil {
		return ErrLaneNotShipped
	}
	return Verify(root)
}
