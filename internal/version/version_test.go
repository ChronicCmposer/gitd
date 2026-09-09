package version

import "testing"

func TestString(t *testing.T) {
	// Version is a link-time variable (R1-Q11); the default is the devel
	// fallback. The test pins the contract: String() returns the current
	// Version value verbatim.
	if got := String(); got != Version {
		t.Fatalf("String() = %q, want %q", got, Version)
	}
	if got := Version; got == "" {
		t.Fatal("Version is empty")
	}
}
