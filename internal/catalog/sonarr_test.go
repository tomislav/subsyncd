package catalog

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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

	catalog, err := NewSonarr("sonarr-main", server.URL, "secret", []config.PathMapping{{Remote: "/remote/tv", Local: root}}, []string{root})
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
	catalog, err := NewSonarr("sonarr-main", server.URL, "secret", []config.PathMapping{{Remote: "/remote/tv", Local: root}}, []string{root})
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
	catalog, err := NewSonarr("sonarr-main", server.URL, "secret", nil, []string{t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	_, err = catalog.GetMedia(context.Background(), domain.MediaRef{Instance: "sonarr-main", Kind: domain.MediaEpisode, FileID: 1001})
	if err == nil || !strings.Contains(err.Error(), "file 1001 has no episode") {
		t.Fatalf("GetMedia() error = %v", err)
	}
}

func TestSonarrListChangesSinceHydratesLatestRelevantHistory(t *testing.T) {
	root := t.TempDir()
	var historyDate string
	fileRequests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/v3/history/since":
			historyDate = r.URL.Query().Get("date")
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"id": 10, "eventType": "downloadFolderImported", "date": "2026-09-04T10:00:00Z", "episodeFileId": 1001},
				{"id": 11, "eventType": "grabbed", "date": "2026-09-04T10:15:00Z", "episodeFileId": 1001},
				{"id": 12, "eventType": "episodeFileRenamed", "date": "2026-09-04T10:30:00Z", "episodeFileId": 1001},
			})
		case r.URL.Path == "/api/v3/episodefile/1001":
			fileRequests++
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 1001, "seriesId": 10, "path": "/remote/tv/show.mkv", "size": 1, "dateAdded": "2026-09-04T10:00:00Z"})
		case r.URL.Path == "/api/v3/episode":
			_ = json.NewEncoder(w).Encode([]map[string]any{{"id": 101, "seriesId": 10, "seasonNumber": 1, "episodeNumber": 1}})
		case r.URL.Path == "/api/v3/series/10":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 10, "title": "Show"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	catalog, err := NewSonarr("sonarr-main", server.URL, "secret", []config.PathMapping{{Remote: "/remote/tv", Local: root}}, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	since := time.Date(2026, 9, 4, 9, 0, 0, 123, time.UTC)
	changes, err := catalog.ListChangesSince(context.Background(), since)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 1 || changes[0].HistoryID != 12 || changes[0].Type != EventRename || changes[0].Ref.FileID != 1001 || changes[0].Media.Ref.FileID != 1001 || !changes[0].OccurredAt.Equal(time.Date(2026, 9, 4, 10, 30, 0, 0, time.UTC)) {
		t.Fatalf("changes = %#v", changes)
	}
	if fileRequests != 1 {
		t.Fatalf("episode file requests = %d, want 1", fileRequests)
	}
	if historyDate != since.Format(time.RFC3339Nano) {
		t.Fatalf("history date = %q, want %q", historyDate, since.Format(time.RFC3339Nano))
	}
}

func TestSonarrListChangesSinceImportThenDeleteDoesNotHydrate(t *testing.T) {
	fileRequests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/v3/history/since" {
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"id": 20, "eventType": "downloadFolderImported", "date": "2026-09-04T10:00:00Z", "episodeFileId": 1002},
				{"id": 21, "eventType": "episodeFileDeleted", "date": "2026-09-04T11:00:00Z", "episodeFileId": 1002},
			})
			return
		}
		fileRequests++
		http.NotFound(w, r)
	}))
	defer server.Close()
	catalog, err := NewSonarr("sonarr-main", server.URL, "secret", nil, []string{t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	changes, err := catalog.ListChangesSince(context.Background(), time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 1 || changes[0].HistoryID != 21 || changes[0].Type != EventDelete || changes[0].Ref.FileID != 1002 || changes[0].Media.Ref.FileID != 0 {
		t.Fatalf("changes = %#v", changes)
	}
	if fileRequests != 0 {
		t.Fatalf("hydration requests = %d, want 0", fileRequests)
	}
}

func TestSonarrListChangesSinceRejectsMalformedRelevantHistory(t *testing.T) {
	for name, record := range map[string]map[string]any{
		"missing history id": {"eventType": "episodeFileDeleted", "date": "2026-09-04T11:00:00Z", "episodeFileId": 1002},
		"missing file id":    {"id": 21, "eventType": "episodeFileDeleted", "date": "2026-09-04T11:00:00Z"},
		"missing date":       {"id": 21, "eventType": "episodeFileDeleted", "episodeFileId": 1002},
	} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _ = json.NewEncoder(w).Encode([]map[string]any{record}) }))
			defer server.Close()
			catalog, err := NewSonarr("sonarr-main", server.URL, "secret", nil, []string{t.TempDir()})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := catalog.ListChangesSince(context.Background(), time.Time{}); err == nil {
				t.Fatal("ListChangesSince() error = nil")
			}
		})
	}
}
