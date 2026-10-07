package subsource

import (
	"regexp"
	"strconv"
	"strings"

	"subsyncd/internal/domain"
	"subsyncd/internal/match"
	"subsyncd/internal/pack"
	baseprovider "subsyncd/internal/provider"
)

// evidenceVersion marks candidates normalized under the current episode,
// annotation and translation rules.
const evidenceVersion = "subsource-evidence-v1"

var (
	// SubSource sometimes stores its own page slug as the release name.
	slugReleasePattern = regexp.MustCompile(`^[a-z0-9-]+_[a-z_]+-\d+$`)
	// Fallbacks for names the shared parser leaves without a season or episode.
	seasonWordPattern   = regexp.MustCompile(`(?i)(?:^|[^a-z0-9])season[\s._-]*0*(\d{1,2})(?:[^0-9]|$)`)
	looseEpisodePattern = regexp.MustCompile(`(?i)(?:^|[^a-z0-9])s0*(\d{1,2})[\s._-]*ep?[\s._-]*0*(\d{1,3})(?:[^0-9]|$)`)
	wordEpisodePattern  = regexp.MustCompile(`(?i)(?:^|[^a-z0-9])season[\s._-]*0*(\d{1,2})[\s._-]*(?:episode|ep)[\s._-]*0*(\d{1,3})(?:[^0-9]|$)`)
)

type titleEntry struct {
	MovieID        int64      `json:"movieId"`
	Title          string     `json:"title"`
	AlternateTitle string     `json:"alternateTitle"`
	Type           string     `json:"type"`
	ReleaseYear    int        `json:"releaseYear"`
	IMDb           string     `json:"imdbId"`
	TMDB           flexibleID `json:"tmdbId"`
	Season         *int       `json:"season"`
}

type subtitleRow struct {
	SubtitleID     int64    `json:"subtitleId"`
	Language       string   `json:"language"`
	ReleaseInfo    []string `json:"releaseInfo"`
	Commentary     string   `json:"commentary"`
	ForeignParts   *bool    `json:"foreignParts"`
	ProductionType *string  `json:"productionType"`
	Downloads      int64    `json:"downloads"`
	Rating         struct {
		Good  int64 `json:"good"`
		Bad   int64 `json:"bad"`
		Total int64 `json:"total"`
	} `json:"rating"`
}

// flexibleID accepts a numeric ID sent as a JSON string, number or null.
type flexibleID int64

func (id *flexibleID) UnmarshalJSON(data []byte) error {
	value, err := strconv.ParseInt(strings.Trim(string(data), `"`), 10, 64)
	if err != nil || value < 0 {
		value = 0
	}
	*id = flexibleID(value)
	return nil
}

func (c *Client) normalize(query baseprovider.SearchQuery, entry titleEntry, rows []subtitleRow) []domain.Candidate {
	candidates := make([]domain.Candidate, 0, len(rows))
	for _, row := range rows {
		if row.SubtitleID <= 0 || !supportsSlug(query.Language, row.Language) {
			continue
		}
		if row.ProductionType != nil && *row.ProductionType == "machine" {
			continue
		}
		names := releaseNames(row.ReleaseInfo)
		id := strconv.FormatInt(row.SubtitleID, 10)
		candidate := domain.Candidate{
			EvidenceVersion: evidenceVersion,
			ProviderID:      c.id,
			ResultID:        id,
			DownloadRef:     id,
			Language:        query.Language,
			Kind:            query.Media.Ref.Kind,
			Title:           entry.Title,
			ExternalIDs:     domain.ExternalIDs{IMDb: entry.IMDb, TMDB: int64(entry.TMDB)},
			ReleaseNames:    names,
			Rating:          rating(row.Rating.Good, row.Rating.Bad, row.Rating.Total),
			Popularity:      baseprovider.NormalizePopularity(row.Downloads),
			DownloadCount:   row.Downloads,
		}
		if alternate := strings.TrimSpace(entry.AlternateTitle); alternate != "" {
			candidate.AlternateTitles = []string{alternate}
		}
		candidate.Forced, candidate.HearingImpaired = annotations(names, row.Commentary)
		if row.ProductionType != nil && *row.ProductionType == "forced" || row.ForeignParts != nil && *row.ForeignParts {
			candidate.Forced = true
		}
		if query.Media.Ref.Kind == domain.MediaEpisode {
			if !applyEpisodeEvidence(&candidate, names, query.Media) {
				continue
			}
		} else {
			candidate.Year = entry.ReleaseYear
		}
		candidates = append(candidates, candidate)
	}
	return baseprovider.DeduplicateCandidates(candidates)
}

func releaseNames(raw []string) []string {
	names := make([]string, 0, len(raw))
	seen := map[string]bool{}
	for _, name := range raw {
		name = strings.TrimSpace(name)
		if name == "" || slugReleasePattern.MatchString(name) || seen[name] {
			continue
		}
		seen[name] = true
		names = append(names, name)
	}
	return names
}

func annotations(names []string, commentary string) (forced, hearing bool) {
	for _, name := range names {
		f, h := baseprovider.Annotations(name, "", false)
		forced, hearing = forced || f, hearing || h
	}
	f, h := baseprovider.Annotations("", commentary, false)
	return forced || f, hearing || h
}

// rating weights the share of positive votes by vote count, so a single vote
// contributes little.
func rating(good, bad, total int64) float64 {
	if good < 0 || bad < 0 || good+bad == 0 || total <= 0 {
		return 0
	}
	value := float64(good) / float64(good+bad) * float64(total) / float64(total+5)
	return min(max(value, 0), 1)
}

type episodeEvidence struct {
	season     int
	from, to   int
	absolute   bool
	seasonOnly bool
}

func parseEpisodeEvidence(name string) (evidence episodeEvidence, found, invalid bool) {
	rangeSeason, rangeFrom, rangeTo, rangeFound, bad := pack.ReleaseEpisodeRange(name)
	if bad {
		return episodeEvidence{}, false, true
	}
	release := match.ParseRelease(name)
	season, episode, end := release.Season, release.Episode, release.EpisodeEnd
	if season == 0 {
		if m := seasonWordPattern.FindStringSubmatch(name); m != nil {
			season, _ = strconv.Atoi(m[1])
		}
	}
	if episode == 0 && season > 0 {
		for _, pattern := range []*regexp.Regexp{looseEpisodePattern, wordEpisodePattern} {
			if m := pattern.FindStringSubmatch(name); m != nil {
				if s, _ := strconv.Atoi(m[1]); s == season {
					episode, _ = strconv.Atoi(m[2])
					break
				}
			}
		}
	}
	if rangeFound {
		season, episode, end = rangeSeason, rangeFrom, rangeTo
	}
	switch {
	case season > 0 && episode > 0:
		return episodeEvidence{season: season, from: episode, to: max(end, episode)}, true, false
	case season == 0 && release.AbsoluteEpisode > 0:
		return episodeEvidence{absolute: true, from: release.AbsoluteEpisode, to: max(release.AbsoluteEpisodeEnd, release.AbsoluteEpisode)}, true, false
	case season > 0:
		return episodeEvidence{season: season, seasonOnly: true}, true, false
	}
	return episodeEvidence{}, false, false
}

// applyEpisodeEvidence requires every release alternative with episode evidence
// to agree and to cover the target; rows without evidence are dropped.
func applyEpisodeEvidence(candidate *domain.Candidate, names []string, media domain.Media) bool {
	var evidence episodeEvidence
	found := false
	for _, name := range names {
		parsed, ok, invalid := parseEpisodeEvidence(name)
		if invalid || ok && found && parsed != evidence {
			return false
		}
		if ok {
			evidence, found = parsed, true
		}
	}
	if !found {
		return false
	}
	targetEnd := max(media.EpisodeEnd, media.Episode)
	if evidence.absolute {
		target, end := media.AbsoluteEpisode, max(media.AbsoluteEpisodeEnd, media.AbsoluteEpisode)
		if target <= 0 || target < evidence.from || end > evidence.to {
			return false
		}
		if evidence.from == evidence.to {
			candidate.AbsoluteEpisode = evidence.from
			return !media.IsEpisodeRange()
		}
		candidate.Pack = &domain.PackInfo{Scope: domain.PackRange, AbsoluteEpisodeFrom: evidence.from, AbsoluteEpisodeTo: evidence.to}
		return true
	}
	if evidence.season != media.Season {
		return false
	}
	candidate.Season = media.Season
	switch {
	case evidence.seasonOnly:
		candidate.Pack = &domain.PackInfo{Scope: domain.PackSeason, Season: media.Season}
	case evidence.from == evidence.to:
		if evidence.from != media.Episode || media.IsEpisodeRange() {
			return false
		}
		candidate.Episode = media.Episode
	default:
		if media.Episode < evidence.from || targetEnd > evidence.to {
			return false
		}
		candidate.Pack = &domain.PackInfo{Scope: domain.PackRange, Season: media.Season, EpisodeFrom: evidence.from, EpisodeTo: evidence.to}
	}
	return true
}
