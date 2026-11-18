// Package provider defines the extension contract for cloud backends.
package provider

import (
	"context"
	"errors"
	"net"
	"net/http"
)

var ErrNotFound = errors.New("machine not found")

// ErrCreateRejected means the provider explicitly declined creation without
// allocating a machine. Transport errors and unknown outcomes must not use it.
var ErrCreateRejected = errors.New("provider refused machine creation")

var ErrUnsupported = errors.New("operation not supported by provider")

type State string

const (
	StateStarting State = "starting"
	StateRunning  State = "running"
	StateStopped  State = "stopped"
	StateDeleted  State = "deleted"
	StateUnknown  State = "unknown"
)

// Template refers to a prepared image or a snapshot. Source identifies the
// golden Devbox when the provider scopes snapshots to a source machine.
type Template struct {
	Image    string `json:"image,omitempty"`
	Snapshot string `json:"snapshot,omitempty"`
	Source   string `json:"source,omitempty"`
}

type CreateRequest struct {
	Name        string
	Template    Template
	Size        string
	OperationID string
	Labels      map[string]string
}

type Machine struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	State State  `json:"state"`
}

type ExecRequest struct {
	Args []string
	Env  map[string]string
}
type ExecResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

// Endpoint is a private connection to one port. URL and Headers may contain
// credentials: callers must never log them or return them to clients.
// DialContext permits private transports (e.g. Namespace's authenticated SSH).
// Close releases any underlying tunnel; nil means no resources need closing.
type Endpoint struct {
	URL         string
	Headers     http.Header
	DialContext func(context.Context, string, string) (net.Conn, error)
	Close       func() error
}

// Provider owns vendor-specific lifecycle and private transport. Methods must
// honor context cancellation. Create uses OperationID to deduplicate retries.
type Provider interface {
	Name() string
	Create(context.Context, CreateRequest) (Machine, error)
	Get(context.Context, string) (Machine, error)
	Start(context.Context, string) error
	Stop(context.Context, string) error
	Delete(context.Context, string) error
	Exec(context.Context, string, ExecRequest) (ExecResult, error)
	Connect(context.Context, string, int) (Endpoint, error)
}

type Snapshot struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Ready bool   `json:"ready"`
}

// Snapshotter is optional: providers may support restoring prebuilt snapshots
// without offering an explicit capture operation.
type Snapshotter interface {
	Snapshot(context.Context, string, string) (Snapshot, error)
}
