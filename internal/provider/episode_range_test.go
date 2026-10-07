package provider

import (
	"context"
	"slices"
	"testing"

	"subsyncd/internal/domain"
)

func TestCoordinatorSkipsSingleEpisodeOnlyProvidersForEpisodeRanges(t *testing.T) {
	ranged := &fakeProvider{id: "ranged", capabilities: Capabilities{ExactFileHash: true}, candidates: map[SearchMode][]domain.Candidate{
		SearchBroad: {{ProviderID: "ranged", ResultID: "combined"}},
	}}
	single := &fakeProvider{id: "single", capabilities: Capabilities{ExactFileHash: true, SingleEpisodeOnly: true, MediaKinds: []domain.MediaKind{domain.MediaEpisode}}, candidates: map[SearchMode][]domain.Candidate{
		SearchBroad: {{ProviderID: "single", ResultID: "part-one"}},
	}}
	media := testQueryMedia()
	media.Ref.Kind, media.Season, media.Episode, media.EpisodeEnd = domain.MediaEpisode, 6, 1, 2

	for _, mode := range []SearchMode{SearchExactHash, SearchBroad} {
		result := newTestCoordinator(ranged, single).Search(context.Background(), SearchQuery{Media: media, Language: "en", Mode: mode})
		if slices.Contains(result.ApplicableProviders, "single") || len(single.calls) != 0 {
			t.Fatalf("%s: single-episode provider was used for a range: applicable=%v calls=%v", mode, result.ApplicableProviders, single.calls)
		}
	}

	media.EpisodeEnd = 0
	result := newTestCoordinator(single).Search(context.Background(), SearchQuery{Media: media, Language: "en", Mode: SearchBroad})
	if !slices.Contains(result.ApplicableProviders, "single") {
		t.Fatalf("single-episode provider skipped for a single episode: %v", result.ApplicableProviders)
	}
}
