package catalog

import (
	"regexp"
	"strconv"
	"strings"

	"subsyncd/internal/match"
)

var sceneEpisodeMarker = regexp.MustCompile(`(?i)^S\d{1,2}(?:E\d{1,4})?$`)

// streamingServiceFromSceneName requires a web release and an identity boundary
// before the service marker, so movie/show title words cannot supply evidence.
func streamingServiceFromSceneName(raw string) string {
	source := match.ParseRelease(raw).Source
	if source != "web-dl" && source != "webrip" {
		return ""
	}
	fields := strings.Fields(strings.NewReplacer(".", " ", "_", " ", "-", " ").Replace(raw))
	start := -1
	for index, field := range fields {
		year, err := strconv.Atoi(field)
		if sceneEpisodeMarker.MatchString(field) || err == nil && len(field) == 4 && year >= 1900 && year <= 2099 {
			start = index + 1
		}
	}
	if start < 0 {
		return ""
	}
	return match.ParseRelease(strings.Join(fields[start:], " ")).Service
}
