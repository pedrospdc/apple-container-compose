package compose

import (
	"bytes"
	"context"
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

var palette = []string{"36", "33", "32", "35", "34", "96", "93", "92", "95", "94"}

// LogPrinter multiplexes container output with Docker-style prefixes.
type LogPrinter struct {
	mu         sync.Mutex
	out        io.Writer
	width      int
	colors     map[string]string
	NoPrefix   bool
	NoColor    bool
	Timestamps bool
}

// NewLogPrinter returns a printer writing to out.
func NewLogPrinter(out io.Writer) *LogPrinter {
	return &LogPrinter{out: out, colors: map[string]string{}}
}

// Register reserves width for a container name so prefixes align.
func (l *LogPrinter) Register(name string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(name) > l.width {
		l.width = len(name)
	}
	if _, ok := l.colors[name]; !ok {
		l.colors[name] = palette[len(l.colors)%len(palette)]
	}
}

// Writer returns a line-buffered writer for one container.
func (l *LogPrinter) Writer(name string) io.Writer {
	l.Register(name)
	return &prefixWriter{lp: l, name: name}
}

// Event prints a status line for a container (e.g. exit notices).
func (l *LogPrinter) Event(name, msg string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	fmt.Fprintln(l.out, msg)
}

func (l *LogPrinter) line(name string, b []byte) {
	l.mu.Lock()
	defer l.mu.Unlock()
	var buf bytes.Buffer
	if !l.NoPrefix {
		prefix := fmt.Sprintf("%-*s | ", l.width, name)
		if !l.NoColor {
			prefix = "\x1b[" + l.colors[name] + "m" + prefix + "\x1b[0m"
		}
		buf.WriteString(prefix)
	}
	if l.Timestamps {
		buf.WriteString(time.Now().UTC().Format(time.RFC3339Nano) + " ")
	}
	buf.Write(bytes.TrimRight(b, "\r"))
	buf.WriteByte('\n')
	_, _ = l.out.Write(buf.Bytes())
}

type prefixWriter struct {
	lp   *LogPrinter
	name string
	mu   sync.Mutex
	buf  []byte
}

func (w *prefixWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			break
		}
		w.lp.line(w.name, w.buf[:i])
		w.buf = w.buf[i+1:]
	}
	return len(p), nil
}

// LogsOptions mirrors `docker compose logs`.
type LogsOptions struct {
	Services   []string
	Follow     bool
	Tail       string
	Timestamps bool
	NoColor    bool
	NoPrefix   bool
	Since      string
	Until      string
	Index      int
}

// Logs prints (and optionally follows) logs of project containers.
func (e *Engine) Logs(ctx context.Context, p *types.Project, o LogsOptions) error {
	if o.Since != "" || o.Until != "" {
		e.warnf("--since/--until are not supported by Apple container logs; showing all lines")
	}
	all, err := e.ctr.ListContainers(ctx, true)
	if err != nil {
		return err
	}
	cs := FilterServices(ProjectContainers(all, p.Name, false), o.Services)
	if o.Index > 0 {
		var sel = cs[:0]
		for _, c := range cs {
			if ContainerNumber(c) == o.Index {
				sel = append(sel, c)
			}
		}
		cs = sel
	}
	lp := NewLogPrinter(e.Out)
	lp.NoColor, lp.NoPrefix, lp.Timestamps = o.NoColor, o.NoPrefix, o.Timestamps && o.Follow
	for _, c := range cs {
		lp.Register(c.Name())
	}
	var wg sync.WaitGroup
	for _, c := range cs {
		args := []string{"logs"}
		if o.Follow && c.Running() {
			args = append(args, "--follow")
		}
		if o.Tail != "" && o.Tail != "all" {
			if _, err := strconv.Atoi(o.Tail); err == nil {
				args = append(args, "-n", o.Tail)
			}
		}
		args = append(args, c.Name())
		w := lp.Writer(c.Name())
		wg.Add(1)
		go func(args []string) {
			defer wg.Done()
			cmd := exec.CommandContext(ctx, e.ctr.Bin, args...)
			cmd.Stdout = w
			cmd.Stderr = w
			cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			_ = cmd.Run()
		}(args)
	}
	wg.Wait()
	return nil
}

func colorize(s, code string, enabled bool) string {
	if !enabled {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}

func padRight(s string, n int) string {
	if len(s) >= n {
		return s
	}
	return s + strings.Repeat(" ", n-len(s))
}
