package match

import (
	"testing"

	"subsyncd/internal/domain"
)

func TestEvaluateAwardsEveryDocumentedWeight(t *testing.T) {
	media := scoredMedia()
	candidate := scoredCandidate()
	score := Evaluate(media, candidate, "en")
	if len(score.RejectedReasons) != 0 || score.Total != 100 {
		t.Fatalf("score = %#v", score)
	}
	want := map[string]int{"external_id": 20, "title_year": 15, "episode": 20, "release_group": 25, "source": 15, "edition": 10, "streaming_service": 5, "resolution": 5, "provider_rating": 3, "popularity": 2}
	for _, contribution := range score.Contributions {
		if points, ok := want[contribution.Signal]; ok {
			if contribution.Points != points {
				t.Errorf("%s = %d, want %d", contribution.Signal, contribution.Points, points)
			}
			delete(want, contribution.Signal)
		}
	}
	if len(want) != 0 {
		t.Fatalf("missing contributions: %#v", want)
	}
}

func TestEvaluateAwardsEpisodeOrContainingPackEvidence(t *testing.T) {
	media := scoredMedia()
	base := domain.Candidate{Language: "en", Kind: domain.MediaEpisode, Title: media.Title, Year: media.Year, Season: media.Season}
	tests := []struct {
		name      string
		candidate domain.Candidate
		want      int
	}{
		{name: "exact episode", candidate: func() domain.Candidate { candidate := base; candidate.Episode = media.Episode; return candidate }(), want: 20},
		{name: "parsed multi-episode release", candidate: func() domain.Candidate {
			candidate := base
			candidate.ReleaseNames = []string{"Example.Show.S01E01-E03.1080p.WEB-DL-GROUP"}
			return candidate
		}(), want: 20},
		{name: "containing season pack", candidate: func() domain.Candidate {
			candidate := base
			candidate.Pack = &domain.PackInfo{Scope: domain.PackSeason, Season: media.Season}
			return candidate
		}(), want: 20},
		{name: "containing range pack", candidate: func() domain.Candidate {
			candidate := base
			candidate.Pack = &domain.PackInfo{Scope: domain.PackRange, Season: media.Season, EpisodeFrom: 1, EpisodeTo: 3}
			return candidate
		}(), want: 20},
		{name: "no episode evidence", candidate: base, want: 0},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			score := Evaluate(media, test.candidate, "en")
			for _, contribution := range score.Contributions {
				if contribution.Signal == "episode" {
					if contribution.Points != test.want {
						t.Fatalf("episode contribution = %#v, want %d", contribution, test.want)
					}
					return
				}
			}
			t.Fatal("episode contribution missing")
		})
	}
}

func TestEvaluateHardGatesContradictoryIdentity(t *testing.T) {
	baseMedia := scoredMedia()
	baseCandidate := scoredCandidate()
	tests := []struct {
		name   string
		mutate func(*domain.Candidate)
	}{
		{"language", func(c *domain.Candidate) { c.Language = "hr" }},
		{"kind", func(c *domain.Candidate) { c.Kind = domain.MediaMovie }},
		{"forced only", func(c *domain.Candidate) { c.Forced = true }},
		{"external id", func(c *domain.Candidate) { c.ExternalIDs.IMDb = "tt999" }},
		{"season", func(c *domain.Candidate) { c.Season = 2 }},
		{"episode", func(c *domain.Candidate) { c.Episode = 3 }},
		{"edition", func(c *domain.Candidate) {
			c.ReleaseNames = []string{"Example.Show.S01E02.DIRECTORS.CUT.1080p.WEB-DL-GROUP"}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := baseCandidate
			test.mutate(&candidate)
			score := Evaluate(baseMedia, candidate, "en")
			if len(score.RejectedReasons) == 0 || score.Total != 0 {
				t.Fatalf("score = %#v, want rejection", score)
			}
		})
	}
}

func TestExactHashOverridesConflictingEditionLabel(t *testing.T) {
	media := scoredMedia()
	candidate := scoredCandidate()
	candidate.ExactHash = true
	candidate.ReleaseNames = []string{"Example.Show.S01E02.DIRECTORS.CUT.1080p.WEB-DL-GROUP"}
	score := Evaluate(media, candidate, "en")
	if len(score.RejectedReasons) != 0 || score.Total != 100 || len(score.Contributions) != 1 || score.Contributions[0].Signal != "exact_hash" {
		t.Fatalf("exact-hash score = %#v", score)
	}
}

func TestUnknownCandidateEditionRemainsEligibleWithoutEditionPoints(t *testing.T) {
	media := scoredMedia()
	candidate := scoredCandidate()
	candidate.ReleaseNames = []string{"Example.Show.S01E02.1080p.NF.WEB-DL-GROUP"}
	score := Evaluate(media, candidate, "en")
	if len(score.RejectedReasons) != 0 {
		t.Fatalf("unknown edition rejected: %#v", score)
	}
	found := false
	for _, contribution := range score.Contributions {
		if contribution.Signal == "edition" {
			found = true
			if contribution.Points != 0 {
				t.Fatalf("unknown edition contribution = %#v", contribution)
			}
		}
	}
	if !found {
		t.Fatal("edition contribution missing")
	}
}

func TestEvaluateAllowsExplicitContainingPackAndRejectsOtherMovieYear(t *testing.T) {
	media := scoredMedia()
	candidate := scoredCandidate()
	candidate.Episode = 0
	candidate.Pack = &domain.PackInfo{Scope: domain.PackRange, Season: 1, EpisodeFrom: 1, EpisodeTo: 10}
	if score := Evaluate(media, candidate, "en"); len(score.RejectedReasons) != 0 {
		t.Fatalf("containing pack rejected: %#v", score)
	}

	movie := media
	movie.Ref.Kind = domain.MediaMovie
	movie.Season, movie.Episode = 0, 0
	movie.ExternalIDs = domain.ExternalIDs{}
	movie.Year = 2020
	candidate.Kind = domain.MediaMovie
	candidate.Season, candidate.Episode = 0, 0
	candidate.Pack = nil
	candidate.ExternalIDs = domain.ExternalIDs{}
	candidate.Year = 2023
	if score := Evaluate(movie, candidate, "en"); len(score.RejectedReasons) == 0 {
		t.Fatalf("wrong movie year accepted: %#v", score)
	}
}

func TestEvaluateRejectsPackWhoseOwnSeasonConflicts(t *testing.T) {
	candidate := scoredCandidate()
	candidate.Season = 0
	candidate.Episode = 0
	candidate.Pack = &domain.PackInfo{Scope: domain.PackRange, Season: 2, EpisodeFrom: 1, EpisodeTo: 10}
	if score := Evaluate(scoredMedia(), candidate, "en"); len(score.RejectedReasons) == 0 {
		t.Fatalf("wrong-season pack accepted: %#v", score)
	}
}

func TestExactHashIsTerminalAndThresholdIsExplicit(t *testing.T) {
	candidate := scoredCandidate()
	candidate.ExactHash = true
	score := Evaluate(scoredMedia(), candidate, "en")
	if score.Total != 100 || len(score.Contributions) != 1 || score.Contributions[0].Signal != "exact_hash" {
		t.Fatalf("exact score = %#v", score)
	}
	candidate.ExactHash = false
	candidate.ExternalIDs = domain.ExternalIDs{}
	candidate.ReleaseNames = nil
	candidate.Rating, candidate.Popularity = 0, 0
	score = Evaluate(scoredMedia(), candidate, "en")
	if !Eligible(score, 35) || Eligible(score, 36) || score.Total != 35 {
		t.Fatalf("identity-baseline score = %#v", score)
	}
}

func TestKnownReleaseGroupAliasesMatch(t *testing.T) {
	media := scoredMedia()
	media.ReleaseGroup = "LOL"
	candidate := scoredCandidate()
	candidate.ReleaseNames = []string{"Example.Show.S01E02.1080p.NF.WEB-DL.EXTENDED-DIMENSION"}
	score := Evaluate(media, candidate, "en")
	for _, contribution := range score.Contributions {
		if contribution.Signal == "release_group" {
			if contribution.Points != 25 {
				t.Fatalf("release group contribution = %#v", contribution)
			}
			return
		}
	}
	t.Fatal("release group contribution missing")
}

func TestRankUsesDocumentedStableTieBreaks(t *testing.T) {
	items := []EvaluatedCandidate{
		{Candidate: domain.Candidate{ProviderID: "b", ResultID: "2", Rating: .9, Popularity: .8}, Score: domain.Score{Total: 50}, ProviderPriority: 1},
		{Candidate: domain.Candidate{ProviderID: "a", ResultID: "3", Rating: .1, Popularity: .1}, Score: domain.Score{Total: 51}, ProviderPriority: 2},
		{Candidate: domain.Candidate{ProviderID: "a", ResultID: "1", Rating: .5, Popularity: .9}, Score: domain.Score{Total: 50}, ProviderPriority: 1},
		{Candidate: domain.Candidate{ProviderID: "a", ResultID: "0", Rating: .5, Popularity: .9}, Score: domain.Score{Total: 50}, ProviderPriority: 1},
	}
	Rank(items)
	want := []string{"3", "2", "0", "1"}
	for index := range want {
		if items[index].Candidate.ResultID != want[index] {
			t.Fatalf("ranked = %#v", items)
		}
	}
}

func scoredMedia() domain.Media {
	return domain.Media{Ref: domain.MediaRef{Kind: domain.MediaEpisode}, Title: "Example Show", Year: 2024, Season: 1, Episode: 2, ExternalIDs: domain.ExternalIDs{IMDb: "tt123", TMDB: 456}, ReleaseName: "Example.Show.S01E02.1080p.NF.WEB-DL.EXTENDED-GROUP", ReleaseGroup: "GROUP", Source: "WEB-DL", Resolution: "1080p", StreamingService: "Netflix", Edition: "Extended"}
}

func scoredCandidate() domain.Candidate {
	return domain.Candidate{ProviderID: "provider", ResultID: "one", Language: "en", Kind: domain.MediaEpisode, Title: "Example Show", Year: 2024, Season: 1, Episode: 2, ExternalIDs: domain.ExternalIDs{IMDb: "tt123", TMDB: 456}, ReleaseNames: []string{"Example.Show.S01E02.1080p.NF.WEB-DL.EXTENDED-GROUP"}, Rating: 1, Popularity: 1}
}
