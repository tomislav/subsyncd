package workflow

import (
	"context"
	"strings"
	"testing"

	"subsyncd/internal/domain"
	"subsyncd/internal/inventory"
	"subsyncd/internal/provider"
)

func movieVersionsService(t *testing.T, candidate domain.Candidate) (*Service, *fakeProvider, *fakeSynchronizer, *versionInstaller) {
	t.Helper()
	adapter := &fakeProvider{id: "provider", payloads: map[string][]byte{candidate.ResultID: workflowZIP(t, map[string]string{
		"Movie.2024.DVDRip.XviD-ONE.srt":             strings.ReplaceAll(installSRT, "Hello", "dvd-version"),
		"Movie.2024.BDRip.x264-TWO.srt":              strings.ReplaceAll(installSRT, "Hello", "bd-version"),
		"Movie.2024.BDRip.x264-TWO - Commentary.srt": strings.ReplaceAll(installSRT, "Hello", "commentary"),
	})}}
	sync := &fakeSynchronizer{confidence: map[string]float64{"bd-version": 0.95, "dvd-version": 0.6, "commentary": 0.99}}
	installer := &versionInstaller{}
	service := testService(t, inventory.Inventory{}, &fakeSearcher{result: provider.SearchResult{Candidates: []domain.Candidate{candidate}}}, nil, sync, installer)
	service.Providers["provider"] = adapter
	return service, adapter, sync, installer
}

func TestMovieArchiveVersionsAreRankedByTiming(t *testing.T) {
	request := serviceRequest(t)
	candidate := broadCandidate("movie-versions")
	candidate.Kind = domain.MediaMovie
	service, adapter, sync, installer := movieVersionsService(t, candidate)
	result, err := service.Run(context.Background(), request)
	if err != nil || result.Outcome != OutcomeInstalled || sync.synchronizeCalls != 2 || adapter.downloads != 1 {
		t.Fatalf("result=%+v err=%v synchronize=%d downloads=%d", result, err, sync.synchronizeCalls, adapter.downloads)
	}
	if len(installer.payloads) != 1 || !strings.Contains(installer.payloads[0], "bd-version") {
		t.Fatalf("installed = %v; want the better-timed version, never the commentary", installer.payloads)
	}
}

func TestMovieArchiveVersionsNeverSkipLapseOnScore(t *testing.T) {
	request := serviceRequest(t)
	request.Media.ReleaseGroup = "GROUP"
	request.Media.Source = "WEB-DL"
	candidate := broadCandidate("strong-versions")
	candidate.Kind = domain.MediaMovie
	candidate.ReleaseNames = []string{"Movie.2024.1080p.WEB-DL-GROUP"}
	service, _, sync, installer := movieVersionsService(t, candidate)
	result, err := service.Run(context.Background(), request)
	if err != nil || result.Outcome != OutcomeInstalled || sync.synchronizeCalls != 2 {
		t.Fatalf("result=%+v err=%v synchronize=%d; versions must be ranked by LAPSE even for a strong release match", result, err, sync.synchronizeCalls)
	}
	if len(installer.payloads) != 1 || !strings.Contains(installer.payloads[0], "bd-version") {
		t.Fatalf("installed = %v", installer.payloads)
	}
}
