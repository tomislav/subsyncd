package match

import (
	"slices"
	"testing"
)

func TestParseReleaseKeepsOnlyTheCutAsEdition(t *testing.T) {
	tests := map[string]string{
		"Film.2016.Ultimate.Edition.Remastered.IMAX.2160p.BluRay-GRP": "ultimate edition",
		"Film.2016.Ultimate.Edition.1080p.WEB-DL-GRP":                 "ultimate edition",
		"Film.2016.REMASTERED.1080p.BluRay-GRP":                       "",
		"Film.2016.IMAX.2160p.WEB-DL-GRP":                             "",
		"Film.2016.EXTENDED.IMAX.1080p.BluRay-GRP":                    "extended",
		"Film.2016.Open.Matte.1080p.BluRay-GRP":                       "",
		"Film.2016.UNRATED.REMASTERED.1080p.BluRay-GRP":               "unrated",
	}
	for raw, want := range tests {
		if got := ParseRelease(raw).Edition; got != want {
			t.Errorf("ParseRelease(%q).Edition = %q, want %q", raw, got, want)
		}
	}
}

func editionScore(t *testing.T, mediaEdition, release string) (rejected bool, points int) {
	t.Helper()
	media := scoredMedia()
	media.Edition = mediaEdition
	candidate := scoredCandidate()
	candidate.ReleaseNames = []string{release}
	score := Evaluate(media, candidate, "en")
	rejected = slices.Contains(score.RejectedReasons, "candidate edition conflicts with target")
	for _, contribution := range score.Contributions {
		if contribution.Signal == "edition" {
			points = contribution.Points
		}
	}
	return rejected, points
}

func TestEditionComparisonUsesTheCutOnly(t *testing.T) {
	tests := []struct {
		name, media, release string
		rejected             bool
		points               int
	}{
		{"remastered ultimate edition matches ultimate edition", "Ultimate Edition Remastered", "Example.Show.S01E02.Ultimate.Edition.1080p.WEB-DL-GROUP", false, 10},
		{"remastered ultimate edition still conflicts with theatrical", "Ultimate Edition Remastered", "Example.Show.S01E02.THEATRICAL.1080p.BluRay-GROUP", true, 0},
		{"imax has no cut, so theatrical does not conflict", "IMAX", "Example.Show.S01E02.THEATRICAL.1080p.BluRay-GROUP", false, 0},
		{"imax has no cut, so extended does not conflict", "IMAX", "Example.Show.S01E02.EXTENDED.IMAX.1080p.BluRay-GROUP", false, 0},
		{"remastered has no cut, so extended does not conflict", "Remastered", "Example.Show.S01E02.EXTENDED.1080p.BluRay-GROUP", false, 0},
		{"a remastered candidate names no cut", "Extended", "Example.Show.S01E02.REMASTERED.1080p.BluRay-GROUP", false, 0},
		{"different cuts still conflict", "Directors Cut", "Example.Show.S01E02.EXTENDED.1080p.BluRay-GROUP", true, 0},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			rejected, points := editionScore(t, test.media, test.release)
			if rejected != test.rejected || points != test.points {
				t.Fatalf("rejected/points = %v/%d, want %v/%d", rejected, points, test.rejected, test.points)
			}
		})
	}
}

func TestHasMatchingEditionTreatsAFileWithoutACutLikeNoEdition(t *testing.T) {
	media := scoredMedia()
	candidate := scoredCandidate()
	candidate.ReleaseNames = []string{"Example.Show.S01E02.1080p.WEB-DL-GROUP"}
	for _, edition := range []string{"", "IMAX", "Remastered"} {
		media.Edition = edition
		if !HasMatchingEdition(media, candidate) {
			t.Errorf("HasMatchingEdition(%q) = false, want true", edition)
		}
	}
	media.Edition = "Ultimate Edition Remastered"
	candidate.ReleaseNames = []string{"Example.Show.S01E02.Ultimate.Edition.1080p.WEB-DL-GROUP"}
	if !HasMatchingEdition(media, candidate) {
		t.Error("HasMatchingEdition(ultimate edition) = false, want true")
	}
}
