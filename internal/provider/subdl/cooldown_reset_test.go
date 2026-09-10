package subdl

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"subsyncd/internal/domain"
	baseprovider "subsyncd/internal/provider"
)

func TestJSONThrottlePreservesResponseReset(t *testing.T) {
	for _, operation := range []string{"search", "download"} {
		for _, code := range []string{"rate_limit", "service_busy", "daily_limit"} {
			t.Run(operation+"/"+code, func(t *testing.T) {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Retry-After", "7200")
					w.WriteHeader(http.StatusTooManyRequests)
					io.WriteString(w, `{"status":false,"error":"`+code+`"}`)
				}))
				defer server.Close()
				client, states := newTestClientWithState(t, server, 1<<20)
				var err error
				if operation == "search" {
					_, err = client.Search(t.Context(), baseprovider.SearchQuery{Media: episodeMedia(), Language: "en", Mode: baseprovider.SearchBroad})
				} else {
					_, err = client.Download(t.Context(), domain.Candidate{DownloadRef: "/subtitle/example.zip"}, io.Discard)
				}
				want := time.Date(2026, 9, 4, 14, 0, 0, 0, time.UTC)
				var cooldown *baseprovider.CooldownError
				var quota *baseprovider.QuotaError
				var got time.Time
				if errors.As(err, &cooldown) {
					got = cooldown.ResetAt
				} else if errors.As(err, &quota) {
					got = quota.ResetAt
				}
				if !got.Equal(want) {
					t.Fatalf("returned reset = %v, want %v (error %v)", got, want, err)
				}
				state, err := states.GetProviderState(context.Background(), "subdl-main", operation)
				if err != nil || !state.ResetAt.Equal(want) {
					t.Fatalf("saved reset = %v, error %v", state.ResetAt, err)
				}
			})
		}
	}
}

func TestJSONThrottlePreservesTransportStateFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("unexpected HTTP request") }))
	defer server.Close()
	client, states := newTestClientWithState(t, server, 1024)
	cause := errors.New("state write failed")
	failure := &baseprovider.AvailabilityError{Err: cause}
	err := client.decodeLimit(t.Context(), baseprovider.OperationSearch, strings.NewReader(`{"error":"daily_limit"}`), http.Header{}, failure)
	if !errors.Is(err, cause) {
		t.Fatalf("state failure replaced by %v", err)
	}
	if _, err := states.GetProviderState(t.Context(), "subdl-main", "search"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("unexpected replacement state: %v", err)
	}
}

func TestJSONThrottleExpiredHeaderUsesFutureFallback(t *testing.T) {
	for _, retry := range []string{"0", "Fri, 04 Sep 2026 11:00:00 GMT"} {
		t.Run(retry, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Retry-After", retry)
				w.WriteHeader(http.StatusTooManyRequests)
				io.WriteString(w, `{"error":"rate_limit"}`)
			}))
			defer server.Close()
			client := newTestClient(t, server, 1024)
			_, err := client.Search(t.Context(), baseprovider.SearchQuery{Media: episodeMedia(), Language: "en", Mode: baseprovider.SearchBroad})
			want := time.Date(2026, 9, 4, 12, 15, 0, 0, time.UTC)
			var cooldown *baseprovider.CooldownError
			if !errors.As(err, &cooldown) || !cooldown.ResetAt.Equal(want) {
				t.Fatalf("reset = %#v; want %v", cooldown, want)
			}
		})
	}
}

func TestJSONThrottlePreservesPositiveRemainingHeaderReset(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("RateLimit", `"default";r=5;t=7200`)
		w.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(w, `{"error":"rate_limit"}`)
	}))
	defer server.Close()
	client, states := newTestClientWithState(t, server, 1024)
	_, err := client.Search(t.Context(), baseprovider.SearchQuery{Media: episodeMedia(), Language: "en", Mode: baseprovider.SearchBroad})
	want := time.Date(2026, 9, 4, 14, 0, 0, 0, time.UTC)
	var cooldown *baseprovider.CooldownError
	if !errors.As(err, &cooldown) || !cooldown.ResetAt.Equal(want) {
		t.Fatalf("cooldown = %#v, error %v; want %v", cooldown, err, want)
	}
	state, err := states.GetProviderState(t.Context(), "subdl-main", "search")
	if err != nil || !state.ResetAt.Equal(want) || state.Remaining != 0 {
		t.Fatalf("saved throttle = %+v, error %v", state, err)
	}
}
