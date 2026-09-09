package cli

import "io"

// runMirror inspects and manages S3 bundle mirrors (Phase 3+): list <repo>
// emits NDJSON of bundles, delete <repo> removes them, and fetch <repo> <dest>
// restores a repo from its latest bundle.
func runMirror(_ []string, _, _ io.Writer) error {
	return errNotImplemented()
}
