package workflow

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"os"
	"path"
	"strings"

	"subsyncd/internal/pack"
)

// ProviderResponseError is a download that delivered the provider's own
// message instead of a subtitle: an HTML login or limit page, a JSON error
// body, or Titlovi's placeholder subtitle. It is a provider problem that is
// retried, never a rejection of the candidate.
type ProviderResponseError struct{ Reason string }

func (e *ProviderResponseError) Error() string {
	return "provider download is not a subtitle: " + e.Reason
}

// titloviPlaceholder is the only member of the archive Titlovi serves when it
// does not authorize a download; its one cue tells the viewer to log in.
const titloviPlaceholder = "titlovi_titl_plugin_message.srt"

// sniffLimit bounds how much of a download is read to recognize an error body.
const sniffLimit = 64 << 10

// checkProviderResponse recognizes an error page or JSON error body served in
// place of a subtitle, from the declared content type and the first bytes.
// Archives are never error pages, and a subtitle does not start like HTML or
// parse as JSON (a MicroDVD line starts with '{' but is not JSON).
func checkProviderResponse(payloadPath, contentType string) error {
	file, err := os.Open(payloadPath)
	if err != nil {
		return err
	}
	defer file.Close()
	head, err := io.ReadAll(io.LimitReader(file, sniffLimit+1))
	if err != nil {
		return fmt.Errorf("read provider download: %w", err)
	}
	if pack.HasArchiveMagic(head) {
		return nil
	}
	text := bytes.TrimLeft(bytes.TrimPrefix(head, []byte{0xef, 0xbb, 0xbf}), " \t\r\n")
	lower := strings.ToLower(string(text[:min(len(text), 32)]))
	mediaType, _, _ := mime.ParseMediaType(contentType)
	switch {
	case strings.HasPrefix(lower, "<!doctype html") || strings.HasPrefix(lower, "<html") || strings.HasPrefix(lower, "<head") || strings.HasPrefix(lower, "<body"):
		return &ProviderResponseError{Reason: "HTML page"}
	case mediaType == "text/html" && strings.HasPrefix(lower, "<"):
		return &ProviderResponseError{Reason: "HTML page"}
	case len(head) <= sniffLimit && (strings.HasPrefix(lower, "{") || strings.HasPrefix(lower, "[")) && json.Valid(text):
		return &ProviderResponseError{Reason: "JSON body"}
	}
	return nil
}

// checkPlaceholderMembers recognizes an archive holding only Titlovi's
// placeholder message.
func checkPlaceholderMembers(manifest pack.Manifest) error {
	if len(manifest.Members) == 0 {
		return nil
	}
	for _, member := range manifest.Members {
		if !strings.EqualFold(path.Base(strings.ReplaceAll(member.SafeName, "\\", "/")), titloviPlaceholder) {
			return nil
		}
	}
	return &ProviderResponseError{Reason: "Titlovi placeholder message"}
}
