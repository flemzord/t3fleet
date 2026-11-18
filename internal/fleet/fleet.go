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

type Storage interface {
	Load(any) error
	Save(any) error
}
type Options struct {
	Providers       map[string]provider.Provider
	Templates       map[string]provider.Template
	Store           Storage
	ExpectedVersion string
	ListenHost      string
	ReadyTimeout    time.Duration
	Now             func() time.Time
}
type CreateRequest struct {
	Name     string            `json:"name"`
	Provider string            `json:"provider"`
	Size     string            `json:"size,omitempty"`
	Template provider.Template `json:"template"`
}
type Machine struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Provider string `json:"provider"`
	RemoteID string `json:"remoteId,omitempty"`
	State    string `json:"state"`
	URL      string `json:"url"`
}
type clientToken struct {
	Value     string    `json:"value"`
	ExpiresAt time.Time `json:"expiresAt"`
}
type record struct {
	Machine
	Template      provider.Template `json:"template"`
	Size          string            `json:"size,omitempty"`
	Port          int               `json:"port"`
	Token         string            `json:"serviceToken,omitempty"`
	EnvironmentID string            `json:"environmentId,omitempty"`
	Clients       []clientToken     `json:"clients,omitempty"`
	Descriptor    map[string]any    `json:"descriptor,omitempty"`
}
type diskState struct {
	Schema  int                    `json:"schema"`
	Records []record               `json:"machines"`
	Batches map[string]batchRecord `json:"batches,omitempty"`
}
type machineHandle struct {
	operation   sync.Mutex
	record      record // protected by Manager.mu
	proxy       *proxy.Proxy
	server      *http.Server
	listener    net.Listener
	checked     time.Time // protected by operation
	initialized time.Time // independent service-authorization refresh clock
}
type Manager struct {
	mu       sync.RWMutex
	opts     Options
	machines map[string]*machineHandle
	batches  map[string]batchRecord
	ctx      context.Context
	cancel   context.CancelFunc
	closed   bool
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
	ctx, cancel := context.WithCancel(context.Background())
	m := &Manager{opts: opts, machines: map[string]*machineHandle{}, ctx: ctx, cancel: cancel}
	var disk diskState
	if err := opts.Store.Load(&disk); err != nil && !errors.Is(err, state.ErrEmpty) {
		cancel()
		return nil, fmt.Errorf("fleet: load state: %w", err)
	}
	if disk.Schema != 0 && disk.Schema != 1 {
		cancel()
		return nil, errors.New("fleet: unsupported state schema")
	}
	m.batches = disk.Batches
	if m.batches == nil {
		m.batches = make(map[string]batchRecord)
	}
	for _, r := range disk.Records {
		if !validName.MatchString(r.Name) || len(r.ID) != 32 || opts.Providers[r.Provider] == nil || r.Port < 1 || r.Port > 65535 {
			_ = m.Close(context.Background())
			return nil, errors.New("fleet: invalid saved machine or missing provider")
		}
		if _, ok := m.machines[r.ID]; ok {
			_ = m.Close(context.Background())
			return nil, errors.New("fleet: duplicate saved machine")
		}
		h, err := m.listen(r)
		if err != nil {
			_ = m.Close(context.Background())
			return nil, err
		}
		m.mu.Lock()
		m.machines[r.ID] = h
		m.mu.Unlock()
	}
	return m, nil
}

func (m *Manager) listen(r record) (*machineHandle, error) {
	listener, err := net.Listen("tcp", net.JoinHostPort(m.opts.ListenHost, strconv.Itoa(r.Port)))
	if err != nil {
		return nil, errors.New("fleet: cannot bind machine port; stop the conflicting listener and retry")
	}
	r.Port = listener.Addr().(*net.TCPAddr).Port
	r.URL = "http://" + net.JoinHostPort(m.opts.ListenHost, strconv.Itoa(r.Port))
	h := &machineHandle{record: r, listener: listener}
	p, err := proxy.New(proxy.Config{
		Label: r.Name, Origin: r.URL,
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
	disk := diskState{Schema: 1, Batches: m.batches, Records: make([]record, 0, len(m.machines))}
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

func (m *Manager) Create(ctx context.Context, req CreateRequest) (Machine, error) {
	if !validName.MatchString(req.Name) {
		return Machine{}, fmt.Errorf("%w: name must match [a-z][a-z0-9-]{0,31}", ErrInvalid)
	}
	p := m.opts.Providers[req.Provider]
	if p == nil {
		return Machine{}, fmt.Errorf("%w: unknown provider", ErrInvalid)
	}
	if req.Template == (provider.Template{}) {
		req.Template = m.opts.Templates[req.Provider]
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
	h, err := m.listen(record{Machine: Machine{ID: id, Name: req.Name, Provider: req.Provider, State: "creating"}, Template: req.Template, Size: req.Size})
	if err != nil {
		m.mu.Unlock()
		return Machine{}, err
	}
	m.machines[id] = h
	err = m.persistLocked()
	if err != nil {
		delete(m.machines, id)
		_ = h.server.Close()
		m.mu.Unlock()
		return Machine{}, err
	}
	m.mu.Unlock()
	h.operation.Lock()
	defer h.operation.Unlock()
	remote, createErr := p.Create(ctx, provider.CreateRequest{Name: req.Name, Template: req.Template, Size: req.Size, OperationID: id, Labels: map[string]string{"t3fleet-id": id}})
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
	return m.copy(h).Machine, nil
}

// initialize executes an already installed first-boot hook; it never installs
// code or copies a user's local model credentials into the machine.
func (m *Manager) initialize(ctx context.Context, h *machineHandle, p provider.Provider) error {
	ctx, cancel := context.WithTimeout(ctx, m.opts.ReadyTimeout)
	defer cancel()
	r := m.copy(h)
	if err := waitRunning(ctx, p, r.RemoteID); err != nil {
		return err
	}
	result, err := p.Exec(ctx, r.RemoteID, provider.ExecRequest{Args: []string{"/bin/bash", "-lc", "exec t3fleet-runtime-info"}})
	if err != nil {
		return errors.New("fleet: private runtime execution failed; inspect provider connectivity")
	}
	if result.ExitCode == 127 {
		return ErrRuntimeUnavailable
	}
	if result.ExitCode != 0 {
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
	defer response.Body.Close()
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
	defer authResponse.Body.Close()
	if authResponse.StatusCode != http.StatusOK {
		return errors.New("fleet: T3 service authorization was rejected")
	}
	// A reused authenticated backup is not a safe golden template. Check under
	// the persistence lock so concurrent workers cannot commit a shared identity.
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, other := range m.machines {
		if other != h && (other.record.Token == info.Token || other.record.EnvironmentID == info.EnvironmentID) {
			return errors.New("fleet: runtime identity or service credential is shared with another managed machine; use a credential-free golden template")
		}
	}
	h.checked = m.opts.Now()
	h.initialized = m.opts.Now()
	return m.updateLocked(h, func(r *record) {
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
	return err
}
func (m *Manager) ensure(ctx context.Context, h *machineHandle, force bool) error {
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
		remote, createErr := p.Create(ctx, provider.CreateRequest{Name: r.Name, Template: r.Template, Size: r.Size, OperationID: r.ID, Labels: map[string]string{"t3fleet-id": r.ID}})
		if remote.ID != "" {
			if err := m.update(h, func(r *record) { r.RemoteID = remote.ID; r.State = "starting" }); err != nil {
				return err
			}
		}
		if createErr != nil {
			return errors.New("fleet: provider creation reconciliation failed; saved operation retained")
		}
		return m.initialize(ctx, h, p)
	}
	if !force && r.State == "ready" && m.opts.Now().Sub(h.checked) < 5*time.Second {
		return nil
	}
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
	h.checked = m.opts.Now()
	return nil
}
func (m *Manager) resolve(ctx context.Context, id string) (proxy.Upstream, error) {
	h, err := m.handle(id)
	if err != nil {
		return proxy.Upstream{}, err
	}
	h.operation.Lock()
	defer h.operation.Unlock()
	if err = m.ensure(ctx, h, false); err != nil {
		return proxy.Upstream{}, err
	}
	r := m.copy(h)
	endpoint, err := m.opts.Providers[r.Provider].Connect(ctx, r.RemoteID, 3773)
	if err != nil {
		return proxy.Upstream{}, errors.New("fleet: private T3 connection failed")
	}
	return proxy.Upstream{Endpoint: endpoint, Token: r.Token}, nil
}
func (m *Manager) Stop(ctx context.Context, id string) error {
	h, err := m.handle(id)
	if err != nil {
		return err
	}
	h.operation.Lock()
	defer h.operation.Unlock()
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
	return m.update(h, func(r *record) { r.State = "stopped" })
}
func (m *Manager) Delete(ctx context.Context, id string) error {
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
		if err = m.opts.Providers[r.Provider].Delete(ctx, r.RemoteID); err != nil && !errors.Is(err, provider.ErrNotFound) {
			return errors.New("fleet: provider deletion failed; local state retained")
		}
	}
	m.mu.Lock()
	delete(m.machines, id)
	err = m.persistLocked()
	if err != nil {
		m.machines[id] = h
	}
	m.mu.Unlock()
	if err != nil {
		return err
	}
	return h.server.Close()
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
	servers := make([]*http.Server, 0, len(m.machines))
	for _, h := range m.machines {
		servers = append(servers, h.server)
	}
	m.mu.Unlock()
	var result error
	for _, s := range servers {
		if err := s.Shutdown(ctx); err != nil {
			_ = s.Close()
			result = errors.Join(result, err)
		}
	}
	return result
}
