package titlovi

import (
	"archive/zip"
	"bytes"
	"context"
	"database/sql"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
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

func TestSearchAuthenticatesPaginatesAndNormalizesEpisodeAndSeasonPack(t *testing.T) {
	login := readFixture(t, "login.json")
	pageOne := readFixture(t, "search_page_1.json")
	pageTwo := readFixture(t, "search_page_2.json")
	searchCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/subtitles/gettoken":
			if r.URL.Query().Get("username") != "user" || r.URL.Query().Get("password") != "pass" {
				t.Errorf("login query missing credentials")
			}
			w.Write(login)
		case "/api/subtitles/search":
			searchCalls++
			query := r.URL.Query()
			if query.Get("query") != "Example Show" || query.Get("season") != "1" || !reflect.DeepEqual(query["episode"], []string{"0", "2"}) || query.Get("lang") != "Hrvatski" || query.Get("token") != "titlovi-token" || query.Get("userid") != "42" {
				t.Errorf("search query = %s", r.URL.RawQuery)
			}
			if query.Get("pg") == "2" {
				w.Write(pageTwo)
			} else {
				w.Write(pageOne)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client := newTestClient(t, server, 1<<20)
	candidates, err := client.Search(context.Background(), baseprovider.SearchQuery{Media: episodeMedia(), Language: "hr", Mode: baseprovider.SearchBroad})
	if err != nil {
		t.Fatal(err)
	}
	if searchCalls != 2 || len(candidates) != 2 {
		t.Fatalf("calls/candidates = %d/%#v", searchCalls, candidates)
	}
	if candidates[0].ResultID != "101" || candidates[0].Episode != 2 || candidates[0].Rating != 0.9 || candidates[0].DownloadRef != "/download/101" {
		t.Fatalf("episode candidate = %#v", candidates[0])
	}
	pack := candidates[1]
	if pack.Episode != 0 || pack.Pack == nil || pack.Pack.Scope != domain.PackSeason || pack.Pack.Season != 1 {
		t.Fatalf("season-pack candidate = %#v", pack)
	}
	if candidates[0].Title != "Example Show" || len(candidates[0].ReleaseNames) != 1 {
		t.Fatalf("title/release normalization = %#v", candidates[0])
	}
}

func TestSearchRejectsExactModeMalformedPayloadAndRefreshesOnce(t *testing.T) {
	loginCalls, searchCalls := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/subtitles/gettoken":
			loginCalls++
			io.WriteString(w, `{"Token":"token-`+strconv.Itoa(loginCalls)+`","UserId":42,"ExpirationDate":"2026-09-04T14:00:00Z"}`)
		case "/api/subtitles/search":
			searchCalls++
			if searchCalls == 1 {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			if searchCalls == 2 {
				io.WriteString(w, `{not json`)
			}
		}
	}))
	defer server.Close()
	client := newTestClient(t, server, 1<<20)
	if _, err := client.Search(context.Background(), baseprovider.SearchQuery{Media: episodeMedia(), Language: "hr", Mode: baseprovider.SearchExactHash}); err == nil {
		t.Fatal("exact mode should be rejected")
	}
	_, err := client.Search(context.Background(), baseprovider.SearchQuery{Media: episodeMedia(), Language: "hr", Mode: baseprovider.SearchBroad})
	if _, ok := err.(*baseprovider.InvalidPayloadError); !ok || loginCalls != 2 || searchCalls != 2 {
		t.Fatalf("error/calls = %T %v / %d/%d", err, err, loginCalls, searchCalls)
	}
}

func TestSearchReturnsTypedCooldownOn429(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/subtitles/gettoken" {
			io.WriteString(w, `{"Token":"token","UserId":42,"ExpirationDate":"2026-09-04T14:00:00Z"}`)
			return
		}
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()
	client := newTestClient(t, server, 1<<20)
	_, err := client.Search(context.Background(), baseprovider.SearchQuery{Media: episodeMedia(), Language: "hr", Mode: baseprovider.SearchBroad})
	if _, ok := err.(*baseprovider.CooldownError); !ok {
		t.Fatalf("error = %T %v", err, err)
	}
}

func TestDownloadStreamsZipAndRARBytesAndRejectsExternalRedirect(t *testing.T) {
	zipBytes := makeZip(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/subtitles/gettoken":
			io.WriteString(w, `{"Token":"token","UserId":42,"ExpirationDate":"2026-09-04T14:00:00Z"}`)
		case "/download/zip":
			w.Header().Set("Content-Type", "application/zip")
			w.Write(zipBytes)
		case "/download/rar":
			w.Header().Set("Content-Type", "application/vnd.rar")
			w.Write([]byte("Rar!\x1a\x07\x01\x00fake"))
		case "/download/redirect":
			http.Redirect(w, r, "https://evil.example/file", http.StatusFound)
		}
	}))
	defer server.Close()
	client := newTestClient(t, server, 1<<20)
	for _, path := range []string{"/download/zip", "/download/rar"} {
		var output bytes.Buffer
		if _, err := client.Download(context.Background(), domain.Candidate{DownloadRef: path}, &output); err != nil || output.Len() == 0 {
			t.Fatalf("download %s = %d bytes, %v", path, output.Len(), err)
		}
	}
	if _, err := client.Download(context.Background(), domain.Candidate{DownloadRef: "/download/redirect"}, io.Discard); err == nil {
		t.Fatal("external redirect should fail")
	}
}

func newTestClient(t *testing.T, server *httptest.Server, maxBytes int64) *Client {
	t.Helper()
	clock := testutil.NewClock(time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC))
	states := &testStateStore{states: map[string]store.ProviderState{}}
	gate := baseprovider.NewGate(states, clock, 1)
	gate.Configure("titlovi-main", 1000, 10, 1)
	transport := baseprovider.Client{HTTP: server.Client(), Gate: gate, Clock: clock, ProviderID: "titlovi-main", ProviderType: "titlovi"}
	client, err := New(Config{Username: "user", Password: "pass", APIBaseURL: server.URL + "/api/subtitles", DownloadBaseURL: server.URL, DownloadHosts: []string{server.Listener.Addr().String()}, MaxDownloadBytes: maxBytes, MaxPages: 5, AllowInsecureForTests: true}, transport, clock)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func episodeMedia() domain.Media {
	return domain.Media{Ref: domain.MediaRef{Instance: "sonarr", Kind: domain.MediaEpisode, FileID: 42}, Title: "Example Show", Year: 2024, Season: 1, Episode: 2, ExternalIDs: domain.ExternalIDs{IMDb: "tt1234567"}}
}

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	payload, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return payload
}

func makeZip(t *testing.T) []byte {
	t.Helper()
	var buffer bytes.Buffer
	archive := zip.NewWriter(&buffer)
	file, err := archive.Create("episode.srt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write([]byte("subtitle")); err != nil {
		t.Fatal(err)
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func TestDownloadUnauthorizedIsAnOrdinaryFailure(t *testing.T) {
	logins := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/subtitles/gettoken" {
			logins++
			io.WriteString(w, `{"Token":"token","UserId":42,"ExpirationDate":"2026-09-04T14:00:00Z"}`)
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()
	client := newTestClient(t, server, 1<<20)
	_, err := client.Download(context.Background(), domain.Candidate{DownloadRef: "/download/101"}, io.Discard)
	if err == nil || logins != 0 {
		t.Fatalf("error = %v logins = %d; want a failed download without a login", err, logins)
	}
	if _, ok := err.(*baseprovider.AuthenticationError); ok {
		t.Fatalf("error = %T; a download carries no token", err)
	}
	if err := client.CheckSearchAvailability(context.Background()); err != nil {
		t.Fatalf("search availability = %v; a download 401 must not disable the provider", err)
	}
	if err := client.CheckDownloadAvailability(context.Background()); err != nil {
		t.Fatalf("download availability = %v", err)
	}
}

func TestDownloadRedirectLoopStopsWithoutProviderCooldown(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/download/loop":
			requests++
			http.Redirect(w, r, "/download/loop", http.StatusFound)
		case "/download/external":
			http.Redirect(w, r, "https://evil.example/file", http.StatusFound)
		}
	}))
	defer server.Close()
	client := newTestClient(t, server, 1<<20)
	_, err := client.Download(context.Background(), domain.Candidate{DownloadRef: "/download/loop"}, io.Discard)
	var redirect *baseprovider.RedirectError
	if !errors.As(err, &redirect) || requests != baseprovider.MaxDownloadRedirects {
		t.Fatalf("error = %T %v after %d requests; want a redirect refusal after %d", err, err, requests, baseprovider.MaxDownloadRedirects)
	}
	_, err = client.Download(context.Background(), domain.Candidate{DownloadRef: "/download/external"}, io.Discard)
	if !errors.As(err, &redirect) {
		t.Fatalf("error = %T %v; want a redirect refusal", err, err)
	}
	if err := client.CheckDownloadAvailability(context.Background()); err != nil {
		t.Fatalf("download availability = %v; a refused redirect is not a provider outage", err)
	}
}

func TestNormalizeDropsResultsWhoseTypeContradictsTheSearchedKind(t *testing.T) {
	client := &Client{id: "titlovi"}
	movie := baseprovider.SearchQuery{Media: domain.Media{Ref: domain.MediaRef{Kind: domain.MediaMovie}, Title: "Example", Year: 1999}, Language: "hr"}
	episode := baseprovider.SearchQuery{Media: domain.Media{Ref: domain.MediaRef{Kind: domain.MediaEpisode}, Title: "Example", Season: 1, Episode: 2}, Language: "hr"}
	items := []searchItem{
		{ID: 1, Type: 1, Language: "Hrvatski", Link: "/download/1", Title: "Example Movie", Season: -1, Episode: -1},
		{ID: 2, Type: 2, Language: "Hrvatski", Link: "/download/2", Title: "Example Series", Season: 1, Episode: 2},
		{ID: 3, Type: 2, Language: "Hrvatski", Link: "/download/3", Title: "Example Series", Season: 1, Episode: 0},
		{ID: 4, Language: "Hrvatski", Link: "/download/4", Title: "Untyped", Season: 1, Episode: 2},
		{ID: 5, Type: 3, Language: "Hrvatski", Link: "/download/5", Title: "Unknown Type", Season: 1, Episode: 2},
	}
	for _, test := range []struct {
		name  string
		query baseprovider.SearchQuery
		want  []string
	}{
		{"movie search keeps movies, untyped and unknown types", movie, []string{"1", "4", "5"}},
		{"episode search keeps series, untyped and unknown types", episode, []string{"2", "3", "4", "5"}},
	} {
		var got []string
		for _, candidate := range client.normalize(test.query, items) {
			got = append(got, candidate.ResultID)
		}
		if !reflect.DeepEqual(got, test.want) {
			t.Errorf("%s: got %v, want %v", test.name, got, test.want)
		}
	}
}

func TestNormalizeDoesNotTreatSeasonZeroEpisodeZeroAsASpecialsPack(t *testing.T) {
	// Titlovi marks an all-seasons upload as season 0 with episode 0 or -1,
	// the same season number Sonarr uses for specials.
	client := &Client{id: "titlovi"}
	special := baseprovider.SearchQuery{Media: domain.Media{Ref: domain.MediaRef{Kind: domain.MediaEpisode}, Title: "Example", Season: 0, Episode: 5}, Language: "hr"}
	items := []searchItem{
		{ID: 1, Type: 2, Language: "Hrvatski", Link: "/download/1", Title: "Example", Season: 0, Episode: 0},
		{ID: 2, Type: 2, Language: "Hrvatski", Link: "/download/2", Title: "Example", Season: 0, Episode: -1},
		{ID: 3, Type: 2, Language: "Hrvatski", Link: "/download/3", Title: "Example", Season: 0, Episode: 5},
		{ID: 5, Language: "Hrvatski", Link: "/download/5", Title: "Example", Season: 0, Episode: 0},
	}
	got := client.normalize(special, items)
	if len(got) != 1 || got[0].ResultID != "3" || got[0].Pack != nil {
		t.Fatalf("special search candidates = %+v, want only the S00E05 episode result", got)
	}
	season := special
	season.Media.Season, season.Media.Episode = 1, 2
	if got := client.normalize(season, []searchItem{{ID: 4, Type: 2, Language: "Hrvatski", Link: "/download/4", Title: "Example", Season: 1, Episode: 0}}); len(got) != 1 || got[0].Pack == nil || got[0].Pack.Season != 1 {
		t.Fatalf("regular season pack = %+v, want a season 1 pack", got)
	}
}

func TestCachedAllSeasonsUploadIsNotReusedAsASpecialsPack(t *testing.T) {
	// Packs cached before season 0 episode 0 was recognised as an all-seasons
	// upload must not keep serving specials.
	client := &Client{id: "titlovi"}
	allSeasons := domain.Candidate{ProviderID: "titlovi", ResultID: "1", Kind: domain.MediaEpisode, Season: 0, Episode: 0, Pack: &domain.PackInfo{Scope: domain.PackSeason, Season: 0}}
	if client.CanReuseCachedCandidate(allSeasons) {
		t.Fatal("cached all-seasons upload reused as a season 0 pack")
	}
	for _, candidate := range []domain.Candidate{
		{ProviderID: "titlovi", ResultID: "2", Kind: domain.MediaEpisode, Season: 1, Episode: 0, Pack: &domain.PackInfo{Scope: domain.PackSeason, Season: 1}},
		{ProviderID: "titlovi", ResultID: "3", Kind: domain.MediaEpisode, Season: 0, Episode: 5},
	} {
		if !client.CanReuseCachedCandidate(candidate) {
			t.Errorf("cached candidate %s S%dE%d refused", candidate.ResultID, candidate.Season, candidate.Episode)
		}
	}
}

func TestSearchAsksTitloviToFilterByLanguageKindAndEpisode(t *testing.T) {
	// Without ignoreLangAndEpisode=false Titlovi ignores lang and episode and
	// returns every language and every episode of the season. With it, only
	// the listed episodes come back, so episode 0 keeps season packs.
	login := readFixture(t, "login.json")
	var searched []url.Values
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/subtitles/gettoken":
			w.Write(login)
		case "/api/subtitles/search":
			searched = append(searched, r.URL.Query())
			w.Write([]byte(`{"PagesAvailable":1,"SubtitleResults":[]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client := newTestClient(t, server, 1<<20)
	movie := domain.Media{Ref: domain.MediaRef{Instance: "radarr", Kind: domain.MediaMovie, FileID: 7}, Title: "Example Movie", Year: 1999, ExternalIDs: domain.ExternalIDs{IMDb: "tt7654321"}}
	special := episodeMedia()
	special.Season, special.Episode = 0, 5
	for _, media := range []domain.Media{episodeMedia(), movie, special} {
		if _, err := client.Search(context.Background(), baseprovider.SearchQuery{Media: media, Language: "hr", Mode: baseprovider.SearchBroad}); err != nil {
			t.Fatal(err)
		}
	}
	if len(searched) != 3 {
		t.Fatalf("searches = %d, want 3", len(searched))
	}
	for _, test := range []struct {
		name  string
		query url.Values
		want  map[string][]string
	}{
		{"episode", searched[0], map[string][]string{"ignoreLangAndEpisode": {"false"}, "lang": {"Hrvatski"}, "type": {"2"}, "season": {"1"}, "episode": {"0", "2"}, "imdbID": {"tt1234567"}}},
		{"movie", searched[1], map[string][]string{"ignoreLangAndEpisode": {"false"}, "lang": {"Hrvatski"}, "type": {"1"}, "season": nil, "episode": nil, "imdbID": {"tt7654321"}}},
		// Season 0 episode 0 is an all-seasons upload, which is always dropped.
		{"special", searched[2], map[string][]string{"ignoreLangAndEpisode": {"false"}, "type": {"2"}, "season": {"0"}, "episode": {"5"}}},
	} {
		for key, want := range test.want {
			if got := test.query[key]; !reflect.DeepEqual(got, want) {
				t.Errorf("%s search %s = %q, want %q", test.name, key, got, want)
			}
		}
	}
}
