package main

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/flemzord/t3fleet/internal/config"
)

// testApp runs commands against a preloaded configuration and in-memory streams.
func testApp(cfg config.Config, out, diagnostics io.Writer) *app {
	return &app{stdout: out, stderr: diagnostics, stdin: strings.NewReader(""), cfg: &cfg}
}

func runClient(ctx context.Context, cfg config.Config, command string, args []string, out, diagnostics io.Writer) error {
	return testApp(cfg, out, diagnostics).execute(ctx, append([]string{command}, args...))
}

func mustAdminToken(t *testing.T, dir string) string {
	t.Helper()
	token, err := ensureAdminToken(dir)
	if err != nil {
		t.Fatal(err)
	}
	return token
}
