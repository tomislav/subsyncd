package workflow

import (
	"testing"

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
