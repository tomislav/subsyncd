package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"subsyncd/internal/cli"
)

func TestRunWithoutCommandReturnsUsageExit(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), nil, &stdout, &stderr)
	if code != cli.ExitUsage || !strings.Contains(stderr.String(), "command is required") {
		t.Fatalf("exit/stderr = %d/%q", code, stderr.String())
	}
}
