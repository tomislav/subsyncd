package store

import (
	"testing"
	"time"

	"subsyncd/internal/domain"
	"subsyncd/internal/selection"
)

var missingNow = time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)

func missingQuery() MissingQuery {
	return MissingQuery{Languages: []domain.Language{"en", "hr"}, Now: missingNow, Limit: 50}
}

// addMissingMedia stores a movie and, unless priority is zero, queues search
// states for every given language at that priority and time.
func addMissingMedia(t *testing.T, repo *Repository, fileID int64, title string, at time.Time, priority SearchPriority, languages ...domain.Language) int64 {
	t.Helper()
	id := addMatchMedia(t, repo, "radarr", domain.MediaMovie, fileID, title, nil)
	for _, language := range languages {
		if err := repo.UpsertSearchStateWithPriority(t.Context(), id, language, at, priority); err != nil {
			t.Fatal(err)
		}
	}
	return id
}

func setLastOutcome(t *testing.T, repo *Repository, mediaID int64, language, outcome string) {
	t.Helper()
	if _, err := repo.store.db.ExecContext(t.Context(), `UPDATE search_states SET last_outcome=?, state='complete' WHERE media_id=? AND language=?`, outcome, mediaID, language); err != nil {
		t.Fatal(err)
	}
}

func missingKeys(rows []MissingRow) []string {
	keys := make([]string, len(rows))
	for i, row := range rows {
		keys[i] = row.Title + "/" + row.Language
	}
	return keys
}

func TestMissingExcludesSatisfiedByEmbeddedTrack(t *testing.T) {
	repo := openTestRepository(t)
	ctx := t.Context()
	// Embedded English covers en; forced-only and (disallowed) SDH tracks do not.
	covered := addMissingMedia(t, repo, 1, "Covered", missingNow, SearchPriorityMissing, "en")
	forced := addMissingMedia(t, repo, 2, "Forced", missingNow, SearchPriorityMissing, "en")
	sdh := addMissingMedia(t, repo, 3, "Sdh", missingNow, SearchPriorityMissing, "en")
	installed := addMissingMedia(t, repo, 4, "Installed", missingNow, SearchPriorityMissing, "en")
	satisfied := addMissingMedia(t, repo, 5, "Satisfied", missingNow, SearchPriorityMissing, "en")
	setLastOutcome(t, repo, satisfied, "en", "satisfied")
	for _, fixture := range []struct {
		id    int64
		title string
		track TrackRecord
	}{
		{covered, "Covered", TrackRecord{Language: "en", Embedded: true}},
		{forced, "Forced", TrackRecord{Language: "en", Embedded: true, Forced: true}},
		{sdh, "Sdh", TrackRecord{Language: "en", Path: "/media/Sdh.en.srt", SDH: true}},
	} {
		fingerprint := domain.MediaFingerprint{Path: "/media/" + fixture.title + ".mkv", FileID: fixture.id, Size: 1, ModTime: time.Unix(1, 0).UTC()}
		fixture.track.MediaID = fixture.id
		if err := repo.ReplaceTrackInventory(ctx, fixture.id, fingerprint, fingerprint, []TrackRecord{fixture.track}); err != nil {
			t.Fatal(err)
		}
	}
	if err := repo.RecordInstallation(ctx, Installation{MediaID: installed, Language: "en", Path: "/media/Installed.en.srt", Checksum: "x", ProviderID: "alpha", CandidateID: "1", MediaPath: "/media/Installed.mkv", MediaFileID: 4, MediaSize: 1, MediaModTimeNS: time.Unix(1, 0).UTC().UnixNano(), ScoreJSON: []byte("{}"), SyncResultJSON: []byte("{}")}); err != nil {
		t.Fatal(err)
	}
	base := missingQuery()
	base.Languages = []domain.Language{"en"}
	rows, total, err := repo.ListMissing(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	if got := missingKeys(rows); total != 2 || len(got) != 2 {
		t.Fatalf("rows = %v total = %d", got, total)
	}
	want := map[string]bool{"Forced/en": true, "Sdh/en": true}
	for _, key := range missingKeys(rows) {
		if !want[key] {
			t.Fatalf("unexpected row %q in %v", key, missingKeys(rows))
		}
	}
	// Allowing hearing-impaired tracks lets the SDH sidecar cover English.
	query := base
	query.AllowHearingImpaired = true
	if _, total, err = repo.ListMissing(ctx, query); err != nil || total != 1 {
		t.Fatalf("allow_hearing_impaired total = %d, %v", total, err)
	}
}

func TestMissingOrderFollowsQueue(t *testing.T) {
	repo := openTestRepository(t)
	// Higher priority first, then earlier queue time; a language with no
	// search state, and an unqueued media file, come last.
	addMissingMedia(t, repo, 1, "Late", missingNow.Add(time.Hour), SearchPriorityMissing, "en")
	addMissingMedia(t, repo, 2, "Early", missingNow, SearchPriorityMissing, "en")
	addMissingMedia(t, repo, 3, "Import", missingNow.Add(2*time.Hour), SearchPriorityImport, "en")
	addMissingMedia(t, repo, 4, "Unqueued", missingNow, SearchPriorityMissing)
	query := missingQuery()
	query.Languages = []domain.Language{"en"}
	rows, total, err := repo.ListMissing(t.Context(), query)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"Import/en", "Early/en", "Late/en", "Unqueued/en"}
	got := missingKeys(rows)
	if total != 4 || len(got) != 4 {
		t.Fatalf("got %v total %d", got, total)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}
	if !rows[3].NoSearch || rows[0].NoSearch {
		t.Fatalf("NoSearch = %v/%v", rows[0].NoSearch, rows[3].NoSearch)
	}
}

func TestMissingLimitReportsTotal(t *testing.T) {
	repo := openTestRepository(t)
	for i := int64(1); i <= 5; i++ {
		addMissingMedia(t, repo, i, "Film", missingNow.Add(time.Duration(i)*time.Minute), SearchPriorityMissing, "en")
	}
	query := missingQuery()
	query.Languages = []domain.Language{"en"}
	query.Limit = 2
	rows, total, err := repo.ListMissing(t.Context(), query)
	if err != nil || len(rows) != 2 || total != 5 {
		t.Fatalf("rows = %d total = %d err = %v", len(rows), total, err)
	}
	if rows[0].FileID != 1 || rows[1].FileID != 2 {
		t.Fatalf("rows = %+v", rows)
	}
}

func TestMissingFilters(t *testing.T) {
	repo := openTestRepository(t)
	ctx := t.Context()
	addMissingMedia(t, repo, 1, "Dune", missingNow, SearchPriorityMissing, "en", "hr")
	addMissingMedia(t, repo, 2, "Heat", missingNow, SearchPriorityMissing, "en", "hr")
	other := addMatchMedia(t, repo, "radarr-4k", domain.MediaMovie, 3, "Dune", nil)
	if err := repo.UpsertSearchStateWithPriority(ctx, other, "en", missingNow, SearchPriorityMissing); err != nil {
		t.Fatal(err)
	}
	// Deleted media never shows.
	gone := addMissingMedia(t, repo, 4, "Gone", missingNow, SearchPriorityMissing, "en")
	if _, err := repo.store.db.ExecContext(ctx, `UPDATE media SET deleted=1 WHERE id=?`, gone); err != nil {
		t.Fatal(err)
	}
	query := missingQuery()
	query.Language = "hr"
	if _, total, err := repo.ListMissing(ctx, query); err != nil || total != 3 {
		t.Fatalf("language total = %d, %v", total, err)
	}
	query = missingQuery()
	query.Instance = "radarr-4k"
	if rows, total, err := repo.ListMissing(ctx, query); err != nil || total != 2 || rows[0].Instance != "radarr-4k" {
		t.Fatalf("instance rows = %+v total = %d, %v", rows, total, err)
	}
	title, _ := selection.ParseTitle("dune")
	query = missingQuery()
	query.Title = title
	if rows, total, err := repo.ListMissing(ctx, query); err != nil || total != 4 || len(rows) != 4 {
		t.Fatalf("title rows = %v total = %d, %v", missingKeys(rows), total, err)
	}
	query = missingQuery()
	query.Title = selection.TitleQuery{Name: "nothing"}
	if rows, total, err := repo.ListMissing(ctx, query); err != nil || total != 0 || len(rows) != 0 {
		t.Fatalf("no-match rows = %d total = %d, %v", len(rows), total, err)
	}
}
