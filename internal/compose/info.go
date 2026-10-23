package compose

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"text/template"
	"time"

	"github.com/compose-spec/compose-go/v2/types"

	"github.com/pedrospdc/apple-container-compose/internal/ctr"
)

// Publisher matches the `docker compose ps --format json` port entry.
type Publisher struct {
	URL           string
	TargetPort    int
	PublishedPort int
	Protocol      string
}

// ContainerSummary matches `docker compose ps --format json`.
type ContainerSummary struct {
	ID           string
	Name         string
	Names        []string
	Image        string
	Command      string
	Project      string
	Service      string
	Created      int64
	State        string
	Status       string
	Health       string
	ExitCode     int
	Publishers   []Publisher
	Labels       map[string]string
	Ports        string
	CreatedAt    string
	RunningFor   string
	Networks     string
	LocalVolumes string
	Mounts       string
	Size         string
}

func (e *Engine) summarize(c ctr.Container) ContainerSummary {
	l := c.Labels()
	created := parseTime(c.Configuration.CreationDate)
	s := ContainerSummary{
		ID:        c.Name(),
		Name:      c.Name(),
		Names:     []string{c.Name()},
		Image:     c.Configuration.Image.Reference,
		Command:   c.Command(),
		Project:   l[LabelProject],
		Service:   l[LabelService],
		State:     c.Status.State,
		Labels:    l,
		Health:    e.Health(c.Name()),
		CreatedAt: created.Format("2006-01-02 15:04:05 -0700 MST"),
	}
	if !created.IsZero() {
		s.Created = created.Unix()
		s.RunningFor = humanDuration(time.Since(created)) + " ago"
	}
	switch c.Status.State {
	case "running":
		s.Status = "Up"
		if t := parseTime(c.Status.StartedDate); !t.IsZero() {
			s.Status = "Up " + humanDuration(time.Since(t))
		}
		if s.Health != "" {
			s.Status += " (" + s.Health + ")"
		}
	case "stopped":
		s.State = "exited"
		s.Status = "Exited"
		if w := e.waiter(c.Name()); w != nil && w.code >= 0 {
			s.ExitCode = w.code
			s.Status = fmt.Sprintf("Exited (%d)", w.code)
		}
	default:
		s.Status = titleCase(c.Status.State)
	}
	var ports []string
	for _, p := range c.Configuration.PublishedPorts {
		count := max(p.Count, 1)
		for i := 0; i < count; i++ {
			host := p.HostAddress
			if host == "" {
				host = "0.0.0.0"
			}
			proto := strings.ToLower(p.Proto)
			if proto == "" {
				proto = "tcp"
			}
			s.Publishers = append(s.Publishers, Publisher{URL: host, TargetPort: p.ContainerPort + i, PublishedPort: p.HostPort + i, Protocol: proto})
			ports = append(ports, fmt.Sprintf("%s:%d->%d/%s", host, p.HostPort+i, p.ContainerPort+i, proto))
		}
	}
	s.Ports = strings.Join(ports, ", ")
	var nets []string
	for _, n := range c.Configuration.Networks {
		nets = append(nets, n.Network)
	}
	s.Networks = strings.Join(nets, ",")
	var mounts []string
	for _, m := range c.Configuration.Mounts {
		if v := m.VolumeName(); v != "" {
			mounts = append(mounts, v)
		} else {
			mounts = append(mounts, m.Source)
		}
	}
	s.Mounts = strings.Join(mounts, ",")
	s.LocalVolumes = strconv.Itoa(len(mounts))
	return s
}

// PsOptions mirrors `docker compose ps`.
type PsOptions struct {
	Services     []string
	All          bool
	Quiet        bool
	Format       string
	NoTrunc      bool
	Status       []string
	Filter       string
	Orphans      bool
	ListServices bool
}

// Ps lists project containers.
func (e *Engine) Ps(ctx context.Context, p *types.Project, o PsOptions) error {
	all, err := e.ctr.ListContainers(ctx, true)
	if err != nil {
		return err
	}
	cs := FilterServices(ProjectContainers(all, p.Name, true), o.Services)
	if !o.Orphans && len(p.Services) > 0 {
		var kept []ctr.Container
		for _, c := range cs {
			if _, ok := p.Services[c.Labels()[LabelService]]; ok {
				kept = append(kept, c)
			}
		}
		cs = kept
	}
	statuses := o.Status
	if o.Filter != "" {
		k, v, _ := strings.Cut(o.Filter, "=")
		if k == "status" {
			statuses = append(statuses, v)
		}
	}
	var rows []ContainerSummary
	for _, c := range cs {
		s := e.summarize(c)
		if !o.All && len(statuses) == 0 && s.State != "running" {
			continue
		}
		if len(statuses) > 0 && !contains(statuses, s.State) {
			continue
		}
		rows = append(rows, s)
	}
	if o.ListServices {
		seen := map[string]bool{}
		for _, r := range rows {
			if !seen[r.Service] {
				seen[r.Service] = true
				fmt.Fprintln(e.Out, r.Service)
			}
		}
		return nil
	}
	if o.Quiet {
		for _, r := range rows {
			fmt.Fprintln(e.Out, r.ID)
		}
		return nil
	}
	return renderRows(e.Out, o.Format, rows, []string{"NAME", "IMAGE", "COMMAND", "SERVICE", "CREATED", "STATUS", "PORTS"},
		func(r ContainerSummary) []string {
			cmd := r.Command
			if !o.NoTrunc && len(cmd) > 20 {
				cmd = cmd[:19] + "…"
			}
			return []string{r.Name, r.Image, strconv.Quote(cmd), r.Service, r.RunningFor, r.Status, r.Ports}
		})
}

// renderRows prints rows as a table, JSON (lines) or a Go template.
func renderRows[T any](out io.Writer, format string, rows []T, header []string, cols func(T) []string) error {
	switch {
	case format == "" || format == "table" || format == "pretty":
		tw := tabwriter.NewWriter(out, 0, 0, 3, ' ', 0)
		fmt.Fprintln(tw, strings.Join(header, "\t"))
		for _, r := range rows {
			fmt.Fprintln(tw, strings.Join(cols(r), "\t"))
		}
		return tw.Flush()
	case format == "json":
		if len(rows) == 0 {
			fmt.Fprintln(out, "[]")
			return nil
		}
		b, err := json.Marshal(rows)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(out, string(b))
		return err
	default:
		tmplText := format
		table := strings.HasPrefix(tmplText, "table ")
		tmplText = strings.TrimPrefix(tmplText, "table ")
		tmpl, err := template.New("format").Funcs(template.FuncMap{
			"json":  func(v any) string { b, _ := json.Marshal(v); return string(b) },
			"join":  strings.Join,
			"lower": strings.ToLower,
			"upper": strings.ToUpper,
		}).Parse(tmplText)
		if err != nil {
			return err
		}
		var w io.Writer = out
		var tw *tabwriter.Writer
		if table {
			tw = tabwriter.NewWriter(out, 0, 0, 3, ' ', 0)
			w = tw
		}
		for _, r := range rows {
			if err := tmpl.Execute(w, r); err != nil {
				return err
			}
			fmt.Fprintln(w)
		}
		if tw != nil {
			return tw.Flush()
		}
		return nil
	}
}

// ---- ls ----

// ProjectSummary matches `docker compose ls --format json`.
type ProjectSummary struct {
	Name        string
	Status      string
	ConfigFiles string
}

// Ls lists compose projects known from container labels.
func (e *Engine) Ls(ctx context.Context, all, quiet bool, format, filter string) error {
	cs, err := e.ctr.ListContainers(ctx, true)
	if err != nil {
		return err
	}
	type agg struct {
		states map[string]int
		files  string
	}
	projects := map[string]*agg{}
	for _, c := range cs {
		l := c.Labels()
		name := l[LabelProject]
		if name == "" || l[LabelRole] != "" {
			continue
		}
		a := projects[name]
		if a == nil {
			a = &agg{states: map[string]int{}}
			projects[name] = a
		}
		state := c.Status.State
		if state == "stopped" {
			state = "exited"
		}
		a.states[state]++
		if a.files == "" {
			a.files = l[LabelConfigFiles]
		}
	}
	var rows []ProjectSummary
	for _, name := range sortedKeys(projects) {
		a := projects[name]
		if !all && a.states["running"] == 0 {
			continue
		}
		if filter != "" {
			k, v, _ := strings.Cut(filter, "=")
			if k == "name" && !strings.Contains(name, v) {
				continue
			}
		}
		var parts []string
		for _, st := range sortedKeys(a.states) {
			parts = append(parts, fmt.Sprintf("%s(%d)", st, a.states[st]))
		}
		rows = append(rows, ProjectSummary{Name: name, Status: strings.Join(parts, ", "), ConfigFiles: a.files})
	}
	if quiet {
		for _, r := range rows {
			fmt.Fprintln(e.Out, r.Name)
		}
		return nil
	}
	return renderRows(e.Out, format, rows, []string{"NAME", "STATUS", "CONFIG FILES"},
		func(r ProjectSummary) []string { return []string{r.Name, r.Status, r.ConfigFiles} })
}

// ---- images ----

// ImageSummary matches `docker compose images --format json`.
type ImageSummary struct {
	ID            string
	ContainerName string
	Repository    string
	Tag           string
	Platform      string
	Size          int64
	LastTagTime   string
}

// Images lists images used by project containers.
func (e *Engine) Images(ctx context.Context, p *types.Project, services []string, quiet bool, format string) error {
	all, err := e.ctr.ListContainers(ctx, true)
	if err != nil {
		return err
	}
	cs := FilterServices(ProjectContainers(all, p.Name, true), services)
	var rows []ImageSummary
	for _, c := range cs {
		ref := c.Configuration.Image.Reference
		repo, tag := splitRef(ref)
		r := ImageSummary{ContainerName: c.Name(), Repository: repo, Tag: tag}
		if img, err := e.ctr.InspectImage(ctx, ref); err == nil && img != nil {
			r.ID = strings.TrimPrefix(img.Configuration.Descriptor.Digest, "sha256:")
			if v := img.Variant("arm64"); v != nil {
				r.Size = v.Size
				r.Platform = v.Platform.OS + "/" + v.Platform.Architecture
			}
		}
		rows = append(rows, r)
	}
	if quiet {
		seen := map[string]bool{}
		for _, r := range rows {
			if !seen[r.ID] {
				seen[r.ID] = true
				fmt.Fprintln(e.Out, r.ID)
			}
		}
		return nil
	}
	return renderRows(e.Out, format, rows, []string{"CONTAINER", "REPOSITORY", "TAG", "PLATFORM", "IMAGE ID", "SIZE"},
		func(r ImageSummary) []string {
			id := r.ID
			if len(id) > 12 {
				id = id[:12]
			}
			return []string{r.ContainerName, r.Repository, r.Tag, r.Platform, id, humanSize(r.Size)}
		})
}

func splitRef(ref string) (string, string) {
	if i := strings.Index(ref, "@"); i >= 0 {
		return ref[:i], ref[i+1:]
	}
	slash := strings.LastIndex(ref, "/")
	if i := strings.LastIndex(ref, ":"); i > slash {
		return ref[:i], ref[i+1:]
	}
	return ref, "latest"
}

// ---- top ----

// Top shows processes running in project containers.
func (e *Engine) Top(ctx context.Context, p *types.Project, services []string) error {
	all, err := e.ctr.ListContainers(ctx, false)
	if err != nil {
		return err
	}
	for _, c := range FilterServices(ProjectContainers(all, p.Name, true), services) {
		fmt.Fprintln(e.Out, c.Name())
		out, err := e.ctr.Output(ctx, "exec", c.Name(), "ps", "-ef")
		if err != nil {
			out, err = e.ctr.Output(ctx, "exec", c.Name(), "ps", "aux")
		}
		if err != nil {
			fmt.Fprintf(e.Out, "  (ps is not available in this container)\n\n")
			continue
		}
		fmt.Fprintln(e.Out, string(out))
	}
	return nil
}

// ---- port ----

// Port prints the public binding of a container port.
func (e *Engine) Port(ctx context.Context, p *types.Project, service string, port int, proto string, index int) error {
	c, err := e.FindContainer(ctx, p, service, index, false)
	if err != nil {
		return err
	}
	if proto == "" {
		proto = "tcp"
	}
	for _, pp := range c.Configuration.PublishedPorts {
		count := max(pp.Count, 1)
		pr := strings.ToLower(pp.Proto)
		if pr == "" {
			pr = "tcp"
		}
		if pr == proto && port >= pp.ContainerPort && port < pp.ContainerPort+count {
			host := pp.HostAddress
			if host == "" {
				host = "0.0.0.0"
			}
			fmt.Fprintf(e.Out, "%s:%d\n", host, pp.HostPort+(port-pp.ContainerPort))
			return nil
		}
	}
	return fmt.Errorf("no port %d/%s published for %s", port, proto, c.Name())
}

// ---- events ----

// Event matches `docker compose events --json`.
type Event struct {
	Time       string            `json:"time"`
	Type       string            `json:"type"`
	Action     string            `json:"action"`
	ID         string            `json:"id"`
	Service    string            `json:"service"`
	Attributes map[string]string `json:"attributes"`
}

// Events polls container state and reports transitions, since the Apple
// runtime has no event stream.
func (e *Engine) Events(ctx context.Context, p *types.Project, services []string, asJSON bool, since, until string) error {
	last := map[string]string{}
	first := true
	var deadline <-chan time.Time
	if until != "" {
		if d, err := time.ParseDuration(until); err == nil {
			deadline = time.After(d)
		}
	}
	for {
		all, err := e.ctr.ListContainers(ctx, true)
		if err != nil {
			return err
		}
		seen := map[string]bool{}
		for _, c := range FilterServices(ProjectContainers(all, p.Name, true), services) {
			name := c.Name()
			seen[name] = true
			prev, known := last[name]
			last[name] = c.Status.State
			if first {
				continue
			}
			var action string
			switch {
			case !known:
				action = "create"
				if c.Running() {
					e.emitEvent(c, "create", asJSON)
					action = "start"
				}
			case prev != c.Status.State && c.Running():
				action = "start"
			case prev != c.Status.State && prev == "running":
				action = "die"
			}
			if action != "" {
				e.emitEvent(c, action, asJSON)
			}
		}
		for name := range last {
			if !seen[name] {
				delete(last, name)
				if !first {
					e.emitEvent(ctr.Container{ID: name}, "destroy", asJSON)
				}
			}
		}
		first = false
		select {
		case <-ctx.Done():
			return nil
		case <-deadline:
			return nil
		case <-time.After(time.Second):
		}
	}
}

func (e *Engine) emitEvent(c ctr.Container, action string, asJSON bool) {
	l := c.Labels()
	ev := Event{
		Time:    time.Now().Format(time.RFC3339Nano),
		Type:    "container",
		Action:  action,
		ID:      c.Name(),
		Service: l[LabelService],
		Attributes: map[string]string{
			"name":  c.Name(),
			"image": c.Configuration.Image.Reference,
		},
	}
	if asJSON {
		b, _ := json.Marshal(ev)
		fmt.Fprintln(e.Out, string(b))
		return
	}
	var attrs []string
	for _, k := range sortedKeys(ev.Attributes) {
		if ev.Attributes[k] != "" {
			attrs = append(attrs, k+"="+ev.Attributes[k])
		}
	}
	fmt.Fprintf(e.Out, "%s container %s %s (%s)\n", ev.Time, action, ev.ID, strings.Join(attrs, ", "))
}

// ---- stats ----

// Stats streams resource usage of project containers.
func (e *Engine) Stats(ctx context.Context, p *types.Project, services []string, noStream bool, format string, all bool) error {
	cs, err := e.ctr.ListContainers(ctx, all)
	if err != nil {
		return err
	}
	args := []string{"stats"}
	if noStream {
		args = append(args, "--no-stream")
	}
	if format != "" {
		args = append(args, "--format", format)
	}
	n := 0
	for _, c := range FilterServices(ProjectContainers(cs, p.Name, true), services) {
		if c.Running() {
			args = append(args, c.Name())
			n++
		}
	}
	if n == 0 {
		return nil
	}
	return e.ctr.Stream(ctx, os.Stdin, e.Out, e.Err, args...)
}

// ---- volumes ----

// VolumeSummary matches `docker compose volumes --format json`.
type VolumeSummary struct {
	Name       string
	Driver     string
	Mountpoint string
	Labels     string
	Size       string
}

// Volumes lists project volumes.
func (e *Engine) Volumes(ctx context.Context, p *types.Project, services []string, quiet bool, format string) error {
	vols, err := e.ctr.ListVolumes(ctx)
	if err != nil {
		return err
	}
	wanted := map[string]bool{}
	if len(services) > 0 {
		for _, sn := range services {
			s, ok := p.Services[sn]
			if !ok {
				return fmt.Errorf("no such service: %s", sn)
			}
			for _, v := range s.Volumes {
				if v.Type == types.VolumeTypeVolume && v.Source != "" {
					wanted[VolumeName(p, v.Source)] = true
				}
			}
		}
	}
	var rows []VolumeSummary
	for _, v := range vols {
		if v.Configuration.Labels[LabelProject] != p.Name {
			continue
		}
		if len(services) > 0 && !wanted[v.Name()] {
			continue
		}
		var labels []string
		for _, k := range sortedKeys(v.Configuration.Labels) {
			labels = append(labels, k+"="+v.Configuration.Labels[k])
		}
		rows = append(rows, VolumeSummary{Name: v.Name(), Driver: v.Configuration.Driver, Mountpoint: v.Configuration.Source,
			Labels: strings.Join(labels, ","), Size: humanSize(v.Configuration.SizeInBytes)})
	}
	if quiet {
		for _, r := range rows {
			fmt.Fprintln(e.Out, r.Name)
		}
		return nil
	}
	return renderRows(e.Out, format, rows, []string{"DRIVER", "VOLUME NAME", "SIZE"},
		func(r VolumeSummary) []string { return []string{r.Driver, r.Name, r.Size} })
}

// ---- wait ----

// Wait blocks until service containers stop and returns the first exit code.
func (e *Engine) Wait(ctx context.Context, p *types.Project, services []string, downProject bool) (int, error) {
	all, err := e.ctr.ListContainers(ctx, true)
	if err != nil {
		return 1, err
	}
	cs := FilterServices(ProjectContainers(all, p.Name, false), services)
	if len(cs) == 0 {
		return 1, errors.New("no containers for the selected services")
	}
	code := 0
	for {
		running := false
		for _, c := range cs {
			cur, err := e.ctr.InspectContainer(ctx, c.Name())
			if err != nil {
				return 1, err
			}
			if cur != nil && cur.Running() {
				running = true
			}
		}
		if !running {
			break
		}
		select {
		case <-ctx.Done():
			return 1, ctx.Err()
		case <-time.After(time.Second):
		}
	}
	if downProject {
		if err := e.Down(ctx, p, DownOptions{}); err != nil {
			return 1, err
		}
	}
	return code, nil
}

// ---- pause ----

// Pause is not available: Apple container has no freezer support.
func (e *Engine) Pause(context.Context, *types.Project, []string) error {
	return errors.New("pause is not supported by Apple container (no cgroup freezer access); use stop/start instead")
}

// ---- cp / commit / export ----

// Copy copies files between a service container and the host. Paths in
// SERVICE:PATH form are resolved to the container.
func (e *Engine) Copy(ctx context.Context, p *types.Project, src, dst string, index int, all bool) error {
	resolve := func(arg string) ([]string, error) {
		svc, path, ok := strings.Cut(arg, ":")
		if !ok || strings.HasPrefix(arg, "/") || strings.HasPrefix(arg, ".") {
			return []string{arg}, nil
		}
		if _, err := p.GetService(svc); err != nil {
			return []string{arg}, nil
		}
		if all {
			cs, err := e.ctr.ListContainers(ctx, true)
			if err != nil {
				return nil, err
			}
			var out []string
			for _, c := range ServiceContainers(ProjectContainers(cs, p.Name, false), svc) {
				out = append(out, c.Name()+":"+path)
			}
			return out, nil
		}
		c, err := e.FindContainer(ctx, p, svc, index, false)
		if err != nil {
			return nil, err
		}
		return []string{c.Name() + ":" + path}, nil
	}
	srcs, err := resolve(src)
	if err != nil {
		return err
	}
	dsts, err := resolve(dst)
	if err != nil {
		return err
	}
	for _, s := range srcs {
		for _, d := range dsts {
			if err := e.ctr.Stream(ctx, nil, e.Out, e.Err, "copy", s, d); err != nil {
				return err
			}
		}
	}
	return nil
}

// Commit creates an image from a service container.
func (e *Engine) Commit(ctx context.Context, p *types.Project, service string, index int, ref string) error {
	c, err := e.FindContainer(ctx, p, service, index, false)
	if err != nil {
		return err
	}
	return e.ctr.Stream(ctx, nil, e.Out, e.Err, "commit", c.Name(), ref)
}

// Export writes a service container's filesystem as a tar archive.
func (e *Engine) Export(ctx context.Context, p *types.Project, service string, index int, output string) error {
	c, err := e.FindContainer(ctx, p, service, index, false)
	if err != nil {
		return err
	}
	args := []string{"export"}
	if output != "" {
		args = append(args, "--output", output)
	}
	return e.ctr.Stream(ctx, nil, e.Out, e.Err, append(args, c.Name())...)
}

// ---- pull / push ----

// Pull pulls service images.
func (e *Engine) Pull(ctx context.Context, p *types.Project, services []string, ignoreFailures, includeDeps, ignoreBuildable bool, policy string, quiet bool) error {
	sp, err := SelectServices(p, services, !includeDeps)
	if err != nil {
		return err
	}
	var errs []error
	seen := map[string]bool{}
	for _, name := range sortedServiceNames(sp) {
		s := sp.Services[name]
		if s.Image == "" {
			continue
		}
		if s.Build != nil && ignoreBuildable {
			continue
		}
		if seen[s.Image] {
			continue
		}
		seen[s.Image] = true
		if policy == "missing" {
			if img, _ := e.ctr.InspectImage(ctx, s.Image); img != nil {
				continue
			}
		}
		if err := e.pull(ctx, s.Image, s.Platform, quiet); err != nil {
			if ignoreFailures || s.Build != nil {
				e.warnf("%v", err)
				continue
			}
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Push pushes images of services with both image and build.
func (e *Engine) Push(ctx context.Context, p *types.Project, services []string, ignoreFailures, includeDeps bool) error {
	sp, err := SelectServices(p, services, !includeDeps)
	if err != nil {
		return err
	}
	var errs []error
	for _, name := range sortedServiceNames(sp) {
		s := sp.Services[name]
		if s.Image == "" || s.Build == nil {
			continue
		}
		e.status("Image", s.Image, "Pushing")
		if err := e.ctr.Stream(ctx, nil, e.Err, e.Err, "image", "push", s.Image); err != nil {
			if ignoreFailures {
				e.warnf("%v", err)
				continue
			}
			errs = append(errs, err)
			continue
		}
		e.status("Image", s.Image, "Pushed")
	}
	return errors.Join(errs...)
}

// ---- helpers ----

func parseTime(s string) time.Time {
	t, _ := time.Parse(time.RFC3339, s)
	return t
}

func humanDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%d seconds", int(d.Seconds()))
	case d < time.Hour:
		n := int(d.Minutes())
		if n == 1 {
			return "About a minute"
		}
		return fmt.Sprintf("%d minutes", n)
	case d < 48*time.Hour:
		n := int(d.Hours())
		if n == 1 {
			return "About an hour"
		}
		return fmt.Sprintf("%d hours", n)
	default:
		return fmt.Sprintf("%d days", int(d.Hours()/24))
	}
}

func humanSize(b int64) string {
	units := []string{"B", "kB", "MB", "GB", "TB"}
	f := float64(b)
	i := 0
	for f >= 1000 && i < len(units)-1 {
		f /= 1000
		i++
	}
	if i == 0 {
		return fmt.Sprintf("%dB", b)
	}
	return fmt.Sprintf("%.3g%s", f, units[i])
}

func titleCase(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
