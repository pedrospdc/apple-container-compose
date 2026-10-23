package compose

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/compose-spec/compose-go/v2/types"
)

// Apple container has no healthcheck support, so compose runs the probes
// itself (through `container exec`) whenever it needs to know a service is
// healthy: depends_on service_healthy, `up --wait` and `ps` while waiting.

type healthSpec struct {
	test          []string
	interval      time.Duration
	timeout       time.Duration
	startPeriod   time.Duration
	startInterval time.Duration
	retries       int
}

// healthSpecFor returns the effective healthcheck, falling back to the
// image's HEALTHCHECK instruction. nil means none.
func (e *Engine) healthSpecFor(ctx context.Context, p *types.Project, s types.ServiceConfig) *healthSpec {
	h := &healthSpec{interval: 30 * time.Second, timeout: 30 * time.Second, retries: 3, startInterval: 5 * time.Second}
	if hc := s.HealthCheck; hc != nil {
		if hc.Disable || (len(hc.Test) > 0 && hc.Test[0] == "NONE") {
			return nil
		}
		h.test = hc.Test
		if hc.Interval != nil {
			h.interval = time.Duration(*hc.Interval)
		}
		if hc.Timeout != nil {
			h.timeout = time.Duration(*hc.Timeout)
		}
		if hc.StartPeriod != nil {
			h.startPeriod = time.Duration(*hc.StartPeriod)
		}
		if hc.StartInterval != nil {
			h.startInterval = time.Duration(*hc.StartInterval)
		}
		if hc.Retries != nil {
			h.retries = int(*hc.Retries)
		}
	}
	if len(h.test) == 0 {
		img, err := e.ctr.InspectImage(ctx, ImageName(p.Name, s))
		if err != nil || img == nil {
			return nil
		}
		v := img.Variant("arm64")
		if v == nil || v.Config.Config.Healthcheck == nil || len(v.Config.Config.Healthcheck.Test) == 0 {
			return nil
		}
		ih := v.Config.Config.Healthcheck
		if ih.Test[0] == "NONE" {
			return nil
		}
		h.test = ih.Test
		if ih.Interval > 0 {
			h.interval = time.Duration(ih.Interval)
		}
		if ih.Timeout > 0 {
			h.timeout = time.Duration(ih.Timeout)
		}
		if ih.StartPeriod > 0 {
			h.startPeriod = time.Duration(ih.StartPeriod)
		}
		if ih.Retries > 0 {
			h.retries = ih.Retries
		}
	}
	if len(h.test) == 0 {
		return nil
	}
	return h
}

func (h *healthSpec) command() []string {
	switch h.test[0] {
	case "CMD":
		return h.test[1:]
	case "CMD-SHELL":
		return []string{"/bin/sh", "-c", h.test[1]}
	default:
		return h.test
	}
}

func (e *Engine) setHealth(name, state string) {
	e.mu.Lock()
	e.health[name] = state
	e.mu.Unlock()
}

// Health returns the last known health of a container in this process.
func (e *Engine) Health(name string) string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.health[name]
}

var errUnhealthy = errors.New("unhealthy")

// waitHealthy probes a container until it is healthy, unhealthy or exits.
// Probes run more often than `interval` so a healthy service is noticed
// quickly; only failures spaced by `interval` count toward `retries`.
func (e *Engine) waitHealthy(ctx context.Context, name string, h *healthSpec) error {
	start := time.Now()
	failures := 0
	var lastCounted time.Time
	poll := min(h.interval, h.startInterval, 2*time.Second)
	if poll <= 0 {
		poll = time.Second
	}
	e.setHealth(name, "starting")
	for {
		c, err := e.ctr.InspectContainer(ctx, name)
		if err != nil {
			return err
		}
		if c == nil || !c.Running() {
			e.setHealth(name, "")
			return fmt.Errorf("container %s exited", name)
		}
		pctx, cancel := context.WithTimeout(ctx, h.timeout)
		_, err = e.ctr.Output(pctx, append([]string{"exec", name}, h.command()...)...)
		cancel()
		if err == nil {
			e.setHealth(name, "healthy")
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		now := time.Now()
		if now.Sub(start) >= h.startPeriod && now.Sub(lastCounted) >= h.interval {
			failures++
			lastCounted = now
			if failures >= h.retries {
				e.setHealth(name, "unhealthy")
				return fmt.Errorf("container %s is %w", name, errUnhealthy)
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(poll):
		}
	}
}

func (e *Engine) waitServiceHealthy(ctx context.Context, p *types.Project, s types.ServiceConfig) error {
	if e.ctr.DryRun {
		return nil
	}
	h := e.healthSpecFor(ctx, p, s)
	if h == nil {
		return fmt.Errorf("service %q has no healthcheck configured", s.Name)
	}
	names, err := e.serviceContainerNames(ctx, p, s.Name)
	if err != nil {
		return err
	}
	for _, name := range names {
		e.status("Container", name, "Waiting")
		if err := e.waitHealthy(ctx, name, h); err != nil {
			return err
		}
		e.status("Container", name, "Healthy")
	}
	return nil
}

// waitAll implements `up --wait`: every container must be running, and
// healthy when it has a healthcheck.
func (e *Engine) waitAll(ctx context.Context, p *types.Project, o StartOptions) error {
	if o.WaitTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, o.WaitTimeout)
		defer cancel()
	}
	for _, name := range p.ServiceNames() {
		if len(o.Services) > 0 && !contains(o.Services, name) {
			continue
		}
		s := p.Services[name]
		h := e.healthSpecFor(ctx, p, s)
		names, err := e.serviceContainerNames(ctx, p, name)
		if err != nil {
			return err
		}
		for _, cn := range names {
			if h != nil {
				e.status("Container", cn, "Waiting")
				if err := e.waitHealthy(ctx, cn, h); err != nil {
					return err
				}
				e.status("Container", cn, "Healthy")
				continue
			}
			c, err := e.ctr.InspectContainer(ctx, cn)
			if err != nil {
				return err
			}
			if c == nil || !c.Running() {
				if w := e.waiter(cn); w != nil && w.code == 0 {
					continue // one-shot that completed
				}
				return fmt.Errorf("container %s exited", cn)
			}
		}
	}
	return nil
}
