package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
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
	boatSize          string
	namespaceSite     string
	namespaceSize     string
	namespaceSource   string
	namespaceSnapshot string
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
do not provide. The file holds no secrets: 'serve' reads cloud credentials from
the environment or the provider's own login when it starts.

The directory is created private (0700) and the file is written 0600. An
existing file is never replaced unless you pass --force.`,
		Example: `  t3fleet init
  t3fleet init --provider boat --boat-organization my-org --boat-snapshot snap-123 --boat-size small
  t3fleet init --provider namespace --namespace-source golden-devbox --no-input
  t3fleet init --provider boat --print`,
		Args:              noArgs,
		ValidArgsFunction: cobra.NoFileCompletions,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.runInit(cmd.Context(), cmd, opts)
		},
	}
	flags := cmd.Flags()
	flags.SortFlags = false
	flags.StringSliceVarP(&opts.providers, "provider", "p", nil, "provider to configure: boat, namespace (repeat or comma-separate for both)")
	flags.StringVar(&opts.boatOrganization, "boat-organization", "", "Boat organization to bill (empty follows the current Boat wallet)")
	flags.StringVar(&opts.boatSnapshot, "boat-snapshot", "", "prepared Boat snapshot used as the default template")
	flags.StringVar(&opts.boatSize, "boat-size", "", "default Boat size: small, default, large, xlarge")
	flags.StringVar(&opts.namespaceSite, "namespace-site", "", "Namespace site, such as iad (empty uses the provider default)")
	flags.StringVar(&opts.namespaceSize, "namespace-size", "", "default Namespace size: s, m, l, xl")
	flags.StringVar(&opts.namespaceSource, "namespace-source", "", "golden Namespace Devbox used as the default template")
	flags.StringVar(&opts.namespaceSnapshot, "namespace-snapshot", "", "completed snapshot of the golden Devbox (empty uses the latest)")
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
	if changed("boat-size") && !slices.Contains(providers, "boat") {
		return errors.New("--boat-size requires --provider boat")
	}
	if changed("namespace-size") && !slices.Contains(providers, "namespace") {
		return errors.New("--namespace-size requires --provider namespace")
	}

	cfg := config.Default()
	if opts.stateDir != "" {
		cfg.StateDir = opts.stateDir
	}
	if slices.Contains(providers, "boat") {
		if err := ask("boat-organization", "Boat organization (empty follows the current Boat wallet)", &opts.boatOrganization); err != nil {
			return err
		}
		if err := ask("boat-snapshot", "Prepared Boat snapshot (empty to set later)", &opts.boatSnapshot); err != nil {
			return err
		}
		if err := ask("boat-size", "Boat size (small, default, large, xlarge; empty uses Boat's default)", &opts.boatSize); err != nil {
			return err
		}
		cfg.Providers.Boat = &config.Boat{APIKeyEnv: "BOAT_API_KEY", Organization: opts.boatOrganization, Size: opts.boatSize}
		if opts.boatSnapshot != "" {
			cfg.Templates["boat"] = provider.Template{Snapshot: opts.boatSnapshot}
		}
	}
	if slices.Contains(providers, "namespace") {
		if err := ask("namespace-site", "Namespace site (empty uses the provider default)", &opts.namespaceSite); err != nil {
			return err
		}
		if err := ask("namespace-source", "Golden Namespace Devbox (empty to set later)", &opts.namespaceSource); err != nil {
			return err
		}
		if opts.namespaceSource != "" {
			if err := ask("namespace-snapshot", "Completed snapshot ID (empty uses the latest)", &opts.namespaceSnapshot); err != nil {
				return err
			}
		}
		if opts.namespaceSnapshot != "" && opts.namespaceSource == "" {
			return hinted("pass --namespace-source with --namespace-snapshot", "a Namespace snapshot requires its golden Devbox")
		}
		if err := ask("namespace-size", "Namespace size (s, m, l, xl; empty uses the default CPU and memory)", &opts.namespaceSize); err != nil {
			return err
		}
		cfg.Providers.Namespace = &config.Namespace{Site: opts.namespaceSite, Size: opts.namespaceSize}
		if opts.namespaceSource != "" {
			cfg.Templates["namespace"] = provider.Template{Source: opts.namespaceSource, Snapshot: opts.namespaceSnapshot}
		}
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
	slices.Sort(out)
	out = slices.Compact(out)
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
	if cfg.Providers.Boat != nil {
		next("Export BOAT_API_KEY in the shell that runs 'serve', or sign in with Boat once.")
		if cfg.Templates["boat"] == (provider.Template{}) {
			next("Set templates.boat.snapshot to a prepared t3fleet snapshot, or pass --snapshot to create.")
		}
	}
	if cfg.Providers.Namespace != nil {
		next("Sign in with the Namespace CLI (nsc login), or set NSC_TOKEN_FILE.")
		if cfg.Templates["namespace"] == (provider.Template{}) {
			next("Set templates.namespace to a prepared golden Devbox, or pass --source to create.")
		}
	}
	next("Start the daemon: t3fleet serve")
	next("From another terminal: t3fleet create --name my-agent")
}
