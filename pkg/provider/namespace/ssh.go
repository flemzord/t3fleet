package namespace

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/flemzord/t3fleet/pkg/provider"
	"golang.org/x/crypto/ssh"
	"namespacelabs.dev/integrations/api"
	"namespacelabs.dev/integrations/nsc/ingress"
)

// SSHAuth is delivered by the authenticated Activate RPC. The key is transient
// and must not be persisted or included in logs.
type SSHAuth struct {
	InstanceID, IngressDomain, User string
	PrivateKey                      []byte
}

// Connection is the private transport seam; production uses SSH over a
// bearer-authenticated TLS WebSocket, tests supply an in-memory connection.
type Connection interface {
	Exec(context.Context, provider.ExecRequest) (provider.ExecResult, error)
	DialContext(context.Context, string, string) (net.Conn, error)
	Close() error
}

func nativeSSH(tokens api.TokenSource) func(context.Context, SSHAuth) (Connection, error) {
	return func(ctx context.Context, a SSHAuth) (Connection, error) {
		ctx, stopSetup := context.WithTimeout(ctx, 90*time.Second)
		defer stopSetup()
		signer, err := ssh.ParsePrivateKey(a.PrivateKey)
		if err != nil {
			return nil, errors.New("namespace: invalid instance SSH credential")
		}
		conn, err := ingress.DialHostedService(ctx, io.Discard, tokens, a.InstanceID, a.IngressDomain, "unixsocket", url.Values{"name": {"ssh"}})
		if err != nil {
			return nil, fmt.Errorf("namespace: connect authenticated gateway: %w", err)
		}
		// The Namespace HTTPS control plane selects the instance and supplies its
		// credential; the TLS gateway authenticates that exact instance route.
		// This is the trust model used by Namespace's maintained Devbox SDK. There
		// is no unauthenticated direct SSH hop and TLS verification stays enabled.
		config := &ssh.ClientConfig{User: a.User, Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)}, HostKeyCallback: ssh.InsecureIgnoreHostKey()}
		cancel := context.AfterFunc(ctx, func() { _ = conn.Close() })
		clientConn, chans, reqs, err := ssh.NewClientConn(conn, "namespace-devbox", config)
		cancel()
		if err != nil {
			_ = conn.Close()
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, errors.New("namespace: SSH handshake failed")
		}
		if ctx.Err() != nil {
			_ = clientConn.Close()
			return nil, ctx.Err()
		}
		return &sshConnection{client: ssh.NewClient(clientConn, chans, reqs)}, nil
	}
}

type sshConnection struct {
	client   *ssh.Client
	once     sync.Once
	closeErr error
}

func (c *sshConnection) Close() error {
	c.once.Do(func() { c.closeErr = c.client.Close() })
	return c.closeErr
}
func (c *sshConnection) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	return c.client.DialContext(ctx, network, address)
}

var validEnv = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

func shellCommand(req provider.ExecRequest) (string, error) {
	if len(req.Args) == 0 {
		return "", errors.New("namespace: empty exec command")
	}
	parts := make([]string, 0, len(req.Args)+len(req.Env)+1)
	if len(req.Env) > 0 {
		parts = append(parts, "env")
		keys := make([]string, 0, len(req.Env))
		for key := range req.Env {
			keys = append(keys, key)
		}
		slices.Sort(keys)
		for _, key := range keys {
			if !validEnv.MatchString(key) {
				return "", errors.New("namespace: invalid environment name")
			}
			if strings.ContainsRune(req.Env[key], 0) {
				return "", errors.New("namespace: environment value contains NUL")
			}
			parts = append(parts, quote(key+"="+req.Env[key]))
		}
	}
	for _, arg := range req.Args {
		if strings.ContainsRune(arg, 0) {
			return "", errors.New("namespace: command argument contains NUL")
		}
		parts = append(parts, quote(arg))
	}
	return strings.Join(parts, " "), nil
}
func quote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }

const maxOutput = 8 << 20

type outputBuffer struct {
	data      []byte
	truncated bool
}

func (b *outputBuffer) Write(p []byte) (int, error) {
	n := len(p)
	remaining := maxOutput - len(b.data)
	if len(p) > remaining {
		p = p[:remaining]
		b.truncated = true
	}
	b.data = append(b.data, p...)
	return n, nil
}
func (c *sshConnection) Exec(ctx context.Context, req provider.ExecRequest) (provider.ExecResult, error) {
	command, err := shellCommand(req)
	if err != nil {
		return provider.ExecResult{}, err
	}
	// NewSession itself waits for a channel-open reply. Close the owned
	// transport on cancellation before waiting, not only once a session exists.
	stop := context.AfterFunc(ctx, func() { _ = c.Close() })
	defer stop()
	session, err := c.client.NewSession()
	if err != nil {
		if ctx.Err() != nil {
			return provider.ExecResult{}, ctx.Err()
		}
		return provider.ExecResult{}, fmt.Errorf("namespace: open SSH session: %w", err)
	}
	defer session.Close()
	var stdout, stderr outputBuffer
	session.Stdout = &stdout
	session.Stderr = &stderr
	cancel := context.AfterFunc(ctx, func() { _ = session.Close() })
	defer cancel()
	err = session.Run(command)
	result := provider.ExecResult{Stdout: string(stdout.data), Stderr: string(stderr.data)}
	if ctx.Err() != nil {
		return result, ctx.Err()
	}
	if stdout.truncated || stderr.truncated {
		return result, errors.New("namespace: command output exceeded limit")
	}
	if err != nil {
		if exit, ok := errors.AsType[*ssh.ExitError](err); ok {
			result.ExitCode = exit.ExitStatus()
			return result, nil
		}
		return result, fmt.Errorf("namespace: execute command: %w", err)
	}
	return result, nil
}

var _ provider.Provider = (*Client)(nil)
