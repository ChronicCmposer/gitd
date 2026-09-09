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
// purge removes delivered events past their retention TTL (R6-Q1).
func runSpool(args []string, stdout, _ io.Writer) error {
	cfg, rest, err := parseConfigFlag(args)
	if err != nil {
		return err
	}
	sub := "list"
	var subArgs []string
	if len(rest) > 0 {
		sub = rest[0]
		subArgs = rest[1:]
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
		return errUsage("unknown spool subcommand %q (list|replay|purge)", sub)
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
		return spoolReplay(cfg, store, subArgs[0], socket.NewClient(socket.DefaultPath, socketTimeout), log)
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
