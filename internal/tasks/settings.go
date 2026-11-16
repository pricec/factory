package tasks

import (
	"context"
	"database/sql"
	"errors"
)

// KeyPriorityCriteria is the settings key of the operator's free-text
// priority criteria (set by the planner's set_priority_criteria tool).
const KeyPriorityCriteria = "priority_criteria"

// GetSetting returns the value of key; ok is false when unset.
func (s *Store) GetSetting(ctx context.Context, key string) (value string, ok bool, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return value, true, nil
}

// SetSetting upserts key = value.
func (s *Store) SetSetting(ctx context.Context, key, value string) error {
	if key == "" {
		return errInvalid("key: required")
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO settings (key, value) VALUES (?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

// PriorityCriteria returns the priority_criteria text ("" when unset).
func (s *Store) PriorityCriteria(ctx context.Context) (string, error) {
	v, _, err := s.GetSetting(ctx, KeyPriorityCriteria)
	return v, err
}

// SetPriorityCriteria stores the priority_criteria text.
func (s *Store) SetPriorityCriteria(ctx context.Context, text string) error {
	return s.SetSetting(ctx, KeyPriorityCriteria, text)
}
