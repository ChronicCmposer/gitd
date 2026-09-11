package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/ChronicCmposer/gitd/internal/config"
	"github.com/ChronicCmposer/gitd/internal/gitenv"
	"github.com/ChronicCmposer/gitd/internal/mirror"
	"github.com/ChronicCmposer/gitd/internal/objectstore/s3"
)

// runMirrorAgent runs the git-context restore agent daemon — the
// gitd-restore container's entrypoint. It is a background role, not an admin
// data-plane op: it scan-then-watches /var/spool/gitd/restore for restore
// jobs staged by gitd-serve, re-verifies the staged bundle, and writes
// /srv/git/<repo>.git as the git user (the container runs with no elevated
// caps and /srv/git rw). SIGTERM/SIGINT stop the watch loop cleanly.
func runMirrorAgent(args []string, _, _ io.Writer) error {
	cfg, rest, err := parseConfigFlag(args)
	if err != nil {
		return err
	}
	if len(rest) > 0 {
		return errUsage("mirror-agent takes no arguments")
	}
	gitd, err := config.LoadGitd(cfg)
	if err != nil {
		return err
	}
	log, err := gitd.NewLogger(os.Stderr)
	if err != nil {
		return err
	}
	store, err := s3.New(context.Background(), gitd.Storage.Bucket, gitd.Storage.Region)
	if err != nil {
		return fmt.Errorf("mirror-agent: storage: %w", err)
	}
	git := gitenv.NewRunner(gitd.GitBinary, gitHome, os.Getenv("PATH"))
	m := mirror.New(store, git, reposRoot, gitd.Storage.Prefix, spoolDir, time.Now, log)
	agent := mirror.NewAgent(mirror.AgentConfig{
		Mirror:    m,
		WorkDir:   filepath.Join(spoolDir, "restore"),
		ReposRoot: reposRoot,
		Log:       log,
	})
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	return agent.Run(ctx)
}
