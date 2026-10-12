package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"subsyncd/internal/catalog"
	"subsyncd/internal/domain"
	"subsyncd/internal/observability"
	"subsyncd/internal/provider"
	"subsyncd/internal/store"
	"subsyncd/internal/workflow"
)

func TestOnlyMutableAssemblySettlesPendingInstalls(t *testing.T) {
	// A diagnostic command (doctor, explain) started while the daemon is
	// between publishing a subtitle and recording it must not undo it: it
	// does not hold the mutation lock, so the pending row is not a crash.
	ctx := context.Background()
	cfg := testConfig(t)
	options := Options{LapseRunner: capabilityRunner{}, ProbeRunner: probeRunner{}, Providers: map[string]provider.Provider{"english": fakeProvider{id: "english"}}, Catalogs: map[string]catalog.Catalog{"tv": fakeCatalog{}}}
	daemon, err := New(ctx, cfg, options)
	if err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(cfg.MediaRoots[0], "Movie.en.srt")
	if err := os.WriteFile(destination, []byte("subtitle"), 0o644); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte("subtitle"))
	if _, err := daemon.Repository.BeginPendingInstall(ctx, store.PendingInstall{MediaID: 1, Language: "en", Destination: destination, Checksum: hex.EncodeToString(sum[:])}); err != nil {
		t.Fatal(err)
	}
	readOnly := options
	readOnly.ReadOnly = true
	for _, command := range []string{"doctor", "explain", "analyze-sync", "forget-instance-preview"} {
		if !readOnlyCommand(command) {
			t.Fatalf("%s is not a read-only command", command)
		}
	}
	diagnostic, err := New(ctx, cfg, readOnly)
	if err != nil {
		t.Fatal(err)
	}
	diagnostic.Close()
	if _, err := os.Stat(destination); err != nil {
		t.Fatalf("read-only assembly touched the in-flight install: %v", err)
	}
	if pending, err := daemon.Repository.ListPendingInstalls(ctx); err != nil || len(pending) != 1 {
		t.Fatalf("pending installs after read-only assembly = %+v/%v", pending, err)
	}
	if err := daemon.Close(); err != nil {
		t.Fatal(err)
	}
	// The next mutable start holds the lock: the row is a crash and is undone.
	restarted, err := New(ctx, cfg, options)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	if _, err := os.Stat(destination); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("mutable start kept the unrecorded subtitle: %v", err)
	}
}

type blockingRecoverer struct{ release chan struct{} }

func (r blockingRecoverer) RecoverPendingInstalls(ctx context.Context) (int, error) {
	<-r.release
	return 0, ctx.Err()
}

func TestStartupRecoveryOnAHungMountTimesOut(t *testing.T) {
	var logs bytes.Buffer
	events, err := observability.New(&logs, observability.Options{Level: "info", Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	recoverer := blockingRecoverer{release: make(chan struct{})}
	defer close(recoverer.release)
	started := time.Now()
	recoverPendingInstalls(context.Background(), recoverer, events, 50*time.Millisecond)
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("startup waited %s for a hung recovery", elapsed)
	}
	var found bool
	for _, record := range decodeLogRecords(t, logs.String()) {
		if record["event"] == "install.recovery_timed_out" && record["level"] == "warn" && record["timeout_ms"] == float64(50) {
			found = true
		}
	}
	if !found {
		t.Fatalf("no install.recovery_timed_out warning: %s", logs.String())
	}
}

func TestStartupWarnsAboutWebhooksOfUnconfiguredInstances(t *testing.T) {
	ctx := context.Background()
	cfg := testConfig(t)
	if err := os.MkdirAll(cfg.DataDir, 0o750); err != nil {
		t.Fatal(err)
	}
	database, err := store.Open(ctx, filepath.Join(cfg.DataDir, "subsyncd.db"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)
	for _, instance := range []string{"tv", "old-radarr", "old-radarr"} {
		if _, err := database.Repository().EnqueueWebhook(ctx, instance, []byte(`{}`), now); err != nil {
			t.Fatal(err)
		}
	}
	database.Close()
	var logs bytes.Buffer
	events, err := observability.New(&logs, observability.Options{Level: "info", Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	application, err := New(ctx, cfg, Options{LapseRunner: capabilityRunner{}, ProbeRunner: probeRunner{}, Providers: map[string]provider.Provider{"english": fakeProvider{id: "english"}}, Catalogs: map[string]catalog.Catalog{"tv": fakeCatalog{}}, Events: events})
	if err != nil {
		t.Fatal(err)
	}
	defer application.Close()
	var warnings []map[string]any
	for _, record := range decodeLogRecords(t, logs.String()) {
		if record["event"] == "webhook.inbox_orphaned" {
			warnings = append(warnings, record)
		}
	}
	if len(warnings) != 1 || warnings[0]["instance"] != "old-radarr" || warnings[0]["webhook_count"] != float64(2) || warnings[0]["level"] != "warn" {
		t.Fatalf("orphaned webhook warnings = %#v", warnings)
	}
}

func TestMutableStartDeletesRollbackCopiesOlderBuildsKept(t *testing.T) {
	ctx := context.Background()
	cfg := testConfig(t)
	options := Options{LapseRunner: capabilityRunner{}, ProbeRunner: probeRunner{}, Providers: map[string]provider.Provider{"english": fakeProvider{id: "english"}}, Catalogs: map[string]catalog.Catalog{"tv": fakeCatalog{}}}
	daemon, err := New(ctx, cfg, options)
	if err != nil {
		t.Fatal(err)
	}
	root := cfg.MediaRoots[0]
	media := domain.Media{EntityID: 7, Ref: domain.MediaRef{Instance: "tv", Kind: domain.MediaMovie, FileID: 7}, Title: "Movie", Fingerprint: domain.MediaFingerprint{Path: filepath.Join(root, "Movie.mkv"), FileID: 7, Size: 5, ModTime: time.Unix(100, 0).UTC()}}
	mediaID, _, err := daemon.Repository.UpsertMedia(ctx, media)
	if err != nil {
		t.Fatal(err)
	}
	sidecar := filepath.Join(root, "Movie.en.srt")
	rollback := filepath.Join(root, ".subsyncd-rollback-123")
	for _, path := range []string{sidecar, rollback} {
		if err := os.WriteFile(path, []byte("subtitle"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	installation := store.Installation{MediaID: mediaID, Language: "en", Path: sidecar, Checksum: "sum", RollbackPath: rollback, MediaPath: media.Fingerprint.Path, MediaFileID: 7, MediaSize: 5, MediaModTimeNS: media.Fingerprint.ModTime.UnixNano()}
	if err := daemon.Repository.RecordInstallation(ctx, installation); err != nil {
		t.Fatal(err)
	}
	if err := daemon.Close(); err != nil {
		t.Fatal(err)
	}
	// A read-only diagnostic leaves the copy alone.
	readOnly := options
	readOnly.ReadOnly = true
	diagnostic, err := New(ctx, cfg, readOnly)
	if err != nil {
		t.Fatal(err)
	}
	diagnostic.Close()
	if _, err := os.Stat(rollback); err != nil {
		t.Fatalf("read-only assembly touched the rollback copy: %v", err)
	}
	var logs bytes.Buffer
	events, err := observability.New(&logs, observability.Options{Level: "info", Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	options.Events = events
	restarted, err := New(ctx, cfg, options)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	if _, err := os.Stat(rollback); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rollback copy after a mutable start: %v", err)
	}
	if got, _, err := restarted.Repository.GetInstallation(ctx, mediaID, "en"); err != nil || got.RollbackPath != "" {
		t.Fatalf("installation = %+v/%v", got, err)
	}
	var found bool
	for _, record := range decodeLogRecords(t, logs.String()) {
		if record["event"] == "install.rollback_sweep_completed" && record["removed_count"] == float64(1) && record["skipped_count"] == float64(0) {
			found = true
		}
	}
	if !found {
		t.Fatalf("no install.rollback_sweep_completed summary: %s", logs.String())
	}
}

type blockingSweeper struct{ release chan struct{} }

func (s blockingSweeper) DiscardRollbackCopies(ctx context.Context) (workflow.RollbackSweep, error) {
	<-s.release
	return workflow.RollbackSweep{}, ctx.Err()
}

func TestRollbackSweepOnAHungMountTimesOut(t *testing.T) {
	var logs bytes.Buffer
	events, err := observability.New(&logs, observability.Options{Level: "info", Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	sweeper := blockingSweeper{release: make(chan struct{})}
	defer close(sweeper.release)
	started := time.Now()
	discardRollbackCopies(context.Background(), sweeper, events, 50*time.Millisecond)
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("startup waited %s for a hung sweep", elapsed)
	}
	var found bool
	for _, record := range decodeLogRecords(t, logs.String()) {
		if record["event"] == "install.rollback_sweep_timed_out" && record["level"] == "warn" && record["timeout_ms"] == float64(50) {
			found = true
		}
	}
	if !found {
		t.Fatalf("no install.rollback_sweep_timed_out warning: %s", logs.String())
	}
}
