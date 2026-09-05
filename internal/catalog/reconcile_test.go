package catalog

import (
	"context"
	"errors"
	"testing"
	"time"

	"subsyncd/internal/domain"
	"subsyncd/internal/store"
)

type fakeReconcileCatalog struct {
	changes []HistoryChange
	err     error
	since   time.Time
}

func (f *fakeReconcileCatalog) GetMedia(context.Context, domain.MediaRef) (domain.Media, error) {
	return domain.Media{}, errors.New("not used")
}

func (f *fakeReconcileCatalog) ListChangesSince(_ context.Context, since time.Time) ([]HistoryChange, error) {
	f.since = since
	return f.changes, f.err
}

type fakeReconcileStore struct {
	cursor    time.Time
	committed time.Time
	mutations []store.MediaEventMutation
	commitErr error
}

func (f *fakeReconcileStore) GetReconciliationCursor(context.Context, string) (time.Time, error) {
	return f.cursor, nil
}

func (f *fakeReconcileStore) CommitReconciliation(_ context.Context, _ string, cursor time.Time, mutations []store.MediaEventMutation) error {
	if f.commitErr != nil {
		return f.commitErr
	}
	f.committed = cursor
	f.mutations = append([]store.MediaEventMutation(nil), mutations...)
	return nil
}

func TestReconcilerDoesNotAdvanceCursorWhenPageCommitFails(t *testing.T) {
	start := time.Date(2026, 9, 4, 10, 0, 0, 0, time.UTC)
	catalog := &fakeReconcileCatalog{changes: []HistoryChange{{HistoryID: 1, Type: EventImport, Ref: domain.MediaRef{Instance: "sonarr-main", Kind: domain.MediaEpisode, FileID: 1}, OccurredAt: start}}}
	store := &fakeReconcileStore{cursor: start, commitErr: errors.New("disk full")}
	reconciler := Reconciler{Instance: "sonarr-main", Catalog: catalog, Store: store, Languages: []domain.Language{"hr"}, Now: func() time.Time { return start.Add(time.Hour) }}

	if err := reconciler.Run(context.Background()); err == nil {
		t.Fatal("expected commit error")
	}
	if !store.committed.IsZero() {
		t.Fatalf("cursor advanced to %s after failed commit", store.committed)
	}
}

func TestReconcilerConvertsHistoryChanges(t *testing.T) {
	start := time.Date(2026, 9, 4, 10, 0, 0, 0, time.UTC)
	pageEnd := start.Add(time.Hour)
	media := domain.Media{EntityID: 101, Ref: domain.MediaRef{Instance: "sonarr-main", Kind: domain.MediaEpisode, FileID: 7}, Title: "Episode"}
	changes := []HistoryChange{
		{HistoryID: 41, Type: EventImport, Ref: media.Ref, Media: media, OccurredAt: start.Add(10 * time.Minute)},
		{HistoryID: 42, Type: EventDelete, Ref: domain.MediaRef{Instance: "sonarr-main", Kind: domain.MediaEpisode, FileID: 8}, OccurredAt: start.Add(20 * time.Minute)},
	}
	catalog := &fakeReconcileCatalog{changes: changes}
	backend := &fakeReconcileStore{cursor: start}
	reconciler := Reconciler{Instance: "sonarr-main", Catalog: catalog, Store: backend, Languages: []domain.Language{"hr", "en"}, Now: func() time.Time { return pageEnd }}
	if err := reconciler.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !catalog.since.Equal(start) || !backend.committed.Equal(pageEnd) {
		t.Fatalf("since/committed = %s/%s", catalog.since, backend.committed)
	}
	if len(backend.mutations) != 2 {
		t.Fatalf("mutations = %#v", backend.mutations)
	}
	first, second := backend.mutations[0], backend.mutations[1]
	if first.EventID != "reconcile:sonarr-main:41" || first.Type != "import" || first.Ref != media.Ref || first.Media.Ref != media.Ref || !first.At.Equal(changes[0].OccurredAt) || first.Priority != store.SearchPriorityMissing || len(first.Languages) != 2 {
		t.Fatalf("first mutation = %#v", first)
	}
	if second.EventID != "reconcile:sonarr-main:42" || second.Type != "delete" || second.Ref.FileID != 8 || second.Media.Ref.FileID != 0 || !second.At.Equal(changes[1].OccurredAt) || second.Priority != store.SearchPriorityMissing {
		t.Fatalf("second mutation = %#v", second)
	}
}

func TestReconcilerCallsOnCommittedOnlyAfterSuccessfulCommit(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	wakes := 0
	reconciler := Reconciler{Instance: "sonarr-main", Catalog: &fakeReconcileCatalog{}, Store: &fakeReconcileStore{}, Languages: []domain.Language{"hr"}, Now: func() time.Time { return now }, OnCommitted: func() { wakes++ }}
	if err := reconciler.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if wakes != 1 {
		t.Fatalf("successful reconciliation callbacks = %d, want 1", wakes)
	}
	reconciler.Store = &fakeReconcileStore{commitErr: errors.New("disk full")}
	if err := reconciler.Run(context.Background()); err == nil {
		t.Fatal("failed reconciliation error = nil")
	}
	if wakes != 1 {
		t.Fatalf("failed reconciliation changed callbacks to %d", wakes)
	}
}
