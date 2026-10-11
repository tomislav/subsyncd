package opensubtitles

import (
	"context"
	"errors"
	"io"
	"net/http"
	"testing"
	"time"

	"subsyncd/internal/domain"
	baseprovider "subsyncd/internal/provider"
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
			if test.quota && !quota.ResetAt.Equal(reset) {
				t.Fatalf("quota reset = %s, want %s", quota.ResetAt, reset)
			}
		})
	}
}
