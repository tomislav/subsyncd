package workflow

import (
	"context"
	"errors"
	"os"
	"strings"
	"subsyncd/internal/domain"
	"subsyncd/internal/inventory"
	"subsyncd/internal/provider"
	"testing"
)

type removingSearcher struct {
	path string
	t    *testing.T
}

func (s removingSearcher) Search(_ context.Context, q provider.SearchQuery) provider.SearchResult {
	if err := os.Remove(s.path); err != nil && !os.IsNotExist(err) {
		s.t.Fatal(err)
	}
	return provider.SearchResult{Candidates: []domain.Candidate{broadCandidate("one"), broadCandidate("two")}}
}
func TestMediaDisappearingDuringSearchStopsAcquisition(t *testing.T) {
	request := serviceRequest(t)
	syncer := &fakeSynchronizer{}
	adapter := &fakeProvider{id: "provider"}
	repo := &workflowRepository{}
	service := testService(t, inventory.Inventory{}, &fakeSearcher{}, nil, syncer, &fakeInstaller{})
	service.Searcher = removingSearcher{request.Media.Fingerprint.Path, t}
	service.Repository = repo
	service.Providers = map[string]provider.Provider{"provider": adapter}
	_, err := service.Run(context.Background(), request)
	if err == nil || !strings.Contains(err.Error(), "media file is missing") {
		t.Fatalf("error=%v", err)
	}
	if adapter.downloads != 0 || syncer.synchronizeCalls != 0 || len(repo.rejections) != 0 {
		t.Fatalf("downloads=%d lapse=%d rejections=%d", adapter.downloads, syncer.synchronizeCalls, len(repo.rejections))
	}
}

func TestMediaDisappearingDuringLapseStopsLaterCandidates(t *testing.T) {
	request := serviceRequest(t)
	synchronizer := &fakeSynchronizer{synchronizeErr: errors.New("LAPSE exited with code 1"), synchronizeHook: func(domain.Candidate) {
		if err := os.Remove(request.Media.Fingerprint.Path); err != nil {
			t.Fatal(err)
		}
	}}
	adapter := &fakeProvider{id: "provider"}
	repo := &workflowRepository{}
	searcher := &fakeSearcher{result: provider.SearchResult{Candidates: []domain.Candidate{broadCandidate("one"), broadCandidate("two")}}}
	service := testService(t, inventory.Inventory{}, searcher, nil, synchronizer, &fakeInstaller{})
	service.Repository = repo
	service.Providers = map[string]provider.Provider{"provider": adapter}
	fallback := &fakeSearcher{}
	service.FallbackSearcher = fallback
	service.FallbackProviderOrder = []string{"fallback"}
	_, err := service.Run(context.Background(), request)
	if err == nil || !strings.Contains(err.Error(), "media file is missing") || strings.Contains(err.Error(), request.Media.Fingerprint.Path) {
		t.Fatalf("error=%v", err)
	}
	if adapter.downloads != 1 || synchronizer.synchronizeCalls != 1 || len(repo.rejections) != 0 || fallback.calls != 0 {
		t.Fatalf("downloads=%d lapse=%d rejections=%d fallback=%d", adapter.downloads, synchronizer.synchronizeCalls, len(repo.rejections), fallback.calls)
	}
}
