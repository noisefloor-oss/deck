package config

import (
	"runtime"
	"strings"
	"testing"
)

func privateConfig(t *testing.T) *Config {
	t.Helper()
	validEnv(t)
	cfg := mustLoad(t, "")
	cfg.Server.Bind = ""
	cfg.Server.UnixSocket = "/data/gateway/backend.sock"
	cfg.Server.GatewayUID = 1000
	cfg.Server.AllowedOrigins = []string{"https://deck.example.test"}
	cfg.Auth.Google.RedirectURL = "https://deck.example.test/auth/callback"
	cfg.Sealed.RequireEncryptedState = true
	cfg.Sealed.Profile = "shared-host"
	return cfg
}

func TestPrivateSocketConfiguration(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux-only transport")
	}
	cfg := privateConfig(t)
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if base := cfg.Server.LoopbackBase(); base != "http://localhost" {
		t.Fatalf("private hook URL base = %q", base)
	}
	for _, tc := range []struct {
		name string
		edit func(*Config)
		want string
	}{
		{"TCP also enabled", func(c *Config) { c.Server.Bind = "127.0.0.1:8837" }, "empty server.bind"},
		{"relative socket", func(c *Config) { c.Server.UnixSocket = "backend.sock" }, "canonical absolute"},
		{"unclean socket", func(c *Config) { c.Server.UnixSocket = "/data/../backend.sock" }, "canonical absolute"},
		{"long socket", func(c *Config) { c.Server.UnixSocket = "/" + strings.Repeat("s", 104) }, "103 bytes"},
		{"root gateway", func(c *Config) { c.Server.GatewayUID = 0 }, "non-root"},
		{"negative UID", func(c *Config) { c.Server.GatewayUID = -1 }, "non-root"},
		{"password mode", func(c *Config) { c.Auth.Mode = "password" }, "oidc authentication"},
		{"unchecked stores", func(c *Config) { c.Sealed.RequireEncryptedState = false }, "shared-host encrypted-state gate"},
		{"own host", func(c *Config) { c.Sealed.Profile = "own-host" }, "shared-host encrypted-state gate"},
		{"HTTP origin", func(c *Config) { c.Server.AllowedOrigins = []string{"http://deck.example.test"} }, "HTTPS browser origins"},
		{"loopback previews", func(c *Config) { c.ProjectPreviews.Loopback = true }, "alternate project preview listener"},
		{"preview listener", func(c *Config) { c.ProjectPreviews.Bind = "127.0.0.1:8838" }, "alternate project preview listener"},
		{"capacity experiment", func(c *Config) { c.Experiments.UnconfinedCapacityRouting = true }, "TCP capacity-routing"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := *cfg
			tc.edit(&c)
			if err := c.Validate(); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected %q refusal, got %v", tc.want, err)
			}
		})
	}
}

func TestPrivateSocketEnvironment(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux-only transport")
	}
	validEnv(t)
	t.Setenv("NOISEDECK_SERVER_UNIX_SOCKET", "/data/gateway/backend.sock")
	t.Setenv("NOISEDECK_SERVER_GATEWAY_UID", "1000")
	t.Setenv("NOISEDECK_SEALED_REQUIRE_ENCRYPTED_STATE", "true")
	t.Setenv("NOISEDECK_AUTH_GOOGLE_REDIRECT_URL", "https://deck.example.test/auth/callback")
	path := writeConfig(t, "server:\n  bind: ''\n  allowed_origins: ['https://deck.example.test']\n")
	cfg := mustLoad(t, path)
	if cfg.Server.UnixSocket != "/data/gateway/backend.sock" || cfg.Server.GatewayUID != 1000 || cfg.Server.Bind != "" {
		t.Fatal("private transport configuration did not survive loading")
	}
	t.Setenv("NOISEDECK_SERVER_GATEWAY_UID", "not-a-uid")
	if _, err := Load(path); err == nil {
		t.Fatal("invalid peer UID accepted")
	}
}
