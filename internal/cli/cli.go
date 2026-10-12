package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"subsyncd/internal/control"
)

// Schema names the JSON envelope --json writes to stdout.
const Schema = "subsyncd.cli/v1"

const (
	ExitOK      = 0
	ExitFailure = 1
	ExitUsage   = 2
)

type Backend interface {
	Close() error
	Serve(context.Context) error
	Scan(context.Context, control.ScanArgs) (control.ScanResult, error)
	Search(context.Context, control.SearchArgs) (control.SearchResult, error)
	Reject(context.Context, control.RejectArgs) (control.RejectResult, error)
	Retry(context.Context, string) (control.RetryResult, error)
	Explain(context.Context, control.ExplainArgs) (control.ExplainResult, error)
	Find(context.Context, control.Selection) (control.FindResult, error)
	Missing(context.Context, control.MissingArgs) (control.MissingResult, error)
	Recent(context.Context, control.RecentArgs) (control.RecentResult, error)
	Status(context.Context) (control.StatusResult, error)
	ListProviders(context.Context) (control.ProvidersResult, error)
	Doctor(context.Context) (control.DoctorResult, error)
	AnalyzeSync(context.Context, string, string) (control.AnalyzeSyncResult, error)
	ForgetInstance(context.Context, string, bool) (control.ForgetInstanceResult, error)
	WebhookURL(ctx context.Context, instance, base string) (control.WebhookURLResult, error)
	RotateWebhookToken(ctx context.Context, instance string) (control.RotateTokenResult, error)
}

// Remote is a connection to a running daemon that carries one call.
type Remote interface {
	Call(ctx context.Context, command string, args, result any) error
	Close() error
}

// DialFunc connects to the running daemon of the configuration at the given
// path. It returns control.ErrNoDaemon when none is running.
type DialFunc func(ctx context.Context, configPath string) (Remote, error)

type OpenFunc func(context.Context, string, string) (Backend, error)

type ErrorRedactor interface {
	Redact(error) error
}

type FailureReporter interface {
	ReportFailure(context.Context, string, error)
}

type Command struct {
	Open OpenFunc
	// Dial, when set, lets commands the daemon can execute go to it. Without
	// a running daemon the command opens the backend itself.
	Dial DialFunc
	// DaemonTimeout bounds status's wait for the daemon; zero means 2s.
	DaemonTimeout     time.Duration
	Stdout            io.Writer
	Stderr            io.Writer
	DefaultConfigPath string
	Version           string
}

func (c Command) Run(ctx context.Context, args []string) int {
	if c.Stdout == nil {
		c.Stdout = io.Discard
	}
	if c.Stderr == nil {
		c.Stderr = io.Discard
	}
	if c.DefaultConfigPath == "" {
		c.DefaultConfigPath = "/config/config.yaml"
	}
	jsonMode := wantsJSON(args)
	if len(args) == 0 {
		return c.usageError("", jsonMode, "command is required")
	}
	if len(args) == 1 && (args[0] == "--version" || args[0] == "version") {
		version := c.Version
		if version == "" {
			version = "dev"
		}
		_, _ = fmt.Fprintf(c.Stdout, "subsyncd %s\n", version)
		return ExitOK
	}
	command := args[0]
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	if jsonMode {
		flags.SetOutput(io.Discard)
	} else {
		flags.SetOutput(c.Stderr)
	}
	flags.Usage = func() {
		if !jsonMode {
			c.printHelp(flags, command)
		}
	}
	configPath := flags.String("config", c.DefaultConfigPath, "configuration file")
	instance := flags.String("instance", "", "Arr instance name")
	kind := flags.String("kind", "", "media kind (movie or episode)")
	fileID := flags.Int64("file-id", 0, "Arr file ID (with --instance and --kind)")
	title := flags.String("title", "", `media title: "Dune", "Dune (2021)", "Show S01E02", "Show 1x02"`)
	path := flags.String("path", "", "media file path, in this container or as Sonarr or Radarr reports it")
	language := flags.String("language", "", "subtitle language tag")
	limit := flags.Int("limit", 0, "most rows to list (default 50)")
	since := flags.Duration("since", 0, "how far back to look, as a Go duration (default 24h)")
	provider := flags.String("provider", "", "provider instance name")
	forceProbe := flags.Bool("force-probe", false, "refresh embedded subtitle tracks")
	retryRejected := flags.Bool("retry-rejected", false, "clear and retry the candidates subsyncd rejected (not those you rejected)")
	reason := flags.String("reason", "", "why the subtitle is wrong: "+strings.Join(control.RejectReasons, ", "))
	undo := flags.Bool("undo", false, "remove your rejections for the file and language instead; runs no search")
	media := flags.String("media", "", "media file path")
	subtitle := flags.String("subtitle", "", "subtitle file path")
	base := flags.String("base", "", "webhook base URL overriding server.webhook_base_url")
	yes := flags.Bool("yes", false, "delete the records instead of previewing them")
	timeout := flags.Duration("timeout", 0, "how long to wait for the outcome (default 5m); needs a running daemon")
	noWait := flags.Bool("no-wait", false, "return once the daemon has accepted the request; needs a running daemon")
	flags.Bool("json", false, "write one JSON object to stdout instead of text")
	if err := flags.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			if jsonMode {
				c.printHelp(flags, command)
			}
			return ExitUsage
		}
		return c.usageError(command, jsonMode, err.Error())
	}
	if flags.NArg() != 0 {
		return c.usageError(command, jsonMode, "unexpected positional arguments")
	}
	allowed := commandFlags(command)
	var unrelated string
	flags.Visit(func(item *flag.Flag) {
		if !allowed[item.Name] && unrelated == "" {
			unrelated = "--" + item.Name
		}
	})
	if unrelated != "" {
		return c.usageError(command, jsonMode, unrelated+" is not valid for "+command)
	}

	var validateErr error
	switch command {
	case "serve", "doctor", "status", "providers":
	case "scan", "forget-instance", "webhook-url", "rotate-webhook-token":
		validateErr = require("--instance", *instance)
		if validateErr == nil && command == "scan" {
			validateErr = validateSearchWait(flags, *timeout, *noWait)
		}
	case "search", "explain", "find":
		sel := control.Selection{Instance: *instance, Kind: *kind, FileID: *fileID, Title: *title, Path: *path}
		if validateErr = validateMediaSelection(sel); validateErr == nil && command == "search" {
			validateErr = validateSearchWait(flags, *timeout, *noWait)
		}
	case "reject":
		sel := control.Selection{Instance: *instance, Kind: *kind, FileID: *fileID, Title: *title, Path: *path}
		if validateErr = validateMediaSelection(sel); validateErr == nil {
			validateErr = validateReject(flags, *reason, *undo, *timeout, *noWait)
		}
	case "missing":
		if *limit < 0 {
			validateErr = fmt.Errorf("--limit must not be negative")
		}
	case "recent":
		sinceGiven := false
		flags.Visit(func(item *flag.Flag) { sinceGiven = sinceGiven || item.Name == "since" })
		switch {
		case sinceGiven && *since <= 0:
			validateErr = fmt.Errorf("--since must be positive")
		case *limit < 0:
			validateErr = fmt.Errorf("--limit must not be negative")
		}
	case "retry":
		validateErr = require("--provider", *provider)
	case "analyze-sync":
		if validateErr = require("--media", *media); validateErr == nil {
			validateErr = require("--subtitle", *subtitle)
		}
	default:
		return c.usageError(command, jsonMode, "unknown command "+command)
	}
	if validateErr != nil {
		return c.usageError(command, jsonMode, validateErr.Error())
	}
	if c.Open == nil {
		return c.failure(ctx, command, jsonMode, nil, errorsText("backend is unavailable"))
	}

	// The CLI sends a wait only when --timeout was given; the daemon applies
	// its default otherwise.
	searchArgs := control.SearchArgs{
		Selection:     control.Selection{Instance: *instance, Kind: *kind, FileID: *fileID, Title: *title, Path: *path},
		Language:      *language,
		RetryRejected: *retryRejected,
		Wait:          *timeout,
		NoWait:        *noWait,
	}
	rejectArgs := control.RejectArgs{
		Selection: searchArgs.Selection,
		Language:  *language,
		Reason:    *reason,
		Undo:      *undo,
		Wait:      *timeout,
		NoWait:    *noWait,
	}
	scanArgs := control.ScanArgs{Instance: *instance, ForceProbe: *forceProbe, Wait: *timeout, NoWait: *noWait}
	if c.Dial != nil && routed(command) {
		remote, err := c.Dial(ctx, *configPath)
		switch {
		case err == nil:
			defer remote.Close()
			var result control.Result
			switch command {
			case "retry":
				var typed control.RetryResult
				err = remote.Call(ctx, command, control.RetryArgs{Provider: *provider}, &typed)
				result = typed
			case "search":
				var typed control.SearchResult
				err = remote.Call(ctx, command, searchArgs, &typed)
				result = typed
			case "scan":
				var typed control.ScanResult
				err = remote.Call(ctx, command, scanArgs, &typed)
				result = typed
			case "reject":
				var typed control.RejectResult
				err = remote.Call(ctx, command, rejectArgs, &typed)
				result = typed
			}
			if err != nil {
				return c.failure(ctx, command, jsonMode, nil, err)
			}
			return c.succeed(command, jsonMode, result)
		case !errors.Is(err, control.ErrNoDaemon):
			return c.failure(ctx, command, jsonMode, nil, err)
		}
	}

	// A preview only reads, so it opens like the diagnostic commands: no
	// migrations or startup writes, and it can run beside the daemon.
	openCommand := command
	if command == "forget-instance" && !*yes {
		openCommand = "forget-instance-preview"
	}
	backend, err := c.Open(ctx, *configPath, openCommand)
	if err != nil {
		return c.failure(ctx, command, jsonMode, nil, err)
	}
	defer backend.Close()

	var result control.Result
	switch command {
	case "serve":
		err = backend.Serve(ctx)
	case "scan":
		result, err = wrap(backend.Scan(ctx, scanArgs))
	case "search":
		result, err = wrap(backend.Search(ctx, searchArgs))
	case "reject":
		result, err = wrap(backend.Reject(ctx, rejectArgs))
	case "retry":
		result, err = wrap(backend.Retry(ctx, *provider))
	case "explain":
		result, err = wrap(backend.Explain(ctx, control.ExplainArgs{
			Selection: control.Selection{Instance: *instance, Kind: *kind, FileID: *fileID, Title: *title, Path: *path},
			Language:  *language,
		}))
	case "find":
		result, err = wrap(backend.Find(ctx, control.Selection{Instance: *instance, Kind: *kind, FileID: *fileID, Title: *title, Path: *path}))
	case "missing":
		result, err = wrap(backend.Missing(ctx, control.MissingArgs{Instance: *instance, Language: *language, Title: *title, Limit: *limit}))
	case "recent":
		result, err = wrap(backend.Recent(ctx, control.RecentArgs{Since: *since, Instance: *instance, Language: *language, Limit: *limit}))
	case "status":
		var status control.StatusResult
		if status, err = backend.Status(ctx); err == nil {
			c.addDaemonState(ctx, *configPath, &status)
		}
		result = status
	case "providers":
		result, err = wrap(backend.ListProviders(ctx))
	case "doctor":
		result, err = wrap(backend.Doctor(ctx))
	case "analyze-sync":
		result, err = wrap(backend.AnalyzeSync(ctx, *media, *subtitle))
	case "forget-instance":
		result, err = wrap(backend.ForgetInstance(ctx, *instance, *yes))
	case "webhook-url":
		result, err = wrap(backend.WebhookURL(ctx, *instance, *base))
	case "rotate-webhook-token":
		result, err = wrap(backend.RotateWebhookToken(ctx, *instance))
	}
	if err != nil {
		return c.failure(ctx, command, jsonMode, backend, err)
	}
	return c.succeed(command, jsonMode, result)
}

// addDaemonState fills the parts of status only the running daemon knows. A
// missing, unreachable or confused daemon never fails the command: it reads
// as not running, with a short reason that holds no path.
func (c Command) addDaemonState(ctx context.Context, configPath string, status *control.StatusResult) {
	status.Daemon = control.DaemonState{}
	if c.Dial == nil {
		return
	}
	timeout := c.DaemonTimeout
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	remote, err := c.Dial(ctx, configPath)
	if err == nil {
		defer remote.Close()
		var live control.StatusResult
		if err = remote.Call(ctx, "status", nil, &live); err == nil {
			status.Daemon, status.MediaPause = live.Daemon, live.MediaPause
			return
		}
	}
	if errors.Is(err, control.ErrNoDaemon) {
		return
	}
	var coded *control.Error
	if errors.As(err, &coded) && coded.Code != "" && ctx.Err() == nil {
		status.Daemon.Error = coded.Code
		return
	}
	status.Daemon.Error = "daemon unreachable"
}

// routed reports whether the daemon can execute command when it is running.
func routed(command string) bool {
	return control.Routed(command)
}

// validateReject checks reject's --reason, --undo and waiting flags.
func validateReject(flags *flag.FlagSet, reason string, undo bool, timeout time.Duration, noWait bool) error {
	waiting := false
	flags.Visit(func(item *flag.Flag) { waiting = waiting || item.Name == "timeout" || item.Name == "no-wait" })
	switch {
	case undo && reason != "":
		return fmt.Errorf("--reason is not used with --undo")
	case undo && waiting:
		return fmt.Errorf("--timeout and --no-wait are not used with --undo, which runs no search")
	case undo:
		return nil
	case reason == "":
		return fmt.Errorf("--reason is required: one of %s", strings.Join(control.RejectReasons, ", "))
	case !control.ValidRejectReason(reason):
		return fmt.Errorf("unknown --reason %q: use one of %s", reason, strings.Join(control.RejectReasons, ", "))
	}
	return validateSearchWait(flags, timeout, noWait)
}

// validateSearchWait checks search's --timeout and --no-wait.
func validateSearchWait(flags *flag.FlagSet, timeout time.Duration, noWait bool) error {
	given := false
	flags.Visit(func(item *flag.Flag) { given = given || item.Name == "timeout" })
	switch {
	case given && noWait:
		return fmt.Errorf("use only one of --timeout and --no-wait")
	case given && timeout <= 0:
		return fmt.Errorf("--timeout must be positive")
	}
	return nil
}

func (c Command) succeed(command string, jsonMode bool, result control.Result) int {
	if result == nil {
		return ExitOK
	}
	if jsonMode {
		c.writeEnvelope(envelope{Schema: Schema, Command: command, OK: true, Result: result})
		return ExitOK
	}
	if output := result.Text(); output != "" {
		_, _ = fmt.Fprintln(c.Stdout, strings.TrimRight(output, "\n"))
	}
	return ExitOK
}

// wrap turns a backend's typed result into the common Result interface.
func wrap[R control.Result](result R, err error) (control.Result, error) {
	return result, err
}

func commandFlags(command string) map[string]bool {
	allowed := map[string]bool{"config": true}
	if command != "serve" {
		allowed["json"] = true
	}
	var names []string
	switch command {
	case "scan":
		names = []string{"instance", "force-probe", "timeout", "no-wait"}
	case "search":
		names = []string{"instance", "kind", "file-id", "title", "path", "language", "retry-rejected", "timeout", "no-wait"}
	case "reject":
		names = []string{"instance", "kind", "file-id", "title", "path", "language", "reason", "undo", "timeout", "no-wait"}
	case "explain":
		names = []string{"instance", "kind", "file-id", "title", "path", "language"}
	case "find":
		names = []string{"instance", "kind", "file-id", "title", "path"}
	case "missing":
		names = []string{"instance", "language", "title", "limit"}
	case "recent":
		names = []string{"since", "instance", "language", "limit"}
	case "retry":
		names = []string{"provider"}
	case "analyze-sync":
		names = []string{"media", "subtitle"}
	case "forget-instance":
		names = []string{"instance", "yes"}
	case "webhook-url":
		names = []string{"instance", "base"}
	case "rotate-webhook-token":
		names = []string{"instance"}
	}
	for _, name := range names {
		allowed[name] = true
	}
	return allowed
}

// validateMediaSelection checks that exactly one of --file-id, --title and
// --path names the media. --file-id needs --instance and --kind; the other two
// accept them as filters.
func validateMediaSelection(sel control.Selection) error {
	forms := 0
	for _, used := range []bool{sel.FileID != 0, sel.Title != "", sel.Path != ""} {
		if used {
			forms++
		}
	}
	switch {
	case forms == 0:
		return fmt.Errorf("select the media with --file-id (and --instance, --kind), --title or --path")
	case forms > 1:
		return fmt.Errorf("use only one of --file-id, --title and --path")
	}
	if sel.Kind != "" && sel.Kind != "movie" && sel.Kind != "episode" {
		return fmt.Errorf("--kind must be movie or episode")
	}
	if sel.FileID != 0 {
		if err := require("--instance", sel.Instance); err != nil {
			return err
		}
		if sel.Kind == "" {
			return fmt.Errorf("--kind is required with --file-id")
		}
		if sel.FileID < 0 {
			return fmt.Errorf("--file-id must be positive")
		}
	}
	return nil
}

func require(name, value string) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("%s is required", name)
	}
	return nil
}

type envelope struct {
	Schema  string         `json:"schema"`
	Command string         `json:"command"`
	OK      bool           `json:"ok"`
	Result  control.Result `json:"result,omitempty"`
	Error   *control.Error `json:"error,omitempty"`
}

func (c Command) writeEnvelope(value envelope) {
	encoder := json.NewEncoder(c.Stdout)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(value)
}

// wantsJSON reports whether --json appears in args, so even a command line
// that fails to parse is answered with an envelope.
func wantsJSON(args []string) bool {
	for _, arg := range args {
		switch arg {
		case "--json", "-json", "--json=true", "-json=true":
			return true
		}
	}
	return false
}

func (c Command) usageError(command string, jsonMode bool, message string) int {
	if jsonMode {
		c.writeEnvelope(envelope{Schema: Schema, Command: command, Error: &control.Error{Code: control.CodeUsage, Message: message}})
		return ExitUsage
	}
	_, _ = fmt.Fprintln(c.Stderr, "subsyncd:", message)
	return ExitUsage
}

// failure reports an operation error. backend is nil when the failure came
// before one was opened.
func (c Command) failure(ctx context.Context, command string, jsonMode bool, backend Backend, err error) int {
	original := err
	if backend != nil {
		if reporter, ok := backend.(FailureReporter); ok {
			reporter.ReportFailure(ctx, command, err)
			if !jsonMode {
				// The report is a log record; the candidates of an ambiguous
				// selection are what the operator needs to read next.
				var coded *control.Error
				if errors.As(original, &coded) && len(coded.Candidates) > 0 {
					_, _ = fmt.Fprintln(c.Stderr, control.CandidatesText(coded.Candidates))
				}
				return ExitFailure
			}
		}
		if redactor, ok := backend.(ErrorRedactor); ok {
			err = redactor.Redact(err)
		}
	}
	if jsonMode {
		failure := &control.Error{Code: ErrorCode(original), Message: messageOf(err)}
		var coded *control.Error
		if errors.As(original, &coded) {
			failure.Candidates = coded.Candidates
		}
		c.writeEnvelope(envelope{Schema: Schema, Command: command, Error: failure})
		return ExitFailure
	}
	var reported interface{ AlreadyReported() bool }
	if errors.As(err, &reported) && reported.AlreadyReported() {
		return ExitFailure
	}
	_, _ = fmt.Fprintln(c.Stderr, "subsyncd:", messageOf(err))
	var coded *control.Error
	if errors.As(original, &coded) && len(coded.Candidates) > 0 {
		_, _ = fmt.Fprintln(c.Stderr, control.CandidatesText(coded.Candidates))
	}
	return ExitFailure
}

// messageOf is the error text without the code prefix of a *control.Error.
func messageOf(err error) string {
	var coded *control.Error
	if errors.As(err, &coded) && err == error(coded) {
		return coded.Message
	}
	return err.Error()
}

// ErrorCode maps an error to the stable code the JSON envelope carries.
func ErrorCode(err error) string {
	var coded *control.Error
	if errors.As(err, &coded) && coded.Code != "" {
		return coded.Code
	}
	if err != nil && strings.Contains(err.Error(), "another subsyncd mutation process is active") {
		return control.CodeBusy
	}
	return control.CodeInternal
}

func errorsText(message string) error { return fmt.Errorf("%s", message) }

// commandHelp describes what each command returns. With --json the result
// fields below are the "result" object of the envelope; a failure carries
// "error": {"code", "message"} with one of the codes named here.
var commandHelp = map[string]string{
	"serve": "Run the daemon. It has no result and does not accept --json.",
	"scan": `Reconcile one Arr instance with its library.
With the daemon running, the daemon runs the scan between its scheduled reconciliations and the command waits for it, at most --timeout (default 5m). A scan still running then reports outcome running (exit 0) and finishes in the daemon; --no-wait returns at once with running. Without a daemon, scan runs in this process to completion, and --timeout and --no-wait fail with daemon_required.
Result fields: instance, media, force_probe, probe_skipped, outcome (running), history_deferred, deferred, deferred_media.
Error codes: usage, unknown_instance, busy, daemon_required, daemon_stopping, protocol_mismatch, internal.`,
	"search": `Search subtitles for one media file now: for --language, or else for every configured language the file is missing (when none is, nothing runs and each language reports satisfied).
Select the file with --instance, --kind and --file-id, or with --title or --path. A title or path finds only files subsyncd already knows; only --file-id adds a file it has not indexed yet.
With the daemon running, the daemon runs the search beside its worker and the command waits for the outcome, at most --timeout (default 5m). A language still searching then reports outcome running (exit 0) and the search carries on; follow up with explain or find. --no-wait returns at once with running. A dropped connection never cancels the search. Without a daemon, search runs in this process to completion, and --timeout and --no-wait fail with daemon_required.
--retry-rejected reconsiders the candidates subsyncd rejected itself; those you rejected with reject stay rejected (reject --undo removes them).
Result fields: media_id, languages[] (language, outcome, provider, candidate, release, reason, score, sync_verdict, offset_ms, subtitle, next_attempt_at). subtitle is relative to its media root.
Outcomes: installed, satisfied, no_result, rejected, throttled, running, and removed when a replacement queued by reject was still pending for the language and found nothing acceptable (see reject).
Error codes: usage, unknown_instance, unknown_language, media_not_found, ambiguous_media, arr_unreachable, busy, daemon_required, daemon_stopping, protocol_mismatch, internal.`,
	"reject": `Reject a subtitle subsyncd installed that is wrong, so it is never chosen again, and search for a replacement now.
Select the file with --instance, --kind and --file-id, or with --title or --path. --language may be left out when the file has a managed subtitle in only one language. --reason is one of out_of_sync, wrong_language, wrong_release, poor_quality, other.
The rejection is bound to this media file and holds across LAPSE upgrades and search --retry-rejected; replacing the media file drops it. The search that follows installs the best acceptable remaining candidate even when it scores lower. When none is acceptable, the rejected subtitle is removed (outcome removed) and the language counts as missing again. A throttled or failed search keeps the subtitle; the queued search retries the replacement.
Only a subtitle subsyncd installed and that is unchanged on disk can be rejected: embedded tracks, your own sidecars and sidecars you edited fail with not_managed.
--undo removes your rejections for the file and language, for a reject made by mistake, and runs no search.
With the daemon running, it waits like search: at most --timeout (default 5m), then reports running and carries on; --no-wait returns at once. Without a daemon, it runs in this process, and --timeout and --no-wait fail with daemon_required.
Result fields: media_id, rejected (language, outcome, provider, candidate, release, reason, score, sync_verdict, offset_ms, subtitle, next_attempt_at), undone, undo. For removed, reason is the search outcome (no_result or rejected) that found nothing acceptable.
Outcomes: installed, satisfied, removed, no_result, rejected, throttled, running.
Error codes: usage, unknown_instance, unknown_language, media_not_found, ambiguous_media, not_managed, busy, daemon_required, daemon_stopping, protocol_mismatch, internal.`,
	"retry": `Clear a provider's throttle and failure state and make throttled searches due.
With the daemon running, the daemon clears the state itself.
Result fields: provider, searches_due.
Error codes: usage, unknown_provider, busy, daemon_stopping, protocol_mismatch, internal.`,
	"explain": `Show why a media file and language are in their current state.
Select the file with --instance, --kind and --file-id, or with --title or --path. Without --language every configured language is explained.
Result fields: media_id, instance, kind, file_id, title, probe, tracks, search, candidates, rejections, installation, pack_cache_reusable, provider_states. Without --language the same fields identify the media and its tracks, and languages[] holds one complete result per configured language.
Error codes: usage, unknown_instance, unknown_language, media_not_found, ambiguous_media, internal.`,
	"find": `List the indexed media a selection matches and the state of each configured language: installed, satisfied (a track already covers it), running or missing. Several matches are a success.
Select with --title ("Dune", "Dune (2021)", "Show S01E02", "Show 1x02"; case, accents and punctuation do not matter; an exact title beats a partial one), --path (a path in this container or as Sonarr or Radarr reports it), or --instance, --kind and --file-id. --instance and --kind narrow --title and --path.
Result fields: matches[] (instance, kind, file_id, title, year, season, episode, episode_end, languages[] (language, state)).
Error codes: usage, unknown_instance, media_not_found (only for --file-id), internal.`,
	"missing": `List what subsyncd has no subtitle for yet: each active media file and configured language with no installation and no track that covers it (a forced-only track does not count; a hearing-impaired one only with allow_hearing_impaired). Runs beside the daemon.
Filters: --instance, --language, --title ("Dune", "Show S01E02"; an exact title beats a partial one), --limit (default 50). Rows follow the queue: what would run next comes first, then languages with no search yet. A search running now is listed too, with no next_attempt_at.
Reasons: not_searched (no search has run yet), no_candidates (providers returned nothing usable), all_rejected (every candidate was rejected), providers_cooling_down (the last search was throttled, or every provider of the language's route is cooling down; providers names them), search_failing (the last search failed technically, for example a network, LAPSE or media-storage problem; it retries by itself at next_attempt_at, and explain or the logs show the cause), unsupported (for example a multi-episode file subsyncd does not search).
action is a complete command to run (search ..., search ... --retry-rejected); for providers_cooling_down it is providers, which shows when the cooldown or quota resets (searches resume by themselves), or retry --provider ... when a provider rejected its credentials; for search_failing it is the explain command to see the cause, and for unsupported a short instruction instead.
Result fields: total (all rows before --limit), items[] (instance, kind, file_id, title, year, season, episode, episode_end, language, reason, providers, next_attempt_at, action).
Error codes: usage, unknown_instance, unknown_language, internal.`,
	"recent": `List what happened in a time window, newest first: subtitles installed (and replaced), searches that ended with no result or only rejected candidates, subtitles you rejected, subtitles removed, and searches that gave up on upgrade checks. Reads the search history beside the daemon; the history is kept for 30 days.
Filters: --since (a Go duration such as 90m or 72h; default 24h), --instance, --language, --limit (default 50).
Outcomes: installed (a subtitle was installed or replaced), satisfied (nothing needed doing), no_result (providers returned nothing usable), rejected (every candidate was rejected; a reason starting with user_ is a rejection you made with reject, such as user_out_of_sync; its replacement's result is a separate row; reject --undo does not remove rows), throttled (providers were rate limited, or the subtitle of a replaced media file waits for its hourly deletion slot), removed (a rejected subtitle was taken out and nothing acceptable replaced it). manual is true for searches you started.
Result fields: since, items[] (instance, kind, file_id, title, language, outcome, provider, release, reason, manual, finished_at). The title is the name the log shows (episodes include season and episode); a file subsyncd has since forgotten still appears.
Error codes: usage, unknown_instance, unknown_language, internal.`,
	"status": `Overview to read before acting: the daemon, the queue, running searches, providers that cannot be used, the media pause, each Arr instance and the backlogs. Runs beside the daemon, and still answers without one.
Result fields: daemon (running, version, started_at, error), queue (new, missing, upgrade; each total and due), running[] (the media fields, language, lease_until), providers[] (id, state, until, last_failure), media_pause (until), instances[] (name, last_reconcile_at, reconcile_failures, discovery_complete, webhook, webhook_checked_at), backlog (webhook_inbox, notification_outbox).
daemon.running is false when no daemon answers, with daemon.error set (a code, or "daemon unreachable") when something did but failed. version, started_at and media_pause come from the daemon only. A pause is reported only while it lasts.
queue counts pending searches: new (just imported or renamed, or queued by reject), missing, and upgrade checks. due counts those scheduled now and not running. running[] are searches holding a live lease (the worker's, or a manual search); the database records when the lease ends (lease_until), not when the search began.
providers[] lists only providers that cannot be used now; providers shows them all. State is cooling_down (a rate limit, busy service, blocked client or transient failure until the reset time in until), quota_exhausted (a quota is used up until until), or auth_failed (credentials were rejected; until is empty and the provider stays off until you fix them and run retry --provider ID).
instances[]: last_reconcile_at is the last attempt and succeeded when reconcile_failures is 0; discovery_complete is whether the first library discovery finished; webhook is connected (subsyncd manages the Arr connection and checked it at webhook_checked_at), pending (managed, not yet connected) or unmanaged (no server.webhook_base_url).
Error codes: internal.`,
	"providers": `Show every configured provider: its routes, its state, and what it did in the last 24 hours. Runs beside the daemon.
Result fields: providers[] (id, type, routes[] (language, tier), state, until, last_failure, last_success_at, last_24h (installed, no_result, rejected)).
routes[] lists the languages the provider serves; tier is preferred (tried first) or fallback (tried when the preferred providers find nothing).
state is available, cooling_down (until is when it can be used again; last_failure is the reason, such as rate_limit or service_busy), quota_exhausted (until is the quota reset) or auth_failed (credentials rejected; until is empty). retry --provider ID clears a provider's state, which helps for a cooldown or after fixing credentials, not while the quota is truly exhausted.
last_success_at is the last installed subtitle from the provider that the search history (kept 30 days) still holds; last_24h counts its search outcomes over the last 24 hours.
Error codes: internal.`,
	"doctor": `Check configuration, storage, LAPSE and ffprobe without contacting Arr or providers.
Result fields: configuration, sqlite, media_roots, lapse, ffprobe, webhooks.
Error codes: internal.`,
	"analyze-sync": `Run LAPSE on a media file and a subtitle file and report its verdict.
Result fields: analysis (LAPSE's document).
Error codes: usage, internal.`,
	"forget-instance": `Preview, or with --yes delete, the records of an instance removed from the configuration.
Result fields: instance, applied, media, searches, installations, rollback_copies, candidates, rejections, notifications, events, replays, history.
Error codes: unknown_instance, busy, internal.`,
	"webhook-url": `Print the webhook URL to enter in Arr. The result includes the token.
Result fields: instance, url.
Error codes: unknown_instance, internal.`,
	"rotate-webhook-token": `Replace an instance's webhook token. Stop the daemon first.
Result fields: instance, managed.
Error codes: unknown_instance, busy, internal.`,
}

// printHelp writes the usage of command: only its own flags, then what it
// returns.
func (c Command) printHelp(flags *flag.FlagSet, command string) {
	out := c.Stderr
	_, _ = fmt.Fprintf(out, "Usage: subsyncd %s [flags]\n\n", command)
	if text, ok := commandHelp[command]; ok {
		_, _ = fmt.Fprintln(out, text)
		_, _ = fmt.Fprintln(out)
	}
	allowed := commandFlags(command)
	own := flag.NewFlagSet(command, flag.ContinueOnError)
	own.SetOutput(out)
	flags.VisitAll(func(item *flag.Flag) {
		if allowed[item.Name] {
			own.Var(item.Value, item.Name, item.Usage)
		}
	})
	own.PrintDefaults()
	if allowed["json"] {
		_, _ = fmt.Fprintln(out, "\nWith --json, stdout is one JSON object: {\"schema\":\"subsyncd.cli/v1\",\"command\",\"ok\",\"result\"} or, on failure, {...,\"ok\":false,\"error\":{\"code\",\"message\"}}. Exit codes: 0 success, 1 failure, 2 usage.")
	}
}
