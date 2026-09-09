package workflow

import (
	"context"
	"subsyncd/internal/domain"
	"subsyncd/internal/observability"
	"subsyncd/internal/pack"
	"subsyncd/internal/provider"
	"testing"
)

type compatibilityProvider struct{ provider.Provider }

func (compatibilityProvider) CanReuseCachedCandidate(c domain.Candidate) bool {
	return c.ResultID == "current"
}

type compatibilityCache struct {
	fakePackCache
	members []pack.CachedMember
}

func (c *compatibilityCache) FindEligible(_ context.Context, _ domain.Media, _ domain.Language, accept func(pack.CachedMember) (bool, error)) (pack.CachedMember, bool, error) {
	for _, m := range c.members {
		ok, err := accept(m)
		if err != nil {
			return pack.CachedMember{}, false, err
		}
		if ok {
			return m, true, nil
		}
	}
	return pack.CachedMember{}, false, nil
}

func TestPackLookupSkipsIncompatibleProviderEvidence(t *testing.T) {
	legacy := pack.CachedMember{Candidate: domain.Candidate{ProviderID: "provider", ResultID: "legacy"}}
	current := pack.CachedMember{Candidate: domain.Candidate{ProviderID: "provider", ResultID: "current"}}
	s := &Service{ProviderOrder: []string{"provider"}, Providers: map[string]provider.Provider{"provider": provider.Observe(compatibilityProvider{}, observability.Discard())}, Repository: &workflowRepository{}, Clock: provider.SystemClock{}}
	request := Request{Language: "en"}
	s.PackCache = &fakePackCache{found: true, member: legacy}
	if _, found, _, _ := s.findUnrejectedPack(context.Background(), request, &Result{}); found {
		t.Fatal("legacy cache accepted incompatible evidence")
	}
	s.PackCache = &compatibilityCache{members: []pack.CachedMember{legacy, current}}
	got, found, cacheErr, lookupErr := s.findUnrejectedPack(context.Background(), request, &Result{})
	if !found || got.Candidate.ResultID != "current" || cacheErr != nil || lookupErr != nil {
		t.Fatalf("legacy member hid compatible member: %+v %v %v %v", got, found, cacheErr, lookupErr)
	}
}
