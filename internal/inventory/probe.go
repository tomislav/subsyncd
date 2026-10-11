package inventory

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"
	"unicode"

	"subsyncd/internal/domain"
)

const (
	maxProbeOutput = 8 << 20
	maxProbeStderr = 4 << 10
)

type CommandRunner interface {
	Run(context.Context, string, ...string) (stdout, stderr []byte, err error)
}

// defaultProbeTimeout bounds one ffprobe run. Reading stream headers takes
// seconds even over a network mount; minutes mean the mount stopped answering.
const defaultProbeTimeout = 5 * time.Minute

// ErrProbeTimedOut means ffprobe did not finish reading the media file in
// time, which is how a hung mount looks; the media is unavailable for now.
var ErrProbeTimedOut = errors.New("ffprobe timed out reading the media file")

// maxStuckProbes is how many timed-out ffprobe runs may still be blocked at
// once. Each holds a process and a goroutine until its read returns, so
// beyond this no new run starts and every probe times out at once.
const maxStuckProbes = 4

// stuckProbes counts the timed-out ffprobe runs still blocked, per media
// path, so a retry on a hung mount does not start another one beside them.
var stuckProbes = &probeRegistry{paths: map[string]int{}}

type probeRegistry struct {
	mu    sync.Mutex
	paths map[string]int
	total int
}

// probeRun is one ffprobe run; its fields are guarded by the registry.
type probeRun struct {
	finished, abandoned bool
}

// blocked reports whether a new run must not start: one for mediaPath is
// still blocked, or too many are.
func (r *probeRegistry) blocked(mediaPath string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.paths[mediaPath] > 0 || r.total >= maxStuckProbes
}

// abandon records a run that is no longer awaited, unless it already ended.
func (r *probeRegistry) abandon(run *probeRun, mediaPath string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if run.finished || run.abandoned {
		return
	}
	run.abandoned = true
	r.paths[mediaPath]++
	r.total++
}

// finish records that a run's process returned.
func (r *probeRegistry) finish(run *probeRun, mediaPath string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	run.finished = true
	if !run.abandoned {
		return
	}
	r.total--
	if r.paths[mediaPath]--; r.paths[mediaPath] <= 0 {
		delete(r.paths, mediaPath)
	}
}

type Probe struct {
	Path   string
	Runner CommandRunner
	// Timeout bounds one run; 0 uses defaultProbeTimeout.
	Timeout time.Duration
}

func (p Probe) Tracks(ctx context.Context, mediaPath string) ([]Track, error) {
	timeout := p.Timeout
	if timeout <= 0 {
		timeout = defaultProbeTimeout
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if stuckProbes.blocked(mediaPath) {
		return nil, ErrProbeTimedOut
	}
	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	type output struct {
		stdout []byte
		err    error
	}
	// A process blocked in a read of a hung mount cannot be killed until the
	// read returns, so the result is awaited only until the deadline; a late
	// run finishes in the background.
	done := make(chan output, 1)
	run := &probeRun{}
	go func() {
		stdout, _, err := p.Runner.Run(probeCtx, p.Path, "-v", "error", "-show_streams", "-show_format", "-of", "json", mediaPath)
		stuckProbes.finish(run, mediaPath)
		done <- output{stdout, err}
	}()
	var stdout []byte
	select {
	case result := <-done:
		stdout = result.stdout
		if result.err != nil {
			if ctx.Err() == nil && probeCtx.Err() != nil {
				return nil, ErrProbeTimedOut
			}
			return nil, fmt.Errorf("ffprobe failed: %w", result.err)
		}
	case <-probeCtx.Done():
		stuckProbes.abandon(run, mediaPath)
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return nil, ErrProbeTimedOut
	}
	if len(stdout) > maxProbeOutput {
		return nil, fmt.Errorf("ffprobe output exceeds %d bytes", maxProbeOutput)
	}
	return ParseProbeTracks(stdout)
}

type probeDocument struct {
	Streams []struct {
		Index     int    `json:"index"`
		CodecName string `json:"codec_name"`
		CodecType string `json:"codec_type"`
		Tags      struct {
			Language string `json:"language"`
			Title    string `json:"title"`
		} `json:"tags"`
		Disposition struct {
			Default         int `json:"default"`
			Forced          int `json:"forced"`
			HearingImpaired int `json:"hearing_impaired"`
		} `json:"disposition"`
	} `json:"streams"`
}

func ParseProbeTracks(payload []byte) ([]Track, error) {
	if len(payload) > maxProbeOutput {
		return nil, fmt.Errorf("ffprobe output exceeds %d bytes", maxProbeOutput)
	}
	var document probeDocument
	decoder := json.NewDecoder(bytes.NewReader(payload))
	if err := decoder.Decode(&document); err != nil {
		return nil, fmt.Errorf("decode ffprobe output: %w", err)
	}
	tracks := make([]Track, 0, len(document.Streams))
	for _, stream := range document.Streams {
		if stream.CodecType != "subtitle" {
			continue
		}
		var language domain.Language
		if parsed, err := domain.ParseLanguage(stream.Tags.Language); err == nil {
			language = parsed
		}
		title := strings.ToLower(stream.Tags.Title)
		sdh := stream.Disposition.HearingImpaired != 0 || strings.Contains(title, "sdh") || strings.Contains(title, "hearing impaired") || strings.Contains(title, "closed caption")
		tracks = append(tracks, Track{
			Index:    stream.Index,
			Language: language,
			Codec:    stream.CodecName,
			Embedded: true,
			Forced:   stream.Disposition.Forced != 0 || forcedTrackTitle(title),
			Default:  stream.Disposition.Default != 0,
			SDH:      sdh,
		})
	}
	return tracks, nil
}

// Track titles are labels, not reliable prose. Require a complete marker and
// honor explicit negation/removal; title evidence can never clear a disposition.
func forcedTrackTitle(title string) bool {
	clauses := strings.FieldsFunc(title, func(r rune) bool {
		return strings.ContainsRune(";,\n()[]{}|", r)
	})
	for _, clause := range clauses {
		words := strings.FieldsFunc(clause, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
		negative := false
		for i, word := range words {
			switch word {
			case "but", "with":
				negative = false
			case "no", "not", "non", "without", "remove", "removed", "exclude", "excluded", "strip", "stripped":
				negative = true
			case "forced":
				if negative {
					continue
				}
				removed := false
			suffix:
				for _, suffix := range words[i+1:] {
					switch suffix {
					case "with", "without", "but", "and", "or", "no", "not", "non":
						break suffix
					case "removed", "excluded", "stripped", "free":
						removed = true
					}
				}
				if !removed {
					return true
				}
			}
		}
	}
	return false
}

type OSCommandRunner struct{}

func (OSCommandRunner) Run(ctx context.Context, name string, args ...string) ([]byte, []byte, error) {
	command := exec.CommandContext(ctx, name, args...)
	stdout := &boundedBuffer{limit: maxProbeOutput + 1}
	stderr := &boundedBuffer{limit: maxProbeStderr}
	command.Stdout = stdout
	command.Stderr = stderr
	err := command.Run()
	return stdout.Bytes(), stderr.Bytes(), err
}

type boundedBuffer struct {
	buffer bytes.Buffer
	limit  int
}

func (b *boundedBuffer) Write(payload []byte) (int, error) {
	original := len(payload)
	remaining := b.limit - b.buffer.Len()
	if remaining > 0 {
		if len(payload) > remaining {
			payload = payload[:remaining]
		}
		_, _ = b.buffer.Write(payload)
	}
	return original, nil
}

func (b *boundedBuffer) Bytes() []byte { return b.buffer.Bytes() }
