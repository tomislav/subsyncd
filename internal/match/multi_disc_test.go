package match

import (
	"slices"
	"testing"
)

const multiDiscReason = "candidate is one disc of a multi-disc release"

func multiDiscRejected(t *testing.T, releases ...string) bool {
	t.Helper()
	candidate := scoredCandidate()
	candidate.ReleaseNames = releases
	return slices.Contains(Evaluate(scoredMedia(), candidate, "en").RejectedReasons, multiDiscReason)
}

func TestOneDiscOfAMultiDiscReleaseIsRejected(t *testing.T) {
	for _, releases := range [][]string{
		{"Gods and Generals (2003)", "Gods and Generals CD1"},
		{"Gods and Generals (2003)", "Gods and Generals (2003) (CD 2)"},
		{"Gods.and.Generals.2003.DVDRip.XviD-CD1"},
		{"DVDRip.XviD 2cd"},
		{"Film.2003.DVDRip.2CDs-GRP"},
	} {
		if !multiDiscRejected(t, releases...) {
			t.Errorf("releases %q: want the multi-disc rejection", releases)
		}
	}
}

func TestSingleFileReleasesAreNotTakenForDiscs(t *testing.T) {
	for _, releases := range [][]string{
		{"Dune.Part.Two.2024.2160p.WEB-DL-GRP"},
		{"Gods and Generals (2003)"},
		{"Film.2003.1080p.BluRay.DTS-HD.MA.5.1-CDDHD"},
		{"Kill.Bill.Vol.1.2003.1080p.BluRay-GRP"},
		{"Film.2003.CDR.Edition.1080p-GRP"},
	} {
		if multiDiscRejected(t, releases...) {
			t.Errorf("releases %q: unexpected multi-disc rejection", releases)
		}
	}
}

func TestExactHashOverridesTheMultiDiscLabel(t *testing.T) {
	candidate := scoredCandidate()
	candidate.ExactHash = true
	candidate.ReleaseNames = []string{"Film CD1"}
	if score := Evaluate(scoredMedia(), candidate, "en"); len(score.RejectedReasons) != 0 {
		t.Fatalf("exact-hash score rejected: %v", score.RejectedReasons)
	}
}
