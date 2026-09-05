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

// This body models a response that has returned headers but is still streaming.
// Closing it unblocks a pending read, like a net/http response body.
type streamingPermitTestBody struct {
	ready  chan struct{}
	closed chan struct{}
	once   sync.Once
	end    sync.Once
}

func newStreamingPermitTestBody() *streamingPermitTestBody {
	return &streamingPermitTestBody{ready: make(chan struct{}), closed: make(chan struct{})}
}

func (b *streamingPermitTestBody) Read([]byte) (int, error) {
	<-b.ready
	return 0, io.EOF
}

func (b *streamingPermitTestBody) finish() { b.end.Do(func() { close(b.ready) }) }

func (b *streamingPermitTestBody) Close() error {
	b.once.Do(func() { close(b.closed) })
	b.finish()
	return nil
}

func TestTransportPermitLivesThroughResponseBody(t *testing.T) {
	for _, shared := range []bool{false, true} {
		for _, eof := range []bool{false, true} {
			for _, status := range []int{http.StatusOK, http.StatusUnauthorized, http.StatusTooManyRequests, http.StatusServiceUnavailable} {
				t.Run(fmt.Sprintf("shared=%t/eof=%t/status=%d", shared, eof, status), func(t *testing.T) {
					clock := testutil.NewClock(time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC))
					sharedMax := 2
					secondProvider := "one"
					if shared {
						sharedMax, secondProvider = 1, "two"
					}
					gate := NewGate(&memoryStateStore{states: map[string]store.ProviderState{}}, clock, sharedMax)
					gate.Configure("one", 1000, 10, 1)
					gate.Configure("two", 1000, 10, 1)
					body := newStreamingPermitTestBody()
					t.Cleanup(func() { _ = body.Close() })
					httpClient := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
						if request.URL.Path == "/first" {
							return &http.Response{StatusCode: status, Header: make(http.Header), Body: body}, nil
						}
						return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("ok"))}, nil
					})}
					client := Client{HTTP: httpClient, Gate: gate, Clock: clock, ProviderID: "one", ProviderType: "subdl"}
					ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
					defer cancel()
					request, _ := http.NewRequest(http.MethodGet, "https://api.example/first", nil)
					first, err := client.Do(ctx, OperationSearch, request)
					if first == nil || (err != nil) != (status != http.StatusOK) {
						t.Fatalf("first response/error = %v / %v", first, err)
					}
					defer first.Body.Close()
					readDone := make(chan error, 1)
					go func() { _, err := io.ReadAll(first.Body); readDone <- err }()
					client.ProviderID = secondProvider
					type result struct {
						response *http.Response
						err      error
					}
					start := func() <-chan result {
						done := make(chan result, 1)
						go func() {
							request, _ := http.NewRequest(http.MethodGet, "https://api.example/next", nil)
							// A search cooldown must not mask the concurrency limit.
							response, err := client.Do(ctx, OperationDownload, request)
							done <- result{response, err}
						}()
						return done
					}
					assertBlocked := func(done <-chan result) {
						t.Helper()
						select {
						case got := <-done:
							if got.response != nil {
								_ = got.response.Body.Close()
							}
							t.Fatalf("request returned before current body ended: %v", got.err)
						case <-time.After(20 * time.Millisecond):
						}
					}
					awaitResponse := func(done <-chan result) *http.Response {
						t.Helper()
						select {
						case got := <-done:
							if got.err != nil {
								t.Fatal(got.err)
							}
							return got.response
						case <-ctx.Done():
							t.Fatal("request did not acquire after body ended")
							return nil
						}
					}
					secondDone := start()
					assertBlocked(secondDone)
					if eof {
						body.finish()
					} else if err := first.Body.Close(); err != nil {
						t.Fatal(err)
					}
					if err := <-readDone; err != nil {
						t.Fatal(err)
					}
					second := awaitResponse(secondDone)
					defer second.Body.Close()
					if eof {
						select {
						case <-body.closed:
							t.Fatal("EOF unexpectedly closed the underlying body")
						default:
						}
					}
					// Old body cleanup must not release a permit now owned by second.
					var cleanup sync.WaitGroup
					for i := 0; i < 4; i++ {
						cleanup.Add(1)
						go func() { defer cleanup.Done(); _ = first.Body.Close(); _, _ = first.Body.Read(make([]byte, 1)) }()
					}
					cleanup.Wait()
					thirdDone := start()
					assertBlocked(thirdDone)
					_ = second.Body.Close()
					third := awaitResponse(thirdDone)
					_ = third.Body.Close()
				})
			}
		}
	}
}

type permitErrorTestBody struct {
	readErr  error
	closeErr error
}

func (b permitErrorTestBody) Read(payload []byte) (int, error) {
	return copy(payload, "ok"), b.readErr
}
func (b permitErrorTestBody) Close() error { return b.closeErr }

func TestPermitBodyPreservesDataAndErrors(t *testing.T) {
	readErr, closeErr := errors.New("read failed"), errors.New("close failed")
	for _, terminal := range []error{io.EOF, readErr} {
		t.Run(terminal.Error(), func(t *testing.T) {
			releases := 0
			body := &permitBody{
				body:    permitErrorTestBody{readErr: terminal, closeErr: closeErr},
				release: func() { releases++ },
			}
			payload := make([]byte, 2)
			n, err := body.Read(payload)
			if n != 2 || string(payload) != "ok" || err != terminal {
				t.Fatalf("Read = %d / %q / %v", n, payload, err)
			}
			if terminal == io.EOF && releases != 1 {
				t.Fatalf("EOF release count = %d", releases)
			}
			if terminal != io.EOF && releases != 0 {
				t.Fatalf("non-EOF read error released before Close: %d", releases)
			}
			for i := 0; i < 2; i++ {
				if err := body.Close(); err != closeErr {
					t.Fatalf("Close = %v", err)
				}
			}
			if releases != 1 {
				t.Fatalf("release count = %d", releases)
			}
		})
	}
}

func TestTransportPermitReleasedOnTransportFailure(t *testing.T) {
	for _, failure := range []string{"network", "invalid_nil_body", "canceled"} {
		t.Run(failure, func(t *testing.T) {
			clock := testutil.NewClock(time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC))
			gate := NewGate(&memoryStateStore{states: map[string]store.ProviderState{}}, clock, 1)
			gate.Configure("one", 1000, 10, 1)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			httpClient := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				switch failure {
				case "invalid_nil_body":
					return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), ContentLength: 2}, nil
				case "canceled":
					cancel()
				}
				return nil, errors.New("transport failed")
			})}
			client := Client{HTTP: httpClient, Gate: gate, Clock: clock, ProviderID: "one", ProviderType: "subdl"}
			request, _ := http.NewRequest(http.MethodGet, "https://api.example/first", nil)
			response, err := client.Do(ctx, OperationSearch, request)
			if err == nil || response != nil {
				t.Fatalf("response/error = %v / %v", response, err)
			}
			if failure == "canceled" && !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation = %v", err)
			}
			// Both semaphores must be available without response cleanup, even if
			// search failure persisted a circuit for this provider.
			nextCtx, nextCancel := context.WithTimeout(context.Background(), time.Second)
			defer nextCancel()
			release, err := gate.Acquire(nextCtx, "one", "https://api.example", OperationDownload)
			if err != nil {
				t.Fatalf("transport failure leaked permit: %v", err)
			}
			release()
		})
	}
}

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
