package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// setRuntimePaths points the pinned runtime dirs at temp dirs and restores
// them at cleanup (the vars are test seams, see common.go).
func setRuntimePaths(t *testing.T) {
	t.Helper()
	old := struct {
		reposRoot, spoolDir, hooksDir, gitHome string
		socketPath                             string
	}{
		reposRoot, spoolDir, hooksDir, gitHome, socketPath,
	}
	reposRoot = t.TempDir()
	spoolDir = t.TempDir()
	hooksDir = t.TempDir()
	gitHome = t.TempDir()
	socketPath = filepath.Join(t.TempDir(), "gitd.sock")
	t.Cleanup(func() {
		reposRoot, spoolDir, hooksDir, gitHome, socketPath = old.reposRoot, old.spoolDir, old.hooksDir, old.gitHome, old.socketPath
	})
}

// cliBareRepoWithCommit creates a bare sha256 repo with one commit at
// reposRoot/name.git using the real git, and returns its path.
func cliBareRepoWithCommit(t *testing.T, reposRoot, name string) string {
	t.Helper()
	if _, err := exec.LookPath(cliGitBin); err != nil {
		t.Skip("git not available")
	}
	root := t.TempDir()
	work := filepath.Join(root, "work")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	run := func(dir string, args ...string) {
		cmd := exec.Command(cliGitBin, args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	run(work, "init", "-q", "-b", "main", "--object-format=sha256", ".")
	run(work, "config", "user.email", "t@t")
	run(work, "config", "user.name", "T")
	if err := os.WriteFile(filepath.Join(work, "a"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(work, "add", "a")
	run(work, "commit", "-qm", "first")
	bare := filepath.Join(reposRoot, name+".git")
	run(root, "clone", "-q", "--bare", work, bare)
	return bare
}

// fakeSocketWithRecords starts a unix-socket server that records bundle
// requests and delivers, replying success.
func fakeSocketWithRecords(t *testing.T, path string) (*[]string, *[]string) {
	t.Helper()
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	var bundles, delivers []string
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/bundle":
			var req struct {
				Repo string `json:"repo"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, "bad", http.StatusBadRequest)
				return
			}
			bundles = append(bundles, req.Repo)
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"uploaded":true}`)
		case "/v1/deliver":
			var req struct {
				PluginID string `json:"plugin-id"`
				EventID  string `json:"event-id"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, "bad", http.StatusBadRequest)
				return
			}
			delivers = append(delivers, req.PluginID+"/"+req.EventID)
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	})
	go http.Serve(ln, handler)
	t.Cleanup(func() { ln.Close() })
	return &bundles, &delivers
}

func TestRunNotifyEndToEnd(t *testing.T) {
	if _, err := exec.LookPath(cliGitBin); err != nil {
		t.Skip("git not available")
	}
	setRuntimePaths(t)
	repos, _ := fakeSocketWithRecords(t, socketPath)
	repoDir := cliBareRepoWithCommit(t, reposRoot, "r")
	if err := os.MkdirAll(filepath.Join(repoDir, "hooks"), 0o755); err != nil {
		t.Fatal(err)
	}

	cfgPath := filepath.Join(t.TempDir(), "gitd.yaml")
	os.WriteFile(cfgPath, []byte("git_binary: "+cliGitBin+"\n"), 0o600)
	os.WriteFile(webhooksPathFor(cfgPath), []byte("plugins:\n  - id: p1\n    type: logger\n    sync: false\n"), 0o600)

	t.Chdir(repoDir)
	var stdout, stderr bytes.Buffer
	// A valid post-receive line: old-zero -> new-commit on refs/heads/main.
	cmd := exec.Command(cliGitBin, "rev-parse", "HEAD")
	cmd.Dir = repoDir
	newSHA, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	z := strings.Repeat("0", 64)
	stdin := fmt.Sprintf("%s %s refs/heads/main\n", z, strings.TrimSpace(string(newSHA)))
	code := runNotifyWithStdin(&stdout, &stderr, cfgPath, stdin)
	if code != ExitOK {
		t.Fatalf("runNotify exit = %d, stderr = %s", code, stderr.String())
	}
	if len(*repos) != 1 || (*repos)[0] != "r" {
		t.Errorf("bundle uploads = %v, want [r] (R11-Q1: one per push)", *repos)
	}
	// One spool event written for the single ref line.
	entries, err := os.ReadDir(spoolDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("spool events = %d, want 1", len(entries))
	}
}

// runNotifyWithStdin invokes runNotify with the given stdin content. The
// entry reads os.Stdin, so we temporarily swap it.
func runNotifyWithStdin(stdout, stderr *bytes.Buffer, cfgPath, stdin string) int {
	old := os.Stdin
	r, w, err := os.Pipe()
	if err != nil {
		return ExitError
	}
	os.Stdin = r
	_, _ = w.WriteString(stdin)
	_ = w.Close()
	defer func() { os.Stdin = old }()
	return Run([]string{"notify", "--config", cfgPath}, stdout, stderr)
}

func TestRunNotifyServeDownFails(t *testing.T) {
	if _, err := exec.LookPath(cliGitBin); err != nil {
		t.Skip("git not available")
	}
	setRuntimePaths(t)
	repoDir := cliBareRepoWithCommit(t, reposRoot, "r")
	if err := os.MkdirAll(filepath.Join(repoDir, "hooks"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(t.TempDir(), "gitd.yaml")
	os.WriteFile(cfgPath, []byte("git_binary: "+cliGitBin+"\n"), 0o600)
	os.WriteFile(webhooksPathFor(cfgPath), []byte("plugins: []\n"), 0o600)

	t.Chdir(repoDir)
	var stdout, stderr bytes.Buffer
	code := runNotifyWithStdin(&stdout, &stderr, cfgPath, fmt.Sprintf("%s %s refs/heads/main\n", strings.Repeat("0", 64), strings.Repeat("a", 64)))
	// Serve down => bundle upload fails => notify exits non-zero (R12-Q2).
	if code == ExitOK {
		t.Error("runNotify = ok with serve down, want failure (R5-Q2)")
	}
}

func TestRunNotifyUsage(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"notify", "extra"}, &stdout, &stderr); code != ExitUsage {
		t.Errorf("notify extra arg exit = %d, want %d", code, ExitUsage)
	}
}
