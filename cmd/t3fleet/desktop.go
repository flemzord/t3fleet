package main

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/flemzord/t3fleet/internal/config"
)

// The CLI owns the desktop handoff: the daemon can run on another host and
// must never launch an application on the machine hosting its cloud credentials.
var openT3 = openDesktop

func registerCreatedMachine(ctx context.Context, cfg config.Config, token string, data []byte, diagnostics io.Writer) error {
	var machine struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(data, &machine) != nil || machine.ID == "" || len(machine.ID) > 128 || strings.ContainsAny(machine.ID, "/\\?#\x00\r\n") {
		return errors.New("machine was created but its identity is invalid; use list before retrying")
	}
	// A failed desktop handoff must never turn into a second billable Create.
	recover := func(reason string) error {
		return fmt.Errorf("machine %s is ready, but %s; run t3fleet pair %s with the same --config to retry association without creating another VM", machine.ID, reason, machine.ID)
	}
	pair, err := callAdmin(ctx, cfg.Listen, token, http.MethodPost, "/v1/machines/"+url.PathEscape(machine.ID)+"/pair", nil)
	if err != nil {
		return recover("association could not be prepared")
	}
	link, err := pairingAppLink(pair)
	if err != nil {
		return recover("the pairing response was invalid")
	}
	if err := openT3(ctx, link); err != nil {
		// Launcher errors can contain the entire one-use credential. Never wrap
		// them, print their output, or include the link in ordinary diagnostics.
		return recover("T3 Code could not be opened")
	}
	fmt.Fprintln(diagnostics, "T3 Code opened. Confirm the backend association in T3 to finish adding this machine.")
	return nil
}

func pairingAppLink(data []byte) (string, error) {
	var pair struct {
		URL string `json:"url"`
	}
	if json.Unmarshal(data, &pair) != nil {
		return "", errors.New("invalid pairing response")
	}
	u, err := url.Parse(pair.URL)
	if err != nil {
		return "", errors.New("invalid pairing URL")
	}
	port, err := strconv.Atoi(u.Port())
	fragment, fragmentErr := url.ParseQuery(u.Fragment)
	if err != nil || fragmentErr != nil || port < 1 || port > 65535 || u.Scheme != "http" ||
		(u.Hostname() != "127.0.0.1" && u.Hostname() != "localhost") || u.User != nil ||
		u.Path != "/pair" || u.RawQuery != "" || fragment.Get("token") == "" {
		return "", errors.New("invalid pairing URL")
	}
	return "t3code://pair?pairingUrl=" + url.QueryEscape(u.String()), nil
}

func desktopCommand(platform, link string) (string, []string, error) {
	switch platform {
	case "darwin":
		return "/usr/bin/open", []string{link}, nil
	case "linux":
		return "xdg-open", []string{link}, nil
	case "windows":
		return "rundll32.exe", []string{"url.dll,FileProtocolHandler", link}, nil
	default:
		return "", nil, errors.New("desktop opening is unsupported on this platform")
	}
}

func openDesktop(ctx context.Context, link string) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	program, args, err := desktopCommand(runtime.GOOS, link)
	if err != nil {
		return err
	}
	// Pass the URL as a single argument, without a shell. Keep credential-bearing
	// launcher output private. Success means only that the OS accepted the link.
	return exec.CommandContext(ctx, program, args...).Run()
}
