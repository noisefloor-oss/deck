package workspacehost

import (
	"context"
	"errors"
	"time"

	"github.com/noisefloor-oss/deck/internal/auth"
	"github.com/noisefloor-oss/deck/internal/workspacegateway"
	"github.com/noisefloor-oss/deck/internal/workspacekeys"
	"github.com/noisefloor-oss/deck/internal/workspaceplain"
)

type plainNative interface {
	Observe(context.Context) (workspaceplain.State, error)
	Start(context.Context, string) (workspaceplain.Operation, error)
	Operation(context.Context, string) (workspaceplain.Operation, error)
}

type plainService struct {
	keys   *workspacekeys.Registry
	native plainNative
	access *plainAccess
	mu     chan struct{}
	epoch  string
}

func (s *plainService) acquire(ctx context.Context) error {
	select {
	case s.mu <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (s *plainService) release() { <-s.mu }

func plainState(state workspaceplain.State) workspacegateway.RuntimeState {
	view := workspacegateway.RuntimeState{Storage: "unknown", Runtime: state.Runtime}
	if view.Runtime == "" {
		view.Runtime = "unknown"
	}
	if state.Configured {
		view.Storage = "unencrypted"
	}
	return view
}

func (s *plainService) observe(ctx context.Context) (workspaceplain.State, error) {
	state, err := s.native.Observe(ctx)
	ready := err == nil && state.Configured && state.Runtime == "running" && state.Epoch != ""
	s.access.reconcile(ready, s.epoch != "" && state.Epoch != s.epoch)
	if err == nil {
		s.epoch = state.Epoch
	}
	return state, err
}

func (s *plainService) ObserveSecurity(ctx context.Context) (workspacegateway.RuntimeState, error) {
	if err := s.acquire(ctx); err != nil {
		return plainState(workspaceplain.State{}), err
	}
	defer s.release()
	state, err := s.observe(ctx)
	return plainState(state), err
}

func (s *plainService) Prepare(ctx context.Context, login auth.Login) (workspacekeys.PlainStartPermit, error) {
	if err := s.acquire(ctx); err != nil {
		return workspacekeys.PlainStartPermit{}, err
	}
	defer s.release()
	if err := s.access.admitLogin(ctx, login); err != nil {
		return workspacekeys.PlainStartPermit{}, err
	}
	intent, err := s.keys.CurrentPlainStart(ctx)
	if err != nil {
		return workspacekeys.PlainStartPermit{}, err
	}
	if intent.Status == "pending" {
		permit, err := s.keys.AuthorizePlainStart(ctx, login, intent.ID)
		if err != nil {
			return permit, err
		}
		previous, nativeErr := s.native.Operation(ctx, intent.ID)
		if nativeErr == nil && (previous.Status == "succeeded" || previous.Status == "failed") {
			state, observeErr := s.observe(ctx)
			if observeErr != nil {
				return workspacekeys.PlainStartPermit{}, observeErr
			}
			if state.Configured && state.Runtime == "stopped" {
				// A terminal native start may precede a lost setup-completion
				// write and a reboot. Retire that receipt without replaying it.
				if err := s.keys.FailPlainStart(ctx, permit); err != nil {
					return workspacekeys.PlainStartPermit{}, err
				}
				return s.keys.ReservePlainStart(ctx, login, intent.ID)
			}
		}
		return permit, nil
	}
	state, err := s.observe(ctx)
	if err != nil || state.Runtime != "stopped" {
		return workspacekeys.PlainStartPermit{}, ErrUnavailable
	}
	previous, err := s.native.Operation(ctx, intent.ID)
	if err != nil || (previous.Status != "succeeded" && previous.Status != "failed") {
		return workspacekeys.PlainStartPermit{}, ErrUnavailable
	}
	return s.keys.ReservePlainStart(ctx, login, intent.ID)
}

func (s *plainService) Execute(ctx context.Context, login auth.Login, permit workspacekeys.PlainStartPermit) error {
	if err := s.acquire(ctx); err != nil {
		return err
	}
	defer s.release()
	if s.access.admitLogin(ctx, login) != nil || s.keys.ValidatePlainStart(ctx, login, permit) != nil {
		return workspacekeys.ErrDenied
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	op, err := s.native.Start(ctx, permit.ID())
	if err != nil {
		return ErrUnavailable
	}
	for op.Status == "running" {
		select {
		case <-ctx.Done():
			return ErrUnavailable
		case <-time.After(100 * time.Millisecond):
		}
		op, err = s.native.Operation(ctx, permit.ID())
		if err != nil {
			return ErrUnavailable
		}
	}
	return s.complete(ctx, login, permit, op)
}

func (s *plainService) complete(ctx context.Context, login auth.Login, permit workspacekeys.PlainStartPermit, op workspaceplain.Operation) error {
	if op.ID != permit.ID() || (op.Status != "succeeded" && op.Status != "failed") || !op.State.Configured || op.State.Runtime != "running" || op.State.Epoch == "" {
		return ErrUnavailable
	}
	state, err := s.observe(ctx)
	if err != nil || !state.Configured || state.Runtime != "running" || state.Epoch != op.State.Epoch {
		return ErrUnavailable
	}
	if s.access.admitLogin(ctx, login) != nil || s.keys.ValidatePlainStart(ctx, login, permit) != nil {
		return workspacekeys.ErrDenied
	}
	return s.keys.CompletePlainStart(ctx, permit)
}

func (s *plainService) Status(ctx context.Context, login auth.Login, id string) (workspacegateway.OperationState, error) {
	if err := s.acquire(ctx); err != nil {
		return workspacegateway.OperationState{}, err
	}
	defer s.release()
	if s.access.admitLogin(ctx, login) != nil {
		return workspacegateway.OperationState{}, workspacekeys.ErrDenied
	}
	intent, err := s.keys.PlainOperation(ctx, login, id)
	if err != nil {
		return workspacegateway.OperationState{}, err
	}
	result := workspacegateway.OperationState{ID: id, Status: intent.Status, RuntimeState: plainState(workspaceplain.State{})}
	op, err := s.native.Operation(ctx, id)
	if errors.Is(err, workspaceplain.ErrNotFound) && intent.Status == "pending" {
		state, observeErr := s.observe(ctx)
		result.RuntimeState = plainState(state)
		return result, observeErr
	}
	if err != nil {
		return result, err
	}
	result.Status, result.RuntimeState = op.Status, plainState(op.State)
	if intent.Status == "pending" && (op.Status == "succeeded" || (op.Status == "failed" && op.State.Runtime == "running")) {
		permit, err := s.keys.AuthorizePlainStart(ctx, login, id)
		if err != nil {
			return result, err
		}
		if err := s.complete(ctx, login, permit, op); err != nil {
			return result, err
		}
	}
	return result, nil
}
