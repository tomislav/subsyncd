package workflow

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"subsyncd/internal/domain"
	"subsyncd/internal/inventory"
	"subsyncd/internal/observability"
	"subsyncd/internal/provider"
	"subsyncd/internal/store"
	"subsyncd/internal/syncer"
)

func TestTooFewCuesIsRememberedAcrossRestart(t *testing.T) {
	ctx := context.Background()
	request := serviceRequest(t)
	request.Media.EntityID = 1
	path := filepath.Join(t.TempDir(), "db.sqlite")
	db, err := store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	request.MediaID, _, err = db.Repository().UpsertMedia(ctx, request.Media)
	if err != nil {
		t.Fatal(err)
	}
	adapter := &fakeProvider{id: "provider"}
	searcher := &fakeSearcher{result: provider.SearchResult{Candidates: []domain.Candidate{broadCandidate("few")}}}
	synchronizer := &fakeSynchronizer{synchronizeErrors: map[string]error{"few": &syncer.TooFewCuesError{Cues: 6, IgnoredCues: 2}}}
	service := testService(t, inventory.Inventory{}, searcher, nil, synchronizer, &fakeInstaller{})
	service.Repository = db.Repository()
	service.Providers = map[string]provider.Provider{"provider": adapter}
	result, err := service.Run(ctx, request)
	if err != nil || result.Outcome != OutcomeRejected {
		t.Fatalf("too few cues not rejected: %+v %v", result, err)
	}
	records, err := db.Repository().ListCandidateRejections(ctx, request.MediaID, request.Language, service.Clock.Now())
	if err != nil || len(records) != 1 || records[0].ReasonCode != "lapse_too_few_cues" || records[0].ArtifactChecksum == "" {
		t.Fatalf("rejection=%+v %v", records, err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	service.Repository = db.Repository()
	if _, err := service.Run(ctx, request); err != nil || adapter.downloads != 1 || synchronizer.synchronizeCalls != 1 {
		t.Fatalf("too few cues retried: %v downloads=%d lapse=%d", err, adapter.downloads, synchronizer.synchronizeCalls)
	}
}

func TestTooFewCuesMovesOnToTheNextCandidate(t *testing.T) {
	ctx := context.Background()
	request := serviceRequest(t)
	searcher := &fakeSearcher{result: provider.SearchResult{Candidates: []domain.Candidate{broadCandidate("few"), broadCandidate("good")}}}
	synchronizer := &fakeSynchronizer{synchronizeErrors: map[string]error{"few": &syncer.TooFewCuesError{Cues: 6}}}
	service := testService(t, inventory.Inventory{}, searcher, nil, synchronizer, &fakeInstaller{})
	service.Providers = map[string]provider.Provider{"provider": &fakeProvider{id: "provider"}}
	result, err := service.Run(ctx, request)
	if err != nil || result.Outcome != OutcomeInstalled || result.Candidate.ResultID != "good" {
		t.Fatalf("next candidate not installed: %+v %v", result, err)
	}
	rejections := service.Repository.(*workflowRepository).rejections
	if len(rejections) != 1 || rejections[0].ResultID != "few" || rejections[0].ReasonCode != "lapse_too_few_cues" {
		t.Fatalf("rejections = %+v", rejections)
	}
}

// LAPSE refuses before it reads the media, so unreadable media does not turn
// the rejection into a media-unavailable deferral.
func TestTooFewCuesRejectsWithoutReadingMedia(t *testing.T) {
	ctx := context.Background()
	request := serviceRequest(t)
	service := testService(t, inventory.Inventory{}, &fakeSearcher{}, nil, &fakeSynchronizer{}, &fakeInstaller{})
	if err := os.Remove(request.Media.Fingerprint.Path); err != nil {
		t.Fatal(err)
	}
	recorded, err := service.recordCandidateRejection(ctx, request, broadCandidate("few"), "checksum", &syncer.TooFewCuesError{Cues: 6})
	if !recorded || err != nil {
		t.Fatalf("recorded=%t err=%v, want a rejection", recorded, err)
	}
}

func TestTooFewCuesIsLoggedAsAWarning(t *testing.T) {
	request := serviceRequest(t)
	var logs bytes.Buffer
	events, err := observability.New(&logs, observability.Options{Level: "info", Version: "test", MediaRoots: []string{filepath.Dir(request.Media.Fingerprint.Path)}})
	if err != nil {
		t.Fatal(err)
	}
	searcher := &fakeSearcher{result: provider.SearchResult{Candidates: []domain.Candidate{broadCandidate("few")}}}
	service := testService(t, inventory.Inventory{}, searcher, nil, &fakeSynchronizer{synchronizeErr: &syncer.TooFewCuesError{Cues: 6}}, &fakeInstaller{})
	service.Providers = map[string]provider.Provider{"provider": &fakeProvider{id: "provider"}}
	service.Events = events
	if _, err := service.Run(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	records := workflowLogRecords(t, logs.String())
	failed := workflowEvents(records, "lapse.failed")
	if len(failed) != 1 || failed[0]["level"] != "warn" || failed[0]["verdict"] != "nothing" || failed[0]["reference"] != "none" {
		t.Fatalf("lapse.failed = %#v", failed)
	}
	rejected := workflowEvents(records, "candidate.rejected")
	if len(rejected) != 1 || rejected[0]["reason_code"] != "lapse_too_few_cues" {
		t.Fatalf("candidate.rejected = %#v", rejected)
	}

	logs.Reset()
	if _, err := service.Run(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	skipped := workflowEvents(workflowLogRecords(t, logs.String()), "candidate.skipped")
	if len(skipped) != 1 || skipped[0]["reason_code"] != "lapse_too_few_cues" {
		t.Fatalf("candidate.skipped = %#v", skipped)
	}
}
