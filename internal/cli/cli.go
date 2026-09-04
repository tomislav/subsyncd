package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strings"
)

const (
	ExitOK      = 0
	ExitFailure = 1
	ExitUsage   = 2
)

type Backend interface {
	Close() error
	Serve(context.Context) error
	Scan(context.Context, string, bool) (string, error)
	Search(context.Context, string, string, int64, string) (string, error)
	Retry(context.Context, string) (string, error)
	Explain(context.Context, string, string, int64, string) (string, error)
	Doctor(context.Context) (string, error)
	AnalyzeSync(context.Context, string, string) (string, error)
}

type OpenFunc func(context.Context, string) (Backend, error)

type ErrorRedactor interface {
	Redact(error) error
}

type Command struct {
	Open              OpenFunc
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
	if len(args) == 0 {
		return c.usageError("command is required")
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
	flags.SetOutput(c.Stderr)
	configPath := flags.String("config", c.DefaultConfigPath, "configuration file")
	instance := flags.String("instance", "", "Arr instance name")
	kind := flags.String("kind", "", "media kind (movie or episode)")
	fileID := flags.Int64("file-id", 0, "Arr file ID")
	language := flags.String("language", "", "subtitle language tag")
	provider := flags.String("provider", "", "provider instance name")
	forceProbe := flags.Bool("force-probe", false, "refresh embedded subtitle tracks")
	media := flags.String("media", "", "media file path")
	subtitle := flags.String("subtitle", "", "subtitle file path")
	if err := flags.Parse(args[1:]); err != nil {
		return ExitUsage
	}
	if flags.NArg() != 0 {
		return c.usageError("unexpected positional arguments")
	}
	allowed := commandFlags(command)
	var unrelated string
	flags.Visit(func(item *flag.Flag) {
		if !allowed[item.Name] && unrelated == "" {
			unrelated = "--" + item.Name
		}
	})
	if unrelated != "" {
		return c.usageError(unrelated + " is not valid for " + command)
	}

	var validateErr error
	switch command {
	case "serve", "doctor":
	case "scan":
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
		return c.usageError("unknown command " + command)
	}
	if validateErr != nil {
		return c.usageError(validateErr.Error())
	}
	if c.Open == nil {
		return c.failure(errorsText("backend is unavailable"))
	}

	backend, err := c.Open(ctx, *configPath)
	if err != nil {
		return c.failure(err)
	}
	defer backend.Close()

	var output string
	switch command {
	case "serve":
		err = backend.Serve(ctx)
	case "scan":
		output, err = backend.Scan(ctx, *instance, *forceProbe)
	case "search":
		output, err = backend.Search(ctx, *instance, *kind, *fileID, *language)
	case "retry":
		output, err = backend.Retry(ctx, *provider)
	case "explain":
		output, err = backend.Explain(ctx, *instance, *kind, *fileID, *language)
	case "doctor":
		output, err = backend.Doctor(ctx)
	case "analyze-sync":
		output, err = backend.AnalyzeSync(ctx, *media, *subtitle)
	}
	if err != nil {
		if redactor, ok := backend.(ErrorRedactor); ok {
			err = redactor.Redact(err)
		}
		return c.failure(err)
	}
	if output != "" {
		_, _ = fmt.Fprintln(c.Stdout, strings.TrimRight(output, "\n"))
	}
	return ExitOK
}

func commandFlags(command string) map[string]bool {
	allowed := map[string]bool{"config": true}
	var names []string
	switch command {
	case "scan":
		names = []string{"instance", "force-probe"}
	case "search", "explain":
		names = []string{"instance", "kind", "file-id", "language"}
	case "retry":
		names = []string{"provider"}
	case "analyze-sync":
		names = []string{"media", "subtitle"}
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

func (c Command) usageError(message string) int {
	_, _ = fmt.Fprintln(c.Stderr, "subsyncd:", message)
	return ExitUsage
}

func (c Command) failure(err error) int {
	_, _ = fmt.Fprintln(c.Stderr, "subsyncd:", err)
	return ExitFailure
}

func errorsText(message string) error { return fmt.Errorf("%s", message) }
