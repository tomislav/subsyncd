package provider

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"subsyncd/internal/domain"
	"subsyncd/internal/observability"
	"subsyncd/internal/store"
	"subsyncd/internal/testutil"
)

type unavailableDownloadProvider struct {
	*fakeProvider
	unavailable error
}

func (p *unavailableDownloadProvider) CheckDownloadAvailability(context.Context) error {
	return p.unavailable
}

type skippedProviderAccessProbe struct {
	*fakeProvider
	t *testing.T
}

func (p *skippedProviderAccessProbe) CheckDownloadAvailability(context.Context) error {
	p.t.Error("skipped provider reached download availability")
	return nil
}

func (p *skippedProviderAccessProbe) CheckSearchAvailability(context.Context) error {
	p.t.Error("skipped provider reached search availability")
	return nil
}

func (p *skippedProviderAccessProbe) Search(context.Context, SearchQuery) ([]domain.Candidate, error) {
	p.t.Error("skipped provider reached remote search")
	return nil, nil
}

type skipAccessCache struct {
	*memoryCache
	t            *testing.T
	forbiddenKey string
}

func (c *skipAccessCache) GetProviderCache(ctx context.Context, key string, now time.Time) (store.ProviderCacheEntry, bool, error) {
	if key == c.forbiddenKey {
		c.t.Error("skipped provider reached cache read")
	}
	return c.memoryCache.GetProviderCache(ctx, key, now)
}

func (c *skipAccessCache) PutProviderCache(ctx context.Context, entry store.ProviderCacheEntry) error {
	if entry.Key == c.forbiddenKey {
		c.t.Error("skipped provider reached cache write")
	}
	return c.memoryCache.PutProviderCache(ctx, entry)
}

func TestCoordinatorSkippedProviderHasNoObservableAccess(t *testing.T) {
	var logs bytes.Buffer
	events, err := observability.New(&logs, observability.Options{Level: "debug", Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	skipped := &skippedProviderAccessProbe{
		fakeProvider: &fakeProvider{id: "skipped"},
		t:            t,
	}
	active := &fakeProvider{id: "active", candidates: map[SearchMode][]domain.Candidate{
		SearchBroad: {{ProviderID: "active", ResultID: "candidate"}},
	}}
	coordinator := newTestCoordinator(skipped, active)
	coordinator.Events = events
	query := SearchQuery{
		Media: testQueryMedia(), Language: "en", Mode: SearchBroad,
		SkipProviders: []string{"skipped"},
	}
	coordinator.Cache = &skipAccessCache{
		memoryCache:  &memoryCache{entries: map[string]store.ProviderCacheEntry{}},
		t:            t,
		forbiddenKey: providerCacheKey("skipped", query),
	}

	result := coordinator.Search(t.Context(), query)
	if got := result.ApplicableProviders; !slices.Equal(got, []string{"active"}) {
		t.Fatalf("applicable providers = %v", got)
	}
	if got := candidateIDs(result.Candidates); !slices.Equal(got, []string{"candidate"}) {
		t.Fatalf("candidate IDs = %v", got)
	}
	for _, record := range providerLogRecords(t, logs.String()) {
		if record["provider"] == "skipped" {
			t.Fatalf("skipped provider emitted lifecycle log: %#v", record)
		}
	}
}

func TestCoordinatorSkipsDownloadCooldownBeforeSearchAndCache(t *testing.T) {
	for _, mode := range []SearchMode{SearchExactHash, SearchBroad} {
		t.Run(string(mode), func(t *testing.T) {
			cooldown := &CooldownError{ProviderID: "blocked", Scope: OperationDownload, Reason: "download_quota", ResetAt: time.Now().Add(time.Hour)}
			blocked := &unavailableDownloadProvider{fakeProvider: &fakeProvider{id: "blocked", capabilities: Capabilities{ExactFileHash: true}}, unavailable: cooldown}
			var logs bytes.Buffer
			events, err := observability.New(&logs, observability.Options{Level: "info"})
			if err != nil {
				t.Fatal(err)
			}
			c := newTestCoordinator(Observe(blocked, events))
			c.Events = events
			c.Cache = noAccessCache{t}
			result := c.Search(t.Context(), SearchQuery{Media: testQueryMedia(), Language: "en", Mode: mode})
			if len(blocked.calls) != 0 || len(result.Candidates) != 0 || !errors.Is(result.Errors["blocked"], cooldown) {
				t.Fatalf("cooling-down provider was searched: calls=%v result=%+v", blocked.calls, result)
			}
			if logs.Len() != 0 {
				t.Fatalf("suppressed search logged at info: %s", logs.String())
			}
		})
	}
}

func TestCoordinatorDownloadCooldownKeepsOtherProvidersAvailable(t *testing.T) {
	blocked := &unavailableDownloadProvider{fakeProvider: &fakeProvider{id: "blocked"}, unavailable: &CooldownError{ProviderID: "blocked", Scope: OperationDownload, ResetAt: time.Now().Add(time.Hour)}}
	active := &fakeProvider{id: "active", candidates: map[SearchMode][]domain.Candidate{SearchBroad: {{ProviderID: "active", ResultID: "one"}}}}
	result := newTestCoordinator(blocked, active).Search(t.Context(), SearchQuery{Media: testQueryMedia(), Language: "en", Mode: SearchBroad})
	if len(blocked.calls) != 0 || len(active.calls) != 1 || len(result.Candidates) != 1 || result.Candidates[0].ProviderID != "active" || result.Errors["blocked"] == nil {
		t.Fatalf("calls=%v/%v result=%+v", blocked.calls, active.calls, result)
	}
}

func TestTransportDownloadAvailabilityReadsPersistedScopes(t *testing.T) {
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	for _, scope := range []Operation{OperationDownload, OperationDownloadTransfer, OperationAll, OperationAuth, OperationSearch} {
		t.Run(string(scope), func(t *testing.T) {
			states := &memoryStateStore{states: map[string]store.ProviderState{}}
			clock := testutil.NewClock(now)
			gate := NewGate(states, clock, 1)
			if err := gate.Persist(t.Context(), Throttle{ProviderID: "p", Scope: scope, Reason: "download_quota", ResetAt: now.Add(time.Hour)}); err != nil {
				t.Fatal(err)
			}
			client := Client{Gate: gate, ProviderID: "p"}
			available, ok := any(client).(DownloadAvailability)
			if !ok {
				t.Fatal("provider transport does not expose download availability")
			}
			err := available.CheckDownloadAvailability(t.Context())
			wantBlocked := scope != OperationSearch && scope != OperationAuth
			var cooldown *CooldownError
			if wantBlocked {
				if !errors.As(err, &cooldown) || !cooldown.ResetAt.Equal(now.Add(time.Hour)) {
					t.Fatalf("availability=%v", err)
				}
			} else if err != nil {
				t.Fatalf("unrelated operation blocks downloads: %v", err)
			}
			clock.Advance(time.Hour)
			if err := available.CheckDownloadAvailability(t.Context()); err != nil {
				t.Fatalf("expired state blocks download: %v", err)
			}
		})
	}
}

func TestDownloadAvailabilityWaitsForAllDownloadScopes(t *testing.T) {
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	states := &memoryStateStore{states: map[string]store.ProviderState{}}
	clock := testutil.NewClock(now)
	gate := NewGate(states, clock, 1)
	for i, scope := range []Operation{OperationAll, OperationDownload, OperationDownloadTransfer} {
		if err := gate.Persist(t.Context(), Throttle{ProviderID: "p", Scope: scope, ResetAt: now.Add(time.Duration(i+1) * time.Hour)}); err != nil {
			t.Fatal(err)
		}
	}
	client := Client{Gate: gate, ProviderID: "p"}
	var cooldown *CooldownError
	if err := client.CheckDownloadAvailability(t.Context()); !errors.As(err, &cooldown) || !cooldown.ResetAt.Equal(now.Add(3*time.Hour)) {
		t.Fatalf("availability=%v", err)
	}
	if err := states.PutProviderState(t.Context(), store.ProviderState{ProviderID: "p", Scope: "auth", Disabled: true}); err != nil {
		t.Fatal(err)
	}
	var disabled *DisabledError
	if err := client.CheckDownloadAvailability(t.Context()); !errors.As(err, &disabled) {
		t.Fatalf("disabled authentication availability=%v", err)
	}
}

func TestDownloadAvailabilityCancellationDoesNotAccessState(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// A nil gate would panic if canceled work reached persistent state.
	if err := (Client{}).CheckDownloadAvailability(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("availability=%v", err)
	}
}
