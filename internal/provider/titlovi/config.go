package titlovi

import (
	"bytes"
	"fmt"
	"net/url"
	"strings"

	"gopkg.in/yaml.v3"

	"subsyncd/internal/domain"
)

const (
	defaultAPIBaseURL       = "https://kodi.titlovi.com/api/subtitles"
	defaultDownloadBaseURL  = "https://titlovi.com"
	defaultMaxDownloadBytes = 64 << 20
)

type Config struct {
	Type                  string   `yaml:"type"`
	Username              string   `yaml:"username"`
	Password              string   `yaml:"password"`
	APIBaseURL            string   `yaml:"api_base_url"`
	DownloadBaseURL       string   `yaml:"download_base_url"`
	DownloadHosts         []string `yaml:"download_hosts"`
	MaxDownloadBytes      int64    `yaml:"max_download_bytes"`
	MaxPages              int      `yaml:"max_pages"`
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
		return Config{}, fmt.Errorf("decode Titlovi config: %w", err)
	}
	config.applyDefaults()
	if err := config.Validate(); err != nil {
		return Config{}, err
	}
	return config, nil
}

func (c *Config) applyDefaults() {
	if c.APIBaseURL == "" {
		c.APIBaseURL = defaultAPIBaseURL
	}
	if c.DownloadBaseURL == "" {
		c.DownloadBaseURL = defaultDownloadBaseURL
	}
	if len(c.DownloadHosts) == 0 {
		c.DownloadHosts = []string{"titlovi.com", "www.titlovi.com", "kodi.titlovi.com"}
	}
	if c.MaxDownloadBytes == 0 {
		c.MaxDownloadBytes = defaultMaxDownloadBytes
	}
	if c.MaxPages == 0 {
		c.MaxPages = 5
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
	if c.Username == "" || c.Password == "" {
		return fmt.Errorf("Titlovi username and password are required")
	}
	for _, raw := range []string{c.APIBaseURL, c.DownloadBaseURL} {
		parsed, err := url.Parse(raw)
		if err != nil || parsed.Scheme == "" || parsed.Host == "" {
			return fmt.Errorf("Titlovi base URL is invalid")
		}
		if parsed.Scheme != "https" && !c.AllowInsecureForTests {
			return fmt.Errorf("Titlovi base URLs must use HTTPS")
		}
	}
	if len(c.DownloadHosts) == 0 || c.MaxDownloadBytes <= 0 || c.MaxPages <= 0 || c.RequestsPerSecond <= 0 || c.Burst <= 0 || c.MaxConcurrent <= 0 {
		return fmt.Errorf("Titlovi hosts and request limits must be positive")
	}
	return nil
}

var titloviCodes = map[domain.Language]string{
	"bs":      "Bosanski",
	"en":      "English",
	"hr":      "Hrvatski",
	"mk":      "Makedonski",
	"sr":      "Srpski",
	"sr-Cyrl": "Cirilica",
	"sl":      "Slovenski",
}

func titloviLanguage(language domain.Language) (string, bool) {
	value, ok := titloviCodes[language]
	return value, ok
}

func fromTitloviLanguage(raw string) (domain.Language, error) {
	aliases := map[string]domain.Language{
		"bosanski": "bs", "english": "en", "hrvatski": "hr", "croatian": "hr", "hr": "hr", "hrv": "hr",
		"makedonski": "mk", "srpski": "sr", "cirilica": "sr-Cyrl", "slovenski": "sl",
	}
	if language, ok := aliases[strings.ToLower(strings.TrimSpace(raw))]; ok {
		return language, nil
	}
	return "", fmt.Errorf("unsupported Titlovi language %q", raw)
}
