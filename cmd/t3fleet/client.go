package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/flemzord/t3fleet/internal/config"
	"github.com/flemzord/t3fleet/internal/fleet"
)

var loadConfig = config.Load

func extractConfig(args []string) (string, []string, error) {
	path := config.DefaultPath()
	var out []string
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--config":
			if i+1 == len(args) || strings.HasPrefix(args[i+1], "--") {
				return "", nil, errors.New("--config requires a path")
			}
			i++
			path = args[i]
		case strings.HasPrefix(args[i], "--config="):
			path = strings.TrimPrefix(args[i], "--config=")
			if path == "" {
				return "", nil, errors.New("--config requires a path")
			}
		default:
			out = append(out, args[i])
		}
	}
	return path, out, nil
}

func runClient(ctx context.Context, cfg config.Config, command string, args []string, out, diagnostics io.Writer) error {
	method, path := http.MethodGet, "/v1/machines"
	var body any
	pairing := false
	switch command {
	case "list":
		if len(args) != 0 {
			return errors.New("list takes no arguments")
		}
	case "create":
		fs := flag.NewFlagSet("create", flag.ContinueOnError)
		fs.SetOutput(diagnostics)
		var request fleet.CreateRequest
		fs.StringVar(&request.Name, "name", "", "machine name")
		fs.StringVar(&request.Provider, "provider", "", "boat or namespace")
		fs.StringVar(&request.Size, "size", "", "provider machine size")
		fs.StringVar(&request.Template.Image, "image", "", "prepared image reference")
		fs.StringVar(&request.Template.Snapshot, "snapshot", "", "prepared snapshot reference")
		fs.StringVar(&request.Template.Source, "source", "", "golden Namespace Devbox")
		if err := fs.Parse(args); err != nil {
			return err
		}
		if fs.NArg() != 0 || request.Name == "" || request.Provider == "" {
			return errors.New("create requires --name and --provider")
		}
		method, body = http.MethodPost, request
	case "inspect", "start", "stop", "delete", "snapshot", "pair":
		if len(args) == 0 || args[0] == "" || strings.HasPrefix(args[0], "-") {
			return errors.New("command requires a machine ID")
		}
		id := args[0]
		args = args[1:]
		if strings.ContainsAny(id, "/\\?#\x00\r\n") {
			return errors.New("invalid machine ID")
		}
		path += "/" + url.PathEscape(id)
		switch command {
		case "inspect":
		case "delete":
			method = http.MethodDelete
		case "snapshot":
			fs := flag.NewFlagSet("snapshot", flag.ContinueOnError)
			fs.SetOutput(diagnostics)
			name := fs.String("name", "", "snapshot name")
			if err := fs.Parse(args); err != nil {
				return err
			}
			if fs.NArg() != 0 || *name == "" {
				return errors.New("snapshot requires --name")
			}
			args = nil
			method = http.MethodPost
			path += "/snapshot"
			body = map[string]string{"name": *name}
		default:
			method = http.MethodPost
			path += "/" + command
			pairing = command == "pair"
		}
		if len(args) != 0 {
			return errors.New("unexpected arguments")
		}
	default:
		return errors.New("unknown command; run t3fleet help")
	}
	token, err := readAdminToken(cfg.StateDir)
	if err != nil {
		return errors.New("daemon credentials unavailable; start t3fleet serve with the same configuration")
	}
	data, err := callAdmin(ctx, cfg.Listen, token, method, path, body)
	if err != nil {
		return err
	}
	if pairing {
		var pair struct {
			URL string `json:"url"`
		}
		if json.Unmarshal(data, &pair) != nil {
			return errors.New("invalid pairing response")
		}
		link := pair.URL
		if !strings.HasPrefix(link, "http://127.0.0.1:") && !strings.HasPrefix(link, "http://localhost:") {
			return errors.New("invalid pairing URL")
		}
		fmt.Fprintln(out, "t3code://pair?pairingUrl="+url.QueryEscape(link))
		return nil
	}
	if len(data) == 0 {
		fmt.Fprintln(out, "ok")
		return nil
	}
	var pretty bytes.Buffer
	if json.Indent(&pretty, data, "", "  ") != nil {
		return errors.New("invalid daemon response")
	}
	_, err = fmt.Fprintln(out, pretty.String())
	return err
}

func callAdmin(ctx context.Context, address, token, method, path string, body any) ([]byte, error) {
	var data []byte
	var err error
	if body != nil {
		data, err = json.Marshal(body)
		if err != nil {
			return nil, errors.New("cannot encode request")
		}
	}
	request, err := http.NewRequestWithContext(ctx, method, "http://"+address+path, bytes.NewReader(data))
	if err != nil {
		return nil, errors.New("cannot construct daemon request")
	}
	request.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	client := http.Client{Timeout: 10 * time.Minute, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errors.New("cannot reach the fleet daemon; check t3fleet serve")
	}
	defer response.Body.Close()
	data, err = io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if err != nil || len(data) > 1<<20 {
		return nil, errors.New("invalid daemon response")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		// The daemon emits a closed set of safe error codes, never vendor bodies.
		var failure struct {
			Error   string `json:"error"`
			Machine struct {
				ID string `json:"id"`
			} `json:"machine"`
		}
		_ = json.Unmarshal(data, &failure)
		switch failure.Error {
		case "not_found", "unsupported", "invalid_request", "conflict", "operation_failed", "unauthorized":
			if raw, err := hex.DecodeString(failure.Machine.ID); err == nil && len(raw) == 16 {
				return nil, fmt.Errorf("daemon: %s (HTTP %d); machine %s remains managed; inspect it or run start to recover", failure.Error, response.StatusCode, failure.Machine.ID)
			}
			return nil, fmt.Errorf("daemon: %s (HTTP %d)", failure.Error, response.StatusCode)
		default:
			return nil, fmt.Errorf("daemon request failed (HTTP %d)", response.StatusCode)
		}
	}
	return data, nil
}
