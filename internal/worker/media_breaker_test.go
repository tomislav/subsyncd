package worker

import (
	"fmt"
	"os"
	"testing"
	"time"

	"subsyncd/internal/testutil"
	"subsyncd/internal/workflow"
)

func missingMedia() error {
	return fmt.Errorf("refresh subtitle inventory: stat media: %w", os.ErrNotExist)
}

func TestMediaUnavailableFailureKeepsItsQueuePosition(t *testing.T) {
	now := time.Date(2026, 10, 10, 1, 48, 46, 0, time.UTC)
	repository := newWorkerRepository(1, now)
	worker := testWorker(repository, &workerWorkflow{err: missingMedia()}, testutil.NewClock(now))
	_ = worker.RunOnce(t.Context())
	completion := repository.searchCompletions[0]
	if !completion.PreserveQueueOrder || !completion.AdvanceFailureAttempt || !completion.NextAttemptAt.Equal(now.Add(time.Minute)) {
		t.Fatalf("media-unavailable completion = %#v; want a failure that keeps its queue position", completion)
	}
}

func TestConsecutiveMediaUnavailableFailuresPauseLeasingBriefly(t *testing.T) {
	now := time.Date(2026, 10, 10, 1, 48, 46, 0, time.UTC)
	clock := testutil.NewClock(now)
	repository := newWorkerRepository(5, now)
	service := &workerWorkflow{err: missingMedia()}
	worker := testWorker(repository, service, clock)
	worker.SearchBatch = 1
	for range mediaBreakerThreshold + 2 {
		_ = worker.RunOnce(t.Context())
	}
	if service.calls != mediaBreakerThreshold {
		t.Fatalf("workflow calls = %d, want %d before the pause", service.calls, mediaBreakerThreshold)
	}
	clock.Advance(mediaBreakerPause)
	_ = worker.RunOnce(t.Context())
	if service.calls != mediaBreakerThreshold+1 {
		t.Fatalf("workflow calls after the pause = %d, want %d", service.calls, mediaBreakerThreshold+1)
	}
}

func TestOtherOutcomesResetTheMediaBreaker(t *testing.T) {
	now := time.Date(2026, 10, 10, 1, 48, 46, 0, time.UTC)
	repository := newWorkerRepository(6, now)
	missing := workflow.Result{}
	service := &workerWorkflow{outcomes: []workflow.Result{missing, missing, {Outcome: workflow.OutcomeNoResult}, missing, missing, missing}}
	service.errs = []error{missingMedia(), missingMedia(), nil, missingMedia(), missingMedia(), nil}
	worker := testWorker(repository, service, testutil.NewClock(now))
	worker.SearchBatch = 1
	for range 6 {
		_ = worker.RunOnce(t.Context())
	}
	if service.calls != 6 {
		t.Fatalf("workflow calls = %d, want 6: no run of %d media failures in a row", service.calls, mediaBreakerThreshold)
	}
}
