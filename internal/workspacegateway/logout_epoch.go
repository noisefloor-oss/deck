package workspacegateway

import (
	"context"
	"database/sql"
	"errors"

	"github.com/noisefloor-oss/deck/internal/auth"
)

type logoutEpochStore struct {
	db *sql.DB
}

func NewLogoutEpochStore(ctx context.Context, db *sql.DB) (auth.EpochStore, error) {
	if db == nil {
		return nil, errors.New("workspace logout epoch store requires a database")
	}
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS workspace_logout_epoch (
		login TEXT PRIMARY KEY, epoch INTEGER NOT NULL
	)`); err != nil {
		return nil, err
	}
	return &logoutEpochStore{db: db}, nil
}

func (s *logoutEpochStore) LoadLogoutEpochs(ctx context.Context) (map[string]uint64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT login, epoch FROM workspace_logout_epoch`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	epochs := map[string]uint64{}
	for rows.Next() {
		var login string
		var epoch uint64
		if err := rows.Scan(&login, &epoch); err != nil {
			return nil, err
		}
		epochs[login] = epoch
	}
	return epochs, rows.Err()
}

func (s *logoutEpochStore) SaveLogoutEpoch(ctx context.Context, loginID string, epoch uint64) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO workspace_logout_epoch(login,epoch) VALUES(?,?)
		ON CONFLICT(login) DO UPDATE SET epoch=excluded.epoch`, loginID, epoch)
	return err
}
