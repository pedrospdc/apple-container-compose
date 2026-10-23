package compose

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/compose-spec/compose-go/v2/graph"
	"github.com/compose-spec/compose-go/v2/types"

	"github.com/pedrospdc/apple-container-compose/internal/ctr"
)

// Engine implements compose operations on top of the Apple container CLI.
type Engine struct {
	ctr   *ctr.Client
	Out   io.Writer
	Err   io.Writer
	Quiet bool

	mu       sync.Mutex
	dnsAddrs map[string]map[string]string
	warned   map[string]bool
	health   map[string]string
	exits    map[string]*exitWaiter
}

// NewEngine returns an engine using c.
func NewEngine(c *ctr.Client) *Engine {
	return &Engine{
		ctr:      c,
		Out:      os.Stdout,
		Err:      os.Stderr,
		dnsAddrs: map[string]map[string]string{},
		warned:   map[string]bool{},
		health:   map[string]string{},
		exits:    map[string]*exitWaiter{},
	}
}

// Client exposes the underlying runtime client.
func (e *Engine) Client() *ctr.Client { return e.ctr }

func (e *Engine) stateDir(project string) string {
	base := os.Getenv("COMPOSE_STATE_DIR")
	if base == "" {
		home, _ := os.UserHomeDir()
		base = filepath.Join(home, "Library", "Application Support", "apple-container-compose")
	}
	return filepath.Join(base, "projects", project)
}

func (e *Engine) status(kind, name, state string) {
	if e.Quiet {
		return
	}
	icon := "✔"
	switch state {
	case "Creating", "Starting", "Stopping", "Removing", "Pulling", "Building", "Recreating", "Waiting", "Restarting", "Killing":
		icon = "•"
	}
	fmt.Fprintf(e.Err, " %s %s %s  %s\n", icon, kind, name, state)
}

func (e *Engine) warnf(format string, a ...any) {
	msg := fmt.Sprintf(format, a...)
	e.mu.Lock()
	seen := e.warned[msg]
	e.warned[msg] = true
	e.mu.Unlock()
	if !seen {
		fmt.Fprintf(e.Err, "WARN[0000] %s\n", msg)
	}
}

// ---- networks & volumes ----

// EnsureNetworks creates the project networks used by services.
func (e *Engine) EnsureNetworks(ctx context.Context, p *types.Project) error {
	used := map[string]bool{}
	for _, s := range p.Services {
		for key := range s.Networks {
			used[key] = true
		}
	}
	for _, key := range sortedKeys(used) {
		cfg, ok := p.Networks[key]
		if !ok {
			return fmt.Errorf("service network %q is not declared", key)
		}
		name := NetworkName(p, key)
		existing, err := e.ctr.InspectNetwork(ctx, name)
		if err != nil {
			return err
		}
		if bool(cfg.External) {
			if existing == nil {
				return fmt.Errorf("network %s declared as external, but could not be found", name)
			}
			continue
		}
		if existing != nil {
			continue
		}
		if cfg.Driver != "" && cfg.Driver != "bridge" {
			e.warnf("network %s: driver %q is not supported; using the vmnet driver", key, cfg.Driver)
		}
		args := []string{"network", "create",
			"--label", LabelProject + "=" + p.Name,
			"--label", LabelNetwork + "=" + key,
			"--label", LabelVersion + "=" + Version}
		for _, k := range sortedKeys(cfg.Labels) {
			args = append(args, "--label", k+"="+cfg.Labels[k])
		}
		if cfg.Internal {
			args = append(args, "--internal")
		}
		for _, pool := range cfg.Ipam.Config {
			if pool == nil || pool.Subnet == "" {
				continue
			}
			if strings.Contains(pool.Subnet, ":") {
				args = append(args, "--subnet-v6", pool.Subnet)
			} else {
				args = append(args, "--subnet", pool.Subnet)
			}
		}
		args = append(args, name)
		e.status("Network", name, "Creating")
		if _, err := e.ctr.Exec(ctx, args...); err != nil {
			if strings.Contains(err.Error(), "subnet") && len(cfg.Ipam.Config) > 0 {
				// Requested subnets may collide with vmnet; fall back to automatic.
				e.warnf("network %s: subnet rejected (%v); using an automatic subnet", key, err)
				if _, err := e.ctr.Exec(ctx, stripSubnetArgs(args)...); err != nil {
					return err
				}
			} else {
				return err
			}
		}
		e.status("Network", name, "Created")
	}
	return nil
}

func stripSubnetArgs(args []string) []string {
	var out []string
	for i := 0; i < len(args); i++ {
		if args[i] == "--subnet" || args[i] == "--subnet-v6" {
			i++
			continue
		}
		out = append(out, args[i])
	}
	return out
}

// EnsureVolumes creates named volumes used by services and returns the ones
// that were newly created.
func (e *Engine) EnsureVolumes(ctx context.Context, p *types.Project) (map[string]bool, error) {
	created := map[string]bool{}
	used := map[string]bool{}
	for _, s := range p.Services {
		for _, v := range s.Volumes {
			if v.Type == types.VolumeTypeVolume && v.Source != "" {
				used[v.Source] = true
			}
		}
	}
	existing, err := e.ctr.ListVolumes(ctx)
	if err != nil {
		return nil, err
	}
	have := map[string]bool{}
	for _, v := range existing {
		have[v.Name()] = true
	}
	for _, key := range sortedKeys(used) {
		cfg, ok := p.Volumes[key]
		if !ok {
			return nil, fmt.Errorf("service volume %q is not declared", key)
		}
		if bindDevice(cfg) != "" {
			continue
		}
		if !bool(cfg.External) && mountUsage(p).volumes[key] > 1 {
			continue // backed by a shared directory, see mountFlag
		}
		name := VolumeName(p, key)
		if bool(cfg.External) {
			if !have[name] {
				return nil, fmt.Errorf("external volume %q not found", name)
			}
			continue
		}
		if have[name] {
			continue
		}
		args := []string{"volume", "create",
			"--label", LabelProject + "=" + p.Name,
			"--label", LabelVolume + "=" + key,
			"--label", LabelVersion + "=" + Version}
		for _, k := range sortedKeys(cfg.Labels) {
			args = append(args, "--label", k+"="+cfg.Labels[k])
		}
		for _, k := range sortedKeys(cfg.DriverOpts) {
			if k == "size" {
				args = append(args, "-s", cfg.DriverOpts[k])
				continue
			}
			args = append(args, "--opt", k+"="+cfg.DriverOpts[k])
		}
		args = append(args, name)
		e.status("Volume", name, "Creating")
		if _, err := e.ctr.Exec(ctx, args...); err != nil {
			return nil, err
		}
		e.status("Volume", name, "Created")
		created[name] = true
	}
	return created, nil
}

func (e *Engine) ensureAnonymousVolume(ctx context.Context, p *types.Project, s types.ServiceConfig, name, target string) error {
	vols, err := e.ctr.ListVolumes(ctx)
	if err != nil {
		return err
	}
	for _, v := range vols {
		if v.Name() == name {
			return nil
		}
	}
	if _, err := e.ctr.Exec(ctx, "volume", "create",
		"--label", LabelProject+"="+p.Name,
		"--label", LabelService+"="+s.Name,
		"--label", LabelAnonymous+"=true",
		name); err != nil {
		return err
	}
	return e.initVolume(ctx, p, s, name, target, true)
}

func (e *Engine) ensureHostPathVolume(ctx context.Context, p *types.Project, s types.ServiceConfig, name, src, target string) error {
	vols, err := e.ctr.ListVolumes(ctx)
	if err != nil {
		return err
	}
	for _, v := range vols {
		if v.Name() == name {
			return nil
		}
	}
	if _, err := e.ctr.Exec(ctx, "volume", "create",
		"--label", LabelProject+"="+p.Name,
		"--label", LabelHostPath+"="+src,
		name); err != nil {
		return err
	}
	// Like a fresh host directory: no image content. It takes the ownership
	// of the image's directory though, since the user can't chown inside a
	// volume image from macOS the way they would a host directory.
	return e.initVolume(ctx, p, s, name, target, false)
}

// HelperImage is used to prepare volumes when a service image has no shell.
var HelperImage = envOr("COMPOSE_HELPER_IMAGE", "docker.io/library/alpine:3.20")

// initVolume makes a freshly created volume behave like a Docker volume:
// Apple volumes are ext4 images whose root holds lost+found (which breaks
// e.g. postgres initdb), and Docker seeds empty volumes with the image's
// content at the mount point, including its ownership.
func (e *Engine) initVolume(ctx context.Context, p *types.Project, s types.ServiceConfig, volume, target string, seed bool) error {
	if e.ctr.DryRun {
		return nil
	}
	script := `rmdir /__acc_vol/lost+found 2>/dev/null
if [ -d "$T" ] && [ -z "$(ls -A /__acc_vol)" ]; then
  [ "$SEED" = 1 ] && cp -a "$T"/. /__acc_vol/ 2>/dev/null
  chown "$(stat -c %u:%g "$T")" /__acc_vol 2>/dev/null
  chmod "$(stat -c %a "$T")" /__acc_vol 2>/dev/null
fi
exit 0`
	image := ImageName(p.Name, s)
	args := []string{"run", "--progress", "none", "--remove", "--user", "0", "--network", "none",
		"--env", "T=" + target, "--env", "SEED=" + map[bool]string{true: "1", false: "0"}[seed],
		"--volume", volume + ":/__acc_vol",
		"--entrypoint", "/bin/sh"}
	if platform, rosetta := e.platformFor(ctx, s, image); platform != "" {
		args = append(args, "--platform", platform)
		if rosetta {
			args = append(args, "--rosetta")
		}
	}
	if _, err := e.ctr.Exec(ctx, append(args, image, "-c", script)...); err == nil {
		return nil
	}
	// Image without a shell: at least remove lost+found.
	_, err := e.ctr.Exec(ctx, "run", "--remove", "--network", "none",
		"--volume", volume+":/__acc_vol", HelperImage,
		"sh", "-c", "rmdir /__acc_vol/lost+found 2>/dev/null; exit 0")
	return err
}

// ---- images ----

// ImageOptions controls image resolution before create.
type ImageOptions struct {
	Build     bool   // always build services with a build section
	NoBuild   bool   // never build
	Pull      string // override pull policy: always|missing|never
	QuietPull bool
}

// EnsureImages pulls or builds images as required by policy.
func (e *Engine) EnsureImages(ctx context.Context, p *types.Project, o ImageOptions) error {
	var toBuild []string
	type pullItem struct{ service, image string }
	var toPull []pullItem
	for _, name := range p.ServiceNames() {
		s := p.Services[name]
		image := ImageName(p.Name, s)
		policy := s.PullPolicy
		if o.Pull != "" {
			policy = o.Pull
		}
		if strings.HasPrefix(policy, "every") || policy == "daily" || policy == "weekly" || policy == "refresh" {
			policy = types.PullPolicyMissing
		}
		hasBuild := s.Build != nil
		if hasBuild && !o.NoBuild && (o.Build || policy == types.PullPolicyBuild) {
			toBuild = append(toBuild, name)
			continue
		}
		local, err := e.ctr.InspectImage(ctx, image)
		if err != nil {
			return err
		}
		switch {
		case policy == types.PullPolicyAlways && s.Image != "":
			toPull = append(toPull, pullItem{name, image})
		case local != nil:
		case policy == types.PullPolicyNever:
			return fmt.Errorf("image %s for service %s is not present locally and pull_policy is never", image, name)
		case hasBuild && s.Image != "" && !o.NoBuild:
			// Try pulling the tagged image first, build on failure.
			if err := e.pull(ctx, image, s.Platform, o.QuietPull); err != nil {
				toBuild = append(toBuild, name)
			}
		case hasBuild && !o.NoBuild:
			toBuild = append(toBuild, name)
		case hasBuild:
			return fmt.Errorf("image for service %s is not built (--no-build)", name)
		default:
			toPull = append(toPull, pullItem{name, image})
		}
	}
	// Pull in parallel, deduplicating shared images.
	seen := map[string]bool{}
	var wg sync.WaitGroup
	var errMu sync.Mutex
	var errs []error
	sem := make(chan struct{}, 4)
	for _, it := range toPull {
		if seen[it.image] {
			continue
		}
		seen[it.image] = true
		s := p.Services[it.service]
		wg.Add(1)
		go func(image, platform string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			if err := e.pull(ctx, image, platform, o.QuietPull); err != nil {
				errMu.Lock()
				errs = append(errs, err)
				errMu.Unlock()
			}
		}(it.image, s.Platform)
	}
	wg.Wait()
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	if len(toBuild) > 0 {
		return e.Build(ctx, p, BuildOptions{Services: toBuild, Quiet: o.QuietPull})
	}
	return nil
}

func (e *Engine) pull(ctx context.Context, image, platform string, quiet bool) error {
	e.status("Image", image, "Pulling")
	args := []string{"image", "pull", "--progress", "none"}
	if platform != "" {
		args = append(args, "--platform", platform)
	}
	_, err := e.ctr.Exec(ctx, append(args, image)...)
	if err != nil && platform == "" && strings.Contains(strings.ToLower(err.Error()), "platform") {
		// amd64-only images: retry for Rosetta.
		_, err = e.ctr.Exec(ctx, "image", "pull", "--progress", "none", "--platform", "linux/amd64", image)
	}
	if err != nil && strings.HasPrefix(image, "lscr.io/") && strings.Contains(err.Error(), "registrable domain") {
		// Apple's registry client refuses lscr.io's cross-domain auth to
		// ghcr.io. lscr.io images are mirrors of ghcr.io, so pull from there
		// and tag the result with the requested name.
		mirror := "ghcr.io/" + strings.TrimPrefix(image, "lscr.io/")
		args := []string{"image", "pull", "--progress", "none"}
		if platform != "" {
			args = append(args, "--platform", platform)
		}
		if _, err = e.ctr.Exec(ctx, append(args, mirror)...); err == nil {
			_, err = e.ctr.Exec(ctx, "image", "tag", mirror, image)
		}
	}
	e.ctr.ForgetImage(image)
	if err != nil {
		return fmt.Errorf("pulling %s: %w", image, err)
	}
	e.status("Image", image, "Pulled")
	return nil
}

// ---- create / converge ----

// CreateOptions controls container convergence.
type CreateOptions struct {
	Services           []string
	ForceRecreate      bool
	NoRecreate         bool
	AlwaysRecreateDeps bool
	RemoveOrphans      bool
	IgnoreOrphans      bool
	Scale              map[string]int
	Timeout            *int
	RenewAnonVolumes   bool
	Images             ImageOptions
}

// Create converges containers to the project model without starting them.
func (e *Engine) Create(ctx context.Context, p *types.Project, o CreateOptions) error {
	for name, n := range o.Scale {
		s, ok := p.Services[name]
		if !ok {
			return fmt.Errorf("no such service: %s", name)
		}
		s.SetScale(n)
		p.Services[name] = s
	}
	if err := e.EnsureImages(ctx, p, o.Images); err != nil {
		return err
	}
	if err := e.EnsureNetworks(ctx, p); err != nil {
		return err
	}
	created, err := e.EnsureVolumes(ctx, p)
	if err != nil {
		return err
	}
	if err := e.handleOrphans(ctx, p, o); err != nil {
		return err
	}
	if err := e.ensureDNS(ctx, p); err != nil {
		return err
	}
	// Seed new named volumes from the first service that mounts them.
	for _, name := range p.ServiceNames() {
		s := p.Services[name]
		for _, v := range s.Volumes {
			if v.Type != types.VolumeTypeVolume || v.Source == "" {
				continue
			}
			vn := VolumeName(p, v.Source)
			if created[vn] {
				delete(created, vn)
				if err := e.initVolume(ctx, p, s, vn, v.Target, true); err != nil {
					e.warnf("volume %s: initialisation failed: %v", vn, err)
				}
			}
		}
	}
	all, err := e.ctr.ListContainers(ctx, true)
	if err != nil {
		return err
	}
	existing := ProjectContainers(all, p.Name, false)
	recreated := map[string]bool{}
	var mu sync.Mutex
	return graph.InDependencyOrder(ctx, p, func(ctx context.Context, name string, s types.ServiceConfig) error {
		selected := len(o.Services) == 0 || contains(o.Services, name)
		force := (o.ForceRecreate && selected) || (o.AlwaysRecreateDeps && !selected)
		mu.Lock()
		for _, dep := range s.GetDependencies() {
			if recreated[dep] && s.DependsOn[dep].Restart {
				force = true
			}
		}
		mu.Unlock()
		did, err := e.convergeService(ctx, p, s, ServiceContainers(existing, name), force, o)
		if did {
			mu.Lock()
			recreated[name] = true
			mu.Unlock()
		}
		return err
	})
}

func (e *Engine) convergeService(ctx context.Context, p *types.Project, s types.ServiceConfig, current []ctr.Container, force bool, o CreateOptions) (bool, error) {
	want := s.GetScale()
	if s.ContainerName != "" && want > 1 {
		return false, fmt.Errorf("service %s: container_name %q cannot be scaled to %d", s.Name, s.ContainerName, want)
	}
	hash, err := ServiceHash(s)
	if err != nil {
		return false, err
	}
	recreated := false
	byNumber := map[int]ctr.Container{}
	for _, c := range current {
		n := ContainerNumber(c)
		if n < 1 || n > want {
			// Scale down: remove extra replicas, highest number first.
			if err := e.removeContainer(ctx, c, s, true, o.Timeout); err != nil {
				return recreated, err
			}
			continue
		}
		byNumber[n] = c
	}
	for n := 1; n <= want; n++ {
		name := ContainerName(p.Name, s, n)
		c, ok := byNumber[n]
		if ok {
			drift := c.Labels()[LabelConfigHash] != hash || c.Configuration.Image.Reference != "" && !sameImage(c.Configuration.Image.Reference, ImageName(p.Name, s))
			if !force && (o.NoRecreate || !drift) {
				continue
			}
			e.status("Container", name, "Recreating")
			if err := e.removeContainer(ctx, c, s, false, o.Timeout); err != nil {
				return recreated, err
			}
			if o.RenewAnonVolumes {
				e.removeAnonymousVolumes(ctx, name)
			}
			recreated = true
		}
		if err := e.createContainer(ctx, p, s, createOptions{Name: name, Number: n}); err != nil {
			return recreated, err
		}
	}
	return recreated, nil
}

// sameImage compares references, tolerating docker.io normalisation.
func sameImage(a, b string) bool {
	return normalizeRef(a) == normalizeRef(b)
}

func normalizeRef(r string) string {
	r = strings.TrimPrefix(r, "docker.io/")
	r = strings.TrimPrefix(r, "library/")
	if !strings.Contains(r[strings.LastIndex(r, "/")+1:], ":") && !strings.Contains(r, "@") {
		r += ":latest"
	}
	return r
}

func (e *Engine) createContainer(ctx context.Context, p *types.Project, s types.ServiceConfig, o createOptions) error {
	spec, err := e.toContainerSpec(ctx, p, s, o)
	if err != nil {
		return err
	}
	for _, w := range spec.Warnings {
		e.warnf("service %q: %s", s.Name, w)
	}
	e.status("Container", spec.Name, "Creating")
	if _, err := e.ctr.Exec(ctx, spec.CommandLine("create")...); err != nil {
		return err
	}
	e.status("Container", spec.Name, "Created")
	return nil
}

func (e *Engine) handleOrphans(ctx context.Context, p *types.Project, o CreateOptions) error {
	all, err := e.ctr.ListContainers(ctx, true)
	if err != nil {
		return err
	}
	var orphans []ctr.Container
	for _, c := range ProjectContainers(all, p.Name, false) {
		svc := c.Labels()[LabelService]
		if _, ok := p.Services[svc]; ok {
			continue
		}
		if _, ok := p.DisabledServices[svc]; ok {
			continue
		}
		orphans = append(orphans, c)
	}
	if len(orphans) == 0 || o.IgnoreOrphans {
		return nil
	}
	if !o.RemoveOrphans {
		var names []string
		for _, c := range orphans {
			names = append(names, c.Name())
		}
		e.warnf("Found orphan containers (%s) for this project. If you removed or renamed this service in your compose file, you can run this command with the --remove-orphans flag to clean it up.", strings.Join(names, ", "))
		return nil
	}
	for _, c := range orphans {
		if err := e.removeContainer(ctx, c, types.ServiceConfig{}, true, o.Timeout); err != nil {
			return err
		}
	}
	return nil
}

func (e *Engine) removeContainer(ctx context.Context, c ctr.Container, s types.ServiceConfig, announce bool, timeout *int) error {
	if c.Running() {
		if err := e.stopContainer(ctx, c.Name(), s, timeout, announce); err != nil {
			return err
		}
	}
	if announce {
		e.status("Container", c.Name(), "Removing")
	}
	e.markStopped(c.Name())
	if _, err := e.ctr.Exec(ctx, "delete", "--force", c.Name()); err != nil && !ctr.IsNotFound(err) {
		return err
	}
	e.clearStopped(c.Name())
	if announce {
		e.status("Container", c.Name(), "Removed")
	}
	return nil
}

func (e *Engine) removeAnonymousVolumes(ctx context.Context, containerName string) {
	vols, err := e.ctr.ListVolumes(ctx)
	if err != nil {
		return
	}
	prefix := sanitizeName(containerName) + "_"
	for _, v := range vols {
		if v.Configuration.Labels[LabelAnonymous] == "true" && strings.HasPrefix(v.Name(), prefix) {
			_, _ = e.ctr.Exec(ctx, "volume", "delete", v.Name())
		}
	}
}

// ---- start / stop ----

func stopTimeout(s types.ServiceConfig, override *int) int {
	if override != nil {
		return *override
	}
	if s.StopGracePeriod != nil {
		return int(time.Duration(*s.StopGracePeriod).Seconds())
	}
	return 10
}

func (e *Engine) stopContainer(ctx context.Context, name string, s types.ServiceConfig, timeout *int, announce bool) error {
	for _, hook := range s.PreStop {
		if err := e.runHook(ctx, name, hook); err != nil {
			e.warnf("pre_stop hook for %s failed: %v", name, err)
		}
	}
	if announce {
		e.status("Container", name, "Stopping")
	}
	e.markStopping(name)
	e.markStopped(name)
	args := []string{"stop", "--time", strconv.Itoa(stopTimeout(s, timeout))}
	if s.StopSignal != "" {
		args = append(args, "--signal", s.StopSignal)
	}
	if _, err := e.ctr.Exec(ctx, append(args, name)...); err != nil && !ctr.IsNotFound(err) {
		return err
	}
	if announce {
		e.status("Container", name, "Stopped")
	}
	return nil
}

// StartOptions controls Start.
type StartOptions struct {
	Services []string
	// Attach is called for each container started, to stream its output.
	Attach func(name, service string) io.Writer
	// Wait for running/healthy state after start.
	Wait        bool
	WaitTimeout time.Duration
	// OnExit is notified when an attached container exits.
	OnExit func(name, service string, code int)
}

// Start starts existing containers in dependency order, honouring
// depends_on conditions.
func (e *Engine) Start(ctx context.Context, p *types.Project, o StartOptions) error {
	all, err := e.ctr.ListContainers(ctx, true)
	if err != nil {
		return err
	}
	containers := ProjectContainers(all, p.Name, false)
	needExit := map[string]bool{}
	for _, s := range p.Services {
		for dep, d := range s.DependsOn {
			if d.Condition == types.ServiceConditionCompletedSuccessfully {
				needExit[dep] = true
			}
		}
	}
	err = graph.InDependencyOrder(ctx, p, func(ctx context.Context, name string, s types.ServiceConfig) error {
		if err := e.waitDependencies(ctx, p, s); err != nil {
			return err
		}
		return e.startService(ctx, p, s, ServiceContainers(containers, name), o, needExit[name])
	})
	if err != nil {
		return err
	}
	if o.Wait {
		return e.waitAll(ctx, p, o)
	}
	return nil
}

func (e *Engine) startService(ctx context.Context, p *types.Project, s types.ServiceConfig, cs []ctr.Container, o StartOptions, trackExit bool) error {
	for _, c := range cs {
		if c.Running() {
			if o.Attach != nil {
				e.follow(ctx, c.Name(), s.Name, o)
			}
			continue
		}
		if err := e.startContainer(ctx, p, c.Name(), s, o, trackExit); err != nil {
			return err
		}
	}
	return nil
}

func (e *Engine) startContainer(ctx context.Context, p *types.Project, name string, s types.ServiceConfig, o StartOptions, trackExit bool) error {
	// Make sure dependencies are resolvable before the process starts.
	if changed, err := e.refreshHosts(ctx, p); err == nil && changed {
		time.Sleep(dnsReloadDelay)
	}
	e.status("Container", name, "Starting")
	e.clearStopped(name)
	if o.Attach != nil || trackExit {
		var out io.Writer = io.Discard
		if o.Attach != nil {
			out = o.Attach(name, s.Name)
		}
		if err := e.startAttached(ctx, name, s, out, o.OnExit); err != nil {
			return err
		}
	} else {
		if _, err := e.ctr.Exec(ctx, "start", name); err != nil {
			return err
		}
	}
	e.status("Container", name, "Started")
	e.waitAddress(ctx, name)
	if _, err := e.refreshHosts(ctx, p); err != nil {
		e.warnf("updating DNS records: %v", err)
	}
	for _, hook := range s.PostStart {
		if err := e.runHook(ctx, name, hook); err != nil {
			return fmt.Errorf("post_start hook for %s: %w", name, err)
		}
	}
	return nil
}

func (e *Engine) runHook(ctx context.Context, name string, hook types.ServiceHook) error {
	args := []string{"exec"}
	if hook.User != "" {
		args = append(args, "--user", hook.User)
	}
	if hook.WorkingDir != "" {
		args = append(args, "--workdir", hook.WorkingDir)
	}
	for _, k := range sortedKeys(hook.Environment) {
		if v := hook.Environment[k]; v != nil {
			args = append(args, "--env", k+"="+*v)
		}
	}
	args = append(args, name)
	args = append(args, hook.Command...)
	return e.ctr.Stream(ctx, nil, e.Out, e.Err, args...)
}

// Stop stops containers in reverse dependency order.
func (e *Engine) Stop(ctx context.Context, p *types.Project, services []string, timeout *int) error {
	all, err := e.ctr.ListContainers(ctx, false)
	if err != nil {
		return err
	}
	cs := FilterServices(ProjectContainers(all, p.Name, true), services)
	return e.inReverseOrder(ctx, p, cs, func(ctx context.Context, c ctr.Container, s types.ServiceConfig) error {
		return e.stopContainer(ctx, c.Name(), s, timeout, true)
	})
}

// inReverseOrder applies fn to containers, dependents first. Containers of
// services unknown to the project are handled last.
func (e *Engine) inReverseOrder(ctx context.Context, p *types.Project, cs []ctr.Container, fn func(context.Context, ctr.Container, types.ServiceConfig) error) error {
	done := map[string]bool{}
	var mu sync.Mutex
	err := graph.InDependencyOrder(ctx, p, func(ctx context.Context, name string, s types.ServiceConfig) error {
		var wg sync.WaitGroup
		var errs []error
		var emu sync.Mutex
		for _, c := range ServiceContainers(cs, name) {
			mu.Lock()
			done[c.Name()] = true
			mu.Unlock()
			wg.Add(1)
			go func(c ctr.Container) {
				defer wg.Done()
				if err := fn(ctx, c, s); err != nil {
					emu.Lock()
					errs = append(errs, err)
					emu.Unlock()
				}
			}(c)
		}
		wg.Wait()
		return errors.Join(errs...)
	}, graph.InReverseOrder)
	if err != nil {
		return err
	}
	var rest []error
	for _, c := range cs {
		if !done[c.Name()] {
			if err := fn(ctx, c, types.ServiceConfig{Name: c.Labels()[LabelService]}); err != nil {
				rest = append(rest, err)
			}
		}
	}
	return errors.Join(rest...)
}

// DownOptions controls Down.
type DownOptions struct {
	RemoveOrphans bool
	Volumes       bool
	Images        string // "", local, all
	Timeout       *int
	Services      []string
}

// Down stops and removes containers, networks and optionally volumes/images.
func (e *Engine) Down(ctx context.Context, p *types.Project, o DownOptions) error {
	all, err := e.ctr.ListContainers(ctx, true)
	if err != nil {
		return err
	}
	cs := ProjectContainers(all, p.Name, true)
	if len(o.Services) > 0 {
		cs = FilterServices(cs, o.Services)
	} else if !o.RemoveOrphans && len(p.Services) > 0 {
		var kept []ctr.Container
		var orphans []string
		for _, c := range cs {
			if _, ok := p.Services[c.Labels()[LabelService]]; ok || c.Labels()[LabelOneoff] == "True" {
				kept = append(kept, c)
			} else {
				orphans = append(orphans, c.Name())
			}
		}
		if len(orphans) > 0 {
			e.warnf("Found orphan containers (%s) for this project. Use --remove-orphans to clean them up.", strings.Join(orphans, ", "))
		}
		cs = kept
	}
	err = e.inReverseOrder(ctx, p, cs, func(ctx context.Context, c ctr.Container, s types.ServiceConfig) error {
		if err := e.removeContainer(ctx, c, s, true, o.Timeout); err != nil {
			return err
		}
		if o.Volumes {
			e.removeAnonymousVolumes(ctx, c.Name())
		}
		return nil
	})
	if err != nil {
		return err
	}
	if len(o.Services) > 0 {
		return nil
	}
	// Remaining containers (orphans we kept) still need the network.
	all, err = e.ctr.ListContainers(ctx, true)
	if err != nil {
		return err
	}
	remaining := ProjectContainers(all, p.Name, true)
	if len(remaining) == 0 {
		if err := e.removeDNS(ctx, p.Name); err != nil {
			return err
		}
		if err := e.removeNetworks(ctx, p); err != nil {
			return err
		}
	}
	if o.Volumes {
		if err := e.removeVolumes(ctx, p); err != nil {
			return err
		}
	}
	if o.Images != "" {
		e.removeImages(ctx, p, o.Images)
	}
	if len(remaining) == 0 && !e.ctr.DryRun {
		_ = os.RemoveAll(e.stateDir(p.Name))
	}
	return nil
}

func (e *Engine) removeNetworks(ctx context.Context, p *types.Project) error {
	nets, err := e.ctr.ListNetworks(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, n := range nets {
		if n.Configuration.Labels[LabelProject] != p.Name {
			continue
		}
		key := n.Configuration.Labels[LabelNetwork]
		if cfg, ok := p.Networks[key]; ok && bool(cfg.External) {
			continue
		}
		e.status("Network", n.ID, "Removing")
		if _, err := e.ctr.Exec(ctx, "network", "delete", n.ID); err != nil && !ctr.IsNotFound(err) {
			errs = append(errs, err)
			continue
		}
		e.status("Network", n.ID, "Removed")
	}
	return errors.Join(errs...)
}

func (e *Engine) removeVolumes(ctx context.Context, p *types.Project) error {
	for key, cfg := range p.Volumes {
		if bool(cfg.External) {
			continue
		}
		dir := e.sharedVolumeDir(p, key)
		if _, err := os.Stat(dir); err == nil {
			e.status("Volume", VolumeName(p, key), "Removing")
			if !e.ctr.DryRun {
				if err := os.RemoveAll(dir); err != nil {
					return err
				}
			}
			e.status("Volume", VolumeName(p, key), "Removed")
		}
	}
	vols, err := e.ctr.ListVolumes(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, v := range vols {
		l := v.Configuration.Labels
		if l[LabelProject] != p.Name {
			continue
		}
		if cfg, ok := p.Volumes[l[LabelVolume]]; ok && bool(cfg.External) {
			continue
		}
		if l[LabelHostPath] != "" {
			continue // stands in for host data, which `down -v` never deletes
		}
		e.status("Volume", v.Name(), "Removing")
		if _, err := e.ctr.Exec(ctx, "volume", "delete", v.Name()); err != nil && !ctr.IsNotFound(err) {
			errs = append(errs, err)
			continue
		}
		e.status("Volume", v.Name(), "Removed")
	}
	return errors.Join(errs...)
}

func (e *Engine) removeImages(ctx context.Context, p *types.Project, mode string) {
	seen := map[string]bool{}
	for _, name := range p.ServiceNames() {
		s := p.Services[name]
		if mode == "local" && s.Image != "" {
			continue
		}
		img := ImageName(p.Name, s)
		if seen[img] {
			continue
		}
		seen[img] = true
		e.status("Image", img, "Removing")
		if _, err := e.ctr.Exec(ctx, "image", "delete", img); err != nil {
			e.warnf("removing image %s: %v", img, err)
			continue
		}
		e.status("Image", img, "Removed")
	}
}

// Remove deletes stopped containers (or all with force).
func (e *Engine) Remove(ctx context.Context, p *types.Project, services []string, force, stop, volumes bool, timeout *int) error {
	all, err := e.ctr.ListContainers(ctx, true)
	if err != nil {
		return err
	}
	cs := FilterServices(ProjectContainers(all, p.Name, true), services)
	var targets []ctr.Container
	for _, c := range cs {
		if c.Running() && !stop && !force {
			continue
		}
		targets = append(targets, c)
	}
	if len(targets) == 0 {
		fmt.Fprintln(e.Err, "No stopped containers")
		return nil
	}
	if stop {
		if err := e.Stop(ctx, p, services, timeout); err != nil {
			return err
		}
	}
	var errs []error
	for _, c := range targets {
		if err := e.removeContainer(ctx, c, types.ServiceConfig{}, true, timeout); err != nil {
			errs = append(errs, err)
		}
		if volumes {
			e.removeAnonymousVolumes(ctx, c.Name())
		}
	}
	_, _ = e.refreshHosts(ctx, p)
	return errors.Join(errs...)
}

// Kill sends a signal to running containers.
func (e *Engine) Kill(ctx context.Context, p *types.Project, services []string, signal string) error {
	all, err := e.ctr.ListContainers(ctx, false)
	if err != nil {
		return err
	}
	cs := FilterServices(ProjectContainers(all, p.Name, true), services)
	if len(cs) == 0 {
		return errors.New("no container to kill")
	}
	var errs []error
	for _, c := range cs {
		e.markStopping(c.Name())
		e.markStopped(c.Name())
		e.status("Container", c.Name(), "Killing")
		args := []string{"kill"}
		if signal != "" {
			args = append(args, "--signal", signal)
		}
		if _, err := e.ctr.Exec(ctx, append(args, c.Name())...); err != nil {
			errs = append(errs, err)
			continue
		}
		e.status("Container", c.Name(), "Killed")
	}
	return errors.Join(errs...)
}

// Restart stops then starts containers.
func (e *Engine) Restart(ctx context.Context, p *types.Project, services []string, timeout *int, noDeps bool) error {
	all, err := e.ctr.ListContainers(ctx, true)
	if err != nil {
		return err
	}
	cs := FilterServices(ProjectContainers(all, p.Name, false), services)
	if err := e.inReverseOrder(ctx, p, cs, func(ctx context.Context, c ctr.Container, s types.ServiceConfig) error {
		if !c.Running() {
			return nil
		}
		e.status("Container", c.Name(), "Restarting")
		return e.stopContainer(ctx, c.Name(), s, timeout, false)
	}); err != nil {
		return err
	}
	sel := services
	if len(sel) == 0 {
		sel = p.ServiceNames()
	}
	sp := p
	if noDeps {
		sp, err = p.WithSelectedServices(sel, types.IgnoreDependencies)
	} else {
		sp, err = p.WithSelectedServices(sel)
	}
	if err != nil {
		return err
	}
	return e.Start(ctx, sp, StartOptions{Services: services})
}

// sortedServiceNames returns service names sorted.
func sortedServiceNames(p *types.Project) []string {
	names := p.ServiceNames()
	sort.Strings(names)
	return names
}

// waitAddress waits briefly for a started container to report its IP.
func (e *Engine) waitAddress(ctx context.Context, name string) {
	if e.ctr.DryRun {
		return
	}
	for i := 0; i < 50; i++ {
		c, err := e.ctr.InspectContainer(ctx, name)
		if err != nil || c == nil || !c.Running() {
			return
		}
		for _, n := range c.Status.Networks {
			if n.IP() != "" {
				return
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(100 * time.Millisecond):
		}
	}
}
