package catalog

import (
	"bytes"
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
	policy := *client
	policy.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &arrClient{instance: instance, baseURL: baseURL, apiKey: apiKey, http: &policy}, nil
}

func (c *arrClient) getJSON(ctx context.Context, path string, query url.Values, destination any) error {
	endpoint := c.baseURL.ResolveReference(&url.URL{Path: strings.TrimRight(c.baseURL.Path, "/") + path})
	endpoint.RawQuery = query.Encode()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return safeArrAPIError(c.instance, "detail_request", err)
	}
	request.Header.Set("X-Api-Key", c.apiKey)
	request.Header.Set("Accept", "application/json")
	response, err := c.http.Do(request)
	if err != nil {
		return safeArrAPIError(c.instance, "detail_request", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return &arrAPIError{Instance: c.instance, Operation: "detail_request", Kind: "status", StatusCode: response.StatusCode, Retryable: response.StatusCode >= 500}
	}
	const limit = 16 << 20
	payload, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil {
		return safeArrAPIError(c.instance, "detail_body", err)
	}
	if len(payload) > limit {
		return &arrAPIError{Instance: c.instance, Operation: "detail_body", Kind: "response_too_large"}
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	if err := decoder.Decode(destination); err != nil {
		return &arrAPIError{Instance: c.instance, Operation: "detail_decode", Kind: "invalid_json"}
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return &arrAPIError{Instance: c.instance, Operation: "detail_decode", Kind: "invalid_json"}
	}
	return nil
}
