package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// aliasName is the command other tooling (Makefiles, scripts) expects.
const aliasName = "docker-compose"

func aliasCommand() *cobra.Command {
	var yes, force, remove bool
	var dir string
	cmd := &cobra.Command{
		Use:   "alias [OPTIONS]",
		Short: "Install a docker-compose command that runs container-compose",
		Long: `Creates a "docker-compose" symlink to this binary so Makefiles and scripts that call
docker-compose use Apple container. A symlink is used rather than a shell alias because
make and scripts run /bin/sh, which doesn't see interactive shell aliases.

By default it asks for confirmation, and does nothing if another docker-compose is
already on PATH.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			out := cmd.OutOrStdout()
			self, err := selfPath()
			if err != nil {
				return err
			}
			existing, _ := exec.LookPath(aliasName)
			ours := existing != "" && sameFile(existing, self)

			if remove {
				if existing == "" || !ours {
					fmt.Fprintf(out, "No %s alias to container-compose found.\n", aliasName)
					return nil
				}
				if err := os.Remove(existing); err != nil {
					return err
				}
				fmt.Fprintf(out, "Removed %s\n", existing)
				return nil
			}
			if ours {
				fmt.Fprintf(out, "%s already runs container-compose (%s).\n", aliasName, existing)
				return nil
			}
			if existing != "" && !force {
				fmt.Fprintf(out, "%s is already installed at %s; leaving it alone (use --force to override).\n", aliasName, existing)
				return nil
			}
			if !yes {
				ok, err := confirm(out, fmt.Sprintf("%s was not found. Create a %s command that runs container-compose (for Makefiles and scripts)? [Y/n] ", aliasName, aliasName))
				if err != nil {
					fmt.Fprintf(out, "No terminal to ask on; run `container-compose alias --yes` to create the %s alias.\n", aliasName)
					return nil
				}
				if !ok {
					fmt.Fprintln(out, "Skipped. You can run `container-compose alias` at any time.")
					return nil
				}
			}

			target := dir
			if target == "" {
				target = filepath.Dir(self)
				if !writable(target) {
					home, _ := os.UserHomeDir()
					target = filepath.Join(home, ".local", "bin")
				}
			}
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			link := filepath.Join(target, aliasName)
			if force {
				_ = os.Remove(link)
			}
			if err := os.Symlink(self, link); err != nil {
				return fmt.Errorf("creating %s: %w", link, err)
			}
			fmt.Fprintf(out, "Created %s -> %s\n", link, self)
			if !onPath(target) {
				fmt.Fprintf(out, "Note: %s is not on your PATH; add it to use %s.\n", target, aliasName)
			}
			return nil
		},
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "Don't ask for confirmation")
	cmd.Flags().BoolVar(&force, "force", false, "Replace an existing docker-compose")
	cmd.Flags().BoolVar(&remove, "remove", false, "Remove the docker-compose alias")
	cmd.Flags().StringVar(&dir, "dir", "", "Directory for the symlink (default: next to container-compose, or ~/.local/bin)")
	return cmd
}

// selfPath returns the path container-compose is reachable at, preferring
// the PATH entry (e.g. Homebrew's bin symlink) so upgrades keep the alias
// working, over the resolved file in a versioned directory.
func selfPath() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if p, err := exec.LookPath("container-compose"); err == nil && sameFile(p, exe) {
		return filepath.Abs(p)
	}
	return exe, nil
}

func sameFile(a, b string) bool {
	ra, err1 := filepath.EvalSymlinks(a)
	rb, err2 := filepath.EvalSymlinks(b)
	return err1 == nil && err2 == nil && ra == rb
}

func writable(dir string) bool {
	f, err := os.CreateTemp(dir, ".container-compose-*")
	if err != nil {
		return false
	}
	f.Close()
	os.Remove(f.Name())
	return true
}

func onPath(dir string) bool {
	for _, p := range filepath.SplitList(os.Getenv("PATH")) {
		if filepath.Clean(p) == filepath.Clean(dir) {
			return true
		}
	}
	return false
}

// confirm asks a yes/no question on the controlling terminal, which also
// works when stdin is a pipe (e.g. `curl ... | sh`).
func confirm(out io.Writer, question string) (bool, error) {
	var in io.Reader = os.Stdin
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		tty, err := os.Open("/dev/tty")
		if err != nil {
			return false, errors.New("no terminal")
		}
		defer tty.Close()
		in = tty
	}
	fmt.Fprint(out, question)
	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && line == "" {
		return false, errors.New("no answer")
	}
	answer := strings.ToLower(strings.TrimSpace(line))
	return answer == "" || answer == "y" || answer == "yes", nil
}
