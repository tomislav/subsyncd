package opensubtitles

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"subsyncd/internal/domain"
	baseprovider "subsyncd/internal/provider"
	"subsyncd/internal/store"
	"subsyncd/internal/testutil"
)

type staticHasher struct{ result FileHash }

func (h staticHasher) Hash(string) (FileHash, error) { return h.result, nil }

type countingHasher struct {
	result FileHash
	calls  int
}

func (h *countingHasher) Hash(string) (FileHash, error) {
	h.calls++
	return h.result, nil
}

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

func TestSearchAuthenticatesPaginatesAndNormalizesExactCandidates(t *testing.T) {
	pageOne := readFixture(t, "search_page_1.json")
	pageTwo := readFixture(t, "search_page_2.json")
	login := readFixture(t, "login.json")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Api-Key") != "api-key" || r.Header.Get("User-Agent") != "subsyncd-test" {
			t.Errorf("missing common headers: %#v", r.Header)
		}
		switch r.URL.Path {
		case "/api/v1/login":
			w.Write(login)
		case "/api/v1/subtitles":
			if r.Header.Get("Authorization") != "Bearer token-one" {
				t.Errorf("authorization = %q", r.Header.Get("Authorization"))
			}
			if r.URL.Query().Get("moviehash") != "0123456789abcdef" || r.URL.Query().Get("moviebytesize") != "196608" || r.URL.Query().Get("languages") != "en" {
				t.Errorf("exact query = %s", r.URL.RawQuery)
			}
			if r.URL.Query().Get("page") == "2" {
				w.Write(pageTwo)
			} else {
				w.Write(pageOne)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client := newTestClient(t, server, staticHasher{result: FileHash{MovieHash: "0123456789abcdef", ByteSize: 196608}}, 1024)
	candidates, err := client.Search(context.Background(), baseprovider.SearchQuery{Media: episodeMedia(), Language: "en", Mode: baseprovider.SearchExactHash})
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 2 || !candidates[0].ExactHash || candidates[0].ResultID != "501" || candidates[0].DownloadRef != "501" || candidates[0].Rating != 0.85 || candidates[0].ExternalIDs.IMDb != "tt1234567" || candidates[0].ExternalIDs.TMDB != 7654 || !candidates[1].HearingImpaired {
		t.Fatalf("candidates = %#v", candidates)
	}
}

func TestSearchExcludesAIAndMachineTranslationsOnEveryPage(t *testing.T) {
	for _, mode := range []baseprovider.SearchMode{baseprovider.SearchExactHash, baseprovider.SearchBroad} {
		t.Run(string(mode), func(t *testing.T) {
			pages := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v1/login":
					io.WriteString(w, `{"token":"token","expires_in":3600}`)
				case "/api/v1/subtitles":
					pages++
					for _, flag := range []string{"ai_translated", "machine_translated"} {
						if got := r.URL.Query().Get(flag); got != "exclude" {
							t.Errorf("page %s: %s = %q, want exclude", r.URL.Query().Get("page"), flag, got)
						}
					}
					io.WriteString(w, `{"total_pages":2,"data":[]}`)
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			client := newTestClient(t, server, staticHasher{result: FileHash{MovieHash: "0123456789abcdef", ByteSize: 196608}}, 1024)
			if _, err := client.Search(context.Background(), baseprovider.SearchQuery{Media: episodeMedia(), Language: "en", Mode: mode}); err != nil {
				t.Fatal(err)
			}
			if pages != 2 {
				t.Fatalf("search pages = %d, want 2", pages)
			}
		})
	}
}

func TestNormalizeCandidatesKeepsFeatureIDsOnlyForMovies(t *testing.T) {
	item := searchItem{}
	item.Attributes.Language = "en"
	item.Attributes.ForeignPartsOnly = true
	item.Attributes.FeatureDetails.IMDbID = 15169108
	item.Attributes.FeatureDetails.TMDBID = 2411002
	item.Attributes.FeatureDetails.ParentIMDbID = 13016388
	item.Attributes.FeatureDetails.ParentTMDBID = 85937
	item.Attributes.Files = append(item.Attributes.Files, struct {
		FileID   int64  `json:"file_id"`
		FileName string `json:"file_name"`
	}{FileID: 1})

	episode := normalizeCandidates("opensubtitles", baseprovider.SearchQuery{Media: domain.Media{Ref: domain.MediaRef{Kind: domain.MediaEpisode}}}, []searchItem{item})
	if len(episode) != 1 || episode[0].ExternalIDs.IMDb != "tt13016388" || episode[0].ExternalIDs.TMDB != 85937 || !episode[0].Forced {
		t.Fatalf("episode parent IDs = %#v", episode)
	}
	movie := normalizeCandidates("opensubtitles", baseprovider.SearchQuery{Media: domain.Media{Ref: domain.MediaRef{Kind: domain.MediaMovie}}}, []searchItem{item})
	if len(movie) != 1 || movie[0].ExternalIDs.IMDb != "tt15169108" || movie[0].ExternalIDs.TMDB != 2411002 {
		t.Fatalf("movie feature IDs = %#v", movie)
	}
}

func TestExactSearchReusesHashStoredForMediaFingerprint(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/login":
			io.WriteString(w, `{"token":"token","expires_in":3600}`)
		case "/api/v1/subtitles":
			io.WriteString(w, `{"total_pages":1,"data":[]}`)
		}
	}))
	defer server.Close()

	media := episodeMedia()
	media.Fingerprint.Size = 196608
	media.Fingerprint.ModTime = time.Date(2026, 9, 4, 11, 0, 0, 0, time.UTC)
	repository := openHashRepository(t)
	if _, _, err := repository.UpsertMedia(context.Background(), media); err != nil {
		t.Fatal(err)
	}
	hasher := &countingHasher{result: FileHash{MovieHash: "0123456789abcdef", ByteSize: media.Fingerprint.Size}}
	client := newTestClient(t, server, hasher, 1024, repository)
	query := baseprovider.SearchQuery{Media: media, Language: "en", Mode: baseprovider.SearchExactHash}
	if _, err := client.Search(context.Background(), query); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Search(context.Background(), query); err != nil {
		t.Fatal(err)
	}
	if hasher.calls != 1 {
		t.Fatalf("hasher calls = %d, want 1", hasher.calls)
	}
}

func TestBroadSearchUsesStrongestExternalIDAndEpisodeIdentity(t *testing.T) {
	var query url.Values
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/login":
			io.WriteString(w, `{"token":"token","expires_in":3600}`)
		case "/api/v1/subtitles":
			query = r.URL.Query()
			io.WriteString(w, `{"total_pages":1,"data":[]}`)
		}
	}))
	defer server.Close()
	client := newTestClient(t, server, staticHasher{}, 1024)
	if _, err := client.Search(context.Background(), baseprovider.SearchQuery{Media: episodeMedia(), Language: "en", Mode: baseprovider.SearchBroad}); err != nil {
		t.Fatal(err)
	}
	if query.Get("parent_imdb_id") != "1234567" || query.Get("season_number") != "1" || query.Get("episode_number") != "2" || query.Get("query") != "Example Show" || query.Get("year") != "2024" {
		t.Fatalf("broad query = %s", query.Encode())
	}
}

func TestOpenSubtitlesLanguageConversionRoundTripsCustomCodes(t *testing.T) {
	tests := []struct {
		language domain.Language
		api      string
	}{
		{"pt", "pt-PT"},
		{"zh", "zh-CN"},
		{"es-MX", "ea"},
		{"hr", "hr"},
	}
	for _, test := range tests {
		if got := openSubtitlesLanguage(test.language); got != test.api {
			t.Errorf("to API %s = %q, want %q", test.language, got, test.api)
		}
		if got, err := fromOpenSubtitlesLanguage(test.api); err != nil || got != test.language {
			t.Errorf("from API %q = %q, %v; want %s", test.api, got, err, test.language)
		}
	}
}

func TestSearchRefreshesTokenOnceAfter401(t *testing.T) {
	loginCalls, searchCalls := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/login":
			loginCalls++
			io.WriteString(w, `{"token":"token-`+strconv.Itoa(loginCalls)+`","expires_in":3600}`)
		case "/api/v1/subtitles":
			searchCalls++
			if searchCalls == 1 {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			if r.Header.Get("Authorization") != "Bearer token-2" {
				t.Errorf("retry authorization = %q", r.Header.Get("Authorization"))
			}
			io.WriteString(w, `{"total_pages":1,"data":[]}`)
		}
	}))
	defer server.Close()
	client := newTestClient(t, server, staticHasher{}, 1024)
	if _, err := client.Search(context.Background(), baseprovider.SearchQuery{Media: episodeMedia(), Language: "en", Mode: baseprovider.SearchBroad}); err != nil {
		t.Fatal(err)
	}
	if loginCalls != 2 || searchCalls != 2 {
		t.Fatalf("login/search calls = %d/%d", loginCalls, searchCalls)
	}
}

func TestRepeated401DisablesOnlyThisProviderInstance(t *testing.T) {
	loginCalls, searchCalls := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/login":
			loginCalls++
			io.WriteString(w, `{"token":"token-`+strconv.Itoa(loginCalls)+`","expires_in":3600}`)
		case "/api/v1/subtitles":
			searchCalls++
			w.WriteHeader(http.StatusUnauthorized)
		}
	}))
	defer server.Close()
	client := newTestClient(t, server, staticHasher{}, 1024)
	query := baseprovider.SearchQuery{Media: episodeMedia(), Language: "en", Mode: baseprovider.SearchBroad}
	if _, err := client.Search(context.Background(), query); err == nil {
		t.Fatal("expected repeated authentication failure")
	}
	if _, err := client.Search(context.Background(), query); err == nil {
		t.Fatal("expected disabled provider failure")
	}
	if loginCalls != 2 || searchCalls != 2 {
		t.Fatalf("disabled provider made another HTTP call: login/search = %d/%d", loginCalls, searchCalls)
	}
}

func TestDownloadRetrievesTemporaryLinkStreamsWithLimitAndPersistsQuota(t *testing.T) {
	var server *httptest.Server
	quota := false
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/login":
			io.WriteString(w, `{"token":"token","expires_in":3600}`)
		case "/api/v1/download":
			if quota {
				w.WriteHeader(http.StatusNotAcceptable)
				io.WriteString(w, `{"message":"download limit","reset_time_utc":"2026-09-04T18:00:00Z"}`)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"link": server.URL + "/temporary/file", "file_name": "episode.srt"})
		case "/temporary/file":
			io.WriteString(w, "subtitle")
		}
	}))
	defer server.Close()
	client := newTestClient(t, server, staticHasher{}, 32)
	var output bytes.Buffer
	metadata, err := client.Download(context.Background(), domain.Candidate{DownloadRef: "501"}, &output)
	if err != nil || output.String() != "subtitle" || metadata.Filename != "episode.srt" {
		t.Fatalf("download = %#v/%q/%v", metadata, output.String(), err)
	}
	quota = true
	_, err = client.Download(context.Background(), domain.Candidate{DownloadRef: "501"}, io.Discard)
	if _, ok := err.(*baseprovider.QuotaError); !ok {
		t.Fatalf("quota error = %T %v", err, err)
	}
}

func TestDownloadRefreshesTokenOnlyOnceAfter401(t *testing.T) {
	loginCalls, downloadCalls := 0, 0
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/login":
			loginCalls++
			io.WriteString(w, `{"token":"token-`+strconv.Itoa(loginCalls)+`","expires_in":3600}`)
		case "/api/v1/download":
			downloadCalls++
			if downloadCalls == 1 {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			if r.Header.Get("Authorization") != "Bearer token-2" {
				t.Errorf("retry authorization = %q", r.Header.Get("Authorization"))
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"link": server.URL + "/file", "file_name": "episode.srt"})
		case "/file":
			io.WriteString(w, "subtitle")
		}
	}))
	defer server.Close()
	client := newTestClient(t, server, staticHasher{}, 32)
	if _, err := client.Download(context.Background(), domain.Candidate{DownloadRef: "501"}, io.Discard); err != nil {
		t.Fatal(err)
	}
	if loginCalls != 2 || downloadCalls != 2 {
		t.Fatalf("login/download calls = %d/%d", loginCalls, downloadCalls)
	}
}

func TestSearchRejectsMalformedJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/login" {
			io.WriteString(w, `{"token":"token"}`)
			return
		}
		io.WriteString(w, `{not json`)
	}))
	defer server.Close()
	client := newTestClient(t, server, staticHasher{}, 1024)
	_, err := client.Search(context.Background(), baseprovider.SearchQuery{Media: episodeMedia(), Language: "en", Mode: baseprovider.SearchBroad})
	if _, ok := err.(*baseprovider.InvalidPayloadError); !ok {
		t.Fatalf("error = %T %v", err, err)
	}
}

func newTestClient(t *testing.T, server *httptest.Server, hasher Hasher, maxBytes int64, caches ...HashCache) *Client {
	t.Helper()
	clock := testutil.NewClock(time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC))
	states := &testStateStore{states: map[string]store.ProviderState{}}
	gate := baseprovider.NewGate(states, clock, 1)
	gate.Configure("opensubtitles-main", 1000, 10, 1)
	transport := baseprovider.Client{HTTP: server.Client(), Gate: gate, Clock: clock, ProviderID: "opensubtitles-main", ProviderType: "opensubtitles"}
	var cache HashCache
	if len(caches) != 0 {
		cache = caches[0]
	}
	client, err := New(Config{APIKey: "api-key", Username: "user", Password: "pass", UserAgent: "subsyncd-test", BaseURL: server.URL + "/api/v1", MaxDownloadBytes: maxBytes}, transport, hasher, cache, clock)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func openHashRepository(t *testing.T) *store.Repository {
	t.Helper()
	database, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "subsyncd.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	return database.Repository()
}

func episodeMedia() domain.Media {
	return domain.Media{Ref: domain.MediaRef{Instance: "sonarr", Kind: domain.MediaEpisode, FileID: 42}, EntityID: 24, Fingerprint: domain.MediaFingerprint{Path: "/media/show.mkv", FileID: 42}, Title: "Example Show", Year: 2024, Season: 1, Episode: 2, ExternalIDs: domain.ExternalIDs{IMDb: "tt1234567", TMDB: 7654}}
}

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	payload, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return payload
}

type reviewRoundTripper func(*http.Request) (*http.Response, error)

func (f reviewRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func reviewClient(t *testing.T, f reviewRoundTripper) *Client {
	clock := testutil.NewClock(time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC))
	states := &testStateStore{states: map[string]store.ProviderState{}}
	gate := baseprovider.NewGate(states, clock, 1)
	gate.Configure("review", 1000, 20, 1)
	c, err := New(Config{APIKey: "app-key", Username: "u", Password: "p", UserAgent: "review", BaseURL: "https://api.opensubtitles.com/api/v1"}, baseprovider.Client{HTTP: &http.Client{Transport: f}, Gate: gate, Clock: clock, ProviderID: "review", ProviderType: "opensubtitles"}, staticHasher{}, nil, clock)
	if err != nil {
		t.Fatal(err)
	}
	return c
}
func reviewResponse(r *http.Request, status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(bytes.NewBufferString(body)), Request: r}
}
func TestLoginRejectionDisablesInstance(t *testing.T) {
	calls := 0
	c := reviewClient(t, func(r *http.Request) (*http.Response, error) { calls++; return reviewResponse(r, 401, `{}`), nil })
	q := baseprovider.SearchQuery{Media: episodeMedia(), Language: "en", Mode: baseprovider.SearchBroad}
	for i := 0; i < 2; i++ {
		if _, err := c.Search(context.Background(), q); err == nil {
			t.Fatal("want login rejection")
		}
	}
	if calls != 1 {
		t.Fatalf("login requests = %d", calls)
	}
}
func TestSearchAdoptsReturnedVIPHost(t *testing.T) {
	var host string
	c := reviewClient(t, func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/api/v1/login" {
			return reviewResponse(r, 200, `{"token":"token","base_url":"vip-api.opensubtitles.com"}`), nil
		}
		host = r.URL.Host
		return reviewResponse(r, 200, `{"data":[],"total_pages":1}`), nil
	})
	if _, err := c.Search(context.Background(), baseprovider.SearchQuery{Media: episodeMedia(), Language: "en", Mode: baseprovider.SearchBroad}); err != nil {
		t.Fatal(err)
	}
	if host != "vip-api.opensubtitles.com" {
		t.Fatalf("host = %s", host)
	}
}
func TestIssuedDownloadLinkSurvivesExhaustedAPIQuota(t *testing.T) {
	cdnCalls := 0
	c := reviewClient(t, func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/api/v1/login":
			return reviewResponse(r, 200, `{"token":"token"}`), nil
		case "/api/v1/download":
			res := reviewResponse(r, 200, `{"link":"https://cdn.example.test/file","file_name":"sub.srt"}`)
			res.Header.Set("RateLimit", `"default";r=0;t=3600`)
			return res, nil
		default:
			cdnCalls++
			return reviewResponse(r, 200, "subtitle"), nil
		}
	})
	_, err := c.Download(context.Background(), domain.Candidate{DownloadRef: "1"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if cdnCalls != 1 {
		t.Fatal("expected issued CDN link redemption")
	}
}
func TestSignedDownloadDoesNotForwardAPIKey(t *testing.T) {
	key := ""
	c := reviewClient(t, func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/api/v1/login":
			return reviewResponse(r, 200, `{"token":"token"}`), nil
		case "/api/v1/download":
			return reviewResponse(r, 200, `{"link":"https://unrelated.example.test/file","file_name":"sub.srt"}`), nil
		default:
			key = r.Header.Get("Api-Key")
			return reviewResponse(r, 200, "subtitle"), nil
		}
	})
	if _, err := c.Download(context.Background(), domain.Candidate{DownloadRef: "1"}, io.Discard); err != nil {
		t.Fatal(err)
	}
	if key != "" {
		t.Fatalf("key = %q", key)
	}
}

func TestReturnedAPIHostValidationAndPrivateEndpoints(t *testing.T) {
	for _, tc := range []struct{ base, returned, want string }{
		{defaultBaseURL, "vip-api.opensubtitles.com", "https://vip-api.opensubtitles.com/api/v1"},
		{defaultBaseURL, "https://api.opensubtitles.com/api/v1", defaultBaseURL},
		{defaultBaseURL, "api.opensubtitles.com.evil.test", ""},
		{defaultBaseURL, "https://evil.test/api/v1", ""},
		{defaultBaseURL, "https://user:pass@api.opensubtitles.com", ""},
		{defaultBaseURL, "https://vip-api.opensubtitles.com:444", ""},
		{defaultBaseURL, "http://vip-api.opensubtitles.com", ""},
		{defaultBaseURL, "https://vip-api.opensubtitles.com/other", ""},
		{defaultBaseURL, "https://vip-api.opensubtitles.com?key=secret", ""},
		{"http://localhost:1234/api/v1", "api.opensubtitles.com", "http://localhost:1234/api/v1"},
		{"http://localhost:1234/api/v1", "http://localhost:1234/api/v1", "http://localhost:1234/api/v1"},
		{"https://private.example/api/v1", "evil.test", ""},
	} {
		t.Run(tc.base+"/"+tc.returned, func(t *testing.T) {
			c := &Client{config: Config{BaseURL: tc.base}}
			got, err := c.returnedAPIBase(tc.returned)
			if tc.want == "" {
				if err == nil {
					t.Fatalf("unsafe returned host accepted: %s", got)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("host=%s err=%v want=%s", got, err, tc.want)
			}
		})
	}
}

type failingSearchBody struct{}

func (failingSearchBody) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }
func (failingSearchBody) Close() error             { return nil }

func TestSearchBodyFailureRemainsTechnicalAndSuccessfulJSONResetsCircuit(t *testing.T) {
	failing := true
	c := reviewClient(t, func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/api/v1/login" {
			return reviewResponse(r, 200, `{"token":"token"}`), nil
		}
		response := reviewResponse(r, 200, `{"total_pages":1,"data":[]}`)
		if failing {
			response.Body = failingSearchBody{}
		}
		return response, nil
	})
	q := baseprovider.SearchQuery{Media: episodeMedia(), Language: "en", Mode: baseprovider.SearchBroad}
	_, err := c.Search(context.Background(), q)
	var cooldown *baseprovider.CooldownError
	var invalid *baseprovider.InvalidPayloadError
	if !errors.As(err, &cooldown) || errors.As(err, &invalid) {
		t.Fatalf("body transport error misclassified: %T %v", err, err)
	}
	c.clock.(*testutil.Clock).Advance(time.Minute + time.Second)
	failing = false
	if _, err := c.Search(context.Background(), q); err != nil {
		t.Fatal(err)
	}
	failing = true
	_, err = c.Search(context.Background(), q)
	if !errors.As(err, &cooldown) || !cooldown.ResetAt.Equal(c.clock.Now().Add(time.Minute)) {
		t.Fatalf("successful JSON did not reset circuit: %v", err)
	}
}
