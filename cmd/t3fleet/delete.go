package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/flemzord/t3fleet/internal/config"
	"github.com/flemzord/t3fleet/internal/fleet"
	"golang.org/x/term"
)

func interactiveInput() bool { return term.IsTerminal(int(os.Stdin.Fd())) }

func runDelete(ctx context.Context, cfg config.Config, args []string, input io.Reader, interactive bool, out, diagnostics io.Writer) error {
	fs := flag.NewFlagSet("delete", flag.ContinueOnError)
	fs.SetOutput(diagnostics)
	idsArg := fs.String("ids", "", "comma-separated managed IDs")
	yes := fs.Bool("yes", false, "confirm exactly the explicit IDs")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 || (*idsArg == "" && *yes) {
		return errors.New("use delete ID, delete interactively, or delete --ids ID,ID --yes")
	}
	if !interactive && (*idsArg == "" || !*yes) {
		return errors.New("without a TTY use delete ID or delete --ids ID,ID --yes")
	}
	token, err := readAdminToken(cfg.StateDir)
	if err != nil {
		return errors.New("daemon credentials unavailable")
	}
	data, err := callAdmin(ctx, cfg.Listen, token, http.MethodGet, "/v1/machines", nil)
	if err != nil {
		return err
	}
	var machines []fleet.Machine
	if json.Unmarshal(data, &machines) != nil {
		return errors.New("invalid daemon response")
	}
	var ids []string
	scanner := bufio.NewScanner(input)
	if *idsArg != "" {
		ids = strings.Split(*idsArg, ",")
	} else {
		if len(machines) == 0 {
			fmt.Fprintln(diagnostics, "No managed machines.")
			return nil
		}
		for i, m := range machines {
			fmt.Fprintf(diagnostics, "%d\t%s\t%s\t%s\t%s\n", i+1, m.Name, m.Provider, m.State, m.ID)
		}
		fmt.Fprintln(diagnostics, "Select numbers separated by spaces (empty or q cancels):")
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
		fmt.Fprintln(diagnostics, "Empty selection; nothing deleted.")
		return nil
	}
	fmt.Fprintln(diagnostics, "Selected for permanent deletion:")
	for _, m := range selected {
		fmt.Fprintf(diagnostics, "%s\t%s\t%s\t%s\n", m.Name, m.Provider, m.State, m.ID)
	}
	if !*yes {
		fmt.Fprintln(diagnostics, "Type delete to confirm this exact selection (anything else cancels):")
		line, err := readSelection(ctx, scanner)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if line != "delete" {
			fmt.Fprintln(diagnostics, "Cancelled; nothing deleted.")
			return nil
		}
	}
	type result struct {
		ID      string `json:"id"`
		Deleted bool   `json:"deleted"`
	}
	results := make([]result, 0, len(selected))
	failed := false
	for _, m := range selected {
		if err := ctx.Err(); err != nil {
			failed = true
			results = append(results, result{ID: m.ID})
			continue
		}
		finish := startProgress(diagnostics, progressMessages{"Deleting " + m.Name + " (" + m.ID + ")", "Deleted " + m.ID, "Deletion failed for " + m.ID}, interactiveProgress(diagnostics))
		_, err := callAdmin(ctx, cfg.Listen, token, http.MethodDelete, "/v1/machines/"+url.PathEscape(m.ID), nil)
		finish(err)
		results = append(results, result{m.ID, err == nil})
		if err != nil {
			failed = true
		}
	}
	if err := json.NewEncoder(out).Encode(results); err != nil {
		return err
	}
	if failed {
		return errors.New("some selected machines were not deleted; inspect reported IDs before retrying")
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
