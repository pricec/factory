package tasks

import (
	"context"
	"database/sql"
	"errors"
	"unicode/utf8"
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

// Settings keys of the standing chat's rolling summary: the summary text
// and the id of the newest message already folded into it.
const (
	KeyStandingSummary        = "standing_summary"
	KeyStandingSummaryThrough = "standing_summary_through"
)

// StandingSummaryMax bounds the stored summary in bytes (it rides every
// standing-chat wake, so it must stay small).
const StandingSummaryMax = 4000

// StandingSummary returns the standing chat's running summary of older
// messages and the id of the newest message it covers ("" when none).
func (s *Store) StandingSummary(ctx context.Context) (summary, through string, err error) {
	summary, _, err = s.GetSetting(ctx, KeyStandingSummary)
	if err != nil {
		return "", "", err
	}
	through, _, err = s.GetSetting(ctx, KeyStandingSummaryThrough)
	return summary, through, err
}

// SetStandingSummary stores the running summary and the id of the newest
// message it covers. The summary is cut to StandingSummaryMax bytes.
func (s *Store) SetStandingSummary(ctx context.Context, summary, through string) error {
	if len(summary) > StandingSummaryMax {
		summary = summary[:StandingSummaryMax]
		for len(summary) > 0 && !utf8.ValidString(summary) {
			summary = summary[:len(summary)-1]
		}
	}
	if err := s.SetSetting(ctx, KeyStandingSummary, summary); err != nil {
		return err
	}
	return s.SetSetting(ctx, KeyStandingSummaryThrough, through)
}
