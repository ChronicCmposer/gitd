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
	"github.com/ChronicCmposer/gitd/internal/webhook"
	// Plugin packages self-register their constructors into webhook.Default
	// via init (4.1); the blank imports keep them live in the gitd binary.
	_ "github.com/ChronicCmposer/gitd/internal/webhook/plugins/http"
	_ "github.com/ChronicCmposer/gitd/internal/webhook/plugins/logger"
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
	deliverer := webhook.NewDeliverer(rt.Webhooks, spoolStore, webhook.Default, time.Now, log)

	srv := serve.New(serve.Config{
		Mirror:            m,
		Spool:             spoolStore,
		Webhooks:          rt.Webhooks,
		Deliver:           deliverer.Deliver,
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
