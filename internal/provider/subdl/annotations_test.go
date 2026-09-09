package subdl

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"subsyncd/internal/domain"
	baseprovider "subsyncd/internal/provider"
	"testing"
)

func TestSearchAnnotations(t *testing.T) {
	for _, tt := range []struct {
		name, comment string
		hi, forced    bool
	}{
		{"Example.zip", "Forced subtitles for foreign dialogue", false, true},
		{"Example.zip", "SDH subtitles", true, false},
		{"Example.zip", "Removed SDH subtitles", false, false},
		{"Example.zip", "Stripped hearing-impaired annotations", false, false},
		{"Example.zip", "No forced or SDH subtitles", false, false},
		{"Example.zip", "No SDH or forced subtitles", false, false},
		{"Example.zip", "No SDH and forced subtitles", false, false},
		{"Example.zip", "No SDH but forced subtitles", false, true},
		{"Example.Non.SDH.zip", "", false, false},
		{"Example.Not.Forced.zip", "", false, false},
		{"Example.zip", "SDH subtitles with no forced parts", true, false},
		{"Example.zip", "Forced subtitles without SDH", false, true},
		{"Example.zip", "SDH removed with forced subtitles", false, true},
		{"Example_HI_English.zip", "", true, false},
		{"Example.zip", "Hearing-impaired subtitles", true, false},
		{"Example.zip", "Not forced; non-SDH; no hearing impaired subtitles", false, false},
		{"Example.zip", "Forced subtitles removed; SDH removed", false, false},
		{"highlights.zip", "reinforced English", false, false},
		{"Example.zip", "Forced subtitles only", false, true},
		{"Example.zip", "Not sure if forced or SDH", false, false},
		{"Example.zip", "Forced subtitles; SDH removed", false, true},
	} {
		t.Run(tt.name+tt.comment, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Query().Get("comment") != "1" {
					t.Error("missing comment request")
				}
				fmt.Fprintf(w, `{"status":true,"subtitles":[{"name":%q,"comment":%q,"url":"/a.zip","language":"EN"}]}`, tt.name, tt.comment)
			}))
			defer server.Close()
			got, err := newTestClient(t, server, 1<<20).Search(context.Background(), baseprovider.SearchQuery{Media: domain.Media{Ref: domain.MediaRef{Kind: domain.MediaMovie}}, Language: "en", Mode: baseprovider.SearchBroad})
			if err != nil || len(got) != 1 {
				t.Fatalf("%v %v", got, err)
			}
			if got[0].Forced != tt.forced || got[0].HearingImpaired != tt.hi {
				t.Fatalf("flags forced=%v HI=%v", got[0].Forced, got[0].HearingImpaired)
			}
		})
	}
}

func TestDirectMemberPreservesParentHI(t *testing.T) {
	c := &Client{}
	got := c.normalize(baseprovider.SearchQuery{Media: episodeMedia(), Language: "en"}, []searchItem{{URL: "/a.zip", Language: "EN", Hearing: true, UnpackFiles: []unpackFile{{FileID: "2", URL: "/b.srt", Language: "EN", Season: 1, Episode: 2}}}})
	if len(got) != 1 || !got[0].HearingImpaired {
		t.Fatalf("%#v", got)
	}
}

func TestMovieAlternateIDOnlyAfterEmpty(t *testing.T) {
	for _, tt := range []struct {
		name, body    string
		status, calls int
		wantErr       bool
	}{
		{"empty", `{"status":true,"subtitles":[]}`, 200, 2, false},
		{"no film", `{"status":false,"error":"Can't find film"}`, 200, 2, false},
		{"result", `{"status":true,"subtitles":[{"url":"/a.zip","language":"EN"}]}`, 200, 1, false},
		{"invalid", `{`, 200, 1, true},
		{"unknown missing resource", `{"status":false,"error":"API key not found"}`, 200, 1, true},
		{"auth", `{}`, 403, 1, true},
		{"quota", `{"status":false,"error":"daily_limit"}`, 200, 1, true},
		{"server", `{}`, 503, 1, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				q := r.URL.Query()
				if calls == 1 {
					if q.Get("imdb_id") != "tt123" || q.Has("tmdb_id") {
						t.Errorf("first %v", q)
					}
					w.WriteHeader(tt.status)
					io.WriteString(w, tt.body)
					return
				}
				if q.Has("imdb_id") || q.Get("tmdb_id") != "456" {
					t.Errorf("fallback %v", q)
				}
				io.WriteString(w, `{"status":true,"subtitles":[{"url":"/b.zip","language":"EN"}]}`)
			}))
			defer server.Close()
			_, err := newTestClient(t, server, 1<<20).Search(context.Background(), baseprovider.SearchQuery{Media: domain.Media{Ref: domain.MediaRef{Kind: domain.MediaMovie}, ExternalIDs: domain.ExternalIDs{IMDb: "tt123", TMDB: 456}}, Language: "en", Mode: baseprovider.SearchBroad})
			if calls != tt.calls || (err != nil) != tt.wantErr {
				t.Fatalf("calls=%d err=%v", calls, err)
			}
		})
	}
}

func TestMovieFallbackRequiresBothIDsAndHonorsCancellation(t *testing.T) {
	for _, ids := range []domain.ExternalIDs{{IMDb: "tt123"}, {TMDB: 456}, {}} {
		calls := 0
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls++
			io.WriteString(w, `{"status":true,"subtitles":[]}`)
		}))
		client := newTestClient(t, server, 1<<20)
		query := baseprovider.SearchQuery{Media: domain.Media{Ref: domain.MediaRef{Kind: domain.MediaMovie}, ExternalIDs: ids}, Language: "en", Mode: baseprovider.SearchBroad}
		if _, err := client.Search(context.Background(), query); err != nil || calls != 1 {
			t.Fatalf("calls=%d err=%v", calls, err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		query.Media.ExternalIDs = domain.ExternalIDs{IMDb: "tt123", TMDB: 456}
		if _, err := client.Search(ctx, query); err == nil || calls != 1 {
			t.Fatalf("cancel calls=%d err=%v", calls, err)
		}
		server.Close()
	}
}

func TestCachedCandidateRequiresAnnotationEvidence(t *testing.T) {
	c := &Client{}
	validator, ok := any(c).(interface{ CanReuseCachedCandidate(domain.Candidate) bool })
	if !ok {
		t.Fatal("missing cached-candidate annotation validation")
	}
	if validator.CanReuseCachedCandidate(domain.Candidate{}) {
		t.Fatal("legacy cache accepted")
	}
	got := c.normalize(baseprovider.SearchQuery{Media: domain.Media{Ref: domain.MediaRef{Kind: domain.MediaMovie}}, Language: "en"}, []searchItem{{URL: "/a.zip", Language: "EN"}})
	if len(got) != 1 || !validator.CanReuseCachedCandidate(got[0]) {
		t.Fatal("current candidate rejected")
	}
}
