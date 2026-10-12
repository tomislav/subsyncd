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

// Text renders the match on one line: movies as `inst movie 42 "Dune" (2021)`,
// episodes as `inst episode 1234 "Shameless" S01E01` (S01E01-E02 for a range).
func (m MediaMatch) Text() string {
	line := fmt.Sprintf("%s %s %d %q", m.Instance, m.Kind, m.FileID, m.Title)
	if m.Kind == "episode" {
		if m.Season > 0 || m.Episode > 0 {
			line += fmt.Sprintf(" S%02dE%02d", m.Season, m.Episode)
			if m.EpisodeEnd > m.Episode {
				line += fmt.Sprintf("-E%02d", m.EpisodeEnd)
			}
		}
	} else if m.Year > 0 {
		line += fmt.Sprintf(" (%d)", m.Year)
	}
	return line
}

// CandidatesText lists matches one per line.
func CandidatesText(matches []MediaMatch) string {
	lines := make([]string, len(matches))
	for i, match := range matches {
		lines[i] = match.Text()
	}
	return strings.Join(lines, "\n")
}

// LanguageState is one configured language's state for a media file: one of
// installed, satisfied, missing or running.
type LanguageState struct {
	Language string `json:"language"`
	State    string `json:"state"`
}

// FindMatch is a media file a find selection matched and where each
// configured language stands.
type FindMatch struct {
	MediaMatch
	Languages []LanguageState `json:"languages"`
}

// FindResult lists every file a selection matched. Several matches are a
// success: it is how a caller sees the candidates.
type FindResult struct {
	Matches []FindMatch `json:"matches"`
}

func (r FindResult) Text() string {
	if len(r.Matches) == 0 {
		return "no matches"
	}
	var lines []string
	for _, match := range r.Matches {
		lines = append(lines, match.Text())
		for _, language := range match.Languages {
			lines = append(lines, fmt.Sprintf("  %s: %s", language.Language, language.State))
		}
	}
	return strings.Join(lines, "\n")
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

// RejectReasons are the reasons reject accepts; the rejection is stored with
// reason code "user_" + reason.
var RejectReasons = []string{"out_of_sync", "wrong_language", "wrong_release", "poor_quality", "other"}

// ValidRejectReason reports whether reason is one of RejectReasons.
func ValidRejectReason(reason string) bool {
	for _, known := range RejectReasons {
		if reason == known {
			return true
		}
	}
	return false
}

// RejectArgs are the arguments of the reject command. Language may be empty
// when the file has exactly one managed subtitle. Undo removes the owner's
// rejections instead and needs no Reason.
type RejectArgs struct {
	Selection
	Language string        `json:"language,omitempty"`
	Reason   string        `json:"reason,omitempty"`
	Undo     bool          `json:"undo,omitempty"`
	Wait     time.Duration `json:"wait,omitempty"`
	NoWait   bool          `json:"no_wait,omitempty"`
}

// RejectResult reports a reject: the follow-up search's outcome for the
// language (removed when nothing acceptable replaced the subtitle), or, for
// --undo, how many of the owner's rejections were removed.
type RejectResult struct {
	MediaID  int64           `json:"media_id"`
	Rejected LanguageOutcome `json:"rejected"`
	Undone   int             `json:"undone"`
	Undo     bool            `json:"undo,omitempty"`
}

func (r RejectResult) Text() string {
	if r.Undo {
		return fmt.Sprintf("media_id: %d\nlanguage: %s\nundone: %d", r.MediaID, r.Rejected.Language, r.Undone)
	}
	lines := []string{fmt.Sprintf("media_id: %d", r.MediaID), "language: " + r.Rejected.Language, "outcome: " + r.Rejected.Outcome}
	language := r.Rejected
	if language.Reason != "" {
		lines = append(lines, "reason: "+language.Reason)
	}
	if language.Provider != "" {
		lines = append(lines, "provider: "+language.Provider, "candidate: "+language.Candidate, fmt.Sprintf("score: %d", language.Score))
	}
	if language.Subtitle != "" {
		lines = append(lines, "subtitle: "+language.Subtitle)
	}
	if !language.NextAttemptAt.IsZero() {
		lines = append(lines, "retry_at: "+language.NextAttemptAt.Format(time.RFC3339Nano))
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

	// Languages holds one full result per configured language when explain ran
	// without --language. The fields above then only identify the media and its
	// inventory; each entry repeats them with that language's details.
	Languages []ExplainResult `json:"languages,omitempty"`
}

func (r ExplainResult) Text() string {
	if len(r.Languages) > 0 {
		sections := make([]string, len(r.Languages))
		for i, language := range r.Languages {
			sections[i] = "language: " + language.Language + "\n" + language.Text()
		}
		return strings.Join(sections, "\n\n")
	}
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
	History        int64  `json:"history"`
}

func (r ForgetInstanceResult) Text() string {
	counts := fmt.Sprintf("media=%d searches=%d installations=%d rollback_copies=%d candidates=%d rejections=%d notifications=%d events=%d replays=%d history=%d",
		r.Media, r.Searches, r.Installations, r.RollbackCopies, r.Candidates, r.Rejections, r.Notifications, r.Events, r.Replays, r.History)
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

// MissingArgs are the arguments of the missing command. Limit zero means the
// default of 50.
type MissingArgs struct {
	Instance string `json:"instance,omitempty"`
	Language string `json:"language,omitempty"`
	Title    string `json:"title,omitempty"`
	Limit    int    `json:"limit,omitempty"`
}

// MissingItem is one media file and configured language that has no
// subtitle yet, why, and what to run about it. Providers names the cooling
// providers for providers_cooling_down. NextAttemptAt is the next scheduled
// search, zero when none is queued or one is running. Action is a complete
// command, or for unsupported and media_unavailable a short instruction.
type MissingItem struct {
	MediaMatch
	Language      string    `json:"language"`
	Reason        string    `json:"reason"`
	Providers     []string  `json:"providers,omitempty"`
	NextAttemptAt time.Time `json:"next_attempt_at,omitzero"`
	Action        string    `json:"action"`
}

// MissingResult lists the missing subtitles in queue order. Total counts every
// one before the limit cut Items.
type MissingResult struct {
	Total int           `json:"total"`
	Items []MissingItem `json:"items"`
}

func (r MissingResult) Text() string {
	if r.Total == 0 {
		return "nothing is missing"
	}
	lines := []string{fmt.Sprintf("%d missing (showing %d)", r.Total, len(r.Items))}
	for _, item := range r.Items {
		line := fmt.Sprintf("%s %s: %s", item.MediaMatch.Text(), item.Language, item.Reason)
		if len(item.Providers) > 0 {
			line += " (" + strings.Join(item.Providers, ", ") + ")"
		}
		if !item.NextAttemptAt.IsZero() {
			line += " next=" + item.NextAttemptAt.UTC().Format(time.RFC3339)
		}
		lines = append(lines, line, "  action: "+item.Action)
	}
	return strings.Join(lines, "\n")
}

// DaemonState is whether the daemon answers on its socket. Error is a short
// reason, without paths, when something answered badly or not at all.
type DaemonState struct {
	Running   bool      `json:"running"`
	Version   string    `json:"version,omitempty"`
	StartedAt time.Time `json:"started_at,omitzero"`
	Error     string    `json:"error,omitempty"`
}

// QueueClass counts the pending searches of one class: all of them, and those
// due now (scheduled, and not running).
type QueueClass struct {
	Total int `json:"total"`
	Due   int `json:"due"`
}

// QueueCounts counts pending searches by class: new (just imported or
// renamed, or queued by a user rejection), missing, and upgrade checks.
type QueueCounts struct {
	New     QueueClass `json:"new"`
	Missing QueueClass `json:"missing"`
	Upgrade QueueClass `json:"upgrade"`
}

// RunningSearch is a search holding a live lease. The database records when
// the lease runs out, not when the search began.
type RunningSearch struct {
	MediaMatch
	Language   string    `json:"language"`
	LeaseUntil time.Time `json:"lease_until"`
}

// ProviderState is a provider that cannot be used right now. State is
// cooling_down, auth_failed or quota_exhausted; Until is when it is usable
// again, zero when only fixing the credentials or retry --provider helps.
type ProviderState struct {
	ID          string    `json:"id"`
	State       string    `json:"state"`
	Until       time.Time `json:"until,omitzero"`
	LastFailure string    `json:"last_failure,omitempty"`
}

// MediaPause is the pause after a run of searches that could not read their
// media.
type MediaPause struct {
	Until time.Time `json:"until"`
}

// InstanceState is one Arr instance's reconciliation, discovery and webhook
// state. LastReconcileAt is the last attempt; it succeeded when
// ReconcileFailures is 0. Webhook is connected (subsyncd keeps the Arr
// connection and checked it at WebhookCheckedAt), pending (it manages the
// connection but has not recorded one), or unmanaged (no
// server.webhook_base_url).
type InstanceState struct {
	Name              string    `json:"name"`
	LastReconcileAt   time.Time `json:"last_reconcile_at,omitzero"`
	ReconcileFailures int       `json:"reconcile_failures"`
	DiscoveryComplete bool      `json:"discovery_complete"`
	Webhook           string    `json:"webhook"`
	WebhookCheckedAt  time.Time `json:"webhook_checked_at,omitzero"`
}

// Backlog counts work waiting outside the search queue.
type Backlog struct {
	WebhookInbox       int `json:"webhook_inbox"`
	NotificationOutbox int `json:"notification_outbox"`
}

// StatusResult is the overview an agent reads before acting. Daemon and
// MediaPause come from the running daemon; everything else from the database.
type StatusResult struct {
	Daemon     DaemonState     `json:"daemon"`
	Queue      QueueCounts     `json:"queue"`
	Running    []RunningSearch `json:"running"`
	Providers  []ProviderState `json:"providers"`
	MediaPause *MediaPause     `json:"media_pause,omitempty"`
	Instances  []InstanceState `json:"instances"`
	Backlog    Backlog         `json:"backlog"`
}

func (r StatusResult) Text() string {
	var lines []string
	switch {
	case r.Daemon.Running:
		line := "daemon: running"
		if r.Daemon.Version != "" {
			line += " version=" + r.Daemon.Version
		}
		if !r.Daemon.StartedAt.IsZero() {
			line += " started=" + r.Daemon.StartedAt.UTC().Format(time.RFC3339)
		}
		lines = append(lines, line)
	case r.Daemon.Error != "":
		lines = append(lines, "daemon: not running ("+r.Daemon.Error+")")
	default:
		lines = append(lines, "daemon: not running")
	}
	for _, class := range []struct {
		name  string
		count QueueClass
	}{{"new", r.Queue.New}, {"missing", r.Queue.Missing}, {"upgrade", r.Queue.Upgrade}} {
		lines = append(lines, fmt.Sprintf("queue %s: %d pending, %d due", class.name, class.count.Total, class.count.Due))
	}
	if r.MediaPause != nil {
		lines = append(lines, "media unavailable: searches paused until "+r.MediaPause.Until.UTC().Format(time.RFC3339))
	}
	for _, run := range r.Running {
		lines = append(lines, fmt.Sprintf("running: %s %s (lease until %s)", run.MediaMatch.Text(), run.Language, run.LeaseUntil.UTC().Format(time.RFC3339)))
	}
	for _, provider := range r.Providers {
		lines = append(lines, "provider "+provider.ID+": "+providerStateText(provider.State, provider.Until, provider.LastFailure))
	}
	for _, instance := range r.Instances {
		line := fmt.Sprintf("instance %s: discovery=%s webhook=%s", instance.Name, map[bool]string{true: "complete", false: "incomplete"}[instance.DiscoveryComplete], instance.Webhook)
		if !instance.LastReconcileAt.IsZero() {
			line += " reconciled=" + instance.LastReconcileAt.UTC().Format(time.RFC3339)
			if instance.ReconcileFailures > 0 {
				line += fmt.Sprintf(" (failing, %d in a row)", instance.ReconcileFailures)
			}
		}
		lines = append(lines, line)
	}
	lines = append(lines, fmt.Sprintf("backlog: %d webhooks waiting, %d notifications waiting", r.Backlog.WebhookInbox, r.Backlog.NotificationOutbox))
	return strings.Join(lines, "\n")
}

func providerStateText(state string, until time.Time, failure string) string {
	text := state
	if !until.IsZero() {
		text += " until " + until.UTC().Format(time.RFC3339)
	}
	if failure != "" {
		text += " (" + failure + ")"
	}
	return text
}

// ProviderRoute is a language a provider serves and at which tier: preferred
// providers are tried first, fallback providers when those find nothing.
type ProviderRoute struct {
	Language string `json:"language"`
	Tier     string `json:"tier"`
}

// ProviderCounts tallies a provider's finished searches over a window.
type ProviderCounts struct {
	Installed int `json:"installed"`
	NoResult  int `json:"no_result"`
	Rejected  int `json:"rejected"`
}

// ProviderRow is one configured provider.
type ProviderRow struct {
	ID            string          `json:"id"`
	Type          string          `json:"type"`
	Routes        []ProviderRoute `json:"routes"`
	State         string          `json:"state"`
	Until         time.Time       `json:"until,omitzero"`
	LastFailure   string          `json:"last_failure,omitempty"`
	LastSuccessAt time.Time       `json:"last_success_at,omitzero"`
	Last24h       ProviderCounts  `json:"last_24h"`
}

// ProvidersResult lists every configured provider.
type ProvidersResult struct {
	Providers []ProviderRow `json:"providers"`
}

func (r ProvidersResult) Text() string {
	if len(r.Providers) == 0 {
		return "no providers"
	}
	var lines []string
	for _, row := range r.Providers {
		routes := make([]string, len(row.Routes))
		for i, route := range row.Routes {
			routes[i] = route.Language + ":" + route.Tier
		}
		line := fmt.Sprintf("%s (%s): %s", row.ID, row.Type, providerStateText(row.State, row.Until, row.LastFailure))
		if len(routes) > 0 {
			line += " routes=" + strings.Join(routes, ",")
		}
		line += fmt.Sprintf(" last24h installed=%d no_result=%d rejected=%d", row.Last24h.Installed, row.Last24h.NoResult, row.Last24h.Rejected)
		if !row.LastSuccessAt.IsZero() {
			line += " last_success=" + row.LastSuccessAt.UTC().Format(time.RFC3339)
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}
