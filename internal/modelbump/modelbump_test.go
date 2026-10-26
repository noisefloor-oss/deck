package modelbump

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestParseSlugFamilies(t *testing.T) {
	for _, tc := range []struct {
		slug    string
		vendor  string
		tier    string
		version []int
	}{
		{"gpt-6.1-sol", "gpt", "sol", []int{6, 1}},
		{"gpt-6-astra", "gpt", "astra", []int{6}},
		{"gpt-5.5", "gpt", "", []int{5, 5}},
		{"claude-sonnet-5-5", "claude", "sonnet", []int{5, 5}},
		{"claude-opus-4-1", "claude", "opus", []int{4, 1}},
		{"kimi-code/k3.1-pro", "kimi", "pro", []int{3, 1}},
		{"kimi-code/k3.1-pro-highspeed", "kimi", "pro-highspeed", []int{3, 1}},
	} {
		got, ok := parseSlug(tc.slug)
		if !ok {
			t.Errorf("parseSlug(%q) rejected a versioned slug", tc.slug)
			continue
		}
		if got.vendor != tc.vendor || got.tier != tc.tier || !reflect.DeepEqual(got.version, tc.version) {
			t.Errorf("parseSlug(%q) = {vendor:%q tier:%q version:%v}, want {%q %q %v}",
				tc.slug, got.vendor, got.tier, got.version, tc.vendor, tc.tier, tc.version)
		}
	}
	for _, slug := range []string{"codex-auto-review", "gpt-daybreak-blue-latest", "fable", "latest", "gpt-", "kimi-code/k3", "kimi-code/k3.1", "kimi-code/k3.1-", "other/k3.1-pro"} {
		if _, ok := parseSlug(slug); ok {
			t.Errorf("parseSlug(%q) accepted a slug with no versioned family shape", slug)
		}
	}
}

// codexCatalogFixture is the pinned CLI 0.162.1's visible catalog, captured
// from `codex debug models --bundled` so successor selection is tested
// against the real slugs the lane will see.
var codexCatalogFixture = []string{
	"gpt-6-astra", "gpt-6.1-sol", "gpt-6-sol", "gpt-6-luna",
	"gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna", "gpt-5.5",
}

func TestSuccessorIsLikeForLike(t *testing.T) {
	for _, tc := range []struct {
		name    string
		pinned  string
		catalog []string
		want    string
	}{
		{"newest sol is current", "gpt-6.1-sol", codexCatalogFixture, ""},
		{"older sol moves to newest", "gpt-6-sol", codexCatalogFixture, "gpt-6.1-sol"},
		{"retired pin still refreshes to family max", "gpt-5.5-sol", codexCatalogFixture, "gpt-6.1-sol"},
		{"tier does not leak across families", "gpt-6-astra", codexCatalogFixture, ""},
		{"untiered slug is its own family", "gpt-5.5", codexCatalogFixture, ""},
		{"empty family offers nothing", "gpt-6.1-sol", []string{"claude-sonnet-5-6"}, ""},
		{"minor ten beats minor nine numerically", "claude-sonnet-5-9", []string{"claude-sonnet-5-9", "claude-sonnet-5-10"}, "claude-sonnet-5-10"},
		{"kimi moves within the pro tier", "kimi-code/k3.1-pro", []string{"kimi-code/k3.1-pro", "kimi-code/k3.2-pro", "kimi-code/k3.1-pro-highspeed"}, "kimi-code/k3.2-pro"},
		// A workspace CLI behind an already-accepted refresh must not
		// produce a downgrade offer: the catalog maximum is compared to
		// the pin numerically, not by slug equality.
		{"stale catalog never offers a downgrade", "gpt-6.2-sol", []string{"gpt-6.1-sol", "gpt-6-sol"}, ""},
		{"stale kimi catalog never offers a downgrade", "kimi-code/k3.2-pro", []string{"kimi-code/k3.1-pro"}, ""},
		{"same version spelled differently offers nothing", "gpt-6.2-sol", []string{"gpt-06.2-sol"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Successor(tc.pinned, tc.catalog)
			if err != nil {
				t.Fatalf("Successor(%q): %v", tc.pinned, err)
			}
			if got != tc.want {
				t.Fatalf("Successor(%q) = %q, want %q", tc.pinned, got, tc.want)
			}
		})
	}
	if _, err := Successor("fable", codexCatalogFixture); err == nil {
		t.Fatal("Successor accepted a slug with no versioned family shape")
	}
}

func TestClaudeCandidatesAreNearestFirst(t *testing.T) {
	got, err := ClaudeCandidates("claude-sonnet-5-5")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"claude-sonnet-5-6", "claude-sonnet-6-0"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("candidates = %v, want %v", got, want)
	}
	if got, err := ClaudeCandidates("claude-opus-4-1"); err != nil || !reflect.DeepEqual(got, []string{"claude-opus-4-2", "claude-opus-5-0"}) {
		t.Fatalf("opus candidates = %v, err %v", got, err)
	}
	for _, bad := range []string{"gpt-6.1-sol", "fable"} {
		if _, err := ClaudeCandidates(bad); err == nil {
			t.Fatalf("ClaudeCandidates(%q) accepted a non-claude slug", bad)
		}
	}
}

// fixtureTree writes a minimal tree with the real repeat literals.
func fixtureTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	sdlc := strings.Join([]string{
		"  dominates any recipe: when the implementer already ran gpt-6.1-sol, use",
		"  --reviewer-cmd 'codex exec --model gpt-6.1-sol \\",
		"  --reviewer-model gpt-6.1-sol \\",
		"The primary reviewer is Codex on `gpt-6.1-sol` at high reasoning effort.",
		"The secondary reviewer is Claude on Sonnet (`claude-sonnet-5-5`), also",
		"The tertiary reviewer is Kimi (`kimi-code/k3.1-pro`)",
		"kimi --model kimi-code/k3.1-pro --prompt",
		"--reviewer-model    kimi-code/k3.1-pro \\",
		"  --reviewer-cmd 'codex exec --model gpt-6.1-sol \\",
		"  --reviewer-model gpt-6.1-sol \\",
	}, "\n")
	if err := os.WriteFile(filepath.Join(root, "SDLC.md"), []byte(sdlc), 0o644); err != nil {
		t.Fatal(err)
	}
	workflowDir := filepath.Join(root, ".github", "workflows")
	if err := os.MkdirAll(workflowDir, 0o755); err != nil {
		t.Fatal(err)
	}
	workflow := "          NOISE_REVIEW_MODEL: ${{ vars.NOISE_REVIEW_MODEL || 'gpt-6-astra' }}\n"
	if err := os.WriteFile(filepath.Join(workflowDir, "harness-bump-merge.yaml"), []byte(workflow), 0o644); err != nil {
		t.Fatal(err)
	}
	// The heal workflow's reviewer fallback shares the settle seat.
	if err := os.WriteFile(filepath.Join(workflowDir, "ci-heal.yaml"), []byte(workflow), 0o644); err != nil {
		t.Fatal(err)
	}
	truth := `{"schema":1,"pins":{"primary":{"harness":"codex","model":"gpt-6.1-sol"},"secondary":{"harness":"claude","model":"claude-sonnet-5-5"},"tertiary":{"harness":"kimi","model":"kimi-code/k3.1-pro"},"settle-fallback":{"harness":"codex","model":"gpt-6-astra"}}}`
	noiseDir := filepath.Join(root, ".noise")
	if err := os.MkdirAll(noiseDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(noiseDir, "reviewer-models.json"), []byte(truth), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

func fixtureTruth(t *testing.T) Truth {
	t.Helper()
	truth, err := ReadTruth(fixtureTree(t))
	if err != nil {
		t.Fatal(err)
	}
	return truth
}

func TestDiscoverOffersAndWithholds(t *testing.T) {
	ctx := context.Background()
	codex := func(context.Context, string) ([]string, error) {
		return []string{"gpt-6.1-sol", "gpt-6.2-sol", "gpt-6-astra", "gpt-7-astra"}, nil
	}
	kimi := func(context.Context, string) ([]string, error) {
		return []string{"kimi-code/k3.1-pro", "kimi-code/k3.2-pro"}, nil
	}
	for _, tc := range []struct {
		name       string
		probe      func(context.Context, string, string) (Presence, error)
		wantOffers []Offer
		wantNotes  int
	}{
		{
			name: "claude successor found on the nearest candidate",
			probe: func(_ context.Context, _, model string) (Presence, error) {
				return map[string]Presence{"claude-sonnet-5-6": Present}[model], nil
			},
			wantOffers: []Offer{
				{Role: RolePrimary, Harness: "codex", From: "gpt-6.1-sol", To: "gpt-6.2-sol"},
				{Role: RoleSecondary, Harness: "claude", From: "claude-sonnet-5-5", To: "claude-sonnet-5-6"},
				{Role: RoleTertiary, Harness: "kimi", From: "kimi-code/k3.1-pro", To: "kimi-code/k3.2-pro"},
				{Role: RoleSettleFallback, Harness: "codex", From: "gpt-6-astra", To: "gpt-7-astra"},
			},
		},
		{
			name:  "ambiguous probe withholds the claude offer only",
			probe: func(_ context.Context, _, _ string) (Presence, error) { return Absent, nil },
			wantOffers: []Offer{
				{Role: RolePrimary, Harness: "codex", From: "gpt-6.1-sol", To: "gpt-6.2-sol"},
				{Role: RoleTertiary, Harness: "kimi", From: "kimi-code/k3.1-pro", To: "kimi-code/k3.2-pro"},
				{Role: RoleSettleFallback, Harness: "codex", From: "gpt-6-astra", To: "gpt-7-astra"},
			},
			wantNotes: 1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan, err := Discover(ctx, fixtureTruth(t), "codex", "claude", "kimi", codex, tc.probe, kimi)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(plan.Offers, tc.wantOffers) {
				t.Fatalf("offers = %+v, want %+v", plan.Offers, tc.wantOffers)
			}
			if plan.ID == "" {
				t.Fatal("plan carries no id")
			}
			notes := 0
			for _, s := range plan.Statuses {
				if s.Note != "" {
					notes++
				}
			}
			if notes != tc.wantNotes {
				t.Fatalf("notes = %d, want %d (statuses %+v)", notes, tc.wantNotes, plan.Statuses)
			}
		})
	}
}

func TestDiscoverAmbiguousProbeNeverOffers(t *testing.T) {
	ctx := context.Background()
	plan, err := Discover(ctx, fixtureTruth(t), "codex", "claude", "kimi",
		func(context.Context, string) ([]string, error) { return codexCatalogFixture, nil },
		func(_ context.Context, _, _ string) (Presence, error) { return Ambiguous, nil },
		func(context.Context, string) ([]string, error) { return []string{"kimi-code/k3.1-pro"}, nil })
	if err != nil {
		t.Fatal(err)
	}
	for _, offer := range plan.Offers {
		if offer.Role == RoleSecondary {
			t.Fatalf("ambiguous probe produced an offer: %+v", offer)
		}
	}
}

func TestFilesRewritesEveryRepeat(t *testing.T) {
	root := fixtureTree(t)
	offers := []Offer{
		{Role: RolePrimary, Harness: "codex", From: "gpt-6.1-sol", To: "gpt-6.2-sol"},
		{Role: RoleSecondary, Harness: "claude", From: "claude-sonnet-5-5", To: "claude-sonnet-5-6"},
		{Role: RoleTertiary, Harness: "kimi", From: "kimi-code/k3.1-pro", To: "kimi-code/k3.2-pro"},
		{Role: RoleSettleFallback, Harness: "codex", From: "gpt-6-astra", To: "gpt-7-astra"},
	}
	files, err := Files(root, offers)
	if err != nil {
		t.Fatal(err)
	}
	for _, stale := range []string{"gpt-6.1-sol", "claude-sonnet-5-5", "kimi-code/k3.1-pro", "'gpt-6-astra'"} {
		for path, body := range files {
			if strings.Contains(string(body.Body), stale) {
				t.Fatalf("%s still spells %q after the rewrite", path, stale)
			}
		}
	}
	sdlc := string(files["SDLC.md"].Body)
	for _, fresh := range []string{
		"when the implementer already ran gpt-6.2-sol, use",
		"codex exec --model gpt-6.2-sol \\",
		"--reviewer-model gpt-6.2-sol \\",
		"Codex on `gpt-6.2-sol` at high reasoning effort",
		"Claude on Sonnet (`claude-sonnet-5-6`)",
		"Kimi (`kimi-code/k3.2-pro`)",
		"kimi --model kimi-code/k3.2-pro --prompt",
		"--reviewer-model    kimi-code/k3.2-pro \\",
	} {
		if !strings.Contains(sdlc, fresh) {
			t.Fatalf("rewritten SDLC.md is missing %q", fresh)
		}
	}
	if got := strings.Count(sdlc, "gpt-6.2-sol \\"); got != 4 {
		t.Fatalf("recipe lines rewritten %d times, want 4 (two blocks x two flags)", got)
	}
	next, err := ReadTruth(root)
	if err != nil {
		t.Fatal(err)
	}
	if next.Pins[RolePrimary].Model != "gpt-6.1-sol" {
		t.Fatal("Files touched the pin store on disk")
	}
}

func TestFilesFailsClosedOnMissingAndDuplicatedRepeats(t *testing.T) {
	root := fixtureTree(t)
	offer := []Offer{{Role: RolePrimary, Harness: "codex", From: "gpt-6.1-sol", To: "gpt-6.2-sol"}}

	// A tree missing the prose repeat refuses to plan anything.
	sdlcPath := filepath.Join(root, "SDLC.md")
	raw, err := os.ReadFile(sdlcPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sdlcPath, []byte(strings.Replace(string(raw), "Codex on `gpt-6.1-sol` at high reasoning effort", "Codex on a rotating model", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Files(root, offer); err == nil {
		t.Fatal("Files planned a rewrite over a missing repeat")
	}

	// A tree that already spells the target refuses too: the rewrite
	// would double it.
	if err := os.WriteFile(sdlcPath, append(append([]byte{}, raw...), []byte("codex exec --model gpt-6.2-sol \\\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Files(root, offer); err == nil {
		t.Fatal("Files planned a rewrite onto a tree that already spells the target")
	}
}

func TestVerifyBindsTreeToPinStore(t *testing.T) {
	root := fixtureTree(t)
	if err := Verify(root); err != nil {
		t.Fatalf("consistent tree failed Verify: %v", err)
	}
	sdlcPath := filepath.Join(root, "SDLC.md")
	raw, err := os.ReadFile(sdlcPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, mutation := range []struct {
		name, find, replace string
	}{
		{"recipe reworded away the model", "codex exec --model gpt-6.1-sol \\", "codex exec --model $THE_MODEL \\"},
		{"one recipe copy dropped", "The primary reviewer is Codex on `gpt-6.1-sol` at high reasoning effort.", ""},
	} {
		t.Run(mutation.name, func(t *testing.T) {
			if err := os.WriteFile(sdlcPath, []byte(strings.Replace(string(raw), mutation.find, mutation.replace, 1)), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := Verify(root); err == nil {
				t.Fatal("Verify accepted a tree that drifted from the pin store")
			}
			if err := os.WriteFile(sdlcPath, raw, 0o644); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// The repeat catalogue binds exact occurrence counts, for offered and
// unoffered roles alike: a drifted copy, a duplicated recipe, or a
// leftover from an earlier model must fail the plan and the build, never
// pass because "at least one" occurrence exists.
func TestRepeatsFailClosedOnDriftAndLeftovers(t *testing.T) {
	offer := []Offer{{Role: RolePrimary, Harness: "codex", From: "gpt-6.1-sol", To: "gpt-6.2-sol"}}
	for _, tc := range []struct {
		name   string
		mutate func(t *testing.T, root string)
	}{
		{
			name: "one recipe of two reworded away",
			mutate: func(t *testing.T, root string) {
				rewrite(t, root, "codex exec --model gpt-6.1-sol \\", "codex exec --model $MODEL \\")
			},
		},
		{
			name: "recipe duplicated",
			mutate: func(t *testing.T, root string) {
				rewrite(t, root, "  --reviewer-model gpt-6.1-sol \\", "  --reviewer-model gpt-6.1-sol \\\n  --reviewer-model gpt-6.1-sol \\")
			},
		},
		{
			name: "leftover recipe from an earlier model",
			mutate: func(t *testing.T, root string) {
				rewrite(t, root, "  --reviewer-model gpt-6.1-sol \\", "  --reviewer-model gpt-6.1-sol \\\n  --reviewer-model gpt-6-sol \\")
			},
		},
		{
			name: "unoffered role drifted",
			mutate: func(t *testing.T, root string) {
				rewrite(t, root, "Claude on Sonnet (`claude-sonnet-5-5`)", "Claude on Sonnet (ask around)")
			},
		},
		{
			name: "tertiary recipe drifted",
			mutate: func(t *testing.T, root string) {
				rewrite(t, root, "kimi --model kimi-code/k3.1-pro --prompt", "kimi --model kimi-code/k3.1-pro-highspeed --prompt")
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := fixtureTree(t)
			tc.mutate(t, root)
			if _, err := Files(root, offer); err == nil {
				t.Error("Files planned over a drifted repeat")
			}
			if err := Verify(root); err == nil {
				t.Error("Verify accepted a drifted repeat")
			}
		})
	}
}

func rewrite(t *testing.T, root, from, to string) {
	t.Helper()
	path := filepath.Join(root, "SDLC.md")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), from) {
		t.Fatalf("fixture does not contain %q", from)
	}
	if err := os.WriteFile(path, []byte(strings.Replace(string(raw), from, to, 1)), 0o644); err != nil {
		t.Fatal(err)
	}
}

// The source hash and the replacement body must come from one capture:
// Files reads each input once, so a checkout update after planning
// cannot separate the hash publication validates from the body it
// writes.
func TestFilesBindsSourceHashToPlannedBody(t *testing.T) {
	root := fixtureTree(t)
	preSDLC, err := os.ReadFile(filepath.Join(root, "SDLC.md"))
	if err != nil {
		t.Fatal(err)
	}
	preTruth, err := os.ReadFile(filepath.Join(root, TruthPath))
	if err != nil {
		t.Fatal(err)
	}
	offers := []Offer{{Role: RolePrimary, Harness: "codex", From: "gpt-6.1-sol", To: "gpt-6.2-sol"}}
	files, err := Files(root, offers)
	if err != nil {
		t.Fatal(err)
	}
	if files["SDLC.md"].SourceSHA != sha256Hex(preSDLC) {
		t.Fatal("SDLC source hash does not match the bytes the body was rendered from")
	}
	if files[TruthPath].SourceSHA != sha256Hex(preTruth) {
		t.Fatal("pin store source hash does not match the bytes the plan read")
	}
	if !strings.Contains(string(files["SDLC.md"].Body), "gpt-6.2-sol") {
		t.Fatal("planned body does not carry the offered model")
	}
	// A checkout update after planning leaves the captured plan intact.
	if err := os.WriteFile(filepath.Join(root, "SDLC.md"), []byte("rewritten meanwhile\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if files["SDLC.md"].SourceSHA != sha256Hex(preSDLC) || !strings.Contains(string(files["SDLC.md"].Body), "gpt-6.2-sol") {
		t.Fatal("the plan changed under a later checkout update")
	}
}

// The reviewer's interleaving, made deterministic: a tree that advances
// between discovery and rendering must not yield a publishable plan. A
// consistent advance refuses the stale offer (the captured pin store no
// longer spells the offered From); an inconsistent advance fails repeat
// validation on the captured bytes. Either way nothing is planned.
func TestFilesRefusesATreeThatAdvancedDuringPlanning(t *testing.T) {
	offer := []Offer{{Role: RolePrimary, Harness: "codex", From: "gpt-6.1-sol", To: "gpt-6.2-sol"}}
	t.Run("consistent advance refuses the stale offer", func(t *testing.T) {
		root := fixtureTree(t)
		advance := func(from, to string) {
			raw, err := os.ReadFile(filepath.Join(root, "SDLC.md"))
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, "SDLC.md"), bytes.ReplaceAll(raw, []byte(from), []byte(to)), 0o644); err != nil {
				t.Fatal(err)
			}
			store, err := os.ReadFile(filepath.Join(root, TruthPath))
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, TruthPath), bytes.ReplaceAll(store, []byte(from), []byte(to)), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		// The whole tree moves to a consistent gpt-6.3-sol state, as a
		// checkout update between discovery and rendering would.
		advance("gpt-6.1-sol", "gpt-6.3-sol")
		files, err := Files(root, offer)
		if err == nil {
			t.Fatalf("a stale offer planned against an advanced tree: %+v", files)
		}
		if !strings.Contains(err.Error(), "since discovery") {
			t.Fatalf("unexpected refusal: %v", err)
		}
	})
	t.Run("inconsistent advance fails repeat validation", func(t *testing.T) {
		root := fixtureTree(t)
		// Only the pin store advances; the recipes no longer repeat it.
		raw, err := os.ReadFile(filepath.Join(root, TruthPath))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, TruthPath), bytes.ReplaceAll(raw, []byte("gpt-6.1-sol"), []byte("gpt-6.3-sol")), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := Files(root, offer); err == nil {
			t.Fatal("an inconsistent tree planned anyway")
		}
	})
}
