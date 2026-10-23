package compose

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/compose-spec/compose-go/v2/types"

	"github.com/pedrospdc/apple-container-compose/internal/ctr"
)

// SelectServices narrows the project to services (plus dependencies unless
// noDeps). An empty selection keeps every service.
func SelectServices(p *types.Project, services []string, noDeps bool) (*types.Project, error) {
	if len(services) == 0 {
		return p, nil
	}
	if noDeps {
		return p.WithSelectedServices(services, types.IgnoreDependencies)
	}
	return p.WithSelectedServices(services)
}

// UpOptions mirrors `docker compose up`.
type UpOptions struct {
	Create             CreateOptions
	Services           []string
	NoDeps             bool
	NoStart            bool
	Detach             bool
	Wait               bool
	WaitTimeout        time.Duration
	AbortOnExit        bool
	AbortOnFailure     bool
	ExitCodeFrom       string
	Attach             []string
	NoAttach           []string
	AttachDependencies bool
	NoLogPrefix        bool
	NoColor            bool
	Timestamps         bool
	Timeout            *int
	Watch              bool
}

// Up creates and starts the project. It returns the process exit code.
func (e *Engine) Up(ctx context.Context, p *types.Project, o UpOptions) (int, error) {
	sp, err := SelectServices(p, o.Services, o.NoDeps)
	if err != nil {
		return 1, err
	}
	o.Create.Services = o.Services
	if err := e.Create(ctx, sp, o.Create); err != nil {
		return 1, err
	}
	if o.NoStart {
		return 0, nil
	}
	if o.Detach {
		err := e.Start(ctx, sp, StartOptions{Wait: o.Wait, WaitTimeout: o.WaitTimeout})
		e.EnsureSupervisor(ctx, p.Name)
		if err != nil {
			return 1, err
		}
		if o.Watch {
			return 0, e.Watch(ctx, sp, WatchOptions{Services: o.Services})
		}
		return 0, nil
	}
	return e.upAttached(ctx, sp, o)
}

func (e *Engine) upAttached(ctx context.Context, p *types.Project, o UpOptions) (int, error) {
	// Shutdown must still work after the first Ctrl-C cancels ctx.
	bg := context.WithoutCancel(ctx)

	attached := map[string]bool{}
	switch {
	case len(o.Attach) > 0:
		for _, s := range o.Attach {
			attached[s] = true
		}
	case len(o.Services) > 0 && !o.AttachDependencies:
		for _, s := range o.Services {
			attached[s] = true
		}
	default:
		for _, s := range p.ServiceNames() {
			attached[s] = true
		}
	}
	for _, s := range o.NoAttach {
		delete(attached, s)
	}
	if o.ExitCodeFrom != "" {
		o.AbortOnExit = true
		if _, ok := p.Services[o.ExitCodeFrom]; !ok {
			return 1, fmt.Errorf("no such service: %s", o.ExitCodeFrom)
		}
	}

	lp := NewLogPrinter(e.Out)
	lp.NoPrefix, lp.NoColor, lp.Timestamps = o.NoLogPrefix, o.NoColor, o.Timestamps
	if names, err := e.ctr.ListContainers(ctx, true); err == nil {
		for _, c := range ProjectContainers(names, p.Name, false) {
			if attached[c.Labels()[LabelService]] {
				lp.Register(c.Name())
			}
		}
	}

	var (
		mu        sync.Mutex
		exitCode  = 0
		abort     = make(chan struct{})
		abortOnce sync.Once
		tracked   []string
	)
	triggerAbort := func() { abortOnce.Do(func() { close(abort) }) }
	onExit := func(name, service string, code int) {
		if !attached[service] {
			return
		}
		if code >= 0 {
			lp.Event(name, colorize(fmt.Sprintf("%s exited with code %d", name, code), "1", !o.NoColor))
		}
		mu.Lock()
		if o.ExitCodeFrom != "" && service == o.ExitCodeFrom {
			exitCode = max(code, 0)
		} else if o.ExitCodeFrom == "" && code > 0 && (o.AbortOnExit || o.AbortOnFailure) {
			exitCode = code
		}
		mu.Unlock()
		if o.AbortOnExit && (o.ExitCodeFrom == "" || service == o.ExitCodeFrom) {
			triggerAbort()
		}
		if o.AbortOnFailure && code > 0 {
			triggerAbort()
		}
	}

	sigs := make(chan os.Signal, 2)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigs)

	startErr := make(chan error, 1)
	go func() {
		startErr <- e.Start(ctx, p, StartOptions{
			Attach: func(name, service string) io.Writer {
				if attached[service] {
					mu.Lock()
					tracked = append(tracked, name)
					mu.Unlock()
					return lp.Writer(name)
				}
				return io.Discard
			},
			OnExit: onExit,
		})
	}()

	var watchErr chan error
	interrupted := false
	stopAll := func(force bool) {
		if force {
			_ = e.Kill(bg, p, nil, "KILL")
			return
		}
		_ = e.Stop(bg, p, nil, o.Timeout)
	}

	// Wait for start to finish (or be interrupted).
	select {
	case err := <-startErr:
		if err != nil {
			stopAll(false)
			return 1, err
		}
	case <-sigs:
		fmt.Fprintln(e.Err, "Gracefully stopping... (press Ctrl+C again to force)")
		go func() { <-sigs; stopAll(true) }()
		stopAll(false)
		return 130, nil
	}
	if o.Watch {
		watchErr = make(chan error, 1)
		go func() { watchErr <- e.Watch(ctx, p, WatchOptions{}) }()
	}

	allDone := make(chan struct{})
	go func() {
		mu.Lock()
		names := append([]string(nil), tracked...)
		mu.Unlock()
		for _, n := range names {
			if w := e.waiter(n); w != nil {
				<-w.done
			}
		}
		close(allDone)
	}()

	select {
	case <-allDone:
	case <-abort:
		fmt.Fprintln(e.Err, "Aborting on container exit...")
		stopAll(false)
	case err := <-watchErr:
		stopAll(false)
		if err != nil {
			return 1, err
		}
	case <-sigs:
		interrupted = true
		fmt.Fprintln(e.Err, "Gracefully stopping... (press Ctrl+C again to force)")
		go func() { <-sigs; stopAll(true) }()
		stopAll(false)
	}
	// Let log streams drain.
	select {
	case <-allDone:
	case <-time.After(3 * time.Second):
	}
	mu.Lock()
	defer mu.Unlock()
	if interrupted && exitCode == 0 {
		return 130, nil
	}
	return exitCode, nil
}

// prepare provisions images, networks, volumes and DNS without creating
// service containers.
func (e *Engine) prepare(ctx context.Context, p *types.Project, img ImageOptions) error {
	if err := e.EnsureImages(ctx, p, img); err != nil {
		return err
	}
	if err := e.EnsureNetworks(ctx, p); err != nil {
		return err
	}
	if _, err := e.EnsureVolumes(ctx, p); err != nil {
		return err
	}
	return e.ensureDNS(ctx, p)
}

// RunOptions mirrors `docker compose run`.
type RunOptions struct {
	Service      string
	Command      []string
	Name         string
	Detach       bool
	Remove       bool
	NoDeps       bool
	Entrypoint   *types.ShellCommand
	Env          []string
	Labels       []string
	User         string
	WorkDir      string
	Publish      []string
	ServicePorts bool
	UseAliases   bool
	Volumes      []string
	Interactive  bool
	TTY          bool
	CapAdd       []string
	CapDrop      []string
	Images       ImageOptions
	RemoveOrphan bool
}

// Run starts a one-off container for a service and returns its exit code.
func (e *Engine) Run(ctx context.Context, p *types.Project, o RunOptions) (int, error) {
	s, err := p.GetService(o.Service)
	if err != nil {
		return 1, err
	}
	sp, err := SelectServices(p, []string{o.Service}, o.NoDeps)
	if err != nil {
		return 1, err
	}
	if err := e.prepare(ctx, sp, o.Images); err != nil {
		return 1, err
	}
	if !o.NoDeps && len(s.GetDependencies()) > 0 {
		deps := sp.WithServicesDisabled(o.Service)
		if err := e.Create(ctx, deps, CreateOptions{IgnoreOrphans: true, RemoveOrphans: o.RemoveOrphan, Images: ImageOptions{NoBuild: true, Pull: types.PullPolicyNever}}); err != nil {
			return 1, err
		}
		if err := e.Start(ctx, deps, StartOptions{}); err != nil {
			return 1, err
		}
		if err := e.waitDependencies(ctx, sp, s); err != nil {
			return 1, err
		}
	}
	name := o.Name
	if name == "" {
		b := make([]byte, 6)
		_, _ = rand.Read(b)
		name = fmt.Sprintf("%s-%s-run-%s", p.Name, s.Name, hex.EncodeToString(b))
	}
	var cmd types.ShellCommand
	if len(o.Command) > 0 {
		cmd = o.Command
	}
	s.ContainerName = "" // one-off containers never reuse container_name
	spec, err := e.toContainerSpec(ctx, p, s, createOptions{
		Name: name, Number: 1, Oneoff: true, NoPorts: !o.ServicePorts,
		ExtraPorts: o.Publish, ExtraVolumes: o.Volumes, ExtraLabels: o.Labels, ExtraEnv: o.Env,
		Entrypoint: o.Entrypoint, Command: cmd, User: o.User, WorkDir: o.WorkDir,
		Interactive: o.Interactive, TTY: o.TTY, Remove: o.Remove && !o.Detach,
		CapAdd: o.CapAdd, CapDrop: o.CapDrop,
	})
	if err != nil {
		return 1, err
	}
	for _, w := range spec.Warnings {
		e.warnf("service %q: %s", s.Name, w)
	}
	if o.Detach {
		if _, err := e.ctr.Exec(ctx, spec.CommandLine("run", "--detach", "--progress", "none")...); err != nil {
			return 1, err
		}
		_, _ = e.refreshHosts(ctx, p)
		fmt.Fprintln(e.Out, name)
		return 0, nil
	}
	// Publish DNS records once the container is up.
	done := make(chan struct{})
	go func() {
		for i := 0; i < 600; i++ {
			select {
			case <-done:
				return
			case <-time.After(200 * time.Millisecond):
			}
			if c, err := e.ctr.InspectContainer(ctx, name); err == nil && c != nil && c.Running() {
				_, _ = e.refreshHosts(ctx, p)
				return
			}
		}
	}()
	err = e.ctr.Stream(ctx, os.Stdin, e.Out, e.Err, spec.CommandLine("run", "--progress", "none")...)
	close(done)
	_, _ = e.refreshHosts(ctx, p)
	var ee *ctr.ExitError
	if errors.As(err, &ee) {
		return ee.Code, nil
	}
	if err != nil {
		return 1, err
	}
	return 0, nil
}

// ExecOptions mirrors `docker compose exec`.
type ExecOptions struct {
	Service     string
	Index       int
	Command     []string
	Detach      bool
	Env         []string
	User        string
	WorkDir     string
	Interactive bool
	TTY         bool
}

// FindContainer returns the container for service replica index.
func (e *Engine) FindContainer(ctx context.Context, p *types.Project, service string, index int, running bool) (*ctr.Container, error) {
	all, err := e.ctr.ListContainers(ctx, !running)
	if err != nil {
		return nil, err
	}
	cs := ServiceContainers(ProjectContainers(all, p.Name, false), service)
	if index <= 0 {
		index = 1
	}
	for i := range cs {
		if ContainerNumber(cs[i]) == index {
			return &cs[i], nil
		}
	}
	if running {
		return nil, fmt.Errorf("service %q is not running", service)
	}
	return nil, fmt.Errorf("no container found for service %q", service)
}

// Exec runs a command in a running service container.
func (e *Engine) Exec(ctx context.Context, p *types.Project, o ExecOptions) (int, error) {
	c, err := e.FindContainer(ctx, p, o.Service, o.Index, true)
	if err != nil {
		return 1, err
	}
	args := []string{"exec"}
	if o.Detach {
		args = append(args, "--detach")
	}
	if o.Interactive {
		args = append(args, "--interactive")
	}
	if o.TTY {
		args = append(args, "--tty")
	}
	if o.User != "" {
		args = append(args, "--user", o.User)
	}
	if o.WorkDir != "" {
		args = append(args, "--workdir", o.WorkDir)
	}
	for _, kv := range o.Env {
		args = append(args, "--env", kv)
	}
	args = append(args, c.Name())
	args = append(args, o.Command...)
	err = e.ctr.Stream(ctx, os.Stdin, e.Out, e.Err, args...)
	var ee *ctr.ExitError
	if errors.As(err, &ee) {
		return ee.Code, nil
	}
	if err != nil {
		return 1, err
	}
	return 0, nil
}
