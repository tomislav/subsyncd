package workflow

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"subsyncd/internal/domain"
	"subsyncd/internal/store"
	"subsyncd/internal/syncer"
)

func TestLapseTimeoutStopsAcquisitionWithoutTryingOtherCandidates(t *testing.T) {
	ctx := t.Context()
	request := serviceRequest(t)
	request.Media.EntityID = 1
	request.Media.ReleaseGroup = "GROUP"
	slow := broadCandidate("slow")
	slow.ReleaseNames = []string{"Movie.2024-GROUP"}
	good := broadCandidate("good")
	timeout := &syncer.TimeoutError{Limit: 80 * time.Minute}
	synchronizer := &fakeSynchronizer{synchronizeErrors: map[string]error{"slow": timeout}}
	database, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	request.MediaID, _, err = database.Repository().UpsertMedia(ctx, request.Media)
	if err != nil {
		t.Fatal(err)
	}
	service := repeatedLapseTestService(t, request, database.Repository(), synchronizer, &fakeProvider{id: "provider"}, []domain.Candidate{slow, good})

	result, err := service.Run(ctx, request)
	var got *syncer.TimeoutError
	if !errors.As(err, &got) {
		t.Fatalf("Run() = %+v, %T %v, want the LAPSE timeout", result, err, err)
	}
	var exhausted *acquisitionExhaustedError
	if errors.As(err, &exhausted) {
		t.Fatalf("Run() error = %v, want a terminal timeout rather than exhausted acquisition", err)
	}
	if !slices.Equal(synchronizer.synchronized, []string{"slow"}) {
		t.Fatalf("synchronized = %v, want only the candidate that timed out", synchronizer.synchronized)
	}
	if rejections, listErr := database.Repository().ListCandidateRejections(ctx, request.MediaID, request.Language, service.Clock.Now()); listErr != nil || len(rejections) != 0 {
		t.Fatalf("rejections = %#v/%v, want none for a timeout", rejections, listErr)
	}
}

func TestWorkflowCompletionReportsLapseTimeoutAsFailure(t *testing.T) {
	timeout := &syncer.TimeoutError{Limit: time.Hour}
	outcome, reason, level := workflowCompletion(Result{}, errors.Join(timeout, errors.New("other")))
	if outcome != "failed" || reason != "lapse_timeout" || level != slog.LevelError {
		t.Fatalf("workflowCompletion(timeout) = %s/%s/%v, want failed/lapse_timeout/error", outcome, reason, level)
	}
	outcome, reason, _ = workflowCompletion(Result{}, context.Canceled)
	if outcome != "canceled" || reason != "canceled" {
		t.Fatalf("workflowCompletion(canceled) = %s/%s, want canceled/canceled", outcome, reason)
	}
}
