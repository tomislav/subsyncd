package worker

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"subsyncd/internal/domain"
	"subsyncd/internal/notifier"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
	"subsyncd/internal/store"
	"subsyncd/internal/testutil"
	"subsyncd/internal/workflow"
)

func TestCanceledRunDoesNotDispatch(t *testing.T) {
	for _, once := range []bool{false, true} {
		t.Run(map[bool]string{false: "daemon", true: "once"}[once], func(t *testing.T) {
			repo := newWorkerRepository(1, time.Now())
			service := &workerWorkflow{outcome: workflow.Result{Outcome: workflow.OutcomeSatisfied}}
			w := testWorker(repo, service, testutil.NewClock(time.Now()))
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if once {
				_ = w.RunOnce(ctx)
			} else {
				_ = w.Run(ctx)
			}
			if len(repo.searches) != 1 || service.calls != 0 {
				t.Fatalf("canceled dispatch consumed searches: remaining=%d calls=%d", len(repo.searches), service.calls)
			}
		})
	}
}

type cancelingClaimRepository struct {
	*workerRepository
	cancel         context.CancelFunc
	parent         context.Context
	receivedParent bool
}

func (r *cancelingClaimRepository) LeaseDueSearches(ctx context.Context, now time.Time, limit int, duration time.Duration) ([]store.SearchLease, error) {
	r.receivedParent = ctx == r.parent
	leases, err := r.workerRepository.LeaseDueSearches(ctx, now, limit, duration)
	r.cancel()
	return leases, err
}
func TestCanceledReturnedLeaseIsAbandoned(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	repo := &cancelingClaimRepository{workerRepository: newWorkerRepository(1, time.Now()), cancel: cancel, parent: ctx}
	service := &workerWorkflow{outcome: workflow.Result{Outcome: workflow.OutcomeSatisfied}}
	w := testWorker(repo.workerRepository, service, testutil.NewClock(time.Now()))
	w.Repository = repo
	_ = w.Run(ctx)
	if !repo.receivedParent || service.calls != 0 || len(repo.searchCompletions) != 0 {
		t.Fatalf("parent=%v workflow calls=%d completions=%d", repo.receivedParent, service.calls, len(repo.searchCompletions))
	}
}

func TestComposeAllowsDefaultWorkerShutdownBudget(t *testing.T) {
	payload, err := os.ReadFile("../../compose.example.yml")
	if err != nil {
		t.Fatal(err)
	}
	var config struct {
		Services map[string]struct {
			StopGracePeriod string `yaml:"stop_grace_period"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal(payload, &config); err != nil {
		t.Fatal(err)
	}
	grace, err := time.ParseDuration(config.Services["subsyncd"].StopGracePeriod)
	if err != nil {
		t.Fatal(err)
	}
	w := testWorker(newWorkerRepository(0, time.Now()), &workerWorkflow{}, testutil.NewClock(time.Now()))
	w.ShutdownTimeout = 0
	if err := w.prepare(); err != nil {
		t.Fatal(err)
	}
	if grace <= 2*w.ShutdownTimeout {
		t.Fatalf("Compose grace %v does not cover two worker windows (%v) plus exit margin", grace, 2*w.ShutdownTimeout)
	}
}

type cancellationReconciler struct{ cancel context.CancelFunc }

func (r cancellationReconciler) Run(context.Context) error { r.cancel(); return nil }

type maintenanceClaimRepository struct {
	*workerRepository
	claims int
}

func (r *maintenanceClaimRepository) LeaseDueNotifications(ctx context.Context, now time.Time, limit int, duration time.Duration) ([]store.NotificationLease, error) {
	r.claims++
	return r.workerRepository.LeaseDueNotifications(ctx, now, limit, duration)
}
func TestCancellationDuringReconcileStopsLaterMaintenance(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	repo := &maintenanceClaimRepository{workerRepository: newWorkerRepository(0, time.Now())}
	w := testWorker(repo.workerRepository, &workerWorkflow{}, testutil.NewClock(time.Now()))
	w.Repository = repo
	later := &workerReconciler{}
	w.Reconcilers = map[string]Reconciler{"a": cancellationReconciler{cancel: cancel}, "b": later}
	if err := w.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if repo.claims != 0 || later.calls != 0 {
		t.Fatalf("post-cancel notification claims=%d reconciliation calls=%d", repo.claims, later.calls)
	}
}

type scopedDispatchWorkflow struct {
	ids    []int64
	cancel context.CancelFunc
}

func (w *scopedDispatchWorkflow) Run(_ context.Context, request workflow.Request) (workflow.Result, error) {
	w.ids = append(w.ids, request.MediaID)
	if w.cancel != nil {
		w.cancel()
	}
	return workflow.Result{Outcome: workflow.OutcomeSatisfied}, nil
}
func TestRunAndRunOnceUseSameSQLiteClaimScope(t *testing.T) {
	for _, once := range []bool{false, true} {
		t.Run(map[bool]string{false: "daemon", true: "once"}[once], func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			database, err := store.Open(ctx, filepath.Join(t.TempDir(), "worker.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer database.Close()
			repo := database.Repository()
			now := time.Now()
			for index, route := range []struct {
				instance string
				language domain.Language
			}{{"removed", "en"}, {"tv", "hr"}, {"tv", "en"}} {
				media := domain.Media{EntityID: int64(index + 1), Ref: domain.MediaRef{Instance: route.instance, Kind: domain.MediaMovie, FileID: int64(index + 1)}, Fingerprint: domain.MediaFingerprint{FileID: int64(index + 1), Path: fmt.Sprintf("/media/%d.mkv", index), Size: 100, ModTime: now}, Title: "Movie"}
				id, _, err := repo.UpsertMedia(ctx, media)
				if err != nil {
					t.Fatal(err)
				}
				if err := repo.UpsertSearchStateWithPriority(ctx, id, route.language, now, store.SearchPriorityMissing); err != nil {
					t.Fatal(err)
				}
			}
			service := &scopedDispatchWorkflow{}
			if !once {
				service.cancel = cancel
			}
			w := &Worker{Repository: repo.WithSearchScope([]string{"tv"}, []domain.Language{"en"}), Workflow: service, Clock: testutil.NewClock(now)}
			if once {
				err = w.RunOnce(ctx)
			} else {
				err = w.Run(ctx)
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(service.ids) != 1 || service.ids[0] != 3 {
				t.Fatalf("dispatched media=%v", service.ids)
			}
			leases, err := repo.LeaseDueSearches(context.Background(), now, 10, time.Minute)
			if err != nil || len(leases) != 2 {
				t.Fatalf("preserved obsolete jobs=%+v error=%v", leases, err)
			}
		})
	}
}

type cancelingNotificationClaim struct {
	*workerRepository
	cancel         context.CancelFunc
	parent         context.Context
	receivedParent bool
}

func (r *cancelingNotificationClaim) LeaseDueNotifications(ctx context.Context, now time.Time, limit int, duration time.Duration) ([]store.NotificationLease, error) {
	r.receivedParent = ctx == r.parent
	r.cancel()
	return []store.NotificationLease{{JobID: "notification", Notifier: "silo", PayloadJSON: []byte(`{}`)}}, nil
}
func TestCanceledReturnedNotificationIsAbandoned(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	repo := &cancelingNotificationClaim{workerRepository: newWorkerRepository(0, time.Now()), parent: ctx, cancel: cancel}
	destination := &workerNotifier{}
	w := testWorker(repo.workerRepository, &workerWorkflow{}, testutil.NewClock(time.Now()))
	w.Repository = repo
	w.Notifiers = map[string]notifier.Notifier{"silo": destination}
	if err := w.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if !repo.receivedParent || destination.calls != 0 || len(repo.notificationCompletions) != 0 {
		t.Fatalf("parent=%v notifications=%d completions=%d", repo.receivedParent, destination.calls, len(repo.notificationCompletions))
	}
}
