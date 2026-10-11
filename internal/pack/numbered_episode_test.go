package pack

import (
	"testing"

	"subsyncd/internal/domain"
)

func TestSelectReadsAnEpisodeTitleStartingWithELetterAsATitle(t *testing.T) {
	media := domain.Media{Title: "The Terminal List: Dark Wolf", Season: 1, Episode: 5}
	selected, err := Select(Manifest{Members: []Member{{SafeName: "The Terminal List Dark Wolf S01E05 - E and E.srt"}}}, domain.Candidate{}, media, false)
	if err != nil || selected.SelectionRule != "episode_token" {
		t.Fatalf("Select() = %#v, %v; want the S01E05 file", selected, err)
	}
	for _, name := range []string{"Show.S01E05-E.srt", "Show S01E05 - E.srt", "Show S01E05 - E06.srt"} {
		if !hasInvalidOrAmbiguousRangeEvidence(name) {
			t.Errorf("%q is no longer treated as an incomplete or spaced range", name)
		}
	}
}

func TestSelectNumberedEpisodesOfASingleSeason(t *testing.T) {
	tests := []struct {
		name    string
		members []string
		episode int
		want    string
	}{
		{"season-episode prefix", []string{"1-01 Generation Kill - Get Some.srt", "1-02 Generation Kill - The Cradle of Civilization.srt", "1-03 Generation Kill - Screwby.srt"}, 3, "1-03 Generation Kill - Screwby.srt"},
		{"part number", []string{"The.Night.Of.Part.1.720p.HDTV.x264-BATV.srt", "The.Night.Of.Part.2.720p.HDTV.x264-BATV.srt", "The.Night.Of.Part.3.720p.HDTV.x264-BATV.srt"}, 2, "The.Night.Of.Part.2.720p.HDTV.x264-BATV.srt"},
		{"leading number in a folder", []string{"Planet Earth II/1 - Islands.srt", "Planet Earth II/2 - Mountains.srt", "Planet Earth II/3 - Jungles.srt"}, 3, "Planet Earth II/3 - Jungles.srt"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var members []Member
			for _, name := range test.members {
				members = append(members, Member{SafeName: name})
			}
			selected, err := Select(Manifest{Members: members}, domain.Candidate{}, domain.Media{Season: 1, Episode: test.episode}, false)
			if err != nil || selected.SafeName != test.want || selected.SelectionRule != "numbered_episode" {
				t.Fatalf("Select() = %#v, %v; want %q", selected, err, test.want)
			}
		})
	}
}

func TestSelectNumberedEpisodesStaysStrict(t *testing.T) {
	parts := []Member{{SafeName: "Show.Part.1.srt"}, {SafeName: "Show.Part.2.srt"}, {SafeName: "Show.Part.3.srt"}}
	if _, err := Select(Manifest{Members: parts}, domain.Candidate{}, domain.Media{Season: 2, Episode: 2}, false); err == nil {
		t.Fatal("a Part number was taken as an episode outside season 1")
	}
	lone := []Member{{SafeName: "Show.Part.1.srt"}, {SafeName: "Show.Part.2.srt"}}
	if _, err := Select(Manifest{Members: lone}, domain.Candidate{}, domain.Media{Season: 1, Episode: 2}, false); err == nil {
		t.Fatal("a lone numbered file was taken as a numbered set")
	}
	wrongSeason := []Member{{SafeName: "2-01 Show - One.srt"}, {SafeName: "2-02 Show - Two.srt"}, {SafeName: "2-03 Show - Three.srt"}}
	if _, err := Select(Manifest{Members: wrongSeason}, domain.Candidate{}, domain.Media{Season: 1, Episode: 2}, false); err == nil {
		t.Fatal("a 2-02 prefix was taken as season 1")
	}
	tokens := []Member{{SafeName: "Show.S01E01.Part.2.srt"}, {SafeName: "Show.S01E02.Part.1.srt"}}
	selected, err := Select(Manifest{Members: tokens}, domain.Candidate{}, domain.Media{Season: 1, Episode: 2}, false)
	if err != nil || selected.SafeName != "Show.S01E02.Part.1.srt" {
		t.Fatalf("Select() = %#v, %v; an explicit SxxEyy token must win over a Part number", selected, err)
	}
}

func TestSelectNumberedEpisodesNeedsAtLeastThreeNumberedFiles(t *testing.T) {
	twoParts := []Member{{SafeName: "Show - Pilot Part 1.srt"}, {SafeName: "Show - Pilot Part 2.srt"}}
	if _, err := Select(Manifest{Members: twoParts}, domain.Candidate{}, domain.Media{Season: 1, Episode: 2}, false); err == nil {
		t.Fatal("the halves of a two-part episode were taken as episodes 1 and 2")
	}
}
