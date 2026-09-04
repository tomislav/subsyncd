package main

import (
	"context"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"subsyncd/internal/app"
	"subsyncd/internal/cli"
	"subsyncd/internal/version"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Stdout, os.Stderr))
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	logger := slog.New(slog.NewJSONHandler(stderr, nil))
	defaultConfig := os.Getenv("SUBSYNCD_CONFIG")
	return (cli.Command{
		Stdout: stdout, Stderr: stderr, DefaultConfigPath: defaultConfig, Version: version.Value,
		Open: func(ctx context.Context, path string) (cli.Backend, error) {
			return app.Open(ctx, path, logger)
		},
	}).Run(ctx, args)
}
