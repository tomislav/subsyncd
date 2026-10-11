package provider

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// DownloadAvailability is an optional, read-only preflight for acquisition.
// Transport still checks persisted state when it acquires request permits.
type DownloadAvailability interface {
	CheckDownloadAvailability(context.Context) error
}

// SearchAvailability is checked only when reusable search results are absent.
type SearchAvailability interface {
	CheckSearchAvailability(context.Context) error
}

// DownloadRefusalRecorder is an optional hook for a download that delivered
// the provider's limit, login or client-block message instead of a subtitle.
// It pauses the provider's downloads for the cooldown of the given kind and
// returns the CooldownError to report.
type DownloadRefusalRecorder interface {
	RecordDownloadRefusal(context.Context, CooldownKind) error
}

// RecordDownloadRefusal pauses p's downloads after a refused download. It
// returns nil when p cannot record a cooldown.
func RecordDownloadRefusal(ctx context.Context, p Provider, kind CooldownKind) error {
	if recorder, ok := p.(DownloadRefusalRecorder); ok {
		return recorder.RecordDownloadRefusal(ctx, kind)
	}
	return nil
}

// AvailabilityError keeps state-store read/write failures terminal across tiers.
type AvailabilityError struct{ Err error }

func (e *AvailabilityError) Error() string { return "provider state persistence failed" }
func (e *AvailabilityError) Unwrap() error { return e.Err }

func CheckDownloadAvailability(ctx context.Context, p Provider) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if available, ok := p.(DownloadAvailability); ok {
		return classifyAvailabilityError(available.CheckDownloadAvailability(ctx))
	}
	return nil
}

func CheckSearchAvailability(ctx context.Context, p Provider) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if available, ok := p.(SearchAvailability); ok {
		return classifyAvailabilityError(available.CheckSearchAvailability(ctx))
	}
	return nil
}

func classifyAvailabilityError(err error) error {
	var cooldown *CooldownError
	var quota *QuotaError
	var disabled *DisabledError
	var availability *AvailabilityError
	if err == nil || errors.As(err, &cooldown) || errors.As(err, &quota) || errors.As(err, &disabled) || errors.As(err, &availability) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return &AvailabilityError{Err: err}
}

// CheckDownloadAvailability reads state without waiting for or consuming permits.
// Search quotas remain independent. An issued OpenSubtitles URL can still be
// redeemed directly through OperationDownloadTransfer after its API quota ends.
func (c Client) CheckDownloadAvailability(ctx context.Context) error {
	return c.checkAvailability(ctx, OperationAll, OperationAuth, OperationDownload, OperationDownloadTransfer)
}

func (c Client) CheckSearchAvailability(ctx context.Context) error {
	return c.checkAvailability(ctx, OperationAll, OperationAuth, OperationSearch)
}

func (c Client) checkAvailability(ctx context.Context, scopes ...Operation) error {
	return c.Gate.checkAvailability(ctx, c.ProviderID, false, scopes...)
}

// checkAvailability shares scope precedence between preflight and the final gate.
func (g *Gate) checkAvailability(ctx context.Context, providerID string, includeAuthCooldown bool, scopes ...Operation) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	var blocked *CooldownError
	for _, scope := range scopes {
		state, err := g.store.GetProviderState(ctx, providerID, string(scope))
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return err
		}
		if state.Disabled {
			return &DisabledError{ProviderID: providerID, Reason: state.Reason, Suppressed: true}
		}
		if scope == OperationAuth && !includeAuthCooldown {
			continue
		}
		if state.Remaining <= 0 && state.ResetAt.After(g.clock.Now()) && (blocked == nil || state.ResetAt.After(blocked.ResetAt)) {
			blocked = &CooldownError{ProviderID: providerID, Scope: scope, Reason: state.Reason, ResetAt: state.ResetAt, Suppressed: true}
		}
	}
	if blocked != nil {
		return blocked
	}
	return nil
}

const (
	// downloadRefusalWindow is how long downloads pause after a refused
	// download when the provider has no known quota reset, and after a
	// client block (Titlovi's "unsupported plugin" placeholder).
	downloadRefusalWindow = time.Hour
	// downloadRateLimitWindow is how long downloads pause after a "too many
	// requests" refusal when the provider has no rate-limit fallback.
	downloadRateLimitWindow = 15 * time.Minute
)

// RecordDownloadRefusal persists a download cooldown of kind and returns it
// as a CooldownError: for a download quota, until the provider's known quota
// reset or for an hour; for a rate limit, the provider's rate-limit fallback
// or 15 minutes; for a client block, an hour.
func (c Client) RecordDownloadRefusal(ctx context.Context, kind CooldownKind) error {
	now := c.Clock.Now()
	var resetAt time.Time
	switch kind {
	case CooldownRateLimit:
		resetAt = FallbackReset(now, c.ProviderType, CooldownRateLimit)
		if resetAt.IsZero() {
			resetAt = now.Add(downloadRateLimitWindow)
		}
	case CooldownClientBlocked:
		resetAt = now.Add(downloadRefusalWindow)
	default:
		kind = CooldownDownloadQuota
		resetAt = FallbackReset(now, c.ProviderType, CooldownDownloadQuota)
		if resetAt.IsZero() {
			resetAt = now.Add(downloadRefusalWindow)
		}
	}
	resetAt, err := c.PersistCooldown(ctx, OperationDownload, kind, resetAt)
	if err != nil {
		return err
	}
	return &CooldownError{ProviderID: c.ProviderID, Scope: OperationDownload, Reason: string(kind), ResetAt: resetAt}
}
