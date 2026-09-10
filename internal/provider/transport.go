package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

type CooldownKind string

const (
	CooldownRateLimit     CooldownKind = "rate_limit"
	CooldownDownloadQuota CooldownKind = "download_quota"
	CooldownServiceBusy   CooldownKind = "service_busy"
)

type Client struct {
	HTTP         *http.Client
	Gate         *Gate
	Clock        Clock
	ProviderID   string
	ProviderType string
}

// permitBody owns the request permits until streaming completes or the caller
// closes the response. EOF does not replace the caller's obligation to Close.
type permitBody struct {
	body       io.ReadCloser
	release    func()
	once       sync.Once
	finishOnce sync.Once
	onReadEnd  func(error) error
}

func (b *permitBody) Read(payload []byte) (int, error) {
	n, err := b.body.Read(payload)
	reachedEOF := err == io.EOF
	if err != nil {
		b.finishOnce.Do(func() {
			if b.onReadEnd != nil {
				err = b.onReadEnd(err)
			}
		})
		if reachedEOF {
			b.once.Do(b.release)
		}
	}
	return n, err
}

func (b *permitBody) Close() error {
	err := b.body.Close()
	b.once.Do(b.release)
	return err
}

func (c Client) Do(ctx context.Context, operation Operation, request *http.Request) (*http.Response, error) {
	origin := request.URL.Scheme + "://" + request.URL.Host
	release, err := c.Gate.Acquire(ctx, c.ProviderID, origin, operation)
	if err != nil {
		return nil, err
	}
	releaseNow := true
	defer func() {
		if releaseNow {
			release()
		}
	}()
	response, err := c.HTTP.Do(request.WithContext(ctx))
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		state, persistErr := c.Gate.RecordTransientFailure(ctx, c.ProviderID, operation, "network_error", time.Time{})
		if persistErr != nil {
			return nil, persistErr
		}
		return nil, &CooldownError{ProviderID: c.ProviderID, Scope: operation, Reason: state.Reason, ResetAt: state.ResetAt}
	}
	if response.Body != nil {
		body := &permitBody{body: response.Body, release: release}
		if response.StatusCode >= 200 && response.StatusCode < 300 {
			body.onReadEnd = func(readErr error) error {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				if readErr == io.EOF {
					if err := c.Gate.ResetTransientFailures(ctx, c.ProviderID, operation); err != nil {
						return err
					}
					return readErr
				}
				var networkError net.Error
				if !errors.Is(readErr, io.ErrUnexpectedEOF) && !errors.As(readErr, &networkError) {
					return readErr
				}
				if errors.Is(readErr, context.Canceled) {
					return readErr
				}
				state, err := c.Gate.RecordTransientFailure(ctx, c.ProviderID, operation, "network_error", time.Time{})
				if err != nil {
					return &bodyReadError{cause: readErr, stateError: err}
				}
				return &bodyReadError{cause: readErr, stateError: &CooldownError{ProviderID: c.ProviderID, Scope: operation, Reason: state.Reason, ResetAt: state.ResetAt}}
			}
		}
		response.Body = body
		releaseNow = false
	}
	now := c.Clock.Now()
	rateHeaders := response.Header
	if response.StatusCode >= http.StatusOK && response.StatusCode < http.StatusMultipleChoices && response.Header.Get("Retry-After") != "" {
		rateHeaders = response.Header.Clone()
		rateHeaders.Del("Retry-After")
	}
	window, found := ParseRateLimit(now, rateHeaders)
	if response.StatusCode >= http.StatusInternalServerError && response.StatusCode <= 599 {
		var resetAt time.Time
		if found && window.Remaining <= 0 {
			resetAt = window.ResetAt
		}
		state, persistErr := c.Gate.RecordTransientFailure(ctx, c.ProviderID, operation, fmt.Sprintf("http_%d", response.StatusCode), resetAt)
		if persistErr != nil {
			return response, persistErr
		}
		return response, &CooldownError{ProviderID: c.ProviderID, Scope: operation, Reason: state.Reason, ResetAt: state.ResetAt}
	}
	if response.StatusCode == http.StatusTooManyRequests && (!found || !window.ResetAt.After(now)) {
		window = RateLimitWindow{Name: "fallback", Remaining: 0, ResetAt: FallbackReset(now, c.ProviderType, CooldownRateLimit), Source: "fallback"}
		found = !window.ResetAt.IsZero()
	}
	if response.StatusCode == http.StatusTooManyRequests && found {
		// The status establishes exhaustion even if headers describe a different
		// quota window that still has capacity.
		window.Remaining = 0
	}
	if found {
		throttle := Throttle{ProviderID: c.ProviderID, Scope: operation, Reason: window.Source, Limit: window.Limit, Remaining: window.Remaining, ResetAt: window.ResetAt}
		if err := c.Gate.Persist(ctx, throttle); err != nil {
			return response, fmt.Errorf("persist provider %s throttle: %w", c.ProviderID, err)
		}
		if response.StatusCode == http.StatusTooManyRequests {
			return response, &CooldownError{ProviderID: c.ProviderID, Scope: operation, Reason: throttle.Reason, ResetAt: throttle.ResetAt}
		}
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 || response.Body == nil {
		if err := c.Gate.ResetTransientFailures(ctx, c.ProviderID, operation); err != nil {
			return response, err
		}
	}
	if response.StatusCode == http.StatusUnauthorized {
		return response, &AuthenticationError{Message: "HTTP 401"}
	}
	return response, nil
}

func (c Client) PersistCooldown(ctx context.Context, operation Operation, kind CooldownKind, resetAt time.Time) error {
	if resetAt.IsZero() {
		resetAt = FallbackReset(c.Clock.Now(), c.ProviderType, kind)
	}
	return c.Gate.Persist(ctx, Throttle{ProviderID: c.ProviderID, Scope: operation, Reason: string(kind), Remaining: 0, ResetAt: resetAt})
}

func (c Client) DisableAuthentication(ctx context.Context, reason string) error {
	return c.Gate.Persist(ctx, Throttle{ProviderID: c.ProviderID, Scope: OperationAuth, Reason: reason, Remaining: 0, Disabled: true})
}

func FallbackReset(now time.Time, providerType string, kind CooldownKind) time.Time {
	switch strings.ToLower(providerType) {
	case "titlovi":
		if kind == CooldownRateLimit {
			return now.Add(5 * time.Minute)
		}
	case "opensubtitles", "opensubtitlescom":
		switch kind {
		case CooldownRateLimit:
			return now.Add(time.Minute)
		case CooldownDownloadQuota:
			return now.Add(6 * time.Hour)
		}
	case "subdl":
		switch kind {
		case CooldownRateLimit:
			return now.Add(15 * time.Minute)
		case CooldownDownloadQuota:
			utc := now.UTC()
			return time.Date(utc.Year(), utc.Month(), utc.Day()+1, 0, 15, 0, 0, time.UTC)
		case CooldownServiceBusy:
			return now.Add(time.Hour)
		}
	}
	return time.Time{}
}

// DecodeJSON reads the bounded response through EOF so transport completion and
// body-read failures remain visible before JSON validation.
func DecodeJSON(reader io.Reader, destination any, limit int64, message string) error {
	data, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return err
	}
	if int64(len(data)) > limit {
		return &InvalidPayloadError{Message: message}
	}
	if err := json.Unmarshal(data, destination); err != nil {
		return &InvalidPayloadError{Message: message}
	}
	return nil
}

type bodyReadError struct{ cause, stateError error }

func (e *bodyReadError) Error() string   { return "provider response body read failed" }
func (e *bodyReadError) Unwrap() []error { return []error{e.cause, e.stateError} }
