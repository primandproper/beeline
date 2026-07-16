// Command main is the application's entrypoint.
//
// It sets up a context that is cancelled on SIGINT/SIGTERM and hands off to the
// CLI, which bootstraps observability and dispatches to the requested
// subcommand. cobra prints any command error to stderr; a non-nil result exits
// with a non-zero status.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/primandproper/template-go/internal/cli"
)

func main() {
	if err := run(); err != nil {
		os.Exit(1)
	}
}

// run owns the signal-cancellable context so its deferred stop() runs before
// main calls os.Exit.
func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	return cli.Execute(ctx)
}
