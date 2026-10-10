// Package watermark removes subtitle-site advert cues from SRT subtitles.
package watermark

import (
	"bytes"
	"regexp"
	"strconv"
	"strings"
)

// siteWatermark matches a cue whose whole text, without markup, is a
// subtitle site's address or "downloaded from" line: Titlovi adds
// "www.titlovi.com" and "Preuzeto sa www.titlovi.com" to many of its
// subtitles, and re-uploads elsewhere carry them along. Spaced-out dots and
// a "@" handle form are accepted. Cues that merely mention a site, such as
// translator credits ("Preveo: ... za www.titlovi.com"), are not matched.
var siteWatermark = regexp.MustCompile(`(?i)^(?:preuzeto\s+sa?\s+)?(?:@|(?:https?://)?(?:www\s*\.\s*)?)(?:tit?lovi\s*\.\s*com|addic7ed\s*\.\s*com|subscene(?:\s*\.\s*com)?|prijevodi-online\s*\.\s*org)/?$`)

// openSubtitlesAdvert matches the adverts OpenSubtitles inserts ("Advertise
// your product or brand here", "become VIP member", "rate this subtitle"),
// in any language: they all name the site, which dialogue never does.
var openSubtitlesAdvert = regexp.MustCompile(`(?i)opensubtitles\s*\.\s*(?:org|com)`)

var cueMarkup = regexp.MustCompile(`<[^>]*>|\{[^}]*\}`)

// Strip removes watermark cues from an SRT subtitle and renumbers the rest.
// Cues are separated by any whitespace-only line; line endings and a leading
// byte order mark are kept. Other formats, a subtitle that would be left
// empty, and any payload with nothing to remove are returned unchanged.
func Strip(extension string, payload []byte) []byte {
	if strings.ToLower(extension) != ".srt" {
		return payload
	}
	newline := "\n"
	if bytes.Contains(payload, []byte("\r\n")) {
		newline = "\r\n"
	}
	text := strings.ReplaceAll(string(payload), "\r\n", "\n")
	bom := ""
	if strings.HasPrefix(text, "\ufeff") {
		bom, text = "\ufeff", strings.TrimPrefix(text, "\ufeff")
	}
	var blocks [][]string
	var current []string
	for _, line := range strings.Split(text, "\n") {
		if strings.TrimSpace(line) == "" {
			if len(current) > 0 {
				blocks = append(blocks, current)
				current = nil
			}
			continue
		}
		current = append(current, line)
	}
	if len(current) > 0 {
		blocks = append(blocks, current)
	}
	var kept [][]string
	for _, lines := range blocks {
		if len(lines) >= 3 && strings.Contains(lines[1], "-->") {
			cue := strings.TrimSpace(cueMarkup.ReplaceAllString(strings.Join(lines[2:], " "), ""))
			if siteWatermark.MatchString(cue) || openSubtitlesAdvert.MatchString(cue) {
				continue
			}
		}
		kept = append(kept, lines)
	}
	if len(kept) == len(blocks) || len(kept) == 0 {
		return payload
	}
	var out strings.Builder
	out.WriteString(bom)
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
