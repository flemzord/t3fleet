package main

import (
	"context"
	"encoding/json/v2"
	"errors"
	"net/url"
	"os/exec"
	"runtime"
	"strconv"
	"time"

	"github.com/flemzord/t3fleet/internal/config"
)

// The CLI owns the desktop handoff: the daemon can run on another host and
// must never launch an application on the machine hosting its cloud credentials.
var openT3 = openDesktop

func pairingAppLink(data []byte) (string, error) {
	return pairingAppLinkForOrigin(data, false)
}

func pairingAppLinkForOrigin(data []byte, external bool) (string, error) {
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
	fragment, fragmentErr := url.ParseQuery(u.Fragment)
	if fragmentErr != nil || u.User != nil || u.EscapedPath() != "/pair" || u.ForceQuery || u.RawQuery != "" || fragment.Get("token") == "" {
		return "", errors.New("invalid pairing URL")
	}
	if external {
		if _, err := config.ParseExternalOrigin("https://" + u.Host); u.Scheme != "https" || err != nil {
			return "", errors.New("invalid HTTPS pairing URL")
		}
	} else {
		port, err := strconv.Atoi(u.Port())
		if err != nil || port < 1 || port > 65535 || u.Scheme != "http" || (u.Hostname() != "127.0.0.1" && u.Hostname() != "localhost") {
			return "", errors.New("invalid local pairing URL")
		}
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
