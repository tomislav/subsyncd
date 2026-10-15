package app

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"subsyncd/internal/catalog"
	"subsyncd/internal/config"
	"subsyncd/internal/control"
	"subsyncd/internal/domain"
	"subsyncd/internal/provider"
	"subsyncd/internal/store"
	"subsyncd/internal/worker"
)

func newStatusApp(t *testing.T, mutate func(*config.Config)) *App {
	t.Helper()
	cfg := testConfig(t)
	if mutate != nil {
		mutate(&cfg)
	}
	providers := map[string]provider.Provider{}
	for id := range cfg.Providers {
		providers[id] = fakeProvider{id: id}
	}
	application, err := New(context.Background(), cfg, Options{LapseRunner: capabilityRunner{}, ProbeRunner: probeRunner{}, Providers: providers, Catalogs: map[string]catalog.Catalog{"tv": fakeCatalog{}}, Worker: &waitingWorker{}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { application.Close() })
	return application
}

// queueEpisode stores an episode with one pending search.
func queueEpisode(t *testing.T, application *App, fileID int64, title string, priority store.SearchPriority, next time.Time) int64 {
	t.Helper()
	media := domain.Media{EntityID: fileID, Ref: domain.MediaRef{Instance: "tv", Kind: domain.MediaEpisode, FileID: fileID},
		Fingerprint: domain.MediaFingerprint{Path: filepath.Join(application.Config.MediaRoots[0], title+".mkv"), FileID: fileID, Size: 100, ModTime: time.Unix(1700000000, 0).UTC()},
		Title:       title, Season: 1, Episode: 1}
	id, _, err := application.Repository.UpsertMedia(t.Context(), media)
	if err != nil {
		t.Fatal(err)
	}
	if err := application.Repository.UpsertSearchStateWithPriority(t.Context(), id, "en", next, priority); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestStatusWithoutDaemon(t *testing.T) {
	application := newStatusApp(t, nil)
	now := application.Clock.Now()
	queueEpisode(t, application, 1, "New One", store.SearchPriorityImport, now.Add(-time.Minute))
	queueEpisode(t, application, 2, "Missing One", store.SearchPriorityMissing, now.Add(-time.Minute))
	queueEpisode(t, application, 3, "Missing Two", store.SearchPriorityMissing, now.Add(time.Hour))
	queueEpisode(t, application, 4, "Upgrade One", store.SearchPriorityUpgrade, now.Add(time.Hour))
	if err := application.Repository.EnsureInstance(t.Context(), "tv", "sonarr", "http://sonarr.invalid", now); err != nil {
		t.Fatal(err)
	}
	status, err := application.Status(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if status.Daemon.Running {
		t.Fatalf("daemon = %+v", status.Daemon)
	}
	// next_at is the earliest scheduled attempt, in the past while one is due.
	due, later := now.Add(-time.Minute).UTC(), now.Add(time.Hour).UTC()
	want := control.QueueCounts{New: control.QueueClass{Total: 1, Due: 1, NextAt: due}, Missing: control.QueueClass{Total: 2, Due: 1, NextAt: due}, Upgrade: control.QueueClass{Total: 1, NextAt: later}}
	if !queueEqual(status.Queue, want) {
		t.Fatalf("queue = %+v, want %+v", status.Queue, want)
	}
	if len(status.QueueByLibrary) != 1 || status.QueueByLibrary[0].Instance != "tv" || status.QueueByLibrary[0].Language != "en" || !queueEqual(status.QueueByLibrary[0].QueueCounts, want) {
		t.Fatalf("queue by library = %+v", status.QueueByLibrary)
	}
	if len(status.Running) != 0 || status.MediaPause != nil || len(status.Providers) != 0 {
		t.Fatalf("status = %+v", status)
	}
	if len(status.Instances) != 1 || status.Instances[0].Name != "tv" || status.Instances[0].DiscoveryComplete || status.Instances[0].Webhook != "unmanaged" {
		t.Fatalf("instances = %+v", status.Instances)
	}
}

type snapshotWorker struct {
	waitingWorker
	until time.Time
}

func (w *snapshotWorker) Snapshot() worker.Snapshot {
	return worker.Snapshot{MediaPausedUntil: w.until}
}

func TestStatusWithDaemonAddsLiveState(t *testing.T) {
	until := time.Now().Add(10 * time.Minute).UTC().Truncate(time.Second)
	application, _, _ := serveWithControlWorker(t, &snapshotWorker{until: until})
	client := dialDaemon(t, application)
	var live control.StatusResult
	if err := client.Call(t.Context(), "status", nil, &live); err != nil {
		t.Fatal(err)
	}
	if !live.Daemon.Running || live.Daemon.Version != "9.9.9" || live.Daemon.StartedAt.IsZero() || live.Daemon.StartedAt.After(time.Now()) {
		t.Fatalf("daemon = %+v", live.Daemon)
	}
	if live.MediaPause == nil || !live.MediaPause.Until.Equal(until) {
		t.Fatalf("pause = %+v", live.MediaPause)
	}
	// A pause that has ended is not reported, nor is one from a worker with
	// no snapshot.
	application2, _, _ := serveWithControlWorker(t, &waitingWorker{})
	client2 := dialDaemon(t, application2)
	var idle control.StatusResult
	if err := client2.Call(t.Context(), "status", nil, &idle); err != nil || idle.MediaPause != nil || !idle.Daemon.Running {
		t.Fatalf("idle = %+v, %v", idle, err)
	}
}

func TestStatusListsRunningSearches(t *testing.T) {
	application := newStatusApp(t, nil)
	now := application.Clock.Now()
	queueEpisode(t, application, 1, "Running Show", store.SearchPriorityMissing, now)
	queueEpisode(t, application, 2, "Waiting Show", store.SearchPriorityMissing, now.Add(time.Hour))
	leases, err := application.Repository.LeaseDueSearches(t.Context(), now, 5, time.Minute)
	if err != nil || len(leases) != 1 {
		t.Fatalf("leases = %+v, %v", leases, err)
	}
	status, err := application.Status(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(status.Running) != 1 {
		t.Fatalf("running = %+v", status.Running)
	}
	run := status.Running[0]
	if run.Title != "Running Show" || run.Language != "en" || run.Instance != "tv" || run.LeaseUntil.IsZero() {
		t.Fatalf("running = %+v", run)
	}
	// A leased search is not pending-and-due, and its scheduled time is not
	// the next attempt.
	if missing := status.Queue.Missing; missing.Total != 2 || missing.Due != 0 || !missing.NextAt.Equal(now.Add(time.Hour)) {
		t.Fatalf("queue = %+v", status.Queue)
	}
}

func queueEqual(a, b control.QueueCounts) bool {
	classEqual := func(x, y control.QueueClass) bool {
		return x.Total == y.Total && x.Due == y.Due && x.NextAt.Equal(y.NextAt)
	}
	return classEqual(a.New, b.New) && classEqual(a.Missing, b.Missing) && classEqual(a.Upgrade, b.Upgrade)
}

func TestStatusQueueByLibrary(t *testing.T) {
	application := newStatusApp(t, func(cfg *config.Config) {
		cfg.Languages["hr"] = config.LanguageConfig{Providers: []string{"english"}}
	})
	now := application.Clock.Now()
	ctx := t.Context()
	tvOne := queueEpisode(t, application, 1, "Show One", store.SearchPriorityMissing, now.Add(2*time.Hour))
	queueEpisode(t, application, 2, "Show Two", store.SearchPriorityUpgrade, now.Add(3*time.Hour))
	if err := application.Repository.UpsertSearchStateWithPriority(ctx, tvOne, "hr", now.Add(-time.Minute), store.SearchPriorityImport); err != nil {
		t.Fatal(err)
	}
	movie := domain.Media{EntityID: 5, Ref: domain.MediaRef{Instance: "films", Kind: domain.MediaMovie, FileID: 5},
		Fingerprint: domain.MediaFingerprint{Path: filepath.Join(application.Config.MediaRoots[0], "Film.mkv"), FileID: 5, Size: 100, ModTime: time.Unix(1700000000, 0).UTC()},
		Title:       "Film", Year: 2020}
	movieID, _, err := application.Repository.UpsertMedia(ctx, movie)
	if err != nil {
		t.Fatal(err)
	}
	if err := application.Repository.UpsertSearchStateWithPriority(ctx, movieID, "en", now.Add(time.Hour), store.SearchPriorityMissing); err != nil {
		t.Fatal(err)
	}
	status, err := application.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	at := func(d time.Duration) time.Time { return now.Add(d).UTC() }
	want := []control.QueueGroup{
		{Instance: "films", Language: "en", QueueCounts: control.QueueCounts{Missing: control.QueueClass{Total: 1, NextAt: at(time.Hour)}}},
		{Instance: "tv", Language: "en", QueueCounts: control.QueueCounts{Missing: control.QueueClass{Total: 1, NextAt: at(2 * time.Hour)}, Upgrade: control.QueueClass{Total: 1, NextAt: at(3 * time.Hour)}}},
		{Instance: "tv", Language: "hr", QueueCounts: control.QueueCounts{New: control.QueueClass{Total: 1, Due: 1, NextAt: at(-time.Minute)}}},
	}
	if len(status.QueueByLibrary) != len(want) {
		t.Fatalf("queue by library = %+v", status.QueueByLibrary)
	}
	for i, group := range status.QueueByLibrary {
		if group.Instance != want[i].Instance || group.Language != want[i].Language || !queueEqual(group.QueueCounts, want[i].QueueCounts) {
			t.Fatalf("group %d = %+v, want %+v", i, group, want[i])
		}
	}
	// A priority outside new, missing and upgrade has no class, so Status
	// counts it nowhere and opens no group for it. The store refuses to
	// write one, so the guard is tested directly.
	if class := queueClass(&control.QueueCounts{}, store.SearchPriority(150)); class != nil {
		t.Fatalf("class = %+v", class)
	}
	// The totals fold the groups, keeping the earliest next attempt.
	total := control.QueueCounts{New: control.QueueClass{Total: 1, Due: 1, NextAt: at(-time.Minute)}, Missing: control.QueueClass{Total: 2, NextAt: at(time.Hour)}, Upgrade: control.QueueClass{Total: 1, NextAt: at(3 * time.Hour)}}
	if !queueEqual(status.Queue, total) {
		t.Fatalf("queue = %+v, want %+v", status.Queue, total)
	}
	text := status.Text()
	for _, line := range []string{
		"queue missing: 2 pending, 0 due, next " + at(time.Hour).Format(time.RFC3339),
		"queue new: 1 pending, 1 due",
		"queue tv en: 0 new, 1 missing, 1 upgrade; 0 due, next " + at(2*time.Hour).Format(time.RFC3339),
		"queue tv hr: 1 new, 0 missing, 0 upgrade; 1 due",
	} {
		if !strings.Contains(text, line+"\n") {
			t.Errorf("text lacks %q:\n%s", line, text)
		}
	}
}

func providersConfig(cfg *config.Config) {
	cfg.Providers["backup"] = config.ProviderSpec{Type: "fake", RequestsPerSecond: 1, Burst: 1, MaxConcurrent: 1}
	cfg.Languages["en"] = config.LanguageConfig{Providers: []string{"english"}, FallbackProviders: []string{"backup"}}
}

func providerRow(t *testing.T, rows []control.ProviderRow, id string) control.ProviderRow {
	t.Helper()
	for _, row := range rows {
		if row.ID == id {
			return row
		}
	}
	t.Fatalf("no provider %s in %+v", id, rows)
	return control.ProviderRow{}
}

func TestProvidersShowsCooldownAndRoutes(t *testing.T) {
	application := newStatusApp(t, providersConfig)
	ctx := t.Context()
	now := application.Clock.Now()
	reset := now.Add(30 * time.Minute).UTC()
	if err := application.Repository.PutProviderState(ctx, store.ProviderState{ProviderID: "english", Scope: "search", Reason: "rate_limit", ResetAt: reset}); err != nil {
		t.Fatal(err)
	}
	id := queueEpisode(t, application, 1, "Show", store.SearchPriorityMissing, now)
	for outcome, n := range map[string]int{"installed": 2, "no_result": 3, "rejected": 1} {
		for range n {
			if err := application.Repository.RecordSearchHistory(ctx, id, "en", outcome, store.SearchHistoryDetail{MediaTitle: "Show", ProviderID: "english"}, now.Add(-time.Hour)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := application.Repository.RecordSearchHistory(ctx, id, "en", "installed", store.SearchHistoryDetail{MediaTitle: "Show", ProviderID: "english"}, now.Add(-48*time.Hour)); err != nil {
		t.Fatal(err)
	}
	result, err := application.ListProviders(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Providers) != 2 || result.Providers[0].ID != "backup" {
		t.Fatalf("providers = %+v", result.Providers)
	}
	english := providerRow(t, result.Providers, "english")
	if english.Type != "fake" || english.State != "cooling_down" || !english.Until.Equal(reset) || english.LastFailure != "rate_limit" {
		t.Fatalf("english = %+v", english)
	}
	if len(english.Routes) != 1 || english.Routes[0] != (control.ProviderRoute{Language: "en", Tier: "preferred"}) {
		t.Fatalf("routes = %+v", english.Routes)
	}
	if english.Last24h != (control.ProviderCounts{Installed: 2, NoResult: 3, Rejected: 1}) {
		t.Fatalf("counts = %+v", english.Last24h)
	}
	if english.LastSuccessAt.IsZero() || !english.LastSuccessAt.Before(now) || english.LastSuccessAt.Before(now.Add(-2*time.Hour)) {
		t.Fatalf("last success = %v", english.LastSuccessAt)
	}
	backup := providerRow(t, result.Providers, "backup")
	if backup.State != "available" || !backup.Until.IsZero() || backup.LastFailure != "" || !backup.LastSuccessAt.IsZero() {
		t.Fatalf("backup = %+v", backup)
	}
	if len(backup.Routes) != 1 || backup.Routes[0].Tier != "fallback" {
		t.Fatalf("backup routes = %+v", backup.Routes)
	}
	// Status lists only the providers that are not available.
	status, err := application.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(status.Providers) != 1 || status.Providers[0].ID != "english" || status.Providers[0].State != "cooling_down" || !status.Providers[0].Until.Equal(reset) {
		t.Fatalf("status providers = %+v", status.Providers)
	}
}

func TestProvidersAuthFailedState(t *testing.T) {
	application := newStatusApp(t, providersConfig)
	ctx := t.Context()
	now := application.Clock.Now()
	if err := application.Repository.PutProviderState(ctx, store.ProviderState{ProviderID: "english", Scope: "auth", Reason: "login credentials rejected", Disabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := application.Repository.PutProviderState(ctx, store.ProviderState{ProviderID: "backup", Scope: "download", Reason: "download_quota", ResetAt: now.Add(2 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	// An expired cooldown is available again.
	result, err := application.ListProviders(ctx)
	if err != nil {
		t.Fatal(err)
	}
	english := providerRow(t, result.Providers, "english")
	if english.State != "auth_failed" || english.LastFailure != "login credentials rejected" || !english.Until.IsZero() {
		t.Fatalf("english = %+v", english)
	}
	backup := providerRow(t, result.Providers, "backup")
	if backup.State != "quota_exhausted" || backup.Until.IsZero() {
		t.Fatalf("backup = %+v", backup)
	}
	if err := application.Repository.PutProviderState(ctx, store.ProviderState{ProviderID: "backup", Scope: "download", Reason: "download_quota", ResetAt: now.Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	result, err = application.ListProviders(ctx)
	if err != nil || providerRow(t, result.Providers, "backup").State != "available" {
		t.Fatalf("expired = %+v, %v", result.Providers, err)
	}
}

func TestRateLimitWithHeaderLimitIsCoolingDownNotQuota(t *testing.T) {
	application := newStatusApp(t, providersConfig)
	ctx := t.Context()
	now := application.Clock.Now()
	// What a 429 with X-RateLimit-Limit: 100 stores: the header's limit, nothing remaining.
	if err := application.Repository.PutProviderState(ctx, store.ProviderState{ProviderID: "english", Scope: "search", Reason: "x-ratelimit", Limit: 100, Remaining: 0, ResetAt: now.Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	if err := application.Repository.PutProviderState(ctx, store.ProviderState{ProviderID: "backup", Scope: "download", Reason: "download_quota", Remaining: 0, ResetAt: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	result, err := application.ListProviders(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := providerRow(t, result.Providers, "english"); got.State != "cooling_down" || got.LastFailure != "x-ratelimit" {
		t.Fatalf("english = %+v", got)
	}
	if got := providerRow(t, result.Providers, "backup"); got.State != "quota_exhausted" {
		t.Fatalf("backup = %+v", got)
	}
}

func TestRateLimitWindowWithCapacityLeftIsAvailable(t *testing.T) {
	application := newStatusApp(t, providersConfig)
	ctx := t.Context()
	// What a successful response with rate-limit headers stores: requests left
	// in the window. The provider gate lets searches through, so it is usable.
	if err := application.Repository.PutProviderState(ctx, store.ProviderState{ProviderID: "english", Scope: "search", Reason: "x-ratelimit", Limit: 100, Remaining: 40, ResetAt: application.Clock.Now().Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	result, err := application.ListProviders(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := providerRow(t, result.Providers, "english"); got.State != "available" || !got.Until.IsZero() {
		t.Fatalf("english = %+v", got)
	}
}

func TestStatusHasNoAbsolutePaths(t *testing.T) {
	application := newStatusApp(t, providersConfig)
	ctx := t.Context()
	now := application.Clock.Now()
	queueEpisode(t, application, 1, "Running Show", store.SearchPriorityMissing, now)
	if _, err := application.Repository.LeaseDueSearches(ctx, now, 1, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := application.Repository.PutProviderState(ctx, store.ProviderState{ProviderID: "english", Scope: "auth", Reason: "login credentials rejected", Disabled: true}); err != nil {
		t.Fatal(err)
	}
	status, err := application.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	providers, err := application.ListProviders(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []control.Result{status, providers} {
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		text := string(encoded) + value.Text()
		for _, forbidden := range []string{application.Config.DataDir, application.Config.MediaRoots[0], "sonarr.invalid"} {
			if strings.Contains(text, forbidden) {
				t.Fatalf("output contains %q: %s", forbidden, text)
			}
		}
	}
}
