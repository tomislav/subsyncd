package notifier

import (
	"context"
	"errors"

	"subsyncd/internal/domain"
)

type Notifier interface {
	SubtitleChanged(context.Context, domain.Media, string) error
}

type Noop struct{}

func (Noop) SubtitleChanged(context.Context, domain.Media, string) error { return nil }

type DeliveryError struct {
	StatusCode int
	Retryable  bool
	Reason     string
}

func (e *DeliveryError) Error() string {
	if e.StatusCode != 0 {
		return "Silo notification failed with HTTP status " + e.Reason
	}
	return "Silo notification failed: " + e.Reason
}

func IsRetryable(err error) bool {
	var delivery *DeliveryError
	return errors.As(err, &delivery) && delivery.Retryable
}
