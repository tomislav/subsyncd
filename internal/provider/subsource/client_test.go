package subsource

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"subsyncd/internal/domain"
	base "subsyncd/internal/provider"
	"subsyncd/internal/store"
)

const testKey = "test-subsource-key"

type recorder struct {
	mu       sync.Mutex
	requests []string
}

func (r *recorder) add(request string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.requests = append(r.requests, request)
}

func (r *recorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.requests)
}

type testEnv struct {
	client   *Client
	repo     *store.Repository
	recorder *recorder
}

// newTestEnv serves handler behind credential assertions that apply to every
// request any adapter test makes.
func newTestEnv(t *testing.T, config Config, handler http.HandlerFunc) testEnv {
	t.Helper()
	rec := &recorder{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.String(), testKey) || r.URL.Query().Has("api_key") {
			t.Errorf("request URL carries the API key: %s", r.URL.Path)
		}
		if r.Header.Get("X-API-Key") != testKey {
			t.Errorf("request %s lacks the API key header", r.URL.Path)
		}
		rec.add(r.URL.Path + "?" + r.URL.RawQuery)
		handler(w, r)
	}))
	t.Cleanup(server.Close)
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "provider.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	clock := base.SystemClock{}
	gate := base.NewGate(db.Repository(), clock, 1)
	gate.Configure("subsource-test", 1000, 100, 1, "subsource")
	config.APIKey = testKey
	config.BaseURL = server.URL
	config.AllowInsecureForTests = true
	client, err := New(config, base.Client{HTTP: server.Client(), Gate: gate, Clock: clock, ProviderID: "subsource-test", ProviderType: "subsource"}, clock)
	if err != nil {
		t.Fatal(err)
	}
	return testEnv{client: client, repo: db.Repository(), recorder: rec}
}

func movieQuery() base.SearchQuery {
	return base.SearchQuery{Mode: base.SearchBroad, Language: "en", Media: domain.Media{Ref: domain.MediaRef{Kind: domain.MediaMovie}, Title: "Example Movie", Year: 2025, ExternalIDs: domain.ExternalIDs{IMDb: "tt0000001"}}}
}

func episodeQuery(season, episode int) base.SearchQuery {
	return base.SearchQuery{Mode: base.SearchBroad, Language: "en", Media: domain.Media{Ref: domain.MediaRef{Kind: domain.MediaEpisode}, Title: "Example Show", Year: 2020, Season: season, Episode: episode, ExternalIDs: domain.ExternalIDs{IMDb: "tt0000002"}}}
}

const movieSearchJSON = `{"success":true,"data":[{"movieId":10,"subsourceLink":"https://untrusted.invalid/x","title":"Example Movie","alternateTitle":"","type":"movie","releaseYear":2025,"imdbId":"tt0000001","tmdbId":"77","season":null,"subtitleCount":2,"posters":{"small":"https://untrusted.invalid/p.jpg"}}]}`
const seriesSearchJSON = `{"success":true,"data":[{"movieId":20,"title":"Example Show","alternateTitle":"","type":"tvseries","releaseYear":2020,"imdbId":"tt0000002","tmdbId":null,"season":1,"subtitleCount":3},{"movieId":21,"title":"Example Show","type":"tvseries","releaseYear":2021,"imdbId":"tt0000002","tmdbId":null,"season":2,"subtitleCount":1},{"movieId":22,"title":"Example Show","type":"tvseries","releaseYear":2020,"imdbId":"tt0000002","tmdbId":null,"season":0,"subtitleCount":0}]}`

func listing(pages int, rows ...string) string {
	return fmt.Sprintf(`{"success":true,"data":[%s],"pagination":{"page":1,"limit":100,"total":%d,"pages":%d}}`, strings.Join(rows, ","), len(rows), pages)
}

func row(id int, language string, releases ...string) string {
	quoted := make([]string, len(releases))
	for i, release := range releases {
		quoted[i] = fmt.Sprintf("%q", release)
	}
	return fmt.Sprintf(`{"subtitleId":%d,"movieId":10,"language":%q,"releaseInfo":[%s],"commentary":"","files":null,"size":null,"hearingImpaired":false,"foreignParts":false,"framerate":null,"productionType":null,"releaseType":null,"downloads":120,"comments":0,"rating":{"good":9,"bad":1,"total":10},"preview":"1\n00:00:01,000 --> 00:00:02,000\nPreview.","uploaderId":5,"contributors":[{"id":5,"displayname":"Example"}],"link":"/subtitle/x","createdAt":"2025-01-01T00:00:00.000Z"}`, id, language, strings.Join(quoted, ","))
}

func TestSearchMovieUsesResolvedIdentity(t *testing.T) {
	env := newTestEnv(t, Config{}, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/movies/search" && r.URL.RawQuery == "imdb=tt0000001&searchType=imdb&type=movie":
			io.WriteString(w, movieSearchJSON)
		case r.URL.Path == "/subtitles" && r.URL.RawQuery == "language=english&limit=100&movieId=10&page=1&sort=popular":
			io.WriteString(w, listing(1, row(501, "english", " Example.Movie.2025.1080p.WEB-DL-GRP ", "example-movie_english-501")))
		default:
			t.Errorf("unexpected request %s?%s", r.URL.Path, r.URL.RawQuery)
			http.NotFound(w, r)
		}
	})
	got, err := env.client.Search(context.Background(), movieQuery())
	if err != nil || len(got) != 1 {
		t.Fatalf("search = %#v, %v", got, err)
	}
	c := got[0]
	if c.ProviderID != "subsource-test" || c.ResultID != "501" || c.DownloadRef != "501" || c.Kind != domain.MediaMovie || c.Language != "en" {
		t.Fatalf("wrong reference: %#v", c)
	}
	if c.Title != "Example Movie" || c.Year != 2025 || c.ExternalIDs.IMDb != "tt0000001" || c.ExternalIDs.TMDB != 77 || c.EvidenceVersion != evidenceVersion {
		t.Fatalf("wrong identity: %#v", c)
	}
	if len(c.ReleaseNames) != 1 || c.ReleaseNames[0] != "Example.Movie.2025.1080p.WEB-DL-GRP" {
		t.Fatalf("release names = %#v", c.ReleaseNames)
	}
	if c.DownloadCount != 120 || c.Popularity != base.NormalizePopularity(120) || c.HearingImpaired || c.Forced {
		t.Fatalf("wrong signals: %#v", c)
	}
}

func TestSearchEpisodesOfOneSeasonShareRequests(t *testing.T) {
	env := newTestEnv(t, Config{}, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/movies/search" && r.URL.Query().Get("type") == "series":
			io.WriteString(w, seriesSearchJSON)
		case r.URL.Path == "/subtitles" && r.URL.Query().Get("movieId") == "20":
			io.WriteString(w, listing(1, row(601, "english", "Example.Show.S01E01.1080p.WEB-GRP"), row(602, "english", "Example.Show.S01E02.1080p.WEB-GRP")))
		default:
			t.Errorf("unexpected request %s?%s", r.URL.Path, r.URL.RawQuery)
			http.NotFound(w, r)
		}
	})
	first, err := env.client.Search(context.Background(), episodeQuery(1, 1))
	if err != nil || len(first) != 1 || first[0].ResultID != "601" || first[0].Season != 1 || first[0].Episode != 1 {
		t.Fatalf("first = %#v, %v", first, err)
	}
	if first[0].Year != 0 {
		t.Fatalf("episode candidates must not carry a per-season year: %#v", first[0])
	}
	second, err := env.client.Search(context.Background(), episodeQuery(1, 2))
	if err != nil || len(second) != 1 || second[0].ResultID != "602" {
		t.Fatalf("second = %#v, %v", second, err)
	}
	if env.recorder.count() != 2 {
		t.Fatalf("requests = %v; want one search and one listing", env.recorder.requests)
	}
}

func TestSearchEpisodeWithoutMatchingSeasonListsNothing(t *testing.T) {
	env := newTestEnv(t, Config{}, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/movies/search" {
			t.Errorf("unexpected request %s", r.URL.Path)
		}
		io.WriteString(w, seriesSearchJSON)
	})
	for range 2 {
		got, err := env.client.Search(context.Background(), episodeQuery(3, 1))
		if err != nil || len(got) != 0 {
			t.Fatalf("search = %#v, %v", got, err)
		}
	}
	if env.recorder.count() != 1 {
		t.Fatalf("requests = %v; the empty answer should be cached", env.recorder.requests)
	}
}

func TestSearchRejectsConflictingIMDb(t *testing.T) {
	env := newTestEnv(t, Config{}, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/movies/search" {
			t.Errorf("unexpected request %s", r.URL.Path)
		}
		io.WriteString(w, strings.Replace(movieSearchJSON, "tt0000001", "tt0000009", 1))
	})
	got, err := env.client.Search(context.Background(), movieQuery())
	if err != nil || len(got) != 0 {
		t.Fatalf("search = %#v, %v", got, err)
	}
}

func TestSearchTitleFallbackRequiresUniqueExactMatch(t *testing.T) {
	entries := map[string]string{
		"exact":     `{"movieId":10,"title":"Example: Movie","type":"movie","releaseYear":2025,"imdbId":"tt0000001","season":null}`,
		"other":     `{"movieId":11,"title":"Example Movie Returns","type":"movie","releaseYear":2025,"imdbId":"tt0000003","season":null}`,
		"duplicate": `{"movieId":12,"title":"Example Movie","type":"movie","releaseYear":2025,"imdbId":"tt0000004","season":null}`,
		"wrongyear": `{"movieId":13,"title":"Example Movie","type":"movie","releaseYear":2019,"imdbId":"tt0000005","season":null}`,
	}
	for name, test := range map[string]struct {
		entries []string
		want    string
	}{
		"unique exact":    {[]string{entries["exact"], entries["other"]}, "10"},
		"ambiguous":       {[]string{entries["exact"], entries["duplicate"]}, ""},
		"no exact":        {[]string{entries["other"]}, ""},
		"year must match": {[]string{entries["wrongyear"]}, ""},
	} {
		t.Run(name, func(t *testing.T) {
			env := newTestEnv(t, Config{}, func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/movies/search":
					if r.URL.RawQuery != "q=Example+Movie&searchType=text&type=movie&year=2025" {
						t.Errorf("text search query = %s", r.URL.RawQuery)
					}
					fmt.Fprintf(w, `{"success":true,"data":[%s]}`, strings.Join(test.entries, ","))
				case "/subtitles":
					if r.URL.Query().Get("movieId") != test.want {
						t.Errorf("listed movie %s; want %q", r.URL.Query().Get("movieId"), test.want)
					}
					io.WriteString(w, listing(1, row(501, "english", "Example.Movie.2025.1080p.WEB-DL-GRP")))
				}
			})
			query := movieQuery()
			query.Media.ExternalIDs = domain.ExternalIDs{}
			got, err := env.client.Search(context.Background(), query)
			if err != nil || (test.want == "") != (len(got) == 0) {
				t.Fatalf("search = %#v, %v", got, err)
			}
		})
	}
}

func TestSearchPaginatesUpToMaxPages(t *testing.T) {
	env := newTestEnv(t, Config{MaxPages: 2}, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/movies/search":
			io.WriteString(w, movieSearchJSON)
		case "/subtitles":
			page := r.URL.Query().Get("page")
			id := 500
			fmt.Sscan(page, &id)
			io.WriteString(w, listing(4, row(500+id, "english", "Example.Movie.2025.1080p.WEB-DL-GRP"+page)))
		}
	})
	got, err := env.client.Search(context.Background(), movieQuery())
	if err != nil || len(got) != 2 {
		t.Fatalf("search = %#v, %v", got, err)
	}
	if env.recorder.count() != 3 {
		t.Fatalf("requests = %v; want one search and two pages", env.recorder.requests)
	}
}

func TestSearchNotFoundIsEmpty(t *testing.T) {
	env := newTestEnv(t, Config{}, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/movies/search" {
			io.WriteString(w, movieSearchJSON)
			return
		}
		http.NotFound(w, r)
	})
	got, err := env.client.Search(context.Background(), movieQuery())
	if err != nil || len(got) != 0 {
		t.Fatalf("search = %#v, %v", got, err)
	}
}

func TestSearchFailuresAreNotCached(t *testing.T) {
	fail := true
	env := newTestEnv(t, Config{}, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/movies/search":
			io.WriteString(w, movieSearchJSON)
		case "/subtitles":
			if fail {
				io.WriteString(w, `{"success":false,"error":"temporary"}`)
				return
			}
			io.WriteString(w, listing(1, row(501, "english", "Example.Movie.2025.1080p.WEB-DL-GRP")))
		}
	})
	_, err := env.client.Search(context.Background(), movieQuery())
	var invalid *base.InvalidPayloadError
	if !errors.As(err, &invalid) {
		t.Fatalf("err = %v; want invalid payload", err)
	}
	fail = false
	got, err := env.client.Search(context.Background(), movieQuery())
	if err != nil || len(got) != 1 {
		t.Fatalf("retry = %#v, %v", got, err)
	}
}

func TestSearchCanceledIsNotCached(t *testing.T) {
	env := newTestEnv(t, Config{}, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/movies/search":
			io.WriteString(w, movieSearchJSON)
		case "/subtitles":
			io.WriteString(w, listing(1, row(501, "english", "Example.Movie.2025.1080p.WEB-DL-GRP")))
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := env.client.Search(ctx, movieQuery()); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v; want cancellation", err)
	}
	got, err := env.client.Search(context.Background(), movieQuery())
	if err != nil || len(got) != 1 {
		t.Fatalf("search = %#v, %v", got, err)
	}
}

func TestSearchSharedLanguageListsEverySlug(t *testing.T) {
	env := newTestEnv(t, Config{}, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/movies/search":
			io.WriteString(w, movieSearchJSON)
		case "/subtitles":
			slug := r.URL.Query().Get("language")
			id := map[string]int{"sinhala": 701, "sinhalese": 702}[slug]
			io.WriteString(w, listing(1, row(id, slug, "Example.Movie.2025.1080p.WEB-DL-GRP")))
		}
	})
	query := movieQuery()
	query.Language = "si"
	got, err := env.client.Search(context.Background(), query)
	if err != nil || len(got) != 2 || got[0].Language != "si" || got[1].Language != "si" {
		t.Fatalf("search = %#v, %v", got, err)
	}
}

func TestSearchRejectsExactHashAndUnsupportedLanguage(t *testing.T) {
	env := newTestEnv(t, Config{}, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request %s", r.URL.Path)
	})
	query := movieQuery()
	query.Mode = base.SearchExactHash
	if _, err := env.client.Search(context.Background(), query); err == nil {
		t.Fatal("exact-hash search should be rejected")
	}
	query = movieQuery()
	query.Language = "tlh"
	if _, err := env.client.Search(context.Background(), query); err == nil {
		t.Fatal("unsupported language should be rejected")
	}
	if !env.client.SupportsLanguage("hr") || env.client.SupportsLanguage("tlh") {
		t.Fatal("SupportsLanguage disagrees with the language table")
	}
}

func TestSearchNotFoundIsNotCached(t *testing.T) {
	missing := true
	env := newTestEnv(t, Config{}, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case missing:
			http.NotFound(w, r)
		case r.URL.Path == "/movies/search":
			io.WriteString(w, movieSearchJSON)
		default:
			io.WriteString(w, listing(1, row(501, "english", "Example.Movie.2025.1080p.WEB-DL-GRP")))
		}
	})
	if got, err := env.client.Search(context.Background(), movieQuery()); err != nil || len(got) != 0 {
		t.Fatalf("search = %#v, %v", got, err)
	}
	missing = false
	got, err := env.client.Search(context.Background(), movieQuery())
	if err != nil || len(got) != 1 {
		t.Fatalf("a 404 answer was cached: %#v, %v", got, err)
	}
}

func TestSearchListingNotFoundIsNotCached(t *testing.T) {
	missing := true
	env := newTestEnv(t, Config{}, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/movies/search":
			io.WriteString(w, movieSearchJSON)
		case missing:
			http.NotFound(w, r)
		default:
			io.WriteString(w, listing(1, row(501, "english", "Example.Movie.2025.1080p.WEB-DL-GRP")))
		}
	})
	if got, err := env.client.Search(context.Background(), movieQuery()); err != nil || len(got) != 0 {
		t.Fatalf("search = %#v, %v", got, err)
	}
	missing = false
	got, err := env.client.Search(context.Background(), movieQuery())
	if err != nil || len(got) != 1 {
		t.Fatalf("a 404 listing was cached: %#v, %v", got, err)
	}
}

func TestConcurrentSearchesOfOneSeasonShareRequests(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	env := newTestEnv(t, Config{}, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/movies/search":
			<-release
			io.WriteString(w, seriesSearchJSON)
		case "/subtitles":
			io.WriteString(w, listing(1, row(601, "english", "Example.Show.S01E01.1080p.WEB-GRP"), row(602, "english", "Example.Show.S01E02.1080p.WEB-GRP")))
		}
	})
	// Cleanups run last-in first-out: release the handler before the server
	// closes, so a failed wait cannot hang shutdown.
	t.Cleanup(unblock)
	var wait sync.WaitGroup
	errs := make(chan error, 4)
	for episode := 1; episode <= 4; episode++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			_, err := env.client.Search(context.Background(), episodeQuery(1, episode))
			errs <- err
		}()
	}
	waitForWaiters(t, env.client.titles, 3)
	unblock()
	wait.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if env.recorder.count() != 2 {
		t.Fatalf("requests = %v; want one search and one listing", env.recorder.requests)
	}
}

func waitForWaiters[V any](t *testing.T, cache *ttlCache[V], want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for cache.waiting() < want {
		if time.Now().After(deadline) {
			t.Fatalf("waiters = %d; want %d", cache.waiting(), want)
		}
		time.Sleep(time.Millisecond)
	}
}
