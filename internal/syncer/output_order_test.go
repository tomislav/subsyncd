package syncer

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSynchronizeOrdersCuesWithoutChangingTimingOrText(t *testing.T) {
	media, source := testFiles(t)
	output := filepath.Join(t.TempDir(), "output.srt")
	body := "1\n00:21:51,355 --> 00:21:54,355\n<i>Later</i>\n\n2\n00:21:50,300 --> 00:21:53,300\nEarlier\nSecond line\n\n3\n00:21:51,355 --> 00:21:55,000\nSame start\n"
	calls := 0
	lapse := newTestLapse(t, runnerFunc(func(context.Context, Command) (Execution, error) {
		calls++
		if err := os.WriteFile(output, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		return Execution{Stdout: reportJSON(t, "solid", output, true)}, nil
	}), filepath.Dir(media))
	result, err := lapse.Synchronize(t.Context(), media, source, output)
	if err != nil || result.Verdict != "solid" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	got, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	want := "1\n00:21:50,300 --> 00:21:53,300\nEarlier\nSecond line\n\n2\n00:21:51,355 --> 00:21:54,355\n<i>Later</i>\n\n3\n00:21:51,355 --> 00:21:55,000\nSame start\n"
	if string(got) != want || calls != 1 {
		t.Fatalf("output=%q calls=%d", got, calls)
	}
	original, _ := os.ReadFile(source)
	if string(original) != testSRT {
		t.Fatal("source changed")
	}
	if err := validateOutput(output); err != nil {
		t.Fatal(err)
	}
}

func TestSynchronizeStillRejectsReversedIntervals(t *testing.T) {
	media, source := testFiles(t)
	output := filepath.Join(t.TempDir(), "output.srt")
	lapse := newTestLapse(t, runnerFunc(func(context.Context, Command) (Execution, error) {
		_ = os.WriteFile(output, []byte("1\n00:00:03,000 --> 00:00:02,000\nBad\n"), 0600)
		return Execution{Stdout: reportJSON(t, "solid", output, true)}, nil
	}), filepath.Dir(media))
	_, err := lapse.Synchronize(t.Context(), media, source, output)
	var invalid *InvalidOutputError
	if !errors.As(err, &invalid) {
		t.Fatalf("error=%v", err)
	}
	if _, err := os.Stat(output); !os.IsNotExist(err) {
		t.Fatal("failed output retained")
	}
}

func TestSynchronizeLeavesOrderedOutputUntouched(t *testing.T) {
	media, source := testFiles(t)
	output := filepath.Join(t.TempDir(), "output.srt")
	body := strings.ReplaceAll(testSRT, "\n", "\r\n")
	lapse := newTestLapse(t, runnerFunc(func(context.Context, Command) (Execution, error) {
		_ = os.WriteFile(output, []byte(body), 0600)
		return Execution{Stdout: reportJSON(t, "solid", output, true)}, nil
	}), filepath.Dir(media))
	if _, err := lapse.Synchronize(t.Context(), media, source, output); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(output)
	if string(got) != body {
		t.Fatal("ordered bytes changed")
	}
}

func TestNormalizeOutputFormats(t *testing.T) {
	for _, ext := range []string{".srt", ".vtt", ".ass", ".ssa"} {
		t.Run(ext, func(t *testing.T) {
			body := "1\n00:00:03,000 --> 00:00:04,000\nLater\n\n2\n00:00:01,000 --> 00:00:02,000\nEarlier\n"
			if ext == ".vtt" {
				body = "WEBVTT\n\nlate-id\n00:00:03.000 --> 00:00:04.000 align:start\nLater\n\nearly-id\n00:00:01.000 --> 00:00:02.000\nEarlier\n"
			}
			if ext == ".ass" || ext == ".ssa" {
				body = "[Script Info]\nScriptType: v4.00+\n\n[Events]\nFormat: Layer, Start, End, Style, Name, MarginL, MarginR, MarginV, Effect, Text\nDialogue: 0,0:00:03.00,0:00:04.00,Default,,0,0,0,,{\\i1}Later\nDialogue: 0,0:00:01.00,0:00:02.00,Default,,0,0,0,,Earlier\n"
			}
			path := filepath.Join(t.TempDir(), "output"+ext)
			if err := os.WriteFile(path, []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
			if err := normalizeOutputOrder(path); err != nil {
				t.Fatal(err)
			}
			if err := validateOutput(path); err != nil {
				t.Fatal(err)
			}
			got, _ := os.ReadFile(path)
			if strings.Index(string(got), "Earlier") >= strings.Index(string(got), "Later") {
				t.Fatalf("cues not sorted: %s", got)
			}
			if ext == ".vtt" && (!strings.Contains(string(got), "late-id\n00:00:03.000 --> 00:00:04.000 align:start") || !strings.HasPrefix(string(got), "WEBVTT")) {
				t.Fatalf("VTT metadata changed: %s", got)
			}
			if (ext == ".ass" || ext == ".ssa") && !strings.Contains(string(got), "{\\i1}Later") {
				t.Fatal("styling lost")
			}
		})
	}
}

func TestNormalizeOutputRejectsChangingASSFormat(t *testing.T) {
	body := "[Script Info]\nScriptType: v4.00+\n[Events]\nFormat: Layer, Start, End, Style, Name, MarginL, MarginR, MarginV, Effect, Text\nDialogue: 0,0:00:03.00,0:00:04.00,Default,Alice,0,0,0,,Later\nFormat : Layer, Start, End, Name, Style, MarginL, MarginR, MarginV, Effect, Text\nDialogue: 0,0:00:01.00,0:00:02.00,Bob,Default,0,0,0,,Earlier\n"
	path := filepath.Join(t.TempDir(), "output.ass")
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	if err := normalizeOutputOrder(path); err == nil {
		t.Fatal("changing event formats must not reorder")
	}
	got, _ := os.ReadFile(path)
	if string(got) != body {
		t.Fatal("unsafe output changed")
	}
}
