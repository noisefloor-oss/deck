// Package workspaceruntime controls one installation's native encrypted store.
// It does not authenticate browsers or qualify a deployment's privacy boundary.
package workspaceruntime

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/noisefloor-oss/deck/internal/sealedstate"
)

var (
	ErrConfiguration = errors.New("workspace runtime: invalid installation configuration")
	ErrUnavailable   = errors.New("workspace runtime: native observation unavailable")
	ErrUnsafeStorage = errors.New("workspace runtime: storage gate refused")
	ErrPrecondition  = errors.New("workspace runtime: operation precondition failed")
	ErrCommand       = errors.New("workspace runtime: native command failed; inspect operation state")
	ErrLockRefused   = errors.New("workspace runtime: lock preparation refused")
	ErrUncertain     = errors.New("workspace runtime: an earlier operation needs reconciliation")
	ErrConflict      = errors.New("workspace runtime: operation ID already has another action")
	ErrNotFound      = errors.New("workspace runtime: operation not found")
	ErrJournal       = errors.New("workspace runtime: operation journal unavailable")
)

var identifier = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// Config is installation-owned. No field may come from a browser operation.
// The controller runs as OwnerUID in the host mount namespace, outside the
// workload. The existing shared-host/rootless-Podman gate is the only supported
// profile; setting these paths does not qualify the host.
type Config struct {
	SwapPolicy        sealedstate.SwapPolicy `json:",omitempty"`
	InstanceID        string
	OwnerUID          int
	CipherDir         string
	MountPoint        string
	ControlDir        string
	HomeDir           string
	RuntimeDir        string
	GocryptfsPath     string
	FusermountPath    string `json:",omitempty"`
	PodmanPath        string
	SealedComposePath string
	ComposeEnvFile    string
	ComposeProject    string
	AppContainer      string
	WorkspaceGateway  bool             `json:",omitempty"`
	Migration         *MigrationConfig `json:",omitempty"`
}

type State struct {
	// Locked additionally proves whole-project shutdown and no observed decrypted
	// aliases or volume daemon. A closed host mount alone is insufficient.
	Locked    bool   `json:"locked"`
	Migration string `json:"migration,omitempty"`
	Storage   string `json:"storage"` // closed, open, unknown
	// Runtime observes the configured app container. "stopped" is not proof
	// that every sidecar, CLI or mount alias has been stopped for an unmount.
	Runtime string `json:"runtime"` // stopped, starting, running, unknown
	// Configured means native metadata exists, not that it decrypts correctly.
	Configured bool `json:"configured"`
	// Epoch is absent when observations are uncertain. It is stable across
	// browser authorization and controller restart while native identity stays.
	Epoch string `json:"epoch"`
}

type Operation struct {
	ID     string `json:"id"`
	Kind   string `json:"kind"`
	Status string `json:"status"` // running, succeeded, uncertain
	Stage  string `json:"stage"`
	Code   string `json:"code,omitempty"`
	State  State  `json:"state"` // observed now, never a saved runtime receipt
}

type Controller struct {
	cfg             Config
	db              *sql.DB
	run             commandRunner
	kernel          func() (kernelFacts, error)
	mu              chan struct{}
	closedProcesses func(context.Context) error
}

func Open(cfg Config) (*Controller, error) {
	return open(cfg, execRunner{}, func() (kernelFacts, error) { return readKernelFor(cfg.MountPoint) })
}

func open(cfg Config, runner commandRunner, kernel func() (kernelFacts, error)) (*Controller, error) {
	if !cfg.SwapPolicy.Valid() {
		return nil, ErrConfiguration
	}
	if !identifier.MatchString(cfg.InstanceID) || !identifier.MatchString(cfg.ComposeProject) ||
		cfg.ComposeProject == "deck" || !identifier.MatchString(cfg.AppContainer) ||
		cfg.OwnerUID <= 0 || cfg.OwnerUID != os.Geteuid() {
		return nil, ErrConfiguration
	}
	for _, path := range []string{cfg.CipherDir, cfg.MountPoint, cfg.ControlDir, cfg.HomeDir,
		cfg.RuntimeDir, cfg.GocryptfsPath, cfg.PodmanPath, cfg.SealedComposePath, cfg.ComposeEnvFile} {
		if !filepath.IsAbs(path) || filepath.Clean(path) != path || strings.ContainsAny(path, "\x00\r\n") {
			return nil, ErrConfiguration
		}
	}
	if filepath.Base(cfg.PodmanPath) != "podman" || filepath.Base(cfg.GocryptfsPath) != "gocryptfs" {
		return nil, ErrConfiguration
	}
	if cfg.FusermountPath != "" && (!filepath.IsAbs(cfg.FusermountPath) || filepath.Clean(cfg.FusermountPath) != cfg.FusermountPath ||
		strings.ContainsAny(cfg.FusermountPath, "\x00\r\n") || filepath.Base(cfg.FusermountPath) != "fusermount3") {
		return nil, ErrConfiguration
	}
	for _, pair := range [][2]string{{cfg.CipherDir, cfg.MountPoint}, {cfg.ControlDir, cfg.MountPoint}, {cfg.ControlDir, cfg.CipherDir}} {
		if within(pair[0], pair[1]) || within(pair[1], pair[0]) {
			return nil, ErrConfiguration
		}
	}
	installationPaths := []string{cfg.HomeDir, cfg.RuntimeDir, cfg.GocryptfsPath, cfg.PodmanPath, cfg.SealedComposePath, cfg.ComposeEnvFile}
	if cfg.FusermountPath != "" {
		installationPaths = append(installationPaths, cfg.FusermountPath)
	}
	for _, path := range installationPaths {
		// The command and its engine state must remain available while the
		// encrypted store is closed, including through symlinked parents.
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil || within(path, cfg.MountPoint) || within(path, cfg.CipherDir) ||
			within(resolved, cfg.MountPoint) || within(resolved, cfg.CipherDir) {
			return nil, ErrConfiguration
		}
	}
	for _, path := range []string{cfg.CipherDir, cfg.MountPoint, cfg.ControlDir} {
		if !privateDirectory(path, cfg.OwnerUID) {
			return nil, ErrConfiguration
		}
	}
	if err := validateMigration(cfg); err != nil {
		return nil, err
	}
	journal := filepath.Join(cfg.ControlDir, "native.sqlite")
	// #nosec G304 -- The installation pins this private, owner-checked control directory.
	file, err := os.OpenFile(journal, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, ErrJournal
	}
	if err := file.Close(); err != nil {
		return nil, ErrJournal
	}
	if !privateFile(journal, cfg.OwnerUID) {
		return nil, ErrConfiguration
	}
	db, err := sql.Open("sqlite3", (&url.URL{Scheme: "file", Path: journal}).String()+"?_busy_timeout=5000&_journal_mode=WAL&_synchronous=FULL")
	if err != nil {
		return nil, ErrJournal
	}
	db.SetMaxOpenConns(1)
	ok := false
	defer func() {
		if !ok {
			_ = db.Close()
		}
	}()
	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS installation (id INTEGER PRIMARY KEY CHECK(id=1), fingerprint TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS operations (id TEXT PRIMARY KEY, kind TEXT NOT NULL, status TEXT NOT NULL, code TEXT NOT NULL DEFAULT '', stage TEXT NOT NULL, config_digest TEXT NOT NULL DEFAULT '');
CREATE TABLE IF NOT EXISTS initialization_reopens (initialization_id TEXT PRIMARY KEY, reopen_id TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS workspace_import (id INTEGER PRIMARY KEY CHECK(id=1), manifest_digest TEXT NOT NULL, cipher_digest TEXT NOT NULL);`)
	if err != nil {
		return nil, ErrJournal
	}
	raw, _ := json.Marshal(cfg)
	fingerprint := sha256.Sum256(raw)
	_, err = db.Exec(`INSERT OR IGNORE INTO installation(id,fingerprint) VALUES(1,?)`, hex.EncodeToString(fingerprint[:]))
	if err != nil {
		return nil, ErrJournal
	}
	var stored string
	if db.QueryRow(`SELECT fingerprint FROM installation WHERE id=1`).Scan(&stored) != nil || stored != hex.EncodeToString(fingerprint[:]) {
		return nil, ErrConfiguration
	}
	// An interrupted call may already have reached the kernel or engine.
	// Reopening the journal never dispatches its native command again.
	if _, err = db.Exec(`UPDATE operations SET status='uncertain',code='interrupted' WHERE status='running'`); err != nil {
		return nil, ErrJournal
	}
	ok = true
	c := &Controller{cfg: cfg, db: db, run: runner, kernel: kernel, mu: make(chan struct{}, 1)}
	c.closedProcesses = c.observeClosedProcesses
	return c, nil
}

// Close closes only the control journal. Mounts and real CLIs keep running.
func (c *Controller) Close() error { c.mu <- struct{}{}; defer c.release(); return c.db.Close() }

func (c *Controller) acquire(ctx context.Context) error {
	select {
	case c.mu <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (c *Controller) release() { <-c.mu }

func (c *Controller) Observe(ctx context.Context) (State, error) {
	if err := c.acquire(ctx); err != nil {
		return State{Storage: "unknown", Runtime: "unknown"}, err
	}
	defer c.release()
	state, err := c.observe(ctx)
	if err != nil {
		return state, err
	}
	if c.cfg.Migration != nil {
		switch check := c.migrationReady(ctx); {
		case check == nil:
			state.Migration = "complete"
		case errors.Is(check, ErrMigrationRequired):
			state.Migration = "pending"
		default:
			return State{Storage: "unknown", Runtime: "unknown"}, check
		}
	}
	return state, nil
}

func (c *Controller) Initialize(ctx context.Context, id string, secret []byte) (Operation, error) {
	return c.execute(ctx, id, "initialize", secret)
}

func (c *Controller) Unlock(ctx context.Context, id string, secret []byte) (Operation, error) {
	return c.execute(ctx, id, "unlock", secret)
}

func (c *Controller) Start(ctx context.Context, id string) (Operation, error) {
	return c.execute(ctx, id, "start", nil)
}

// Operation reconciles only effects visible in native state. It never retries
// a command. Initialize with the same ID can continue a durably recorded
// prepared/initialized stage; an effect already in flight is observe-only.
func (c *Controller) Operation(ctx context.Context, id string) (Operation, error) {
	if err := c.acquire(ctx); err != nil {
		return Operation{}, err
	}
	defer c.release()
	return c.operation(ctx, id)
}

func (c *Controller) operation(ctx context.Context, id string) (Operation, error) {
	if !identifier.MatchString(id) {
		return Operation{}, ErrPrecondition
	}
	var op Operation
	var digest string
	err := c.db.QueryRowContext(ctx, `SELECT id,kind,status,code,stage,config_digest FROM operations WHERE id=?`, id).Scan(&op.ID, &op.Kind, &op.Status, &op.Code, &op.Stage, &digest)
	if errors.Is(err, sql.ErrNoRows) {
		return op, ErrNotFound
	}
	if err != nil {
		return op, ErrJournal
	}
	op.State, err = c.observe(ctx)
	// A reboot can close the mount without committing the session stop cohort.
	// Only stages recorded after successful preparation can confirm that lock.
	canConfirm := op.Kind != "lock" || op.Stage == "stopping-runtime" || op.Stage == "unmounting"
	if err == nil && canConfirm && (op.Status == "uncertain" || op.Status == "running") && reached(op.Kind, op.State) && digest != "" && digest == c.configDigest() {
		if _, e := c.db.ExecContext(ctx, `UPDATE operations SET status='succeeded',code='',stage='complete' WHERE id=?`, id); e != nil {
			return op, ErrJournal
		}
		op.Status, op.Code, op.Stage = "succeeded", "", "complete"
	}
	return op, err
}

func reached(kind string, s State) bool {
	if kind == "lock" {
		return s.Locked
	}
	if kind == "start" {
		return s.Storage == "open" && s.Runtime == "running"
	}
	return s.Configured && s.Storage == "open"
}

func (c *Controller) execute(ctx context.Context, id, kind string, secret []byte) (Operation, error) {
	if err := c.acquire(ctx); err != nil {
		return Operation{}, err
	}
	defer c.release()
	return c.executeLocked(ctx, id, kind, secret)
}

func (c *Controller) executeLocked(ctx context.Context, id, kind string, secret []byte) (Operation, error) {
	if !identifier.MatchString(id) || (kind != "start" && len(secret) != 32) {
		return Operation{}, ErrPrecondition
	}
	if kind == "start" {
		if err := c.migrationReady(ctx); err != nil {
			return Operation{}, err
		}
	}
	op := Operation{ID: id, Kind: kind, Status: "running", Stage: "prepared"}
	var digest string
	err := c.db.QueryRowContext(ctx, `SELECT kind,status,stage,config_digest FROM operations WHERE id=?`, id).Scan(&op.Kind, &op.Status, &op.Stage, &digest)
	exists := err == nil
	if exists {
		if op.Kind != kind {
			return Operation{}, ErrConflict
		}
		if op.Stage != "prepared" && (kind != "initialize" || op.Stage != "initialized") {
			return c.operation(ctx, id)
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return Operation{}, ErrJournal
	}
	var unresolved int
	if c.db.QueryRowContext(ctx, `SELECT count(*) FROM operations WHERE id != ? AND status IN ('running','uncertain')`, id).Scan(&unresolved) != nil {
		return Operation{}, ErrJournal
	}
	if unresolved != 0 {
		return Operation{}, ErrUncertain
	}
	before, err := c.observe(ctx)
	if err != nil {
		return Operation{State: before}, err
	}
	if kind == "initialize" && op.Stage == "prepared" && (before.Configured || before.Storage != "closed" || before.Runtime != "stopped") {
		return Operation{State: before}, ErrPrecondition
	}
	if (kind == "unlock" || op.Stage == "initialized") && (!before.Configured || before.Storage != "closed" || before.Runtime != "stopped") {
		return Operation{State: before}, ErrPrecondition
	}
	if kind == "start" && (before.Storage != "open" || before.Runtime != "stopped") {
		return Operation{State: before}, ErrPrecondition
	}
	if !privateDirectory(c.cfg.CipherDir, c.cfg.OwnerUID) || !privateDirectory(c.cfg.MountPoint, c.cfg.OwnerUID) {
		return Operation{State: before}, ErrUnsafeStorage
	}
	if kind == "initialize" && op.Stage == "prepared" && !emptyDirectory(c.cfg.CipherDir) {
		return Operation{State: before}, ErrPrecondition
	}
	if kind != "start" && !emptyDirectory(c.cfg.MountPoint) {
		return Operation{State: before}, ErrPrecondition
	}
	if before.Configured {
		current := c.configDigest()
		if current == "" || (digest != "" && digest != current) {
			return Operation{State: before}, ErrUnsafeStorage
		}
		digest = current
	}
	if !exists {
		if _, err = c.db.ExecContext(ctx, `INSERT INTO operations(id,kind,status,stage,config_digest) VALUES(?,?,'running','prepared',?)`, id, kind, digest); err != nil {
			return Operation{}, ErrJournal
		}
	}
	op.State = before
	var nativeErr error
	if kind == "start" {
		nativeErr = c.stage(ctx, &op, "starting", digest)
		if nativeErr == nil {
			nativeErr = c.start(ctx)
		}
	} else {
		password := make([]byte, base64.RawURLEncoding.EncodedLen(len(secret))+1)
		base64.RawURLEncoding.Encode(password[:len(password)-1], secret)
		password[len(password)-1] = '\n'
		defer clear(password)
		if kind == "initialize" && op.Stage == "prepared" {
			nativeErr = c.stage(ctx, &op, "initializing", "")
			if nativeErr == nil {
				_, nativeErr = c.command(ctx, c.cfg.GocryptfsPath, []string{"-q", "-init", "-passfile", "/dev/stdin", c.cfg.CipherDir}, password, false)
			}
			if nativeErr == nil {
				digest = c.configDigest()
				if digest == "" {
					nativeErr = ErrUnsafeStorage
				} else {
					nativeErr = c.stage(ctx, &op, "initialized", digest)
				}
			}
		}
		if nativeErr == nil {
			nativeErr = c.stage(ctx, &op, "mounting", digest)
		}
		if nativeErr == nil {
			_, nativeErr = c.command(ctx, c.cfg.GocryptfsPath, []string{"-q", "-nosyslog", "-passfile", "/dev/stdin", c.cfg.CipherDir, c.cfg.MountPoint}, password, false)
		}
	}
	// Cancellation says nothing about a daemonized mount or an engine launch.
	// Observe independently, but do not stop either to manufacture a result.
	checkCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	op.State, err = c.observe(checkCtx)
	if nativeErr == nil && err == nil && reached(kind, op.State) && digest != "" && digest == c.configDigest() {
		op.Status, op.Stage = "succeeded", "complete"
	} else if nativeErr == nil && err == nil && kind == "start" && op.State.Storage == "open" && op.State.Runtime == "starting" {
		op.Status = "running"
	} else {
		op.Status, op.Code = "uncertain", "native-result-unconfirmed"
	}
	if _, e := c.db.ExecContext(checkCtx, `UPDATE operations SET status=?,code=?,stage=? WHERE id=?`, op.Status, op.Code, op.Stage, id); e != nil {
		return op, ErrJournal
	}
	if op.Status == "uncertain" {
		return op, ErrCommand
	}
	return op, nil
}

func (c *Controller) stage(ctx context.Context, op *Operation, stage, digest string) error {
	if _, err := c.db.ExecContext(ctx, `UPDATE operations SET stage=?,config_digest=? WHERE id=?`, stage, digest, op.ID); err != nil {
		return ErrJournal
	}
	op.Stage = stage
	return nil
}

func (c *Controller) configDigest() string {
	path := filepath.Join(c.cfg.CipherDir, "gocryptfs.conf")
	if !privateFile(path, c.cfg.OwnerUID) {
		return ""
	}
	// #nosec G304 -- This fixed native config is under the installation-owned cipher directory.
	file, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(io.LimitReader(file, 1024*1024+1))
	if err != nil || len(data) == 0 || len(data) > 1024*1024 {
		return ""
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func (c *Controller) start(ctx context.Context) error {
	// Provision only after the root itself passed the live mount gate. The
	// wrapper then qualifies every store, generates its launch-bound report,
	// and requires the container's independent sealed-check before returning.
	if err := c.prepareStores(); err != nil {
		return err
	}
	_, err := c.command(ctx, c.cfg.SealedComposePath, []string{c.cfg.ComposeEnvFile, "up"}, nil, false)
	return err
}

func (c *Controller) environment() []string {
	path := filepath.Dir(c.cfg.PodmanPath) + ":/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
	env := []string{"PATH=" + path, "HOME=" + c.cfg.HomeDir, "XDG_RUNTIME_DIR=" + c.cfg.RuntimeDir, "LANG=C.UTF-8",
		"COMPOSE_PROJECT_NAME=" + c.cfg.ComposeProject, "NOISEDECK_SEALED_PROFILE=shared-host",
		"NOISEDECK_SWAP_POLICY=" + string(c.cfg.SwapPolicy.Effective()),
		"NOISEDECK_SEALED_STATE_ROOT=" + c.cfg.MountPoint, "NOISEDECK_COMPOSE_COMMAND=podman compose"}
	if c.cfg.WorkspaceGateway {
		env = append(env, "NOISEDECK_WORKSPACE_GATEWAY=true")
	}
	return env
}

func (c *Controller) command(ctx context.Context, path string, args []string, input []byte, capture bool) ([]byte, error) {
	out, err := c.run.run(ctx, path, args, c.environment(), input, capture)
	if err != nil {
		return nil, ErrCommand
	}
	return out, nil
}

func within(path, root string) bool {
	return path == root || strings.HasPrefix(path, root+string(os.PathSeparator))
}

func emptyDirectory(path string) bool {
	// #nosec G304 -- Callers supply only the installation-pinned cipher and mount directories.
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	names, err := f.Readdirnames(1)
	return len(names) == 0 && errors.Is(err, io.EOF)
}

func epoch(boot string, mountID int, mountInstance uint64, device, container string) string {
	sum := sha256.Sum256([]byte(boot + "\x00" + strconv.Itoa(mountID) + "\x00" + strconv.FormatUint(mountInstance, 10) + "\x00" + device + "\x00" + container))
	return fmt.Sprintf("%x", sum)
}
