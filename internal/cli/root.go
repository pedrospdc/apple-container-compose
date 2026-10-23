// Package cli implements the `container-compose` command line, mirroring
// `docker compose`.
package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/spf13/cobra"

	"github.com/pedrospdc/apple-container-compose/internal/compose"
	"github.com/pedrospdc/apple-container-compose/internal/ctr"
)

// ExitCodeError carries a process exit code through cobra.
type ExitCodeError struct{ Code int }

func (e ExitCodeError) Error() string { return fmt.Sprintf("exit status %d", e.Code) }

type globals struct {
	opts     compose.ProjectOptions
	dryRun   bool
	debug    bool
	ansi     string
	progress string
	parallel int
	engine   *compose.Engine
}

func (g *globals) noColor() bool {
	return g.ansi == "never" || os.Getenv("NO_COLOR") != ""
}

// project loads the compose project and optionally checks service names.
func (g *globals) project(ctx context.Context, services []string) (*types.Project, error) {
	p, err := g.opts.Load(ctx)
	if err != nil {
		return nil, err
	}
	if len(services) > 0 {
		if _, err := compose.ServiceNamesOrAll(p, services); err != nil {
			return nil, err
		}
		// Explicitly named services are enabled even if their profile isn't.
		p, err = p.WithServicesEnabled(services...)
		if err != nil {
			return nil, err
		}
	}
	return p, nil
}

// projectOrName loads the project, or — for commands that can operate on
// labels alone, like `down -p name` — returns a stub with just the name.
func (g *globals) projectOrName(ctx context.Context, services []string) (*types.Project, error) {
	p, err := g.project(ctx, services)
	if err == nil {
		return p, nil
	}
	if errors.Is(err, compose.ErrNoConfig) && (g.opts.ProjectName != "" || os.Getenv("COMPOSE_PROJECT_NAME") != "") {
		return &types.Project{Name: g.opts.ProjectNameFromEnv(), Services: types.Services{}}, nil
	}
	return nil, err
}

// NewRootCommand builds the CLI. name is how the binary was invoked.
func NewRootCommand(name string) *cobra.Command {
	g := &globals{}
	root := &cobra.Command{
		Use:           name,
		Short:         "Define and run multi-container applications with Apple container",
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			c := ctr.New()
			c.DryRun = g.dryRun
			c.Debug = g.debug || os.Getenv("COMPOSE_DEBUG") != ""
			g.engine = compose.NewEngine(c)
			g.engine.Quiet = g.progress == "quiet"
			if g.opts.ProjectName != "" {
				g.opts.ProjectName = strings.TrimSpace(g.opts.ProjectName)
			}
			return nil
		},
	}
	root.CompletionOptions.HiddenDefaultCmd = true
	root.TraverseChildren = true
	f := root.Flags()
	f.StringArrayVarP(&g.opts.ConfigPaths, "file", "f", nil, "Compose configuration files")
	f.StringVarP(&g.opts.ProjectName, "project-name", "p", "", "Project name")
	f.StringVar(&g.opts.ProjectDir, "project-directory", "", "Specify an alternate working directory (default: the path of the first specified Compose file)")
	f.StringArrayVar(&g.opts.Profiles, "profile", nil, "Specify a profile to enable")
	f.StringArrayVar(&g.opts.EnvFiles, "env-file", nil, "Specify an alternate environment file")
	f.BoolVar(&g.opts.AllResources, "all-resources", false, "Include all resources, even those not used by services")
	f.BoolVar(&g.opts.Compatibility, "compatibility", false, "Run compose in backward compatibility mode")
	f.BoolVar(&g.dryRun, "dry-run", false, "Execute command in dry run mode")
	f.StringVar(&g.ansi, "ansi", "auto", `Control when to print ANSI control characters ("never"|"always"|"auto")`)
	f.StringVar(&g.progress, "progress", "auto", `Set type of progress output (auto, tty, plain, json, quiet)`)
	f.IntVar(&g.parallel, "parallel", -1, "Control max parallelism, -1 for unlimited")
	f.BoolVar(&g.debug, "debug", false, "Print the container commands being executed")
	f.BoolP("verbose", "", false, "Show more output")
	_ = f.MarkHidden("verbose")

	root.AddCommand(
		upCommand(g), downCommand(g), createCommand(g), startCommand(g), stopCommand(g),
		restartCommand(g), rmCommand(g), killCommand(g), pauseCommand(g, "pause"), pauseCommand(g, "unpause"),
		psCommand(g), lsCommand(g), logsCommand(g), execCommand(g), runCommand(g), buildCommand(g),
		pullCommand(g), pushCommand(g), configCommand(g), imagesCommand(g), topCommand(g),
		portCommand(g), eventsCommand(g), cpCommand(g), statsCommand(g), versionCommand(g),
		waitCommand(g), scaleCommand(g), watchCommand(g), attachCommand(g), exportCommand(g),
		commitCommand(g), volumesCommand(g), publishCommand(g), aliasCommand(),
		&cobra.Command{
			Use:    compose.SuperviseCommand + " PROJECT",
			Hidden: true,
			Args:   cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
				defer stop()
				return g.engine.Supervise(ctx, args[0])
			},
		},
	)
	return root
}
