package store

import (
	"testing"
	"time"
)

func TestClaimSearchRefusesLiveLease(t *testing.T) {
	repo := openTestRepository(t)
	ctx := t.Context()
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	mediaID, _, err := repo.UpsertMedia(ctx, testMedia())
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertSearchStateWithPriority(ctx, mediaID, "en", now, SearchPriorityMissing); err != nil {
		t.Fatal(err)
	}
	worker := leaseOne(t, repo, now)
	claimed, err := repo.ClaimSearch(ctx, mediaID, "en", "manual-1", now.Add(time.Second), time.Minute)
	if err != nil || claimed {
		t.Fatalf("claim beside a live lease = %v/%v", claimed, err)
	}
	// The worker still owns its search: it can renew and complete it.
	if err := repo.RenewSearchLease(ctx, worker.JobID, now.Add(2*time.Second), time.Minute); err != nil {
		t.Fatalf("worker lost its lease: %v", err)
	}
	if err := repo.ReleaseSearchLease(ctx, worker.JobID); err != nil {
		t.Fatal(err)
	}
	claimed, err = repo.ClaimSearch(ctx, mediaID, "en", "manual-1", now.Add(3*time.Second), time.Minute)
	if err != nil || !claimed {
		t.Fatalf("claim after release = %v/%v", claimed, err)
	}
	// A claimed search is not leased to the worker, and the claim renews.
	if leases, err := repo.LeaseDueSearches(ctx, now.Add(4*time.Second), 1, time.Minute); err != nil || len(leases) != 0 {
		t.Fatalf("worker leased a claimed search: %+v/%v", leases, err)
	}
	if err := repo.RenewSearchLease(ctx, "manual-1", now.Add(5*time.Second), time.Minute); err != nil {
		t.Fatal(err)
	}
}

func TestClaimSearchTakesExpiredLease(t *testing.T) {
	repo := openTestRepository(t)
	ctx := t.Context()
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	mediaID, _, err := repo.UpsertMedia(ctx, testMedia())
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertSearchStateWithPriority(ctx, mediaID, "en", now, SearchPriorityMissing); err != nil {
		t.Fatal(err)
	}
	dead := leaseOne(t, repo, now)
	claimed, err := repo.ClaimSearch(ctx, mediaID, "en", "manual-1", dead.LeaseUntil, time.Minute)
	if err != nil || !claimed {
		t.Fatalf("claim of an expired lease = %v/%v", claimed, err)
	}
	if err := repo.RenewSearchLease(ctx, dead.JobID, dead.LeaseUntil, time.Minute); err == nil {
		t.Fatal("the dead owner still renews")
	}
	var owner string
	var state string
	if err := repo.store.db.QueryRow(`SELECT lease_owner, state FROM search_states WHERE media_id=?`, mediaID).Scan(&owner, &state); err != nil {
		t.Fatal(err)
	}
	if owner != "manual-1" || state != "pending" {
		t.Fatalf("owner/state = %s/%s", owner, state)
	}
}

func TestClaimSearchCreatesMissingRow(t *testing.T) {
	repo := openTestRepository(t)
	ctx := t.Context()
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	mediaID, _, err := repo.UpsertMedia(ctx, testMedia())
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := repo.ClaimSearch(ctx, mediaID, "hr", "manual-1", now, time.Minute)
	if err != nil || !claimed {
		t.Fatalf("claim of a missing row = %v/%v", claimed, err)
	}
	status, err := repo.GetSearchStatus(ctx, mediaID, "hr")
	if err != nil || status.State != "pending" || status.Priority != SearchPriorityMissing {
		t.Fatalf("created status = %+v/%v", status, err)
	}
	var owner string
	var until int64
	if err := repo.store.db.QueryRow(`SELECT lease_owner, lease_until_ns FROM search_states WHERE media_id=? AND language='hr'`, mediaID).Scan(&owner, &until); err != nil {
		t.Fatal(err)
	}
	if owner != "manual-1" || until != now.Add(time.Minute).UnixNano() {
		t.Fatalf("lease = %s/%d", owner, until)
	}
	// Released, the created row is an ordinary due missing search.
	if err := repo.ReleaseSearchLease(ctx, "manual-1"); err != nil {
		t.Fatal(err)
	}
	if lease := leaseOne(t, repo, now); lease.Language != "hr" {
		t.Fatalf("lease = %+v", lease)
	}
}

func TestSettlePendingSearchAcceptsItsOwnClaim(t *testing.T) {
	repo := openTestRepository(t)
	ctx := t.Context()
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	mediaID, _, err := repo.UpsertMedia(ctx, testMedia())
	if err != nil {
		t.Fatal(err)
	}
	if claimed, err := repo.ClaimSearch(ctx, mediaID, "en", "manual-1", now, time.Minute); err != nil || !claimed {
		t.Fatalf("claim = %v/%v", claimed, err)
	}
	// Another owner's settlement leaves the claimed search alone.
	if err := repo.SettlePendingSearch(ctx, mediaID, "en", "installed", now, "other"); err != nil {
		t.Fatal(err)
	}
	if status, _ := repo.GetSearchStatus(ctx, mediaID, "en"); status.State != "pending" {
		t.Fatalf("settled under a foreign claim: %+v", status)
	}
	// A change that arrived during the claim keeps the search queued.
	if _, err := repo.store.db.Exec(`UPDATE search_states SET rerun_requested=1 WHERE media_id=?`, mediaID); err != nil {
		t.Fatal(err)
	}
	if err := repo.SettlePendingSearch(ctx, mediaID, "en", "installed", now, "manual-1"); err != nil {
		t.Fatal(err)
	}
	if status, _ := repo.GetSearchStatus(ctx, mediaID, "en"); status.State != "pending" {
		t.Fatalf("settled over a requested rerun: %+v", status)
	}
	if _, err := repo.store.db.Exec(`UPDATE search_states SET rerun_requested=0 WHERE media_id=?`, mediaID); err != nil {
		t.Fatal(err)
	}
	if err := repo.SettlePendingSearch(ctx, mediaID, "en", "installed", now, "manual-1"); err != nil {
		t.Fatal(err)
	}
	if status, _ := repo.GetSearchStatus(ctx, mediaID, "en"); status.State != "complete" || status.LastOutcome != "installed" {
		t.Fatalf("own claim not settled: %+v", status)
	}
}
