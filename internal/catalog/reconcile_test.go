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
	through time.Time
}

func (f *fakeReconcileCatalog) GetMedia(context.Context, domain.MediaRef) (domain.Media, error) {
	return domain.Media{}, errors.New("not used")
}

func (f *fakeReconcileCatalog) ListChanges(_ context.Context, since, through time.Time) ([]HistoryChange, error) {
	f.since = since
	f.through = through
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
	catalog := &fakeReconcileCatalog{changes: []HistoryChange{{HistoryID: 1, EntityID: 101, Kind: domain.MediaEpisode, Type: EventImport, State: HistoryPresent, Media: domain.Media{EntityID: 101, Ref: domain.MediaRef{Instance: "sonarr-main", Kind: domain.MediaEpisode, FileID: 1}}, OccurredAt: start}}}
	store := &fakeReconcileStore{cursor: start, commitErr: errors.New("disk full")}
	reconciler := Reconciler{Instance: "sonarr-main", Catalog: catalog, Store: store, Languages: []domain.Language{"hr"}, Now: func() time.Time { return start.Add(time.Hour) }}

	if err := reconciler.Run(context.Background()); err == nil {
		t.Fatal("expected commit error")
	}
	if !store.committed.IsZero() {
		t.Fatalf("cursor advanced to %s after failed commit", store.committed)
	}
}

func TestReconcilerConvertsEntityHistoryStates(t *testing.T) {
	start := time.Date(2026, 9, 4, 10, 0, 0, 0, time.UTC)
	pageEnd := start.Add(time.Hour)
	media := domain.Media{EntityID: 101, Ref: domain.MediaRef{Instance: "sonarr-main", Kind: domain.MediaEpisode, FileID: 7}, Title: "Episode"}
	changes := []HistoryChange{
		{HistoryID: 41, EntityID: 101, Kind: domain.MediaEpisode, Type: EventImport, State: HistoryPresent, Media: media, OccurredAt: start.Add(10 * time.Minute)},
		{HistoryID: 42, EntityID: 102, Kind: domain.MediaEpisode, Type: EventDelete, State: HistoryAbsent, OccurredAt: start.Add(20 * time.Minute)},
		{HistoryID: 43, EntityID: 103, Kind: domain.MediaEpisode, Type: EventDelete, State: HistoryOutsideScope, OccurredAt: start.Add(30 * time.Minute)},
	}
	catalog := &fakeReconcileCatalog{changes: changes}
	backend := &fakeReconcileStore{cursor: start}
	reconciler := Reconciler{Instance: "sonarr-main", Catalog: catalog, Store: backend, Languages: []domain.Language{"hr", "en"}, Now: func() time.Time { return pageEnd }}
	if err := reconciler.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !catalog.since.Equal(start) || !catalog.through.Equal(pageEnd) || !backend.committed.Equal(pageEnd) {
		t.Fatalf("since/through/committed = %s/%s/%s", catalog.since, catalog.through, backend.committed)
	}
	if len(backend.mutations) != 3 {
		t.Fatalf("mutations = %#v", backend.mutations)
	}
	first, second := backend.mutations[0], backend.mutations[1]
	if first.EventID != "reconcile:sonarr-main:41" || first.Type != "import" || first.Ref != media.Ref || first.Media.Ref != media.Ref || !first.At.Equal(changes[0].OccurredAt) || first.Priority != store.SearchPriorityMissing || len(first.Languages) != 2 {
		t.Fatalf("first mutation = %#v", first)
	}
	if second.EventID != "reconcile:sonarr-main:42" || second.Type != "delete" || second.EntityID != 102 || second.Ref.Kind != domain.MediaEpisode || second.Ref.FileID != 0 || second.Media.Ref.FileID != 0 || !second.At.Equal(changes[1].OccurredAt) || second.Priority != store.SearchPriorityMissing {
		t.Fatalf("second mutation = %#v", second)
	}
	third := backend.mutations[2]
	if third.EventID != "reconcile:sonarr-main:43" || third.Type != "delete" || third.EntityID != 103 || third.Ref.FileID != 0 {
		t.Fatalf("third mutation = %#v", third)
	}
}

func TestReconcilerRejectsInvalidEntityHistoryStateBeforeCommit(t *testing.T) {
	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	catalog := &fakeReconcileCatalog{changes: []HistoryChange{{HistoryID: 44, EntityID: 104, Kind: domain.MediaEpisode, Type: EventDelete, State: HistoryState("uncertain"), OccurredAt: now}}}
	backend := &fakeReconcileStore{}
	wakes := 0
	reconciler := Reconciler{Instance: "sonarr-main", Catalog: catalog, Store: backend, Now: func() time.Time { return now }, OnCommitted: func() { wakes++ }}
	if err := reconciler.Run(context.Background()); err == nil {
		t.Fatal("Reconciler.Run() error = nil")
	}
	if !backend.committed.IsZero() || len(backend.mutations) != 0 || wakes != 0 {
		t.Fatalf("invalid state committed = %s/%#v, wakes=%d", backend.committed, backend.mutations, wakes)
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
