package store

import (
	"testing"
	"time"

	"subsyncd/internal/control"
	"subsyncd/internal/domain"
	"subsyncd/internal/selection"
)

// addMatchMedia stores one media row; fileID doubles as the entity ID.
func addMatchMedia(t *testing.T, repo *Repository, instance string, kind domain.MediaKind, fileID int64, title string, edit func(*domain.Media)) int64 {
	t.Helper()
	media := domain.Media{
		EntityID:    fileID,
		Ref:         domain.MediaRef{Instance: instance, Kind: kind, FileID: fileID},
		Fingerprint: domain.MediaFingerprint{Path: "/media/" + title + ".mkv", FileID: fileID, Size: 1, ModTime: time.Unix(1, 0).UTC()},
		Title:       title,
	}
	if edit != nil {
		edit(&media)
	}
	id, _, err := repo.UpsertMedia(t.Context(), media)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func matchTitle(t *testing.T, repo *Repository, text string) []control.MediaMatch {
	t.Helper()
	query, err := selection.ParseTitle(text)
	if err != nil {
		t.Fatal(err)
	}
	matches, err := repo.MatchMedia(t.Context(), MediaQuery{Title: query})
	if err != nil {
		t.Fatal(err)
	}
	return matches
}

func TestMatchNormalizesPunctuationCaseAndDiacritics(t *testing.T) {
	repo := openTestRepository(t)
	addMatchMedia(t, repo, "radarr", domain.MediaMovie, 1, "Amélie", nil)
	addMatchMedia(t, repo, "sonarr", domain.MediaEpisode, 2, "The Office (US)", func(m *domain.Media) { m.Season, m.Episode = 1, 2 })
	addMatchMedia(t, repo, "sonarr", domain.MediaEpisode, 3, "Marvel's Agents of S.H.I.E.L.D.", func(m *domain.Media) { m.Season, m.Episode = 1, 1 })
	for text, wantID := range map[string]int64{"amelie": 1, "the office us s01e02": 2, "marvels agents of shield": 3} {
		matches := matchTitle(t, repo, text)
		if len(matches) != 1 || matches[0].FileID != wantID {
			t.Fatalf("%q matched %+v, want file %d", text, matches, wantID)
		}
	}
}

func TestMatchUsesAlternateTitles(t *testing.T) {
	repo := openTestRepository(t)
	addMatchMedia(t, repo, "radarr", domain.MediaMovie, 1, "Das Boot", func(m *domain.Media) { m.AlternateTitles = []string{"The Boat"} })
	if matches := matchTitle(t, repo, "the boat"); len(matches) != 1 || matches[0].Title != "Das Boot" {
		t.Fatalf("matches = %+v", matches)
	}
}

func TestMatchExactBeatsPartial(t *testing.T) {
	repo := openTestRepository(t)
	addMatchMedia(t, repo, "radarr", domain.MediaMovie, 1, "Dune", func(m *domain.Media) { m.Year = 2021 })
	addMatchMedia(t, repo, "radarr", domain.MediaMovie, 2, "Dune: Part Two", func(m *domain.Media) { m.Year = 2024 })
	matches := matchTitle(t, repo, "dune")
	if len(matches) != 1 || matches[0].FileID != 1 || matches[0].Year != 2021 {
		t.Fatalf("matches = %+v", matches)
	}
	if matches := matchTitle(t, repo, "part two"); len(matches) != 1 || matches[0].FileID != 2 {
		t.Fatalf("partial matches = %+v", matches)
	}
}

func TestMatchYearNarrowsTitle(t *testing.T) {
	repo := openTestRepository(t)
	addMatchMedia(t, repo, "radarr", domain.MediaMovie, 1, "Dune", func(m *domain.Media) { m.Year = 1984 })
	addMatchMedia(t, repo, "radarr", domain.MediaMovie, 2, "Dune", func(m *domain.Media) { m.Year = 2021 })
	if matches := matchTitle(t, repo, "Dune"); len(matches) != 2 {
		t.Fatalf("matches = %+v", matches)
	}
	if matches := matchTitle(t, repo, "Dune (2021)"); len(matches) != 1 || matches[0].FileID != 2 {
		t.Fatalf("matches = %+v", matches)
	}
}

func TestMatchSameTitleTwoSeriesIsAmbiguous(t *testing.T) {
	repo := openTestRepository(t)
	for id, year := range map[int64]int{1: 2004, 2: 2011} {
		addMatchMedia(t, repo, "sonarr", domain.MediaEpisode, id, "Shameless", func(m *domain.Media) { m.Season, m.Episode, m.Year = 1, 1, year })
	}
	matches := matchTitle(t, repo, "Shameless S01E01")
	if len(matches) != 2 || matches[0].Season != 1 || matches[0].Episode != 1 || matches[0].Kind != "episode" {
		t.Fatalf("matches = %+v", matches)
	}
}

func TestMatchEpisodeInsideMultiEpisodeFile(t *testing.T) {
	repo := openTestRepository(t)
	addMatchMedia(t, repo, "sonarr", domain.MediaEpisode, 1, "Show", func(m *domain.Media) { m.Season, m.Episode, m.EpisodeEnd = 1, 1, 2 })
	matches := matchTitle(t, repo, "Show S01E02")
	if len(matches) != 1 || matches[0].EpisodeEnd != 2 {
		t.Fatalf("matches = %+v", matches)
	}
}

func TestMatchSkipsDeletedMedia(t *testing.T) {
	repo := openTestRepository(t)
	id := addMatchMedia(t, repo, "radarr", domain.MediaMovie, 1, "Gone", nil)
	if _, err := repo.store.db.ExecContext(t.Context(), `UPDATE media SET deleted=1 WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	if matches := matchTitle(t, repo, "gone"); len(matches) != 0 {
		t.Fatalf("matches = %+v", matches)
	}
	matches, err := repo.MatchMedia(t.Context(), MediaQuery{Path: "/media/Gone.mkv"})
	if err != nil || len(matches) != 0 {
		t.Fatalf("path matches = %+v, %v", matches, err)
	}
}

func TestMatchNarrowsByInstanceAndKind(t *testing.T) {
	repo := openTestRepository(t)
	addMatchMedia(t, repo, "radarr-a", domain.MediaMovie, 1, "Dune", nil)
	addMatchMedia(t, repo, "radarr-b", domain.MediaMovie, 1, "Dune", nil)
	query := selection.TitleQuery{Name: "dune"}
	matches, err := repo.MatchMedia(t.Context(), MediaQuery{Instance: "radarr-b", Kind: "movie", Title: query})
	if err != nil || len(matches) != 1 || matches[0].Instance != "radarr-b" {
		t.Fatalf("matches = %+v, %v", matches, err)
	}
	if matches, _ := repo.MatchMedia(t.Context(), MediaQuery{Kind: "episode", Title: query}); len(matches) != 0 {
		t.Fatalf("matches = %+v", matches)
	}
}

func TestMatchPathIsExact(t *testing.T) {
	repo := openTestRepository(t)
	addMatchMedia(t, repo, "radarr", domain.MediaMovie, 1, "Dune", nil)
	matches, err := repo.MatchMedia(t.Context(), MediaQuery{Path: "/media/Dune.mkv"})
	if err != nil || len(matches) != 1 || matches[0].FileID != 1 {
		t.Fatalf("matches = %+v, %v", matches, err)
	}
	matches, err = repo.MatchMedia(t.Context(), MediaQuery{Path: "/media/Dune"})
	if err != nil || len(matches) != 0 {
		t.Fatalf("partial path matches = %+v, %v", matches, err)
	}
}

func TestLanguageStatesPrecedence(t *testing.T) {
	repo := openTestRepository(t)
	ctx := t.Context()
	now := time.Now().UTC()
	id := addMatchMedia(t, repo, "radarr", domain.MediaMovie, 1, "Dune", nil)
	for _, language := range []domain.Language{"en", "hr", "de", "fr"} {
		if err := repo.UpsertSearchStateWithPriority(ctx, id, language, now.Add(-time.Hour), SearchPriorityMissing); err != nil {
			t.Fatal(err)
		}
	}
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := repo.store.db.ExecContext(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	// en: installed and leased (running wins); hr: installed; de: satisfied; fr: nothing yet.
	exec(`UPDATE search_states SET lease_owner='job', lease_until_ns=? WHERE media_id=? AND language='en'`, now.Add(time.Minute).UnixNano(), id)
	exec(`UPDATE search_states SET last_outcome='satisfied' WHERE media_id=? AND language='de'`, id)
	for _, language := range []domain.Language{"en", "hr"} {
		if err := repo.RecordInstallation(ctx, Installation{MediaID: id, Language: language.String(), Path: "/media/Dune." + language.String() + ".srt", Checksum: "c", ProviderID: "p", CandidateID: "x", ScoreJSON: []byte(`{}`), SyncResultJSON: []byte(`{}`), MediaPath: "/media/Dune.mkv", MediaFileID: 1, MediaSize: 1, MediaModTimeNS: time.Unix(1, 0).UnixNano()}); err != nil {
			t.Fatal(err)
		}
	}
	states, err := repo.LanguageStates(ctx, id, now)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"en": "running", "hr": "installed", "de": "satisfied"}
	if len(states) != len(want) {
		t.Fatalf("states = %v", states)
	}
	for language, state := range want {
		if states[language] != state {
			t.Fatalf("states = %v, want %v", states, want)
		}
	}
	// An expired lease no longer counts as running.
	exec(`UPDATE search_states SET lease_until_ns=? WHERE media_id=? AND language='en'`, now.Add(-time.Minute).UnixNano(), id)
	if states, _ = repo.LanguageStates(ctx, id, now); states["en"] != "installed" {
		t.Fatalf("states = %v", states)
	}
}
