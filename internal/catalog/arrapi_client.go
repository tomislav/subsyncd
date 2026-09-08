package catalog

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"time"

	"github.com/cplieger/arrapi/v2"

	"subsyncd/internal/observability"
)

type arrHistoryClient interface {
	History(context.Context, arrapi.HistoryOptions) (arrapi.HistoryPage, error)
}

type sonarrEntityClient interface {
	arrHistoryClient
	EpisodeByID(context.Context, int) (arrapi.Episode, error)
}

type sonarrLibraryClient interface {
	Series(context.Context) ([]arrapi.Series, error)
	EpisodeFiles(context.Context, int) ([]arrapi.EpisodeFile, error)
}

type radarrEntityClient interface {
	arrHistoryClient
	MovieByID(context.Context, int) (arrapi.Movie, error)
}

type radarrLibraryClient interface {
	Movies(context.Context) ([]arrapi.Movie, error)
}

func newSonarrEntityClient(instance, baseURL, apiKey string, events *observability.Emitter) (sonarrEntityClient, error) {
	return arrapi.NewSonarr(
		baseURL,
		arrapi.APIKey(apiKey),
		arrapi.WithTimeout(15*time.Second),
		arrapi.WithLogger(slog.New(arrRetryHandler{Events: events, Instance: instance})),
	)
}

func newRadarrEntityClient(instance, baseURL, apiKey string, events *observability.Emitter) (radarrEntityClient, error) {
	return arrapi.NewRadarr(
		baseURL,
		arrapi.APIKey(apiKey),
		arrapi.WithTimeout(15*time.Second),
		arrapi.WithLogger(slog.New(arrRetryHandler{Events: events, Instance: instance})),
	)
}

// arrRetryHandler is the privacy boundary for arrapi retry diagnostics. It
// intentionally discards the upstream message, attributes, and groups.
type arrRetryHandler struct {
	Events   *observability.Emitter
	Instance string
}

func (h arrRetryHandler) Enabled(context.Context, slog.Level) bool {
	return true
}

func (h arrRetryHandler) Handle(ctx context.Context, record slog.Record) error {
	events := h.Events
	if events == nil {
		events = observability.Discard()
	}
	event := "arr.request_retry"
	message := "Arr request retry scheduled"
	if record.Level >= slog.LevelWarn {
		event = "arr.request_retries_exhausted"
		message = "Arr request retries exhausted"
	}
	events.For("catalog").Log(
		ctx,
		record.Level,
		event,
		message,
		slog.String("instance", observability.SafeText(h.Instance)),
	)
	return nil
}

func (h arrRetryHandler) WithAttrs([]slog.Attr) slog.Handler {
	return h
}

func (h arrRetryHandler) WithGroup(string) slog.Handler {
	return h
}

type arrAPIError struct {
	Instance   string
	Operation  string
	Kind       string
	StatusCode int
	Retryable  bool
}

func (e *arrAPIError) Error() string {
	return fmt.Sprintf(
		"Arr API request failed: instance=%s operation=%s kind=%s status=%d retryable=%t",
		observability.SafeText(e.Instance),
		observability.SafeText(e.Operation),
		e.Kind,
		e.StatusCode,
		e.Retryable,
	)
}

func (e *arrAPIError) Is(target error) bool {
	return e.Kind == "canceled" && target == context.Canceled || e.Kind == "deadline" && target == context.DeadlineExceeded
}
func (e *arrAPIError) Temporary() bool { return e.Retryable }
func (e *arrAPIError) Timeout() bool   { return e.Kind == "deadline" || e.Kind == "timeout" }

func safeArrAPIError(instance, operation string, err error) error {
	safe := &arrAPIError{
		Instance:  instance,
		Operation: operation,
		Kind:      "transport",
		Retryable: true,
	}
	var timeout net.Error
	var statusErr *arrapi.StatusError
	var tooLargeErr *arrapi.ResponseTooLargeError
	switch {
	case errors.As(err, &statusErr):
		safe.Kind = "status"
		safe.StatusCode = statusErr.Code
		safe.Retryable = statusErr.IsTransient()
	case errors.As(err, &tooLargeErr):
		safe.Kind = "response_too_large"
		safe.Retryable = false
	case errors.Is(err, context.Canceled):
		safe.Kind = "canceled"
		safe.Retryable = false
	case errors.Is(err, context.DeadlineExceeded):
		safe.Kind = "deadline"
	case errors.As(err, &timeout) && timeout.Timeout():
		safe.Kind = "timeout"
	}
	return safe
}
