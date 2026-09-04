package workflow

import (
	"encoding/json"
	"fmt"
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
