package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"subsyncd/internal/domain"
)

func TestOpenAppliesBaselineIdempotently(t *testing.T) {
	path := filepath.Join(t.TempDir(), "subsyncd.db")
	for run := 0; run < 2; run++ {
		store, err := Open(context.Background(), path)
		if err != nil {
			t.Fatalf("Open() run %d error = %v", run, err)
		}
		var version string
		if err := store.db.QueryRow(`SELECT version FROM schema_migrations`).Scan(&version); err != nil {
			t.Fatal(err)
		}
		if version != "001_baseline.sql" {
			t.Fatalf("migration version = %q, want 001_baseline.sql", version)
		}
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestOpenRejectsUnknownMigrationLineage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "subsyncd.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE schema_migrations (version TEXT PRIMARY KEY, applied_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP); INSERT INTO schema_migrations(version) VALUES ('001_initial.sql')`); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	_, err = Open(context.Background(), path)
	if err == nil || !strings.Contains(err.Error(), `unsupported database migration "001_initial.sql"; rebuild from an empty data directory`) {
		t.Fatalf("Open() error = %v", err)
	}
}

func TestBaselineRequiresPositiveMediaEntityID(t *testing.T) {
	repo := openTestRepository(t)
	_, err := repo.store.db.Exec(`INSERT INTO media(instance, kind, entity_id, file_id, path, size, mod_time_ns, title, updated_at_ns) VALUES ('sonarr-main', 'episode', 0, 42, '/media/show.mkv', 100, 1, 'Show', 1)`)
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "constraint") {
		t.Fatalf("zero entity insert error = %v", err)
	}
}

func TestBaselineHasCompleteCurrentSurface(t *testing.T) {
	repo := openTestRepository(t)
	wantTables := []string{"instances", "media", "tracks", "search_states", "provider_states", "provider_cache", "pack_cache", "pack_members", "candidates", "installations", "notifications", "events", "media_hashes", "candidate_rejections"}
	for _, name := range wantTables {
		var count int
		if err := repo.store.db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?`, name).Scan(&count); err != nil || count != 1 {
			t.Fatalf("table %s count/error = %d/%v", name, count, err)
		}
	}
	wantColumns := map[string][]string{
		"media":         {"entity_id", "episode_title", "unsupported_reason"},
		"search_states": {"priority", "rerun_requested"},
		"installations": {"media_path", "media_file_id", "media_size", "media_mod_time_ns"},
		"notifications": {"dedupe_key", "lease_owner", "lease_until_ns"},
		"events":        {"event_id", "instance", "kind", "file_id", "entity_id"},
	}
	for table, columns := range wantColumns {
		rows, err := repo.store.db.Query(`PRAGMA table_info(` + table + `)`)
		if err != nil {
			t.Fatal(err)
		}
		got := map[string]bool{}
		for rows.Next() {
			var cid, notnull, pk int
			var name, typ string
			var defaultValue any
			if err := rows.Scan(&cid, &name, &typ, &notnull, &defaultValue, &pk); err != nil {
				t.Fatal(err)
			}
			got[name] = true
		}
		_ = rows.Close()
		for _, column := range columns {
			if !got[column] {
				t.Errorf("%s missing column %s", table, column)
			}
		}
	}
	wantIndexes := []string{"tracks_media_language_idx", "search_due_idx", "provider_cache_expiry_idx", "pack_lookup_idx", "pack_lru_idx", "notifications_dedupe_idx", "notifications_due_idx", "events_event_id_idx", "events_created_idx", "candidate_rejections_lookup_idx", "media_entity_identity_idx"}
	for _, name := range wantIndexes {
		var count int
		if err := repo.store.db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='index' AND name=?`, name).Scan(&count); err != nil || count != 1 {
			t.Fatalf("index %s count/error = %d/%v", name, count, err)
		}
	}
	wantForeignKeys := map[string]string{"tracks": "media", "search_states": "media", "pack_members": "pack_cache", "candidates": "media", "installations": "media", "events": "media", "media_hashes": "media", "candidate_rejections": "media"}
	for table, parent := range wantForeignKeys {
		rows, err := repo.store.db.Query(`PRAGMA foreign_key_list(` + table + `)`)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for rows.Next() {
			var id, seq int
			var target, from, to, onUpdate, onDelete, match string
			if err := rows.Scan(&id, &seq, &target, &from, &to, &onUpdate, &onDelete, &match); err != nil {
				t.Fatal(err)
			}
			found = found || target == parent
		}
		_ = rows.Close()
		if !found {
			t.Errorf("%s missing foreign key to %s", table, parent)
		}
	}
}

func TestMediaEntityIDRoundTrips(t *testing.T) {
	repo := openTestRepository(t)
	media := testMedia()
	media.EntityID = 101
	id, _, err := repo.UpsertMedia(context.Background(), media)
	if err != nil {
		t.Fatal(err)
	}
	got, err := repo.GetMedia(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if got.EntityID != 101 {
		t.Fatalf("entity ID = %d, want 101", got.EntityID)
	}
	foundID, _, found, err := repo.FindMediaByEntity(context.Background(), media.Ref.Instance, media.Ref.Kind, 101)
	if err != nil || !found || foundID != id {
		t.Fatalf("entity lookup = %d/%v/%v, want %d/true/nil", foundID, found, err, id)
	}
}

func TestEntityUpgradeUpdatesOneRowAndPreservesLease(t *testing.T) {
	repo := openTestRepository(t)
	ctx := context.Background()
	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	media := testMedia()
	media.EntityID = 101
	media.Ref.FileID = 1001
	media.Fingerprint.FileID = 1001
	media.Fingerprint.Path = "/media/show-old.mkv"
	first := MediaEventMutation{EventID: "import-1001", Type: "import", EntityID: 101, Media: media, Ref: media.Ref, Languages: []domain.Language{"hr"}, At: now}
	if applied, err := repo.ApplyMediaEvent(ctx, first); err != nil || !applied {
		t.Fatalf("first import = %v/%v", applied, err)
	}
	mediaID, _, err := repo.FindMedia(ctx, media.Ref)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.RecordCandidates(ctx, mediaID, "hr", []CandidateRecord{{ProviderID: "titlovi", ResultID: "1", MetadataJSON: []byte(`{}`), ScoreJSON: []byte(`{"total":50}`)}}); err != nil {
		t.Fatal(err)
	}
	if err := repo.RecordInstallation(ctx, Installation{MediaID: mediaID, Language: "hr", Path: "/media/show-old.hr.srt", Checksum: "checksum", ProviderID: "titlovi", CandidateID: "1", ScoreJSON: []byte(`{"total":50}`), SyncResultJSON: []byte(`{"verdict":"solid"}`), MediaPath: media.Fingerprint.Path, MediaFileID: 1001, MediaSize: media.Fingerprint.Size, MediaModTimeNS: media.Fingerprint.ModTime.UnixNano()}); err != nil {
		t.Fatal(err)
	}
	leases, err := repo.LeaseDueSearches(ctx, now, 1, time.Hour)
	if err != nil || len(leases) != 1 {
		t.Fatalf("leases = %#v/%v", leases, err)
	}

	oldRef := media.Ref
	media.Ref.FileID = 1002
	media.Fingerprint.FileID = 1002
	media.Fingerprint.Path = "/media/show-new.mkv"
	second := MediaEventMutation{EventID: "import-1002", Type: "import", EntityID: 101, Media: media, Ref: media.Ref, Languages: []domain.Language{"hr"}, At: now.Add(time.Minute)}
	if applied, err := repo.ApplyMediaEvent(ctx, second); err != nil || !applied {
		t.Fatalf("replacement import = %v/%v", applied, err)
	}

	var rows int
	if err := repo.store.db.QueryRow(`SELECT count(*) FROM media WHERE instance='sonarr-main' AND kind='episode' AND entity_id=101`).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("entity rows = %d/%v, want 1/nil", rows, err)
	}
	var eventEntityID int64
	if err := repo.store.db.QueryRow(`SELECT entity_id FROM events WHERE event_id='import-1002'`).Scan(&eventEntityID); err != nil || eventEntityID != 101 {
		t.Fatalf("event entity ID = %d/%v, want 101/nil", eventEntityID, err)
	}
	if _, _, err := repo.FindMedia(ctx, oldRef); err == nil {
		t.Fatal("old file identity still resolves")
	}
	newID, got, err := repo.FindMedia(ctx, media.Ref)
	if err != nil || newID != mediaID || got.EntityID != 101 {
		t.Fatalf("replacement lookup = %d/%#v/%v, want original row and entity", newID, got, err)
	}
	if err := repo.store.db.QueryRow(`SELECT count(*) FROM candidates WHERE media_id=?`, mediaID).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("candidate rows = %d/%v, want 0/nil", rows, err)
	}
	installation, found, err := repo.GetInstallation(ctx, mediaID, "hr")
	if err != nil || !found || string(installation.ScoreJSON) != "{}" || string(installation.SyncResultJSON) != "{}" || installation.MediaFileID != 0 {
		t.Fatalf("invalidated installation = %#v/%v/%v", installation, found, err)
	}
	var owner string
	var rerun bool
	if err := repo.store.db.QueryRow(`SELECT lease_owner, rerun_requested FROM search_states WHERE media_id=? AND language='hr'`, mediaID).Scan(&owner, &rerun); err != nil {
		t.Fatal(err)
	}
	if owner != leases[0].JobID || !rerun {
		t.Fatalf("lease owner/rerun = %q/%v, want %q/true", owner, rerun, leases[0].JobID)
	}
}

func TestMediaEntityIDConflictFailsClosed(t *testing.T) {
	repo := openTestRepository(t)
	first := testMedia()
	first.EntityID = 101
	first.Ref.FileID = 1001
	first.Fingerprint.FileID = 1001
	first.Fingerprint.Path = "/media/first.mkv"
	if _, _, err := repo.UpsertMedia(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	second := testMedia()
	second.EntityID = 202
	second.Ref.FileID = 1002
	second.Fingerprint.FileID = 1002
	second.Fingerprint.Path = "/media/second.mkv"
	if _, _, err := repo.UpsertMedia(context.Background(), second); err != nil {
		t.Fatal(err)
	}

	conflict := first
	conflict.Ref.FileID = 1002
	conflict.Fingerprint.FileID = 1002
	if _, _, err := repo.UpsertMedia(context.Background(), conflict); err == nil || !strings.Contains(err.Error(), "conflicting media identities") {
		t.Fatalf("identity conflict error = %v", err)
	}
}

func TestImportMutationRequiresExplicitEntityID(t *testing.T) {
	repo := openTestRepository(t)
	media := testMedia()
	media.EntityID = 101
	_, err := repo.ApplyMediaEvent(context.Background(), MediaEventMutation{
		EventID:   "missing-explicit-entity",
		Type:      "import",
		Ref:       media.Ref,
		Media:     media,
		Languages: []domain.Language{"hr"},
		At:        time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC),
	})
	if err == nil || !strings.Contains(err.Error(), "media event identity is incomplete") {
		t.Fatalf("missing entity error = %v", err)
	}
}

func TestMediaUnsupportedReasonRoundTrips(t *testing.T) {
	repo := openTestRepository(t)
	media := testMedia()
	media.UnsupportedReason = domain.UnsupportedMultiEpisode
	mediaID, _, err := repo.UpsertMedia(context.Background(), media)
	if err != nil {
		t.Fatal(err)
	}
	got, err := repo.GetMedia(context.Background(), mediaID)
	if err != nil {
		t.Fatal(err)
	}
	if got.UnsupportedReason != domain.UnsupportedMultiEpisode {
		t.Fatalf("unsupported reason = %q, want %q", got.UnsupportedReason, domain.UnsupportedMultiEpisode)
	}
}

func TestUnsupportedMediaEventCompletesSearchWithoutLease(t *testing.T) {
	repo := openTestRepository(t)
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	media := testMedia()
	media.UnsupportedReason = domain.UnsupportedMultiEpisode
	mutation := MediaEventMutation{EventID: "unsupported-import", Type: "import", EntityID: media.EntityID, Media: media, Ref: media.Ref, Languages: []domain.Language{"hr"}, At: now}
	if applied, err := repo.ApplyMediaEvent(context.Background(), mutation); err != nil || !applied {
		t.Fatalf("ApplyMediaEvent() = %v, %v", applied, err)
	}
	mediaID, _, err := repo.FindMedia(context.Background(), media.Ref)
	if err != nil {
		t.Fatal(err)
	}
	status, err := repo.GetSearchStatus(context.Background(), mediaID, "hr")
	if err != nil {
		t.Fatal(err)
	}
	if status.State != "complete" || status.LastOutcome != string(domain.UnsupportedMultiEpisode) || status.RerunPending {
		t.Fatalf("unsupported search status = %#v", status)
	}
	if leases, err := repo.LeaseDueSearches(context.Background(), now, 1, time.Minute); err != nil || len(leases) != 0 {
		t.Fatalf("unsupported leases = %#v, %v", leases, err)
	}
}

func TestUnsupportedMediaEventDuringLeasePreservesOneTerminalRerun(t *testing.T) {
	repo := openTestRepository(t)
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	media := testMedia()
	first := MediaEventMutation{EventID: "searchable-import", Type: "import", EntityID: media.EntityID, Media: media, Ref: media.Ref, Languages: []domain.Language{"hr"}, At: now}
	if applied, err := repo.ApplyMediaEvent(context.Background(), first); err != nil || !applied {
		t.Fatalf("first ApplyMediaEvent() = %v, %v", applied, err)
	}
	leases, err := repo.LeaseDueSearches(context.Background(), now, 1, 5*time.Minute)
	if err != nil || len(leases) != 1 {
		t.Fatalf("initial leases = %#v, %v", leases, err)
	}
	media.UnsupportedReason = domain.UnsupportedMultiEpisode
	second := MediaEventMutation{EventID: "unsupported-import", Type: "import", EntityID: media.EntityID, Media: media, Ref: media.Ref, Languages: []domain.Language{"hr"}, At: now.Add(time.Minute)}
	if applied, err := repo.ApplyMediaEvent(context.Background(), second); err != nil || !applied {
		t.Fatalf("second ApplyMediaEvent() = %v, %v", applied, err)
	}
	var owner string
	var rerun bool
	if err := repo.store.db.QueryRow(`SELECT lease_owner, rerun_requested FROM search_states WHERE media_id=? AND language='hr'`, leases[0].MediaID).Scan(&owner, &rerun); err != nil {
		t.Fatal(err)
	}
	if owner != leases[0].JobID || !rerun {
		t.Fatalf("owner/rerun = %q/%v, want %q/true", owner, rerun, leases[0].JobID)
	}
	result, err := repo.CompleteSearch(context.Background(), SearchCompletion{JobID: leases[0].JobID, Outcome: "installed"})
	if err != nil || !result.RerunScheduled {
		t.Fatalf("first completion = %#v, %v", result, err)
	}
	rerunLease, err := repo.LeaseDueSearches(context.Background(), now.Add(2*time.Minute), 1, 5*time.Minute)
	if err != nil || len(rerunLease) != 1 {
		t.Fatalf("terminal rerun lease = %#v, %v", rerunLease, err)
	}
	result, err = repo.CompleteSearch(context.Background(), SearchCompletion{JobID: rerunLease[0].JobID, Outcome: string(domain.UnsupportedMultiEpisode)})
	if err != nil || result.RerunScheduled {
		t.Fatalf("terminal completion = %#v, %v", result, err)
	}
	if third, err := repo.LeaseDueSearches(context.Background(), now.Add(24*time.Hour), 1, 5*time.Minute); err != nil || len(third) != 0 {
		t.Fatalf("unexpected third lease = %#v, %v", third, err)
	}
}

func TestRecordInstallationRejectsUnsupportedMedia(t *testing.T) {
	repo := openTestRepository(t)
	media := testMedia()
	mediaID, _, err := repo.UpsertMedia(context.Background(), media)
	if err != nil {
		t.Fatal(err)
	}
	media.UnsupportedReason = domain.UnsupportedMultiEpisode
	if _, _, err := repo.UpsertMedia(context.Background(), media); err != nil {
		t.Fatal(err)
	}
	err = repo.RecordInstallation(context.Background(), Installation{MediaID: mediaID, Language: "hr", Path: "/media/show.hr.srt", Checksum: "sum"})
	if err == nil || !strings.Contains(err.Error(), string(domain.UnsupportedMultiEpisode)) {
		t.Fatalf("RecordInstallation() error = %v", err)
	}
	if _, found, err := repo.GetInstallation(context.Background(), mediaID, "hr"); err != nil || found {
		t.Fatalf("GetInstallation() found/error = %v/%v, want false/nil", found, err)
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

func TestLeaseDueSearchesOrdersByPriorityBeforeDueTime(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	repo := openTestRepository(t)
	upgrade := insertTestMedia(t, repo, 1, now)
	missing := insertTestMedia(t, repo, 2, now)
	imported := insertTestMedia(t, repo, 3, now)
	requireSearchState(t, repo, upgrade, "en", now.Add(-3*time.Hour), SearchPriorityUpgrade)
	requireSearchState(t, repo, missing, "en", now.Add(-2*time.Hour), SearchPriorityMissing)
	requireSearchState(t, repo, imported, "en", now.Add(-time.Hour), SearchPriorityImport)

	leases, err := repo.LeaseDueSearches(context.Background(), now, 3, 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(leases) != 3 {
		t.Fatalf("lease count = %d, want 3", len(leases))
	}
	got := []int64{leases[0].MediaID, leases[1].MediaID, leases[2].MediaID}
	want := []int64{imported, missing, upgrade}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("lease order = %v, want %v", got, want)
		}
	}
	if leases[0].Priority != SearchPriorityImport || leases[1].Priority != SearchPriorityMissing || leases[2].Priority != SearchPriorityUpgrade {
		t.Fatalf("lease priorities = %v, %v, %v", leases[0].Priority, leases[1].Priority, leases[2].Priority)
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
	completionResult, err := repo.CompleteSearch(context.Background(), SearchCompletion{JobID: leases[0].JobID, Outcome: "missing", NextAttemptAt: next, AdvanceMissingAttempt: true, ResetFailureAttempt: true})
	if err != nil {
		t.Fatal(err)
	}
	if completionResult.RerunScheduled {
		t.Fatal("ordinary completion unexpectedly reported a rerun")
	}
	leases, err = repo.LeaseDueSearches(context.Background(), next, 1, 5*time.Minute)
	if err != nil || len(leases) != 1 || leases[0].Attempt != 1 || leases[0].FailureAttempt != 0 {
		t.Fatalf("missing completion lease = %#v, %v", leases, err)
	}
	if _, err := repo.CompleteSearch(context.Background(), SearchCompletion{JobID: leases[0].JobID, Outcome: "transport_error", NextAttemptAt: next.Add(time.Minute), AdvanceFailureAttempt: true}); err != nil {
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
	for attempt := range 2 {
		inserted, err := repo.EnqueueNotification(context.Background(), NotificationRequest{Notifier: "silo", DedupeKey: "install:1:sum", PayloadJSON: payload, NextAttemptAt: now})
		if err != nil {
			t.Fatal(err)
		}
		if inserted != (attempt == 0) {
			t.Fatalf("enqueue %d inserted = %t, want %t", attempt, inserted, attempt == 0)
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

func TestUpdateInstallationAssessmentPreservesArtifactAndRejectsStaleIdentity(t *testing.T) {
	repo := openTestRepository(t)
	media := testMedia()
	mediaID, _, err := repo.UpsertMedia(context.Background(), media)
	if err != nil {
		t.Fatal(err)
	}
	installation := Installation{MediaID: mediaID, Language: "en", Path: "/media/x.en.srt", Checksum: "sum", ProviderID: "provider", CandidateID: "candidate", ScoreJSON: []byte(`{"total":35}`), SyncResultJSON: []byte(`{"verdict":"solid"}`), MediaPath: media.Fingerprint.Path, MediaFileID: media.Fingerprint.FileID, MediaSize: media.Fingerprint.Size, MediaModTimeNS: media.Fingerprint.ModTime.UnixNano()}
	if err := repo.RecordInstallation(context.Background(), installation); err != nil {
		t.Fatal(err)
	}
	installation.ScoreJSON = []byte(`{"total":57}`)
	if err := repo.UpdateInstallationAssessment(context.Background(), installation); err != nil {
		t.Fatal(err)
	}
	got, found, err := repo.GetInstallation(context.Background(), mediaID, "en")
	if err != nil || !found || string(got.ScoreJSON) != `{"total":57}` || got.Path != installation.Path || got.Checksum != installation.Checksum {
		t.Fatalf("GetInstallation() = %#v/%v/%v", got, found, err)
	}
	var installs int
	if err := repo.store.db.QueryRow(`SELECT count(*) FROM events WHERE event_type = 'subtitle_installed'`).Scan(&installs); err != nil || installs != 1 {
		t.Fatalf("install audit count = %d/%v, want unchanged", installs, err)
	}
	stale := installation
	stale.Checksum = "replaced"
	if err := repo.UpdateInstallationAssessment(context.Background(), stale); err == nil {
		t.Fatal("stale assessment update unexpectedly succeeded")
	}
}

func TestInventoryFingerprintChangeInvalidatesInstallationProvenance(t *testing.T) {
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
	changed := media.Fingerprint
	changed.ModTime = changed.ModTime.Add(time.Second)
	if err := repo.ReplaceTrackInventory(context.Background(), mediaID, changed, nil); err != nil {
		t.Fatal(err)
	}
	got, found, err := repo.GetInstallation(context.Background(), mediaID, "en")
	if err != nil || !found || string(got.ScoreJSON) != "{}" || string(got.SyncResultJSON) != "{}" || got.MediaPath != "" || got.MediaFileID != 0 || got.MediaSize != 0 || got.MediaModTimeNS != 0 {
		t.Fatalf("invalidated installation = %#v/%v/%v", got, found, err)
	}
}

func TestCatalogRefreshPreservesAuthoritativeInventoryFingerprintAndInstallation(t *testing.T) {
	repo := openTestRepository(t)
	catalogMedia := testMedia()
	mediaID, _, err := repo.UpsertMedia(context.Background(), catalogMedia)
	if err != nil {
		t.Fatal(err)
	}
	authoritative := catalogMedia.Fingerprint
	authoritative.ModTime = authoritative.ModTime.Add(2 * time.Hour)
	if err := repo.ReplaceTrackInventory(context.Background(), mediaID, authoritative, nil); err != nil {
		t.Fatal(err)
	}
	installation := Installation{MediaID: mediaID, Language: "en", Path: "/media/x.en.srt", Checksum: "sum", ScoreJSON: []byte(`{"total":70}`), SyncResultJSON: []byte(`{"verdict":"solid"}`), MediaPath: authoritative.Path, MediaFileID: authoritative.FileID, MediaSize: authoritative.Size, MediaModTimeNS: authoritative.ModTime.UnixNano()}
	if err := repo.RecordInstallation(context.Background(), installation); err != nil {
		t.Fatal(err)
	}
	if _, changed, err := repo.UpsertMedia(context.Background(), catalogMedia); err != nil || changed {
		t.Fatalf("catalog refresh changed = %v, err = %v", changed, err)
	}
	gotMedia, err := repo.GetMedia(context.Background(), mediaID)
	if err != nil || !gotMedia.Fingerprint.ModTime.Equal(authoritative.ModTime) {
		t.Fatalf("media fingerprint = %#v, err = %v", gotMedia.Fingerprint, err)
	}
	got, found, err := repo.GetInstallation(context.Background(), mediaID, "en")
	if err != nil || !found || got.MediaPath != authoritative.Path || got.MediaFileID != authoritative.FileID || got.MediaSize != authoritative.Size || got.MediaModTimeNS != authoritative.ModTime.UnixNano() || string(got.ScoreJSON) != `{"total":70}` || string(got.SyncResultJSON) != `{"verdict":"solid"}` {
		t.Fatalf("installation = %#v/%v/%v", got, found, err)
	}
}

func TestCatalogEventPreservesAuthoritativeInventoryFingerprintAndInstallation(t *testing.T) {
	repo := openTestRepository(t)
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	catalogMedia := testMedia()
	if _, err := repo.ApplyMediaEvent(context.Background(), MediaEventMutation{EventID: "initial-import", Type: "import", EntityID: catalogMedia.EntityID, Media: catalogMedia, Ref: catalogMedia.Ref, Languages: []domain.Language{"en"}, At: now}); err != nil {
		t.Fatal(err)
	}
	mediaID, stored, err := repo.FindMedia(context.Background(), catalogMedia.Ref)
	if err != nil {
		t.Fatal(err)
	}
	authoritative := stored.Fingerprint
	authoritative.ModTime = authoritative.ModTime.Add(2 * time.Hour)
	if err := repo.ReplaceTrackInventory(context.Background(), mediaID, authoritative, nil); err != nil {
		t.Fatal(err)
	}
	installation := Installation{MediaID: mediaID, Language: "en", Path: "/media/x.en.srt", Checksum: "sum", ScoreJSON: []byte(`{"total":70}`), SyncResultJSON: []byte(`{"verdict":"solid"}`), MediaPath: authoritative.Path, MediaFileID: authoritative.FileID, MediaSize: authoritative.Size, MediaModTimeNS: authoritative.ModTime.UnixNano()}
	if err := repo.RecordInstallation(context.Background(), installation); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.ApplyMediaEvent(context.Background(), MediaEventMutation{EventID: "repeat-import", Type: "import", EntityID: catalogMedia.EntityID, Media: catalogMedia, Ref: catalogMedia.Ref, Languages: []domain.Language{"en"}, At: now.Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	gotMedia, err := repo.GetMedia(context.Background(), mediaID)
	if err != nil || !gotMedia.Fingerprint.ModTime.Equal(authoritative.ModTime) {
		t.Fatalf("media fingerprint = %#v, err = %v", gotMedia.Fingerprint, err)
	}
	got, found, err := repo.GetInstallation(context.Background(), mediaID, "en")
	if err != nil || !found || got.MediaPath != authoritative.Path || got.MediaFileID != authoritative.FileID || got.MediaSize != authoritative.Size || got.MediaModTimeNS != authoritative.ModTime.UnixNano() || string(got.ScoreJSON) != `{"total":70}` || string(got.SyncResultJSON) != `{"verdict":"solid"}` {
		t.Fatalf("installation = %#v/%v/%v", got, found, err)
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
	if _, err := repo.ApplyMediaEvent(context.Background(), MediaEventMutation{EventID: "import-before-rename", Type: "import", EntityID: media.EntityID, Media: media, Ref: media.Ref, Languages: []domain.Language{"en"}, At: now}); err != nil {
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
	if _, err := repo.ApplyMediaEvent(context.Background(), MediaEventMutation{EventID: "rename-1", Type: "rename", EntityID: media.EntityID, Media: media, Ref: media.Ref, Languages: []domain.Language{"en"}, At: now.Add(time.Minute)}); err != nil {
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

func TestCandidateRejectionMatchesTheSameCandidateArtifactAndMedia(t *testing.T) {
	repo := openTestRepository(t)
	media := testMedia()
	mediaID, _, err := repo.UpsertMedia(context.Background(), media)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	rejection := CandidateRejection{
		MediaID: mediaID, Language: "en", ProviderID: "opensubtitles", ResultID: "result-1",
		CandidateSignature: "candidate-a", ArtifactChecksum: "member-a", ReasonCode: "lapse_unsure", ToolSignature: "lapse-2.0.5/policy-a",
		MediaPath: media.Fingerprint.Path, MediaFileID: media.Fingerprint.FileID, MediaSize: media.Fingerprint.Size, MediaModTimeNS: media.Fingerprint.ModTime.UnixNano(),
		RejectedAt: now, ExpiresAt: now.Add(30 * 24 * time.Hour),
	}
	if err := repo.PutCandidateRejection(context.Background(), rejection); err != nil {
		t.Fatal(err)
	}

	lookup := CandidateRejectionLookup{
		MediaID: mediaID, Language: "en", ProviderID: "opensubtitles", ResultID: "result-1",
		CandidateSignature: "candidate-a", ArtifactChecksum: "member-a", ToolSignature: "lapse-2.0.5/policy-a",
		MediaPath: media.Fingerprint.Path, MediaFileID: media.Fingerprint.FileID, MediaSize: media.Fingerprint.Size, MediaModTimeNS: media.Fingerprint.ModTime.UnixNano(), Now: now,
	}
	got, found, err := repo.GetCandidateRejection(context.Background(), lookup)
	if err != nil || !found || got.ReasonCode != "lapse_unsure" {
		t.Fatalf("GetCandidateRejection() = %#v/%v/%v", got, found, err)
	}
	lookup.ArtifactChecksum = "member-b"
	if _, found, err := repo.GetCandidateRejection(context.Background(), lookup); err != nil || found {
		t.Fatalf("different artifact matched = %v/%v", found, err)
	}
	lookup.ArtifactChecksum = ""
	if _, found, err := repo.GetCandidateRejection(context.Background(), lookup); err != nil || !found {
		t.Fatalf("pre-download candidate lookup = %v/%v", found, err)
	}
	lookup.ArtifactChecksum = "member-a"
	lookup.MediaSize++
	if _, found, err := repo.GetCandidateRejection(context.Background(), lookup); err != nil || found {
		t.Fatalf("changed media fingerprint matched = %v/%v", found, err)
	}
	lookup.MediaSize--
	lookup.ToolSignature = "lapse-3/policy-b"
	if _, found, err := repo.GetCandidateRejection(context.Background(), lookup); err != nil || found {
		t.Fatalf("changed tool policy matched = %v/%v", found, err)
	}
	lookup.ToolSignature = "lapse-2.0.5/policy-a"
	lookup.Now = rejection.ExpiresAt
	if _, found, err := repo.GetCandidateRejection(context.Background(), lookup); err != nil || found {
		t.Fatalf("expired rejection matched = %v/%v", found, err)
	}
}

func TestCandidateRejectionsExpireAndCanBeClearedForManualRetry(t *testing.T) {
	repo := openTestRepository(t)
	media := testMedia()
	mediaID, _, _ := repo.UpsertMedia(context.Background(), media)
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	rejection := CandidateRejection{
		MediaID: mediaID, Language: "en", ProviderID: "subdl", ResultID: "bad", CandidateSignature: "candidate", ArtifactChecksum: "artifact", ReasonCode: "lapse_nothing", ToolSignature: "tool",
		MediaPath: media.Fingerprint.Path, MediaFileID: media.Fingerprint.FileID, MediaSize: media.Fingerprint.Size, MediaModTimeNS: media.Fingerprint.ModTime.UnixNano(), RejectedAt: now, ExpiresAt: now.Add(time.Hour),
	}
	if err := repo.PutCandidateRejection(context.Background(), rejection); err != nil {
		t.Fatal(err)
	}
	listed, err := repo.ListCandidateRejections(context.Background(), mediaID, "en", now)
	if err != nil || len(listed) != 1 || listed[0].ResultID != "bad" {
		t.Fatalf("ListCandidateRejections() = %#v/%v", listed, err)
	}
	if listed, err = repo.ListCandidateRejections(context.Background(), mediaID, "en", rejection.ExpiresAt); err != nil || len(listed) != 0 {
		t.Fatalf("expired rejections = %#v/%v", listed, err)
	}
	if err := repo.ClearCandidateRejections(context.Background(), mediaID, "en"); err != nil {
		t.Fatal(err)
	}
	if listed, err = repo.ListCandidateRejections(context.Background(), mediaID, "en", now); err != nil || len(listed) != 0 {
		t.Fatalf("cleared rejections = %#v/%v", listed, err)
	}
}

func TestApplyMediaEventIsIdempotentAndResetsConfiguredLanguages(t *testing.T) {
	repo := openTestRepository(t)
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	media := testMedia()
	media.Fingerprint = domain.MediaFingerprint{Path: "/media/episode.mkv", FileID: media.Ref.FileID, Size: 100, ModTime: now}
	mutation := MediaEventMutation{EventID: "event-1", Type: "import", EntityID: media.EntityID, Media: media, Ref: media.Ref, Languages: []domain.Language{"hr", "en"}, At: now}

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

func TestApplyMediaEventDuringLeaseRequestsOneRerun(t *testing.T) {
	repo := openTestRepository(t)
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	media := testMedia()
	media.Fingerprint = domain.MediaFingerprint{Path: "/media/episode.mkv", FileID: media.Ref.FileID, Size: 100, ModTime: now}
	firstEvent := MediaEventMutation{EventID: "import-1", Type: "import", EntityID: media.EntityID, Media: media, Ref: media.Ref, Languages: []domain.Language{"hr"}, At: now}
	if applied, err := repo.ApplyMediaEvent(context.Background(), firstEvent); err != nil || !applied {
		t.Fatalf("first event = %v, %v", applied, err)
	}
	leases, err := repo.LeaseDueSearches(context.Background(), now, 1, 5*time.Minute)
	if err != nil || len(leases) != 1 {
		t.Fatalf("first lease = %#v, %v", leases, err)
	}

	secondEvent := firstEvent
	secondEvent.EventID = "import-2"
	secondEvent.At = now.Add(time.Minute)
	if applied, err := repo.ApplyMediaEvent(context.Background(), secondEvent); err != nil || !applied {
		t.Fatalf("second event = %v, %v", applied, err)
	}
	var owner string
	var rerunRequested bool
	if err := repo.store.db.QueryRow(`SELECT lease_owner, rerun_requested FROM search_states WHERE media_id=? AND language='hr'`, leases[0].MediaID).Scan(&owner, &rerunRequested); err != nil {
		t.Fatal(err)
	}
	if owner != leases[0].JobID || !rerunRequested {
		t.Fatalf("active owner/rerun = %q/%v, want %q/true", owner, rerunRequested, leases[0].JobID)
	}
	if competing, err := repo.LeaseDueSearches(context.Background(), secondEvent.At, 1, 5*time.Minute); err != nil || len(competing) != 0 {
		t.Fatalf("competing lease = %#v, %v", competing, err)
	}

	completionResult, err := repo.CompleteSearch(context.Background(), SearchCompletion{JobID: leases[0].JobID, Outcome: "installed", NextAttemptAt: now.Add(24 * time.Hour), Priority: SearchPriorityUpgrade})
	if err != nil {
		t.Fatal(err)
	}
	if !completionResult.RerunScheduled {
		t.Fatal("completion did not report the consumed same-key rerun")
	}
	rerun, err := repo.LeaseDueSearches(context.Background(), now.Add(2*time.Minute), 1, 5*time.Minute)
	if err != nil || len(rerun) != 1 {
		t.Fatalf("rerun lease = %#v, %v", rerun, err)
	}
	if rerun[0].Priority != SearchPriorityImport {
		t.Fatalf("rerun priority = %d, want %d", rerun[0].Priority, SearchPriorityImport)
	}
	completionResult, err = repo.CompleteSearch(context.Background(), SearchCompletion{JobID: rerun[0].JobID, Outcome: "satisfied"})
	if err != nil {
		t.Fatal(err)
	}
	if completionResult.RerunScheduled {
		t.Fatal("second completion unexpectedly reported another rerun")
	}
	if third, err := repo.LeaseDueSearches(context.Background(), now.Add(48*time.Hour), 1, 5*time.Minute); err != nil || len(third) != 0 {
		t.Fatalf("unexpected third lease = %#v, %v", third, err)
	}
}

func TestChangedImportInvalidatesCandidatesAndDeleteCancelsSearches(t *testing.T) {
	repo := openTestRepository(t)
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	media := testMedia()
	media.Fingerprint = domain.MediaFingerprint{Path: "/media/episode.mkv", FileID: media.Ref.FileID, Size: 100, ModTime: now}
	if _, err := repo.ApplyMediaEvent(context.Background(), MediaEventMutation{EventID: "import-1", Type: "import", EntityID: media.EntityID, Media: media, Ref: media.Ref, Languages: []domain.Language{"hr"}, At: now}); err != nil {
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
	if _, err := repo.ApplyMediaEvent(context.Background(), MediaEventMutation{EventID: "import-2", Type: "import", EntityID: media.EntityID, Media: media, Ref: media.Ref, Languages: []domain.Language{"hr"}, At: now.Add(time.Minute)}); err != nil {
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

func TestCommitReconciliationSchedulesMissingPriority(t *testing.T) {
	repo := openTestRepository(t)
	ctx := context.Background()
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	if err := repo.EnsureInstance(ctx, "sonarr-main", "sonarr", "http://sonarr:8989", now); err != nil {
		t.Fatal(err)
	}
	media := testMedia()
	mutation := MediaEventMutation{EventID: "reconcile:sonarr-main:1", Type: "import", EntityID: media.EntityID, Ref: media.Ref, Media: media, Languages: []domain.Language{"hr"}, At: now, Priority: SearchPriorityMissing}
	if err := repo.CommitReconciliation(ctx, "sonarr-main", now, []MediaEventMutation{mutation}); err != nil {
		t.Fatal(err)
	}
	var priority SearchPriority
	if err := repo.store.db.QueryRow(`SELECT priority FROM search_states WHERE language='hr'`).Scan(&priority); err != nil {
		t.Fatal(err)
	}
	if priority != SearchPriorityMissing {
		t.Fatalf("reconciliation priority = %d, want %d", priority, SearchPriorityMissing)
	}
}

func TestCommitReconciliationPreservesActiveLeaseAndRequestsOneRerun(t *testing.T) {
	repo := openTestRepository(t)
	ctx := context.Background()
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	if err := repo.EnsureInstance(ctx, "sonarr-main", "sonarr", "http://sonarr:8989", now); err != nil {
		t.Fatal(err)
	}
	media := testMedia()
	initial := MediaEventMutation{EventID: "webhook-import", Type: "import", EntityID: media.EntityID, Ref: media.Ref, Media: media, Languages: []domain.Language{"hr"}, At: now}
	if _, err := repo.ApplyMediaEvent(ctx, initial); err != nil {
		t.Fatal(err)
	}
	leases, err := repo.LeaseDueSearches(ctx, now, 1, 5*time.Minute)
	if err != nil || len(leases) != 1 {
		t.Fatalf("initial lease = %#v, %v", leases, err)
	}
	mutation := MediaEventMutation{EventID: "reconcile:sonarr-main:51", Type: "rename", EntityID: media.EntityID, Ref: media.Ref, Media: media, Languages: []domain.Language{"hr"}, At: now.Add(time.Minute), Priority: SearchPriorityMissing}
	if err := repo.CommitReconciliation(ctx, "sonarr-main", now.Add(2*time.Minute), []MediaEventMutation{mutation}); err != nil {
		t.Fatal(err)
	}
	var owner string
	var leaseUntil int64
	var rerun bool
	var priority SearchPriority
	if err := repo.store.db.QueryRow(`SELECT lease_owner, lease_until_ns, rerun_requested, priority FROM search_states WHERE media_id=? AND language='hr'`, leases[0].MediaID).Scan(&owner, &leaseUntil, &rerun, &priority); err != nil {
		t.Fatal(err)
	}
	if owner != leases[0].JobID || leaseUntil != leases[0].LeaseUntil.UnixNano() || !rerun || priority != SearchPriorityImport {
		t.Fatalf("owner/until/rerun/priority = %q/%d/%v/%d", owner, leaseUntil, rerun, priority)
	}
	result, err := repo.CompleteSearch(ctx, SearchCompletion{JobID: leases[0].JobID, Outcome: "satisfied"})
	if err != nil || !result.RerunScheduled {
		t.Fatalf("completion = %#v, %v", result, err)
	}
	if rerunLease, err := repo.LeaseDueSearches(ctx, now.Add(3*time.Minute), 2, 5*time.Minute); err != nil || len(rerunLease) != 1 {
		t.Fatalf("rerun lease = %#v, %v", rerunLease, err)
	}
}

func TestCommitReconciliationAppliesDeletesAndCursorAtomically(t *testing.T) {
	repo := openTestRepository(t)
	ctx := context.Background()
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	if err := repo.EnsureInstance(ctx, "sonarr-main", "sonarr", "http://sonarr:8989", now); err != nil {
		t.Fatal(err)
	}
	media := testMedia()
	if _, err := repo.ApplyMediaEvent(ctx, MediaEventMutation{EventID: "initial", Type: "import", EntityID: media.EntityID, Ref: media.Ref, Media: media, Languages: []domain.Language{"hr"}, At: now}); err != nil {
		t.Fatal(err)
	}
	pageEnd := now.Add(time.Hour)
	mutations := []MediaEventMutation{
		{EventID: "reconcile:sonarr-main:61", Type: "delete", Ref: media.Ref, At: now.Add(time.Minute), Priority: SearchPriorityMissing},
		{EventID: "reconcile:sonarr-main:62", Type: "delete", Ref: domain.MediaRef{Instance: "sonarr-main", Kind: domain.MediaEpisode, FileID: 999}, At: now.Add(2 * time.Minute), Priority: SearchPriorityMissing},
	}
	if err := repo.CommitReconciliation(ctx, "sonarr-main", pageEnd, mutations); err != nil {
		t.Fatal(err)
	}
	mediaID, _, err := repo.FindMedia(ctx, media.Ref)
	if err != nil {
		t.Fatal(err)
	}
	status, err := repo.GetSearchStatus(ctx, mediaID, "hr")
	if err != nil || status.State != "complete" || status.LastOutcome != "deleted" {
		t.Fatalf("deleted status = %#v, %v", status, err)
	}
	if cursor, err := repo.GetReconciliationCursor(ctx, "sonarr-main"); err != nil || !cursor.Equal(pageEnd) {
		t.Fatalf("cursor = %s, %v", cursor, err)
	}
	var audits int
	if err := repo.store.db.QueryRow(`SELECT count(*) FROM events WHERE event_id IN ('reconcile:sonarr-main:61','reconcile:sonarr-main:62')`).Scan(&audits); err != nil || audits != 2 {
		t.Fatalf("reconciliation audits = %d, %v", audits, err)
	}

	newMedia := media
	newMedia.EntityID = 77
	newMedia.Ref.FileID = 77
	newMedia.Fingerprint.FileID = 77
	newMedia.Fingerprint.Path = "/media/new.mkv"
	valid := MediaEventMutation{EventID: "reconcile:sonarr-main:63", Type: "import", EntityID: newMedia.EntityID, Ref: newMedia.Ref, Media: newMedia, Languages: []domain.Language{"hr"}, At: pageEnd, Priority: SearchPriorityMissing}
	bad := MediaEventMutation{EventID: "reconcile:other:64", Type: "delete", Ref: domain.MediaRef{Instance: "other", Kind: domain.MediaEpisode, FileID: 5}, At: pageEnd.Add(time.Minute), Priority: SearchPriorityMissing}
	if err := repo.CommitReconciliation(ctx, "sonarr-main", pageEnd.Add(time.Hour), []MediaEventMutation{valid, bad}); err == nil {
		t.Fatal("mismatched instance reconciliation error = nil")
	}
	if cursor, err := repo.GetReconciliationCursor(ctx, "sonarr-main"); err != nil || !cursor.Equal(pageEnd) {
		t.Fatalf("cursor after failed page = %s, %v", cursor, err)
	}
	if _, _, err := repo.FindMedia(ctx, newMedia.Ref); err == nil {
		t.Fatal("earlier media mutation survived failed page")
	}
	if err := repo.store.db.QueryRow(`SELECT count(*) FROM events WHERE event_id='reconcile:sonarr-main:63'`).Scan(&audits); err != nil || audits != 0 {
		t.Fatalf("earlier audit survived failed page = %d, %v", audits, err)
	}
}

func TestCommitReconciliationResolvesKnownAndUnknownEntityDeletes(t *testing.T) {
	repo := openTestRepository(t)
	ctx := context.Background()
	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	if err := repo.EnsureInstance(ctx, "sonarr-main", "sonarr", "http://sonarr.invalid", now); err != nil {
		t.Fatal(err)
	}
	media := testMedia()
	media.EntityID = 101
	media.Ref.FileID = 1001
	media.Fingerprint.FileID = 1001
	if _, err := repo.ApplyMediaEvent(ctx, MediaEventMutation{EventID: "seed-101", Type: "import", EntityID: 101, Ref: media.Ref, Media: media, Languages: []domain.Language{"hr"}, At: now.Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	mediaID, _, err := repo.FindMedia(ctx, media.Ref)
	if err != nil {
		t.Fatal(err)
	}

	mutations := []MediaEventMutation{
		{EventID: "reconcile:sonarr-main:42", Type: "delete", EntityID: 101, Ref: domain.MediaRef{Instance: "sonarr-main", Kind: domain.MediaEpisode}, At: now, Priority: SearchPriorityMissing},
		{EventID: "reconcile:sonarr-main:43", Type: "delete", EntityID: 999, Ref: domain.MediaRef{Instance: "sonarr-main", Kind: domain.MediaEpisode}, At: now.Add(time.Minute), Priority: SearchPriorityMissing},
	}
	if err := repo.CommitReconciliation(ctx, "sonarr-main", now.Add(2*time.Minute), mutations); err != nil {
		t.Fatal(err)
	}
	status, err := repo.GetSearchStatus(ctx, mediaID, "hr")
	if err != nil || status.State != "complete" || status.LastOutcome != "deleted" {
		t.Fatalf("known delete status = %#v/%v", status, err)
	}
	var knownEntity, knownFile int64
	var knownMedia sql.NullInt64
	if err := repo.store.db.QueryRow(`SELECT entity_id, file_id, media_id FROM events WHERE event_id='reconcile:sonarr-main:42'`).Scan(&knownEntity, &knownFile, &knownMedia); err != nil {
		t.Fatal(err)
	}
	if knownEntity != 101 || knownFile != 1001 || !knownMedia.Valid || knownMedia.Int64 != mediaID {
		t.Fatalf("known audit = entity:%d file:%d media:%#v", knownEntity, knownFile, knownMedia)
	}
	var unknownEntity, unknownFile int64
	var unknownMedia sql.NullInt64
	if err := repo.store.db.QueryRow(`SELECT entity_id, file_id, media_id FROM events WHERE event_id='reconcile:sonarr-main:43'`).Scan(&unknownEntity, &unknownFile, &unknownMedia); err != nil {
		t.Fatal(err)
	}
	if unknownEntity != 999 || unknownFile != 0 || unknownMedia.Valid {
		t.Fatalf("unknown audit = entity:%d file:%d media:%#v", unknownEntity, unknownFile, unknownMedia)
	}
	if err := repo.CommitReconciliation(ctx, "sonarr-main", now.Add(3*time.Minute), mutations); err != nil {
		t.Fatal(err)
	}
	var audits int
	if err := repo.store.db.QueryRow(`SELECT count(*) FROM events WHERE event_id IN ('reconcile:sonarr-main:42','reconcile:sonarr-main:43')`).Scan(&audits); err != nil || audits != 2 {
		t.Fatalf("replayed audit count = %d/%v, want 2/nil", audits, err)
	}
}

func TestCommitReconciliationEntityDeleteRollsBackWithLaterMismatch(t *testing.T) {
	repo := openTestRepository(t)
	ctx := context.Background()
	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	if err := repo.EnsureInstance(ctx, "sonarr-main", "sonarr", "http://sonarr.invalid", now); err != nil {
		t.Fatal(err)
	}
	media := testMedia()
	if _, err := repo.ApplyMediaEvent(ctx, MediaEventMutation{EventID: "seed", Type: "import", EntityID: media.EntityID, Ref: media.Ref, Media: media, Languages: []domain.Language{"hr"}, At: now.Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	mediaID, _, err := repo.FindMedia(ctx, media.Ref)
	if err != nil {
		t.Fatal(err)
	}
	mutations := []MediaEventMutation{
		{EventID: "reconcile:sonarr-main:50", Type: "delete", EntityID: media.EntityID, Ref: domain.MediaRef{Instance: "sonarr-main", Kind: domain.MediaEpisode}, At: now, Priority: SearchPriorityMissing},
		{EventID: "reconcile:other:51", Type: "delete", EntityID: 999, Ref: domain.MediaRef{Instance: "other", Kind: domain.MediaEpisode}, At: now, Priority: SearchPriorityMissing},
	}
	if err := repo.CommitReconciliation(ctx, "sonarr-main", now.Add(time.Hour), mutations); err == nil {
		t.Fatal("CommitReconciliation() error = nil")
	}
	status, err := repo.GetSearchStatus(ctx, mediaID, "hr")
	if err != nil || status.State != "pending" {
		t.Fatalf("rolled-back search status = %#v/%v", status, err)
	}
	var audits int
	if err := repo.store.db.QueryRow(`SELECT count(*) FROM events WHERE event_id='reconcile:sonarr-main:50'`).Scan(&audits); err != nil || audits != 0 {
		t.Fatalf("rolled-back audits = %d/%v", audits, err)
	}
	cursor, err := repo.GetReconciliationCursor(ctx, "sonarr-main")
	if err != nil || !cursor.IsZero() {
		t.Fatalf("rolled-back cursor = %s/%v", cursor, err)
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

func insertTestMedia(t *testing.T, repo *Repository, fileID int64, now time.Time) int64 {
	t.Helper()
	media := testMedia()
	media.EntityID = fileID
	media.Ref.FileID = fileID
	media.Fingerprint.FileID = fileID
	media.Fingerprint.Path = filepath.Join("/media", "show-"+time.Unix(fileID, 0).UTC().Format("150405")+".mkv")
	media.Fingerprint.ModTime = now
	id, _, err := repo.UpsertMedia(context.Background(), media)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func requireSearchState(t *testing.T, repo *Repository, mediaID int64, language domain.Language, next time.Time, priority SearchPriority) {
	t.Helper()
	if err := repo.UpsertSearchStateWithPriority(context.Background(), mediaID, language, next, priority); err != nil {
		t.Fatal(err)
	}
}

func testMedia() domain.Media {
	return domain.Media{
		EntityID:    101,
		Ref:         domain.MediaRef{Instance: "sonarr-main", Kind: domain.MediaEpisode, FileID: 42},
		Fingerprint: domain.MediaFingerprint{Path: "/media/show.mkv", FileID: 42, Size: 100, ModTime: time.Date(2026, 9, 4, 11, 0, 0, 123, time.UTC)},
		Title:       "Show", Season: 1, Episode: 2, ExternalIDs: domain.ExternalIDs{TVDB: 1},
	}
}
