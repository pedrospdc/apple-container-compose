package compose

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/compose-spec/compose-go/v2/types"
)

// Apple container exposes no exit code once a container stopped. The only
// place it is reported is the exit status of an attached `container start
// -a`, so containers whose exit status matters are started attached and
// supervised by this process.

type exitWaiter struct {
	done     chan struct{}
	code     int // -1 when unknown
	stopping bool
	cmd      *exec.Cmd
}

func (e *Engine) waiter(name string) *exitWaiter {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.exits[name]
}

// markStopping records that we are stopping a container on purpose, so the
// restart supervisor leaves it alone.
func (e *Engine) markStopping(name string) {
	e.mu.Lock()
	if w := e.exits[name]; w != nil {
		w.stopping = true
	}
	e.mu.Unlock()
}

// tailBuffer keeps the last bytes written to it.
type tailBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf.Write(p)
	if t.buf.Len() > 8192 {
		b := t.buf.Bytes()
		t.buf.Reset()
		t.buf.Write(b[len(b)-4096:])
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.buf.String()
}

// startAttached runs `container start --attach` in the background and
// returns once the container is running (or already exited).
func (e *Engine) startAttached(ctx context.Context, name string, s types.ServiceConfig, out io.Writer, onExit func(name, service string, code int)) error {
	service := s.Name
	if e.ctr.DryRun {
		e.ctr.Command(ctx, "start", "--attach", name)
		return nil
	}
	w := &exitWaiter{done: make(chan struct{}), code: -1}
	e.mu.Lock()
	e.exits[name] = w
	e.mu.Unlock()

	var policy restartPolicy
	if onExit != nil {
		// Only supervised (attached) runs honour restart policies.
		policy = restartPolicyOf(s)
	}

	errTail := &tailBuffer{}
	launch := func() (*exec.Cmd, error) {
		// Not bound to ctx: stopping is done through `container stop` so
		// that Ctrl-C produces a graceful shutdown.
		cmd := exec.Command(e.ctr.Bin, "start", "--attach", name)
		cmd.Stdout = out
		cmd.Stderr = io.MultiWriter(out, errTail)
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if e.ctr.Debug {
			fmt.Fprintf(e.Err, "+ container start --attach %s\n", name)
		}
		return cmd, cmd.Start()
	}
	cmd, err := launch()
	if err != nil {
		return err
	}
	w.cmd = cmd
	go func() {
		restarts := 0
		for {
			err := cmd.Wait()
			code := 0
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				code = ee.ExitCode()
			} else if err != nil {
				code = -1
			}
			e.mu.Lock()
			stopping := w.stopping
			e.mu.Unlock()
			if !stopping && ctx.Err() == nil && policy.shouldRestart(code, restarts) {
				restarts++
				time.Sleep(policy.delay(restarts))
				if c, err := launch(); err == nil {
					cmd = c
					e.mu.Lock()
					w.cmd = c
					e.mu.Unlock()
					continue
				}
			}
			e.mu.Lock()
			w.code = code
			e.mu.Unlock()
			close(w.done)
			if onExit != nil {
				onExit(name, service, code)
			}
			return
		}
	}()

	// Wait until it is running, or exited.
	deadline := time.Now().Add(5 * time.Minute)
	for time.Now().Before(deadline) {
		select {
		case <-w.done:
			if w.code != 0 && strings.Contains(errTail.String(), "Error: ") {
				return fmt.Errorf("starting %s: %s", name, strings.TrimSpace(lastLine(errTail.String())))
			}
			return nil
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(150 * time.Millisecond):
		}
		c, err := e.ctr.InspectContainer(ctx, name)
		if err == nil && c != nil && c.Running() {
			return nil
		}
	}
	return fmt.Errorf("timed out starting %s", name)
}

// follow streams the output of an already running container.
func (e *Engine) follow(ctx context.Context, name, service string, o StartOptions) {
	if e.ctr.DryRun {
		return
	}
	out := o.Attach(name, service)
	w := &exitWaiter{done: make(chan struct{}), code: -1}
	e.mu.Lock()
	e.exits[name] = w
	e.mu.Unlock()
	cmd := exec.Command(e.ctr.Bin, "logs", "--follow", "-n", "0", name)
	cmd.Stdout = out
	cmd.Stderr = out
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		close(w.done)
		return
	}
	w.cmd = cmd
	go func() {
		_ = cmd.Wait()
		close(w.done)
		if o.OnExit != nil {
			o.OnExit(name, service, -1)
		}
	}()
}

func lastLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndex(s, "\n"); i >= 0 {
		return s[i+1:]
	}
	return s
}

// ---- restart policies ----

type restartPolicy struct {
	mode       string // no, always, on-failure, unless-stopped
	maxRetries int
}

func restartPolicyOf(s types.ServiceConfig) restartPolicy {
	if s.Deploy != nil && s.Deploy.RestartPolicy != nil {
		rp := s.Deploy.RestartPolicy
		p := restartPolicy{mode: rp.Condition}
		switch rp.Condition {
		case "any", "":
			p.mode = "always"
		case "none":
			p.mode = "no"
		}
		if rp.MaxAttempts != nil {
			p.maxRetries = int(*rp.MaxAttempts)
		}
		return p
	}
	mode, n, _ := strings.Cut(s.Restart, ":")
	p := restartPolicy{mode: mode}
	if n != "" {
		p.maxRetries, _ = strconv.Atoi(n)
	}
	return p
}

func (p restartPolicy) shouldRestart(code, restarts int) bool {
	switch p.mode {
	case "always", "unless-stopped":
		return true
	case "on-failure":
		return code != 0 && (p.maxRetries == 0 || restarts < p.maxRetries)
	}
	return false
}

func (p restartPolicy) delay(restarts int) time.Duration {
	d := 100 * time.Millisecond << min(restarts, 9)
	if d > time.Minute {
		d = time.Minute
	}
	return d
}

// ---- dependency conditions ----

func (e *Engine) waitDependencies(ctx context.Context, p *types.Project, s types.ServiceConfig) error {
	for _, dep := range sortedKeys(s.DependsOn) {
		d := s.DependsOn[dep]
		depSvc, ok := p.Services[dep]
		if !ok {
			continue
		}
		var err error
		switch d.Condition {
		case types.ServiceConditionHealthy:
			err = e.waitServiceHealthy(ctx, p, depSvc)
		case types.ServiceConditionCompletedSuccessfully:
			err = e.waitServiceCompleted(ctx, p, depSvc)
		}
		if err != nil {
			if !d.Required {
				e.warnf("optional dependency %s of %s: %v", dep, s.Name, err)
				continue
			}
			return fmt.Errorf("dependency failed to start: %w", err)
		}
	}
	return nil
}

func (e *Engine) serviceContainerNames(ctx context.Context, p *types.Project, service string) ([]string, error) {
	all, err := e.ctr.ListContainers(ctx, true)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, c := range ServiceContainers(ProjectContainers(all, p.Name, false), service) {
		names = append(names, c.Name())
	}
	return names, nil
}

func (e *Engine) waitServiceCompleted(ctx context.Context, p *types.Project, s types.ServiceConfig) error {
	if e.ctr.DryRun {
		return nil
	}
	names, err := e.serviceContainerNames(ctx, p, s.Name)
	if err != nil {
		return err
	}
	for _, name := range names {
		e.status("Container", name, "Waiting")
		if w := e.waiter(name); w != nil {
			select {
			case <-w.done:
			case <-ctx.Done():
				return ctx.Err()
			}
			if w.code > 0 {
				return fmt.Errorf("service %q didn't complete successfully: exit %d", s.Name, w.code)
			}
		} else {
			// Not started by us: wait for it to stop; the code is unknown.
			for {
				c, err := e.ctr.InspectContainer(ctx, name)
				if err != nil {
					return err
				}
				if c == nil || !c.Running() {
					break
				}
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(500 * time.Millisecond):
				}
			}
		}
		e.status("Container", name, "Exited")
	}
	return nil
}
