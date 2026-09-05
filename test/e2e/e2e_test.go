//go:build e2e

package e2e

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"subsyncd/internal/app"
	"subsyncd/internal/catalog"
	"subsyncd/internal/config"
	"subsyncd/internal/domain"
	"subsyncd/internal/httpapi"
	"subsyncd/internal/store"
	"subsyncd/internal/syncer"
	"subsyncd/internal/worker"
	"subsyncd/internal/workflow"
)

func TestWebhookWakeDispatchesPersistedSearch(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	database, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "subsyncd.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	wake := make(chan struct{}, 1)
	notify := func() {
		select {
		case wake <- struct{}{}:
		default:
		}
	}
	catalogSource := wakeCatalog{now: now}
	handler := catalog.WebhookHandler{Instance: "radarr-main", InstanceType: "radarr", Catalog: catalogSource, Store: database.Repository(), Languages: []domain.Language{"en"}, Now: func() time.Time { return now }, OnApplied: notify}
	api := httpapi.Server{Instances: map[string]httpapi.Instance{"radarr-main": {Token: "webhook-key", Handler: handler}}}.Handler()
	service := newWakeWorkflow()
	background := &worker.Worker{Repository: database.Repository(), Workflow: service, Clock: fixedE2EClock{now: now}, Wake: wake, MaxWorkflows: 2, PollInterval: time.Hour, LeaseDuration: 5 * time.Minute, RenewInterval: time.Minute, ShutdownTimeout: time.Second}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- background.Run(ctx) }()

	postWebhook(t, api, `{"eventType":"Download","movieFile":{"id":1,"path":"/media/one.mkv","size":100}}`)
	first := waitForWakeWorkflow(t, service.started)
	postWebhook(t, api, `{"eventType":"Download","movieFile":{"id":2,"path":"/media/two.mkv","size":100}}`)
	second := waitForWakeWorkflow(t, service.started)
	if first == second {
		t.Fatalf("webhook dispatch IDs = %d/%d", first, second)
	}
	service.release(first)
	service.release(second)
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

type fixedE2EClock struct{ now time.Time }

func (c fixedE2EClock) Now() time.Time { return c.now }

type wakeCatalog struct{ now time.Time }

func (c wakeCatalog) GetMedia(_ context.Context, ref domain.MediaRef) (domain.Media, error) {
	return domain.Media{EntityID: ref.FileID, Ref: ref, Fingerprint: domain.MediaFingerprint{Path: fmt.Sprintf("/media/%d.mkv", ref.FileID), FileID: ref.FileID, Size: 100, ModTime: c.now}, Title: "Movie"}, nil
}

func (wakeCatalog) ListChanges(context.Context, time.Time, time.Time) ([]catalog.HistoryChange, error) {
	return nil, nil
}

type wakeWorkflow struct {
	mu       sync.Mutex
	started  chan int64
	releases map[int64]chan struct{}
}

func newWakeWorkflow() *wakeWorkflow {
	return &wakeWorkflow{started: make(chan int64, 2), releases: make(map[int64]chan struct{})}
}

func (w *wakeWorkflow) Run(ctx context.Context, request workflow.Request) (workflow.Result, error) {
	w.mu.Lock()
	release := make(chan struct{})
	w.releases[request.MediaID] = release
	w.mu.Unlock()
	w.started <- request.MediaID
	select {
	case <-release:
		return workflow.Result{Outcome: workflow.OutcomeSatisfied}, nil
	case <-ctx.Done():
		return workflow.Result{}, ctx.Err()
	}
}

func (w *wakeWorkflow) release(mediaID int64) {
	w.mu.Lock()
	release := w.releases[mediaID]
	w.mu.Unlock()
	close(release)
}

func waitForWakeWorkflow(t *testing.T, started <-chan int64) int64 {
	t.Helper()
	select {
	case mediaID := <-started:
		return mediaID
	case <-time.After(time.Second):
		t.Fatal("persisted webhook search did not start")
		return 0
	}
}

func TestWebhookToLapseInstallSiloAndRestartDeduplication(t *testing.T) {
	root := t.TempDir()
	mediaPath := filepath.Join(root, "Movie.2024.mkv")
	if err := os.WriteFile(mediaPath, make([]byte, 196608), 0o640); err != nil {
		t.Fatal(err)
	}

	arr := newArrServer(t, mediaPath)
	defer arr.Close()
	providerServer, providerCounts := newOpenSubtitlesServer(t, false)
	defer providerServer.Close()
	var siloCalls atomic.Int64
	silo := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != "/api/v1/scan" || request.Header.Get("Authorization") != "Bearer silo-key" {
			t.Errorf("unexpected Silo request: %s %s", request.Method, request.URL.Path)
			response.WriteHeader(http.StatusBadRequest)
			return
		}
		var payload struct {
			Path string `json:"path"`
		}
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil || payload.Path != filepath.Dir(mediaPath) {
			t.Errorf("unexpected Silo payload: %#v, %v", payload, err)
		}
		siloCalls.Add(1)
		response.WriteHeader(http.StatusAccepted)
	}))
	defer silo.Close()

	cfg := e2eConfig(t, root, arr.URL, providerServer.URL, silo.URL)
	// Keep this black-box path focused on LAPSE and exercise the compatibility
	// policy explicitly; score-bypass behavior is covered by workflow tests.
	cfg.Sync.Policy = "always"
	arrCatalog, err := catalog.NewRadarr("radarr-main", arr.URL, "arr-key", []config.PathMapping{{Remote: "/remote/movies", Local: root}}, []string{root}, nil)
	if err != nil {
		t.Fatal(err)
	}
	webhook := `{"eventType":"Download","isUpgrade":false,"movieFile":{"id":42,"path":"/remote/movies/Movie.2024.mkv","size":196608,"sceneName":"Movie.2024.1080p.WEB-DL-GROUP","releaseGroup":"GROUP"}}`
	countingRunner := &countingLapseRunner{}
	build := func() *app.App {
		application, err := app.New(context.Background(), cfg, app.Options{LapseRunner: countingRunner, ProbeRunner: probeRunner{}, HTTPClient: providerServer.Client(), Catalogs: map[string]catalog.Catalog{"radarr-main": arrCatalog}})
		if err != nil {
			t.Fatal(err)
		}
		return application
	}

	first := build()
	postWebhook(t, first.Handler, webhook)
	if err := first.Worker.(*worker.Worker).RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	sidecar := filepath.Join(root, "Movie.2024.en.srt")
	payload, err := os.ReadFile(sidecar)
	if err != nil || !strings.Contains(string(payload), "Hello from provider") {
		t.Fatalf("installed sidecar = %q, %v", payload, err)
	}
	if providerCounts.download.Load() != 1 || providerCounts.exact.Load() != 1 || providerCounts.broad.Load() != 1 || siloCalls.Load() != 1 {
		t.Fatalf("first run counts exact/broad/download/silo = %d/%d/%d/%d", providerCounts.exact.Load(), providerCounts.broad.Load(), providerCounts.download.Load(), siloCalls.Load())
	}
	if countingRunner.work.Load() != 2 {
		t.Fatalf("LAPSE work calls = %d, want one analysis and one synchronization", countingRunner.work.Load())
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	second := build()
	defer second.Close()
	postWebhook(t, second.Handler, webhook)
	if err := second.Worker.(*worker.Worker).RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if providerCounts.download.Load() != 1 || providerCounts.exact.Load() != 1 || providerCounts.broad.Load() != 1 || siloCalls.Load() != 1 {
		t.Fatalf("restart redownloaded or renotified: exact/broad/download/silo = %d/%d/%d/%d", providerCounts.exact.Load(), providerCounts.broad.Load(), providerCounts.download.Load(), siloCalls.Load())
	}
}

func TestManualAndDaemonInstallationsKeepDurableNotificationAcrossFailureAndRestart(t *testing.T) {
	for _, mode := range []string{"manual", "daemon"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			root := t.TempDir()
			mediaPath := filepath.Join(root, "Movie.2024.mkv")
			if err := os.WriteFile(mediaPath, make([]byte, 196608), 0o640); err != nil {
				t.Fatal(err)
			}
			arr := newArrServer(t, mediaPath)
			defer arr.Close()
			providerServer, providerCounts := newOpenSubtitlesServer(t, true)
			defer providerServer.Close()
			var siloCalls atomic.Int64
			var siloHealthy atomic.Bool
			silo := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				siloCalls.Add(1)
				if siloHealthy.Load() {
					response.WriteHeader(http.StatusAccepted)
				} else {
					response.WriteHeader(http.StatusServiceUnavailable)
				}
			}))
			defer silo.Close()
			cfg := e2eConfig(t, root, arr.URL, providerServer.URL, silo.URL)
			arrCatalog, err := catalog.NewRadarr("radarr-main", arr.URL, "arr-key", []config.PathMapping{{Remote: "/remote/movies", Local: root}}, []string{root}, nil)
			if err != nil {
				t.Fatal(err)
			}
			now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
			build := func() *app.App {
				application, err := app.New(ctx, cfg, app.Options{Clock: fixedE2EClock{now: now}, LapseRunner: &countingLapseRunner{}, ProbeRunner: probeRunner{}, HTTPClient: providerServer.Client(), Catalogs: map[string]catalog.Catalog{"radarr-main": arrCatalog}})
				if err != nil {
					t.Fatal(err)
				}
				return application
			}
			first := build()
			defer first.Close()
			audit, err := sql.Open("sqlite", filepath.Join(cfg.DataDir, "subsyncd.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer audit.Close()
			if mode == "manual" {
				if _, err := first.Search(ctx, "radarr-main", "movie", 42, "en", false); err != nil {
					t.Fatal(err)
				}
			} else {
				postWebhook(t, first.Handler, `{"eventType":"Download","movieFile":{"id":42,"path":"/remote/movies/Movie.2024.mkv","size":196608,"sceneName":"Movie.2024.1080p.WEB-DL-GROUP","releaseGroup":"GROUP"}}`)
				if err := first.Worker.(*worker.Worker).RunOnce(ctx); err != nil {
					t.Fatal(err)
				}
			}
			assertOutbox := func() {
				t.Helper()
				var installations, intents int
				if err := audit.QueryRow(`SELECT count(*) FROM installations`).Scan(&installations); err != nil {
					t.Fatal(err)
				}
				if err := audit.QueryRow(`SELECT count(*) FROM notifications WHERE notifier='silo' AND dedupe_key <> ''`).Scan(&intents); err != nil {
					t.Fatal(err)
				}
				if installations != 1 || intents != 1 {
					t.Fatalf("installation/intent counts = %d/%d", installations, intents)
				}
			}
			assertOutbox()
			sidecar := filepath.Join(root, "Movie.2024.en.srt")
			original, err := os.ReadFile(sidecar)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "manual" {
				if siloCalls.Load() != 0 {
					t.Fatal("manual installation attempted synchronous delivery")
				}
				if err := first.Worker.(*worker.Worker).RunOnce(ctx); err != nil {
					t.Fatal(err)
				}
			}
			if siloCalls.Load() != 1 {
				t.Fatalf("failed delivery calls = %d", siloCalls.Load())
			}
			var result string
			var retryAt int64
			if err := audit.QueryRow(`SELECT result, next_attempt_at_ns FROM notifications`).Scan(&result, &retryAt); err != nil || result != "retryable_error" || retryAt <= now.UnixNano() {
				t.Fatalf("delivery retry = %s/%d/%v", result, retryAt, err)
			}
			if payload, err := os.ReadFile(sidecar); err != nil || string(payload) != string(original) {
				t.Fatalf("delivery failure altered subtitle: %q/%v", payload, err)
			}
			assertOutbox()
			// A true same-checksum reinstall must preserve the existing retry row.
			if err := os.Remove(sidecar); err != nil {
				t.Fatal(err)
			}
			if _, err := first.Search(ctx, "radarr-main", "movie", 42, "en", false); err != nil {
				t.Fatal(err)
			}
			assertOutbox()
			if providerCounts.download.Load() != 2 {
				t.Fatalf("reacquisition downloads = %d", providerCounts.download.Load())
			}
			if err := first.Close(); err != nil {
				t.Fatal(err)
			}
			now = now.Add(time.Hour)
			siloHealthy.Store(true)
			second := build()
			defer second.Close()
			if err := second.Worker.(*worker.Worker).RunOnce(ctx); err != nil {
				t.Fatal(err)
			}
			if err := second.Worker.(*worker.Worker).RunOnce(ctx); err != nil {
				t.Fatal(err)
			}
			assertOutbox()
			if siloCalls.Load() != 2 || providerCounts.download.Load() != 2 {
				t.Fatalf("restart silo/download calls = %d/%d", siloCalls.Load(), providerCounts.download.Load())
			}
			if err := audit.QueryRow(`SELECT result, next_attempt_at_ns FROM notifications`).Scan(&result, &retryAt); err != nil || result != "success" || retryAt != 0 {
				t.Fatalf("delivered outbox = %s/%d/%v", result, retryAt, err)
			}
		})
	}
}

func TestEmbeddedSubtitlePreventsProviderAccess(t *testing.T) {
	root := t.TempDir()
	mediaPath := filepath.Join(root, "Movie.2024.mkv")
	if err := os.WriteFile(mediaPath, make([]byte, 196608), 0o640); err != nil {
		t.Fatal(err)
	}
	arr := newArrServer(t, mediaPath)
	defer arr.Close()
	providerServer, providerCounts := newOpenSubtitlesServer(t, false)
	defer providerServer.Close()
	cfg := e2eConfig(t, root, arr.URL, providerServer.URL, "")
	cfg.Silo.Enabled = false
	arrCatalog, err := catalog.NewRadarr("radarr-main", arr.URL, "arr-key", []config.PathMapping{{Remote: "/remote/movies", Local: root}}, []string{root}, nil)
	if err != nil {
		t.Fatal(err)
	}
	application, err := app.New(context.Background(), cfg, app.Options{LapseRunner: lapseRunner{}, ProbeRunner: embeddedProbeRunner{}, HTTPClient: providerServer.Client(), Catalogs: map[string]catalog.Catalog{"radarr-main": arrCatalog}})
	if err != nil {
		t.Fatal(err)
	}
	defer application.Close()
	postWebhook(t, application.Handler, `{"eventType":"Download","movieFile":{"id":42,"path":"/remote/movies/Movie.2024.mkv","size":196608}}`)
	if err := application.Worker.(*worker.Worker).RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if providerCounts.exact.Load()+providerCounts.broad.Load()+providerCounts.download.Load() != 0 {
		t.Fatalf("embedded subtitle triggered provider calls: %#v", providerCounts)
	}
	if _, err := os.Stat(filepath.Join(root, "Movie.2024.en.srt")); !os.IsNotExist(err) {
		t.Fatalf("embedded subtitle unexpectedly produced a sidecar: %v", err)
	}
}

func TestExactHashInstallsWithoutLapseOrBroadSearch(t *testing.T) {
	root := t.TempDir()
	mediaPath := filepath.Join(root, "Movie.2024.mkv")
	if err := os.WriteFile(mediaPath, make([]byte, 196608), 0o640); err != nil {
		t.Fatal(err)
	}
	arr := newArrServer(t, mediaPath)
	defer arr.Close()
	providerServer, providerCounts := newOpenSubtitlesServer(t, true)
	defer providerServer.Close()
	cfg := e2eConfig(t, root, arr.URL, providerServer.URL, "")
	cfg.Silo.Enabled = false
	arrCatalog, err := catalog.NewRadarr("radarr-main", arr.URL, "arr-key", []config.PathMapping{{Remote: "/remote/movies", Local: root}}, []string{root}, nil)
	if err != nil {
		t.Fatal(err)
	}
	runner := &countingLapseRunner{}
	application, err := app.New(context.Background(), cfg, app.Options{LapseRunner: runner, ProbeRunner: probeRunner{}, HTTPClient: providerServer.Client(), Catalogs: map[string]catalog.Catalog{"radarr-main": arrCatalog}})
	if err != nil {
		t.Fatal(err)
	}
	defer application.Close()
	postWebhook(t, application.Handler, `{"eventType":"Download","movieFile":{"id":42,"path":"/remote/movies/Movie.2024.mkv","size":196608,"sceneName":"Movie.2024.1080p.WEB-DL-GROUP","releaseGroup":"GROUP"}}`)
	if err := application.Worker.(*worker.Worker).RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if providerCounts.exact.Load() != 1 || providerCounts.broad.Load() != 0 || providerCounts.download.Load() != 1 || runner.work.Load() != 0 {
		t.Fatalf("exact/broad/download/lapse = %d/%d/%d/%d", providerCounts.exact.Load(), providerCounts.broad.Load(), providerCounts.download.Load(), runner.work.Load())
	}
	if _, err := os.Stat(filepath.Join(root, "Movie.2024.en.srt")); err != nil {
		t.Fatal(err)
	}
}

func TestSonarrReconciliationPersistsImportDeleteAndUnsupportedMultiEpisode(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	root := t.TempDir()
	database, err := store.Open(context.Background(), filepath.Join(root, "subsyncd.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	repository := database.Repository()
	auditDB, err := sql.Open("sqlite", filepath.Join(root, "subsyncd.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer auditDB.Close()
	if err := repository.EnsureInstance(context.Background(), "sonarr-main", "sonarr", "http://sonarr.invalid", now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}

	deletedRef := domain.MediaRef{Instance: "sonarr-main", Kind: domain.MediaEpisode, FileID: 1002}
	deletedMedia := domain.Media{
		EntityID:    102,
		Ref:         deletedRef,
		Fingerprint: domain.MediaFingerprint{Path: filepath.Join(root, "Deleted.S01E02.mkv"), FileID: 1002, Size: 100, ModTime: now.Add(-2 * time.Hour)},
		Title:       "Deleted",
		Season:      1,
		Episode:     2,
	}
	if _, err := repository.ApplyMediaEvent(context.Background(), store.MediaEventMutation{EventID: "seed:1002", Type: "import", EntityID: deletedMedia.EntityID, Ref: deletedRef, Media: deletedMedia, Languages: []domain.Language{"en"}, At: now.Add(-2 * time.Hour)}); err != nil {
		t.Fatal(err)
	}

	sonarr := newReconciliationSonarrServer(t, now)
	defer sonarr.Close()
	sonarrCatalog, err := catalog.NewSonarr("sonarr-main", sonarr.URL, "arr-key", []config.PathMapping{{Remote: "/remote/tv", Local: root}}, []string{root}, nil)
	if err != nil {
		t.Fatal(err)
	}
	reconciler := catalog.Reconciler{Instance: "sonarr-main", Catalog: sonarrCatalog, Store: repository, Languages: []domain.Language{"en"}, Now: func() time.Time { return now }}
	if err := reconciler.Run(context.Background()); err != nil {
		t.Fatal(err)
	}

	importID, _, err := repository.FindMedia(context.Background(), domain.MediaRef{Instance: "sonarr-main", Kind: domain.MediaEpisode, FileID: 1001})
	if err != nil {
		t.Fatal(err)
	}
	importStatus, err := repository.GetSearchStatus(context.Background(), importID, "en")
	if err != nil {
		t.Fatal(err)
	}
	if importStatus.State != "pending" || importStatus.Priority != store.SearchPriorityMissing || importStatus.NextAttemptAt.After(now) {
		t.Fatalf("import search status = %#v", importStatus)
	}

	deletedID, _, err := repository.FindMedia(context.Background(), deletedRef)
	if err != nil {
		t.Fatal(err)
	}
	deletedStatus, err := repository.GetSearchStatus(context.Background(), deletedID, "en")
	if err != nil {
		t.Fatal(err)
	}
	if deletedStatus.State != "complete" || deletedStatus.LastOutcome != "deleted" {
		t.Fatalf("deleted search status = %#v", deletedStatus)
	}

	unsupportedID, unsupported, err := repository.FindMedia(context.Background(), domain.MediaRef{Instance: "sonarr-main", Kind: domain.MediaEpisode, FileID: 1003})
	if err != nil {
		t.Fatal(err)
	}
	if unsupported.UnsupportedReason != domain.UnsupportedMultiEpisode || unsupported.Season != 1 || unsupported.Episode != 3 {
		t.Fatalf("unsupported media = %#v", unsupported)
	}
	unsupportedStatus, err := repository.GetSearchStatus(context.Background(), unsupportedID, "en")
	if err != nil {
		t.Fatal(err)
	}
	if unsupportedStatus.State != "complete" || unsupportedStatus.LastOutcome != string(domain.UnsupportedMultiEpisode) {
		t.Fatalf("unsupported search status = %#v", unsupportedStatus)
	}
	if _, _, found, err := repository.FindMediaByEntity(context.Background(), "sonarr-main", domain.MediaEpisode, 105); err != nil || found {
		t.Fatalf("outside-scope entity lookup = %v/%v, want false/nil", found, err)
	}
	var outsideFileID int64
	var outsideMediaID sql.NullInt64
	if err := auditDB.QueryRow(`SELECT file_id, media_id FROM events WHERE event_id='reconcile:sonarr-main:104'`).Scan(&outsideFileID, &outsideMediaID); err != nil {
		t.Fatal(err)
	}
	if outsideFileID != 0 || outsideMediaID.Valid {
		t.Fatalf("outside-scope audit = file:%d media:%#v", outsideFileID, outsideMediaID)
	}
	cursor, err := repository.GetReconciliationCursor(context.Background(), "sonarr-main")
	if err != nil {
		t.Fatal(err)
	}
	if !cursor.Equal(now) {
		t.Fatalf("reconciliation cursor = %s, want %s", cursor, now)
	}
	if err := reconciler.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	var reconciliationAudits int
	if err := auditDB.QueryRow(`SELECT count(*) FROM events WHERE event_id LIKE 'reconcile:sonarr-main:%'`).Scan(&reconciliationAudits); err != nil || reconciliationAudits != 4 {
		t.Fatalf("replayed reconciliation audits = %d/%v, want 4/nil", reconciliationAudits, err)
	}

	leases, err := repository.LeaseDueSearches(context.Background(), now, 10, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(leases) != 1 || leases[0].MediaID != importID {
		t.Fatalf("due leases = %#v, want only imported media %d", leases, importID)
	}
	if _, err := repository.CompleteSearch(context.Background(), store.SearchCompletion{JobID: leases[0].JobID, Outcome: "satisfied", ResetMissingAttempt: true, ResetFailureAttempt: true}); err != nil {
		t.Fatal(err)
	}
	workflowCalls := &countingSatisfiedWorkflow{}
	background := &worker.Worker{Repository: repository, Workflow: workflowCalls, Clock: fixedE2EClock{now: now}, MaxWorkflows: 1, LeaseDuration: time.Minute, RenewInterval: time.Second, ShutdownTimeout: time.Second}
	if err := background.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if workflowCalls.calls.Load() != 0 {
		t.Fatalf("unsupported reconciliation triggered %d acquisition workflows", workflowCalls.calls.Load())
	}
}

func newReconciliationSonarrServer(t *testing.T, now time.Time) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Header.Get("X-Api-Key") != "arr-key" {
			response.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch request.URL.Path {
		case "/api/v3/history/since":
			_ = json.NewEncoder(response).Encode([]map[string]any{
				{"id": 101, "seriesId": 11, "episodeId": 101, "eventType": "downloadFolderImported", "date": now.Add(-30 * time.Minute), "data": map[string]string{"fileId": "1001"}},
				{"id": 102, "seriesId": 12, "episodeId": 102, "eventType": "episodeFileDeleted", "date": now.Add(-20 * time.Minute), "data": map[string]string{}},
				{"id": 103, "seriesId": 13, "episodeId": 103, "eventType": "downloadFolderImported", "date": now.Add(-10 * time.Minute), "data": map[string]string{"fileId": "1003"}},
				{"id": 104, "seriesId": 15, "episodeId": 105, "eventType": "downloadFolderImported", "date": now.Add(-5 * time.Minute), "data": map[string]string{"fileId": "1005"}},
			})
		case "/api/v3/episode/101":
			_ = json.NewEncoder(response).Encode(map[string]any{"id": 101, "seriesId": 11, "hasFile": true, "episodeFile": map[string]any{"id": 1001, "seriesId": 11, "path": "/remote/tv/Show.S01E01.mkv"}})
		case "/api/v3/episode/102":
			_ = json.NewEncoder(response).Encode(map[string]any{"id": 102, "seriesId": 12, "hasFile": false})
		case "/api/v3/episode/103":
			_ = json.NewEncoder(response).Encode(map[string]any{"id": 103, "seriesId": 13, "hasFile": true, "episodeFile": map[string]any{"id": 1003, "seriesId": 13, "path": "/remote/tv/Combined.S01E03E04.mkv"}})
		case "/api/v3/episode/105":
			_ = json.NewEncoder(response).Encode(map[string]any{"id": 105, "seriesId": 15, "hasFile": true, "episodeFile": map[string]any{"id": 1005, "seriesId": 15, "path": "/outside/Other.S01E01.mkv"}})
		case "/api/v3/episodefile/1001":
			_ = json.NewEncoder(response).Encode(map[string]any{"id": 1001, "seriesId": 11, "path": "/remote/tv/Show.S01E01.mkv", "size": 100, "dateAdded": now.Add(-30 * time.Minute)})
		case "/api/v3/episodefile/1003":
			_ = json.NewEncoder(response).Encode(map[string]any{"id": 1003, "seriesId": 13, "path": "/remote/tv/Combined.S01E03E04.mkv", "size": 300, "dateAdded": now.Add(-10 * time.Minute)})
		case "/api/v3/episode":
			switch request.URL.Query().Get("episodeFileId") {
			case "1001":
				_ = json.NewEncoder(response).Encode([]map[string]any{{"id": 101, "seriesId": 11, "seasonNumber": 1, "episodeNumber": 1, "absoluteEpisodeNumber": 1, "title": "Pilot"}})
			case "1003":
				_ = json.NewEncoder(response).Encode([]map[string]any{
					{"id": 104, "seriesId": 13, "seasonNumber": 1, "episodeNumber": 4, "absoluteEpisodeNumber": 4, "title": "Fourth"},
					{"id": 103, "seriesId": 13, "seasonNumber": 1, "episodeNumber": 3, "absoluteEpisodeNumber": 3, "title": "Third"},
				})
			default:
				http.NotFound(response, request)
			}
		case "/api/v3/series/11":
			_, _ = io.WriteString(response, `{"id":11,"title":"Show","year":2026,"tvdbId":11}`)
		case "/api/v3/series/13":
			_, _ = io.WriteString(response, `{"id":13,"title":"Combined","year":2026,"tvdbId":13}`)
		default:
			http.NotFound(response, request)
		}
	}))
}

type countingSatisfiedWorkflow struct{ calls atomic.Int64 }

func (w *countingSatisfiedWorkflow) Run(context.Context, workflow.Request) (workflow.Result, error) {
	w.calls.Add(1)
	return workflow.Result{Outcome: workflow.OutcomeSatisfied}, nil
}

type counts struct {
	exact    atomic.Int64
	broad    atomic.Int64
	download atomic.Int64
}

func newOpenSubtitlesServer(t *testing.T, exactHit bool) (*httptest.Server, *counts) {
	t.Helper()
	calls := &counts{}
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api/v1/login":
			_, _ = io.WriteString(response, `{"token":"token","expires_in":3600}`)
		case "/api/v1/subtitles":
			if request.URL.Query().Get("moviehash") != "" {
				calls.exact.Add(1)
				if exactHit {
					_, _ = io.WriteString(response, `{"total_pages":1,"data":[{"id":"subtitle-1","attributes":{"language":"en","hearing_impaired":false,"ratings":9,"download_count":1000,"release":"Movie.2024.1080p.WEB-DL-GROUP","moviehash_match":true,"feature_details":{"movie_name":"Movie","year":2024,"imdb_id":1234567,"tmdb_id":9},"files":[{"file_id":501,"file_name":"Movie.2024.en.srt"}]}}]}`)
					return
				}
				_, _ = io.WriteString(response, `{"total_pages":1,"data":[]}`)
				return
			}
			calls.broad.Add(1)
			_, _ = io.WriteString(response, `{"total_pages":1,"data":[{"id":"subtitle-1","attributes":{"language":"en","hearing_impaired":false,"ratings":9,"download_count":1000,"release":"Movie.2024.1080p.WEB-DL-GROUP","moviehash_match":false,"feature_details":{"movie_name":"Movie","year":2024,"imdb_id":1234567,"tmdb_id":9},"files":[{"file_id":501,"file_name":"Movie.2024.en.srt"}]}}]}`)
		case "/api/v1/download":
			calls.download.Add(1)
			_ = json.NewEncoder(response).Encode(map[string]any{"link": server.URL + "/subtitle.srt", "file_name": "Movie.2024.en.srt"})
		case "/subtitle.srt":
			payload, err := os.ReadFile(filepath.Join("fixtures", "candidate.srt"))
			if err != nil {
				t.Error(err)
				response.WriteHeader(http.StatusInternalServerError)
				return
			}
			_, _ = response.Write(payload)
		default:
			http.NotFound(response, request)
		}
	}))
	return server, calls
}

func newArrServer(t *testing.T, mediaPath string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Header.Get("X-Api-Key") != "arr-key" {
			response.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch request.URL.Path {
		case "/api/v3/moviefile/42":
			_ = json.NewEncoder(response).Encode(map[string]any{"id": 42, "movieId": 9, "path": "/remote/movies/" + filepath.Base(mediaPath), "size": 196608, "dateAdded": time.Now().UTC(), "sceneName": "Movie.2024.1080p.WEB-DL-GROUP", "releaseGroup": "GROUP", "quality": map[string]any{"quality": map[string]any{"name": "WEBDL-1080p", "resolution": 1080, "source": "WEB-DL"}}, "mediaInfo": map[string]any{"runTime": "01:30:00"}})
		case "/api/v3/movie/9":
			_, _ = io.WriteString(response, `{"id":9,"title":"Movie","year":2024,"imdbId":"tt1234567","tmdbId":9}`)
		case "/api/v3/history/since":
			_, _ = io.WriteString(response, `[]`)
		default:
			http.NotFound(response, request)
		}
	}))
}

func e2eConfig(t *testing.T, root, arrURL, providerURL, siloURL string) config.Config {
	t.Helper()
	var providerNode yaml.Node
	settings := fmt.Sprintf("type: opensubtitles\napi_key: provider-key\nusername: user\npassword: pass\nuser_agent: subsyncd-e2e\nbase_url: %s/api/v1\nrequests_per_second: 100\nburst: 10\nmax_concurrent: 1\n", providerURL)
	if err := yaml.Unmarshal([]byte(settings), &providerNode); err != nil {
		t.Fatal(err)
	}
	return config.Config{
		DataDir: filepath.Join(root, "data"), MediaRoots: []string{root}, Server: config.ServerConfig{Listen: "127.0.0.1:0"},
		Worker:    config.WorkerConfig{MaxConcurrent: 1},
		Instances: []config.InstanceConfig{{Name: "radarr-main", Type: "radarr", URL: arrURL, APIKey: "arr-key", WebhookToken: "webhook-key", PathMappings: []config.PathMapping{{Remote: "/remote/movies", Local: root}}}},
		Providers: map[string]config.ProviderSpec{"opensubtitles-main": {Type: "opensubtitles", RequestsPerSecond: 100, Burst: 10, MaxConcurrent: 1, Settings: *providerNode.Content[0]}},
		Languages: map[domain.Language]config.LanguageConfig{"en": {Providers: []string{"opensubtitles-main"}}}, AllowHearingImpaired: true, MinimumReleaseScore: 35,
		ProviderHTTP: config.ProviderHTTPConfig{SharedOriginMaxConcurrent: 1}, PackCache: config.PackCacheConfig{TTL: 24 * time.Hour, MaxBytes: 16 << 20},
		Sync: config.SyncConfig{LapsePath: "/fake/lapse", Timeout: time.Minute, Policy: "confidence", BypassScore: 75, RequireIdentityAnchor: true, RequireEpisodeEvidence: true, RequireReleaseGroup: true, LapseForPacks: true, LapseForUpgrades: true}, Install: config.InstallConfig{FileMode: 0o640},
		Silo: config.SiloConfig{Enabled: true, URL: siloURL, APIKey: "silo-key"},
	}
}

func postWebhook(t *testing.T, handler http.Handler, body string) {
	t.Helper()
	server := httptest.NewServer(handler)
	defer server.Close()
	request, err := http.NewRequest(http.MethodPost, server.URL+"/webhooks/radarr-main?token=webhook-key", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		payload, _ := io.ReadAll(response.Body)
		t.Fatalf("webhook status = %d: %s", response.StatusCode, payload)
	}
}

type probeRunner struct{}

func (probeRunner) Run(_ context.Context, _ string, args ...string) ([]byte, []byte, error) {
	if len(args) == 1 && args[0] == "-version" {
		return []byte("ffprobe version e2e"), nil, nil
	}
	return []byte(`{"streams":[],"format":{}}`), nil, nil
}

type embeddedProbeRunner struct{}

func (embeddedProbeRunner) Run(_ context.Context, _ string, args ...string) ([]byte, []byte, error) {
	if len(args) == 1 && args[0] == "-version" {
		return []byte("ffprobe version e2e"), nil, nil
	}
	return []byte(`{"streams":[{"index":0,"codec_name":"subrip","codec_type":"subtitle","disposition":{"forced":0},"tags":{"language":"en","title":"English"}}],"format":{}}`), nil, nil
}

type lapseRunner struct{}

func (lapseRunner) Run(_ context.Context, command syncer.Command) (syncer.Execution, error) {
	if len(command.Args) == 0 {
		return syncer.Execution{ExitCode: 255, Stderr: []byte("--json --strict --output --no-sidecar --no-cache")}, nil
	}
	output := command.Args[1]
	written := false
	for index, argument := range command.Args {
		if argument == "--output" && index+1 < len(command.Args) {
			output = command.Args[index+1]
			payload, err := os.ReadFile(command.Args[1])
			if err != nil {
				return syncer.Execution{}, err
			}
			if err := os.WriteFile(output, payload, 0o600); err != nil {
				return syncer.Execution{}, err
			}
			written = true
		}
	}
	report, _ := json.Marshal(map[string]any{"mode": "auto/shifted", "reference": "vad", "offset_ms": 25, "ratio": 1, "confidence": 0.9, "margin": 0.2, "sigma": 2, "agreement": 0.9, "verdict": "solid", "coverage": 1, "cues": 1, "ignored_cues": 0, "parts": 1, "written": written || contains(command.Args, "--dry-run"), "output": output, "splits": []any{}})
	return syncer.Execution{Stdout: report}, nil
}

type countingLapseRunner struct {
	work atomic.Int64
}

func (r *countingLapseRunner) Run(ctx context.Context, command syncer.Command) (syncer.Execution, error) {
	if len(command.Args) > 0 {
		r.work.Add(1)
	}
	return lapseRunner{}.Run(ctx, command)
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
