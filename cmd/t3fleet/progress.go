package main

import (
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"golang.org/x/term"

	"github.com/flemzord/t3fleet/internal/config"
	"github.com/flemzord/t3fleet/internal/fleet"
	"github.com/flemzord/t3fleet/pkg/provider"
)

type progressMessages struct {
	pending string
	success string
	failure string
}

func operationProgress(cfg config.Config, command string, body any) progressMessages {
	switch command {
	case "create":
		req := body.(fleet.CreateRequest)
		cloud := map[string]string{"boat": "Boat", "namespace": "Namespace"}[req.Provider]
		if cloud == "" {
			cloud = "cloud"
		}
		template := req.Template
		if template == (provider.Template{}) {
			template = cfg.Templates[req.Provider]
		}
		base := "prepared template"
		if template.Snapshot != "" || template.Source != "" {
			base = "snapshot"
		} else if template.Image != "" {
			base = "image"
		}
		return progressMessages{"Creating " + cloud + " machine from " + base + "; waiting for T3 readiness", "Machine ready", "Creation did not complete"}
	case "start":
		return progressMessages{"Starting machine; waiting for T3 readiness", "Machine ready", "Start did not complete"}
	case "stop":
		return progressMessages{"Stopping machine", "Machine stopped", "Stop did not complete"}
	case "delete":
		return progressMessages{"Deleting machine", "Machine deleted", "Deletion did not complete"}
	case "snapshot":
		return progressMessages{"Saving snapshot; waiting for provider completion", "Snapshot ready", "Snapshot did not complete"}
	case "pair":
		return progressMessages{"Preparing T3 association; waiting for readiness", "Association link ready", "Association could not be prepared"}
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
