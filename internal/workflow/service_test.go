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
		if err != nil || result.Candidate.ResultID != "four" || sync.analyzeCalls != 3 || sync.synchronizeCalls != 3 || !slices.Equal(providerFake.downloaded, []string{"five", "four", "one"}) {
			t.Fatalf("Run() = %#v, %v, analyzed/synced=%d/%d downloads=%#v", result, err, sync.analyzeCalls, sync.synchronizeCalls, providerFake.downloaded)
		}
	})
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
		service := testService(t, inventory.Inventory{}, &fakeSearcher{result: provider.SearchResult{Candidates: []domain.Candidate{candidate}}}, cache, &fakeSynchronizer{}, &fakeInstaller{})
		service.Providers = map[string]provider.Provider{"provider": providerFake}
		result, err := service.Run(context.Background(), request)
		if err != nil || result.Outcome != OutcomeRejected || cache.puts != 0 {
			t.Fatalf("Run() = %#v, %v, cache puts=%d", result, err, cache.puts)
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
	if len(result.Decisions) != 1 || result.Decisions[0].Stage != "pack_cache" || !strings.Contains(result.Decisions[0].Reason, "cache unavailable") {
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
	if len(result.Decisions) != 1 || !strings.Contains(result.Decisions[0].Reason, "exceeds") {
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
	return &Service{Inventory: &fakeInventory{current: current}, Searcher: searcher, PackCache: cache, Synchronizer: sync, Installer: installer, Repository: &workflowRepository{}, Providers: map[string]provider.Provider{}, ProviderOrder: []string{"provider"}, MinimumScore: 35, PackTTL: 24 * time.Hour, Clock: fixedWorkflowClock{at: time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)}}
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
	confidence       map[string]float64
	rejectText       string
	rejectAll        bool
	analyzeCalls     int
	synchronizeCalls int
}

func (f *fakeSynchronizer) AnalyzeCandidate(_ context.Context, _ domain.Candidate, _ string, subtitle string) (domain.SyncResult, error) {
	f.analyzeCalls++
	payload, _ := os.ReadFile(subtitle)
	if f.rejectAll || f.rejectText != "" && strings.Contains(string(payload), f.rejectText) {
		return domain.SyncResult{}, errors.New("rejected")
	}
	confidence := 0.5
	for id, value := range f.confidence {
		if strings.Contains(string(payload), id) {
			confidence = value
		}
	}
	return domain.SyncResult{Verdict: "solid", Confidence: confidence, Ratio: 1, Parts: 1}, nil
}

func (f *fakeSynchronizer) SynchronizeCandidate(_ context.Context, _ domain.Candidate, _ string, input, output string) (domain.SyncResult, error) {
	f.synchronizeCalls++
	payload, err := os.ReadFile(input)
	if err != nil {
		return domain.SyncResult{}, err
	}
	if err := os.WriteFile(output, payload, 0o600); err != nil {
		return domain.SyncResult{}, err
	}
	result, err := f.AnalyzeCandidate(context.Background(), domain.Candidate{}, "", input)
	f.analyzeCalls--
	return result, err
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
}

func (r *workflowRepository) GetInstallation(context.Context, int64, domain.Language) (store.Installation, bool, error) {
	return r.installation, r.found, nil
}

func (r *workflowRepository) RecordCandidates(_ context.Context, _ int64, _ domain.Language, candidates []store.CandidateRecord) error {
	r.candidates = append([]store.CandidateRecord(nil), candidates...)
	return nil
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
