package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"subsyncd/internal/control"
)

type fakeBackend struct {
	call string
	err  error
}

type redactingBackend struct{ *fakeBackend }

func (redactingBackend) Redact(error) error { return errors.New("[redacted]") }

type reportingBackend struct {
	*fakeBackend
	reportedCommand string
	reportedError   error
}

func (b *reportingBackend) ReportFailure(_ context.Context, command string, err error) {
	b.reportedCommand = command
	b.reportedError = err
}

func (f *fakeBackend) Close() error                { return nil }
func (f *fakeBackend) Serve(context.Context) error { f.call = "serve"; return f.err }
func (f *fakeBackend) Scan(_ context.Context, instance string, force bool) (control.ScanResult, error) {
	f.call = "scan:" + instance
	if force {
		f.call += ":force"
	}
	return control.ScanResult{Instance: instance, Media: 3, ForceProbe: force}, f.err
}
func (f *fakeBackend) Search(_ context.Context, args control.SearchArgs) (control.SearchResult, error) {
	f.call = "search:" + args.Instance + ":" + args.Kind + ":" + args.Language + ":" + args.Title + ":" + args.Path
	if args.RetryRejected {
		f.call += ":retry-rejected"
	}
	if args.Wait != 0 {
		f.call += ":wait=" + args.Wait.String()
	}
	if args.NoWait {
		f.call += ":no-wait"
	}
	return control.SearchResult{MediaID: 9, Languages: []control.LanguageOutcome{{Language: args.Language, Outcome: "installed"}}}, f.err
}
func (f *fakeBackend) Reject(_ context.Context, args control.RejectArgs) (control.RejectResult, error) {
	f.call = "reject:" + args.Title + ":" + args.Language + ":" + args.Reason
	if args.Undo {
		f.call += ":undo"
		return control.RejectResult{MediaID: 9, Rejected: control.LanguageOutcome{Language: "en"}, Undone: 1, Undo: true}, f.err
	}
	return control.RejectResult{MediaID: 9, Rejected: control.LanguageOutcome{Language: "en", Outcome: "removed", Reason: "rejected"}}, f.err
}
func (f *fakeBackend) Retry(_ context.Context, provider string) (control.RetryResult, error) {
	f.call = "retry:" + provider
	return control.RetryResult{Provider: provider, SearchesDue: 2}, f.err
}
func (f *fakeBackend) Find(_ context.Context, sel control.Selection) (control.FindResult, error) {
	f.call = "find:" + sel.Instance + ":" + sel.Kind + ":" + fmt.Sprint(sel.FileID) + ":" + sel.Title + ":" + sel.Path
	return control.FindResult{Matches: []control.FindMatch{{MediaMatch: control.MediaMatch{Instance: "radarr-main", Kind: "movie", FileID: 42, Title: "Dune", Year: 2021}, Languages: []control.LanguageState{{Language: "en", State: "missing"}}}}}, f.err
}
func (f *fakeBackend) Missing(_ context.Context, args control.MissingArgs) (control.MissingResult, error) {
	f.call = fmt.Sprintf("missing:%s:%s:%s:%d", args.Instance, args.Language, args.Title, args.Limit)
	return control.MissingResult{Total: 1, Items: []control.MissingItem{{MediaMatch: control.MediaMatch{Instance: "radarr-main", Kind: "movie", FileID: 42, Title: "Dune", Year: 2021}, Language: "en", Reason: "no_candidates", Action: "subsyncd search --instance radarr-main --kind movie --file-id 42 --language en"}}}, f.err
}
func (f *fakeBackend) Explain(_ context.Context, args control.ExplainArgs) (control.ExplainResult, error) {
	f.call = "explain:" + args.Instance + ":" + args.Kind + ":" + args.Language + ":" + args.Title + ":" + args.Path
	return control.ExplainResult{MediaID: 1, Instance: args.Instance, Kind: args.Kind, FileID: args.FileID, Title: "T", Probe: "unprobed"}, f.err
}
func (f *fakeBackend) Doctor(context.Context) (control.DoctorResult, error) {
	f.call = "doctor"
	return control.DoctorResult{Configuration: "ok", SQLite: "ok", MediaRoots: 1, LAPSE: "compatible", FFprobe: "available", Webhooks: "manual"}, f.err
}
func (f *fakeBackend) ForgetInstance(_ context.Context, instance string, apply bool) (control.ForgetInstanceResult, error) {
	f.call = "forget:" + instance
	if apply {
		f.call += ":yes"
	}
	return control.ForgetInstanceResult{Instance: instance, Applied: apply}, f.err
}
func (f *fakeBackend) AnalyzeSync(_ context.Context, media, subtitle string) (control.AnalyzeSyncResult, error) {
	f.call = "analyze:" + media + ":" + subtitle
	return control.AnalyzeSyncResult{Analysis: json.RawMessage(`{"verdict":"solid"}`)}, f.err
}

func TestCommandSurface(t *testing.T) {
	tests := []struct {
		name string
		args []string
		call string
		out  string
	}{
		{"serve", []string{"serve", "--config", "/data/config.yaml"}, "serve", ""},
		{"scan", []string{"scan", "--instance", "tv", "--force-probe"}, "scan:tv:force", "scan complete: instance=tv media=3 force_probe=true\n"},
		{"search", []string{"search", "--instance", "tv", "--kind", "episode", "--file-id", "42", "--language", "hr"}, "search:tv:episode:hr::", "media_id: 9\noutcome: installed\n"},
		{"search rejected", []string{"search", "--instance", "tv", "--kind", "episode", "--file-id", "42", "--language", "hr", "--retry-rejected"}, "search:tv:episode:hr::" + ":retry-rejected", "media_id: 9\noutcome: installed\n"},
		{"retry", []string{"retry", "--provider", "titlovi"}, "retry:titlovi", "provider retry state reset: titlovi; throttled searches due again: 2\n"},
		{"explain", []string{"explain", "--instance", "movies", "--kind", "movie", "--file-id", "7", "--language", "en"}, "explain:movies:movie:en::", "media: id=1 instance=movies kind=movie file_id=7 title=\"T\"\ninventory: tracks=0 probe=unprobed fingerprint_size=0 fingerprint_mtime=0001-01-01T00:00:00Z\nsearch: not scheduled\ncandidates: 0\ncandidate_rejections: 0\ninstallation: none\npack_cache: reusable=0\n"},
		{"search by title", []string{"search", "--title", "Dune (2021)", "--language", "hr"}, "search:::hr:Dune (2021):", "media_id: 9\noutcome: installed\n"},
		{"explain by path", []string{"explain", "--path", "/tv/Show/S01E01.mkv"}, "explain:::::/tv/Show/S01E01.mkv", "media: id=1 instance= kind= file_id=0 title=\"T\"\ninventory: tracks=0 probe=unprobed fingerprint_size=0 fingerprint_mtime=0001-01-01T00:00:00Z\nsearch: not scheduled\ncandidates: 0\ncandidate_rejections: 0\ninstallation: none\npack_cache: reusable=0\n"},
		{"find by title", []string{"find", "--title", "Dune", "--instance", "radarr-main"}, "find:radarr-main::0:Dune:", "radarr-main movie 42 \"Dune\" (2021)\n  en: missing\n"},
		{"find by file id", []string{"find", "--instance", "radarr-main", "--kind", "movie", "--file-id", "42"}, "find:radarr-main:movie:42::", "radarr-main movie 42 \"Dune\" (2021)\n  en: missing\n"},
		{"missing", []string{"missing", "--instance", "radarr-main", "--language", "en", "--title", "Dune", "--limit", "5"}, "missing:radarr-main:en:Dune:5", "1 missing (showing 1)\nradarr-main movie 42 \"Dune\" (2021) en: no_candidates\n  action: subsyncd search --instance radarr-main --kind movie --file-id 42 --language en\n"},
		{"missing defaults", []string{"missing"}, "missing::::0", "1 missing (showing 1)\nradarr-main movie 42 \"Dune\" (2021) en: no_candidates\n  action: subsyncd search --instance radarr-main --kind movie --file-id 42 --language en\n"},
		{"doctor", []string{"doctor", "--config", "/data/config.yaml"}, "doctor", "configuration: ok\nsqlite: ok\nmedia roots: ok (1)\nLAPSE: compatible\nffprobe: available\nwebhooks: manual\n"},
		{"forget", []string{"forget-instance", "--instance", "old", "--yes"}, "forget:old:yes", "forget-instance complete: instance=old media=0 searches=0 installations=0 rollback_copies=0 candidates=0 rejections=0 notifications=0 events=0 replays=0 history=0\nsubtitle files installed for this instance stay on disk and will no longer be managed\n"},
		{"webhook-url", []string{"webhook-url", "--instance", "tv", "--base", "http://x"}, "webhook-url:tv:http://x", "http://h/webhooks/tv?token=t\n"},
		{"rotate-webhook-token", []string{"rotate-webhook-token", "--instance", "tv"}, "rotate:tv", "webhook token rotated for instance tv; start subsyncd now: the Arr connection is updated in Arr at that start.\n"},
		{"analyze", []string{"analyze-sync", "--media", "/media/a.mkv", "--subtitle", "/tmp/a.srt"}, "analyze:/media/a.mkv:/tmp/a.srt", "{\n  \"verdict\": \"solid\"\n}\n"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			backend := &fakeBackend{}
			var stdout, stderr bytes.Buffer
			command := Command{Open: func(_ context.Context, _ string, openedCommand string) (Backend, error) {
				if openedCommand != test.args[0] {
					t.Fatalf("opened command = %q, want %q", openedCommand, test.args[0])
				}
				return backend, nil
			}, Stdout: &stdout, Stderr: &stderr}
			if code := command.Run(context.Background(), test.args); code != ExitOK {
				t.Fatalf("exit = %d, stderr=%q", code, stderr.String())
			}
			if backend.call != test.call || stdout.String() != test.out {
				t.Fatalf("call/output = %q/%q, want %q/%q", backend.call, stdout.String(), test.call, test.out)
			}
		})
	}
}

func TestForgetInstancePreviewOpensAsDiagnostic(t *testing.T) {
	backend := &fakeBackend{}
	var opened string
	var stdout bytes.Buffer
	command := Command{Open: func(_ context.Context, _ string, openedCommand string) (Backend, error) {
		opened = openedCommand
		return backend, nil
	}, Stdout: &stdout, Stderr: &bytes.Buffer{}}
	if code := command.Run(context.Background(), []string{"forget-instance", "--instance", "old"}); code != ExitOK {
		t.Fatalf("exit = %d", code)
	}
	if opened != "forget-instance-preview" || backend.call != "forget:old" {
		t.Fatalf("opened/call = %q/%q", opened, backend.call)
	}
}

func TestVersionDoesNotOpenBackend(t *testing.T) {
	var stdout, stderr bytes.Buffer
	command := Command{Version: "1.2.3", Open: func(context.Context, string, string) (Backend, error) { t.Fatal("backend opened"); return nil, nil }, Stdout: &stdout, Stderr: &stderr}
	if code := command.Run(context.Background(), []string{"--version"}); code != ExitOK || stdout.String() != "subsyncd 1.2.3\n" {
		t.Fatalf("exit/output = %d/%q", code, stdout.String())
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
		{"forget without instance", []string{"forget-instance", "--yes"}, nil, ExitUsage, "--instance is required"},
		{"yes outside forget", []string{"scan", "--instance", "tv", "--yes"}, nil, ExitUsage, "not valid for scan"},
		{"open failure", []string{"doctor", "--config", "/bad"}, func(context.Context, string, string) (Backend, error) {
			return nil, errors.New("invalid configuration")
		}, ExitFailure, "invalid configuration"},
		{"operation failure", []string{"retry", "--provider", "missing"}, func(context.Context, string, string) (Backend, error) {
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
	code := (Command{Open: func(context.Context, string, string) (Backend, error) { return backend, nil }, Stdout: &bytes.Buffer{}, Stderr: &stderr}).Run(context.Background(), []string{"retry", "--provider", "p"})
	if code != ExitFailure || stderr.String() != "subsyncd: [redacted]\n" {
		t.Fatalf("exit/stderr = %d/%q", code, stderr.String())
	}
}

func TestInitializedBackendFailureUsesStructuredReporterInsteadOfPlainText(t *testing.T) {
	backend := &reportingBackend{fakeBackend: &fakeBackend{err: errors.New("backend exploded")}}
	var stderr bytes.Buffer
	code := (Command{Open: func(context.Context, string, string) (Backend, error) { return backend, nil }, Stdout: &bytes.Buffer{}, Stderr: &stderr}).Run(context.Background(), []string{"serve"})
	if code != ExitFailure {
		t.Fatalf("exit = %d, want %d", code, ExitFailure)
	}
	if backend.reportedCommand != "serve" || backend.reportedError == nil || backend.reportedError.Error() != "backend exploded" {
		t.Fatalf("reported command/error = %q/%v", backend.reportedCommand, backend.reportedError)
	}
	if strings.Contains(stderr.String(), "backend exploded") || stderr.Len() != 0 {
		t.Fatalf("plain stderr = %q, want empty", stderr.String())
	}
}

type alreadyReportedFailure struct{}

func (alreadyReportedFailure) Error() string         { return "sanitized startup failure" }
func (alreadyReportedFailure) AlreadyReported() bool { return true }
func TestWrappedAlreadyReportedStartupFailureIsSilent(t *testing.T) {
	var stderr bytes.Buffer
	command := Command{Stderr: &stderr, Open: func(context.Context, string, string) (Backend, error) {
		return nil, errors.Join(errors.New("outer startup wrapper"), alreadyReportedFailure{})
	}}
	if code := command.Run(context.Background(), []string{"doctor"}); code != ExitFailure {
		t.Fatalf("exit=%d", code)
	}
	if stderr.Len() != 0 {
		t.Fatalf("duplicate output: %s", stderr.String())
	}
}

func (f *fakeBackend) WebhookURL(_ context.Context, instance, base string) (control.WebhookURLResult, error) {
	f.call = "webhook-url:" + instance + ":" + base
	return control.WebhookURLResult{Instance: instance, URL: "http://h/webhooks/" + instance + "?token=t"}, f.err
}
func (f *fakeBackend) RotateWebhookToken(_ context.Context, instance string) (control.RotateTokenResult, error) {
	f.call = "rotate:" + instance
	return control.RotateTokenResult{Instance: instance, Managed: true}, f.err
}

func TestWebhookCommands(t *testing.T) {
	for _, test := range []struct {
		name, opened, call, out string
		args                    []string
		code                    int
	}{
		{"url", "webhook-url", "webhook-url:tv:", "http://h/webhooks/tv?token=t\n", []string{"webhook-url", "--instance", "tv"}, ExitOK},
		{"url base", "webhook-url", "webhook-url:tv:http://x/p", "http://h/webhooks/tv?token=t\n", []string{"webhook-url", "--instance", "tv", "--base", "http://x/p"}, ExitOK},
		{"rotate", "rotate-webhook-token", "rotate:tv", "webhook token rotated for instance tv; start subsyncd now: the Arr connection is updated in Arr at that start.\n", []string{"rotate-webhook-token", "--instance", "tv"}, ExitOK},
		{"url without instance", "", "", "", []string{"webhook-url"}, ExitUsage},
		{"rotate without instance", "", "", "", []string{"rotate-webhook-token"}, ExitUsage},
		{"rotate rejects base", "", "", "", []string{"rotate-webhook-token", "--instance", "tv", "--base", "http://x"}, ExitUsage},
		{"url rejects yes", "", "", "", []string{"webhook-url", "--instance", "tv", "--yes"}, ExitUsage},
		{"url rejects unknown flag", "", "", "", []string{"webhook-url", "--instance", "tv", "--nope"}, ExitUsage},
	} {
		t.Run(test.name, func(t *testing.T) {
			backend := &fakeBackend{}
			var stdout, stderr bytes.Buffer
			var opened string
			command := Command{Open: func(_ context.Context, _ string, name string) (Backend, error) {
				opened = name
				return backend, nil
			}, Stdout: &stdout, Stderr: &stderr}
			if code := command.Run(context.Background(), test.args); code != test.code {
				t.Fatalf("exit = %d, stderr=%q", code, stderr.String())
			}
			if opened != test.opened || backend.call != test.call || stdout.String() != test.out {
				t.Fatalf("opened/call/out = %q/%q/%q", opened, backend.call, stdout.String())
			}
		})
	}
}

func runJSON(t *testing.T, open OpenFunc, args ...string) (int, map[string]any, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := (Command{Open: open, Stdout: &stdout, Stderr: &stderr}).Run(context.Background(), args)
	var envelope map[string]any
	decoder := json.NewDecoder(bytes.NewReader(stdout.Bytes()))
	if err := decoder.Decode(&envelope); err != nil {
		t.Fatalf("stdout %q is not JSON: %v", stdout.String(), err)
	}
	if decoder.More() {
		t.Fatalf("stdout %q holds more than one JSON value", stdout.String())
	}
	return code, envelope, stderr.String()
}

func openFake(backend Backend) OpenFunc {
	return func(context.Context, string, string) (Backend, error) { return backend, nil }
}

func TestJSONEnvelopeOnSuccess(t *testing.T) {
	code, envelope, stderr := runJSON(t, openFake(&fakeBackend{}), "doctor", "--json")
	if code != ExitOK || stderr != "" {
		t.Fatalf("exit/stderr = %d/%q", code, stderr)
	}
	result, _ := envelope["result"].(map[string]any)
	if envelope["schema"] != "subsyncd.cli/v1" || envelope["command"] != "doctor" || envelope["ok"] != true || result["lapse"] != "compatible" {
		t.Fatalf("envelope = %v", envelope)
	}
	if _, present := envelope["error"]; present {
		t.Fatalf("success envelope has an error: %v", envelope)
	}
}

func TestJSONEnvelopeOnUsageError(t *testing.T) {
	for _, args := range [][]string{
		{"search", "--json"},
		{"search", "--json", "--nope"},
		{"serve", "--json"},
		{"wat", "--json"},
	} {
		code, envelope, stderr := runJSON(t, nil, args...)
		failure, _ := envelope["error"].(map[string]any)
		if code != ExitUsage || stderr != "" || envelope["ok"] != false || failure["code"] != "usage" || failure["message"] == "" {
			t.Fatalf("%v: exit/stderr/envelope = %d/%q/%v", args, code, stderr, envelope)
		}
		if _, present := envelope["result"]; present {
			t.Fatalf("%v: failure envelope has a result", args)
		}
	}
}

func TestJSONErrorCodes(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
		code string
	}{
		{"unknown instance", &control.Error{Code: control.CodeUnknownInstance, Message: `unknown instance "x"`}, "unknown_instance"},
		{"wrapped coded", fmt.Errorf("outer: %w", &control.Error{Code: control.CodeUnknownProvider, Message: "p"}), "unknown_provider"},
		{"busy", errors.New("another subsyncd mutation process is active"), "busy"},
		{"other", errors.New("boom"), "internal"},
	} {
		t.Run(test.name, func(t *testing.T) {
			code, envelope, _ := runJSON(t, openFake(&fakeBackend{err: test.err}), "retry", "--provider", "p", "--json")
			failure, _ := envelope["error"].(map[string]any)
			if code != ExitFailure || envelope["ok"] != false || failure["code"] != test.code {
				t.Fatalf("exit/envelope = %d/%v, want code %s", code, envelope, test.code)
			}
		})
	}
}

func TestJSONUnknownInstanceCode(t *testing.T) {
	err := &control.Error{Code: control.CodeUnknownInstance, Message: `unknown instance "x"`}
	_, envelope, _ := runJSON(t, openFake(&fakeBackend{err: err}), "scan", "--instance", "x", "--json")
	failure, _ := envelope["error"].(map[string]any)
	if failure["code"] != "unknown_instance" || failure["message"] != `unknown instance "x"` {
		t.Fatalf("error = %v", failure)
	}
}

func TestBusyCode(t *testing.T) {
	open := func(context.Context, string, string) (Backend, error) {
		return nil, errors.New("another subsyncd mutation process is active")
	}
	code, envelope, _ := runJSON(t, open, "scan", "--instance", "tv", "--json")
	failure, _ := envelope["error"].(map[string]any)
	if code != ExitFailure || failure["code"] != "busy" {
		t.Fatalf("exit/error = %d/%v", code, failure)
	}
	if got := ErrorCode(errors.New("another subsyncd mutation process is active")); got != control.CodeBusy {
		t.Fatalf("ErrorCode = %q", got)
	}
}

func TestJSONFailureUsesRedactionAndReporter(t *testing.T) {
	backend := &reportingBackend{fakeBackend: &fakeBackend{err: errors.New("secret path")}}
	code, envelope, stderr := runJSON(t, openFake(redactingReporter{backend}), "retry", "--provider", "p", "--json")
	failure, _ := envelope["error"].(map[string]any)
	if code != ExitFailure || stderr != "" || failure["message"] != "[redacted]" || failure["code"] != "internal" {
		t.Fatalf("exit/stderr/error = %d/%q/%v", code, stderr, failure)
	}
	if backend.reportedError == nil {
		t.Fatal("failure was not reported")
	}
}

type redactingReporter struct{ *reportingBackend }

func (redactingReporter) Redact(error) error { return errors.New("[redacted]") }

func TestJSONAlreadyReportedStartupFailureStillEmitsEnvelope(t *testing.T) {
	open := func(context.Context, string, string) (Backend, error) { return nil, alreadyReportedFailure{} }
	code, envelope, stderr := runJSON(t, open, "doctor", "--json")
	failure, _ := envelope["error"].(map[string]any)
	if code != ExitFailure || stderr != "" || failure["message"] != "sanitized startup failure" {
		t.Fatalf("exit/stderr/error = %d/%q/%v", code, stderr, failure)
	}
}

func TestJSONIsNotValidForServe(t *testing.T) {
	var stderr bytes.Buffer
	code := (Command{Open: openFake(&fakeBackend{}), Stdout: &bytes.Buffer{}, Stderr: &stderr}).Run(context.Background(), []string{"serve", "--json"})
	if code != ExitUsage {
		t.Fatalf("exit = %d", code)
	}
}

func TestCodedErrorsPrintTheirMessageInTextMode(t *testing.T) {
	var stderr bytes.Buffer
	err := &control.Error{Code: control.CodeUnknownProvider, Message: `unknown provider "x"`}
	code := (Command{Open: openFake(&fakeBackend{err: err}), Stdout: &bytes.Buffer{}, Stderr: &stderr}).Run(context.Background(), []string{"retry", "--provider", "x"})
	if code != ExitFailure || stderr.String() != "subsyncd: unknown provider \"x\"\n" {
		t.Fatalf("exit/stderr = %d/%q", code, stderr.String())
	}
}

func TestHelpDescribesResultAndCodes(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := (Command{Stdout: &stdout, Stderr: &stderr}).Run(context.Background(), []string{"search", "--help"})
	if code != ExitUsage || !strings.Contains(stderr.String(), "--json") || !strings.Contains(stderr.String(), "unknown_instance") || strings.Contains(stderr.String(), "-force-probe") {
		t.Fatalf("exit/help = %d/%q", code, stderr.String())
	}
}

type fakeRemote struct {
	command string
	args    any
	err     error
	closed  bool
}

func (r *fakeRemote) Call(_ context.Context, command string, args, result any) error {
	r.command, r.args = command, args
	if r.err != nil {
		return r.err
	}
	switch out := result.(type) {
	case *control.RetryResult:
		*out = control.RetryResult{Provider: "titlovi", SearchesDue: 4}
	case *control.SearchResult:
		*out = control.SearchResult{MediaID: 9, Languages: []control.LanguageOutcome{{Language: "hr", Outcome: "running"}}}
	case *control.RejectResult:
		*out = control.RejectResult{MediaID: 9, Rejected: control.LanguageOutcome{Language: "en", Outcome: "installed", Provider: "titlovi", Candidate: "c2", Score: 40, Subtitle: "Movie (2019)/Movie.en.srt"}}
	}
	return nil
}
func (r *fakeRemote) Close() error { r.closed = true; return nil }

func TestRoutedCommandUsesDaemonWhenAvailable(t *testing.T) {
	remote := &fakeRemote{}
	var stdout bytes.Buffer
	command := Command{
		Stdout: &stdout, Stderr: &bytes.Buffer{},
		Open: func(context.Context, string, string) (Backend, error) { t.Fatal("Open called"); return nil, nil },
		Dial: func(context.Context, string) (Remote, error) { return remote, nil },
	}
	if code := command.Run(context.Background(), []string{"retry", "--provider", "titlovi"}); code != ExitOK {
		t.Fatalf("exit = %d", code)
	}
	args, ok := remote.args.(control.RetryArgs)
	if remote.command != "retry" || !ok || args.Provider != "titlovi" || !remote.closed {
		t.Fatalf("call = %q %#v closed=%v", remote.command, remote.args, remote.closed)
	}
	if want := "provider retry state reset: titlovi; throttled searches due again: 4\n"; stdout.String() != want {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestRoutedCommandJSONThroughDaemon(t *testing.T) {
	open := func(context.Context, string, string) (Backend, error) { t.Fatal("Open called"); return nil, nil }
	command := Command{Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}, Open: open,
		Dial: func(context.Context, string) (Remote, error) { return &fakeRemote{}, nil }}
	var stdout bytes.Buffer
	command.Stdout = &stdout
	if code := command.Run(context.Background(), []string{"retry", "--provider", "titlovi", "--json"}); code != ExitOK {
		t.Fatalf("exit = %d", code)
	}
	var envelope map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &envelope); err != nil || envelope["ok"] != true || envelope["command"] != "retry" {
		t.Fatalf("stdout = %q (%v)", stdout.String(), err)
	}
}

func TestRoutedCommandFallsBackWithoutDaemon(t *testing.T) {
	backend := &fakeBackend{}
	var opened string
	command := Command{
		Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{},
		Open: func(_ context.Context, _ string, name string) (Backend, error) { opened = name; return backend, nil },
		Dial: func(context.Context, string) (Remote, error) { return nil, control.ErrNoDaemon },
	}
	if code := command.Run(context.Background(), []string{"retry", "--provider", "titlovi"}); code != ExitOK {
		t.Fatalf("exit = %d", code)
	}
	if opened != "retry" || backend.call != "retry:titlovi" {
		t.Fatalf("opened/call = %q/%q", opened, backend.call)
	}
}

func TestRoutedCommandReportsOtherDialErrors(t *testing.T) {
	command := Command{
		Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{},
		Open: func(context.Context, string, string) (Backend, error) { t.Fatal("Open called"); return nil, nil },
		Dial: func(context.Context, string) (Remote, error) { return nil, errors.New("connect failed") },
	}
	var stdout bytes.Buffer
	command.Stdout = &stdout
	code := command.Run(context.Background(), []string{"retry", "--provider", "titlovi", "--json"})
	var envelope struct{ Error struct{ Code string } }
	_ = json.Unmarshal(stdout.Bytes(), &envelope)
	if code != ExitFailure || envelope.Error.Code != control.CodeInternal {
		t.Fatalf("exit/stdout = %d/%q", code, stdout.String())
	}
}

func TestRoutedCommandReportsDaemonErrorCode(t *testing.T) {
	remote := &fakeRemote{err: &control.Error{Code: control.CodeUnknownProvider, Message: `unknown provider "x"`}}
	command := Command{
		Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{},
		Open: func(context.Context, string, string) (Backend, error) { t.Fatal("Open called"); return nil, nil },
		Dial: func(context.Context, string) (Remote, error) { return remote, nil },
	}
	var stdout bytes.Buffer
	command.Stdout = &stdout
	code := command.Run(context.Background(), []string{"retry", "--provider", "x", "--json"})
	var envelope struct {
		Error struct{ Code, Message string }
	}
	_ = json.Unmarshal(stdout.Bytes(), &envelope)
	if code != ExitFailure || envelope.Error.Code != "unknown_provider" || envelope.Error.Message != `unknown provider "x"` || !remote.closed {
		t.Fatalf("exit/stdout = %d/%q", code, stdout.String())
	}
}

func TestStopFirstCommandsDoNotDial(t *testing.T) {
	backend := &fakeBackend{}
	command := Command{
		Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{},
		Open: func(context.Context, string, string) (Backend, error) { return backend, nil },
		Dial: func(context.Context, string) (Remote, error) { t.Fatal("Dial called"); return nil, nil },
	}
	for _, args := range [][]string{
		{"rotate-webhook-token", "--instance", "tv"},
		{"forget-instance", "--instance", "old", "--yes"},
		{"scan", "--instance", "tv"},
		{"doctor"},
	} {
		if code := command.Run(context.Background(), args); code != ExitOK {
			t.Fatalf("%v exit = %d", args, code)
		}
	}
}

func TestSelectionFlagsAreExclusive(t *testing.T) {
	for _, test := range []struct {
		name string
		args []string
	}{
		{"title with file id", []string{"explain", "--title", "Dune", "--instance", "radarr-main", "--kind", "movie", "--file-id", "42"}},
		{"title with path", []string{"find", "--title", "Dune", "--path", "/m/Dune.mkv"}},
		{"nothing selected", []string{"find"}},
		{"file id without instance", []string{"explain", "--kind", "movie", "--file-id", "42"}},
		{"file id without kind", []string{"find", "--instance", "radarr-main", "--file-id", "42"}},
		{"bad kind with title", []string{"find", "--title", "Dune", "--kind", "show"}},
		{"timeout with no-wait", []string{"search", "--title", "Dune", "--timeout", "1m", "--no-wait"}},
		{"non-positive timeout", []string{"search", "--title", "Dune", "--timeout", "0s"}},
		{"timeout on explain", []string{"explain", "--title", "Dune", "--timeout", "1m"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			var stdout bytes.Buffer
			code := (Command{Open: openFake(&fakeBackend{}), Stdout: &stdout, Stderr: &bytes.Buffer{}}).Run(context.Background(), append(test.args, "--json"))
			var out struct {
				Error control.Error `json:"error"`
			}
			if err := json.Unmarshal(stdout.Bytes(), &out); err != nil || code != ExitUsage || out.Error.Code != control.CodeUsage {
				t.Fatalf("exit/out = %d/%s", code, stdout.String())
			}
		})
	}
}

func TestAmbiguousMediaListsCandidates(t *testing.T) {
	candidates := []control.MediaMatch{
		{Instance: "sonarr-main", Kind: "episode", FileID: 1234, Title: "Shameless", Season: 1, Episode: 1},
		{Instance: "sonarr-main", Kind: "episode", FileID: 5678, Title: "Shameless", Season: 1, Episode: 1},
	}
	err := &control.Error{Code: control.CodeAmbiguousMedia, Message: "2 files match this selection", Candidates: candidates}
	var stdout, stderr bytes.Buffer
	code := (Command{Open: openFake(&fakeBackend{err: err}), Stdout: &stdout, Stderr: &stderr}).Run(context.Background(), []string{"explain", "--title", "Shameless"})
	want := "subsyncd: 2 files match this selection\nsonarr-main episode 1234 \"Shameless\" S01E01\nsonarr-main episode 5678 \"Shameless\" S01E01\n"
	if code != ExitFailure || stderr.String() != want {
		t.Fatalf("exit/stderr = %d/%q", code, stderr.String())
	}
	stdout.Reset()
	code = (Command{Open: openFake(&fakeBackend{err: err}), Stdout: &stdout, Stderr: &stderr}).Run(context.Background(), []string{"explain", "--title", "Shameless", "--json"})
	var out struct {
		Error control.Error `json:"error"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil || code != ExitFailure || out.Error.Code != "ambiguous_media" || len(out.Error.Candidates) != 2 || out.Error.Candidates[1].FileID != 5678 {
		t.Fatalf("exit/out = %d/%s", code, stdout.String())
	}
}

func TestAmbiguousMediaCandidatesPrintAfterStructuredReport(t *testing.T) {
	err := &control.Error{Code: control.CodeAmbiguousMedia, Message: "2 files match", Candidates: []control.MediaMatch{{Instance: "radarr-main", Kind: "movie", FileID: 1, Title: "Dune", Year: 1984}, {Instance: "radarr-main", Kind: "movie", FileID: 2, Title: "Dune", Year: 2021}}}
	backend := &reportingBackend{fakeBackend: &fakeBackend{err: err}}
	var stderr bytes.Buffer
	code := (Command{Open: openFake(backend), Stdout: &bytes.Buffer{}, Stderr: &stderr}).Run(context.Background(), []string{"find", "--file-id", "1", "--instance", "radarr-main", "--kind", "movie"})
	want := "radarr-main movie 1 \"Dune\" (1984)\nradarr-main movie 2 \"Dune\" (2021)\n"
	if code != ExitFailure || stderr.String() != want || backend.reportedCommand != "find" {
		t.Fatalf("exit/stderr = %d/%q", code, stderr.String())
	}
}

func TestSearchGoesToTheDaemonWithItsWait(t *testing.T) {
	for _, test := range []struct {
		name string
		args []string
		want control.SearchArgs
	}{
		{"default wait", []string{"search", "--title", "Show S02E05"}, control.SearchArgs{Selection: control.Selection{Title: "Show S02E05"}}},
		{"timeout", []string{"search", "--title", "Show S02E05", "--language", "hr", "--timeout", "90s"}, control.SearchArgs{Selection: control.Selection{Title: "Show S02E05"}, Language: "hr", Wait: 90 * time.Second}},
		{"no wait", []string{"search", "--instance", "tv", "--kind", "episode", "--file-id", "7", "--language", "hr", "--no-wait", "--retry-rejected"}, control.SearchArgs{Selection: control.Selection{Instance: "tv", Kind: "episode", FileID: 7}, Language: "hr", NoWait: true, RetryRejected: true}},
	} {
		t.Run(test.name, func(t *testing.T) {
			remote := &fakeRemote{}
			var stdout bytes.Buffer
			command := Command{Stdout: &stdout, Stderr: &bytes.Buffer{},
				Open: func(context.Context, string, string) (Backend, error) { t.Fatal("Open called"); return nil, nil },
				Dial: func(context.Context, string) (Remote, error) { return remote, nil },
			}
			if code := command.Run(context.Background(), test.args); code != ExitOK {
				t.Fatalf("exit = %d", code)
			}
			if args, ok := remote.args.(control.SearchArgs); remote.command != "search" || !ok || args != test.want {
				t.Fatalf("call = %q %#v", remote.command, remote.args)
			}
			if want := "media_id: 9\noutcome: running\n"; stdout.String() != want {
				t.Fatalf("stdout = %q", stdout.String())
			}
		})
	}
}

func TestSearchWithoutDaemonPassesWaitToTheBackend(t *testing.T) {
	backend := &fakeBackend{}
	command := Command{Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{},
		Open: openFake(backend),
		Dial: func(context.Context, string) (Remote, error) { return nil, control.ErrNoDaemon },
	}
	if code := command.Run(context.Background(), []string{"search", "--title", "Dune", "--timeout", "2m"}); code != ExitOK {
		t.Fatalf("exit = %d", code)
	}
	if backend.call != "search::::Dune::wait=2m0s" {
		t.Fatalf("call = %q", backend.call)
	}
}

func TestRejectCommand(t *testing.T) {
	backend := &fakeBackend{}
	var stdout, stderr bytes.Buffer
	command := Command{Stdout: &stdout, Stderr: &stderr, Open: openFake(backend),
		Dial: func(context.Context, string) (Remote, error) { return nil, control.ErrNoDaemon }}
	if code := command.Run(context.Background(), []string{"reject", "--title", "Movie (2019)", "--language", "en", "--reason", "out_of_sync"}); code != ExitOK {
		t.Fatalf("exit = %d, stderr %q", code, stderr.String())
	}
	if backend.call != "reject:Movie (2019):en:out_of_sync" || stdout.String() != "media_id: 9\nlanguage: en\noutcome: removed\nreason: rejected\n" {
		t.Fatalf("call/stdout = %q/%q", backend.call, stdout.String())
	}
	stdout.Reset()
	if code := command.Run(context.Background(), []string{"reject", "--title", "Movie (2019)", "--language", "en", "--undo"}); code != ExitOK {
		t.Fatalf("undo exit = %d, stderr %q", code, stderr.String())
	}
	if backend.call != "reject:Movie (2019):en::undo" || stdout.String() != "media_id: 9\nlanguage: en\nundone: 1\n" {
		t.Fatalf("undo call/stdout = %q/%q", backend.call, stdout.String())
	}
	for _, args := range [][]string{
		{"reject", "--title", "Movie"},
		{"reject", "--title", "Movie", "--reason", "boring"},
		{"reject", "--title", "Movie", "--undo", "--reason", "other"},
		{"reject", "--title", "Movie", "--undo", "--timeout", "1m"},
		{"reject", "--reason", "other"},
		{"reject", "--title", "Movie", "--reason", "other", "--retry-rejected"},
		{"reject", "--title", "Movie", "--reason", "other", "--timeout", "1m", "--no-wait"},
	} {
		backend.call = ""
		var out bytes.Buffer
		usage := Command{Stdout: &out, Stderr: &bytes.Buffer{}, Open: openFake(backend)}
		if code := usage.Run(context.Background(), append(args, "--json")); code != ExitUsage || backend.call != "" || !strings.Contains(out.String(), `"code":"usage"`) {
			t.Fatalf("%v: exit = %d, call %q, stdout %q", args, code, backend.call, out.String())
		}
	}
}

func TestRejectGoesToTheDaemon(t *testing.T) {
	remote := &fakeRemote{}
	var stdout bytes.Buffer
	command := Command{Stdout: &stdout, Stderr: &bytes.Buffer{},
		Open: func(context.Context, string, string) (Backend, error) { t.Fatal("Open called"); return nil, nil },
		Dial: func(context.Context, string) (Remote, error) { return remote, nil },
	}
	if code := command.Run(context.Background(), []string{"reject", "--title", "Movie (2019)", "--reason", "wrong_release", "--timeout", "2m", "--json"}); code != ExitOK {
		t.Fatalf("exit = %d", code)
	}
	want := control.RejectArgs{Selection: control.Selection{Title: "Movie (2019)"}, Reason: "wrong_release", Wait: 2 * time.Minute}
	if args, ok := remote.args.(control.RejectArgs); remote.command != "reject" || !ok || args != want {
		t.Fatalf("call = %q %#v", remote.command, remote.args)
	}
	var envelope struct {
		OK     bool
		Result control.RejectResult
	}
	if err := json.Unmarshal(stdout.Bytes(), &envelope); err != nil || !envelope.OK || envelope.Result.Rejected.Subtitle != "Movie (2019)/Movie.en.srt" {
		t.Fatalf("stdout = %s (%v)", stdout.String(), err)
	}
}

func TestMissingRejectsBadFlags(t *testing.T) {
	for _, args := range [][]string{
		{"missing", "--limit", "-1"},
		{"missing", "--file-id", "4"},
		{"missing", "--path", "/m/a.mkv"},
	} {
		var stderr bytes.Buffer
		backend := &fakeBackend{}
		code := (Command{Open: openFake(backend), Stdout: &bytes.Buffer{}, Stderr: &stderr}).Run(context.Background(), args)
		if code != ExitUsage || backend.call != "" {
			t.Fatalf("%v: exit = %d call = %q stderr = %q", args, code, backend.call, stderr.String())
		}
	}
}

func TestMissingHelpListsReasons(t *testing.T) {
	var stderr bytes.Buffer
	(Command{Stdout: &bytes.Buffer{}, Stderr: &stderr}).Run(context.Background(), []string{"missing", "--help"})
	for _, want := range []string{"not_searched", "no_candidates", "all_rejected", "providers_cooling_down", "search_failing", "unsupported", "total", "items[]", "unknown_instance", "unknown_language", "--limit"} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("help lacks %q: %s", want, stderr.String())
		}
	}
}
