package schedule

import (
	"testing"
	"time"

	"subsyncd/internal/store"
)

type fixedClock struct{ now time.Time }

func (c fixedClock) Now() time.Time { return c.now }

func TestSchedulerBuildsMissingCompletionFromInjectedClock(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	scheduler := Scheduler{Clock: fixedClock{now: now}, RandomUnit: func() float64 { return 0.5 }}

	completion := scheduler.Missing("job-1", 2, time.Time{})
	if completion.JobID != "job-1" || completion.Outcome != "missing" {
		t.Fatalf("unexpected completion identity: %#v", completion)
	}
	if !completion.AdvanceMissingAttempt {
		t.Fatal("missing result must advance the missing-attempt index")
	}
	if completion.Priority != store.SearchPriorityMissing {
		t.Fatalf("missing priority = %d, want %d", completion.Priority, store.SearchPriorityMissing)
	}
	if want := now.Add(6*time.Hour + 36*time.Minute); !completion.NextAttemptAt.Equal(want) {
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
	if completion.Priority != 0 {
		t.Fatalf("failure priority = %d, want retained priority marker 0", completion.Priority)
	}
	if want := now.Add(time.Hour); !completion.NextAttemptAt.Equal(want) {
		t.Fatalf("next attempt = %s, want %s", completion.NextAttemptAt, want)
	}
}

func TestSchedulerSkipsEarlyMissingRetriesForAgedMedia(t *testing.T) {
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	scheduler := Scheduler{Clock: fixedClock{now: now}, RandomUnit: func() float64 { return 0 }, AgedAfter: 30 * 24 * time.Hour}
	week, fortnight := 7*24*time.Hour, 14*24*time.Hour
	cases := []struct {
		name     string
		attempt  int
		released time.Time
		next     time.Duration
		floor    int
	}{
		{"unknown release keeps the full schedule", 1, time.Time{}, 6 * time.Hour, 0},
		{"recent release keeps the full schedule", 1, now.Add(-29 * 24 * time.Hour), 6 * time.Hour, 0},
		{"future release keeps the full schedule", 1, now.Add(24 * time.Hour), 6 * time.Hour, 0},
		{"release exactly at the cutoff is aged", 1, now.Add(-30 * 24 * time.Hour), week, AgedMissingAttempt},
		{"aged first retry waits a week", 1, now.Add(-400 * 24 * time.Hour), week, AgedMissingAttempt},
		{"aged search past the weekly step waits a fortnight", AgedMissingAttempt + 1, now.Add(-400 * 24 * time.Hour), fortnight, 0},
		{"aged search stays fortnightly", 12, now.Add(-400 * 24 * time.Hour), fortnight, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			completion := scheduler.Missing("job", tc.attempt, tc.released)
			if want := now.Add(tc.next); !completion.NextAttemptAt.Equal(want) {
				t.Fatalf("next attempt = %s, want %s", completion.NextAttemptAt, want)
			}
			if completion.MissingAttemptFloor != tc.floor {
				t.Fatalf("attempt floor = %d, want %d", completion.MissingAttemptFloor, tc.floor)
			}
		})
	}
}

func TestSchedulerAgedRuleDisabled(t *testing.T) {
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	scheduler := Scheduler{Clock: fixedClock{now: now}, RandomUnit: func() float64 { return 0 }}
	completion := scheduler.Missing("job", 1, now.Add(-400*24*time.Hour))
	if want := now.Add(6 * time.Hour); !completion.NextAttemptAt.Equal(want) || completion.MissingAttemptFloor != 0 {
		t.Fatalf("completion = %#v, want the full schedule", completion)
	}
}

func TestAgedMissingAttemptIsTheWeeklyStep(t *testing.T) {
	if got := MissingDelay(AgedMissingAttempt, 0); got != 7*24*time.Hour {
		t.Fatalf("aged step delay = %s, want 7 days", got)
	}
	if got := MissingDelay(AgedMissingAttempt+1, 0); got != 14*24*time.Hour {
		t.Fatalf("step after the aged step = %s, want 14 days", got)
	}
}
