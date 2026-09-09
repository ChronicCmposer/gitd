// Package spool is the durable webhook event store: one JSON file per event,
// temp-file + atomic rename + fsync, retry budget, TTL sweep, and replay.
// Logic lands in Phase 3.5.
package spool
