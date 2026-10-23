package compose

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/compose-spec/compose-go/v2/types"

	"github.com/pedrospdc/apple-container-compose/internal/ctr"
)

// Apple container only resolves container names for DNS domains registered
// with `sudo container system dns create`, and single-label aliases are
// rejected outright. To give services Docker-like name resolution without
// root, each project runs a tiny CoreDNS sidecar attached to every project
// network. It serves a hosts file per network (selected by client subnet via
// the `view` plugin) and forwards everything else to the network gateway.
//
// The hosts files live in a host directory bind-mounted into the sidecar;
// directory mounts propagate in-place writes, and CoreDNS re-reads the files
// every second, so the records stay current as containers come and go.

// DNSImage is the CoreDNS image used for the sidecar.
var DNSImage = envOr("COMPOSE_DNS_IMAGE", "docker.io/coredns/coredns:1.12.1")

const dnsLayoutVersion = "1"

func dnsEnabled() bool {
	v := strings.ToLower(os.Getenv("COMPOSE_DNS"))
	return v != "0" && v != "false" && v != "off"
}

func (e *Engine) dnsDir(project string) string {
	return filepath.Join(e.stateDir(project), "dns")
}

func hostsFileName(network string) string {
	return sanitizeName(network) + ".hosts"
}

// projectRuntimeNetworks returns the runtime network names services use.
func (e *Engine) projectRuntimeNetworks(p *types.Project) []string {
	set := map[string]bool{}
	for _, s := range p.Services {
		nets, err := e.serviceNetworks(p, s)
		if err != nil {
			continue
		}
		for _, n := range nets {
			if n.runtime != "none" {
				set[n.runtime] = true
			}
		}
	}
	return sortedKeys(set)
}

// ensureDNS makes sure the sidecar runs on all project networks and records
// its address on each of them.
func (e *Engine) ensureDNS(ctx context.Context, p *types.Project) error {
	if !dnsEnabled() {
		return nil
	}
	networks := e.projectRuntimeNetworks(p)
	if len(networks) == 0 {
		return nil
	}
	type netInfo struct{ name, subnet, gateway string }
	var infos []netInfo
	for _, n := range networks {
		info, err := e.ctr.InspectNetwork(ctx, n)
		if err != nil {
			return err
		}
		if info == nil {
			if e.ctr.DryRun {
				continue
			}
			return fmt.Errorf("network %s not found", n)
		}
		infos = append(infos, netInfo{n, info.Status.IPv4Subnet, info.Status.IPv4Gateway})
	}

	var corefile strings.Builder
	for _, n := range infos {
		fmt.Fprintf(&corefile, ".:53 {\n")
		if n.subnet != "" {
			fmt.Fprintf(&corefile, "  view %s {\n    expr incidr(client_ip(), '%s')\n  }\n", sanitizeName(n.name), n.subnet)
		}
		fmt.Fprintf(&corefile, "  hosts /etc/compose/%s {\n    ttl 1\n    reload 1s\n    fallthrough\n  }\n", hostsFileName(n.name))
		if n.gateway != "" {
			fmt.Fprintf(&corefile, "  forward . %s\n", n.gateway)
		}
		fmt.Fprintf(&corefile, "  errors\n}\n")
	}
	conf := corefile.String()
	hash := shortID(dnsLayoutVersion + conf)

	dir := e.dnsDir(p.Name)
	name := DNSContainerName(p.Name)
	existing, err := e.ctr.InspectContainer(ctx, name)
	if err != nil {
		return err
	}
	if existing != nil && existing.Labels()[LabelConfigHash] != hash {
		if _, err := e.ctr.Exec(ctx, "delete", "--force", name); err != nil {
			return err
		}
		existing = nil
	}
	if !e.ctr.DryRun {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, "Corefile"), []byte(conf), 0o644); err != nil {
			return err
		}
		for _, n := range infos {
			path := filepath.Join(dir, hostsFileName(n.name))
			if _, err := os.Stat(path); os.IsNotExist(err) {
				if err := os.WriteFile(path, nil, 0o644); err != nil {
					return err
				}
			}
		}
	}

	switch {
	case existing == nil:
		args := []string{"run", "--detach", "--name", name,
			"--label", LabelProject + "=" + p.Name,
			"--label", LabelRole + "=" + RoleDNS,
			"--label", LabelConfigHash + "=" + hash,
			"--cpus", "1", "--memory", "256M",
			"--volume", dir + ":/etc/compose:ro"}
		for _, n := range infos {
			args = append(args, "--network", n.name)
		}
		args = append(args, DNSImage, "-conf", "/etc/compose/Corefile")
		e.status("DNS", name, "Starting")
		if _, err := e.ctr.Exec(ctx, args...); err != nil {
			e.warnf("could not start DNS sidecar; services won't resolve each other by name: %v", err)
			return nil
		}
	case !existing.Running():
		if _, err := e.ctr.Exec(ctx, "start", name); err != nil {
			return err
		}
	}
	return e.loadDNSAddrs(ctx, p.Name)
}

func (e *Engine) loadDNSAddrs(ctx context.Context, project string) error {
	if e.ctr.DryRun {
		return nil
	}
	c, err := e.ctr.InspectContainer(ctx, DNSContainerName(project))
	if err != nil || c == nil {
		return err
	}
	addrs := map[string]string{}
	for _, n := range c.Status.Networks {
		addrs[n.Network] = n.IP()
	}
	e.mu.Lock()
	e.dnsAddrs[project] = addrs
	e.mu.Unlock()
	return nil
}

// dnsServerFor returns the sidecar address reachable on network.
func (e *Engine) dnsServerFor(ctx context.Context, project, network string) string {
	if !dnsEnabled() {
		return ""
	}
	e.mu.Lock()
	addrs, ok := e.dnsAddrs[project]
	e.mu.Unlock()
	if !ok {
		_ = e.loadDNSAddrs(ctx, project)
		e.mu.Lock()
		addrs = e.dnsAddrs[project]
		e.mu.Unlock()
	}
	return addrs[network]
}

// dnsReloadDelay covers CoreDNS's 1s hosts-file reload interval.
const dnsReloadDelay = 1200 * time.Millisecond

// refreshHosts rewrites the per-network hosts files from live state and
// reports whether any record changed.
func (e *Engine) refreshHosts(ctx context.Context, p *types.Project) (bool, error) {
	if !dnsEnabled() || e.ctr.DryRun {
		return false, nil
	}
	dir := e.dnsDir(p.Name)
	if _, err := os.Stat(dir); err != nil {
		return false, nil
	}
	all, err := e.ctr.ListContainers(ctx, false)
	if err != nil {
		return false, err
	}
	changed := false
	records := map[string][]string{} // network -> lines
	gateways := map[string]string{}
	for _, c := range all {
		l := c.Labels()
		if l[LabelProject] != p.Name || l[LabelRole] != "" {
			continue
		}
		svcName := l[LabelService]
		svc, hasSvc := p.Services[svcName]
		for _, n := range c.Status.Networks {
			if n.IP() == "" {
				continue
			}
			nw := sanitizeName(n.Network)
			gateways[nw] = n.IPv4Gateway
			names := []string{c.Name()}
			if svcName != "" && l[LabelOneoff] != "True" {
				names = append(names, svcName)
			}
			if hasSvc && l[LabelOneoff] != "True" {
				for key, cfg := range svc.Networks {
					if NetworkName(p, key) != n.Network || cfg == nil {
						continue
					}
					names = append(names, cfg.Aliases...)
				}
				if svc.Hostname != "" {
					names = append(names, svc.Hostname)
				}
			}
			records[nw] = append(records[nw], n.IP()+" "+strings.Join(dedupe(names), " "))
		}
	}
	// links, external_links and extra_hosts are applied network-wide.
	for _, svc := range p.Services {
		for _, link := range svc.Links {
			target, alias, ok := strings.Cut(link, ":")
			if !ok {
				continue
			}
			for nw, lines := range records {
				for _, line := range lines {
					fields := strings.Fields(line)
					if contains(fields[1:], target) {
						records[nw] = append(records[nw], fields[0]+" "+alias)
						break
					}
				}
			}
		}
		for host, ips := range svc.ExtraHosts {
			for nw := range records {
				for _, ip := range ips {
					if ip == "host-gateway" {
						ip = gateways[nw]
					}
					records[nw] = append(records[nw], ip+" "+host)
				}
			}
		}
	}
	entries, _ := os.ReadDir(dir)
	for _, ent := range entries {
		if !strings.HasSuffix(ent.Name(), ".hosts") {
			continue
		}
		network := strings.TrimSuffix(ent.Name(), ".hosts")
		lines := records[network]
		if gw := gateways[network]; gw != "" {
			lines = append(lines, gw+" host.docker.internal host.containers.internal gateway.docker.internal")
		}
		sort.Strings(lines)
		content := strings.Join(lines, "\n")
		if content != "" {
			content += "\n"
		}
		path := filepath.Join(dir, ent.Name())
		if old, _ := os.ReadFile(path); string(old) == content {
			continue
		}
		// Write in place: renames aren't picked up through virtiofs.
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			return changed, err
		}
		changed = true
	}
	return changed, nil
}

// removeDNS deletes the sidecar and its state.
func (e *Engine) removeDNS(ctx context.Context, project string) error {
	name := DNSContainerName(project)
	c, err := e.ctr.InspectContainer(ctx, name)
	if err != nil {
		return err
	}
	if c != nil {
		if _, err := e.ctr.Exec(ctx, "delete", "--force", name); err != nil && !ctr.IsNotFound(err) {
			return err
		}
	}
	if !e.ctr.DryRun {
		_ = os.RemoveAll(e.dnsDir(project))
	}
	e.mu.Lock()
	delete(e.dnsAddrs, project)
	e.mu.Unlock()
	return nil
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
