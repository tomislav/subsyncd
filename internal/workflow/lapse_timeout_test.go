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
	"subsyncd/internal/provider"
	"subsyncd/internal/store"
	"subsyncd/internal/syncer"
)

func lapseTimeoutTestRun(t *testing.T, candidates ...domain.Candidate) (Result, error, *fakeSynchronizer, int) {
	t.Helper()
	ctx := t.Context()
	request := serviceRequest(t)
	request.Media.EntityID = 1
	request.Media.ReleaseGroup = "GROUP"
	synchronizer := &fakeSynchronizer{synchronizeErrors: map[string]error{"slow": &syncer.TimeoutError{Limit: 80 * time.Minute}}}
	database, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	request.MediaID, _, err = database.Repository().UpsertMedia(ctx, request.Media)
	if err != nil {
		t.Fatal(err)
	}
	service := repeatedLapseTestService(t, request, database.Repository(), synchronizer, &fakeProvider{id: "provider"}, candidates)
	result, runErr := service.Run(ctx, request)
	rejections, err := database.Repository().ListCandidateRejections(ctx, request.MediaID, request.Language, service.Clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	return result, runErr, synchronizer, len(rejections)
}

func slowTimeoutCandidate() domain.Candidate {
	slow := broadCandidate("slow")
	slow.ReleaseNames = []string{"Movie.2024-GROUP"}
	return slow
}

func TestLapseTimeoutIsReusedForLaterLapseCandidates(t *testing.T) {
	result, err, synchronizer, rejections := lapseTimeoutTestRun(t, slowTimeoutCandidate(), broadCandidate("good"))
	var got *syncer.TimeoutError
	if !errors.As(err, &got) {
		t.Fatalf("Run() = %+v, %T %v, want the LAPSE timeout", result, err, err)
	}
	if !slices.Equal(synchronizer.synchronized, []string{"slow"}) {
		t.Fatalf("synchronized = %v, want LAPSE run only for the candidate that timed out", synchronizer.synchronized)
	}
	if rejections != 0 {
		t.Fatalf("rejections = %d, want none for a timeout", rejections)
	}
}

func TestLapseTimeoutStillTriesCandidatesThatDoNotReadTheMedia(t *testing.T) {
	preferred := &fakeSearcher{result: provider.SearchResult{Candidates: []domain.Candidate{broadCandidate("slow")}}}
	fallback := &fakeSearcher{result: provider.SearchResult{Candidates: []domain.Candidate{fallbackCandidate("exact", true)}}}
	s, installer := tierService(t, preferred, fallback)
	synchronizer := &fakeSynchronizer{synchronizeErrors: map[string]error{"slow": &syncer.TimeoutError{Limit: 80 * time.Minute}}}
	s.Synchronizer = synchronizer
	result, err := s.Run(t.Context(), serviceRequest(t))
	if err != nil || result.Outcome != OutcomeInstalled || result.Candidate.ResultID != "exact" || installer.calls != 1 {
		t.Fatalf("Run() = %s/%s, %v, installs=%d; want the fallback exact-hash candidate installed", result.Outcome, result.Candidate.ResultID, err, installer.calls)
	}
	if !slices.Equal(synchronizer.synchronized, []string{"slow"}) {
		t.Fatalf("synchronized = %v, want LAPSE only for the preferred candidate", synchronizer.synchronized)
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
