package titlovi

import (
	"archive/zip"
	"bytes"
	"context"
	"database/sql"
	"io"
	"net/http"
	"net/http/httptest"
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
			if query.Get("query") != "Example Show" || query.Get("season") != "1" || query.Get("episode") != "2" || query.Get("lang") != "Hrvatski" || query.Get("token") != "titlovi-token" || query.Get("userid") != "42" {
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
