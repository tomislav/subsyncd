//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"subsyncd/internal/app"
	"subsyncd/internal/catalog"
	"subsyncd/internal/config"
	"subsyncd/internal/domain"
	"subsyncd/internal/syncer"
	"subsyncd/internal/worker"
)

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
		if request.Method != http.MethodPost || request.URL.Path != "/Library/Media/Updated" || request.Header.Get("X-Emby-Token") != "silo-key" {
			t.Errorf("unexpected Silo request: %s %s", request.Method, request.URL.Path)
			response.WriteHeader(http.StatusBadRequest)
			return
		}
		var payload struct {
			Updates []struct {
				Path       string `json:"path"`
				UpdateType string `json:"updateType"`
			} `json:"Updates"`
		}
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil || len(payload.Updates) != 1 || payload.Updates[0].Path != mediaPath || payload.Updates[0].UpdateType != "Modified" {
			t.Errorf("unexpected Silo payload: %#v, %v", payload, err)
		}
		siloCalls.Add(1)
		response.WriteHeader(http.StatusNoContent)
	}))
	defer silo.Close()

	cfg := e2eConfig(t, root, arr.URL, providerServer.URL, silo.URL)
	// Keep this black-box path focused on LAPSE and exercise the compatibility
	// policy explicitly; score-bypass behavior is covered by workflow tests.
	cfg.Sync.Policy = "always"
	arrCatalog, err := catalog.NewRadarr("radarr-main", arr.URL, "arr-key", []config.PathMapping{{Remote: "/remote/movies", Local: root}}, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	build := func() *app.App {
		application, err := app.New(context.Background(), cfg, app.Options{LapseRunner: lapseRunner{}, ProbeRunner: probeRunner{}, HTTPClient: providerServer.Client(), Catalogs: map[string]catalog.Catalog{"radarr-main": arrCatalog}})
		if err != nil {
			t.Fatal(err)
		}
		return application
	}

	webhook := `{"eventType":"Download","isUpgrade":false,"movieFile":{"id":42,"path":"/remote/movies/Movie.2024.mkv","size":196608,"sceneName":"Movie.2024.1080p.WEB-DL-GROUP","releaseGroup":"GROUP"}}`
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
	arrCatalog, err := catalog.NewRadarr("radarr-main", arr.URL, "arr-key", []config.PathMapping{{Remote: "/remote/movies", Local: root}}, []string{root})
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
	arrCatalog, err := catalog.NewRadarr("radarr-main", arr.URL, "arr-key", []config.PathMapping{{Remote: "/remote/movies", Local: root}}, []string{root})
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
