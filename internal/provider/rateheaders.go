package provider

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

type RateLimitWindow struct {
	Name      string
	Limit     int64
	Remaining int64
	ResetAt   time.Time
	Source    string
}

func ParseRateLimit(now time.Time, headers http.Header, jsonResets ...time.Time) (RateLimitWindow, bool) {
	var windows []RateLimitWindow
	policies := parsePolicies(headers.Get("RateLimit-Policy"))
	for _, entry := range splitHeaderValues(headers.Get("RateLimit")) {
		name, parameters := parseParameterized(entry)
		remaining, remainingOK := parseIntParameter(parameters, "r", "remaining")
		reset, resetOK := parseIntParameter(parameters, "t", "reset")
		if !remainingOK || !resetOK || reset <= 0 {
			continue
		}
		limit := policies[name]
		if direct, ok := parseIntParameter(parameters, "limit", "q"); ok {
			limit = direct
		}
		resetAt := now.Add(time.Duration(reset) * time.Second)
		if resetAt.After(now) {
			windows = append(windows, RateLimitWindow{Name: name, Limit: limit, Remaining: remaining, ResetAt: resetAt, Source: "ratelimit"})
		}
	}

	if remaining, err := strconv.ParseInt(strings.TrimSpace(headers.Get("X-RateLimit-Remaining")), 10, 64); err == nil {
		if resetEpoch, err := strconv.ParseInt(strings.TrimSpace(headers.Get("X-RateLimit-Reset")), 10, 64); err == nil {
			resetAt := time.Unix(resetEpoch, 0).UTC()
			if resetAt.After(now) {
				limit, _ := strconv.ParseInt(strings.TrimSpace(headers.Get("X-RateLimit-Limit")), 10, 64)
				if limit <= 0 {
					limit = max(remaining, 1)
				}
				windows = append(windows, RateLimitWindow{Name: "x-ratelimit", Limit: limit, Remaining: remaining, ResetAt: resetAt, Source: "x-ratelimit"})
			}
		}
	}

	if retryAt, ok := parseRetryAfter(now, headers.Get("Retry-After")); ok {
		windows = append(windows, RateLimitWindow{Name: "retry-after", Remaining: 0, ResetAt: retryAt, Source: "retry-after"})
	}
	for _, resetAt := range jsonResets {
		if resetAt.After(now) {
			windows = append(windows, RateLimitWindow{Name: "json", Remaining: 0, ResetAt: resetAt, Source: "json"})
		}
	}
	if len(windows) == 0 {
		return RateLimitWindow{}, false
	}
	selected := windows[0]
	for _, window := range windows[1:] {
		if moreRestrictive(window, selected) {
			selected = window
		}
	}
	return selected, true
}

func parsePolicies(raw string) map[string]int64 {
	policies := make(map[string]int64)
	for _, item := range splitHeaderValues(raw) {
		name, parameters := parseParameterized(item)
		if limit, ok := parseIntParameter(parameters, "q", "limit"); ok && limit > 0 {
			policies[name] = limit
		}
	}
	return policies
}

func splitHeaderValues(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	return strings.Split(raw, ",")
}

func parseParameterized(raw string) (string, map[string]string) {
	parts := strings.Split(raw, ";")
	name := strings.Trim(strings.TrimSpace(parts[0]), `"`)
	parameters := make(map[string]string)
	if strings.Contains(name, "=") {
		key, value, _ := strings.Cut(name, "=")
		parameters[strings.ToLower(strings.TrimSpace(key))] = strings.TrimSpace(value)
		name = "default"
	}
	for _, part := range parts[1:] {
		key, value, ok := strings.Cut(part, "=")
		if ok {
			parameters[strings.ToLower(strings.TrimSpace(key))] = strings.Trim(strings.TrimSpace(value), `"`)
		}
	}
	return name, parameters
}

func parseIntParameter(parameters map[string]string, names ...string) (int64, bool) {
	for _, name := range names {
		if raw, ok := parameters[name]; ok {
			value, err := strconv.ParseInt(raw, 10, 64)
			return value, err == nil
		}
	}
	return 0, false
}

func parseRetryAfter(now time.Time, raw string) (time.Time, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, false
	}
	if seconds, err := strconv.ParseInt(raw, 10, 64); err == nil && seconds >= 0 {
		return now.Add(time.Duration(seconds) * time.Second), true
	}
	parsed, err := http.ParseTime(raw)
	return parsed, err == nil && parsed.After(now)
}

func moreRestrictive(candidate, current RateLimitWindow) bool {
	candidateExhausted := candidate.Remaining <= 0
	currentExhausted := current.Remaining <= 0
	if candidateExhausted != currentExhausted {
		return candidateExhausted
	}
	if candidateExhausted {
		return candidate.ResetAt.After(current.ResetAt)
	}
	candidateRatio := float64(candidate.Remaining) / float64(max(candidate.Limit, 1))
	currentRatio := float64(current.Remaining) / float64(max(current.Limit, 1))
	if candidateRatio != currentRatio {
		return candidateRatio < currentRatio
	}
	return candidate.ResetAt.After(current.ResetAt)
}
