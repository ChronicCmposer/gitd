package disk

import (
	"bytes"
	"errors"
	"log/slog"
	"os"
	"strings"
	"testing"
)

func TestFreeBytes(t *testing.T) {
	free, err := FreeBytes(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if free == 0 {
		t.Error("FreeBytes = 0 on a real filesystem")
	}
}

func TestFreeBytesMissingPath(t *testing.T) {
	if _, err := FreeBytes("/nonexistent-gitd-test"); err == nil {
		t.Fatal("FreeBytes = nil error for a missing path")
	}
}

func TestHeadroomCheck(t *testing.T) {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	dir := t.TempDir()

	// Below MinFree: reject with ErrLowSpace (R3-Q7).
	h := Headroom{MinFree: 1 << 62, WarnFree: 1 << 62}
	if err := h.Check(dir, log); !errors.Is(err, ErrLowSpace) {
		t.Errorf("Check(low) = %v, want ErrLowSpace", err)
	}

	// Plenty of space: pass.
	h = Headroom{MinFree: 1, WarnFree: 1}
	if err := h.Check(dir, log); err != nil {
		t.Errorf("Check(ok) = %v", err)
	}
}

func TestHeadroomCheckWarns(t *testing.T) {
	// Free space lies between MinFree and WarnFree: pass the check but log a
	// warning (R3-Q7 warn at 1GiB free).
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	dir := t.TempDir()
	h := Headroom{MinFree: 1, WarnFree: 1 << 62}
	if err := h.Check(dir, log); err != nil {
		t.Errorf("Check(warn band) = %v", err)
	}
	if !strings.Contains(buf.String(), "disk headroom low") {
		t.Errorf("warning not logged: %q", buf.String())
	}
}

func TestHeadroomCheckStatfsError(t *testing.T) {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	h := Headroom{MinFree: 1, WarnFree: 1}
	if err := h.Check("/nonexistent-gitd-test", log); err == nil {
		t.Fatal("Check = nil error for a missing path")
	}
}
