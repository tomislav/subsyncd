package worker

import (
	"testing"
	"time"

	"subsyncd/internal/store"
	"subsyncd/internal/testutil"
	"subsyncd/internal/workflow"
)

func TestWorkerHonorsAQueuedReplacement(t *testing.T) {
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		outcome   workflow.Outcome
		clear     bool
		history   bool
		scheduled bool
	}{
		{workflow.OutcomeInstalled, true, true, false},
		{workflow.OutcomeRemoved, true, false, true},
		{workflow.OutcomeNoResult, true, true, true},
		{workflow.OutcomeThrottled, false, true, false},
	} {
		t.Run(string(test.outcome), func(t *testing.T) {
			repository := newWorkerRepository(1, now)
			repository.searches[0].Priority = store.SearchPriorityImport
			repository.searches[0].ReplaceInstalled = true
			service := &workerWorkflow{outcome: workflow.Result{Outcome: test.outcome, RetryAt: now.Add(time.Hour)}}
			w := testWorker(repository, service, testutil.NewClock(now))
			if err := w.runSearchLease(t.Context(), repository.searches[0]); err != nil {
				t.Fatal(err)
			}
			if len(service.requests) != 1 || !service.requests[0].ReplaceInstalled {
				t.Fatalf("workflow requests = %#v", service.requests)
			}
			completion := repository.searchCompletions[0]
			if completion.ClearReplaceInstalled != test.clear {
				t.Fatalf("ClearReplaceInstalled = %v, want %v", completion.ClearReplaceInstalled, test.clear)
			}
			// RemoveManaged records the removal's history row itself.
			if (completion.History != nil) != test.history {
				t.Fatalf("history = %#v", completion.History)
			}
			if test.scheduled && (completion.Priority != store.SearchPriorityMissing || completion.NextAttemptAt.IsZero() || completion.Outcome != string(test.outcome)) {
				t.Fatalf("completion = %#v, want a missing-subtitle retry", completion)
			}
		})
	}

	// An ordinary search never replaces.
	repository := newWorkerRepository(1, now)
	service := &workerWorkflow{outcome: workflow.Result{Outcome: workflow.OutcomeInstalled}}
	if err := testWorker(repository, service, testutil.NewClock(now)).runSearchLease(t.Context(), repository.searches[0]); err != nil {
		t.Fatal(err)
	}
	if service.requests[0].ReplaceInstalled || repository.searchCompletions[0].ClearReplaceInstalled {
		t.Fatal("an ordinary search ran as a replacement")
	}
}
