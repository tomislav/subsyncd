package provider

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"subsyncd/internal/store"
	"subsyncd/internal/testutil"
)

func TestLateAuthResponsesPreservePermanentDisable(t *testing.T) {
	for _, late := range []string{"success", "transient failure"} {
		t.Run(late, func(t *testing.T) {
			now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
			states := &memoryStateStore{states: map[string]store.ProviderState{}}
			gate := NewGate(states, testutil.NewClock(now), 1)
			if err := gate.Persist(t.Context(), Throttle{ProviderID: "p", Scope: OperationAuth, Disabled: true, Reason: "credentials rejected"}); err != nil {
				t.Fatal(err)
			}
			if late == "success" {
				if err := gate.Persist(t.Context(), Throttle{ProviderID: "p", Scope: OperationAuth, Remaining: 5, ResetAt: now.Add(time.Minute)}); err != nil {
					t.Fatal(err)
				}
			} else {
				if _, err := gate.RecordTransientFailure(t.Context(), "p", OperationAuth, "network_error", time.Time{}); err != nil {
					t.Fatal(err)
				}
				if err := gate.ResetTransientFailures(t.Context(), "p", OperationAuth); err != nil {
					t.Fatal(err)
				}
			}
			state, err := states.GetProviderState(t.Context(), "p", string(OperationAuth))
			if err != nil || !state.Disabled || state.Reason != "credentials rejected" {
				t.Fatalf("permanent disable lost: state=%+v error=%v", state, err)
			}
			_, err = gate.Acquire(t.Context(), "p", "https://example.test", OperationSearch)
			if !errors.As(err, new(*DisabledError)) {
				t.Fatalf("request no longer disabled: %v", err)
			}
		})
	}
}

func TestHTTP429PositiveRemainingStillBlocksNextRequest(t *testing.T) {
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	clock := testutil.NewClock(now)
	gate := NewGate(&memoryStateStore{states: map[string]store.ProviderState{}}, clock, 1)
	gate.Configure("p", 1000, 2, 1)
	requests := 0
	client := Client{ProviderID: "p", ProviderType: "subdl", Gate: gate, Clock: clock, HTTP: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		requests++
		return &http.Response{StatusCode: http.StatusTooManyRequests, Header: http.Header{"Ratelimit": {`"minute";r=5;t=60`}}, Body: io.NopCloser(strings.NewReader("{}"))}, nil
	})}}
	for i := 0; i < 2; i++ {
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://example.test/search", nil)
		response, err := client.Do(t.Context(), OperationSearch, req)
		if response != nil {
			response.Body.Close()
		}
		var cooldown *CooldownError
		if !errors.As(err, &cooldown) || !cooldown.ResetAt.Equal(now.Add(time.Minute)) {
			t.Fatalf("attempt %d cooldown=%v", i, err)
		}
	}
	if requests != 1 {
		t.Fatalf("requests=%d; second request must be suppressed", requests)
	}
}

func TestHTTP429ExpiredResetUsesFutureFallback(t *testing.T) {
	for _, retryAfter := range []string{"0", "Thu, 10 Sep 2026 11:00:00 GMT"} {
		t.Run(retryAfter, func(t *testing.T) {
			now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
			clock := testutil.NewClock(now)
			gate := NewGate(&memoryStateStore{states: map[string]store.ProviderState{}}, clock, 1)
			gate.Configure("p", 1000, 2, 1)
			requests := 0
			client := Client{ProviderID: "p", ProviderType: "titlovi", Gate: gate, Clock: clock, HTTP: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				requests++
				return &http.Response{StatusCode: 429, Header: http.Header{"Retry-After": {retryAfter}}, Body: io.NopCloser(strings.NewReader("{}"))}, nil
			})}}
			for range 2 {
				req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://example.test", nil)
				response, err := client.Do(t.Context(), OperationSearch, req)
				if response != nil {
					response.Body.Close()
				}
				var cooldown *CooldownError
				if !errors.As(err, &cooldown) || !cooldown.ResetAt.Equal(now.Add(5*time.Minute)) {
					t.Fatalf("cooldown=%v", err)
				}
			}
			if requests != 1 {
				t.Fatalf("requests=%d", requests)
			}
		})
	}
}
