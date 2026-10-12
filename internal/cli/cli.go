package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

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
	Scan(context.Context, string, bool) (control.ScanResult, error)
	Search(context.Context, control.SearchArgs) (control.SearchResult, error)
	Retry(context.Context, string) (control.RetryResult, error)
	Explain(context.Context, control.ExplainArgs) (control.ExplainResult, error)
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
	Dial              DialFunc
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
	fileID := flags.Int64("file-id", 0, "Arr file ID")
	language := flags.String("language", "", "subtitle language tag")
	provider := flags.String("provider", "", "provider instance name")
	forceProbe := flags.Bool("force-probe", false, "refresh embedded subtitle tracks")
	retryRejected := flags.Bool("retry-rejected", false, "clear and retry rejected subtitle candidates")
	media := flags.String("media", "", "media file path")
	subtitle := flags.String("subtitle", "", "subtitle file path")
	base := flags.String("base", "", "webhook base URL overriding server.webhook_base_url")
	yes := flags.Bool("yes", false, "delete the records instead of previewing them")
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
	case "serve", "doctor":
	case "scan", "forget-instance", "webhook-url", "rotate-webhook-token":
		validateErr = require("--instance", *instance)
	case "search", "explain":
		validateErr = validateMediaSelection(*instance, *kind, *fileID, *language)
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
		result, err = wrap(backend.Scan(ctx, *instance, *forceProbe))
	case "search":
		result, err = wrap(backend.Search(ctx, control.SearchArgs{
			Selection:     control.Selection{Instance: *instance, Kind: *kind, FileID: *fileID},
			Language:      *language,
			RetryRejected: *retryRejected,
		}))
	case "retry":
		result, err = wrap(backend.Retry(ctx, *provider))
	case "explain":
		result, err = wrap(backend.Explain(ctx, control.ExplainArgs{
			Selection: control.Selection{Instance: *instance, Kind: *kind, FileID: *fileID},
			Language:  *language,
		}))
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

// routed reports whether the daemon can execute command when it is running.
func routed(command string) bool {
	return command == "retry"
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
		names = []string{"instance", "force-probe"}
	case "search":
		names = []string{"instance", "kind", "file-id", "language", "retry-rejected"}
	case "explain":
		names = []string{"instance", "kind", "file-id", "language"}
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

func validateMediaSelection(instance, kind string, fileID int64, language string) error {
	if err := require("--instance", instance); err != nil {
		return err
	}
	if kind != "movie" && kind != "episode" {
		return fmt.Errorf("--kind must be movie or episode")
	}
	if fileID <= 0 {
		return fmt.Errorf("--file-id must be positive")
	}
	return require("--language", language)
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
Result fields: instance, media, force_probe, history_deferred, deferred, deferred_media.
Error codes: unknown_instance, busy, internal.`,
	"search": `Search subtitles for one media file and language now.
Result fields: media_id, languages[] (language, outcome, provider, candidate, release, reason, score, sync_verdict, offset_ms, subtitle, next_attempt_at).
Outcomes: installed, satisfied, no_result, rejected, throttled.
Error codes: usage, unknown_instance, unknown_language, busy, internal.`,
	"retry": `Clear a provider's throttle and failure state and make throttled searches due.
Result fields: provider, searches_due.
Error codes: unknown_provider, busy, internal.`,
	"explain": `Show why a media file and language are in their current state.
Result fields: media_id, instance, kind, file_id, title, probe, tracks, search, candidates, rejections, installation, pack_cache_reusable, provider_states.
Error codes: usage, unknown_instance, unknown_language, internal.`,
	"doctor": `Check configuration, storage, LAPSE and ffprobe without contacting Arr or providers.
Result fields: configuration, sqlite, media_roots, lapse, ffprobe, webhooks.
Error codes: internal.`,
	"analyze-sync": `Run LAPSE on a media file and a subtitle file and report its verdict.
Result fields: analysis (LAPSE's document).
Error codes: usage, internal.`,
	"forget-instance": `Preview, or with --yes delete, the records of an instance removed from the configuration.
Result fields: instance, applied, media, searches, installations, rollback_copies, candidates, rejections, notifications, events, replays.
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
