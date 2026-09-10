package opensubtitles

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"subsyncd/internal/domain"
	baseprovider "subsyncd/internal/provider"
)

func TestDownloadQuotaReturnsPersistedFutureReset(t *testing.T) {
	for _, scenario := range []struct {
		name, reset, retry string
		hours              int
	}{
		{"missing", "", "", 6},
		{"malformed", "invalid", "", 6},
		{"expired", "2026-09-04T11:00:00Z", "", 6},
		{"json", "2026-09-04T15:00:00Z", "", 3},
		{"header", "", "7200", 2},
		{"expired_header", "", "Fri, 04 Sep 2026 11:00:00 GMT", 6},
		{"zero_header", "", "0", 6},
		{"header_over_json", "2026-09-04T13:00:00Z", "7200", 2},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/login" {
					io.WriteString(w, `{"token":"token","expires_in":3600}`)
					return
				}
				if scenario.retry != "" {
					w.Header().Set("Retry-After", scenario.retry)
				}
				w.WriteHeader(http.StatusNotAcceptable)
				io.WriteString(w, `{"message":"download limit","reset_time_utc":"`+scenario.reset+`"}`)
			}))
			defer server.Close()
			client := newTestClient(t, server, staticHasher{}, 1024)
			_, err := client.Download(t.Context(), domain.Candidate{DownloadRef: "501"}, io.Discard)
			want := time.Date(2026, 9, 4, 12+scenario.hours, 0, 0, 0, time.UTC)
			var quota *baseprovider.QuotaError
			if !errors.As(err, &quota) || !quota.ResetAt.Equal(want) {
				t.Fatalf("quota = %#v, error %v; want %v", quota, err, want)
			}
			var cooldown *baseprovider.CooldownError
			err = client.CheckDownloadAvailability(t.Context())
			if !errors.As(err, &cooldown) || !cooldown.ResetAt.Equal(want) {
				t.Fatalf("saved cooldown = %#v, error %v; want %v", cooldown, err, want)
			}
		})
	}
}
