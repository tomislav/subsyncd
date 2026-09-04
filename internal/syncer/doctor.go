package syncer

import (
	"context"
	"fmt"
	"strings"
)

func CheckCapabilities(ctx context.Context, path string, runner Runner) error {
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("LAPSE executable path is required")
	}
	if runner == nil {
		runner = OSRunner{}
	}
	execution, err := runner.Run(ctx, Command{Path: path, Args: []string{"--help"}})
	if err != nil {
		return fmt.Errorf("run LAPSE capability check: %w", err)
	}
	help := string(execution.Stdout) + "\n" + string(execution.Stderr)
	for _, capability := range []string{"--json", "--strict", "--output", "--no-sidecar", "--no-cache"} {
		if !strings.Contains(help, capability) {
			if strings.TrimSpace(help) == "" && execution.ExitCode != 0 {
				return fmt.Errorf("LAPSE capability check returned exit code %d", execution.ExitCode)
			}
			return fmt.Errorf("LAPSE does not advertise required capability %s", capability)
		}
	}
	return nil
}
