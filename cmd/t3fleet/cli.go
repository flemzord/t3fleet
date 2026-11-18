package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/flemzord/t3fleet/internal/config"
	"github.com/flemzord/t3fleet/internal/fleet"
	"github.com/flemzord/t3fleet/pkg/provider"
)

var errDaemonCredentials = hinted("start 't3fleet serve' with the same configuration", "daemon credentials unavailable")

var errNoConfiguration = hinted("run 't3fleet init' to create one, then 't3fleet serve'", "no configuration file found")

// app carries the I/O and configuration shared by every command. Tests build
// one with a preloaded configuration and in-memory streams.
type app struct {
	stdout, stderr io.Writer
	stdin          io.Reader
	stdinTTY       bool
	load           func(string) (config.Config, error)
	cfg            *config.Config
	configPath     string
	// configMissing is set when the default configuration file does not exist
	// and built-in defaults are used instead.
	configMissing bool
}

func newApp(stdout, stderr io.Writer) *app {
	return &app{stdout: stdout, stderr: stderr, stdin: os.Stdin, stdinTTY: interactiveInput(), load: loadConfig}
}

func (a *app) execute(ctx context.Context, args []string) error {
	root := newRootCommand(a)
	root.SetArgs(args)
	return root.ExecuteContext(ctx)
}

// configLocation reports the configuration path and where it came from:
// --config, then T3FLEET_CONFIG, then the platform default.
func (a *app) configLocation() (string, string) {
	if a.configPath != "" {
		return a.configPath, "--config"
	}
	if path := os.Getenv("T3FLEET_CONFIG"); path != "" {
		return path, "T3FLEET_CONFIG"
	}
	return config.DefaultPath(), "default"
}

func (a *app) config() (config.Config, error) {
	if a.cfg != nil {
		return *a.cfg, nil
	}
	path, source := a.configLocation()
	cfg, err := a.load(path)
	if err != nil && source == "default" {
		// Only the implicit default may be absent: an explicit --config or
		// T3FLEET_CONFIG that does not exist is a mistake worth reporting.
		if _, statErr := os.Stat(path); errors.Is(statErr, os.ErrNotExist) {
			cfg, err = config.Default(), nil
			a.configMissing = true
		}
	}
	if err != nil {
		return config.Config{}, &hintError{msg: err.Error(), hint: fmt.Sprintf("configuration path: %s (from %s); set --config or T3FLEET_CONFIG", path, source)}
	}
	a.cfg = &cfg
	return cfg, nil
}

func (a *app) daemon() (daemonClient, error) {
	cfg, err := a.config()
	if err != nil {
		return daemonClient{}, err
	}
	token, err := readAdminToken(cfg.StateDir)
	if err != nil {
		if a.configMissing {
			return daemonClient{}, errNoConfiguration
		}
		return daemonClient{}, errDaemonCredentials
	}
	return daemonClient{cfg: cfg, token: token}, nil
}

func configuredProviders(cfg config.Config) []string {
	var names []string
	if cfg.Providers.Boat != nil {
		names = append(names, "boat")
	}
	if cfg.Providers.Namespace != nil {
		names = append(names, "namespace")
	}
	return names
}

func defaultProvider(cfg config.Config) (string, error) {
	names := configuredProviders(cfg)
	switch len(names) {
	case 1:
		return names[0], nil
	case 0:
		return "", hinted("add a provider to the configuration (see examples/config.json), or pass --provider", "no provider is configured")
	default:
		return "", hinted("pass --provider "+strings.Join(names, " or --provider "), "several providers are configured")
	}
}

func machineRef(m fleet.Machine) string {
	if m.Name != "" {
		return m.Name
	}
	return m.ID
}

func noArgs(cmd *cobra.Command, args []string) error {
	if len(args) > 0 {
		return hinted(fmt.Sprintf("run '%s --help'", cmd.CommandPath()), "%s takes no arguments (got %q)", cmd.CommandPath(), args[0])
	}
	return nil
}

func oneMachine(cmd *cobra.Command, args []string) error {
	if len(args) != 1 {
		return hinted("pass a machine name, full ID or ID prefix; run 't3fleet list' to see them", "%s needs exactly one machine", cmd.CommandPath())
	}
	return validateMachineRef(args[0])
}

func (a *app) completeMachines(cmd *cobra.Command, args []string, _ string) ([]string, cobra.ShellCompDirective) {
	if len(args) > 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	d, err := a.daemon()
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	parent := cmd.Context()
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, 2*time.Second)
	defer cancel()
	machines, err := d.machines(ctx)
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	completions := make([]string, 0, len(machines))
	for _, m := range machines {
		completions = append(completions, machineRef(m)+"\t"+strings.TrimSpace(m.Provider+" "+m.State))
	}
	return completions, cobra.ShellCompDirectiveNoFileComp
}

func newRootCommand(a *app) *cobra.Command {
	cobra.EnableCommandSorting = false
	defaultPath := config.DefaultPath()
	root := &cobra.Command{
		Use:   "t3fleet",
		Short: "Run T3 Code on persistent cloud machines through a local daemon",
		Long: `t3fleet runs T3 Code on persistent Boat sandboxes and Namespace Devboxes.

Start the daemon with 't3fleet serve', then manage machines from another
terminal. Designate a machine by name (with or without the t3fleet- prefix),
full ID, or a unique ID prefix of at least 4 characters.

Configuration is read from --config, then $T3FLEET_CONFIG, then
` + defaultPath + `.
Cloud machines keep billing until you stop or delete them, even when the
daemon is not running.`,
		Version:       version,
		SilenceErrors: true,
		SilenceUsage:  true,
	}
	root.SetVersionTemplate("t3fleet {{.Version}}\n")
	root.SetOut(a.stdout)
	root.SetErr(a.stderr)
	root.SetIn(a.stdin)
	root.PersistentFlags().StringVar(&a.configPath, "config", "", "configuration file (default $T3FLEET_CONFIG or "+defaultPath+")")
	_ = root.RegisterFlagCompletionFunc("config", func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
		return []string{"json"}, cobra.ShellCompDirectiveFilterFileExt
	})
	root.SetFlagErrorFunc(func(cmd *cobra.Command, err error) error {
		return hinted(fmt.Sprintf("run '%s --help'", cmd.CommandPath()), "%v", err)
	})
	root.AddGroup(
		&cobra.Group{ID: "machines", Title: "Machine commands:"},
		&cobra.Group{ID: "access", Title: "T3 Code access:"},
		&cobra.Group{ID: "daemon", Title: "Setup and daemon:"},
	)
	root.AddCommand(
		a.createCommand(), a.listCommand(), a.inspectCommand(),
		a.powerCommand("start"), a.powerCommand("stop"),
		a.deleteCommand(), a.snapshotCommand(),
		a.openCommand(), a.pairCommand(), a.dispatchCommand(), a.legacyPairDispatchCommand(),
		a.initCommand(), a.serveCommand(), a.statusCommand(),
		a.versionCommand(),
	)
	return root
}

func (a *app) serveCommand() *cobra.Command {
	return &cobra.Command{
		Use:     "serve",
		Short:   "Run the local fleet daemon",
		GroupID: "daemon",
		Long: `Run the local fleet daemon in the foreground.

The daemon loads cloud credentials, owns the state directory, and relays
T3 Code traffic to the machines. Other commands talk to it over loopback.
Stopping the daemon does not stop or delete cloud machines.`,
		Example:           "  t3fleet serve\n  t3fleet serve --config ~/.config/t3fleet/config.json",
		Args:              noArgs,
		ValidArgsFunction: cobra.NoFileCompletions,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := a.config()
			if err != nil {
				return err
			}
			if a.configMissing {
				return errNoConfiguration
			}
			return serve(cmd.Context(), cfg, a.stderr)
		},
	}
}

func (a *app) createCommand() *cobra.Command {
	var request fleet.CreateRequest
	var count, concurrency int
	var batchID, resume string
	var noRegister, asJSON bool
	cmd := &cobra.Command{
		Use:     "create --name NAME",
		Short:   "Create a machine from a template and open it in T3 Code",
		GroupID: "machines",
		Long: `Create a machine from the configured template, wait for T3 readiness,
then open T3 Code Desktop to confirm the association.

New names receive the t3fleet- prefix once; the full name is limited to 32
characters. --provider is optional when only one provider is configured.

With --count, t3fleet creates a durable batch named t3fleet-NAME-01,
t3fleet-NAME-02, ... and prints a batch ID. Resume an interrupted batch with
--resume BATCH_ID.`,
		Example: `  t3fleet create --name agent
  t3fleet create --name agent --provider namespace --size large
  t3fleet create --name agent --snapshot my-golden --no-register
  t3fleet create --name workers --count 10 --concurrency 3 --no-register
  t3fleet create --resume 0123456789abcdef0123456789abcdef`,
		Args:              noArgs,
		ValidArgsFunction: cobra.NoFileCompletions,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := a.config()
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			if resume != "" {
				for _, name := range []string{"name", "provider", "batch-id", "count", "size", "image", "snapshot", "source"} {
					if cmd.Flags().Changed(name) {
						return hinted("example: t3fleet create --resume BATCH_ID --concurrency 3", "--resume accepts only --concurrency, --no-register and --json (got --%s)", name)
					}
				}
				return runBatchWithInput(ctx, cfg, fleet.BatchRequest{ID: resume}, concurrency, true, !noRegister, asJSON, a.stdin, a.stdinTTY, a.stdout, a.stderr)
			}
			if request.Name == "" {
				return hinted("example: t3fleet create --name agent", "create needs --name")
			}
			if request.Provider == "" {
				if request.Provider, err = defaultProvider(cfg); err != nil {
					return err
				}
			}
			if count != 1 || batchID != "" {
				return runBatchWithInput(ctx, cfg, fleet.BatchRequest{ID: batchID, CreateRequest: request, Count: count}, concurrency, false, !noRegister, asJSON, a.stdin, a.stdinTTY, a.stdout, a.stderr)
			}
			if concurrency < 1 || concurrency > fleet.MaxConcurrency {
				return errors.New("--concurrency must be between 1 and 10")
			}
			return a.createOne(ctx, request, !noRegister, asJSON)
		},
	}
	flags := cmd.Flags()
	flags.SortFlags = false
	flags.StringVarP(&request.Name, "name", "n", "", "machine name")
	flags.StringVarP(&request.Provider, "provider", "p", "", "boat or namespace (default: the only configured provider)")
	flags.StringVar(&request.Size, "size", "", "provider machine size")
	flags.StringVar(&request.Template.Image, "image", "", "prepared image reference (overrides the template)")
	flags.StringVar(&request.Template.Snapshot, "snapshot", "", "prepared snapshot reference (overrides the template)")
	flags.StringVar(&request.Template.Source, "source", "", "golden Namespace Devbox to fork (overrides the template)")
	flags.BoolVar(&noRegister, "no-register", false, "do not open T3 Code; associate later with 't3fleet open'")
	flags.IntVar(&count, "count", 1, "number of identical machines, 1-100 (creates a batch)")
	flags.IntVar(&concurrency, "concurrency", 3, "maximum concurrent starts in a batch, 1-10")
	flags.StringVar(&batchID, "batch-id", "", "stable batch ID (32 hex characters) for request replay")
	flags.StringVar(&resume, "resume", "", "resume an existing batch by ID")
	flags.BoolVar(&asJSON, "json", false, "print the result as JSON")
	_ = cmd.RegisterFlagCompletionFunc("provider", func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
		cfg, err := a.config()
		if names := configuredProviders(cfg); err == nil && len(names) > 0 {
			return names, cobra.ShellCompDirectiveNoFileComp
		}
		return []string{"boat", "namespace"}, cobra.ShellCompDirectiveNoFileComp
	})
	return cmd
}

func (a *app) createOne(ctx context.Context, request fleet.CreateRequest, register, asJSON bool) error {
	d, err := a.daemon()
	if err != nil {
		return err
	}
	finish := startProgress(a.stderr, operationProgress(d.cfg, "create", request), interactiveProgress(a.stderr))
	data, err := d.call(ctx, http.MethodPost, "/v1/machines", request)
	finish(err)
	if err != nil {
		return err
	}
	var machine fleet.Machine
	if json.Unmarshal(data, &machine) != nil {
		return errors.New("invalid daemon response")
	}
	// Print the identity before the desktop handoff: a failed handoff must
	// still leave the caller with the created machine.
	if asJSON {
		if err := printRawJSON(a.stdout, data); err != nil {
			return err
		}
	} else {
		fmt.Fprintf(a.stdout, "Created %s (%s) on %s\n", displayName(machine), machine.ID, orDash(machine.Provider))
	}
	if !register {
		if !asJSON {
			fmt.Fprintf(a.stderr, "Open it in T3 Code with: t3fleet open %s\n", machineRef(machine))
		}
		return nil
	}
	return registerCreatedMachine(ctx, d.cfg, d.token, data, a.stderr)
}

func (a *app) listCommand() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:               "list",
		Aliases:           []string{"ls"},
		Short:             "List managed machines",
		GroupID:           "machines",
		Example:           "  t3fleet list\n  t3fleet list --json | jq -r '.[].id'",
		Args:              noArgs,
		ValidArgsFunction: cobra.NoFileCompletions,
		RunE: func(cmd *cobra.Command, _ []string) error {
			d, err := a.daemon()
			if err != nil {
				return err
			}
			data, err := d.call(cmd.Context(), http.MethodGet, "/v1/machines", nil)
			if err != nil {
				return err
			}
			if asJSON {
				return printRawJSON(a.stdout, data)
			}
			var machines []fleet.Machine
			if json.Unmarshal(data, &machines) != nil {
				return errors.New("invalid daemon response")
			}
			if len(machines) == 0 {
				fmt.Fprintln(a.stderr, "No managed machines. Create one with: t3fleet create --name NAME")
				return nil
			}
			printMachineTable(a.stdout, machines, colorEnabled(a.stdout))
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print machines as JSON")
	return cmd
}

func (a *app) inspectCommand() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:               "inspect MACHINE",
		Short:             "Show details of one machine",
		GroupID:           "machines",
		Example:           "  t3fleet inspect agent\n  t3fleet inspect 3f9c --json",
		Args:              oneMachine,
		ValidArgsFunction: a.completeMachines,
		RunE: func(cmd *cobra.Command, args []string) error {
			d, err := a.daemon()
			if err != nil {
				return err
			}
			machine, err := d.resolve(cmd.Context(), args[0], true)
			if err != nil {
				return err
			}
			data, err := d.call(cmd.Context(), http.MethodGet, "/v1/machines/"+url.PathEscape(machine.ID), nil)
			if err != nil {
				return err
			}
			if asJSON {
				return printRawJSON(a.stdout, data)
			}
			if json.Unmarshal(data, &machine) != nil {
				return errors.New("invalid daemon response")
			}
			printMachineDetails(a.stdout, machine, colorEnabled(a.stdout))
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the machine as JSON")
	return cmd
}

func (a *app) powerCommand(action string) *cobra.Command {
	var asJSON bool
	short, example := "Start a machine and wait for T3 readiness", "  t3fleet start agent"
	if action == "stop" {
		short, example = "Stop a machine", "  t3fleet stop agent"
	}
	cmd := &cobra.Command{
		Use:               action + " MACHINE",
		Short:             short,
		GroupID:           "machines",
		Example:           example,
		Args:              oneMachine,
		ValidArgsFunction: a.completeMachines,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			d, err := a.daemon()
			if err != nil {
				return err
			}
			machine, err := d.resolve(ctx, args[0], true)
			if err != nil {
				return err
			}
			path := "/v1/machines/" + url.PathEscape(machine.ID)
			finish := startProgress(a.stderr, machineProgress(action, displayName(machine)), interactiveProgress(a.stderr))
			_, err = d.call(ctx, http.MethodPost, path+"/"+action, nil)
			finish(err)
			if err != nil || !asJSON {
				return err
			}
			data, err := d.call(ctx, http.MethodGet, path, nil)
			if err != nil {
				return err
			}
			return printRawJSON(a.stdout, data)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the resulting machine as JSON")
	return cmd
}

func (a *app) deleteCommand() *cobra.Command {
	var opts deleteOptions
	cmd := &cobra.Command{
		Use:     "delete [MACHINE]",
		Aliases: []string{"rm"},
		Short:   "Delete one machine, or select several interactively",
		GroupID: "machines",
		Long: `Permanently delete managed machines and their cloud resources.

With one argument, delete that machine. The argument must be a full ID or an
exact name: ID prefixes are refused for deletion.
Without an argument, select machines from a list and confirm by typing delete.
In scripts, pass explicit managed IDs with --ids and confirm with --yes.`,
		Example: `  t3fleet delete agent
  t3fleet delete
  t3fleet delete --ids ID1,ID2 --yes`,
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) > 1 {
				return hinted("pass one machine, or use --ids ID,ID --yes for several", "delete accepts at most one machine")
			}
			if len(args) == 1 {
				return validateMachineRef(args[0])
			}
			return nil
		},
		ValidArgsFunction: a.completeMachines,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 {
				return a.deleteOne(cmd.Context(), args[0], opts)
			}
			return a.deleteMany(cmd.Context(), opts)
		},
	}
	cmd.Flags().StringVar(&opts.ids, "ids", "", "comma-separated managed machine IDs")
	cmd.Flags().BoolVarP(&opts.yes, "yes", "y", false, "confirm exactly the IDs given with --ids")
	cmd.Flags().BoolVar(&opts.jsonOutput, "json", false, "print deletion results as JSON")
	return cmd
}

func (a *app) snapshotCommand() *cobra.Command {
	var name string
	var asJSON bool
	cmd := &cobra.Command{
		Use:               "snapshot MACHINE --name NAME",
		Short:             "Capture a provider snapshot of a machine",
		GroupID:           "machines",
		Long:              "Capture a snapshot of a machine where the provider supports it (Boat).\nUse the snapshot later with 't3fleet create --snapshot'.",
		Example:           "  t3fleet snapshot agent --name golden-2026-11",
		Args:              oneMachine,
		ValidArgsFunction: a.completeMachines,
		RunE: func(cmd *cobra.Command, args []string) error {
			if name == "" {
				return hinted("example: t3fleet snapshot "+args[0]+" --name golden", "snapshot needs --name")
			}
			ctx := cmd.Context()
			d, err := a.daemon()
			if err != nil {
				return err
			}
			machine, err := d.resolve(ctx, args[0], true)
			if err != nil {
				return err
			}
			finish := startProgress(a.stderr, machineProgress("snapshot", displayName(machine)), interactiveProgress(a.stderr))
			data, err := d.call(ctx, http.MethodPost, "/v1/machines/"+url.PathEscape(machine.ID)+"/snapshot", map[string]string{"name": name})
			finish(err)
			if err != nil {
				return err
			}
			if asJSON {
				return printRawJSON(a.stdout, data)
			}
			var snapshot provider.Snapshot
			if json.Unmarshal(data, &snapshot) != nil {
				return errors.New("invalid daemon response")
			}
			status := "ready"
			if !snapshot.Ready {
				status = "saved; the provider is still preparing it"
			}
			fmt.Fprintf(a.stdout, "Snapshot %s (%s) %s\n", orDash(snapshot.Name), orDash(snapshot.ID), status)
			return nil
		},
	}
	cmd.Flags().StringVarP(&name, "name", "n", "", "snapshot name (required)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the snapshot as JSON")
	return cmd
}

func (a *app) openCommand() *cobra.Command {
	return &cobra.Command{
		Use:     "open MACHINE",
		Short:   "Open a machine in T3 Code Desktop",
		GroupID: "access",
		Long: `Prepare a one-time pairing link and hand it to T3 Code Desktop on this
computer. Confirm the association in T3 to finish. Use 'pair' instead on a
headless host.`,
		Example:           "  t3fleet open agent",
		Args:              oneMachine,
		ValidArgsFunction: a.completeMachines,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			d, err := a.daemon()
			if err != nil {
				return err
			}
			machine, err := d.resolve(ctx, args[0], true)
			if err != nil {
				return err
			}
			payload, err := json.Marshal(map[string]string{"id": machine.ID})
			if err != nil {
				return err
			}
			finish := startProgress(a.stderr, machineProgress("pair", displayName(machine)), interactiveProgress(a.stderr))
			// The handoff writes nothing while the spinner runs; report afterwards.
			err = registerCreatedMachine(ctx, d.cfg, d.token, payload, io.Discard)
			finish(err)
			if err != nil {
				return err
			}
			fmt.Fprintln(a.stderr, "T3 Code opened. Confirm the backend association in T3 to finish.")
			return nil
		},
	}
}

func (a *app) pairCommand() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:     "pair MACHINE",
		Short:   "Print a one-time T3 Code pairing link",
		GroupID: "access",
		Long: `Print a short-lived T3 Code pairing deep link for a machine. Use it on a
headless host, or when 'open' cannot launch T3 Code Desktop.
Keep the link private: it grants access to the machine until it expires.`,
		Example:           "  t3fleet pair agent",
		Args:              oneMachine,
		ValidArgsFunction: a.completeMachines,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			d, err := a.daemon()
			if err != nil {
				return err
			}
			machine, err := d.resolve(ctx, args[0], true)
			if err != nil {
				return err
			}
			finish := startProgress(a.stderr, machineProgress("pair", displayName(machine)), interactiveProgress(a.stderr))
			data, err := d.call(ctx, http.MethodPost, "/v1/machines/"+url.PathEscape(machine.ID)+"/pair", nil)
			finish(err)
			if err != nil {
				return err
			}
			link, err := pairingAppLink(data)
			if err != nil {
				return err
			}
			if asJSON {
				return printJSON(a.stdout, map[string]string{"link": link})
			}
			_, err = fmt.Fprintln(a.stdout, link)
			return err
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the link as JSON")
	return cmd
}

func (a *app) dispatchCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "dispatch",
		Short:   "Experimental dispatch commands",
		GroupID: "access",
		Long:    "Commands for the experimental dispatch endpoint (fixed pools and on-demand workers).\nEnable it with dispatchPOC in the configuration; see docs/dispatch-poc.md.",
		Args:    noArgs,
	}
	cmd.AddCommand(a.dispatchPairCommand("pair"))
	return cmd
}

func (a *app) legacyPairDispatchCommand() *cobra.Command {
	cmd := a.dispatchPairCommand("pair-dispatch")
	cmd.Hidden = true
	cmd.GroupID = ""
	// cobra's Deprecated notice goes to the output stream; keep stdout for the link.
	runE := cmd.RunE
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		fmt.Fprintln(a.stderr, "Warning: 'pair-dispatch' is deprecated; use 't3fleet dispatch pair'.")
		return runE(cmd, args)
	}
	return cmd
}

func (a *app) dispatchPairCommand(use string) *cobra.Command {
	var peerAccess string
	var asJSON bool
	cmd := &cobra.Command{
		Use:   use,
		Short: "Print the dispatch pairing link, or a Linked Environments code",
		Long: `Print a T3 Code Desktop pairing link for the dispatch endpoint.
With --peer-access, print a Linked Environments code instead, for launching
on-demand threads from another T3 server.`,
		Example:           "  t3fleet dispatch pair\n  t3fleet dispatch pair --peer-access LEVEL",
		Args:              noArgs,
		ValidArgsFunction: cobra.NoFileCompletions,
		RunE: func(cmd *cobra.Command, _ []string) error {
			d, err := a.daemon()
			if err != nil {
				return err
			}
			path, body := "/v1/dispatch-poc/pair", any(nil)
			if peerAccess != "" {
				path, body = "/v1/dispatch-poc/pair-peer", map[string]string{"access": peerAccess}
			}
			data, err := d.call(cmd.Context(), http.MethodPost, path, body)
			if hasDaemonCode(err, "not_found") {
				return hinted("enable dispatchPOC in the configuration and restart 't3fleet serve'", "dispatch is not enabled on this daemon")
			}
			if err != nil {
				return err
			}
			if peerAccess == "" {
				link, err := pairingAppLink(data)
				if err != nil {
					return err
				}
				if asJSON {
					return printJSON(a.stdout, map[string]string{"link": link})
				}
				_, err = fmt.Fprintln(a.stdout, link)
				return err
			}
			var reply struct{ Credential string }
			if json.Unmarshal(data, &reply) != nil || reply.Credential == "" {
				return errors.New("invalid peer pairing response")
			}
			if asJSON {
				return printJSON(a.stdout, map[string]string{"credential": reply.Credential})
			}
			_, err = fmt.Fprintln(a.stdout, reply.Credential)
			return err
		},
	}
	cmd.Flags().StringVar(&peerAccess, "peer-access", "", "issue a Linked Environments code at this access level")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the result as JSON")
	return cmd
}

type statusReport struct {
	Config       string         `json:"config"`
	ConfigSource string         `json:"configSource"`
	ConfigFound  bool           `json:"configFound"`
	StateDir     string         `json:"stateDir"`
	Listen       string         `json:"listen"`
	Providers    []string       `json:"providers"`
	Daemon       daemonStatus   `json:"daemon"`
	Machines     map[string]int `json:"machines,omitempty"`
	Total        int            `json:"total"`
}

type daemonStatus struct {
	Running bool   `json:"running"`
	Version string `json:"version,omitempty"`
}

func (a *app) statusCommand() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:               "status",
		Short:             "Show the configuration, daemon and machine summary",
		GroupID:           "daemon",
		Long:              "Show which configuration is in use, whether the daemon answers, and how many\nmachines it manages by state. Exits with an error when the daemon is not running.",
		Example:           "  t3fleet status\n  t3fleet status --json",
		Args:              noArgs,
		ValidArgsFunction: cobra.NoFileCompletions,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := a.config()
			if err != nil {
				return err
			}
			path, source := a.configLocation()
			report := statusReport{Config: path, ConfigSource: source, ConfigFound: !a.configMissing, StateDir: cfg.StateDir, Listen: cfg.Listen, Providers: configuredProviders(cfg)}
			ctx, cancel := context.WithTimeout(cmd.Context(), 3*time.Second)
			defer cancel()
			if data, err := callAdmin(ctx, cfg.Listen, "", http.MethodGet, "/_info", nil); err == nil {
				var info struct{ Version string }
				_ = json.Unmarshal(data, &info)
				report.Daemon = daemonStatus{Running: true, Version: info.Version}
				if token, err := readAdminToken(cfg.StateDir); err == nil {
					if machines, err := (daemonClient{cfg: cfg, token: token}).machines(ctx); err == nil {
						report.Machines = map[string]int{}
						for _, m := range machines {
							report.Machines[m.State]++
						}
						report.Total = len(machines)
					}
				}
			}
			if asJSON {
				if err := printJSON(a.stdout, report); err != nil {
					return err
				}
			} else {
				a.printStatus(report)
			}
			if !report.Daemon.Running {
				if a.configMissing {
					return errNoConfiguration
				}
				return hinted("start it with 't3fleet serve' using the same configuration", "the fleet daemon is not running on %s", cfg.Listen)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the status as JSON")
	return cmd
}

func (a *app) printStatus(r statusReport) {
	color := colorEnabled(a.stdout)
	tw := tabwriter.NewWriter(a.stdout, 0, 0, 2, ' ', 0)
	if r.ConfigFound {
		fmt.Fprintf(tw, "Config:\t%s (%s)\n", r.Config, r.ConfigSource)
	} else {
		fmt.Fprintf(tw, "Config:\t%s\n", styleIf(color, "33", r.Config+" not found; using defaults (run 't3fleet init')"))
	}
	fmt.Fprintf(tw, "State dir:\t%s\n", r.StateDir)
	providers := strings.Join(r.Providers, ", ")
	if providers == "" {
		providers = "none configured"
	}
	fmt.Fprintf(tw, "Providers:\t%s\n", providers)
	if r.Daemon.Running {
		daemon := "running on " + r.Listen
		if r.Daemon.Version != "" {
			daemon += ", version " + r.Daemon.Version
			if r.Daemon.Version != version {
				daemon += " (CLI " + version + ")"
			}
		}
		fmt.Fprintf(tw, "Daemon:\t%s\n", styleIf(color, "32", daemon))
	} else {
		fmt.Fprintf(tw, "Daemon:\t%s\n", styleIf(color, "31", "not running on "+r.Listen))
	}
	if r.Machines != nil {
		fmt.Fprintf(tw, "Machines:\t%s\n", machineSummary(r.Machines, r.Total))
	}
	tw.Flush()
}

func machineSummary(byState map[string]int, total int) string {
	if total == 0 {
		return "none"
	}
	states := make([]string, 0, len(byState))
	for state := range byState {
		states = append(states, state)
	}
	slices.Sort(states)
	parts := make([]string, 0, len(states))
	for _, state := range states {
		parts = append(parts, fmt.Sprintf("%d %s", byState[state], state))
	}
	return fmt.Sprintf("%d (%s)", total, strings.Join(parts, ", "))
}

func styleIf(color bool, code, text string) string {
	if !color {
		return text
	}
	return "\x1b[" + code + "m" + text + "\x1b[0m"
}

func (a *app) versionCommand() *cobra.Command {
	return &cobra.Command{
		Use:               "version",
		Short:             "Print the CLI version",
		Args:              noArgs,
		ValidArgsFunction: cobra.NoFileCompletions,
		RunE: func(*cobra.Command, []string) error {
			_, err := fmt.Fprintln(a.stdout, "t3fleet", version)
			return err
		},
	}
}
