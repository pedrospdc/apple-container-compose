package compose

import (
	"context"
	"fmt"
	"math"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/compose-spec/compose-go/v2/types"
)

// createOptions tunes how a service is turned into a container.
type createOptions struct {
	Name         string
	Number       int
	Oneoff       bool
	NoPorts      bool     // `run` without --service-ports
	ExtraPorts   []string // `run -p`
	ExtraVolumes []string // `run -v`
	ExtraLabels  []string
	ExtraEnv     []string
	Entrypoint   *types.ShellCommand
	Command      types.ShellCommand
	User         string
	WorkDir      string
	Interactive  bool
	TTY          bool
	Remove       bool
	CapAdd       []string
	CapDrop      []string
}

// containerSpec is the result of converting a service.
type containerSpec struct {
	Name     string
	Flags    []string
	Image    string
	Args     []string
	Warnings []string
}

// CommandLine returns the full argument list for `container <verb>`.
func (s *containerSpec) CommandLine(verb string, extra ...string) []string {
	args := append([]string{verb}, extra...)
	args = append(args, s.Flags...)
	args = append(args, s.Image)
	return append(args, s.Args...)
}

func (e *Engine) toContainerSpec(ctx context.Context, p *types.Project, s types.ServiceConfig, o createOptions) (*containerSpec, error) {
	spec := &containerSpec{Name: o.Name, Image: ImageName(p.Name, s)}
	warn := func(format string, a ...any) { spec.Warnings = append(spec.Warnings, fmt.Sprintf(format, a...)) }
	f := []string{"--name", o.Name}

	// ---- labels ----
	hash, err := ServiceHash(s)
	if err != nil {
		return nil, err
	}
	labels := map[string]string{}
	for k, v := range s.Labels {
		labels[k] = v
	}
	for k, v := range s.CustomLabels {
		labels[k] = v
	}
	labels[LabelProject] = p.Name
	labels[LabelService] = s.Name
	labels[LabelNumber] = strconv.Itoa(o.Number)
	labels[LabelConfigHash] = hash
	labels[LabelImage] = spec.Image
	labels[LabelVersion] = Version
	if o.Oneoff {
		labels[LabelOneoff] = "True"
	} else {
		labels[LabelOneoff] = "False"
	}
	if r := restartLabel(s); r != "" && !o.Oneoff {
		labels[LabelRestart] = r
	}
	if deps := dependsOnLabel(s); deps != "" {
		labels[LabelDependsOn] = deps
	}
	for _, l := range o.ExtraLabels {
		k, v, _ := strings.Cut(l, "=")
		labels[k] = v
	}
	for _, k := range sortedKeys(labels) {
		f = append(f, "--label", k+"="+labels[k])
	}

	// ---- process ----
	env := map[string]string{}
	for k, v := range s.Environment {
		if v != nil {
			env[k] = *v
		}
	}
	for _, kv := range o.ExtraEnv {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			if hv, found := os.LookupEnv(k); found {
				v = hv
			} else {
				continue
			}
		}
		env[k] = v
	}
	for _, k := range sortedKeys(env) {
		f = append(f, "--env", k+"="+env[k])
	}
	user := s.User
	if o.User != "" {
		user = o.User
	}
	if user != "" {
		f = append(f, "--user", user)
	}
	wd := s.WorkingDir
	if o.WorkDir != "" {
		wd = o.WorkDir
	}
	if wd != "" {
		f = append(f, "--workdir", wd)
	}
	if s.StdinOpen || o.Interactive {
		f = append(f, "--interactive")
	}
	if s.Tty || o.TTY {
		f = append(f, "--tty")
	}
	for _, name := range sortedKeys(s.Ulimits) {
		u := s.Ulimits[name]
		if u.Single != 0 {
			f = append(f, "--ulimit", fmt.Sprintf("%s=%d", name, u.Single))
		} else {
			f = append(f, "--ulimit", fmt.Sprintf("%s=%d:%d", name, u.Soft, u.Hard))
		}
	}

	// ---- resources ----
	cpus := float64(s.CPUS)
	mem := int64(s.MemLimit)
	if s.Deploy != nil && s.Deploy.Resources.Limits != nil {
		if l := s.Deploy.Resources.Limits; l.NanoCPUs > 0 && cpus == 0 {
			cpus = float64(l.NanoCPUs)
		}
		if l := s.Deploy.Resources.Limits; l.MemoryBytes > 0 && mem == 0 {
			mem = int64(l.MemoryBytes)
		}
	}
	if cpus == 0 && s.CPUCount > 0 {
		cpus = float64(s.CPUCount)
	}
	if cpus > 0 {
		f = append(f, "--cpus", strconv.Itoa(int(math.Max(1, math.Ceil(cpus)))))
	}
	if mem > 0 {
		f = append(f, "--memory", mebibytes(mem))
	}
	if s.ShmSize > 0 {
		f = append(f, "--shm-size", strconv.FormatInt(max(int64(s.ShmSize)/(1024*1024), 1), 10)+"M")
	}

	// ---- security ----
	if s.Privileged {
		f = append(f, "--cap-add", "ALL")
	}
	for _, c := range append(append([]string{}, s.CapAdd...), o.CapAdd...) {
		f = append(f, "--cap-add", capName(c))
	}
	drops := append(append([]string{}, s.CapDrop...), o.CapDrop...)
	for _, c := range drops {
		f = append(f, "--cap-drop", capName(c))
	}
	// Every container is its own VM with its own kernel, so sysctls are
	// set on the kernel command line. Docker's defaults come first so the
	// service can override them.
	sysctls := map[string]string{
		"net.ipv4.ip_unprivileged_port_start": "0",
		"net.ipv4.ping_group_range":           "0 2147483647",
	}
	for k, v := range s.Sysctls {
		sysctls[k] = v
	}
	for _, k := range sortedKeys(sysctls) {
		v := sysctls[k]
		if strings.ContainsAny(v, " \t") {
			v = `"` + v + `"` // kernel command-line quoting
		}
		f = append(f, "--kernel-arg", "sysctl."+k+"="+v)
	}
	if s.ReadOnly {
		f = append(f, "--read-only")
	}
	if s.Init != nil && *s.Init {
		f = append(f, "--init")
	}

	// ---- networking ----
	nets, err := e.serviceNetworks(p, s)
	if err != nil {
		return nil, err
	}
	for i, n := range nets {
		arg := n.runtime
		if n.mac != "" {
			arg += ",mac=" + n.mac
		} else if i == 0 && s.MacAddress != "" {
			arg += ",mac=" + s.MacAddress
		}
		f = append(f, "--network", arg)
		if n.cfg != nil && (n.cfg.Ipv4Address != "" || n.cfg.Ipv6Address != "") {
			warn("static IP addresses (ipv4_address/ipv6_address) are not supported; using DHCP")
		}
	}
	if len(nets) > 0 && nets[0].runtime != "none" {
		if ip := e.dnsServerFor(ctx, p.Name, nets[0].runtime); ip != "" {
			f = append(f, "--dns", ip)
		}
	}
	for _, d := range s.DNS {
		f = append(f, "--dns", d)
	}
	for _, d := range s.DNSSearch {
		f = append(f, "--dns-search", d)
	}
	for _, d := range s.DNSOpts {
		f = append(f, "--dns-option", d)
	}
	if s.DomainName != "" {
		f = append(f, "--dns-domain", s.DomainName)
	}
	if !o.NoPorts {
		for _, port := range s.Ports {
			spec, err := portSpec(port)
			if err != nil {
				return nil, err
			}
			f = append(f, "--publish", spec)
		}
	}
	for _, port := range o.ExtraPorts {
		f = append(f, "--publish", port)
	}

	// ---- storage ----
	mounts, err := e.serviceMounts(ctx, p, s, o.Name, warn)
	if err != nil {
		return nil, err
	}
	f = append(f, mounts...)
	for _, v := range o.ExtraVolumes {
		mf, err := e.shortVolumeFlag(ctx, p, s, o.Name, v, warn)
		if err != nil {
			return nil, err
		}
		f = append(f, mf...)
	}
	for _, t := range s.Tmpfs {
		path, _, _ := strings.Cut(t, ":")
		f = append(f, "--tmpfs", path)
	}
	secretFlags, err := e.secretMounts(p, s, warn)
	if err != nil {
		return nil, err
	}
	f = append(f, secretFlags...)

	// ---- platform ----
	platform, rosetta := e.platformFor(ctx, s, spec.Image)
	if platform != "" {
		f = append(f, "--platform", platform)
		if rosetta {
			f = append(f, "--rosetta")
		}
	}
	if o.Remove {
		f = append(f, "--remove")
	}

	// ---- entrypoint / command ----
	entrypoint := s.Entrypoint
	if o.Entrypoint != nil {
		entrypoint = *o.Entrypoint
	}
	command := s.Command
	if o.Command != nil {
		command = o.Command
	}
	var args []string
	switch {
	case entrypoint != nil && isEmptyCommand(entrypoint):
		// `entrypoint: ""` clears the image entrypoint.
		if len(command) == 0 {
			command = e.imageCmd(ctx, spec.Image)
		}
		if len(command) > 0 {
			f = append(f, "--entrypoint", command[0])
			args = append(args, command[1:]...)
		}
	case entrypoint != nil:
		f = append(f, "--entrypoint", entrypoint[0])
		args = append(args, entrypoint[1:]...)
		args = append(args, command...)
	default:
		args = append(args, command...)
	}

	unsupported(s, warn)
	spec.Flags = f
	spec.Args = args
	return spec, nil
}

func isEmptyCommand(c types.ShellCommand) bool {
	return len(c) == 0 || (len(c) == 1 && c[0] == "")
}

func (e *Engine) imageCmd(ctx context.Context, ref string) []string {
	img, err := e.ctr.InspectImage(ctx, ref)
	if err != nil || img == nil {
		return nil
	}
	if v := img.Variant("arm64"); v != nil {
		return v.Config.Config.Cmd
	}
	return nil
}

func (e *Engine) imageVolumes(ctx context.Context, ref string) []string {
	img, err := e.ctr.InspectImage(ctx, ref)
	if err != nil || img == nil {
		return nil
	}
	v := img.Variant("arm64")
	if v == nil {
		return nil
	}
	return sortedKeys(v.Config.Config.Volumes)
}

// platformFor picks a platform override. Apple container defaults to
// linux/arm64; images that only ship amd64 run under Rosetta.
func (e *Engine) platformFor(ctx context.Context, s types.ServiceConfig, ref string) (string, bool) {
	if s.Platform != "" {
		return s.Platform, strings.Contains(s.Platform, "amd64")
	}
	img, err := e.ctr.InspectImage(ctx, ref)
	if err != nil || img == nil {
		return "", false
	}
	if !img.HasArch("arm64") && img.HasArch("amd64") {
		return "linux/amd64", true
	}
	return "", false
}

type serviceNetwork struct {
	key     string
	runtime string
	mac     string
	cfg     *types.ServiceNetworkConfig
}

// serviceNetworks resolves the networks a service attaches to, ordered by
// priority like Docker does.
func (e *Engine) serviceNetworks(p *types.Project, s types.ServiceConfig) ([]serviceNetwork, error) {
	mode := s.NetworkMode
	switch {
	case mode == "none":
		return []serviceNetwork{{runtime: "none"}}, nil
	case mode == "host" || mode == "bridge":
		return []serviceNetwork{{runtime: "default"}}, nil
	case strings.HasPrefix(mode, "service:"):
		other, err := p.GetService(strings.TrimPrefix(mode, "service:"))
		if err != nil {
			return nil, err
		}
		return e.serviceNetworks(p, other)
	case strings.HasPrefix(mode, "container:"):
		return []serviceNetwork{{runtime: "default"}}, nil
	}
	var out []serviceNetwork
	for key, cfg := range s.Networks {
		sn := serviceNetwork{key: key, runtime: NetworkName(p, key), cfg: cfg}
		if cfg != nil {
			sn.mac = cfg.MacAddress
		}
		out = append(out, sn)
	}
	sort.SliceStable(out, func(i, j int) bool {
		pi, pj := 0, 0
		if out[i].cfg != nil {
			pi = out[i].cfg.Priority + out[i].cfg.GatewayPriority
		}
		if out[j].cfg != nil {
			pj = out[j].cfg.Priority + out[j].cfg.GatewayPriority
		}
		if pi != pj {
			return pi > pj
		}
		return out[i].key < out[j].key
	})
	return out, nil
}

// serviceMounts converts volumes, volumes_from and their options.
func (e *Engine) serviceMounts(ctx context.Context, p *types.Project, s types.ServiceConfig, containerName string, warn func(string, ...any)) ([]string, error) {
	var f []string
	seen := map[string]bool{}
	add := func(v types.ServiceVolumeConfig) error {
		if seen[v.Target] {
			return nil
		}
		seen[v.Target] = true
		mf, err := e.mountFlag(ctx, p, s, containerName, v, warn)
		if err != nil {
			return err
		}
		f = append(f, mf...)
		return nil
	}
	for _, v := range s.Volumes {
		if err := add(v); err != nil {
			return nil, err
		}
	}
	if s.ReadOnly {
		// With a read-only rootfs, Docker keeps the image's VOLUME paths
		// writable through anonymous volumes.
		for _, target := range e.imageVolumes(ctx, ImageName(p.Name, s)) {
			if err := add(types.ServiceVolumeConfig{Type: types.VolumeTypeVolume, Target: target}); err != nil {
				return nil, err
			}
		}
	}
	for _, from := range s.VolumesFrom {
		name, mode, _ := strings.Cut(from, ":")
		if strings.HasPrefix(name, "container:") {
			warn("volumes_from %s: only services are supported", from)
			continue
		}
		other, err := p.GetService(name)
		if err != nil {
			return nil, fmt.Errorf("volumes_from: %w", err)
		}
		for _, v := range other.Volumes {
			if v.Type == types.VolumeTypeVolume && v.Source == "" {
				continue // anonymous volumes are per-container
			}
			if mode == "ro" {
				v.ReadOnly = true
			}
			if err := add(v); err != nil {
				return nil, err
			}
		}
	}
	return f, nil
}

func (e *Engine) shortVolumeFlag(ctx context.Context, p *types.Project, s types.ServiceConfig, containerName, spec string, warn func(string, ...any)) ([]string, error) {
	parts := strings.Split(spec, ":")
	v := types.ServiceVolumeConfig{Type: types.VolumeTypeVolume}
	switch len(parts) {
	case 1:
		v.Target = parts[0]
	default:
		v.Source, v.Target = parts[0], parts[1]
		if len(parts) > 2 && strings.Contains(parts[2], "ro") {
			v.ReadOnly = true
		}
	}
	if strings.HasPrefix(v.Source, ".") || strings.HasPrefix(v.Source, "/") || strings.HasPrefix(v.Source, "~") {
		v.Type = types.VolumeTypeBind
		v.Source = expandPath(p.WorkingDir, v.Source)
	}
	return e.mountFlag(ctx, p, s, containerName, v, warn)
}

func (e *Engine) mountFlag(ctx context.Context, p *types.Project, s types.ServiceConfig, containerName string, v types.ServiceVolumeConfig, warn func(string, ...any)) ([]string, error) {
	suffix := ""
	if v.ReadOnly {
		suffix = ":ro"
	}
	switch v.Type {
	case types.VolumeTypeBind:
		return e.bindFlag(ctx, p, s, v.Source, v.Target, suffix, v.Bind == nil || bool(v.Bind.CreateHostPath), warn)
	case types.VolumeTypeVolume:
		if v.Source == "" {
			name := sanitizeName(containerName + "_" + shortID(v.Target))
			if err := e.ensureAnonymousVolume(ctx, p, s, name, v.Target); err != nil {
				return nil, err
			}
			return []string{"--volume", name + ":" + v.Target + suffix}, nil
		}
		cfg, ok := p.Volumes[v.Source]
		if !ok {
			return nil, fmt.Errorf("service %q refers to undefined volume %s", s.Name, v.Source)
		}
		if dev := bindDevice(cfg); dev != "" {
			return e.bindFlag(ctx, p, s, dev, v.Target, suffix, true, warn)
		}
		if v.Volume != nil && v.Volume.Subpath != "" {
			warn("volume subpath is not supported; mounting the whole volume %s", v.Source)
		}
		if !bool(cfg.External) && mountUsage(p).volumes[v.Source] > 1 {
			// Apple volumes are disk images that only one VM can attach, so
			// volumes shared between containers live in a host directory.
			dir := e.sharedVolumeDir(p, v.Source)
			if !e.ctr.DryRun {
				if err := os.MkdirAll(dir, 0o755); err != nil {
					return nil, err
				}
			}
			return []string{"--volume", dir + ":" + v.Target + suffix}, nil
		}
		return []string{"--volume", VolumeName(p, v.Source) + ":" + v.Target + suffix}, nil
	case types.VolumeTypeTmpfs:
		return []string{"--tmpfs", v.Target}, nil
	default:
		warn("volume type %q is not supported; skipped %s", v.Type, v.Target)
		return nil, nil
	}
}

// bindDevice detects the common "local driver bind" volume pattern:
//
//	driver_opts: {type: none, o: bind, device: /path}
func bindDevice(v types.VolumeConfig) string {
	if v.DriverOpts == nil || bool(v.External) {
		return ""
	}
	if strings.Contains(v.DriverOpts["o"], "bind") && v.DriverOpts["device"] != "" {
		return v.DriverOpts["device"]
	}
	return ""
}

// systemPath reports Linux host paths that only make sense on a Linux
// host (kernel interfaces, device nodes, runtime sockets).
func systemPath(p string) bool {
	for _, prefix := range []string{"/proc", "/sys", "/dev", "/run", "/var/run", "/lib/modules", "/usr/src", "/etc", "/boot"} {
		if p == prefix || strings.HasPrefix(p, prefix+"/") {
			return true
		}
	}
	return false
}

type hostPathMode int

const (
	bindMount hostPathMode = iota
	skipMount
	missingMount
	volumeMount
)

// hostPath resolves a bind source the way Docker does on Linux, where
// missing paths are created as directories.
func (e *Engine) hostPath(src string, create bool, warn func(string, ...any)) hostPathMode {
	info, err := os.Stat(src)
	switch {
	case err == nil && info.Mode()&os.ModeSocket != 0:
		warn("bind mount of socket %s is not supported by Apple container; skipped", src)
		return skipMount
	case err == nil:
		return bindMount
	case !os.IsNotExist(err):
		warn("cannot access bind source %s (%v); mount skipped", src, err)
		return skipMount
	case systemPath(src):
		warn("host path %s does not exist on macOS; mount skipped", src)
		return skipMount
	case !create:
		return missingMount
	}
	if e.ctr.DryRun {
		if writableAncestor(src) {
			return bindMount
		}
		return volumeMount
	}
	if os.MkdirAll(src, 0o755) == nil {
		return bindMount
	}
	return volumeMount
}

// bindFlag converts a bind mount. Sources that can't be created on macOS
// (e.g. /mnt/docker-volumes/app, under the root-owned /mnt) are backed by a
// named volume instead: it persists like the host directory would, and
// unlike virtiofs bind mounts it lets entrypoints chown their data dirs.
func (e *Engine) bindFlag(ctx context.Context, p *types.Project, s types.ServiceConfig, src, target, suffix string, create bool, warn func(string, ...any)) ([]string, error) {
	switch e.hostPath(src, create, warn) {
	case skipMount:
		return nil, nil
	case missingMount:
		return nil, fmt.Errorf("bind source path does not exist: %s", src)
	case volumeMount:
		if mountUsage(p).paths[src] > 1 {
			// Block volumes can't be attached to two VMs; share a directory.
			dir := filepath.Join(e.dataRoot(), "host-paths", src)
			if !e.ctr.DryRun {
				if err := os.MkdirAll(dir, 0o755); err != nil {
					return nil, err
				}
			}
			e.warnf("host path %s cannot be created on macOS; using %s instead", src, dir)
			return []string{"--volume", dir + ":" + target + suffix}, nil
		}
		name := HostPathVolumeName(p.Name, src)
		e.warnf("host path %s cannot be created on macOS; using volume %s instead", src, name)
		if err := e.ensureHostPathVolume(ctx, p, s, name, src, target); err != nil {
			return nil, err
		}
		return []string{"--volume", name + ":" + target + suffix}, nil
	}
	return []string{"--volume", src + ":" + target + suffix}, nil
}

// HostPathVolumeName returns the volume backing an uncreatable host path.
func HostPathVolumeName(project, src string) string {
	return sanitizeName(project + "_host_" + filepath.Base(src) + "_" + shortID(src)[:8])
}

// writableAncestor reports whether the closest existing ancestor of p is
// writable, i.e. whether MkdirAll would succeed.
func writableAncestor(p string) bool {
	for dir := filepath.Dir(p); ; dir = filepath.Dir(dir) {
		if _, err := os.Stat(dir); err == nil {
			f, err := os.CreateTemp(dir, ".acc-probe")
			if err != nil {
				return false
			}
			f.Close()
			os.Remove(f.Name())
			return true
		}
		if dir == "/" {
			return false
		}
	}
}

func expandPath(base, p string) string {
	if strings.HasPrefix(p, "~") {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, p[1:])
	}
	if !filepath.IsAbs(p) {
		return filepath.Join(base, p)
	}
	return p
}

// secretMounts materialises secrets and configs as read-only file mounts.
func (e *Engine) secretMounts(p *types.Project, s types.ServiceConfig, warn func(string, ...any)) ([]string, error) {
	var f []string
	type ref struct {
		source, target string
		obj            types.FileObjectConfig
		kind           string
	}
	var refs []ref
	for _, sec := range s.Secrets {
		obj, ok := p.Secrets[sec.Source]
		if !ok {
			return nil, fmt.Errorf("service %q refers to undefined secret %s", s.Name, sec.Source)
		}
		target := sec.Target
		if target == "" {
			target = "/run/secrets/" + sec.Source
		} else if !strings.HasPrefix(target, "/") {
			target = "/run/secrets/" + target
		}
		refs = append(refs, ref{sec.Source, target, types.FileObjectConfig(obj), "secrets"})
	}
	for _, cfg := range s.Configs {
		obj, ok := p.Configs[cfg.Source]
		if !ok {
			return nil, fmt.Errorf("service %q refers to undefined config %s", s.Name, cfg.Source)
		}
		target := cfg.Target
		if target == "" {
			target = "/" + cfg.Source
		}
		refs = append(refs, ref{cfg.Source, target, types.FileObjectConfig(obj), "configs"})
	}
	for _, r := range refs {
		var src string
		switch {
		case bool(r.obj.External):
			warn("external %s %q are not supported; skipped", r.kind, r.source)
			continue
		case r.obj.File != "":
			src = r.obj.File
		default:
			content := r.obj.Content
			if r.obj.Environment != "" {
				content = p.Environment[r.obj.Environment]
				if v, ok := os.LookupEnv(r.obj.Environment); ok && content == "" {
					content = v
				}
			}
			dir := filepath.Join(e.stateDir(p.Name), r.kind)
			src = filepath.Join(dir, sanitizeName(r.source))
			if !e.ctr.DryRun {
				if err := os.MkdirAll(dir, 0o700); err != nil {
					return nil, err
				}
				if err := os.WriteFile(src, []byte(content), 0o600); err != nil {
					return nil, err
				}
			}
		}
		f = append(f, "--volume", src+":"+r.target+":ro")
	}
	return f, nil
}

// portSpec converts a port config into Apple's -p syntax.
func portSpec(p types.ServicePortConfig) (string, error) {
	published := p.Published
	if strings.Contains(published, "-") {
		lo, hi, _ := strings.Cut(published, "-")
		a, err1 := strconv.Atoi(lo)
		b, err2 := strconv.Atoi(hi)
		if err1 != nil || err2 != nil {
			return "", fmt.Errorf("invalid published port range %q", published)
		}
		published = ""
		for port := a; port <= b; port++ {
			if portFree(p.HostIP, port, p.Protocol) {
				published = strconv.Itoa(port)
				break
			}
		}
		if published == "" {
			return "", fmt.Errorf("no free host port in range %s", p.Published)
		}
	}
	if published == "" {
		port, err := freePort(p.HostIP, p.Protocol)
		if err != nil {
			return "", err
		}
		published = strconv.Itoa(port)
	}
	spec := published + ":" + strconv.Itoa(int(p.Target))
	if p.HostIP != "" {
		host := p.HostIP
		if strings.Contains(host, ":") {
			host = "[" + host + "]"
		}
		spec = host + ":" + spec
	}
	if p.Protocol != "" && p.Protocol != "tcp" {
		spec += "/" + p.Protocol
	}
	return spec, nil
}

func portFree(host string, port int, proto string) bool {
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	if proto == "udp" {
		c, err := net.ListenPacket("udp", addr)
		if err != nil {
			return false
		}
		c.Close()
		return true
	}
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return false
	}
	l.Close()
	return true
}

func freePort(host, proto string) (int, error) {
	addr := net.JoinHostPort(host, "0")
	if proto == "udp" {
		c, err := net.ListenPacket("udp", addr)
		if err != nil {
			return 0, err
		}
		defer c.Close()
		return c.LocalAddr().(*net.UDPAddr).Port, nil
	}
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

func mebibytes(b int64) string {
	m := int64(math.Ceil(float64(b) / (1024 * 1024)))
	if m < 200 {
		m = 200 // Apple container's minimum
	}
	return strconv.FormatInt(m, 10) + "M"
}

func capName(c string) string {
	c = strings.ToUpper(c)
	if c == "ALL" || strings.HasPrefix(c, "CAP_") {
		return c
	}
	return "CAP_" + c
}

func dependsOnLabel(s types.ServiceConfig) string {
	var parts []string
	for _, name := range sortedKeys(s.DependsOn) {
		d := s.DependsOn[name]
		parts = append(parts, fmt.Sprintf("%s:%s:%t", name, d.Condition, d.Restart))
	}
	return strings.Join(parts, ",")
}

// unsupported lists service attributes the Apple runtime cannot honour.
func unsupported(s types.ServiceConfig, warn func(string, ...any)) {
	var ignored []string
	check := func(cond bool, name string) {
		if cond {
			ignored = append(ignored, name)
		}
	}
	check(len(s.Devices) > 0, "devices")
	check(len(s.SecurityOpt) > 0, "security_opt")
	check(s.Pid != "", "pid")
	check(s.Ipc != "", "ipc")
	check(s.Uts != "", "uts")
	check(s.UserNSMode != "", "userns_mode")
	check(len(s.GroupAdd) > 0, "group_add")
	check(s.CgroupParent != "" || s.Cgroup != "", "cgroup")
	check(len(s.Gpus) > 0, "gpus")
	check(s.Runtime != "", "runtime")
	check(s.Isolation != "", "isolation")
	check(s.PidsLimit != 0, "pids_limit")
	check(s.OomScoreAdj != 0 || s.OomKillDisable, "oom settings")
	check(s.StorageOpt != nil, "storage_opt")
	check(s.NetworkMode == "host", "network_mode: host (using the default network; publish ports explicitly)")
	check(strings.HasPrefix(s.NetworkMode, "service:") || strings.HasPrefix(s.NetworkMode, "container:"),
		"network_mode: "+s.NetworkMode+" (namespaces can't be shared; attached to the same networks instead)")
	if len(ignored) > 0 {
		warn("ignored (unsupported by Apple container): %s", strings.Join(ignored, ", "))
	}
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

type usage struct {
	volumes map[string]int // project volume key -> containers mounting it
	paths   map[string]int // host path -> containers mounting it
}

// mountUsage counts how many containers of the project mount each named
// volume and host path, to detect sharing.
func mountUsage(p *types.Project) usage {
	u := usage{volumes: map[string]int{}, paths: map[string]int{}}
	var add func(s types.ServiceConfig, n int, depth int)
	add = func(s types.ServiceConfig, n int, depth int) {
		for _, v := range s.Volumes {
			switch {
			case v.Type == types.VolumeTypeBind:
				u.paths[v.Source] += n
			case v.Type == types.VolumeTypeVolume && v.Source != "":
				if dev := bindDevice(p.Volumes[v.Source]); dev != "" {
					u.paths[dev] += n
				} else {
					u.volumes[v.Source] += n
				}
			}
		}
		if depth > 5 {
			return
		}
		for _, from := range s.VolumesFrom {
			name, _, _ := strings.Cut(from, ":")
			if other, ok := p.Services[name]; ok {
				add(other, n, depth+1)
			}
		}
	}
	for _, s := range p.Services {
		add(s, s.GetScale(), 0)
	}
	return u
}

// dataRoot holds data compose manages outside the runtime.
func (e *Engine) dataRoot() string {
	return filepath.Dir(filepath.Dir(e.stateDir("x")))
}

func (e *Engine) sharedVolumeDir(p *types.Project, key string) string {
	return filepath.Join(e.dataRoot(), "volumes", VolumeName(p, key))
}
