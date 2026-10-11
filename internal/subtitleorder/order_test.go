package subtitleorder

import (
	"errors"
	"testing"
	"time"
)

func TestReorderSortsRecordsForEachFormat(t *testing.T) {
	tests := map[string]struct {
		extension string
		in, want  string
		starts    []time.Duration
	}{
		"srt with byte order mark":      {".srt", "\ufeff1\n00:00:02,000 --> 00:00:03,000\nB\n\n2\n00:00:01,000 --> 00:00:02,000\nA\n", "\ufeff1\n00:00:01,000 --> 00:00:02,000\nA\n\n2\n00:00:02,000 --> 00:00:03,000\nB\n", []time.Duration{2 * time.Second, time.Second}},
		"vtt keeps its header":          {".vtt", "WEBVTT\n\n00:00:02.000 --> 00:00:03.000\nB\n\n00:00:01.000 --> 00:00:02.000\nA\n", "WEBVTT\n\n00:00:01.000 --> 00:00:02.000\nA\n\n00:00:02.000 --> 00:00:03.000\nB\n", []time.Duration{2 * time.Second, time.Second}},
		"ass sorts dialogue lines only": {".ass", "[Events]\nFormat: Layer, Start, End, Text\nDialogue: 0,0:00:02.00,0:00:03.00,B\nDialogue: 0,0:00:01.00,0:00:02.00,A\n", "[Events]\nFormat: Layer, Start, End, Text\nDialogue: 0,0:00:01.00,0:00:02.00,A\nDialogue: 0,0:00:02.00,0:00:03.00,B\n", []time.Duration{2 * time.Second, time.Second}},
	}
	for name, test := range tests {
		got, err := Reorder([]byte(test.in), test.extension, test.starts)
		if err != nil || string(got) != test.want {
			t.Errorf("%s: Reorder() = %q, %v\nwant %q", name, got, err, test.want)
		}
	}
}

func TestReorderLeavesOrderedTextUntouchedAndRefusesMismatches(t *testing.T) {
	ordered := "1\r\n00:00:01,000 --> 00:00:02,000\r\nA\r\n"
	if got, err := Reorder([]byte(ordered), ".srt", []time.Duration{time.Second}); err != nil || string(got) != ordered {
		t.Fatalf("ordered text changed: %q, %v", got, err)
	}
	_, err := Reorder([]byte("1\n00:00:02,000 --> 00:00:03,000\nB\n"), ".srt", []time.Duration{2 * time.Second, time.Second})
	if !errors.Is(err, ErrUnsafe) {
		t.Fatalf("error = %v; want ErrUnsafe when cues and records disagree", err)
	}
}
