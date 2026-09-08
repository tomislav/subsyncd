package store

import (
	"context"
	"fmt"
	"time"

	"subsyncd/internal/domain"
)

// EnsureUpgradeSearch retains a manual search's next upgrade without displacing
// earlier queued work or altering any durable lease, including expired leases.
func (r *Repository) EnsureUpgradeSearch(ctx context.Context, mediaID int64, language domain.Language, next time.Time) error {
	if next.IsZero() {
		return fmt.Errorf("upgrade search requires a nonzero next attempt")
	}
	_, err := r.store.db.ExecContext(ctx, `
 INSERT INTO search_states(media_id,language,state,next_attempt_at_ns,priority)
 SELECT id,?,'pending',?,? FROM media WHERE id=? AND deleted=0 AND unsupported_reason=''
 ON CONFLICT(media_id,language) DO UPDATE SET
 state='pending',
 next_attempt_at_ns=excluded.next_attempt_at_ns,
 priority=CASE WHEN search_states.state='complete' THEN excluded.priority ELSE search_states.priority END,
 attempt=CASE WHEN search_states.state='complete' THEN 0 ELSE search_states.attempt END,
 failure_attempt=CASE WHEN search_states.state='complete' THEN 0 ELSE search_states.failure_attempt END,
 last_outcome=CASE WHEN search_states.state='complete' THEN '' ELSE search_states.last_outcome END,
 rerun_requested=CASE WHEN search_states.state='complete' THEN 0 ELSE search_states.rerun_requested END
 WHERE search_states.lease_owner IS NULL AND search_states.lease_until_ns IS NULL
 AND (search_states.state='complete' OR (search_states.state='pending' AND search_states.next_attempt_at_ns>excluded.next_attempt_at_ns))`, language.String(), next.UnixNano(), SearchPriorityUpgrade, mediaID)
	if err != nil {
		return fmt.Errorf("ensure upgrade search: %w", err)
	}
	return nil
}
