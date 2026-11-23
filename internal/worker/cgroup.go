package worker

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/vamos-build/vamos/internal/remote"
)

// cgroupRoot is where the worker makes a cgroup for each attempt, beneath the cgroup v2 hierarchy,
// so that a kill reaches every process an attempt started, however it detached itself.
const cgroupRoot = "/sys/fs/cgroup/vamos"

// containment is how attempts are contained: cgroups, when the host has cgroup v2 that the worker may write.
type containment struct {
	root string // the cgroup that attempts' cgroups are made in
	// noMemory is why attempts' cgroups can't count their peak memory, if they can't: the memory controller couldn't
	// be enabled for them.
	noMemory error
}

// newContainment checks that attempts can be contained here, making the worker's cgroup, and returns how.
// A worker that can't contain attempts mustn't register: a kill that misses a process leaves it running.
func newContainment(root string) (*containment, error) {
	if _, err := os.Stat(filepath.Join(filepath.Dir(root), "cgroup.controllers")); err != nil {
		return nil, fmt.Errorf("attempts can't be contained: the cgroup v2 hierarchy isn't at %s: %w",
			filepath.Dir(root), err)
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("attempts can't be contained: making the cgroup %s: %w", root, err)
	}
	// A cgroup can hold processes only if it has no children with controllers, or controllers of its own:
	// the worker's cgroup holds none; each attempt's cgroup holds that attempt.
	probe := filepath.Join(root, "probe")
	if err := os.Mkdir(probe, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, fmt.Errorf("attempts can't be contained: making a cgroup in %s: %w", root, err)
	}
	if _, err := os.Stat(filepath.Join(probe, "cgroup.kill")); err != nil {
		os.Remove(probe)
		return nil, fmt.Errorf("attempts can't be contained: the kernel's cgroups have no cgroup.kill: %w", err)
	}
	if err := os.Remove(probe); err != nil {
		return nil, fmt.Errorf("attempts can't be contained: removing a cgroup in %s: %w", root, err)
	}
	// Attempts' cgroups count their peak memory only if the memory controller reaches them, which the worker's own
	// cgroup must have been given; without it, their reports leave the peak out.
	c := &containment{root: root}
	if err := os.WriteFile(filepath.Join(root, "cgroup.subtree_control"), []byte("+memory"), 0o644); err != nil {
		c.noMemory = err
	}
	return c, nil
}

// path returns the cgroup of the attempt id.
func (c *containment) path(id string) string {
	return filepath.Join(c.root, "attempt-"+id)
}

// create makes the cgroup for the attempt id and returns it, open, for the attempt's process to be started in.
// The caller closes the file.
func (c *containment) create(id string) (*os.File, error) {
	dir := c.path(id)
	if err := os.Mkdir(dir, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, fmt.Errorf("making the attempt's cgroup: %w", err)
	}
	f, err := os.OpenFile(dir, os.O_RDONLY|syscall.O_DIRECTORY, 0)
	if err != nil {
		return nil, fmt.Errorf("opening the attempt's cgroup: %w", err)
	}
	return f, nil
}

// usage returns what the attempt id's processes have used, as its cgroup counts it: its CPU time, always counted, and
// its peak memory, if the memory controller reaches it; nil if the cgroup can tell nothing.
func (c *containment) usage(id string) *remote.Usage {
	var u remote.Usage
	if stat, err := os.ReadFile(filepath.Join(c.path(id), "cpu.stat")); err == nil {
		for line := range strings.SplitSeq(string(stat), "\n") {
			if v, ok := strings.CutPrefix(line, "usage_usec "); ok {
				if n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64); err == nil && n >= 0 {
					u.CPUMicros = &n
				}
			}
		}
	}
	if peak, err := os.ReadFile(filepath.Join(c.path(id), "memory.peak")); err == nil {
		if n, err := strconv.ParseInt(strings.TrimSpace(string(peak)), 10, 64); err == nil && n >= 0 {
			u.MemoryPeak = &n
		}
	}
	if u.CPUMicros == nil && u.MemoryPeak == nil {
		return nil
	}
	return &u
}

// errStuck says that an attempt's processes didn't exit once killed.
var errStuck = errors.New("the attempt's processes didn't exit")

// kill kills every process in the attempt's cgroup and waits for it to empty, for at most wait,
// then removes the cgroup.
func (c *containment) kill(id string, wait time.Duration) error {
	if err := c.stop(id, wait); err != nil {
		return err
	}
	return c.remove(id)
}

// holds reports whether the attempt's cgroup holds the process pid, which it does until the process has exited,
// zombie or not, and never once its pid is another's, which another cgroup holds; a cgroup that is there but can't
// be read is taken to hold it, as the process hasn't been waited for, and one that is gone holds nothing.
func (c *containment) holds(id string, pid int) bool {
	procs, err := os.ReadFile(filepath.Join(c.path(id), "cgroup.procs"))
	if err != nil {
		return !errors.Is(err, os.ErrNotExist)
	}
	return slices.Contains(strings.Fields(string(procs)), strconv.Itoa(pid))
}

// stop kills every process in the attempt's cgroup and waits for it to empty, for at most wait, leaving the cgroup,
// so that what its processes used can still be read.
func (c *containment) stop(id string, wait time.Duration) error {
	dir := c.path(id)
	if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err := os.WriteFile(filepath.Join(dir, "cgroup.kill"), []byte("1"), 0o644); err != nil {
		return fmt.Errorf("killing the attempt's cgroup: %w", err)
	}
	deadline := time.Now().Add(wait)
	for {
		procs, err := os.ReadFile(filepath.Join(dir, "cgroup.procs"))
		if err != nil {
			return fmt.Errorf("reading the attempt's cgroup: %w", err)
		}
		if strings.TrimSpace(string(procs)) == "" {
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%w within %s of being killed: %s", errStuck, wait,
				strings.Join(strings.Fields(string(procs)), ", "))
		}
		time.Sleep(20 * time.Millisecond)
	}
	return nil
}

// remove removes the attempt's cgroup, which must be empty.
func (c *containment) remove(id string) error {
	if err := os.Remove(c.path(id)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("removing the attempt's cgroup: %w", err)
	}
	return nil
}

// leftovers returns the IDs of the attempts whose cgroups remain from an earlier run of a worker that crashed.
func (c *containment) leftovers() ([]string, error) {
	entries, err := os.ReadDir(c.root)
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, e := range entries {
		if id, ok := strings.CutPrefix(e.Name(), "attempt-"); ok && e.IsDir() {
			ids = append(ids, id)
		}
	}
	return ids, nil
}
