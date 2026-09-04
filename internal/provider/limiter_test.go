package provider

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"subsyncd/internal/observability"
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

func TestGateAppliesTimedAuthCooldownOnlyToAuthentication(t *testing.T) {
	ctx := context.Background()
	database, err := store.Open(ctx, filepath.Join(t.TempDir(), "subsyncd.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	clock := testutil.NewClock(now)
	providerID := "opensubtitles-main"
	if err := database.Repository().PutProviderState(ctx, store.ProviderState{ProviderID: providerID, Scope: "auth", Reason: "retry-after", ResetAt: now.Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	gate := NewGate(database.Repository(), clock, 1)
	gate.Configure(providerID, 1000, 1, 1)

	if _, err := gate.Acquire(ctx, providerID, "api.opensubtitles.com", OperationAuth); !errors.As(err, new(*CooldownError)) {
		t.Fatalf("authentication error = %v, want CooldownError", err)
	}
	release, err := gate.Acquire(ctx, providerID, "api.opensubtitles.com", OperationSearch)
	if err != nil {
		t.Fatalf("valid-token search blocked by auth cooldown: %v", err)
	}
	release()
	release, err = gate.Acquire(ctx, providerID, "api.opensubtitles.com", OperationDownload)
	if err != nil {
		t.Fatalf("valid-token download blocked by auth cooldown: %v", err)
	}
	release()

	if err := database.Repository().PutProviderState(ctx, store.ProviderState{ProviderID: providerID, Scope: "auth", Reason: "credentials rejected", Disabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := gate.Acquire(ctx, providerID, "api.opensubtitles.com", OperationSearch); !errors.As(err, new(*DisabledError)) {
		t.Fatalf("search with disabled authentication error = %v, want DisabledError", err)
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

func TestGateLogsOnlyPersistedProviderStateTransitions(t *testing.T) {
	ctx := context.Background()
	database, err := store.Open(ctx, filepath.Join(t.TempDir(), "subsyncd.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	clock := testutil.NewClock(now)
	var logs bytes.Buffer
	events, err := observability.New(&logs, observability.Options{Level: "info", Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	gate := NewGate(database.Repository(), clock, 1, events)
	gate.Configure("provider-a", 1000, 1, 1, "subdl")

	if err := gate.Persist(ctx, Throttle{ProviderID: "provider-a", Scope: OperationDownload, Reason: "ratelimit", Limit: 100, Remaining: 50, ResetAt: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	cooldown := Throttle{ProviderID: "provider-a", Scope: OperationDownload, Reason: "quota", Remaining: 0, ResetAt: now.Add(time.Hour)}
	if err := gate.Persist(ctx, cooldown); err != nil {
		t.Fatal(err)
	}
	if _, err := gate.Acquire(ctx, "provider-a", "https://api.example", OperationDownload); !errors.As(err, new(*CooldownError)) {
		t.Fatalf("suppression error = %v", err)
	}
	if err := gate.Persist(ctx, cooldown); err != nil {
		t.Fatal(err)
	}
	if err := gate.Persist(ctx, Throttle{ProviderID: "provider-a", Scope: OperationAuth, Reason: "bad token secret-value", Disabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := gate.Persist(ctx, Throttle{ProviderID: "provider-a", Scope: OperationAuth, Reason: "bad token secret-value", Disabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := gate.RecordTransientFailure(ctx, "provider-a", OperationSearch, "network_error", time.Time{}); err != nil {
		t.Fatal(err)
	}
	clock.Advance(time.Minute)
	if _, err := gate.RecordTransientFailure(ctx, "provider-a", OperationSearch, "network_error", time.Time{}); err != nil {
		t.Fatal(err)
	}
	if err := gate.ResetTransientFailures(ctx, "provider-a", OperationSearch); err != nil {
		t.Fatal(err)
	}

	records := providerLogRecords(t, logs.String())
	if got := len(providerEvents(records, "provider.cooldown_started")); got != 1 {
		t.Fatalf("cooldown events = %d, want 1: %s", got, logs.String())
	}
	cooldownRecord := providerEvents(records, "provider.cooldown_started")[0]
	if cooldownRecord["provider_type"] != "subdl" || cooldownRecord["reason"] != "provider_state" {
		t.Fatalf("cooldown classification = %#v", cooldownRecord)
	}
	if got := len(providerEvents(records, "provider.auth_disabled")); got != 1 {
		t.Fatalf("auth events = %d, want 1: %s", got, logs.String())
	}
	circuits := providerEvents(records, "provider.circuit_opened")
	if len(circuits) != 2 || circuits[0]["attempt"] != float64(1) || circuits[1]["attempt"] != float64(2) {
		t.Fatalf("circuit events = %#v", circuits)
	}
	if circuits[0]["reason"] != "network_error" || circuits[0]["provider_type"] != "subdl" {
		t.Fatalf("circuit classification = %#v", circuits[0])
	}
	if got := len(providerEvents(records, "provider.recovered")); got != 1 {
		t.Fatalf("recovery events = %d, want 1: %s", got, logs.String())
	}
	if strings.Contains(logs.String(), "secret-value") {
		t.Fatalf("transition logs leaked provider reason: %s", logs.String())
	}
}

type emptyStateStore struct{}

func (emptyStateStore) GetProviderState(context.Context, string, string) (store.ProviderState, error) {
	return store.ProviderState{}, sql.ErrNoRows
}

func (emptyStateStore) PutProviderState(context.Context, store.ProviderState) error { return nil }
