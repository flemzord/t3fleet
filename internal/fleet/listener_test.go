package fleet

import (
	"net"
	"net/http"
	"testing"
)

func TestManagerCloseReleasesPortBeforeServe(t *testing.T) {
	var config net.ListenConfig
	listener, err := config.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	addr := listener.Addr().String()

	// Model a shutdown before the Serve goroutine registers the listener.
	m := &Manager{
		cancel: func() {},
		machines: map[string]*machineHandle{
			"worker": {server: new(http.Server), listener: listener},
		},
	}
	if err := m.Close(t.Context()); err != nil {
		t.Fatal(err)
	}

	reopened, err := config.Listen(t.Context(), "tcp", addr)
	if err != nil {
		t.Fatalf("cannot reuse machine port after shutdown: %v", err)
	}
	_ = reopened.Close()
}
