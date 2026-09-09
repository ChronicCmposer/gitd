// Package webhook delivers spool events to configured plugins over HTTP with
// HMAC-SHA256 signatures, TLS verification on by default, and a 3-attempt
// backoff budget. Plugin implementations live in webhook/plugins. Logic lands
// in Phase 3.5+.
package webhook
