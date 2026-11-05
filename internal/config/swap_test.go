package config

import "testing"

func TestSealedSwapPolicyCanonicalEnvironmentPrecedesLegacy(t *testing.T) {
	validEnv(t)
	t.Setenv("NOISEDECK_SEALED_SWAP_POLICY", "paranoid")
	t.Setenv("NOISEDECK_SWAP_POLICY", "personal")
	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Sealed.SwapPolicy != "paranoid" {
		t.Fatalf("canonical policy lost during load: %q", cfg.Sealed.SwapPolicy)
	}
}

func TestSealedRequireEncryptedStateEnvironment(t *testing.T) {
	for _, tc := range []struct {
		name, canonical, legacy string
		want                    bool
	}{
		{"canonical true wins", "true", "false", true},
		{"canonical false wins", "false", "true", false},
		{"legacy fallback", "", "true", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			validEnv(t)
			if tc.canonical != "" {
				t.Setenv("NOISEDECK_SEALED_REQUIRE_ENCRYPTED_STATE", tc.canonical)
			}
			t.Setenv("NOISEDECK_REQUIRE_ENCRYPTED_STATE", tc.legacy)
			cfg, err := Load("")
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Sealed.RequireEncryptedState != tc.want {
				t.Fatalf("encrypted state = %v, want %v", cfg.Sealed.RequireEncryptedState, tc.want)
			}
		})
	}
	t.Run("malformed canonical refused", func(t *testing.T) {
		validEnv(t)
		t.Setenv("NOISEDECK_SEALED_REQUIRE_ENCRYPTED_STATE", "not-bool")
		t.Setenv("NOISEDECK_REQUIRE_ENCRYPTED_STATE", "true")
		if _, err := Load(""); err == nil {
			t.Fatal("malformed canonical boolean was ignored in favor of the legacy value")
		}
	})
}

func TestSwapPolicyEnvironmentAndValidation(t *testing.T) {
	validEnv(t)
	for _, policy := range []string{"", "personal", "paranoid", "typo"} {
		t.Run(policy, func(t *testing.T) {
			t.Setenv("NOISEDECK_SWAP_POLICY", policy)
			cfg, err := Load("")
			if policy == "typo" {
				if err == nil {
					t.Fatal("unknown swap policy accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Sealed.SwapPolicy != policy {
				t.Fatalf("policy lost during load: %q", cfg.Sealed.SwapPolicy)
			}
		})
	}
}
