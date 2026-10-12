package workflow

import (
	"encoding/json"
	"slices"
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
			s.scheduleUpgrade(&result, tc.attempt, time.Time{}, time.Time{})
			want := now.Add(time.Duration(tc.days * float64(24*time.Hour)))
			if !result.NextUpgrade.Equal(want) {
				t.Fatalf("next=%v want=%v", result.NextUpgrade, want)
			}
		})
	}
}

func TestUpgradeChecksStopAfterTheConfiguredNumberOfUnchangedChecks(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name            string
		limit, attempt  int
		outcome         Outcome
		provider        string
		wantDays        float64
		wantNoMoreCheck bool
	}{
		{"before the limit", 4, 3, OutcomeSatisfied, "preferred", 60, false},
		{"at the limit", 4, 4, OutcomeSatisfied, "preferred", 0, true},
		{"past the limit", 4, 9, OutcomeSatisfied, "preferred", 0, true},
		{"replacement restarts the count", 4, 4, OutcomeInstalled, "preferred", 7, false},
		{"fallback keeps checking", 4, 9, OutcomeSatisfied, "fallback", 90, false},
		{"zero means unlimited", 0, 99, OutcomeSatisfied, "preferred", 90, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &Service{Clock: fixedWorkflowClock{at: now}, RandomUnit: func() float64 { return 0.5 }, FallbackProviderOrder: []string{"fallback"}, MaxUpgradeChecks: tc.limit}
			c := domain.Candidate{ProviderID: tc.provider}
			result := Result{Outcome: tc.outcome, Score: domain.Score{Total: 35}, Candidate: c}
			result.NextUpgrade = s.nextUpgradeAt(now, result.Score, c)
			s.scheduleUpgrade(&result, tc.attempt, time.Time{}, time.Time{})
			if tc.wantNoMoreCheck {
				if !result.NextUpgrade.IsZero() {
					t.Fatalf("next=%v, want no further upgrade check", result.NextUpgrade)
				}
				return
			}
			if want := now.Add(time.Duration(tc.wantDays * float64(24*time.Hour))); !result.NextUpgrade.Equal(want) {
				t.Fatalf("next=%v want=%v", result.NextUpgrade, want)
			}
		})
	}
}

func TestFallbackPromotionChecksForRecentReleases(t *testing.T) {
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	day := 24 * time.Hour
	recent := now.Add(-3 * day)
	aged := now.Add(-45 * day)
	for _, tc := range []struct {
		name                  string
		provider              string
		outcome               Outcome
		attempt               int
		unit                  float64
		releasedAt, installed time.Time
		recovery              time.Duration
		want                  time.Duration
	}{
		{"install checks after six hours", "fallback", OutcomeInstalled, 0, 0, recent, time.Time{}, 0, 6 * time.Hour},
		{"install jitter only delays", "fallback", OutcomeInstalled, 0, 1, recent, time.Time{}, 0, 7*time.Hour + 12*time.Minute},
		{"jitter below zero clamps to the step", "fallback", OutcomeInstalled, 0, -1, recent, time.Time{}, 0, 6 * time.Hour},
		{"second check", "fallback", OutcomeSatisfied, 1, 0, recent, recent, 0, 6 * time.Hour},
		{"third check", "fallback", OutcomeSatisfied, 2, 0, recent, recent, 0, 12 * time.Hour},
		{"fourth check", "fallback", OutcomeSatisfied, 3, 0, recent, recent, 0, 2 * day},
		{"weekly after the early steps", "fallback", OutcomeSatisfied, 4, 0.5, recent, recent, 0, 7 * day},
		{"then the usual stretch", "fallback", OutcomeSatisfied, 5, 0.5, recent, recent, 0, 14 * day},
		{"aged out mid-way starts weekly", "fallback", OutcomeSatisfied, 2, 0.5, now.Add(-31 * day), now.Add(-30*day + time.Hour), 0, 7 * day},
		{"aged out after the early steps", "fallback", OutcomeSatisfied, 6, 0.5, now.Add(-40 * day), now.Add(-29 * day), 0, 30 * day},
		{"aged at install keeps the usual stretch", "fallback", OutcomeSatisfied, 2, 0.5, aged, now.Add(-14 * day), 0, 30 * day},
		{"aged install is weekly", "fallback", OutcomeInstalled, 0, 0.5, aged, time.Time{}, 0, 7 * day},
		{"unknown release date", "fallback", OutcomeInstalled, 0, 0.5, time.Time{}, time.Time{}, 0, 7 * day},
		{"unknown release date keeps the stretch", "fallback", OutcomeSatisfied, 2, 0.5, time.Time{}, recent, 0, 30 * day},
		{"future release date", "fallback", OutcomeInstalled, 0, 0.5, now.Add(day), time.Time{}, 0, 7 * day},
		{"preferred install unaffected", "preferred", OutcomeInstalled, 0, 0.5, recent, time.Time{}, 0, 7 * day},
		{"preferred check unaffected", "preferred", OutcomeSatisfied, 1, 0.5, recent, recent, 0, 14 * day},
		{"earlier recovery wins", "fallback", OutcomeInstalled, 0, 0, recent, time.Time{}, 2 * time.Hour, 2 * time.Hour},
		{"later recovery loses", "fallback", OutcomeInstalled, 0, 0, recent, time.Time{}, 2 * day, 6 * time.Hour},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &Service{Clock: fixedWorkflowClock{at: now}, RandomUnit: func() float64 { return tc.unit }, FallbackProviderOrder: []string{"fallback"}, AgedAfter: 30 * day, MaxUpgradeChecks: 4}
			c := domain.Candidate{ProviderID: tc.provider}
			result := Result{Outcome: tc.outcome, Score: domain.Score{Total: 35}, Candidate: c}
			result.NextUpgrade = s.nextUpgradeAt(now, result.Score, c)
			if tc.recovery > 0 {
				result.NextUpgrade = now.Add(tc.recovery)
				result.upgradeRecovery = true
			}
			s.scheduleUpgrade(&result, tc.attempt, tc.releasedAt, tc.installed)
			if want := now.Add(tc.want); !result.NextUpgrade.Equal(want) {
				t.Fatalf("next=%v want=%v", result.NextUpgrade.Sub(now), tc.want)
			}
		})
	}
}

// The early checks of a recent fallback installation fall at the
// missing-search milestones, counted from the install: 6h, 12h, 24h and 3d.
func TestFallbackPromotionChecksFollowTheMissingMilestones(t *testing.T) {
	installed := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	released := installed.Add(-24 * time.Hour)
	now := installed
	var got []time.Duration
	for attempt := range len(recentFallbackChecks) {
		s := &Service{Clock: fixedWorkflowClock{at: now}, RandomUnit: func() float64 { return 0 }, FallbackProviderOrder: []string{"fallback"}, AgedAfter: 30 * 24 * time.Hour, MaxUpgradeChecks: 4}
		outcome := OutcomeSatisfied
		if attempt == 0 {
			outcome = OutcomeInstalled
		}
		c := domain.Candidate{ProviderID: "fallback"}
		result := Result{Outcome: outcome, Score: domain.Score{Total: 35}, Candidate: c}
		result.NextUpgrade = s.nextUpgradeAt(now, result.Score, c)
		s.scheduleUpgrade(&result, attempt, released, installed)
		now = result.NextUpgrade
		got = append(got, now.Sub(installed))
	}
	want := []time.Duration{6 * time.Hour, 12 * time.Hour, 24 * time.Hour, 72 * time.Hour}
	if !slices.Equal(got, want) {
		t.Fatalf("checks after install = %v, want %v", got, want)
	}
}

func TestFallbackPromotionJitterNeverPrecedesTheStep(t *testing.T) {
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	for attempt, step := range recentFallbackChecks {
		for _, unit := range []float64{-0.5, 0, 0.25, 0.5, 0.999, 1, 1.5} {
			s := &Service{Clock: fixedWorkflowClock{at: now}, RandomUnit: func() float64 { return unit }, FallbackProviderOrder: []string{"fallback"}, AgedAfter: 30 * 24 * time.Hour}
			outcome := OutcomeSatisfied
			if attempt == 0 {
				outcome = OutcomeInstalled
			}
			result := Result{Outcome: outcome, Score: domain.Score{Total: 35}, Candidate: domain.Candidate{ProviderID: "fallback"}, NextUpgrade: now.Add(7 * 24 * time.Hour)}
			s.scheduleUpgrade(&result, attempt, now.Add(-24*time.Hour), now.Add(-time.Hour))
			if delay := result.NextUpgrade.Sub(now); delay < step || delay > step+step/5 {
				t.Fatalf("attempt %d unit %v: delay %v outside [%v, %v]", attempt, unit, delay, step, step+step/5)
			}
		}
	}
}
