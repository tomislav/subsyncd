package store

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"subsyncd/internal/domain"
)

func TestSeriesDeleteRollsBackAllEpisodesAndAuditOnFailure(t *testing.T) {
	repo := openTestRepository(t)
	ctx := t.Context()
	now := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	if err := repo.EnsureInstance(ctx, "sonarr-main", "sonarr", "http://sonarr.invalid", now); err != nil {
		t.Fatal(err)
	}
	for i := int64(1); i <= 2; i++ {
		media := testMedia()
		media.EntityID = i
		media.SeriesID = 42
		media.Ref.FileID = i
		media.Fingerprint.FileID = i
		if _, err := repo.ApplyMediaEvent(ctx, MediaEventMutation{EventID: fmt.Sprint("seed", i), Type: "import", EntityID: i, Ref: media.Ref, Media: media, Languages: []domain.Language{"en"}, At: now}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := repo.store.db.Exec(`CREATE TRIGGER fail_second_delete BEFORE UPDATE OF deleted ON media WHEN NEW.entity_id=2 AND NEW.deleted=1 BEGIN SELECT RAISE(ABORT,'injected failure'); END`); err != nil {
		t.Fatal(err)
	}
	mutation := MediaEventMutation{EventID: "series-delete", Type: "delete", SeriesID: 42, Ref: domain.MediaRef{Instance: "sonarr-main", Kind: domain.MediaEpisode}, At: now}
	if _, err := repo.ApplyMediaEvent(ctx, mutation); err == nil {
		t.Fatal("expected transaction failure")
	}
	var deleted, audits, complete int
	for _, q := range []struct {
		sql  string
		dest *int
	}{
		{`SELECT count(*) FROM media WHERE deleted=1`, &deleted},
		{`SELECT count(*) FROM events WHERE event_id LIKE 'series-delete%'`, &audits},
		{`SELECT count(*) FROM search_states WHERE state='complete'`, &complete},
	} {
		if err := repo.store.db.QueryRow(q.sql).Scan(q.dest); err != nil {
			t.Fatal(err)
		}
	}
	if deleted != 0 || audits != 0 || complete != 0 {
		t.Fatalf("partial transaction: deleted=%d audits=%d complete=%d", deleted, audits, complete)
	}
	if _, err := repo.store.db.Exec(`DROP TRIGGER fail_second_delete`); err != nil {
		t.Fatal(err)
	}
	if applied, err := repo.ApplyMediaEvent(ctx, mutation); err != nil || !applied {
		t.Fatalf("retry=%t,%v", applied, err)
	}
	if err := repo.store.db.QueryRow(`SELECT count(*) FROM events WHERE event_id LIKE 'series-delete:episode:%' AND media_id IS NOT NULL AND file_id>0 AND entity_id>0`).Scan(&audits); err != nil || audits != 2 {
		t.Fatalf("child audits=%d,%v", audits, err)
	}
}

func TestSeriesMigrationPreservesLegacyMediaAndSearches(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	repo := db.Repository()
	now := time.Now().UTC()
	if err := repo.EnsureInstance(ctx, "sonarr-main", "sonarr", "http://sonarr.invalid", now); err != nil {
		t.Fatal(err)
	}
	media := testMedia()
	id, _, err := repo.UpsertMedia(ctx, media)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertSearchStateWithPriority(ctx, id, "en", now, SearchPriorityImport); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.Exec(`DROP INDEX media_series_idx; ALTER TABLE media DROP COLUMN series_id; ALTER TABLE events DROP COLUMN series_id; DELETE FROM schema_migrations WHERE version='004_sonarr_series.sql'`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repo = db.Repository()
	got, err := repo.GetMedia(ctx, id)
	if err != nil || got.SeriesID != 0 || got.EntityID != media.EntityID {
		t.Fatalf("migrated media=%#v,%v", got, err)
	}
	status, err := repo.GetSearchStatus(ctx, id, "en")
	if err != nil || status.State != "pending" || status.Priority != SearchPriorityImport {
		t.Fatalf("migrated search=%#v,%v", status, err)
	}
}
