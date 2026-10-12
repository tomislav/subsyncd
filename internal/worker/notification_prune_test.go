package worker

import (
	"bytes"
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"subsyncd/internal/observability"
	"subsyncd/internal/store"
	"subsyncd/internal/testutil"
)

// Each maintenance tick deletes at most notificationPruneBatch notifications
// that finished more than the retention ago, logs how many at debug, and
// leaves recent and unfinished ones.
func TestMaintenancePrunesOldFinishedNotificationsInBatches(t *testing.T) {
	ctx := context.Background()
	database, err := store.Open(ctx, filepath.Join(t.TempDir(), "subsyncd.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	repository := database.Repository()
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	finished := now.Add(-store.NotificationRetention - time.Hour)
	const old = notificationPruneBatch + 1
	for index := range old + 1 {
		if _, err := repository.EnqueueNotification(ctx, store.NotificationRequest{Notifier: "silo", DedupeKey: fmt.Sprint("finished-", index), PayloadJSON: []byte(`{}`), NextAttemptAt: finished}); err != nil {
			t.Fatal(err)
		}
	}
	leases, err := repository.LeaseDueNotifications(ctx, finished, old+1, time.Minute)
	if err != nil || len(leases) != old+1 {
		t.Fatalf("lease = %d/%v", len(leases), err)
	}
	for index, lease := range leases {
		at := finished
		if index == old {
			at = now.Add(-time.Hour) // finished recently
		}
		if err := repository.CompleteNotification(ctx, store.NotificationCompletion{JobID: lease.JobID, Result: "success", FinishedAt: at}); err != nil {
			t.Fatal(err)
		}
	}
	// Not due before the test ends: maintenance leases nothing.
	if _, err := repository.EnqueueNotification(ctx, store.NotificationRequest{Notifier: "silo", DedupeKey: "pending", PayloadJSON: []byte(`{}`), NextAttemptAt: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	w := testWorker(nil, &workerWorkflow{}, testutil.NewClock(now))
	w.Repository = repository
	var logs bytes.Buffer
	w.Events, err = observability.New(&logs, observability.Options{Level: "debug", Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []float64{notificationPruneBatch, 1} {
		logs.Reset()
		if err := w.runMaintenance(ctx, ctx); err != nil {
			t.Fatal(err)
		}
		record := findWorkerEvent(t, workerLogRecords(t, logs.String()), "notification.pruned")
		if record["removed_count"] != want || record["level"] != "debug" || record["retention_days"] != float64(30) {
			t.Fatalf("notification.pruned = %#v, want removed_count %v", record, want)
		}
	}
	logs.Reset()
	if err := w.runMaintenance(ctx, ctx); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(logs.String(), "notification.pruned") {
		t.Fatalf("logged a prune that removed nothing: %s", logs.String())
	}
	leases, err = repository.LeaseDueNotifications(ctx, now.Add(2*time.Hour), 10, time.Minute)
	if err != nil || len(leases) != 1 {
		t.Fatalf("pending notifications after pruning = %d/%v", len(leases), err)
	}
}
