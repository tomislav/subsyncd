package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"subsyncd/internal/catalog"
	"subsyncd/internal/config"
	"subsyncd/internal/domain"
	"subsyncd/internal/httpapi"
	"subsyncd/internal/observability"
	"subsyncd/internal/provider"
	"subsyncd/internal/store"
	"subsyncd/internal/syncer"
	"subsyncd/internal/worker"
)

type fakeProvider struct{ id string }

func (p fakeProvider) ID() string                          { return p.id }
func (fakeProvider) Capabilities() provider.Capabilities   { return provider.Capabilities{} }
func (fakeProvider) SupportsLanguage(domain.Language) bool { return true }
func (fakeProvider) Search(context.Context, provider.SearchQuery) ([]domain.Candidate, error) {
	return nil, nil
}
func (fakeProvider) Download(context.Context, domain.Candidate, io.Writer) (provider.DownloadMetadata, error) {
	return provider.DownloadMetadata{}, nil
}

type fakeCatalog struct{}

func (fakeCatalog) GetMedia(context.Context, domain.MediaRef) (domain.Media, error) {
	return domain.Media{}, nil
}
func (fakeCatalog) ListChanges(context.Context, time.Time, time.Time) ([]catalog.HistoryChange, error) {
	return nil, nil
}

type staticCatalog struct{ media domain.Media }

func (c staticCatalog) GetMedia(context.Context, domain.MediaRef) (domain.Media, error) {
	return c.media, nil
}
func (staticCatalog) ListChanges(context.Context, time.Time, time.Time) ([]catalog.HistoryChange, error) {
	return nil, nil
}

type capabilityRunner struct{}

func (capabilityRunner) Run(_ context.Context, command syncer.Command) (syncer.Execution, error) {
	return syncer.Execution{ExitCode: 255, Stderr: []byte("--json --strict --output --no-sidecar --no-cache")}, nil
}

type probeRunner struct{ err error }

func (r probeRunner) Run(context.Context, string, ...string) ([]byte, []byte, error) {
	return []byte("ffprobe version 1"), nil, r.err
}

type waitingWorker struct{ stopped atomic.Bool }

func (w *waitingWorker) Run(ctx context.Context) error {
	<-ctx.Done()
	w.stopped.Store(true)
	return nil
}

func testConfig(t *testing.T) config.Config {
	t.Helper()
	root := t.TempDir()
	return config.Config{
		DataDir: root + "/data", MediaRoots: []string{root}, Server: config.ServerConfig{Listen: "127.0.0.1:0"},
		Instances:            []config.InstanceConfig{{Name: "tv", Type: "sonarr", URL: "http://sonarr.invalid", APIKey: "api", WebhookToken: "webhook"}},
		Providers:            map[string]config.ProviderSpec{"english": {Type: "fake", RequestsPerSecond: 1, Burst: 1, MaxConcurrent: 1}},
		Languages:            map[domain.Language]config.LanguageConfig{"en": {Providers: []string{"english"}}},
		AllowHearingImpaired: true, MinimumReleaseScore: 35,
		Worker:       config.WorkerConfig{MaxConcurrent: 1},
		ProviderHTTP: config.ProviderHTTPConfig{SharedOriginMaxConcurrent: 1}, PackCache: config.PackCacheConfig{TTL: time.Hour, MaxBytes: 1 << 20},
		Sync: config.SyncConfig{LapsePath: "/usr/local/bin/lapse", Timeout: time.Minute}, Install: config.InstallConfig{FileMode: 0o644},
	}
}

func TestNewWiresConfiguredWorkflowConcurrency(t *testing.T) {
	cfg := testConfig(t)
	cfg.Worker.MaxConcurrent = 4
	application, err := New(context.Background(), cfg, Options{LapseRunner: capabilityRunner{}, ProbeRunner: probeRunner{}, Providers: map[string]provider.Provider{"english": fakeProvider{id: "english"}}, Catalogs: map[string]catalog.Catalog{"tv": fakeCatalog{}}})
	if err != nil {
		t.Fatal(err)
	}
	defer application.Close()
	background, ok := application.Worker.(*worker.Worker)
	if !ok {
		t.Fatalf("default worker type = %T", application.Worker)
	}
	if background.MaxWorkflows != 4 {
		t.Fatalf("worker max workflows = %d, want 4", background.MaxWorkflows)
	}
}

func TestNewWiresProviderObservabilityIntoSuppliedProvidersAndSearchers(t *testing.T) {
	cfg := testConfig(t)
	var logs bytes.Buffer
	events, err := observability.New(&logs, observability.Options{Level: "info", Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	application, err := New(context.Background(), cfg, Options{Events: events, LapseRunner: capabilityRunner{}, ProbeRunner: probeRunner{}, Providers: map[string]provider.Provider{"english": fakeProvider{id: "english"}}, Catalogs: map[string]catalog.Catalog{"tv": fakeCatalog{}}})
	if err != nil {
		t.Fatal(err)
	}
	defer application.Close()

	if _, err := application.Providers["english"].Download(context.Background(), domain.Candidate{ResultID: "candidate"}, io.Discard); err != nil {
		t.Fatal(err)
	}
	application.Workflows["en"].Searcher.Search(context.Background(), provider.SearchQuery{Media: domain.Media{Ref: domain.MediaRef{Instance: "tv", Kind: domain.MediaEpisode, FileID: 1}}, Language: "en"})

	records := decodeLogRecords(t, logs.String())
	want := map[string]bool{"provider.download_completed": false, "provider.search_started": false, "provider.search_completed": false}
	for _, record := range records {
		if _, ok := want[record["event"].(string)]; ok {
			want[record["event"].(string)] = true
		}
	}
	for event, found := range want {
		if !found {
			t.Errorf("missing %s in %s", event, logs.String())
		}
	}
}

func TestOpenUsesConfiguredEmitterAndLogsStartupLifecycle(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "config.yaml")
	text := fmt.Sprintf(`
data_dir: %q
media_roots: [%q]
server: {listen: "127.0.0.1:0"}
logging: {level: debug}
worker: {max_concurrent: 1}
instances:
  - {name: tv, type: sonarr, url: "http://sonarr.invalid", api_key: api-secret, webhook_token: hook-secret, path_mappings: [{remote: /tv, local: %q}]}
providers:
  english: {type: fake, requests_per_second: 1, burst: 1, max_concurrent: 1}
provider_http: {shared_origin_max_concurrent: 1}
languages: {en: {providers: [english]}}
pack_cache: {ttl: 1h, max_bytes: 1048576}
sync: {lapse_path: /usr/local/bin/lapse, timeout: 1m}
install: {file_mode: "0644"}
`, filepath.Join(root, "data"), root, root)
	if err := os.WriteFile(configPath, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	application, err := Open(context.Background(), configPath, OpenOptions{
		LogWriter: &logs, Version: "sha-test", Command: "doctor",
		Runtime: Options{SkipLapseCheck: true, SkipProbeCheck: true, Providers: map[string]provider.Provider{"english": fakeProvider{id: "english"}}, Catalogs: map[string]catalog.Catalog{"tv": fakeCatalog{}}, Worker: &waitingWorker{}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer application.Close()
	application.Events.For("test").Log(context.Background(), slog.LevelDebug, "test.debug", "debug enabled")
	records := decodeLogRecords(t, logs.String())
	if len(records) != 3 {
		t.Fatalf("log records = %d, want starting, ready, debug: %s", len(records), logs.String())
	}
	for index, event := range []string{"service.starting", "service.ready", "test.debug"} {
		if records[index]["event"] != event {
			t.Errorf("record %d event = %#v, want %q", index, records[index]["event"], event)
		}
	}
	if records[0]["version"] != "sha-test" || records[0]["command"] != "doctor" || records[0]["instance_count"] != float64(1) || records[0]["provider_count"] != float64(1) || records[0]["language_count"] != float64(1) {
		t.Fatalf("startup fields = %#v", records[0])
	}
}

func TestLoggingDocumentationContract(t *testing.T) {
	read := func(path string) string {
		t.Helper()
		payload, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return string(payload)
	}
	readme := read("../../README.md")
	operations := read("../../docs/operations.md")
	combined := readme + "\n" + operations
	for _, required := range []string{
		"SUBSYNCD_LOG_LEVEL", "logging:", "level: info",
		"service", "environment", "level", "component", "event",
		"job_id", "media_id", "duration_ms", "loki.process", "loki.write",
	} {
		if !strings.Contains(combined, required) {
			t.Errorf("logging documentation is missing %q", required)
		}
	}
	for _, highCardinalityLabel := range []string{"job_id", "media_id", "candidate_id", "file_id", "language", "provider"} {
		if strings.Contains(operations, highCardinalityLabel+" = label_drop") || strings.Contains(operations, highCardinalityLabel+" = \"") {
			t.Errorf("operations documentation promotes high-cardinality %s as a Loki label", highCardinalityLabel)
		}
	}
	if !strings.Contains(operations, "Loki credentials belong only in Alloy") {
		t.Error("operations documentation must keep Loki credentials out of subsyncd configuration")
	}
}

func TestOpenLogsSanitizedStartupFailure(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "config.yaml")
	text := fmt.Sprintf(`
data_dir: %q
media_roots: [%q]
server: {listen: "127.0.0.1:0"}
instances:
  - {name: tv, type: sonarr, url: "http://sonarr.invalid", api_key: api-secret, webhook_token: hook-secret, path_mappings: [{remote: /tv, local: %q}]}
providers:
  english: {type: fake, requests_per_second: 1, burst: 1, max_concurrent: 1}
languages: {en: {providers: [english]}}
sync: {lapse_path: /usr/local/bin/lapse, timeout: 1m}
`, filepath.Join(root, "data"), root, root)
	if err := os.WriteFile(configPath, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	_, err := Open(context.Background(), configPath, OpenOptions{
		LogWriter: &logs, Version: "test", Command: "serve",
		Runtime: Options{SkipLapseCheck: true, ProbeRunner: probeRunner{err: errors.New("api-secret\n" + root + "/Movie.mkv")}, Providers: map[string]provider.Provider{"english": fakeProvider{id: "english"}}, Catalogs: map[string]catalog.Catalog{"tv": fakeCatalog{}}},
	})
	if err == nil {
		t.Fatal("Open() error = nil")
	}
	for _, forbidden := range []string{"api-secret", root} {
		if strings.Contains(logs.String(), forbidden) {
			t.Fatalf("startup failure log contains %q: %s", forbidden, logs.String())
		}
	}
	records := decodeLogRecords(t, logs.String())
	if got := records[len(records)-1]["event"]; got != "service.start_failed" {
		t.Fatalf("last event = %#v, want service.start_failed", got)
	}
	if message, _ := records[len(records)-1]["error"].(string); strings.ContainsAny(message, "\r\n") {
		t.Fatalf("startup error field is multiline: %q", message)
	}
}

func TestNewWiresNonblockingCatalogWake(t *testing.T) {
	cfg := testConfig(t)
	application, err := New(context.Background(), cfg, Options{LapseRunner: capabilityRunner{}, ProbeRunner: probeRunner{}, Providers: map[string]provider.Provider{"english": fakeProvider{id: "english"}}, Catalogs: map[string]catalog.Catalog{"tv": fakeCatalog{}}})
	if err != nil {
		t.Fatal(err)
	}
	defer application.Close()
	background := application.Worker.(*worker.Worker)
	if cap(background.Wake) != 1 {
		t.Fatalf("wake capacity = %d, want 1", cap(background.Wake))
	}
	reconciler := application.Reconcilers["tv"]
	reconciler.OnCommitted()
	reconciler.OnCommitted()
	if len(background.Wake) != 1 {
		t.Fatalf("queued wakes = %d, want coalesced wake", len(background.Wake))
	}
}

func TestNewAssemblesLanguageWorkflowWithoutContactingRemoteServices(t *testing.T) {
	cfg := testConfig(t)
	application, err := New(context.Background(), cfg, Options{LapseRunner: capabilityRunner{}, ProbeRunner: probeRunner{}, Providers: map[string]provider.Provider{"english": fakeProvider{id: "english"}}, Catalogs: map[string]catalog.Catalog{"tv": fakeCatalog{}}, Worker: &waitingWorker{}})
	if err != nil {
		t.Fatal(err)
	}
	defer application.Close()
	if application.Workflows["en"] == nil || application.Catalogs["tv"] == nil || application.Providers["english"] == nil {
		t.Fatal("runtime dependencies were not assembled")
	}
	if err := application.Ready(context.Background()); err != nil {
		t.Fatalf("readiness = %v", err)
	}
}

func TestNewBackfillsNewConfiguredLanguageForIndexedMedia(t *testing.T) {
	cfg := testConfig(t)
	ctx := context.Background()
	if err := os.MkdirAll(cfg.DataDir, 0o750); err != nil {
		t.Fatal(err)
	}
	database, err := store.Open(ctx, filepath.Join(cfg.DataDir, "subsyncd.db"))
	if err != nil {
		t.Fatal(err)
	}
	media := domain.Media{
		EntityID: 7,
		Ref:      domain.MediaRef{Instance: "tv", Kind: domain.MediaMovie, FileID: 7},
		Fingerprint: domain.MediaFingerprint{
			Path: filepath.Join(cfg.MediaRoots[0], "Movie.mkv"), FileID: 7, Size: 100, ModTime: time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC),
		},
		Title: "Movie", Year: 2024,
	}
	mediaID, _, err := database.Repository().UpsertMedia(ctx, media)
	if err != nil {
		t.Fatal(err)
	}
	englishDue := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	if err := database.Repository().UpsertSearchStateWithPriority(ctx, mediaID, "en", englishDue, store.SearchPriorityUpgrade); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	cfg.Languages["hr"] = config.LanguageConfig{Providers: []string{"english"}}
	before := time.Now()
	application, err := New(ctx, cfg, Options{LapseRunner: capabilityRunner{}, ProbeRunner: probeRunner{}, Providers: map[string]provider.Provider{"english": fakeProvider{id: "english"}}, Catalogs: map[string]catalog.Catalog{"tv": fakeCatalog{}}, Worker: &waitingWorker{}})
	if err != nil {
		t.Fatal(err)
	}
	defer application.Close()
	after := time.Now()

	english, err := application.Repository.GetSearchStatus(ctx, mediaID, "en")
	if err != nil {
		t.Fatal(err)
	}
	if english.Priority != store.SearchPriorityUpgrade || !english.NextAttemptAt.Equal(englishDue) {
		t.Fatalf("existing English schedule changed: %#v", english)
	}
	croatian, err := application.Repository.GetSearchStatus(ctx, mediaID, "hr")
	if err != nil {
		t.Fatal(err)
	}
	if croatian.State != "pending" || croatian.Priority != store.SearchPriorityMissing || croatian.NextAttemptAt.Before(before) || croatian.NextAttemptAt.After(after) {
		t.Fatalf("Croatian startup backfill = %#v, want immediately pending missing search", croatian)
	}
}

func TestExplainListsActiveCandidateRejections(t *testing.T) {
	cfg := testConfig(t)
	application, err := New(context.Background(), cfg, Options{LapseRunner: capabilityRunner{}, ProbeRunner: probeRunner{}, Providers: map[string]provider.Provider{"english": fakeProvider{id: "english"}}, Catalogs: map[string]catalog.Catalog{"tv": fakeCatalog{}}, Worker: &waitingWorker{}})
	if err != nil {
		t.Fatal(err)
	}
	defer application.Close()
	now := application.Clock.Now()
	media := domain.Media{EntityID: 7, Ref: domain.MediaRef{Instance: "tv", Kind: domain.MediaMovie, FileID: 7}, Fingerprint: domain.MediaFingerprint{Path: filepath.Join(cfg.MediaRoots[0], "Movie.mkv"), FileID: 7, Size: 100, ModTime: now}, Title: "Movie", Year: 2024, ExternalIDs: domain.ExternalIDs{TMDB: 7}}
	mediaID, _, err := application.Repository.UpsertMedia(context.Background(), media)
	if err != nil {
		t.Fatal(err)
	}
	if err := application.Repository.PutCandidateRejection(context.Background(), store.CandidateRejection{MediaID: mediaID, Language: "en", ProviderID: "english", ResultID: "bad-1", CandidateSignature: "candidate", ArtifactChecksum: "artifact", ReasonCode: "lapse_unsure", ToolSignature: "tool", MediaPath: media.Fingerprint.Path, MediaFileID: 7, MediaSize: 100, MediaModTimeNS: now.UnixNano(), RejectedAt: now, ExpiresAt: now.Add(24 * time.Hour)}); err != nil {
		t.Fatal(err)
	}

	output, err := application.Explain(context.Background(), "tv", "movie", 7, "en")
	if err != nil || !strings.Contains(output, "candidate_rejections: 1") || !strings.Contains(output, "provider=english result=bad-1 reason=lapse_unsure") {
		t.Fatalf("Explain() = %q/%v", output, err)
	}
}

func TestExplainShowsSearchPriority(t *testing.T) {
	cfg := testConfig(t)
	application, err := New(context.Background(), cfg, Options{LapseRunner: capabilityRunner{}, ProbeRunner: probeRunner{}, Providers: map[string]provider.Provider{"english": fakeProvider{id: "english"}}, Catalogs: map[string]catalog.Catalog{"tv": fakeCatalog{}}, Worker: &waitingWorker{}})
	if err != nil {
		t.Fatal(err)
	}
	defer application.Close()
	now := application.Clock.Now()
	media := domain.Media{EntityID: 8, Ref: domain.MediaRef{Instance: "tv", Kind: domain.MediaMovie, FileID: 8}, Fingerprint: domain.MediaFingerprint{Path: filepath.Join(cfg.MediaRoots[0], "Priority.mkv"), FileID: 8, Size: 100, ModTime: now}, Title: "Priority"}
	mediaID, _, err := application.Repository.UpsertMedia(context.Background(), media)
	if err != nil {
		t.Fatal(err)
	}
	if err := application.Repository.UpsertSearchStateWithPriority(context.Background(), mediaID, "en", now, store.SearchPriorityImport); err != nil {
		t.Fatal(err)
	}
	if err := application.Repository.ReplaceTrackInventory(context.Background(), mediaID, media.Fingerprint, nil); err != nil {
		t.Fatal(err)
	}
	output, err := application.Explain(context.Background(), "tv", "movie", 8, "en")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output, "priority=import") || !strings.Contains(output, "rerun_pending=false") {
		t.Fatalf("Explain() search status = %q", output)
	}
}

func TestExplainShowsUnsupportedReason(t *testing.T) {
	cfg := testConfig(t)
	application, err := New(context.Background(), cfg, Options{LapseRunner: capabilityRunner{}, ProbeRunner: probeRunner{}, Providers: map[string]provider.Provider{"english": fakeProvider{id: "english"}}, Catalogs: map[string]catalog.Catalog{"tv": fakeCatalog{}}, Worker: &waitingWorker{}})
	if err != nil {
		t.Fatal(err)
	}
	defer application.Close()
	now := application.Clock.Now()
	media := domain.Media{EntityID: 9, Ref: domain.MediaRef{Instance: "tv", Kind: domain.MediaEpisode, FileID: 9}, Fingerprint: domain.MediaFingerprint{Path: filepath.Join(cfg.MediaRoots[0], "Combined.mkv"), FileID: 9, Size: 100, ModTime: now}, Title: "Combined", UnsupportedReason: domain.UnsupportedMultiEpisode}
	mediaID, _, err := application.Repository.UpsertMedia(context.Background(), media)
	if err != nil {
		t.Fatal(err)
	}
	if err := application.Repository.UpsertSearchStateWithPriority(context.Background(), mediaID, "en", now, store.SearchPriorityImport); err != nil {
		t.Fatal(err)
	}
	if err := application.Repository.ReplaceTrackInventory(context.Background(), mediaID, media.Fingerprint, nil); err != nil {
		t.Fatal(err)
	}
	output, err := application.Explain(context.Background(), "tv", "episode", 9, "en")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output, "unsupported_reason=unsupported_multi_episode") {
		t.Fatalf("Explain() = %q", output)
	}
}

func TestManualSearchRetryRejectedClearsCandidateQuarantine(t *testing.T) {
	cfg := testConfig(t)
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	mediaPath := filepath.Join(cfg.MediaRoots[0], "Movie.mkv")
	if err := os.WriteFile(mediaPath, []byte("media"), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(mediaPath)
	if err != nil {
		t.Fatal(err)
	}
	media := domain.Media{EntityID: 7, Ref: domain.MediaRef{Instance: "tv", Kind: domain.MediaMovie, FileID: 7}, Fingerprint: domain.MediaFingerprint{Path: mediaPath, FileID: 7, Size: info.Size(), ModTime: info.ModTime()}, Title: "Movie", Year: 2024, ExternalIDs: domain.ExternalIDs{TMDB: 7}}
	application, err := New(context.Background(), cfg, Options{LapseRunner: capabilityRunner{}, ProbeRunner: probeRunner{}, Providers: map[string]provider.Provider{"english": fakeProvider{id: "english"}}, Catalogs: map[string]catalog.Catalog{"tv": staticCatalog{media: media}}, Worker: &waitingWorker{}})
	if err != nil {
		t.Fatal(err)
	}
	defer application.Close()
	mediaID, _, err := application.Repository.UpsertMedia(context.Background(), media)
	if err != nil {
		t.Fatal(err)
	}
	if err := application.Repository.ReplaceTrackInventory(context.Background(), mediaID, media.Fingerprint, []store.TrackRecord{{Language: "en", Embedded: true}}); err != nil {
		t.Fatal(err)
	}
	if err := application.Repository.PutCandidateRejection(context.Background(), store.CandidateRejection{MediaID: mediaID, Language: "en", ProviderID: "english", ResultID: "bad", CandidateSignature: "candidate", ReasonCode: "lapse_unsure", ToolSignature: "tool", MediaPath: mediaPath, MediaFileID: 7, MediaSize: info.Size(), MediaModTimeNS: info.ModTime().UnixNano(), RejectedAt: now, ExpiresAt: now.Add(24 * time.Hour)}); err != nil {
		t.Fatal(err)
	}

	if _, err := application.Search(context.Background(), "tv", "movie", 7, "en", true); err != nil {
		t.Fatal(err)
	}
	rejections, err := application.Repository.ListCandidateRejections(context.Background(), mediaID, "en", now)
	if err != nil || len(rejections) != 0 {
		t.Fatalf("rejections after manual retry = %#v/%v", rejections, err)
	}
}

func TestNewReportsUnknownProviderTypeAndMissingLapse(t *testing.T) {
	t.Run("unknown provider", func(t *testing.T) {
		cfg := testConfig(t)
		_, err := New(context.Background(), cfg, Options{SkipLapseCheck: true, SkipProbeCheck: true, Catalogs: map[string]catalog.Catalog{"tv": fakeCatalog{}}, Worker: &waitingWorker{}})
		if err == nil || !strings.Contains(err.Error(), `unknown provider type "fake"`) {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("missing lapse", func(t *testing.T) {
		cfg := testConfig(t)
		cfg.Sync.LapsePath = "/definitely/missing/lapse"
		_, err := New(context.Background(), cfg, Options{})
		if err == nil || !strings.Contains(err.Error(), "run LAPSE capability check") {
			t.Fatalf("error = %v", err)
		}
	})
}

func TestNewBuildsCompiledProviderFactoriesWithoutNetworkCalls(t *testing.T) {
	cfg := testConfig(t)
	var document yaml.Node
	if err := yaml.Unmarshal([]byte("type: subdl\napi_key: test-key\nrequests_per_second: 1\nburst: 1\nmax_concurrent: 1\n"), &document); err != nil {
		t.Fatal(err)
	}
	cfg.Providers["english"] = config.ProviderSpec{Type: "subdl", RequestsPerSecond: 1, Burst: 1, MaxConcurrent: 1, Settings: *document.Content[0]}
	application, err := New(context.Background(), cfg, Options{LapseRunner: capabilityRunner{}, ProbeRunner: probeRunner{}, Catalogs: map[string]catalog.Catalog{"tv": fakeCatalog{}}, Worker: &waitingWorker{}})
	if err != nil {
		t.Fatal(err)
	}
	defer application.Close()
	if application.Providers["english"] == nil || application.Providers["english"].ID() != "english" {
		t.Fatal("compiled provider was not assembled")
	}
}

func TestServeDrainsHTTPAndWorkerOnCancellation(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	worker := &waitingWorker{}
	cfg := testConfig(t)
	if err := os.MkdirAll(cfg.DataDir, 0o750); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	events, err := observability.New(&logs, observability.Options{Level: "info", Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	application := &App{Config: cfg, Listener: listener, Handler: httpapi.Server{}.Handler(), Worker: worker, Events: events, Command: "serve"}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- application.Serve(ctx) }()

	client := http.Client{Timeout: time.Second}
	response, err := client.Get("http://" + listener.Addr().String() + "/healthz")
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("health status = %d", response.StatusCode)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("daemon did not drain")
	}
	if !worker.stopped.Load() {
		t.Fatal("worker was not drained")
	}
	records := decodeLogRecords(t, logs.String())
	if len(records) != 2 || records[0]["event"] != "service.shutdown_requested" || records[1]["event"] != "service.stopped" {
		t.Fatalf("shutdown events = %#v", records)
	}
	if records[0]["trigger"] != "context" || records[1]["outcome"] != "success" {
		t.Fatalf("shutdown fields = %#v / %#v", records[0], records[1])
	}
}

func TestMutationLockExcludesSecondProcess(t *testing.T) {
	cfg := testConfig(t)
	if err := os.MkdirAll(cfg.DataDir, 0o750); err != nil {
		t.Fatal(err)
	}
	first := &App{Config: cfg}
	second := &App{Config: cfg}
	release, err := first.acquireMutationLock()
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if _, err := second.acquireMutationLock(); err == nil {
		t.Fatal("second mutation lock unexpectedly succeeded")
	}
}

func TestRedactRemovesConfiguredSecretsAndMediaRoots(t *testing.T) {
	cfg := testConfig(t)
	var document yaml.Node
	if err := yaml.Unmarshal([]byte("api_key: provider-secret\nusername: account-name\n"), &document); err != nil {
		t.Fatal(err)
	}
	spec := cfg.Providers["english"]
	spec.Settings = *document.Content[0]
	cfg.Providers["english"] = spec
	application := &App{Config: cfg}
	message := application.Redact(errors.New("provider-secret account-name api webhook " + cfg.MediaRoots[0])).Error()
	for _, forbidden := range []string{"provider-secret", "account-name", "api", "webhook", cfg.MediaRoots[0]} {
		if strings.Contains(message, forbidden) {
			t.Fatalf("redacted error still contains %q: %s", forbidden, message)
		}
	}
}

func decodeLogRecords(t *testing.T, output string) []map[string]any {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(output), "\n")
	if len(lines) == 1 && lines[0] == "" {
		return nil
	}
	records := make([]map[string]any, 0, len(lines))
	for _, line := range lines {
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("decode log %q: %v", line, err)
		}
		records = append(records, record)
	}
	return records
}
