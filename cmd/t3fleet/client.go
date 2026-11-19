package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/flemzord/t3fleet/internal/config"
	"github.com/flemzord/t3fleet/internal/fleet"
)

var loadConfig = config.Load

// daemonClient is an authenticated handle on the local control API.
type daemonClient struct {
	cfg   config.Config
	token string
}

// machineCreateRequest selects a configured instance. The daemon owns its template and size.
type machineCreateRequest struct {
	Name     string `json:"name"`
	Provider string `json:"provider"`
}

func (d daemonClient) call(ctx context.Context, method, path string, body any) ([]byte, error) {
	return callAdmin(ctx, d.cfg.Listen, d.token, method, path, body)
}

func (d daemonClient) machines(ctx context.Context) ([]fleet.Machine, error) {
	data, err := d.call(ctx, http.MethodGet, "/v1/machines", nil)
	if err != nil {
		return nil, err
	}
	var machines []fleet.Machine
	if json.Unmarshal(data, &machines) != nil {
		return nil, errors.New("invalid daemon response")
	}
	return machines, nil
}

// Only inventory commands ask for provider observations. Reference resolution
// and shell completion keep using the local, inexpensive machine list.
func (d daemonClient) inventory(ctx context.Context) ([]byte, error) {
	// Allow time for the daemon's bounded provider read and the local response.
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return d.call(ctx, http.MethodGet, "/v1/machines?observe=1", nil)
}

func parseInventory(data []byte) ([]fleet.ObservedMachine, error) {
	var machines []fleet.ObservedMachine
	if json.Unmarshal(data, &machines) != nil {
		return nil, errors.New("invalid daemon response")
	}
	return machines, nil
}

func (d daemonClient) resolve(ctx context.Context, ref string, allowPrefix bool) (fleet.Machine, error) {
	if err := validateMachineRef(ref); err != nil {
		return fleet.Machine{}, err
	}
	machines, err := d.machines(ctx)
	if err != nil {
		return fleet.Machine{}, err
	}
	return resolveMachine(machines, ref, allowPrefix)
}

func validMachineID(id string) bool {
	raw, err := hex.DecodeString(id)
	return err == nil && len(raw) == 16
}

var errorDescriptions = map[string]string{
	"not_found":        "the machine or operation was not found",
	"unsupported":      "the provider does not support this operation",
	"invalid_request":  "the daemon rejected the request",
	"conflict":         "the machine is busy or in a conflicting state",
	"operation_failed": "the provider operation failed",
	"unauthorized":     "the daemon rejected the CLI credentials",
}

func callAdmin(ctx context.Context, address, token, method, path string, body any) ([]byte, error) {
	var data []byte
	var err error
	if body != nil {
		data, err = json.Marshal(body)
		if err != nil {
			return nil, errors.New("cannot encode request")
		}
	}
	request, err := http.NewRequestWithContext(ctx, method, "http://"+address+path, bytes.NewReader(data))
	if err != nil {
		return nil, errors.New("cannot construct daemon request")
	}
	request.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	client := http.Client{Timeout: 10 * time.Minute, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, hinted("start it with 't3fleet serve' using the same --config", "cannot reach the fleet daemon on %s", address)
	}
	defer func() { _ = response.Body.Close() }()
	data, err = io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if err != nil || len(data) > 1<<20 {
		return nil, errors.New("invalid daemon response")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, daemonError(response.StatusCode, method, path, data)
	}
	return data, nil
}

// daemonError maps the daemon's closed set of safe error codes to guidance.
// It never renders vendor bodies or unknown codes.
func daemonError(status int, method, path string, data []byte) error {
	var failure struct {
		Error   string `json:"error"`
		Machine struct {
			ID string `json:"id"`
		} `json:"machine"`
	}
	_ = json.Unmarshal(data, &failure)
	id := failure.Machine.ID
	if !validMachineID(id) {
		id = ""
	}
	switch failure.Error {
	case "worker_started_listing_unavailable":
		return hinted("retry 't3fleet admin start' for this worker or use 't3fleet admin pair' to inspect it directly",
			"the worker started, but its thread listing could not be restored")
	case "runtime_unavailable":
		if id != "" {
			return hinted(fmt.Sprintf("prepare its T3 runtime, then run 't3fleet admin start %s'; or delete it and use a prepared t3fleet snapshot", id),
				"the image is missing t3fleet-runtime-info or its required executables; machine %s remains allocated", id)
		}
		return hinted("use a prepared t3fleet snapshot, or prepare the machine's T3 runtime before retrying start",
			"the image is missing t3fleet-runtime-info or its required executables")
	case "not_found", "unsupported", "invalid_request", "conflict", "operation_failed", "unauthorized":
		if failure.Error == "conflict" && method == http.MethodPost && path == "/v1/machines" {
			if id != "" {
				return hinted(fmt.Sprintf("use another --name to create a new machine, or run 't3fleet inspect %s'", id),
					"machine name already exists (existing machine %s)", id)
			}
			return hinted("use another --name to create a new machine, or run 't3fleet list' to find the existing one",
				"machine name already exists")
		}
		description := errorDescriptions[failure.Error] + " (" + failure.Error + ")"
		if id != "" {
			return hinted(fmt.Sprintf("run 't3fleet inspect %s', or 't3fleet admin start %s' to recover", id, id),
				"%s; machine %s remains managed", description, id)
		}
		if failure.Error == "unauthorized" {
			return &hintError{msg: description, hint: "restart 't3fleet serve' with the same configuration", code: failure.Error}
		}
		return &hintError{msg: description, code: failure.Error}
	default:
		return fmt.Errorf("daemon request failed (HTTP %d)", status)
	}
}
