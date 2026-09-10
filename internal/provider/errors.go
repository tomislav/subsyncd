package provider

import (
	"fmt"
	"time"
)

type CooldownError struct {
	// Suppressed means persisted state blocked the request before transport.
	Suppressed bool
	ProviderID string
	Scope      Operation
	Reason     string
	ResetAt    time.Time
}

func (e *CooldownError) Error() string {
	return fmt.Sprintf("provider %s %s operation is cooling down until %s: %s", e.ProviderID, e.Scope, e.ResetAt.UTC().Format(time.RFC3339), e.Reason)
}

type DisabledError struct {
	Suppressed bool
	ProviderID string
	Reason     string
}

func (e *DisabledError) Error() string {
	return fmt.Sprintf("provider %s is disabled: %s", e.ProviderID, e.Reason)
}

type AuthenticationError struct{ Message string }

func (e *AuthenticationError) Error() string { return "provider authentication failed: " + e.Message }

type InvalidPayloadError struct{ Message string }

func (e *InvalidPayloadError) Error() string {
	return "provider returned an invalid payload: " + e.Message
}

type QuotaError struct {
	Scope   Operation
	ResetAt time.Time
	Message string
}

func (e *QuotaError) Error() string { return "provider quota exhausted: " + e.Message }
