package workflow

import (
	"context"
	"errors"
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
}

// RouteAvailability checks the preferred and fallback providers with the same
// read-only download preflight acquisition uses. It consumes no permits and
// makes no network request. A search-only cooldown does not pause the route,
// because cached search results can still lead to a download. A route with no
// provider for the media kind is never paused. State-read failures are returned.
func (s *Service) RouteAvailability(ctx context.Context, kind domain.MediaKind) (RouteStatus, error) {
	var status RouteStatus
	ids := append(append([]string(nil), s.ProviderOrder...), s.FallbackProviderOrder...)
	for _, id := range ids {
		item := s.Providers[id]
		if item == nil || !provider.SupportsMediaKind(item, kind) {
			continue
		}
		status.ProviderCount++
		err := provider.CheckDownloadAvailability(ctx, item)
		var cooldown *provider.CooldownError
		var quota *provider.QuotaError
		var disabled *provider.DisabledError
		var reset time.Time
		switch {
		case err == nil:
			return RouteStatus{}, nil
		case errors.As(err, &cooldown):
			reset = cooldown.ResetAt
		case errors.As(err, &quota):
			reset = quota.ResetAt
		case errors.As(err, &disabled):
		default:
			return RouteStatus{}, err
		}
		if !reset.IsZero() && (status.ResetAt.IsZero() || reset.Before(status.ResetAt)) {
			status.ResetAt = reset
		}
	}
	if status.ProviderCount == 0 {
		return RouteStatus{}, nil
	}
	status.Paused = true
	return status, nil
}
