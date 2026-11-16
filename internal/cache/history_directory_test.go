package cache

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	"golang.org/x/sys/unix"

	"github.com/vamos-build/vamos/internal/testenv"
)

// historyHelperEnv marks the test binary run by the history tests across processes,
// in which TestHistoryHelper works on the directory the variable names, as historyHelperMode says.
const (
	historyHelperEnv  = "VAMOS_TEST_HISTORY_DIR"
	historyHelperMode = "VAMOS_TEST_HISTORY_MODE" // "add" or "hold"
)

// historyAdds is how many entries each process adds to one history in TestHistoryAcrossProcesses.
const historyAdds = 40

// historyHelperName is the history that the processes add to.
var historyHelperName = digest.FromString("history across processes")

// TestHistoryHelper adds entries to a history, or holds the history lock until its ready file is removed,
// as a test across processes asks; otherwise it does nothing.
func TestHistoryHelper(t *testing.T) {
	dir := os.Getenv(historyHelperEnv)
	if dir == "" {
		return
	}
	d, err := OpenDirectory(dir)
	testenv.Must(t, err)
	defer d.Close()
	switch os.Getenv(historyHelperMode) {
	case "add":
		for i := range historyAdds {
			e := HistoryEntry{Key: digest.FromString("child " + strconv.Itoa(i)), Action: json.RawMessage(`{}`)}
			testenv.Must(t, d.AddHistory(t.Context(), historyHelperName, e, nil, 2*historyAdds))
		}
	case "hold":
		unlock, err := d.lockHistory(t.Context())
		testenv.Must(t, err)
		defer unlock()
		ready := filepath.Join(dir, "ready")
		testenv.Must(t, os.WriteFile(ready, nil, 0o600))
		for {
			if _, err := os.Stat(ready); err != nil {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
}

// startHistoryHelper starts this test binary as TestHistoryHelper, on dir, in mode.
func startHistoryHelper(t *testing.T, dir, mode string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run", "^TestHistoryHelper$")
	cmd.Env = append(os.Environ(), historyHelperEnv+"="+dir, historyHelperMode+"="+mode)
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	testenv.Must(t, cmd.Start())
	return cmd
}

// TestHistoryAcrossProcesses checks that two processes adding to one history in the same directory at once
// both keep every entry they add.
func TestHistoryAcrossProcesses(t *testing.T) {
	dir := t.TempDir()
	d, err := OpenDirectory(dir)
	testenv.Must(t, err)
	defer d.Close()
	child := startHistoryHelper(t, dir, "add")
	for i := range historyAdds {
		e := HistoryEntry{Key: digest.FromString("parent " + strconv.Itoa(i)), Action: json.RawMessage(`{}`)}
		testenv.Must(t, d.AddHistory(t.Context(), historyHelperName, e, nil, 2*historyAdds))
	}
	if err := child.Wait(); err != nil {
		t.Fatalf("the other process's adds: %v", err)
	}
	h, err := d.History(t.Context(), historyHelperName)
	testenv.Must(t, err)
	if h == nil || len(h.Entries) != 2*historyAdds {
		t.Errorf("the history holds %d entries; want %d, every add of both processes", len(h.Entries), 2*historyAdds)
	}
}

// TestHistoryLockGivesUp checks that an add gives up, with ErrLocked, once it has waited its bound for a lock that
// another process holds, and at once when its context ends.
func TestHistoryLockGivesUp(t *testing.T) {
	dir := t.TempDir()
	d, err := OpenDirectory(dir)
	testenv.Must(t, err)
	defer d.Close()
	child := startHistoryHelper(t, dir, "hold")
	ready := filepath.Join(dir, "ready")
	defer func() {
		os.Remove(ready)
		done := make(chan error, 1)
		go func() { done <- child.Wait() }()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("the process holding the lock: %v", err)
			}
		case <-time.After(time.Minute):
			_ = child.Process.Kill() // the test fails anyway
			t.Errorf("the process holding the lock didn't end, and was killed: %v", <-done)
		}
	}()
	for deadline := time.Now().Add(time.Minute); ; time.Sleep(10 * time.Millisecond) {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the process to hold the lock never took it")
		}
	}

	e := HistoryEntry{Key: digest.FromString("a"), Action: json.RawMessage(`{}`)}
	d.historyWait = 200 * time.Millisecond
	start := time.Now()
	err = d.AddHistory(t.Context(), historyHelperName, e, nil, 8)
	if took := time.Since(start); !errors.Is(err, ErrLocked) || took < d.historyWait || took > 30*time.Second {
		t.Errorf("an add while another process holds the lock = %v after %v; want %v after %v",
			err, took, ErrLocked, d.historyWait)
	}

	d.historyWait = time.Minute
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	start = time.Now()
	err = d.AddHistory(ctx, historyHelperName, e, nil, 8)
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 10*time.Second {
		t.Errorf("an add whose context ends = %v after %v; want %v at the context's end",
			err, time.Since(start), context.DeadlineExceeded)
	}
	if h, _ := d.History(t.Context(), historyHelperName); h != nil {
		t.Error("an add that gave up added its entry")
	}
}

// TestHistoryLockWaitsInProcess checks that an add waits for another add of this process, holding the lock,
// and gives up at its bound, the wait for the process's own lock included.
func TestHistoryLockWaitsInProcess(t *testing.T) {
	dir := t.TempDir()
	d, err := OpenDirectory(dir)
	testenv.Must(t, err)
	defer d.Close()
	unlock, err := d.lockHistory(t.Context())
	testenv.Must(t, err)
	d.historyWait = 100 * time.Millisecond
	_, err = d.PruneHistory(t.Context(), time.Now())
	if !errors.Is(err, ErrLocked) {
		t.Errorf("a prune while an add holds the lock = %v; want %v", err, ErrLocked)
	}
	unlock()
	if _, err := d.PruneHistory(t.Context(), time.Now()); err != nil {
		t.Errorf("a prune once the lock is free = %v", err)
	}

	// With the lock free, an add whose context has ended fails rather than take it.
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	e := HistoryEntry{Key: digest.FromString("a"), Action: json.RawMessage(`{}`)}
	if err := d.AddHistory(cancelled, historyHelperName, e, nil, 8); !errors.Is(err, context.Canceled) {
		t.Errorf("an add whose context had ended = %v; want %v", err, context.Canceled)
	}
}

// TestPruneHistoryGoesOn checks that a prune that fails to remove one history goes on with the rest, counts what it
// removed, and leaves the one it failed to remove for the next prune.
// The failure is injected, since the tests may run as root, which no permission stops.
func TestPruneHistoryGoesOn(t *testing.T) {
	dir := t.TempDir()
	d, err := OpenDirectory(dir)
	testenv.Must(t, err)
	defer d.Close()
	ctx := t.Context()
	names := make([]digest.Digest, 3)
	for i := range names {
		names[i] = digest.FromString(fmt.Sprint("history ", i))
		e := HistoryEntry{Key: digest.FromString(fmt.Sprint(i)), Action: json.RawMessage(`{}`)}
		testenv.Must(t, d.AddHistory(ctx, names[i], e, nil, 8))
	}
	stuck := d.historyPath(names[1])
	d.remove = func(path string) error {
		if path == stuck {
			return &os.PathError{Op: "remove", Path: path, Err: unix.EBUSY}
		}
		return os.Remove(path)
	}
	removed, err := d.PruneHistory(ctx, time.Now().Add(time.Hour))
	if removed != 2 || err == nil || !strings.Contains(err.Error(), names[1].Encoded()) {
		t.Errorf("PruneHistory = %d, %v; want 2 removed and a failure naming %s", removed, err, names[1].Encoded())
	}
	if h, _ := d.History(ctx, names[1]); h == nil {
		t.Error("the history the prune failed to remove is gone")
	}
	d.remove = os.Remove
	if removed, err := d.PruneHistory(ctx, time.Now().Add(time.Hour)); removed != 1 || err != nil {
		t.Errorf("the next PruneHistory = %d, %v; want the one left removed", removed, err)
	}
}

// TestDamagedHistory checks that a record whose entries are damaged reads as none, still gives its head,
// is judged by its Used at a prune, and that the next add rewrites it.
func TestDamagedHistory(t *testing.T) {
	d, err := OpenDirectory(t.TempDir())
	testenv.Must(t, err)
	defer d.Close()
	ctx := t.Context()
	name := digest.FromString("damaged")
	e := HistoryEntry{Key: digest.FromString("a"), Action: json.RawMessage(`{}`)}
	testenv.Must(t, d.AddHistory(ctx, name, e, nil, 8))
	data, err := os.ReadFile(d.historyPath(name))
	testenv.Must(t, err)
	damaged := append(data[:strings.Index(string(data), `"entries"`)], `"entries":[{"key":"nope"`...)
	testenv.Must(t, os.WriteFile(d.historyPath(name), damaged, 0o600))

	if h, err := d.History(ctx, name); err != nil || h != nil {
		t.Errorf("History of a damaged record = %v, %v; want none", h, err)
	}
	if head, err := d.Newest(ctx, name); err != nil || head.Newest != e.Fingerprint() {
		t.Errorf("Newest of a damaged record = %+v, %v; want its head", head, err)
	}
	if removed, err := d.PruneHistory(ctx, time.Now().Add(-time.Hour)); err != nil || removed != 0 {
		t.Errorf("a prune before its Used = %d, %v; want it kept", removed, err)
	}
	b := HistoryEntry{Key: digest.FromString("b"), Action: json.RawMessage(`{}`)}
	testenv.Must(t, d.AddHistory(ctx, name, b, nil, 8))
	if h, err := d.History(ctx, name); err != nil || h == nil || len(h.Entries) != 1 || h.Entries[0].Key != b.Key {
		t.Errorf("History after an add = %v, %v; want the record rewritten with the new entry", h, err)
	}
	testenv.Must(t, os.WriteFile(d.historyPath(name), []byte("not JSON"), 0o600))
	if removed, err := d.PruneHistory(ctx, time.Now().Add(-time.Hour)); err != nil || removed != 1 {
		t.Errorf("a prune of a record whose head can't be read = %d, %v; want it removed", removed, err)
	}
}
