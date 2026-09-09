package cli

import "io"

// runDDNS refreshes the Namecheap dynamic DNS record for git.cmposer.cc
// (Phase 4+). The timer unit runs it every 6h; records expire ~30d if left
// unrefreshed.
func runDDNS(_ []string, _, _ io.Writer) error {
	return errNotImplemented()
}
