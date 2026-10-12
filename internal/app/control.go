package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"time"

	"subsyncd/internal/control"
	"subsyncd/internal/observability"
)

// HandleControl executes one command the CLI handed to the running daemon.
// It holds the data-directory lock already, so the App methods it reuses take
// the no-op lock path.
func (a *App) HandleControl(ctx context.Context, command string, args json.RawMessage) (any, error) {
	started := time.Now()
	result, err := a.dispatchControl(ctx, command, args)
	events := a.Events
	if events == nil {
		events = observability.Discard()
	}
	events = events.For("control")
	if err != nil {
		code := control.CodeInternal
		message := err.Error()
		var coded *control.Error
		if errors.As(err, &coded) {
			code, message = coded.Code, coded.Message
		} else {
			message = redactedError(err, a.Config, a.redaction.Values())
		}
		events.Log(ctx, slog.LevelWarn, "control.request_failed", "control request failed",
			append([]slog.Attr{slog.String("command", controlCommandLabel(command)), slog.String("code", code)},
				events.ErrorAttrs("control", errors.New(message))...)...)
		return nil, err
	}
	events.Log(ctx, slog.LevelInfo, "control.request_completed", "control request completed",
		slog.String("command", command),
		slog.String("outcome", "success"),
		slog.Int64("duration_ms", time.Since(started).Milliseconds()),
	)
	return result, nil
}

// controlCommandLabel keeps an arbitrary client-supplied name out of the log.
func controlCommandLabel(command string) string {
	switch command {
	case "retry":
		return command
	}
	return "unknown"
}

func (a *App) dispatchControl(ctx context.Context, command string, raw json.RawMessage) (any, error) {
	switch command {
	case "retry":
		var args control.RetryArgs
		if err := decodeControlArgs(raw, &args); err != nil {
			return nil, err
		}
		if args.Provider == "" {
			return nil, &control.Error{Code: control.CodeUsage, Message: "provider is required"}
		}
		result, err := a.Retry(ctx, args.Provider)
		if err != nil {
			return nil, err
		}
		a.wakeWorker()
		return result, nil
	default:
		return nil, &control.Error{Code: control.CodeUsage, Message: "unknown command"}
	}
}

// decodeControlArgs strictly decodes a command's arguments.
func decodeControlArgs(raw json.RawMessage, into any) error {
	if len(raw) == 0 {
		return nil
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(into); err != nil {
		return &control.Error{Code: control.CodeUsage, Message: "invalid arguments"}
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return &control.Error{Code: control.CodeUsage, Message: "invalid arguments"}
	}
	return nil
}

// wakeWorker nudges the worker to re-read the queue without blocking.
func (a *App) wakeWorker() {
	select {
	case a.wake <- struct{}{}:
	default:
	}
}
