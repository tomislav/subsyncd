package pack

import (
	"errors"
	"testing"

	"subsyncd/internal/domain"
)

func TestSelectUsesStrictRulesInOrder(t *testing.T) {
	media := domain.Media{Title: "Example Show", Season: 1, Episode: 2, AbsoluteEpisode: 102}
	tests := []struct {
		name      string
		candidate domain.Candidate
		members   []Member
		want      string
		wantRule  string
	}{
		{"direct", domain.Candidate{Pack: &domain.PackInfo{DirectMembers: []domain.PackMemberRef{{ID: "direct", Filename: "opaque.srt", Season: 1, Episode: 2}}}}, []Member{{SafeName: "opaque.srt"}, {SafeName: "Show.S01E02.srt"}}, "opaque.srt", "provider_direct"},
		{"sxe", domain.Candidate{}, []Member{{SafeName: "Show.S01E01.srt"}, {SafeName: "Show.S01E02.srt"}}, "Show.S01E02.srt", "episode_token"},
		{"x", domain.Candidate{}, []Member{{SafeName: "Show.01x02.ass"}}, "Show.01x02.ass", "episode_token"},
		{"range", domain.Candidate{}, []Member{{SafeName: "Show.S01E01-E03.srt"}}, "Show.S01E01-E03.srt", "episode_range"},
		{"absolute", domain.Candidate{}, []Member{{SafeName: "Show.EP101.srt"}, {SafeName: "Show.EP102.srt"}}, "Show.EP102.srt", "absolute_episode"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			selected, err := Select(Manifest{Members: test.members}, test.candidate, media, false)
			if err != nil || selected.SafeName != test.want || selected.SelectionRule != test.wantRule {
				t.Fatalf("Select() = %#v, %v", selected, err)
			}
		})
	}
}

func TestSelectDoesNotMisclassifyRangeStartAsSingleEpisode(t *testing.T) {
	media := domain.Media{Season: 1, Episode: 1}
	selected, err := Select(Manifest{Members: []Member{{SafeName: "Show.S01E01-E03.srt"}}}, domain.Candidate{}, media, false)
	if err != nil || selected.SelectionRule != "episode_range" {
		t.Fatalf("range-start selection = %#v, %v", selected, err)
	}
}

func TestSelectDeduplicatesRepeatedProviderDirectEvidence(t *testing.T) {
	media := domain.Media{Season: 1, Episode: 2}
	direct := domain.PackMemberRef{Filename: "episode.srt", Season: 1, Episode: 2}
	candidate := domain.Candidate{Pack: &domain.PackInfo{DirectMembers: []domain.PackMemberRef{direct, direct}}}
	selected, err := Select(Manifest{Members: []Member{{SafeName: "episode.srt"}}}, candidate, media, false)
	if err != nil || selected.SafeName != "episode.srt" {
		t.Fatalf("direct selection = %#v, %v", selected, err)
	}
}

func TestSelectExcludesForcedAndFailsClosedOnAmbiguityOrWrongEvidence(t *testing.T) {
	media := domain.Media{Title: "Example Show", Season: 1, Episode: 2}
	tests := []struct {
		name    string
		members []Member
	}{
		{"forced only", []Member{{SafeName: "Show.S01E02.forced.srt", Forced: true}}},
		{"duplicate", []Member{{SafeName: "a.S01E02.srt"}, {SafeName: "b.S01E02.srt"}}},
		{"wrong season", []Member{{SafeName: "Show.S02E02.srt"}}},
		{"wrong episode", []Member{{SafeName: "Show.S01E03.srt"}}},
		{"no evidence", []Member{{SafeName: "subtitle.srt"}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := Select(Manifest{Members: test.members}, domain.Candidate{}, media, false)
			var rejection *SelectionError
			if !errors.As(err, &rejection) {
				t.Fatalf("error = %T %v", err, err)
			}
		})
	}
}

func TestSelectUsesEpisodeTitleOnlyAtStrictSimilarityThreshold(t *testing.T) {
	exact := domain.Media{Season: 1, Episode: 2, EpisodeTitle: "A Great Adventure"}
	selected, err := Select(Manifest{Members: []Member{{SafeName: "unknown.srt", NormalizedTitle: "a great adventure"}}}, domain.Candidate{}, exact, false)
	if err != nil || selected.SelectionRule != "episode_title" {
		t.Fatalf("exact title selection = %#v, %v", selected, err)
	}

	acceptedTitle := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	acceptedMember := "baaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	selected, err = Select(Manifest{Members: []Member{{SafeName: "accepted.srt", NormalizedTitle: acceptedMember}}}, domain.Candidate{}, domain.Media{EpisodeTitle: acceptedTitle}, false)
	if err != nil || selected.SelectionRule != "episode_title" {
		t.Fatalf("0.98 title selection = %#v, %v", selected, err)
	}

	rejectedTitle := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	rejectedMember := "baaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if _, err := Select(Manifest{Members: []Member{{SafeName: "rejected.srt", NormalizedTitle: rejectedMember}}}, domain.Candidate{}, domain.Media{EpisodeTitle: rejectedTitle}, false); err == nil {
		t.Fatal("similarity below 0.98 should be rejected")
	}
	if _, err := Select(Manifest{Members: []Member{{SafeName: "short.srt", NormalizedTitle: "lost"}}}, domain.Candidate{}, domain.Media{EpisodeTitle: "Lost"}, false); err == nil {
		t.Fatal("short episode titles should not be selected")
	}
}
