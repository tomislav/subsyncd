package pack

import (
	"subsyncd/internal/domain"
	"testing"
)

func TestSelectAlternativesRequiresExplicitEpisodeIdentity(t *testing.T) {
	media := domain.Media{Season: 4, Episode: 13}
	manifest := Manifest{Members: []Member{
		{SafeName: "Show.S04E13.WEB.srt", Checksum: "web"},
		{SafeName: "Show.04x13.HDTV.srt", Checksum: "hdtv"},
		{SafeName: "Show.S04E12.srt", Checksum: "wrong"},
		{SafeName: "Show.S04E13.forced.srt", Checksum: "forced"},
	}}
	got, err := SelectAlternatives(manifest, domain.Candidate{}, media, false)
	if err != nil || len(got) != 2 {
		t.Fatalf("alternatives = %+v, %v", got, err)
	}
	if _, err := Select(manifest, domain.Candidate{}, media, false); err == nil {
		t.Fatal("strict single selector became arbitrary")
	}
	if _, err := SelectAlternatives(manifest, domain.Candidate{ExactHash: true}, media, false); err == nil {
		t.Fatal("ambiguous exact hash bypassed selection")
	}
	for _, name := range []string{"Show.S04E13.third.srt", "Show.S04E13.fourth.srt"} {
		manifest.Members = append(manifest.Members, Member{SafeName: name, Checksum: name})
	}
	if _, err := SelectAlternatives(manifest, domain.Candidate{}, media, false); err == nil {
		t.Fatal("unbounded alternatives accepted")
	}
}
