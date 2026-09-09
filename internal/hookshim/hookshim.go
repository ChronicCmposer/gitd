// Package hookshim implements the two static git hook shims (R7-Q10).
//
// The from-scratch image has no /bin/sh, so the git hooks under
// /usr/local/lib/gitd/hooks (R6-Q2) are tiny static Go binaries. Each shim
// execs the real gitd subcommand with the absolute --config path baked at
// build time (R11-Q7: the image rootfs is immutable and mounted --rootfs-ro,
// so there is no runtime override path) and passes stdio + exit code through
// untouched.
package hookshim

import (
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
)

// gitdConfig is the baked absolute config path (R11-Q7).
const gitdConfig = "/etc/gitd/gitd.yaml"

// gitdBinary is the image's gitd path (the hook shims live beside it under
// /usr/local/lib/gitd/hooks).
const gitdBinary = "/usr/local/bin/gitd"

// subcommandFor maps the hook binary's own name to the gitd verb it must
// exec: pre-receive -> gitd pre-receive, post-receive -> gitd notify.
func subcommandFor(bin string) (string, error) {
	switch filepath.Base(bin) {
	case "pre-receive":
		return "pre-receive", nil
	case "post-receive":
		return "notify", nil
	default:
		return "", fmt.Errorf("hookshim: unknown hook binary %q", bin)
	}
}

// Run execs the gitd subcommand named by argv[0], wiring the hook's
// stdin/stdout/stderr to the child and returning the child's exit code. A
// missing gitd or an unknown hook name fails loudly with exit 1.
func Run(argv []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(argv) == 0 {
		fmt.Fprintln(stderr, "gitd: hookshim: no argv[0]; expected pre-receive or post-receive")
		return 1
	}
	sub, err := subcommandFor(argv[0])
	if err != nil {
		fmt.Fprintln(stderr, "gitd:", err)
		return 1
	}

	cmd := exec.Command(gitdBinary, sub, "--config", gitdConfig)
	cmd.Stdin = stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return exitErr.ExitCode()
		}
		fmt.Fprintf(stderr, "gitd: hookshim: %v\n", err)
		return 1
	}
	return 0
}
