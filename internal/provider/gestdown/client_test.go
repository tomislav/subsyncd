package gestdown

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"subsyncd/internal/domain"
	base "subsyncd/internal/provider"
	"subsyncd/internal/store"
)

const showID = "11111111-1111-4111-8111-111111111111"
const subtitleID = "22222222-2222-4222-8222-222222222222"
const showJSON = `{"shows":[{"id":"` + showID + `","name":"Example Show","tvDbId":123,"tmdbId":456,"nbSeasons":1,"seasons":[1],"slug":"example-show"}]}`
const episodeJSON = `{"episode":{"season":1,"number":2,"show":"Example Show","title":"Example Episode"},"matchingSubtitles":[{"subtitleId":"` + subtitleID + `","version":"WEB-DL-GROUP","release":"Example.Show.S01E02.1080p.WEB-DL-GROUP","language":"English","completed":true,"hearingImpaired":true,"downloadCount":100,"downloadUri":"https://untrusted.invalid/ignore","qualities":["720p","1080p"]}]}`
const subtitleText = "1\n00:00:01,000 --> 00:00:02,000\nExample dialogue.\n\n"

func query() base.SearchQuery {
	return base.SearchQuery{Mode: base.SearchBroad, Language: "en", Media: domain.Media{Ref: domain.MediaRef{Kind: domain.MediaEpisode}, Title: "Example Show", Year: 2020, Season: 1, Episode: 2, ExternalIDs: domain.ExternalIDs{TVDB: 123, IMDb: "tt9999999"}}}
}

func newTestClient(t *testing.T, handler http.HandlerFunc) (*Client, *store.Repository) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "provider.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	clock := base.SystemClock{}
	gate := base.NewGate(db.Repository(), clock, 1)
	gate.Configure("gestdown-test", 1000, 100, 1, "gestdown")
	c, err := New(Config{BaseURL: server.URL, AllowInsecureForTests: true}, base.Client{HTTP: server.Client(), Gate: gate, Clock: clock, ProviderID: "gestdown-test", ProviderType: "gestdown"})
	if err != nil {
		t.Fatal(err)
	}
	return c, db.Repository()
}

func TestSearchDownloadUsesReturnedEvidence(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" || r.Header.Get("Api-Key") != "" {
			t.Error("unexpected credentials")
		}
		switch r.URL.Path {
		case "/shows/external/tvdb/123":
			io.WriteString(w, showJSON)
		case "/subtitles/get/" + showID + "/1/2/en":
			io.WriteString(w, episodeJSON)
		case "/subtitles/download/" + subtitleID:
			w.Header().Set("Content-Type", "text/plain")
			io.WriteString(w, subtitleText)
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
			http.NotFound(w, r)
		}
	})
	got, err := c.Search(context.Background(), query())
	if err != nil || len(got) != 1 {
		t.Fatalf("search = %#v, %v", got, err)
	}
	v := got[0]
	if v.Title != "Example Show" || v.Year != 0 || v.ExternalIDs.IMDb != "" || v.ExternalIDs.TVDB != 123 || v.ExternalIDs.TMDB != 456 || v.Season != 1 || v.Episode != 2 || !v.HearingImpaired || v.ExactHash || v.Language != "en" {
		t.Fatalf("wrong evidence: %#v", v)
	}
	if len(v.ReleaseNames) != 2 || v.DownloadRef != "/subtitles/download/"+subtitleID {
		t.Fatalf("wrong release/reference: %#v", v)
	}
	var body bytes.Buffer
	meta, err := c.Download(context.Background(), v, &body)
	if err != nil || body.String() != subtitleText || meta.Filename != "subtitle.srt" {
		t.Fatalf("download = %q, %+v, %v", body.String(), meta, err)
	}
}

func TestSearchRejectsIncompleteOrConflictingEvidence(t *testing.T) {
	for _, tc := range []struct{ name, shows, episode string }{
		{"wrong show ID", strings.Replace(showJSON, `"tvDbId":123`, `"tvDbId":999`, 1), episodeJSON},
		{"wrong episode", showJSON, strings.Replace(episodeJSON, `"number":2`, `"number":3`, 1)},
		{"missing episode", showJSON, `{"matchingSubtitles":[]}`},
		{"conflicting episode show", showJSON, strings.Replace(episodeJSON, `"show":"Example Show"`, `"show":"Another Show"`, 1)},
		{"incomplete", showJSON, strings.Replace(episodeJSON, `"completed":true`, `"completed":false`, 1)},
		{"missing HI", showJSON, strings.Replace(episodeJSON, `"hearingImpaired":true,`, "", 1)},
		{"null HI", showJSON, strings.Replace(episodeJSON, `"hearingImpaired":true`, `"hearingImpaired":null`, 1)},
		{"wrong language", showJSON, strings.Replace(episodeJSON, `"English"`, `"Croatian"`, 1)},
		{"bad subtitle ID", showJSON, strings.ReplaceAll(episodeJSON, subtitleID, "../escape?secret=value")},
		{"malformed catalog entry", showJSON, strings.ReplaceAll(episodeJSON, subtitleID, "sp_"+showID+"_entry_bad")},
		{"catalog entry trailing path", showJSON, strings.ReplaceAll(episodeJSON, subtitleID, "sp_"+showID+"_entry_"+subtitleID+"/escape")},
		{"catalog entry chained suffix", showJSON, strings.ReplaceAll(episodeJSON, subtitleID, "sp_"+showID+"_entry_"+subtitleID+"_ep_2")},
		{"whole season pack", showJSON, strings.ReplaceAll(episodeJSON, subtitleID, "sp_"+subtitleID)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				if strings.HasPrefix(r.URL.Path, "/shows/") {
					io.WriteString(w, tc.shows)
				} else {
					io.WriteString(w, tc.episode)
				}
			})
			got, err := c.Search(context.Background(), query())
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != 0 {
				t.Fatalf("accepted: %#v", got)
			}
		})
	}
}

func TestTitleFallbackDoesNotInventIDs(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/shows/external/tvdb/123":
			http.NotFound(w, r)
		case "/shows/search/Example Show":
			io.WriteString(w, strings.ReplaceAll(strings.ReplaceAll(showJSON, `"tvDbId":123`, `"tvDbId":null`), `"tmdbId":456`, `"tmdbId":null`))
		case "/subtitles/get/" + showID + "/1/2/en":
			io.WriteString(w, episodeJSON)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			http.NotFound(w, r)
		}
	})
	got, err := c.Search(context.Background(), query())
	if err != nil || len(got) != 1 {
		t.Fatalf("%#v %v", got, err)
	}
	if got[0].ExternalIDs != (domain.ExternalIDs{}) {
		t.Fatalf("invented IDs: %+v", got[0])
	}
}

func TestCooldownPersistsAndStopsFurtherRequests(t *testing.T) {
	for _, status := range []int{423, 429, 503} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			count := 0
			c, repo := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				count++
				w.Header().Set("Retry-After", "120")
				w.WriteHeader(status)
			})
			before := time.Now()
			_, err := c.Search(context.Background(), query())
			var cooldown *base.CooldownError
			if !errors.As(err, &cooldown) || cooldown.ResetAt.Before(before.Add(119*time.Second)) {
				t.Fatalf("cooldown = %v", err)
			}
			state, err := repo.GetProviderState(context.Background(), "gestdown-test", "search")
			if err != nil || !state.ResetAt.After(before) {
				t.Fatalf("state %+v %v", state, err)
			}
			_, err = c.Search(context.Background(), query())
			if !errors.As(err, &cooldown) || count != 1 {
				t.Fatalf("retry escaped cooldown: calls=%d err=%v", count, err)
			}
		})
	}
}

func TestDownloadRejectsUnsafeIDsRedirectsAndOversize(t *testing.T) {
	for _, tc := range []struct {
		name, id string
		status   int
		limit    int64
	}{
		{"path traversal", "../escape", 200, 100}, {"redirect", subtitleID, 302, 100}, {"oversize", subtitleID, 200, 10},
	} {
		t.Run(tc.name, func(t *testing.T) {
			count := 0
			c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				count++
				if tc.status == 302 {
					w.Header().Set("Location", "/escaped")
					w.WriteHeader(302)
					return
				}
				io.WriteString(w, subtitleText)
			})
			c.config.MaxDownloadBytes = tc.limit
			_, err := c.Download(context.Background(), domain.Candidate{ProviderID: c.ID(), ResultID: tc.id}, io.Discard)
			if err == nil {
				t.Fatal("download accepted")
			}
			if count > 1 || tc.name == "path traversal" && count != 0 {
				t.Fatalf("unsafe requests=%d", count)
			}
		})
	}
}

func TestHeaderlessBusyAndRateLimitHaveFiniteRetry(t *testing.T) {
	for _, status := range []int{423, 429} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(status) })
			now := time.Now()
			_, err := c.Search(context.Background(), query())
			var cooldown *base.CooldownError
			if !errors.As(err, &cooldown) || cooldown.ResetAt.Before(now.Add(time.Minute)) || cooldown.ResetAt.After(now.Add(10*time.Minute)) {
				t.Fatalf("missing bounded retry: %v", err)
			}
		})
	}
}

func TestInvalidJSONAndCancellationRemainTechnical(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, `{"secret":"do not expose"`) })
	_, err := c.Search(context.Background(), query())
	if err == nil || strings.Contains(err.Error(), "do not expose") {
		t.Fatalf("unsafe error %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = c.Search(ctx, query())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation lost: %v", err)
	}
}

func TestServerExtractedEpisodeIDMustMatchReturnedEpisode(t *testing.T) {
	for _, tc := range []struct {
		id   string
		want int
	}{
		{"sp_" + subtitleID + "_ep_2", 1}, {"sp_" + subtitleID + "_ep_3", 0}, {"sp_" + subtitleID, 0},
	} {
		t.Run(tc.id, func(t *testing.T) {
			c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.HasPrefix(r.URL.Path, "/shows/"):
					io.WriteString(w, showJSON)
				case strings.HasPrefix(r.URL.Path, "/subtitles/get/"):
					io.WriteString(w, strings.ReplaceAll(episodeJSON, subtitleID, tc.id))
				case r.URL.Path == "/subtitles/download/"+tc.id:
					io.WriteString(w, subtitleText)
				default:
					t.Errorf("unexpected request %s", r.URL.Path)
					http.NotFound(w, r)
				}
			})
			result, err := c.Search(context.Background(), query())
			if err != nil || len(result) != tc.want {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			if tc.want == 0 {
				return
			}
			var body bytes.Buffer
			if _, err := c.Download(context.Background(), result[0], &body); err != nil {
				t.Fatal(err)
			}
			if body.String() != subtitleText {
				t.Fatal("incorrect downloaded episode")
			}
		})
	}
}
