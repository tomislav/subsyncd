package provider

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"

	"golang.org/x/time/rate"

	"subsyncd/internal/store"
)

type StateStore interface {
	GetProviderState(context.Context, string, string) (store.ProviderState, error)
	PutProviderState(context.Context, store.ProviderState) error
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
	instances map[string]*instanceGate
	origins   map[string]chan struct{}
}

func NewGate(stateStore StateStore, clock Clock, sharedMax int) *Gate {
	if sharedMax <= 0 {
		sharedMax = 1
	}
	return &Gate{store: stateStore, clock: clock, sharedMax: sharedMax, instances: make(map[string]*instanceGate), origins: make(map[string]chan struct{})}
}

func (g *Gate) Configure(providerID string, requestsPerSecond float64, burst, maxConcurrent int) {
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
	var once sync.Once
	return func() {
		once.Do(func() {
			<-shared
			<-instance.semaphore
		})
	}, nil
}

func (g *Gate) checkState(ctx context.Context, providerID string, operation Operation) error {
	for _, scope := range []Operation{OperationAll, OperationAuth, operation} {
		state, err := g.store.GetProviderState(ctx, providerID, string(scope))
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return fmt.Errorf("read provider %s throttle: %w", providerID, err)
		}
		if state.Disabled {
			return &DisabledError{ProviderID: providerID, Reason: state.Reason}
		}
		if state.Remaining <= 0 && state.ResetAt.After(g.clock.Now()) {
			return &CooldownError{ProviderID: providerID, Scope: scope, Reason: state.Reason, ResetAt: state.ResetAt}
		}
	}
	return nil
}

func (g *Gate) Persist(ctx context.Context, throttle Throttle) error {
	return g.store.PutProviderState(ctx, store.ProviderState{ProviderID: throttle.ProviderID, Scope: string(throttle.Scope), Reason: throttle.Reason, Limit: throttle.Limit, Remaining: throttle.Remaining, ResetAt: throttle.ResetAt, Disabled: throttle.Disabled})
}
