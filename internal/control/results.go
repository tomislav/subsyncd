package control

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Result is what a command returns. Text renders the plain-text output the CLI
// prints without --json; the struct itself is the JSON result.
type Result interface {
	Text() string
}

// Selection names one media file. Instance, Kind and FileID identify it
// exactly; Title and Path are the looser forms a later command accepts.
type Selection struct {
	Instance string `json:"instance,omitempty"`
	Kind     string `json:"kind,omitempty"`
	FileID   int64  `json:"file_id,omitempty"`
	Title    string `json:"title,omitempty"`
	Path     string `json:"path,omitempty"`
}

// SearchArgs are the arguments of the search command.
type SearchArgs struct {
	Selection
	Language      string        `json:"language"`
	RetryRejected bool          `json:"retry_rejected,omitempty"`
	Wait          time.Duration `json:"wait,omitempty"`
	NoWait        bool          `json:"no_wait,omitempty"`
}

// ExplainArgs are the arguments of the explain command.
type ExplainArgs struct {
	Selection
	Language string `json:"language"`
}

// ScanResult reports one reconciliation of an Arr instance.
type ScanResult struct {
	Instance   string `json:"instance"`
	Media      int    `json:"media"`
	ForceProbe bool   `json:"force_probe"`
	// HistoryDeferred is set when Arr could not supply some entities; the
	// scan still completed for everything else.
	HistoryDeferred bool     `json:"history_deferred,omitempty"`
	Deferred        int      `json:"deferred,omitempty"`
	DeferredMedia   []string `json:"deferred_media,omitempty"`
}

func (r ScanResult) Text() string {
	summary := fmt.Sprintf("scan complete: instance=%s media=%d force_probe=%t", r.Instance, r.Media, r.ForceProbe)
	if r.HistoryDeferred {
		summary += fmt.Sprintf(" deferred=%d (%s)", r.Deferred, strings.Join(r.DeferredMedia, "; "))
	}
	return summary
}

// LanguageOutcome is the result of searching for one language.
type LanguageOutcome struct {
	Language      string    `json:"language"`
	Outcome       string    `json:"outcome"`
	Provider      string    `json:"provider,omitempty"`
	Candidate     string    `json:"candidate,omitempty"`
	Release       string    `json:"release,omitempty"`
	Reason        string    `json:"reason,omitempty"`
	Score         int       `json:"score,omitempty"`
	SyncVerdict   string    `json:"sync_verdict,omitempty"`
	OffsetMS      int64     `json:"offset_ms,omitempty"`
	Subtitle      string    `json:"subtitle,omitempty"`
	NextAttemptAt time.Time `json:"next_attempt_at,omitzero"`
}

// SearchResult reports a manual search.
type SearchResult struct {
	MediaID   int64             `json:"media_id"`
	Languages []LanguageOutcome `json:"languages"`
}

func (r SearchResult) Text() string {
	var lines []string
	lines = append(lines, fmt.Sprintf("media_id: %d", r.MediaID))
	for _, language := range r.Languages {
		if len(r.Languages) > 1 {
			lines = append(lines, "language: "+language.Language)
		}
		lines = append(lines, "outcome: "+language.Outcome)
		if language.Provider != "" {
			lines = append(lines, "provider: "+language.Provider, "candidate: "+language.Candidate, fmt.Sprintf("score: %d", language.Score))
		}
		if !language.NextAttemptAt.IsZero() {
			lines = append(lines, "retry_at: "+language.NextAttemptAt.Format(time.RFC3339Nano))
		}
	}
	return strings.Join(lines, "\n")
}

// RetryArgs is the request of the retry command.
type RetryArgs struct {
	Provider string `json:"provider"`
}

// RetryResult reports a provider retry-state reset.
type RetryResult struct {
	Provider    string `json:"provider"`
	SearchesDue int64  `json:"searches_due"`
}

func (r RetryResult) Text() string {
	return fmt.Sprintf("provider retry state reset: %s; throttled searches due again: %d", r.Provider, r.SearchesDue)
}

// ExplainTrack is one subtitle track in a media file's inventory.
type ExplainTrack struct {
	Language  string `json:"language"`
	Embedded  bool   `json:"embedded"`
	Forced    bool   `json:"forced"`
	SDH       bool   `json:"sdh"`
	Protected bool   `json:"protected"`
}

// ExplainSearch is the scheduling state of one language's search.
type ExplainSearch struct {
	State          string    `json:"state"`
	Outcome        string    `json:"outcome"`
	Priority       string    `json:"priority"`
	RerunPending   bool      `json:"rerun_pending"`
	MissingAttempt int       `json:"missing_attempt"`
	FailureAttempt int       `json:"failure_attempt"`
	NextAttemptAt  time.Time `json:"next_attempt_at,omitzero"`
}

// ExplainCandidate is one stored candidate; Score and Identity are the stored
// compact JSON documents.
type ExplainCandidate struct {
	Provider string          `json:"provider"`
	ResultID string          `json:"result_id"`
	Score    json.RawMessage `json:"score"`
	Identity json.RawMessage `json:"identity"`
}

// ExplainRejection is one active candidate rejection.
type ExplainRejection struct {
	Provider string `json:"provider"`
	ResultID string `json:"result_id"`
	Reason   string `json:"reason"`
	Artifact string `json:"artifact"`
}

// ExplainInstallation is the installed subtitle's provenance.
type ExplainInstallation struct {
	Provider  string          `json:"provider"`
	Candidate string          `json:"candidate"`
	Checksum  string          `json:"checksum"`
	Score     json.RawMessage `json:"score"`
	Lapse     json.RawMessage `json:"lapse"`
	Rollback  bool            `json:"rollback"`
	Fallback  bool            `json:"fallback"`
}

// ExplainProviderState is one provider availability record.
type ExplainProviderState struct {
	Provider  string    `json:"provider"`
	Scope     string    `json:"scope"`
	Reason    string    `json:"reason"`
	Remaining int64     `json:"remaining"`
	ResetAt   time.Time `json:"reset_at,omitzero"`
	Disabled  bool      `json:"disabled"`
}

// ExplainResult reports why a media file and language are in their current
// state. It carries no filesystem paths.
type ExplainResult struct {
	MediaID           int64  `json:"media_id"`
	Instance          string `json:"instance"`
	Kind              string `json:"kind"`
	FileID            int64  `json:"file_id"`
	Language          string `json:"language,omitempty"`
	Title             string `json:"title"`
	Episodes          string `json:"episodes,omitempty"`
	UnsupportedReason string `json:"unsupported_reason,omitempty"`

	Probe            string         `json:"probe"`
	FingerprintSize  int64          `json:"fingerprint_size"`
	FingerprintMTime time.Time      `json:"fingerprint_mtime,omitzero"`
	Tracks           []ExplainTrack `json:"tracks"`

	// Search is nil when no search is scheduled.
	Search *ExplainSearch `json:"search"`

	Candidates           []ExplainCandidate     `json:"candidates"`
	Rejections           []ExplainRejection     `json:"rejections"`
	RejectionsSuperseded int                    `json:"rejections_superseded,omitempty"`
	Installation         *ExplainInstallation   `json:"installation"`
	PackCacheReusable    int                    `json:"pack_cache_reusable"`
	ProviderStates       []ExplainProviderState `json:"provider_states"`
}

func (r ExplainResult) Text() string {
	var output strings.Builder
	fmt.Fprintf(&output, "media: id=%d instance=%s kind=%s file_id=%d title=%q\n", r.MediaID, r.Instance, r.Kind, r.FileID, r.Title)
	if r.Episodes != "" {
		fmt.Fprintf(&output, "episodes=%s\n", r.Episodes)
	}
	if r.UnsupportedReason != "" {
		fmt.Fprintf(&output, "unsupported_reason=%s\n", r.UnsupportedReason)
	}
	fmt.Fprintf(&output, "inventory: tracks=%d probe=%s fingerprint_size=%d fingerprint_mtime=%s\n", len(r.Tracks), r.Probe, r.FingerprintSize, r.FingerprintMTime.Format(time.RFC3339Nano))
	for _, track := range r.Tracks {
		fmt.Fprintf(&output, "  track: language=%s embedded=%t forced=%t sdh=%t protected=%t\n", track.Language, track.Embedded, track.Forced, track.SDH, track.Protected)
	}
	if status := r.Search; status != nil {
		fmt.Fprintf(&output, "search: state=%s outcome=%s priority=%s rerun_pending=%t missing_attempt=%d failure_attempt=%d next_attempt=%s\n", status.State, status.Outcome, status.Priority, status.RerunPending, status.MissingAttempt, status.FailureAttempt, status.NextAttemptAt.Format(time.RFC3339Nano))
	} else {
		output.WriteString("search: not scheduled\n")
	}
	fmt.Fprintf(&output, "candidates: %d\n", len(r.Candidates))
	for _, candidate := range r.Candidates {
		fmt.Fprintf(&output, "  candidate: provider=%s result=%s score=%s identity=%s\n", candidate.Provider, candidate.ResultID, candidate.Score, candidate.Identity)
	}
	fmt.Fprintf(&output, "candidate_rejections: %d\n", len(r.Rejections))
	if r.RejectionsSuperseded > 0 {
		fmt.Fprintf(&output, "candidate_rejections_superseded: %d\n", r.RejectionsSuperseded)
	}
	for _, rejection := range r.Rejections {
		fmt.Fprintf(&output, "  rejection: provider=%s result=%s reason=%s artifact=%s expires=never\n", rejection.Provider, rejection.ResultID, rejection.Reason, rejection.Artifact)
	}
	if installation := r.Installation; installation != nil {
		fmt.Fprintf(&output, "installation: provider=%s candidate=%s checksum=%s score=%s lapse=%s rollback=%t fallback=%t\n", installation.Provider, installation.Candidate, installation.Checksum, installation.Score, installation.Lapse, installation.Rollback, installation.Fallback)
	} else {
		output.WriteString("installation: none\n")
	}
	fmt.Fprintf(&output, "pack_cache: reusable=%d\n", r.PackCacheReusable)
	for _, state := range r.ProviderStates {
		fmt.Fprintf(&output, "provider_state: provider=%s scope=%s reason=%s remaining=%d reset=%s disabled=%t\n", state.Provider, state.Scope, state.Reason, state.Remaining, state.ResetAt.Format(time.RFC3339Nano), state.Disabled)
	}
	return strings.TrimRight(output.String(), "\n")
}

// DoctorResult reports the local readiness checks.
type DoctorResult struct {
	Configuration string `json:"configuration"`
	SQLite        string `json:"sqlite"`
	MediaRoots    int    `json:"media_roots"`
	LAPSE         string `json:"lapse"`
	FFprobe       string `json:"ffprobe"`
	// Webhooks is "manual" or "managed (<base URL>)".
	Webhooks string `json:"webhooks"`
}

func (r DoctorResult) Text() string {
	return fmt.Sprintf("configuration: %s\nsqlite: %s\nmedia roots: ok (%d)\nLAPSE: %s\nffprobe: %s\nwebhooks: %s", r.Configuration, r.SQLite, r.MediaRoots, r.LAPSE, r.FFprobe, r.Webhooks)
}

// AnalyzeSyncResult carries LAPSE's analysis document.
type AnalyzeSyncResult struct {
	Analysis json.RawMessage `json:"analysis"`
}

func (r AnalyzeSyncResult) Text() string {
	var out bytes.Buffer
	if err := json.Indent(&out, r.Analysis, "", "  "); err != nil {
		return string(r.Analysis)
	}
	return out.String()
}

// ForgetInstanceResult reports the records of a removed instance, deleted when
// Applied and only counted otherwise.
type ForgetInstanceResult struct {
	Instance       string `json:"instance"`
	Applied        bool   `json:"applied"`
	Media          int64  `json:"media"`
	Searches       int64  `json:"searches"`
	Installations  int64  `json:"installations"`
	RollbackCopies int64  `json:"rollback_copies"`
	Candidates     int64  `json:"candidates"`
	Rejections     int64  `json:"rejections"`
	Notifications  int64  `json:"notifications"`
	Events         int64  `json:"events"`
	Replays        int64  `json:"replays"`
}

func (r ForgetInstanceResult) Text() string {
	counts := fmt.Sprintf("media=%d searches=%d installations=%d rollback_copies=%d candidates=%d rejections=%d notifications=%d events=%d replays=%d",
		r.Media, r.Searches, r.Installations, r.RollbackCopies, r.Candidates, r.Rejections, r.Notifications, r.Events, r.Replays)
	leftOnDisk := "subtitle files installed for this instance stay on disk and will no longer be managed"
	if r.RollbackCopies > 0 {
		leftOnDisk += fmt.Sprintf(", as do %d hidden .subsyncd-rollback-* copies beside them, which you can delete by hand", r.RollbackCopies)
	}
	if !r.Applied {
		return fmt.Sprintf("forget-instance preview: instance=%s %s\n%s\nnothing was deleted; rerun with --yes to delete these records", r.Instance, counts, leftOnDisk)
	}
	return fmt.Sprintf("forget-instance complete: instance=%s %s\n%s", r.Instance, counts, leftOnDisk)
}

// WebhookURLResult carries the URL to enter in Arr; it includes the token.
type WebhookURLResult struct {
	Instance string `json:"instance"`
	URL      string `json:"url"`
}

func (r WebhookURLResult) Text() string { return r.URL }

// RotateTokenResult reports a rotated webhook token. It never carries a token.
type RotateTokenResult struct {
	Instance string `json:"instance"`
	// Managed is true when subsyncd updates the Arr connection itself at its
	// next start (server.webhook_base_url is set).
	Managed bool `json:"managed"`
}

func (r RotateTokenResult) Text() string {
	if r.Managed {
		return fmt.Sprintf("webhook token rotated for instance %s; start subsyncd now: the Arr connection is updated in Arr at that start.", r.Instance)
	}
	return fmt.Sprintf("webhook token rotated for instance %s; start subsyncd, then run subsyncd webhook-url --instance %s --base URL (the address Arr uses to reach subsyncd) and update the URL in Arr by hand.", r.Instance, r.Instance)
}
