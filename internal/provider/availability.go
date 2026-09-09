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

// AvailabilityError keeps state-store failures terminal across provider tiers.
type AvailabilityError struct{ Err error }

func (e *AvailabilityError) Error() string { return "read provider availability failed" }
func (e *AvailabilityError) Unwrap() error { return e.Err }

func CheckDownloadAvailability(ctx context.Context, p Provider) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if available, ok := p.(DownloadAvailability); ok {
		err := available.CheckDownloadAvailability(ctx)
		var cooldown *CooldownError
		var quota *QuotaError
		var disabled *DisabledError
		if err == nil || errors.As(err, &cooldown) || errors.As(err, &quota) || errors.As(err, &disabled) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		return &AvailabilityError{Err: err}
	}
	return nil
}

// CheckDownloadAvailability reads state without waiting for or consuming permits.
// Search quotas remain independent. An issued OpenSubtitles URL can still be
// redeemed directly through OperationDownloadTransfer after its API quota ends.
func (c Client) CheckDownloadAvailability(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	var blocked *CooldownError
	for _, scope := range []Operation{OperationAll, OperationAuth, OperationDownload, OperationDownloadTransfer} {
		state, err := c.Gate.store.GetProviderState(ctx, c.ProviderID, string(scope))
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return err
		}
		if state.Disabled {
			return &DisabledError{ProviderID: c.ProviderID, Reason: state.Reason}
		}
		if scope == OperationAuth {
			continue
		}
		if state.Remaining <= 0 && state.ResetAt.After(c.Gate.clock.Now()) && (blocked == nil || state.ResetAt.After(blocked.ResetAt)) {
			blocked = &CooldownError{ProviderID: c.ProviderID, Scope: scope, Reason: state.Reason, ResetAt: state.ResetAt}
		}
	}
	if blocked != nil {
		return blocked
	}
	return nil
}
