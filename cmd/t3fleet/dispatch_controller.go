package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/flemzord/t3fleet/internal/config"
	"github.com/flemzord/t3fleet/internal/proxy"
	"github.com/flemzord/t3fleet/internal/state"
	"github.com/flemzord/t3fleet/pkg/provider"
)

const dispatchControllerID = "controller"

// startDispatchController starts a genuine local T3 service for the
// orchestration metadata protocol. Its directory and environment are separate
// from the user's Desktop installation; no user credentials are imported.
func startDispatchController(ctx context.Context, cfg config.Config, diagnostics io.Writer) (proxy.DispatchWorker, func(), error) {
	if cfg.Dispatch == nil {
		return proxy.DispatchWorker{}, nil, errors.New("local dispatch controller requires dispatch configuration")
	}
	binary := cfg.Dispatch.T3Binary
	if info, err := os.Stat(binary); err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return proxy.DispatchWorker{}, nil, errors.New("dispatch controller T3 binary is not executable")
	}
	root := filepath.Join(cfg.StateDir, "dispatch-controller")
	lock, err := state.Open(root)
	if err != nil {
		return proxy.DispatchWorker{}, nil, fmt.Errorf("cannot open private dispatch controller directory: %w", err)
	}
	home := filepath.Join(root, "home")
	base := filepath.Join(root, "runtime")
	userdata := filepath.Join(base, "userdata")
	for _, path := range []string{home, base, userdata, filepath.Join(home, "tmp")} {
		if err := privateControllerDirectory(path); err != nil {
			_ = lock.Close()
			return proxy.DispatchWorker{}, nil, err
		}
	}
	var drivers []string
	for _, agent := range cfg.Dispatch.AgentCatalog() {
		drivers = append(drivers, agent.Driver)
	}
	if err := prepareControllerSettings(userdata, drivers); err != nil {
		_ = lock.Close()
		return proxy.DispatchWorker{}, nil, err
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		_ = lock.Close()
		return proxy.DispatchWorker{}, nil, errors.New("cannot reserve local dispatch controller port")
	}
	address := listener.Addr().String()
	_, port, _ := net.SplitHostPort(address)
	_ = listener.Close()
	origin := "http://" + address
	env := controllerEnvironment(home, base)

	lifetime, cancel := context.WithCancel(ctx)
	command := exec.CommandContext(lifetime, binary, "serve", "--mode", "desktop", "--host", "127.0.0.1", "--port", port, "--base-dir", base, "--no-browser")
	command.Dir, command.Env = home, env
	command.Stdout, command.Stderr = io.Discard, io.Discard
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error {
		err := syscall.Kill(-command.Process.Pid, syscall.SIGINT)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	command.WaitDelay = 5 * time.Second
	if err := command.Start(); err != nil {
		cancel()
		_ = lock.Close()
		return proxy.DispatchWorker{}, nil, errors.New("could not launch local T3 dispatch controller")
	}
	done := make(chan struct{})
	go func() {
		_ = command.Wait()
		close(done)
	}()
	cleanup := sync.OnceFunc(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(6 * time.Second):
			_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
			<-done
		}
		_ = lock.Close()
	})
	fail := func(err error) (proxy.DispatchWorker, func(), error) {
		cleanup()
		return proxy.DispatchWorker{}, nil, err
	}
	transport := &http.Transport{}
	defer transport.CloseIdleConnections()
	client := &http.Client{
		Transport: transport,
		Timeout:   time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	ready, stopReady := context.WithTimeout(lifetime, 45*time.Second)
	defer stopReady()
	descriptor, err := waitControllerDescriptor(ready, done, client, origin, cfg.ExpectedVersion)
	if err != nil {
		return fail(err)
	}
	// Mint short-lived sessions against the dedicated state. Refresh before a
	// new upstream connection uses an expired token; existing sockets continue.
	var mu sync.Mutex
	var token string
	var refreshAt time.Time
	resolve := func(ctx context.Context) (proxy.Upstream, error) {
		mu.Lock()
		defer mu.Unlock()
		if err := ctx.Err(); err != nil {
			return proxy.Upstream{}, err
		}
		select {
		case <-done:
			return proxy.Upstream{}, errors.New("local T3 dispatch controller is unavailable")
		default:
		}
		if token == "" || time.Now().After(refreshAt) {
			issueCtx, stop := context.WithTimeout(ctx, 15*time.Second)
			defer stop()
			issue := exec.CommandContext(issueCtx, binary, "auth", "session", "issue", "--base-dir", base, "--token-only", "--ttl", "1h", "--label", "t3fleet-dispatch-controller")
			issue.Dir, issue.Env, issue.Stderr = home, env, io.Discard
			out, err := issue.Output()
			value := strings.TrimSpace(string(out))
			if err != nil || len(value) == 0 || len(value) > 4096 || strings.ContainsAny(value, "\r\n") {
				return proxy.Upstream{}, errors.New("could not issue local T3 dispatch controller session")
			}
			token, refreshAt = value, time.Now().Add(50*time.Minute)
		}
		return proxy.Upstream{Endpoint: provider.Endpoint{URL: origin}, Token: token}, nil
	}
	upstream, err := resolve(ready)
	if err != nil {
		return fail(err)
	}
	req, err := http.NewRequestWithContext(ready, http.MethodGet, origin+"/api/auth/session", nil)
	if err != nil {
		return fail(errors.New("invalid dispatch controller session endpoint"))
	}
	req.Header.Set("Authorization", "Bearer "+upstream.Token)
	response, err := client.Do(req)
	if err != nil {
		return fail(errors.New("local T3 dispatch controller authentication check failed"))
	}
	var session struct {
		Authenticated bool `json:"authenticated"`
	}
	decodeErr := json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&session)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK || decodeErr != nil || !session.Authenticated {
		return fail(errors.New("local T3 dispatch controller rejected the private session"))
	}
	fmt.Fprintln(diagnostics, "t3fleet local dispatch controller ready")
	return proxy.DispatchWorker{
		ID:      dispatchControllerID,
		Resolve: resolve,
		Descriptor: func(context.Context) (map[string]any, error) {
			select {
			case <-done:
				return nil, errors.New("local T3 dispatch controller is unavailable")
			default:
				return descriptor, nil
			}
		},
	}, cleanup, nil
}

func privateControllerDirectory(path string) error {
	if err := os.Mkdir(path, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return errors.New("cannot prepare private local dispatch controller directory")
	}
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
		return errors.New("unsafe local dispatch controller directory")
	}
	return nil
}

// T3 loads this settings file at startup. The controller belongs to t3fleet;
// only Linked Environments and the declared agents are managed here. Desktop
// lists a provider in the fleet settings only when the controller enables it,
// and T3 leaves Pi disabled by default.
func prepareControllerSettings(userdata string, drivers []string) error {
	const maxSettings = 4 << 20
	path := filepath.Join(userdata, "settings.json")
	settings := make(map[string]json.RawMessage)
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() || info.Size() > maxSettings {
			return errors.New("invalid local T3 dispatch controller settings file")
		}
		data, err := os.ReadFile(path)
		if err != nil || len(data) > maxSettings || json.Unmarshal(data, &settings) != nil || settings == nil {
			return errors.New("could not read local T3 dispatch controller settings")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return errors.New("could not find local T3 dispatch controller settings")
	}
	changed := false
	if string(settings["linkedEnvironmentsEnabled"]) != "true" {
		settings["linkedEnvironmentsEnabled"] = json.RawMessage("true")
		changed = true
	}
	instances := make(map[string]map[string]json.RawMessage)
	if raw := settings["providerInstances"]; len(raw) != 0 && string(raw) != "null" {
		if json.Unmarshal(raw, &instances) != nil || instances == nil {
			return errors.New("could not read local T3 dispatch controller provider settings")
		}
	}
	instancesChanged := false
	for _, driver := range drivers {
		encoded, _ := json.Marshal(driver)
		instance := instances[driver]
		if instance == nil || string(instance["driver"]) != string(encoded) {
			instance = map[string]json.RawMessage{"driver": encoded}
		}
		if string(instance["enabled"]) != "true" {
			instance["enabled"] = json.RawMessage("true")
			instances[driver] = instance
			instancesChanged = true
		}
	}
	if instancesChanged {
		raw, err := json.Marshal(instances)
		if err != nil {
			return errors.New("could not encode local T3 dispatch controller provider settings")
		}
		settings["providerInstances"] = raw
		changed = true
	}
	if !changed {
		return nil
	}
	data, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return errors.New("could not encode local T3 dispatch controller settings")
	}
	file, err := os.CreateTemp(userdata, ".settings-*")
	if err != nil {
		return errors.New("could not write local T3 dispatch controller settings")
	}
	defer func() { _ = os.Remove(file.Name()) }()
	_, writeErr := file.Write(append(data, '\n'))
	syncErr := file.Sync()
	closeErr := file.Close()
	if writeErr != nil || syncErr != nil || closeErr != nil || os.Rename(file.Name(), path) != nil {
		return errors.New("could not save local T3 dispatch controller settings")
	}
	return nil
}

func controllerEnvironment(home, base string) []string {
	// In particular, exclude cloud credentials, Git credential helpers from
	// the user's config, and environment options that can change T3's bind.
	env := []string{
		"HOME=" + home,
		"TMPDIR=" + filepath.Join(home, "tmp"),
		"T3CODE_HOME=" + base,
		"XDG_CONFIG_HOME=" + filepath.Join(home, ".config"),
		"XDG_CACHE_HOME=" + filepath.Join(home, ".cache"),
		"XDG_DATA_HOME=" + filepath.Join(home, ".local", "share"),
		"XDG_STATE_HOME=" + filepath.Join(home, ".local", "state"),
	}
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if name == "PATH" || name == "LANG" || strings.HasPrefix(name, "LC_") {
			env = append(env, entry)
		}
	}
	return env
}

func waitControllerDescriptor(ctx context.Context, done <-chan struct{}, client *http.Client, origin, version string) (map[string]any, error) {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, origin+"/.well-known/t3/environment", nil)
		if err != nil {
			return nil, errors.New("invalid local T3 dispatch controller endpoint")
		}
		response, err := client.Do(req)
		if err == nil {
			var descriptor map[string]any
			data, readErr := io.ReadAll(io.LimitReader(response.Body, (4<<20)+1))
			_ = response.Body.Close()
			if response.StatusCode == http.StatusOK && readErr == nil && len(data) <= 4<<20 && json.Unmarshal(data, &descriptor) == nil {
				if id, _ := descriptor["environmentId"].(string); id != "" {
					if descriptor["serverVersion"] != version || descriptor["orchestrationProtocolVersion"] != float64(2) {
						return nil, errors.New("local dispatch controller must match the configured T3 version and protocol 2")
					}
					return descriptor, nil
				}
			}
		}
		select {
		case <-ctx.Done():
			return nil, errors.New("local T3 dispatch controller did not become ready")
		case <-done:
			return nil, errors.New("local T3 dispatch controller exited before readiness")
		case <-ticker.C:
		}
	}
}
