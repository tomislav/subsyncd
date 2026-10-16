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
	CooldownSearchQuota   CooldownKind = "search_quota"
	CooldownServiceBusy   CooldownKind = "service_busy"
	// CooldownClientBlocked is a provider refusing this client rather than
	// a limit: Titlovi's "unsupported plugin" placeholder.
	CooldownClientBlocked CooldownKind = "client_blocked"
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
	response, err := c.credentialSafeHTTP().Do(request.WithContext(ctx))
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		var redirect *RedirectError
		if errors.As(err, &redirect) {
			// Our own redirect check refused it: the provider answered.
			return nil, redirect
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
	if found && response.StatusCode >= http.StatusOK && response.StatusCode < http.StatusMultipleChoices && window.Remaining <= 0 && window.ResetAt.Sub(now) < minimumPersistedWindow {
		// Gateways report per-second windows on ordinary successful bursts.
		// The local token bucket paces those; persisting them would only
		// churn cooldowns, route pauses, and warnings.
		found = false
	}
	if found {
		throttle := Throttle{ProviderID: c.ProviderID, Scope: operation, Reason: window.Source, Limit: window.Limit, Remaining: window.Remaining, ResetAt: window.ResetAt}
		resetAt, err := c.Gate.persist(ctx, throttle)
		if err != nil {
			return response, fmt.Errorf("persist provider %s throttle: %w", c.ProviderID, err)
		}
		if response.StatusCode == http.StatusTooManyRequests {
			return response, &CooldownError{ProviderID: c.ProviderID, Scope: operation, Reason: throttle.Reason, ResetAt: resetAt}
		}
	}
	if (response.StatusCode < 200 || response.StatusCode >= 300 || response.Body == nil) && !refusalStatus(response.StatusCode) {
		if err := c.Gate.ResetTransientFailures(ctx, c.ProviderID, operation); err != nil {
			return response, err
		}
	}
	if response.StatusCode == http.StatusUnauthorized {
		return response, &AuthenticationError{Message: "HTTP 401"}
	}
	return response, nil
}

// refusalStatus reports a status that refuses the request (401, 403, 429).
// It proves the provider answered, but not that it recovered, so it keeps the
// transient-failure count: a provider that counts its own refusals as
// transient failures (SubDL's 403) escalates its backoff instead of starting
// at the first delay every time.
func refusalStatus(status int) bool {
	return status == http.StatusUnauthorized || status == http.StatusForbidden || status == http.StatusTooManyRequests
}

// credentialHeaders carry provider credentials. Go drops Authorization and
// Cookie on a redirect to another domain, but forwards every custom header,
// such as an API key, to any host a redirect names.
var credentialHeaders = []string{"Authorization", "Cookie", "Api-Key", "X-Api-Key", "X-Auth-Token"}

// credentialSafeHTTP returns the client with a redirect check that strips
// credential headers whenever a redirect leaves the original scheme and host
// (port included), before the provider's own check runs.
func (c Client) credentialSafeHTTP() *http.Client {
	client := *c.HTTP
	next := client.CheckRedirect
	client.CheckRedirect = func(request *http.Request, via []*http.Request) error {
		if len(via) > 0 && (!strings.EqualFold(request.URL.Scheme, via[0].URL.Scheme) || !strings.EqualFold(request.URL.Host, via[0].URL.Host)) {
			for _, header := range credentialHeaders {
				request.Header.Del(header)
			}
		}
		if next != nil {
			return next(request, via)
		}
		if len(via) >= 10 {
			return errors.New("stopped after 10 redirects")
		}
		return nil
	}
	return &client
}

// minimumPersistedWindow is the shortest exhausted window a successful
// response persists as a provider cooldown.
const minimumPersistedWindow = 5 * time.Second

// defaultCooldown is the cooldown of a refusal whose provider reports no
// future reset and has no compiled fallback for its kind.
const defaultCooldown = 5 * time.Minute

// PersistCooldown records a cooldown and returns the reset that applies,
// which callers report in their CooldownError or QuotaError. A reset that is
// not in the future (none, "Retry-After: 0", a past date) takes the
// provider's fallback for kind, or defaultCooldown, so a refusal never saves
// a cooldown that is already over and the provider is not asked again in a
// loop.
func (c Client) PersistCooldown(ctx context.Context, operation Operation, kind CooldownKind, resetAt time.Time) (time.Time, error) {
	now := c.Clock.Now()
	if !resetAt.After(now) {
		resetAt = FallbackReset(now, c.ProviderType, kind)
	}
	if !resetAt.After(now) {
		resetAt = now.Add(defaultCooldown)
	}
	return c.Gate.persist(ctx, Throttle{ProviderID: c.ProviderID, Scope: operation, Reason: string(kind), Remaining: 0, ResetAt: resetAt})
}

// ReportCooldownFailure logs a PersistCooldown failure the caller recovers
// from, such as a successful download whose answer also ended the quota.
func (c Client) ReportCooldownFailure(ctx context.Context, operation Operation, kind CooldownKind, err error) {
	c.Gate.reportCooldownSaveFailed(ctx, c.ProviderID, operation, kind, err)
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
		case CooldownDownloadQuota, CooldownSearchQuota:
			utc := now.UTC()
			return time.Date(utc.Year(), utc.Month(), utc.Day()+1, 0, 15, 0, 0, time.UTC)
		case CooldownServiceBusy:
			return now.Add(time.Hour)
		}
	case "subsource":
		switch kind {
		case CooldownRateLimit:
			return now.Add(time.Minute)
		case CooldownDownloadQuota:
			// The hourly and daily caps are indistinguishable; retry hourly.
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
