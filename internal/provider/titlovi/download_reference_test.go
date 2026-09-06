package titlovi

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"subsyncd/internal/domain"
	"subsyncd/internal/pack"
)

func TestMalformedDownloadReferencesFailBeforeRequests(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if strings.Contains(r.URL.Path, "gettoken") {
			io.WriteString(w, `{"Token":"fixture","UserId":42,"ExpirationDate":"2026-09-04T14:00:00Z"}`)
		} else {
			io.WriteString(w, "0")
		}
	}))
	defer server.Close()
	client := newTestClient(t, server, 1024)
	refs := []struct{ name, ref string }{
		{"empty", ""},
		{"bare download endpoint", "/download/"},
		{"bare download endpoint without slash", "/download"},
		{"download fragment only", "/download/#private"},
		{"query only", "?token=private"},
		{"fragment only", "#private"},
		{"network path", "//outside.example/download/42"},
		{"network userinfo", "//user:private@outside.example/download/42"},
		{"absolute root", server.URL + "/"},
		{"absolute root traversal", server.URL + "/../"},
		{"absolute query only", server.URL + "?token=private"},
		{"absolute fragment only", server.URL + "#private"},
		{"root", "/"},
		{"root traversal", "/../"},
		{"root dot", "/./"},
		{"absolute userinfo", "https://user:private@outside.example/download/42"},
	}
	for _, test := range refs {
		t.Run(test.name, func(t *testing.T) {
			ref := test.ref
			requests = 0
			if _, ok := client.downloadReference(ref); ok {
				t.Error("malformed reference accepted during normalization")
			}
			_, err := client.Download(context.Background(), domain.Candidate{DownloadRef: ref}, io.Discard)
			if err == nil {
				t.Error("malformed download reference accepted")
			}
			var content *pack.ContentError
			if errors.As(err, &content) {
				t.Error("metadata failure was classified as subtitle content rejection")
			}
			if err != nil && strings.Contains(err.Error(), "private") {
				t.Error("reference leaked in error")
			}
			if requests != 0 {
				t.Errorf("malformed reference caused %d requests", requests)
			}
		})
	}
}

func TestDownloadReferencePreservesUsablePath(t *testing.T) {
	client := &Client{config: Config{DownloadHosts: []string{"download.example"}}}
	for _, ref := range []string{"/subtitle/42.zip", "subtitle/42.zip", "https://download.example/subtitle/42.zip"} {
		got, ok := client.downloadReference(ref)
		if !ok || got != "/subtitle/42.zip" {
			t.Fatalf("usable reference: got %q, accepted %v", got, ok)
		}
	}
}

func TestTitloviDownloadReferenceRetainsQueryIdentity(t *testing.T) {
	client := &Client{config: Config{DownloadHosts: []string{"download.example"}}}
	for _, ref := range []string{"/download/?id=42", "https://download.example/download/?id=42"} {
		got, ok := client.downloadReference(ref)
		if !ok || got != "/download/?id=42" {
			t.Fatalf("query identity lost: got %q, accepted %v", got, ok)
		}
	}
	if got, ok := client.downloadReference("/download/42"); !ok || got != "/download/42" {
		t.Fatal("path identity rejected")
	}
}
