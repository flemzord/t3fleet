package main

import (
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"golang.org/x/term"

	"github.com/flemzord/t3fleet/pkg/provider"
)

type progressMessages struct {
	pending string
	success string
	failure string
}

func createProgress(instance string, template provider.Template) progressMessages {
	base := "snapshot"
	if template.Image != "" {
		base = "image"
	}
	return progressMessages{"Creating machine on " + instance + " from " + base + "; waiting for T3 readiness", "Machine ready", "Creation did not complete"}
}

// machineProgress names the machine an operation acts on, so a long wait
// always shows which cloud resource it concerns.
func machineProgress(command, name string) progressMessages {
	switch command {
	case "start":
		return progressMessages{"Starting " + name + "; waiting for T3 readiness", name + " is ready", "Start of " + name + " did not complete"}
	case "stop":
		return progressMessages{"Stopping " + name, name + " stopped", "Stop of " + name + " did not complete"}
	case "delete":
		return progressMessages{"Deleting " + name, name + " deleted", "Deletion of " + name + " did not complete"}
	case "snapshot":
		return progressMessages{"Saving snapshot of " + name + "; waiting for provider completion", "Snapshot ready", "Snapshot did not complete"}
	case "pair":
		return progressMessages{"Preparing T3 association for " + name + "; waiting for readiness", "Association link ready", "Association could not be prepared"}
	default:
		return progressMessages{}
	}
}

func interactiveProgress(w io.Writer) bool {
	file, ok := w.(interface{ Fd() uintptr })
	return ok && os.Getenv("TERM") != "dumb" && term.IsTerminal(int(file.Fd()))
}

// startProgress writes only to diagnostics. It reports activity and elapsed
// time, not an invented percentage or unobserved cloud lifecycle stages.
// The returned function joins the renderer before any subsequent CLI output.
func startProgress(w io.Writer, messages progressMessages, interactive bool) func(error) {
	if messages.pending == "" {
		return func(error) {}
	}
	started := time.Now()
	elapsed := func() time.Duration { return time.Since(started).Round(time.Second) }
	done, stopped := make(chan struct{}), make(chan struct{})
	frames := [...]string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
	draw := func(frame int) {
		fmt.Fprintf(w, "\r\x1b[2K%s %s… (%s)", frames[frame%len(frames)], messages.pending, elapsed())
	}
	if interactive {
		draw(0)
		go func() {
			defer close(stopped)
			ticker := time.NewTicker(100 * time.Millisecond)
			defer ticker.Stop()
			for frame := 1; ; frame++ {
				select {
				case <-done:
					return
				case <-ticker.C:
					draw(frame)
				}
			}
		}()
	} else {
		fmt.Fprintln(w, messages.pending+"…")
		close(stopped)
	}
	var once sync.Once
	return func(err error) {
		once.Do(func() {
			close(done)
			<-stopped
			if interactive {
				fmt.Fprint(w, "\r\x1b[2K")
			}
			message := messages.success
			if err != nil {
				message = messages.failure
			}
			// Error details are emitted separately by the CLI's existing safe
			// error handling. Never render arbitrary provider/transport errors.
			fmt.Fprintf(w, "%s (%s)\n", message, elapsed())
		})
	}
}
