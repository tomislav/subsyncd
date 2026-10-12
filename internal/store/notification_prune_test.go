package store

import (
	"context"
	"database/sql"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// Finished intents (delivered, skipped, refused, permanently failed) older
// than the cutoff go, in bounded batches, oldest first; recent finished ones
// and pending, retry-scheduled and leased ones stay.
func TestPruneFinishedNotificationsDeletesOnlyOldFinishedRows(t *testing.T) {
	ctx := context.Background()
	repo := openTestRepository(t)
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	old, recent := now.Add(-31*24*time.Hour), now.Add(-29*24*time.Hour)
	type row struct {
		key    string
		result string
		retry  bool
		at     time.Time
		leased bool
	}
	rows := []row{
		{key: "delivered", result: "success", at: old},
		{key: "skipped", result: "skipped", at: old.Add(time.Minute)},
		{key: "refused", result: "refused", at: old.Add(2 * time.Minute)},
		{key: "failed", result: "permanent_error", at: old.Add(3 * time.Minute)},
		{key: "recent", result: "success", at: recent},
		{key: "retrying", result: "retryable_error", retry: true, at: old},
		{key: "leased", leased: true},
		{key: "pending"},
	}
	for _, r := range rows {
		if _, err := repo.EnqueueNotification(ctx, NotificationRequest{Notifier: "silo", DedupeKey: r.key, PayloadJSON: []byte(`{}`), NextAttemptAt: old.Add(-time.Hour)}); err != nil {
			t.Fatal(err)
		}
	}
	leases, err := repo.LeaseDueNotifications(ctx, old, 100, 365*24*time.Hour)
	if err != nil || len(leases) != len(rows) {
		t.Fatalf("lease = %d/%v", len(leases), err)
	}
	for index, r := range rows {
		switch {
		case r.key == "pending":
			if err := repo.ReleaseNotificationLease(ctx, leases[index].JobID); err != nil {
				t.Fatal(err)
			}
		case r.leased:
		default:
			completion := NotificationCompletion{JobID: leases[index].JobID, Result: r.result, FinishedAt: r.at}
			if r.retry {
				completion.NextAttemptAt = now.Add(time.Hour)
			}
			if err := repo.CompleteNotification(ctx, completion); err != nil {
				t.Fatal(err)
			}
		}
	}
	cutoff := now.Add(-NotificationRetention)
	for _, want := range []int64{3, 1, 0} {
		if deleted, err := repo.PruneFinishedNotifications(ctx, cutoff, 3); err != nil || deleted != want {
			t.Fatalf("PruneFinishedNotifications() = %d/%v, want %d", deleted, err, want)
		}
	}
	remaining, err := repo.store.db.Query(`SELECT dedupe_key, result FROM notifications ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer remaining.Close()
	var kept []string
	for remaining.Next() {
		var key sql.NullString
		var result string
		if err := remaining.Scan(&key, &result); err != nil {
			t.Fatal(err)
		}
		kept = append(kept, result+":"+key.String)
	}
	// The finished recent row released its key; the others keep theirs.
	if got := strings.Join(kept, " "); got != "success: retryable_error:retrying :leased :pending" {
		t.Fatalf("kept = %q", got)
	}
}

// The prune reads the partial index, not every finished row.
func TestPruneFinishedNotificationsUsesItsIndex(t *testing.T) {
	repo := openTestRepository(t)
	plan, err := repo.store.db.Query(`EXPLAIN QUERY PLAN SELECT id FROM notifications WHERE next_attempt_at_ns = 0 AND finished_at_ns < ? AND lease_owner IS NULL ORDER BY finished_at_ns LIMIT ?`, 1, 500)
	if err != nil {
		t.Fatal(err)
	}
	defer plan.Close()
	var details []string
	for plan.Next() {
		var id, parent, unused int
		var detail string
		if err := plan.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		details = append(details, detail)
	}
	if got := strings.Join(details, "; "); !strings.Contains(got, "notifications_finished_idx") || strings.Contains(got, "TEMP B-TREE") {
		t.Fatalf("plan = %s", got)
	}
}

// Migration 024 counts intents that finished before it as finished then, so
// they are pruned a retention period after the upgrade; unfinished ones get
// no finish time.
func TestMigration024StampsFinishedNotifications(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "subsyncd.db")
	old, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := old.Exec(`CREATE TABLE schema_migrations (version TEXT PRIMARY KEY, applied_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP)`); err != nil {
		t.Fatal(err)
	}
	entries, err := fs.ReadDir(migrationFiles, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		if entry.Name() < "024" {
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
	if _, err := old.Exec(`INSERT INTO notifications(notifier,payload_json,attempt,next_attempt_at_ns,result) VALUES ('silo',CAST('{}' AS BLOB),1,0,'success'), ('silo',CAST('{}' AS BLOB),1,5,'retryable_error')`); err != nil {
		t.Fatal(err)
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	before := time.Now().Add(-time.Second)
	database, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	repo := database.Repository()
	var finished, unfinished sql.NullInt64
	if err := repo.store.db.QueryRow(`SELECT finished_at_ns FROM notifications WHERE next_attempt_at_ns = 0`).Scan(&finished); err != nil {
		t.Fatal(err)
	}
	if err := repo.store.db.QueryRow(`SELECT finished_at_ns FROM notifications WHERE next_attempt_at_ns > 0`).Scan(&unfinished); err != nil {
		t.Fatal(err)
	}
	if !finished.Valid || time.Unix(0, finished.Int64).Before(before.Truncate(time.Second)) || time.Unix(0, finished.Int64).After(time.Now()) || unfinished.Valid {
		t.Fatalf("finished_at_ns = %v, unfinished = %v", finished, unfinished)
	}
	if deleted, err := repo.PruneFinishedNotifications(ctx, time.Now().Add(-NotificationRetention), 500); err != nil || deleted != 0 {
		t.Fatalf("prune within the retention = %d/%v", deleted, err)
	}
	if deleted, err := repo.PruneFinishedNotifications(ctx, time.Now().Add(time.Minute), 500); err != nil || deleted != 1 {
		t.Fatalf("prune past the retention = %d/%v", deleted, err)
	}
}
