package workflow

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"subsyncd/internal/domain"
	"subsyncd/internal/inventory"
	"subsyncd/internal/provider"
	"subsyncd/internal/store"
	"subsyncd/internal/syncer"
)

func TestInvalidLapseOutputIsRememberedAcrossRestart(t *testing.T) {
	for _, kind := range []domain.MediaKind{domain.MediaMovie, domain.MediaEpisode} {
		t.Run(string(kind), func(t *testing.T) {
			ctx := context.Background()
			request := serviceRequest(t)
			request.Media.EntityID = 1
			request.Media.Ref.Kind = kind
			candidate := broadCandidate("bad")
			candidate.Kind = kind
			if kind == domain.MediaEpisode {
				request.Media.Ref.Instance = "sonarr-main"
				request.Media.Season, request.Media.Episode = 1, 2
				candidate.Season, candidate.Episode = 1, 2
			}
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
			searcher := &fakeSearcher{result: provider.SearchResult{Candidates: []domain.Candidate{candidate}}}
			synchronizer := &fakeSynchronizer{synchronizeErrors: map[string]error{"bad": &syncer.InvalidOutputError{}}}
			service := testService(t, inventory.Inventory{}, searcher, nil, synchronizer, &fakeInstaller{})
			service.Repository = db.Repository()
			service.Providers = map[string]provider.Provider{"provider": adapter}
			result, err := service.Run(ctx, request)
			if err != nil || result.Outcome != OutcomeRejected {
				t.Fatalf("invalid output not rejected: %+v %v", result, err)
			}
			records, err := db.Repository().ListCandidateRejections(ctx, request.MediaID, request.Language, service.Clock.Now())
			if err != nil || len(records) != 1 || records[0].ReasonCode != "lapse_invalid_output" || records[0].ArtifactChecksum == "" {
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
			service.Clock = fixedWorkflowClock{at: service.Clock.Now().AddDate(10, 0, 0)}
			result, err = service.Run(ctx, request)
			if err != nil || adapter.downloads != 1 || synchronizer.synchronizeCalls != 1 {
				t.Fatalf("bad output repeated: %v downloads=%d lapse=%d", err, adapter.downloads, synchronizer.synchronizeCalls)
			}
			good := candidate
			good.ResultID = "good"
			searcher.result.Candidates = append(searcher.result.Candidates, good)
			result, err = service.Run(ctx, request)
			if err != nil || result.Outcome != OutcomeInstalled || result.Candidate.ResultID != "good" {
				t.Fatalf("alternative blocked: %+v %v", result, err)
			}
		})
	}
}

func TestTechnicalLapseFailuresNeverBecomeRejections(t *testing.T) {
	for _, failure := range []error{errors.Join(&syncer.InvalidOutputError{}, context.Canceled), errors.Join(&syncer.InvalidOutputError{}, errors.New("cleanup failed")), context.Canceled, context.DeadlineExceeded, errors.New("LAPSE process failed"), errors.New("LAPSE returned invalid JSON protocol data"), errors.New("LAPSE output is missing or not a regular file"), &syncer.NoSpeechError{}} {
		service := testService(t, inventory.Inventory{}, &fakeSearcher{}, nil, &fakeSynchronizer{}, &fakeInstaller{})
		recorded, err := service.recordCandidateRejection(context.Background(), serviceRequest(t), broadCandidate("candidate"), "checksum", failure)
		if err != nil || recorded {
			t.Fatalf("technical failure recorded: %v %v", failure, err)
		}
	}
}
