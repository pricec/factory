package jobs

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"
)

// Reconciliation of running jobs (spec §6.2): the server believes a job is
// running only while its foreman does. A foreman that reconnects without
// claiming the job, or stays offline past a grace, has lost it.

// ReapRunning ends a running job whose worker is gone, through the
// ordinary state machine (running -> failed -> dead | retry armed). When a
// final result for the current attempt is already recorded (results holds
// more entries than attempts) the job goes straight to dead; merge jobs
// never retry (spec §6.11); otherwise it fails as a crash and the §6.4
// retry policy decides between a backed-off retry and dead. It returns
// whether the job was running.
func (s *Store) ReapRunning(ctx context.Context, id, reason string) (bool, error) {
	job, ok, err := s.Get(ctx, id)
	if err != nil || !ok || job.State != StateRunning {
		return false, err
	}
	var results []json.RawMessage
	_ = json.Unmarshal(job.Results, &results)
	hasFinal := len(results) > job.Attempts

	_, applied, err := s.transition(ctx, id, StateFailed)
	if err != nil || !applied {
		return false, err
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE jobs SET last_error = ? WHERE id = ?`,
		"reconciled: "+reason, id); err != nil {
		return true, err
	}
	log.Printf("jobs: reaped running job %s: %s", id, reason)
	_, isMerge, err := s.MergeRequestForJob(ctx, id)
	if err != nil {
		return true, err
	}
	if hasFinal || isMerge {
		return true, s.MarkDead(ctx, id)
	}
	return true, s.MarkFailedForRetry(ctx, id, "crash")
}

// ReconcileForeman reaps foremanID's running jobs absent from active (the
// ids its workers are live for, sent on the capabilities push) that have
// been quiet for reconcileGrace. Returns the reaped ids.
func (s *Store) ReconcileForeman(ctx context.Context, foremanID string, active []string) ([]string, error) {
	claimed := make(map[string]bool, len(active))
	for _, id := range active {
		claimed[id] = true
	}
	cutoff := s.Now().Add(-reconcileGrace).UTC().Format(time.RFC3339)
	rows, err := s.db.QueryContext(ctx,
		`SELECT id FROM jobs WHERE state = ? AND foreman_id = ? AND updated_at <= ?`,
		StateRunning, foremanID, cutoff)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		if !claimed[id] {
			ids = append(ids, id)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var reaped []string
	for _, id := range ids {
		ok, err := s.ReapRunning(ctx, id, fmt.Sprintf("foreman %s does not claim it", foremanID))
		if err != nil {
			return reaped, err
		}
		if ok {
			reaped = append(reaped, id)
		}
	}
	return reaped, nil
}

// reapOfflineForemen reaps running jobs whose foreman has been offline for
// offlineGrace.
func (s *Store) reapOfflineForemen(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx,
		`SELECT j.id, j.foreman_id FROM jobs j JOIN foremen f ON f.id = j.foreman_id
		 WHERE j.state = ? AND f.online = 0 AND f.last_seen <= ?`,
		StateRunning, s.Now().Add(-offlineGrace).UTC().Format(time.RFC3339))
	if err != nil {
		return err
	}
	type pair struct{ id, foreman string }
	var ps []pair
	for rows.Next() {
		var p pair
		if err := rows.Scan(&p.id, &p.foreman); err != nil {
			rows.Close()
			return err
		}
		ps = append(ps, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, p := range ps {
		if _, err := s.ReapRunning(ctx, p.id, "foreman "+p.foreman+" offline past grace"); err != nil {
			return err
		}
	}
	return nil
}

// settleStrandedFailed finishes failed jobs nobody armed: the result was
// applied but retry scheduling never ran (consumer crash between the two
// steps) or the failed result carried no error kind, so the job would sit
// in failed forever. Kind comes from the last recorded result; a missing
// kind is an unknown kind, i.e. dead-letter (spec §6.4).
func (s *Store) settleStrandedFailed(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, results FROM jobs WHERE state = ? AND next_attempt_at IS NULL AND updated_at <= ?`,
		StateFailed, s.Now().Add(-reconcileGrace).UTC().Format(time.RFC3339))
	if err != nil {
		return err
	}
	type stranded struct{ id, kind string }
	var list []stranded
	for rows.Next() {
		var id, raw string
		if err := rows.Scan(&id, &raw); err != nil {
			rows.Close()
			return err
		}
		var results []struct {
			Error *struct {
				Kind   string `json:"kind"`
				Reason string `json:"reason"`
			} `json:"error"`
		}
		_ = json.Unmarshal([]byte(raw), &results)
		kind := ""
		if n := len(results); n > 0 && results[n-1].Error != nil {
			kind = results[n-1].Error.Kind
			if r := results[n-1].Error.Reason; r == "budget" || r == "loop" {
				kind = r
			}
		}
		list = append(list, stranded{id, kind})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, st := range list {
		if _, isMerge, err := s.MergeRequestForJob(ctx, st.id); err != nil {
			return err
		} else if isMerge {
			err = s.MarkDead(ctx, st.id)
		} else {
			err = s.MarkFailedForRetry(ctx, st.id, st.kind)
		}
		if err != nil {
			return err
		}
		log.Printf("jobs: settled stranded failed job %s", st.id)
	}
	return nil
}
