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
	List() []fleet.Machine
	ObservedList(context.Context) []fleet.ObservedMachine
	Get(string) (fleet.Machine, error)
	Start(context.Context, string) error
	Stop(context.Context, string) error
	Delete(context.Context, string) error
	Snapshot(context.Context, string, string) (provider.Snapshot, error)
	Pair(context.Context, string) (proxy.Pairing, error)
}

func serve(ctx context.Context, cfg config.Config, diagnostics io.Writer) error {
	return serveWithController(ctx, cfg, diagnostics, startDispatchController)
}

type controllerStarter func(context.Context, config.Config, io.Writer) (proxy.DispatchWorker, func(), error)

func serveWithController(ctx context.Context, cfg config.Config, diagnostics io.Writer, startController controllerStarter) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	providers, err := cfg.BuildProviders()
	if err != nil {
		return err
	}
	disk, err := state.Open(cfg.StateDir)
	if err != nil {
		return fmt.Errorf("cannot open private fleet state: %w", err)
	}
	defer func() { _ = disk.Close() }()
	token, err := ensureAdminToken(cfg.StateDir)
	if err != nil {
		return errors.New("cannot initialize private daemon token")
	}
	manager, err := fleet.New(fleet.Options{Providers: providers, Templates: cfg.ProviderTemplates(), Sizes: cfg.ProviderSizes(), Store: disk, ExpectedVersion: cfg.ExpectedVersion})
	if err != nil {
		return fmt.Errorf("cannot initialize fleet: %w", err)
	}
	defer func() {
		shutdown, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = manager.Close(shutdown)
	}()
	// Keep dispatch state at its original path: a new directory would lose
	// pairing, project metadata and the link to already allocated workers.
	root := filepath.Join(cfg.StateDir, "dispatch-boat")
	store, err := state.Open(root)
	if err != nil {
		return fmt.Errorf("cannot open private dispatch state: %w", err)
	}
	defer func() { _ = store.Close() }()
	controller, stop, err := startController(ctx, cfg, diagnostics)
	if err != nil {
		return err
	}
	defer stop()
	settings := cfg.Dispatch
	var releaseWorker func(context.Context, string) error
	if settings.DeleteWorkerOnArchive {
		releaseWorker = manager.ReleaseDispatchWorker
	}
	dispatch, err := proxy.NewDispatchOnDemand("http://"+settings.Listen, store, proxy.DispatchOnDemand{
		Controller: controller, ProjectRoot: filepath.Join(root, "projects"),
		MaxWorkers: settings.MaxWorkerCount(), Model: settings.Model,
		Provision: func(ctx context.Context, key, remote string) (proxy.DispatchWorker, string, error) {
			return manager.ProvisionDispatchWorker(ctx, key, remote, settings.ProviderOrder)
		},
		Lookup: manager.LookupDispatchWorker, Release: releaseWorker, Reclaim: manager.ReclaimDispatchWorker,
		Lost: manager.DispatchWorkerLost,
	})
	if err != nil {
		return fmt.Errorf("cannot initialize T3 dispatch: %w", err)
	}
	ln, err := net.Listen("tcp", settings.Listen)
	if err != nil {
		return fmt.Errorf("cannot listen for T3 requests on %s: %w", settings.Listen, err)
	}
	defer func() { _ = ln.Close() }()
	listener, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return errors.New("cannot listen for admin requests; check listen address or another daemon")
	}
	defer func() { _ = listener.Close() }()
	lifetime, cancel := context.WithCancel(ctx)
	base := func(net.Listener) context.Context { return lifetime }
	t3 := &http.Server{Handler: dispatch, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 60 * time.Second,
		MaxHeaderBytes: 64 << 10, BaseContext: base}
	admin := &http.Server{Handler: newAdminHandlerWithDispatch(manager, token, dispatch), ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout: 30 * time.Second, WriteTimeout: 11 * time.Minute, IdleTimeout: 60 * time.Second, BaseContext: base}
	cleanupDone := make(chan struct{})
	go func() {
		defer close(cleanupDone)
		dispatch.RunDispatchCleanup(lifetime, func(err error) {
			fmt.Fprintln(diagnostics, "t3fleet dispatch cleanup:", err)
		})
	}()
	defer func() {
		cancel()
		_ = t3.Close()
		_ = admin.Close()
		<-cleanupDone
	}()
	done := make(chan error, 2)
	go func() { done <- t3.Serve(ln) }()
	go func() { done <- admin.Serve(listener) }()
	fmt.Fprintln(diagnostics, "t3fleet listening for T3 on", ln.Addr().String())
	fmt.Fprintln(diagnostics, "t3fleet control listening on", listener.Addr().String())
	printServeSummary(diagnostics, cfg, manager.List())
	select {
	case err := <-done:
		return fmt.Errorf("fleet listener stopped: %w", err)
	case <-ctx.Done():
		cancel()
		shutdown, stopShutdown := context.WithTimeout(context.Background(), 15*time.Second)
		defer stopShutdown()
		return errors.Join(t3.Shutdown(shutdown), admin.Shutdown(shutdown))
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

		if r.URL.Path == "/v1/dispatch/pair" {
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
		if r.URL.Path == "/v1/dispatch/pair-peer" {
			w.Header().Set("Cache-Control", "no-store")
			switch {
			case dispatch == nil:
				safeError(w, http.StatusNotFound, "not_found")
			case r.Method != http.MethodPost:
				w.WriteHeader(http.StatusMethodNotAllowed)
			default:
				var request struct {
					Access string `json:"access"`
				}
				if !decodeRequest(w, r, &request) {
					return
				}
				code, err := dispatch.NewPeerPairing(ctx, request.Access)
				if err != nil {
					safeError(w, http.StatusBadRequest, err.Error())
				} else {
					writeJSON(w, http.StatusOK, map[string]string{"credential": code})
				}
			}
			return
		}

		if r.URL.Path == "/v1/machines" {
			switch r.Method {
			case http.MethodGet:
				if r.URL.Query().Get("observe") == "1" {
					writeJSON(w, http.StatusOK, manager.ObservedList(ctx))
				} else {
					writeJSON(w, http.StatusOK, manager.List())
				}
			case http.MethodPost:
				var request machineCreateRequest
				if !decodeRequest(w, r, &request) {
					return
				}
				if request.Name == "" || request.Provider == "" {
					safeError(w, http.StatusBadRequest, "invalid_request")
					return
				}
				machine, err := manager.Create(ctx, fleet.CreateRequest{Name: request.Name, Provider: request.Provider})
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
			if err == nil && dispatch != nil && dispatch.RestoreWorkerListing(ctx, id) != nil {
				safeError(w, http.StatusBadGateway, "worker_started_listing_unavailable")
				return
			}
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
		_ = file.Close()
		return "", err
	}
	if err = file.Sync(); err != nil {
		_ = file.Close()
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
	defer func() { _ = file.Close() }()
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

// printServeSummary runs after the listener is ready, so readers that wait
// for the first daemon output can already reach the control API.
func printServeSummary(w io.Writer, cfg config.Config, machines []fleet.Machine) {
	providers := strings.Join(configuredProviders(cfg), ", ")
	if providers == "" {
		providers = "none configured"
	}
	byState := map[string]int{}
	for _, m := range machines {
		byState[m.State]++
	}
	fmt.Fprintln(w, "  providers:", providers)
	fmt.Fprintln(w, "  state:    ", cfg.StateDir)
	fmt.Fprintln(w, "  machines: ", machineSummary(byState, len(machines)))
	fmt.Fprintln(w, "Cloud machines keep running and billing after the daemon stops; use 't3fleet admin stop' or 't3fleet admin delete'.")
}
