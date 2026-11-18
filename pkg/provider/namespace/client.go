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
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/flemzord/t3fleet/pkg/provider"
	"namespacelabs.dev/integrations/api"
	"namespacelabs.dev/integrations/auth"
)

const defaultEndpoint = "https://private-api.iad.namespaceapis.com"
const operationLabel = "t3fleet-operation-id"
const servicePath = "/namespace.private.devbox.v1beta.DevBoxService/"

// Config separates the control-plane endpoint from the machine's placement.
// HTTPClient and DialSSH are injectable to test the native contracts offline.
type Config struct {
	Endpoint     string
	Site         string
	VirtualCPU   int32
	MemoryMB     int32
	Architecture string
	TokenSource  api.TokenSource
	HTTPClient   *http.Client
	DialSSH      func(context.Context, SSHAuth) (Connection, error)
}

type Client struct{ config Config }

func New(config Config) (*Client, error) {
	if config.Endpoint == "" {
		config.Endpoint = defaultEndpoint
	}
	u, err := url.Parse(config.Endpoint)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, errors.New("namespace: invalid API endpoint")
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && (u.Hostname() == "127.0.0.1" || u.Hostname() == "localhost" || u.Hostname() == "::1")) {
		return nil, errors.New("namespace: API endpoint requires HTTPS")
	}
	config.Endpoint = strings.TrimRight(config.Endpoint, "/")
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
	if config.VirtualCPU < 1 || config.MemoryMB < 1 || (config.Architecture != "amd64" && config.Architecture != "arm64") {
		return nil, errors.New("namespace: invalid machine shape")
	}
	if config.TokenSource == nil {
		config.TokenSource, err = auth.LoadDefaults()
		if err != nil {
			return nil, fmt.Errorf("namespace: load credentials: %w", err)
		}
	}
	if config.HTTPClient == nil {
		config.HTTPClient = &http.Client{Timeout: 2 * time.Minute}
	}
	if config.DialSSH == nil {
		config.DialSSH = nativeSSH(config.TokenSource)
	}
	return &Client{config: config}, nil
}

func (*Client) Name() string { return "namespace" }

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

func (c *Client) call(ctx context.Context, method string, input, output any) error {
	body, err := json.Marshal(input)
	if err != nil {
		return err
	}
	token, err := c.config.TokenSource.IssueToken(ctx, 5*time.Minute, false)
	if err != nil {
		return fmt.Errorf("namespace: issue access token: %w", err)
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
	defer res.Body.Close()
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
	if (t.Image == "") == (t.Source == "") {
		return provider.Machine{}, errors.New("namespace: choose either an image or a source Devbox snapshot")
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
		return provider.Machine{}, err
	}
	labels := make([]label, 0, len(req.Labels)+1)
	for name, value := range req.Labels {
		labels = append(labels, label{name, value})
	}
	labels = append(labels, label{operationLabel, req.OperationID})
	shape, err := c.shape(req.Size)
	if err != nil {
		return provider.Machine{}, err
	}
	method := "Create"
	input := map[string]any{"name": req.Name, "instanceShape": shape, "labels": labels}
	if t.Source != "" {
		method = "CreateFromDevbox"
		input["fromDevbox"] = t.Source
		if t.Snapshot != "" {
			input["snapshotId"] = t.Snapshot
		}
	} else {
		input["site"] = c.config.Site
		input["activate"] = true
		input["versionControl"] = map[string]any{}
		if strings.HasPrefix(t.Image, "builtin:") {
			input["imageName"] = t.Image
		} else if strings.ContainsAny(t.Image, "/@:") {
			input["imageRef"] = t.Image
		} else {
			input["imageName"] = t.Image
		}
	}
	var out response
	if err = c.call(ctx, method, input, &out); err != nil {
		return provider.Machine{}, err
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
func (c *Client) shape(size string) (map[string]any, error) {
	cpu, mem := c.config.VirtualCPU, c.config.MemoryMB
	switch size {
	case "":
	case "s":
		cpu, mem = 4, 8192
	case "m":
		cpu, mem = 8, 16384
	case "l":
		cpu, mem = 16, 32768
	case "xl":
		cpu, mem = 32, 65536
	default:
		return nil, fmt.Errorf("namespace: unsupported size %q", size)
	}
	return map[string]any{"virtualCpu": cpu, "memoryMegabytes": mem, "machineArch": c.config.Architecture, "os": "linux"}, nil
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
func (c *Client) Stop(ctx context.Context, id string) error   { return c.byName(ctx, id, "Stop") }
func (c *Client) Delete(ctx context.Context, id string) error { return c.byName(ctx, id, "Expire") }

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
	defer conn.Close()
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
