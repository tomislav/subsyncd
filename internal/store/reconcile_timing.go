package store

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// ReconcileTiming is one instance's last reconciliation attempt, kept in the
// database so a restart neither repeats a reconciliation that just ran nor
// forgets the failure backoff.
type ReconcileTiming struct {
	LastAttempt time.Time
	Failures    int
	Deferred    bool
}

// ReconcileTimings returns the saved timing of every instance that has one.
func (r *Repository) ReconcileTimings(ctx context.Context) (map[string]ReconcileTiming, error) {
	rows, err := r.store.db.QueryContext(ctx, `SELECT name, reconcile_attempted_at_ns, reconcile_failures, reconcile_deferred FROM instances WHERE reconcile_attempted_at_ns > 0`)
	if err != nil {
		return nil, fmt.Errorf("read reconciliation timing: %w", err)
	}
	timings := make(map[string]ReconcileTiming)
	for rows.Next() {
		var name string
		var attempted int64
		var timing ReconcileTiming
		if err := rows.Scan(&name, &attempted, &timing.Failures, &timing.Deferred); err != nil {
			return nil, errors.Join(fmt.Errorf("scan reconciliation timing: %w", err), rows.Close())
		}
		timing.LastAttempt = time.Unix(0, attempted).UTC()
		timings[name] = timing
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, fmt.Errorf("read reconciliation timing: %w", err)
	}
	return timings, nil
}

// RecordReconcileTiming saves one instance's latest reconciliation attempt.
func (r *Repository) RecordReconcileTiming(ctx context.Context, instance string, timing ReconcileTiming) error {
	if _, err := r.store.db.ExecContext(ctx, `UPDATE instances SET reconcile_attempted_at_ns=?, reconcile_failures=?, reconcile_deferred=? WHERE name=?`, timing.LastAttempt.UnixNano(), timing.Failures, timing.Deferred, instance); err != nil {
		return fmt.Errorf("record reconciliation timing: %w", err)
	}
	return nil
}
