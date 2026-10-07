package workflow

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"subsyncd/internal/domain"
)

func TestCandidateSignatureIncludesEpisodeRangeOnlyForRanges(t *testing.T) {
	candidate := domain.Candidate{ProviderID: "p", ResultID: "r", Language: "hr", Kind: domain.MediaEpisode}
	single := domain.Media{Ref: domain.MediaRef{Kind: domain.MediaEpisode}, Season: 6, Episode: 1}
	checked := single
	checked.EpisodeEnd = domain.CheckedUnsupportedEpisodeEnd
	rangeTwo, rangeThree := single, single
	rangeTwo.EpisodeEnd, rangeThree.EpisodeEnd = 2, 3
	sign := func(media domain.Media) string {
		t.Helper()
		value, err := candidateSignature(candidate, media)
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	if sign(single) != sign(checked) {
		t.Error("the checked marker changed a single-episode signature")
	}
	if sign(single) == sign(rangeTwo) || sign(rangeTwo) == sign(rangeThree) {
		t.Error("episode ranges do not change the signature")
	}
	// Single-episode signatures must stay byte-for-byte what they were, so
	// existing rejections keep applying.
	if got := sign(single); got != "b68dd5d1f6341ee5f5ece1673b886b7c4a502497d926fd12d3d35221596cfb20" {
		t.Errorf("single-episode signature changed to %s", got)
	}
}

func writeSRTEndingAt(t *testing.T, end time.Duration) string {
	t.Helper()
	format := func(d time.Duration) string {
		return fmt.Sprintf("%02d:%02d:%02d,%03d", int(d.Hours()), int(d.Minutes())%60, int(d.Seconds())%60, d.Milliseconds()%1000)
	}
	body := fmt.Sprintf("1\n%s --> %s\nHello\n\n2\n%s --> %s\nBye\n", format(time.Second), format(2*time.Second), format(end-time.Second), format(end))
	path := filepath.Join(t.TempDir(), "candidate.srt")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRangeCoverageRequiresThreeQuartersOfTheRuntime(t *testing.T) {
	ranged := domain.Media{Ref: domain.MediaRef{Kind: domain.MediaEpisode}, Season: 6, Episode: 1, EpisodeEnd: 2, Duration: 93 * time.Minute}
	partial := writeSRTEndingAt(t, 46*time.Minute)
	full := writeSRTEndingAt(t, 90*time.Minute)

	err := checkRangeCoverage(partial, ranged)
	var validation *subtitleValidationError
	if !errors.As(err, &validation) || validation.code != "partial_coverage" || rejectionReasonCode(err) != "partial_coverage" {
		t.Fatalf("partial subtitle: err = %v (reason %q), want partial_coverage", err, rejectionReasonCode(err))
	}
	if err := checkRangeCoverage(full, ranged); err != nil {
		t.Fatalf("full subtitle: %v", err)
	}
	unknown := ranged
	unknown.Duration = 0
	single := ranged
	single.EpisodeEnd = 0
	for name, media := range map[string]domain.Media{"unknown runtime": unknown, "single episode": single} {
		if err := checkRangeCoverage(partial, media); err != nil {
			t.Errorf("%s: %v, want no coverage check", name, err)
		}
	}
	// The install-time validation applies the same rule.
	if _, err := validatedSubtitle(partial, ranged); rejectionReasonCode(err) != "partial_coverage" {
		t.Fatalf("install validation: %v, want partial_coverage", err)
	}
	if _, err := validatedSubtitle(full, ranged); err != nil {
		t.Fatalf("install validation of full subtitle: %v", err)
	}
}
