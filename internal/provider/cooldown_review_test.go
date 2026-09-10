package provider

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"subsyncd/internal/domain"
	"subsyncd/internal/observability"
	"subsyncd/internal/store"
	"subsyncd/internal/testutil"
	"testing"
	"time"
)

type lateCooldownProvider struct {
	*fakeProvider
	client Client
}

func (p *lateCooldownProvider) request(ctx context.Context, operation Operation) error {
	if err := p.client.Gate.Persist(ctx, Throttle{ProviderID: p.ID(), Scope: operation, ResetAt: p.client.Clock.Now().Add(time.Minute)}); err != nil {
		return err
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://example.test", nil)
	response, err := p.client.Do(ctx, operation, req)
	if response != nil {
		response.Body.Close()
	}
	return err
}
func (p *lateCooldownProvider) Search(ctx context.Context, _ SearchQuery) ([]domain.Candidate, error) {
	return nil, p.request(ctx, OperationSearch)
}
func (p *lateCooldownProvider) Download(ctx context.Context, _ domain.Candidate, _ io.Writer) (DownloadMetadata, error) {
	return DownloadMetadata{}, p.request(ctx, OperationDownload)
}

func TestCooldownAppearingAfterPreflightDoesNotWarnAtCompletion(t *testing.T) {
	for _, operation := range []Operation{OperationSearch, OperationDownload} {
		t.Run(string(operation), func(t *testing.T) {
			clock := testutil.NewClock(time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC))
			gate := NewGate(&memoryStateStore{states: map[string]store.ProviderState{}}, clock, 1)
			gate.Configure("p", 1000, 1, 1)
			requests := 0
			client := Client{ProviderID: "p", Gate: gate, Clock: clock, HTTP: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { requests++; return nil, errors.New("unexpected request") })}}
			p := &lateCooldownProvider{fakeProvider: &fakeProvider{id: "p"}, client: client}
			var logs bytes.Buffer
			events, _ := observability.New(&logs, observability.Options{Level: "debug"})
			observed := Observe(p, events)
			var err error
			if operation == OperationSearch {
				c := newTestCoordinator(observed)
				c.Events = events
				err = c.Search(t.Context(), SearchQuery{Media: testQueryMedia(), Language: "en", Mode: SearchBroad}).Errors["p"]
			} else {
				_, err = observed.Download(t.Context(), domain.Candidate{ResultID: "one"}, io.Discard)
			}
			if !errors.As(err, new(*CooldownError)) || requests != 0 {
				t.Fatalf("error=%v requests=%d", err, requests)
			}
			records := providerEvents(providerLogRecords(t, logs.String()), "provider."+string(operation)+"_completed")
			if len(records) != 1 || records[0]["level"] != "debug" {
				t.Fatalf("completion=%+v", records)
			}
		})
	}
}

type failedStateStore struct{ readErr, writeErr error }

func (s failedStateStore) GetProviderState(_ context.Context, id, scope string) (store.ProviderState, error) {
	return store.ProviderState{ProviderID: id, Scope: scope, FailureAttempt: 1}, s.readErr
}
func (s failedStateStore) PutProviderState(context.Context, store.ProviderState) error {
	return s.writeErr
}

func TestGateStateFailuresRemainTyped(t *testing.T) {
	sentinel := errors.New("state failure")
	for _, test := range []struct {
		name  string
		store failedStateStore
		run   func(*Gate) error
	}{
		{"acquire", failedStateStore{readErr: sentinel}, func(g *Gate) error {
			_, err := g.Acquire(t.Context(), "p", "example.test", OperationSearch)
			return err
		}},
		{"persist", failedStateStore{writeErr: sentinel}, func(g *Gate) error { return g.Persist(t.Context(), Throttle{ProviderID: "p", Scope: OperationSearch}) }},
		{"circuit", failedStateStore{writeErr: sentinel}, func(g *Gate) error {
			_, err := g.RecordTransientFailure(t.Context(), "p", OperationSearch, "network_error", time.Time{})
			return err
		}},
		{"recover", failedStateStore{writeErr: sentinel}, func(g *Gate) error { return g.ResetTransientFailures(t.Context(), "p", OperationSearch) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := test.run(NewGate(test.store, SystemClock{}, 1))
			if !errors.Is(err, sentinel) || !errors.As(err, new(*AvailabilityError)) {
				t.Fatalf("untyped persistence error=%v", err)
			}
		})
	}
}

func TestGateUsesLatestApplicableCooldownReset(t *testing.T) {
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	gate := NewGate(&memoryStateStore{states: map[string]store.ProviderState{}}, testutil.NewClock(now), 1)
	for i, scope := range []Operation{OperationAll, OperationSearch} {
		if err := gate.Persist(t.Context(), Throttle{ProviderID: "p", Scope: scope, ResetAt: now.Add(time.Duration(i+1) * time.Hour)}); err != nil {
			t.Fatal(err)
		}
	}
	_, err := gate.Acquire(t.Context(), "p", "example.test", OperationSearch)
	var cooldown *CooldownError
	if !errors.As(err, &cooldown) || !cooldown.ResetAt.Equal(now.Add(2*time.Hour)) {
		t.Fatalf("gate reset=%v", err)
	}
}
