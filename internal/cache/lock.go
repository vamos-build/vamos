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

// ErrRelock means that a prune left the directory without its shared lock, which it couldn't take back, after a
// refused upgrade or after pruning, so that the handle may no longer be used: its owner ends the process, and the next
// opens the directory again. It wraps the lock's own error without %w, so that it never matches ErrLocked, which says
// a prune was refused and removed nothing.
var ErrRelock = errors.New("the build cache lost its lock")

// lockFlock takes a lock as flock does; a test replaces it to make taking the shared lock back fail.
var lockFlock = flock

// relockError returns err, a failure to take the lock back shared, as ErrRelock.
func relockError(when string, err error) error {
	//nolint:errorlint // the lock's own error may be ErrLocked, which a lost lock must never match
	return fmt.Errorf("%w %s: %v; the build cache can't be used until this process ends", ErrRelock, when, err)
}

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
	if err := lockFlock(l.f, unix.LOCK_EX); err != nil {
		// The conversion gave the shared lock up first, whatever made it fail, so it is taken back.
		if again := lockFlock(l.f, unix.LOCK_SH); again != nil {
			return nil, relockError(fmt.Sprintf("after a failed prune (%v)", err), again)
		}
		if errors.Is(err, ErrLocked) {
			return nil, fmt.Errorf("%w: another vamos is using the build cache at %s; prune when it finishes",
				ErrLocked, dir)
		}
		return nil, err
	}
	return func() error {
		if err := lockFlock(l.f, unix.LOCK_SH); err != nil {
			return relockError("after pruning", err)
		}
		return nil
	}, nil
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

// FailRelock makes every Directory's prune fail to take its lock back shared, as when a prune elsewhere took it in
// between, until the returned function is called. It is for tests of what a prune that loses its lock does, and so
// must not run in parallel with any that prunes.
func FailRelock() func() {
	lockFlock = func(f *os.File, how int) error {
		if how == unix.LOCK_SH {
			return ErrLocked
		}
		return flock(f, how)
	}
	return func() { lockFlock = flock }
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
