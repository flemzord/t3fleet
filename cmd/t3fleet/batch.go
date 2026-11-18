package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/flemzord/t3fleet/internal/config"
	"github.com/flemzord/t3fleet/internal/fleet"
)

type batchResult struct {
	Machine     fleet.Machine `json:"machine"`
	ErrorDetail string        `json:"errorDetail,omitempty"`
	Error       string        `json:"error,omitempty"`
	Association string        `json:"association"`
}

func runBatchWithInput(ctx context.Context, cfg config.Config, req fleet.BatchRequest, concurrency int, resume, register, asJSON bool, input io.Reader, interactive bool, out, diagnostics io.Writer) error {
	if register && !interactive {
		return hinted("add --no-register, then run 't3fleet open' for each machine later", "batch association needs a terminal to confirm each T3 handoff")
	}
	if concurrency < 1 || concurrency > fleet.MaxConcurrency || (!resume && (req.Count < 1 || req.Count > fleet.MaxBatchCount)) {
		return errors.New("--count must be between 1 and 100 and --concurrency between 1 and 10")
	}
	token, err := readAdminToken(cfg.StateDir)
	if err != nil {
		return errDaemonCredentials
	}
	if req.ID == "" {
		var raw [16]byte
		if _, err = rand.Read(raw[:]); err != nil {
			return err
		}
		req.ID = hex.EncodeToString(raw[:])
	}
	if raw, err := hex.DecodeString(req.ID); err != nil || len(raw) != 16 {
		return errors.New("batch ID must contain 32 hexadecimal characters")
	}
	// Print before sending: even a lost prepare response has an explicit recovery key.
	fmt.Fprintf(diagnostics, "Batch %s; recover with: t3fleet create --resume %s (use the same --config)\n", req.ID, req.ID)
	var data []byte
	if resume {
		data, err = callAdmin(ctx, cfg.Listen, token, http.MethodGet, "/v1/batches/"+req.ID, nil)
	} else {
		data, err = callAdmin(ctx, cfg.Listen, token, http.MethodPost, "/v1/batches", req)
	}
	if err != nil {
		return err
	}
	var plan fleet.Batch
	if json.Unmarshal(data, &plan) != nil || plan.ID != req.ID || len(plan.Machines) == 0 || len(plan.Machines) > fleet.MaxBatchCount {
		return errors.New("invalid batch response")
	}
	results := make([]batchResult, len(plan.Machines))
	jobs := make(chan int)
	var wg sync.WaitGroup
	var outputMu sync.Mutex
	for range concurrency {
		wg.Go(func() {
			for i := range jobs {
				machine := plan.Machines[i]
				result := batchResult{Machine: machine, Association: "skipped"}
				started := time.Now()
				var startErr error
				if machine.State != "ready" && machine.State != "deleted" {
					outputMu.Lock()
					fmt.Fprintf(diagnostics, "%s (%s): waiting for T3 readiness\n", machine.Name, machine.ID)
					outputMu.Unlock()
					_, startErr = callAdmin(ctx, cfg.Listen, token, http.MethodPost, "/v1/machines/"+url.PathEscape(machine.ID)+"/start", nil)
				}
				if startErr != nil {
					result.Error = "start_failed"
					result.ErrorDetail = startErr.Error()
					result.Machine.State = "incomplete"
				} else if machine.State != "deleted" {
					result.Machine.State = "ready"
				}
				outputMu.Lock()
				fmt.Fprintf(diagnostics, "%s (%s): %s (%s)\n", machine.Name, machine.ID, result.Machine.State, time.Since(started).Round(time.Second))
				outputMu.Unlock()
				results[i] = result
			}
		})
	}
	for i := range plan.Machines {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	failed := false
	scanner := bufio.NewScanner(input)
	deferAssociation := false
	for i := range results {
		result := &results[i]
		// Refresh visible durable status, including any retained remote identity.
		if data, err := callAdmin(ctx, cfg.Listen, token, http.MethodGet, "/v1/machines/"+url.PathEscape(result.Machine.ID), nil); err == nil {
			_ = json.Unmarshal(data, &result.Machine)
		}
		if result.Error != "" {
			failed = true
			continue
		}
		if register && result.Machine.State == "ready" {
			if deferAssociation {
				result.Association = "pending"
				failed = true
				continue
			}
			payload, _ := json.Marshal(result.Machine)
			if err := registerCreatedMachine(ctx, cfg, token, payload, diagnostics); err != nil {
				result.Error = "association_failed"
				result.ErrorDetail = err.Error()
				result.Association = "failed"
				failed = true
				fmt.Fprintln(diagnostics, err)
			} else {
				result.Association = "confirmation_required"
				if i < len(results)-1 {
					fmt.Fprintln(diagnostics, "Confirm this backend in T3, then press Enter for the next machine (q stops further handoffs):")
					line, err := readSelection(ctx, scanner)
					if err != nil || strings.TrimSpace(line) != "" {
						deferAssociation = true
					}
				}
			}
		}
	}
	if asJSON {
		if err := json.NewEncoder(out).Encode(struct {
			ID      string        `json:"batchId"`
			Results []batchResult `json:"results"`
		}{plan.ID, results}); err != nil {
			return err
		}
	} else {
		printBatchResults(out, results)
	}
	if failed {
		return hinted(fmt.Sprintf("run 't3fleet create --resume %s', or use start/open with the machines above", plan.ID), "batch %s is incomplete", plan.ID)
	}
	return ctx.Err()
}

func printBatchResults(w io.Writer, results []batchResult) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tID\tSTATE\tASSOCIATION\tERROR")
	for _, r := range results {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", displayName(r.Machine), r.Machine.ID, orDash(r.Machine.State), r.Association, orDash(r.Error))
	}
	tw.Flush()
}
