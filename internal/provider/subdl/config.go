package subdl

import (
	"bytes"
	"fmt"
	"net/url"
	"strings"

	"gopkg.in/yaml.v3"

	"subsyncd/internal/domain"
)

const (
	defaultBaseURL          = "https://api.subdl.com/api/v1/subtitles"
	defaultDownloadBaseURL  = "https://dl.subdl.com"
	defaultMaxDownloadBytes = 64 << 20
)

type Config struct {
	Type                  string   `yaml:"type"`
	APIKey                string   `yaml:"api_key"`
	BaseURL               string   `yaml:"base_url"`
	DownloadBaseURL       string   `yaml:"download_base_url"`
	DownloadHosts         []string `yaml:"download_hosts"`
	MaxDownloadBytes      int64    `yaml:"max_download_bytes"`
	RequestsPerSecond     float64  `yaml:"requests_per_second"`
	Burst                 int      `yaml:"burst"`
	MaxConcurrent         int      `yaml:"max_concurrent"`
	AllowInsecureForTests bool     `yaml:"-"`
}

func DecodeConfig(payload []byte) (Config, error) {
	var config Config
	decoder := yaml.NewDecoder(bytes.NewReader(payload))
	decoder.KnownFields(true)
	if err := decoder.Decode(&config); err != nil {
		return Config{}, fmt.Errorf("decode SubDL config: %w", err)
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
	if c.DownloadBaseURL == "" {
		c.DownloadBaseURL = defaultDownloadBaseURL
	}
	if len(c.DownloadHosts) == 0 {
		c.DownloadHosts = []string{"dl.subdl.com"}
	}
	if c.MaxDownloadBytes == 0 {
		c.MaxDownloadBytes = defaultMaxDownloadBytes
	}
	if c.RequestsPerSecond == 0 {
		c.RequestsPerSecond = 1
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
		return fmt.Errorf("SubDL api_key is required")
	}
	for _, raw := range []string{c.BaseURL, c.DownloadBaseURL} {
		parsed, err := url.Parse(raw)
		if err != nil || parsed.Scheme == "" || parsed.Host == "" {
			return fmt.Errorf("SubDL base URL is invalid")
		}
		if parsed.Scheme != "https" && !c.AllowInsecureForTests {
			return fmt.Errorf("SubDL base URLs must use HTTPS")
		}
	}
	if len(c.DownloadHosts) == 0 || c.MaxDownloadBytes <= 0 || c.RequestsPerSecond <= 0 || c.Burst <= 0 || c.MaxConcurrent <= 0 {
		return fmt.Errorf("SubDL hosts and request limits must be positive")
	}
	return nil
}

var subDLCodes = map[domain.Language]string{
	"ar": "AR", "da": "DA", "nl": "NL", "en": "EN", "fa": "FA", "fi": "FI", "fr": "FR", "id": "ID",
	"it": "IT", "no": "NO", "ro": "RO", "es": "ES", "sv": "SV", "vi": "VI", "sq": "SQ", "az": "AZ",
	"be": "BE", "bn": "BN", "bs": "BS", "bg": "BG", "my": "MY", "ca": "CA", "zh": "ZH", "hr": "HR",
	"cs": "CS", "eo": "EO", "et": "ET", "ka": "KA", "de": "DE", "el": "EL", "kl": "KL", "he": "HE",
	"hi": "HI", "hu": "HU", "is": "IS", "ja": "JA", "ko": "KO", "ku": "KU", "lv": "LV", "lt": "LT",
	"mk": "MK", "ms": "MS", "ml": "ML", "pl": "PL", "pt": "PT", "ru": "RU", "sr": "SR", "si": "SI",
	"sk": "SK", "sl": "SL", "tl": "TL", "ta": "TA", "te": "TE", "th": "TH", "tr": "TR", "uk": "UK", "ur": "UR",
	"pt-BR": "BR_PT", "zh-Hant": "ZH_BG",
}

func subDLLanguage(language domain.Language) (string, bool) {
	value, ok := subDLCodes[language]
	return value, ok
}

func fromSubDLLanguage(raw string) (domain.Language, error) {
	value := strings.ToUpper(strings.TrimSpace(raw))
	for language, code := range subDLCodes {
		if code == value {
			return language, nil
		}
	}
	return "", fmt.Errorf("unsupported SubDL language %q", raw)
}
