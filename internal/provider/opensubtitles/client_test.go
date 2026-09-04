package opensubtitles

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
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
	return domain.Media{Ref: domain.MediaRef{Instance: "sonarr", Kind: domain.MediaEpisode, FileID: 42}, Fingerprint: domain.MediaFingerprint{Path: "/media/show.mkv", FileID: 42}, Title: "Example Show", Year: 2024, Season: 1, Episode: 2, ExternalIDs: domain.ExternalIDs{IMDb: "tt1234567", TMDB: 7654}}
}

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	payload, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return payload
}
