package cli

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/ChronicCmposer/gitd/internal/config"
)

// ddnsEndpoint is the Namecheap dynamic DNS update endpoint; a var so tests
// can point it at a local server.
var ddnsEndpoint = "https://dynamicdns.park-your-domain.com/update"

var (
	ddnsInterfaceRe = regexp.MustCompile(`(?is)<interface-response>`)
	ddnsErrCountRe  = regexp.MustCompile(`(?is)<ErrCount>\s*(\d+)\s*</ErrCount>`)
)

// ddnsSuccess reports whether a Namecheap dynamic-DNS reply indicates success
// and returns a short, log-friendly summary. The dynamicdns endpoint answers
// HTTP 200 for BOTH outcomes, so the body is authoritative. Two formats exist:
//   - classic plain-text: "Good <ip>" (updated) or "No change" (IP unchanged);
//   - since ~2021, an <interface-response> XML blob (declared UTF-16 but
//     actually UTF-8; regex-matched rather than XML-decoded to sidestep the
//     misdeclared charset) whose success signal is <ErrCount>0</ErrCount> with
//     an empty <errors/>.
func ddnsSuccess(reply string) (bool, string) {
	if strings.HasPrefix(reply, "Good") || strings.HasPrefix(reply, "OK") || strings.HasPrefix(reply, "No change") {
		return true, reply
	}
	if !ddnsInterfaceRe.MatchString(reply) {
		return false, reply
	}
	if m := ddnsErrCountRe.FindStringSubmatch(reply); len(m) == 2 && m[1] == "0" {
		return true, "interface-response ErrCount=0 (record updated)"
	}
	return false, reply
}

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
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("ddns: read response: %w", err)
	}
	reply := strings.ReplaceAll(strings.TrimSpace(string(body)), "\x00", "")

	if ok, summary := ddnsSuccess(reply); ok {
		log.Info("ddns updated", "host", gitd.DDNS.Host, "domain", gitd.DDNS.Domain, "reply", summary)
		return nil
	}
	return fmt.Errorf("ddns: update failed (HTTP %d): %s", resp.StatusCode, reply)
}
