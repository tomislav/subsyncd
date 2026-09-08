package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"subsyncd/internal/domain"
)

func TestLibraryDiscoveryCreatesMissingWork(t *testing.T) {
	repo := openTestRepository(t)
	ctx := t.Context()
	at := time.Now().UTC()
	if err := repo.EnsureInstance(ctx, "sonarr-main", "sonarr", "http://sonarr", at); err != nil {
		t.Fatal(err)
	}
	scope, revision, err := repo.LibraryDiscoveryState(ctx, "sonarr-main")
	if err != nil || scope != "" || revision != 0 {
		t.Fatalf("initial state = %q/%d/%v", scope, revision, err)
	}
	first := testMedia()
	second := first
	second.EntityID++
	second.Ref.FileID++
	second.Fingerprint.FileID++
	second.UnsupportedReason = domain.UnsupportedMultiEpisode
	if err := repo.CommitLibraryDiscovery(ctx, "sonarr-main", "scope-one", revision, []domain.Media{first, second}, []domain.Language{"hr", "en"}, at); err != nil {
		t.Fatal(err)
	}
	for _, item := range []domain.Media{first, second} {
		id, _, err := repo.FindMedia(ctx, item.Ref)
		if err != nil {
			t.Fatal(err)
		}
		for _, lang := range []domain.Language{"hr", "en"} {
			state, err := repo.GetSearchStatus(ctx, id, lang)
			if err != nil {
				t.Fatal(err)
			}
			if state.Priority != SearchPriorityMissing {
				t.Fatalf("priority = %v", state.Priority)
			}
			if item.UnsupportedReason == "" && (state.State != "pending" || !state.NextAttemptAt.Equal(at)) {
				t.Fatalf("missing state = %+v", state)
			}
			if item.UnsupportedReason != "" && (state.State != "complete" || state.LastOutcome != string(item.UnsupportedReason)) {
				t.Fatalf("unsupported state = %+v", state)
			}
		}
	}
	scope, revision, err = repo.LibraryDiscoveryState(ctx, "sonarr-main")
	if err != nil || scope != "scope-one" || revision != 2 {
		t.Fatalf("completed state = %q/%d/%v", scope, revision, err)
	}
}

func TestLibraryDiscoveryRejectsInvalidBatchAtomically(t *testing.T) {
	for _, scenario := range []string{"scope", "time", "language", "noncanonical", "instance", "kind", "entity", "file", "fingerprint", "unsupported"} {
		t.Run(scenario, func(t *testing.T) {
			repo := openTestRepository(t)
			ctx := t.Context()
			at := time.Now().UTC()
			if err := repo.EnsureInstance(ctx, "sonarr-main", "sonarr", "http://sonarr", at); err != nil {
				t.Fatal(err)
			}
			first, invalid := testMedia(), testMedia()
			invalid.EntityID++
			invalid.Ref.FileID++
			invalid.Fingerprint.FileID++
			scope := "scope"
			langs := []domain.Language{"hr"}
			switch scenario {
			case "scope":
				scope = ""
			case "time":
				at = time.Time{}
			case "language":
				langs = []domain.Language{"invalid_language"}
			case "noncanonical":
				langs = []domain.Language{"eng"}
			case "instance":
				invalid.Ref.Instance = "another"
			case "kind":
				invalid.Ref.Kind = domain.MediaMovie
			case "entity":
				invalid.EntityID = 0
			case "file":
				invalid.Ref.FileID = 0
			case "fingerprint":
				invalid.Fingerprint.FileID++
			case "unsupported":
				invalid.UnsupportedReason = "unknown"
			}
			if err := repo.CommitLibraryDiscovery(ctx, "sonarr-main", scope, 0, []domain.Media{first, invalid}, langs, at); err == nil {
				t.Fatal("invalid discovery committed")
			}
			var count int
			if err := repo.store.db.QueryRow(`SELECT count(*) FROM media`).Scan(&count); err != nil || count != 0 {
				t.Fatalf("partial media persisted: %d/%v", count, err)
			}
			marker, revision, err := repo.LibraryDiscoveryState(ctx, "sonarr-main")
			if err != nil || marker != "" || revision != 0 {
				t.Fatalf("partial state persisted: %q/%d/%v", marker, revision, err)
			}
		})
	}
}

func TestLibraryDiscoveryEventFenceIncludesUnknownDeletes(t *testing.T) {
	for _, series := range []bool{false, true} {
		t.Run(fmt.Sprint(series), func(t *testing.T) {
			repo := openTestRepository(t)
			ctx, at := t.Context(), time.Now().UTC()
			if err := repo.EnsureInstance(ctx, "sonarr-main", "sonarr", "http://sonarr", at); err != nil {
				t.Fatal(err)
			}
			_, revision, err := repo.LibraryDiscoveryState(ctx, "sonarr-main")
			if err != nil {
				t.Fatal(err)
			}
			mutation := MediaEventMutation{EventID: "unknown-delete", Type: "delete", Ref: domain.MediaRef{Instance: "sonarr-main", Kind: domain.MediaEpisode}, EntityID: 999, At: at}
			if series {
				mutation.EntityID = 0
				mutation.SeriesID = 77
			}
			if _, err := repo.ApplyMediaEvent(ctx, mutation); err != nil {
				t.Fatal(err)
			}
			if err := repo.CommitLibraryDiscovery(ctx, "sonarr-main", "scope", revision, []domain.Media{testMedia()}, []domain.Language{"hr"}, at); !errors.Is(err, ErrLibraryDiscoveryStale) {
				t.Fatalf("stale error = %v", err)
			}
			marker, after, err := repo.LibraryDiscoveryState(ctx, "sonarr-main")
			if err != nil || marker != "" || after != revision+1 {
				t.Fatalf("fence = %q/%d/%v", marker, after, err)
			}
			if _, err := repo.ApplyMediaEvent(ctx, mutation); err != nil {
				t.Fatal(err)
			}
			_, replayRevision, _ := repo.LibraryDiscoveryState(ctx, "sonarr-main")
			if replayRevision != after {
				t.Fatal("redelivery advanced revision")
			}
		})
	}
}

func TestLibraryDiscoveryEmptyCompletionSurvivesRestartAndReadOnly(t *testing.T) {
	ctx, at := t.Context(), time.Now().UTC()
	path := filepath.Join(t.TempDir(), "discovery.db")
	database, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	repo := database.Repository()
	if err := repo.EnsureInstance(ctx, "radarr", "radarr", "http://radarr", at); err != nil {
		t.Fatal(err)
	}
	if err := repo.CommitLibraryDiscovery(ctx, "radarr", "scope", 0, nil, []domain.Language{"en"}, at); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	database, err = OpenReadOnly(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	marker, revision, err := database.Repository().LibraryDiscoveryState(ctx, "radarr")
	if err != nil || marker != "scope" || revision != 0 {
		t.Fatalf("restarted state = %q/%d/%v", marker, revision, err)
	}
	if err := database.Repository().CommitLibraryDiscovery(ctx, "radarr", "new-scope", revision, nil, []domain.Language{"en"}, at); err == nil {
		t.Fatal("read-only discovery committed")
	}
}

func TestLibraryDiscoveryPreservesExistingRows(t *testing.T) {
	repo := openTestRepository(t)
	ctx, at := t.Context(), time.Now().UTC()
	if err := repo.EnsureInstance(ctx, "sonarr-main", "sonarr", "http://sonarr", at); err != nil {
		t.Fatal(err)
	}
	first, deleted := testMedia(), testMedia()
	deleted.EntityID++
	deleted.Ref.FileID++
	deleted.Fingerprint.FileID++
	for i, media := range []domain.Media{first, deleted} {
		if _, err := repo.ApplyMediaEvent(ctx, MediaEventMutation{EventID: fmt.Sprint("initial", i), Type: "import", Ref: media.Ref, EntityID: media.EntityID, Media: media, Languages: []domain.Language{"hr"}, At: at}); err != nil {
			t.Fatal(err)
		}
	}
	id, _, err := repo.FindMedia(ctx, first.Ref)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.store.db.Exec(`UPDATE search_states SET attempt=7, failure_attempt=3, lease_owner='worker', lease_until_ns=?, rerun_requested=1 WHERE media_id=?`, at.Add(time.Minute).UnixNano(), id); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.ApplyMediaEvent(ctx, MediaEventMutation{EventID: "delete", Type: "delete", Ref: deleted.Ref, EntityID: deleted.EntityID, At: at}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.store.db.Exec(`INSERT INTO installations(media_id,language,path,checksum,installed_at_ns,media_path,media_file_id,media_size,media_mod_time_ns) VALUES (?, 'hr','/media/show.hr.srt','checksum',1,'/media/show.mkv',42,100,123)`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.store.db.Exec(`INSERT INTO candidate_rejections(media_id,language,provider_id,result_id,candidate_signature,reason_code,tool_signature,media_path,media_file_id,media_size,media_mod_time_ns,rejected_at_ns,expires_at_ns) VALUES (?,'hr','test','1','signature','invalid_subtitle','tool','/media/show.mkv',42,100,123,1,0)`, id); err != nil {
		t.Fatal(err)
	}
	tables := []string{"media", "search_states", "installations", "candidate_rejections", "events"}
	before := make(map[string]string)
	for _, table := range tables {
		before[table] = libraryTableSnapshot(t, repo, table)
	}
	_, revision, err := repo.LibraryDiscoveryState(ctx, "sonarr-main")
	if err != nil {
		t.Fatal(err)
	}
	// Discovery leaves newer/different catalog metadata and replaced file identities to history.
	first.Title = "stale title"
	first.Ref.FileID = 999
	first.Fingerprint.FileID = 999
	first.Fingerprint.Path = "/media/stale.mkv"
	deleted.Title = "must remain deleted"
	if err := repo.CommitLibraryDiscovery(ctx, "sonarr-main", "scope", revision, []domain.Media{first, deleted}, []domain.Language{"hr", "en"}, at.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	for _, table := range tables {
		if after := libraryTableSnapshot(t, repo, table); before[table] != after {
			t.Fatalf("discovery changed existing %s\nbefore: %s\nafter: %s", table, before[table], after)
		}
	}
}

func libraryTableSnapshot(t *testing.T, repo *Repository, table string) string {
	t.Helper()
	rows, err := repo.store.db.Query(`SELECT * FROM ` + table + ` ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	var data [][]any
	for rows.Next() {
		values := make([]any, len(columns))
		pointers := make([]any, len(columns))
		for i := range values {
			pointers[i] = &values[i]
		}
		if err := rows.Scan(pointers...); err != nil {
			t.Fatal(err)
		}
		data = append(data, values)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func TestLibraryDiscoveryConflictingFileIdentityRollsBackBatch(t *testing.T) {
	repo := openTestRepository(t)
	ctx, at := t.Context(), time.Now().UTC()
	if err := repo.EnsureInstance(ctx, "sonarr-main", "sonarr", "http://sonarr", at); err != nil {
		t.Fatal(err)
	}
	existing := testMedia()
	if _, _, err := repo.UpsertMedia(ctx, existing); err != nil {
		t.Fatal(err)
	}
	missing, conflict := existing, existing
	missing.EntityID++
	missing.Ref.FileID++
	missing.Fingerprint.FileID++
	conflict.EntityID += 2
	before := libraryTableSnapshot(t, repo, "media")
	if err := repo.CommitLibraryDiscovery(ctx, "sonarr-main", "scope", 0, []domain.Media{missing, conflict}, []domain.Language{"hr"}, at); err == nil {
		t.Fatal("conflicting file identity committed")
	}
	if libraryTableSnapshot(t, repo, "media") != before {
		t.Fatal("partial media committed")
	}
	scope, revision, err := repo.LibraryDiscoveryState(ctx, "sonarr-main")
	if err != nil || scope != "" || revision != 0 {
		t.Fatalf("partial completion = %q/%d/%v", scope, revision, err)
	}
	var count int
	if err := repo.store.db.QueryRow(`SELECT count(*) FROM search_states`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("partial searches = %d/%v", count, err)
	}
}

func TestLibraryDiscoveryBoundsAuditWithoutResettingRevision(t *testing.T) {
	repo := openTestRepository(t)
	ctx, at := t.Context(), time.Now().UTC()
	if err := repo.EnsureInstance(ctx, "sonarr-main", "sonarr", "http://sonarr", at); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.store.db.Exec(`WITH RECURSIVE seq(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM seq WHERE n<10000) INSERT INTO events(event_type,instance,outcome,created_at_ns) SELECT 'delete','sonarr-main','applied',n FROM seq`); err != nil {
		t.Fatal(err)
	}
	_, revision, err := repo.LibraryDiscoveryState(ctx, "sonarr-main")
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.CommitLibraryDiscovery(ctx, "sonarr-main", "scope", revision, []domain.Media{testMedia()}, []domain.Language{"hr"}, at); err != nil {
		t.Fatal(err)
	}
	var count, oldest int64
	if err := repo.store.db.QueryRow(`SELECT count(*),min(created_at_ns) FROM events`).Scan(&count, &oldest); err != nil {
		t.Fatal(err)
	}
	if count != 10000 || oldest != 2 {
		t.Fatalf("audit retention count/oldest = %d/%d", count, oldest)
	}
	scope, after, err := repo.LibraryDiscoveryState(ctx, "sonarr-main")
	if err != nil || scope != "scope" || after != revision+1 {
		t.Fatalf("retention changed revision = %q/%d/%v", scope, after, err)
	}
}

func TestLibraryDiscoveryRejectsConflictingSnapshotEntities(t *testing.T) {
	for _, preexisting := range []bool{false, true} {
		t.Run(fmt.Sprint(preexisting), func(t *testing.T) {
			repo := openTestRepository(t)
			ctx, at := t.Context(), time.Now().UTC()
			if err := repo.EnsureInstance(ctx, "sonarr-main", "sonarr", "http://sonarr", at); err != nil {
				t.Fatal(err)
			}
			first, second := testMedia(), testMedia()
			second.Ref.FileID++
			second.Fingerprint.FileID++
			if preexisting {
				if _, _, err := repo.UpsertMedia(ctx, first); err != nil {
					t.Fatal(err)
				}
			}
			before := libraryTableSnapshot(t, repo, "media")
			if err := repo.CommitLibraryDiscovery(ctx, "sonarr-main", "scope", 0, []domain.Media{first, second}, []domain.Language{"hr"}, at); err == nil {
				t.Fatal("conflicting snapshot entities committed")
			}
			if after := libraryTableSnapshot(t, repo, "media"); after != before {
				t.Fatal("conflicting snapshot changed media")
			}
			scope, revision, err := repo.LibraryDiscoveryState(ctx, "sonarr-main")
			if err != nil || scope != "" || revision != 0 {
				t.Fatalf("conflicting snapshot state = %q/%d/%v", scope, revision, err)
			}
		})
	}
}

func TestLibraryDiscoveryRejectsSnapshotFileSharedByExistingEntities(t *testing.T) {
	repo := openTestRepository(t)
	ctx, at := t.Context(), time.Now().UTC()
	if err := repo.EnsureInstance(ctx, "sonarr-main", "sonarr", "http://sonarr", at); err != nil {
		t.Fatal(err)
	}
	first, second := testMedia(), testMedia()
	second.EntityID++
	second.Ref.FileID++
	second.Fingerprint.FileID++
	for _, item := range []domain.Media{first, second} {
		if _, _, err := repo.UpsertMedia(ctx, item); err != nil {
			t.Fatal(err)
		}
	}
	first.Ref.FileID, first.Fingerprint.FileID = 999, 999
	second.Ref.FileID, second.Fingerprint.FileID = 999, 999
	if err := repo.CommitLibraryDiscovery(ctx, "sonarr-main", "scope", 0, []domain.Media{first, second}, []domain.Language{"hr"}, at); err == nil {
		t.Fatal("shared snapshot file committed")
	}
}
