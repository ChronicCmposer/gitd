// Package config implements the strict YAML config protocol from R1-Q3:
// kebab-case keys, DefaultConfig() layering, DisallowUnknownFields, duration
// strings, and fail-fast validate() with the file path in every error.
// Logic lands in Phase 3.6.
package config
