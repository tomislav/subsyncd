package workflow

import (
	"context"
	"log/slog"

	"subsyncd/internal/domain"
	"subsyncd/internal/pack"
	"subsyncd/internal/provider"
)

// releaseNumberedMedia is an episode under its release's own numbering, when
// that names another season: Sonarr may number a show as one long season that
// releases and providers split in two, so the file Sonarr calls S01E10 is
// released, and has subtitles filed, as S02E02. Only the numbering changes;
// the file, identity and titles stay the media's.
func releaseNumberedMedia(media domain.Media) (domain.Media, bool) {
	if media.Ref.Kind != domain.MediaEpisode || media.IsEpisodeRange() {
		return media, false
	}
	season, episode, found := pack.ReleaseEpisode(media)
	if !found {
		return media, false
	}
	release := media
	release.Season, release.Episode = season, episode
	release.EpisodeEnd = 0
	// Absolute numbers are Arr's and would contradict the release's.
	release.AbsoluteEpisode, release.AbsoluteEpisodeEnd = 0, 0
	return release, true
}

// candidateMedia is the media a candidate is scored and selected against: the
// release's numbering for a release-numbered result, otherwise Arr's.
func candidateMedia(media domain.Media, candidate domain.Candidate) domain.Media {
	if candidate.ReleaseNumbered {
		if release, found := releaseNumberedMedia(media); found {
			return release
		}
	}
	return media
}

// mergeReleaseNumberedSearch adds the release-numbered search's results to the
// search in Arr's numbering. A result both searches returned keeps its Arr
// identity. A provider that found something under the release's numbering is
// not empty. The release search's provider errors stay out: throttling and
// resume follow the search in Arr's numbering.
func mergeReleaseNumberedSearch(search, release provider.SearchResult) provider.SearchResult {
	seen := make(map[[2]string]bool, len(search.Candidates))
	for _, candidate := range search.Candidates {
		seen[[2]string{candidate.ProviderID, candidate.ResultID}] = true
	}
	found := map[string]bool{}
	merged := search
	merged.Candidates = append([]domain.Candidate(nil), search.Candidates...)
	for _, candidate := range release.Candidates {
		key := [2]string{candidate.ProviderID, candidate.ResultID}
		if seen[key] {
			continue
		}
		seen[key] = true
		candidate.ReleaseNumbered = true
		merged.Candidates = append(merged.Candidates, candidate)
		found[candidate.ProviderID] = true
	}
	merged.EmptyProviders = nil
	for _, providerID := range search.EmptyProviders {
		if !found[providerID] {
			merged.EmptyProviders = append(merged.EmptyProviders, providerID)
		}
	}
	return merged
}

func (s *Service) logReleaseNumberedSearch(ctx context.Context, release domain.Media, search provider.SearchResult) {
	s.workflowEvents().Log(ctx, slog.LevelInfo, "search.release_numbered", "searched the release's own episode number",
		slog.Int("release_season", release.Season), slog.Int("release_episode", release.Episode),
		slog.Int("candidate_count", len(search.Candidates)), slog.Int("provider_error_count", len(search.Errors)))
}
