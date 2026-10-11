package pack

import (
	"path/filepath"
	"slices"
	"strings"
	"unicode"

	"subsyncd/internal/domain"
	"subsyncd/internal/match"
)

// extraWords mark subtitles for bonus material shipped beside the film.
var extraWords = map[string]bool{"commentary": true, "trailer": true, "featurette": true, "featurettes": true, "extras": true, "bonus": true, "short": true, "shorts": true}

// languageWords map explicit language tags in a file name to a language. Only
// unambiguous tags are listed: two-letter codes such as "es" collide with
// release text ("DTS-ES"). The listed two-letter codes count only when written
// in capitals ("Movie.HR.srt") or placed just before the extension
// ("Movie.hr.srt"), since a lower-case "en" is often a title word.
var languageWords = map[string]domain.Language{
	"en": "en", "eng": "en", "english": "en",
	"hr": "hr", "hrv": "hr", "cro": "hr", "croatian": "hr", "hrvatski": "hr",
	"sr": "sr", "srp": "sr", "serbian": "sr", "srpski": "sr",
	"bs": "bs", "bos": "bs", "bosnian": "bs",
}

// SelectMovieAlternatives picks the subtitle of a movie archive. Bonus
// material (commentary, shorts, trailers) and files tagged with another
// language are set aside when something else remains. One remaining file is
// the movie's subtitle; two or three distinct versions are all returned so
// timing can rank them. Disc splits and more versions are refused.
func SelectMovieAlternatives(manifest Manifest, candidate domain.Candidate, wantForced bool) ([]Member, error) {
	members := eligibleMembers(manifest.Members, wantForced)
	if candidate.Forced && !wantForced || len(members) == 0 {
		return nil, selectionError(manifest, "forced_policy", 0, "no members satisfy the forced-subtitle policy")
	}
	titleWords := map[string]bool{}
	for _, word := range nameWords(candidate.Title) {
		titleWords[word] = true
	}
	members = preferWhere(members, func(member Member) bool { return !isExtra(member.SafeName, titleWords) })
	// Files tagged with the requested language beat untagged ones, which
	// in turn beat files tagged with another language.
	members = preferWhere(members, func(member Member) bool {
		language, tagged := languageTag(member.SafeName, titleWords)
		return tagged && language == candidate.Language
	})
	members = preferWhere(members, func(member Member) bool {
		language, tagged := languageTag(member.SafeName, titleWords)
		return !tagged || language == candidate.Language
	})
	var unique []Member
	seen := map[string]bool{}
	for _, member := range members {
		if member.Checksum != "" && seen[member.Checksum] {
			continue
		}
		seen[member.Checksum] = true
		unique = append(unique, member)
	}
	if slices.ContainsFunc(unique, func(member Member) bool { return match.IsMultiDisc(member.SafeName) }) {
		return nil, selectionError(manifest, "single_movie", len(unique), "movie subtitle is split across discs")
	}
	if candidate.ExactHash && len(unique) > 1 {
		// A hash match names one file; several distinct files are ambiguous.
		return nil, selectionError(manifest, "single_movie", len(unique), "movie candidate must contain exactly one eligible subtitle member")
	}
	switch {
	case len(unique) == 1:
		selected := unique[0]
		selected.SelectionRule = "single_movie"
		selected.SelectionEvidence = "single movie subtitle member satisfies subtitle policy"
		return []Member{selected}, nil
	case len(unique) <= 3:
		slices.SortFunc(unique, func(a, b Member) int { return strings.Compare(a.SafeName, b.SafeName) })
		for i := range unique {
			unique[i].SelectionRule = "movie_versions"
			unique[i].SelectionEvidence = "one of up to three movie subtitle versions, ranked by timing"
		}
		return unique, nil
	}
	return nil, selectionError(manifest, "single_movie", len(unique), "movie candidate has more subtitle versions than can be ranked")
}

// preferWhere keeps the members that satisfy keep, unless none do.
func preferWhere(members []Member, keep func(Member) bool) []Member {
	var kept []Member
	for _, member := range members {
		if keep(member) {
			kept = append(kept, member)
		}
	}
	if len(kept) == 0 {
		return members
	}
	return kept
}

func isExtra(name string, titleWords map[string]bool) bool {
	for _, word := range nameWords(name) {
		if extraWords[word] && !titleWords[word] {
			return true
		}
	}
	return false
}

func languageTag(name string, titleWords map[string]bool) (domain.Language, bool) {
	stem := strings.TrimSuffix(name, filepath.Ext(name))
	words := strings.FieldsFunc(stem, func(r rune) bool { return !unicode.IsLetter(r) })
	for index, word := range words {
		lower := strings.ToLower(word)
		language, found := languageWords[lower]
		if !found || titleWords[lower] {
			continue
		}
		if len(word) == 2 && word != strings.ToUpper(word) && index != len(words)-1 {
			continue
		}
		return language, true
	}
	return "", false
}

// nameWords splits a file name or title into lowercase letter-only words.
func nameWords(name string) []string {
	return strings.FieldsFunc(strings.ToLower(name), func(r rune) bool { return !unicode.IsLetter(r) })
}
