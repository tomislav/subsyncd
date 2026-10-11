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
	// AgedAfter is how long after release a missing subtitle skips the quick
	// early retries; 0 keeps the full schedule for everything.
	AgedAfter time.Duration
}

// Missing schedules the next search for a subtitle still missing after the
// given attempt. Media released at least AgedAfter ago jumps to the weekly step
// (then fortnightly): its subtitles won't appear within hours, so the quick
// early retries would only repeat every provider search.
func (s Scheduler) Missing(jobID string, attempt int, releasedAt time.Time) store.SearchCompletion {
	randomUnit := rand.Float64()
	if s.RandomUnit != nil {
		randomUnit = s.RandomUnit()
	}
	completion := store.SearchCompletion{
		JobID:                 jobID,
		Outcome:               "missing",
		AdvanceMissingAttempt: true,
		ResetFailureAttempt:   true,
		Priority:              store.SearchPriorityMissing,
	}
	now := s.Clock.Now()
	if s.aged(releasedAt, now) && attempt < AgedMissingAttempt {
		attempt = AgedMissingAttempt
		completion.MissingAttemptFloor = AgedMissingAttempt
	}
	completion.NextAttemptAt = now.Add(MissingDelay(attempt, randomUnit))
	return completion
}

// aged reports whether media released at releasedAt is old enough at now to
// skip the quick early retries. An unknown or future release date is not.
func (s Scheduler) aged(releasedAt, now time.Time) bool {
	return s.AgedAfter > 0 && !releasedAt.IsZero() && !releasedAt.After(now) && now.Sub(releasedAt) >= s.AgedAfter
}

func (s Scheduler) Failure(jobID string, failureAttempt int, outcome string) store.SearchCompletion {
	return store.SearchCompletion{
		JobID:                 jobID,
		Outcome:               outcome,
		NextAttemptAt:         s.Clock.Now().Add(WorkflowFailureDelay(failureAttempt)),
		AdvanceFailureAttempt: true,
	}
}
