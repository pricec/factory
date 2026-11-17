package jobs

import (
	"context"
	"database/sql"
	"errors"
)

// Settings persistence (spec §6.5, 2026-11-15 amendment): the factory
// stores the operator's panel-set role models in the jobs database. The
// store is a generic role -> model map; role semantics and precedence
// (stored setting > env var > pinned default) live in the callers.

// settingsDDL is the role -> model table (spec §6.5, 2026-11-15
// amendment). It lives in jobs.db, distinct from the tasks store's
// free-form key/value settings table in inbox.db.
const settingsDDL = `
CREATE TABLE IF NOT EXISTS settings (
	role       TEXT PRIMARY KEY,
	model      TEXT NOT NULL,
	updated_at TEXT NOT NULL
);
`

// Setting returns the stored model for role; ok is false when the role
// carries no stored setting (the caller falls back to env / default).
func (s *Store) Setting(ctx context.Context, role string) (model string, ok bool, err error) {
	err = s.db.QueryRowContext(ctx,
		`SELECT model FROM settings WHERE role = ?`, role).Scan(&model)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return model, err == nil, err
}

// SetSetting upserts the stored model for role.
func (s *Store) SetSetting(ctx context.Context, role, model string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO settings (role, model, updated_at) VALUES (?, ?, ?)
		 ON CONFLICT(role) DO UPDATE SET model = excluded.model, updated_at = excluded.updated_at`,
		role, model, s.now())
	return err
}

// ClearSetting removes the stored model for role; clearing an absent
// setting is a no-op, never an error.
func (s *Store) ClearSetting(ctx context.Context, role string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM settings WHERE role = ?`, role)
	return err
}

// AllSettings returns every stored role -> model pair (empty map when
// nothing is stored).
func (s *Store) AllSettings(ctx context.Context) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT role, model FROM settings`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var role, model string
		if err := rows.Scan(&role, &model); err != nil {
			return nil, err
		}
		out[role] = model
	}
	return out, rows.Err()
}
