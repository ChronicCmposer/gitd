// Package version holds the gitd version string.
//
// Version is a link-time variable: the release pipeline overrides it via Bazel
// stamping (x_defs, R1-Q11). Until then it reports the v0.0.0-devel default.
package version

// Version is the gitd version string. It is overridden at link time by the
// release pipeline; do not edit it by hand.
var Version = "v0.0.0-devel"

// String returns the current gitd version string.
func String() string {
	return Version
}
