// Package workspaceplain starts one explicitly unencrypted gateway installation.
// It does not select an onboarding mode or authenticate a browser.
package workspaceplain

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

var (
	ErrConfiguration = errors.New("plain workspace: invalid installation configuration")
	ErrStorage       = errors.New("plain workspace: unrecognized or changed state root")
	ErrUnavailable   = errors.New("plain workspace: runtime observation unavailable")
	ErrPrecondition  = errors.New("plain workspace: start precondition failed")
	ErrUncertain     = errors.New("plain workspace: previous start needs observation")
	ErrJournal       = errors.New("plain workspace: journal unavailable")
	ErrNotFound      = errors.New("plain workspace: operation not found")
	ErrCommand       = errors.New("plain workspace: command outcome unconfirmed")
)

var identifier = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)

// Config comes only from the installation, never an HTTP request. StateRoot is
// an initially empty, owner-only directory separate from encrypted storage.
type Config struct {
	InstanceID     string
	OwnerUID       int
	StateRoot      string
	ControlDir     string
	HomeDir        string
	RuntimeDir     string
	PodmanPath     string
	ComposePath    string
	ComposeEnvFile string
	ComposeProject string
	AppContainer   string
	PublicOrigin   string
}

// State describes plaintext storage and the app process. It never certifies
// encrypted storage or a locked workspace. Epoch follows observed identity.
type State struct {
	Runtime    string `json:"runtime"`
	Configured bool   `json:"configured"`
	Epoch      string `json:"epoch"`
}

type Operation struct {
	ID     string `json:"id"`
	Kind   string `json:"kind"`
	Status string `json:"status"`
	Stage  string `json:"stage"`
	Code   string `json:"code,omitempty"`
	State  State  `json:"state"`
}

type Controller struct {
	cfg          Config
	db           *sql.DB
	run          commandRunner
	process      func(int, int) (string, error)
	boot         func() (string, error)
	mu           chan struct{}
	fingerprint  string
	rootIdentity identity
}

// ValidateConfig checks paths without creating journals or workload stores.
func ValidateConfig(cfg Config) error {
	if !identifier.MatchString(cfg.InstanceID) || !identifier.MatchString(cfg.ComposeProject) ||
		cfg.ComposeProject == "deck" || !identifier.MatchString(cfg.AppContainer) ||
		cfg.OwnerUID <= 0 || cfg.OwnerUID != os.Geteuid() {
		return ErrConfiguration
	}
	if len(filepath.Join(cfg.StateRoot, "data/gateway/backend.sock")) >= 108 {
		return ErrConfiguration
	}
	u, err := url.Parse(cfg.PublicOrigin)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.String() != cfg.PublicOrigin {
		return ErrConfiguration
	}
	paths := []string{cfg.StateRoot, cfg.ControlDir, cfg.HomeDir, cfg.RuntimeDir, cfg.PodmanPath, cfg.ComposePath, cfg.ComposeEnvFile}
	for _, p := range paths {
		if !filepath.IsAbs(p) || filepath.Clean(p) != p || p == "/" || strings.ContainsAny(p, "\x00\r\n") {
			return ErrConfiguration
		}
		resolved, e := filepath.EvalSymlinks(p)
		if e != nil || resolved != p {
			return ErrConfiguration
		}
	}
	for _, p := range []string{cfg.ControlDir, cfg.HomeDir, cfg.RuntimeDir, cfg.PodmanPath, cfg.ComposePath, cfg.ComposeEnvFile} {
		if within(p, cfg.StateRoot) || within(cfg.StateRoot, p) {
			return ErrConfiguration
		}
	}
	for _, p := range []string{cfg.StateRoot, cfg.ControlDir, cfg.HomeDir, cfg.RuntimeDir} {
		if _, err := privateIdentity(p, cfg.OwnerUID, true); err != nil {
			return ErrConfiguration
		}
	}
	if filepath.Base(cfg.PodmanPath) != "podman" {
		return ErrConfiguration
	}
	for _, p := range []string{cfg.PodmanPath, cfg.ComposePath} {
		fi, e := os.Stat(p)
		if e != nil || !fi.Mode().IsRegular() || fi.Mode().Perm()&0022 != 0 || fi.Mode().Perm()&0111 == 0 {
			return ErrConfiguration
		}
	}
	if _, err := privateIdentity(cfg.ComposeEnvFile, cfg.OwnerUID, false); err != nil {
		return ErrConfiguration
	}
	return nil
}

func Open(cfg Config) (*Controller, error) {
	return open(cfg, execRunner{}, processIdentity, bootIdentity)
}

func open(cfg Config, runner commandRunner, process func(int, int) (string, error), boot func() (string, error)) (*Controller, error) {
	if err := ValidateConfig(cfg); err != nil {
		return nil, err
	}
	root, err := privateIdentity(cfg.StateRoot, cfg.OwnerUID, true)
	if err != nil {
		return nil, ErrStorage
	}
	// A separate name allows a gateway control directory to also contain the
	// encrypted controller's dormant journal. The registry owns mode choice.
	journal := filepath.Join(cfg.ControlDir, "plain-native.sqlite")
	if _, err := os.Lstat(journal); err == nil {
		if _, err = privateIdentity(journal, cfg.OwnerUID, false); err != nil {
			return nil, ErrConfiguration
		}
	} else if !os.IsNotExist(err) {
		return nil, ErrConfiguration
	}
	// #nosec G304 -- Installation-owned private control directory, checked above.
	f, err := os.OpenFile(journal, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, ErrJournal
	}
	if f.Close() != nil {
		return nil, ErrJournal
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
	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS installation(id INTEGER PRIMARY KEY CHECK(id=1),fingerprint TEXT NOT NULL,root TEXT NOT NULL,layout TEXT NOT NULL DEFAULT '{}');
CREATE TABLE IF NOT EXISTS operations(id TEXT PRIMARY KEY,status TEXT NOT NULL,stage TEXT NOT NULL,code TEXT NOT NULL DEFAULT '');`)
	if err != nil {
		return nil, ErrJournal
	}
	raw, _ := json.Marshal(cfg)
	digest := sha256.Sum256(raw)
	fingerprint := hex.EncodeToString(digest[:])
	rootRaw, _ := json.Marshal(root)
	_, err = db.Exec(`INSERT OR IGNORE INTO installation(id,fingerprint,root) VALUES(1,?,?)`, fingerprint, string(rootRaw))
	if err != nil {
		return nil, ErrJournal
	}
	var savedFingerprint, savedRoot string
	if db.QueryRow(`SELECT fingerprint,root FROM installation WHERE id=1`).Scan(&savedFingerprint, &savedRoot) != nil {
		return nil, ErrJournal
	}
	if savedFingerprint != fingerprint || savedRoot != string(rootRaw) {
		return nil, ErrStorage
	}
	c := &Controller{cfg: cfg, db: db, run: runner, process: process, boot: boot, mu: make(chan struct{}, 1), fingerprint: fingerprint, rootIdentity: root}
	if _, err = c.layout(false); err != nil {
		return nil, err
	}
	if _, err = db.Exec(`UPDATE operations SET status='uncertain',code='interrupted' WHERE status='running'`); err != nil {
		return nil, ErrJournal
	}
	ok = true
	return c, nil
}

// Close releases only control resources. It never stops a container or CLI.
func (c *Controller) Close() error {
	c.mu <- struct{}{}
	defer c.release()
	return c.db.Close()
}
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
		return State{Runtime: "unknown"}, err
	}
	defer c.release()
	return c.observe(ctx)
}

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
	op := Operation{ID: id, Kind: "start"}
	err := c.db.QueryRowContext(ctx, `SELECT status,stage,code FROM operations WHERE id=?`, id).Scan(&op.Status, &op.Stage, &op.Code)
	if errors.Is(err, sql.ErrNoRows) {
		return Operation{}, ErrNotFound
	}
	if err != nil {
		return op, ErrJournal
	}
	op.State, err = c.observe(ctx)
	if err == nil && op.Stage == "dispatched" && op.State.Runtime == "running" && op.State.Configured {
		if _, e := c.db.ExecContext(ctx, `UPDATE operations SET status='succeeded',stage='complete',code='' WHERE id=?`, id); e != nil {
			return op, ErrJournal
		}
		op.Status, op.Stage, op.Code = "succeeded", "complete", ""
	}
	if err == nil && (op.Status == "uncertain" || op.Status == "running") && op.Stage == "dispatched" && op.State.Configured && op.State.Runtime == "stopped" {
		if _, e := c.db.ExecContext(ctx, `UPDATE operations SET status='failed',stage='complete' WHERE id=?`, id); e != nil {
			return op, ErrJournal
		}
		op.Status, op.Stage = "failed", "complete"
	}
	return op, err
}

// Start is authorized outside this package. A recorded dispatched ID is always
// observe-only. A prepared ID can finish only its owned provisioning stages.
func (c *Controller) Start(ctx context.Context, id string) (Operation, error) {
	if !identifier.MatchString(id) {
		return Operation{}, ErrPrecondition
	}
	if err := c.acquire(ctx); err != nil {
		return Operation{}, err
	}
	defer c.release()
	op, err := c.operation(ctx, id)
	if err == nil && op.Stage != "prepared" {
		return op, nil
	}
	if err != nil && !errors.Is(err, ErrNotFound) {
		return op, err
	}
	if errors.Is(err, ErrNotFound) {
		var pending int
		if c.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM operations WHERE status NOT IN ('succeeded','failed')`).Scan(&pending) != nil {
			return op, ErrJournal
		}
		if pending != 0 {
			return op, ErrUncertain
		}
		s, e := c.observe(ctx)
		if e != nil {
			return op, e
		}
		if s.Runtime != "stopped" && s.Runtime != "running" {
			return op, ErrPrecondition
		}
		op = Operation{ID: id, Kind: "start", Status: "running", Stage: "prepared", State: s}
		if s.Runtime == "running" {
			op.Status, op.Stage = "succeeded", "complete"
		}
		if _, e = c.db.ExecContext(ctx, `INSERT INTO operations(id,status,stage) VALUES(?,?,?)`, id, op.Status, op.Stage); e != nil {
			return op, ErrJournal
		}
		if op.Status == "succeeded" {
			return op, nil
		}
	}
	if err := c.prepare(); err != nil {
		return c.failed(id, "provisioning", err)
	}
	// Persist dispatch before invoking Compose. A lost result cannot cause a
	// second command that might replace a container and its running sessions.
	if _, err := c.db.ExecContext(ctx, `UPDATE operations SET status='running',stage='dispatched',code='' WHERE id=?`, id); err != nil {
		return op, ErrJournal
	}
	launchCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	_, err = c.command(launchCtx, c.cfg.ComposePath, []string{c.cfg.ComposeEnvFile, "up"}, false)
	if err != nil {
		return c.failed(id, "launch-unconfirmed", ErrCommand)
	}
	op, err = c.operation(ctx, id)
	if err != nil {
		return c.failed(id, "observation-unavailable", err)
	}
	return op, nil
}

func (c *Controller) failed(id, code string, cause error) (Operation, error) {
	if _, err := c.db.Exec(`UPDATE operations SET status='uncertain',code=? WHERE id=?`, code, id); err != nil {
		return Operation{ID: id, Kind: "start", Status: "uncertain", State: State{Runtime: "unknown"}}, ErrJournal
	}
	op, _ := c.operation(context.Background(), id)
	return op, cause
}

func within(p, root string) bool {
	return p == root || strings.HasPrefix(p, root+string(os.PathSeparator))
}
