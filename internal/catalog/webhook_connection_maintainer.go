package catalog

import (
	"context"
	"errors"
	"log/slog"
	"sort"
	"time"

	"subsyncd/internal/observability"
)

const (
	webhookSyncInterval = 6 * time.Hour
	webhookSyncTimeout  = 60 * time.Second
)

// webhookRetryDelays are the waits after the first, second and third (and
// later) consecutive failures of one instance.
var webhookRetryDelays = [...]time.Duration{15 * time.Minute, time.Hour, webhookSyncInterval}

type webhookSyncer interface{ Sync(context.Context) error }

// WebhookConnectionMaintainer keeps every instance's webhook connection in
// sync: once at start, then every six hours, retrying failures sooner. It is
// independent of reconciliation and never blocks readiness.
type WebhookConnectionMaintainer struct {
	Syncs   map[string]webhookSyncer // keyed by instance name
	Events  *observability.Emitter
	After   func(time.Duration) <-chan time.Time // nil → time.After
	Now     func() time.Time                     // nil → time.Now
	Timeout time.Duration                        // 0 → 60s per instance
}

// NewWebhookConnectionMaintainer builds a maintainer over concrete syncs.
func NewWebhookConnectionMaintainer(syncs map[string]WebhookConnectionSync, events *observability.Emitter, now func() time.Time) *WebhookConnectionMaintainer {
	converted := make(map[string]webhookSyncer, len(syncs))
	for name, sync := range syncs {
		converted[name] = sync
	}
	return &WebhookConnectionMaintainer{Syncs: converted, Events: events, Now: now}
}

// Run syncs until ctx is canceled, then returns nil.
func (m *WebhookConnectionMaintainer) Run(ctx context.Context) error {
	after := m.After
	if after == nil {
		after = time.After
	}
	now := m.Now
	if now == nil {
		now = time.Now
	}
	timeout := m.Timeout
	if timeout <= 0 {
		timeout = webhookSyncTimeout
	}
	events := m.Events.For("catalog")
	names := make([]string, 0, len(m.Syncs))
	for name := range m.Syncs {
		names = append(names, name)
	}
	sort.Strings(names)
	if len(names) == 0 {
		<-ctx.Done()
		return nil
	}
	due := make(map[string]time.Time, len(names))
	failures := make(map[string]int, len(names))
	start := now()
	for _, name := range names {
		due[name] = start
	}
	for {
		for _, name := range names {
			if ctx.Err() != nil {
				return nil
			}
			if due[name].After(now()) {
				continue
			}
			syncCtx, cancel := context.WithTimeout(ctx, timeout)
			err := m.Syncs[name].Sync(syncCtx)
			cancel()
			if ctx.Err() != nil {
				return nil
			}
			if err == nil {
				failures[name] = 0
				due[name] = now().Add(webhookSyncInterval)
				continue
			}
			failures[name]++
			index := failures[name] - 1
			if index >= len(webhookRetryDelays) {
				index = len(webhookRetryDelays) - 1
			}
			due[name] = now().Add(webhookRetryDelays[index])
			attrs := []slog.Attr{slog.String("instance", name)}
			var syncErr *WebhookSyncError
			if errors.As(err, &syncErr) {
				attrs = append(attrs, slog.String("reason", syncErr.Reason), slog.Int("status", syncErr.StatusCode))
			} else {
				// A local failure; the emitter's redactor cleans the error.
				attrs = append(attrs, slog.String("reason", "internal"), slog.Int("status", 0))
				attrs = append(attrs, events.ErrorAttrs("webhook_connection_sync", err)...)
			}
			events.Log(ctx, slog.LevelWarn, "webhook.connection_sync_failed", "webhook connection sync failed", attrs...)
		}
		earliest := due[names[0]]
		for _, name := range names[1:] {
			if due[name].Before(earliest) {
				earliest = due[name]
			}
		}
		wait := earliest.Sub(now())
		if wait < 0 {
			wait = 0
		}
		select {
		case <-ctx.Done():
			return nil
		case <-after(wait):
		}
	}
}
