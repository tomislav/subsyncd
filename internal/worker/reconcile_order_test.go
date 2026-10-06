package worker

import (
	"context"
	"slices"
	"testing"
	"time"

	"subsyncd/internal/testutil"
)

type orderReconciler struct {
	name  string
	order *[]string
}

func (r orderReconciler) Run(context.Context) error {
	*r.order = append(*r.order, r.name)
	return nil
}

func TestReconcileRunsInstancesByQueuePriorityThenName(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	w := testWorker(newWorkerRepository(0, now), &workerWorkflow{}, testutil.NewClock(now))
	var order []string
	w.Reconcilers = map[string]Reconciler{}
	for _, name := range []string{"radarr-lq", "radarr-main", "sonarr-lq", "sonarr-main", "unranked"} {
		w.Reconcilers[name] = orderReconciler{name: name, order: &order}
	}
	// radarr-lq and unranked tie at 0 and keep alphabetical order.
	w.ReconcilePriorities = map[string]int{"radarr-main": 30, "sonarr-main": 20, "sonarr-lq": 10}

	if err := w.RunOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	want := []string{"radarr-main", "sonarr-main", "sonarr-lq", "radarr-lq", "unranked"}
	if !slices.Equal(order, want) {
		t.Fatalf("reconcile order = %v, want %v", order, want)
	}
}
