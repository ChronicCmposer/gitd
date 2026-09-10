package cli

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ddnsTestConfig writes a password file and a gitd config pointing at it,
// returning the config path. Mirrors the setup style of TestDDNS.
func ddnsTestConfig(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	pw := filepath.Join(dir, "pw")
	os.WriteFile(pw, []byte("s3cret\n"), 0o600)
	cfgPath := filepath.Join(dir, "gitd.yaml")
	os.WriteFile(cfgPath, []byte("ddns:\n  host: git\n  domain: cmposer.cc\n  password_file: "+pw+"\n"), 0o600)
	return cfgPath
}

// ddnsServe swaps ddnsEndpoint to an httptest server and returns its URL.
func ddnsServe(t *testing.T, h http.Handler) string {
	t.Helper()
	old := ddnsEndpoint
	ddnsEndpoint = ""
	t.Cleanup(func() { ddnsEndpoint = old })
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	ddnsEndpoint = ts.URL
	return ts.URL
}

func TestDDNSXMLSuccess(t *testing.T) {
	ddnsServe(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `<?xml version="1.0"?>
<interface-response>
  <Done>true</Done>
  <IP>3.128.205.205</IP>
  <ErrCount>0</ErrCount>
  <errors/>
</interface-response>`)
	}))
	cfgPath := ddnsTestConfig(t)

	var stdout, stderr bytes.Buffer
	if code := Run([]string{"ddns", "--config", cfgPath}, &stdout, &stderr); code != ExitOK {
		t.Errorf("ddns XML success exit = %d, want %d (stderr: %s)", code, ExitOK, stderr.String())
	}
}

func TestDDNSNoChange(t *testing.T) {
	ddnsServe(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "No change")
	}))
	cfgPath := ddnsTestConfig(t)

	var stdout, stderr bytes.Buffer
	if code := Run([]string{"ddns", "--config", cfgPath}, &stdout, &stderr); code != ExitOK {
		t.Errorf("ddns no-change exit = %d, want %d (stderr: %s)", code, ExitOK, stderr.String())
	}
}

func TestDDNSXMLFailure(t *testing.T) {
	ddnsServe(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `<?xml version="1.0"?>
<interface-response>
  <Done>false</Done>
  <ErrCount>1</ErrCount>
  <errors><Err1>Some error</Err1></errors>
</interface-response>`)
	}))
	cfgPath := ddnsTestConfig(t)

	var stdout, stderr bytes.Buffer
	if code := Run([]string{"ddns", "--config", cfgPath}, &stdout, &stderr); code != ExitError {
		t.Errorf("ddns XML failure exit = %d, want %d (stderr: %s)", code, ExitError, stderr.String())
	}
	if !strings.Contains(stderr.String(), "ddns") {
		t.Errorf("stderr = %q", stderr.String())
	}
}
