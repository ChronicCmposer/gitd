package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"

	"github.com/ChronicCmposer/gitd/internal/config"
	"github.com/ChronicCmposer/gitd/internal/socket"
	"github.com/ChronicCmposer/gitd/internal/spool"
)

// runSpool inspects, replays, and purges the webhook event spool (3.5):
// list emits NDJSON of full events (envelope + internal state block, R13-Q6),
// replay <id> re-delivers via POST /v1/deliver over the socket (R12-Q2), and
// purge removes delivered events past their retention TTL (R6-Q1). Bare
// `gitd spool` and `gitd spool help` print the subcommand reference.
func runSpool(args []string, stdout, _ io.Writer) error {
	cfg, rest, err := parseConfigFlag(args)
	if err != nil {
		return err
	}
	// Bare `gitd spool` is a usage error: surface the subcommand reference so
	// list/replay/purge are discoverable (fail-fast, exit 2 on usage).
	if len(rest) == 0 {
		return errUsage("%s", spoolUsageText())
	}
	sub := rest[0]
	subArgs := rest[1:]
	// `gitd spool help` is an explicit help request: print to stdout, exit 0.
	if sub == "help" {
		if len(subArgs) != 0 {
			return errUsage("usage: gitd spool help")
		}
		spoolUsage(stdout)
		return nil
	}
	// Validate usage before touching config (fail-fast, exit 2 on usage).
	switch sub {
	case "list", "purge":
		if len(subArgs) > 0 {
			return errUsage("usage: gitd spool %s (no arguments)", sub)
		}
	case "replay":
		if len(subArgs) != 1 {
			return errUsage("usage: gitd spool replay <event-id>")
		}
	default:
		return errUsage("unknown spool subcommand %q (list|replay|purge|help)", sub)
	}

	gitd, err := config.LoadGitd(cfg)
	if err != nil {
		return err
	}
	log, err := gitd.NewLogger(os.Stderr)
	if err != nil {
		return err
	}
	store := spool.NewStore(spoolDir, time.Now, gitd.Spool.Retention.D(), log)

	switch sub {
	case "list":
		return spoolList(store, stdout)
	case "replay":
		return spoolReplay(cfg, store, subArgs[0], socket.NewClient(socketPath, socketTimeout), log)
	case "purge":
		n, err := store.Purge()
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(stdout, "purged %d events\n", n)
		return err
	}
	return nil
}

// spoolUsageText renders the gitd spool subcommand reference: list emits the
// queued webhook events as NDJSON (envelope + internal state block), replay
// re-delivers one event to every configured webhook plugin via the serve
// socket, and purge removes delivered events past their retention TTL.
func spoolUsageText() string {
	return `usage: gitd spool <command> [args]

commands:
  list                emit the queued webhook events as NDJSON (one object per
                      event: envelope + internal state block). Default
                      destination is the configured spool dir (/var/spool/gitd)
  replay <event-id>   re-deliver one event to every configured webhook plugin
                      synchronously via the serve socket (POST /v1/deliver);
                      fails loudly if serve is down
  purge               remove events already delivered past the spool retention
                      TTL (spool.retention)
  help                print this reference

exit codes: 0 ok, 1 runtime error, 2 usage error
`
}

// spoolUsage writes the gitd spool subcommand reference to w.
func spoolUsage(w io.Writer) {
	_, _ = io.WriteString(w, spoolUsageText())
}

// spoolList emits one NDJSON object per record (R13-Q6).
func spoolList(store *spool.Store, stdout io.Writer) error {
	recs, err := store.List()
	if err != nil {
		return err
	}
	enc := json.NewEncoder(stdout)
	for i := range recs {
		if err := enc.Encode(&recs[i]); err != nil {
			return fmt.Errorf("spool list: encode: %w", err)
		}
	}
	return nil
}

// spoolReplay delivers id synchronously to every configured plugin (R7-Q8):
// one socket round-trip per plugin; the serve side marks delivered/dead in
// Phase 4. Serve down fails loudly (R12-Q2).
func spoolReplay(gitdPath string, store *spool.Store, id string, client *socket.Client, log *slog.Logger) error {
	if _, err := store.Read(id); err != nil {
		return err
	}
	webhooks, err := config.LoadWebhooks(webhooksPathFor(gitdPath))
	if err != nil {
		return err
	}
	if len(webhooks.Plugins) == 0 {
		log.Warn("no plugins configured; nothing to replay to", "event-id", id)
		return nil
	}
	for _, p := range webhooks.Plugins {
		if err := client.Deliver(context.Background(), p.ID, id); err != nil {
			return fmt.Errorf("spool replay %s: %w", id, err)
		}
		log.Info("spool replay delivered", "event-id", id, "plugin", p.ID)
	}
	return nil
}
