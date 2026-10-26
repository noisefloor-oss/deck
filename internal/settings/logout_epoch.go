package settings

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Logout epochs persist the per-login logout generation the auth session
// verifier consults (auth.EpochStore). Keyed "auth/logout-epoch/<login>"
// with "" encoded as "legacy" so the empty login has a stable row.
const logoutEpochPrefix = "auth/logout-epoch/"

func logoutEpochKey(loginID string) string {
	if loginID == "" {
		return logoutEpochPrefix + "legacy"
	}
	return logoutEpochPrefix + loginID
}

// GetLogoutEpochs reads every persisted logout epoch. A row that does not
// parse is skipped rather than failing the load: one bad row must not
// unbind the others' logouts.
func (r *Repository) LoadLogoutEpochs(ctx context.Context) (map[string]uint64, error) {
	rows, err := r.db.Reader().QueryContext(ctx,
		`SELECT key, value FROM settings WHERE key LIKE ?`, logoutEpochPrefix+"%")
	if err != nil {
		return nil, fmt.Errorf("read logout epochs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	epochs := map[string]uint64{}
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return nil, fmt.Errorf("read logout epoch row: %w", err)
		}
		epoch, err := strconv.ParseUint(value, 10, 64)
		if err != nil || epoch == 0 {
			continue
		}
		loginID := strings.TrimPrefix(key, logoutEpochPrefix)
		if loginID == "legacy" {
			loginID = ""
		}
		epochs[loginID] = epoch
	}
	return epochs, rows.Err()
}

// SetLogoutEpoch writes one login's epoch.
func (r *Repository) SaveLogoutEpoch(ctx context.Context, loginID string, epoch uint64) error {
	return r.db.WithTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		return putString(ctx, tx, logoutEpochKey(loginID), strconv.FormatUint(epoch, 10), time.Now())
	})
}
