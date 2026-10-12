package catalog

import (
	"context"
	"encoding/json"
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

func TestArrClientSendJSONPostsBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v3/notification" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		for key, want := range map[string]string{"X-Api-Key": "secret", "Accept": "application/json", "Content-Type": "application/json"} {
			if got := r.Header.Get(key); got != want {
				t.Errorf("%s = %q", key, got)
			}
		}
		var in map[string]any
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in["name"] != "x" {
			t.Errorf("body = %v err=%v", in, err)
		}
		w.WriteHeader(http.StatusCreated)
		io.WriteString(w, `{"id":42}`)
	}))
	defer server.Close()
	c, _ := newArrClient("main", server.URL, "secret", nil)
	var out struct {
		ID int `json:"id"`
	}
	if err := c.sendJSON(t.Context(), http.MethodPost, "/api/v3/notification", map[string]any{"name": "x"}, &out, "notification_create"); err != nil || out.ID != 42 {
		t.Fatalf("out=%+v err=%v", out, err)
	}
}

func TestArrClientSendJSONValidationErrorKeepsOnlyPropertyNames(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `[{"propertyName":"Url","errorMessage":"x token=SENTINEL y"},{"propertyName":"Url"},{"propertyName":"Name","errorMessage":"SENTINEL"}]`)
	}))
	defer server.Close()
	c, _ := newArrClient("main", server.URL, "secret", nil)
	err := c.sendJSON(t.Context(), http.MethodPut, "/api/v3/notification/1", map[string]any{}, nil, "notification_update")
	var verr *arrValidationError
	if !errors.As(err, &verr) {
		t.Fatalf("err = %v", err)
	}
	if strings.Join(verr.Properties, ",") != "Name,Url" || verr.StatusCode != 400 || verr.Operation != "notification_update" {
		t.Fatalf("validation = %+v", verr)
	}
	if strings.Contains(err.Error(), "SENTINEL") {
		t.Fatalf("leak: %v", err)
	}
}

func TestArrClientSendJSON500DiscardsBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		io.WriteString(w, `{"message":"Connection refused SENTINEL"}`)
	}))
	defer server.Close()
	c, _ := newArrClient("main", server.URL, "secret", nil)
	err := c.sendJSON(t.Context(), http.MethodPost, "/x", map[string]any{}, nil, "op")
	var aerr *arrAPIError
	if !errors.As(err, &aerr) || aerr.StatusCode != 500 || !aerr.Retryable || strings.Contains(err.Error(), "SENTINEL") {
		t.Fatalf("err = %v", err)
	}
}

func TestArrClientSendJSONNonArray400IsStatusError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"message":"SENTINEL"}`)
	}))
	defer server.Close()
	c, _ := newArrClient("main", server.URL, "secret", nil)
	err := c.sendJSON(t.Context(), http.MethodPost, "/x", map[string]any{}, nil, "op")
	var aerr *arrAPIError
	if !errors.As(err, &aerr) || aerr.StatusCode != 400 || strings.Contains(err.Error(), "SENTINEL") {
		t.Fatalf("err = %v", err)
	}
}

func TestArrClientSendJSONRejectsRedirect(t *testing.T) {
	var reached atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/target" {
			reached.Add(1)
			return
		}
		http.Redirect(w, r, "/target", http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	c, _ := newArrClient("main", server.URL, "secret", nil)
	if err := c.sendJSON(t.Context(), http.MethodPost, "/start", map[string]any{}, nil, "op"); err == nil || reached.Load() != 0 {
		t.Fatalf("err=%v reached=%d", err, reached.Load())
	}
}

func TestArrClientSendJSONPreservesCancellation(t *testing.T) {
	c, _ := newArrClient("main", "http://private.invalid", "secret", &http.Client{Transport: arrErrorTransport{}})
	err := c.sendJSON(t.Context(), http.MethodPost, "/private", map[string]any{}, nil, "op")
	if !errors.Is(err, context.DeadlineExceeded) || strings.Contains(err.Error(), "private") {
		t.Fatalf("err = %v", err)
	}
}

type closeCountingTransport struct {
	status         int
	body           string
	opened, closed atomic.Int32
}

func (c *closeCountingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	c.opened.Add(1)
	return &http.Response{StatusCode: c.status, Header: http.Header{}, Request: r, Body: &countedBody{Reader: strings.NewReader(c.body), closed: &c.closed}}, nil
}

type countedBody struct {
	io.Reader
	closed *atomic.Int32
}

func (b *countedBody) Close() error { b.closed.Add(1); return nil }

func TestArrClientSendJSONClosesBodies(t *testing.T) {
	cases := map[string]struct {
		status int
		body   string
	}{
		"success": {200, `{"id":1}`}, "400": {400, `[{"propertyName":"Url"}]`}, "400-bad": {400, `nope`},
		"500": {500, `boom`}, "decode": {200, `{`},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			transport := &closeCountingTransport{status: tc.status, body: tc.body}
			c, _ := newArrClient("main", "http://arr.invalid", "secret", &http.Client{Transport: transport})
			var out struct {
				ID int `json:"id"`
			}
			_ = c.sendJSON(t.Context(), http.MethodPost, "/x", map[string]any{}, &out, "op")
			if transport.opened.Load() != 1 || transport.closed.Load() != 1 {
				t.Fatalf("opened=%d closed=%d", transport.opened.Load(), transport.closed.Load())
			}
		})
	}
}
