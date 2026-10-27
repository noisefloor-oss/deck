package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/noisefloor-oss/deck/internal/provider"
)

// validSessionSecret is 32 bytes: oidc mode requires a session secret of
// at least 32 bytes because it keys the HMAC that signs sessions.
const validSessionSecret = "0123456789abcdef0123456789abcdef"

// validEnv selects OIDC and supplies its complete configuration. Tests for
// OIDC validation then mutate one input at a time without depending on the
// compiled default mode.
func validEnv(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	t.Setenv("NOISEDECK_PROJECTS_ALLOWED_ROOTS", root)
	t.Setenv("NOISEDECK_AUTH_MODE", "oidc")
	// OIDC mode is strict-always: a partially configured Google login is
	// a locked-out console, refused at startup rather than at first login.
	t.Setenv("NOISEDECK_AUTH_SESSION_SECRET", validSessionSecret)
	t.Setenv("NOISEDECK_AUTH_GOOGLE_CLIENT_ID", "client-id.apps.googleusercontent.com")
	t.Setenv("NOISEDECK_AUTH_GOOGLE_CLIENT_SECRET", "test-google-secret")
	t.Setenv("NOISEDECK_AUTH_GOOGLE_REDIRECT_URL", "http://127.0.0.1:8837/auth/callback")
	t.Setenv("NOISEDECK_AUTH_GOOGLE_ALLOWED_EMAIL", "operator@example.com")
	return root
}

func mustLoad(t *testing.T, path string) *Config {
	t.Helper()
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load(%q): %v", path, err)
	}
	return cfg
}

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func unsetEnv(t *testing.T, keys ...string) {
	t.Helper()
	for _, key := range keys {
		value, existed := os.LookupEnv(key)
		if err := os.Unsetenv(key); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if existed {
				_ = os.Setenv(key, value)
			} else {
				_ = os.Unsetenv(key)
			}
		})
	}
}

func defaultPasswordEnv(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	t.Setenv("NOISEDECK_PROJECTS_ALLOWED_ROOTS", root)
	unsetEnv(t,
		"NOISEDECK_AUTH_MODE",
		"NOISEDECK_AUTH_SESSION_SECRET",
		"NOISEDECK_AUTH_PASSWORD_HASH",
		"NOISEDECK_AUTH_GOOGLE_CLIENT_ID",
		"NOISEDECK_AUTH_GOOGLE_CLIENT_SECRET",
		"NOISEDECK_AUTH_GOOGLE_ALLOWED_EMAIL",
	)
	return root
}

func TestDefaultsAloneFailClosed(t *testing.T) {
	_, err := Load("")
	if err == nil || !strings.Contains(err.Error(), "projects.allowed_roots") {
		t.Fatalf("defaults without allowed roots must fail on projects.allowed_roots, got: %v", err)
	}
}

func TestDefaultsWithRootsAreValid(t *testing.T) {
	root := defaultPasswordEnv(t)
	cfg := mustLoad(t, "")

	if cfg.Server.Bind != "127.0.0.1:8837" {
		t.Errorf("default bind = %q", cfg.Server.Bind)
	}
	if cfg.Auth.Mode != "password" {
		t.Errorf("default auth mode = %q", cfg.Auth.Mode)
	}
	if got := cfg.Reconciler.Interval.Std(); got != 15*time.Second {
		t.Errorf("default reconcile interval = %s", got)
	}
	if cfg.Taskmaster.Executable != "/usr/local/bin/noise" {
		t.Errorf("default task executable = %q", cfg.Taskmaster.Executable)
	}
	if len(cfg.Projects.AllowedRoots) != 1 || cfg.Projects.AllowedRoots[0] != root {
		t.Errorf("allowed roots = %v, want [%s]", cfg.Projects.AllowedRoots, root)
	}
	if !filepath.IsAbs(cfg.Database.Path) {
		t.Errorf("database path not absolutized: %q", cfg.Database.Path)
	}
	if cfg.Experiments.UnconfinedCapacityRouting {
		t.Error("unconfined capacity routing must be disabled by default")
	}
}

func TestUnconfinedCapacityRoutingRequiresExplicitEnable(t *testing.T) {
	defaultPasswordEnv(t)
	t.Setenv("NOISEDECK_EXPERIMENTS_UNCONFINED_CAPACITY_ROUTING", "true")
	if !mustLoad(t, "").Experiments.UnconfinedCapacityRouting {
		t.Fatal("explicit experiment environment flag was not applied")
	}
}

func TestFontBenchRequiresExplicitEnable(t *testing.T) {
	defaultPasswordEnv(t)
	if mustLoad(t, "").Experiments.FontBench {
		t.Fatal("the font bench must be disabled by default")
	}
	t.Setenv("NOISEDECK_EXPERIMENTS_FONT_BENCH", "true")
	if !mustLoad(t, "").Experiments.FontBench {
		t.Fatal("explicit font-bench environment flag was not applied")
	}
}

func TestDevPreviewRequiresExplicitEnable(t *testing.T) {
	defaultPasswordEnv(t)
	cfg := mustLoad(t, "")
	if cfg.Experiments.DevPreview {
		t.Fatal("the dev preview must be disabled by default")
	}
	// The upstream default is present even while the flag is off, so
	// enabling the experiment is a one-flag operation.
	if cfg.Experiments.DevPreviewUpstream != "127.0.0.1:5173" {
		t.Fatalf("default dev-preview upstream = %q, want the Vite port", cfg.Experiments.DevPreviewUpstream)
	}
	t.Setenv("NOISEDECK_EXPERIMENTS_DEV_PREVIEW", "true")
	if !mustLoad(t, "").Experiments.DevPreview {
		t.Fatal("explicit dev-preview environment flag was not applied")
	}
}

// The dev-preview route forwards requests carrying your live
// session, so configuration alone must never be able to point it off-host.
func TestDevPreviewUpstreamMustBeLoopback(t *testing.T) {
	for _, upstream := range []string{"10.0.0.5:5173", "evil.test:5173", "0.0.0.0:5173", "127.0.0.1", "127.0.0.1:0"} {
		t.Run(upstream, func(t *testing.T) {
			defaultPasswordEnv(t)
			t.Setenv("NOISEDECK_EXPERIMENTS_DEV_PREVIEW", "true")
			t.Setenv("NOISEDECK_EXPERIMENTS_DEV_PREVIEW_UPSTREAM", upstream)
			if _, err := Load(""); err == nil {
				t.Fatalf("dev-preview upstream %q was accepted", upstream)
			}
		})
	}
}

// The upstream is only meaningful when the route exists. A stale or odd value
// left in configuration must not fail a deployment that has the flag off.
func TestDevPreviewUpstreamIsUncheckedWhileDisabled(t *testing.T) {
	defaultPasswordEnv(t)
	t.Setenv("NOISEDECK_EXPERIMENTS_DEV_PREVIEW_UPSTREAM", "not-a-host-port")
	if _, err := Load(""); err != nil {
		t.Fatalf("disabled dev preview validated its upstream: %v", err)
	}
}

func TestDefaultPasswordNeedsNoExternalAuthConfiguration(t *testing.T) {
	defaultPasswordEnv(t)

	cfg := mustLoad(t, "")
	if cfg.Auth.Mode != "password" {
		t.Fatalf("default auth mode = %q, want password", cfg.Auth.Mode)
	}
	if cfg.Auth.SessionSecret != "" || cfg.Auth.PasswordHash != "" {
		t.Fatal("default password mode required external authentication material")
	}
}

// The committed example must always parse, pass strict unknown-key checking,
// and validate. This is what keeps configs/config.example.yaml honest.
func TestExampleConfigIsValid(t *testing.T) {
	cfg, err := Load(filepath.Join("..", "..", "configs", "config.example.yaml"))
	if err != nil {
		t.Fatalf("the committed example config does not load: %v", err)
	}
	if len(cfg.Projects.AllowedRoots) == 0 {
		t.Fatal("example config carries no allowed roots")
	}
	if cfg.Auth.Mode != "password" {
		t.Errorf("example auth mode = %q, want password", cfg.Auth.Mode)
	}
	if cfg.Taskmaster.Executable != "/usr/local/bin/noise" {
		t.Errorf("example task executable = %q", cfg.Taskmaster.Executable)
	}
}

func TestFileOverlaysDefaults(t *testing.T) {
	validEnv(t)
	p := writeConfig(t, `
server:
  bind: "127.0.0.1:9000"
tmux:
  session_prefix: "acx-"
reconciler:
  interval: "10s"
`)
	cfg := mustLoad(t, p)
	if cfg.Server.Bind != "127.0.0.1:9000" {
		t.Errorf("bind = %q", cfg.Server.Bind)
	}
	if cfg.Tmux.SessionPrefix != "acx-" {
		t.Errorf("prefix = %q", cfg.Tmux.SessionPrefix)
	}
	if got := cfg.Reconciler.Interval.Std(); got != 10*time.Second {
		t.Errorf("interval = %s", got)
	}
	// Untouched fields keep defaults.
	if cfg.Logging.Level != "info" {
		t.Errorf("level = %q", cfg.Logging.Level)
	}
}

func TestEnvOverridesFile(t *testing.T) {
	validEnv(t)
	p := writeConfig(t, `
server:
  bind: "127.0.0.1:9000"
`)
	t.Setenv("NOISEDECK_SERVER_BIND", "127.0.0.1:9100")
	cfg := mustLoad(t, p)
	if cfg.Server.Bind != "127.0.0.1:9100" {
		t.Errorf("env must beat file: bind = %q", cfg.Server.Bind)
	}
}

func TestDefaultScrollbackIsTwoThousandLines(t *testing.T) {
	if got := Defaults().Tmux.ScrollbackLines; got != 2000 {
		t.Errorf("default scrollback = %d, want 2000", got)
	}
	validEnv(t)
	cfg := mustLoad(t, "")
	if cfg.Tmux.ScrollbackLines != 2000 {
		t.Errorf("loaded default scrollback = %d, want 2000", cfg.Tmux.ScrollbackLines)
	}
}

func TestDefaultOmpProviderResolvesFromPathWithItsHook(t *testing.T) {
	defaults := Defaults()
	if got := defaults.Providers.Omp.Executable; got != "" {
		t.Errorf("default omp executable = %q, want PATH discovery", got)
	}
	if got := defaults.Providers.Omp.HookConfig; got != "~/.omp/noisedeck-provider-hook.ts" {
		t.Errorf("default omp hook config = %q", got)
	}
}

func TestDefaultPiHookIsPackaged(t *testing.T) {
	defaults := Defaults()
	if got := defaults.Providers.Pi.HookConfig; got != "~/.pi/noisedeck-provider-hook.js" {
		t.Errorf("default pi hook config = %q", got)
	}
}

func TestDefaultCommandCodeProviderResolvesFromPathWithItsHook(t *testing.T) {
	defaults := Defaults()
	if got := defaults.Providers.CommandCode.Executable; got != "" {
		t.Errorf("default command-code executable = %q, want PATH discovery", got)
	}
	if got := defaults.Providers.CommandCode.HookConfig; got != "~/.commandcode/settings.json" {
		t.Errorf("default command-code hook config = %q", got)
	}
}

func TestDefaultDevinProviderResolvesFromPathWithItsHook(t *testing.T) {
	defaults := Defaults()
	if got := defaults.Providers.Devin.Executable; got != "" {
		t.Fatalf("devin executable = %q, want PATH discovery", got)
	}
	if got := defaults.Providers.Devin.HookConfig; got != "~/.config/devin/config.json" {
		t.Fatalf("devin hook config = %q", got)
	}
	if got := defaults.Providers.Devin.HookReceiver; got != provider.HookReceiverPath {
		t.Fatalf("devin hook receiver = %q", got)
	}
}

func TestDefaultQwenProviderResolvesFromPathWithNoHook(t *testing.T) {
	defaults := Defaults()
	if got := defaults.Providers.Qwen.Executable; got != "" {
		t.Fatalf("qwen executable = %q, want PATH discovery", got)
	}
	// Qwen Code is fresh-relaunch-only: a default hook config would name an
	// identity path no authenticated run observed.
	if got := defaults.Providers.Qwen.HookConfig; got != "" {
		t.Fatalf("qwen hook config = %q, want none", got)
	}
	if got := defaults.Providers.Qwen.HookReceiver; got != "" {
		t.Fatalf("qwen hook receiver = %q, want none", got)
	}
}

func TestDefaultMimoProviderResolvesFromPathWithoutIdentityHook(t *testing.T) {
	defaults := Defaults()
	if got := defaults.Providers.Mimo.Executable; got != "" {
		t.Fatalf("mimo executable = %q, want PATH discovery", got)
	}
	if got := defaults.Providers.Mimo.HookConfig; got != "" {
		t.Fatalf("mimo hook config = %q", got)
	}
}

func TestDefaultQoderProviderResolvesFromPathWithoutIdentityHook(t *testing.T) {
	defaults := Defaults()
	if got := defaults.Providers.Qoder.Executable; got != "" {
		t.Fatalf("qoder executable = %q, want PATH discovery", got)
	}
	if got := defaults.Providers.Qoder.HookConfig; got != "" {
		t.Fatalf("qoder hook config = %q", got)
	}
}

func TestDefaultCursorProviderResolvesFromPathWithoutIdentityHook(t *testing.T) {
	defaults := Defaults()
	if got := defaults.Providers.Cursor.Executable; got != "" {
		t.Fatalf("cursor executable = %q, want PATH discovery", got)
	}
	if got := defaults.Providers.Cursor.HookConfig; got != "" {
		t.Fatalf("cursor hook config = %q", got)
	}
}

func TestDefaultGrokProviderResolvesFromPathWithoutIdentityHook(t *testing.T) {
	defaults := Defaults()
	if got := defaults.Providers.Grok.Executable; got != "" {
		t.Fatalf("grok executable = %q, want PATH discovery", got)
	}
	if got := defaults.Providers.Grok.HookConfig; got != "" {
		t.Fatalf("grok hook config = %q", got)
	}
}

func TestScrollbackUserOverrideIsHonored(t *testing.T) {
	validEnv(t)
	t.Setenv("NOISEDECK_TMUX_SCROLLBACK_LINES", "5000")
	cfg := mustLoad(t, "")
	if cfg.Tmux.ScrollbackLines != 5000 {
		t.Errorf("env override = %d, want 5000 (must not be clamped to the default)", cfg.Tmux.ScrollbackLines)
	}
}

func TestEnvListParsing(t *testing.T) {
	validEnv(t)
	root := t.TempDir()
	other := t.TempDir()
	t.Setenv("NOISEDECK_PROJECTS_ALLOWED_ROOTS", root+" , "+other)
	cfg := mustLoad(t, "")
	if len(cfg.Projects.AllowedRoots) != 2 {
		t.Fatalf("roots = %v", cfg.Projects.AllowedRoots)
	}
	if cfg.Projects.AllowedRoots[0] != root || cfg.Projects.AllowedRoots[1] != other {
		t.Errorf("roots = %v, want [%s %s]", cfg.Projects.AllowedRoots, root, other)
	}
}

func TestUnknownYAMLKeyRejected(t *testing.T) {
	validEnv(t)
	p := writeConfig(t, `
tmux:
  sesion_prefix: "oops-"
`)
	if _, err := Load(p); err == nil {
		t.Fatal("a misspelled key must fail loudly, not silently default")
	}
}

func TestSessionSecretHasNoYAMLKey(t *testing.T) {
	validEnv(t)
	p := writeConfig(t, `
auth:
  session_secret: "should-not-live-in-a-file"
`)
	if _, err := Load(p); err == nil {
		t.Fatal("session_secret in the file must be rejected — the secret is env-only")
	}
}

func TestPasswordModeDefersGeneratedSecretsToBoot(t *testing.T) {
	defaultPasswordEnv(t)
	t.Setenv("NOISEDECK_AUTH_MODE", "password")
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("password config: %v", err)
	}
	if cfg.Auth.Mode != "password" || cfg.Auth.SessionSecret != "" {
		t.Fatalf("password config = %+v", cfg.Auth)
	}
}

func TestPasswordHashHasNoYAMLKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("auth:\n  password_hash: plaintext\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "field password_hash not found") {
		t.Fatalf("password hash YAML error = %v", err)
	}
}

func TestShortSessionSecretRejected(t *testing.T) {
	// The session secret keys the HMAC that signs every session cookie, so a
	// short, guessable value is refused even on a loopback bind.
	validEnv(t)
	t.Setenv("NOISEDECK_AUTH_SESSION_SECRET", "short")
	if _, err := Load(""); err == nil || !strings.Contains(err.Error(), "32 bytes") {
		t.Fatalf("a short session secret must be rejected in builtin mode, got: %v", err)
	}
}

func TestRedirectURLValidation(t *testing.T) {
	bad := map[string]string{
		"https without secure cookies": "https://console.example.test/auth/callback",
		"trailing empty query":         "http://127.0.0.1:8837/auth/callback?",
		"trailing empty fragment":      "http://127.0.0.1:8837/auth/callback#",
		"percent-encoded path":         "http://127.0.0.1:8837/%61uth/callback",
		"userinfo":                     "http://user@127.0.0.1:8837/auth/callback",
		"hostless":                     "https:///auth/callback",
		"wrong path":                   "http://127.0.0.1:8837/login",
		"origin not allowlisted":       "http://127.0.0.1:9999/auth/callback",
	}
	for name, ru := range bad {
		t.Run(name, func(t *testing.T) {
			validEnv(t)
			t.Setenv("NOISEDECK_AUTH_GOOGLE_REDIRECT_URL", ru)
			if _, err := Load(""); err == nil {
				t.Fatalf("redirect_url %q must be rejected", ru)
			}
		})
	}
}

func TestOIDCAuthIsStrictAlways(t *testing.T) {
	// OIDC refuses ANY missing login element on ANY bind: a partially
	// configured Google login is a locked-out console, and locked out must
	// be loud at startup. This strictness also subsumes the non-loopback
	// bind rule for builtin mode.
	validEnv(t)
	t.Setenv("NOISEDECK_AUTH_GOOGLE_CLIENT_ID", "")
	if _, err := Load(""); err == nil || !strings.Contains(err.Error(), "client_id") {
		t.Fatalf("oidc without a client id must refuse, got: %v", err)
	}
}

func TestBuiltinIsDeprecatedOIDCAlias(t *testing.T) {
	validEnv(t)
	t.Setenv("NOISEDECK_AUTH_MODE", "builtin")
	if got := mustLoad(t, "").Auth.Mode; got != "oidc" {
		t.Fatalf("builtin alias normalized to %q, want oidc", got)
	}
}

func TestPasswordModeRejectsOIDCOnlyConfiguration(t *testing.T) {
	for _, key := range []string{
		"NOISEDECK_AUTH_SESSION_SECRET",
		"NOISEDECK_AUTH_GOOGLE_CLIENT_ID",
		"NOISEDECK_AUTH_GOOGLE_CLIENT_SECRET",
		"NOISEDECK_AUTH_GOOGLE_ALLOWED_EMAIL",
	} {
		t.Run(key, func(t *testing.T) {
			defaultPasswordEnv(t)
			t.Setenv("NOISEDECK_AUTH_MODE", "password")
			t.Setenv(key, "stale-oidc-value")
			if _, err := Load(""); err == nil || !strings.Contains(err.Error(), "refuses OIDC-only configuration") {
				t.Fatalf("Load error = %v", err)
			}
		})
	}
}

func TestAuthModeOperationOverrideSupportsPreReceiptOIDCAdoption(t *testing.T) {
	validEnv(t)
	unsetEnv(t, "NOISEDECK_AUTH_MODE")
	if _, err := Load(""); err == nil || !strings.Contains(err.Error(), "refuses OIDC-only configuration") {
		t.Fatalf("ordinary absent-mode load error = %v", err)
	}
	cfg, err := LoadForAuthModeOperation("", "oidc")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Auth.Mode != "oidc" {
		t.Fatalf("operation mode = %q, want oidc", cfg.Auth.Mode)
	}
}

func TestNoneRequiresDeclaredLoopbackOrigins(t *testing.T) {
	validEnv(t)
	t.Setenv("NOISEDECK_AUTH_MODE", "none")
	t.Setenv("NOISEDECK_SERVER_ALLOWED_ORIGINS", "http://127.0.0.1:8837,http://[::1]:8837")
	mustLoad(t, "")

	// The name people actually type counts as loopback too.
	t.Setenv("NOISEDECK_SERVER_ALLOWED_ORIGINS", "http://localhost:8837,http://LocalHost:18082")
	mustLoad(t, "")

	// A name that merely CONTAINS localhost is a public host.
	for _, origin := range []string{"https://console.example.test", "http://localhost.example.test:8837", "http://notlocalhost:8837"} {
		t.Setenv("NOISEDECK_SERVER_ALLOWED_ORIGINS", origin)
		_, err := Load("")
		if err == nil || !strings.Contains(err.Error(), "every server.allowed_origins entry") {
			t.Fatalf("none with public origin %q must fail directly, got: %v", origin, err)
		}
	}
}

// The name is matched byte-wise in ASCII: case variants pass, and nothing
// that merely folds to or contains it does.
func TestIsLoopbackHost(t *testing.T) {
	for host, want := range map[string]bool{
		"localhost": true, "LOCALHOST": true, "LocalHost": true,
		"127.0.0.1": true, "::1": true, "127.9.9.9": true,
		"localhost.":             false, // trailing dot: a different name
		"localhost.example.test": false,
		"notlocalhost":           false,
		"localho\u017ft":         false, // long s: strings.EqualFold accepts this
		"10.0.0.5":               false,
		"":                       false,
	} {
		if got := isLoopbackHost(host); got != want {
			t.Errorf("isLoopbackHost(%q) = %v, want %v", host, got, want)
		}
	}
}

// The OIDC callback follows the same rule: plain http is admitted only on a
// loopback host, and localhost is one.
func TestRedirectURLAdmitsPlainHTTPOnLocalhost(t *testing.T) {
	validEnv(t)
	t.Setenv("NOISEDECK_SERVER_ALLOWED_ORIGINS", "http://localhost:8837")
	t.Setenv("NOISEDECK_AUTH_GOOGLE_REDIRECT_URL", "http://localhost:8837/auth/callback")
	mustLoad(t, "")

	t.Setenv("NOISEDECK_SERVER_ALLOWED_ORIGINS", "http://localhost.example.test:8837")
	t.Setenv("NOISEDECK_AUTH_GOOGLE_REDIRECT_URL", "http://localhost.example.test:8837/auth/callback")
	if _, err := Load(""); err == nil || !strings.Contains(err.Error(), "http only on a loopback host") {
		t.Fatalf("plain-http callback on a public host must fail, got: %v", err)
	}
}

func TestCookieSecurityIsDerivedFromHomogeneousOrigins(t *testing.T) {
	validEnv(t)
	if got := mustLoad(t, "").Auth.SecureCookies; got {
		t.Fatal("plain HTTP origins selected Secure cookies")
	}
	t.Setenv("NOISEDECK_SERVER_ALLOWED_ORIGINS", "https://console.example.test,https://admin.example.test")
	t.Setenv("NOISEDECK_AUTH_GOOGLE_REDIRECT_URL", "https://console.example.test/auth/callback")
	if got := mustLoad(t, "").Auth.SecureCookies; !got {
		t.Fatal("HTTPS origins did not select Secure cookies")
	}
	t.Setenv("NOISEDECK_SERVER_ALLOWED_ORIGINS", "https://console.example.test,http://127.0.0.1:8837")
	if _, err := Load(""); err == nil || !strings.Contains(err.Error(), "homogeneous scheme") {
		t.Fatalf("mixed origin schemes error = %v", err)
	}
}

func TestSecureCookiesHasNoIndependentConfigKey(t *testing.T) {
	path := writeConfig(t, "auth:\n  secure_cookies: true\n")
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "field secure_cookies not found") {
		t.Fatalf("secure_cookies YAML error = %v", err)
	}
	validEnv(t)
	t.Setenv("NOISEDECK_AUTH_SECURE_COOKIES", "true")
	if got := mustLoad(t, "").Auth.SecureCookies; got {
		t.Fatal("retired environment setting overrode HTTP-derived cookies")
	}
}

func TestNonLoopbackBindWithConfiguredBuiltinPasses(t *testing.T) {
	validEnv(t)
	t.Setenv("NOISEDECK_SERVER_BIND", "0.0.0.0:8837")
	t.Setenv("NOISEDECK_SERVER_ALLOWED_ORIGINS", "https://console.example.test")
	// The redirect origin must be a trusted origin, so it moves with
	// allowed_origins.
	t.Setenv("NOISEDECK_AUTH_GOOGLE_REDIRECT_URL", "https://console.example.test/auth/callback")
	t.Setenv("NOISEDECK_AUTH_SECURE_COOKIES", "true")
	mustLoad(t, "")
}

func TestNonLoopbackBindWithTrustedProxy(t *testing.T) {
	validEnv(t)
	t.Setenv("NOISEDECK_SERVER_BIND", "0.0.0.0:8837")
	t.Setenv("NOISEDECK_SERVER_ALLOWED_ORIGINS", "https://console.example.test")
	t.Setenv("NOISEDECK_AUTH_MODE", "trusted_proxy")

	// Proxy mode without addresses is invalid on its own and cannot satisfy
	// the non-loopback rule either.
	if _, err := Load(""); err == nil || !strings.Contains(err.Error(), "trusted_proxy.addresses") {
		t.Fatalf("trusted_proxy without addresses must fail, got: %v", err)
	}

	t.Setenv("NOISEDECK_AUTH_TRUSTED_PROXY_ADDRESSES", "10.0.0.5, 10.0.1.0/24")
	mustLoad(t, "")

	// A header net/http would refuse to send cannot count as configured
	// authentication for a public bind.
	t.Setenv("NOISEDECK_AUTH_TRUSTED_PROXY_IDENTITY_HEADER", ":")
	if _, err := Load(""); err == nil || !strings.Contains(err.Error(), "identity_header") {
		t.Fatalf("invalid header field name must be rejected, got: %v", err)
	}
}

func TestBindHostMustBeIPLiteral(t *testing.T) {
	validEnv(t)
	t.Setenv("NOISEDECK_SERVER_BIND", "localhost:8837")
	if _, err := Load(""); err == nil || !strings.Contains(err.Error(), "IP literal") {
		t.Fatalf("hostname binds must be refused, got: %v", err)
	}
}

func TestOriginValidation(t *testing.T) {
	validEnv(t)
	for _, bad := range []string{
		"ftp://x.test",
		"http://*.example.test",
		"not-a-url",
		// An origin is scheme://host[:port] only; these parse as URLs but
		// could never equal a browser's Origin header.
		"http://x.test/path",
		"http://x.test/",
		"http://x.test?q=1",
		"http://x.test#frag",
		"http://user@x.test",
		// Malformed authorities url.Parse tolerates but browsers never send.
		"http://:80",
		"http://x.test:",
		"http://x.test:99999",
	} {
		t.Setenv("NOISEDECK_SERVER_ALLOWED_ORIGINS", bad)
		if _, err := Load(""); err == nil {
			t.Errorf("origin %q must be rejected", bad)
		}
	}
}

func TestReconcileIntervalBounds(t *testing.T) {
	validEnv(t)
	// Fifteen seconds is the longest permitted period.
	for _, bad := range []string{"500ms", "30s", "2m"} {
		t.Setenv("NOISEDECK_RECONCILER_INTERVAL", bad)
		if _, err := Load(""); err == nil || !strings.Contains(err.Error(), "reconciler.interval") {
			t.Errorf("interval %q must be rejected", bad)
		}
	}
	t.Setenv("NOISEDECK_RECONCILER_INTERVAL", "5s")
	mustLoad(t, "")
}

func TestIPv6OriginAccepted(t *testing.T) {
	validEnv(t)
	t.Setenv("NOISEDECK_SERVER_ALLOWED_ORIGINS", "http://[::1]:8837")
	t.Setenv("NOISEDECK_AUTH_GOOGLE_REDIRECT_URL", "http://[::1]:8837/auth/callback")
	mustLoad(t, "")
}

// A trailing-slash public origin is the classic deployment mistake when the
// callback is built as origin + "/auth/callback": the origin itself stops
// being an origin and the concatenation doubles the slash. Both must fail
// closed at load, and the origin diagnostic must name the slash rather than
// surface as a generic shape error.
func TestTrailingSlashOriginFailsClosed(t *testing.T) {
	validEnv(t)
	t.Setenv("NOISEDECK_SERVER_BIND", "0.0.0.0:8837")
	t.Setenv("NOISEDECK_AUTH_SECURE_COOKIES", "true")
	t.Setenv("NOISEDECK_SERVER_ALLOWED_ORIGINS", "https://console.example.test/")
	t.Setenv("NOISEDECK_AUTH_GOOGLE_REDIRECT_URL", "https://console.example.test//auth/callback")
	_, err := Load("")
	if err == nil {
		t.Fatal("a trailing-slash origin must fail closed")
	}
	if !strings.Contains(err.Error(), "trailing slash") {
		t.Errorf("origin diagnostic must name the trailing slash, got: %v", err)
	}
	if !strings.Contains(err.Error(), "redirect_url") {
		t.Errorf("the doubled-slash callback must also be rejected, got: %v", err)
	}
}

func TestEmptyProjectRootRejected(t *testing.T) {
	p := writeConfig(t, `
projects:
  allowed_roots: [""]
`)
	if _, err := Load(p); err == nil || !strings.Contains(err.Error(), "non-empty absolute path") {
		t.Fatalf("an empty allowed root must be rejected, got: %v", err)
	}
}

func TestMultiDocumentYAMLRejected(t *testing.T) {
	validEnv(t)
	// A second document would be silently ignored by a single Decode,
	// reopening both unknown-key protection and the env-only secret rule.
	p := writeConfig(t, `
server:
  bind: "127.0.0.1:9000"
---
auth:
  session_secret: "smuggled"
`)
	if _, err := Load(p); err == nil || !strings.Contains(err.Error(), "more than one YAML document") {
		t.Fatalf("multi-document config must be rejected, got: %v", err)
	}
}

func TestClaudeHookConfigRejected(t *testing.T) {
	validEnv(t)
	p := writeConfig(t, `
providers:
  claude:
    hook_config: "~/.claude/hooks.json"
`)
	if _, err := Load(p); err == nil || !strings.Contains(err.Error(), "providers.claude.hook_config") {
		t.Fatalf("claude hook_config must be rejected — no such mechanism exists, got: %v", err)
	}
}

func TestSessionPrefixRejectsTargetSyntax(t *testing.T) {
	validEnv(t)
	t.Setenv("NOISEDECK_TMUX_SESSION_PREFIX", "ac:")
	if _, err := Load(""); err == nil {
		t.Fatal("prefix containing ':' must be rejected — it is tmux target syntax")
	}
}

func TestConfiguredHookProviderRequiresHookConfig(t *testing.T) {
	validEnv(t)
	t.Setenv("NOISEDECK_PROVIDERS_KIMI_EXECUTABLE", "/usr/local/bin/kimi")
	t.Setenv("NOISEDECK_PROVIDERS_KIMI_HOOK_CONFIG", "")
	if _, err := Load(""); err == nil || !strings.Contains(err.Error(), "providers.kimi.hook_config") {
		t.Fatalf("a configured kimi without hook_config must fail, got: %v", err)
	}
}

func TestHookConfigTildeExpansion(t *testing.T) {
	validEnv(t)
	t.Setenv("NOISEDECK_PROVIDERS_CODEX_EXECUTABLE", "/usr/local/bin/codex")
	cfg := mustLoad(t, "")

	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, ".codex", "hooks.json")
	if cfg.Providers.Codex.HookConfig != want {
		t.Errorf("hook config = %q, want %q", cfg.Providers.Codex.HookConfig, want)
	}
}

func TestLockPathDerivedBesideDatabase(t *testing.T) {
	validEnv(t)
	dir := t.TempDir()
	t.Setenv("NOISEDECK_DATABASE_PATH", filepath.Join(dir, "control.db"))
	cfg := mustLoad(t, "")
	if want := filepath.Join(dir, "noisedeck.lock"); cfg.Database.LockPath != want {
		t.Errorf("lock path = %q, want %q", cfg.Database.LockPath, want)
	}

	// An explicit lock path is honored.
	explicit := filepath.Join(dir, "elsewhere.lock")
	t.Setenv("NOISEDECK_DATABASE_LOCK_PATH", explicit)
	cfg = mustLoad(t, "")
	if cfg.Database.LockPath != explicit {
		t.Errorf("lock path = %q, want %q", cfg.Database.LockPath, explicit)
	}
}

func TestMissingNamedFileIsAnError(t *testing.T) {
	validEnv(t)
	if _, err := Load(filepath.Join(t.TempDir(), "absent.yaml")); err == nil {
		t.Fatal("an explicitly named config file that cannot be read must fail")
	}
}

func TestInvalidDurationEnv(t *testing.T) {
	validEnv(t)
	t.Setenv("NOISEDECK_DATABASE_BUSY_TIMEOUT", "banana")
	if _, err := Load(""); err == nil {
		t.Fatal("unparseable duration env must fail")
	}
}

func TestValidationCollectsAllErrors(t *testing.T) {
	// One load, several violations: every one must be reported so you
	// fix the config in one round trip.
	t.Setenv("NOISEDECK_SERVER_BIND", "localhost:0")
	t.Setenv("NOISEDECK_LOGGING_LEVEL", "chatty")
	t.Setenv("NOISEDECK_LOGGING_FORMAT", "xml")
	_, err := Load("")
	if err == nil {
		t.Fatal("expected errors")
	}
	for _, want := range []string{"server.bind", "logging.level", "logging.format", "projects.allowed_roots"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error must mention %s; got:\n%v", want, err)
		}
	}
}

func TestLoopbackBaseHonorsTheBindHost(t *testing.T) {
	cases := map[string]string{
		"127.0.0.1:8837": "http://127.0.0.1:8837",
		"0.0.0.0:8837":   "http://127.0.0.1:8837",
		"[::1]:8837":     "http://[::1]:8837",
		"[::]:8837":      "http://[::1]:8837",
		"10.0.0.5:9000":  "http://10.0.0.5:9000",
	}
	for bind, want := range cases {
		if got := (Server{Bind: bind}).LoopbackBase(); got != want {
			t.Errorf("LoopbackBase(%q) = %q, want %q", bind, got, want)
		}
	}
}

func TestShellHookConfigRejected(t *testing.T) {
	validEnv(t)
	t.Setenv("NOISEDECK_PROVIDERS_SHELL_EXECUTABLE", "/bin/bash")
	p := writeConfig(t, `
providers:
  shell:
    hook_config: "~/.bash_hooks.json"
`)
	if _, err := Load(p); err == nil || !strings.Contains(err.Error(), "providers.shell.hook_config") {
		t.Fatalf("shell hook_config must be rejected — identity-none has no hook, got: %v", err)
	}
}

// Every provider executable env binding must still bind: an edit that
// inserts a new binding has been measured to replace its neighbor, and the
// neighbor's override then dies silently. Sentinel values make each missing
// binding fail by name.
func TestEveryProviderExecutableEnvBindingStillBinds(t *testing.T) {
	validEnv(t)
	executables := map[string]string{
		"NOISEDECK_PROVIDERS_CLAUDE_EXECUTABLE":       "/sentinel/claude",
		"NOISEDECK_PROVIDERS_CLINE_EXECUTABLE":        "/sentinel/cline",
		"NOISEDECK_PROVIDERS_COMMAND_CODE_EXECUTABLE": "/sentinel/command-code",
		"NOISEDECK_PROVIDERS_CODEX_EXECUTABLE":        "/sentinel/codex",
		"NOISEDECK_PROVIDERS_CURSOR_EXECUTABLE":       "/sentinel/cursor",
		"NOISEDECK_PROVIDERS_DEVIN_EXECUTABLE":        "/sentinel/devin",
		"NOISEDECK_PROVIDERS_GROK_EXECUTABLE":         "/sentinel/grok",
		"NOISEDECK_PROVIDERS_KIMI_EXECUTABLE":         "/sentinel/kimi",
		"NOISEDECK_PROVIDERS_MIMO_EXECUTABLE":         "/sentinel/mimo",
		"NOISEDECK_PROVIDERS_OMP_EXECUTABLE":          "/sentinel/omp",
		"NOISEDECK_PROVIDERS_OPENCODE_EXECUTABLE":     "/sentinel/opencode",
		"NOISEDECK_PROVIDERS_PI_EXECUTABLE":           "/sentinel/pi",
		"NOISEDECK_PROVIDERS_QODER_EXECUTABLE":        "/sentinel/qoder",
		"NOISEDECK_PROVIDERS_QWEN_EXECUTABLE":         "/sentinel/qwen",
		"NOISEDECK_PROVIDERS_SHELL_EXECUTABLE":        "/sentinel/bash",
	}
	hooks := map[string]string{
		"NOISEDECK_PROVIDERS_COMMAND_CODE_HOOK_CONFIG": "/sentinel/commandcode-hooks",
		"NOISEDECK_PROVIDERS_CODEX_HOOK_CONFIG":        "/sentinel/codex-hooks",
		"NOISEDECK_PROVIDERS_DEVIN_HOOK_CONFIG":        "/sentinel/devin-hooks",
		"NOISEDECK_PROVIDERS_KIMI_HOOK_CONFIG":         "/sentinel/kimi-hooks",
		"NOISEDECK_PROVIDERS_OMP_HOOK_CONFIG":          "/sentinel/omp-hook",
		"NOISEDECK_PROVIDERS_OPENCODE_HOOK_CONFIG":     "/sentinel/opencode-plugin",
		"NOISEDECK_PROVIDERS_PI_HOOK_CONFIG":           "/sentinel/pi-hook",
	}
	for name, value := range executables {
		t.Setenv(name, value)
	}
	for name, value := range hooks {
		t.Setenv(name, value)
	}
	cfg := mustLoad(t, "")
	for env, want := range executables {
		got := cfg.providerEnvField(env)
		if got == nil || *got != want {
			t.Errorf("%s binds %v, want %q — the env binding was dropped or retargeted", env, got, want)
		}
	}
	for env, want := range hooks {
		got := cfg.providerEnvField(env)
		if got == nil || *got != want {
			t.Errorf("%s binds %v, want %q — the env binding was dropped or retargeted", env, got, want)
		}
	}
}

func (c *Config) providerEnvField(env string) *string {
	fields := map[string]*string{
		"NOISEDECK_PROVIDERS_CLAUDE_EXECUTABLE":        &c.Providers.Claude.Executable,
		"NOISEDECK_PROVIDERS_CLINE_EXECUTABLE":         &c.Providers.Cline.Executable,
		"NOISEDECK_PROVIDERS_COMMAND_CODE_EXECUTABLE":  &c.Providers.CommandCode.Executable,
		"NOISEDECK_PROVIDERS_CODEX_EXECUTABLE":         &c.Providers.Codex.Executable,
		"NOISEDECK_PROVIDERS_CURSOR_EXECUTABLE":        &c.Providers.Cursor.Executable,
		"NOISEDECK_PROVIDERS_DEVIN_EXECUTABLE":         &c.Providers.Devin.Executable,
		"NOISEDECK_PROVIDERS_GROK_EXECUTABLE":          &c.Providers.Grok.Executable,
		"NOISEDECK_PROVIDERS_KIMI_EXECUTABLE":          &c.Providers.Kimi.Executable,
		"NOISEDECK_PROVIDERS_MIMO_EXECUTABLE":          &c.Providers.Mimo.Executable,
		"NOISEDECK_PROVIDERS_OMP_EXECUTABLE":           &c.Providers.Omp.Executable,
		"NOISEDECK_PROVIDERS_OPENCODE_EXECUTABLE":      &c.Providers.Opencode.Executable,
		"NOISEDECK_PROVIDERS_PI_EXECUTABLE":            &c.Providers.Pi.Executable,
		"NOISEDECK_PROVIDERS_QODER_EXECUTABLE":         &c.Providers.Qoder.Executable,
		"NOISEDECK_PROVIDERS_QWEN_EXECUTABLE":          &c.Providers.Qwen.Executable,
		"NOISEDECK_PROVIDERS_SHELL_EXECUTABLE":         &c.Providers.Shell.Executable,
		"NOISEDECK_PROVIDERS_COMMAND_CODE_HOOK_CONFIG": &c.Providers.CommandCode.HookConfig,
		"NOISEDECK_PROVIDERS_CODEX_HOOK_CONFIG":        &c.Providers.Codex.HookConfig,
		"NOISEDECK_PROVIDERS_DEVIN_HOOK_CONFIG":        &c.Providers.Devin.HookConfig,
		"NOISEDECK_PROVIDERS_KIMI_HOOK_CONFIG":         &c.Providers.Kimi.HookConfig,
		"NOISEDECK_PROVIDERS_OMP_HOOK_CONFIG":          &c.Providers.Omp.HookConfig,
		"NOISEDECK_PROVIDERS_OPENCODE_HOOK_CONFIG":     &c.Providers.Opencode.HookConfig,
		"NOISEDECK_PROVIDERS_PI_HOOK_CONFIG":           &c.Providers.Pi.HookConfig,
	}
	return fields[env]
}

func TestPiExecutableAndHookPolicy(t *testing.T) {
	validEnv(t)
	t.Setenv("NOISEDECK_PROVIDERS_PI_EXECUTABLE", "/usr/local/bin/pi")
	t.Setenv("NOISEDECK_PROVIDERS_PI_HOOK_CONFIG", "")
	if _, err := Load(""); err == nil || !strings.Contains(err.Error(), "providers.pi.hook_config") {
		t.Fatalf("a configured pi without hook_config must fail, got: %v", err)
	}

	t.Setenv("NOISEDECK_PROVIDERS_PI_HOOK_CONFIG", "/home/claude/.pi/noisedeck-provider-hook.js")
	cfg := mustLoad(t, "")
	if cfg.Providers.Pi.Executable != "/usr/local/bin/pi" {
		t.Fatalf("pi executable = %q", cfg.Providers.Pi.Executable)
	}
	if cfg.Providers.Pi.HookConfig != "/home/claude/.pi/noisedeck-provider-hook.js" {
		t.Fatalf("pi hook config = %q", cfg.Providers.Pi.HookConfig)
	}
}

func TestClineExecutableAndHookPolicy(t *testing.T) {
	validEnv(t)
	t.Setenv("NOISEDECK_PROVIDERS_CLINE_EXECUTABLE", "/opt/cline")
	cfg := mustLoad(t, "")
	if cfg.Providers.Cline.Executable != "/opt/cline" {
		t.Fatalf("cline executable = %q", cfg.Providers.Cline.Executable)
	}
	p := writeConfig(t, `
providers:
  cline:
    hook_config: "~/.cline/hooks/TaskStart"
`)
	if _, err := Load(p); err == nil || !strings.Contains(err.Error(), "providers.cline.hook_config") {
		t.Fatalf("fresh-only Cline must reject identity hook config, got: %v", err)
	}
}

func TestShellExecutableResolvesAsBash(t *testing.T) {
	validEnv(t)
	cfg := mustLoad(t, "")
	if cfg.Providers.Shell.Executable == "" {
		t.Fatal("unset shell executable must resolve bash from PATH")
	}
	if filepath.Base(cfg.Providers.Shell.Executable) != "bash" {
		t.Fatalf("shell executable = %q, want the bash binary (the provider name is shell, the binary is bash)", cfg.Providers.Shell.Executable)
	}
}

func TestConfiguredOpencodeRequiresHookConfig(t *testing.T) {
	validEnv(t)
	t.Setenv("NOISEDECK_PROVIDERS_OPENCODE_EXECUTABLE", "/usr/local/bin/opencode")
	t.Setenv("NOISEDECK_PROVIDERS_OPENCODE_HOOK_CONFIG", "")
	if _, err := Load(""); err == nil || !strings.Contains(err.Error(), "providers.opencode.hook_config") {
		t.Fatalf("a configured opencode without hook_config must fail, got: %v", err)
	}
}

// Which harnesses a Deck image contains is a property of that image, so
// every provider executable resolves from PATH at load. The lookup name is
// not always the provider's own name — Cursor ships as cursor-agent, the
// shell as bash — and a mapping that lost one of those would silently
// unregister a harness the full image does contain.
func TestProviderExecutablesResolveFromPathByBinaryName(t *testing.T) {
	dir := t.TempDir()
	binaries := map[string]func(Providers) Provider{
		"claude":       func(p Providers) Provider { return p.Claude },
		"cline":        func(p Providers) Provider { return p.Cline },
		"command-code": func(p Providers) Provider { return p.CommandCode },
		"codex":        func(p Providers) Provider { return p.Codex },
		"cursor-agent": func(p Providers) Provider { return p.Cursor },
		"devin":        func(p Providers) Provider { return p.Devin },
		"grok":         func(p Providers) Provider { return p.Grok },
		"kimi":         func(p Providers) Provider { return p.Kimi },
		"kilo":         func(p Providers) Provider { return p.Kilo },
		"mimo":         func(p Providers) Provider { return p.Mimo },
		"omp":          func(p Providers) Provider { return p.Omp },
		"opencode":     func(p Providers) Provider { return p.Opencode },
		"pi":           func(p Providers) Provider { return p.Pi },
		"qoder":        func(p Providers) Provider { return p.Qoder },
		"qwen":         func(p Providers) Provider { return p.Qwen },
		"bash":         func(p Providers) Provider { return p.Shell },
	}
	for binary := range binaries {
		path := filepath.Join(dir, binary)
		if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir)
	validEnv(t)
	cfg := mustLoad(t, "")

	for binary, read := range binaries {
		want := filepath.Join(dir, binary)
		if got := read(cfg.Providers).Executable; got != want {
			t.Errorf("%s resolved to %q, want %q", binary, got, want)
		}
	}

	// The other half of the contract: nothing on PATH, nothing resolved.
	empty := t.TempDir()
	t.Setenv("PATH", empty)
	bare := mustLoad(t, "")
	for binary, read := range binaries {
		if got := read(bare.Providers).Executable; got != "" {
			t.Errorf("%s resolved to %q with an empty PATH", binary, got)
		}
	}
}
