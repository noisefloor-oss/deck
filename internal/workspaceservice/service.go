// Package workspaceservice joins fresh method proofs to one installation's
// native runtime. Durable operation lookup never supplies execution authority.
package workspaceservice

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"time"

	"github.com/noisefloor-oss/deck/internal/auth"
	"github.com/noisefloor-oss/deck/internal/workspacegateway"
	"github.com/noisefloor-oss/deck/internal/workspacegrants"
	"github.com/noisefloor-oss/deck/internal/workspacekeys"
	"github.com/noisefloor-oss/deck/internal/workspaceruntime"
)

var (
	ErrConfiguration = errors.New("workspace service: incomplete installation")
	ErrDenied        = errors.New("workspace service: operation not authorized")
	ErrUnconfirmed   = errors.New("workspace service: native result unconfirmed")
)

// Native is the fixed, installation-owned controller. No method accepts a
// browser-selected path, command, container or Compose project.
type Native interface {
	Observe(context.Context) (workspaceruntime.State, error)
	Initialize(context.Context, string, []byte) (workspaceruntime.Operation, error)
	Initialization(context.Context, string) (workspaceruntime.Operation, error)
	ReopenInitialization(context.Context, string, []byte) (workspaceruntime.Operation, error)
	Unlock(context.Context, string, []byte) (workspaceruntime.Operation, error)
	Start(context.Context, string) (workspaceruntime.Operation, error)
	Lock(context.Context, string, func(context.Context) error) (workspaceruntime.Operation, error)
	Operation(context.Context, string) (workspaceruntime.Operation, error)
}

type Service struct {
	native      Native
	keys        *workspacekeys.Registry
	grants      *workspacegrants.Store
	prepareLock func(context.Context) error
	mu          chan struct{}
	unknown     string
}

var _ workspacegateway.RuntimeObserver = (*Service)(nil)
var _ workspacegateway.Operations = (*Service)(nil)

func New(native Native, keys *workspacekeys.Registry, grants *workspacegrants.Store, prepareLock func(context.Context) error) (*Service, error) {
	if native == nil || keys == nil || grants == nil || prepareLock == nil {
		return nil, ErrConfiguration
	}
	var nonce [32]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, ErrConfiguration
	}
	return &Service{native: native, keys: keys, grants: grants, prepareLock: prepareLock, mu: make(chan struct{}, 1),
		unknown: "unknown_" + base64.RawURLEncoding.EncodeToString(nonce[:])}, nil
}

func (s *Service) acquire(ctx context.Context) error {
	select {
	case s.mu <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (s *Service) release() { <-s.mu }

// ObserveSecurity serializes epoch publication with native completion. An
// intermediate mount epoch must not invalidate the proof completing that mount.
func (s *Service) ObserveSecurity(ctx context.Context) (workspacegateway.RuntimeState, error) {
	if err := s.acquire(ctx); err != nil {
		return publicState(workspaceruntime.State{}), ErrUnconfirmed
	}
	defer s.release()
	state, err := s.observe(ctx)
	return publicState(state), err
}

func (s *Service) observe(ctx context.Context) (workspaceruntime.State, error) {
	pending, pendingErr := s.keys.PendingInitialization(ctx)
	if pendingErr == nil {
		// Reconcile a restart's interrupted journals without replaying effects or
		// manufacturing a browser grant from the saved intent.
		_, _ = s.native.Operation(ctx, pending.ID)
		if mount, err := s.native.Initialization(ctx, pending.ID); err == nil {
			_, _ = s.native.Operation(ctx, startID(mount.ID))
		}
	} else if !errors.Is(pendingErr, workspacekeys.ErrNotFound) {
		if !abandoned(ctx, pendingErr) {
			s.grants.RevokeRegistryGrants(workspacekeys.GrantRevocation{StorageFailed: true})
		}
		return workspaceruntime.State{}, ErrUnconfirmed
	}
	state, err := s.native.Observe(ctx)
	if pendingErr != nil {
		if ctx.Err() != nil {
			return workspaceruntime.State{}, ErrUnconfirmed
		}
		epoch := state.Epoch
		if err != nil || epoch == "" {
			epoch = s.unknown
		}
		// The caller was live above; only the store itself may fail publication.
		if s.keys.SetRuntimeEpoch(context.WithoutCancel(ctx), epoch) != nil {
			return workspaceruntime.State{}, ErrUnconfirmed
		}
	}
	if err != nil {
		state.Locked = false
		return state, ErrUnconfirmed
	}
	return state, nil
}

// A caller that disconnected or ran out of time cannot establish a control-store
// fault.
func abandoned(ctx context.Context, err error) bool {
	cause := ctx.Err()
	return cause != nil && errors.Is(err, cause)
}

// Execute consumes the transient secret buffer. Success returns cookie delivery
// only after both the native observation and the grant transaction succeed.
func (s *Service) Execute(ctx context.Context, login auth.Login, permit workspacekeys.AuthorizedOperation, secret []byte) (*workspacegateway.BrowserAuthorization, error) {
	defer clear(secret)
	if err := s.acquire(ctx); err != nil {
		return nil, ErrUnconfirmed
	}
	defer s.release()
	if s.grants.ValidateExecution(ctx, login, permit) != nil {
		return nil, ErrDenied
	}
	needsSecret := permit.Action.Operation == workspacekeys.Initialize || permit.Action.Operation == workspacekeys.Unlock
	if needsSecret {
		if !s.matchesSecret(ctx, login, secret) {
			return nil, ErrDenied
		}
	} else if len(secret) != 0 {
		return nil, ErrDenied
	}
	switch permit.Action.Operation {
	case workspacekeys.Initialize, workspacekeys.Unlock:
		return s.open(ctx, login, permit, secret)
	case workspacekeys.AuthorizeBrowser:
		state, err := s.native.Observe(ctx)
		if err != nil || !running(state) || state.Epoch != permit.RuntimeEpoch {
			return nil, ErrUnconfirmed
		}
		return s.issue(ctx, login, permit)
	case workspacekeys.Lock:
		ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()
		op, err := s.native.Lock(ctx, permit.ID, s.prepareLock)
		if err != nil || op.Status != "succeeded" || !op.State.Locked {
			return nil, ErrUnconfirmed
		}
		if state, err := s.observe(ctx); err != nil || !state.Locked {
			return nil, ErrUnconfirmed
		}
		return nil, nil
	default:
		// Registry management already committed during proof consumption.
		return nil, nil
	}
}

func (s *Service) matchesSecret(ctx context.Context, login auth.Login, secret []byte) bool {
	if len(secret) != 32 {
		return false
	}
	state, err := s.keys.State(ctx, login)
	if err != nil {
		return false
	}
	commitment, err := base64.RawURLEncoding.Strict().DecodeString(state.Commitment)
	if err != nil || len(commitment) != sha256.Size {
		return false
	}
	h := sha256.New()
	_, _ = h.Write([]byte("deck.volume.v1\x00"))
	_, _ = h.Write(secret)
	return subtle.ConstantTimeCompare(commitment, h.Sum(nil)) == 1
}

func (s *Service) open(ctx context.Context, login auth.Login, permit workspacekeys.AuthorizedOperation, secret []byte) (*workspacegateway.BrowserAuthorization, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	var state workspaceruntime.State
	mountID := permit.ID
	if permit.Action.Operation == workspacekeys.Initialize {
		op, err := s.native.Initialize(ctx, permit.ID, secret)
		if err != nil || op.Status != "succeeded" || !op.State.Configured {
			return nil, ErrUnconfirmed
		}
		if op.State.Storage == "closed" {
			if s.keys.RequireInitializationResume(ctx, login, permit) != nil || s.grants.ValidateExecution(ctx, login, permit) != nil {
				return nil, ErrDenied
			}
			op, err = s.native.ReopenInitialization(ctx, permit.ID, secret)
		} else {
			op, err = s.native.Initialization(ctx, permit.ID)
		}
		if err != nil || op.Status != "succeeded" || op.State.Storage != "open" {
			return nil, ErrUnconfirmed
		}
		mountID = op.ID
		state = op.State
	} else {
		var err error
		state, err = s.native.Observe(ctx)
		if err != nil {
			return nil, ErrUnconfirmed
		}
		if state.Storage != "open" {
			op, err := s.native.Unlock(ctx, permit.ID, secret)
			if err != nil || op.Status != "succeeded" {
				return nil, ErrUnconfirmed
			}
			state = op.State
		}
	}
	if !state.Configured || state.Storage != "open" {
		return nil, ErrUnconfirmed
	}
	if s.grants.ValidateExecution(ctx, login, permit) != nil {
		return nil, ErrDenied
	}
	if !running(state) {
		op, err := s.native.Start(ctx, startID(mountID))
		if err != nil {
			return nil, ErrUnconfirmed
		}
		for op.Status == "running" {
			select {
			case <-ctx.Done():
				return nil, ErrUnconfirmed
			case <-time.After(100 * time.Millisecond):
			}
			op, err = s.native.Operation(ctx, startID(mountID))
			if err != nil {
				return nil, ErrUnconfirmed
			}
		}
		if op.Status != "succeeded" {
			return nil, ErrUnconfirmed
		}
	}
	// Observe again immediately before the transactional epoch handoff; an old
	// operation's succeeded bit alone cannot certify the current runtime.
	state, err := s.native.Observe(ctx)
	if err != nil || !running(state) {
		return nil, ErrUnconfirmed
	}
	completed, err := s.grants.CompleteNative(ctx, login, permit, state.Epoch)
	if err != nil {
		return nil, ErrDenied
	}
	return s.issue(ctx, login, completed)
}

func (s *Service) issue(ctx context.Context, login auth.Login, permit workspacekeys.AuthorizedOperation) (*workspacegateway.BrowserAuthorization, error) {
	delivery, err := s.grants.Issue(ctx, login, permit)
	if err != nil {
		return nil, ErrDenied
	}
	return workspacegateway.NewBrowserAuthorization(delivery.Token(), delivery.ExpiresAt)
}

func running(state workspaceruntime.State) bool {
	return state.Configured && state.Storage == "open" && state.Runtime == "running" && state.Epoch != "" && !state.Locked
}

func startID(id string) string {
	digest := sha256.Sum256([]byte("deck.runtime.start.v1\x00" + id))
	return "start_" + base64.RawURLEncoding.EncodeToString(digest[:])
}

func publicState(state workspaceruntime.State) workspacegateway.RuntimeState {
	storage, runtime := state.Storage, state.Runtime
	if storage == "" {
		storage = "unknown"
	}
	if runtime == "" {
		runtime = "unknown"
	}
	return workspacegateway.RuntimeState{Storage: storage, Runtime: runtime, Migration: state.Migration,
		Locked: state.Locked && storage == "closed" && runtime == "stopped"}
}

func (s *Service) Status(ctx context.Context, login auth.Login, id string) (workspacegateway.OperationState, error) {
	if err := s.acquire(ctx); err != nil {
		return workspacegateway.OperationState{}, ErrUnconfirmed
	}
	defer s.release()
	record, err := s.keys.Operation(ctx, login, id)
	if err != nil {
		return workspacegateway.OperationState{}, ErrDenied
	}
	status := "complete"
	var nativeOp workspaceruntime.Operation
	var nativeErr error
	switch record.Action.Operation {
	case workspacekeys.Initialize:
		_, _ = s.native.Operation(ctx, id)
		mount, err := s.native.Initialization(ctx, id)
		if err == nil {
			nativeOp, nativeErr = s.native.Operation(ctx, startID(mount.ID))
		}
		status = "unconfirmed"
	case workspacekeys.Unlock:
		_, _ = s.native.Operation(ctx, id)
		nativeOp, nativeErr = s.native.Operation(ctx, startID(id))
		status = "unconfirmed"
	case workspacekeys.Lock:
		nativeOp, nativeErr = s.native.Operation(ctx, id)
		status = "unconfirmed"
	case workspacekeys.AuthorizeBrowser:
		status = "unconfirmed"
	}
	state, observeErr := s.observe(ctx)
	if observeErr == nil {
		if nativeErr == nil && nativeOp.Status == "running" {
			status = "running"
		}
		if record.Action.Operation == workspacekeys.Lock && nativeErr == nil && nativeOp.Status == "succeeded" && state.Locked {
			status = "complete"
		}
		if record.Action.Operation == workspacekeys.Lock && nativeErr == nil && nativeOp.Status == "failed" && nativeOp.Code == "lock-preparation-refused" && running(state) {
			status = "refused"
		}
		if (record.Action.Operation == workspacekeys.Initialize || record.Action.Operation == workspacekeys.Unlock) && record.Status == "complete" && running(state) && state.Epoch == record.RuntimeEpoch {
			status = "complete"
		}
		if record.Action.Operation == workspacekeys.AuthorizeBrowser && running(state) && state.Epoch == record.RuntimeEpoch && s.grants.Require(ctx, login) == nil {
			status = "complete"
		}
	}
	return workspacegateway.OperationState{ID: id, Status: status, RuntimeState: publicState(state)}, nil
}
