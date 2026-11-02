package workspacehost

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"path/filepath"
	"sync"
	"time"

	"github.com/noisefloor-oss/deck/internal/auth"
	"github.com/noisefloor-oss/deck/internal/lock"
	"github.com/noisefloor-oss/deck/internal/workspacegateway"
	"github.com/noisefloor-oss/deck/internal/workspacegrants"
	"github.com/noisefloor-oss/deck/internal/workspacekeys"
	"github.com/noisefloor-oss/deck/internal/workspaceplain"
	"github.com/noisefloor-oss/deck/internal/workspaceproxy"
	"github.com/noisefloor-oss/deck/internal/workspaceruntime"
	"github.com/noisefloor-oss/deck/internal/workspaceservice"
)

var ErrUnavailable = errors.New("workspace gateway unavailable")

type Host struct {
	cfg         Config
	lifetime    *lock.ExclusiveLock
	native      *workspaceruntime.Controller
	plainNative *workspaceplain.Controller
	db          *sql.DB
	grants      *workspacegrants.Store
	service     *workspaceservice.Service
	mode        *gatewayMode
	proxy       *workspaceproxy.Handler
	control     *http.Transport
	server      *http.Server
	closeOnce   sync.Once
}

func Open(ctx context.Context, cfg Config, assets fs.FS, buildID string) (*Host, error) {
	if cfg.Validate() != nil || assets == nil {
		return nil, ErrConfiguration
	}
	if _, err := fs.Stat(assets, "index.html"); err != nil {
		return nil, errors.New("workspace gateway requires built frontend assets")
	}
	h := &Host{cfg: cfg}
	ok := false
	defer func() {
		if !ok {
			h.Close()
		}
	}()
	if err := h.acquire(); err != nil {
		return nil, err
	}
	var err error
	if cfg.policy() != workspacekeys.EncryptionUnavailable {
		if h.native, err = workspaceruntime.Open(cfg.Native); err != nil {
			return nil, ErrConfiguration
		}
	}
	if cfg.Plain != nil {
		if h.plainNative, err = workspaceplain.Open(*cfg.Plain); err != nil {
			return nil, ErrConfiguration
		}
	}
	var encrypted workspaceservice.Native
	if h.native != nil {
		encrypted = h.native
	}
	var plain plainNative
	if h.plainNative != nil {
		plain = h.plainNative
	}
	if err := h.mountWithPlain(ctx, assets, buildID, encrypted, plain); err != nil {
		return nil, err
	}
	ok = true
	return h, nil
}

func (h *Host) acquire() error {
	// A second gateway must not reset live grants or mark another controller's
	// in-flight native operations interrupted before discovering a busy port.
	held, err := lock.Acquire(filepath.Join(h.cfg.Native.ControlDir, "gateway.lock"))
	if err != nil {
		return errors.Join(ErrUnavailable, err)
	}
	h.lifetime = held
	return nil
}

func (h *Host) mount(ctx context.Context, assets fs.FS, buildID string, native workspaceservice.Native) error {
	return h.mountWithPlain(ctx, assets, buildID, native, nil)
}

func (h *Host) mountWithPlain(ctx context.Context, assets fs.FS, buildID string, native workspaceservice.Native, plain plainNative) error {
	cfg := h.cfg
	var err error
	if h.db, err = workspacegateway.OpenControl(cfg.control()); err != nil {
		return ErrUnavailable
	}
	if h.grants, err = workspacegrants.New(ctx, h.db, workspacegrants.Config{InstanceID: cfg.Native.InstanceID}); err != nil {
		return ErrUnavailable
	}
	gatewayConfig := cfg.Gateway()
	epochStore, err := workspacegateway.NewLogoutEpochStore(ctx, h.db)
	if err != nil {
		return ErrUnavailable
	}
	gatewayConfig.Auth.EpochStore = epochStore
	keys, err := workspacekeys.New(ctx, h.db, workspacekeys.Config{InstanceID: gatewayConfig.InstanceID,
		Origin: gatewayConfig.Origin, RPID: gatewayConfig.RPID, Preparation: h.grants, EncryptionPolicy: cfg.policy()})
	if err != nil || h.grants.BindRegistry(keys) != nil {
		return ErrUnavailable
	}
	invitations, err := workspacegateway.NewInvitations(ctx, h.db, keys, cfg.AllowedEmail, nil)
	if err != nil {
		return ErrUnavailable
	}
	setup, err := keys.Onboarding(ctx)
	if err != nil {
		return ErrUnavailable
	}
	if setup.Protection == workspacekeys.ProtectionUndecided {
		if native != nil {
			state, err := native.Observe(ctx)
			if err != nil || state.Configured || state.Storage != "closed" || state.Runtime != "stopped" {
				return ErrConfiguration
			}
		}
		if plain != nil {
			state, err := plain.Observe(ctx)
			if err != nil || state.Configured || state.Runtime != "stopped" {
				return ErrConfiguration
			}
		}
	}
	h.mode = &gatewayMode{keys: keys, grants: h.grants}
	h.control = workspaceproxy.SocketTransport(cfg.Socket(), cfg.Native.OwnerUID)
	// Stopping a cohort can take longer than an ordinary data request. The
	// preparation callback supplies the bounded deadline for this transport.
	h.control.ResponseHeaderTimeout = 0
	if native != nil {
		if h.service, err = workspaceservice.New(native, keys, h.grants, h.prepareLock); err != nil {
			return ErrUnavailable
		}
		if h.proxy, err = workspaceproxy.New(workspaceproxy.Config{Origin: cfg.Origin, Socket: cfg.Socket(),
			OwnerUID: cfg.Native.OwnerUID}, h.mode, h.mode); err != nil {
			return ErrUnavailable
		}
		h.mode.encrypted, h.mode.encryptedProxy = h.service, h.proxy
	}
	if plain != nil {
		access, err := newPlainAccess(ctx, h.db, func(ctx context.Context, login auth.Login) error {
			state, err := keys.State(ctx, login)
			if err != nil {
				return err
			}
			if !state.OwnerBound || state.Onboarding.Protection != workspacekeys.ProtectionNone || !state.Onboarding.SetupComplete {
				return workspacekeys.ErrDenied
			}
			return nil
		})
		if err != nil {
			return ErrUnavailable
		}
		h.mode.plain = &plainService{keys: keys, native: plain, access: access, mu: make(chan struct{}, 1)}
		if cfg.Plain == nil {
			return ErrConfiguration
		}
		h.mode.plainProxy, err = workspaceproxy.New(workspaceproxy.Config{Origin: cfg.Origin,
			Socket: filepath.Join(cfg.Plain.StateRoot, "data", "gateway", "backend.sock"), OwnerUID: cfg.Native.OwnerUID, Unencrypted: true}, h.mode, h.mode)
		if err != nil {
			return ErrUnavailable
		}
	}
	var plainOperations workspacegateway.PlainOperations
	if h.mode.plain != nil {
		plainOperations = h.mode.plain
	}
	handler, err := workspacegateway.New(gatewayConfig, keys, invitations, workspacegateway.Options{
		Assets: assets, BuildID: buildID, Runtime: h.mode, Operations: h.mode, Access: h.mode, Application: h.mode, PlainOperations: plainOperations})
	if err != nil {
		return ErrUnavailable
	}
	h.server = &http.Server{Addr: cfg.Bind, Handler: workspacegrants.CookieContext(privateRequestContext(handler)),
		ConnContext: workspaceproxy.ConnectionContext, ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout: 60 * time.Second, MaxHeaderBytes: 32 * 1024, ErrorLog: log.New(io.Discard, "", 0)}
	return nil
}

func (h *Host) Run(ctx context.Context) error {
	listener, err := net.Listen("tcp", h.cfg.Bind)
	if err != nil {
		return ErrUnavailable
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	observed := make(chan struct{})
	go func() {
		defer close(observed)
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-runCtx.Done():
				return
			case <-ticker.C:
				checkCtx, stop := context.WithTimeout(runCtx, 5*time.Second)
				_, _ = h.mode.ObserveSecurity(checkCtx)
				_ = h.grants.Sweep(checkCtx)
				stop()
			}
		}
	}()
	served := make(chan error, 1)
	go func() { served <- h.server.Serve(listener) }()
	select {
	case <-ctx.Done():
	case <-served:
		cancel()
		<-observed
		return ErrUnavailable
	}
	cancel()
	<-observed
	// Restart drops browser streams but never stops real CLIs or unmounts data.
	_ = h.grants.Close()
	if h.mode != nil && h.mode.plain != nil {
		h.mode.plain.access.Close()
	}
	shutdownCtx, stop := context.WithTimeout(context.Background(), 10*time.Second)
	defer stop()
	if h.server.Shutdown(shutdownCtx) != nil {
		_ = h.server.Close()
		return ErrUnavailable
	}
	if err := <-served; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return ErrUnavailable
	}
	return nil
}

func (h *Host) Close() {
	h.closeOnce.Do(func() {
		if h.grants != nil {
			_ = h.grants.Close()
		}
		if h.server != nil {
			_ = h.server.Close()
		}
		if h.proxy != nil {
			h.proxy.Close()
		}
		if h.mode != nil {
			if h.mode.plainProxy != nil {
				h.mode.plainProxy.Close()
			}
			if h.mode.plain != nil {
				h.mode.plain.access.Close()
			}
		}
		if h.control != nil {
			h.control.CloseIdleConnections()
		}
		if h.native != nil {
			_ = h.native.Close()
		}
		if h.plainNative != nil {
			_ = h.plainNative.Close()
		}
		if h.db != nil {
			_ = h.db.Close()
		}
		if h.lifetime != nil {
			_ = h.lifetime.Release()
		}
	})
}
