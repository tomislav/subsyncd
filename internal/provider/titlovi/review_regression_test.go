package titlovi

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"subsyncd/internal/domain"
	"subsyncd/internal/match"
	baseprovider "subsyncd/internal/provider"
	"subsyncd/internal/store"
	"subsyncd/internal/testutil"
	"testing"
	"time"
)

type reviewTransport func(*http.Request) (*http.Response, error)

func (f reviewTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestRefreshOnSecondPagePreservesEarlierCandidates(t *testing.T) {
	logins := 0
	calls := []string{}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/subtitles/gettoken" {
			logins++
			fmt.Fprintf(w, `{"Token":"t%d","UserId":42,"ExpirationDate":"2026-09-04T14:00:00Z"}`, logins)
			return
		}
		page := r.URL.Query().Get("pg")
		calls = append(calls, page)
		if page == "2" && logins == 1 {
			w.WriteHeader(401)
			return
		}
		id := 101
		if page == "2" {
			id = 102
		}
		fmt.Fprintf(w, `{"PagesAvailable":2,"SubtitleResults":[{"Id":%d,"Lang":"Hrvatski","Link":"/download/x","Title":"Example Show","Season":1,"Episode":2}]}`, id)
	})
	clock := testutil.NewClock(time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC))
	gate := baseprovider.NewGate(&testStateStore{states: map[string]store.ProviderState{}}, clock, 1)
	gate.Configure("titlovi", 1000, 10, 1)
	httpClient := &http.Client{Transport: reviewTransport(func(r *http.Request) (*http.Response, error) {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w.Result(), nil
	})}
	c, err := New(Config{Username: "user", Password: "pass"}, baseprovider.Client{HTTP: httpClient, Gate: gate, Clock: clock, ProviderID: "titlovi", ProviderType: "titlovi"}, clock)
	if err != nil {
		t.Fatal(err)
	}
	got, err := c.Search(context.Background(), baseprovider.SearchQuery{Media: episodeMedia(), Language: "hr", Mode: baseprovider.SearchBroad})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || len(calls) != 3 || calls[0] != "" || calls[1] != "2" || calls[2] != "2" || got[1].ResultID != "102" {
		t.Fatalf("pagination = %v candidates=%+v", calls, got)
	}
	if got[0].ResultID != "101" {
		t.Errorf("lost first-page candidate after refresh")
	}
}
func TestUnknownResponseIdentityDoesNotInheritQueryIMDb(t *testing.T) {
	media := episodeMedia()
	media.ReleaseGroup = "GROUP"
	media.Source = "WEB-DL"
	media.Resolution = "1080p"
	c := &Client{id: "titlovi"}
	got := c.normalize(baseprovider.SearchQuery{Media: media, Language: "hr"}, []searchItem{{ID: 1, Language: "Hrvatski", Link: "/download/1", Title: "Different Show", Year: 1999, Season: 1, Episode: 2, Release: "Different.Show.S01E02.1080p.WEB-DL-GROUP"}})
	score := match.Evaluate(media, got[0], "hr")
	t.Logf("candidate imdb=%s score=%+v", got[0].ExternalIDs.IMDb, score)
	if got[0].ExternalIDs.IMDb != "" {
		t.Error("response without IMDb acquired the query IMDb")
	}
}

func TestWrongMovieYearIsRejectedWithoutInventedIMDb(t *testing.T) {
	media := domain.Media{Ref: domain.MediaRef{Kind: domain.MediaMovie}, Title: "Target Movie", Year: 2024, ExternalIDs: domain.ExternalIDs{IMDb: "tt1234567"}, ReleaseGroup: "GROUP", Source: "WEB-DL", Resolution: "1080p", StreamingService: "Netflix", Edition: "Extended"}
	c := &Client{id: "titlovi"}
	got := c.normalize(baseprovider.SearchQuery{Media: media, Language: "hr"}, []searchItem{{ID: 1, Language: "Hrvatski", Link: "/download/1", Title: "Different Movie", Year: 1999, Release: "Different.Movie.1999.1080p.NF.WEB-DL.EXTENDED-GROUP"}})
	score := match.Evaluate(media, got[0], "hr")
	t.Logf("candidate imdb=%s score=%+v", got[0].ExternalIDs.IMDb, score)
	if len(score.RejectedReasons) == 0 {
		t.Error("wrong movie year was accepted due to invented IMDb identity")
	}
}

func TestLoginRejectionDisablesInstance(t *testing.T) {
	calls := 0
	clock := testutil.NewClock(time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC))
	gate := baseprovider.NewGate(&testStateStore{states: map[string]store.ProviderState{}}, clock, 1)
	gate.Configure("titlovi", 1000, 10, 1)
	client, err := New(Config{Username: "user", Password: "pass"}, baseprovider.Client{HTTP: &http.Client{Transport: reviewTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		w := httptest.NewRecorder()
		w.WriteHeader(401)
		return w.Result(), nil
	})}, Gate: gate, Clock: clock, ProviderID: "titlovi", ProviderType: "titlovi"}, clock)
	if err != nil {
		t.Fatal(err)
	}
	q := baseprovider.SearchQuery{Media: episodeMedia(), Language: "hr", Mode: baseprovider.SearchBroad}
	for i := 0; i < 2; i++ {
		if _, err := client.Search(context.Background(), q); err == nil {
			t.Fatal("login should fail")
		}
	}
	if calls != 1 {
		t.Fatalf("repeated rejected login: %d", calls)
	}
}
