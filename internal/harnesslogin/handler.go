package harnesslogin

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"

	"github.com/noisefloor-oss/deck/internal/auth"
)

const CSRFHeader = "X-Harness-Login-CSRF"

type Handler struct {
	Service *Service
	Origins []string
	User    string
	secret  [32]byte
}

func NewHandler(service *Service, origins []string) *Handler {
	h := &Handler{Service: service, Origins: append([]string(nil), origins...)}
	if _, err := rand.Read(h.secret[:]); err != nil {
		panic("harness login randomness unavailable")
	}
	return h
}
func (h *Handler) csrf(principal string) string {
	mac := hmac.New(sha256.New, h.secret[:])
	mac.Write([]byte(principal))
	return hex.EncodeToString(mac.Sum(nil))
}
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	principal := auth.Principal(r.Context())
	if principal == "" {
		http.Error(w, "Harness login requires authentication.", http.StatusUnauthorized)
		return
	}
	if h.User != "" && principal != h.User {
		http.Error(w, "Harness login request refused.", http.StatusForbidden)
		return
	}
	if r.Method == http.MethodGet {
		if r.URL.Query().Get("watch") == "1" {
			h.watch(w, r)
			return
		}
		if h.Service == nil {
			respond(w, map[string]any{"pending": []Pending{}, "unavailable": true})
			return
		}
		pending := h.Service.Pending(r.Context(), principal, r.URL.Query().Get("session"))
		respond(w, map[string]any{"pending": pending, "csrf": h.csrf(principal)})
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed.", http.StatusMethodNotAllowed)
		return
	}
	originAllowed := false
	for _, origin := range h.Origins {
		if origin != "" && r.Header.Get("Origin") == origin {
			originAllowed = true
		}
	}
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if !originAllowed || !hmac.Equal([]byte(r.Header.Get(CSRFHeader)), []byte(h.csrf(principal))) || err != nil || media != "application/json" {
		http.Error(w, "Harness login request refused.", http.StatusForbidden)
		return
	}
	if h.Service == nil {
		http.Error(w, "Harness login is unavailable.", http.StatusServiceUnavailable)
		return
	}
	var body struct {
		Action       string `json:"action"`
		Session      string `json:"session"`
		Registration string `json:"registration"`
		Handle       string `json:"handle"`
		Callback     string `json:"callback"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, MaxURLBytes+2048))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&body) != nil || decoder.Decode(new(any)) != io.EOF {
		http.Error(w, "Invalid harness login request.", http.StatusBadRequest)
		return
	}
	switch body.Action {
	case "authorization":
		var url string
		var err error
		if body.Handle != "" {
			url, err = h.Service.AuthorizationForHandle(r.Context(), principal, body.Session, body.Handle)
		} else {
			url, err = h.Service.Authorization(r.Context(), principal, body.Session, body.Registration)
		}
		if err != nil {
			http.Error(w, "Login attempt is no longer available.", http.StatusConflict)
			return
		}
		respond(w, map[string]string{"url": url})
	case "begin":
		handle, err := h.Service.Begin(r.Context(), principal, body.Session, body.Registration)
		if err != nil {
			http.Error(w, "Login attempt is no longer available.", http.StatusConflict)
			return
		}
		respond(w, handle)
	case "cancel":
		if h.Service.Cancel(principal, body.Handle) != nil {
			http.Error(w, "Login attempt is no longer available.", http.StatusConflict)
			return
		}
		respond(w, map[string]string{"status": "cancelled"})
	case "dismiss":
		if h.Service.Dismiss(r.Context(), principal, body.Handle) != nil {
			http.Error(w, "Login attempt is no longer available.", http.StatusConflict)
			return
		}
		respond(w, map[string]string{"status": "dismissed"})
	case "submit":
		status, err := h.Service.Submit(r.Context(), principal, body.Handle, body.Callback)
		body.Callback = ""
		if errors.Is(err, ErrInvalid) {
			http.Error(w, "Callback does not match this login attempt.", http.StatusBadRequest)
			return
		}
		if err != nil {
			http.Error(w, "Login could not be confirmed. Check the native CLI before starting another login.", http.StatusConflict)
			return
		}
		respond(w, map[string]string{"status": status})
	default:
		http.Error(w, "Invalid harness login request.", http.StatusBadRequest)
	}
}
func respond(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}
