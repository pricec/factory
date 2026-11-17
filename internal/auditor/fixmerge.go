package auditor

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
)

// FixBranchPrefix is the branch namespace of auto-fix PRs (FixBranch). CI
// makes the audit-guardrail check fail-closed for exactly these branches.
const FixBranchPrefix = "auto/audit-"

// RequestedByAuditor is the merge requests' requested_by for auto-fixes.
const RequestedByAuditor = "auditor"

// IsAuditMerge reports whether a merge request is an auditor auto-fix.
func IsAuditMerge(requestedBy, branch string) bool {
	return requestedBy == RequestedByAuditor && strings.HasPrefix(branch, FixBranchPrefix)
}

// FixForBranch finds the newest fix job assigned branch and returns its
// payload and id (ok=false when there is none).
func FixForBranch(ctx context.Context, db *sql.DB, branch string) (p FixPayload, jobID string, ok bool, err error) {
	var spec string
	err = db.QueryRowContext(ctx, `
SELECT id, spec FROM jobs
WHERE json_extract(spec, '$.created_by') = ? AND json_extract(spec, '$.payload.kind') = ?
  AND json_extract(spec, '$.payload.branch') = ?
ORDER BY created_at DESC LIMIT 1`, CreatedBy, PayloadKindFix, branch).Scan(&jobID, &spec)
	if errors.Is(err, sql.ErrNoRows) {
		return p, "", false, nil
	}
	if err != nil {
		return p, "", false, err
	}
	var s struct {
		Payload FixPayload `json:"payload"`
	}
	if err := json.Unmarshal([]byte(spec), &s); err != nil {
		return p, jobID, false, err
	}
	return s.Payload, jobID, s.Payload.TaskID != "", nil
}

// MergeFailureReason renders why an auto-fix merge request failed, naming
// the auditor guardrail when that is what refused it.
func MergeFailureReason(mrID, result string) string {
	result = clip(strings.TrimSpace(result), 600)
	if result == "" {
		result = "no result recorded"
	}
	lc := strings.ToLower(result)
	if strings.Contains(lc, "audit-guardrail") || strings.Contains(lc, "auditor guardrail") {
		return "the auditor guardrail check (CI audit-guardrail, run from master) refused the branch's diff; merge request " +
			mrID + " needs attention: " + result
	}
	return "merge request " + mrID + " needs attention (CI or the merge failed): " + result
}
