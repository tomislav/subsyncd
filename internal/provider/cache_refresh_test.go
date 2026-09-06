package provider

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"subsyncd/internal/pack"
	"subsyncd/internal/store"
	"testing"

	"subsyncd/internal/domain"
)

func TestCoordinatorRefreshesLossyCacheAfterRestart(t *testing.T) {
	for _, ref := range []string{"/download/?id=42&token=private", "https://signed.example/subtitle?token=private"} {
		t.Run(ref, func(t *testing.T) {
			p := &fakeProvider{id: "only", candidates: map[SearchMode][]domain.Candidate{SearchBroad: {{ProviderID: "only", ResultID: "42", DownloadRef: ref}}}}
			c := newTestCoordinator(p)
			q := SearchQuery{Media: testQueryMedia(), Language: "hr", Mode: SearchBroad}
			c.Search(context.Background(), q)
			// A new coordinator represents a process restart with the same durable cache.
			restarted := newTestCoordinator(p)
			restarted.Cache = c.Cache
			got := restarted.Search(context.Background(), q)
			if len(p.calls) != 2 || len(got.Candidates) != 1 || got.Candidates[0].DownloadRef != ref {
				t.Fatalf("cache replay lost the usable download reference: calls=%d candidates=%#v", len(p.calls), got.Candidates)
			}
			for _, e := range c.Cache.(*memoryCache).entries {
				if strings.Contains(string(e.ResultsJSON), "private") || strings.Contains(string(e.ResultsJSON), "signed.example") {
					t.Fatal("cache leaked temporary reference")
				}
			}
		})
	}
}

func TestCoordinatorRefreshesStrippedPackMemberReferences(t *testing.T) {
	p := &fakeProvider{id: "only", candidates: map[SearchMode][]domain.Candidate{SearchBroad: {{ProviderID: "only", ResultID: "42", DownloadRef: "opaque", Pack: &domain.PackInfo{DirectMembers: []domain.PackMemberRef{{ID: "member", DownloadRef: "/member?token=private"}}}}}}}
	c := newTestCoordinator(p)
	q := SearchQuery{Media: testQueryMedia(), Language: "hr", Mode: SearchBroad}
	c.Search(context.Background(), q)
	got := c.Search(context.Background(), q)
	if len(p.calls) != 2 || got.Candidates[0].Pack.DirectMembers[0].DownloadRef != "/member?token=private" {
		t.Fatal("stripped pack member reused")
	}
}

func TestCoordinatorRetainsEmptySearchCache(t *testing.T) {
	p := &fakeProvider{id: "only", candidates: map[SearchMode][]domain.Candidate{}}
	c := newTestCoordinator(p)
	q := SearchQuery{Media: testQueryMedia(), Language: "hr", Mode: SearchBroad}
	c.Search(context.Background(), q)
	c.Search(context.Background(), q)
	if len(p.calls) != 1 {
		t.Fatal("empty search was not cached")
	}
}

func TestCoordinatorRefreshFailureDoesNotReturnStrippedCandidates(t *testing.T) {
	p := &fakeProvider{id: "only", candidates: map[SearchMode][]domain.Candidate{SearchBroad: {{ProviderID: "only", ResultID: "42", DownloadRef: "/download/?id=42"}}}}
	c := newTestCoordinator(p)
	q := SearchQuery{Media: testQueryMedia(), Language: "hr", Mode: SearchBroad}
	c.Search(context.Background(), q)
	p.err = map[SearchMode]error{SearchBroad: context.DeadlineExceeded}
	got := c.Search(context.Background(), q)
	if len(got.Candidates) != 0 || got.Errors["only"] == nil {
		t.Fatal("refresh failure fell back to incomplete cached candidates")
	}
}

func TestCoordinatorRejectsOldArrayCachePayload(t *testing.T) {
	p := &fakeProvider{id: "only", candidates: map[SearchMode][]domain.Candidate{SearchBroad: {{ProviderID: "only", ResultID: "fresh"}}}}
	c := newTestCoordinator(p)
	q := SearchQuery{Media: testQueryMedia(), Language: "hr", Mode: SearchBroad}
	c.Search(context.Background(), q)
	for k, e := range c.Cache.(*memoryCache).entries {
		e.ResultsJSON, _ = json.Marshal([]domain.Candidate{{ProviderID: "only", ResultID: "stale", DownloadRef: "/download/"}})
		c.Cache.(*memoryCache).entries[k] = e
	}
	got := c.Search(context.Background(), q)
	if len(p.calls) != 2 || got.Candidates[0].ResultID != "fresh" {
		t.Fatal("legacy cache replayed")
	}
}

// Exercise acquisition after closing and reopening SQLite, with a local endpoint
// that returns the same invalid body as Titlovi when the identifying query is lost.
func TestCoordinatorRestartRefreshesBeforeExtractingDownload(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("id") != "42" {
			_, _ = io.WriteString(w, "0")
			return
		}
		_, _ = io.WriteString(w, "1\n00:00:01,000 --> 00:00:02,000\nExample\n")
	}))
	defer server.Close()
	p := &fakeProvider{id: "only", candidates: map[SearchMode][]domain.Candidate{SearchBroad: {{ProviderID: "only", ResultID: "42", DownloadRef: server.URL + "/subtitle.srt?id=42"}}}}
	c := newTestCoordinator(p)
	q := SearchQuery{Media: testQueryMedia(), Language: "hr", Mode: SearchBroad}
	database := filepath.Join(t.TempDir(), "state.db")
	db, err := store.Open(context.Background(), database)
	if err != nil {
		t.Fatal(err)
	}
	c.Cache = db.Repository()
	c.Search(context.Background(), q)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = store.Open(context.Background(), database)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	restarted := newTestCoordinator(p)
	restarted.Cache = db.Repository()
	got := restarted.Search(context.Background(), q)
	if len(got.Errors) != 0 || len(got.Candidates) != 1 {
		t.Fatalf("search: %#v", got)
	}
	response, err := server.Client().Get(got.Candidates[0].DownloadRef)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	candidate := got.Candidates[0]
	candidate.DownloadRef = "subtitle.srt"
	manifest, err := pack.Extract(context.Background(), candidate, response.Body, response.ContentLength, filepath.Join(t.TempDir(), "extracted"), pack.DefaultLimits())
	if err != nil || len(manifest.Members) != 1 {
		t.Fatalf("restarted acquisition failed: %v", err)
	}
}
