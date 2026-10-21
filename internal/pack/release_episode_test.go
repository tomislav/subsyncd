package pack

import (
	"errors"
	"fmt"
	"testing"

	"subsyncd/internal/domain"
)

// seasonTwoPack is a season pack named the way releases number a show that
// Sonarr's database numbers as one longer season: S02E01-S02E08.
func seasonTwoPack() Manifest {
	members := make([]Member, 0, 8)
	for episode := 1; episode <= 8; episode++ {
		members = append(members, Member{SafeName: fmt.Sprintf("Show.S02E%02d.WEB.srt", episode)})
	}
	return Manifest{ArchiveType: "zip", Members: members}
}

// Sonarr numbers the file S01E10; its release, and every subtitle for it, says
// S02E02. Sonarr's number matches no member, so the release's number picks one.
func TestSelectFallsBackToTheReleaseNameEpisode(t *testing.T) {
	media := domain.Media{Title: "Show", Season: 1, Episode: 10, EpisodeTitle: "Some Title",
		ReleaseName:      "Show.S02E02.MULTI.HDR.2160p.WEB.H265-GROUP",
		OriginalFilename: "Show.S02E02.MULTI.HDR.2160p.WEB.H265-GROUP"}
	selected, err := Select(seasonTwoPack(), domain.Candidate{Pack: &domain.PackInfo{Scope: domain.PackSeason, Season: 2}}, media, false)
	if err != nil || selected.SafeName != "Show.S02E02.WEB.srt" || selected.SelectionRule != "release_episode_token" {
		t.Fatalf("Select() = %#v, %v", selected, err)
	}
}

func TestSelectPrefersArrNumberingOverTheReleaseName(t *testing.T) {
	media := domain.Media{Title: "Show", Season: 1, Episode: 10, ReleaseName: "Show.S02E02.WEB-GROUP"}
	manifest := Manifest{Members: []Member{{SafeName: "Show.S01E10.srt"}, {SafeName: "Show.S02E02.srt"}}}
	selected, err := Select(manifest, domain.Candidate{}, media, false)
	if err != nil || selected.SafeName != "Show.S01E10.srt" || selected.SelectionRule != "episode_token" {
		t.Fatalf("Select() = %#v, %v", selected, err)
	}
}

func TestSelectReleaseNameFallbackFailsClosed(t *testing.T) {
	tests := []struct {
		name     string
		media    domain.Media
		manifest Manifest
	}{
		{"no release name", domain.Media{Season: 1, Episode: 10}, seasonTwoPack()},
		{"release name agrees with Sonarr", domain.Media{Season: 1, Episode: 10, ReleaseName: "Show.S01E10.WEB-GROUP"}, seasonTwoPack()},
		{"release name in Sonarr's own season", domain.Media{Season: 1, Episode: 10, ReleaseName: "Show.S01E02.WEB-GROUP"}, Manifest{Members: []Member{{SafeName: "Show.S01E01.srt"}, {SafeName: "Show.S01E02.srt"}, {SafeName: "Show.S01E03.srt"}}}},
		{"release name has two episode tokens", domain.Media{Season: 1, Episode: 10, ReleaseName: "Show.S02E02.S02E03.WEB-GROUP"}, seasonTwoPack()},
		{"release name is a range", domain.Media{Season: 1, Episode: 10, ReleaseName: "Show.S02E02-E03.WEB-GROUP"}, seasonTwoPack()},
		{"release episode missing from the pack", domain.Media{Season: 1, Episode: 10, ReleaseName: "Show.S02E09.WEB-GROUP"}, seasonTwoPack()},
		{"release episode only in a range member", domain.Media{Season: 1, Episode: 10, ReleaseName: "Show.S02E02.WEB-GROUP"}, Manifest{Members: []Member{{SafeName: "Show.S02E01-E03.srt"}}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			selected, err := Select(test.manifest, domain.Candidate{}, test.media, false)
			var selection *SelectionError
			if !errors.As(err, &selection) || selection.Rule != "none" {
				t.Fatalf("Select() = %#v, %v; want no unique member", selected, err)
			}
		})
	}
}

func TestSelectReleaseNameFallbackRejectsAmbiguousMembers(t *testing.T) {
	media := domain.Media{Season: 1, Episode: 10, ReleaseName: "Show.S02E02.WEB-GROUP"}
	manifest := Manifest{Members: []Member{{SafeName: "Show.S02E02.WEB.srt"}, {SafeName: "Show.S02E02.BluRay.srt"}}}
	selected, err := Select(manifest, domain.Candidate{}, media, false)
	var selection *SelectionError
	if !errors.As(err, &selection) || selection.Rule != "release_episode_token" || selection.MatchingMemberCount != 2 {
		t.Fatalf("Select() = %#v, %v; want an ambiguity rejection", selected, err)
	}
}

// A pack with two versions of each episode (WEB and BluRay, as English season
// packs often are) offers both versions of the release's episode for timing.
func TestSelectUntriedAlternativesKeepsReleaseNameVersions(t *testing.T) {
	media := domain.Media{Season: 1, Episode: 10, ReleaseName: "Show.S02E02.WEB-GROUP"}
	var members []Member
	for episode := 1; episode <= 8; episode++ {
		for _, source := range []string{"WEB", "BluRay"} {
			name := fmt.Sprintf("Show.S02E%02d.%s.srt", episode, source)
			members = append(members, Member{SafeName: name, Checksum: name})
		}
	}
	selected, _, err := SelectUntriedAlternatives(Manifest{Members: members}, domain.Candidate{}, media, false, nil)
	if err != nil || len(selected) != 2 {
		t.Fatalf("SelectUntriedAlternatives() = %#v, %v", selected, err)
	}
	for _, member := range selected {
		if member.SelectionRule != "release_episode_token" || (member.SafeName != "Show.S02E02.WEB.srt" && member.SafeName != "Show.S02E02.BluRay.srt") {
			t.Fatalf("selected %#v", member)
		}
	}
}

// A complete pack in Sonarr's numbering (16 episodes in season 1, missing the
// target, and its own season 2) must not hand over Sonarr's S02E02, another
// episode, just because the release calls the target S02E02.
func TestSelectReleaseNameFallbackSkipsPacksInArrNumbering(t *testing.T) {
	media := domain.Media{Season: 1, Episode: 10, ReleaseName: "Show.S02E02.WEB-GROUP"}
	var members []Member
	for episode := 1; episode <= 16; episode++ {
		if episode != 10 {
			members = append(members, Member{SafeName: fmt.Sprintf("Show.S01E%02d.srt", episode)})
		}
	}
	for episode := 1; episode <= 8; episode++ {
		members = append(members, Member{SafeName: fmt.Sprintf("Show.S02E%02d.srt", episode)})
	}
	selected, err := Select(Manifest{Members: members}, domain.Candidate{}, media, false)
	var selection *SelectionError
	if !errors.As(err, &selection) || selection.Rule != "none" {
		t.Fatalf("Select() = %#v, %v; want no unique member", selected, err)
	}
}

// A pack in the release's numbering that also holds the release's season 1
// (S01E01-E08, S02E01-E08) still resolves Sonarr's S01E10 to S02E02.
func TestSelectReleaseNameFallbackAcceptsWholeShowPacksInReleaseNumbering(t *testing.T) {
	media := domain.Media{Season: 1, Episode: 10, ReleaseName: "Show.S02E02.WEB-GROUP"}
	var members []Member
	for season := 1; season <= 2; season++ {
		for episode := 1; episode <= 8; episode++ {
			members = append(members, Member{SafeName: fmt.Sprintf("Show.S%02dE%02d.srt", season, episode)})
		}
	}
	selected, err := Select(Manifest{Members: members}, domain.Candidate{}, media, false)
	if err != nil || selected.SafeName != "Show.S02E02.srt" || selected.SelectionRule != "release_episode_token" {
		t.Fatalf("Select() = %#v, %v", selected, err)
	}
}

func TestSelectReleaseNameFallbackIgnoresResolutionTokens(t *testing.T) {
	media := domain.Media{Season: 1, Episode: 10, ReleaseName: "Show.S02E02.1920x1080.WEB-GROUP"}
	selected, err := Select(seasonTwoPack(), domain.Candidate{}, media, false)
	if err != nil || selected.SafeName != "Show.S02E02.WEB.srt" || selected.SelectionRule != "release_episode_token" {
		t.Fatalf("Select() = %#v, %v", selected, err)
	}
}

// The fallback is for packs: a lone file a provider returned for Sonarr's
// S01E10 that names S02E02 stays conflicting evidence.
func TestSelectSingleEpisodeDoesNotUseTheReleaseName(t *testing.T) {
	media := domain.Media{Season: 1, Episode: 10, ReleaseName: "Show.S02E02.WEB-GROUP"}
	selected, err := SelectSingleEpisode(Manifest{Members: []Member{{SafeName: "Show.S02E02.srt"}}}, domain.Candidate{}, media, false)
	if err == nil {
		t.Fatalf("SelectSingleEpisode() = %#v, want a rejection", selected)
	}
}
