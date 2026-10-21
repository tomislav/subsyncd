package workflow

import (
	"context"
	"strings"
	"testing"

	"subsyncd/internal/domain"
	"subsyncd/internal/inventory"
	"subsyncd/internal/provider"
)

// releaseNumberedPackService searches Sonarr's S01E10, which its release
// names S02E02, and finds a season pack in the release's numbering.
func releaseNumberedPackService(t *testing.T, policy LapsePolicy) (*Service, Request, *fakeSynchronizer, *versionInstaller) {
	t.Helper()
	request := serviceRequest(t)
	request.Media.Ref.Kind = domain.MediaEpisode
	request.Media.Season, request.Media.Episode = 1, 10
	request.Media.ReleaseName = "Movie.S02E02.MULTI.2160p.WEB.H265-GROUP"
	request.Media.OriginalFilename = request.Media.ReleaseName
	candidate := broadCandidate("split-season")
	candidate.Kind = domain.MediaEpisode
	candidate.Season = 1
	adapter := &fakeProvider{id: "provider", payloads: map[string][]byte{"split-season": workflowZIP(t, map[string]string{
		"Movie.S02E01.WEB.srt": strings.ReplaceAll(installSRT, "Hello", "episode-one"),
		"Movie.S02E02.WEB.srt": strings.ReplaceAll(installSRT, "Hello", "episode-two"),
		"Movie.S02E03.WEB.srt": strings.ReplaceAll(installSRT, "Hello", "episode-three"),
	})}}
	synchronizer := &fakeSynchronizer{}
	installer := &versionInstaller{}
	service := testService(t, inventory.Inventory{}, &fakeSearcher{result: provider.SearchResult{Candidates: []domain.Candidate{candidate}}}, nil, synchronizer, installer)
	service.Providers["provider"] = adapter
	service.LapsePolicy = policy
	return service, request, synchronizer, installer
}

func TestReleaseNumberedPackMemberInstallsAfterLapse(t *testing.T) {
	service, request, synchronizer, installer := releaseNumberedPackService(t, DefaultLapsePolicy())
	result, err := service.Run(context.Background(), request)
	if err != nil || result.Outcome != OutcomeInstalled || synchronizer.synchronizeCalls != 1 || len(installer.payloads) != 1 || !strings.Contains(installer.payloads[0], "episode-two") {
		t.Fatalf("result=%+v err=%v synchronize=%d installs=%q", result, err, synchronizer.synchronizeCalls, installer.payloads)
	}
}

// Under sync.policy never nothing would check the timing of a member chosen
// only by the release's number, so the pack is rejected instead.
func TestReleaseNumberedPackMemberIsRejectedWithoutLapse(t *testing.T) {
	service, request, synchronizer, installer := releaseNumberedPackService(t, neverSyncPolicy())
	repository := service.Repository.(*workflowRepository)
	result, err := service.Run(context.Background(), request)
	if err != nil || result.Outcome == OutcomeInstalled || len(installer.payloads) != 0 || synchronizer.synchronizeCalls != 0 {
		t.Fatalf("result=%+v err=%v installs=%d synchronize=%d", result, err, len(installer.payloads), synchronizer.synchronizeCalls)
	}
	if len(repository.rejections) != 1 || repository.rejections[0].ReasonCode != "pack_selection" || repository.rejections[0].ResultID != "split-season" {
		t.Fatalf("rejections = %+v", repository.rejections)
	}
}

func TestReleaseNumberedProviderPackMemberIsRejectedWhenPacksSkipLapse(t *testing.T) {
	policy := DefaultLapsePolicy()
	policy.LapseForPacks = false
	service, request, synchronizer, installer := releaseNumberedPackService(t, policy)
	searcher := service.Searcher.(*fakeSearcher)
	searcher.result.Candidates[0].Pack = &domain.PackInfo{Scope: domain.PackSeason, Season: 1}
	service.Providers["provider"].(*fakeProvider).payloads = map[string][]byte{"split-season": workflowZIP(t, map[string]string{
		"Movie.S02E01.WEB.srt": strings.ReplaceAll(installSRT, "Hello", "episode-one"),
		"Movie.S02E02.WEB.srt": strings.ReplaceAll(installSRT, "Hello", "episode-two"),
	})}
	result, err := service.Run(context.Background(), request)
	if err != nil || result.Outcome == OutcomeInstalled || len(installer.payloads) != 0 || synchronizer.synchronizeCalls != 0 {
		t.Fatalf("result=%+v err=%v installs=%d synchronize=%d", result, err, len(installer.payloads), synchronizer.synchronizeCalls)
	}
}
