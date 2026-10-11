package pack

import (
	"testing"

	"subsyncd/internal/domain"
)

func TestSelectReadsCompactSeasonEpisodeNumbers(t *testing.T) {
	// Malcolm in the Middle S06 on Titlovi: one member names its episode 607.
	manifest := Manifest{Members: []Member{
		{SafeName: "s06_malcolm_in_the_middle_s06e06_hal's_christmas_gift_hdtv-lol.srt"},
		{SafeName: "s06__croatian.malcolm.in.the.middle.607.hdtv-lol.[bt].srt"},
		{SafeName: "s06_malcolm_in_the_middle_s06e08_lois_battles_jamie_hdtv.xvid-tcm.srt"},
	}}
	media := domain.Media{Title: "Malcolm in the Middle", Season: 6, Episode: 7}
	selected, err := Select(manifest, domain.Candidate{}, media, false)
	if err != nil || selected.SelectionRule != "compact_episode" || selected.SafeName != manifest.Members[1].SafeName {
		t.Fatalf("selected = %+v, %v", selected, err)
	}
	media.Episode = 9
	if selected, err := Select(manifest, domain.Candidate{}, media, false); err == nil {
		t.Fatalf("S06E09 selected %q", selected.SafeName)
	}
}

func TestCompactEpisodeNumbersNeedUnambiguousEvidence(t *testing.T) {
	cases := []struct {
		name  string
		media domain.Media
		want  bool
	}{
		{"show.607.hdtv.srt", domain.Media{Title: "Show", Season: 6, Episode: 7}, true},
		{"show.607.hdtv.srt", domain.Media{Title: "Show", Season: 6, Episode: 8}, false},
		{"show.607.hdtv.srt", domain.Media{Title: "Show", Season: 7, Episode: 7}, false},
		{"show.s06e07.607.srt", domain.Media{Title: "Show", Season: 6, Episode: 7}, false}, // explicit token decides elsewhere
		{"show.607.and.608.srt", domain.Media{Title: "Show", Season: 6, Episode: 7}, false},
		{"show.720.hdtv.srt", domain.Media{Title: "Show", Season: 7, Episode: 20}, false},
		{"show.x264.srt", domain.Media{Title: "Show", Season: 2, Episode: 64}, false},
		{"show.1080p.srt", domain.Media{Title: "Show", Season: 1, Episode: 8}, false},
		{"9-1-1.911.srt", domain.Media{Title: "9-1-1", Season: 9, Episode: 11}, false},
		{"show.1007.srt", domain.Media{Title: "Show", Season: 10, Episode: 7}, false},
		{"show.2007.srt", domain.Media{Title: "Show", Season: 2, Episode: 7}, false},
	}
	for _, tc := range cases {
		if got := compactEpisodeMatches(tc.name, tc.media); got != tc.want {
			t.Errorf("compactEpisodeMatches(%q, S%02dE%02d) = %v, want %v", tc.name, tc.media.Season, tc.media.Episode, got, tc.want)
		}
	}
}

func TestSingleGenericFileRejectsCompactNumberForAnotherEpisode(t *testing.T) {
	media := domain.Media{Title: "Show", Season: 2, Episode: 3}
	other := Manifest{Members: []Member{{SafeName: "show.105.hdtv.srt"}}}
	if selected, err := SelectSingleEpisode(other, domain.Candidate{}, media, false); err == nil {
		t.Fatalf("lone file for S01E05 accepted for S02E03: %+v", selected)
	}
	same := Manifest{Members: []Member{{SafeName: "show.203.hdtv.srt"}}}
	if selected, err := SelectSingleEpisode(same, domain.Candidate{}, media, false); err != nil || selected.SelectionRule != "compact_episode" {
		t.Fatalf("lone file for S02E03 = %+v, %v", selected, err)
	}
	generic := Manifest{Members: []Member{{SafeName: "show.hdtv.srt"}}}
	if selected, err := SelectSingleEpisode(generic, domain.Candidate{}, media, false); err != nil || selected.SelectionRule != "single_generic" {
		t.Fatalf("generic lone file = %+v, %v", selected, err)
	}
	if _, err := SelectSingleEpisode(Manifest{}, domain.Candidate{}, media, false); err == nil {
		t.Fatal("empty manifest selected a member")
	}
}

func TestCompactEpisodeVersionsAreAlternatives(t *testing.T) {
	media := domain.Media{Title: "Malcolm in the Middle", Season: 6, Episode: 7}
	manifest := Manifest{Members: []Member{
		{SafeName: "malcolm.in.the.middle.607.hdtv-lol.srt", Checksum: "hdtv"},
		{SafeName: "malcolm.in.the.middle.607.dvdrip-fov.srt", Checksum: "dvd"},
		{SafeName: "malcolm.in.the.middle.608.hdtv-lol.srt", Checksum: "next"},
	}}
	got, err := SelectAlternatives(manifest, domain.Candidate{}, media, false)
	if err != nil || len(got) != 2 {
		t.Fatalf("alternatives = %+v, %v; want both 607 versions", got, err)
	}
	for _, member := range got {
		if member.SelectionRule != "compact_episode" || member.Checksum == "next" {
			t.Fatalf("alternative = %+v", member)
		}
	}
}

func TestCompactEpisodeNumbersAreNotAnimeAbsoluteNumbers(t *testing.T) {
	cases := []struct {
		name  string
		media domain.Media
		want  bool
	}{
		// Absolute-numbered names in the anime convention, when the target's
		// absolute number is unknown.
		{"[SubsPlease] One Piece - 607 (1080p).srt", domain.Media{Title: "One Piece", Season: 6, Episode: 7}, false},
		{"One Piece - 607.srt", domain.Media{Title: "One Piece", Season: 6, Episode: 7}, false},
		// A site tag is not an anime group tag.
		{"[TorrentCouch.com].Malcolm.in.the.Middle.607.srt", domain.Media{Title: "Malcolm in the Middle", Season: 6, Episode: 7}, true},
		{"[www.Site] Show.607.srt", domain.Media{Title: "Show", Season: 6, Episode: 7}, true},
		// A known absolute number decides alone; the name shape does not override it.
		{"Malcolm in the Middle - 607 - Hal Coaches.srt", domain.Media{Title: "Malcolm in the Middle", Season: 6, Episode: 7, AbsoluteEpisode: 129}, true},
		{"[TorrentCouch.com].Malcolm.in.the.Middle.607.srt", domain.Media{Title: "Malcolm in the Middle", Season: 6, Episode: 7, AbsoluteEpisode: 129}, true},
		{"[SubsPlease] One Piece - 607 (1080p).srt", domain.Media{Title: "One Piece", Season: 6, Episode: 7, AbsoluteEpisode: 520}, false},
		// Seasons that average 100 episodes or more: 607 is as likely absolute.
		{"one.piece.607.srt", domain.Media{Title: "One Piece", Season: 6, Episode: 7, AbsoluteEpisode: 520}, false},
		{"show.107.srt", domain.Media{Title: "Show", Season: 1, Episode: 7, AbsoluteEpisode: 12}, false},
		// Ordinary seasons: the absolute number is far below the compact one.
		{"malcolm.in.the.middle.607.hdtv.srt", domain.Media{Title: "Malcolm in the Middle", Season: 6, Episode: 7, AbsoluteEpisode: 129}, true},
		{"show.107.srt", domain.Media{Title: "Show", Season: 1, Episode: 7, AbsoluteEpisode: 7}, true},
		// Adjacent numbers are a double episode, not the first one.
		{"show.607-608.hdtv.srt", domain.Media{Title: "Show", Season: 6, Episode: 7}, false},
		{"show.607608.hdtv.srt", domain.Media{Title: "Show", Season: 6, Episode: 7}, false},
		{"show.1607.srt", domain.Media{Title: "Show", Season: 6, Episode: 7}, false},
		{"show.6070.srt", domain.Media{Title: "Show", Season: 6, Episode: 7}, false},
	}
	for _, tc := range cases {
		if got := compactEpisodeMatches(tc.name, tc.media); got != tc.want {
			t.Errorf("compactEpisodeMatches(%q, %+v) = %v, want %v", tc.name, tc.media, got, tc.want)
		}
	}
	// A lone double-episode or absolute-numbered file is not a generic subtitle
	// for the target either.
	for _, name := range []string{"show.607-608.hdtv.srt", "[Group] Show - 607.srt"} {
		media := domain.Media{Title: "Show", Season: 6, Episode: 7}
		if selected, err := SelectSingleEpisode(Manifest{Members: []Member{{SafeName: name}}}, domain.Candidate{}, media, false); err == nil {
			t.Errorf("lone %q selected for S06E07: %+v", name, selected)
		}
	}
	// Both reviewer examples select as compact_episode with a known absolute number.
	for _, name := range []string{"Malcolm in the Middle - 607 - Hal Coaches.srt", "[TorrentCouch.com].Malcolm.in.the.Middle.607.srt"} {
		media := domain.Media{Title: "Malcolm in the Middle", Season: 6, Episode: 7, AbsoluteEpisode: 129}
		manifest := Manifest{Members: []Member{{SafeName: name, Checksum: "a"}, {SafeName: "Malcolm in the Middle - 608 - Other.srt", Checksum: "b"}}}
		if selected, err := Select(manifest, domain.Candidate{Pack: &domain.PackInfo{Season: 6}}, media, false); err != nil || selected.SelectionRule != "compact_episode" {
			t.Errorf("%q for S06E07 = %+v, %v; want compact_episode", name, selected, err)
		}
	}
	media := domain.Media{Title: "Show", Season: 6, Episode: 7, AbsoluteEpisode: 607}
	if _, err := SelectSingleEpisode(Manifest{Members: []Member{{SafeName: "[Group] Show - 607.srt"}}}, domain.Candidate{}, media, false); err != nil {
		t.Errorf("lone file naming the target's absolute number rejected: %v", err)
	}
}
