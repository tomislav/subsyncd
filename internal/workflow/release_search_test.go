package workflow

import (
	"context"
	"strings"
	"testing"

	"subsyncd/internal/domain"
	"subsyncd/internal/inventory"
	"subsyncd/internal/provider"
)

// splitSeasonService is One Hundred Years of Solitude's case: Sonarr numbers
// the file S01E10, its release and every provider number it S02E02. Searching
// Sonarr's season finds only the Part 1 pack (S01E01-S01E03 here); the Part 2
// pack (S02E01-S02E03) is filed under season 2.
func splitSeasonService(t *testing.T, policy LapsePolicy) (*Service, Request, *fakeSearcher, *fakeSynchronizer, *versionInstaller, *fakePackCache) {
	t.Helper()
	request := serviceRequest(t)
	request.Media.Ref.Kind = domain.MediaEpisode
	request.Media.Season, request.Media.Episode = 1, 10
	request.Media.ReleaseName = "Movie.S02E02.MULTI.2160p.WEB.H265-GROUP"
	request.Media.OriginalFilename = request.Media.ReleaseName
	partOne := broadCandidate("part-one")
	partOne.Kind, partOne.Season = domain.MediaEpisode, 1
	partOne.Pack = &domain.PackInfo{Scope: domain.PackSeason, Season: 1}
	partTwo := broadCandidate("part-two")
	partTwo.Kind, partTwo.Season = domain.MediaEpisode, 2
	partTwo.Pack = &domain.PackInfo{Scope: domain.PackSeason, Season: 2}
	searcher := &fakeSearcher{broadBySeason: map[int]provider.SearchResult{
		1: {Candidates: []domain.Candidate{partOne}},
		2: {Candidates: []domain.Candidate{partTwo}},
	}}
	adapter := &fakeProvider{id: "provider", payloads: map[string][]byte{
		"part-one": workflowZIP(t, map[string]string{
			"Movie.S01E01.WEB.srt": strings.ReplaceAll(installSRT, "Hello", "part-one-episode-one"),
			"Movie.S01E02.WEB.srt": strings.ReplaceAll(installSRT, "Hello", "part-one-episode-two"),
			"Movie.S01E03.WEB.srt": strings.ReplaceAll(installSRT, "Hello", "part-one-episode-three"),
		}),
		"part-two": workflowZIP(t, map[string]string{
			"Movie.S02E01.WEB.srt": strings.ReplaceAll(installSRT, "Hello", "part-two-episode-one"),
			"Movie.S02E02.WEB.srt": strings.ReplaceAll(installSRT, "Hello", "part-two-episode-two"),
			"Movie.S02E03.WEB.srt": strings.ReplaceAll(installSRT, "Hello", "part-two-episode-three"),
		}),
	}}
	synchronizer := &fakeSynchronizer{}
	installer := &versionInstaller{}
	cache := &fakePackCache{}
	service := testService(t, inventory.Inventory{}, searcher, cache, synchronizer, installer)
	service.Providers["provider"] = adapter
	service.LapsePolicy = policy
	return service, request, searcher, synchronizer, installer, cache
}

func TestSplitSeasonEpisodeInstallsFromTheReleaseSeasonPack(t *testing.T) {
	service, request, searcher, synchronizer, installer, cache := splitSeasonService(t, DefaultLapsePolicy())
	result, err := service.Run(context.Background(), request)
	if err != nil || result.Outcome != OutcomeInstalled || len(installer.payloads) != 1 || !strings.Contains(installer.payloads[0], "part-two-episode-two") {
		t.Fatalf("result=%+v err=%v installs=%q", result, err, installer.payloads)
	}
	if synchronizer.synchronizeCalls == 0 {
		t.Fatal("a release-numbered candidate was installed without LAPSE")
	}
	var releaseQuery bool
	for _, query := range searcher.queries {
		if query.Mode == provider.SearchBroad && query.Media.Season == 2 && query.Media.Episode == 2 {
			releaseQuery = true
			if query.Media.Fingerprint != request.Media.Fingerprint || query.Media.ExternalIDs != request.Media.ExternalIDs {
				t.Fatalf("release query changed the media identity: %+v", query.Media)
			}
		}
	}
	if !releaseQuery {
		t.Fatalf("no broad search in the release's numbering: %+v", searcher.queries)
	}
	if !result.Candidate.ReleaseNumbered || result.Candidate.ResultID != "part-two" {
		t.Fatalf("installed candidate = %+v", result.Candidate)
	}
	if cache.puts != 0 {
		t.Fatalf("a release-numbered pack was published to the pack cache (%d puts)", cache.puts)
	}
}

// Under sync.policy never nothing checks a release-numbered candidate's
// timing, so it is rejected instead of installed.
func TestSplitSeasonCandidateIsRejectedWithoutLapse(t *testing.T) {
	service, request, _, synchronizer, installer, _ := splitSeasonService(t, neverSyncPolicy())
	repository := service.Repository.(*workflowRepository)
	result, err := service.Run(context.Background(), request)
	if err != nil || result.Outcome == OutcomeInstalled || len(installer.payloads) != 0 || synchronizer.synchronizeCalls != 0 {
		t.Fatalf("result=%+v err=%v installs=%q synchronize=%d", result, err, installer.payloads, synchronizer.synchronizeCalls)
	}
	var rejectedPartTwo bool
	for _, rejection := range repository.rejections {
		rejectedPartTwo = rejectedPartTwo || rejection.ResultID == "part-two"
	}
	if !rejectedPartTwo {
		t.Fatalf("rejections = %+v", repository.rejections)
	}
}

func TestEpisodeWithoutAnotherReleaseSeasonSearchesOnce(t *testing.T) {
	service, request, searcher, _, _, _ := splitSeasonService(t, DefaultLapsePolicy())
	request.Media.ReleaseName = "Movie.S01E10.WEB-GROUP"
	request.Media.OriginalFilename = request.Media.ReleaseName
	if _, err := service.Run(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	for _, query := range searcher.queries {
		if query.Media.Season != 1 || query.Media.Episode != 10 {
			t.Fatalf("searched outside Arr's numbering: %+v", query.Media)
		}
	}
}
