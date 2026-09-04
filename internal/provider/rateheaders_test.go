package provider

import (
	"net/http"
	"strconv"
	"testing"
	"time"
)

func TestParseRateLimitUsesMostRestrictiveFutureWindow(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	headers := http.Header{
		"Ratelimit":        {`"short";r=0;t=30, "long";r=0;t=90`},
		"Ratelimit-Policy": {`"short";q=10;w=60, "long";q=100;w=120`},
	}
	window, ok := ParseRateLimit(now, headers)
	if !ok || window.Limit != 100 || window.Remaining != 0 || !window.ResetAt.Equal(now.Add(90*time.Second)) {
		t.Fatalf("window = %#v, %v", window, ok)
	}
}

func TestParseRateLimitSupportsXHeadersAndRetryAfterForms(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name    string
		headers http.Header
		reset   time.Time
	}{
		{"x headers", http.Header{"X-Ratelimit-Limit": {"50"}, "X-Ratelimit-Remaining": {"0"}, "X-Ratelimit-Reset": {strconv.FormatInt(now.Add(2*time.Minute).Unix(), 10)}}, now.Add(2 * time.Minute)},
		{"retry seconds", http.Header{"Retry-After": {"45"}}, now.Add(45 * time.Second)},
		{"retry date", http.Header{"Retry-After": {now.Add(3 * time.Minute).Format(http.TimeFormat)}}, now.Add(3 * time.Minute)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			window, ok := ParseRateLimit(now, test.headers)
			if !ok || !window.ResetAt.Equal(test.reset) {
				t.Fatalf("window = %#v, %v; want reset %s", window, ok, test.reset)
			}
		})
	}
}

func TestParseRateLimitCombinesJSONResetAndIgnoresInvalidPastWindows(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	headers := http.Header{"X-Ratelimit-Remaining": {"0"}, "X-Ratelimit-Reset": {strconv.FormatInt(now.Add(-time.Minute).Unix(), 10)}}
	jsonReset := now.Add(time.Hour)
	window, ok := ParseRateLimit(now, headers, jsonReset)
	if !ok || window.Source != "json" || !window.ResetAt.Equal(jsonReset) {
		t.Fatalf("window = %#v, %v", window, ok)
	}
	if _, ok := ParseRateLimit(now, http.Header{"X-Ratelimit-Remaining": {"bad"}, "X-Ratelimit-Reset": {"missing"}}); ok {
		t.Fatal("invalid partial headers should not produce a window")
	}
}
