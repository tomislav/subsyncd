package notifier

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"subsyncd/internal/domain"
)

const defaultTimeout = 15 * time.Second

type PathMapping struct {
	From string
	To   string
}

type SiloConfig struct {
	Enabled      bool
	BaseURL      string
	APIKey       string
	PathMappings []PathMapping
	Client       *http.Client
}

type Silo struct {
	endpoint *url.URL
	apiKey   string
	mappings []PathMapping
	client   *http.Client
}

func NewSilo(config SiloConfig) (Notifier, error) {
	if !config.Enabled {
		return Noop{}, nil
	}
	if strings.TrimSpace(config.APIKey) == "" {
		return nil, fmt.Errorf("Silo API key is required")
	}
	for _, mapping := range config.PathMappings {
		from := filepath.Clean(strings.ReplaceAll(mapping.From, `\`, `/`))
		to := filepath.Clean(strings.ReplaceAll(mapping.To, `\`, `/`))
		if !filepath.IsAbs(from) || !filepath.IsAbs(to) {
			return nil, fmt.Errorf("Silo path mapping endpoints must be absolute")
		}
	}
	base, err := url.Parse(config.BaseURL)
	if err != nil || base.Scheme == "" || base.Host == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" || base.Scheme != "http" && base.Scheme != "https" {
		return nil, fmt.Errorf("Silo base URL must be an absolute HTTP(S) URL without credentials, query, or fragment")
	}
	base.Path = strings.TrimRight(base.Path, "/") + "/api/v1/scan"
	client := http.Client{Timeout: defaultTimeout}
	if config.Client != nil {
		client = *config.Client
		if client.Timeout == 0 {
			client.Timeout = defaultTimeout
		}
	}
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &Silo{endpoint: base, apiKey: config.APIKey, mappings: append([]PathMapping(nil), config.PathMappings...), client: &client}, nil
}

func (s *Silo) SubtitleChanged(ctx context.Context, media domain.Media, subtitlePath string) error {
	target := media.Fingerprint.Path
	if target == "" {
		target = subtitlePath
	}
	if target == "" {
		return &DeliveryError{Reason: "media path is empty"}
	}
	mapped, err := rewritePath(target, s.mappings)
	if err != nil {
		return &DeliveryError{Reason: "path mapping is invalid"}
	}
	payload, err := json.Marshal(struct {
		Path string `json:"path"`
	}{Path: filepath.Dir(mapped)})
	if err != nil {
		return &DeliveryError{Reason: "encode request"}
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, s.endpoint.String(), bytes.NewReader(payload))
	if err != nil {
		return &DeliveryError{Reason: "build request"}
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+s.apiKey)
	response, err := s.client.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return &DeliveryError{Retryable: true, Reason: "transport unavailable"}
	}
	defer response.Body.Close()
	if response.StatusCode >= 200 && response.StatusCode < 300 {
		return nil
	}
	retryable := response.StatusCode == http.StatusRequestTimeout || response.StatusCode == http.StatusTooManyRequests || response.StatusCode >= 500
	return &DeliveryError{StatusCode: response.StatusCode, Retryable: retryable, Reason: strconv.Itoa(response.StatusCode)}
}

func rewritePath(path string, mappings []PathMapping) (string, error) {
	normalized := filepath.ToSlash(filepath.Clean(strings.ReplaceAll(path, `\`, `/`)))
	best := -1
	bestLength := -1
	for index, mapping := range mappings {
		from := strings.TrimSuffix(filepath.ToSlash(filepath.Clean(strings.ReplaceAll(mapping.From, `\`, `/`))), "/")
		to := strings.TrimSuffix(filepath.ToSlash(filepath.Clean(strings.ReplaceAll(mapping.To, `\`, `/`))), "/")
		if from == "." || to == "." || from == "" || to == "" || !strings.HasPrefix(normalized, from) {
			continue
		}
		if len(normalized) != len(from) && normalized[len(from)] != '/' {
			continue
		}
		if len(from) > bestLength {
			best = index
			bestLength = len(from)
		}
	}
	if best < 0 {
		return normalized, nil
	}
	mapping := mappings[best]
	from := strings.TrimSuffix(filepath.ToSlash(filepath.Clean(strings.ReplaceAll(mapping.From, `\`, `/`))), "/")
	to := strings.TrimSuffix(filepath.ToSlash(filepath.Clean(strings.ReplaceAll(mapping.To, `\`, `/`))), "/")
	rewritten := to + strings.TrimPrefix(normalized, from)
	cleaned := filepath.ToSlash(filepath.Clean(rewritten))
	if cleaned != rewritten {
		return "", fmt.Errorf("rewritten path is not clean")
	}
	return cleaned, nil
}
