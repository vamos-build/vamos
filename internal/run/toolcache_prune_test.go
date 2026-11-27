package run

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/vamos-build/vamos/internal/api"
	"github.com/vamos-build/vamos/internal/cache"
	"github.com/vamos-build/vamos/internal/testenv"

	"go.podman.io/storage/pkg/system"
)

// pruneLocal returns a local executor on stores of its own, opened, in coordinator mode if coordinator, and where its
// build cache keeps tool caches.
func pruneLocal(t *testing.T, coordinator bool) (*Local, string) {
	t.Helper()
	testenv.RequireBuildah(t)
	l, err := NewLocal(testenv.PrivateStores(t).Options)
	testenv.Must(t, err)
	t.Cleanup(func() { l.Close() })
	if coordinator {
		l.ServeAsCoordinator()
	}
	testenv.Must(t, l.open())
	return l, cacheDir(l.storeOptions)
}

// makeDirs makes each of paths, directories relative to root, and the file "data/written" in each that ends in
// "data".
func makeDirs(t *testing.T, root string, paths ...string) {
	t.Helper()
	for _, p := range paths {
		testenv.Must(t, os.MkdirAll(filepath.Join(root, p), 0o755))
		if filepath.Base(p) == "data" {
			testenv.Must(t, os.WriteFile(filepath.Join(root, p, "written"), nil, 0o644))
		}
	}
}

// trashIn returns the tool caches that prunes set aside in dir, a build cache, and haven't deleted.
func trashIn(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	testenv.Must(t, err)
	var trash []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), toolCachesTrash) {
			trash = append(trash, e.Name())
		}
	}
	return trash
}

// TestPruneToolCaches checks what a standalone prune removes and counts: each cache, by workspace, label, owner,
// group and mapping, two owners under one label among them, but neither a .tmp- leftover nor a label's directory;
// that it deletes what an earlier prune left; and that an executor that never mounted a cache prunes, counting none.
func TestPruneToolCaches(t *testing.T) {
	t.Run("caches of two workspaces, a leftover and old trash", func(t *testing.T) {
		l, dir := pruneLocal(t, false)
		makeDirs(t, filepath.Join(dir, toolCachesDir),
			"ws1/label1/0-0-host/data", "ws1/label1/1000-1000-host/data", "ws1/label2/0-0-host/data",
			"ws2/label1/0-0-host/data", "ws2/label3/.tmp-1/data", "ws2/.tmp-2", "ws2/label4")
		makeDirs(t, dir, toolCachesTrash+"old/ws/label/0-0-host/data")
		pruned, err := l.prune(t.Context())
		testenv.Must(t, err)
		if pruned.ToolCaches != 4 || len(pruned.Warnings) != 0 {
			t.Errorf("the prune counted %d tool caches, warning %q; want 4 and none", pruned.ToolCaches,
				pruned.Warnings)
		}
		if _, err := os.Stat(filepath.Join(dir, toolCachesDir)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("tool-caches/ is still there: %v", err)
		}
		if trash := trashIn(t, dir); len(trash) != 0 {
			t.Errorf("the prune left %q", trash)
		}
	})

	t.Run("an executor that never mounted a cache", func(t *testing.T) {
		l, _ := pruneLocal(t, false)
		pruned, err := l.prune(t.Context())
		testenv.Must(t, err)
		if pruned.ToolCaches != 0 {
			t.Errorf("the prune counted %d tool caches; want none", pruned.ToolCaches)
		}
	})
}

// TestPruneToolCachesInCoordinatorMode checks that a local coordinator's prune leaves the tool caches of standalone
// builds that share its store, and what a standalone prune left to delete, counting none.
func TestPruneToolCachesInCoordinatorMode(t *testing.T) {
	l, dir := pruneLocal(t, true)
	makeDirs(t, filepath.Join(dir, toolCachesDir), "ws/label/0-0-host/data")
	makeDirs(t, dir, toolCachesTrash+"old/ws/label/0-0-host/data")
	pruned, err := l.prune(t.Context())
	testenv.Must(t, err)
	if pruned.ToolCaches != 0 {
		t.Errorf("a local coordinator's prune counted %d tool caches; want none", pruned.ToolCaches)
	}
	if _, err := os.Stat(filepath.Join(dir, toolCachesDir, "ws/label/0-0-host/data/written")); err != nil {
		t.Errorf("a local coordinator's prune removed a standalone build's cache: %v", err)
	}
	if trash := trashIn(t, dir); len(trash) != 1 {
		t.Errorf("a local coordinator's prune left the trash %q; want the old one, kept", trash)
	}
}

// TestPruneToolCachesFailures checks how a prune of tool caches fails: a failed rename fails the prune and counts
// none; a failed count, after the rename, is a warning, counting what was read, and the tree is still deleted; and a
// failed delete is a warning, leaving the trash for the next prune.
func TestPruneToolCachesFailures(t *testing.T) {
	t.Run("a rename that fails", func(t *testing.T) {
		l, dir := pruneLocal(t, false)
		makeDirs(t, filepath.Join(dir, toolCachesDir), "ws/label/0-0-host/data")
		renameDir = func(string, string) error { return errors.New("the disk refused") }
		t.Cleanup(func() { renameDir = os.Rename })
		pruned, err := l.prune(t.Context())
		if err == nil || !strings.Contains(err.Error(), "setting the tool caches aside: the disk refused") ||
			pruned.ToolCaches != 0 {
			t.Errorf("the prune = %d tool caches, %v; want it failed, counting none", pruned.ToolCaches, err)
		}
		if _, err := os.Stat(filepath.Join(dir, toolCachesDir, "ws/label/0-0-host/data")); err != nil {
			t.Errorf("a failed rename removed the cache: %v", err)
		}
	})

	t.Run("a count that fails", func(t *testing.T) {
		l, dir := pruneLocal(t, false)
		makeDirs(t, filepath.Join(dir, toolCachesDir), "ws1/label/0-0-host/data", "ws2/label/0-0-host/data")
		readDir = func(name string) ([]os.DirEntry, error) {
			entries, err := os.ReadDir(name)
			if strings.HasSuffix(name, "ws2") {
				return entries, errors.New("unreadable partway") // what it read is counted
			}
			return entries, err
		}
		t.Cleanup(func() { readDir = os.ReadDir })
		pruned, err := l.prune(t.Context())
		testenv.Must(t, err)
		if pruned.ToolCaches != 2 || !slices.ContainsFunc(pruned.Warnings, func(w string) bool {
			return strings.Contains(w, "counting the tool caches pruned: unreadable partway")
		}) {
			t.Errorf("the prune counted %d tool caches, warning %q; want 2 and the count's failure",
				pruned.ToolCaches, pruned.Warnings)
		}
		if trash := trashIn(t, dir); len(trash) != 0 {
			t.Errorf("the prune left %q", trash)
		}
	})

	t.Run("a delete that fails", func(t *testing.T) {
		l, dir := pruneLocal(t, false)
		makeDirs(t, filepath.Join(dir, toolCachesDir), "ws/label/0-0-host/data")
		removeAll = func(string) error { return errors.New("busy") }
		t.Cleanup(func() { removeAll = system.EnsureRemoveAll })
		pruned, err := l.prune(t.Context())
		testenv.Must(t, err)
		if pruned.ToolCaches != 1 || !slices.Contains(pruned.Warnings, "deleting the tool caches pruned: busy") {
			t.Errorf("the prune counted %d tool caches, warning %q; want 1 and the delete's failure",
				pruned.ToolCaches, pruned.Warnings)
		}
		if trash := trashIn(t, dir); len(trash) != 1 {
			t.Errorf("the prune left %q; want the trash it couldn't delete", trash)
		}
	})
}

// TestPruneLosesTheLock checks that a prune that leaves the build cache without its lock ends the process, saying
// so, rather than reporting, for a standalone executor and a local coordinator alike: it prunes no history and
// leaves what it set aside for the next standalone prune, and is not reported as refused.
func TestPruneLosesTheLock(t *testing.T) {
	for _, coordinator := range []bool{false, true} {
		t.Run(map[bool]string{false: "standalone", true: "a local coordinator"}[coordinator], func(t *testing.T) {
			l, dir := pruneLocal(t, coordinator)
			makeDirs(t, filepath.Join(dir, toolCachesDir), "ws/label/0-0-host/data")
			restore := cache.FailRelock()
			t.Cleanup(restore)
			var ended error
			lostLock = func(err error) { ended = err }
			t.Cleanup(func() { lostLock = exitOnLostLock })
			pruned, err := l.prune(t.Context())
			restore()
			if !errors.Is(err, cache.ErrRelock) || errors.Is(err, cache.ErrLocked) || ended == nil ||
				!strings.Contains(ended.Error(), "the build cache lost its lock") ||
				!coordinator && !strings.Contains(ended.Error(), "it had removed 1 tool caches from use") {
				t.Errorf("the prune = %v, ending the process with %v; want it ended, the lock lost", err, ended)
			}
			if pruned.Histories != 0 || pruned.ToolCaches != 0 {
				t.Errorf("the prune reported %+v; want nothing", pruned)
			}
			if want := map[bool]int{false: 1, true: 0}[coordinator]; len(trashIn(t, dir)) != want {
				t.Errorf("the prune left %q; want %d set aside", trashIn(t, dir), want)
			}
		})
	}
}

// TestPruneToolCachesBesideAPatternlikeStore checks that a prune deletes the trash of its own build cache, even where
// its path holds characters a glob would read as a pattern, and never another's beside it.
func TestPruneToolCachesBesideAPatternlikeStore(t *testing.T) {
	root := t.TempDir()
	own, other := filepath.Join(root, "store[dev]"), filepath.Join(root, "storev")
	makeDirs(t, own, toolCachesTrash+"old/ws/label/0-0-host/data")
	makeDirs(t, other, toolCachesTrash+"old/ws/label/0-0-host/data")
	var pruned api.Pruned
	deleteToolCacheTrash(own, &pruned)
	if trash := trashIn(t, own); len(trash) != 0 || len(pruned.Warnings) != 0 {
		t.Errorf("the prune left its own trash %q, warning %q", trash, pruned.Warnings)
	}
	if trash := trashIn(t, other); len(trash) != 1 {
		t.Errorf("the prune deleted another store's trash, leaving %q", trash)
	}
}

// TestFailedPruneSaysWhatItRemoved checks that a prune that failed says what it had certainly done, each it did any
// of: the images and target histories it removed and the tool caches it set aside, not the results and files it
// counted before deleting, and its warnings.
func TestFailedPruneSaysWhatItRemoved(t *testing.T) {
	for _, tc := range []struct {
		pruned api.Pruned
		want   string
	}{
		{pruned: api.Pruned{Results: 2, Blobs: 3}},
		{pruned: api.Pruned{Images: 1, Results: 2}, want: "it had removed 1 images"},
		{pruned: api.Pruned{Histories: 5}, want: "it had removed 5 target histories"},
		{pruned: api.Pruned{ToolCaches: 4}, want: "it had removed 4 tool caches from use"},
		{pruned: api.Pruned{Warnings: []string{"left trash"}}, want: "left trash"},
		{
			pruned: api.Pruned{Images: 1, ToolCaches: 4, Warnings: []string{"left trash"}},
			want:   "it had removed 1 images; it had removed 4 tool caches from use; left trash",
		},
	} {
		if got := describePruned(tc.pruned); got != tc.want {
			t.Errorf("describePruned(%+v) = %q; want %q", tc.pruned, got, tc.want)
		}
	}
}
