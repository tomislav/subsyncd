package pack

import (
	"errors"
	"slices"
	"strings"
	"subsyncd/internal/domain"
)

// SelectAlternatives preserves strict selection except for up to three versions
// with explicit, consistent target-episode tokens. Timing chooses between these
// versions; it must never be used to guess which episode an unknown file is.
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
	if len(matches) == 0 || len(matches) > 3 {
		return nil, err
	}
	slices.SortFunc(matches, func(a, b Member) int { return strings.Compare(a.SafeName, b.SafeName) })
	return matches, nil
}
