package catalog

import (
	"context"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"subsyncd/internal/domain"
	"subsyncd/internal/store"
)

func newTestInbox(t *testing.T, eventStore *fakeEventStore, catalog *fakeEventCatalog, now *time.Time) (*WebhookInbox, *store.Repository) {
	t.Helper()
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "subsyncd.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	repository := db.Repository()
	clock := func() time.Time { return *now }
	handler := WebhookHandler{Instance: "main", InstanceType: "sonarr", Catalog: catalog, Store: eventStore, Languages: []domain.Language{"en"}, Now: clock}
	return &WebhookInbox{Store: repository, Handlers: map[string]WebhookHandler{"main": handler}, Now: clock}, repository
}

func TestWebhookInboxAnswersBeforeApplying(t *testing.T) {
	now := time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)
	eventStore := &fakeEventStore{}
	catalog := &fakeEventCatalog{media: domain.Media{EntityID: 101, Ref: domain.MediaRef{Instance: "main", Kind: domain.MediaEpisode, FileID: 1001}, Title: "Show"}}
	inbox, repository := newTestInbox(t, eventStore, catalog, &now)
	acceptor := inbox.Handler("main")

	for _, body := range []string{`{"eventType":"Grab"}`, `{"eventType":"Health"}`, `{"eventType":"SeriesAdd","series":{"id":1}}`} {
		if _, err := acceptor.Handle(t.Context(), []byte(body)); !errors.Is(err, ErrIgnoredEvent) {
			t.Fatalf("%s: error = %v, want ignored (HTTP 204)", body, err)
		}
	}
	if _, err := acceptor.Handle(t.Context(), []byte(`{"eventType":"Download"}`)); !errors.Is(err, ErrInvalidWebhook) {
		t.Fatalf("invalid webhook error = %v", err)
	}
	result, err := acceptor.Handle(t.Context(), []byte(`{"eventType":"Download","episodeFile":{"id":1001}}`))
	if err != nil || !result.Queued || result.EventCount != 1 {
		t.Fatalf("result = %+v/%v", result, err)
	}
	if len(eventStore.mutations) != 0 || catalog.calls != 0 {
		t.Fatal("webhook was applied before the response")
	}
	received := now
	now = now.Add(time.Minute)
	if err := inbox.Drain(t.Context(), "main"); err != nil {
		t.Fatal(err)
	}
	if len(eventStore.mutations) != 1 || !eventStore.mutations[0].At.Equal(received) {
		t.Fatalf("mutations = %+v, want one dated at receipt", eventStore.mutations)
	}
	if items, err := repository.DueWebhooks(t.Context(), "main", now.Add(time.Hour), 10); err != nil || len(items) != 0 {
		t.Fatalf("applied webhook still stored: %+v/%v", items, err)
	}
}

func TestWebhookInboxRetriesFailuresInOrder(t *testing.T) {
	now := time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)
	eventStore := &fakeEventStore{}
	catalog := &fakeEventCatalog{media: domain.Media{EntityID: 101, Ref: domain.MediaRef{Instance: "main", Kind: domain.MediaEpisode, FileID: 1001}, Title: "Show"}, err: errors.New("sonarr unavailable")}
	inbox, repository := newTestInbox(t, eventStore, catalog, &now)
	acceptor := inbox.Handler("main")
	for _, body := range []string{`{"eventType":"Download","episodeFile":{"id":1001}}`, `{"eventType":"EpisodeFileDelete","episodeFile":{"id":1001}}`} {
		if _, err := acceptor.Handle(t.Context(), []byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := inbox.Drain(t.Context(), "main"); err != nil {
		t.Fatal(err)
	}
	// The import failed and waits; the delete behind it still applied.
	if len(eventStore.mutations) != 1 || eventStore.mutations[0].Type != string(EventDelete) {
		t.Fatalf("mutations = %+v", eventStore.mutations)
	}
	next, found, err := repository.NextWebhookAt(t.Context(), "main")
	if err != nil || !found || !next.After(now) {
		t.Fatalf("failed webhook was not kept for retry: %v/%t/%v", next, found, err)
	}
	catalog.err = nil
	now = next
	if err := inbox.Drain(t.Context(), "main"); err != nil {
		t.Fatal(err)
	}
	if len(eventStore.mutations) != 2 || eventStore.mutations[1].Type != string(EventImport) {
		t.Fatalf("retried mutations = %+v", eventStore.mutations)
	}
	if _, found, err := repository.NextWebhookAt(t.Context(), "main"); err != nil || found {
		t.Fatalf("retried webhook still stored: %t/%v", found, err)
	}
}

func TestWebhookInboxRunAppliesStoredWebhooksAtStart(t *testing.T) {
	now := time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)
	eventStore := &fakeEventStore{}
	catalog := &fakeEventCatalog{media: domain.Media{EntityID: 101, Ref: domain.MediaRef{Instance: "main", Kind: domain.MediaEpisode, FileID: 1001}, Title: "Show"}}
	inbox, repository := newTestInbox(t, eventStore, catalog, &now)
	// Accepted by a process that stopped before applying it.
	if _, err := repository.EnqueueWebhook(t.Context(), "main", []byte(`{"eventType":"Download","episodeFile":{"id":1001}}`), now); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); inbox.Run(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, found, err := repository.NextWebhookAt(t.Context(), "main"); err == nil && !found {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("stored webhook was not applied at start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
}

// failingDeleteInbox is a readable inbox whose writes fail, as on a full
// disk or a database that went read-only.
type failingDeleteInbox struct {
	WebhookInboxStore
	deletes atomic.Int64
}

func (s *failingDeleteInbox) DeleteWebhook(context.Context, int64) error {
	s.deletes.Add(1)
	return errors.New("attempt to write a readonly database")
}

func TestWebhookInboxBacksOffWhenTheInboxFails(t *testing.T) {
	previousDelay, previousMax := webhookInboxErrorDelay, webhookInboxMaxErrorDelay
	webhookInboxErrorDelay, webhookInboxMaxErrorDelay = 40*time.Millisecond, time.Second
	t.Cleanup(func() { webhookInboxErrorDelay, webhookInboxMaxErrorDelay = previousDelay, previousMax })

	now := time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)
	catalog := &fakeEventCatalog{media: domain.Media{EntityID: 101, Ref: domain.MediaRef{Instance: "main", Kind: domain.MediaEpisode, FileID: 1001}, Title: "Show"}}
	inbox, repository := newTestInbox(t, &fakeEventStore{}, catalog, &now)
	if _, err := repository.EnqueueWebhook(t.Context(), "main", []byte(`{"eventType":"Download","episodeFile":{"id":1001}}`), now); err != nil {
		t.Fatal(err)
	}
	failing := &failingDeleteInbox{WebhookInboxStore: repository}
	inbox.Store = failing
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); inbox.Run(ctx) }()
	time.Sleep(300 * time.Millisecond)
	cancel()
	<-done
	// 40, 80 and 160 ms waits fit in 300 ms: at most four passes, where
	// retrying at once would call Arr thousands of times.
	if attempts := failing.deletes.Load(); attempts < 2 || attempts > 4 {
		t.Fatalf("passes over the failing inbox = %d, want 2-4 with a growing wait", attempts)
	}
}

func TestWebhookInboxErrorDelayDoublesToItsCap(t *testing.T) {
	var delays []time.Duration
	delay := time.Duration(0)
	for range 9 {
		delay = nextWebhookErrorDelay(delay)
		delays = append(delays, delay)
	}
	want := []time.Duration{5 * time.Second, 10 * time.Second, 20 * time.Second, 40 * time.Second, 80 * time.Second, 160 * time.Second, 5 * time.Minute, 5 * time.Minute, 5 * time.Minute}
	for index := range want {
		if delays[index] != want[index] {
			t.Fatalf("delays = %v, want %v", delays, want)
		}
	}
}
