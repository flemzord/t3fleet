package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/flemzord/t3fleet/internal/config"
	"github.com/flemzord/t3fleet/internal/fleet"
	"github.com/flemzord/t3fleet/internal/proxy"
	"github.com/flemzord/t3fleet/internal/state"
	"github.com/flemzord/t3fleet/pkg/provider"
)

type fleetAPI interface {
	Create(context.Context, fleet.CreateRequest) (fleet.Machine, error)
	PrepareBatch(context.Context, fleet.BatchRequest) (fleet.Batch, error)
	Batch(string) (fleet.Batch, error)
	List() []fleet.Machine
	Get(string) (fleet.Machine, error)
	Start(context.Context, string) error
	Stop(context.Context, string) error
	Delete(context.Context, string) error
	Snapshot(context.Context, string, string) (provider.Snapshot, error)
	Pair(context.Context, string) (proxy.Pairing, error)
}

func serve(ctx context.Context, cfg config.Config, diagnostics io.Writer) error {
	providers, err := cfg.BuildProviders()
	if err != nil {
		return err
	}
	disk, err := state.Open(cfg.StateDir)
	if err != nil {
		return fmt.Errorf("cannot open private fleet state: %w", err)
	}
	defer disk.Close()
	token, err := ensureAdminToken(cfg.StateDir)
	if err != nil {
		return errors.New("cannot initialize private daemon token")
	}
	idleTimeout, err := cfg.IdleDuration()
	if err != nil {
		return err
	}
	manager, err := fleet.New(fleet.Options{IdleTimeout: idleTimeout, Providers: providers, Templates: cfg.Templates, Store: disk, ExpectedVersion: cfg.ExpectedVersion, ListenHost: cfg.MachineHost})
	if err != nil {
		return errors.New("cannot initialize fleet; check configuration and state")
	}
	defer func() {
		shutdown, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = manager.Close(shutdown)
	}()
	var dispatch *proxy.Proxy
	if poc := cfg.DispatchPOC; poc != nil {
		stateName := "dispatch-poc"
		if poc.OnDemand != nil {
			stateName = "dispatch-boat"
		}
		store, err := state.Open(filepath.Join(cfg.StateDir, stateName))
		if err != nil {
			return fmt.Errorf("cannot open private dispatch state: %w", err)
		}
		defer store.Close()
		origin := "http://" + poc.Listen
		if poc.OnDemand != nil {
			controller, stop, startErr := startDispatchController(ctx, cfg, diagnostics)
			if startErr != nil {
				return startErr
			}
			defer stop()
			dispatch, err = proxy.NewDispatchOnDemand(origin, store, proxy.DispatchOnDemand{
				Controller: controller, ProjectRoot: filepath.Join(cfg.StateDir, stateName, "projects"),
				MaxWorkers: poc.OnDemand.MaxWorkerCount(), Model: poc.OnDemand.Model,
				Provision: manager.ProvisionDispatchBoat, Lookup: manager.LookupDispatchBoat,
			})
		} else {
			var workers []proxy.DispatchWorker
			for _, id := range poc.Workers {
				worker, err := manager.DispatchWorker(id)
				if err != nil {
					return fmt.Errorf("dispatch worker %q is missing or unprepared: %w", id, err)
				}
				workers = append(workers, worker)
			}
			dispatch, err = proxy.NewDispatchPOC(origin, store, workers)
		}
		if err != nil {
			return fmt.Errorf("cannot initialize dispatch POC: %w", err)
		}
		ln, err := net.Listen("tcp", poc.Listen)
		if err != nil {
			return errors.New("cannot listen for dispatch requests")
		}
		pocCtx, cancel := context.WithCancel(ctx)
		srv := &http.Server{Handler: dispatch, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 60 * time.Second,
			MaxHeaderBytes: 64 << 10, BaseContext: func(net.Listener) context.Context { return pocCtx }}
		defer func() { cancel(); _ = srv.Close() }()
		go func() { _ = srv.Serve(ln) }()
		fmt.Fprintln(diagnostics, "t3fleet dispatch POC listening on", ln.Addr().String())
	}
	listener, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return errors.New("cannot listen for admin requests; check listen address or another daemon")
	}
	server := http.Server{Handler: newAdminHandlerWithDispatch(manager, token, dispatch), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 11 * time.Minute, IdleTimeout: 60 * time.Second}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	fmt.Fprintln(diagnostics, "t3fleet control listening on", listener.Addr().String())
	select {
	case err := <-done:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return errors.New("admin server stopped unexpectedly")
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdown); err != nil {
			_ = server.Close()
			return errors.New("admin shutdown timed out")
		}
		return nil
	}
}

func newAdminHandler(manager fleetAPI, token string) http.Handler {
	return newAdminHandlerWithDispatch(manager, token, nil)
}

func newAdminHandlerWithDispatch(manager fleetAPI, token string, dispatch *proxy.Proxy) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		ip := net.ParseIP(host)
		if ip == nil || !ip.IsLoopback() {
			safeError(w, http.StatusForbidden, "unauthorized")
			return
		}
		// This is a CLI control surface, never a cross-origin browser API.
		if r.Header.Get("Origin") != "" {
			safeError(w, http.StatusForbidden, "unauthorized")
			return
		}
		if r.Method == http.MethodGet && r.URL.Path == "/_healthcheck" {
			writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
			return
		}
		if r.Method == http.MethodGet && r.URL.Path == "/_info" {
			writeJSON(w, http.StatusOK, map[string]string{"name": "t3fleet", "version": version})
			return
		}
		supplied := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if token == "" || subtle.ConstantTimeCompare([]byte(supplied), []byte(token)) != 1 || !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			safeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
		defer cancel()

		if r.URL.Path == "/v1/dispatch-poc/pair" {
			if dispatch == nil {
				safeError(w, http.StatusNotFound, "not_found")
			} else if r.Method != http.MethodPost {
				w.WriteHeader(http.StatusMethodNotAllowed)
			} else if pair, err := dispatch.NewPairing(); err != nil {
				operationError(w, err)
			} else {
				writeJSON(w, http.StatusOK, pair)
			}
			return
		}

		if r.URL.Path == "/v1/batches" || strings.HasPrefix(r.URL.Path, "/v1/batches/") {
			var batch fleet.Batch
			var err error
			switch {
			case r.URL.Path == "/v1/batches" && r.Method == http.MethodPost:
				var request fleet.BatchRequest
				if !decodeRequest(w, r, &request) {
					return
				}
				batch, err = manager.PrepareBatch(ctx, request)
			case strings.HasPrefix(r.URL.Path, "/v1/batches/") && r.Method == http.MethodGet:
				batch, err = manager.Batch(strings.TrimPrefix(r.URL.Path, "/v1/batches/"))
			default:
				w.WriteHeader(http.StatusMethodNotAllowed)
				return
			}
			if err != nil {
				operationError(w, err)
				return
			}
			writeJSON(w, http.StatusOK, batch)
			return
		}
		if r.URL.Path == "/v1/machines" {
			switch r.Method {
			case http.MethodGet:
				writeJSON(w, http.StatusOK, manager.List())
			case http.MethodPost:
				var request fleet.CreateRequest
				if !decodeRequest(w, r, &request) {
					return
				}
				if request.Name == "" || request.Provider == "" {
					safeError(w, http.StatusBadRequest, "invalid_request")
					return
				}
				machine, err := manager.Create(ctx, request)
				if err != nil {
					if machine.ID != "" {
						status, code := operationFailure(err)
						writeJSON(w, status, struct {
							Error   string        `json:"error"`
							Machine fleet.Machine `json:"machine"`
						}{code, machine})
					} else {
						operationError(w, err)
					}
					return
				}
				writeJSON(w, http.StatusCreated, machine)
			default:
				w.WriteHeader(http.StatusMethodNotAllowed)
			}
			return
		}
		suffix, ok := strings.CutPrefix(r.URL.Path, "/v1/machines/")
		if !ok || suffix == "" {
			safeError(w, http.StatusNotFound, "not_found")
			return
		}
		parts := strings.Split(suffix, "/")
		if len(parts) > 2 || parts[0] == "" {
			safeError(w, http.StatusNotFound, "not_found")
			return
		}
		id := parts[0]
		if len(parts) == 1 {
			switch r.Method {
			case http.MethodGet:
				machine, err := manager.Get(id)
				if err != nil {
					operationError(w, err)
					return
				}
				writeJSON(w, http.StatusOK, machine)
			case http.MethodDelete:
				if err := manager.Delete(ctx, id); err != nil {
					operationError(w, err)
					return
				}
				w.WriteHeader(http.StatusNoContent)
			default:
				w.WriteHeader(http.StatusMethodNotAllowed)
			}
			return
		}
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		var err error
		switch parts[1] {
		case "start":
			err = manager.Start(ctx, id)
		case "stop":
			err = manager.Stop(ctx, id)
		case "pair":
			pair, pairErr := manager.Pair(ctx, id)
			if pairErr != nil {
				operationError(w, pairErr)
				return
			}
			writeJSON(w, http.StatusOK, pair)
			return
		case "snapshot":
			var request struct {
				Name string `json:"name"`
			}
			if !decodeRequest(w, r, &request) {
				return
			}
			if request.Name == "" {
				safeError(w, http.StatusBadRequest, "invalid_request")
				return
			}
			snapshot, snapErr := manager.Snapshot(ctx, id, request.Name)
			if snapErr != nil {
				operationError(w, snapErr)
				return
			}
			writeJSON(w, http.StatusOK, snapshot)
			return
		default:
			safeError(w, http.StatusNotFound, "not_found")
			return
		}
		if err != nil {
			operationError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}

func decodeRequest(w http.ResponseWriter, r *http.Request, dst any) bool {
	if ct := r.Header.Get("Content-Type"); ct != "application/json" && !strings.HasPrefix(ct, "application/json;") {
		safeError(w, http.StatusUnsupportedMediaType, "invalid_request")
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if dec.Decode(dst) != nil {
		safeError(w, http.StatusBadRequest, "invalid_request")
		return false
	}
	var extra any
	if dec.Decode(&extra) != io.EOF {
		safeError(w, http.StatusBadRequest, "invalid_request")
		return false
	}
	return true
}

func operationError(w http.ResponseWriter, err error) {
	status, code := operationFailure(err)
	safeError(w, status, code)
}

func operationFailure(err error) (int, string) {
	switch {
	case errors.Is(err, provider.ErrNotFound), errors.Is(err, fleet.ErrNotFound):
		return http.StatusNotFound, "not_found"
	case errors.Is(err, fleet.ErrInvalid):
		return http.StatusBadRequest, "invalid_request"
	case errors.Is(err, fleet.ErrConflict):
		return http.StatusConflict, "conflict"
	case errors.Is(err, fleet.ErrRuntimeUnavailable):
		return http.StatusUnprocessableEntity, "runtime_unavailable"
	case errors.Is(err, provider.ErrUnsupported):
		return http.StatusNotImplemented, "unsupported"
	case errors.Is(err, context.Canceled):
		return http.StatusRequestTimeout, "operation_failed"
	case errors.Is(err, context.DeadlineExceeded):
		return http.StatusGatewayTimeout, "operation_failed"
	default:
		return http.StatusBadGateway, "operation_failed"
	}
}
func safeError(w http.ResponseWriter, status int, code string) {
	writeJSON(w, status, map[string]string{"error": code})
}
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func ensureAdminToken(dir string) (string, error) {
	if token, err := readAdminToken(dir); err == nil {
		return token, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	token := hex.EncodeToString(raw)
	file, err := os.OpenFile(filepath.Join(dir, "admin-token"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return "", err
	}
	if _, err = file.WriteString(token + "\n"); err != nil {
		file.Close()
		return "", err
	}
	if err = file.Sync(); err != nil {
		file.Close()
		return "", err
	}
	if err = file.Close(); err != nil {
		return "", err
	}
	return token, nil
}
func readAdminToken(dir string) (string, error) {
	path := filepath.Join(dir, "admin-token")
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return "", errors.New("unsafe admin token")
	}
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	current, err := file.Stat()
	if err != nil || !os.SameFile(info, current) || !current.Mode().IsRegular() || current.Mode().Perm()&0o077 != 0 {
		return "", errors.New("admin token changed while opening")
	}
	data, err := io.ReadAll(io.LimitReader(file, 128))
	if err != nil {
		return "", err
	}
	token := strings.TrimSpace(string(data))
	raw, err := hex.DecodeString(token)
	if err != nil || len(raw) != 32 {
		return "", errors.New("invalid admin token")
	}
	return token, nil
}
