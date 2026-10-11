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
// explicit, consistent target-episode tokens or compact numbers (607).
// Timing chooses between these versions; it must never be used to guess which
// episode an unknown file is. When a pack holds more than maxEpisodeVersions
// distinct versions, the ones whose release best matches the media file are
// tried.
func SelectAlternatives(manifest Manifest, candidate domain.Candidate, media domain.Media, wantForced bool) ([]Member, error) {
	members, _, err := SelectUntriedAlternatives(manifest, candidate, media, wantForced, nil)
	return members, err
}

// SelectUntriedAlternatives is SelectAlternatives that, when more than
// maxEpisodeVersions versions match, puts the versions tried reports as already
// rejected for this media after the untried ones before keeping the best
// maxEpisodeVersions, so a rejected version never displaces an untried one. The
// rejected versions that still fit stay in the result. untriedLeft reports
// untried versions that did not fit, so the pack is not exhausted when every
// returned version is rejected. A nil tried counts every version as untried.
func SelectUntriedAlternatives(manifest Manifest, candidate domain.Candidate, media domain.Media, wantForced bool, tried func(Member) (bool, error)) (members []Member, untriedLeft bool, err error) {
	member, err := Select(manifest, candidate, media, wantForced)
	if err == nil {
		return []Member{member}, false, nil
	}
	var selection *SelectionError
	if candidate.ExactHash || !errors.As(err, &selection) || selection.Rule != "episode_token" && selection.Rule != "compact_episode" {
		return nil, false, err
	}
	var matches []Member
	seen := map[string]bool{}
	for _, member := range manifest.Members {
		one := manifest
		one.Members = []Member{member}
		selected, e := Select(one, candidate, media, wantForced)
		if e != nil || selected.SelectionRule != selection.Rule {
			continue
		}
		if member.Checksum != "" && seen[member.Checksum] {
			continue
		}
		seen[member.Checksum] = true
		matches = append(matches, selected)
	}
	if len(matches) == 0 {
		return nil, false, err
	}
	slices.SortFunc(matches, func(a, b Member) int { return strings.Compare(a.SafeName, b.SafeName) })
	if len(matches) > maxEpisodeVersions {
		affinity := make(map[string]int, len(matches))
		for _, member := range matches {
			affinity[member.SafeName] = match.ReleaseAffinity(media, member.SafeName)
		}
		slices.SortStableFunc(matches, func(a, b Member) int { return affinity[b.SafeName] - affinity[a.SafeName] })
		untried := matches
		if tried != nil {
			var rejected []Member
			untried = nil
			for _, member := range matches {
				done, err := tried(member)
				if err != nil {
					return nil, false, err
				}
				if done {
					rejected = append(rejected, member)
				} else {
					untried = append(untried, member)
				}
			}
			matches = append(untried, rejected...)
		}
		untriedLeft = len(untried) > maxEpisodeVersions
		matches = matches[:maxEpisodeVersions]
	}
	return matches, untriedLeft, nil
}
