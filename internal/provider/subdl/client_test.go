package subdl

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"subsyncd/internal/domain"
	baseprovider "subsyncd/internal/provider"
	"subsyncd/internal/store"
	"subsyncd/internal/testutil"
)

type testStateStore struct {
	mu     sync.Mutex
	states map[string]store.ProviderState
}

func (s *testStateStore) GetProviderState(_ context.Context, providerID, scope string) (store.ProviderState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, ok := s.states[providerID+"/"+scope]
	if !ok {
		return store.ProviderState{}, sql.ErrNoRows
	}
	return state, nil
}

func (s *testStateStore) PutProviderState(_ context.Context, state store.ProviderState) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.states[state.ProviderID+"/"+state.Scope] = state
	return nil
}

func TestEpisodeSearchRunsFallbacksDeduplicatesAndPrefersDirectMember(t *testing.T) {
	var queries []url.Values
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		queries = append(queries, r.URL.Query())
		io.WriteString(w, `{"status":true,"subtitles":[{"name":"pack.zip","url":"/subtitle/pack.zip","language":"EN","season":1,"episode":1,"episode_from":1,"episode_end":10,"hi":true,"releases":["Show.S01.1080p.WEB-DL-GROUP","Show alternate"],"unpack_files":[{"file_n_id":"file-2","name":"Show.S01E02.srt","release_name":"Show.S01E02.WEB-DL-GROUP","season":1,"episode":2,"language":"EN","hi":true,"url":"/subtitle/pack/file-2"}]}]}`)
	}))
	defer server.Close()

	client := newTestClient(t, server, 1<<20)
	media := episodeMedia()
	media.AbsoluteEpisode = 102
	candidates, err := client.Search(context.Background(), baseprovider.SearchQuery{Media: media, Language: "en", Mode: baseprovider.SearchBroad})
	if err != nil {
		t.Fatal(err)
	}
	if len(queries) != 3 {
		t.Fatalf("query count = %d, want standard, absolute, and season-only", len(queries))
	}
	if queries[0].Get("season_number") != "1" || queries[0].Get("episode_number") != "2" || queries[1].Get("episode_number") != "102" || queries[1].Has("season_number") || queries[2].Has("episode_number") {
		t.Fatalf("fallback queries = %#v", queries)
	}
	for _, key := range []string{"api_key", "imdb_id", "file_name", "type", "year", "languages", "releases", "hi", "unpack", "subs_per_page", "client"} {
		if queries[0].Get(key) == "" {
			t.Errorf("standard query missing %s: %s", key, queries[0].Encode())
		}
	}
	if len(candidates) != 1 || candidates[0].ResultID != "/subtitle/pack.zip:file-2" || candidates[0].DownloadRef != "/subtitle/pack/file-2" || candidates[0].Pack != nil || candidates[0].Episode != 2 || !candidates[0].HearingImpaired || len(candidates[0].ReleaseNames) != 3 {
		t.Fatalf("direct candidate = %#v", candidates)
	}
}

func TestEpisodeSearchUsesTitleFallbackAndAcceptsOnlyContainingPacks(t *testing.T) {
	searchCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		searchCalls++
		if searchCalls < 3 {
			io.WriteString(w, `{"status":true,"subtitles":[]}`)
			return
		}
		if r.URL.Query().Has("season_number") || r.URL.Query().Has("episode_number") || r.URL.Query().Get("film_name") != "Example Show" {
			t.Errorf("title fallback = %s", r.URL.RawQuery)
		}
		io.WriteString(w, `{"status":true,"subtitles":[{"name":"good.zip","url":"/subtitle/good.zip","language":"EN","season":1,"episode":0,"releases":["Show.S01E01-E10"]},{"name":"bad.zip","url":"/subtitle/bad.zip","language":"EN","season":1,"episode":0,"episode_from":11,"episode_end":20,"releases":["Show.S01E11-E20"]}]}`)
	}))
	defer server.Close()
	client := newTestClient(t, server, 1<<20)
	candidates, err := client.Search(context.Background(), baseprovider.SearchQuery{Media: episodeMedia(), Language: "en", Mode: baseprovider.SearchBroad})
	if err != nil {
		t.Fatal(err)
	}
	if searchCalls != 3 || len(candidates) != 1 || candidates[0].Pack == nil || candidates[0].Pack.Scope != domain.PackRange || candidates[0].Pack.EpisodeFrom != 1 || candidates[0].Pack.EpisodeTo != 10 {
		t.Fatalf("calls/candidates = %d/%#v", searchCalls, candidates)
	}
}

func TestFullSeasonResultWithoutRangeBecomesSeasonPack(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"status":true,"subtitles":[{"name":"season.zip","url":"/subtitle/season.zip","language":"EN","season":1,"episode":0,"full_season":true,"releases":["Show.S01.COMPLETE"]}]}`)
	}))
	defer server.Close()
	client := newTestClient(t, server, 1<<20)
	candidates, err := client.Search(context.Background(), baseprovider.SearchQuery{Media: episodeMedia(), Language: "en", Mode: baseprovider.SearchBroad})
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 || candidates[0].Pack == nil || candidates[0].Pack.Scope != domain.PackSeason || candidates[0].Pack.Season != 1 {
		t.Fatalf("season candidate = %#v", candidates)
	}
}

func TestMovieSearchPrefersIMDbAndExactModeIsRejected(t *testing.T) {
	var query url.Values
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query = r.URL.Query()
		io.WriteString(w, `{"status":true,"results":[{"imdb_id":"tt123","tmdb_id":456,"name":"Movie","year":2025,"type":"movie"}],"subtitles":[{"name":"movie.zip","url":"/subtitle/movie.zip","language":"HR","releases":["Movie.2025.1080p.WEB-DL-GROUP"]}]}`)
	}))
	defer server.Close()
	client := newTestClient(t, server, 1<<20)
	media := domain.Media{Ref: domain.MediaRef{Kind: domain.MediaMovie}, Title: "Movie", Year: 2025, OriginalFilename: "Movie.2025.mkv", ExternalIDs: domain.ExternalIDs{IMDb: "tt123", TMDB: 456}}
	candidates, err := client.Search(context.Background(), baseprovider.SearchQuery{Media: media, Language: "hr", Mode: baseprovider.SearchBroad})
	if err != nil {
		t.Fatal(err)
	}
	if query.Get("imdb_id") != "tt123" || query.Has("tmdb_id") || query.Get("type") != "movie" || query.Get("languages") != "HR" {
		t.Fatalf("movie query = %s", query.Encode())
	}
	if len(candidates) != 1 || candidates[0].ExternalIDs.IMDb != "tt123" || candidates[0].ExternalIDs.TMDB != 456 || candidates[0].Title != "Movie" || candidates[0].Year != 2025 {
		t.Fatalf("movie identity = %#v", candidates)
	}
	if _, err := client.Search(context.Background(), baseprovider.SearchQuery{Media: media, Language: "hr", Mode: baseprovider.SearchExactHash}); err == nil {
		t.Fatal("exact mode should fail")
	}
}

func TestTypedRemoteFailuresAndRedactedAPIKey(t *testing.T) {
	response := `{"status":false,"error":"daily_limit"}`
	status := http.StatusTooManyRequests
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		io.WriteString(w, response)
	}))
	defer server.Close()
	client := newTestClient(t, server, 1<<20)
	query := baseprovider.SearchQuery{Media: episodeMedia(), Language: "en", Mode: baseprovider.SearchBroad}
	if _, err := client.Search(context.Background(), query); func() bool { _, ok := err.(*baseprovider.QuotaError); return ok }() == false {
		t.Fatalf("daily limit error = %T %v", err, err)
	}
	response = `{"status":false,"error":"service_busy"}`
	client = newTestClient(t, server, 1<<20)
	if _, err := client.Search(context.Background(), query); func() bool { _, ok := err.(*baseprovider.CooldownError); return ok }() == false {
		t.Fatalf("service busy error = %T %v", err, err)
	}
	status = http.StatusOK
	response = `{not json`
	client = newTestClient(t, server, 1<<20)
	if _, err := client.Search(context.Background(), query); func() bool { _, ok := err.(*baseprovider.InvalidPayloadError); return ok }() == false || bytes.Contains([]byte(err.Error()), []byte("api-key")) {
		t.Fatalf("malformed error = %T %v", err, err)
	}
}

func TestDownloadStreamsWithinLimitAndRejectsExternalRedirect(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/subtitle/good":
			io.WriteString(w, "subtitle")
		case "/subtitle/large":
			io.WriteString(w, "too large")
		case "/subtitle/redirect":
			http.Redirect(w, r, "https://evil.example/file", http.StatusFound)
		}
	}))
	defer server.Close()
	client := newTestClient(t, server, 8)
	var output bytes.Buffer
	if _, err := client.Download(context.Background(), domain.Candidate{DownloadRef: "/subtitle/good"}, &output); err != nil || output.String() != "subtitle" {
		t.Fatalf("download = %q/%v", output.String(), err)
	}
	if _, err := client.Download(context.Background(), domain.Candidate{DownloadRef: "/subtitle/large"}, io.Discard); err == nil {
		t.Fatal("oversized download should fail")
	}
	if _, err := client.Download(context.Background(), domain.Candidate{DownloadRef: "/subtitle/redirect"}, io.Discard); err == nil {
		t.Fatal("external redirect should fail")
	}
}

func TestForbiddenSearchDisablesProviderUntilExplicitRetry(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusForbidden)
	}))
	defer server.Close()
	client, states := newTestClientWithState(t, server, 1<<20)
	query := baseprovider.SearchQuery{Media: episodeMedia(), Language: "en", Mode: baseprovider.SearchBroad}
	_, err := client.Search(context.Background(), query)
	var authentication *baseprovider.AuthenticationError
	if !errors.As(err, &authentication) {
		t.Fatalf("first search error = %T %v", err, err)
	}
	state, err := states.GetProviderState(context.Background(), "subdl-main", string(baseprovider.OperationAuth))
	if err != nil || !state.Disabled {
		t.Fatalf("disabled state = %#v, %v", state, err)
	}
	_, err = client.Search(context.Background(), query)
	var disabled *baseprovider.DisabledError
	if !errors.As(err, &disabled) || calls != 1 {
		t.Fatalf("second search error/calls = %T %v/%d", err, err, calls)
	}
}

func TestForbiddenDownloadDisablesProviderUntilExplicitRetry(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusForbidden)
	}))
	defer server.Close()
	client, states := newTestClientWithState(t, server, 1<<20)
	_, err := client.Download(context.Background(), domain.Candidate{DownloadRef: "/subtitle/forbidden"}, io.Discard)
	var authentication *baseprovider.AuthenticationError
	if !errors.As(err, &authentication) {
		t.Fatalf("first download error = %T %v", err, err)
	}
	state, err := states.GetProviderState(context.Background(), "subdl-main", string(baseprovider.OperationAuth))
	if err != nil || !state.Disabled {
		t.Fatalf("disabled state = %#v, %v", state, err)
	}
	_, err = client.Download(context.Background(), domain.Candidate{DownloadRef: "/subtitle/forbidden"}, io.Discard)
	var disabled *baseprovider.DisabledError
	if !errors.As(err, &disabled) || calls != 1 {
		t.Fatalf("second download error/calls = %T %v/%d", err, err, calls)
	}
}

func newTestClient(t *testing.T, server *httptest.Server, maxBytes int64) *Client {
	t.Helper()
	client, _ := newTestClientWithState(t, server, maxBytes)
	return client
}

func newTestClientWithState(t *testing.T, server *httptest.Server, maxBytes int64) (*Client, *testStateStore) {
	t.Helper()
	clock := testutil.NewClock(time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC))
	states := &testStateStore{states: map[string]store.ProviderState{}}
	gate := baseprovider.NewGate(states, clock, 1)
	gate.Configure("subdl-main", 1000, 10, 1)
	transport := baseprovider.Client{HTTP: server.Client(), Gate: gate, Clock: clock, ProviderID: "subdl-main", ProviderType: "subdl"}
	client, err := New(Config{APIKey: "api-key", BaseURL: server.URL + "/api/v1/subtitles", DownloadBaseURL: server.URL, DownloadHosts: []string{server.Listener.Addr().String()}, MaxDownloadBytes: maxBytes, AllowInsecureForTests: true}, transport, clock)
	if err != nil {
		t.Fatal(err)
	}
	return client, states
}

func episodeMedia() domain.Media {
	return domain.Media{Ref: domain.MediaRef{Kind: domain.MediaEpisode}, Title: "Example Show", Year: 2024, Season: 1, Episode: 2, OriginalFilename: "Example.Show.S01E02.mkv", ExternalIDs: domain.ExternalIDs{IMDb: "tt1234567", TMDB: 7654}}
}
