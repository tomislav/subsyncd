package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"subsyncd/internal/catalog"
	"subsyncd/internal/provider"
	"subsyncd/internal/testutil"
)

func TestLapseCacheStartupAndPeriodicExpiry(t *testing.T) {
	cfg := testConfig(t)
	cfg.LapseCache.TTL = 24 * time.Hour
	now := time.Now()
	clock := testutil.NewClock(now)
	dir := filepath.Join(cfg.DataDir, "lapse-cache")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "0123456789abcdef.spans")
	write := func(at time.Time) {
		t.Helper()
		if err := os.WriteFile(path, []byte("profile"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, at, at); err != nil {
			t.Fatal(err)
		}
	}
	write(now.Add(-48 * time.Hour))
	a, err := New(t.Context(), cfg, Options{Clock: clock, LapseRunner: capabilityRunner{}, ProbeRunner: probeRunner{}, Providers: map[string]provider.Provider{"english": fakeProvider{id: "english"}}, Catalogs: map[string]catalog.Catalog{"tv": fakeCatalog{}}})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("startup retained expired profile: %v", err)
	}
	write(now)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	ticks := make(chan time.Time)
	done := make(chan struct{})
	go func() { defer close(done); a.maintainLapseCache(ctx, ticks) }()
	clock.Advance(25 * time.Hour)
	ticks <- clock.Now()
	ticks <- clock.Now() // the first sweep finished before accepting the next tick
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("periodic expiry retained profile: %v", err)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("cache maintenance did not stop")
	}
	write(now.Add(-48 * time.Hour))
	// Diagnostic assembly must not clean the persistent cache.
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	diagnostic, err := New(t.Context(), cfg, Options{ReadOnly: true, LapseRunner: capabilityRunner{}, ProbeRunner: probeRunner{}})
	if err != nil {
		t.Fatal(err)
	}
	defer diagnostic.Close()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("diagnostic altered cache: %v", err)
	}
}
