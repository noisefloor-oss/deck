package tmux

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// DefaultTimeout bounds every tmux invocation. tmux commands are local and
// fast; a hang means the server is wedged, and blocking forever would turn
// that into a hung reconciler with no diagnostic.
const DefaultTimeout = 10 * time.Second

// Config parameterizes NewClient.
type Config struct {
	// SocketPath is the dedicated Deck tmux namespace. It must not be
	// the user's default tmux socket: this client kills and respawns panes.
	SocketPath string
	Timeout    time.Duration
	// ScrollbackLines is the tmux history-limit for panes in this namespace
	// (NOISEDECK_TMUX_SCROLLBACK_LINES). Zero or negative selects
	// DefaultScrollbackLines.
	ScrollbackLines int
}

// Client runs tmux against one dedicated socket.
//
// Every invocation is a fixed executable plus argv — never a shell — so no
// caller-supplied string is ever evaluated.
type Client struct {
	socketPath      string
	configPath      string
	timeout         time.Duration
	scrollbackLines int

	writeObserverMu  sync.RWMutex
	writeObserver    func(WriteObservation)
	attachObserverMu sync.RWMutex
	attachObserver   func(AttachObservation)
	testHooks        *clientTestHooks
}

// ErrNoSession reports that the named session or pane does not exist on a
// server we successfully reached.
var ErrNoSession = errors.New("tmux: no such session")

// ErrNoServer reports that the tmux server itself is unreachable — not
// running, or its socket is gone.
//
// This is deliberately NOT the same as ErrNoSession. A missing pane on a live
// server proves the process behind it is gone; an unreachable server proves
// nothing about any process, because tmux dying does not kill the panes'
// children. Conflating the two lets "we cannot see it" be reported as "it
// exited", which is precisely the kind of unverified claim this product must
// never make.
var ErrNoServer = errors.New("tmux: no server running")

// NewClient validates the socket location and prepares the server config.
//
// It does not start a tmux server: tmux exits a server with no sessions, so
// there is nothing to start until the first session is created.
func NewClient(cfg Config) (*Client, error) {
	if strings.TrimSpace(cfg.SocketPath) == "" {
		return nil, errors.New("tmux: Config.SocketPath is required")
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = DefaultTimeout
	}
	if cfg.ScrollbackLines <= 0 {
		cfg.ScrollbackLines = DefaultScrollbackLines
	}

	socketPath, err := filepath.Abs(cfg.SocketPath)
	if err != nil {
		return nil, fmt.Errorf("tmux: resolve socket path: %w", err)
	}
	dir := filepath.Dir(socketPath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("tmux: create socket directory: %w", err)
	}
	if err := verifySocketDir(dir); err != nil {
		return nil, err
	}

	configPath := filepath.Join(dir, "noisedeck.tmux.conf")
	serverConfig := fmt.Sprintf(serverConfigFormat, cfg.ScrollbackLines)
	if err := os.WriteFile(configPath, []byte(serverConfig), 0o600); err != nil {
		return nil, fmt.Errorf("tmux: write server config: %w", err)
	}

	return &Client{socketPath: socketPath, configPath: configPath,
		timeout: cfg.Timeout, scrollbackLines: cfg.ScrollbackLines}, nil
}

// SocketPath reports the socket this client drives.
func (c *Client) SocketPath() string { return c.socketPath }

// run executes tmux with the given arguments and returns trimmed stdout.
func (c *Client) run(ctx context.Context, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	full := append([]string{"-S", c.socketPath, "-f", c.configPath}, args...)
	cmd := exec.CommandContext(ctx, "tmux", full...)
	// A wedged server can hold the killed client's output pipes open. Without
	// WaitDelay, the tmux invocation remains stuck in Wait after its deadline.
	cmd.WaitDelay = time.Second

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	out := strings.TrimRight(stdout.String(), "\n")
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		if isServerUnreachable(msg) {
			return out, fmt.Errorf("%w: %s", ErrNoServer, msg)
		}
		if isMissingTarget(msg) {
			return out, fmt.Errorf("%w: %s", ErrNoSession, msg)
		}
		return out, fmt.Errorf("tmux %s: %s", strings.Join(args, " "), msg)
	}
	return out, nil
}

// isServerUnreachable matches failures that mean the tmux server is not there,
// as opposed to a target being absent from a server we did reach.
//
// The socket-missing case is narrowed to not-found deliberately: "error
// connecting to" also covers permission-denied, and treating THAT as an
// absent server would turn a socket-permission violation into a silent
// empty result.
func isServerUnreachable(msg string) bool {
	m := strings.ToLower(msg)
	return strings.Contains(m, "no server running") ||
		strings.Contains(m, "server exited unexpectedly") ||
		(strings.Contains(m, "error connecting to") &&
			strings.Contains(m, "no such file or directory"))
}

func isMissingTarget(msg string) bool {
	m := strings.ToLower(msg)
	return strings.Contains(m, "no such session") ||
		strings.Contains(m, "no such pane") ||
		strings.Contains(m, "can't find session") ||
		strings.Contains(m, "can't find pane") ||
		// A reachable server holding zero sessions. `exit-empty off` keeps such
		// a server alive, so this is a normal steady state for this namespace,
		// not a failure — tmux just has nothing to resolve a target against.
		strings.Contains(m, "no current target")
}
