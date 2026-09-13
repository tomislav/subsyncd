package workflow

import (
	"encoding/json"
	"testing"
	"time"

	"subsyncd/internal/domain"
	"subsyncd/internal/store"
)

func TestUpgradePolicyRequiresExactHashOrMinimumScoreDelta(t *testing.T) {
	existing := installationWithScore(t, 70, false)
	tests := []struct {
		name      string
		candidate domain.Score
		exact     bool
		want      bool
	}{
		{"nine points", domain.Score{Total: 79}, false, false},
		{"ten points", domain.Score{Total: 80}, false, true},
		{"exact improvement", domain.Score{Total: 100}, true, true},
		{"lower exact improvement", domain.Score{Total: 60}, true, true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			allowed, err := ShouldUpgrade(existing, test.candidate, test.exact, 10)
			if err != nil || allowed != test.want {
				t.Fatalf("ShouldUpgrade() = %v, %v", allowed, err)
			}
		})
	}
}

func TestUpgradePolicyNeverReplacesExactHashAndInvalidatesChangedFingerprint(t *testing.T) {
	existing := installationWithScore(t, 100, true)
	allowed, err := ShouldUpgrade(existing, domain.Score{Total: 100}, true, 10)
	if err != nil || allowed {
		t.Fatalf("ShouldUpgrade() = %v, %v", allowed, err)
	}

	media := domain.Media{Fingerprint: domain.MediaFingerprint{Path: "/media/movie.mkv", FileID: 7, Size: 100, ModTime: time.Unix(10, 20)}}
	existing.MediaPath = media.Fingerprint.Path
	existing.MediaFileID = media.Fingerprint.FileID
	existing.MediaSize = media.Fingerprint.Size
	existing.MediaModTimeNS = media.Fingerprint.ModTime.UnixNano()
	if !InstallationMatchesMedia(existing, media) {
		t.Fatal("matching fingerprint was rejected")
	}
	media.Fingerprint.Size++
	if InstallationMatchesMedia(existing, media) {
		t.Fatal("changed fingerprint was accepted")
	}
	allowed, err = ShouldUpgradeForMedia(existing, media, domain.Score{Total: 35}, false, 10)
	if err != nil || !allowed {
		t.Fatalf("changed-fingerprint ShouldUpgradeForMedia() = %v, %v", allowed, err)
	}
}

func TestNextUpgradeAtUsesScoreBandsAndStopsExactHash(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		score domain.Score
		exact bool
		want  time.Duration
	}{
		{domain.Score{Total: 35}, false, 7 * 24 * time.Hour},
		{domain.Score{Total: 59}, false, 7 * 24 * time.Hour},
		{domain.Score{Total: 60}, false, 30 * 24 * time.Hour},
		{domain.Score{Total: 84}, false, 30 * 24 * time.Hour},
		{domain.Score{Total: 85}, false, 90 * 24 * time.Hour},
		{domain.Score{Total: 100}, false, 90 * 24 * time.Hour},
		{domain.Score{Total: 100}, true, 0},
	}
	for _, test := range tests {
		got := NextUpgradeAt(now, test.score, test.exact)
		if test.want == 0 && !got.IsZero() || test.want != 0 && !got.Equal(now.Add(test.want)) {
			t.Fatalf("NextUpgradeAt(%d, exact=%v) = %v", test.score.Total, test.exact, got)
		}
	}
}

func installationWithScore(t *testing.T, total int, exact bool) store.Installation {
	t.Helper()
	score := domain.Score{Total: total}
	if exact {
		score.Contributions = []domain.Contribution{{Signal: "exact_hash", Points: 100}}
	}
	payload, err := json.Marshal(score)
	if err != nil {
		t.Fatal(err)
	}
	return store.Installation{ScoreJSON: payload}
}

func TestUpgradeSchedulingBackoffJitterAndRecovery(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name               string
		score, attempt     int
		unit               float64
		outcome            Outcome
		fallback, recovery bool
		days               float64
	}{
		{"first low", 35, 0, 0.5, OutcomeInstalled, false, false, 7},
		{"first unchanged", 35, 1, 0.5, OutcomeSatisfied, false, false, 14},
		{"second unchanged", 35, 2, 0.5, OutcomeSatisfied, false, false, 30},
		{"third unchanged", 35, 3, 0.5, OutcomeSatisfied, false, false, 60},
		{"capped", 35, 99, 0.5, OutcomeSatisfied, false, false, 90},
		{"medium", 70, 1, 0.5, OutcomeSatisfied, false, false, 60},
		{"high jitter", 90, 9, 1, OutcomeSatisfied, false, false, 99},
		{"low jitter", 35, 1, 0, OutcomeSatisfied, false, false, 12.6},
		{"replacement resets", 35, 4, 0.5, OutcomeInstalled, false, false, 7},
		{"fallback exact", 100, 2, 0.5, OutcomeSatisfied, true, false, 30},
		{"recovery", 35, 4, 1, OutcomeInstalled, true, true, 0.5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &Service{Clock: fixedWorkflowClock{at: now}, RandomUnit: func() float64 { return tc.unit }, FallbackProviderOrder: []string{"fallback"}}
			c := domain.Candidate{ProviderID: "preferred"}
			if tc.fallback {
				c.ProviderID = "fallback"
				c.ExactHash = tc.score == 100
			}
			result := Result{Outcome: tc.outcome, Score: domain.Score{Total: tc.score}, Candidate: c}
			result.NextUpgrade = s.nextUpgradeAt(now, result.Score, c)
			if tc.recovery {
				result.NextUpgrade = now.Add(12 * time.Hour)
				result.upgradeRecovery = true
			}
			s.scheduleUpgrade(&result, tc.attempt)
			want := now.Add(time.Duration(tc.days * float64(24*time.Hour)))
			if !result.NextUpgrade.Equal(want) {
				t.Fatalf("next=%v want=%v", result.NextUpgrade, want)
			}
		})
	}
}
