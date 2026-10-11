package workflow

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"maps"
	"mime"
	"os"
	"path"
	"regexp"
	"strings"
	"sync"
	"time"

	"subsyncd/internal/domain"
	"subsyncd/internal/pack"
	"subsyncd/internal/provider"
)

// ProviderResponseError is a download that delivered the provider's own
// message instead of a subtitle: an HTML login or limit page, a JSON error
// body, or Titlovi's placeholder subtitle. It is a provider problem that is
// retried, never a rejection of the candidate. Refusal marks a limit, login
// or client-block message, which pauses the provider's downloads once a
// second candidate of the same provider confirms it (see RefusalLedger);
// Limit marks an unambiguous refusal, an explicit limit or quota in a JSON
// body, which pauses them at once. Cooldown is the kind of pause a refusal
// records: a rate limit for "too many requests" wording, a client block for
// Titlovi's placeholder, otherwise the download quota.
type ProviderResponseError struct {
	Reason   string
	Refusal  bool
	Limit    bool
	Cooldown provider.CooldownKind
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
// does not authorize a download; its one cue says the viewer uses a plugin
// Titlovi does not support and should install the official one.
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
		refusal, cooldown := refusalDocument(text)
		return &ProviderResponseError{Reason: "HTML page", Refusal: refusal, Cooldown: cooldown}
	case strings.HasPrefix(lower, "<head") || strings.HasPrefix(lower, "<body"):
		return &ProviderResponseError{Reason: "HTML page"}
	case mediaType == "text/html" && strings.HasPrefix(lower, "<"):
		return &ProviderResponseError{Reason: "HTML page"}
	case len(head) <= sniffLimit && (strings.HasPrefix(lower, "{") || strings.HasPrefix(lower, "[")) && json.Valid(text):
		message := strings.ReplaceAll(strings.ToLower(jsonMessage(text)), "_", " ")
		limit := limitPhrases.MatchString(message)
		return &ProviderResponseError{Reason: "JSON body", Refusal: limit || loginPhrases.MatchString(message) || unauthorizedAccess.MatchString(message), Limit: limit, Cooldown: refusalCooldown(message)}
	}
	return nil
}

// jsonMessageFields are the fields of a JSON error body that carry its
// message. Only their string values are read: a field name such as
// "remaining_quota" says nothing about the answer.
var jsonMessageFields = map[string]bool{"error": true, "message": true, "status": true, "detail": true}

// jsonMessage joins the string values of a JSON body's message fields, in
// the body or in objects nested under them ({"error":{"message":"..."}}), and
// in each object of a top-level array.
func jsonMessage(text []byte) string {
	var body any
	if json.Unmarshal(text, &body) != nil {
		return ""
	}
	var parts []string
	var collect func(value any, message bool)
	collect = func(value any, message bool) {
		switch value := value.(type) {
		case string:
			if message {
				parts = append(parts, value)
			}
		case map[string]any:
			for key, field := range value {
				if jsonMessageFields[strings.ToLower(key)] {
					collect(field, true)
				}
			}
		case []any:
			for _, item := range value {
				collect(item, message)
			}
		}
	}
	collect(body, false)
	return strings.Join(parts, " ")
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
// download limit or quota; loginPhrases one that asks to log in, and
// unauthorizedAccess one that calls a download or access unauthorized
// ("Unauthorized access", "unauthorized to download"), not any use of the word
// ("Unauthorized reproduction prohibited"). Underscores in the message count
// as spaces, so "limit_exceeded" is "limit exceeded".
var (
	limitPhrases = phrasePattern("download limit", "daily limit", "limit reached", "limit exceeded", "quota", "too many requests", "too many downloads")
	// rateLimitPhrases mark a limit on how fast requests come, and
	// quotaPhrases one on how much may be downloaded: a message with the
	// first and none of the second is a short rate limit, not the quota.
	rateLimitPhrases   = phrasePattern("too many requests", "rate limit", "rate limited")
	quotaPhrases       = phrasePattern("download limit", "daily limit", "quota", "too many downloads")
	loginPhrases       = phrasePattern("not authorized", "log in to", "login required", "please log in", "please login", "sign in to")
	unauthorizedAccess = regexp.MustCompile(`\bunauthori[sz]ed(?:\s+\S+){0,2}?\s+(?:access|download|downloads|downloading)\b|\b(?:access|download|downloads|downloading)(?:\s+\S+){0,2}?\s+unauthori[sz]ed\b`)
	// passwordField is an <input> whose type is password.
	passwordField = regexp.MustCompile(`<input\b[^>]*\btype\s*=\s*["']?password\b`)
	// hiddenElement is an element whose text the page does not show as its
	// message (script, style, noscript, textarea, template) or a comment,
	// which may mention "quota" or a password field without the page saying
	// so.
	hiddenElement = regexp.MustCompile(`(?s)<(script|style|noscript|textarea|template)\b.*?(?:</(?:script|style|noscript|textarea|template)\s*>|$)|<!--.*?(?:-->|$)`)
	markupTag     = regexp.MustCompile(`(?s)<[^>]*>`)
)

// loginFormTextLimit is how much visible text a page may have to count as a
// login page. A site page with a login box in its header or a "Log in to post
// a comment" prompt (a 404 page, a deleted subtitle's page) has far more.
const loginFormTextLimit = 2 << 10

// refusalDocument recognizes an HTML limit or login page by its visible text,
// with scripts, styles, other unshown elements and comments removed: a limit
// message or an unauthorized download on any page, or a login message or
// password field on a page that is little more than a login form. A
// navigation "Login" link on an ordinary error page does not count.
// It also returns the kind of cooldown a confirmed refusal records.
func refusalDocument(text []byte) (bool, provider.CooldownKind) {
	markup := hiddenElement.ReplaceAllString(strings.ToLower(string(text)), " ")
	visible := strings.Join(strings.Fields(html.UnescapeString(markupTag.ReplaceAllString(markup, " "))), " ")
	message := strings.ReplaceAll(visible, "_", " ")
	if limitPhrases.MatchString(message) || unauthorizedAccess.MatchString(message) {
		return true, refusalCooldown(message)
	}
	return len(visible) < loginFormTextLimit && (loginPhrases.MatchString(message) || passwordField.MatchString(markup)), provider.CooldownDownloadQuota
}

// refusalCooldown is the cooldown a refusal message calls for: a rate limit
// when it says too many requests or names a rate limit and names no download
// limit or quota, otherwise the download quota.
func refusalCooldown(message string) provider.CooldownKind {
	if rateLimitPhrases.MatchString(message) && !quotaPhrases.MatchString(message) {
		return provider.CooldownRateLimit
	}
	return provider.CooldownDownloadQuota
}

// checkPlaceholderMembers recognizes an archive holding only Titlovi's
// placeholder message. Its text says the client is a plugin Titlovi does not
// support, so it is a client block rather than a limit: like a refusal page,
// it pauses Titlovi's downloads only once a second candidate confirms it, and
// then for an hour as client_blocked.
func checkPlaceholderMembers(manifest pack.Manifest) error {
	if len(manifest.Members) == 0 {
		return nil
	}
	for _, member := range manifest.Members {
		if !strings.EqualFold(path.Base(strings.ReplaceAll(member.SafeName, "\\", "/")), titloviPlaceholder) {
			return nil
		}
	}
	return &ProviderResponseError{Reason: "Titlovi placeholder message (client_blocked)", Refusal: true, Cooldown: provider.CooldownClientBlocked}
}

// refusalWindow is how close together two refusals of one provider must be
// to confirm each other.
const refusalWindow = 30 * time.Minute

// RefusalLedger remembers recent refused downloads of each provider, across
// workflow runs and languages. One candidate's page is not enough to pause a
// provider: a deleted subtitle can redirect to a site page that reads like a
// refusal, and pausing on it would retry the same candidate first after every
// cooldown. Two distinct candidates of the same provider refusing within
// refusalWindow, in one search or in several, confirm it. The zero value is
// ready to use and safe for concurrent runs.
type RefusalLedger struct {
	mu        sync.Mutex
	witnesses map[string]refusalWitness
}

type refusalWitness struct {
	resultID string
	at       time.Time
	// run and err identify the failure the refusal produced, which a
	// confirmation in the same run marks as the provider being unavailable.
	run *refusalRun
	err *ProviderResponseError
}

// refusalRun is one workflow run's view of the ledger.
type refusalRun struct {
	ledger *RefusalLedger
	now    func() time.Time
}

// witness records a refusal and reports the earlier refusal of another
// candidate of the same provider that confirms it, if any. Each provider holds
// at most one unconfirmed refusal, since any other candidate confirms it; a
// confirmation clears it, as the cooldown it records takes over. Refusals
// older than refusalWindow are pruned.
func (l *RefusalLedger) witness(providerID string, current refusalWitness) (refusalWitness, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	maps.DeleteFunc(l.witnesses, func(_ string, w refusalWitness) bool { return current.at.Sub(w.at) > refusalWindow })
	earlier, seen := l.witnesses[providerID]
	if seen && earlier.resultID != current.resultID {
		delete(l.witnesses, providerID)
		return earlier, true
	}
	if seen && earlier.run == current.run {
		// The candidate refused again in the same run: keep the failure it
		// first produced, and refresh its time.
		current.err = earlier.err
	}
	if l.witnesses == nil {
		l.witnesses = map[string]refusalWitness{}
	}
	l.witnesses[providerID] = current
	return refusalWitness{}, false
}

// refusedDownload turns a limit or login message into a download cooldown of
// the provider, so the search ends as throttled and keeps its place instead of
// failing candidate after candidate. An unconfirmed refusal fails only its
// candidate, as a retryable provider error like an ordinary error page; a
// refusal from a second, distinct candidate of that provider within
// refusalWindow confirms it. An explicit limit in a JSON body pauses at once.
// The cooldown recorded is the message's kind (see ProviderResponseError).
// Any other error is returned as it is.
func refusedDownload(ctx context.Context, adapter provider.Provider, candidate domain.Candidate, run *refusalRun, err error) error {
	var message *ProviderResponseError
	if !errors.As(err, &message) || !message.Refusal {
		return err
	}
	var first refusalWitness
	if !message.Limit {
		if run == nil || run.ledger == nil {
			return err
		}
		earlier, confirmed := run.ledger.witness(candidate.ProviderID, refusalWitness{resultID: candidate.ResultID, at: run.now(), run: run, err: message})
		if !confirmed {
			return err
		}
		first = earlier
	}
	kind := message.Cooldown
	if kind == "" {
		kind = provider.CooldownDownloadQuota
	}
	recorded := provider.RecordDownloadRefusal(ctx, adapter, kind)
	if recorded == nil {
		return err
	}
	var cooldown *provider.CooldownError
	if !errors.As(recorded, &cooldown) {
		// The cooldown could not be stored: a state-store failure ends the
		// workflow like every other one.
		return &provider.AvailabilityError{Err: recorded}
	}
	if first.err != nil && first.run == run {
		// The earlier refusal failed a candidate of this run, which now
		// counts as the provider being unavailable too. A refusal from
		// another run belongs to that run and is left alone.
		first.err.confirmed = recorded
	}
	return recorded
}
