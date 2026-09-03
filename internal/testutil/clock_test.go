package testutil

import (
	"testing"
	"time"
)

func TestClockCanAdvanceDeterministically(t *testing.T) {
	start := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	clock := NewClock(start)
	clock.Advance(90 * time.Second)

	if want := start.Add(90 * time.Second); !clock.Now().Equal(want) {
		t.Fatalf("now = %s, want %s", clock.Now(), want)
	}
}
