package workflow

import (
	"context"
	"time"

	"subsyncd/internal/domain"
	"subsyncd/internal/provider"
)

// RouteStatus reports whether a language route can acquire subtitles for one
// media kind right now.
type RouteStatus struct {
	// Paused is true when every provider on the route that supports the media
	// kind is unable to download: a cooldown, an exhausted quota, or disabled.
	Paused bool
	// ResetAt is the earliest cooldown or quota reset among those providers. It
	// is zero when all of them are disabled.
	ResetAt time.Time
	// ProviderCount is the number of route providers that support the media kind.
	ProviderCount int
	// RangesPaused is true when multi-episode files cannot be served although
	// single episodes can: every provider able to cover a range (not
	// SingleEpisodeOnly) is unavailable. RangesResetAt is its earliest reset.
	RangesPaused  bool
	RangesResetAt time.Time
}

// routeAvailability tracks one provider subset: paused while every member is
// unavailable, with the earliest reset.
type routeAvailability struct {
	count     int
	available bool
	resetAt   time.Time
}

func (a *routeAvailability) add(reset time.Time, available bool) {
	a.count++
	if available {
		a.available = true
		return
	}
	if !reset.IsZero() && (a.resetAt.IsZero() || reset.Before(a.resetAt)) {
		a.resetAt = reset
	}
}

func (a routeAvailability) paused() bool { return a.count > 0 && !a.available }

// RouteAvailability checks the preferred and fallback providers with the same
// read-only download preflight acquisition uses. It consumes no permits and
// makes no network request. A search-only cooldown does not pause the route,
// because cached search results can still lead to a download. A route with no
// provider for the media kind is never paused. State-read failures are returned.
func (s *Service) RouteAvailability(ctx context.Context, kind domain.MediaKind) (RouteStatus, error) {
	var all, ranges routeAvailability
	ids := append(append([]string(nil), s.ProviderOrder...), s.FallbackProviderOrder...)
	for _, id := range ids {
		item := s.Providers[id]
		if item == nil || !provider.SupportsMediaKind(item, kind) {
			continue
		}
		var reset time.Time
		err := provider.CheckDownloadAvailability(ctx, item)
		if err != nil {
			var unavailable bool
			if reset, unavailable = unavailableReset(err); !unavailable {
				return RouteStatus{}, err
			}
		}
		all.add(reset, err == nil)
		if kind == domain.MediaEpisode && !item.Capabilities().SingleEpisodeOnly {
			ranges.add(reset, err == nil)
		}
	}
	switch {
	case all.paused():
		return RouteStatus{Paused: true, ResetAt: all.resetAt, ProviderCount: all.count}, nil
	case ranges.paused():
		return RouteStatus{RangesPaused: true, RangesResetAt: ranges.resetAt, ProviderCount: ranges.count}, nil
	}
	return RouteStatus{}, nil
}
