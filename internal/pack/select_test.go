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

func TestEpisodeRangeRequiresExplicitHyphenatedEndpoint(t *testing.T) {
	accepted := map[string][3]int{
		"Show.S01E01-E03.srt":    {1, 1, 3},
		"Show.S01E01-S01E03.srt": {1, 1, 3},
		"Show.1x01-1x03.srt":     {1, 1, 3},
	}
	for name, want := range accepted {
		season, from, to, found := episodeRange(name)
		if !found || [3]int{season, from, to} != want {
			t.Errorf("episodeRange(%q) = %d,%d,%d,%v", name, season, from, to, found)
		}
	}
	for _, name := range []string{
		"Show.S01E01.1080p.srt",
		"Show.S01E01.2024.srt",
		"Show.S01E01.E03.srt",
		"Show.S01E01_E03.srt",
		"Show.S01E01 E03.srt",
		"Show.S01E03-E01.srt",
		"Show.S01E01-S02E03.srt",
		"Show.S01E01-E03p.srt",
		"Show.S01E01-E03é.srt",
		"Show.1x01-1x03extra.srt",
		"Show.S01E01-E03-S01E05.srt",
		"Show.S01E01-E03.S02E01-E03.srt",
		"Show.S01E01-S02E03.S03E01-E03.srt",
	} {
		if _, _, _, found := episodeRange(name); found {
			t.Errorf("episodeRange(%q) unexpectedly matched", name)
		}
	}

	evidence := memberEvidence("Show.S01E01.1080p.srt", "Show")
	if evidence.EpisodeFrom != 1 || evidence.EpisodeTo != 1 {
		t.Fatalf("memberEvidence() = %#v, want single episode evidence", evidence)
	}
	selected, err := Select(Manifest{Members: []Member{evidence}}, domain.Candidate{}, domain.Media{Season: 1, Episode: 1}, false)
	if err != nil || selected.SelectionRule == "episode_range" {
		t.Fatalf("suffix selection = %#v, %v", selected, err)
	}
}

func TestSelectRejectsInvalidOrAmbiguousRangeEvidenceWithoutTokenFallback(t *testing.T) {
	for _, test := range []struct {
		name   string
		member string
		media  domain.Media
	}{
		{"multiple valid ranges", "Show.S01E01-E03.S02E01-E03.srt", domain.Media{Season: 1, Episode: 1}},
		{"invalid then valid first token", "Show.S01E01-S02E03.S03E01-E03.srt", domain.Media{Season: 1, Episode: 1}},
		{"invalid then valid later range", "Show.S01E01-S02E03.S03E01-E03.srt", domain.Media{Season: 3, Episode: 1}},
	} {
		t.Run(test.name, func(t *testing.T) {
			selected, err := Select(Manifest{Members: []Member{{SafeName: test.member}}}, domain.Candidate{}, test.media, false)
			var selection *SelectionError
			if !errors.As(err, &selection) {
				t.Fatalf("Select() = %#v, %v", selected, err)
			}
			if selected.SelectionRule == "episode_token" {
				t.Fatalf("Select() unexpectedly used episode-token fallback: %#v", selected)
			}
		})
	}
}

func TestSelectRejectsMalformedHyphenatedRangeWithoutTokenFallback(t *testing.T) {
	for _, test := range []struct {
		name   string
		member string
	}{
		{"standard endpoint suffix", "Show.S01E01-E03p.srt"},
		{"x endpoint suffix", "Show.1x01-1x03extra.srt"},
	} {
		t.Run(test.name, func(t *testing.T) {
			selected, err := Select(Manifest{Members: []Member{{SafeName: test.member}}}, domain.Candidate{}, domain.Media{Season: 1, Episode: 1}, false)
			var selection *SelectionError
			if !errors.As(err, &selection) {
				t.Fatalf("Select() = %#v, %v", selected, err)
			}
			if selected.SelectionRule == "episode_token" {
				t.Fatalf("Select() unexpectedly used episode-token fallback: %#v", selected)
			}
		})
	}
}

func TestEpisodeRangeAndSelectionRejectSeparatedOrIncompleteExpressions(t *testing.T) {
	for _, name := range []string{
		"Show.S01E01 E03.srt",
		"Show.S01E01_E03.srt",
		"Show.S01E01.E03.srt",
		"Show.S01E01-E.srt",
		"Show.S01E01-S01E.srt",
		"Show.1x01-1x.srt",
		"Show.S01E01-E03 E05.srt",
		"Show.S01E01-E03-E.srt",
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, _, found := episodeRange(name); found {
				t.Errorf("episodeRange(%q) unexpectedly matched", name)
			}
			manifest := Manifest{Members: []Member{{SafeName: name}}}
			media := domain.Media{Season: 1, Episode: 1}
			for label, selector := range map[string]func(Manifest, domain.Candidate, domain.Media, bool) (Member, error){
				"Select": Select, "SelectSingleEpisode": SelectSingleEpisode,
			} {
				selected, err := selector(manifest, domain.Candidate{}, media, false)
				var selection *SelectionError
				if !errors.As(err, &selection) {
					t.Errorf("%s() = %#v, %v; want rejection", label, selected, err)
				}
			}
		})
	}
}

func TestEpisodeSelectionPreservesExplicitRangesAndReleaseSuffixes(t *testing.T) {
	for name, rule := range map[string]string{
		"Show.S01E01-E03.srt":      "episode_range",
		"Show.S01E01-S01E03.srt":   "episode_range",
		"Show.1x01-1x03.srt":       "episode_range",
		"Show.S01E01.1080p.srt":    "episode_token",
		"Show.S01E01-Extended.srt": "episode_token",
		"Show.S01E01_-1080p.srt":   "episode_token",
	} {
		for _, selector := range []func(Manifest, domain.Candidate, domain.Media, bool) (Member, error){Select, SelectSingleEpisode} {
			selected, err := selector(Manifest{Members: []Member{{SafeName: name}}}, domain.Candidate{}, domain.Media{Season: 1, Episode: 1}, false)
			if err != nil || selected.SelectionRule != rule {
				t.Errorf("selection(%q) = %#v, %v; want %s", name, selected, err, rule)
			}
		}
	}
}

func TestEpisodeRangeAndSelectionRejectWhitespaceContinuations(t *testing.T) {
	for label, separator := range map[string]string{"tab": "\t", "nbsp": "\u00a0", "em space": "\u2003"} {
		t.Run(label, func(t *testing.T) {
			for _, expression := range []string{
				"S01E01" + separator + "E03",
				"S01E01" + separator + "E",
				"S01E01-" + separator + "S01E",
				"1x01-" + separator + "1x",
				"S01E01-E03" + separator + "E05",
				"S01E01-S01E03" + separator + "S01E",
				"1x01-1x03" + separator + "1x05",
			} {
				t.Run(expression, func(t *testing.T) {
					name := "Show." + expression + ".srt"
					if _, _, _, found := episodeRange(name); found {
						t.Errorf("episodeRange(%q) unexpectedly matched", name)
					}
					for label, selector := range map[string]func(Manifest, domain.Candidate, domain.Media, bool) (Member, error){
						"Select": Select, "SelectSingleEpisode": SelectSingleEpisode,
					} {
						selected, err := selector(Manifest{Members: []Member{{SafeName: name}}}, domain.Candidate{}, domain.Media{Season: 1, Episode: 1}, false)
						var selection *SelectionError
						if !errors.As(err, &selection) {
							t.Errorf("%s(%q) = %#v, %v; want rejection", label, name, selected, err)
						}
					}
				})
			}
		})
	}
}

func TestEpisodeSelectionPreservesWhitespaceReleaseSuffixes(t *testing.T) {
	for _, separator := range []string{"\t", "\u00a0", "\u2003"} {
		for _, suffix := range []string{"1080p", "Extended", "Édition", "S01Extras", "1xSpeed"} {
			for expression, rule := range map[string]string{
				"S01E01": "episode_token", "S01E01-E03": "episode_range", "S01E01-S01E03": "episode_range", "1x01-1x03": "episode_range",
			} {
				name := "Show." + expression + separator + suffix + ".srt"
				for _, selector := range []func(Manifest, domain.Candidate, domain.Media, bool) (Member, error){Select, SelectSingleEpisode} {
					selected, err := selector(Manifest{Members: []Member{{SafeName: name}}}, domain.Candidate{}, domain.Media{Season: 1, Episode: 1}, false)
					if err != nil || selected.SelectionRule != rule {
						t.Errorf("selection(%q) = %#v, %v; want %s", name, selected, err, rule)
					}
				}
			}
		}
	}
}

func TestSelectSingleMovieEnforcesForcedPolicy(t *testing.T) {
	manifest := Manifest{ArchiveType: "plain", Members: []Member{{SafeName: "Movie.forced.srt", Forced: true}}}
	_, err := SelectSingleMovie(manifest, domain.Candidate{Forced: true}, false)
	var selection *SelectionError
	if !errors.As(err, &selection) || selection.Rule != "forced_policy" {
		t.Fatalf("error = %T %v", err, err)
	}

	selected, err := SelectSingleMovie(Manifest{ArchiveType: "plain", Members: []Member{{SafeName: "Movie.srt"}}}, domain.Candidate{}, false)
	if err != nil || selected.SelectionRule != "single_movie" {
		t.Fatalf("selection = %#v, %v", selected, err)
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
