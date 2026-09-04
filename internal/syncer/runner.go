package syncer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

const (
	defaultStdoutLimit int64 = 1 << 20
	defaultStderrLimit int64 = 64 << 10
)

type Command struct {
	Path        string
	Args        []string
	Dir         string
	Env         []string
	StdoutLimit int64
	StderrLimit int64
}

type Execution struct {
	Stdout          []byte
	Stderr          []byte
	ExitCode        int
	StdoutTruncated bool
	StderrTruncated bool
}

type Runner interface {
	Run(context.Context, Command) (Execution, error)
}

type OSRunner struct{}

func (OSRunner) Run(ctx context.Context, command Command) (Execution, error) {
	if command.Path == "" {
		return Execution{}, fmt.Errorf("executable path is required")
	}
	stdoutLimit := command.StdoutLimit
	if stdoutLimit <= 0 {
		stdoutLimit = defaultStdoutLimit
	}
	stderrLimit := command.StderrLimit
	if stderrLimit <= 0 {
		stderrLimit = defaultStderrLimit
	}
	stdout := &limitedBuffer{limit: stdoutLimit}
	stderr := &limitedBuffer{limit: stderrLimit}
	process := exec.CommandContext(ctx, command.Path, command.Args...)
	process.Dir = command.Dir
	process.Env = mergeEnvironment(os.Environ(), command.Env)
	process.Stdout = stdout
	process.Stderr = stderr
	process.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	process.Cancel = func() error {
		if process.Process == nil {
			return os.ErrProcessDone
		}
		err := syscall.Kill(-process.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	process.WaitDelay = time.Second
	err := process.Run()
	result := Execution{Stdout: stdout.Bytes(), Stderr: stderr.Bytes(), ExitCode: 0, StdoutTruncated: stdout.truncated, StderrTruncated: stderr.truncated}
	if process.ProcessState != nil {
		result.ExitCode = process.ProcessState.ExitCode()
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return result, ctxErr
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return result, nil
	}
	if err != nil {
		return result, fmt.Errorf("start or wait for executable: %w", err)
	}
	return result, nil
}

func mergeEnvironment(inherited, overrides []string) []string {
	replacements := make(map[string]string, len(overrides))
	order := make([]string, 0, len(overrides))
	for _, entry := range overrides {
		if key, _, found := strings.Cut(entry, "="); found {
			if _, exists := replacements[key]; !exists {
				order = append(order, key)
			}
			replacements[key] = entry
		}
	}
	merged := make([]string, 0, len(inherited)+len(replacements))
	used := make(map[string]bool, len(replacements))
	for _, entry := range inherited {
		key, _, found := strings.Cut(entry, "=")
		if replacement, exists := replacements[key]; found && exists {
			if !used[key] {
				merged = append(merged, replacement)
				used[key] = true
			}
			continue
		}
		merged = append(merged, entry)
	}
	for _, key := range order {
		if !used[key] {
			merged = append(merged, replacements[key])
		}
	}
	return merged
}

type limitedBuffer struct {
	buffer    bytes.Buffer
	limit     int64
	truncated bool
}

func (b *limitedBuffer) Write(payload []byte) (int, error) {
	written := len(payload)
	remaining := b.limit - int64(b.buffer.Len())
	if remaining <= 0 {
		b.truncated = b.truncated || len(payload) != 0
		return written, nil
	}
	keep := len(payload)
	if int64(keep) > remaining {
		keep = int(remaining)
		b.truncated = true
	}
	_, _ = b.buffer.Write(payload[:keep])
	return written, nil
}

func (b *limitedBuffer) Bytes() []byte {
	return append([]byte(nil), b.buffer.Bytes()...)
}
