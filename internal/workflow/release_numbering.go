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
func (r Request) candidateMedia(candidate domain.Candidate) domain.Media {
	if candidate.ReleaseNumbered && r.releaseMedia != nil {
		return *r.releaseMedia
	}
	return r.Media
}

// mergeReleaseNumberedSearch adds the release-numbered search's results to the
// search in Arr's numbering and tags them. A result both searches returned is
// tagged when it names the release's season (a provider that ignores the
// season returns the Part 2 pack for both), and otherwise keeps its Arr
// identity. A provider that found something under the release's numbering is
// not empty. Of the release search's provider errors only cooldowns and
// quotas are kept, for providers the first search did not already fail: they
// leave that search unfinished, so the job waits for the reset and the
// provider is asked again. Technical failures there are only logged.
func mergeReleaseNumberedSearch(search, release provider.SearchResult, releaseSeason, arrSeason int) provider.SearchResult {
	merged := search
	merged.Candidates = append([]domain.Candidate(nil), search.Candidates...)
	index := make(map[[2]string]int, len(merged.Candidates))
	for position, candidate := range merged.Candidates {
		index[[2]string{candidate.ProviderID, candidate.ResultID}] = position
	}
	found := map[string]bool{}
	for _, candidate := range release.Candidates {
		key := [2]string{candidate.ProviderID, candidate.ResultID}
		candidate.ReleaseNumbered = true
		if position, duplicate := index[key]; duplicate {
			if season := candidateSeason(candidate); season == releaseSeason && season != arrSeason {
				merged.Candidates[position] = candidate
				found[candidate.ProviderID] = true
			}
			continue
		}
		index[key] = len(merged.Candidates)
		merged.Candidates = append(merged.Candidates, candidate)
		found[candidate.ProviderID] = true
	}
	merged.Errors = make(map[string]error, len(search.Errors))
	for providerID, err := range search.Errors {
		merged.Errors[providerID] = err
	}
	for providerID, err := range release.Errors {
		if _, failed := merged.Errors[providerID]; failed {
			continue
		}
		if _, unavailable := unavailableReset(err); unavailable {
			merged.Errors[providerID] = err
		}
	}
	merged.EmptyProviders = nil
	for _, providerID := range search.EmptyProviders {
		if _, failed := merged.Errors[providerID]; !found[providerID] && !failed {
			merged.EmptyProviders = append(merged.EmptyProviders, providerID)
		}
	}
	return merged
}

// candidateSeason is the season a result names: its pack's, else its own.
func candidateSeason(candidate domain.Candidate) int {
	if candidate.Pack != nil && candidate.Pack.Season != 0 {
		return candidate.Pack.Season
	}
	return candidate.Season
}

func (s *Service) logReleaseNumberedSearch(ctx context.Context, release domain.Media, search provider.SearchResult) {
	s.workflowEvents().Log(ctx, slog.LevelInfo, "search.release_numbered", "searched the release's own episode number",
		slog.Int("release_season", release.Season), slog.Int("release_episode", release.Episode),
		slog.Int("candidate_count", len(search.Candidates)), slog.Int("provider_error_count", len(search.Errors)))
}
