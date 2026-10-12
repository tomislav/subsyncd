package app

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"subsyncd/internal/catalog"
	"subsyncd/internal/control"
	"subsyncd/internal/domain"
	"subsyncd/internal/provider"
)

// gatedLibrary is a catalog whose library listing waits for release, so a scan
// can be held in flight.
type gatedLibrary struct {
	fakeCatalog
	started chan struct{}
	release chan struct{}
}

func (g gatedLibrary) ListLibrary(ctx context.Context) ([]domain.Media, error) {
	g.started <- struct{}{}
	select {
	case <-g.release:
		return nil, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// recordingFence stands in for the worker's reconcile fence.
type recordingFence struct {
	entered atomic.Int32
	exited  atomic.Int32
}

func (f *recordingFence) WithReconcileLock(ctx context.Context, fn func(context.Context) error) error {
	f.entered.Add(1)
	defer f.exited.Add(1)
	return fn(ctx)
}

type scanHarness struct {
	app   *App
	fence *recordingFence
	gate  gatedLibrary
	stop  func() error
}

func newScanHarness(t *testing.T) *scanHarness {
	t.Helper()
	cfg := testConfig(t)
	gate := gatedLibrary{started: make(chan struct{}, 4), release: make(chan struct{})}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	application, err := New(t.Context(), cfg, Options{
		LapseRunner: capabilityRunner{}, ProbeRunner: probeRunner{}, Worker: &waitingWorker{}, Listener: listener,
		Providers: map[string]provider.Provider{"english": fakeProvider{id: "english"}}, Catalogs: map[string]catalog.Catalog{"tv": gate},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = application.Close() })
	fence := &recordingFence{}
	application.reconcileFence = fence
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- application.Serve(ctx) }()
	h := &scanHarness{app: application, fence: fence, gate: gate}
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
	return h
}

type scanCall struct {
	result control.ScanResult
	err    error
}

func (h *scanHarness) callScan(t *testing.T, args control.ScanArgs) <-chan scanCall {
	t.Helper()
	client := dialDaemon(t, h.app)
	out := make(chan scanCall, 1)
	go func() {
		var result control.ScanResult
		err := client.Call(context.Background(), "scan", args, &result)
		out <- scanCall{result, err}
	}()
	return out
}

func awaitScan(t *testing.T, calls <-chan scanCall) scanCall {
	t.Helper()
	select {
	case call := <-calls:
		return call
	case <-time.After(5 * time.Second):
		t.Fatal("scan did not answer")
		return scanCall{}
	}
}

func TestDaemonScanReturnsSummary(t *testing.T) {
	h := newScanHarness(t)
	close(h.gate.release)
	call := awaitScan(t, h.callScan(t, control.ScanArgs{Instance: "tv"}))
	if call.err != nil || call.result.Text() != "scan complete: instance=tv media=0 force_probe=false" {
		t.Fatalf("scan = %q/%v", call.result.Text(), call.err)
	}
	if h.fence.entered.Load() != 1 || h.fence.exited.Load() != 1 {
		t.Fatalf("fence entered/exited = %d/%d, want 1/1", h.fence.entered.Load(), h.fence.exited.Load())
	}
}

func TestDaemonScanRunningAfterTimeout(t *testing.T) {
	h := newScanHarness(t)
	call := awaitScan(t, h.callScan(t, control.ScanArgs{Instance: "tv", Wait: 50 * time.Millisecond}))
	if call.err != nil || call.result.Outcome != "running" || call.result.Instance != "tv" {
		t.Fatalf("scan = %+v/%v, want running", call.result, call.err)
	}
	// The scan outlived the wait and still finishes in the daemon.
	close(h.gate.release)
	eventually(t, "the scan did not finish after the wait ended", func() bool { return h.fence.exited.Load() == 1 })
}

func TestDaemonScanNoWaitReturnsAtOnce(t *testing.T) {
	h := newScanHarness(t)
	call := awaitScan(t, h.callScan(t, control.ScanArgs{Instance: "tv", NoWait: true}))
	if call.err != nil || call.result.Outcome != "running" {
		t.Fatalf("scan = %+v/%v, want running", call.result, call.err)
	}
	close(h.gate.release)
	eventually(t, "the scan did not finish", func() bool { return h.fence.exited.Load() == 1 })
}

func TestDaemonScanUnknownInstanceCode(t *testing.T) {
	h := newScanHarness(t)
	call := awaitScan(t, h.callScan(t, control.ScanArgs{Instance: "nope"}))
	var coded *control.Error
	if !errors.As(call.err, &coded) || coded.Code != control.CodeUnknownInstance {
		t.Fatalf("scan = %+v/%v, want unknown_instance", call.result, call.err)
	}
}

func TestDaemonShutdownEndsScan(t *testing.T) {
	h := newScanHarness(t)
	calls := h.callScan(t, control.ScanArgs{Instance: "tv"})
	<-h.gate.started
	if err := h.stop(); err != nil {
		t.Fatal(err)
	}
	call := awaitScan(t, calls)
	var coded *control.Error
	if !errors.As(call.err, &coded) || coded.Code != control.CodeDaemonStopping {
		t.Fatalf("scan = %+v/%v, want daemon_stopping", call.result, call.err)
	}
}

func TestScanWithoutDaemonRefusesWaitingFlags(t *testing.T) {
	cfg := testConfig(t)
	application, err := New(t.Context(), cfg, Options{SkipLapseCheck: true, SkipProbeCheck: true, Providers: map[string]provider.Provider{"english": fakeProvider{id: "english"}}, Catalogs: map[string]catalog.Catalog{"tv": fakeCatalog{}}})
	if err != nil {
		t.Fatal(err)
	}
	defer application.Close()
	for _, args := range []control.ScanArgs{{Instance: "tv", Wait: time.Minute}, {Instance: "tv", NoWait: true}} {
		_, err := application.Scan(t.Context(), args)
		var coded *control.Error
		if !errors.As(err, &coded) || coded.Code != control.CodeDaemonRequired {
			t.Fatalf("Scan(%+v) error = %v, want daemon_required", args, err)
		}
	}
}
