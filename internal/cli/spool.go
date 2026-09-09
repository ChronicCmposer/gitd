package cli

import "io"

// runSpool inspects, replays, and purges the webhook event spool (Phase 3+):
// list emits NDJSON, replay <id> re-delivers through the plugin pipeline, and
// purge removes delivered events past their retention TTL.
func runSpool(_ []string, _, _ io.Writer) error {
	return errNotImplemented()
}
