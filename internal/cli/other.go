package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/spf13/cobra"

	"github.com/pedrospdc/apple-container-compose/internal/compose"
)

func psCommand(g *globals) *cobra.Command {
	var o compose.PsOptions
	cmd := &cobra.Command{
		Use:     "ps [OPTIONS] [SERVICE...]",
		Short:   "List containers",
		Aliases: []string{"list"},
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := g.projectOrName(cmd.Context(), nil)
			if err != nil {
				return err
			}
			o.Services = args
			return g.engine.Ps(cmd.Context(), p, o)
		},
	}
	f := cmd.Flags()
	f.BoolVarP(&o.All, "all", "a", false, "Show all stopped containers (including those created by the run command)")
	f.BoolVarP(&o.Quiet, "quiet", "q", false, "Only display IDs")
	f.StringVar(&o.Format, "format", "table", "Format output using a custom template: 'table', 'table TEMPLATE', 'json', 'TEMPLATE'")
	f.BoolVar(&o.NoTrunc, "no-trunc", false, "Don't truncate output")
	f.StringArrayVar(&o.Status, "status", nil, "Filter services by status. Values: [paused | restarting | removing | running | dead | created | exited]")
	f.StringVar(&o.Filter, "filter", "", "Filter services by a property (supported filters: status)")
	f.BoolVar(&o.Orphans, "orphans", true, "Include orphaned services (not declared by project)")
	f.BoolVar(&o.ListServices, "services", false, "Display services")
	return cmd
}

func lsCommand(g *globals) *cobra.Command {
	var all, quiet bool
	var format, filter string
	cmd := &cobra.Command{
		Use:   "ls [OPTIONS]",
		Short: "List running compose projects",
		RunE: func(cmd *cobra.Command, args []string) error {
			return g.engine.Ls(cmd.Context(), all, quiet, format, filter)
		},
	}
	cmd.Flags().BoolVarP(&all, "all", "a", false, "Show all stopped Compose projects")
	cmd.Flags().BoolVarP(&quiet, "quiet", "q", false, "Only display project names")
	cmd.Flags().StringVar(&format, "format", "table", `Format the output. Values: [table | json]`)
	cmd.Flags().StringVar(&filter, "filter", "", "Filter output based on conditions provided")
	return cmd
}

func logsCommand(g *globals) *cobra.Command {
	var o compose.LogsOptions
	cmd := &cobra.Command{
		Use:   "logs [OPTIONS] [SERVICE...]",
		Short: "View output from containers",
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := g.projectOrName(cmd.Context(), nil)
			if err != nil {
				return err
			}
			o.Services = args
			o.NoColor = o.NoColor || g.noColor()
			return g.engine.Logs(cmd.Context(), p, o)
		},
	}
	f := cmd.Flags()
	f.BoolVarP(&o.Follow, "follow", "f", false, "Follow log output")
	f.StringVarP(&o.Tail, "tail", "n", "all", "Number of lines to show from the end of the logs for each container")
	f.BoolVarP(&o.Timestamps, "timestamps", "t", false, "Show timestamps")
	f.BoolVar(&o.NoColor, "no-color", false, "Produce monochrome output")
	f.BoolVar(&o.NoPrefix, "no-log-prefix", false, "Don't print prefix in logs")
	f.StringVar(&o.Since, "since", "", "Show logs since timestamp (not supported)")
	f.StringVar(&o.Until, "until", "", "Show logs before a timestamp (not supported)")
	f.IntVar(&o.Index, "index", 0, "index of the container if service has multiple replicas")
	return cmd
}

func attachCommand(g *globals) *cobra.Command {
	var index int
	var noStdin, sigProxy bool
	var detachKeys string
	cmd := &cobra.Command{
		Use:   "attach [OPTIONS] SERVICE",
		Short: "Attach local standard input, output, and error streams to a service's running container",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := g.projectOrName(cmd.Context(), nil)
			if err != nil {
				return err
			}
			// Apple container can't attach to a running process; stream its
			// output instead.
			return g.engine.Logs(cmd.Context(), p, compose.LogsOptions{Services: args, Follow: true, Tail: "0", NoPrefix: true, Index: index})
		},
	}
	cmd.Flags().IntVar(&index, "index", 0, "index of the container if service has multiple replicas")
	cmd.Flags().BoolVar(&noStdin, "no-stdin", false, "Do not attach STDIN")
	cmd.Flags().BoolVar(&sigProxy, "sig-proxy", true, "Proxy all received signals to the process")
	cmd.Flags().StringVar(&detachKeys, "detach-keys", "", "Override the key sequence for detaching from a container")
	return cmd
}

func buildCommand(g *globals) *cobra.Command {
	var o compose.BuildOptions
	var buildArgs []string
	cmd := &cobra.Command{
		Use:   "build [OPTIONS] [SERVICE...]",
		Short: "Build or rebuild services",
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := g.project(cmd.Context(), args)
			if err != nil {
				return err
			}
			o.Services = args
			o.Args = map[string]string{}
			for _, a := range buildArgs {
				k, v, ok := strings.Cut(a, "=")
				if !ok {
					v = os.Getenv(k)
				}
				o.Args[k] = v
			}
			if g.progress != "auto" && g.progress != "quiet" {
				o.Progress = g.progress
			}
			return g.engine.Build(cmd.Context(), p, o)
		},
	}
	f := cmd.Flags()
	f.StringArrayVar(&buildArgs, "build-arg", nil, "Set build-time variables for services")
	f.BoolVar(&o.NoCache, "no-cache", false, "Do not use cache when building the image")
	f.BoolVar(&o.Pull, "pull", false, "Always attempt to pull a newer version of the image")
	f.BoolVar(&o.Push, "push", false, "Push service images")
	f.BoolVarP(&o.Quiet, "quiet", "q", false, "Don't print anything to STDOUT")
	f.StringVarP(&o.Memory, "memory", "m", "", "Set memory limit for the build container")
	f.StringVar(&o.SSH, "ssh", "", "Set SSH authentications used when building service images")
	f.BoolVar(&o.Deps, "with-dependencies", false, "Also build dependencies (transitively)")
	f.String("builder", "", "Set builder to use (ignored)")
	f.Bool("check", false, "Check build configuration (not supported)")
	f.Bool("print", false, "Print equivalent bake file (not supported)")
	f.String("provenance", "", "Add a provenance attestation (not supported)")
	f.String("sbom", "", "Add a SBOM attestation (not supported)")
	return cmd
}

func pullCommand(g *globals) *cobra.Command {
	var ignoreFailures, includeDeps, ignoreBuildable, quiet bool
	var policy string
	cmd := &cobra.Command{
		Use:   "pull [OPTIONS] [SERVICE...]",
		Short: "Pull service images",
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := g.project(cmd.Context(), args)
			if err != nil {
				return err
			}
			return g.engine.Pull(cmd.Context(), p, args, ignoreFailures, includeDeps, ignoreBuildable, policy, quiet)
		},
	}
	cmd.Flags().BoolVar(&ignoreFailures, "ignore-pull-failures", false, "Pull what it can and ignores images with pull failures")
	cmd.Flags().BoolVar(&includeDeps, "include-deps", false, "Also pull services declared as dependencies")
	cmd.Flags().BoolVar(&ignoreBuildable, "ignore-buildable", false, "Ignore images that can be built")
	cmd.Flags().StringVar(&policy, "policy", "", `Apply pull policy ("missing"|"always")`)
	cmd.Flags().BoolVarP(&quiet, "quiet", "q", false, "Pull without printing progress information")
	return cmd
}

func pushCommand(g *globals) *cobra.Command {
	var ignoreFailures, includeDeps, quiet bool
	cmd := &cobra.Command{
		Use:   "push [OPTIONS] [SERVICE...]",
		Short: "Push service images",
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := g.project(cmd.Context(), args)
			if err != nil {
				return err
			}
			return g.engine.Push(cmd.Context(), p, args, ignoreFailures, includeDeps)
		},
	}
	cmd.Flags().BoolVar(&ignoreFailures, "ignore-push-failures", false, "Push what it can and ignores images with push failures")
	cmd.Flags().BoolVar(&includeDeps, "include-deps", false, "Also push images of services declared as dependencies")
	cmd.Flags().BoolVarP(&quiet, "quiet", "q", false, "Push without printing progress information")
	return cmd
}

var varPattern = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)((:?[-?+])([^}]*))?\}|\$([A-Za-z_][A-Za-z0-9_]*)`)

func configCommand(g *globals) *cobra.Command {
	var format, output, hashServices string
	var services, volumes, networks, images, profiles, quiet, environment, variables, models bool
	var noInterpolate, noNormalize, noPathResolution, noConsistency, noEnvResolution, resolveDigests, lockDigests bool
	cmd := &cobra.Command{
		Use:     "config [OPTIONS] [SERVICE...]",
		Aliases: []string{"convert"},
		Short:   "Parse, resolve and render compose file in canonical format",
		RunE: func(cmd *cobra.Command, args []string) error {
			g.opts.NoInterpolate = noInterpolate
			g.opts.NoNormalize = noNormalize
			g.opts.NoResolvePaths = noPathResolution
			g.opts.NoConsistency = noConsistency
			g.opts.NoResolveEnv = noEnvResolution
			if resolveDigests || lockDigests {
				g.engine.Client().Stderr.Write([]byte("WARN[0000] image digest resolution is not supported; ignoring\n"))
			}
			out := cmd.OutOrStdout()
			if output != "" {
				f, err := os.Create(output)
				if err != nil {
					return err
				}
				defer f.Close()
				out = f
			}
			if profiles {
				p, err := g.opts.Load(cmd.Context())
				if err != nil {
					return err
				}
				all := map[string]bool{}
				for _, s := range append(servicesOf(p.Services), servicesOf(p.DisabledServices)...) {
					for _, pr := range s.Profiles {
						all[pr] = true
					}
				}
				for _, k := range sortedSet(all) {
					fmt.Fprintln(out, k)
				}
				return nil
			}
			if variables {
				model, err := (&compose.ProjectOptions{ConfigPaths: g.opts.ConfigPaths, ProjectDir: g.opts.ProjectDir, ProjectName: g.opts.ProjectName, EnvFiles: g.opts.EnvFiles, NoInterpolate: true}).LoadModel(cmd.Context())
				if err != nil {
					return err
				}
				b, _ := json.Marshal(model)
				seen := map[string]string{}
				for _, m := range varPattern.FindAllStringSubmatch(string(b), -1) {
					name := m[1]
					if name == "" {
						name = m[5]
					}
					if _, ok := seen[name]; !ok || seen[name] == "" {
						def := ""
						if strings.Contains(m[3], "-") {
							def = m[4]
						}
						seen[name] = def
					}
				}
				fmt.Fprintf(out, "%-30s %-10s %s\n", "NAME", "REQUIRED", "DEFAULT VALUE")
				for _, k := range sortedKeysStr(seen) {
					req := "true"
					if seen[k] != "" {
						req = "false"
					}
					fmt.Fprintf(out, "%-30s %-10s %s\n", k, req, seen[k])
				}
				return nil
			}
			p, err := g.project(cmd.Context(), args)
			if err != nil {
				return err
			}
			if len(args) > 0 {
				if p, err = p.WithSelectedServices(args); err != nil {
					return err
				}
			}
			switch {
			case quiet:
				return nil
			case services:
				for _, s := range p.ServiceNames() {
					fmt.Fprintln(out, s)
				}
				return nil
			case volumes:
				for _, v := range sortedKeysOf(p.Volumes) {
					fmt.Fprintln(out, v)
				}
				return nil
			case networks:
				for _, n := range sortedKeysOf(p.Networks) {
					fmt.Fprintln(out, n)
				}
				return nil
			case models:
				for _, n := range sortedKeysOf(p.Models) {
					fmt.Fprintln(out, n)
				}
				return nil
			case images:
				for _, name := range p.ServiceNames() {
					fmt.Fprintln(out, compose.ImageName(p.Name, p.Services[name]))
				}
				return nil
			case environment:
				for _, k := range sortedKeysStr(p.Environment) {
					fmt.Fprintf(out, "%s=%s\n", k, p.Environment[k])
				}
				return nil
			case hashServices != "":
				names := p.ServiceNames()
				if hashServices != "*" {
					names = strings.Split(hashServices, ",")
				}
				for _, n := range names {
					s, err := p.GetService(n)
					if err != nil {
						return err
					}
					h, err := compose.ServiceHash(s)
					if err != nil {
						return err
					}
					fmt.Fprintf(out, "%s %s\n", n, h)
				}
				return nil
			}
			var b []byte
			switch format {
			case "json":
				b, err = p.MarshalJSON()
			case "yaml", "":
				b, err = p.MarshalYAML()
			default:
				return fmt.Errorf("unsupported format %q", format)
			}
			if err != nil {
				return err
			}
			_, err = out.Write(b)
			return err
		},
	}
	f := cmd.Flags()
	f.StringVar(&format, "format", "yaml", "Format the output. Values: [yaml | json]")
	f.StringVarP(&output, "output", "o", "", "Save to file (default to stdout)")
	f.StringVar(&hashServices, "hash", "", "Print the service config hash, one per line")
	f.BoolVar(&services, "services", false, "Print the service names, one per line")
	f.BoolVar(&volumes, "volumes", false, "Print the volume names, one per line")
	f.BoolVar(&networks, "networks", false, "Print the network names, one per line")
	f.BoolVar(&models, "models", false, "Print the model names, one per line")
	f.BoolVar(&images, "images", false, "Print the image names, one per line")
	f.BoolVar(&profiles, "profiles", false, "Print the profile names, one per line")
	f.BoolVarP(&quiet, "quiet", "q", false, "Only validate the configuration, don't print anything")
	f.BoolVar(&environment, "environment", false, "Print environment used for interpolation")
	f.BoolVar(&variables, "variables", false, "Print model variables and default values")
	f.BoolVar(&noInterpolate, "no-interpolate", false, "Don't interpolate environment variables")
	f.BoolVar(&noNormalize, "no-normalize", false, "Don't normalize compose model")
	f.BoolVar(&noPathResolution, "no-path-resolution", false, "Don't resolve file paths")
	f.BoolVar(&noConsistency, "no-consistency", false, "Don't check model consistency")
	f.BoolVar(&noEnvResolution, "no-env-resolution", false, "Don't resolve service env files")
	f.BoolVar(&resolveDigests, "resolve-image-digests", false, "Pin image tags to digests (not supported)")
	f.BoolVar(&lockDigests, "lock-image-digests", false, "Produces an override file with image digests (not supported)")
	return cmd
}

func imagesCommand(g *globals) *cobra.Command {
	var quiet bool
	var format string
	cmd := &cobra.Command{
		Use:   "images [OPTIONS] [SERVICE...]",
		Short: "List images used by the created containers",
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := g.projectOrName(cmd.Context(), nil)
			if err != nil {
				return err
			}
			return g.engine.Images(cmd.Context(), p, args, quiet, format)
		},
	}
	cmd.Flags().BoolVarP(&quiet, "quiet", "q", false, "Only display IDs")
	cmd.Flags().StringVar(&format, "format", "table", "Format the output. Values: [table | json]")
	return cmd
}

func topCommand(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "top [SERVICES...]",
		Short: "Display the running processes",
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := g.projectOrName(cmd.Context(), nil)
			if err != nil {
				return err
			}
			return g.engine.Top(cmd.Context(), p, args)
		},
	}
}

func portCommand(g *globals) *cobra.Command {
	var protocol string
	var index int
	cmd := &cobra.Command{
		Use:   "port [OPTIONS] SERVICE PRIVATE_PORT",
		Short: "Print the public port for a port binding",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := g.projectOrName(cmd.Context(), nil)
			if err != nil {
				return err
			}
			port, err := strconv.Atoi(args[1])
			if err != nil {
				return fmt.Errorf("invalid port %q", args[1])
			}
			return g.engine.Port(cmd.Context(), p, args[0], port, protocol, index)
		},
	}
	cmd.Flags().StringVar(&protocol, "protocol", "tcp", "tcp or udp")
	cmd.Flags().IntVar(&index, "index", 0, "Index of the container if service has multiple replicas")
	return cmd
}

func eventsCommand(g *globals) *cobra.Command {
	var asJSON bool
	var since, until string
	cmd := &cobra.Command{
		Use:   "events [OPTIONS] [SERVICE...]",
		Short: "Receive real time events from containers",
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := g.projectOrName(cmd.Context(), nil)
			if err != nil {
				return err
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt)
			defer stop()
			return g.engine.Events(ctx, p, args, asJSON, since, until)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "Output events as a stream of json objects")
	cmd.Flags().StringVar(&since, "since", "", "Show all events created since timestamp (not supported)")
	cmd.Flags().StringVar(&until, "until", "", "Stream events until this duration elapses")
	return cmd
}

func cpCommand(g *globals) *cobra.Command {
	var index int
	var all, archive, followLink bool
	cmd := &cobra.Command{
		Use:   "cp [OPTIONS] SERVICE:SRC_PATH DEST_PATH|-\n  cp [OPTIONS] SRC_PATH|- SERVICE:DEST_PATH",
		Short: "Copy files/folders between a service container and the local filesystem",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := g.projectOrName(cmd.Context(), nil)
			if err != nil {
				return err
			}
			return g.engine.Copy(cmd.Context(), p, args[0], args[1], index, all)
		},
	}
	cmd.Flags().IntVar(&index, "index", 0, "Index of the container if service has multiple replicas")
	cmd.Flags().BoolVar(&all, "all", false, "Include containers created by the run command")
	cmd.Flags().BoolVarP(&archive, "archive", "a", false, "Archive mode (copy all uid/gid information)")
	cmd.Flags().BoolVarP(&followLink, "follow-link", "L", false, "Always follow symbol link in SRC_PATH")
	return cmd
}

func statsCommand(g *globals) *cobra.Command {
	var noStream, all, noTrunc bool
	var format string
	cmd := &cobra.Command{
		Use:   "stats [OPTIONS] [SERVICE]",
		Short: "Display a live stream of container(s) resource usage statistics",
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := g.projectOrName(cmd.Context(), nil)
			if err != nil {
				return err
			}
			return g.engine.Stats(cmd.Context(), p, args, noStream, format, all)
		},
	}
	cmd.Flags().BoolVar(&noStream, "no-stream", false, "Disable streaming stats and only pull the first result")
	cmd.Flags().BoolVarP(&all, "all", "a", false, "Show all containers (default shows just running)")
	cmd.Flags().BoolVar(&noTrunc, "no-trunc", false, "Do not truncate output")
	cmd.Flags().StringVar(&format, "format", "", "Format output (table|json|yaml|toml)")
	return cmd
}

func versionCommand(g *globals) *cobra.Command {
	var short bool
	var format string
	cmd := &cobra.Command{
		Use:   "version [OPTIONS]",
		Short: "Show the Docker Compose version information",
		RunE: func(cmd *cobra.Command, args []string) error {
			if short {
				fmt.Fprintln(cmd.OutOrStdout(), compose.Version)
				return nil
			}
			runtime := "unknown"
			if out, err := g.engine.Client().Output(cmd.Context(), "--version"); err == nil {
				runtime = strings.TrimSpace(string(out))
			}
			if format == "json" {
				b, _ := json.Marshal(map[string]string{"version": compose.Version, "runtime": runtime})
				fmt.Fprintln(cmd.OutOrStdout(), string(b))
				return nil
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Apple Container Compose version %s\n(%s)\n", compose.Version, runtime)
			return nil
		},
	}
	cmd.Flags().BoolVar(&short, "short", false, "Shows only Compose's version number")
	cmd.Flags().StringVarP(&format, "format", "f", "", "Format the output. Values: [pretty | json]")
	return cmd
}

func waitCommand(g *globals) *cobra.Command {
	var downProject bool
	cmd := &cobra.Command{
		Use:   "wait SERVICE [SERVICE...] [OPTIONS]",
		Short: "Block until containers of all (or specified) services stop.",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := g.projectOrName(cmd.Context(), nil)
			if err != nil {
				return err
			}
			code, err := g.engine.Wait(cmd.Context(), p, args, downProject)
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), code)
			if code != 0 {
				return ExitCodeError{code}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&downProject, "down-project", false, "Drops project when the first container stops")
	return cmd
}

func watchCommand(g *globals) *cobra.Command {
	var o compose.WatchOptions
	cmd := &cobra.Command{
		Use:   "watch [SERVICE...]",
		Short: "Watch build context for service and rebuild/refresh containers when files are updated",
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := g.project(cmd.Context(), args)
			if err != nil {
				return err
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt)
			defer stop()
			o.Services = args
			if !o.NoUp {
				if _, err := g.engine.Up(ctx, p, compose.UpOptions{Services: args, Detach: true}); err != nil {
					return err
				}
			}
			return g.engine.Watch(ctx, p, o)
		},
	}
	cmd.Flags().BoolVar(&o.NoUp, "no-up", false, "Do not build & start services before watching")
	cmd.Flags().BoolVar(&o.Prune, "prune", true, "Prune dangling images on rebuild")
	cmd.Flags().BoolVar(&o.Quiet, "quiet", false, "hide build output")
	return cmd
}

func exportCommand(g *globals) *cobra.Command {
	var index int
	var output string
	cmd := &cobra.Command{
		Use:   "export [OPTIONS] SERVICE",
		Short: "Export a service container's filesystem as a tar archive",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := g.projectOrName(cmd.Context(), nil)
			if err != nil {
				return err
			}
			return g.engine.Export(cmd.Context(), p, args[0], index, output)
		},
	}
	cmd.Flags().IntVar(&index, "index", 0, "index of the container if service has multiple replicas.")
	cmd.Flags().StringVarP(&output, "output", "o", "", "Write to a file, instead of STDOUT")
	return cmd
}

func commitCommand(g *globals) *cobra.Command {
	var index int
	cmd := &cobra.Command{
		Use:   "commit [OPTIONS] SERVICE [REPOSITORY[:TAG]]",
		Short: "Create a new image from a service container's changes",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := g.projectOrName(cmd.Context(), nil)
			if err != nil {
				return err
			}
			ref := ""
			if len(args) > 1 {
				ref = args[1]
			} else {
				ref = p.Name + "-" + args[0] + ":latest"
			}
			return g.engine.Commit(cmd.Context(), p, args[0], index, ref)
		},
	}
	cmd.Flags().IntVar(&index, "index", 0, "index of the container if service has multiple replicas.")
	cmd.Flags().StringP("author", "a", "", "Author (not supported)")
	cmd.Flags().StringArrayP("change", "c", nil, "Apply Dockerfile instruction to the created image (not supported)")
	cmd.Flags().StringP("message", "m", "", "Commit message (not supported)")
	cmd.Flags().BoolP("pause", "p", true, "Pause container during commit (not supported)")
	return cmd
}

func volumesCommand(g *globals) *cobra.Command {
	var quiet bool
	var format string
	cmd := &cobra.Command{
		Use:   "volumes [OPTIONS] [SERVICE...]",
		Short: "List volumes",
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := g.projectOrName(cmd.Context(), nil)
			if err != nil {
				return err
			}
			return g.engine.Volumes(cmd.Context(), p, args, quiet, format)
		},
	}
	cmd.Flags().BoolVarP(&quiet, "quiet", "q", false, "Only display volume names")
	cmd.Flags().StringVar(&format, "format", "table", "Format output using a custom template")
	return cmd
}

func publishCommand(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "publish [OPTIONS] REPOSITORY[:TAG]",
		Short: "Publish compose application (not supported)",
		RunE: func(cmd *cobra.Command, args []string) error {
			return errors.New("publish is not supported: Apple container cannot push OCI artifacts")
		},
	}
}

func servicesOf(s types.Services) []types.ServiceConfig {
	var out []types.ServiceConfig
	for _, v := range s {
		out = append(out, v)
	}
	return out
}

func sortedSet(m map[string]bool) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedKeysStr(m map[string]string) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedKeysOf[V any](m map[string]V) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
