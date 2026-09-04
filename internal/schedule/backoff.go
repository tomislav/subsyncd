package schedule

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

// missingIntervals are the gaps between the absolute missing-subtitle
// milestones: 0, 30m, 2h, 8h, 24h, 3d, 7d, 14d, then every 14d.
var missingIntervals = [...]time.Duration{
	0,
	30 * time.Minute,
	90 * time.Minute,
	6 * time.Hour,
	16 * time.Hour,
	48 * time.Hour,
	96 * time.Hour,
	168 * time.Hour,
	336 * time.Hour,
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
	factor := 0.9 + 0.2*randomUnit
	return time.Duration(float64(missingIntervals[attempt]) * factor)
}

func FailureDelay(attempt int) time.Duration {
	if attempt < 0 {
		attempt = 0
	}
	if attempt >= len(failureDelays) {
		attempt = len(failureDelays) - 1
	}
	return failureDelays[attempt]
}

func RetryAfter(now time.Time, header string) (time.Time, bool) {
	value := strings.TrimSpace(header)
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil {
		if seconds < 0 {
			return time.Time{}, false
		}
		return now.Add(time.Duration(seconds) * time.Second), true
	}
	when, err := http.ParseTime(value)
	if err != nil {
		return time.Time{}, false
	}
	return when, true
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
