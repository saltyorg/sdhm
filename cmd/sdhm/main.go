package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/saltyorg/sdhm/command"
)

var version = "0.0.0-dev"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	root := command.NewRoot(version, runDaemon, regenerateHosts)
	if err := root.ExecuteContext(ctx); err != nil {
		writeProcessError(os.Stderr, journalStreamPresent(), err)
		os.Exit(1)
	}
}
