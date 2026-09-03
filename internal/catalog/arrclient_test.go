package catalog

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestArrClientAuthenticatesAndCapsErrorBodies(t *testing.T) {
	const apiKey = "do-not-leak-this-key"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-Api-Key"); got != apiKey {
			t.Errorf("X-Api-Key = %q", got)
		}
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, strings.Repeat("x", 10*1024))
	}))
	defer server.Close()

	client, err := newArrClient("sonarr-main", server.URL, apiKey, nil)
	if err != nil {
		t.Fatal(err)
	}
	err = client.getJSON(context.Background(), "/api/v3/system/status", nil, &struct{}{})
	if err == nil {
		t.Fatal("expected non-2xx error")
	}
	message := err.Error()
	if strings.Contains(message, apiKey) {
		t.Fatal("error leaked API key")
	}
	if len(message) > 5*1024 {
		t.Fatalf("error was not capped: %d bytes", len(message))
	}
}
