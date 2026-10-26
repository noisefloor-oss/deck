package tmux

import (
	"context"
	"errors"
	"fmt"
	"strconv"
)

// KillSession removes a tmux session. That is ALL it does.
//
// It does not stop the provider: a process ignoring SIGHUP/SIGTERM survives
// it (measured on tmux 3.6a). Worse, it destroys the dead pane and with it
// the exit evidence — a pane reporting `dead=1 status= signal=15` becomes
// unreadable immediately afterwards. That cause must be recorded, and
// native resume needs the dead pane itself, so this must NOT be called on
// the normal stop or exit path.
//
// Terminating a provider is session-lifecycle policy, not a tmux mechanism:
// it needs recorded identity, provider-specific behavior, explicit force
// authorization, and reconciliation to establish the outcome. That belongs in
// internal/session, which is why it does not live here.
func (c *Client) KillSession(ctx context.Context, name string) error {
	if err := ValidateSessionName(name); err != nil {
		return err
	}
	_, err := c.run(ctx, "kill-session", "-t", target(name))
	return err
}

// KillPaneIfIdentity compares the complete pane identity and kills it in one
// tmux server command. A mismatch is a clean applied=false result; no caller
// may turn a prior observation into authority for a later kill.
func (c *Client) KillPaneIfIdentity(ctx context.Context, identity PaneIdentity) (bool, error) {
	if err := ValidatePaneID(identity.PaneID); err != nil {
		return false, err
	}
	if !sessionIDPattern.MatchString(identity.SessionID) {
		return false, fmt.Errorf("tmux: %q is not a session id (expected $N)", identity.SessionID)
	}
	if !identityTextPattern.MatchString(identity.Marker) ||
		!identityTextPattern.MatchString(identity.LaunchMarker) {
		return false, errors.New("tmux: pane identity contains unsafe format syntax")
	}
	checks := []string{
		"#{==:#{session_id}," + identity.SessionID + "}",
		"#{==:#{pane_id}," + identity.PaneID + "}",
	}
	if identity.PanePID != 0 {
		if identity.PanePID <= 1 {
			return false, errors.New("tmux: pane identity contains an invalid pid")
		}
		checks = append(checks,
			"#{==:#{pane_pid},"+strconv.Itoa(identity.PanePID)+"}")
	}
	if identity.Generation != 0 || identity.Marker != "" {
		if identity.Generation < 1 || identity.Marker == "" {
			return false, errors.New("tmux: pane generation and marker must be supplied together")
		}
		checks = append(checks,
			"#{==:#{@noisedeck_generation},"+strconv.Itoa(identity.Generation)+"}",
			"#{==:#{@noisedeck_marker},"+identity.Marker+"}")
	}
	if identity.LaunchMarker != "" {
		checks = append(checks,
			"#{==:#{E:"+launchMarkerEnv+"},"+identity.LaunchMarker+"}")
	}
	if len(checks) == 2 {
		return false, errors.New("tmux: pane identity requires pane options or a launch marker")
	}
	if c.testHooks != nil && c.testHooks.beforeIdentityKill != nil {
		c.testHooks.beforeIdentityKill()
	}
	condition := checks[len(checks)-1]
	for i := len(checks) - 2; i >= 0; i-- {
		condition = "#{&&:" + checks[i] + "," + condition + "}"
	}
	thenCommand := "display-message -p killed ; kill-pane -t " + identity.PaneID
	out, err := c.run(ctx, "if-shell", "-F", "-t", identity.PaneID,
		condition, thenCommand, "display-message -p mismatch")
	if err != nil {
		return false, err
	}
	switch out {
	case "killed":
		return true, nil
	case "mismatch":
		return false, nil
	default:
		return false, fmt.Errorf("tmux: conditional pane kill returned %q", out)
	}
}

type pidWaiter struct {
	wait  func(context.Context) error
	close func()
}

// KillServer terminates the tmux server on this socket, and every session with
// it. Intended for test cleanup and manual recovery.
func (c *Client) KillServer(ctx context.Context) error {
	pidText, err := c.run(ctx, "display-message", "-p", "#{pid}")
	if err != nil && (errors.Is(err, ErrNoSession) || errors.Is(err, ErrNoServer)) {
		return nil
	}
	if err != nil {
		return err
	}
	pid, err := strconv.Atoi(pidText)
	if err != nil || pid <= 0 {
		return fmt.Errorf("tmux: invalid server pid %q", pidText)
	}
	wait, err := preparePIDWait(c, pid)
	if err != nil {
		return err
	}
	defer wait.close()
	if c.testHooks != nil && c.testHooks.beforeServerKill != nil {
		c.testHooks.beforeServerKill()
	}
	_, err = c.run(ctx, "kill-server")
	if err != nil && !errors.Is(err, ErrNoSession) && !errors.Is(err, ErrNoServer) {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	return wait.wait(ctx)
}
