package store

import (
	"context"
	"fmt"
	"time"

	"subsyncd/internal/control"
)

// QueueClassCount counts pending searches of one priority class.
type QueueClassCount struct{ Total, Due int }

// QueueCounts counts the pending searches of active media by priority, and
// how many are due at now: pending, scheduled, and not held by a live lease.
func (r *Repository) QueueCounts(ctx context.Context, now time.Time) (map[SearchPriority]QueueClassCount, error) {
	rows, err := r.store.db.QueryContext(ctx, `SELECT s.priority, count(*), COALESCE(sum(s.next_attempt_at_ns <= ?1 AND (s.lease_until_ns IS NULL OR s.lease_until_ns <= ?1)), 0)
FROM search_states s JOIN media m ON m.id=s.media_id WHERE m.deleted=0 AND s.state='pending' GROUP BY s.priority`, now.UnixNano())
	if err != nil {
		return nil, fmt.Errorf("count queue: %w", err)
	}
	defer rows.Close()
	counts := map[SearchPriority]QueueClassCount{}
	for rows.Next() {
		var priority SearchPriority
		var count QueueClassCount
		if err := rows.Scan(&priority, &count.Total, &count.Due); err != nil {
			return nil, fmt.Errorf("scan queue count: %w", err)
		}
		counts[priority] = count
	}
	return counts, rows.Err()
}

// RunningSearch is a search held by a live lease. The store keeps when the
// lease expires, not when it began.
type RunningSearch struct {
	control.MediaMatch
	Language   string
	LeaseUntil time.Time
}

// RunningSearches lists the searches whose lease is live at now, soonest
// lease expiry first.
func (r *Repository) RunningSearches(ctx context.Context, now time.Time) ([]RunningSearch, error) {
	rows, err := r.store.db.QueryContext(ctx, `SELECT m.instance, m.kind, m.file_id, m.title, m.year, m.season, m.episode, m.episode_end, s.language, s.lease_until_ns
FROM search_states s JOIN media m ON m.id=s.media_id WHERE m.deleted=0 AND s.lease_until_ns > ? ORDER BY s.lease_until_ns, m.id, s.language`, now.UnixNano())
	if err != nil {
		return nil, fmt.Errorf("list running searches: %w", err)
	}
	defer rows.Close()
	var running []RunningSearch
	for rows.Next() {
		var item RunningSearch
		var lease int64
		if err := rows.Scan(&item.Instance, &item.Kind, &item.FileID, &item.Title, &item.Year, &item.Season, &item.Episode, &item.EpisodeEnd, &item.Language, &lease); err != nil {
			return nil, fmt.Errorf("scan running search: %w", err)
		}
		item.LeaseUntil = time.Unix(0, lease).UTC()
		running = append(running, item)
	}
	return running, rows.Err()
}

// NotificationBacklog counts outbox notifications still waiting for delivery
// or a retry.
func (r *Repository) NotificationBacklog(ctx context.Context) (int, error) {
	var count int
	if err := r.store.db.QueryRowContext(ctx, `SELECT count(*) FROM notifications WHERE next_attempt_at_ns > 0`).Scan(&count); err != nil {
		return 0, fmt.Errorf("count notification backlog: %w", err)
	}
	return count, nil
}

// LastInstalledByProvider returns when each provider last supplied an
// installed subtitle, from the retained search history.
func (r *Repository) LastInstalledByProvider(ctx context.Context) (map[string]time.Time, error) {
	rows, err := r.store.db.QueryContext(ctx, `SELECT provider_id, max(finished_at_ns) FROM search_history WHERE provider_id <> '' AND outcome='installed' GROUP BY provider_id`)
	if err != nil {
		return nil, fmt.Errorf("read last installs: %w", err)
	}
	defer rows.Close()
	last := map[string]time.Time{}
	for rows.Next() {
		var provider string
		var at int64
		if err := rows.Scan(&provider, &at); err != nil {
			return nil, fmt.Errorf("scan last install: %w", err)
		}
		last[provider] = time.Unix(0, at).UTC()
	}
	return last, rows.Err()
}
