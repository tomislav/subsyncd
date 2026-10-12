package catalog

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"subsyncd/internal/observability"
)

type fakeSyncer struct {
	mu    sync.Mutex
	calls int
	fn    func(ctx context.Context, call int) error
}

func (f *fakeSyncer) Sync(ctx context.Context) error {
	f.mu.Lock()
	f.calls++
	n := f.calls
	f.mu.Unlock()
	if f.fn == nil {
		return nil
	}
	return f.fn(ctx, n)
}

func (f *fakeSyncer) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// afterRecorder hands out controllable timer channels and records requested delays.
type afterRecorder struct {
	delays chan time.Duration
	fire   chan chan time.Time
}

func newAfterRecorder() *afterRecorder {
	return &afterRecorder{delays: make(chan time.Duration, 16), fire: make(chan chan time.Time, 16)}
}

func (a *afterRecorder) After(d time.Duration) <-chan time.Time {
	ch := make(chan time.Time, 1)
	a.delays <- d
	a.fire <- ch
	return ch
}

func (a *afterRecorder) nextDelay(t *testing.T) time.Duration {
	t.Helper()
	select {
	case d := <-a.delays:
		return d
	case <-time.After(2 * time.Second):
		t.Fatal("maintainer did not wait")
		return 0
	}
}

// advance fires the most recent timer after moving the fake clock.
func (a *afterRecorder) advance(t *testing.T, clock *fakeMaintainerClock, d time.Duration) {
	t.Helper()
	clock.add(d)
	select {
	case ch := <-a.fire:
		ch <- clock.Now()
	case <-time.After(2 * time.Second):
		t.Fatal("no timer to fire")
	}
}

type fakeMaintainerClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeMaintainerClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *fakeMaintainerClock) add(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

func startMaintainer(t *testing.T, m *WebhookConnectionMaintainer) (cancel func(), done chan error) {
	t.Helper()
	ctx, cancelCtx := context.WithCancel(context.Background())
	done = make(chan error, 1)
	go func() { done <- m.Run(ctx) }()
	t.Cleanup(cancelCtx)
	return cancelCtx, done
}

func waitCalls(t *testing.T, f *fakeSyncer, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for f.count() < want {
		if time.Now().After(deadline) {
			t.Fatalf("syncer calls = %d, want %d", f.count(), want)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestMaintainerSyncsAllAtStart(t *testing.T) {
	a, b := &fakeSyncer{}, &fakeSyncer{}
	after := newAfterRecorder()
	clock := &fakeMaintainerClock{now: time.Unix(1000, 0)}
	m := &WebhookConnectionMaintainer{Syncs: map[string]webhookSyncer{"b": b, "a": a}, After: after.After, Now: clock.Now}
	startMaintainer(t, m)
	if d := after.nextDelay(t); d != 6*time.Hour {
		t.Fatalf("first delay = %s, want 6h", d)
	}
	if a.count() != 1 || b.count() != 1 {
		t.Fatalf("calls a=%d b=%d, want 1 each", a.count(), b.count())
	}
	after.advance(t, clock, 6*time.Hour)
	after.nextDelay(t)
	if a.count() != 2 || b.count() != 2 {
		t.Fatalf("calls after 6h a=%d b=%d, want 2 each", a.count(), b.count())
	}
}

func TestMaintainerBackoffSequence(t *testing.T) {
	var logs bytes.Buffer
	events, err := observability.New(&logs, observability.Options{Level: "info", Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	failing := &fakeSyncer{fn: func(context.Context, int) error {
		return &WebhookSyncError{Reason: "arr_unreachable"}
	}}
	after := newAfterRecorder()
	clock := &fakeMaintainerClock{now: time.Unix(1000, 0)}
	m := &WebhookConnectionMaintainer{Syncs: map[string]webhookSyncer{"tv": failing}, Events: events, After: after.After, Now: clock.Now}
	startMaintainer(t, m)
	for i, want := range []time.Duration{15 * time.Minute, time.Hour, 6 * time.Hour, 6 * time.Hour} {
		if d := after.nextDelay(t); d != want {
			t.Fatalf("delay %d = %s, want %s", i, d, want)
		}
		after.advance(t, clock, want)
	}
	out := logs.String()
	if !strings.Contains(out, `"event":"webhook.connection_sync_failed"`) || !strings.Contains(out, `"reason":"arr_unreachable"`) || !strings.Contains(out, `"instance":"tv"`) {
		t.Fatalf("missing sync_failed log: %s", out)
	}
}

func TestMaintainerLogsInternalForPlainErrors(t *testing.T) {
	const secret = "token-SENTINEL"
	var logs bytes.Buffer
	events, _ := observability.New(&logs, observability.Options{Level: "info", Version: "test", Redact: func(err error) string {
		return strings.ReplaceAll(err.Error(), secret, "[redacted]")
	}})
	failing := &fakeSyncer{fn: func(context.Context, int) error { return errors.New("database is locked " + secret) }}
	after := newAfterRecorder()
	clock := &fakeMaintainerClock{now: time.Unix(1000, 0)}
	m := &WebhookConnectionMaintainer{Syncs: map[string]webhookSyncer{"tv": failing}, Events: events, After: after.After, Now: clock.Now}
	startMaintainer(t, m)
	after.nextDelay(t)
	out := logs.String()
	for _, want := range []string{`"reason":"internal"`, `"error_kind":"webhook_connection_sync"`, `"error":"database is locked [redacted]"`} {
		if !strings.Contains(out, want) {
			t.Fatalf("log lacks %s: %s", want, out)
		}
	}
	if strings.Contains(out, secret) {
		t.Fatalf("log leaks the secret: %s", out)
	}
}

func TestMaintainerOmitsErrorForSyncErrors(t *testing.T) {
	var logs bytes.Buffer
	events, _ := observability.New(&logs, observability.Options{Level: "info", Version: "test"})
	failing := &fakeSyncer{fn: func(context.Context, int) error { return &WebhookSyncError{Reason: "arr_status", StatusCode: 401} }}
	after := newAfterRecorder()
	clock := &fakeMaintainerClock{now: time.Unix(1000, 0)}
	m := &WebhookConnectionMaintainer{Syncs: map[string]webhookSyncer{"tv": failing}, Events: events, After: after.After, Now: clock.Now}
	startMaintainer(t, m)
	after.nextDelay(t)
	if out := logs.String(); !strings.Contains(out, `"reason":"arr_status"`) || strings.Contains(out, "error_kind") {
		t.Fatalf("log = %s", out)
	}
}

func TestMaintainerSuccessResetsBackoff(t *testing.T) {
	s := &fakeSyncer{fn: func(_ context.Context, call int) error {
		if call == 1 || call == 3 {
			return &WebhookSyncError{Reason: "arr_status", StatusCode: 500}
		}
		return nil
	}}
	after := newAfterRecorder()
	clock := &fakeMaintainerClock{now: time.Unix(1000, 0)}
	m := &WebhookConnectionMaintainer{Syncs: map[string]webhookSyncer{"tv": s}, After: after.After, Now: clock.Now}
	startMaintainer(t, m)
	for i, want := range []time.Duration{15 * time.Minute, 6 * time.Hour, 15 * time.Minute} {
		if d := after.nextDelay(t); d != want {
			t.Fatalf("delay %d = %s, want %s", i, d, want)
		}
		after.advance(t, clock, want)
	}
}

func TestMaintainerTimeoutBoundsSync(t *testing.T) {
	s := &fakeSyncer{fn: func(ctx context.Context, call int) error {
		if call == 1 {
			<-ctx.Done()
			return ctx.Err()
		}
		return nil
	}}
	after := newAfterRecorder()
	clock := &fakeMaintainerClock{now: time.Unix(1000, 0)}
	m := &WebhookConnectionMaintainer{Syncs: map[string]webhookSyncer{"tv": s}, After: after.After, Now: clock.Now, Timeout: 50 * time.Millisecond}
	startMaintainer(t, m)
	if d := after.nextDelay(t); d != 15*time.Minute {
		t.Fatalf("delay after timeout = %s, want 15m", d)
	}
	after.advance(t, clock, 15*time.Minute)
	if d := after.nextDelay(t); d != 6*time.Hour {
		t.Fatalf("delay after recovery = %s, want 6h", d)
	}
}

func TestMaintainerStopsOnCancel(t *testing.T) {
	for _, blocked := range []bool{false, true} {
		s := &fakeSyncer{}
		if blocked {
			s.fn = func(ctx context.Context, _ int) error { <-ctx.Done(); return ctx.Err() }
		}
		after := newAfterRecorder()
		clock := &fakeMaintainerClock{now: time.Unix(1000, 0)}
		var logs bytes.Buffer
		events, _ := observability.New(&logs, observability.Options{Level: "debug", Version: "test"})
		m := &WebhookConnectionMaintainer{Syncs: map[string]webhookSyncer{"tv": s}, Events: events, After: after.After, Now: clock.Now}
		cancel, done := startMaintainer(t, m)
		if blocked {
			waitCalls(t, s, 1)
		} else {
			after.nextDelay(t)
		}
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("Run() = %v, want nil", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("Run did not stop")
		}
		if strings.Contains(logs.String(), "sync_failed") {
			t.Fatalf("cancellation logged as failure: %s", logs.String())
		}
	}
}
