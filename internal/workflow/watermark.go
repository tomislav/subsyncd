package workflow

import (
	"bytes"
	"regexp"
	"strconv"
	"strings"
)

// siteWatermark matches a cue whose whole text, without markup, is a
// subtitle site's address. Titlovi adds one ("www.titlovi.com") to many of
// its subtitles. Cues that merely mention the site, such as translator
// credits, are not matched.
var siteWatermark = regexp.MustCompile(`(?i)^(?:https?://)?(?:www\.)?titlovi\.com/?$`)

var cueMarkup = regexp.MustCompile(`<[^>]*>|\{[^}]*\}`)

// stripSiteWatermarks removes watermark cues from an SRT subtitle and
// renumbers the rest. Other formats, and a subtitle that would be left empty,
// are returned unchanged; so is any payload with nothing to remove.
func stripSiteWatermarks(extension string, payload []byte) []byte {
	if strings.ToLower(extension) != ".srt" {
		return payload
	}
	newline := "\n"
	if bytes.Contains(payload, []byte("\r\n")) {
		newline = "\r\n"
	}
	text := strings.ReplaceAll(string(payload), "\r\n", "\n")
	var kept [][]string
	removed := false
	for _, block := range strings.Split(strings.Trim(text, "\n"), "\n\n") {
		lines := strings.Split(block, "\n")
		if len(lines) >= 3 && strings.Contains(lines[1], "-->") {
			cue := strings.TrimSpace(cueMarkup.ReplaceAllString(strings.Join(lines[2:], " "), ""))
			if siteWatermark.MatchString(cue) {
				removed = true
				continue
			}
		}
		kept = append(kept, lines)
	}
	if !removed || len(kept) == 0 {
		return payload
	}
	var out strings.Builder
	for index, lines := range kept {
		if _, err := strconv.Atoi(strings.TrimSpace(lines[0])); err == nil && len(lines) >= 2 && strings.Contains(lines[1], "-->") {
			lines[0] = strconv.Itoa(index + 1)
		}
		if index > 0 {
			out.WriteString("\n")
		}
		out.WriteString(strings.Join(lines, "\n"))
		out.WriteString("\n")
	}
	return []byte(strings.ReplaceAll(out.String(), "\n", newline))
}
