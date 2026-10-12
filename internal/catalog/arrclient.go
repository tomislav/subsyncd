package catalog

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"subsyncd/internal/observability"
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

const (
	arrResponseLimit    = 16 << 20
	arrErrorBodyLimit   = 64 << 10
	arrRequestBodyLimit = 1 << 20
)

// arrValidationError reports a 400 validation response. Only the distinct
// property names survive; Arr error messages can echo secrets such as URLs.
type arrValidationError struct {
	Instance   string
	Operation  string
	StatusCode int
	Properties []string
}

func (e *arrValidationError) Error() string {
	properties := make([]string, len(e.Properties))
	for i, property := range e.Properties {
		properties[i] = observability.SafeText(property)
	}
	return fmt.Sprintf(
		"Arr API validation failed: instance=%s operation=%s status=%d properties=%s",
		observability.SafeText(e.Instance),
		observability.SafeText(e.Operation),
		e.StatusCode,
		strings.Join(properties, ","),
	)
}

func (c *arrClient) endpoint(path string, query url.Values) string {
	endpoint := c.baseURL.ResolveReference(&url.URL{Path: strings.TrimRight(c.baseURL.Path, "/") + path})
	endpoint.RawQuery = query.Encode()
	return endpoint.String()
}

func (c *arrClient) getJSON(ctx context.Context, path string, query url.Values, destination any) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.endpoint(path, query), nil)
	if err != nil {
		return safeArrAPIError(c.instance, "detail_request", err)
	}
	return c.do(request, destination, "detail_request", "detail_body", "detail_decode", false)
}

// sendJSON sends a JSON request body and decodes a successful response into
// destination, which may be nil.
func (c *arrClient) sendJSON(ctx context.Context, method, path string, body any, destination any, operation string) error {
	payload, err := json.Marshal(body)
	if err != nil || len(payload) > arrRequestBodyLimit {
		return &arrAPIError{Instance: c.instance, Operation: operation, Kind: "invalid_request"}
	}
	request, err := http.NewRequestWithContext(ctx, method, c.endpoint(path, nil), bytes.NewReader(payload))
	if err != nil {
		return safeArrAPIError(c.instance, operation, err)
	}
	request.Header.Set("Content-Type", "application/json")
	return c.do(request, destination, operation, operation, operation, true)
}

func (c *arrClient) do(request *http.Request, destination any, requestOp, bodyOp, decodeOp string, parseValidation bool) error {
	request.Header.Set("X-Api-Key", c.apiKey)
	request.Header.Set("Accept", "application/json")
	response, err := c.http.Do(request)
	if err != nil {
		return safeArrAPIError(c.instance, requestOp, err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		if parseValidation && response.StatusCode == http.StatusBadRequest {
			if err := c.validationError(response, requestOp); err != nil {
				return err
			}
		} else {
			_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, arrErrorBodyLimit))
		}
		return &arrAPIError{Instance: c.instance, Operation: requestOp, Kind: "status", StatusCode: response.StatusCode, Retryable: response.StatusCode >= 500}
	}
	if destination == nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, arrResponseLimit))
		return nil
	}
	payload, err := io.ReadAll(io.LimitReader(response.Body, arrResponseLimit+1))
	if err != nil {
		return safeArrAPIError(c.instance, bodyOp, err)
	}
	if len(payload) > arrResponseLimit {
		return &arrAPIError{Instance: c.instance, Operation: bodyOp, Kind: "response_too_large"}
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	if err := decoder.Decode(destination); err != nil {
		return &arrAPIError{Instance: c.instance, Operation: decodeOp, Kind: "invalid_json"}
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return &arrAPIError{Instance: c.instance, Operation: decodeOp, Kind: "invalid_json"}
	}
	return nil
}

// validationError returns a validation error when the 400 body is a JSON
// array of objects, and nil otherwise (the caller then returns a status error).
func (c *arrClient) validationError(response *http.Response, operation string) error {
	payload, err := io.ReadAll(io.LimitReader(response.Body, arrErrorBodyLimit+1))
	if err != nil || len(payload) > arrErrorBodyLimit {
		return nil
	}
	var entries []struct {
		PropertyName string `json:"propertyName"`
	}
	if json.Unmarshal(payload, &entries) != nil {
		return nil
	}
	seen := map[string]bool{}
	properties := []string{}
	for _, entry := range entries {
		if entry.PropertyName != "" && !seen[entry.PropertyName] {
			seen[entry.PropertyName] = true
			properties = append(properties, entry.PropertyName)
		}
	}
	sort.Strings(properties)
	return &arrValidationError{Instance: c.instance, Operation: operation, StatusCode: response.StatusCode, Properties: properties}
}
