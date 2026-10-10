package worker

import (
	"context"
	"errors"
	"testing"
	"time"

	"subsyncd/internal/testutil"
	"subsyncd/internal/workflow"
)

func missingMedia() error {
	return &workflow.MediaUnavailableError{Err: errors.New("stat media: no such file or directory")}
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

func TestMediaBreakerPauseDoublesUntilASearchReadsItsMedia(t *testing.T) {
	now := time.Date(2026, 10, 10, 1, 48, 46, 0, time.UTC)
	clock := testutil.NewClock(now)
	repository := newWorkerRepository(40, now)
	service := &workerWorkflow{err: missingMedia()}
	worker := testWorker(repository, service, clock)
	worker.SearchBatch = 1
	trip := func() time.Duration {
		t.Helper()
		for range mediaBreakerThreshold {
			_ = worker.RunOnce(t.Context())
		}
		worker.breakerMu.Lock()
		defer worker.breakerMu.Unlock()
		pause := worker.mediaPausedUntil.Sub(clock.Now())
		clock.Advance(pause)
		return pause
	}
	for _, want := range []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 16 * time.Minute, 30 * time.Minute, 30 * time.Minute} {
		if got := trip(); got != want {
			t.Fatalf("pause = %v, want %v", got, want)
		}
	}
	service.mu.Lock()
	service.err = nil
	service.outcome = workflow.Result{Outcome: workflow.OutcomeNoResult}
	service.mu.Unlock()
	_ = worker.RunOnce(t.Context())
	service.mu.Lock()
	service.err = missingMedia()
	service.mu.Unlock()
	if got := trip(); got != time.Minute {
		t.Fatalf("pause after a search read its media = %v, want 1m", got)
	}
}

func TestDaemonDispatchHoldsDuringAMediaPause(t *testing.T) {
	now := time.Date(2026, 10, 10, 1, 48, 46, 0, time.UTC)
	repository := newWorkerRepository(6, now)
	service := &workerWorkflow{err: missingMedia()}
	worker := testWorker(repository, service, testutil.NewClock(now))
	worker.MaxWorkflows = 1
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- worker.Run(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	for service.callCount() < mediaBreakerThreshold && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	// Several polls pass while paused; the test clock never reaches the end.
	time.Sleep(20 * worker.PollInterval)
	cancel()
	<-done
	if service.callCount() != mediaBreakerThreshold {
		t.Fatalf("workflow calls = %d, want %d: dispatch must hold during the pause", service.callCount(), mediaBreakerThreshold)
	}
}

func (w *workerWorkflow) callCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.calls
}
