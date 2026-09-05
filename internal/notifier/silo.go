package notifier

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	pathpkg "path"
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
	mappings := make([]PathMapping, 0, len(config.PathMappings))
	for _, mapping := range config.PathMappings {
		from, err := normalizeMappingPath(mapping.From)
		if err != nil {
			return nil, fmt.Errorf("Silo path mapping from endpoint: %w", err)
		}
		to, err := normalizeMappingPath(mapping.To)
		if err != nil {
			return nil, fmt.Errorf("Silo path mapping to endpoint: %w", err)
		}
		mappings = append(mappings, PathMapping{From: from, To: to})
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
	return &Silo{endpoint: base, apiKey: config.APIKey, mappings: mappings, client: &client}, nil
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
	}{Path: pathpkg.Dir(mapped)})
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

func rewritePath(value string, mappings []PathMapping) (string, error) {
	normalized, err := normalizeMappingPath(value)
	if err != nil {
		return "", err
	}
	best := -1
	bestLength := -1
	for index, mapping := range mappings {
		from, err := normalizeMappingPath(mapping.From)
		if err != nil {
			return "", err
		}
		if _, err := normalizeMappingPath(mapping.To); err != nil {
			return "", err
		}
		if !pathHasPrefix(normalized, from) {
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
	from, err := normalizeMappingPath(mapping.From)
	if err != nil {
		return "", err
	}
	to, err := normalizeMappingPath(mapping.To)
	if err != nil {
		return "", err
	}
	suffix := strings.TrimPrefix(normalized, from)
	rewritten := to
	if suffix != "" {
		rewritten = pathpkg.Join(to, strings.TrimPrefix(suffix, "/"))
	}
	if !strings.HasPrefix(rewritten, "/") {
		return "", fmt.Errorf("rewritten path is not absolute")
	}
	return rewritten, nil
}

func normalizeMappingPath(value string) (string, error) {
	raw := strings.ReplaceAll(value, `\`, "/")
	for _, component := range strings.Split(raw, "/") {
		if component == ".." {
			return "", fmt.Errorf("mapping path contains traversal")
		}
	}
	normalized := pathpkg.Clean(raw)
	if normalized == "." || !strings.HasPrefix(normalized, "/") {
		return "", fmt.Errorf("mapping endpoint must be absolute")
	}
	return normalized, nil
}

func pathHasPrefix(path, prefix string) bool {
	if prefix == "/" {
		return strings.HasPrefix(path, "/")
	}
	return path == prefix || strings.HasPrefix(path, prefix+"/")
}
