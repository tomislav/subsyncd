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

	"subsyncd/internal/catalog"
)

const defaultMaxBodyBytes int64 = 1 << 20

var ErrInvalidWebhook = catalog.ErrInvalidWebhook

type WebhookHandler interface {
	Handle(context.Context, []byte) error
}

type Instance struct {
	Token   string
	Handler WebhookHandler
}

type Server struct {
	Instances    map[string]Instance
	Ready        func(context.Context) error
	Logger       *slog.Logger
	MaxBodyBytes int64
}

func (s Server) Handler() http.Handler {
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
			s.log(request, slog.LevelWarn, "readiness check failed", "status", http.StatusServiceUnavailable)
			writeText(response, http.StatusServiceUnavailable, "not ready")
			return
		}
	}
	writeText(response, http.StatusOK, "ready")
}

func (s Server) webhook(response http.ResponseWriter, request *http.Request) {
	name := request.PathValue("instance")
	instance, exists := s.Instances[name]
	if !exists || instance.Handler == nil {
		writeText(response, http.StatusNotFound, "not found")
		return
	}
	tokens := request.URL.Query()["token"]
	if len(tokens) != 1 || !sameSecret(tokens[0], instance.Token) {
		s.log(request, slog.LevelWarn, "webhook authentication failed", "instance", name, "status", http.StatusUnauthorized)
		writeText(response, http.StatusUnauthorized, "unauthorized")
		return
	}

	limit := s.MaxBodyBytes
	if limit <= 0 {
		limit = defaultMaxBodyBytes
	}
	body, err := io.ReadAll(io.LimitReader(request.Body, limit+1))
	if err != nil {
		writeText(response, http.StatusBadRequest, "invalid request body")
		return
	}
	if int64(len(body)) > limit {
		writeText(response, http.StatusRequestEntityTooLarge, "request body too large")
		return
	}
	if err := validateJSONObject(body); err != nil {
		writeText(response, http.StatusBadRequest, "invalid JSON object")
		return
	}

	err = instance.Handler.Handle(request.Context(), body)
	switch {
	case err == nil, errors.Is(err, catalog.ErrIgnoredEvent):
		response.WriteHeader(http.StatusNoContent)
	case errors.Is(err, catalog.ErrInvalidWebhook):
		s.log(request, slog.LevelWarn, "webhook rejected", "instance", name, "status", http.StatusBadRequest)
		writeText(response, http.StatusBadRequest, "invalid webhook")
	default:
		s.log(request, slog.LevelError, "webhook processing failed", "instance", name, "status", http.StatusServiceUnavailable)
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

func (s Server) log(request *http.Request, level slog.Level, message string, attributes ...any) {
	if s.Logger == nil {
		return
	}
	base := []any{"request_id", request.Context().Value(requestIDKey{}), "method", request.Method, "path", request.URL.Path}
	s.Logger.Log(request.Context(), level, message, append(base, attributes...)...)
}

func writeText(response http.ResponseWriter, status int, body string) {
	response.Header().Set("Content-Type", "text/plain; charset=utf-8")
	response.WriteHeader(status)
	_, _ = io.WriteString(response, body+"\n")
}
