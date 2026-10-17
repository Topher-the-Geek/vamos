package cache

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// ErrLocked means that another process holds the directory's lock in a way that excludes the operation:
// a prune while the directory is in use, or a use while a prune runs.
var ErrLocked = errors.New("the build cache is locked")

// lockName is the file in a Directory that its advisory lock is held on.
const lockName = "lock"

// dirLock is the advisory lock of a Directory: one host's kernel lock on a file in it, shared by every open handle
// that uses the directory and exclusive for a prune, so that the two never overlap, across processes as within one.
// It survives nothing and a shared network store wouldn't honour it; it is not a lease.
type dirLock struct {
	f *os.File
}

// lockShared opens the lock file in dir and takes it shared, without waiting:
// a prune holding it exclusive makes the open fail with ErrLocked.
func lockShared(dir string) (*dirLock, error) {
	f, err := os.OpenFile(filepath.Join(dir, lockName), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := flock(f, unix.LOCK_SH); err != nil {
		f.Close()
		return nil, fmt.Errorf("%w: another vamos is pruning the build cache at %s; try again when it finishes",
			ErrLocked, dir)
	}
	return &dirLock{f: f}, nil
}

// flock takes the lock on f as how says, LOCK_SH or LOCK_EX, without waiting, reporting ErrLocked if it is held.
func flock(f *os.File, how int) error {
	err := unix.Flock(int(f.Fd()), how|unix.LOCK_NB)
	if errors.Is(err, unix.EWOULDBLOCK) {
		return ErrLocked
	}
	return err
}

// exclusive converts the lock to exclusive, for a prune, without waiting, and returns what converts it back.
// The conversion isn't atomic: the shared lock is given up first, so a use that arrives meanwhile may take it,
// and the conversion then fails with ErrLocked, after taking the shared lock again.
// If a prune elsewhere took the lock in between, so that not even the shared lock can be taken back,
// the handle is left holding nothing, and the error says so; its owner, a prune command, exits on it.
func (l *dirLock) exclusive(dir string) (func() error, error) {
	if err := flock(l.f, unix.LOCK_EX); err != nil {
		if errors.Is(err, ErrLocked) {
			if again := flock(l.f, unix.LOCK_SH); again != nil {
				return nil, fmt.Errorf("taking the build cache's lock back after a failed prune: %w; "+
					"the build cache can't be used until this process ends", again)
			}
			return nil, fmt.Errorf("%w: another vamos is using the build cache at %s; prune when it finishes",
				ErrLocked, dir)
		}
		return nil, err
	}
	return func() error { return flock(l.f, unix.LOCK_SH) }, nil
}

// release gives the lock up and closes its file.
func (l *dirLock) release() error {
	if l == nil || l.f == nil {
		return nil
	}
	err := l.f.Close() // closing releases the lock
	l.f = nil
	return err
}

// HoldExclusive holds the directory's lock exclusive, as a prune does, while hold runs, and takes it back shared.
// It is for tests of what a prune excludes.
func HoldExclusive(d *Directory, hold func()) error {
	shared, err := d.lock.exclusive(d.dir)
	if err != nil {
		return err
	}
	hold()
	return shared()
}
