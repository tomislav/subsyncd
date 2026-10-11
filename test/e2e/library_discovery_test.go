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
	"time"

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
			var enumerations atomic.Int64
			var hydrations atomic.Int64
			arr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("X-Api-Key") != "arr-key" {
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
				switch r.URL.Path {
				case "/api/v3/history":
					io.WriteString(w, `{"records":[],"totalRecords":0,"page":1,"pageSize":100}`)
				case "/api/v3/movie":
					enumerations.Add(1)
					io.WriteString(w, `[{"id":9,"hasFile":true,"movieFile":{"id":42,"path":"/remote/library/Movie.2024.mkv"}}]`)
				case "/api/v3/moviefile/42":
					hydrations.Add(1)
					io.WriteString(w, `{"id":42,"movieId":9,"path":"/remote/library/Movie.2024.mkv","size":196608,"dateAdded":"2026-09-08T10:00:00Z"}`)
				case "/api/v3/movie/9":
					io.WriteString(w, `{"id":9,"title":"Movie","year":2024,"imdbId":"tt1234567","tmdbId":9}`)
				case "/api/v3/series":
					enumerations.Add(1)
					io.WriteString(w, `[{"id":10,"title":"Show"}]`)
				case "/api/v3/episodefile":
					io.WriteString(w, `[{"id":42,"seriesId":10,"path":"/remote/library/Show.S01E02.mkv"}]`)
				case "/api/v3/episodefile/42":
					hydrations.Add(1)
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
			now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
			build := func() *app.App {
				a, err := app.New(context.Background(), cfg, app.Options{Clock: fixedE2EClock{now: now}, LapseRunner: &countingLapseRunner{}, ProbeRunner: probeRunner{}, HTTPClient: provider.Client()})
				if err != nil {
					t.Fatal(err)
				}
				return a
			}
			a := build()
			defer a.Close()
			if enumerations.Load() != 0 || hydrations.Load() != 0 {
				t.Fatal("constructor enumerated Arr library")
			}
			if err := a.Worker.(*worker.Worker).RunOnce(context.Background()); err != nil {
				t.Fatal(err)
			}
			if enumerations.Load() != 2 || hydrations.Load() != 1 {
				t.Fatalf("initial discovery/snapshot calls = %d/%d, want 2/1", enumerations.Load(), hydrations.Load())
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
			// Reconciliation timing is saved per instance, so a restart soon
			// after a reconciliation keeps its six-hour schedule instead of
			// enumerating the Arr library again (docs/development/operations.md).
			now = now.Add(time.Minute)
			b := build()
			defer b.Close()
			if err := b.Worker.(*worker.Worker).RunOnce(context.Background()); err != nil {
				t.Fatal(err)
			}
			if enumerations.Load() != 2 || hydrations.Load() != 1 || calls.download.Load() != 1 {
				t.Fatalf("early restart enumeration/hydration/download calls = %d/%d/%d, want 2/1/1", enumerations.Load(), hydrations.Load(), calls.download.Load())
			}
			if err := b.Close(); err != nil {
				t.Fatal(err)
			}
			// Once the interval has passed, a restart reconciles at once:
			// history and the identity snapshot, without another discovery.
			now = now.Add(6 * time.Hour)
			c := build()
			defer c.Close()
			if err := c.Worker.(*worker.Worker).RunOnce(context.Background()); err != nil {
				t.Fatal(err)
			}
			if enumerations.Load() != 3 || hydrations.Load() != 1 || calls.download.Load() != 1 {
				t.Fatalf("due restart enumeration/hydration/download calls = %d/%d/%d, want 3/1/1", enumerations.Load(), hydrations.Load(), calls.download.Load())
			}
			if err := c.Close(); err != nil {
				t.Fatal(err)
			}
			// A changed discovery scope (here another media root; also a
			// scope version bump) runs its pass at the next start, inside
			// the saved interval: discovery plus the identity snapshot.
			now = now.Add(time.Minute)
			cfg.MediaRoots = append(cfg.MediaRoots, t.TempDir())
			d := build()
			defer d.Close()
			if err := d.Worker.(*worker.Worker).RunOnce(context.Background()); err != nil {
				t.Fatal(err)
			}
			if enumerations.Load() != 5 || calls.download.Load() != 1 {
				t.Fatalf("scope-change restart enumeration/download calls = %d/%d, want 5/1", enumerations.Load(), calls.download.Load())
			}
		})
	}
}
