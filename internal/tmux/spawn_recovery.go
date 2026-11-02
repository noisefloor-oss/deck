package tmux

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	spawnTransitionPoll  = 5 * time.Millisecond
	spawnTransitionLimit = 500 * time.Millisecond
	spawnStabilityWindow = 50 * time.Millisecond
)

// ErrPartialSpawnCleaned accompanies a CreateSession failure only when the
// exact process group first returned by new-session is proven gone and its
// exact pane is killed or positively absent. Callers may use it to revoke
// launch-scoped capabilities; its absence means cleanup was uncertain.
var ErrPartialSpawnCleaned = errors.New("tmux: partial spawn cleanup complete")

func createdPaneInfo(out string, spec SessionSpec) (*PaneInfo, error) {
	parts := strings.Split(out, "\t")
	if len(parts) != 3 || !sessionIDPattern.MatchString(parts[0]) {
		return nil, fmt.Errorf("tmux: new-session returned invalid identity %q", out)
	}
	if err := ValidatePaneID(parts[1]); err != nil {
		return nil, fmt.Errorf("tmux: new-session returned invalid identity %q: %w", out, err)
	}
	pid, err := strconv.Atoi(parts[2])
	if err != nil || pid <= 0 {
		return nil, fmt.Errorf("tmux: new-session returned invalid pane pid %q", parts[2])
	}
	info := &PaneInfo{
		SessionID: parts[0], SessionName: spec.Name, PaneID: parts[1], PID: pid,
		LaunchMarker: spec.Marker,
	}
	if pgid, ticks, exe, err := processIdentity(pid); err == nil {
		info.StartTicks, info.Executable = ticks, exe
		if pgid == pid {
			info.ProcessGroupID = pgid
		}
	}
	return markSpawnOrigin(info), nil
}

func (c *Client) recoverCreatedPane(ctx context.Context, spec SessionSpec, partial *PaneInfo) (*PaneInfo, error) {
	observeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), c.timeout)
	defer cancel()
	var (
		observed *PaneInfo
		err      error
	)
	if partial != nil {
		observed, err = c.observeSpawnedPane(observeCtx, partial.PaneID)
	} else {
		observed, err = c.PaneInfoForSession(observeCtx, spec.Name)
	}
	if err != nil {
		if partial != nil && errors.Is(err, ErrNoSession) {
			return partial, nil
		}
		if partial == nil && errors.Is(err, ErrNoSession) {
			return nil, nil
		}
		return partial, fmt.Errorf("tmux: recover created pane identity: %w", err)
	}
	if observed.SessionName != spec.Name || observed.LaunchMarker != spec.Marker {
		return nil, errors.New("tmux: session found after create error did not match its launch identity")
	}
	if partial != nil && (observed.SessionID != partial.SessionID || observed.PaneID != partial.PaneID) {
		return partial, errors.New("tmux: created pane identity changed before rollback")
	}
	if partial == nil {
		return markSpawnOrigin(preserveSpawnOriginObservation(partial, observed)), nil
	}
	return preserveSpawnOriginObservation(partial, observed), nil
}

func (c *Client) rollbackPartialSpawn(ctx context.Context, info *PaneInfo, cause error) error {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), c.timeout)
	defer cancel()
	observed, observeErr := c.PaneInfoByID(cleanupCtx, info.PaneID)
	if observeErr != nil {
		return errors.Join(cause,
			fmt.Errorf("tmux: partial spawn identity unavailable before rollback: %w", observeErr))
	}
	info = preserveProcessIdentity(info, observed)
	if !info.MatchesSpawnOrigin() {
		return errors.Join(cause,
			errors.New("tmux: partial spawn physical identity changed before rollback"))
	}

	// Stop the process group while the complete physical pane identity still
	// verifies. Removing the pane first would discard the final identity fence
	// and turn a later process-group signal into an effect on an unverifiable
	// runtime.
	processErr := terminatePartialProcess(cleanupCtx, info)
	if processErr != nil {
		return errors.Join(cause, processErr)
	}

	var paneErr error
	applied, err := c.KillPaneIfIdentity(cleanupCtx, paneIdentity(info))
	if err != nil && !errors.Is(err, ErrNoSession) {
		paneErr = fmt.Errorf("tmux: roll back partial pane: %w", err)
	} else if err == nil && !applied {
		paneErr = errors.New("tmux: partial spawn identity changed before rollback")
	}
	if paneErr != nil {
		return errors.Join(cause, paneErr)
	}
	return errors.Join(cause, ErrPartialSpawnCleaned)
}

func terminatePartialProcess(ctx context.Context, info *PaneInfo) error {
	pgid := info.ProcessGroupID
	if info.Dead {
		if info.PID <= 1 {
			return errors.New("tmux: dead partial spawn has no pane process group")
		}
		if pgid > 1 && pgid != info.PID {
			return fmt.Errorf("tmux: dead pane pid %d disagrees with verified process group %d",
				info.PID, pgid)
		}
		// tmux creates the pane leader as its process-group leader and retains
		// pane_pid after reaping it. Children that did not escape with setsid
		// therefore remain enumerable in this group after a rapid leader exit.
		pgid = info.PID
	} else {
		if info.PID <= 0 || info.StartTicks <= 0 || pgid <= 1 {
			return errors.New("tmux: partial spawn process identity is incomplete")
		}
		alive, err := ProcAlive(info.PID, info.StartTicks)
		if err != nil {
			return fmt.Errorf("tmux: verify partial spawn process: %w", err)
		}
		if alive {
			gotPGID, err := ProcGroup(info.PID, info.StartTicks)
			if err != nil {
				return fmt.Errorf("tmux: resolve partial spawn process group: %w", err)
			}
			if gotPGID != info.ProcessGroupID {
				return fmt.Errorf("tmux: partial spawn process moved from group %d to %d",
					info.ProcessGroupID, gotPGID)
			}
		}
	}
	members, err := GroupMembers(pgid)
	if err != nil {
		return fmt.Errorf("tmux: enumerate partial spawn process group: %w", err)
	}
	if len(members) == 0 {
		return nil
	}
	if err := syscall.Kill(-pgid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("tmux: kill partial spawn process group: %w", err)
	}
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		members, err = GroupMembers(pgid)
		if err != nil {
			return fmt.Errorf("tmux: verify partial spawn process group exit: %w", err)
		}
		if len(members) == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("tmux: partial spawn process survived cleanup: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

func preserveProcessIdentity(prior, observed *PaneInfo) *PaneInfo {
	if prior == nil || observed == nil {
		return observed
	}
	if (observed.Generation == 0) != (observed.Marker == "") {
		observed.Generation = prior.Generation
		observed.Marker = prior.Marker
	}
	observed.SpawnPID = prior.SpawnPID
	observed.SpawnProcessGroupID = prior.SpawnProcessGroupID
	observed.SpawnStartTicks = prior.SpawnStartTicks
	if observed.SessionID == prior.SessionID && observed.PaneID == prior.PaneID &&
		observed.PID == prior.PID && observed.StartTicks == 0 && prior.StartTicks > 0 {
		observed.ProcessGroupID = prior.ProcessGroupID
		observed.StartTicks = prior.StartTicks
	}
	return observed
}

// preserveSpawnOriginObservation keeps the recorded spawn origin across an
// observation, repairing an origin that creation could not complete. A pane
// child can lose the race with its own exec: new-session has returned its
// pid, but the process-table read at creation time fails and the origin is
// stamped without a process group or start time. An observation of the same
// pid carrying a complete tuple still describes the process new-session
// returned, so it anchors the origin rather than being mistaken for a
// replacement of it.
func preserveSpawnOriginObservation(prior, observed *PaneInfo) *PaneInfo {
	if prior != nil && observed != nil &&
		prior.SpawnPID == observed.PID &&
		prior.SpawnProcessGroupID == 0 && prior.SpawnStartTicks == 0 &&
		observed.ProcessGroupID > 1 && observed.StartTicks > 0 {
		return markSpawnOrigin(observed)
	}
	// Creation can read the child before setsid, while it still belongs to
	// tmux's group. Its settled group anchors the origin only when the pid
	// and start ticks still match the process returned by new-session.
	if prior != nil && observed != nil &&
		prior.SpawnPID == observed.PID &&
		prior.SpawnStartTicks == observed.StartTicks &&
		observed.StartTicks > 0 &&
		observed.ProcessGroupID == observed.PID &&
		prior.SpawnProcessGroupID != observed.PID {
		return markSpawnOrigin(observed)
	}
	return preserveProcessIdentity(prior, observed)
}

func markSpawnOrigin(info *PaneInfo) *PaneInfo {
	if info != nil {
		info.SpawnPID = info.PID
		info.SpawnProcessGroupID = info.ProcessGroupID
		info.SpawnStartTicks = info.StartTicks
	}
	return info
}

// MatchesSpawnOrigin reports whether an observation still describes the
// process first returned by new-session. A dead pane retains only its leader
// PID; a live process must retain the full PID/start/PGID tuple.
func (p *PaneInfo) MatchesSpawnOrigin() bool {
	if p == nil || p.SpawnPID <= 1 || p.PID != p.SpawnPID {
		return false
	}
	if p.Dead {
		return true
	}
	return p.SpawnProcessGroupID > 1 && p.SpawnStartTicks > 0 &&
		p.ProcessGroupID == p.SpawnProcessGroupID && p.StartTicks == p.SpawnStartTicks
}

var errPaneGroupPending = errors.New("tmux: pane process group has not settled")

// paneObservationPending identifies process-table races worth retrying within
// spawnTransitionLimit. Persistent errors still fail with their original cause;
// a child in tmux's group must finish setsid before its group is safe to signal.
func paneObservationPending(err error) bool {
	return err != nil && (errors.Is(err, os.ErrNotExist) ||
		errors.Is(err, syscall.ESRCH) || errors.Is(err, syscall.EACCES) ||
		errors.Is(err, errPaneGroupPending))
}

// observeSpawnedPane waits for a pane child to finish setsid, or for tmux to
// publish its death after it leaves /proc. Those observations are incomplete,
// so retry them within the transition budget. Every other error remains fatal.
func (c *Client) observeSpawnedPane(ctx context.Context, paneID string) (*PaneInfo, error) {
	info, err := c.PaneInfoByID(ctx, paneID)
	if err == nil || !paneObservationPending(err) || errors.Is(err, context.DeadlineExceeded) {
		return info, err
	}

	observeCtx, cancel := context.WithTimeout(ctx, spawnTransitionLimit)
	defer cancel()
	ticker := time.NewTicker(spawnTransitionPoll)
	defer ticker.Stop()
	for {
		info, err = c.PaneInfoByID(observeCtx, paneID)
		if err == nil || !paneObservationPending(err) {
			return info, err
		}
		select {
		case <-observeCtx.Done():
			return nil, errors.Join(err, observeCtx.Err())
		case <-ticker.C:
		}
	}
}

// PaneInfoAfterSpawn waits through the immediate-exit window before reading a
// newly spawned pane. The previously captured live tuple is retained only
// when the session, pane, and PID still match, so cleanup can prove whether
// children survived after tmux publishes the leader's death.
func (c *Client) PaneInfoAfterSpawn(ctx context.Context, spawned *PaneInfo) (*PaneInfo, error) {
	if spawned == nil {
		return nil, errors.New("tmux: spawned pane identity is required")
	}
	if err := ValidatePaneID(spawned.PaneID); err != nil {
		return nil, err
	}
	timer := time.NewTimer(spawnStabilityWindow)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-timer.C:
	}
	observed, err := c.observeSpawnedPane(ctx, spawned.PaneID)
	if err != nil {
		return nil, err
	}
	return preserveProcessIdentity(spawned, observed), nil
}
