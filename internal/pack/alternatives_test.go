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
	got, err = SelectAlternatives(manifest, domain.Candidate{}, media, false)
	if err != nil || len(got) != maxEpisodeVersions {
		t.Fatalf("four versions = %+v, %v; want the best %d", got, err, maxEpisodeVersions)
	}
}

func TestSelectAlternativesTriesTheBestMatchingVersionsOfALargeSet(t *testing.T) {
	// My Name Is Earl S01 on Titlovi: one HDTV and three DVDRip versions of each
	// episode, all with different content.
	media := domain.Media{Season: 1, Episode: 8, ReleaseGroup: "LOL", Source: "hdtv"}
	manifest := Manifest{Members: []Member{
		{SafeName: "mynameisearl_dvdrip_xvid-fov_my_name_is_earl_-_01x08.joys_wedding.dvdrip_xvid-fov.srt", Checksum: "a"},
		{SafeName: "mynameisearl_dvdrip_xvid-fov_my_name_is_earl_-_01x08_-_joys_wedding.srt", Checksum: "b"},
		{SafeName: "mynameisearl_dvdrip_xvid-fov_my_name_is_earl_-_01x08_-_joys_wedding_(2).srt", Checksum: "c"},
		{SafeName: "my_name_is_earl_s01_hdtv_my_name_is_earl_s01e08_joy's_wedding.hdtv-lol.srt", Checksum: "d"},
		{SafeName: "my_name_is_earl_s01_hdtv_my_name_is_earl_s01e09.hdtv-lol.srt", Checksum: "e"},
	}}
	got, err := SelectAlternatives(manifest, domain.Candidate{}, media, false)
	if err != nil || len(got) != maxEpisodeVersions {
		t.Fatalf("alternatives = %+v, %v", got, err)
	}
	if got[0].Checksum != "d" {
		t.Fatalf("first version = %q, want the HDTV-LOL one matching the media", got[0].SafeName)
	}
	for _, member := range got {
		if member.Checksum == "e" {
			t.Fatal("another episode was offered as a version")
		}
	}
	// Equal affinity keeps the stable name order, so the choice is repeatable.
	again, _ := SelectAlternatives(manifest, domain.Candidate{}, media, false)
	for i := range got {
		if got[i].SafeName != again[i].SafeName {
			t.Fatalf("selection not deterministic: %v vs %v", got, again)
		}
	}
}
