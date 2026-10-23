package compose

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/compose-spec/compose-go/v2/types"
)

// WatchOptions mirrors `docker compose watch`.
type WatchOptions struct {
	Services []string
	NoUp     bool
	Prune    bool
	Quiet    bool
}

type fileStamp struct {
	mod  time.Time
	size int64
}

type watchTrigger struct {
	service string
	types.Trigger
}

// Watch implements develop.watch using a polling file watcher (no native
// dependencies) and `container copy` for sync actions.
func (e *Engine) Watch(ctx context.Context, p *types.Project, o WatchOptions) error {
	var triggers []watchTrigger
	for _, name := range sortedServiceNames(p) {
		if len(o.Services) > 0 && !contains(o.Services, name) {
			continue
		}
		s := p.Services[name]
		if s.Develop == nil {
			continue
		}
		for _, t := range s.Develop.Watch {
			if !filepath.IsAbs(t.Path) {
				t.Path = filepath.Join(p.WorkingDir, t.Path)
			}
			triggers = append(triggers, watchTrigger{service: name, Trigger: t})
		}
	}
	if len(triggers) == 0 {
		return errors.New("none of the selected services is configured for watch, consider setting a 'develop' section")
	}
	snapshots := make([]map[string]fileStamp, len(triggers))
	for i, t := range triggers {
		snapshots[i] = scanTree(t.Path, t.Ignore)
		if t.InitialSync && strings.HasPrefix(string(t.Action), "sync") {
			var all []string
			for f := range snapshots[i] {
				all = append(all, f)
			}
			e.applyTrigger(ctx, p, t, all, nil)
		}
	}
	fmt.Fprintln(e.Err, "Watch enabled")
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
		for i, t := range triggers {
			cur := scanTree(t.Path, t.Ignore)
			var changed, removed []string
			for f, st := range cur {
				if old, ok := snapshots[i][f]; !ok || old != st {
					changed = append(changed, f)
				}
			}
			for f := range snapshots[i] {
				if _, ok := cur[f]; !ok {
					removed = append(removed, f)
				}
			}
			snapshots[i] = cur
			if len(t.Include) > 0 {
				changed = filterInclude(t.Path, changed, t.Include)
				removed = filterInclude(t.Path, removed, t.Include)
			}
			if len(changed)+len(removed) == 0 {
				continue
			}
			sort.Strings(changed)
			e.applyTrigger(ctx, p, t, changed, removed)
		}
	}
}

func (e *Engine) applyTrigger(ctx context.Context, p *types.Project, t watchTrigger, changed, removed []string) {
	if !e.Quiet {
		fmt.Fprintf(e.Err, "%s: %d file(s) changed, action %s\n", t.service, len(changed)+len(removed), t.Action)
	}
	s := p.Services[t.service]
	switch t.Action {
	case types.WatchActionRebuild:
		if err := e.Build(ctx, p, BuildOptions{Services: []string{t.service}}); err != nil {
			e.warnf("watch rebuild %s: %v", t.service, err)
			return
		}
		sp, err := SelectServices(p, []string{t.service}, true)
		if err != nil {
			return
		}
		if err := e.Create(ctx, sp, CreateOptions{Services: []string{t.service}, IgnoreOrphans: true, Images: ImageOptions{NoBuild: true}}); err != nil {
			e.warnf("watch recreate %s: %v", t.service, err)
			return
		}
		_ = e.Start(ctx, sp, StartOptions{})
	case types.WatchActionRestart:
		_ = e.Restart(ctx, p, []string{t.service}, nil, true)
	default:
		all, err := e.ctr.ListContainers(ctx, false)
		if err != nil {
			return
		}
		for _, c := range ServiceContainers(ProjectContainers(all, p.Name, false), t.service) {
			for _, f := range changed {
				rel, _ := filepath.Rel(t.Path, f)
				dst := path.Join(t.Target, filepath.ToSlash(rel))
				if rel == "." {
					dst = t.Target
				}
				_, _ = e.ctr.Exec(ctx, "exec", c.Name(), "mkdir", "-p", path.Dir(dst))
				if _, err := e.ctr.Exec(ctx, "copy", f, c.Name()+":"+dst); err != nil {
					e.warnf("watch sync %s: %v", f, err)
				}
			}
			for _, f := range removed {
				rel, _ := filepath.Rel(t.Path, f)
				_, _ = e.ctr.Exec(ctx, "exec", c.Name(), "rm", "-rf", path.Join(t.Target, filepath.ToSlash(rel)))
			}
			if t.Action == types.WatchActionSyncExec && len(t.Exec.Command) > 0 {
				if err := e.runHook(ctx, c.Name(), t.Exec); err != nil {
					e.warnf("watch exec %s: %v", c.Name(), err)
				}
			}
		}
		if t.Action == types.WatchActionSyncRestart {
			_ = e.Restart(ctx, p, []string{s.Name}, nil, true)
		}
	}
}

// scanTree returns file stamps below root, honouring ignore patterns.
func scanTree(root string, ignore []string) map[string]fileStamp {
	out := map[string]fileStamp{}
	info, err := os.Stat(root)
	if err != nil {
		return out
	}
	if !info.IsDir() {
		out[root] = fileStamp{info.ModTime(), info.Size()}
		return out
	}
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		if rel != "." && ignored(rel, ignore) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if fi, err := d.Info(); err == nil {
			out[p] = fileStamp{fi.ModTime(), fi.Size()}
		}
		return nil
	})
	return out
}

func ignored(rel string, patterns []string) bool {
	rel = filepath.ToSlash(rel)
	for _, pat := range patterns {
		pat = strings.TrimSuffix(strings.TrimPrefix(filepath.ToSlash(pat), "./"), "/")
		if globMatch(pat, rel) {
			return true
		}
		// A pattern also matches any path below a matching directory, and
		// a bare name matches at any depth.
		parts := strings.Split(rel, "/")
		for i := range parts {
			if globMatch(pat, strings.Join(parts[:i+1], "/")) || (!strings.Contains(pat, "/") && globMatch(pat, parts[i])) {
				return true
			}
		}
	}
	return false
}

func globMatch(pattern, name string) bool {
	if strings.Contains(pattern, "**") {
		prefix, suffix, _ := strings.Cut(pattern, "**")
		prefix = strings.TrimSuffix(prefix, "/")
		suffix = strings.TrimPrefix(suffix, "/")
		if prefix != "" && !strings.HasPrefix(name, prefix) {
			return false
		}
		if suffix == "" {
			return true
		}
		parts := strings.Split(name, "/")
		for i := range parts {
			if ok, _ := path.Match(suffix, strings.Join(parts[i:], "/")); ok {
				return true
			}
		}
		return false
	}
	ok, _ := path.Match(pattern, name)
	return ok
}

func filterInclude(root string, files, include []string) []string {
	var out []string
	for _, f := range files {
		rel, _ := filepath.Rel(root, f)
		if ignored(rel, include) {
			out = append(out, f)
		}
	}
	return out
}
