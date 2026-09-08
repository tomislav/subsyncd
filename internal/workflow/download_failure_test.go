package workflow

import (
	"context"
	"errors"
	"io"
	"os"
	"testing"

	"subsyncd/internal/domain"
	"subsyncd/internal/pack"
	"subsyncd/internal/provider"
)

type failingDownloadWriter struct{ err error }

func (w failingDownloadWriter) Write([]byte) (int, error) { return 0, w.err }

type localFailureDownloadProvider struct {
	fakeProvider
	failWrite bool
	exceed    bool
	failure   error
}

func (p *localFailureDownloadProvider) Download(_ context.Context, _ domain.Candidate, writer io.Writer) (provider.DownloadMetadata, error) {
	bounded := writer.(*boundedDownloadWriter)
	bounded.remaining = 1
	if p.failWrite {
		bounded.writer = failingDownloadWriter{p.failure}
	}
	payload := []byte("x")
	if p.exceed {
		payload = []byte("xx")
	}
	_, _ = bounded.Write(payload) // Exercise an adapter that swallows writer errors.
	if !p.failWrite {
		_ = bounded.writer.(*os.File).Close()
	}
	return provider.DownloadMetadata{}, nil
}

func TestDownloadLocalFailureOverridesContentRejection(t *testing.T) {
	for _, test := range []struct {
		name              string
		failWrite, exceed bool
	}{
		{"swallowed_write", true, false},
		{"oversize_with_write_failure", true, true},
		{"oversize_with_flush_failure", false, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			failure := errors.New("local writer failed")
			adapter := &localFailureDownloadProvider{fakeProvider: fakeProvider{id: "provider"}, failWrite: test.failWrite, exceed: test.exceed, failure: failure}
			service := &Service{Providers: map[string]provider.Provider{"provider": adapter}}
			_, _, _, err := service.downloadAndSelect(context.Background(), serviceRequest(t), exactCandidate("failure"), t.TempDir(), 0)
			var content *pack.ContentError
			if err == nil || errors.As(err, &content) {
				t.Fatalf("local failure classified as content: %T %v", err, err)
			}
			if test.failWrite && !errors.Is(err, failure) {
				t.Fatalf("writer failure lost: %v", err)
			}
		})
	}
}

func TestBoundedDownloadRetainsShortWriteFailure(t *testing.T) {
	writer := &boundedDownloadWriter{writer: failingDownloadWriter{}, remaining: 10, limit: 10}
	for range 2 {
		if _, err := writer.Write([]byte("payload")); !errors.Is(err, io.ErrShortWrite) {
			t.Fatalf("short write failure = %v", err)
		}
	}
}
