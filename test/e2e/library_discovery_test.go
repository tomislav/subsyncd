//go:build e2e

package e2e

import (
	"context"
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

func TestExistingArrLibraryInstallsWithoutHistoryOrWebhooks(t *testing.T) {
	for _, kind := range []string{"sonarr", "radarr"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			filename := "Movie.2024.mkv"
			mediaKind := domain.MediaMovie
			if kind == "sonarr" {
				filename = "Show.S01E02.mkv"
				mediaKind = domain.MediaEpisode
			}
			if err := os.WriteFile(filepath.Join(root, filename), make([]byte, 196608), 0600); err != nil {
				t.Fatal(err)
			}
			var listings atomic.Int64
			arr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("X-Api-Key") != "arr-key" {
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
				switch r.URL.Path {
				case "/api/v3/history":
					io.WriteString(w, `{"records":[],"totalRecords":0,"page":1,"pageSize":100}`)
				case "/api/v3/movie":
					listings.Add(1)
					io.WriteString(w, `[{"id":9,"hasFile":true,"movieFile":{"id":42,"path":"/remote/library/Movie.2024.mkv"}}]`)
				case "/api/v3/moviefile/42":
					io.WriteString(w, `{"id":42,"movieId":9,"path":"/remote/library/Movie.2024.mkv","size":196608,"dateAdded":"2026-09-08T10:00:00Z"}`)
				case "/api/v3/movie/9":
					io.WriteString(w, `{"id":9,"title":"Movie","year":2024,"imdbId":"tt1234567","tmdbId":9}`)
				case "/api/v3/series":
					listings.Add(1)
					io.WriteString(w, `[{"id":10,"title":"Show"}]`)
				case "/api/v3/episodefile":
					io.WriteString(w, `[{"id":42,"seriesId":10,"path":"/remote/library/Show.S01E02.mkv"}]`)
				case "/api/v3/episodefile/42":
					io.WriteString(w, `{"id":42,"seriesId":10,"path":"/remote/library/Show.S01E02.mkv","size":196608,"dateAdded":"2026-09-08T10:00:00Z"}`)
				case "/api/v3/episode":
					io.WriteString(w, `[{"id":102,"seriesId":10,"seasonNumber":1,"episodeNumber":2,"title":"An Episode"}]`)
				case "/api/v3/series/10":
					io.WriteString(w, `{"id":10,"title":"Show","year":2024,"imdbId":"tt1234567","tvdbId":12345}`)
				default:
					http.NotFound(w, r)
				}
			}))
			defer arr.Close()
			provider, calls := newOpenSubtitlesServer(t, true)
			defer provider.Close()
			cfg := e2eConfig(t, root, arr.URL, provider.URL, "")
			cfg.Silo.Enabled = false
			cfg.Instances = []config.InstanceConfig{{Name: "library", Type: kind, URL: arr.URL, APIKey: "arr-key", WebhookToken: "webhook-key", PathMappings: []config.PathMapping{{Remote: "/remote/library", Local: root}}}}
			build := func() *app.App {
				a, err := app.New(context.Background(), cfg, app.Options{LapseRunner: &countingLapseRunner{}, ProbeRunner: probeRunner{}, HTTPClient: provider.Client()})
				if err != nil {
					t.Fatal(err)
				}
				return a
			}
			a := build()
			defer a.Close()
			if listings.Load() != 0 {
				t.Fatal("constructor enumerated Arr library")
			}
			if err := a.Worker.(*worker.Worker).RunOnce(context.Background()); err != nil {
				t.Fatal(err)
			}
			id, _, err := a.Repository.FindMedia(context.Background(), domain.MediaRef{Instance: "library", Kind: mediaKind, FileID: 42})
			if err != nil {
				t.Fatal(err)
			}
			installed, ok, err := a.Repository.GetInstallation(context.Background(), id, "en")
			if err != nil || !ok {
				t.Fatalf("installation = %#v, %v, %v", installed, ok, err)
			}
			contents, err := os.ReadFile(filepath.Join(root, strings.TrimSuffix(filename, ".mkv")+".en.srt"))
			if err != nil || !strings.Contains(string(contents), "Hello from provider") {
				t.Fatalf("subtitle = %q, %v", contents, err)
			}
			if err := a.Close(); err != nil {
				t.Fatal(err)
			}
			b := build()
			defer b.Close()
			if err := b.Worker.(*worker.Worker).RunOnce(context.Background()); err != nil {
				t.Fatal(err)
			}
			if listings.Load() != 1 || calls.download.Load() != 1 {
				t.Fatalf("restart repeated discovery/download: %d/%d", listings.Load(), calls.download.Load())
			}
		})
	}
}
