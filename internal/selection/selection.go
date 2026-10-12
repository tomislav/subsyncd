// Package selection parses and normalizes the human-typed titles a command
// uses to pick a media file. It depends on nothing else in the project.
package selection

import (
	"errors"
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

// TitleQuery is a parsed title selection. Year, Season and Episode are zero
// when the text did not name them.
type TitleQuery struct {
	Name    string
	Year    int
	Season  int
	Episode int
}

var (
	episodeMarker = regexp.MustCompile(`(?i)(?:^|[\s\-])(?:s(\d{1,3})e(\d{1,4})|(\d{1,3})x(\d{1,4}))(?:$|[\s\-])`)
	yearSuffix    = regexp.MustCompile(`\s*\((\d{4})\)\s*$`)
)

// ParseTitle accepts "Title", "Title (Year)", "Show S01E02", "Show 1x02" and
// "Show - S01E02 - Episode Title". Text after the episode marker is ignored.
func ParseTitle(text string) (TitleQuery, error) {
	var query TitleQuery
	name := strings.TrimSpace(text)
	if match := episodeMarker.FindStringSubmatchIndex(name); match != nil {
		group := func(first int) int {
			if match[first] >= 0 {
				value, _ := strconv.Atoi(name[match[first]:match[first+1]])
				return value
			}
			return 0
		}
		query.Season, query.Episode = group(2), group(4)
		if match[2] < 0 {
			query.Season, query.Episode = group(6), group(8)
		}
		name = name[:match[0]]
	}
	name = strings.Trim(name, " \t-")
	if match := yearSuffix.FindStringSubmatchIndex(name); match != nil {
		query.Year, _ = strconv.Atoi(name[match[2]:match[3]])
		name = strings.TrimSpace(name[:match[0]])
	}
	if name == "" {
		return TitleQuery{}, errors.New("title is empty")
	}
	query.Name = name
	return query, nil
}

// Normalize lowercases s, folds Latin diacritics, drops apostrophes and
// periods, turns other punctuation into a space and collapses whitespace.
func Normalize(s string) string {
	var out strings.Builder
	space := true
	for _, r := range strings.ToLower(s) {
		if folded, ok := fold[r]; ok {
			out.WriteString(folded)
			space = false
			continue
		}
		switch {
		case r == '\'' || r == '’' || r == '‘' || r == '`' || r == '.':
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			out.WriteRune(r)
			space = false
		default:
			if !space {
				out.WriteByte(' ')
				space = true
			}
		}
	}
	return strings.TrimSpace(out.String())
}

// fold maps lowercase Latin-1 and Latin Extended-A letters to ASCII.
var fold = buildFold()

func buildFold() map[rune]string {
	table := map[rune]string{}
	for letters, plain := range map[string]string{
		"àáâãäåāăą":  "a",
		"çćĉċč":      "c",
		"ďđð":        "d",
		"èéêëēĕėęě":  "e",
		"ĝğġģ":       "g",
		"ĥħ":         "h",
		"ìíîïĩīĭįı":  "i",
		"ĵ":          "j",
		"ķĸ":         "k",
		"ĺļľŀł":      "l",
		"ñńņňŉŋ":     "n",
		"òóôõöøōŏő":  "o",
		"ŕŗř":        "r",
		"śŝşš":       "s",
		"ţťŧ":        "t",
		"ùúûüũūŭůűų": "u",
		"ŵ":          "w",
		"ýÿŷ":        "y",
		"źżž":        "z",
	} {
		for _, r := range letters {
			table[r] = plain
		}
	}
	table['æ'] = "ae"
	table['œ'] = "oe"
	table['ß'] = "ss"
	table['þ'] = "th"
	return table
}
