package workflow

import (
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"path/filepath"
	"time"

	"subsyncd/internal/domain"
	"subsyncd/internal/schedule"
	"subsyncd/internal/store"
)

const DefaultMinimumUpgradeDelta = 10

func ShouldUpgrade(existing store.Installation, candidate domain.Score, candidateExact bool, minimumDelta int) (bool, error) {
	if minimumDelta <= 0 {
		minimumDelta = DefaultMinimumUpgradeDelta
	}
	current, exact, err := installedScore(existing)
	if err != nil {
		return false, err
	}
	if exact {
		return false, nil
	}
	if candidateExact {
		return true, nil
	}
	return candidate.Total >= current.Total+minimumDelta, nil
}

func ShouldUpgradeForMedia(existing store.Installation, media domain.Media, candidate domain.Score, candidateExact bool, minimumDelta int) (bool, error) {
	if !InstallationMatchesMedia(existing, media) {
		return true, nil
	}
	return ShouldUpgrade(existing, candidate, candidateExact, minimumDelta)
}

func InstallationMatchesMedia(installation store.Installation, media domain.Media) bool {
	fingerprint := media.Fingerprint
	return filepath.Clean(installation.MediaPath) == filepath.Clean(fingerprint.Path) &&
		installation.MediaFileID == fingerprint.FileID &&
		installation.MediaSize == fingerprint.Size &&
		installation.MediaModTimeNS == fingerprint.ModTime.UnixNano()
}

func NextUpgradeAt(now time.Time, score domain.Score, exact bool) time.Time {
	if exact {
		return time.Time{}
	}
	switch {
	case score.Total >= 85:
		return now.Add(90 * 24 * time.Hour)
	case score.Total >= 60:
		return now.Add(30 * 24 * time.Hour)
	default:
		return now.Add(7 * 24 * time.Hour)
	}
}

func installedScore(installation store.Installation) (domain.Score, bool, error) {
	if len(installation.ScoreJSON) == 0 || string(installation.ScoreJSON) == "{}" {
		return domain.Score{}, false, nil
	}
	var score domain.Score
	if err := json.Unmarshal(installation.ScoreJSON, &score); err != nil {
		return domain.Score{}, false, fmt.Errorf("decode installed subtitle score: %w", err)
	}
	for _, contribution := range score.Contributions {
		if contribution.Signal == "exact_hash" && contribution.Points == 100 {
			return score, true, nil
		}
	}
	return score, false, nil
}

// recentFallbackChecks is how many unchanged promotion checks of a fallback
// installation of a recent release follow the missing-search schedule
// (schedule.MissingDelay): about 6h, 12h, 24h and 3 days after install,
// because a preferred provider often publishes days after release. Like
// missing searches, each delay counts from the previous check, so a check that
// ran late (a throttled provider, a stopped daemon) still leaves a full step,
// and at least the six-hour search cache, before the next one.
const recentFallbackChecks = 4

// scheduleUpgrade spreads ordinary checks and lengthens repeated unchanged
// assessments, and ends them after MaxUpgradeChecks unchanged checks. A preferred-provider reset earlier than the base interval remains
// authoritative, without jitter or backoff delaying its recovery check.
// A fallback installation of a release younger than AgedAfter is checked on
// the recentFallbackChecks steps first; installedAt tells whether its later
// checks follow those steps.
func (s *Service) scheduleUpgrade(result *Result, attempt int, releasedAt, installedAt time.Time) {
	if result.NextUpgrade.IsZero() {
		return
	}
	now := s.Clock.Now()
	base := s.nextUpgradeAt(now, result.Score, result.Candidate).Sub(now)
	if result.Outcome != OutcomeSatisfied {
		attempt = 0
	}
	fallback := s.isFallbackProvider(result.Candidate.ProviderID)
	if fallback && s.recentRelease(releasedAt, now) && attempt < recentFallbackChecks {
		// The missing-search delay after search attempt+1, jitter included.
		next := now.Add(schedule.MissingDelay(attempt+1, s.randomUnit()))
		if !result.upgradeRecovery || next.Before(result.NextUpgrade) {
			result.NextUpgrade = next
		}
		return
	}
	if result.upgradeRecovery {
		return
	}
	if fallback && s.recentRelease(releasedAt, installedAt) {
		// The early steps already used do not stretch the weekly cadence.
		attempt = max(0, attempt-recentFallbackChecks)
	}
	if s.MaxUpgradeChecks > 0 && attempt >= s.MaxUpgradeChecks && !fallback {
		result.NextUpgrade = time.Time{}
		return
	}
	for _, days := range []int{14, 30, 60, 90} {
		delay := time.Duration(days) * 24 * time.Hour
		if attempt > 0 && delay > base {
			base = delay
			attempt--
		}
	}
	result.NextUpgrade = now.Add(base + time.Duration(float64(base)*0.1*(2*s.randomUnit()-1)))
}

// recentRelease reports whether media released at releasedAt was younger than
// AgedAfter at "at". An unknown or later release date, or a zero AgedAfter, is
// not recent.
func (s *Service) recentRelease(releasedAt, at time.Time) bool {
	return s.AgedAfter > 0 && !releasedAt.IsZero() && !at.IsZero() && !releasedAt.After(at) && at.Sub(releasedAt) < s.AgedAfter
}

func (s *Service) randomUnit() float64 {
	unit := rand.Float64()
	if s.RandomUnit != nil {
		unit = s.RandomUnit()
	}
	return max(0, min(1, unit))
}
