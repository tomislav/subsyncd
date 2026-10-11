package store

import (
	"testing"
	"time"

	"subsyncd/internal/domain"
)

func TestMediaReleaseDateRoundTripsAndSurvivesUndatedUpdates(t *testing.T) {
	repo := openTestRepository(t)
	ctx := t.Context()
	released := time.Date(2019, 3, 4, 2, 0, 0, 0, time.UTC)
	media := testMedia()
	media.ReleasedAt = released
	id, _, err := repo.UpsertMedia(ctx, media)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := repo.GetMedia(ctx, id); err != nil || !got.ReleasedAt.Equal(released) {
		t.Fatalf("released at = %s err=%v, want %s", got.ReleasedAt, err, released)
	}
	// A later source without a date keeps the known one.
	media.ReleasedAt = time.Time{}
	media.Title = "Show (renamed)"
	if _, _, err := repo.UpsertMedia(ctx, media); err != nil {
		t.Fatal(err)
	}
	if got, err := repo.GetMedia(ctx, id); err != nil || !got.ReleasedAt.Equal(released) || got.Title != "Show (renamed)" {
		t.Fatalf("after undated update = %#v err=%v", got, err)
	}
	// A new date replaces it, also through media events.
	corrected := released.Add(24 * time.Hour)
	media.ReleasedAt = corrected
	if _, err := repo.ApplyMediaEvent(ctx, MediaEventMutation{EventID: "rename", Type: "import", Ref: media.Ref, EntityID: media.EntityID, Media: media, Languages: []domain.Language{"hr"}, At: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	if got, err := repo.GetMedia(ctx, id); err != nil || !got.ReleasedAt.Equal(corrected) {
		t.Fatalf("after event = %s err=%v, want %s", got.ReleasedAt, err, corrected)
	}
}

func TestMediaWithoutReleaseDateReadsAsZero(t *testing.T) {
	repo := openTestRepository(t)
	id, _, err := repo.UpsertMedia(t.Context(), testMedia())
	if err != nil {
		t.Fatal(err)
	}
	if got, err := repo.GetMedia(t.Context(), id); err != nil || !got.ReleasedAt.IsZero() {
		t.Fatalf("released at = %s err=%v, want zero", got.ReleasedAt, err)
	}
}

func TestLibraryDiscoveryBackfillsReleaseDateOnly(t *testing.T) {
	repo := openTestRepository(t)
	ctx, at := t.Context(), time.Now().UTC()
	if err := repo.EnsureInstance(ctx, "sonarr-main", "sonarr", "http://sonarr", at); err != nil {
		t.Fatal(err)
	}
	media := testMedia()
	if _, err := repo.ApplyMediaEvent(ctx, MediaEventMutation{EventID: "initial", Type: "import", Ref: media.Ref, EntityID: media.EntityID, Media: media, Languages: []domain.Language{"hr"}, At: at}); err != nil {
		t.Fatal(err)
	}
	id, _, err := repo.FindMedia(ctx, media.Ref)
	if err != nil {
		t.Fatal(err)
	}
	searchesBefore := libraryTableSnapshot(t, repo, "search_states")
	_, revision, err := repo.LibraryDiscoveryState(ctx, "sonarr-main")
	if err != nil {
		t.Fatal(err)
	}
	released := time.Date(2015, 6, 1, 1, 0, 0, 0, time.UTC)
	listed := media
	listed.ReleasedAt = released
	listed.Title = "newer catalog title"
	if err := repo.CommitLibraryDiscovery(ctx, "sonarr-main", "scope", revision, []domain.Media{listed}, []domain.Language{"hr"}, at.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	got, err := repo.GetMedia(ctx, id)
	if err != nil || !got.ReleasedAt.Equal(released) {
		t.Fatalf("released at = %s err=%v, want %s", got.ReleasedAt, err, released)
	}
	if got.Title != media.Title {
		t.Fatalf("discovery changed title to %q; other metadata stays with history", got.Title)
	}
	if after := libraryTableSnapshot(t, repo, "search_states"); after != searchesBefore {
		t.Fatalf("discovery changed search states\nbefore: %s\nafter: %s", searchesBefore, after)
	}
}

func TestLibraryDiscoveryBackfillsDurationOnly(t *testing.T) {
	repo := openTestRepository(t)
	ctx, at := t.Context(), time.Now().UTC()
	if err := repo.EnsureInstance(ctx, "sonarr-main", "sonarr", "http://sonarr", at); err != nil {
		t.Fatal(err)
	}
	media := testMedia()
	if _, err := repo.ApplyMediaEvent(ctx, MediaEventMutation{EventID: "initial", Type: "import", Ref: media.Ref, EntityID: media.EntityID, Media: media, Languages: []domain.Language{"hr"}, At: at}); err != nil {
		t.Fatal(err)
	}
	id, _, err := repo.FindMedia(ctx, media.Ref)
	if err != nil {
		t.Fatal(err)
	}
	searchesBefore := libraryTableSnapshot(t, repo, "search_states")
	commit := func(listed domain.Media, offset time.Duration) {
		t.Helper()
		_, revision, err := repo.LibraryDiscoveryState(ctx, "sonarr-main")
		if err != nil {
			t.Fatal(err)
		}
		if err := repo.CommitLibraryDiscovery(ctx, "sonarr-main", "scope", revision, []domain.Media{listed}, []domain.Language{"hr"}, at.Add(offset)); err != nil {
			t.Fatal(err)
		}
	}
	duration := 47*time.Minute + 44*time.Second
	listed := media
	listed.Duration = duration
	listed.Title = "newer catalog title"
	commit(listed, time.Hour)
	got, err := repo.GetMedia(ctx, id)
	if err != nil || got.Duration != duration {
		t.Fatalf("duration = %s err=%v, want %s", got.Duration, err, duration)
	}
	if got.Title != media.Title {
		t.Fatalf("discovery changed title to %q; other metadata stays with history", got.Title)
	}
	// A listing without a runtime keeps the known one.
	listed.Duration = 0
	commit(listed, 2*time.Hour)
	if got, err := repo.GetMedia(ctx, id); err != nil || got.Duration != duration {
		t.Fatalf("duration after unknown runtime = %s err=%v, want %s", got.Duration, err, duration)
	}
	if after := libraryTableSnapshot(t, repo, "search_states"); after != searchesBefore {
		t.Fatalf("discovery changed search states\nbefore: %s\nafter: %s", searchesBefore, after)
	}
}

func TestMissingAttemptFloorJumpsAndThenAdvances(t *testing.T) {
	repo := openTestRepository(t)
	ctx, now := t.Context(), time.Now()
	id := insertTestMedia(t, repo, 1, now)
	requireSearchState(t, repo, id, "hr", now, SearchPriorityMissing)
	complete := func(floor int) {
		t.Helper()
		leases, err := repo.LeaseDueSearches(ctx, now, 1, time.Minute)
		if err != nil || len(leases) != 1 {
			t.Fatalf("leases=%#v err=%v", leases, err)
		}
		if _, err := repo.CompleteSearch(ctx, SearchCompletion{JobID: leases[0].JobID, Outcome: "no_result", NextAttemptAt: now, Priority: SearchPriorityMissing, AdvanceMissingAttempt: true, MissingAttemptFloor: floor}); err != nil {
			t.Fatal(err)
		}
	}
	attempt := func() int {
		t.Helper()
		var value int
		if err := repo.store.db.QueryRow(`SELECT attempt FROM search_states WHERE media_id=?`, id).Scan(&value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	complete(0)
	if got := attempt(); got != 1 {
		t.Fatalf("attempt without floor = %d, want 1", got)
	}
	complete(7)
	if got := attempt(); got != 7 {
		t.Fatalf("attempt with floor = %d, want 7", got)
	}
	complete(0)
	if got := attempt(); got != 8 {
		t.Fatalf("attempt after floor = %d, want 8", got)
	}
}

func TestMediaReleaseDateBeforeNineteenSeventyIsKnown(t *testing.T) {
	repo := openTestRepository(t)
	ctx := t.Context()
	released := time.Date(1966, 9, 8, 0, 30, 0, 0, time.UTC)
	media := testMedia()
	media.ReleasedAt = released
	id, _, err := repo.UpsertMedia(ctx, media)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := repo.GetMedia(ctx, id); err != nil || !got.ReleasedAt.Equal(released) {
		t.Fatalf("released at = %s err=%v, want %s", got.ReleasedAt, err, released)
	}
	media.ReleasedAt = time.Time{}
	if _, _, err := repo.UpsertMedia(ctx, media); err != nil {
		t.Fatal(err)
	}
	if got, err := repo.GetMedia(ctx, id); err != nil || !got.ReleasedAt.Equal(released) {
		t.Fatalf("after undated update = %s err=%v, want %s", got.ReleasedAt, err, released)
	}
}
