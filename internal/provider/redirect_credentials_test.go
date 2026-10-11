package provider

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"subsyncd/internal/store"
	"subsyncd/internal/testutil"
)

func TestCrossHostRedirectDropsCredentialHeaders(t *testing.T) {
	seen := make(chan http.Header, 2)
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Header.Clone()
		io.WriteString(w, "ok")
	}))
	defer other.Close()
	var origin *httptest.Server
	origin = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/same" {
			http.Redirect(w, r, origin.URL+"/landing", http.StatusFound)
			return
		}
		if r.URL.Path == "/landing" {
			seen <- r.Header.Clone()
			io.WriteString(w, "ok")
			return
		}
		// Same hostname, another port: a different origin.
		http.Redirect(w, r, other.URL+"/file", http.StatusFound)
	}))
	defer origin.Close()
	clock := testutil.NewClock(time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC))
	gate := NewGate(&memoryStateStore{states: map[string]store.ProviderState{}}, clock, 4)
	gate.Configure("p", 1000, 10, 4)
	transport := Client{HTTP: &http.Client{}, Gate: gate, Clock: clock, ProviderID: "p", ProviderType: "subdl"}
	for path, wantKept := range map[string]bool{"/elsewhere": false, "/same": true} {
		request, _ := http.NewRequest(http.MethodGet, origin.URL+path, nil)
		request.Header.Set("X-Api-Key", "secret")
		request.Header.Set("Api-Key", "secret")
		request.Header.Set("Authorization", "Bearer secret")
		request.Header.Set("User-Agent", "subsyncd")
		response, err := transport.Do(context.Background(), OperationDownload, request)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, response.Body)
		response.Body.Close()
		headers := <-seen
		for _, name := range []string{"X-Api-Key", "Api-Key", "Authorization"} {
			if kept := headers.Get(name) != ""; kept != wantKept {
				t.Errorf("%s: %s kept = %t, want %t", path, name, kept, wantKept)
			}
		}
		if headers.Get("User-Agent") != "subsyncd" {
			t.Errorf("%s: ordinary header dropped", path)
		}
	}
}
