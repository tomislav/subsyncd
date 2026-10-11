package store

import (
	"context"
	"fmt"
)

// ReleaseSearchLease gives up a search lease its owner will not complete, as
// on shutdown, so the search is leased again at once instead of after the
// lease expires. The search keeps its schedule and progress. Releasing a lease
// that is no longer held does nothing.
func (r *Repository) ReleaseSearchLease(ctx context.Context, jobID string) error {
	if jobID == "" {
		return fmt.Errorf("search lease owner is required")
	}
	if _, err := r.store.db.ExecContext(ctx, `UPDATE search_states SET lease_owner=NULL, lease_until_ns=NULL, rerun_requested=0 WHERE lease_owner=?`, jobID); err != nil {
		return fmt.Errorf("release search lease: %w", err)
	}
	return nil
}

// ReleaseNotificationLease gives up a notification lease its owner will not
// complete; the notification keeps its attempt count and due time.
func (r *Repository) ReleaseNotificationLease(ctx context.Context, jobID string) error {
	if jobID == "" {
		return fmt.Errorf("notification lease owner is required")
	}
	if _, err := r.store.db.ExecContext(ctx, `UPDATE notifications SET lease_owner=NULL, lease_until_ns=NULL WHERE lease_owner=?`, jobID); err != nil {
		return fmt.Errorf("release notification lease: %w", err)
	}
	return nil
}
