package provider

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"

	"subsyncd/internal/observability"
	"subsyncd/internal/store"
)

type StateStore interface {
	GetProviderState(context.Context, string, string) (store.ProviderState, error)
	PutProviderState(context.Context, store.ProviderState) error
}

// Preserve state-store failures through adapter and workflow error handling.
type stateErrorStore struct{ StateStore }

func (s stateErrorStore) GetProviderState(ctx context.Context, id, scope string) (store.ProviderState, error) {
	state, err := s.StateStore.GetProviderState(ctx, id, scope)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		err = &AvailabilityError{Err: err}
	}
	return state, err
}

func (s stateErrorStore) PutProviderState(ctx context.Context, state store.ProviderState) error {
	if err := s.StateStore.PutProviderState(ctx, state); err != nil {
		return &AvailabilityError{Err: err}
	}
	return nil
}

type instanceGate struct {
	limiter   *rate.Limiter
	semaphore chan struct{}
}

type Gate struct {
	store     StateStore
	clock     Clock
	sharedMax int
	mu        sync.Mutex
	stateMu   sync.Mutex
	instances map[string]*instanceGate
	origins   map[string]chan struct{}
	types     map[string]string
	events    *observability.Emitter
}

var transientFailureDelays = [...]time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute, time.Hour}

func NewGate(stateStore StateStore, clock Clock, sharedMax int, emitters ...*observability.Emitter) *Gate {
	if sharedMax <= 0 {
		sharedMax = 1
	}
	events := observability.Discard()
	if len(emitters) > 0 && emitters[0] != nil {
		events = emitters[0]
	}
	return &Gate{store: stateErrorStore{stateStore}, clock: clock, sharedMax: sharedMax, instances: make(map[string]*instanceGate), origins: make(map[string]chan struct{}), types: make(map[string]string), events: events.For("provider")}
}

func (g *Gate) Configure(providerID string, requestsPerSecond float64, burst, maxConcurrent int, providerTypes ...string) {
	if requestsPerSecond <= 0 {
		requestsPerSecond = 1
	}
	if burst <= 0 {
		burst = 1
	}
	if maxConcurrent <= 0 {
		maxConcurrent = 1
	}
	g.mu.Lock()
	g.instances[providerID] = &instanceGate{limiter: rate.NewLimiter(rate.Limit(requestsPerSecond), burst), semaphore: make(chan struct{}, maxConcurrent)}
	if len(providerTypes) > 0 {
		g.types[providerID] = observability.SafeText(providerTypes[0])
	}
	g.mu.Unlock()
}

func (g *Gate) Acquire(ctx context.Context, providerID, origin string, operation Operation) (func(), error) {
	if err := g.checkState(ctx, providerID, operation); err != nil {
		return nil, err
	}
	g.mu.Lock()
	instance := g.instances[providerID]
	shared := g.origins[origin]
	if shared == nil {
		shared = make(chan struct{}, g.sharedMax)
		g.origins[origin] = shared
	}
	g.mu.Unlock()
	if instance == nil {
		return nil, fmt.Errorf("provider %q has no configured limiter", providerID)
	}
	if err := instance.limiter.Wait(ctx); err != nil {
		return nil, fmt.Errorf("wait for provider %s rate token: %w", providerID, err)
	}
	select {
	case instance.semaphore <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	select {
	case shared <- struct{}{}:
	case <-ctx.Done():
		<-instance.semaphore
		return nil, ctx.Err()
	}
	if err := g.checkState(ctx, providerID, operation); err != nil {
		<-shared
		<-instance.semaphore
		return nil, err
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			<-shared
			<-instance.semaphore
		})
	}, nil
}

func (g *Gate) checkState(ctx context.Context, providerID string, operation Operation) error {
	return g.checkAvailability(ctx, providerID, operation == OperationAuth, OperationAll, OperationAuth, operation)
}

func (g *Gate) Persist(ctx context.Context, throttle Throttle) error {
	g.stateMu.Lock()
	defer g.stateMu.Unlock()
	state := store.ProviderState{ProviderID: throttle.ProviderID, Scope: string(throttle.Scope), Reason: throttle.Reason, Limit: throttle.Limit, Remaining: throttle.Remaining, ResetAt: throttle.ResetAt, Disabled: throttle.Disabled}
	previous, found, err := g.previousState(ctx, state.ProviderID, state.Scope)
	if err != nil {
		return err
	}
	state.FailureAttempt = previous.FailureAttempt
	// Only explicit provider retry may clear a permanent disable. Responses
	// already in flight when authentication failed must not restore availability.
	if previous.Disabled {
		state = previous
	}
	if err := g.store.PutProviderState(ctx, state); err != nil {
		return err
	}
	if state.Scope == string(OperationAuth) && state.Disabled && (!found || !previous.Disabled) {
		g.events.Log(ctx, slog.LevelWarn, "provider.auth_disabled", "provider authentication disabled", append(g.stateAttrs(state.ProviderID, state.Scope), slog.String("reason", "authentication_failed"))...)
		return nil
	}
	unavailable := !state.Disabled && state.Remaining <= 0 && state.ResetAt.After(g.clock.Now())
	previousUnavailable := found && !previous.Disabled && previous.Remaining <= 0 && previous.ResetAt.After(g.clock.Now())
	if unavailable && (!previousUnavailable || !previous.ResetAt.Equal(state.ResetAt) || previous.Reason != state.Reason) {
		attrs := append(g.stateAttrs(state.ProviderID, state.Scope), slog.String("reason", boundedProviderReason(state.Reason)), slog.Time("reset_at", state.ResetAt.UTC()))
		g.events.Log(ctx, slog.LevelWarn, "provider.cooldown_started", "provider cooldown started", attrs...)
	}
	return nil
}

func (g *Gate) RecordTransientFailure(ctx context.Context, providerID string, operation Operation, reason string, resetAt time.Time) (store.ProviderState, error) {
	g.stateMu.Lock()
	defer g.stateMu.Unlock()

	attempt := 1
	existing, err := g.store.GetProviderState(ctx, providerID, string(operation))
	if err == nil && existing.FailureAttempt > 0 {
		attempt = existing.FailureAttempt + 1
	} else if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return store.ProviderState{}, fmt.Errorf("read provider %s transient failure state: %w", providerID, err)
	}
	if reason == "" {
		reason = "transient_failure"
	} else if !strings.HasPrefix(reason, "transient_") {
		reason = "transient_" + reason
	}
	if !resetAt.After(g.clock.Now()) {
		index := attempt - 1
		if index >= len(transientFailureDelays) {
			index = len(transientFailureDelays) - 1
		}
		resetAt = g.clock.Now().Add(transientFailureDelays[index])
	}
	state := store.ProviderState{
		ProviderID:     providerID,
		Scope:          string(operation),
		Reason:         reason,
		Remaining:      0,
		ResetAt:        resetAt,
		FailureAttempt: attempt,
	}
	if existing.Disabled {
		state = existing
		state.FailureAttempt = attempt
	} else if existing.Remaining <= 0 && existing.ResetAt.After(g.clock.Now()) && !strings.HasPrefix(existing.Reason, "transient_") {
		state.Reason, state.Limit, state.Remaining, state.Disabled = existing.Reason, existing.Limit, existing.Remaining, existing.Disabled
		if existing.ResetAt.After(state.ResetAt) {
			state.ResetAt = existing.ResetAt
		}
	}
	if err := g.store.PutProviderState(ctx, state); err != nil {
		return store.ProviderState{}, fmt.Errorf("persist provider %s transient failure: %w", providerID, err)
	}
	attrs := append(g.stateAttrs(providerID, string(operation)), slog.String("reason", boundedProviderReason(reason)), slog.Int("attempt", attempt), slog.Time("reset_at", resetAt.UTC()))
	g.events.Log(ctx, slog.LevelWarn, "provider.circuit_opened", "provider transient-failure circuit opened", attrs...)
	return state, nil
}

func (g *Gate) ResetTransientFailures(ctx context.Context, providerID string, operation Operation) error {
	g.stateMu.Lock()
	defer g.stateMu.Unlock()

	state, err := g.store.GetProviderState(ctx, providerID, string(operation))
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read provider %s transient failure state: %w", providerID, err)
	}
	if state.FailureAttempt == 0 {
		return nil
	}
	previousAttempt := state.FailureAttempt
	state.FailureAttempt = 0
	if strings.HasPrefix(state.Reason, "transient_") {
		state.Reason = ""
		state.Limit = 0
		state.Remaining = 0
		state.ResetAt = time.Time{}
		state.Disabled = false
	}
	if err := g.store.PutProviderState(ctx, state); err != nil {
		return fmt.Errorf("reset provider %s transient failures: %w", providerID, err)
	}
	attrs := append(g.stateAttrs(providerID, string(operation)), slog.String("reason", "transient_cleared"), slog.Int("previous_attempt", previousAttempt))
	g.events.Log(ctx, slog.LevelInfo, "provider.recovered", "provider recovered", attrs...)
	return nil
}

func (g *Gate) previousState(ctx context.Context, providerID, scope string) (store.ProviderState, bool, error) {
	state, err := g.store.GetProviderState(ctx, providerID, scope)
	if errors.Is(err, sql.ErrNoRows) {
		return store.ProviderState{}, false, nil
	}
	if err != nil {
		return store.ProviderState{}, false, fmt.Errorf("read provider %s state: %w", providerID, err)
	}
	return state, true, nil
}

func (g *Gate) stateAttrs(providerID, scope string) []slog.Attr {
	g.mu.Lock()
	providerType := g.types[providerID]
	g.mu.Unlock()
	attrs := []slog.Attr{slog.String("provider", providerID), slog.String("scope", scope)}
	if providerType != "" {
		attrs = append(attrs, slog.String("provider_type", providerType))
	}
	return attrs
}

func boundedProviderReason(reason string) string {
	switch reason {
	case "ratelimit", "x-ratelimit", "retry-after", "json", "fallback", "rate_limit", "download_quota", "service_busy":
		return reason
	case "transient_network_error", "network_error":
		return "network_error"
	default:
		if strings.HasPrefix(reason, "transient_http_5") {
			return "http_5xx"
		}
		return "provider_state"
	}
}
