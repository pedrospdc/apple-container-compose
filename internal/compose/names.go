package compose

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/compose-spec/compose-go/v2/types"

	"github.com/pedrospdc/apple-container-compose/internal/ctr"
)

// Labels use the Docker Compose keys so tooling that understands them keeps
// working, plus a few of our own for resources Docker doesn't have.
const (
	LabelProject     = "com.docker.compose.project"
	LabelService     = "com.docker.compose.service"
	LabelNumber      = "com.docker.compose.container-number"
	LabelOneoff      = "com.docker.compose.oneoff"
	LabelConfigHash  = "com.docker.compose.config-hash"
	LabelWorkingDir  = "com.docker.compose.project.working_dir"
	LabelConfigFiles = "com.docker.compose.project.config_files"
	LabelImage       = "com.docker.compose.image"
	LabelDependsOn   = "com.docker.compose.depends_on"
	LabelVersion     = "com.docker.compose.version"
	LabelNetwork     = "com.docker.compose.network"
	LabelVolume      = "com.docker.compose.volume"
	LabelReplace     = "com.docker.compose.replace"

	// LabelRole marks containers that are compose internals (DNS sidecar).
	LabelRole = "dev.apple-container-compose.role"
	// LabelPlatform records the platform override chosen at create time.
	LabelPlatform = "dev.apple-container-compose.platform"
	// LabelAnonymous marks volumes created for anonymous mounts.
	LabelAnonymous = "dev.apple-container-compose.anonymous"
	// LabelRestart records the restart policy for the supervisor.
	LabelRestart = "dev.apple-container-compose.restart"
	// LabelHostPath marks volumes standing in for an uncreatable host path.
	LabelHostPath = "dev.apple-container-compose.host-path"

	RoleDNS = "dns"
)

// Version is set at build time.
var Version = "dev"

// ContainerName returns the canonical name for replica n of a service.
func ContainerName(project string, s types.ServiceConfig, n int) string {
	if s.ContainerName != "" {
		return s.ContainerName
	}
	return fmt.Sprintf("%s-%s-%d", project, s.Name, n)
}

// NetworkName returns the runtime name of a project network.
func NetworkName(p *types.Project, key string) string {
	if n, ok := p.Networks[key]; ok && n.Name != "" {
		return n.Name
	}
	return p.Name + "_" + key
}

// VolumeName returns the runtime name of a project volume.
func VolumeName(p *types.Project, key string) string {
	if v, ok := p.Volumes[key]; ok && v.Name != "" {
		return v.Name
	}
	return p.Name + "_" + key
}

// DNSContainerName is the name of the per-project DNS sidecar.
func DNSContainerName(project string) string {
	return project + "-compose-dns"
}

// ServiceHash returns a stable hash of the service definition, used to detect
// configuration drift and decide whether to recreate containers.
func ServiceHash(s types.ServiceConfig) (string, error) {
	// Fields that don't influence the container itself are excluded.
	s.Scale = nil
	if s.Deploy != nil {
		d := *s.Deploy
		d.Replicas = nil
		s.Deploy = &d
	}
	s.DependsOn = nil
	s.Profiles = nil
	s.Develop = nil
	s.Build = nil
	s.PullPolicy = ""
	s.CustomLabels = nil
	b, err := json.Marshal(s)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// ImageName returns the image a service runs, defaulting to the name compose
// gives built images.
func ImageName(project string, s types.ServiceConfig) string {
	if s.Image != "" {
		return s.Image
	}
	return project + "-" + s.Name
}

// ---- observed state helpers ----

// ProjectContainers filters containers belonging to a project.
func ProjectContainers(all []ctr.Container, project string, includeOneoff bool) []ctr.Container {
	var out []ctr.Container
	for _, c := range all {
		l := c.Labels()
		if l[LabelProject] != project || l[LabelRole] != "" {
			continue
		}
		if !includeOneoff && l[LabelOneoff] == "True" {
			continue
		}
		out = append(out, c)
	}
	SortContainers(out)
	return out
}

// ServiceContainers filters a project's containers by service.
func ServiceContainers(cs []ctr.Container, service string) []ctr.Container {
	var out []ctr.Container
	for _, c := range cs {
		if c.Labels()[LabelService] == service {
			out = append(out, c)
		}
	}
	return out
}

// FilterServices keeps containers of the named services (all when empty).
func FilterServices(cs []ctr.Container, services []string) []ctr.Container {
	if len(services) == 0 {
		return cs
	}
	want := map[string]bool{}
	for _, s := range services {
		want[s] = true
	}
	var out []ctr.Container
	for _, c := range cs {
		if want[c.Labels()[LabelService]] {
			out = append(out, c)
		}
	}
	return out
}

// ContainerNumber returns the replica number of a container.
func ContainerNumber(c ctr.Container) int {
	n, _ := strconv.Atoi(c.Labels()[LabelNumber])
	return n
}

// SortContainers orders by service then replica number.
func SortContainers(cs []ctr.Container) {
	sort.SliceStable(cs, func(i, j int) bool {
		si, sj := cs[i].Labels()[LabelService], cs[j].Labels()[LabelService]
		if si != sj {
			return si < sj
		}
		ni, nj := ContainerNumber(cs[i]), ContainerNumber(cs[j])
		if ni != nj {
			return ni < nj
		}
		return cs[i].Name() < cs[j].Name()
	})
}

// shortID returns a short random-looking suffix derived from a string.
func shortID(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:12]
}

func sanitizeName(s string) string {
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' {
			b.WriteRune(r)
		} else {
			b.WriteRune('_')
		}
	}
	return b.String()
}
