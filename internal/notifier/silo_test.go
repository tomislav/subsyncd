package notifier

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"subsyncd/internal/domain"
)

func TestDisabledSiloNotifierIsNoop(t *testing.T) {
	notifier, err := NewSilo(SiloConfig{Enabled: false})
	if err != nil {
		t.Fatal(err)
	}
	if err := notifier.SubtitleChanged(context.Background(), domain.Media{}, ""); err != nil {
		t.Fatal(err)
	}
}

func TestSiloPostsJellyfinCompatibleMediaUpdateWithMappedMediaPath(t *testing.T) {
	var gotPath, gotType, gotToken string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != "/Library/Media/Updated" {
			t.Errorf("request = %s %s", request.Method, request.URL.Path)
		}
		gotToken = request.Header.Get("X-Emby-Token")
		var payload struct {
			Updates []struct {
				Path       string `json:"path"`
				UpdateType string `json:"updateType"`
			} `json:"Updates"`
		}
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil || len(payload.Updates) != 1 {
			t.Errorf("payload = %#v, %v", payload, err)
		} else {
			gotPath = payload.Updates[0].Path
			gotType = payload.Updates[0].UpdateType
		}
		writer.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	notifier, err := NewSilo(SiloConfig{Enabled: true, BaseURL: server.URL, APIKey: "sa_secret", PathMappings: []PathMapping{{From: "/local/media", To: "/mnt/library"}}})
	if err != nil {
		t.Fatal(err)
	}
	media := domain.Media{Fingerprint: domain.MediaFingerprint{Path: "/local/media/shows/Show/episode.mkv"}}
	if err := notifier.SubtitleChanged(context.Background(), media, "/local/media/shows/Show/episode.en.srt"); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/mnt/library/shows/Show/episode.mkv" || gotType != "Modified" || gotToken != "sa_secret" {
		t.Fatalf("update = path %q type %q token %q", gotPath, gotType, gotToken)
	}
}

func TestSiloClassifiesFailuresWithoutLeakingCredentialsOrBodies(t *testing.T) {
	for _, test := range []struct {
		status    int
		retryable bool
	}{
		{http.StatusUnauthorized, false},
		{http.StatusForbidden, false},
		{http.StatusBadRequest, false},
		{http.StatusRequestTimeout, true},
		{http.StatusTooManyRequests, true},
		{http.StatusBadGateway, true},
	} {
		t.Run(http.StatusText(test.status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				writer.WriteHeader(test.status)
				_, _ = writer.Write([]byte("body-secret"))
			}))
			defer server.Close()
			notifier, err := NewSilo(SiloConfig{Enabled: true, BaseURL: server.URL, APIKey: "sa_secret"})
			if err != nil {
				t.Fatal(err)
			}
			err = notifier.SubtitleChanged(context.Background(), domain.Media{Fingerprint: domain.MediaFingerprint{Path: "/media/movie.mkv"}}, "/media/movie.en.srt")
			if err == nil || IsRetryable(err) != test.retryable {
				t.Fatalf("error = %T %v, retryable=%v", err, err, IsRetryable(err))
			}
			if strings.Contains(err.Error(), "sa_secret") || strings.Contains(err.Error(), "body-secret") {
				t.Fatalf("error leaks sensitive data: %v", err)
			}
		})
	}
}

func TestSiloTimeoutIsRetryable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		time.Sleep(100 * time.Millisecond)
		writer.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	notifier, err := NewSilo(SiloConfig{Enabled: true, BaseURL: server.URL, APIKey: "sa_secret", Client: &http.Client{Timeout: 20 * time.Millisecond}})
	if err != nil {
		t.Fatal(err)
	}
	err = notifier.SubtitleChanged(context.Background(), domain.Media{Fingerprint: domain.MediaFingerprint{Path: "/media/movie.mkv"}}, "/media/movie.en.srt")
	if err == nil || !IsRetryable(err) {
		t.Fatalf("timeout error = %T %v", err, err)
	}
}

func TestSiloDoesNotFollowRedirectWithToken(t *testing.T) {
	received := false
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { received = true }))
	defer target.Close()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		http.Redirect(writer, &http.Request{}, target.URL, http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	notifier, err := NewSilo(SiloConfig{Enabled: true, BaseURL: server.URL, APIKey: "sa_secret"})
	if err != nil {
		t.Fatal(err)
	}
	err = notifier.SubtitleChanged(context.Background(), domain.Media{Fingerprint: domain.MediaFingerprint{Path: "/media/movie.mkv"}}, "/media/movie.en.srt")
	if err == nil || received {
		t.Fatalf("redirect error=%v received=%v", err, received)
	}
}
