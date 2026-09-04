package schedule

import (
	"testing"
	"time"
)

type fixedClock struct{ now time.Time }

func (c fixedClock) Now() time.Time { return c.now }

func TestSchedulerBuildsMissingCompletionFromInjectedClock(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	scheduler := Scheduler{Clock: fixedClock{now: now}, RandomUnit: func() float64 { return 0.5 }}

	completion := scheduler.Missing("job-1", 2)
	if completion.JobID != "job-1" || completion.Outcome != "missing" {
		t.Fatalf("unexpected completion identity: %#v", completion)
	}
	if !completion.AdvanceMissingAttempt {
		t.Fatal("missing result must advance the missing-attempt index")
	}
	if want := now.Add(2 * time.Hour); !completion.NextAttemptAt.Equal(want) {
		t.Fatalf("next attempt = %s, want %s", completion.NextAttemptAt, want)
	}
}

func TestSchedulerBuildsFailureCompletionWithoutAdvancingMissingAttempt(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	scheduler := Scheduler{Clock: fixedClock{now: now}}

	completion := scheduler.Failure("job-2", 3, "transport_error")
	if completion.AdvanceMissingAttempt {
		t.Fatal("transport failure must not advance the missing-attempt index")
	}
	if !completion.AdvanceFailureAttempt {
		t.Fatal("transport failure must advance the failure-attempt index")
	}
	if completion.Outcome != "transport_error" {
		t.Fatalf("outcome = %q", completion.Outcome)
	}
	if want := now.Add(time.Hour); !completion.NextAttemptAt.Equal(want) {
		t.Fatalf("next attempt = %s, want %s", completion.NextAttemptAt, want)
	}
}
