package subsource

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"subsyncd/internal/domain"
)

func downloadCandidate(id string) domain.Candidate {
	return domain.Candidate{ProviderID: "subsource-test", ResultID: id, DownloadRef: id}
}

func TestDownloadStreamsArchive(t *testing.T) {
	env := newTestEnv(t, Config{}, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/subtitles/501/download" || r.URL.RawQuery != "" {
			t.Errorf("unexpected request %s?%s", r.URL.Path, r.URL.RawQuery)
		}
		w.Header().Set("Content-Type", "application/zip")
		w.Header().Set("Content-Disposition", `attachment; filename="example-movie_english_501-[SubSource].zip"`)
		io.WriteString(w, "PK-archive-bytes")
	})
	var body bytes.Buffer
	meta, err := env.client.Download(context.Background(), downloadCandidate("501"), &body)
	if err != nil || body.String() != "PK-archive-bytes" {
		t.Fatalf("download = %q, %v", body.String(), err)
	}
	if meta.Filename != "subsource-501.zip" || meta.ContentType != "application/zip" {
		t.Fatalf("metadata = %#v", meta)
	}
}

func TestDownloadRejectsInvalidReferences(t *testing.T) {
	env := newTestEnv(t, Config{}, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request %s", r.URL.Path)
	})
	for _, id := range []string{"", "0", "-1", "abc", "1/2", "../1", "501?x=1", "01", "12345678901234567890"} {
		if _, err := env.client.Download(context.Background(), downloadCandidate(id), io.Discard); err == nil {
			t.Errorf("reference %q was accepted", id)
		}
	}
	other := downloadCandidate("501")
	other.ProviderID = "subsource-other"
	if _, err := env.client.Download(context.Background(), other, io.Discard); err == nil {
		t.Error("another instance's candidate was accepted")
	}
}

func TestDownloadRefusesRedirects(t *testing.T) {
	env := newTestEnv(t, Config{}, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/subtitles/501/download" {
			t.Errorf("redirect was followed to %s", r.URL.Path)
		}
		http.Redirect(w, r, "/elsewhere", http.StatusFound)
	})
	if _, err := env.client.Download(context.Background(), downloadCandidate("501"), io.Discard); err == nil {
		t.Fatal("redirect should fail the download")
	}
}

func TestDownloadEnforcesSizeLimit(t *testing.T) {
	for name, declare := range map[string]bool{"declared": true, "streamed": false} {
		t.Run(name, func(t *testing.T) {
			env := newTestEnv(t, Config{MaxDownloadBytes: 8}, func(w http.ResponseWriter, r *http.Request) {
				payload := strings.Repeat("x", 9)
				if declare {
					w.Header().Set("Content-Length", "9")
				} else {
					w.(http.Flusher).Flush()
				}
				io.WriteString(w, payload)
			})
			if _, err := env.client.Download(context.Background(), downloadCandidate("501"), io.Discard); err == nil || !strings.Contains(err.Error(), "exceeds") {
				t.Fatalf("err = %v; want size error", err)
			}
		})
	}
}

func TestDownloadRejectsNonOK(t *testing.T) {
	env := newTestEnv(t, Config{}, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"gone","message":"secret detail"}`, http.StatusGone)
	})
	_, err := env.client.Download(context.Background(), downloadCandidate("501"), io.Discard)
	if err == nil || strings.Contains(err.Error(), "secret detail") {
		t.Fatalf("err = %v; want generic status error", err)
	}
}
