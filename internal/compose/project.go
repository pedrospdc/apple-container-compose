package compose

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/compose-spec/compose-go/v2/cli"
	"github.com/compose-spec/compose-go/v2/types"
)

// ProjectOptions mirrors the global flags of `docker compose`.
type ProjectOptions struct {
	ConfigPaths      []string
	ProjectName      string
	ProjectDir       string
	Profiles         []string
	EnvFiles         []string
	Compatibility    bool
	AllResources     bool
	NoInterpolate    bool
	NoNormalize      bool
	NoResolvePaths   bool
	NoConsistency    bool
	NoResolveEnv     bool
	SelectedServices []string
}

// ErrNoConfig is returned when no compose file can be located.
var ErrNoConfig = errors.New("no configuration file provided: not found")

// Load parses the compose model using the same rules as Docker Compose:
// COMPOSE_FILE / COMPOSE_PROJECT_NAME / COMPOSE_PROFILES, .env files,
// interpolation, extends, include, profiles and overrides.
func (o *ProjectOptions) Load(ctx context.Context) (*types.Project, error) {
	po, err := o.toCLIOptions()
	if err != nil {
		return nil, err
	}
	project, err := po.LoadProject(ctx)
	if err != nil {
		if strings.Contains(err.Error(), "no configuration file provided") {
			return nil, ErrNoConfig
		}
		return nil, err
	}
	if !o.AllResources {
		project = project.WithoutUnnecessaryResources()
	}
	for name, s := range project.Services {
		// Record the compose files on the service so containers carry them.
		s.CustomLabels = s.CustomLabels.Add(LabelWorkingDir, project.WorkingDir)
		s.CustomLabels = s.CustomLabels.Add(LabelConfigFiles, strings.Join(project.ComposeFiles, ","))
		project.Services[name] = s
	}
	return project, nil
}

// LoadModel returns the raw (optionally un-interpolated) model for `config`.
func (o *ProjectOptions) LoadModel(ctx context.Context) (map[string]any, error) {
	po, err := o.toCLIOptions()
	if err != nil {
		return nil, err
	}
	return po.LoadModel(ctx)
}

func (o *ProjectOptions) toCLIOptions() (*cli.ProjectOptions, error) {
	profiles := o.Profiles
	if len(profiles) == 0 {
		if env := os.Getenv("COMPOSE_PROFILES"); env != "" {
			profiles = strings.Split(env, ",")
		}
	}
	fns := []cli.ProjectOptionsFn{
		cli.WithWorkingDirectory(o.ProjectDir),
		cli.WithOsEnv,
		cli.WithEnvFiles(o.EnvFiles...),
		cli.WithDotEnv,
		cli.WithConfigFileEnv,
		cli.WithDefaultConfigPath,
		cli.WithName(o.ProjectName),
		cli.WithProfiles(profiles),
		cli.WithInterpolation(!o.NoInterpolate),
		cli.WithNormalization(!o.NoNormalize),
		cli.WithResolvedPaths(!o.NoResolvePaths),
		cli.WithConsistency(!o.NoConsistency),
		cli.WithDiscardEnvFile,
	}
	if o.NoResolveEnv {
		fns = append(fns, cli.WithoutEnvironmentResolution)
	}
	paths := o.ConfigPaths
	if len(paths) == 1 && paths[0] == "-" {
		paths = []string{"-"}
	}
	if o.ProjectDir == "" && len(paths) > 0 && paths[0] != "-" {
		// Docker Compose uses the first file's directory as project dir.
		abs, err := filepath.Abs(paths[0])
		if err == nil {
			fns[0] = cli.WithWorkingDirectory(filepath.Dir(abs))
		}
	}
	return cli.NewProjectOptions(paths, fns...)
}

// ProjectNameFromEnv returns the project name that would be used when no
// compose file is present (explicit flag, env or current directory).
func (o *ProjectOptions) ProjectNameFromEnv() string {
	if o.ProjectName != "" {
		return NormalizeProjectName(o.ProjectName)
	}
	if v := os.Getenv("COMPOSE_PROJECT_NAME"); v != "" {
		return NormalizeProjectName(v)
	}
	dir := o.ProjectDir
	if dir == "" {
		dir, _ = os.Getwd()
	}
	return NormalizeProjectName(filepath.Base(dir))
}

// NormalizeProjectName applies the compose-spec project name rules.
func NormalizeProjectName(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			b.WriteRune(r)
		}
	}
	return strings.TrimLeft(b.String(), "_-")
}

// ServiceNamesOrAll validates the requested names against the project.
func ServiceNamesOrAll(p *types.Project, names []string) ([]string, error) {
	if len(names) == 0 {
		return p.ServiceNames(), nil
	}
	for _, n := range names {
		if _, ok := p.Services[n]; !ok {
			if _, disabled := p.DisabledServices[n]; disabled {
				continue
			}
			return nil, fmt.Errorf("no such service: %s", n)
		}
	}
	return names, nil
}
