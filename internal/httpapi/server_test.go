package httpapi

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"subsyncd/internal/catalog"
)

type webhookFunc func(context.Context, []byte) error

func (f webhookFunc) Handle(ctx context.Context, body []byte) error { return f(ctx, body) }

func TestWebhookAuthenticationAndDispatch(t *testing.T) {
	var calls int
	server := Server{Instances: map[string]Instance{
		"sonarr-main": {Token: "secret", Handler: webhookFunc(func(_ context.Context, body []byte) error {
			calls++
			if !strings.Contains(string(body), `"eventType":"Download"`) {
				t.Fatalf("unexpected body %q", body)
			}
			return nil
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
		"main": {Token: "secret", Handler: webhookFunc(func(context.Context, []byte) error { t.Fatal("handler called"); return nil })},
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
		"main": {Token: "secret", Handler: webhookFunc(func(context.Context, []byte) error { t.Fatal("handler called"); return nil })},
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
			server := Server{Instances: map[string]Instance{"main": {Token: "secret", Handler: webhookFunc(func(context.Context, []byte) error { return test.err })}}}
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
	server := Server{Instances: map[string]Instance{"main": {Token: "secret", Handler: webhookFunc(func(context.Context, []byte) error { return nil })}}}
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
	server := Server{Logger: slog.New(slog.NewJSONHandler(&logs, nil)), Instances: map[string]Instance{
		"main": {Token: "correct-secret", Handler: webhookFunc(func(context.Context, []byte) error { return nil })},
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
