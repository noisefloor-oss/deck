package harnesslogin

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net"
	"sync"
	"time"
)

const lifetime = 5 * time.Minute
const maxRegistrations = 1024

// Binding comes from kernel process/socket evidence, never a browser request.
// A backend incarnation owns all bindings in memory; restart revokes them all.
type Binding struct {
	Session    string
	Generation int
	Provider   string
	PID        int
	StartTicks uint64
	Listener   uint64
	Network    string
}

type Runtime interface {
	// Capture proves that the browser opener is descended from the reconciled
	// session and identifies the native process that owns the declared listener.
	Capture(context.Context, int, int) (Binding, error)
	// Dial verifies the original listener and ownership of the accepted peer
	// socket before returning a connection on which callback bytes may be sent.
	Dial(context.Context, Binding, int) (net.Conn, error)
	Alive(context.Context, Binding) bool
}

type registration struct {
	binding  Binding
	flow     Flow
	callback callback
	expires  time.Time
	claimed  bool
	// Dismissal must not let another user claim this attempt.
	owner string
	// The exact native authorization URL, retained in memory for the Codex
	// flow alone and cleared on observed expiry, replacement, cancellation,
	// or callback submission.
	url string
}
type ticket struct {
	registration *registration
	user         string
	used         bool
}
type Pending struct {
	ID      string    `json:"id"`
	Flow    Flow      `json:"flow"`
	Expires time.Time `json:"expires"`
}
type Handle struct {
	ID      string    `json:"id"`
	Expires time.Time `json:"expires"`
}

type Service struct {
	mu            sync.Mutex
	runtime       Runtime
	now           func() time.Time
	registrations map[string]*registration
	tickets       map[string]*ticket
	subscribers   map[string]map[chan struct{}]struct{}
}

func New(runtime Runtime) *Service {
	return &Service{runtime: runtime, now: time.Now, registrations: make(map[string]*registration), tickets: make(map[string]*ticket)}
}
func randomID() string {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("harness login randomness unavailable")
	}
	return hex.EncodeToString(b[:])
}

// Register is called only on the local native-opener socket. The peer pid is
// obtained with SO_PEERCRED, not read from a request field.
func (s *Service) Register(ctx context.Context, peerPID int, provider, raw string) error {
	flow, cb, err := authorization(provider, raw)
	if err != nil {
		return err
	}
	binding, err := s.runtime.Capture(ctx, peerPID, cb.port())
	if err != nil || binding.Provider != provider {
		return ErrUnavailable
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// Retain consumed registrations for this incarnation: another opener call
	// must not turn the same native state into a fresh callback authority.
	for _, r := range s.registrations {
		if r.binding == binding && r.callback.state == cb.state {
			return ErrConsumed
		}
	}
	if len(s.registrations) >= maxRegistrations {
		return ErrUnavailable
	}
	for _, r := range s.registrations {
		if r.binding.PID == binding.PID && r.binding.StartTicks == binding.StartTicks {
			r.claimed = true
			r.expires = s.now()
			r.url = ""
		}
	}
	r := &registration{binding: binding, flow: flow, callback: cb, expires: s.now().Add(lifetime)}
	// Only the Codex helper reopens the native login; a Pi attempt never
	// retains its URL anywhere.
	if flow == CodexBrowser {
		r.url = raw
	}
	s.registrations[randomID()] = r
	s.notifyLocked(binding.Session)
	return nil
}

// Pending lists only attempts the calling user could actually begin: a
// dismissed attempt stays owned by the user who held it, and listing it
// for anyone else would offer a handoff that Begin then refuses.
func (s *Service) Pending(ctx context.Context, user, session string) []Pending {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []Pending{}
	if user == "" {
		return out
	}
	for id, r := range s.registrations {
		if !s.now().Before(r.expires) {
			r.url = ""
			continue
		}
		if r.owner != "" && r.owner != user {
			continue
		}
		if r.binding.Session == session && !r.claimed && s.now().Before(r.expires) && s.runtime.Alive(ctx, r.binding) {
			out = append(out, Pending{id, r.flow, r.expires})
		}
	}
	return out
}
func (s *Service) Begin(ctx context.Context, user, session, id string) (Handle, error) {
	if user == "" {
		return Handle{}, ErrUnavailable
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.registrations[id]
	if r == nil || r.binding.Session != session || r.claimed || !s.now().Before(r.expires) || !s.runtime.Alive(ctx, r.binding) {
		return Handle{}, ErrConsumed
	}
	if r.owner != "" && r.owner != user {
		return Handle{}, ErrConsumed
	}
	if r.owner == "" {
		r.owner = user
	}
	r.claimed = true
	handle := randomID()
	s.tickets[handle] = &ticket{registration: r, user: user}
	return Handle{handle, r.expires}, nil
}

// Authorization returns the exact native URL the waiting CLI launched, so the
// Codex helper can reopen the login page instead of starting a
// second login. The gates match Begin, but the fetch neither claims nor
// consumes the attempt, and it holds no URL for any other flow.
func (s *Service) Authorization(ctx context.Context, user, session, id string) (string, error) {
	if user == "" {
		return "", ErrUnavailable
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.registrations[id]
	if r == nil {
		return "", ErrConsumed
	}
	expired := !s.now().Before(r.expires)
	if r.binding.Session != session || r.claimed || expired || !s.runtime.Alive(ctx, r.binding) || (r.owner != "" && r.owner != user) {
		if expired {
			r.url = ""
		}
		return "", ErrConsumed
	}
	if r.flow != CodexBrowser || r.url == "" {
		return "", ErrUnavailable
	}
	r.owner = user
	return r.url, nil
}

// AuthorizationForHandle allows retrying the browser step after a rejected
// callback paste left the same user's live claim intact.
func (s *Service) AuthorizationForHandle(ctx context.Context, user, session, id string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.tickets[id]
	if user == "" || t == nil || t.used || t.user != user {
		return "", ErrConsumed
	}
	r := t.registration
	if !s.now().Before(r.expires) {
		r.url = ""
		return "", ErrConsumed
	}
	if r.binding.Session != session || !s.runtime.Alive(ctx, r.binding) || r.flow != CodexBrowser || r.url == "" {
		return "", ErrUnavailable
	}
	return r.url, nil
}
func (s *Service) Cancel(user, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.tickets[id]
	if t == nil || t.used || t.user != user || !s.now().Before(t.registration.expires) {
		return ErrConsumed
	}
	t.used = true
	// Cancellation is terminal for the attempt: nothing may reopen this login.
	t.registration.url = ""
	return nil
}

// Dismiss lets the same user reclaim a live attempt with a fresh handle.
// Cancellation and submission remain terminal.
func (s *Service) Dismiss(ctx context.Context, user, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.tickets[id]
	if t == nil || t.used || t.user != user || !s.now().Before(t.registration.expires) {
		return ErrConsumed
	}
	if !s.runtime.Alive(ctx, t.registration.binding) {
		return ErrConsumed
	}
	// Delete rather than mark used: retaining released handles lets repeated
	// begin/dismiss cycles grow the ticket map without bound.
	delete(s.tickets, id)
	t.registration.claimed = false
	return nil
}
func (s *Service) Submit(ctx context.Context, user, id, raw string) (string, error) {
	s.mu.Lock()
	t := s.tickets[id]
	if t == nil || t.used || t.user != user || !s.now().Before(t.registration.expires) {
		s.mu.Unlock()
		return "", ErrConsumed
	}
	r := t.registration
	target, err := r.callback.requestTarget(raw)
	if err != nil {
		s.mu.Unlock()
		return "", ErrInvalid
	}
	t.used = true
	r.url = ""
	remaining := min(20*time.Second, r.expires.Sub(s.now()))
	s.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, remaining)
	defer cancel()
	return deliver(ctx, s.runtime, r.binding, r.flow, r.callback, target)
}
