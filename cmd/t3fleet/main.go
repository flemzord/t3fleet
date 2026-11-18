// Command t3fleet manages prepared T3 cloud machines through a local daemon.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
)

var version = "dev"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "t3fleet:", err)
		os.Exit(1)
	}
}

func usage(w io.Writer) {
	fmt.Fprintln(w, `Usage: t3fleet COMMAND [arguments] [--config PATH]

Commands:
  serve                       Run the local fleet daemon
  create --name N --provider P Create from a template and open T3 association
  list                        List managed machines
  inspect ID                  Inspect one managed machine
  start ID                    Start and wait for T3 readiness
  stop ID                     Stop one machine
  delete [ID]                 Delete one machine, or interactively select several
  snapshot ID --name NAME     Capture a provider-supported snapshot
  pair ID                     Print the T3 Code pairing deep link
  version                     Print the CLI version

Bulk create: --count 10 --concurrency 3; names become NAME-01 through NAME-10.
Resume a batch: create --resume BATCH_ID [--no-register].
Scripted multi-delete: delete --ids ID,ID --yes (managed IDs only).
Create overrides: --image REF, --snapshot REF, --source GOLDEN_DEVBOX, --size SIZE.
Use create --no-register for headless scripts or to pair manually later.
T3 Code requires confirmation before saving an association.
Cloud credentials are loaded only by serve. See examples/config.json and docs.`)
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		usage(stdout)
		return nil
	}
	path, rest, err := extractConfig(args)
	if err != nil {
		return err
	}
	if len(rest) == 0 {
		return errors.New("a command is required")
	}
	command, args := rest[0], rest[1:]
	if command == "help" || command == "--help" || command == "-h" {
		usage(stdout)
		return nil
	}
	if command == "version" {
		fmt.Fprintln(stdout, "t3fleet", version)
		return nil
	}
	switch command {
	case "serve", "create", "list", "inspect", "start", "stop", "delete", "snapshot", "pair":
	default:
		return errors.New("unknown command; run t3fleet help")
	}
	cfg, err := loadConfig(path)
	if err != nil {
		return err
	}
	if command == "serve" {
		if len(args) != 0 {
			return errors.New("serve takes no positional arguments")
		}
		return serve(ctx, cfg, stderr)
	}
	return runClient(ctx, cfg, command, args, stdout, stderr)
}
