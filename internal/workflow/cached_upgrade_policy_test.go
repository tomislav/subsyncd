package workflow

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"subsyncd/internal/domain"
	"subsyncd/internal/pack"
	"subsyncd/internal/provider"
)

// This fails if an upgrade-policy denial is sent to prepareCandidate: that path
// treats the denial as a technical candidate failure and advances failure backoff.
func TestCachedUpgradeBelowDeltaKeepsNormalNoResultOutcome(t *testing.T) {
	request, cached, repository := cachedUpgradeFixture(t, []byte(`{"total":100}`))
	cachedPath := writeInstallFile(t, filepath.Join(t.TempDir(), "cached.srt"), installSRT)
	cache := &fakePackCache{member: pack.CachedMember{Path: cachedPath, Candidate: cached}, found: true}
	searcher := &fakeSearcher{}
	synchronizer := &fakeSynchronizer{}
	installer := &fakeInstaller{}
	service := testService(t, managedSidecarInventory(repository.installation), searcher, cache, synchronizer, installer)
	service.Repository = repository

	result, err := service.Run(context.Background(), request)
	if err != nil || result.Outcome != OutcomeNoResult || !result.NextUpgrade.IsZero() {
		t.Fatalf("Run() = %+v, %v", result, err)
	}
	if synchronizer.synchronizeCalls != 0 || installer.calls != 0 || len(repository.rejections) != 0 {
		t.Fatalf("sync/install/rejections = %d/%d/%#v", synchronizer.synchronizeCalls, installer.calls, repository.rejections)
	}
	if len(searcher.queries) != 2 {
		t.Fatalf("search queries = %#v", searcher.queries)
	}
	if got := []provider.SearchMode{searcher.queries[0].Mode, searcher.queries[1].Mode}; got[0] != provider.SearchExactHash || got[1] != provider.SearchBroad {
		t.Fatalf("search modes = %#v", searcher.queries)
	}
}

func TestCachedUpgradeBelowDeltaContinuesToEligibleProviderCandidate(t *testing.T) {
	request, cached, repository := cachedUpgradeFixture(t, []byte(`{"total":55}`))
	request.Media.ReleaseGroup = "GROUP"
	request.Media.Source = "web-dl"
	request.Media.Resolution = "1080p"
	cachedPath := writeInstallFile(t, filepath.Join(t.TempDir(), "cached.srt"), installSRT)
	cache := &fakePackCache{member: pack.CachedMember{Path: cachedPath, Candidate: cached}, found: true}
	providerCandidate := cached
	providerCandidate.ResultID = "provider-better"
	providerCandidate.Pack = nil
	providerCandidate.ReleaseNames = []string{"Show.S01E02.1080p.WEB-DL-GROUP"}
	searcher := &fakeSearcher{results: map[provider.SearchMode]provider.SearchResult{
		provider.SearchBroad: {Candidates: []domain.Candidate{providerCandidate}},
	}}
	synchronizer := &fakeSynchronizer{}
	installer := &fakeInstaller{}
	adapter := &fakeProvider{id: "provider"}
	service := testService(t, managedSidecarInventory(repository.installation), searcher, cache, synchronizer, installer)
	service.Repository = repository
	service.Providers = map[string]provider.Provider{"provider": adapter}

	result, err := service.Run(context.Background(), request)
	if err != nil || result.Outcome != OutcomeInstalled || result.Candidate.ResultID != "provider-better" {
		t.Fatalf("Run() = %+v, %v", result, err)
	}
	if synchronizer.synchronizeCalls != 1 || synchronizer.synchronized[0] != "provider-better" || installer.calls != 1 || installer.request.Candidate.ResultID != "provider-better" {
		t.Fatalf("sync/install = %#v/%+v", synchronizer.synchronized, installer.request)
	}
	if len(repository.rejections) != 0 {
		t.Fatalf("unexpected rejections = %#v", repository.rejections)
	}
}

func TestCachedUpgradeMalformedInstalledScoreIsTerminal(t *testing.T) {
	request, cached, repository := cachedUpgradeFixture(t, []byte(`not-json`))
	cachedPath := writeInstallFile(t, filepath.Join(t.TempDir(), "cached.srt"), installSRT)
	cache := &fakePackCache{member: pack.CachedMember{Path: cachedPath, Candidate: cached}, found: true}
	searcher := &fakeSearcher{}
	synchronizer := &fakeSynchronizer{}
	installer := &fakeInstaller{}
	service := testService(t, managedSidecarInventory(repository.installation), searcher, cache, synchronizer, installer)
	service.Repository = repository

	_, err := service.Run(context.Background(), request)
	var exhausted *acquisitionExhaustedError
	if err == nil || !strings.Contains(err.Error(), "decode installed subtitle score") || errors.As(err, &exhausted) {
		t.Fatalf("Run() error = %v", err)
	}
	if len(searcher.queries) != 0 || synchronizer.synchronizeCalls != 0 || installer.calls != 0 || len(repository.rejections) != 0 {
		t.Fatalf("searches/sync/install/rejections = %#v/%d/%d/%#v", searcher.queries, synchronizer.synchronizeCalls, installer.calls, repository.rejections)
	}
}

func cachedUpgradeFixture(t *testing.T, scoreJSON []byte) (Request, domain.Candidate, *workflowRepository) {
	t.Helper()
	request := serviceRequest(t)
	request.Media.Ref.Kind = domain.MediaEpisode
	request.Media.Title = "Show"
	request.Media.Season = 1
	request.Media.Episode = 2
	cached := broadCandidate("cached-low")
	cached.Kind = domain.MediaEpisode
	cached.Title = "Show"
	cached.Season = 1
	cached.Episode = 2
	cached.Pack = &domain.PackInfo{Scope: domain.PackSeason, Season: 1}
	existing := matchingInstallation(request, broadCandidate("installed"), scoreJSON)
	existing.Checksum = "managed-checksum"
	return request, cached, &workflowRepository{installation: existing, found: true}
}
