package opensubtitles

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"subsyncd/internal/domain"
	"subsyncd/internal/observability"
	baseprovider "subsyncd/internal/provider"
	"subsyncd/internal/store"
	"subsyncd/internal/testutil"
)

func TestLastDownloadOfTheQuotaPausesFurtherDownloads(t *testing.T) {
	c := reviewClient(t, func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/api/v1/login":
			return reviewResponse(r, 200, `{"token":"token"}`), nil
		case "/api/v1/download":
			return reviewResponse(r, 200, `{"link":"https://dl.opensubtitles.example/file","file_name":"sub.srt","remaining":0,"reset_time_utc":"2026-09-04T18:00:00Z"}`), nil
		default:
			return reviewResponse(r, 200, "subtitle"), nil
		}
	})
	if _, err := c.Download(context.Background(), domain.Candidate{DownloadRef: "1"}, io.Discard); err != nil {
		t.Fatalf("last download of the quota failed: %v", err)
	}
	err := c.CheckDownloadAvailability(context.Background())
	var cooldown *baseprovider.CooldownError
	var quota *baseprovider.QuotaError
	if err == nil || (!errors.As(err, &cooldown) && !errors.As(err, &quota)) {
		t.Fatalf("availability after remaining 0 = %v, want the download quota paused", err)
	}
	if errors.As(err, &cooldown) && !cooldown.ResetAt.Equal(time.Date(2026, 9, 4, 18, 0, 0, 0, time.UTC)) {
		t.Fatalf("pause until %s, want the reported reset", cooldown.ResetAt)
	}
}

func TestDownloadsLeftKeepDownloadsAvailable(t *testing.T) {
	c := reviewClient(t, func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/api/v1/login":
			return reviewResponse(r, 200, `{"token":"token"}`), nil
		case "/api/v1/download":
			return reviewResponse(r, 200, `{"link":"https://dl.opensubtitles.example/file","file_name":"sub.srt","remaining":3}`), nil
		default:
			return reviewResponse(r, 200, "subtitle"), nil
		}
	})
	if _, err := c.Download(context.Background(), domain.Candidate{DownloadRef: "1"}, io.Discard); err != nil {
		t.Fatal(err)
	}
	if err := c.CheckDownloadAvailability(context.Background()); err != nil {
		t.Fatalf("availability = %v", err)
	}
}

func TestQuotaRefusalsBeyond406(t *testing.T) {
	reset := time.Date(2026, 9, 4, 18, 0, 0, 0, time.UTC)
	for name, test := range map[string]struct {
		status int
		body   string
		quota  bool
	}{
		"403 quota message":    {http.StatusForbidden, `{"message":"You have downloaded your allowed 20 subtitles","reset_time_utc":"2026-09-04T18:00:00Z"}`, true},
		"429 quota message":    {http.StatusTooManyRequests, `{"message":"Download quota exceeded","reset_time_utc":"2026-09-04T18:00:00Z"}`, true},
		"403 without quota":    {http.StatusForbidden, `{"message":"forbidden"}`, false},
		"403 not allowed":      {http.StatusForbidden, `{"message":"Downloading is not allowed for this API key"}`, false},
		"403 bare allowed":     {http.StatusForbidden, `{"message":"not allowed"}`, false},
		"403 limit reached":    {http.StatusForbidden, `{"message":"Download limit reached"}`, true},
		"403 quota renewed":    {http.StatusForbidden, `{"message":"You have downloaded your allowed 20 subtitles for 24h. Your quota will be renewed in 13 hours"}`, true},
		"429 plain rate limit": {http.StatusTooManyRequests, `{"message":"Throttle limit reached"}`, false},
	} {
		t.Run(name, func(t *testing.T) {
			c := reviewClient(t, func(r *http.Request) (*http.Response, error) {
				if r.URL.Path == "/api/v1/login" {
					return reviewResponse(r, 200, `{"token":"token"}`), nil
				}
				return reviewResponse(r, test.status, test.body), nil
			})
			_, err := c.Download(context.Background(), domain.Candidate{DownloadRef: "1"}, io.Discard)
			var quota *baseprovider.QuotaError
			if errors.As(err, &quota) != test.quota {
				t.Fatalf("error = %T %v, quota %t", err, err, test.quota)
			}
			if test.quota && strings.Contains(test.body, "reset_time_utc") && !quota.ResetAt.Equal(reset) {
				t.Fatalf("quota reset = %s, want %s", quota.ResetAt, reset)
			}
		})
	}
}

// failingStateStore saves nothing, like a database that went read-only.
type failingStateStore struct{ testStateStore }

func (s *failingStateStore) PutProviderState(context.Context, store.ProviderState) error {
	return errors.New("attempt to write a readonly database")
}

func TestLastDownloadKeepsItsLinkWhenTheCooldownCannotBeSaved(t *testing.T) {
	clock := testutil.NewClock(time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC))
	var logs bytes.Buffer
	events, err := observability.New(&logs, observability.Options{Level: "info", Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	gate := baseprovider.NewGate(&failingStateStore{testStateStore{states: map[string]store.ProviderState{}}}, clock, 1, events)
	gate.Configure("review", 1000, 20, 1)
	transport := func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/api/v1/login":
			return reviewResponse(r, 200, `{"token":"token"}`), nil
		case "/api/v1/download":
			return reviewResponse(r, 200, `{"link":"https://dl.opensubtitles.example/file","file_name":"sub.srt","remaining":0,"reset_time_utc":"2026-09-04T18:00:00Z"}`), nil
		default:
			return reviewResponse(r, 200, "subtitle"), nil
		}
	}
	c, err := New(Config{APIKey: "app-key", Username: "u", Password: "p", UserAgent: "review", BaseURL: "https://api.opensubtitles.com/api/v1"}, baseprovider.Client{HTTP: &http.Client{Transport: reviewRoundTripper(transport)}, Gate: gate, Clock: clock, ProviderID: "review", ProviderType: "opensubtitles"}, staticHasher{}, nil, clock)
	if err != nil {
		t.Fatal(err)
	}
	var body bytes.Buffer
	if _, err := c.Download(context.Background(), domain.Candidate{DownloadRef: "1"}, &body); err != nil {
		t.Fatalf("download that used up the quota = %v, want its link still used", err)
	}
	if body.String() != "subtitle" {
		t.Fatalf("downloaded %q", body.String())
	}
	if !strings.Contains(logs.String(), `"event":"provider.cooldown_save_failed"`) {
		t.Fatalf("unsaved cooldown was not logged: %s", logs.String())
	}
}
