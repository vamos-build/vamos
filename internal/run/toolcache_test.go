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
	"syscall"
	"testing"

	"github.com/vamos-build/vamos/internal/api"
	"github.com/vamos-build/vamos/internal/cache"
	"github.com/vamos-build/vamos/internal/graph"
	"github.com/vamos-build/vamos/internal/remote"
	"github.com/vamos-build/vamos/internal/sched"
	"github.com/vamos-build/vamos/internal/testenv"

	"github.com/containerd/platforms"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"go.podman.io/buildah"
	"go.podman.io/buildah/define"
	"go.podman.io/storage"
)

// TestToolCacheRequestsCross checks that an action's request keeps its tool caches, its workspace's name and whether
// it asks for empty tool caches as a coordinator assigns it to a worker, and as the worker gives it to the subprocess
// that runs it.
func TestToolCacheRequestsCross(t *testing.T) {
	caches := []graph.ToolCache{{Path: "/cache", Label: "//tools:go-build", Owner: 1000, Group: 1000}}
	req := &sched.Request{Action: &sched.Action{Kind: sched.KindTask, ToolCaches: caches}, Workspace: "com.acme.shop",
		NoToolCaches: true}
	roundTrip := func(in, out any) []byte {
		t.Helper()
		data, err := json.Marshal(in)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(data, out); err != nil {
			t.Fatal(err)
		}
		return data
	}
	var assignment remote.Assignment
	data := roundTrip(remote.Assignment{Attempt: "a-1", Request: req}, &assignment)
	if r := assignment.Request; r == nil || r.Action == nil || r.Workspace != "com.acme.shop" || !r.NoToolCaches ||
		!slices.Equal(r.Action.ToolCaches, caches) {
		t.Errorf("an assignment %s carries %+v; want its mounts, its workspace and that it is clean", data, r)
	}
	var input AttemptInput
	data = roundTrip(AttemptInput{Attempt: "a-1", Request: req}, &input)
	if r := input.Request; r == nil || r.Action == nil || r.Workspace != "com.acme.shop" || !r.NoToolCaches ||
		!slices.Equal(r.Action.ToolCaches, caches) {
		t.Errorf("an attempt's input %s carries %+v; want its mounts, its workspace and that it is clean", data, r)
	}
}

// cacheGraph returns a graph of a task on busybox, pinned, that mounts a tool cache and runs argv, stored in l's
// content store, and the request to run it.
func cacheGraph(t *testing.T, l *Local, argv ...string) api.GraphRequest {
	t.Helper()
	exec := platforms.Format(platforms.Normalize(platforms.DefaultSpec()))
	g := &graph.Graph{Version: graph.Version, ExecPlatform: exec, Roots: []graph.Ref{1}, Nodes: []graph.Node{
		{Label: "busybox", Desc: `pull "busybox"`, Pull: &graph.Pull{
			Image: "docker.io/library/busybox:1.36", Platform: exec, Digest: testenv.BusyboxIndex,
		}},
		{Label: "build", Desc: `task "build"`, Task: &graph.Task{
			Base:       0,
			Steps:      []graph.Step{{Op: graph.OpRun, Display: "run", Argv: argv}},
			ToolCaches: []graph.ToolCache{{Path: "/cache", Label: "//:tool"}},
		}},
	}}
	testenv.Must(t, g.Validate())
	content, err := l.ContentStore()
	testenv.Must(t, err)
	blob, err := content.PutBytes(t.Context(), graph.Encode(g))
	testenv.Must(t, err)
	platform, err := graph.ParsePlatform(exec)
	testenv.Must(t, err)
	return api.GraphRequest{Graph: blob, Version: graph.Version, Platform: platform}
}

// TestWorkspaceNameRefused checks that a standalone build of a graph that mounts caches fails before anything runs,
// without opening the container store, when its request's workspace name breaks the rules of a name, clean or not,
// or is empty without NoToolCaches; and that an empty name with NoToolCaches, or any name in coordinator mode, gets
// past the check, to the container store, which here can't be opened.
func TestWorkspaceNameRefused(t *testing.T) {
	for _, tc := range []struct {
		name         string
		workspace    string
		noToolCaches bool
		coordinator  bool
		want         string // in the failure
	}{
		{
			name: "no name",
			want: "the build mounts tool caches, so the workspace needs a name in WORKSPACE",
		},
		{
			name:      "a malformed name",
			workspace: "Shop",
			want:      `the request's workspace name "Shop" must start with a lowercase letter`,
		},
		{
			name:         "a malformed name, clean",
			workspace:    "../shop",
			noToolCaches: true,
			want:         "the request's workspace name",
		},
		{
			name:         "no name, clean",
			noToolCaches: true,
			want:         "no-such-driver",
		},
		{
			name:        "no name, for a coordinator",
			coordinator: true,
			want:        "no-such-driver",
		},
		{
			name:        "a malformed name, for a coordinator",
			workspace:   "../shop",
			coordinator: true,
			want:        "no-such-driver",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			l, err := NewLocal(storage.StoreOptions{
				GraphRoot:       filepath.Join(dir, "storage"),
				RunRoot:         filepath.Join(dir, "run"),
				GraphDriverName: "no-such-driver", // which would fail to open
			})
			testenv.Must(t, err)
			defer l.Close()
			if tc.coordinator {
				l.ServeAsCoordinator()
			}
			req := cacheGraph(t, l, "true")
			req.Workspace, req.NoToolCaches = tc.workspace, tc.noToolCaches
			outcome := awaitJob(t, l, api.Request{Graph: &req})
			if outcome.Failure == nil || !strings.Contains(outcome.Failure.Message, tc.want) {
				t.Errorf("the job's outcome = %+v; want it to fail with %q", outcome.Failure, tc.want)
			}
		})
	}
}

// TestWorkspaceNameAccepted checks that a build of a graph that mounts caches succeeds without a workspace name when
// it asks for empty caches, and in coordinator mode without one or with a malformed one.
func TestWorkspaceNameAccepted(t *testing.T) {
	testenv.RequireBuildah(t)
	for _, tc := range []struct {
		name         string
		workspace    string
		noToolCaches bool
		coordinator  bool
	}{
		{name: "no name, clean", noToolCaches: true},
		{name: "no name, for a coordinator", coordinator: true},
		{name: "a malformed name, for a coordinator", workspace: "../shop", coordinator: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l, err := NewLocal(testenv.PrivateStores(t).Options)
			testenv.Must(t, err)
			defer l.Close()
			if tc.coordinator {
				l.ServeAsCoordinator()
			}
			req := cacheGraph(t, l, "echo", testenv.NewNonce())
			req.Workspace, req.NoToolCaches = tc.workspace, tc.noToolCaches
			if outcome := awaitJob(t, l, api.Request{Graph: &req}); outcome.Failure != nil {
				t.Errorf("the job failed: %s", outcome.Failure.Message)
			}
		})
	}
}

// outputOf runs req on l to its end and returns what its steps printed, failing the test if the job failed.
func outputOf(t *testing.T, l *Local, req api.GraphRequest) string {
	t.Helper()
	ctx := t.Context()
	id, err := l.Submit(ctx, api.Request{Graph: &req})
	testenv.Must(t, err)
	var out strings.Builder
	testenv.Must(t, l.Watch(ctx, id, 0, func(e api.Event) error {
		if e.Kind == api.EventOutput {
			out.Write(e.Data)
		}
		return nil
	}))
	outcome, err := l.Result(ctx, id)
	testenv.Must(t, err)
	if outcome.Failure != nil {
		t.Fatalf("the job failed: %s\n%s", outcome.Failure.Message, out.String())
	}
	return out.String()
}

// TestCoordinatorModeGivesCachesEmpty checks that in coordinator mode two attempts of different keys under one
// workspace name each find their cache empty, the second seeing nothing the first wrote, with a name or without one,
// and that their results record the caches as given empty.
func TestCoordinatorModeGivesCachesEmpty(t *testing.T) {
	testenv.RequireBuildah(t)
	for _, workspace := range []string{"com.acme.shop", "", "../shop"} {
		t.Run(fmt.Sprintf("workspace %q", workspace), func(t *testing.T) {
			l, err := NewLocal(testenv.PrivateStores(t).Options)
			testenv.Must(t, err)
			defer l.Close()
			l.ServeAsCoordinator()
			for i := range 2 {
				req := cacheGraph(t, l, "sh", "-c", fmt.Sprintf("echo listing=$(ls -A /cache); touch /cache/%d-%s", i,
					testenv.NewNonce()))
				req.Workspace = workspace
				if out := outputOf(t, l, req); !strings.Contains(out, "listing=\n") {
					t.Errorf("attempt %d found its cache holding something:\n%s", i, out)
				}
			}
			uses := recordedToolCaches(t, l)
			if len(uses) != 2 || !uses[0].Empty || !uses[1].Empty {
				t.Errorf("the results record the tool caches %+v; want each of the two given empty", uses)
			}
		})
	}
}

// TestMountTableNamesNoWorkspace checks that a step that mounts a workspace's cache finds no workspace name in its
// mount table, which holds a digest of the name in the cache's directory, as it may hold any path of the executor's.
func TestMountTableNamesNoWorkspace(t *testing.T) {
	testenv.RequireBuildah(t)
	l, err := NewLocal(testenv.PrivateStores(t).Options)
	testenv.Must(t, err)
	defer l.Close()
	req := cacheGraph(t, l, "sh", "-c", "echo "+testenv.NewNonce()+"; echo naming=$(grep -c com.acme.shop "+
		"/proc/self/mountinfo); echo mounted=$(grep -c ' /cache ' /proc/self/mountinfo)")
	req.Workspace = "com.acme.shop"
	out := outputOf(t, l, req)
	if !strings.Contains(out, "naming=0\n") || !strings.Contains(out, "mounted=1\n") {
		t.Errorf("the step's mount table, as it reported it:\n%s\nwant the cache mounted and no workspace named", out)
	}
}

// recordedToolCaches returns the tool caches that the results in l's build cache record in their provenance.
func recordedToolCaches(t *testing.T, l *Local) []cache.ToolCacheUse {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(cacheDir(l.storeOptions), "ac", "*"))
	testenv.Must(t, err)
	var uses []cache.ToolCacheUse
	for _, p := range paths {
		data, err := os.ReadFile(p)
		testenv.Must(t, err)
		var result cache.ActionResult
		testenv.Must(t, json.Unmarshal(data, &result))
		if result.Provenance != nil {
			uses = append(uses, result.Provenance.ToolCaches...)
		}
	}
	return uses
}

// remappedGraph returns a graph of a layer on busybox, pinned, whose step mounts a tool cache owned by 1000:1000 and
// reports, as observations, who runs it, who owns the cache, what it held and who owns what the step writes there,
// and of a task on the layer that reports, fresh, since the layers of two runs may make one image, who owns the
// cache's mount point in the layer's image and what it holds; stored in l's content store, and the request to run the
// task.
func remappedGraph(t *testing.T, l *Local) api.GraphRequest {
	t.Helper()
	exec := platforms.Format(platforms.Normalize(platforms.DefaultSpec()))
	g := &graph.Graph{Version: graph.Version, ExecPlatform: exec, Roots: []graph.Ref{2}, Nodes: []graph.Node{
		{Label: "busybox", Desc: `pull "busybox"`, Pull: &graph.Pull{
			Image: "docker.io/library/busybox:1.36", Platform: exec, Digest: testenv.BusyboxIndex,
		}},
		{Label: "cached", Desc: `layer "cached"`, Layer: &graph.Layer{
			Base:     0,
			Platform: exec,
			Steps: []graph.Step{{Op: graph.OpRun, Display: "run", Argv: []string{"sh", "-c", "set -e; echo " +
				testenv.NewNonce() + "; echo observed user=$(id -u):$(id -g); " +
				"echo observed cache=$(stat -c %u:%g /cache); ls -A /cache > /tmp/held; " +
				"echo observed held=$(wc -l < /tmp/held); echo written > /cache/written; " +
				"echo observed written=$(stat -c %u:%g /cache/written)"}}},
			ToolCaches: []graph.ToolCache{{Path: "/cache", Label: "//:tool", Owner: 1000, Group: 1000}},
		}},
		{Label: "check", Desc: `task "check"`, Task: &graph.Task{
			Base: 1,
			Steps: []graph.Step{{Op: graph.OpRun, Display: "run", Argv: []string{"sh", "-c",
				"set -e; echo " + testenv.NewNonce() + "; ls -A /cache > /tmp/held; " +
					"echo observed mount point=$(stat -c %u:%g /cache) holding $(wc -l < /tmp/held)"}}},
		}},
	}}
	testenv.Must(t, g.Validate())
	content, err := l.ContentStore()
	testenv.Must(t, err)
	blob, err := content.PutBytes(t.Context(), graph.Encode(g))
	testenv.Must(t, err)
	platform, err := graph.ParsePlatform(exec)
	testenv.Must(t, err)
	return api.GraphRequest{Graph: blob, Version: graph.Version, Platform: platform}
}

// remapBuilders makes every builder that the runner makes from now until the test ends map container IDs to the host
// from uid and gid, its users' and its groups', and run as 1000:1000, as an image whose config names that user does.
// Like every test that sets a package hook, such as newBuilder or removeTempCaches, its test must not run in parallel.
func remapBuilders(t *testing.T, uid, gid uint32) {
	t.Helper()
	prev := newBuilder
	newBuilder = func(ctx context.Context, store storage.Store, options buildah.BuilderOptions) (*buildah.Builder,
		error) {
		mappings := remapping(uid, gid) // each builder's own
		options.IDMappingOptions = &mappings
		b, err := prev(ctx, store, options)
		if err == nil {
			b.SetUser("1000:1000")
		}
		return b, err
	}
	t.Cleanup(func() { newBuilder = prev })
}

// remapping returns the ID mappings of 65536 container IDs from 0 to the host's from uid, users', and gid, groups'.
func remapping(uid, gid uint32) define.IDMappingOptions {
	return define.IDMappingOptions{
		UIDMap: []specs.LinuxIDMapping{{ContainerID: 0, HostID: uid, Size: 65536}},
		GIDMap: []specs.LinuxIDMapping{{ContainerID: 0, HostID: gid, Size: 65536}},
	}
}

// hostOwner returns who owns p on the host.
func hostOwner(p string) (uid, gid uint32, err error) {
	info, err := os.Stat(p)
	if err != nil {
		return 0, 0, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, fmt.Errorf("%s has no owner to read", p)
	}
	return stat.Uid, stat.Gid, nil
}

// requireIDs reports the test unavailable unless this process's user namespace can map builders onto each range of
// size IDs from each of uids and gids: the kernel takes a mapping only if one extent of the namespace's own map holds
// it.
func requireIDs(t *testing.T, size uint64, uids, gids []uint64) {
	t.Helper()
	for _, need := range []struct {
		file   string
		starts []uint64
	}{{"/proc/self/uid_map", uids}, {"/proc/self/gid_map", gids}} {
		data, err := os.ReadFile(need.file)
		if err != nil {
			testenv.Unavailable(t, "mapping builders' IDs", err)
		}
		for _, start := range need.starts {
			if !mapsRange(string(data), start, size) {
				testenv.Unavailable(t, "mapping builders' IDs",
					fmt.Errorf("%s holds no extent of the %d IDs from %d:\n%s", need.file, size, start, data))
			}
		}
	}
}

// mapsRange reports whether one extent of the ID map idMap, as /proc/self/uid_map holds it, holds the size IDs from
// start.
func mapsRange(idMap string, start, size uint64) bool {
	for line := range strings.Lines(idMap) {
		var inside, outside, count uint64
		if _, err := fmt.Sscan(line, &inside, &outside, &count); err == nil && inside <= start &&
			start+size <= inside+count {
			return true
		}
	}
	return false
}

// cachedFiles returns the files named written in the data directories of the persistent tool caches in l's build
// cache, found by walking it, since its path may hold what a glob reads as a pattern.
func cachedFiles(t *testing.T, l *Local) []string {
	t.Helper()
	var files []string
	root := filepath.Join(cacheDir(l.storeOptions), toolCachesDir)
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if errors.Is(err, os.ErrNotExist) && p == root {
			return filepath.SkipDir
		}
		if err == nil && !d.IsDir() && d.Name() == "written" && filepath.Base(filepath.Dir(p)) == "data" {
			files = append(files, p)
		}
		return err
	})
	testenv.Must(t, err)
	return files
}

// TestNonRootStepWithRemappedBuilder checks that a non-root run() step whose builder's ID mappings differ from the
// store's, its users' from its groups', writes to a persistent cache and to a temporary one, seeing the rule's owner
// and group on each, which the host sees mapped through the builder's mappings, a persistent cache in a directory the
// mappings name, which a builder mapped otherwise finds empty; and that the image's mount point keeps them, empty,
// once the cache is unmounted.
func TestNonRootStepWithRemappedBuilder(t *testing.T) {
	testenv.RequireBuildah(t)
	testenv.RequireOwnership(t)
	requireIDs(t, 65536, []uint64{100000, 300000}, []uint64{200000, 400000}) // the mappings' ranges, as below
	observed := []string{"observed user=1000:1000\n", "observed cache=1000:1000\n", "observed held=0\n",
		"observed written=1000:1000\n", "observed mount point=1000:1000 holding 0\n"}
	check := func(t *testing.T, out string) {
		t.Helper()
		for _, want := range observed {
			if !strings.Contains(out, want) {
				t.Errorf("the steps printed:\n%s\nwant %q", out, strings.TrimSpace(want))
			}
		}
	}

	t.Run("a persistent cache", func(t *testing.T) {
		l, err := NewLocal(testenv.PrivateStores(t).Options)
		testenv.Must(t, err)
		defer l.Close()
		var before []string
		for _, ids := range [][2]uint32{{100000, 200000}, {300000, 400000}} {
			t.Run(fmt.Sprintf("mapped from %d:%d", ids[0], ids[1]), func(t *testing.T) {
				remapBuilders(t, ids[0], ids[1])
				req := remappedGraph(t, l)
				req.Workspace = "com.acme.shop"
				check(t, outputOf(t, l, req)) // the second, mapped otherwise, finds its cache empty too
				after := cachedFiles(t, l)
				var added []string
				for _, w := range after {
					if !slices.Contains(before, w) {
						added = append(added, w)
					}
				}
				was := before
				before = after // so that a later mapping is checked against what is there, whatever this one found
				if len(added) != 1 || len(after) != len(was)+1 {
					t.Fatalf("the caches' files were %q and are %q; want one more, the new mapping's", was, after)
				}
				name := filepath.Base(filepath.Dir(filepath.Dir(added[0])))
				if want := "1000-1000-" + idMappingName(remapping(ids[0], ids[1])); name != want {
					t.Errorf("the cache's directory is %s; want %s, for 1000:1000 named by the builder's mappings",
						name, want)
				}
				for _, p := range []string{filepath.Dir(added[0]), added[0]} {
					uid, gid, err := hostOwner(p)
					testenv.Must(t, err)
					if uid != ids[0]+1000 || gid != ids[1]+1000 {
						t.Errorf("%s is owned by %d:%d on the host; want %d:%d, as the builder's mappings put "+
							"1000:1000", p, uid, gid, ids[0]+1000, ids[1]+1000)
					}
				}
			})
		}
	})

	t.Run("a temporary cache", func(t *testing.T) {
		remapBuilders(t, 100000, 200000)
		// The runner removes them on a goroutine of its own, so what it found is checked once the job ends.
		var owners []string
		var probe error
		prev := removeTempCaches
		removeTempCaches = func(dir string) error {
			for _, p := range []string{filepath.Join(dir, "0"), filepath.Join(dir, "0", "written")} {
				uid, gid, err := hostOwner(p)
				probe = errors.Join(probe, err)
				owners = append(owners, fmt.Sprintf("%d:%d", uid, gid))
			}
			return prev(dir)
		}
		t.Cleanup(func() { removeTempCaches = prev })
		l, err := NewLocal(testenv.PrivateStores(t).Options)
		testenv.Must(t, err)
		defer l.Close()
		req := remappedGraph(t, l)
		req.Workspace, req.NoToolCaches = "com.acme.shop", true
		check(t, outputOf(t, l, req))
		testenv.Must(t, probe)
		if !slices.Equal(owners, []string{"101000:201000", "101000:201000"}) {
			t.Errorf("before its removal, the temporary cache and its file were owned on the host by %q; want "+
				"101000:201000, as the builder's mappings put 1000:1000", owners)
		}
		if written := cachedFiles(t, l); len(written) != 0 {
			t.Errorf("an attempt given an empty cache wrote to the workspace's: %q", written)
		}
	})
}

// TestMapsRange checks that an ID map maps a range only if one of its extents holds all of it: an extent that ends
// where the range does, not one that ends before, nor two that hold it between them, and one beyond a gap.
func TestMapsRange(t *testing.T) {
	for _, tc := range []struct {
		idMap       string
		start, size uint64
		want        bool
	}{
		{"         0          0 4294967295\n", 100000, 65536, true},
		{"0 0 165536\n", 100000, 65536, true},
		{"0 0 165535\n", 100000, 65536, false},
		{"0 0 120000\n120000 120000 245536\n", 100000, 65536, false},
		{"0 0 1000\n100000 500000 65536\n", 100000, 65536, true},
		{"1000 100000 64536\n", 0, 1, false},
		{"", 0, 1, false},
	} {
		if got := mapsRange(tc.idMap, tc.start, tc.size); got != tc.want {
			t.Errorf("mapsRange(%q, %d, %d) = %v; want %v", tc.idMap, tc.start, tc.size, got, tc.want)
		}
	}
}
