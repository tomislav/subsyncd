package provider

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"subsyncd/internal/store"
	"subsyncd/internal/testutil"
)

func TestGateRestoresOperationScopedCooldownAcrossRestart(t *testing.T) {
	ctx := context.Background()
	database, err := store.Open(ctx, filepath.Join(t.TempDir(), "subsyncd.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	clock := testutil.NewClock(now)
	if err := database.Repository().PutProviderState(ctx, store.ProviderState{ProviderID: "account-a", Scope: "download", Reason: "quota", ResetAt: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}

	for run := 0; run < 2; run++ {
		gate := NewGate(database.Repository(), clock, 1)
		gate.Configure("account-a", 1000, 1, 1)
		gate.Configure("account-b", 1000, 1, 1)
		if _, err := gate.Acquire(ctx, "account-a", "api.example", OperationDownload); !errors.As(err, new(*CooldownError)) {
			t.Fatalf("run %d download error = %v, want CooldownError", run, err)
		}
		release, err := gate.Acquire(ctx, "account-a", "api.example", OperationSearch)
		if err != nil {
			t.Fatalf("search scope blocked: %v", err)
		}
		release()
		release, err = gate.Acquire(ctx, "account-b", "api.example", OperationDownload)
		if err != nil {
			t.Fatalf("second account blocked: %v", err)
		}
		release()
	}
}

func TestGateSerializesSharedOriginAcrossProviders(t *testing.T) {
	clock := testutil.NewClock(time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC))
	gate := NewGate(emptyStateStore{}, clock, 1)
	gate.Configure("one", 1000, 1, 1)
	gate.Configure("two", 1000, 1, 1)
	firstRelease, err := gate.Acquire(context.Background(), "one", "api.example", OperationSearch)
	if err != nil {
		t.Fatal(err)
	}
	acquired := make(chan func(), 1)
	go func() {
		release, acquireErr := gate.Acquire(context.Background(), "two", "api.example", OperationSearch)
		if acquireErr == nil {
			acquired <- release
		}
	}()
	select {
	case <-acquired:
		t.Fatal("second provider acquired shared origin before release")
	case <-time.After(20 * time.Millisecond):
	}
	firstRelease()
	select {
	case release := <-acquired:
		release()
	case <-time.After(time.Second):
		t.Fatal("second provider did not acquire after release")
	}
}

type emptyStateStore struct{}

func (emptyStateStore) GetProviderState(context.Context, string, string) (store.ProviderState, error) {
	return store.ProviderState{}, sql.ErrNoRows
}

func (emptyStateStore) PutProviderState(context.Context, store.ProviderState) error { return nil }
