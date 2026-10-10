package workflow

import (
	"slices"
	"testing"

	"subsyncd/internal/domain"
	"subsyncd/internal/inventory"
	"subsyncd/internal/provider"
)

func earlyAcceptRun(t *testing.T, sync *fakeSynchronizer) Result {
	t.Helper()
	searcher := &fakeSearcher{results: map[provider.SearchMode]provider.SearchResult{
		provider.SearchExactHash: {},
		provider.SearchBroad:     {Candidates: []domain.Candidate{broadCandidate("first"), broadCandidate("second"), broadCandidate("third")}},
	}}
	service := testService(t, inventory.Inventory{}, searcher, nil, sync, &fakeInstaller{})
	service.Providers = map[string]provider.Provider{"provider": &fakeProvider{id: "provider"}}
	service.LapsePolicy.Mode = "always"
	result, err := service.Run(t.Context(), serviceRequest(t))
	if err != nil || result.Outcome != OutcomeInstalled {
		t.Fatalf("Run() = %s, %v; want installed", result.Outcome, err)
	}
	return result
}

func TestExcellentLapseResultSkipsTheRestOfItsTier(t *testing.T) {
	sync := &fakeSynchronizer{confidence: map[string]float64{"first": 0.985, "second": 0.79, "third": 0.78}, fullAgreement: true}
	result := earlyAcceptRun(t, sync)
	if !slices.Equal(sync.synchronized, []string{"first"}) || result.Candidate.ResultID != "first" {
		t.Fatalf("synchronized %v, installed %q; want only first checked and installed", sync.synchronized, result.Candidate.ResultID)
	}
}

func TestGoodButNotExcellentResultStillComparesTheTier(t *testing.T) {
	sync := &fakeSynchronizer{confidence: map[string]float64{"first": 0.8, "second": 0.95, "third": 0.7}, fullAgreement: true}
	result := earlyAcceptRun(t, sync)
	if len(sync.synchronized) < 2 || result.Candidate.ResultID != "second" {
		t.Fatalf("synchronized %v, installed %q; want the tier compared and second installed", sync.synchronized, result.Candidate.ResultID)
	}
}

func TestHighConfidenceWithoutFullAgreementStillComparesTheTier(t *testing.T) {
	sync := &fakeSynchronizer{confidence: map[string]float64{"first": 0.97, "second": 0.6, "third": 0.6}}
	earlyAcceptRun(t, sync)
	if len(sync.synchronized) != 3 {
		t.Fatalf("synchronized %v; want all three checked without full agreement and coverage", sync.synchronized)
	}
}
