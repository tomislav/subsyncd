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
