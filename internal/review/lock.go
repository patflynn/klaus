package review

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// ErrLocked: another klaus review --post holds this PR's lock.
var ErrLocked = errors.New("another klaus review --post is running for this PR")

// lockPR takes a non-blocking exclusive flock on dir/review-<owner>-<repo>-<n>.lock; the returned func releases it.
func lockPR(dir, repo, prNumber string) (func(), error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	name := strings.NewReplacer("/", "-", string(filepath.Separator), "-").Replace("review-" + repo + "-" + prNumber + ".lock")
	path := filepath.Join(dir, name)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("%w (lock %s)", ErrLocked, path)
		}
		return nil, fmt.Errorf("locking %s: %w", path, err)
	}
	return func() { f.Close() }, nil // close drops the flock
}
