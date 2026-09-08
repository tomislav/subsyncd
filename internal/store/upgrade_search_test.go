package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestEnsureUpgradeSearchPreservesDurableQueue(t *testing.T) {
	for _, kind := range []string{"absent", "complete", "pending-earlier", "pending-later", "leased", "abandoned-lease", "deleted", "unsupported", "zero"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "state.db")
			db, err := Open(ctx, path)
			if err != nil {
				t.Fatal(err)
			}
			repo := db.Repository()
			id, _, err := repo.UpsertMedia(ctx, testMedia())
			if err != nil {
				t.Fatal(err)
			}
			next := time.Date(2026, 9, 20, 0, 0, 0, 123, time.UTC)
			if kind != "absent" && kind != "deleted" && kind != "unsupported" && kind != "zero" {
				due := next.Add(-time.Hour)
				if kind == "pending-later" {
					due = next.Add(time.Hour)
				}
				if err := repo.UpsertSearchStateWithPriority(ctx, id, "en", due, SearchPriorityImport); err != nil {
					t.Fatal(err)
				}
				if _, err := db.db.Exec(`UPDATE search_states SET attempt=3,failure_attempt=2,last_outcome='old'`); err != nil {
					t.Fatal(err)
				}
				if kind == "complete" {
					if _, err := db.db.Exec(`UPDATE search_states SET state='complete'`); err != nil {
						t.Fatal(err)
					}
				}
				if kind == "leased" || kind == "abandoned-lease" {
					until := next.Add(time.Hour)
					if kind == "abandoned-lease" {
						until = next.Add(-time.Hour)
					}
					if _, err := db.db.Exec(`UPDATE search_states SET lease_owner='owner',lease_until_ns=?,rerun_requested=1`, until.UnixNano()); err != nil {
						t.Fatal(err)
					}
				}
			}
			if kind == "deleted" {
				if _, err := db.db.Exec(`UPDATE media SET deleted=1`); err != nil {
					t.Fatal(err)
				}
			}
			if kind == "unsupported" {
				if _, err := db.db.Exec(`UPDATE media SET unsupported_reason='unsupported_multi_episode'`); err != nil {
					t.Fatal(err)
				}
			}
			before, _ := repo.GetSearchStatus(ctx, id, "en")
			var beforeOwner sql.NullString
			var beforeUntil sql.NullInt64
			if kind == "leased" || kind == "abandoned-lease" {
				if err := db.db.QueryRow(`SELECT lease_owner, lease_until_ns FROM search_states WHERE media_id=?`, id).Scan(&beforeOwner, &beforeUntil); err != nil {
					t.Fatal(err)
				}
			}

			requested := next
			if kind == "zero" {
				requested = time.Time{}
			}
			err = repo.EnsureUpgradeSearch(ctx, id, "en", requested)
			if kind == "zero" {
				if err == nil {
					t.Fatal("zero schedule accepted")
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			db, err = Open(ctx, path)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			repo = db.Repository()
			got, err := repo.GetSearchStatus(ctx, id, "en")
			if kind == "deleted" || kind == "unsupported" || kind == "zero" {
				if !errors.Is(err, sql.ErrNoRows) {
					t.Fatalf("unexpected schedule %#v/%v", got, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if kind == "leased" || kind == "abandoned-lease" {
				var owner sql.NullString
				var until sql.NullInt64
				if err := db.db.QueryRow(`SELECT lease_owner, lease_until_ns FROM search_states WHERE media_id=?`, id).Scan(&owner, &until); err != nil {
					t.Fatal(err)
				}
				if owner != beforeOwner || until != beforeUntil {
					t.Fatalf("lease changed: %v/%v", owner, until)
				}
			}
			if kind == "leased" || kind == "abandoned-lease" || kind == "pending-earlier" {
				if !reflect.DeepEqual(got, before) {
					t.Fatalf("queue changed: before %#v after %#v", before, got)
				}
				return
			}
			if got.State != "pending" || !got.NextAttemptAt.Equal(next) {
				t.Fatalf("schedule = %#v", got)
			}
			if kind == "pending-later" {
				if got.Priority != before.Priority || got.Attempt != before.Attempt || got.FailureAttempt != before.FailureAttempt {
					t.Fatalf("pending metadata changed: %#v", got)
				}
			} else if got.Priority != SearchPriorityUpgrade || got.Attempt != 0 || got.FailureAttempt != 0 {
				t.Fatalf("upgrade metadata = %#v", got)
			}
		})
	}
}
