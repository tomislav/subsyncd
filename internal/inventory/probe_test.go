package inventory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestForcedTrackTitleCannotSatisfyFullSubtitleCoverage(t *testing.T) {
	for _, tt := range []struct {
		title  string
		flag   int
		forced bool
	}{
		{"English [Forced]", 0, true},
		{"FORCED ONLY", 0, true},
		{"English.forced", 0, true},
		{"English (forced narrative)", 0, true},
		{"English", 0, false},
		{"English reinforced", 0, false},
		{"English unforced", 0, false},
		{"English forced2", 0, false},
		{"English non-forced", 0, false},
		{"English not forced", 0, false},
		{"English without forced subtitles", 0, false},
		{"English forced subtitles removed", 0, false},
		{"English forced-free", 0, false},
		{"No SDH but forced", 0, true},
		{"English not forced; French forced", 0, true},
		{"English (No SDH) [Forced]", 0, true},
		{"English forced (SDH removed)", 0, true},
		{"English not forced", 1, true},
	} {
		t.Run(tt.title, func(t *testing.T) {
			title, _ := json.Marshal(tt.title)
			payload := `{"streams":[{"codec_type":"subtitle","tags":{"language":"en","title":` + string(title) + `},"disposition":{"forced":` + fmt.Sprint(tt.flag) + `}}]}`
			tracks, err := ParseProbeTracks([]byte(payload))
			if err != nil {
				t.Fatal(err)
			}
			if len(tracks) != 1 || tracks[0].Forced != tt.forced {
				t.Fatalf("tracks = %#v, forced want %v", tracks, tt.forced)
			}
			if (Inventory{Tracks: tracks}).Satisfies("en", true) == tt.forced {
				t.Fatalf("full coverage must exclude forced tracks: %#v", tracks)
			}
		})
	}
}

func TestParseProbeTracksNormalizesLanguagesFlagsAndImageTracks(t *testing.T) {
	payload, err := os.ReadFile(filepath.Join("testdata", "ffprobe_streams.json"))
	if err != nil {
		t.Fatal(err)
	}
	tracks, err := ParseProbeTracks(payload)
	if err != nil {
		t.Fatal(err)
	}
	if len(tracks) != 6 {
		t.Fatalf("track count = %d, want 6", len(tracks))
	}
	if tracks[0].Language != "en" || !tracks[0].Default || tracks[0].Codec != "subrip" {
		t.Fatalf("English track = %#v", tracks[0])
	}
	if tracks[1].Language != "hr" || !tracks[1].SDH {
		t.Fatalf("Croatian SDH track = %#v", tracks[1])
	}
	if tracks[2].Language != "hr" || !tracks[2].Forced {
		t.Fatalf("forced Croatian track = %#v", tracks[2])
	}
	if tracks[3].Codec != "hdmv_pgs_subtitle" || tracks[3].Language != "en" {
		t.Fatalf("PGS track = %#v", tracks[3])
	}
	if tracks[4].Language != "" || tracks[5].Language != "" {
		t.Fatalf("unknown tracks should remain inventoried without a language: %#v", tracks[4:])
	}
}

type fakeRunner struct {
	stdout []byte
	stderr []byte
	err    error
	name   string
	args   []string
	calls  int
}

func (f *fakeRunner) Run(_ context.Context, name string, args ...string) ([]byte, []byte, error) {
	f.calls++
	f.name = name
	f.args = append([]string(nil), args...)
	return f.stdout, f.stderr, f.err
}

func TestProbeRunsApprovedCommandAndRedactsBoundedStderr(t *testing.T) {
	runner := &fakeRunner{err: errors.New("exit 1"), stderr: []byte(strings.Repeat("secret ", 2000))}
	probe := Probe{Path: "ffprobe", Runner: runner}
	_, err := probe.Tracks(context.Background(), "/media/show.mkv")
	if err == nil {
		t.Fatal("expected probe error")
	}
	if runner.name != "ffprobe" || strings.Join(runner.args, " ") != "-v error -show_streams -show_format -of json /media/show.mkv" {
		t.Fatalf("command = %q %#v", runner.name, runner.args)
	}
	if strings.Contains(err.Error(), "secret") || len(err.Error()) > 1024 {
		t.Fatalf("stderr was not redacted and bounded: %q", err)
	}
}

func TestProbeRejectsOversizedOutput(t *testing.T) {
	runner := &fakeRunner{stdout: make([]byte, maxProbeOutput+1)}
	_, err := (Probe{Path: "ffprobe", Runner: runner}).Tracks(context.Background(), "/media/show.mkv")
	if err == nil {
		t.Fatal("expected oversized-output error")
	}
}
