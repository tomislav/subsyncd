package domain

import (
	"fmt"
	"strings"

	"golang.org/x/text/language"
)

// Language is a canonical BCP 47 language tag.
type Language string

var languageAliases = map[string]string{
	"eng": "en",
	"hrv": "hr",
	"scr": "hr",
	"srp": "sr",
	"bos": "bs",
}

func ParseLanguage(raw string) (Language, error) {
	value := strings.TrimSpace(raw)
	if alias, ok := languageAliases[strings.ToLower(value)]; ok {
		value = alias
	}
	if value == "" {
		return "", fmt.Errorf("language tag is empty")
	}
	if strings.Contains(value, "_") {
		return "", fmt.Errorf("language %q is not a BCP 47 tag", raw)
	}

	tag, err := language.Parse(value)
	if err != nil {
		return "", fmt.Errorf("parse language %q: %w", raw, err)
	}
	if tag == language.Und {
		return "", fmt.Errorf("language %q is undetermined", raw)
	}
	return Language(tag.String()), nil
}

func (l Language) String() string {
	return string(l)
}

func EquivalentLanguage(a, b Language) bool {
	left, err := ParseLanguage(string(a))
	if err != nil {
		return false
	}
	right, err := ParseLanguage(string(b))
	return err == nil && left == right
}

// TrackCoversLanguage reports whether a subtitle track in trackLanguage
// satisfies a wanted language when subsyncd has not installed one itself: the
// languages are equivalent, the track is not forced-only, and it is not
// hearing-impaired unless allowHearingImpaired is set.
func TrackCoversLanguage(trackLanguage Language, forced, hearingImpaired bool, want Language, allowHearingImpaired bool) bool {
	if trackLanguage == "" || forced || !EquivalentLanguage(trackLanguage, want) {
		return false
	}
	return !hearingImpaired || allowHearingImpaired
}
