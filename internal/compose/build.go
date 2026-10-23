package compose

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/compose-spec/compose-go/v2/types"
)

// BuildOptions mirrors `docker compose build` flags.
type BuildOptions struct {
	Services []string
	Args     map[string]string
	NoCache  bool
	Pull     bool
	Quiet    bool
	Push     bool
	Memory   string
	SSH      string
	Deps     bool
	Progress string
}

// Build builds images for services that declare a build section.
func (e *Engine) Build(ctx context.Context, p *types.Project, o BuildOptions) error {
	names := o.Services
	if len(names) == 0 {
		names = p.ServicesWithBuild()
	} else if o.Deps {
		sp, err := p.WithSelectedServices(names)
		if err != nil {
			return err
		}
		names = sp.ServicesWithBuild()
	}
	built := map[string]bool{}
	var errs []error
	for _, name := range names {
		s, ok := p.Services[name]
		if !ok {
			return fmt.Errorf("no such service: %s", name)
		}
		if s.Build == nil {
			continue
		}
		image := ImageName(p.Name, s)
		if built[image] {
			continue
		}
		built[image] = true
		if err := e.buildService(ctx, p, s, image, o); err != nil {
			errs = append(errs, fmt.Errorf("building %s: %w", name, err))
			continue
		}
		if o.Push && s.Image != "" {
			if err := e.ctr.Stream(ctx, nil, e.Out, e.Err, "image", "push", image); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

func (e *Engine) buildService(ctx context.Context, p *types.Project, s types.ServiceConfig, image string, o BuildOptions) error {
	b := s.Build
	contextDir := b.Context
	if contextDir == "" {
		contextDir = "."
	}
	if strings.Contains(contextDir, "://") || strings.HasPrefix(contextDir, "git@") {
		return fmt.Errorf("remote build contexts (%s) are not supported by Apple container build", contextDir)
	}
	if !filepath.IsAbs(contextDir) {
		contextDir = filepath.Join(p.WorkingDir, contextDir)
	}
	args := []string{"build", "--tag", image}
	for _, t := range b.Tags {
		args = append(args, "--tag", t)
	}
	switch {
	case b.DockerfileInline != "":
		dir := filepath.Join(e.stateDir(p.Name), "build")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		path := filepath.Join(dir, sanitizeName(s.Name)+".Dockerfile")
		if err := os.WriteFile(path, []byte(b.DockerfileInline), 0o644); err != nil {
			return err
		}
		args = append(args, "--file", path)
	case b.Dockerfile != "":
		df := b.Dockerfile
		if !filepath.IsAbs(df) {
			df = filepath.Join(contextDir, df)
		}
		args = append(args, "--file", df)
	}
	buildArgs := map[string]string{}
	for k, v := range b.Args {
		if v != nil {
			buildArgs[k] = *v
		} else if hv, ok := os.LookupEnv(k); ok {
			buildArgs[k] = hv
		}
	}
	for k, v := range o.Args {
		buildArgs[k] = v
	}
	for _, k := range sortedKeys(buildArgs) {
		args = append(args, "--build-arg", k+"="+buildArgs[k])
	}
	for _, k := range sortedKeys(b.Labels) {
		args = append(args, "--label", k+"="+b.Labels[k])
	}
	if b.Target != "" {
		args = append(args, "--target", b.Target)
	}
	if b.NoCache || o.NoCache {
		args = append(args, "--no-cache")
	}
	if b.Pull || o.Pull {
		args = append(args, "--pull")
	}
	platforms := b.Platforms
	if s.Platform != "" {
		platforms = []string{s.Platform}
	}
	for _, pl := range platforms {
		args = append(args, "--platform", pl)
	}
	for _, sec := range b.Secrets {
		obj, ok := p.Secrets[sec.Source]
		if !ok {
			return fmt.Errorf("build secret %q is not declared", sec.Source)
		}
		id := sec.Source
		switch {
		case obj.File != "":
			args = append(args, "--secret", "id="+id+",src="+obj.File)
		case obj.Environment != "":
			args = append(args, "--secret", "id="+id+",env="+obj.Environment)
		}
	}
	if len(b.SSH) > 0 || o.SSH != "" {
		args = append(args, "--ssh", "default")
	}
	if o.Memory != "" {
		args = append(args, "--memory", o.Memory)
	}
	if o.Quiet {
		args = append(args, "--quiet")
	}
	if o.Progress != "" {
		args = append(args, "--progress", o.Progress)
	}
	if len(b.AdditionalContexts) > 0 {
		e.warnf("service %q: build.additional_contexts is not supported by Apple container build", s.Name)
	}
	if len(b.CacheFrom) > 0 || len(b.CacheTo) > 0 {
		e.warnf("service %q: build cache_from/cache_to are ignored", s.Name)
	}
	args = append(args, contextDir)
	e.status("Image", image, "Building")
	if err := e.ctr.Stream(ctx, nil, e.Err, e.Err, args...); err != nil {
		return err
	}
	e.ctr.ForgetImage(image)
	e.status("Image", image, "Built")
	return nil
}
