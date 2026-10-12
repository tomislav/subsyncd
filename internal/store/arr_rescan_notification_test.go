package store

import (
	"context"
	"database/sql"
	"io/fs"
	"path/filepath"
	"sort"
	"testing"
	"time"
)

// Arr rescans need no schema change: notifications.notifier is free text and
// the payload already carries the media's instance, series ID and entity ID.
// A database at migration 023 opens unchanged, keeps delivering its pending
// Silo intents, and takes Arr rescans beside them.
func TestDatabaseAt023KeepsPendingSiloNotificationsAndTakesArrRescans(t *testing.T) {
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
		if entry.Name() <= "023_live_control.sql" {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	if len(names) != 23 {
		t.Fatalf("migrations up to 023 = %q", names)
	}
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
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	siloPayload := `{"media":{"series_id":5,"entity_id":51,"ref":{"instance":"sonarr","kind":"episode","file_id":7},"fingerprint":{"path":"/media/Show/e.mkv","file_id":7,"size":1,"mod_time":"2026-10-12T12:00:00Z"},"title":"Show","external_ids":{}},"subtitle_path":"/media/Show/e.en.srt"}`
	for _, statement := range []string{
		// Due, and one a crashed process still leases (expired).
		`INSERT INTO notifications(notifier,payload_json,attempt,next_attempt_at_ns,dedupe_key) VALUES ('silo',CAST('` + siloPayload + `' AS BLOB),0,1,'silo-due')`,
		`INSERT INTO notifications(notifier,payload_json,attempt,next_attempt_at_ns,result,lease_owner,lease_until_ns,dedupe_key) VALUES ('silo',CAST('` + siloPayload + `' AS BLOB),2,1,'retryable_error','dead-worker',2,'silo-leased')`,
		`INSERT INTO notifications(notifier,payload_json,attempt,next_attempt_at_ns,result) VALUES ('silo',CAST('` + siloPayload + `' AS BLOB),1,0,'success')`,
	} {
		if _, err := old.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}

	database, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	repository := database.Repository()
	var applied int
	if err := database.db.QueryRow(`SELECT count(*) FROM schema_migrations`).Scan(&applied); err != nil || applied != len(entries) {
		t.Fatalf("applied migrations = %d/%v, want %d", applied, err, len(entries))
	}
	if backlog, err := repository.NotificationBacklog(ctx); err != nil || backlog != 2 {
		t.Fatalf("backlog = %d/%v", backlog, err)
	}
	inserted, err := repository.EnqueueNotification(ctx, NotificationRequest{Notifier: "arr_rescan", DedupeKey: "arr-1", PayloadJSON: []byte(siloPayload), NextAttemptAt: now})
	if err != nil || !inserted {
		t.Fatalf("enqueue Arr rescan = %v/%v", inserted, err)
	}
	leases, err := repository.LeaseDueNotifications(ctx, now, 10, 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]int{}
	for _, lease := range leases {
		got[lease.Notifier]++
		if string(lease.PayloadJSON) != siloPayload {
			t.Fatalf("payload changed: %s", lease.PayloadJSON)
		}
	}
	if got["silo"] != 2 || got["arr_rescan"] != 1 || len(leases) != 3 {
		t.Fatalf("leased = %#v", got)
	}
	for _, lease := range leases {
		result := "success"
		if lease.Notifier == "arr_rescan" {
			result = "refused"
		}
		if err := repository.CompleteNotification(ctx, NotificationCompletion{JobID: lease.JobID, Result: result}); err != nil {
			t.Fatal(err)
		}
	}
	if backlog, err := repository.NotificationBacklog(ctx); err != nil || backlog != 0 {
		t.Fatalf("backlog after delivery = %d/%v", backlog, err)
	}
}

func TestRebaseInstallationQueuesNotificationsOnlyWithTheRebase(t *testing.T) {
	repo := openTestRepository(t)
	ctx := context.Background()
	media := testMedia()
	mediaID, _, err := repo.UpsertMedia(ctx, media)
	if err != nil {
		t.Fatal(err)
	}
	installation := Installation{MediaID: mediaID, Language: "en", Path: "/media/x.en.srt", Checksum: "sum", ProviderID: "p", CandidateID: "c", MediaPath: media.Fingerprint.Path, MediaFileID: media.Fingerprint.FileID, MediaSize: media.Fingerprint.Size, MediaModTimeNS: media.Fingerprint.ModTime.UnixNano()}
	if err := repo.RecordInstallation(ctx, installation); err != nil {
		t.Fatal(err)
	}
	request := NotificationRequest{Notifier: "arr_rescan", DedupeKey: "moved", PayloadJSON: []byte(`{}`), NextAttemptAt: time.Unix(1, 0)}
	stale := installation
	stale.Checksum = "other"
	if _, err := repo.RebaseInstallationWithNotifications(ctx, stale, "/media/y.en.srt", "/media/y.mkv", []NotificationRequest{request}); err == nil {
		t.Fatal("stale rebase succeeded")
	}
	if backlog, err := repo.NotificationBacklog(ctx); err != nil || backlog != 0 {
		t.Fatalf("stale rebase queued %d/%v", backlog, err)
	}
	results, err := repo.RebaseInstallationWithNotifications(ctx, installation, "/media/y.en.srt", "/media/y.mkv", []NotificationRequest{request})
	if err != nil || len(results) != 1 || !results[0].Inserted || results[0].Notifier != "arr_rescan" {
		t.Fatalf("rebase = %#v/%v", results, err)
	}
	if got, _, err := repo.GetInstallation(ctx, mediaID, "en"); err != nil || got.Path != "/media/y.en.srt" || got.MediaPath != "/media/y.mkv" {
		t.Fatalf("installation = %#v/%v", got, err)
	}
	if backlog, err := repo.NotificationBacklog(ctx); err != nil || backlog != 1 {
		t.Fatalf("backlog = %d/%v", backlog, err)
	}
}

// A coalescing intent (an Arr rescan of one series) is absorbed by a queued
// one with its key, pending or waiting for a retry, which keeps its due time.
// One already leased may have been sent before the change: the change gets a
// row of its own, which absorbs later changes in turn.
func TestCoalescingNotificationJoinsTheQueuedOneUntilItIsLeased(t *testing.T) {
	ctx := context.Background()
	repo := openTestRepository(t)
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	request := func(at time.Time) NotificationRequest {
		return NotificationRequest{Notifier: "arr_rescan", DedupeKey: "series-5", PayloadJSON: []byte(`{}`), NextAttemptAt: at, Coalesce: true}
	}
	queued := func() (rows int, due []int64) {
		t.Helper()
		result, err := repo.store.db.Query(`SELECT next_attempt_at_ns FROM notifications WHERE next_attempt_at_ns > 0 ORDER BY id`)
		if err != nil {
			t.Fatal(err)
		}
		defer result.Close()
		for result.Next() {
			var at int64
			if err := result.Scan(&at); err != nil {
				t.Fatal(err)
			}
			due = append(due, at)
		}
		return len(due), due
	}
	first := now.Add(time.Minute)
	if inserted, err := repo.EnqueueNotification(ctx, request(first)); err != nil || !inserted {
		t.Fatalf("first enqueue = %t, %v", inserted, err)
	}
	for minutes := 1; minutes <= 3; minutes++ {
		if inserted, err := repo.EnqueueNotification(ctx, request(first.Add(time.Duration(minutes)*time.Minute))); err != nil || inserted {
			t.Fatalf("enqueue while queued = %t, %v", inserted, err)
		}
	}
	if n, due := queued(); n != 1 || due[0] != first.UnixNano() {
		t.Fatalf("queued = %d %v, want one row due at the first change's time", n, due)
	}
	// Leased: the next change is queued separately.
	jobs, err := repo.LeaseDueNotifications(ctx, first, 10, time.Minute)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("lease = %#v, %v", jobs, err)
	}
	second := first.Add(10 * time.Minute)
	if inserted, err := repo.EnqueueNotification(ctx, request(second)); err != nil || !inserted {
		t.Fatalf("enqueue while in flight = %t, %v", inserted, err)
	}
	if inserted, err := repo.EnqueueNotification(ctx, request(second.Add(time.Minute))); err != nil || inserted {
		t.Fatalf("enqueue beside the new row = %t, %v", inserted, err)
	}
	// The in-flight one fails and waits for a retry; the new row still holds
	// the key, and nothing collides.
	if err := repo.CompleteNotification(ctx, NotificationCompletion{JobID: jobs[0].JobID, Result: "retryable_error", NextAttemptAt: first.Add(5 * time.Minute)}); err != nil {
		t.Fatal(err)
	}
	if n, _ := queued(); n != 2 {
		t.Fatalf("queued = %d, want the retry and the later change", n)
	}
	// A retry-scheduled row with the key absorbs changes too.
	jobs, err = repo.LeaseDueNotifications(ctx, second, 10, time.Minute)
	if err != nil || len(jobs) != 2 {
		t.Fatalf("lease = %#v, %v", jobs, err)
	}
	for _, job := range jobs {
		if err := repo.CompleteNotification(ctx, NotificationCompletion{JobID: job.JobID, Result: "retryable_error", NextAttemptAt: second.Add(time.Hour)}); err != nil {
			t.Fatal(err)
		}
	}
	if inserted, err := repo.EnqueueNotification(ctx, request(second.Add(2*time.Minute))); err != nil || inserted {
		t.Fatalf("enqueue beside a retry = %t, %v", inserted, err)
	}
	// Another series is never absorbed.
	other := request(second)
	other.DedupeKey = "series-6"
	if inserted, err := repo.EnqueueNotification(ctx, other); err != nil || !inserted {
		t.Fatalf("other series = %t, %v", inserted, err)
	}
	if n, _ := queued(); n != 3 {
		t.Fatalf("queued = %d", n)
	}
}
