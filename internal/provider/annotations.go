package provider

import (
	"strings"
	"unicode"
)

// Annotations interprets explicit forced and hearing-impaired annotation tokens
// only. Negation is scoped to each marker; filename punctuation separates words,
// while prose punctuation separates clauses. Neither negative prose nor absent
// member flags erase structured HI evidence.
func Annotations(name, comment string, structuredHI bool) (forced, hearing bool) {
	hearing = structuredHI
	for sourceIndex, source := range []string{name, comment} {
		clauses := []string{strings.ToLower(source)}
		if sourceIndex == 1 {
			clauses = strings.FieldsFunc(clauses[0], func(r rune) bool { return r == ';' || r == '\n' || r == '.' || r == ',' })
		}
		for _, clause := range clauses {
			words := strings.FieldsFunc(clause, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
			if strings.Contains(strings.Join(words, " "), "not sure") {
				continue
			}
			type marker struct {
				start, end int
				forced     bool
			}
			var markers []marker
			for i, word := range words {
				switch {
				case word == "forced":
					markers = append(markers, marker{i, i + 1, true})
				case word == "sdh" || sourceIndex == 0 && word == "hi":
					markers = append(markers, marker{i, i + 1, false})
				case word == "hearing" && i+1 < len(words) && words[i+1] == "impaired":
					markers = append(markers, marker{i, i + 2, false})
				}
			}
			previousEnd := 0
			previousNegative := false
			for index, m := range markers {
				negative := false
				inheritedNegative := previousNegative
				for _, word := range words[previousEnd:m.start] {
					switch word {
					case "with", "but":
						negative, inheritedNegative = false, false
					case "and", "or":
						negative = negative || inheritedNegative
					case "no", "not", "non", "without", "remove", "exclude", "strip", "removed", "excluded", "stripped":
						negative = true
					}
				}
				nextStart := len(words)
				if index+1 < len(markers) {
					nextStart = markers[index+1].start
				}
			suffix:
				for _, word := range words[m.end:nextStart] {
					switch word {
					case "with", "without", "but", "and", "or", "no", "not", "non":
						break suffix
					case "removed", "stripped", "excluded", "free":
						negative = true
					}
				}
				if !negative {
					if m.forced {
						forced = true
					} else {
						hearing = true
					}
				}
				previousEnd = m.end
				previousNegative = negative
			}
		}
	}
	return
}
