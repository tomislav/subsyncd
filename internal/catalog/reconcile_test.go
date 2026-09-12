package catalog

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"subsyncd/internal/domain"
	"subsyncd/internal/store"
)

type fakeReconcileCatalog struct {
	changes        []HistoryChange
	err            error
	since          time.Time
	through        time.Time
	afterHydration func()
}

func (f *fakeReconcileCatalog) GetMedia(context.Context, domain.MediaRef) (domain.Media, error) {
	return domain.Media{}, errors.New("not used")
}

func (f *fakeReconcileCatalog) ListChanges(_ context.Context, since, through time.Time) ([]HistoryChange, error) {
	f.since = since
	f.through = through
	if f.afterHydration != nil {
		f.afterHydration()
	}
	return f.changes, f.err
}

type fakeReconcileStore struct {
	cursor    time.Time
	committed time.Time
	mutations []store.MediaEventMutation
	commitErr error
}

func (f *fakeReconcileStore) GetReconciliationState(context.Context, string) (store.ReconciliationState, error) {
	return store.ReconciliationState{Cursor: f.cursor}, nil
}

func (f *fakeReconcileStore) CommitReconciliation(_ context.Context, _ string, _ store.ReconciliationState, cursor time.Time, mutations []store.MediaEventMutation) error {
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

// A webhook committed after history hydration must win over the stale page.
// Removing the reconciliation fence resurrects deletions and restores old files.
func TestReconcilerRejectsHistoryHydratedBeforeConcurrentWebhook(t *testing.T) {
	for _, event := range []string{"movie_delete", "series_delete", "replacement", "rename"} {
		t.Run(event, func(t *testing.T) {
			ctx := context.Background()
			now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
			db, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = db.Close() })
			repo := db.Repository()
			instance, instanceType, kind := "sonarr-main", "sonarr", domain.MediaEpisode
			if event == "movie_delete" {
				instance, instanceType, kind = "radarr-main", "radarr", domain.MediaMovie
			}
			if err := repo.EnsureInstance(ctx, instance, instanceType, "http://arr.invalid", now); err != nil {
				t.Fatal(err)
			}
			media := domain.Media{
				Ref: domain.MediaRef{Instance: instance, Kind: kind, FileID: 1001}, EntityID: 101, SeriesID: 10,
				Title: "Example", Fingerprint: domain.MediaFingerprint{Path: "/media/example.mkv", FileID: 1001, Size: 100, ModTime: now.Add(-time.Hour)},
			}
			seed := store.MediaEventMutation{EventID: "seed", Type: "import", Ref: media.Ref, EntityID: media.EntityID, Media: media, Languages: []domain.Language{"hr"}, At: now.Add(-time.Hour)}
			if _, err := repo.ApplyMediaEvent(ctx, seed); err != nil {
				t.Fatal(err)
			}
			snapshot, err := repo.GetReconciliationState(ctx, instance)
			if err != nil {
				t.Fatal(err)
			}
			if err := repo.CommitReconciliation(ctx, instance, snapshot, now.Add(-30*time.Minute), nil); err != nil {
				t.Fatal(err)
			}
			mediaID, _, err := repo.FindMedia(ctx, media.Ref)
			if err != nil {
				t.Fatal(err)
			}
			change := HistoryChange{HistoryID: 41, EntityID: 101, Kind: kind, Type: EventImport, State: HistoryPresent, Media: media, OccurredAt: now.Add(-time.Minute)}
			cat := &fakeReconcileCatalog{changes: []HistoryChange{change}}
			var expectedMedia domain.Media
			var expectedStatus store.SearchStatus
			var expectedInventory store.InventoryRecord
			var expectedRevision int64
			cat.afterHydration = func() {
				mutation := seed
				mutation.EventID, mutation.At = "newer-webhook", now
				switch event {
				case "movie_delete":
					mutation.Type, mutation.Ref.FileID = "delete", 0
				case "series_delete":
					mutation.Type, mutation.Ref.FileID, mutation.EntityID, mutation.SeriesID = "delete", 0, 0, 10
				case "replacement":
					mutation.Ref.FileID, mutation.Media.Ref.FileID, mutation.Media.Fingerprint.FileID = 2002, 2002, 2002
					mutation.Media.Fingerprint.Path, mutation.Media.Fingerprint.Size = "/media/replacement.mkv", 200
				case "rename":
					mutation.Type, mutation.Media.Fingerprint.Path = "rename", "/media/renamed.mkv"
				}
				if _, err := repo.ApplyMediaEvent(ctx, mutation); err != nil {
					t.Fatal(err)
				}
				expectedMedia, err = repo.GetMedia(ctx, mediaID)
				if err != nil {
					t.Fatal(err)
				}
				expectedInventory, err = repo.GetTrackInventory(ctx, mediaID)
				if err != nil {
					t.Fatal(err)
				}
				expectedStatus, err = repo.GetSearchStatus(ctx, mediaID, "hr")
				if err != nil {
					t.Fatal(err)
				}
				_, expectedRevision, err = repo.LibraryDiscoveryState(ctx, instance)
				if err != nil {
					t.Fatal(err)
				}
			}
			wakes := 0
			r := Reconciler{Instance: instance, Catalog: cat, Store: repo, Languages: []domain.Language{"hr"}, Now: func() time.Time { return now.Add(time.Minute) }, OnCommitted: func() { wakes++ }}
			if err := r.Run(ctx); !errors.Is(err, store.ErrReconciliationStale) {
				t.Errorf("stale reconciliation error = %v", err)
			}
			gotMedia, err := repo.GetMedia(ctx, mediaID)
			if err != nil || !reflect.DeepEqual(gotMedia, expectedMedia) {
				t.Errorf("stale page changed media: got %#v, want %#v, err %v", gotMedia, expectedMedia, err)
			}
			gotInventory, err := repo.GetTrackInventory(ctx, mediaID)
			if err != nil || !reflect.DeepEqual(gotInventory, expectedInventory) {
				t.Errorf("stale page changed inventory: got %#v, want %#v, err %v", gotInventory, expectedInventory, err)
			}
			gotStatus, err := repo.GetSearchStatus(ctx, mediaID, "hr")
			if err != nil || !reflect.DeepEqual(gotStatus, expectedStatus) {
				t.Errorf("stale page changed search: got %#v, want %#v, err %v", gotStatus, expectedStatus, err)
			}
			cursor, err := repo.GetReconciliationCursor(ctx, instance)
			if err != nil || !cursor.Equal(now.Add(-30*time.Minute)) {
				t.Errorf("stale page advanced cursor: %s, %v", cursor, err)
			}
			if applied, err := repo.HasAppliedMediaEvent(ctx, "reconcile:"+instance+":41"); err != nil || applied {
				t.Errorf("stale page persisted event: %v, %v", applied, err)
			}
			_, revision, err := repo.LibraryDiscoveryState(ctx, instance)
			if err != nil || revision != expectedRevision || wakes != 0 {
				t.Errorf("stale page changed revision/wakes: %d/%d, %v", revision, wakes, err)
			}
			if t.Failed() {
				return
			}

			// Retry rehydrates the current entity from the same cursor. Replay remains idempotent.
			cat.afterHydration = nil
			change.Media = expectedMedia
			if expectedInventory.Deleted {
				change.State, change.Type, change.Media = HistoryAbsent, EventDelete, domain.Media{}
			}
			cat.changes = []HistoryChange{change}
			if err := r.Run(ctx); err != nil {
				t.Fatal(err)
			}
			if !cat.since.Equal(now.Add(-30 * time.Minute)) {
				t.Fatalf("retry cursor = %s", cat.since)
			}
			if applied, err := repo.HasAppliedMediaEvent(ctx, "reconcile:"+instance+":41"); err != nil || !applied {
				t.Fatalf("retry event = %v, %v", applied, err)
			}
			_, afterRetry, err := repo.LibraryDiscoveryState(ctx, instance)
			if err != nil || afterRetry != expectedRevision+1 {
				t.Fatalf("retry revision = %d, %v", afterRetry, err)
			}
			if err := r.Run(ctx); err != nil {
				t.Fatal(err)
			}
			_, afterReplay, err := repo.LibraryDiscoveryState(ctx, instance)
			if err != nil || afterReplay != afterRetry || wakes != 2 {
				t.Fatalf("replay revision/wakes = %d/%d, %v", afterReplay, wakes, err)
			}
			gotMedia, err = repo.GetMedia(ctx, mediaID)
			if err != nil || !reflect.DeepEqual(gotMedia, expectedMedia) {
				t.Fatalf("retry/replay media = %#v, want %#v, %v", gotMedia, expectedMedia, err)
			}
		})
	}
}
