package workflow

import (
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"path/filepath"
	"time"

	"subsyncd/internal/domain"
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

// scheduleUpgrade spreads ordinary checks and lengthens repeated unchanged
// assessments, and ends them after MaxUpgradeChecks unchanged checks. A preferred-provider reset earlier than the base interval remains
// authoritative, without jitter or backoff delaying its recovery check.
func (s *Service) scheduleUpgrade(result *Result, attempt int) {
	if result.NextUpgrade.IsZero() {
		return
	}
	now := s.Clock.Now()
	base := s.nextUpgradeAt(now, result.Score, result.Candidate).Sub(now)
	if result.upgradeRecovery {
		return
	}
	if result.Outcome != OutcomeSatisfied {
		attempt = 0
	}
	if s.MaxUpgradeChecks > 0 && attempt >= s.MaxUpgradeChecks && !s.isFallbackProvider(result.Candidate.ProviderID) {
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
	unit := rand.Float64()
	if s.RandomUnit != nil {
		unit = s.RandomUnit()
	}
	unit = max(0, min(1, unit))
	result.NextUpgrade = now.Add(base + time.Duration(float64(base)*0.1*(2*unit-1)))
}
