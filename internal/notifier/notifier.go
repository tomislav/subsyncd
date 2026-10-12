package notifier

import (
	"context"
	"errors"
	"log/slog"

	"subsyncd/internal/domain"
)

type Notifier interface {
	SubtitleChanged(context.Context, domain.Media, string) error
}

// Coalescer is implemented by notifiers that can satisfy several pending
// deliveries with one request. Deliveries with equal keys from the same
// notifier are interchangeable once their subtitles are installed.
type Coalescer interface {
	CoalesceKey(media domain.Media, subtitlePath string) (string, error)
}

// Describer is implemented by notifiers whose deliveries have a log-safe
// target worth naming in the worker's notification events (never paths,
// URLs or credentials).
type Describer interface {
	DeliveryAttrs(media domain.Media) []slog.Attr
}

type Noop struct{}

func (Noop) SubtitleChanged(context.Context, domain.Media, string) error { return nil }

type DeliveryError struct {
	// Target names the receiving service in the message; empty means Silo.
	Target     string
	StatusCode int
	Retryable  bool
	// Refused marks a final answer the target gives for this delivery alone
	// (an Arr 4xx such as a deleted series): the intent is given up with a
	// warning rather than reported as a failure.
	Refused bool
	Reason  string
}

func (e *DeliveryError) Error() string {
	target := e.Target
	if target == "" {
		target = "Silo"
	}
	if e.StatusCode != 0 {
		return target + " notification failed with HTTP status " + e.Reason
	}
	return target + " notification failed: " + e.Reason
}

// SkippedError reports a delivery the notifier decided not to send, such as
// an Arr rescan for an instance whose rescans were turned off after the
// intent was queued. The intent is finished without a request.
type SkippedError struct {
	Reason string
}

func (e *SkippedError) Error() string { return "notification skipped: " + e.Reason }

// IsSkipped reports whether err is a SkippedError and returns its reason.
func IsSkipped(err error) (string, bool) {
	var skipped *SkippedError
	if errors.As(err, &skipped) {
		return skipped.Reason, true
	}
	return "", false
}

// IsRefused reports whether err is a delivery the target refused for good.
func IsRefused(err error) bool {
	var delivery *DeliveryError
	return errors.As(err, &delivery) && delivery.Refused
}

func IsRetryable(err error) bool {
	var delivery *DeliveryError
	return errors.As(err, &delivery) && delivery.Retryable
}
