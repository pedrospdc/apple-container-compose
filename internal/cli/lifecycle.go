package cli

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/pedrospdc/apple-container-compose/internal/compose"
)

func timeoutFlag(cmd *cobra.Command, dst *int) func() *int {
	cmd.Flags().IntVarP(dst, "timeout", "t", 0, "Specify a shutdown timeout in seconds")
	return func() *int {
		if cmd.Flags().Changed("timeout") {
			return dst
		}
		return nil
	}
}

func parseScale(values []string) (map[string]int, error) {
	out := map[string]int{}
	for _, v := range values {
		name, n, ok := strings.Cut(v, "=")
		if !ok {
			return nil, fmt.Errorf("invalid scale %q, expected SERVICE=NUM", v)
		}
		i, err := strconv.Atoi(n)
		if err != nil {
			return nil, fmt.Errorf("invalid scale %q: %w", v, err)
		}
		out[name] = i
	}
	return out, nil
}

type createFlags struct {
	build, noBuild, forceRecreate, noRecreate, removeOrphans, quietPull, renewAnon, alwaysRecreateDeps, yes bool
	pull                                                                                                    string
	scale                                                                                                   []string
}

func (c *createFlags) register(cmd *cobra.Command) {
	f := cmd.Flags()
	f.BoolVar(&c.build, "build", false, "Build images before starting containers")
	f.BoolVar(&c.noBuild, "no-build", false, "Don't build an image, even if it's policy")
	f.BoolVar(&c.forceRecreate, "force-recreate", false, "Recreate containers even if their configuration and image haven't changed")
	f.BoolVar(&c.noRecreate, "no-recreate", false, "If containers already exist, don't recreate them")
	f.BoolVar(&c.removeOrphans, "remove-orphans", false, "Remove containers for services not defined in the Compose file")
	f.BoolVar(&c.quietPull, "quiet-pull", false, "Pull without printing progress information")
	f.StringVar(&c.pull, "pull", "policy", `Pull image before running ("always"|"missing"|"never"|"policy")`)
	f.StringArrayVar(&c.scale, "scale", nil, "Scale SERVICE to NUM instances. Overrides the scale setting in the Compose file if present")
	f.BoolVarP(&c.yes, "y", "y", false, "Assume \"yes\" as answer to all prompts and run non-interactively")
}

func (c *createFlags) options() (compose.CreateOptions, error) {
	if c.build && c.noBuild {
		return compose.CreateOptions{}, fmt.Errorf("--build and --no-build are incompatible")
	}
	if c.forceRecreate && c.noRecreate {
		return compose.CreateOptions{}, fmt.Errorf("--force-recreate and --no-recreate are incompatible")
	}
	scale, err := parseScale(c.scale)
	if err != nil {
		return compose.CreateOptions{}, err
	}
	pull := c.pull
	if pull == "policy" {
		pull = ""
	}
	return compose.CreateOptions{
		ForceRecreate:      c.forceRecreate,
		NoRecreate:         c.noRecreate,
		AlwaysRecreateDeps: c.alwaysRecreateDeps,
		RemoveOrphans:      c.removeOrphans || os.Getenv("COMPOSE_REMOVE_ORPHANS") == "true",
		IgnoreOrphans:      os.Getenv("COMPOSE_IGNORE_ORPHANS") == "true",
		RenewAnonVolumes:   c.renewAnon,
		Scale:              scale,
		Images:             compose.ImageOptions{Build: c.build, NoBuild: c.noBuild, Pull: pull, QuietPull: c.quietPull},
	}, nil
}

func upCommand(g *globals) *cobra.Command {
	var cf createFlags
	var o compose.UpOptions
	var timeout int
	var waitTimeout int
	var abortOnFailure bool
	cmd := &cobra.Command{
		Use:   "up [OPTIONS] [SERVICE...]",
		Short: "Create and start containers",
	}
	getTimeout := timeoutFlag(cmd, &timeout)
	cf.register(cmd)
	f := cmd.Flags()
	f.BoolVarP(&o.Detach, "detach", "d", false, "Detached mode: Run containers in the background")
	f.BoolVar(&o.NoDeps, "no-deps", false, "Don't start linked services")
	f.BoolVar(&o.NoStart, "no-start", false, "Don't start the services after creating them")
	f.BoolVar(&o.Wait, "wait", false, "Wait for services to be running|healthy. Implies detached mode")
	f.IntVar(&waitTimeout, "wait-timeout", 0, "Maximum duration in seconds to wait for the project to be running|healthy")
	f.BoolVar(&o.AbortOnExit, "abort-on-container-exit", false, "Stops all containers if any container was stopped. Incompatible with -d")
	f.BoolVar(&abortOnFailure, "abort-on-container-failure", false, "Stops all containers if any container exited with failure. Incompatible with -d")
	f.StringVar(&o.ExitCodeFrom, "exit-code-from", "", "Return the exit code of the selected service container. Implies --abort-on-container-exit")
	f.StringArrayVar(&o.Attach, "attach", nil, "Restrict attaching to the specified services")
	f.StringArrayVar(&o.NoAttach, "no-attach", nil, "Do not attach (stream logs) to the specified services")
	f.BoolVar(&o.AttachDependencies, "attach-dependencies", false, "Automatically attach to log output of dependent services")
	f.BoolVar(&o.NoLogPrefix, "no-log-prefix", false, "Don't print prefix in logs")
	f.BoolVar(&o.NoColor, "no-color", false, "Produce monochrome output")
	f.BoolVar(&o.Timestamps, "timestamps", false, "Show timestamps")
	f.BoolVar(&cf.alwaysRecreateDeps, "always-recreate-deps", false, "Recreate dependent containers. Incompatible with --no-recreate")
	f.BoolVarP(&cf.renewAnon, "renew-anon-volumes", "V", false, "Recreate anonymous volumes instead of retrieving data from the previous containers")
	f.BoolVarP(&o.Watch, "watch", "w", false, "Watch source code and rebuild/refresh containers when files are updated")
	f.Bool("menu", false, "Enable interactive shortcuts when running attached (not supported)")
	f.Bool("no-recreate-deps", false, "")
	_ = f.MarkHidden("no-recreate-deps")
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		p, err := g.project(cmd.Context(), args)
		if err != nil {
			return err
		}
		co, err := cf.options()
		if err != nil {
			return err
		}
		o.Create = co
		o.Create.Timeout = getTimeout()
		o.Services = args
		o.Timeout = getTimeout()
		o.AbortOnFailure = abortOnFailure
		o.WaitTimeout = time.Duration(waitTimeout) * time.Second
		o.NoColor = o.NoColor || g.noColor()
		if o.Wait {
			o.Detach = true
		}
		if o.Detach && (o.AbortOnExit || abortOnFailure || o.ExitCodeFrom != "") {
			return fmt.Errorf("--abort-on-container-exit, --abort-on-container-failure and --exit-code-from are incompatible with --detach")
		}
		code, err := g.engine.Up(cmd.Context(), p, o)
		if err != nil {
			return err
		}
		if code != 0 {
			return ExitCodeError{code}
		}
		return nil
	}
	return cmd
}

func downCommand(g *globals) *cobra.Command {
	var o compose.DownOptions
	var timeout int
	cmd := &cobra.Command{
		Use:   "down [OPTIONS] [SERVICES]",
		Short: "Stop and remove containers, networks",
	}
	getTimeout := timeoutFlag(cmd, &timeout)
	f := cmd.Flags()
	f.BoolVar(&o.RemoveOrphans, "remove-orphans", false, "Remove containers for services not defined in the Compose file")
	f.StringVar(&o.Images, "rmi", "", `Remove images used by services. "local" remove only images that don't have a custom tag ("local"|"all")`)
	f.BoolVarP(&o.Volumes, "volumes", "v", false, "Remove named volumes declared in the \"volumes\" section of the Compose file and anonymous volumes attached to containers")
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		p, err := g.projectOrName(cmd.Context(), nil)
		if err != nil {
			return err
		}
		if o.Images != "" && o.Images != "local" && o.Images != "all" {
			return fmt.Errorf("invalid value for --rmi: %q", o.Images)
		}
		o.Timeout = getTimeout()
		o.Services = args
		o.RemoveOrphans = o.RemoveOrphans || os.Getenv("COMPOSE_REMOVE_ORPHANS") == "true"
		return g.engine.Down(cmd.Context(), p, o)
	}
	return cmd
}

func createCommand(g *globals) *cobra.Command {
	var cf createFlags
	cmd := &cobra.Command{
		Use:   "create [OPTIONS] [SERVICE...]",
		Short: "Creates containers for a service",
	}
	cf.register(cmd)
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		p, err := g.project(cmd.Context(), args)
		if err != nil {
			return err
		}
		o, err := cf.options()
		if err != nil {
			return err
		}
		o.Services = args
		sp, err := compose.SelectServices(p, args, false)
		if err != nil {
			return err
		}
		return g.engine.Create(cmd.Context(), sp, o)
	}
	return cmd
}

func startCommand(g *globals) *cobra.Command {
	var wait bool
	var waitTimeout int
	cmd := &cobra.Command{
		Use:   "start [SERVICE...]",
		Short: "Start services",
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := g.project(cmd.Context(), args)
			if err != nil {
				return err
			}
			sp, err := compose.SelectServices(p, args, false)
			if err != nil {
				return err
			}
			err = g.engine.Start(cmd.Context(), sp, compose.StartOptions{Services: args, Wait: wait, WaitTimeout: time.Duration(waitTimeout) * time.Second})
			g.engine.EnsureSupervisor(cmd.Context(), p.Name)
			return err
		},
	}
	cmd.Flags().BoolVar(&wait, "wait", false, "Wait for services to be running|healthy. Implies detached mode")
	cmd.Flags().IntVar(&waitTimeout, "wait-timeout", 0, "Maximum duration in seconds to wait for the project to be running|healthy")
	return cmd
}

func stopCommand(g *globals) *cobra.Command {
	var timeout int
	cmd := &cobra.Command{Use: "stop [OPTIONS] [SERVICE...]", Short: "Stop services"}
	getTimeout := timeoutFlag(cmd, &timeout)
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		p, err := g.projectOrName(cmd.Context(), args)
		if err != nil {
			return err
		}
		return g.engine.Stop(cmd.Context(), p, args, getTimeout())
	}
	return cmd
}

func restartCommand(g *globals) *cobra.Command {
	var timeout int
	var noDeps bool
	cmd := &cobra.Command{Use: "restart [OPTIONS] [SERVICE...]", Short: "Restart service containers"}
	getTimeout := timeoutFlag(cmd, &timeout)
	cmd.Flags().BoolVar(&noDeps, "no-deps", false, "Don't restart dependent services")
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		p, err := g.project(cmd.Context(), args)
		if err != nil {
			return err
		}
		err = g.engine.Restart(cmd.Context(), p, args, getTimeout(), noDeps)
		g.engine.EnsureSupervisor(cmd.Context(), p.Name)
		return err
	}
	return cmd
}

func rmCommand(g *globals) *cobra.Command {
	var force, stop, volumes bool
	cmd := &cobra.Command{
		Use:   "rm [OPTIONS] [SERVICE...]",
		Short: "Removes stopped service containers",
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := g.projectOrName(cmd.Context(), args)
			if err != nil {
				return err
			}
			return g.engine.Remove(cmd.Context(), p, args, force, stop, volumes, nil)
		},
	}
	cmd.Flags().BoolVarP(&force, "force", "f", false, "Don't ask to confirm removal")
	cmd.Flags().BoolVarP(&stop, "stop", "s", false, "Stop the containers, if required, before removing")
	cmd.Flags().BoolVarP(&volumes, "volumes", "v", false, "Remove any anonymous volumes attached to containers")
	return cmd
}

func killCommand(g *globals) *cobra.Command {
	var signal string
	var removeOrphans bool
	cmd := &cobra.Command{
		Use:   "kill [OPTIONS] [SERVICE...]",
		Short: "Force stop service containers",
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := g.projectOrName(cmd.Context(), args)
			if err != nil {
				return err
			}
			return g.engine.Kill(cmd.Context(), p, args, signal)
		},
	}
	cmd.Flags().StringVarP(&signal, "signal", "s", "SIGKILL", "SIGNAL to send to the container")
	cmd.Flags().BoolVar(&removeOrphans, "remove-orphans", false, "Remove containers for services not defined in the Compose file")
	return cmd
}

func pauseCommand(g *globals, verb string) *cobra.Command {
	short := "Pause services"
	if verb == "unpause" {
		short = "Unpause services"
	}
	return &cobra.Command{
		Use:   verb + " [SERVICE...]",
		Short: short + " (not supported by Apple container)",
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := g.projectOrName(cmd.Context(), args)
			if err != nil {
				return err
			}
			return g.engine.Pause(cmd.Context(), p, args)
		},
	}
}

func scaleCommand(g *globals) *cobra.Command {
	var noDeps bool
	cmd := &cobra.Command{
		Use:   "scale [SERVICE=REPLICAS...]",
		Short: "Scale services",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			scale, err := parseScale(args)
			if err != nil {
				return err
			}
			var names []string
			for n := range scale {
				names = append(names, n)
			}
			p, err := g.project(cmd.Context(), names)
			if err != nil {
				return err
			}
			code, err := g.engine.Up(cmd.Context(), p, compose.UpOptions{
				Services: names, NoDeps: noDeps, Detach: true,
				Create: compose.CreateOptions{Scale: scale, IgnoreOrphans: true},
			})
			if err == nil && code != 0 {
				err = ExitCodeError{code}
			}
			return err
		},
	}
	cmd.Flags().BoolVar(&noDeps, "no-deps", false, "Don't start linked services")
	return cmd
}

func runCommand(g *globals) *cobra.Command {
	var o compose.RunOptions
	var entrypoint string
	var noTTY, quietPull, build, quietBuild bool
	var pull string
	var envFiles []string
	cmd := &cobra.Command{
		Use:   "run [OPTIONS] SERVICE [COMMAND] [ARGS...]",
		Short: "Run a one-off command on a service",
		Args:  cobra.MinimumNArgs(1),
	}
	f := cmd.Flags()
	f.SetInterspersed(false)
	f.BoolVarP(&o.Detach, "detach", "d", false, "Run container in background and print container ID")
	f.BoolVar(&o.Remove, "rm", false, "Automatically remove the container when it exits")
	f.StringVar(&o.Name, "name", "", "Assign a name to the container")
	f.StringVar(&entrypoint, "entrypoint", "", "Override the entrypoint of the image")
	f.StringArrayVarP(&o.Env, "env", "e", nil, "Set environment variables")
	f.StringArrayVar(&envFiles, "env-from-file", nil, "Set environment variables from file")
	f.StringArrayVarP(&o.Labels, "label", "l", nil, "Add or override a label")
	f.StringVarP(&o.User, "user", "u", "", "Run as specified username or uid")
	f.StringVarP(&o.WorkDir, "workdir", "w", "", "Working directory inside the container")
	f.StringArrayVarP(&o.Publish, "publish", "p", nil, "Publish a container's port(s) to the host")
	f.BoolVarP(&o.ServicePorts, "service-ports", "P", false, "Run command with all service's ports enabled and mapped to the host")
	f.BoolVar(&o.UseAliases, "use-aliases", false, "Use the service's network useAliases in the network(s) the container connects to")
	f.StringArrayVarP(&o.Volumes, "volume", "v", nil, "Bind mount a volume")
	f.BoolVarP(&noTTY, "no-TTY", "T", !term.IsTerminal(int(os.Stdout.Fd())), "Disable pseudo-TTY allocation (default: auto-detected)")
	f.BoolVarP(&o.Interactive, "interactive", "i", true, "Keep STDIN open even if not attached")
	f.BoolVar(&o.NoDeps, "no-deps", false, "Don't start linked services")
	f.BoolVar(&build, "build", false, "Build image before starting container")
	f.BoolVar(&quietPull, "quiet-pull", false, "Pull without printing progress information")
	f.BoolVar(&quietBuild, "quiet-build", false, "Suppress progress output from the build process")
	f.StringVar(&pull, "pull", "policy", `Pull image before running ("always"|"missing"|"never")`)
	f.BoolVar(&o.RemoveOrphan, "remove-orphans", false, "Remove containers for services not defined in the Compose file")
	f.StringArrayVar(&o.CapAdd, "cap-add", nil, "Add Linux capabilities")
	f.StringArrayVar(&o.CapDrop, "cap-drop", nil, "Drop Linux capabilities")
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		p, err := g.project(cmd.Context(), args[:1])
		if err != nil {
			return err
		}
		o.Service = args[0]
		o.Command = args[1:]
		if cmd.Flags().Changed("entrypoint") {
			ep := types.ShellCommand{entrypoint}
			if fields := strings.Fields(entrypoint); len(fields) > 1 {
				ep = fields
			}
			o.Entrypoint = &ep
		}
		for _, file := range envFiles {
			data, err := os.ReadFile(file)
			if err != nil {
				return err
			}
			for _, line := range strings.Split(string(data), "\n") {
				line = strings.TrimSpace(line)
				if line != "" && !strings.HasPrefix(line, "#") {
					o.Env = append(o.Env, line)
				}
			}
		}
		o.TTY = !noTTY && !o.Detach
		o.Interactive = o.Interactive && !o.Detach
		if pull == "policy" {
			pull = ""
		}
		o.Images = compose.ImageOptions{Build: build, Pull: pull, QuietPull: quietPull || quietBuild}
		code, err := g.engine.Run(cmd.Context(), p, o)
		if err != nil {
			return err
		}
		if code != 0 {
			return ExitCodeError{code}
		}
		return nil
	}
	return cmd
}

func execCommand(g *globals) *cobra.Command {
	var o compose.ExecOptions
	var noTTY, privileged bool
	var envFiles []string
	cmd := &cobra.Command{
		Use:   "exec [OPTIONS] SERVICE COMMAND [ARGS...]",
		Short: "Execute a command in a running container",
		Args:  cobra.MinimumNArgs(2),
	}
	f := cmd.Flags()
	f.SetInterspersed(false)
	f.BoolVarP(&o.Detach, "detach", "d", false, "Detached mode: Run command in the background")
	f.StringArrayVarP(&o.Env, "env", "e", nil, "Set environment variables")
	f.StringArrayVar(&envFiles, "env-from-file", nil, "Set environment variables from file")
	f.IntVar(&o.Index, "index", 0, "Index of the container if service has multiple replicas")
	f.BoolVar(&privileged, "privileged", false, "Give extended privileges to the process (not supported)")
	f.BoolVarP(&noTTY, "no-TTY", "T", !term.IsTerminal(int(os.Stdout.Fd())), "Disable pseudo-TTY allocation. By default 'docker compose exec' allocates a TTY")
	f.StringVarP(&o.User, "user", "u", "", "Run the command as this user")
	f.StringVarP(&o.WorkDir, "workdir", "w", "", "Path to workdir directory for this command")
	f.BoolVarP(&o.Interactive, "interactive", "i", true, "Keep STDIN open even if not attached")
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		p, err := g.projectOrName(cmd.Context(), nil)
		if err != nil {
			return err
		}
		o.Service = args[0]
		o.Command = args[1:]
		o.TTY = !noTTY && !o.Detach
		o.Interactive = o.Interactive && !o.Detach
		for _, file := range envFiles {
			data, err := os.ReadFile(file)
			if err != nil {
				return err
			}
			for _, line := range strings.Split(string(data), "\n") {
				if line = strings.TrimSpace(line); line != "" && !strings.HasPrefix(line, "#") {
					o.Env = append(o.Env, line)
				}
			}
		}
		code, err := g.engine.Exec(cmd.Context(), p, o)
		if err != nil {
			return err
		}
		if code != 0 {
			return ExitCodeError{code}
		}
		return nil
	}
	return cmd
}
