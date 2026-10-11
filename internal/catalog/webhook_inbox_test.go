package catalog

import (
	"context"
	"errors"
	"path/filepath"
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
