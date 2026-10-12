package worker

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"subsyncd/internal/catalog"
	"subsyncd/internal/domain"
	"subsyncd/internal/notifier"
	"subsyncd/internal/observability"
	"subsyncd/internal/store"
	"subsyncd/internal/testutil"
)

// fakeArr records the commands an Arr instance received and answers each
// with status.
type fakeArr struct {
	*httptest.Server
	mu       sync.Mutex
	commands []string
	status   int
}

func newFakeArr(t *testing.T, status int) *fakeArr {
	t.Helper()
	arr := &fakeArr{status: status}
	arr.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var command map[string]any
		if r.URL.Path != "/api/v3/command" || json.NewDecoder(r.Body).Decode(&command) != nil {
			t.Errorf("unexpected Arr request %s %s", r.Method, r.URL.Path)
		}
		arr.mu.Lock()
		defer arr.mu.Unlock()
		id := command["seriesId"]
		if id == nil {
			id = command["movieId"]
		}
		arr.commands = append(arr.commands, command["name"].(string)+":"+jsonNumber(id))
		w.WriteHeader(arr.status)
	}))
	t.Cleanup(arr.Close)
	return arr
}

func jsonNumber(value any) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

func (a *fakeArr) received() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	commands := append([]string(nil), a.commands...)
	sort.Strings(commands)
	return commands
}

func arrRescanNotifier(t *testing.T, instances map[string]*fakeArr) notifier.Notifier {
	t.Helper()
	clients := map[string]notifier.ArrRescanner{}
	for name, arr := range instances {
		instanceType := "sonarr"
		if strings.HasPrefix(name, "radarr") {
			instanceType = "radarr"
		}
		client, err := catalog.NewArrRescanClient(name, instanceType, arr.URL, "arr-secret", nil)
		if err != nil {
			t.Fatal(err)
		}
		clients[name] = client
	}
	return notifier.NewArrRescan(clients)
}

func rescanEpisode(seriesID, episodeID int64, folder string) domain.Media {
	return domain.Media{SeriesID: seriesID, EntityID: episodeID, Ref: domain.MediaRef{Instance: "sonarr", Kind: domain.MediaEpisode, FileID: episodeID}, Fingerprint: domain.MediaFingerprint{Path: "/media/Show/" + folder + "/e.mkv"}}
}

func rescanMovie(movieID int64) domain.Media {
	return domain.Media{EntityID: movieID, Ref: domain.MediaRef{Instance: "radarr", Kind: domain.MediaMovie, FileID: movieID}, Fingerprint: domain.MediaFingerprint{Path: "/media/Movie/m.mkv"}}
}

func workerLogs(t *testing.T) (*observability.Emitter, *bytes.Buffer) {
	t.Helper()
	var logs bytes.Buffer
	events, err := observability.New(&logs, observability.Options{Level: "info", Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	return events, &logs
}

func TestRunOnceSendsOneArrRescanPerSeriesOrMovie(t *testing.T) {
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	sonarr, radarr := newFakeArr(t, http.StatusCreated), newFakeArr(t, http.StatusCreated)
	repository := newWorkerRepository(0, now)
	lease := func(id int64, name string, media domain.Media) store.NotificationLease {
		return store.NotificationLease{ID: id, Notifier: name, PayloadJSON: notificationJSON(t, media, ""), Attempt: 0, JobID: "n-" + time.Duration(id).String()}
	}
	repository.notifications = []store.NotificationLease{
		// A season pack and an episode of another season of series 5, one
		// episode of series 6 and one movie.
		lease(1, "arr_rescan", rescanEpisode(5, 51, "Season 01")),
		lease(2, "arr_rescan", rescanEpisode(5, 52, "Season 01")),
		lease(3, "arr_rescan", rescanEpisode(5, 53, "Season 01")),
		lease(4, "arr_rescan", rescanEpisode(5, 61, "Season 02")),
		lease(5, "arr_rescan", rescanEpisode(6, 71, "Season 01")),
		lease(6, "arr_rescan", rescanMovie(9)),
		lease(7, "silo", rescanEpisode(5, 51, "Season 01")),
	}
	silo := &coalescingNotifier{}
	events, logs := workerLogs(t)
	worker := testWorker(repository, &workerWorkflow{}, testutil.NewClock(now))
	worker.Events = events
	worker.Notifiers = map[string]notifier.Notifier{"silo": silo, "arr_rescan": arrRescanNotifier(t, map[string]*fakeArr{"sonarr": sonarr, "radarr": radarr})}
	if err := worker.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(sonarr.received(), ","); got != "RescanSeries:5,RescanSeries:6" {
		t.Fatalf("Sonarr commands = %s", got)
	}
	if got := strings.Join(radarr.received(), ","); got != "RescanMovie:9" {
		t.Fatalf("Radarr commands = %s", got)
	}
	if len(silo.directories) != 1 {
		t.Fatalf("Silo scans = %q", silo.directories)
	}
	if len(repository.notificationCompletions) != 7 {
		t.Fatalf("completions = %#v", repository.notificationCompletions)
	}
	for _, completion := range repository.notificationCompletions {
		if completion.Result != "success" || !completion.NextAttemptAt.IsZero() {
			t.Fatalf("completion = %#v", completion)
		}
	}
	var seriesDelivered, movieDelivered int
	for _, record := range workerLogRecords(t, logs.String()) {
		if record["event"] != "notification.delivered" || record["notifier"] != "arr_rescan" {
			continue
		}
		if record["outcome"] != "success" {
			t.Fatalf("delivered record = %#v", record)
		}
		switch {
		case record["instance"] == "sonarr" && record["media_kind"] == "episode" && record["series_id"] != nil:
			seriesDelivered++
		case record["instance"] == "radarr" && record["media_kind"] == "movie" && record["movie_id"] == float64(9):
			movieDelivered++
		default:
			t.Fatalf("delivered record = %#v", record)
		}
	}
	if seriesDelivered != 5 || movieDelivered != 1 {
		t.Fatalf("delivered records series=%d movie=%d", seriesDelivered, movieDelivered)
	}
	if strings.Contains(logs.String(), "arr-secret") || strings.Contains(logs.String(), sonarr.URL) {
		t.Fatalf("logs leak the Arr key or URL: %s", logs.String())
	}
}

func TestRunOnceRetriesUnavailableArrAndGivesUpOnRefusal(t *testing.T) {
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		status      int
		wantResult  string
		wantRetry   bool
		wantEvent   string
		wantLevel   string
		wantOutcome string
	}{
		{status: http.StatusServiceUnavailable, wantResult: "retryable_error", wantRetry: true, wantEvent: "notification.retry_scheduled", wantLevel: "warn", wantOutcome: "retry_scheduled"},
		{status: http.StatusNotFound, wantResult: "refused", wantEvent: "notification.abandoned", wantLevel: "warn", wantOutcome: "abandoned"},
	} {
		t.Run(http.StatusText(test.status), func(t *testing.T) {
			sonarr := newFakeArr(t, test.status)
			repository := newWorkerRepository(0, now)
			repository.notifications = []store.NotificationLease{{ID: 1, Notifier: "arr_rescan", PayloadJSON: notificationJSON(t, rescanEpisode(5, 51, "Season 01"), ""), Attempt: 1, JobID: "n-1"}}
			events, logs := workerLogs(t)
			worker := testWorker(repository, &workerWorkflow{}, testutil.NewClock(now))
			worker.Events = events
			worker.Notifiers = map[string]notifier.Notifier{"arr_rescan": arrRescanNotifier(t, map[string]*fakeArr{"sonarr": sonarr})}
			if err := worker.RunOnce(context.Background()); err != nil {
				t.Fatal(err)
			}
			completion := repository.notificationCompletions[0]
			if completion.Result != test.wantResult || completion.NextAttemptAt.IsZero() == test.wantRetry {
				t.Fatalf("completion = %#v", completion)
			}
			if test.wantRetry && !completion.NextAttemptAt.Equal(now.Add(5*time.Minute)) {
				t.Fatalf("retry at %s", completion.NextAttemptAt)
			}
			record := findWorkerEvent(t, workerLogRecords(t, logs.String()), test.wantEvent)
			if record["level"] != test.wantLevel || record["outcome"] != test.wantOutcome || record["instance"] != "sonarr" || record["series_id"] != float64(5) || record["notifier"] != "arr_rescan" {
				t.Fatalf("%s = %#v", test.wantEvent, record)
			}
		})
	}
}

func TestRunOnceSkipsArrRescanForInstanceTurnedOff(t *testing.T) {
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	repository := newWorkerRepository(0, now)
	repository.notifications = []store.NotificationLease{{ID: 1, Notifier: "arr_rescan", PayloadJSON: notificationJSON(t, rescanMovie(9), ""), JobID: "n-1"}}
	events, logs := workerLogs(t)
	worker := testWorker(repository, &workerWorkflow{}, testutil.NewClock(now))
	worker.Events = events
	worker.Notifiers = map[string]notifier.Notifier{"arr_rescan": notifier.NewArrRescan(nil)}
	if err := worker.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if completion := repository.notificationCompletions[0]; completion.Result != "skipped" || !completion.NextAttemptAt.IsZero() {
		t.Fatalf("completion = %#v", completion)
	}
	if record := findWorkerEvent(t, workerLogRecords(t, logs.String()), "notification.skipped"); record["reason"] != "rescan_disabled" || record["outcome"] != "skipped" || record["level"] != "info" {
		t.Fatalf("notification.skipped = %#v", record)
	}
}

// A rescan queued before a crash, whose lease the dead process still held,
// is delivered once by the next process after the lease expires.
func TestArrRescanSurvivesRestartWithAnExpiredLease(t *testing.T) {
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "subsyncd.db")
	database, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	for index, media := range []domain.Media{rescanEpisode(5, 51, "Season 01"), rescanEpisode(5, 52, "Season 01")} {
		if _, err := database.Repository().EnqueueNotification(context.Background(), store.NotificationRequest{Notifier: "arr_rescan", DedupeKey: "key-" + time.Duration(index).String(), PayloadJSON: notificationJSON(t, media, ""), NextAttemptAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	if leased, err := database.Repository().LeaseDueNotifications(context.Background(), now, 10, 5*time.Minute); err != nil || len(leased) != 2 {
		t.Fatalf("crashed lease = %d/%v", len(leased), err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	database, err = store.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	sonarr := newFakeArr(t, http.StatusCreated)
	clock := testutil.NewClock(now.Add(time.Minute))
	worker := &Worker{Repository: database.Repository(), Workflow: &workerWorkflow{}, Clock: clock, LeaseDuration: 5 * time.Minute, RenewInterval: time.Minute, Notifiers: map[string]notifier.Notifier{"arr_rescan": arrRescanNotifier(t, map[string]*fakeArr{"sonarr": sonarr})}}
	if err := worker.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := sonarr.received(); len(got) != 0 {
		t.Fatalf("delivered under a live lease: %q", got)
	}
	clock.Set(now.Add(6 * time.Minute))
	if err := worker.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(sonarr.received(), ","); got != "RescanSeries:5" {
		t.Fatalf("Sonarr commands after restart = %s", got)
	}
	audit, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer audit.Close()
	var pending, delivered int
	if err := audit.QueryRow(`SELECT count(*) FILTER (WHERE next_attempt_at_ns > 0), count(*) FILTER (WHERE result='success') FROM notifications`).Scan(&pending, &delivered); err != nil {
		t.Fatal(err)
	}
	if pending != 0 || delivered != 2 {
		t.Fatalf("pending=%d delivered=%d", pending, delivered)
	}
}
