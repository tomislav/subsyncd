package inventory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
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

// hungRunner never returns, like ffprobe blocked in a read of a hung mount.
type hungRunner struct{ release chan struct{} }

func (h hungRunner) Run(context.Context, string, ...string) ([]byte, []byte, error) {
	<-h.release
	return nil, nil, errors.New("released")
}

func TestProbeGivesUpOnAHungMount(t *testing.T) {
	runner := hungRunner{release: make(chan struct{})}
	defer close(runner.release)
	_, err := (Probe{Path: "ffprobe", Runner: runner, Timeout: 20 * time.Millisecond}).Tracks(context.Background(), "/media/show.mkv")
	if !errors.Is(err, ErrProbeTimedOut) {
		t.Fatalf("error = %v; want ErrProbeTimedOut", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := (Probe{Path: "ffprobe", Runner: runner, Timeout: time.Minute}).Tracks(ctx, "/media/show.mkv"); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v; a canceled job is not a probe timeout", err)
	}
}

// countingHungRunner counts the ffprobe processes it starts; each blocks until
// released.
type countingHungRunner struct {
	mu      sync.Mutex
	starts  int
	release chan struct{}
}

func (h *countingHungRunner) Run(context.Context, string, ...string) ([]byte, []byte, error) {
	h.mu.Lock()
	h.starts++
	h.mu.Unlock()
	<-h.release
	return nil, nil, errors.New("released")
}

func (h *countingHungRunner) started() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.starts
}

func waitForNoStuckProbes(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		stuckProbes.mu.Lock()
		total := len(stuckProbes.paths)
		stuckProbes.mu.Unlock()
		if total == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d stuck probes remain", total)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestStuckProbeIsNotStartedAgain(t *testing.T) {
	waitForNoStuckProbes(t)
	runner := &countingHungRunner{release: make(chan struct{})}
	probe := Probe{Path: "ffprobe", Runner: runner, Timeout: 10 * time.Millisecond}
	for range 3 {
		if _, err := probe.Tracks(context.Background(), "/media/stuck.mkv"); !errors.Is(err, ErrProbeTimedOut) {
			t.Fatalf("error = %v; want ErrProbeTimedOut", err)
		}
	}
	if got := runner.started(); got != 1 {
		t.Fatalf("ffprobe started %d times; want one while the first is still blocked", got)
	}
	close(runner.release)
	waitForNoStuckProbes(t)
	// Once the blocked run returns, the path is probed again.
	healthy := Probe{Path: "ffprobe", Runner: &fakeRunner{stdout: []byte(`{"streams":[]}`)}}
	if _, err := healthy.Tracks(context.Background(), "/media/stuck.mkv"); err != nil {
		t.Fatalf("probe after release: %v", err)
	}
}

func TestTooManyStuckProbesStopNewOnes(t *testing.T) {
	waitForNoStuckProbes(t)
	runner := &countingHungRunner{release: make(chan struct{})}
	probe := Probe{Path: "ffprobe", Runner: runner, Timeout: 10 * time.Millisecond}
	for i := range maxStuckProbes + 2 {
		if _, err := probe.Tracks(context.Background(), fmt.Sprintf("/media/hung-%d.mkv", i)); !errors.Is(err, ErrProbeTimedOut) {
			t.Fatalf("error = %v; want ErrProbeTimedOut", err)
		}
	}
	if got := runner.started(); got != maxStuckProbes {
		t.Fatalf("ffprobe started %d times; want at most %d stuck", got, maxStuckProbes)
	}
	close(runner.release)
	waitForNoStuckProbes(t)
}

func TestStuckProbesOnOneMountDoNotBlockOthers(t *testing.T) {
	waitForNoStuckProbes(t)
	runner := &countingHungRunner{release: make(chan struct{})}
	roots := []string{"/media/nfs", "/media/local"}
	hung := Probe{Path: "ffprobe", Runner: runner, Timeout: 10 * time.Millisecond, MediaRoots: roots}
	for i := range maxStuckProbes {
		if _, err := hung.Tracks(context.Background(), fmt.Sprintf("/media/nfs/show/hung-%d.mkv", i)); !errors.Is(err, ErrProbeTimedOut) {
			t.Fatalf("error = %v; want ErrProbeTimedOut", err)
		}
	}
	if _, err := hung.Tracks(context.Background(), "/media/nfs/show/next.mkv"); !errors.Is(err, ErrProbeTimedOut) || runner.started() != maxStuckProbes {
		t.Fatalf("error = %v, started = %d; want the hung mount blocked", err, runner.started())
	}
	healthy := Probe{Path: "ffprobe", Runner: &fakeRunner{stdout: []byte(`{"streams":[]}`)}, MediaRoots: roots}
	for _, path := range []string{"/media/local/movie.mkv", "/srv/other/movie.mkv"} {
		if _, err := healthy.Tracks(context.Background(), path); err != nil {
			t.Errorf("%s: probe on a healthy mount = %v", path, err)
		}
	}
	close(runner.release)
	waitForNoStuckProbes(t)
}

func TestCanceledProbeIsNotCountedAsStuck(t *testing.T) {
	waitForNoStuckProbes(t)
	runner := &countingHungRunner{release: make(chan struct{})}
	defer close(runner.release)
	probe := Probe{Path: "ffprobe", Runner: runner, Timeout: time.Minute}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		for runner.started() == 0 {
			time.Sleep(time.Millisecond)
		}
		cancel()
	}()
	if _, err := probe.Tracks(ctx, "/media/canceled.mkv"); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v; want cancellation", err)
	}
	stuckProbes.mu.Lock()
	stuck := len(stuckProbes.paths) + len(stuckProbes.mounts)
	stuckProbes.mu.Unlock()
	if stuck != 0 {
		t.Fatalf("a canceled probe counted as stuck (%d)", stuck)
	}
}

func TestProbeMountFollowsMediaRoots(t *testing.T) {
	roots := []string{"/media", "/media/nfs"}
	for path, want := range map[string]string{
		"/media/nfs/show/e1.mkv": "/media/nfs",
		"/media/local/movie.mkv": "/media",
		"/data/tv/show/e1.mkv":   "/data/tv",
		"/data/movie.mkv":        "/data",
		"/movie.mkv":             "/",
	} {
		if got := probeMount(path, roots); got != want {
			t.Errorf("probeMount(%q) = %q; want %q", path, got, want)
		}
	}
}
