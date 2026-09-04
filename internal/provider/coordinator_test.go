package provider

import (
	"context"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"subsyncd/internal/domain"
	"subsyncd/internal/store"
	"subsyncd/internal/testutil"
)

type fakeProvider struct {
	id           string
	capabilities Capabilities
	candidates   map[SearchMode][]domain.Candidate
	err          map[SearchMode]error
	mu           sync.Mutex
	calls        []SearchMode
	started      chan string
	release      <-chan struct{}
}

func (p *fakeProvider) ID() string                            { return p.id }
func (p *fakeProvider) Capabilities() Capabilities            { return p.capabilities }
func (p *fakeProvider) SupportsLanguage(domain.Language) bool { return true }
func (p *fakeProvider) Search(_ context.Context, query SearchQuery) ([]domain.Candidate, error) {
	p.mu.Lock()
	p.calls = append(p.calls, query.Mode)
	p.mu.Unlock()
	if p.started != nil {
		p.started <- p.id
	}
	if p.release != nil {
		<-p.release
	}
	return p.candidates[query.Mode], p.err[query.Mode]
}
func (p *fakeProvider) Download(context.Context, domain.Candidate, io.Writer) (DownloadMetadata, error) {
	return DownloadMetadata{}, nil
}

type memoryCache struct {
	mu      sync.Mutex
	entries map[string]store.ProviderCacheEntry
}

func (c *memoryCache) GetProviderCache(_ context.Context, key string, now time.Time) (store.ProviderCacheEntry, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[key]
	return entry, ok && entry.ExpiresAt.After(now), nil
}
func (c *memoryCache) PutProviderCache(_ context.Context, entry store.ProviderCacheEntry) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[entry.Key] = entry
	return nil
}

func TestCoordinatorRunsExactProvidersSequentiallyAndStopsOnExactResult(t *testing.T) {
	first := &fakeProvider{id: "first", capabilities: Capabilities{ExactFileHash: true}, candidates: map[SearchMode][]domain.Candidate{SearchExactHash: nil}}
	second := &fakeProvider{id: "second", capabilities: Capabilities{ExactFileHash: true}, candidates: map[SearchMode][]domain.Candidate{SearchExactHash: {{ProviderID: "second", ResultID: "exact", ExactHash: true}}}}
	third := &fakeProvider{id: "third", capabilities: Capabilities{ExactFileHash: true}, candidates: map[SearchMode][]domain.Candidate{SearchExactHash: {{ProviderID: "third", ResultID: "never", ExactHash: true}}}}
	coordinator := newTestCoordinator(first, second, third)

	result := coordinator.Search(context.Background(), SearchQuery{Media: testQueryMedia(), Language: "en"})
	if len(result.Candidates) != 1 || result.Candidates[0].ResultID != "exact" {
		t.Fatalf("candidates = %#v", result.Candidates)
	}
	if len(first.calls) != 1 || len(second.calls) != 1 || len(third.calls) != 0 {
		t.Fatalf("calls = %#v / %#v / %#v", first.calls, second.calls, third.calls)
	}
}

func TestCoordinatorRunsBroadSearchesConcurrentlyAndIsolatesErrors(t *testing.T) {
	started := make(chan string, 2)
	release := make(chan struct{})
	first := &fakeProvider{id: "first", candidates: map[SearchMode][]domain.Candidate{SearchBroad: {{ProviderID: "first", ResultID: "one"}}}, started: started, release: release}
	second := &fakeProvider{id: "second", candidates: map[SearchMode][]domain.Candidate{}, err: map[SearchMode]error{SearchBroad: context.DeadlineExceeded}, started: started, release: release}
	coordinator := newTestCoordinator(first, second)
	done := make(chan SearchResult, 1)
	go func() {
		done <- coordinator.Search(context.Background(), SearchQuery{Media: testQueryMedia(), Language: "en"})
	}()
	for range 2 {
		<-started
	}
	close(release)
	result := <-done
	if len(result.Candidates) != 1 || result.Candidates[0].ProviderID != "first" || result.Errors["second"] == nil {
		t.Fatalf("result = %#v", result)
	}
}

func TestCoordinatorClearsExactPhaseFailureAfterSuccessfulBroadSearch(t *testing.T) {
	provider := &fakeProvider{
		id:           "only",
		capabilities: Capabilities{ExactFileHash: true},
		candidates:   map[SearchMode][]domain.Candidate{SearchBroad: {}},
		err:          map[SearchMode]error{SearchExactHash: context.DeadlineExceeded},
	}
	result := newTestCoordinator(provider).Search(context.Background(), SearchQuery{Media: testQueryMedia(), Language: "en"})
	if len(result.Candidates) != 0 || len(result.Errors) != 0 {
		t.Fatalf("successful broad phase retained stale exact failure: %#v", result)
	}
}

func TestCoordinatorCachesNormalizedResultsForSixHours(t *testing.T) {
	provider := &fakeProvider{id: "only", candidates: map[SearchMode][]domain.Candidate{SearchBroad: {{ProviderID: "only", ResultID: "one", DownloadRef: "opaque-id"}}}}
	coordinator := newTestCoordinator(provider)
	query := SearchQuery{Media: testQueryMedia(), Language: "en"}
	first := coordinator.Search(context.Background(), query)
	second := coordinator.Search(context.Background(), query)
	if len(first.Candidates) != 1 || len(second.Candidates) != 1 || len(provider.calls) != 1 {
		t.Fatalf("candidate counts/calls = %d/%d/%#v", len(first.Candidates), len(second.Candidates), provider.calls)
	}
	coordinator.Clock.(*testutil.Clock).Advance(6 * time.Hour)
	coordinator.Search(context.Background(), query)
	if len(provider.calls) != 2 {
		t.Fatalf("calls after expiry = %#v", provider.calls)
	}
}

func TestCoordinatorNeverCachesDownloadURLs(t *testing.T) {
	provider := &fakeProvider{id: "only", candidates: map[SearchMode][]domain.Candidate{SearchBroad: {{ProviderID: "only", ResultID: "one", DownloadRef: "https://signed.example/subtitle?token=secret"}}}}
	coordinator := newTestCoordinator(provider)
	coordinator.Search(context.Background(), SearchQuery{Media: testQueryMedia(), Language: "en"})
	cache := coordinator.Cache.(*memoryCache)
	for _, entry := range cache.entries {
		if strings.Contains(string(entry.ResultsJSON), "signed.example") || strings.Contains(string(entry.ResultsJSON), "secret") {
			t.Fatalf("cache leaked temporary download URL: %s", entry.ResultsJSON)
		}
	}
}

func newTestCoordinator(providers ...Provider) *Coordinator {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	return &Coordinator{Providers: providers, Cache: &memoryCache{entries: map[string]store.ProviderCacheEntry{}}, Clock: testutil.NewClock(now)}
}

func testQueryMedia() domain.Media {
	return domain.Media{Ref: domain.MediaRef{Instance: "sonarr", Kind: domain.MediaEpisode, FileID: 42}, Fingerprint: domain.MediaFingerprint{Path: "/media/show.mkv", FileID: 42, Size: 100, ModTime: time.Unix(0, 1)}, ReleaseName: "Show.S01E01.1080p.WEB-DL-GROUP"}
}
