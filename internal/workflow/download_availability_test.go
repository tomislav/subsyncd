package workflow

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"io"
	"path/filepath"
	"testing"
	"time"

	"subsyncd/internal/domain"
	"subsyncd/internal/inventory"
	"subsyncd/internal/observability"
	"subsyncd/internal/pack"
	"subsyncd/internal/provider"
	"subsyncd/internal/store"
)

type availabilityProvider struct {
	*fakeProvider
	unavailable error
	results     map[provider.SearchMode][]domain.Candidate
	searches    []provider.SearchMode
	afterSearch error
}

func (p *availabilityProvider) Capabilities() provider.Capabilities {
	return provider.Capabilities{ExactFileHash: true}
}
func (p *availabilityProvider) CheckDownloadAvailability(context.Context) error { return p.unavailable }
func (p *availabilityProvider) Search(_ context.Context, q provider.SearchQuery) ([]domain.Candidate, error) {
	p.searches = append(p.searches, q.Mode)
	if len(p.results[q.Mode]) > 0 && p.afterSearch != nil {
		p.unavailable = p.afterSearch
	}
	return p.results[q.Mode], nil
}
func (p *availabilityProvider) Download(ctx context.Context, c domain.Candidate, w io.Writer) (provider.DownloadMetadata, error) {
	metadata, err := p.fakeProvider.Download(ctx, c, w)
	// Model an adapter persisting the quota/circuit before returning its failure.
	if err != nil {
		p.unavailable = err
	}
	return metadata, err
}
func useAvailabilityProviders(s *Service, items ...provider.Provider) {
	s.Searcher = &provider.Coordinator{Providers: items, Clock: s.Clock, Events: s.Events}
	s.ProviderOrder = nil
	for _, p := range items {
		s.Providers[p.ID()] = p
		s.ProviderOrder = append(s.ProviderOrder, p.ID())
	}
}

func TestDownloadCooldownStopsRemainingCandidates(t *testing.T) {
	for _, mode := range []provider.SearchMode{provider.SearchExactHash, provider.SearchBroad} {
		t.Run(string(mode), func(t *testing.T) {
			s := testService(t, inventory.Inventory{}, &fakeSearcher{}, nil, nil, nil)
			reset := s.Clock.Now().Add(time.Hour)
			quota := &provider.QuotaError{Scope: provider.OperationDownload, ResetAt: reset}
			candidates := []domain.Candidate{broadCandidate("one"), broadCandidate("two"), broadCandidate("three")}
			for i := range candidates {
				candidates[i].ExactHash = mode == provider.SearchExactHash
			}
			p := &availabilityProvider{fakeProvider: &fakeProvider{id: "provider", downloadErr: quota}, results: map[provider.SearchMode][]domain.Candidate{mode: candidates}}
			var logs bytes.Buffer
			events, err := observability.New(&logs, observability.Options{Level: "info"})
			if err != nil {
				t.Fatal(err)
			}
			s.Events = events
			useAvailabilityProviders(s, provider.Observe(p, events))
			result, err := s.Run(t.Context(), serviceRequest(t))
			if err != nil || result.Outcome != OutcomeThrottled || !result.RetryAt.Equal(reset) || p.downloads != 1 {
				t.Fatalf("result=%+v err=%v downloads=%d searches=%v", result, err, p.downloads, p.searches)
			}
			if got := bytes.Count(logs.Bytes(), []byte(`"event":"provider.download_completed"`)); got != 1 {
				t.Fatalf("download completions=%d logs=%s", got, logs.String())
			}
			if len(s.Repository.(*workflowRepository).candidates) != 3 {
				t.Fatal("lost scored candidates")
			}
			if mode == provider.SearchExactHash && len(p.searches) != 1 {
				t.Fatalf("quota still allowed broad search: %v", p.searches)
			}
		})
	}
}

func TestKnownDownloadCooldownUsesFallbackAndReset(t *testing.T) {
	s, installer := tierService(t, &fakeSearcher{}, &fakeSearcher{result: provider.SearchResult{Candidates: []domain.Candidate{fallbackCandidate("fallback", true)}}})
	reset := s.Clock.Now().Add(time.Hour)
	p := &availabilityProvider{fakeProvider: &fakeProvider{id: "provider"}, unavailable: &provider.CooldownError{ProviderID: "provider", Scope: provider.OperationDownload, ResetAt: reset}}
	useAvailabilityProviders(s, p)
	result, err := s.Run(t.Context(), serviceRequest(t))
	if err != nil || result.Outcome != OutcomeInstalled || !installer.request.Fallback || !result.NextUpgrade.Equal(reset) || len(p.searches) != 0 || p.downloads != 0 {
		t.Fatalf("result=%+v err=%v searches=%v downloads=%d", result, err, p.searches, p.downloads)
	}
}

func TestDownloadCooldownMidShortlistPreservesPartialOutageReset(t *testing.T) {
	s, _ := tierService(t, &fakeSearcher{}, &fakeSearcher{result: provider.SearchResult{Candidates: []domain.Candidate{fallbackCandidate("fallback", true)}}})
	reset := s.Clock.Now().Add(time.Hour)
	blocked := &availabilityProvider{fakeProvider: &fakeProvider{id: "provider", downloadErr: &provider.QuotaError{Scope: provider.OperationDownload, ResetAt: reset}}, results: map[provider.SearchMode][]domain.Candidate{provider.SearchBroad: {broadCandidate("one"), broadCandidate("two")}}}
	activeCandidate := broadCandidate("active")
	activeCandidate.ProviderID = "active"
	active := &availabilityProvider{fakeProvider: &fakeProvider{id: "active", downloadErr: errors.New("network failed")}, results: map[provider.SearchMode][]domain.Candidate{provider.SearchBroad: {activeCandidate}}}
	useAvailabilityProviders(s, blocked, active)
	result, err := s.Run(t.Context(), serviceRequest(t))
	if err != nil || result.Outcome != OutcomeInstalled || !result.NextUpgrade.Equal(reset) || blocked.downloads != 1 {
		t.Fatalf("result=%+v err=%v downloads=%d", result, err, blocked.downloads)
	}
}

func TestDownloadCooldownKeepsLocalPackUsable(t *testing.T) {
	request := serviceRequest(t)
	request.Media.Ref.Kind = domain.MediaEpisode
	request.Media.Season, request.Media.Episode = 1, 2
	candidate := broadCandidate("cached")
	candidate.Kind = domain.MediaEpisode
	path := writeInstallFile(t, filepath.Join(t.TempDir(), "cached.srt"), installSRT)
	cache := &fakePackCache{member: pack.CachedMember{Path: path, Candidate: candidate}, found: true}
	s := testService(t, inventory.Inventory{}, &fakeSearcher{}, cache, nil, nil)
	p := &availabilityProvider{fakeProvider: &fakeProvider{id: "provider"}, unavailable: &provider.CooldownError{ProviderID: "provider", Scope: provider.OperationDownload, ResetAt: s.Clock.Now().Add(time.Hour)}}
	useAvailabilityProviders(s, p)
	result, err := s.Run(t.Context(), request)
	if err != nil || result.Outcome != OutcomeInstalled || result.Candidate.ResultID != "cached" || len(p.searches) != 0 || p.downloads != 0 {
		t.Fatalf("result=%+v err=%v searches=%v downloads=%d", result, err, p.searches, p.downloads)
	}
}

func TestDownloadAvailabilityStoreFailureIsTerminal(t *testing.T) {
	failure := errors.New("state store failed")
	fallback := &fakeSearcher{result: provider.SearchResult{Candidates: []domain.Candidate{fallbackCandidate("fallback", true)}}}
	s, installer := tierService(t, &fakeSearcher{}, fallback)
	p := &availabilityProvider{fakeProvider: &fakeProvider{id: "provider"}, unavailable: failure}
	useAvailabilityProviders(s, p)
	_, err := s.Run(t.Context(), serviceRequest(t))
	if !errors.Is(err, failure) || fallback.calls != 0 || installer.calls != 0 {
		t.Fatalf("error=%v fallback=%d installs=%d", err, fallback.calls, installer.calls)
	}
}

func TestDownloadAvailabilityStateFailureAfterSearchIsTerminal(t *testing.T) {
	for _, mode := range []provider.SearchMode{provider.SearchExactHash, provider.SearchBroad} {
		t.Run(string(mode), func(t *testing.T) {
			failure := errors.New("state read failed after search")
			fallback := &fakeSearcher{result: provider.SearchResult{Candidates: []domain.Candidate{fallbackCandidate("fallback", true)}}}
			s, installer := tierService(t, &fakeSearcher{}, fallback)
			c := broadCandidate("candidate")
			c.ExactHash = mode == provider.SearchExactHash
			p := &availabilityProvider{fakeProvider: &fakeProvider{id: "provider"}, afterSearch: failure, results: map[provider.SearchMode][]domain.Candidate{mode: {c}}}
			useAvailabilityProviders(s, p)
			_, err := s.Run(t.Context(), serviceRequest(t))
			if !errors.Is(err, failure) || fallback.calls != 0 || installer.calls != 0 || p.downloads != 0 {
				t.Fatalf("error=%v fallback=%d installs=%d downloads=%d", err, fallback.calls, installer.calls, p.downloads)
			}
		})
	}
}

func TestDownloadCooldownKeepsOtherProviderCandidateUsable(t *testing.T) {
	s := testService(t, inventory.Inventory{}, &fakeSearcher{}, nil, nil, nil)
	blocked := &availabilityProvider{fakeProvider: &fakeProvider{id: "provider", downloadErr: &provider.QuotaError{Scope: provider.OperationDownload, ResetAt: s.Clock.Now().Add(time.Hour)}}, results: map[provider.SearchMode][]domain.Candidate{provider.SearchBroad: {broadCandidate("one"), broadCandidate("two")}}}
	c := broadCandidate("active")
	c.ProviderID = "active"
	active := &availabilityProvider{fakeProvider: &fakeProvider{id: "active"}, results: map[provider.SearchMode][]domain.Candidate{provider.SearchBroad: {c}}}
	useAvailabilityProviders(s, blocked, active)
	result, err := s.Run(t.Context(), serviceRequest(t))
	if err != nil || result.Outcome != OutcomeInstalled || result.Candidate.ProviderID != "active" || blocked.downloads != 1 || active.downloads != 1 {
		t.Fatalf("result=%+v err=%v downloads=%d/%d", result, err, blocked.downloads, active.downloads)
	}
}

type searchLimitedWorkflowProvider struct {
	*availabilityProvider
	searchUnavailable error
}

func (p *searchLimitedWorkflowProvider) CheckSearchAvailability(context.Context) error {
	return p.searchUnavailable
}

func TestSearchCooldownUsesFallbackAndPreservesReset(t *testing.T) {
	s, installer := tierService(t, &fakeSearcher{}, &fakeSearcher{result: provider.SearchResult{Candidates: []domain.Candidate{fallbackCandidate("fallback", true)}}})
	reset := s.Clock.Now().Add(time.Hour)
	p := &searchLimitedWorkflowProvider{availabilityProvider: &availabilityProvider{fakeProvider: &fakeProvider{id: "provider"}}, searchUnavailable: &provider.CooldownError{ProviderID: "provider", Scope: provider.OperationSearch, ResetAt: reset}}
	useAvailabilityProviders(s, p)
	result, err := s.Run(t.Context(), serviceRequest(t))
	if err != nil || result.Outcome != OutcomeInstalled || !installer.request.Fallback || !result.NextUpgrade.Equal(reset) || len(p.searches) != 0 || p.downloads != 0 {
		t.Fatalf("result=%+v error=%v searches=%v downloads=%d", result, err, p.searches, p.downloads)
	}
}

type unavailableStateStore struct{ err error }

func (s unavailableStateStore) GetProviderState(context.Context, string, string) (store.ProviderState, error) {
	return store.ProviderState{}, sql.ErrNoRows
}
func (s unavailableStateStore) PutProviderState(context.Context, store.ProviderState) error {
	return s.err
}

func TestTransportStateWriteFailureCannotBeHiddenByFallback(t *testing.T) {
	sentinel := errors.New("write provider state failed")
	gate := provider.NewGate(unavailableStateStore{sentinel}, provider.SystemClock{}, 1)
	_, stateErr := gate.RecordTransientFailure(t.Context(), "provider", provider.OperationSearch, "network_error", time.Time{})
	for _, phase := range []string{"search", "exact_download", "broad_download"} {
		t.Run(phase, func(t *testing.T) {
			preferred := &fakeSearcher{}
			fallback := &fakeSearcher{result: provider.SearchResult{Candidates: []domain.Candidate{fallbackCandidate("fallback", true)}}}
			s, installer := tierService(t, preferred, fallback)
			if phase == "search" {
				preferred.result.Errors = map[string]error{"provider": stateErr}
			} else {
				candidate := broadCandidate("candidate")
				candidate.ExactHash = phase == "exact_download"
				preferred.result.Candidates = []domain.Candidate{candidate}
				s.Providers["provider"].(*fakeProvider).downloadErr = stateErr
			}
			_, err := s.Run(t.Context(), serviceRequest(t))
			if !errors.Is(err, sentinel) || fallback.calls != 0 || installer.calls != 0 {
				t.Fatalf("error=%v fallback=%d installs=%d", err, fallback.calls, installer.calls)
			}
		})
	}
}

func TestSearchAvailabilityReadFailureIsTerminal(t *testing.T) {
	failure := errors.New("search state read failed")
	fallback := &fakeSearcher{result: provider.SearchResult{Candidates: []domain.Candidate{fallbackCandidate("fallback", true)}}}
	s, installer := tierService(t, &fakeSearcher{}, fallback)
	p := &searchLimitedWorkflowProvider{availabilityProvider: &availabilityProvider{fakeProvider: &fakeProvider{id: "provider"}}, searchUnavailable: failure}
	useAvailabilityProviders(s, p)
	_, err := s.Run(t.Context(), serviceRequest(t))
	if !errors.Is(err, failure) || fallback.calls != 0 || installer.calls != 0 {
		t.Fatalf("error=%v fallback=%d installs=%d", err, fallback.calls, installer.calls)
	}
}
