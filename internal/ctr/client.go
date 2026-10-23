// Package ctr wraps the Apple `container` CLI.
//
// The CLI is the only stable public interface to the Apple container
// runtime, so every operation shells out to it and parses its JSON output.
package ctr

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
)

// Client runs `container` commands.
type Client struct {
	Bin    string
	DryRun bool
	Debug  bool
	Stderr io.Writer

	imageMu    sync.Mutex
	imageCache map[string]*Image
}

// New returns a client using the `container` binary found on PATH, or the
// one named by $CONTAINER_BIN.
func New() *Client {
	bin := os.Getenv("CONTAINER_BIN")
	if bin == "" {
		bin = "container"
	}
	return &Client{Bin: bin, Stderr: os.Stderr, imageCache: map[string]*Image{}}
}

// ExitError carries the exit status of a failed `container` invocation.
type ExitError struct {
	Args   []string
	Code   int
	Stderr string
}

func (e *ExitError) Error() string {
	var lines []string
	for _, l := range strings.Split(e.Stderr, "\n") {
		// Drop progress lines like "[3/6] Fetching kernel [0s]".
		if t := strings.TrimSpace(l); t != "" && !progressLine.MatchString(t) {
			lines = append(lines, strings.TrimPrefix(t, "Error: "))
		}
	}
	msg := strings.Join(lines, "\n")
	if msg == "" {
		msg = fmt.Sprintf("exit status %d", e.Code)
	}
	verb := e.Args[0]
	if len(e.Args) > 1 && !strings.HasPrefix(e.Args[1], "-") {
		verb += " " + e.Args[1]
	}
	return fmt.Sprintf("container %s: %s", verb, msg)
}

var progressLine = regexp.MustCompile(`^\[\d+/\d+\]`)

// IsNotFound reports whether err looks like a "not found" error from the CLI.
func IsNotFound(err error) bool {
	var ee *ExitError
	if errors.As(err, &ee) {
		s := strings.ToLower(ee.Stderr)
		return strings.Contains(s, "not found") || strings.Contains(s, "notfound")
	}
	return false
}

// Command builds an *exec.Cmd for the given arguments.
func (c *Client) Command(ctx context.Context, args ...string) *exec.Cmd {
	c.trace(args)
	return exec.CommandContext(ctx, c.Bin, args...)
}

func (c *Client) trace(args []string) {
	if c.Debug || c.DryRun {
		prefix := "+"
		if c.DryRun {
			prefix = "DRY-RUN:"
		}
		fmt.Fprintf(c.Stderr, "%s container %s\n", prefix, shellJoin(args))
	}
}

// Output runs a read-only command and returns stdout. It is executed even in
// dry-run mode because it does not mutate state.
func (c *Client) Output(ctx context.Context, args ...string) ([]byte, error) {
	if c.Debug {
		c.trace(args)
	}
	cmd := exec.CommandContext(ctx, c.Bin, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return stdout.Bytes(), wrapErr(args, err, stderr.String())
	}
	return stdout.Bytes(), nil
}

// Exec runs a mutating command, capturing output. Skipped in dry-run mode.
func (c *Client) Exec(ctx context.Context, args ...string) (string, error) {
	c.trace(args)
	if c.DryRun {
		return "", nil
	}
	cmd := exec.CommandContext(ctx, c.Bin, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return stdout.String(), wrapErr(args, err, stderr.String())
	}
	return strings.TrimSpace(stdout.String()), nil
}

// Stream runs a command connected to the given stdio. Skipped in dry-run mode.
func (c *Client) Stream(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer, args ...string) error {
	c.trace(args)
	if c.DryRun {
		return nil
	}
	cmd := exec.CommandContext(ctx, c.Bin, args...)
	cmd.Stdin = stdin
	cmd.Stdout = stdout
	var errBuf bytes.Buffer
	if stderr != nil {
		cmd.Stderr = io.MultiWriter(stderr, &errBuf)
	} else {
		cmd.Stderr = &errBuf
	}
	if err := cmd.Run(); err != nil {
		return wrapErr(args, err, errBuf.String())
	}
	return nil
}

func wrapErr(args []string, err error, stderr string) error {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return &ExitError{Args: args, Code: ee.ExitCode(), Stderr: stderr}
	}
	return err
}

func shellJoin(args []string) string {
	out := make([]string, len(args))
	for i, a := range args {
		if a == "" || strings.ContainsAny(a, " \t\n'\"$`\\|&;<>()*?[]{}") {
			out[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
		} else {
			out[i] = a
		}
	}
	return strings.Join(out, " ")
}

// ---- containers ----

// Container is the subset of `container inspect` output used by compose.
type Container struct {
	ID            string `json:"id"`
	Configuration struct {
		ID    string `json:"id"`
		Image struct {
			Reference  string `json:"reference"`
			Descriptor struct {
				Digest string `json:"digest"`
			} `json:"descriptor"`
		} `json:"image"`
		InitProcess struct {
			Executable string   `json:"executable"`
			Arguments  []string `json:"arguments"`
		} `json:"initProcess"`
		Labels         map[string]string `json:"labels"`
		CreationDate   string            `json:"creationDate"`
		PublishedPorts []PublishedPort   `json:"publishedPorts"`
		Networks       []struct {
			Network string `json:"network"`
		} `json:"networks"`
		Mounts []Mount `json:"mounts"`
	} `json:"configuration"`
	Status struct {
		State       string              `json:"state"`
		StartedDate string              `json:"startedDate"`
		Networks    []NetworkAttachment `json:"networks"`
	} `json:"status"`
}

// Mount is a filesystem mount of a container.
type Mount struct {
	Source      string         `json:"source"`
	Destination string         `json:"destination"`
	Type        map[string]any `json:"type"`
}

// VolumeName returns the volume name for named-volume mounts.
func (m Mount) VolumeName() string {
	if v, ok := m.Type["volume"].(map[string]any); ok {
		if n, ok := v["name"].(string); ok {
			return n
		}
	}
	return ""
}

// PublishedPort is a host port mapping.
type PublishedPort struct {
	HostAddress   string `json:"hostAddress"`
	HostPort      int    `json:"hostPort"`
	ContainerPort int    `json:"containerPort"`
	Proto         string `json:"proto"`
	Count         int    `json:"count"`
}

// NetworkAttachment describes a live network attachment.
type NetworkAttachment struct {
	Network     string `json:"network"`
	Hostname    string `json:"hostname"`
	IPv4Address string `json:"ipv4Address"`
	IPv4Gateway string `json:"ipv4Gateway"`
}

// IP returns the IPv4 address without prefix length.
func (n NetworkAttachment) IP() string {
	ip, _, _ := strings.Cut(n.IPv4Address, "/")
	return ip
}

// Name returns the container ID.
func (c *Container) Name() string {
	if c.Configuration.ID != "" {
		return c.Configuration.ID
	}
	return c.ID
}

// Labels returns the container labels (never nil).
func (c *Container) Labels() map[string]string {
	if c.Configuration.Labels == nil {
		return map[string]string{}
	}
	return c.Configuration.Labels
}

// Running reports whether the container is running.
func (c *Container) Running() bool { return c.Status.State == "running" }

// Command returns the init process command line.
func (c *Container) Command() string {
	parts := append([]string{c.Configuration.InitProcess.Executable}, c.Configuration.InitProcess.Arguments...)
	return strings.Join(parts, " ")
}

// ListContainers lists containers; all includes stopped ones.
func (c *Client) ListContainers(ctx context.Context, all bool) ([]Container, error) {
	args := []string{"list", "--format", "json"}
	if all {
		args = append(args, "--all")
	}
	out, err := c.Output(ctx, args...)
	if err != nil {
		return nil, err
	}
	var cs []Container
	if err := json.Unmarshal(out, &cs); err != nil {
		return nil, fmt.Errorf("parsing container list: %w", err)
	}
	return cs, nil
}

// InspectContainer returns a single container, or nil when it doesn't exist.
func (c *Client) InspectContainer(ctx context.Context, id string) (*Container, error) {
	out, err := c.Output(ctx, "inspect", id)
	if err != nil {
		if IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	var cs []Container
	if err := json.Unmarshal(out, &cs); err != nil {
		return nil, fmt.Errorf("parsing inspect output: %w", err)
	}
	if len(cs) == 0 {
		return nil, nil
	}
	return &cs[0], nil
}

// ---- networks ----

// Network is the subset of `container network inspect` output used.
type Network struct {
	ID            string `json:"id"`
	Configuration struct {
		Name   string            `json:"name"`
		Labels map[string]string `json:"labels"`
		Mode   string            `json:"mode"`
	} `json:"configuration"`
	Status struct {
		IPv4Gateway string `json:"ipv4Gateway"`
		IPv4Subnet  string `json:"ipv4Subnet"`
	} `json:"status"`
}

// ListNetworks lists networks.
func (c *Client) ListNetworks(ctx context.Context) ([]Network, error) {
	out, err := c.Output(ctx, "network", "list", "--format", "json")
	if err != nil {
		return nil, err
	}
	var ns []Network
	if err := json.Unmarshal(out, &ns); err != nil {
		return nil, fmt.Errorf("parsing network list: %w", err)
	}
	return ns, nil
}

// InspectNetwork returns the named network or nil.
func (c *Client) InspectNetwork(ctx context.Context, name string) (*Network, error) {
	ns, err := c.ListNetworks(ctx)
	if err != nil {
		return nil, err
	}
	for i := range ns {
		if ns[i].ID == name || ns[i].Configuration.Name == name {
			return &ns[i], nil
		}
	}
	return nil, nil
}

// ---- volumes ----

// Volume is the subset of `container volume inspect` output used.
type Volume struct {
	ID            string `json:"id"`
	Configuration struct {
		Name        string            `json:"name"`
		Labels      map[string]string `json:"labels"`
		Driver      string            `json:"driver"`
		SizeInBytes int64             `json:"sizeInBytes"`
		Source      string            `json:"source"`
	} `json:"configuration"`
}

// Name returns the volume name.
func (v *Volume) Name() string {
	if v.Configuration.Name != "" {
		return v.Configuration.Name
	}
	return v.ID
}

// ListVolumes lists volumes.
func (c *Client) ListVolumes(ctx context.Context) ([]Volume, error) {
	out, err := c.Output(ctx, "volume", "list", "--format", "json")
	if err != nil {
		return nil, err
	}
	var vs []Volume
	if err := json.Unmarshal(out, &vs); err != nil {
		return nil, fmt.Errorf("parsing volume list: %w", err)
	}
	return vs, nil
}

// ---- images ----

// Image is the subset of `container image inspect` output used.
type Image struct {
	ID            string `json:"id"`
	Configuration struct {
		Name       string `json:"name"`
		Descriptor struct {
			Digest string `json:"digest"`
			Size   int64  `json:"size"`
		} `json:"descriptor"`
	} `json:"configuration"`
	Variants []ImageVariant `json:"variants"`
}

// ImageVariant is one platform variant of an image.
type ImageVariant struct {
	Size     int64 `json:"size"`
	Platform struct {
		Architecture string `json:"architecture"`
		OS           string `json:"os"`
	} `json:"platform"`
	Config struct {
		Config ImageConfig `json:"config"`
	} `json:"config"`
}

// ImageConfig is the OCI image runtime config.
type ImageConfig struct {
	Cmd         []string          `json:"Cmd"`
	Entrypoint  []string          `json:"Entrypoint"`
	Env         []string          `json:"Env"`
	User        string            `json:"User"`
	WorkingDir  string            `json:"WorkingDir"`
	Healthcheck *ImageHealthcheck `json:"Healthcheck"`
	Labels      map[string]string `json:"Labels"`
	Volumes     map[string]any    `json:"Volumes"`
	ExposedPort map[string]any    `json:"ExposedPorts"`
}

// ImageHealthcheck is the Docker image HEALTHCHECK instruction.
type ImageHealthcheck struct {
	Test          []string `json:"Test"`
	Interval      int64    `json:"Interval"`
	Timeout       int64    `json:"Timeout"`
	StartPeriod   int64    `json:"StartPeriod"`
	StartInterval int64    `json:"StartInterval"`
	Retries       int      `json:"Retries"`
}

// Variant returns the variant matching the given architecture, falling back
// to the first linux variant.
func (i *Image) Variant(arch string) *ImageVariant {
	var fallback *ImageVariant
	for k := range i.Variants {
		v := &i.Variants[k]
		if v.Platform.OS != "linux" {
			continue
		}
		if v.Platform.Architecture == arch {
			return v
		}
		if fallback == nil {
			fallback = v
		}
	}
	return fallback
}

// HasArch reports whether the image provides a linux variant for arch.
func (i *Image) HasArch(arch string) bool {
	for _, v := range i.Variants {
		if v.Platform.OS == "linux" && v.Platform.Architecture == arch {
			return true
		}
	}
	return false
}

// ListImages lists local images.
func (c *Client) ListImages(ctx context.Context) ([]Image, error) {
	out, err := c.Output(ctx, "image", "list", "--format", "json")
	if err != nil {
		return nil, err
	}
	var is []Image
	if err := json.Unmarshal(out, &is); err != nil {
		return nil, fmt.Errorf("parsing image list: %w", err)
	}
	return is, nil
}

// InspectImage returns a local image or nil when it isn't present.
func (c *Client) InspectImage(ctx context.Context, ref string) (*Image, error) {
	c.imageMu.Lock()
	if img, ok := c.imageCache[ref]; ok {
		c.imageMu.Unlock()
		return img, nil
	}
	c.imageMu.Unlock()
	out, err := c.Output(ctx, "image", "inspect", ref)
	if err != nil {
		if IsNotFound(err) {
			return nil, nil
		}
		var ee *ExitError
		if errors.As(err, &ee) {
			// The CLI reports missing images with assorted messages.
			return nil, nil
		}
		return nil, err
	}
	var is []Image
	if err := json.Unmarshal(out, &is); err != nil {
		return nil, fmt.Errorf("parsing image inspect: %w", err)
	}
	if len(is) == 0 {
		return nil, nil
	}
	c.imageMu.Lock()
	c.imageCache[ref] = &is[0]
	c.imageMu.Unlock()
	return &is[0], nil
}

// ForgetImage drops a cached image inspection (after pull/build).
func (c *Client) ForgetImage(ref string) {
	c.imageMu.Lock()
	delete(c.imageCache, ref)
	c.imageMu.Unlock()
}
