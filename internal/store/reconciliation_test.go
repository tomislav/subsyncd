package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestCommitReconciliationRejectsPageAfterConcurrentEmptyPage(t *testing.T) {
	repo := openTestRepository(t)
	ctx := context.Background()
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	if err := repo.EnsureInstance(ctx, "sonarr-main", "sonarr", "http://arr.invalid", now); err != nil {
		t.Fatal(err)
	}
	snapshot := reconciliationState(t, repo, "sonarr-main")
	// Both requests started at the empty cursor. The newer empty page wins.
	if err := repo.CommitReconciliation(ctx, "sonarr-main", snapshot, now.Add(time.Hour), nil); err != nil {
		t.Fatal(err)
	}
	if err := repo.CommitReconciliation(ctx, "sonarr-main", snapshot, now, nil); !errors.Is(err, ErrReconciliationStale) {
		t.Errorf("stale empty reconciliation page error = %v", err)
	}
	cursor, err := repo.GetReconciliationCursor(ctx, "sonarr-main")
	if err != nil || !cursor.Equal(now.Add(time.Hour)) {
		t.Fatalf("stale empty page moved cursor backwards: %s, %v", cursor, err)
	}
}

func reconciliationState(t *testing.T, repo *Repository, instance string) ReconciliationState {
	t.Helper()
	state, err := repo.GetReconciliationState(context.Background(), instance)
	if err != nil {
		t.Fatal(err)
	}
	return state
}

func TestRetainReconciliationRecordsFirstDeferralUntilCursorAdvances(t *testing.T) {
	repo := openTestRepository(t)
	ctx := context.Background()
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	if err := repo.EnsureInstance(ctx, "sonarr-main", "sonarr", "http://arr.invalid", now); err != nil {
		t.Fatal(err)
	}
	if err := repo.CommitReconciliation(ctx, "sonarr-main", reconciliationState(t, repo, "sonarr-main"), now, nil); err != nil {
		t.Fatal(err)
	}
	if state := reconciliationState(t, repo, "sonarr-main"); !state.DeferredSince.IsZero() {
		t.Fatalf("fresh deferral start = %s, want zero", state.DeferredSince)
	}

	first := now.Add(time.Hour)
	if err := repo.RetainReconciliation(ctx, "sonarr-main", reconciliationState(t, repo, "sonarr-main"), first, nil); err != nil {
		t.Fatal(err)
	}
	if err := repo.RetainReconciliation(ctx, "sonarr-main", reconciliationState(t, repo, "sonarr-main"), first.Add(time.Hour), nil); err != nil {
		t.Fatal(err)
	}
	state := reconciliationState(t, repo, "sonarr-main")
	if !state.Cursor.Equal(now) || !state.DeferredSince.Equal(first) {
		t.Fatalf("retained state = %#v, want cursor %s deferred since %s", state, now, first)
	}

	if err := repo.CommitReconciliation(ctx, "sonarr-main", state, first.Add(2*time.Hour), nil); err != nil {
		t.Fatal(err)
	}
	if state := reconciliationState(t, repo, "sonarr-main"); !state.DeferredSince.IsZero() {
		t.Fatalf("deferral start after cursor advance = %s, want zero", state.DeferredSince)
	}
}

func TestRetainReconciliationRejectsStaleState(t *testing.T) {
	repo := openTestRepository(t)
	ctx := context.Background()
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	if err := repo.EnsureInstance(ctx, "sonarr-main", "sonarr", "http://arr.invalid", now); err != nil {
		t.Fatal(err)
	}
	stale := reconciliationState(t, repo, "sonarr-main")
	if err := repo.CommitReconciliation(ctx, "sonarr-main", stale, now, nil); err != nil {
		t.Fatal(err)
	}
	if err := repo.RetainReconciliation(ctx, "sonarr-main", stale, now.Add(time.Hour), nil); !errors.Is(err, ErrReconciliationStale) {
		t.Fatalf("stale retain error = %v, want ErrReconciliationStale", err)
	}
	if state := reconciliationState(t, repo, "sonarr-main"); !state.DeferredSince.IsZero() {
		t.Fatalf("stale retain recorded deferral start %s", state.DeferredSince)
	}
}
