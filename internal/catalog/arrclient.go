package catalog

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type arrClient struct {
	instance string
	baseURL  *url.URL
	apiKey   string
	http     *http.Client
}

func newArrClient(instance, rawURL, apiKey string, client *http.Client) (*arrClient, error) {
	baseURL, err := url.Parse(rawURL)
	if err != nil || baseURL.Scheme != "http" && baseURL.Scheme != "https" || baseURL.Host == "" || baseURL.User != nil || baseURL.RawQuery != "" || baseURL.Fragment != "" {
		return nil, fmt.Errorf("invalid URL for Arr instance %q", instance)
	}
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	return &arrClient{instance: instance, baseURL: baseURL, apiKey: apiKey, http: client}, nil
}

func (c *arrClient) getJSON(ctx context.Context, path string, query url.Values, destination any) error {
	endpoint := c.baseURL.ResolveReference(&url.URL{Path: strings.TrimRight(c.baseURL.Path, "/") + path})
	endpoint.RawQuery = query.Encode()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return fmt.Errorf("create request for Arr instance %s: %w", c.instance, err)
	}
	request.Header.Set("X-Api-Key", c.apiKey)
	request.Header.Set("Accept", "application/json")
	response, err := c.http.Do(request)
	if err != nil {
		return fmt.Errorf("request Arr instance %s: %w", c.instance, err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("Arr instance %s returned %s", c.instance, response.Status)
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, 16<<20))
	if err := decoder.Decode(destination); err != nil {
		return fmt.Errorf("decode response from Arr instance %s: %w", c.instance, err)
	}
	return nil
}
