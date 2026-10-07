package subsource

import (
	"math"
	"testing"

	"subsyncd/internal/domain"
	base "subsyncd/internal/provider"
)

func testClient(t *testing.T) *Client {
	t.Helper()
	client, err := New(Config{APIKey: testKey}, base.Client{ProviderID: "subsource-test"}, base.SystemClock{})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

var seasonOne = titleEntry{MovieID: 20, Title: "Example Show", Type: "tvseries", ReleaseYear: 2020, IMDb: "tt0000002", Season: ptr(1)}

func ptr[T any](value T) *T { return &value }

func englishRow(id int64, releases ...string) subtitleRow {
	return subtitleRow{SubtitleID: id, Language: "english", ReleaseInfo: releases}
}

func TestNormalizeEpisodeEvidence(t *testing.T) {
	type want struct {
		keep           bool
		episode        int
		absolute       int
		scope          domain.PackScope
		from, to       int
		absFrom, absTo int
	}
	for name, test := range map[string]struct {
		releases []string
		query    base.SearchQuery
		want     want
	}{
		"exact episode":             {[]string{"Example.Show.S01E04.1080p.WEB-GRP"}, episodeQuery(1, 4), want{keep: true, episode: 4}},
		"loose episode":             {[]string{"--Example Show S1 EP04--", " example.show.s1e4.720p.brrip.x264-grp"}, episodeQuery(1, 4), want{keep: true, episode: 4}},
		"cross form":                {[]string{"Example Show 1x04 HDTV"}, episodeQuery(1, 4), want{keep: true, episode: 4}},
		"season word episode":       {[]string{"Example Show Season 1 Episode 4"}, episodeQuery(1, 4), want{keep: true, episode: 4}},
		"season word ep":            {[]string{"Example Show - Season 1 EP04"}, episodeQuery(1, 4), want{keep: true, episode: 4}},
		"season word other episode": {[]string{"Example Show Season 1 Episode 3"}, episodeQuery(1, 4), want{}},
		"absolute range misses end": {[]string{"Example Show - 64-66 [1080p]"}, absoluteRangeQuery(66, 67), want{}},
		"absolute range covers end": {[]string{"Example Show - 64-67 [1080p]"}, absoluteRangeQuery(66, 67), want{keep: true, scope: domain.PackRange, absFrom: 64, absTo: 67}},
		"other episode":             {[]string{"Example.Show.S01E03.1080p.WEB-GRP"}, episodeQuery(1, 4), want{}},
		"loose other episode":       {[]string{"--Example Show S1 EP03--"}, episodeQuery(1, 4), want{}},
		"other season":              {[]string{"Example.Show.S02E04.1080p.WEB-GRP"}, episodeQuery(1, 4), want{}},
		"containing range":          {[]string{"Example.Show.S01E01-E05.1080p.WEB-GRP"}, episodeQuery(1, 4), want{keep: true, scope: domain.PackRange, from: 1, to: 5}},
		"double episode":            {[]string{"Example.Show.S01E03E04.1080p.WEB-GRP"}, episodeQuery(1, 4), want{keep: true, scope: domain.PackRange, from: 3, to: 4}},
		"excluding range":           {[]string{"Example.Show.S01E05-E07.1080p.WEB-GRP"}, episodeQuery(1, 4), want{}},
		"season pack":               {[]string{"Example.Show.S01.1080p.BluRay.x264-GRP"}, episodeQuery(1, 4), want{keep: true, scope: domain.PackSeason}},
		"season words":              {[]string{"Example Show - Season 1 (WEBRip)"}, episodeQuery(1, 4), want{keep: true, scope: domain.PackSeason}},
		"compact season word":       {[]string{"Example.Show.Season01.1080p.WEB.h264-GRP"}, episodeQuery(1, 4), want{keep: true, scope: domain.PackSeason}},
		"complete season":           {[]string{"Example.Show.S01.COMPLETE.720p.WEBRip-GRP"}, episodeQuery(1, 4), want{keep: true, scope: domain.PackSeason}},
		"other season pack":         {[]string{"Example.Show.S02.1080p.BluRay.x264-GRP"}, episodeQuery(1, 4), want{}},
		"no evidence":               {[]string{"Example Show 720p BluRay"}, episodeQuery(1, 4), want{}},
		"slug only":                 {[]string{"example-show_english-601"}, episodeQuery(1, 4), want{}},
		"disagreeing":               {[]string{"Example.Show.S01E04.720p-GRP", "Example.Show.S01E05.720p-GRP"}, episodeQuery(1, 4), want{}},
		"episode and season pack":   {[]string{"Example.Show.S01E04.720p-GRP", "Example.Show.S01.720p-GRP"}, episodeQuery(1, 4), want{}},
		"agreeing alternatives":     {[]string{"Example.Show.S01E04.720p-A", "Example.Show.S01E04.1080p-B"}, episodeQuery(1, 4), want{keep: true, episode: 4}},
		"lone absolute number":      {[]string{"Example Show - 66 [1080p]"}, absoluteQuery(66), want{}},
		"absolute range":            {[]string{"Example Show - 64-67 [1080p]"}, absoluteQuery(66), want{keep: true, scope: domain.PackRange, absFrom: 64, absTo: 67}},
		"absolute without target":   {[]string{"Example Show - 64-67 [1080p]"}, episodeQuery(1, 4), want{}},
		"range covers file range":   {[]string{"Example.Show.S01E01-E05.1080p.WEB-GRP"}, rangeQuery(1, 4, 5), want{keep: true, scope: domain.PackRange, from: 1, to: 5}},
		"range misses file range":   {[]string{"Example.Show.S01E01-E04.1080p.WEB-GRP"}, rangeQuery(1, 4, 5), want{}},
		"single misses file range":  {[]string{"Example.Show.S01E04.1080p.WEB-GRP"}, rangeQuery(1, 4, 5), want{}},
	} {
		t.Run(name, func(t *testing.T) {
			got := testClient(t).normalize(test.query, seasonOne, []subtitleRow{englishRow(601, test.releases...)})
			if !test.want.keep {
				if len(got) != 0 {
					t.Fatalf("got %#v; want row dropped", got)
				}
				return
			}
			if len(got) != 1 {
				t.Fatalf("got %d candidates; want 1", len(got))
			}
			c := got[0]
			if c.Episode != test.want.episode || c.AbsoluteEpisode != test.want.absolute {
				t.Fatalf("episode = %d absolute = %d; want %d %d", c.Episode, c.AbsoluteEpisode, test.want.episode, test.want.absolute)
			}
			if test.want.scope == "" {
				if c.Pack != nil {
					t.Fatalf("pack = %#v; want single episode", c.Pack)
				}
				return
			}
			if c.Pack == nil || c.Pack.Scope != test.want.scope || c.Pack.EpisodeFrom != test.want.from || c.Pack.EpisodeTo != test.want.to || c.Pack.AbsoluteEpisodeFrom != test.want.absFrom || c.Pack.AbsoluteEpisodeTo != test.want.absTo {
				t.Fatalf("pack = %#v; want %s %d-%d abs %d-%d", c.Pack, test.want.scope, test.want.from, test.want.to, test.want.absFrom, test.want.absTo)
			}
			if c.Pack.Scope != domain.PackRange || test.want.absFrom == 0 {
				if c.Season != 1 || c.Pack.Season != 1 {
					t.Fatalf("season = %d pack season = %d; want 1", c.Season, c.Pack.Season)
				}
			}
		})
	}
}

func absoluteQuery(absolute int) base.SearchQuery {
	query := episodeQuery(3, 6)
	query.Media.AbsoluteEpisode = absolute
	return query
}

func absoluteRangeQuery(absolute, end int) base.SearchQuery {
	query := rangeQuery(3, 6, 7)
	query.Media.AbsoluteEpisode, query.Media.AbsoluteEpisodeEnd = absolute, end
	return query
}

func rangeQuery(season, episode, end int) base.SearchQuery {
	query := episodeQuery(season, episode)
	query.Media.EpisodeEnd = end
	return query
}

func TestNormalizePolicyFlags(t *testing.T) {
	movie := titleEntry{MovieID: 10, Title: "Example Movie", Type: "movie", ReleaseYear: 2025, IMDb: "tt0000001"}
	release := "Example.Movie.2025.1080p.WEB-DL-GRP"
	for name, test := range map[string]struct {
		row             subtitleRow
		keep            bool
		forced, hearing bool
	}{
		"plain":              {subtitleRow{SubtitleID: 1, Language: "english", ReleaseInfo: []string{release}}, true, false, false},
		"machine translated": {subtitleRow{SubtitleID: 1, Language: "english", ReleaseInfo: []string{release}, ProductionType: ptr("machine")}, false, false, false},
		"forced type":        {subtitleRow{SubtitleID: 1, Language: "english", ReleaseInfo: []string{release}, ProductionType: ptr("forced")}, true, true, false},
		"foreign parts":      {subtitleRow{SubtitleID: 1, Language: "english", ReleaseInfo: []string{release}, ForeignParts: ptr(true)}, true, true, false},
		"sdh name":           {subtitleRow{SubtitleID: 1, Language: "english", ReleaseInfo: []string{release + ".SDH"}}, true, false, true},
		"hi commentary":      {subtitleRow{SubtitleID: 1, Language: "english", ReleaseInfo: []string{release}, Commentary: "Hearing impaired version"}, true, false, true},
		"non-hi commentary":  {subtitleRow{SubtitleID: 1, Language: "english", ReleaseInfo: []string{release}, Commentary: "NON-HI"}, true, false, false},
		"other language":     {subtitleRow{SubtitleID: 1, Language: "croatian", ReleaseInfo: []string{release}}, false, false, false},
		"invalid id":         {subtitleRow{SubtitleID: 0, Language: "english", ReleaseInfo: []string{release}}, false, false, false},
		"movie without name": {subtitleRow{SubtitleID: 1, Language: "english", ReleaseInfo: []string{"example-movie_english-1"}}, true, false, false},
	} {
		t.Run(name, func(t *testing.T) {
			got := testClient(t).normalize(movieQuery(), movie, []subtitleRow{test.row})
			if (len(got) == 1) != test.keep {
				t.Fatalf("got %#v; keep = %v", got, test.keep)
			}
			if test.keep && (got[0].Forced != test.forced || got[0].HearingImpaired != test.hearing) {
				t.Fatalf("forced = %v hearing = %v; want %v %v", got[0].Forced, got[0].HearingImpaired, test.forced, test.hearing)
			}
		})
	}
}

func TestRatingIsVoteWeighted(t *testing.T) {
	for _, test := range []struct {
		good, bad, total int64
		want             float64
	}{
		{0, 0, 0, 0},
		{1, 0, 1, 1.0 / 6},
		{9, 1, 10, 0.9 * 10 / 15},
		{45, 0, 45, 0.9},
		{0, 10, 10, 0},
	} {
		if got := rating(test.good, test.bad, test.total); math.Abs(got-test.want) > 1e-9 {
			t.Errorf("rating(%d, %d, %d) = %v; want %v", test.good, test.bad, test.total, got, test.want)
		}
	}
}
