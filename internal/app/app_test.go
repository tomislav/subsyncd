package app

import (
	"context"
	"errors"
	"io"
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
func (fakeCatalog) ListMediaChangedSince(context.Context, time.Time) ([]domain.Media, error) {
	return nil, nil
}

type staticCatalog struct{ media domain.Media }

func (c staticCatalog) GetMedia(context.Context, domain.MediaRef) (domain.Media, error) {
	return c.media, nil
}
func (staticCatalog) ListMediaChangedSince(context.Context, time.Time) ([]domain.Media, error) {
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

func TestExplainListsActiveCandidateRejections(t *testing.T) {
	cfg := testConfig(t)
	application, err := New(context.Background(), cfg, Options{LapseRunner: capabilityRunner{}, ProbeRunner: probeRunner{}, Providers: map[string]provider.Provider{"english": fakeProvider{id: "english"}}, Catalogs: map[string]catalog.Catalog{"tv": fakeCatalog{}}, Worker: &waitingWorker{}})
	if err != nil {
		t.Fatal(err)
	}
	defer application.Close()
	now := application.Clock.Now()
	media := domain.Media{Ref: domain.MediaRef{Instance: "tv", Kind: domain.MediaMovie, FileID: 7}, Fingerprint: domain.MediaFingerprint{Path: filepath.Join(cfg.MediaRoots[0], "Movie.mkv"), FileID: 7, Size: 100, ModTime: now}, Title: "Movie", Year: 2024, ExternalIDs: domain.ExternalIDs{TMDB: 7}}
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
	media := domain.Media{Ref: domain.MediaRef{Instance: "tv", Kind: domain.MediaMovie, FileID: 7}, Fingerprint: domain.MediaFingerprint{Path: mediaPath, FileID: 7, Size: info.Size(), ModTime: info.ModTime()}, Title: "Movie", Year: 2024, ExternalIDs: domain.ExternalIDs{TMDB: 7}}
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
	application := &App{Config: cfg, Listener: listener, Handler: httpapi.Server{}.Handler(), Worker: worker}
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
