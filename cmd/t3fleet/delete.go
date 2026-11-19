package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"

	"golang.org/x/term"

	"github.com/flemzord/t3fleet/internal/fleet"
)

func interactiveInput() bool { return term.IsTerminal(int(os.Stdin.Fd())) }

type deleteOptions struct {
	ids        string
	yes        bool
	jsonOutput bool
}

type deleteResult struct {
	ID      string `json:"id"`
	Deleted bool   `json:"deleted"`
}

// deleteOne removes a single machine designated by its full ID or exact name.
// Prefixes are refused: a short typo must never select another machine.
func (a *app) deleteOne(ctx context.Context, ref string, opts deleteOptions) error {
	if opts.ids != "" {
		return hinted("use 't3fleet admin delete REF' or 't3fleet admin delete --ids ID,ID --yes'", "a machine argument cannot be combined with --ids")
	}
	d, err := a.daemon()
	if err != nil {
		return err
	}
	machine, err := d.resolve(ctx, ref, false)
	if err != nil {
		return err
	}
	finish := startProgress(a.stderr, machineProgress("delete", displayName(machine)), interactiveProgress(a.stderr))
	_, err = d.call(ctx, http.MethodDelete, "/v1/machines/"+url.PathEscape(machine.ID), nil)
	finish(err)
	if err != nil {
		return err
	}
	if opts.jsonOutput {
		return printJSON(a.stdout, deleteResult{ID: machine.ID, Deleted: true})
	}
	return nil
}

func (a *app) deleteMany(ctx context.Context, opts deleteOptions) error {
	if opts.ids == "" && opts.yes {
		return hinted("use 't3fleet admin delete REF', 't3fleet admin delete' interactively, or 't3fleet admin delete --ids ID,ID --yes'", "--yes requires --ids")
	}
	if !a.stdinTTY && (opts.ids == "" || !opts.yes) {
		return hinted("without a terminal, use 't3fleet admin delete REF' or 't3fleet admin delete --ids ID,ID --yes'", "interactive deletion needs a terminal")
	}
	d, err := a.daemon()
	if err != nil {
		return err
	}
	machines, err := d.machines(ctx)
	if err != nil {
		return err
	}
	var ids []string
	scanner := bufio.NewScanner(a.stdin)
	if opts.ids != "" {
		ids = strings.Split(opts.ids, ",")
	} else {
		if len(machines) == 0 {
			fmt.Fprintln(a.stderr, "No managed machines.")
			return nil
		}
		tw := tabwriter.NewWriter(a.stderr, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "#\tNAME\tPROVIDER\tSTATE\tID")
		for i, m := range machines {
			fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\n", i+1, orDash(m.Name), orDash(m.Provider), orDash(m.State), m.ID)
		}
		tw.Flush()
		fmt.Fprintln(a.stderr, "Select numbers separated by spaces (empty or q cancels):")
		line, err := readSelection(ctx, scanner)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if strings.TrimSpace(line) == "q" {
			return nil
		}
		for _, field := range strings.Fields(line) {
			n, err := strconv.Atoi(field)
			if err != nil || n < 1 || n > len(machines) {
				return errors.New("invalid selection; nothing deleted")
			}
			ids = append(ids, machines[n-1].ID)
		}
	}
	selected, err := selectedMachines(machines, ids)
	if err != nil {
		return err
	}
	if len(selected) == 0 {
		fmt.Fprintln(a.stderr, "Empty selection; nothing deleted.")
		return nil
	}
	fmt.Fprintln(a.stderr, "Selected for permanent deletion:")
	tw := tabwriter.NewWriter(a.stderr, 0, 0, 2, ' ', 0)
	for _, m := range selected {
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\n", orDash(m.Name), orDash(m.Provider), orDash(m.State), m.ID)
	}
	tw.Flush()
	if !opts.yes {
		fmt.Fprintln(a.stderr, "Type delete to confirm this exact selection (anything else cancels):")
		line, err := readSelection(ctx, scanner)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if line != "delete" {
			fmt.Fprintln(a.stderr, "Cancelled; nothing deleted.")
			return nil
		}
	}
	results := make([]deleteResult, 0, len(selected))
	failed := 0
	for _, m := range selected {
		if err := ctx.Err(); err != nil {
			failed++
			results = append(results, deleteResult{ID: m.ID})
			continue
		}
		finish := startProgress(a.stderr, machineProgress("delete", displayName(m)+" ("+m.ID+")"), interactiveProgress(a.stderr))
		_, err := d.call(ctx, http.MethodDelete, "/v1/machines/"+url.PathEscape(m.ID), nil)
		finish(err)
		results = append(results, deleteResult{m.ID, err == nil})
		if err != nil {
			failed++
		}
	}
	if opts.jsonOutput {
		if err := printJSON(a.stdout, results); err != nil {
			return err
		}
	} else {
		fmt.Fprintf(a.stderr, "Deleted %d of %d selected machines.\n", len(results)-failed, len(results))
	}
	if failed > 0 {
		return hinted("run 't3fleet list' and inspect the remaining machines before retrying", "%d selected machines were not deleted", failed)
	}
	return nil
}

func selectedMachines(machines []fleet.Machine, ids []string) ([]fleet.Machine, error) {
	byID := make(map[string]fleet.Machine, len(machines))
	for _, m := range machines {
		byID[m.ID] = m
	}
	selected := make([]fleet.Machine, 0, len(ids))
	seen := map[string]bool{}
	for _, id := range ids {
		m, ok := byID[id]
		if !ok || id == "" {
			return nil, errors.New("selection contains an unmanaged ID; nothing deleted")
		}
		if !seen[id] {
			selected = append(selected, m)
			seen[id] = true
		}
	}
	return selected, nil
}

func readSelection(ctx context.Context, scanner *bufio.Scanner) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	type line struct {
		text string
		err  error
	}
	done := make(chan line, 1)
	go func() {
		if scanner.Scan() {
			done <- line{text: scanner.Text()}
		} else {
			err := scanner.Err()
			if err == nil {
				err = io.EOF
			}
			done <- line{err: err}
		}
	}()
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case value := <-done:
		return value.text, value.err
	}
}
