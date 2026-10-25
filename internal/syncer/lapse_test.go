package syncer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"subsyncd/internal/domain"
)

const testSRT = "1\n00:00:01,000 --> 00:00:02,000\nHello\n"

func TestParseReportAcceptsDocumentedSolidResult(t *testing.T) {
	payload := fixture(t, "solid.json")
	result, err := parseReport(payload)
	if err != nil {
		t.Fatal(err)
	}
	if result.Verdict != "solid" || result.Mode != "auto/shifted" || result.OffsetMS != 22 || result.Splits != 0 {
		t.Fatalf("result = %#v", result)
	}
}

func TestParseReportAcceptsVersion230Report(t *testing.T) {
	result, err := parseReport(fixture(t, "solid-v2.3.json"))
	if err != nil {
		t.Fatal(err)
	}
	if result.Verdict != "solid" || result.OffsetMS != 22 || result.Splits != 0 {
		t.Fatalf("result = %#v", result)
	}
}

func TestParseReportRejectsMalformedOrUnsafeResults(t *testing.T) {
	tests := []struct {
		name    string
		payload string
	}{
		{"truncated", `{"mode":"auto/shifted"`},
		{"trailing document", string(fixture(t, "solid.json")) + `{}`},
		{"unknown field", strings.Replace(string(fixture(t, "solid.json")), `"splits":[]`, `"splits":[],"surprise":true`, 1)},
		{"unknown verdict", strings.Replace(string(fixture(t, "solid.json")), `"solid"`, `"forced"`, 1)},
		{"nonfinite", strings.Replace(string(fixture(t, "solid.json")), `"ratio":1`, `"ratio":1e999`, 1)},
		{"confidence above one", strings.Replace(string(fixture(t, "solid.json")), `"confidence":0.455`, `"confidence":1.1`, 1)},
		{"missing snapped", strings.Replace(string(fixture(t, "solid.json")), `"snapped":0,`, ``, 1)},
		{"negative snapped", strings.Replace(string(fixture(t, "solid.json")), `"snapped":0`, `"snapped":-1`, 1)},
		{"snapped above cues", strings.Replace(string(fixture(t, "solid.json")), `"snapped":0`, `"snapped":1579`, 1)},
		{"parts mismatch", strings.Replace(string(fixture(t, "solid.json")), `"parts":1`, `"parts":2`, 1)},
		{"offsets count mismatch", strings.Replace(string(fixture(t, "solid-v2.3.json")), `"offsets":[22]`, `"offsets":[22,30]`, 1)},
		{"empty offsets", strings.Replace(string(fixture(t, "solid-v2.3.json")), `"offsets":[22]`, `"offsets":[]`, 1)},
		{"unchanged without write", strings.Replace(string(fixture(t, "solid-v2.3.json")), `"written":true,"unchanged":false`, `"written":false,"unchanged":true`, 1)},
		{"unknown field in v2.3 report", strings.Replace(string(fixture(t, "solid-v2.3.json")), `"offsets":[22]`, `"offsets":[22],"surprise":true`, 1)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := parseReport([]byte(test.payload)); err == nil {
				t.Fatal("parseReport() error = nil")
			}
		})
	}
}

// Splits must ascend and stay below "cues" (1578 in solid.json), and each
// refusal must come from the split check itself, not an earlier one.
func TestParseReportRefusesSplitsOutsideTheCues(t *testing.T) {
	for _, test := range []struct {
		name, parts, splits string
	}{
		{"unordered split", "3", "[10,9]"},
		{"split at cues", "2", "[1578]"},
		{"split past cues", "2", "[1579]"},
		{"split at zero", "2", "[0]"},
	} {
		t.Run(test.name, func(t *testing.T) {
			payload := strings.Replace(strings.Replace(string(fixture(t, "solid.json")), `"parts":1`, `"parts":`+test.parts, 1), `"splits":[]`, `"splits":`+test.splits, 1)
			_, err := parseReport([]byte(payload))
			var check *reportCheckError
			if !errors.As(err, &check) || check.message != "LAPSE JSON contains invalid split positions" {
				t.Fatalf("parseReport() error = %v, want the split check", err)
			}
		})
	}
}

func TestAnalyzeUsesSafeFlagsAndRequiresSolidVerdict(t *testing.T) {
	media, subtitle := testFiles(t)
	var commands []Command
	runner := runnerFunc(func(_ context.Context, command Command) (Execution, error) {
		commands = append(commands, command)
		return Execution{Stdout: fixture(t, "solid.json"), ExitCode: 0}, nil
	})
	lapse := newTestLapse(t, runner, filepath.Dir(media))
	result, err := lapse.Analyze(context.Background(), media, subtitle)
	if err != nil || result.Verdict != "solid" {
		t.Fatalf("Analyze() = %#v, %v", result, err)
	}
	if len(commands) != 1 {
		t.Fatalf("commands = %#v", commands)
	}
	wantTail := []string{"--dry-run", "--json", "--strict", "--no-sidecar"}
	if len(commands[0].Args) != 6 || commands[0].Args[0] != media || commands[0].Args[1] == subtitle || !slices.Equal(commands[0].Args[2:], wantTail) {
		t.Fatalf("args = %#v", commands[0].Args)
	}
	if !strings.HasPrefix(commands[0].Args[1], commands[0].Dir+string(filepath.Separator)) {
		t.Fatalf("analysis subtitle %q is outside workspace %q", commands[0].Args[1], commands[0].Dir)
	}
	if _, err := os.Stat(subtitle + ".bak"); !os.IsNotExist(err) {
		t.Fatalf("analysis touched input: %v", err)
	}

	runner = runnerFunc(func(_ context.Context, _ Command) (Execution, error) {
		return Execution{Stdout: fixture(t, "unsure.json"), ExitCode: 2}, nil
	})
	lapse = newTestLapse(t, runner, filepath.Dir(media))
	_, err = lapse.Analyze(context.Background(), media, subtitle)
	var verdictErr *VerdictError
	if !errors.As(err, &verdictErr) || verdictErr.Verdict != "unsure" {
		t.Fatalf("weak Analyze() error = %T %v", err, err)
	}
}

func TestAnalyzeAcceptsAnUnchangedDryRun(t *testing.T) {
	media, subtitle := testFiles(t)
	// A dry run of an already synchronized subtitle: LAPSE 2.3 reports it
	// written and unchanged.
	payload := strings.Replace(string(fixture(t, "solid-v2.3.json")), `"unchanged":false`, `"unchanged":true`, 1)
	lapse := newTestLapse(t, runnerFunc(func(context.Context, Command) (Execution, error) {
		return Execution{Stdout: []byte(payload), ExitCode: 0}, nil
	}), filepath.Dir(media))
	result, err := lapse.Analyze(context.Background(), media, subtitle)
	if err != nil || result.Verdict != "solid" {
		t.Fatalf("Analyze() = %#v, %v", result, err)
	}
}

func TestAnalyzeCandidateBypassesLapseForExactHash(t *testing.T) {
	calls := 0
	lapse := newTestLapse(t, runnerFunc(func(_ context.Context, _ Command) (Execution, error) {
		calls++
		return Execution{}, errors.New("should not run")
	}), t.TempDir())
	result, err := lapse.AnalyzeCandidate(context.Background(), domain.Candidate{ExactHash: true}, "/media/movie.mkv", "/cache/subtitle.srt")
	if err != nil || result.Verdict != "exact_hash" || calls != 0 {
		t.Fatalf("AnalyzeCandidate() = %#v, %v, calls=%d", result, err, calls)
	}
	result, err = lapse.SynchronizeCandidate(context.Background(), domain.Candidate{ExactHash: true}, "/media/movie.mkv", "/cache/subtitle.srt", "/work/output.srt")
	if err != nil || result.Verdict != "exact_hash" || calls != 0 {
		t.Fatalf("SynchronizeCandidate() = %#v, %v, calls=%d", result, err, calls)
	}
}

func TestLapseReportsItsVersionAndRejectionBaseline(t *testing.T) {
	lapse := newTestLapse(t, runnerFunc(func(context.Context, Command) (Execution, error) {
		return Execution{}, nil
	}), t.TempDir())
	if got := lapse.CompatibilityVersion(); got != "2.3.1" {
		t.Fatalf("CompatibilityVersion() = %q, want 2.3.1", got)
	}
	if got := lapse.RejectionBaseline(); got != "2.3.1" {
		t.Fatalf("RejectionBaseline() = %q, want 2.3.1", got)
	}
}

// Every LAPSE bump must record a keep-or-move decision for the rejection
// baseline; forgetting it would keep stale rejections silently.
func TestRejectionBaselineWasReviewedForThisLAPSE(t *testing.T) {
	if RejectionBaselineReviewedFor != CompatibilityVersion {
		t.Fatalf("RejectionBaselineReviewedFor = %q, CompatibilityVersion = %q: decide whether LAPSE %s moves RejectionBaseline (see docs/development/providers/lapse.md#upgrading-lapse), then set RejectionBaselineReviewedFor to %q", RejectionBaselineReviewedFor, CompatibilityVersion, CompatibilityVersion, CompatibilityVersion)
	}
}

func TestDockerfilesPinCompatibilityVersion(t *testing.T) {
	for _, name := range []string{"Dockerfile", "Dockerfile.release"} {
		payload, err := os.ReadFile(filepath.Join("..", "..", name))
		if err != nil {
			t.Fatal(err)
		}
		pins := regexp.MustCompile(`(?m)^ARG LAPSE_VERSION=(\S+)$`).FindAllStringSubmatch(string(payload), -1)
		if len(pins) == 0 {
			t.Fatalf("%s has no ARG LAPSE_VERSION", name)
		}
		for _, pin := range pins {
			if pin[1] != CompatibilityVersion {
				t.Errorf("%s pins LAPSE %s, CompatibilityVersion is %s", name, pin[1], CompatibilityVersion)
			}
		}
	}
}

func TestAnalyzeRejectsTimeoutNonzeroAndTruncatedOutput(t *testing.T) {
	media, subtitle := testFiles(t)
	tests := []struct {
		name      string
		execution Execution
		runErr    error
		wantIs    error
	}{
		{"timeout", Execution{}, context.DeadlineExceeded, context.DeadlineExceeded},
		{"nonzero", Execution{Stderr: []byte("decoder failed"), ExitCode: 1}, nil, nil},
		{"truncated JSON", Execution{Stdout: fixture(t, "solid.json"), StdoutTruncated: true}, nil, nil},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			lapse := newTestLapse(t, runnerFunc(func(_ context.Context, _ Command) (Execution, error) {
				return test.execution, test.runErr
			}), filepath.Dir(media))
			_, err := lapse.Analyze(context.Background(), media, subtitle)
			if err == nil || test.wantIs != nil && !errors.Is(err, test.wantIs) {
				t.Fatalf("Analyze() error = %T %v", err, err)
			}
		})
	}
}

func TestSynchronizeClassifiesUndecodableNonzeroExit(t *testing.T) {
	media, subtitle := testFiles(t)
	const stdoutToken = "private stdout fixture /private/path/token"
	const stderrToken = "private stderr fixture /private/path/token"
	tests := []struct {
		name        string
		exitCode    int
		stdout      []byte
		stderr      []byte
		stdoutEmpty bool
		stderrEmpty bool
	}{
		{name: "empty buffers", exitCode: 2, stdoutEmpty: true, stderrEmpty: true},
		{name: "stdout only", exitCode: 2, stdout: []byte(stdoutToken), stderrEmpty: true},
		{name: "stderr only", exitCode: 2, stdoutEmpty: true, stderr: []byte(stderrToken)},
		{name: "another nonzero exit", exitCode: 17, stdoutEmpty: true, stderrEmpty: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			lapse := newTestLapse(t, runnerFunc(func(context.Context, Command) (Execution, error) {
				return Execution{ExitCode: test.exitCode, Stdout: test.stdout, Stderr: test.stderr}, nil
			}), filepath.Dir(media))
			_, err := lapse.Synchronize(context.Background(), media, subtitle, filepath.Join(t.TempDir(), "output.srt"))
			var processExit *ProcessExitError
			if !errors.As(err, &processExit) {
				t.Fatalf("Synchronize() error = %T %v, want ProcessExitError", err, err)
			}
			if processExit.ExitCode != test.exitCode || processExit.StdoutEmpty != test.stdoutEmpty || processExit.StderrEmpty != test.stderrEmpty {
				t.Fatalf("ProcessExitError = %#v", processExit)
			}
			if message := processExit.Error(); strings.Contains(message, stdoutToken) || strings.Contains(message, stderrToken) || strings.Contains(message, media) || strings.Contains(message, subtitle) {
				t.Fatalf("ProcessExitError leaked private data: %q", message)
			}
			if test.stdoutEmpty && (processExit.Report != "" || processExit.Error() != fmt.Sprintf("LAPSE exited with code %d", test.exitCode)) {
				t.Fatalf("ProcessExitError without a report = %#v, %q", processExit, processExit.Error())
			}
			if !strings.Contains(processExit.Error(), fmt.Sprintf("%d", test.exitCode)) {
				t.Fatalf("ProcessExitError = %q, want exit code", processExit.Error())
			}
		})
	}
}

func TestSynchronizeRequiresSolidReportAndValidatedOutput(t *testing.T) {
	media, subtitle := testFiles(t)
	output := filepath.Join(t.TempDir(), "synced.srt")
	runner := runnerFunc(func(_ context.Context, command Command) (Execution, error) {
		wantTail := []string{"--output", output, "--no-backup", "--json", "--strict", "--no-sidecar"}
		if len(command.Args) != 8 || command.Args[0] != media || command.Args[1] != subtitle || !slices.Equal(command.Args[2:], wantTail) {
			t.Fatalf("args = %#v", command.Args)
		}
		if err := os.WriteFile(output, []byte(testSRT), 0o600); err != nil {
			t.Fatal(err)
		}
		return Execution{Stdout: reportJSON(t, "solid", output, true), ExitCode: 0}, nil
	})
	lapse := newTestLapse(t, runner, filepath.Dir(media))
	result, err := lapse.Synchronize(context.Background(), media, subtitle, output)
	if err != nil || result.Verdict != "solid" {
		t.Fatalf("Synchronize() = %#v, %v", result, err)
	}

	// json.Marshal sorts the keys, so "written" comes last.
	unchanged := bytes.Replace(reportJSON(t, "solid", output, true), []byte(`"written":true`), []byte(`"unchanged":true,"written":true`), 1)
	for _, test := range []struct {
		name   string
		write  string
		stdout []byte
		exit   int
	}{
		{"missing output", "", reportJSON(t, "solid", output, true), 0},
		{"reported unchanged", testSRT, unchanged, 0},
		{"invalid output", "not a subtitle", reportJSON(t, "solid", output, true), 0},
		{"reported unwritten", testSRT, reportJSON(t, "solid", output, false), 0},
		{"wrong reported path", testSRT, reportJSON(t, "solid", output+".other", true), 0},
		{"weak verdict", "", fixture(t, "nothing.json"), 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "synced.srt")
			stdout := bytesReplacingPath(test.stdout, output, path)
			runner := runnerFunc(func(_ context.Context, _ Command) (Execution, error) {
				if test.write != "" {
					if err := os.WriteFile(path, []byte(test.write), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				return Execution{Stdout: stdout, ExitCode: test.exit}, nil
			})
			lapse := newTestLapse(t, runner, filepath.Dir(media))
			if _, err := lapse.Synchronize(context.Background(), media, subtitle, path); err == nil {
				t.Fatal("Synchronize() error = nil")
			}
		})
	}
}

func TestFailuresRedactMediaPathsAndClassifyNoSpeech(t *testing.T) {
	media, subtitle := testFiles(t)
	runner := runnerFunc(func(_ context.Context, _ Command) (Execution, error) {
		return Execution{Stderr: []byte("No speech found in: " + media + " while reading " + subtitle), ExitCode: 1}, nil
	})
	lapse := newTestLapse(t, runner, filepath.Dir(media))
	_, err := lapse.Analyze(context.Background(), media, subtitle)
	var noSpeech *NoSpeechError
	if !errors.As(err, &noSpeech) {
		t.Fatalf("error = %T %v", err, err)
	}
	if strings.Contains(err.Error(), media) || strings.Contains(err.Error(), subtitle) {
		t.Fatalf("error leaked paths: %v", err)
	}
}

func TestLapseFailuresDoNotExposeRawProcessOutput(t *testing.T) {
	const sentinel = "private-subtitle https://example.invalid/token /unrelated/private/file"
	media, subtitle := testFiles(t)
	report := func(field, value string) []byte {
		var document map[string]any
		if err := json.Unmarshal(fixture(t, "unsure.json"), &document); err != nil {
			t.Fatal(err)
		}
		document[field] = value
		payload, err := json.Marshal(document)
		if err != nil {
			t.Fatal(err)
		}
		return payload
	}
	for _, test := range []struct {
		name      string
		execution Execution
		runErr    error
		verdict   string
		noSpeech  bool
	}{
		{name: "stderr", execution: Execution{ExitCode: 1, Stderr: []byte(sentinel)}},
		{name: "no speech", execution: Execution{ExitCode: 1, Stderr: []byte("no speech found: " + sentinel)}, noSpeech: true},
		{name: "reason", execution: Execution{ExitCode: 2, Stdout: report("why", sentinel)}, verdict: "unsure"},
		{name: "nothing", execution: Execution{ExitCode: 3, Stdout: fixture(t, "nothing.json")}, verdict: "nothing"},
		{name: "unsure kept back", execution: Execution{ExitCode: 3, Stdout: fixture(t, "unsure.json")}, verdict: "unsure"},
		{name: "mode", execution: Execution{Stdout: report("mode", sentinel)}},
		{name: "unknown field", execution: Execution{Stdout: report(sentinel, "value")}},
		{name: "runner", runErr: errors.New(sentinel)},
	} {
		t.Run(test.name, func(t *testing.T) {
			lapse := newTestLapse(t, runnerFunc(func(context.Context, Command) (Execution, error) { return test.execution, test.runErr }), filepath.Dir(media))
			for _, synchronize := range []bool{false, true} {
				var err error
				if synchronize {
					_, err = lapse.Synchronize(context.Background(), media, subtitle, filepath.Join(t.TempDir(), "output.srt"))
				} else {
					_, err = lapse.Analyze(context.Background(), media, subtitle)
				}
				if err == nil || strings.Contains(err.Error(), "private-subtitle") {
					t.Fatalf("unsafe or absent error: %v", err)
				}
				var noSpeech *NoSpeechError
				var verdict *VerdictError
				var processExit *ProcessExitError
				if errors.As(err, &noSpeech) != test.noSpeech || test.noSpeech && errors.As(err, &processExit) || errors.As(err, &verdict) != (test.verdict != "") || verdict != nil && verdict.Verdict != test.verdict {
					t.Fatalf("wrong failure classification: %T %v", err, err)
				}
				if test.name == "nothing" && (verdict.Result.Reference != "vad" || verdict.Result.Confidence != 0.11 || verdict.Result.Coverage != 0.4 || verdict.Result.Agreement != 0.125 || verdict.Result.Sigma != 1.2 || verdict.Result.Margin != 0.01) {
					t.Fatalf("verdict error dropped LAPSE metrics: %#v", verdict.Result)
				}
			}
		})
	}
}

func newTestLapse(t *testing.T, runner Runner, mediaRoot string) *Lapse {
	t.Helper()
	lapse, err := New(Options{Path: "/usr/local/bin/lapse", CacheDir: filepath.Join(t.TempDir(), "speech-cache"), AnalyzeTimeout: time.Second, SynchronizeTimeout: time.Second, MediaRoots: []string{mediaRoot}, Runner: runner})
	if err != nil {
		t.Fatal(err)
	}
	return lapse
}

func testFiles(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	media := filepath.Join(root, "Movie Name.mkv")
	subtitle := filepath.Join(root, "Movie Name.hr.srt")
	if err := os.WriteFile(media, []byte("media"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(subtitle, []byte(testSRT), 0o600); err != nil {
		t.Fatal(err)
	}
	return media, subtitle
}

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	payload, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return payload
}

func reportJSON(t *testing.T, verdict, output string, written bool) []byte {
	t.Helper()
	var value map[string]any
	if err := json.Unmarshal(fixture(t, "solid.json"), &value); err != nil {
		t.Fatal(err)
	}
	value["verdict"] = verdict
	value["output"] = output
	value["written"] = written
	payload, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return payload
}

func bytesReplacingPath(payload []byte, old, replacement string) []byte {
	return []byte(strings.ReplaceAll(string(payload), old, replacement))
}

type runnerFunc func(context.Context, Command) (Execution, error)

func (function runnerFunc) Run(ctx context.Context, command Command) (Execution, error) {
	return function(ctx, command)
}

func TestTimeoutScalesWithMediaSize(t *testing.T) {
	tests := []struct {
		name      string
		mediaSize int64
		rate      int64
		want      time.Duration
	}{
		{"small media keeps the configured timeout", 1 << 20, 1 << 20, time.Minute},
		{"large media gets size divided by read rate", 300 << 20, 1 << 20, 300 * time.Second},
		{"no read rate keeps the configured timeout", 300 << 20, 0, time.Minute},
		{"scaling stops at the maximum", 1 << 30, 1 << 20, 10 * time.Minute},
		{"a tiny rate cannot overflow past the maximum", 1 << 40, 1, 10 * time.Minute},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			media, subtitle := testFiles(t)
			if err := os.Truncate(media, test.mediaSize); err != nil {
				t.Fatal(err)
			}
			var got time.Duration
			runner := runnerFunc(func(ctx context.Context, _ Command) (Execution, error) {
				deadline, ok := ctx.Deadline()
				if !ok {
					t.Fatal("LAPSE context has no deadline")
				}
				got = time.Until(deadline)
				return Execution{}, context.DeadlineExceeded
			})
			lapse, err := New(Options{Path: "/usr/local/bin/lapse", CacheDir: filepath.Join(t.TempDir(), "speech-cache"), AnalyzeTimeout: time.Minute, SynchronizeTimeout: time.Minute, MinReadRate: test.rate, MaxTimeout: 10 * time.Minute, MediaRoots: []string{filepath.Dir(media)}, Runner: runner})
			if err != nil {
				t.Fatal(err)
			}
			_, _ = lapse.Synchronize(context.Background(), media, subtitle, filepath.Join(t.TempDir(), "output.srt"))
			if got > test.want || got < test.want-5*time.Second {
				t.Fatalf("timeout = %v, want about %v", got, test.want)
			}
		})
	}
}

func TestTimeoutIsTypedAndParentCancellationIsNot(t *testing.T) {
	media, subtitle := testFiles(t)
	lapse := newTestLapse(t, runnerFunc(func(context.Context, Command) (Execution, error) {
		return Execution{}, context.DeadlineExceeded
	}), filepath.Dir(media))
	_, err := lapse.Analyze(context.Background(), media, subtitle)
	var timeout *TimeoutError
	if !errors.As(err, &timeout) || !errors.Is(err, context.DeadlineExceeded) || timeout.Limit != time.Second {
		t.Fatalf("Analyze() error = %T %v, want TimeoutError after 1s", err, err)
	}

	parent, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	<-parent.Done()
	_, err = lapse.Analyze(parent, media, subtitle)
	if errors.As(err, &timeout) {
		t.Fatalf("Analyze() with an expired parent = %v, want no TimeoutError", err)
	}
}

// LAPSE 2.3.1 counts splits by the cues in the file, so every split is below
// "cues". The lapse#102 report from 2.3.0 ("cues":201, a split at 202, from a
// 203-cue subtitle) is refused as a technical failure, never a verdict, at the
// exit 2 a 2.3.0 binary used and the exit 3 of 2.3.1.
func TestSynchronizeRefusesASplitPastTheLastCue(t *testing.T) {
	media, subtitle := testFiles(t)
	for _, exitCode := range []int{2, 3} {
		t.Run(fmt.Sprintf("exit %d", exitCode), func(t *testing.T) {
			lapse := newTestLapse(t, runnerFunc(func(context.Context, Command) (Execution, error) {
				return Execution{Stdout: fixture(t, "nothing-recut.json"), ExitCode: exitCode}, nil
			}), filepath.Dir(media))
			_, err := lapse.Synchronize(context.Background(), media, subtitle, filepath.Join(t.TempDir(), "output.srt"))
			var processExit *ProcessExitError
			if !errors.As(err, &processExit) || processExit.Report != "LAPSE JSON contains invalid split positions" || processExit.ExitCode != exitCode || processExit.StdoutEmpty {
				t.Fatalf("Synchronize() error = %T %#v, want a refused split report at exit %d", err, err, exitCode)
			}
			var verdict *VerdictError
			if errors.As(err, &verdict) {
				t.Fatalf("Synchronize() error = %v, want no verdict", err)
			}
		})
	}
}

// A solid report with a split past the last cue is protocol data LAPSE
// cannot produce: it is neither a verdict nor a process failure, and the
// subtitle LAPSE already wrote is removed instead of reaching installation.
func TestSynchronizeRemovesSolidOutputWithASplitPastTheLastCue(t *testing.T) {
	media, subtitle := testFiles(t)
	output := filepath.Join(t.TempDir(), "synced.srt")
	// json.Marshal sorts the keys, so these fields keep their fixture spelling.
	report := strings.Replace(strings.Replace(string(reportJSON(t, "solid", output, true)), `"parts":1`, `"parts":2`, 1), `"splits":[]`, `"splits":[1578]`, 1)
	lapse := newTestLapse(t, runnerFunc(func(context.Context, Command) (Execution, error) {
		if err := os.WriteFile(output, []byte(testSRT), 0o600); err != nil {
			t.Fatal(err)
		}
		return Execution{Stdout: []byte(report), ExitCode: 0}, nil
	}), filepath.Dir(media))
	_, err := lapse.Synchronize(context.Background(), media, subtitle, output)
	var processExit *ProcessExitError
	var verdict *VerdictError
	if err == nil || errors.As(err, &processExit) || errors.As(err, &verdict) {
		t.Fatalf("Synchronize() error = %T %v, want a protocol error", err, err)
	}
	if _, statErr := os.Stat(output); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("output after refusal: %v, want it removed", statErr)
	}
}

func TestSynchronizeReadsARecutVerdict(t *testing.T) {
	media, subtitle := testFiles(t)
	lapse := newTestLapse(t, runnerFunc(func(context.Context, Command) (Execution, error) {
		return Execution{Stdout: fixture(t, "nothing-recut-v2.3.json"), ExitCode: 3}, nil
	}), filepath.Dir(media))
	_, err := lapse.Synchronize(context.Background(), media, subtitle, filepath.Join(t.TempDir(), "output.srt"))
	var verdict *VerdictError
	if !errors.As(err, &verdict) || verdict.Verdict != "nothing" || verdict.Result.Splits != 2 {
		t.Fatalf("Synchronize() error = %T %v, want nothing verdict with two splits", err, err)
	}
}

func TestRefusedReportNamesTheFailedCheckOnly(t *testing.T) {
	media, subtitle := testFiles(t)
	payload := strings.Replace(string(fixture(t, "nothing-recut.json")), `"splits":[107,202]`, `"splits":[202,107]`, 1)
	lapse := newTestLapse(t, runnerFunc(func(context.Context, Command) (Execution, error) {
		return Execution{Stdout: []byte(payload), ExitCode: 2}, nil
	}), filepath.Dir(media))
	_, err := lapse.Synchronize(context.Background(), media, subtitle, filepath.Join(t.TempDir(), "output.srt"))
	var processExit *ProcessExitError
	if !errors.As(err, &processExit) || processExit.Report != "LAPSE JSON contains invalid split positions" {
		t.Fatalf("Synchronize() error = %T %v", err, err)
	}
	if message := err.Error(); !strings.Contains(message, "invalid split positions") || strings.Contains(message, "audio does not back") || strings.Contains(message, "/tmp/out") {
		t.Fatalf("error = %q", message)
	}

	lapse = newTestLapse(t, runnerFunc(func(context.Context, Command) (Execution, error) {
		return Execution{Stdout: []byte(`{"mode":"auto/recut","secret":"Hello"}`), ExitCode: 2}, nil
	}), filepath.Dir(media))
	_, err = lapse.Synchronize(context.Background(), media, subtitle, filepath.Join(t.TempDir(), "output.srt"))
	if !errors.As(err, &processExit) || processExit.Report != "unknown JSON field" || strings.Contains(err.Error(), "secret") {
		t.Fatalf("unknown field error = %T %v", err, err)
	}
}
