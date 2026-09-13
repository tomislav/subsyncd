package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

func TestMigrationReschedulesOnlyUnfinishedNoResultBackfill(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "subsyncd.db")
	old, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	if _, err := old.Exec(`CREATE TABLE schema_migrations (version TEXT PRIMARY KEY, applied_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP)`); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"001_baseline.sql", "002_scrub_provider_credentials.sql", "003_inventory_probes.sql", "004_sonarr_series.sql", "005_library_discovery.sql", "006_fallback_installations.sql", "007_clear_lapse_invalid_output.sql", "008_forced_track_probe_refresh.sql", "009_movie_duplicate_rejections.sql", "010_reconciliation_replays.sql"} {
		contents, err := migrationFiles.ReadFile("migrations/" + name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := old.Exec(string(contents)); err != nil {
			t.Fatal(err)
		}
		if _, err := old.Exec(`INSERT INTO schema_migrations(version) VALUES (?)`, name); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := old.Exec(`
		INSERT INTO media(id,instance,kind,entity_id,file_id,path,size,mod_time_ns,title,updated_at_ns,deleted)
		VALUES (1,'radarr','movie',1,11,'/media/one.mkv',100,1,'One',1,0),
		       (2,'radarr','movie',2,22,'/media/two.mkv',100,1,'Two',1,0),
		       (3,'radarr','movie',3,33,'/media/three.mkv',100,1,'Three',1,0),
		       (4,'radarr','movie',4,44,'/media/four.mkv',100,1,'Four',1,0),
		       (5,'radarr','movie',5,55,'/media/five.mkv',100,1,'Five',1,1);
		INSERT INTO search_states(media_id,language,state,attempt,failure_attempt,next_attempt_at_ns,last_outcome,priority,lease_owner,lease_until_ns)
		VALUES (1,'en','pending',4,2,900,'no_result',200,'old-worker',950),
		       (2,'en','pending',4,2,901,'rejected',200,NULL,NULL),
		       (3,'en','pending',4,2,902,'no_result',100,NULL,NULL),
		       (4,'en','pending',4,2,903,'no_result',200,NULL,NULL),
		       (5,'en','pending',4,2,904,'no_result',200,NULL,NULL);
		INSERT INTO installations(media_id,language,path,checksum,installed_at_ns)
		VALUES (4,'en','/media/four.en.srt','checksum',1);
	`); err != nil {
		t.Fatal(err)
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}

	migrated, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer migrated.Close()
	rows, err := migrated.db.Query(`SELECT media_id,next_attempt_at_ns,attempt,failure_attempt,COALESCE(lease_owner,''),COALESCE(lease_until_ns,0) FROM search_states ORDER BY media_id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	wantNext := []int64{0, 901, 902, 903, 904}
	for i := 0; rows.Next(); i++ {
		var mediaID, next int64
		var attempt, failureAttempt int
		var leaseOwner string
		var leaseUntil int64
		if err := rows.Scan(&mediaID, &next, &attempt, &failureAttempt, &leaseOwner, &leaseUntil); err != nil {
			t.Fatal(err)
		}
		if mediaID != int64(i+1) || next != wantNext[i] || attempt != 4 || failureAttempt != 2 {
			t.Fatalf("row %d = media=%d next=%d attempts=%d/%d", i, mediaID, next, attempt, failureAttempt)
		}
		if i == 0 && (leaseOwner != "old-worker" || leaseUntil != 950) {
			t.Fatalf("leased row changed lease = %q/%d", leaseOwner, leaseUntil)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
}
