// Command t3fleet manages prepared T3 cloud machines through a local daemon.
package main

import (
	"context"
	"io"
	"os"
	"os/signal"
	"syscall"
)

var version = "dev"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	if err != nil {
		printError(os.Stderr, err, colorEnabled(os.Stderr))
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	return newApp(stdout, stderr).execute(ctx, args)
}
