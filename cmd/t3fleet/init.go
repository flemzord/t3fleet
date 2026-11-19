package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/flemzord/t3fleet/internal/config"
	"github.com/flemzord/t3fleet/pkg/provider"
)

type initOptions struct {
	providers         []string
	stateDir          string
	boatOrganization  string
	boatSnapshot      string
	namespaceTenantID string
	namespaceSite     string
	namespaceImage    string
	t3Binary          string
	noInput           bool
	force             bool
	print             bool
}

func (a *app) initCommand() *cobra.Command {
	var opts initOptions
	cmd := &cobra.Command{
		Use:     "init",
		Short:   "Create a configuration file",
		GroupID: "daemon",
		Long: `Create a t3fleet configuration file at the --config path, $T3FLEET_CONFIG,
or the default location. In a terminal, init asks for the values that flags
do not provide. Set the Boat organization or Namespace tenant for each instance.
The file holds no secrets. The daemon reads credentials when it uses an instance.

The directory is created private (0700) and the file is written 0600. An
existing file is never replaced unless you pass --force.`,
		Example: `  t3fleet init
  t3fleet init --provider boat --boat-organization my-org --boat-snapshot snap-123
  t3fleet init --provider namespace --namespace-tenant-id my-tenant --namespace-image registered:org/t3-worker --no-input
  t3fleet init --provider boat --boat-organization my-org --boat-snapshot snap-123 --t3-binary /opt/t3/bin/t3 --print`,
		Args:              noArgs,
		ValidArgsFunction: cobra.NoFileCompletions,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.runInit(cmd.Context(), cmd, opts)
		},
	}
	flags := cmd.Flags()
	flags.SortFlags = false
	flags.StringSliceVarP(&opts.providers, "provider", "p", nil, "provider types in placement order: boat, namespace (repeat or comma-separate)")
	flags.StringVar(&opts.boatOrganization, "boat-organization", "", "Boat organization to bill (required for Boat)")
	flags.StringVar(&opts.boatSnapshot, "boat-snapshot", "", "prepared Boat snapshot used as the default template")
	flags.StringVar(&opts.namespaceTenantID, "namespace-tenant-id", "", "Namespace tenant ID for this instance (required for Namespace)")
	flags.StringVar(&opts.namespaceSite, "namespace-site", "", "Namespace site, such as iad (empty uses the provider default)")
	flags.StringVar(&opts.namespaceImage, "namespace-image", "", "prepared Namespace image, such as registered:org/t3-worker")
	flags.StringVar(&opts.t3Binary, "t3-binary", "", "absolute path to the local T3 server binary (default: t3 on PATH)")
	flags.StringVar(&opts.stateDir, "state-dir", "", "absolute private state directory (default "+config.Default().StateDir+")")
	flags.BoolVar(&opts.noInput, "no-input", false, "never prompt; use flags and defaults only")
	flags.BoolVar(&opts.force, "force", false, "replace an existing configuration file")
	flags.BoolVar(&opts.print, "print", false, "print the configuration instead of writing it")
	_ = cmd.RegisterFlagCompletionFunc("provider", cobra.FixedCompletions([]string{"boat", "namespace"}, cobra.ShellCompDirectiveNoFileComp))
	return cmd
}

func (a *app) runInit(ctx context.Context, cmd *cobra.Command, opts initOptions) error {
	path, source := a.configLocation()
	if !opts.print && !opts.force {
		if _, err := os.Lstat(path); err == nil {
			return hinted("edit it, pass --force to replace it, or choose another path with --config", "a configuration already exists at %s", path)
		}
	}
	interactive := a.stdinTTY && !opts.noInput
	var prompt func(label, current string) (string, error)
	if interactive {
		scanner := bufio.NewScanner(a.stdin)
		prompt = func(label, current string) (string, error) {
			if current != "" {
				fmt.Fprintf(a.stderr, "%s [%s]: ", label, current)
			} else {
				fmt.Fprintf(a.stderr, "%s: ", label)
			}
			line, err := readSelection(ctx, scanner)
			if err != nil {
				return "", err
			}
			if line = strings.TrimSpace(line); line == "" {
				return current, nil
			}
			return line, nil
		}
	}
	changed := cmd.Flags().Changed
	ask := func(flag, label string, value *string) error {
		if prompt == nil || changed(flag) {
			return nil
		}
		answer, err := prompt(label, *value)
		*value = answer
		return err
	}

	if len(opts.providers) == 0 {
		if prompt == nil {
			return hinted("pass --provider boat, --provider namespace, or both", "init needs at least one provider")
		}
		answer, err := prompt("Providers (boat, namespace, or both)", "boat")
		if err != nil {
			return err
		}
		opts.providers = strings.FieldsFunc(answer, func(r rune) bool { return r == ',' || r == ' ' })
	}
	providers, err := normalizeProviders(opts.providers)
	if err != nil {
		return err
	}

	cfg := config.Default()
	if opts.stateDir != "" {
		cfg.StateDir = opts.stateDir
	}
	if opts.t3Binary == "" {
		if binary, err := exec.LookPath("t3"); err == nil && filepath.IsAbs(binary) {
			opts.t3Binary = binary
		}
	}
	if err := ask("t3-binary", "Absolute path to the local T3 server binary", &opts.t3Binary); err != nil {
		return err
	}
	if !filepath.IsAbs(opts.t3Binary) {
		return hinted("install T3 or pass --t3-binary /absolute/path/to/t3", "init needs a local T3 server binary path")
	}
	cfg.Dispatch = &config.Dispatch{Listen: "127.0.0.1:47733", T3Binary: opts.t3Binary, ProviderOrder: providers}
	if slices.Contains(providers, "boat") {
		if err := ask("boat-organization", "Boat organization to bill", &opts.boatOrganization); err != nil {
			return err
		}
		if opts.boatOrganization == "" {
			return hinted("pass --boat-organization ORGANIZATION", "init needs a Boat organization")
		}
		if err := ask("boat-snapshot", "Prepared Boat snapshot", &opts.boatSnapshot); err != nil {
			return err
		}
		if opts.boatSnapshot == "" {
			return hinted("pass --boat-snapshot SNAPSHOT", "init needs a prepared Boat snapshot")
		}
		cfg.Providers["boat"] = config.ProviderConfig{Type: "boat", Template: provider.Template{Snapshot: opts.boatSnapshot}, Boat: config.Boat{Organization: opts.boatOrganization}}
	}
	if slices.Contains(providers, "namespace") {
		if err := ask("namespace-tenant-id", "Namespace tenant ID", &opts.namespaceTenantID); err != nil {
			return err
		}
		if opts.namespaceTenantID == "" {
			return hinted("pass --namespace-tenant-id TENANT_ID", "init needs a Namespace tenant ID")
		}
		if err := ask("namespace-site", "Namespace site (empty uses the provider default)", &opts.namespaceSite); err != nil {
			return err
		}
		if err := ask("namespace-image", "Prepared Namespace image (for example registered:org/t3-worker)", &opts.namespaceImage); err != nil {
			return err
		}
		if opts.namespaceImage == "" {
			return hinted("pass --namespace-image IMAGE", "init needs a prepared Namespace image")
		}
		cfg.Providers["namespace"] = config.ProviderConfig{Type: "namespace", Template: provider.Template{Image: opts.namespaceImage}, Namespace: config.Namespace{TenantID: opts.namespaceTenantID, Site: opts.namespaceSite}}
	}
	if err := cfg.Validate(); err != nil {
		return hinted("check the values passed to init", "the generated configuration is invalid: %v", err)
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if opts.print {
		_, err := a.stdout.Write(data)
		return err
	}
	if err := writePrivateFile(path, data, opts.force); err != nil {
		if errors.Is(err, os.ErrExist) {
			return hinted("pass --force to replace it", "a configuration already exists at %s", path)
		}
		return hinted("check the directory permissions, or choose another path with --config", "cannot write %s: %v", path, err)
	}
	fmt.Fprintf(a.stdout, "Wrote %s\n", path)
	a.printInitNextSteps(path, source, cfg)
	return nil
}

func normalizeProviders(values []string) ([]string, error) {
	var out []string
	for _, value := range values {
		name := strings.ToLower(strings.TrimSpace(value))
		switch name {
		case "":
			continue
		case "both":
			out = append(out, "boat", "namespace")
		case "boat", "namespace":
			out = append(out, name)
		default:
			return nil, hinted("use boat, namespace, or both", "unknown provider %q", value)
		}
	}
	seen := make(map[string]bool)
	for _, name := range out {
		if seen[name] {
			return nil, hinted("add other named instances in the configuration file", "init already includes %q", name)
		}
		seen[name] = true
	}
	if len(out) == 0 {
		return nil, hinted("use boat, namespace, or both", "init needs at least one provider")
	}
	return out, nil
}

// writePrivateFile writes data atomically with mode 0600. Without replace, it
// fails with os.ErrExist rather than overwrite an existing file.
func writePrivateFile(path string, data []byte, replace bool) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	temp, err := os.CreateTemp(dir, ".config-*.json")
	if err != nil {
		return err
	}
	name := temp.Name()
	defer func() { _ = os.Remove(name) }()
	_, writeErr := temp.Write(data)
	syncErr := temp.Sync()
	if err := errors.Join(writeErr, syncErr, temp.Close()); err != nil {
		return err
	}
	if replace {
		return os.Rename(name, path)
	}
	// A hard link fails when the target exists, so a concurrent init cannot
	// silently replace another file.
	return os.Link(name, path)
}

func (a *app) printInitNextSteps(path, source string, cfg config.Config) {
	w := a.stderr
	step := 0
	next := func(format string, args ...any) {
		step++
		fmt.Fprintf(w, "  %d. "+format+"\n", append([]any{step}, args...)...)
	}
	fmt.Fprintln(w, "Next steps:")
	if source == "--config" {
		next("Export T3FLEET_CONFIG=%s to skip --config on each command.", path)
	}
	if _, boat := cfg.Providers["boat"]; boat {
		next("Export BOAT_API_KEY in the shell that runs 'serve', or sign in with Boat once.")
	}
	if _, ns := cfg.Providers["namespace"]; ns {
		next("Sign in with the Namespace CLI (nsc login), or set NSC_TOKEN_FILE.")
	}
	next("Start the daemon: t3fleet serve")
	next("From another terminal, pair the fleet: t3fleet pair")
	next("Add a project in T3 and start a thread to allocate its worker.")
}
