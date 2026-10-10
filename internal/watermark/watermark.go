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
// a "@" handle form are accepted. It also matches a single line of a cue:
// such a line is dropped and the cue's other lines are kept. Text that merely
// mentions a site ("Preveo: ... za www.titlovi.com") is not matched.
var siteWatermark = regexp.MustCompile(`(?i)^(?:preuzeto\s+sa?\s+)?(?:@|(?:https?://)?(?:www\s*\.\s*)?)(?:(?:divx-)?tit?lovi\s*\.\s*com|addic7ed\s*\.\s*com|subscene(?:\s*\.\s*com)?|prijevodi-online\s*\.\s*org)/?$`)

// openSubtitlesAdvert matches the adverts OpenSubtitles inserts ("Advertise
// your product or brand here", "become VIP member", "rate this subtitle"),
// in any language: they all name the site, which dialogue never does.
var openSubtitlesAdvert = regexp.MustCompile(`(?i)opensubtitles\s*\.\s*(?:org|com)`)

var cueMarkup = regexp.MustCompile(`<[^>]*>|\{[^}]*\}`)

// signatureCredit matches the credit line of a site signature cue, such as
// Addic7ed's "Synced and corrected by <name>", "provided by <name>" or
// "WEB-DL resync by <name>". It only counts together with a site address line
// in the same cue, so a credit on its own, or dialogue such as "corrected by
// now.", is kept.
var signatureCredit = regexp.MustCompile(`(?i)^(?:(?:web-?dl|web-?rip|blu-?ray|b[dr]-?rip|hdtv|dvd-?rip)\s+)?(?:(?:re-?)?sync(?:ed)?|subtitles?|corrections?|corrected|provided)(?:\s*(?:and|&)\s*correct(?:ed|ions))?\s+by\s+\S`)

// isSignature reports whether every line of a cue is a site address or a
// signature credit, with at least one address: Addic7ed's
// "- Synced and corrected by X -" / "- www.addic7ed.com -" pair.
func isSignature(lines []string) bool {
	address := false
	for _, line := range lines {
		line = plainLine(line)
		switch {
		case line == "":
		case siteWatermark.MatchString(line):
			address = true
		case signatureCredit.MatchString(line):
		default:
			return false
		}
	}
	return address
}

// plainLine returns a cue line without markup and Addic7ed's framing dashes.
func plainLine(line string) string {
	return strings.Trim(strings.TrimSpace(cueMarkup.ReplaceAllString(line, "")), "- \t")
}

// withoutAddressLines drops the lines of a cue that are only a site address,
// such as "www.titlovi.com" under a translator credit. It reports false when
// no line was dropped or when nothing would be left.
func withoutAddressLines(lines []string) ([]string, bool) {
	kept := make([]string, 0, len(lines))
	for _, line := range lines {
		if !siteWatermark.MatchString(plainLine(line)) {
			kept = append(kept, line)
		}
	}
	if len(kept) == len(lines) || len(kept) == 0 {
		return lines, false
	}
	return kept, true
}

// Strip removes watermark cues from an SRT subtitle and renumbers the rest.
// A site address line in a cue that also has other text, such as a
// translator credit, is dropped on its own. Cues are separated by any
// whitespace-only line; line endings and a leading byte order mark are kept. Other formats, a subtitle that would be left
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
	changed := false
	for _, lines := range blocks {
		if len(lines) >= 3 && strings.Contains(lines[1], "-->") {
			cue := strings.TrimSpace(cueMarkup.ReplaceAllString(strings.Join(lines[2:], " "), ""))
			if siteWatermark.MatchString(cue) || openSubtitlesAdvert.MatchString(cue) || isSignature(lines[2:]) {
				changed = true
				continue
			}
			if text, dropped := withoutAddressLines(lines[2:]); dropped {
				lines = append(lines[:2:2], text...)
				changed = true
			}
		}
		kept = append(kept, lines)
	}
	if !changed || len(kept) == 0 {
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
