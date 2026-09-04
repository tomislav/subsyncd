package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
)

type fakeBackend struct {
	call string
	err  error
}

type redactingBackend struct{ *fakeBackend }

func (redactingBackend) Redact(error) error { return errors.New("[redacted]") }

func (f *fakeBackend) Close() error                { return nil }
func (f *fakeBackend) Serve(context.Context) error { f.call = "serve"; return f.err }
func (f *fakeBackend) Scan(_ context.Context, instance string, force bool) (string, error) {
	f.call = "scan:" + instance
	if force {
		f.call += ":force"
	}
	return "scan complete", f.err
}
func (f *fakeBackend) Search(_ context.Context, instance, kind string, fileID int64, language string) (string, error) {
	f.call = "search:" + instance + ":" + kind + ":" + language
	return "outcome: installed", f.err
}
func (f *fakeBackend) Retry(_ context.Context, provider string) (string, error) {
	f.call = "retry:" + provider
	return "provider retry state reset", f.err
}
func (f *fakeBackend) Explain(_ context.Context, instance, kind string, fileID int64, language string) (string, error) {
	f.call = "explain:" + instance + ":" + kind + ":" + language
	return "inventory: 2 tracks\nnext attempt: tomorrow", f.err
}
func (f *fakeBackend) Doctor(context.Context) (string, error) {
	f.call = "doctor"
	return "configuration: ok\nsqlite: ok\nLAPSE: ok", f.err
}
func (f *fakeBackend) AnalyzeSync(_ context.Context, media, subtitle string) (string, error) {
	f.call = "analyze:" + media + ":" + subtitle
	return "verdict: solid", f.err
}

func TestCommandSurface(t *testing.T) {
	tests := []struct {
		name string
		args []string
		call string
		out  string
	}{
		{"serve", []string{"serve", "--config", "/data/config.yaml"}, "serve", ""},
		{"scan", []string{"scan", "--instance", "tv", "--force-probe"}, "scan:tv:force", "scan complete\n"},
		{"search", []string{"search", "--instance", "tv", "--kind", "episode", "--file-id", "42", "--language", "hr"}, "search:tv:episode:hr", "outcome: installed\n"},
		{"retry", []string{"retry", "--provider", "titlovi"}, "retry:titlovi", "provider retry state reset\n"},
		{"explain", []string{"explain", "--instance", "movies", "--kind", "movie", "--file-id", "7", "--language", "en"}, "explain:movies:movie:en", "inventory: 2 tracks\nnext attempt: tomorrow\n"},
		{"doctor", []string{"doctor", "--config", "/data/config.yaml"}, "doctor", "configuration: ok\nsqlite: ok\nLAPSE: ok\n"},
		{"analyze", []string{"analyze-sync", "--media", "/media/a.mkv", "--subtitle", "/tmp/a.srt"}, "analyze:/media/a.mkv:/tmp/a.srt", "verdict: solid\n"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			backend := &fakeBackend{}
			var stdout, stderr bytes.Buffer
			command := Command{Open: func(context.Context, string) (Backend, error) { return backend, nil }, Stdout: &stdout, Stderr: &stderr}
			if code := command.Run(context.Background(), test.args); code != ExitOK {
				t.Fatalf("exit = %d, stderr=%q", code, stderr.String())
			}
			if backend.call != test.call || stdout.String() != test.out {
				t.Fatalf("call/output = %q/%q, want %q/%q", backend.call, stdout.String(), test.call, test.out)
			}
		})
	}
}

func TestUsageAndOperationExitCodes(t *testing.T) {
	for _, test := range []struct {
		name string
		args []string
		open OpenFunc
		code int
		text string
	}{
		{"unknown command", []string{"wat"}, nil, ExitUsage, "unknown command"},
		{"missing required", []string{"search", "--instance", "tv"}, nil, ExitUsage, "--kind"},
		{"bad kind", []string{"search", "--instance", "tv", "--kind", "track", "--file-id", "1", "--language", "en"}, nil, ExitUsage, "movie or episode"},
		{"unrelated flag", []string{"serve", "--provider", "x"}, nil, ExitUsage, "not valid for serve"},
		{"open failure", []string{"doctor", "--config", "/bad"}, func(context.Context, string) (Backend, error) { return nil, errors.New("invalid configuration") }, ExitFailure, "invalid configuration"},
		{"operation failure", []string{"retry", "--provider", "missing"}, func(context.Context, string) (Backend, error) {
			return &fakeBackend{err: errors.New("unknown provider")}, nil
		}, ExitFailure, "unknown provider"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var stderr bytes.Buffer
			code := (Command{Open: test.open, Stdout: &bytes.Buffer{}, Stderr: &stderr}).Run(context.Background(), test.args)
			if code != test.code || !strings.Contains(stderr.String(), test.text) {
				t.Fatalf("exit/stderr = %d/%q, want %d containing %q", code, stderr.String(), test.code, test.text)
			}
		})
	}
}

func TestOperationErrorsUseBackendRedaction(t *testing.T) {
	backend := redactingBackend{&fakeBackend{err: errors.New("secret path")}}
	var stderr bytes.Buffer
	code := (Command{Open: func(context.Context, string) (Backend, error) { return backend, nil }, Stdout: &bytes.Buffer{}, Stderr: &stderr}).Run(context.Background(), []string{"retry", "--provider", "p"})
	if code != ExitFailure || stderr.String() != "subsyncd: [redacted]\n" {
		t.Fatalf("exit/stderr = %d/%q", code, stderr.String())
	}
}
