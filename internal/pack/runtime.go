package pack

import (
	"strings"

	"subsyncd/internal/domain"
)

// IsRuntimePack classifies extracted subtitle content, independently of the
// provider's identity. Count all subtitle members, including forced variants.
func IsRuntimePack(manifest Manifest) bool {
	if manifest.Candidate.Kind != domain.MediaEpisode || manifest.Candidate.Pack != nil {
		return false
	}
	if len(manifest.Members) > 1 {
		return true
	}
	for _, member := range manifest.Members {
		if _, from, to, ok := episodeRange(member.SafeName); ok && to > from {
			return true
		}
	}
	return false
}

// RuntimeCacheSeason requires provider series identity and a single safe season.
// Never use the requested media to fill missing evidence. Parsed Member.Season
// is insufficient: its permissive first-token fallback can hide malformed ranges.
func RuntimeCacheSeason(manifest Manifest) (int, bool) {
	c := manifest.Candidate
	if c.ExactHash || (c.ExternalIDs == (domain.ExternalIDs{}) && strings.TrimSpace(c.Title) == "") {
		return 0, false
	}
	season := c.Season
	if season < 0 {
		return 0, false
	}
	for _, member := range manifest.Members {
		name := member.SafeName
		if hasInvalidOrAmbiguousRangeEvidence(name) {
			return 0, false
		}
		for _, token := range episodeTokenPattern.FindAllStringIndex(name, -1) {
			if !completeRangeToken(name, token[0], token[1]) {
				return 0, false
			}
			s, e, _ := episodeToken(name[token[0]:token[1]])
			if s <= 0 || e <= 0 || (season != 0 && season != s) {
				return 0, false
			}
			season = s
		}
	}
	return season, season > 0
}
