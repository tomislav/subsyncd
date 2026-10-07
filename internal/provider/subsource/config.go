package subsource

import (
	"bytes"
	"fmt"
	"net/url"

	"gopkg.in/yaml.v3"

	"subsyncd/internal/domain"
)

const (
	defaultBaseURL   = "https://api.subsource.net/api/v1"
	maxDownloadBytes = 20 << 20
	defaultMaxPages  = 5
	maximumMaxPages  = 20
)

type Config struct {
	Type                  string  `yaml:"type"`
	APIKey                string  `yaml:"api_key"`
	BaseURL               string  `yaml:"base_url"`
	MaxPages              int     `yaml:"max_pages"`
	MaxDownloadBytes      int64   `yaml:"max_download_bytes"`
	RequestsPerSecond     float64 `yaml:"requests_per_second"`
	Burst                 int     `yaml:"burst"`
	MaxConcurrent         int     `yaml:"max_concurrent"`
	AllowInsecureForTests bool    `yaml:"-"`
}

func DecodeConfig(payload []byte) (Config, error) {
	var config Config
	decoder := yaml.NewDecoder(bytes.NewReader(payload))
	decoder.KnownFields(true)
	if err := decoder.Decode(&config); err != nil {
		return Config{}, fmt.Errorf("decode SubSource config: %w", err)
	}
	config.applyDefaults()
	if err := config.Validate(); err != nil {
		return Config{}, err
	}
	return config, nil
}

func (c *Config) applyDefaults() {
	if c.BaseURL == "" {
		c.BaseURL = defaultBaseURL
	}
	if c.MaxPages == 0 {
		c.MaxPages = defaultMaxPages
	}
	if c.MaxDownloadBytes == 0 {
		c.MaxDownloadBytes = maxDownloadBytes
	}
	if c.RequestsPerSecond == 0 {
		c.RequestsPerSecond = 0.5
	}
	if c.Burst == 0 {
		c.Burst = 1
	}
	if c.MaxConcurrent == 0 {
		c.MaxConcurrent = 1
	}
}

func (c Config) Validate() error {
	if c.APIKey == "" {
		return fmt.Errorf("SubSource api_key is required")
	}
	parsed, err := url.Parse(c.BaseURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" {
		return fmt.Errorf("SubSource base URL is invalid")
	}
	if parsed.Scheme != "https" && !c.AllowInsecureForTests {
		return fmt.Errorf("SubSource base URL must use HTTPS")
	}
	if c.MaxPages < 1 || c.MaxPages > maximumMaxPages {
		return fmt.Errorf("SubSource max_pages must be between 1 and %d", maximumMaxPages)
	}
	if c.MaxDownloadBytes <= 0 || c.MaxDownloadBytes > maxDownloadBytes {
		return fmt.Errorf("SubSource max_download_bytes must be positive and at most %d", maxDownloadBytes)
	}
	if c.RequestsPerSecond <= 0 || c.Burst <= 0 || c.MaxConcurrent <= 0 {
		return fmt.Errorf("SubSource request limits must be positive")
	}
	return nil
}

type languageEntry struct {
	slug string
	tag  domain.Language
}

// languageTable maps SubSource's language slugs (its front-end language enum)
// to canonical BCP 47 tags. SubSource answers an unknown slug with an empty
// success, so every slug here must be exact. The encoding and mixed-language
// pseudo-languages big_5_code, chinese_bg_code and chinese_bilingual have no
// tag and are omitted.
var languageTable = []languageEntry{
	{"abkhazian", "ab"}, {"afrikaans", "af"}, {"albanian", "sq"}, {"amharic", "am"},
	{"arabic", "ar"}, {"aragonese", "an"}, {"armenian", "hy"}, {"assamese", "as"},
	{"asturian", "ast"}, {"azerbaijani", "az"}, {"basque", "eu"}, {"belarusian", "be"},
	{"bengali", "bn"}, {"bosnian", "bs"}, {"brazilian_portuguese", "pt-BR"}, {"breton", "br"},
	{"bulgarian", "bg"}, {"burmese", "my"}, {"catalan", "ca"}, {"chinese", "zh"},
	{"chinese_cantonese", "yue"}, {"chinese_simplified", "zh-Hans"}, {"chinese_traditional", "zh-Hant"}, {"croatian", "hr"},
	{"czech", "cs"}, {"danish", "da"}, {"dari", "prs"}, {"dutch", "nl"},
	{"english", "en"}, {"espranto", "eo"}, {"estonian", "et"}, {"extremaduran", "ext"},
	{"farsi_persian", "fa"}, {"filipino", "fil"}, {"finnish", "fi"}, {"french", "fr"},
	{"french_canada", "fr-CA"}, {"french_france", "fr-FR"}, {"gaelic", "gd"}, {"gaelician", "gl"},
	{"georgian", "ka"}, {"german", "de"}, {"greek", "el"}, {"greenlandic", "kl"},
	{"hebrew", "he"}, {"hindi", "hi"}, {"hungarian", "hu"}, {"icelandic", "is"},
	{"igbo", "ig"}, {"indonesian", "id"}, {"interlingua", "ia"}, {"irish", "ga"},
	{"italian", "it"}, {"japanese", "ja"}, {"kannada", "kn"}, {"kazakh", "kk"},
	{"khmer", "km"}, {"korean", "ko"}, {"kurdish", "ku"}, {"kyrgyz", "ky"},
	{"latvian", "lv"}, {"lithuanian", "lt"}, {"luxembourgish", "lb"}, {"macedonian", "mk"},
	{"malay", "ms"}, {"malayalam", "ml"}, {"manipuri", "mni"}, {"marathi", "mr"},
	{"mongolian", "mn"}, {"montenegrin", "cnr"}, {"navajo", "nv"}, {"nepali", "ne"},
	{"northen_sami", "se"}, {"norwegian", "no"}, {"occitan", "oc"}, {"odia", "or"},
	{"pashto", "ps"}, {"polish", "pl"}, {"portuguese", "pt"}, {"pushto", "ps"},
	{"romanian", "ro"}, {"russian", "ru"}, {"santli", "sat"}, {"serbian", "sr"},
	{"sindhi", "sd"}, {"sinhala", "si"}, {"sinhalese", "si"}, {"slovak", "sk"},
	{"slovenian", "sl"}, {"somali", "so"}, {"sorbian", "wen"}, {"spanish", "es"},
	{"spanish_latin_america", "es-419"}, {"spanish_spain", "es-ES"}, {"swahili", "sw"}, {"swedish", "sv"},
	{"sylheti", "syl"}, {"syriac", "syr"}, {"tagalog", "fil"}, {"tamil", "ta"},
	{"tatar", "tt"}, {"telugu", "te"}, {"tetum", "tet"}, {"thai", "th"},
	{"toki_pona", "tok"}, {"turkish", "tr"}, {"turkmen", "tk"}, {"ukrainian", "uk"},
	{"urdu", "ur"}, {"uzbek", "uz"}, {"vietnamese", "vi"}, {"welsh", "cy"},
}

// sharedTags are reached by several SubSource slugs; searches list every slug
// and merge the results.
var sharedTags = []domain.Language{"fil", "ps", "si"}

// Lookups derived once from languageTable; slugs keep table order.
var (
	slugTags = map[string]domain.Language{}
	tagSlugs = map[domain.Language][]string{}
)

func init() {
	for _, entry := range languageTable {
		slugTags[entry.slug] = entry.tag
		tagSlugs[entry.tag] = append(tagSlugs[entry.tag], entry.slug)
	}
}

// languageSlugs returns the SubSource slugs for a canonical tag, in table order.
func languageSlugs(language domain.Language) []string {
	return tagSlugs[language]
}

func supportsSlug(language domain.Language, slug string) bool {
	tag, ok := slugTags[slug]
	return ok && tag == language
}
