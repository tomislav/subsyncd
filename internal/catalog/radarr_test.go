package catalog

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cplieger/arrapi/v2"

	"subsyncd/internal/config"
	"subsyncd/internal/domain"
)

func TestRadarrGetMediaHydratesFileAndMovie(t *testing.T) {
	root := t.TempDir()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v3/moviefile/2001":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 2001, "movieId": 20, "path": "/remote/movies/Example Movie/Example.Movie.2024.mkv", "size": 4321, "dateAdded": "2026-09-04T11:00:00Z", "sceneName": "Example.Movie.2024.2160p.AMZN.WEB-DL-GROUP", "releaseGroup": "GROUP", "edition": "Extended", "quality": map[string]any{"quality": map[string]any{"name": "WEBDL-2160p", "resolution": 2160, "source": "webdl"}}, "mediaInfo": map[string]any{"runTime": "02:03:04"}})
		case "/api/v3/movie/20":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 20, "title": "Example Movie", "alternateTitles": []map[string]any{{"title": "Primjer filma"}}, "year": 2024, "imdbId": "tt7654321", "tmdbId": 2468})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	catalog, err := NewRadarr("radarr-main", server.URL, "secret", []config.PathMapping{{Remote: "/remote/movies", Local: root}}, []string{root}, nil)
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
	if media.StreamingService != "amazon" {
		t.Fatalf("streaming service = %q, want amazon", media.StreamingService)
	}
	if media.EntityID != 20 {
		t.Fatalf("entity ID = %d, want 20", media.EntityID)
	}
	if media.Fingerprint.Path != filepath.Join(root, "Example Movie", "Example.Movie.2024.mkv") || media.Fingerprint.Size != 4321 {
		t.Fatalf("fingerprint = %#v", media.Fingerprint)
	}
	if media.OriginalFilename == "" || media.ReleaseGroup != "GROUP" || media.Edition != "Extended" || media.Resolution != "2160p" || media.Source != "webdl" || media.Duration != 2*time.Hour+3*time.Minute+4*time.Second {
		t.Fatalf("release metadata = %#v", media)
	}
}

func TestRadarrHistoryResolvesLatestEntityStateWithinPageEnd(t *testing.T) {
	root := t.TempDir()
	history, err := os.ReadFile("testdata/radarr_history.json")
	if err != nil {
		t.Fatal(err)
	}
	var historyDate string
	movieRequests := 0
	fileRequests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v3/history/since":
			historyDate = r.URL.Query().Get("date")
			if got := r.URL.Query().Get("includeMovie"); got != "false" {
				t.Errorf("includeMovie = %q, want false", got)
			}
			_, _ = w.Write(history)
		case "/api/v3/movie/400":
			movieRequests++
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 400, "title": "Example Movie", "year": 2026, "hasFile": true, "movieFile": map[string]any{"id": 1532, "path": "/remote/movies/movie.mkv", "size": 1}})
		case "/api/v3/moviefile/1532":
			fileRequests++
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 1532, "movieId": 400, "path": "/remote/movies/movie.mkv", "size": 1, "dateAdded": "2026-09-05T05:14:00Z"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	catalog, err := NewRadarr("radarr-main", server.URL, "secret", []config.PathMapping{{Remote: "/remote/movies", Local: root}}, []string{root}, nil)
	if err != nil {
		t.Fatal(err)
	}
	since := time.Date(2026, 9, 5, 5, 0, 0, 987654321, time.UTC)
	through := time.Date(2026, 9, 5, 6, 0, 0, 0, time.UTC)
	changes, err := catalog.ListChanges(context.Background(), since, through)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 1 || changes[0].HistoryID != 5419 || changes[0].EntityID != 400 || changes[0].Type != EventImport || changes[0].State != HistoryPresent || changes[0].Media.Ref.FileID != 1532 || changes[0].Media.EntityID != 400 {
		t.Fatalf("changes = %#v", changes)
	}
	if fileRequests != 1 {
		t.Fatalf("movie file requests = %d, want 1", fileRequests)
	}
	if movieRequests != 2 {
		t.Fatalf("movie requests = %d, want current-state plus detail", movieRequests)
	}
	if historyDate != "2026-09-05T05:00:00Z" {
		t.Fatalf("history date = %q, want RFC3339 seconds", historyDate)
	}
}

func TestRadarrHistoryReturnsAbsentWithoutDetailHydration(t *testing.T) {
	fileRequests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v3/history/since":
			_ = json.NewEncoder(w).Encode([]map[string]any{{"id": 40, "movieId": 402, "eventType": "movieFileDeleted", "date": "2026-09-05T12:00:00Z", "data": map[string]string{}}})
		case "/api/v3/movie/402":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 402, "hasFile": false})
		default:
			fileRequests++
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	catalog, err := NewRadarr("radarr-main", server.URL, "secret", nil, []string{t.TempDir()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	changes, err := catalog.ListChanges(context.Background(), time.Time{}, time.Date(2026, 9, 5, 13, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 1 || changes[0].Type != EventDelete || changes[0].State != HistoryAbsent || changes[0].EntityID != 402 || changes[0].Media.Ref.FileID != 0 {
		t.Fatalf("changes = %#v", changes)
	}
	if fileRequests != 0 {
		t.Fatalf("hydration requests = %d, want 0", fileRequests)
	}
}

func TestRadarrHistoryReturnsOutsideScopeWithoutDetailHydration(t *testing.T) {
	detailRequests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v3/history/since":
			_ = json.NewEncoder(w).Encode([]map[string]any{{"id": 50, "movieId": 403, "eventType": "downloadFolderImported", "date": "2026-09-05T12:00:00Z", "data": map[string]string{"fileId": "1700"}}})
		case "/api/v3/movie/403":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 403, "hasFile": true, "movieFile": map[string]any{"id": 1700, "path": "/unmapped/movie.mkv"}})
		default:
			detailRequests++
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	catalog, err := NewRadarr("radarr-main", server.URL, "secret", nil, []string{t.TempDir()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	changes, err := catalog.ListChanges(context.Background(), time.Time{}, time.Date(2026, 9, 5, 13, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 1 || changes[0].State != HistoryOutsideScope || changes[0].EntityID != 403 {
		t.Fatalf("changes = %#v", changes)
	}
	if detailRequests != 0 {
		t.Fatalf("detail requests = %d, want 0", detailRequests)
	}
}

func TestRadarrHistoryRejectsMalformedRelevantEntityRecords(t *testing.T) {
	through := time.Date(2026, 9, 5, 13, 0, 0, 0, time.UTC)
	for name, record := range map[string]arrapi.HistoryRecord{
		"missing history ID": {MovieID: 400, EventType: arrapi.EventFileDeleted, Date: through.Add(-time.Minute)},
		"missing entity ID":  {ID: 1, EventType: arrapi.EventFileDeleted, Date: through.Add(-time.Minute)},
		"missing date":       {ID: 1, MovieID: 400, EventType: arrapi.EventFileDeleted},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := reduceHistoryByEntity([]arrapi.HistoryRecord{record}, domain.MediaMovie, through, func(item arrapi.HistoryRecord) int64 { return int64(item.MovieID) }); err == nil {
				t.Fatal("reduceHistoryByEntity() error = nil")
			}
		})
	}
}
