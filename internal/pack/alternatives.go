package pack

import (
	"errors"
	"slices"
	"strings"

	"subsyncd/internal/domain"
	"subsyncd/internal/match"
)

// maxEpisodeVersions is how many versions of one episode in a pack are timed.
const maxEpisodeVersions = 3

// SelectAlternatives preserves strict selection except for versions with
// explicit, consistent target-episode tokens. Timing chooses between these
// versions; it must never be used to guess which episode an unknown file is.
// When a pack holds more than maxEpisodeVersions distinct versions, the ones
// whose release best matches the media file are tried.
func SelectAlternatives(manifest Manifest, candidate domain.Candidate, media domain.Media, wantForced bool) ([]Member, error) {
	member, err := Select(manifest, candidate, media, wantForced)
	if err == nil {
		return []Member{member}, nil
	}
	var selection *SelectionError
	if candidate.ExactHash || !errors.As(err, &selection) || selection.Rule != "episode_token" {
		return nil, err
	}
	var matches []Member
	seen := map[string]bool{}
	for _, member := range manifest.Members {
		one := manifest
		one.Members = []Member{member}
		selected, e := Select(one, candidate, media, wantForced)
		if e != nil || selected.SelectionRule != "episode_token" {
			continue
		}
		if member.Checksum != "" && seen[member.Checksum] {
			continue
		}
		seen[member.Checksum] = true
		matches = append(matches, selected)
	}
	if len(matches) == 0 {
		return nil, err
	}
	slices.SortFunc(matches, func(a, b Member) int { return strings.Compare(a.SafeName, b.SafeName) })
	if len(matches) > maxEpisodeVersions {
		affinity := make(map[string]int, len(matches))
		for _, member := range matches {
			affinity[member.SafeName] = match.ReleaseAffinity(media, member.SafeName)
		}
		slices.SortStableFunc(matches, func(a, b Member) int { return affinity[b.SafeName] - affinity[a.SafeName] })
		matches = matches[:maxEpisodeVersions]
	}
	return matches, nil
}
