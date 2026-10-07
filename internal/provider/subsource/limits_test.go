package subsource

import (
	"context"
	"errors"
	"io"
	"net/http"
	"testing"
	"time"

	base "subsyncd/internal/provider"
)

func TestUnauthorizedDisablesProvider(t *testing.T) {
	env := newTestEnv(t, Config{}, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"error":"API key required","message":"detail"}`)
	})
	_, err := env.client.Search(context.Background(), movieQuery())
	var authentication *base.AuthenticationError
	if !errors.As(err, &authentication) {
		t.Fatalf("err = %v; want authentication error", err)
	}
	if err := env.client.CheckSearchAvailability(context.Background()); err == nil {
		t.Fatal("provider should be disabled after HTTP 401")
	}
}

func TestForbiddenDoesNotDisableProvider(t *testing.T) {
	env := newTestEnv(t, Config{}, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	})
	if _, err := env.client.Search(context.Background(), movieQuery()); err == nil {
		t.Fatal("HTTP 403 should fail the search")
	}
	if err := env.client.CheckSearchAvailability(context.Background()); err != nil {
		t.Fatalf("HTTP 403 disabled the provider: %v", err)
	}
}

func TestTooManyRequestsWithMinuteCapacityCoolsDownWholeKey(t *testing.T) {
	env := newTestEnv(t, Config{}, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Limit", "60")
		w.Header().Set("X-RateLimit-Remaining", "41")
		w.Header().Set("X-RateLimit-Reset", time.Now().Add(30*time.Second).UTC().Format(time.RFC3339Nano))
		w.WriteHeader(http.StatusTooManyRequests)
	})
	before := time.Now()
	_, err := env.client.Search(context.Background(), movieQuery())
	var quota *base.QuotaError
	if !errors.As(err, &quota) || quota.Scope != base.OperationAll {
		t.Fatalf("err = %v; want key-wide quota error", err)
	}
	if quota.ResetAt.Before(before.Add(59*time.Minute)) || quota.ResetAt.After(time.Now().Add(61*time.Minute)) {
		t.Fatalf("reset = %s; want about one hour", quota.ResetAt)
	}
	if err := env.client.CheckDownloadAvailability(context.Background()); err == nil {
		t.Fatal("downloads should share the key-wide cooldown")
	}
}

func TestTooManyRequestsInMinuteWindowUsesHeaderReset(t *testing.T) {
	reset := time.Now().Add(30 * time.Second).UTC().Truncate(time.Millisecond)
	env := newTestEnv(t, Config{}, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Limit", "60")
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", reset.Format("2006-01-02T15:04:05.000Z"))
		w.WriteHeader(http.StatusTooManyRequests)
	})
	_, err := env.client.Search(context.Background(), movieQuery())
	var cooldown *base.CooldownError
	if !errors.As(err, &cooldown) || !cooldown.ResetAt.Equal(reset) {
		t.Fatalf("err = %v; want cooldown until %s", err, reset)
	}
	var quota *base.QuotaError
	if errors.As(err, &quota) {
		t.Fatal("a minute-window 429 must not escalate to the hourly cooldown")
	}
}

func TestTooManyRequestsWithoutWindowHeadersCoolsDownWholeKey(t *testing.T) {
	env := newTestEnv(t, Config{}, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	})
	_, err := env.client.Search(context.Background(), movieQuery())
	var quota *base.QuotaError
	if !errors.As(err, &quota) || quota.Scope != base.OperationAll || quota.ResetAt.Before(time.Now().Add(59*time.Minute)) {
		t.Fatalf("err = %v; want hour-long key-wide quota error", err)
	}
}

func TestTooManyRequestsWithRetryAfterKeepsTransportReset(t *testing.T) {
	env := newTestEnv(t, Config{}, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "120")
		w.WriteHeader(http.StatusTooManyRequests)
	})
	_, err := env.client.Search(context.Background(), movieQuery())
	var quota *base.QuotaError
	var cooldown *base.CooldownError
	if errors.As(err, &quota) || !errors.As(err, &cooldown) {
		t.Fatalf("err = %v; want transport cooldown from Retry-After", err)
	}
}
