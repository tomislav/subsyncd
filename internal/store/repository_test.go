package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"subsyncd/internal/domain"
)

func TestOpenAppliesMigrationsIdempotently(t *testing.T) {
	path := filepath.Join(t.TempDir(), "subsyncd.db")
	for run := 0; run < 2; run++ {
		store, err := Open(context.Background(), path)
		if err != nil {
			t.Fatalf("Open() run %d error = %v", run, err)
		}
		var count int
		if err := store.db.QueryRow(`SELECT count(*) FROM schema_migrations`).Scan(&count); err != nil {
			t.Fatalf("query migrations: %v", err)
		}
		if count != 6 {
			t.Errorf("migration count = %d, want 6", count)
		}
		if err := store.Close(); err != nil {
			t.Fatalf("Close(): %v", err)
		}
	}
}

func TestUpsertMediaReportsFingerprintChanges(t *testing.T) {
	repo := openTestRepository(t)
	media := testMedia()
	id, changed, err := repo.UpsertMedia(context.Background(), media)
	if err != nil || !changed {
		t.Fatalf("first UpsertMedia() = %d, %v, %v; want changed", id, changed, err)
	}
	secondID, changed, err := repo.UpsertMedia(context.Background(), media)
	if err != nil || changed || secondID != id {
		t.Fatalf("second UpsertMedia() = %d, %v, %v; want %d, false", secondID, changed, err, id)
	}
	media.Fingerprint.Size++
	thirdID, changed, err := repo.UpsertMedia(context.Background(), media)
	if err != nil || !changed || thirdID != id {
		t.Fatalf("changed UpsertMedia() = %d, %v, %v; want %d, true", thirdID, changed, err, id)
	}
}

func TestReplaceTrackInventoryReplacesOnlyRequestedMedia(t *testing.T) {
	repo := openTestRepository(t)
	media := testMedia()
	mediaID, _, _ := repo.UpsertMedia(context.Background(), media)
	tracks := []TrackRecord{{Index: 1, Language: "en", Embedded: true}, {Path: "/media/show.en.srt", Language: "en", Checksum: "one"}}
	if err := repo.ReplaceTrackInventory(context.Background(), mediaID, media.Fingerprint, tracks); err != nil {
		t.Fatal(err)
	}
	if err := repo.ReplaceTrackInventory(context.Background(), mediaID, media.Fingerprint, tracks[1:]); err != nil {
		t.Fatal(err)
	}
	got, err := repo.GetTrackInventory(context.Background(), mediaID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Tracks) != 1 || got.Tracks[0].Path != tracks[1].Path {
		t.Fatalf("tracks = %#v, want only external track", got.Tracks)
	}
	if got.Fingerprint != media.Fingerprint {
		t.Fatalf("fingerprint = %#v, want %#v", got.Fingerprint, media.Fingerprint)
	}
}

func TestUpsertSearchStateKeepsOneRowPerMediaLanguage(t *testing.T) {
	repo := openTestRepository(t)
	mediaID, _, _ := repo.UpsertMedia(context.Background(), testMedia())
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	if err := repo.UpsertSearchState(context.Background(), mediaID, "en", now); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertSearchState(context.Background(), mediaID, "en", now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := repo.store.db.QueryRow(`SELECT count(*) FROM search_states WHERE media_id = ? AND language = ?`, mediaID, "en").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("search state count = %d, want 1", count)
	}
}

func TestProviderStateIsIndependentByOperationScope(t *testing.T) {
	repo := openTestRepository(t)
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	for _, state := range []ProviderState{
		{ProviderID: "subdl-main", Scope: "search", Reason: "rate", ResetAt: now.Add(time.Minute)},
		{ProviderID: "subdl-main", Scope: "download", Reason: "quota", ResetAt: now.Add(time.Hour)},
	} {
		if err := repo.PutProviderState(context.Background(), state); err != nil {
			t.Fatal(err)
		}
	}
	got, err := repo.GetProviderState(context.Background(), "subdl-main", "download")
	if err != nil {
		t.Fatal(err)
	}
	if got.Reason != "quota" || !got.ResetAt.Equal(now.Add(time.Hour)) {
		t.Fatalf("download state = %#v", got)
	}
}

func TestProviderCacheExpiresAtBoundary(t *testing.T) {
	repo := openTestRepository(t)
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	entry := ProviderCacheEntry{Key: "key", ProviderID: "subdl-main", ResultsJSON: []byte(`[]`), ExpiresAt: now.Add(time.Hour)}
	if err := repo.PutProviderCache(context.Background(), entry); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := repo.GetProviderCache(context.Background(), "key", now.Add(time.Hour-time.Nanosecond)); err != nil || !ok {
		t.Fatalf("cache before expiry = %v, %v", ok, err)
	}
	if _, ok, err := repo.GetProviderCache(context.Background(), "key", now.Add(time.Hour)); err != nil || ok {
		t.Fatalf("cache at expiry = %v, %v", ok, err)
	}
}

func TestPackEntriesAreListedExpiredThenLeastRecentlyUsed(t *testing.T) {
	repo := openTestRepository(t)
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	entries := []PackCacheEntry{
		{ProviderID: "subdl", ResultID: "recent", SeriesKey: "tvdb:1", Season: 1, Language: "en", ContentChecksum: "a", ManifestPath: "/cache/a", ByteSize: 10, ExpiresAt: now.Add(time.Hour), LastAccessAt: now},
		{ProviderID: "subdl", ResultID: "expired", SeriesKey: "tvdb:1", Season: 1, Language: "en", ContentChecksum: "b", ManifestPath: "/cache/b", ByteSize: 10, ExpiresAt: now.Add(-time.Second), LastAccessAt: now.Add(time.Minute)},
		{ProviderID: "subdl", ResultID: "old", SeriesKey: "tvdb:1", Season: 1, Language: "en", ContentChecksum: "c", ManifestPath: "/cache/c", ByteSize: 10, ExpiresAt: now.Add(time.Hour), LastAccessAt: now.Add(-time.Hour)},
	}
	for _, entry := range entries {
		if err := repo.PutPack(context.Background(), entry, nil); err != nil {
			t.Fatal(err)
		}
	}
	got, err := repo.ListPacksForEviction(context.Background(), now)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].ResultID != "expired" || got[1].ResultID != "old" || got[2].ResultID != "recent" {
		t.Fatalf("eviction order = %#v", got)
	}
}

func TestPackLookupMatchesAnyKnownSeriesIdentity(t *testing.T) {
	repo := openTestRepository(t)
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	entry := PackCacheEntry{ProviderID: "titlovi", ResultID: "pack", SeriesKey: "imdb:tt123", Season: 1, Language: "hr", ContentChecksum: "sum", ManifestPath: "/cache/manifest", ByteSize: 10, ExpiresAt: now.Add(time.Hour), LastAccessAt: now}
	member := PackMemberRecord{SafeName: "show.s01e02.srt", CachePath: "/cache/member", Checksum: "member", Season: 1, EpisodeFrom: 2, EpisodeTo: 2}
	if err := repo.PutPack(context.Background(), entry, []PackMemberRecord{member}); err != nil {
		t.Fatal(err)
	}
	got, found, err := repo.GetReusablePackMember(context.Background(), PackLookup{SeriesIDs: domain.ExternalIDs{TVDB: 456, IMDb: "tt123"}, SeriesTitle: "Show", SeriesYear: 2024, Season: 1, Episode: 2, Language: "hr"}, now)
	if err != nil || !found || got.ResultID != "pack" {
		t.Fatalf("lookup = %#v/%v/%v", got, found, err)
	}
}

func TestExpiredLeaseCanBeRecovered(t *testing.T) {
	repo := openTestRepository(t)
	mediaID, _, _ := repo.UpsertMedia(context.Background(), testMedia())
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	if err := repo.UpsertSearchState(context.Background(), mediaID, "en", now); err != nil {
		t.Fatal(err)
	}
	first, err := repo.LeaseDueSearches(context.Background(), now, 1, time.Minute)
	if err != nil || len(first) != 1 {
		t.Fatalf("first lease = %#v, %v", first, err)
	}
	blocked, err := repo.LeaseDueSearches(context.Background(), now.Add(30*time.Second), 1, time.Minute)
	if err != nil || len(blocked) != 0 {
		t.Fatalf("unexpired lease returned %#v, %v", blocked, err)
	}
	recovered, err := repo.LeaseDueSearches(context.Background(), now.Add(time.Minute), 1, time.Minute)
	if err != nil || len(recovered) != 1 || recovered[0].JobID == first[0].JobID {
		t.Fatalf("recovered lease = %#v, %v", recovered, err)
	}
}

func TestSearchLeaseRenewalAndAttemptAccountingAreCompareAndSwap(t *testing.T) {
	repo := openTestRepository(t)
	mediaID, _, _ := repo.UpsertMedia(context.Background(), testMedia())
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	if err := repo.UpsertSearchState(context.Background(), mediaID, "en", now); err != nil {
		t.Fatal(err)
	}
	leases, err := repo.LeaseDueSearches(context.Background(), now, 1, 5*time.Minute)
	if err != nil || len(leases) != 1 || leases[0].FailureAttempt != 0 {
		t.Fatalf("leases = %#v, %v", leases, err)
	}
	if err := repo.RenewSearchLease(context.Background(), "wrong-owner", now.Add(time.Minute), 5*time.Minute); err == nil {
		t.Fatal("wrong-owner renewal succeeded")
	}
	if err := repo.RenewSearchLease(context.Background(), leases[0].JobID, now.Add(time.Minute), 5*time.Minute); err != nil {
		t.Fatal(err)
	}
	next := now.Add(30 * time.Minute)
	if err := repo.CompleteSearch(context.Background(), SearchCompletion{JobID: leases[0].JobID, Outcome: "missing", NextAttemptAt: next, AdvanceMissingAttempt: true, ResetFailureAttempt: true}); err != nil {
		t.Fatal(err)
	}
	leases, err = repo.LeaseDueSearches(context.Background(), next, 1, 5*time.Minute)
	if err != nil || len(leases) != 1 || leases[0].Attempt != 1 || leases[0].FailureAttempt != 0 {
		t.Fatalf("missing completion lease = %#v, %v", leases, err)
	}
	if err := repo.CompleteSearch(context.Background(), SearchCompletion{JobID: leases[0].JobID, Outcome: "transport_error", NextAttemptAt: next.Add(time.Minute), AdvanceFailureAttempt: true}); err != nil {
		t.Fatal(err)
	}
	leases, err = repo.LeaseDueSearches(context.Background(), next.Add(time.Minute), 1, 5*time.Minute)
	if err != nil || len(leases) != 1 || leases[0].Attempt != 1 || leases[0].FailureAttempt != 1 {
		t.Fatalf("failure completion lease = %#v, %v", leases, err)
	}
}

func TestNotificationLeaseRetryCompletionAndDedupeLifecycle(t *testing.T) {
	repo := openTestRepository(t)
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	payload := []byte(`{"path":"/media/movie.mkv"}`)
	for range 2 {
		if err := repo.EnqueueNotification(context.Background(), NotificationRequest{Notifier: "silo", DedupeKey: "install:1:sum", PayloadJSON: payload, NextAttemptAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	jobs, err := repo.LeaseDueNotifications(context.Background(), now, 10, 5*time.Minute)
	if err != nil || len(jobs) != 1 || jobs[0].Attempt != 0 || string(jobs[0].PayloadJSON) != string(payload) {
		t.Fatalf("first notification lease = %#v, %v", jobs, err)
	}
	if competing, err := repo.LeaseDueNotifications(context.Background(), now, 10, 5*time.Minute); err != nil || len(competing) != 0 {
		t.Fatalf("competing notification lease = %#v, %v", competing, err)
	}
	if err := repo.RenewNotificationLease(context.Background(), jobs[0].JobID, now.Add(time.Minute), 5*time.Minute); err != nil {
		t.Fatal(err)
	}
	retryAt := now.Add(5 * time.Minute)
	if err := repo.CompleteNotification(context.Background(), NotificationCompletion{JobID: jobs[0].JobID, Result: "transport_error", NextAttemptAt: retryAt}); err != nil {
		t.Fatal(err)
	}
	jobs, err = repo.LeaseDueNotifications(context.Background(), retryAt, 10, 5*time.Minute)
	if err != nil || len(jobs) != 1 || jobs[0].Attempt != 1 {
		t.Fatalf("retry notification lease = %#v, %v", jobs, err)
	}
	if err := repo.CompleteNotification(context.Background(), NotificationCompletion{JobID: jobs[0].JobID, Result: "success"}); err != nil {
		t.Fatal(err)
	}
	if jobs, err := repo.LeaseDueNotifications(context.Background(), retryAt.Add(time.Hour), 10, 5*time.Minute); err != nil || len(jobs) != 0 {
		t.Fatalf("completed notification was leased again: %#v, %v", jobs, err)
	}
}

func TestGetMediaReturnsWorkflowIdentity(t *testing.T) {
	repo := openTestRepository(t)
	want := testMedia()
	mediaID, _, err := repo.UpsertMedia(context.Background(), want)
	if err != nil {
		t.Fatal(err)
	}
	got, err := repo.GetMedia(context.Background(), mediaID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Ref != want.Ref || got.Fingerprint != want.Fingerprint || got.Title != want.Title || got.ReleaseName != want.ReleaseName || got.ExternalIDs != want.ExternalIDs {
		t.Fatalf("GetMedia() = %#v, want %#v", got, want)
	}
}

func TestRecordInstallationRollsBackAuditWhenInsertFails(t *testing.T) {
	repo := openTestRepository(t)
	mediaID, _, _ := repo.UpsertMedia(context.Background(), testMedia())
	if _, err := repo.store.db.Exec(`CREATE TRIGGER fail_install BEFORE INSERT ON installations BEGIN SELECT RAISE(ABORT, 'fail'); END`); err != nil {
		t.Fatal(err)
	}
	err := repo.RecordInstallation(context.Background(), Installation{MediaID: mediaID, Language: "en", Path: "/media/x.en.srt", Checksum: "sum"})
	if err == nil {
		t.Fatal("RecordInstallation() error = nil")
	}
	var count int
	if err := repo.store.db.QueryRow(`SELECT count(*) FROM events WHERE event_type = 'subtitle_installed'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("audit event count = %d, want rollback", count)
	}
}

func TestInstallationFingerprintRoundTripsAndInvalidatesOnMediaChange(t *testing.T) {
	repo := openTestRepository(t)
	media := testMedia()
	mediaID, _, err := repo.UpsertMedia(context.Background(), media)
	if err != nil {
		t.Fatal(err)
	}
	installation := Installation{MediaID: mediaID, Language: "en", Path: "/media/x.en.srt", Checksum: "sum", ScoreJSON: []byte(`{"total":70}`), SyncResultJSON: []byte(`{"verdict":"solid"}`), MediaPath: media.Fingerprint.Path, MediaFileID: media.Fingerprint.FileID, MediaSize: media.Fingerprint.Size, MediaModTimeNS: media.Fingerprint.ModTime.UnixNano()}
	if err := repo.RecordInstallation(context.Background(), installation); err != nil {
		t.Fatal(err)
	}
	got, found, err := repo.GetInstallation(context.Background(), mediaID, "en")
	if err != nil || !found || got.MediaPath != installation.MediaPath || got.MediaModTimeNS != installation.MediaModTimeNS {
		t.Fatalf("GetInstallation() = %#v/%v/%v", got, found, err)
	}
	media.Fingerprint.Size++
	if _, changed, err := repo.UpsertMedia(context.Background(), media); err != nil || !changed {
		t.Fatalf("changed UpsertMedia() = %v/%v", changed, err)
	}
	got, found, err = repo.GetInstallation(context.Background(), mediaID, "en")
	if err != nil || !found || string(got.ScoreJSON) != "{}" || string(got.SyncResultJSON) != "{}" || got.MediaPath != "" {
		t.Fatalf("invalidated installation = %#v/%v/%v", got, found, err)
	}
}

func TestMediaRenameRetainsInstallationProvenanceAndRebasesManagedPaths(t *testing.T) {
	repo := openTestRepository(t)
	media := testMedia()
	media.Fingerprint.Path = "/media/Old.Name.mkv"
	mediaID, _, err := repo.UpsertMedia(context.Background(), media)
	if err != nil {
		t.Fatal(err)
	}
	installation := Installation{
		MediaID:        mediaID,
		Language:       "en",
		Path:           "/media/Old.Name.en.srt",
		Checksum:       "sum",
		ScoreJSON:      []byte(`{"total":70}`),
		SyncResultJSON: []byte(`{"verdict":"solid"}`),
		RollbackPath:   "/media/.subsyncd-rollback-old",
		MediaPath:      media.Fingerprint.Path,
		MediaFileID:    media.Fingerprint.FileID,
		MediaSize:      media.Fingerprint.Size,
		MediaModTimeNS: media.Fingerprint.ModTime.UnixNano(),
	}
	if err := repo.RecordInstallation(context.Background(), installation); err != nil {
		t.Fatal(err)
	}
	media.Fingerprint.Path = "/media/renamed/New.Name.mkv"
	if _, changed, err := repo.UpsertMedia(context.Background(), media); err != nil || !changed {
		t.Fatalf("renamed UpsertMedia() = %v/%v", changed, err)
	}
	got, found, err := repo.GetInstallation(context.Background(), mediaID, "en")
	if err != nil || !found {
		t.Fatalf("GetInstallation() = %#v/%v/%v", got, found, err)
	}
	if got.MediaPath != media.Fingerprint.Path || got.Path != "/media/renamed/New.Name.en.srt" || got.RollbackPath != "/media/renamed/.subsyncd-rollback-old" || string(got.ScoreJSON) != `{"total":70}` || string(got.SyncResultJSON) != `{"verdict":"solid"}` {
		t.Fatalf("renamed installation = %#v", got)
	}
}

func TestRenameEventRetainsInstallationProvenance(t *testing.T) {
	repo := openTestRepository(t)
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	media := testMedia()
	media.Fingerprint.Path = "/media/Old.Name.mkv"
	media.Fingerprint.ModTime = now
	if _, err := repo.ApplyMediaEvent(context.Background(), MediaEventMutation{EventID: "import-before-rename", Type: "import", Media: media, Ref: media.Ref, Languages: []domain.Language{"en"}, At: now}); err != nil {
		t.Fatal(err)
	}
	var mediaID int64
	if err := repo.store.db.QueryRow(`SELECT id FROM media WHERE instance=? AND kind=? AND file_id=?`, media.Ref.Instance, media.Ref.Kind, media.Ref.FileID).Scan(&mediaID); err != nil {
		t.Fatal(err)
	}
	installation := Installation{MediaID: mediaID, Language: "en", Path: "/media/Old.Name.en.srt", Checksum: "sum", ScoreJSON: []byte(`{"total":70}`), SyncResultJSON: []byte(`{"verdict":"solid"}`), MediaPath: media.Fingerprint.Path, MediaFileID: media.Fingerprint.FileID, MediaSize: media.Fingerprint.Size, MediaModTimeNS: media.Fingerprint.ModTime.UnixNano()}
	if err := repo.RecordInstallation(context.Background(), installation); err != nil {
		t.Fatal(err)
	}
	media.Fingerprint.Path = "/media/New.Name.mkv"
	if _, err := repo.ApplyMediaEvent(context.Background(), MediaEventMutation{EventID: "rename-1", Type: "rename", Media: media, Ref: media.Ref, Languages: []domain.Language{"en"}, At: now.Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	got, found, err := repo.GetInstallation(context.Background(), mediaID, "en")
	if err != nil || !found || got.Path != "/media/New.Name.en.srt" || got.MediaPath != media.Fingerprint.Path || string(got.ScoreJSON) != `{"total":70}` {
		t.Fatalf("renamed event installation = %#v/%v/%v", got, found, err)
	}
}

func TestRecordCandidatesAtomicallyReplacesMediaLanguageSet(t *testing.T) {
	repo := openTestRepository(t)
	mediaID, _, err := repo.UpsertMedia(context.Background(), testMedia())
	if err != nil {
		t.Fatal(err)
	}
	first := []CandidateRecord{
		{ProviderID: "one", ResultID: "1", MetadataJSON: []byte(`{"provider_id":"one"}`), ScoreJSON: []byte(`{"total":35}`)},
		{ProviderID: "two", ResultID: "2", MetadataJSON: []byte(`{"provider_id":"two"}`), ScoreJSON: []byte(`{"total":40}`)},
	}
	if err := repo.RecordCandidates(context.Background(), mediaID, "en", first); err != nil {
		t.Fatal(err)
	}
	second := []CandidateRecord{{ProviderID: "three", ResultID: "3", MetadataJSON: []byte(`{"provider_id":"three"}`), ScoreJSON: []byte(`{"total":45}`), ValidationJSON: []byte(`{"eligible":true}`)}}
	if err := repo.RecordCandidates(context.Background(), mediaID, "en", second); err != nil {
		t.Fatal(err)
	}
	var count int
	var providerID string
	if err := repo.store.db.QueryRow(`SELECT count(*), provider_id FROM candidates WHERE media_id=? AND language=?`, mediaID, "en").Scan(&count, &providerID); err != nil {
		t.Fatal(err)
	}
	if count != 1 || providerID != "three" {
		t.Fatalf("candidate set = %d/%q", count, providerID)
	}
}

func TestApplyMediaEventIsIdempotentAndResetsConfiguredLanguages(t *testing.T) {
	repo := openTestRepository(t)
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	media := testMedia()
	media.Fingerprint = domain.MediaFingerprint{Path: "/media/episode.mkv", FileID: media.Ref.FileID, Size: 100, ModTime: now}
	mutation := MediaEventMutation{EventID: "event-1", Type: "import", Media: media, Ref: media.Ref, Languages: []domain.Language{"hr", "en"}, At: now}

	applied, err := repo.ApplyMediaEvent(context.Background(), mutation)
	if err != nil || !applied {
		t.Fatalf("first apply = %v, %v", applied, err)
	}
	applied, err = repo.ApplyMediaEvent(context.Background(), mutation)
	if err != nil || applied {
		t.Fatalf("duplicate apply = %v, %v", applied, err)
	}

	var events, searches int
	if err := repo.store.db.QueryRow(`SELECT count(*) FROM events WHERE event_id='event-1'`).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if err := repo.store.db.QueryRow(`SELECT count(*) FROM search_states`).Scan(&searches); err != nil {
		t.Fatal(err)
	}
	if events != 1 || searches != 2 {
		t.Fatalf("events/searches = %d/%d, want 1/2", events, searches)
	}
}

func TestChangedImportInvalidatesCandidatesAndDeleteCancelsSearches(t *testing.T) {
	repo := openTestRepository(t)
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	media := testMedia()
	media.Fingerprint = domain.MediaFingerprint{Path: "/media/episode.mkv", FileID: media.Ref.FileID, Size: 100, ModTime: now}
	if _, err := repo.ApplyMediaEvent(context.Background(), MediaEventMutation{EventID: "import-1", Type: "import", Media: media, Ref: media.Ref, Languages: []domain.Language{"hr"}, At: now}); err != nil {
		t.Fatal(err)
	}
	var mediaID int64
	if err := repo.store.db.QueryRow(`SELECT id FROM media`).Scan(&mediaID); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.store.db.Exec(`INSERT INTO candidates(media_id, language, provider_id, result_id, metadata_json, score_json, created_at_ns) VALUES (?, 'hr', 'titlovi', '1', '{}', '{}', ?)`, mediaID, now.UnixNano()); err != nil {
		t.Fatal(err)
	}

	media.Fingerprint.Size++
	if _, err := repo.ApplyMediaEvent(context.Background(), MediaEventMutation{EventID: "import-2", Type: "import", Media: media, Ref: media.Ref, Languages: []domain.Language{"hr"}, At: now.Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	var candidates int
	if err := repo.store.db.QueryRow(`SELECT count(*) FROM candidates`).Scan(&candidates); err != nil {
		t.Fatal(err)
	}
	if candidates != 0 {
		t.Fatalf("candidate count = %d, want 0 after fingerprint change", candidates)
	}

	if _, err := repo.ApplyMediaEvent(context.Background(), MediaEventMutation{EventID: "delete-1", Type: "delete", Ref: media.Ref, At: now.Add(2 * time.Minute)}); err != nil {
		t.Fatal(err)
	}
	var state, outcome string
	if err := repo.store.db.QueryRow(`SELECT state, last_outcome FROM search_states WHERE media_id=? AND language='hr'`, mediaID).Scan(&state, &outcome); err != nil {
		t.Fatal(err)
	}
	if state != "complete" || outcome != "deleted" {
		t.Fatalf("delete state = %q/%q", state, outcome)
	}
}

func TestReconciliationCursorAdvancesOnlyWithCommittedPage(t *testing.T) {
	repo := openTestRepository(t)
	ctx := context.Background()
	start := time.Date(2026, 9, 4, 10, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)
	if err := repo.EnsureInstance(ctx, "sonarr-main", "sonarr", "http://sonarr:8989", start); err != nil {
		t.Fatal(err)
	}
	if got, err := repo.GetReconciliationCursor(ctx, "sonarr-main"); err != nil || !got.IsZero() {
		t.Fatalf("initial cursor = %s, %v", got, err)
	}
	if err := repo.CommitReconciliationCursor(ctx, "sonarr-main", end); err != nil {
		t.Fatal(err)
	}
	if got, err := repo.GetReconciliationCursor(ctx, "sonarr-main"); err != nil || !got.Equal(end) {
		t.Fatalf("cursor = %s, %v; want %s", got, err, end)
	}
}

func TestMediaHashCacheIsBoundToExactFingerprint(t *testing.T) {
	repo := openTestRepository(t)
	media := testMedia()
	if _, _, err := repo.UpsertMedia(context.Background(), media); err != nil {
		t.Fatal(err)
	}
	if err := repo.PutMediaHash(context.Background(), media, "opensubtitles", "0123456789abcdef", media.Fingerprint.Size); err != nil {
		t.Fatal(err)
	}
	value, size, found, err := repo.GetMediaHash(context.Background(), media, "opensubtitles")
	if err != nil || !found || value != "0123456789abcdef" || size != media.Fingerprint.Size {
		t.Fatalf("cached hash = %q/%d/%v/%v", value, size, found, err)
	}
	media.Fingerprint.ModTime = media.Fingerprint.ModTime.Add(time.Nanosecond)
	if _, _, err := repo.UpsertMedia(context.Background(), media); err != nil {
		t.Fatal(err)
	}
	if _, _, found, err := repo.GetMediaHash(context.Background(), media, "opensubtitles"); err != nil || found {
		t.Fatalf("changed fingerprint cache = %v/%v, want miss", found, err)
	}
}

func openTestRepository(t *testing.T) *Repository {
	t.Helper()
	store, err := Open(context.Background(), filepath.Join(t.TempDir(), "subsyncd.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store.Repository()
}

func testMedia() domain.Media {
	return domain.Media{
		Ref:         domain.MediaRef{Instance: "sonarr-main", Kind: domain.MediaEpisode, FileID: 42},
		Fingerprint: domain.MediaFingerprint{Path: "/media/show.mkv", FileID: 42, Size: 100, ModTime: time.Date(2026, 9, 4, 11, 0, 0, 123, time.UTC)},
		Title:       "Show", Season: 1, Episode: 2, ExternalIDs: domain.ExternalIDs{TVDB: 1},
	}
}
