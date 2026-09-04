package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"subsyncd/internal/catalog"
	"subsyncd/internal/observability"
)

type webhookFunc func(context.Context, []byte) (catalog.WebhookResult, error)

func (f webhookFunc) Handle(ctx context.Context, body []byte) (catalog.WebhookResult, error) {
	return f(ctx, body)
}

func TestWebhookAuthenticationAndDispatch(t *testing.T) {
	var calls int
	server := Server{Instances: map[string]Instance{
		"sonarr-main": {Token: "secret", Handler: webhookFunc(func(_ context.Context, body []byte) (catalog.WebhookResult, error) {
			calls++
			if !strings.Contains(string(body), `"eventType":"Download"`) {
				t.Fatalf("unexpected body %q", body)
			}
			return catalog.WebhookResult{EventCount: 1, AppliedCount: 1}, nil
		})},
	}}

	request := httptest.NewRequest(http.MethodPost, "/webhooks/sonarr-main?token=secret", strings.NewReader(`{"eventType":"Download"}`))
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)

	if response.Code != http.StatusNoContent || calls != 1 {
		t.Fatalf("status/calls = %d/%d, want 204/1", response.Code, calls)
	}
	if response.Header().Get("X-Request-ID") == "" {
		t.Fatal("response has no request ID")
	}
}

func TestWebhookRejectsUnknownInstanceAndInvalidToken(t *testing.T) {
	server := Server{Instances: map[string]Instance{
		"main": {Token: "secret", Handler: webhookFunc(func(context.Context, []byte) (catalog.WebhookResult, error) {
			t.Fatal("handler called")
			return catalog.WebhookResult{}, nil
		})},
	}}
	for _, test := range []struct {
		name string
		url  string
		want int
	}{
		{"unknown instance", "/webhooks/missing?token=secret", http.StatusNotFound},
		{"invalid token", "/webhooks/main?token=wrong", http.StatusUnauthorized},
		{"missing token", "/webhooks/main", http.StatusUnauthorized},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := httptest.NewRecorder()
			server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodPost, test.url, strings.NewReader(`{"eventType":"Download"}`)))
			if response.Code != test.want {
				t.Fatalf("status = %d, want %d", response.Code, test.want)
			}
		})
	}
}

func TestWebhookBodyAndJSONValidation(t *testing.T) {
	server := Server{MaxBodyBytes: 32, Instances: map[string]Instance{
		"main": {Token: "secret", Handler: webhookFunc(func(context.Context, []byte) (catalog.WebhookResult, error) {
			t.Fatal("handler called")
			return catalog.WebhookResult{}, nil
		})},
	}}
	for _, test := range []struct {
		name string
		body string
		want int
	}{
		{"oversized", `{"eventType":"Download","padding":"too large"}`, http.StatusRequestEntityTooLarge},
		{"malformed", `{"eventType":`, http.StatusBadRequest},
		{"trailing value", `{} {}`, http.StatusBadRequest},
		{"non object", `[]`, http.StatusBadRequest},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := httptest.NewRecorder()
			server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/webhooks/main?token=secret", strings.NewReader(test.body)))
			if response.Code != test.want {
				t.Fatalf("status = %d, want %d; body=%q", response.Code, test.want, response.Body.String())
			}
		})
	}
}

func TestWebhookStatusMapping(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
		want int
	}{
		{"test event", catalog.ErrIgnoredEvent, http.StatusNoContent},
		{"invalid event", ErrInvalidWebhook, http.StatusBadRequest},
		{"dependency failure", errors.New("arr unavailable"), http.StatusServiceUnavailable},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := Server{Instances: map[string]Instance{"main": {Token: "secret", Handler: webhookFunc(func(context.Context, []byte) (catalog.WebhookResult, error) {
				return catalog.WebhookResult{}, test.err
			})}}}
			response := httptest.NewRecorder()
			server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/webhooks/main?token=secret", strings.NewReader(`{"eventType":"Download"}`)))
			if response.Code != test.want {
				t.Fatalf("status = %d, want %d", response.Code, test.want)
			}
		})
	}
}

func TestHealthAndReadiness(t *testing.T) {
	readyErr := errors.New("sqlite unavailable")
	server := Server{Ready: func(context.Context) error { return readyErr }}

	health := httptest.NewRecorder()
	server.Handler().ServeHTTP(health, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if health.Code != http.StatusOK || strings.TrimSpace(health.Body.String()) != "ok" {
		t.Fatalf("health response = %d %q", health.Code, health.Body.String())
	}

	notReady := httptest.NewRecorder()
	server.Handler().ServeHTTP(notReady, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if notReady.Code != http.StatusServiceUnavailable || strings.Contains(notReady.Body.String(), readyErr.Error()) {
		t.Fatalf("readiness response = %d %q", notReady.Code, notReady.Body.String())
	}

	server.Ready = func(context.Context) error { return nil }
	ready := httptest.NewRecorder()
	server.Handler().ServeHTTP(ready, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if ready.Code != http.StatusOK {
		t.Fatalf("ready status = %d, want 200", ready.Code)
	}
}

func TestHTTPMethodsAreRestricted(t *testing.T) {
	server := Server{Instances: map[string]Instance{"main": {Token: "secret", Handler: webhookFunc(func(context.Context, []byte) (catalog.WebhookResult, error) {
		return catalog.WebhookResult{}, nil
	})}}}
	for _, path := range []string{"/healthz", "/readyz", "/webhooks/main?token=secret"} {
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodPut, path, nil))
		if response.Code != http.StatusMethodNotAllowed {
			t.Fatalf("PUT %s status = %d, want 405", path, response.Code)
		}
	}
}

func TestWebhookLogsNeverContainQueryToken(t *testing.T) {
	var logs bytes.Buffer
	events, err := observability.New(&logs, observability.Options{Level: "info", Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	server := Server{Events: events, Instances: map[string]Instance{
		"main": {Token: "correct-secret", Handler: webhookFunc(func(context.Context, []byte) (catalog.WebhookResult, error) {
			return catalog.WebhookResult{}, nil
		})},
	}}
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/webhooks/main?token=leaked-secret", strings.NewReader(`{}`)))
	if strings.Contains(logs.String(), "leaked-secret") || strings.Contains(logs.String(), "token=") {
		t.Fatalf("query token leaked in logs: %s", logs.String())
	}
	if !strings.Contains(logs.String(), `"path":"/webhooks/main"`) {
		t.Fatalf("redacted path missing from log: %s", logs.String())
	}
}

func TestSuccessfulProbesAreSilentAndReadinessLogsTransitionsOnly(t *testing.T) {
	var logs bytes.Buffer
	events, err := observability.New(&logs, observability.Options{Level: "info", Version: "test", Redact: func(error) string { return "redacted" }})
	if err != nil {
		t.Fatal(err)
	}
	var readyErr error
	server := Server{Events: events, Ready: func(context.Context) error { return readyErr }}
	handler := server.Handler()
	request := func(path string) int {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		return response.Code
	}
	if request("/healthz") != http.StatusOK || request("/readyz") != http.StatusOK || logs.Len() != 0 {
		t.Fatalf("healthy probes produced logs: %s", logs.String())
	}
	readyErr = errors.New("sqlite unavailable")
	if request("/readyz") != http.StatusServiceUnavailable || request("/readyz") != http.StatusServiceUnavailable {
		t.Fatal("unhealthy readiness status mismatch")
	}
	readyErr = nil
	if request("/readyz") != http.StatusOK || request("/readyz") != http.StatusOK {
		t.Fatal("recovered readiness status mismatch")
	}
	records := decodeHTTPLogs(t, logs.String())
	if len(records) != 2 || records[0]["event"] != "readiness.unhealthy" || records[1]["event"] != "readiness.recovered" {
		t.Fatalf("readiness events = %#v", records)
	}
}

func TestSuccessfulWebhookLogsCorrelatedLifecycleAndCounts(t *testing.T) {
	var logs bytes.Buffer
	events, err := observability.New(&logs, observability.Options{Level: "info", Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	server := Server{Events: events, Instances: map[string]Instance{
		"main": {Token: "secret", Handler: webhookFunc(func(context.Context, []byte) (catalog.WebhookResult, error) {
			return catalog.WebhookResult{EventCount: 2, AppliedCount: 1}, nil
		})},
	}}
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/webhooks/main?token=secret", strings.NewReader(`{"eventType":"Download"}`))
	request.Header.Set("X-Request-ID", "request-123")
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("status = %d", response.Code)
	}
	records := decodeHTTPLogs(t, logs.String())
	if len(records) != 2 || records[0]["event"] != "webhook.accepted" || records[1]["event"] != "webhook.applied" {
		t.Fatalf("webhook events = %#v", records)
	}
	for _, record := range records {
		if record["request_id"] != "request-123" || record["path"] != "/webhooks/main" || record["instance"] != "main" {
			t.Fatalf("correlation fields = %#v", record)
		}
	}
	if records[1]["event_count"] != float64(2) || records[1]["applied_count"] != float64(1) || records[1]["status"] != float64(http.StatusNoContent) {
		t.Fatalf("applied fields = %#v", records[1])
	}
}

func decodeHTTPLogs(t *testing.T, output string) []map[string]any {
	t.Helper()
	trimmed := strings.TrimSpace(output)
	if trimmed == "" {
		return nil
	}
	lines := strings.Split(trimmed, "\n")
	records := make([]map[string]any, 0, len(lines))
	for _, line := range lines {
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("decode log %q: %v", line, err)
		}
		records = append(records, record)
	}
	return records
}
