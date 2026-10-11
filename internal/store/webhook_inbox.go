package store

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// WebhookInboxItem is an accepted Arr webhook waiting to be applied.
type WebhookInboxItem struct {
	ID         int64
	Instance   string
	Body       []byte
	ReceivedAt time.Time
	Attempt    int
}

// EnqueueWebhook durably accepts a webhook body before it is applied, so the
// HTTP response does not wait for Arr lookups and a restart loses nothing.
func (r *Repository) EnqueueWebhook(ctx context.Context, instance string, body []byte, receivedAt time.Time) (int64, error) {
	if instance == "" || len(body) == 0 || receivedAt.IsZero() {
		return 0, fmt.Errorf("webhook instance, body and receipt time are required")
	}
	result, err := r.store.db.ExecContext(ctx, `INSERT INTO webhook_inbox(instance, body, received_at_ns, next_attempt_at_ns) VALUES (?, ?, ?, ?)`, instance, body, receivedAt.UnixNano(), receivedAt.UnixNano())
	if err != nil {
		return 0, fmt.Errorf("enqueue webhook: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("read enqueued webhook ID: %w", err)
	}
	return id, nil
}

// DueWebhooks lists an instance's webhooks due at now in arrival order.
func (r *Repository) DueWebhooks(ctx context.Context, instance string, now time.Time, limit int) ([]WebhookInboxItem, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("webhook batch limit must be positive")
	}
	rows, err := r.store.db.QueryContext(ctx, `SELECT id, instance, body, received_at_ns, attempt FROM webhook_inbox WHERE instance=? AND next_attempt_at_ns <= ? ORDER BY id LIMIT ?`, instance, now.UnixNano(), limit)
	if err != nil {
		return nil, fmt.Errorf("list due webhooks: %w", err)
	}
	var items []WebhookInboxItem
	for rows.Next() {
		var item WebhookInboxItem
		var received int64
		if err := rows.Scan(&item.ID, &item.Instance, &item.Body, &received, &item.Attempt); err != nil {
			return nil, errors.Join(fmt.Errorf("scan due webhook: %w", err), rows.Close())
		}
		item.ReceivedAt = time.Unix(0, received).UTC()
		items = append(items, item)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, fmt.Errorf("list due webhooks: %w", err)
	}
	return items, nil
}

// NextWebhookAt reports when an instance's earliest waiting webhook is due.
func (r *Repository) NextWebhookAt(ctx context.Context, instance string) (time.Time, bool, error) {
	var next *int64
	if err := r.store.db.QueryRowContext(ctx, `SELECT MIN(next_attempt_at_ns) FROM webhook_inbox WHERE instance=?`, instance).Scan(&next); err != nil {
		return time.Time{}, false, fmt.Errorf("read next webhook: %w", err)
	}
	if next == nil {
		return time.Time{}, false, nil
	}
	return time.Unix(0, *next).UTC(), true, nil
}

// DeleteWebhook removes an applied or abandoned webhook.
func (r *Repository) DeleteWebhook(ctx context.Context, id int64) error {
	if _, err := r.store.db.ExecContext(ctx, `DELETE FROM webhook_inbox WHERE id=?`, id); err != nil {
		return fmt.Errorf("delete webhook: %w", err)
	}
	return nil
}

// RetryWebhook counts a failed attempt and makes the webhook due at next.
func (r *Repository) RetryWebhook(ctx context.Context, id int64, next time.Time) error {
	if _, err := r.store.db.ExecContext(ctx, `UPDATE webhook_inbox SET attempt=attempt+1, next_attempt_at_ns=? WHERE id=?`, next.UnixNano(), id); err != nil {
		return fmt.Errorf("reschedule webhook: %w", err)
	}
	return nil
}

// WebhookInboxCounts reports how many webhooks wait for each instance.
func (r *Repository) WebhookInboxCounts(ctx context.Context) (map[string]int, error) {
	rows, err := r.store.db.QueryContext(ctx, `SELECT instance, count(*) FROM webhook_inbox GROUP BY instance ORDER BY instance`)
	if err != nil {
		return nil, fmt.Errorf("count waiting webhooks: %w", err)
	}
	counts := map[string]int{}
	for rows.Next() {
		var instance string
		var count int
		if err := rows.Scan(&instance, &count); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("scan waiting webhook count: %w", err)
		}
		counts[instance] = count
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, fmt.Errorf("count waiting webhooks: %w", err)
	}
	return counts, nil
}
