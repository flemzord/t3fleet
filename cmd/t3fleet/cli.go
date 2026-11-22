package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
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
	return slices.Sorted(maps.Keys(cfg.Providers))
}

func defaultProvider(cfg config.Config) (string, error) {
	names := configuredProviders(cfg)
	switch len(names) {
	case 1:
		return names[0], nil
	case 0:
		return "", hinted("add a named instance under providers in the configuration", "no provider is configured")
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
		Short: "Run one T3 environment backed by on-demand cloud workers",
		Long: `t3fleet runs one T3 environment. Pair it once with T3 Code Desktop or
another T3 server. Register projects from T3; starting a thread allocates
a worker using the provider order in the configuration.

Start with 't3fleet init', 't3fleet serve', then 't3fleet pair' from
another terminal. Stopping the daemon does not release allocated workers.

Configuration is read from --config, then $T3FLEET_CONFIG, then
` + defaultPath + `.`,
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
		&cobra.Group{ID: "daemon", Title: "Fleet service:"},
		&cobra.Group{ID: "machines", Title: "Worker inventory and recovery:"},
	)
	root.AddCommand(
		a.initCommand(), a.validateCommand(), a.serveCommand(), a.fleetPairCommand(), a.statusCommand(),
		a.listCommand(), a.inspectCommand(), a.adminCommand(), a.versionCommand(),
	)
	return root
}

func (a *app) validateCommand() *cobra.Command {
	return &cobra.Command{
		Use: "validate", Short: "Check the fleet configuration without contacting providers",
		GroupID: "daemon", Args: noArgs, ValidArgsFunction: cobra.NoFileCompletions,
		RunE: func(*cobra.Command, []string) error {
			if _, err := a.config(); err != nil {
				return err
			}
			if a.configMissing {
				return errNoConfiguration
			}
			path, _ := a.configLocation()
			fmt.Fprintf(a.stdout, "Valid configuration: %s\n", path)
			return nil
		},
	}
}

func (a *app) adminCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use: "admin", Short: "Recover or maintain individual workers", GroupID: "machines",
		Long: "Operate on individual machines directly. New T3 threads allocate workers automatically.",
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	cmd.AddCommand(a.createCommand(), a.deleteCommand(), a.powerCommand("start"),
		a.powerCommand("stop"), a.snapshotCommand(), a.machinePairCommand(false), a.machinePairCommand(true))
	return cmd
}

func (a *app) serveCommand() *cobra.Command {
	return &cobra.Command{
		Use:     "serve",
		Short:   "Run the local fleet daemon",
		GroupID: "daemon",
		Long: `Run the local fleet daemon in the foreground.

Start the local T3 server and expose the fleet endpoint for paired clients.
Worker allocation starts when a thread needs one. Other CLI commands use
the separate loopback admin endpoint. Stopping the daemon does not delete workers.`,
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
	var request machineCreateRequest
	var asJSON bool
	cmd := &cobra.Command{
		Use:               "create --name NAME",
		Short:             "Allocate one machine for manual recovery or diagnostics",
		Long:              "Allocate one machine with the selected provider instance's configured size and template. This machine is independent of on-demand thread placement.",
		Example:           "  t3fleet admin create --name diagnostics --provider primary",
		Args:              noArgs,
		ValidArgsFunction: cobra.NoFileCompletions,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if request.Name == "" {
				return hinted("example: t3fleet admin create --name diagnostics", "create needs --name")
			}
			cfg, err := a.config()
			if err != nil {
				return err
			}
			if request.Provider == "" {
				if request.Provider, err = defaultProvider(cfg); err != nil {
					return err
				}
			}
			if _, ok := cfg.Providers[request.Provider]; !ok {
				return hinted("select a configured provider instance", "unknown provider %q", request.Provider)
			}
			d, err := a.daemon()
			if err != nil {
				return err
			}
			finish := startProgress(a.stderr, createProgress(request.Provider, cfg.Providers[request.Provider].Template), interactiveProgress(a.stderr))
			data, err := d.call(cmd.Context(), http.MethodPost, "/v1/machines", request)
			finish(err)
			if err != nil {
				return err
			}
			if asJSON {
				return printRawJSON(a.stdout, data)
			}
			var machine fleet.Machine
			if json.Unmarshal(data, &machine) != nil || machine.ID == "" {
				return errors.New("invalid daemon response")
			}
			fmt.Fprintf(a.stdout, "Created %s (%s) on %s\n", displayName(machine), machine.ID, orDash(machine.Provider))
			return nil
		},
	}
	cmd.Flags().StringVarP(&request.Name, "name", "n", "", "machine name")
	cmd.Flags().StringVarP(&request.Provider, "provider", "p", "", "provider instance (default: the only configured instance)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the result as JSON")
	_ = cmd.RegisterFlagCompletionFunc("provider", func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
		cfg, _ := a.config()
		return configuredProviders(cfg), cobra.ShellCompDirectiveNoFileComp
	})
	return cmd
}

func (a *app) listCommand() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:               "list",
		Aliases:           []string{"ls"},
		Short:             "List managed machines with their fleet and live provider states",
		GroupID:           "machines",
		Example:           "  t3fleet list\n  t3fleet list --json | jq -r '.[].id'",
		Args:              noArgs,
		ValidArgsFunction: cobra.NoFileCompletions,
		RunE: func(cmd *cobra.Command, _ []string) error {
			d, err := a.daemon()
			if err != nil {
				return err
			}
			data, err := d.inventory(cmd.Context())
			if err != nil {
				return err
			}
			if asJSON {
				return printRawJSON(a.stdout, data)
			}
			machines, err := parseInventory(data)
			if err != nil {
				return err
			}
			if len(machines) == 0 {
				fmt.Fprintln(a.stderr, "No workers yet. Start a thread in the paired T3 environment to allocate one.")
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
	short, example := "Start a machine and wait for T3 readiness", "  t3fleet admin start agent"
	if action == "stop" {
		short, example = "Stop a machine", "  t3fleet admin stop agent"
	}
	cmd := &cobra.Command{
		Use:               action + " MACHINE",
		Short:             short,
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
		Long: `Permanently delete managed machines and their cloud resources.

With one argument, delete that machine. The argument must be a full ID or an
exact name: ID prefixes are refused for deletion.
Without an argument, use the arrow keys to move and Space to check or uncheck
workers. Press Enter to review the selection, then type delete to confirm.
Esc or Ctrl+C cancels the selection.
In scripts, pass explicit managed IDs with --ids and confirm with --yes.`,
		Example: `  t3fleet admin delete agent
  t3fleet admin delete
  t3fleet admin delete --ids ID1,ID2 --yes`,
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
		Long:              "Capture a snapshot of a machine where the provider supports it (Boat).\nTo use the snapshot for new workers, update the provider template in the configuration.",
		Example:           "  t3fleet admin snapshot agent --name golden-2026-11",
		Args:              oneMachine,
		ValidArgsFunction: a.completeMachines,
		RunE: func(cmd *cobra.Command, args []string) error {
			if name == "" {
				return hinted("example: t3fleet admin snapshot "+args[0]+" --name golden", "snapshot needs --name")
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

// Machine links are recovery tools. The user-facing pair command always targets the fleet.
func (a *app) machinePairCommand(open bool) *cobra.Command {
	var asJSON bool
	name, short := "pair", "Print a one-time link to this machine"
	if open {
		name, short = "open", "Open this machine directly in T3 Code Desktop"
	}
	cmd := &cobra.Command{
		Use: name + " MACHINE", Short: short,
		Example: "  t3fleet admin " + name + " diagnostics", Args: oneMachine,
		ValidArgsFunction: a.completeMachines,
		RunE: func(cmd *cobra.Command, args []string) error {
			d, err := a.daemon()
			if err != nil {
				return err
			}
			ctx := cmd.Context()
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
			if open {
				if err := openT3(ctx, link); err != nil {
					// Launcher errors can include the credential-bearing URL.
					return hinted("use 't3fleet admin pair MACHINE' to print a new link", "T3 Code Desktop could not be opened")
				}
				fmt.Fprintln(a.stderr, "Confirm the machine association in T3 Code Desktop.")
				return nil
			}
			if asJSON {
				return printJSON(a.stdout, map[string]string{"link": link})
			}
			_, err = fmt.Fprintln(a.stdout, link)
			return err
		},
	}
	if !open {
		cmd.Flags().BoolVar(&asJSON, "json", false, "print the link as JSON")
	}
	return cmd
}

func (a *app) fleetPairCommand() *cobra.Command {
	var peerAccess string
	var asJSON, external bool
	cmd := &cobra.Command{
		Use:     "pair",
		GroupID: "daemon",
		Short:   "Pair the fleet with T3 Code Desktop or another T3 server",
		Long: `Print a T3 Code Desktop pairing link for the fleet.
With --peer-access, print a Linked Environments code instead, for launching
on-demand threads from another T3 server. Keep the link or code private.`,
		Example:           "  t3fleet pair\n  t3fleet pair --external\n  t3fleet pair --peer-access approval-required",
		Args:              noArgs,
		ValidArgsFunction: cobra.NoFileCompletions,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if external && peerAccess != "" {
				return errors.New("--external selects a Desktop pairing URL; use the configured HTTPS URL with the peer code")
			}
			d, err := a.daemon()
			if err != nil {
				return err
			}
			path, body := "/v1/dispatch/pair", any(nil)
			if peerAccess != "" {
				path, body = "/v1/dispatch/pair-peer", map[string]string{"access": peerAccess}
			} else if external {
				path = "/v1/dispatch/pair-external"
			}
			data, err := d.call(cmd.Context(), http.MethodPost, path, body)
			if hasDaemonCode(err, "not_found") {
				return hinted("check the daemon version and restart 't3fleet serve' using the same configuration", "the fleet pairing endpoint is unavailable")
			}
			if err != nil {
				return err
			}
			if peerAccess == "" {
				link, err := pairingAppLinkForOrigin(data, external)
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
	cmd.Flags().StringVar(&peerAccess, "peer-access", "", "issue a Linked Environments code: read-only, approval-required, auto-accept-edits, auto, or full-access")
	cmd.Flags().BoolVar(&external, "external", false, "use the configured HTTPS origin for Desktop pairing")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the result as JSON")
	return cmd
}

type statusReport struct {
	FleetListen    string                      `json:"fleetListen,omitempty"`
	ProviderOrder  []string                    `json:"providerOrder,omitempty"`
	MaxWorkers     int                         `json:"maxWorkers,omitempty"`
	Config         string                      `json:"config"`
	ConfigSource   string                      `json:"configSource"`
	ConfigFound    bool                        `json:"configFound"`
	StateDir       string                      `json:"stateDir"`
	Listen         string                      `json:"listen"`
	Providers      []string                    `json:"providers"`
	Daemon         daemonStatus                `json:"daemon"`
	Machines       map[string]int              `json:"machines,omitempty"`
	Total          int                         `json:"total"`
	ProviderStates map[string]*providerSummary `json:"providerStates,omitempty"`
}

type providerSummary struct {
	States     map[string]int `json:"states"`
	Total      int            `json:"total"`
	ObservedAt time.Time      `json:"observedAt,omitzero"`
}

func (r *statusReport) summarize(machines []fleet.ObservedMachine) {
	r.Total = len(machines)
	r.Machines = make(map[string]int)
	r.ProviderStates = make(map[string]*providerSummary)
	for _, m := range machines {
		r.Machines[m.State]++
		if m.ProviderState == "" {
			continue // Unsupported provider, or an older daemon.
		}
		s := r.ProviderStates[m.Provider]
		if s == nil {
			s = &providerSummary{States: make(map[string]int)}
			r.ProviderStates[m.Provider] = s
		}
		s.Total++
		s.States[m.ProviderState]++
		if m.ProviderObservedAt.After(s.ObservedAt) {
			s.ObservedAt = m.ProviderObservedAt
		}
	}
}

func (a *app) readInventorySummary(ctx context.Context, report *statusReport) error {
	d, err := a.daemon()
	if err != nil {
		return err
	}
	data, err := d.inventory(ctx)
	if err != nil {
		return err
	}
	machines, err := parseInventory(data)
	if err != nil {
		return err
	}
	report.summarize(machines)
	return nil
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
		Long:              "Show settings from the selected file, whether the daemon answers, and counts by\nfleet state and live provider state. Restart the daemon after editing the file. Exits with an error when the daemon is not running.",
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
			if d := cfg.Dispatch; d != nil && !a.configMissing {
				report.FleetListen, report.ProviderOrder, report.MaxWorkers = d.Listen, d.ProviderOrder, d.MaxWorkerCount()
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), 3*time.Second)
			defer cancel()
			var inventoryErr error
			if data, err := callAdmin(ctx, cfg.Listen, "", http.MethodGet, "/_info", nil); err == nil {
				var info struct{ Version string }
				_ = json.Unmarshal(data, &info)
				report.Daemon = daemonStatus{Running: true, Version: info.Version}
				inventoryErr = a.readInventorySummary(cmd.Context(), &report)
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
			return inventoryErr
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
	if r.FleetListen != "" {
		fmt.Fprintf(tw, "Fleet endpoint:\t%s\nPlacement order:\t%s\nWorker limit:\t%d\n", r.FleetListen, strings.Join(r.ProviderOrder, " -> "), r.MaxWorkers)
	}
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
		fmt.Fprintf(tw, "Fleet state:\t%s\n", machineSummary(r.Machines, r.Total))
		if r.Total > 0 && len(r.ProviderStates) == 0 {
			fmt.Fprintln(tw, "Provider state:\tnot reported")
		}
		for _, name := range slices.Sorted(maps.Keys(r.ProviderStates)) {
			s := r.ProviderStates[name]
			fmt.Fprintf(tw, "Provider state (%s):\t%s", name, machineSummary(s.States, s.Total))
			if !s.ObservedAt.IsZero() {
				fmt.Fprintf(tw, "; last observed %s", observationTime(s.ObservedAt))
			}
			fmt.Fprintln(tw)
		}
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
