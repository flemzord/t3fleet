package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"text/tabwriter"
	"time"

	"golang.org/x/term"

	"github.com/flemzord/t3fleet/internal/fleet"
)

// colorEnabled follows the NO_COLOR convention and never colors redirected output.
func colorEnabled(w io.Writer) bool {
	if os.Getenv("NO_COLOR") != "" || os.Getenv("TERM") == "dumb" {
		return false
	}
	file, ok := w.(interface{ Fd() uintptr })
	return ok && term.IsTerminal(int(file.Fd()))
}

func styleState(state string, color bool) string {
	if !color {
		return state
	}
	code := ""
	switch state {
	case "ready":
		code = "32"
	case "stopped", "deleted":
		code = "2"
	case "creating", "starting", "stopping", "planned":
		code = "36"
	default:
		code = "33"
	}
	return "\x1b[" + code + "m" + state + "\x1b[0m"
}

func printRawJSON(w io.Writer, data []byte) error {
	var pretty bytes.Buffer
	if json.Indent(&pretty, data, "", "  ") != nil {
		return errors.New("invalid daemon response")
	}
	_, err := fmt.Fprintln(w, pretty.String())
	return err
}

func printJSON(w io.Writer, value any) error {
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}

func printMachineTable(w io.Writer, machines []fleet.ObservedMachine, color bool) {
	// tabwriter counts escape bytes as width, so pad states before coloring.
	width := len("FLEET STATE")
	for _, m := range machines {
		width = max(width, len(m.State))
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "NAME\tPROVIDER\t%-*s\tPROVIDER STATE\tOBSERVED AT\tID\tLAST STOP\n", width, "FLEET STATE")
	for _, m := range machines {
		state := fmt.Sprintf("%-*s", width, m.State)
		if color {
			state = styleState(m.State, true) + state[len(m.State):]
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", orDash(m.Name), orDash(m.Provider), state, orDash(m.ProviderState), observationTime(m.ProviderObservedAt), shortID(m.ID), lastStop(m.Machine, time.Now()))
	}
	tw.Flush()
}

func observationTime(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.UTC().Format(time.RFC3339)
}

func printMachineDetails(w io.Writer, m fleet.Machine, color bool) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	row := func(key, value string) {
		if value != "" {
			fmt.Fprintf(tw, "%s:\t%s\n", key, value)
		}
	}
	row("Name", m.Name)
	row("ID", m.ID)
	row("Provider", m.Provider)
	fmt.Fprintf(tw, "State:\t%s\n", styleState(m.State, color))
	row("Remote ID", m.RemoteID)
	row("Local URL", m.URL)
	if !m.LastStoppedAt.IsZero() {
		row("Last stop", lastStop(m, time.Now())+" ("+m.LastStoppedAt.Local().Format(time.DateTime)+")")
	}
	tw.Flush()
}

func lastStop(m fleet.Machine, now time.Time) string {
	if m.LastStoppedAt.IsZero() {
		return "-"
	}
	text := "just now"
	if d := now.Sub(m.LastStoppedAt); d >= time.Minute {
		text = humanDuration(d) + " ago"
	}
	if m.LastStopReason != "" {
		text = m.LastStopReason + ", " + text
	}
	return text
}

func humanDuration(d time.Duration) string {
	switch {
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

func orDash(value string) string {
	if value == "" {
		return "-"
	}
	return value
}
