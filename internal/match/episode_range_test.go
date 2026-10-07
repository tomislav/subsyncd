package match

import (
	"strings"
	"testing"

	"subsyncd/internal/domain"
)

func rangeMedia() domain.Media {
	media := scoredMedia()
	media.Season, media.Episode, media.EpisodeEnd = 6, 1, 2
	media.AbsoluteEpisode, media.AbsoluteEpisodeEnd = 0, 0
	return media
}

func TestRangeTargetRequiresWholeRangeCoverage(t *testing.T) {
	media := rangeMedia()
	base := domain.Candidate{Language: "en", Kind: domain.MediaEpisode, Title: media.Title, Year: media.Year, Season: 6, Episode: 1}
	with := func(edit func(*domain.Candidate)) domain.Candidate {
		candidate := base
		edit(&candidate)
		return candidate
	}
	for _, test := range []struct {
		name      string
		candidate domain.Candidate
		covers    bool
	}{
		{name: "release name range", candidate: with(func(c *domain.Candidate) { c.ReleaseNames = []string{"Mad.Men.S06E01-E02.PROPER.HDTV.x264-2HD"} }), covers: true},
		{name: "SSxEE.EE release name", candidate: with(func(c *domain.Candidate) { c.ReleaseNames = []string{"Mad Men - 06x01.02 - The Doorway"} }), covers: true},
		{name: "wider release range", candidate: with(func(c *domain.Candidate) { c.ReleaseNames = []string{"Mad.Men.S06E01-E03.WEB"} }), covers: true},
		{name: "range pack", candidate: with(func(c *domain.Candidate) {
			c.Episode = 0
			c.Pack = &domain.PackInfo{Scope: domain.PackRange, Season: 6, EpisodeFrom: 1, EpisodeTo: 2}
		}), covers: true},
		{name: "season pack (member selection enforces the range)", candidate: with(func(c *domain.Candidate) {
			c.Episode = 0
			c.Pack = &domain.PackInfo{Scope: domain.PackSeason, Season: 6}
		}), covers: true},
		{name: "exact hash", candidate: with(func(c *domain.Candidate) { c.ExactHash = true }), covers: true},
		{name: "first episode only", candidate: with(func(c *domain.Candidate) {
			c.ReleaseNames = []string{"mad.men.s06e01 The Doorway.1080p.web-dl.h264-nts"}
		}), covers: false},
		{name: "no release evidence", candidate: base, covers: false},
		{name: "range starting too late", candidate: with(func(c *domain.Candidate) { c.ReleaseNames = []string{"Mad.Men.S06E02-E03.WEB"} }), covers: false},
		{name: "range pack missing the last episode", candidate: with(func(c *domain.Candidate) {
			c.Episode = 0
			c.Pack = &domain.PackInfo{Scope: domain.PackRange, Season: 6, EpisodeFrom: 1, EpisodeTo: 1}
		}), covers: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			score := Evaluate(media, test.candidate, "en")
			rejected := false
			for _, reason := range score.RejectedReasons {
				rejected = rejected || strings.Contains(reason, "does not cover target")
			}
			if test.covers && (len(score.RejectedReasons) != 0 || score.Total == 0) {
				t.Fatalf("covering candidate rejected: %+v", score)
			}
			if !test.covers && !rejected {
				t.Fatalf("partial candidate not rejected for coverage: %+v", score)
			}
		})
	}
}

func TestRangeCoverageDoesNotChangeSingleEpisodeTargets(t *testing.T) {
	media := scoredMedia() // single episode
	candidate := domain.Candidate{Language: "en", Kind: domain.MediaEpisode, Title: media.Title, Year: media.Year, Season: media.Season, Episode: media.Episode}
	if score := Evaluate(media, candidate, "en"); len(score.RejectedReasons) != 0 {
		t.Fatalf("single-episode candidate rejected: %+v", score.RejectedReasons)
	}
}

func TestParseReleaseSeasonCrossEpisodeDotRange(t *testing.T) {
	for raw, want := range map[string][3]int{
		"Mad Men - 06x01.02 - The Doorway": {6, 1, 2},
		"Show 1x09.10 Finale":              {1, 9, 10},
	} {
		got := ParseRelease(raw)
		if got.Season != want[0] || got.Episode != want[1] || got.EpisodeEnd != want[2] {
			t.Errorf("ParseRelease(%q) = S%d E%d-E%d, want S%d E%d-E%d", raw, got.Season, got.Episode, got.EpisodeEnd, want[0], want[1], want[2])
		}
	}
}
