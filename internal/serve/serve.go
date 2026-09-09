// Package serve owns the serve-side actions channel (R9-Q11): a buffered chan
// with a single worker running all serve work (bundle uploads, deliveries,
// sweeps, verifies) plus the unix-socket HTTP server. Logic lands in Phase
// 3.4+.
package serve
