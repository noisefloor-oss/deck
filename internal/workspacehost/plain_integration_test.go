//go:build linux

package workspacehost

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"testing/fstest"

	"github.com/noisefloor-oss/deck/internal/workspacegateway"
	"github.com/noisefloor-oss/deck/internal/workspacegrants"
	"github.com/noisefloor-oss/deck/internal/workspacekeys"
	"github.com/noisefloor-oss/deck/internal/workspaceplain"
	"github.com/noisefloor-oss/deck/internal/workspaceruntime"
)

type assemblyPlain struct {
	mu            sync.Mutex
	state         workspaceplain.State
	ops           map[string]workspaceplain.Operation
	calls         int
	uncertain     bool
	observedEpoch string
}

func (p *assemblyPlain) Observe(context.Context) (workspaceplain.State, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	state := p.state
	if p.observedEpoch != "" {
		state.Epoch = p.observedEpoch
	}
	return state, nil
}
func (p *assemblyPlain) Start(_ context.Context, id string) (workspaceplain.Operation, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if op, ok := p.ops[id]; ok {
		op.State = p.state
		return op, nil
	}
	p.calls++
	p.state = workspaceplain.State{Configured: true, Runtime: "running", Epoch: "plain-" + id}
	op := workspaceplain.Operation{ID: id, Kind: "start", Status: "succeeded", State: p.state}
	if p.uncertain {
		op.Status = "uncertain"
		p.state.Runtime = "unknown"
		op.State = p.state
	}
	p.ops[id] = op
	return op, nil
}
func (p *assemblyPlain) Operation(_ context.Context, id string) (workspaceplain.Operation, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	op, ok := p.ops[id]
	if !ok {
		return op, workspaceplain.ErrNotFound
	}
	op.State = p.state
	if p.state.Runtime == "running" && (op.Status == "running" || op.Status == "uncertain") {
		op.Status = "succeeded"
	}
	return op, nil
}

func plainHostConfig(t *testing.T, policy workspacekeys.EncryptionPolicy) Config {
	t.Helper()
	cfg := hostConfig(t)
	root := filepath.Dir(cfg.Native.ControlDir)
	for _, name := range []string{"plain", "host-home", "bin"} {
		requireOK(t, os.Mkdir(filepath.Join(root, name), 0700))
	}
	for _, name := range []string{"podman", "plain-compose"} {
		requireOK(t, os.WriteFile(filepath.Join(root, "bin", name), []byte("#!/bin/sh\nexit 1\n"), 0700))
	}
	env := filepath.Join(cfg.Native.ControlDir, "plain.env")
	requireOK(t, os.WriteFile(env, nil, 0600))
	cfg.EncryptionPolicy = policy
	cfg.Plain = &workspaceplain.Config{InstanceID: cfg.Native.InstanceID, OwnerUID: cfg.Native.OwnerUID, StateRoot: filepath.Join(root, "plain"), ControlDir: cfg.Native.ControlDir, HomeDir: filepath.Join(root, "host-home"), RuntimeDir: cfg.Native.RuntimeDir, PodmanPath: filepath.Join(root, "bin", "podman"), ComposePath: filepath.Join(root, "bin", "plain-compose"), ComposeEnvFile: env, ComposeProject: "plain_fixture", AppContainer: "plain_fixture-app-1", PublicOrigin: cfg.Origin}
	if policy == workspacekeys.EncryptionUnavailable {
		cfg.Native.MountPoint = ""
		cfg.Native.CipherDir = ""
		cfg.Native.FusermountPath = ""
	}
	return cfg
}

func plainAssembly(t *testing.T, policy workspacekeys.EncryptionPolicy) (*assemblyFixture, *assemblyPlain) {
	t.Helper()
	p := &assemblyPlain{state: workspaceplain.State{Runtime: "stopped", Epoch: "before"}, ops: make(map[string]workspaceplain.Operation)}
	return newAssemblyWithPlain(t, plainHostConfig(t, policy), p), p
}

func (f *assemblyFixture) setupLink(b *assemblyBrowser) {
	f.t.Helper()
	keys, err := workspacekeys.OpenExisting(context.Background(), f.h.db, workspacekeys.Config{InstanceID: f.h.cfg.Native.InstanceID, Origin: f.h.cfg.Origin, RPID: f.h.cfg.Gateway().RPID, EncryptionPolicy: f.h.cfg.policy()})
	requireOK(f.t, err)
	invites, err := workspacegateway.NewInvitations(context.Background(), f.h.db, keys, f.h.cfg.AllowedEmail, nil)
	requireOK(f.t, err)
	token, _, err := invites.Issue(context.Background())
	requireOK(f.t, err)
	b.call("POST", "/api/security/v1/invitation/redeem", map[string]string{"token": token}, 200, nil)
}

func TestPlainAssemblyRequiresExplicitChoiceBeforePrivateData(t *testing.T) {
	for _, policy := range []workspacekeys.EncryptionPolicy{workspacekeys.EncryptionOptional, workspacekeys.EncryptionUnavailable} {
		t.Run(string(policy), func(t *testing.T) {
			f, p := plainAssembly(t, policy)
			b := f.browser("a")
			b.call("GET", "/api/sessions", nil, http.StatusForbidden, nil)
			b.call("POST", "/api/security/v1/onboarding/protection", map[string]string{"protection": "none"}, http.StatusForbidden, nil)
			f.setupLink(b)
			b.call("POST", "/api/security/v1/onboarding/protection", map[string]string{"protection": "none"}, 200, nil)
			b.call("GET", "/api/sessions", nil, http.StatusForbidden, nil)
			b.call("POST", "/api/security/v1/onboarding/start", map[string]string{}, 200, nil)
			var state struct {
				Onboarding                          workspacekeys.OnboardingState
				Storage                             string
				WorkspaceAuthorized, BrowserGranted bool
			}
			b.call("GET", "/api/security/v1/state", nil, 200, &state)
			if !state.Onboarding.SetupComplete || state.Storage != "unencrypted" || !state.WorkspaceAuthorized || state.BrowserGranted || b.cookies[workspacegrants.CookieName] != nil {
				t.Fatal("plain setup acquired encryption state or lost authorization")
			}
			b.call("GET", "/api/sessions", nil, 200, nil)
			if f.native.callCount() != 0 || p.calls != 1 {
				t.Fatal("plain setup dispatched an encrypted operation or repeated start")
			}
			b.call("POST", "/api/security/v1/methods/prepare", map[string]string{"kind": "password", "label": "not allowed"}, 403, nil)
			wrongOwner := f.browser("c")
			wrongOwner.login.Subject = "different-google-owner"
			wrongOwner.call("GET", "/api/sessions", nil, 403, nil)
		})
	}
}

func TestPlainAssemblyUncertainStartResumesByObservation(t *testing.T) {
	f, p := plainAssembly(t, workspacekeys.EncryptionOptional)
	p.uncertain = true
	b := f.browser("a")
	f.setupLink(b)
	b.call("POST", "/api/security/v1/onboarding/protection", map[string]string{"protection": "none"}, 200, nil)
	var receipt struct{ ID, Status string }
	b.call("POST", "/api/security/v1/onboarding/start", map[string]string{}, 202, &receipt)
	b.call("GET", "/api/sessions", nil, 403, nil)
	var state struct {
		Onboarding       workspacekeys.OnboardingState
		PlainOperationID string `json:"plainOperationId"`
	}
	b.call("GET", "/api/security/v1/state", nil, 200, &state)
	if state.Onboarding.SetupComplete || state.PlainOperationID != receipt.ID {
		t.Fatal("lost start completion or operation identity")
	}
	other := f.browser("b")
	other.call("GET", "/api/security/v1/onboarding/operations/"+receipt.ID, nil, 200, nil)
	p.mu.Lock()
	p.state.Runtime = "running"
	p.mu.Unlock()
	other.call("GET", "/api/security/v1/onboarding/operations/"+receipt.ID, nil, 200, nil)
	other.call("GET", "/api/sessions", nil, 200, nil)
	if p.calls != 1 {
		t.Fatal("observing a native receipt repeated the start")
	}
}

func TestPlainAssemblyFailedStartCompletesAfterRuntimeRecovery(t *testing.T) {
	for _, recoverBy := range []string{"status", "start"} {
		t.Run(recoverBy, func(t *testing.T) {
			f, p := plainAssembly(t, workspacekeys.EncryptionOptional)
			p.uncertain = true
			b := f.browser("a")
			f.setupLink(b)
			b.call("POST", "/api/security/v1/onboarding/protection", map[string]string{"protection": "none"}, 200, nil)
			var receipt struct{ ID string }
			b.call("POST", "/api/security/v1/onboarding/start", map[string]string{}, 202, &receipt)
			p.mu.Lock()
			op := p.ops[receipt.ID]
			op.Status = "failed"
			p.ops[receipt.ID] = op
			p.state.Runtime, p.state.Epoch = "stopped", "stopped-epoch"
			p.mu.Unlock()
			b.call("GET", "/api/security/v1/onboarding/operations/"+receipt.ID, nil, 200, nil)
			b.call("GET", "/api/sessions", nil, 403, nil)
			p.mu.Lock()
			p.state.Runtime, p.state.Epoch = "running", "recovered-epoch"
			p.mu.Unlock()
			wrongOwner := f.browser("wrong")
			wrongOwner.login.Subject = "another-owner"
			wrongOwner.call("GET", "/api/security/v1/onboarding/operations/"+receipt.ID, nil, 401, nil)
			p.mu.Lock()
			p.observedEpoch = "changed-during-completion"
			p.mu.Unlock()
			if recoverBy == "status" {
				b.call("GET", "/api/security/v1/onboarding/operations/"+receipt.ID, nil, 503, nil)
			} else {
				b.call("POST", "/api/security/v1/onboarding/start", map[string]string{}, 202, nil)
			}
			b.call("GET", "/api/sessions", nil, 403, nil)
			p.mu.Lock()
			p.observedEpoch = ""
			p.mu.Unlock()
			if recoverBy == "status" {
				b.call("GET", "/api/security/v1/onboarding/operations/"+receipt.ID, nil, 200, nil)
			} else {
				b.call("POST", "/api/security/v1/onboarding/start", map[string]string{}, 200, nil)
			}
			b.call("GET", "/api/sessions", nil, 200, nil)
			p.mu.Lock()
			defer p.mu.Unlock()
			if p.calls != 1 || p.ops[receipt.ID].Status != "failed" {
				t.Fatal("recovery replayed start or changed terminal history")
			}
		})
	}
}

func TestPlainAssemblyStoppedStartReservesFreshOnlyAfterTerminalReceipt(t *testing.T) {
	for _, status := range []string{"failed", "uncertain"} {
		t.Run(status, func(t *testing.T) {
			f, p := plainAssembly(t, workspacekeys.EncryptionOptional)
			p.uncertain = true
			b := f.browser("a")
			f.setupLink(b)
			b.call("POST", "/api/security/v1/onboarding/protection", map[string]string{"protection": "none"}, 200, nil)
			var previous struct{ ID string }
			b.call("POST", "/api/security/v1/onboarding/start", map[string]string{}, 202, &previous)
			b.call("GET", "/api/sessions", nil, 403, nil)
			p.mu.Lock()
			p.state.Runtime, p.state.Epoch = "stopped", "after-reboot"
			op := p.ops[previous.ID]
			op.Status = status
			p.ops[previous.ID] = op
			p.uncertain = false
			p.mu.Unlock()
			var next struct{ ID string }
			if status == "uncertain" {
				b.call("POST", "/api/security/v1/onboarding/start", map[string]string{}, 202, &next)
				if next.ID != previous.ID || p.calls != 1 {
					t.Fatal("an unresolved start was retired instead of resumed")
				}
				return
			}
			b.call("POST", "/api/security/v1/onboarding/start", map[string]string{}, 200, &next)
			if next.ID == previous.ID || p.calls != 2 {
				t.Fatal("stopped runtime stranded setup or replayed the previous start")
			}
			b.call("GET", "/api/sessions", nil, 200, nil)
		})
	}
}

func TestPlainAssemblyRetiredStartWithoutReplacementAllowsFreshStart(t *testing.T) {
	f, p := plainAssembly(t, workspacekeys.EncryptionOptional)
	p.uncertain = true
	b := f.browser("a")
	f.setupLink(b)
	b.call("POST", "/api/security/v1/onboarding/protection", map[string]string{"protection": "none"}, 200, nil)
	var previous struct{ ID string }
	b.call("POST", "/api/security/v1/onboarding/start", map[string]string{}, 202, &previous)
	p.mu.Lock()
	p.state.Runtime = "stopped"
	op := p.ops[previous.ID]
	op.Status = "failed"
	p.ops[previous.ID] = op
	p.uncertain = false
	p.mu.Unlock()
	ctx := context.Background()
	keys, err := workspacekeys.OpenExisting(ctx, f.h.db, workspacekeys.Config{InstanceID: f.h.cfg.Native.InstanceID, Origin: f.h.cfg.Origin, RPID: f.h.cfg.Gateway().RPID, EncryptionPolicy: f.h.cfg.policy()})
	requireOK(t, err)
	permit, err := keys.AuthorizePlainStart(ctx, b.login, previous.ID)
	requireOK(t, err)
	requireOK(t, keys.FailPlainStart(ctx, permit))
	var next struct{ ID string }
	b.call("POST", "/api/security/v1/onboarding/start", map[string]string{}, 200, &next)
	if next.ID == previous.ID || p.calls != 2 {
		t.Fatal("a retired start without a replacement stranded setup")
	}
	b.call("GET", "/api/sessions", nil, 200, nil)
}

func TestPlainAssemblyLogoutPreservesOtherBrowserAndRuntime(t *testing.T) {
	f, p := plainAssembly(t, workspacekeys.EncryptionOptional)
	a, b := f.browser("a"), f.browser("b")
	f.setupLink(a)
	a.call("POST", "/api/security/v1/onboarding/protection", map[string]string{"protection": "none"}, 200, nil)
	a.call("POST", "/api/security/v1/onboarding/start", map[string]string{}, 200, nil)
	wa, wb := a.websocket(), b.websocket()
	defer wb.Close()
	ea, eb := a.events(), b.events()
	defer ea.Body.Close()
	defer eb.Body.Close()
	a.call("POST", "/auth/logout", nil, 204, nil)
	requireClosedSocket(t, wa)
	requireClosedEvents(t, ea)
	a.call("GET", "/api/sessions", nil, 403, nil)
	b.call("GET", "/api/sessions", nil, 200, nil)
	if p.calls != 1 || f.native.callCount() != 0 {
		t.Fatal("sign out restarted or stopped the workspace")
	}
}

func TestPlainAssemblyControlReadFailureClosesLiveTransports(t *testing.T) {
	f, _ := plainAssembly(t, workspacekeys.EncryptionOptional)
	b := f.browser("a")
	f.setupLink(b)
	b.call("POST", "/api/security/v1/onboarding/protection", map[string]string{"protection": "none"}, 200, nil)
	b.call("POST", "/api/security/v1/onboarding/start", map[string]string{}, 200, nil)
	ws, events := b.websocket(), b.events()
	defer events.Body.Close()
	_, err := f.h.db.Exec(`DROP TABLE workspacekeys_onboarding`)
	requireOK(t, err)
	if _, err := f.h.mode.ObserveSecurity(context.Background()); err == nil {
		t.Fatal("missing control state was accepted")
	}
	requireClosedSocket(t, ws)
	requireClosedEvents(t, events)
}

func TestPlainAssemblyLostSetupCompletionCanStartAfterReboot(t *testing.T) {
	f, native := plainAssembly(t, workspacekeys.EncryptionOptional)
	b := f.browser("a")
	f.setupLink(b)
	b.call("POST", "/api/security/v1/onboarding/protection", map[string]string{"protection": "none"}, 200, nil)
	intent, err := f.h.mode.keys.CurrentPlainStart(context.Background())
	requireOK(t, err)
	// The native start succeeded before the gateway could record completion.
	// A host reboot then stopped that runtime while its native receipt survived.
	_, err = native.Start(context.Background(), intent.ID)
	requireOK(t, err)
	native.mu.Lock()
	native.state.Runtime, native.state.Epoch = "stopped", "after-reboot"
	native.mu.Unlock()
	var next struct{ ID string }
	b.call("POST", "/api/security/v1/onboarding/start", map[string]string{}, 200, &next)
	if next.ID == intent.ID || native.calls != 2 {
		t.Fatal("reboot stranded setup or replayed the previous start")
	}
	b.call("GET", "/api/sessions", nil, 200, nil)
}

func TestMissingSetupRecordDoesNotAdoptAnExistingRuntime(t *testing.T) {
	for _, encrypted := range []bool{true, false} {
		t.Run(map[bool]string{true: "encrypted", false: "plain"}[encrypted], func(t *testing.T) {
			cfg := plainHostConfig(t, workspacekeys.EncryptionOptional)
			h := &Host{cfg: cfg}
			t.Cleanup(h.Close)
			requireOK(t, h.acquire())
			native := &assemblyNative{state: workspaceruntime.State{Configured: encrypted, Storage: "closed", Runtime: "stopped", Epoch: "existing"}}
			plain := &assemblyPlain{state: workspaceplain.State{Configured: !encrypted, Runtime: "stopped", Epoch: "existing"}}
			if err := h.mountWithPlain(context.Background(), fstest.MapFS{"index.html": {Data: []byte("setup")}}, "test", native, plain); !errors.Is(err, ErrConfiguration) {
				t.Fatal("existing storage was presented as fresh setup", err)
			}
			if native.callCount() != 0 || plain.calls != 0 {
				t.Fatal("refusing existing data started a runtime")
			}
		})
	}
}
