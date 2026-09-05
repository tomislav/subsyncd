package pack

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"subsyncd/internal/domain"
)

type SelectionError struct {
	Reason              string
	Rule                string
	ArchiveType         string
	MemberCount         int
	MatchingMemberCount int
}

func (e *SelectionError) Error() string { return "subtitle archive member rejected: " + e.Reason }

var (
	episodeTokenPattern  = regexp.MustCompile(`(?i)(?:s(\d{1,3})e(\d{1,4})|(\d{1,3})x(\d{1,4}))`)
	episodeRangePatterns = []*regexp.Regexp{
		regexp.MustCompile(`(?i)s(\d{1,3})e(\d{1,4})-e(\d{1,4})`),
		regexp.MustCompile(`(?i)s(\d{1,3})e(\d{1,4})-s(\d{1,3})e(\d{1,4})`),
		regexp.MustCompile(`(?i)(\d{1,3})x(\d{1,4})-(\d{1,3})x(\d{1,4})`),
	}
	rangeEndpointBeforePattern = regexp.MustCompile(`(?i)(?:s\d{1,3}e\d{1,4}|\d{1,3}x\d{1,4}|e\d{1,4})$`)
	rangeEndpointAfterPattern  = regexp.MustCompile(`(?i)^(?:s\d{1,3}e\d{1,4}|\d{1,3}x\d{1,4}|e\d{1,4})(?:$|[^[:alnum:]])`)
	episodeContinuationPattern = regexp.MustCompile(`(?i)^(?:s\d{1,3}e\d{0,4}|\d{1,3}x\d{0,4}|e\d{0,4})`)
	absolutePattern            = regexp.MustCompile(`(?i)(?:\bEP|\bABS(?:OLUTE)?[ ._-]*)(\d{2,5})\b`)
	forcedPattern              = regexp.MustCompile(`(?i)(?:^|[ ._-])forced(?:[ ._-]|$)`)
)

func Select(manifest Manifest, candidate domain.Candidate, media domain.Media, wantForced bool) (Member, error) {
	members := eligibleMembers(manifest.Members, wantForced)
	if len(members) == 0 {
		return Member{}, selectionError(manifest, "forced_policy", 0, "no members satisfy the forced-subtitle policy")
	}
	if candidate.Pack != nil && len(candidate.Pack.DirectMembers) != 0 {
		var matches []Member
		seen := map[string]struct{}{}
		for _, direct := range candidate.Pack.DirectMembers {
			if !directMatchesMedia(direct, media) {
				continue
			}
			for _, member := range members {
				if strings.EqualFold(filepath.Base(direct.Filename), member.SafeName) {
					key := strings.ToLower(member.SafeName)
					if _, exists := seen[key]; exists {
						continue
					}
					seen[key] = struct{}{}
					matches = append(matches, member)
				}
			}
		}
		if selected, done, err := uniqueRule(manifest, matches, "provider_direct", "provider supplied a direct member for the target episode"); done {
			return selected, err
		}
	}

	var episodeMatches []Member
	for _, member := range members {
		if hasInvalidOrAmbiguousRangeEvidence(member.SafeName) {
			continue
		}
		if _, _, _, ranged := episodeRange(member.SafeName); ranged {
			continue
		}
		season, episode, found := episodeToken(member.SafeName)
		if found && season == media.Season && episode == media.Episode {
			episodeMatches = append(episodeMatches, member)
		}
	}
	if selected, done, err := uniqueRule(manifest, episodeMatches, "episode_token", fmt.Sprintf("filename identifies S%02dE%02d", media.Season, media.Episode)); done {
		return selected, err
	}

	var rangeMatches []Member
	for _, member := range members {
		if hasInvalidOrAmbiguousRangeEvidence(member.SafeName) {
			continue
		}
		season, from, to, found := episodeRange(member.SafeName)
		if found && season == media.Season && from <= media.Episode && media.Episode <= to {
			rangeMatches = append(rangeMatches, member)
		}
	}
	if selected, done, err := uniqueRule(manifest, rangeMatches, "episode_range", fmt.Sprintf("filename range contains S%02dE%02d", media.Season, media.Episode)); done {
		return selected, err
	}

	if media.AbsoluteEpisode > 0 {
		var absoluteMatches []Member
		for _, member := range members {
			if absolute, found := absoluteEpisode(member.SafeName); found && absolute == media.AbsoluteEpisode {
				absoluteMatches = append(absoluteMatches, member)
			}
		}
		if selected, done, err := uniqueRule(manifest, absoluteMatches, "absolute_episode", fmt.Sprintf("filename identifies absolute episode %d", media.AbsoluteEpisode)); done {
			return selected, err
		}
	}
	if len([]rune(normalizeTitle(media.EpisodeTitle))) > 4 {
		var titleMatches []Member
		for _, member := range members {
			if hasInvalidOrAmbiguousRangeEvidence(member.SafeName) || hasEpisodeEvidence(member.SafeName) || member.NormalizedTitle == "" {
				continue
			}
			if similarity(normalizeTitle(media.EpisodeTitle), normalizeTitle(member.NormalizedTitle)) >= 0.98 {
				titleMatches = append(titleMatches, member)
			}
		}
		if selected, done, err := uniqueRule(manifest, titleMatches, "episode_title", "normalized episode title similarity is at least 0.98"); done {
			return selected, err
		}
	}
	return Member{}, selectionError(manifest, "none", 0, "no unique member identifies the target episode")
}

// SelectSingleEpisode preserves compatibility with providers that return one
// generically named subtitle while rejecting any explicit conflicting episode
// evidence through the normal strict selector.
func SelectSingleEpisode(manifest Manifest, candidate domain.Candidate, media domain.Media, wantForced bool) (Member, error) {
	selected, err := Select(manifest, candidate, media, wantForced)
	if err == nil {
		return selected, nil
	}
	members := eligibleMembers(manifest.Members, wantForced)
	if candidate.Pack == nil && len(members) == 1 && !hasInvalidOrAmbiguousRangeEvidence(members[0].SafeName) && !hasEpisodeEvidence(members[0].SafeName) {
		selected = members[0]
		selected.SelectionRule = "single_generic"
		selected.SelectionEvidence = "single subtitle member has no conflicting episode evidence"
		return selected, nil
	}
	return Member{}, err
}

// SelectSingleMovie permits only one archive member after applying the same
// forced-subtitle policy used for every other selection shape.
func SelectSingleMovie(manifest Manifest, candidate domain.Candidate, wantForced bool) (Member, error) {
	members := eligibleMembers(manifest.Members, wantForced)
	if candidate.Forced && !wantForced || len(members) == 0 {
		return Member{}, selectionError(manifest, "forced_policy", 0, "no members satisfy the forced-subtitle policy")
	}
	if len(members) != 1 || len(manifest.Members) != 1 {
		return Member{}, selectionError(manifest, "single_movie", len(members), "movie candidate must contain exactly one eligible subtitle member")
	}
	selected := members[0]
	selected.SelectionRule = "single_movie"
	selected.SelectionEvidence = "single movie subtitle member satisfies subtitle policy"
	return selected, nil
}

func eligibleMembers(members []Member, wantForced bool) []Member {
	result := make([]Member, 0, len(members))
	for _, member := range members {
		forced := member.Forced || forcedPattern.MatchString(member.SafeName)
		if forced != wantForced {
			continue
		}
		member.Forced = forced
		result = append(result, member)
	}
	return result
}

func directMatchesMedia(direct domain.PackMemberRef, media domain.Media) bool {
	standard := direct.Episode == media.Episode && (direct.Season == 0 || direct.Season == media.Season)
	absolute := media.AbsoluteEpisode > 0 && direct.AbsoluteEpisode == media.AbsoluteEpisode
	return standard || absolute
}

func uniqueRule(manifest Manifest, matches []Member, rule, evidence string) (Member, bool, error) {
	switch len(matches) {
	case 0:
		return Member{}, false, nil
	case 1:
		selected := matches[0]
		selected.SelectionRule = rule
		selected.SelectionEvidence = evidence
		return selected, true, nil
	default:
		return Member{}, true, selectionError(manifest, rule, len(matches), rule+" matched multiple members")
	}
}

func selectionError(manifest Manifest, rule string, matchingMembers int, reason string) *SelectionError {
	return &SelectionError{Reason: reason, Rule: rule, ArchiveType: manifest.ArchiveType, MemberCount: len(manifest.Members), MatchingMemberCount: matchingMembers}
}

func episodeToken(name string) (int, int, bool) {
	match := episodeTokenPattern.FindStringSubmatch(name)
	if len(match) == 0 {
		return 0, 0, false
	}
	if match[1] != "" {
		season, _ := strconv.Atoi(match[1])
		episode, _ := strconv.Atoi(match[2])
		return season, episode, true
	}
	season, _ := strconv.Atoi(match[3])
	episode, _ := strconv.Atoi(match[4])
	return season, episode, true
}

func episodeRange(name string) (int, int, int, bool) {
	_, season, from, to, found := acceptedEpisodeRangeMatch(name)
	return season, from, to, found
}

type episodeRangeMatch struct {
	indices      []int
	patternIndex int
}

func acceptedEpisodeRangeMatch(name string) (episodeRangeMatch, int, int, int, bool) {
	matches := rangeLikeEpisodeMatches(name)
	if len(matches) != 1 {
		return episodeRangeMatch{}, 0, 0, 0, false
	}
	match := matches[0]
	season, from, to, found := acceptedEpisodeRange(name, match.indices, match.patternIndex)
	if !found {
		return episodeRangeMatch{}, 0, 0, 0, false
	}
	return match, season, from, to, true
}

func rangeLikeEpisodeMatches(name string) []episodeRangeMatch {
	var matches []episodeRangeMatch
	for patternIndex, pattern := range episodeRangePatterns {
		for _, match := range pattern.FindAllStringSubmatchIndex(name, -1) {
			if completeRangeToken(name, match[0], match[1]) {
				matches = append(matches, episodeRangeMatch{indices: match, patternIndex: patternIndex})
			}
		}
	}
	return matches
}

func hasInvalidOrAmbiguousRangeEvidence(name string) bool {
	if hasMalformedHyphenatedRangeEvidence(name) {
		return true
	}
	ranged, _, _, _, accepted := acceptedEpisodeRangeMatch(name)
	// Detect episode-shaped continuations independently of complete ranges.
	// Missing endpoints and ordinary separators must not expose a single-token
	// fallback, while punctuation followed only by release text stays harmless.
	for _, token := range episodeTokenPattern.FindAllStringIndex(name, -1) {
		if completeRangeToken(name, token[0], token[1]) && episodeContinuation(name, token[1]) {
			if !accepted || token[0] != ranged.indices[0] {
				return true
			}
		}
	}
	return len(rangeLikeEpisodeMatches(name)) != 0 && !accepted
}

func episodeContinuation(name string, offset int) bool {
	suffix := strings.TrimLeftFunc(name[offset:], func(r rune) bool {
		return unicode.IsSpace(r) || r == '.' || r == '_' || r == '-'
	})
	match := episodeContinuationPattern.FindStringIndex(suffix)
	if match == nil {
		return false
	}
	if match[1] == len(suffix) {
		return true
	}
	next, _ := utf8.DecodeRuneInString(suffix[match[1]:])
	// Combining marks continue the preceding letter in decomposed release
	// text, so an accented E must not become an incomplete episode endpoint.
	return !isAlphaNumeric(next) && !unicode.IsMark(next)
}

func hasMalformedHyphenatedRangeEvidence(name string) bool {
	for _, pattern := range episodeRangePatterns {
		for _, match := range pattern.FindAllStringSubmatchIndex(name, -1) {
			if !hasRangeStartBoundary(name, match[0]) || match[1] == len(name) {
				continue
			}
			next, _ := utf8.DecodeRuneInString(name[match[1]:])
			if isAlphaNumeric(next) {
				return true
			}
		}
	}
	return false
}

func acceptedEpisodeRange(name string, match []int, patternIndex int) (int, int, int, bool) {
	if !completeRangeToken(name, match[0], match[1]) || rangeIsChained(name, match[0], match[1]) || episodeContinuation(name, match[1]) {
		return 0, 0, 0, false
	}
	season, from, to, endSeason := parseRangeMatch(name, match, patternIndex)
	if season <= 0 || from <= 0 || to <= 0 || endSeason != season || from > to {
		return 0, 0, 0, false
	}
	return season, from, to, true
}

func parseRangeMatch(name string, match []int, patternIndex int) (season, from, to, endSeason int) {
	value := func(index int) int {
		parsed, _ := strconv.Atoi(name[match[index]:match[index+1]])
		return parsed
	}
	switch patternIndex {
	case 0:
		season, from, to = value(2), value(4), value(6)
		return season, from, to, season
	case 1:
		return value(2), value(4), value(8), value(6)
	default:
		return value(2), value(4), value(8), value(6)
	}
}

func completeRangeToken(name string, start, end int) bool {
	if !hasRangeStartBoundary(name, start) {
		return false
	}
	if end < len(name) {
		next, _ := utf8.DecodeRuneInString(name[end:])
		if isAlphaNumeric(next) {
			return false
		}
	}
	return true
}

func hasRangeStartBoundary(name string, start int) bool {
	if start == 0 {
		return true
	}
	previous, _ := utf8.DecodeLastRuneInString(name[:start])
	return !isAlphaNumeric(previous)
}

func rangeIsChained(name string, start, end int) bool {
	before := start > 0 && name[start-1] == '-' && rangeEndpointBeforePattern.MatchString(name[:start-1])
	after := end < len(name) && name[end] == '-' && rangeEndpointAfterPattern.MatchString(name[end+1:])
	return before || after
}

func isAlphaNumeric(value rune) bool {
	return unicode.IsLetter(value) || unicode.IsNumber(value)
}

func absoluteEpisode(name string) (int, bool) {
	match := absolutePattern.FindStringSubmatch(name)
	if len(match) != 2 {
		return 0, false
	}
	value, _ := strconv.Atoi(match[1])
	return value, value > 0
}

func memberEvidence(name, seriesTitle string) Member {
	member := Member{SafeName: name, Forced: forcedPattern.MatchString(name)}
	if season, from, to, found := episodeRange(name); found {
		member.Season, member.EpisodeFrom, member.EpisodeTo = season, from, to
	} else if season, episode, found := episodeToken(name); found {
		member.Season, member.EpisodeFrom, member.EpisodeTo = season, episode, episode
	} else if absolute, found := absoluteEpisode(name); found {
		member.AbsoluteFrom, member.AbsoluteTo = absolute, absolute
	}
	member.NormalizedTitle = filenameTitle(name, seriesTitle)
	return member
}

func filenameTitle(name, seriesTitle string) string {
	stem := strings.TrimSuffix(filepath.Base(name), filepath.Ext(name))
	stem = removeAcceptedEpisodeRanges(stem)
	stem = episodeTokenPattern.ReplaceAllString(stem, " ")
	stem = absolutePattern.ReplaceAllString(stem, " ")
	stem = forcedPattern.ReplaceAllString(stem, " ")
	normalized := normalizeTitle(stem)
	series := normalizeTitle(seriesTitle)
	if series != "" && strings.HasPrefix(normalized, series+" ") {
		normalized = strings.TrimSpace(strings.TrimPrefix(normalized, series))
	}
	return normalized
}

func removeAcceptedEpisodeRanges(name string) string {
	match, _, _, _, found := acceptedEpisodeRangeMatch(name)
	if !found {
		return name
	}
	return name[:match.indices[0]] + " " + name[match.indices[1]:]
}

func normalizeTitle(value string) string {
	var builder strings.Builder
	space := false
	for _, r := range strings.ToLower(value) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r > 127 {
			if space && builder.Len() != 0 {
				builder.WriteByte(' ')
			}
			space = false
			builder.WriteRune(r)
		} else {
			space = true
		}
	}
	return builder.String()
}

func hasEpisodeEvidence(name string) bool {
	_, _, episode := episodeToken(name)
	_, _, _, episodeRange := episodeRange(name)
	_, absolute := absoluteEpisode(name)
	return episode || episodeRange || absolute
}

func similarity(left, right string) float64 {
	a, b := []rune(left), []rune(right)
	maximum := len(a)
	if len(b) > maximum {
		maximum = len(b)
	}
	if maximum == 0 {
		return 1
	}
	previous := make([]int, len(b)+1)
	for index := range previous {
		previous[index] = index
	}
	for i, leftRune := range a {
		current := make([]int, len(b)+1)
		current[0] = i + 1
		for j, rightRune := range b {
			cost := 0
			if leftRune != rightRune {
				cost = 1
			}
			current[j+1] = min(current[j]+1, previous[j+1]+1, previous[j]+cost)
		}
		previous = current
	}
	return 1 - float64(previous[len(b)])/float64(maximum)
}
