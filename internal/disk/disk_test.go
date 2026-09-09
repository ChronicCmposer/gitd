package disk

import (
	"errors"
	"log/slog"
	"os"
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
