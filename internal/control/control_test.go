package control_test

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"subsyncd/internal/control"
)

type handlerFunc func(ctx context.Context, command string, args json.RawMessage) (any, error)

func (f handlerFunc) HandleControl(ctx context.Context, command string, args json.RawMessage) (any, error) {
	return f(ctx, command, args)
}

func shortDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "ssd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

type running struct {
	dir    string
	cancel context.CancelFunc
	done   chan error
}

func start(t *testing.T, dir string, h control.Handler, redact func(error) string) *running {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	srv := &control.Server{Path: control.SocketPath(dir), Handler: h, Redact: redact}
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx); close(done) }()
	r := &running{dir: dir, cancel: cancel, done: done}
	t.Cleanup(func() { cancel(); <-done })
	deadline := time.Now().Add(5 * time.Second)
	for {
		c, err := control.Dial(context.Background(), dir)
		if err == nil {
			_ = c.Close()
			return r
		}
		if time.Now().After(deadline) {
			t.Fatalf("server did not start: %v", err)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func echo(_ context.Context, _ string, args json.RawMessage) (any, error) {
	var in map[string]string
	if err := json.Unmarshal(args, &in); err != nil {
		return nil, err
	}
	return in, nil
}

func call(t *testing.T, dir, command string, args, out any) error {
	t.Helper()
	c, err := control.Dial(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	return c.Call(context.Background(), command, args, out)
}

func TestServerRoundTripsRequest(t *testing.T) {
	dir := shortDir(t)
	start(t, dir, handlerFunc(echo), nil)
	var out map[string]string
	if err := call(t, dir, "echo", map[string]string{"a": "b"}, &out); err != nil {
		t.Fatal(err)
	}
	if out["a"] != "b" {
		t.Fatalf("out = %v", out)
	}
}

func TestServerSocketIsOwnerOnly(t *testing.T) {
	dir := shortDir(t)
	start(t, dir, handlerFunc(echo), nil)
	info, err := os.Stat(control.SocketPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v", info.Mode().Perm())
	}
}

func TestServerReplacesStaleSocket(t *testing.T) {
	dir := shortDir(t)
	path := control.SocketPath(dir)
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	l.(*net.UnixListener).SetUnlinkOnClose(false)
	_ = l.Close()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("stale socket missing: %v", err)
	}
	start(t, dir, handlerFunc(echo), nil)
	var out map[string]string
	if err := call(t, dir, "echo", map[string]string{"a": "b"}, &out); err != nil {
		t.Fatal(err)
	}
}

func TestServerRefusesToReplaceRegularFile(t *testing.T) {
	dir := shortDir(t)
	path := control.SocketPath(dir)
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	srv := &control.Server{Path: path, Handler: handlerFunc(echo)}
	if err := srv.Serve(context.Background()); err == nil {
		t.Fatal("Serve replaced a regular file")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("regular file was removed")
	}
}

func TestServerRemovesSocketOnShutdown(t *testing.T) {
	dir := shortDir(t)
	r := start(t, dir, handlerFunc(echo), nil)
	r.cancel()
	select {
	case err := <-r.done:
		if err != nil {
			t.Fatalf("Serve = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return")
	}
	if _, err := os.Lstat(control.SocketPath(dir)); !os.IsNotExist(err) {
		t.Fatalf("socket still exists: %v", err)
	}
}

func TestServerRejectsOversizedRequest(t *testing.T) {
	dir := shortDir(t)
	start(t, dir, handlerFunc(echo), nil)
	conn, err := net.Dial("unix", control.SocketPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	body := `{"version":1,"command":"echo","args":"` + strings.Repeat("a", control.MaxRequestBytes) + `"}`
	go func() { _, _ = conn.Write([]byte(body)) }()
	var resp control.Response
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if resp.OK || resp.Error == nil || resp.Error.Code != control.CodeUsage {
		t.Fatalf("resp = %+v", resp)
	}
}

func TestServerHidesInternalErrors(t *testing.T) {
	dir := shortDir(t)
	h := handlerFunc(func(context.Context, string, json.RawMessage) (any, error) {
		return nil, errors.New("open /data/secret: denied")
	})
	start(t, dir, h, func(error) string { return "redacted failure" })
	err := call(t, dir, "x", nil, nil)
	var ce *control.Error
	if !errors.As(err, &ce) {
		t.Fatalf("err = %v", err)
	}
	if ce.Code != control.CodeInternal || ce.Message != "redacted failure" {
		t.Fatalf("err = %+v", ce)
	}
}

func TestServerPassesThroughControlErrors(t *testing.T) {
	dir := shortDir(t)
	h := handlerFunc(func(context.Context, string, json.RawMessage) (any, error) {
		return nil, &control.Error{Code: control.CodeAmbiguousMedia, Message: "pick one", Candidates: []control.MediaMatch{{Instance: "sonarr", FileID: 7}}}
	})
	start(t, dir, h, nil)
	err := call(t, dir, "x", nil, nil)
	var ce *control.Error
	if !errors.As(err, &ce) || ce.Code != control.CodeAmbiguousMedia || len(ce.Candidates) != 1 || ce.Candidates[0].FileID != 7 {
		t.Fatalf("err = %v", err)
	}
}

func TestDialWithoutDaemon(t *testing.T) {
	dir := shortDir(t)
	if _, err := control.Dial(context.Background(), dir); !errors.Is(err, control.ErrNoDaemon) {
		t.Fatalf("missing socket: %v", err)
	}
	l, err := net.Listen("unix", control.SocketPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	l.(*net.UnixListener).SetUnlinkOnClose(false)
	_ = l.Close()
	if _, err := control.Dial(context.Background(), dir); !errors.Is(err, control.ErrNoDaemon) {
		t.Fatalf("stale socket: %v", err)
	}
}

func TestClientReportsProtocolMismatch(t *testing.T) {
	dir := shortDir(t)
	l, err := net.Listen("unix", filepath.Join(dir, control.SocketName))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		conn, err := l.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		var req control.Request
		_ = json.NewDecoder(conn).Decode(&req)
		_ = json.NewEncoder(conn).Encode(control.Response{Version: 2, OK: true})
	}()
	err = call(t, dir, "x", nil, nil)
	var ce *control.Error
	if !errors.As(err, &ce) || ce.Code != control.CodeProtocolMismatch {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(ce.Message, "protocol 1") || !strings.Contains(ce.Message, "daemon speaks 2") {
		t.Fatalf("message = %q", ce.Message)
	}
}

func TestServerCancelsHandlersOnShutdown(t *testing.T) {
	dir := shortDir(t)
	entered := make(chan struct{})
	h := handlerFunc(func(ctx context.Context, _ string, _ json.RawMessage) (any, error) {
		close(entered)
		<-ctx.Done()
		return nil, ctx.Err()
	})
	r := start(t, dir, h, nil)
	errc := make(chan error, 1)
	go func() { errc <- call(t, dir, "block", nil, nil) }()
	<-entered
	r.cancel()
	select {
	case err := <-r.done:
		if err != nil {
			t.Fatalf("Serve = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return")
	}
	err := <-errc
	var ce *control.Error
	if !errors.As(err, &ce) || ce.Code != control.CodeDaemonStopping {
		t.Fatalf("err = %v", err)
	}
}

func TestClientDisconnectDoesNotCancelHandler(t *testing.T) {
	dir := shortDir(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	result := make(chan error, 1)
	h := handlerFunc(func(ctx context.Context, _ string, _ json.RawMessage) (any, error) {
		close(entered)
		<-release
		result <- ctx.Err()
		return nil, nil
	})
	start(t, dir, h, nil)
	callCtx, cancel := context.WithCancel(context.Background())
	c, err := control.Dial(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = c.Call(callCtx, "slow", nil, nil) }()
	<-entered
	cancel()
	_ = c.Close()
	time.Sleep(50 * time.Millisecond)
	close(release)
	if err := <-result; err != nil {
		t.Fatalf("handler ctx canceled: %v", err)
	}
}

func TestDialErrorOmitsSocketPath(t *testing.T) {
	dir := shortDir(t)
	l, err := net.Listen("unix", control.SocketPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if err := os.Chmod(control.SocketPath(dir), 0); err != nil {
		t.Fatal(err)
	}
	_, err = control.Dial(context.Background(), dir)
	if err == nil || errors.Is(err, control.ErrNoDaemon) {
		t.Skipf("connect to a mode 0 socket did not fail with a permission error: %v", err)
	}
	if strings.Contains(err.Error(), dir) {
		t.Fatalf("error leaks the socket path: %v", err)
	}
	if !errors.Is(err, syscall.EACCES) {
		t.Fatalf("errno not preserved: %v", err)
	}
}

func TestServeStartupErrorOmitsSocketPath(t *testing.T) {
	dir := shortDir(t)
	for name, path := range map[string]string{
		"missing directory": filepath.Join(dir, "missing", control.SocketName),
		"regular file":      filepath.Join(dir, "file.sock"),
	} {
		if name == "regular file" {
			if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		srv := &control.Server{Path: path, Handler: handlerFunc(echo)}
		err := srv.Serve(context.Background())
		if err == nil {
			t.Fatalf("%s: Serve succeeded", name)
		}
		if strings.Contains(err.Error(), dir) {
			t.Fatalf("%s: error leaks the socket path: %v", name, err)
		}
	}
}
