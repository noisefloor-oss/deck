package boot

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/noisefloor-oss/deck/internal/session"
)

func workspaceLockHandler(prepare func(context.Context) error) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if r.ContentLength != 0 || len(r.TransferEncoding) != 0 {
			http.Error(w, "workspace lock preparation refused", http.StatusBadRequest)
			return
		}
		budget := session.WorkspaceLockTimeout
		if v := r.Header.Get(session.WorkspaceLockBudgetHeader); v != "" {
			ms, err := strconv.ParseInt(v, 10, 64)
			if err != nil || ms <= 0 {
				http.Error(w, "workspace lock preparation refused", http.StatusBadRequest)
				return
			}
			if ms < budget.Milliseconds() {
				budget = time.Duration(ms) * time.Millisecond
			}
		}
		ctx, cancel := context.WithTimeout(r.Context(), budget)
		defer cancel()
		if err := prepare(ctx); err != nil {
			if errors.Is(err, session.ErrWorkspaceLockRefused) {
				http.Error(w, "workspace lock preparation refused", http.StatusUnprocessableEntity)
				return
			}
			http.Error(w, "workspace lock preparation unconfirmed", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}
