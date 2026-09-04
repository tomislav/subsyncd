package match

import "testing"

func TestParseReleaseExtractsComparableEvidence(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want Release
	}{
		{"episode", "Example.Show.S01E02.1080p.NF.WEB-DL.DDP5.1.H.264-GROUP", Release{Title: "example show", Season: 1, Episode: 2, Resolution: "1080p", Source: "web-dl", Service: "netflix", Group: "group"}},
		{"extended movie", "Open.Feature.2014.EXTENDED.1080p.BluRay.x264-RARBG", Release{Title: "open feature", Year: 2014, Resolution: "1080p", Source: "bluray", Edition: "extended", Group: "rarbg"}},
		{"final cut", "Film.2020.FINAL.CUT.1080p.BluRay-GRP", Release{Title: "film", Year: 2020, Resolution: "1080p", Source: "bluray", Edition: "final cut", Group: "grp"}},
		{"special edition", "Film.2020.SPECIAL.EDITION.1080p.BluRay-GRP", Release{Title: "film", Year: 2020, Resolution: "1080p", Source: "bluray", Edition: "special edition", Group: "grp"}},
		{"ultimate cut", "Film.2020.ULTIMATE.CUT.1080p.BluRay-GRP", Release{Title: "film", Year: 2020, Resolution: "1080p", Source: "bluray", Edition: "ultimate cut", Group: "grp"}},
		{"redux", "Film.2020.REDUX.1080p.BluRay-GRP", Release{Title: "film", Year: 2020, Resolution: "1080p", Source: "bluray", Edition: "redux", Group: "grp"}},
		{"anniversary edition", "Film.2020.20TH.ANNIVERSARY.EDITION.1080p.BluRay-GRP", Release{Title: "film", Year: 2020, Resolution: "1080p", Source: "bluray", Edition: "anniversary edition", Group: "grp"}},
		{"remux", "Film.2020.2160p.UHD.BluRay.REMUX.DV-GRP", Release{Title: "film", Year: 2020, Resolution: "2160p", Source: "remux", Group: "grp"}},
		{"season pack", "Example.Show.S01.COMPLETE.720p.WEBRip-GRP", Release{Title: "example show", Season: 1, Resolution: "720p", Source: "webrip", Group: "grp", Complete: true}},
		{"multi episode", "Example.Show.S01E02-E04.1080p.HDTV-GRP", Release{Title: "example show", Season: 1, Episode: 2, EpisodeEnd: 4, Resolution: "1080p", Source: "hdtv", Group: "grp"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := ParseRelease(test.raw)
			if got.Raw != test.raw || got.Title != test.want.Title || got.Year != test.want.Year || got.Season != test.want.Season || got.Episode != test.want.Episode || got.EpisodeEnd != test.want.EpisodeEnd || got.Resolution != test.want.Resolution || got.Source != test.want.Source || got.Edition != test.want.Edition || got.Service != test.want.Service || got.Group != test.want.Group || got.Complete != test.want.Complete {
				t.Fatalf("ParseRelease() = %#v, want fields %#v", got, test.want)
			}
			if again := ParseRelease(test.raw); again != got {
				t.Fatalf("ParseRelease() is not deterministic: %#v != %#v", again, got)
			}
		})
	}
}

func TestNormalizeIdentityHandlesAliasesAndPunctuationWithoutFuzzyMatching(t *testing.T) {
	if NormalizeIdentity("Marvel's Jessica Jones") != NormalizeIdentity("Marvels.Jessica-Jones") {
		t.Fatal("punctuation should normalize")
	}
	if NormalizeIdentity("The Office") == NormalizeIdentity("Office Space") {
		t.Fatal("different titles must not fuzzy-match")
	}
}

func TestParseReleaseDoesNotTreatMovieTitleAsEdition(t *testing.T) {
	for _, raw := range []string{
		"The.Final.Cut.2004.1080p.BluRay-GRP",
		"Anniversary.2015.1080p.BluRay-GRP",
	} {
		if release := ParseRelease(raw); release.Edition != "" {
			t.Errorf("ParseRelease(%q) edition = %q", raw, release.Edition)
		}
	}
}
