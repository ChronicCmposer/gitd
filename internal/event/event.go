// Package event defines the webhook event envelope (R10-Q7, R11-Q4): the
// kebab-case JSON schema shared by spool files and webhook payloads, with
// strict decode (unknown fields = error, newer schema-version = refuse).
// Logic lands in Phase 3.5.
package event
