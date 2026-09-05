package workflow

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"subsyncd/internal/domain"
	"subsyncd/internal/inventory"
	"subsyncd/internal/pack"
	"subsyncd/internal/provider"
	"subsyncd/internal/store"
)

type broadValidationInstaller struct {
	fakeInstaller
	failure   error
	before    func(InstallRequest)
	attempted []string
}

func (i *broadValidationInstaller) Install(ctx context.Context, r InstallRequest) (store.Installation, error) {
	if i.before != nil {
		i.before(r)
	}
	i.attempted = append(i.attempted, r.Candidate.ResultID)
	if r.Candidate.ResultID == "first" && i.failure != nil {
		return store.Installation{}, i.failure
	}
	if _, err := validatedSubtitle(r.SourcePath, r.Media.Duration); err != nil {
		return store.Installation{}, err
	}
	return i.fakeInstaller.Install(ctx, r)
}

type broadOutputSynchronizer struct {
	fakeSynchronizer
	t                                       *testing.T
	invalid                                 bool
	firstSource, firstOutput, firstChecksum string
}

func (s *broadOutputSynchronizer) SynchronizeCandidate(ctx context.Context, c domain.Candidate, media, input, output string) (domain.SyncResult, error) {
	result, err := s.fakeSynchronizer.SynchronizeCandidate(ctx, c, media, input, output)
	if c.ResultID == "first" {
		s.firstSource = input
		s.firstOutput = output
		s.firstChecksum, _ = fileChecksum(input)
		if s.invalid {
			err = os.WriteFile(output, []byte("1\n00:00:01,000 --> 01:36:00,000\nInvalid duration\n"), 0600)
		}
	}
	return result, err
}

func TestBroadInstallationContentFailureFallsBack(t *testing.T) {
	for _, tier := range []string{"same_tier", "lower_tier"} {
		t.Run(tier, func(t *testing.T) {
			request := serviceRequest(t)
			first, second := broadCandidate("first"), broadCandidate("second")
			if tier == "lower_tier" {
				request.Media.ReleaseGroup = "GROUP"
				first.ReleaseNames = []string{"Movie.2024-GROUP"}
			}
			synchronizer := &broadOutputSynchronizer{t: t, invalid: true}
			installer := &broadValidationInstaller{before: func(r InstallRequest) {
				if r.Candidate.ResultID == "second" {
					for _, path := range []string{synchronizer.firstSource, synchronizer.firstOutput} {
						if _, err := os.Stat(path); !os.IsNotExist(err) {
							t.Errorf("rejected install scratch survives: %v", err)
						}
					}
				}
			}}
			service := testService(t, inventory.Inventory{}, &fakeSearcher{result: provider.SearchResult{Candidates: []domain.Candidate{first, second}}}, nil, synchronizer, installer)
			service.LapsePolicy.Mode = "always"
			service.Providers = map[string]provider.Provider{"provider": &fakeProvider{id: "provider"}}
			result, err := service.Run(context.Background(), request)
			if err != nil || result.Outcome != OutcomeInstalled || result.Candidate.ResultID != "second" {
				t.Fatalf("Run=%+v,%v", result, err)
			}
			rejections := service.Repository.(*workflowRepository).rejections
			if len(rejections) != 1 || rejections[0].ResultID != "first" || rejections[0].ReasonCode != "invalid_subtitle" || rejections[0].ArtifactChecksum != synchronizer.firstChecksum {
				t.Fatalf("rejections=%+v", rejections)
			}
			early := 0
			for _, d := range result.Decisions {
				if d.Stage == "early_stop" {
					early++
					if d.ResultID != "second" {
						t.Errorf("false early stop: %+v", d)
					}
				}
			}
			if early != 1 {
				t.Fatalf("early stops=%d", early)
			}
			if synchronizer.analyzeCalls != 0 || synchronizer.synchronizeCalls != 2 || len(installer.attempted) != 2 {
				t.Fatalf("unexpected fallback calls: %+v %+v", synchronizer, installer)
			}
		})
	}
}

func TestBroadInstallationTechnicalFailureIsTerminal(t *testing.T) {
	for _, kind := range []string{"technical", "wrapped_validation", "joined_rollback"} {
		t.Run(kind, func(t *testing.T) {
			technical := errors.New("database commit failed")
			var failure error = technical
			validation := &subtitleValidationError{reason: "invalid subtitle"}
			if kind == "wrapped_validation" {
				failure = fmt.Errorf("wrapped failure: %w", validation)
			}
			if kind == "joined_rollback" {
				failure = errors.Join(validation, technical)
			}
			installer := &broadValidationInstaller{failure: failure}
			synchronizer := &fakeSynchronizer{}
			service := testService(t, inventory.Inventory{}, &fakeSearcher{result: provider.SearchResult{Candidates: []domain.Candidate{broadCandidate("first"), broadCandidate("second")}}}, nil, synchronizer, installer)
			service.LapsePolicy.Mode = "always"
			service.Providers = map[string]provider.Provider{"provider": &fakeProvider{id: "provider"}}
			result, err := service.Run(context.Background(), serviceRequest(t))
			if !errors.Is(err, failure) || len(installer.attempted) != 1 || synchronizer.synchronizeCalls != 2 {
				t.Fatalf("Run=%+v,%v; attempts=%v", result, err, installer.attempted)
			}
			if len(service.Repository.(*workflowRepository).rejections) != 0 {
				t.Fatal("technical failure poisoned rejection state")
			}
			for _, d := range result.Decisions {
				if d.Stage == "early_stop" {
					t.Fatalf("false early stop: %+v", d)
				}
			}
		})
	}
}

func TestBroadFormatChangingUpgradeFallsBack(t *testing.T) {
	for _, terminal := range []bool{false, true} {
		t.Run(fmt.Sprint(terminal), func(t *testing.T) {
			request := serviceRequest(t)
			repository := &workflowRepository{found: true, installation: matchingInstallation(request, broadCandidate("installed"), []byte(`{"total":0}`))}
			adapter := &fakeProvider{id: "provider", payloads: map[string][]byte{"first": []byte("WEBVTT\n\n00:00:00.000 --> 00:00:01.000\nFirst\n")}, filenames: map[string]string{"first": "first.vtt"}}
			installer := &fakeInstaller{}
			if terminal {
				installer.err = errors.New("database unavailable")
			}
			synchronizer := &broadOutputSynchronizer{t: t}
			service := testService(t, managedSidecarInventory(repository.installation), &fakeSearcher{result: provider.SearchResult{Candidates: []domain.Candidate{broadCandidate("first"), broadCandidate("second")}}}, nil, synchronizer, installer)
			service.Repository = repository
			service.Providers = map[string]provider.Provider{"provider": adapter}
			result, err := service.Run(context.Background(), request)
			if terminal {
				if !errors.Is(err, installer.err) || result.Outcome != "" {
					t.Fatalf("Run=%+v,%v", result, err)
				}
			} else if err != nil || result.Outcome != OutcomeInstalled || result.Candidate.ResultID != "second" {
				t.Fatalf("Run=%+v,%v", result, err)
			}
			if installer.calls != 1 || installer.request.Candidate.ResultID != "second" {
				t.Fatalf("installer=%+v", installer)
			}
			if len(repository.rejections) != 0 {
				t.Fatalf("format change poisoned content rejection: %+v", repository.rejections)
			}
			for _, d := range result.Decisions {
				if d.Stage == "early_stop" && (terminal || d.ResultID != "second") {
					t.Fatalf("false early stop: %+v", d)
				}
			}
		})
	}
}

func TestCachedInstallationFallbackPreservesImmutableMember(t *testing.T) {
	for _, kind := range []string{"content", "format", "technical", "wrapped_validation", "joined_rollback"} {
		t.Run(kind, func(t *testing.T) {
			request := serviceRequest(t)
			request.Media.Ref.Kind = domain.MediaEpisode
			request.Media.Season = 1
			request.Media.Episode = 2
			candidate := broadCandidate("first")
			candidate.Kind = domain.MediaEpisode
			candidate.Season = 1
			candidate.Episode = 2
			candidate.Pack = &domain.PackInfo{Scope: domain.PackSeason, Season: 1}
			filename, payload := "cached.srt", installSRT
			if kind == "content" {
				payload = "1\n00:00:01,000 --> 01:36:00,000\nInvalid duration\n"
			}
			if kind == "format" {
				filename = "cached.vtt"
				payload = "WEBVTT\n\n00:00:00.000 --> 00:00:01.000\nCached\n"
			}
			path := writeInstallFile(t, filepath.Join(t.TempDir(), filename), payload)
			checksum, err := fileChecksum(path)
			if err != nil {
				t.Fatal(err)
			}
			cache := &fakePackCache{member: pack.CachedMember{Path: path, Checksum: checksum, Candidate: candidate}, found: true}
			remote := exactCandidate("second")
			remote.Kind = domain.MediaEpisode
			searcher := &fakeSearcher{result: provider.SearchResult{Candidates: []domain.Candidate{remote}}}
			installer := &broadValidationInstaller{}
			validation := &subtitleValidationError{reason: "invalid subtitle"}
			technical := errors.New("database failed")
			switch kind {
			case "technical":
				installer.failure = technical
			case "wrapped_validation":
				installer.failure = fmt.Errorf("wrapped: %w", validation)
			case "joined_rollback":
				installer.failure = errors.Join(validation, technical)
			}
			var output string
			synchronizer := &cleanupSynchronizer{sync: func(_ domain.Candidate, _, out string) error { output = out; return nil }}
			adapter := &cleanupProvider{fakeProvider: fakeProvider{id: "provider"}, before: func(domain.Candidate) {
				if _, err := os.Stat(output); !os.IsNotExist(err) {
					t.Errorf("cached failed synchronized artifact survives: %v", err)
				}
				if got, err := os.ReadFile(path); err != nil || string(got) != payload {
					t.Fatalf("cached member changed: %v", err)
				}
			}}
			repository := &workflowRepository{}
			current := inventory.Inventory{}
			if kind == "format" {
				repository.found = true
				repository.installation = matchingInstallation(request, broadCandidate("installed"), []byte(`{"total":0}`))
				current = managedSidecarInventory(repository.installation)
			}
			service := testService(t, current, searcher, cache, synchronizer, installer)
			service.Repository = repository
			service.Providers = map[string]provider.Provider{"provider": adapter}
			result, err := service.Run(context.Background(), request)
			if installer.failure != nil {
				if !errors.Is(err, installer.failure) || searcher.calls != 0 || len(repository.rejections) != 0 {
					t.Fatalf("terminal Run=%+v,%v search=%d rejections=%v", result, err, searcher.calls, repository.rejections)
				}
			} else {
				if err != nil || result.Outcome != OutcomeInstalled || result.Candidate.ResultID != "second" || searcher.calls != 1 {
					t.Fatalf("fallback Run=%+v,%v search=%d", result, err, searcher.calls)
				}
			}
			if kind == "content" {
				if len(repository.rejections) != 1 || repository.rejections[0].ArtifactChecksum != checksum || repository.rejections[0].ResultID != "first" || repository.rejections[0].ReasonCode != "invalid_subtitle" {
					t.Fatalf("member rejection=%+v", repository.rejections)
				}
			} else if len(repository.rejections) != 0 {
				t.Fatalf("unexpected rejection=%+v", repository.rejections)
			}
			if got, err := os.ReadFile(path); err != nil || string(got) != payload {
				t.Fatalf("cached member changed after Run: %v", err)
			}
		})
	}
}
