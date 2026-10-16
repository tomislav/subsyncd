package subdl

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"sync"
	"testing"
	"time"

	"subsyncd/internal/domain"
	"subsyncd/internal/match"
	baseprovider "subsyncd/internal/provider"
	"subsyncd/internal/store"
	"subsyncd/internal/testutil"
)

type testStateStore struct {
	mu     sync.Mutex
	states map[string]store.ProviderState
}

func TestNormalizeDoesNotInventIdentityEvidence(t *testing.T) {
	media := domain.Media{Ref: domain.MediaRef{Kind: domain.MediaEpisode}, Title: "Example Show", Year: 2020, Season: 1, Episode: 2, Source: "WEB-DL", ReleaseGroup: "GROUP", Resolution: "1080p"}
	for _, test := range []struct {
		name     string
		identity mediaResult
		release  string
		want     int
	}{
		{"unknown identity", mediaResult{}, "S01E02.1080p.WEB-DL-GROUP", 65},
		{"unknown year", mediaResult{Name: "Example Show"}, "S01E02.1080p.WEB-DL-GROUP", 65},
		{"returned title and year", mediaResult{Name: "Example Show", Year: 2020}, "S01E02.1080p.WEB-DL-GROUP", 80},
		{"parsed title and year", mediaResult{}, "Example.Show.2020.S01E02.1080p.WEB-DL-GROUP", 80},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := &Client{}
			items := []searchItem{{URL: "/subtitle/example.zip", Language: "EN", Season: 1, Episode: 2, Identity: test.identity, Releases: []string{test.release}}}
			got := client.normalize(baseprovider.SearchQuery{Media: media, Language: "en"}, items)
			if len(got) != 1 {
				t.Fatalf("candidates = %#v", got)
			}
			if score := match.Evaluate(media, got[0], "en"); score.Total != test.want {
				t.Fatalf("score = %#v, want %d", score, test.want)
			}
			if got[0].Title != test.identity.Name || got[0].Year != test.identity.Year {
				t.Fatalf("request identity leaked into candidate: %#v", got[0])
			}
		})
	}
}

func TestNormalizeMergesMissingEpisodeBeforeFiltering(t *testing.T) {
	client := &Client{id: "subdl-main"}
	query := baseprovider.SearchQuery{Media: episodeMedia(), Language: "en"}
	first := searchItem{URL: "/subtitle/example.zip", Language: "EN", Releases: []string{"2160p.BluRay-GROUP"}, Rating: 1, DownloadCount: 10000}
	second := searchItem{URL: first.URL, Language: "EN", Season: 1, Episode: 2, Releases: []string{"720p.WEB-DL-OTHER"}}
	got := client.normalize(query, []searchItem{first, second})
	if len(got) != 1 || got[0].Episode != 2 || got[0].Rating != 1 || got[0].DownloadCount != 10000 || !slices.Equal(got[0].ReleaseNames, []string{"2160p.BluRay-GROUP", "720p.WEB-DL-OTHER"}) {
		t.Fatalf("missing-episode duplicate lost evidence: %#v", got)
	}
	if got := client.normalize(query, []searchItem{first}); len(got) != 0 {
		t.Fatalf("unknown episode accepted: %#v", got)
	}
	second.Episode = 3
	if got := client.normalize(query, []searchItem{first, second}); len(got) != 0 {
		t.Fatalf("wrong episode accepted: %#v", got)
	}
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
	for _, key := range []string{"api_key", "imdb_id", "file_name", "type", "year", "languages", "releases", "unpack", "subs_per_page", "client"} {
		if queries[0].Get(key) == "" {
			t.Errorf("standard query missing %s: %s", key, queries[0].Encode())
		}
	}
	// SubDL's hi=1 filters to hearing-impaired subtitles only; the hi flag is
	// returned on every result without it.
	for _, query := range queries {
		if query.Has("hi") {
			t.Errorf("query must not filter to hearing-impaired subtitles: %s", query.Encode())
		}
	}
	if len(candidates) != 1 || candidates[0].ResultID != "/subtitle/pack.zip:file-2" || candidates[0].DownloadRef != "/subtitle/pack/file-2" || candidates[0].Pack != nil || candidates[0].Episode != 2 || !candidates[0].HearingImpaired || len(candidates[0].ReleaseNames) != 3 {
		t.Fatalf("direct candidate = %#v", candidates)
	}
}

func TestEpisodeSearchMergesLaterDuplicateScoringEvidence(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			io.WriteString(w, `{"status":true,"subtitles":[{"url":"/subtitle/example.zip","language":"EN","season":1,"episode":2,"releases":["720p.WEB-DL-A"]}]}`)
			return
		}
		io.WriteString(w, `{"status":true,"results":[{"name":"Example Show","year":2024,"imdb_id":"tt1234567"}],"subtitles":[{"url":"/subtitle/example.zip","language":"EN","season":1,"episode":2,"releases":["2160p.BluRay-B"],"rating":1,"download_count":10000}]}`)
	}))
	defer server.Close()
	client := newTestClient(t, server, 1<<20)
	media := episodeMedia()
	media.Source, media.Resolution, media.ReleaseGroup = "bluray", "2160p", "B"
	got, err := client.Search(context.Background(), baseprovider.SearchQuery{Media: media, Language: "en", Mode: baseprovider.SearchBroad})
	if err != nil || len(got) != 1 || calls != 2 {
		t.Fatalf("candidates/calls = %#v/%d, %v", got, calls, err)
	}
	candidate := got[0]
	if !slices.Equal(candidate.ReleaseNames, []string{"720p.WEB-DL-A", "2160p.BluRay-B"}) || candidate.Rating != 1 || candidate.DownloadCount != 10000 || candidate.Title != media.Title || candidate.Year != media.Year || candidate.ExternalIDs.IMDb != media.ExternalIDs.IMDb {
		t.Fatalf("duplicate evidence lost: %#v", candidate)
	}
	if score := match.Evaluate(media, candidate, "en"); score.Total != 100 {
		t.Fatalf("merged score = %#v, want 100", score)
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

func TestSearchStripsDownloadCredentialsAndDownloadReappliesConfiguredKey(t *testing.T) {
	var downloadQuery url.Values
	var downloadHeader string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/subtitles" {
			io.WriteString(w, `{"status":true,"results":[{"imdb_id":"tt123","name":"Movie","year":2025,"type":"movie"}],"subtitles":[{"name":"movie.srt","url":"/subtitle/movie.srt?api_key=returned-secret","language":"EN"}]}`)
			return
		}
		downloadQuery = r.URL.Query()
		downloadHeader = r.Header.Get("x-api-key")
		io.WriteString(w, "subtitle")
	}))
	defer server.Close()
	client := newTestClient(t, server, 1<<20)
	media := domain.Media{Ref: domain.MediaRef{Kind: domain.MediaMovie}, Title: "Movie", Year: 2025, ExternalIDs: domain.ExternalIDs{IMDb: "tt123"}}

	candidates, err := client.Search(context.Background(), baseprovider.SearchQuery{Media: media, Language: "en", Mode: baseprovider.SearchBroad})
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 || candidates[0].ResultID != "/subtitle/movie.srt" || candidates[0].DownloadRef != "/subtitle/movie.srt" {
		t.Fatalf("credential-free candidate = %#v", candidates)
	}
	var output bytes.Buffer
	if _, err := client.Download(context.Background(), candidates[0], &output); err != nil {
		t.Fatal(err)
	}
	if output.String() != "subtitle" || downloadQuery.Get("api_key") != "api-key" || downloadHeader != "api-key" {
		t.Fatalf("download authentication query/header/output = %q/%q/%q", downloadQuery.Get("api_key"), downloadHeader, output.String())
	}
}

func TestForbiddenInvalidKeyDisablesProviderUntilExplicitRetry(t *testing.T) {
	for _, operation := range []string{"search", "download"} {
		t.Run(operation, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.WriteHeader(http.StatusForbidden)
				io.WriteString(w, `{"status":false,"error":"Invalid API key"}`)
			}))
			defer server.Close()
			client, states := newTestClientWithState(t, server, 1<<20)
			run := func() error {
				if operation == "search" {
					_, err := client.Search(context.Background(), baseprovider.SearchQuery{Media: episodeMedia(), Language: "en", Mode: baseprovider.SearchBroad})
					return err
				}
				_, err := client.Download(context.Background(), domain.Candidate{DownloadRef: "/subtitle/forbidden"}, io.Discard)
				return err
			}
			var authentication *baseprovider.AuthenticationError
			if err := run(); !errors.As(err, &authentication) {
				t.Fatalf("first error = %T %v", err, err)
			}
			state, err := states.GetProviderState(context.Background(), "subdl-main", string(baseprovider.OperationAuth))
			if err != nil || !state.Disabled {
				t.Fatalf("disabled state = %#v, %v", state, err)
			}
			var disabled *baseprovider.DisabledError
			if err := run(); !errors.As(err, &disabled) || calls != 1 {
				t.Fatalf("second error/calls = %T %v/%d", err, err, calls)
			}
		})
	}
}

func TestForbiddenWithoutAKeyMessageIsTemporary(t *testing.T) {
	for _, body := range []string{"", `<html>Access denied</html>`, `{"status":false,"error":"Forbidden"}`} {
		for _, operation := range []string{"search", "download"} {
			t.Run(operation+"/"+body, func(t *testing.T) {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.WriteHeader(http.StatusForbidden)
					io.WriteString(w, body)
				}))
				defer server.Close()
				client, states := newTestClientWithState(t, server, 1<<20)
				var err error
				if operation == "search" {
					_, err = client.Search(context.Background(), baseprovider.SearchQuery{Media: episodeMedia(), Language: "en", Mode: baseprovider.SearchBroad})
				} else {
					_, err = client.Download(context.Background(), domain.Candidate{DownloadRef: "/subtitle/forbidden"}, io.Discard)
				}
				var cooldown *baseprovider.CooldownError
				if !errors.As(err, &cooldown) {
					t.Fatalf("error = %T %v; want a temporary cooldown", err, err)
				}
				if state, err := states.GetProviderState(context.Background(), "subdl-main", string(baseprovider.OperationAuth)); err == nil && state.Disabled {
					t.Fatalf("provider disabled by a 403 without a key message: %#v", state)
				}
			})
		}
	}
}

func TestLimitMessagesInJSONBodiesCoolDownUntilTheDailyReset(t *testing.T) {
	reset := time.Date(2026, 9, 5, 0, 15, 0, 0, time.UTC)
	for _, tt := range []struct {
		name, operation, body string
		status                int
	}{
		{"search status false", "search", `{"status":false,"error":"You have reached your daily limit"}`, http.StatusOK},
		{"search 400", "search", `{"status":false,"error":"API quota exceeded"}`, http.StatusBadRequest},
		{"download 403", "download", `{"status":false,"message":"Daily download limit exceeded"}`, http.StatusForbidden},
	} {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.status)
				io.WriteString(w, tt.body)
			}))
			defer server.Close()
			client, states := newTestClientWithState(t, server, 1<<20)
			var err error
			if tt.operation == "search" {
				_, err = client.Search(context.Background(), baseprovider.SearchQuery{Media: episodeMedia(), Language: "en", Mode: baseprovider.SearchBroad})
			} else {
				_, err = client.Download(context.Background(), domain.Candidate{DownloadRef: "/subtitle/example.zip"}, io.Discard)
			}
			var quota *baseprovider.QuotaError
			if !errors.As(err, &quota) || !quota.ResetAt.Equal(reset) {
				t.Fatalf("error = %T %v; want a quota until %v", err, err, reset)
			}
			state, err := states.GetProviderState(context.Background(), "subdl-main", tt.operation)
			if err != nil || !state.ResetAt.Equal(reset) {
				t.Fatalf("saved state = %#v, %v", state, err)
			}
			if state, err := states.GetProviderState(context.Background(), "subdl-main", string(baseprovider.OperationAuth)); err == nil && state.Disabled {
				t.Fatal("a limit message disabled the provider")
			}
		})
	}
}

func TestLimitWordingNeedsAReachedDailyDownloadOrQuotaLimit(t *testing.T) {
	for _, message := range []string{"film_name exceeds the character limit", "subs_per_page must not exceed the limit of 30", "Upgrade for unlimited downloads"} {
		if got := limitCode(message); got != "" {
			t.Errorf("limitCode(%q) = %q, want no limit", message, got)
		}
	}
	for _, message := range []string{"Max file size exceeded for download", "Download link expired"} {
		if got := limitCode(message); got != "" {
			t.Errorf("limitCode(%q) = %q, want no limit", message, got)
		}
	}
	for _, message := range []string{"You have reached your daily download limit", "Daily download limit exceeded", "API quota exceeded", "You have reached your daily limit", "Download limit reached", "daily_limit", "Quota exhausted"} {
		if got := limitCode(message); got != "daily_limit" {
			t.Errorf("limitCode(%q) = %q, want daily_limit", message, got)
		}
	}
	// A reached limit that names neither the day, downloads nor a quota may
	// be SubDL's per-minute limit, so it is a short rate-limit cooldown.
	for _, message := range []string{"Request limit reached", "You have reached your limit", "Limit exceeded", "Too many requests", "rate_limit"} {
		if got := limitCode(message); got != "rate_limit" {
			t.Errorf("limitCode(%q) = %q, want rate_limit", message, got)
		}
	}
}

func TestRequestLimitReachedIsARateLimitNotTheDailyQuota(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"status":false,"error":"Request limit reached"}`)
	}))
	defer server.Close()
	client, states := newTestClientWithState(t, server, 1<<20)
	_, err := client.Search(context.Background(), baseprovider.SearchQuery{Media: episodeMedia(), Language: "en", Mode: baseprovider.SearchBroad})
	want := client.clock.Now().Add(15 * time.Minute)
	var cooldown *baseprovider.CooldownError
	if !errors.As(err, &cooldown) || cooldown.Reason != string(baseprovider.CooldownRateLimit) || !cooldown.ResetAt.Equal(want) {
		t.Fatalf("error = %T %v; want a rate-limit cooldown until %v", err, err, want)
	}
	if state, err := states.GetProviderState(context.Background(), "subdl-main", "search"); err != nil || !state.ResetAt.Equal(want) {
		t.Fatalf("saved state = %#v, %v", state, err)
	}
}

func TestRepeated403BacksOff(t *testing.T) {
	// A Cloudflare block answers 403 every time; the circuit must escalate
	// 1, 5, 15, then 60 minutes instead of restarting at one minute.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		io.WriteString(w, `<html>Access denied</html>`)
	}))
	defer server.Close()
	client, _ := newTestClientWithState(t, server, 1<<20)
	clock := client.clock.(*testutil.Clock)
	for _, delay := range []time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute, time.Hour, time.Hour} {
		_, err := client.Search(context.Background(), baseprovider.SearchQuery{Media: episodeMedia(), Language: "en", Mode: baseprovider.SearchBroad})
		var cooldown *baseprovider.CooldownError
		if !errors.As(err, &cooldown) || !cooldown.ResetAt.Equal(clock.Now().Add(delay)) {
			t.Fatalf("error = %T %v; want a cooldown of %v", err, err, delay)
		}
		clock.Advance(delay + time.Second)
	}
}

func Test401IsReadLikeA403(t *testing.T) {
	reset := time.Date(2026, 9, 5, 0, 15, 0, 0, time.UTC)
	for _, tt := range []struct {
		name, body string
		check      func(t *testing.T, err error, disabled bool)
	}{
		{"daily limit", `{"status":false,"error":"Daily limit reached"}`, func(t *testing.T, err error, disabled bool) {
			var quota *baseprovider.QuotaError
			if !errors.As(err, &quota) || !quota.ResetAt.Equal(reset) || disabled {
				t.Fatalf("error = %T %v, disabled = %t; want the daily quota", err, err, disabled)
			}
		}},
		{"other message", `{"status":false,"error":"Temporarily blocked"}`, func(t *testing.T, err error, disabled bool) {
			var cooldown *baseprovider.CooldownError
			if !errors.As(err, &cooldown) || cooldown.Reason != "transient_http_401" || disabled {
				t.Fatalf("error = %T %v, disabled = %t; want the transient circuit", err, err, disabled)
			}
		}},
		{"rejected key", `{"status":false,"error":"Invalid API key"}`, func(t *testing.T, err error, disabled bool) {
			var authentication *baseprovider.AuthenticationError
			if !errors.As(err, &authentication) || !disabled {
				t.Fatalf("error = %T %v, disabled = %t; want the provider disabled", err, err, disabled)
			}
		}},
		{"html page", `<html>401</html>`, func(t *testing.T, err error, disabled bool) {
			if !disabled {
				t.Fatalf("error = %T %v; want the provider disabled", err, err)
			}
		}},
	} {
		for _, operation := range []string{"search", "download"} {
			t.Run(operation+"/"+tt.name, func(t *testing.T) {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.WriteHeader(http.StatusUnauthorized)
					io.WriteString(w, tt.body)
				}))
				defer server.Close()
				client, states := newTestClientWithState(t, server, 1<<20)
				var err error
				if operation == "search" {
					_, err = client.Search(context.Background(), baseprovider.SearchQuery{Media: episodeMedia(), Language: "en", Mode: baseprovider.SearchBroad})
				} else {
					_, err = client.Download(context.Background(), domain.Candidate{DownloadRef: "/subtitle/example.zip"}, io.Discard)
				}
				state, stateErr := states.GetProviderState(context.Background(), "subdl-main", string(baseprovider.OperationAuth))
				tt.check(t, err, stateErr == nil && state.Disabled)
			})
		}
	}
}

func TestRejectedKeyOrTokenAndAny401DisableProvider(t *testing.T) {
	for _, tt := range []struct {
		status int
		body   string
	}{
		{http.StatusForbidden, `{"status":false,"error":"API key banned"}`},
		{http.StatusForbidden, `{"status":false,"error":"invalid token"}`},
		{http.StatusForbidden, `{"status":false,"error":"Unauthorized API key"}`},
		{http.StatusForbidden, `{"status":false,"message":"Your token has expired"}`},
		{http.StatusUnauthorized, `{"status":false,"error":"Unauthorized"}`},
		{http.StatusUnauthorized, ""},
	} {
		for _, operation := range []string{"search", "download"} {
			t.Run(fmt.Sprintf("%s/%d/%s", operation, tt.status, tt.body), func(t *testing.T) {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.WriteHeader(tt.status)
					io.WriteString(w, tt.body)
				}))
				defer server.Close()
				client, states := newTestClientWithState(t, server, 1<<20)
				var err error
				if operation == "search" {
					_, err = client.Search(context.Background(), baseprovider.SearchQuery{Media: episodeMedia(), Language: "en", Mode: baseprovider.SearchBroad})
				} else {
					_, err = client.Download(context.Background(), domain.Candidate{DownloadRef: "/subtitle/forbidden"}, io.Discard)
				}
				var authentication *baseprovider.AuthenticationError
				if !errors.As(err, &authentication) {
					t.Fatalf("error = %T %v; want an authentication error", err, err)
				}
				state, err := states.GetProviderState(context.Background(), "subdl-main", string(baseprovider.OperationAuth))
				if err != nil || !state.Disabled {
					t.Fatalf("disabled state = %#v, %v", state, err)
				}
			})
		}
	}
}

func TestOnlyAPIKeyMessagesDisableProvider(t *testing.T) {
	for _, message := range []string{"Download token expired", "Subtitle not found for this key", "Invalid download token", "Subtitle not found"} {
		if invalidAPIKey(message) {
			t.Errorf("invalidAPIKey(%q) = true; want false", message)
		}
	}
	for _, message := range []string{"API key banned", "invalid token", "Your token has expired", "apikey is required", "Unknown api_key"} {
		if !invalidAPIKey(message) {
			t.Errorf("invalidAPIKey(%q) = false; want true", message)
		}
	}
}

func TestDownload401FromRedirectTargetDoesNotDisableProvider(t *testing.T) {
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer cdn.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, cdn.URL+"/file.zip", http.StatusFound)
	}))
	defer server.Close()
	client, states := newTestClientWithState(t, server, 1<<20)
	client.config.DownloadHosts = append(client.config.DownloadHosts, cdn.Listener.Addr().String())
	_, err := client.Download(context.Background(), domain.Candidate{DownloadRef: "/subtitle/redirected"}, io.Discard)
	var authentication *baseprovider.AuthenticationError
	if err == nil || errors.As(err, &authentication) {
		t.Fatalf("error = %T %v; want a download failure that is not an authentication error", err, err)
	}
	if state, err := states.GetProviderState(context.Background(), "subdl-main", string(baseprovider.OperationAuth)); err == nil && state.Disabled {
		t.Fatal("a redirect target's 401 disabled the provider")
	}
}

func TestIdentityComesOnlyFromASingleMatchedTitle(t *testing.T) {
	for _, tt := range []struct {
		name, results string
		want          string
	}{
		{"one title", `[{"sd_id":1,"name":"Example Show","year":2024,"imdb_id":"tt1234567"}]`, "Example Show"},
		{"one title listed twice", `[{"sd_id":1,"name":"Example Show","year":2024},{"sd_id":1,"name":"Example Show","year":2024}]`, "Example Show"},
		{"two titles", `[{"sd_id":1,"name":"Example Show","year":2024,"imdb_id":"tt1234567"},{"sd_id":2,"name":"Example Show","year":1994,"imdb_id":"tt7654321"}]`, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				io.WriteString(w, `{"status":true,"results":`+tt.results+`,"subtitles":[{"url":"/subtitle/example.zip","language":"EN","season":1,"episode":2}]}`)
			}))
			defer server.Close()
			candidates, err := newTestClient(t, server, 1<<20).Search(context.Background(), baseprovider.SearchQuery{Media: episodeMedia(), Language: "en", Mode: baseprovider.SearchBroad})
			if err != nil || len(candidates) != 1 || candidates[0].Title != tt.want {
				t.Fatalf("candidates = %#v, %v; want title %q", candidates, err, tt.want)
			}
			if tt.want == "" && (candidates[0].ExternalIDs.IMDb != "" || candidates[0].Year != 0) {
				t.Fatalf("identity taken from one of several titles: %#v", candidates[0])
			}
		})
	}
}

func TestDownloadRedirectLoopStopsWithoutProviderCooldown(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		http.Redirect(w, r, "/subtitle/loop", http.StatusFound)
	}))
	defer server.Close()
	client := newTestClient(t, server, 1<<20)
	_, err := client.Download(context.Background(), domain.Candidate{DownloadRef: "/subtitle/loop"}, io.Discard)
	var redirect *baseprovider.RedirectError
	if !errors.As(err, &redirect) || requests != baseprovider.MaxDownloadRedirects {
		t.Fatalf("error = %T %v after %d requests; want a redirect refusal after %d", err, err, requests, baseprovider.MaxDownloadRedirects)
	}
	if err := client.CheckDownloadAvailability(context.Background()); err != nil {
		t.Fatalf("download availability = %v; a refused redirect is not a provider outage", err)
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

func TestSingleEpisodeResolutionIsNotPack(t *testing.T) {
	c := &Client{id: "subdl-main"}
	q := baseprovider.SearchQuery{Media: episodeMedia(), Language: "en"}
	got := c.normalize(q, []searchItem{{URL: "/subtitle/example.zip", Language: "EN", Season: 1, Episode: 2, Releases: []string{"Example.Show.S01E02.1080p.WEB-DL-GROUP"}}})
	if len(got) != 1 {
		t.Fatalf("candidates=%#v", got)
	}
	if got[0].Pack != nil || got[0].Episode != 2 {
		t.Fatalf("single episode misclassified: episode=%d pack=%+v", got[0].Episode, got[0].Pack)
	}
}
func TestDirectMemberMustMatchSeason(t *testing.T) {
	c := &Client{id: "subdl-main"}
	q := baseprovider.SearchQuery{Media: episodeMedia(), Language: "en"}
	got := c.normalize(q, []searchItem{{URL: "/subtitle/example.zip", Language: "EN", Season: 1, FullSeason: true, UnpackFiles: []unpackFile{
		{FileID: "wrong", URL: "/subtitle/wrong.srt", Language: "EN", Season: 2, Episode: 2},
		{FileID: "right", URL: "/subtitle/right.srt", Language: "EN", Season: 1, Episode: 2},
	}}})
	if len(got) != 1 || got[0].DownloadRef != "/subtitle/right.srt" {
		t.Fatalf("wrong direct selected: %#v", got)
	}
}
func TestAbsoluteRangeSuppliesEpisodeEvidence(t *testing.T) {
	c := &Client{id: "subdl-main"}
	media := episodeMedia()
	media.AbsoluteEpisode = 102
	q := baseprovider.SearchQuery{Media: media, Language: "en"}
	got := c.normalize(q, []searchItem{{URL: "/subtitle/example.zip", Language: "EN", Season: 1, EpisodeFrom: 101, EpisodeEnd: 110}})
	if len(got) != 1 {
		t.Fatalf("candidates=%#v", got)
	}
	if !match.HasEpisodeEvidence(media, got[0]) {
		t.Fatalf("absolute containing pack lost evidence: %+v", got[0].Pack)
	}
}

func TestAbsoluteSingleSuppliesEpisodeEvidence(t *testing.T) {
	c := &Client{id: "subdl-main"}
	media := episodeMedia()
	media.AbsoluteEpisode = 102
	q := baseprovider.SearchQuery{Media: media, Language: "en"}
	got := c.normalize(q, []searchItem{{URL: "/subtitle/example.zip", Language: "EN", Episode: 102}})
	if len(got) != 1 {
		t.Fatalf("candidates=%#v", got)
	}
	if !match.HasEpisodeEvidence(media, got[0]) {
		t.Fatalf("absolute episode lost evidence: %+v", got[0])
	}
}

func TestReleaseRangesUseStrictSharedContract(t *testing.T) {
	for _, tc := range []struct {
		name         string
		pack, reject bool
	}{
		{"Example.S01E02.1080p.WEB-DL-GROUP", false, false},
		{"Example.S01E02-WEB-DL-GROUP", false, false},
		{"Example.S01E01-E03.1080p", true, false},
		{"Example.S01E01-S01E03.1080p", true, false},
		{"Example.1x01-1x03.1080p", true, false},
		{"Example.S01E01-S02E03", false, true},
		{"Example.S01E03-E01", false, true},
		{"Example.S01E01-E03-E04", false, true},
		{"Example.S01E01-E03oops", false, true},
		{"Example.S01E01-E", false, true},
		{"Example.S02E01-E03", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &Client{id: "subdl"}
			q := baseprovider.SearchQuery{Media: episodeMedia(), Language: "en"}
			got := c.normalize(q, []searchItem{{URL: "/subtitle/x", Language: "EN", Season: 1, Episode: 2, ReleaseName: tc.name}})
			if tc.reject {
				if len(got) != 0 {
					t.Fatalf("invalid range accepted: %+v", got)
				}
				return
			}
			if len(got) != 1 || (got[0].Pack != nil) != tc.pack {
				t.Fatalf("candidate shape=%+v", got)
			}
		})
	}
}

func TestDirectMembersRequireUniqueMatch(t *testing.T) {
	q := baseprovider.SearchQuery{Media: episodeMedia(), Language: "en"}
	q.Media.AbsoluteEpisode = 102
	c := &Client{id: "subdl"}
	files := []unpackFile{{FileID: "one", URL: "/one", Language: "EN", Season: 1, Episode: 2}, {FileID: "two", URL: "/two", Language: "EN", Season: 1, Episode: 2}}
	got := c.normalize(q, []searchItem{{URL: "/pack", Language: "EN", Season: 1, FullSeason: true, UnpackFiles: files}})
	if len(got) != 1 || got[0].Pack == nil || got[0].DownloadRef != "/pack" {
		t.Fatalf("ambiguous member chosen: %+v", got)
	}
	got = c.normalize(q, []searchItem{{URL: "/pack", Language: "EN", UnpackFiles: []unpackFile{{FileID: "absolute", URL: "/absolute", Language: "EN", Episode: 102}}}})
	if len(got) != 1 || got[0].Episode != 0 || got[0].AbsoluteEpisode != 102 || !match.HasEpisodeEvidence(q.Media, got[0]) {
		t.Fatalf("direct absolute identity=%+v", got)
	}
}

func TestDailyLimitOnSearchIsSearchQuota(t *testing.T) {
	reset := time.Date(2026, 9, 5, 0, 15, 0, 0, time.UTC)
	for _, tt := range []struct {
		operation, reason string
		status            int
	}{
		{"search", "search_quota", http.StatusOK},
		{"download", "download_quota", http.StatusForbidden},
	} {
		t.Run(tt.operation, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.status)
				io.WriteString(w, `{"status":false,"error":"daily_limit"}`)
			}))
			defer server.Close()
			client, states := newTestClientWithState(t, server, 1<<20)
			var err error
			if tt.operation == "search" {
				_, err = client.Search(context.Background(), baseprovider.SearchQuery{Media: episodeMedia(), Language: "en", Mode: baseprovider.SearchBroad})
			} else {
				_, err = client.Download(context.Background(), domain.Candidate{DownloadRef: "/subtitle/example.zip"}, io.Discard)
			}
			var quota *baseprovider.QuotaError
			if !errors.As(err, &quota) || quota.Scope != baseprovider.Operation(tt.operation) || !quota.ResetAt.Equal(reset) {
				t.Fatalf("error = %T %v; want a %s quota until %v", err, err, tt.operation, reset)
			}
			state, err := states.GetProviderState(context.Background(), "subdl-main", tt.operation)
			if err != nil || state.Reason != tt.reason || !state.ResetAt.Equal(reset) {
				t.Fatalf("saved state = %#v, %v; want reason %s until %v", state, err, tt.reason, reset)
			}
		})
	}
}
