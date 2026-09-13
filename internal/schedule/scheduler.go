package schedule

import (
	"math/rand/v2"
	"time"

	"subsyncd/internal/store"
)

// Clock makes schedule decisions deterministic in tests and independent of
// wall-clock reads in workers.
type Clock interface {
	Now() time.Time
}

// Scheduler converts workflow outcomes into durable search completions.
type Scheduler struct {
	Clock      Clock
	RandomUnit func() float64
}

func (s Scheduler) Missing(jobID string, attempt int) store.SearchCompletion {
	randomUnit := rand.Float64()
	if s.RandomUnit != nil {
		randomUnit = s.RandomUnit()
	}
	return store.SearchCompletion{
		JobID:                 jobID,
		Outcome:               "missing",
		NextAttemptAt:         s.Clock.Now().Add(MissingDelay(attempt, randomUnit)),
		AdvanceMissingAttempt: true,
		ResetFailureAttempt:   true,
		Priority:              store.SearchPriorityMissing,
	}
}

func (s Scheduler) Failure(jobID string, failureAttempt int, outcome string) store.SearchCompletion {
	return store.SearchCompletion{
		JobID:                 jobID,
		Outcome:               outcome,
		NextAttemptAt:         s.Clock.Now().Add(WorkflowFailureDelay(failureAttempt)),
		AdvanceFailureAttempt: true,
	}
}
