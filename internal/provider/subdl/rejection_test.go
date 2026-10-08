package subdl

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"

	"subsyncd/internal/domain"
	"subsyncd/internal/observability"
	baseprovider "subsyncd/internal/provider"
)

// searchWithResponse runs one search against a server that always returns body
// and returns the candidates, the error and every logged record.
func searchWithResponse(t *testing.T, media domain.Media, body string) ([]domain.Candidate, error, []map[string]any) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, body)
	}))
	defer server.Close()
	client := newTestClient(t, server, 1<<20)
	var logs bytes.Buffer
	emitter, err := observability.New(&logs, observability.Options{Level: "debug", Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	client.events = emitter.For("provider")
	candidates, searchErr := client.Search(context.Background(), baseprovider.SearchQuery{Media: media, Language: "en", Mode: baseprovider.SearchBroad})
	var records []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		if line == "" {
			continue
		}
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("log line %q: %v", line, err)
		}
		records = append(records, record)
	}
	return candidates, searchErr, records
}

func movieMedia() domain.Media {
	return domain.Media{Ref: domain.MediaRef{Kind: domain.MediaMovie}, Title: "Alice in Chains: MTV Unplugged", Year: 1996, ExternalIDs: domain.ExternalIDs{IMDb: "tt0276768"}}
}

// SubDL answers a title it doesn't know with HTTP 200, status false and
// "can't find movie or tv" (seen 2026-10 for concert films), or small variants
// of it. That is an empty result, without a warning.
func TestUnknownTitleIsNoResult(t *testing.T) {
	for _, message := range []string{"can't find movie or tv", "Can't find movie or TV.", "can’t find movie or tv", "  no subtitles   found! "} {
		for name, media := range map[string]domain.Media{"movie": movieMedia(), "episode": episodeMedia()} {
			body, _ := json.Marshal(map[string]any{"status": false, "error": message})
			candidates, err, records := searchWithResponse(t, media, string(body))
			if err != nil || len(candidates) != 0 {
				t.Fatalf("%s %q: candidates=%d err=%v, want an empty result", name, message, len(candidates), err)
			}
			if len(records) != 0 {
				t.Fatalf("%s %q: logged %v, want nothing for a known no-result message", name, message, records)
			}
		}
	}
}

// Any other status-false answer is also an empty result (so the search is not
// retried on the technical schedule), but logs one warning carrying a short,
// plain-text, redacted excerpt of SubDL's message, so a new wording is visible.
func TestUnrecognisedRejectionIsNoResultWithWarning(t *testing.T) {
	body := `{"status":false,"error":"Unknown parameter\n<b>api-key</b> \u0007used ` + "`rm`" + ` ` + strings.Repeat("word ", 40) + `"}`
	candidates, err, records := searchWithResponse(t, movieMedia(), body)
	if err != nil || len(candidates) != 0 {
		t.Fatalf("candidates=%d err=%v, want an empty result", len(candidates), err)
	}
	if len(records) != 1 || records[0]["event"] != "provider.response_unrecognized" || records[0]["level"] != "warn" {
		t.Fatalf("records = %v, want one provider.response_unrecognized warning", records)
	}
	excerpt, _ := records[0]["excerpt"].(string)
	if !strings.HasPrefix(excerpt, "Unknown parameter b [redacted] b used rm word") || !strings.HasSuffix(excerpt, "…") {
		t.Fatalf("excerpt = %q", excerpt)
	}
	if strings.Contains(excerpt, "api-key") || strings.ContainsAny(excerpt, "\n\u0007<>`") {
		t.Fatalf("excerpt leaks unsafe content: %q", excerpt)
	}
	if utf8.RuneCountInString(excerpt) > maxRejectionExcerpt+1 {
		t.Fatalf("excerpt has %d runes, want at most %d plus the marker", utf8.RuneCountInString(excerpt), maxRejectionExcerpt)
	}
	if records[0]["provider"] != "subdl-main" || records[0]["operation"] != "search" {
		t.Fatalf("record = %v, want provider and operation", records[0])
	}
}

// A message about the API key or authorisation stays a failed search (a bad key
// must not look like an empty library), with the plain error and the warning.
func TestCredentialRejectionStaysAnError(t *testing.T) {
	for _, message := range []string{"API key not found", "Invalid api_key", "unauthorized", "Token expired"} {
		body, _ := json.Marshal(map[string]any{"status": false, "error": message})
		_, err, records := searchWithResponse(t, movieMedia(), string(body))
		if err == nil || err.Error() != "SubDL search was rejected" {
			t.Fatalf("%q: error = %v, want the plain rejection", message, err)
		}
		if len(records) != 1 || records[0]["event"] != "provider.response_unrecognized" {
			t.Fatalf("%q: records = %v, want the warning too", message, records)
		}
	}
}

func TestUnrecognisedRejectionWithoutMessageWarnsWithoutExcerpt(t *testing.T) {
	for _, body := range []string{`{"status":false}`, `{"status":false,"error":"  \n "}`, `{"success":false,"error":"<<<>>>"}`} {
		candidates, err, records := searchWithResponse(t, movieMedia(), body)
		if err != nil || len(candidates) != 0 || len(records) != 1 {
			t.Fatalf("body %s: candidates=%d err=%v records=%v", body, len(candidates), err, records)
		}
		if _, ok := records[0]["excerpt"]; ok {
			t.Fatalf("body %s: excerpt present in %v", body, records[0])
		}
	}
}

func TestRejectionExcerpt(t *testing.T) {
	key := "AbC123/xYz+456_q"
	for _, test := range []struct{ name, message, want string }{
		{"key with characters outside the allow-list", "bad key " + key + " sent", "bad key [redacted] sent"},
		{"key echoed in another case", "invalid api_key: " + strings.ToUpper("abcdef0123456789abcd"), "invalid api_key: [redacted]"},
		{"key echoed with separators", "key abcd-1234-efgh-5678 refused", "key [redacted] refused"},
		{"long plain word kept", "internationalization failed", "internationalization failed"},
		{"nothing readable", "<<< >>> ///", ""},
		{"only the key", key, "[redacted]"},
	} {
		if got := rejectionExcerpt(test.message, key); got != test.want {
			t.Errorf("%s: rejectionExcerpt(%q) = %q, want %q", test.name, test.message, got, test.want)
		}
	}
	// The redaction marker is never cut by truncation, and a cut is marked.
	message := strings.Repeat("x ", 37) + key + " tail"
	got := rejectionExcerpt(message, key)
	if strings.Contains(got, "[reda") && !strings.Contains(got, "[redacted]") {
		t.Fatalf("truncation cut the redaction marker: %q", got)
	}
	if !strings.HasSuffix(got, "…") || utf8.RuneCountInString(got) > maxRejectionExcerpt+1 {
		t.Fatalf("truncated excerpt = %q", got)
	}
}
