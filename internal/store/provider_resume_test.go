package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestLeaseDueSearchesRejectsIterationErrorBeforeClaim(t *testing.T) {
	claims := &atomic.Int32{}
	driverName := fmt.Sprintf("provider-resume-iteration-%d", time.Now().UnixNano())
	sql.Register(driverName, &resumeIterationErrorDriver{claims: claims})
	db, err := sql.Open(driverName, "")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	repo := (&Store{db: db}).Repository()

	leases, err := repo.LeaseDueSearches(context.Background(), time.Unix(100, 0).UTC(), 2, 5*time.Minute)
	if err == nil {
		t.Errorf("iteration failure returned leases %#v", leases)
	}
	if got := claims.Load(); got != 0 {
		t.Errorf("iteration failure allowed %d claim updates", got)
	}
}

type resumeIterationErrorDriver struct {
	claims *atomic.Int32
}

func (d *resumeIterationErrorDriver) Open(string) (driver.Conn, error) {
	return &resumeIterationErrorConn{claims: d.claims}, nil
}

type resumeIterationErrorConn struct {
	claims *atomic.Int32
}

func (*resumeIterationErrorConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("prepare unsupported")
}

func (*resumeIterationErrorConn) Close() error { return nil }

func (*resumeIterationErrorConn) Begin() (driver.Tx, error) {
	return resumeIterationErrorTx{}, nil
}

func (*resumeIterationErrorConn) BeginTx(context.Context, driver.TxOptions) (driver.Tx, error) {
	return resumeIterationErrorTx{}, nil
}

func (*resumeIterationErrorConn) QueryContext(context.Context, string, []driver.NamedValue) (driver.Rows, error) {
	return &resumeIterationErrorRows{}, nil
}

func (c *resumeIterationErrorConn) ExecContext(context.Context, string, []driver.NamedValue) (driver.Result, error) {
	c.claims.Add(1)
	return driver.RowsAffected(1), nil
}

type resumeIterationErrorTx struct{}

func (resumeIterationErrorTx) Commit() error   { return nil }
func (resumeIterationErrorTx) Rollback() error { return nil }

type resumeIterationErrorRows struct {
	returned bool
}

func (*resumeIterationErrorRows) Columns() []string {
	return []string{"media_id", "language", "attempt", "failure_attempt", "priority", "resume_providers_json", "resume_route_signature"}
}

func (*resumeIterationErrorRows) Close() error { return nil }

func (r *resumeIterationErrorRows) Next(destination []driver.Value) error {
	if r.returned {
		return errors.New("injected due-search iteration failure")
	}
	r.returned = true
	copy(destination, []driver.Value{int64(1), "en", int64(0), int64(0), int64(SearchPriorityMissing), "[]", "route"})
	return nil
}

func TestSearchResumeRoundTrip(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 13, 10, 0, 0, 0, time.UTC)
	db, err := Open(ctx, filepath.Join(t.TempDir(), "subsyncd.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repo := db.Repository()
	mediaID, _, err := repo.UpsertMedia(ctx, testMedia())
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertSearchStateWithPriority(ctx, mediaID, "en", now, SearchPriorityMissing); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.Exec(`UPDATE search_states SET resume_providers_json='["titlovi-main","subdl-main"]',resume_route_signature='route-v1' WHERE media_id=? AND language='en'`, mediaID); err != nil {
		t.Fatal(err)
	}

	leases, err := repo.LeaseDueSearches(ctx, now, 1, 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(leases) != 1 {
		t.Fatalf("leases = %d, want 1", len(leases))
	}
	if want := []string{"titlovi-main", "subdl-main"}; !reflect.DeepEqual(leases[0].ResumeProviders, want) {
		t.Fatalf("resume providers = %v, want %v", leases[0].ResumeProviders, want)
	}
	if leases[0].ResumeRouteSignature != "route-v1" {
		t.Fatalf("resume route signature = %q", leases[0].ResumeRouteSignature)
	}

	next := now.Add(30 * time.Minute)
	if _, err := repo.CompleteSearch(ctx, SearchCompletion{
		JobID:                leases[0].JobID,
		Outcome:              "throttled",
		NextAttemptAt:        next,
		ResumeProviders:      []string{"opensubtitles-main", "titlovi-main"},
		ResumeRouteSignature: "route-v2",
		PreserveResume:       true,
	}); err != nil {
		t.Fatal(err)
	}
	leasingAgain, err := repo.LeaseDueSearches(ctx, next, 1, 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(leasingAgain) != 1 {
		t.Fatalf("leases after completion = %d, want 1", len(leasingAgain))
	}
	if want := []string{"opensubtitles-main", "titlovi-main"}; !reflect.DeepEqual(leasingAgain[0].ResumeProviders, want) {
		t.Fatalf("persisted resume providers = %v, want %v", leasingAgain[0].ResumeProviders, want)
	}
	if leasingAgain[0].ResumeRouteSignature != "route-v2" {
		t.Fatalf("persisted resume route signature = %q", leasingAgain[0].ResumeRouteSignature)
	}
}

func TestSearchResumeRejectsMalformedState(t *testing.T) {
	fixtures := map[string]string{
		"corrupt-json":   `not-json-provider-secret`,
		"duplicate-id":   `["duplicate-secret","duplicate-secret"]`,
		"empty-id":       `[""]`,
		"non-string":     `["provider",17]`,
		"oversized-list": `["one","two","three","four","five","six","seven","eight","ninth-secret"]`,
	}
	for name, persisted := range fixtures {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			now := time.Date(2026, 9, 13, 11, 0, 0, 0, time.UTC)
			db, err := Open(ctx, filepath.Join(t.TempDir(), "subsyncd.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			repo := db.Repository()
			for index := int64(1); index <= 2; index++ {
				media := testMedia()
				media.Ref.FileID += index
				media.EntityID += index
				media.Fingerprint.Path = fmt.Sprintf("/media/resume-%d.mkv", index)
				mediaID, _, err := repo.UpsertMedia(ctx, media)
				if err != nil {
					t.Fatal(err)
				}
				if err := repo.UpsertSearchStateWithPriority(ctx, mediaID, "en", now, SearchPriorityMissing); err != nil {
					t.Fatal(err)
				}
				if index == 2 {
					if _, err := db.db.Exec(`UPDATE search_states SET resume_providers_json=?,resume_route_signature='route' WHERE media_id=?`, persisted, mediaID); err != nil {
						t.Fatal(err)
					}
				}
			}

			leases, err := repo.LeaseDueSearches(ctx, now, 2, 5*time.Minute)
			if err == nil {
				t.Fatalf("malformed state leased %d rows", len(leases))
			}
			if len(err.Error()) > 128 {
				t.Fatalf("repository error is unbounded: %q", err)
			}
			if strings.Contains(err.Error(), persisted) || strings.Contains(err.Error(), "secret") {
				t.Fatalf("repository error exposes persisted state: %q", err)
			}
			var claimed int
			if err := db.db.QueryRow(`SELECT count(*) FROM search_states WHERE lease_owner IS NOT NULL OR lease_until_ns IS NOT NULL`).Scan(&claimed); err != nil {
				t.Fatal(err)
			}
			if claimed != 0 {
				t.Fatalf("claimed %d rows before rejecting malformed state", claimed)
			}
		})
	}
}

func TestProviderResumeMigrationPreservesStateAndReschedulesThrottle(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "subsyncd.db")
	old, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := old.Exec(`CREATE TABLE schema_migrations (version TEXT PRIMARY KEY, applied_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP)`); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{
		"001_baseline.sql",
		"002_scrub_provider_credentials.sql",
		"003_inventory_probes.sql",
		"004_sonarr_series.sql",
		"005_library_discovery.sql",
		"006_fallback_installations.sql",
		"007_clear_lapse_invalid_output.sql",
		"008_forced_track_probe_refresh.sql",
		"009_movie_duplicate_rejections.sql",
		"010_reconciliation_replays.sql",
		"011_reschedule_unfinished_backfill.sql",
	} {
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
	if _, err := old.Exec(`
		INSERT INTO media(id,instance,kind,entity_id,file_id,path,size,mod_time_ns,title,updated_at_ns,deleted)
		VALUES (1,'radarr','movie',1,11,'/media/one.mkv',101,11,'One',1011,0),
		       (2,'radarr','movie',2,22,'/media/two.mkv',102,12,'Two',1012,0),
		       (3,'radarr','movie',3,33,'/media/three.mkv',103,13,'Three',1013,0),
		       (4,'radarr','movie',4,44,'/media/four.mkv',104,14,'Four',1014,0),
		       (5,'radarr','movie',5,55,'/media/five.mkv',105,15,'Five',1015,0),
		       (6,'radarr','movie',6,66,'/media/six.mkv',106,16,'Six',1016,0),
		       (7,'radarr','movie',7,77,'/media/seven.mkv',107,17,'Seven',1017,0),
		       (8,'radarr','movie',8,88,'/media/eight.mkv',108,18,'Eight',1018,1);
		INSERT INTO search_states(media_id,language,state,attempt,failure_attempt,next_attempt_at_ns,last_outcome,priority,rerun_requested,lease_owner,lease_until_ns)
		VALUES (1,'en','pending',11,21,901,'throttled',200,0,NULL,NULL),
		       (2,'hr','pending',12,22,902,'throttled',200,1,'worker-two',1902),
		       (3,'en','pending',13,23,903,'throttled',200,0,NULL,NULL),
		       (4,'en','pending',14,24,904,'throttled',100,0,NULL,NULL),
		       (5,'en','pending',15,25,905,'no_result',200,0,NULL,NULL),
		       (6,'en','pending',16,26,906,'rejected',200,0,NULL,NULL),
		       (7,'en','complete',17,27,907,'throttled',200,0,NULL,NULL),
		       (8,'en','pending',18,28,908,'throttled',200,0,NULL,NULL);
		INSERT INTO installations(media_id,language,path,checksum,provider_id,candidate_id,score_json,sync_result_json,rollback_path,installed_at_ns,media_path,media_file_id,media_size,media_mod_time_ns,fallback)
		VALUES (3,'en','/media/three.en.srt','install-checksum','provider-main','candidate-3','{"total":88}','{"verdict":"solid"}','/rollback/three',303,'/media/three.mkv',33,103,13,1);
		INSERT INTO candidate_rejections(media_id,language,provider_id,result_id,candidate_signature,artifact_checksum,reason_code,tool_signature,media_path,media_file_id,media_size,media_mod_time_ns,rejected_at_ns,expires_at_ns)
		VALUES (1,'en','provider-main','result-1','signature-1','artifact-1','lapse_unsure','lapse-v2','/media/one.mkv',11,101,11,401,402);
		INSERT INTO provider_states(provider_id,scope,reason,quota_limit,quota_remaining,reset_at_ns,disabled,failure_attempt,quota_json)
		VALUES ('provider-main','search','rate_limit',500,3,501,0,4,'{"window":"day"}');
		INSERT INTO provider_cache(cache_key,provider_id,results_json,expires_at_ns)
		VALUES ('cache-key','provider-main','[{"result_id":"safe"}]',601);
	`); err != nil {
		t.Fatal(err)
	}
	before, err := providerResumeMigrationSnapshot(old)
	if err != nil {
		t.Fatal(err)
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}

	migrated, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	after, err := providerResumeMigrationSnapshot(migrated.db)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(after.invariants, before.invariants) {
		t.Fatalf("search metadata changed:\nbefore: %#v\nafter:  %#v", before.invariants, after.invariants)
	}
	if !reflect.DeepEqual(after.installations, before.installations) {
		t.Fatalf("installations changed:\nbefore: %#v\nafter:  %#v", before.installations, after.installations)
	}
	if !reflect.DeepEqual(after.rejections, before.rejections) {
		t.Fatalf("candidate rejections changed:\nbefore: %#v\nafter:  %#v", before.rejections, after.rejections)
	}
	if !reflect.DeepEqual(after.providerStates, before.providerStates) {
		t.Fatalf("provider states changed:\nbefore: %#v\nafter:  %#v", before.providerStates, after.providerStates)
	}
	if !reflect.DeepEqual(after.providerCache, before.providerCache) {
		t.Fatalf("provider cache changed:\nbefore: %#v\nafter:  %#v", before.providerCache, after.providerCache)
	}
	if want := []int64{0, 0, 903, 904, 905, 906, 907, 908}; !reflect.DeepEqual(after.nextAttempts, want) {
		t.Fatalf("next attempts = %v, want %v", after.nextAttempts, want)
	}
	var migrationCount int
	if err := migrated.db.QueryRow(`SELECT count(*) FROM schema_migrations WHERE version='012_provider_resume.sql'`).Scan(&migrationCount); err != nil {
		t.Fatal(err)
	}
	if migrationCount != 1 {
		t.Fatalf("migration ledger count = %d, want 1", migrationCount)
	}
	rows, err := migrated.db.Query(`SELECT resume_providers_json,resume_route_signature FROM search_states ORDER BY media_id`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var providers, signature string
		if err := rows.Scan(&providers, &signature); err != nil {
			t.Fatal(err)
		}
		if providers != "[]" || signature != "" {
			t.Fatalf("resume defaults = %q/%q", providers, signature)
		}
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := migrated.db.Exec(`UPDATE search_states SET next_attempt_at_ns=777 WHERE media_id=1`); err != nil {
		t.Fatal(err)
	}
	if err := migrated.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	var next int64
	if err := reopened.db.QueryRow(`SELECT next_attempt_at_ns FROM search_states WHERE media_id=1`).Scan(&next); err != nil {
		t.Fatal(err)
	}
	if next != 777 {
		t.Fatalf("one-shot migration reapplied: next attempt = %d", next)
	}
	if err := reopened.db.QueryRow(`SELECT count(*) FROM schema_migrations WHERE version='012_provider_resume.sql'`).Scan(&migrationCount); err != nil {
		t.Fatal(err)
	}
	if migrationCount != 1 {
		t.Fatalf("migration ledger count after reopen = %d, want 1", migrationCount)
	}
}

type providerResumeMigrationState struct {
	nextAttempts   []int64
	invariants     []string
	installations  []string
	rejections     []string
	providerStates []string
	providerCache  []string
}

func providerResumeMigrationSnapshot(db *sql.DB) (providerResumeMigrationState, error) {
	var state providerResumeMigrationState
	searchRows, err := db.Query(`SELECT media_id,language,state,attempt,failure_attempt,next_attempt_at_ns,last_outcome,priority,rerun_requested,COALESCE(lease_owner,''),COALESCE(lease_until_ns,0) FROM search_states ORDER BY media_id,language`)
	if err != nil {
		return state, err
	}
	for searchRows.Next() {
		var mediaID, next, leaseUntil int64
		var language, searchState, outcome, leaseOwner string
		var attempt, failureAttempt, priority, rerun int
		if err := searchRows.Scan(&mediaID, &language, &searchState, &attempt, &failureAttempt, &next, &outcome, &priority, &rerun, &leaseOwner, &leaseUntil); err != nil {
			_ = searchRows.Close()
			return state, err
		}
		state.nextAttempts = append(state.nextAttempts, next)
		state.invariants = append(state.invariants, fmt.Sprintf("%d|%s|%s|%d|%d|%s|%d|%d|%s|%d", mediaID, language, searchState, attempt, failureAttempt, outcome, priority, rerun, leaseOwner, leaseUntil))
	}
	if err := searchRows.Close(); err != nil {
		return state, err
	}
	queries := []struct {
		destination *[]string
		query       string
	}{
		{&state.installations, `SELECT printf('%d|%d|%s|%s|%s|%s|%s|%s|%s|%s|%d|%s|%d|%d|%d|%d',id,media_id,language,path,checksum,provider_id,candidate_id,CAST(score_json AS TEXT),CAST(sync_result_json AS TEXT),rollback_path,installed_at_ns,media_path,media_file_id,media_size,media_mod_time_ns,fallback) FROM installations ORDER BY id`},
		{&state.rejections, `SELECT printf('%d|%d|%s|%s|%s|%s|%s|%s|%s|%s|%d|%d|%d|%d|%d',id,media_id,language,provider_id,result_id,candidate_signature,artifact_checksum,reason_code,tool_signature,media_path,media_file_id,media_size,media_mod_time_ns,rejected_at_ns,expires_at_ns) FROM candidate_rejections ORDER BY id`},
		{&state.providerStates, `SELECT printf('%s|%s|%s|%d|%d|%d|%d|%d|%s',provider_id,scope,reason,quota_limit,quota_remaining,reset_at_ns,disabled,failure_attempt,CAST(quota_json AS TEXT)) FROM provider_states ORDER BY provider_id,scope`},
		{&state.providerCache, `SELECT printf('%s|%s|%s|%d',cache_key,provider_id,CAST(results_json AS TEXT),expires_at_ns) FROM provider_cache ORDER BY cache_key`},
	}
	for _, item := range queries {
		rows, err := db.Query(item.query)
		if err != nil {
			return state, err
		}
		for rows.Next() {
			var value string
			if err := rows.Scan(&value); err != nil {
				_ = rows.Close()
				return state, err
			}
			*item.destination = append(*item.destination, value)
		}
		if err := rows.Close(); err != nil {
			return state, err
		}
	}
	return state, nil
}
