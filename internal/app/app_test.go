package app

import (
	"bytes"
	"context"
	"database/sql"
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
	"subsyncd/internal/cli"
	"subsyncd/internal/config"
	"subsyncd/internal/domain"
	"subsyncd/internal/httpapi"
	"subsyncd/internal/notifier"
	"subsyncd/internal/observability"
	"subsyncd/internal/provider"
	"subsyncd/internal/store"
	"subsyncd/internal/syncer"
	"subsyncd/internal/testutil"
	"subsyncd/internal/worker"
	"subsyncd/internal/workflow"
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

func TestNewQueuesSiloIntentForManualAndDaemonInstallations(t *testing.T) {
	for _, mode := range []string{"manual", "daemon"} {
		for _, language := range []domain.Language{"en", "hr"} {
			t.Run(mode+"/"+language.String(), func(t *testing.T) {
				ctx := context.Background()
				cfg := testConfig(t)
				cfg.Languages["hr"] = config.LanguageConfig{Providers: []string{"english"}}
				cfg.Silo = config.SiloConfig{Enabled: true, URL: "http://silo.invalid", APIKey: "test-key"}
				root := cfg.MediaRoots[0]
				mediaPath := filepath.Join(root, "Movie.mkv")
				if err := os.WriteFile(mediaPath, []byte("media"), 0o600); err != nil {
					t.Fatal(err)
				}
				info, err := os.Stat(mediaPath)
				if err != nil {
					t.Fatal(err)
				}
				now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
				media := domain.Media{EntityID: 7, Ref: domain.MediaRef{Instance: "tv", Kind: domain.MediaMovie, FileID: 7}, Title: "Movie", Fingerprint: domain.MediaFingerprint{Path: mediaPath, FileID: 7, Size: info.Size(), ModTime: info.ModTime()}}
				application, err := New(ctx, cfg, Options{Clock: testutil.NewClock(now), LapseRunner: capabilityRunner{}, ProbeRunner: notificationProbe{}, Providers: map[string]provider.Provider{"english": notificationProvider{}}, Catalogs: map[string]catalog.Catalog{"tv": staticCatalog{media: media}}})
				if err != nil {
					t.Fatal(err)
				}
				defer application.Close()
				var delivered notificationDelivery
				background := application.Worker.(*worker.Worker)
				background.Notifiers = map[string]notifier.Notifier{"silo": &delivered}
				var mediaID int64
				if mode == "manual" {
					output, err := application.Search(ctx, "tv", "movie", 7, language.String(), false)
					if err != nil || !strings.Contains(output, "installed") {
						t.Fatalf("manual output/error = %q/%v", output, err)
					}
				} else {
					mediaID, _, err = application.Repository.UpsertMedia(ctx, media)
					if err != nil {
						t.Fatal(err)
					}
					if err := application.Repository.UpsertSearchStateWithPriority(ctx, mediaID, language, now, store.SearchPriorityImport); err != nil {
						t.Fatal(err)
					}
					if err := background.RunOnce(ctx); err != nil {
						t.Fatal(err)
					}
					if delivered.calls != 1 || delivered.path != filepath.Join(root, "Movie."+language.String()+".srt") {
						t.Fatalf("delivery = %#v", delivered)
					}
				}
				mediaID, _, err = application.Repository.FindMedia(ctx, media.Ref)
				if err != nil {
					t.Fatal(err)
				}
				installation, found, err := application.Repository.GetInstallation(ctx, mediaID, language)
				if err != nil || !found || installation.Checksum == "" {
					t.Fatalf("installation = %#v/%v/%v", installation, found, err)
				}
				if mode == "manual" {
					leases, err := application.Repository.LeaseDueNotifications(ctx, now, 10, time.Minute)
					if err != nil || len(leases) != 1 || leases[0].Notifier != "silo" {
						t.Fatalf("intents = %#v/%v", leases, err)
					}
					var payload workflow.NotificationPayload
					if err := json.Unmarshal(leases[0].PayloadJSON, &payload); err != nil || payload.SubtitlePath != installation.Path || payload.Media.Ref != media.Ref {
						t.Fatalf("payload = %#v/%v", payload, err)
					}
				}
			})
		}
	}
}

type notificationProvider struct{}

func (notificationProvider) ID() string { return "english" }
func (notificationProvider) Capabilities() provider.Capabilities {
	return provider.Capabilities{ExactFileHash: true}
}
func (notificationProvider) SupportsLanguage(domain.Language) bool { return true }
func (notificationProvider) Search(_ context.Context, query provider.SearchQuery) ([]domain.Candidate, error) {
	return []domain.Candidate{{ProviderID: "english", ResultID: "one", Language: query.Language, Kind: domain.MediaMovie, Title: "Movie", ExactHash: true}}, nil
}
func (notificationProvider) Download(_ context.Context, _ domain.Candidate, out io.Writer) (provider.DownloadMetadata, error) {
	_, err := io.WriteString(out, "1\n00:00:01,000 --> 00:00:02,000\nHello\n")
	return provider.DownloadMetadata{Filename: "Movie.srt"}, err
}

type notificationProbe struct{}

func (notificationProbe) Run(_ context.Context, _ string, args ...string) ([]byte, []byte, error) {
	if len(args) == 1 && args[0] == "-version" {
		return []byte("ffprobe version 1"), nil, nil
	}
	return []byte(`{"streams":[],"format":{"duration":"100"}}`), nil, nil
}

type notificationDelivery struct {
	calls int
	path  string
}

func (n *notificationDelivery) SubtitleChanged(_ context.Context, _ domain.Media, path string) error {
	n.calls++
	n.path = path
	return nil
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
	application.Workflows["en"].Searcher.Search(context.Background(), provider.SearchQuery{Media: domain.Media{Ref: domain.MediaRef{Instance: "tv", Kind: domain.MediaEpisode, FileID: 1}}, Language: "en", Mode: provider.SearchBroad})

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
	if err := os.MkdirAll(filepath.Join(root, "data"), 0o750); err != nil {
		t.Fatal(err)
	}
	initialized, err := store.Open(context.Background(), filepath.Join(root, "data", "subsyncd.db"))
	if err != nil {
		t.Fatal(err)
	}
	initialized.Close()
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
	for _, document := range []struct {
		path     string
		required []string
	}{
		{"../../README.md", []string{"docs/logging.md"}},
		{"../../docs/operations.md", []string{"(logging.md)"}},
		{"../../docs/logging.md", []string{
			"docker compose logs", "SUBSYNCD_LOG_LEVEL", "logging:", "level: info",
			"job_id", "outcome", "reason", "development/logging.md",
		}},
		{"../../docs/development/logging.md", []string{
			"service", "level", "component", "event", "job_id", "media_id", "duration_ms",
			"Credentials", "absolute media/data/temp paths", "never intentional log fields",
		}},
	} {
		t.Run(document.path, func(t *testing.T) {
			content := read(document.path)
			for _, required := range document.required {
				if !strings.Contains(content, required) {
					t.Errorf("%s is missing %q", document.path, required)
				}
			}
		})
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
		if strings.Contains(err.Error(), forbidden) {
			t.Fatalf("direct Open leaked %q: %v", forbidden, err)
		}
		if strings.Contains(logs.String(), forbidden) {
			t.Fatalf("startup failure log contains %q: %s", forbidden, logs.String())
		}
	}
	var cliLogs bytes.Buffer
	command := cli.Command{Stderr: &cliLogs, Open: func(ctx context.Context, path, command string) (cli.Backend, error) {
		return Open(ctx, path, OpenOptions{LogWriter: &cliLogs, Command: command, Runtime: Options{SkipLapseCheck: true, ProbeRunner: probeRunner{err: errors.New("api-secret " + root + "/data/private " + os.TempDir() + "/scratch /usr/local/bin/lapse")}}})
	}}
	if code := command.Run(context.Background(), []string{"serve", "--config", configPath}); code != cli.ExitFailure {
		t.Fatalf("code=%d", code)
	}
	if strings.Contains(cliLogs.String(), "subsyncd:") || strings.Contains(cliLogs.String(), "api-secret") || strings.Contains(cliLogs.String(), root) || strings.Contains(cliLogs.String(), "/usr/local/bin/lapse") {
		t.Fatalf("duplicate/raw startup output: %s", cliLogs.String())
	}
	if strings.Count(cliLogs.String(), "service.start_failed") != 1 {
		t.Fatalf("failure ownership: %s", cliLogs.String())
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
	if err := application.Repository.ReplaceTrackInventory(context.Background(), mediaID, media.Fingerprint, media.Fingerprint, nil); err != nil {
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
	if err := application.Repository.ReplaceTrackInventory(context.Background(), mediaID, media.Fingerprint, media.Fingerprint, nil); err != nil {
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
	if err := application.Repository.ReplaceTrackInventory(context.Background(), mediaID, media.Fingerprint, media.Fingerprint, []store.TrackRecord{{Language: "en", Embedded: true}}); err != nil {
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

func TestRedactTemporaryPathsInErrorsAndLogs(t *testing.T) {
	tempRoot := filepath.Join(t.TempDir(), "scratch storage")
	t.Setenv("TMPDIR", tempRoot)
	cfg := testConfig(t)
	application := &App{Config: cfg}
	failure := fmt.Errorf("prepare candidate: %w", &os.PathError{Op: "mkdir", Path: filepath.Join(tempRoot, ".subsyncd-work-123"), Err: os.ErrPermission})
	var logs bytes.Buffer
	events, err := observability.New(&logs, observability.Options{Level: "info", Redact: func(err error) string { return redactedError(err, cfg) }})
	if err != nil {
		t.Fatal(err)
	}
	events.For("worker").Log(context.Background(), slog.LevelError, "job.failed", "job failed", events.ErrorAttrs("filesystem", failure)...)
	decodeLogRecords(t, logs.String())
	for _, message := range []string{application.Redact(failure).Error(), logs.String()} {
		if strings.Contains(message, tempRoot) || !strings.Contains(message, "mkdir") || !strings.Contains(message, "permission denied") {
			t.Fatalf("temporary error lost privacy or useful cause: %s", message)
		}
	}
}

func TestTemporaryRootRedactionBoundaries(t *testing.T) {
	for _, test := range []struct{ root, input, want string }{
		{"/tmp", "mkdir /tmp: permission denied", "mkdir [temp]: permission denied"},
		{"/tmp/", "open /tmp/work: denied", "open [temp]/work: denied"},
		{"/tmp", "open /tmp-other/work: denied", "open /tmp-other/work: denied"},
		{"/tmp", "open /other/tmp/work: denied", "open /other/tmp/work: denied"},
		{"/", "open /work: read/write denied", "open [temp]/work: read/write denied"},
	} {
		if got := redactTemporaryRoot(test.input, test.root); got != test.want {
			t.Errorf("root %q: got %q, want %q", test.root, got, test.want)
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

func TestNewLocksBeforePersistentAssembly(t *testing.T) {
	cfg := testConfig(t)
	options := Options{LapseRunner: capabilityRunner{}, ProbeRunner: probeRunner{}, Providers: map[string]provider.Provider{"english": fakeProvider{id: "english"}}, Catalogs: map[string]catalog.Catalog{"tv": fakeCatalog{}}}
	first, err := New(context.Background(), cfg, options)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	cfg.Instances[0].Name = "unexpected"
	options.Catalogs = map[string]catalog.Catalog{"unexpected": fakeCatalog{}}
	second, err := New(context.Background(), cfg, options)
	if err == nil {
		second.Close()
		t.Fatal("second mutable assembly succeeded while first owns lifecycle")
	}
	inspection, err := sql.Open("sqlite", filepath.Join(cfg.DataDir, "subsyncd.db"))
	if err != nil {
		t.Fatal(err)
	}
	var inserted int
	if err := inspection.QueryRow(`SELECT count(*) FROM instances WHERE name='unexpected'`).Scan(&inserted); err != nil {
		t.Fatal(err)
	}
	inspection.Close()
	if inserted != 0 {
		t.Fatal("rejected second assembly persisted configured instance")
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	third, err := New(context.Background(), cfg, options)
	if err != nil {
		t.Fatalf("lock retained after Close: %v", err)
	}
	third.Close()
}

func TestDiagnosticAssemblyIsReadOnlyWhileMutatorOwnsLock(t *testing.T) {
	ctx := context.Background()
	cfg := testConfig(t)
	options := Options{LapseRunner: capabilityRunner{}, ProbeRunner: probeRunner{}, Providers: map[string]provider.Provider{"english": fakeProvider{id: "english"}}, Catalogs: map[string]catalog.Catalog{"tv": fakeCatalog{}}}
	first, err := New(ctx, cfg, options)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	options.ReadOnly = true
	diagnostic, err := New(ctx, cfg, options)
	if err != nil {
		t.Fatal(err)
	}
	defer diagnostic.Close()
	if err := diagnostic.Repository.EnsureInstance(ctx, "forbidden", "sonarr", "http://fake.invalid", time.Now()); err == nil {
		t.Fatal("diagnostic database accepted write")
	}
	if _, err := diagnostic.Retry(ctx, "english"); err == nil {
		t.Fatal("diagnostic accepted mutation")
	}
	if _, err := diagnostic.Doctor(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestExplainReportsCompletedEmptyProbeWithoutRefresh(t *testing.T) {
	ctx := context.Background()
	cfg := testConfig(t)
	app, err := New(ctx, cfg, Options{LapseRunner: capabilityRunner{}, ProbeRunner: probeRunner{}, Providers: map[string]provider.Provider{"english": fakeProvider{id: "english"}}, Catalogs: map[string]catalog.Catalog{"tv": fakeCatalog{}}})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	media := domain.Media{EntityID: 7, Ref: domain.MediaRef{Instance: "tv", Kind: domain.MediaMovie, FileID: 7}, Fingerprint: domain.MediaFingerprint{Path: filepath.Join(cfg.MediaRoots[0], "movie.mkv"), FileID: 7, Size: 1, ModTime: time.Now()}, Title: "Movie"}
	if err := os.WriteFile(media.Fingerprint.Path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(media.Fingerprint.Path)
	if err != nil {
		t.Fatal(err)
	}
	media.Fingerprint.ModTime = info.ModTime()
	id, _, err := app.Repository.UpsertMedia(ctx, media)
	if err != nil {
		t.Fatal(err)
	}
	for _, state := range []string{"unprobed", "completed", "stale"} {
		if state == "completed" {
			if err := app.Repository.ReplaceTrackInventory(ctx, id, media.Fingerprint, media.Fingerprint, nil); err != nil {
				t.Fatal(err)
			}
		}
		if state == "stale" {
			if err := os.WriteFile(media.Fingerprint.Path, []byte("changed"), 0o600); err != nil {
				t.Fatal(err)
			}
		}

		output, err := app.Explain(ctx, "tv", "movie", 7, "en")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(output, "probe="+state) {
			t.Fatalf("want probe=%s: %s", state, output)
		}
	}
}

func TestFailedAssemblyReleasesLockAndDiagnosticsDoNotInitialize(t *testing.T) {
	ctx := context.Background()
	cfg := testConfig(t)
	options := Options{ReadOnly: true, SkipLapseCheck: true, SkipProbeCheck: true, Catalogs: map[string]catalog.Catalog{"tv": fakeCatalog{}}}
	if app, err := New(ctx, cfg, options); err == nil {
		app.Close()
		t.Fatal("diagnostic initialized fresh data directory")
	}
	if _, err := os.Stat(cfg.DataDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("diagnostic created data: %v", err)
	}
	options.ReadOnly = false
	if app, err := New(ctx, cfg, options); err == nil {
		app.Close()
		t.Fatal("expected unknown provider failure")
	}
	options.Providers = map[string]provider.Provider{"english": fakeProvider{id: "english"}}
	app, err := New(ctx, cfg, options)
	if err != nil {
		t.Fatalf("failed assembly retained lock: %v", err)
	}
	defer app.Close()
	for _, name := range []string{"pack-cache", "lapse-cache"} {
		if err := os.RemoveAll(filepath.Join(cfg.DataDir, name)); err != nil {
			t.Fatal(err)
		}
	}
	media := domain.Media{EntityID: 7, Ref: domain.MediaRef{Instance: "tv", Kind: domain.MediaMovie, FileID: 7}, Fingerprint: domain.MediaFingerprint{Path: filepath.Join(cfg.MediaRoots[0], "movie.mkv"), FileID: 7, Size: 1, ModTime: time.Now()}, Title: "Movie"}
	id, _, err := app.Repository.UpsertMedia(ctx, media)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Languages["hr"] = config.LanguageConfig{Providers: []string{"english"}}
	options.ReadOnly = true
	diagnostic, err := New(ctx, cfg, options)
	if err != nil {
		t.Fatal(err)
	}
	defer diagnostic.Close()
	if _, err := diagnostic.Repository.GetSearchStatus(ctx, id, "hr"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("diagnostic backfilled language: %v", err)
	}
	for _, name := range []string{"pack-cache", "lapse-cache"} {
		if _, err := os.Stat(filepath.Join(cfg.DataDir, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("diagnostic created %s: %v", name, err)
		}
	}
}

type diagnosticCacheRunner struct{ cache string }

func (r *diagnosticCacheRunner) Run(_ context.Context, command syncer.Command) (syncer.Execution, error) {
	for _, env := range command.Env {
		if strings.HasPrefix(env, "LAPSE_CACHE=") {
			r.cache = strings.TrimPrefix(env, "LAPSE_CACHE=")
		}
	}
	if r.cache == "" {
		return syncer.Execution{}, errors.New("missing cache")
	}
	if err := os.WriteFile(filepath.Join(r.cache, "speech.cache"), []byte("fake"), 0o600); err != nil {
		return syncer.Execution{}, err
	}
	return syncer.Execution{}, errors.New("fake technical failure")
}
func TestAnalyzeSyncRemovesPrivateSpeechCacheOnFailure(t *testing.T) {
	cfg := testConfig(t)
	runner := &diagnosticCacheRunner{}
	subtitle := filepath.Join(t.TempDir(), "sample.srt")
	if err := os.WriteFile(subtitle, []byte("1\n00:00:01,000 --> 00:00:02,000\nHello\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	diagnostic := &App{Config: cfg, LapseRunner: runner, readOnly: true}
	if _, err := diagnostic.AnalyzeSync(context.Background(), filepath.Join(cfg.MediaRoots[0], "movie.mkv"), subtitle); err == nil {
		t.Fatal("expected fake failure")
	}
	if runner.cache == "" || withinRoots(runner.cache, []string{cfg.DataDir}) {
		t.Fatalf("persistent or absent cache: %q", runner.cache)
	}
	if _, err := os.Stat(filepath.Dir(runner.cache)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("private diagnostic cache remains: %v", err)
	}
}

func TestNewScopesOnlyWorkerSearchClaims(t *testing.T) {
	cfg := testConfig(t)
	application, err := New(context.Background(), cfg, Options{LapseRunner: capabilityRunner{}, ProbeRunner: probeRunner{}, Providers: map[string]provider.Provider{"english": fakeProvider{id: "english"}}, Catalogs: map[string]catalog.Catalog{"tv": fakeCatalog{}}})
	if err != nil {
		t.Fatal(err)
	}
	defer application.Close()
	now := time.Now()
	for index, route := range []struct {
		instance string
		language domain.Language
	}{{"removed", "en"}, {"tv", "hr"}, {"tv", "en"}} {
		media := domain.Media{EntityID: int64(index + 1), Ref: domain.MediaRef{Instance: route.instance, Kind: domain.MediaMovie, FileID: int64(index + 1)}, Fingerprint: domain.MediaFingerprint{FileID: int64(index + 1), Path: filepath.Join(cfg.MediaRoots[0], fmt.Sprintf("%d.mkv", index)), Size: 100, ModTime: now}, Title: "Movie"}
		id, _, err := application.Repository.UpsertMedia(context.Background(), media)
		if err != nil {
			t.Fatal(err)
		}
		if err := application.Repository.UpsertSearchStateWithPriority(context.Background(), id, route.language, now, store.SearchPriorityMissing); err != nil {
			t.Fatal(err)
		}
	}
	background := application.Worker.(*worker.Worker)
	leases, err := background.Repository.LeaseDueSearches(context.Background(), now, 1, time.Minute)
	if err != nil || len(leases) != 1 || leases[0].MediaID != 3 || leases[0].Language != "en" {
		t.Fatalf("worker scope claims=%+v error=%v", leases, err)
	}
	leases, err = application.Repository.LeaseDueSearches(context.Background(), now, 10, time.Minute)
	if err != nil || len(leases) != 2 {
		t.Fatalf("ordinary repository claims=%+v error=%v", leases, err)
	}
}

// A retained catalog deletion must not prevent scanning later active media.
func TestScanSkipsDeletedMedia(t *testing.T) {
	for _, concurrentDelete := range []bool{false, true} {
		t.Run(fmt.Sprint(concurrentDelete), func(t *testing.T) {
			ctx := context.Background()
			cfg := testConfig(t)
			application, err := New(ctx, cfg, Options{SkipLapseCheck: true, SkipProbeCheck: true, Providers: map[string]provider.Provider{"english": fakeProvider{id: "english"}}, Catalogs: map[string]catalog.Catalog{"tv": fakeCatalog{}}})
			if err != nil {
				t.Fatal(err)
			}
			defer application.Close()
			var active domain.Media
			for i := int64(1); i <= 2; i++ {
				path := filepath.Join(cfg.MediaRoots[0], fmt.Sprintf("movie%d.mkv", i))
				if err := os.WriteFile(path, []byte("media"), 0600); err != nil {
					t.Fatal(err)
				}
				info, err := os.Stat(path)
				if err != nil {
					t.Fatal(err)
				}
				media := domain.Media{Ref: domain.MediaRef{Instance: "tv", Kind: domain.MediaMovie, FileID: i}, EntityID: i, Fingerprint: domain.MediaFingerprint{Path: path, FileID: i, Size: info.Size(), ModTime: info.ModTime()}}
				if _, _, err := application.Repository.UpsertMedia(ctx, media); err != nil {
					t.Fatal(err)
				}
				if i == 1 {
					if _, err := application.Repository.ApplyMediaEvent(ctx, store.MediaEventMutation{EventID: "deleted", Type: "delete", Ref: media.Ref, At: time.Now()}); err != nil {
						t.Fatal(err)
					}
				} else {
					active = media
				}
			}
			if result, err := application.Scan(ctx, "tv", false); err != nil || result != "scan complete: instance=tv media=1 force_probe=false" {
				t.Fatalf("ordinary scan=%q error=%v", result, err)
			}
			calls := 0
			application.Inventory.Probe.Runner = scanProbeFunc(func(path string) error {
				calls++
				if path != active.Fingerprint.Path {
					t.Errorf("probed tombstone %q", path)
				}
				if concurrentDelete {
					_, err := application.Repository.ApplyMediaEvent(ctx, store.MediaEventMutation{EventID: "concurrent-delete", Type: "delete", Ref: active.Ref, At: time.Now()})
					return err
				}
				return nil
			})
			result, err := application.Scan(ctx, "tv", true)
			if concurrentDelete {
				if !errors.Is(err, store.ErrStaleInventory) {
					t.Fatalf("concurrent delete error=%v", err)
				}
			} else if err != nil || result != "scan complete: instance=tv media=1 force_probe=true" {
				t.Fatalf("scan=%q error=%v", result, err)
			}
			if calls != 1 {
				t.Fatalf("probe calls=%d, want active media only", calls)
			}
		})
	}
}

type scanProbeFunc func(string) error

func (f scanProbeFunc) Run(_ context.Context, _ string, args ...string) ([]byte, []byte, error) {
	return []byte(`{"streams":[]}`), nil, f(args[len(args)-1])
}
