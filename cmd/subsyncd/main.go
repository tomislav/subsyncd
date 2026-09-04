package main

import (
	"context"
	"io"
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
	defaultConfig := os.Getenv("SUBSYNCD_CONFIG")
	return (cli.Command{
		Stdout: stdout, Stderr: stderr, DefaultConfigPath: defaultConfig, Version: version.Value,
		Open: func(ctx context.Context, path, command string) (cli.Backend, error) {
			return app.Open(ctx, path, app.OpenOptions{LogWriter: stderr, Version: version.Value, Command: command})
		},
	}).Run(ctx, args)
}
