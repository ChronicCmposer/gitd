package sshcmd

import (
	"fmt"
	"strings"
)

// Identity is the authenticated pusher identity from the patched sshd
// environment (R2-Q6, R10-Q5): key fingerprint, key type, cert id, and the
// client IP from SSH_CONNECTION.
type Identity struct {
	KeyFP    string // SSH_AUTH_KEY_FP
	KeyType  string // SSH_AUTH_KEY_TYPE
	CertID   string // SSH_AUTH_CERT_ID
	ClientIP string // SSH_CONNECTION first field
}

// IdentityFromEnv reads the patched-sshd identity variables from env.
// Missing variables yield empty fields; the gateway decides whether an empty
// identity is fatal (git commands fail closed, R10-Q5; the greeting does not).
func IdentityFromEnv(env []string) Identity {
	return Identity{
		KeyFP:    envGet(env, "SSH_AUTH_KEY_FP"),
		KeyType:  envGet(env, "SSH_AUTH_KEY_TYPE"),
		CertID:   envGet(env, "SSH_AUTH_CERT_ID"),
		ClientIP: clientIP(envGet(env, "SSH_CONNECTION")),
	}
}

// Complete reports whether the identity carries the fields required for git
// commands (R10-Q5): key fingerprint and cert id must be present.
func (i Identity) Complete() bool { return i.KeyFP != "" && i.CertID != "" }

// clientIP extracts the first field of SSH_CONNECTION ("clientip clientport
// serverip serverport").
func clientIP(connection string) string {
	fields := strings.Fields(connection)
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
}

// envGet looks up key in a key=value environment slice (os.Environ format).
func envGet(env []string, key string) string {
	prefix := key + "="
	for _, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			return strings.TrimPrefix(kv, prefix)
		}
	}
	return ""
}

// Greeting returns the exactly-two-line ssh greeting (R12-Q8): line 1 is the
// authenticated identity (cert id, key fingerprint, client IP); line 2 is the
// no-shell-access + push-to-create hint. It never runs git (env-only).
func Greeting(ident Identity) string {
	certID := ident.CertID
	if certID == "" {
		certID = "unknown identity"
	}
	keyFP := ident.KeyFP
	if keyFP == "" {
		keyFP = "unknown key"
	}
	ip := ident.ClientIP
	if ip == "" {
		ip = "unknown address"
	}
	return fmt.Sprintf("Hello %s (key %s from %s)!\n", certID, keyFP, ip) +
		"You have no shell access. Push to create or update repositories: git push git@git.cmposer.cc:<repo>.git\n"
}
