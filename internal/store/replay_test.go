package store

import (
	"testing"
	"time"

	"subsyncd/internal/domain"
)

// replayFixture applies a webhook import of testMedia, then marks its
// searches finished as the worker would after installing.
func replayFixture(t *testing.T) (*Repository, domain.Media, time.Time) {
	t.Helper()
	repo := openTestRepository(t)
	at := time.Date(2026, 10, 9, 10, 16, 17, 0, time.UTC)
	media := testMedia()
	webhook := MediaEventMutation{EventID: "webhook-1", Type: "import", EntityID: media.EntityID, Media: media, Ref: media.Ref, Languages: []domain.Language{"hr", "en"}, At: at}
	if applied, err := repo.ApplyMediaEvent(t.Context(), webhook); err != nil || !applied {
		t.Fatalf("webhook import = %v, %v", applied, err)
	}
	if _, err := repo.store.db.Exec(`UPDATE search_states SET state='complete', last_outcome='installed', priority=?, next_attempt_at_ns=?`, SearchPriorityUpgrade, at.Add(30*24*time.Hour).UnixNano()); err != nil {
		t.Fatal(err)
	}
	return repo, media, at
}

func replayOf(media domain.Media, at time.Time) MediaEventMutation {
	return MediaEventMutation{EventID: "reconcile:sonarr-main:60743", Type: "import", EntityID: media.EntityID, Media: media, Ref: media.Ref, Languages: []domain.Language{"hr", "en"}, At: at, Priority: SearchPriorityMissing, Replay: true}
}

func reopenedSearches(t *testing.T, repo *Repository) int {
	t.Helper()
	var reopened int
	if err := repo.store.db.QueryRow(`SELECT count(*) FROM search_states WHERE state='pending' AND last_outcome=''`).Scan(&reopened); err != nil {
		t.Fatal(err)
	}
	return reopened
}

func TestReplayOfAppliedImportForUnchangedFileKeepsSearches(t *testing.T) {
	repo, media, at := replayFixture(t)
	applied, err := repo.ApplyMediaEvent(t.Context(), replayOf(media, at))
	if err != nil || !applied {
		t.Fatalf("replay = %v, %v; want recorded", applied, err)
	}
	if got := reopenedSearches(t, repo); got != 0 {
		t.Fatalf("reopened searches = %d, want 0 for a replay of an applied import", got)
	}
	var linked int
	if err := repo.store.db.QueryRow(`SELECT count(*) FROM events WHERE event_id='reconcile:sonarr-main:60743' AND media_id IS NOT NULL`).Scan(&linked); err != nil || linked != 1 {
		t.Fatalf("linked replay events = %d, %v; want 1", linked, err)
	}
}

func TestReplayReopensSearchesWhenSomethingChanged(t *testing.T) {
	tests := []struct {
		name   string
		change func(*domain.Media)
	}{
		{"new file", func(m *domain.Media) {
			m.Ref.FileID, m.Fingerprint.FileID, m.Fingerprint.Size = 43, 43, 200
		}},
		{"new size", func(m *domain.Media) { m.Fingerprint.Size = 200 }},
		{"missed rename", func(m *domain.Media) { m.Fingerprint.Path = "/media/renamed.mkv" }},
		{"now unsupported", func(m *domain.Media) { m.UnsupportedReason = domain.UnsupportedMultiEpisode }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			repo, media, at := replayFixture(t)
			test.change(&media)
			replay := replayOf(media, at)
			if applied, err := repo.ApplyMediaEvent(t.Context(), replay); err != nil || !applied {
				t.Fatalf("replay = %v, %v", applied, err)
			}
			if got := changedSearches(t, repo); got != 2 {
				t.Fatalf("changed searches = %d, want 2", got)
			}
		})
	}
}

// changedSearches counts searches no longer in the fixture's finished state.
func changedSearches(t *testing.T, repo *Repository) int {
	t.Helper()
	var changed int
	if err := repo.store.db.QueryRow(`SELECT count(*) FROM search_states WHERE last_outcome<>'installed'`).Scan(&changed); err != nil {
		t.Fatal(err)
	}
	return changed
}

func TestReplayNewerThanTheLastAppliedChangeReopensSearches(t *testing.T) {
	repo, media, at := replayFixture(t)
	if applied, err := repo.ApplyMediaEvent(t.Context(), replayOf(media, at.Add(time.Hour))); err != nil || !applied {
		t.Fatalf("replay = %v, %v", applied, err)
	}
	if got := reopenedSearches(t, repo); got != 2 {
		t.Fatalf("reopened searches = %d, want 2 for history newer than the last applied change", got)
	}
}

func TestReplayOfDeletedMediaReopensSearches(t *testing.T) {
	repo, media, at := replayFixture(t)
	if _, err := repo.store.db.Exec(`UPDATE media SET deleted=1`); err != nil {
		t.Fatal(err)
	}
	if applied, err := repo.ApplyMediaEvent(t.Context(), replayOf(media, at)); err != nil || !applied {
		t.Fatalf("replay = %v, %v", applied, err)
	}
	if got := reopenedSearches(t, repo); got != 2 {
		t.Fatalf("reopened searches = %d, want 2 for a re-added file", got)
	}
}

func TestReplayedRenameOfAppliedStateKeepsSearches(t *testing.T) {
	repo, media, at := replayFixture(t)
	rename := replayOf(media, at)
	rename.Type = "rename"
	if applied, err := repo.ApplyMediaEvent(t.Context(), rename); err != nil || !applied {
		t.Fatalf("replay = %v, %v", applied, err)
	}
	if got := reopenedSearches(t, repo); got != 0 {
		t.Fatalf("reopened searches = %d, want 0", got)
	}
}

func TestRepeatedWebhookImportStillReopensSearches(t *testing.T) {
	repo, media, at := replayFixture(t)
	again := replayOf(media, at.Add(time.Minute))
	again.EventID, again.Replay, again.Priority = "webhook-2", false, 0
	if applied, err := repo.ApplyMediaEvent(t.Context(), again); err != nil || !applied {
		t.Fatalf("second webhook = %v, %v", applied, err)
	}
	if got := reopenedSearches(t, repo); got != 2 {
		t.Fatalf("reopened searches = %d, want 2", got)
	}
}

func TestConsecutiveReplaysOfAppliedChangesKeepSearches(t *testing.T) {
	repo, media, at := replayFixture(t)
	older := replayOf(media, at.Add(-2*time.Minute))
	older.EventID = "reconcile:sonarr-main:60740"
	newer := replayOf(media, at.Add(-time.Minute))
	newer.Type = "rename"
	for _, replay := range []MediaEventMutation{older, newer} {
		if applied, err := repo.ApplyMediaEvent(t.Context(), replay); err != nil || !applied {
			t.Fatalf("replay %s = %v, %v", replay.EventID, applied, err)
		}
	}
	if got := reopenedSearches(t, repo); got != 0 {
		t.Fatalf("reopened searches = %d, want 0: a skipped replay must not move the media's update time back", got)
	}
}
