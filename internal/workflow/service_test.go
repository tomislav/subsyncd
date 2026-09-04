package workflow

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"subsyncd/internal/domain"
	"subsyncd/internal/inventory"
	"subsyncd/internal/pack"
	"subsyncd/internal/provider"
	"subsyncd/internal/store"
	"subsyncd/internal/syncer"
)

func TestServiceStopsForEmbeddedOrProtectedSubtitle(t *testing.T) {
	for _, track := range []inventory.Track{
		{Language: "en", Embedded: true},
		{Language: "en", Path: "/media/Movie.en.srt", Protected: true},
	} {
		searcher := &fakeSearcher{}
		service := testService(t, inventory.Inventory{Tracks: []inventory.Track{track}}, searcher, nil, nil, nil)
		result, err := service.Run(context.Background(), serviceRequest(t))
		if err != nil || result.Outcome != OutcomeSatisfied || searcher.calls != 0 {
			t.Fatalf("Run() = %#v, %v, search calls=%d", result, err, searcher.calls)
		}
	}
}

func TestServiceUsesRefreshedFilesystemFingerprintForProviderSearch(t *testing.T) {
	request := serviceRequest(t)
	refreshed := request.Media.Fingerprint
	refreshed.Size++
	refreshed.ModTime = refreshed.ModTime.Add(time.Second)
	searcher := &fakeSearcher{result: provider.SearchResult{Candidates: []domain.Candidate{exactCandidate("exact")}}}
	service := testService(t, inventory.Inventory{Fingerprint: refreshed}, searcher, nil, &fakeSynchronizer{}, &fakeInstaller{})
	service.Providers = map[string]provider.Provider{"provider": &fakeProvider{id: "provider"}}
	if _, err := service.Run(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if searcher.query.Media.Fingerprint != refreshed {
		t.Fatalf("provider fingerprint = %#v, want refreshed %#v", searcher.query.Media.Fingerprint, refreshed)
	}
}

func TestServiceHonorsHearingImpairedInventoryPolicy(t *testing.T) {
	current := inventory.Inventory{Tracks: []inventory.Track{{Language: "en", Embedded: true, SDH: true}}}

	t.Run("disallowed SDH does not satisfy request", func(t *testing.T) {
		searcher := &fakeSearcher{result: provider.SearchResult{Candidates: []domain.Candidate{exactCandidate("replacement")}}}
		providerFake := &fakeProvider{id: "provider"}
		service := testService(t, current, searcher, nil, &fakeSynchronizer{}, &fakeInstaller{})
		service.Providers = map[string]provider.Provider{"provider": providerFake}
		result, err := service.Run(context.Background(), serviceRequest(t))
		if err != nil || result.Outcome != OutcomeInstalled || searcher.calls != 1 {
			t.Fatalf("Run() = %#v, %v, search calls=%d", result, err, searcher.calls)
		}
	})

	t.Run("allowed SDH satisfies request", func(t *testing.T) {
		searcher := &fakeSearcher{}
		service := testService(t, current, searcher, nil, nil, nil)
		service.AllowHearingImpaired = true
		result, err := service.Run(context.Background(), serviceRequest(t))
		if err != nil || result.Outcome != OutcomeSatisfied || searcher.calls != 0 {
			t.Fatalf("Run() = %#v, %v, search calls=%d", result, err, searcher.calls)
		}
	})
}

func TestServiceRejectsHearingImpairedCandidateWhenDisallowed(t *testing.T) {
	candidate := exactCandidate("sdh")
	candidate.HearingImpaired = true
	providerFake := &fakeProvider{id: "provider"}
	repository := &workflowRepository{}
	service := testService(t, inventory.Inventory{}, &fakeSearcher{result: provider.SearchResult{Candidates: []domain.Candidate{candidate}}}, nil, &fakeSynchronizer{}, &fakeInstaller{})
	service.Providers = map[string]provider.Provider{"provider": providerFake}
	service.Repository = repository
	result, err := service.Run(context.Background(), serviceRequest(t))
	if err != nil || result.Outcome != OutcomeRejected || providerFake.downloads != 0 || len(repository.candidates) != 1 {
		t.Fatalf("Run() = %#v, %v, downloads=%d candidates=%d", result, err, providerFake.downloads, len(repository.candidates))
	}
	if !strings.Contains(string(repository.candidates[0].ScoreJSON), "hearing-impaired") {
		t.Fatalf("persisted score does not explain rejection: %s", repository.candidates[0].ScoreJSON)
	}
}

func TestServiceUsesCachedPackBeforeProvidersAndFallsThroughAfterLapseRejection(t *testing.T) {
	request := serviceRequest(t)
	request.Media.Ref.Kind = domain.MediaEpisode
	request.Media.Season = 1
	request.Media.Episode = 2
	cachedPath := writeInstallFile(t, filepath.Join(t.TempDir(), "cached.srt"), strings.Replace(installSRT, "Hello", "cached", 1))
	cachedCandidate := broadCandidate("cached")
	cachedCandidate.Kind = domain.MediaEpisode
	cache := &fakePackCache{member: pack.CachedMember{Path: cachedPath, Candidate: cachedCandidate}, found: true}
	searcher := &fakeSearcher{}
	sync := &fakeSynchronizer{confidence: map[string]float64{"cached": 0.8}}
	installer := &fakeInstaller{}
	service := testService(t, inventory.Inventory{}, searcher, cache, sync, installer)
	result, err := service.Run(context.Background(), request)
	if err != nil || result.Outcome != OutcomeInstalled || result.Candidate.ResultID != "cached" || searcher.calls != 0 {
		t.Fatalf("cache Run() = %#v, %v, search calls=%d", result, err, searcher.calls)
	}

	providerFake := &fakeProvider{id: "provider"}
	remote := exactCandidate("remote")
	remote.Kind = domain.MediaEpisode
	searcher = &fakeSearcher{result: provider.SearchResult{Candidates: []domain.Candidate{remote}}}
	sync = &fakeSynchronizer{rejectText: "cached"}
	installer = &fakeInstaller{}
	service = testService(t, inventory.Inventory{}, searcher, cache, sync, installer)
	service.Providers = map[string]provider.Provider{"provider": providerFake}
	result, err = service.Run(context.Background(), request)
	if err != nil || result.Outcome != OutcomeInstalled || result.Candidate.ResultID != "remote" || searcher.calls != 1 || providerFake.downloads != 1 {
		t.Fatalf("fallback Run() = %#v, %v, search=%d downloads=%d", result, err, searcher.calls, providerFake.downloads)
	}
}

func TestServiceSkipsARejectedCachedPackMember(t *testing.T) {
	request := serviceRequest(t)
	request.Media.Ref.Kind = domain.MediaEpisode
	request.Media.Season = 1
	request.Media.Episode = 2
	cachedPath := writeInstallFile(t, filepath.Join(t.TempDir(), "cached.srt"), installSRT)
	candidate := broadCandidate("pack")
	candidate.Kind = domain.MediaEpisode
	candidate.Season = 1
	candidate.Episode = 2
	candidate.Pack = &domain.PackInfo{Scope: domain.PackSeason, Season: 1}
	cache := &fakePackCache{member: pack.CachedMember{Path: cachedPath, Checksum: "member-checksum", Candidate: candidate}, found: true}
	repository := &workflowRepository{}
	synchronizer := &fakeSynchronizer{}
	service := testService(t, inventory.Inventory{}, &fakeSearcher{}, cache, synchronizer, &fakeInstaller{})
	service.Repository = repository
	signature, err := candidateSignature(candidate)
	if err != nil {
		t.Fatal(err)
	}
	now := service.Clock.Now()
	fingerprint := request.Media.Fingerprint
	repository.rejections = append(repository.rejections, store.CandidateRejection{MediaID: request.MediaID, Language: "en", ProviderID: "provider", ResultID: "pack", CandidateSignature: signature, ArtifactChecksum: "member-checksum", ReasonCode: "lapse_unsure", ToolSignature: service.rejectionToolSignature(), MediaPath: fingerprint.Path, MediaFileID: fingerprint.FileID, MediaSize: fingerprint.Size, MediaModTimeNS: fingerprint.ModTime.UnixNano(), RejectedAt: now, ExpiresAt: now.Add(time.Hour)})

	result, err := service.Run(context.Background(), request)
	if err != nil || result.Outcome != OutcomeNoResult || synchronizer.analyzeCalls != 0 || len(result.Decisions) != 1 || result.Decisions[0].Stage != "candidate_rejection" {
		t.Fatalf("Run() = %#v/%v analyze=%d", result, err, synchronizer.analyzeCalls)
	}
}

func TestServiceExactHashSkipsLapseAndBroadCandidatesAreLimitedToThree(t *testing.T) {
	t.Run("exact", func(t *testing.T) {
		searcher := &fakeSearcher{result: provider.SearchResult{Candidates: []domain.Candidate{exactCandidate("exact")}}}
		providerFake := &fakeProvider{id: "provider"}
		sync := &fakeSynchronizer{}
		installer := &fakeInstaller{}
		service := testService(t, inventory.Inventory{}, searcher, nil, sync, installer)
		service.Providers = map[string]provider.Provider{"provider": providerFake}
		result, err := service.Run(context.Background(), serviceRequest(t))
		if err != nil || result.Candidate.ResultID != "exact" || sync.analyzeCalls != 0 || sync.synchronizeCalls != 0 {
			t.Fatalf("Run() = %#v, %v, analyze/sync=%d/%d", result, err, sync.analyzeCalls, sync.synchronizeCalls)
		}
	})

	t.Run("top three and confidence tie break", func(t *testing.T) {
		var candidates []domain.Candidate
		for _, id := range []string{"one", "two", "three", "four", "five"} {
			candidates = append(candidates, broadCandidate(id))
		}
		searcher := &fakeSearcher{result: provider.SearchResult{Candidates: candidates}}
		providerFake := &fakeProvider{id: "provider"}
		sync := &fakeSynchronizer{confidence: map[string]float64{"five": 0.5, "four": 0.9, "one": 0.7}}
		installer := &fakeInstaller{}
		service := testService(t, inventory.Inventory{}, searcher, nil, sync, installer)
		service.Providers = map[string]provider.Provider{"provider": providerFake}
		result, err := service.Run(context.Background(), serviceRequest(t))
		if err != nil || result.Candidate.ResultID != "four" || sync.analyzeCalls != 3 || sync.synchronizeCalls != 1 || !slices.Equal(providerFake.downloaded, []string{"five", "four", "one"}) {
			t.Fatalf("Run() = %#v, %v, analyzed/synced=%d/%d downloads=%#v", result, err, sync.analyzeCalls, sync.synchronizeCalls, providerFake.downloaded)
		}
	})
}

func TestRunStopsAfterUniqueHighestScoreInstalls(t *testing.T) {
	request := serviceRequest(t)
	request.Media.ReleaseGroup = "GROUP"
	request.Media.Source = "webdl"
	leader := broadCandidate("leader")
	leader.ReleaseNames = []string{"Movie.2024-GROUP"}
	runnerUp := broadCandidate("runner-up")
	runnerUp.ReleaseNames = []string{"Movie.2024.WEB-DL"}
	last := broadCandidate("last")
	providerFake := &fakeProvider{id: "provider"}
	synchronizer := &fakeSynchronizer{}
	searcher := &fakeSearcher{result: provider.SearchResult{Candidates: []domain.Candidate{last, runnerUp, leader}}}
	service := testService(t, inventory.Inventory{}, searcher, nil, synchronizer, &fakeInstaller{})
	service.Providers = map[string]provider.Provider{"provider": providerFake}

	result, err := service.Run(context.Background(), request)
	if err != nil || result.Candidate.ResultID != "leader" {
		t.Fatalf("Run() = %#v, %v", result, err)
	}
	if !slices.Equal(providerFake.downloaded, []string{"leader"}) ||
		!slices.Equal(synchronizer.analyzed, []string{"leader"}) ||
		!slices.Equal(synchronizer.synchronized, []string{"leader"}) {
		t.Fatalf("download/analyze/sync = %#v/%#v/%#v", providerFake.downloaded, synchronizer.analyzed, synchronizer.synchronized)
	}
}

func TestRunAnalyzesEqualScoreTierBeforeFinalizing(t *testing.T) {
	low := broadCandidate("low-confidence")
	low.ProviderID = "first"
	high := broadCandidate("high-confidence")
	high.ProviderID = "second"
	firstProvider := &fakeProvider{id: "first"}
	secondProvider := &fakeProvider{id: "second"}
	synchronizer := &fakeSynchronizer{confidence: map[string]float64{"low-confidence": 0.76, "high-confidence": 0.91}}
	service := testService(t, inventory.Inventory{}, &fakeSearcher{result: provider.SearchResult{Candidates: []domain.Candidate{low, high}}}, nil, synchronizer, &fakeInstaller{})
	service.ProviderOrder = []string{"first", "second"}
	service.Providers = map[string]provider.Provider{"first": firstProvider, "second": secondProvider}

	result, err := service.Run(context.Background(), serviceRequest(t))
	if err != nil || result.Candidate.ResultID != "high-confidence" {
		t.Fatalf("Run() = %#v, %v", result, err)
	}
	if !slices.Equal(firstProvider.downloaded, []string{"low-confidence"}) || !slices.Equal(secondProvider.downloaded, []string{"high-confidence"}) {
		t.Fatalf("downloads = %#v/%#v", firstProvider.downloaded, secondProvider.downloaded)
	}
	if !slices.Equal(synchronizer.analyzed, []string{"low-confidence", "high-confidence"}) || !slices.Equal(synchronizer.synchronized, []string{"high-confidence"}) {
		t.Fatalf("analyzed/synchronized = %#v/%#v", synchronizer.analyzed, synchronizer.synchronized)
	}
}

func TestAnalyzedTierOrderingIsDeterministic(t *testing.T) {
	items := []analyzedCandidate{
		analyzedTestCandidate("provider-b", "result-b", 1, 9, 0.8),
		analyzedTestCandidate("provider-a", "result-b", 1, 9, 0.8),
		analyzedTestCandidate("provider-a", "result-a", 1, 9, 0.8),
		analyzedTestCandidate("provider-a", "low-rating", 1, 1, 0.8),
		analyzedTestCandidate("provider-z", "priority-wins", 0, 1, 0.8),
		analyzedTestCandidate("provider-z", "confidence-wins", 2, 1, 0.9),
	}
	sortAnalyzed(items)
	got := make([]string, len(items))
	for index, item := range items {
		got[index] = item.candidate.ResultID
	}
	want := []string{"confidence-wins", "priority-wins", "result-a", "result-b", "result-b", "low-rating"}
	if !slices.Equal(got, want) {
		t.Fatalf("analyzed order = %v, want %v", got, want)
	}
}

func TestTournamentAnalysisFallbackToNextScoreTier(t *testing.T) {
	request := serviceRequest(t)
	request.Media.ReleaseGroup = "GROUP"
	leader := broadCandidate("leader")
	leader.ReleaseNames = []string{"Movie.2024-GROUP"}
	lower := broadCandidate("lower")
	providerFake := &fakeProvider{id: "provider"}
	synchronizer := &fakeSynchronizer{verdicts: map[string]string{"leader": "unsure"}}
	repository := &workflowRepository{}
	service := testService(t, inventory.Inventory{}, &fakeSearcher{result: provider.SearchResult{Candidates: []domain.Candidate{lower, leader}}}, nil, synchronizer, &fakeInstaller{})
	service.Providers = map[string]provider.Provider{"provider": providerFake}
	service.Repository = repository

	result, err := service.Run(context.Background(), request)
	if err != nil || result.Candidate.ResultID != "lower" {
		t.Fatalf("Run() = %#v, %v", result, err)
	}
	if !slices.Equal(providerFake.downloaded, []string{"leader", "lower"}) || !slices.Equal(synchronizer.analyzed, []string{"leader", "lower"}) || !slices.Equal(synchronizer.synchronized, []string{"lower"}) || len(repository.rejections) != 1 {
		t.Fatalf("downloads/analyzed/synchronized/rejections = %#v/%#v/%#v/%d", providerFake.downloaded, synchronizer.analyzed, synchronizer.synchronized, len(repository.rejections))
	}
	for _, expected := range []string{"tournament_tier", "lapse_analysis", "fallback", "early_stop"} {
		if !hasDecisionStage(result.Decisions, expected) {
			t.Fatalf("decisions %#v missing stage %q", result.Decisions, expected)
		}
	}
}

func TestTournamentSynchronizationFallbackWithinTie(t *testing.T) {
	first := broadCandidate("first")
	second := broadCandidate("second")
	providerFake := &fakeProvider{id: "provider"}
	synchronizer := &fakeSynchronizer{confidence: map[string]float64{"first": 0.9, "second": 0.8}, synchronizeErrors: map[string]error{"first": errors.New("lapse process failed")}}
	repository := &workflowRepository{}
	service := testService(t, inventory.Inventory{}, &fakeSearcher{result: provider.SearchResult{Candidates: []domain.Candidate{first, second}}}, nil, synchronizer, &fakeInstaller{})
	service.Providers = map[string]provider.Provider{"provider": providerFake}
	service.Repository = repository

	result, err := service.Run(context.Background(), serviceRequest(t))
	if err != nil || result.Candidate.ResultID != "second" {
		t.Fatalf("Run() = %#v, %v", result, err)
	}
	if !slices.Equal(synchronizer.analyzed, []string{"first", "second"}) || !slices.Equal(synchronizer.synchronized, []string{"first", "second"}) || len(repository.rejections) != 0 {
		t.Fatalf("analyzed/synchronized/rejections = %#v/%#v/%d", synchronizer.analyzed, synchronizer.synchronized, len(repository.rejections))
	}
}

func TestTournamentSynchronizationFallbackToLowerTier(t *testing.T) {
	request := serviceRequest(t)
	request.Media.ReleaseGroup = "GROUP"
	first := broadCandidate("first")
	first.ReleaseNames = []string{"Movie.2024-GROUP"}
	second := broadCandidate("second")
	second.ReleaseNames = []string{"Movie.2024-GROUP"}
	lower := broadCandidate("lower")
	providerFake := &fakeProvider{id: "provider"}
	synchronizer := &fakeSynchronizer{synchronizeErrors: map[string]error{"first": errors.New("first failed"), "second": errors.New("second failed")}}
	repository := &workflowRepository{}
	service := testService(t, inventory.Inventory{}, &fakeSearcher{result: provider.SearchResult{Candidates: []domain.Candidate{first, second, lower}}}, nil, synchronizer, &fakeInstaller{})
	service.Providers = map[string]provider.Provider{"provider": providerFake}
	service.Repository = repository

	result, err := service.Run(context.Background(), request)
	if err != nil || result.Candidate.ResultID != "lower" || len(repository.rejections) != 0 {
		t.Fatalf("Run() = %#v, %v, rejections=%#v", result, err, repository.rejections)
	}
}

func TestTournamentTransientAnalysisFallbackDoesNotBlacklist(t *testing.T) {
	request := serviceRequest(t)
	request.Media.ReleaseGroup = "GROUP"
	leader := broadCandidate("leader")
	leader.ReleaseNames = []string{"Movie.2024-GROUP"}
	lower := broadCandidate("lower")
	providerFake := &fakeProvider{id: "provider"}
	synchronizer := &fakeSynchronizer{analyzeErrors: map[string]error{"leader": errors.New("temporary lapse failure")}}
	repository := &workflowRepository{}
	service := testService(t, inventory.Inventory{}, &fakeSearcher{result: provider.SearchResult{Candidates: []domain.Candidate{leader, lower}}}, nil, synchronizer, &fakeInstaller{})
	service.Providers = map[string]provider.Provider{"provider": providerFake}
	service.Repository = repository

	result, err := service.Run(context.Background(), request)
	if err != nil || result.Candidate.ResultID != "lower" || len(repository.rejections) != 0 {
		t.Fatalf("Run() = %#v, %v, rejections=%#v", result, err, repository.rejections)
	}
}

func TestTournamentAllTransientFallbackReturnsError(t *testing.T) {
	one, two := broadCandidate("one"), broadCandidate("two")
	synchronizer := &fakeSynchronizer{analyzeErrors: map[string]error{"one": errors.New("one failed"), "two": errors.New("two failed")}}
	repository := &workflowRepository{}
	service := testService(t, inventory.Inventory{}, &fakeSearcher{result: provider.SearchResult{Candidates: []domain.Candidate{one, two}}}, nil, synchronizer, &fakeInstaller{})
	service.Providers = map[string]provider.Provider{"provider": &fakeProvider{id: "provider"}}
	service.Repository = repository
	if _, err := service.Run(context.Background(), serviceRequest(t)); err == nil || len(repository.rejections) != 0 {
		t.Fatalf("Run() error/rejections = %v/%#v", err, repository.rejections)
	}
}

func TestTournamentStopsOnCancellation(t *testing.T) {
	request := serviceRequest(t)
	request.Media.ReleaseGroup = "GROUP"
	leader := broadCandidate("leader")
	leader.ReleaseNames = []string{"Movie.2024-GROUP"}
	lower := broadCandidate("lower")
	ctx, cancel := context.WithCancel(context.Background())
	providerFake := &fakeProvider{id: "provider"}
	synchronizer := &fakeSynchronizer{analyzeHook: func(candidate domain.Candidate) {
		if candidate.ResultID == "leader" {
			cancel()
		}
	}}
	service := testService(t, inventory.Inventory{}, &fakeSearcher{result: provider.SearchResult{Candidates: []domain.Candidate{leader, lower}}}, nil, synchronizer, &fakeInstaller{})
	service.Providers = map[string]provider.Provider{"provider": providerFake}
	_, err := service.Run(ctx, request)
	if !errors.Is(err, context.Canceled) || !slices.Equal(providerFake.downloaded, []string{"leader"}) || len(synchronizer.synchronized) != 0 {
		t.Fatalf("Run() = %v, downloads=%#v synchronized=%#v", err, providerFake.downloaded, synchronizer.synchronized)
	}
}

func TestTournamentReachesSeasonPackLazilyAndExtractsOnce(t *testing.T) {
	request := serviceRequest(t)
	request.Media.Ref.Kind = domain.MediaEpisode
	request.Media.Season, request.Media.Episode = 1, 2
	request.Media.ReleaseGroup = "GROUP"
	leader := broadCandidate("leader")
	leader.Kind, leader.Season, leader.Episode = domain.MediaEpisode, 1, 2
	leader.ReleaseNames = []string{"Movie.S01E02-GROUP"}
	packCandidate := broadCandidate("pack")
	packCandidate.Kind, packCandidate.Season, packCandidate.Episode = domain.MediaEpisode, 1, 2
	packCandidate.Pack = &domain.PackInfo{Scope: domain.PackSeason, Season: 1}
	providerFake := &fakeProvider{id: "provider", payloads: map[string][]byte{"pack": workflowZIP(t, map[string]string{"Movie.S01E02.srt": installSRT})}, filenames: map[string]string{"pack": "season.zip"}}
	cache := &fakePackCache{}
	synchronizer := &fakeSynchronizer{verdicts: map[string]string{"leader": "unsure"}}
	service := testService(t, inventory.Inventory{}, &fakeSearcher{result: provider.SearchResult{Candidates: []domain.Candidate{packCandidate, leader}}}, cache, synchronizer, &fakeInstaller{})
	service.Providers = map[string]provider.Provider{"provider": providerFake}
	service.LapsePolicy.Mode = "always"

	result, err := service.Run(context.Background(), request)
	if err != nil || result.Candidate.ResultID != "pack" || cache.puts != 1 || !slices.Equal(providerFake.downloaded, []string{"leader", "pack"}) {
		t.Fatalf("Run() = %#v/%v cache puts=%d downloads=%#v", result, err, cache.puts, providerFake.downloaded)
	}
}

func TestTournamentAlwaysRemovesTemporaryWorkspace(t *testing.T) {
	tests := []struct {
		name string
		sync *fakeSynchronizer
		ctx  func() context.Context
	}{
		{name: "success", sync: &fakeSynchronizer{}},
		{name: "rejection", sync: &fakeSynchronizer{rejectAll: true}},
		{name: "error", sync: &fakeSynchronizer{analyzeErr: errors.New("lapse crashed")}},
		{name: "cancellation", sync: &fakeSynchronizer{analyzeErr: context.Canceled}, ctx: func() context.Context { ctx, cancel := context.WithCancel(context.Background()); cancel(); return ctx }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := serviceRequest(t)
			service := testService(t, inventory.Inventory{}, &fakeSearcher{result: provider.SearchResult{Candidates: []domain.Candidate{broadCandidate("candidate")}}}, nil, test.sync, &fakeInstaller{})
			service.Providers = map[string]provider.Provider{"provider": &fakeProvider{id: "provider"}}
			ctx := context.Background()
			if test.ctx != nil {
				ctx = test.ctx()
			}
			_, _ = service.Run(ctx, request)
			matches, err := filepath.Glob(filepath.Join(filepath.Dir(request.Media.Fingerprint.Path), ".subsyncd-work-*"))
			if err != nil || len(matches) != 0 {
				t.Fatalf("temporary workspaces = %#v, %v", matches, err)
			}
		})
	}
}

func hasDecisionStage(decisions []Decision, stage string) bool {
	for _, decision := range decisions {
		if decision.Stage == stage {
			return true
		}
	}
	return false
}

func decisionWithStage(decisions []Decision, stage string) (Decision, bool) {
	for _, decision := range decisions {
		if decision.Stage == stage {
			return decision, true
		}
	}
	return Decision{}, false
}

func analyzedTestCandidate(providerID, resultID string, priority int, rating, confidence float64) analyzedCandidate {
	return analyzedCandidate{downloadedCandidate: downloadedCandidate{candidate: domain.Candidate{ProviderID: providerID, ResultID: resultID, Rating: rating}, priority: priority}, analysis: domain.SyncResult{Confidence: confidence}}
}

func TestServiceStrongAnchoredFirstInstallBypassesLapse(t *testing.T) {
	request := serviceRequest(t)
	request.Media.ReleaseGroup = "GROUP"
	request.Media.Source = "WEB-DL"
	candidate := broadCandidate("strong")
	candidate.ReleaseNames = []string{"Movie.2024.1080p.WEB-DL-GROUP"}
	searcher := &fakeSearcher{result: provider.SearchResult{Candidates: []domain.Candidate{candidate}}}
	providerFake := &fakeProvider{id: "provider"}
	synchronizer := &fakeSynchronizer{}
	installer := &fakeInstaller{}
	service := testService(t, inventory.Inventory{}, searcher, nil, synchronizer, installer)
	service.Providers = map[string]provider.Provider{"provider": providerFake}

	result, err := service.Run(context.Background(), request)
	if err != nil || result.Outcome != OutcomeInstalled || result.Score.Total != 75 {
		t.Fatalf("Run() = %#v, %v", result, err)
	}
	if synchronizer.analyzeCalls != 0 || synchronizer.synchronizeCalls != 0 {
		t.Fatalf("strong anchored match invoked LAPSE: analyze/synchronize=%d/%d", synchronizer.analyzeCalls, synchronizer.synchronizeCalls)
	}
	if installer.request.SyncResult.Verdict != "score_bypass" || installer.request.SyncResult.Reference != "release_evidence" || installer.request.SyncResult.Confidence != 0 {
		t.Fatalf("sync provenance = %#v", installer.request.SyncResult)
	}
}

func TestServiceConfidencePolicyThresholdCanRequireLapse(t *testing.T) {
	request := serviceRequest(t)
	request.Media.ReleaseGroup = "GROUP"
	request.Media.Source = "WEB-DL"
	candidate := broadCandidate("below-bypass-threshold")
	candidate.ReleaseNames = []string{"Movie.2024.1080p.WEB-DL-GROUP"}
	searcher := &fakeSearcher{result: provider.SearchResult{Candidates: []domain.Candidate{candidate}}}
	synchronizer := &fakeSynchronizer{}
	service := testService(t, inventory.Inventory{}, searcher, nil, synchronizer, &fakeInstaller{})
	service.Providers = map[string]provider.Provider{"provider": &fakeProvider{id: "provider"}}
	service.LapsePolicy.BypassScore = 80

	result, err := service.Run(context.Background(), request)
	if err != nil || result.Outcome != OutcomeInstalled {
		t.Fatalf("Run() = %#v, %v", result, err)
	}
	if synchronizer.analyzeCalls != 1 || synchronizer.synchronizeCalls != 1 {
		t.Fatalf("below-threshold candidate did not invoke LAPSE: analyze/synchronize=%d/%d", synchronizer.analyzeCalls, synchronizer.synchronizeCalls)
	}
}

func TestLapseConfidencePolicySafetyBoundaries(t *testing.T) {
	strongScore := domain.Score{Total: 75, Contributions: []domain.Contribution{
		{Signal: "external_id", Points: 20},
		{Signal: "title_year", Points: 15},
		{Signal: "release_group", Points: 25},
		{Signal: "source", Points: 15},
	}}
	movie := domain.Media{Ref: domain.MediaRef{Kind: domain.MediaMovie}}
	episode := domain.Media{Ref: domain.MediaRef{Kind: domain.MediaEpisode}, Season: 1, Episode: 2}
	policy := DefaultLapsePolicy()

	tests := []struct {
		name      string
		media     domain.Media
		candidate domain.Candidate
		score     domain.Score
		installed bool
		policy    LapsePolicy
		want      bool
	}{
		{name: "strong movie", media: movie, score: strongScore, policy: policy, want: true},
		{name: "always policy", media: movie, score: strongScore, policy: func() LapsePolicy { p := policy; p.Mode = "always"; return p }()},
		{name: "below threshold", media: movie, score: domain.Score{Total: 74, Contributions: strongScore.Contributions}, policy: policy},
		{name: "missing identity", media: movie, score: domain.Score{Total: 75, Contributions: []domain.Contribution{{Signal: "release_group", Points: 25}}}, policy: policy},
		{name: "missing release group", media: movie, score: domain.Score{Total: 75, Contributions: []domain.Contribution{{Signal: "external_id", Points: 20}}}, policy: policy},
		{name: "episode without episode evidence", media: episode, score: strongScore, policy: policy},
		{name: "episode with explicit evidence", media: episode, candidate: domain.Candidate{Season: 1, Episode: 2}, score: strongScore, policy: policy, want: true},
		{name: "episode with release-name evidence", media: episode, candidate: domain.Candidate{ReleaseNames: []string{"Show.S01E02.1080p.WEB-DL-GROUP"}}, score: strongScore, policy: policy, want: true},
		{name: "pack", media: episode, candidate: domain.Candidate{Season: 1, Episode: 2, Pack: &domain.PackInfo{Scope: domain.PackSeason, Season: 1}}, score: strongScore, policy: policy},
		{name: "upgrade", media: movie, score: strongScore, installed: true, policy: policy},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := canBypassLapse(test.media, test.candidate, test.score, test.installed, test.policy); got != test.want {
				t.Fatalf("canBypassLapse() = %v, want %v", got, test.want)
			}
		})
	}
}

func TestCandidateRejectionSignatureIgnoresVolatileProviderMetrics(t *testing.T) {
	candidate := broadCandidate("stable")
	candidate.ReleaseNames = []string{"Movie.2024.1080p.WEB-DL-GROUP"}
	first, err := candidateSignature(candidate)
	if err != nil {
		t.Fatal(err)
	}
	candidate.Rating = 0.9
	candidate.Popularity = 0.8
	candidate.DownloadCount = 12345
	candidate.DownloadRef = "/different/temporary/url"
	second, err := candidateSignature(candidate)
	if err != nil {
		t.Fatal(err)
	}
	if second != first {
		t.Fatalf("volatile metrics changed candidate signature: %q != %q", second, first)
	}
	candidate.ReleaseNames = []string{"Movie.2024.1080p.BluRay-OTHER"}
	third, err := candidateSignature(candidate)
	if err != nil {
		t.Fatal(err)
	}
	if third == first {
		t.Fatal("changed release evidence retained candidate signature")
	}
}

func TestServiceDoesNotReprocessInstalledProviderCandidateAfterRescore(t *testing.T) {
	request := serviceRequest(t)
	request.Media.Resolution = "1080p"
	candidate := broadCandidate("same")
	alternative := broadCandidate("alternative")
	alternative.ReleaseNames = []string{"Movie.2024.1080p.WEB-DL-OTHER"}
	searcher := &fakeSearcher{result: provider.SearchResult{Candidates: []domain.Candidate{candidate, alternative}}}
	providerFake := &fakeProvider{id: "provider"}
	synchronizer := &fakeSynchronizer{}
	installer := &fakeInstaller{}
	repository := &workflowRepository{
		found: true,
		installation: store.Installation{
			MediaID:        request.MediaID,
			Language:       request.Language.String(),
			Path:           filepath.Join(filepath.Dir(request.Media.Fingerprint.Path), "Movie.en.srt"),
			ProviderID:     candidate.ProviderID,
			CandidateID:    candidate.ResultID,
			ScoreJSON:      []byte(`{"total":20}`),
			MediaPath:      request.Media.Fingerprint.Path,
			MediaFileID:    request.Media.Fingerprint.FileID,
			MediaSize:      request.Media.Fingerprint.Size,
			MediaModTimeNS: request.Media.Fingerprint.ModTime.UnixNano(),
		},
	}
	service := testService(t, inventory.Inventory{}, searcher, nil, synchronizer, installer)
	service.Repository = repository
	service.Providers = map[string]provider.Provider{"provider": providerFake}

	result, err := service.Run(context.Background(), request)
	updatedScore, _, scoreErr := installedScore(repository.installation)
	if err != nil || scoreErr != nil || result.Outcome != OutcomeSatisfied || result.NextUpgrade.IsZero() || updatedScore.Total != 35 || providerFake.downloads != 0 || synchronizer.analyzeCalls != 0 || installer.calls != 0 {
		t.Fatalf("Run() = %#v/%v downloads=%d analyzes=%d installs=%d", result, err, providerFake.downloads, synchronizer.analyzeCalls, installer.calls)
	}
}

func TestServiceRefreshesCachedPackAssessmentWithoutTreatingItAsFailure(t *testing.T) {
	request := serviceRequest(t)
	request.Media.Ref.Kind = domain.MediaEpisode
	request.Media.Season, request.Media.Episode = 1, 2
	candidate := broadCandidate("same-pack")
	candidate.Kind = domain.MediaEpisode
	candidate.Season, candidate.Episode = 1, 2
	candidate.Pack = &domain.PackInfo{Scope: domain.PackSeason, Season: 1}
	cachedPath := writeInstallFile(t, filepath.Join(t.TempDir(), "cached.srt"), installSRT)
	cache := &fakePackCache{member: pack.CachedMember{Path: cachedPath, Candidate: candidate}, found: true}
	searcher := &fakeSearcher{}
	synchronizer := &fakeSynchronizer{}
	repository := &workflowRepository{found: true, installation: matchingInstallation(request, candidate, []byte(`{"total":20}`))}
	service := testService(t, inventory.Inventory{}, searcher, cache, synchronizer, &fakeInstaller{})
	service.Repository = repository

	result, err := service.Run(context.Background(), request)
	updatedScore, _, scoreErr := installedScore(repository.installation)
	if err != nil || scoreErr != nil || result.Outcome != OutcomeSatisfied || result.NextUpgrade.IsZero() || updatedScore.Total != 55 || searcher.calls != 1 || synchronizer.analyzeCalls != 0 {
		t.Fatalf("Run() = %#v/%v score=%#v/%v search=%d analyzes=%d", result, err, updatedScore, scoreErr, searcher.calls, synchronizer.analyzeCalls)
	}
}

func TestServicePromotesSameCandidateToExactHashWithoutDownload(t *testing.T) {
	request := serviceRequest(t)
	candidate := exactCandidate("same")
	searcher := &fakeSearcher{result: provider.SearchResult{Candidates: []domain.Candidate{candidate}}}
	providerFake := &fakeProvider{id: "provider"}
	repository := &workflowRepository{found: true, installation: matchingInstallation(request, candidate, []byte(`{"total":35}`))}
	service := testService(t, inventory.Inventory{}, searcher, nil, &fakeSynchronizer{}, &fakeInstaller{})
	service.Repository = repository
	service.Providers = map[string]provider.Provider{"provider": providerFake}

	result, err := service.Run(context.Background(), request)
	updatedScore, exact, scoreErr := installedScore(repository.installation)
	if err != nil || scoreErr != nil || result.Outcome != OutcomeSatisfied || !result.NextUpgrade.IsZero() || updatedScore.Total != 100 || !exact || providerFake.downloads != 0 || !strings.Contains(string(repository.installation.SyncResultJSON), `"verdict":"exact_hash"`) {
		t.Fatalf("Run() = %#v/%v score=%#v exact=%v/%v downloads=%d sync=%s", result, err, updatedScore, exact, scoreErr, providerFake.downloads, repository.installation.SyncResultJSON)
	}
	searcher.calls = 0
	second, err := service.Run(context.Background(), request)
	if err != nil || second.Outcome != OutcomeSatisfied || searcher.calls != 0 {
		t.Fatalf("second Run() = %#v/%v search=%d", second, err, searcher.calls)
	}
}

func TestServiceRevalidatesSameProviderCandidateWhenMediaChanged(t *testing.T) {
	request := serviceRequest(t)
	candidate := exactCandidate("same")
	providerFake := &fakeProvider{id: "provider"}
	installer := &fakeInstaller{}
	existing := matchingInstallation(request, candidate, []byte(`{"total":35}`))
	existing.MediaModTimeNS--
	repository := &workflowRepository{found: true, installation: existing}
	service := testService(t, inventory.Inventory{}, &fakeSearcher{result: provider.SearchResult{Candidates: []domain.Candidate{candidate}}}, nil, &fakeSynchronizer{}, installer)
	service.Repository = repository
	service.Providers = map[string]provider.Provider{"provider": providerFake}

	result, err := service.Run(context.Background(), request)
	if err != nil || result.Outcome != OutcomeInstalled || providerFake.downloads != 1 || installer.calls != 1 {
		t.Fatalf("Run() = %#v/%v downloads=%d installs=%d", result, err, providerFake.downloads, installer.calls)
	}
}

func TestServiceHandlesProviderOutagesThrottlesZeroResultsAndCancellation(t *testing.T) {
	reset := time.Date(2026, 9, 4, 14, 0, 0, 0, time.UTC)
	t.Run("all throttled", func(t *testing.T) {
		searcher := &fakeSearcher{result: provider.SearchResult{Errors: map[string]error{"provider": &provider.CooldownError{ProviderID: "provider", Scope: provider.OperationSearch, ResetAt: reset}}}}
		service := testService(t, inventory.Inventory{}, searcher, nil, &fakeSynchronizer{}, &fakeInstaller{})
		result, err := service.Run(context.Background(), serviceRequest(t))
		if err != nil || result.Outcome != OutcomeThrottled || !result.RetryAt.Equal(reset) {
			t.Fatalf("Run() = %#v, %v", result, err)
		}
	})

	t.Run("partial outage", func(t *testing.T) {
		searcher := &fakeSearcher{result: provider.SearchResult{Candidates: []domain.Candidate{exactCandidate("healthy")}, Errors: map[string]error{"down": errors.New("unavailable")}}}
		providerFake := &fakeProvider{id: "provider"}
		service := testService(t, inventory.Inventory{}, searcher, nil, &fakeSynchronizer{}, &fakeInstaller{})
		service.Providers = map[string]provider.Provider{"provider": providerFake}
		result, err := service.Run(context.Background(), serviceRequest(t))
		if err != nil || result.Outcome != OutcomeInstalled {
			t.Fatalf("Run() = %#v, %v", result, err)
		}
	})

	t.Run("zero results", func(t *testing.T) {
		service := testService(t, inventory.Inventory{}, &fakeSearcher{}, nil, &fakeSynchronizer{}, &fakeInstaller{})
		result, err := service.Run(context.Background(), serviceRequest(t))
		if err != nil || result.Outcome != OutcomeNoResult {
			t.Fatalf("Run() = %#v, %v", result, err)
		}
	})

	t.Run("canceled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		service := testService(t, inventory.Inventory{}, &fakeSearcher{}, nil, &fakeSynchronizer{}, &fakeInstaller{})
		if _, err := service.Run(ctx, serviceRequest(t)); !errors.Is(err, context.Canceled) {
			t.Fatalf("Run() error = %T %v", err, err)
		}
	})
}

func TestServiceRejectsAmbiguousPackAndAllLapseFailures(t *testing.T) {
	t.Run("ambiguous pack", func(t *testing.T) {
		request := serviceRequest(t)
		request.Media.Ref.Kind = domain.MediaEpisode
		request.Media.Season = 1
		request.Media.Episode = 2
		candidate := broadCandidate("pack")
		candidate.Kind = domain.MediaEpisode
		candidate.Season = 1
		candidate.Pack = &domain.PackInfo{Scope: domain.PackSeason, Season: 1}
		providerFake := &fakeProvider{id: "provider", payloads: map[string][]byte{"pack": workflowZIP(t, map[string]string{"one.S01E02.srt": installSRT, "two.S01E02.srt": installSRT})}, filenames: map[string]string{"pack": "season.zip"}}
		cache := &fakePackCache{}
		repository := &workflowRepository{}
		service := testService(t, inventory.Inventory{}, &fakeSearcher{result: provider.SearchResult{Candidates: []domain.Candidate{candidate}}}, cache, &fakeSynchronizer{}, &fakeInstaller{})
		service.Repository = repository
		service.Providers = map[string]provider.Provider{"provider": providerFake}
		result, err := service.Run(context.Background(), request)
		if err != nil || result.Outcome != OutcomeRejected || cache.puts != 0 || len(repository.rejections) != 1 || repository.rejections[0].ReasonCode != "pack_selection" {
			t.Fatalf("Run() = %#v, %v, cache puts=%d rejections=%#v", result, err, cache.puts, repository.rejections)
		}
	})

	t.Run("all LAPSE failures", func(t *testing.T) {
		candidates := []domain.Candidate{broadCandidate("one"), broadCandidate("two"), broadCandidate("three")}
		providerFake := &fakeProvider{id: "provider"}
		service := testService(t, inventory.Inventory{}, &fakeSearcher{result: provider.SearchResult{Candidates: candidates}}, nil, &fakeSynchronizer{rejectAll: true}, &fakeInstaller{})
		service.Providers = map[string]provider.Provider{"provider": providerFake}
		result, err := service.Run(context.Background(), serviceRequest(t))
		if err != nil || result.Outcome != OutcomeRejected || providerFake.downloads != 3 {
			t.Fatalf("Run() = %#v, %v, downloads=%d", result, err, providerFake.downloads)
		}
	})
}

func TestServicePersistsDeterministicLapseRejectionsBeforeTheShortlist(t *testing.T) {
	candidates := []domain.Candidate{broadCandidate("1-bad"), broadCandidate("2-bad"), broadCandidate("3-bad"), broadCandidate("4-good")}
	searcher := &fakeSearcher{result: provider.SearchResult{Candidates: candidates}}
	providerFake := &fakeProvider{id: "provider"}
	synchronizer := &fakeSynchronizer{verdicts: map[string]string{"1-bad": "unsure", "2-bad": "nothing", "3-bad": "unsure"}}
	repository := &workflowRepository{}
	service := testService(t, inventory.Inventory{}, searcher, nil, synchronizer, &fakeInstaller{})
	service.Repository = repository
	service.Providers = map[string]provider.Provider{"provider": providerFake}
	request := serviceRequest(t)

	first, err := service.Run(context.Background(), request)
	if err != nil || first.Outcome != OutcomeRejected || !slices.Equal(providerFake.downloaded, []string{"1-bad", "2-bad", "3-bad"}) || len(repository.rejections) != 3 || repository.rejections[0].ExpiresAt.Sub(repository.rejections[0].RejectedAt) != 30*24*time.Hour {
		t.Fatalf("first Run() = %#v/%v downloads=%#v rejections=%#v", first, err, providerFake.downloaded, repository.rejections)
	}
	providerFake.downloaded = nil
	second, err := service.Run(context.Background(), request)
	if err != nil || second.Outcome != OutcomeInstalled || second.Candidate.ResultID != "4-good" || !slices.Equal(providerFake.downloaded, []string{"4-good"}) {
		t.Fatalf("second Run() = %#v/%v downloads=%#v", second, err, providerFake.downloaded)
	}
}

func TestServiceReturnsOperationalLapseFailureWithoutRejectingCandidate(t *testing.T) {
	candidate := broadCandidate("candidate")
	providerFake := &fakeProvider{id: "provider"}
	synchronizer := &fakeSynchronizer{analyzeErr: errors.New("LAPSE process crashed")}
	repository := &workflowRepository{}
	service := testService(t, inventory.Inventory{}, &fakeSearcher{result: provider.SearchResult{Candidates: []domain.Candidate{candidate}}}, nil, synchronizer, &fakeInstaller{})
	service.Repository = repository
	service.Providers = map[string]provider.Provider{"provider": providerFake}

	result, err := service.Run(context.Background(), serviceRequest(t))
	if err == nil || !strings.Contains(err.Error(), "process crashed") || result.Outcome != "" || len(repository.rejections) != 0 {
		t.Fatalf("Run() = %#v/%v rejections=%#v", result, err, repository.rejections)
	}
}

func TestServiceQuarantinesInvalidSubtitlePayload(t *testing.T) {
	candidate := broadCandidate("invalid")
	providerFake := &fakeProvider{id: "provider", payloads: map[string][]byte{"invalid": []byte("this is not a subtitle")}}
	repository := &workflowRepository{}
	service := testService(t, inventory.Inventory{}, &fakeSearcher{result: provider.SearchResult{Candidates: []domain.Candidate{candidate}}}, nil, &fakeSynchronizer{}, &fakeInstaller{})
	service.Repository = repository
	service.Providers = map[string]provider.Provider{"provider": providerFake}
	request := serviceRequest(t)

	first, err := service.Run(context.Background(), request)
	if err != nil || first.Outcome != OutcomeRejected || providerFake.downloads != 1 || len(repository.rejections) != 1 || repository.rejections[0].ReasonCode != "invalid_subtitle" {
		t.Fatalf("first Run() = %#v/%v downloads=%d rejections=%#v", first, err, providerFake.downloads, repository.rejections)
	}
	second, err := service.Run(context.Background(), request)
	if err != nil || second.Outcome != OutcomeRejected || providerFake.downloads != 1 {
		t.Fatalf("second Run() = %#v/%v downloads=%d", second, err, providerFake.downloads)
	}
}

func TestServiceTreatsPackCacheWriteAsBestEffort(t *testing.T) {
	request := serviceRequest(t)
	request.Media.Ref.Kind = domain.MediaEpisode
	request.Media.Season = 1
	request.Media.Episode = 2
	candidate := exactCandidate("pack")
	candidate.Kind = domain.MediaEpisode
	candidate.Season = 1
	candidate.Pack = &domain.PackInfo{Scope: domain.PackSeason, Season: 1}
	providerFake := &fakeProvider{id: "provider", payloads: map[string][]byte{"pack": workflowZIP(t, map[string]string{"Show.S01E02.srt": installSRT})}, filenames: map[string]string{"pack": "season.zip"}}
	cache := &fakePackCache{putErr: errors.New("cache unavailable")}
	service := testService(t, inventory.Inventory{}, &fakeSearcher{result: provider.SearchResult{Candidates: []domain.Candidate{candidate}}}, cache, &fakeSynchronizer{}, &fakeInstaller{})
	service.Providers = map[string]provider.Provider{"provider": providerFake}
	result, err := service.Run(context.Background(), request)
	if err != nil || result.Outcome != OutcomeInstalled || result.Candidate.ResultID != "pack" || cache.puts != 1 {
		t.Fatalf("Run() = %#v, %v, cache puts=%d", result, err, cache.puts)
	}
	if decision, found := decisionWithStage(result.Decisions, "pack_cache"); !found || !strings.Contains(decision.Reason, "cache unavailable") {
		t.Fatalf("cache decision = %#v", result.Decisions)
	}
}

func TestServiceBoundsProviderDownloadEvenWhenProviderIgnoresWriteError(t *testing.T) {
	providerFake := &fakeProvider{id: "provider", streamBytes: pack.DefaultLimits().MaxCompressed + 1, ignoreWriteError: true}
	service := testService(t, inventory.Inventory{}, &fakeSearcher{result: provider.SearchResult{Candidates: []domain.Candidate{exactCandidate("oversized")}}}, nil, &fakeSynchronizer{}, &fakeInstaller{})
	service.Providers = map[string]provider.Provider{"provider": providerFake}
	result, err := service.Run(context.Background(), serviceRequest(t))
	if err != nil || result.Outcome != OutcomeRejected {
		t.Fatalf("Run() = %#v, %v", result, err)
	}
	if decision, found := decisionWithStage(result.Decisions, "candidate"); !found || !strings.Contains(decision.Reason, "exceeds") {
		t.Fatalf("download decision = %#v", result.Decisions)
	}
}

func TestServiceAppliesUpgradeDeltaAndPersistsCandidatesWithoutDownloadReferences(t *testing.T) {
	request := serviceRequest(t)
	destination := strings.TrimSuffix(request.Media.Fingerprint.Path, filepath.Ext(request.Media.Fingerprint.Path)) + ".en.srt"
	currentPayload := []byte(installSRT)
	installation := installationWithScore(t, 70, false)
	installation.MediaID = request.MediaID
	installation.Language = "en"
	installation.Path = destination
	installation.Checksum = checksumBytes(currentPayload)
	installation.MediaPath = request.Media.Fingerprint.Path
	installation.MediaFileID = request.Media.Fingerprint.FileID
	installation.MediaSize = request.Media.Fingerprint.Size
	installation.MediaModTimeNS = request.Media.Fingerprint.ModTime.UnixNano()
	repository := &workflowRepository{installation: installation, found: true}
	searcher := &fakeSearcher{result: provider.SearchResult{Candidates: []domain.Candidate{broadCandidate("not-better")}}}
	providerFake := &fakeProvider{id: "provider"}
	service := testService(t, inventory.Inventory{Tracks: []inventory.Track{{Language: "en", Path: destination, Checksum: installation.Checksum}}}, searcher, nil, &fakeSynchronizer{}, &fakeInstaller{})
	service.Repository = repository
	service.Providers = map[string]provider.Provider{"provider": providerFake}
	result, err := service.Run(context.Background(), request)
	if err != nil || result.Outcome != OutcomeRejected || providerFake.downloads != 0 || len(repository.candidates) != 1 {
		t.Fatalf("Run() = %#v, %v, downloads=%d candidates=%d", result, err, providerFake.downloads, len(repository.candidates))
	}
	if strings.Contains(string(repository.candidates[0].MetadataJSON), "/secret/") {
		t.Fatalf("persisted candidate leaked download reference: %s", repository.candidates[0].MetadataJSON)
	}
}

func TestServiceClassifiesDownloadCooldownWhenNoCandidateCanRun(t *testing.T) {
	reset := time.Date(2026, 9, 4, 15, 0, 0, 0, time.UTC)
	providerFake := &fakeProvider{id: "provider", downloadErr: &provider.CooldownError{ProviderID: "provider", Scope: provider.OperationDownload, ResetAt: reset}}
	service := testService(t, inventory.Inventory{}, &fakeSearcher{result: provider.SearchResult{Candidates: []domain.Candidate{exactCandidate("exact")}}}, nil, &fakeSynchronizer{}, &fakeInstaller{})
	service.Providers = map[string]provider.Provider{"provider": providerFake}
	result, err := service.Run(context.Background(), serviceRequest(t))
	if err != nil || result.Outcome != OutcomeThrottled || !result.RetryAt.Equal(reset) {
		t.Fatalf("Run() = %#v, %v", result, err)
	}
}

func TestServiceDoesNotClassifyPartialProviderThrottleAsAllUnavailable(t *testing.T) {
	reset := time.Date(2026, 9, 4, 15, 0, 0, 0, time.UTC)
	searcher := &fakeSearcher{result: provider.SearchResult{Errors: map[string]error{"down": &provider.CooldownError{ProviderID: "down", Scope: provider.OperationSearch, ResetAt: reset}}}}
	service := testService(t, inventory.Inventory{}, searcher, nil, &fakeSynchronizer{}, &fakeInstaller{})
	service.ProviderOrder = []string{"down", "healthy"}
	result, err := service.Run(context.Background(), serviceRequest(t))
	if err != nil || result.Outcome != OutcomeNoResult || !result.RetryAt.IsZero() {
		t.Fatalf("Run() = %#v, %v", result, err)
	}
}

func TestServiceReturnsTechnicalFailureWhenEveryProviderErrors(t *testing.T) {
	searcher := &fakeSearcher{result: provider.SearchResult{Errors: map[string]error{
		"one": errors.New("network unavailable"),
		"two": errors.New("bad gateway"),
	}}}
	service := testService(t, inventory.Inventory{}, searcher, nil, &fakeSynchronizer{}, &fakeInstaller{})
	service.ProviderOrder = []string{"one", "two"}
	result, err := service.Run(context.Background(), serviceRequest(t))
	if err == nil || result.Outcome != "" || !strings.Contains(err.Error(), "all 2") {
		t.Fatalf("Run() = %#v, %v", result, err)
	}
}

func TestServiceManualSearchStillHonorsProviderCooldown(t *testing.T) {
	reset := time.Date(2026, 9, 4, 15, 0, 0, 0, time.UTC)
	searcher := &fakeSearcher{result: provider.SearchResult{Errors: map[string]error{"provider": &provider.CooldownError{ProviderID: "provider", Scope: provider.OperationSearch, ResetAt: reset}}}}
	service := testService(t, inventory.Inventory{}, searcher, nil, &fakeSynchronizer{}, &fakeInstaller{})
	request := serviceRequest(t)
	request.Manual = true
	result, err := service.Run(context.Background(), request)
	if err != nil || result.Outcome != OutcomeThrottled || !result.RetryAt.Equal(reset) {
		t.Fatalf("Run() = %#v, %v", result, err)
	}
}

func testService(t *testing.T, current inventory.Inventory, searcher *fakeSearcher, cache PackCache, sync CandidateSynchronizer, installer CandidateInstaller) *Service {
	t.Helper()
	if sync == nil {
		sync = &fakeSynchronizer{}
	}
	if installer == nil {
		installer = &fakeInstaller{}
	}
	return &Service{Inventory: &fakeInventory{current: current}, Searcher: searcher, PackCache: cache, Synchronizer: sync, Installer: installer, Repository: &workflowRepository{}, Providers: map[string]provider.Provider{}, ProviderOrder: []string{"provider"}, MinimumScore: 35, PackTTL: 24 * time.Hour, LapsePolicy: DefaultLapsePolicy(), Clock: fixedWorkflowClock{at: time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)}}
}

func serviceRequest(t *testing.T) Request {
	t.Helper()
	root := t.TempDir()
	mediaPath := writeInstallFile(t, filepath.Join(root, "Movie.mkv"), "media")
	info, err := os.Stat(mediaPath)
	if err != nil {
		t.Fatal(err)
	}
	return Request{MediaID: 1, Media: domain.Media{Ref: domain.MediaRef{Instance: "sonarr", Kind: domain.MediaMovie, FileID: 7}, Fingerprint: domain.MediaFingerprint{Path: mediaPath, FileID: 7, Size: info.Size(), ModTime: info.ModTime()}, Title: "Movie", Year: 2024, ExternalIDs: domain.ExternalIDs{IMDb: "tt123"}, Duration: 90 * time.Minute}, Language: "en"}
}

func exactCandidate(id string) domain.Candidate {
	candidate := broadCandidate(id)
	candidate.ExactHash = true
	return candidate
}

func broadCandidate(id string) domain.Candidate {
	return domain.Candidate{ProviderID: "provider", ResultID: id, Language: "en", Kind: domain.MediaMovie, Title: "Movie", Year: 2024, ExternalIDs: domain.ExternalIDs{IMDb: "tt123"}, DownloadRef: "/secret/" + id}
}

func matchingInstallation(request Request, candidate domain.Candidate, scoreJSON []byte) store.Installation {
	fingerprint := request.Media.Fingerprint
	return store.Installation{
		MediaID: request.MediaID, Language: request.Language.String(), Path: filepath.Join(filepath.Dir(fingerprint.Path), "Movie.en.srt"),
		ProviderID: candidate.ProviderID, CandidateID: candidate.ResultID, ScoreJSON: scoreJSON,
		MediaPath: fingerprint.Path, MediaFileID: fingerprint.FileID, MediaSize: fingerprint.Size, MediaModTimeNS: fingerprint.ModTime.UnixNano(),
	}
}

type fakeInventory struct {
	current inventory.Inventory
	calls   int
}

func (f *fakeInventory) Refresh(context.Context, int64, domain.Media, bool) (inventory.Inventory, error) {
	f.calls++
	return f.current, nil
}

type fakeSearcher struct {
	result provider.SearchResult
	calls  int
	query  provider.SearchQuery
}

func (f *fakeSearcher) Search(_ context.Context, query provider.SearchQuery) provider.SearchResult {
	f.calls++
	f.query = query
	if f.result.Errors == nil {
		f.result.Errors = map[string]error{}
	}
	return f.result
}

type fakePackCache struct {
	member pack.CachedMember
	found  bool
	err    error
	puts   int
	putErr error
}

func (f *fakePackCache) Find(context.Context, domain.Media, domain.Language) (pack.CachedMember, bool, error) {
	return f.member, f.found, f.err
}

func (f *fakePackCache) Put(context.Context, pack.Manifest, time.Time) error {
	f.puts++
	return f.putErr
}

type fakeProvider struct {
	id               string
	downloads        int
	downloaded       []string
	downloadErr      error
	payloads         map[string][]byte
	filenames        map[string]string
	streamBytes      int64
	ignoreWriteError bool
}

func (f *fakeProvider) ID() string                            { return f.id }
func (f *fakeProvider) Capabilities() provider.Capabilities   { return provider.Capabilities{} }
func (f *fakeProvider) SupportsLanguage(domain.Language) bool { return true }
func (f *fakeProvider) Search(context.Context, provider.SearchQuery) ([]domain.Candidate, error) {
	return nil, nil
}
func (f *fakeProvider) Download(_ context.Context, candidate domain.Candidate, writer io.Writer) (provider.DownloadMetadata, error) {
	f.downloads++
	f.downloaded = append(f.downloaded, candidate.ResultID)
	if f.downloadErr != nil {
		return provider.DownloadMetadata{}, f.downloadErr
	}
	if f.streamBytes > 0 {
		_, err := io.CopyN(writer, zeroReader{}, f.streamBytes)
		if f.ignoreWriteError {
			err = nil
		}
		return provider.DownloadMetadata{Filename: candidate.ResultID + ".srt", ContentType: "application/x-subrip"}, err
	}
	payload := f.payloads[candidate.ResultID]
	if payload == nil {
		payload = []byte(strings.Replace(installSRT, "Hello", candidate.ResultID, 1))
	}
	_, err := writer.Write(payload)
	filename := f.filenames[candidate.ResultID]
	if filename == "" {
		filename = candidate.ResultID + ".srt"
	}
	return provider.DownloadMetadata{Filename: filename, ContentType: "application/x-subrip"}, err
}

type zeroReader struct{}

func (zeroReader) Read(payload []byte) (int, error) {
	clear(payload)
	return len(payload), nil
}

type fakeSynchronizer struct {
	confidence        map[string]float64
	verdicts          map[string]string
	analyzeErr        error
	rejectText        string
	rejectAll         bool
	analyzeCalls      int
	synchronizeCalls  int
	analyzed          []string
	synchronized      []string
	analyzeErrors     map[string]error
	synchronizeErrors map[string]error
	analyzeHook       func(domain.Candidate)
}

func (f *fakeSynchronizer) AnalyzeCandidate(ctx context.Context, candidate domain.Candidate, _ string, subtitle string) (domain.SyncResult, error) {
	f.analyzeCalls++
	f.analyzed = append(f.analyzed, candidate.ResultID)
	if f.analyzeHook != nil {
		f.analyzeHook(candidate)
	}
	if err := ctx.Err(); err != nil {
		return domain.SyncResult{}, err
	}
	if err := f.analyzeErrors[candidate.ResultID]; err != nil {
		return domain.SyncResult{}, err
	}
	if f.analyzeErr != nil {
		return domain.SyncResult{}, f.analyzeErr
	}
	if verdict := f.verdicts[candidate.ResultID]; verdict != "" {
		return domain.SyncResult{}, &syncer.VerdictError{Verdict: verdict, Reason: "test verdict"}
	}
	payload, _ := os.ReadFile(subtitle)
	if f.rejectAll || f.rejectText != "" && strings.Contains(string(payload), f.rejectText) {
		return domain.SyncResult{}, &syncer.VerdictError{Verdict: "unsure", Reason: "test rejection"}
	}
	return f.syncResult(payload), nil
}

func (f *fakeSynchronizer) SynchronizeCandidate(_ context.Context, candidate domain.Candidate, _ string, input, output string) (domain.SyncResult, error) {
	f.synchronizeCalls++
	f.synchronized = append(f.synchronized, candidate.ResultID)
	if err := f.synchronizeErrors[candidate.ResultID]; err != nil {
		return domain.SyncResult{}, err
	}
	payload, err := os.ReadFile(input)
	if err != nil {
		return domain.SyncResult{}, err
	}
	if err := os.WriteFile(output, payload, 0o600); err != nil {
		return domain.SyncResult{}, err
	}
	if f.rejectAll || f.rejectText != "" && strings.Contains(string(payload), f.rejectText) {
		return domain.SyncResult{}, &syncer.VerdictError{Verdict: "unsure", Reason: "test rejection"}
	}
	return f.syncResult(payload), nil
}

func (f *fakeSynchronizer) syncResult(payload []byte) domain.SyncResult {
	confidence := 0.5
	for id, value := range f.confidence {
		if strings.Contains(string(payload), id) {
			confidence = value
		}
	}
	return domain.SyncResult{Verdict: "solid", Confidence: confidence, Ratio: 1, Parts: 1}
}

type fakeInstaller struct {
	request InstallRequest
	calls   int
	err     error
}

func (f *fakeInstaller) Install(_ context.Context, request InstallRequest) (store.Installation, error) {
	f.calls++
	f.request = request
	if f.err != nil {
		return store.Installation{}, f.err
	}
	return store.Installation{MediaID: request.MediaID, Language: request.Language.String(), Path: request.DestinationPath, ProviderID: request.Candidate.ProviderID, CandidateID: request.Candidate.ResultID}, nil
}

type workflowRepository struct {
	installation store.Installation
	found        bool
	candidates   []store.CandidateRecord
	rejections   []store.CandidateRejection
}

func (r *workflowRepository) GetInstallation(context.Context, int64, domain.Language) (store.Installation, bool, error) {
	return r.installation, r.found, nil
}

func (r *workflowRepository) UpdateInstallationAssessment(_ context.Context, installation store.Installation) error {
	r.installation = installation
	return nil
}

func (r *workflowRepository) RecordCandidates(_ context.Context, _ int64, _ domain.Language, candidates []store.CandidateRecord) error {
	r.candidates = append([]store.CandidateRecord(nil), candidates...)
	return nil
}

func (r *workflowRepository) PutCandidateRejection(_ context.Context, rejection store.CandidateRejection) error {
	r.rejections = append(r.rejections, rejection)
	return nil
}

func (r *workflowRepository) GetCandidateRejection(_ context.Context, lookup store.CandidateRejectionLookup) (store.CandidateRejection, bool, error) {
	for _, rejection := range r.rejections {
		if rejection.MediaID == lookup.MediaID && rejection.Language == lookup.Language && rejection.ProviderID == lookup.ProviderID && rejection.ResultID == lookup.ResultID && rejection.CandidateSignature == lookup.CandidateSignature && rejection.ToolSignature == lookup.ToolSignature && rejection.MediaPath == lookup.MediaPath && rejection.MediaFileID == lookup.MediaFileID && rejection.MediaSize == lookup.MediaSize && rejection.MediaModTimeNS == lookup.MediaModTimeNS && rejection.ExpiresAt.After(lookup.Now) && (lookup.ArtifactChecksum == "" || rejection.ArtifactChecksum == lookup.ArtifactChecksum) {
			return rejection, true, nil
		}
	}
	return store.CandidateRejection{}, false, nil
}

type fixedWorkflowClock struct{ at time.Time }

func (c fixedWorkflowClock) Now() time.Time { return c.at }

func workflowZIP(t *testing.T, entries map[string]string) []byte {
	t.Helper()
	var payload bytes.Buffer
	archive := zip.NewWriter(&payload)
	for name, content := range entries {
		writer, err := archive.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(writer, content); err != nil {
			t.Fatal(err)
		}
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	return payload.Bytes()
}
