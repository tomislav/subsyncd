package catalog

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"subsyncd/internal/config"
	"subsyncd/internal/domain"
)

func TestRadarrGetMediaHydratesFileAndMovie(t *testing.T) {
	root := t.TempDir()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v3/moviefile/2001":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 2001, "movieId": 20, "path": "/remote/movies/Example Movie/Example.Movie.2024.mkv", "size": 4321, "dateAdded": "2026-09-04T11:00:00Z", "sceneName": "Example.Movie.2024.2160p.WEB-DL-GROUP", "releaseGroup": "GROUP", "edition": "Extended", "quality": map[string]any{"quality": map[string]any{"name": "WEBDL-2160p", "resolution": 2160, "source": "webdl"}}, "mediaInfo": map[string]any{"runTime": "02:03:04"}})
		case "/api/v3/movie/20":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 20, "title": "Example Movie", "alternateTitles": []map[string]any{{"title": "Primjer filma"}}, "year": 2024, "imdbId": "tt7654321", "tmdbId": 2468})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	catalog, err := NewRadarr("radarr-main", server.URL, "secret", []config.PathMapping{{Remote: "/remote/movies", Local: root}}, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	media, err := catalog.GetMedia(context.Background(), domain.MediaRef{Instance: "radarr-main", Kind: domain.MediaMovie, FileID: 2001})
	if err != nil {
		t.Fatal(err)
	}
	if media.Title != "Example Movie" || media.Year != 2024 || media.ExternalIDs.TMDB != 2468 || media.ExternalIDs.IMDb != "tt7654321" {
		t.Fatalf("movie identity = %#v", media)
	}
	if media.Fingerprint.Path != filepath.Join(root, "Example Movie", "Example.Movie.2024.mkv") || media.Fingerprint.Size != 4321 {
		t.Fatalf("fingerprint = %#v", media.Fingerprint)
	}
	if media.OriginalFilename == "" || media.ReleaseGroup != "GROUP" || media.Edition != "Extended" || media.Resolution != "2160p" || media.Source != "webdl" || media.Duration != 2*time.Hour+3*time.Minute+4*time.Second {
		t.Fatalf("release metadata = %#v", media)
	}
}

func TestRadarrListChangesSinceDeleteThenImportHydratesFinalState(t *testing.T) {
	root := t.TempDir()
	fileRequests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v3/history/since":
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"id": 30, "eventType": "movieFileDeleted", "date": "2026-09-04T10:00:00Z", "movieFileId": 2001},
				{"id": 31, "eventType": "downloadFolderImported", "date": "2026-09-04T11:00:00Z", "movieFileId": 2001},
			})
		case "/api/v3/moviefile/2001":
			fileRequests++
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 2001, "movieId": 20, "path": "/remote/movies/movie.mkv", "size": 1, "dateAdded": "2026-09-04T10:00:00Z"})
		case "/api/v3/movie/20":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 20, "title": "Movie"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	catalog, err := NewRadarr("radarr-main", server.URL, "secret", []config.PathMapping{{Remote: "/remote/movies", Local: root}}, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	changes, err := catalog.ListChangesSince(context.Background(), time.Date(2026, 9, 4, 9, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 1 || changes[0].HistoryID != 31 || changes[0].Type != EventImport || changes[0].Ref.FileID != 2001 || changes[0].Media.Ref.FileID != 2001 {
		t.Fatalf("changes = %#v", changes)
	}
	if fileRequests != 1 {
		t.Fatalf("movie file requests = %d, want 1", fileRequests)
	}
}

func TestRadarrListChangesSinceReturnsDeleteWithoutHydration(t *testing.T) {
	fileRequests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v3/history/since" {
			_ = json.NewEncoder(w).Encode([]map[string]any{{"id": 40, "eventType": "movieFileDeleted", "date": "2026-09-04T12:00:00Z", "movieFileId": 2002}})
			return
		}
		fileRequests++
		http.NotFound(w, r)
	}))
	defer server.Close()
	catalog, err := NewRadarr("radarr-main", server.URL, "secret", nil, []string{t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	changes, err := catalog.ListChangesSince(context.Background(), time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 1 || changes[0].Type != EventDelete || changes[0].Ref.FileID != 2002 {
		t.Fatalf("changes = %#v", changes)
	}
	if fileRequests != 0 {
		t.Fatalf("hydration requests = %d, want 0", fileRequests)
	}
}
