package cli

import "io"

// runServe runs the git gateway service (Phase 3+): the SSH ForceCommand
// gateway, the unix-socket server (/v1/bundle, /v1/deliver), the spool sweep,
// and the weekly mirror verification.
func runServe(_ []string, _, _ io.Writer) error {
	return errNotImplemented()
}
