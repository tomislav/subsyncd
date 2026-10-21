package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// Installation-time failures of LAPSE output used to be stored as
// partial_coverage or invalid_subtitle, which now survive LAPSE upgrades. Old
// rows cannot say which ones LAPSE caused, so the migration reconsiders every
// system rejection with those two reasons once and keeps everything else.
func TestMigrationReopensOwnContentRejectionsOnce(t *testing.T) {
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
	entries, err := migrationFiles.ReadDir("migrations")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() >= "025" {
			continue
		}
		contents, err := migrationFiles.ReadFile("migrations/" + entry.Name())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := old.Exec(string(contents)); err != nil {
			t.Fatalf("%s: %v", entry.Name(), err)
		}
		if _, err := old.Exec(`INSERT INTO schema_migrations(version) VALUES (?)`, entry.Name()); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := old.Exec(`INSERT INTO media(id,instance,kind,entity_id,file_id,path,size,mod_time_ns,title,updated_at_ns) VALUES (1,'sonarr','episode',1,11,'/media/episode.mkv',100,1,'Show',1)`); err != nil {
		t.Fatal(err)
	}
	insertRejection := func(db *sql.DB, result, signature, reason string) {
		t.Helper()
		if _, err := db.Exec(`INSERT INTO candidate_rejections(media_id,language,provider_id,result_id,candidate_signature,reason_code,tool_signature,media_path,media_file_id,media_size,media_mod_time_ns,rejected_at_ns,expires_at_ns) VALUES (1,'en','provider',?,?,?,'lapse-2.2.4/policy-a','/media/episode.mkv',11,100,1,1,0)`, result, signature, reason); err != nil {
			t.Fatal(err)
		}
	}
	insertRejection(old, "coverage", "signature", "partial_coverage")
	insertRejection(old, "content", "signature/subtitle-rules-v2", "invalid_subtitle")
	for _, reason := range []string{"language_mismatch", "lapse_unsure", "lapse_nothing", "lapse_invalid_output", "pack_selection", "user_out_of_sync"} {
		insertRejection(old, reason, "signature", reason)
	}
	reasons := func(db *sql.DB) []string {
		t.Helper()
		rows, err := db.Query(`SELECT reason_code FROM candidate_rejections ORDER BY reason_code`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var codes []string
		for rows.Next() {
			var code string
			if err := rows.Scan(&code); err != nil {
				t.Fatal(err)
			}
			codes = append(codes, code)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return codes
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}

	migrated, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"language_mismatch", "lapse_invalid_output", "lapse_nothing", "lapse_unsure", "pack_selection", "user_out_of_sync"}
	if got := reasons(migrated.db); !reflect.DeepEqual(got, want) {
		t.Fatalf("rejections after migrating = %v, want %v", got, want)
	}
	// A rejection recorded after the upgrade is kept: the migration runs once.
	insertRejection(migrated.db, "later", "signature", "partial_coverage")
	if err := migrated.Close(); err != nil {
		t.Fatal(err)
	}
	migrated, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer migrated.Close()
	if got := strings.Join(reasons(migrated.db), ","); !strings.Contains(got, "partial_coverage") {
		t.Fatalf("reopening cleared a new rejection: %s", got)
	}
}
