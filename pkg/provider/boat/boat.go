// Package boat implements the Boat v1 HTTP API without invoking its CLI.
package boat

import (
	"bytes"
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/flemzord/t3fleet/pkg/provider"
)

const defaultBaseURL = "https://boat.dev/api/v1"

// Config contains runtime credentials, never image-building credentials.
type Config struct {
	APIKey       string
	Organization string
	// Environment explicitly opts in to Boat-managed repositories and secrets.
	// Empty means no account credential injection.
	Environment string
	BaseURL     string
	HTTPClient  *http.Client
}

type Client struct {
	base         string
	key          string
	organization string
	environment  string
	http         *http.Client
	pollInterval time.Duration
}

var _ provider.Provider = (*Client)(nil)
var _ provider.Snapshotter = (*Client)(nil)

// New validates the API origin and keeps credentials only in memory. Plain HTTP
// is accepted for loopback test servers; production endpoints require HTTPS.
func New(cfg Config) (*Client, error) {
	base := cmp.Or(cfg.BaseURL, defaultBaseURL)
	u, err := url.Parse(base)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("boat: invalid API base URL")
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && slices.Contains([]string{"localhost", "127.0.0.1", "::1"}, u.Hostname())) {
		return nil, errors.New("boat: API base URL requires HTTPS")
	}
	if cfg.APIKey == "" || strings.ContainsAny(cfg.APIKey+cfg.Organization+cfg.Environment, "\r\n\x00") {
		return nil, errors.New("boat: invalid credentials or organization")
	}
	hc := http.Client{Timeout: 70 * time.Second}
	if cfg.HTTPClient != nil {
		hc = *cfg.HTTPClient
	}
	// Never forward the account bearer token to a redirect target.
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Client{base: strings.TrimRight(base, "/"), key: cfg.APIKey, organization: cfg.Organization, environment: cfg.Environment, http: &hc, pollInterval: time.Second}, nil
}

func (*Client) Name() string { return "boat" }

// APIError deliberately excludes the vendor message and response body, which
// may contain private command output, credentials, or token-bearing URLs.
type APIError struct {
	Status int
	Code   string
}

func (e *APIError) Error() string {
	if e.Code == "" {
		return fmt.Sprintf("boat API returned HTTP %d", e.Status)
	}
	return fmt.Sprintf("boat API returned HTTP %d (%s)", e.Status, e.Code)
}

type envelope struct {
	OK   bool   `json:"ok"`
	Code string `json:"code"`
}

var safeCode = regexp.MustCompile(`^[a-z][a-z0-9_]{0,80}$`)

func (c *Client) request(ctx context.Context, method, path string, body any, headers http.Header, dst any) error {
	var data []byte
	var err error
	if body != nil {
		data, err = json.Marshal(body)
		if err != nil {
			return errors.New("boat: cannot encode request")
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, bytes.NewReader(data))
	if err != nil {
		return errors.New("boat: cannot construct request")
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	if c.organization != "" {
		req.Header.Set("X-Boat-Org", c.organization)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, values := range headers {
		req.Header[k] = values
	}
	res, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("boat: API transport failed")
	}
	defer res.Body.Close()
	data, err = io.ReadAll(io.LimitReader(res.Body, 8*1024*1024+1))
	if err != nil || len(data) > 8*1024*1024 {
		return errors.New("boat: invalid API response")
	}
	if res.StatusCode == http.StatusNotFound {
		return provider.ErrNotFound
	}
	var env envelope
	_ = json.Unmarshal(data, &env)
	if res.StatusCode < 200 || res.StatusCode >= 300 || (len(data) > 0 && !env.OK) {
		code := env.Code
		if !safeCode.MatchString(code) {
			code = ""
		}
		return &APIError{Status: res.StatusCode, Code: code}
	}
	if dst != nil && len(data) > 0 {
		if err := json.Unmarshal(data, dst); err != nil {
			return errors.New("boat: invalid API response")
		}
	}
	return nil
}

type sandbox struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	State string `json:"state"`
}

func (s sandbox) machine() provider.Machine {
	state := provider.StateUnknown
	switch s.State {
	case "ready", "idle", "running":
		state = provider.StateRunning
	case "provisioning", "provisioned", "cloning", "resuming", "archiving":
		state = provider.StateStarting
	case "archived", "stopped":
		state = provider.StateStopped
	case "cancelled", "deleted":
		state = provider.StateDeleted
	}
	return provider.Machine{ID: s.ID, Name: s.Name, State: state}
}

func sandboxPath(id string) (string, error) {
	if id == "" || strings.ContainsAny(id, "/\\\x00\r\n") || id == "." || id == ".." {
		return "", errors.New("boat: invalid sandbox id")
	}
	return "/sandboxes/" + url.PathEscape(id), nil
}

func (c *Client) Create(ctx context.Context, req provider.CreateRequest) (provider.Machine, error) {
	template := cmp.Or(req.Template.Snapshot, req.Template.Image)
	if template == "" || (req.Template.Snapshot != "" && req.Template.Image != "" && req.Template.Snapshot != req.Template.Image) {
		return provider.Machine{}, errors.New("boat: one saved snapshot is required")
	}
	if req.OperationID == "" || strings.ContainsAny(req.OperationID, "\r\n\x00") {
		return provider.Machine{}, errors.New("boat: operation id is required")
	}
	if req.Size != "" && !slices.Contains([]string{"small", "default", "large", "xlarge"}, req.Size) {
		return provider.Machine{}, errors.New("boat: unsupported machine size")
	}
	body := struct {
		From        string            `json:"from"`
		Name        string            `json:"name"`
		Type        string            `json:"type,omitempty"`
		Visibility  string            `json:"visibility"`
		Environment string            `json:"environment,omitempty"`
		NoEnv       bool              `json:"noEnv"`
		Labels      map[string]string `json:"labels,omitzero"`
	}{template, req.Name, req.Size, "private", c.environment, c.environment == "", req.Labels}
	var res struct {
		Sandbox sandbox `json:"sandbox"`
	}
	err := c.request(ctx, http.MethodPost, "/sandboxes", body, http.Header{"Idempotency-Key": {req.OperationID}}, &res)
	if err != nil {
		return provider.Machine{}, err
	}
	if res.Sandbox.ID == "" {
		return provider.Machine{}, errors.New("boat: create response has no sandbox id")
	}
	return res.Sandbox.machine(), nil
}

func (c *Client) Get(ctx context.Context, id string) (provider.Machine, error) {
	path, err := sandboxPath(id)
	if err != nil {
		return provider.Machine{}, err
	}
	var res struct {
		Sandbox sandbox `json:"sandbox"`
	}
	if err := c.request(ctx, http.MethodGet, path, nil, nil, &res); err != nil {
		return provider.Machine{}, err
	}
	if res.Sandbox.ID == "" || res.Sandbox.ID != id {
		return provider.Machine{}, errors.New("boat: inconsistent sandbox response")
	}
	return res.Sandbox.machine(), nil
}

func (c *Client) Start(ctx context.Context, id string) error {
	return c.transition(ctx, id, "/resume", provider.StateRunning)
}

func (c *Client) Stop(ctx context.Context, id string) error {
	return c.transition(ctx, id, "/stop", provider.StateStopped)
}

func (c *Client) transition(ctx context.Context, id, action string, wanted provider.State) error {
	path, err := sandboxPath(id)
	if err != nil {
		return err
	}
	if err := c.request(ctx, http.MethodPost, path+action, struct{}{}, nil, nil); err != nil {
		return err
	}
	for {
		m, err := c.Get(ctx, id)
		if err != nil {
			return err
		}
		if m.State == wanted {
			return nil
		}
		if m.State == provider.StateUnknown || m.State == provider.StateDeleted {
			return errors.New("boat: lifecycle transition failed")
		}
		if err := c.wait(ctx); err != nil {
			return err
		}
	}
}

func (c *Client) Delete(ctx context.Context, id string) error {
	path, err := sandboxPath(id)
	if err != nil {
		return err
	}
	var res struct {
		Operation struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"operation"`
	}
	if err := c.request(ctx, http.MethodDelete, path, nil, http.Header{"X-Ascii-Confirm-Delete": {id}}, &res); err != nil {
		if errors.Is(err, provider.ErrNotFound) {
			return nil
		}
		return err
	}
	if res.Operation.ID == "" {
		return errors.New("boat: deletion response has no operation id")
	}
	for res.Operation.Status != "completed" {
		if err := c.wait(ctx); err != nil {
			return err
		}
		if err := c.request(ctx, http.MethodGet, "/deletion-operations/"+url.PathEscape(res.Operation.ID), nil, nil, &res); err != nil {
			return err
		}
	}
	return nil
}

var envKey = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }

// Exec uses the private files API for output. Boat retains command history, so
// only the numeric exit status is allowed into a command's stdout/stderr.
// Arguments and environment values remain part of Boat's retained command input
// and must not contain credentials.
func (c *Client) Exec(ctx context.Context, id string, req provider.ExecRequest) (result provider.ExecResult, resultErr error) {
	path, err := sandboxPath(id)
	if err != nil {
		return provider.ExecResult{}, err
	}
	if len(req.Args) == 0 {
		return provider.ExecResult{}, errors.New("boat: command arguments are required")
	}
	parts := []string{"env", "--"}
	keys := make([]string, 0, len(req.Env))
	for key := range req.Env {
		if !envKey.MatchString(key) || strings.ContainsRune(req.Env[key], 0) {
			return provider.ExecResult{}, errors.New("boat: invalid command environment")
		}
		keys = append(keys, key)
	}
	slices.Sort(keys)
	for _, key := range keys {
		parts = append(parts, shellQuote(key+"="+req.Env[key]))
	}
	for _, arg := range req.Args {
		if strings.ContainsRune(arg, 0) {
			return provider.ExecResult{}, errors.New("boat: invalid command argument")
		}
		parts = append(parts, shellQuote(arg))
	}
	var random [32]byte
	if _, err := rand.Read(random[:]); err != nil {
		return provider.ExecResult{}, errors.New("boat: cannot prepare private command output")
	}
	directory := "/tmp/t3fleet-exec-" + hex.EncodeToString(random[:])
	// Cleanup is attempted even after a transport error: the execution may have
	// reached the machine before the response was lost. A canceled caller must
	// not cancel removal of private output files.
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		defer cancel()
		query := url.Values{"path": {directory}, "recursive": {"true"}}
		err := c.request(cleanupCtx, http.MethodDelete, path+"/files?"+query.Encode(), nil, nil, nil)
		if err != nil && !errors.Is(err, provider.ErrNotFound) {
			// Return no partially retrieved secrets when cleanup did not succeed.
			result = provider.ExecResult{}
			resultErr = errors.Join(resultErr, errors.New("boat: private command output cleanup failed"))
		}
	}()
	command := "exec 2>/dev/null\numask 077\n" +
		"mkdir -m 700 -- " + shellQuote(directory) + " || exit 125\n" +
		"( " + strings.Join(parts, " ") + " ) >" + shellQuote(directory+"/stdout") + " 2>" + shellQuote(directory+"/stderr") + "\n" +
		"status=$?\nprintf '%s\\n' \"$status\""
	body := struct {
		Command        string `json:"command"`
		TimeoutSeconds int    `json:"timeoutSeconds"`
	}{command, 60}
	var res struct {
		Stdout          string `json:"stdout"`
		Stderr          string `json:"stderr"`
		ExitCode        *int   `json:"exitCode"`
		StdoutTruncated bool   `json:"stdoutTruncated"`
		StderrTruncated bool   `json:"stderrTruncated"`
	}
	if err := c.request(ctx, http.MethodPost, path+"/commands", body, nil, &res); err != nil {
		return provider.ExecResult{}, err
	}
	if res.ExitCode == nil || *res.ExitCode != 0 || res.Stderr != "" || res.StdoutTruncated || res.StderrTruncated || !commandStatus.MatchString(res.Stdout) {
		return provider.ExecResult{}, errors.New("boat: invalid private command completion")
	}
	status, err := strconv.Atoi(strings.TrimSuffix(res.Stdout, "\n"))
	if err != nil || status > 255 {
		return provider.ExecResult{}, errors.New("boat: invalid private command exit status")
	}
	stdout, err := c.readPrivateOutput(ctx, path, directory+"/stdout")
	if err != nil {
		return provider.ExecResult{}, err
	}
	stderr, err := c.readPrivateOutput(ctx, path, directory+"/stderr")
	if err != nil {
		return provider.ExecResult{}, err
	}
	return provider.ExecResult{Stdout: stdout, Stderr: stderr, ExitCode: status}, nil
}

var commandStatus = regexp.MustCompile(`^(0|[1-9][0-9]{0,2})\n?$`)

const maxExecOutput = 4 * 1024 * 1024

func (c *Client) readPrivateOutput(ctx context.Context, sandbox, filename string) (string, error) {
	query := url.Values{"path": {filename}, "encoding": {"utf8"}}
	var res struct {
		Success  bool    `json:"success"`
		Path     string  `json:"path"`
		Encoding string  `json:"encoding"`
		Size     *int64  `json:"size"`
		Content  *string `json:"content"`
	}
	if err := c.request(ctx, http.MethodGet, sandbox+"/files?"+query.Encode(), nil, nil, &res); err != nil {
		return "", err
	}
	if !res.Success || res.Path != filename || res.Encoding != "utf8" || res.Size == nil || res.Content == nil ||
		*res.Size < 0 || *res.Size > maxExecOutput || int64(len(*res.Content)) != *res.Size || !utf8.ValidString(*res.Content) {
		return "", errors.New("boat: invalid private command output")
	}
	return *res.Content, nil
}

func (c *Client) Connect(ctx context.Context, id string, port int) (provider.Endpoint, error) {
	path, err := sandboxPath(id)
	if err != nil {
		return provider.Endpoint{}, err
	}
	if port < 1 || port > 65535 {
		return provider.Endpoint{}, errors.New("boat: invalid port")
	}
	body := struct {
		Port      int  `json:"port"`
		Public    bool `json:"public"`
		Localhost bool `json:"localhost"`
	}{port, false, true}
	var res struct {
		URL string `json:"url"`
	}
	if err := c.request(ctx, http.MethodPost, path+"/host", body, nil, &res); err != nil {
		return provider.Endpoint{}, err
	}
	u, err := url.Parse(res.URL)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Fragment != "" {
		return provider.Endpoint{}, errors.New("boat: invalid hosted endpoint")
	}
	if u.Query().Get("_token") == "" {
		return provider.Endpoint{}, errors.New("boat: hosted endpoint is not private")
	}
	return provider.Endpoint{URL: u.String()}, nil
}

var snapshotName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

func (c *Client) Snapshot(ctx context.Context, id, name string) (provider.Snapshot, error) {
	if _, err := sandboxPath(id); err != nil {
		return provider.Snapshot{}, err
	}
	if !snapshotName.MatchString(name) || slices.Contains([]string{"latest", "tree", "pull", "rm", "save", "current", "self", "new"}, name) {
		return provider.Snapshot{}, errors.New("boat: invalid saved snapshot name")
	}
	body := struct {
		SandboxID string `json:"sandboxId"`
		Name      string `json:"name"`
	}{id, name}
	var res struct {
		Snapshot struct {
			ID     string `json:"snapshotId"`
			Name   string `json:"name"`
			Status string `json:"status"`
		} `json:"snapshot"`
	}
	if err := c.request(ctx, http.MethodPost, "/saved-snapshots", body, nil, &res); err != nil {
		return provider.Snapshot{}, err
	}
	for {
		switch res.Snapshot.Status {
		case "ready":
			if res.Snapshot.Name != name || res.Snapshot.ID == "" {
				return provider.Snapshot{}, errors.New("boat: inconsistent saved snapshot response")
			}
			return provider.Snapshot{ID: name, Name: name, Ready: true}, nil
		case "failed":
			return provider.Snapshot{}, errors.New("boat: snapshot save failed")
		case "saving":
		default:
			return provider.Snapshot{}, errors.New("boat: unknown saved snapshot status")
		}
		if err := c.wait(ctx); err != nil {
			return provider.Snapshot{}, err
		}
		if err := c.request(ctx, http.MethodGet, "/saved-snapshots/"+url.PathEscape(name), nil, nil, &res); err != nil {
			return provider.Snapshot{}, err
		}
	}
}

func (c *Client) wait(ctx context.Context) error {
	timer := time.NewTimer(c.pollInterval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
