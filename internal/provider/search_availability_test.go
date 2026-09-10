package provider

import (
	"bytes"
	"context"
	"errors"
	"subsyncd/internal/domain"
	"subsyncd/internal/observability"
	"testing"
	"time"
)

type searchAvailabilityProvider struct {
	*fakeProvider
	unavailable error
}

func (p *searchAvailabilityProvider) CheckSearchAvailability(context.Context) error {
	return p.unavailable
}

func TestCoordinatorSkipsKnownSearchCooldown(t *testing.T) {
	for _, mode := range []SearchMode{SearchExactHash, SearchBroad} {
		t.Run(string(mode), func(t *testing.T) {
			cooldown := &CooldownError{ProviderID: "p", Scope: OperationSearch, Reason: "transient_network_error", ResetAt: time.Now().Add(time.Minute)}
			p := &searchAvailabilityProvider{fakeProvider: &fakeProvider{id: "p", capabilities: Capabilities{ExactFileHash: true}}, unavailable: cooldown}
			var logs bytes.Buffer
			events, err := observability.New(&logs, observability.Options{Level: "info"})
			if err != nil {
				t.Fatal(err)
			}
			c := newTestCoordinator(Observe(p, events))
			c.Events = events
			result := c.Search(t.Context(), SearchQuery{Media: testQueryMedia(), Language: "en", Mode: mode})
			if len(p.calls) != 0 || !errors.Is(result.Errors["p"], cooldown) || logs.Len() != 0 {
				t.Fatalf("calls=%v result=%+v logs=%s", p.calls, result, logs.String())
			}
		})
	}
}

func TestCoordinatorSearchCooldownPreservesUsableCache(t *testing.T) {
	p := &searchAvailabilityProvider{fakeProvider: &fakeProvider{id: "p", candidates: map[SearchMode][]domain.Candidate{SearchBroad: {{ProviderID: "p", ResultID: "one", DownloadRef: "opaque"}}}}}
	c := newTestCoordinator(p)
	query := SearchQuery{Media: testQueryMedia(), Language: "en", Mode: SearchBroad}
	c.Search(t.Context(), query)
	p.unavailable = &CooldownError{ProviderID: "p", Scope: OperationSearch, ResetAt: time.Now().Add(time.Hour)}
	result := c.Search(t.Context(), query)
	if len(p.calls) != 1 || len(result.Candidates) != 1 || len(result.Errors) != 0 {
		t.Fatalf("calls=%v result=%+v", p.calls, result)
	}
}

func TestSearchCooldownSkipsUnusableCacheWithoutRefreshing(t *testing.T) {
	for _, state := range []string{"requires_refresh", "expired", "malformed"} {
		t.Run(state, func(t *testing.T) {
			candidate := domain.Candidate{ProviderID: "p", ResultID: "one", DownloadRef: "opaque"}
			if state == "requires_refresh" {
				candidate.DownloadRef = "https://example.test/file?token=fixture"
			}
			p := &searchAvailabilityProvider{fakeProvider: &fakeProvider{id: "p", candidates: map[SearchMode][]domain.Candidate{SearchBroad: {candidate}}}}
			c := newTestCoordinator(p)
			query := SearchQuery{Media: testQueryMedia(), Language: "en", Mode: SearchBroad}
			if result := c.Search(t.Context(), query); len(result.Candidates) != 1 {
				t.Fatalf("seed=%+v", result)
			}
			cache := c.Cache.(*memoryCache)
			for key, entry := range cache.entries {
				if state == "expired" {
					entry.ExpiresAt = c.Clock.Now().Add(-time.Second)
				}
				if state == "malformed" {
					entry.ResultsJSON = []byte("invalid")
				}
				cache.entries[key] = entry
			}
			p.unavailable = &CooldownError{ProviderID: "p", Scope: OperationSearch, ResetAt: c.Clock.Now().Add(time.Hour)}
			result := c.Search(t.Context(), query)
			if len(p.calls) != 1 || len(result.Candidates) != 0 || !errors.Is(result.Errors["p"], p.unavailable) {
				t.Fatalf("calls=%v result=%+v", p.calls, result)
			}
		})
	}
}
