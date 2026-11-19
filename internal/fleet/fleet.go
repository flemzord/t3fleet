// Package fleet coordinates durable machine lifecycle and stable T3 listeners.
package fleet

import (
	"cmp"
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
	"regexp"
	"slices"
	"strconv"
	"sync"
	"time"

	runtimebundle "github.com/flemzord/t3fleet"
	"github.com/flemzord/t3fleet/internal/proxy"
	"github.com/flemzord/t3fleet/internal/state"
	"github.com/flemzord/t3fleet/pkg/provider"
)

var ErrNotFound = errors.New("fleet machine not found")
var ErrClosed = errors.New("fleet is shutting down")
var ErrInvalid = errors.New("invalid fleet request")
var ErrConflict = errors.New("fleet machine already exists")
var ErrRuntimeUnavailable = errors.New("fleet: image does not provide t3fleet-runtime-info or its required executables; use a prepared t3fleet image")
var validName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

// Older daemons must not ignore the saved fallback operation ID and issue a
// second creation request during recovery.
const fleetSchema = 2

type Storage interface {
	Load(any) error
	Save(any) error
}
type Options struct {
	IdleTimeout       time.Duration
	IdleCheckInterval time.Duration
	Providers         map[string]provider.Provider
	Templates         map[string]provider.Template
	Sizes             map[string]string
	Store             Storage
	ExpectedVersion   string
	ListenHost        string
	ReadyTimeout      time.Duration
	Now               func() time.Time
}
type CreateRequest struct {
	Name     string            `json:"name"`
	Provider string            `json:"provider"`
	Size     string            `json:"size,omitempty"`
	Template provider.Template `json:"template"`
}
type Machine struct {
	LastStopReason string    `json:"lastStopReason,omitempty"`
	LastStoppedAt  time.Time `json:"lastStoppedAt,omitzero"`
	ID             string    `json:"id"`
	Name           string    `json:"name"`
	Provider       string    `json:"provider"`
	RemoteID       string    `json:"remoteId,omitempty"`
	State          string    `json:"state"`
	URL            string    `json:"url"`
}
type clientToken struct {
	Value     string    `json:"value"`
	ExpiresAt time.Time `json:"expiresAt"`
}
type record struct {
	Machine
	// Provider is the durable instance name; the driver must still match on restart.
	Driver          string              `json:"driver,omitempty"`
	Scope           string              `json:"scope,omitempty"`
	CreateOperation string              `json:"createOperation,omitempty"` // first attempt and old state use the machine ID
	Template        provider.Template   `json:"template"`
	Size            string              `json:"size,omitempty"`
	Placement       *provider.Placement `json:"placement,omitempty"`
	Shape           provider.Shape      `json:"shape,omitzero"`
	Port            int                 `json:"port"`
	Token           string              `json:"serviceToken,omitempty"`
	EnvironmentID   string              `json:"environmentId,omitempty"`
	Clients         []clientToken       `json:"clients,omitempty"`
	Descriptor      map[string]any      `json:"descriptor,omitempty"`
	Session         map[string]any      `json:"session,omitempty"`

	InstallRuntime    bool             `json:"installRuntime,omitempty"`
	FallbackPlan      []dispatchTarget `json:"fallbackPlan,omitzero"`
	DispatchFallbacks []string         `json:"dispatchFallbacks,omitzero"` // legacy instance names
}

func (r record) createRequest() provider.CreateRequest {
	op := cmp.Or(r.CreateOperation, r.ID)
	return provider.CreateRequest{
		Name: r.Name, Template: r.Template, Size: r.Size, Shape: r.Shape, Placement: r.Placement,
		OperationID: op, Labels: map[string]string{"t3fleet-id": op},
	}
}

type diskState struct {
	Schema  int                    `json:"schema"`
	Records []record               `json:"machines"`
	Batches map[string]batchRecord `json:"batches,omitempty"`
}
type machineHandle struct {
	activity    *proxy.Activity
	operation   sync.Mutex
	record      record // protected by Manager.mu
	proxy       *proxy.Proxy
	server      *http.Server
	listener    net.Listener
	initialized time.Time // independent service-authorization refresh clock

	// A failed save may have already replaced the on-disk provider selection.
	// Guarded by operation; reset only by reloading the store on restart.
	dispatchSwitchUncertain bool
}

func (h *machineHandle) closeListener() error {
	// Serve starts asynchronously. If it has not registered the listener yet,
	// closing the HTTP server does not release the machine's port.
	err := h.listener.Close()
	if errors.Is(err, net.ErrClosed) {
		return nil
	}
	return err
}

func (h *machineHandle) close() error {
	err := h.server.Close()
	return errors.Join(err, h.closeListener())
}

type Manager struct {
	mu       sync.RWMutex
	opts     Options
	machines map[string]*machineHandle
	batches  map[string]batchRecord
	ctx      context.Context
	cancel   context.CancelFunc
	closed   bool
	idleWG   sync.WaitGroup
}

func New(opts Options) (*Manager, error) {
	if opts.Store == nil {
		return nil, errors.New("fleet: a durable store is required")
	}
	if opts.ListenHost == "" {
		opts.ListenHost = "127.0.0.1"
	}
	if opts.ListenHost != "127.0.0.1" && opts.ListenHost != "::1" {
		return nil, errors.New("fleet: machine listeners must use loopback")
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.ReadyTimeout == 0 {
		opts.ReadyTimeout = 3 * time.Minute
	}
	if opts.ReadyTimeout < time.Second {
		return nil, errors.New("fleet: readiness timeout must be at least one second")
	}
	if opts.IdleTimeout < 0 || opts.IdleTimeout > 0 && opts.IdleTimeout < time.Minute {
		return nil, errors.New("fleet: idle timeout must be zero or at least one minute")
	}
	if opts.IdleCheckInterval == 0 {
		opts.IdleCheckInterval = 15 * time.Second
	}
	if opts.IdleCheckInterval < time.Millisecond {
		return nil, errors.New("fleet: idle check interval is too short")
	}
	ctx, cancel := context.WithCancel(context.Background())
	m := &Manager{opts: opts, machines: map[string]*machineHandle{}, ctx: ctx, cancel: cancel}
	var disk diskState
	if err := opts.Store.Load(&disk); err != nil && !errors.Is(err, state.ErrEmpty) {
		cancel()
		return nil, fmt.Errorf("fleet: load state: %w", err)
	}
	if disk.Schema != 0 && disk.Schema != 1 && disk.Schema != fleetSchema {
		cancel()
		return nil, errors.New("fleet: unsupported state schema")
	}
	m.batches = disk.Batches
	if m.batches == nil {
		m.batches = make(map[string]batchRecord)
	}
	migrated := false
	ids := make(map[string]bool, len(disk.Records))
	for i := range disk.Records {
		r := &disk.Records[i]
		if !validName.MatchString(r.Name) || len(r.ID) != 32 || r.Port < 1 || r.Port > 65535 {
			cancel()
			return nil, errors.New("fleet: invalid saved machine")
		}
		if ids[r.ID] {
			cancel()
			return nil, errors.New("fleet: duplicate saved machine")
		}
		ids[r.ID] = true
		host := opts.Providers[r.Provider]
		// Before instances were named, the provider key was the driver.
		if r.Driver == "" {
			r.Driver = r.Provider
		}
		if host == nil || host.Name() != r.Driver {
			cancel()
			return nil, fmt.Errorf("fleet: saved worker requires provider %q with driver %q", r.Provider, r.Driver)
		}
		scope := providerScope(host)
		if r.Scope != "" && r.Scope != scope {
			cancel()
			return nil, fmt.Errorf("fleet: restore the original account and endpoint of provider %q before managing its saved workers", r.Provider)
		}
		// Earlier records lack an account pin. Verify an existing remote before
		// adopting this account; a 404 could mean the operator selected another.
		if r.Scope == "" && scope != "" {
			if err := verifyLegacyAccount(ctx, host, r); err != nil {
				cancel()
				return nil, err
			}
			r.Scope = scope
			migrated = true
		}
		if r.Placement == nil {
			r.Placement = providerPlacement(host)
			migrated = migrated || r.Placement != nil
		}
	}
	if migrated {
		disk.Schema = fleetSchema
		if err := opts.Store.Save(disk); err != nil {
			cancel()
			return nil, fmt.Errorf("fleet: cannot save migrated worker reservations: %w", err)
		}
	}
	for _, r := range disk.Records {
		h, err := m.listen(r)
		if err != nil {
			_ = m.Close(context.Background())
			return nil, err
		}
		m.mu.Lock()
		m.machines[r.ID] = h
		m.mu.Unlock()
	}
	if opts.IdleTimeout > 0 {
		m.idleWG.Go(m.idleLoop)
	}
	return m, nil
}

// No create, start or cleanup is safe until the original account is known.
func verifyLegacyAccount(ctx context.Context, host provider.Provider, r *record) error {
	if r.RemoteID == "" {
		if r.State == "planned" {
			return nil // no allocation was attempted
		}
		return fmt.Errorf("fleet: cannot verify original account for worker %q with no remote ID; reconcile it with the previous daemon and configuration before upgrading", r.Name)
	}
	lookup, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	remote, err := host.Get(lookup, r.RemoteID)
	if err != nil || remote.ID != r.RemoteID || remote.Name != r.Name || remote.State == provider.StateDeleted {
		return fmt.Errorf("fleet: cannot verify worker %q in its original provider account; restore the original configuration or reconcile the worker with the previous daemon before upgrading", r.Name)
	}
	return nil
}

func (m *Manager) listen(r record) (*machineHandle, error) {
	listener, err := net.Listen("tcp", net.JoinHostPort(m.opts.ListenHost, strconv.Itoa(r.Port)))
	if err != nil {
		return nil, errors.New("fleet: cannot bind machine port; stop the conflicting listener and retry")
	}
	r.Port = listener.Addr().(*net.TCPAddr).Port
	r.URL = "http://" + net.JoinHostPort(m.opts.ListenHost, strconv.Itoa(r.Port))
	h := &machineHandle{record: r, listener: listener}
	if m.opts.IdleTimeout > 0 {
		h.activity = proxy.NewActivity(m.opts.Now, r.State == "stopped")
	}
	p, err := proxy.New(proxy.Config{
		Label: r.Name, Origin: r.URL, Activity: h.activity,
		Session:        func(context.Context) (map[string]any, error) { return m.copy(h).Session, nil },
		Descriptor:     func(ctx context.Context) (map[string]any, error) { return m.descriptor(ctx, r.ID) },
		Resolve:        func(ctx context.Context) (proxy.Upstream, error) { return m.resolve(ctx, r.ID) },
		Authorize:      func(token string) bool { return m.authorize(r.ID, token) },
		SaveToken:      func(ctx context.Context, token string) error { return m.saveClient(ctx, r.ID, token) },
		RequestTimeout: m.opts.ReadyTimeout,
	})
	if err != nil {
		_ = listener.Close()
		return nil, err
	}
	h.proxy = p
	h.server = &http.Server{Handler: p, ReadHeaderTimeout: 5 * time.Second, MaxHeaderBytes: 64 << 10, BaseContext: func(net.Listener) context.Context { return m.ctx }}
	go func() { _ = h.server.Serve(listener) }()
	return h, nil
}

// persistLocked serializes a complete snapshot while holding mu. No caller can
// observe a half-applied state mutation or interleave durable writes.
func (m *Manager) persistLocked() error {
	disk := diskState{Schema: fleetSchema, Batches: m.batches, Records: make([]record, 0, len(m.machines))}
	for _, h := range m.machines {
		disk.Records = append(disk.Records, h.record)
	}
	slices.SortFunc(disk.Records, func(a, b record) int { return cmp.Compare(a.ID, b.ID) })
	if err := m.opts.Store.Save(disk); err != nil {
		return fmt.Errorf("fleet: persist state: %w", err)
	}
	return nil
}
func (m *Manager) handle(id string) (*machineHandle, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.closed {
		return nil, ErrClosed
	}
	h := m.machines[id]
	if h == nil {
		return nil, ErrNotFound
	}
	return h, nil
}
func (m *Manager) copy(h *machineHandle) record {
	m.mu.RLock()
	defer m.mu.RUnlock()
	r := h.record
	r.Clients = slices.Clone(r.Clients)
	return r
}
func (m *Manager) update(h *machineHandle, fn func(*record)) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.updateLocked(h, fn)
}

func (m *Manager) updateLocked(h *machineHandle, fn func(*record)) error {
	old := h.record
	fn(&h.record)
	if err := m.persistLocked(); err != nil {
		h.record = old
		return err
	}
	return nil
}
func (m *Manager) List() []Machine {
	m.mu.RLock()
	defer m.mu.RUnlock()
	result := make([]Machine, 0, len(m.machines))
	for _, h := range m.machines {
		result = append(result, h.record.Machine)
	}
	slices.SortFunc(result, func(a, b Machine) int { return cmp.Compare(a.Name, b.Name) })
	return result
}
func (m *Manager) Get(id string) (Machine, error) {
	h, err := m.handle(id)
	if err != nil {
		return Machine{}, err
	}
	return m.copy(h).Machine, nil
}

func (m *Manager) applyProviderDefaults(req *CreateRequest) (provider.Shape, error) {
	if req.Template == (provider.Template{}) {
		req.Template = m.opts.Templates[req.Provider]
	}
	if req.Size == "" {
		req.Size = m.opts.Sizes[req.Provider]
	}
	if resolver, ok := m.opts.Providers[req.Provider].(provider.ShapeResolver); ok {
		return resolver.ResolveShape(req.Size)
	}
	return provider.Shape{}, nil
}

func providerPlacement(host provider.Provider) *provider.Placement {
	if placed, ok := host.(provider.PlacementProvider); ok {
		placement := placed.DefaultPlacement()
		return &placement
	}
	return nil
}

func providerScope(host provider.Provider) string {
	if scoped, ok := host.(provider.ScopeIdentifier); ok {
		return scoped.ScopeID()
	}
	return ""
}

func (m *Manager) Create(ctx context.Context, req CreateRequest) (Machine, error) {
	if !validName.MatchString(req.Name) {
		return Machine{}, fmt.Errorf("%w: name must match [a-z][a-z0-9-]{0,31}", ErrInvalid)
	}
	req.Name = managedName(req.Name)
	if !validName.MatchString(req.Name) {
		return Machine{}, fmt.Errorf("%w: name including t3fleet- must fit 32 characters", ErrInvalid)
	}
	p := m.opts.Providers[req.Provider]
	if p == nil {
		return Machine{}, fmt.Errorf("%w: unknown provider", ErrInvalid)
	}
	shape, err := m.applyProviderDefaults(&req)
	if err != nil {
		return Machine{}, fmt.Errorf("%w: invalid machine shape", ErrInvalid)
	}
	if req.Template == (provider.Template{}) {
		return Machine{}, fmt.Errorf("%w: a prepared image or snapshot is required", ErrInvalid)
	}
	idBytes := make([]byte, 16)
	if _, err := rand.Read(idBytes); err != nil {
		return Machine{}, errors.New("fleet: cannot generate machine identity")
	}
	id := hex.EncodeToString(idBytes)
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return Machine{}, ErrClosed
	}
	for _, existing := range m.machines {
		if existing.record.Name == req.Name {
			machine := existing.record.Machine
			m.mu.Unlock()
			return machine, ErrConflict
		}
	}
	intent := record{Machine: Machine{ID: id, Name: req.Name, Provider: req.Provider, State: "creating"}, Driver: p.Name(), Scope: providerScope(p), Template: req.Template, Size: req.Size, Shape: shape, Placement: providerPlacement(p)}
	h, err := m.listen(intent)
	if err != nil {
		m.mu.Unlock()
		return Machine{}, err
	}
	m.machines[id] = h
	err = m.persistLocked()
	if err != nil {
		delete(m.machines, id)
		_ = h.close()
		m.mu.Unlock()
		return Machine{}, err
	}
	m.mu.Unlock()
	h.operation.Lock()
	defer h.operation.Unlock()
	remote, createErr := p.Create(ctx, intent.createRequest())
	// A provider may allocate successfully and then fail activation. Retain
	// that identity before reporting the error so recovery and deletion do not
	// depend on replaying a billable creation request.
	if remote.ID != "" {
		if err := m.update(h, func(r *record) { r.RemoteID = remote.ID; r.State = "starting" }); err != nil {
			return m.copy(h).Machine, err
		}
	}
	if createErr != nil {
		_ = m.update(h, func(r *record) { r.State = "create-failed" })
		return m.copy(h).Machine, errors.New("fleet: provider creation failed; inspect the saved machine before retrying")
	}
	if err = m.initialize(ctx, h, p); err != nil {
		_ = m.update(h, func(r *record) { r.State = "not-ready" })
		return m.copy(h).Machine, err
	}
	if h.activity != nil {
		h.activity.Awake()
	}
	return m.copy(h).Machine, nil
}

// initialize runs the first-boot hook. A Namespace dispatch reservation may
// also install the pinned runtime bundled with the daemon on its base image.
func (m *Manager) initialize(ctx context.Context, h *machineHandle, p provider.Provider) error {
	ctx, cancel := context.WithTimeout(ctx, m.opts.ReadyTimeout)
	defer cancel()
	r := m.copy(h)
	if err := waitRunning(ctx, p, r.RemoteID); err != nil {
		return err
	}
	args := []string{"/bin/bash", "-lc", "exec t3fleet-runtime-info"}
	if r.InstallRuntime {
		args = append([]string{"/bin/bash", "-lc", dispatchRuntimeScript, "t3fleet-runtime"}, runtimebundle.Sources()...)
	}
	result, err := p.Exec(ctx, r.RemoteID, provider.ExecRequest{Args: args})
	if err != nil {
		return errors.New("fleet: private runtime execution failed; inspect provider connectivity")
	}
	if result.ExitCode == 127 {
		return ErrRuntimeUnavailable
	}
	if result.ExitCode != 0 {
		if r.InstallRuntime {
			return fmt.Errorf("fleet: Namespace runtime bootstrap or initialization exited %d", result.ExitCode)
		}
		return fmt.Errorf("fleet: image runtime initialization exited %d; use a prepared t3fleet image", result.ExitCode)
	}
	var info struct {
		Token         string `json:"token"`
		EnvironmentID string `json:"environmentId"`
		ServerVersion string `json:"serverVersion"`
	}
	if len(result.Stdout) > 64<<10 || json.Unmarshal([]byte(result.Stdout), &info) != nil || info.Token == "" || info.EnvironmentID == "" {
		return errors.New("fleet: invalid image runtime response")
	}
	if m.opts.ExpectedVersion != "" && info.ServerVersion != m.opts.ExpectedVersion {
		return errors.New("fleet: T3 server version differs from configured desktop version; rebuild the golden image")
	}
	endpoint, err := p.Connect(ctx, r.RemoteID, 3773)
	if err != nil {
		return errors.New("fleet: private T3 connection failed")
	}
	defer closeEndpoint(endpoint)
	client, base, err := endpointClient(endpoint)
	if err != nil {
		return err
	}
	defer client.CloseIdleConnections()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base, nil)
	if err != nil {
		return errors.New("fleet: invalid private T3 endpoint")
	}
	// Query credentials belong to the provider URL, not an appended path.
	applyEndpoint(req, endpoint, "/.well-known/t3/environment")
	req.Header.Set("Authorization", "Bearer "+info.Token)
	response, err := client.Do(req)
	if err != nil {
		return errors.New("fleet: T3 readiness request failed")
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return errors.New("fleet: T3 endpoint did not become ready")
	}
	payload, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	var descriptor map[string]any
	if err != nil || len(payload) > 1<<20 || json.Unmarshal(payload, &descriptor) != nil || descriptor["environmentId"] != info.EnvironmentID || descriptor["serverVersion"] != info.ServerVersion {
		return errors.New("fleet: T3 descriptor does not match this machine's runtime identity and version")
	}
	delete(descriptor, "directEndpoints")
	authRequest, err := http.NewRequestWithContext(ctx, http.MethodGet, base, nil)
	if err != nil {
		return errors.New("fleet: invalid private T3 endpoint")
	}
	applyEndpoint(authRequest, endpoint, "/api/auth/session")
	authRequest.Header.Set("Authorization", "Bearer "+info.Token)
	authResponse, err := client.Do(authRequest)
	if err != nil {
		return errors.New("fleet: T3 service authorization check failed")
	}
	defer func() { _ = authResponse.Body.Close() }()
	if authResponse.StatusCode != http.StatusOK {
		return errors.New("fleet: T3 service authorization was rejected")
	}
	var session map[string]any
	sessionBytes, readErr := io.ReadAll(io.LimitReader(authResponse.Body, (1<<20)+1))
	if readErr != nil || len(sessionBytes) > 1<<20 || json.Unmarshal(sessionBytes, &session) != nil || session == nil {
		return errors.New("fleet: invalid T3 service session")
	}
	// Only authorization metadata is cached, never a remote token or arbitrary response fields.
	cachedSession := map[string]any{"scopes": session["scopes"], "permissions": session["permissions"]}
	// A reused authenticated backup is not a safe golden template. Check under
	// the persistence lock so concurrent workers cannot commit a shared identity.
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, other := range m.machines {
		if other != h && (other.record.Token == info.Token || other.record.EnvironmentID == info.EnvironmentID) {
			return errors.New("fleet: runtime identity or service credential is shared with another managed machine; use a credential-free golden template")
		}
	}
	h.initialized = m.opts.Now()
	return m.updateLocked(h, func(r *record) {
		r.Session = cachedSession
		r.Token = info.Token
		r.EnvironmentID = info.EnvironmentID
		r.Descriptor = descriptor
		r.State = "ready"
	})
}

// Cloud creation can return an allocated identity before command execution is
// possible. Readiness belongs to the manager, rather than each caller guessing
// whether an allocated instance can already execute its first-boot hook.
func waitRunning(ctx context.Context, p provider.Provider, id string) error {
	for {
		remote, err := p.Get(ctx, id)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return errors.New("fleet: provider readiness check failed")
		}
		switch remote.State {
		case provider.StateRunning:
			return nil
		case provider.StateStopped:
			if err := p.Start(ctx, id); err != nil {
				return errors.New("fleet: provider start failed")
			}
		case provider.StateStarting:
			// Wait for the provider to finish restoring the image.
		default:
			return errors.New("fleet: remote machine cannot become ready")
		}
		timer := time.NewTimer(200 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (m *Manager) Start(ctx context.Context, id string) error {
	h, err := m.handle(id)
	if err != nil {
		return err
	}
	h.operation.Lock()
	defer h.operation.Unlock()
	// A planned intent can be deleted while this caller waits for its lock.
	// Never allocate through a stale handle after its durable record was removed.
	if current, err := m.handle(id); err != nil || current != h {
		return ErrNotFound
	}
	err = m.ensure(ctx, h, true)
	if errors.Is(err, errDispatchSwitchUncertain) {
		return err
	}
	if err != nil {
		_ = m.update(h, func(r *record) {
			if r.State == "planned" {
				return
			}
			if r.RemoteID == "" {
				r.State = "create-failed"
			} else {
				r.State = "not-ready"
			}
		})
	}
	if err == nil && h.activity != nil {
		h.activity.Awake()
	}
	return err
}
func (m *Manager) ensure(ctx context.Context, h *machineHandle, force bool) error {
	if h.dispatchSwitchUncertain {
		return errDispatchSwitchUncertain
	}
	r := m.copy(h)
	p := m.opts.Providers[r.Provider]
	if r.RemoteID == "" {
		if err := ctx.Err(); err != nil {
			return err
		}
		if r.State == "planned" {
			if err := m.update(h, func(r *record) { r.State = "creating" }); err != nil {
				return err
			}
		}
		remote, createErr := p.Create(ctx, r.createRequest())
		if remote.ID != "" {
			if err := m.update(h, func(r *record) { r.RemoteID = remote.ID; r.State = "starting" }); err != nil {
				return err
			}
		}
		if createErr != nil {
			if remote.ID == "" && errors.Is(createErr, provider.ErrCreateRejected) {
				return provider.ErrCreateRejected
			}
			return errors.New("fleet: provider creation reconciliation failed; saved operation retained")
		}
		return m.initialize(ctx, h, p)
	}
	// Local readiness does not prove the provider has kept the VM awake.
	// An explicit thread request must also recover an out-of-band suspension.
	remote, err := p.Get(ctx, r.RemoteID)
	if err != nil {
		return errors.New("fleet: provider status check failed")
	}
	if remote.State == provider.StateStopped {
		if err = p.Start(ctx, r.RemoteID); err != nil {
			return errors.New("fleet: provider start failed")
		}
		force = true
	}
	if remote.State == provider.StateDeleted {
		return errors.New("fleet: remote machine was deleted")
	}
	if force || r.Token == "" || r.State != "ready" || m.opts.Now().Sub(h.initialized) > 10*time.Minute {
		return m.initialize(ctx, h, p)
	}
	return nil
}
func (m *Manager) resolve(ctx context.Context, id string) (proxy.Upstream, error) {
	h, err := m.handle(id)
	if err != nil {
		return proxy.Upstream{}, err
	}
	h.operation.Lock()
	defer h.operation.Unlock()
	wasSleeping := h.activity != nil && h.activity.Sleeping()
	if err = m.ensure(ctx, h, false); err != nil {
		return proxy.Upstream{}, err
	}
	r := m.copy(h)
	endpoint, err := m.opts.Providers[r.Provider].Connect(ctx, r.RemoteID, 3773)
	if err != nil {
		return proxy.Upstream{}, errors.New("fleet: private T3 connection failed")
	}
	if wasSleeping {
		h.activity.Awake()
	}
	return proxy.Upstream{Endpoint: endpoint, Token: r.Token}, nil
}

// Background shell reads must not call ensure, Exec, or the normal Connect:
// even opening a provider transport can implicitly activate a suspended VM.
func (m *Manager) resolvePassive(ctx context.Context, id string) (proxy.Upstream, error) {
	h, err := m.handle(id)
	if err != nil {
		return proxy.Upstream{}, err
	}
	if !h.operation.TryLock() {
		return proxy.Upstream{}, errors.New("worker is busy")
	}
	defer h.operation.Unlock()
	if current, err := m.handle(id); err != nil || current != h {
		return proxy.Upstream{}, ErrNotFound
	}
	r := m.copy(h)
	if r.State != "ready" || r.Token == "" || r.RemoteID == "" {
		return proxy.Upstream{}, errors.New("worker is not ready for passive reads")
	}
	host := m.opts.Providers[r.Provider]
	passive, ok := host.(provider.PassiveConnector)
	if !ok {
		return proxy.Upstream{}, provider.ErrUnsupported
	}
	// Do not trust the last local state: a provider can suspend a worker
	// out of band. Only the passive connector may bridge the check/dial race.
	remote, err := host.Get(ctx, r.RemoteID)
	if err != nil || remote.State != provider.StateRunning {
		return proxy.Upstream{}, errors.New("running worker could not be confirmed")
	}
	endpoint, err := passive.ConnectPassive(ctx, r.RemoteID, 3773)
	if err != nil {
		return proxy.Upstream{}, errors.New("passive worker connection unavailable")
	}
	return proxy.Upstream{Endpoint: endpoint, Token: r.Token}, nil
}

// DispatchWorker reuses the normal fleet wake and private transport paths.
// A reserved worker is routable once its T3 server has completed initialization.
func (m *Manager) DispatchWorker(id string) (proxy.DispatchWorker, error) {
	h, err := m.handle(id)
	if err != nil {
		return proxy.DispatchWorker{}, err
	}
	if r := m.copy(h); r.Token == "" || r.RemoteID == "" {
		return proxy.DispatchWorker{}, errors.New("dispatch worker has not completed initialization")
	}
	return proxy.DispatchWorker{ID: id,
		Resolve:        func(ctx context.Context) (proxy.Upstream, error) { return m.resolve(ctx, id) },
		ResolvePassive: func(ctx context.Context) (proxy.Upstream, error) { return m.resolvePassive(ctx, id) },
		Descriptor:     func(ctx context.Context) (map[string]any, error) { return m.descriptor(ctx, id) },
	}, nil
}
func (m *Manager) Stop(ctx context.Context, id string) error {
	h, err := m.handle(id)
	if err != nil {
		return err
	}
	h.operation.Lock()
	defer h.operation.Unlock()
	err = m.stopLocked(ctx, h, "manual")
	if err == nil && h.activity != nil {
		h.activity.Suspend()
	}
	return err
}

func (m *Manager) stopLocked(ctx context.Context, h *machineHandle, reason string) error {
	var err error
	r := m.copy(h)
	if r.RemoteID == "" {
		return errors.New("fleet: incomplete creation has no remote machine to stop")
	}
	if err = m.opts.Providers[r.Provider].Stop(ctx, r.RemoteID); err != nil {
		return errors.New("fleet: provider stop failed")
	}
	wait, cancel := context.WithTimeout(ctx, m.opts.ReadyTimeout)
	defer cancel()
	for {
		remote, err := m.opts.Providers[r.Provider].Get(wait, r.RemoteID)
		if err != nil {
			return errors.New("fleet: provider stop could not be confirmed")
		}
		if remote.State == provider.StateStopped {
			break
		}
		timer := time.NewTimer(200 * time.Millisecond)
		select {
		case <-wait.Done():
			timer.Stop()
			return wait.Err()
		case <-timer.C:
		}
	}
	return m.update(h, func(r *record) {
		r.State = "stopped"
		r.LastStopReason = reason
		r.LastStoppedAt = m.opts.Now()
	})
}
func (m *Manager) Delete(ctx context.Context, id string) error {
	return m.deleteMachine(ctx, id, false)
}

func (m *Manager) deleteMachine(ctx context.Context, id string, reconcileAbsence bool) error {
	h, err := m.handle(id)
	if err != nil {
		return err
	}
	h.operation.Lock()
	defer h.operation.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if current, err := m.handle(id); err != nil || current != h {
		return ErrNotFound
	}
	r := m.copy(h)
	if r.RemoteID == "" && r.State != "planned" {
		return errors.New("fleet: creation is unresolved; run start to reconcile before deleting")
	}
	if r.RemoteID != "" {
		host := m.opts.Providers[r.Provider]
		var absent bool
		if reconcileAbsence {
			// A previous attempt may remove the machine before its
			// operation is acknowledged. Verify absence before retrying
			// deletion so the dispatch slot cannot get stranded.
			remote, lookupErr := host.Get(ctx, r.RemoteID)
			absent = errors.Is(lookupErr, provider.ErrNotFound) || (lookupErr == nil && remote.State == provider.StateDeleted)
			if !absent && lookupErr != nil {
				return errors.New("fleet: provider absence could not be checked; local state retained")
			}
		}
		if !absent {
			if err = host.Delete(ctx, r.RemoteID); err != nil && !errors.Is(err, provider.ErrNotFound) {
				return errors.New("fleet: provider deletion failed; local state retained")
			}
		}
	}
	return m.removeMachineRecord(h)
}

// Caller owns h.operation. Preserve the batch member as a durable tombstone,
// without making another provider request.
func (m *Manager) removeMachineRecord(h *machineHandle) error {
	m.mu.Lock()
	id := h.record.ID
	if m.machines[id] != h {
		m.mu.Unlock()
		return ErrNotFound
	}
	delete(m.machines, id)
	err := m.persistLocked()
	if err != nil {
		m.machines[id] = h
	}
	m.mu.Unlock()
	if err != nil {
		return err
	}
	return h.close()
}
func (m *Manager) Snapshot(ctx context.Context, id, name string) (provider.Snapshot, error) {
	h, err := m.handle(id)
	if err != nil {
		return provider.Snapshot{}, err
	}
	h.operation.Lock()
	defer h.operation.Unlock()
	r := m.copy(h)
	snapshotter, ok := m.opts.Providers[r.Provider].(provider.Snapshotter)
	if !ok {
		return provider.Snapshot{}, provider.ErrUnsupported
	}
	// Capturing a live machine includes its identity and logins. Golden images
	// must be built separately; this explicit operation is a private backup.
	return snapshotter.Snapshot(ctx, r.RemoteID, name)
}
func (m *Manager) Pair(ctx context.Context, id string) (proxy.Pairing, error) {
	if err := m.Start(ctx, id); err != nil {
		return proxy.Pairing{}, err
	}
	h, err := m.handle(id)
	if err != nil {
		return proxy.Pairing{}, err
	}
	return h.proxy.NewPairing()
}
func (m *Manager) descriptor(ctx context.Context, id string) (map[string]any, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	h, err := m.handle(id)
	if err != nil {
		return nil, err
	}
	m.mu.RLock()
	cached := h.record.Descriptor
	m.mu.RUnlock()
	if cached == nil {
		return nil, errors.New("fleet: no verified descriptor; start the machine first")
	}
	// Values in the cached descriptor are immutable until the next replacement.
	copy := make(map[string]any, len(cached))
	for key, value := range cached {
		copy[key] = value
	}
	return copy, nil
}

func (m *Manager) authorize(id, token string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	h := m.machines[id]
	if h == nil || m.closed {
		return false
	}
	for _, entry := range h.record.Clients {
		if time.Now().Before(entry.ExpiresAt) && subtle.ConstantTimeCompare([]byte(entry.Value), []byte(token)) == 1 {
			return true
		}
	}
	return false
}
func (m *Manager) saveClient(ctx context.Context, id, token string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	h, err := m.handle(id)
	if err != nil {
		return err
	}
	return m.update(h, func(r *record) {
		now := time.Now()
		r.Clients = slices.DeleteFunc(slices.Clone(r.Clients), func(t clientToken) bool { return !now.Before(t.ExpiresAt) })
		r.Clients = append(r.Clients, clientToken{Value: token, ExpiresAt: now.Add(365 * 24 * time.Hour)})
	})
}
func (m *Manager) Close(ctx context.Context) error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	m.cancel()
	handles := make([]*machineHandle, 0, len(m.machines))
	for _, h := range m.machines {
		handles = append(handles, h)
	}
	m.mu.Unlock()
	m.idleWG.Wait()
	var result error
	for _, h := range handles {
		err := h.server.Shutdown(ctx)
		if err != nil {
			_ = h.server.Close()
		}
		result = errors.Join(result, err, h.closeListener())
	}
	return result
}
