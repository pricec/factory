package jobs

import "context"

// RecordSecretGrants audits that the named secrets were handed out for
// jobID (spec §13.2): one secret_grants row per name, written at fetch
// time. Names only - the table never holds a value.
func (s *Store) RecordSecretGrants(ctx context.Context, jobID string, names []string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	at := s.now()
	for _, n := range names {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO secret_grants (job_id, name, granted_at) VALUES (?, ?, ?)`, jobID, n, at); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// GrantedSecrets returns the distinct secret names ever granted to jobID,
// sorted. Never values.
func (s *Store) GrantedSecrets(ctx context.Context, jobID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT DISTINCT name FROM secret_grants WHERE job_id = ? ORDER BY name`, jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}
