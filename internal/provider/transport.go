package provider

import (
	"context"
	"fmt"
	"net/http"
	"strings"
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

func (c Client) Do(ctx context.Context, operation Operation, request *http.Request) (*http.Response, error) {
	origin := request.URL.Scheme + "://" + request.URL.Host
	release, err := c.Gate.Acquire(ctx, c.ProviderID, origin, operation)
	if err != nil {
		return nil, err
	}
	defer release()
	response, err := c.HTTP.Do(request.WithContext(ctx))
	if err != nil {
		return nil, fmt.Errorf("provider %s request failed: %w", c.ProviderID, err)
	}
	now := c.Clock.Now()
	window, found := ParseRateLimit(now, response.Header)
	if response.StatusCode == http.StatusTooManyRequests && !found {
		window = RateLimitWindow{Name: "fallback", Remaining: 0, ResetAt: FallbackReset(now, c.ProviderType, CooldownRateLimit), Source: "fallback"}
		found = !window.ResetAt.IsZero()
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
