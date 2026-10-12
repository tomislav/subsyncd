package app

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"subsyncd/internal/catalog"
	"subsyncd/internal/cli"
	"subsyncd/internal/config"
	"subsyncd/internal/control"
	"subsyncd/internal/domain"
	"subsyncd/internal/observability"
	"subsyncd/internal/provider"
	"subsyncd/internal/store"
	"subsyncd/internal/workflow"
)

// gatedWorkflow stands in for the subtitle workflow: each run reports on
// started and then blocks until the test sends on release or its context ends.
type gatedWorkflow struct {
	started chan workflow.Request
	release chan struct{}
	outcome workflow.Outcome
	// result, when set, is what a finished run returns.
	result workflow.Result
	// deaf makes runs ignore cancellation, like a read on a hung mount.
	deaf bool

	mu      sync.Mutex
	running int
	peak    int
}

func newGatedWorkflow() *gatedWorkflow {
	return &gatedWorkflow{started: make(chan workflow.Request, 16), release: make(chan struct{}), outcome: workflow.OutcomeSatisfied}
}

func (w *gatedWorkflow) Run(ctx context.Context, request workflow.Request) (workflow.Result, error) {
	w.mu.Lock()
	w.running++
	w.peak = max(w.peak, w.running)
	w.mu.Unlock()
	defer func() {
		w.mu.Lock()
		w.running--
		w.mu.Unlock()
	}()
	w.started <- request
	done := ctx.Done()
	if w.deaf {
		done = nil
	}
	select {
	case <-w.release:
		if w.result.Outcome != "" {
			return w.result, nil
		}
		return workflow.Result{Outcome: w.outcome}, nil
	case <-done:
		return workflow.Result{}, ctx.Err()
	}
}

func (w *gatedWorkflow) peakRunning() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.peak
}

func (w *gatedWorkflow) awaitStart(t *testing.T) workflow.Request {
	t.Helper()
	select {
	case request := <-w.started:
		return request
	case <-time.After(5 * time.Second):
		t.Fatal("workflow did not start")
		return workflow.Request{}
	}
}

func (w *gatedWorkflow) finishOne(t *testing.T) {
	t.Helper()
	select {
	case w.release <- struct{}{}:
	case <-time.After(5 * time.Second):
		t.Fatal("no workflow waiting to finish")
	}
}

// manualEpisode is the indexed episode the manual-search tests search.
func manualEpisode(cfg config.Config, fileID int64) domain.Media {
	return domain.Media{EntityID: fileID, Ref: domain.MediaRef{Instance: "tv", Kind: domain.MediaEpisode, FileID: fileID},
		Fingerprint: domain.MediaFingerprint{Path: filepath.Join(cfg.MediaRoots[0], "Show", fmt.Sprintf("S01E%02d.mkv", fileID)), FileID: fileID, Size: 100, ModTime: time.Unix(1700000000, 0).UTC()},
		Title:       "Show", Season: 1, Episode: int(fileID)}
}

// byFileCatalog answers GetMedia with the configured media of each file ID.
type byFileCatalog struct {
	fakeCatalog
	media map[int64]domain.Media
}

func (c byFileCatalog) GetMedia(_ context.Context, ref domain.MediaRef) (domain.Media, error) {
	media, ok := c.media[ref.FileID]
	if !ok {
		return domain.Media{}, errors.New("no such file")
	}
	return media, nil
}

type manualHarness struct {
	app      *App
	workflow *gatedWorkflow
	media    map[int64]domain.Media
	ids      map[int64]int64
	logs     *syncBuffer
	stop     func() error
}

// newManualHarness builds an application with episodes 1..count indexed but
// no searches queued, its workflow replaced by a gated fake.
// With serve it also runs the daemon.
func newManualHarness(t *testing.T, count int64, serve bool, languages ...domain.Language) *manualHarness {
	t.Helper()
	cfg := testConfig(t)
	for _, language := range languages {
		cfg.Languages[language] = config.LanguageConfig{Providers: []string{"english"}}
	}
	media := map[int64]domain.Media{}
	for id := int64(1); id <= count; id++ {
		media[id] = manualEpisode(cfg, id)
	}
	logs := &syncBuffer{}
	events, err := observability.New(logs, observability.Options{Level: "info", Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	application, err := New(t.Context(), cfg, Options{
		LapseRunner: capabilityRunner{}, ProbeRunner: probeRunner{}, Worker: &waitingWorker{}, Listener: listener, Events: events,
		Providers: map[string]provider.Provider{"english": fakeProvider{id: "english"}}, Catalogs: map[string]catalog.Catalog{"tv": byFileCatalog{media: media}},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = application.Close() })
	gated := newGatedWorkflow()
	application.runWorkflow = gated.Run
	application.manualClaimPoll = 5 * time.Millisecond
	h := &manualHarness{app: application, workflow: gated, media: media, ids: map[int64]int64{}, logs: logs}
	for id, item := range media {
		mediaID, _, err := application.Repository.UpsertMedia(t.Context(), item)
		if err != nil {
			t.Fatal(err)
		}
		h.ids[id] = mediaID
	}
	if serve {
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- application.Serve(ctx) }()
		var once sync.Once
		var stopErr error
		h.stop = func() error {
			once.Do(func() {
				cancel()
				select {
				case stopErr = <-done:
				case <-time.After(5 * time.Second):
					t.Error("daemon did not stop within the shutdown budget")
				}
			})
			return stopErr
		}
		t.Cleanup(func() { _ = h.stop() })
		_ = dialDaemon(t, application)
	}
	return h
}

func episodeArgs(fileID int64, language string) control.SearchArgs {
	return control.SearchArgs{Selection: control.Selection{Instance: "tv", Kind: "episode", FileID: fileID}, Language: language}
}

type searchCall struct {
	result control.SearchResult
	err    error
}

// callSearch sends a search to the daemon in the background.
func (h *manualHarness) callSearch(t *testing.T, args control.SearchArgs) <-chan searchCall {
	t.Helper()
	client := dialDaemon(t, h.app)
	out := make(chan searchCall, 1)
	go func() {
		var result control.SearchResult
		err := client.Call(context.Background(), "search", args, &result)
		out <- searchCall{result, err}
	}()
	return out
}

func awaitCall(t *testing.T, calls <-chan searchCall) searchCall {
	t.Helper()
	select {
	case call := <-calls:
		return call
	case <-time.After(5 * time.Second):
		t.Fatal("search did not answer")
		return searchCall{}
	}
}

// queue schedules a missing search for the file and language, due in an hour.
func (h *manualHarness) queue(t *testing.T, fileID int64, language domain.Language) {
	t.Helper()
	if err := h.app.Repository.UpsertSearchStateWithPriority(t.Context(), h.ids[fileID], language, h.app.Clock.Now().Add(time.Hour), store.SearchPriorityMissing); err != nil {
		t.Fatal(err)
	}
}

// exec runs one statement against the database beside the application.
func (h *manualHarness) exec(t *testing.T, statement string, args ...any) {
	t.Helper()
	inspection, err := sql.Open("sqlite", filepath.Join(h.app.Config.DataDir, "subsyncd.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer inspection.Close()
	if _, err := inspection.Exec(statement, args...); err != nil {
		t.Fatal(err)
	}
}

// leaseOwner reads the search's lease owner; "" when unleased.
func (h *manualHarness) leaseOwner(t *testing.T, fileID int64, language string) string {
	t.Helper()
	inspection, err := sql.Open("sqlite", filepath.Join(h.app.Config.DataDir, "subsyncd.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer inspection.Close()
	var owner *string
	if err := inspection.QueryRow(`SELECT lease_owner FROM search_states WHERE media_id=? AND language=?`, h.ids[fileID], language).Scan(&owner); err != nil {
		t.Fatal(err)
	}
	if owner == nil {
		return ""
	}
	return *owner
}

// eventually polls condition until it holds or a deadline passes.
func eventually(t *testing.T, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal(what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (h *manualHarness) settled(t *testing.T, fileID int64, language string) bool {
	t.Helper()
	status, err := h.app.Repository.GetSearchStatus(t.Context(), h.ids[fileID], domain.Language(language))
	if err != nil {
		t.Fatal(err)
	}
	return status.State == "complete" && status.LastOutcome == "satisfied" && h.leaseOwner(t, fileID, language) == ""
}

func TestDaemonSearchWaitsForWorkerLease(t *testing.T) {
	h := newManualHarness(t, 1, true)
	ctx := t.Context()
	h.queue(t, 1, "en")
	leases, err := h.app.Repository.LeaseDueSearches(ctx, h.app.Clock.Now().Add(2*time.Hour), 1, time.Minute)
	if err != nil || len(leases) != 1 {
		t.Fatalf("worker lease = %+v/%v", leases, err)
	}
	calls := h.callSearch(t, episodeArgs(1, "en"))
	// Several claim polls pass while the worker holds the search.
	time.Sleep(50 * time.Millisecond)
	select {
	case request := <-h.workflow.started:
		t.Fatalf("manual search ran beside the worker: %+v", request)
	default:
	}
	if owner := h.leaseOwner(t, 1, "en"); owner != leases[0].JobID {
		t.Fatalf("lease owner = %q, want the worker's", owner)
	}
	if err := h.app.Repository.ReleaseSearchLease(ctx, leases[0].JobID); err != nil {
		t.Fatal(err)
	}
	request := h.workflow.awaitStart(t)
	if !request.Manual || request.Language != "en" || request.MediaID != h.ids[1] {
		t.Fatalf("request = %+v", request)
	}
	if owner := h.leaseOwner(t, 1, "en"); owner == "" || owner == leases[0].JobID {
		t.Fatalf("manual search runs without its own lease: %q", owner)
	}
	h.workflow.finishOne(t)
	call := awaitCall(t, calls)
	if call.err != nil || len(call.result.Languages) != 1 || call.result.Languages[0].Outcome != "satisfied" {
		t.Fatalf("search = %+v/%v", call.result, call.err)
	}
	if owner := h.leaseOwner(t, 1, "en"); owner != "" {
		t.Fatalf("manual search kept its lease: %q", owner)
	}
	// The search the worker had queued keeps its place and progress.
	if status, err := h.app.Repository.GetSearchStatus(ctx, h.ids[1], "en"); err != nil || status.State != "pending" {
		t.Fatalf("queued search after the manual run = %+v/%v", status, err)
	}
}

func TestDaemonSearchReturnsRunningAfterTimeout(t *testing.T) {
	h := newManualHarness(t, 1, true)
	args := episodeArgs(1, "en")
	args.Wait = 50 * time.Millisecond
	calls := h.callSearch(t, args)
	h.workflow.awaitStart(t)
	call := awaitCall(t, calls)
	if call.err != nil || len(call.result.Languages) != 1 || call.result.Languages[0] != (control.LanguageOutcome{Language: "en", Outcome: "running"}) {
		t.Fatalf("search = %+v/%v", call.result, call.err)
	}
	h.workflow.finishOne(t)
	eventually(t, "the search did not settle after the wait ended", func() bool { return h.settled(t, 1, "en") })
	eventually(t, "the finished search logged no completion", func() bool {
		for _, record := range decodeLogRecords(t, h.logs.String()) {
			if record["event"] == "job.completed" && record["priority"] == "manual" && record["outcome"] == "satisfied" && record["media_title"] != nil {
				return true
			}
		}
		return false
	})
	if strings.Contains(h.logs.String(), h.app.Config.MediaRoots[0]) {
		t.Fatalf("logs leak a media path: %s", h.logs.String())
	}
}

func TestDaemonSearchNoWaitReturnsAtOnce(t *testing.T) {
	h := newManualHarness(t, 1, true)
	args := episodeArgs(1, "en")
	args.NoWait = true
	call := awaitCall(t, h.callSearch(t, args))
	if call.err != nil || len(call.result.Languages) != 1 || call.result.Languages[0].Outcome != "running" {
		t.Fatalf("search = %+v/%v", call.result, call.err)
	}
	h.workflow.awaitStart(t)
	h.workflow.finishOne(t)
	eventually(t, "the search did not settle", func() bool { return h.settled(t, 1, "en") })
}

func TestDaemonSearchSurvivesClientDisconnect(t *testing.T) {
	h := newManualHarness(t, 1, true)
	conn, err := net.Dial("unix", control.SocketPath(h.app.Config.DataDir))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(episodeArgs(1, "en"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.NewEncoder(conn).Encode(control.Request{Version: control.ProtocolVersion, Command: "search", Args: raw}); err != nil {
		t.Fatal(err)
	}
	h.workflow.awaitStart(t)
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	h.workflow.finishOne(t)
	eventually(t, "the search did not finish after its client left", func() bool { return h.settled(t, 1, "en") })
}

func TestDaemonShutdownEndsManualSearch(t *testing.T) {
	h := newManualHarness(t, 1, true)
	calls := h.callSearch(t, episodeArgs(1, "en"))
	h.workflow.awaitStart(t)
	if owner := h.leaseOwner(t, 1, "en"); owner == "" {
		t.Fatal("manual search holds no lease")
	}
	if err := h.stop(); err != nil {
		t.Fatal(err)
	}
	call := awaitCall(t, calls)
	var coded *control.Error
	if !errors.As(call.err, &coded) || coded.Code != control.CodeDaemonStopping {
		t.Fatalf("search = %+v/%v, want daemon_stopping", call.result, call.err)
	}
	if owner := h.leaseOwner(t, 1, "en"); owner != "" {
		t.Fatalf("lease still held after shutdown: %q", owner)
	}
	status, err := h.app.Repository.GetSearchStatus(t.Context(), h.ids[1], "en")
	if err != nil || status.State != "pending" {
		t.Fatalf("status after shutdown = %+v/%v", status, err)
	}
}

func TestManualSearchSlotsLimitConcurrency(t *testing.T) {
	h := newManualHarness(t, 3, false)
	done := make(chan error, 3)
	for id := int64(1); id <= 3; id++ {
		go func() {
			_, err := h.app.runManualSearch(t.Context(), h.ids[id], h.media[id], "en", manualOptions{})
			done <- err
		}()
	}
	h.workflow.awaitStart(t)
	h.workflow.awaitStart(t)
	// The third waits for a slot.
	time.Sleep(50 * time.Millisecond)
	select {
	case request := <-h.workflow.started:
		t.Fatalf("a third manual search ran: %+v", request)
	default:
	}
	h.workflow.finishOne(t)
	h.workflow.awaitStart(t)
	h.workflow.finishOne(t)
	h.workflow.finishOne(t)
	for range 3 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	if peak := h.workflow.peakRunning(); peak != 2 {
		t.Fatalf("peak concurrent manual searches = %d, want 2", peak)
	}
}

func TestManualSlotsAreFIFOAndAbortOnCancel(t *testing.T) {
	slots := newManualSlots(1)
	if err := slots.acquire(t.Context()); err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(t.Context())
	abandoned := make(chan error, 1)
	go func() { abandoned <- slots.acquire(canceled) }()
	order := make(chan int, 2)
	for i := 1; i <= 2; i++ {
		eventually(t, "waiter did not queue", func() bool { return slots.waiting() == i })
		go func() {
			if err := slots.acquire(t.Context()); err == nil {
				order <- i
				slots.release()
			}
		}()
	}
	eventually(t, "waiters did not queue", func() bool { return slots.waiting() == 3 })
	cancel()
	if err := <-abandoned; !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled acquire = %v", err)
	}
	slots.release()
	if first, second := <-order, <-order; first != 1 || second != 2 {
		t.Fatalf("order = %d, %d", first, second)
	}
}

func TestSearchWithoutLanguageCoversMissingOnly(t *testing.T) {
	h := newManualHarness(t, 1, false, "hr")
	ctx := t.Context()
	media := h.media[1]
	if err := h.app.Repository.RecordInstallation(ctx, store.Installation{MediaID: h.ids[1], Language: "en", Path: filepath.Join(h.app.Config.MediaRoots[0], "Show", "S01E01.en.srt"), Checksum: "c", ProviderID: "english", CandidateID: "x", ScoreJSON: []byte(`{}`), SyncResultJSON: []byte(`{}`), MediaPath: media.Fingerprint.Path, MediaFileID: 1, MediaSize: 100, MediaModTimeNS: media.Fingerprint.ModTime.UnixNano()}); err != nil {
		t.Fatal(err)
	}
	results := make(chan searchCall, 1)
	go func() {
		result, err := h.app.Search(ctx, episodeArgs(1, ""))
		results <- searchCall{result, err}
	}()
	if request := h.workflow.awaitStart(t); request.Language != "hr" {
		t.Fatalf("searched %s", request.Language)
	}
	h.workflow.finishOne(t)
	call := awaitCall(t, results)
	if call.err != nil || len(call.result.Languages) != 1 || call.result.Languages[0].Language != "hr" || call.result.Languages[0].Outcome != "satisfied" {
		t.Fatalf("search = %+v/%v", call.result, call.err)
	}
	// Nothing missing: nothing runs, and every language reports satisfied.
	result, err := h.app.Search(ctx, episodeArgs(1, ""))
	if err != nil || len(result.Languages) != 2 || result.Languages[0] != (control.LanguageOutcome{Language: "en", Outcome: "satisfied"}) || result.Languages[1] != (control.LanguageOutcome{Language: "hr", Outcome: "satisfied"}) {
		t.Fatalf("search with nothing missing = %+v/%v", result, err)
	}
	select {
	case request := <-h.workflow.started:
		t.Fatalf("searched with nothing missing: %+v", request)
	default:
	}
}

func TestInProcessSearchRejectsTimeout(t *testing.T) {
	h := newManualHarness(t, 1, false)
	for _, args := range []control.SearchArgs{{Wait: time.Minute}, {NoWait: true}} {
		args.Selection, args.Language = episodeArgs(1, "en").Selection, "en"
		_, err := h.app.Search(t.Context(), args)
		var coded *control.Error
		if !errors.As(err, &coded) || coded.Code != control.CodeDaemonRequired {
			t.Fatalf("Search(%+v) err = %v, want daemon_required", args, err)
		}
	}
}

func TestSearchByTitleThroughDaemon(t *testing.T) {
	h := newManualHarness(t, 2, true)
	var stdout bytes.Buffer
	command := cli.Command{Stdout: &stdout, Stderr: &bytes.Buffer{},
		Open: func(context.Context, string, string) (cli.Backend, error) {
			t.Error("the search opened its own backend beside the daemon")
			return nil, errors.New("not opened")
		},
		Dial: func(ctx context.Context, _ string) (cli.Remote, error) {
			return control.Dial(ctx, h.app.Config.DataDir)
		},
	}
	codes := make(chan int, 1)
	go func() {
		codes <- command.Run(context.Background(), []string{"search", "--title", "Show S01E02", "--language", "en", "--json"})
	}()
	if request := h.workflow.awaitStart(t); request.MediaID != h.ids[2] || !request.Manual {
		t.Fatalf("request = %+v", request)
	}
	h.workflow.finishOne(t)
	if code := <-codes; code != cli.ExitOK {
		t.Fatalf("exit = %d, stdout = %s", code, stdout.String())
	}
	var envelope struct {
		OK     bool
		Result control.SearchResult
	}
	if err := json.Unmarshal(stdout.Bytes(), &envelope); err != nil || !envelope.OK || envelope.Result.MediaID != h.ids[2] || len(envelope.Result.Languages) != 1 || envelope.Result.Languages[0].Outcome != "satisfied" {
		t.Fatalf("stdout = %s (%v)", stdout.String(), err)
	}
	if _, err := os.Stat(control.SocketPath(h.app.Config.DataDir)); err != nil {
		t.Fatalf("socket: %v", err)
	}
}

func TestManualSearchPreservesQueuedUpgradeProgress(t *testing.T) {
	h := newManualHarness(t, 1, false)
	h.queue(t, 1, "en")
	h.exec(t, `UPDATE search_states SET priority=?, attempt=3, failure_attempt=1 WHERE media_id=?`, store.SearchPriorityUpgrade, h.ids[1])
	results := make(chan searchCall, 1)
	go func() {
		result, err := h.app.Search(t.Context(), episodeArgs(1, "en"))
		results <- searchCall{result, err}
	}()
	h.workflow.awaitStart(t)
	h.workflow.finishOne(t)
	if call := awaitCall(t, results); call.err != nil || call.result.Languages[0].Outcome != "satisfied" {
		t.Fatalf("search = %+v/%v", call.result, call.err)
	}
	status, err := h.app.Repository.GetSearchStatus(t.Context(), h.ids[1], "en")
	if err != nil || status.State != "pending" || status.Priority != store.SearchPriorityUpgrade || status.Attempt != 3 || status.FailureAttempt != 1 {
		t.Fatalf("queued upgrade after a manual search = %+v/%v", status, err)
	}
	if owner := h.leaseOwner(t, 1, "en"); owner != "" {
		t.Fatalf("lease kept: %q", owner)
	}
}

func TestManualSearchAbandonsWorkflowIgnoringCancellation(t *testing.T) {
	h := newManualHarness(t, 1, false)
	h.workflow.deaf = true
	t.Cleanup(func() { close(h.workflow.release) })
	h.app.manualJobLimit = 30 * time.Millisecond
	h.app.manualJobGrace = 60 * time.Millisecond
	done := make(chan error, 1)
	go func() {
		_, err := h.app.runManualSearch(t.Context(), h.ids[1], h.media[1], "en", manualOptions{})
		done <- err
	}()
	h.workflow.awaitStart(t)
	select {
	case err := <-done:
		if !errors.Is(err, errManualTimedOut) {
			t.Fatalf("error = %v, want the job time limit", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the manual search kept waiting for a workflow that ignores cancellation")
	}
	// The timeout is recorded as a technical failure by the lease owner, so
	// the worker does not lease the search at once beside the stuck workflow.
	status, err := h.app.Repository.GetSearchStatus(t.Context(), h.ids[1], "en")
	if err != nil || status.State != "pending" || status.LastOutcome != "workflow_error" || status.FailureAttempt != 1 || !status.NextAttemptAt.After(h.app.Clock.Now()) {
		t.Fatalf("status after abandonment = %+v/%v", status, err)
	}
	if owner := h.leaseOwner(t, 1, "en"); owner != "" {
		t.Fatalf("lease kept: %q", owner)
	}
	// Both slots are free again.
	for range manualSearchSlots {
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		err := h.app.manualSlots.acquire(ctx)
		cancel()
		if err != nil {
			t.Fatalf("slot still held: %v", err)
		}
	}
	if !strings.Contains(h.logs.String(), `"event":"job.timed_out"`) {
		t.Fatalf("logs = %s", h.logs.String())
	}
}

func TestManualSearchReportsSubtitleRelativeToItsRoot(t *testing.T) {
	for _, daemon := range []bool{false, true} {
		h := newManualHarness(t, 1, daemon)
		root := h.app.Config.MediaRoots[0]
		h.workflow.result = workflow.Result{Outcome: workflow.OutcomeInstalled, Installation: store.Installation{Path: filepath.Join(root, "Show", "S01E01.en.srt")}, SyncResult: domain.SyncResult{Verdict: "solid", OffsetMS: 120}}
		var results <-chan searchCall
		if daemon {
			results = h.callSearch(t, episodeArgs(1, "en"))
		} else {
			local := make(chan searchCall, 1)
			go func() {
				result, err := h.app.Search(t.Context(), episodeArgs(1, "en"))
				local <- searchCall{result, err}
			}()
			results = local
		}
		h.workflow.awaitStart(t)
		h.workflow.finishOne(t)
		call := awaitCall(t, results)
		if call.err != nil || len(call.result.Languages) != 1 {
			t.Fatalf("daemon=%t search = %+v/%v", daemon, call.result, call.err)
		}
		got := call.result.Languages[0]
		if got.Outcome != "installed" || got.Subtitle != "Show/S01E01.en.srt" || got.SyncVerdict != "solid" || got.OffsetMS != 120 {
			t.Fatalf("daemon=%t outcome = %+v", daemon, got)
		}
		if text := call.result.Text(); strings.Contains(text, root) {
			t.Fatalf("daemon=%t output leaks the media root: %s", daemon, text)
		}
		payload, _ := json.Marshal(call.result)
		if strings.Contains(string(payload), root) {
			t.Fatalf("daemon=%t JSON leaks the media root: %s", daemon, payload)
		}
	}
}

func TestManualSearchRecordsHistory(t *testing.T) {
	h := newManualHarness(t, 1, false, "en")
	h.workflow.outcome = workflow.OutcomeSatisfied
	h.workflow.result = workflow.Result{Outcome: workflow.OutcomeInstalled, Candidate: domain.Candidate{ProviderID: "english", ResultID: "r1", ReleaseNames: []string{"Show.S01E01.1080p", "other"}}}
	go func() { h.workflow.release <- struct{}{} }()
	if _, err := h.app.runManualSearch(t.Context(), h.ids[1], h.media[1], "en", manualOptions{}); err != nil {
		t.Fatal(err)
	}
	entries, err := h.app.Repository.ListHistory(t.Context(), store.HistoryQuery{})
	if err != nil || len(entries) != 1 {
		t.Fatalf("history = %+v/%v", entries, err)
	}
	got := entries[0]
	if !got.Manual || got.Outcome != "installed" || got.ProviderID != "english" || got.ReleaseName != "Show.S01E01.1080p" || got.MediaTitle != "Show - S01E01" || got.Language != "en" || got.MediaID != h.ids[1] {
		t.Fatalf("entry = %+v", got)
	}
}

func TestManualTimeoutRecordsManualHistory(t *testing.T) {
	h := newManualHarness(t, 1, false)
	h.workflow.deaf = true
	t.Cleanup(func() { close(h.workflow.release) })
	h.app.manualJobLimit = 30 * time.Millisecond
	h.app.manualJobGrace = 60 * time.Millisecond
	go func() { _, _ = h.app.runManualSearch(t.Context(), h.ids[1], h.media[1], "en", manualOptions{}) }()
	h.workflow.awaitStart(t)
	var entries []store.HistoryEntry
	eventually(t, "timeout history was not recorded", func() bool {
		entries, _ = h.app.Repository.ListHistory(t.Context(), store.HistoryQuery{})
		return len(entries) == 1
	})
	if !entries[0].Manual || entries[0].Outcome != "workflow_error" || entries[0].MediaTitle != "Show - S01E01" {
		t.Fatalf("entry = %+v", entries[0])
	}
}

func TestHourlyMaintenancePrunesOldHistory(t *testing.T) {
	h := newManualHarness(t, 1, false)
	now := h.app.Clock.Now()
	for _, at := range []time.Time{now.Add(-31 * 24 * time.Hour), now.Add(-time.Hour)} {
		if err := h.app.Repository.RecordSearchHistory(t.Context(), h.ids[1], "en", "no_result", store.SearchHistoryDetail{MediaTitle: "Show"}, at); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	ticks := make(chan time.Time)
	done := make(chan struct{})
	go func() { defer close(done); h.app.maintainLapseCache(ctx, ticks) }()
	ticks <- now
	ticks <- now // the first sweep finished before accepting the next tick
	entries, err := h.app.Repository.ListHistory(t.Context(), store.HistoryQuery{})
	if err != nil || len(entries) != 1 {
		t.Fatalf("history after maintenance = %+v/%v", entries, err)
	}
	cancel()
	<-done
}
