package store

import (
	"context"
	"subsyncd/internal/domain"
	"testing"
	"time"
)

func TestSearchScopeFiltersBeforePriorityLimitAndPreservesHistory(t *testing.T) {
	repo := openTestRepository(t)
	ctx := context.Background()
	now := time.Now()
	valid := insertTestMedia(t, repo, 1, now)
	removed := insertTestMedia(t, repo, 2, now)
	deleted := insertTestMedia(t, repo, 3, now)
	if _, err := repo.store.db.Exec(`UPDATE media SET instance='removed' WHERE id=?`, removed); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.store.db.Exec(`UPDATE media SET deleted=1 WHERE id=?`, deleted); err != nil {
		t.Fatal(err)
	}
	requireSearchState(t, repo, valid, "hr", now, SearchPriorityImport)
	requireSearchState(t, repo, valid, "en", now, SearchPriorityUpgrade)
	requireSearchState(t, repo, removed, "en", now, SearchPriorityImport)
	requireSearchState(t, repo, deleted, "en", now, SearchPriorityImport)
	instances := []string{"sonarr-main"}
	languages := []domain.Language{"en"}
	scoped := repo.WithSearchScope(instances, languages)
	instances[0] = "removed"
	languages[0] = "hr"
	leases, err := scoped.LeaseDueSearches(ctx, now, 1, time.Minute)
	if err != nil || len(leases) != 1 || leases[0].MediaID != valid || leases[0].Language != "en" {
		t.Fatalf("eligible limited leases=%+v error=%v", leases, err)
	}
	empty := repo.WithSearchScope(nil, nil)
	if err := empty.RenewSearchLease(ctx, leases[0].JobID, now, time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := empty.CompleteSearch(ctx, SearchCompletion{JobID: leases[0].JobID, Outcome: "no_result", NextAttemptAt: now.Add(time.Hour), AdvanceMissingAttempt: true}); err != nil {
		t.Fatal(err)
	}
	for _, scopes := range []*Repository{empty, repo.WithSearchScope([]string{"sonarr-main"}, nil), repo.WithSearchScope(nil, []domain.Language{"en"})} {
		if got, err := scopes.LeaseDueSearches(ctx, now, 10, time.Minute); err != nil || len(got) != 0 {
			t.Fatalf("empty scope leases=%+v error=%v", got, err)
		}
	}
	restored := repo.WithSearchScope([]string{"sonarr-main", "removed"}, []domain.Language{"en", "hr"})
	got, err := restored.LeaseDueSearches(ctx, now, 10, time.Minute)
	if err != nil || len(got) != 2 {
		t.Fatalf("restored route leases=%+v error=%v", got, err)
	}
	for _, l := range got {
		if l.MediaID == deleted {
			t.Fatal("tombstoned media leased")
		}
	}
	// Unscoped callers retain access without changing the scoped clone or stored retry history.
	got, err = repo.LeaseDueSearches(ctx, now.Add(2*time.Hour), 10, time.Minute)
	if err != nil || len(got) != 3 {
		t.Fatalf("unscoped leases=%+v error=%v", got, err)
	}
	found := false
	for _, l := range got {
		if l.MediaID == valid && l.Language == "en" {
			found = true
			if l.Attempt != 1 {
				t.Fatalf("retry history lost: %+v", l)
			}
		}
	}
	if !found {
		t.Fatal("re-enabled retry missing")
	}
}

func TestSearchClaimsExcludeDeletedMedia(t *testing.T) {
	repo := openTestRepository(t)
	now := time.Now()
	id := insertTestMedia(t, repo, 1, now)
	requireSearchState(t, repo, id, "en", now, SearchPriorityImport)
	if _, err := repo.store.db.Exec(`UPDATE media SET deleted=1 WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	got, err := repo.LeaseDueSearches(context.Background(), now, 1, time.Minute)
	if err != nil || len(got) != 0 {
		t.Fatalf("deleted media claims=%+v error=%v", got, err)
	}
}

func TestEmptySearchScopeStillDeliversCommittedNotification(t *testing.T) {
	repo := openTestRepository(t)
	ctx := context.Background()
	now := time.Now()
	if _, err := repo.EnqueueNotification(ctx, NotificationRequest{Notifier: "silo", DedupeKey: "committed", PayloadJSON: []byte(`{}`), NextAttemptAt: now}); err != nil {
		t.Fatal(err)
	}
	scoped := repo.WithSearchScope(nil, nil)
	leases, err := scoped.LeaseDueNotifications(ctx, now, 1, time.Minute)
	if err != nil || len(leases) != 1 {
		t.Fatalf("committed notification=%+v error=%v", leases, err)
	}
	if err := scoped.CompleteNotification(ctx, NotificationCompletion{JobID: leases[0].JobID, Result: "success"}); err != nil {
		t.Fatal(err)
	}
}
