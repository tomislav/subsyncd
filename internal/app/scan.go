package app

import (
	"context"
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
		var result control.ScanResult
		run := func(ctx context.Context) (err error) {
			result, err = a.runScan(ctx, args)
			return err
		}
		var err error
		if a.reconcileFence != nil {
			err = a.reconcileFence.WithReconcileLock(runCtx, run)
		} else {
			err = run(runCtx)
		}
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
