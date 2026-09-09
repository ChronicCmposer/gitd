package cli

import "io"

// runPreReceive enforces pre-receive push policies (Phase 5+): reads the
// old/new/ref lines on stdin, evaluates every line against the enabled
// policies, and rejects the push on any violation.
func runPreReceive(_ []string, _, _ io.Writer) error {
	return errNotImplemented()
}
