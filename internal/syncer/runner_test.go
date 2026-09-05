package syncer

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestOSRunnerCapsOutputAndPreservesExitCode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test helper is a POSIX shell script")
	}
	script := executableScript(t, "printf '123456789'; printf 'abcdefghi' >&2; exit 7\n")
	result, err := (OSRunner{}).Run(context.Background(), Command{Path: script, StdoutLimit: 5, StderrLimit: 4})
	if err != nil || result.ExitCode != 7 || string(result.Stdout) != "12345" || string(result.Stderr) != "abcd" || !result.StdoutTruncated || !result.StderrTruncated {
		t.Fatalf("Run() = %#v, %v", result, err)
	}
}

func TestOSRunnerOverridesInheritedEnvironment(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test helper is a POSIX shell script")
	}
	t.Setenv("LAPSE_CACHE", "/inherited")
	script := executableScript(t, "printf '%s' \"$LAPSE_CACHE\"\n")
	result, err := (OSRunner{}).Run(context.Background(), Command{Path: script, Env: []string{"LAPSE_CACHE=/configured"}})
	if err != nil || string(result.Stdout) != "/configured" {
		t.Fatalf("Run() = %#v, %v", result, err)
	}
}

func TestOSRunnerCancellationKillsProcessGroup(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("process-group implementation targets Linux and macOS")
	}
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	script := executableScript(t, "sleep 60 & echo $! > \"$1\"; wait\n")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := (OSRunner{}).Run(ctx, Command{Path: script, Args: []string{pidFile}, StdoutLimit: 1024, StderrLimit: 1024})
		done <- err
	}()
	deadline := time.Now().Add(2 * time.Second)
	var pid int
	for {
		payload, readErr := os.ReadFile(pidFile)
		if readErr == nil {
			parsed, parseErr := strconv.Atoi(strings.TrimSpace(string(payload)))
			if parseErr == nil && parsed > 0 {
				pid = parsed
				break
			}
		} else if !errors.Is(readErr, os.ErrNotExist) {
			cancel()
			t.Fatal(readErr)
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("helper did not publish a valid child PID")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run() error = %T %v", err, err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("runner did not return after cancellation")
	}
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		killErr := syscall.Kill(pid, 0)
		if errors.Is(killErr, syscall.ESRCH) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("child process %d survived cancellation", pid)
}

func executableScript(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "helper.sh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}
