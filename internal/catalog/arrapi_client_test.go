package catalog

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cplieger/arrapi/v2"

	"subsyncd/internal/observability"
)

func TestArrapiConstructionIsOfflineAndAcceptsBasePath(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		requests++
	}))
	defer server.Close()

	if _, err := newSonarrEntityClient("sonarr-main", server.URL+"/sonarr", "secret", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := newRadarrEntityClient("radarr-main", server.URL+"/radarr", "secret", nil); err != nil {
		t.Fatal(err)
	}
	if requests != 0 {
		t.Fatalf("constructor requests = %d, want 0", requests)
	}
}

func TestSafeArrAPIErrorAndRetryHandlerOmitUpstreamDetails(t *testing.T) {
	const sensitive = "secret /media/private marker"
	err := &arrapi.StatusError{
		Code: 502,
		Path: "/api/v3/history/since",
		Body: sensitive,
	}

	got := safeArrAPIError("radarr-main", "history", err).Error()
	for _, want := range []string{"radarr-main", "history", "502", "retryable=true"} {
		if !strings.Contains(got, want) {
			t.Errorf("safe error %q does not contain %q", got, want)
		}
	}
	for _, forbidden := range []string{"secret", "/media/private", "/api/v3/history/since"} {
		if strings.Contains(got, forbidden) {
			t.Errorf("safe error %q contains sensitive value %q", got, forbidden)
		}
	}

	var output bytes.Buffer
	events, emitterErr := observability.New(&output, observability.Options{Level: "debug", Version: "test"})
	if emitterErr != nil {
		t.Fatal(emitterErr)
	}
	logger := slog.New(arrRetryHandler{Events: events, Instance: "radarr-main"}).
		With("url", "https://radarr.example/api/v3/history/since?apikey=secret").
		WithGroup("upstream")
	logger.Debug(sensitive, "error", sensitive)
	logger.Warn(sensitive, "error", sensitive)

	lines := bytes.Split(bytes.TrimSpace(output.Bytes()), []byte("\n"))
	if len(lines) != 2 {
		t.Fatalf("retry log records = %d, want 2: %s", len(lines), output.String())
	}
	for index, wantEvent := range []string{"arr.request_retry", "arr.request_retries_exhausted"} {
		var logged map[string]any
		if err := json.Unmarshal(lines[index], &logged); err != nil {
			t.Fatalf("decode retry log: %v: %s", err, output.String())
		}
		if got := logged["event"]; got != wantEvent {
			t.Fatalf("event = %#v, want %s", got, wantEvent)
		}
		if got := logged["instance"]; got != "radarr-main" {
			t.Fatalf("instance = %#v, want radarr-main", got)
		}
		for _, key := range []string{"error", "url", "upstream"} {
			if _, exists := logged[key]; exists {
				t.Errorf("retry log retained library attribute/group %q: %#v", key, logged)
			}
		}
	}
	for _, forbidden := range []string{"secret", "/media/private", "radarr.example", "apikey"} {
		if strings.Contains(output.String(), forbidden) {
			t.Errorf("retry log contains sensitive value %q: %s", forbidden, output.String())
		}
	}
}
