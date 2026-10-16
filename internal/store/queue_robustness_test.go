package store

import (
	"database/sql"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"subsyncd/internal/domain"
)

func leaseOne(t *testing.T, repo *Repository, now time.Time) SearchLease {
	t.Helper()
	leases, err := repo.LeaseDueSearches(t.Context(), now, 1, 5*time.Minute)
	if err != nil || len(leases) != 1 {
		t.Fatalf("lease = %+v/%v", leases, err)
	}
	return leases[0]
}

func TestSearchResumeExpiresAtTheSafetyCap(t *testing.T) {
	repo := openTestRepository(t)
	ctx := t.Context()
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	mediaID, _, err := repo.UpsertMedia(ctx, testMedia())
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertSearchStateWithPriority(ctx, mediaID, "en", now, SearchPriorityMissing); err != nil {
		t.Fatal(err)
	}
	saved := now.Add(-time.Hour)
	lease := leaseOne(t, repo, now)
	if _, err := repo.CompleteSearch(ctx, SearchCompletion{JobID: lease.JobID, Outcome: "throttled", NextAttemptAt: now.Add(time.Hour), ResumeProviders: []string{"p"}, ResumeRouteSignature: "route", PreserveResume: true, ResumeSavedAt: saved}); err != nil {
		t.Fatal(err)
	}
	lease = leaseOne(t, repo, saved.Add(30*time.Hour))
	if !reflect.DeepEqual(lease.ResumeProviders, []string{"p"}) || !lease.ResumeSavedAt.Equal(saved) {
		t.Fatalf("fresh resume = %+v", lease)
	}
	// Throttled again past the safety cap counted from the first answer.
	if _, err := repo.CompleteSearch(ctx, SearchCompletion{JobID: lease.JobID, Outcome: "throttled", NextAttemptAt: saved.Add(ResumeProvidersMaxAge), ResumeProviders: []string{"p"}, ResumeRouteSignature: "route", PreserveResume: true, ResumeSavedAt: lease.ResumeSavedAt}); err != nil {
		t.Fatal(err)
	}
	lease = leaseOne(t, repo, saved.Add(ResumeProvidersMaxAge))
	if len(lease.ResumeProviders) != 0 || lease.ResumeRouteSignature != "" || !lease.ResumeSavedAt.IsZero() {
		t.Fatalf("expired resume list still skipped providers: %+v", lease)
	}
}

func TestReleasedSearchLeaseIsLeasedAgainAtOnce(t *testing.T) {
	repo := openTestRepository(t)
	ctx := t.Context()
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	mediaID, _, err := repo.UpsertMedia(ctx, testMedia())
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertSearchStateWithPriority(ctx, mediaID, "en", now, SearchPriorityMissing); err != nil {
		t.Fatal(err)
	}
	lease := leaseOne(t, repo, now)
	if err := repo.ReleaseSearchLease(ctx, lease.JobID); err != nil {
		t.Fatal(err)
	}
	again := leaseOne(t, repo, now)
	if again.JobID == lease.JobID {
		t.Fatal("released lease kept its owner")
	}
	if _, err := repo.CompleteSearch(ctx, SearchCompletion{JobID: lease.JobID, Outcome: "no_result"}); err == nil {
		t.Fatal("released owner completed the search")
	}
}

func TestReleasedNotificationLeaseIsLeasedAgainAtOnce(t *testing.T) {
	repo := openTestRepository(t)
	ctx := t.Context()
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	if _, err := repo.EnqueueNotification(ctx, NotificationRequest{Notifier: "silo", DedupeKey: "k", PayloadJSON: []byte(`{}`), NextAttemptAt: now}); err != nil {
		t.Fatal(err)
	}
	leases, err := repo.LeaseDueNotifications(ctx, now, 1, 5*time.Minute)
	if err != nil || len(leases) != 1 {
		t.Fatalf("lease = %v/%v", leases, err)
	}
	if err := repo.ReleaseNotificationLease(ctx, leases[0].JobID); err != nil {
		t.Fatal(err)
	}
	again, err := repo.LeaseDueNotifications(ctx, now, 1, 5*time.Minute)
	if err != nil || len(again) != 1 || again[0].Attempt != leases[0].Attempt {
		t.Fatalf("released notification = %v/%v", again, err)
	}
}

func TestExpiredLeaseCountsAsUnleased(t *testing.T) {
	repo := openTestRepository(t)
	ctx := t.Context()
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	media := testMedia()
	mediaID, _, err := repo.UpsertMedia(ctx, media)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertSearchStateWithPriority(ctx, mediaID, "en", now, SearchPriorityMissing); err != nil {
		t.Fatal(err)
	}
	crashed := leaseOne(t, repo, now)
	// A change arrives while the dead owner's lease is still recorded.
	media.Fingerprint.Size++
	if _, err := repo.ApplyMediaEvent(ctx, MediaEventMutation{EventID: "replace", Type: "import", EntityID: media.EntityID, Ref: media.Ref, Media: media, Languages: []domain.Language{"en"}, At: now.Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	expired := crashed.LeaseUntil
	successor := leaseOne(t, repo, expired)
	result, err := repo.CompleteSearch(ctx, SearchCompletion{JobID: successor.JobID, Outcome: "no_result", NextAttemptAt: expired.Add(time.Hour)})
	if err != nil || result.RerunScheduled {
		t.Fatalf("successor inherited the dead owner's rerun: %+v/%v", result, err)
	}

	if _, err := repo.store.db.Exec(`UPDATE search_states SET last_outcome='throttled', next_attempt_at_ns=?, lease_owner='dead', lease_until_ns=? WHERE media_id=?`, expired.Add(24*time.Hour).UnixNano(), expired.UnixNano(), mediaID); err != nil {
		t.Fatal(err)
	}
	if count, err := repo.RetryThrottledSearches(ctx, expired.Add(time.Minute), []domain.Language{"en"}); err != nil || count != 1 {
		t.Fatalf("retry skipped an expired lease: %d/%v", count, err)
	}
	if err := repo.SettlePendingSearch(ctx, mediaID, "en", "installed", expired.Add(time.Minute), ""); err != nil {
		t.Fatal(err)
	}
	var state string
	var owner *string
	if err := repo.store.db.QueryRow(`SELECT state, lease_owner FROM search_states WHERE media_id=?`, mediaID).Scan(&state, &owner); err != nil {
		t.Fatal(err)
	}
	if state != "complete" || owner != nil {
		t.Fatalf("settle skipped an expired lease: %s/%v", state, owner)
	}
}

func TestReconcileTimingRoundTrip(t *testing.T) {
	repo := openTestRepository(t)
	ctx := t.Context()
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	if err := repo.EnsureInstance(ctx, "sonarr-main", "sonarr", "http://sonarr", now); err != nil {
		t.Fatal(err)
	}
	if timings, err := repo.ReconcileTimings(ctx); err != nil || len(timings) != 0 {
		t.Fatalf("fresh timings = %v/%v", timings, err)
	}
	want := ReconcileTiming{LastAttempt: now, Failures: 2, Deferred: true}
	if err := repo.RecordReconcileTiming(ctx, "sonarr-main", want); err != nil {
		t.Fatal(err)
	}
	timings, err := repo.ReconcileTimings(ctx)
	if err != nil || !reflect.DeepEqual(timings, map[string]ReconcileTiming{"sonarr-main": want}) {
		t.Fatalf("timings = %v/%v", timings, err)
	}
}

func TestWebhookInboxKeepsArrivalOrderAndRetries(t *testing.T) {
	repo := openTestRepository(t)
	ctx := t.Context()
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	first, err := repo.EnqueueWebhook(ctx, "sonarr", []byte(`{"a":1}`), now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.EnqueueWebhook(ctx, "sonarr", []byte(`{"a":2}`), now); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.EnqueueWebhook(ctx, "radarr", []byte(`{"a":3}`), now); err != nil {
		t.Fatal(err)
	}
	items, err := repo.DueWebhooks(ctx, "sonarr", now, 10)
	if err != nil || len(items) != 2 || string(items[0].Body) != `{"a":1}` || string(items[1].Body) != `{"a":2}` || !items[0].ReceivedAt.Equal(now) {
		t.Fatalf("due = %+v/%v", items, err)
	}
	if err := repo.RetryWebhook(ctx, first, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := repo.DeleteWebhook(ctx, items[1].ID); err != nil {
		t.Fatal(err)
	}
	if items, err := repo.DueWebhooks(ctx, "sonarr", now, 10); err != nil || len(items) != 0 {
		t.Fatalf("retried webhook due early: %+v/%v", items, err)
	}
	if next, found, err := repo.NextWebhookAt(ctx, "sonarr"); err != nil || !found || !next.Equal(now.Add(time.Minute)) {
		t.Fatalf("next = %v/%t/%v", next, found, err)
	}
	items, err = repo.DueWebhooks(ctx, "sonarr", now.Add(time.Minute), 10)
	if err != nil || len(items) != 1 || items[0].Attempt != 1 {
		t.Fatalf("retried = %+v/%v", items, err)
	}
}

func TestPendingInstallRoundTrip(t *testing.T) {
	repo := openTestRepository(t)
	ctx := t.Context()
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	id, err := repo.BeginPendingInstall(ctx, PendingInstall{MediaID: 1, Language: "en", Destination: "/media/show.en.srt", Checksum: "c", StagedPath: "/media/.subsyncd-stage-1", PublishedInode: 1<<63 + 5, CreatedAt: now})
	if err != nil {
		t.Fatal(err)
	}
	pending, err := repo.ListPendingInstalls(ctx)
	if err != nil || len(pending) != 1 || pending[0].ID != id || pending[0].StagedPath != "/media/.subsyncd-stage-1" || pending[0].PublishedInode != 1<<63+5 || !pending[0].CreatedAt.Equal(now) {
		t.Fatalf("pending = %+v/%v", pending, err)
	}
	if err := repo.FinishPendingInstall(ctx, id); err != nil {
		t.Fatal(err)
	}
	if pending, err := repo.ListPendingInstalls(ctx); err != nil || len(pending) != 0 {
		t.Fatalf("finished install still pending: %+v/%v", pending, err)
	}
}

func TestLateImportDoesNotReviveDeletedMedia(t *testing.T) {
	repo := openTestRepository(t)
	ctx := t.Context()
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	media := testMedia()
	if _, err := repo.ApplyMediaEvent(ctx, MediaEventMutation{EventID: "import", Type: "import", EntityID: media.EntityID, Ref: media.Ref, Media: media, Languages: []domain.Language{"en"}, At: now}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.ApplyMediaEvent(ctx, MediaEventMutation{EventID: "delete", Type: "delete", Ref: media.Ref, At: now.Add(2 * time.Minute)}); err != nil {
		t.Fatal(err)
	}
	// A rename received before the delete but applied after it (a retry).
	applied, err := repo.ApplyMediaEvent(ctx, MediaEventMutation{EventID: "late-rename", Type: "rename", EntityID: media.EntityID, Ref: media.Ref, Media: media, Languages: []domain.Language{"en"}, At: now.Add(time.Minute)})
	if err != nil || applied {
		t.Fatalf("late rename applied=%t err=%v", applied, err)
	}
	var deleted bool
	if err := repo.store.db.QueryRow(`SELECT deleted FROM media WHERE file_id=?`, media.Ref.FileID).Scan(&deleted); err != nil || !deleted {
		t.Fatalf("deleted media revived: deleted=%t err=%v", deleted, err)
	}
	// A change that happened after the delete still applies.
	if applied, err := repo.ApplyMediaEvent(ctx, MediaEventMutation{EventID: "reimport", Type: "import", EntityID: media.EntityID, Ref: media.Ref, Media: media, Languages: []domain.Language{"en"}, At: now.Add(3 * time.Minute)}); err != nil || !applied {
		t.Fatalf("newer import applied=%t err=%v", applied, err)
	}
}

func TestDeleteOfAnOlderFileDoesNotBlockTheNewFilesImport(t *testing.T) {
	// A Sonarr upgrade: the new file's import (t1) failed to hydrate and is
	// retried, while the old file's delete (t2 > t1) was applied first.
	repo := openTestRepository(t)
	ctx := t.Context()
	t0 := time.Date(2026, 10, 12, 10, 0, 0, 0, time.UTC)
	old := testMedia()
	if _, err := repo.ApplyMediaEvent(ctx, MediaEventMutation{EventID: "import-old", Type: "import", EntityID: old.EntityID, Ref: old.Ref, Media: old, Languages: []domain.Language{"en"}, At: t0}); err != nil {
		t.Fatal(err)
	}
	t1 := t0.Add(time.Hour)
	t2 := t1.Add(2 * time.Second)
	if _, err := repo.ApplyMediaEvent(ctx, MediaEventMutation{EventID: "delete-old", Type: "delete", EntityID: old.EntityID, Ref: old.Ref, At: t2}); err != nil {
		t.Fatal(err)
	}
	upgraded := old
	upgraded.Ref.FileID = old.Ref.FileID + 1
	upgraded.Fingerprint.FileID = upgraded.Ref.FileID
	upgraded.Fingerprint.Path = "/media/show.2160p.mkv"
	if applied, err := repo.ApplyMediaEvent(ctx, MediaEventMutation{EventID: "import-new", Type: "import", EntityID: upgraded.EntityID, Ref: upgraded.Ref, Media: upgraded, Languages: []domain.Language{"en"}, At: t1}); err != nil || !applied {
		t.Fatalf("retried upgrade import applied=%t err=%v", applied, err)
	}
	if active, err := repo.MediaActive(ctx, upgraded.Ref); err != nil || !active {
		t.Fatalf("upgraded episode active=%t err=%v", active, err)
	}

	// A delete of the whole episode (no file id) after the import still wins.
	if _, err := repo.ApplyMediaEvent(ctx, MediaEventMutation{EventID: "delete-entity", Type: "delete", EntityID: old.EntityID, Ref: domain.MediaRef{Instance: old.Ref.Instance, Kind: old.Ref.Kind}, At: t1.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	other := upgraded
	other.Ref.FileID = upgraded.Ref.FileID + 1
	other.Fingerprint.FileID = other.Ref.FileID
	if applied, err := repo.ApplyMediaEvent(ctx, MediaEventMutation{EventID: "import-late", Type: "import", EntityID: other.EntityID, Ref: other.Ref, Media: other, Languages: []domain.Language{"en"}, At: t1.Add(30 * time.Minute)}); err != nil || applied {
		t.Fatalf("import before the entity delete applied=%t err=%v", applied, err)
	}
	if active, err := repo.MediaActive(ctx, upgraded.Ref); err != nil || active {
		t.Fatalf("entity-deleted episode active=%t err=%v", active, err)
	}
}

func TestInPlaceReplacementSearchesInstalledLanguagesAgain(t *testing.T) {
	repo := openTestRepository(t)
	ctx := t.Context()
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	media := testMedia()
	mediaID, _, err := repo.UpsertMedia(ctx, media)
	if err != nil {
		t.Fatal(err)
	}
	for _, language := range []domain.Language{"en", "hr"} {
		if err := repo.UpsertSearchStateWithPriority(ctx, mediaID, language, now, SearchPriorityMissing); err != nil {
			t.Fatal(err)
		}
	}
	if err := repo.RecordInstallation(ctx, Installation{MediaID: mediaID, Language: "en", Path: "/media/show.en.srt", Checksum: "c", MediaPath: media.Fingerprint.Path, MediaFileID: media.Fingerprint.FileID, MediaSize: media.Fingerprint.Size, MediaModTimeNS: media.Fingerprint.ModTime.UnixNano()}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.store.db.Exec(`UPDATE search_states SET state='complete', attempt=2, failure_attempt=1, next_attempt_at_ns=0, last_outcome='installed', priority=? WHERE media_id=?`, SearchPriorityMissing, mediaID); err != nil {
		t.Fatal(err)
	}
	// A new mtime alone (a remount, a metadata repair) is not a replacement.
	touched := media.Fingerprint
	touched.ModTime = touched.ModTime.Add(time.Hour)
	if err := repo.ReplaceTrackInventory(ctx, mediaID, media.Fingerprint, touched, nil); err != nil {
		t.Fatal(err)
	}
	if status, err := repo.GetSearchStatus(ctx, mediaID, "en"); err != nil || status.State != "complete" {
		t.Fatalf("installed language after an mtime-only change = %+v/%v, want nothing searched again", status, err)
	}
	replaced := touched
	replaced.Size++
	if err := repo.ReplaceTrackInventory(ctx, mediaID, touched, replaced, nil); err != nil {
		t.Fatal(err)
	}
	status, err := repo.GetSearchStatus(ctx, mediaID, "en")
	if err != nil || status.State != "pending" || status.Priority != SearchPriorityImport || status.NextAttemptAt.After(time.Now()) {
		t.Fatalf("installed language after in-place replacement = %+v/%v, want an immediate import search", status, err)
	}
	if status.Attempt != 2 || status.FailureAttempt != 1 || status.LastOutcome != "installed" {
		t.Fatalf("history after in-place replacement = %+v, want attempts and last outcome kept", status)
	}
	if status, err := repo.GetSearchStatus(ctx, mediaID, "hr"); err != nil || status.State != "complete" {
		t.Fatalf("language without an installation = %+v/%v, want unchanged", status, err)
	}
}

func TestDeleteTimeIsNotMovedByOtherWrites(t *testing.T) {
	// A delete at T+1h, then a re-import of the entity at T+1.5h replayed by
	// reconciliation. An unrelated write (an inventory refresh) that moved
	// the row's updated_at past the re-import before the delete must not make
	// the re-import look older than the delete.
	for _, bumped := range []bool{false, true} {
		repo := openTestRepository(t)
		ctx := t.Context()
		start := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
		if err := repo.EnsureInstance(ctx, "sonarr-main", "sonarr", "http://s.invalid", start); err != nil {
			t.Fatal(err)
		}
		old := testMedia()
		old.EntityID, old.Ref.FileID, old.Fingerprint.FileID = 101, 1001, 1001
		if _, err := repo.ApplyMediaEvent(ctx, MediaEventMutation{EventID: "import-old", Type: "import", EntityID: 101, Ref: old.Ref, Media: old, Languages: []domain.Language{"en"}, At: start}); err != nil {
			t.Fatal(err)
		}
		if bumped {
			if _, err := repo.store.db.Exec(`UPDATE media SET updated_at_ns=? WHERE file_id=1001`, start.Add(2*time.Hour).UnixNano()); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := repo.ApplyMediaEvent(ctx, MediaEventMutation{EventID: "delete-old", Type: "delete", EntityID: 101, Ref: old.Ref, At: start.Add(time.Hour)}); err != nil {
			t.Fatal(err)
		}
		fresh := testMedia()
		fresh.EntityID, fresh.Ref.FileID, fresh.Fingerprint.FileID = 101, 1002, 1002
		fresh.Fingerprint.Path = "/media/new.mkv"
		applied, err := repo.ApplyMediaEvent(ctx, MediaEventMutation{EventID: "import-new", Type: "import", EntityID: 101, Ref: fresh.Ref, Media: fresh, Languages: []domain.Language{"en"}, At: start.Add(90 * time.Minute), Replay: true})
		if err != nil || !applied {
			t.Fatalf("bumped=%t: re-import after the delete applied=%t err=%v", bumped, applied, err)
		}
		var live int
		if err := repo.store.db.QueryRow(`SELECT count(*) FROM media WHERE deleted=0`).Scan(&live); err != nil || live != 1 {
			t.Fatalf("bumped=%t: live media = %d/%v, want the re-imported file", bumped, live, err)
		}
	}
}

func TestRecordingAnInstallationClearsItsPendingRow(t *testing.T) {
	repo := openTestRepository(t)
	ctx := t.Context()
	media := testMedia()
	mediaID, _, err := repo.UpsertMedia(ctx, media)
	if err != nil {
		t.Fatal(err)
	}
	pending := PendingInstall{MediaID: mediaID, Language: "en", Destination: "/media/show.en.srt", Checksum: "c"}
	stale, err := repo.BeginPendingInstall(ctx, pending)
	if err != nil {
		t.Fatal(err)
	}
	installation := Installation{MediaID: mediaID, Language: "en", Path: "/media/show.en.srt", Checksum: "c", MediaPath: media.Fingerprint.Path, MediaFileID: media.Fingerprint.FileID, MediaSize: media.Fingerprint.Size, MediaModTimeNS: media.Fingerprint.ModTime.UnixNano(), PendingInstallID: stale}
	// A record that fails keeps the row for the startup sweep.
	changed := installation
	changed.MediaSize++
	if _, err := repo.RecordInstallationWithNotifications(ctx, changed, nil); err == nil {
		t.Fatal("record with a changed media fingerprint succeeded")
	}
	if rows, err := repo.ListPendingInstalls(ctx); err != nil || len(rows) != 1 {
		t.Fatalf("pending after a failed record = %+v/%v, want it kept", rows, err)
	}
	if _, err := repo.RecordInstallationWithNotifications(ctx, installation, nil); err != nil {
		t.Fatal(err)
	}
	if rows, err := repo.ListPendingInstalls(ctx); err != nil || len(rows) != 0 {
		t.Fatalf("pending after the record = %+v/%v, want it cleared in the same transaction", rows, err)
	}
}

func TestWebhookInboxCountsPerInstance(t *testing.T) {
	repo := openTestRepository(t)
	ctx := t.Context()
	now := time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)
	for _, instance := range []string{"radarr", "gone", "gone"} {
		if _, err := repo.EnqueueWebhook(ctx, instance, []byte(`{}`), now); err != nil {
			t.Fatal(err)
		}
	}
	counts, err := repo.WebhookInboxCounts(ctx)
	if err != nil || !reflect.DeepEqual(counts, map[string]int{"radarr": 1, "gone": 2}) {
		t.Fatalf("counts = %v/%v", counts, err)
	}
}

func TestMigrationDatesExistingDeletesFromTheirLastUpdate(t *testing.T) {
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
		if entry.Name() >= "021_" {
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
	if _, err := old.Exec(`INSERT INTO media(id,instance,kind,entity_id,file_id,path,size,mod_time_ns,title,deleted,updated_at_ns) VALUES (1,'radarr','movie',1,11,'/media/a.mkv',100,1,'A',1,500),(2,'radarr','movie',2,22,'/media/b.mkv',100,1,'B',0,700)`); err != nil {
		t.Fatal(err)
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	migrated, err := Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer migrated.Close()
	for id, want := range map[int]int64{1: 500, 2: 0} {
		var deletedAt int64
		if err := migrated.db.QueryRow(`SELECT deleted_at_ns FROM media WHERE id=?`, id).Scan(&deletedAt); err != nil || deletedAt != want {
			t.Fatalf("media %d deleted_at_ns = %d/%v, want %d", id, deletedAt, err, want)
		}
	}
}
