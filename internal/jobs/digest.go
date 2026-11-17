package jobs

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// ResultDigest is the local-model condensation of one job result (spec
// §6.13). It is stored ALONGSIDE the results array (column
// result_digest) and never replaces it: the verbatim result stays in
// results and read_result(full=true) returns it.
type ResultDigest struct {
	// ResultIndex is the index in the results array of the result this
	// digest condenses; a digest whose index is not the last result is
	// stale (a retry appended a newer result) and is ignored.
	ResultIndex int    `json:"result_index"`
	Text        string `json:"text"`
	Model       string `json:"model"`
	InputBytes  int    `json:"input_bytes"`
	Truncated   bool   `json:"truncated,omitempty"`
	DurationMS  int64  `json:"duration_ms"`
	CreatedAt   string `json:"created_at"`
}

// SetResultDigest stores d for job id (last write wins).
func (s *Store) SetResultDigest(ctx context.Context, id string, d ResultDigest) error {
	if d.CreatedAt == "" {
		d.CreatedAt = s.now()
	}
	b, err := json.Marshal(d)
	if err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, `UPDATE jobs SET result_digest = ? WHERE id = ?`, string(b), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("set result digest: %w: %s", ErrNoSuchJob, id)
	}
	return nil
}

// ResultDigest returns the stored digest for job id; ok is false when
// there is none (or it is unreadable).
func (s *Store) ResultDigest(ctx context.Context, id string) (ResultDigest, bool, error) {
	var raw sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT result_digest FROM jobs WHERE id = ?`, id).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return ResultDigest{}, false, nil
	}
	if err != nil {
		return ResultDigest{}, false, err
	}
	if !raw.Valid || raw.String == "" {
		return ResultDigest{}, false, nil
	}
	var d ResultDigest
	if err := json.Unmarshal([]byte(raw.String), &d); err != nil || d.Text == "" {
		return ResultDigest{}, false, nil
	}
	return d, true, nil
}
