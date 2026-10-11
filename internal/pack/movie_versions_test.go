package pack

import (
	"errors"
	"strings"
	"testing"

	"subsyncd/internal/domain"
)

func movieManifest(names ...string) Manifest {
	var members []Member
	for _, name := range names {
		members = append(members, Member{SafeName: name, Checksum: "sum-" + name})
	}
	return Manifest{ArchiveType: "zip", Members: members}
}

func selectedNames(members []Member) string {
	var names []string
	for _, member := range members {
		names = append(names, member.SafeName)
	}
	return strings.Join(names, ",")
}

func TestSelectMovieAlternativesDropsCommentaryAndShorts(t *testing.T) {
	manifest := movieManifest("Pixar.Short.Burn-E.2008.720p.BluRay.x264-ESiR.srt", "WALL-E.2008.720p.BluRay.DTS-ES.x264-ESiR - Commentary.srt", "WALL-E.2008.720p.BluRay.DTS-ES.x264-ESiR.srt")
	members, err := SelectMovieAlternatives(manifest, domain.Candidate{Title: "WALL·E", Language: "hr"}, false)
	if err != nil || selectedNames(members) != "WALL-E.2008.720p.BluRay.DTS-ES.x264-ESiR.srt" || members[0].SelectionRule != "single_movie" {
		t.Fatalf("members = %v, %v", selectedNames(members), err)
	}
}

func TestSelectMovieAlternativesKeepsATitleThatSoundsLikeAnExtra(t *testing.T) {
	members, err := SelectMovieAlternatives(movieManifest("Short.Circuit.1986.srt"), domain.Candidate{Title: "Short Circuit", Language: "en"}, false)
	if err != nil || selectedNames(members) != "Short.Circuit.1986.srt" {
		t.Fatalf("members = %v, %v", selectedNames(members), err)
	}
}

func TestSelectMovieAlternativesReturnsUpToThreeVersions(t *testing.T) {
	manifest := movieManifest("Watchmen.2009.DVDRip.XviD-DrSi.srt", "Watchmen.2009.iNTERNAL.BDRip.x264-MARS.srt")
	members, err := SelectMovieAlternatives(manifest, domain.Candidate{Title: "Watchmen", Language: "hr"}, false)
	if err != nil || len(members) != 2 || members[0].SelectionRule != "movie_versions" {
		t.Fatalf("members = %v, %v; want both versions for LAPSE to rank", selectedNames(members), err)
	}
	four := movieManifest("A.DVDRip.srt", "A.BDRip.srt", "A.WEB-DL.srt", "A.HDTV.srt")
	if _, err := SelectMovieAlternatives(four, domain.Candidate{Title: "A", Language: "hr"}, false); err == nil {
		t.Fatal("four versions were accepted")
	}
}

func TestSelectMovieAlternativesKeepsTheRequestedLanguage(t *testing.T) {
	members, err := SelectMovieAlternatives(movieManifest("Movie.EN.srt", "Movie.HR.srt"), domain.Candidate{Title: "Movie", Language: "hr"}, false)
	if err != nil || selectedNames(members) != "Movie.HR.srt" {
		t.Fatalf("members = %v, %v; want only the Croatian file", selectedNames(members), err)
	}
}

func TestSelectMovieAlternativesRejectsDiscSplits(t *testing.T) {
	for _, manifest := range []Manifest{
		movieManifest("The Town (Extended) (2010) CD01.srt", "The Town (Extended) (2010) CD02.srt"),
		movieManifest("Troy.EN.DC.CD1.srt", "Troy.EN.DC.CD2.srt", "Troy.HR.CD1.srt", "Troy.HR.CD2.srt"),
	} {
		_, err := SelectMovieAlternatives(manifest, domain.Candidate{Title: "The Town", Language: "hr"}, false)
		var selection *SelectionError
		if !errors.As(err, &selection) {
			t.Errorf("%v: error = %v; want a selection rejection", selectedNames(manifest.Members), err)
		}
	}
}
