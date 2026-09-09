package provider_test

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"testing"
	"time"

	"subsyncd/internal/observability"
	"subsyncd/internal/provider"
	"subsyncd/internal/provider/gestdown"
	"subsyncd/internal/provider/opensubtitles"
	"subsyncd/internal/provider/subdl"
	"subsyncd/internal/provider/titlovi"
	"subsyncd/internal/store"
	"subsyncd/internal/testutil"
)

type availabilityStateStore struct{ state store.ProviderState }

func (s *availabilityStateStore) GetProviderState(_ context.Context, id, scope string) (store.ProviderState, error) {
	if s.state.ProviderID == id && s.state.Scope == scope {
		return s.state, nil
	}
	return store.ProviderState{}, sql.ErrNoRows
}
func (s *availabilityStateStore) PutProviderState(_ context.Context, state store.ProviderState) error {
	s.state = state
	return nil
}

func TestCompiledAdaptersExposePersistedDownloadAvailability(t *testing.T) {
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	state := &availabilityStateStore{state: store.ProviderState{ProviderID: "p", Scope: "download", Reason: "download_quota", ResetAt: now.Add(time.Hour)}}
	clock := testutil.NewClock(now)
	transport := provider.Client{ProviderID: "p", Gate: provider.NewGate(state, clock, 1), Clock: clock, HTTP: &http.Client{}}
	constructors := map[string]func() (provider.Provider, error){
		"opensubtitles": func() (provider.Provider, error) {
			return opensubtitles.New(opensubtitles.Config{APIKey: "fixture", Username: "fixture", Password: "fixture", UserAgent: "test"}, transport, nil, nil, clock)
		},
		"subdl": func() (provider.Provider, error) { return subdl.New(subdl.Config{APIKey: "fixture"}, transport, clock) },
		"titlovi": func() (provider.Provider, error) {
			return titlovi.New(titlovi.Config{Username: "fixture", Password: "fixture"}, transport, clock)
		},
		"gestdown": func() (provider.Provider, error) { return gestdown.New(gestdown.Config{}, transport) },
	}
	for name, newProvider := range constructors {
		t.Run(name, func(t *testing.T) {
			p, err := newProvider()
			if err != nil {
				t.Fatal(err)
			}
			for _, p := range []provider.Provider{p, provider.Observe(p, observability.Discard())} {
				err := provider.CheckDownloadAvailability(t.Context(), p)
				var cooldown *provider.CooldownError
				if !errors.As(err, &cooldown) || !cooldown.ResetAt.Equal(state.state.ResetAt) {
					t.Fatalf("adapter availability=%v", err)
				}
			}
		})
	}
}
