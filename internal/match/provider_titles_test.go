package match

import (
	"encoding/json"
	"subsyncd/internal/domain"
	"testing"
)

func TestProviderAlternateTitleSuppliesOnlyTitleEvidence(t *testing.T) {
	var candidate domain.Candidate
	if err := json.Unmarshal([]byte(`{"language":"hr","kind":"movie","title":"Original Name","alternate_titles":["English Movie","English Movie"],"year":2024,"release_names":["1080p.WEB-DL"]}`), &candidate); err != nil {
		t.Fatal(err)
	}
	media := domain.Media{Ref: domain.MediaRef{Kind: domain.MediaMovie}, Title: "English Movie", Year: 2024, Source: "WEB-DL", Resolution: "1080p"}
	score := Evaluate(media, candidate, "hr")
	if score.Total != 35 || !Eligible(score, 35) {
		t.Fatalf("lost returned title evidence: %+v", score)
	}
	candidate.Year = 0
	if score := Evaluate(media, candidate, "hr"); score.Total != 20 {
		t.Fatalf("alternate title invented year: %+v", score)
	}
	candidate.ExternalIDs.IMDb = "tt1234567"
	media.ExternalIDs.IMDb = "tt7654321"
	if Eligible(Evaluate(media, candidate, "hr"), 1) {
		t.Fatal("alternate title overrode conflicting ID")
	}
}
