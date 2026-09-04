package syncer

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type doctorRunner struct {
	execution Execution
	err       error
}

func (r doctorRunner) Run(_ context.Context, command Command) (Execution, error) {
	if len(command.Args) != 0 {
		return Execution{}, errors.New("unexpected command")
	}
	return r.execution, r.err
}

func TestCheckCapabilities(t *testing.T) {
	help := "usage: lapse MEDIA SUBTITLE [--json] [--strict] [--no-sidecar] [--output PATH] [--no-cache]"
	if err := CheckCapabilities(context.Background(), "/usr/bin/lapse", doctorRunner{execution: Execution{Stdout: []byte(help)}}); err != nil {
		t.Fatal(err)
	}
}

func TestCheckCapabilitiesRejectsMissingAndFailedFeatures(t *testing.T) {
	for _, test := range []struct {
		name string
		run  doctorRunner
		want string
	}{
		{"cannot run", doctorRunner{err: errors.New("not found")}, "run LAPSE capability check"},
		{"missing strict", doctorRunner{execution: Execution{Stdout: []byte("--json --output --no-sidecar --no-cache")}}, "--strict"},
		{"bad exit", doctorRunner{execution: Execution{ExitCode: 2}}, "exit code 2"},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := CheckCapabilities(context.Background(), "/usr/bin/lapse", test.run)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want containing %q", err, test.want)
			}
		})
	}
}
