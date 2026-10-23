package compose

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/compose-spec/compose-go/v2/types"

	"github.com/pedrospdc/apple-container-compose/internal/ctr"
)

// Apple container has no daemon that applies restart policies, so for
// detached projects compose spawns a small supervisor process. It polls the
// project's containers and restarts the ones that stopped on their own,
// following each container's restart policy (recorded in a label at create
// time). Containers stopped through compose are marked so the supervisor
// leaves them alone, mirroring Docker's "manually stopped" semantics.

// SuperviseCommand is the hidden CLI command that runs the supervisor.
const SuperviseCommand = "__supervise"

func restartLabel(s types.ServiceConfig) string {
	p := restartPolicyOf(s)
	if p.mode == "" || p.mode == "no" {
		return ""
	}
	if p.maxRetries > 0 {
		return p.mode + ":" + strconv.Itoa(p.maxRetries)
	}
	return p.mode
}

func parseRestartLabel(v string) restartPolicy {
	mode, n, _ := strings.Cut(v, ":")
	p := restartPolicy{mode: mode}
	p.maxRetries, _ = strconv.Atoi(n)
	return p
}

func (e *Engine) stoppedMarker(name string) string {
	return filepath.Join(e.dataRoot(), "stopped", sanitizeName(name))
}

// markStopped records a deliberate stop so the supervisor won't restart it.
func (e *Engine) markStopped(name string) {
	if e.ctr.DryRun {
		return
	}
	_ = os.MkdirAll(filepath.Dir(e.stoppedMarker(name)), 0o755)
	_ = os.WriteFile(e.stoppedMarker(name), nil, 0o644)
}

func (e *Engine) clearStopped(name string) {
	if !e.ctr.DryRun {
		_ = os.Remove(e.stoppedMarker(name))
	}
}

func (e *Engine) isMarkedStopped(name string) bool {
	_, err := os.Stat(e.stoppedMarker(name))
	return err == nil
}

func (e *Engine) supervisorPidFile(project string) string {
	return filepath.Join(e.dataRoot(), "supervisors", project+".pid")
}

// EnsureSupervisor starts the restart supervisor for a project if any of
// its containers has a restart policy and none is running yet.
func (e *Engine) EnsureSupervisor(ctx context.Context, project string) {
	if e.ctr.DryRun || os.Getenv("COMPOSE_NO_SUPERVISOR") != "" {
		return
	}
	all, err := e.ctr.ListContainers(ctx, true)
	if err != nil {
		return
	}
	needed := false
	for _, c := range ProjectContainers(all, project, false) {
		if c.Labels()[LabelRestart] != "" {
			needed = true
			break
		}
	}
	if !needed || supervisorAlive(e.supervisorPidFile(project)) {
		return
	}
	self, err := os.Executable()
	if err != nil {
		return
	}
	dir := filepath.Dir(e.supervisorPidFile(project))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	logf, err := os.OpenFile(filepath.Join(dir, project+".log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	defer logf.Close()
	cmd := exec.Command(self, SuperviseCommand, project)
	cmd.Stdout = logf
	cmd.Stderr = logf
	cmd.Env = os.Environ()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return
	}
	_ = os.WriteFile(e.supervisorPidFile(project), []byte(strconv.Itoa(cmd.Process.Pid)), 0o644)
	_ = cmd.Process.Release()
}

func supervisorAlive(pidFile string) bool {
	b, err := os.ReadFile(pidFile)
	if err != nil {
		return false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 0 {
		return false
	}
	return syscall.Kill(pid, 0) == nil
}

// projectFromLabels reloads the compose model a container was created from,
// falling back to a name-only project.
func projectFromLabels(ctx context.Context, project string, cs []ctr.Container) *types.Project {
	for _, c := range cs {
		l := c.Labels()
		if l[LabelConfigFiles] == "" {
			continue
		}
		o := ProjectOptions{ProjectName: project, ProjectDir: l[LabelWorkingDir], ConfigPaths: strings.Split(l[LabelConfigFiles], ",")}
		if p, err := o.Load(ctx); err == nil {
			return p
		}
		break
	}
	return &types.Project{Name: project, Services: types.Services{}}
}

// Supervise runs the restart loop until the project has no containers.
func (e *Engine) Supervise(ctx context.Context, project string) error {
	pidFile := e.supervisorPidFile(project)
	if b, err := os.ReadFile(pidFile); err == nil && strings.TrimSpace(string(b)) != strconv.Itoa(os.Getpid()) && supervisorAlive(pidFile) {
		return errors.New("supervisor already running")
	}
	_ = os.WriteFile(pidFile, []byte(strconv.Itoa(os.Getpid())), 0o644)
	defer os.Remove(pidFile)

	type state struct {
		restarts  int
		lastCode  int
		next      time.Time
		startedAt time.Time
		inflight  bool
	}
	var mu sync.Mutex
	states := map[string]*state{}
	var p *types.Project
	log := func(format string, a ...any) {
		fmt.Fprintf(e.Err, "%s %s\n", time.Now().Format(time.RFC3339), fmt.Sprintf(format, a...))
	}
	log("supervising project %s", project)
	for {
		all, err := e.ctr.ListContainers(ctx, true)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			time.Sleep(5 * time.Second)
			continue
		}
		cs := ProjectContainers(all, project, false)
		if len(cs) == 0 {
			log("project %s has no containers left; exiting", project)
			return nil
		}
		if p == nil {
			p = projectFromLabels(ctx, project, cs)
		}
		for _, c := range cs {
			name := c.Name()
			policy := parseRestartLabel(c.Labels()[LabelRestart])
			if policy.mode == "" || policy.mode == "no" {
				continue
			}
			mu.Lock()
			st := states[name]
			if st == nil {
				st = &state{lastCode: -1}
				states[name] = st
			}
			if c.Running() || st.inflight {
				// A container that stays up for a while resets its backoff.
				if c.Running() && !st.startedAt.IsZero() && time.Since(st.startedAt) > 10*time.Second {
					st.restarts = 0
				}
				mu.Unlock()
				continue
			}
			if e.isMarkedStopped(name) || time.Now().Before(st.next) {
				mu.Unlock()
				continue
			}
			code := st.lastCode
			if code < 0 {
				code = 1 // exit status unknown: assume failure
			}
			if !policy.shouldRestart(code, st.restarts) {
				mu.Unlock()
				continue
			}
			st.restarts++
			st.inflight = true
			st.startedAt = time.Now()
			st.next = time.Now().Add(policy.delay(st.restarts))
			mu.Unlock()
			log("restarting %s (policy %s, attempt %d)", name, c.Labels()[LabelRestart], st.restarts)
			go func(name string, st *state) {
				cmd := exec.Command(e.ctr.Bin, "start", "--attach", name)
				err := cmd.Start()
				if err == nil {
					e.waitAddress(ctx, name)
					if _, err := e.refreshHosts(ctx, p); err != nil {
						log("updating DNS records: %v", err)
					}
					err = cmd.Wait()
				}
				code := 0
				var ee *exec.ExitError
				if errors.As(err, &ee) {
					code = ee.ExitCode()
				} else if err != nil {
					code = -1
				}
				mu.Lock()
				st.lastCode = code
				st.inflight = false
				mu.Unlock()
				log("%s exited with code %d", name, code)
			}(name, st)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(2 * time.Second):
		}
	}
}
