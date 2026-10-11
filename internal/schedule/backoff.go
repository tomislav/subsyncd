package schedule

import (
	"time"
)

// missingIntervals are the gaps between the absolute missing-subtitle
// milestones: 0, 6h, 12h, 24h, 3d, 7d, 14d, then every 14d. No retry comes
// sooner than six hours, the provider search-result cache lifetime: an earlier
// check would reuse the cached answer and could not find a new subtitle.
var missingIntervals = [...]time.Duration{
	0,
	6 * time.Hour,
	6 * time.Hour,
	12 * time.Hour,
	48 * time.Hour,
	96 * time.Hour,
	168 * time.Hour,
	336 * time.Hour,
}

// AgedMissingAttempt is the missing attempt whose interval is the 7-day step;
// media released long ago starts its retries there.
const AgedMissingAttempt = 6

var workflowFailureDelays = [...]time.Duration{
	time.Minute,
	5 * time.Minute,
	15 * time.Minute,
	time.Hour,
	6 * time.Hour,
	24 * time.Hour,
}

var failureDelays = [...]time.Duration{
	time.Minute,
	5 * time.Minute,
	15 * time.Minute,
	time.Hour,
}

func MissingDelay(attempt int, randomUnit float64) time.Duration {
	if attempt <= 0 {
		return 0
	}
	if attempt >= len(missingIntervals) {
		attempt = len(missingIntervals) - 1
	}
	if randomUnit < 0 {
		randomUnit = 0
	} else if randomUnit > 1 {
		randomUnit = 1
	}
	// Jitter only delays (0 to +20%), so a retry never lands inside the
	// six-hour search cache of the previous search.
	factor := 1 + 0.2*randomUnit
	return time.Duration(float64(missingIntervals[attempt]) * factor)
}

func FailureDelay(attempt int) time.Duration {
	return delayAt(attempt, failureDelays[:])
}

func WorkflowFailureDelay(attempt int) time.Duration {
	return delayAt(attempt, workflowFailureDelays[:])
}

func delayAt(attempt int, delays []time.Duration) time.Duration {
	if attempt < 0 {
		attempt = 0
	}
	if attempt >= len(delays) {
		attempt = len(delays) - 1
	}
	return delays[attempt]
}

func UpgradeDelay(exactHash bool, score int) (time.Duration, bool) {
	if exactHash || score < 35 || score > 100 {
		return 0, false
	}
	switch {
	case score >= 85:
		return 90 * 24 * time.Hour, true
	case score >= 60:
		return 30 * 24 * time.Hour, true
	default:
		return 7 * 24 * time.Hour, true
	}
}
