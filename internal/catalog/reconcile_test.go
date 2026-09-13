package catalog

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"subsyncd/internal/domain"
	"subsyncd/internal/store"
)

type fakeReconcileCatalog struct {
	changes       []HistoryChange
	err           error
	since         time.Time
	through       time.Time
	changeCalls   int
	snapshot      CatalogIdentitySnapshot
	snapshotErr   error
	snapshotCalls int
	afterSnapshot func()
}

func (f *fakeReconcileCatalog) GetMedia(context.Context, domain.MediaRef) (domain.Media, error) {
	return domain.Media{}, errors.New("not used")
}

func (f *fakeReconcileCatalog) ListChanges(_ context.Context, since, through time.Time) ([]HistoryChange, error) {
	f.changeCalls++
	f.since = since
	f.through = through
	return f.changes, f.err
}

func (f *fakeReconcileCatalog) ListIdentitySnapshot(context.Context) (CatalogIdentitySnapshot, error) {
	f.snapshotCalls++
	snapshot := f.snapshot
	if f.afterSnapshot != nil {
		f.afterSnapshot()
	}
	return snapshot, f.snapshotErr
}

type fakeReconcileStore struct {
	cursor    time.Time
	committed time.Time
	mutations []store.MediaEventMutation
	commitErr error
	stateErr  error
	activeIDs []int64
	listErr   error
	listCalls int
	commits   int
	instance  string
	kind      domain.MediaKind
}

func (f *fakeReconcileStore) GetReconciliationState(context.Context, string) (store.ReconciliationState, error) {
	return store.ReconciliationState{Cursor: f.cursor}, f.stateErr
}

func (f *fakeReconcileStore) ListActiveCatalogIdentities(_ context.Context, instance string, kind domain.MediaKind) ([]int64, error) {
	f.listCalls++
	f.instance = instance
	f.kind = kind
	return append([]int64(nil), f.activeIDs...), f.listErr
}

func (f *fakeReconcileStore) CommitReconciliation(_ context.Context, _ string, _ store.ReconciliationState, cursor time.Time, mutations []store.MediaEventMutation) error {
	f.commits++
	if f.commitErr != nil {
		return f.commitErr
	}
	f.committed = cursor
	f.mutations = append([]store.MediaEventMutation(nil), mutations...)
	return nil
}

func completeSnapshot(kind domain.MediaKind, ids ...int64) CatalogIdentitySnapshot {
	set := make(map[int64]struct{}, len(ids))
	for _, id := range ids {
		set[id] = struct{}{}
	}
	return CatalogIdentitySnapshot{Kind: kind, IDs: set}
}

func TestReconcilerRetiresEveryEpisodeForSeriesMissingFromCompleteSnapshot(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 13, 10, 0, 0, 0, time.UTC)
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	repo := db.Repository()
	if err := repo.EnsureInstance(ctx, "sonarr-main", "sonarr", "http://arr.invalid", now); err != nil {
		t.Fatal(err)
	}

	var mediaIDs []int64
	for i, entityID := range []int64{101, 102} {
		media := domain.Media{
			EntityID: entityID,
			SeriesID: 42,
			Ref:      domain.MediaRef{Instance: "sonarr-main", Kind: domain.MediaEpisode, FileID: 1001 + int64(i)},
			Fingerprint: domain.MediaFingerprint{
				Path: "/media/show/episode.mkv", FileID: 1001 + int64(i), Size: 100, ModTime: now.Add(-time.Hour),
			},
		}
		applied, err := repo.ApplyMediaEvent(ctx, store.MediaEventMutation{
			EventID: "seed-" + string(rune('a'+i)), Type: "import", EntityID: entityID,
			Ref: media.Ref, Media: media, Languages: []domain.Language{"hr", "en"}, At: now.Add(-time.Hour),
		})
		if err != nil || !applied {
			t.Fatalf("seed episode %d: applied=%t err=%v", entityID, applied, err)
		}
		mediaID, _, err := repo.FindMedia(ctx, media.Ref)
		if err != nil {
			t.Fatal(err)
		}
		mediaIDs = append(mediaIDs, mediaID)
	}

	pageEnd := now.Add(time.Minute)
	reconciler := Reconciler{
		Instance: "sonarr-main",
		Kind:     domain.MediaEpisode,
		Catalog: &fakeReconcileCatalog{snapshot: CatalogIdentitySnapshot{
			Kind: domain.MediaEpisode,
			IDs:  map[int64]struct{}{},
		}},
		Store: repo, Languages: []domain.Language{"hr", "en"}, Now: func() time.Time { return pageEnd },
	}
	if err := reconciler.Run(ctx); err != nil {
		t.Fatal(err)
	}
	for _, mediaID := range mediaIDs {
		for _, language := range []domain.Language{"hr", "en"} {
			status, err := repo.GetSearchStatus(ctx, mediaID, language)
			if err != nil || status.State != "complete" || status.LastOutcome != "deleted" {
				t.Fatalf("media %d language %s status = %#v, %v", mediaID, language, status, err)
			}
		}
	}
	identities, err := repo.ListActiveCatalogIdentities(ctx, "sonarr-main", domain.MediaEpisode)
	if err != nil || len(identities) != 0 {
		t.Fatalf("active series after reconciliation = %v, %v", identities, err)
	}
	cursor, err := repo.GetReconciliationCursor(ctx, "sonarr-main")
	if err != nil || !cursor.Equal(pageEnd) {
		t.Fatalf("cursor = %s, %v; want %s", cursor, err, pageEnd)
	}
	eventID := snapshotDeleteEventID("sonarr-main", domain.MediaEpisode, 42, pageEnd)
	if applied, err := repo.HasAppliedMediaEvent(ctx, eventID); err != nil || !applied {
		t.Fatalf("snapshot event %q applied = %t, %v", eventID, applied, err)
	}
	if err := reconciler.Run(ctx); err != nil {
		t.Fatal(err)
	}
	for _, mediaID := range mediaIDs {
		status, err := repo.GetSearchStatus(ctx, mediaID, "hr")
		if err != nil || status.State != "complete" || status.LastOutcome != "deleted" {
			t.Fatalf("replayed media %d status = %#v, %v", mediaID, status, err)
		}
	}
}

func TestReconcilerSnapshotDeletionIsInstanceKindAndLegacySeriesIsolated(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 13, 10, 30, 0, 0, time.UTC)
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	repo := db.Repository()
	for _, instance := range []string{"sonarr-main", "sonarr-other"} {
		if err := repo.EnsureInstance(ctx, instance, "sonarr", "http://arr.invalid", now); err != nil {
			t.Fatal(err)
		}
	}

	seed := func(media domain.Media) int64 {
		t.Helper()
		if _, err := repo.ApplyMediaEvent(ctx, store.MediaEventMutation{
			EventID: fmt.Sprintf("seed:%s:%s:%d", media.Ref.Instance, media.Ref.Kind, media.EntityID), Type: "import",
			EntityID: media.EntityID, Ref: media.Ref, Media: media, Languages: []domain.Language{"hr"}, At: now.Add(-time.Hour),
		}); err != nil {
			t.Fatal(err)
		}
		id, _, err := repo.FindMedia(ctx, media.Ref)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	media := func(instance string, kind domain.MediaKind, entityID, seriesID, fileID int64) domain.Media {
		return domain.Media{
			EntityID: entityID, SeriesID: seriesID, Ref: domain.MediaRef{Instance: instance, Kind: kind, FileID: fileID},
			Fingerprint: domain.MediaFingerprint{Path: fmt.Sprintf("/media/%s/%d.mkv", instance, fileID), FileID: fileID, Size: 100, ModTime: now.Add(-time.Hour)},
		}
	}
	legacyID := seed(media("sonarr-main", domain.MediaEpisode, 101, 0, 1001))
	otherInstanceID := seed(media("sonarr-other", domain.MediaEpisode, 102, 42, 1002))
	otherKindID := seed(media("sonarr-main", domain.MediaMovie, 42, 0, 1003))

	r := Reconciler{
		Instance: "sonarr-main", Kind: domain.MediaEpisode, Catalog: &fakeReconcileCatalog{snapshot: completeSnapshot(domain.MediaEpisode)},
		Store: repo, Languages: []domain.Language{"hr"}, Now: func() time.Time { return now },
	}
	if err := r.Run(ctx); err != nil {
		t.Fatal(err)
	}
	for name, mediaID := range map[string]int64{"legacy zero series": legacyID, "other instance": otherInstanceID, "other kind": otherKindID} {
		status, err := repo.GetSearchStatus(ctx, mediaID, "hr")
		if err != nil || status.State != "pending" || status.LastOutcome != "" {
			t.Fatalf("%s status = %#v, %v", name, status, err)
		}
	}
}

func TestReconcilerDoesNotAdvanceCursorWhenPageCommitFails(t *testing.T) {
	start := time.Date(2026, 9, 4, 10, 0, 0, 0, time.UTC)
	catalog := &fakeReconcileCatalog{changes: []HistoryChange{{HistoryID: 1, EntityID: 101, Kind: domain.MediaEpisode, Type: EventImport, State: HistoryPresent, Media: domain.Media{EntityID: 101, Ref: domain.MediaRef{Instance: "sonarr-main", Kind: domain.MediaEpisode, FileID: 1}}, OccurredAt: start}}}
	store := &fakeReconcileStore{cursor: start, commitErr: errors.New("disk full")}
	catalog.snapshot = completeSnapshot(domain.MediaEpisode)
	reconciler := Reconciler{Instance: "sonarr-main", Kind: domain.MediaEpisode, Catalog: catalog, Store: store, Languages: []domain.Language{"hr"}, Now: func() time.Time { return start.Add(time.Hour) }}

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
	catalog := &fakeReconcileCatalog{changes: changes, snapshot: completeSnapshot(domain.MediaEpisode)}
	backend := &fakeReconcileStore{cursor: start}
	reconciler := Reconciler{Instance: "sonarr-main", Kind: domain.MediaEpisode, Catalog: catalog, Store: backend, Languages: []domain.Language{"hr", "en"}, Now: func() time.Time { return pageEnd }}
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

func TestReconcilerCommitsNondeferredChangesWithoutAdvancingCursor(t *testing.T) {
	start := time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC)
	pageEnd := start.Add(time.Hour)
	change := HistoryChange{HistoryID: 42, EntityID: 102, Kind: domain.MediaEpisode, Type: EventDelete, State: HistoryAbsent, OccurredAt: start.Add(20 * time.Minute)}
	catalog := &fakeReconcileCatalog{changes: []HistoryChange{change}, err: ErrHistoryDeferred}
	backend := &fakeReconcileStore{cursor: start}
	wakes := 0
	reconciler := Reconciler{Instance: "sonarr-main", Kind: domain.MediaEpisode, Catalog: catalog, Store: backend, Languages: []domain.Language{"hr"}, Now: func() time.Time { return pageEnd }, OnCommitted: func() { wakes++ }}

	if err := reconciler.Run(context.Background()); !errors.Is(err, ErrHistoryDeferred) {
		t.Fatalf("Reconciler.Run() error = %v, want ErrHistoryDeferred", err)
	}
	if !backend.committed.Equal(start) {
		t.Fatalf("cursor = %s, want retained %s", backend.committed, start)
	}
	if len(backend.mutations) != 1 || backend.mutations[0].EventID != "reconcile:sonarr-main:42" {
		t.Fatalf("committed mutations = %#v", backend.mutations)
	}
	if wakes != 1 {
		t.Fatalf("commit callbacks = %d, want 1", wakes)
	}
	if catalog.snapshotCalls != 0 || backend.listCalls != 0 {
		t.Fatalf("deferred page used incomplete snapshot view: snapshot calls=%d store calls=%d", catalog.snapshotCalls, backend.listCalls)
	}
}

func TestReconcilerRejectsInvalidEntityHistoryStateBeforeCommit(t *testing.T) {
	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	catalog := &fakeReconcileCatalog{changes: []HistoryChange{{HistoryID: 44, EntityID: 104, Kind: domain.MediaEpisode, Type: EventDelete, State: HistoryState("uncertain"), OccurredAt: now}}}
	backend := &fakeReconcileStore{}
	wakes := 0
	reconciler := Reconciler{Instance: "sonarr-main", Kind: domain.MediaEpisode, Catalog: catalog, Store: backend, Now: func() time.Time { return now }, OnCommitted: func() { wakes++ }}
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
	reconciler := Reconciler{Instance: "sonarr-main", Kind: domain.MediaEpisode, Catalog: &fakeReconcileCatalog{snapshot: completeSnapshot(domain.MediaEpisode)}, Store: &fakeReconcileStore{}, Languages: []domain.Language{"hr"}, Now: func() time.Time { return now }, OnCommitted: func() { wakes++ }}
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

func TestReconcilerRetiresOnlyRadarrMoviesAbsentFromCompleteSnapshot(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 13, 11, 0, 0, 0, time.UTC)
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	repo := db.Repository()
	if err := repo.EnsureInstance(ctx, "radarr-main", "radarr", "http://arr.invalid", now); err != nil {
		t.Fatal(err)
	}

	mediaIDs := make(map[int64]int64)
	for _, entityID := range []int64{201, 202} {
		media := domain.Media{
			EntityID: entityID,
			Ref:      domain.MediaRef{Instance: "radarr-main", Kind: domain.MediaMovie, FileID: entityID + 1000},
			Fingerprint: domain.MediaFingerprint{
				Path: "/media/movies/movie.mkv", FileID: entityID + 1000, Size: 100, ModTime: now.Add(-time.Hour),
			},
		}
		if _, err := repo.ApplyMediaEvent(ctx, store.MediaEventMutation{
			EventID: fmt.Sprintf("seed-%d", entityID), Type: "import", EntityID: entityID,
			Ref: media.Ref, Media: media, Languages: []domain.Language{"hr"}, At: now.Add(-time.Hour),
		}); err != nil {
			t.Fatal(err)
		}
		mediaID, _, err := repo.FindMedia(ctx, media.Ref)
		if err != nil {
			t.Fatal(err)
		}
		mediaIDs[entityID] = mediaID
	}

	reconciler := Reconciler{
		Instance: "radarr-main",
		Kind:     domain.MediaMovie,
		Catalog:  &fakeReconcileCatalog{snapshot: completeSnapshot(domain.MediaMovie, 202)},
		Store:    repo, Languages: []domain.Language{"hr"}, Now: func() time.Time { return now.Add(time.Minute) },
	}
	if err := reconciler.Run(ctx); err != nil {
		t.Fatal(err)
	}
	deleted, err := repo.GetSearchStatus(ctx, mediaIDs[201], "hr")
	if err != nil || deleted.State != "complete" || deleted.LastOutcome != "deleted" {
		t.Fatalf("absent movie status = %#v, %v", deleted, err)
	}
	current, err := repo.GetSearchStatus(ctx, mediaIDs[202], "hr")
	if err != nil || current.State != "pending" || current.LastOutcome != "" {
		t.Fatalf("current movie status = %#v, %v", current, err)
	}
}

func TestReconcilerComposesHistoryAndSnapshotDeletesDeterministically(t *testing.T) {
	start := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	pageEnd := start.Add(time.Hour)
	media := domain.Media{EntityID: 101, Ref: domain.MediaRef{Instance: "sonarr-main", Kind: domain.MediaEpisode, FileID: 1001}}
	catalog := &fakeReconcileCatalog{
		changes:  []HistoryChange{{HistoryID: 41, EntityID: 101, Kind: domain.MediaEpisode, Type: EventImport, State: HistoryPresent, Media: media, OccurredAt: start}},
		snapshot: completeSnapshot(domain.MediaEpisode, 10),
	}
	backend := &fakeReconcileStore{cursor: start.Add(-time.Hour), activeIDs: []int64{42, 10}}
	r := Reconciler{Instance: "sonarr-main", Kind: domain.MediaEpisode, Catalog: catalog, Store: backend, Languages: []domain.Language{"hr"}, Now: func() time.Time { return pageEnd }}

	if err := r.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(backend.mutations) != 2 {
		t.Fatalf("composed mutations = %#v", backend.mutations)
	}
	if backend.mutations[0].EventID != "reconcile:sonarr-main:41" || backend.mutations[1].SeriesID != 42 || backend.mutations[1].EntityID != 0 || !backend.mutations[1].At.Equal(pageEnd) {
		t.Fatalf("composed mutations = %#v", backend.mutations)
	}
	firstSnapshotID := backend.mutations[1].EventID
	if !strings.HasPrefix(firstSnapshotID, "snapshot-reconcile:") || len(firstSnapshotID) != len("snapshot-reconcile:")+64 || strings.HasPrefix(firstSnapshotID, "reconcile:") {
		t.Fatalf("snapshot event ID = %q", firstSnapshotID)
	}
	if err := r.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(backend.mutations) != 2 || backend.mutations[1].EventID != firstSnapshotID {
		t.Fatalf("replayed mutations = %#v, want snapshot ID %q", backend.mutations, firstSnapshotID)
	}
	if backend.instance != "sonarr-main" || backend.kind != domain.MediaEpisode {
		t.Fatalf("active identity scope = %q/%q", backend.instance, backend.kind)
	}
}

func TestReconcilerRejectsInvalidOrUnavailableIdentityViewsBeforeCommit(t *testing.T) {
	now := time.Date(2026, 9, 13, 13, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name      string
		kind      domain.MediaKind
		snapshot  CatalogIdentitySnapshot
		snapErr   error
		activeIDs []int64
		listErr   error
	}{
		{name: "snapshot failure", kind: domain.MediaEpisode, snapshot: completeSnapshot(domain.MediaEpisode), snapErr: errors.New("snapshot unavailable")},
		{name: "incomplete nil set", kind: domain.MediaEpisode, snapshot: CatalogIdentitySnapshot{Kind: domain.MediaEpisode}},
		{name: "invalid kind", kind: domain.MediaEpisode, snapshot: completeSnapshot(domain.MediaKind("book"))},
		{name: "configured kind mismatch", kind: domain.MediaEpisode, snapshot: completeSnapshot(domain.MediaMovie)},
		{name: "invalid snapshot identity", kind: domain.MediaMovie, snapshot: completeSnapshot(domain.MediaMovie, 0)},
		{name: "active identity read failure", kind: domain.MediaMovie, snapshot: completeSnapshot(domain.MediaMovie), listErr: errors.New("database unavailable")},
		{name: "invalid active identity", kind: domain.MediaMovie, snapshot: completeSnapshot(domain.MediaMovie), activeIDs: []int64{0}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			catalog := &fakeReconcileCatalog{snapshot: tc.snapshot, snapshotErr: tc.snapErr}
			backend := &fakeReconcileStore{activeIDs: tc.activeIDs, listErr: tc.listErr}
			wakes := 0
			r := Reconciler{Instance: "arr-main", Kind: tc.kind, Catalog: catalog, Store: backend, Now: func() time.Time { return now }, OnCommitted: func() { wakes++ }}
			if err := r.Run(context.Background()); err == nil {
				t.Fatal("Reconciler.Run() error = nil")
			}
			if backend.commits != 0 || !backend.committed.IsZero() || len(backend.mutations) != 0 || wakes != 0 {
				t.Fatalf("invalid identity view committed: commits=%d cursor=%s mutations=%#v wakes=%d", backend.commits, backend.committed, backend.mutations, wakes)
			}
		})
	}
}

func TestReconcilerCompleteSnapshotWithNoStoredIdentitiesCommitsNoDelete(t *testing.T) {
	now := time.Date(2026, 9, 13, 14, 0, 0, 0, time.UTC)
	backend := &fakeReconcileStore{}
	r := Reconciler{Instance: "radarr-main", Kind: domain.MediaMovie, Catalog: &fakeReconcileCatalog{snapshot: completeSnapshot(domain.MediaMovie)}, Store: backend, Now: func() time.Time { return now }}
	if err := r.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if backend.commits != 1 || len(backend.mutations) != 0 || !backend.committed.Equal(now) {
		t.Fatalf("empty tracked set commit = %d/%#v/%s", backend.commits, backend.mutations, backend.committed)
	}
}

func TestReconcilerStateReadFailureAvoidsRemoteIdentityAndHistoryReads(t *testing.T) {
	catalog := &fakeReconcileCatalog{snapshot: completeSnapshot(domain.MediaEpisode)}
	backend := &fakeReconcileStore{stateErr: errors.New("database unavailable")}
	r := Reconciler{Instance: "sonarr-main", Kind: domain.MediaEpisode, Catalog: catalog, Store: backend, Now: time.Now}
	if err := r.Run(context.Background()); err == nil {
		t.Fatal("Reconciler.Run() error = nil")
	}
	if catalog.changeCalls != 0 || catalog.snapshotCalls != 0 || backend.listCalls != 0 || backend.commits != 0 {
		t.Fatalf("failed state read used remote/store work: changes=%d snapshots=%d lists=%d commits=%d", catalog.changeCalls, catalog.snapshotCalls, backend.listCalls, backend.commits)
	}
}

// A webhook committed after history and identity snapshot hydration must win
// over the stale page. Removing the fence resurrects deletions and old files.
func TestReconcilerRejectsHistoryAndSnapshotFetchedBeforeConcurrentWebhook(t *testing.T) {
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
			cat := &fakeReconcileCatalog{changes: []HistoryChange{change}, snapshot: completeSnapshot(kind)}
			var expectedMedia domain.Media
			var expectedStatus store.SearchStatus
			var expectedInventory store.InventoryRecord
			var expectedRevision int64
			cat.afterSnapshot = func() {
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
			r := Reconciler{Instance: instance, Kind: kind, Catalog: cat, Store: repo, Languages: []domain.Language{"hr"}, Now: func() time.Time { return now.Add(time.Minute) }, OnCommitted: func() { wakes++ }}
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
			cat.afterSnapshot = nil
			change.Media = expectedMedia
			if expectedInventory.Deleted {
				change.State, change.Type, change.Media = HistoryAbsent, EventDelete, domain.Media{}
				cat.snapshot = completeSnapshot(kind)
			} else if kind == domain.MediaEpisode {
				cat.snapshot = completeSnapshot(kind, expectedMedia.SeriesID)
			} else {
				cat.snapshot = completeSnapshot(kind, expectedMedia.EntityID)
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
