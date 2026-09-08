package match

import (
	"fmt"
	"math"
	"sort"
	"strings"

	"subsyncd/internal/domain"
)

type EvaluatedCandidate struct {
	Candidate        domain.Candidate
	Score            domain.Score
	ProviderPriority int
}

func Evaluate(media domain.Media, candidate domain.Candidate, requestedLanguage domain.Language) domain.Score {
	return evaluate(media, candidate, requestedLanguage, false)
}

// EvaluateSelectedPackMember is only for a runtime-pack member already selected
// by the strict archive selector. It substitutes member episode evidence without
// rewriting provider identity or changing point weights. Exact hashes cannot be
// transferred between media through this path.
func EvaluateSelectedPackMember(media domain.Media, candidate domain.Candidate, language domain.Language) domain.Score {
	if media.Ref.Kind != domain.MediaEpisode || candidate.ExactHash {
		return Evaluate(media, candidate, language)
	}
	return evaluate(media, candidate, language, true)
}

func evaluate(media domain.Media, candidate domain.Candidate, requestedLanguage domain.Language, selectedMember bool) domain.Score {
	releases := parseReleases(candidate.ReleaseNames)
	rejected := identityRejections(media, candidate, requestedLanguage, releases, selectedMember)
	if len(rejected) != 0 {
		return domain.Score{RejectedReasons: rejected}
	}
	if candidate.ExactHash {
		return domain.Score{Total: 100, Contributions: []domain.Contribution{{Signal: "exact_hash", Points: 100, Reason: "provider verified the exact media-file hash"}}}
	}

	externalMatch := matchingExternalID(media.ExternalIDs, candidate.ExternalIDs)
	titleMatch := titleMatches(media, candidate, releases)
	yearMatch := candidate.Year != 0 && media.Year != 0 && candidate.Year == media.Year
	if !yearMatch {
		for _, release := range releases {
			if release.Year != 0 && release.Year == media.Year {
				yearMatch = true
				break
			}
		}
	}

	contributions := []domain.Contribution{
		contribution("external_id", boolPoints(externalMatch, 20), "matching IMDb, TMDB, or TVDB identity"),
		contribution("title_year", boolPoints(titleMatch && yearMatch, 15), "normalized title and year"),
		contribution("episode", boolPoints(selectedMember || episodeEvidenceMatches(media, candidate, releases), 20), "episode or containing pack"),
		contribution("release_group", boolPoints(releaseGroupMatches(releases, media.ReleaseGroup), 25), "release group"),
		contribution("source", boolPoints(releaseSourceMatches(releases, media.Source), 15), "media source"),
		contribution("edition", boolPoints(editionMatches(releases, media.Edition), 10), "edition or cut"),
		contribution("streaming_service", boolPoints(releaseFieldMatches(releases, media.StreamingService, func(r Release) string { return r.Service }), 5), "streaming service"),
		contribution("resolution", boolPoints(releaseFieldMatches(releases, media.Resolution, func(r Release) string { return r.Resolution }), 5), "resolution"),
		contribution("provider_rating", int(math.Round(clamp(candidate.Rating)*3)), "normalized provider rating"),
		contribution("popularity", int(math.Round(clamp(candidate.Popularity)*2)), "normalized provider popularity"),
	}
	total := 0
	for _, item := range contributions {
		total += item.Points
	}
	if total > 100 {
		total = 100
	}
	return domain.Score{Total: total, Contributions: contributions}
}

func HasEpisodeEvidence(media domain.Media, candidate domain.Candidate) bool {
	releases := parseReleases(candidate.ReleaseNames)
	return episodeEvidenceMatches(media, candidate, releases)
}

func episodeEvidenceMatches(media domain.Media, candidate domain.Candidate, releases []Release) bool {
	if media.Ref.Kind != domain.MediaEpisode {
		return false
	}
	if candidate.Season == media.Season && candidate.Episode == media.Episode && media.Season > 0 && media.Episode > 0 {
		return true
	}
	if media.AbsoluteEpisode > 0 && candidate.AbsoluteEpisode == media.AbsoluteEpisode {
		return true
	}
	if packContains(candidate.Pack, media) {
		return true
	}
	for _, release := range releases {
		end := release.EpisodeEnd
		if end == 0 {
			end = release.Episode
		}
		if release.Season == media.Season && release.Episode > 0 && release.Episode <= media.Episode && media.Episode <= end {
			return true
		}
	}
	return false
}

func Eligible(score domain.Score, minimum int) bool {
	return len(score.RejectedReasons) == 0 && score.Total >= minimum
}

func Rank(items []EvaluatedCandidate) {
	sort.SliceStable(items, func(i, j int) bool {
		left, right := items[i], items[j]
		if left.Score.Total != right.Score.Total {
			return left.Score.Total > right.Score.Total
		}
		if left.ProviderPriority != right.ProviderPriority {
			return left.ProviderPriority < right.ProviderPriority
		}
		if left.Candidate.Rating != right.Candidate.Rating {
			return left.Candidate.Rating > right.Candidate.Rating
		}
		if left.Candidate.Popularity != right.Candidate.Popularity {
			return left.Candidate.Popularity > right.Candidate.Popularity
		}
		if left.Candidate.ProviderID != right.Candidate.ProviderID {
			return left.Candidate.ProviderID < right.Candidate.ProviderID
		}
		return left.Candidate.ResultID < right.Candidate.ResultID
	})
}

func identityRejections(media domain.Media, candidate domain.Candidate, requestedLanguage domain.Language, releases []Release, selectedMember bool) []string {
	var reasons []string
	if !domain.EquivalentLanguage(candidate.Language, requestedLanguage) {
		reasons = append(reasons, "candidate language conflicts with requested language")
	}
	if candidate.Kind != "" && candidate.Kind != media.Ref.Kind {
		reasons = append(reasons, "candidate media kind conflicts with target")
	}
	if candidate.Forced {
		reasons = append(reasons, "forced-only candidate does not satisfy a full-language request")
	}
	reasons = append(reasons, externalConflicts(media.ExternalIDs, candidate.ExternalIDs)...)
	externalMatch := matchingExternalID(media.ExternalIDs, candidate.ExternalIDs)
	if media.Ref.Kind == domain.MediaMovie {
		if media.Year != 0 && candidate.Year != 0 && abs(media.Year-candidate.Year) > 1 && !externalMatch {
			reasons = append(reasons, "candidate movie year differs by more than one")
		}
	} else if packSeasonConflicts(candidate.Pack, media) {
		reasons = append(reasons, "candidate pack season conflicts with target")
	} else if !packContains(candidate.Pack, media) {
		if candidate.Season != 0 && candidate.Season != media.Season {
			reasons = append(reasons, "candidate season conflicts with target")
		}
		if !selectedMember && candidate.Episode != 0 && candidate.Episode != media.Episode && candidate.Episode != media.AbsoluteEpisode {
			reasons = append(reasons, "candidate episode conflicts with target")
		}
	}
	if media.Edition != "" && !candidate.ExactHash {
		known, matched := false, false
		wantedEdition := comparable(normalizeEdition(media.Edition, "", false, false, false))
		for _, release := range releases {
			if release.Edition != "" {
				known = true
				matched = matched || comparable(release.Edition) == wantedEdition
			}
		}
		if known && !matched {
			reasons = append(reasons, "candidate edition conflicts with target")
		}
	}
	return reasons
}

func HasMatchingEdition(media domain.Media, candidate domain.Candidate) bool {
	if strings.TrimSpace(media.Edition) == "" {
		return true
	}
	releases := parseReleases(candidate.ReleaseNames)
	return editionMatches(releases, media.Edition)
}

func packSeasonConflicts(pack *domain.PackInfo, media domain.Media) bool {
	if pack == nil || pack.Season == 0 || pack.Season == media.Season {
		return false
	}
	return !(pack.Scope == domain.PackRange && media.AbsoluteEpisode > 0 && pack.AbsoluteEpisodeFrom <= media.AbsoluteEpisode && media.AbsoluteEpisode <= pack.AbsoluteEpisodeTo)
}

func externalConflicts(media, candidate domain.ExternalIDs) []string {
	var reasons []string
	if media.IMDb != "" && candidate.IMDb != "" && comparable(media.IMDb) != comparable(candidate.IMDb) {
		reasons = append(reasons, "candidate IMDb identity conflicts with target")
	}
	if media.TMDB != 0 && candidate.TMDB != 0 && media.TMDB != candidate.TMDB {
		reasons = append(reasons, "candidate TMDB identity conflicts with target")
	}
	if media.TVDB != 0 && candidate.TVDB != 0 && media.TVDB != candidate.TVDB {
		reasons = append(reasons, "candidate TVDB identity conflicts with target")
	}
	return reasons
}

func matchingExternalID(media, candidate domain.ExternalIDs) bool {
	return media.IMDb != "" && candidate.IMDb != "" && comparable(media.IMDb) == comparable(candidate.IMDb) ||
		media.TMDB != 0 && candidate.TMDB != 0 && media.TMDB == candidate.TMDB ||
		media.TVDB != 0 && candidate.TVDB != 0 && media.TVDB == candidate.TVDB
}

func packContains(pack *domain.PackInfo, media domain.Media) bool {
	if pack == nil {
		return false
	}
	switch pack.Scope {
	case domain.PackSeason:
		return pack.Season == media.Season
	case domain.PackRange:
		standard := (pack.Season == 0 || pack.Season == media.Season) && pack.EpisodeFrom <= media.Episode && media.Episode <= pack.EpisodeTo
		absolute := media.AbsoluteEpisode > 0 && pack.AbsoluteEpisodeFrom <= media.AbsoluteEpisode && media.AbsoluteEpisode <= pack.AbsoluteEpisodeTo
		return standard || absolute
	}
	return false
}

func titleMatches(media domain.Media, candidate domain.Candidate, releases []Release) bool {
	wanted := map[string]struct{}{NormalizeIdentity(media.Title): {}}
	for _, title := range media.AlternateTitles {
		wanted[NormalizeIdentity(title)] = struct{}{}
	}
	if _, ok := wanted[NormalizeIdentity(candidate.Title)]; ok && NormalizeIdentity(candidate.Title) != "" {
		return true
	}
	for _, release := range releases {
		if _, ok := wanted[release.Title]; ok && release.Title != "" {
			return true
		}
	}
	return false
}

func releaseFieldMatches(releases []Release, wanted string, field func(Release) string) bool {
	wanted = comparable(wanted)
	if wanted == "" {
		return false
	}
	for _, release := range releases {
		if comparable(field(release)) == wanted {
			return true
		}
	}
	return false
}

func releaseSourceMatches(releases []Release, wanted string) bool {
	return releaseFieldMatches(releases, normalizeSource(wanted, ""), func(r Release) string { return r.Source })
}

func editionMatches(releases []Release, wanted string) bool {
	wanted = comparable(normalizeEdition(wanted, "", false, false, false))
	if wanted == "" {
		return false
	}
	for _, release := range releases {
		if comparable(release.Edition) == wanted {
			return true
		}
	}
	return false
}

var equivalentReleaseGroups = [][]string{
	{"framestor", "w4nk3r", "bhdstudio"},
	{"lol", "dimension"},
	{"asap", "immerse", "fleet"},
	{"avs", "sva"},
}

func releaseGroupMatches(releases []Release, wanted string) bool {
	wanted = comparable(wanted)
	if wanted == "" {
		return false
	}
	accepted := map[string]struct{}{wanted: {}}
	for _, group := range equivalentReleaseGroups {
		contains := false
		for _, alias := range group {
			contains = contains || comparable(alias) == wanted
		}
		if contains {
			for _, alias := range group {
				accepted[comparable(alias)] = struct{}{}
			}
		}
	}
	for _, release := range releases {
		if _, ok := accepted[comparable(release.Group)]; ok {
			return true
		}
	}
	return false
}

func contribution(signal string, points int, label string) domain.Contribution {
	reason := fmt.Sprintf("%s did not match or was unknown", label)
	if points > 0 {
		reason = fmt.Sprintf("%s matched", label)
	}
	return domain.Contribution{Signal: signal, Points: points, Reason: reason}
}

func boolPoints(value bool, points int) int {
	if value {
		return points
	}
	return 0
}

func comparable(value string) string { return NormalizeIdentity(strings.TrimSpace(value)) }

func clamp(value float64) float64 {
	if value < 0 {
		return 0
	}
	if value > 1 {
		return 1
	}
	return value
}

func abs(value int) int {
	if value < 0 {
		return -value
	}
	return value
}
