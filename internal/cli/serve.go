package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ChronicCmposer/gitd/internal/config"
	"github.com/ChronicCmposer/gitd/internal/disk"
	"github.com/ChronicCmposer/gitd/internal/gitenv"
	"github.com/ChronicCmposer/gitd/internal/mirror"
	"github.com/ChronicCmposer/gitd/internal/objectstore/s3"
	"github.com/ChronicCmposer/gitd/internal/serve"
	"github.com/ChronicCmposer/gitd/internal/socket"
	"github.com/ChronicCmposer/gitd/internal/spool"
	"github.com/ChronicCmposer/gitd/internal/sshcmd"
)

// runServe runs the git gateway service (3.1-3.7). One binary, two roles
// (R10-Q1, R12-Q5):
//
//   - As the sshd ForceCommand for the git user (SSH_CONNECTION present) it
//     runs the sshcmd gateway: greeting, git-upload-pack/git-receive-pack.
//   - As the gitd-serve systemd unit (no SSH_CONNECTION) it runs the
//     actions-channel server: unix-socket /v1/bundle + /v1/deliver, spool
//     sweep, weekly mirror verify, startup catch-up, SIGTERM drain.
func runServe(args []string, _, _ io.Writer) error {
	cfg, rest, err := parseConfigFlag(args)
	if err != nil {
		return err
	}
	if len(rest) > 0 {
		return errUsage("serve takes no arguments")
	}

	gitd, err := config.LoadGitd(cfg)
	if err != nil {
		return err
	}
	log, err := gitd.NewLogger(os.Stderr)
	if err != nil {
		return err
	}
	git := gitenv.NewRunner(gitd.GitBinary, gitHome, os.Getenv("PATH"))

	if os.Getenv("SSH_CONNECTION") != "" {
		return sshcmd.Serve(os.Environ(), os.Stdin, os.Stdout, os.Stderr, sshcmd.GatewayConfig{
			ReposRoot: reposRoot,
			HooksDir:  hooksDir,
			Git:       git,
			Headroom:  disk.Headroom{MinFree: gitd.DiskMinFreeBytes, WarnFree: 1 << 30},
			Log:       log,
		})
	}

	// Daemon mode.
	rt, err := config.Load(cfg, webhooksPathFor(cfg), log)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	// SIGHUP reloads the webhooks.yaml + gitd.yaml spool + policies subset
	// (R8-Q6 fail-safe: invalid config keeps the old one and audit-logs).
	sighup := make(chan os.Signal, 1)
	signal.Notify(sighup, syscall.SIGHUP)
	go func() {
		for range sighup {
			if err := rt.Reload(); err != nil {
				log.Error("SIGHUP reload failed; keeping previous config", "error", err)
			}
		}
	}()

	store, err := s3.New(ctx, gitd.Storage.Bucket, gitd.Storage.Region)
	if err != nil {
		return fmt.Errorf("serve: storage: %w", err)
	}
	spoolStore := spool.NewStore(spoolDir, time.Now, gitd.Spool.Retention.D(), log)
	m := mirror.New(store, git, reposRoot, gitd.Storage.Prefix, spoolDir, time.Now, log)

	srv := serve.New(serve.Config{
		Mirror:            m,
		Spool:             spoolStore,
		Webhooks:          rt.Webhooks,
		Deliver:           deliverSeam,
		ReposRoot:         reposRoot,
		SocketPath:        socket.DefaultPath,
		Now:               time.Now,
		Log:               log,
		SweepInterval:     time.Minute,
		VerifyInterval:    gitd.Mirror.VerifyInterval.D(),
		ActionsBufferSize: int(gitd.Serve.ActionsBufferSize),
	})
	return srv.Run(ctx)
}

// deliverSeam is the Phase 3 placeholder for the webhook delivery engine
// (Phase 4.2 wires the plugin pipeline + durable retry bookkeeping here).
func deliverSeam(_ context.Context, pluginID, eventID string) error {
	return fmt.Errorf("webhook delivery engine lands in Phase 4 (plugin %s, event %s)", pluginID, eventID)
}
