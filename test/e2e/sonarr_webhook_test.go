//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"subsyncd/internal/app"
	"subsyncd/internal/config"
	"subsyncd/internal/domain"
	"subsyncd/internal/worker"
)

func TestSonarrSingleFileWebhookInstallsAndDeduplicatesAfterRestart(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "Show.S02E02.mkv"), make([]byte, 196608), 0600); err != nil {
		t.Fatal(err)
	}
	var hydration atomic.Int64
	arr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-Key") != "arr-key" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/api/v3/series":
			io.WriteString(w, `[{"id":10,"title":"Show"}]`)
		case "/api/v3/episodefile":
			io.WriteString(w, `[{"id":42,"seriesId":10,"path":"/remote/tv/Show.S02E02.mkv"}]`)
		case "/api/v3/episodefile/42":
			hydration.Add(1)
			io.WriteString(w, `{"id":42,"seriesId":10,"path":"/remote/tv/Show.S02E02.mkv","size":196608,"dateAdded":"2026-09-08T10:00:00Z","sceneName":"Show.S02E02.1080p.WEB-DL-GROUP","releaseGroup":"GROUP"}`)
		case "/api/v3/episode":
			if r.URL.Query().Get("episodeFileId") != "42" {
				t.Errorf("unexpected episode query: %s", r.URL.RawQuery)
			}
			io.WriteString(w, `[{"id":102,"seriesId":10,"seasonNumber":2,"episodeNumber":2,"absoluteEpisodeNumber":14,"title":"The Return"}]`)
		case "/api/v3/history":
			io.WriteString(w, `{"records":[],"totalRecords":0,"page":1,"pageSize":100}`)
		case "/api/v3/series/10":
			io.WriteString(w, `{"id":10,"title":"Show","year":2024,"imdbId":"tt1234567","tvdbId":12345}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer arr.Close()
	var downloads atomic.Int64
	var provider *httptest.Server
	provider = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/login":
			io.WriteString(w, `{"token":"token","expires_in":3600}`)
		case "/api/v1/subtitles":
			if r.URL.Query().Get("moviehash") != "" {
				io.WriteString(w, `{"total_pages":1,"data":[]}`)
				return
			}
			if r.URL.Query().Get("season_number") != "2" || r.URL.Query().Get("episode_number") != "2" || r.URL.Query().Get("parent_imdb_id") != "1234567" {
				t.Errorf("unexpected episode search: %s", r.URL.RawQuery)
			}
			io.WriteString(w, `{"total_pages":1,"data":[{"id":"sub-1","attributes":{"language":"en","release":"Show.S02E02.1080p.WEB-DL-GROUP","feature_details":{"title":"Show","parent_imdb_id":1234567,"season_number":2,"episode_number":2},"files":[{"file_id":501,"file_name":"Show.S02E02.srt"}]}}]}`)
		case "/api/v1/download":
			downloads.Add(1)
			json.NewEncoder(w).Encode(map[string]any{"link": provider.URL + "/subtitle.srt", "file_name": "Show.S02E02.srt"})
		case "/subtitle.srt":
			payload, err := os.ReadFile(filepath.Join("fixtures", "candidate.srt"))
			if err != nil {
				t.Error(err)
				w.WriteHeader(500)
				return
			}
			w.Write(payload)
		default:
			http.NotFound(w, r)
		}
	}))
	defer provider.Close()
	cfg := e2eConfig(t, root, arr.URL, provider.URL, "")
	cfg.Instances = []config.InstanceConfig{{Name: "sonarr-main", Type: "sonarr", URL: arr.URL, APIKey: "arr-key", WebhookToken: "webhook-key", PathMappings: []config.PathMapping{{Remote: "/remote/tv", Local: root}}}}
	cfg.Silo.Enabled = false
	cfg.Sync.Policy = "always"
	runner := &countingLapseRunner{}
	build := func() *app.App {
		a, err := app.New(context.Background(), cfg, app.Options{LapseRunner: runner, ProbeRunner: probeRunner{}, HTTPClient: provider.Client()})
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	post := func(a *app.App) {
		req := httptest.NewRequest(http.MethodPost, "/webhooks/sonarr-main?token=webhook-key", strings.NewReader(`{"eventType":"Download","isUpgrade":true,"series":{"id":10},"episodes":[{"id":102,"seasonNumber":2,"episodeNumber":2}],"episodeFile":{"id":42,"path":"/remote/tv/Show.S02E02.mkv","size":196608,"sceneName":"Show.S02E02.1080p.WEB-DL-GROUP","quality":"WEBDL-1080p"}}`))
		response := httptest.NewRecorder()
		a.Handler.ServeHTTP(response, req)
		if response.Code != http.StatusNoContent {
			t.Fatalf("webhook status = %d: %s", response.Code, response.Body.String())
		}
	}
	first := build()
	defer first.Close()
	post(first)
	select {
	case <-first.Worker.(*worker.Worker).Wake:
	default:
		t.Fatal("Sonarr import did not wake worker")
	}
	id, media, err := first.Repository.FindMedia(context.Background(), domain.MediaRef{Instance: "sonarr-main", Kind: domain.MediaEpisode, FileID: 42})
	if err != nil || media.EntityID != 102 || media.SeriesID != 10 || media.AbsoluteEpisode != 14 {
		t.Fatalf("hydrated media = %#v, %v", media, err)
	}
	status, err := first.Repository.GetSearchStatus(context.Background(), id, "en")
	if err != nil || status.Priority != 300 || status.State != "pending" {
		t.Fatalf("search status = %#v, %v", status, err)
	}
	if err := first.Worker.(*worker.Worker).RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	payload, err := os.ReadFile(filepath.Join(root, "Show.S02E02.en.srt"))
	if err != nil || !strings.Contains(string(payload), "Hello from provider") {
		t.Fatalf("sidecar = %q, %v", payload, err)
	}
	if downloads.Load() != 1 || runner.work.Load() != 1 {
		t.Fatalf("downloads/LAPSE = %d/%d", downloads.Load(), runner.work.Load())
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	hydrationBeforeRestart := hydration.Load()
	second := build()
	defer second.Close()
	post(second)
	select {
	case <-second.Worker.(*worker.Worker).Wake:
		t.Fatal("redelivery woke worker")
	default:
	}
	if err := second.Worker.(*worker.Worker).RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if hydration.Load() != hydrationBeforeRestart || downloads.Load() != 1 || runner.work.Load() != 1 {
		t.Fatalf("restart repeated work: hydration/downloads/LAPSE = %d/%d/%d", hydration.Load(), downloads.Load(), runner.work.Load())
	}
}
