package catalog

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"subsyncd/internal/observability"
	"subsyncd/internal/schedule"
	"subsyncd/internal/store"
)

// WebhookInboxStore durably holds accepted webhooks until they are applied.
type WebhookInboxStore interface {
	EnqueueWebhook(context.Context, string, []byte, time.Time) (int64, error)
	DueWebhooks(context.Context, string, time.Time, int) ([]store.WebhookInboxItem, error)
	NextWebhookAt(context.Context, string) (time.Time, bool, error)
	DeleteWebhook(context.Context, int64) error
	RetryWebhook(context.Context, int64, time.Time) error
}

const (
	webhookInboxBatch = 20
	// webhookInboxPoll bounds how long a processor sleeps without a wake, so
	// work committed by an earlier process is never stranded.
	webhookInboxPoll = time.Minute
	// maxWebhookAttempts abandons a webhook that keeps failing: the first
	// attempt and six retries, 1m, 5m, 15m, 1h, 6h and 24h apart
	// (schedule.WorkflowFailureDelay), about 31 hours in all. Reconciliation
	// from Arr history then covers its change.
	maxWebhookAttempts = 7
)

// A processor whose Drain fails (the inbox cannot be read or written) waits
// before trying again, from webhookInboxErrorDelay doubling up to
// webhookInboxMaxErrorDelay, instead of re-running the same webhooks against
// Arr at once. A Drain that succeeds resets the delay. Variables for tests.
var (
	webhookInboxErrorDelay    = 5 * time.Second
	webhookInboxMaxErrorDelay = 5 * time.Minute
)

// nextWebhookErrorDelay is the wait after another failed Drain.
func nextWebhookErrorDelay(previous time.Duration) time.Duration {
	return min(max(2*previous, webhookInboxErrorDelay), webhookInboxMaxErrorDelay)
}

// WebhookInbox answers Arr webhooks as soon as they are durably stored and
// applies them in the background: one processor per instance applies that
// instance's webhooks in arrival order, so a media item's changes keep their
// order and concurrency is bounded by the number of instances. A webhook that
// fails is retried with backoff; one interrupted by shutdown is applied at
// the next start.
type WebhookInbox struct {
	Store    WebhookInboxStore
	Handlers map[string]WebhookHandler
	Now      func() time.Time
	Events   *observability.Emitter

	once  sync.Once
	wakes map[string]chan struct{}
}

func (b *WebhookInbox) init() {
	b.once.Do(func() {
		b.wakes = make(map[string]chan struct{}, len(b.Handlers))
		for name := range b.Handlers {
			b.wakes[name] = make(chan struct{}, 1)
		}
	})
}

// Handler returns the HTTP-facing handler for one instance.
func (b *WebhookInbox) Handler(instance string) WebhookAcceptor {
	b.init()
	return WebhookAcceptor{inbox: b, instance: instance}
}

// WebhookAcceptor validates a webhook and stores it for the inbox.
type WebhookAcceptor struct {
	inbox    *WebhookInbox
	instance string
}

// Handle rejects malformed payloads and ignores irrelevant events at once,
// exactly as applying them would; anything else is stored and queued.
func (a WebhookAcceptor) Handle(ctx context.Context, body []byte) (WebhookResult, error) {
	handler, ok := a.inbox.Handlers[a.instance]
	if !ok {
		return WebhookResult{}, errors.New("webhook instance is not configured")
	}
	events, err := NormalizeWebhook(handler.Instance, handler.InstanceType, body)
	if err != nil {
		return WebhookResult{}, err
	}
	if _, err := a.inbox.Store.EnqueueWebhook(ctx, a.instance, body, a.inbox.Now().UTC()); err != nil {
		return WebhookResult{EventCount: len(events)}, err
	}
	select {
	case a.inbox.wakes[a.instance] <- struct{}{}:
	default:
	}
	return WebhookResult{EventCount: len(events), Queued: true}, nil
}

// Run applies stored webhooks until ctx ends.
func (b *WebhookInbox) Run(ctx context.Context) {
	b.init()
	var wait sync.WaitGroup
	for name := range b.Handlers {
		wait.Add(1)
		go func(name string) {
			defer wait.Done()
			b.process(ctx, name)
		}(name)
	}
	wait.Wait()
}

func (b *WebhookInbox) process(ctx context.Context, instance string) {
	events := b.Events.For("http")
	var errorDelay time.Duration
	for ctx.Err() == nil {
		err := b.Drain(ctx, instance)
		if ctx.Err() != nil {
			return
		}
		delay := webhookInboxPoll
		wake := b.wakes[instance]
		if err != nil {
			errorDelay = nextWebhookErrorDelay(errorDelay)
			delay = errorDelay
			// A new webhook does not cut the wait short: the failure is the
			// inbox's, not the webhook's.
			wake = nil
			events.Log(ctx, slog.LevelError, "webhook.failed", "webhook inbox unavailable",
				append([]slog.Attr{slog.String("instance", instance), slog.String("outcome", "inbox_unavailable"), slog.Time("retry_at", b.Now().Add(delay).UTC())}, events.ErrorAttrs("webhook_inbox", err)...)...)
		} else {
			errorDelay = 0
			if next, found, err := b.Store.NextWebhookAt(ctx, instance); err == nil && found {
				delay = min(max(next.Sub(b.Now()), 0), webhookInboxPoll)
			}
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
		case <-wake:
		case <-timer.C:
		}
		timer.Stop()
	}
}

// Drain applies every webhook of instance that is due now, in arrival order.
func (b *WebhookInbox) Drain(ctx context.Context, instance string) error {
	handler := b.Handlers[instance]
	events := b.Events.For("http")
	for {
		items, err := b.Store.DueWebhooks(ctx, instance, b.Now(), webhookInboxBatch)
		if err != nil || len(items) == 0 {
			return err
		}
		for _, item := range items {
			if err := ctx.Err(); err != nil {
				return err
			}
			started := time.Now()
			result, err := handler.HandleAt(ctx, item.Body, item.ReceivedAt)
			if ctx.Err() != nil {
				// Shutdown: leave it for the next start.
				return ctx.Err()
			}
			attrs := []slog.Attr{slog.String("instance", instance), slog.Int("event_count", result.EventCount), slog.Int("applied_count", result.AppliedCount), slog.Int("attempt", item.Attempt+1), slog.Int64("duration_ms", time.Since(started).Milliseconds())}
			switch {
			case err == nil || errors.Is(err, ErrIgnoredEvent):
				outcome := "applied"
				if errors.Is(err, ErrIgnoredEvent) {
					outcome = "ignored"
				} else if result.AppliedCount == 0 {
					outcome = "duplicate"
				}
				events.Log(ctx, slog.LevelInfo, "webhook.applied", "webhook processing completed", append(attrs, slog.String("outcome", outcome))...)
			case errors.Is(err, ErrInvalidWebhook) || item.Attempt+1 >= maxWebhookAttempts:
				events.Log(ctx, slog.LevelError, "webhook.failed", "webhook abandoned; reconciliation will pick up its change",
					append(append(attrs, slog.String("outcome", "abandoned")), events.ErrorAttrs("webhook_processing", err)...)...)
			default:
				retryAt := b.Now().Add(schedule.WorkflowFailureDelay(item.Attempt))
				if retryErr := b.Store.RetryWebhook(ctx, item.ID, retryAt); retryErr != nil {
					return retryErr
				}
				events.Log(ctx, slog.LevelWarn, "webhook.failed", "webhook processing failed; retry scheduled",
					append(append(attrs, slog.String("outcome", "retry_scheduled"), slog.Time("retry_at", retryAt)), events.ErrorAttrs("webhook_processing", err)...)...)
				continue
			}
			if err := b.Store.DeleteWebhook(ctx, item.ID); err != nil {
				return err
			}
		}
	}
}
