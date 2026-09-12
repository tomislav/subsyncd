package catalog

import (
	"context"
	"encoding/json"
	"errors"
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

func TestRadarrListLibraryHydratesInScopeMoviesWithoutHistory(t *testing.T) {
	root := t.TempDir()
	fileRequests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v3/movie":
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"id": 20, "hasFile": true, "movieFile": map[string]any{"id": 2001, "path": "/remote/movies/Example/Example.mkv"}},
				{"id": 20, "hasFile": true, "movieFile": map[string]any{"id": 2001, "path": "/remote/movies/Example/Example.mkv"}},
				{"id": 21, "hasFile": false},
				{"id": 22, "hasFile": true, "movieFile": map[string]any{"id": 2002, "path": "/elsewhere/Other.mkv"}},
			})
		case "/api/v3/moviefile/2001":
			fileRequests++
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 2001, "movieId": 20, "path": "/remote/movies/Example/Example.mkv", "size": 4321})
		case "/api/v3/movie/20":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 20, "title": "Example", "year": 2024})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	cat, err := NewRadarr("radarr-main", server.URL, "secret", []config.PathMapping{{Remote: "/remote/movies", Local: root}}, []string{root}, nil)
	if err != nil {
		t.Fatal(err)
	}
	items, err := cat.ListLibrary(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].EntityID != 20 || items[0].Ref.FileID != 2001 || items[0].Title != "Example" {
		t.Fatalf("library = %#v", items)
	}
	if fileRequests != 1 {
		t.Fatalf("detail requests = %d, want 1", fileRequests)
	}
}

func TestRadarrGetMediaRejectsMismatchedHydrationIdentities(t *testing.T) {
	root := t.TempDir()
	for name, tc := range map[string]struct {
		file  map[string]any
		movie map[string]any
	}{
		"file":   {map[string]any{"id": 2002, "movieId": 20, "path": "/remote/movies/movie.mkv"}, map[string]any{"id": 20, "title": "Movie"}},
		"movie":  {map[string]any{"id": 2001, "movieId": 20, "path": "/remote/movies/movie.mkv"}, map[string]any{"id": 21, "title": "Movie"}},
		"entity": {map[string]any{"id": 2001, "movieId": 0, "path": "/remote/movies/movie.mkv"}, map[string]any{}},
	} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/api/v3/moviefile/2001":
					_ = json.NewEncoder(w).Encode(tc.file)
				case "/api/v3/movie/20":
					_ = json.NewEncoder(w).Encode(tc.movie)
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			cat, err := NewRadarr("radarr-main", server.URL, "secret", []config.PathMapping{{Remote: "/remote/movies", Local: root}}, []string{root}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := cat.GetMedia(context.Background(), domain.MediaRef{Instance: "radarr-main", Kind: domain.MediaMovie, FileID: 2001}); err == nil {
				t.Fatal("GetMedia() error = nil")
			}
		})
	}
}

func TestRadarrHistoryResolvesLatestEntityStateWithinPageEnd(t *testing.T) {
	root := t.TempDir()
	history, err := os.ReadFile("testdata/radarr_history.json")
	if err != nil {
		t.Fatal(err)
	}
	var historyPage string
	movieRequests := 0
	fileRequests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v3/history":
			historyPage = r.URL.Query().Get("page")
			if r.URL.Query().Get("pageSize") != "100" || r.URL.Query().Get("sortDirection") != "descending" {
				t.Error("history request is not bounded and descending")
			}
			writeHistoryFixture(t, w, history)
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
	if historyPage != "1" {
		t.Fatalf("history page = %q, want 1", historyPage)
	}
}

func TestRadarrHistoryReturnsAbsentWithoutDetailHydration(t *testing.T) {
	fileRequests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v3/history":
			writeHistoryRecords(t, w, []map[string]any{{"id": 40, "movieId": 402, "eventType": "movieFileDeleted", "date": "2026-09-05T12:00:00Z", "data": map[string]string{}}})
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

func TestRadarrHistoryDefersBrokenSymlinkAfterCurrentStateRecheck(t *testing.T) {
	root := t.TempDir()
	if err := os.Symlink(filepath.Join(root, "missing", "movie.mkv"), filepath.Join(root, "movie.mkv")); err != nil {
		t.Fatal(err)
	}
	movieRequests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v3/history":
			writeHistoryRecords(t, w, []map[string]any{
				{"id": 40, "movieId": 402, "eventType": "downloadFolderImported", "date": "2026-09-05T10:00:00Z", "data": map[string]string{"fileId": "1700"}},
				{"id": 41, "movieId": 403, "eventType": "movieFileDeleted", "date": "2026-09-05T11:00:00Z", "data": map[string]string{}},
			})
		case "/api/v3/movie/402":
			movieRequests++
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 402, "hasFile": true, "movieFile": map[string]any{"id": 1700, "movieId": 402, "path": "/remote/movies/movie.mkv"}})
		case "/api/v3/movie/403":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 403, "hasFile": false})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	catalog, err := NewRadarr("radarr-main", server.URL, "secret", []config.PathMapping{{Remote: "/remote/movies", Local: root}}, []string{root}, nil)
	if err != nil {
		t.Fatal(err)
	}
	changes, err := catalog.ListChanges(context.Background(), time.Time{}, time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC))
	if !errors.Is(err, ErrHistoryDeferred) {
		t.Fatalf("ListChanges() error = %v, want ErrHistoryDeferred", err)
	}
	if movieRequests != 2 {
		t.Fatalf("current movie requests = %d, want 2", movieRequests)
	}
	if len(changes) != 1 || changes[0].HistoryID != 41 || changes[0].EntityID != 403 || changes[0].State != HistoryAbsent {
		t.Fatalf("nondeferred changes = %#v", changes)
	}
}

func TestRadarrHistoryUsesReplacementWhenSymlinkBreaksDuringHydration(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target.mkv")
	if err := os.WriteFile(target, []byte("media"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(root, "movie.mkv")); err != nil {
		t.Fatal(err)
	}
	replacement := filepath.Join(root, "replacement.mkv")
	if err := os.WriteFile(replacement, []byte("replacement"), 0o600); err != nil {
		t.Fatal(err)
	}
	movieRequests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v3/history":
			writeHistoryRecords(t, w, []map[string]any{{"id": 40, "movieId": 402, "eventType": "downloadFolderImported", "date": "2026-09-05T10:00:00Z", "data": map[string]string{"fileId": "1700"}}})
		case "/api/v3/movie/402":
			movieRequests++
			fileID, path := 1700, "/remote/movies/movie.mkv"
			if movieRequests == 3 {
				fileID, path = 1701, "/remote/movies/replacement.mkv"
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 402, "title": "Movie", "hasFile": true, "movieFile": map[string]any{"id": fileID, "movieId": 402, "path": path}})
		case "/api/v3/moviefile/1700":
			_ = os.Remove(target)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 1700, "movieId": 402, "path": "/remote/movies/movie.mkv", "size": 5})
		case "/api/v3/moviefile/1701":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 1701, "movieId": 402, "path": "/remote/movies/replacement.mkv", "size": 11})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	catalog, err := NewRadarr("radarr-main", server.URL, "secret", []config.PathMapping{{Remote: "/remote/movies", Local: root}}, []string{root}, nil)
	if err != nil {
		t.Fatal(err)
	}
	changes, err := catalog.ListChanges(context.Background(), time.Time{}, time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC))
	if err != nil || len(changes) != 1 || changes[0].State != HistoryPresent || changes[0].Media.Ref.FileID != 1701 {
		t.Fatalf("ListChanges() = %#v, %v; want present replacement file 1701", changes, err)
	}
	if movieRequests != 4 {
		t.Fatalf("current movie requests = %d, want current, old detail, recheck, and replacement detail", movieRequests)
	}
}

func TestRadarrHistoryReturnsOutsideScopeWithoutDetailHydration(t *testing.T) {
	detailRequests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v3/history":
			writeHistoryRecords(t, w, []map[string]any{{"id": 50, "movieId": 403, "eventType": "downloadFolderImported", "date": "2026-09-05T12:00:00Z", "data": map[string]string{"fileId": "1700"}}})
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
