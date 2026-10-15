package store

import (
	"testing"
	"time"

	"subsyncd/internal/domain"
)

func TestQueueCountsByInstanceLanguageAndPriority(t *testing.T) {
	repo := openTestRepository(t)
	ctx := t.Context()
	now := missingNow
	// radarr: en due now and in an hour, hr an upgrade in a day.
	addMissingMedia(t, repo, 1, "Dune", now.Add(-time.Minute), SearchPriorityMissing, "en")
	heat := addMissingMedia(t, repo, 2, "Heat", now.Add(time.Hour), SearchPriorityMissing, "en")
	if err := repo.UpsertSearchStateWithPriority(ctx, heat, "hr", now.Add(24*time.Hour), SearchPriorityUpgrade); err != nil {
		t.Fatal(err)
	}
	// A running search is pending but neither due nor the next attempt.
	running := addMatchMedia(t, repo, "sonarr", domain.MediaEpisode, 3, "Show", nil)
	if err := repo.UpsertSearchStateWithPriority(ctx, running, "en", now.Add(-time.Hour), SearchPriorityImport); err != nil {
		t.Fatal(err)
	}
	if leases, err := repo.LeaseDueSearches(ctx, now.Add(-30*time.Minute), 1, 2*time.Hour); err != nil || len(leases) != 1 {
		t.Fatalf("leases = %+v, %v", leases, err)
	}
	// Deleted media and finished searches do not count.
	gone := addMissingMedia(t, repo, 4, "Gone", now, SearchPriorityMissing, "en")
	// Heat and the deleted Gone have an English installation.
	for _, media := range []struct {
		id, fileID int64
		title      string
	}{{heat, 2, "Heat"}, {gone, 4, "Gone"}} {
		if err := repo.RecordInstallation(ctx, Installation{MediaID: media.id, Language: "en", Path: "/media/" + media.title + ".en.srt", Checksum: "x", ProviderID: "alpha", CandidateID: "1", MediaPath: "/media/" + media.title + ".mkv", MediaFileID: media.fileID, MediaSize: 1, MediaModTimeNS: time.Unix(1, 0).UnixNano(), ScoreJSON: []byte("{}"), SyncResultJSON: []byte("{}")}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := repo.store.db.ExecContext(ctx, `UPDATE media SET deleted=1 WHERE id=?`, gone); err != nil {
		t.Fatal(err)
	}
	done := addMissingMedia(t, repo, 5, "Done", now, SearchPriorityMissing, "en")
	setLastOutcome(t, repo, done, "en", "no_result")

	counts, err := repo.QueueCounts(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	want := []QueueCount{
		{Instance: "radarr", Language: "en", Priority: SearchPriorityMissing, Total: 2, Due: 1, NextAt: now.Add(-time.Minute)},
		{Instance: "radarr", Language: "hr", Priority: SearchPriorityUpgrade, Total: 1, NextAt: now.Add(24 * time.Hour)},
		{Instance: "sonarr", Language: "en", Priority: SearchPriorityImport, Total: 1},
	}
	if len(counts) != len(want) {
		t.Fatalf("counts = %+v", counts)
	}
	for i, count := range counts {
		if count.Instance != want[i].Instance || count.Language != want[i].Language || count.Priority != want[i].Priority || count.Total != want[i].Total || count.Due != want[i].Due || !count.NextAt.Equal(want[i].NextAt) {
			t.Fatalf("count %d = %+v, want %+v", i, count, want[i])
		}
	}

	snapshot, err := repo.Coverage(ctx, MissingQuery{Languages: []domain.Language{"en", "hr"}, Now: now, Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Media) != 2 || snapshot.Media["radarr"] != 3 || snapshot.Media["sonarr"] != 1 {
		t.Fatalf("media = %v", snapshot.Media)
	}
	if len(snapshot.Installed) != 1 || snapshot.Installed[InstanceLanguage{Instance: "radarr", Language: "en"}] != 1 {
		t.Fatalf("installed = %v", snapshot.Installed)
	}
	// Coverage ignores Limit and lists the same rows as an unlimited
	// ListMissing.
	listed, total, err := repo.ListMissing(ctx, MissingQuery{Languages: []domain.Language{"en", "hr"}, Now: now})
	if err != nil || len(snapshot.Missing) != total || len(listed) != total || total == 1 {
		t.Fatalf("snapshot missing = %d, listed = %d (total %d), %v", len(snapshot.Missing), len(listed), total, err)
	}
	// A language filter narrows the missing rows, not the media counts.
	hr, err := repo.Coverage(ctx, MissingQuery{Languages: []domain.Language{"en", "hr"}, Language: "hr", Now: now})
	if err != nil || len(hr.Media) != 2 {
		t.Fatalf("hr = %+v, %v", hr, err)
	}
	for _, row := range hr.Missing {
		if row.Language != "hr" {
			t.Fatalf("hr row = %+v", row)
		}
	}
	// With no language there are no missing rows, and no malformed query.
	if none, err := repo.Coverage(ctx, MissingQuery{Now: now}); err != nil || len(none.Missing) != 0 || len(none.Media) != 2 {
		t.Fatalf("none = %+v, %v", none, err)
	}
}
