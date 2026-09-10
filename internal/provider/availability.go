package provider

import (
	"context"
	"database/sql"
	"errors"
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
