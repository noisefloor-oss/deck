package workspaceruntime

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// Lock requires an already authorized, installation-owned preparation callback.
// It must fence/drain backend writers and durably stop the full session cohort.
// A preparation failure never authorizes container shutdown or unmounting.
// Reusing an ID only observes its effect; it never repeats uncertain signals.
func (c *Controller) Lock(ctx context.Context, id string, prepare func(context.Context) error) (Operation, error) {
	if err := c.acquire(ctx); err != nil {
		return Operation{}, err
	}
	defer c.release()
	if !identifier.MatchString(id) || prepare == nil || c.cfg.FusermountPath == "" {
		return Operation{}, ErrPrecondition
	}
	var kind string
	err := c.db.QueryRowContext(ctx, `SELECT kind FROM operations WHERE id=?`, id).Scan(&kind)
	if err == nil {
		if kind != "lock" {
			return Operation{}, ErrConflict
		}
		return c.operation(ctx, id)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Operation{}, ErrJournal
	}
	var unresolved int
	if c.db.QueryRowContext(ctx, `SELECT count(*) FROM operations WHERE status IN ('running','uncertain')`).Scan(&unresolved) != nil {
		return Operation{}, ErrJournal
	}
	if unresolved != 0 {
		return Operation{}, ErrUncertain
	}
	before, err := c.observe(ctx)
	if err != nil {
		return Operation{State: before}, err
	}
	if !before.Configured || before.Storage != "open" {
		return Operation{State: before}, ErrPrecondition
	}
	digest := c.configDigest()
	if digest == "" {
		return Operation{State: before}, ErrUnsafeStorage
	}
	op := Operation{ID: id, Kind: "lock", Status: "running", Stage: "preparing-stop", State: before}
	if _, err := c.db.ExecContext(ctx, `INSERT INTO operations(id,kind,status,stage,config_digest) VALUES(?,'lock','running',?,?)`, id, op.Stage, digest); err != nil {
		return Operation{}, ErrJournal
	}
	nativeErr := prepare(ctx)
	if nativeErr == nil {
		nativeErr = c.stage(ctx, &op, "stopping-runtime", digest)
	}
	if nativeErr == nil {
		_, nativeErr = c.command(ctx, c.cfg.SealedComposePath, []string{c.cfg.ComposeEnvFile, "down"}, nil, false)
	}
	if nativeErr == nil {
		nativeErr = c.projectGone(ctx)
	}
	if nativeErr == nil {
		nativeErr = c.stage(ctx, &op, "unmounting", digest)
	}
	if nativeErr == nil {
		// Neither lazy nor forced detach can establish that decrypted aliases and
		// processes are gone. A busy mount remains visibly open.
		_, nativeErr = c.command(ctx, c.cfg.FusermountPath, []string{"-u", c.cfg.MountPoint}, nil, false)
	}
	checkCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	op.State, err = c.observe(checkCtx)
	if errors.Is(nativeErr, ErrLockRefused) {
		op.Status, op.Stage, op.Code = "failed", "complete", "lock-preparation-refused"
	} else if nativeErr == nil && err == nil && op.State.Locked && digest == c.configDigest() {
		op.Status, op.Stage = "succeeded", "complete"
	} else {
		op.Status, op.Code = "uncertain", "lock-result-unconfirmed"
	}
	if _, err := c.db.ExecContext(checkCtx, `UPDATE operations SET status=?,stage=?,code=? WHERE id=?`, op.Status, op.Stage, op.Code, id); err != nil {
		return op, ErrJournal
	}
	if op.Status == "failed" {
		return op, ErrLockRefused
	}
	if op.Status != "succeeded" {
		return op, ErrUncertain
	}
	return op, nil
}

func (c *Controller) projectGone(ctx context.Context) error {
	for _, label := range []string{"com.docker.compose.project", "io.podman.compose.project"} {
		out, err := c.command(ctx, c.cfg.PodmanPath, []string{"ps", "--all", "--no-trunc", "--filter", "label=" + label + "=" + c.cfg.ComposeProject, "--format", "{{.ID}}"}, nil, true)
		if err != nil {
			return ErrUnavailable
		}
		if strings.TrimSpace(string(out)) != "" {
			return ErrPrecondition
		}
	}
	// A mislabeled configured app must not evade the project-wide query.
	_, err := c.run.run(ctx, c.cfg.PodmanPath, []string{"container", "exists", c.cfg.AppContainer}, c.environment(), nil, true)
	if exitCode(err) != 1 {
		return ErrUnavailable
	}
	return nil
}
