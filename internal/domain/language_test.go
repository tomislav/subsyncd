package domain

import "testing"

func TestParseLanguageCanonicalizesSupportedAliases(t *testing.T) {
	tests := map[string]Language{
		"eng":   "en",
		"EN":    "en",
		"hrv":   "hr",
		"scr":   "hr",
		"srp":   "sr",
		"bos":   "bs",
		"pt-br": "pt-BR",
	}
	for input, want := range tests {
		t.Run(input, func(t *testing.T) {
			got, err := ParseLanguage(input)
			if err != nil {
				t.Fatalf("ParseLanguage(%q) error = %v", input, err)
			}
			if got != want {
				t.Errorf("ParseLanguage(%q) = %q, want %q", input, got, want)
			}
		})
	}
}

func TestParseLanguageRejectsEmptyUnknownAndMalformedTags(t *testing.T) {
	for _, input := range []string{"", "und", "not_a_language"} {
		t.Run(input, func(t *testing.T) {
			if _, err := ParseLanguage(input); err == nil {
				t.Fatalf("ParseLanguage(%q) error = nil", input)
			}
		})
	}
}

func TestEquivalentLanguageUsesCanonicalTags(t *testing.T) {
	if !EquivalentLanguage("hrv", "hr") {
		t.Fatal("EquivalentLanguage(hrv, hr) = false")
	}
	if EquivalentLanguage("hr", "sr-Latn") {
		t.Fatal("EquivalentLanguage(hr, sr-Latn) = true")
	}
}
