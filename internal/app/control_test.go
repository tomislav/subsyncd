package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"subsyncd/internal/catalog"
	"subsyncd/internal/cli"
	"subsyncd/internal/control"
	"subsyncd/internal/observability"
	"subsyncd/internal/provider"
	"subsyncd/internal/store"
)

// serveWithControl starts Serve on a fresh application whose data directory
// is short enough for a Unix socket path.
func serveWithControl(t *testing.T) (*App, *bytes.Buffer, func() error) {
	t.Helper()
	cfg := testConfig(t)
	var logs bytes.Buffer
	events, err := observability.New(&logs, observability.Options{Level: "info", Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	application, err := New(t.Context(), cfg, Options{
		LapseRunner: capabilityRunner{}, ProbeRunner: probeRunner{}, Worker: &waitingWorker{}, Listener: listener, Events: events,
		Providers: map[string]provider.Provider{"english": fakeProvider{id: "english"}}, Catalogs: map[string]catalog.Catalog{"tv": fakeCatalog{}},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = application.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- application.Serve(ctx) }()
	stop := func() error {
		cancel()
		select {
		case err := <-done:
			return err
		case <-time.After(5 * time.Second):
			t.Fatal("daemon did not stop")
			return nil
		}
	}
	t.Cleanup(func() { cancel() })
	return application, &logs, stop
}

func dialDaemon(t *testing.T, application *App) *control.Client {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		client, err := control.Dial(t.Context(), application.Config.DataDir)
		if err == nil {
			t.Cleanup(func() { _ = client.Close() })
			return client
		}
		if !errors.Is(err, control.ErrNoDaemon) || time.Now().After(deadline) {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestServeListensOnControlSocket(t *testing.T) {
	application, logs, stop := serveWithControl(t)
	ctx := t.Context()
	if err := application.Repository.PutProviderState(ctx, store.ProviderState{ProviderID: "english", Scope: "search", Reason: "throttled", ResetAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	client := dialDaemon(t, application)
	var result control.RetryResult
	if err := client.Call(ctx, "retry", control.RetryArgs{Provider: "english"}, &result); err != nil {
		t.Fatal(err)
	}
	if result.Provider != "english" {
		t.Fatalf("result = %+v", result)
	}
	states, err := application.Repository.ListProviderStates(ctx, "english")
	if err != nil || len(states) != 0 {
		t.Fatalf("provider states = %v, err=%v", states, err)
	}
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(control.SocketPath(application.Config.DataDir)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("socket after shutdown: %v", err)
	}
	text := logs.String()
	if !strings.Contains(text, `"event":"control.request_completed"`) || !strings.Contains(text, `"command":"retry"`) {
		t.Fatalf("logs = %s", text)
	}
	if strings.Contains(text, "subsyncd.sock") || strings.Contains(text, application.Config.DataDir) {
		t.Fatalf("logs leak a path: %s", text)
	}
}

func TestRetryThroughDaemonWakesWorker(t *testing.T) {
	application, _, _ := serveWithControl(t)
	client := dialDaemon(t, application)
	if err := client.Call(t.Context(), "retry", control.RetryArgs{Provider: "english"}, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case <-application.wake:
	default:
		t.Fatal("worker wake channel holds no signal")
	}
}

func TestControlRejectsBadRequests(t *testing.T) {
	application, logs, _ := serveWithControl(t)
	for _, test := range []struct {
		name, command, args, code string
	}{
		{"unknown command", "bogus", `{}`, control.CodeUsage},
		{"unknown field", "retry", `{"provider":"english","extra":1}`, control.CodeUsage},
		{"not an object", "retry", `"english"`, control.CodeUsage},
		{"unknown provider", "retry", `{"provider":"nope"}`, control.CodeUnknownProvider},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := dialDaemon(t, application)
			err := client.Call(t.Context(), test.command, json.RawMessage(test.args), nil)
			var coded *control.Error
			if !errors.As(err, &coded) || coded.Code != test.code {
				t.Fatalf("err = %v, want code %s", err, test.code)
			}
		})
	}
	if text := logs.String(); !strings.Contains(text, `"event":"control.request_failed"`) || !strings.Contains(text, `"code":"usage"`) {
		t.Fatalf("logs = %s", text)
	}
}

func TestStopFirstCommandsStillRequireStop(t *testing.T) {
	application, _, _ := serveWithControl(t)
	_ = dialDaemon(t, application)
	var stdout bytes.Buffer
	command := cli.Command{Stdout: &stdout, Stderr: &bytes.Buffer{}, Open: func(ctx context.Context, _, _ string) (cli.Backend, error) {
		return New(ctx, application.Config, Options{LapseRunner: capabilityRunner{}, ProbeRunner: probeRunner{}, Catalogs: map[string]catalog.Catalog{"tv": fakeCatalog{}}})
	}}
	code := command.Run(t.Context(), []string{"rotate-webhook-token", "--instance", "tv", "--json"})
	var envelope struct {
		Error struct{ Code, Message string }
	}
	if err := json.Unmarshal(stdout.Bytes(), &envelope); err != nil {
		t.Fatalf("stdout = %q: %v", stdout.String(), err)
	}
	if code != cli.ExitFailure || envelope.Error.Code != control.CodeBusy || !strings.Contains(envelope.Error.Message, "stop the daemon first") {
		t.Fatalf("exit/envelope = %d/%+v", code, envelope)
	}
}
