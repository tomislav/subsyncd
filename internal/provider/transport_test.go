package provider

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"subsyncd/internal/store"
	"subsyncd/internal/testutil"
)

type memoryStateStore struct {
	mu     sync.Mutex
	states map[string]store.ProviderState
}

func (s *memoryStateStore) GetProviderState(_ context.Context, providerID, scope string) (store.ProviderState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, ok := s.states[providerID+"/"+scope]
	if !ok {
		return store.ProviderState{}, sql.ErrNoRows
	}
	return state, nil
}
func (s *memoryStateStore) PutProviderState(_ context.Context, state store.ProviderState) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.states[state.ProviderID+"/"+state.Scope] = state
	return nil
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

func TestTransportPersistsRetryAfterAndNeverSleepsThroughCooldown(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	clock := testutil.NewClock(now)
	states := &memoryStateStore{states: map[string]store.ProviderState{}}
	gate := NewGate(states, clock, 1)
	gate.Configure("titlovi-main", 1000, 1, 1)
	calls := 0
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: http.StatusTooManyRequests, Header: http.Header{"Retry-After": {"120"}}, Body: io.NopCloser(strings.NewReader("limited"))}, nil
	})}
	transport := Client{HTTP: client, Gate: gate, Clock: clock, ProviderID: "titlovi-main", ProviderType: "titlovi"}
	request, _ := http.NewRequest(http.MethodGet, "https://api.example/search", nil)
	started := time.Now()
	response, err := transport.Do(context.Background(), OperationSearch, request)
	if response != nil {
		response.Body.Close()
	}
	var cooldown *CooldownError
	if !errors.As(err, &cooldown) || !cooldown.ResetAt.Equal(now.Add(2*time.Minute)) {
		t.Fatalf("error = %#v", err)
	}
	if time.Since(started) > 100*time.Millisecond {
		t.Fatal("transport slept through remote cooldown")
	}
	request, _ = http.NewRequest(http.MethodGet, "https://api.example/search", nil)
	_, err = transport.Do(context.Background(), OperationSearch, request)
	if !errors.As(err, &cooldown) || calls != 1 {
		t.Fatalf("second call error/calls = %v/%d", err, calls)
	}
}

func TestTransportIgnoresRetryAfterOnSuccessfulResponse(t *testing.T) {
	now := time.Date(2026, 9, 5, 9, 34, 57, 0, time.UTC)
	clock := testutil.NewClock(now)
	states := &memoryStateStore{states: map[string]store.ProviderState{}}
	gate := NewGate(states, clock, 1)
	gate.Configure("opensubtitles-main", 1000, 1, 1)
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Retry-After": {"1"}}, Body: io.NopCloser(strings.NewReader("ok"))}, nil
	})}
	transport := Client{HTTP: client, Gate: gate, Clock: clock, ProviderID: "opensubtitles-main", ProviderType: "opensubtitles"}
	request, _ := http.NewRequest(http.MethodPost, "https://api.example/login", nil)

	response, err := transport.Do(context.Background(), OperationAuth, request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if state, err := states.GetProviderState(context.Background(), "opensubtitles-main", string(OperationAuth)); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("successful response persisted cooldown = %#v, %v", state, err)
	}
}

func TestTransportRetainsStandardRateLimitOnSuccessfulResponseWithRetryAfter(t *testing.T) {
	now := time.Date(2026, 9, 5, 9, 34, 57, 0, time.UTC)
	clock := testutil.NewClock(now)
	states := &memoryStateStore{states: map[string]store.ProviderState{}}
	gate := NewGate(states, clock, 1)
	gate.Configure("opensubtitles-main", 1000, 1, 1)
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		headers := make(http.Header)
		headers.Set("Retry-After", "1")
		headers.Set("X-RateLimit-Limit", "100")
		headers.Set("X-RateLimit-Remaining", "0")
		headers.Set("X-RateLimit-Reset", fmt.Sprint(now.Add(10*time.Minute).Unix()))
		return &http.Response{StatusCode: http.StatusOK, Header: headers, Body: io.NopCloser(strings.NewReader("ok"))}, nil
	})}
	transport := Client{HTTP: client, Gate: gate, Clock: clock, ProviderID: "opensubtitles-main", ProviderType: "opensubtitles"}
	request, _ := http.NewRequest(http.MethodPost, "https://api.example/login", nil)

	response, err := transport.Do(context.Background(), OperationAuth, request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	state, err := states.GetProviderState(context.Background(), "opensubtitles-main", string(OperationAuth))
	if err != nil {
		t.Fatal(err)
	}
	if state.Reason != "x-ratelimit" || state.Remaining != 0 || !state.ResetAt.Equal(now.Add(10*time.Minute)) {
		t.Fatalf("standard rate-limit state = %#v", state)
	}
}

func TestFallbackResetMatchesProviderPolicy(t *testing.T) {
	now := time.Date(2026, 9, 4, 23, 30, 0, 0, time.UTC)
	tests := []struct {
		provider string
		kind     CooldownKind
		want     time.Time
	}{
		{"titlovi", CooldownRateLimit, now.Add(5 * time.Minute)},
		{"opensubtitles", CooldownRateLimit, now.Add(time.Minute)},
		{"opensubtitles", CooldownDownloadQuota, now.Add(6 * time.Hour)},
		{"subdl", CooldownRateLimit, now.Add(15 * time.Minute)},
		{"subdl", CooldownDownloadQuota, time.Date(2026, 9, 5, 0, 15, 0, 0, time.UTC)},
		{"subdl", CooldownServiceBusy, now.Add(time.Hour)},
	}
	for _, test := range tests {
		if got := FallbackReset(now, test.provider, test.kind); !got.Equal(test.want) {
			t.Errorf("FallbackReset(%s, %s) = %s, want %s", test.provider, test.kind, got, test.want)
		}
	}
}

func TestTransportErrorDoesNotExposeRequestURLOrSignedQuery(t *testing.T) {
	clock := testutil.NewClock(time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC))
	states := &memoryStateStore{states: map[string]store.ProviderState{}}
	gate := NewGate(states, clock, 1)
	gate.Configure("provider", 1000, 1, 1)
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("request to https://signed.example/file?token=secret failed")
	})}
	transport := Client{HTTP: client, Gate: gate, Clock: clock, ProviderID: "provider", ProviderType: "subdl"}
	request, _ := http.NewRequest(http.MethodGet, "https://signed.example/file?token=secret", nil)
	_, err := transport.Do(context.Background(), OperationDownload, request)
	if err == nil || strings.Contains(err.Error(), "signed.example") || strings.Contains(err.Error(), "secret") {
		t.Fatalf("unsafe transport error = %v", err)
	}
}

func TestTransportPersistsEscalatingTransientCircuitAndResetsAfterSuccess(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	clock := testutil.NewClock(now)
	states := &memoryStateStore{states: map[string]store.ProviderState{}}
	failing := true
	calls := 0
	httpClient := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls++
		if failing {
			return nil, errors.New("dial failed")
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("ok"))}, nil
	})}

	newTransport := func() Client {
		gate := NewGate(states, clock, 1)
		gate.Configure("subdl-main", 1000, 1, 1)
		return Client{HTTP: httpClient, Gate: gate, Clock: clock, ProviderID: "subdl-main", ProviderType: "subdl"}
	}
	do := func(transport Client) (*http.Response, error) {
		request, _ := http.NewRequest(http.MethodGet, "https://api.example/search", nil)
		return transport.Do(context.Background(), OperationSearch, request)
	}

	transport := newTransport()
	_, err := do(transport)
	var cooldown *CooldownError
	if !errors.As(err, &cooldown) || !cooldown.ResetAt.Equal(now.Add(time.Minute)) {
		t.Fatalf("first failure = %#v", err)
	}
	state, err := states.GetProviderState(context.Background(), "subdl-main", string(OperationSearch))
	if err != nil || state.FailureAttempt != 1 {
		t.Fatalf("first persisted state = %#v, %v", state, err)
	}

	// A new gate simulates a process restart: the open circuit must still avoid HTTP.
	transport = newTransport()
	_, err = do(transport)
	if !errors.As(err, &cooldown) || calls != 1 {
		t.Fatalf("restart call error/calls = %v/%d", err, calls)
	}

	clock.Advance(time.Minute + time.Second)
	_, err = do(transport)
	if !errors.As(err, &cooldown) || !cooldown.ResetAt.Equal(clock.Now().Add(5*time.Minute)) {
		t.Fatalf("second failure = %#v", err)
	}
	state, _ = states.GetProviderState(context.Background(), "subdl-main", string(OperationSearch))
	if state.FailureAttempt != 2 {
		t.Fatalf("second failure attempt = %d", state.FailureAttempt)
	}

	clock.Advance(5*time.Minute + time.Second)
	failing = false
	response, err := do(transport)
	if err != nil {
		t.Fatalf("recovery request: %v", err)
	}
	response.Body.Close()
	state, _ = states.GetProviderState(context.Background(), "subdl-main", string(OperationSearch))
	if state.FailureAttempt != 0 || state.Reason != "" || !state.ResetAt.IsZero() {
		t.Fatalf("recovered state = %#v", state)
	}

	failing = true
	_, err = do(transport)
	if !errors.As(err, &cooldown) || !cooldown.ResetAt.Equal(clock.Now().Add(time.Minute)) {
		t.Fatalf("failure after recovery = %#v", err)
	}
}

func TestTransportUsesRetryAfterForServerFailure(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	clock := testutil.NewClock(now)
	states := &memoryStateStore{states: map[string]store.ProviderState{}}
	gate := NewGate(states, clock, 1)
	gate.Configure("subdl-main", 1000, 1, 1)
	calls := 0
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: http.StatusServiceUnavailable, Header: http.Header{"Retry-After": {"120"}}, Body: io.NopCloser(strings.NewReader("busy"))}, nil
	})}
	transport := Client{HTTP: client, Gate: gate, Clock: clock, ProviderID: "subdl-main", ProviderType: "subdl"}
	request, _ := http.NewRequest(http.MethodGet, "https://api.example/search", nil)
	response, err := transport.Do(context.Background(), OperationSearch, request)
	if response != nil {
		response.Body.Close()
	}
	var cooldown *CooldownError
	if !errors.As(err, &cooldown) || !cooldown.ResetAt.Equal(now.Add(2*time.Minute)) {
		t.Fatalf("server failure = %#v", err)
	}
	request, _ = http.NewRequest(http.MethodGet, "https://api.example/search", nil)
	_, err = transport.Do(context.Background(), OperationSearch, request)
	if !errors.As(err, &cooldown) || calls != 1 {
		t.Fatalf("blocked call error/calls = %v/%d", err, calls)
	}
}

func TestTransportRecoveryPreservesNonTransientProviderState(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	clock := testutil.NewClock(now)
	wantReset := now.Add(-time.Minute)
	states := &memoryStateStore{states: map[string]store.ProviderState{
		"subdl-main/download": {ProviderID: "subdl-main", Scope: "download", Reason: "download_quota", Remaining: 0, ResetAt: wantReset, FailureAttempt: 2},
	}}
	gate := NewGate(states, clock, 1)
	gate.Configure("subdl-main", 1000, 1, 1)
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("ok"))}, nil
	})}
	transport := Client{HTTP: client, Gate: gate, Clock: clock, ProviderID: "subdl-main", ProviderType: "subdl"}
	request, _ := http.NewRequest(http.MethodGet, "https://api.example/download", nil)
	response, err := transport.Do(context.Background(), OperationDownload, request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	state, err := states.GetProviderState(context.Background(), "subdl-main", string(OperationDownload))
	if err != nil || state.FailureAttempt != 0 || state.Reason != "download_quota" || !state.ResetAt.Equal(wantReset) {
		t.Fatalf("preserved state = %#v, %v", state, err)
	}
}

func TestTransientCircuitEscalatesAndIsOperationScoped(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	clock := testutil.NewClock(now)
	states := &memoryStateStore{states: map[string]store.ProviderState{}}
	gate := NewGate(states, clock, 1)
	gate.Configure("provider", 1000, 1, 1)

	for attempt, delay := range []time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute, time.Hour, time.Hour} {
		state, err := gate.RecordTransientFailure(context.Background(), "provider", OperationSearch, "network_error", time.Time{})
		if err != nil {
			t.Fatal(err)
		}
		if state.FailureAttempt != attempt+1 || !state.ResetAt.Equal(clock.Now().Add(delay)) {
			t.Fatalf("attempt %d state = %#v", attempt+1, state)
		}
		clock.Advance(delay + time.Second)
	}

	// A search outage must not block an independent download operation.
	release, err := gate.Acquire(context.Background(), "provider", "https://api.example", OperationDownload)
	if err != nil {
		t.Fatalf("download operation was blocked by search circuit: %v", err)
	}
	release()
}
