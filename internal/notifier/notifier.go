package notifier

import (
	"context"
	"errors"

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
