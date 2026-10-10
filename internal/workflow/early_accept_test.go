package workflow

import (
	"bytes"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"subsyncd/internal/domain"
	"subsyncd/internal/inventory"
	"subsyncd/internal/observability"
	"subsyncd/internal/provider"
)

func earlyAcceptRun(t *testing.T, sync *fakeSynchronizer) Result {
	t.Helper()
	result, _ := earlyAcceptRunLogged(t, sync)
	return result
}

func earlyAcceptRunLogged(t *testing.T, sync *fakeSynchronizer) (Result, string) {
	t.Helper()
	var logs bytes.Buffer
	events, err := observability.New(&logs, observability.Options{Level: "info", Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	searcher := &fakeSearcher{results: map[provider.SearchMode]provider.SearchResult{
		provider.SearchExactHash: {},
		provider.SearchBroad:     {Candidates: []domain.Candidate{broadCandidate("first"), broadCandidate("second"), broadCandidate("third")}},
	}}
	service := testService(t, inventory.Inventory{}, searcher, nil, sync, &fakeInstaller{})
	service.Providers = map[string]provider.Provider{"provider": &fakeProvider{id: "provider"}}
	service.LapsePolicy.Mode = "always"
	service.Events = events
	result, err := service.Run(t.Context(), serviceRequest(t))
	if err != nil || result.Outcome != OutcomeInstalled {
		t.Fatalf("Run() = %s, %v; want installed", result.Outcome, err)
	}
	return result, logs.String()
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

func TestEarlyAcceptLogsConfidenceAndSkippedCount(t *testing.T) {
	sync := &fakeSynchronizer{confidence: map[string]float64{"first": 0.985, "second": 0.79, "third": 0.78}, fullAgreement: true}
	_, logs := earlyAcceptRunLogged(t, sync)
	for _, line := range strings.Split(logs, "\n") {
		if !strings.Contains(line, `"candidate.early_accepted"`) {
			continue
		}
		var record struct {
			Level        string  `json:"level"`
			Confidence   float64 `json:"confidence"`
			SkippedCount int     `json:"skipped_count"`
			CandidateID  string  `json:"candidate_id"`
		}
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatal(err)
		}
		if record.Level != "INFO" && record.Level != "info" || record.Confidence != 0.985 || record.SkippedCount != 2 || record.CandidateID != "first" {
			t.Fatalf("early-accept record = %+v; want info, confidence 0.985, skipped_count 2, candidate first", record)
		}
		return
	}
	t.Fatalf("no candidate.early_accepted record in logs:\n%s", logs)
}
