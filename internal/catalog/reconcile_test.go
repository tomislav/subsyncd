package catalog

import (
	"context"
	"errors"
	"testing"
	"time"

	"subsyncd/internal/domain"
)

type fakeReconcileCatalog struct {
	media []domain.Media
	err   error
	since time.Time
}

func (f *fakeReconcileCatalog) GetMedia(context.Context, domain.MediaRef) (domain.Media, error) {
	return domain.Media{}, errors.New("not used")
}

func (f *fakeReconcileCatalog) ListMediaChangedSince(_ context.Context, since time.Time) ([]domain.Media, error) {
	f.since = since
	return f.media, f.err
}

type fakeReconcileStore struct {
	cursor    time.Time
	committed time.Time
	mutations []domain.Media
	commitErr error
}

func (f *fakeReconcileStore) GetReconciliationCursor(context.Context, string) (time.Time, error) {
	return f.cursor, nil
}

func (f *fakeReconcileStore) CommitReconciliation(context.Context, string, time.Time, []domain.Media, []domain.Language) error {
	if f.commitErr != nil {
		return f.commitErr
	}
	f.committed = time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	return nil
}

func TestReconcilerDoesNotAdvanceCursorWhenPageCommitFails(t *testing.T) {
	start := time.Date(2026, 9, 4, 10, 0, 0, 0, time.UTC)
	catalog := &fakeReconcileCatalog{media: []domain.Media{{Title: "Episode"}}}
	store := &fakeReconcileStore{cursor: start, commitErr: errors.New("disk full")}
	reconciler := Reconciler{Instance: "sonarr-main", Catalog: catalog, Store: store, Languages: []domain.Language{"hr"}, Now: func() time.Time { return start.Add(time.Hour) }}

	if err := reconciler.Run(context.Background()); err == nil {
		t.Fatal("expected commit error")
	}
	if !store.committed.IsZero() {
		t.Fatalf("cursor advanced to %s after failed commit", store.committed)
	}
}
