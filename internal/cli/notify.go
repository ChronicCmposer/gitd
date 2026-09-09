package cli

import "io"

// runNotify handles a post-receive notification (Phase 3+): reads the ref
// lines on stdin, writes one spool event per ref, and submits the bundle
// upload to serve over the unix socket.
func runNotify(_ []string, _, _ io.Writer) error {
	return errNotImplemented()
}
