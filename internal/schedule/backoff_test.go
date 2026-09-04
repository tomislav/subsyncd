package schedule

import (
	"net/http"
	"testing"
	"time"
)

func TestMissingDelayProducesAbsoluteMilestonesAcrossAttempts(t *testing.T) {
	if got := MissingDelay(0, 0.5); got != 0 {
		t.Fatalf("MissingDelay(0, .5) = %s, want 0s", got)
	}

	wants := []time.Duration{
		30 * time.Minute,
		2 * time.Hour,
		8 * time.Hour,
		24 * time.Hour,
		72 * time.Hour,
		168 * time.Hour,
		336 * time.Hour,
		672 * time.Hour,
	}
	elapsed := time.Duration(0)
	for attempt, want := range wants {
		elapsed += MissingDelay(attempt+1, 0.5)
		if elapsed != want {
			t.Errorf("elapsed after missing attempt %d = %s, want %s", attempt+1, elapsed, want)
		}
	}
}

func TestMissingDelayAppliesTenPercentJitter(t *testing.T) {
	tests := []struct {
		random float64
		want   time.Duration
	}{{0, 27 * time.Minute}, {0.5, 30 * time.Minute}, {1, 33 * time.Minute}}
	for _, test := range tests {
		if got := MissingDelay(1, test.random); got != test.want {
			t.Errorf("MissingDelay(1, %v) = %s, want %s", test.random, got, test.want)
		}
	}
}

func TestFailureDelayCapsAtOneHour(t *testing.T) {
	wants := []time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute, time.Hour, time.Hour}
	for attempt, want := range wants {
		if got := FailureDelay(attempt); got != want {
			t.Errorf("FailureDelay(%d) = %s, want %s", attempt, got, want)
		}
	}
}

func TestRetryAfterParsesSecondsAndHTTPDate(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	if got, ok := RetryAfter(now, "90"); !ok || !got.Equal(now.Add(90*time.Second)) {
		t.Errorf("RetryAfter seconds = %s, %v", got, ok)
	}
	date := now.Add(3 * time.Minute).Format(http.TimeFormat)
	if got, ok := RetryAfter(now, date); !ok || !got.Equal(now.Add(3*time.Minute)) {
		t.Errorf("RetryAfter date = %s, %v", got, ok)
	}
	if _, ok := RetryAfter(now, "nonsense"); ok {
		t.Error("RetryAfter malformed header accepted")
	}
}

func TestUpgradeDelayUsesScoreBandsAndSkipsExactHash(t *testing.T) {
	if _, ok := UpgradeDelay(true, 100); ok {
		t.Fatal("exact hash received an upgrade schedule")
	}
	tests := []struct {
		score int
		want  time.Duration
		ok    bool
	}{{34, 0, false}, {35, 7 * 24 * time.Hour, true}, {59, 7 * 24 * time.Hour, true}, {60, 30 * 24 * time.Hour, true}, {84, 30 * 24 * time.Hour, true}, {85, 90 * 24 * time.Hour, true}, {100, 90 * 24 * time.Hour, true}}
	for _, test := range tests {
		got, ok := UpgradeDelay(false, test.score)
		if got != test.want || ok != test.ok {
			t.Errorf("UpgradeDelay(false, %d) = %s, %v; want %s, %v", test.score, got, ok, test.want, test.ok)
		}
	}
}
