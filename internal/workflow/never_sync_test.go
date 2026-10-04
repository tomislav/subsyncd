package workflow

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"subsyncd/internal/domain"
	"subsyncd/internal/inventory"
	"subsyncd/internal/observability"
	"subsyncd/internal/provider"
	"testing"
)

func neverSyncPolicy() LapsePolicy {
	policy := DefaultLapsePolicy()
	policy.Mode = "never"
	return policy
}

func TestNeverSyncPolicyInstallsNonexactCandidateWithoutLapse(t *testing.T) {
	request := serviceRequest(t)
	searcher := &fakeSearcher{result: provider.SearchResult{Candidates: []domain.Candidate{broadCandidate("weak")}}}
	synchronizer := &fakeSynchronizer{}
	installer := &fakeInstaller{}
	service := testService(t, inventory.Inventory{}, searcher, nil, synchronizer, installer)
	service.Providers = map[string]provider.Provider{"provider": &fakeProvider{id: "provider"}}
	service.LapsePolicy = neverSyncPolicy()
	var logs bytes.Buffer
	service.Events, _ = observability.New(&logs, observability.Options{Level: "info", Version: "test"})

	result, err := service.Run(context.Background(), request)
	if err != nil || result.Outcome != OutcomeInstalled {
		t.Fatalf("Run() = %#v, %v", result, err)
	}
	if synchronizer.analyzeCalls != 0 || synchronizer.synchronizeCalls != 0 {
		t.Fatalf("never policy invoked LAPSE: analyze/synchronize=%d/%d", synchronizer.analyzeCalls, synchronizer.synchronizeCalls)
	}
	if got := installer.request.SyncResult; got.Verdict != "sync_disabled" || got.Mode != "bypass" || got.Reference != "policy" {
		t.Fatalf("sync provenance = %#v", got)
	}
	selected := workflowEvents(workflowLogRecords(t, logs.String()), "candidate.selected")
	if len(selected) != 1 || selected[0]["selection_mode"] != "sync_disabled" {
		t.Fatalf("selection events = %s", logs.String())
	}
}

func TestNeverSyncPolicyBypassesLapseForPacksAndUpgrades(t *testing.T) {
	request := serviceRequest(t)
	pack := broadCandidate("pack")
	pack.Pack = &domain.PackInfo{Scope: domain.PackSeason, Season: 1}
	existing := matchingInstallation(request, broadCandidate("old"), []byte(`{"total":20}`))
	tests := []struct {
		name      string
		item      downloadedCandidate
		installed bool
	}{
		{name: "provider pack", item: downloadedCandidate{candidate: pack}},
		{name: "runtime pack", item: downloadedCandidate{candidate: broadCandidate("runtime"), runtimePack: true}},
		{name: "upgrade", item: downloadedCandidate{candidate: broadCandidate("upgrade")}, installed: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			synchronizer := &fakeSynchronizer{}
			service := testService(t, inventory.Inventory{}, &fakeSearcher{}, nil, synchronizer, nil)
			service.MinimumUpgradeDelta = DefaultMinimumUpgradeDelta
			service.LapsePolicy = neverSyncPolicy()
			test.item.path = writeInstallFile(t, filepath.Join(t.TempDir(), "candidate.srt"), installSRT)
			test.item.score = domain.Score{Total: 35}
			prepared, err := service.prepareCandidate(context.Background(), request, test.item, test.installed, existing, t.TempDir(), 0)
			if err != nil {
				t.Fatal(err)
			}
			if synchronizer.synchronizeCalls != 0 || !prepared.bypass || prepared.output != test.item.path || prepared.sync.Verdict != "sync_disabled" {
				t.Fatalf("prepared=%+v synchronize=%d", prepared, synchronizer.synchronizeCalls)
			}
		})
	}
}

func TestNeverSyncPolicySkipsAmbiguousPackVersions(t *testing.T) {
	request := serviceRequest(t)
	request.Media.Ref.Kind = domain.MediaEpisode
	request.Media.Season, request.Media.Episode = 4, 13
	candidate := broadCandidate("versions")
	candidate.Kind = domain.MediaEpisode
	candidate.Season, candidate.Episode = 4, 13
	adapter := &fakeProvider{id: "provider", payloads: map[string][]byte{"versions": workflowZIP(t, map[string]string{
		"Movie.S04E13.WEB.srt": strings.ReplaceAll(installSRT, "Hello", "web-version"),
		"Movie.04x13.HDTV.srt": strings.ReplaceAll(installSRT, "Hello", "hdtv-version"),
	})}}
	synchronizer := &fakeSynchronizer{}
	installer := &versionInstaller{}
	service := testService(t, inventory.Inventory{}, &fakeSearcher{result: provider.SearchResult{Candidates: []domain.Candidate{candidate}}}, nil, synchronizer, installer)
	service.Providers["provider"] = adapter
	service.LapsePolicy = neverSyncPolicy()
	repository := service.Repository.(*workflowRepository)

	result, err := service.Run(context.Background(), request)
	if err != nil || result.Outcome == OutcomeInstalled || len(installer.payloads) != 0 || synchronizer.synchronizeCalls != 0 {
		t.Fatalf("result=%+v err=%v installs=%d synchronize=%d", result, err, len(installer.payloads), synchronizer.synchronizeCalls)
	}
	if len(repository.rejections) != 1 || repository.rejections[0].ReasonCode != "pack_selection" || repository.rejections[0].ResultID != "versions" {
		t.Fatalf("rejections = %+v", repository.rejections)
	}
}
