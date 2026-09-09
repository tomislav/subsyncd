package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

func TestForcedTitleMigrationInvalidatesProbeOnceWithoutChangingInventoryOrWork(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	old, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = old.Close() })
	if _, err := old.Exec(`CREATE TABLE schema_migrations(version TEXT PRIMARY KEY, applied_at TEXT DEFAULT CURRENT_TIMESTAMP)`); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"001_baseline.sql", "002_scrub_provider_credentials.sql", "003_inventory_probes.sql", "004_sonarr_series.sql", "005_library_discovery.sql", "006_fallback_installations.sql", "007_clear_lapse_invalid_output.sql"} {
		content, err := migrationFiles.ReadFile("migrations/" + name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := old.Exec(string(content)); err != nil {
			t.Fatal(err)
		}
		if _, err := old.Exec(`INSERT INTO schema_migrations(version) VALUES (?)`, name); err != nil {
			t.Fatal(err)
		}
	}
	for _, statement := range []string{
		`INSERT INTO media(id,instance,kind,entity_id,file_id,path,size,mod_time_ns,title,updated_at_ns) VALUES (1,'radarr','movie',1,11,'/media/movie.mkv',100,1,'Movie',1)`,
		`INSERT INTO tracks(media_id,language,embedded,forced,is_default,sdh,protected) VALUES (1,'en',1,0,0,0,0)`,
		`INSERT INTO inventory_probes VALUES (1,'/media/movie.mkv',11,100,1)`,
		`INSERT INTO search_states(media_id,language,state,attempt,failure_attempt,next_attempt_at_ns,lease_owner,lease_until_ns) VALUES (1,'en','pending',3,2,100,'worker',200)`,
		`INSERT INTO installations(media_id,language,path,checksum,installed_at_ns) VALUES (1,'en','/media/movie.en.srt','checksum',1)`,
	} {
		if _, err := old.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	got, err := db.Repository().GetTrackInventory(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if got.ProbeFingerprint != nil {
		t.Fatal("old completed probe survived; stale forced classification can still satisfy coverage")
	}
	if len(got.Tracks) != 1 || got.Tracks[0].Forced || got.CatalogFingerprint.FileID != 11 {
		t.Fatalf("migration changed inventory: %#v", got)
	}
	var count int
	for _, query := range []string{
		`SELECT count(*) FROM search_states WHERE media_id=1 AND attempt=3 AND failure_attempt=2 AND next_attempt_at_ns=100 AND lease_owner='worker' AND lease_until_ns=200`,
		`SELECT count(*) FROM installations WHERE media_id=1 AND checksum='checksum'`,
	} {
		if err := db.db.QueryRow(query).Scan(&count); err != nil || count != 1 {
			t.Fatalf("retained state changed: count=%d err=%v", count, err)
		}
	}
	if _, err := db.db.Exec(`INSERT INTO inventory_probes VALUES (1,'/media/movie.mkv',11,100,1)`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	got, err = db.Repository().GetTrackInventory(ctx, 1)
	if err != nil || got.ProbeFingerprint == nil {
		t.Fatalf("new completed probe did not survive restart: %#v %v", got, err)
	}
}
