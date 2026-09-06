package provider

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"subsyncd/internal/domain"
	"subsyncd/internal/observability"
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

func TestCoordinatorExactModeReturnsEveryExactCandidateInProviderOrder(t *testing.T) {
	first := &fakeProvider{id: "first", capabilities: Capabilities{ExactFileHash: true}, candidates: map[SearchMode][]domain.Candidate{
		SearchExactHash: {
			{ProviderID: "first", ResultID: "not-exact"},
			{ProviderID: "first", ResultID: "exact-a", ExactHash: true},
		},
	}}
	second := &fakeProvider{id: "second", capabilities: Capabilities{ExactFileHash: true}, candidates: map[SearchMode][]domain.Candidate{
		SearchExactHash: {{ProviderID: "second", ResultID: "exact-b", ExactHash: true}},
	}}
	result := newTestCoordinator(first, second).Search(context.Background(), SearchQuery{
		Media: testQueryMedia(), Language: "en", Mode: SearchExactHash,
	})
	if got := candidateIDs(result.Candidates); !slices.Equal(got, []string{"exact-a", "exact-b"}) {
		t.Fatalf("exact candidates = %#v", got)
	}
	if !slices.Equal(first.calls, []SearchMode{SearchExactHash}) || !slices.Equal(second.calls, []SearchMode{SearchExactHash}) {
		t.Fatalf("calls = %#v / %#v", first.calls, second.calls)
	}
}

func TestCoordinatorBroadModeNeverCallsExactSearch(t *testing.T) {
	item := &fakeProvider{id: "only", capabilities: Capabilities{ExactFileHash: true}, candidates: map[SearchMode][]domain.Candidate{
		SearchBroad: {{ProviderID: "only", ResultID: "broad"}},
	}}
	result := newTestCoordinator(item).Search(context.Background(), SearchQuery{
		Media: testQueryMedia(), Language: "en", Mode: SearchBroad,
	})
	if len(result.Candidates) != 1 || !slices.Equal(item.calls, []SearchMode{SearchBroad}) {
		t.Fatalf("result/calls = %#v / %#v", result, item.calls)
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
		done <- coordinator.Search(context.Background(), SearchQuery{Media: testQueryMedia(), Language: "en", Mode: SearchBroad})
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

func TestCoordinatorCachesNormalizedResultsForSixHours(t *testing.T) {
	provider := &fakeProvider{id: "only", candidates: map[SearchMode][]domain.Candidate{SearchBroad: {{ProviderID: "only", ResultID: "one", DownloadRef: "opaque-id"}}}}
	coordinator := newTestCoordinator(provider)
	query := SearchQuery{Media: testQueryMedia(), Language: "en", Mode: SearchBroad}
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

func TestCoordinatorDeduplicatesStableCandidateIdentityBeforeCaching(t *testing.T) {
	provider := &fakeProvider{id: "only", candidates: map[SearchMode][]domain.Candidate{SearchBroad: {
		{ProviderID: "only", ResultID: "same", ReleaseNames: []string{"Release.A"}, Rating: 0.4, Popularity: 0.3, DownloadCount: 10},
		{ProviderID: "only", ResultID: "same", Language: "en", Kind: domain.MediaEpisode, Title: "Show", Year: 2026, Season: 1, Episode: 2, AbsoluteEpisode: 14, ExternalIDs: domain.ExternalIDs{IMDb: "tt123", TMDB: 456, TVDB: 789}, ReleaseNames: []string{"Release.B", "Release.A"}, Rating: 0.8, Popularity: 0.7, DownloadCount: 20},
		{ProviderID: "only", ResultID: "same", Language: "hr", Kind: domain.MediaMovie, Title: "Conflicting title", Year: 1999, Season: 9, Episode: 8, AbsoluteEpisode: 77, ExternalIDs: domain.ExternalIDs{IMDb: "tt999", TMDB: 999, TVDB: 999}},
	}}}
	coordinator := newTestCoordinator(provider)
	query := SearchQuery{Media: testQueryMedia(), Language: "en", Mode: SearchBroad}

	first := coordinator.Search(context.Background(), query)
	second := coordinator.Search(context.Background(), query)
	for _, result := range []SearchResult{first, second} {
		if len(result.Candidates) != 1 {
			t.Fatalf("deduplicated candidates = %#v", result.Candidates)
		}
		candidate := result.Candidates[0]
		if candidate.ResultID != "same" || candidate.Language != "en" || candidate.Kind != domain.MediaEpisode || candidate.Title != "Show" || candidate.Year != 2026 || candidate.Season != 1 || candidate.Episode != 2 || candidate.AbsoluteEpisode != 14 || candidate.ExternalIDs != (domain.ExternalIDs{IMDb: "tt123", TMDB: 456, TVDB: 789}) || !slices.Equal(candidate.ReleaseNames, []string{"Release.A", "Release.B"}) || candidate.Rating != 0.8 || candidate.Popularity != 0.7 || candidate.DownloadCount != 20 {
			t.Fatalf("merged candidate = %#v", candidate)
		}
	}
	cache := coordinator.Cache.(*memoryCache)
	for _, entry := range cache.entries {
		var cached cachedSearchResults
		if err := json.Unmarshal(entry.ResultsJSON, &cached); err != nil || len(cached.Candidates) != 1 {
			t.Fatalf("cached candidates = %#v, %v", cached, err)
		}
	}
}

func TestCoordinatorDoesNotReuseInferredIdentityCache(t *testing.T) {
	p := &fakeProvider{id: "only", candidates: map[SearchMode][]domain.Candidate{SearchBroad: {{ProviderID: "only", ResultID: "fresh", Language: "en"}}}}
	c := newTestCoordinator(p)
	q := SearchQuery{Media: testQueryMedia(), Language: "en", Mode: SearchBroad}
	q.Media.Title, q.Media.Year = "Example Movie", 2020
	f := q.Media.Fingerprint
	// Historical v2 keys can contain SubDL identity copied from the request.
	legacy := fmt.Sprintf("candidate-v2\x00%s\x00%s\x00%s\x00%s\x00%d\x00%d\x00%d\x00%s\x00%d\x00%d", p.ID(), q.Mode, q.Language, q.Media.Ref.Instance, q.Media.Ref.FileID, f.Size, f.ModTime.UnixNano(), q.Media.ReleaseName, q.Media.Season, q.Media.Episode)
	key := fmt.Sprintf("%x", sha256.Sum256([]byte(legacy)))
	encoded, err := json.Marshal([]domain.Candidate{{ProviderID: "only", ResultID: "stale", Language: "en", Title: q.Media.Title, Year: q.Media.Year}})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Cache.PutProviderCache(context.Background(), store.ProviderCacheEntry{Key: key, ProviderID: p.ID(), ResultsJSON: encoded, ExpiresAt: c.Clock.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	got := c.Search(context.Background(), q)
	if len(p.calls) != 1 || len(got.Candidates) != 1 || got.Candidates[0].ResultID != "fresh" {
		t.Fatalf("legacy inferred identity cache reused: calls=%v result=%#v", p.calls, got)
	}
}

func TestCoordinatorDoesNotReusePreTranslationExclusionCache(t *testing.T) {
	p := &fakeProvider{id: "only", candidates: map[SearchMode][]domain.Candidate{SearchBroad: {{ProviderID: "only", ResultID: "fresh", Language: "en"}}}}
	c := newTestCoordinator(p)
	q := SearchQuery{Media: testQueryMedia(), Language: "en", Mode: SearchBroad}
	q.Media.Title, q.Media.Year = "Example Movie", 2020
	f := q.Media.Fingerprint
	// Historical v3 results predate explicit OpenSubtitles translation exclusions.
	legacy := fmt.Sprintf("candidate-v3\x00%s\x00%s\x00%s\x00%s\x00%d\x00%d\x00%d\x00%s\x00%d\x00%d", p.ID(), q.Mode, q.Language, q.Media.Ref.Instance, q.Media.Ref.FileID, f.Size, f.ModTime.UnixNano(), q.Media.ReleaseName, q.Media.Season, q.Media.Episode)
	key := fmt.Sprintf("%x", sha256.Sum256([]byte(legacy)))
	encoded, err := json.Marshal([]domain.Candidate{{ProviderID: "only", ResultID: "stale", Language: "en", Title: q.Media.Title, Year: q.Media.Year}})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Cache.PutProviderCache(context.Background(), store.ProviderCacheEntry{Key: key, ProviderID: p.ID(), ResultsJSON: encoded, ExpiresAt: c.Clock.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	got := c.Search(context.Background(), q)
	if len(p.calls) != 1 || len(got.Candidates) != 1 || got.Candidates[0].ResultID != "fresh" {
		t.Fatalf("pre-exclusion cache reused: calls=%v result=%#v", p.calls, got)
	}
}

func TestCoordinatorDeduplicatesLegacyCachedCandidates(t *testing.T) {
	provider := &fakeProvider{id: "only", candidates: map[SearchMode][]domain.Candidate{}}
	coordinator := newTestCoordinator(provider)
	query := SearchQuery{Media: testQueryMedia(), Language: "en", Mode: SearchBroad}
	candidates := []domain.Candidate{
		{ProviderID: "only", ResultID: "same", ReleaseNames: []string{"Release.A"}},
		{ProviderID: "only", ResultID: "same", ReleaseNames: []string{"Release.B"}},
	}
	payload, err := json.Marshal(cachedSearchResults{Version: 1, Candidates: candidates})
	if err != nil {
		t.Fatal(err)
	}
	cache := coordinator.Cache.(*memoryCache)
	cache.entries[providerCacheKey(provider.ID(), query)] = store.ProviderCacheEntry{ResultsJSON: payload, ExpiresAt: coordinator.Clock.Now().Add(time.Hour)}

	result := coordinator.Search(context.Background(), query)
	if len(result.Candidates) != 1 || !slices.Equal(result.Candidates[0].ReleaseNames, []string{"Release.A", "Release.B"}) {
		t.Fatalf("legacy cached candidates = %#v", result.Candidates)
	}
	if len(provider.calls) != 0 {
		t.Fatalf("provider calls = %#v, want cache-only", provider.calls)
	}
}

func TestCoordinatorNeverCachesDownloadURLs(t *testing.T) {
	provider := &fakeProvider{id: "only", candidates: map[SearchMode][]domain.Candidate{SearchBroad: {{ProviderID: "only", ResultID: "one", DownloadRef: "https://signed.example/subtitle?token=secret"}}}}
	coordinator := newTestCoordinator(provider)
	coordinator.Search(context.Background(), SearchQuery{Media: testQueryMedia(), Language: "en", Mode: SearchBroad})
	cache := coordinator.Cache.(*memoryCache)
	for _, entry := range cache.entries {
		if strings.Contains(string(entry.ResultsJSON), "signed.example") || strings.Contains(string(entry.ResultsJSON), "secret") {
			t.Fatalf("cache leaked temporary download URL: %s", entry.ResultsJSON)
		}
	}
}

func TestCoordinatorNeverCachesCredentialBearingCandidateIDs(t *testing.T) {
	provider := &fakeProvider{id: "only", candidates: map[SearchMode][]domain.Candidate{SearchBroad: {{
		ProviderID:  "only",
		ResultID:    "/subtitle/movie.srt?api_key=secret",
		DownloadRef: "/subtitle/movie.srt?api_key=secret",
		Pack: &domain.PackInfo{DirectMembers: []domain.PackMemberRef{{
			ID:          "member",
			DownloadRef: "https://signed.example/member?token=secret",
		}}},
	}}}}
	coordinator := newTestCoordinator(provider)
	coordinator.Search(context.Background(), SearchQuery{Media: testQueryMedia(), Language: "en", Mode: SearchBroad})
	cache := coordinator.Cache.(*memoryCache)
	for _, entry := range cache.entries {
		if strings.Contains(string(entry.ResultsJSON), "api_key") || strings.Contains(string(entry.ResultsJSON), "secret") || strings.Contains(string(entry.ResultsJSON), "signed.example") {
			t.Fatalf("cache leaked candidate credential: %s", entry.ResultsJSON)
		}
		var cached cachedSearchResults
		if err := json.Unmarshal(entry.ResultsJSON, &cached); err != nil {
			t.Fatal(err)
		}
		candidates := cached.Candidates
		if len(candidates) != 1 || candidates[0].ResultID != "/subtitle/movie.srt" || candidates[0].DownloadRef != "/subtitle/movie.srt" || candidates[0].Pack == nil || candidates[0].Pack.DirectMembers[0].DownloadRef != "" {
			t.Fatalf("safe cached candidate = %#v", candidates)
		}
	}
}

func TestCoordinatorLogsSearchAttemptsAndCacheStateWithoutMediaDetails(t *testing.T) {
	var logs bytes.Buffer
	events, err := observability.New(&logs, observability.Options{Level: "debug", Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	item := &fakeProvider{id: "only", candidates: map[SearchMode][]domain.Candidate{SearchBroad: {{ProviderID: "only", ResultID: "one", DownloadRef: "opaque-id"}}}}
	coordinator := newTestCoordinator(item)
	coordinator.Events = events
	query := SearchQuery{Media: testQueryMedia(), Language: "en", Mode: SearchBroad}

	coordinator.Search(context.Background(), query)
	coordinator.Search(context.Background(), query)

	records := providerLogRecords(t, logs.String())
	started := providerEvents(records, "provider.search_started")
	completed := providerEvents(records, "provider.search_completed")
	if len(started) != 2 || len(completed) != 2 {
		t.Fatalf("search events started/completed = %d/%d, want 2/2: %s", len(started), len(completed), logs.String())
	}
	if completed[0]["cache_status"] != "miss" || completed[1]["cache_status"] != "hit" {
		t.Fatalf("cache statuses = %#v/%#v", completed[0]["cache_status"], completed[1]["cache_status"])
	}
	if completed[0]["outcome"] != "success" || completed[0]["candidate_count"] != float64(1) {
		t.Fatalf("first completion = %#v", completed[0])
	}
	if len(providerEvents(records, "provider.cache_miss")) != 1 || len(providerEvents(records, "provider.cache_hit")) != 1 {
		t.Fatalf("cache debug events missing: %s", logs.String())
	}
	for _, forbidden := range []string{"/media/show.mkv", "Show.S01E01", "signed.example", "token=secret"} {
		if strings.Contains(logs.String(), forbidden) {
			t.Fatalf("provider logs leaked %q: %s", forbidden, logs.String())
		}
	}
}

func TestCoordinatorClassifiesCanceledSearchAsWarning(t *testing.T) {
	var logs bytes.Buffer
	events, err := observability.New(&logs, observability.Options{Level: "debug", Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	item := &fakeProvider{id: "only", candidates: map[SearchMode][]domain.Candidate{}, err: map[SearchMode]error{SearchBroad: context.DeadlineExceeded}}
	coordinator := newTestCoordinator(item)
	coordinator.Events = events

	coordinator.Search(context.Background(), SearchQuery{Media: testQueryMedia(), Language: "en", Mode: SearchBroad})

	completed := providerEvents(providerLogRecords(t, logs.String()), "provider.search_completed")
	if len(completed) != 1 || completed[0]["outcome"] != "canceled" || completed[0]["level"] != "warn" {
		t.Fatalf("canceled completion = %#v", completed)
	}
}

func newTestCoordinator(providers ...Provider) *Coordinator {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	return &Coordinator{Providers: providers, Cache: &memoryCache{entries: map[string]store.ProviderCacheEntry{}}, Clock: testutil.NewClock(now)}
}

func testQueryMedia() domain.Media {
	return domain.Media{Ref: domain.MediaRef{Instance: "sonarr", Kind: domain.MediaEpisode, FileID: 42}, Fingerprint: domain.MediaFingerprint{Path: "/media/show.mkv", FileID: 42, Size: 100, ModTime: time.Unix(0, 1)}, ReleaseName: "Show.S01E01.1080p.WEB-DL-GROUP"}
}

func candidateIDs(candidates []domain.Candidate) []string {
	ids := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		ids = append(ids, candidate.ResultID)
	}
	return ids
}

func TestCoordinatorDoesNotReusePreProviderRepairEvidenceCache(t *testing.T) {
	p := &fakeProvider{id: "only", candidates: map[SearchMode][]domain.Candidate{SearchBroad: {{ProviderID: "only", ResultID: "fresh", Language: "en"}}}}
	c := newTestCoordinator(p)
	q := SearchQuery{Media: testQueryMedia(), Language: "en", Mode: SearchBroad}
	q.Media.Title, q.Media.Year = "Example Movie", 2020
	f := q.Media.Fingerprint
	// Historical v4 results contain fabricated Titlovi IDs and incorrect SubDL packs.
	legacy := fmt.Sprintf("candidate-v4\x00%s\x00%s\x00%s\x00%s\x00%d\x00%d\x00%d\x00%s\x00%d\x00%d", p.ID(), q.Mode, q.Language, q.Media.Ref.Instance, q.Media.Ref.FileID, f.Size, f.ModTime.UnixNano(), q.Media.ReleaseName, q.Media.Season, q.Media.Episode)
	key := fmt.Sprintf("%x", sha256.Sum256([]byte(legacy)))
	encoded, err := json.Marshal([]domain.Candidate{{ProviderID: "only", ResultID: "stale", Language: "en", Title: q.Media.Title, Year: q.Media.Year}})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Cache.PutProviderCache(context.Background(), store.ProviderCacheEntry{Key: key, ProviderID: p.ID(), ResultsJSON: encoded, ExpiresAt: c.Clock.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	got := c.Search(context.Background(), q)
	if len(p.calls) != 1 || len(got.Candidates) != 1 || got.Candidates[0].ResultID != "fresh" {
		t.Fatalf("pre-repair evidence cache reused: calls=%v result=%#v", p.calls, got)
	}
}
