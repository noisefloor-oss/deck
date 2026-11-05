package config

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/noisefloor-oss/deck/internal/provider"
)

// Duration is a time.Duration that unmarshals from YAML strings like "15s".
type Duration time.Duration

// UnmarshalYAML implements yaml.Unmarshaler.
func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err != nil {
		return err
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	*d = Duration(v)
	return nil
}

// Std returns the standard-library value.
func (d Duration) Std() time.Duration { return time.Duration(d) }

// Config is the full backend configuration. Field layout mirrors
// configs/config.example.yaml exactly; see doc.go for precedence and the
// environment-variable naming rule.
type Config struct {
	Server          Server          `yaml:"server"`
	Auth            Auth            `yaml:"auth"`
	HarnessLogin    HarnessLogin    `yaml:"harness_login"`
	Database        Database        `yaml:"database"`
	Tmux            Tmux            `yaml:"tmux"`
	Projects        Projects        `yaml:"projects"`
	ProjectPreviews ProjectPreviews `yaml:"project_previews"`
	Reconciler      Reconciler      `yaml:"reconciler"`
	Taskmaster      Taskmaster      `yaml:"taskmaster"`
	Chats           Chats           `yaml:"chats"`
	Providers       Providers       `yaml:"providers"`
	Experiments     Experiments     `yaml:"experiments"`
	Sealed          Sealed          `yaml:"sealed"`
	Logging         Logging         `yaml:"logging"`
}

// HarnessLogin is local callback transport for a single-user Deck instance.
type HarnessLogin struct {
	Enabled              bool   `yaml:"enabled"`
	TrustedProxyIdentity string `yaml:"trusted_proxy_identity"`
}

type Server struct {
	Bind           string   `yaml:"bind"`
	UnixSocket     string   `yaml:"unix_socket"`
	GatewayUID     int      `yaml:"gateway_uid"`
	AllowedOrigins []string `yaml:"allowed_origins"`
}

// LoopbackBase is this service's own address as a provider hook on the
// same host reaches it: the configured bind's own host when it
// is loopback or a specific address, and the wildcard's loopback
// equivalent otherwise — a hook dialing 127.0.0.1 when the service binds
// [::1] or a specific address would never connect. Bind was already
// validated as host:port with an IP literal.
func (s Server) LoopbackBase() string {
	if s.UnixSocket != "" {
		return "http://localhost"
	}
	host, port, err := net.SplitHostPort(s.Bind)
	if err != nil {
		// Validate rejects anything else before this is reachable.
		return "http://127.0.0.1:8837"
	}
	switch host {
	case "", "0.0.0.0":
		host = "127.0.0.1"
	case "::":
		host = "::1"
	}
	return "http://" + net.JoinHostPort(host, port)
}

type Auth struct {
	// Mode is "none", "password", "oidc", or "trusted_proxy". "builtin"
	// remains a deprecated input alias for oidc during deployment migration.
	Mode         string       `yaml:"mode"`
	Google       GoogleOIDC   `yaml:"google"`
	TrustedProxy TrustedProxy `yaml:"trusted_proxy"`
	// SessionSecret intentionally has no YAML key — env only (doc.go).
	SessionSecret string `yaml:"-"`
	// PasswordHash is an env-only pin. Without one, you create your password
	// through a one-time setup link.
	PasswordHash string `yaml:"-"`
	// SecureCookies is derived from the homogeneous allowed-origin scheme.
	// It has no independent configuration surface.
	SecureCookies bool `yaml:"-"`
}

// GoogleOIDC configures the single-user Google login (oidc mode).
type GoogleOIDC struct {
	ClientID string `yaml:"client_id"`
	// ClientSecret intentionally has no YAML key — env only, like the
	// session secret (NOISEDECK_AUTH_GOOGLE_CLIENT_SECRET).
	ClientSecret string `yaml:"-"`
	// RedirectURL is the EXACT registered callback, ending /auth/callback.
	// Nothing about it is ever derived from request headers.
	RedirectURL string `yaml:"redirect_url"`
	// AllowedEmail is the verified Google email of the one user. Exactly one
	// account signs in; adding more is a recorded decision, not a config
	// edit pattern.
	AllowedEmail string `yaml:"allowed_email"`
}

type TrustedProxy struct {
	Addresses      []string `yaml:"addresses"`
	IdentityHeader string   `yaml:"identity_header"`
}

type Database struct {
	Path string `yaml:"path"`
	// LockPath is the exclusive backend lock (internal/lock). Empty
	// derives "noisedeck.lock" beside Path, so the lock and the database
	// it guards live on the same filesystem by default.
	LockPath    string   `yaml:"lock_path"`
	BusyTimeout Duration `yaml:"busy_timeout"`
}

type Tmux struct {
	SocketPath      string `yaml:"socket_path"`
	SessionPrefix   string `yaml:"session_prefix"`
	ScrollbackLines int    `yaml:"scrollback_lines"`
}

type Projects struct {
	AllowedRoots []string `yaml:"allowed_roots"`
}

type Reconciler struct {
	Interval Duration `yaml:"interval"`
}

type Taskmaster struct {
	Executable string `yaml:"executable"`
}

// Chats names the a2a client the console runs to read the chat ledger.
// Read-only: the console runs `a2a ledger` and nothing else.
type Chats struct {
	Executable string `yaml:"executable"`
}

type Providers struct {
	Claude      Provider `yaml:"claude"`
	Cline       Provider `yaml:"cline"`
	CommandCode Provider `yaml:"command_code"`
	Codex       Provider `yaml:"codex"`
	Cursor      Provider `yaml:"cursor"`
	Devin       Provider `yaml:"devin"`
	Grok        Provider `yaml:"grok"`
	Kimi        Provider `yaml:"kimi"`
	Kilo        Provider `yaml:"kilo"`
	Mimo        Provider `yaml:"mimo"`
	Omp         Provider `yaml:"omp"`
	Opencode    Provider `yaml:"opencode"`
	Pi          Provider `yaml:"pi"`
	Qoder       Provider `yaml:"qoder"`
	Qwen        Provider `yaml:"qwen"`
	Shell       Provider `yaml:"shell"`
}

type Provider struct {
	Executable string `yaml:"executable"`
	// HookConfig is the provider config file carrying the identity hook —
	// the hooks file for Codex/Kimi's SessionStart entries, or a plugin file
	// for a session.created subscription. Provisioning installs
	// the hook. Unused by Claude, whose
	// identity is assigned rather than retrieved.
	HookConfig string `yaml:"hook_config"`
	// HookReceiver is the provisioned capture receiver the hook must invoke
	// (the image's provider-hook path by default). It is deployment
	// configuration, so it must match what provisioning installed.
	// Codex/Kimi only;
	// plugin-backed providers POST directly and leave this unset.
	HookReceiver string `yaml:"hook_receiver"`
}

type Experiments struct {
	// UnconfinedCapacityRouting enables the loopback capacity-routing experiment. It
	// is false by default and never represents a security boundary.
	UnconfinedCapacityRouting bool `yaml:"unconfined_capacity_routing"`
	// FontBench serves the font-substitution bench at /bench.html, behind the
	// ordinary browser authentication. It is false by default; off, the route
	// 404s exactly like any unknown path.
	FontBench bool `yaml:"font_bench"`
	// DevPreview reverse-proxies a locally running Vite dev server at
	// /__dev/, behind the ordinary browser authentication, so a frontend
	// change can be seen on a running deployment without rebuilding the
	// embedded assets and replacing the container. It is false by default;
	// off, the route 404s exactly like any unknown path.
	DevPreview bool `yaml:"dev_preview"`
	// DevPreviewUpstream is the dev server the preview route forwards to. It
	// must be a loopback host:port: the route runs with your live
	// authority, so it may only ever reach a process on this host.
	DevPreviewUpstream string `yaml:"dev_preview_upstream"`
}

// Sealed is the encrypted-state posture. With
// RequireEncryptedState set, boot refuses to serve unless every store Deck
// writes to is backed by encrypted storage the container can verify itself
// or through the host preflight's launch-bound report. Unset, the same check
// runs and only logs. Profile names the storage layout: "shared-host" (the
// default, gocryptfs owned by the partner uid, /tmp on disk under it) or
// "own-host" (whole-disk LUKS, tmpfs /tmp allowed). SwapPolicy independently
// permits ordinary swap by default or requires encrypted swap in paranoid mode.
type Sealed struct {
	RequireEncryptedState bool   `yaml:"require_encrypted_state"`
	Profile               string `yaml:"profile"`
	SwapPolicy            string `yaml:"swap_policy"`
	// ReportPath is where the compose wrapper bind-mounts the host report.
	ReportPath string `yaml:"report_path"`
}

type Logging struct {
	Level  string `yaml:"level"`
	Format string `yaml:"format"`
}

// Defaults returns the compiled-in configuration. It is not valid on its
// own: project roots have no safe default and must be configured.
func Defaults() Config {
	return Config{
		Server: Server{
			Bind: "127.0.0.1:8837",
			// Both names a person types for this machine; an allowlist of the
			// IP alone refuses every write from http://localhost.
			AllowedOrigins: []string{"http://127.0.0.1:8837", "http://localhost:8837"},
		},
		HarnessLogin: HarnessLogin{Enabled: true},
		Auth: Auth{
			Mode:         "password",
			TrustedProxy: TrustedProxy{IdentityHeader: "X-Forwarded-User"},
		},
		Database: Database{
			Path: "./run/noisedeck.db",
			// Matches internal/db DefaultBusyTimeout; duplicated rather than
			// imported so config does not pull in the cgo SQLite driver.
			BusyTimeout: Duration(5 * time.Second),
		},
		Tmux: Tmux{
			SocketPath:      "./run/tmux/noisedeck.sock",
			SessionPrefix:   "noisedeck-",
			ScrollbackLines: 2000,
		},
		Reconciler: Reconciler{Interval: Duration(15 * time.Second)},
		Taskmaster: Taskmaster{Executable: "/usr/local/bin/noise"},
		Chats:      Chats{Executable: "/usr/local/bin/a2a"},
		// No provider carries a default executable. Deck ships in more
		// than one image, and which harnesses an image contains is a
		// property of that image, so every executable resolves from PATH
		// at load. A hardcoded default would advertise a harness in an
		// image that does not have it.
		Providers: Providers{
			CommandCode: Provider{HookConfig: "~/.commandcode/settings.json", HookReceiver: provider.HookReceiverPath},
			Codex:       Provider{HookConfig: "~/.codex/hooks.json", HookReceiver: provider.CodexIdentityHookPath},
			Devin:       Provider{HookConfig: "~/.config/devin/config.json", HookReceiver: provider.HookReceiverPath},
			Kimi:        Provider{HookConfig: "~/.kimi-code/config.toml", HookReceiver: provider.HookReceiverPath},
			Kilo:        Provider{HookConfig: "~/.config/kilo/plugin/noisedeck-provider-hook.js"},
			Omp:         Provider{HookConfig: "~/.omp/noisedeck-provider-hook.ts"},
			Pi:          Provider{HookConfig: "~/.pi/noisedeck-provider-hook.js"},
			Opencode:    Provider{HookConfig: "~/.config/opencode/plugins/noisedeck-provider-hook.js"},
		},
		Sealed:  Sealed{Profile: "shared-host", ReportPath: "/run/noisedeck/sealed-state/report.json"},
		Logging: Logging{Level: "info", Format: "json"},
		Experiments: Experiments{
			// The Vite dev server's own fixed default (web/vite.config.ts
			// sets strictPort, so it is this port or nothing). Only
			// consulted when the dev-preview experiment is on.
			DevPreviewUpstream: "127.0.0.1:5173",
		},
	}
}

// Load builds the effective configuration: defaults, then the YAML file at
// path (skipped when path is empty), then NOISEDECK_* environment variables, then
// normalization and validation. A named file that cannot be read is an
// error — an explicitly configured file that silently does not apply would
// be a debugging trap.
func Load(path string) (*Config, error) {
	return load(path, "")
}

// LoadForAuthModeOperation loads the same effective configuration as Load but
// makes your explicit offline auth-mode command authoritative over
// an absent or stale deployment environment value. All target-mode validation
// still applies.
func LoadForAuthModeOperation(path, mode string) (*Config, error) {
	return load(path, mode)
}

func load(path, authModeOverride string) (*Config, error) {
	cfg := Defaults()

	if path != "" {
		raw, err := os.ReadFile(path) // #nosec G304 -- the -config flag is supplied by the person running Deck; reading it is the point.
		if err != nil {
			return nil, fmt.Errorf("config: read %q: %w", path, err)
		}
		dec := yaml.NewDecoder(strings.NewReader(string(raw)))
		// Unknown keys are typos; fail rather than silently defaulting.
		dec.KnownFields(true)
		if err := dec.Decode(&cfg); err != nil {
			return nil, fmt.Errorf("config: parse %q: %w", path, err)
		}
		// Exactly one document. A second `---` document would be silently
		// ignored by a single Decode, which reopens both the unknown-key
		// protection and the env-only-secret guarantee through the back door.
		var extra any
		if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("config: %q: contains more than one YAML document; only the first would apply", path)
		}
	}

	if err := applyEnv(&cfg); err != nil {
		return nil, err
	}
	if authModeOverride != "" {
		cfg.Auth.Mode = authModeOverride
	}
	if err := normalize(&cfg); err != nil {
		return nil, err
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("config: invalid:\n%w", err)
	}
	return &cfg, nil
}

// applyEnv overlays NOISEDECK_* variables. Names follow the YAML path (doc.go).
func applyEnv(c *Config) error {
	var errs []error

	str := func(key string, dst *string) {
		if v, ok := os.LookupEnv(key); ok {
			*dst = v
		}
	}
	list := func(key string, dst *[]string) {
		if v, ok := os.LookupEnv(key); ok {
			var out []string
			for _, item := range strings.Split(v, ",") {
				if item = strings.TrimSpace(item); item != "" {
					out = append(out, item)
				}
			}
			*dst = out
		}
	}
	dur := func(key string, dst *Duration) {
		if v, ok := os.LookupEnv(key); ok {
			parsed, err := time.ParseDuration(v)
			if err != nil {
				errs = append(errs, fmt.Errorf("config: %s: invalid duration %q", key, v))
				return
			}
			*dst = Duration(parsed)
		}
	}
	num := func(key string, dst *int) {
		if v, ok := os.LookupEnv(key); ok {
			parsed, err := strconv.Atoi(v)
			if err != nil {
				errs = append(errs, fmt.Errorf("config: %s: invalid integer %q", key, v))
				return
			}
			*dst = parsed
		}
	}
	boolean := func(key string, dst *bool) {
		if v, ok := os.LookupEnv(key); ok {
			parsed, err := strconv.ParseBool(v)
			if err != nil {
				errs = append(errs, fmt.Errorf("config: %s: invalid boolean %q", key, v))
				return
			}
			*dst = parsed
		}
	}

	str("NOISEDECK_SERVER_BIND", &c.Server.Bind)
	str("NOISEDECK_SERVER_UNIX_SOCKET", &c.Server.UnixSocket)
	num("NOISEDECK_SERVER_GATEWAY_UID", &c.Server.GatewayUID)
	boolean("NOISEDECK_PROJECT_PREVIEWS_LOOPBACK", &c.ProjectPreviews.Loopback)
	num("NOISEDECK_PROJECT_PREVIEWS_LOOPBACK_PORT", &c.ProjectPreviews.LoopbackPort)
	str("NOISEDECK_PROJECT_PREVIEWS_BIND", &c.ProjectPreviews.Bind)
	str("NOISEDECK_PROJECT_PREVIEWS_METADATA_SOCKET", &c.ProjectPreviews.MetadataSocket)
	str("NOISEDECK_PROJECT_PREVIEWS_DECK_ORIGIN", &c.ProjectPreviews.DeckOrigin)
	boolean("NOISEDECK_PROJECT_PREVIEWS_UNCONFINED", &c.ProjectPreviews.Unconfined)
	str("NOISEDECK_PROJECT_PREVIEWS_BASE_DOMAIN", &c.ProjectPreviews.BaseDomain)
	str("NOISEDECK_PROJECT_PREVIEWS_HOSTNAME_PREFIX", &c.ProjectPreviews.HostnamePrefix)
	num("NOISEDECK_PROJECT_PREVIEWS_PORT_MIN", &c.ProjectPreviews.PortMin)
	num("NOISEDECK_PROJECT_PREVIEWS_PORT_MAX", &c.ProjectPreviews.PortMax)
	if v, ok := os.LookupEnv("NOISEDECK_PROJECT_PREVIEWS_TARGETS"); ok {
		if err := decodePreviewTargets(v, &c.ProjectPreviews.Targets); err != nil {
			errs = append(errs, err)
		}
	}
	list("NOISEDECK_SERVER_ALLOWED_ORIGINS", &c.Server.AllowedOrigins)
	str("NOISEDECK_AUTH_MODE", &c.Auth.Mode)
	boolean("NOISEDECK_HARNESS_LOGIN_ENABLED", &c.HarnessLogin.Enabled)
	str("NOISEDECK_HARNESS_LOGIN_TRUSTED_PROXY_IDENTITY", &c.HarnessLogin.TrustedProxyIdentity)
	list("NOISEDECK_AUTH_TRUSTED_PROXY_ADDRESSES", &c.Auth.TrustedProxy.Addresses)
	str("NOISEDECK_AUTH_TRUSTED_PROXY_IDENTITY_HEADER", &c.Auth.TrustedProxy.IdentityHeader)
	str("NOISEDECK_AUTH_SESSION_SECRET", &c.Auth.SessionSecret)
	str("NOISEDECK_AUTH_PASSWORD_HASH", &c.Auth.PasswordHash)
	str("NOISEDECK_AUTH_GOOGLE_CLIENT_ID", &c.Auth.Google.ClientID)
	str("NOISEDECK_AUTH_GOOGLE_CLIENT_SECRET", &c.Auth.Google.ClientSecret)
	str("NOISEDECK_AUTH_GOOGLE_REDIRECT_URL", &c.Auth.Google.RedirectURL)
	str("NOISEDECK_AUTH_GOOGLE_ALLOWED_EMAIL", &c.Auth.Google.AllowedEmail)
	str("NOISEDECK_DATABASE_PATH", &c.Database.Path)
	str("NOISEDECK_DATABASE_LOCK_PATH", &c.Database.LockPath)
	dur("NOISEDECK_DATABASE_BUSY_TIMEOUT", &c.Database.BusyTimeout)
	str("NOISEDECK_TMUX_SOCKET_PATH", &c.Tmux.SocketPath)
	str("NOISEDECK_TMUX_SESSION_PREFIX", &c.Tmux.SessionPrefix)
	num("NOISEDECK_TMUX_SCROLLBACK_LINES", &c.Tmux.ScrollbackLines)
	list("NOISEDECK_PROJECTS_ALLOWED_ROOTS", &c.Projects.AllowedRoots)
	dur("NOISEDECK_RECONCILER_INTERVAL", &c.Reconciler.Interval)
	str("NOISEDECK_TASKMASTER_EXECUTABLE", &c.Taskmaster.Executable)
	str("NOISEDECK_CHATS_EXECUTABLE", &c.Chats.Executable)
	str("NOISEDECK_PROVIDERS_CLAUDE_EXECUTABLE", &c.Providers.Claude.Executable)
	str("NOISEDECK_PROVIDERS_CLINE_EXECUTABLE", &c.Providers.Cline.Executable)
	str("NOISEDECK_PROVIDERS_COMMAND_CODE_EXECUTABLE", &c.Providers.CommandCode.Executable)
	str("NOISEDECK_PROVIDERS_COMMAND_CODE_HOOK_CONFIG", &c.Providers.CommandCode.HookConfig)
	str("NOISEDECK_PROVIDERS_COMMAND_CODE_HOOK_RECEIVER", &c.Providers.CommandCode.HookReceiver)
	str("NOISEDECK_PROVIDERS_CODEX_EXECUTABLE", &c.Providers.Codex.Executable)
	str("NOISEDECK_PROVIDERS_CURSOR_EXECUTABLE", &c.Providers.Cursor.Executable)
	str("NOISEDECK_PROVIDERS_CODEX_HOOK_CONFIG", &c.Providers.Codex.HookConfig)
	str("NOISEDECK_PROVIDERS_CODEX_HOOK_RECEIVER", &c.Providers.Codex.HookReceiver)
	str("NOISEDECK_PROVIDERS_DEVIN_EXECUTABLE", &c.Providers.Devin.Executable)
	str("NOISEDECK_PROVIDERS_DEVIN_HOOK_CONFIG", &c.Providers.Devin.HookConfig)
	str("NOISEDECK_PROVIDERS_DEVIN_HOOK_RECEIVER", &c.Providers.Devin.HookReceiver)
	str("NOISEDECK_PROVIDERS_GROK_EXECUTABLE", &c.Providers.Grok.Executable)
	str("NOISEDECK_PROVIDERS_KIMI_HOOK_RECEIVER", &c.Providers.Kimi.HookReceiver)
	str("NOISEDECK_PROVIDERS_KIMI_EXECUTABLE", &c.Providers.Kimi.Executable)
	str("NOISEDECK_PROVIDERS_KIMI_HOOK_CONFIG", &c.Providers.Kimi.HookConfig)
	str("NOISEDECK_PROVIDERS_MIMO_EXECUTABLE", &c.Providers.Mimo.Executable)
	str("NOISEDECK_PROVIDERS_OMP_EXECUTABLE", &c.Providers.Omp.Executable)
	str("NOISEDECK_PROVIDERS_OMP_HOOK_CONFIG", &c.Providers.Omp.HookConfig)
	str("NOISEDECK_PROVIDERS_OPENCODE_EXECUTABLE", &c.Providers.Opencode.Executable)
	str("NOISEDECK_PROVIDERS_OPENCODE_HOOK_CONFIG", &c.Providers.Opencode.HookConfig)
	str("NOISEDECK_PROVIDERS_PI_EXECUTABLE", &c.Providers.Pi.Executable)
	str("NOISEDECK_PROVIDERS_PI_HOOK_CONFIG", &c.Providers.Pi.HookConfig)
	str("NOISEDECK_PROVIDERS_QODER_EXECUTABLE", &c.Providers.Qoder.Executable)
	str("NOISEDECK_PROVIDERS_QWEN_EXECUTABLE", &c.Providers.Qwen.Executable)
	str("NOISEDECK_PROVIDERS_SHELL_EXECUTABLE", &c.Providers.Shell.Executable)
	boolean("NOISEDECK_EXPERIMENTS_UNCONFINED_CAPACITY_ROUTING", &c.Experiments.UnconfinedCapacityRouting)
	boolean("NOISEDECK_EXPERIMENTS_FONT_BENCH", &c.Experiments.FontBench)
	boolean("NOISEDECK_EXPERIMENTS_DEV_PREVIEW", &c.Experiments.DevPreview)
	str("NOISEDECK_EXPERIMENTS_DEV_PREVIEW_UPSTREAM", &c.Experiments.DevPreviewUpstream)
	boolean("NOISEDECK_SEALED_REQUIRE_ENCRYPTED_STATE", &c.Sealed.RequireEncryptedState)
	if _, ok := os.LookupEnv("NOISEDECK_SEALED_REQUIRE_ENCRYPTED_STATE"); !ok {
		boolean("NOISEDECK_REQUIRE_ENCRYPTED_STATE", &c.Sealed.RequireEncryptedState)
	}
	str("NOISEDECK_SEALED_PROFILE", &c.Sealed.Profile)
	str("NOISEDECK_SEALED_SWAP_POLICY", &c.Sealed.SwapPolicy)
	if _, ok := os.LookupEnv("NOISEDECK_SEALED_SWAP_POLICY"); !ok {
		str("NOISEDECK_SWAP_POLICY", &c.Sealed.SwapPolicy)
	}
	str("NOISEDECK_SEALED_REPORT_PATH", &c.Sealed.ReportPath)
	str("NOISEDECK_LOGGING_LEVEL", &c.Logging.Level)
	str("NOISEDECK_LOGGING_FORMAT", &c.Logging.Format)

	return errors.Join(errs...)
}

// normalize derives values, expands ~, absolutizes paths, derives the lock
// path, and resolves unset provider executables from PATH (doc.go).
func normalize(c *Config) error {
	var errs []error
	if c.Auth.Mode == "builtin" {
		c.Auth.Mode = "oidc"
	}
	var originScheme string
	for _, raw := range c.Server.AllowedOrigins {
		u, err := url.Parse(raw)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
			continue // Validate owns the precise origin-shape diagnostic.
		}
		if originScheme == "" {
			originScheme = u.Scheme
		} else if originScheme != u.Scheme {
			errs = append(errs, errors.New("config: server.allowed_origins must use one homogeneous scheme so cookie security is unambiguous"))
		}
	}
	c.Auth.SecureCookies = originScheme == "https"

	abs := func(what string, p *string) {
		if *p == "" {
			return
		}
		expanded, err := expandHome(*p)
		if err != nil {
			errs = append(errs, fmt.Errorf("config: %s: %w", what, err))
			return
		}
		resolved, err := filepath.Abs(expanded)
		if err != nil {
			errs = append(errs, fmt.Errorf("config: %s: %w", what, err))
			return
		}
		*p = resolved
	}

	abs("database.path", &c.Database.Path)
	if c.Database.LockPath == "" && c.Database.Path != "" {
		c.Database.LockPath = filepath.Join(filepath.Dir(c.Database.Path), "noisedeck.lock")
	}
	abs("database.lock_path", &c.Database.LockPath)
	abs("tmux.socket_path", &c.Tmux.SocketPath)
	abs("project_previews.metadata_socket", &c.ProjectPreviews.MetadataSocket)
	for i := range c.Projects.AllowedRoots {
		abs("projects.allowed_roots", &c.Projects.AllowedRoots[i])
	}
	abs("taskmaster.executable", &c.Taskmaster.Executable)
	abs("chats.executable", &c.Chats.Executable)

	for _, entry := range c.Providers.Entries() {
		p := entry.Config
		abs("providers.*.hook_config", &p.HookConfig)
		if p.Executable != "" {
			abs("providers.*.executable", &p.Executable)
		}
	}
	// Resolve unset executables from PATH once, at load. This is the whole
	// of harness discovery: an image that does not contain a harness leaves
	// its executable empty, the provider layer skips it, and the console
	// offers what the image actually has. A default that named an absolute
	// path instead would advertise a harness the image never installed.
	//
	// Absence is not an error: a missing kimi must not stop claude.
	for _, entry := range c.Providers.Entries() {
		if entry.Config.Executable == "" {
			if found, err := exec.LookPath(entry.Binary); err == nil {
				entry.Config.Executable = found
			}
		}
	}

	return errors.Join(errs...)
}

// isLoopbackHost reports whether the host of a browser-facing URL denotes
// this machine: a loopback IP literal, or the name "localhost" in any ASCII
// case. The name is admitted because it is what people type — a first run
// that refuses every write from http://localhost:18082 with "origin not
// allowed" is a trap, not a defence — and it is safe to admit here because
// the callers classify an origin a browser PRESENTS or a callback a browser
// is SENT TO, values Deck compares as strings and never resolves or
// connects to; every browser's resolver reserves the name to loopback
// (RFC 6761). server.bind and the dev-preview upstream keep their IP-literal
// rules: one opens a listener and the other is dialed, and a name could
// resolve anywhere.
//
// The comparison is byte-wise ASCII on purpose. strings.EqualFold applies
// Unicode simple folding, under which "localhoſt" (long s) also matches —
// a spelling no browser would ever present as its Origin.
func isLoopbackHost(host string) bool {
	if len(host) == len("localhost") {
		match := true
		for i := 0; i < len(host); i++ {
			c := host[i]
			if 'A' <= c && c <= 'Z' {
				c += 'a' - 'A'
			}
			if c != "localhost"[i] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// validateRedirectURL enforces the exact-callback contract: a
// fully-formed absolute URL with a host and no userinfo,
// query, or fragment; the mounted /auth/callback path; https in production
// or http only on a loopback host; Secure cookies only over https; and an
// origin that is one of the trusted browser origins, so a login can never
// be redirected off the console's own origin.
func validateRedirectURL(c *Config, raw string, add func(string, ...any)) {
	// A literal exact callback: Google matches redirect_uri by exact string,
	// so a trailing "?" or "#" (empty query/fragment) or any percent-encoded
	// path is a different URI that would never match — reject it here rather
	// than fail opaquely at Google. Checking the RAW string catches the
	// empty-but-present cases url.Parse would normalize away.
	if strings.ContainsAny(raw, "?#") {
		add("auth.google.redirect_url %q: must carry no query or fragment (not even an empty one)", raw)
		return
	}
	u, err := url.Parse(raw)
	if err != nil || !u.IsAbs() || u.Host == "" || u.User != nil ||
		u.EscapedPath() != "/auth/callback" {
		add("auth.google.redirect_url %q: must be an absolute URL with a host and the literal path /auth/callback (no encoding)", raw)
		return
	}
	if u.Scheme != "https" && (u.Scheme != "http" || !isLoopbackHost(u.Hostname())) {
		add("auth.google.redirect_url %q: must be https, or http only on a loopback host (a loopback IP or localhost)", raw)
	}
	// The redirect origin must be one the browser is already trusted to use:
	// scheme://host of the callback must appear in server.allowed_origins.
	origin := u.Scheme + "://" + u.Host
	allowed := false
	for _, o := range c.Server.AllowedOrigins {
		if strings.EqualFold(strings.TrimRight(o, "/"), origin) {
			allowed = true
			break
		}
	}
	if !allowed {
		add("auth.google.redirect_url %q: its origin %q is not in server.allowed_origins", raw, origin)
	}
}

// isHeaderFieldName reports whether s is a valid HTTP field name — an RFC
// 7230 token. A header net/http itself would refuse to send cannot count as
// configured authentication: it would satisfy the non-loopback check while
// the proxy identity could never actually arrive under that name.
func isHeaderFieldName(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case strings.ContainsRune("!#$%&'*+-.^_`|~", r):
		default:
			return false
		}
	}
	return true
}

// expandHome rewrites a leading "~/" against the current user's home.
func expandHome(p string) (string, error) {
	if p != "~" && !strings.HasPrefix(p, "~/") {
		return p, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("expand %q: %w", p, err)
	}
	return filepath.Join(home, strings.TrimPrefix(p, "~")), nil
}

// Validate reports every violation at once, so a broken config is fixed in
// one round trip. Rules cite the requirement they enforce; see doc.go.
func (c *Config) Validate() error {
	var errs []error
	if err := c.validateProjectPreviews(); err != nil {
		errs = append(errs, err)
	}
	add := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf(format, args...))
	}

	if c.Sealed.SwapPolicy != "" && c.Sealed.SwapPolicy != "personal" && c.Sealed.SwapPolicy != "paranoid" {
		add("sealed.swap_policy %q: must be personal or paranoid", c.Sealed.SwapPolicy)
	}
	// sealed.profile names a storage layout the preflight knows how to check.
	switch c.Sealed.Profile {
	case "shared-host", "own-host":
	default:
		add("sealed.profile %q: must be shared-host or own-host", c.Sealed.Profile)
	}

	// A private gateway socket replaces TCP; ordinary deployments retain the
	// IP-literal bind so a hostname cannot resolve outside their loopback boundary.
	host, port, err := net.SplitHostPort(c.Server.Bind)
	var bindIP net.IP
	switch {
	case c.Server.UnixSocket != "":
		c.validatePrivateSocket(add)
	case err != nil:
		add("server.bind %q: not host:port: %v", c.Server.Bind, err)
	default:
		if bindIP = net.ParseIP(host); bindIP == nil {
			add("server.bind %q: host must be an IP literal — a hostname could resolve anywhere, defeating the loopback check", c.Server.Bind)
		}
		if n, convErr := strconv.Atoi(port); convErr != nil || n < 1 || n > 65535 {
			add("server.bind %q: invalid port", c.Server.Bind)
		}
	}

	// Authentication mode and its prerequisites.
	authConfigured := false
	switch c.Auth.Mode {
	case "password":
		// Boot generates or loads both secrets before the auth handler is
		// constructed. An optional supplied hash is validated there with the
		// password library; plaintext configuration has no accepted key.
		if c.Auth.SessionSecret != "" || c.Auth.Google.ClientID != "" ||
			c.Auth.Google.ClientSecret != "" || c.Auth.Google.AllowedEmail != "" {
			add("auth.mode password refuses OIDC-only configuration; set NOISEDECK_AUTH_MODE=oidc or remove every OIDC value before startup")
		}
		authConfigured = true
	case "oidc":
		// OIDC uses single-user Google login (internal/auth/doc.go). All of
		// it is required:
		// a partially configured login is a locked-out console, and locked
		// out must be loud at startup, not at first login.
		g := c.Auth.Google
		if c.Auth.SessionSecret == "" {
			add("auth: NOISEDECK_AUTH_SESSION_SECRET required in oidc mode (signs the local session)")
		} else if len(c.Auth.SessionSecret) < 32 {
			add("auth: NOISEDECK_AUTH_SESSION_SECRET must be at least 32 bytes in oidc mode (it keys the HMAC that signs sessions; a short value is guessable)")
		}
		if g.ClientID == "" {
			add("auth.google.client_id: required in oidc mode")
		}
		if g.ClientSecret == "" {
			add("auth: NOISEDECK_AUTH_GOOGLE_CLIENT_SECRET required in oidc mode")
		}
		if !strings.Contains(g.AllowedEmail, "@") {
			add("auth.google.allowed_email %q: your verified Google email is required", g.AllowedEmail)
		}
		validateRedirectURL(c, g.RedirectURL, add)
		authConfigured = c.Auth.SessionSecret != "" && g.ClientID != "" &&
			g.ClientSecret != "" && g.RedirectURL != "" && g.AllowedEmail != ""
	case "none":
		authConfigured = true
		for _, raw := range c.Server.AllowedOrigins {
			u, parseErr := url.Parse(raw)
			if parseErr != nil || !isLoopbackHost(u.Hostname()) {
				add("auth.mode none requires every server.allowed_origins entry to be a loopback origin (a loopback IP or localhost); %q is not loopback. Use password, oidc, or trusted_proxy for a public origin", raw)
			}
		}
	case "trusted_proxy":
		if len(c.Auth.TrustedProxy.Addresses) == 0 {
			add("auth.trusted_proxy.addresses: required in trusted_proxy mode — an empty list would accept the identity header from anyone")
		}
		for _, a := range c.Auth.TrustedProxy.Addresses {
			if net.ParseIP(a) == nil {
				if _, _, cidrErr := net.ParseCIDR(a); cidrErr != nil {
					add("auth.trusted_proxy.addresses: %q is neither an IP nor a CIDR", a)
				}
			}
		}
		if !isHeaderFieldName(c.Auth.TrustedProxy.IdentityHeader) {
			add("auth.trusted_proxy.identity_header %q: not a valid HTTP header field name", c.Auth.TrustedProxy.IdentityHeader)
		}
		authConfigured = len(c.Auth.TrustedProxy.Addresses) > 0 &&
			isHeaderFieldName(c.Auth.TrustedProxy.IdentityHeader)
	default:
		add("auth.mode %q: must be \"none\", \"password\", \"oidc\", or \"trusted_proxy\" (\"builtin\" is accepted as a deprecated oidc alias)", c.Auth.Mode)
	}

	// A non-loopback bind without configured authentication is refused.
	if bindIP != nil && !bindIP.IsLoopback() && !authConfigured {
		add("server.bind %q: non-loopback bind refused: authentication is not configured for mode %q", c.Server.Bind, c.Auth.Mode)
	}

	// WebSocket origins: strict, no wildcards.
	if len(c.Server.AllowedOrigins) == 0 {
		add("server.allowed_origins: at least one origin is required")
	}
	for _, o := range c.Server.AllowedOrigins {
		// Name the trailing-slash case directly: it is the classic
		// origin + "/path" concatenation accident (which also doubles the
		// slash in auth.google.redirect_url), and the generic origin-shape
		// error below would not say so.
		if strings.HasSuffix(o, "/") {
			add("server.allowed_origins: %q: an origin is scheme://host[:port] exactly — remove the trailing slash", o)
			continue
		}
		u, parseErr := url.Parse(o)
		if parseErr != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			add("server.allowed_origins: %q is not an http(s) origin", o)
			continue
		}
		// An origin is scheme://host[:port] and nothing else. Anything with
		// a path, query, fragment, or userinfo would never equal a browser's
		// Origin header, so accepting it here silently allowlists nothing.
		if u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil || u.Opaque != "" {
			add("server.allowed_origins: %q: an origin is scheme://host[:port] only — no path, query, fragment, or userinfo", o)
			continue
		}
		// The authority itself must be well-formed: a hostname is required,
		// and a port, when present, must be a real one. url.Parse accepts
		// "http://:80", "http://x.test:", and five-digit ports without
		// complaint, and none of them can match a browser's Origin header.
		if u.Hostname() == "" || strings.HasSuffix(u.Host, ":") {
			add("server.allowed_origins: %q: missing hostname or empty port", o)
			continue
		}
		if p := u.Port(); p != "" {
			n, portErr := strconv.Atoi(p)
			if portErr != nil || n < 1 || n > 65535 {
				add("server.allowed_origins: %q: invalid port", o)
				continue
			}
			if (u.Scheme == "https" && n == 443) || (u.Scheme == "http" && n == 80) {
				add("server.allowed_origins: %q: omit the default port — browsers never send it in the Origin header", o)
				continue
			}
		}
		if strings.Contains(o, "*") {
			add("server.allowed_origins: %q: wildcards are not permitted", o)
		}
	}

	if c.Database.Path == "" {
		add("database.path: required")
	}
	if c.Database.BusyTimeout <= 0 {
		add("database.busy_timeout: must be positive (PRD §9.1 requires a bounded wait)")
	}

	if c.Tmux.SocketPath == "" {
		add("tmux.socket_path: required — the dedicated namespace is what keeps your own tmux sessions out of reach")
	}
	if c.Tmux.SessionPrefix == "" {
		add("tmux.session_prefix: required")
	} else if strings.ContainsAny(c.Tmux.SessionPrefix, ".:") {
		// '.' and ':' are tmux target-syntax separators; a prefix containing
		// them would make every session name unaddressable.
		add("tmux.session_prefix %q: must not contain '.' or ':'", c.Tmux.SessionPrefix)
	}
	if c.Tmux.ScrollbackLines <= 0 {
		add("tmux.scrollback_lines: must be positive (PRD open question 5 bounds it)")
	}

	if len(c.Projects.AllowedRoots) == 0 {
		add("projects.allowed_roots: required — there is no safe default allowlist")
	}
	// normalize absolutizes non-empty entries, so anything empty or
	// relative here is a root that never went through normalization — an
	// empty string would otherwise count as "an allowlist exists" while
	// allowlisting nothing checkable.
	for _, root := range c.Projects.AllowedRoots {
		if root == "" || !filepath.IsAbs(root) {
			add("projects.allowed_roots: entry %q must be a non-empty absolute path", root)
		}
	}

	// Reconciliation runs at least every fifteen seconds while ready, so
	// fifteen seconds is the longest permitted period; shorter is stricter
	// and allowed, with a 1s floor so a typo cannot configure a busy loop.
	if iv := c.Reconciler.Interval.Std(); iv < time.Second || iv > 15*time.Second {
		add("reconciler.interval %s: must be between 1s and 15s — reconciliation is required at least every fifteen seconds", iv)
	}
	// The dev-preview route forwards requests carrying your live
	// session. Confine it to a process on this host: a routable upstream
	// would let configuration alone turn an authenticated user into a
	// proxy for somewhere else.
	if c.Experiments.DevPreview {
		host, port, splitErr := net.SplitHostPort(c.Experiments.DevPreviewUpstream)
		switch {
		case splitErr != nil:
			add("experiments.dev_preview_upstream %q: must be host:port", c.Experiments.DevPreviewUpstream)
		case net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback():
			add("experiments.dev_preview_upstream %q: must be a loopback IP — the dev preview runs with your authority and may only reach this host", c.Experiments.DevPreviewUpstream)
		default:
			if n, portErr := strconv.Atoi(port); portErr != nil || n < 1 || n > 65535 {
				add("experiments.dev_preview_upstream %q: invalid port", c.Experiments.DevPreviewUpstream)
			}
		}
	}

	if c.Taskmaster.Executable == "" || !filepath.IsAbs(c.Taskmaster.Executable) {
		add("taskmaster.executable %q: an absolute path is required (fixed executable)", c.Taskmaster.Executable)
	}
	if c.Chats.Executable == "" || !filepath.IsAbs(c.Chats.Executable) {
		add("chats.executable %q: an absolute path is required (fixed executable)", c.Chats.Executable)
	}

	// Providers: unset means unavailable with a direct "executable or
	// config is missing" diagnostic at the provider layer, but a configured
	// Codex or Kimi without its hook config could never capture identity,
	// and the verify-and-refuse policy needs a file to verify. Claude's
	// identity is assigned, not retrieved: a hook_config for it describes a
	// mechanism that does not exist and is rejected rather than ignored.
	for _, pv := range []struct {
		name   string
		p      Provider
		hooked bool
	}{
		{"claude", c.Providers.Claude, false},
		{"cline", c.Providers.Cline, false},
		{"command_code", c.Providers.CommandCode, true},
		{"codex", c.Providers.Codex, true},
		{"cursor", c.Providers.Cursor, false},
		{"devin", c.Providers.Devin, true},
		{"grok", c.Providers.Grok, false},
		{"kimi", c.Providers.Kimi, true},
		{"kilo", c.Providers.Kilo, true},
		{"mimo", c.Providers.Mimo, false},
		{"omp", c.Providers.Omp, true},
		{"opencode", c.Providers.Opencode, true},
		{"pi", c.Providers.Pi, true},
		{"qoder", c.Providers.Qoder, false},
		{"qwen", c.Providers.Qwen, false},
		{"shell", c.Providers.Shell, false},
	} {
		if pv.p.Executable != "" && !filepath.IsAbs(pv.p.Executable) {
			add("providers.%s.executable %q: must be absolute", pv.name, pv.p.Executable)
		}
		if pv.hooked && pv.p.Executable != "" {
			if pv.p.HookConfig == "" || !filepath.IsAbs(pv.p.HookConfig) {
				add("providers.%s.hook_config %q: an absolute path is required when the provider is configured (identity capture needs the provisioned identity hook; see docs/provider-contracts/)", pv.name, pv.p.HookConfig)
			}
		}
		if !pv.hooked && pv.p.HookConfig != "" {
			add("providers.%s.hook_config: %s has no identity hook (its identity contract does not use one; see docs/provider-contracts/); remove this setting", pv.name, pv.name)
		}
	}

	switch c.Logging.Level {
	case "trace", "debug", "info", "warn", "error":
	default:
		add("logging.level %q: must be one of trace, debug, info, warn, error", c.Logging.Level)
	}
	switch c.Logging.Format {
	case "json", "console":
	default:
		add("logging.format %q: must be \"json\" or \"console\"", c.Logging.Format)
	}

	return errors.Join(errs...)
}
