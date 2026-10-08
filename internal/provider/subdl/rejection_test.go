package subdl

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"

	baseprovider "subsyncd/internal/provider"

	"subsyncd/internal/domain"
)

func searchWithResponse(t *testing.T, media domain.Media, body string) ([]domain.Candidate, error) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, body)
	}))
	defer server.Close()
	client := newTestClient(t, server, 1<<20)
	return client.Search(context.Background(), baseprovider.SearchQuery{Media: media, Language: "en", Mode: baseprovider.SearchBroad})
}

func movieMedia() domain.Media {
	return domain.Media{Ref: domain.MediaRef{Kind: domain.MediaMovie}, Title: "Alice in Chains: MTV Unplugged", Year: 1996, ExternalIDs: domain.ExternalIDs{IMDb: "tt0276768"}}
}

// SubDL answers a title it doesn't know with HTTP 200, status false and
// "can't find movie or tv" (seen 2026-10 for concert films). That is an empty
// result, not a failed search.
func TestUnknownTitleIsNoResult(t *testing.T) {
	for name, media := range map[string]domain.Media{"movie": movieMedia(), "episode": episodeMedia()} {
		t.Run(name, func(t *testing.T) {
			candidates, err := searchWithResponse(t, media, `{"status":false,"error":"can't find movie or tv"}`)
			if err != nil || len(candidates) != 0 {
				t.Fatalf("candidates=%d err=%v, want an empty result", len(candidates), err)
			}
		})
	}
}

// An unrecognised rejection carries SubDL's message, so the next wording change
// shows up in the logs, but only as a short, single-line, plain-text excerpt
// with the API key redacted.
func TestUnrecognisedRejectionIncludesSanitizedMessage(t *testing.T) {
	long := strings.Repeat("x", 200)
	_, err := searchWithResponse(t, movieMedia(), `{"status":false,"error":"Unknown parameter\n<b>api-key</b> \u0007used `+"`rm`"+` `+long+`"}`)
	if err == nil {
		t.Fatal("unrecognised rejection returned no error")
	}
	message := err.Error()
	const prefix = "SubDL search was rejected: "
	if !strings.HasPrefix(message, prefix) {
		t.Fatalf("error = %q, want prefix %q", message, prefix)
	}
	excerpt := strings.TrimPrefix(message, prefix)
	if !strings.HasPrefix(excerpt, "Unknown parameter b [redacted] b used rm x") {
		t.Fatalf("excerpt = %q", excerpt)
	}
	if strings.Contains(message, "api-key") || strings.ContainsAny(message, "\n\u0007<>`") {
		t.Fatalf("excerpt leaks unsafe content: %q", message)
	}
	if utf8.RuneCountInString(excerpt) > maxRejectionExcerpt {
		t.Fatalf("excerpt has %d runes, want at most %d", utf8.RuneCountInString(excerpt), maxRejectionExcerpt)
	}
}

func TestRejectionWithoutMessageKeepsPlainError(t *testing.T) {
	for _, body := range []string{`{"status":false}`, `{"status":false,"error":"  \n "}`, `{"success":false,"error":"<<<>>>"}`} {
		if _, err := searchWithResponse(t, movieMedia(), body); err == nil || err.Error() != "SubDL search was rejected" {
			t.Fatalf("body %s: error = %v, want plain rejection", body, err)
		}
	}
}
