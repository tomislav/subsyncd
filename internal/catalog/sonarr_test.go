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
	if media.Fingerprint.Path != filepath.Join(root, "Example Show", "Example.Show.S01E02.mkv") || media.Fingerprint.Size != 1234 {
		t.Fatalf("fingerprint = %#v", media.Fingerprint)
	}
	if media.ExternalIDs.IMDb != "tt1234567" || media.ExternalIDs.TVDB != 7654 || media.OriginalFilename == "" || media.ReleaseGroup != "GROUP" || media.Quality != "WEBDL-1080p" || media.Duration != 42*time.Minute+30*time.Second {
		t.Fatalf("release metadata = %#v", media)
	}
}

func TestSonarrListMediaChangedSinceHydratesUniqueHistoryFiles(t *testing.T) {
	root := t.TempDir()
	var historyDate string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/v3/history/since":
			historyDate = r.URL.Query().Get("date")
			_ = json.NewEncoder(w).Encode([]map[string]any{{"episodeFileId": 1001}, {"episodeFileId": 1001}})
		case r.URL.Path == "/api/v3/episodefile/1001":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 1001, "seriesId": 10, "path": "/remote/tv/show.mkv", "size": 1, "dateAdded": "2026-09-04T10:00:00Z"})
		case r.URL.Path == "/api/v3/episode":
			_ = json.NewEncoder(w).Encode([]map[string]any{{"seriesId": 10, "seasonNumber": 1, "episodeNumber": 1}})
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
	media, err := catalog.ListMediaChangedSince(context.Background(), since)
	if err != nil {
		t.Fatal(err)
	}
	if len(media) != 1 || media[0].Ref.FileID != 1001 {
		t.Fatalf("media = %#v", media)
	}
	if historyDate != since.Format(time.RFC3339Nano) {
		t.Fatalf("history date = %q, want %q", historyDate, since.Format(time.RFC3339Nano))
	}
}
