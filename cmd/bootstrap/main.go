package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/ventus-ag/magnum-bootstrap/internal/app"
)

func main() {
	// Cancel the run context on SIGTERM/SIGINT so a systemd stop (or the
	// TimeoutStartSec backstop) unwinds the reconcile gracefully: in-flight
	// barriers, health waits, and Pulumi operations observe ctx.Done(), the
	// flock is released on exit, and a proper failure result/journal entry is
	// written — instead of the process being killed mid-operation. A second
	// signal (NotifyContext stops catching after the first) falls through to
	// the default terminate action as a hard escape hatch.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	os.Exit(app.Main(ctx, os.Args[1:], os.Stdout, os.Stderr))
}
