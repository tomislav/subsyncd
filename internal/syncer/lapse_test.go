package syncer

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
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
		{"parts mismatch", strings.Replace(string(fixture(t, "solid.json")), `"parts":1`, `"parts":2`, 1)},
		{"unordered split", ""},
	}
	// Construct the split case separately to keep the fixture mutation clear.
	tests[len(tests)-1].payload = strings.Replace(strings.Replace(string(fixture(t, "solid.json")), `"parts":1`, `"parts":3`, 1), `"splits":[]`, `"splits":[10,9]`, 1)
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := parseReport([]byte(test.payload)); err == nil {
				t.Fatal("parseReport() error = nil")
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

	for _, test := range []struct {
		name   string
		write  string
		stdout []byte
		exit   int
	}{
		{"missing output", "", reportJSON(t, "solid", output, true), 0},
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
