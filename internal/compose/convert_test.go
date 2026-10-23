package compose

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/compose-spec/compose-go/v2/types"

	"github.com/pedrospdc/apple-container-compose/internal/ctr"
)

func loadYAML(t *testing.T, yaml string) *types.Project {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "compose.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	p, err := (&ProjectOptions{ProjectName: "demo", ConfigPaths: []string{path}}).Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func testEngine() *Engine {
	c := ctr.New()
	c.Bin = "/usr/bin/false" // never reach the real runtime in unit tests
	c.DryRun = true
	return NewEngine(c)
}

func flagValues(flags []string, name string) []string {
	var out []string
	for i := 0; i < len(flags)-1; i++ {
		if flags[i] == name {
			out = append(out, flags[i+1])
		}
	}
	return out
}

func TestToContainerSpec(t *testing.T) {
	p := loadYAML(t, `
services:
  web:
    image: nginx:alpine
    entrypoint: ["/bin/sh", "-c"]
    command: ["echo hi"]
    environment:
      A: "1"
    ports: ["127.0.0.1:8080:80", "53:53/udp"]
    cap_add: [NET_ADMIN]
    mem_limit: 512m
    cpus: 1.5
    user: "1000:1000"
    working_dir: /app
    labels:
      x: y
    networks: [front]
    sysctls:
      net.core.somaxconn: 1024
networks:
  front: {}
`)
	e := testEngine()
	spec, err := e.toContainerSpec(context.Background(), p, p.Services["web"], createOptions{Name: "demo-web-1", Number: 1})
	if err != nil {
		t.Fatal(err)
	}
	f := spec.Flags
	check := func(flag string, want ...string) {
		t.Helper()
		got := flagValues(f, flag)
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("%s = %v, want %v", flag, got, want)
		}
	}
	check("--entrypoint", "/bin/sh")
	check("--env", "A=1")
	check("--publish", "127.0.0.1:8080:80", "53:53/udp")
	check("--cap-add", "CAP_NET_ADMIN")
	check("--memory", "512M")
	check("--cpus", "2")
	check("--user", "1000:1000")
	check("--workdir", "/app")
	check("--network", "demo_front")
	if strings.Join(spec.Args, " ") != "-c echo hi" {
		t.Errorf("args = %q", spec.Args)
	}
	labels := strings.Join(flagValues(f, "--label"), " ")
	for _, want := range []string{"x=y", LabelProject + "=demo", LabelService + "=web", LabelNumber + "=1"} {
		if !strings.Contains(labels, want) {
			t.Errorf("labels missing %s: %s", want, labels)
		}
	}
	check("--kernel-arg", "sysctl.net.core.somaxconn=1024", "sysctl.net.ipv4.ip_unprivileged_port_start=0", `sysctl.net.ipv4.ping_group_range="0 2147483647"`)
	if len(spec.Warnings) != 0 {
		t.Errorf("warnings = %v", spec.Warnings)
	}
}

func TestSecretsAndConfigs(t *testing.T) {
	p := loadYAML(t, `
services:
  app:
    image: alpine
    secrets: [token]
    configs:
      - source: conf
        target: /etc/app.conf
secrets:
  token:
    environment: HOME
configs:
  conf:
    content: "hello"
`)
	e := testEngine()
	spec, err := e.toContainerSpec(context.Background(), p, p.Services["app"], createOptions{Name: "demo-app-1", Number: 1})
	if err != nil {
		t.Fatal(err)
	}
	vols := strings.Join(flagValues(spec.Flags, "--volume"), " ")
	if !strings.Contains(vols, ":/run/secrets/token:ro") || !strings.Contains(vols, ":/etc/app.conf:ro") {
		t.Errorf("volumes = %s", vols)
	}
}

func TestPortSpec(t *testing.T) {
	cases := []struct {
		in   types.ServicePortConfig
		want string
	}{
		{types.ServicePortConfig{Target: 80, Published: "8080", Protocol: "tcp"}, "8080:80"},
		{types.ServicePortConfig{Target: 53, Published: "5353", Protocol: "udp"}, "5353:53/udp"},
		{types.ServicePortConfig{Target: 80, Published: "8080", HostIP: "::1"}, "[::1]:8080:80"},
	}
	for _, c := range cases {
		got, err := portSpec(c.in)
		if err != nil || got != c.want {
			t.Errorf("portSpec(%+v) = %q, %v; want %q", c.in, got, err, c.want)
		}
	}
	got, err := portSpec(types.ServicePortConfig{Target: 80})
	if err != nil || !strings.HasSuffix(got, ":80") {
		t.Errorf("random port: %q %v", got, err)
	}
}

func TestIgnorePatterns(t *testing.T) {
	cases := []struct {
		rel  string
		pats []string
		want bool
	}{
		{"node_modules/a/b.js", []string{"node_modules/"}, true},
		{"src/node_modules/x", []string{"node_modules"}, true},
		{"src/main.go", []string{"*.md"}, false},
		{"docs/readme.md", []string{"**/*.md"}, true},
		{"build/out.o", []string{"build/**"}, true},
	}
	for _, c := range cases {
		if got := ignored(c.rel, c.pats); got != c.want {
			t.Errorf("ignored(%q, %v) = %v", c.rel, c.pats, got)
		}
	}
}

func TestRestartPolicy(t *testing.T) {
	p := restartPolicyOf(types.ServiceConfig{Restart: "on-failure:2"})
	if !p.shouldRestart(1, 0) || !p.shouldRestart(1, 1) || p.shouldRestart(1, 2) || p.shouldRestart(0, 0) {
		t.Errorf("on-failure:2 misbehaves: %+v", p)
	}
	if !restartPolicyOf(types.ServiceConfig{Restart: "unless-stopped"}).shouldRestart(0, 5) {
		t.Error("unless-stopped should restart")
	}
	if restartPolicyOf(types.ServiceConfig{Restart: "no"}).shouldRestart(1, 0) {
		t.Error("no should not restart")
	}
}

func TestNormalizeRef(t *testing.T) {
	if !sameImage("docker.io/library/nginx:latest", "nginx") {
		t.Error("nginx should equal docker.io/library/nginx:latest")
	}
	if sameImage("nginx:1", "nginx:2") {
		t.Error("different tags are different images")
	}
	if !sameImage("ghcr.io/a/b:1", "ghcr.io/a/b:1") {
		t.Error("identical refs")
	}
}

func TestSystemPath(t *testing.T) {
	for _, p := range []string{"/proc/cpuinfo", "/var/run/docker.sock", "/etc/timezone", "/dev/dri"} {
		if !systemPath(p) {
			t.Errorf("%s should be a system path", p)
		}
	}
	for _, p := range []string{"/mnt/docker-volumes/x", "/Users/me/data", "/srv/app"} {
		if systemPath(p) {
			t.Errorf("%s should not be a system path", p)
		}
	}
}

func TestMountUsage(t *testing.T) {
	p := loadYAML(t, `
services:
  web:
    image: nginx
    volumes: [shared:/a, solo:/b]
  php:
    image: php
    volumes: [shared:/a]
  worker:
    image: alpine
    scale: 2
    volumes: [scaled:/c]
  helper:
    image: alpine
    volumes_from: [php]
volumes:
  shared: {}
  solo: {}
  scaled: {}
`)
	u := mountUsage(p)
	if u.volumes["shared"] != 3 || u.volumes["solo"] != 1 || u.volumes["scaled"] != 2 {
		t.Errorf("usage = %v", u.volumes)
	}
	e := testEngine()
	spec, err := e.toContainerSpec(context.Background(), p, p.Services["web"], createOptions{Name: "demo-web-1", Number: 1})
	if err != nil {
		t.Fatal(err)
	}
	vols := strings.Join(flagValues(spec.Flags, "--volume"), " ")
	if !strings.Contains(vols, "/volumes/demo_shared:/a") || !strings.Contains(vols, "demo_solo:/b") {
		t.Errorf("volumes = %s", vols)
	}
}
