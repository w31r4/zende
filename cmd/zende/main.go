package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"zende.local/harness/internal/cli"
	"zende.local/harness/internal/process"
)

var version = "0.1.0-dev"
var commit = "unreleased"
var buildDate = "unknown"

func main() {
	if handled, code := process.RunInternalPTY(os.Args[1:]); handled {
		os.Exit(code)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	code := cli.Run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr, cli.BuildInfo{Version: version, Commit: commit, Date: buildDate})
	os.Exit(code)
}
