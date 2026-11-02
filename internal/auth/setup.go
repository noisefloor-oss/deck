package auth

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"unicode/utf8"

	"golang.org/x/crypto/bcrypt"
)

func (h *Handler) SetupPending() bool {
	h.credentialMu.RLock()
	defer h.credentialMu.RUnlock()
	return h.cfg.Mode == "password" && len(h.credentials.PasswordHash) == 0
}

// SetupPage gates the public shell on durable password state.
func (h *Handler) SetupPage(shell http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Referrer-Policy", "no-referrer")
		if !h.SetupPending() {
			http.NotFound(w, r)
			return
		}
		copy := r.Clone(r.Context())
		copy.URL.Path = "/"
		shell.ServeHTTP(w, copy)
	})
}

type passwordUpdate struct {
	Token           string `json:"token,omitempty"`
	Password        string `json:"password,omitempty"`
	CurrentPassword string `json:"current_password,omitempty"`
}

func (h *Handler) readPasswordUpdate(w http.ResponseWriter, r *http.Request) (passwordUpdate, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, maxPasswordBody)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	var body passwordUpdate
	err := dec.Decode(&body)
	return body, err == nil && dec.Decode(&struct{}{}) == io.EOF && len(body.Token) <= 128 &&
		len(body.Password) <= maxPasswordLength && len(body.CurrentPassword) <= maxPasswordLength
}

func (h *Handler) setupAdmission(w http.ResponseWriter, r *http.Request) bool {
	w.Header().Set("Cache-Control", "no-store")
	if !h.SetupPending() {
		http.NotFound(w, r)
		return false
	}
	allowed := false
	for _, origin := range h.cfg.AllowedOrigins {
		if r.Header.Get("Origin") == origin {
			allowed = true
		}
	}
	if !allowed || strings.Split(r.Header.Get("Content-Type"), ";")[0] != "application/json" {
		http.Error(w, "forbidden", http.StatusForbidden)
		return false
	}
	return h.passwordAdmission(w, r)
}

func (h *Handler) passwordAdmission(w http.ResponseWriter, r *http.Request) bool {
	if ok, wait := h.password.admit(h.passwordSource(r)); !ok {
		w.Header().Set("Retry-After", retryAfter(wait))
		http.Error(w, "password request failed", http.StatusTooManyRequests)
		return false
	}
	return true
}

func (h *Handler) handleSetupCheck(w http.ResponseWriter, r *http.Request) {
	if !h.setupAdmission(w, r) {
		return
	}
	body, ok := h.readPasswordUpdate(w, r)
	h.credentialMu.Lock()
	defer h.credentialMu.Unlock()
	if err := h.renewSetupURL(); err != nil {
		h.password.record(h.passwordSource(r), true)
		http.Error(w, "setup link could not be renewed", http.StatusInternalServerError)
		return
	}
	valid := ok && validSetupToken(h.credentials, body.Token, h.now())
	h.password.record(h.passwordSource(r), valid)
	if !valid {
		http.Error(w, "setup link is missing, invalid, or expired", http.StatusUnauthorized)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) renewSetupURL() error {
	if len(h.credentials.PasswordHash) != 0 || h.now().Before(h.credentials.SetupExpires) {
		return nil
	}
	link, err := IssueSetupToken(h.cfg.PasswordStatePath, h.cfg.AllowedOrigins[0], h.now())
	if err != nil {
		return err
	}
	state, err := readBootstrapState(h.cfg.PasswordStatePath)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(h.cfg.SetupOutput, "Create your password: %s\n", link); err != nil {
		return err
	}
	h.credentials = state.State
	return nil
}

func (h *Handler) handleSetup(w http.ResponseWriter, r *http.Request) {
	if !h.setupAdmission(w, r) {
		return
	}
	body, ok := h.readPasswordUpdate(w, r)
	h.credentialMu.Lock()
	defer h.credentialMu.Unlock()
	if len(h.credentials.PasswordHash) != 0 {
		h.password.record(h.passwordSource(r), true)
		http.NotFound(w, r)
		return
	}
	if !ok || !validSetupToken(h.credentials, body.Token, h.now()) {
		h.password.record(h.passwordSource(r), false)
		http.Error(w, "setup link is missing, invalid, or expired", http.StatusUnauthorized)
		return
	}
	h.password.record(h.passwordSource(r), true)
	if !passwordLongEnough(body.Password) {
		refuseShortPassword(w)
		return
	}
	if err := h.savePassword(body.Password); err != nil {
		http.Error(w, "password could not be saved", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) handlePasswordChange(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !h.passwordAdmission(w, r) {
		return
	}
	body, ok := h.readPasswordUpdate(w, r)
	h.credentialMu.Lock()
	defer h.credentialMu.Unlock()
	if !ok || bcrypt.CompareHashAndPassword(h.credentials.PasswordHash, []byte(body.CurrentPassword)) != nil {
		h.password.record(h.passwordSource(r), false)
		http.Error(w, "password change failed", http.StatusUnauthorized)
		return
	}
	h.password.record(h.passwordSource(r), true)
	if h.cfg.PasswordPinned {
		http.Error(w, "Remove NOISEDECK_AUTH_PASSWORD_HASH from your service configuration and restart Deck before changing your password.", http.StatusConflict)
		return
	}
	if !passwordLongEnough(body.Password) {
		refuseShortPassword(w)
		return
	}
	if err := h.savePassword(body.Password); err != nil {
		http.Error(w, "password could not be saved", http.StatusInternalServerError)
		return
	}
	login, _ := CurrentLogin(r.Context())
	now := h.now()
	value, err := h.issueLoginSession(now, now, login)
	if err != nil {
		http.Error(w, "login unavailable", http.StatusInternalServerError)
		return
	}
	payload, err := h.verifySessionValue(value, now)
	if err != nil {
		http.Error(w, "login unavailable", http.StatusInternalServerError)
		return
	}
	h.setSessionCookies(w, value, payload)
	w.WriteHeader(http.StatusNoContent)
}

func passwordLongEnough(password string) bool {
	return utf8.RuneCountInString(password) >= minPasswordLength
}

func refuseShortPassword(w http.ResponseWriter) {
	http.Error(w, fmt.Sprintf("Use at least %d characters for your password.", minPasswordLength), http.StatusBadRequest)
}

func (h *Handler) savePassword(password string) error {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), passwordHashCost)
	if err != nil {
		return err
	}
	secret := make([]byte, sessionSecretBytes)
	if _, err := io.ReadFull(rand.Reader, secret); err != nil {
		return err
	}
	state := BootstrapState{PasswordHash: hash, SessionSecret: secret}
	if err := saveBootstrapState(h.cfg.PasswordStatePath, state, true); err != nil {
		// A rename followed by a failed sync may already have consumed the token.
		h.credentials.SetupHash = nil
		if loaded, readErr := readBootstrapState(h.cfg.PasswordStatePath); readErr == nil {
			h.replaceCredentials(loaded.State)
		}
		return err
	}
	h.replaceCredentials(state)
	if err := os.Remove(SetupURLPath(h.cfg.PasswordStatePath)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func (h *Handler) replaceCredentials(state BootstrapState) {
	h.credentials = state
	h.sessionMu.Lock()
	h.cfg.SessionSecret = state.SessionSecret
	h.sessionMu.Unlock()
}
