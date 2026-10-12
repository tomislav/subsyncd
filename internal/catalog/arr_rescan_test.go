package catalog

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"subsyncd/internal/domain"
)

func TestArrRescanClientSendsRescanCommandPerInstanceType(t *testing.T) {
	for _, test := range []struct {
		instanceType string
		kind         domain.MediaKind
		want         map[string]any
	}{
		{instanceType: "sonarr", kind: domain.MediaEpisode, want: map[string]any{"name": "RescanSeries", "seriesId": float64(42)}},
		{instanceType: "radarr", kind: domain.MediaMovie, want: map[string]any{"name": "RescanMovie", "movieId": float64(42)}},
	} {
		t.Run(test.instanceType, func(t *testing.T) {
			var got map[string]any
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != "/arr/api/v3/command" {
					t.Errorf("request = %s %s", r.Method, r.URL.Path)
				}
				if r.Header.Get("X-Api-Key") != "secret" || r.Header.Get("Content-Type") != "application/json" {
					t.Errorf("headers = %v", r.Header)
				}
				if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
					t.Error(err)
				}
				w.WriteHeader(http.StatusCreated)
				io.WriteString(w, `{"id":7,"name":"RescanSeries","status":"queued"}`)
			}))
			defer server.Close()
			client, err := NewArrRescanClient("main", test.instanceType, server.URL+"/arr/", "secret", nil)
			if err != nil {
				t.Fatal(err)
			}
			if client.Kind() != test.kind {
				t.Fatalf("kind = %q", client.Kind())
			}
			if err := client.Rescan(t.Context(), 42); err != nil {
				t.Fatalf("Rescan() error = %v", err)
			}
			if len(got) != len(test.want) {
				t.Fatalf("body = %v, want %v", got, test.want)
			}
			for key, value := range test.want {
				if got[key] != value {
					t.Fatalf("body = %v, want %v", got, test.want)
				}
			}
		})
	}
}

func TestArrRescanClientClassifiesFailuresWithoutLeaks(t *testing.T) {
	for _, test := range []struct {
		status        int
		body          string
		wantRetryable bool
	}{
		{status: http.StatusNotFound, body: `{"message":"Series SENTINEL not found"}`},
		{status: http.StatusBadRequest, body: `[{"propertyName":"SeriesId","errorMessage":"SENTINEL"}]`},
		{status: http.StatusUnauthorized, body: `SENTINEL`},
		{status: http.StatusBadGateway, body: `SENTINEL`, wantRetryable: true},
	} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(test.status)
			io.WriteString(w, test.body)
		}))
		client, err := NewArrRescanClient("main", "sonarr", server.URL, "do-not-leak", nil)
		if err != nil {
			t.Fatal(err)
		}
		err = client.Rescan(t.Context(), 9)
		server.Close()
		status, retryable := ArrRequestFailure(err)
		if err == nil || status != test.status || retryable != test.wantRetryable {
			t.Fatalf("status %d: err=%v classified=%d,%v", test.status, err, status, retryable)
		}
		for _, leaked := range []string{"SENTINEL", "do-not-leak", server.URL} {
			if strings.Contains(err.Error(), leaked) {
				t.Fatalf("error leaked %q: %v", leaked, err)
			}
		}
	}
}

func TestArrRescanClientUnreachableIsRetryable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := server.URL
	server.Close()
	client, err := NewArrRescanClient("main", "radarr", url, "secret", &http.Client{Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	err = client.Rescan(context.Background(), 3)
	if status, retryable := ArrRequestFailure(err); err == nil || status != 0 || !retryable {
		t.Fatalf("err=%v classified=%d,%v", err, status, retryable)
	}
	if strings.Contains(err.Error(), url) {
		t.Fatalf("error leaked URL: %v", err)
	}
}

func TestArrRescanClientRejectsUnknownIDAndType(t *testing.T) {
	if _, err := NewArrRescanClient("main", "lidarr", "http://arr.invalid", "secret", nil); err == nil {
		t.Fatal("unsupported type accepted")
	}
	client, err := NewArrRescanClient("main", "sonarr", "http://arr.invalid", "secret", nil)
	if err != nil {
		t.Fatal(err)
	}
	err = client.Rescan(t.Context(), 0)
	if status, retryable := ArrRequestFailure(err); err == nil || status != 0 || retryable {
		t.Fatalf("err=%v classified=%d,%v", err, status, retryable)
	}
}
