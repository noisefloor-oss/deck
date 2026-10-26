package boot

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/rs/zerolog"

	"github.com/noisefloor-oss/deck/internal/apppreview"
	"github.com/noisefloor-oss/deck/internal/audit"
	"github.com/noisefloor-oss/deck/internal/auth"
	"github.com/noisefloor-oss/deck/internal/capacity"
	"github.com/noisefloor-oss/deck/internal/chats"
	"github.com/noisefloor-oss/deck/internal/config"
	"github.com/noisefloor-oss/deck/internal/db"
	"github.com/noisefloor-oss/deck/internal/experiment"
	"github.com/noisefloor-oss/deck/internal/githubconnect"
	"github.com/noisefloor-oss/deck/internal/githubrepos"
	"github.com/noisefloor-oss/deck/internal/harnesslogin"
	"github.com/noisefloor-oss/deck/internal/hookcapture"
	"github.com/noisefloor-oss/deck/internal/inputgate"
	"github.com/noisefloor-oss/deck/internal/lane"
	"github.com/noisefloor-oss/deck/internal/lease"
	"github.com/noisefloor-oss/deck/internal/lock"
	"github.com/noisefloor-oss/deck/internal/project"
	"github.com/noisefloor-oss/deck/internal/provider"
	"github.com/noisefloor-oss/deck/internal/reconciler"
	"github.com/noisefloor-oss/deck/internal/sealedstate"
	"github.com/noisefloor-oss/deck/internal/server"
	"github.com/noisefloor-oss/deck/internal/server/webassets"
	"github.com/noisefloor-oss/deck/internal/session"
	"github.com/noisefloor-oss/deck/internal/settings"
	"github.com/noisefloor-oss/deck/internal/taskengine"
	"github.com/noisefloor-oss/deck/internal/tasks"
	"github.com/noisefloor-oss/deck/internal/terminal"
	"github.com/noisefloor-oss/deck/internal/tmux"
)

// Handles are the wired subsystems of a booted backend.
type Handles struct {
	Lock          *lock.ExclusiveLock
	DB            *db.DB
	Tmux          *tmux.Client
	Providers     map[provider.Name]provider.Descriptor
	Sessions      *session.Repository
	Manager       *session.Manager
	Reconciler    *reconciler.Reconciler
	Auth          *auth.Handler
	Leases        *lease.Manager
	Terminal      *terminal.Proxy
	Taskmaster    *tasks.Runner
	Lanes         *lane.Service
	HarnessLogin  *harnesslogin.LocalServer
	GitHubConnect *githubconnect.Manager
	// Projects owns the clone jobs, which Close stops.
	Projects         *project.Handler
	Server           *server.Server
	Preview          *apppreview.Gateway
	PreviewRegistry  *apppreview.Registry
	previewMetadata  *apppreview.MetadataServer
	previewHTTP      *http.Server
	previewListeners []net.Listener

	log      zerolog.Logger
	interval time.Duration
}

type upOptions struct {
	buildID        string
	setupOutput    io.Writer
	passwordPinned bool
	// sealedFacts overrides the kernel facts the encrypted-state check reads;
	// tests use it to boot against a fixture layout.
	sealedFacts func(reportPath string) (sealedstate.Facts, error)
}

type UpOption func(*upOptions)

// WithSetupOutput keeps the one-time setup link outside structured request logs.
func WithSetupOutput(output io.Writer) UpOption {
	return func(options *upOptions) { options.setupOutput = output }
}

// WithBuildID binds the browser document and authenticated build endpoint to
// the identity of the binary serving them.
func WithBuildID(buildID string) UpOption {
	return func(options *upOptions) { options.buildID = buildID }
}

// WithSealedFacts supplies the encrypted-state check's inputs instead of
// reading them from /proc and /sys.
func WithSealedFacts(read func(reportPath string) (sealedstate.Facts, error)) UpOption {
	return func(options *upOptions) { options.sealedFacts = read }
}

// Up runs the startup sequence against an already-validated configuration
// and returns a ready backend. On any failure it unwinds what it opened and
// returns the cause; nothing half-booted stays behind.
func Up(ctx context.Context, cfg *config.Config, log zerolog.Logger, opts ...UpOption) (*Handles, error) {
	options := upOptions{buildID: "dev", setupOutput: os.Stderr, passwordPinned: cfg.Auth.PasswordHash != ""}
	for _, option := range opts {
		option(&options)
	}
	h := &Handles{log: log, interval: cfg.Reconciler.Interval.Std()}
	booted := false
	defer func() {
		if !booted {
			h.Close()
		}
	}()
	var err error

	// 0. Encrypted state, before any directory is created or file written.
	// Refusing here keeps a plaintext store empty; a warning here is the
	// user's notice that the same layout would be refused once required.
	if err := verifySealedState(cfg, options.sealedFacts, log); err != nil {
		return nil, fmt.Errorf("boot: %w", err)
	}

	// Fresh data directories are created private; existing ones are left
	// untouched — the tmux client refuses an insecure directory itself. The
	// lock path is listed separately: it defaults beside the database but is
	// independently configurable, and a boot must not fail confusingly just
	// because the user pointed it somewhere fresh.
	for _, dir := range []string{
		filepath.Dir(cfg.Database.Path),
		filepath.Dir(cfg.Database.LockPath),
		filepath.Dir(cfg.Tmux.SocketPath),
	} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("boot: create %s: %w", dir, err)
		}
	}

	// 1. Exclusivity before any shared state is touched.
	h.Lock, err = lock.Acquire(cfg.Database.LockPath)
	if err != nil {
		return nil, fmt.Errorf("boot: %w", err)
	}
	existingInstallation, err := installationStateExists(cfg.Database.Path)
	if err != nil {
		return nil, fmt.Errorf("boot: inspect installation state: %w", err)
	}
	if err := auth.EnsureInstallationMode(auth.InstallationModePath(cfg.Database.Path), cfg.Auth.Mode, existingInstallation); err != nil {
		return nil, fmt.Errorf("boot: %w", err)
	}
	if cfg.Auth.Mode == "password" {
		statePath := filepath.Join(filepath.Dir(cfg.Database.Path), "auth-state.json")
		bootstrap, bootstrapErr := auth.EnsureBootstrapState(statePath, cfg.Auth.PasswordHash)
		if bootstrapErr != nil {
			return nil, fmt.Errorf("boot: %w", bootstrapErr)
		}
		cfg.Auth.PasswordHash = string(bootstrap.State.PasswordHash)
		cfg.Auth.SessionSecret = string(bootstrap.State.SessionSecret)
		link, err := auth.EnsureSetupURL(statePath, cfg.Server.AllowedOrigins[0], time.Now())
		if err != nil {
			return nil, fmt.Errorf("boot: %w", err)
		}
		if link != "" {
			if _, err := fmt.Fprintf(options.setupOutput, "Create your password: %s\n", link); err != nil {
				return nil, fmt.Errorf("boot: write setup link: %w", err)
			}
		}
	}

	// 2. Control database: open migrates; then prove writability now rather
	// than at the first session write.
	h.DB, err = db.Open(ctx, db.Config{
		Path:        cfg.Database.Path,
		BusyTimeout: cfg.Database.BusyTimeout.Std(),
	})
	if err != nil {
		return nil, fmt.Errorf("boot: %w", err)
	}
	if err := h.DB.VerifyWritable(ctx); err != nil {
		return nil, fmt.Errorf("boot: %w", err)
	}

	// 3. The dedicated tmux namespace, security-verified.
	h.Tmux, err = tmux.NewClient(tmux.Config{SocketPath: cfg.Tmux.SocketPath,
		Timeout: 10 * time.Second, ScrollbackLines: cfg.Tmux.ScrollbackLines})
	if err != nil {
		return nil, fmt.Errorf("boot: %w", err)
	}
	if err := h.Tmux.VerifySecurity(); err != nil {
		return nil, fmt.Errorf("boot: %w", err)
	}

	// 4. Providers: configured and constructible means available for launch;
	// anything else is logged and unavailable, never fatal.
	h.Providers = buildProviders(cfg, log)

	h.Sessions = session.NewRepository(h.DB)
	h.Manager = session.NewManager(h.Sessions, h.Tmux, h.Providers, cfg.Tmux.SessionPrefix)
	projects := project.NewRepository(h.DB)
	// The roster registration carries the committed project scope: resolved
	// here through the projects repository, since the a2a helper never reads
	// the control database.
	h.Manager.SetProjectNameResolver(func(ctx context.Context, projectID string) (string, error) {
		p, err := projects.GetIncludingRemoved(ctx, projectID)
		if err != nil {
			return "", err
		}
		return p.Name, nil
	})
	h.Manager.SetDisplayNameRegistrar(session.A2ADisplayNameRegistrar(cfg.Chats.Executable, log))
	temporaryBase, err := filepath.EvalSymlinks("/tmp")
	if err != nil {
		return nil, fmt.Errorf("boot: resolve provider temporary base: %w", err)
	}
	if err := h.Manager.SetTemporaryRoot(temporaryBase); err != nil {
		return nil, fmt.Errorf("boot: provider temporary storage: %w", err)
	}
	h.Leases = lease.NewManager(h.DB)
	gate := inputgate.New()
	consoleSettings := settings.NewRepository(h.DB)
	h.Manager.SetSettings(consoleSettings)
	h.Manager.SetResumeDeps(h.Leases, gate,
		func(ctx context.Context, projectID string) (string, error) {
			p, err := projects.GetIncludingRemoved(ctx, projectID)
			if err != nil {
				return "", err
			}
			return p.CanonicalPath, nil
		})
	experimentCapability := ""
	if cfg.Experiments.UnconfinedCapacityRouting {
		var err error
		experimentCapability, err = experiment.NewControlCapability()
		if err != nil {
			return nil, fmt.Errorf("boot: create unconfined experiment capability: %w", err)
		}
		h.Manager.SetUnconfinedCapacityRouting(cfg.Server.LoopbackBase(), experimentCapability)
		log.Warn().Msg("unconfined capacity-routing experiment enabled")
	}

	// Bearer hashes and exact launch fences are durable before spawn. A service
	// restart reconstructs only this stateless envelope; a pending grant stays
	// acceptable while its generation is the live running row, and otherwise
	// until this absolute expiry - the window bounds a launch that died, not
	// the user's first submission.
	hookCaptures, err := hookcapture.NewService(h.Sessions, time.Hour)
	if err != nil {
		return nil, fmt.Errorf("boot: identity capture: %w", err)
	}
	h.Manager.SetHookCaptureDeps(hookCaptures, cfg.Server.LoopbackBase())
	h.Manager.SetHookCaptureSocket(cfg.Server.UnixSocket)
	if err := hookCaptures.Sweep(ctx); err != nil {
		return nil, fmt.Errorf("boot: expire identity captures: %w", err)
	}
	login := h.startHarnessLogin(ctx, cfg)
	if err := h.Manager.ConvergeStopAttempts(ctx); err != nil {
		return nil, fmt.Errorf("boot: stop convergence: %w", err)
	}
	if err := h.Manager.ConvergeRemovalAttempts(ctx); err != nil {
		return nil, fmt.Errorf("boot: removal convergence: %w", err)
	}
	// A crash may have occurred after durable candidate admission or spawn.
	// Resolve those exact candidates before the generic capture-expiry and pane
	// inventories can mistake them for sources or orphans. This convergence
	// never spawns a second candidate.
	if err := h.Manager.ConvergeRecoveryCandidates(ctx); err != nil {
		return nil, fmt.Errorf("boot: recovery-candidate convergence: %w", err)
	}
	// Legacy needs_attention rows are a frozen input from earlier releases.
	// Converge them before the generic pass can restore an exact-live row while
	// preserving its obsolete retry counter. An exact live tuple restores and
	// re-arms running, a dead pane records exit evidence, and every other row
	// becomes honest interrupted metadata. This pass never launches, signals,
	// adopts, or writes to a pane.
	if err := h.Manager.ConvergeLegacyRows(ctx); err != nil {
		return nil, fmt.Errorf("boot: legacy row convergence: %w", err)
	}

	// 5. The startup reconciliation pass: the moment a previous life's
	// ordinary claims meet observed reality. Capture expiry and legacy
	// convergence above are prerequisites for safe generic classification.
	h.Reconciler = reconciler.New(h.Sessions, h.Tmux, h.interval)
	report, err := h.Reconciler.RunOnce(ctx)
	if err != nil {
		return nil, fmt.Errorf("boot: startup reconciliation: %w", err)
	}
	// Legacy rows and ordinary lost generations receive their single durable
	// automatic opportunity before the backend can report ready or return the
	// first session response.
	if err := h.Manager.AutoResumeSweep(ctx); err != nil {
		return nil, fmt.Errorf("boot: startup automatic recovery: %w", err)
	}
	if err := h.Manager.SweepTemporaryDirs(ctx); err != nil {
		return nil, fmt.Errorf("boot: provider temporary-directory convergence: %w", err)
	}
	if err := h.Manager.RestoreDisplayNames(ctx); err != nil {
		log.Warn().Err(err).Msg("could not restore agent roster names")
	}
	log.Info().
		Int("sessions", report.SessionsActive).
		Int("panes", report.PanesObserved).
		Int("discrepancies", report.Discrepancies).
		Bool("server_down", report.ServerDown).
		Msg("startup reconciliation complete")

	// 6. Authentication at the web boundary. Constructed from
	// config, which already refused a partially configured OIDC login
	// fail-closed; New performs no network request.
	authCfg := authConfig(cfg)
	if cfg.Auth.Mode == "password" {
		authCfg.PasswordStatePath = filepath.Join(filepath.Dir(cfg.Database.Path), "auth-state.json")
		authCfg.SetupOutput = options.setupOutput
		authCfg.PasswordPinned = options.passwordPinned
		authCfg.AllowedOrigins = cfg.Server.AllowedOrigins
	}
	// Logout epochs persist with the console settings so a logout outlives
	// the process that served it: an in-flight session renewal from before
	// the logout must verify stale after a restart too.
	authCfg.EpochStore = consoleSettings
	h.Auth, err = auth.New(authCfg, log)
	if err != nil {
		return nil, fmt.Errorf("boot: %w", err)
	}

	// 8. The HTTP layer.
	h.Server = server.New(server.Config{
		Bind:              cfg.Server.Bind,
		UnixSocket:        cfg.Server.UnixSocket,
		GatewayUID:        cfg.Server.GatewayUID,
		AllowedOrigins:    cfg.Server.AllowedOrigins,
		ProtectBrowserAPI: cfg.ProjectPreviews.Loopback || cfg.ProjectPreviews.Bind != "" || cfg.Server.UnixSocket != "",
	}, log, h.readinessChecks())
	// The login ceremony is public; everything the API and WebSocket tasks
	// mount goes behind h.Auth.Require. Process liveness and readiness stay
	// public because a supervisor cannot authenticate.
	h.Auth.Mount(h.Server.Handle)
	if cfg.Server.UnixSocket != "" {
		h.Server.Handle("POST /api/internal/workspace/prepare-lock", h.Auth.Require(workspaceLockHandler(h.Manager.PrepareWorkspaceLock)))
	}
	loginTransport := harnesslogin.NewHandler(login, cfg.Server.AllowedOrigins)
	if cfg.Auth.Mode == "trusted_proxy" && cfg.HarnessLogin.TrustedProxyIdentity != "" {
		loginTransport.Operator = auth.ProxyPrincipal(cfg.HarnessLogin.TrustedProxyIdentity)
	}
	loginHandler := h.Auth.Require(loginTransport)
	h.Server.Handle("GET /api/harness-login", loginHandler)
	h.Server.Handle("POST /api/harness-login", loginHandler)
	h.Server.Handle("GET /api/build", h.Auth.Require(server.BuildHandler(options.buildID)))
	h.Server.Handle("GET /api/release-update", h.Auth.Require(server.NewReleaseNotice(options.buildID)))
	h.Server.Handle("GET /api/doctor", h.Auth.Require(server.DoctorHandler(cfg, func() bool {
		return h.Server.BoundAddr() != ""
	})))
	// What the picker may offer. Deck ships in more than one image, so the
	// browser cannot know the harness list from the bundle it was served —
	// only the process that resolved the executables knows it.
	h.Server.Handle("GET /api/providers", h.Auth.Require(server.ProvidersHandler(h.Providers)))

	// The terminal attachment: authenticated, lease-fenced, origin-checked.
	// The one serialized write gate sits in the terminal proxy.
	h.Terminal = terminal.New(h.Tmux, h.Sessions, h.Leases,
		gate, cfg.Server.AllowedOrigins, log)
	h.Server.Handle("GET /api/sessions/{id}/terminal",
		h.Auth.Require(http.HandlerFunc(h.Terminal.Handle)))
	// GitHub authentication can run before a project exists. Its fixed-purpose
	// terminal shares workspace admission while using a separate tmux namespace.
	githubTmux, err := tmux.NewClient(tmux.Config{
		SocketPath: filepath.Join(filepath.Dir(cfg.Tmux.SocketPath), "github-connect", "tmux.sock"),
		Timeout:    10 * time.Second, ScrollbackLines: cfg.Tmux.ScrollbackLines,
	})
	if err != nil {
		return nil, fmt.Errorf("initialize GitHub connection terminal: %w", err)
	}
	h.GitHubConnect = githubconnect.NewManager(githubTmux, gate, log)
	githubconnect.NewHandler(h.GitHubConnect, h.GitHubConnect.NewProxy(cfg.Server.AllowedOrigins, log), cfg.Server.AllowedOrigins).Mount(h.Server.Handle, h.Auth.Require)
	server.NewDiagnostics(h.Sessions, h.Manager, h.Reconciler, h.Leases,
		h.Terminal, log).Mount(h.Server.Handle, h.Auth.Require)

	// The retired composer API: submit/release/cancel no longer exist —
	// Deck owns no durable input surface. The routes stay mounted so stale
	// callers get a definitive 410 Gone rather than a misleading 404 or an
	// accidental fall-through; the web lane's own client is the first to
	// drop them.
	terminal.NewGoneHandler().Mount(h.Server.Handle, h.Auth.Require)

	// The project registry: registration validates against the configured
	// allowlist on every create.
	// Console settings: runtime user choices the UI writes, read at
	// each session launch.
	settings.NewHandler(consoleSettings, log).Mount(h.Server.Handle, h.Auth.Require)

	githubRepositories := githubrepos.NewService()
	githubrepos.NewHandler(githubRepositories).Mount(h.Server.Handle, h.Auth.Require)
	projectHandler := project.NewHandler(projects, cfg.Projects.AllowedRoots, h.Manager, log)
	// A committed project rename refreshes the roster scope of the project's
	// active sessions; the refresh is best-effort and never fails the rename.
	projectHandler.OnRenamed(func(ctx context.Context, projectID string) {
		if err := h.Manager.SyncProjectRegistrations(ctx, projectID); err != nil {
			log.Warn().Err(err).Msg("could not refresh agent roster project metadata")
		}
	})
	projectHandler.SetGitHubRepositoryResolver(func(ctx context.Context, account string, id int64) (string, error) {
		result := githubRepositories.Repository(ctx, account, id)
		if result.Outcome != "ok" || result.Repository == nil {
			return "", fmt.Errorf("GitHub access could not be confirmed (%s). Refresh the repository list before importing", result.Outcome)
		}
		return result.Repository.CloneURL, nil
	})
	projectHandler.Mount(h.Server.Handle, h.Auth.Require)
	projectHandler.MountFiles(h.Server.Handle, h.Auth.Require)
	h.Projects = projectHandler
	if err := h.mountProjectPreviews(cfg, projects); err != nil {
		return nil, err
	}

	// The experiment API is outside browser authentication. It is absent by
	// default and requires the per-process capability inherited only by
	// sessions this Deck launches, so a reverse proxy cannot turn loopback
	// into remote authority.
	experiment.New(cfg.Experiments.UnconfinedCapacityRouting, experimentCapability, capacity.New(h.Providers),
		h.Providers, h.Manager, projects, log).Mount(h.Server.Handle)

	// The Taskmaster read side: canonical task state and PRD files, read
	// fresh per request from the projects' own files. The
	// generation runner rides the same handler: fixed argv in managed tmux
	// sessions, serialized per project by the database, and reconciled
	// against observed tmux state at startup so a previous life's running
	// rows reach their honest terminal state.
	taskmasterRuns := tasks.NewRunsRepository(h.DB)
	h.Taskmaster = tasks.NewRunner(taskmasterRuns, projects, h.Tmux,
		cfg.Taskmaster.Executable, cfg.Tmux.SessionPrefix, log).WithProviders(h.Providers)
	if err := h.Taskmaster.Reconcile(ctx); err != nil {
		return nil, fmt.Errorf("boot: taskmaster reconciliation: %w", err)
	}
	tasks.NewHandler(projects, cfg.Taskmaster.Executable, log).WithRunner(h.Taskmaster).
		Mount(h.Server.Handle, h.Auth.Require)

	// Task lanes: one task, its own branch and worktree, opened through the
	// engine's claim, run and worktree commands. The fencing tokens stay in
	// the control database; a renewal loop keeps the leases alive while a
	// lane is open.
	h.Lanes = lane.NewService(lane.Config{
		Store:    lane.NewStore(h.DB),
		Projects: projects,
		Engine: func(family, tag string) taskengine.Engine {
			return taskengine.Engine{
				Binary: cfg.Taskmaster.Executable, Prefix: []string{"tasks"},
				Family: family, Tag: tag, Owner: lane.Owner, ClaimTTL: lane.LeaseTTL,
				Env: append(slices.DeleteFunc(os.Environ(), func(e string) bool {
					return strings.HasPrefix(e, "NOISE_PROJECT_DIR=")
				}), "NOISE_PROJECT_DIR="+family),
			}
		},
		RemoveProject: projectHandler.RemoveProject,
		StopSessions:  projectHandler.StopSessions,
		HoldSessions:  h.Manager.HoldProject,
		AllowedRoots:  cfg.Projects.AllowedRoots,
		Log:           log,
	})
	lane.NewHandler(h.Lanes, log).Mount(h.Server.Handle, h.Auth.Require)

	// The audit trail: the typed taxonomy every
	// repository writes through, and the user's ordered read of it.
	audit.NewHandler(audit.NewRepository(h.DB), log).
		Mount(h.Server.Handle, h.Auth.Require)

	// Automatic recovery runs after every later reconciliation pass. Manual
	// recovery is the explicit nonce-bound session endpoint mounted below.
	h.Reconciler.SetRecovery(h.Manager)
	// Cadence-loop pass failures (including recovery-sweep errors) are
	// logged, never silently discarded.
	h.Reconciler.SetLogger(log)
	session.NewHandler(h.Manager, h.Sessions, projects, log).
		Mount(h.Server.Handle, h.Auth.Require)

	// The chat ledger read side: what the console knows about
	// the chats a session's agent is in, read through the a2a client.
	chats.NewHandler(h.Sessions, cfg.Chats.Executable, log).
		Mount(h.Server.Handle, h.Auth.Require)

	// The retrieved-identity capture: each retrieved provider launch
	// registers a per-launch, one-shot token; the provisioned SessionStart
	// hook POSTs the native id to the token URL. The route is deliberately
	// OUTSIDE the session-auth middleware — the hook carries no session
	// credential; the unguessable one-shot token IS the credential and the
	// correlation.
	//
	// An expired capture is RECORDED without changing lifecycle status
	// without consuming a recovery attempt. A live session stays usable
	// instead of being withdrawn to report something the user cannot
	// repair. A detached row stays detached, and its next death
	// classification sees the durable loss marker rather than mistaking NULL
	// identity for proof that the session was empty.
	h.Server.Handle("POST /api/hooks/capture", hookCaptures.Handle())
	h.Server.Handle("POST /api/hooks/conversation", hookCaptures.ConversationHandler())
	go func() {
		tick := time.NewTicker(time.Minute)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				sweepCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				if err := hookCaptures.Sweep(sweepCtx); err != nil {
					log.Warn().Err(err).Msg("identity capture expiry sweep failed")
				}
				if err := h.Manager.ConvergeRecoveryCandidates(sweepCtx); err != nil {
					log.Warn().Err(err).Msg("recovery candidate convergence failed")
				}
				cancel()
			}
		}
	}()

	// The compiled browser shell is public so an unauthenticated browser can
	// render its Google sign-in action. More-specific API/auth/health routes
	// stay authoritative in the shared ServeMux; unknown reserved paths are
	// explicitly rejected by StaticHandler rather than receiving index.html.
	shell := server.StaticHandler(webassets.FS(), options.buildID)
	h.Server.Handle("GET /setup", h.Auth.SetupPage(shell))
	h.Server.Handle("GET /", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" && h.Auth.SetupPending() {
			w.Header().Set("Cache-Control", "no-store")
			http.Redirect(w, r, "/setup", http.StatusSeeOther)
			return
		}
		shell.ServeHTTP(w, r)
	}))

	// The font-substitution bench (default-off experiment): a separate page
	// in the same build, served only when the flag is on and always behind
	// the browser authentication. StaticHandler refuses bench.html
	// unconditionally, so with the flag off the path 404s like any unknown
	// one instead of hinting at a gated surface.
	if cfg.Experiments.FontBench {
		h.Server.Handle("GET /bench.html", h.Auth.Require(server.FontBenchHandler(webassets.FS())))
		log.Warn().Msg("font-bench experiment enabled")
	}

	// The dev preview (default-off experiment): a reverse proxy onto a Vite
	// dev server you start by hand, so a frontend change is
	// visible on a running deployment without rebuilding the embedded
	// assets and replacing the container — which would terminate every tmux
	// session and provider. It is same-origin with the app because the
	// session cookie is host-only and Path=/; a second hostname would
	// receive neither it nor the OIDC transaction cookie. Auth.Require is
	// not optional here: the dev server serves project source and executes
	// it with the user's authority.
	if cfg.Experiments.DevPreview {
		upstream, err := url.Parse("http://" + cfg.Experiments.DevPreviewUpstream)
		if err != nil {
			return nil, fmt.Errorf("dev-preview upstream: %w", err)
		}
		// Methods are explicit. A methodless subtree pattern next to the
		// "GET /" shell is a ServeMux conflict panic ("matches more methods
		// than GET /, but has a more specific path pattern"), so the route
		// names what a dev server actually receives: module and asset GETs,
		// the HMR upgrade (also a GET), HEADs, and the POST/OPTIONS a Vite
		// plugin middleware may add.
		preview := h.Auth.Require(server.DevPreviewHandler(upstream))
		for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodPost, http.MethodOptions} {
			h.Server.Handle(method+" "+server.DevPreviewPrefix, preview)
		}
		log.Warn().Str("upstream", cfg.Experiments.DevPreviewUpstream).
			Msg("dev-preview experiment enabled: /__dev/ proxies a local dev server with your authority")
	}

	// 9. Ready — every readiness precondition above holds.
	h.Server.SetReady(true)
	booted = true
	return h, nil
}
