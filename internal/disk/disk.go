// Package disk guards disk headroom for pushes (R3-Q7, R7-Q4). The same
// statfs check runs in both the ssh gateway (fast UX before receive-pack) and
// the pre-receive hook path (the true objects-received chokepoint).
package disk

import (
	"errors"
	"fmt"
	"log/slog"
	"syscall"
)

// FreeBytes returns the free bytes on the filesystem containing path.
func FreeBytes(path string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, fmt.Errorf("statfs %s: %w", path, err)
	}
	return st.Bavail * uint64(st.Bsize), nil
}

// Headroom holds the reject/warn thresholds for a filesystem. MinFree is
// disk_min_free_bytes (default 512MiB, R3-Q7); WarnFree is the warn-at level
// (1GiB free, R3-Q7).
type Headroom struct {
	MinFree  uint64
	WarnFree uint64
}

// ErrLowSpace is returned by Check when free space is below MinFree: the push
// is rejected with a clear message on the git client's stderr.
var ErrLowSpace = errors.New("not enough disk space")

// Check verifies headroom on the filesystem containing path. It returns
// ErrLowSpace (wrapped) when free space falls below MinFree and logs a warning
// when it falls below WarnFree.
func (h Headroom) Check(path string, log *slog.Logger) error {
	free, err := FreeBytes(path)
	if err != nil {
		return err
	}
	if free < h.MinFree {
		return fmt.Errorf("%w: need %d bytes free, have %d on %s", ErrLowSpace, h.MinFree, free, path)
	}
	if free < h.WarnFree {
		log.Warn("disk headroom low", "path", path, "free_bytes", free, "warn_free_bytes", h.WarnFree)
	}
	return nil
}
