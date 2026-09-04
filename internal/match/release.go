package match

import (
	"strconv"
	"strings"
	"unicode"

	"github.com/chill-institute/torrentname"
)

type Release struct {
	Raw        string
	Title      string
	Year       int
	Season     int
	Episode    int
	EpisodeEnd int
	Group      string
	Source     string
	Edition    string
	Service    string
	Resolution string
	Complete   bool
}

func ParseRelease(raw string) Release {
	release := Release{Raw: raw}
	parsed, err := torrentname.Parse(raw)
	if err == nil && parsed != nil {
		release.Title = NormalizeIdentity(parsed.Title)
		release.Year = parsed.Year
		release.Season = parsed.Season
		release.Episode = parsed.Episode
		release.EpisodeEnd = parsed.EpisodeEnd
		release.Group = NormalizeIdentity(parsed.Group)
		release.Source = normalizeSource(firstNonEmpty(parsed.Quality, parsed.Source), raw)
		release.Edition = normalizeEdition(parsed.Edition, raw, parsed.Extended, parsed.Remastered, parsed.Unrated)
		release.Resolution = strings.ToLower(parsed.Resolution)
		release.Complete = parsed.Complete
	}
	release.Service = streamingService(raw)
	if release.Source == "" {
		release.Source = normalizeSource("", raw)
	}
	if release.Edition == "" {
		release.Edition = normalizeEdition("", raw, false, false, false)
	}
	return release
}

func NormalizeIdentity(value string) string {
	var output strings.Builder
	space := false
	for _, r := range strings.ToLower(strings.TrimSpace(value)) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			if space && output.Len() > 0 {
				output.WriteByte(' ')
			}
			space = false
			output.WriteRune(r)
			continue
		}
		if r == '\'' || r == '’' {
			continue
		}
		space = true
	}
	return output.String()
}

func normalizeSource(value, raw string) string {
	joined := strings.ToLower(value + " " + raw)
	switch {
	case strings.Contains(joined, "remux"):
		return "remux"
	case strings.Contains(joined, "web-dl") || strings.Contains(joined, "webdl") || strings.Contains(joined, "web dl"):
		return "web-dl"
	case strings.Contains(joined, "webrip") || strings.Contains(joined, "web-rip"):
		return "webrip"
	case strings.Contains(joined, "bluray") || strings.Contains(joined, "blu-ray") || strings.Contains(joined, "bdrip"):
		return "bluray"
	case strings.Contains(joined, "hdtv"):
		return "hdtv"
	}
	return NormalizeIdentity(value)
}

func normalizeEdition(value, raw string, extended, remastered, unrated bool) string {
	joined := strings.ToLower(value + " " + editionDescriptor(raw))
	switch {
	case strings.Contains(joined, "director's cut") || strings.Contains(joined, "directors cut") || strings.Contains(joined, "director cut"):
		return "directors cut"
	case strings.Contains(joined, "final cut"):
		return "final cut"
	case strings.Contains(joined, "ultimate cut"):
		return "ultimate cut"
	case strings.Contains(joined, "special edition"):
		return "special edition"
	case strings.Contains(joined, "anniversary edition"):
		return "anniversary edition"
	case strings.Contains(joined, "redux"):
		return "redux"
	case extended || strings.Contains(joined, "extended"):
		return "extended"
	case remastered || strings.Contains(joined, "remaster"):
		return "remastered"
	case unrated || strings.Contains(joined, "unrated"):
		return "unrated"
	case strings.Contains(joined, "theatrical"):
		return "theatrical"
	}
	return NormalizeIdentity(value)
}

func editionDescriptor(raw string) string {
	fields := strings.Fields(strings.NewReplacer(".", " ", "_", " ", "-", " ").Replace(raw))
	for index, field := range fields {
		year, err := strconv.Atoi(field)
		if err == nil && len(field) == 4 && year >= 1900 && year <= 2099 {
			return strings.Join(fields[index+1:], " ")
		}
	}
	return strings.Join(fields, " ")
}

func streamingService(raw string) string {
	tokens := " " + strings.ToLower(strings.NewReplacer(".", " ", "_", " ", "-", " ").Replace(raw)) + " "
	switch {
	case containsToken(tokens, "nf") || strings.Contains(tokens, " netflix "):
		return "netflix"
	case containsToken(tokens, "amzn") || strings.Contains(tokens, " amazon "):
		return "amazon"
	case containsToken(tokens, "dsnp") || strings.Contains(tokens, " disney "):
		return "disney+"
	case containsToken(tokens, "atvp") || strings.Contains(tokens, " apple tv "):
		return "apple tv+"
	case containsToken(tokens, "hmax") || containsToken(tokens, "max"):
		return "max"
	case containsToken(tokens, "hulu"):
		return "hulu"
	}
	return ""
}

func containsToken(haystack, token string) bool { return strings.Contains(haystack, " "+token+" ") }

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
