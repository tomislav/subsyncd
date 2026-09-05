package catalog

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"subsyncd/internal/config"
	"subsyncd/internal/domain"
)

func TestSonarrGetMediaHydratesFileEpisodeAndSeries(t *testing.T) {
	root := t.TempDir()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-Key") != "secret" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/v3/episodefile/1001":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 1001, "seriesId": 10, "path": "/remote/tv/Example Show/Example.Show.S01E02.mkv", "size": 1234, "dateAdded": "2026-09-04T10:00:00Z", "sceneName": "Example.Show.S01E02.1080p.WEB-DL-GROUP", "releaseGroup": "GROUP", "quality": map[string]any{"quality": map[string]any{"name": "WEBDL-1080p", "resolution": 1080, "source": "webdl"}}, "mediaInfo": map[string]any{"runTime": "00:42:30"}})
		case r.URL.Path == "/api/v3/episode" && r.URL.Query().Get("episodeFileId") == "1001":
			_ = json.NewEncoder(w).Encode([]map[string]any{{"id": 101, "seriesId": 10, "seasonNumber": 1, "episodeNumber": 2, "absoluteEpisodeNumber": 14, "title": "Second Episode"}})
		case r.URL.Path == "/api/v3/series/10":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 10, "title": "Example Show", "alternateTitles": []map[string]any{{"title": "Primjer"}}, "year": 2024, "imdbId": "tt1234567", "tvdbId": 7654})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	catalog, err := NewSonarr("sonarr-main", server.URL, "secret", []config.PathMapping{{Remote: "/remote/tv", Local: root}}, []string{root}, nil)
	if err != nil {
		t.Fatal(err)
	}
	media, err := catalog.GetMedia(context.Background(), domain.MediaRef{Instance: "sonarr-main", Kind: domain.MediaEpisode, FileID: 1001})
	if err != nil {
		t.Fatal(err)
	}
	if media.Title != "Example Show" || media.EpisodeTitle != "Second Episode" || media.Season != 1 || media.Episode != 2 || media.AbsoluteEpisode != 14 {
		t.Fatalf("episode identity = %#v", media)
	}
	if media.EntityID != 101 {
		t.Fatalf("entity ID = %d, want 101", media.EntityID)
	}
	if media.Fingerprint.Path != filepath.Join(root, "Example Show", "Example.Show.S01E02.mkv") || media.Fingerprint.Size != 1234 {
		t.Fatalf("fingerprint = %#v", media.Fingerprint)
	}
	if media.ExternalIDs.IMDb != "tt1234567" || media.ExternalIDs.TVDB != 7654 || media.OriginalFilename == "" || media.ReleaseGroup != "GROUP" || media.Quality != "WEBDL-1080p" || media.Duration != 42*time.Minute+30*time.Second {
		t.Fatalf("release metadata = %#v", media)
	}
}

func TestSonarrGetMediaIndexesMultiEpisodeFileAsUnsupported(t *testing.T) {
	root := t.TempDir()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/v3/episodefile/1001":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 1001, "seriesId": 10, "path": "/remote/tv/show.mkv", "size": 1234, "dateAdded": "2026-09-04T10:00:00Z"})
		case r.URL.Path == "/api/v3/episode":
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"id": 102, "seriesId": 10, "seasonNumber": 1, "episodeNumber": 2, "absoluteEpisodeNumber": 12, "title": "Second"},
				{"id": 101, "seriesId": 10, "seasonNumber": 1, "episodeNumber": 1, "absoluteEpisodeNumber": 11, "title": "First"},
			})
		case r.URL.Path == "/api/v3/series/10":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 10, "title": "Show"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	catalog, err := NewSonarr("sonarr-main", server.URL, "secret", []config.PathMapping{{Remote: "/remote/tv", Local: root}}, []string{root}, nil)
	if err != nil {
		t.Fatal(err)
	}
	media, episodeIDs, err := catalog.hydrateMedia(context.Background(), domain.MediaRef{Instance: "sonarr-main", Kind: domain.MediaEpisode, FileID: 1001})
	if err != nil {
		t.Fatal(err)
	}
	if media.EntityID != 101 || media.Season != 1 || media.Episode != 1 || media.EpisodeTitle != "First" || media.UnsupportedReason != domain.UnsupportedMultiEpisode {
		t.Fatalf("multi-episode media = %#v", media)
	}
	if len(episodeIDs) != 2 || episodeIDs[0] != 101 || episodeIDs[1] != 102 {
		t.Fatalf("attached episode IDs = %v, want [101 102]", episodeIDs)
	}
}

func TestSonarrGetMediaRejectsFileWithoutEpisodes(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v3/episodefile/1001":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 1001, "seriesId": 10})
		case "/api/v3/episode":
			_ = json.NewEncoder(w).Encode([]map[string]any{})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	catalog, err := NewSonarr("sonarr-main", server.URL, "secret", nil, []string{t.TempDir()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = catalog.GetMedia(context.Background(), domain.MediaRef{Instance: "sonarr-main", Kind: domain.MediaEpisode, FileID: 1001})
	if err == nil || !strings.Contains(err.Error(), "file 1001 has no episode") {
		t.Fatalf("GetMedia() error = %v", err)
	}
}

func TestSonarrHistoryCollapsesAttachedEpisodesToCanonicalEntity(t *testing.T) {
	root := t.TempDir()
	history, err := os.ReadFile("testdata/sonarr_history.json")
	if err != nil {
		t.Fatal(err)
	}
	var historyDate string
	fileRequests := 0
	currentRequests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/v3/history/since":
			historyDate = r.URL.Query().Get("date")
			if got := r.URL.Query().Get("includeEpisode"); got != "false" {
				t.Errorf("includeEpisode = %q, want false", got)
			}
			if got := r.URL.Query().Get("includeSeries"); got != "false" {
				t.Errorf("includeSeries = %q, want false", got)
			}
			_, _ = w.Write(history)
		case r.URL.Path == "/api/v3/episode/101":
			currentRequests++
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 101, "seriesId": 10, "seasonNumber": 1, "episodeNumber": 1, "hasFile": true, "episodeFile": map[string]any{"id": 1001, "seriesId": 10, "path": "/remote/tv/show.mkv"}})
		case r.URL.Path == "/api/v3/episode/102":
			currentRequests++
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 102, "seriesId": 10, "seasonNumber": 1, "episodeNumber": 2, "hasFile": true, "episodeFile": map[string]any{"id": 1001, "seriesId": 10, "path": "/remote/tv/show.mkv"}})
		case r.URL.Path == "/api/v3/episodefile/1001":
			fileRequests++
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 1001, "seriesId": 10, "path": "/remote/tv/show.mkv", "size": 1, "dateAdded": "2026-09-05T05:20:00Z"})
		case r.URL.Path == "/api/v3/episode" && r.URL.Query().Get("episodeFileId") == "1001":
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"id": 102, "seriesId": 10, "seasonNumber": 1, "episodeNumber": 2, "title": "Second"},
				{"id": 101, "seriesId": 10, "seasonNumber": 1, "episodeNumber": 1, "title": "First"},
			})
		case r.URL.Path == "/api/v3/series/10":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 10, "title": "Show"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	catalog, err := NewSonarr("sonarr-main", server.URL, "secret", []config.PathMapping{{Remote: "/remote/tv", Local: root}}, []string{root}, nil)
	if err != nil {
		t.Fatal(err)
	}
	since := time.Date(2026, 9, 5, 5, 0, 0, 123456789, time.UTC)
	changes, err := catalog.ListChanges(context.Background(), since, time.Date(2026, 9, 5, 6, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 1 || changes[0].HistoryID != 6102 || changes[0].EntityID != 101 || changes[0].Type != EventRename || changes[0].State != HistoryPresent || changes[0].Media.Ref.FileID != 1001 || changes[0].Media.EntityID != 101 {
		t.Fatalf("changes = %#v", changes)
	}
	if fileRequests != 1 {
		t.Fatalf("episode file requests = %d, want 1", fileRequests)
	}
	if currentRequests != 2 {
		t.Fatalf("current episode requests = %d, want 2", currentRequests)
	}
	if historyDate != "2026-09-05T05:00:00Z" {
		t.Fatalf("history date = %q, want RFC3339 seconds", historyDate)
	}
}

func TestSonarrHistoryAbsentDoesNotHydrateDetail(t *testing.T) {
	fileRequests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/v3/history/since" {
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"id": 20, "seriesId": 10, "episodeId": 103, "eventType": "downloadFolderImported", "date": "2026-09-05T10:00:00Z", "data": map[string]string{"fileId": "1002"}},
				{"id": 21, "seriesId": 10, "episodeId": 103, "eventType": "episodeFileDeleted", "date": "2026-09-05T11:00:00Z", "data": map[string]string{}},
			})
			return
		}
		if r.URL.Path == "/api/v3/episode/103" {
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 103, "seriesId": 10, "hasFile": false})
			return
		}
		fileRequests++
		http.NotFound(w, r)
	}))
	defer server.Close()
	catalog, err := NewSonarr("sonarr-main", server.URL, "secret", nil, []string{t.TempDir()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	changes, err := catalog.ListChanges(context.Background(), time.Time{}, time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 1 || changes[0].HistoryID != 21 || changes[0].EntityID != 103 || changes[0].Type != EventDelete || changes[0].State != HistoryAbsent || changes[0].Media.Ref.FileID != 0 {
		t.Fatalf("changes = %#v", changes)
	}
	if fileRequests != 0 {
		t.Fatalf("hydration requests = %d, want 0", fileRequests)
	}
}

func TestSonarrHistoryRejectsEpisodeMissingFromCurrentFile(t *testing.T) {
	root := t.TempDir()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/v3/history/since":
			_ = json.NewEncoder(w).Encode([]map[string]any{{"id": 30, "seriesId": 10, "episodeId": 104, "eventType": "downloadFolderImported", "date": "2026-09-05T11:00:00Z", "data": map[string]string{"fileId": "1004"}}})
		case r.URL.Path == "/api/v3/episode/104":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 104, "seriesId": 10, "hasFile": true, "episodeFile": map[string]any{"id": 1004, "seriesId": 10, "path": "/remote/tv/show.mkv"}})
		case r.URL.Path == "/api/v3/episodefile/1004":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 1004, "seriesId": 10, "path": "/remote/tv/show.mkv", "size": 1})
		case r.URL.Path == "/api/v3/episode":
			_ = json.NewEncoder(w).Encode([]map[string]any{{"id": 101, "seriesId": 10, "seasonNumber": 1, "episodeNumber": 1}})
		case r.URL.Path == "/api/v3/series/10":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 10, "title": "Show"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	catalog, err := NewSonarr("sonarr-main", server.URL, "secret", []config.PathMapping{{Remote: "/remote/tv", Local: root}}, []string{root}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := catalog.ListChanges(context.Background(), time.Time{}, time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)); err == nil || !strings.Contains(err.Error(), "not attached") {
		t.Fatalf("ListChanges() error = %v, want membership failure", err)
	}
}
