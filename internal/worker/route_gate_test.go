package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"subsyncd/internal/domain"
	"subsyncd/internal/observability"
	"subsyncd/internal/store"
	"subsyncd/internal/testutil"
	"subsyncd/internal/workflow"
)

type fakeRouteGate struct {
	pauses []RoutePause
	err    error
	calls  int
}

func (g *fakeRouteGate) PausedRoutes(context.Context) ([]RoutePause, error) {
	g.calls++
	return append([]RoutePause(nil), g.pauses...), g.err
}

func routeEvents(t *testing.T, logs *bytes.Buffer) []map[string]any {
	t.Helper()
	var events []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		if line == "" {
			continue
		}
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatal(err)
		}
		if event, _ := record["event"].(string); strings.HasPrefix(event, "queue.route_") {
			events = append(events, record)
		}
	}
	return events
}

func TestRunOnceSkipsPausedRoutes(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	repository := newWorkerRepository(2, now)
	repository.searches[0].Language = "hr"
	repository.searches[1].Language = "en"
	service := &workerWorkflow{outcome: workflow.Result{Outcome: workflow.OutcomeNoResult}}
	w := testWorker(repository, service, testutil.NewClock(now))
	paused := store.RouteKey{Language: "hr", Kind: domain.MediaMovie}
	w.Routes = &fakeRouteGate{pauses: []RoutePause{{RouteKey: paused, ResetAt: now.Add(time.Hour), ProviderCount: 1}}}

	if err := w.RunOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(repository.pausedRoutes, []store.RouteKey{paused}) {
		t.Fatalf("paused routes passed to lease = %v, want %v", repository.pausedRoutes, []store.RouteKey{paused})
	}
	if len(service.requests) != 1 || service.requests[0].Language != "en" {
		t.Fatalf("workflow requests = %+v, want only the en search", service.requests)
	}
}

func TestRouteGateErrorLeasesNothing(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	repository := newWorkerRepository(1, now)
	service := &workerWorkflow{outcome: workflow.Result{Outcome: workflow.OutcomeNoResult}}
	w := testWorker(repository, service, testutil.NewClock(now))
	var logs bytes.Buffer
	var err error
	if w.Events, err = observability.New(&logs, observability.Options{Level: "info", Version: "test"}); err != nil {
		t.Fatal(err)
	}
	failure := errors.New("provider state unavailable")
	w.Routes = &fakeRouteGate{err: failure}

	if err := w.RunOnce(t.Context()); !errors.Is(err, failure) {
		t.Fatalf("RunOnce() error = %v, want %v", err, failure)
	}
	if repository.searchLeaseCalls != 0 || len(service.requests) != 0 {
		t.Fatalf("lease calls = %d, workflow requests = %d; want none", repository.searchLeaseCalls, len(service.requests))
	}
	events := routeEvents(t, &logs)
	if len(events) != 1 || events[0]["event"] != "queue.route_check_failed" || events[0]["level"] != "error" {
		t.Fatalf("route events = %v, want one queue.route_check_failed error", events)
	}
}

func TestRoutePauseAndResumeAreLoggedOncePerTransition(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	repository := newWorkerRepository(0, now)
	w := testWorker(repository, &workerWorkflow{}, testutil.NewClock(now))
	var logs bytes.Buffer
	var err error
	if w.Events, err = observability.New(&logs, observability.Options{Level: "info", Version: "test"}); err != nil {
		t.Fatal(err)
	}
	movie := RoutePause{RouteKey: store.RouteKey{Language: "hr", Kind: domain.MediaMovie}, ResetAt: now.Add(time.Hour), ProviderCount: 2}
	disabled := RoutePause{RouteKey: store.RouteKey{Language: "en", Kind: domain.MediaEpisode}, ProviderCount: 1}
	gate := &fakeRouteGate{pauses: []RoutePause{movie, disabled}}
	w.Routes = gate

	for range 2 {
		if err := w.RunOnce(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	gate.pauses = []RoutePause{disabled}
	for range 2 {
		if err := w.RunOnce(t.Context()); err != nil {
			t.Fatal(err)
		}
	}

	events := routeEvents(t, &logs)
	if len(events) != 3 {
		t.Fatalf("route events = %v, want paused, paused, resumed", events)
	}
	pausedMovie, pausedDisabled, resumed := events[0], events[1], events[2]
	if pausedMovie["event"] != "queue.route_paused" || pausedMovie["level"] != "warn" || pausedMovie["language"] != "hr" || pausedMovie["media_kind"] != "movie" || pausedMovie["provider_count"] != float64(2) || pausedMovie["reset_at"] == nil {
		t.Fatalf("movie pause event = %v", pausedMovie)
	}
	if pausedDisabled["event"] != "queue.route_paused" || pausedDisabled["reason"] != "disabled" || pausedDisabled["reset_at"] != nil {
		t.Fatalf("disabled pause event = %v", pausedDisabled)
	}
	if resumed["event"] != "queue.route_resumed" || resumed["level"] != "info" || resumed["language"] != "hr" || resumed["media_kind"] != "movie" {
		t.Fatalf("resume event = %v", resumed)
	}
}
