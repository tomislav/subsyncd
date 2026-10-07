package pack

import (
	"testing"

	"subsyncd/internal/domain"
)

func rangeTarget() domain.Media {
	return domain.Media{Ref: domain.MediaRef{Kind: domain.MediaEpisode}, Title: "Mad Men", EpisodeTitle: "The Doorway", Season: 6, Episode: 1, EpisodeEnd: 2}
}

func TestSelectForRangeTargetRequiresAMemberCoveringTheRange(t *testing.T) {
	media := rangeTarget()
	selected, err := Select(Manifest{Members: []Member{{SafeName: "Mad.Men.S06E01.srt"}, {SafeName: "Mad.Men.S06E02.srt"}, {SafeName: "Mad.Men.S06E01-E02.srt"}}}, domain.Candidate{}, media, false)
	if err != nil || selected.SafeName != "Mad.Men.S06E01-E02.srt" || selected.SelectionRule != "episode_range" {
		t.Fatalf("Select() = %#v, %v; want the S06E01-E02 member", selected, err)
	}
	for name, members := range map[string][]Member{
		"separate episodes only":         {{SafeName: "Mad.Men.S06E01.srt"}, {SafeName: "Mad.Men.S06E02.srt"}},
		"range missing the last episode": {{SafeName: "Mad.Men.S06E00-E01.srt"}},
		"episode title only":             {{SafeName: "The Doorway.srt", NormalizedTitle: "the doorway"}},
		"provider direct member":         nil,
	} {
		t.Run(name, func(t *testing.T) {
			candidate := domain.Candidate{}
			if members == nil {
				members = []Member{{SafeName: "opaque.srt"}}
				candidate.Pack = &domain.PackInfo{DirectMembers: []domain.PackMemberRef{{ID: "direct", Filename: "opaque.srt", Season: 6, Episode: 1}}}
			}
			if selected, err := Select(Manifest{Members: members}, candidate, media, false); err == nil {
				t.Fatalf("Select() = %#v, want a selection error", selected)
			}
		})
	}
}

func TestSelectSingleEpisodeForRangeTargetAcceptsANonContradictingSingleFile(t *testing.T) {
	media := rangeTarget()
	for _, name := range []string{"Mad Men - 06x01.02 - The Doorway.srt", "mad.men.s06e01.the.doorway.srt", "subtitle.srt"} {
		selected, err := SelectSingleEpisode(Manifest{Members: []Member{{SafeName: name}}}, domain.Candidate{}, media, false)
		if err != nil || selected.SafeName != name {
			t.Errorf("SelectSingleEpisode(%q) = %#v, %v; want it selected", name, selected, err)
		}
	}
	for _, name := range []string{"Mad.Men.S07E01.srt", "Mad.Men.S06E03.srt", "Mad.Men.S06E01-E02.and.S07E05.srt"} {
		if selected, err := SelectSingleEpisode(Manifest{Members: []Member{{SafeName: name}}}, domain.Candidate{}, media, false); err == nil {
			t.Errorf("SelectSingleEpisode(%q) = %#v, want an error for contradicting evidence", name, selected)
		}
	}
	// A pack candidate never falls back to a single member.
	pack := domain.Candidate{Pack: &domain.PackInfo{Scope: domain.PackSeason, Season: 6}}
	if selected, err := SelectSingleEpisode(Manifest{Members: []Member{{SafeName: "mad.men.s06e01.srt"}}}, pack, media, false); err == nil {
		t.Errorf("pack fallback selected %#v", selected)
	}
}
