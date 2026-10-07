package observability

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"strings"
)

type Options struct {
	Level      string
	Version    string
	MediaRoots []string
	Redact     func(error) string
}

type Emitter struct {
	logger    *slog.Logger
	component string
	redact    func(error) string
	roots     []resolvedRoot
}

type contextAttributesKey struct{}

var reservedFields = map[string]struct{}{
	slog.TimeKey:    {},
	slog.LevelKey:   {},
	slog.MessageKey: {},
	"event":         {},
	"service":       {},
	"version":       {},
	"component":     {},
}

func New(out io.Writer, options Options) (*Emitter, error) {
	level, err := parseLevel(options.Level)
	if err != nil {
		return nil, err
	}
	if out == nil {
		out = io.Discard
	}
	version := SafeText(options.Version)
	if version == "" {
		version = "dev"
	}
	handler := slog.NewJSONHandler(out, &slog.HandlerOptions{
		Level: level,
		ReplaceAttr: func(groups []string, attribute slog.Attr) slog.Attr {
			if len(groups) == 0 && attribute.Key == slog.LevelKey {
				attribute.Value = slog.StringValue(strings.ToLower(attribute.Value.String()))
			}
			return attribute
		},
	})
	roots := make([]resolvedRoot, 0, len(options.MediaRoots))
	for _, root := range options.MediaRoots {
		if item, ok := resolveRoot(root); ok {
			roots = append(roots, item)
		}
	}
	return &Emitter{
		logger: slog.New(handler).With("service", "subsyncd", "version", version),
		redact: options.Redact,
		roots:  roots,
	}, nil
}

func Discard() *Emitter {
	emitter, _ := New(io.Discard, Options{Level: "error", Version: "dev"})
	return emitter
}

func (e *Emitter) For(component string) *Emitter {
	if e == nil {
		return nil
	}
	copyOfEmitter := *e
	copyOfEmitter.component = SafeText(component)
	return &copyOfEmitter
}

func WithAttrs(ctx context.Context, attrs ...slog.Attr) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	combined := append([]slog.Attr(nil), attrsFromContext(ctx)...)
	combined = append(combined, attrs...)
	return context.WithValue(ctx, contextAttributesKey{}, append([]slog.Attr(nil), combined...))
}

func (e *Emitter) Log(ctx context.Context, level slog.Level, event, message string, attrs ...slog.Attr) {
	if e == nil || e.logger == nil || strings.TrimSpace(event) == "" || strings.TrimSpace(e.component) == "" {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	all := make([]slog.Attr, 0, 2+len(attrsFromContext(ctx))+len(attrs))
	all = append(all, slog.String("event", strings.TrimSpace(event)), slog.String("component", e.component))
	all = append(all, mergeDynamicAttrs(attrsFromContext(ctx), attrs)...)
	e.logger.LogAttrs(ctx, level, SafeText(message), all...)
}

func (e *Emitter) ErrorAttrs(kind string, err error) []slog.Attr {
	if err == nil {
		return nil
	}
	message := err.Error()
	if e != nil && e.redact != nil {
		message = e.redact(err)
	}
	return []slog.Attr{
		slog.String("error_kind", SafeText(kind)),
		slog.String("error", SafeText(message)),
	}
}

func parseLevel(value string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "info":
		return slog.LevelInfo, nil
	case "debug":
		return slog.LevelDebug, nil
	case "warn":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return slog.LevelInfo, fmt.Errorf("log level must be debug, info, warn, or error")
	}
}

func attrsFromContext(ctx context.Context) []slog.Attr {
	if ctx == nil {
		return nil
	}
	attrs, _ := ctx.Value(contextAttributesKey{}).([]slog.Attr)
	return attrs
}

func mergeDynamicAttrs(groups ...[]slog.Attr) []slog.Attr {
	merged := make([]slog.Attr, 0)
	positions := make(map[string]int)
	for _, group := range groups {
		for _, attribute := range group {
			attribute.Value = attribute.Value.Resolve()
			if attribute.Key == "" || attribute.Equal(slog.Attr{}) {
				continue
			}
			if _, reserved := reservedFields[attribute.Key]; reserved {
				continue
			}
			if index, exists := positions[attribute.Key]; exists {
				merged[index] = attribute
				continue
			}
			positions[attribute.Key] = len(merged)
			merged = append(merged, attribute)
		}
	}
	return merged
}

// ServerErrorLog adapts net/http's internal error logging (accept failures,
// handler panics, TLS handshakes) to one NDJSON record per call. Only the
// redacted first line is kept, so panic stack traces never reach the log.
func (e *Emitter) ServerErrorLog() *log.Logger {
	return log.New(serverErrorWriter{events: e}, "", 0)
}

type serverErrorWriter struct{ events *Emitter }

func (w serverErrorWriter) Write(payload []byte) (int, error) {
	line, _, _ := strings.Cut(string(payload), "\n")
	line = strings.TrimPrefix(strings.TrimSpace(line), "http: ")
	reason, level := "other", slog.LevelWarn
	switch {
	case strings.HasPrefix(line, "panic serving"):
		reason, level = "panic", slog.LevelError
	case strings.HasPrefix(line, "Accept error"):
		reason = "accept"
	case strings.HasPrefix(line, "TLS handshake error"):
		reason = "tls_handshake"
	}
	attrs := append([]slog.Attr{slog.String("reason", reason)}, w.events.ErrorAttrs("http_server", errors.New(line))...)
	w.events.Log(context.Background(), level, "http.server_error", "HTTP server error", attrs...)
	return len(payload), nil
}
