package run

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/vamos-build/vamos/internal/cache"
	"github.com/vamos-build/vamos/internal/graph"
	"github.com/vamos-build/vamos/internal/sched"
	"github.com/vamos-build/vamos/internal/testenv"

	"github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"go.podman.io/buildah"
	"go.podman.io/buildah/define"
	"go.podman.io/buildah/pkg/parse"
	"go.podman.io/storage"
	"go.podman.io/storage/pkg/system"
)

// TestResolveIn checks that a path is resolved in a root filesystem as the container would follow it, never leaving
// it, and that what it passes through on the way is recorded.
func TestResolveIn(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{"real/sub", "first/entry", "second"} {
		testenv.Must(t, os.MkdirAll(filepath.Join(root, dir), 0o755))
	}
	for link, target := range map[string]string{
		"alias":    "/real",
		"relative": "real/sub",
		"nested":   "alias/sub",
		"up":       "../../../real",
		"joined":   "first/entry/../../second",
		"loop":     "loop",
	} {
		testenv.Must(t, os.Symlink(target, filepath.Join(root, link)))
	}
	for _, tc := range []struct {
		path     string
		location string
		through  []string
		err      string
	}{
		{path: "/alias/cache", location: "/real/cache", through: []string{"/real"}},
		{path: "/relative", location: "/real/sub", through: []string{"/real"}},
		{path: "/nested/x", location: "/real/sub/x", through: []string{"/real", "/real/sub"}},
		{path: "/up", location: "/real"},
		{path: "/joined", location: "/second", through: []string{"/first", "/first/entry", "/first"}},
		{path: "/missing/../x", location: "/x", through: []string{"/missing"}},
		{path: "/loop", err: "too many levels of symbolic links"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			res, err := resolveIn(root, tc.path)
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("resolveIn = %+v, %v; want an error with %q", res, err, tc.err)
				}
				return
			}
			testenv.Must(t, err)
			if res.location != tc.location || !slices.Equal(res.through, tc.through) {
				t.Errorf("resolveIn = %s through %q; want %s through %q", res.location, res.through, tc.location,
					tc.through)
			}
		})
	}
}

// TestToolCacheIDMappings checks that a cache's owner is mapped to the host through the builder's ID mappings, that
// mappings name the directory, "host" without any, and that mappings that differ, even one that keeps root's ID and
// remaps another, name another.
func TestToolCacheIDMappings(t *testing.T) {
	remapped := []specs.LinuxIDMapping{{ContainerID: 0, HostID: 100000, Size: 65536}}
	keepsRoot := []specs.LinuxIDMapping{
		{ContainerID: 0, HostID: 0, Size: 1},
		{ContainerID: 1, HostID: 200000, Size: 1000},
	}
	for _, tc := range []struct {
		id   uint32
		maps []specs.LinuxIDMapping
		host uint32
		ok   bool
	}{
		{id: 1000, host: 1000, ok: true},
		{id: 1000, maps: remapped, host: 101000, ok: true},
		{id: 70000, maps: remapped},
		{id: 0, maps: keepsRoot, host: 0, ok: true},
		{id: 5, maps: keepsRoot, host: 200004, ok: true},
		{id: 10, maps: []specs.LinuxIDMapping{{ContainerID: 0, HostID: 4294967290, Size: 100}}},
	} {
		if host, ok := hostID(tc.id, tc.maps); host != tc.host || ok != tc.ok {
			t.Errorf("hostID(%d, %v) = %d, %v; want %d, %v", tc.id, tc.maps, host, ok, tc.host, tc.ok)
		}
	}
	names := map[string]bool{}
	for _, options := range []define.IDMappingOptions{
		{},
		{UIDMap: remapped, GIDMap: remapped},
		{UIDMap: keepsRoot, GIDMap: keepsRoot},
		{UIDMap: keepsRoot, GIDMap: remapped},
	} {
		names[idMappingName(options)] = true
	}
	if len(names) != 4 || !names["host"] {
		t.Errorf("the mappings are named %v; want four names, host for none", names)
	}
}

// TestPersistentToolCacheLayout checks where a workspace's cache is kept, that a second owner and group get a directory
// of their own beside the first's, and that the cache's directory has mode 0755 under any umask.
func TestPersistentToolCacheLayout(t *testing.T) {
	testenv.RequireOwnership(t)
	root := t.TempDir()
	old := syscall.Umask(0o077)
	defer syscall.Umask(old)
	first := &toolCacheMount{Path: "/cache", Label: "//tools:go-build"}
	dir, err := persistentToolCache(root, "com.acme.shop", first, "host")
	testenv.Must(t, err)
	labelDir := filepath.Join(root, digestName("com.acme.shop"), digestName("//tools:go-build"))
	if want := filepath.Join(labelDir, "0-0-host", "data"); dir != want {
		t.Errorf("the cache is at %s; want %s", dir, want)
	}
	if data, err := os.ReadFile(filepath.Join(labelDir, "label")); err != nil || string(data) != "//tools:go-build\n" {
		t.Errorf("the label's file holds %q, %v; want the label", data, err)
	}
	second := &toolCacheMount{Path: "/cache", Label: "//tools:go-build", Owner: 1000, Group: 1001, hostUID: 1000,
		hostGID: 1001}
	other, err := persistentToolCache(root, "com.acme.shop", second, "host")
	testenv.Must(t, err)
	if want := filepath.Join(labelDir, "1000-1001-host", "data"); other != want {
		t.Errorf("the second owner's cache is at %s; want %s", other, want)
	}
	for _, d := range []string{dir, other} {
		info, err := os.Stat(d)
		testenv.Must(t, err)
		if info.Mode().Perm() != 0o755 {
			t.Errorf("%s has mode %v; want 0755", d, info.Mode().Perm())
		}
	}
	if info, err := os.Stat(other); err == nil {
		stat := info.Sys().(*syscall.Stat_t)
		if stat.Uid != 1000 || stat.Gid != 1001 {
			t.Errorf("the second owner's cache is owned by %d:%d; want 1000:1001", stat.Uid, stat.Gid)
		}
	}
	// Two owners that two stores' mappings put on one host ID keep apart, by the rule's IDs.
	mapped := &toolCacheMount{Path: "/cache", Label: "//tools:go-build", hostUID: 1000, hostGID: 1001}
	apart, err := persistentToolCache(root, "com.acme.shop", mapped, "mapped")
	testenv.Must(t, err)
	if apart == other {
		t.Errorf("an owner that another mapping puts on the same host IDs shares %s", apart)
	}
}

// TestPersistentToolCacheRace checks that attempts that make one cache at once all find the same directory, and leave
// no temporary one behind.
func TestPersistentToolCacheRace(t *testing.T) {
	root := t.TempDir()
	var wg sync.WaitGroup
	dirs := make([]string, 8)
	errs := make([]error, len(dirs))
	for i := range dirs {
		wg.Go(func() {
			m := &toolCacheMount{Path: "/cache", Label: "//:tool",
				hostUID: uint32(os.Getuid()), hostGID: uint32(os.Getgid())}
			dirs[i], errs[i] = persistentToolCache(root, "com.acme.shop", m, "host")
		})
	}
	wg.Wait()
	for i := range dirs {
		testenv.Must(t, errs[i])
		if dirs[i] != dirs[0] {
			t.Errorf("attempts found %s and %s", dirs[0], dirs[i])
		}
	}
	err := filepath.WalkDir(root, func(p string, _ os.DirEntry, err error) error {
		if strings.HasPrefix(filepath.Base(p), ".tmp-") {
			t.Errorf("%s was left behind", p)
		}
		return err
	})
	testenv.Must(t, err)
}

// cacheTarget returns a target being built from scratch, with its container mounted, that mounts tool caches at
// paths, each from a directory of its own, root's, as the runner would set it up once its builder exists.
func cacheTarget(t *testing.T, paths ...string) *targetState {
	t.Helper()
	testenv.RequireBuildah(t)
	stores := testenv.PrivateStores(t)
	store, err := storage.GetStore(stores.Options)
	testenv.Must(t, err)
	t.Cleanup(func() { _, _ = store.Shutdown(true) })
	sys, err := SystemContext()
	testenv.Must(t, err)
	b, err := buildah.NewBuilder(t.Context(), store, builderOptions("scratch", nil, sys))
	testenv.Must(t, err)
	st := &targetState{runner: &localRunner{store: store, isolation: define.IsolationChroot}, builder: b,
		buildEnv: map[string]string{}}
	t.Cleanup(func() { testenv.Must(t, st.delete()) })
	if st.mountPoint, err = b.Mount(""); err != nil {
		t.Fatal(err)
	}
	st.caches = &toolCaches{}
	for i, p := range paths {
		st.caches.mounts = append(st.caches.mounts, &toolCacheMount{
			Path: p, Label: fmt.Sprintf("//:cache%d", i), dir: t.TempDir(),
		})
	}
	return st
}

// inRoot makes, in the root filesystem at root, the directories dirs, empty regular files files, and the links links,
// each a path to its target, replacing a link already there.
func inRoot(t *testing.T, root string, dirs, files []string, links map[string]string) {
	t.Helper()
	for _, d := range dirs {
		testenv.Must(t, os.MkdirAll(filepath.Join(root, d), 0o755))
	}
	for _, f := range files {
		testenv.Must(t, os.WriteFile(filepath.Join(root, f), nil, 0o644))
	}
	for link, target := range links {
		testenv.Must(t, os.MkdirAll(filepath.Dir(filepath.Join(root, link)), 0o755))
		if info, err := os.Lstat(filepath.Join(root, link)); err == nil && info.Mode()&os.ModeSymlink != 0 {
			testenv.Must(t, os.Remove(filepath.Join(root, link)))
		}
		testenv.Must(t, os.Symlink(target, filepath.Join(root, link)))
	}
}

// TestToolCacheChecks checks each check of a cache path in the container, at the stage of the attempt where it fails:
// before the steps, just before the first run() step, before a later one, or after the steps.
func TestToolCacheChecks(t *testing.T) {
	type change struct {
		dirs, files []string
		links       map[string]string
	}
	for _, tc := range []struct {
		name    string
		caches  []string
		image   change         // the image's, before the steps
		volumes []string       // that the image declares
		secrets []graph.Secret // of each run() step
		early   change         // made by a step before the first run() step, as an add() does
		first   change         // made by the first run() step
		next    bool           // a second run() step follows the first
		workdir string         // set by a workdir() after the first run() step, if any
		stage   string         // where it fails, or "" if it passes
		want    string         // in the error
	}{
		{
			name:   "a symbolic link that puts a cache at /run",
			caches: []string{"/alias"},
			image:  change{links: map[string]string{"alias": "/run"}},
			stage:  "before",
			want:   "/run/.containerenv",
		},
		{
			name:   "an alias through a directory of the image's /dev",
			caches: []string{"/alias/cache"},
			image:  change{dirs: []string{"dev/shm"}, links: map[string]string{"alias": "/dev/shm"}},
			stage:  "before",
			want:   "resolves through /dev",
		},
		{
			name:   "a /etc/resolv.conf that links into a cache at /run/systemd",
			caches: []string{"/run/systemd"},
			image:  change{links: map[string]string{"etc/resolv.conf": "/run/systemd/resolve/stub-resolv.conf"}},
			stage:  "before",
			want:   "/etc/resolv.conf",
		},
		{
			name:    "a volume /alias/../cache/data, /alias linked elsewhere",
			caches:  []string{"/cache"},
			image:   change{dirs: []string{"elsewhere/deep"}, links: map[string]string{"alias": "/elsewhere/deep"}},
			volumes: []string{"/alias/../cache/data"},
			stage:   "before",
			want:    "the volume /alias/../cache/data",
		},
		{
			name:  "a /dev that is a symbolic link, with no cache",
			image: change{links: map[string]string{"dev": "devices"}},
		},
		{
			name:   "a /dev that is a relative symbolic link, with a cache at /devices/cache",
			caches: []string{"/devices/cache"},
			image:  change{links: map[string]string{"dev": "devices"}},
			stage:  "before",
			want:   "/dev is a symbolic link",
		},
		{
			name:   "crypto-policies' back-ends linked to a cache",
			caches: []string{"/cache"},
			image:  change{links: map[string]string{"etc/crypto-policies/back-ends": "/cache/back-ends"}},
			stage:  "before",
			want:   "/etc/crypto-policies/back-ends",
		},
		{
			name:   "a first run() step that links /etc/hostname into a cache",
			caches: []string{"/cache"},
			first:  change{links: map[string]string{"etc/hostname": "/cache/hostname"}},
			next:   true,
			stage:  "next run",
			want:   "/etc/hostname",
		},
		{
			name:   "a last run() step that points a cache path through /dev",
			caches: []string{"/alias/cache"},
			image:  change{dirs: []string{"real", "dev"}, links: map[string]string{"alias": "/real", "dev/x": "/real"}},
			first:  change{links: map[string]string{"alias": "/dev/x"}},
			stage:  "after",
			want:   "resolves through /dev",
		},
		{
			name:   "an add() that makes a missing parent a symbolic link to /var/lib",
			caches: []string{"/app/cache"},
			image:  change{dirs: []string{"var/lib"}},
			early:  change{links: map[string]string{"app": "/var/lib"}},
			stage:  "first run",
			want:   "tool cache path /app/cache now resolves to /var/lib/cache",
		},
		{
			name:    "a secret's file above a cache",
			caches:  []string{"/data/cache"},
			secrets: []graph.Secret{{ID: "token", File: "/data"}},
			stage:   "first run",
			want:    `secret "token"'s file /data`,
		},
		{
			name:    "a secret's file at a cache that a step moves away before it runs",
			caches:  []string{"/cache"},
			image:   change{dirs: []string{"elsewhere"}, links: map[string]string{"alias": "/cache"}},
			secrets: []graph.Secret{{ID: "token", File: "/alias"}},
			early:   change{links: map[string]string{"alias": "/elsewhere/token"}},
		},
		{
			name:    "a last run() step that links a secret's file through a cache",
			caches:  []string{"/cache"},
			image:   change{dirs: []string{"elsewhere"}, links: map[string]string{"alias": "/elsewhere"}},
			secrets: []graph.Secret{{ID: "token", File: "/alias/token"}},
			first:   change{links: map[string]string{"alias": "/cache/x/../../elsewhere"}},
			stage:   "after",
			want:    `secret "token"'s file /alias/token resolves through /cache`,
		},
		{
			name:   "a last run() step that moves a cache path",
			caches: []string{"/alias/cache"},
			image:  change{dirs: []string{"real", "other"}, links: map[string]string{"alias": "/real"}},
			first:  change{links: map[string]string{"alias": "/other"}},
			stage:  "after",
			want:   "tool cache path /alias/cache now resolves to /other/cache",
		},
		{
			name:   "a mount point beneath a regular file",
			caches: []string{"/file/cache"},
			image:  change{files: []string{"file"}},
			stage:  "first run",
			want:   "making tool cache path /file/cache",
		},
		{
			name:   "a dangling symbolic link on a cache path",
			caches: []string{"/cache"},
			image:  change{links: map[string]string{"cache": "/nowhere/cache"}},
		},
		{
			name:   "a cache path that nested links lead beneath /proc",
			caches: []string{"/a/cache"},
			image:  change{links: map[string]string{"a": "b", "b": "proc/x"}},
			stage:  "before",
			want:   "resolves through /proc",
		},
		{
			name:   "a cache path that a link leads beneath /sys",
			caches: []string{"/s/cache"},
			image:  change{links: map[string]string{"s": "/sys"}},
			stage:  "before",
			want:   "resolves through /sys",
		},
		{
			name:   "a cache at /device/cache",
			caches: []string{"/device/cache"},
		},
		{
			name:   "an add() before the first run() step that copies a directory to a cache's location",
			caches: []string{"/cache"},
			early:  change{dirs: []string{"cache"}},
			stage:  "first run",
			want:   "a step put something at tool cache path /cache",
		},
		{
			name:   "a cache path linked through its own cache",
			caches: []string{"/alias"},
			image:  change{dirs: []string{"real/sub"}, links: map[string]string{"alias": "real/sub/.."}},
			stage:  "before",
			want:   "tool cache path /alias resolves through /real, within the tool cache at /alias",
		},
		{
			name:   "an /etc/hostname linked through a cache to a file outside it",
			caches: []string{"/cache"},
			image:  change{links: map[string]string{"etc/hostname": "/cache/x/../../hostfile"}},
			stage:  "before",
			want:   "/etc/hostname, which buildah binds into run() steps resolves through /cache",
		},
		{
			name:   "a last run() step that links /etc/hostname through a cache",
			caches: []string{"/cache"},
			first:  change{links: map[string]string{"etc/hostname": "/cache/x/../../hostfile"}},
			stage:  "after",
			want:   "/etc/hostname, which buildah binds into run() steps resolves through /cache",
		},
		{
			name:    `a workdir("/alias/sub"), /alias linked through a cache outside it`,
			caches:  []string{"/cache"},
			image:   change{dirs: []string{"out"}, links: map[string]string{"alias": "/cache/x/../../out"}},
			next:    true,
			workdir: "/alias/sub",
			stage:   "next run",
			want:    "the working directory /alias/sub resolves through /cache",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := cacheTarget(t, tc.caches...)
			root := st.mountPoint
			inRoot(t, root, tc.image.dirs, tc.image.files, tc.image.links)
			for _, v := range tc.volumes {
				st.builder.AddVolume(v)
			}
			stage, err := func() (string, error) {
				if err := st.beforeSteps(tc.secrets); err != nil {
					return "before", err
				}
				inRoot(t, root, tc.early.dirs, tc.early.files, tc.early.links)
				if _, err := st.beforeRun(tc.secrets); err != nil {
					return "first run", err
				}
				inRoot(t, root, tc.first.dirs, tc.first.files, tc.first.links)
				if tc.workdir != "" {
					st.builder.SetWorkDir(tc.workdir)
				}
				if tc.next {
					if _, err := st.beforeRun(tc.secrets); err != nil {
						return "next run", err
					}
				}
				return "after", st.afterSteps()
			}()
			if err == nil {
				stage = ""
			}
			if stage != tc.stage || tc.stage != "" && !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("it failed at %q with %v; want %q with %q", stage, err, tc.stage, tc.want)
			}
		})
	}
}

// TestToolCacheMountPoints checks that a missing mount point is made, with its parents, that an empty directory
// already there, as an earlier layer left one, is kept and given the cache's owner and group and mode 0755, and that
// a dangling link's target is where the mount point is made.
func TestToolCacheMountPoints(t *testing.T) {
	testenv.RequireOwnership(t)
	st := cacheTarget(t, "/a/b/cache", "/kept", "/dangling")
	root := st.mountPoint
	testenv.Must(t, os.Mkdir(filepath.Join(root, "kept"), 0o700))
	testenv.Must(t, os.Chown(filepath.Join(root, "kept"), 5, 5))
	testenv.Must(t, os.Symlink("/nowhere/cache", filepath.Join(root, "dangling")))
	for _, m := range st.caches.mounts {
		m.Owner, m.Group, m.hostUID, m.hostGID = 1000, 1000, 1000, 1000
	}
	before, err := os.Stat(filepath.Join(root, "kept"))
	testenv.Must(t, err)
	testenv.Must(t, st.beforeSteps(nil))
	mounts, err := st.beforeRun(nil)
	testenv.Must(t, err)
	for _, p := range []string{"a/b/cache", "kept", "nowhere/cache"} {
		info, err := os.Stat(filepath.Join(root, p))
		if err != nil {
			t.Errorf("no mount point at /%s: %v", p, err)
			continue
		}
		stat := info.Sys().(*syscall.Stat_t)
		if !info.IsDir() || info.Mode().Perm() != 0o755 || stat.Uid != 1000 || stat.Gid != 1000 {
			t.Errorf("/%s is %v owned by %d:%d; want a directory with mode 0755 owned by 1000:1000", p,
				info.Mode(), stat.Uid, stat.Gid)
		}
	}
	after, err := os.Stat(filepath.Join(root, "kept"))
	testenv.Must(t, err)
	if !os.SameFile(before, after) {
		t.Error("the empty directory at /kept was replaced, not kept")
	}
	var destinations []string
	for _, m := range mounts {
		destinations = append(destinations, m.Destination)
		if m.Type != define.TypeBind || !slices.Contains(m.Options, "rw") {
			t.Errorf("the mount at %s is %s %v; want a read-write bind", m.Destination, m.Type, m.Options)
		}
	}
	if want := []string{"/a/b/cache", "/kept", "/nowhere/cache"}; !slices.Equal(destinations, want) {
		t.Errorf("the run() step mounts at %q; want %q", destinations, want)
	}
}

// TestToolCacheIntoACache checks that an add() destination, which a variable or ".." may put in a cache once
// expanded and cleaned, and a task's source that a link resolves into one, are refused, and that a source whose ".."
// a link at its start puts outside a cache is collected.
func TestToolCacheIntoACache(t *testing.T) {
	st := cacheTarget(t, "/cache", "/build")
	root := st.mountPoint
	inRoot(t, root, []string{"elsewhere/deep"}, nil, map[string]string{"out": "/cache/out", "work": "/elsewhere/deep"})
	testenv.Must(t, st.beforeSteps(nil))
	st.builder.SetEnv("D", "/x")
	files := cache.Blob{Digest: digest.FromString("files"), Size: 1}
	err := st.apply(context.Background(), sched.Step{Op: graph.OpAdd, Files: &files, Dest: "$D/../cache/f"})
	if err == nil || !strings.Contains(err.Error(), "add() destination /cache/f resolves to /cache/f") {
		t.Errorf("an add() that a variable and .. put in a cache = %v; want it refused", err)
	}
	if err := st.checkInto("source", "/out"); err == nil || !strings.Contains(err.Error(), "/cache/out") {
		t.Errorf("a source that a link puts in a cache = %v; want it refused", err)
	}
	if err := st.checkInto("source", "/work/../build"); err != nil {
		t.Errorf("a source whose .. a link puts outside a cache = %v; want it collected", err)
	}
	st.builder.SetEnv("W", "/x")
	testenv.Must(t, st.apply(context.Background(), sched.Step{Op: graph.OpWorkdir, Dir: "$W/../cache/sub"}))
	if _, err := st.beforeRun(nil); err == nil || !strings.Contains(err.Error(), "the working directory /cache/sub") {
		t.Errorf("a workdir() that a variable and .. put in a cache = %v; want it refused", err)
	}
}

// TestToolCacheWorkdirAsBuildahCleansIt checks that a working directory that only buildah's cleaning, which settles a
// ".." before it follows the link before it, puts in a cache is refused before a run() step.
func TestToolCacheWorkdirAsBuildahCleansIt(t *testing.T) {
	st := cacheTarget(t, "/cache")
	inRoot(t, st.mountPoint, []string{"a/b"}, nil, map[string]string{"link": "/a/b"})
	testenv.Must(t, st.beforeSteps(nil))
	st.builder.SetWorkDir("/link/../cache/sub")
	want := "the working directory /link/../cache/sub, as buildah cleans it to /cache/sub, is in the tool cache"
	if _, err := st.beforeRun(nil); err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("a working directory that cleaning puts in a cache = %v; want it refused", err)
	}
}

// TestToolCacheWorkdirPassingThroughOnceCleaned checks that a working directory that passes through a cache only as
// buildah cleans it, before it enters it, is refused before a run() step, naming that spelling.
func TestToolCacheWorkdirPassingThroughOnceCleaned(t *testing.T) {
	st := cacheTarget(t, "/cache")
	inRoot(t, st.mountPoint, []string{"a/b", "elsewhere"}, nil, map[string]string{"link": "/a/b"})
	testenv.Must(t, st.beforeSteps(nil))
	_, err := st.beforeRun(nil)
	testenv.Must(t, err)
	// A step links /cache/out, in the mounted cache, elsewhere; the original spelling resolves to /a/cache/out.
	testenv.Must(t, os.Symlink("/elsewhere", filepath.Join(st.caches.mounts[0].dir, "out")))
	inRoot(t, st.mountPoint, nil, nil, map[string]string{"cache/out": "/elsewhere"})
	st.builder.SetWorkDir("/link/../cache/out")
	_, err = st.beforeRun(nil)
	want := "the working directory /link/../cache/out, as buildah cleans it to /cache/out, resolves through /cache"
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("a working directory that passes through a cache once cleaned = %v; want an error with %q", err, want)
	}
}

// TestToolCacheOwnerTheExecutorCantSet checks that an owner that the builder's ID mappings don't map fails the attempt,
// naming the cache.
func TestToolCacheOwnerTheExecutorCantSet(t *testing.T) {
	st := cacheTarget(t)
	st.builder.IDMappingOptions.UIDMap = []specs.LinuxIDMapping{{ContainerID: 0, HostID: 100000, Size: 65536}}
	r := &localRunner{store: st.runner.store, isolation: define.IsolationChroot, toolCacheRoot: t.TempDir()}
	st.runner = r
	req := sched.Request{Action: &sched.Action{Kind: sched.KindTask, ToolCaches: []graph.ToolCache{
		{Path: "/cache", Label: "//:tool", Owner: 70000, Group: 0},
	}}, Workspace: "com.acme.shop"}
	err := r.openToolCaches(st, req)
	if err == nil || !strings.Contains(err.Error(), "tool cache //:tool: its owner, 70000, is no ID") {
		t.Errorf("an owner the executor can't map = %v; want the attempt to fail naming the cache", err)
	}
}

// TestToolCacheWorkspaceName checks that the runner refuses an empty or malformed workspace name when it works out a
// persistent cache's directory, and checks none for an attempt given empty caches.
func TestToolCacheWorkspaceName(t *testing.T) {
	for _, tc := range []struct {
		name         string
		workspace    string
		noToolCaches bool
		root         bool // the runner keeps persistent caches, as a standalone local executor's does
		want         string
	}{
		{name: "an empty name", root: true, want: "workspace name must not be empty"},
		{name: "a malformed name", workspace: "../x", root: true, want: `workspace name "../x" must start`},
		{name: "a worker's attempt"},
		{name: "a clean attempt", noToolCaches: true, root: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := cacheTarget(t)
			r := &localRunner{store: st.runner.store, isolation: define.IsolationChroot}
			if tc.root {
				r.toolCacheRoot = t.TempDir()
			}
			st.runner = r
			req := sched.Request{Action: &sched.Action{Kind: sched.KindTask, ToolCaches: []graph.ToolCache{
				{Path: "/cache", Label: "//:tool"},
			}}, Workspace: tc.workspace, NoToolCaches: tc.noToolCaches}
			err := r.openToolCaches(st, req)
			if tc.want == "" {
				testenv.Must(t, err)
				if !st.caches.mounts[0].empty {
					t.Error("the attempt was given the workspace's cache, not an empty one of its own")
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("openToolCaches = %v; want an error with %q", err, tc.want)
			}
		})
	}
}

// TestToolCacheIsolation checks that the executor runs steps with the isolation it is given, and otherwise with
// buildah's own resolution, as an attempt that mounts no cache keeps, reporting the one it runs steps with, and that
// an attempt that mounts caches under an isolation other than chroot fails, naming it.
func TestToolCacheIsolation(t *testing.T) {
	testenv.RequireBuildah(t)
	oci, byDefault := define.IsolationOCI, define.IsolationDefault
	for _, tc := range []struct {
		name string
		use  *define.Isolation
	}{
		{name: "given OCI", use: &oci},
		{name: "given the default", use: &byDefault},
		{name: "unset"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// What buildah's environment says, which an unset or default isolation takes, whatever the host sets.
			t.Setenv("BUILDAH_ISOLATION", "chroot")
			l, err := NewLocal(testenv.PrivateStores(t).Options)
			testenv.Must(t, err)
			defer l.Close()
			if tc.use != nil {
				l.UseIsolation(*tc.use)
			}
			testenv.Must(t, l.open())
			want, err := parse.IsolationOption("")
			testenv.Must(t, err)
			if tc.use != nil && *tc.use != define.IsolationDefault {
				want = *tc.use
			}
			if l.runner.isolation != want {
				t.Errorf("steps run with %s isolation; want %s", l.runner.isolation, want)
			}
			caps, err := l.Capabilities(t.Context())
			testenv.Must(t, err)
			if caps.Isolation != want.String() {
				t.Errorf("the executor reports %s isolation; want %s, as steps run", caps.Isolation, want)
			}
		})
	}
	st := cacheTarget(t)
	st.runner.isolation = define.IsolationOCI
	req := sched.Request{Action: &sched.Action{Kind: sched.KindTask, ToolCaches: []graph.ToolCache{
		{Path: "/cache", Label: "//:tool"},
	}}, NoToolCaches: true}
	if err := st.runner.openToolCaches(st, req); err == nil || !strings.Contains(err.Error(), "with oci isolation") {
		t.Errorf("an attempt that mounts caches under OCI isolation = %v; want it to fail naming oci", err)
	}
}

// TestTempToolCachesRemoved checks that an attempt's own empty caches are deleted as its target's container is.
func TestTempToolCachesRemoved(t *testing.T) {
	st := cacheTarget(t)
	m := &toolCacheMount{Path: "/cache", Label: "//:tool",
		hostUID: uint32(os.Getuid()), hostGID: uint32(os.Getgid())}
	dir, err := st.caches.tempToolCache(st.runner.store, st.builder.ContainerID, 0, m)
	testenv.Must(t, err)
	testenv.Must(t, os.WriteFile(filepath.Join(dir, "written"), nil, 0o644))
	temp := st.caches.temp
	testenv.Must(t, st.delete())
	if _, err := os.Stat(temp); !os.IsNotExist(err) {
		t.Errorf("the attempt's temporary caches at %s are still there: %v", temp, err)
	}
}

// attemptTarget returns a target of the attempt id being built from scratch, its container named for the attempt and
// mounted, as a worker's runner makes it, with an empty tool cache of its own holding a file.
func attemptTarget(t *testing.T, id string) (*targetState, storage.Store) {
	t.Helper()
	testenv.RequireBuildah(t)
	store, err := storage.GetStore(testenv.PrivateStores(t).Options)
	testenv.Must(t, err)
	t.Cleanup(func() { _, _ = store.Shutdown(true) })
	sys, err := SystemContext()
	testenv.Must(t, err)
	options := builderOptions("scratch", nil, sys)
	options.Container = attemptPrefix + id
	b, err := buildah.NewBuilder(t.Context(), store, options)
	testenv.Must(t, err)
	st := &targetState{runner: &localRunner{store: store, isolation: define.IsolationChroot}, builder: b}
	if st.mountPoint, err = b.Mount(""); err != nil {
		t.Fatal(err)
	}
	st.caches = &toolCaches{}
	m := &toolCacheMount{Path: "/cache", Label: "//:tool", hostUID: uint32(os.Getuid()), hostGID: uint32(os.Getgid())}
	dir, err := st.caches.tempToolCache(store, b.ContainerID, 0, m)
	testenv.Must(t, err)
	testenv.Must(t, os.WriteFile(filepath.Join(dir, "written"), nil, 0o644))
	return st, store
}

// TestRemoveAttemptRemovesItsToolCaches checks that removing a killed attempt's container deletes its temporary
// tool caches too.
func TestRemoveAttemptRemovesItsToolCaches(t *testing.T) {
	st, store := attemptTarget(t, "killed")
	temp := st.caches.temp
	testenv.Must(t, removeAttempt(store, "killed"))
	if _, err := os.Stat(temp); !os.IsNotExist(err) {
		t.Errorf("the attempt's temporary caches at %s are still there: %v", temp, err)
	}
	if _, err := store.Container(attemptPrefix + "killed"); !errors.Is(err, storage.ErrContainerUnknown) {
		t.Errorf("the attempt's container is still there: %v", err)
	}
}

// TestRemoveAttemptWhoseToolCachesWontGo checks that a failure to delete an attempt's temporary tool caches still
// removes its container, which deletes what is left, and is returned, saying it was the caches.
func TestRemoveAttemptWhoseToolCachesWontGo(t *testing.T) {
	st, store := attemptTarget(t, "stuck")
	held := errors.New("a process holds them")
	removeAll = func(p string) error {
		// The caches go first, while the container is still there.
		if p != st.caches.temp {
			t.Errorf("the attempt's caches were removed at %s; want %s", p, st.caches.temp)
		}
		if _, err := store.Container(attemptPrefix + "stuck"); err != nil {
			t.Errorf("the attempt's container was gone before its caches: %v", err)
		}
		return held
	}
	t.Cleanup(func() { removeAll = system.EnsureRemoveAll })
	err := removeAttempt(store, "stuck")
	want := "removing the attempt's temporary tool caches: a process holds them"
	if err == nil || !strings.Contains(err.Error(), want) || !errors.Is(err, held) {
		t.Errorf("removeAttempt = %v; want the caches' failure", err)
	}
	if _, err := store.Container(attemptPrefix + "stuck"); !errors.Is(err, storage.ErrContainerUnknown) {
		t.Errorf("the attempt's container is still there: %v", err)
	}
}

// TestRemoveAttemptFinishesAPartialRemoval checks that a removal that failed once the container's layer and buildah's
// state were gone, as one racing a process that a failed kill left writing can, is finished by the next: the layer
// store no longer knows the layer, as DeleteContainer leaves it once it deleted it first, whether or not buildah's
// state is gone too.
func TestRemoveAttemptFinishesAPartialRemoval(t *testing.T) {
	for _, keepState := range []bool{false, true} {
		t.Run(fmt.Sprintf("buildah's state kept: %v", keepState), func(t *testing.T) {
			removePartial(t, keepState)
		})
	}
}

// removePartial checks that a removal left with the container's layer gone, and buildah's state too unless keepState,
// is finished, through buildah's builder if its state is there, or the store's container otherwise.
func removePartial(t *testing.T, keepState bool) {
	st, store := attemptTarget(t, "partial")
	testenv.Must(t, st.builder.Unmount())
	container, err := store.Container(attemptPrefix + "partial")
	testenv.Must(t, err)
	dir, err := store.ContainerDirectory(container.ID)
	testenv.Must(t, err)
	if !keepState {
		testenv.Must(t, os.Remove(filepath.Join(dir, "buildah.json")))
	}
	options := storage.StoreOptions{GraphRoot: store.GraphRoot(), RunRoot: store.RunRoot(),
		GraphDriverName: store.GraphDriverName(), GraphDriverOptions: store.GraphOptions()}
	_, err = store.Shutdown(true)
	testenv.Must(t, err)
	// A container's layer is recorded in volatile-layers.json, or else layers.json.
	var layers string
	var data []byte
	for _, name := range []string{"volatile-layers.json", "layers.json"} {
		p := filepath.Join(options.GraphRoot, options.GraphDriverName+"-layers", name)
		if d, err := os.ReadFile(p); err == nil && strings.Contains(string(d), container.LayerID) {
			layers, data = p, d
			break
		}
	}
	if layers == "" {
		t.Fatal("no record of the container's layer found")
	}
	var records []map[string]any
	testenv.Must(t, json.Unmarshal(data, &records))
	records = slices.DeleteFunc(records, func(r map[string]any) bool { return r["id"] == container.LayerID })
	data, err = json.Marshal(records)
	testenv.Must(t, err)
	testenv.Must(t, os.WriteFile(layers, data, 0o600))
	reopened, err := storage.GetStore(options)
	testenv.Must(t, err)
	t.Cleanup(func() { _, _ = reopened.Shutdown(true) })
	testenv.Must(t, removeAttempt(reopened, "partial"))
	if _, err := reopened.Container(attemptPrefix + "partial"); !errors.Is(err, storage.ErrContainerUnknown) {
		t.Errorf("the attempt's container is still there: %v", err)
	}
}

// TestRemoveAttemptReportsEveryFailure checks that a failure to delete an attempt's temporary tool caches is returned
// with a failure to remove its container, each as it was.
func TestRemoveAttemptReportsEveryFailure(t *testing.T) {
	st, store := attemptTarget(t, "twice")
	held := errors.New("a process holds them")
	removeAll = func(string) error { return held }
	t.Cleanup(func() { removeAll = system.EnsureRemoveAll })
	// A file buildah's state can't be read from fails opening the container's builder.
	dir, err := store.ContainerDirectory(st.builder.ContainerID)
	testenv.Must(t, err)
	testenv.Must(t, os.WriteFile(filepath.Join(dir, "buildah.json"), []byte("not JSON"), 0o600))
	err = removeAttempt(store, "twice")
	if err == nil || !errors.Is(err, held) || !strings.Contains(err.Error(), "opening the attempt's container") {
		t.Errorf("removeAttempt = %v; want both the caches' failure and the container's", err)
	}
}

// TestTempToolCachesRemovedReadOnly checks that an attempt's own caches go even when a tool left directories in them
// read-only, as Go's module cache does: container storage runs as root, which deletes them regardless.
func TestTempToolCachesRemovedReadOnly(t *testing.T) {
	st := cacheTarget(t)
	m := &toolCacheMount{Path: "/cache", Label: "//:tool", hostUID: uint32(os.Getuid()), hostGID: uint32(os.Getgid())}
	dir, err := st.caches.tempToolCache(st.runner.store, st.builder.ContainerID, 0, m)
	testenv.Must(t, err)
	testenv.Must(t, os.MkdirAll(filepath.Join(dir, "mod", "pkg"), 0o755))
	testenv.Must(t, os.WriteFile(filepath.Join(dir, "mod", "pkg", "go.mod"), nil, 0o444))
	testenv.Must(t, os.Chmod(filepath.Join(dir, "mod", "pkg"), 0o555))
	testenv.Must(t, os.Chmod(filepath.Join(dir, "mod"), 0o555))
	temp := st.caches.temp
	testenv.Must(t, st.delete())
	if _, err := os.Stat(temp); !os.IsNotExist(err) {
		t.Errorf("the attempt's temporary caches at %s are still there: %v", temp, err)
	}
}
