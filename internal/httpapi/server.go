package httpapi

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"subsyncd/internal/catalog"
	"subsyncd/internal/observability"
)

const defaultMaxBodyBytes int64 = 1 << 20

var ErrInvalidWebhook = catalog.ErrInvalidWebhook

type WebhookHandler interface {
	Handle(context.Context, []byte) (catalog.WebhookResult, error)
}

type Instance struct {
	Token   string
	Handler WebhookHandler
}

type Server struct {
	Instances    map[string]Instance
	Ready        func(context.Context) error
	Events       *observability.Emitter
	MaxBodyBytes int64
	readyState   *readinessState
}

func (s Server) Handler() http.Handler {
	if s.readyState == nil {
		s.readyState = &readinessState{}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.health)
	mux.HandleFunc("GET /readyz", s.readiness)
	mux.HandleFunc("POST /webhooks/{instance}", s.webhook)
	return s.withRequestID(mux)
}

func (s Server) health(response http.ResponseWriter, _ *http.Request) {
	writeText(response, http.StatusOK, "ok")
}

func (s Server) readiness(response http.ResponseWriter, request *http.Request) {
	if s.Ready != nil {
		if err := s.Ready(request.Context()); err != nil {
			if s.readyState.transition(false) == readinessBecameUnhealthy {
				attrs := []slog.Attr{slog.Int("status", http.StatusServiceUnavailable)}
				if s.Events != nil {
					attrs = append(attrs, s.Events.ErrorAttrs("readiness", err)...)
				}
				s.log(request, slog.LevelWarn, "readiness.unhealthy", "readiness check failed", attrs...)
			}
			writeText(response, http.StatusServiceUnavailable, "not ready")
			return
		}
	}
	if s.readyState.transition(true) == readinessRecovered {
		s.log(request, slog.LevelInfo, "readiness.recovered", "readiness recovered", slog.Int("status", http.StatusOK))
	}
	writeText(response, http.StatusOK, "ready")
}

func (s Server) webhook(response http.ResponseWriter, request *http.Request) {
	started := time.Now()
	name := request.PathValue("instance")
	instance, exists := s.Instances[name]
	if !exists || instance.Handler == nil {
		s.log(request, slog.LevelWarn, "webhook.rejected", "webhook rejected",
			slog.String("instance", observability.SafeText(name)), slog.String("reason", "unknown_instance"), slog.Int("status", http.StatusNotFound), slog.Int64("duration_ms", time.Since(started).Milliseconds()))
		writeText(response, http.StatusNotFound, "not found")
		return
	}
	tokens := request.URL.Query()["token"]
	if len(tokens) != 1 || !sameSecret(tokens[0], instance.Token) {
		s.log(request, slog.LevelWarn, "webhook.rejected", "webhook authentication failed",
			slog.String("instance", name), slog.String("reason", "authentication"), slog.Int("status", http.StatusUnauthorized), slog.Int64("duration_ms", time.Since(started).Milliseconds()))
		writeText(response, http.StatusUnauthorized, "unauthorized")
		return
	}

	limit := s.MaxBodyBytes
	if limit <= 0 {
		limit = defaultMaxBodyBytes
	}
	body, err := io.ReadAll(io.LimitReader(request.Body, limit+1))
	if err != nil {
		s.log(request, slog.LevelWarn, "webhook.rejected", "webhook request body rejected",
			slog.String("instance", name), slog.String("reason", "body_read"), slog.Int("status", http.StatusBadRequest), slog.Int64("duration_ms", time.Since(started).Milliseconds()))
		writeText(response, http.StatusBadRequest, "invalid request body")
		return
	}
	if int64(len(body)) > limit {
		s.log(request, slog.LevelWarn, "webhook.rejected", "webhook request body rejected",
			slog.String("instance", name), slog.String("reason", "body_too_large"), slog.Int("status", http.StatusRequestEntityTooLarge), slog.Int64("duration_ms", time.Since(started).Milliseconds()))
		writeText(response, http.StatusRequestEntityTooLarge, "request body too large")
		return
	}
	if err := validateJSONObject(body); err != nil {
		s.log(request, slog.LevelWarn, "webhook.rejected", "webhook JSON rejected",
			slog.String("instance", name), slog.String("reason", "invalid_json"), slog.Int("status", http.StatusBadRequest), slog.Int64("duration_ms", time.Since(started).Milliseconds()))
		writeText(response, http.StatusBadRequest, "invalid JSON object")
		return
	}

	s.log(request, slog.LevelInfo, "webhook.accepted", "webhook accepted",
		slog.String("instance", name), slog.Int("body_bytes", len(body)), slog.Int64("duration_ms", time.Since(started).Milliseconds()))
	result, err := instance.Handler.Handle(request.Context(), body)
	switch {
	case err == nil:
		outcome := "applied"
		if result.AppliedCount == 0 {
			outcome = "duplicate"
		}
		s.log(request, slog.LevelInfo, "webhook.applied", "webhook processing completed",
			slog.String("instance", name), slog.String("outcome", outcome), slog.Int("event_count", result.EventCount), slog.Int("applied_count", result.AppliedCount), slog.Int("status", http.StatusNoContent), slog.Int64("duration_ms", time.Since(started).Milliseconds()))
		response.WriteHeader(http.StatusNoContent)
	case errors.Is(err, catalog.ErrIgnoredEvent):
		s.log(request, slog.LevelInfo, "webhook.applied", "webhook event ignored",
			slog.String("instance", name), slog.String("outcome", "ignored"), slog.Int("event_count", result.EventCount), slog.Int("applied_count", result.AppliedCount), slog.Int("status", http.StatusNoContent), slog.Int64("duration_ms", time.Since(started).Milliseconds()))
		response.WriteHeader(http.StatusNoContent)
	case errors.Is(err, catalog.ErrInvalidWebhook):
		s.log(request, slog.LevelWarn, "webhook.rejected", "webhook rejected",
			slog.String("instance", name), slog.String("reason", "invalid_event"), slog.Int("event_count", result.EventCount), slog.Int("applied_count", result.AppliedCount), slog.Int("status", http.StatusBadRequest), slog.Int64("duration_ms", time.Since(started).Milliseconds()))
		writeText(response, http.StatusBadRequest, "invalid webhook")
	default:
		attrs := []slog.Attr{slog.String("instance", name), slog.String("outcome", "failed"), slog.Int("event_count", result.EventCount), slog.Int("applied_count", result.AppliedCount), slog.Int("status", http.StatusServiceUnavailable), slog.Int64("duration_ms", time.Since(started).Milliseconds())}
		if s.Events != nil {
			attrs = append(attrs, s.Events.ErrorAttrs("webhook_processing", err)...)
		}
		s.log(request, slog.LevelError, "webhook.failed", "webhook processing failed", attrs...)
		writeText(response, http.StatusServiceUnavailable, "webhook processing unavailable")
	}
}

func validateJSONObject(body []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return err
	}
	if _, ok := value.(map[string]any); !ok {
		return errors.New("JSON value is not an object")
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}

func sameSecret(provided, expected string) bool {
	if expected == "" {
		return false
	}
	providedHash := sha256.Sum256([]byte(provided))
	expectedHash := sha256.Sum256([]byte(expected))
	return subtle.ConstantTimeCompare(providedHash[:], expectedHash[:]) == 1
}

func (s Server) withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		requestID := strings.TrimSpace(request.Header.Get("X-Request-ID"))
		if !validRequestID(requestID) {
			requestID = newRequestID()
		}
		response.Header().Set("X-Request-ID", requestID)
		request = request.WithContext(context.WithValue(request.Context(), requestIDKey{}, requestID))
		next.ServeHTTP(response, request)
	})
}

func validRequestID(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for _, character := range value {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || strings.ContainsRune("-_.", character) {
			continue
		}
		return false
	}
	return true
}

type requestIDKey struct{}

func newRequestID() string {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "unavailable"
	}
	return hex.EncodeToString(value[:])
}

func (s Server) log(request *http.Request, level slog.Level, event, message string, attributes ...slog.Attr) {
	if s.Events == nil {
		return
	}
	requestID, _ := request.Context().Value(requestIDKey{}).(string)
	base := []slog.Attr{slog.String("request_id", requestID), slog.String("method", request.Method), slog.String("path", request.URL.Path)}
	s.Events.For("http").Log(request.Context(), level, event, message, append(base, attributes...)...)
}

type readinessTransition int

const (
	readinessUnchanged readinessTransition = iota
	readinessBecameUnhealthy
	readinessRecovered
)

type readinessState struct {
	mu      sync.Mutex
	known   bool
	healthy bool
}

func (s *readinessState) transition(healthy bool) readinessTransition {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.known {
		s.known = true
		s.healthy = healthy
		if !healthy {
			return readinessBecameUnhealthy
		}
		return readinessUnchanged
	}
	if s.healthy == healthy {
		return readinessUnchanged
	}
	s.healthy = healthy
	if healthy {
		return readinessRecovered
	}
	return readinessBecameUnhealthy
}

func writeText(response http.ResponseWriter, status int, body string) {
	response.Header().Set("Content-Type", "text/plain; charset=utf-8")
	response.WriteHeader(status)
	_, _ = io.WriteString(response, body+"\n")
}
