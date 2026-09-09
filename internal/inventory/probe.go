package inventory

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
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

type Probe struct {
	Path   string
	Runner CommandRunner
}

func (p Probe) Tracks(ctx context.Context, mediaPath string) ([]Track, error) {
	stdout, _, err := p.Runner.Run(ctx, p.Path, "-v", "error", "-show_streams", "-show_format", "-of", "json", mediaPath)
	if err != nil {
		return nil, fmt.Errorf("ffprobe failed: %w", err)
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
