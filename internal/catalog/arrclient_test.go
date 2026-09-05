package catalog

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestArrClientAuthenticatesAndOmitsErrorBodies(t *testing.T) {
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
	if strings.Contains(message, "xxxx") {
		t.Fatal("error surfaced untrusted response body")
	}
}

func TestArrClientRejectsRedirectsWithoutChangingInjectedPolicy(t *testing.T) {
	for _, cross := range []bool{false, true} {
		t.Run(fmt.Sprint(cross), func(t *testing.T) {
			var reached atomic.Int32
			target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached.Add(1); io.WriteString(w, `{}`) }))
			defer target.Close()
			source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/target" {
					reached.Add(1)
					io.WriteString(w, `{}`)
					return
				}
				dest := "/target"
				if cross {
					dest = target.URL
				}
				http.Redirect(w, r, dest, http.StatusFound)
			}))
			defer source.Close()
			original := &http.Client{Timeout: time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return nil }}
			client, err := newArrClient("main", source.URL, "secret", original)
			if err != nil {
				t.Fatal(err)
			}
			err = client.getJSON(t.Context(), "/start", nil, &struct{}{})
			if err == nil || reached.Load() != 0 {
				t.Fatalf("redirect error=%v target requests=%d", err, reached.Load())
			}
			if client.http == original || original.CheckRedirect(nil, nil) != nil || client.http.Timeout != time.Second {
				t.Fatal("injected policy mutated or discarded")
			}
		})
	}
}

func TestArrClientStrictPrivateErrors(t *testing.T) {
	for _, body := range []string{`{"value":"PRIVATE"}`, `{} {}`, `{} garbage`, strings.Repeat(" ", 16<<20) + `{}`} {
		t.Run(fmt.Sprint(len(body)), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, body) }))
			defer server.Close()
			c, _ := newArrClient("main", server.URL, "secret", nil)
			var dst struct {
				Value int `json:"value"`
			}
			err := c.getJSON(t.Context(), "/private", nil, &dst)
			if err == nil {
				t.Fatal("invalid response accepted")
			}
			for _, s := range []string{"PRIVATE", server.URL, "/private", "cannot unmarshal"} {
				if strings.Contains(err.Error(), s) {
					t.Fatalf("unsafe error: %v", err)
				}
			}
		})
	}
}

func TestArrClientPreservesCancellationSafely(t *testing.T) {
	c, _ := newArrClient("main", "http://private.invalid", "secret", &http.Client{Transport: arrErrorTransport{}})
	err := c.getJSON(t.Context(), "/private", nil, &struct{}{})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("deadline lost: %v", err)
	}
	var timeout net.Error
	if !errors.As(err, &timeout) || !timeout.Timeout() {
		t.Fatal("timeout semantics lost")
	}
	if strings.Contains(err.Error(), "private") {
		t.Fatalf("unsafe error %v", err)
	}
}

type arrErrorTransport struct{}

func (arrErrorTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, fmt.Errorf("private upstream: %w", context.DeadlineExceeded)
}
