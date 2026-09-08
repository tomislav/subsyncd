package match

import (
	"reflect"
	"testing"

	"subsyncd/internal/domain"
)

func TestSelectedPackMemberPreservesWeightsAndOtherIdentityGates(t *testing.T) {
	media := domain.Media{Ref: domain.MediaRef{Kind: domain.MediaEpisode}, Title: "Show", Year: 2024, Season: 2, Episode: 9}
	candidate := domain.Candidate{Kind: domain.MediaEpisode, Language: "hr", Title: "Show", Year: 2024, Season: 2, Episode: 9, Rating: 0.6}
	want := Evaluate(media, candidate, "hr")
	if want.Total != 37 {
		t.Fatalf("fixture score = %d", want.Total)
	}
	media.Episode = 10
	if Eligible(Evaluate(media, candidate, "hr"), 35) {
		t.Fatal("ordinary wrong episode accepted")
	}
	if got := EvaluateSelectedPackMember(media, candidate, "hr"); !reflect.DeepEqual(got, want) {
		t.Fatalf("score changed: %+v vs %+v", got, want)
	}
	for _, test := range []struct {
		name   string
		change func(*domain.Candidate)
	}{
		{"season", func(c *domain.Candidate) { c.Season = 3 }},
		{"language", func(c *domain.Candidate) { c.Language = "en" }},
		{"kind", func(c *domain.Candidate) { c.Kind = domain.MediaMovie }},
		{"forced", func(c *domain.Candidate) { c.Forced = true }},
		{"external_id", func(c *domain.Candidate) { c.ExternalIDs.IMDb = "ttother" }},
		{"exact_hash", func(c *domain.Candidate) { c.ExactHash = true }},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := candidate
			m := media
			m.ExternalIDs.IMDb = "ttshow"
			test.change(&c)
			if Eligible(EvaluateSelectedPackMember(m, c, "hr"), 35) {
				t.Fatal("identity restriction bypassed")
			}
		})
	}
}
