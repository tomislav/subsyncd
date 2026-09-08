package store

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"
)

func TestMigrationClearsExistingInvalidLapseOutputRejectionsOnce(t *testing.T) {
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
	for _, name := range []string{"001_baseline.sql", "002_scrub_provider_credentials.sql", "003_inventory_probes.sql", "004_sonarr_series.sql", "005_library_discovery.sql", "006_fallback_installations.sql"} {
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
	for _, statement := range []string{
		`INSERT INTO media(id,instance,kind,entity_id,file_id,path,size,mod_time_ns,title,updated_at_ns) VALUES (1,'radarr','movie',1,11,'/media/movie.mkv',100,1,'Movie',1),(2,'sonarr','episode',2,22,'/media/episode.mkv',200,2,'Show',2)`,
		`INSERT INTO search_states(media_id,language,state,attempt,failure_attempt,next_attempt_at_ns,lease_owner,lease_until_ns,priority) VALUES (1,'en','pending',3,2,100,'worker',200,300),(2,'hr','complete',4,1,300,NULL,NULL,100)`,
		`INSERT INTO installations(media_id,language,path,checksum,installed_at_ns,fallback) VALUES (1,'en','/media/movie.en.srt','checksum',1,1)`,
		`INSERT INTO provider_cache VALUES ('cache','provider','[]',999)`,
		`INSERT INTO pack_cache(provider_id,result_id,series_key,season,language,content_checksum,manifest_path,byte_size,expires_at_ns,last_access_at_ns) VALUES ('provider','pack','series',1,'hr','checksum','/cache/manifest',10,999,1)`,
		`INSERT INTO pack_members(pack_id,safe_name,cache_path,checksum) VALUES (1,'episode.srt','/cache/episode.srt','checksum')`,
		`INSERT INTO notifications(notifier,payload_json,next_attempt_at_ns,dedupe_key) VALUES ('silo','{}',123,'dedupe')`,
	} {
		if _, err := old.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	insertRejection := func(db *sql.DB, id int, reason string) {
		t.Helper()
		if _, err := db.Exec(`INSERT INTO candidate_rejections(media_id,language,provider_id,result_id,candidate_signature,reason_code,tool_signature,media_path,media_file_id,media_size,media_mod_time_ns,rejected_at_ns,expires_at_ns) VALUES (? ,?,'provider',?,'signature',?,'tool','/media/fixture.mkv',1,100,1,1,0)`, 1+id%2, []string{"en", "hr"}[id%2], fmt.Sprint(id), reason); err != nil {
			t.Fatal(err)
		}
	}
	for i, reason := range []string{"lapse_invalid_output", "lapse_invalid_output", "lapse_unsure", "lapse_nothing", "pack_selection", "invalid_subtitle", "oversized_payload"} {
		insertRejection(old, i, reason)
	}
	snapshot := func(db *sql.DB, query string) [][]any {
		t.Helper()
		rows, err := db.Query(query)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		columns, err := rows.Columns()
		if err != nil {
			t.Fatal(err)
		}
		var result [][]any
		for rows.Next() {
			values := make([]any, len(columns))
			pointers := make([]any, len(columns))
			for i := range values {
				pointers[i] = &values[i]
			}
			if err := rows.Scan(pointers...); err != nil {
				t.Fatal(err)
			}
			result = append(result, values)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return result
	}
	queries := []string{`SELECT * FROM candidate_rejections WHERE reason_code <> 'lapse_invalid_output' ORDER BY id`}
	for _, table := range []string{"media", "search_states", "installations", "provider_cache", "pack_cache", "pack_members", "notifications"} {
		queries = append(queries, "SELECT * FROM "+table+" ORDER BY 1")
	}
	before := make([][][]any, len(queries))
	for i, q := range queries {
		before[i] = snapshot(old, q)
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	migrated, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { migrated.Close() }()
	var count int
	if err := migrated.db.QueryRow(`SELECT count(*) FROM candidate_rejections WHERE reason_code='lapse_invalid_output'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("existing invalid-output rejections = %d, want 0", count)
	}
	for i, q := range queries {
		if after := snapshot(migrated.db, q); !reflect.DeepEqual(before[i], after) {
			t.Errorf("migration changed retained state for %s: before=%v after=%v", q, before[i], after)
		}
	}
	insertRejection(migrated.db, 10, "lapse_invalid_output")
	retained := snapshot(migrated.db, `SELECT * FROM candidate_rejections ORDER BY id`)
	if err := migrated.Close(); err != nil {
		t.Fatal(err)
	}
	migrated, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if after := snapshot(migrated.db, `SELECT * FROM candidate_rejections ORDER BY id`); !reflect.DeepEqual(retained, after) {
		t.Fatalf("reopening cleared new rejections: before=%v after=%v", retained, after)
	}
}
