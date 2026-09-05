package workflow

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"subsyncd/internal/domain"
	"subsyncd/internal/inventory"
	"subsyncd/internal/observability"
	"subsyncd/internal/provider"
)

func TestWorkflowLogsSatisfiedInventoryWithoutPaths(t *testing.T) {
	request := serviceRequest(t)
	request.Media.Ref.Kind = domain.MediaEpisode
	request.Media.Title = "Show\nName"
	request.Media.Year = 0
	request.Media.Season = 1
	request.Media.Episode = 2
	request.Media.EpisodeTitle = "Pilot\tPart"
	var logs bytes.Buffer
	events, err := observability.New(&logs, observability.Options{Level: "info", Version: "test", MediaRoots: []string{filepath.Dir(request.Media.Fingerprint.Path)}})
	if err != nil {
		t.Fatal(err)
	}
	service := testService(t, inventory.Inventory{Tracks: []inventory.Track{{Language: "en", Embedded: true}}}, &fakeSearcher{}, nil, nil, nil)
	service.Events = events

	result, err := service.Run(context.Background(), request)
	if err != nil || result.Outcome != OutcomeSatisfied {
		t.Fatalf("Run() = %#v, %v", result, err)
	}
	records := workflowLogRecords(t, logs.String())
	if len(workflowEvents(records, "search.started")) != 1 || len(workflowEvents(records, "search.completed")) != 1 {
		t.Fatalf("workflow lifecycle = %s", logs.String())
	}
	inventoryEvents := workflowEvents(records, "inventory.refresh_completed")
	if len(inventoryEvents) != 1 || inventoryEvents[0]["track_count"] != float64(1) || inventoryEvents[0]["embedded_count"] != float64(1) || inventoryEvents[0]["satisfied"] != true {
		t.Fatalf("inventory event = %#v", inventoryEvents)
	}
	completed := workflowEvents(records, "search.completed")[0]
	if completed["outcome"] != "satisfied" || completed["reason"] != "existing_subtitle" {
		t.Fatalf("completion = %#v", completed)
	}
	for _, event := range []string{"search.started", "search.completed"} {
		matches := workflowEvents(records, event)
		if len(matches) != 1 || matches[0]["media_title"] != "Show Name - S01E02 - Pilot Part" {
			t.Fatalf("%s media title = %#v", event, matches)
		}
	}
	if strings.Contains(logs.String(), request.Media.Fingerprint.Path) {
		t.Fatalf("info logs leaked media path: %s", logs.String())
	}
}

func TestWorkflowLogsExactSelectionAndCommittedInstallWithoutLapse(t *testing.T) {
	request := serviceRequest(t)
	var logs bytes.Buffer
	events, err := observability.New(&logs, observability.Options{Level: "info", Version: "test", MediaRoots: []string{filepath.Dir(request.Media.Fingerprint.Path)}})
	if err != nil {
		t.Fatal(err)
	}
	candidate := exactCandidate("exact-secret")
	candidate.ReleaseNames = []string{"Secret.Release.Name.2024"}
	providerFake := &fakeProvider{id: "provider"}
	service := testService(t, inventory.Inventory{}, &fakeSearcher{result: provider.SearchResult{Candidates: []domain.Candidate{candidate}}}, nil, &fakeSynchronizer{}, &fakeInstaller{})
	service.Providers = map[string]provider.Provider{"provider": providerFake}
	service.Events = events

	result, err := service.Run(context.Background(), request)
	if err != nil || result.Outcome != OutcomeInstalled {
		t.Fatalf("Run() = %#v, %v", result, err)
	}
	selected := workflowEvents(workflowLogRecords(t, logs.String()), "candidate.selected")
	if len(selected) != 1 || selected[0]["selection_mode"] != "exact_hash" || selected[0]["candidate_id"] != "exact-secret" || selected[0]["exact_hash"] != true {
		t.Fatalf("selected = %#v", selected)
	}
	if len(workflowEvents(workflowLogRecords(t, logs.String()), "subtitle.installed")) != 1 {
		t.Fatalf("install event missing: %s", logs.String())
	}
	if strings.Contains(logs.String(), "Secret.Release.Name") || strings.Contains(logs.String(), request.Media.Fingerprint.Path) || len(workflowEvents(workflowLogRecords(t, logs.String()), "lapse.analysis_completed")) != 0 {
		t.Fatalf("exact-hash info logs leaked detail or invoked LAPSE: %s", logs.String())
	}
}

func TestWorkflowLogsDebugScoringAndTypedLapsePhases(t *testing.T) {
	request := serviceRequest(t)
	var logs bytes.Buffer
	events, err := observability.New(&logs, observability.Options{Level: "debug", Version: "test", MediaRoots: []string{filepath.Dir(request.Media.Fingerprint.Path)}})
	if err != nil {
		t.Fatal(err)
	}
	candidate := broadCandidate("broad")
	candidate.ReleaseNames = []string{"Movie.2024.WEB-DL-GROUP"}
	providerFake := &fakeProvider{id: "provider"}
	service := testService(t, inventory.Inventory{}, &fakeSearcher{result: provider.SearchResult{Candidates: []domain.Candidate{candidate}}}, nil, &fakeSynchronizer{}, &fakeInstaller{})
	service.Providers = map[string]provider.Provider{"provider": providerFake}
	service.Events = events

	result, err := service.Run(context.Background(), request)
	if err != nil || result.Outcome != OutcomeInstalled {
		t.Fatalf("Run() = %#v, %v", result, err)
	}
	records := workflowLogRecords(t, logs.String())
	evaluated := workflowEvents(records, "candidate.evaluated")
	if len(evaluated) != 1 || evaluated[0]["provider"] != "provider" || evaluated[0]["candidate_id"] != "broad" || evaluated[0]["eligible"] != true || evaluated[0]["relative_path"] != "Movie.mkv" {
		t.Fatalf("evaluated = %#v", evaluated)
	}
	for _, event := range []string{"lapse.analysis_completed", "lapse.sync_completed"} {
		matches := workflowEvents(records, event)
		if len(matches) != 1 || matches[0]["verdict"] != "solid" || matches[0]["ratio"] != float64(1) || matches[0]["parts"] != float64(1) || matches[0]["compatibility_version"] == "" {
			t.Fatalf("%s = %#v", event, matches)
		}
	}
	if len(workflowEvents(records, "candidate.tier_started")) != 1 || len(workflowEvents(records, "candidate.early_stopped")) != 1 {
		t.Fatalf("tournament events missing: %s", logs.String())
	}
}

func TestWorkflowLogsBoundedArchiveSelectionDiagnostics(t *testing.T) {
	request := serviceRequest(t)
	request.Media.Ref.Kind = domain.MediaEpisode
	request.Media.Title = "Ozark"
	request.Media.Season = 3
	request.Media.Episode = 1
	candidate := broadCandidate("306201")
	candidate.Kind = domain.MediaEpisode
	candidate.Title = "Ozark"
	candidate.Season = 3
	candidate.Episode = 1
	providerFake := &fakeProvider{
		id: "provider",
		payloads: map[string][]byte{"306201": workflowZIP(t, map[string]string{
			"Ozark.S03E03.iNTERNAL.1080p.WEB.x264-GHOSTS.cyr_utf8.srt": installSRT,
			"Ozark.S03E03.iNTERNAL.1080p.WEB.x264-GHOSTS.srt":          installSRT,
		})},
		filenames: map[string]string{"306201": "306201.zip"},
	}
	var logs bytes.Buffer
	events, err := observability.New(&logs, observability.Options{Level: "debug", Version: "test", MediaRoots: []string{filepath.Dir(request.Media.Fingerprint.Path)}})
	if err != nil {
		t.Fatal(err)
	}
	service := testService(t, inventory.Inventory{}, &fakeSearcher{result: provider.SearchResult{Candidates: []domain.Candidate{candidate}}}, nil, &fakeSynchronizer{}, &fakeInstaller{})
	service.Providers = map[string]provider.Provider{"provider": providerFake}
	service.Events = events

	result, err := service.Run(context.Background(), request)
	if err != nil || result.Outcome != OutcomeRejected {
		t.Fatalf("Run() = %#v, %v", result, err)
	}
	rejected := workflowEvents(workflowLogRecords(t, logs.String()), "candidate.rejected")
	if len(rejected) != 1 || rejected[0]["provider"] != "provider" || rejected[0]["candidate_id"] != "306201" || rejected[0]["reason_code"] != "pack_selection" || rejected[0]["selection_rule"] != "none" || rejected[0]["archive_type"] != "zip" || rejected[0]["subtitle_member_count"] != float64(2) || rejected[0]["matching_member_count"] != float64(0) {
		t.Fatalf("candidate rejection event = %#v; logs=%s", rejected, logs.String())
	}
	if strings.Contains(logs.String(), "Ozark.S03E03") || strings.Contains(logs.String(), request.Media.Fingerprint.Path) {
		t.Fatalf("archive diagnostics leaked filenames or absolute paths: %s", logs.String())
	}
}

func TestWorkflowLogsLapseFailureOnceAndSanitizesIt(t *testing.T) {
	request := serviceRequest(t)
	var logs bytes.Buffer
	events, err := observability.New(&logs, observability.Options{Level: "info", Version: "test", MediaRoots: []string{filepath.Dir(request.Media.Fingerprint.Path)}, Redact: func(error) string { return "LAPSE unavailable" }})
	if err != nil {
		t.Fatal(err)
	}
	candidate := broadCandidate("failure")
	providerFake := &fakeProvider{id: "provider"}
	service := testService(t, inventory.Inventory{}, &fakeSearcher{result: provider.SearchResult{Candidates: []domain.Candidate{candidate}}}, nil, &fakeSynchronizer{analyzeErr: errors.New("secret output at " + request.Media.Fingerprint.Path)}, &fakeInstaller{})
	service.Providers = map[string]provider.Provider{"provider": providerFake}
	service.Events = events

	if _, err := service.Run(context.Background(), request); err == nil {
		t.Fatal("Run() succeeded, want technical failure")
	}
	records := workflowLogRecords(t, logs.String())
	failed := workflowEvents(records, "lapse.failed")
	completed := workflowEvents(records, "search.completed")
	if len(failed) != 1 || failed[0]["phase"] != "analysis" || failed[0]["error"] != "LAPSE unavailable" {
		t.Fatalf("LAPSE failure = %#v", failed)
	}
	if len(completed) != 1 || completed[0]["outcome"] != "failed" || len(workflowEvents(records, "search.failed")) != 0 {
		t.Fatalf("terminal events = %s", logs.String())
	}
	if strings.Contains(logs.String(), "secret output") || strings.Contains(logs.String(), request.Media.Fingerprint.Path) {
		t.Fatalf("failure logs leaked sensitive text: %s", logs.String())
	}
}

func TestWorkflowLogsCommittedProvenanceRefresh(t *testing.T) {
	request := serviceRequest(t)
	candidate := broadCandidate("same")
	repository := &workflowRepository{found: true, installation: matchingInstallation(request, candidate, []byte(`{"total":20}`))}
	var logs bytes.Buffer
	events, err := observability.New(&logs, observability.Options{Level: "info", Version: "test", MediaRoots: []string{filepath.Dir(request.Media.Fingerprint.Path)}})
	if err != nil {
		t.Fatal(err)
	}
	service := testService(t, inventory.Inventory{}, &fakeSearcher{result: provider.SearchResult{Candidates: []domain.Candidate{candidate}}}, nil, &fakeSynchronizer{}, &fakeInstaller{})
	service.Repository = repository
	service.Providers = map[string]provider.Provider{"provider": &fakeProvider{id: "provider"}}
	service.Events = events

	result, err := service.Run(context.Background(), request)
	if err != nil || result.Outcome != OutcomeSatisfied {
		t.Fatalf("Run() = %#v, %v", result, err)
	}
	records := workflowLogRecords(t, logs.String())
	if len(workflowEvents(records, "subtitle.provenance_refreshed")) != 1 || len(workflowEvents(records, "upgrade.scheduled")) != 1 {
		t.Fatalf("provenance/upgrade events missing: %s", logs.String())
	}
}

func workflowLogRecords(t *testing.T, output string) []map[string]any {
	t.Helper()
	var records []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		if line == "" {
			continue
		}
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("decode workflow log %q: %v", line, err)
		}
		records = append(records, record)
	}
	return records
}

func workflowEvents(records []map[string]any, event string) []map[string]any {
	var matches []map[string]any
	for _, record := range records {
		if record["event"] == event {
			matches = append(matches, record)
		}
	}
	return matches
}
