package workflow

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"mime"
	"os"
	"path"
	"regexp"
	"strings"

	"subsyncd/internal/domain"
	"subsyncd/internal/pack"
	"subsyncd/internal/provider"
)

// ProviderResponseError is a download that delivered the provider's own
// message instead of a subtitle: an HTML login or limit page, a JSON error
// body, or Titlovi's placeholder subtitle. It is a provider problem that is
// retried, never a rejection of the candidate. Refusal marks a limit or login
// message (or the placeholder), which pauses the provider's downloads once a
// second candidate of the same provider confirms it in the same run; Limit
// marks an explicit limit or quota in a JSON body, which pauses them at once.
type ProviderResponseError struct {
	Reason  string
	Refusal bool
	Limit   bool
	// confirmed is the download cooldown a later refusal of another
	// candidate recorded, so this failure also counts as the provider being
	// unavailable rather than as a candidate failure.
	confirmed error
}

func (e *ProviderResponseError) Error() string {
	return "provider download is not a subtitle: " + e.Reason
}

func (e *ProviderResponseError) Unwrap() error { return e.confirmed }

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
	case strings.HasPrefix(lower, "<!doctype html") || strings.HasPrefix(lower, "<html"):
		// Only a whole HTML document is read for a refusal; a fragment, or
		// other markup served as text/html, may be a subtitle in another
		// format (SAMI) whose dialogue reads like one.
		return &ProviderResponseError{Reason: "HTML page", Refusal: refusalDocument(text)}
	case strings.HasPrefix(lower, "<head") || strings.HasPrefix(lower, "<body"):
		return &ProviderResponseError{Reason: "HTML page"}
	case mediaType == "text/html" && strings.HasPrefix(lower, "<"):
		return &ProviderResponseError{Reason: "HTML page"}
	case len(head) <= sniffLimit && (strings.HasPrefix(lower, "{") || strings.HasPrefix(lower, "[")) && json.Valid(text):
		message := strings.ReplaceAll(strings.ToLower(string(text)), "_", " ")
		limit := limitPhrases.MatchString(message)
		return &ProviderResponseError{Reason: "JSON body", Refusal: limit || loginPhrases.MatchString(message), Limit: limit}
	}
	return nil
}

// phrasePattern matches any of phrases as whole words, with any run of
// spaces between the words.
func phrasePattern(phrases ...string) *regexp.Regexp {
	alternatives := make([]string, len(phrases))
	for i, phrase := range phrases {
		words := strings.Fields(phrase)
		for j, word := range words {
			words[j] = regexp.QuoteMeta(word)
		}
		alternatives[i] = strings.Join(words, `\s+`)
	}
	return regexp.MustCompile(`\b(?:` + strings.Join(alternatives, "|") + `)\b`)
}

// limitPhrases mark a message that refuses downloads for now because of a
// download limit or quota; loginPhrases one that asks to log in. Underscores
// in the message count as spaces, so "limit_exceeded" is "limit exceeded".
var (
	limitPhrases = phrasePattern("download limit", "daily limit", "limit reached", "limit exceeded", "quota", "too many requests", "too many downloads")
	loginPhrases = phrasePattern("unauthorized", "not authorized", "log in to", "login required", "please log in", "please login", "sign in to")
	// passwordField is an <input> whose type is password.
	passwordField = regexp.MustCompile(`<input\b[^>]*\btype\s*=\s*["']?password\b`)
	// hiddenElement is a script or style element or a comment, which may
	// mention "quota" or a password field without the page saying so.
	hiddenElement = regexp.MustCompile(`(?s)<(script|style)\b.*?(?:</(?:script|style)\s*>|$)|<!--.*?(?:-->|$)`)
	markupTag     = regexp.MustCompile(`(?s)<[^>]*>`)
)

// loginFormTextLimit is how much visible text a page with a password field
// may have to count as a login page. A site page with a login box in its
// header (a 404 page, a deleted subtitle's page) has far more.
const loginFormTextLimit = 2 << 10

// refusalDocument recognizes an HTML limit or login page by its visible text,
// with scripts, styles and comments removed: a limit or login message, or a
// password field on a page that is little more than a login form. A
// navigation "Login" link on an ordinary error page does not count.
func refusalDocument(text []byte) bool {
	markup := hiddenElement.ReplaceAllString(strings.ToLower(string(text)), " ")
	visible := strings.Join(strings.Fields(html.UnescapeString(markupTag.ReplaceAllString(markup, " "))), " ")
	message := strings.ReplaceAll(visible, "_", " ")
	if limitPhrases.MatchString(message) || loginPhrases.MatchString(message) {
		return true
	}
	return len(visible) < loginFormTextLimit && passwordField.MatchString(markup)
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
	return &ProviderResponseError{Reason: "Titlovi placeholder message", Refusal: true}
}

// refusalLedger remembers, for one workflow run, the first refused download
// of each provider. One candidate's page is not enough to pause a provider: a
// deleted subtitle can redirect to a site page that reads like a refusal, and
// pausing on it would retry the same candidate first after every cooldown.
type refusalLedger struct {
	first map[string]refusalWitness
}

type refusalWitness struct {
	resultID string
	err      *ProviderResponseError
}

// refusedDownload turns a limit or login message into a download cooldown of
// the provider, so the search ends as throttled and keeps its place instead of
// failing candidate after candidate. The first refusal of a provider in a run
// fails only its candidate, as an ordinary error page does; a refusal from a
// second, distinct candidate of that provider confirms it. An explicit limit
// in a JSON body pauses at once. Any other error is returned as it is.
func refusedDownload(ctx context.Context, adapter provider.Provider, candidate domain.Candidate, ledger *refusalLedger, err error) error {
	var message *ProviderResponseError
	if !errors.As(err, &message) || !message.Refusal {
		return err
	}
	var first refusalWitness
	if !message.Limit {
		if ledger == nil {
			return err
		}
		if ledger.first == nil {
			ledger.first = map[string]refusalWitness{}
		}
		witness, seen := ledger.first[candidate.ProviderID]
		if !seen {
			ledger.first[candidate.ProviderID] = refusalWitness{resultID: candidate.ResultID, err: message}
			return err
		}
		if witness.resultID == candidate.ResultID {
			return err
		}
		first = witness
	}
	recorded := provider.RecordDownloadRefusal(ctx, adapter)
	if recorded == nil {
		return err
	}
	var cooldown *provider.CooldownError
	if !errors.As(recorded, &cooldown) {
		// The cooldown could not be stored: a state-store failure ends the
		// workflow like every other one.
		return &provider.AvailabilityError{Err: recorded}
	}
	if first.err != nil {
		first.err.confirmed = recorded
	}
	return recorded
}
