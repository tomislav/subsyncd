package workflow

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"subsyncd/internal/domain"
	"subsyncd/internal/match"
	"subsyncd/internal/provider"
	"subsyncd/internal/provider/subdl"
	"subsyncd/internal/store"
)

func TestSubDLResponseIdentityControlsLapseBypass(t *testing.T) {
	for _, test := range []struct {
		name, identity string
		wantScore      int
		wantBypass     bool
	}{
		{"missing identity", "[]", 65, false},
		{"returned identity", `[{"name":"Example Show","year":2020}]`, 80, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprintf(w, `{"status":true,"results":%s,"subtitles":[{"url":"/subtitle/example.zip","language":"EN","season":1,"episode":2,"releases":["S01E02.WEB-DL.1080p-GROUP"]}]}`, test.identity)
			}))
			defer server.Close()
			db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			clock := provider.SystemClock{}
			gate := provider.NewGate(db.Repository(), clock, 1)
			gate.Configure("subdl", 1000, 10, 1)
			client, err := subdl.New(subdl.Config{APIKey: "fixture-key", BaseURL: server.URL, AllowInsecureForTests: true}, provider.Client{HTTP: server.Client(), Gate: gate, Clock: clock, ProviderID: "subdl", ProviderType: "subdl"}, clock)
			if err != nil {
				t.Fatal(err)
			}
			media := domain.Media{Ref: domain.MediaRef{Kind: domain.MediaEpisode}, Title: "Example Show", Year: 2020, Season: 1, Episode: 2, Source: "WEB-DL", ReleaseGroup: "GROUP", Resolution: "1080p"}
			candidates, err := client.Search(context.Background(), provider.SearchQuery{Media: media, Language: "en", Mode: provider.SearchBroad})
			if err != nil || len(candidates) != 1 {
				t.Fatalf("candidates = %#v, %v", candidates, err)
			}
			candidate := candidates[0]
			if candidate.Pack != nil {
				t.Fatal("fixture must exercise a non-pack first install")
			}
			score := match.Evaluate(media, candidate, "en")
			if score.Total != test.wantScore {
				t.Fatalf("score = %#v, want %d", score, test.wantScore)
			}
			for _, threshold := range []int{75, 60} {
				policy := DefaultLapsePolicy()
				policy.BypassScore = threshold
				if got := canBypassLapse(media, candidate, score, false, policy); got != test.wantBypass {
					t.Fatalf("bypass at threshold %d = %t, want %t", threshold, got, test.wantBypass)
				}
			}
		})
	}
}

func TestAlternativeEvidenceKeepsEditionAndEpisodeBypassGates(t *testing.T) {
	media := domain.Media{Ref: domain.MediaRef{Kind: domain.MediaEpisode}, Title: "Example Show", Year: 2020, Season: 1, Episode: 2, ReleaseGroup: "GROUPB", Source: "bluray", Resolution: "2160p", Edition: "Extended"}
	matching := "Example.Show.2020.S01E02.EXTENDED.2160p.BluRay-GROUPB"
	conflicting := "Example.Show.2020.S01E03.DIRECTORS.CUT.720p.WEB-DL-GROUPA"
	unknown := "Example.Show.2020.S01E02.2160p.BluRay-GROUPB"
	for _, test := range []struct {
		name, first, second    string
		wantReject, wantBypass bool
	}{
		{"matching alternative", matching, conflicting, false, true},
		{"reversed alternatives", conflicting, matching, false, true},
		{"conflict and unknown", conflicting, unknown, true, false},
		{"unknown and conflict", unknown, conflicting, true, false},
		{"unknown edition", unknown, unknown, false, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			candidate := domain.Candidate{Language: "en", Kind: domain.MediaEpisode, Title: media.Title, Year: media.Year, ReleaseNames: []string{test.first + " / " + test.second}}
			score := match.Evaluate(media, candidate, "en")
			if (len(score.RejectedReasons) > 0) != test.wantReject {
				t.Fatalf("score = %#v", score)
			}
			if !test.wantReject && canBypassLapse(media, candidate, score, false, DefaultLapsePolicy()) != test.wantBypass {
				t.Fatalf("unexpected bypass: score=%#v", score)
			}
			candidate.Episode = 3
			if score := match.Evaluate(media, candidate, "en"); len(score.RejectedReasons) == 0 {
				t.Fatalf("explicit episode conflict accepted: %#v", score)
			}
		})
	}
}
