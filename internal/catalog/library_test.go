package catalog

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cplieger/arrapi/v2"

	"subsyncd/internal/config"
)

type radarrLibraryFake struct {
	movies []arrapi.Movie
	cancel context.CancelFunc
}

func (f radarrLibraryFake) History(context.Context, arrapi.HistoryOptions) (arrapi.HistoryPage, error) {
	return arrapi.HistoryPage{}, nil
}

func (f radarrLibraryFake) MovieByID(context.Context, int) (arrapi.Movie, error) {
	return arrapi.Movie{}, nil
}

func (f radarrLibraryFake) Movies(context.Context) ([]arrapi.Movie, error) {
	if f.cancel != nil {
		f.cancel()
	}
	return f.movies, nil
}

type sonarrLibraryFake struct {
	series []arrapi.Series
	files  map[int][]arrapi.EpisodeFile
	cancel context.CancelFunc
}

func (f sonarrLibraryFake) History(context.Context, arrapi.HistoryOptions) (arrapi.HistoryPage, error) {
	return arrapi.HistoryPage{}, nil
}

func (f sonarrLibraryFake) EpisodeByID(context.Context, int) (arrapi.Episode, error) {
	return arrapi.Episode{}, nil
}

func (f sonarrLibraryFake) Series(context.Context) ([]arrapi.Series, error) {
	if f.cancel != nil {
		f.cancel()
	}
	return f.series, nil
}

func (f sonarrLibraryFake) EpisodeFiles(_ context.Context, seriesID int) ([]arrapi.EpisodeFile, error) {
	return f.files[seriesID], nil
}

func TestRadarrListLibraryRejectsMalformedAndConflictingEntries(t *testing.T) {
	for name, movies := range map[string][]arrapi.Movie{
		"missing file details": {{ID: 1, HasFile: true}},
		"conflicting duplicate": {
			{ID: 1, HasFile: true, MovieFile: &arrapi.MovieFile{ID: 10, Path: "/movies/a.mkv"}},
			{ID: 1, HasFile: true, MovieFile: &arrapi.MovieFile{ID: 11, Path: "/movies/b.mkv"}},
		},
	} {
		t.Run(name, func(t *testing.T) {
			cat := &Radarr{client: &arrClient{instance: "radarr-main"}, entity: radarrLibraryFake{movies: movies}}
			if _, err := cat.ListLibrary(context.Background()); err == nil {
				t.Fatal("ListLibrary() error = nil")
			}
		})
	}
}

func TestSonarrListLibraryRejectsConflictingDuplicateFile(t *testing.T) {
	cat := &Sonarr{
		client: &arrClient{instance: "sonarr-main"},
		entity: sonarrLibraryFake{
			series: []arrapi.Series{{ID: 1}},
			files: map[int][]arrapi.EpisodeFile{1: {
				{ID: 10, SeriesID: 1, Path: "/tv/a.mkv"},
				{ID: 10, SeriesID: 1, Path: "/tv/b.mkv"},
			}},
		},
	}
	if _, err := cat.ListLibrary(context.Background()); err == nil || !strings.Contains(err.Error(), "multiple series") {
		t.Fatalf("ListLibrary() error = %v", err)
	}
}

func TestListLibraryHonorsCancellationFromEnumeration(t *testing.T) {
	for name, run := range map[string]func(context.Context, context.CancelFunc) error{
		"radarr": func(ctx context.Context, cancel context.CancelFunc) error {
			cat := &Radarr{client: &arrClient{instance: "radarr-main"}, entity: radarrLibraryFake{cancel: cancel}}
			_, err := cat.ListLibrary(ctx)
			return err
		},
		"sonarr": func(ctx context.Context, cancel context.CancelFunc) error {
			cat := &Sonarr{client: &arrClient{instance: "sonarr-main"}, entity: sonarrLibraryFake{cancel: cancel}}
			_, err := cat.ListLibrary(ctx)
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			if err := run(ctx, cancel); err != context.Canceled {
				t.Fatalf("ListLibrary() error = %v, want context canceled", err)
			}
		})
	}
}

func TestListLibraryRejectsNullCollectionsAndAcceptsEmptyCollections(t *testing.T) {
	for _, tc := range []struct {
		name      string
		kind      string
		response  string
		wantError bool
	}{
		{name: "radarr null movies", kind: "radarr", response: "null", wantError: true},
		{name: "radarr empty movies", kind: "radarr", response: "[]"},
		{name: "sonarr null series", kind: "sonarr", response: "null", wantError: true},
		{name: "sonarr empty series", kind: "sonarr", response: "[]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tc.response))
			}))
			defer server.Close()
			root := t.TempDir()
			var err error
			if tc.kind == "radarr" {
				cat, createErr := NewRadarr("radarr-main", server.URL, "secret", []config.PathMapping{{Remote: "/movies", Local: root}}, []string{root}, nil)
				if createErr != nil {
					t.Fatal(createErr)
				}
				_, err = cat.ListLibrary(context.Background())
			} else {
				cat, createErr := NewSonarr("sonarr-main", server.URL, "secret", []config.PathMapping{{Remote: "/tv", Local: root}}, []string{root}, nil)
				if createErr != nil {
					t.Fatal(createErr)
				}
				_, err = cat.ListLibrary(context.Background())
			}
			if (err != nil) != tc.wantError {
				t.Fatalf("ListLibrary() error = %v, wantError %t", err, tc.wantError)
			}
		})
	}
}

func TestSonarrListLibraryRejectsNullEpisodeFiles(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/v3/series" {
			_, _ = w.Write([]byte(`[{"id":1}]`))
			return
		}
		_, _ = w.Write([]byte("null"))
	}))
	defer server.Close()
	root := t.TempDir()
	cat, err := NewSonarr("sonarr-main", server.URL, "secret", []config.PathMapping{{Remote: "/tv", Local: root}}, []string{root}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cat.ListLibrary(context.Background()); err == nil {
		t.Fatal("ListLibrary() error = nil")
	}
}
