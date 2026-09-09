package provider

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"subsyncd/internal/domain"
	"subsyncd/internal/observability"
)

func TestDeduplicationAndCacheRetainExplicitReleaseEvidence(t *testing.T) {
	var candidates []domain.Candidate
	if err := json.Unmarshal([]byte(`[{"provider_id":"only","result_id":"one","language":"en","release_groups":["groupa"],"resolutions":["720p"]},{"provider_id":"only","result_id":"one","language":"en","release_groups":["groupb","groupa"],"resolutions":["1080p","720p"]}]`), &candidates); err != nil {
		t.Fatal(err)
	}
	p := &fakeProvider{id: "only", candidates: map[SearchMode][]domain.Candidate{SearchBroad: candidates}}
	c := newTestCoordinator(p)
	q := SearchQuery{Mode: SearchBroad, Language: "en", Media: testQueryMedia()}
	c.Search(context.Background(), q)
	result := c.Search(context.Background(), q)
	payload, err := json.Marshal(result.Candidates)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.calls) != 1 || len(result.Candidates) != 1 || !strings.Contains(string(payload), `"release_groups":["groupa","groupb"]`) || !strings.Contains(string(payload), `"resolutions":["720p","1080p"]`) {
		t.Fatalf("cache lost evidence: calls=%d %s", len(p.calls), payload)
	}
	before, _ := json.Marshal(candidates)
	DeduplicateCandidates(candidates)
	after, _ := json.Marshal(candidates)
	if string(before) != string(after) {
		t.Fatal("merge mutated original evidence")
	}
}

type versionedProvider struct {
	Provider
	version string
}

func (p versionedProvider) SearchCacheVersion() string { return p.version }

func TestProviderCacheVersionRefreshesObservedProviderOnly(t *testing.T) {
	p := &fakeProvider{id: "only", candidates: map[SearchMode][]domain.Candidate{}}
	other := &fakeProvider{id: "other", candidates: map[SearchMode][]domain.Candidate{}}
	c := newTestCoordinator(p, other)
	q := SearchQuery{Mode: SearchBroad, Language: "en", Media: testQueryMedia()}
	c.Search(context.Background(), q) // Legacy empty results must also refresh.
	c.Providers = []Provider{Observe(versionedProvider{Provider: p, version: "repaired"}, observability.Discard()), other}
	c.Search(context.Background(), q)
	c.Search(context.Background(), q)
	if len(p.calls) != 2 || len(other.calls) != 1 {
		t.Fatalf("provider-specific refresh: changed=%d unchanged=%d", len(p.calls), len(other.calls))
	}
}
