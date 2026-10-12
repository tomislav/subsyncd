package store

import (
	"testing"
	"time"

	"subsyncd/internal/domain"
)

func historyDetail() *SearchHistoryDetail {
	return &SearchHistoryDetail{MediaTitle: "Show - S01E02", ProviderID: "alpha", ReleaseName: "Show.S01E02.1080p"}
}

func TestCompleteSearchWritesHistoryAtomically(t *testing.T) {
	repo := openTestRepository(t)
	ctx := t.Context()
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	mediaID, _, err := repo.UpsertMedia(ctx, testMedia())
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertSearchStateWithPriority(ctx, mediaID, "en", now, SearchPriorityMissing); err != nil {
		t.Fatal(err)
	}
	lease := leaseOne(t, repo, now)
	if _, err := repo.CompleteSearch(ctx, SearchCompletion{JobID: "not-the-owner", Outcome: "installed", History: historyDetail()}); err == nil {
		t.Fatal("completion by a non-owner succeeded")
	}
	if entries, err := repo.ListHistory(ctx, HistoryQuery{}); err != nil || len(entries) != 0 {
		t.Fatalf("failed guard wrote history: %+v/%v", entries, err)
	}
	if _, err := repo.CompleteSearch(ctx, SearchCompletion{JobID: lease.JobID, Outcome: "installed", History: historyDetail()}); err != nil {
		t.Fatal(err)
	}
	entries, err := repo.ListHistory(ctx, HistoryQuery{})
	if err != nil || len(entries) != 1 {
		t.Fatalf("history = %+v/%v", entries, err)
	}
	got := entries[0]
	if got.MediaID != mediaID || got.Ref != testMedia().Ref || got.Language != "en" || got.Outcome != "installed" || got.ProviderID != "alpha" || got.ReleaseName != "Show.S01E02.1080p" || got.MediaTitle != "Show - S01E02" || got.Manual || got.FinishedAt.IsZero() {
		t.Fatalf("entry = %+v", got)
	}
}

func TestCompleteSearchDeletedOutcomeWritesNoHistory(t *testing.T) {
	repo := openTestRepository(t)
	ctx := t.Context()
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	mediaID, _, err := repo.UpsertMedia(ctx, testMedia())
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertSearchStateWithPriority(ctx, mediaID, "en", now, SearchPriorityMissing); err != nil {
		t.Fatal(err)
	}
	lease := leaseOne(t, repo, now)
	if _, err := repo.store.db.ExecContext(ctx, `UPDATE media SET deleted=1 WHERE id=?`, mediaID); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.CompleteSearch(ctx, SearchCompletion{JobID: lease.JobID, Outcome: "installed", History: historyDetail()}); err != nil {
		t.Fatal(err)
	}
	if entries, err := repo.ListHistory(ctx, HistoryQuery{}); err != nil || len(entries) != 0 {
		t.Fatalf("deleted completion wrote history: %+v/%v", entries, err)
	}
}

func TestInstallationRecordsCandidateSignatureAndArtifact(t *testing.T) {
	repo := openTestRepository(t)
	ctx := t.Context()
	media := testMedia()
	id, _, err := repo.UpsertMedia(ctx, media)
	if err != nil {
		t.Fatal(err)
	}
	installation := Installation{MediaID: id, Language: "en", Path: "/media/x.en.srt", Checksum: "sum", ProviderID: "alpha", CandidateID: "one", CandidateSignature: "sig-1", ArtifactChecksum: "artifact-1", ScoreJSON: []byte(`{}`), SyncResultJSON: []byte(`{}`), MediaPath: media.Fingerprint.Path, MediaFileID: media.Fingerprint.FileID, MediaSize: media.Fingerprint.Size, MediaModTimeNS: media.Fingerprint.ModTime.UnixNano()}
	if err := repo.RecordInstallation(ctx, installation); err != nil {
		t.Fatal(err)
	}
	got, found, err := repo.GetInstallation(ctx, id, "en")
	if err != nil || !found || got.CandidateSignature != "sig-1" || got.ArtifactChecksum != "artifact-1" {
		t.Fatalf("installation = %+v/%v/%v", got, found, err)
	}
}

func recordHistory(t *testing.T, repo *Repository, mediaID int64, language domain.Language, outcome string, manual bool, at time.Time) {
	t.Helper()
	detail := historyDetail()
	detail.Manual = manual
	if err := repo.RecordSearchHistory(t.Context(), mediaID, language, outcome, *detail, at); err != nil {
		t.Fatal(err)
	}
}

func TestPruneHistoryRemovesOnlyOlderThanCutoff(t *testing.T) {
	repo := openTestRepository(t)
	ctx := t.Context()
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	mediaID, _, err := repo.UpsertMedia(ctx, testMedia())
	if err != nil {
		t.Fatal(err)
	}
	recordHistory(t, repo, mediaID, "en", "no_result", false, now.Add(-29*24*time.Hour))
	recordHistory(t, repo, mediaID, "en", "no_result", false, now.Add(-31*24*time.Hour))
	removed, err := repo.PruneHistory(ctx, now.Add(-30*24*time.Hour))
	if err != nil || removed != 1 {
		t.Fatalf("pruned %d/%v", removed, err)
	}
	if entries, err := repo.ListHistory(ctx, HistoryQuery{}); err != nil || len(entries) != 1 {
		t.Fatalf("remaining = %+v/%v", entries, err)
	}
}

func TestListHistoryFiltersAndOrders(t *testing.T) {
	repo := openTestRepository(t)
	ctx := t.Context()
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	mediaID, _, err := repo.UpsertMedia(ctx, testMedia())
	if err != nil {
		t.Fatal(err)
	}
	other := testMedia()
	other.Ref = domain.MediaRef{Instance: "radarr-main", Kind: domain.MediaMovie, FileID: 7}
	other.Fingerprint.Path, other.Fingerprint.FileID = "/media/movie.mkv", 7
	other.EntityID, other.Title = 202, "Movie"
	otherID, _, err := repo.UpsertMedia(ctx, other)
	if err != nil {
		t.Fatal(err)
	}
	recordHistory(t, repo, mediaID, "en", "no_result", false, now.Add(-3*time.Hour))
	recordHistory(t, repo, mediaID, "hr", "installed", true, now.Add(-2*time.Hour))
	recordHistory(t, repo, otherID, "en", "rejected", false, now.Add(-time.Hour))

	all, err := repo.ListHistory(ctx, HistoryQuery{})
	if err != nil || len(all) != 3 || all[0].Outcome != "rejected" || all[1].Outcome != "installed" || all[2].Outcome != "no_result" || !all[1].Manual {
		t.Fatalf("all = %+v/%v", all, err)
	}
	if got, err := repo.ListHistory(ctx, HistoryQuery{Since: now.Add(-150 * time.Minute)}); err != nil || len(got) != 2 {
		t.Fatalf("since = %+v/%v", got, err)
	}
	if got, err := repo.ListHistory(ctx, HistoryQuery{Instance: "radarr-main"}); err != nil || len(got) != 1 || got[0].Ref.Kind != domain.MediaMovie {
		t.Fatalf("instance = %+v/%v", got, err)
	}
	if got, err := repo.ListHistory(ctx, HistoryQuery{Language: "en"}); err != nil || len(got) != 2 {
		t.Fatalf("language = %+v/%v", got, err)
	}
	if got, err := repo.ListHistory(ctx, HistoryQuery{Limit: 1}); err != nil || len(got) != 1 || got[0].Outcome != "rejected" {
		t.Fatalf("limit = %+v/%v", got, err)
	}
}

func TestProviderHistoryCounts(t *testing.T) {
	repo := openTestRepository(t)
	ctx := t.Context()
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	mediaID, _, err := repo.UpsertMedia(ctx, testMedia())
	if err != nil {
		t.Fatal(err)
	}
	recordHistory(t, repo, mediaID, "en", "installed", false, now.Add(-time.Hour))
	recordHistory(t, repo, mediaID, "en", "rejected", false, now.Add(-time.Hour))
	recordHistory(t, repo, mediaID, "en", "no_result", false, now.Add(-time.Hour))
	recordHistory(t, repo, mediaID, "en", "installed", false, now.Add(-48*time.Hour))
	counts, err := repo.ProviderHistoryCounts(ctx, now.Add(-24*time.Hour))
	if err != nil || counts["alpha"] != (ProviderCounts{Installed: 1, NoResult: 1, Rejected: 1}) {
		t.Fatalf("counts = %+v/%v", counts, err)
	}
}

func TestHistorySurvivesMediaDeletion(t *testing.T) {
	repo := openTestRepository(t)
	ctx := t.Context()
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	mediaID, _, err := repo.UpsertMedia(ctx, testMedia())
	if err != nil {
		t.Fatal(err)
	}
	recordHistory(t, repo, mediaID, "en", "installed", false, now)
	if _, err := repo.store.db.ExecContext(ctx, `DELETE FROM media WHERE id=?`, mediaID); err != nil {
		t.Fatal(err)
	}
	entries, err := repo.ListHistory(ctx, HistoryQuery{})
	if err != nil || len(entries) != 1 || entries[0].MediaID != 0 || entries[0].MediaTitle != "Show - S01E02" || entries[0].Ref.FileID != 42 {
		t.Fatalf("entries = %+v/%v", entries, err)
	}
}

func TestCompleteSearchHistoryUsesCompletionTime(t *testing.T) {
	repo := openTestRepository(t)
	ctx := t.Context()
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	mediaID, _, err := repo.UpsertMedia(ctx, testMedia())
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertSearchStateWithPriority(ctx, mediaID, "en", now, SearchPriorityMissing); err != nil {
		t.Fatal(err)
	}
	lease := leaseOne(t, repo, now)
	if _, err := repo.CompleteSearch(ctx, SearchCompletion{JobID: lease.JobID, Outcome: "no_result", History: historyDetail(), FinishedAt: now.Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	entries, err := repo.ListHistory(ctx, HistoryQuery{})
	if err != nil || len(entries) != 1 || !entries[0].FinishedAt.Equal(now.Add(time.Minute)) {
		t.Fatalf("entries = %+v/%v", entries, err)
	}
}
