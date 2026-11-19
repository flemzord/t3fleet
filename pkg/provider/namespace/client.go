// Package namespace implements persistent Namespace Devboxes through their native
// Connect API and authenticated SSH gateway. It does not invoke a vendor CLI.
package namespace

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"namespacelabs.dev/integrations/api"

	"github.com/flemzord/t3fleet/pkg/provider"
)

const defaultEndpoint = "https://private-api.iad.namespaceapis.com"
const operationLabel = "t3fleet-operation-id"
const servicePath = "/namespace.private.devbox.v1beta.DevBoxService/"

// Config separates the control-plane endpoint from the machine's placement.
// HTTPClient and DialSSH are injectable to test the native contracts offline.
type Config struct {
	TenantID     string
	Endpoint     string
	Site         string
	VirtualCPU   int32
	MemoryMB     int32
	Architecture string
	TokenFile    string // Optional per-instance Namespace SDK credential file.
	TokenSource  api.TokenSource
	HTTPClient   *http.Client
	DialSSH      func(context.Context, SSHAuth) (Connection, error)
}

type Client struct {
	config Config
	scope  string
}

var _ provider.ShapeResolver = (*Client)(nil)

// NormalizeEndpoint returns the canonical Devbox Connect origin without
// opening the Namespace token file or exchanging credentials.
func NormalizeEndpoint(raw string) (string, error) {
	if raw == "" {
		raw = defaultEndpoint
	}
	return provider.NormalizeAPIEndpoint(raw, false)
}

func New(config Config) (*Client, error) {
	endpoint, err := NormalizeEndpoint(config.Endpoint)
	if err != nil {
		return nil, err
	}
	config.Endpoint = endpoint
	if config.Site == "" {
		config.Site = "iad"
	}
	if config.VirtualCPU == 0 {
		config.VirtualCPU = 4
	}
	if config.MemoryMB == 0 {
		config.MemoryMB = 8192
	}
	if config.Architecture == "" {
		config.Architecture = "amd64"
	}
	if !validShape(provider.Shape{VirtualCPU: config.VirtualCPU, MemoryMB: config.MemoryMB, Architecture: config.Architecture}) {
		return nil, errors.New("namespace: invalid machine shape")
	}
	if !provider.ValidAccountID(config.TenantID) {
		return nil, errors.New("namespace: an explicit tenant ID is required")
	}
	config.TokenSource = tenantTokens{tenantID: config.TenantID, file: config.TokenFile, source: config.TokenSource}
	if config.HTTPClient == nil {
		config.HTTPClient = &http.Client{Timeout: 2 * time.Minute}
	}
	if config.DialSSH == nil {
		config.DialSSH = nativeSSH(config.TokenSource)
	}
	return &Client{config: config, scope: provider.ScopeFingerprint("namespace", endpoint, config.TenantID)}, nil
}

func (*Client) Name() string      { return "namespace" }
func (c *Client) ScopeID() string { return c.scope }

type label struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}
type devbox struct {
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	MainUser    string  `json:"mainUser"`
	DestroyedAt string  `json:"destroyedAt"`
	Labels      []label `json:"labels"`
	Spec        struct {
		RemoteUser string `json:"remoteUser"`
	} `json:"spec"`
}
type response struct {
	Devbox        *devbox `json:"devbox"`
	InstanceID    string  `json:"instanceId"`
	PrivateSSHKey []byte  `json:"privateSshKey"`
	Metadata      struct {
		IngressDomain string `json:"ingressDomain"`
	} `json:"instanceMetadataSummary"`
}

func (r response) machine() (provider.Machine, error) {
	if r.Devbox == nil || r.Devbox.ID == "" || r.Devbox.Name == "" {
		return provider.Machine{}, errors.New("namespace: incomplete Devbox response")
	}
	state := provider.StateStopped
	if r.InstanceID != "" {
		state = provider.StateRunning
	}
	if r.Devbox.DestroyedAt != "" {
		state = provider.StateDeleted
	}
	return provider.Machine{ID: r.Devbox.ID, Name: r.Devbox.Name, State: state}, nil
}

var errAccessTokenUnavailable = errors.New("namespace: could not issue access token")

func (c *Client) call(ctx context.Context, method string, input, output any) error {
	body, err := json.Marshal(input)
	if err != nil {
		return err
	}
	token, err := c.config.TokenSource.IssueToken(ctx, 5*time.Minute, false)
	if err != nil {
		return fmt.Errorf("%w: %w", errAccessTokenUnavailable, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.config.Endpoint+servicePath+method, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Connect-Protocol-Version", "1")
	// Never forward a bearer credential to a redirect target.
	client := *c.config.HTTPClient
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	res, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("namespace %s: %w", method, err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode == http.StatusNotFound {
		return provider.ErrNotFound
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return fmt.Errorf("namespace %s: HTTP %d", method, res.StatusCode)
	}
	if output == nil {
		_, err = io.Copy(io.Discard, io.LimitReader(res.Body, 1<<20))
		return err
	}
	if err = json.NewDecoder(io.LimitReader(res.Body, 8<<20)).Decode(output); err != nil {
		return fmt.Errorf("namespace %s: invalid response: %w", method, err)
	}
	return nil
}

func (c *Client) fetch(ctx context.Context, id string) (response, error) {
	var out response
	err := c.call(ctx, "Fetch", map[string]any{"idOrName": id, "returnActivatedInstance": true}, &out)
	return out, err
}
func (c *Client) Get(ctx context.Context, id string) (provider.Machine, error) {
	out, err := c.fetch(ctx, id)
	if err != nil {
		return provider.Machine{}, err
	}
	return out.machine()
}

var validName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,62}$`)
var validLabelName = regexp.MustCompile(`^[a-z]([a-z0-9-.]*[a-z0-9])?$`)

func (c *Client) DefaultPlacement() provider.Placement {
	return provider.Placement{Site: c.config.Site}
}

func (c *Client) Create(ctx context.Context, req provider.CreateRequest) (provider.Machine, error) {
	if !validName.MatchString(req.Name) || req.OperationID == "" || len(req.OperationID) > 255 {
		return provider.Machine{}, errors.New("namespace: a valid stable name and operation ID are required")
	}
	// Namespace labels are not Kubernetes labels: their names exclude '/'.
	// Reject malformed user labels before contacting the control plane.
	for name, value := range req.Labels {
		if name == operationLabel {
			return provider.Machine{}, errors.New("namespace: reserved operation label")
		}
		if len(name) > 63 || !validLabelName.MatchString(name) || len(value) > 255 {
			return provider.Machine{}, errors.New("namespace: label name or value violates Namespace limits")
		}
	}
	t := req.Template
	if t.Snapshot != "" && t.Source == "" {
		return provider.Machine{}, errors.New("namespace: snapshot requires its source Devbox")
	}
	if t.Image == "registered:" {
		return provider.Machine{}, errors.New("namespace: registered image name is empty")
	}
	if (t.Image == "") == (t.Source == "") {
		return provider.Machine{}, errors.New("namespace: choose either an image or a source Devbox snapshot")
	}
	shape := req.Shape
	if shape == (provider.Shape{}) {
		var err error
		if shape, err = c.ResolveShape(req.Size); err != nil {
			return provider.Machine{}, err
		}
	} else if !validShape(shape) {
		return provider.Machine{}, errors.New("namespace: invalid reserved machine shape")
	}
	// A token failure happens before sending a request and is the only
	// Namespace failure that currently proves this attempt did not create a
	// Devbox. Errors from Create itself can happen during activation.
	creationError := func(err error) error {
		if ctx.Err() == nil && errors.Is(err, errAccessTokenUnavailable) {
			return fmt.Errorf("%w: %w", provider.ErrCreateRejected, err)
		}
		return err
	}
	// Namespace has no idempotency-key field. Reconcile retries by stable name
	// and an operation label; never adopt another operation's Devbox.
	existing, err := c.fetch(ctx, req.Name)
	if err == nil {
		if existing.Devbox == nil {
			return provider.Machine{}, errors.New("namespace: incomplete existing Devbox")
		}
		for _, l := range existing.Devbox.Labels {
			if l.Name == operationLabel && l.Value == req.OperationID {
				return existing.machine()
			}
		}
		return provider.Machine{}, errors.New("namespace: Devbox name already belongs to another operation")
	}
	if !errors.Is(err, provider.ErrNotFound) {
		return provider.Machine{}, creationError(err)
	}
	labels := make([]label, 0, len(req.Labels)+1)
	for name, value := range req.Labels {
		labels = append(labels, label{name, value})
	}
	labels = append(labels, label{operationLabel, req.OperationID})
	method := "Create"
	input := map[string]any{"name": req.Name, "instanceShape": map[string]any{"virtualCpu": shape.VirtualCPU, "memoryMegabytes": shape.MemoryMB, "machineArch": shape.Architecture, "os": "linux"}, "labels": labels}
	if t.Source != "" {
		method = "CreateFromDevbox"
		input["fromDevbox"] = t.Source
		if t.Snapshot != "" {
			input["snapshotId"] = t.Snapshot
		}
	} else {
		site := c.config.Site
		if req.Placement != nil {
			site = req.Placement.Site
			if site == "" {
				return provider.Machine{}, errors.New("namespace: reserved placement has no site")
			}
		}
		input["site"] = site
		input["activate"] = true
		input["versionControl"] = map[string]any{}
		name, registered := strings.CutPrefix(t.Image, "registered:")
		switch {
		case registered:
			input["imageName"] = name
		case strings.HasPrefix(t.Image, "builtin:"):
			input["imageName"] = t.Image
		case strings.ContainsAny(t.Image, "/@:"):
			input["imageRef"] = t.Image
		default:
			input["imageName"] = t.Image
		}
	}
	var out response
	if err = c.call(ctx, method, input, &out); err != nil {
		return provider.Machine{}, creationError(err)
	}
	machine, err := out.machine()
	if err != nil {
		return provider.Machine{}, err
	}
	if machine.State != provider.StateRunning {
		if err = c.Start(ctx, machine.ID); err != nil {
			return machine, err
		}
		machine, err = c.Get(ctx, machine.ID)
	}
	return machine, err
}

// ValidSize reports whether the preset is supported. Empty uses the configured shape.
func ValidSize(size string) bool {
	_, ok := presets[size]
	return size == "" || ok
}

var presets = map[string]struct{ cpu, mem int32 }{
	"s": {4, 8192}, "m": {8, 16384}, "l": {16, 32768}, "xl": {32, 65536},
}

func validShape(s provider.Shape) bool {
	return s.VirtualCPU > 0 && s.MemoryMB > 0 && (s.Architecture == "amd64" || s.Architecture == "arm64")
}

func (c *Client) ResolveShape(size string) (provider.Shape, error) {
	cpu, mem := c.config.VirtualCPU, c.config.MemoryMB
	if size != "" {
		shape, ok := presets[size]
		if !ok {
			return provider.Shape{}, fmt.Errorf("namespace: unsupported size %q", size)
		}
		cpu, mem = shape.cpu, shape.mem
	}
	return provider.Shape{VirtualCPU: cpu, MemoryMB: mem, Architecture: c.config.Architecture}, nil
}
func (c *Client) activate(ctx context.Context, id string, ssh bool) (response, error) {
	var out response
	err := c.call(ctx, "Activate", map[string]any{"idOrName": id, "waitForReadiness": true, "includeSshCredentials": ssh}, &out)
	return out, err
}
func (c *Client) Start(ctx context.Context, id string) error {
	out, err := c.activate(ctx, id, false)
	if err != nil {
		return err
	}
	machine, err := out.machine()
	if err != nil {
		return err
	}
	if machine.State != provider.StateRunning {
		return errors.New("namespace: activation did not return a ready instance")
	}
	return nil
}
func (c *Client) byName(ctx context.Context, id, method string) error {
	out, err := c.fetch(ctx, id)
	if err != nil {
		return err
	}
	if out.Devbox == nil || out.Devbox.Name == "" {
		return errors.New("namespace: incomplete Devbox response")
	}
	return c.call(ctx, method, map[string]string{"name": out.Devbox.Name}, nil)
}
func (c *Client) Stop(ctx context.Context, id string) error { return c.byName(ctx, id, "Stop") }

// Delete acknowledges completion only after the control plane reports a
// terminal state. Expire accepting a request is insufficient cleanup proof.
func (c *Client) Delete(ctx context.Context, id string) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	out, err := c.fetch(ctx, id)
	if errors.Is(err, provider.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	machine, err := out.machine()
	if err != nil {
		return err
	}
	if machine.State == provider.StateDeleted {
		return nil
	}
	if err := c.call(ctx, "Expire", map[string]string{"name": machine.Name}, nil); err != nil {
		return err
	}
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		machine, err := c.Get(ctx, id)
		if errors.Is(err, provider.ErrNotFound) || (err == nil && machine.State == provider.StateDeleted) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("namespace: confirm deletion: %w", err)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("namespace: confirm deletion: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

// ListSnapshots discovers provider-created snapshots. Namespace does not expose
// an explicit capture RPC; this client deliberately does not implement Snapshotter.
func (c *Client) ListSnapshots(ctx context.Context, id string) ([]provider.Snapshot, error) {
	var out struct {
		Snapshots []struct {
			ID        string `json:"id"`
			Completed string `json:"completedSnapshot"`
			Abandoned string `json:"abandonedAt"`
		} `json:"snapshots"`
	}
	if err := c.call(ctx, "ListSnapshots", map[string]string{"idOrName": id}, &out); err != nil {
		return nil, err
	}
	result := make([]provider.Snapshot, 0, len(out.Snapshots))
	for _, s := range out.Snapshots {
		result = append(result, provider.Snapshot{ID: s.ID, Name: s.ID, Ready: s.Completed != "" && s.Abandoned == ""})
	}
	return result, nil
}

func (c *Client) connection(ctx context.Context, id string) (Connection, error) {
	out, err := c.activate(ctx, id, true)
	if err != nil {
		return nil, err
	}
	if out.Devbox == nil || out.InstanceID == "" || out.Metadata.IngressDomain == "" || len(out.PrivateSSHKey) == 0 {
		return nil, errors.New("namespace: incomplete SSH activation response")
	}
	user := out.Devbox.MainUser
	if user == "" {
		user = out.Devbox.Spec.RemoteUser
	}
	if user == "" {
		user = "root"
	}
	return c.config.DialSSH(ctx, SSHAuth{InstanceID: out.InstanceID, IngressDomain: out.Metadata.IngressDomain, User: user, PrivateKey: out.PrivateSSHKey})
}
func (c *Client) Exec(ctx context.Context, id string, req provider.ExecRequest) (provider.ExecResult, error) {
	if _, err := shellCommand(req); err != nil {
		return provider.ExecResult{}, err
	}
	conn, err := c.connection(ctx, id)
	if err != nil {
		return provider.ExecResult{}, err
	}
	defer func() { _ = conn.Close() }()
	return conn.Exec(ctx, req)
}
func (c *Client) Connect(ctx context.Context, id string, port int) (provider.Endpoint, error) {
	if port < 1 || port > 65535 {
		return provider.Endpoint{}, errors.New("namespace: invalid port")
	}
	conn, err := c.connection(ctx, id)
	if err != nil {
		return provider.Endpoint{}, err
	}
	address := "127.0.0.1:" + strconv.Itoa(port)
	return provider.Endpoint{URL: "http://" + address, DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
		return conn.DialContext(ctx, network, address)
	}, Close: conn.Close}, nil
}
