package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func TestMigrationCopiesDueTimeIntoQueueOrder(t *testing.T) {
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
	var names []string
	for _, entry := range entries {
		if entry.Name() < "014_" {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	for _, name := range names {
		contents, err := migrationFiles.ReadFile("migrations/" + name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := old.Exec(string(contents)); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if _, err := old.Exec(`INSERT INTO schema_migrations(version) VALUES (?)`, name); err != nil {
			t.Fatal(err)
		}
	}
	for _, statement := range []string{
		`INSERT INTO media(id,instance,kind,entity_id,file_id,path,size,mod_time_ns,title,updated_at_ns) VALUES (1,'radarr','movie',1,11,'/media/movie.mkv',100,1,'Movie',1),(2,'sonarr','episode',2,22,'/media/episode.mkv',200,2,'Show',2)`,
		`INSERT INTO search_states(media_id,language,state,next_attempt_at_ns,priority) VALUES (1,'en','pending',500,200),(1,'hr','pending',100,300),(2,'en','complete',0,100),(2,'hr','pending',300,100)`,
	} {
		if _, err := old.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}

	migrated, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer migrated.Close()
	rows, err := migrated.db.Query(`SELECT next_attempt_at_ns, queue_order_ns FROM search_states ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		var next, order int64
		if err := rows.Scan(&next, &order); err != nil {
			t.Fatal(err)
		}
		if order != next {
			t.Errorf("queue_order_ns = %d, want copied due time %d", order, next)
		}
		count++
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if count != 4 {
		t.Fatalf("rows = %d, want 4", count)
	}
	var index string
	if err := migrated.db.QueryRow(`SELECT sql FROM sqlite_master WHERE type='index' AND name='search_due_idx'`).Scan(&index); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(index, "instance_rank") || !strings.Contains(index, "queue_order_ns") {
		t.Fatalf("search_due_idx = %q, want instance_rank and queue_order_ns", index)
	}
}
