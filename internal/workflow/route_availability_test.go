package workflow

import (
	"context"
	"errors"
	"testing"
	"time"

	"subsyncd/internal/domain"
	"subsyncd/internal/provider"
)

// routeProvider is a provider with a configurable media-kind capability and
// download availability.
type routeProvider struct {
	*fakeProvider
	kinds       []domain.MediaKind
	unavailable error
	checks      int
}

func (p *routeProvider) Capabilities() provider.Capabilities {
	return provider.Capabilities{MediaKinds: p.kinds}
}

func (p *routeProvider) CheckDownloadAvailability(context.Context) error {
	p.checks++
	return p.unavailable
}

func routeService(preferred []*routeProvider, fallback []*routeProvider) *Service {
	s := &Service{Providers: map[string]provider.Provider{}}
	for _, p := range preferred {
		s.Providers[p.ID()] = p
		s.ProviderOrder = append(s.ProviderOrder, p.ID())
	}
	for _, p := range fallback {
		s.Providers[p.ID()] = p
		s.FallbackProviderOrder = append(s.FallbackProviderOrder, p.ID())
	}
	return s
}

func newRouteProvider(id string, unavailable error, kinds ...domain.MediaKind) *routeProvider {
	return &routeProvider{fakeProvider: &fakeProvider{id: id}, kinds: kinds, unavailable: unavailable}
}

func TestRouteAvailability(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	early, late := now.Add(time.Hour), now.Add(3*time.Hour)
	cooldown := func(id string, reset time.Time) error {
		return &provider.CooldownError{ProviderID: id, Scope: provider.OperationDownload, ResetAt: reset, Suppressed: true}
	}
	for _, test := range []struct {
		name      string
		preferred []*routeProvider
		fallback  []*routeProvider
		kind      domain.MediaKind
		want      RouteStatus
	}{
		{
			name:      "every provider cooling down pauses with earliest reset",
			preferred: []*routeProvider{newRouteProvider("a", cooldown("a", late))},
			fallback:  []*routeProvider{newRouteProvider("b", &provider.QuotaError{Scope: provider.OperationDownload, ResetAt: early})},
			kind:      domain.MediaMovie,
			want:      RouteStatus{Paused: true, ResetAt: early, ProviderCount: 2},
		},
		{
			name:      "one available provider keeps the route open",
			preferred: []*routeProvider{newRouteProvider("a", cooldown("a", late))},
			fallback:  []*routeProvider{newRouteProvider("b", nil)},
			kind:      domain.MediaMovie,
			want:      RouteStatus{},
		},
		{
			name:      "providers without the media kind are ignored",
			preferred: []*routeProvider{newRouteProvider("a", cooldown("a", late)), newRouteProvider("tv-only", nil, domain.MediaEpisode)},
			kind:      domain.MediaMovie,
			want:      RouteStatus{Paused: true, ResetAt: late, ProviderCount: 1},
		},
		{
			name:      "no provider for the media kind never pauses",
			preferred: []*routeProvider{newRouteProvider("tv-only", cooldown("tv-only", late), domain.MediaEpisode)},
			kind:      domain.MediaMovie,
			want:      RouteStatus{},
		},
		{
			name:      "all disabled pauses without a reset",
			preferred: []*routeProvider{newRouteProvider("a", &provider.DisabledError{ProviderID: "a", Reason: "auth"})},
			kind:      domain.MediaEpisode,
			want:      RouteStatus{Paused: true, ProviderCount: 1},
		},
		{
			name:      "a disabled provider and a cooldown pause until the cooldown resets",
			preferred: []*routeProvider{newRouteProvider("a", &provider.DisabledError{ProviderID: "a"}), newRouteProvider("b", cooldown("b", late))},
			kind:      domain.MediaEpisode,
			want:      RouteStatus{Paused: true, ResetAt: late, ProviderCount: 2},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := routeService(test.preferred, test.fallback).RouteAvailability(t.Context(), test.kind)
			if err != nil {
				t.Fatal(err)
			}
			if got.Paused != test.want.Paused || !got.ResetAt.Equal(test.want.ResetAt) || got.ProviderCount != test.want.ProviderCount {
				t.Fatalf("RouteAvailability() = %+v, want %+v", got, test.want)
			}
		})
	}
}

func TestRouteAvailabilityReturnsStateReadFailures(t *testing.T) {
	failure := &provider.AvailabilityError{Err: errors.New("database is locked")}
	s := routeService([]*routeProvider{newRouteProvider("a", failure)}, nil)
	if _, err := s.RouteAvailability(t.Context(), domain.MediaMovie); !errors.Is(err, failure) {
		t.Fatalf("RouteAvailability() error = %v, want %v", err, failure)
	}
}
