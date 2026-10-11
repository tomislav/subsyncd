package store

import (
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

func TestSearchResumeExpiresWithSearchCache(t *testing.T) {
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
	lease = leaseOne(t, repo, now.Add(time.Hour))
	if !reflect.DeepEqual(lease.ResumeProviders, []string{"p"}) || !lease.ResumeSavedAt.Equal(saved) {
		t.Fatalf("fresh resume = %+v", lease)
	}
	// Throttled again past the cache TTL counted from the first answer.
	if _, err := repo.CompleteSearch(ctx, SearchCompletion{JobID: lease.JobID, Outcome: "throttled", NextAttemptAt: saved.Add(ResumeProvidersTTL), ResumeProviders: []string{"p"}, ResumeRouteSignature: "route", PreserveResume: true, ResumeSavedAt: lease.ResumeSavedAt}); err != nil {
		t.Fatal(err)
	}
	lease = leaseOne(t, repo, saved.Add(ResumeProvidersTTL))
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
	if err := repo.SettlePendingSearch(ctx, mediaID, "en", "installed", expired.Add(time.Minute)); err != nil {
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
	if _, err := repo.store.db.Exec(`UPDATE search_states SET state='complete', next_attempt_at_ns=0, last_outcome='installed', priority=? WHERE media_id=?`, SearchPriorityUpgrade, mediaID); err != nil {
		t.Fatal(err)
	}
	replaced := media.Fingerprint
	replaced.Size++
	if err := repo.ReplaceTrackInventory(ctx, mediaID, media.Fingerprint, replaced, nil); err != nil {
		t.Fatal(err)
	}
	status, err := repo.GetSearchStatus(ctx, mediaID, "en")
	if err != nil || status.State != "pending" || status.Priority != SearchPriorityImport || status.NextAttemptAt.After(time.Now()) {
		t.Fatalf("installed language after in-place replacement = %+v/%v, want an immediate import search", status, err)
	}
	if status, err := repo.GetSearchStatus(ctx, mediaID, "hr"); err != nil || status.State != "complete" {
		t.Fatalf("language without an installation = %+v/%v, want unchanged", status, err)
	}
}
