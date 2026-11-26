package cache

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/opencontainers/go-digest"
	"golang.org/x/sys/unix"
)

// historyLockWait bounds how long a history add or prune waits for the history lock, the in-process wait included.
const historyLockWait = 10 * time.Second

// historyLockRetry is how often a history add or prune tries the history lock file again while another holds it.
const historyLockRetry = 20 * time.Millisecond

// historyLockName is the file in history/ that the history lock is held on, which no record is named.
const historyLockName = "lock"

// historyPath returns where the history named name, which must be valid, is kept.
func (d *Directory) historyPath(name digest.Digest) string {
	return filepath.Join(d.dir, "history", name.Encoded())
}

// lockHistory takes the history lock, which every add and prune holds and no read takes:
// this handle's channel, then an exclusive flock on history/lock, taken without blocking and tried again until
// historyWait has passed in all, or ctx ends.
// It returns what releases the lock.
func (d *Directory) lockHistory(ctx context.Context) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err // before the select below, which might take the lock as readily
	}
	timeout := time.NewTimer(d.historyWait)
	defer timeout.Stop()
	gaveUp := func() error {
		return fmt.Errorf("%w: the target history at %s was held by another add or prune for %v",
			ErrLocked, filepath.Join(d.dir, "history"), d.historyWait)
	}
	select {
	case d.history <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-timeout.C:
		return nil, gaveUp()
	}
	f, err := os.OpenFile(filepath.Join(d.dir, "history", historyLockName), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		<-d.history
		return nil, err
	}
	release := func() {
		f.Close() // which releases the flock
		<-d.history
	}
	retry := time.NewTicker(historyLockRetry)
	defer retry.Stop()
	for {
		err := flock(f, unix.LOCK_EX)
		if err == nil {
			return release, nil
		}
		if !errors.Is(err, ErrLocked) {
			release()
			return nil, err
		}
		select {
		case <-retry.C:
		case <-ctx.Done():
			release()
			return nil, ctx.Err()
		case <-timeout.C:
			release()
			return nil, gaveUp()
		}
	}
}

// History returns the history named name, or nil if there is none that can be read.
func (d *Directory) History(_ context.Context, name digest.Digest) (*History, error) {
	if err := validateKey(name); err != nil {
		return nil, err
	}
	var h History
	ok, err := readRecordWithin(d.historyPath(name), MaxHistorySize, &h)
	if !ok || err != nil {
		return nil, err
	}
	return &h, nil
}

// Newest returns the leading fields of the history named name, reading no further into its file.
func (d *Directory) Newest(_ context.Context, name digest.Digest) (HistoryHead, error) {
	if err := validateKey(name); err != nil {
		return HistoryHead{}, err
	}
	return readHistoryHead(d.historyPath(name))
}

// readHistoryHead returns the leading fields of the history at path, or a zero head if there is none that can be
// read.
func readHistoryHead(path string) (HistoryHead, error) {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return HistoryHead{}, nil
	} else if err != nil {
		return HistoryHead{}, err
	}
	defer f.Close()
	head, _ := decodeHistoryHead(limited(f, MaxHistorySize))
	return head, nil
}

// AddHistory adds entry to the history named name, under the history lock, and publishes it by a rename,
// so that reads, which take no lock, see it whole.
func (d *Directory) AddHistory(ctx context.Context, name digest.Digest, entry HistoryEntry,
	trees map[digest.Digest][]HistoryFile, keep int) error {
	if err := validateAdd(name, entry, trees); err != nil {
		return err
	}
	unlock, err := d.lockHistory(ctx)
	if err != nil {
		return fmt.Errorf("adding to target history: %w", err)
	}
	defer unlock()
	h, err := d.History(ctx, name)
	if err != nil {
		return err
	}
	if h == nil {
		h = &History{}
	}
	h.add(entry, trees, keep, MaxHistorySize, time.Now())
	data, err := encodeHistory(h)
	if err != nil {
		return fmt.Errorf("adding to target history: %w", err)
	}
	return d.writeRecordBytes(d.historyPath(name), data)
}

// PruneHistory removes every history whose Used is before before, under the history lock.
// A record whose leading fields can't be read is as good as none, and is removed too;
// one whose entries alone are damaged is judged by its Used, and the next add rewrites it.
func (d *Directory) PruneHistory(ctx context.Context, before time.Time) (int, error) {
	unlock, err := d.lockHistory(ctx)
	if err != nil {
		return 0, fmt.Errorf("pruning target history: %w", err)
	}
	defer unlock()
	entries, err := os.ReadDir(filepath.Join(d.dir, "history"))
	if err != nil {
		return 0, fmt.Errorf("pruning target history: %w", err)
	}
	removed := 0
	var errs []error
	for _, e := range entries {
		if e.Name() == historyLockName || !e.Type().IsRegular() {
			continue // what no add writes, such as a FIFO, which opening could block on
		}
		path := filepath.Join(d.dir, "history", e.Name())
		head, err := readHistoryHead(path)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if !head.Used.Before(before) {
			continue
		}
		if err := d.sizes.change(path, func() error { return d.remove(path) }); err != nil &&
			!errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, fmt.Errorf("removing target history %s: %w", e.Name(), err))
			continue
		}
		removed++
	}
	return removed, errors.Join(errs...)
}

// SetHistoryUsed sets the Used of the history named name to used, as if it were last used then, under the history
// lock, so that tests can age a history; it does nothing if there is none that can be read.
func (d *Directory) SetHistoryUsed(ctx context.Context, name digest.Digest, used time.Time) error {
	unlock, err := d.lockHistory(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	h, err := d.History(ctx, name)
	if err != nil || h == nil {
		return err
	}
	h.Used = used.UTC()
	data, err := encodeHistory(h)
	if err != nil {
		return err
	}
	return d.writeRecordBytes(d.historyPath(name), data)
}
