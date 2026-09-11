package sshcmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/ChronicCmposer/gitd/internal/disk"
	"github.com/ChronicCmposer/gitd/internal/gitenv"
	"github.com/ChronicCmposer/gitd/internal/repo"
)

// Allowed git commands, dash+space forms only (R2-Q1).
const (
	opUploadPack  = "git-upload-pack"
	opReceivePack = "git-receive-pack"
)

// GatewayConfig carries the runtime inputs for the ForceCommand gateway.
type GatewayConfig struct {
	ReposRoot string // where bare repos live (/srv/git, R10-Q4)
	HooksDir  string // hook shims directory, verified on push-to-create (R6-Q2)
	Git       *gitenv.Runner
	Headroom  disk.Headroom
	Log       *slog.Logger
}

// Serve runs the ForceCommand handler for one sshd invocation (3.1): reads
// SSH_ORIGINAL_COMMAND, prints the two-line greeting for empty commands
// (env-only, R12-Q8), and argv-execs git-upload-pack / git-receive-pack for
// valid commands. Git commands fail closed without the patched-sshd identity
// env (R10-Q5).
func Serve(env []string, stdin io.Reader, stdout, stderr io.Writer, cfg GatewayConfig) error {
	ident := IdentityFromEnv(env)
	cfg.Log.Info("session start",
		"key_fp", ident.KeyFP, "key_type", ident.KeyType, "cert_id", ident.CertID, "client_ip", ident.ClientIP)

	raw := envGet(env, "SSH_ORIGINAL_COMMAND")
	if strings.TrimSpace(raw) == "" {
		if _, err := io.WriteString(stdout, Greeting(ident)); err != nil {
			return fmt.Errorf("write greeting: %w", err)
		}
		return nil
	}

	args, err := ParseCommand(raw)
	if err != nil {
		cfg.Log.Warn("command rejected", "reason", err)
		return err
	}
	if len(args) == 0 {
		if _, err := io.WriteString(stdout, Greeting(ident)); err != nil {
			return fmt.Errorf("write greeting: %w", err)
		}
		return nil
	}

	op := args[0]
	if op != opUploadPack && op != opReceivePack {
		err := fmt.Errorf("command rejected: only %s and %s are allowed, got %q", opUploadPack, opReceivePack, op)
		cfg.Log.Warn("command rejected", "command", op, "reason", err)
		return err
	}
	if len(args) != 2 {
		return fmt.Errorf("command rejected: %s takes exactly one repository argument", op)
	}

	name, err := RepoNameFromArg(args[1])
	if err != nil {
		cfg.Log.Warn("command rejected", "command", op, "repo_arg", args[1], "reason", err)
		return err
	}

	if !ident.Complete() {
		err := fmt.Errorf("command rejected: missing pusher identity (SSH_AUTH_KEY_FP/SSH_AUTH_CERT_ID); is sshd patched?")
		cfg.Log.Error("command rejected", "command", op, "repo", name, "reason", err)
		return err
	}

	repoDir := filepath.Join(cfg.ReposRoot, name+".git")
	if op == opReceivePack {
		if err := cfg.Headroom.Check(cfg.ReposRoot, cfg.Log); err != nil {
			return fmt.Errorf("push rejected: %w", err)
		}
		if err := pushToCreate(cfg, name, repoDir); err != nil {
			return err
		}
	}

	cfg.Log.Info("command accepted", "command", op, "repo", name)
	cmd := cfg.Git.Command(context.Background(), op, repoDir)
	cmd.Stdin = stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s %s: %w", op, repoDir, err)
	}
	return nil
}

// RepoNameFromArg extracts the repo name from a client-supplied path: one
// optional trailing ".git" is stripped (R10-Q4), then the allowlist applies
// (R2-Q1).
func RepoNameFromArg(arg string) (string, error) {
	base := filepath.Base(filepath.Clean(arg))
	name := repo.Normalize(base)
	if !repo.ValidName(name) {
		return "", fmt.Errorf("invalid repository name %q", base)
	}
	return name, nil
}

// pushToCreate creates the bare repo when receive-pack targets a missing
// repo (R10-Q4). os.Mkdir is the atomic race check (R2-Q10): a successful
// mkdir means we own creation; EEXIST means the repo appeared concurrently
// (or pre-exists) and is used as-is after a bare-repo sanity check.
func pushToCreate(cfg GatewayConfig, name, repoDir string) error {
	err := os.Mkdir(repoDir, 0o755)
	switch {
	case err == nil:
		// The object format of new repos comes from GIT_DEFAULT_HASH (pinned
		// from config.object_format on the Runner; sha1 default), not a
		// hardcoded flag, so the operator can flip it via config.
		if _, err := cfg.Git.Run(context.Background(), "init", "--bare", repoDir); err != nil {
			return fmt.Errorf("push-to-create %s: init: %w", name, err)
		}
		if err := writeDescription(repoDir, name); err != nil {
			return fmt.Errorf("push-to-create %s: %w", name, err)
		}
		if err := verifyHooks(cfg.HooksDir); err != nil {
			return fmt.Errorf("push-to-create %s: %w", name, err)
		}
		cfg.Log.Info("repository created", "repo", name, "dir", repoDir)
		return nil
	case errors.Is(err, os.ErrExist):
		ok, statErr := repo.IsBareRepo(repoDir)
		if statErr != nil {
			return fmt.Errorf("push-to-create %s: %w", name, statErr)
		}
		if !ok {
			return fmt.Errorf("push-to-create %s: %s exists but is not a bare repository", name, repoDir)
		}
		return nil
	default:
		return fmt.Errorf("push-to-create %s: mkdir: %w", name, err)
	}
}

// writeDescription writes the default bare-repo description file (R10-Q4).
func writeDescription(repoDir, name string) error {
	return os.WriteFile(filepath.Join(repoDir, "description"), []byte(name+" repository on git.cmposer.cc\n"), 0o644)
}

// verifyHooks ensures the hook shim directory is present on push-to-create
// (R6-Q2): a repo created without hooks would silently skip notify/mirror.
func verifyHooks(hooksDir string) error {
	if hooksDir == "" {
		return nil
	}
	st, err := os.Stat(hooksDir)
	if err != nil {
		return fmt.Errorf("hook shims missing at %s: %w", hooksDir, err)
	}
	if !st.IsDir() {
		return fmt.Errorf("hook shims path %s is not a directory", hooksDir)
	}
	return nil
}
