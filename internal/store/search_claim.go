package store

import (
	"context"
	"fmt"
	"time"

	"subsyncd/internal/domain"
)

// ClaimSearch takes the lease of one media file's search for one language for
// owner jobID, as a manual search does, so the worker cannot lease it at the
// same time. A missing search row is created as a pending missing-subtitle
// search. It reports false, changing nothing, while another owner holds a live
// lease; an expired lease counts as no lease, and claiming it clears a rerun
// the dead owner left requested, as LeaseDueSearches does. jobID may claim a
// lease it already holds, as reject leases its search before the manual run
// claims it. The search's schedule and progress are left as they are.
func (r *Repository) ClaimSearch(ctx context.Context, mediaID int64, language domain.Language, jobID string, now time.Time, lease time.Duration) (bool, error) {
	if jobID == "" || lease <= 0 {
		return false, fmt.Errorf("search lease owner and duration are required")
	}
	result, err := r.store.db.ExecContext(ctx, `INSERT INTO search_states(media_id, language, state, attempt, failure_attempt, next_attempt_at_ns, queue_order_ns, priority, lease_owner, lease_until_ns)
 VALUES (?, ?, 'pending', 0, 0, ?, ?, ?, ?, ?)
 ON CONFLICT(media_id, language) DO UPDATE SET lease_owner=excluded.lease_owner, lease_until_ns=excluded.lease_until_ns, rerun_requested=0
 WHERE search_states.lease_until_ns IS NULL OR search_states.lease_until_ns <= ? OR search_states.lease_owner = excluded.lease_owner`,
		mediaID, language.String(), now.UnixNano(), now.UnixNano(), SearchPriorityMissing, jobID, now.Add(lease).UnixNano(), now.UnixNano())
	if err != nil {
		return false, fmt.Errorf("claim search: %w", err)
	}
	claimed, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("count claimed search: %w", err)
	}
	return claimed == 1, nil
}
