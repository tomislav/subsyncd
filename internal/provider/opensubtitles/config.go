package opensubtitles

import (
	"bytes"
	"fmt"
	"net/url"

	"gopkg.in/yaml.v3"
)

const (
	defaultBaseURL          = "https://api.opensubtitles.com/api/v1"
	defaultMaxDownloadBytes = 32 << 20
)

type Config struct {
	Type              string  `yaml:"type"`
	APIKey            string  `yaml:"api_key"`
	Username          string  `yaml:"username"`
	Password          string  `yaml:"password"`
	UserAgent         string  `yaml:"user_agent"`
	BaseURL           string  `yaml:"base_url"`
	MaxDownloadBytes  int64   `yaml:"max_download_bytes"`
	RequestsPerSecond float64 `yaml:"requests_per_second"`
	Burst             int     `yaml:"burst"`
	MaxConcurrent     int     `yaml:"max_concurrent"`
}

func DecodeConfig(payload []byte) (Config, error) {
	var config Config
	decoder := yaml.NewDecoder(bytes.NewReader(payload))
	decoder.KnownFields(true)
	if err := decoder.Decode(&config); err != nil {
		return Config{}, fmt.Errorf("decode OpenSubtitles config: %w", err)
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
	if c.APIKey == "" || c.Username == "" || c.Password == "" || c.UserAgent == "" {
		return fmt.Errorf("OpenSubtitles api_key, username, password, and user_agent are required")
	}
	parsed, err := url.Parse(c.BaseURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return fmt.Errorf("OpenSubtitles base_url is invalid")
	}
	if c.MaxDownloadBytes <= 0 || c.RequestsPerSecond <= 0 || c.Burst <= 0 || c.MaxConcurrent <= 0 {
		return fmt.Errorf("OpenSubtitles download and rate limits must be positive")
	}
	return nil
}
