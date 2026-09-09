package cli

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/ChronicCmposer/gitd/internal/config"
)

// ddnsEndpoint is the Namecheap dynamic DNS update endpoint; a var so tests
// can point it at a local server.
var ddnsEndpoint = "https://dynamicdns.park-your-domain.com/update"

// runDDNS refreshes the Namecheap dynamic DNS record for git.cmposer.cc. The
// ip param is omitted so Namecheap uses the requester IP (= the EIP). The
// password comes from a file-path reference (R1-Q4: never inline, never env).
func runDDNS(args []string, _, _ io.Writer) error {
	cfg, rest, err := parseConfigFlag(args)
	if err != nil {
		return err
	}
	if len(rest) > 0 {
		return errUsage("ddns takes no arguments")
	}
	gitd, err := config.LoadGitd(cfg)
	if err != nil {
		return err
	}
	log, err := gitd.NewLogger(os.Stderr)
	if err != nil {
		return err
	}

	raw, err := os.ReadFile(gitd.DDNS.PasswordFile)
	if err != nil {
		return fmt.Errorf("ddns: read password_file: %w", err)
	}
	password := strings.TrimSpace(string(raw))
	if password == "" {
		return fmt.Errorf("ddns: password_file %s is empty", gitd.DDNS.PasswordFile)
	}

	endpoint := fmt.Sprintf("%s?host=%s&domain=%s&password=%s", ddnsEndpoint,
		url.QueryEscape(gitd.DDNS.Host), url.QueryEscape(gitd.DDNS.Domain), url.QueryEscape(password))
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Get(endpoint)
	if err != nil {
		return fmt.Errorf("ddns: update: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body := make([]byte, 512)
	n, _ := resp.Body.Read(body)
	reply := strings.TrimSpace(string(body[:n]))

	// Namecheap replies "Good <ip>" or "OK" on success, error codes otherwise.
	if strings.HasPrefix(reply, "Good") || strings.HasPrefix(reply, "OK") {
		log.Info("ddns updated", "host", gitd.DDNS.Host, "domain", gitd.DDNS.Domain, "reply", reply)
		return nil
	}
	return fmt.Errorf("ddns: update failed (HTTP %d): %s", resp.StatusCode, reply)
}
