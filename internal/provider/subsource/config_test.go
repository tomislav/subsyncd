package subsource

import (
	"slices"
	"strings"
	"testing"

	"subsyncd/internal/domain"
)

func TestDecodeConfigAppliesDefaults(t *testing.T) {
	config, err := DecodeConfig([]byte("type: subsource\napi_key: test-key\n"))
	if err != nil {
		t.Fatal(err)
	}
	if config.BaseURL != "https://api.subsource.net/api/v1" || config.RequestsPerSecond != 0.5 || config.Burst != 1 || config.MaxConcurrent != 1 || config.MaxPages != 5 || config.MaxDownloadBytes != 20<<20 {
		t.Fatalf("config = %#v", config)
	}
}

func TestDecodeConfigRejectsInvalidSettings(t *testing.T) {
	for name, payload := range map[string]string{
		"missing key":        "type: subsource\n",
		"unknown field":      "type: subsource\napi_key: k\nlanguage: english\n",
		"http base":          "type: subsource\napi_key: k\nbase_url: http://api.example.test/api/v1\n",
		"negative rate":      "type: subsource\napi_key: k\nrequests_per_second: -1\n",
		"too many pages":     "type: subsource\napi_key: k\nmax_pages: 21\n",
		"download cap raise": "type: subsource\napi_key: k\nmax_download_bytes: 20971521\n",
		"negative download":  "type: subsource\napi_key: k\nmax_download_bytes: -1\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeConfig([]byte(payload)); err == nil {
				t.Fatal("expected configuration error")
			}
		})
	}
}

func TestDecodeConfigErrorsNeverContainKey(t *testing.T) {
	_, err := DecodeConfig([]byte("type: subsource\napi_key: secret-value\nmax_pages: 99\n"))
	if err == nil || strings.Contains(err.Error(), "secret-value") {
		t.Fatalf("error = %v", err)
	}
}

func TestLanguageTableTagsAreCanonical(t *testing.T) {
	for _, entry := range languageTable {
		parsed, err := domain.ParseLanguage(string(entry.tag))
		if err != nil {
			t.Errorf("slug %s: tag %s does not parse: %v", entry.slug, entry.tag, err)
			continue
		}
		if parsed != entry.tag {
			t.Errorf("slug %s: tag %s canonicalizes to %s", entry.slug, entry.tag, parsed)
		}
	}
}

func TestLanguageTableExcludesPseudoLanguages(t *testing.T) {
	seen := map[string]bool{}
	for _, entry := range languageTable {
		if seen[entry.slug] {
			t.Errorf("slug %s appears twice", entry.slug)
		}
		seen[entry.slug] = true
	}
	for _, slug := range []string{"big_5_code", "chinese_bg_code", "chinese_bilingual"} {
		if seen[slug] {
			t.Errorf("pseudo-language %s must not be mapped", slug)
		}
	}
	if len(seen) < 100 {
		t.Fatalf("language table has %d slugs; want the full SubSource list", len(seen))
	}
}

func TestLanguageTableDeclaresEveryCollision(t *testing.T) {
	bySlugs := map[domain.Language][]string{}
	for _, entry := range languageTable {
		bySlugs[entry.tag] = append(bySlugs[entry.tag], entry.slug)
	}
	for tag, slugs := range bySlugs {
		if len(slugs) > 1 && !slices.Contains(sharedTags, tag) {
			t.Errorf("tag %s is reached by %v but is not declared shared", tag, slugs)
		}
	}
	for _, tag := range sharedTags {
		if len(bySlugs[tag]) < 2 {
			t.Errorf("declared shared tag %s has slugs %v", tag, bySlugs[tag])
		}
	}
}

func TestLanguageSlugs(t *testing.T) {
	for tag, want := range map[domain.Language][]string{
		"en":     {"english"},
		"hr":     {"croatian"},
		"pt-BR":  {"brazilian_portuguese"},
		"es-419": {"spanish_latin_america"},
		"si":     {"sinhala", "sinhalese"},
		"ps":     {"pashto", "pushto"},
		"fil":    {"filipino", "tagalog"},
	} {
		if got := languageSlugs(tag); !slices.Equal(got, want) {
			t.Errorf("languageSlugs(%s) = %v; want %v", tag, got, want)
		}
	}
	for _, tag := range []domain.Language{"tlh", "pt-PT", ""} {
		if got := languageSlugs(tag); len(got) != 0 {
			t.Errorf("languageSlugs(%s) = %v; want none", tag, got)
		}
	}
}
