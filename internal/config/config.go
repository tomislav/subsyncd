package config

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"subsyncd/internal/domain"
)

const (
	defaultListen                          = "0.0.0.0:8097"
	defaultMinimumReleaseScore             = 35
	defaultRequestsPerSecond               = 1
	defaultBurst                           = 1
	defaultMaxConcurrent                   = 1
	defaultSharedOriginMaxConcurrent       = 1
	defaultPackCacheTTL                    = 24 * time.Hour
	defaultPackCacheMaxBytes         int64 = 512 * 1024 * 1024
)

type Config struct {
	DataDir              string
	MediaRoots           []string
	Server               ServerConfig
	Instances            []InstanceConfig
	Providers            map[string]ProviderSpec
	Languages            map[domain.Language]LanguageConfig
	AllowHearingImpaired bool
	MinimumReleaseScore  int
	ProviderHTTP         ProviderHTTPConfig
	PackCache            PackCacheConfig
	Sync                 SyncConfig
	Silo                 SiloConfig
}

type ServerConfig struct {
	Listen string `yaml:"listen"`
}

type PathMapping struct {
	Remote string `yaml:"remote"`
	Local  string `yaml:"local"`
}

type InstanceConfig struct {
	Name         string        `yaml:"name"`
	Type         string        `yaml:"type"`
	URL          string        `yaml:"url"`
	APIKey       string        `yaml:"api_key"`
	WebhookToken string        `yaml:"webhook_token"`
	PathMappings []PathMapping `yaml:"path_mappings"`
}

type ProviderSpec struct {
	Type              string
	RequestsPerSecond float64
	Burst             int
	MaxConcurrent     int
	Settings          yaml.Node
}

type LanguageConfig struct {
	Providers []string `yaml:"providers"`
}

type ProviderHTTPConfig struct {
	SharedOriginMaxConcurrent int `yaml:"shared_origin_max_concurrent"`
}

type PackCacheConfig struct {
	TTL      time.Duration
	MaxBytes int64
}

type SyncConfig struct {
	LapsePath string
	Timeout   time.Duration
}

type SiloConfig struct {
	Enabled      bool              `yaml:"enabled"`
	URL          string            `yaml:"url"`
	APIKey       string            `yaml:"api_key"`
	PathMappings []SiloPathMapping `yaml:"path_mappings"`
}

type SiloPathMapping struct {
	From string `yaml:"from"`
	To   string `yaml:"to"`
}

type rawConfig struct {
	DataDir              string                    `yaml:"data_dir"`
	MediaRoots           []string                  `yaml:"media_roots"`
	Server               ServerConfig              `yaml:"server"`
	Instances            []InstanceConfig          `yaml:"instances"`
	Providers            map[string]yaml.Node      `yaml:"providers"`
	Languages            map[string]LanguageConfig `yaml:"languages"`
	AllowHearingImpaired *bool                     `yaml:"allow_hearing_impaired"`
	MinimumReleaseScore  int                       `yaml:"minimum_release_score"`
	ProviderHTTP         rawProviderHTTPConfig     `yaml:"provider_http"`
	PackCache            rawPackCacheConfig        `yaml:"pack_cache"`
	Sync                 rawSyncConfig             `yaml:"sync"`
	Silo                 SiloConfig                `yaml:"silo"`
}

type rawProviderHTTPConfig struct {
	SharedOriginMaxConcurrent *int `yaml:"shared_origin_max_concurrent"`
}

type rawPackCacheConfig struct {
	TTL      *duration `yaml:"ttl"`
	MaxBytes *int64    `yaml:"max_bytes"`
}

type rawSyncConfig struct {
	LapsePath string    `yaml:"lapse_path"`
	Timeout   *duration `yaml:"timeout"`
}

type providerCommon struct {
	Type              string   `yaml:"type"`
	RequestsPerSecond *float64 `yaml:"requests_per_second"`
	Burst             *int     `yaml:"burst"`
	MaxConcurrent     *int     `yaml:"max_concurrent"`
}

type duration time.Duration

func (d *duration) UnmarshalYAML(node *yaml.Node) error {
	parsed, err := time.ParseDuration(node.Value)
	if err != nil {
		return fmt.Errorf("parse duration %q: %w", node.Value, err)
	}
	*d = duration(parsed)
	return nil
}

func Load(path string, lookupEnv func(string) (string, bool)) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read config: %w", err)
	}

	var document yaml.Node
	if err := yaml.Unmarshal(data, &document); err != nil {
		return Config{}, fmt.Errorf("parse config: %w", err)
	}
	if err := expandEnv(&document, lookupEnv); err != nil {
		return Config{}, err
	}
	expanded, err := yaml.Marshal(&document)
	if err != nil {
		return Config{}, fmt.Errorf("encode expanded config: %w", err)
	}

	var raw rawConfig
	decoder := yaml.NewDecoder(bytes.NewReader(expanded))
	decoder.KnownFields(true)
	if err := decoder.Decode(&raw); err != nil {
		return Config{}, fmt.Errorf("decode config: %w", err)
	}
	if err := ensureSingleDocument(decoder); err != nil {
		return Config{}, err
	}

	cfg, err := normalize(raw)
	if err != nil {
		return Config{}, err
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func ensureSingleDocument(decoder *yaml.Decoder) error {
	var extra any
	err := decoder.Decode(&extra)
	if err == io.EOF {
		return nil
	}
	if err != nil {
		return fmt.Errorf("decode trailing config document: %w", err)
	}
	return fmt.Errorf("config must contain exactly one YAML document")
}

func expandEnv(node *yaml.Node, lookupEnv func(string) (string, bool)) error {
	if node.Kind == yaml.ScalarNode && node.Tag == "!!str" {
		value := node.Value
		if strings.HasPrefix(value, "${") && strings.HasSuffix(value, "}") && strings.Count(value, "${") == 1 {
			name := value[2 : len(value)-1]
			if name == "" {
				return fmt.Errorf("environment variable name is empty")
			}
			replacement, ok := lookupEnv(name)
			if !ok {
				return fmt.Errorf("environment variable %s is not set", name)
			}
			node.Value = replacement
		}
	}
	for _, child := range node.Content {
		if err := expandEnv(child, lookupEnv); err != nil {
			return err
		}
	}
	return nil
}

func normalize(raw rawConfig) (Config, error) {
	allowHearingImpaired := true
	if raw.AllowHearingImpaired != nil {
		allowHearingImpaired = *raw.AllowHearingImpaired
	}
	cfg := Config{
		DataDir:              raw.DataDir,
		MediaRoots:           raw.MediaRoots,
		Server:               raw.Server,
		Instances:            raw.Instances,
		Providers:            make(map[string]ProviderSpec, len(raw.Providers)),
		Languages:            make(map[domain.Language]LanguageConfig, len(raw.Languages)),
		AllowHearingImpaired: allowHearingImpaired,
		MinimumReleaseScore:  raw.MinimumReleaseScore,
		Silo:                 raw.Silo,
	}
	if cfg.Server.Listen == "" {
		cfg.Server.Listen = defaultListen
	}
	if cfg.MinimumReleaseScore == 0 {
		cfg.MinimumReleaseScore = defaultMinimumReleaseScore
	}

	shared := defaultSharedOriginMaxConcurrent
	if raw.ProviderHTTP.SharedOriginMaxConcurrent != nil {
		shared = *raw.ProviderHTTP.SharedOriginMaxConcurrent
	}
	cfg.ProviderHTTP.SharedOriginMaxConcurrent = shared

	cacheTTL := defaultPackCacheTTL
	if raw.PackCache.TTL != nil {
		cacheTTL = time.Duration(*raw.PackCache.TTL)
	}
	cacheBytes := defaultPackCacheMaxBytes
	if raw.PackCache.MaxBytes != nil {
		cacheBytes = *raw.PackCache.MaxBytes
	}
	cfg.PackCache = PackCacheConfig{TTL: cacheTTL, MaxBytes: cacheBytes}

	syncTimeout := 30 * time.Minute
	if raw.Sync.Timeout != nil {
		syncTimeout = time.Duration(*raw.Sync.Timeout)
	}
	cfg.Sync = SyncConfig{LapsePath: raw.Sync.LapsePath, Timeout: syncTimeout}

	for id, settings := range raw.Providers {
		var common providerCommon
		if err := settings.Decode(&common); err != nil {
			return Config{}, fmt.Errorf("decode provider %s: %w", id, err)
		}
		rps := float64(defaultRequestsPerSecond)
		if common.RequestsPerSecond != nil {
			rps = *common.RequestsPerSecond
		}
		burst := defaultBurst
		if common.Burst != nil {
			burst = *common.Burst
		}
		maxConcurrent := defaultMaxConcurrent
		if common.MaxConcurrent != nil {
			maxConcurrent = *common.MaxConcurrent
		}
		cfg.Providers[id] = ProviderSpec{
			Type:              common.Type,
			RequestsPerSecond: rps,
			Burst:             burst,
			MaxConcurrent:     maxConcurrent,
			Settings:          settings,
		}
	}

	for rawTag, languageConfig := range raw.Languages {
		tag, err := domain.ParseLanguage(rawTag)
		if err != nil {
			return Config{}, fmt.Errorf("language %q: %w", rawTag, err)
		}
		if _, exists := cfg.Languages[tag]; exists {
			return Config{}, fmt.Errorf("duplicate canonical language %s", tag)
		}
		cfg.Languages[tag] = languageConfig
	}
	return cfg, nil
}

func (c Config) Validate() error {
	if !filepath.IsAbs(c.DataDir) {
		return fmt.Errorf("data_dir must be absolute")
	}
	if len(c.MediaRoots) == 0 {
		return fmt.Errorf("at least one media root is required")
	}
	_, port, err := net.SplitHostPort(c.Server.Listen)
	if err != nil {
		return fmt.Errorf("server listen address %q is invalid", c.Server.Listen)
	}
	portNumber, err := strconv.Atoi(port)
	if err != nil || portNumber < 0 || portNumber > 65535 {
		return fmt.Errorf("server listen port %q is invalid", port)
	}
	for _, root := range c.MediaRoots {
		if !filepath.IsAbs(root) {
			return fmt.Errorf("media root %q must be absolute", root)
		}
	}
	if c.ProviderHTTP.SharedOriginMaxConcurrent <= 0 {
		return fmt.Errorf("provider_http shared_origin_max_concurrent must be positive")
	}
	if c.PackCache.TTL <= 0 {
		return fmt.Errorf("pack cache ttl must be positive")
	}
	if c.PackCache.MaxBytes <= 0 {
		return fmt.Errorf("pack cache max_bytes must be positive")
	}
	if c.MinimumReleaseScore < 1 || c.MinimumReleaseScore > 100 {
		return fmt.Errorf("minimum_release_score must be between 1 and 100")
	}

	for id, provider := range c.Providers {
		if strings.TrimSpace(id) == "" {
			return fmt.Errorf("provider id is empty")
		}
		if provider.Type == "" {
			return fmt.Errorf("provider %s type is required", id)
		}
		if provider.RequestsPerSecond <= 0 {
			return fmt.Errorf("provider %s requests_per_second must be positive", id)
		}
		if provider.Burst <= 0 {
			return fmt.Errorf("provider %s burst must be positive", id)
		}
		if provider.MaxConcurrent <= 0 {
			return fmt.Errorf("provider %s max_concurrent must be positive", id)
		}
	}

	if len(c.Languages) == 0 {
		return fmt.Errorf("at least one language is required")
	}
	for language, languageConfig := range c.Languages {
		if len(languageConfig.Providers) == 0 {
			return fmt.Errorf("language %s requires at least one provider", language)
		}
		seen := make(map[string]struct{}, len(languageConfig.Providers))
		for _, id := range languageConfig.Providers {
			if _, ok := c.Providers[id]; !ok {
				return fmt.Errorf("language %s references unknown provider %s", language, id)
			}
			if _, duplicate := seen[id]; duplicate {
				return fmt.Errorf("language %s repeats provider %s", language, id)
			}
			seen[id] = struct{}{}
		}
	}

	instanceNames := make(map[string]struct{}, len(c.Instances))
	for _, instance := range c.Instances {
		if _, duplicate := instanceNames[instance.Name]; duplicate {
			return fmt.Errorf("duplicate instance name %s", instance.Name)
		}
		instanceNames[instance.Name] = struct{}{}
		if instance.Name == "" || (instance.Type != "sonarr" && instance.Type != "radarr") {
			return fmt.Errorf("instance %q must have a name and type sonarr or radarr", instance.Name)
		}
		parsedURL, err := url.ParseRequestURI(instance.URL)
		if err != nil || parsedURL.Scheme != "http" && parsedURL.Scheme != "https" || parsedURL.Host == "" || parsedURL.User != nil || parsedURL.RawQuery != "" || parsedURL.Fragment != "" {
			return fmt.Errorf("instance %s url is invalid", instance.Name)
		}
		if instance.APIKey == "" || instance.WebhookToken == "" {
			return fmt.Errorf("instance %s credentials are required", instance.Name)
		}
		for _, mapping := range instance.PathMappings {
			if !filepath.IsAbs(mapping.Local) || !withinAnyRoot(mapping.Local, c.MediaRoots) {
				return fmt.Errorf("instance %s path mapping %q must be inside a media root", instance.Name, mapping.Local)
			}
			if strings.TrimSpace(mapping.Remote) == "" {
				return fmt.Errorf("instance %s path mapping remote path is empty", instance.Name)
			}
		}
	}

	if c.Sync.LapsePath == "" || !filepath.IsAbs(c.Sync.LapsePath) {
		return fmt.Errorf("sync lapse_path must be absolute")
	}
	if c.Sync.Timeout <= 0 {
		return fmt.Errorf("sync timeout must be positive")
	}
	if c.Silo.Enabled {
		parsedURL, err := url.ParseRequestURI(c.Silo.URL)
		if err != nil || parsedURL.Scheme == "" || parsedURL.Host == "" || parsedURL.User != nil || parsedURL.RawQuery != "" || parsedURL.Fragment != "" || c.Silo.APIKey == "" {
			return fmt.Errorf("enabled silo requires a valid url and api_key")
		}
		for _, mapping := range c.Silo.PathMappings {
			if !filepath.IsAbs(mapping.From) || !filepath.IsAbs(mapping.To) {
				return fmt.Errorf("Silo path mapping from and to must be absolute")
			}
		}
	}
	return nil
}

func withinAnyRoot(path string, roots []string) bool {
	cleanPath := filepath.Clean(path)
	for _, root := range roots {
		relative, err := filepath.Rel(filepath.Clean(root), cleanPath)
		if err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return true
		}
	}
	return false
}
