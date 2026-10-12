package workflow

import (
	"context"
	"testing"
	"time"

	"subsyncd/internal/domain"
	"subsyncd/internal/provider"
	"subsyncd/internal/store"
)

// removingInstaller records removals besides installs.
type removingInstaller struct {
	fakeInstaller
	removals []RemovalRequest
	err      error
}

func (r *removingInstaller) RemoveManaged(_ context.Context, removal RemovalRequest) error {
	r.removals = append(r.removals, removal)
	return r.err
}

// replaceFixture is a managed subtitle installed from an exact-hash candidate
// with a high score, which an ordinary search would keep.
func replaceFixture(t *testing.T, searcher *fakeSearcher) (*Service, *removingInstaller, Request, store.Installation) {
	t.Helper()
	request := serviceRequest(t)
	request.Manual = true
	request.ReplaceInstalled = true
	existing := matchingInstallation(request, exactCandidate("rejected"), []byte(`{"total":100,"contributions":[{"signal":"exact_hash","points":100}]}`))
	existing.Checksum = "managed"
	installer := &removingInstaller{}
	service := testService(t, managedSidecarInventory(existing), searcher, nil, &fakeSynchronizer{}, installer)
	service.Repository = &workflowRepository{found: true, installation: existing}
	service.Providers = map[string]provider.Provider{"provider": &fakeProvider{id: "provider"}}
	service.LapsePolicy.Mode = "never"
	return service, installer, request, existing
}

func TestReplaceInstalledInstallsLowerScoringCandidate(t *testing.T) {
	service, installer, request, existing := replaceFixture(t, &fakeSearcher{result: provider.SearchResult{Candidates: []domain.Candidate{broadCandidate("lower")}}})
	result, err := service.Run(context.Background(), request)
	if err != nil || result.Outcome != OutcomeInstalled || installer.calls != 1 || installer.request.Candidate.ResultID != "lower" {
		t.Fatalf("Run() = %#v, %v; installs=%d", result, err, installer.calls)
	}
	if installer.request.DestinationPath != existing.Path {
		t.Fatalf("replacement destination = %q, want the managed subtitle's %q", installer.request.DestinationPath, existing.Path)
	}
	if len(installer.removals) != 0 {
		t.Fatal("an installed replacement also removed the subtitle")
	}

	// Without ReplaceInstalled the exact-hash subtitle is kept.
	service, installer, request, _ = replaceFixture(t, &fakeSearcher{result: provider.SearchResult{Candidates: []domain.Candidate{broadCandidate("lower")}}})
	request.ReplaceInstalled = false
	result, err = service.Run(context.Background(), request)
	if err != nil || result.Outcome != OutcomeSatisfied || installer.calls != 0 {
		t.Fatalf("ordinary Run() = %#v, %v; installs=%d", result, err, installer.calls)
	}
}

func TestReplaceInstalledRemovesWhenNothingIsAcceptable(t *testing.T) {
	service, installer, request, existing := replaceFixture(t, &fakeSearcher{})
	result, err := service.Run(context.Background(), request)
	if err != nil || result.Outcome != OutcomeRemoved || result.RemovedAfter != OutcomeNoResult || installer.calls != 0 {
		t.Fatalf("Run() = %#v, %v", result, err)
	}
	if len(installer.removals) != 1 || installer.removals[0].Installation.Path != existing.Path || !installer.removals[0].Manual || installer.removals[0].Media.Ref != request.Media.Ref {
		t.Fatalf("removals = %#v", installer.removals)
	}
	if !result.NextUpgrade.IsZero() {
		t.Fatalf("a removal scheduled an upgrade: %v", result.NextUpgrade)
	}
}

func TestReplaceInstalledKeepsSubtitleWhenTheSearchDidNotFinish(t *testing.T) {
	reset := time.Date(2026, 9, 4, 15, 0, 0, 0, time.UTC)
	searcher := &fakeSearcher{result: provider.SearchResult{Errors: map[string]error{"provider": &provider.CooldownError{ProviderID: "provider", Scope: provider.OperationSearch, ResetAt: reset}}}}
	service, installer, request, _ := replaceFixture(t, searcher)
	result, err := service.Run(context.Background(), request)
	if err != nil || result.Outcome != OutcomeThrottled || len(installer.removals) != 0 {
		t.Fatalf("throttled Run() = %#v, %v; removals=%d", result, err, len(installer.removals))
	}

	searcher = &fakeSearcher{result: provider.SearchResult{Errors: map[string]error{"provider": context.DeadlineExceeded}}}
	service, installer, request, _ = replaceFixture(t, searcher)
	if _, err := service.Run(context.Background(), request); err == nil || len(installer.removals) != 0 {
		t.Fatalf("failed Run() error = %v; removals=%d", err, len(installer.removals))
	}
}

func TestReplaceInstalledLeavesAnEditedSubtitleAlone(t *testing.T) {
	service, installer, request, existing := replaceFixture(t, &fakeSearcher{})
	edited := managedSidecarInventory(existing)
	edited.Tracks[0].Checksum = "edited"
	service.Inventory = &fakeInventory{current: edited}
	result, err := service.Run(context.Background(), request)
	if err != nil || len(installer.removals) != 0 || result.Outcome == OutcomeRemoved {
		t.Fatalf("Run() = %#v, %v; removals=%d", result, err, len(installer.removals))
	}
}
