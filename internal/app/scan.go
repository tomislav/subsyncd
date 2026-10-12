package app

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"subsyncd/internal/control"
)

// scanInDaemon runs a scan on the daemon's run context, so neither the
// request nor a vanished client cancels it, and inside the worker's reconcile
// fence, so it never overlaps a scheduled reconciliation. It waits up to
// args.Wait (5 minutes when unset, none with NoWait) and reports running when
// the scan outlives that; the scan then carries on to completion.
func (a *App) scanInDaemon(args control.ScanArgs) (control.ScanResult, error) {
	wait, err := daemonWait(args.Wait, args.NoWait)
	if err != nil {
		return control.ScanResult{}, err
	}
	runCtx := a.serveCtx
	if runCtx == nil || runCtx.Err() != nil {
		return control.ScanResult{}, &control.Error{Code: control.CodeDaemonStopping, Message: "subsyncd is shutting down"}
	}
	if _, ok := a.Reconcilers[args.Instance]; !ok {
		return control.ScanResult{}, unknownInstance(args.Instance)
	}
	type finished struct {
		result control.ScanResult
		err    error
	}
	done := make(chan finished, 1)
	a.manualRuns.Add(1)
	go func() {
		defer a.manualRuns.Done()
		started := time.Now()
		result, err := a.scanWith(runCtx, args, a.reconcileFence)
		a.logScan(runCtx, args, result, err, time.Since(started))
		done <- finished{result, err}
	}()
	running := control.ScanResult{Instance: args.Instance, ForceProbe: args.ForceProbe, Outcome: "running"}
	stopping := &control.Error{Code: control.CodeDaemonStopping, Message: "subsyncd is shutting down; the scan was canceled"}
	if wait == 0 {
		return running, nil
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case item := <-done:
		if item.err != nil {
			if runCtx.Err() != nil {
				return control.ScanResult{}, stopping
			}
			return control.ScanResult{}, item.err
		}
		return item.result, nil
	case <-timer.C:
		return running, nil
	case <-runCtx.Done():
		return control.ScanResult{}, stopping
	}
}

// logScan records how a daemon scan ended, so one that outlived its request's
// wait is not lost. A scan canceled by shutdown is not a failure.
func (a *App) logScan(ctx context.Context, args control.ScanArgs, result control.ScanResult, err error, elapsed time.Duration) {
	events := a.events().For("app")
	attrs := []slog.Attr{slog.String("instance", args.Instance), slog.Bool("force_probe", args.ForceProbe), slog.Int64("duration_ms", elapsed.Milliseconds())}
	ctx = context.WithoutCancel(ctx)
	switch {
	case err == nil:
		events.Log(ctx, slog.LevelInfo, "scan.completed", "scan completed",
			append(attrs, slog.Int("media", result.Media), slog.Int("deferred", result.Deferred), slog.Int("probe_skipped", result.ProbeSkipped))...)
	case errors.Is(err, context.Canceled) && a.serveCtx != nil && a.serveCtx.Err() != nil:
		events.Log(ctx, slog.LevelInfo, "scan.failed", "scan canceled by shutdown", append(attrs, slog.String("outcome", "canceled"))...)
	default:
		events.Log(ctx, slog.LevelWarn, "scan.failed", "scan failed", append(append(attrs, slog.String("outcome", "failed")), events.ErrorAttrs("scan", err)...)...)
	}
}
