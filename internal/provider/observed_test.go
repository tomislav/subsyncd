package provider

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"subsyncd/internal/domain"
	"subsyncd/internal/observability"
	"subsyncd/internal/store"
	"subsyncd/internal/testutil"
)

type downloadProvider struct {
	err error
}

func (downloadProvider) ID() string                            { return "download-test" }
func (downloadProvider) Capabilities() Capabilities            { return Capabilities{} }
func (downloadProvider) SupportsLanguage(domain.Language) bool { return true }
func (downloadProvider) Search(context.Context, SearchQuery) ([]domain.Candidate, error) {
	return nil, nil
}
func (p downloadProvider) Download(_ context.Context, _ domain.Candidate, writer io.Writer) (DownloadMetadata, error) {
	_, _ = io.WriteString(writer, "subtitle")
	return DownloadMetadata{Filename: "private.release.name.srt"}, p.err
}

func TestObservedProviderLogsDownloadBytesWithoutProviderMetadata(t *testing.T) {
	var logs bytes.Buffer
	events, err := observability.New(&logs, observability.Options{Level: "info", Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	item := Observe(downloadProvider{}, events)

	var output bytes.Buffer
	_, err = item.Download(context.Background(), domain.Candidate{ProviderID: "download-test", ResultID: "candidate-7", DownloadRef: "https://signed.example/file?token=secret"}, &output)
	if err != nil {
		t.Fatal(err)
	}
	records := providerEvents(providerLogRecords(t, logs.String()), "provider.download_completed")
	if len(records) != 1 || records[0]["outcome"] != "success" || records[0]["bytes"] != float64(len("subtitle")) {
		t.Fatalf("download record = %#v", records)
	}
	if records[0]["provider"] != "download-test" || records[0]["candidate_id"] != "candidate-7" {
		t.Fatalf("download correlation = %#v", records[0])
	}
	for _, forbidden := range []string{"private.release.name.srt", "signed.example", "token=secret"} {
		if strings.Contains(logs.String(), forbidden) {
			t.Fatalf("download logs leaked %q: %s", forbidden, logs.String())
		}
	}
}

func TestObservedProviderStripsCandidateQueryFromLogs(t *testing.T) {
	var logs bytes.Buffer
	events, err := observability.New(&logs, observability.Options{Level: "debug", Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	item := Observe(downloadProvider{}, events)
	_, err = item.Download(context.Background(), domain.Candidate{ResultID: "/subtitle/movie.srt?api_key=secret"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	records := providerEvents(providerLogRecords(t, logs.String()), "provider.download_completed")
	if len(records) != 1 || records[0]["candidate_id"] != "/subtitle/movie.srt" {
		t.Fatalf("safe candidate correlation = %#v", records)
	}
	if strings.Contains(logs.String(), "api_key") || strings.Contains(logs.String(), "secret") {
		t.Fatalf("candidate query leaked to logs: %s", logs.String())
	}
}

func TestObservedProviderStripsMalformedCandidateQueryFromLogs(t *testing.T) {
	var logs bytes.Buffer
	events, err := observability.New(&logs, observability.Options{Level: "debug", Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	item := Observe(downloadProvider{}, events)
	_, err = item.Download(context.Background(), domain.Candidate{ResultID: "/subtitle/%zz?api_key=secret#fragment"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(logs.String(), "api_key") || strings.Contains(logs.String(), "secret") || strings.Contains(logs.String(), "fragment") {
		t.Fatalf("malformed candidate query leaked to logs: %s", logs.String())
	}
}

func TestObservedProviderClassifiesDownloadCooldown(t *testing.T) {
	var logs bytes.Buffer
	events, err := observability.New(&logs, observability.Options{Level: "info", Version: "test", Redact: func(error) string { return "provider unavailable" }})
	if err != nil {
		t.Fatal(err)
	}
	cooldown := &CooldownError{ProviderID: "download-test", Scope: OperationDownload, Reason: "quota token=secret", ResetAt: time.Now().Add(time.Hour)}
	item := Observe(downloadProvider{err: cooldown}, events)

	_, got := item.Download(context.Background(), domain.Candidate{ResultID: "candidate-8"}, io.Discard)
	if !errors.Is(got, cooldown) {
		t.Fatalf("download error = %v", got)
	}
	records := providerEvents(providerLogRecords(t, logs.String()), "provider.download_completed")
	if len(records) != 1 || records[0]["outcome"] != "throttled" || records[0]["error"] != "provider unavailable" {
		t.Fatalf("download record = %#v", records)
	}
	if strings.Contains(logs.String(), "token=secret") {
		t.Fatalf("download logs leaked provider error: %s", logs.String())
	}
}

func TestObservedProviderClassifiesOtherDownloadFailuresOnce(t *testing.T) {
	tests := []struct {
		name    string
		err     error
		outcome string
		level   string
	}{
		{name: "disabled", err: &DisabledError{ProviderID: "download-test", Reason: "secret"}, outcome: "disabled", level: "warn"},
		{name: "canceled", err: context.Canceled, outcome: "canceled", level: "warn"},
		{name: "technical", err: errors.New("request https://api.example?token=secret failed"), outcome: "failed", level: "error"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var logs bytes.Buffer
			events, err := observability.New(&logs, observability.Options{Level: "info", Version: "test", Redact: func(error) string { return "safe failure" }})
			if err != nil {
				t.Fatal(err)
			}
			item := Observe(downloadProvider{err: test.err}, events)
			_, _ = item.Download(context.Background(), domain.Candidate{ResultID: "candidate"}, io.Discard)

			records := providerEvents(providerLogRecords(t, logs.String()), "provider.download_completed")
			if len(records) != 1 || records[0]["outcome"] != test.outcome || records[0]["level"] != test.level {
				t.Fatalf("completion = %#v", records)
			}
			if strings.Contains(logs.String(), "secret") || strings.Contains(logs.String(), "api.example") {
				t.Fatalf("failure log leaked source error: %s", logs.String())
			}
		})
	}
}

type transportDownloadProvider struct {
	downloadProvider
	client Client
}

func (p transportDownloadProvider) Download(ctx context.Context, _ domain.Candidate, writer io.Writer) (DownloadMetadata, error) {
	request, _ := http.NewRequest(http.MethodGet, "https://example.test/subtitle", nil)
	response, err := p.client.Do(ctx, OperationDownload, request)
	if err != nil {
		if response != nil {
			response.Body.Close()
		}
		return DownloadMetadata{}, err
	}
	defer response.Body.Close()
	_, err = io.Copy(writer, response.Body)
	return DownloadMetadata{}, err
}

func TestObservedBodyTimeoutIsThrottledRatherThanCanceled(t *testing.T) {
	var logs bytes.Buffer
	events, err := observability.New(&logs, observability.Options{Level: "info", Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	clock := testutil.NewClock(time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC))
	gate := NewGate(&memoryStateStore{states: map[string]store.ProviderState{}}, clock, 1)
	gate.Configure("download-test", 1000, 10, 1)
	client := Client{Gate: gate, Clock: clock, ProviderID: "download-test", ProviderType: "subdl", HTTP: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: responseReadFailure{responseTimeout{}}}, nil
	})}}
	item := Observe(transportDownloadProvider{client: client}, events)
	_, err = item.Download(context.Background(), domain.Candidate{ResultID: "candidate"}, io.Discard)
	var cooldown *CooldownError
	if !errors.As(err, &cooldown) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("body timeout lost its classification/cause: %v", err)
	}
	if got := providerOutcome(err, 0); got != "throttled" {
		t.Errorf("provider outcome = %q", got)
	}
	records := providerEvents(providerLogRecords(t, logs.String()), "provider.download_completed")
	if len(records) != 1 || records[0]["outcome"] != "throttled" || records[0]["error_kind"] != "throttled" || records[0]["level"] != "warn" {
		t.Fatalf("body timeout completion = %+v", records)
	}
	if strings.Contains(logs.String(), "private-host") {
		t.Fatal("transport cause leaked")
	}
}

func TestProviderOutcomePrefersTypedQuotaOverWrappedDeadline(t *testing.T) {
	err := errors.Join(context.DeadlineExceeded, &QuotaError{Scope: OperationDownload})
	if got := providerOutcome(err, 0); got != "throttled" {
		t.Fatalf("quota outcome=%q", got)
	}
	if got := providerOutcome(context.DeadlineExceeded, 0); got != "canceled" {
		t.Fatalf("caller deadline outcome=%q", got)
	}
}
