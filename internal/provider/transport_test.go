package provider

import (
	"context"
	"database/sql"
	"errors"
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
