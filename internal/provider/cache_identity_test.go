package provider

import (
	"testing"
	"time"

	"subsyncd/internal/domain"
)

func TestSearchCacheKeyCoversSearchIdentity(t *testing.T) {
	base := SearchQuery{Language: "en", Mode: SearchBroad, Media: domain.Media{
		Ref:         domain.MediaRef{Instance: "sonarr", Kind: domain.MediaEpisode, FileID: 7},
		Fingerprint: domain.MediaFingerprint{Path: "/media/show.mkv", FileID: 7, Size: 100, ModTime: time.Unix(1, 0)},
		Title:       "Show", Year: 2020, Season: 1, Episode: 2,
		ExternalIDs: domain.ExternalIDs{IMDb: "tt1", TMDB: 1, TVDB: 1},
	}}
	key := providerCacheKey("p", base)
	for name, change := range map[string]func(*SearchQuery){
		"IMDb":          func(q *SearchQuery) { q.Media.ExternalIDs.IMDb = "tt2" },
		"TMDB":          func(q *SearchQuery) { q.Media.ExternalIDs.TMDB = 2 },
		"TVDB":          func(q *SearchQuery) { q.Media.ExternalIDs.TVDB = 2 },
		"title":         func(q *SearchQuery) { q.Media.Title = "Other Show" },
		"year":          func(q *SearchQuery) { q.Media.Year = 2021 },
		"episode range": func(q *SearchQuery) { q.Media.EpisodeEnd = 3 },
		"absolute":      func(q *SearchQuery) { q.Media.AbsoluteEpisode = 14 },
		"kind":          func(q *SearchQuery) { q.Media.Ref.Kind = domain.MediaMovie },
		"language":      func(q *SearchQuery) { q.Language = "hr" },
		"season":        func(q *SearchQuery) { q.Media.Season = 2 },
		"episode":       func(q *SearchQuery) { q.Media.Episode = 3 },
	} {
		query := base
		change(&query)
		if providerCacheKey("p", query) == key {
			t.Errorf("changing the %s reuses the cached search", name)
		}
	}
	if providerCacheKey("p", base) != key {
		t.Fatal("cache key is not stable")
	}
}
