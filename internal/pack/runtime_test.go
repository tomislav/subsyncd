package pack

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

func TestRuntimePackClassificationAndSafeSeason(t *testing.T) {
	for _, test := range []struct {
		name               string
		season             int
		names              []string
		runtime, cacheable bool
		wantSeason         int
	}{
		{"multiple", 1, []string{"Show.S01E01.srt", "Show.S01E02.srt"}, true, true, 1},
		{"inferred", 0, []string{"Show.S02E01.srt", "Show.S02E02.srt"}, true, true, 2},
		{"range_singleton", 0, []string{"Show.S02E01-E03.srt"}, true, true, 2},
		{"same_episode_variants", 1, []string{"a.S01E01.srt", "b.S01E01.srt"}, true, true, 1},
		{"forced_variant", 1, []string{"Show.S01E01.srt", "Show.S01E01.forced.srt"}, true, true, 1},
		{"generic_singleton", 1, []string{"subtitle.srt"}, false, true, 1},
		{"episode_singleton", 1, []string{"Show.S01E01.srt"}, false, true, 1},
		{"mixed_season", 0, []string{"Show.S01E01.srt", "Show.S02E01.srt"}, true, false, 0},
		{"provider_conflict", 2, []string{"Show.S01E01.srt", "Show.S01E02.srt"}, true, false, 0},
		{"malformed_range", 1, []string{"Show.S01E01.srt", "Show.S01E02-E.srt"}, true, false, 0},
		{"cross_season_range", 1, []string{"Show.S01E01.srt", "Show.S01E02-S02E03.srt"}, true, false, 0},
		{"suffix_contaminated", 1, []string{"Show.S01E01.srt", "Show.S01E02-E03a.srt"}, true, false, 0},
		{"unknown", 0, []string{"one.srt", "two.srt"}, true, false, 0},
		{"absolute_unknown", 0, []string{"Show.ABS001.srt", "Show.ABS002.srt"}, true, false, 0},
		{"absolute_known", 1, []string{"Show.ABS001.srt", "Show.ABS002.srt"}, true, true, 1},
		{"specials", 0, []string{"Show.S00E01.srt", "Show.S00E02.srt"}, true, false, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			candidate := cacheCandidate("runtime")
			candidate.Pack, candidate.Season, candidate.Episode = nil, test.season, 1
			original := candidate
			var entries []zipEntry
			for _, name := range test.names {
				entries = append(entries, zipEntry{name: name, body: validSRT})
			}
			payload := zipPayload(t, entries)
			manifest, err := Extract(context.Background(), candidate, strings.NewReader(string(payload)), int64(len(payload)), t.TempDir()+"/extracted", testLimits())
			if err != nil {
				t.Fatal(err)
			}
			season, safe := RuntimeCacheSeason(manifest)
			if manifest.RuntimePack != test.runtime || safe != test.cacheable || safe && season != test.wantSeason {
				t.Fatalf("classification=%v season=%d safe=%v", manifest.RuntimePack, season, safe)
			}
			if !reflect.DeepEqual(manifest.Candidate, original) {
				t.Fatal("candidate mutated")
			}
		})
	}
}
