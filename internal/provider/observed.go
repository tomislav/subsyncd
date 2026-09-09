package provider

import (
	"context"
	"io"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"subsyncd/internal/domain"
	"subsyncd/internal/observability"
)

type observedProvider struct {
	Provider
	events *observability.Emitter
}

// Observe adds privacy-safe download telemetry to a provider. Calling Observe
// more than once returns the existing wrapper.
func Observe(item Provider, events *observability.Emitter) Provider {
	if item == nil || events == nil {
		return item
	}
	if _, ok := item.(*observedProvider); ok {
		return item
	}
	return &observedProvider{Provider: item, events: events.For("provider")}
}

func (p *observedProvider) Download(ctx context.Context, candidate domain.Candidate, output io.Writer) (DownloadMetadata, error) {
	startedAt := time.Now()
	counter := &countingWriter{writer: output}
	metadata, err := p.Provider.Download(ctx, candidate, counter)
	attrs := []slog.Attr{
		slog.String("provider", p.ID()),
		slog.String("candidate_id", safeCandidateID(candidate.ResultID)),
		slog.String("outcome", providerOutcome(err, 1)),
		slog.Int64("bytes", counter.bytes),
		slog.Int64("duration_ms", time.Since(startedAt).Milliseconds()),
	}
	if err != nil {
		attrs = append(attrs, p.events.ErrorAttrs(providerErrorKind(err), err)...)
	}
	p.events.Log(ctx, searchLogLevel(err), "provider.download_completed", "provider download completed", attrs...)
	if err == nil && metadata.Filename != "" {
		p.events.Log(ctx, slog.LevelDebug, "provider.download_details", "provider download metadata", slog.String("provider", p.ID()), slog.String("candidate_id", safeCandidateID(candidate.ResultID)), slog.String("filename", observability.SafeText(metadata.Filename)))
	}
	return metadata, err
}

func safeCandidateID(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		if boundary := strings.IndexAny(raw, "?#"); boundary >= 0 {
			raw = raw[:boundary]
		}
		return observability.SafeText(raw)
	}
	if parsed.IsAbs() {
		return observability.SafeText(parsed.EscapedPath())
	}
	parsed.RawQuery = ""
	parsed.ForceQuery = false
	parsed.Fragment = ""
	return observability.SafeText(parsed.String())
}

type countingWriter struct {
	writer io.Writer
	bytes  int64
}

func (w *countingWriter) Write(data []byte) (int, error) {
	written, err := w.writer.Write(data)
	w.bytes += int64(written)
	return written, err
}

func (p *observedProvider) SearchCacheVersion() string { return searchCacheVersion(p.Provider) }
