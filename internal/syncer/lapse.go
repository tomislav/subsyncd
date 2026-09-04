package syncer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/asticode/go-astisub"

	"subsyncd/internal/domain"
)

const (
	defaultTimeout         = 30 * time.Minute
	maximumSubtitleBytes   = 100 << 20
	maximumSubtitleCues    = 100_000
	speechCacheEnvVariable = "LAPSE_CACHE"
)

type Options struct {
	Path               string
	CacheDir           string
	AnalyzeTimeout     time.Duration
	SynchronizeTimeout time.Duration
	MediaRoots         []string
	Runner             Runner
}

type Lapse struct {
	path               string
	cacheDir           string
	analyzeTimeout     time.Duration
	synchronizeTimeout time.Duration
	mediaRoots         []string
	runner             Runner
}

type VerdictError struct {
	Verdict string
	Reason  string
}

func (e *VerdictError) Error() string {
	if e.Reason == "" {
		return "LAPSE rejected subtitle with verdict " + e.Verdict
	}
	return "LAPSE rejected subtitle with verdict " + e.Verdict + ": " + e.Reason
}

type NoSpeechError struct{ Detail string }

func (e *NoSpeechError) Error() string {
	if e.Detail == "" {
		return "LAPSE found no usable speech"
	}
	return "LAPSE found no usable speech: " + e.Detail
}

func New(options Options) (*Lapse, error) {
	if strings.TrimSpace(options.Path) == "" {
		return nil, fmt.Errorf("LAPSE executable path is required")
	}
	if options.Runner == nil {
		options.Runner = OSRunner{}
	}
	if options.AnalyzeTimeout <= 0 {
		options.AnalyzeTimeout = defaultTimeout
	}
	if options.SynchronizeTimeout <= 0 {
		options.SynchronizeTimeout = defaultTimeout
	}
	cacheDir, err := filepath.Abs(options.CacheDir)
	if err != nil || strings.TrimSpace(options.CacheDir) == "" {
		return nil, fmt.Errorf("LAPSE speech cache directory is required")
	}
	if err := os.MkdirAll(cacheDir, 0o750); err != nil {
		return nil, fmt.Errorf("create LAPSE speech cache: %w", err)
	}
	roots := make([]string, 0, len(options.MediaRoots))
	for _, root := range options.MediaRoots {
		absolute, err := filepath.Abs(root)
		if err != nil {
			return nil, fmt.Errorf("resolve media root: %w", err)
		}
		roots = append(roots, filepath.Clean(absolute))
	}
	return &Lapse{path: options.Path, cacheDir: cacheDir, analyzeTimeout: options.AnalyzeTimeout, synchronizeTimeout: options.SynchronizeTimeout, mediaRoots: roots, runner: options.Runner}, nil
}

func (l *Lapse) AnalyzeCandidate(ctx context.Context, candidate domain.Candidate, mediaPath, subtitlePath string) (domain.SyncResult, error) {
	if candidate.ExactHash {
		return domain.SyncResult{Verdict: "exact_hash", Mode: "bypass", Reference: "provider_hash", Ratio: 1, Confidence: 1, Agreement: 1, Coverage: 1, Parts: 1}, nil
	}
	return l.Analyze(ctx, mediaPath, subtitlePath)
}

func (l *Lapse) SynchronizeCandidate(ctx context.Context, candidate domain.Candidate, mediaPath, subtitlePath, outputPath string) (domain.SyncResult, error) {
	if candidate.ExactHash {
		return domain.SyncResult{Verdict: "exact_hash", Mode: "bypass", Reference: "provider_hash", Ratio: 1, Confidence: 1, Agreement: 1, Coverage: 1, Parts: 1}, nil
	}
	return l.Synchronize(ctx, mediaPath, subtitlePath, outputPath)
}

func (l *Lapse) Analyze(ctx context.Context, mediaPath, subtitlePath string) (domain.SyncResult, error) {
	workspace, copiedSubtitle, err := analysisCopy(subtitlePath)
	if err != nil {
		return domain.SyncResult{}, err
	}
	defer os.RemoveAll(workspace)
	arguments := []string{mediaPath, copiedSubtitle, "--dry-run", "--json", "--strict", "--no-sidecar"}
	execution, err := l.execute(ctx, l.analyzeTimeout, Command{Path: l.path, Args: arguments, Dir: workspace, Env: []string{speechCacheEnvVariable + "=" + l.cacheDir}}, mediaPath, subtitlePath, copiedSubtitle)
	if err != nil {
		return domain.SyncResult{}, err
	}
	result, report, err := l.interpret(execution, mediaPath, subtitlePath, copiedSubtitle)
	if err != nil {
		return domain.SyncResult{}, err
	}
	if report.Verdict != "solid" {
		return domain.SyncResult{}, &VerdictError{Verdict: report.Verdict, Reason: l.redact(report.Why, mediaPath, subtitlePath, copiedSubtitle)}
	}
	return result, nil
}

func (l *Lapse) Synchronize(ctx context.Context, mediaPath, subtitlePath, outputPath string) (result domain.SyncResult, err error) {
	if !supportedTextExtension(outputPath) {
		return domain.SyncResult{}, fmt.Errorf("LAPSE output must use a supported text subtitle extension")
	}
	if _, statErr := os.Lstat(outputPath); statErr == nil {
		return domain.SyncResult{}, fmt.Errorf("LAPSE output already exists and will not be overwritten")
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return domain.SyncResult{}, fmt.Errorf("inspect LAPSE output: %w", statErr)
	}
	defer func() {
		if err != nil {
			_ = os.Remove(outputPath)
		}
	}()
	arguments := []string{mediaPath, subtitlePath, "--output", outputPath, "--no-backup", "--json", "--strict", "--no-sidecar"}
	execution, err := l.execute(ctx, l.synchronizeTimeout, Command{Path: l.path, Args: arguments, Dir: filepath.Dir(outputPath), Env: []string{speechCacheEnvVariable + "=" + l.cacheDir}}, mediaPath, subtitlePath, outputPath)
	if err != nil {
		return domain.SyncResult{}, err
	}
	result, report, err := l.interpret(execution, mediaPath, subtitlePath, outputPath)
	if err != nil {
		return domain.SyncResult{}, err
	}
	if report.Verdict != "solid" {
		return domain.SyncResult{}, &VerdictError{Verdict: report.Verdict, Reason: l.redact(report.Why, mediaPath, subtitlePath, outputPath)}
	}
	if !report.Written {
		return domain.SyncResult{}, fmt.Errorf("LAPSE reported a solid result without writing output")
	}
	if !samePath(report.Output, outputPath) {
		return domain.SyncResult{}, fmt.Errorf("LAPSE reported an unexpected output path")
	}
	if err := validateOutput(outputPath); err != nil {
		return domain.SyncResult{}, err
	}
	return result, nil
}

func (l *Lapse) execute(ctx context.Context, timeout time.Duration, command Command, sensitivePaths ...string) (Execution, error) {
	runContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	execution, err := l.runner.Run(runContext, command)
	if err == nil {
		return execution, nil
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return execution, fmt.Errorf("LAPSE command timed out: %w", context.DeadlineExceeded)
	}
	if errors.Is(err, context.Canceled) {
		return execution, fmt.Errorf("LAPSE command canceled: %w", context.Canceled)
	}
	return execution, fmt.Errorf("LAPSE command failed: %s", l.redact(err.Error(), sensitivePaths...))
}

func (l *Lapse) interpret(execution Execution, sensitivePaths ...string) (domain.SyncResult, lapseReport, error) {
	if execution.StdoutTruncated {
		return domain.SyncResult{}, lapseReport{}, fmt.Errorf("LAPSE JSON output exceeded the configured limit")
	}
	report, err := decodeReport(execution.Stdout)
	if err != nil {
		stderr := l.redact(string(execution.Stderr), sensitivePaths...)
		if noSpeechMessage(stderr) {
			return domain.SyncResult{}, lapseReport{}, &NoSpeechError{Detail: strings.TrimSpace(stderr)}
		}
		if execution.ExitCode != 0 {
			return domain.SyncResult{}, lapseReport{}, fmt.Errorf("LAPSE exited with code %d: %s", execution.ExitCode, strings.TrimSpace(stderr))
		}
		return domain.SyncResult{}, lapseReport{}, fmt.Errorf("parse LAPSE JSON: %w", err)
	}
	if report.Verdict == "solid" && execution.ExitCode != 0 {
		return domain.SyncResult{}, lapseReport{}, fmt.Errorf("LAPSE returned solid with exit code %d", execution.ExitCode)
	}
	if report.Verdict != "solid" && execution.ExitCode != 2 && execution.ExitCode != 3 {
		return domain.SyncResult{}, lapseReport{}, fmt.Errorf("LAPSE returned %s with unexpected exit code %d", report.Verdict, execution.ExitCode)
	}
	return report.syncResult(), report, nil
}

func (l *Lapse) redact(value string, explicit ...string) string {
	paths := append(append([]string(nil), explicit...), l.mediaRoots...)
	for _, path := range paths {
		if path == "" {
			continue
		}
		value = strings.ReplaceAll(value, path, "[media]")
	}
	return value
}

type lapseReport struct {
	Mode        string
	Reference   string
	OffsetMS    int64
	Ratio       float64
	Confidence  float64
	Margin      float64
	Sigma       float64
	Agreement   float64
	Verdict     string
	Coverage    float64
	Cues        int
	IgnoredCues int
	Parts       int
	Written     bool
	Why         string
	Output      string
	Splits      []int
}

type rawReport struct {
	Mode        *string  `json:"mode"`
	Reference   *string  `json:"reference"`
	OffsetMS    *int64   `json:"offset_ms"`
	Ratio       *float64 `json:"ratio"`
	Confidence  *float64 `json:"confidence"`
	Margin      *float64 `json:"margin"`
	Sigma       *float64 `json:"sigma"`
	Agreement   *float64 `json:"agreement"`
	Verdict     *string  `json:"verdict"`
	Coverage    *float64 `json:"coverage"`
	Cues        *int     `json:"cues"`
	IgnoredCues *int     `json:"ignored_cues"`
	Parts       *int     `json:"parts"`
	Written     *bool    `json:"written"`
	Why         *string  `json:"why,omitempty"`
	Output      *string  `json:"output"`
	Splits      *[]int   `json:"splits"`
}

func parseReport(payload []byte) (domain.SyncResult, error) {
	report, err := decodeReport(payload)
	if err != nil {
		return domain.SyncResult{}, err
	}
	return report.syncResult(), nil
}

func decodeReport(payload []byte) (lapseReport, error) {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var raw rawReport
	if err := decoder.Decode(&raw); err != nil {
		return lapseReport{}, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return lapseReport{}, fmt.Errorf("multiple JSON documents")
		}
		return lapseReport{}, fmt.Errorf("trailing JSON: %w", err)
	}
	if raw.Mode == nil || raw.Reference == nil || raw.OffsetMS == nil || raw.Ratio == nil || raw.Confidence == nil || raw.Margin == nil || raw.Sigma == nil || raw.Agreement == nil || raw.Verdict == nil || raw.Coverage == nil || raw.Cues == nil || raw.IgnoredCues == nil || raw.Parts == nil || raw.Written == nil || raw.Output == nil || raw.Splits == nil {
		return lapseReport{}, fmt.Errorf("LAPSE JSON is missing required fields")
	}
	report := lapseReport{Mode: *raw.Mode, Reference: *raw.Reference, OffsetMS: *raw.OffsetMS, Ratio: *raw.Ratio, Confidence: *raw.Confidence, Margin: *raw.Margin, Sigma: *raw.Sigma, Agreement: *raw.Agreement, Verdict: *raw.Verdict, Coverage: *raw.Coverage, Cues: *raw.Cues, IgnoredCues: *raw.IgnoredCues, Parts: *raw.Parts, Written: *raw.Written, Output: *raw.Output, Splits: append([]int(nil), (*raw.Splits)...)}
	if raw.Why != nil {
		report.Why = *raw.Why
	}
	if err := report.validate(); err != nil {
		return lapseReport{}, err
	}
	return report, nil
}

func (r lapseReport) validate() error {
	validModes := map[string]bool{"ols": true, "nosplit": true, "split": true, "auto/restart": true, "auto/joined": true, "auto/shifted": true, "auto/shifted+split": true, "auto/drifting": true, "auto/drifting+split": true, "auto/recut": true}
	if !validModes[r.Mode] {
		return fmt.Errorf("unknown LAPSE mode %q", r.Mode)
	}
	if r.Reference != "vad" && r.Reference != "embedded" && r.Reference != "subtitle" {
		return fmt.Errorf("unknown LAPSE reference %q", r.Reference)
	}
	if r.Verdict != "solid" && r.Verdict != "unsure" && r.Verdict != "nothing" {
		return fmt.Errorf("unknown LAPSE verdict %q", r.Verdict)
	}
	if !finite(r.Ratio) || r.Ratio <= 0 || !unit(r.Confidence) || !unit(r.Margin) || !finite(r.Sigma) || r.Sigma < 0 || !unit(r.Agreement) || !unit(r.Coverage) {
		return fmt.Errorf("LAPSE JSON contains unsafe metrics")
	}
	if r.Cues <= 0 || r.IgnoredCues < 0 || r.Parts < 1 || len(r.Splits) != r.Parts-1 {
		return fmt.Errorf("LAPSE JSON contains inconsistent cue or part counts")
	}
	previous := 0
	for _, split := range r.Splits {
		if split <= previous || split >= r.Cues {
			return fmt.Errorf("LAPSE JSON contains invalid split positions")
		}
		previous = split
	}
	if strings.TrimSpace(r.Output) == "" {
		return fmt.Errorf("LAPSE JSON has no output path")
	}
	return nil
}

func (r lapseReport) syncResult() domain.SyncResult {
	return domain.SyncResult{Verdict: r.Verdict, Mode: r.Mode, Reference: r.Reference, OffsetMS: r.OffsetMS, Ratio: r.Ratio, Confidence: r.Confidence, Agreement: r.Agreement, Coverage: r.Coverage, Parts: r.Parts, Splits: len(r.Splits)}
}

func analysisCopy(source string) (string, string, error) {
	if !supportedTextExtension(source) {
		return "", "", fmt.Errorf("LAPSE input must use a supported text subtitle extension")
	}
	info, err := os.Lstat(source)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return "", "", fmt.Errorf("LAPSE input is not a regular file")
	}
	if info.Size() <= 0 || info.Size() > maximumSubtitleBytes {
		return "", "", fmt.Errorf("LAPSE input size is invalid")
	}
	payload, err := os.ReadFile(source)
	if err != nil {
		return "", "", fmt.Errorf("read LAPSE input: %w", err)
	}
	workspace, err := os.MkdirTemp("", "subsyncd-lapse-analyze-")
	if err != nil {
		return "", "", fmt.Errorf("create LAPSE analysis workspace: %w", err)
	}
	destination := filepath.Join(workspace, "candidate"+strings.ToLower(filepath.Ext(source)))
	if err := os.WriteFile(destination, payload, 0o600); err != nil {
		_ = os.RemoveAll(workspace)
		return "", "", fmt.Errorf("copy LAPSE input: %w", err)
	}
	return workspace, destination, nil
}

func validateOutput(path string) error {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("LAPSE output is missing or not a regular file")
	}
	if info.Size() <= 0 || info.Size() > maximumSubtitleBytes {
		return fmt.Errorf("LAPSE output size is invalid")
	}
	payload, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read LAPSE output: %w", err)
	}
	if !utf8.Valid(payload) || bytes.IndexByte(payload, 0) >= 0 {
		return fmt.Errorf("LAPSE output is not valid UTF-8 text")
	}
	var subtitles *astisub.Subtitles
	switch strings.ToLower(filepath.Ext(path)) {
	case ".srt":
		subtitles, err = astisub.ReadFromSRT(bytes.NewReader(payload))
	case ".ass", ".ssa":
		subtitles, err = astisub.ReadFromSSA(bytes.NewReader(payload))
	case ".vtt":
		subtitles, err = astisub.ReadFromWebVTT(bytes.NewReader(payload))
	default:
		return fmt.Errorf("LAPSE output has an unsupported extension")
	}
	if err != nil || subtitles == nil || len(subtitles.Items) == 0 || len(subtitles.Items) > maximumSubtitleCues {
		return fmt.Errorf("LAPSE output subtitle syntax is invalid")
	}
	previous := subtitles.Items[0].StartAt
	for _, item := range subtitles.Items {
		if item.StartAt < 0 || item.EndAt < item.StartAt || item.StartAt < previous {
			return fmt.Errorf("LAPSE output timestamps are invalid")
		}
		previous = item.StartAt
	}
	return nil
}

func supportedTextExtension(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".srt", ".ass", ".ssa", ".vtt":
		return true
	}
	return false
}

func noSpeechMessage(value string) bool {
	lower := strings.ToLower(value)
	return strings.Contains(lower, "no speech found") || strings.Contains(lower, "got no audio out of") || strings.Contains(lower, "no audio track")
}

func samePath(left, right string) bool {
	leftAbsolute, leftErr := filepath.Abs(left)
	rightAbsolute, rightErr := filepath.Abs(right)
	return leftErr == nil && rightErr == nil && filepath.Clean(leftAbsolute) == filepath.Clean(rightAbsolute)
}

func finite(value float64) bool { return !math.IsNaN(value) && !math.IsInf(value, 0) }
func unit(value float64) bool   { return finite(value) && value >= 0 && value <= 1 }
