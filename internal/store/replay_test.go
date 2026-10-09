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
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			repo, media, at := replayFixture(t)
			test.change(&media)
			if applied, err := repo.ApplyMediaEvent(t.Context(), replayOf(media, at)); err != nil || !applied {
				t.Fatalf("replay = %v, %v", applied, err)
			}
			if got := reopenedSearches(t, repo); got != 2 {
				t.Fatalf("reopened searches = %d, want 2", got)
			}
		})
	}
}

func TestReplayReopensSearchesWithoutAnEarlierImportEvent(t *testing.T) {
	repo := openTestRepository(t)
	media := testMedia()
	if _, _, err := repo.UpsertMedia(t.Context(), media); err != nil {
		t.Fatal(err)
	}
	if applied, err := repo.ApplyMediaEvent(t.Context(), replayOf(media, time.Date(2026, 10, 9, 10, 16, 17, 0, time.UTC))); err != nil || !applied {
		t.Fatalf("replay = %v, %v", applied, err)
	}
	if got := reopenedSearches(t, repo); got != 2 {
		t.Fatalf("reopened searches = %d, want 2", got)
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
