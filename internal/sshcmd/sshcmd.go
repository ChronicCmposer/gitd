// Package sshcmd is the SSH_ORIGINAL_COMMAND gateway (Phase 3.1): a
// hand-written single-quote-aware tokenizer (R2-Q10), repo allowlist
// validation, the two-line authenticated greeting (R12-Q8), and argv exec of
// git-upload-pack / git-receive-pack with push-to-create (R10-Q4) and the
// statfs headroom guard (R3-Q7). Git command sessions fail closed without the
// patched-sshd identity env (R10-Q5).
package sshcmd
